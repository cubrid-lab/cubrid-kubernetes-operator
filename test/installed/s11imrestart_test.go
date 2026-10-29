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
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testIMBackupUID = "imrestart-backup-uid"
	// testIMContainerID is the database container of a standalone member
	// before any restart.
	testIMContainerID = "containerd://db-1"
	// testIMOperation and testIMOperation2 are the backup's operation and
	// another one.
	testIMOperation  = "op-00112233445566778899aabbccddeeff"
	testIMOperation2 = "op-ffeeddccbbaa99887766554433221100"
)

func testS11IMRestartConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupInstanceManagerRestart = true
	cfg.BackupInstanceManagerRestartReadyLimit = 5 * time.Second
	return cfg
}

// s11IMRestartFixture plays, after S11 complete, an Operator that forms the
// standalone cluster s11-imrestart and takes its backup, a member whose
// upload passes through its network namespace, a kubelet that starts the
// database container again when its Instance Manager is killed, and the
// object store. While the backup is Uploading before the kill, every reading
// puts uploadPerRead bytes through the limit. After the kill, the first
// readings stay Uploading (the Operator cannot reach the manager while it
// restarts), and then the backup ends as afterKill, Failed by default with
// the restarted manager's reason.
type s11IMRestartFixture struct {
	*s11InterruptedFixture
	// afterKill is the phase the backup ends in after the kill, and
	// failMessage the message of a Failed one, s12IMRestartedReason unless
	// set; uploadingAfterKill is how many readings after the kill stay
	// Uploading.
	afterKill          databasev1alpha1.CubridBackupPhase
	failMessage        string
	uploadingAfterKill int
	// refuse answers the kill as the script does when it finds no manager;
	// exitCode is the killed container's, 137 unless set; extraRestarts
	// adds restarts after the kill.
	refuse        bool
	exitCode      int32
	extraRestarts int32

	imBackup      *databasev1alpha1.CubridBackup
	imReads       int
	killed        bool
	kills         []string
	afterReads    int
	failedSeen    bool
	limitAtFailed bool
}

func newS11IMRestartFixture(t *testing.T, cfg HAConfig) *s11IMRestartFixture {
	t.Helper()
	f := &s11IMRestartFixture{s11InterruptedFixture: newS11InterruptedFixture(t, cfg),
		afterKill: databasev1alpha1.BackupPhaseFailed, uploadingAfterKill: 1}
	f.net.member = s11IMRestartMember
	created, read := f.backupCreated, f.backupRead
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		if backup.Name != s11IMRestartBackupName {
			return created(ctx, c, backup)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		backup.UID = testIMBackupUID
		f.imBackup = backup.DeepCopy()
		return nil
	}
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name != s11IMRestartBackupName {
			return read(ctx, c, key)
		}
		return f.advanceIMBackup(ctx, c, key)
	}
	f.memberHook = f.kill
	return f
}

// kill plays the kubelet when the run kills the member's Instance Manager:
// the database container ends with exitCode and runs again, ready, on the
// same Pod.
func (f *s11IMRestartFixture) kill(ctx context.Context, pod, script string) (string, error, bool) {
	if !strings.Contains(script, "kill -KILL") {
		return "", nil, false
	}
	if script != imKillScript {
		f.t.Errorf("kill script = %q, want imKillScript", script)
	}
	f.mu.Lock()
	f.kills = append(f.kills, pod)
	refuse := f.refuse
	f.mu.Unlock()
	if refuse {
		return "", errors.New(imKillRefused + " none\ncommand terminated with exit code 3"), true
	}
	c := f.haFixture.store
	current := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: pod}, current); err != nil {
		f.t.Error(err)
		return "", err, true
	}
	finished := metav1.NewTime(time.Now().Truncate(time.Second))
	status := &current.Status.ContainerStatuses[0]
	old := status.ContainerID
	status.RestartCount += 1 + f.extraRestarts
	status.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: cmp.Or(f.exitCode, sigkillExitCode), Reason: "Error", ContainerID: old, FinishedAt: finished}}
	status.ContainerID, status.Ready = old+"-restarted", true
	status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: finished}}
	if err := c.Status().Update(ctx, current); err != nil {
		f.t.Error(err)
	}
	f.mu.Lock()
	f.killed = true
	f.mu.Unlock()
	return "", errors.New("command terminated with exit code 137"), true
}

// nextIM is the phase of the backup at this reading. It runs with f.mu held.
func (f *s11IMRestartFixture) nextIM() databasev1alpha1.CubridBackupPhase {
	if f.final != "" {
		return f.final
	}
	f.imReads++
	switch {
	case f.imReads == 1:
		return databasev1alpha1.BackupPhasePending
	case f.imReads == 2:
		return databasev1alpha1.BackupPhaseRunning
	case f.killed:
		f.afterReads++
		if f.afterReads <= f.uploadingAfterKill {
			return databasev1alpha1.BackupPhaseUploading
		}
		f.final = f.afterKill
		return f.final
	}
	n := f.net
	n.mu.Lock()
	if n.throttleIP != "" {
		n.send(n.throttleIP, f.uploadPerRead)
	}
	n.mu.Unlock()
	f.uploading++
	if f.completeAfter > 0 && f.uploading >= f.completeAfter {
		f.final = databasev1alpha1.BackupPhaseCompleted
		return f.final
	}
	return databasev1alpha1.BackupPhaseUploading
}

// advanceIMBackup writes the status of the backup's phase at this reading.
func (f *s11IMRestartFixture) advanceIMBackup(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	backup := &databasev1alpha1.CubridBackup{}
	if err := c.Get(ctx, key, backup); err != nil {
		return nil
	}
	f.mu.Lock()
	phase := f.nextIM()
	killed, tamper, message := f.killed, f.tamper, cmp.Or(f.failMessage, s12IMRestartedReason)
	f.mu.Unlock()
	started := metav1.NewTime(time.Now().Add(-time.Second))
	dest := backup.Spec.Destination.ObjectStorage
	attempt := path.Join(dest.Prefix, testAttempt)
	s := &backup.Status
	s.Phase, s.ClusterUID, s.StartedAt = phase, testInterruptedClusterUID, &started
	s.Conditions = []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse,
		Reason: testRunningReason, Message: testInProgress, LastTransitionTime: started}}
	if phase != databasev1alpha1.BackupPhasePending {
		s.TargetInstance, s.TargetRole = s11IMRestartMember, string(databasev1alpha1.RoleUnknown)
		s.OperationRef = testIMOperation
	}
	switch phase {
	case databasev1alpha1.BackupPhaseUploading:
		s.Conditions[0].Reason, s.Conditions[0].Message = testUploadRunning, testUploadingMessage
		if killed {
			s.Conditions[0].Reason, s.Conditions[0].Message = "InstanceManagerUnavailable", testIMRefused
		}
	case databasev1alpha1.BackupPhaseFailed:
		s.CompletedAt = &started
		s.Conditions[0].Reason, s.Conditions[0].Message = s11MissingReason, message
		for _, key := range f.leave {
			f.store.put(dest.Bucket, attempt+"/"+key, []byte("partial"))
		}
		// At the first reading of Failed only: the backup is read again
		// after the limit was removed.
		f.net.mu.Lock()
		limited := f.net.throttleIP != ""
		f.net.mu.Unlock()
		f.mu.Lock()
		if !f.failedSeen {
			f.failedSeen, f.limitAtFailed = true, limited
		}
		f.mu.Unlock()
	case databasev1alpha1.BackupPhaseCompleted:
		s.CompletedAt = &started
		s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: "s3://" + dest.Bucket + "/" + attempt +
			"/manifest.json", ManifestDigest: strings.Repeat("a", 64), SizeBytes: 1, Database: databaseName,
			Level: databasev1alpha1.CubridBackupFull}
		s.Conditions[0].Status, s.Conditions[0].Reason, s.Conditions[0].Message = metav1.ConditionTrue,
			testCompleted, testCompletedMessage
		f.store.put(dest.Bucket, attempt+"/"+testFirstBackupKey, []byte("v"))
		f.store.put(dest.Bucket, attempt+"/manifest.json", []byte("{}"))
	}
	if tamper != nil {
		tamper(s)
	}
	return c.Update(ctx, backup)
}

// s11IMRestartOf runs the fixture and returns S11 instance-manager-restart,
// requiring LAB01 and S11 complete passed.
func s11IMRestartOf(t *testing.T, f *s11IMRestartFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	scenarios := s11IMRestartScenarios(t, f.cfg.EvidenceDir)
	for _, s := range scenarios[:2] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01 and S11 complete passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, scenarios[2], "S11/"+s11IMRestartVariant)
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	f.requireForwardStopped(t)
	return scenarios[2], err
}

func s11IMRestartScenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s11IMRestartLane || len(summary.Scenarios) != 3 || summary.Scenarios[2].ID != s11ID ||
		summary.Scenarios[2].Variant != s11IMRestartVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/instance-manager-restart in lane %s", summary,
			s11IMRestartLane)
	}
	return summary.Scenarios
}

func readS11IMRestartRecord(t *testing.T, dir string) s11IMRestartRecord {
	t.Helper()
	var rec s11IMRestartRecord
	data, err := os.ReadFile(filepath.Join(dir, s11IMRestartRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 instance-manager-restart record = %s, %v", data, err)
	}
	return rec
}

// wantIMRestartDeleted are the deletions of a run that created both objects.
var wantIMRestartDeleted = []string{"CubridBackup/" + s11IMRestartBackupName + "/" + testIMBackupUID,
	testKindCluster + "/" + s11IMRestartClusterName + "/" + testInterruptedClusterUID}

// requireCleaned fails unless the limit was removed and both objects the
// run created were deleted by UID.
func (f *s11IMRestartFixture) requireCleaned(t *testing.T, scenario evidence.Scenario) {
	t.Helper()
	f.requireFaultsRemoved(t)
	if scenario.CleanupSucceeded == nil || !*scenario.CleanupSucceeded ||
		!slices.Equal(f.deleted, wantIMRestartDeleted) {
		t.Fatalf("cleanup = %v, deleted = %v; want the limit removed and both objects deleted by UID",
			scenario.CleanupSucceeded, f.deleted)
	}
}

func TestRunS11InstanceManagerRestartPassesWhenTheKilledUploadFails(t *testing.T) {
	cfg := testS11IMRestartConfig(t)
	f := newS11IMRestartFixture(t, cfg)
	scenario, err := s11IMRestartOf(t, f)
	if err != nil || scenario.Result != evidence.Pass {
		t.Fatalf("S11 instance-manager-restart = %+v, RunHA() error = %v; want a pass", scenario, err)
	}
	if !readSummary(t, cfg.EvidenceDir).Passed {
		t.Fatal("summary not passed")
	}
	if !strings.HasPrefix(strings.Join(f.writes, ","),
		"namespace,secret,cluster,client,backup,imrestart-standalone-cluster,ephemeral-container,backup") {
		t.Fatalf("writes = %v", f.writes)
	}
	if !slices.Equal(f.kills, []string{s11IMRestartMember}) || !f.limitAtFailed {
		t.Fatalf("kills = %v, limit installed when Failed = %t; want one kill with the limit held until the end",
			f.kills, f.limitAtFailed)
	}
	if slices.Contains(f.net.ops, "reset") {
		t.Fatalf("fault commands = %v, want no cut", f.net.ops)
	}
	f.requireCleaned(t, scenario)
	m := scenario.Measurements
	for _, key := range []string{failedWithinKey, "failedAfterKill", "memberBackWithin", "readyAgainWithin"} {
		if m[key] == evidence.Unknown {
			t.Fatalf("measurements = %v, want %s", m, key)
		}
	}
	if m["uploadBytesBeforeKill"] != float64(2*testUploadPerRead) || m["containerRestarts"] != float64(1) ||
		m["operationRefUnchanged"] != true || m["rateLimit.configuredRateBitsPerSecond"] != float64(interruptedRate) {
		t.Fatalf("measurements = %v", m)
	}
	if len(f.store.puts) != 0 || len(f.store.removes) != 0 {
		t.Fatalf("the run wrote %v and removed %v in the object store; want nothing", f.store.puts, f.store.removes)
	}
	want := map[string]string{podSecurityEnforce: podSecurityPrivileged, podSecurityWarn: podSecurityRestricted,
		podSecurityAudit: podSecurityRestricted}
	rec := readS11IMRestartRecord(t, cfg.EvidenceDir)
	if lab01 := readHARecord(t, cfg.EvidenceDir); !maps.Equal(lab01.PodSecurity, want) ||
		!maps.Equal(rec.PodSecurity, want) {
		t.Fatalf("Pod Security = %v and %v, want %v", lab01.PodSecurity, rec.PodSecurity, want)
	}
	requireS11IMRestartRecord(t, rec)
	requireNamespaceGone(t, f.haFixture)
}

// requireS11IMRestartRecord checks the record of a passed run.
func requireS11IMRestartRecord(t *testing.T, rec s11IMRestartRecord) {
	t.Helper()
	requireS11IMKill(t, rec.Kill)
	requireS11IMRestartEnd(t, rec)
}

// requireS11IMKill checks the kill in the record of a passed run.
func requireS11IMKill(t *testing.T, k *s11IMKill) {
	t.Helper()
	if k == nil || k.Pod != s11IMRestartMember || k.PodUID != testInterruptedPodUID ||
		k.ContainerBefore != testIMContainerID || k.ContainerAfter != testIMContainerID+"-restarted" ||
		k.PlacedAt == nil || k.KilledAt == nil || !k.confirmed() || k.ExitCode == nil {
		t.Fatalf("kill = %+v, want the Instance Manager killed and the container restarted", k)
	}
	if *k.ExitCode != sigkillExitCode || k.BackAt == nil || k.ReadyAgainAt == nil || k.ReadyAgainWithin == "" ||
		k.RestartsAtEnd == nil || *k.RestartsAtEnd != 1 || k.OperationRef != testIMOperation ||
		k.PhaseAtPlacement != phaseUploading || k.SentBeforeKill != 2*testUploadPerRead ||
		!slices.Equal(k.OperationRefsSeen, []string{testIMOperation}) {
		t.Fatalf("kill = %+v, want it placed during the upload and the member back once", k)
	}
}

// requireS11IMRestartEnd checks the limit, the backup's end and the checks
// at the end in the record of a passed run.
func requireS11IMRestartEnd(t *testing.T, rec s11IMRestartRecord) {
	t.Helper()
	if f := rec.Throttle; f == nil || f.Target != s11IMRestartMember || f.LimitConfirmedAt == nil ||
		f.AtRequest == nil || f.RemovedAt == nil || f.StoreCheckedAtEnd == nil || len(f.ExecErrors) != 0 {
		t.Fatalf("throttle = %+v, want the limit confirmed and removed", f)
	}
	if rec.Failure != s11MissingReason+": "+s12IMRestartedReason || !rec.EndedAfterKill ||
		rec.CompletedBeforeKill || rec.LastPhase != databasePhaseFailed || rec.PhaseAtEnd != databasePhaseFailed ||
		rec.Artifact != nil || rec.ArtifactAtEnd != nil || len(rec.ArtifactsNotDone) != 0 ||
		rec.FailedAfterKill == "" || rec.StoreError != "" {
		t.Fatalf("record = %+v, want the backup Failed for the restart", rec)
	}
	if rec.CheckedAtEnd == nil || len(rec.ObjectsAtEnd) != 0 || rec.ManifestAtEnd || !rec.IncompleteListed {
		t.Fatalf("record = %+v, want the prefix listed empty at the end", rec)
	}
	if rec.Source == nil || !rec.Source.DataRulesHeld || rec.ManagerAfter == nil || rec.JudgedAt == nil ||
		len(rec.Credentials.FoundIn) != 0 {
		t.Fatalf("record = %+v, want the source, the manager and the credentials checked", rec)
	}
	requireS11IMRestartLeft(t, rec)
}

// requireS11IMRestartLeft checks the residue and the logs searched in the
// record of a passed run.
func requireS11IMRestartLeft(t *testing.T, rec s11IMRestartRecord) {
	t.Helper()
	for _, want := range []string{"PVC", "ephemeral container", "staging directory"} {
		if !strings.Contains(rec.Residue, want) {
			t.Fatalf("residue = %q, want %s named", rec.Residue, want)
		}
	}
	logs := rec.Credentials.Logs
	if len(logs) != 2 || logs[1].Pod != s11IMRestartMember || logs[1].ReadFrom != logWhole ||
		!logs[1].PreviousRead || logs[1].RestartCount != 1 || logs[1].Unchecked != "" {
		t.Fatalf("logs = %+v, want the member's log read whole with its previous instance", logs)
	}
}

// A partial object left under the prefix is recorded, not failed.
func TestRunS11InstanceManagerRestartRecordsAPartialArtifact(t *testing.T) {
	cfg := testS11IMRestartConfig(t)
	f := newS11IMRestartFixture(t, cfg)
	f.leave = []string{testFirstBackupKey}
	f.incomplete = []string{"x/backup/appdb_bk0v000 (upload 1)"}
	scenario, err := s11IMRestartOf(t, f)
	if err != nil || scenario.Result != evidence.Pass {
		t.Fatalf("S11 instance-manager-restart = %+v, %v; want a pass with the partial artifact recorded",
			scenario, err)
	}
	rec := readS11IMRestartRecord(t, cfg.EvidenceDir)
	if !rec.PartialArtifact || rec.ManifestAtEnd || len(rec.ObjectsAtEnd) != 1 || len(rec.IncompleteUploads) != 1 ||
		!strings.Contains(rec.Residue, "a partial artifact") {
		t.Fatalf("record = %+v, want the partial object and the incomplete upload recorded", rec)
	}
}

func TestRunS11InstanceManagerRestartFailsOnANeverEvent(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(f *s11IMRestartFixture)
		want []string
	}{
		"Completed after the kill": {set: func(f *s11IMRestartFixture) {
			f.afterKill = databasev1alpha1.BackupPhaseCompleted
		}, want: []string{"ended Completed after its Instance Manager was killed", testArtifactField,
			instancemanager.ManifestObjectName}},
		"an artifact on an Uploading backup": {set: func(f *s11IMRestartFixture) {
			f.tamper = func(s *databasev1alpha1.CubridBackupStatus) {
				if s.Phase == databasev1alpha1.BackupPhaseUploading {
					s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: testElsewhereURI}
				}
			}
		}, want: []string{"status.artifact names s3://elsewhere/manifest.json", "in phase Uploading"}},
		"a manifest under the prefix": {set: func(f *s11IMRestartFixture) {
			f.leave = []string{testFirstBackupKey, instancemanager.ManifestObjectName}
		}, want: []string{"holds a manifest.json"}},
		"a second restart": {set: func(f *s11IMRestartFixture) { f.extraRestarts = 1 },
			want: []string{"restarted 2 times after one kill of its Instance Manager"}},
		"a second operation": {set: func(f *s11IMRestartFixture) {
			f.tamper = func(s *databasev1alpha1.CubridBackupStatus) {
				if s.Phase == databasev1alpha1.BackupPhaseFailed {
					s.OperationRef = testIMOperation2
				}
			}
		}, want: []string{"a second operation", testIMOperation2}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11IMRestartConfig(t)
			f := newS11IMRestartFixture(t, cfg)
			tc.set(f)
			scenario, err := s11IMRestartOf(t, f)
			if err == nil || scenario.Result != evidence.Fail || len(scenario.NeverEvents) == 0 {
				t.Fatalf("S11 instance-manager-restart = %+v, want a never-event", scenario)
			}
			for _, want := range tc.want {
				if !strings.Contains(scenario.Reason, want) {
					t.Fatalf("reason = %q, want %q", scenario.Reason, want)
				}
			}
			f.requireCleaned(t, scenario)
		})
	}
}

func TestRunS11InstanceManagerRestartFailsWhenTheBackupFailsForAnotherReason(t *testing.T) {
	for name, tc := range map[string]struct {
		message string
		want    []string
	}{
		"for backupdb": {message: testBackupdbFailed, want: []string{"not for the restart", testBackupdbFailed}},
		// A store error names its answer and kind in the failure.
		"the store": {message: "upload failed: put lab-backups/x: " + testResetAnswer,
			want: []string{"not for the restart", "store error kind " + storeErrorReset, "connection reset by peer"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11IMRestartConfig(t)
			f := newS11IMRestartFixture(t, cfg)
			f.failMessage = tc.message
			scenario, err := s11IMRestartOf(t, f)
			if err == nil || scenario.Result != evidence.Fail {
				t.Fatalf("S11 instance-manager-restart = %+v, want a failure", scenario)
			}
			for _, want := range tc.want {
				if !strings.Contains(scenario.Reason, want) {
					t.Fatalf("reason = %q, want %q", scenario.Reason, want)
				}
			}
			f.requireCleaned(t, scenario)
		})
	}
}

func TestRunS11InstanceManagerRestartFailsOnAKeyInTheKilledManagersLog(t *testing.T) {
	cfg := testS11IMRestartConfig(t)
	f := newS11IMRestartFixture(t, cfg)
	f.logs[logKey(cfg.Namespace, s11IMRestartMember, databaseContainer, true)] = "retry put with " +
		testSecretKey + "\n"
	scenario, err := s11IMRestartOf(t, f)
	if err == nil || scenario.Result != evidence.Fail || !strings.Contains(scenario.Reason, testCredential) {
		t.Fatalf("S11 instance-manager-restart = %+v, want a failure for the key", scenario)
	}
	rec := readS11IMRestartRecord(t, cfg.EvidenceDir)
	if logs := rec.Credentials.Logs; len(logs) != 2 || !logs[1].Found || !logs[1].PreviousRead {
		t.Fatalf("logs = %+v, want the key found in the member's previous instance", logs)
	}
}

func TestRunS11InstanceManagerRestartIsBlocked(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(f *s11IMRestartFixture)
		want string
	}{
		// The upload finished before enough of it was seen: a complete
		// backup, not one whose upload was killed.
		"the upload finished before the kill": {set: func(f *s11IMRestartFixture) {
			f.uploadPerRead, f.completeAfter = 100<<10, 2
		}, want: "inconclusive"},
		"the kill was refused": {set: func(f *s11IMRestartFixture) { f.refuse = true },
			want: "was not confirmed"},
		"the container ended otherwise": {set: func(f *s11IMRestartFixture) { f.exitCode = 1 },
			want: "ended for another reason than the kill"},
		"the documented escalation": {set: func(f *s11IMRestartFixture) {
			f.tamper = func(s *databasev1alpha1.CubridBackupStatus) {
				if s.Phase == databasev1alpha1.BackupPhaseFailed {
					s.Conditions[0].Reason = s11OperationLostReason
					s.Conditions[0].Message = "held no record of this backup's operation"
				}
			}
		}, want: "documented escalation " + s11OperationLostReason},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11IMRestartConfig(t)
			f := newS11IMRestartFixture(t, cfg)
			tc.set(f)
			scenario, err := s11IMRestartOf(t, f)
			if err == nil || scenario.Result != evidence.Blocked || !strings.Contains(scenario.Reason, tc.want) {
				t.Fatalf("S11 instance-manager-restart = %+v, want blocked: %s", scenario, tc.want)
			}
			f.requireCleaned(t, scenario)
		})
	}
}

// A backup that completed before the kill keeps its manifest apart from a
// partial artifact.
func TestRunS11InstanceManagerRestartKeepsACompleteBackupApart(t *testing.T) {
	cfg := testS11IMRestartConfig(t)
	f := newS11IMRestartFixture(t, cfg)
	f.uploadPerRead, f.completeAfter = 100<<10, 2
	if _, err := s11IMRestartOf(t, f); err == nil {
		t.Fatal("RunHA() passed")
	}
	rec := readS11IMRestartRecord(t, cfg.EvidenceDir)
	if !rec.CompletedBeforeKill || !rec.ManifestAtEnd || rec.PartialArtifact || rec.Kill.KilledAt != nil ||
		len(f.kills) != 0 || !strings.Contains(rec.Residue, "an artifact with a manifest.json") {
		t.Fatalf("record = %+v, kills = %v; want the complete backup recorded and nothing killed", rec, f.kills)
	}
}

// The variant is refused before anything is created when the run namespace
// does not enforce privileged: the limit's container would not be admitted.
func TestRunS11InstanceManagerRestartRefusesANamespaceThatIsNotPrivileged(t *testing.T) {
	cfg := testS11IMRestartConfig(t)
	f := newS11IMRestartFixture(t, cfg)
	created := f.backupCreated
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		ns := &corev1.Namespace{}
		if err := c.Get(ctx, client.ObjectKey{Name: cfg.Namespace}, ns); err != nil {
			return err
		}
		ns.Labels[podSecurityEnforce] = podSecurityRestricted
		if err := c.Update(ctx, ns); err != nil {
			return err
		}
		return created(ctx, c, backup)
	}
	scenario, err := s11IMRestartOf(t, f)
	if err == nil || scenario.Result != evidence.Blocked || !strings.Contains(scenario.Reason,
		`enforces Pod Security "restricted", not privileged`) ||
		!strings.Contains(scenario.Reason, "S11 instance-manager-restart creates nothing") {
		t.Fatalf("S11 instance-manager-restart = %+v, want refused for the namespace", scenario)
	}
	if f.cluster != nil || f.imBackup != nil || len(f.net.ops) != 0 || len(f.deleted) != 0 || len(f.kills) != 0 {
		t.Fatalf("cluster %v, backup %v, fault commands %v, deletions %v, kills %v; want nothing done", f.cluster,
			f.imBackup, f.net.ops, f.deleted, f.kills)
	}
}

func TestRunS11InstanceManagerRestartIsBlockedWhenS11CompleteDidNotPass(t *testing.T) {
	cfg := testS11IMRestartConfig(t)
	f := newS11IMRestartFixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if scenario := s11IMRestartScenarios(t, cfg.EvidenceDir)[2]; scenario.Result != evidence.Blocked ||
		!strings.Contains(scenario.Reason, "S11 complete did not pass") {
		t.Fatalf("S11 instance-manager-restart = %+v, want blocked by S11 complete", scenario)
	}
	if f.cluster != nil || len(f.net.ops) != 0 || len(f.kills) != 0 {
		t.Fatal("S11 instance-manager-restart ran although S11 complete did not pass")
	}
}

func TestRunHARefusesBackupInstanceManagerRestartWithoutItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(cfg *HAConfig)
		want   string
	}{
		testNoBucket: {change: func(cfg *HAConfig) { cfg.BackupBucket = "" },
			want: flagInstanceManagerRestart + " needs -backup-bucket"},
		testWithRestore: {change: func(cfg *HAConfig) {
			cfg.Restore, cfg.RestoreLimit = true, time.Minute
		}, want: flagInstanceManagerRestart + " and " + flagRestore + " select"},
		"with S11 missing-destination": {change: func(cfg *HAConfig) {
			cfg.BackupMissingDestination = true
		}, want: flagInstanceManagerRestart + " and " + flagMissingDestination + " select"},
		"with S11 denied-credentials": {change: func(cfg *HAConfig) {
			cfg.BackupDeniedCredentials, cfg.BackupDeniedReadyLimit = true, time.Minute
		}, want: flagInstanceManagerRestart + " and " + flagDeniedCredentials + " select"},
		"with S11 signature-mismatch": {change: func(cfg *HAConfig) {
			cfg.BackupSignatureMismatch, cfg.BackupSignatureMismatchReadyLimit = true, time.Minute
		}, want: "-backup-signature-mismatch and " + flagInstanceManagerRestart + " select"},
		"with S11 interrupted-upload": {change: func(cfg *HAConfig) {
			cfg.BackupInterruptedUpload, cfg.BackupInterruptedReadyLimit = true, time.Minute
		}, want: "-backup-interrupted-upload and " + flagInstanceManagerRestart + " select"},
		"with S11 operator-restart": {change: func(cfg *HAConfig) {
			cfg.BackupOperatorRestart = true
		}, want: "-backup-operator-restart and " + flagInstanceManagerRestart + " select"},
		"without its ready limit": {change: func(cfg *HAConfig) { cfg.BackupInstanceManagerRestartReadyLimit = 0 },
			want: "-backup-instance-manager-restart-ready-limit"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11IMRestartConfig(t)
			tc.change(&cfg)
			f := newS11IMRestartFixture(t, testS11IMRestartConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunHA() error = %v, want %s refused %s", err, flagInstanceManagerRestart, name)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}
