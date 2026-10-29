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
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testOverlapOpA = "backup-op-a"
	testOverlapOpB = "backup-op-b"
	// testOverlapTarget is the standalone cluster's one member, which both
	// backups target.
	testOverlapTarget = s11OverlappingMember
	// testOverlapHold is the message of a backup held after its target
	// refused the start with OperationInProgress, as holdRecordedTarget
	// writes it.
	testOverlapHold = "the member refused the start as a conflict: another operation runs on the database (409); " +
		"it holds no operation of this backup; waiting for at most 30m0s from when the target was recorded"
)

// testOverlapUIDs are the UIDs the fake API server gives the two backups.
var testOverlapUIDs = [2]string{"overlap-a-uid", "overlap-b-uid"}

// overlapStep is what the fake Operator reports of a backup at one reading.
type overlapStep struct {
	phase  databasev1alpha1.CubridBackupPhase
	target string
	op     string
	// reason is the Ready reason, BackupRunning when empty; message its
	// message.
	reason, message string
}

var (
	pendingStep = overlapStep{phase: databasev1alpha1.BackupPhasePending}
	heldStep    = overlapStep{phase: databasev1alpha1.BackupPhasePending, target: testOverlapTarget,
		reason: s11OperationInProgressReason, message: testOverlapHold}
	runningStep = func(target, op string) overlapStep {
		return overlapStep{phase: databasev1alpha1.BackupPhaseRunning, target: target, op: op}
	}
	uploadStep = func(target, op string) overlapStep {
		return overlapStep{phase: databasev1alpha1.BackupPhaseUploading, target: target, op: op}
	}
	doneStep = func(target, op string) overlapStep {
		return overlapStep{phase: databasev1alpha1.BackupPhaseCompleted, target: target, op: op}
	}
)

// s11OverlapFixture plays, after S11 complete, an Operator that forms the
// standalone cluster s11-overlapping and takes the two
// backups of S11 overlapping as steps say, one step per reading of each; the
// last step is kept. By default both run on the member: the first runs and
// completes while the second is held with OperationInProgress, then the
// second runs and completes.
type s11OverlapFixture struct {
	*s11InterruptedFixture
	steps [2][]overlapStep
	// tamperManifest changes the manifest of backup i before it is stored;
	// startedAt is status.startedAt of backup i, given when its operation was
	// first reported, which it defaults to. corrupt changes an object of
	// backup i's artifact after its manifest was written; secondAttempt
	// stores an object under a second attempt prefix of backup i, and
	// strayObject one the manifest does not name in its own attempt prefix.
	// tamperRecord changes the target's record of backup i's operation, which
	// is created when its operation is first reported and ends when it is
	// Completed; recordErr answers the read of that record instead.
	tamperManifest func(i int, m *instancemanager.BackupManifest)
	startedAt      func(i int, first time.Time) time.Time
	corrupt        [2]bool
	secondAttempt  [2]bool
	strayObject    [2]bool
	tamperRecord   func(i int, op *instancemanager.Operation)
	recordErr      [2]error
	// recordReads are the records read, as pod/operation ID.
	recordReads []string

	created   [2]*databasev1alpha1.CubridBackup
	reads     [2]int
	firstOp   [2]time.Time
	doneAt    [2]time.Time
	artifacts [2]*databasev1alpha1.CubridBackupArtifact
	// deleted are the backups the run deleted, with the UID it required.
	deleted []string
}

func testS11OverlapConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupOverlapping = true
	cfg.BackupOverlappingReadyLimit = 5 * time.Second
	return cfg
}

func newS11OverlapFixture(t *testing.T, cfg HAConfig) *s11OverlapFixture {
	t.Helper()
	f := &s11OverlapFixture{s11InterruptedFixture: newS11InterruptedFixture(t, cfg)}
	f.net.member = s11OverlappingMember
	f.steps = [2][]overlapStep{
		{pendingStep, runningStep(testOverlapTarget, testOverlapOpA), uploadStep(testOverlapTarget, testOverlapOpA),
			doneStep(testOverlapTarget, testOverlapOpA)},
		{heldStep, heldStep, heldStep, heldStep, runningStep(testOverlapTarget, testOverlapOpB),
			doneStep(testOverlapTarget, testOverlapOpB)}}
	created, read := f.backupCreated, f.backupRead
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		i := overlapIndex(backup.Name)
		if i < 0 {
			return created(ctx, c, backup)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		backup.UID = types.UID(testOverlapUIDs[i])
		f.created[i] = backup.DeepCopy()
		return nil
	}
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		i := overlapIndex(key.Name)
		if i < 0 {
			return read(ctx, c, key)
		}
		return f.advanceOverlap(ctx, c, key, i)
	}
	f.backupDeleted = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup,
		opts ...client.DeleteOption) error {
		options := (&client.DeleteOptions{}).ApplyOptions(opts)
		uid := ""
		if options.Preconditions != nil && options.Preconditions.UID != nil {
			uid = string(*options.Preconditions.UID)
		}
		f.mu.Lock()
		f.deleted = append(f.deleted, backup.Name+"/"+uid)
		f.mu.Unlock()
		return c.Delete(ctx, backup)
	}
	// The source's marker table holds no marker of a restored cluster.
	f.markersOf = func(string) []string { return nil }
	f.memberHook = f.operationRecord
	return f
}

// operationRecord answers the read of the target's record of an operation
// of the two backups, as the Instance Manager keeps it.
func (f *s11OverlapFixture) operationRecord(_ context.Context, pod, script string) (string, error, bool) {
	if !strings.HasPrefix(script, imOperationsDir+"/") {
		return "", nil, false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(script, imOperationsDir+"/"), ".json")
	i := slices.Index([]string{testOverlapOpA, testOverlapOpB}, id)
	if i < 0 {
		return "", errors.New("cat: no such file"), true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordReads = append(f.recordReads, pod+"/"+id)
	if f.recordErr[i] != nil {
		return "", f.recordErr[i], true
	}
	op := instancemanager.Operation{ID: id, Kind: instancemanager.OpBackup, State: instancemanager.OpCompleted,
		CreatedAt: f.firstOp[i], UpdatedAt: f.doneAt[i]}
	if f.tamperRecord != nil {
		f.tamperRecord(i, &op)
	}
	data, err := json.Marshal(op)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data), nil, true
}

// overlapIndex is the index of the S11 overlapping backup name, or -1.
func overlapIndex(name string) int {
	for i, b := range s11OverlappingBackups {
		if b.name == name {
			return i
		}
	}
	return -1
}

// advanceOverlap writes the status of backup i's next reading, as the
// Operator would.
func (f *s11OverlapFixture) advanceOverlap(ctx context.Context, c client.WithWatch, key client.ObjectKey,
	i int) error {
	backup := &databasev1alpha1.CubridBackup{}
	if err := c.Get(ctx, key, backup); err != nil {
		return nil
	}
	f.mu.Lock()
	f.reads[i]++
	st := f.steps[i][min(f.reads[i], len(f.steps[i]))-1]
	if st.op != "" && f.firstOp[i].IsZero() {
		f.firstOp[i] = time.Now()
	}
	if st.phase == databasev1alpha1.BackupPhaseCompleted && f.doneAt[i].IsZero() {
		f.doneAt[i] = time.Now()
	}
	message := st.message
	if st.reason != "" {
		// Each refusal names its own request, as imclient's answers do.
		message = fmt.Sprintf("%s (request %d)", st.message, f.reads[i])
	}
	first, startedAt := f.firstOp[i], f.startedAt
	f.mu.Unlock()
	if inProgress(st.phase) {
		n := f.net
		n.mu.Lock()
		if n.throttleIP != "" {
			n.send(n.throttleIP, testUploadPerRead)
		}
		n.mu.Unlock()
	}
	now := metav1.Now()
	s := &backup.Status
	s.Phase, s.ClusterUID, s.OperationRef = st.phase, testInterruptedClusterUID, st.op
	s.TargetInstance, s.TargetRole = st.target, ""
	if st.target != "" {
		s.TargetRole = string(databasev1alpha1.RoleUnknown)
	}
	s.Conditions = []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse,
		Reason: cmp.Or(st.reason, testRunningReason), Message: cmp.Or(message, testInProgress),
		LastTransitionTime: now}}
	if st.op != "" {
		at := first
		if startedAt != nil {
			at = startedAt(i, first)
		}
		started := metav1.NewTime(at)
		s.StartedAt = &started
	}
	switch st.phase {
	case databasev1alpha1.BackupPhaseUploading:
		s.Conditions[0].Reason, s.Conditions[0].Message = testUploadRunning, testUploadingMessage
	case databasev1alpha1.BackupPhaseCompleted:
		s.Artifact = f.uploadOverlap(i, backup, st)
		s.CompletedAt = &now
		s.Conditions[0].Status, s.Conditions[0].Reason, s.Conditions[0].Message = metav1.ConditionTrue,
			testCompleted, testCompletedMessage
	case databasev1alpha1.BackupPhaseFailed:
		s.CompletedAt = &now
	}
	if err := c.Update(ctx, backup); err != nil {
		f.t.Error(err)
	}
	return nil
}

// uploadOverlap writes backup i's artifact once, as upload writes S11
// complete's, with its own operation and target in the manifest, and
// returns what the status reports of it; S11 complete's artifact is kept for
// its own readings.
func (f *s11OverlapFixture) uploadOverlap(i int, backup *databasev1alpha1.CubridBackup,
	st overlapStep) *databasev1alpha1.CubridBackupArtifact {
	f.mu.Lock()
	if f.artifacts[i] != nil {
		defer f.mu.Unlock()
		return f.artifacts[i].DeepCopy()
	}
	s11 := f.s11Fixture
	saved, savedTamper, tamper := s11.artifact, s11.tamper, f.tamperManifest
	s11.artifact = nil
	s11.tamper = func(a *fakeArtifact) {
		a.manifest.OperationID, a.manifest.SourceInstance = st.op, st.target
		a.manifest.SourceRole, a.manifest.ClusterUID = string(databasev1alpha1.RoleUnknown), testInterruptedClusterUID
		a.move(testClusterUID, testInterruptedClusterUID)
		if tamper != nil {
			tamper(i, &a.manifest)
		}
	}
	f.mu.Unlock()
	artifact := s11.upload(backup)
	f.mu.Lock()
	s11.artifact, s11.tamper, f.artifacts[i] = saved, savedTamper, artifact.DeepCopy()
	f.mu.Unlock()
	dest := backup.Spec.Destination.ObjectStorage
	dir := path.Join(dest.Prefix, testInterruptedClusterUID, databaseName, string(backup.UID))
	if f.corrupt[i] {
		f.store.put(dest.Bucket, dir+"/"+testAttempt+"/"+testFirstBackupKey, []byte("changed"))
	}
	if f.secondAttempt[i] {
		f.store.put(dest.Bucket, dir+"/"+testSecondAttempt+"/"+testFirstBackupKey, []byte("v"))
	}
	if f.strayObject[i] {
		f.store.put(dest.Bucket, dir+"/"+testAttempt+"/backup/stray", []byte("v"))
	}
	return artifact
}

// overlapOfS11 runs the fixture and returns S11 overlapping, requiring
// LAB01 and S11 complete passed.
func overlapOfS11(t *testing.T, f *s11OverlapFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	summary := readSummary(t, f.cfg.EvidenceDir)
	if summary.Lane != s11OverlappingLane || len(summary.Scenarios) != 3 || summary.Scenarios[2].ID != s11ID ||
		summary.Scenarios[2].Variant != s11OverlappingVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/overlapping in lane %s", summary,
			s11OverlappingLane)
	}
	for _, s := range summary.Scenarios[:2] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01 and S11 complete passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, summary.Scenarios[2], "S11/overlapping")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	f.requireForwardStopped(t)
	// Both backups are deleted by their UIDs, whatever the result.
	want := []string{}
	for i, b := range s11OverlappingBackups {
		if f.created[i] != nil {
			want = append(want, b.name+"/"+testOverlapUIDs[i])
		}
	}
	if !slices.Equal(f.deleted, want) {
		t.Fatalf("deleted = %v, want %v", f.deleted, want)
	}
	return summary.Scenarios[2], err
}

func readS11OverlapRecord(t *testing.T, dir string) s11OverlappingRecord {
	t.Helper()
	var rec s11OverlappingRecord
	data, err := os.ReadFile(filepath.Join(dir, s11OverlappingRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 overlapping record = %s, %v", data, err)
	}
	return rec
}

// requireS11OverlapResult fails unless S11 overlapping has result with every
// part of want in its reason.
func requireS11OverlapResult(t *testing.T, overlap evidence.Scenario, err error, result evidence.Result,
	want ...string) {
	t.Helper()
	if err == nil || overlap.Result != result {
		t.Fatalf("S11 overlapping = %+v, want %s", overlap, result)
	}
	for _, w := range want {
		if !strings.Contains(overlap.Reason, w) {
			t.Fatalf("S11 overlapping reason = %q, want %q", overlap.Reason, w)
		}
	}
}

// requireS11OverlapPassed checks a passed run's result and record.
func requireS11OverlapPassed(t *testing.T, f *s11OverlapFixture, overlap evidence.Scenario,
	err error) s11OverlappingRecord {
	t.Helper()
	if err != nil || overlap.Result != evidence.Pass {
		t.Fatalf("S11 overlapping = %+v, %v; want a pass", overlap, err)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,overlapping-cluster,ephemeral-container,"+
		"backup,backup" {
		t.Fatalf("writes = %v", f.writes)
	}
	m := overlap.Measurements
	if overlap.Limits["backup_limit"] != f.cfg.BackupLimit.String() ||
		overlap.Limits["readyLimit"] != f.cfg.BackupOverlappingReadyLimit.String() ||
		m["operationRefsDistinct"] != true || m["heldWithConflict"] != true ||
		m["uploadBytesThroughLimit"] == evidence.Unknown || m["uploadBytesThroughLimit"] == float64(0) {
		t.Fatalf("S11 overlapping limits = %v, measurements = %v", overlap.Limits, m)
	}
	for _, l := range []string{"a", "b"} {
		if m[l+".backupCompletedWithin"] == evidence.Unknown || m[l+".objectsVerified"] != float64(2) ||
			m[l+".attemptPrefixes"] != float64(1) || m[l+".objectsNotInManifest"] != float64(0) {
			t.Fatalf("S11 overlapping measurements = %v, want both artifacts verified", m)
		}
	}
	for _, rel := range []string{s11OverlappingRecordFile, s11OverlappingSourceDataCheckFile,
		s11OverlappingBackups[0].manifest, s11OverlappingBackups[1].manifest} {
		if !slices.Contains(overlap.Evidence, rel) {
			t.Fatalf("S11 overlapping evidence = %v, want %s", overlap.Evidence, rel)
		}
	}
	if overlap.CleanupSucceeded == nil || !*overlap.CleanupSucceeded {
		t.Fatalf("S11 overlapping cleanup = %v, want both backups deleted", overlap.CleanupSucceeded)
	}
	rec := readS11OverlapRecord(t, f.cfg.EvidenceDir)
	requireS11OverlapStandalone(t, f, rec)
	requireS11OverlapBackups(t, f, rec)
	if rec.Source == nil || !rec.Source.DataRulesHeld || rec.ManagerAfter == nil || rec.JudgedAt == nil ||
		rec.Overlap.NotContended != "" {
		t.Fatalf("record = %+v, overlap %+v; want the source checked and the overlap judged", rec, rec.Overlap)
	}
	requireNamespaceGone(t, f.haFixture)
	return rec
}

// requireS11OverlapStandalone checks the cluster and the limit of a passed
// run: the limit confirmed and removed, the cluster deleted by its UID after
// the backups, and nothing written to the object store.
func requireS11OverlapStandalone(t *testing.T, f *s11OverlapFixture, rec s11OverlappingRecord) {
	t.Helper()
	if f.net.throttleIP != "" || !slices.Equal(f.s11InterruptedFixture.deleted,
		[]string{testKindCluster + "/" + s11OverlappingClusterName + "/" + testInterruptedClusterUID}) {
		t.Fatalf("limit on %q, deleted %v; want the limit removed and the cluster deleted by its UID",
			f.net.throttleIP, f.s11InterruptedFixture.deleted)
	}
	if len(f.store.puts) != 0 || len(f.store.removes) != 0 {
		t.Fatalf("the run wrote %v and removed %v in the object store; want nothing", f.store.puts, f.store.removes)
	}
	if rec.Throttle == nil || rec.Throttle.LimitConfirmedAt == nil || rec.Throttle.RemovedAt == nil ||
		rec.Throttle.Last == nil || rec.Cluster == nil || rec.Cluster.UID != testInterruptedClusterUID ||
		rec.Credentials.StatusesChecked == 0 || rec.PodSecurity[podSecurityEnforce] != podSecurityPrivileged {
		t.Fatalf("record = %+v, want the limit confirmed and removed on a privileged namespace's cluster", rec)
	}
}

// requireS11OverlapBackups checks each backup's request and record in a
// passed run.
func requireS11OverlapBackups(t *testing.T, f *s11OverlapFixture, rec s11OverlappingRecord) {
	t.Helper()
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	for i, b := range rec.Backups {
		dest := f.created[i].Spec.Destination.ObjectStorage
		if dest == nil || dest.Bucket != testBackupBucket ||
			dest.Prefix != "lab-runs/"+lab01.RunID+"/"+s11OverlappingBackups[i].prefix ||
			f.created[i].Spec.ClusterRef.Name != s11OverlappingClusterName {
			t.Fatalf("backup = %+v, want the standalone cluster to the backup bucket under its own prefix",
				f.created[i])
		}
		if b.Artifact == nil || b.Verification == nil || b.Verification.ObjectsVerified != 2 ||
			b.Attempts == nil || len(b.Attempts.Prefixes) != 1 || b.Attempts.Manifests != 1 ||
			b.AttemptsAtEnd == nil || len(b.AttemptsAtEnd.Prefixes) != 1 || len(b.OperationRefsSeen) != 1 ||
			b.ManifestOperationID != b.OperationRefsSeen[0] || b.StatusStartedAt == nil || b.ManifestCreatedAt == nil ||
			!strings.Contains(b.Cleanup, "was deleted") {
			t.Fatalf("backup record = %+v, want its artifact verified under one attempt prefix", b)
		}
		// The manager's log and the target member's were searched for each.
		logs := []string{}
		for _, l := range b.Credentials.Logs {
			if l.Unchecked != "" || l.Found {
				t.Fatalf("log check = %+v, want it searched without a key", l)
			}
			logs = append(logs, l.Pod)
		}
		if !slices.Equal(logs, []string{testManagerPodName, lastTarget(b.Phases)}) {
			t.Fatalf("logs searched for %s = %v, want the manager's and the member's", b.Backup, logs)
		}
	}
}

// The second backup is held with OperationInProgress while the first
// uploads through the limit, and starts once it ended.
func TestRunS11OverlappingPassesWhenTheMemberSerializesTheBackups(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	overlap, err := overlapOfS11(t, f)
	rec := requireS11OverlapPassed(t, f, overlap, err)
	o := rec.Overlap
	if !o.HeldWithConflict || !slices.Equal(o.Started, []string{s11OverlappingBackups[0].name,
		s11OverlappingBackups[1].name}) || o.First != s11OverlappingBackups[0].name ||
		o.Second != s11OverlappingBackups[1].name || o.SecondStartedAt == nil || o.FirstManifestCreatedAt == nil ||
		o.FirstEndedAt == nil || o.SecondCreatedAt == nil || o.SecondCreatedAt.Before(*o.FirstEndedAt) ||
		overlap.Measurements["heldWithConflict"] != true {
		t.Fatalf("overlap = %+v, want the second held and its record created after the first's ended", o)
	}
	if !slices.Equal(f.recordReads, []string{testOverlapTarget + "/" + testOverlapOpA,
		testOverlapTarget + "/" + testOverlapOpB}) || rec.Backups[0].OperationRecord == nil ||
		rec.Backups[0].OperationRecord.UpdatedAt == nil || rec.Backups[1].OperationRecord == nil ||
		rec.Backups[1].OperationRecord.CreatedAt == nil {
		t.Fatalf("records read = %v, backups %+v; want both records read on the target", f.recordReads, rec.Backups)
	}
	b := rec.Backups[1]
	if len(b.Holds) == 0 || b.Holds[0].Reason != s11OperationInProgressReason ||
		b.Holds[0].TargetInstance != testOverlapTarget || b.Holds[0].OtherPhase == "" {
		t.Fatalf("holds = %+v, want the hold recorded with the other backup's phase", b.Holds)
	}
	reason := s11OverlappingPassReason(&rec)
	for _, want := range []string{"through the rate limit on " + testOverlapTarget,
		"s11-overlapping-b was held with " + s11OperationInProgressReason, testOverlapOpA + " and " + testOverlapOpB,
		"by the member's clock", "the limit was seen removed"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("S11 overlapping reason = %q, want %q", reason, want)
		}
	}
}

// The backup requested second may be started first: the held one ran
// second, whatever the order of the requests.
// A pass needs the order on the member judged: a backup without an
// operation, whose record cannot be named, leaves it unjudged.
func TestS11OverlappingRequiresTheOrderToBeJudged(t *testing.T) {
	h := &haRun{}
	rec := &s11OverlappingRecord{Overlap: &s11Overlap{}}
	read := [2]*databasev1alpha1.CubridBackup{{}, {}}
	read[0].Status.OperationRef = "op-a"
	err := h.orderJudged(context.Background(), read, rec, &evidence.Scenario{})
	if err == nil || !strings.Contains(err.Error(), "the order on the member was not judged") {
		t.Fatalf("orderJudged = %v, want the order not judged", err)
	}
}

func TestRunS11OverlappingOrdersTheBackupsByTheHold(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[0], f.steps[1] = []overlapStep{heldStep, heldStep, heldStep, heldStep,
		runningStep(testOverlapTarget, testOverlapOpA), doneStep(testOverlapTarget, testOverlapOpA)},
		[]overlapStep{pendingStep, runningStep(testOverlapTarget, testOverlapOpB),
			uploadStep(testOverlapTarget, testOverlapOpB), doneStep(testOverlapTarget, testOverlapOpB)}
	overlap, err := overlapOfS11(t, f)
	rec := requireS11OverlapPassed(t, f, overlap, err)
	if o := rec.Overlap; o.First != s11OverlappingBackups[1].name || o.Second != s11OverlappingBackups[0].name {
		t.Fatalf("overlap = %+v, want the held backup, requested first, ordered second", o)
	}
}

func TestRunS11OverlappingFailsOnASharedOperation(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[1] = []overlapStep{heldStep, heldStep, heldStep, heldStep, runningStep(testOverlapTarget, testOverlapOpA),
		doneStep(testOverlapTarget, testOverlapOpA)}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "s11-overlapping-a and s11-overlapping-b share "+
		"operation "+testOverlapOpA)
	if overlap.Measurements["operationRefsDistinct"] != false {
		t.Fatalf("measurements = %v, want the operations recorded as shared", overlap.Measurements)
	}
}

func TestRunS11OverlappingFailsOnASecondAttemptPrefix(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.secondAttempt[1] = true
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "s11-overlapping-b: a second writer: 2 attempt prefixes",
		testSecondAttempt)
}

// A manifest that names the other backup's operation must never be stored
// under a backup's prefix.
func TestRunS11OverlappingFailsOnTheManifestOfTheOtherBackup(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.tamperManifest = func(i int, m *instancemanager.BackupManifest) {
		if i == 1 {
			m.OperationID = testOverlapOpA
		}
	}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "the manifest of s11-overlapping-b names operation "+
		testOverlapOpA+", which is s11-overlapping-a's")
}

func TestRunS11OverlappingFailsWhenAnArtifactDoesNotVerify(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.corrupt[0] = true
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "the artifact of s11-overlapping-a does not match",
		testFirstBackupKey)
}

// The second's status.startedAt and the first's manifest createdAt are two
// clocks to the second: evidence, not a verdict.
func TestRunS11OverlappingRecordsTheOperatorClockAsEvidenceOnly(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.startedAt = func(i int, first time.Time) time.Time {
		if i == 1 {
			return first.Add(-time.Hour)
		}
		return first
	}
	overlap, err := overlapOfS11(t, f)
	rec := requireS11OverlapPassed(t, f, overlap, err)
	if o := rec.Overlap; o.SecondStartedAt == nil || o.FirstManifestCreatedAt == nil ||
		!o.SecondStartedAt.Before(*o.FirstManifestCreatedAt) {
		t.Fatalf("overlap = %+v, want the earlier startedAt recorded", o)
	}
}

// The second's operation record created before the first's
// was written as ended is two backups on one member at once, by the
// member's own clock and below the second.
func TestRunS11OverlappingFailsWhenTheTargetRanBothAtOnce(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.tamperRecord = func(i int, op *instancemanager.Operation) {
		if i == 1 {
			op.CreatedAt = f.doneAt[0].Add(-time.Millisecond)
		}
	}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "two backups ran on "+testOverlapTarget+" at once",
		"the record of s11-overlapping-b's operation "+testOverlapOpB+" was created at",
		"before the record of s11-overlapping-a's operation "+testOverlapOpA+" was written as ended")
}

// A record that cannot be read leaves the order on the member unknown.
func TestRunS11OverlappingIsBlockedWhenARecordCannotBeRead(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.recordErr[0] = errors.New("cat: no such file")
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Blocked, "the record of s11-overlapping-a's operation "+
		testOverlapOpA+" on "+testOverlapTarget+" could not be read", "cat: no such file")
	if rec := readS11OverlapRecord(t, f.cfg.EvidenceDir); rec.Backups[0].OperationRecord == nil ||
		rec.Backups[0].OperationRecord.ReadError == "" {
		t.Fatalf("backup = %+v, want the read error recorded", rec.Backups[0])
	}
}

// A first record that has not ended does not show when it ended.
func TestRunS11OverlappingIsBlockedWhenTheFirstRecordHasNotEnded(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.tamperRecord = func(i int, op *instancemanager.Operation) {
		if i == 0 {
			op.State = instancemanager.OpUploading
		}
	}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Blocked, "is Uploading, not ended")
}

// Every retry of a held start names its own request: one hold is recorded
// for each change of reason, target or the other backup's phase.
func TestRunS11OverlappingRecordsAHoldPerChange(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	running := runningStep(testOverlapTarget, testOverlapOpA)
	f.steps[0] = []overlapStep{pendingStep, running, running, running, uploadStep(testOverlapTarget, testOverlapOpA),
		doneStep(testOverlapTarget, testOverlapOpA)}
	f.steps[1] = []overlapStep{heldStep, heldStep, heldStep, heldStep, heldStep, heldStep,
		runningStep(testOverlapTarget, testOverlapOpB), doneStep(testOverlapTarget, testOverlapOpB)}
	overlap, err := overlapOfS11(t, f)
	rec := requireS11OverlapPassed(t, f, overlap, err)
	phases := make([]string, 0, len(rec.Backups[1].Holds))
	for _, h := range rec.Backups[1].Holds {
		phases = append(phases, h.OtherPhase)
	}
	if !slices.Equal(phases, []string{string(databasev1alpha1.BackupPhasePending),
		string(databasev1alpha1.BackupPhaseRunning), string(databasev1alpha1.BackupPhaseUploading),
		string(databasev1alpha1.BackupPhaseCompleted)}) {
		t.Fatalf("holds = %+v, want one for each phase of the other backup", rec.Backups[1].Holds)
	}
}

func TestRunS11OverlappingFailsOnAStagingHold(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	staging := heldStep
	staging.reason = s11StagingInUseReason
	f.steps[1] = []overlapStep{staging, staging, staging, staging, runningStep(testOverlapTarget, testOverlapOpB),
		doneStep(testOverlapTarget, testOverlapOpB)}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "s11-overlapping-b was held at",
		"another request owns its staging destination")
}

func TestRunS11OverlappingFailsWhenATargetIsReplaced(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[1] = []overlapStep{heldStep, heldStep, heldStep, heldStep, runningStep(testMemberLast, testOverlapOpB),
		doneStep(testMemberLast, testOverlapOpB)}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "s11-overlapping-b's target was "+testOverlapTarget+
		" (unknown), then "+testMemberLast+" (unknown): its recorded target was replaced",
		"not "+testOverlapTarget+", the Pod the limit acts in")
}

// Failed for the Instance Manager's own reason fails the variant, without a
// conflict's never-event.
func TestRunS11OverlappingFailsWhenABackupFailsForAnotherReason(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[1] = []overlapStep{heldStep, heldStep, heldStep, heldStep, runningStep(testOverlapTarget, testOverlapOpB),
		{phase: databasev1alpha1.BackupPhaseFailed, target: testOverlapTarget, op: testOverlapOpB,
			reason: s11MissingReason, message: testBackupdbFailed}}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "s11-overlapping-b ended Failed: "+s11MissingReason,
		testBackupdbFailed)
	if strings.Contains(overlap.Reason, "for a conflict or its staging") {
		t.Fatalf("S11 overlapping reason = %q, want no conflict never-event", overlap.Reason)
	}
}

// A limit not seen removed blocks the variant: the member's traffic may still
// be slowed, and the prefixes were listed through it.
func TestRunS11OverlappingIsBlockedByALimitLeftInstalled(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	// The limit stays after its removal, as a rule the command did not clear.
	f.net.onUnthrottle = func() { f.net.throttleIP, f.net.throttlePort = testStoreIP, testStorePort }
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Blocked, "the faults were not seen removed")
	if overlap.CleanupSucceeded == nil || *overlap.CleanupSucceeded {
		t.Fatalf("S11 overlapping = %+v, want the failed removal recorded", overlap)
	}
	if rec := readS11OverlapRecord(t, f.cfg.EvidenceDir); rec.Throttle.RemovedAt != nil ||
		!strings.Contains(rec.Cleanup, "not seen removed") {
		t.Fatalf("throttle = %+v, cleanup %q; want the limit left named", rec.Throttle, rec.Cleanup)
	}
}

// No upload is interrupted here: an object in a backup's own attempt prefix
// that its manifest does not name came from another writer.
func TestRunS11OverlappingFailsOnAnObjectTheManifestDoesNotName(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.strayObject[1] = true
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "s11-overlapping-b: objects in its own attempt prefix "+
		"that its manifest does not name", "backup/stray")
}

// The first ended before the second was accepted: the overlap was not
// exercised.
func TestRunS11OverlappingIsBlockedWhenTheBackupsNeverOverlap(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[0] = []overlapStep{runningStep(testOverlapTarget, testOverlapOpA), doneStep(testOverlapTarget, testOverlapOpA)}
	f.steps[1] = []overlapStep{pendingStep, pendingStep, runningStep(testOverlapTarget, testOverlapOpB),
		doneStep(testOverlapTarget, testOverlapOpB)}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Blocked, "no reading showed a backup held with "+
		s11OperationInProgressReason, "the overlap was not exercised")
	if rec := readS11OverlapRecord(t, f.cfg.EvidenceDir); rec.Overlap.NotContended == "" {
		t.Fatalf("overlap = %+v, want why it was not exercised", rec.Overlap)
	}
}

// The hold of a backup ends only after backupTargetWait, with
// RecordedTargetNotStarted: a never-event within backup_limit.
func TestRunS11OverlappingFailsWhenTheHeldBackupFails(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[1] = []overlapStep{heldStep, heldStep, heldStep, heldStep, {phase: databasev1alpha1.BackupPhaseFailed,
		target: testOverlapTarget, reason: s11TargetNotStartedReason, message: "the backup was not started on its " +
			"recorded target (last: " + s11OperationInProgressReason + ")"}}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "s11-overlapping-b ended Failed for a conflict or its "+
		"staging", s11TargetNotStartedReason)
	if rec := readS11OverlapRecord(t, f.cfg.EvidenceDir); !strings.Contains(rec.Backups[1].Failure,
		s11TargetNotStartedReason) {
		t.Fatalf("failure = %q, want the reason recorded", rec.Backups[1].Failure)
	}
}

// A conflict with the other backup's operation taken for one of the backup's
// own key must never happen.
func TestRunS11OverlappingFailsOnARequestConflict(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	conflict := heldStep
	conflict.reason = s11RequestConflictReason
	f.steps[1] = append([]overlapStep{conflict}, f.steps[1][1:]...)
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "as an operation of its own key with another request",
		s11RequestConflictReason)
}

func TestRunS11OverlappingIsBlockedWhenS11CompleteDidNotPass(t *testing.T) {
	cfg := testS11OverlapConfig(t)
	f := newS11OverlapFixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	summary := readSummary(t, cfg.EvidenceDir)
	if len(summary.Scenarios) != 3 || summary.Scenarios[2].Result != evidence.Blocked ||
		!strings.Contains(summary.Scenarios[2].Reason, "S11 complete did not pass") {
		t.Fatalf("summary = %+v, want S11 overlapping blocked by S11 complete", summary)
	}
	if f.created[0] != nil || f.created[1] != nil {
		t.Fatal("S11 overlapping ran although S11 complete did not pass")
	}
}

func TestRunHARefusesBackupOverlappingWithoutItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(cfg *HAConfig)
		want   string
	}{
		testNoBucket: {change: func(cfg *HAConfig) { cfg.BackupBucket = "" },
			want: "-backup-overlapping needs -backup-bucket"},
		testWithRestore: {change: func(cfg *HAConfig) {
			cfg.Restore, cfg.RestoreLimit = true, time.Minute
		}, want: "-backup-overlapping and " + flagRestore + " select"},
		testWithMissing: {change: func(cfg *HAConfig) {
			cfg.BackupMissingDestination = true
		}, want: "-backup-overlapping and " + flagMissingDestination + " select"},
		testWithDenied: {change: func(cfg *HAConfig) {
			cfg.BackupDeniedCredentials, cfg.BackupDeniedReadyLimit = true, time.Minute
		}, want: "-backup-overlapping and " + flagDeniedCredentials + " select"},
		"with the signature-mismatch variant": {change: func(cfg *HAConfig) {
			cfg.BackupSignatureMismatch, cfg.BackupSignatureMismatchReadyLimit = true, time.Minute
		}, want: flagSignatureMismatch + " and -backup-overlapping select"},
		testWithInterrupted: {change: func(cfg *HAConfig) {
			cfg.BackupInterruptedUpload, cfg.BackupInterruptedReadyLimit = true, time.Minute
		}, want: flagInterruptedUpload + " and -backup-overlapping select"},
		"with the operator-restart variant": {change: func(cfg *HAConfig) {
			cfg.BackupOperatorRestart, cfg.ResyncLimit, cfg.LeaderLease = true, time.Second, testLeaderLease
		}, want: flagOperatorRestart + " and -backup-overlapping select"},
		"with the instance-manager-restart variant": {change: func(cfg *HAConfig) {
			cfg.BackupInstanceManagerRestart, cfg.BackupInstanceManagerRestartReadyLimit = true, time.Minute
		}, want: flagInstanceManagerRestart + " and -backup-overlapping select"},
		"with a ready limit that is not positive": {change: func(cfg *HAConfig) { cfg.BackupOverlappingReadyLimit = 0 },
			want: "-backup-overlapping needs a positive -backup-overlapping-ready-limit"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11OverlapConfig(t)
			tc.change(&cfg)
			f := newS11OverlapFixture(t, testS11OverlapConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunHA() error = %v, want -backup-overlapping refused %s", err, name)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}
