/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package evidence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testLogScenario = "S03/abrupt"
	testLogsDir     = testLogScenario + "/logs"
	testMaster      = "ha-0"
	testSlave       = "ha-1"
	testManager     = "manager-1"
	testBroker      = "ha-broker-rw-1"
	testServerLog   = "server/appdb_20261106_0910.err"
	testHeartbeat   = "HA-Node Info (current ha-0, state master)\n"
	testEngineLine  = "Time: 11/06/26 09:10:11.123 - ERROR *** the first failure\n"
)

// fakeLogs plays the cluster a collector reads. It records every command it
// is asked to run, and hook, when set, runs inside a read of the given key.
type fakeLogs struct {
	mu      sync.Mutex
	pods    map[string][]LogPod
	podsErr error
	logs    map[string]string
	logErr  map[string]error
	files   string
	tails   map[string]string
	tailErr error
	beat    string
	beatErr error
	execs   [][]string
	hook    func(ctx context.Context, key string, w io.Writer) error
}

func newFakeLogs() *fakeLogs {
	return &fakeLogs{
		pods: map[string][]LogPod{
			LogOperator:        {{Name: testManager, UID: "uid-manager", Node: "control"}},
			LogInstanceManager: {{Name: testMaster, UID: "uid-0", Node: "n0"}, {Name: testSlave, UID: "uid-1", Node: "n1"}},
			LogBrokerProcess:   {{Name: testBroker, UID: "uid-broker", Node: "n1"}},
		},
		logs: map[string]string{
			testManager: "2026-11-06T09:10:02.000000000Z operator saw the primary go\n",
			testMaster:  "2026-11-06T09:10:01.000000000Z the master log\n2026-11-06T09:10:04.000000000Z later\n",
			testSlave:   "2026-11-06T09:10:03.000000000Z the slave log\n",
			testBroker:  "2026-11-06T09:10:05.000000000Z the broker log\n",
		},
		logErr: map[string]error{},
		files:  testServerLog + "\nha-0_master.err\nbroker/sql_log/rw_1.sql.log\nconf/cubrid.conf\n",
		tails:  map[string]string{testServerLog: testEngineLine, "ha-0_master.err": "master.err line\n"},
		beat:   testHeartbeat,
	}
}

func (f *fakeLogs) Pods(_ context.Context, set LogPodSet) ([]LogPod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pods[set.Component], f.podsErr
}

func (f *fakeLogs) PodLog(ctx context.Context, _ LogPodSet, pod LogPod, previous bool, _ int64, w io.Writer) error {
	key := pod.Name
	if previous {
		key += "/previous"
	}
	if f.hook != nil {
		if err := f.hook(ctx, key, w); err != nil {
			return err
		}
	}
	if err := f.logErr[key]; err != nil {
		return err
	}
	_, err := io.WriteString(w, f.logs[key])
	return err
}

func (f *fakeLogs) Exec(ctx context.Context, _ LogPodSet, pod LogPod, command []string, w io.Writer) error {
	f.mu.Lock()
	f.execs = append(f.execs, slices.Clone(command))
	f.mu.Unlock()
	last := command[len(command)-1]
	if slices.Equal(command, heartbeatCommandForTest()) {
		if f.hook != nil {
			if err := f.hook(ctx, pod.Name+"/heartbeat", w); err != nil {
				return err
			}
		}
		if f.beatErr != nil {
			return f.beatErr
		}
		_, err := io.WriteString(w, f.beat)
		return err
	}
	if slices.Equal(command, ListEngineLogs) {
		_, err := io.WriteString(w, f.files)
		return err
	}
	if f.tailErr != nil {
		return f.tailErr
	}
	_, err := io.WriteString(w, f.tails[last])
	return err
}

func heartbeatCommandForTest() []string { return HeartbeatStatus }

func testPlan() LogPlan {
	return ClusterLogPlan("operators", "ns", "ha", []string{testMaster, testSlave}, true)
}

func testLimits() LogLimits {
	limits := DefaultLogLimits()
	limits.SourceTimeout, limits.CheckpointTimeout = 2*time.Second, 4*time.Second
	return limits
}

func newTestBundle(t *testing.T, f LogReader, limits LogLimits, r *Redactor) (*LogBundle, string) {
	t.Helper()
	dir := t.TempDir()
	return NewLogBundle(dir, testLogScenario, r, f, testPlan(), limits, NewLogBudget(1<<30, time.Hour)), dir
}

func readIndex(t *testing.T, dir string) LogIndex {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, testLogsDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index LogIndex
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	return index
}

// sourcesOf returns every source of the bundle, in the order of the index.
func sourcesOf(index LogIndex) []LogSource {
	var all []LogSource
	for _, cp := range index.Checkpoints {
		all = append(all, cp.Sources...)
	}
	return all
}

func findSource(t *testing.T, index LogIndex, component, pod string) LogSource {
	t.Helper()
	for _, s := range sourcesOf(index) {
		if s.Component == component && s.Pod == pod {
			return s
		}
	}
	t.Fatalf("no source %s of %s in %+v", component, pod, sourcesOf(index))
	return LogSource{}
}

// allBytes is the content of every file below dir, keyed by path.
func allBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A reader of a failed run sees every source in one index, in the order it
// was read, and every line that has a kubelet timestamp in one merged file.
func TestBundleIsOrderedAndNamesEverySource(t *testing.T) {
	f := newFakeLogs()
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	if b.Problem() != nil {
		t.Fatal(b.Problem())
	}
	if b.IndexFile() != testLogsDir+"/index.json" {
		t.Fatalf("index file %q", b.IndexFile())
	}
	index := readIndex(t, dir)
	if index.Scenario != testLogScenario || index.SchemaVersion == "" || len(index.Checkpoints) != 1 {
		t.Fatalf("index %+v", index)
	}
	all := sourcesOf(index)
	for i, s := range all {
		if s.Order != i+1 {
			t.Errorf("source %d has order %d: %+v", i, s.Order, s)
		}
		if s.Status == LogCollected || s.Status == LogTruncated {
			if !strings.HasPrefix(s.File, testLogsDir+"/"+LogsAfterVerdict+"/") {
				t.Errorf("file %q is outside the scenario's logs directory", s.File)
			}
			if data, err := os.ReadFile(filepath.Join(dir, s.File)); err != nil || int64(len(data)) != s.StoredBytes {
				t.Errorf("%s: %d bytes, index says %d: %v", s.File, len(data), s.StoredBytes, err)
			}
		}
	}
	if s := findSource(t, index, LogOperator, testManager); s.Status != LogCollected || !s.Ordered || s.Node != "control" {
		t.Errorf("operator source %+v", s)
	}
	findSource(t, index, LogInstanceManager, testMaster)
	findSource(t, index, LogBrokerProcess, testBroker)
	engine := findSource(t, index, LogEngine, testMaster)
	if engine.Status != LogCollected || engine.Ordered {
		t.Errorf("engine source %+v: its lines have no kubelet time and must not be merged", engine)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, engine.File)); string(data) != testEngineLine {
		t.Errorf("engine file %q", data)
	}
	beat := findSource(t, index, LogHeartbeat, testMaster)
	if data, _ := os.ReadFile(filepath.Join(dir, beat.File)); string(data) != testHeartbeat {
		t.Errorf("the raw heartbeat output was not kept as it was: %q", data)
	}
	merged, err := os.ReadFile(filepath.Join(dir, testLogsDir, "ordered.log"))
	if err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, want := range []string{"the master log", "operator saw the primary go", "the slave log", "later",
		"the broker log"} {
		at := strings.Index(string(merged), want)
		if at <= last {
			t.Errorf("%q is at %d, after %d, in:\n%s", want, at, last, merged)
		}
		last = at
	}
	if !strings.Contains(string(merged), "not compared") {
		t.Errorf("the merged file does not say its clocks were not compared:\n%s", merged)
	}
}

// A line seen at two checkpoints of one source is one line of the merged log.
func TestMergedLogKeepsALineSeenTwiceOnce(t *testing.T) {
	b, dir := newTestBundle(t, newFakeLogs(), testLimits(), nil)
	b.Checkpoint(t.Context(), LogsBeforeFault, false)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	merged, err := os.ReadFile(filepath.Join(dir, testLogsDir, "ordered.log"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(merged), "the master log"); n != 1 {
		t.Fatalf("the line appears %d times:\n%s", n, merged)
	}
}

// What could not be read is named with a reason, never shown as an empty log,
// and a source that really is empty is told from one that is missing.
func TestMissingSourceIsNeverReportedAsEmpty(t *testing.T) {
	f := newFakeLogs()
	f.pods[LogInstanceManager] = f.pods[LogInstanceManager][:1] // the slave's Pod is gone
	f.pods[LogBrokerProcess] = nil
	f.logErr[testManager] = errors.New("the node did not answer")
	f.logs[testMaster] = ""
	f.tailErr = errors.New("container not found")
	f.beatErr = errors.New("exit status 1")
	f.beat = ""
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	index := readIndex(t, dir)
	for _, tc := range []struct {
		component, pod string
		status         LogStatus
		reason         string
	}{
		{LogOperator, testManager, LogUnavailable, "the node did not answer"},
		{LogInstanceManager, testSlave, LogUnavailable, "not found"},
		{LogBrokerProcess, "", LogUnavailable, "no Pod"},
		{LogInstanceManager, testMaster, LogEmpty, "answered"},
		{LogEngine, testMaster, LogUnavailable, "container not found"},
		{LogHeartbeat, testMaster, LogUnavailable, "exit status 1"},
	} {
		s := findSource(t, index, tc.component, tc.pod)
		if s.Status != tc.status || !strings.Contains(s.Reason, tc.reason) {
			t.Errorf("%s of %q: %q (%s), want %q (%s)", tc.component, tc.pod, s.Status, s.Reason, tc.status, tc.reason)
		}
		if s.File != "" {
			t.Errorf("%s of %q has a file %q", tc.component, tc.pod, s.File)
		}
	}
	for _, s := range sourcesOf(index) {
		if s.Status != LogCollected && s.Status != LogTruncated && s.Reason == "" {
			t.Errorf("no reason: %+v", s)
		}
	}
}

// A Pod list that fails names the set and every Pod the plan expected.
func TestPodListFailureNamesEveryExpectedPod(t *testing.T) {
	f := newFakeLogs()
	f.podsErr = errors.New("the API server is gone")
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	index := readIndex(t, dir)
	for _, pod := range []string{testMaster, testSlave} {
		s := findSource(t, index, LogInstanceManager, pod)
		if s.Status != LogUnavailable || !strings.Contains(s.Reason, "the API server is gone") {
			t.Errorf("%+v", s)
		}
	}
}

// The log of a container that restarted is read for its previous instance too.
func TestPreviousInstanceIsReadAfterARestart(t *testing.T) {
	f := newFakeLogs()
	f.pods[LogInstanceManager][0].Restarts = 1
	f.logs[testMaster+"/previous"] = "2026-11-06T09:09:00.000000000Z before the restart\n"
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	var found bool
	for _, s := range sourcesOf(readIndex(t, dir)) {
		if s.Component == LogInstanceManager && s.Pod == testMaster && strings.Contains(s.Origin, "previous") {
			found = s.Status == LogCollected
		}
	}
	if !found {
		t.Fatalf("no previous instance in %+v", sourcesOf(readIndex(t, dir)))
	}
}

// A Pod replaced between two checkpoints took its log with it; the bundle says
// so under the old UID rather than letting the new Pod's log stand for it.
func TestReplacedPodIsReported(t *testing.T) {
	f := newFakeLogs()
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsBeforeFault, false)
	f.pods[LogInstanceManager][0].UID = "uid-0-new"
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	var replaced []LogSource
	for _, s := range readIndex(t, dir).Checkpoints[1].Sources {
		if s.Pod == testMaster && s.UID == "uid-0" {
			replaced = append(replaced, s)
		}
	}
	if len(replaced) != 1 || replaced[0].Status != LogUnavailable || !strings.Contains(replaced[0].Reason, "replaced") {
		t.Fatalf("the replaced Pod's sources: %+v", replaced)
	}
}

// A Pod of a Deployment comes back under another name: the Pod read earlier is
// still named as lost.
func TestRemovedPodIsReported(t *testing.T) {
	f := newFakeLogs()
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsBeforeFault, false)
	f.pods[LogBrokerProcess] = []LogPod{{Name: "ha-broker-rw-2", UID: "uid-broker-2", Node: "n1"}}
	f.logs["ha-broker-rw-2"] = "2026-11-06T09:10:09.000000000Z the replacement\n"
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	var gone []LogSource
	for _, s := range readIndex(t, dir).Checkpoints[1].Sources {
		if s.Pod == testBroker {
			gone = append(gone, s)
		}
	}
	if len(gone) != 1 || gone[0].UID != "uid-broker" || gone[0].Status != LogUnavailable {
		t.Fatalf("the removed Pod's sources: %+v", gone)
	}
}

// The end of a log explains a failure, so that is what a bound keeps, in whole
// lines, and the source says it was cut.
func TestSourceBoundKeepsTheEndInWholeLines(t *testing.T) {
	limits := testLimits()
	limits.SourceBytes = 1024
	f := newFakeLogs()
	var long strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&long, "2026-11-06T09:10:01.000000000Z line %d\n", i)
	}
	f.logs[testMaster] = long.String()
	b, dir := newTestBundle(t, f, limits, nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	s := findSource(t, readIndex(t, dir), LogInstanceManager, testMaster)
	if s.Status != LogTruncated || s.StoredBytes > limits.SourceBytes || s.ReadBytes != int64(long.Len()) {
		t.Fatalf("%+v", s)
	}
	data, err := os.ReadFile(filepath.Join(dir, s.File))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "line 1999\n") {
		t.Errorf("the end of the log is missing: %q", data[len(data)-40:])
	}
	if !strings.HasPrefix(string(data), "2026-") {
		t.Errorf("the first kept line was cut: %q", data[:40])
	}
}

// A log that reaches the line limit may have been cut by the API.
func TestLineLimitIsReportedAsTruncated(t *testing.T) {
	limits := testLimits()
	limits.TailLines = 2
	f := newFakeLogs()
	f.logs[testMaster] = "2026-11-06T09:10:01Z a\n2026-11-06T09:10:02Z b\n"
	b, dir := newTestBundle(t, f, limits, nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	if s := findSource(t, readIndex(t, dir), LogInstanceManager, testMaster); s.Status != LogTruncated {
		t.Fatalf("%+v", s)
	}
}

// A single line longer than the bound leaves nothing whole to keep: that is
// not an empty log.
func TestALineLongerThanTheBoundIsNotEmpty(t *testing.T) {
	limits := testLimits()
	limits.SourceBytes = 64
	f := newFakeLogs()
	f.logs[testMaster] = strings.Repeat("x", 1000)
	b, dir := newTestBundle(t, f, limits, nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	if s := findSource(t, readIndex(t, dir), LogInstanceManager, testMaster); s.Status != LogUnavailable {
		t.Fatalf("%+v", s)
	}
}

// Redaction can lengthen text; the bound holds for what is stored.
func TestBoundHoldsAfterRedactionLengthensTheText(t *testing.T) {
	limits := testLimits()
	limits.SourceBytes = 512
	f := newFakeLogs()
	f.logs[testMaster] = strings.Repeat("token=abc def\n", 36) // 504 bytes
	b, dir := newTestBundle(t, f, limits, nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	s := findSource(t, readIndex(t, dir), LogInstanceManager, testMaster)
	if s.StoredBytes > limits.SourceBytes || s.Status != LogTruncated {
		t.Fatalf("%+v", s)
	}
}

// A scenario's logs stay within its bound whatever its sources hold, and what
// did not fit is named.
func TestBundleBound(t *testing.T) {
	limits := testLimits()
	limits.SourceBytes, limits.BundleBytes, limits.OrderedBytes = 4096, 6144, 1024
	f := newFakeLogs()
	for _, pod := range []string{testManager, testMaster, testSlave, testBroker} {
		var text strings.Builder
		for i := range 500 {
			fmt.Fprintf(&text, "2026-11-06T09:10:01.000000000Z %s something happened %d\n", pod, i)
		}
		f.logs[pod] = text.String()
	}
	b, dir := newTestBundle(t, f, limits, nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	index := readIndex(t, dir)
	var stored int64
	var refused bool
	for _, s := range sourcesOf(index) {
		stored += s.StoredBytes
		refused = refused || (s.Status == LogUnavailable && strings.Contains(s.Reason, "size"))
	}
	if stored > limits.BundleBytes-limits.OrderedBytes {
		t.Errorf("sources stored %d bytes, more than the bound", stored)
	}
	if !refused {
		t.Errorf("no source says the size bound stopped it: %+v", sourcesOf(index))
	}
	merged, err := os.ReadFile(filepath.Join(dir, testLogsDir, "ordered.log"))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(merged)) > limits.OrderedBytes {
		t.Errorf("ordered.log holds %d bytes, more than %d", len(merged), limits.OrderedBytes)
	}
	if !strings.Contains(string(merged), "older lines") {
		t.Errorf("ordered.log does not say it dropped lines: %s", merged[:100])
	}
}

// Every scenario of a run draws on one budget, so a run with many failures
// cannot grow its artifact or its duration without bound.
func TestRunBudgetIsSharedAcrossScenarios(t *testing.T) {
	limits := testLimits()
	budget := NewLogBudget(100, time.Hour)
	first := NewLogBundle(t.TempDir(), testLogScenario, nil, newFakeLogs(), testPlan(), limits, budget)
	first.Checkpoint(t.Context(), LogsAfterVerdict, true)
	dir := t.TempDir()
	second := NewLogBundle(dir, testLogScenario, nil, newFakeLogs(), testPlan(), limits, budget)
	second.Checkpoint(t.Context(), LogsAfterVerdict, true)
	for _, s := range sourcesOf(readIndex(t, dir)) {
		if s.StoredBytes != 0 {
			t.Errorf("stored past the run's budget: %+v", s)
		}
		if s.Status != LogUnavailable && s.Status != LogDeferred {
			t.Errorf("%+v", s)
		}
	}
}

func TestRunTimeBudgetIsSharedAcrossScenarios(t *testing.T) {
	budget := NewLogBudget(1<<30, time.Millisecond)
	f := newFakeLogs()
	f.hook = func(ctx context.Context, _ string, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}
	limits := testLimits()
	first := NewLogBundle(t.TempDir(), testLogScenario, nil, f, testPlan(), limits, budget)
	first.Checkpoint(t.Context(), LogsAfterVerdict, true)
	dir := t.TempDir()
	reads := 0
	counting := newFakeLogs()
	counting.hook = func(context.Context, string, io.Writer) error { reads++; return nil }
	second := NewLogBundle(dir, testLogScenario, nil, counting, testPlan(), limits, budget)
	second.Checkpoint(t.Context(), LogsAfterVerdict, true)
	if reads != 0 {
		t.Fatalf("%d reads after the run's collection time was used up", reads)
	}
	if len(sourcesOf(readIndex(t, dir))) == 0 {
		t.Fatal("the index does not say what was not read")
	}
}

// There is no periodic collection: a scenario has a fixed number of
// checkpoints, and one past it reads nothing and is counted.
func TestCheckpointCountIsBounded(t *testing.T) {
	limits := testLimits()
	limits.Checkpoints = 2
	reads := 0
	f := newFakeLogs()
	f.hook = func(context.Context, string, io.Writer) error { reads++; return nil }
	b, dir := newTestBundle(t, f, limits, nil)
	b.Checkpoint(t.Context(), LogsBeforeFault, false)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	before := reads
	for range 5 {
		b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	}
	index := readIndex(t, dir)
	if reads != before || len(index.Checkpoints) != 2 || index.RefusedCheckpoints != 5 {
		t.Fatalf("reads %d, then %d; index %+v", before, reads, index)
	}
}

// A source that never returns, even ignoring its context, costs at most the
// bound; what was read before it stays, and the rest is named as not read.
func TestHangingSourceIsBoundedAndKeepsTheRest(t *testing.T) {
	limits := testLimits()
	limits.SourceTimeout, limits.CheckpointTimeout = 50*time.Millisecond, 150*time.Millisecond
	f := newFakeLogs()
	release := make(chan struct{})
	defer close(release)
	f.hook = func(_ context.Context, key string, w io.Writer) error {
		if key == testMaster {
			_, _ = io.WriteString(w, "2026-11-06T09:10:01.000000000Z whole line\npartial tok")
			<-release
		}
		return nil
	}
	b, dir := newTestBundle(t, f, limits, nil)
	start := time.Now()
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s", took)
	}
	index := readIndex(t, dir)
	s := findSource(t, index, LogInstanceManager, testMaster)
	if s.Status != LogTruncated || !strings.Contains(s.Reason, "deadline") {
		t.Fatalf("%+v", s)
	}
	data, err := os.ReadFile(filepath.Join(dir, s.File))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "2026-11-06T09:10:01.000000000Z whole line\n" {
		t.Errorf("kept %q: an unfinished line may hold a part of a credential", data)
	}
	if s := findSource(t, index, LogOperator, testManager); s.Status != LogCollected {
		t.Errorf("the source read before it was lost: %+v", s)
	}
}

// Cancelling a run keeps what was collected and names what was not.
func TestCancellationKeepsWhatWasCollected(t *testing.T) {
	f := newFakeLogs()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.hook = func(_ context.Context, key string, _ io.Writer) error {
		if key == testMaster {
			cancel()
		}
		return nil
	}
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(ctx, LogsAfterVerdict, true)
	index := readIndex(t, dir)
	if s := findSource(t, index, LogOperator, testManager); s.Status != LogCollected {
		t.Errorf("%+v", s)
	}
	s := findSource(t, index, LogBrokerProcess, "")
	if s.Status != LogUnavailable || !strings.Contains(s.Reason, "stopped") {
		t.Errorf("the source after the cancellation: %+v", s)
	}
	if b.IndexFile() == "" {
		t.Error("the index is not reported")
	}
}

// A run already out of time still leaves an index that says so.
func TestCancelledBeforeTheStartLeavesAnIndex(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := newFakeLogs()
	f.hook = func(context.Context, string, io.Writer) error {
		t.Error("read after the cancellation")
		return nil
	}
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(ctx, LogsAfterVerdict, true)
	for _, s := range sourcesOf(readIndex(t, dir)) {
		if s.Status != LogUnavailable {
			t.Errorf("%+v", s)
		}
	}
}

// A scenario that reached its deadline has its context cancelled before its
// deferred collection runs. The collection's own context goes on for the grace
// and no longer, so an expired suite still gets its logs and is not extended.
func TestLogContextOutlivesAnEndedScenarioOnlyByItsGrace(t *testing.T) {
	const grace = 200 * time.Millisecond
	scenario, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	ctx, stop := LogContext(scenario, grace)
	defer stop()
	if err := ctx.Err(); err != nil {
		t.Fatalf("the grace was not given: %v", err)
	}
	select {
	case <-ctx.Done():
		if took := time.Since(start); took < grace {
			t.Errorf("ended after %s, before its grace of %s", took, grace)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the context outlived its grace")
	}
}

// A collection that started in time is not cut short while the scenario's
// context lives, and has only the grace once it ends. Checkpoint's own timeout
// stays what bounds it in between.
func TestLogContextFollowsAScenarioThatEndsLater(t *testing.T) {
	const grace = 100 * time.Millisecond
	scenario, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx, stop := LogContext(scenario, grace)
	defer stop()
	select {
	case <-ctx.Done():
		t.Fatal("the context ended while the scenario's lives")
	case <-time.After(3 * grace):
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the context outlived the scenario's by more than the grace")
	}
}

// Ending the collection releases its context at once.
func TestLogContextEndsWithItsCollection(t *testing.T) {
	ctx, stop := LogContext(t.Context(), time.Hour)
	stop()
	if ctx.Err() == nil {
		t.Fatal("the context is still live")
	}
}

// No grace means the collection stops with the scenario, as Checkpoint does
// on the scenario's own context.
func TestLogContextWithoutAGraceEndsWithTheScenario(t *testing.T) {
	scenario, cancel := context.WithCancel(t.Context())
	cancel()
	ctx, stop := LogContext(scenario, 0)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the context did not end")
	}
}

// A hung source after the deadline costs the grace and the wait for the read
// that was told to stop, however long the source's own timeout is. What was
// read before it stays in the bundle, and what comes after it is named.
func TestReaderThatHangsAfterTheDeadlineIsBoundedByTheGrace(t *testing.T) {
	const grace = 200 * time.Millisecond
	f := newFakeLogs()
	release := make(chan struct{})
	defer close(release)
	f.hook = func(_ context.Context, key string, _ io.Writer) error {
		if key == testMaster {
			<-release // Ignores its context, like a stuck client.
		}
		return nil
	}
	limits := testLimits()
	limits.SourceTimeout, limits.CheckpointTimeout = time.Minute, time.Minute
	b, dir := newTestBundle(t, f, limits, nil)
	scenario, cancel := context.WithCancel(t.Context())
	cancel()
	ctx, stop := LogContext(scenario, grace)
	defer stop()

	start := time.Now()
	b.Checkpoint(ctx, LogsAfterVerdict, true)
	if took, limit := time.Since(start), grace+logReadGrace+2*time.Second; took > limit {
		t.Errorf("took %s, want at most %s", took, limit)
	}
	index := readIndex(t, dir)
	if s := findSource(t, index, LogOperator, testManager); s.Status != LogCollected {
		t.Errorf("the source before the hung one: %+v", s)
	}
	if s := findSource(t, index, LogBrokerProcess, ""); s.Status != LogUnavailable ||
		!strings.Contains(s.Reason, "stopped") {
		t.Errorf("the source after the hung one: %+v", s)
	}
}

// `cubrid heartbeat status` is not a passive read during a fault: it can turn
// off CUBRID's ping check, and change what the test measures (POC-52). It runs
// only once the fault has ended and the verdict is reached.
func TestHeartbeatStatusNeverRunsBeforeTheFaultEnded(t *testing.T) {
	f := newFakeLogs()
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsBeforeFault, false)
	if slices.ContainsFunc(f.execs, func(c []string) bool { return slices.Equal(c, HeartbeatStatus) }) {
		t.Fatalf("heartbeat status ran while a fault may be active: %q", f.execs)
	}
	s := findSource(t, readIndex(t, dir), LogHeartbeat, testMaster)
	if s.Status != LogDeferred || !strings.Contains(s.Reason, "not a passive") {
		t.Fatalf("%+v", s)
	}
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	if !slices.ContainsFunc(f.execs, func(c []string) bool { return slices.Equal(c, HeartbeatStatus) }) {
		t.Fatalf("heartbeat status did not run after the fault: %q", f.execs)
	}
}

// Every command run in a Pod is one of the fixed reads below. Nothing here may
// start, stop or change anything in CUBRID, whatever the listing returns.
func TestExecRunsOnlyFixedReadOnlyCommands(t *testing.T) {
	f := newFakeLogs()
	f.files += "$(cubrid heartbeat stop).err\n; cubrid service stop\n../../etc/passwd\n"
	b, _ := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	if len(f.execs) == 0 {
		t.Fatal("no command ran")
	}
	for _, command := range f.execs {
		switch {
		case slices.Equal(command, ListEngineLogs), slices.Equal(command, HeartbeatStatus):
			continue
		case len(command) == len(TailEngineLog("x", 1)) && slices.Equal(command[:3], TailEngineLog("x", 1)[:3]):
			if !AllowedEngineLog(command[len(command)-1]) {
				t.Errorf("read of a file not on the list: %q", command)
			}
		default:
			t.Errorf("unexpected command in a Pod: %q", command)
		}
	}
	for _, command := range [][]string{ListEngineLogs, HeartbeatStatus, TailEngineLog("x", 1)} {
		text := strings.Join(command, " ")
		for _, verb := range []string{" start", " stop", "restart", "reload", "changemode", "deregister", "rm ", " > "} {
			if strings.Contains(text, verb) {
				t.Errorf("%q holds %q", text, verb)
			}
		}
	}
}

// Only CUBRID's diagnostic files are read. SQL logs hold statements and bound
// values, configuration and token files hold credentials: neither is copied.
func TestAllowedEngineLog(t *testing.T) {
	for rel, want := range map[string]bool{
		testServerLog:                                   true,
		"ha-0_master.err":                               true,
		"appdb@ha-1_copylogdb.err":                      true,
		"appdb@localhost_applylogdb_appdb_ha-1.err":     true,
		"broker/error_log/rw_1.err":                     true,
		"broker/cubrid_broker.log":                      true,
		"server/appdb.err":                              true,
		"server/appdb_latest.err":                       false,
		"server/appdb_20261106_0910.access":             false,
		"broker/sql_log/rw_1.sql.log":                   false,
		"broker/sql_log/rw_1.slow.log":                  false,
		"../conf/cubrid.conf":                           false,
		"conf/cubrid_ha.conf":                           false,
		"databases.txt":                                 false,
		"/var/run/secrets/kubernetes.io/serviceaccount": false,
		"server/../../../etc/token.err":                 false,
		"token.err":                                     false,
		"kubeconfig.err":                                false,
		"cubrid_utility.log":                            false,
		"":                                              false,
	} {
		if got := AllowedEngineLog(rel); got != want {
			t.Errorf("AllowedEngineLog(%q) = %v, want %v", rel, got, want)
		}
	}
}

// A listed file past the list is not read, and the index says it was refused.
func TestRefusedFilesAreCountedNotCopied(t *testing.T) {
	f := newFakeLogs()
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	index := readIndex(t, dir)
	for path := range allBytes(t, dir) {
		if strings.Contains(path, "sql") || strings.Contains(path, "conf") {
			t.Errorf("copied %s", path)
		}
	}
	found := false
	for _, s := range sourcesOf(index) {
		found = found || (s.Component == LogEngine && strings.Contains(s.Reason, "not on the list"))
	}
	if !found {
		t.Errorf("no source names the refused files: %+v", sourcesOf(index))
	}
}

const (
	testRegistered = "registered-token-value-1234"
	testJWT        = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeXN0ZW06c2EifQ.c2lnbmF0dXJlLXZhbHVl"
)

var testRegisteredBase64 = base64.StdEncoding.EncodeToString([]byte(testRegistered))

// logSecrets is one line each of what a log could hold, and the part of it
// that must not survive.
var logSecrets = map[string]struct{ line, secret string }{
	"bearer header":     {`Authorization: Bearer sekret-bearer-value`, "sekret-bearer-value"},
	"instance token":    {`"token":"sekret-json-token"`, "sekret-json-token"},
	"registered":        {"the value " + testRegistered, testRegistered},
	"registered base64": {"data " + testRegisteredBase64, testRegisteredBase64},
	"environment":       {"CUBRID_PASSWORD=sekret-env", "sekret-env"},
	"jdbc":              {"jdbc:cubrid:ha-rw:33000:appdb:dba:sekret-jdbc:", "sekret-jdbc"},
	"sql user":          {"CREATE USER app PASSWORD 'sekret-sql';", "sekret-sql"},
	"sql user doubled":  {`ALTER USER app PASSWORD "sekret-sql2";`, "sekret-sql2"},
	"service account":   {"read " + testJWT + " from the projected volume", testJWT},
	"kubeconfig key":    {"    client-key-data: c2VrcmV0LWNsaWVudC1rZXk=", "c2VrcmV0LWNsaWVudC1rZXk="},
	"kubeconfig cert":   {"    client-certificate-data: c2VrcmV0LWNlcnQ=", "c2VrcmV0LWNlcnQ="},
	"kubeconfig token":  {"    token: sekret-kube-token", "sekret-kube-token"},
	"access key":        {"AKIAABCDEFGHIJKLMNOP", "AKIAABCDEFGHIJKLMNOP"},
}

// No source of a bundle leaves a credential in any file of it, the error text
// the index quotes included.
func TestNoCredentialInAnyFileOfTheBundle(t *testing.T) {
	var text strings.Builder
	for _, s := range logSecrets {
		fmt.Fprintf(&text, "2026-11-06T09:10:01.000000000Z %s\n", s.line)
	}
	r := NewRedactor(testRegistered)
	f := newFakeLogs()
	f.logs[testManager] = text.String()
	f.logs[testMaster] = text.String()
	f.logs[testBroker] = text.String()
	f.tails[testServerLog] = text.String()
	f.beat = text.String()
	f.logErr[testSlave] = errors.New(text.String())
	b, dir := newTestBundle(t, f, testLimits(), r)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	files := allBytes(t, dir)
	if len(files) < 5 {
		t.Fatalf("only %d files", len(files))
	}
	for name, s := range logSecrets {
		for path, content := range files {
			if strings.Contains(content, s.secret) {
				t.Errorf("%s: %s holds the secret", name, path)
			}
		}
	}
	if leaks, err := r.Leaks(dir); err != nil || len(leaks) != 0 {
		t.Errorf("leaks %v, %v", leaks, err)
	}
}

// A kept window can start inside a private key, after its first line: the
// lines of the key that remain are removed up to the end marker.
func TestKeyCutOffByTheBoundIsRemoved(t *testing.T) {
	const body = "c2VjcmV0LWtleS1ib2R5LWxpbmU="
	limits := testLimits()
	limits.SourceBytes = 256
	f := newFakeLogs()
	key := "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat(body+"\n", 40) + "-----END RSA PRIVATE KEY-----\n"
	f.logs[testMaster] = key + "2026-11-06T09:10:01.000000000Z after the key\n"
	b, dir := newTestBundle(t, f, limits, nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	s := findSource(t, readIndex(t, dir), LogInstanceManager, testMaster)
	data, err := os.ReadFile(filepath.Join(dir, s.File))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), body) {
		t.Fatalf("a line of the key survived: %q", data)
	}
	if !strings.Contains(string(data), "after the key") {
		t.Errorf("the line after the key was lost: %q", data)
	}
}

// The window's first line may start inside a credential: without the start
// of it the patterns cannot match, so a part line is never kept.
func TestCredentialCutOffByTheBoundIsNotKept(t *testing.T) {
	const line = "token=abcdefghijklmnopqrstuvwxyz0123456789 end\n"
	limits := testLimits()
	limits.SourceBytes = int64(len(line))*2 - 12 // starts inside the value
	f := newFakeLogs()
	f.logs[testMaster] = line + line + line
	b, dir := newTestBundle(t, f, limits, NewRedactor("abcdefghijklmnopqrstuvwxyz0123456789"))
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	s := findSource(t, readIndex(t, dir), LogInstanceManager, testMaster)
	data, err := os.ReadFile(filepath.Join(dir, s.File))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "0123456789") {
		t.Fatalf("a part of the credential was kept: %q", data)
	}
}

// Nothing unredacted reaches a path of the run, not even while it is read:
// content stays in memory until the redactor has been through it.
func TestRawContentNeverReachesTheEvidenceDirectory(t *testing.T) {
	const raw = "2026-11-06T09:10:01.000000000Z token=sekret-raw-value\n"
	f := newFakeLogs()
	var dir string
	f.hook = func(_ context.Context, key string, w io.Writer) error {
		if _, err := io.WriteString(w, raw); err != nil {
			return err
		}
		for path, content := range allBytes(t, dir) {
			if strings.Contains(content, "sekret-raw-value") {
				t.Errorf("during the read of %s, %s holds raw content", key, path)
			}
		}
		return nil
	}
	var b *LogBundle
	b, dir = newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	if len(allBytes(t, dir)) == 0 {
		t.Fatal("nothing was written")
	}
}

// A collector that fails stays the collector's problem: nothing escapes to the
// scenario that called it.
func TestCollectionFailureDoesNotReachTheCaller(t *testing.T) {
	t.Run("a reader that panics", func(t *testing.T) {
		f := newFakeLogs()
		f.hook = func(context.Context, string, io.Writer) error { panic("a broken reader") }
		b, dir := newTestBundle(t, f, testLimits(), nil)
		b.Checkpoint(t.Context(), LogsAfterVerdict, true)
		s := findSource(t, readIndex(t, dir), LogOperator, testManager)
		if s.Status != LogUnavailable || !strings.Contains(s.Reason, "a broken reader") {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("a directory that cannot be written", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		b := NewLogBundle(file, testLogScenario, nil, newFakeLogs(), testPlan(), testLimits(),
			NewLogBudget(1<<30, time.Hour))
		b.Checkpoint(t.Context(), LogsAfterVerdict, true)
		if b.IndexFile() != "" {
			t.Errorf("an index that was not written is reported: %q", b.IndexFile())
		}
		if b.Problem() == nil {
			t.Error("the failure is not reported")
		}
	})
	t.Run("a scenario that leaves the run directory", func(t *testing.T) {
		b := NewLogBundle(t.TempDir(), "../elsewhere", nil, newFakeLogs(), testPlan(), testLimits(),
			NewLogBudget(1<<30, time.Hour))
		b.Checkpoint(t.Context(), LogsAfterVerdict, true)
		if b.IndexFile() != "" || b.Problem() == nil {
			t.Errorf("index %q, problem %v", b.IndexFile(), b.Problem())
		}
	})
}

// The scenario's collection runs in a deferred call while a failed assertion
// is panicking. The guard around it must not end that panic: it carries the
// scenario's failure to Ginkgo.
func TestCollectionDoesNotSwallowTheCallersPanic(t *testing.T) {
	const failure = "the scenario's failed assertion"
	defer func() {
		if got := recover(); got != failure {
			t.Fatalf("recovered %v", got)
		}
	}()
	b, _ := newTestBundle(t, newFakeLogs(), testLimits(), nil)
	defer b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	panic(failure)
}

// The tail keeps the last bytes written and counts all of them.
func TestLogTail(t *testing.T) {
	tail := NewLogTail(4)
	for _, chunk := range []string{"ab", "cdef", "g", "hijklmn"} {
		if n, err := tail.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if string(tail.Bytes()) != "klmn" || tail.Total() != 14 {
		t.Fatalf("%q, %d", tail.Bytes(), tail.Total())
	}
}

// A log of any size costs a fixed amount of memory.
func TestLogTailMemoryIsBounded(t *testing.T) {
	tail := NewLogTail(16)
	for range 100000 {
		if _, err := tail.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	if size := cap(tail.data); size > 1024 {
		t.Fatalf("the tail of 16 bytes holds %d bytes", size)
	}
}

// Text with the form of a key's end has no key to end when the marker has gone.
func TestRedactLeavesTextWithoutAnEndMarkerAlone(t *testing.T) {
	const text = "ordinary log line\n"
	if got := string(NewRedactor().Redact([]byte(text))); got != text {
		t.Fatalf("%q", got)
	}
}
