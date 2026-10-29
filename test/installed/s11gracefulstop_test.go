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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testGSBackupUID = "gracefulstop-backup-uid"
	// testGSReplacementUID is the Pod the StatefulSet creates in place of
	// the deleted member.
	testGSReplacementUID = "gracefulstop-pod-2"
	// testGSKilled is the end of backupdb's killed process group.
	testGSKilled = "signal: killed: "
)

// testGSStaging is the operation's staging directory below the staging
// root, and testGSFile the file backupdb writes in it.
var (
	testGSStaging = testGSBackupUID + "/" + testIMOperation
	testGSFile    = imBackupStagingRoot + "/" + testGSStaging + "/" + databaseName + "_bk0v000"
)

func testS11GracefulStopConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupGracefulStop = true
	cfg.BackupGracefulStopReadyLimit = 5 * time.Second
	cfg.BackupGracefulStopFillerMB = 1
	return cfg
}

// testGSNow stands for the time of a log line, which the fake log writes
// when it writes the line.
const testGSNow = "@now@"

// gsSample is one reading the fake sampler prints: its processes, holders
// and entries, as the script prints them after their letter.
type gsSample struct {
	processes, holders, entries []string
}

// s11GracefulStopFixture plays, after S11 complete, an Operator that forms
// the standalone cluster s11-gracefulstop and takes its backup, the
// sampler's exec session and the followed log of its member, and the
// API server and StatefulSet when the run deletes the member's Pod. Before
// the backup is requested, the sampler prints readings with cub_server
// only; once it is, readings with backupdb running and its file open, until
// the Pod is deleted; then afterStop, and its output ends, as the
// container's end ends the exec session. The log holds two lines before the
// deletion and stopLog after it, and then ends. The backup is Running from
// its second reading until the deletion, Running at runningAfterDelete
// readings after it, and then Failed with failMessage, or Completed with an
// artifact when completeAfterDelete.
type s11GracefulStopFixture struct {
	*s11InterruptedFixture
	// neverSeen keeps backupdb out of every reading; the backup then goes
	// Uploading and Completed without a deletion.
	neverSeen bool
	// afterStop are the readings after the deletion; stopLog the log lines
	// after it.
	afterStop []gsSample
	stopLog   []string
	// failMessage is the reason of the Failed backup; completeAfterDelete
	// ends it Completed instead; runningAfterDelete is how many readings
	// after the deletion stay Running.
	failMessage         string
	completeAfterDelete bool
	runningAfterDelete  int
	// notReadyAgain keeps the cluster not Ready after the deletion;
	// manifestAfterDelete leaves a manifest.json under the prefix.
	notReadyAgain       bool
	manifestAfterDelete bool
	// failReason is the Ready reason of the Failed backup, BackupFailed
	// unless set; noOperationRef leaves status.operationRef empty;
	// deleteErr refuses the deletion of the member's Pod.
	failReason     string
	noOperationRef bool
	deleteErr      error
	// exitErrorAt makes the filler's statements with these numbers print
	// csql's -199 and -111 after committing, as POC-44 saw; stuckAt makes the
	// statement with that number, and its retry, add no row.
	exitErrorAt map[int]bool
	stuckAt     int

	gsBackup *databasev1alpha1.CubridBackup
	// fillerRows is the filler table's row count, nil before it is created;
	// fillerSQL the filler's statements as run, counts left out.
	fillerRows *int
	fillerSQL  []string
	gsReads    int
	afterReads int
	gsFinal    databasev1alpha1.CubridBackupPhase
	deleted    bool
	podDeletes []string
	// stopped is closed at the deletion.
	stopped     chan struct{}
	samplerRuns int
	logFollows  int
}

func newS11GracefulStopFixture(t *testing.T, cfg HAConfig) *s11GracefulStopFixture {
	t.Helper()
	f := &s11GracefulStopFixture{s11InterruptedFixture: newS11InterruptedFixture(t, cfg),
		runningAfterDelete: 1, stopped: make(chan struct{}),
		failMessage: stopReasonPrefix + "backupdb failed: " + testGSKilled}
	backupdb := "4242 cub_admin cub_admin backupdb -D " + imBackupStagingRoot + "/" + testGSStaging + " -C -l 0 " +
		databaseName + "@localhost"
	server := "77 cub_server cub_server " + databaseName
	f.afterStop = []gsSample{
		{processes: []string{server}, holders: []string{"77 " + testGSFile}, entries: []string{testGSStaging}},
		{processes: []string{server}, entries: []string{testGSStaging}},
		{processes: []string{server}},
		{processes: []string{server}}}
	f.stopLog = []string{
		gsLogLine("Command failed", "command", "cubrid backupdb -D "+imBackupStagingRoot+"/"+testGSStaging+
			" -C -l 0 appdb@localhost", "", "", `"durationMs":812,"exitCode":-1`),
		gsLogLine("Waiting for processes to close backup staging files", logWriterWait, "", testIMOperation, "", ""),
		gsLogLine("Processes closed backup staging files", logWriterClosed, "", testIMOperation, "", ""),
		gsLogLine(logOperationFailed, "operation_state_changed", "", testIMOperation, f.failMessage, ""),
		gsLogLine("Command failed", "command", heartbeatStop, "", "", `"exitCode":1`),
		gsLogLine("Ran command", "command", serverStop+" "+databaseName, "", "", `"exitCode":0`)}
	f.interruptedRead = f.readCluster
	f.podDeleted = f.deletePod
	f.memberHook = f.du
	f.csqlHook = f.filler
	created, read := f.backupCreated, f.backupRead
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		if backup.Name != s11GracefulStopBackupName {
			return created(ctx, c, backup)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		backup.UID = testGSBackupUID
		f.gsBackup = backup.DeepCopy()
		return nil
	}
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name != s11GracefulStopBackupName {
			return read(ctx, c, key)
		}
		return f.advanceGS(ctx, c, key)
	}
	f.runner.execStream = func(ctx context.Context, args ...string) (io.ReadCloser, error) {
		want := "-n " + cfg.Namespace + " exec " + s11GracefulStopMember + " -c " + databaseContainer +
			" -- bash -c " + gracefulStopSamplerScript()
		if strings.Join(args, " ") != want || len(args) != 10 {
			t.Errorf("streamed command = %q, want the sampler in %s", args, s11GracefulStopMember)
		}
		f.mu.Lock()
		f.samplerRuns++
		f.mu.Unlock()
		return f.stream(ctx, func(w io.Writer) { f.sample(w, backupdb, server) }), nil
	}
	f.runner.followLogs = func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (
		io.ReadCloser, error) {
		if namespace != cfg.Namespace || pod != s11GracefulStopMember || opts.Container != databaseContainer ||
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

// testGSExitError is what csql printed, on the VM lab, after a statement it
// had committed (POC-44).
const testGSExitError = "\nERROR CODE = -199\nServer no longer responding....\nInvalid argument\n" +
	"ERROR CODE = -111\nYour transaction has been aborted by the system due to server failure or mode change.\n"

// du answers du -sk of the databases' directory in the standalone member.
func (f *s11GracefulStopFixture) du(_ context.Context, pod, script string) (string, error, bool) {
	if pod != s11GracefulStopMember || script != imDatabasesDir {
		return "", nil, false
	}
	return "2048\t" + imDatabasesDir + "\n", nil, true
}

// filler plays csql on the filler table in the standalone member: it
// creates the table, inserts the first row, copies the rows LIMIT names, and
// counts them. A statement in exitErrorAt commits and then prints
// testGSExitError; the statement stuckAt and its retry add nothing.
func (f *s11GracefulStopFixture) filler(pod, sql string) (string, error, bool) {
	if !strings.Contains(sql, fillerTable) {
		return "", nil, false
	}
	if pod != s11GracefulStopMember {
		f.t.Errorf("filler SQL in %s, want %s", pod, s11GracefulStopMember)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if sql == fillerCount {
		if f.fillerRows == nil {
			return "ERROR: Unknown class \"dba." + fillerTable + "\".\n", nil, true
		}
		return fmt.Sprintf("=== <Result of SELECT Command in Line 1> ===\n\n  cast(count(*) as varchar)\n======\n"+
			"  '%d'\n\n1 row selected.\n", *f.fillerRows), nil, true
	}
	f.fillerSQL = append(f.fillerSQL, sql)
	statement := len(f.fillerSQL)
	switch {
	case sql == fillerCreate:
		zero := 0
		f.fillerRows = &zero
	case f.stuckAt > 0 && (statement == f.stuckAt || statement == f.stuckAt+1):
	case sql == fillerSeed:
		*f.fillerRows = 1
	default:
		var k int
		if _, err := fmt.Sscanf(sql, "INSERT INTO "+fillerTable+" (pad) SELECT pad FROM "+fillerTable+
			" LIMIT %d; COMMIT;", &k); err != nil {
			f.t.Errorf("filler SQL %q: %v", sql, err)
		}
		*f.fillerRows += k
	}
	out := "Execute OK. (1 rows affected)\nCommitted.\n"
	if f.exitErrorAt[statement] {
		out += testGSExitError
	}
	return out, nil, true
}

// gsLogLine is one line of the Instance Manager's JSON log, with testGSNow
// for its time.
func gsLogLine(msg, event, command, operation, reason, extra string) string {
	fields := map[string]any{"time": testGSNow, "level": "INFO", "msg": msg,
		"component": "instance-manager", "member": s11GracefulStopMember}
	for k, v := range map[string]string{"event": event, "command": command, "operationID": operation,
		"reason": reason} {
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
// write returns, or when ctx ends or the reader closes it.
func (f *s11GracefulStopFixture) stream(ctx context.Context, write func(io.Writer)) io.ReadCloser {
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

// sample writes readings until the deletion, then afterStop.
func (f *s11GracefulStopFixture) sample(w io.Writer, backupdb, server string) {
	write := func(s gsSample) bool {
		var b strings.Builder
		fmt.Fprintf(&b, "T %d.%09d\n", time.Now().Unix(), time.Now().Nanosecond())
		for _, p := range s.processes {
			b.WriteString("P " + p + "\n")
		}
		for _, e := range s.entries {
			b.WriteString("E " + e + "\n")
		}
		for _, p := range s.processes {
			b.WriteString("F " + strings.Fields(p)[0] + "\n")
		}
		for _, h := range s.holders {
			b.WriteString("H " + h + "\n")
		}
		b.WriteString(".\n")
		_, err := io.WriteString(w, b.String())
		return err == nil
	}
	for {
		select {
		case <-f.stopped:
			for _, s := range f.afterStop {
				if !write(s) {
					return
				}
			}
			return
		default:
		}
		f.mu.Lock()
		requested, never := f.gsBackup != nil, f.neverSeen
		f.mu.Unlock()
		s := gsSample{processes: []string{server}}
		if requested && !never {
			s = gsSample{processes: []string{server, backupdb}, holders: []string{"77 " + testGSFile},
				entries: []string{testGSStaging}}
		}
		if !write(s) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// log writes the member's log: two lines, and stopLog once the Pod was
// deleted.
func (f *s11GracefulStopFixture) log(w io.Writer) {
	for _, line := range []string{gsLogLine("Instance Manager listening", "listening", "", "", "", ""),
		gsLogLine("Ran command", "command", "cubrid server start "+databaseName, "", "", `"exitCode":0`)} {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return
		}
	}
	<-f.stopped
	for _, line := range f.stopLog {
		line = strings.Replace(line, testGSNow, time.Now().UTC().Format(time.RFC3339Nano), 1)
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return
		}
	}
}

// deletePod plays the API server and the StatefulSet when the run deletes
// the member: the deletion must carry the Pod's UID, the Pod goes and a
// replacement with another UID runs a ready database container.
func (f *s11GracefulStopFixture) deletePod(ctx context.Context, c client.WithWatch, pod *corev1.Pod,
	opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	uid := ""
	if options.Preconditions != nil && options.Preconditions.UID != nil {
		uid = string(*options.Preconditions.UID)
	}
	if pod.Name != s11GracefulStopMember || uid != testInterruptedPodUID || options.GracePeriodSeconds != nil {
		f.t.Errorf("deleted Pod %s with UID precondition %q and grace period %v, want %s with %s and its own grace "+
			"period", pod.Name, uid, options.GracePeriodSeconds, s11GracefulStopMember, testInterruptedPodUID)
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
	replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: stored.Name, Namespace: stored.Namespace,
		UID: types.UID(testGSReplacementUID), OwnerReferences: stored.OwnerReferences},
		Spec: stored.Spec,
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: databaseContainer, Ready: true,
			ContainerID: testIMContainerID + "-2", State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}}}
	if err := c.Create(ctx, replacement); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.podDeletes = append(f.podDeletes, pod.Name+"/"+uid)
	if !f.deleted {
		f.deleted = true
		close(f.stopped)
	}
	return nil
}

// readCluster plays the Operator before a reading of the standalone cluster:
// Ready as S11 interrupted-upload's, but not Ready after the deletion when
// notReadyAgain.
func (f *s11GracefulStopFixture) readCluster(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	if err := f.advanceCluster(ctx, c, key); err != nil {
		return err
	}
	f.mu.Lock()
	down := f.deleted && f.notReadyAgain
	f.mu.Unlock()
	if !down {
		return nil
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, key, cluster); err != nil {
		return nil
	}
	cluster.Status.Conditions[0].Status, cluster.Status.Conditions[0].Reason = metav1.ConditionFalse, "MemberDown"
	return c.Update(ctx, cluster)
}

// nextGS is the backup's phase at this reading. It runs with f.mu held.
func (f *s11GracefulStopFixture) nextGS() databasev1alpha1.CubridBackupPhase {
	if f.gsFinal != "" {
		return f.gsFinal
	}
	f.gsReads++
	switch {
	case f.gsReads == 1:
		return databasev1alpha1.BackupPhasePending
	case f.neverSeen && f.gsReads == 3:
		return databasev1alpha1.BackupPhaseUploading
	case f.neverSeen && f.gsReads > 3:
		f.gsFinal = databasev1alpha1.BackupPhaseCompleted
		return f.gsFinal
	case f.deleted:
		f.afterReads++
		if f.afterReads <= f.runningAfterDelete {
			return databasev1alpha1.BackupPhaseRunning
		}
		f.gsFinal = databasev1alpha1.BackupPhaseFailed
		if f.completeAfterDelete {
			f.gsFinal = databasev1alpha1.BackupPhaseCompleted
		}
		return f.gsFinal
	}
	return databasev1alpha1.BackupPhaseRunning
}

// advanceGS writes the status of the backup's phase at this reading.
func (f *s11GracefulStopFixture) advanceGS(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	backup := &databasev1alpha1.CubridBackup{}
	if err := c.Get(ctx, key, backup); err != nil {
		return nil
	}
	f.mu.Lock()
	phase := f.nextGS()
	message, manifest := f.failMessage, f.manifestAfterDelete && f.deleted
	reason, noRef := cmp.Or(f.failReason, s11MissingReason), f.noOperationRef
	f.mu.Unlock()
	started := metav1.NewTime(time.Now().Add(-time.Second))
	dest := backup.Spec.Destination.ObjectStorage
	attempt := path.Join(dest.Prefix, testAttempt)
	s := &backup.Status
	s.Phase, s.ClusterUID, s.StartedAt = phase, testInterruptedClusterUID, &started
	s.Conditions = []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse,
		Reason: testRunningReason, Message: testInProgress, LastTransitionTime: started}}
	if phase != databasev1alpha1.BackupPhasePending {
		s.TargetInstance, s.TargetRole = s11GracefulStopMember, string(databasev1alpha1.RoleUnknown)
		if !noRef {
			s.OperationRef = testIMOperation
		}
	}
	if manifest {
		f.store.put(dest.Bucket, attempt+"/"+instancemanager.ManifestObjectName, []byte("{}"))
	}
	switch phase {
	case databasev1alpha1.BackupPhaseUploading:
		s.Conditions[0].Reason, s.Conditions[0].Message = testUploadRunning, testUploadingMessage
	case databasev1alpha1.BackupPhaseFailed:
		s.CompletedAt = &started
		s.Conditions[0].Reason, s.Conditions[0].Message = reason, message
	case databasev1alpha1.BackupPhaseCompleted:
		s.CompletedAt = &started
		s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: "s3://" + dest.Bucket + "/" + attempt +
			"/manifest.json", ManifestDigest: strings.Repeat("a", 64), SizeBytes: 1, Database: databaseName,
			Level: databasev1alpha1.CubridBackupFull}
		s.Conditions[0].Status, s.Conditions[0].Reason, s.Conditions[0].Message = metav1.ConditionTrue,
			testCompleted, testCompletedMessage
	}
	return c.Update(ctx, backup)
}

// gracefulStopOf runs the fixture and returns S11 graceful-stop, requiring
// LAB01 and S11 complete passed.
func gracefulStopOf(t *testing.T, f *s11GracefulStopFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	f.requireForwardStopped(t)
	summary := readSummary(t, f.cfg.EvidenceDir)
	names := make([]string, 0, len(summary.Scenarios))
	for i, s := range summary.Scenarios {
		names = append(names, s.Name())
		if i < 2 && s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01 and S11 complete passed", s.Name(), s)
		}
	}
	want := []string{haScenarioID, s11ID + "/" + s11Variant, s11ID + "/" + s11GracefulStopVariant}
	if summary.Lane != s11GracefulStopLane || !slices.Equal(names, want) {
		t.Fatalf("lane %s with %v, want %v in lane %s", summary.Lane, names, want, s11GracefulStopLane)
	}
	gs := summary.Scenarios[2]
	requireOwnEvidence(t, gs, "S11/"+s11GracefulStopVariant)
	return gs, err
}

func readGracefulStopRecord(t *testing.T, dir string) s11GracefulStopRecord {
	t.Helper()
	var rec s11GracefulStopRecord
	data, err := os.ReadFile(filepath.Join(dir, s11GracefulStopRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 graceful-stop record = %s, %v", data, err)
	}
	return rec
}

func TestRunS11GracefulStopPassesWhenTheStagingIsSettledBeforeCUBRIDStops(t *testing.T) {
	cfg := testS11GracefulStopConfig(t)
	f := newS11GracefulStopFixture(t, cfg)
	scenario, err := gracefulStopOf(t, f)
	if err != nil || scenario.Result != evidence.Pass {
		t.Fatalf("S11 graceful-stop = %+v, RunHA() error = %v; want a pass", scenario, err)
	}
	if !slices.Equal(f.podDeletes, []string{s11GracefulStopMember + "/" + testInterruptedPodUID}) ||
		f.samplerRuns != 1 || f.logFollows != 1 {
		t.Fatalf("deletes = %v, sampler runs = %d, followed logs = %d; want one deletion, one sampler and one log",
			f.podDeletes, f.samplerRuns, f.logFollows)
	}
	if !slices.Contains(f.writes, "gracefulstop-cluster") || slices.Contains(f.writes, "ephemeral-container") {
		t.Fatalf("writes = %v, want the cluster and no ephemeral container", f.writes)
	}
	want := []string{"CubridBackup/" + s11GracefulStopBackupName + "/" + testGSBackupUID,
		testKindCluster + "/" + s11GracefulStopClusterName + "/" + testInterruptedClusterUID}
	if !slices.Equal(f.s11InterruptedFixture.deleted, want) || scenario.CleanupSucceeded == nil ||
		!*scenario.CleanupSucceeded {
		t.Fatalf("deleted = %v, cleanup = %v; want %v", f.s11InterruptedFixture.deleted, scenario.CleanupSucceeded,
			want)
	}
	requireGracefulStopMeasurements(t, scenario.Measurements)
	for _, file := range []string{s11GracefulStopSamplesFile, s11GracefulStopLogFile,
		s11GracefulStopSourceDataCheckFile} {
		if !slices.Contains(scenario.Evidence, file) {
			t.Fatalf("evidence = %v, want %s", scenario.Evidence, file)
		}
	}
	// 1,048 rows: the table, the first row, ten doublings to 1,024 and 24 more.
	wantSQL := []string{fillerCreate, fillerSeed}
	for k := 1; k <= 512; k *= 2 {
		wantSQL = append(wantSQL, fillerCopy(k))
	}
	wantSQL = append(wantSQL, fillerCopy(24))
	if !slices.Equal(f.fillerSQL, wantSQL) {
		t.Fatalf("filler statements = %q, want %q", f.fillerSQL, wantSQL)
	}
	rec := readGracefulStopRecord(t, cfg.EvidenceDir)
	requireGracefulStopRecord(t, rec, f.failMessage)
	if fl := rec.Filler; fl == nil || fl.Rows != fillerRows(1) || fl.TargetRows != fl.Rows || fl.Statements != 13 ||
		fl.Retries != 0 || fl.ExitErrors != 0 || fl.DatabaseDirKB != 2048 || fl.DataVolume != "2052Mi" ||
		fl.Took == "" {
		t.Fatalf("filler = %+v", fl)
	}
	if l := rec.Log; l.StopLine == 0 || l.StopLoggedAt == nil || l.StopLoggedAfterDelete == "" {
		t.Fatalf("log = %+v, want the first stop line timed", l)
	}
	requireNamespaceGone(t, f.haFixture)
}

// requireGracefulStopMeasurements checks the measurements of a passed run.
func requireGracefulStopMeasurements(t *testing.T, m map[string]any) {
	t.Helper()
	if m[measHoldersAfterGone] != float64(0) || m[measBackupdbDurationMs] != float64(812) ||
		m[measOperationRefUnchanged] != true || m[failedWithinKey] == evidence.Unknown ||
		m[measDeleteAfterSeen] == evidence.Unknown || m[measStagingGoneAfterDelete] == evidence.Unknown ||
		m[measReadyAgainWithin] == evidence.Unknown || m[measStopLoggedAfterDelete] == evidence.Unknown ||
		m[measFillerRows] != float64(fillerRows(1)) || m[measFillerGrowth] == evidence.Unknown {
		t.Fatalf("measurements = %v", m)
	}
}

// requireGracefulStopRecord checks the record of a passed run whose backup
// failed with failMessage.
func requireGracefulStopRecord(t *testing.T, rec s11GracefulStopRecord, failMessage string) {
	t.Helper()
	tr, s, l := rec.Trigger, rec.Sampler, rec.Log
	if tr.PodUID != testInterruptedPodUID || tr.SeenAt == nil || !strings.Contains(tr.SeenProcess, "backupdb") ||
		tr.DeleteAcceptedAt == nil || tr.ReplacementUID != testGSReplacementUID || tr.ReadyAgainAt == nil {
		t.Fatalf("trigger = %+v", tr)
	}
	if s.StagingDir != testGSStaging || s.StagingGoneAt == nil || s.LastHolder != "77 "+testGSFile ||
		len(s.HoldersAfterGone) != 0 || s.Truncated {
		t.Fatalf("sampler = %+v", s)
	}
	if l.BackupdbResult != logCommandFailed || len(l.WriterEvents) != 2 || l.OperationFailedLine == 0 ||
		l.HeartbeatStopLine <= l.OperationFailedLine || l.ServerStopLine <= l.HeartbeatStopLine ||
		l.OperationFailedReason != failMessage {
		t.Fatalf("log = %+v", l)
	}
	if rec.Failure != s11MissingReason+": "+failMessage || !rec.EndedAfterDelete || rec.Artifact != nil ||
		!slices.Equal(rec.OperationRefsSeen, []string{testIMOperation}) || rec.ManifestAtEnd {
		t.Fatalf("record = %+v", rec)
	}
	if rec.PodSecurity[podSecurityEnforce] != podSecurityRestricted {
		t.Fatalf("Pod Security = %v, want the namespace left at restricted", rec.PodSecurity)
	}
}

func TestRunS11GracefulStopJudgesTheOutcome(t *testing.T) {
	kept := stopReasonPrefix + "backupdb failed: " + testGSKilled + "; the backup staging " + imBackupStagingRoot +
		"/" + testGSStaging + " is kept and is not uploaded or reused: a process still held a file in it after " +
		"30s (77 cub_server " + testGSFile + ")"
	for name, tc := range map[string]struct {
		change func(f *s11GracefulStopFixture)
		want   evidence.Result
		reason string
	}{
		"backupdb never seen": {change: func(f *s11GracefulStopFixture) { f.neverSeen = true },
			want: evidence.Blocked, reason: "placement missed: the backup was Uploading before the sampler showed"},
		"a reason of the upload": {change: func(f *s11GracefulStopFixture) {
			f.failMessage = memberStopping + uploadFailed + "put b/k: context canceled"
		}, want: evidence.Blocked, reason: "the stop reached the Instance Manager after backupdb had ended"},
		"the staging kept": {change: func(f *s11GracefulStopFixture) { f.failMessage = kept },
			want: evidence.Fail, reason: "the backup ended Failed with a kept staging"},
		"the restart reason": {change: func(f *s11GracefulStopFixture) { f.failMessage = s12IMRestartedReason },
			want: evidence.Fail, reason: "its reason does not start with"},
		"a holder after the staging is gone": {change: func(f *s11GracefulStopFixture) {
			f.afterStop[2].holders = []string{"77 " + testGSFile + " (deleted)"}
		}, want: evidence.Fail, reason: "a process held a file below " + imBackupStagingRoot + "/" + testGSBackupUID},
		"the staging never seen gone": {change: func(f *s11GracefulStopFixture) {
			f.afterStop = f.afterStop[:2]
		}, want: evidence.Blocked, reason: "the sampler did not show the staging directory " + testGSStaging + " gone"},
		"heartbeat stop before the operation failed": {change: func(f *s11GracefulStopFixture) {
			f.stopLog[3], f.stopLog[4] = f.stopLog[4], f.stopLog[3]
		}, want: evidence.Fail, reason: "the member's log holds " + heartbeatStop},
		"a writer event after the operation failed": {change: func(f *s11GracefulStopFixture) {
			f.stopLog = append(f.stopLog[:2], f.stopLog[3], f.stopLog[2], f.stopLog[4], f.stopLog[5])
		}, want: evidence.Fail, reason: "a staging writer event (line"},
		"Operation failed missing": {change: func(f *s11GracefulStopFixture) {
			f.stopLog = slices.Delete(f.stopLog, 3, 4)
		}, want: evidence.Blocked, reason: "is incomplete: it holds no " + logOperationFailed},
		"Completed with an artifact": {change: func(f *s11GracefulStopFixture) { f.completeAfterDelete = true },
			want: evidence.Fail, reason: "ended Completed after its member's Pod was deleted and the member's log " +
				"shows backupdb end for the stop"},
		"Completed after backupdb's success in the log": {change: func(f *s11GracefulStopFixture) {
			f.completeAfterDelete = true
			f.stopLog[0] = strings.Replace(f.stopLog[0], `"Command failed"`, `"Ran command"`, 1)
		}, want: evidence.Blocked, reason: "placement missed: the backup ended Completed"},
		// The VM-lab run of 2026-10-29 19:36 UTC: backupdb succeeded before
		// the stop reached the Instance Manager, and the backup completed with
		// its manifest; the log holds no Operation failed.
		"the lab's placement miss with a manifest": {change: func(f *s11GracefulStopFixture) {
			f.completeAfterDelete, f.manifestAfterDelete = true, true
			f.stopLog = []string{strings.Replace(f.stopLog[0], `"Command failed"`, `"Ran command"`, 1),
				f.stopLog[4], f.stopLog[5]}
		}, want: evidence.Blocked, reason: "placement missed"},
		"a refused deletion": {change: func(f *s11GracefulStopFixture) {
			f.deleteErr = errors.New("pods \"" + s11GracefulStopMember + "\" is forbidden")
		}, want: evidence.Blocked, reason: "the member's Pod could not be deleted: pods"},
		"RecordedOperationLost": {change: func(f *s11GracefulStopFixture) { f.failReason = s11OperationLostReason },
			want: evidence.Blocked, reason: "the documented escalation " + s11OperationLostReason},
		"heartbeat stop missing": {change: func(f *s11GracefulStopFixture) {
			f.stopLog = slices.Delete(f.stopLog, 4, 5)
		}, want: evidence.Blocked, reason: "it holds no the shutdown's " + heartbeatStop},
		"server stop before the operation failed": {change: func(f *s11GracefulStopFixture) {
			f.stopLog = []string{f.stopLog[0], f.stopLog[1], f.stopLog[2], f.stopLog[5], f.stopLog[3], f.stopLog[4]}
		}, want: evidence.Fail, reason: "the member's log holds " + serverStop + " (line"},
		"no operationRef read": {change: func(f *s11GracefulStopFixture) { f.noOperationRef = true },
			want: evidence.Blocked, reason: "no operationRef was read after the deletion"},
		"a filler statement that adds no row, also when run again": {change: func(f *s11GracefulStopFixture) {
			f.stuckAt = 4
		}, want: evidence.Blocked, reason: "did not grow past 2 rows after"},
		"the manifest under its prefix": {change: func(f *s11GracefulStopFixture) { f.manifestAfterDelete = true },
			want: evidence.Fail, reason: "holds a manifest.json under"},
		"the cluster not Ready again": {change: func(f *s11GracefulStopFixture) {
			f.notReadyAgain = true
			f.cfg.BackupGracefulStopReadyLimit = 300 * time.Millisecond
		}, want: evidence.Fail, reason: "was not Ready again within the ready limit"},
		"a credential in the followed log": {change: func(f *s11GracefulStopFixture) {
			f.stopLog = append(f.stopLog, gsLogLine("Upload", "upload", "", "", testSecretKey, ""))
		}, want: evidence.Fail, reason: "the followed log of the deleted member " + s11GracefulStopMember},
	} {
		t.Run(name, func(t *testing.T) {
			f := newS11GracefulStopFixture(t, testS11GracefulStopConfig(t))
			tc.change(f)
			scenario, err := gracefulStopOf(t, f)
			if err == nil || scenario.Result != tc.want || !strings.Contains(scenario.Reason, tc.reason) {
				t.Fatalf("S11 graceful-stop = %+v, RunHA() error = %v; want %s with %q", scenario, err, tc.want,
					tc.reason)
			}
			if scenario.CleanupSucceeded == nil || !*scenario.CleanupSucceeded {
				t.Fatalf("cleanup = %v, want the backup and the cluster deleted", scenario.CleanupSucceeded)
			}
			if tc.want == evidence.Blocked && len(scenario.NeverEvents) != 0 {
				t.Fatalf("never-events = %v, want none for a blocked variant", scenario.NeverEvents)
			}
		})
	}
}

// TestRunS11GracefulStopCountsTheFillerNotCSQLsExitErrors plays csql printing
// -199 and -111 after statements it committed, as POC-44 saw on the VM lab:
// the rows are counted, so the growth goes on, and the statements are
// recorded as exit errors. A statement whose first run adds no row is run
// once more.
func TestRunS11GracefulStopCountsTheFillerNotCSQLsExitErrors(t *testing.T) {
	cfg := testS11GracefulStopConfig(t)
	f := newS11GracefulStopFixture(t, cfg)
	f.exitErrorAt = map[int]bool{2: true, 6: true}
	scenario, err := gracefulStopOf(t, f)
	if err != nil || scenario.Result != evidence.Pass {
		t.Fatalf("S11 graceful-stop = %+v, RunHA() error = %v; want a pass", scenario, err)
	}
	fl := readGracefulStopRecord(t, cfg.EvidenceDir).Filler
	if fl == nil || fl.Rows != fillerRows(1) || fl.ExitErrors != 2 || fl.Retries != 0 || fl.Statements != 13 ||
		scenario.Measurements[measFillerExitErrors] != float64(2) {
		t.Fatalf("filler = %+v, measurements = %v; want every row and two exit errors", fl,
			scenario.Measurements)
	}
}

func TestGrowFillerRunsAStatementThatAddedNoRowOnceMore(t *testing.T) {
	cfg := testS11GracefulStopConfig(t)
	f := newS11GracefulStopFixture(t, cfg)
	// The third statement, the first copy, adds nothing at its first run
	// only: the fixture's stuckAt would hold its retry too.
	f.csqlHook = func(pod, sql string) (string, error, bool) {
		f.mu.Lock()
		first := len(f.fillerSQL) == 2 && sql == fillerCopy(1)
		if first {
			f.fillerSQL = append(f.fillerSQL, sql)
		}
		f.mu.Unlock()
		if first {
			return "Execute OK.\n" + testGSExitError, nil, true
		}
		return f.filler(pod, sql)
	}
	scenario, err := gracefulStopOf(t, f)
	if err != nil || scenario.Result != evidence.Pass {
		t.Fatalf("S11 graceful-stop = %+v, RunHA() error = %v; want a pass", scenario, err)
	}
	fl := readGracefulStopRecord(t, cfg.EvidenceDir).Filler
	if fl == nil || fl.Rows != fillerRows(1) || fl.Retries != 1 || fl.Statements != 14 || fl.ExitErrors != 0 {
		t.Fatalf("filler = %+v, want every row after one retry", fl)
	}
}

// TestJudgeStopSamplesRequiresTheHoldersAfterTheEntryIsGone judges readings
// directly: a holder in the reading that first lacks the operation's entry
// fails the variant, because the script lists the entries before the open
// files; readings that never showed an open file or a cub* process's
// listing block it.
func TestJudgeStopSamplesRequiresTheHoldersAfterTheEntryIsGone(t *testing.T) {
	at := time.Now().UTC()
	accepted := at.Add(-time.Second)
	held := []string{"77 " + testGSFile}
	for name, tc := range map[string]struct {
		samples []stopSample
		verdict bool
		reason  string
	}{
		"a holder and no entry in one reading": {samples: []stopSample{
			{Received: at, Entries: []string{testGSStaging}, Holders: held},
			{Received: at.Add(time.Millisecond), Holders: held}},
			verdict: true, reason: "a process held a file below"},
		"the holder gone with the entry": {samples: []stopSample{
			{Received: at, Entries: []string{testGSStaging}, Holders: held},
			{Received: at.Add(time.Millisecond)}}},
		"nothing seen of open files": {samples: []stopSample{
			{Received: at, Entries: []string{testGSStaging}},
			{Received: at.Add(time.Millisecond)}},
			reason: "the sampler could not see open files"},
		"a cub* process's files listed": {samples: []stopSample{
			{Received: at, Entries: []string{testGSStaging}, FdListed: []string{"77"}},
			{Received: at.Add(time.Millisecond)}}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &s11GracefulStopRecord{BackupUID: testGSBackupUID, OperationRef: testIMOperation,
				Trigger: &s11StopTrigger{DeleteAcceptedAt: &accepted},
				Sampler: &s11StopSampler{HoldersAfterGone: []string{}}}
			result := &evidence.Scenario{Measurements: map[string]any{}}
			err := judgeStopSamples(&stopWatch{samples: tc.samples}, rec, result)
			isVerdict := errors.As(err, new(backupVerdict))
			if (tc.reason == "") != (err == nil) || isVerdict != tc.verdict ||
				(err != nil && !strings.Contains(err.Error(), tc.reason)) {
				t.Fatalf("judgeStopSamples() = %v, want verdict %t with %q", err, tc.verdict, tc.reason)
			}
		})
	}
}

// TestJudgeStopBackupBlocksABackupReadEndedBeforeTheDeletion requires that a
// backup whose ended phase was read before the deletion was accepted is not
// judged as ended by the stop.
func TestJudgeStopBackupBlocksABackupReadEndedBeforeTheDeletion(t *testing.T) {
	accepted := time.Now().UTC()
	rec := &s11GracefulStopRecord{Cluster: &s11StandaloneCluster{Name: s11GracefulStopClusterName},
		Trigger: &s11StopTrigger{DeleteAcceptedAt: &accepted}, Log: &s11StopLog{BackupdbResult: logCommandFailed},
		Phases: []s11Phase{{At: accepted.Add(-time.Millisecond), Phase: databasePhaseFailed}}}
	backup := &databasev1alpha1.CubridBackup{Status: databasev1alpha1.CubridBackupStatus{
		Phase: databasev1alpha1.BackupPhaseFailed, Conditions: []metav1.Condition{{Type: backupReady,
			Reason: s11MissingReason, Message: stopReasonPrefix + testGSKilled}}}}
	err := judgeStopBackup(backup, nil, accepted.Add(-time.Minute), rec,
		&evidence.Scenario{Measurements: map[string]any{}})
	if err == nil || errors.As(err, new(backupVerdict)) ||
		!strings.Contains(err.Error(), "before its member's Pod was deleted") {
		t.Fatalf("judgeStopBackup() = %v, want blocked: read ended before the deletion", err)
	}
}

func TestReadyUnchangedNotesAReadyThatNeverWentFalse(t *testing.T) {
	accepted := time.Date(2026, 10, 29, 19, 36, 10, 500_000_000, time.UTC)
	for _, tc := range []struct {
		transition time.Time
		noted      bool
	}{{accepted.Add(-time.Minute), true}, {accepted.Truncate(time.Second), false},
		{accepted.Add(5 * time.Second), false}} {
		transition := tc.transition
		got := readyUnchanged(&s11StopTrigger{DeleteAcceptedAt: &accepted, ReadyTransitionAt: &transition})
		if (got != "") != tc.noted {
			t.Fatalf("readyUnchanged(transition %s) = %q, want noted %t", transition, got, tc.noted)
		}
	}
}

func TestRunS11GracefulStopIsBlockedWhenS11CompleteDidNotPass(t *testing.T) {
	cfg := testS11GracefulStopConfig(t)
	f := newS11GracefulStopFixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	summary := readSummary(t, cfg.EvidenceDir)
	if summary.Lane != s11GracefulStopLane || len(summary.Scenarios) != 3 {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/%s", summary, s11GracefulStopVariant)
	}
	if scenario := summary.Scenarios[2]; scenario.Variant != s11GracefulStopVariant ||
		scenario.Result != evidence.Blocked || !strings.Contains(scenario.Reason, "S11 complete did not pass") {
		t.Fatalf("S11 graceful-stop = %+v, want blocked by S11 complete", scenario)
	}
	if f.samplerRuns != 0 || len(f.podDeletes) != 0 {
		t.Fatalf("sampler runs = %d, deletes = %v; want nothing done", f.samplerRuns, f.podDeletes)
	}
}

func TestRunHARefusesBackupGracefulStopWithoutItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(cfg *HAConfig)
		want   string
	}{
		testNoBucket: {change: func(cfg *HAConfig) { cfg.BackupBucket = "" },
			want: flagGracefulStop + " needs -backup-bucket"},
		"without the ready limit": {change: func(cfg *HAConfig) { cfg.BackupGracefulStopReadyLimit = 0 },
			want: "-backup-graceful-stop-ready-limit"},
		testWithRestore: {change: func(cfg *HAConfig) {
			cfg.Restore, cfg.RestoreLimit = true, time.Minute
		}, want: flagGracefulStop + " and " + flagRestore + " select"},
		testWithMissing: {change: func(cfg *HAConfig) { cfg.BackupMissingDestination = true },
			want: flagGracefulStop + " and " + flagMissingDestination + " select"},
		testWithDenied: {change: func(cfg *HAConfig) {
			cfg.BackupDeniedCredentials, cfg.BackupDeniedReadyLimit = true, time.Minute
		}, want: flagGracefulStop + " and " + flagDeniedCredentials + " select"},
		"with S11 signature-mismatch selected": {change: func(cfg *HAConfig) {
			cfg.BackupSignatureMismatch, cfg.BackupSignatureMismatchReadyLimit = true, time.Minute
		}, want: flagSignatureMismatch + " and " + flagGracefulStop + " select"},
		testWithInterrupted: {change: func(cfg *HAConfig) {
			cfg.BackupInterruptedUpload, cfg.BackupInterruptedReadyLimit = true, time.Minute
		}, want: flagInterruptedUpload + " and " + flagGracefulStop + " select"},
		"with S11 operator-restart": {change: func(cfg *HAConfig) { cfg.BackupOperatorRestart = true },
			want: flagOperatorRestart + " and " + flagGracefulStop + " select"},
		"with S11 instance-manager-restart": {change: func(cfg *HAConfig) {
			cfg.BackupInstanceManagerRestart, cfg.BackupInstanceManagerRestartReadyLimit = true, time.Minute
		}, want: flagInstanceManagerRestart + " and " + flagGracefulStop + " select"},
		"with the overlapping variant": {change: func(cfg *HAConfig) {
			cfg.BackupOverlapping, cfg.BackupOverlappingReadyLimit = true, time.Minute
		}, want: flagOverlapping + " and " + flagGracefulStop + " select"},
		"with the operation-store-loss variant": {change: func(cfg *HAConfig) {
			cfg.BackupOperationStoreLoss, cfg.BackupOperationStoreLossReadyLimit = true, time.Minute
			cfg.BackupLimit = storeLossMinBackupLimit
		}, want: flagOperationStoreLoss + " and " + flagGracefulStop + " select"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11GracefulStopConfig(t)
			tc.change(&cfg)
			f := newS11GracefulStopFixture(t, testS11GracefulStopConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunHA() error = %v, want %s refused %s", err, flagGracefulStop, name)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}

func TestParseSampleTimeReadsSecondsAndNanoseconds(t *testing.T) {
	at, err := parseSampleTime("1793270400.123456789")
	if err != nil || !at.Equal(time.Unix(1793270400, 123456789)) {
		t.Fatalf("parseSampleTime() = %v, %v", at, err)
	}
	if _, err := parseSampleTime("now"); err == nil {
		t.Fatal("parseSampleTime(now) did not fail")
	}
}

// TestReadSamplesFiresOnceAtTheFirstBackupdbWhileArmed reads a sampler's
// output and requires the trigger to fire at the first reading that shows
// backupdb, and only while armed.
func TestReadSamplesFiresOnceAtTheFirstBackupdbWhileArmed(t *testing.T) {
	output := "T 1.0\nP 77 cub_server cub_server appdb\n.\n" +
		"T 1.1\nP 77 cub_server cub_server appdb\nP 9 cub_admin cub_admin backupdb -D x\nE u/op\n.\n" +
		"garbage\nT 1.2\n.\n"
	for _, armed := range []bool{false, true} {
		w := &stopWatch{seen: make(chan struct{}), armed: armed}
		s := &s11StopSampler{}
		if err := readSamples(strings.NewReader(output), w, s); err != nil {
			t.Fatal(err)
		}
		fired := w.fired
		if fired != armed || s.Samples != 3 || s.Unparsed != 1 || s.FirstBackupdbAt == nil ||
			!slices.Equal(w.samples[1].Entries, []string{"u/op"}) {
			t.Fatalf("armed %t: fired = %t, sampler = %+v, samples = %+v", armed, fired, s, w.samples)
		}
	}
}
