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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testStateDir    = testLogScenario + "/cluster"
	testClusterUID  = "cluster-uid"
	testClusterItem = `{"metadata":{"name":"ha","uid":"cluster-uid","generation":3},` + testClusterStatus + `}`
	testClusterSpec = `"spec":{"image":"cubrid:11.4"}`
	testMemberPod   = `{"metadata":{"name":"ha-0","uid":"uid-0","annotations":{"kubectl.kubernetes.io/` +
		`last-applied-configuration":"password=hidden-in-annotation"}},"spec":{"nodeName":"node-a",` +
		`"containers":[{"name":"cubrid","env":[{"name":"DBA_PASSWORD","value":"hunter2-in-env"}]}]},` +
		`"status":{"phase":"Running","containerStatuses":[{"name":"cubrid","ready":true,"restartCount":2,` +
		`"state":{"running":{"startedAt":"2026-11-06T09:00:05Z"}}}]}}`
	testClusterStatus = `"status":{"observedGeneration":3,"currentPrimary":"ha-0","conditions":[{"type":"Ready",` +
		`"status":"True","reason":"AllGood","observedGeneration":3,"lastTransitionTime":"2026-11-06T09:00:10Z"}]}`
	testOperatorPod = `{"metadata":{"name":"manager-1","uid":"uid-m"},"spec":{"nodeName":"control"},` +
		`"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`
	// noOperator starts the reason of a status no running Operator keeps current.
	noOperator      = "no Operator Pod"
	testMemberUID   = "uid-0"
	testNode        = "node-a"
	testManagerNode = "control"
)

var testStateSince = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

func testStateList(items ...string) string {
	return `{"apiVersion":"v1","kind":"List","items":[` + strings.Join(items, ",") + `]}`
}

// testEvent is an Event of a Pod that was last observed at last.
func testEvent(uid, reason string, count int, first, last string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"uid":%q},"involvedObject":{"kind":"Pod","name":"ha-0","uid":"uid-0"},`+
		`"type":"Warning","reason":%q,"message":"a message","count":%d,"firstTimestamp":%q,"lastTimestamp":%q,`+
		`"source":{"component":"kubelet","host":"node-a"}}`, uid, uid, reason, count, first, last)
}

// fakeState plays the cluster a snapshot reads. Each listing answers with its
// entry in lists, or fails with its entry in errs; hook runs inside a read.
type fakeState struct {
	mu    sync.Mutex
	lists map[string]string
	errs  map[string]error
	hook  func(ctx context.Context, list StateList) error
	reads []StateList
}

func newFakeState() *fakeState {
	return &fakeState{errs: map[string]error{}, lists: map[string]string{
		StateCluster:      testStateList(testClusterItem),
		StatePods:         testStateList(testMemberPod),
		StateOperatorPods: testStateList(testOperatorPod),
		StateServices: testStateList(`{"metadata":{"name":"ha-rw","uid":"uid-s","annotations":{"note":` +
			`"password=hidden-in-annotation"}},"spec":{"type":"ClusterIP","clusterIP":"10.0.0.1"}}`),
		StateEndpointSlices: testStateList(`{"metadata":{"name":"ha-rw-abc","uid":"uid-e"},"addressType":"IPv4",` +
			`"endpoints":[{"addresses":["10.1.0.1"],"targetRef":{"kind":"Pod","name":"ha-0","uid":"uid-0"}}]}`),
		StateEvents: testStateList(testEvent("event-now", "Killing", 1, "2026-11-06T09:00:20Z",
			"2026-11-06T09:00:20Z")),
	}}
}

func (f *fakeState) List(ctx context.Context, list StateList, w io.Writer) error {
	f.mu.Lock()
	f.reads = append(f.reads, list)
	body, err, hook := f.lists[list.Source], f.errs[list.Source], f.hook
	f.mu.Unlock()
	if hook != nil {
		if herr := hook(ctx, list); herr != nil {
			return herr
		}
	}
	if err != nil {
		return err
	}
	_, werr := io.WriteString(w, body)
	return werr
}

func testStatePlan() StatePlan {
	return ClusterStatePlan("system", "ns", "ha", []string{testMaster})
}

func testState(dir string, f StateReader) *StateBundle {
	return NewStateBundle(dir, testLogScenario, NewRedactor(), f, testStatePlan(), DefaultStateLimits(),
		NewLogBudget(1<<30, time.Hour), testStateSince)
}

func readStateIndex(t *testing.T, dir string) StateIndex {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, testStateDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index StateIndex
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	return index
}

func stateSource(t *testing.T, index StateIndex, checkpoint, source string) StateSource {
	t.Helper()
	for _, cp := range index.Checkpoints {
		if cp.Name != checkpoint {
			continue
		}
		for _, s := range cp.Sources {
			if s.Source == source {
				return s
			}
		}
	}
	t.Fatalf("no entry of %s in %s: %+v", source, checkpoint, index)
	return StateSource{}
}

func readStateObservations(t *testing.T, dir string) []StateObservation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, testStateDir, "observations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []StateObservation
	for line := range strings.Lines(string(data)) {
		var o StateObservation
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		lines = append(lines, o)
	}
	return lines
}

// Each checkpoint stores the cluster's and the members' identities and states,
// in the scenario's own directory and in UTC, so that a reader sees what
// Kubernetes showed before the fault, at the fault and after the verdict.
func TestSnapshotsRecordIdentitiesAtEachCheckpoint(t *testing.T) {
	dir := t.TempDir()
	b := testState(dir, newFakeState())
	b.Checkpoint(t.Context(), LogsBeforeFault)
	b.Mark("fault_issued", "delete Pod ha-0")
	b.Mark("fault_confirmed", "")
	b.Checkpoint(t.Context(), StateFaultConfirmed)
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	if got, want := b.IndexFile(), testStateDir+"/index.json"; got != want {
		t.Fatalf("index file %q, want %q", got, want)
	}
	index := readStateIndex(t, dir)
	if index.SchemaVersion != StateSchemaVersion || index.Scenario != testLogScenario || index.Cluster != "ha" ||
		index.Namespace != "ns" || index.ClusterUID != testClusterUID {
		t.Fatalf("identity of the snapshot: %+v", index)
	}
	names := make([]string, 0, len(index.Checkpoints))
	for _, cp := range index.Checkpoints {
		names = append(names, cp.Name)
		if cp.StartedAt.Location() != time.UTC || cp.StartedAt.IsZero() || cp.FinishedAt.Before(cp.StartedAt) {
			t.Errorf("checkpoint %s is not in UTC or has no times: %+v", cp.Name, cp)
		}
		for _, s := range cp.Sources {
			if s.File != "" && !strings.HasPrefix(s.File, testStateDir+"/"+cp.Name+"/") {
				t.Errorf("a snapshot outside its checkpoint's directory: %s", s.File)
			}
			if s.File == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, s.File)); err != nil {
				t.Error(err)
			}
		}
	}
	if want := []string{LogsBeforeFault, StateFaultConfirmed, LogsAfterVerdict}; !slices.Equal(names, want) {
		t.Fatalf("checkpoints %v, want %v", names, want)
	}
	pods := stateSource(t, index, LogsBeforeFault, StatePods)
	member := StateObject{Name: testMaster, UID: testMemberUID, Node: testNode}
	if pods.Status != LogCollected || !slices.Contains(pods.Objects, member) {
		t.Fatalf("member identity: %+v", pods)
	}
	operator := stateSource(t, index, LogsBeforeFault, StateOperatorPods)
	if !slices.Contains(operator.Objects, StateObject{Name: testManager, UID: "uid-m", Node: testManagerNode}) {
		t.Fatalf("Operator identity: %+v", operator)
	}
	data, err := os.ReadFile(filepath.Join(dir, pods.File))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"restartCount":2`) {
		t.Fatalf("the container state is not in the snapshot:\n%s", data)
	}
}

// Times of different clocks must not read as one order: every observation
// names its clock, and the index says that no skew was measured.
func TestObservationsNameTheirClockAndSkewIsNotClaimed(t *testing.T) {
	dir := t.TempDir()
	b := testState(dir, newFakeState())
	b.Mark("fault_issued", "delete Pod ha-0")
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	index := readStateIndex(t, dir)
	if len(index.Clocks) == 0 || !strings.Contains(index.Ordering, "not") {
		t.Fatalf("the index does not limit the order of its times: %+v", index)
	}
	for _, c := range index.Clocks {
		if c.Skew != Unknown {
			t.Errorf("a skew nothing measured: %+v", c)
		}
	}
	seen := map[string]bool{}
	lines := readStateObservations(t, dir)
	for i, o := range lines {
		if o.Clock == "" || o.T.IsZero() {
			t.Errorf("an observation without a clock or time: %+v", o)
		}
		if o.T.Location() != time.UTC {
			t.Errorf("an observation not in UTC: %+v", o)
		}
		if i > 0 && o.T.Before(lines[i-1].T) {
			t.Errorf("observations out of time order: %+v", lines)
		}
		seen[o.Clock+" "+o.Kind] = true
	}
	for _, want := range []string{"test-runner fault_issued", "test-runner checkpoint_started",
		"operator condition", "node/node-a container_running", "reporter/kubelet event"} {
		if !seen[want] {
			t.Errorf("no %q among %v", want, seen)
		}
	}
}

// An Event is not evidence of this scenario unless it was observed during it.
// An aggregated Event carries the name of its first occurrence, so its last
// observation decides, and one that repeated since the scenario began keeps
// its place, marked as started earlier.
func TestSnapshotExcludesEventsOlderThanTheScenario(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	f.lists[StateEvents] = testStateList(
		testEvent("event-old", "OldKilling", 4, "2026-11-06T08:00:00Z", "2026-11-06T08:59:00Z"),
		testEvent("event-repeated", "BackOff", 9, "2026-11-06T08:00:00Z", "2026-11-06T09:01:00Z"),
		testEvent("event-now", "Killing", 1, "2026-11-06T09:02:00Z", "2026-11-06T09:02:00Z"),
		`{"metadata":{"name":"event-undated","uid":"event-undated"},"reason":"Undated","message":"no time",`+
			`"involvedObject":{"kind":"Pod","name":"ha-0"}}`,
		`{"metadata":{"name":"event-series","uid":"event-series"},"reason":"Seriesed","message":"a series",`+
			`"eventTime":"2026-11-06T08:00:00.000000Z","series":{"count":3,"lastObservedTime":"2026-11-06T09:03:00.000000Z"},`+
			`"involvedObject":{"kind":"Pod","name":"ha-0"},"reportingComponent":"scheduler"}`,
		`{"metadata":{"name":"event-former","uid":"event-former"},"reason":"FormerCluster","message":"same name",`+
			`"firstTimestamp":"2026-11-06T09:04:00Z","lastTimestamp":"2026-11-06T09:04:00Z",`+
			`"involvedObject":{"kind":"CubridCluster","name":"ha","uid":"former-uid"}}`)
	b := testState(dir, f)
	b.Checkpoint(t.Context(), LogsBeforeFault)

	source := stateSource(t, readStateIndex(t, dir), LogsBeforeFault, StateEvents)
	want := StateExcluded{OlderThanScenario: 1, Undated: 1, OtherCluster: 1}
	if source.Excluded == nil || *source.Excluded != want {
		t.Fatalf("excluded %+v, want %+v", source.Excluded, want)
	}
	if *source.Listed != 6 || source.Stored != 3 || source.Status != LogCollected {
		t.Fatalf("source %+v", source)
	}
	data, err := os.ReadFile(filepath.Join(dir, source.File))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, gone := range []string{"OldKilling", "Undated", "FormerCluster"} {
		if strings.Contains(text, gone) {
			t.Errorf("stale Event %s is in the snapshot:\n%s", gone, text)
		}
	}
	for _, kept := range []string{"BackOff", "Killing", "Seriesed", `"beganBeforeScenario":true`} {
		if !strings.Contains(text, kept) {
			t.Errorf("no %s in the snapshot:\n%s", kept, text)
		}
	}
}

// An Event already stored and not observed again says nothing of the fault,
// whatever the clocks say; one that repeated does.
func TestLaterCheckpointKeepsOnlyEventsThatChanged(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	same := testEvent("event-same", "Scheduled", 1, "2026-11-06T09:00:30Z", "2026-11-06T09:00:30Z")
	f.lists[StateEvents] = testStateList(same, testEvent("event-grows", "Unhealthy", 1, "2026-11-06T09:00:40Z",
		"2026-11-06T09:00:40Z"))
	b := testState(dir, f)
	b.Checkpoint(t.Context(), LogsBeforeFault)
	f.lists[StateEvents] = testStateList(same, testEvent("event-grows", "Unhealthy", 2, "2026-11-06T09:00:40Z",
		"2026-11-06T09:00:40Z"))
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	source := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateEvents)
	if source.Stored != 1 || source.Excluded == nil || source.Excluded.UnchangedSinceEarlierCheckpoint != 1 {
		t.Fatalf("source %+v", source)
	}
}

// Events that are all older than the scenario are not a healthy, empty
// namespace: the source says that nothing current was found.
func TestOnlyStaleEventsAreObsoleteNotEmpty(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	f.lists[StateEvents] = testStateList(testEvent("event-old", "Old", 1, "2026-11-06T08:00:00Z", "2026-11-06T08:00:00Z"))
	b := testState(dir, f)
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	source := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateEvents)
	if source.Status != StateObsolete || source.Reason == "" || source.File != "" {
		t.Fatalf("source %+v", source)
	}
}

// Conditions written for an older generation describe an older specification.
func TestConditionsBehindTheGenerationAreObsolete(t *testing.T) {
	for name, item := range map[string]string{
		"status": `{"metadata":{"name":"ha","uid":"cluster-uid","generation":5},` + testClusterStatus + `}`,
		"condition": `{"metadata":{"name":"ha","uid":"cluster-uid","generation":3},"status":{"observedGeneration":3,` +
			`"conditions":[{"type":"Ready","status":"True","observedGeneration":2}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			f := newFakeState()
			f.lists[StateCluster] = testStateList(item)
			b := testState(dir, f)
			b.Checkpoint(t.Context(), LogsAfterVerdict)
			source := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateCluster)
			if source.Status != StateObsolete || source.Reason == "" || source.File == "" {
				t.Fatalf("source %+v", source)
			}
		})
	}
}

// A CubridCluster that cannot be found, and a source that cannot be read, are
// named as such and never reported as empty or healthy, and the other sources
// are still stored.
func TestMissingSourcesAreNamedAndTheRestIsKept(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	f.lists[StateCluster] = testStateList()
	f.errs[StateServices] = errors.New("services are forbidden")
	f.errs[StatePods] = errors.New("the API server is gone")
	f.lists[StateEndpointSlices] = "{not json"
	b := testState(dir, f)
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	index := readStateIndex(t, dir)
	for source, want := range map[string]string{StateCluster: "not found", StateServices: "forbidden",
		StatePods: "API server", StateEndpointSlices: "not a Kubernetes list"} {
		got := stateSource(t, index, LogsAfterVerdict, source)
		if got.Status != LogUnavailable || !strings.Contains(got.Reason, want) {
			t.Errorf("%s: %+v, want the reason %q", source, got, want)
		}
	}
	// Nothing was looked for in a listing that failed, so nothing is called
	// missing: the reason says the listing failed.
	if got := stateSource(t, index, LogsAfterVerdict, StatePods); len(got.Missing) != 0 {
		t.Errorf("a member is named as missing from a listing that was not read: %+v", got)
	}
	if got := stateSource(t, index, LogsAfterVerdict, StateEvents); got.Status != LogCollected || got.File == "" {
		t.Errorf("a source that answered was not kept: %+v", got)
	}
}

// The Operator's metrics need a credential a passive read does not hold, so
// each checkpoint says that they were not read instead of leaving them out.
func TestUnreadMetricsAreNamed(t *testing.T) {
	dir := t.TempDir()
	b := testState(dir, newFakeState())
	b.Checkpoint(t.Context(), LogsAfterVerdict)
	got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateOperatorMetrics)
	if got.Status != LogUnavailable || got.Reason == "" || strings.Contains(got.Reason, redacted) {
		t.Fatalf("source %+v", got)
	}
}

// A member the fault removed leaves the snapshot partial, and a Pod replaced
// under the same name keeps its old UID in the index: the identity of what was
// lost is what a reader needs.
func TestDisappearedAndReplacedPodsAreNamed(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	b := NewStateBundle(dir, testLogScenario, NewRedactor(), f,
		ClusterStatePlan("system", "ns", "ha", []string{testMaster, testSlave}), DefaultStateLimits(),
		NewLogBudget(1<<30, time.Hour), testStateSince)
	pod := func(name, uid string) string {
		return fmt.Sprintf(`{"metadata":{"name":%q,"uid":%q},"spec":{"nodeName":"node-a"},"status":{"phase":"Running"}}`,
			name, uid)
	}
	f.lists[StatePods] = testStateList(pod(testMaster, testMemberUID), pod(testSlave, "uid-1"))
	b.Checkpoint(t.Context(), LogsBeforeFault)
	f.lists[StatePods] = testStateList(pod(testSlave, "uid-1-new"))
	b.Checkpoint(t.Context(), StateFaultConfirmed)

	got := stateSource(t, readStateIndex(t, dir), StateFaultConfirmed, StatePods)
	if got.Status != LogTruncated || got.File == "" || !slices.Equal(got.Missing, []string{testMaster}) {
		t.Fatalf("source %+v", got)
	}
	if !slices.Contains(got.Gone, StateObject{Name: testMaster, UID: testMemberUID, Node: testNode}) ||
		!slices.Contains(got.Gone, StateObject{Name: testSlave, UID: "uid-1", Node: testNode}) {
		t.Fatalf("the lost Pods are not named: %+v", got)
	}
}

// Only what the snapshot names is copied: an environment variable, an
// annotation or the specification of an object never reach a file, and what
// is copied goes through the redaction.
func TestSnapshotHoldsOnlyAllowedFieldsAndRedacts(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	f.lists[StateCluster] = testStateList(`{"metadata":{"name":"ha","uid":"cluster-uid","generation":3,` +
		`"annotations":{"x":"y"}},` + testClusterSpec + `,` + testClusterStatus + `}`)
	f.lists[StateEvents] = testStateList(fmt.Sprintf(`{"metadata":{"name":"e","uid":"e"},"reason":"Failed",`+
		`"message":"could not log in with password=hunter3 and %s","lastTimestamp":"2026-11-06T09:02:00Z",`+
		`"involvedObject":{"kind":"Pod","name":"ha-0"}}`, "registered-secret"))
	r := NewRedactor("registered-secret")
	b := NewStateBundle(dir, testLogScenario, r, f, testStatePlan(), DefaultStateLimits(),
		NewLogBudget(1<<30, time.Hour), testStateSince)
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	err := filepath.WalkDir(filepath.Join(dir, testLogScenario), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{"hunter2-in-env", "hidden-in-annotation", "hunter3", "registered-secret",
			"DBA_PASSWORD", "cubrid:11.4"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s holds %q", path, secret)
			}
		}
		if strings.HasSuffix(path, ".json") && !json.Valid(data) {
			t.Errorf("%s is not valid JSON after the redaction:\n%s", path, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.IndexFile() == "" {
		t.Fatal("nothing was stored")
	}
}

// A store that fails is reported and panics nothing.
func TestStoreFailureIsOnlyReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := testState(dir, newFakeState())
	b.Checkpoint(t.Context(), LogsAfterVerdict)
	if b.Problem() == nil || b.IndexFile() != "" {
		t.Fatalf("problem %v, index %q", b.Problem(), b.IndexFile())
	}
}

// A listing past the read bound is not parsed: a part of a JSON document is
// not a part of the cluster.
func TestListingPastTheReadBoundIsNamed(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	limits := DefaultStateLimits()
	limits.ReadBytes = int64(len(testStateList(testMemberPod))) - 1
	b := NewStateBundle(dir, testLogScenario, NewRedactor(), f, testStatePlan(), limits,
		NewLogBudget(1<<30, time.Hour), testStateSince)
	b.Checkpoint(t.Context(), LogsAfterVerdict)
	got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StatePods)
	if got.Status != LogUnavailable || !strings.Contains(got.Reason, "read bound") || got.File != "" {
		t.Fatalf("source %+v", got)
	}
}

// What does not fit a source's bound leaves its oldest objects out, whole, and
// says so.
func TestSourceBoundKeepsTheNewestWholeObjects(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	events := make([]string, 0, 50)
	for i := range 50 {
		stamp := fmt.Sprintf("2026-11-06T09:%02d:00Z", i)
		events = append(events, testEvent(fmt.Sprintf("event-%02d", i), fmt.Sprintf("Reason%02d", i), 1, stamp, stamp))
	}
	f.lists[StateEvents] = testStateList(events...)
	limits := DefaultStateLimits()
	limits.SourceBytes = 2048
	b := NewStateBundle(dir, testLogScenario, NewRedactor(), f, testStatePlan(), limits,
		NewLogBudget(1<<30, time.Hour), testStateSince)
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateEvents)
	if got.Status != LogTruncated || got.Stored == 0 || got.Stored >= 50 || got.Reason == "" {
		t.Fatalf("source %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, got.File))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) > limits.SourceBytes+512 || !json.Valid(data) || !strings.Contains(string(data), "Reason49") ||
		strings.Contains(string(data), "Reason00") {
		t.Fatalf("%d bytes:\n%s", len(data), data)
	}
}

func TestSnapshotBoundsAScenarioAndARun(t *testing.T) {
	t.Run("checkpoints", func(t *testing.T) {
		dir := t.TempDir()
		f := newFakeState()
		b := testState(dir, f)
		for range 4 {
			b.Checkpoint(t.Context(), LogsAfterVerdict)
		}
		index := readStateIndex(t, dir)
		if len(index.Checkpoints) != 3 || index.RefusedCheckpoints != 1 {
			t.Fatalf("index %+v", index)
		}
	})
	t.Run("the bytes of a run", func(t *testing.T) {
		dir := t.TempDir()
		b := NewStateBundle(dir, testLogScenario, NewRedactor(), newFakeState(), testStatePlan(), DefaultStateLimits(),
			NewLogBudget(0, time.Hour), testStateSince)
		b.Checkpoint(t.Context(), LogsAfterVerdict)
		got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StatePods)
		if got.Status != LogUnavailable || !strings.Contains(got.Reason, "size bound") {
			t.Fatalf("source %+v", got)
		}
	})
	t.Run("the time of a run", func(t *testing.T) {
		dir := t.TempDir()
		budget := NewLogBudget(1<<30, 50*time.Millisecond)
		f := newFakeState()
		f.hook = func(ctx context.Context, _ StateList) error {
			<-ctx.Done()
			return ctx.Err()
		}
		b := NewStateBundle(dir, testLogScenario, NewRedactor(), f, testStatePlan(), DefaultStateLimits(), budget,
			testStateSince)
		b.Checkpoint(t.Context(), LogsAfterVerdict)
		if _, wall := budget.left(); wall > 0 {
			t.Fatalf("the time spent was not charged: %s left", wall)
		}
		next := NewStateBundle(dir, testLogScenario, NewRedactor(), newFakeState(), testStatePlan(), DefaultStateLimits(),
			budget, testStateSince)
		next.Checkpoint(t.Context(), LogsAfterVerdict)
		index := readStateIndex(t, dir)
		for _, s := range index.Checkpoints[len(index.Checkpoints)-1].Sources {
			if s.Status != LogUnavailable {
				t.Fatalf("a source read after the run's time was used up: %+v", s)
			}
		}
	})
}

// A reader that never returns costs one checkpoint's bound, and what it did
// not reach is named.
func TestHungReaderIsBounded(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	f.hook = func(context.Context, StateList) error {
		<-never
		return nil
	}
	limits := DefaultStateLimits()
	limits.SourceTimeout, limits.CheckpointTimeout = 20*time.Millisecond, 40*time.Millisecond
	b := NewStateBundle(dir, testLogScenario, NewRedactor(), f, testStatePlan(), limits,
		NewLogBudget(1<<30, time.Hour), testStateSince)
	start := time.Now()
	b.Checkpoint(t.Context(), LogsAfterVerdict)
	if took := time.Since(start); took > limits.CheckpointTimeout+2*logReadGrace+time.Second {
		t.Fatalf("the checkpoint took %s", took)
	}
	index := readStateIndex(t, dir)
	sources := index.Checkpoints[0].Sources
	if len(sources) != len(testStatePlan().Lists)+1 {
		t.Fatalf("not every source is named: %+v", sources)
	}
	for _, s := range sources {
		if s.Status != LogUnavailable {
			t.Errorf("source %+v", s)
		}
	}
}

// A cancelled scenario keeps what was read before it, and the index stays
// whole; a panic of the collection is reported and goes no further.
func TestInterruptedSnapshotKeepsItsPart(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	ctx, cancel := context.WithCancel(t.Context())
	f.hook = func(_ context.Context, list StateList) error {
		if list.Source == StatePods {
			cancel()
		}
		return nil
	}
	b := testState(dir, f)
	b.Checkpoint(ctx, LogsAfterVerdict)
	index := readStateIndex(t, dir)
	if got := stateSource(t, index, LogsAfterVerdict, StateCluster); got.Status != LogCollected {
		t.Fatalf("the source before the cancellation: %+v", got)
	}
	if got := stateSource(t, index, LogsAfterVerdict, StateEvents); got.Status != LogUnavailable {
		t.Fatalf("the source after the cancellation: %+v", got)
	}

	dir = t.TempDir()
	f = newFakeState()
	f.hook = func(context.Context, StateList) error { panic("a broken reader") }
	b = testState(dir, f)
	b.Checkpoint(t.Context(), LogsAfterVerdict)
	if got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StatePods); got.Status != LogUnavailable {
		t.Fatalf("source %+v", got)
	}
}

// The steps of a fault are marked from the fault's goroutine while a
// checkpoint runs on another.
func TestMarkIsSafeDuringACheckpoint(t *testing.T) {
	dir := t.TempDir()
	b := testState(dir, newFakeState())
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 100 {
			b.Mark("fault_step", fmt.Sprint(i))
		}
	})
	b.Checkpoint(t.Context(), LogsBeforeFault)
	wg.Wait()
	b.Checkpoint(t.Context(), LogsAfterVerdict)
	count := 0
	for _, o := range readStateObservations(t, dir) {
		if o.Kind == "fault_step" {
			count++
		}
	}
	if count != 100 {
		t.Fatalf("%d marks kept", count)
	}
}

// testObservedCluster is the CubridCluster of an unchanged generation whose
// Conditions are all True and whose status was last observed age ago.
func testObservedCluster(age time.Duration) string {
	return `{"metadata":{"name":"ha","uid":"cluster-uid","generation":3},"status":{"observedGeneration":3,` +
		`"lastObservedTime":"` + time.Now().Add(-age).UTC().Format(time.RFC3339) + `","conditions":[{"type":"Ready",` +
		`"status":"True","observedGeneration":3}]}}`
}

// A stopped Operator cannot mark its own status as stale, so a status of an
// unchanged generation can say True long after anything checked it. The first
// real snapshot of an Operator being replaced (S06) held Ready, HAReady and
// PrimaryResolved True, 17 seconds old, beside a replacement Pod that was
// Running with its manager container not ready yet: that source must not read
// as collected and healthy. Below the limit, with a ready Operator, it must.
func TestStatusNobodyKeepsCurrentIsObsolete(t *testing.T) {
	const fresh, stale = 17 * time.Second, 2 * time.Minute
	ready := testStateList(testOperatorPod)
	unready := `{"metadata":{"name":"manager-2","uid":"uid-n"},"spec":{"nodeName":"control"},` +
		`"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],` +
		`"containerStatuses":[{"name":"manager","ready":false,` +
		`"state":{"running":{"startedAt":"2026-11-07T03:35:14Z"}}}]}}`
	deleting := `{"metadata":{"name":"manager-3","uid":"uid-d","deletionTimestamp":"2026-11-07T03:35:14Z"},` +
		`"spec":{"nodeName":"control"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`
	for name, tc := range map[string]struct {
		age      time.Duration
		operator string
		want     LogStatus
		reason   string
	}{
		"fresh with a ready Operator": {age: fresh, operator: ready, want: LogCollected},
		"past the limit":              {age: stale, operator: ready, want: StateObsolete, reason: "older than"},
		"no Operator Pod":             {age: fresh, operator: testStateList(), want: StateObsolete, reason: noOperator},
		"an Operator Pod not ready yet": {age: fresh, operator: testStateList(unready), want: StateObsolete,
			reason: noOperator},
		"an Operator Pod being deleted": {age: fresh, operator: testStateList(deleting), want: StateObsolete,
			reason: noOperator},
		"a ready Pod beside one not ready": {age: fresh, operator: testStateList(unready, testOperatorPod),
			want: LogCollected},
		"both at once": {age: stale, operator: testStateList(), want: StateObsolete, reason: noOperator},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			f := newFakeState()
			f.lists[StateCluster] = testStateList(testObservedCluster(tc.age))
			f.lists[StateOperatorPods] = tc.operator
			b := testState(dir, f)
			b.Checkpoint(t.Context(), StateFaultConfirmed)

			got := stateSource(t, readStateIndex(t, dir), StateFaultConfirmed, StateCluster)
			if got.Status != tc.want || !strings.Contains(got.Reason, tc.reason) || got.File == "" {
				t.Fatalf("source %+v, want %q with a reason naming %q", got, tc.want, tc.reason)
			}
			age, err := time.ParseDuration(got.StatusAge)
			if err != nil || age < tc.age-time.Second || age > tc.age+time.Minute {
				t.Fatalf("status age %q of a status %s old: %v", got.StatusAge, tc.age, err)
			}
			if got.LastObservedTime == nil {
				t.Fatalf("source %+v has no lastObservedTime", got)
			}
		})
	}
}

// The limit is the reader rule of docs/observability.md, with the one second
// the whole-second times are allowed in test/faults.
func TestStatusAgeLimitIsTheReaderRule(t *testing.T) {
	if StatusStaleAfter != time.Minute {
		t.Fatalf("limit %s", StatusStaleAfter)
	}
	for age, want := range map[time.Duration]LogStatus{StatusStaleAfter - time.Second: LogCollected,
		StatusStaleAfter + 3*time.Second: StateObsolete} {
		dir := t.TempDir()
		f := newFakeState()
		f.lists[StateCluster] = testStateList(testObservedCluster(age))
		b := testState(dir, f)
		b.Checkpoint(t.Context(), LogsAfterVerdict)
		if got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateCluster); got.Status != want {
			t.Errorf("age %s: %+v, want %q", age, got, want)
		}
	}
}

// What is not known stays unknown: a status with no observation time has no age
// of zero, and an Operator listing that failed proves neither a running nor a
// stopped Operator.
func TestStatusAgeAndOperatorThatAreNotKnownAreSaid(t *testing.T) {
	t.Run("no observation time", func(t *testing.T) {
		dir := t.TempDir()
		b := testState(dir, newFakeState())
		b.Checkpoint(t.Context(), LogsAfterVerdict)
		got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateCluster)
		if got.StatusAge != Unknown || got.LastObservedTime != nil || !strings.Contains(got.Reason, "lastObservedTime") {
			t.Fatalf("source %+v", got)
		}
	})
	t.Run("an Operator listing that failed", func(t *testing.T) {
		dir := t.TempDir()
		f := newFakeState()
		f.lists[StateCluster] = testStateList(testObservedCluster(time.Second))
		f.errs[StateOperatorPods] = errors.New("the API server is gone")
		b := testState(dir, f)
		b.Checkpoint(t.Context(), LogsAfterVerdict)
		got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateCluster)
		if got.Status != LogCollected || !strings.Contains(got.Reason, "not known") {
			t.Fatalf("source %+v", got)
		}
	})
}

// The reasons of a status behind its generation and of a status past its age
// are both kept.
func TestObsoleteStatusKeepsEveryReason(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	item := strings.Replace(testObservedCluster(time.Hour), `"generation":3`, `"generation":5`, 1)
	f.lists[StateCluster] = testStateList(item)
	f.lists[StateOperatorPods] = testStateList()
	b := testState(dir, f)
	b.Checkpoint(t.Context(), LogsAfterVerdict)
	got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateCluster)
	for _, want := range []string{"generation 5", "older than", noOperator} {
		if got.Status != StateObsolete || !strings.Contains(got.Reason, want) {
			t.Errorf("source %+v, want the reason %q", got, want)
		}
	}
}

// An Event counts as stored only once a file holds it. Each way of losing one
// leaves it to the next checkpoint, instead of counting it as unchanged.
func TestEventIsRememberedOnlyOnceStored(t *testing.T) {
	event := testEvent("event-now", "Killing", 1, "2026-11-06T09:00:20Z", "2026-11-06T09:00:20Z")
	check := func(t *testing.T, dir string) {
		t.Helper()
		got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateEvents)
		if got.Status != LogCollected || got.Stored != 1 || got.Excluded.UnchangedSinceEarlierCheckpoint != 0 {
			t.Fatalf("source %+v, excluded %+v", got, got.Excluded)
		}
	}
	t.Run("a failed write", func(t *testing.T) {
		dir := t.TempDir()
		b := testState(dir, newFakeState())
		// A file where the directory of the first checkpoint has to be.
		if err := os.MkdirAll(filepath.Join(dir, testStateDir), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, testStateDir, LogsBeforeFault), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		b.Checkpoint(t.Context(), LogsBeforeFault)
		b.Checkpoint(t.Context(), LogsAfterVerdict)
		check(t, dir)
	})
	t.Run("a listing with a malformed item", func(t *testing.T) {
		dir := t.TempDir()
		f := newFakeState()
		f.lists[StateEvents] = testStateList(event, `{"metadata":"not an object"}`)
		b := testState(dir, f)
		b.Checkpoint(t.Context(), LogsBeforeFault)
		if got := stateSource(t, readStateIndex(t, dir), LogsBeforeFault, StateEvents); got.Status != LogUnavailable {
			t.Fatalf("source %+v", got)
		}
		f.lists[StateEvents] = testStateList(event)
		b.Checkpoint(t.Context(), LogsAfterVerdict)
		check(t, dir)
	})
	t.Run("the size bound", func(t *testing.T) {
		dir := t.TempDir()
		f := newFakeState()
		events := make([]string, 0, 50)
		for i := range 50 {
			stamp := fmt.Sprintf("2026-11-06T09:%02d:00Z", i)
			events = append(events, testEvent(fmt.Sprintf("event-%02d", i), fmt.Sprintf("Reason%02d", i), 1, stamp, stamp))
		}
		f.lists[StateEvents] = testStateList(events...)
		limits := DefaultStateLimits()
		limits.SourceBytes = 2048
		b := NewStateBundle(dir, testLogScenario, NewRedactor(), f, testStatePlan(), limits,
			NewLogBudget(1<<30, time.Hour), testStateSince)
		b.Checkpoint(t.Context(), LogsBeforeFault)
		first := stateSource(t, readStateIndex(t, dir), LogsBeforeFault, StateEvents)
		b.Checkpoint(t.Context(), LogsAfterVerdict)
		second := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateEvents)
		if first.Stored == 0 || second.Stored == 0 || second.Excluded.UnchangedSinceEarlierCheckpoint != first.Stored {
			t.Fatalf("first %+v, second %+v, excluded %+v", first, second, second.Excluded)
		}
	})
}

// Two snapshots never run side by side: the state of one scenario's snapshots
// has one writer. The fault's snapshot runs beside the scenario, which is why
// this has to hold for any caller.
func TestCheckpointsRunOneAtATime(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	var running, overlapped atomic.Int32
	f.hook = func(context.Context, StateList) error {
		if running.Add(1) > 1 {
			overlapped.Add(1)
		}
		time.Sleep(time.Millisecond)
		running.Add(-1)
		return nil
	}
	b := testState(dir, f)
	var wg sync.WaitGroup
	wg.Go(func() { b.Checkpoint(t.Context(), LogsBeforeFault) })
	wg.Go(func() { b.Checkpoint(t.Context(), StateFaultConfirmed) })
	wg.Go(func() {
		for b.Problem() == nil && b.IndexFile() == "" {
			time.Sleep(time.Millisecond)
		}
	})
	wg.Wait()
	if n := overlapped.Load(); n != 0 {
		t.Fatalf("%d reads ran beside another snapshot", n)
	}
	if got := readStateIndex(t, dir); len(got.Checkpoints) != 2 {
		t.Fatalf("index %+v", got)
	}
}

// A source that was not read has no count: an unknown number is not zero. In
// the same index a source that answered with nothing does say zero.
func TestUnreadSourceHasNoCount(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	f.errs[StatePods] = errors.New("the API server is gone")
	f.lists[StateServices] = testStateList()
	b := testState(dir, f)
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	data, err := os.ReadFile(filepath.Join(dir, testStateDir, stateIndexName))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Checkpoints []struct {
			Sources []map[string]any `json:"sources"`
		} `json:"checkpoints"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	for _, s := range index.Checkpoints[0].Sources {
		listed, has := s["listed"]
		switch s["source"] {
		case StatePods, StateOperatorMetrics:
			if has {
				t.Errorf("%v was not read and says listed %v", s["source"], listed)
			}
		case StateServices:
			if listed != float64(0) {
				t.Errorf("services answered with nothing and say listed %v, present %t", listed, has)
			}
		}
	}
}

// The index names what the file holds, so a truncated source names the objects
// a reader can open, and no others.
func TestTruncatedSourceNamesOnlyWhatItsFileHolds(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	services := make([]string, 0, 40)
	for i := range 40 {
		services = append(services, fmt.Sprintf(`{"metadata":{"name":"service-%02d","uid":"uid-%02d"},`+
			`"spec":{"type":"ClusterIP"}}`, i, i))
	}
	f.lists[StateServices] = testStateList(services...)
	limits := DefaultStateLimits()
	limits.SourceBytes = 1024
	b := NewStateBundle(dir, testLogScenario, NewRedactor(), f, testStatePlan(), limits,
		NewLogBudget(1<<30, time.Hour), testStateSince)
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	got := stateSource(t, readStateIndex(t, dir), LogsAfterVerdict, StateServices)
	if got.Status != LogTruncated || *got.Listed != 40 || got.Stored == 0 || len(got.Objects) != got.Stored {
		t.Fatalf("source %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, got.File))
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range got.Objects {
		if !strings.Contains(string(data), `"`+object.Name+`"`) {
			t.Errorf("%s is named and not in the file", object.Name)
		}
	}
}

// observations.jsonl keeps the newest lines its bound allows, and the index says
// how many it had to drop.
func TestObservationBoundKeepsTheNewestAndCountsTheRest(t *testing.T) {
	dir := t.TempDir()
	limits := DefaultStateLimits()
	limits.ObservationBytes = 2048
	b := NewStateBundle(dir, testLogScenario, NewRedactor(), newFakeState(), testStatePlan(), limits,
		NewLogBudget(1<<30, time.Hour), testStateSince)
	const steps = 200
	for i := range steps {
		b.Mark("fault_step", fmt.Sprintf("step-%03d", i))
		time.Sleep(time.Microsecond)
	}
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	index := readStateIndex(t, dir)
	data, err := os.ReadFile(filepath.Join(dir, index.Observations))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) > limits.ObservationBytes {
		t.Fatalf("%d bytes past the bound of %d", len(data), limits.ObservationBytes)
	}
	lines := readStateObservations(t, dir)
	if index.DroppedObservations == 0 || len(lines) == 0 {
		t.Fatalf("%d lines kept, %d dropped", len(lines), index.DroppedObservations)
	}
	if !strings.Contains(string(data), "checkpoint_finished") || strings.Contains(string(data), "step-000") {
		t.Fatalf("the newest lines were not the ones kept:\n%s", data)
	}
	// The checkpoint adds two lines, and the Conditions and containers of the
	// fixture a few more; every line kept or dropped is counted once.
	if total := len(lines) + index.DroppedObservations; total <= steps {
		t.Fatalf("%d lines kept and %d dropped of more than %d", len(lines), index.DroppedObservations, steps)
	}
}

// Everything a scenario stores stays within BundleBytes, the wrapper of each
// file included, and a source past it is named, not silently left out.
func TestBundleBoundHoldsWhatAScenarioStores(t *testing.T) {
	dir := t.TempDir()
	f := newFakeState()
	services := make([]string, 0, 40)
	for i := range 40 {
		services = append(services, fmt.Sprintf(`{"metadata":{"name":"service-%02d","uid":"uid-%02d"},`+
			`"spec":{"type":"ClusterIP"}}`, i, i))
	}
	f.lists[StateServices] = testStateList(services...)
	limits := DefaultStateLimits()
	limits.SourceBytes, limits.ObservationBytes, limits.BundleBytes = 1024, 1024, 4096
	b := NewStateBundle(dir, testLogScenario, NewRedactor(), f, testStatePlan(), limits,
		NewLogBudget(1<<30, time.Hour), testStateSince)
	for _, name := range []string{LogsBeforeFault, StateFaultConfirmed, LogsAfterVerdict} {
		b.Checkpoint(t.Context(), name)
	}

	var total int64
	err := filepath.WalkDir(filepath.Join(dir, testStateDir), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() == stateIndexName {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			total += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if total > limits.BundleBytes {
		t.Errorf("%d bytes stored past the bound of %d", total, limits.BundleBytes)
	}
	bounded := false
	for _, cp := range readStateIndex(t, dir).Checkpoints {
		for _, s := range cp.Sources {
			bounded = bounded || (s.Status != LogCollected && strings.Contains(s.Reason, "size bound"))
		}
	}
	if !bounded {
		t.Errorf("no source names the bound, with %d bytes stored", total)
	}
}

// The plan reads this cluster's objects and Pods by label, so that an Operator
// Pod that replaced another is found, and nothing it reads is a Secret.
func TestPlanListsOnlyTheClusterByLabel(t *testing.T) {
	plan := testStatePlan()
	if plan.Cluster != "ha" || plan.Namespace != "ns" {
		t.Fatalf("plan %+v", plan)
	}
	want := map[string]bool{StateCluster: true, StatePods: true, StateOperatorPods: true, StateServices: true,
		StateEndpointSlices: true, StateEvents: true}
	for _, list := range plan.Lists {
		if !want[list.Source] {
			t.Errorf("an unexpected list: %+v", list)
		}
		delete(want, list.Source)
		if strings.Contains(strings.ToLower(list.Resource), "secret") {
			t.Errorf("a list of Secrets: %+v", list)
		}
		switch list.Source {
		case StateCluster:
			if list.Field != "metadata.name=ha" {
				t.Errorf("list %+v", list)
			}
		case StateOperatorPods:
			if list.Namespace != "system" || list.Selector != "control-plane=controller-manager" || len(list.Names) != 0 {
				t.Errorf("the Operator's Pods must be found by label: %+v", list)
			}
		case StateEvents:
		default:
			if !strings.Contains(list.Selector, "app.kubernetes.io/instance=ha") {
				t.Errorf("list %+v", list)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("not listed: %v", want)
	}
}
