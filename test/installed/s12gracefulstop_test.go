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

package installed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testGSRUID            = "gracefulstop-cluster"
	testGSRTarget         = "ha-gracefulstop-0"
	testGSRSecretUID      = "gracefulstop-secret"
	testGSRReplacementUID = "ha-gracefulstop-0-again"
	testGSROp             = "op-restore-0"
	testGSROp2            = "op-restore-1"
	// testGSRKilled is the end of restoredb's killed process group.
	testGSRKilled = "signal: killed: "
	// testGSRWrapper and testGSRWorker are the pids of `cubrid restoredb` and
	// of the cub_admin it forks (POC-27).
	testGSRWrapper = "4300"
	testGSRWorker  = "4301"
)

// The restore's paths in the fake target, and the Operator's message of the
// attempt the stop ended.
var (
	testGSRStaging      = imRestoreStagingRoot + "/" + testGSRUID
	testGSRDatabase     = imDatabasesDir + "/" + databaseName
	testGSRDatabaseDir  = "databases/" + databaseName
	testGSRStagingDir   = "restore-staging/" + testGSRUID
	testGSRFailReason   = restoreStopReasonPrefix + testGSRKilled
	testGSRRetryMessage = "on " + testGSRTarget + ": " + testGSRFailReason + "; starting attempt 2 of 3"
	testGSRRestoredb    = "cubrid restoredb -u -B " + testGSRStaging + "/backup " + databaseName
)

func testS12GracefulStopConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS12Config(t)
	cfg.RestoreGracefulStop = true
	return cfg
}

// gsrSample is one reading the fake sampler prints after the deletion: at,
// after the deletion, by the container's clock, its processes, holders and
// entries, as the script prints them after their letter.
type gsrSample struct {
	at                          time.Duration
	processes, holders, entries []string
}

// gsrLine is one line of the target's log after the deletion, at after it.
type gsrLine struct {
	at   time.Duration
	line func(at time.Time) string
}

// s12GracefulStopFixture plays, after S12 exact, an Operator that restores
// S11's artifact into ha-gracefulstop and reports beforeDelete until the
// target's Pod is deleted and then afterDelete, one per reading, the last
// kept; the sampler's exec session and the followed log of the target, which
// show restoredb running until the deletion and then afterStop and stopLog
// and end; the API server and the StatefulSet, which replace the deleted
// Pod; and CUBRID and the client on ha-gracefulstop's members.
type s12GracefulStopFixture struct {
	*s12Fixture
	beforeDelete []imStatus
	afterDelete  []imStatus
	// neverSeen keeps restoredb out of every reading; afterStop and stopLog
	// are the readings and the log lines after the deletion.
	neverSeen bool
	afterStop []gsrSample
	stopLog   []gsrLine
	// deleteErr refuses the deletion of the target's Pod.
	deleteErr error

	gsCluster   *databasev1alpha1.CubridCluster
	reads       int
	afterReads  int
	deleted     bool
	deletedAt   time.Time
	stopped     chan struct{}
	podDeletes  []string
	deletions   []string
	gsWrites    []string
	samplerRuns int
	logFollows  int
}

func newS12GracefulStopFixture(t *testing.T, cfg HAConfig) *s12GracefulStopFixture {
	t.Helper()
	f := &s12GracefulStopFixture{s12Fixture: newS12Fixture(t, cfg), stopped: make(chan struct{}),
		beforeDelete: []imStatus{
			{databasev1alpha1.BootstrapPreparing, 0, testGSROp, testRestoreInProgress,
				"the backup is being verified and downloaded on " + testGSRTarget},
			{databasev1alpha1.BootstrapRestoring, 0, testGSROp, testRestoreInProgress,
				"restoredb is running on " + testGSRTarget}},
		afterDelete: []imStatus{
			{databasev1alpha1.BootstrapRestoring, 0, testGSROp, testIMUnavailable, testIMRefused},
			{databasev1alpha1.BootstrapRestoring, 1, "", s12RetryReason, testGSRRetryMessage},
			{databasev1alpha1.BootstrapRestoring, 1, testGSROp2, testRestoreInProgress,
				"restoredb is running on " + testGSRTarget},
			{databasev1alpha1.BootstrapSeedingReplicas, 1, testGSROp2, testSeedInProgress, testIMCopying},
			{databasev1alpha1.BootstrapComplete, 1, testGSROp2, testPeersSeeded, testIMCopied}}}
	server := "77 cub_server cub_server " + databaseName
	f.afterStop = []gsrSample{
		{at: 0, entries: []string{testGSRDatabaseDir, testGSRStagingDir}},
		// The Instance Manager, pid 1 here and not a cub* process, removes
		// the staging while the sampler lists its files.
		{at: 100 * time.Millisecond, holders: []string{"1 " + testGSRStaging + "/backup (deleted)"},
			entries: []string{testGSRStagingDir}},
		{at: 200 * time.Millisecond},
		{at: 300 * time.Millisecond, processes: []string{server}},
		{at: 400 * time.Millisecond}}
	f.stopLog = []gsrLine{
		{150 * time.Millisecond, func(at time.Time) string {
			return gsrLogLine(at, logCommandFailed, logEventCommand, testGSRRestoredb, "", "", "",
				`"durationMs":2412,"exitCode":-1`)
		}},
		{250 * time.Millisecond, func(at time.Time) string {
			return gsrLogLine(at, logOperationFailed, "operation_state_changed", "", testGSROp, "restore",
				testGSRFailReason, "")
		}},
		{450 * time.Millisecond, func(at time.Time) string {
			return gsrLogLine(at, logCommandFailed, logEventCommand, heartbeatStop, "", "", "", `"exitCode":1`)
		}},
		{500 * time.Millisecond, func(at time.Time) string {
			return gsrLogLine(at, logCommandFailed, logEventCommand, serverStop+" "+databaseName, "", "", "",
				`"exitCode":1`)
		}}}
	for _, member := range clusterMembers(s12GracefulStopClusterName) {
		f.roles[member] = roleSlave
	}
	f.roles[testGSRTarget] = roleMaster
	f.gracefulStopCreated = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
		cluster.UID, cluster.Generation = testGSRUID, 1
		f.mu.Lock()
		f.gsCluster = cluster.DeepCopy()
		f.mu.Unlock()
		if err := c.Create(ctx, cluster); err != nil {
			return err
		}
		return f.createChildren(ctx, c, cluster)
	}
	f.gracefulStopRead = f.advanceGSR
	f.podDeleted = f.deleteTarget
	f.clusterDeleted = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster,
		opts ...client.DeleteOption) error {
		return f.deleteOwned(ctx, c, cluster, testGSRUID, opts...)
	}
	f.secretDeleted = func(ctx context.Context, c client.WithWatch, secret *corev1.Secret,
		opts ...client.DeleteOption) error {
		return f.deleteOwned(ctx, c, secret, testGSRSecretUID, opts...)
	}
	s12Client, s12Markers := f.clientHook, f.markersOf
	f.clientHook = func(script string) (string, error, bool) {
		if !strings.Contains(script, "workload marker "+s12GracefulStopMarker+" ") {
			return s12Client(script)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gsWrites = append(f.gsWrites, script)
		return "marker " + s12GracefulStopMarker + " written\n", nil, true
	}
	f.markersOf = func(pod string) []string {
		rows := s12Markers(pod)
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasPrefix(pod, s12GracefulStopClusterName+"-") && len(f.gsWrites) > 0 {
			_, rest, _ := strings.Cut(f.gsWrites[0], "workload marker "+s12GracefulStopMarker+" ")
			rows = append(rows, s12GracefulStopMarker+"="+strings.Fields(rest)[0])
		}
		return rows
	}
	f.runner.execStream = func(ctx context.Context, args ...string) (io.ReadCloser, error) {
		want := "-n " + cfg.Namespace + " exec " + testGSRTarget + " -c " + databaseContainer + " -- bash -c " +
			restoreSamplerScript()
		if strings.Join(args, " ") != want || len(args) != 10 {
			t.Errorf("streamed command = %q, want the sampler in %s", args, testGSRTarget)
		}
		f.mu.Lock()
		f.samplerRuns++
		f.mu.Unlock()
		return f.stream(ctx, f.sample), nil
	}
	f.runner.followLogs = func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (
		io.ReadCloser, error) {
		if namespace != cfg.Namespace || pod != testGSRTarget || opts.Container != databaseContainer ||
			!opts.Follow || opts.SinceTime != nil || opts.LimitBytes == nil ||
			*opts.LimitBytes != f.runner.logLimit()+1 {
			t.Errorf("followed log of %s/%s with %+v", namespace, pod, opts)
		}
		f.mu.Lock()
		f.logFollows++
		f.mu.Unlock()
		return f.stream(ctx, f.log), nil
	}
	return f
}

// gsrLogLine is one line of the target Instance Manager's JSON log.
func gsrLogLine(at time.Time, msg, event, command, operation, kind, reason, extra string) string {
	fields := map[string]any{"time": at.UTC().Format(time.RFC3339Nano), "msg": msg,
		"component": "instance-manager", "member": testGSRTarget}
	for k, v := range map[string]string{"event": event, "command": command, "operationID": operation,
		"kind": kind, "reason": reason} {
		if v != "" {
			fields[k] = v
		}
	}
	data, _ := json.Marshal(fields)
	line := string(data)
	if extra != "" {
		line = strings.TrimSuffix(line, "}") + "," + extra + "}"
	}
	return line
}

// stream returns the reading end of a pipe that write fills; it ends when
// write returns, or when ctx ends.
func (f *s12GracefulStopFixture) stream(ctx context.Context, write func(io.Writer)) io.ReadCloser {
	r, w := io.Pipe()
	go func() {
		defer func() { _ = w.Close() }()
		write(w)
	}()
	go func() {
		<-ctx.Done()
		_ = r.CloseWithError(ctx.Err())
	}()
	return r
}

// writeReading writes one reading of the sampler taken at at.
func writeReading(w io.Writer, at time.Time, processes, holders, entries []string) bool {
	var b strings.Builder
	fmt.Fprintf(&b, "T %d.%09d\n", at.Unix(), at.Nanosecond())
	for _, p := range processes {
		b.WriteString("P " + p + "\n")
	}
	for _, e := range entries {
		b.WriteString("E " + e + "\n")
	}
	for _, p := range processes {
		b.WriteString("F " + strings.Fields(p)[0] + "\n")
	}
	for _, h := range holders {
		b.WriteString("H " + h + "\n")
	}
	b.WriteString(".\n")
	_, err := io.WriteString(w, b.String())
	return err == nil
}

// sample writes readings of restoredb running until the deletion, then
// afterStop at their times after it.
func (f *s12GracefulStopFixture) sample(w io.Writer) {
	running := []string{testGSRWrapper + " cubrid cubrid restoredb",
		testGSRWorker + " cub_admin cub_admin restoredb -u -B " + testGSRStaging + "/backup " + databaseName}
	for {
		select {
		case <-f.stopped:
			f.mu.Lock()
			deletedAt := f.deletedAt
			f.mu.Unlock()
			for _, s := range f.afterStop {
				if !writeReading(w, deletedAt.Add(s.at), s.processes, s.holders, s.entries) {
					return
				}
			}
			return
		default:
		}
		f.mu.Lock()
		never := f.neverSeen
		f.mu.Unlock()
		processes, holders := running, []string{testGSRWorker + " " + testGSRDatabase + "/" + databaseName,
			testGSRWorker + " " + testGSRStaging + "/backup/" + databaseName + "_bk0v000"}
		if never {
			processes, holders = nil, nil
		}
		if !writeReading(w, time.Now(), processes, holders, []string{testGSRDatabaseDir, testGSRStagingDir}) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// log writes the target's log: two lines, and stopLog once the Pod was
// deleted.
func (f *s12GracefulStopFixture) log(w io.Writer) {
	now := time.Now()
	for _, line := range []string{gsrLogLine(now, "Instance Manager listening", "listening", "", "", "", "", ""),
		gsrLogLine(now, "Operation changed state", "operation_state_changed", "", testGSROp, "restore", "", "")} {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return
		}
	}
	<-f.stopped
	f.mu.Lock()
	deletedAt := f.deletedAt
	f.mu.Unlock()
	for _, l := range f.stopLog {
		if _, err := io.WriteString(w, l.line(deletedAt.Add(l.at))+"\n"); err != nil {
			return
		}
	}
}

// deleteTarget plays the API server and the StatefulSet when the run deletes
// the restore's target: the deletion must carry the Pod's UID and no grace
// period of its own; the Pod goes and a replacement with another UID runs.
func (f *s12GracefulStopFixture) deleteTarget(ctx context.Context, c client.WithWatch, pod *corev1.Pod,
	opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	uid := ""
	if options.Preconditions != nil && options.Preconditions.UID != nil {
		uid = string(*options.Preconditions.UID)
	}
	if pod.Name != testGSRTarget || uid != testGSRTarget || options.GracePeriodSeconds != nil {
		f.t.Errorf("deleted Pod %s with UID precondition %q and grace period %v, want %s with its UID and its own "+
			"grace period", pod.Name, uid, options.GracePeriodSeconds, testGSRTarget)
	}
	f.mu.Lock()
	refused := f.deleteErr
	f.mu.Unlock()
	if refused != nil {
		return refused
	}
	stored := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), stored); err != nil {
		return err
	}
	if err := c.Delete(ctx, stored); err != nil {
		return err
	}
	replacement := stored.DeepCopy()
	replacement.UID, replacement.ResourceVersion = types.UID(testGSRReplacementUID), ""
	replacement.Status.ContainerStatuses[0].ContainerID = "containerd://" + testGSRReplacementUID
	if err := c.Create(ctx, replacement); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.podDeletes = append(f.podDeletes, pod.Name+"/"+uid)
	if !f.deleted {
		f.deleted, f.deletedAt = true, time.Now()
		close(f.stopped)
	}
	return nil
}

// deleteOwned plays the API server when the run deletes what it created:
// the deletion must carry uid.
func (f *s12GracefulStopFixture) deleteOwned(ctx context.Context, c client.WithWatch, obj client.Object,
	uid string, opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != uid {
		f.t.Errorf("deleted %T %s without its UID %s as a precondition", obj, obj.GetName(), uid)
	}
	f.mu.Lock()
	f.deletions = append(f.deletions, fmt.Sprintf("%T/%s", obj, obj.GetName()))
	f.mu.Unlock()
	return c.Delete(ctx, obj)
}

// advanceGSR writes the status of ha-gracefulstop's next reading, as the
// Operator would: BootstrapReady and Ready are True only when the bootstrap
// is Complete.
func (f *s12GracefulStopFixture) advanceGSR(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, key, cluster); err != nil {
		return nil
	}
	f.mu.Lock()
	var st imStatus
	if f.deleted {
		st = f.afterDelete[min(f.afterReads, len(f.afterDelete)-1)]
		f.afterReads++
	} else {
		st = f.beforeDelete[min(f.reads, len(f.beforeDelete)-1)]
		f.reads++
	}
	f.mu.Unlock()
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	status := metav1.ConditionFalse
	if st.phase == databasev1alpha1.BootstrapComplete {
		status = metav1.ConditionTrue
	}
	s := &cluster.Status
	s.ObservedGeneration = 1
	s.Bootstrap = &databasev1alpha1.BootstrapStatus{Mode: testRecoveryMode, Phase: st.phase, Attempts: st.attempts,
		ManifestURI: cluster.Spec.Bootstrap.Recovery.ManifestURI, TargetMember: testGSRTarget, OperationID: st.op}
	s.Conditions = []metav1.Condition{{Type: bootstrapReadyCondition, Status: status, Reason: st.reason,
		Message: st.message, LastTransitionTime: now}}
	if st.phase == databasev1alpha1.BootstrapComplete {
		s.CurrentPrimary = testGSRTarget
		s.Databases = []databasev1alpha1.DatabaseStatus{{Name: databaseName, Phase: testDatabaseCreated,
			PrimaryCreated: true, HAConfigured: true}}
	}
	for _, condition := range startingConditions {
		s.Conditions = append(s.Conditions, metav1.Condition{Type: condition, Status: status,
			Reason: testConditionReason, LastTransitionTime: now})
	}
	if err := c.Update(ctx, cluster); err != nil {
		f.t.Error(err)
	}
	return nil
}

// gracefulStopRestoreOf runs the fixture and returns S12 graceful-stop,
// requiring LAB01, S11 and S12 exact passed, each scenario's evidence its
// own and no key in the evidence.
func gracefulStopRestoreOf(t *testing.T, f *s12GracefulStopFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	summary := readSummary(t, f.cfg.EvidenceDir)
	if summary.Lane != s12GracefulStopLane || len(summary.Scenarios) != 4 || summary.Scenarios[3].ID != s12ID ||
		summary.Scenarios[3].Variant != s12GracefulStopVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete, S12/exact and S12/graceful-stop in lane %s", summary,
			s12GracefulStopLane)
	}
	for _, s := range summary.Scenarios[:3] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01, S11 and S12 exact passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, summary.Scenarios[2], "S12/exact")
	requireOwnEvidence(t, summary.Scenarios[3], "S12/"+s12GracefulStopVariant)
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	return summary.Scenarios[3], err
}

// readGSRRecord reads the record of S12 graceful-stop.
func readGSRRecord(t *testing.T, cfg HAConfig) s12GracefulStopRecord {
	t.Helper()
	var rec s12GracefulStopRecord
	data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, s12GracefulStopRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.Trigger == nil || rec.Sampler == nil || rec.Log == nil {
		t.Fatalf("S12 graceful-stop record = %s, %v", data, err)
	}
	return rec
}

// requireGSR fails unless S12 graceful-stop ended as result with every part
// of want in its reason.
func requireGSR(t *testing.T, stop evidence.Scenario, err error, result evidence.Result, want ...string) {
	t.Helper()
	if stop.Result != result || (result != evidence.Pass && err == nil) {
		t.Fatalf("S12 graceful-stop = %+v, %v; want %s", stop, err, result)
	}
	for _, w := range want {
		if !strings.Contains(stop.Reason, w) {
			t.Fatalf("S12 graceful-stop reason = %q, want %q", stop.Reason, w)
		}
	}
}

// requireGSRPassRecord requires the reason, the trigger, the sampler's
// findings and the log's lines of a pass in rec.
func requireGSRPassRecord(t *testing.T, rec s12GracefulStopRecord) {
	t.Helper()
	// The summary keeps no reason for a pass; the run's is made from the
	// record.
	reason := s12GracefulStopPassReason(&rec)
	for _, want := range []string{"restoredb's Command failed", testGSROp, testGSRFailReason, testGSROp2,
		"attempt 1, with one retry", "the shutdown's heartbeat stop"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want %q", reason, want)
		}
	}
	s, l, tr := rec.Sampler, rec.Log, rec.Trigger
	if tr.DeleteAcceptedAt == nil || tr.PodUID != testGSRTarget || tr.ReplacementUID != testGSRReplacementUID ||
		!strings.Contains(tr.SeenProcess, "restoredb") {
		t.Fatalf("trigger = %+v", tr)
	}
	if s.CleanupSeenAt == nil || s.StagingGoneAt == nil || len(s.HoldersAfterCleanup) != 0 ||
		len(s.UtilityAfterCleanup) != 0 || len(s.PresentAfterOperationFailed) != 0 ||
		len(s.OtherHoldersAfterCleanup) != 1 || s.LastHolder == "" || s.DatabaseDir != testGSRDatabaseDir ||
		s.StagingDir != testGSRStagingDir {
		t.Fatalf("sampler = %+v", s)
	}
	if l.RestoredbResult != logCommandFailed || l.OperationID != testGSROp ||
		l.OperationFailedReason != testGSRFailReason || l.HeartbeatStopLine <= l.OperationFailedLine ||
		l.StopLine == 0 {
		t.Fatalf("log = %+v", l)
	}
}

func TestRunS12GracefulStopPassesWhenTheStopEndsRestoredbAndTheRetryCompletes(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	stop, err := gracefulStopRestoreOf(t, f)
	if err != nil || stop.Result != evidence.Pass {
		t.Fatalf("S12 graceful-stop = %+v, %v; want passed", stop, err)
	}
	if stop.CleanupSucceeded == nil || !*stop.CleanupSucceeded {
		t.Fatalf("cleanup = %v, want succeeded", stop.CleanupSucceeded)
	}
	for _, file := range []string{s12GracefulStopRecordFile, s12GracefulStopSamplesFile, s12GracefulStopLogFile,
		s12GracefulStopDataCheckFile, s12GracefulStopSourceDataCheckFile} {
		if !slices.Contains(stop.Evidence, file) {
			t.Fatalf("evidence = %v, want %s", stop.Evidence, file)
		}
	}
	rec := readGSRRecord(t, f.cfg)
	requireGSRPassRecord(t, rec)
	if rec.StoppedOperation != testGSROp || rec.OperationAtDelete != testGSROp || rec.OperationIDAfter != testGSROp2 ||
		len(rec.RetriesAfterStop) != 1 || rec.PlacementMissed != "" {
		t.Fatalf("record = %+v", rec)
	}
	if got := stop.Measurements[measRetriesAfterStop]; got != float64(1) {
		t.Fatalf("retriesAfterStop = %v, want 1", got)
	}
	if !strings.Contains(rec.Cleanup, testGSRUID) || !strings.Contains(rec.Cleanup, testGSRSecretUID) ||
		len(f.deletions) != 2 {
		t.Fatalf("cleanup = %q, deletions = %v; want the cluster and the Secret deleted by UID", rec.Cleanup,
			f.deletions)
	}
	if f.samplerRuns != 1 || f.logFollows != 1 || !slices.Equal(f.podDeletes, []string{testGSRTarget + "/" +
		testGSRTarget}) {
		t.Fatalf("sampler runs %d, log follows %d, Pod deletions %v; want one each", f.samplerRuns, f.logFollows,
			f.podDeletes)
	}
}

func TestRunS12GracefulStopIsBlockedWhenRestoredbIsNeverSeen(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	f.neverSeen = true
	f.beforeDelete = append(f.beforeDelete, imStatus{databasev1alpha1.BootstrapSeedingReplicas, 0, testGSROp,
		testSeedInProgress, testIMCopying})
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Blocked, "placement missed: the bootstrap was read SeedingReplicas before "+
		"the sampler showed restoredb")
	if len(f.podDeletes) != 0 {
		t.Fatalf("Pod deletions = %v, want none", f.podDeletes)
	}
}

func TestRunS12GracefulStopIsBlockedWhenRestoredbEndedBeforeTheStop(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	f.stopLog[0] = gsrLine{150 * time.Millisecond, func(at time.Time) string {
		return gsrLogLine(at, logRanCommand, logEventCommand, testGSRRestoredb, "", "", "", `"exitCode":0`)
	}}
	f.stopLog[1] = gsrLine{250 * time.Millisecond, func(at time.Time) string {
		return gsrLogLine(at, logOperationFailed, "operation_state_changed", "", testGSROp, "restore",
			errStoppingText, "")
	}}
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Blocked, "placement missed: the member's log shows restoredb succeed")
}

func TestRunS12GracefulStopIsBlockedWhenTheStopCameAfterRestoredbByItsReason(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	f.stopLog = slices.Delete(f.stopLog, 0, 1)
	f.stopLog[0] = gsrLine{250 * time.Millisecond, func(at time.Time) string {
		return gsrLogLine(at, logOperationFailed, "operation_state_changed", "", testGSROp, "restore",
			errStoppingText, "")
	}}
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Blocked, "placement missed: the restore operation "+testGSROp+" failed with")
}

func TestRunS12GracefulStopFailsOnACUBRIDHolderAfterTheCleanupBegan(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	worker := testGSRWorker + " cub_admin cub_admin restoredb -u -B " + testGSRStaging + "/backup " + databaseName
	f.afterStop[2] = gsrSample{at: 200 * time.Millisecond, processes: []string{worker},
		holders: []string{testGSRWorker + " " + testGSRDatabase + "/" + databaseName + "_lgat (deleted)"}}
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Fail, "a CUBRID process held a file of the restore",
		testGSRDatabase+"/"+databaseName+"_lgat (deleted)", "restoredb ran after")
	rec := readGSRRecord(t, f.cfg)
	if len(rec.Sampler.HoldersAfterCleanup) != 1 || len(rec.Sampler.UtilityAfterCleanup) != 1 {
		t.Fatalf("sampler = %+v", rec.Sampler)
	}
}

func TestRunS12GracefulStopFailsWhenADirectoryOutlivesTheFailedRecord(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	// The database directory goes, but the staging is still there in a
	// reading started after the operation was logged Failed.
	f.afterStop[2] = gsrSample{at: 300 * time.Millisecond, entries: []string{testGSRStagingDir}}
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Fail, "readings started after the restore operation was logged "+
		logOperationFailed)
}

func TestRunS12GracefulStopFailsWhenTheShutdownPrecedesTheFailedRecord(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	f.stopLog[1], f.stopLog[2] = f.stopLog[2], f.stopLog[1]
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Fail, heartbeatStop+" (line", "before the restore operation's "+
		logOperationFailed)
}

func TestRunS12GracefulStopIsBlockedWhenTheLogLacksTheShutdown(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	f.stopLog = f.stopLog[:2]
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Blocked, "is incomplete: it holds no the shutdown's "+heartbeatStop)
}

func TestRunS12GracefulStopFailsOnAnotherFailureReason(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	other := "restore failed: restoredb failed: exit status 1: " + testGSRKilled
	f.stopLog[1] = gsrLine{250 * time.Millisecond, func(at time.Time) string {
		return gsrLogLine(at, logOperationFailed, "operation_state_changed", "", testGSROp, "restore", other, "")
	}}
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Fail, "its reason does not start with")
}

func TestRunS12GracefulStopFailsWhenTheRetryFailsAgain(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	again := "on " + testGSRTarget + ": restore failed: restoredb failed: exit status 1; starting attempt 3 of 3"
	f.afterDelete = slices.Insert(f.afterDelete, 3, imStatus{databasev1alpha1.BootstrapRestoring, 2, "",
		s12RetryReason, again}, imStatus{databasev1alpha1.BootstrapRestoring, 2, "op-restore-2",
		testRestoreInProgress, "restoredb is running on " + testGSRTarget})
	for i := 4; i < len(f.afterDelete); i++ {
		f.afterDelete[i].attempts, f.afterDelete[i].op = 2, "op-restore-2"
	}
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Fail, "the restore failed again after the attempt the stop ended")
}

func TestRunS12GracefulStopIsBlockedWhenTheDeletionIsRefused(t *testing.T) {
	f := newS12GracefulStopFixture(t, testS12GracefulStopConfig(t))
	f.deleteErr = fmt.Errorf("precondition failed")
	stop, err := gracefulStopRestoreOf(t, f)
	requireGSR(t, stop, err, evidence.Blocked, "the restore target's Pod could not be deleted: precondition failed")
}

func TestRunHARefusesS12GracefulStopOutsideItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*HAConfig)
		want   string
	}{
		"no -restore": {change: func(c *HAConfig) { c.Restore = false }, want: flagRestoreGracefulStop +
			" needs -restore"},
		"and -restore-tampered": {change: func(c *HAConfig) { c.RestoreTampered = true },
			want: "-restore-tampered select different lanes"},
		"and -restore-wrong-target": {change: func(c *HAConfig) { c.RestoreWrongTarget = true },
			want: "-restore-wrong-target select different lanes"},
		"and -restore-operator-restart": {change: func(c *HAConfig) {
			c.RestoreOperatorRestart, c.ResyncLimit, c.LeaderLease = true, time.Second, testLeaderLease
		}, want: "-restore-operator-restart select different lanes"},
		"and -restore-instance-manager-restart": {change: func(c *HAConfig) { c.RestoreInstanceManagerRestart = true },
			want: "-restore-instance-manager-restart select different lanes"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12GracefulStopConfig(t)
			tc.change(&cfg)
			f := newS12Fixture(t, testS12Config(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunHA() error = %v, want the configuration refused for %q", err, tc.want)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}
