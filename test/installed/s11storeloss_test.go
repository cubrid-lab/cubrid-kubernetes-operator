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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	// testStoreListing is the command that lists the member's operation
	// store.
	testStoreListing = "ls -1A -- '" + imOperationsDir + "'"
	// The parts of reasons the failures and blocks of the variants that kill
	// a standalone member's Instance Manager share.
	testManifestHeld    = "holds a manifest.json"
	testInUploading     = "in phase Uploading"
	testSecondOperation = "a second operation"
	testInconclusive    = "inconclusive"
)

func testS11StoreLossConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupOperationStoreLoss = true
	cfg.BackupOperationStoreLossReadyLimit = 5 * time.Second
	cfg.BackupLimit = storeLossMinBackupLimit
	return cfg
}

// s11StoreLossFixture plays S11 operation-store-loss over the fixture of S11
// instance-manager-restart: the member removes the record and its kubelet
// starts the container again when the fault's command runs, and the
// Operator then reads the backup as missing. After the kill, the first
// reading stays Uploading while the manager cannot be reached, the next
// ones show RecordedOperationMissing with status.operationMissingSince, and
// the backup then ends Failed with RecordedOperationLost lostAfter after it.
type s11StoreLossFixture struct {
	*s11IMRestartFixture
	// recordStays keeps <id>.json in the store after the restart; noMissing
	// leaves out the readings of RecordedOperationMissing; failReason and
	// failMessage, when set, end the backup Failed with them instead;
	// lostAfter is the time from operationMissingSince to completedAt;
	// stagingLeft leaves the staging directory on the member; also changes
	// the status of a reading after the fixture's own changes.
	recordStays, noMissing  bool
	failReason, failMessage string
	lostAfter               time.Duration
	stagingLeft             bool
	also                    func(s *databasev1alpha1.CubridBackupStatus)
	// listErr answers the listing of the store after the restart.
	listErr error

	since    metav1.Time
	listings int
	stagings int
}

func newS11StoreLossFixture(t *testing.T, cfg HAConfig) *s11StoreLossFixture {
	t.Helper()
	f := &s11StoreLossFixture{s11IMRestartFixture: newS11KillFixture(t, cfg, s11StoreLossBackupName,
		s11StoreLossMember), lostAfter: storeLossMissingWait + 10*time.Second,
		since: metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))}
	f.uploadingAfterKill = 3
	f.memberHook = f.answer
	f.tamper = f.status
	return f
}

// answer plays the member's commands: the fault, the store's listing and
// the staging directory's.
func (f *s11StoreLossFixture) answer(ctx context.Context, pod, script string) (string, error, bool) {
	switch {
	case strings.Contains(script, "kill -STOP"):
		if script != storeLossScript(testIMOperation) {
			f.t.Errorf("fault script = %q, want storeLossScript of %s", script, testIMOperation)
		}
		f.mu.Lock()
		f.kills = append(f.kills, pod)
		f.mu.Unlock()
		out, err, done := f.restartKilled(ctx, pod)
		return storeLossRemovedLine + testIMOperation + ".json\n" + out, err, done
	case script == testStoreListing:
		f.mu.Lock()
		f.listings++
		f.mu.Unlock()
		if f.listErr != nil {
			return "", f.listErr, true
		}
		names := testIMOperation2 + ".json\n"
		if f.recordStays {
			names += testIMOperation + ".json\n"
		}
		return names, nil, true
	case strings.HasPrefix(script, "d='"+imBackupStagingRoot+"/"+testIMBackupUID+"'"):
		f.mu.Lock()
		f.stagings++
		f.mu.Unlock()
		if f.stagingLeft {
			return stagingPresent + "\nd 4096 " + testIMOperation + "\nf 2048 " + testIMOperation +
				"/appdb_bk0v000\n", nil, true
		}
		return stagingAbsent + "\n", nil, true
	}
	return "", nil, false
}

// status turns the readings after the kill into those of a backup whose
// record is missing, and the Failed one into RecordedOperationLost.
func (f *s11StoreLossFixture) status(s *databasev1alpha1.CubridBackupStatus) {
	f.mu.Lock()
	after := f.afterReads
	f.mu.Unlock()
	switch {
	case s.Phase == databasev1alpha1.BackupPhaseFailed:
		s.OperationMissingSince = &f.since
		lost := metav1.NewTime(f.since.Add(f.lostAfter))
		s.CompletedAt = &lost
		s.Conditions[0].Reason = s11OperationLostReason
		s.Conditions[0].Message = s11StoreLossMember + " has held no record of this backup's operation " +
			testIMOperation
		if f.failReason != "" {
			s.Conditions[0].Reason, s.Conditions[0].Message = f.failReason, f.failMessage
		}
	case after >= 2 && !f.noMissing:
		s.OperationMissingSince = &f.since
		s.Conditions[0].Reason = s12OperationMissingReason
		s.Conditions[0].Message = s11StoreLossMember + " holds no record of this backup's operation " +
			testIMOperation
	}
	if f.also != nil {
		f.also(s)
	}
}

// s11StoreLossOf runs the fixture and returns S11 operation-store-loss,
// requiring LAB01 and S11 complete passed.
func s11StoreLossOf(t *testing.T, f *s11StoreLossFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	summary := readSummary(t, f.cfg.EvidenceDir)
	if summary.Lane != s11StoreLossLane || len(summary.Scenarios) != 3 || summary.Scenarios[2].ID != s11ID ||
		summary.Scenarios[2].Variant != s11StoreLossVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/%s in lane %s", summary, s11StoreLossVariant,
			s11StoreLossLane)
	}
	for _, s := range summary.Scenarios[:2] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01 and S11 complete passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, summary.Scenarios[2], "S11/"+s11StoreLossVariant)
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	f.requireForwardStopped(t)
	return summary.Scenarios[2], err
}

func readS11StoreLossRecord(t *testing.T, dir string) s11StoreLossRecord {
	t.Helper()
	var rec s11StoreLossRecord
	data, err := os.ReadFile(filepath.Join(dir, s11StoreLossRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 operation-store-loss record = %s, %v", data, err)
	}
	return rec
}

// requireStoreLossCleaned fails unless the limit was removed and both
// objects the run created were deleted by UID.
func (f *s11StoreLossFixture) requireStoreLossCleaned(t *testing.T, scenario evidence.Scenario) {
	t.Helper()
	f.requireFaultsRemoved(t)
	want := []string{"CubridBackup/" + s11StoreLossBackupName + "/" + testIMBackupUID,
		testKindCluster + "/" + s11StoreLossClusterName + "/" + testInterruptedClusterUID}
	if scenario.CleanupSucceeded == nil || !*scenario.CleanupSucceeded || !slices.Equal(f.deleted, want) {
		t.Fatalf("cleanup = %v, deleted = %v; want the limit removed and both objects deleted by UID",
			scenario.CleanupSucceeded, f.deleted)
	}
}

func TestRunS11OperationStoreLossPassesWhenTheMissingRecordIsLost(t *testing.T) {
	cfg := testS11StoreLossConfig(t)
	f := newS11StoreLossFixture(t, cfg)
	scenario, err := s11StoreLossOf(t, f)
	if err != nil || scenario.Result != evidence.Pass {
		t.Fatalf("S11 operation-store-loss = %+v, RunHA() error = %v; want a pass", scenario, err)
	}
	if !strings.HasPrefix(strings.Join(f.writes, ","),
		"namespace,secret,cluster,client,backup,storeloss-cluster,ephemeral-container,backup") {
		t.Fatalf("writes = %v", f.writes)
	}
	if !slices.Equal(f.kills, []string{s11StoreLossMember}) || f.listings != 1 || f.stagings != 2 ||
		!f.limitAtFailed {
		t.Fatalf("kills = %v, listings = %d, staging reads = %d, limit when Failed = %t; want one fault, one "+
			"listing, two staging reads and the limit held", f.kills, f.listings, f.stagings, f.limitAtFailed)
	}
	f.requireStoreLossCleaned(t, scenario)
	m := scenario.Measurements
	if m[measRecordAbsent] != true || m[measMissingAfterKill] == evidence.Unknown ||
		m[measLostAfterMissing] != (storeLossMissingWait+10*time.Second).String() ||
		m[measStagingAtRestart] != false || m[measStagingAtEnd] != false || m[measContainerRestarts] != float64(1) ||
		m[measOperationRefUnchanged] != true || m[failedWithinKey] == evidence.Unknown {
		t.Fatalf("measurements = %v", m)
	}
	if len(f.store.puts) != 0 || len(f.store.removes) != 0 {
		t.Fatalf("the run wrote %v and removed %v in the object store; want nothing", f.store.puts, f.store.removes)
	}
	requireS11StoreLossRecord(t, readS11StoreLossRecord(t, cfg.EvidenceDir), f.since.Time)
	requireNamespaceGone(t, f.haFixture)
}

// requireS11StoreLossRecord checks the record of a passed run whose backup
// was missing since since.
func requireS11StoreLossRecord(t *testing.T, rec s11StoreLossRecord, since time.Time) {
	t.Helper()
	requireS11IMKill(t, rec.Kill, s11StoreLossMember)
	l := rec.StoreLoss
	if l == nil || l.OperationID != testIMOperation || !slices.Equal(l.Removed, []string{testIMOperation + ".json"}) ||
		l.RecordAbsent == nil || !*l.RecordAbsent || l.CheckedAt == nil ||
		!slices.Equal(l.StoreAfter, []string{testIMOperation2 + ".json"}) {
		t.Fatalf("store loss = %+v, want the record removed and absent after the restart", l)
	}
	if rec.FirstMissingAt == nil || rec.MissingAfterKill == "" || rec.OperationMissingSince == nil ||
		!rec.OperationMissingSince.Equal(since) || rec.Failure == "" ||
		!strings.HasPrefix(rec.Failure, s11OperationLostReason+": ") || !rec.EndedAfterKill ||
		rec.PhaseAtEnd != databasePhaseFailed || rec.Artifact != nil || rec.ArtifactAtEnd != nil {
		t.Fatalf("record = %+v, want the backup Failed with %s after %s", rec, s11OperationLostReason,
			s12OperationMissingReason)
	}
	requireS11StoreLossLeft(t, rec)
}

// requireS11StoreLossLeft checks the staging directory, the residue, the
// checks at the end and the logs searched in the record of a passed run.
func requireS11StoreLossLeft(t *testing.T, rec s11StoreLossRecord) {
	t.Helper()
	for _, st := range []*s11Staging{rec.StagingAfterRestart, rec.StagingAtEnd} {
		if st == nil || st.Present == nil || *st.Present || st.Dir != imBackupStagingRoot+"/"+testIMBackupUID {
			t.Fatalf("staging = %+v, want it read and absent", st)
		}
	}
	if !strings.Contains(rec.Residue, "staging directory "+imBackupStagingRoot+"/"+testIMBackupUID+" was not there") ||
		strings.Contains(rec.Residue, stagingNotChecked) {
		t.Fatalf("residue = %q, want the staging directory's absence", rec.Residue)
	}
	if rec.MinBackupLimit != storeLossMinBackupLimit.String() || rec.Source == nil || !rec.Source.DataRulesHeld ||
		rec.ManagerAfter == nil || len(rec.Credentials.FoundIn) != 0 {
		t.Fatalf("record = %+v, want the limits, the source, the manager and the credentials", rec)
	}
	if logs := rec.Credentials.Logs; len(logs) != 2 || logs[1].Pod != s11StoreLossMember || !logs[1].PreviousRead {
		t.Fatalf("logs = %+v, want the member's log read with its previous instance", logs)
	}
}

// A staging directory left on the member, and a partial artifact under the
// prefix, are recorded, not judged.
func TestRunS11OperationStoreLossRecordsWhatIsLeft(t *testing.T) {
	cfg := testS11StoreLossConfig(t)
	f := newS11StoreLossFixture(t, cfg)
	f.stagingLeft = true
	f.leave = []string{testFirstBackupKey}
	scenario, err := s11StoreLossOf(t, f)
	if err != nil || scenario.Result != evidence.Pass {
		t.Fatalf("S11 operation-store-loss = %+v, %v; want a pass with what is left recorded", scenario, err)
	}
	rec := readS11StoreLossRecord(t, cfg.EvidenceDir)
	if st := rec.StagingAtEnd; st == nil || st.Present == nil || !*st.Present || len(st.Entries) != 2 ||
		st.Entries[1] != "f 2048 "+testIMOperation+"/appdb_bk0v000" {
		t.Fatalf("staging at end = %+v, want the directory and its entries recorded", st)
	}
	if !rec.PartialArtifact || rec.ManifestAtEnd || !strings.Contains(rec.Residue, "with 2 entries") ||
		scenario.Measurements[measStagingAtEnd] != true {
		t.Fatalf("record = %+v, want the partial artifact and the staging directory recorded", rec)
	}
}

func TestRunS11OperationStoreLossFails(t *testing.T) {
	for name, tc := range map[string]struct {
		set   func(f *s11StoreLossFixture)
		never bool
		want  []string
	}{
		"Failed for the restart": {set: func(f *s11StoreLossFixture) {
			f.failReason, f.failMessage = s11MissingReason, s12IMRestartedReason
		}, want: []string{"the Instance Manager's restart, not " + s11OperationLostReason, "still held the record"}},
		"Failed for the store": {set: func(f *s11StoreLossFixture) {
			f.failReason, f.failMessage = s11MissingReason, "upload failed: put x: "+testResetAnswer
		}, want: []string{"but not with " + s11OperationLostReason, "store error kind " + storeErrorReset}},
		"ended Completed": {set: func(f *s11StoreLossFixture) {
			f.afterKill = databasev1alpha1.BackupPhaseCompleted
		}, never: true, want: []string{"ended Completed after its Instance Manager was killed", testArtifactField}},
		"no missing reading before Failed": {set: func(f *s11StoreLossFixture) { f.noMissing = true },
			want: []string{"no reading after the kill showed " + s12OperationMissingReason}},
		"lost before the bound": {set: func(f *s11StoreLossFixture) { f.lostAfter = time.Minute },
			want: []string{"before the bound of 30m0s"}},
		"an artifact while Uploading": {set: func(f *s11StoreLossFixture) {
			f.also = func(s *databasev1alpha1.CubridBackupStatus) {
				if s.Phase == databasev1alpha1.BackupPhaseUploading {
					s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: testElsewhereURI}
				}
			}
		}, never: true, want: []string{"status.artifact names " + testElsewhereURI, testInUploading}},
		"a manifest.json under the prefix": {set: func(f *s11StoreLossFixture) {
			f.leave = []string{testFirstBackupKey, instancemanager.ManifestObjectName}
		}, never: true, want: []string{testManifestHeld}},
		"Pending after the kill": {set: func(f *s11StoreLossFixture) {
			f.also = func(s *databasev1alpha1.CubridBackupStatus) {
				f.mu.Lock()
				after := f.afterReads
				f.mu.Unlock()
				if after == 2 && s.Phase == databasev1alpha1.BackupPhaseUploading {
					s.Phase = databasev1alpha1.BackupPhasePending
				}
			}
		}, want: []string{"the backup was Pending at", "keeps Running or Uploading until it ends"}},
		"another operation": {set: func(f *s11StoreLossFixture) {
			f.also = func(s *databasev1alpha1.CubridBackupStatus) {
				if s.Phase == databasev1alpha1.BackupPhaseFailed {
					s.OperationRef = testIMOperation2
				}
			}
		}, never: true, want: []string{testSecondOperation, testIMOperation2}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11StoreLossConfig(t)
			f := newS11StoreLossFixture(t, cfg)
			tc.set(f)
			scenario, err := s11StoreLossOf(t, f)
			if err == nil || scenario.Result != evidence.Fail || tc.never != (len(scenario.NeverEvents) > 0) {
				t.Fatalf("S11 operation-store-loss = %+v, want a failure (never-event %t)", scenario, tc.never)
			}
			for _, want := range tc.want {
				if !strings.Contains(scenario.Reason, want) {
					t.Fatalf("reason = %q, want %q", scenario.Reason, want)
				}
			}
			f.requireStoreLossCleaned(t, scenario)
		})
	}
}

func TestRunS11OperationStoreLossIsBlocked(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(f *s11StoreLossFixture)
		want string
	}{
		"the record is still there after the restart": {set: func(f *s11StoreLossFixture) { f.recordStays = true },
			want: "holds " + testIMOperation + ".json after the restart"},
		"the store could not be listed after the restart": {set: func(f *s11StoreLossFixture) {
			f.listErr = errors.New("command terminated with exit code 2")
		}, want: "could not be listed after the restart, so the removal of the record of operation " +
			testIMOperation + " is not confirmed"},
		"the fault was refused": {set: func(f *s11StoreLossFixture) {
			f.memberHook = func(ctx context.Context, pod, script string) (string, error, bool) {
				if strings.Contains(script, "kill -STOP") {
					return storeLossRefused + " no record " + testIMOperation + ".json\n", nil, true
				}
				return f.answer(ctx, pod, script)
			}
		}, want: "nothing was killed in " + s11StoreLossMember},
		"the upload finished before the fault": {set: func(f *s11StoreLossFixture) {
			f.uploadPerRead, f.completeAfter = 100<<10, 2
		}, want: testInconclusive},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11StoreLossConfig(t)
			f := newS11StoreLossFixture(t, cfg)
			tc.set(f)
			scenario, err := s11StoreLossOf(t, f)
			if err == nil || scenario.Result != evidence.Blocked || !strings.Contains(scenario.Reason, tc.want) {
				t.Fatalf("S11 operation-store-loss = %+v, want blocked: %s", scenario, tc.want)
			}
			f.requireStoreLossCleaned(t, scenario)
		})
	}
}

func TestRunHARefusesBackupOperationStoreLossWithoutItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(cfg *HAConfig)
		want   string
	}{
		testNoBucket: {change: func(cfg *HAConfig) { cfg.BackupBucket = "" },
			want: flagOperationStoreLoss + " needs -backup-bucket"},
		"with a backup_limit below 35m": {change: func(cfg *HAConfig) { cfg.BackupLimit = 30 * time.Minute },
			want: flagOperationStoreLoss + " needs -backup-limit of at least 35m0s, not 30m0s"},
		"without the ready limit": {change: func(cfg *HAConfig) { cfg.BackupOperationStoreLossReadyLimit = 0 },
			want: "-backup-operation-store-loss-ready-limit"},
		testWithRestore: {change: func(cfg *HAConfig) {
			cfg.Restore, cfg.RestoreLimit = true, time.Minute
		}, want: flagOperationStoreLoss + " and " + flagRestore + " select"},
		testWithMissing: {change: func(cfg *HAConfig) { cfg.BackupMissingDestination = true },
			want: flagOperationStoreLoss + " and " + flagMissingDestination + " select"},
		testWithDenied: {change: func(cfg *HAConfig) {
			cfg.BackupDeniedCredentials, cfg.BackupDeniedReadyLimit = true, time.Minute
		}, want: flagOperationStoreLoss + " and " + flagDeniedCredentials + " select"},
		"with S11 signature-mismatch": {change: func(cfg *HAConfig) {
			cfg.BackupSignatureMismatch, cfg.BackupSignatureMismatchReadyLimit = true, time.Minute
		}, want: flagSignatureMismatch + " and " + flagOperationStoreLoss + " select"},
		testWithInterrupted: {change: func(cfg *HAConfig) {
			cfg.BackupInterruptedUpload, cfg.BackupInterruptedReadyLimit = true, time.Minute
		}, want: flagInterruptedUpload + " and " + flagOperationStoreLoss + " select"},
		"with the operator-restart variant": {change: func(cfg *HAConfig) { cfg.BackupOperatorRestart = true },
			want: flagOperatorRestart + " and " + flagOperationStoreLoss + " select"},
		"with the instance-manager-restart variant": {change: func(cfg *HAConfig) {
			cfg.BackupInstanceManagerRestart, cfg.BackupInstanceManagerRestartReadyLimit = true, time.Minute
		}, want: flagInstanceManagerRestart + " and " + flagOperationStoreLoss + " select"},
		"with the overlapping variant": {change: func(cfg *HAConfig) {
			cfg.BackupOverlapping, cfg.BackupOverlappingReadyLimit = true, time.Minute
		}, want: flagOverlapping + " and " + flagOperationStoreLoss + " select"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11StoreLossConfig(t)
			tc.change(&cfg)
			f := newS11StoreLossFixture(t, testS11StoreLossConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunHA() error = %v, want %s refused %s", err, flagOperationStoreLoss, name)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}

func TestRunS11OperationStoreLossIsBlockedWhenS11CompleteDidNotPass(t *testing.T) {
	cfg := testS11StoreLossConfig(t)
	f := newS11StoreLossFixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	summary := readSummary(t, cfg.EvidenceDir)
	if summary.Lane != s11StoreLossLane || len(summary.Scenarios) != 3 {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/%s", summary, s11StoreLossVariant)
	}
	if scenario := summary.Scenarios[2]; scenario.Variant != s11StoreLossVariant ||
		scenario.Result != evidence.Blocked || !strings.Contains(scenario.Reason, "S11 complete did not pass") {
		t.Fatalf("S11 operation-store-loss = %+v, want blocked by S11 complete", scenario)
	}
	if f.cluster != nil || len(f.net.ops) != 0 || len(f.kills) != 0 || f.listings != 0 {
		t.Fatal("S11 operation-store-loss ran although S11 complete did not pass")
	}
}

// testStoreLossJudged builds the record and backup judgeStoreLossBackup
// takes: the fault placed and the kill at the first of phases, and the
// backup as the last reading shows it, missing since since and completed at
// done.
func testStoreLossJudged(phases []s11Phase, since, done *metav1.Time) (*s11StoreLossRecord,
	*databasev1alpha1.CubridBackup) {
	placed, absent := phases[0].At, true
	end := phases[len(phases)-1]
	rec := &s11StoreLossRecord{s11IMRestartRecord: s11IMRestartRecord{
		Cluster: &s11StandaloneCluster{Name: s11StoreLossClusterName}, Throttle: &s11Throttle{},
		Kill: &s11IMKill{memberKill: memberKill{KilledAt: &placed, EndedAt: &placed}, PlacedAt: &placed,
			OperationRef: testIMOperation, OperationRefsSeen: []string{}, placed: 0},
		Phases: phases, variant: "S11 operation-store-loss"},
		StoreLoss: &s11StoreLoss{RecordAbsent: &absent}}
	backup := &databasev1alpha1.CubridBackup{Status: databasev1alpha1.CubridBackupStatus{
		Phase: databasev1alpha1.CubridBackupPhase(end.Phase), OperationRef: testIMOperation,
		Conditions: end.Conditions, OperationMissingSince: since, CompletedAt: done}}
	return rec, backup
}

// testReading is a reading of the backup at at, in phase, with Ready reason
// reason.
func testReading(at time.Time, phase databasev1alpha1.CubridBackupPhase, reason string) s11Phase {
	return s11Phase{At: at, Phase: string(phase), OperationRef: testIMOperation,
		Conditions: []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse, Reason: reason}}}
}

// The judgement on its own: a missing reading after the kill is required
// apart from its measurement, the phase stays Running or Uploading, both
// bound times must be set, the bound itself counts as reached, and a
// backup_limit that ran out before the Operator's bound is inconclusive.
func TestJudgeStoreLossBackup(t *testing.T) {
	const pass, fail, block = "pass", "fail", "block"
	kill := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	since := metav1.NewTime(kill.Add(time.Minute))
	atBound := metav1.NewTime(since.Add(storeLossMissingWait))
	early := metav1.NewTime(since.Add(storeLossMissingWait - time.Second))
	up, failed := databasev1alpha1.BackupPhaseUploading, databasev1alpha1.BackupPhaseFailed
	placing := testReading(kill, up, testUploadRunning)
	unreachable := testReading(kill.Add(10*time.Second), up, "InstanceManagerUnavailable")
	missing := testReading(since.Time, up, s12OperationMissingReason)
	lost := testReading(atBound.Add(5*time.Second), failed, s11OperationLostReason)
	limit := backupVerdict{fmt.Errorf("%w 40m0s; its last phase was %q", errBackupLimitPassed, up)}
	noMissing := "no reading after the kill showed " + s12OperationMissingReason
	for name, tc := range map[string]struct {
		phases      []s11Phase
		since, done *metav1.Time
		waitErr     error
		want        string
		reason      string
	}{
		"lost exactly at the bound": {phases: []s11Phase{placing, unreachable, missing, lost}, since: &since,
			done: &atBound, want: pass},
		"no missing reading": {phases: []s11Phase{placing, unreachable, lost}, since: &since, done: &atBound,
			want: fail, reason: noMissing},
		"a missing reading only at the placement": {
			phases: []s11Phase{testReading(kill, up, s12OperationMissingReason), unreachable, lost},
			since:  &since, done: &atBound, want: fail, reason: noMissing},
		"Pending after the kill": {phases: []s11Phase{placing,
			testReading(kill.Add(10*time.Second), databasev1alpha1.BackupPhasePending, testRunningReason), missing,
			lost}, since: &since, done: &atBound, want: fail, reason: "keeps Running or Uploading until it ends"},
		"lost before the bound": {phases: []s11Phase{placing, missing, lost}, since: &since, done: &early,
			want: fail, reason: "before the bound of 30m0s"},
		"lost without operationMissingSince": {phases: []s11Phase{placing, missing, lost}, done: &atBound,
			want: block, reason: "cannot be checked"},
		"lost without completedAt": {phases: []s11Phase{placing, missing, lost}, since: &since, want: block,
			reason: "cannot be checked"},
		"backup_limit before the Operator's bound": {
			phases: []s11Phase{placing, missing, testReading(since.Add(10*time.Minute), up,
				s12OperationMissingReason)}, since: &since, waitErr: limit, want: block,
			reason: "inconclusive: backup_limit ran out while the backup read " + s12OperationMissingReason},
		"backup_limit after the Operator's bound": {
			phases: []s11Phase{placing, missing, testReading(since.Add(storeLossMissingWait+time.Minute), up,
				s12OperationMissingReason)}, since: &since, waitErr: limit, want: fail,
			reason: errBackupLimitPassed.Error()},
		"backup_limit before any missing reading": {phases: []s11Phase{placing, unreachable}, waitErr: limit,
			want: fail, reason: errBackupLimitPassed.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			rec, backup := testStoreLossJudged(tc.phases, tc.since, tc.done)
			result := &evidence.Scenario{Measurements: map[string]any{}}
			err := judgeStoreLossBackup(backup, tc.waitErr, kill.Add(-time.Minute), rec, result)
			got := pass
			switch {
			case errors.As(err, new(backupVerdict)):
				got = fail
			case err != nil:
				got = block
			}
			if got != tc.want || (err != nil && !strings.Contains(err.Error(), tc.reason)) {
				t.Fatalf("judgeStoreLossBackup() = %v (%s), want %s: %s", err, got, tc.want, tc.reason)
			}
			if tc.want == pass && (rec.FirstMissingAt == nil || !rec.FirstMissingAt.Equal(since.Time) ||
				rec.MissingAfterKill != "1m0s" || rec.FailedAfterMissingSince != "30m0s") {
				t.Fatalf("record = %+v, want the first missing reading and the bound measured", rec)
			}
		})
	}
}
