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
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testRestartBackupUID = "restart-backup-uid"
	testRestartBackupOp  = "backup-op-1"
	// testSecondAttempt is an attempt prefix of the backup besides the
	// artifact's.
	testSecondAttempt = "attempt-ffeeddccbbaa99887766554433221100"
)

func testS11RestartConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupOperatorRestart = true
	cfg.ResyncLimit = time.Second
	cfg.LeaderLease = testLeaderLease
	return cfg
}

// s11RestartFixture plays, after S11 complete, an Operator that takes a
// second backup of the HA cluster, keeps it Running with its operation until
// the manager Pod is deleted, and completes it with an artifact under the
// variant's prefix; and a Deployment whose replacement takes the leader
// Lease.
type s11RestartFixture struct {
	*s11Fixture
	// restartPhases are the phases the fake Operator reports, one per
	// reading; the last is kept, and Running is kept until the manager Pod
	// was deleted. tamperRestart changes the status of a reading; deleted
	// tells whether the manager Pod was deleted before it. corruptObject
	// changes an object of the artifact after its manifest was written;
	// secondAttempt stores an object under a second attempt prefix of the
	// backup.
	restartPhases []databasev1alpha1.CubridBackupPhase
	tamperRestart func(read int, deleted bool, s *databasev1alpha1.CubridBackupStatus)
	corruptObject bool
	secondAttempt bool

	restartBackup             *databasev1alpha1.CubridBackup
	restartArtifact           *databasev1alpha1.CubridBackupArtifact
	restartReads, restartStep int
	// deleted are the backups the run deleted, with the UID it required.
	deleted []string
}

func newS11RestartFixture(t *testing.T, cfg HAConfig) *s11RestartFixture {
	t.Helper()
	holder := testManagerPodName + "_old"
	acquired := metav1.NewMicroTime(time.Now().Add(-time.Hour))
	f := &s11RestartFixture{s11Fixture: newS11Fixture(t, cfg, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: testLeaderLease, Namespace: cfg.OperatorNamespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, AcquireTime: &acquired}}),
		restartPhases: []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhasePending,
			databasev1alpha1.BackupPhaseRunning, databasev1alpha1.BackupPhaseUploading,
			databasev1alpha1.BackupPhaseCompleted}}
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.startReplacement(ctx, c)
		f.takeLease(ctx, c)
	}
	created, read := f.backupCreated, f.backupRead
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		if backup.Name != s11RestartBackupName {
			return created(ctx, c, backup)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		backup.UID = testRestartBackupUID
		f.restartBackup = backup.DeepCopy()
		return nil
	}
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name != s11RestartBackupName {
			return read(ctx, c, key)
		}
		return f.advanceRestart(ctx, c, key)
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
	f.markersOf = func(string) []string { return nil }
	return f
}

// advanceRestart writes the status of the second backup's next reading, as
// the Operator would: the operation is recorded with Running.
func (f *s11RestartFixture) advanceRestart(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	backup := &databasev1alpha1.CubridBackup{}
	if err := c.Get(ctx, key, backup); err != nil {
		return nil
	}
	f.mu.Lock()
	f.restartReads++
	read, deleted := f.restartReads, f.managerDeleted
	phase := f.restartPhases[min(f.restartStep, len(f.restartPhases)-1)]
	if phase != databasev1alpha1.BackupPhaseRunning || deleted {
		f.restartStep++
	}
	tamper := f.tamperRestart
	f.mu.Unlock()
	started := metav1.NewTime(time.Now().Add(-time.Second))
	s := &backup.Status
	s.Phase, s.ClusterUID, s.StartedAt = phase, testClusterUID, &started
	s.Conditions = []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse, Reason: testRunningReason,
		Message: testInProgress, LastTransitionTime: started}}
	if phase != databasev1alpha1.BackupPhasePending {
		s.TargetInstance, s.TargetRole = testBackupTarget, string(databasev1alpha1.RoleSlave)
		s.OperationRef = testRestartBackupOp
	}
	switch phase {
	case databasev1alpha1.BackupPhaseUploading:
		s.Conditions[0].Reason, s.Conditions[0].Message = testUploadRunning, testUploadingMessage
	case databasev1alpha1.BackupPhaseCompleted:
		s.Artifact = f.uploadRestart(backup)
		s.CompletedAt = &started
		s.Conditions[0].Status, s.Conditions[0].Reason, s.Conditions[0].Message = metav1.ConditionTrue,
			testCompleted, testCompletedMessage
	case databasev1alpha1.BackupPhaseFailed:
		s.CompletedAt = &started
		s.Conditions[0].Reason, s.Conditions[0].Message = s11MissingReason, testBackupdbFailed
	}
	if tamper != nil {
		tamper(read, deleted, s)
	}
	if err := c.Update(ctx, backup); err != nil {
		f.t.Error(err)
	}
	return nil
}

// uploadRestart writes the second backup's artifact once, as upload writes
// S11 complete's, and returns what the status reports of it; S11 complete's
// artifact is kept for its own readings.
func (f *s11RestartFixture) uploadRestart(
	backup *databasev1alpha1.CubridBackup) *databasev1alpha1.CubridBackupArtifact {
	f.mu.Lock()
	if f.restartArtifact != nil {
		defer f.mu.Unlock()
		return f.restartArtifact.DeepCopy()
	}
	saved := f.artifact
	f.artifact = nil
	f.mu.Unlock()
	artifact := f.upload(backup)
	f.mu.Lock()
	f.artifact, f.restartArtifact = saved, artifact.DeepCopy()
	f.mu.Unlock()
	dest := backup.Spec.Destination.ObjectStorage
	dir := path.Join(dest.Prefix, testClusterUID, databaseName, string(backup.UID))
	if f.corruptObject {
		f.store.put(dest.Bucket, dir+"/"+testAttempt+"/"+testFirstBackupKey, []byte("changed"))
	}
	if f.secondAttempt {
		f.store.put(dest.Bucket, dir+"/"+testSecondAttempt+"/"+testFirstBackupKey, []byte("v"))
	}
	return artifact
}

// restartOfS11 runs the fixture and returns S11 operator-restart, requiring
// LAB01 and S11 complete passed.
func restartOfS11(t *testing.T, f *s11RestartFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	summary := readSummary(t, f.cfg.EvidenceDir)
	if summary.Lane != s11RestartLane || len(summary.Scenarios) != 3 || summary.Scenarios[2].ID != s11ID ||
		summary.Scenarios[2].Variant != s11RestartVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/operator-restart in lane %s", summary,
			s11RestartLane)
	}
	for _, s := range summary.Scenarios[:2] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01 and S11 complete passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, summary.Scenarios[2], "S11/operator-restart")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	f.requireForwardStopped(t)
	return summary.Scenarios[2], err
}

func readS11RestartRecord(t *testing.T, dir string) s11RestartRecord {
	t.Helper()
	var rec s11RestartRecord
	data, err := os.ReadFile(filepath.Join(dir, s11RestartRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 operator-restart record = %s, %v", data, err)
	}
	return rec
}

// requireS11RestartResult fails unless S11 operator-restart has result with
// every part of want in its reason.
func requireS11RestartResult(t *testing.T, restart evidence.Scenario, err error, result evidence.Result,
	want ...string) {
	t.Helper()
	if err == nil || restart.Result != result {
		t.Fatalf("S11 operator-restart = %+v, want %s", restart, result)
	}
	for _, w := range want {
		if !strings.Contains(restart.Reason, w) {
			t.Fatalf("S11 operator-restart reason = %q, want %q", restart.Reason, w)
		}
	}
}

func TestRunS11OperatorRestartPassesWhenTheBackupSurvivesTheRestart(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	restart, err := restartOfS11(t, f)
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if summary := readSummary(t, cfg.EvidenceDir); !summary.Passed || restart.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want every scenario passed", summary)
	}
	if f.managerDeletes != 1 {
		t.Fatalf("manager Pod deleted %d times, want once", f.managerDeletes)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,backup" {
		t.Fatalf("writes = %v", f.writes)
	}
	m := restart.Measurements
	if restart.Limits["backup_limit"] != cfg.BackupLimit.String() ||
		restart.Limits["operator_resync_limit"] != cfg.ResyncLimit.String() ||
		m["backupCompletedWithin"] == evidence.Unknown || m["operatorBackWithin"] == evidence.Unknown ||
		m["operationRefUnchanged"] != true || m["runningAtRestart"] != true || m["objectsVerified"] != float64(2) ||
		m["attemptPrefixes"] != float64(1) || m["objectsNotInManifest"] != float64(0) {
		t.Fatalf("S11 operator-restart limits = %v, measurements = %v", restart.Limits, m)
	}
	for _, rel := range []string{s11RestartRecordFile, s11RestartTimelineFile, s11RestartManifestFile,
		s11RestartSourceDataCheckFile} {
		if !slices.Contains(restart.Evidence, rel) {
			t.Fatalf("S11 operator-restart evidence = %v, want %s", restart.Evidence, rel)
		}
	}
	if restart.CleanupSucceeded == nil || !*restart.CleanupSucceeded ||
		!slices.Equal(f.deleted, []string{s11RestartBackupName + "/" + testRestartBackupUID}) {
		t.Fatalf("deleted = %v, want only the backup the variant created, by its UID", f.deleted)
	}
	// The run writes nothing to the object store and leaves the artifact.
	if len(f.store.puts) != 0 || len(f.store.removes) != 0 {
		t.Fatalf("the run wrote %v and removed %v in the object store; want nothing", f.store.puts, f.store.removes)
	}
	dest := f.restartBackup.Spec.Destination.ObjectStorage
	lab01 := readHARecord(t, cfg.EvidenceDir)
	if dest == nil || dest.Bucket != testBackupBucket || dest.Prefix != "lab-runs/"+lab01.RunID+"/operator-restart" ||
		f.restartBackup.Spec.ClusterRef.Name != haClusterName {
		t.Fatalf("backup = %+v, want the HA cluster to the backup bucket under the variant's prefix", f.restartBackup)
	}
	requireS11RestartRecord(t, f)
	reason := s11RestartPassReason(readS11RestartRecord(t, cfg.EvidenceDir).Restart)
	for _, want := range []string{"Running with operation " + testRestartBackupOp + " on " + testBackupTarget,
		"right before the deletion showed it Running", "exactly one attempt prefix"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("S11 operator-restart reason = %q, want %q", reason, want)
		}
	}
	requireNamespaceGone(t, f.haFixture)
}

// requireS11RestartRecord checks the record of a passed run.
func requireS11RestartRecord(t *testing.T, f *s11RestartFixture) {
	t.Helper()
	rec := readS11RestartRecord(t, f.cfg.EvidenceDir)
	requireS11RestartPlaced(t, rec.Restart)
	if rec.Artifact == nil || rec.Verification == nil || rec.Verification.ObjectsVerified != 2 ||
		rec.Attempts == nil || len(rec.Attempts.Prefixes) != 1 || rec.Attempts.Manifests != 1 ||
		len(rec.Attempts.Outside) != 0 || rec.ManagerAfter == nil || len(rec.ManagerAfter.Pods) != 1 ||
		rec.ManagerAfter.Pods[0].UID != testReplacementPod || rec.Source == nil || !rec.Source.DataRulesHeld ||
		rec.JudgedAt == nil {
		t.Fatalf("record = %+v, want the artifact verified, one attempt prefix and the replacement at the end", rec)
	}
	// The deleted manager's log was searched before its deletion, the
	// replacement's and the target member's at the end.
	logs := make([]string, 0, len(rec.Credentials.Logs))
	for _, l := range rec.Credentials.Logs {
		if l.Unchecked != "" || l.Found {
			t.Fatalf("log check = %+v, want it searched without a key", l)
		}
		logs = append(logs, l.Pod)
	}
	if !slices.Equal(logs, []string{testManagerPodName, testReplacementPod, testBackupTarget}) {
		t.Fatalf("logs searched = %v, want the deleted manager's, the replacement's and the member's", logs)
	}
}

// requireS11RestartPlaced checks the restart of a passed run.
func requireS11RestartPlaced(t *testing.T, r *s11Restart) {
	t.Helper()
	if r == nil || r.PlacedAt == nil || r.RestartedAt == nil || r.PlacedAt.After(*r.RestartedAt) ||
		r.OperationRef != testRestartBackupOp || r.TargetInstance != testBackupTarget || r.PhaseAtRestart != "Running" ||
		!r.RunningAtRestart || r.ManagerAfter == nil || r.ManagerAfter.UID != testReplacementPod ||
		r.ManagerBefore.UID != testManagerPodName || r.LeaseAcquiredAt == nil || r.NotPlaced != "" ||
		!slices.Equal(r.OperationRefsSeen, []string{testRestartBackupOp}) {
		t.Fatalf("restart = %+v, want the deletion placed while the backup ran and the replacement back", r)
	}
}

// A backup that completes before any reading shows it Running with an
// operation cannot have the restart placed during it.
func TestRunS11OperatorRestartIsBlockedWhenTheBackupEndsBeforeItRuns(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	f.restartPhases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhasePending,
		databasev1alpha1.BackupPhaseCompleted}
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Blocked, "the restart could not be placed while the backup ran",
		"before one showed it Completed")
	if f.managerDeleted {
		t.Fatal("the manager Pod was deleted although the restart could not be placed")
	}
	if rec := readS11RestartRecord(t, cfg.EvidenceDir); rec.Restart.NotPlaced == "" || rec.Restart.PlacedAt != nil {
		t.Fatalf("restart = %+v, want why it was not placed", rec.Restart)
	}
}

// A backup that ends between the placing reading and the deletion is not
// restarted either.
func TestRunS11OperatorRestartIsBlockedWhenTheBackupEndsBeforeTheDeletion(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	f.tamperRestart = func(read int, _ bool, s *databasev1alpha1.CubridBackupStatus) {
		if read == 3 {
			s.Phase = databasev1alpha1.BackupPhaseFailed
		}
	}
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Blocked, "ended Failed", "before the manager Pod was deleted")
	if f.managerDeleted {
		t.Fatal("the manager Pod was deleted after the backup ended")
	}
}

func TestRunS11OperatorRestartFailsWhenTheOperationChanges(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	f.tamperRestart = func(_ int, deleted bool, s *databasev1alpha1.CubridBackupStatus) {
		if deleted {
			s.OperationRef = "backup-op-2"
		}
	}
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Fail, "the backup's operationRef was "+testRestartBackupOp+
		", then \"backup-op-2\"", "a second operation")
	if restart.Measurements["operationRefUnchanged"] != false {
		t.Fatalf("measurements = %v, want the operation recorded as changed", restart.Measurements)
	}
}

func TestRunS11OperatorRestartFailsWhenTheTargetChanges(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	f.tamperRestart = func(_ int, deleted bool, s *databasev1alpha1.CubridBackupStatus) {
		if deleted {
			s.TargetInstance = testMemberLast
		}
	}
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Fail, "the backup's target was "+testBackupTarget)
}

func TestRunS11OperatorRestartFailsOnASecondAttemptPrefix(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	f.secondAttempt = true
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Fail, "a second writer: 2 attempt prefixes",
		testSecondAttempt)
}

func TestRunS11OperatorRestartFailsWhenTheArtifactDoesNotVerify(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	f.corruptObject = true
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Fail, "the artifact does not match", testFirstBackupKey)
}

// As in S12 operator-restart, a replacement that is not back fails the
// variant.
func TestRunS11OperatorRestartFailsWhenTheReplacementIsNotBack(t *testing.T) {
	cfg := testS11RestartConfig(t)
	cfg.ResyncLimit, cfg.BackupLimit = 100*time.Millisecond, 10*time.Second
	f := newS11RestartFixture(t, cfg)
	// The replacement starts but never takes the Lease.
	f.replacement = func(ctx context.Context, c client.WithWatch) { f.startReplacement(ctx, c) }
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Fail, "the manager was not back after its Pod was deleted")
}

func TestRunS11OperatorRestartFailsWhenTheReplacementIsLate(t *testing.T) {
	cfg := testS11RestartConfig(t)
	cfg.ResyncLimit = time.Nanosecond
	f := newS11RestartFixture(t, cfg)
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Fail, "over operator_resync_limit")
}

func TestRunS11OperatorRestartFailsWhenTheBackupFails(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	f.restartPhases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhasePending,
		databasev1alpha1.BackupPhaseRunning, databasev1alpha1.BackupPhaseFailed}
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Fail, "the backup ended Failed after the manager restarted",
		"backupdb failed")
	if rec := readS11RestartRecord(t, cfg.EvidenceDir); !strings.Contains(rec.Failure, "backupdb failed") {
		t.Fatalf("failure = %q, want the reason recorded", rec.Failure)
	}
}

// The documented escalation of an unknown outcome is not a pass, and not a
// failure of the restart either.
func TestRunS11OperatorRestartIsBlockedByTheDocumentedEscalation(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	f.restartPhases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhasePending,
		databasev1alpha1.BackupPhaseRunning, databasev1alpha1.BackupPhaseFailed}
	f.tamperRestart = func(_ int, _ bool, s *databasev1alpha1.CubridBackupStatus) {
		if s.Phase == databasev1alpha1.BackupPhaseFailed {
			s.Conditions[0].Reason = s11OperationLostReason
		}
	}
	restart, err := restartOfS11(t, f)
	requireS11RestartResult(t, restart, err, evidence.Blocked, "the documented escalation "+s11OperationLostReason)
}

func TestRunS11OperatorRestartIsBlockedWhenS11CompleteDidNotPass(t *testing.T) {
	cfg := testS11RestartConfig(t)
	f := newS11RestartFixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	summary := readSummary(t, cfg.EvidenceDir)
	if len(summary.Scenarios) != 3 || summary.Scenarios[2].Result != evidence.Blocked ||
		!strings.Contains(summary.Scenarios[2].Reason, "S11 complete did not pass") {
		t.Fatalf("summary = %+v, want S11 operator-restart blocked by S11 complete", summary)
	}
	if f.restartBackup != nil || f.managerDeleted {
		t.Fatal("S11 operator-restart ran although S11 complete did not pass")
	}
}

func TestRunHARefusesBackupOperatorRestartWithoutItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(cfg *HAConfig)
		want   string
	}{
		testNoBucket: {change: func(cfg *HAConfig) { cfg.BackupBucket = "" },
			want: "-backup-operator-restart needs -backup-bucket"},
		testWithRestore: {change: func(cfg *HAConfig) {
			cfg.Restore, cfg.RestoreLimit = true, time.Minute
		}, want: testRestoreFlag},
		"with the missing-destination variant": {change: func(cfg *HAConfig) {
			cfg.BackupMissingDestination = true
		}, want: "-backup-operator-restart and " + flagMissingDestination + " select"},
		"with the denied-credentials variant": {change: func(cfg *HAConfig) {
			cfg.BackupDeniedCredentials, cfg.BackupDeniedReadyLimit = true, time.Minute
		}, want: "-backup-operator-restart and " + flagDeniedCredentials + " select"},
		"with the signature-mismatch variant": {change: func(cfg *HAConfig) {
			cfg.BackupSignatureMismatch, cfg.BackupSignatureMismatchReadyLimit = true, time.Minute
		}, want: "-backup-signature-mismatch and -backup-operator-restart select"},
		"with the interrupted-upload variant": {change: func(cfg *HAConfig) {
			cfg.BackupInterruptedUpload, cfg.BackupInterruptedReadyLimit = true, time.Minute
		}, want: "-backup-interrupted-upload and -backup-operator-restart select"},
		"without a resync limit": {change: func(cfg *HAConfig) { cfg.ResyncLimit = 0 },
			want: "-resync-limit"},
		"without the leader Lease": {change: func(cfg *HAConfig) { cfg.LeaderLease = "" },
			want: "-leader-lease"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11RestartConfig(t)
			tc.change(&cfg)
			f := newS11RestartFixture(t, testS11RestartConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "-backup-operator-restart") {
				t.Fatalf("RunHA() error = %v, want -backup-operator-restart refused %s", err, name)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}
