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
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// seedingPhase is the bootstrap phase while the other members are seeded.
const seedingPhase = databasev1alpha1.BootstrapSeedingReplicas

const (
	// testIMPeer is the member seeded first from the restore's target, and
	// testIMPeer2 the next one.
	testIMPeer  = "ha-imrestart-1"
	testIMPeer2 = "ha-imrestart-2"
	// testIMSeedOp and testIMSeedOp2 are testIMPeer's seed restores under
	// the database's attempts 0 and 1, testIMSeedOp3 testIMPeer2's.
	testIMSeedOp  = "op-seed-0"
	testIMSeedOp2 = "op-seed-1"
	testIMSeedOp3 = "op-seed-2"
	// testIMSeedRetryMessage is the Operator's message of the seed attempt
	// the kill ended.
	testIMSeedRetryMessage = "on " + testIMPeer + ": " + s12IMRestartedReason + "; starting attempt 2 of 3"
	testIMPreparing        = "the backup is being verified and downloaded on " + testIMRestartTarget
	testIMRestoredbRunning = "restoredb is running on " + testIMRestartTarget
	testIMRefused          = "connection refused"
	testIMCopying          = "copying"
	testIMCopied           = "copied"
	testIMSeedBackup       = "SeedBackupInProgress"
)

func testS12IMRestartConfigAt(t *testing.T, at string) HAConfig {
	t.Helper()
	cfg := testS12IMRestartConfig(t)
	cfg.RestoreInstanceManagerRestartAt = at
	return cfg
}

// testOpRecord is the answer of killedOperationScript: the record, and the
// staging line.
func testOpRecord(t *testing.T, op instancemanager.Operation, staging string) string {
	t.Helper()
	data, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n" + imStagingLine + staging + "\n"
}

// newS12IMRestartPreparingFixture plays a restore whose Instance Manager is
// killed while the bootstrap is Preparing with its operation: the next
// reading after the kill is still Preparing, the attempt then fails with the
// manager's restart, and the next one completes. The target answers its
// record of the killed operation as Failed by the restart, without restored
// data, with no staging directory left.
func newS12IMRestartPreparingFixture(t *testing.T) *s12IMRestartFixture {
	t.Helper()
	f := newS12IMRestartFixture(t, testS12IMRestartConfigAt(t, IMRestartAtPreparing))
	f.beforeKill, f.holdStep = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing}, 0
	f.afterKill = []imStatus{
		{databasev1alpha1.BootstrapPreparing, 0, testIMRestartOp, testIMUnavailable, testIMRefused},
		{databasev1alpha1.BootstrapPreparing, 1, "", s12RetryReason, testIMRetryMessage},
		{databasev1alpha1.BootstrapPreparing, 1, testIMRestartOp2, testRestoreInProgress, testIMPreparing},
		{databasev1alpha1.BootstrapRestoring, 1, testIMRestartOp2, testRestoreInProgress,
			testIMRestoredbRunning},
		{databasev1alpha1.BootstrapSeedingReplicas, 1, testIMRestartOp2, testSeedInProgress, testIMCopying},
		{databasev1alpha1.BootstrapComplete, 1, testIMRestartOp2, testPeersSeeded, testIMCopied},
	}
	f.opRecord = func(pod, script string) (string, error) {
		if pod != testIMRestartTarget || !strings.Contains(script, imOperationsDir+"/"+testIMRestartOp+".json") ||
			!strings.Contains(script, imRestoreStagingRoot+"/"+testIMRestartUID) {
			t.Errorf("record read on %s with %q, want the killed operation's and the cluster's staging", pod, script)
		}
		return testOpRecord(t, instancemanager.Operation{ID: testIMRestartOp, Kind: instancemanager.OpRestore,
			State: instancemanager.OpFailed, FailureReason: s12IMRestartedReason}, "absent"), nil
	}
	return f
}

func TestRunS12InstanceManagerRestartPreparingPassesWhenTheRetryCompletesTheRestore(t *testing.T) {
	f := newS12IMRestartPreparingFixture(t)
	cfg := f.cfg
	restart, err := imRestartOf(t, f)
	if err != nil || restart.Result != evidence.Pass {
		t.Fatalf("S12 instance-manager-restart-preparing = %+v, %v; want a pass", restart, err)
	}
	if f.imKills != 1 || !slices.Equal(f.killedPods, []string{testIMRestartTarget}) || f.recordReads != 1 {
		t.Fatalf("kills = %d on %v, record reads = %d; want one each on %s", f.imKills, f.killedPods, f.recordReads,
			testIMRestartTarget)
	}
	for _, want := range []string{"Instance Manager on " + testIMRestartTarget + " was killed",
		"Preparing with operation " + testIMRestartOp, "restoredb had not succeeded",
		"completed the restore under operation " + testIMRestartOp2 + ", attempt 1, with one retry",
		"staging directory was not left"} {
		if reason := imRestartPassReasonAt(readIMRestartRecord(t, cfg).IMRestart); !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want %q", reason, want)
		}
	}
	for _, rel := range []string{"record.json", "timeline.jsonl", "data-check.json", "source-data-check.json"} {
		if !slices.Contains(restart.Evidence, "S12/instance-manager-restart-preparing/"+rel) {
			t.Fatalf("evidence = %v, want %s of the variant", restart.Evidence, rel)
		}
	}
	m := restart.Measurements
	if m["restoredAtKill"] != false || m["stagingLeftAtEnd"] != false || m["retriesAfterKill"] != float64(1) ||
		m["operationIDChanged"] != true || m["containerRestarts"] != float64(1) {
		t.Fatalf("measurements = %v", m)
	}
	r := readIMRestartRecord(t, cfg).IMRestart
	preparing := string(databasev1alpha1.BootstrapPreparing)
	if r.At != IMRestartAtPreparing || r.PhaseAtRestart != preparing || r.PhaseAfterRestart != preparing ||
		r.MessageAtRestart != testIMPreparing || r.KilledOperation == nil ||
		r.KilledOperation.State != string(instancemanager.OpFailed) || r.KilledOperation.Restored != "" ||
		r.KilledOperation.StagingPresent == nil || *r.KilledOperation.StagingPresent {
		t.Fatalf("restart = %+v, killed operation = %+v", r, r.KilledOperation)
	}
}

func TestRunS12InstanceManagerRestartPreparingIsBlockedWhenTheKillDidNotLandWhilePreparing(t *testing.T) {
	restored := func(t *testing.T) func(string, string) (string, error) {
		return func(string, string) (string, error) {
			return testOpRecord(t, instancemanager.Operation{ID: testIMRestartOp, State: instancemanager.OpFailed,
				FailureReason: s12IMRestartedReason, Restored: &instancemanager.OperationArtifact{
					ManifestURI: "s3://bucket/run/manifest.json"}}, "absent"), nil
		}
	}
	for name, tc := range map[string]struct {
		change func(*testing.T, *s12IMRestartFixture)
		kills  int
		want   []string
	}{
		"Preparing with an operation never seen": {change: func(_ *testing.T, f *s12IMRestartFixture) {
			f.beforeKill = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapRestoring}
		}, want: []string{"could not be placed while the restore was Preparing",
			"first seen Restoring, not Preparing with an operation"}},
		"Preparing over before the kill": {change: func(_ *testing.T, f *s12IMRestartFixture) {
			f.beforeKill = append(f.beforeKill, databasev1alpha1.BootstrapRestoring)
			f.noHold = true
		}, want: []string{"could not be placed while the restore was Preparing",
			`right before it the bootstrap phase was "Restoring"`}},
		// The Operator read the operation Restoring before the kill took
		// effect; the retry then follows.
		"Restoring read after the kill": {change: func(_ *testing.T, f *s12IMRestartFixture) {
			f.afterKill = slices.Insert(f.afterKill, 0, imStatus{databasev1alpha1.BootstrapRestoring, 0,
				testIMRestartOp, testRestoreInProgress, testIMRestoredbRunning})
		}, kills: 1, want: []string{"did not land while the restore was Preparing",
			"showed the bootstrap Restoring with operation " + testIMRestartOp + " under attempt 0"}},
		"past Preparing after the kill and no retry": {change: func(_ *testing.T, f *s12IMRestartFixture) {
			f.afterKill = []imStatus{
				{databasev1alpha1.BootstrapRestoring, 0, testIMRestartOp, testRestoreInProgress, testIMRestoredbRunning},
				{databasev1alpha1.BootstrapSeedingReplicas, 0, testIMRestartOp, testSeedInProgress, testIMCopying},
				{databasev1alpha1.BootstrapComplete, 0, testIMRestartOp, testPeersSeeded, testIMCopied},
			}
		}, kills: 1, want: []string{"the kill did not interrupt the restore while it was Preparing",
			"the first reading after it was Restoring"}},
		"the record holds restored data": {change: func(t *testing.T, f *s12IMRestartFixture) {
			f.opRecord = restored(t)
		}, kills: 1, want: []string{"did not land while the restore was Preparing",
			"holds the restored artifact s3://bucket/run/manifest.json"}},
		"the record cannot be read": {change: func(_ *testing.T, f *s12IMRestartFixture) {
			f.opRecord = func(string, string) (string, error) {
				return "", errors.New("No such file or directory\ncommand terminated with exit code 1")
			}
		}, kills: 1, want: []string{"could not be read, so whether restoredb had succeeded before the kill is not " +
			"known", "No such file or directory"}},
		"the record failed for another reason": {change: func(t *testing.T, f *s12IMRestartFixture) {
			f.opRecord = func(string, string) (string, error) {
				return testOpRecord(t, instancemanager.Operation{ID: testIMRestartOp, State: instancemanager.OpFailed,
					FailureReason: "restore failed: download: connection reset"}, "absent"), nil
			}
		}, kills: 1, want: []string{"not Failed by its restart", testReset}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newS12IMRestartPreparingFixture(t)
			tc.change(t, f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, tc.want...)
			if f.imKills != tc.kills {
				t.Fatalf("kills = %d, want %d", f.imKills, tc.kills)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartPreparingRecordsAStagingDirectoryLeft(t *testing.T) {
	f := newS12IMRestartPreparingFixture(t)
	f.opRecord = func(string, string) (string, error) {
		return testOpRecord(t, instancemanager.Operation{ID: testIMRestartOp, State: instancemanager.OpFailed,
			FailureReason: s12IMRestartedReason}, "present"), nil
	}
	restart, err := imRestartOf(t, f)
	if err != nil || restart.Result != evidence.Pass || restart.Measurements["stagingLeftAtEnd"] != true ||
		!strings.Contains(imRestartPassReasonAt(readIMRestartRecord(t, f.cfg).IMRestart), "staging directory was left") {
		t.Fatalf("S12 instance-manager-restart-preparing = %+v, %v; want a pass that records the staging left",
			restart, err)
	}
}

func TestRunS12InstanceManagerRestartPreparingFailsOnTheDataBeforeTheRecordIsRead(t *testing.T) {
	f := newS12IMRestartPreparingFixture(t)
	f.missing[testIMRestartTarget] = 0
	f.opRecord = func(string, string) (string, error) { return "", errors.New(testIMRefused) }
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Fail, testIMRestartTarget)
	if f.recordReads != 0 {
		t.Fatalf("record reads = %d, want none after the data failed", f.recordReads)
	}
}

// newS12IMRestartSeedingFixture plays a recovery whose restore completed on
// the target and whose first peer's Instance Manager is killed during its
// seed restore: the seed attempt fails with the manager's restart, the
// Operator seeds the peer again under the database's next attempt, then the
// other peer, and completes.
func newS12IMRestartSeedingFixture(t *testing.T) *s12IMRestartFixture {
	t.Helper()
	f := newS12IMRestartFixture(t, testS12IMRestartConfigAt(t, IMRestartAtSeeding))
	seeding := databasev1alpha1.BootstrapSeedingReplicas
	f.beforeKill = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing,
		databasev1alpha1.BootstrapRestoring, seeding, seeding}
	f.holdStep = 3
	f.beforeSeed = []*imSeed{nil, nil,
		{member: testIMPeer, reason: testIMSeedBackup, message: "backing up on " + testIMRestartTarget},
		{member: testIMPeer, op: testIMSeedOp, reason: testSeedInProgress, message: "restoring on " + testIMPeer}}
	f.afterKill = []imStatus{
		{seeding, 0, testIMRestartOp, testIMUnavailable, testIMRefused},
		{seeding, 0, testIMRestartOp, s12SeedRetryReason, testIMSeedRetryMessage},
		{seeding, 0, testIMRestartOp, testIMSeedBackup, "backing up on " + testIMRestartTarget},
		{seeding, 0, testIMRestartOp, testSeedInProgress, "restoring on " + testIMPeer},
		{seeding, 0, testIMRestartOp, testSeedInProgress, "restoring on " + testIMPeer2},
		{databasev1alpha1.BootstrapComplete, 0, testIMRestartOp, testPeersSeeded, testIMCopied},
	}
	f.afterSeed = []*imSeed{
		{member: testIMPeer, op: testIMSeedOp},
		{attempts: 1},
		{attempts: 1, member: testIMPeer},
		{attempts: 1, member: testIMPeer, op: testIMSeedOp2},
		{attempts: 1, member: testIMPeer2, op: testIMSeedOp3, seeded: []string{testIMPeer}},
		{attempts: 1, seeded: []string{testIMPeer, testIMPeer2}},
	}
	return f
}

func TestRunS12InstanceManagerRestartSeedingPassesWhenTheSeedRetryCompletes(t *testing.T) {
	f := newS12IMRestartSeedingFixture(t)
	restart, err := imRestartOf(t, f)
	if err != nil || restart.Result != evidence.Pass {
		t.Fatalf("S12 instance-manager-restart-seeding = %+v, %v; want a pass", restart, err)
	}
	if f.imKills != 1 || !slices.Equal(f.killedPods, []string{testIMPeer}) || f.recordReads != 0 {
		t.Fatalf("kills = %d on %v, record reads = %d; want one kill on %s", f.imKills, f.killedPods, f.recordReads,
			testIMPeer)
	}
	for _, want := range []string{"Instance Manager on " + testIMPeer + " was killed while it was seeded",
		"seed restore operation " + testIMSeedOp, s12SeedRetryReason, "seeding attempt 1 (seed restore operation " +
			testIMSeedOp2 + "), with one seed retry", "the restore kept operation " + testIMRestartOp} {
		if reason := imRestartPassReasonAt(readIMRestartRecord(t, f.cfg).IMRestart); !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want %q", reason, want)
		}
	}
	if !slices.Contains(restart.Evidence, "S12/instance-manager-restart-seeding/record.json") {
		t.Fatalf("evidence = %v, want the variant's record", restart.Evidence)
	}
	m := restart.Measurements
	if m["seedRetriesAfterKill"] != float64(1) || m["seedAttemptsAdded"] != float64(1) ||
		m["seedRestoreOperationChanged"] != true || m["retriesAfterKill"] != float64(0) ||
		m["attemptsAdded"] != float64(0) || m["operationIDChanged"] != false || m["containerRestarts"] != float64(1) {
		t.Fatalf("measurements = %v", m)
	}
	r := readIMRestartRecord(t, f.cfg).IMRestart
	if r.At != IMRestartAtSeeding || r.Pod != testIMPeer || r.PodUID != testIMPeer ||
		r.PhaseAtRestart != string(databasev1alpha1.BootstrapSeedingReplicas) ||
		r.SeedRestoreOperationIDBefore != testIMSeedOp || r.SeedAttemptsBefore == nil || *r.SeedAttemptsBefore != 0 ||
		r.SeedRestoreOperationIDAfter != testIMSeedOp2 || r.SeedAttemptsAfter == nil || *r.SeedAttemptsAfter != 1 ||
		len(r.SeedRetries) != 1 || r.SeedRetries[0].Message != testIMSeedRetryMessage || len(r.Retries) != 0 {
		t.Fatalf("restart = %+v", r)
	}
}

func TestRunS12InstanceManagerRestartSeedingIsBlockedWhenTheKillCannotBePlaced(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture)
		want   string
	}{
		"Complete before a seed restore": {change: func(f *s12IMRestartFixture) {
			f.beforeKill[3] = databasev1alpha1.BootstrapComplete
			f.beforeSeed[3] = &imSeed{seeded: []string{testIMPeer, testIMPeer2}, reason: testPeersSeeded,
				message: testIMCopied}
		}, want: "the bootstrap was seen Complete before a reading of SeedingReplicas with a seed restore"},
		"the seed restore over before the kill": {change: func(f *s12IMRestartFixture) {
			f.beforeKill = append(f.beforeKill, databasev1alpha1.BootstrapSeedingReplicas)
			f.beforeSeed = append(f.beforeSeed, &imSeed{member: testIMPeer2, seeded: []string{testIMPeer},
				reason: testIMSeedBackup, message: "backing up"})
			f.noHold = true
		}, want: `right before it the bootstrap phase was "SeedingReplicas" with no seed restore recorded`},
		"the restore's target being seeded": {change: func(f *s12IMRestartFixture) {
			f.beforeSeed[3].member = testIMRestartTarget
		}, want: "the member being seeded, " + testIMRestartTarget + ", is the restore's target"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newS12IMRestartSeedingFixture(t)
			tc.change(f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, tc.want)
			if f.imKills != 0 {
				t.Fatalf("kills = %d, want none", f.imKills)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartSeedingWhenTheKillDidNotEndTheSeedRestore(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture)
		result evidence.Result
		want   []string
	}{
		"the seed restore had completed": {change: func(f *s12IMRestartFixture) {
			f.afterKill = []imStatus{f.afterKill[4], f.afterKill[5]}
			f.afterSeed = []*imSeed{{member: testIMPeer2, op: testIMSeedOp3, seeded: []string{testIMPeer}},
				{seeded: []string{testIMPeer, testIMPeer2}}}
		}, result: evidence.Blocked, want: []string{"the kill did not interrupt a seed restore",
			"with " + testIMPeer + " seeded", "the kill did not interrupt the seed restore: " + testIMPeer +
				" was seeded with no failed seed attempt"}},
		"the seed retry for another reason": {change: func(f *s12IMRestartFixture) {
			f.afterKill[1].message = "on " + testIMPeer + ": restore failed: restoreslave failed; starting attempt 2 of 3"
		}, result: evidence.Blocked, want: []string{"not with a seed restore on " + testIMPeer +
			" failed by the Instance Manager's restart"}},
		"the seed retry of a backup": {change: func(f *s12IMRestartFixture) {
			f.afterKill[1].reason = s12SeedBackupRetryReason
		}, result: evidence.Blocked, want: []string{"ended with " + s12SeedBackupRetryReason}},
		"a seeding attempt without its reason": {change: func(f *s12IMRestartFixture) {
			f.afterKill, f.afterSeed = slices.Delete(f.afterKill, 1, 2), slices.Delete(f.afterSeed, 1, 2)
		}, result: evidence.Blocked, want: []string{"seeding attempts rose from 0 to 1", "cause is unknown"}},
		"the seeding failing again": {change: func(f *s12IMRestartFixture) {
			again := "on " + testIMPeer2 + ": restore failed: restoreslave failed; starting attempt 3 of 3"
			f.afterKill = slices.Insert(f.afterKill, 5,
				imStatus{databasev1alpha1.BootstrapSeedingReplicas, 0, testIMRestartOp, s12SeedRetryReason, again})
			f.afterSeed = slices.Insert(f.afterSeed, 5, &imSeed{attempts: 2, seeded: []string{testIMPeer}})
			f.afterSeed[6].attempts = 2
		}, result: evidence.Fail, want: []string{"the seeding failed again after the seed attempt the kill ended",
			"restoreslave failed"}},
		"the killed seed restore answered for the next attempt": {change: func(f *s12IMRestartFixture) {
			f.afterSeed[3].op = testIMSeedOp
		}, result: evidence.Fail, want: []string{"the seed restore operation " + testIMSeedOp + " the kill ended " +
			"is the one recorded for " + testIMPeer}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newS12IMRestartSeedingFixture(t)
			tc.change(f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, tc.result, tc.want...)
			if f.imKills != 1 {
				t.Fatalf("kills = %d, want one", f.imKills)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartSeedingFailsOnNeverEvents(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture)
		want   []string
	}{
		"the restore retried after it completed": {change: func(f *s12IMRestartFixture) {
			f.afterKill[0] = imStatus{databasev1alpha1.BootstrapSeedingReplicas, 1, "", s12RetryReason,
				"on " + testIMRestartTarget + ": " + s12IMRestartedReason + "; starting attempt 2 of 3"}
			for i := 1; i < len(f.afterKill); i++ {
				f.afterKill[i].attempts = 1
			}
		}, want: []string{"the restore was retried after the kill, after it had completed and seeding had begun"}},
		"the seed restore started again without a failed attempt": {change: func(f *s12IMRestartFixture) {
			f.afterSeed[0].op = testIMSeedOp2
		}, want: []string{"the seed restore of " + testIMPeer + " was started again without a failed attempt",
			testIMSeedOp + " became " + testIMSeedOp2}},
		"seeding attempts falling": {change: func(f *s12IMRestartFixture) {
			f.afterSeed[5].attempts = 0
		}, want: []string{"seeding attempts went from 1 back to 0"}},
		"seeding attempts beyond the Operator's": {change: func(f *s12IMRestartFixture) {
			f.afterSeed[5].attempts = s12MaxAttempts
		}, want: []string{"seeding attempts reached 3"}},
		"BootstrapReady True while seeding": {change: func(f *s12IMRestartFixture) {
			f.tamper = func(_ int, killed bool, s *databasev1alpha1.CubridClusterStatus) {
				if killed && s.Bootstrap.Phase == databasev1alpha1.BootstrapSeedingReplicas {
					s.Conditions[0].Status = "True"
				}
			}
		}, want: []string{"BootstrapReady True (", string(seedingPhase)}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newS12IMRestartSeedingFixture(t)
			tc.change(f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Fail, tc.want...)
		})
	}
}

func TestRunS12InstanceManagerRestartSeedingTakesTheSeedRetryFromItsEvent(t *testing.T) {
	for name, tc := range map[string]struct {
		event  string
		result evidence.Result
	}{
		"the Event names the restart on the member": {event: testIMSeedRetryMessage, result: evidence.Pass},
		"the Event names another member": {event: "on " + testIMPeer2 + ": " + s12IMRestartedReason +
			"; starting attempt 2 of 3", result: evidence.Blocked},
		"the Event is of another attempt": {event: "on " + testIMPeer + ": " + s12IMRestartedReason +
			"; starting attempt 3 of 3", result: evidence.Blocked},
	} {
		t.Run(name, func(t *testing.T) {
			f := newS12IMRestartSeedingFixture(t)
			// The reading of SeedRestoreFailedRetrying was missed; the
			// Operator's Event of it is there.
			f.afterKill, f.afterSeed = slices.Delete(f.afterKill, 1, 2), slices.Delete(f.afterSeed, 1, 2)
			f.retryEvent, f.retryEventReason = tc.event, s12SeedRetryReason
			restart, err := imRestartOf(t, f)
			if restart.Result != tc.result {
				t.Fatalf("S12 instance-manager-restart-seeding = %+v, %v; want %s", restart, err, tc.result)
			}
			if tc.result == evidence.Blocked {
				requireIMRestart(t, restart, err, evidence.Blocked, "cause is unknown")
				return
			}
			r := readIMRestartRecord(t, f.cfg).IMRestart
			if len(r.SeedRetries) != 1 || !r.SeedRetries[0].FromEvent || r.SeedRetries[0].Message != tc.event ||
				!slices.Equal(r.SeedRetryEvents, []string{tc.event}) {
				t.Fatalf("seed retries = %+v, events = %v; want the one the Event recorded", r.SeedRetries,
					r.SeedRetryEvents)
			}
		})
	}
}

func TestRunHARefusesAnInvalidS12InstanceManagerRestartPlacement(t *testing.T) {
	for name, change := range map[string]func(*HAConfig){
		"an unknown placement": func(c *HAConfig) { c.RestoreInstanceManagerRestartAt = "activating" },
		"a placement without the variant": func(c *HAConfig) {
			c.RestoreInstanceManagerRestart, c.RestoreInstanceManagerRestartAt = false, IMRestartAtSeeding
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12IMRestartConfig(t)
			change(&cfg)
			f := newS12IMRestartFixture(t, cfg)
			if err := f.runner.RunHA(t.Context(), cfg); err == nil {
				t.Fatal("RunHA() accepted the configuration")
			}
			if len(f.writes) != 0 {
				t.Fatalf("writes = %v, want nothing done", f.writes)
			}
		})
	}
}

func TestIMRestartPlacementsHaveALaneAndAVariantEach(t *testing.T) {
	for at, want := range map[string]string{IMRestartAtRestoring: "instance-manager-restart",
		IMRestartAtPreparing: "instance-manager-restart-preparing",
		IMRestartAtSeeding:   "instance-manager-restart-seeding"} {
		if got := imRestartVariant(at); got != want {
			t.Fatalf("imRestartVariant(%s) = %s, want %s", at, got, want)
		}
		if got := imRestartLane(at); got != "lab-installed-ha-s12-"+want {
			t.Fatalf("imRestartLane(%s) = %s", at, got)
		}
	}
	if (HAConfig{}).imRestartAt() != IMRestartAtRestoring {
		t.Fatal("the default placement is not restoring")
	}
}

func TestKilledOperationScriptNamesOnlyTheTwoDirectories(t *testing.T) {
	script := killedOperationScript("op-0123", "uid-1")
	if script != "cat -- '/var/lib/cubrid/operations/op-0123.json' && echo && if [ -e "+
		"'/var/lib/cubrid/restore-staging/uid-1' ]; then echo 'staging: present'; else echo 'staging: absent'; fi" {
		t.Fatalf("script = %q", script)
	}
	for _, name := range []string{"", "../x", "a/b", "a'b", "a b", "$(x)", "-x"} {
		if plainName.MatchString(name) {
			t.Fatalf("plainName accepts %q", name)
		}
	}
}

func TestRunS12InstanceManagerRestartPreparingIsBlockedByAnotherOperationsRecord(t *testing.T) {
	f := newS12IMRestartPreparingFixture(t)
	f.opRecord = func(string, string) (string, error) {
		return testOpRecord(t, instancemanager.Operation{ID: testIMRestartOp2, State: instancemanager.OpFailed,
			FailureReason: s12IMRestartedReason, Restored: &instancemanager.OperationArtifact{
				ManifestURI: "s3://bucket/run/manifest.json"}}, "present"), nil
	}
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Blocked, "is of operation \""+testIMRestartOp2+"\"")
	if m := restart.Measurements; m["restoredAtKill"] != evidence.Unknown || m["stagingLeftAtEnd"] != evidence.Unknown {
		t.Fatalf("measurements = %v, want restoredAtKill and stagingLeftAtEnd unknown for another operation's record",
			m)
	}
}

func TestIMRestartSeedingPassReasonMarksAnInferredAttempt(t *testing.T) {
	before, after := int32(0), int32(1)
	r := &s12IMRestart{memberKill: memberKill{Pod: testIMPeer}, SeedAttemptsBefore: &before}
	if reason := imRestartSeedingPassReason(r); !strings.Contains(reason, "seeding attempt 1 (inferred) (seed "+
		"restore operation unknown)") {
		t.Fatalf("reason = %q, want the attempt marked inferred", reason)
	}
	r.SeedAttemptsAfter, r.SeedRestoreOperationIDAfter = &after, testIMSeedOp2
	if reason := imRestartSeedingPassReason(r); strings.Contains(reason, "inferred") ||
		!strings.Contains(reason, "seeding attempt 1 (seed restore operation "+testIMSeedOp2+")") {
		t.Fatalf("reason = %q, want the attempt read", reason)
	}
}
