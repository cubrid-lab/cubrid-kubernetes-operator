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
	heldStep    = overlapStep{phase: databasev1alpha1.BackupPhasePending, target: testBackupTarget,
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

// s11OverlapFixture plays, after S11 complete, an Operator that takes the two
// backups of S11 overlapping as steps say, one step per reading of each; the
// last step is kept. By default both run on one target: the first runs and
// completes while the second is held with OperationInProgress, then the
// second runs and completes.
type s11OverlapFixture struct {
	*s11Fixture
	steps [2][]overlapStep
	// tamperManifest changes the manifest of backup i before it is stored;
	// startedAt is status.startedAt of backup i, given when its operation was
	// first reported, which it defaults to. corrupt changes an object of
	// backup i's artifact after its manifest was written; secondAttempt
	// stores an object under a second attempt prefix of backup i.
	tamperManifest func(i int, m *instancemanager.BackupManifest)
	startedAt      func(i int, first time.Time) time.Time
	corrupt        [2]bool
	secondAttempt  [2]bool

	created   [2]*databasev1alpha1.CubridBackup
	reads     [2]int
	firstOp   [2]time.Time
	artifacts [2]*databasev1alpha1.CubridBackupArtifact
	// deleted are the backups the run deleted, with the UID it required.
	deleted []string
}

func testS11OverlapConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupOverlapping = true
	return cfg
}

func newS11OverlapFixture(t *testing.T, cfg HAConfig) *s11OverlapFixture {
	t.Helper()
	f := &s11OverlapFixture{s11Fixture: newS11Fixture(t, cfg)}
	f.steps = [2][]overlapStep{
		{pendingStep, runningStep(testBackupTarget, testOverlapOpA), uploadStep(testBackupTarget, testOverlapOpA),
			doneStep(testBackupTarget, testOverlapOpA)},
		{heldStep, heldStep, heldStep, heldStep, runningStep(testBackupTarget, testOverlapOpB),
			doneStep(testBackupTarget, testOverlapOpB)}}
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
	return f
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
	first, startedAt := f.firstOp[i], f.startedAt
	f.mu.Unlock()
	now := metav1.Now()
	s := &backup.Status
	s.Phase, s.ClusterUID, s.OperationRef = st.phase, testClusterUID, st.op
	s.TargetInstance, s.TargetRole = st.target, ""
	if st.target != "" {
		s.TargetRole = string(databasev1alpha1.RoleSlave)
	}
	s.Conditions = []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse,
		Reason: cmp.Or(st.reason, testRunningReason), Message: cmp.Or(st.message, testInProgress),
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
	saved, savedTamper, tamper := f.artifact, f.tamper, f.tamperManifest
	f.artifact = nil
	f.tamper = func(a *fakeArtifact) {
		a.manifest.OperationID, a.manifest.SourceInstance = st.op, st.target
		if tamper != nil {
			tamper(i, &a.manifest)
		}
	}
	f.mu.Unlock()
	artifact := f.upload(backup)
	f.mu.Lock()
	f.artifact, f.tamper, f.artifacts[i] = saved, savedTamper, artifact.DeepCopy()
	f.mu.Unlock()
	dest := backup.Spec.Destination.ObjectStorage
	dir := path.Join(dest.Prefix, testClusterUID, databaseName, string(backup.UID))
	if f.corrupt[i] {
		f.store.put(dest.Bucket, dir+"/"+testAttempt+"/"+testFirstBackupKey, []byte("changed"))
	}
	if f.secondAttempt[i] {
		f.store.put(dest.Bucket, dir+"/"+testSecondAttempt+"/"+testFirstBackupKey, []byte("v"))
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
func requireS11OverlapPassed(t *testing.T, f *s11OverlapFixture, overlap evidence.Scenario, err error,
	sameTarget bool) s11OverlappingRecord {
	t.Helper()
	if err != nil || overlap.Result != evidence.Pass {
		t.Fatalf("S11 overlapping = %+v, %v; want a pass", overlap, err)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,backup,backup" {
		t.Fatalf("writes = %v", f.writes)
	}
	m := overlap.Measurements
	if overlap.Limits["backup_limit"] != f.cfg.BackupLimit.String() || m["sameTarget"] != sameTarget ||
		m["operationRefsDistinct"] != true {
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
	// The run writes nothing to the object store and leaves the artifacts.
	if len(f.store.puts) != 0 || len(f.store.removes) != 0 {
		t.Fatalf("the run wrote %v and removed %v in the object store; want nothing", f.store.puts, f.store.removes)
	}
	rec := readS11OverlapRecord(t, f.cfg.EvidenceDir)
	requireS11OverlapBackups(t, f, rec)
	if rec.Source == nil || !rec.Source.DataRulesHeld || rec.ManagerAfter == nil || rec.JudgedAt == nil ||
		rec.Overlap.SameTarget == nil || *rec.Overlap.SameTarget != sameTarget || rec.Overlap.NotContended != "" {
		t.Fatalf("record = %+v, overlap %+v; want the source checked and the overlap judged", rec, rec.Overlap)
	}
	requireNamespaceGone(t, f.haFixture)
	return rec
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
			f.created[i].Spec.ClusterRef.Name != haClusterName {
			t.Fatalf("backup = %+v, want the HA cluster to the backup bucket under its own prefix", f.created[i])
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
			t.Fatalf("logs searched for %s = %v, want the manager's and its target's", b.Backup, logs)
		}
	}
}

// On one target the second backup is held with OperationInProgress while the
// first runs, and starts once it ended.
func TestRunS11OverlappingPassesWhenOneTargetSerializesTheBackups(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	overlap, err := overlapOfS11(t, f)
	rec := requireS11OverlapPassed(t, f, overlap, err, true)
	o := rec.Overlap
	if !o.HeldWithConflict || !slices.Equal(o.Started, []string{s11OverlappingBackups[0].name,
		s11OverlappingBackups[1].name}) || o.SecondStartedAt == nil || o.FirstManifestCreatedAt == nil ||
		overlap.Measurements["heldWithConflict"] != true {
		t.Fatalf("overlap = %+v, want the second held and started after the first's upload began", o)
	}
	b := rec.Backups[1]
	if len(b.Holds) == 0 || b.Holds[0].Reason != s11OperationInProgressReason ||
		b.Holds[0].TargetInstance != testBackupTarget || b.Holds[0].OtherPhase == "" {
		t.Fatalf("holds = %+v, want the hold recorded with the other backup's phase", b.Holds)
	}
	reason := s11OverlappingPassReason(&rec)
	for _, want := range []string{"they had the same target, " + testBackupTarget,
		"held with " + s11OperationInProgressReason, testOverlapOpA + " and " + testOverlapOpB} {
		if !strings.Contains(reason, want) {
			t.Fatalf("S11 overlapping reason = %q, want %q", reason, want)
		}
	}
}

// Two eligible standbys may be ranked differently by the two selections:
// the backups then run at once on different targets.
func TestRunS11OverlappingPassesOnTwoTargets(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[1] = []overlapStep{pendingStep, runningStep(testMemberLast, testOverlapOpB),
		uploadStep(testMemberLast, testOverlapOpB), doneStep(testMemberLast, testOverlapOpB)}
	overlap, err := overlapOfS11(t, f)
	rec := requireS11OverlapPassed(t, f, overlap, err, false)
	if rec.Overlap.BothInProgressFrom == nil || rec.Overlap.HeldWithConflict ||
		overlap.Measurements["bothInProgressInReadings"] != true ||
		!strings.Contains(s11OverlappingPassReason(&rec), "different targets, "+testBackupTarget+" and "+
			testMemberLast) {
		t.Fatalf("S11 overlapping = %+v, overlap %+v; want both seen in progress at once", overlap, rec.Overlap)
	}
}

func TestRunS11OverlappingFailsOnASharedOperation(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[1] = []overlapStep{heldStep, heldStep, heldStep, heldStep, runningStep(testBackupTarget, testOverlapOpA),
		doneStep(testBackupTarget, testOverlapOpA)}
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

// On one target, a second backup started before the first's upload began ran
// beside it.
func TestRunS11OverlappingFailsWhenOneTargetRanBothAtOnce(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.startedAt = func(i int, first time.Time) time.Time {
		if i == 1 {
			return first.Add(-time.Hour)
		}
		return first
	}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Fail, "two backups ran on "+testBackupTarget+" at once",
		"s11-overlapping-b was started at", "before s11-overlapping-a's upload began")
}

// The first ended before the second was accepted: the overlap was not
// exercised.
func TestRunS11OverlappingIsBlockedWhenTheBackupsNeverOverlap(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[0] = []overlapStep{runningStep(testBackupTarget, testOverlapOpA), doneStep(testBackupTarget, testOverlapOpA)}
	f.steps[1] = []overlapStep{pendingStep, pendingStep, runningStep(testBackupTarget, testOverlapOpB),
		doneStep(testBackupTarget, testOverlapOpB)}
	overlap, err := overlapOfS11(t, f)
	requireS11OverlapResult(t, overlap, err, evidence.Blocked, "the readings never showed the two backups contend")
	if rec := readS11OverlapRecord(t, f.cfg.EvidenceDir); rec.Overlap.NotContended == "" {
		t.Fatalf("overlap = %+v, want why it was not exercised", rec.Overlap)
	}
}

// The hold of a backup ends only after backupTargetWait, with
// RecordedTargetNotStarted: a never-event within backup_limit.
func TestRunS11OverlappingFailsWhenTheHeldBackupFails(t *testing.T) {
	f := newS11OverlapFixture(t, testS11OverlapConfig(t))
	f.steps[1] = []overlapStep{heldStep, heldStep, heldStep, heldStep, {phase: databasev1alpha1.BackupPhaseFailed,
		target: testBackupTarget, reason: s11TargetNotStartedReason, message: "the backup was not started on its " +
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
