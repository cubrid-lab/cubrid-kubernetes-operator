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
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogsSchemaVersion is the version of a bundle's index.json.
const LogsSchemaVersion = "1"

// The checkpoints of a scenario. Each is one directory below its logs/.
const (
	// LogsBeforeFault is taken just before a fault that replaces a Pod,
	// because a deleted Pod takes its container log with it.
	LogsBeforeFault = "before-fault"
	// LogsAfterVerdict is taken once the scenario's result is reached and
	// before it is recorded or its namespace is removed.
	LogsAfterVerdict = "after-verdict"
)

// The components of a bundle, under the names their log lines carry
// (docs/observability.md) where they have one.
const (
	LogOperator        = "operator"
	LogInstanceManager = "instance-manager"
	LogBrokerProcess   = "broker-entrypoint"
	LogEngine          = "cubrid-engine"
	LogBroker          = "cubrid-broker"
	LogHeartbeat       = "cubrid-heartbeat"
)

// LogStatus says what became of one source of a bundle.
type LogStatus string

const (
	// LogCollected: all of the source is in its file.
	LogCollected LogStatus = "collected"
	// LogTruncated: the file holds a part of the source, and Reason says which.
	LogTruncated LogStatus = "truncated"
	// LogEmpty: the source answered and held nothing. A source that could not
	// be read is never reported as empty.
	LogEmpty LogStatus = "empty"
	// LogUnavailable: nothing of the source was kept, and Reason says why.
	LogUnavailable LogStatus = "unavailable"
	// LogDeferred: the source is left to a later checkpoint on purpose.
	LogDeferred LogStatus = "deferred"
)

// heartbeatDeferred says why cubrid heartbeat status waits for the end of the
// fault.
const heartbeatDeferred = "cubrid heartbeat status is not a passive read while a fault may be active: it pings " +
	"the hosts of ha_ping_hosts under the heartbeat lock and can turn the isolation check off (POC-52), so it " +
	"runs only once the fault has ended and the verdict is reached"

// podGone says what became of a Pod an earlier checkpoint read.
const podGone = "the Pod was replaced or removed since the last checkpoint, and what it logged after that " +
	"checkpoint went with it"

const (
	logIndexName   = "index.json"
	logOrderedName = "ordered.log"
	// logReasonLimit bounds a reason in the index.
	logReasonLimit = 400
	// logListingLimit bounds the listing of a Pod's CUBRID files.
	logListingLimit = 1 << 16
	// logReadGrace is how long a read that was told to stop may still return.
	// One that does not is left behind: its output is no longer looked at.
	logReadGrace = time.Second
	// logFilesRoot names where CUBRID's own files are, for the index.
	logFilesRoot = "$CUBRID/log/"
)

// LogLimits bound what a scenario collects. There is no periodic collection:
// a scenario has at most Checkpoints checkpoints, each of which stops at
// CheckpointTimeout.
type LogLimits struct {
	// SourceBytes bounds what is stored of one source.
	SourceBytes int64 `json:"sourceBytes"`
	// BundleBytes bounds what a scenario stores, ordered.log included.
	BundleBytes int64 `json:"bundleBytes"`
	// OrderedBytes bounds ordered.log.
	OrderedBytes int64 `json:"orderedBytes"`
	// TailLines bounds what is asked of a container log.
	TailLines int64 `json:"tailLines"`
	// EngineFiles bounds the CUBRID files read in one Pod, newest first.
	EngineFiles int `json:"engineFiles"`
	// Checkpoints bounds the checkpoints of a scenario.
	Checkpoints int `json:"checkpoints"`
	// SourceTimeout bounds the read of one source.
	SourceTimeout time.Duration `json:"sourceTimeoutNanoseconds"`
	// CheckpointTimeout bounds one checkpoint.
	CheckpointTimeout time.Duration `json:"checkpointTimeoutNanoseconds"`
}

// DefaultLogLimits are the limits of a run. They are starting values, and
// docs/testing/scenario-contract.md records what a run has measured of them.
func DefaultLogLimits() LogLimits {
	return LogLimits{SourceBytes: 1 << 20, BundleBytes: 16 << 20, OrderedBytes: 2 << 20, TailLines: 5000,
		EngineFiles: 8, Checkpoints: 2, SourceTimeout: 15 * time.Second, CheckpointTimeout: 90 * time.Second}
}

// LogBudget is what the scenarios of one run may spend together, so that many
// failures grow neither the run's artifact nor its duration without bound. It
// is safe for concurrent use.
type LogBudget struct {
	mu    sync.Mutex
	bytes int64
	wall  time.Duration
}

// NewLogBudget returns a budget of the given stored bytes and collection time.
func NewLogBudget(stored int64, wall time.Duration) *LogBudget {
	return &LogBudget{bytes: stored, wall: wall}
}

func (b *LogBudget) left() (int64, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytes, b.wall
}

func (b *LogBudget) spendBytes(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bytes -= n
}

func (b *LogBudget) spendTime(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wall -= d
}

// LogPodSet is one group of Pods a bundle reads.
type LogPodSet struct {
	// Component names the Pods' container log in the bundle.
	Component string
	Namespace string
	// Selector is a label selector, in kubectl's form.
	Selector  string
	Container string
	// Names are the Pods that must exist; one that does not is named as
	// missing. Without Names, a selector that matches nothing is reported.
	Names []string
	// FilesComponent, when set, reads CUBRID's files in the Pods under this
	// name.
	FilesComponent string
	// Heartbeat also reads cubrid heartbeat status, once the fault has ended.
	Heartbeat bool
}

// LogPlan is what a bundle reads, in order.
type LogPlan struct {
	Sets []LogPodSet
}

// ClusterLogPlan is the plan of one cluster: the Operator's Pods, the
// database members, and the Brokers. ha adds each member's heartbeat status.
func ClusterLogPlan(operatorNamespace, namespace, cluster string, members []string, ha bool) LogPlan {
	return LogPlan{Sets: []LogPodSet{
		{Component: LogOperator, Namespace: operatorNamespace, Selector: "control-plane=controller-manager",
			Container: "manager"},
		{Component: LogInstanceManager, Namespace: namespace, Container: "cubrid", Names: members,
			Selector:       "app.kubernetes.io/instance=" + cluster + ",app.kubernetes.io/component=database",
			FilesComponent: LogEngine, Heartbeat: ha},
		{Component: LogBrokerProcess, Namespace: namespace, Container: "broker",
			Selector:       "database.cubrid.io/broker-access-mode,app.kubernetes.io/instance=" + cluster,
			FilesComponent: LogBroker},
	}}
}

// LogPod is one Pod a reader found.
type LogPod struct {
	Name string
	UID  string
	// Node is where the Pod runs: the clock of its log's timestamps.
	Node string
	// Restarts is the restart count of the set's container.
	Restarts int32
}

// LogReader reads a cluster for a bundle. A method writes what it reads to w
// as it reads it, and stops when ctx ends. Exec runs only the commands of
// this file: ListEngineLogs, TailEngineLog and HeartbeatStatus.
type LogReader interface {
	Pods(ctx context.Context, set LogPodSet) ([]LogPod, error)
	PodLog(ctx context.Context, set LogPodSet, pod LogPod, previous bool, tailLines int64, w io.Writer) error
	Exec(ctx context.Context, set LogPodSet, pod LogPod, command []string, w io.Writer) error
}

// The only commands a bundle runs in a Pod. Each reads and changes nothing in
// CUBRID, and the file a tail reads has passed AllowedEngineLog.
var (
	// ListEngineLogs lists the files of $CUBRID/log that AllowedEngineLog may
	// allow, newest first. A pattern that matches nothing is not an error.
	ListEngineLogs = []string{"sh", "-c", `cd "$CUBRID/log" || exit 3; ls -1t -- server/*.err *_master.err ` +
		`*_copylogdb.err *applylogdb*.err broker/cubrid_broker.log broker/error_log/*.err 2>/dev/null; exit 0`}
	// HeartbeatStatus prints what CUBRID's heartbeat reports. It runs only
	// after the fault has ended: see heartbeatDeferred.
	HeartbeatStatus = []string{"bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`}
)

// TailEngineLog prints the last bytes of a file of $CUBRID/log.
func TailEngineLog(rel string, limit int64) []string {
	return []string{"sh", "-c", `cd "$CUBRID/log" && exec tail -c "$1" -- "$2"`, "tail-engine-log",
		strconv.FormatInt(limit, 10), rel}
}

// engineLogs are the files of $CUBRID/log a bundle may copy, relative to it:
// the error logs of the server, cub_master, the copiers and appliers, and the
// CAS and admin logs of a Broker (docs/poc/RESULTS.md, the table of log files).
// Everything else stays in the Pod: an SQL log holds statements and their
// bound values, and only these files are known not to be configuration.
var engineLogs = []*regexp.Regexp{
	regexp.MustCompile(`^server/[\w.-]+_\d{8}_\d{4}\.err$`),
	regexp.MustCompile(`^[\w.-]+_master\.err$`),
	regexp.MustCompile(`^[\w.-]+@[\w.-]+_copylogdb\.err$`),
	regexp.MustCompile(`^[\w.-]+@localhost_applylogdb_[\w.-]+\.err$`),
	regexp.MustCompile(`^broker/cubrid_broker\.log$`),
	regexp.MustCompile(`^broker/error_log/[\w.-]+\.err$`),
}

// AllowedEngineLog reports whether a bundle may copy the file rel of
// $CUBRID/log.
func AllowedEngineLog(rel string) bool {
	if strings.Contains(rel, "..") {
		return false
	}
	return slices.ContainsFunc(engineLogs, func(p *regexp.Regexp) bool { return p.MatchString(rel) })
}

// LogTail keeps the last bytes written to it and counts all of them, so that
// a log of any size costs a fixed amount of memory. Nothing it holds reaches a
// file before redaction. It is safe for concurrent use.
type LogTail struct {
	mu     sync.Mutex
	limit  int
	data   []byte
	total  int64
	sealed bool
}

// NewLogTail returns a tail that keeps limit bytes.
func NewLogTail(limit int) *LogTail { return &LogTail{limit: limit} }

// Write keeps the end of p. It never fails, so a writer behind it runs to its
// end or its deadline. Once the tail is sealed it only discards.
func (t *LogTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return len(p), nil
	}
	t.total += int64(len(p))
	if len(p) >= t.limit {
		t.data = append(t.data[:0], p[len(p)-t.limit:]...)
		return len(p), nil
	}
	t.data = append(t.data, p...)
	if len(t.data) >= 2*t.limit {
		t.data = t.data[:copy(t.data, t.data[len(t.data)-t.limit:])]
	}
	return len(p), nil
}

// Bytes returns the last bytes written, at most the limit.
func (t *LogTail) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.data[max(0, len(t.data)-t.limit):])
}

// Total counts every byte written.
func (t *LogTail) Total() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total
}

// seal ends the tail: a read left behind can no longer change what it holds.
func (t *LogTail) seal() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sealed = true
}

// LogSource is one entry of a bundle's index: one source, read or not.
type LogSource struct {
	// Order is the source's place in the bundle, across checkpoints.
	Order     int    `json:"order"`
	Component string `json:"component"`
	Pod       string `json:"pod,omitempty"`
	UID       string `json:"uid,omitempty"`
	// Node is the node the Pod ran on, whose clock timestamped its log.
	Node      string `json:"node,omitempty"`
	Container string `json:"container,omitempty"`
	// Origin says where the content came from.
	Origin string `json:"origin"`
	// File is relative to the run directory; empty when nothing was stored.
	File   string    `json:"file,omitempty"`
	Status LogStatus `json:"status"`
	Reason string    `json:"reason,omitempty"`
	// ReadBytes is what the source returned, StoredBytes what the file holds.
	ReadBytes   int64 `json:"readBytes"`
	StoredBytes int64 `json:"storedBytes"`
	// Ordered says the file's lines are also in ordered.log.
	Ordered bool `json:"ordered"`
	// UntimedLines counts lines of an ordered source without a timestamp.
	UntimedLines int       `json:"untimedLines,omitempty"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
}

// LogCheckpoint is one collection of a scenario's logs.
type LogCheckpoint struct {
	Name string `json:"name"`
	// FaultEnded is the caller's word that no fault is active and the
	// verdict is reached; only then does cubrid heartbeat status run.
	FaultEnded bool        `json:"faultEnded"`
	StartedAt  time.Time   `json:"startedAt"`
	FinishedAt time.Time   `json:"finishedAt"`
	Sources    []LogSource `json:"sources"`
}

// LogIndex is the content of a bundle's index.json.
type LogIndex struct {
	SchemaVersion string    `json:"schemaVersion"`
	Scenario      string    `json:"scenario"`
	Limits        LogLimits `json:"limits"`
	// Ordered is the merged file, relative to the run directory.
	Ordered     string          `json:"ordered,omitempty"`
	Checkpoints []LogCheckpoint `json:"checkpoints"`
	// RefusedCheckpoints counts checkpoints asked for past the limit.
	RefusedCheckpoints int `json:"refusedCheckpoints"`
}

// orderedLine is one line of ordered.log.
type orderedLine struct {
	at    time.Time
	stamp string
	label string
	text  string
}

// LogBundle collects one scenario's diagnostic logs into <scenario>/logs/ of
// the run directory. Content stays in memory until it has been cut to whole
// lines and redacted, and reaches the directory only through
// Redactor.WriteFile. The index is rewritten after every source, so a run
// killed at any moment leaves whole files. It is for one goroutine.
//
// Nothing here can fail the caller: Checkpoint returns nothing and does not
// panic, and a problem is only reported by Problem. Evidence of the run's
// verdict stays what the scenario recorded.
type LogBundle struct {
	dir      string
	scenario string
	redactor *Redactor
	reader   LogReader
	plan     LogPlan
	limits   LogLimits
	budget   *LogBudget

	index   LogIndex
	stored  int64
	order   int
	uids    map[string]string
	lines   []orderedLine
	seen    map[string]bool
	written bool
	problem error
}

// NewLogBundle returns the bundle of scenario ("S03/abrupt") below the run
// directory dir.
func NewLogBundle(dir, scenario string, r *Redactor, reader LogReader, plan LogPlan, limits LogLimits,
	budget *LogBudget) *LogBundle {
	return &LogBundle{dir: dir, scenario: scenario, redactor: r, reader: reader, plan: plan, limits: limits,
		budget: budget, uids: map[string]string{}, seen: map[string]bool{},
		index: LogIndex{SchemaVersion: LogsSchemaVersion, Scenario: scenario, Limits: limits,
			Checkpoints: []LogCheckpoint{}}}
}

// IndexFile is the index's path relative to the run directory, or "" until
// one was written.
func (b *LogBundle) IndexFile() string {
	if !b.written {
		return ""
	}
	return b.rel(logIndexName)
}

// Problem is the last reason a file of the bundle could not be written.
func (b *LogBundle) Problem() error { return b.problem }

func (b *LogBundle) rel(name string) string { return b.scenario + "/logs/" + name }

// Checkpoint collects every source of the plan once. faultEnded is the
// caller's word that no fault is active and the verdict is reached. It takes
// at most the limits' CheckpointTimeout, plus logReadGrace for a read that
// does not stop, and what the run's budget still allows; what it did not read
// is named in the index. It runs in a deferred call of a scenario, also while
// a failed assertion panics: the recover below ends only a panic of its own.
func (b *LogBundle) Checkpoint(ctx context.Context, name string, faultEnded bool) {
	defer func() {
		if p := recover(); p != nil {
			b.problem = fmt.Errorf("the log collection panicked: %v", p)
			b.writeIndex()
		}
	}()
	if len(b.index.Checkpoints) >= b.limits.Checkpoints {
		b.index.RefusedCheckpoints++
		b.writeIndex()
		return
	}
	start := time.Now().UTC()
	b.index.Checkpoints = append(b.index.Checkpoints, LogCheckpoint{Name: name, FaultEnded: faultEnded,
		StartedAt: start, Sources: []LogSource{}})
	_, wall := b.budget.left()
	ctx, cancel := context.WithTimeout(ctx, max(0, min(b.limits.CheckpointTimeout, wall)))
	defer cancel()
	for _, set := range b.plan.Sets {
		b.collectSet(ctx, set, faultEnded)
	}
	b.writeOrdered()
	took := time.Since(start)
	b.budget.spendTime(took)
	b.checkpoint().FinishedAt = start.Add(took)
	b.writeIndex()
}

// LogContext returns the context of a collection that concludes a scenario,
// and the function that releases it. A deadline cancels the scenario's context
// before its deferred collection runs, and a read on that context fails at
// once, so the logs of the failure the deadline stopped would be lost while
// the cluster can still give them. This context goes on after ctx ends, also
// when ctx has already ended, for grace and no longer. Checkpoint's own bounds
// still apply, so an expired run is never held longer than grace plus
// logReadGrace for a read that does not stop.
func LogContext(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	logs, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(grace, cancel) })
	return logs, func() {
		stop()
		cancel()
	}
}

func (b *LogBundle) checkpoint() *LogCheckpoint {
	return &b.index.Checkpoints[len(b.index.Checkpoints)-1]
}

// collectSet reads every Pod of a set, and names the Pods it expected and
// did not find.
func (b *LogBundle) collectSet(ctx context.Context, set LogPodSet, faultEnded bool) {
	var pods []LogPod
	err := b.stopped(ctx)
	if err == nil {
		err = b.within(ctx, func(ctx context.Context) error {
			found, err := b.reader.Pods(ctx, set)
			if err != nil {
				return err
			}
			pods = found
			return nil
		})
	}
	if err != nil {
		reason := "the Pods could not be listed: " + b.reason(err)
		for _, name := range set.Names {
			b.note(LogSource{Component: set.Component, Pod: name, Container: set.Container, Origin: set.Selector,
				Status: LogUnavailable, Reason: reason})
		}
		if len(set.Names) == 0 {
			b.note(LogSource{Component: set.Component, Container: set.Container, Origin: set.Selector,
				Status: LogUnavailable, Reason: reason})
		}
		return
	}
	pods = slices.Clone(pods)
	for _, name := range set.Names {
		if !slices.ContainsFunc(pods, func(p LogPod) bool { return p.Name == name }) {
			b.note(LogSource{Component: set.Component, Pod: name, Container: set.Container, Origin: set.Selector,
				UID: b.uids[set.Component+"/"+name], Status: LogUnavailable, Reason: "the Pod was not found"})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(b.uids)) {
		component, name, _ := strings.Cut(key, "/")
		if component == set.Component && !slices.Contains(set.Names, name) &&
			!slices.ContainsFunc(pods, func(p LogPod) bool { return p.Name == name }) {
			b.note(LogSource{Component: set.Component, Pod: name, Container: set.Container, Origin: set.Selector,
				UID: b.uids[key], Status: LogUnavailable, Reason: podGone})
			delete(b.uids, key)
		}
	}
	if len(pods) == 0 && len(set.Names) == 0 {
		b.note(LogSource{Component: set.Component, Container: set.Container, Origin: set.Selector,
			Status: LogUnavailable, Reason: "no Pod matched the selector"})
	}
	for _, pod := range pods {
		b.collectPod(ctx, set, pod, faultEnded)
	}
}

// collectPod reads the sources of one Pod.
func (b *LogBundle) collectPod(ctx context.Context, set LogPodSet, pod LogPod, faultEnded bool) {
	base := LogSource{Component: set.Component, Pod: pod.Name, UID: pod.UID, Node: pod.Node, Container: set.Container}
	if old, seen := b.uids[set.Component+"/"+pod.Name]; seen && old != pod.UID {
		gone := base
		gone.UID, gone.Origin, gone.Status, gone.Reason = old, "container log", LogUnavailable, podGone
		b.note(gone)
	}
	b.uids[set.Component+"/"+pod.Name] = pod.UID

	current := base
	current.Origin = "container log"
	b.read(ctx, current, set.Component+"-"+pod.Name+".log", b.limits.TailLines,
		func(ctx context.Context, w io.Writer) error {
			return b.reader.PodLog(ctx, set, pod, false, b.limits.TailLines, w)
		})
	if pod.Restarts > 0 {
		previous := base
		previous.Origin = "container log, previous instance"
		b.read(ctx, previous, set.Component+"-"+pod.Name+"-previous.log", b.limits.TailLines,
			func(ctx context.Context, w io.Writer) error {
				return b.reader.PodLog(ctx, set, pod, true, b.limits.TailLines, w)
			})
	}
	if set.FilesComponent != "" {
		b.collectFiles(ctx, set, pod, base)
	}
	if !set.Heartbeat {
		return
	}
	beat := base
	beat.Component, beat.Origin = LogHeartbeat, "cubrid heartbeat status"
	if !faultEnded {
		beat.Status, beat.Reason = LogDeferred, heartbeatDeferred
		b.note(beat)
		return
	}
	b.read(ctx, beat, LogHeartbeat+"-"+pod.Name+".txt", 0, func(ctx context.Context, w io.Writer) error {
		return b.reader.Exec(ctx, set, pod, HeartbeatStatus, w)
	})
}

// collectFiles lists CUBRID's files in a Pod and reads the newest it may copy.
func (b *LogBundle) collectFiles(ctx context.Context, set LogPodSet, pod LogPod, base LogSource) {
	base.Component, base.Origin = set.FilesComponent, logFilesRoot
	listed := NewLogTail(logListingLimit)
	err := b.stopped(ctx)
	if err == nil {
		err = b.within(ctx, func(ctx context.Context) error {
			return b.reader.Exec(ctx, set, pod, ListEngineLogs, listed)
		})
	}
	listed.seal()
	if err != nil {
		base.Status, base.Reason = LogUnavailable, "the files could not be listed: "+b.reason(err)
		b.note(base)
		return
	}
	var allowed []string
	refused := 0
	for line := range strings.Lines(string(listed.Bytes())) {
		switch rel := strings.TrimSpace(line); {
		case rel == "", strings.HasSuffix(rel, "_latest.err"):
			// CUBRID's link to the newest server log, which is listed itself.
		case AllowedEngineLog(rel):
			allowed = append(allowed, rel)
		default:
			refused++
		}
	}
	if refused > 0 {
		defer func() {
			note := base
			note.Status = LogUnavailable
			note.Reason = fmt.Sprintf("%d listed file(s) are not on the list of files a bundle may copy", refused)
			b.note(note)
		}()
	}
	if len(allowed) == 0 {
		base.Status, base.Reason = LogEmpty, "the Pod answered and holds none of the files a bundle may copy"
		b.note(base)
		return
	}
	if extra := len(allowed) - b.limits.EngineFiles; extra > 0 {
		note := base
		note.Status = LogTruncated
		note.Reason = fmt.Sprintf("%d older file(s) were not read: at most %d are read in one Pod", extra,
			b.limits.EngineFiles)
		b.note(note)
		allowed = allowed[:b.limits.EngineFiles]
	}
	for _, rel := range allowed {
		source := base
		source.Origin = logFilesRoot + rel
		b.read(ctx, source, base.Component+"-"+pod.Name+"-"+rel, 0, func(ctx context.Context, w io.Writer) error {
			return b.reader.Exec(ctx, set, pod, TailEngineLog(rel, b.limits.SourceBytes+1), w)
		})
	}
}

// within runs read for at most the limits' SourceTimeout, and stops waiting
// for it logReadGrace after that, or after ctx ended: a reader that ignores
// its context cannot hold the scenario. A panic of read is returned as an
// error.
func (b *LogBundle) within(ctx context.Context, read func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, b.limits.SourceTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("the read panicked: %v", p)
			}
		}()
		done <- read(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	select {
	case err := <-done:
		return err
	case <-time.After(logReadGrace):
		return fmt.Errorf("the read did not stop: %w", ctx.Err())
	}
}

// stopped says why no further source may be read, or nil.
func (b *LogBundle) stopped(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("the collection stopped before this source: %w", err)
	}
	return nil
}

// room is how many bytes the next source may store.
func (b *LogBundle) room() int64 {
	run, _ := b.budget.left()
	return min(b.limits.SourceBytes, b.limits.BundleBytes-b.limits.OrderedBytes-b.stored, run)
}

// read reads one source into memory, cuts it to whole lines, redacts it and
// stores it. The source's lines also go to ordered.log when tailLines is set:
// a container log, whose lines carry the kubelet's timestamp.
func (b *LogBundle) read(ctx context.Context, source LogSource, file string, tailLines int64,
	read func(context.Context, io.Writer) error) {
	source.StartedAt = time.Now().UTC()
	limit := b.room()
	if err := b.stopped(ctx); err != nil {
		source.Status, source.Reason = LogUnavailable, b.reason(err)
		b.note(source)
		return
	}
	if limit <= 0 {
		source.Status = LogUnavailable
		source.Reason = "not read: the size bound of the scenario or of the run is used up"
		b.note(source)
		return
	}
	tail := NewLogTail(int(limit))
	err := b.within(ctx, func(ctx context.Context) error { return read(ctx, tail) })
	tail.seal()
	source.ReadBytes = tail.Total()

	raw := tail.Bytes()
	cut := tail.Total() > limit
	// A window can begin inside a line, and a read that ended early can stop
	// inside one. A part of a line may hold a part of a credential that no
	// pattern can recognize, so only whole lines go on.
	kept := wholeLines(raw, cut, err != nil)
	clean := b.redactor.Redact(kept)
	if int64(len(clean)) > limit {
		// The placeholder can be longer than what it replaced.
		clean = wholeLines(clean[int64(len(clean))-limit:], true, false)
		cut = true
	}
	switch {
	case err != nil && len(clean) == 0:
		source.Status, source.Reason = LogUnavailable, b.reason(err)
	case err != nil:
		source.Status, source.Reason = LogTruncated, "the read ended early: "+b.reason(err)
	case tail.Total() == 0:
		source.Status, source.Reason = LogEmpty, "the source answered and held nothing"
	case len(clean) == 0:
		source.Status = LogUnavailable
		source.Reason = fmt.Sprintf("the source returned %d bytes, none of them a whole line within the "+
			"size bound of %d bytes", tail.Total(), limit)
	case cut:
		source.Status = LogTruncated
		source.Reason = fmt.Sprintf("only the end of the source was kept: %d bytes of %d", len(clean), tail.Total())
	case tailLines > 0 && int64(bytes.Count(clean, []byte{'\n'})) >= tailLines:
		source.Status = LogTruncated
		source.Reason = fmt.Sprintf("the log was read to the line limit of %d: older lines may exist", tailLines)
	default:
		source.Status = LogCollected
	}
	if len(clean) > 0 {
		rel := b.rel(b.checkpoint().Name + "/" + fileName(file))
		if werr := b.redactor.WriteFile(b.dir, rel, clean); werr != nil {
			b.problem = werr
			source.Status, source.Reason = LogUnavailable, "the file could not be written: "+b.reason(werr)
		} else {
			source.File, source.StoredBytes = rel, int64(len(clean))
			b.stored += source.StoredBytes
			b.budget.spendBytes(source.StoredBytes)
			if tailLines > 0 {
				source.Ordered = true
				source.UntimedLines = b.addLines(source, clean)
			}
		}
	}
	b.note(source)
}

// wholeLines cuts text to whole lines: without what comes before the first
// line break when front is set, and after the last when back is.
func wholeLines(text []byte, front, back bool) []byte {
	if front {
		i := bytes.IndexByte(text, '\n')
		if i < 0 {
			return nil
		}
		text = text[i+1:]
	}
	if back {
		i := bytes.LastIndexByte(text, '\n')
		if i < 0 {
			return nil
		}
		text = text[:i+1]
	}
	return text
}

var unsafeFileName = regexp.MustCompile(`[^\w.@-]`)

// fileName is name with only characters safe in a file name.
func fileName(name string) string { return unsafeFileName.ReplaceAllString(name, "_") }

// note adds a source to the index and stores the index.
func (b *LogBundle) note(source LogSource) {
	b.order++
	source.Order = b.order
	if source.StartedAt.IsZero() {
		source.StartedAt = time.Now().UTC()
	}
	source.FinishedAt = time.Now().UTC()
	cp := b.checkpoint()
	cp.Sources = append(cp.Sources, source)
	b.writeIndex()
}

// reason is err as the text of an index entry: redacted, on one line, short.
func (b *LogBundle) reason(err error) string {
	text := string(b.redactor.Redact([]byte(err.Error())))
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > logReasonLimit {
		text = strings.ToValidUTF8(text[:logReasonLimit], "") + "..."
	}
	return text
}

func (b *LogBundle) writeIndex() {
	data, err := json.MarshalIndent(b.index, "", "  ")
	if err == nil {
		err = b.redactor.WriteFile(b.dir, b.rel(logIndexName), append(data, '\n'))
	}
	if err != nil {
		b.problem = err
		return
	}
	b.written = true
}

// addLines keeps the timestamped lines of a stored source for ordered.log, a
// line seen at an earlier checkpoint only once, and returns how many lines had
// no timestamp.
func (b *LogBundle) addLines(source LogSource, text []byte) int {
	label := source.Component + "/" + source.Pod
	if strings.Contains(source.Origin, "previous") {
		label += "/previous"
	}
	untimed := 0
	for line := range strings.Lines(string(text)) {
		stamp, rest, _ := strings.Cut(strings.TrimRight(line, "\n"), " ")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			untimed++
			continue
		}
		key := label + " " + line
		if !b.seen[key] {
			b.seen[key] = true
			b.lines = append(b.lines, orderedLine{at: at, stamp: stamp, label: label, text: rest})
		}
	}
	return untimed
}

// writeOrdered stores the timestamped lines of the bundle, oldest first, as
// many of the newest as the bound allows.
func (b *LogBundle) writeOrdered() {
	if len(b.lines) == 0 {
		return
	}
	lines := slices.Clone(b.lines)
	slices.SortStableFunc(lines, func(x, y orderedLine) int { return cmp.Compare(x.at.UnixNano(), y.at.UnixNano()) })
	const header = "# Container log lines of this scenario, ordered by the timestamp the kubelet of each Pod's " +
		"node gave them.\n# The clocks of different nodes were not compared, so the order of lines from different " +
		"nodes is not proven.\n"
	const droppedFormat = "# %d older lines did not fit; they are in the files listed in index.json.\n"
	room := b.limits.OrderedBytes - int64(len(header)) - int64(len(droppedFormat)) - 20
	first := len(lines)
	for first > 0 {
		size := int64(len(lines[first-1].stamp) + len(lines[first-1].label) + len(lines[first-1].text) + 3)
		if room < size {
			break
		}
		room -= size
		first--
	}
	var out bytes.Buffer
	out.WriteString(header)
	if first > 0 {
		fmt.Fprintf(&out, droppedFormat, first)
	}
	for _, line := range lines[first:] {
		fmt.Fprintf(&out, "%s %s %s\n", line.stamp, line.label, line.text)
	}
	rel := b.rel(logOrderedName)
	if err := b.redactor.WriteFile(b.dir, rel, out.Bytes()); err != nil {
		b.problem = errors.Join(b.problem, err)
		return
	}
	b.index.Ordered = rel
}
