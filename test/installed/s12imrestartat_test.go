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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
		IMRestartAtPreparing:  "instance-manager-restart-preparing",
		IMRestartAtSeeding:    "instance-manager-restart-seeding",
		IMRestartAtStarting:   "instance-manager-restart-starting",
		IMRestartAtSeedBackup: "instance-manager-restart-seed-backup"} {
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

const (
	// testIMStarting is the Operator's message while the target's operation
	// is Starting, testIMRestoredManifest the manifest the restore restored.
	testIMStarting         = "the restored database is being started on " + testIMRestartTarget
	testIMRestoredManifest = "s3://bucket/run/manifest.json"
	// testIMNotStarting is how a kill that could not be placed at starting
	// is reported.
	testIMNotStarting = "could not be placed while the restored database was being started"
)

// testIMRestored is the restored artifact the target's Instance Manager
// records once restoredb succeeded.
func testIMRestored() *instancemanager.OperationArtifact {
	return &instancemanager.OperationArtifact{ManifestURI: testIMRestoredManifest, Database: databaseName,
		CubridVersion: testEngineVersion}
}

// testOpJSON is the answer of operationRecordScript: the record alone.
func testOpJSON(t *testing.T, op instancemanager.Operation) string {
	t.Helper()
	data, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

// s12IMRestartStartingRecords are the target's records of the operations
// the fixture at starting answers: polled are the states of the killed
// operation read before the kill, one per read, the last kept; killed is its
// record read after the cluster is Ready, completing that of the next
// operation. pollErr, when set, answers every read before the kill, and
// completingErr the read of the next operation's record.
type s12IMRestartStartingRecords struct {
	polled        []instancemanager.OperationState
	polls         int
	pollErr       error
	killed        instancemanager.Operation
	completing    instancemanager.Operation
	completingErr error
}

// newS12IMRestartStartingFixture plays a restore whose Instance Manager is
// killed while the restored database is being started: the target's record
// of the operation reads Restoring twice and then Starting, at which the
// kill lands. The attempt then fails with the manager's restart, and the
// next operation takes over the data the failed one recorded as restored,
// starts it and completes. After the run, the killed operation's record is
// Failed by the restart with the restored artifact, and the completing
// one's holds the same artifact. The two readings of Restoring are recorded
// once, as one state.
func newS12IMRestartStartingFixture(t *testing.T) (*s12IMRestartFixture, *s12IMRestartStartingRecords) {
	t.Helper()
	f := newS12IMRestartFixture(t, testS12IMRestartConfigAt(t, IMRestartAtStarting))
	f.afterKill = []imStatus{
		{databasev1alpha1.BootstrapRestoring, 0, testIMRestartOp, testIMUnavailable, testIMRefused},
		{databasev1alpha1.BootstrapRestoring, 1, "", s12RetryReason, testIMRetryMessage},
		{databasev1alpha1.BootstrapPreparing, 1, testIMRestartOp2, testRestoreInProgress, testIMPreparing},
		{databasev1alpha1.BootstrapRestoring, 1, testIMRestartOp2, testRestoreInProgress, testIMStarting},
		{databasev1alpha1.BootstrapSeedingReplicas, 1, testIMRestartOp2, testSeedInProgress, testIMCopying},
		{databasev1alpha1.BootstrapComplete, 1, testIMRestartOp2, testPeersSeeded, testIMCopied},
	}
	records := &s12IMRestartStartingRecords{
		polled: []instancemanager.OperationState{instancemanager.OpRestoring, instancemanager.OpRestoring,
			instancemanager.OpStarting},
		killed: instancemanager.Operation{ID: testIMRestartOp, Kind: instancemanager.OpRestore,
			State: instancemanager.OpFailed, FailureReason: s12IMRestartedReason, Restored: testIMRestored()},
		completing: instancemanager.Operation{ID: testIMRestartOp2, Kind: instancemanager.OpRestore,
			State: instancemanager.OpCompleted, Restored: testIMRestored(), Artifact: testIMRestored()},
	}
	f.opRecord = func(pod, script string) (string, error) {
		if pod != testIMRestartTarget {
			t.Errorf("record read on %s, want the restore's target %s", pod, testIMRestartTarget)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(script, imOperationsDir+"/"+records.completing.ID+".json") &&
			records.completingErr != nil:
			return "", records.completingErr
		case strings.Contains(script, imOperationsDir+"/"+records.completing.ID+".json"):
			return testOpJSON(t, records.completing), nil
		case !strings.Contains(script, imOperationsDir+"/"+testIMRestartOp+".json"):
			t.Errorf("record read with %q, want one of the restore's operations", script)
			return "", errors.New("no such record")
		case f.killed:
			return testOpRecord(t, records.killed, "absent"), nil
		case records.pollErr != nil:
			records.polls++
			return "", records.pollErr
		}
		state := records.polled[min(records.polls, len(records.polled)-1)]
		records.polls++
		op := instancemanager.Operation{ID: testIMRestartOp, Kind: instancemanager.OpRestore, State: state}
		if state == instancemanager.OpStarting || state == instancemanager.OpCompleted {
			op.Restored = testIMRestored()
		}
		return testOpJSON(t, op), nil
	}
	return f, records
}

func TestRunS12InstanceManagerRestartStartingPassesWhenTheNextOperationTakesOverTheRestoredData(t *testing.T) {
	f, records := newS12IMRestartStartingFixture(t)
	restart, err := imRestartOf(t, f)
	if err != nil || restart.Result != evidence.Pass {
		t.Fatalf("S12 instance-manager-restart-starting = %+v, %v; want a pass", restart, err)
	}
	if f.imKills != 1 || !slices.Equal(f.killedPods, []string{testIMRestartTarget}) || records.polls != 3 {
		t.Fatalf("kills = %d on %v, polls = %d; want one kill on %s after the third read", f.imKills, f.killedPods,
			records.polls, testIMRestartTarget)
	}
	for _, rel := range []string{"record.json", "timeline.jsonl", "data-check.json", "source-data-check.json"} {
		if !slices.Contains(restart.Evidence, "S12/instance-manager-restart-starting/"+rel) {
			t.Fatalf("evidence = %v, want %s of the variant", restart.Evidence, rel)
		}
	}
	m := restart.Measurements
	if m["restoredAtKill"] != true || m["tookOverRestoredData"] != true || m["retriesAfterKill"] != float64(1) ||
		m["operationIDChanged"] != true || m["containerRestarts"] != float64(1) {
		t.Fatalf("measurements = %v", m)
	}
	r := readIMRestartRecord(t, f.cfg).IMRestart
	if r.At != IMRestartAtStarting || r.PlacingReading == nil || r.PlacingReading.At.IsZero() ||
		r.PlacingReading.State != string(instancemanager.OpStarting) || r.PlacingReading.Operation != testIMRestartOp ||
		len(r.OperationReadings) != 1 || r.OperationReadings[0].State != string(instancemanager.OpRestoring) ||
		r.KilledOperation == nil ||
		r.KilledOperation.Restored != testIMRestoredManifest || r.CompletingOperation == nil ||
		r.CompletingOperation.Restored != testIMRestoredManifest {
		t.Fatalf("restart = %+v, placing = %+v, killed = %+v, completing = %+v", r, r.PlacingReading,
			r.KilledOperation, r.CompletingOperation)
	}
	for _, want := range []string{"Instance Manager on " + testIMRestartTarget + " was killed",
		"record of operation " + testIMRestartOp + " on " + testIMRestartTarget + " was read Starting",
		"took over the data", "operation " + testIMRestartOp2 + ", attempt 1, with one retry"} {
		if reason := imRestartPassReasonAt(r); !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want %q", reason, want)
		}
	}
}

func TestRunS12InstanceManagerRestartStartingIsBlockedWhenTheKillCannotBePlaced(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture, *s12IMRestartStartingRecords)
		polls  int
		want   []string
	}{
		"the record is first seen Completed": {change: func(_ *s12IMRestartFixture, r *s12IMRestartStartingRecords) {
			r.polled = []instancemanager.OperationState{instancemanager.OpRestoring, instancemanager.OpCompleted}
		}, polls: 2, want: []string{testIMNotStarting,
			"read Completed before any reading of it Starting"}},
		"the bootstrap moves past Restoring first": {change: func(f *s12IMRestartFixture,
			r *s12IMRestartStartingRecords) {
			f.beforeKill = append(f.beforeKill, databasev1alpha1.BootstrapSeedingReplicas)
			f.noHold = true
			r.polled = []instancemanager.OperationState{instancemanager.OpRestoring}
		}, want: []string{testIMNotStarting,
			"the bootstrap was seen SeedingReplicas"}},
		// The record is read only while the bootstrap is Restoring with the
		// operation, so a bootstrap past it is seen without any exec.
		"the record read Failed for another reason": {change: func(_ *s12IMRestartFixture,
			r *s12IMRestartStartingRecords) {
			r.polled = []instancemanager.OperationState{instancemanager.OpFailed}
		}, polls: 1, want: []string{testIMNotStarting,
			"read Failed before any reading of it Starting"}},
		"the bootstrap names another operation right before the kill": {change: func(f *s12IMRestartFixture,
			r *s12IMRestartStartingRecords) {
			f.tamper = func(_ int, killed bool, s *databasev1alpha1.CubridClusterStatus) {
				f.mu.Lock()
				defer f.mu.Unlock()
				// The attempt failed and the next one started between the
				// reading of Starting and the reading right before the kill.
				if !killed && r.polls >= len(r.polled) {
					s.Bootstrap.Attempts, s.Bootstrap.OperationID = 1, testIMRestartOp3
					s.Conditions[0].Reason, s.Conditions[0].Message = s12RetryReason, testIMRetryMessage
				}
			}
		}, polls: 3, want: []string{testIMNotStarting,
			"right before it the bootstrap named operation \"" + testIMRestartOp3 + "\", not the one whose " +
				"record was read Starting"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, records := newS12IMRestartStartingFixture(t)
			tc.change(f, records)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, tc.want...)
			if f.imKills != 0 || records.polls != tc.polls {
				t.Fatalf("kills = %d, record reads before the kill = %d; want none and %d", f.imKills,
					records.polls, tc.polls)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartStartingIsBlockedWhenTheKilledRecordHoldsNoRestoredData(t *testing.T) {
	f, records := newS12IMRestartStartingFixture(t)
	records.killed.Restored = nil
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Blocked, "the kill landed before restoredb succeeded",
		"holds no restored artifact")
	if m := restart.Measurements; m["restoredAtKill"] != false || m["tookOverRestoredData"] != evidence.Unknown {
		t.Fatalf("measurements = %v, want restoredAtKill false and tookOverRestoredData unknown", m)
	}
}

func TestRunS12InstanceManagerRestartStartingFailsWhenTheDataWasRestoredAgain(t *testing.T) {
	for name, change := range map[string]func(*s12IMRestartFixture, *s12IMRestartStartingRecords){
		"the completing record holds another artifact": func(_ *s12IMRestartFixture, r *s12IMRestartStartingRecords) {
			r.completing.Restored = &instancemanager.OperationArtifact{ManifestURI: "s3://bucket/run/other.json",
				Database: databaseName, CubridVersion: testEngineVersion}
		},
		"a reading shows restoredb running for the next operation": func(f *s12IMRestartFixture,
			_ *s12IMRestartStartingRecords) {
			f.afterKill[3].message = testIMRestoredbRunning
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, records := newS12IMRestartStartingFixture(t)
			change(f, records)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Fail, "restored again instead of taken over")
			if restart.Measurements["tookOverRestoredData"] != false {
				t.Fatalf("measurements = %v, want tookOverRestoredData false", restart.Measurements)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartStartingFailsWhenTheRetriedRestoreFailsAgain(t *testing.T) {
	f, records := newS12IMRestartStartingFixture(t)
	records.completing.ID = testIMRestartOp3
	again := "on " + testIMRestartTarget + ": restore failed: start failed; starting attempt 3 of 3"
	f.afterKill = slices.Insert(f.afterKill, 4,
		imStatus{databasev1alpha1.BootstrapRestoring, 2, "", s12RetryReason, again},
		imStatus{databasev1alpha1.BootstrapRestoring, 2, testIMRestartOp3, testRestoreInProgress, testIMStarting})
	for i := 6; i < len(f.afterKill); i++ {
		f.afterKill[i].attempts, f.afterKill[i].op = 2, testIMRestartOp3
	}
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Fail, "the restore failed again after the attempt the kill ended",
		"start failed")
}

func TestValidateS12InstanceManagerRestartAcceptsStarting(t *testing.T) {
	if err := testS12IMRestartConfigAt(t, IMRestartAtStarting).validateS12InstanceManagerRestart(); err != nil {
		t.Fatalf("validateS12InstanceManagerRestart() = %v, want starting accepted", err)
	}
}

func TestRunS12InstanceManagerRestartStartingIsBlockedByTheRecordsAfterReady(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartStartingRecords)
		want   []string
	}{
		// The start ended and Completed was recorded before the kill took
		// effect: the kill interrupted nothing.
		"the killed record is Completed": {change: func(r *s12IMRestartStartingRecords) {
			r.killed.State, r.killed.FailureReason = instancemanager.OpCompleted, ""
		}, want: []string{"is Completed", "not Failed by its restart"}},
		"the completing record cannot be read": {change: func(r *s12IMRestartStartingRecords) {
			r.completingErr = errors.New("No such file or directory\ncommand terminated with exit code 1")
		}, want: []string{"which completed the restore, on " + testIMRestartTarget + " could not be read",
			"No such file or directory"}},
		"the completing record is not Completed": {change: func(r *s12IMRestartStartingRecords) {
			r.completing.State, r.completing.FailureReason = instancemanager.OpFailed, s12IMRestartedReason
		}, want: []string{"which completed the restore, is Failed", "not Completed"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, records := newS12IMRestartStartingFixture(t)
			tc.change(records)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, tc.want...)
			if restart.Measurements["tookOverRestoredData"] != evidence.Unknown {
				t.Fatalf("measurements = %v, want tookOverRestoredData unknown", restart.Measurements)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartStartingWhenRestoreLimitPassesBeforeStarting(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartStartingRecords)
		result evidence.Result
		want   []string
	}{
		// The record could not be read, so whether the database was started
		// is not known.
		"the record cannot be read": {change: func(r *s12IMRestartStartingRecords) {
			r.pollErr = errors.New(testIMRefused)
		}, result: evidence.Blocked, want: []string{"no reading before restore_limit", testIMRefused}},
		// The record was read Restoring until the limit: the restored database
		// was not started within it, which S12 requires.
		"the record stays Restoring": {change: func(r *s12IMRestartStartingRecords) {
			r.polled = []instancemanager.OperationState{instancemanager.OpRestoring}
		}, result: evidence.Fail, want: []string{"the restored database was not started within restore_limit",
			"was last read Restoring"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, records := newS12IMRestartStartingFixture(t)
			tc.change(records)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, tc.result, tc.want...)
			if f.imKills != 0 {
				t.Fatalf("kills = %d, want none", f.imKills)
			}
		})
	}
}

const (
	// testIMSeedBackupOp is the first peer's seed backup on the restored
	// member under the database's attempt 0, testIMSeedBackupOp2 under
	// attempt 1, and testIMSeedBackupOp3 the second peer's.
	testIMSeedBackupOp  = "op-seed-backup-0"
	testIMSeedBackupOp2 = "op-seed-backup-1"
	testIMSeedBackupOp3 = "op-seed-backup-2"
	// testIMSeedBackupRetryMessage is the Operator's message of the seed
	// attempt whose backup the kill ended.
	testIMSeedBackupRetryMessage = "on " + testIMRestartTarget + ": " + s12IMRestartedReason +
		"; starting attempt 2 of 3"
	testIMBackingUp = "backing up on " + testIMRestartTarget
	// testIMNotAtSeedBackup is how a kill that could not be placed at
	// seed-backup is reported.
	testIMNotAtSeedBackup = "could not be placed while the seed backup ran"
	// testIMNoRecord is what a member answers for a record it does not
	// hold, and testIMNotFailedByRestart how a killed operation's record
	// that ended otherwise is reported.
	testIMNoRecord           = "No such file or directory"
	testIMNotFailedByRestart = "not Failed by its restart"
)

// s12IMRestartSeedBackupRecords are the source's records of the seed backup
// the fixture at seed-backup answers: polled are its states read before the
// kill, one per read, the last kept; killed is its record read after the
// cluster is Ready, and killedErr, when set, answers that read. net is the
// source's network namespace: every read of the record Uploading puts
// uploadPerRead bytes through the rate limit, when it is installed.
type s12IMRestartSeedBackupRecords struct {
	polled        []instancemanager.OperationState
	polls         int
	killed        instancemanager.Operation
	killedErr     error
	net           *fakeNetns
	uploadPerRead uint64
	// sendAll puts the bytes through the limit at every read that has not
	// ended, RunningBackup too.
	sendAll bool
}

// newS12IMRestartSeedBackupFixture plays a recovery whose restore completed
// on the target and whose first seed backup, on the target, is interrupted
// by the kill of the target's Instance Manager. The target's traffic to the
// object store passes a rate limit the run installs; the source's record of
// the backup reads RunningBackup, then Uploading with testUploadPerRead
// bytes through the limit per read, and once 512 KiB passed the kill lands, the seed attempt fails with
// SeedBackupFailedRetrying naming the manager's restart, and the next seed
// attempt takes a new backup, seeds the first peer and then the second.
// The restore itself keeps its operation and is not run again. After the
// run, the killed backup's record is Failed by the restart.
func newS12IMRestartSeedBackupFixture(t *testing.T) (*s12IMRestartFixture, *s12IMRestartSeedBackupRecords) {
	t.Helper()
	// The object store's Service, headless as on the VM lab, which the limit
	// acts on.
	f := newS12IMRestartFixture(t, testS12IMRestartConfigAt(t, IMRestartAtSeedBackup), testStoreServiceObject(),
		testStoreSlice("seaweedfs-a", testStoreIP))
	seeding := databasev1alpha1.BootstrapSeedingReplicas
	f.beforeKill = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing,
		databasev1alpha1.BootstrapRestoring, seeding, seeding}
	f.holdStep = 3
	f.beforeSeed = []*imSeed{nil, nil,
		{member: testIMPeer, reason: testIMSeedBackup, message: testIMBackingUp},
		{member: testIMPeer, backup: testIMSeedBackupOp, reason: testIMSeedBackup, message: testIMBackingUp}}
	f.afterKill = []imStatus{
		{seeding, 0, testIMRestartOp, testIMUnavailable, testIMRefused},
		{seeding, 0, testIMRestartOp, s12SeedBackupRetryReason, testIMSeedBackupRetryMessage},
		{seeding, 0, testIMRestartOp, testIMSeedBackup, testIMBackingUp},
		{seeding, 0, testIMRestartOp, testSeedInProgress, "restoring on " + testIMPeer},
		{seeding, 0, testIMRestartOp, testSeedInProgress, "restoring on " + testIMPeer2},
		{databasev1alpha1.BootstrapComplete, 0, testIMRestartOp, testPeersSeeded, testIMCopied},
	}
	f.afterSeed = []*imSeed{
		{member: testIMPeer, backup: testIMSeedBackupOp},
		{attempts: 1},
		{attempts: 1, member: testIMPeer, backup: testIMSeedBackupOp2},
		{attempts: 1, member: testIMPeer, backup: testIMSeedBackupOp2, op: testIMSeedOp2},
		{attempts: 1, member: testIMPeer2, backup: testIMSeedBackupOp3, op: testIMSeedOp3, seeded: []string{testIMPeer}},
		{attempts: 1, seeded: []string{testIMPeer, testIMPeer2}},
	}
	records := &s12IMRestartSeedBackupRecords{
		polled: []instancemanager.OperationState{instancemanager.OpRunningBackup, instancemanager.OpUploading},
		killed: instancemanager.Operation{ID: testIMSeedBackupOp, Kind: instancemanager.OpBackup,
			State: instancemanager.OpFailed, FailureReason: s12IMRestartedReason},
		net: &fakeNetns{member: testIMRestartTarget}, uploadPerRead: testUploadPerRead,
	}
	f.netfault, f.ephemeralAdded = records.net.answer, addEphemeralContainer
	created := f.imRestartCreated
	f.imRestartCreated = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
		if err := created(ctx, c, cluster); err != nil {
			return err
		}
		// The target's IP is the one its network namespace answers.
		pod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: testIMRestartTarget}, pod); err != nil {
			return err
		}
		pod.Status.PodIP = testInterruptedPodIP
		return c.Status().Update(ctx, pod)
	}
	f.opRecord = func(pod, script string) (string, error) {
		if pod != testIMRestartTarget || !strings.Contains(script, imOperationsDir+"/"+testIMSeedBackupOp+".json") {
			t.Errorf("record read on %s with %q, want the seed backup's on the restore's target", pod, script)
			return "", errors.New("no such record")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.killed {
			if records.killedErr != nil {
				return "", records.killedErr
			}
			return testOpJSON(t, records.killed), nil
		}
		state := records.polled[min(records.polls, len(records.polled)-1)]
		records.polls++
		if state == instancemanager.OpUploading || (records.sendAll && !state.IsTerminal()) {
			n := records.net
			n.mu.Lock()
			if n.throttleIP != "" {
				n.send(n.throttleIP, records.uploadPerRead)
			}
			n.mu.Unlock()
		}
		return testOpJSON(t, instancemanager.Operation{ID: testIMSeedBackupOp, Kind: instancemanager.OpBackup,
			State: state}), nil
	}
	return f, records
}

func TestRunS12InstanceManagerRestartSeedBackupPassesWhenTheSeedRetryTakesANewBackup(t *testing.T) {
	f, records := newS12IMRestartSeedBackupFixture(t)
	// The readings after the kill the fake Operator had answered when the
	// limit was removed: the retry runs without it.
	removedAt := -1
	records.net.onUnthrottle = func() {
		f.mu.Lock()
		removedAt = f.imAfter
		f.mu.Unlock()
	}
	restart, err := imRestartOf(t, f)
	if err != nil || restart.Result != evidence.Pass {
		t.Fatalf("S12 instance-manager-restart-seed-backup = %+v, %v; want a pass", restart, err)
	}
	// One reading of RunningBackup, then two of Uploading: the second puts
	// the upload past 512 KiB through the limit.
	if f.imKills != 1 || !slices.Equal(f.killedPods, []string{testIMRestartTarget}) || records.polls != 3 ||
		f.recordReads != 4 {
		t.Fatalf("kills = %d on %v, record reads = %d (%d before the kill); want one kill on %s after three reads",
			f.imKills, f.killedPods, f.recordReads, records.polls, testIMRestartTarget)
	}
	if !slices.Contains(restart.Evidence, "S12/instance-manager-restart-seed-backup/record.json") {
		t.Fatalf("evidence = %v, want the variant's record", restart.Evidence)
	}
	m := restart.Measurements
	if m["seedRetriesAfterKill"] != float64(1) || m["seedAttemptsAdded"] != float64(1) ||
		m["seedBackupOperationChanged"] != true || m["killedBackupFailedByRestart"] != true ||
		m["retriesAfterKill"] != float64(0) || m["attemptsAdded"] != float64(0) || m["operationIDChanged"] != false ||
		m["containerRestarts"] != float64(1) || m["uploadBytesBeforeKill"] != float64(2*testUploadPerRead) {
		t.Fatalf("measurements = %v", m)
	}
	if removedAt < 0 || removedAt >= len(f.afterKill)-1 {
		t.Fatalf("the limit was removed after %d readings after the kill, want before the reading of Complete",
			removedAt)
	}
	r := readIMRestartRecord(t, f.cfg).IMRestart
	requireSeedBackupRecord(t, r)
	requireSeedSourceLimit(t, r, records.net)
	for _, want := range []string{"Instance Manager on " + testIMRestartTarget + " was killed",
		"seed backup operation " + testIMSeedBackupOp, "for " + testIMPeer, s12SeedBackupRetryReason,
		"seed backup operation " + testIMSeedBackupOp2, "the restore kept operation " + testIMRestartOp} {
		if reason := imRestartPassReasonAt(r); !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want %q", reason, want)
		}
	}
}

// requireSeedBackupRecord fails unless the record of a pass at seed-backup
// names the source killed, the peer, both seed backups, the one seed retry,
// the reading that placed the kill and the killed backup's record.
func requireSeedBackupRecord(t *testing.T, r *s12IMRestart) {
	t.Helper()
	if r.At != IMRestartAtSeedBackup || r.Pod != testIMRestartTarget || r.SeedPeer != testIMPeer ||
		r.SeedBackupOperationIDBefore != testIMSeedBackupOp || r.SeedBackupOperationIDAfter != testIMSeedBackupOp2 ||
		r.SeedAttemptsBefore == nil || *r.SeedAttemptsBefore != 0 || r.SeedAttemptsAfter == nil ||
		*r.SeedAttemptsAfter != 1 || len(r.SeedRetries) != 1 || r.SeedRetries[0].Reason != s12SeedBackupRetryReason ||
		r.PlacingReading == nil || r.PlacingReading.State != string(instancemanager.OpUploading) ||
		r.PlacingReading.Operation != testIMSeedBackupOp || r.KilledSeedBackup == nil ||
		r.KilledSeedBackup.State != string(instancemanager.OpFailed) {
		t.Fatalf("restart = %+v, placing = %+v, killed backup = %+v", r, r.PlacingReading, r.KilledSeedBackup)
	}
}

// requireSeedSourceLimit fails unless the rate limit on the source's traffic
// to the object store was confirmed before the kill, its counters were read
// at the first reading of the backup and at the kill, 512 KiB passed it
// between them, and it was seen removed after the kill.
func requireSeedSourceLimit(t *testing.T, r *s12IMRestart, n *fakeNetns) {
	t.Helper()
	f := r.Throttle
	if f == nil || f.Target != testIMRestartTarget || f.StoreIP != testStoreIP || f.LimitConfirmedAt == nil ||
		f.RemovedAt == nil || r.KilledAt == nil || !f.LimitConfirmedAt.Before(*r.KilledAt) ||
		!f.RemovedAt.After(*r.KilledAt) || r.UploadBaseline == nil || r.AtKill == nil ||
		r.SentBeforeKill < interruptedUploadSeen || r.PodSecurity[podSecurityEnforce] != podSecurityPrivileged {
		t.Fatalf("throttle = %+v, baseline = %+v, at kill = %+v, sent = %d, Pod Security = %v", f, r.UploadBaseline,
			r.AtKill, r.SentBeforeKill, r.PodSecurity)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.throttleIP != "" || !slices.Contains(n.ops, "throttle") || !slices.Contains(n.ops, "unthrottle") {
		t.Fatalf("limit at the end = %q, ops = %v; want it installed and removed", n.throttleIP, n.ops)
	}
}

func TestS12InstanceManagerRestartSeedBackupNeedsAPrivilegedNamespace(t *testing.T) {
	if !testS12IMRestartConfigAt(t, IMRestartAtSeedBackup).privileged() {
		t.Fatal("seed-backup does not make the run namespace privileged")
	}
	for _, at := range []string{IMRestartAtRestoring, IMRestartAtPreparing, IMRestartAtSeeding, IMRestartAtStarting} {
		if testS12IMRestartConfigAt(t, at).privileged() {
			t.Fatalf("%s makes the run namespace privileged", at)
		}
	}
}

func TestRunS12InstanceManagerRestartSeedBackupWithTheRateLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture, *s12IMRestartSeedBackupRecords)
		kills  int
		want   []string
	}{
		// The bootstrap is first seen seeding: the seed backup may have run
		// before the limit could be in place.
		"the limit cannot be placed before seeding": {change: func(f *s12IMRestartFixture,
			_ *s12IMRestartSeedBackupRecords) {
			f.beforeKill = f.beforeKill[2:]
			f.beforeSeed = f.beforeSeed[2:]
			f.holdStep = 1
		}, want: []string{"the rate limit could not be placed before the seed backup",
			"the bootstrap was first seen SeedingReplicas"}},
		// The backup completes before 512 KiB passed the limit.
		"the backup completes under the limit first": {change: func(_ *s12IMRestartFixture,
			r *s12IMRestartSeedBackupRecords) {
			r.polled = []instancemanager.OperationState{instancemanager.OpRunningBackup,
				instancemanager.OpUploading, instancemanager.OpCompleted}
		}, want: []string{testIMNotAtSeedBackup, "read Completed before the kill was placed"}},
		// Uploading is never read, only RunningBackup, until the record ends.
		"no reading of Uploading": {change: func(_ *s12IMRestartFixture, r *s12IMRestartSeedBackupRecords) {
			r.polled = []instancemanager.OperationState{instancemanager.OpRunningBackup,
				instancemanager.OpRunningBackup, instancemanager.OpCompleted}
		}, want: []string{testIMNotAtSeedBackup, "read Completed"}},
		"the limit not seen removed": {change: func(_ *s12IMRestartFixture, r *s12IMRestartSeedBackupRecords) {
			r.net.ignoreUnthrottle = true
		}, kills: 1, want: []string{"the rate limit was not seen removed"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, records := newS12IMRestartSeedBackupFixture(t)
			tc.change(f, records)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, tc.want...)
			if f.imKills != tc.kills {
				t.Fatalf("kills = %d, want %d", f.imKills, tc.kills)
			}
			n := records.net
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.throttleIP != "" && !n.ignoreUnthrottle {
				t.Fatalf("the limit was left in place: ops = %v", n.ops)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartSeedBackupIsBlockedWhenTheKillCannotBePlaced(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture, *s12IMRestartSeedBackupRecords)
		want   []string
	}{
		"the backup's record is first read Completed": {change: func(_ *s12IMRestartFixture,
			r *s12IMRestartSeedBackupRecords) {
			r.polled = []instancemanager.OperationState{instancemanager.OpCompleted}
		}, want: []string{testIMNotAtSeedBackup, "read Completed before the kill was placed"}},
		"a seed restore is recorded first": {change: func(f *s12IMRestartFixture, _ *s12IMRestartSeedBackupRecords) {
			f.beforeSeed[3].op = testIMSeedOp
		}, want: []string{testIMNotAtSeedBackup, "a seed restore " + testIMSeedOp + " of " + testIMPeer +
			" was recorded before"}},
		"the bootstrap completes first": {change: func(f *s12IMRestartFixture, _ *s12IMRestartSeedBackupRecords) {
			f.beforeKill = append(f.beforeKill[:3], databasev1alpha1.BootstrapComplete)
			f.beforeSeed[3] = &imSeed{seeded: []string{testIMPeer, testIMPeer2}}
		}, want: []string{testIMNotAtSeedBackup, "the bootstrap was seen Complete"}},
		// The seeding names a source other than the restored member before
		// its backup's record is read: nothing is read or killed there.
		"the source is not the restored member": {change: func(f *s12IMRestartFixture,
			_ *s12IMRestartSeedBackupRecords) {
			f.tamper = func(_ int, killed bool, s *databasev1alpha1.CubridClusterStatus) {
				if !killed && len(s.Databases) > 0 && s.Databases[0].Seeding != nil {
					s.Databases[0].Seeding.Source = testIMPeer2
				}
			}
		}, want: []string{testIMNotAtSeedBackup, "the seed backup's source is " + testIMPeer2 +
			", not the restored member " + testIMRestartTarget}},
	} {
		t.Run(name, func(t *testing.T) {
			f, records := newS12IMRestartSeedBackupFixture(t)
			tc.change(f, records)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, tc.want...)
			if f.imKills != 0 {
				t.Fatalf("kills = %d, want none", f.imKills)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartSeedBackupIsBlockedByTheKilledBackupsRecord(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartSeedBackupRecords)
		want   []string
	}{
		"the killed backup's record cannot be read": {change: func(r *s12IMRestartSeedBackupRecords) {
			r.killedErr = errors.New(testIMNoRecord + "\ncommand terminated with exit code 1")
		}, want: []string{"the record of the seed backup " + testIMSeedBackupOp + " on " + testIMRestartTarget +
			" could not be read", testIMNoRecord}},
		"the backup completed": {change: func(r *s12IMRestartSeedBackupRecords) {
			r.killed.State, r.killed.FailureReason = instancemanager.OpCompleted, ""
		}, want: []string{"is Completed", testIMNotFailedByRestart}},
	} {
		t.Run(name, func(t *testing.T) {
			f, records := newS12IMRestartSeedBackupFixture(t)
			tc.change(records)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, tc.want...)
			if restart.Measurements["killedBackupFailedByRestart"] == true {
				t.Fatalf("measurements = %v, want killedBackupFailedByRestart not true", restart.Measurements)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartSeedBackupFails(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture)
		want   []string
	}{
		"the restore retried after the kill": {change: func(f *s12IMRestartFixture) {
			f.afterKill[1] = imStatus{databasev1alpha1.BootstrapSeedingReplicas, 1, "", s12RetryReason,
				"on " + testIMRestartTarget + ": " + s12IMRestartedReason + "; starting attempt 2 of 3"}
			for i := 2; i < len(f.afterKill); i++ {
				f.afterKill[i].attempts = 1
			}
		}, want: []string{"the restore was retried after the kill, after it had completed and seeding had begun"}},
		"restoredb running after the kill": {change: func(f *s12IMRestartFixture) {
			f.afterKill[2] = imStatus{databasev1alpha1.BootstrapRestoring, 0, testIMRestartOp, testRestoreInProgress,
				testIMRestoredbRunning}
		}, want: []string{"the restore ran again after the kill", testIMRestoredbRunning}},
		"a second seed retry": {change: func(f *s12IMRestartFixture) {
			again := "on " + testIMRestartTarget + ": backup failed: upload: " + testReset + "; starting attempt 3 of 3"
			f.afterKill = slices.Insert(f.afterKill, 3, imStatus{databasev1alpha1.BootstrapSeedingReplicas, 0,
				testIMRestartOp, s12SeedBackupRetryReason, again})
			f.afterSeed = slices.Insert(f.afterSeed, 3, &imSeed{attempts: 2})
			for i := 4; i < len(f.afterSeed); i++ {
				f.afterSeed[i].attempts = 2
			}
		}, want: []string{"the seeding failed again after the seed attempt the kill ended", testReset}},
		"the next attempt names the killed backup": {change: func(f *s12IMRestartFixture) {
			f.afterSeed[2].backup, f.afterSeed[3].backup = testIMSeedBackupOp, testIMSeedBackupOp
		}, want: []string{"the seed backup operation " + testIMSeedBackupOp + " the kill ended is the one recorded"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := newS12IMRestartSeedBackupFixture(t)
			tc.change(f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Fail, tc.want...)
		})
	}
}

func TestValidateS12InstanceManagerRestartAcceptsSeedBackup(t *testing.T) {
	if err := testS12IMRestartConfigAt(t, IMRestartAtSeedBackup).validateS12InstanceManagerRestart(); err != nil {
		t.Fatalf("validateS12InstanceManagerRestart() = %v, want seed-backup accepted", err)
	}
}

// tamperSeedingBeforeKill changes the seeding of the reading right before the
// kill, after the reading of the backup's record that placed it (the third,
// one more than r.polled lists): change gets the first database's seeding of
// each such reading.
func tamperSeedingBeforeKill(f *s12IMRestartFixture, r *s12IMRestartSeedBackupRecords,
	change func(*databasev1alpha1.DatabaseSeeding)) {
	f.tamper = func(_ int, killed bool, s *databasev1alpha1.CubridClusterStatus) {
		f.mu.Lock()
		polled := r.polls
		f.mu.Unlock()
		if !killed && polled >= len(r.polled)+1 && len(s.Databases) > 0 && s.Databases[0].Seeding != nil {
			change(s.Databases[0].Seeding)
		}
	}
}

func TestRunS12InstanceManagerRestartSeedBackupIsBlockedWhenTheSourceChangesRightBeforeTheKill(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*databasev1alpha1.DatabaseSeeding)
		want   string
	}{
		"the source is the member being seeded": {change: func(s *databasev1alpha1.DatabaseSeeding) {
			s.Source = s.Member
		}, want: "the seed backup's source, " + testIMPeer + ", is the member being seeded"},
		"the source is not the restored member": {change: func(s *databasev1alpha1.DatabaseSeeding) {
			s.Source = testIMPeer2
		}, want: "the seed backup's source is " + testIMPeer2 + ", not the restored member " + testIMRestartTarget},
	} {
		t.Run(name, func(t *testing.T) {
			f, records := newS12IMRestartSeedBackupFixture(t)
			tamperSeedingBeforeKill(f, records, tc.change)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, tc.want)
			if f.imKills != 0 || records.polls != 3 {
				t.Fatalf("kills = %d, record reads = %d; want none and three", f.imKills, records.polls)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartSeedBackupIsBlockedWhenTheRetryDoesNotShowTheRestart(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture)
		want   []string
	}{
		// The retry the readings show is of a seed restore, not of the
		// killed seed backup.
		"a seed restore retry": {change: func(f *s12IMRestartFixture) {
			f.afterKill[1].reason = s12SeedRetryReason
		}, want: []string{"ended with " + s12SeedRetryReason, "not with a seed backup on " + testIMRestartTarget}},
		"a retry on another member": {change: func(f *s12IMRestartFixture) {
			f.afterKill[1].message = "on " + testIMPeer2 + ": " + s12IMRestartedReason + "; starting attempt 2 of 3"
		}, want: []string{"not with a seed backup on " + testIMRestartTarget}},
		// The backup had ended before the kill took effect: the peer was
		// restored from it under the same attempt.
		"no seed retry": {change: func(f *s12IMRestartFixture) {
			f.afterKill = slices.Delete(f.afterKill, 1, 3)
			f.afterSeed = []*imSeed{
				{member: testIMPeer, backup: testIMSeedBackupOp},
				{member: testIMPeer, backup: testIMSeedBackupOp, op: testIMSeedOp},
				{member: testIMPeer2, backup: testIMSeedBackupOp3, op: testIMSeedOp3, seeded: []string{testIMPeer}},
				{seeded: []string{testIMPeer, testIMPeer2}},
			}
		}, want: []string{"the kill did not interrupt the seed backup", testIMSeedBackupOp + " had ended"}},
		// No reading after the kill showed the peer's seed backup under the
		// next attempt, so whether it is a new one is not known.
		"the next seed backup never seen": {change: func(f *s12IMRestartFixture) {
			for _, seed := range f.afterSeed[2:4] {
				seed.backup = ""
			}
		}, want: []string{"no reading after the kill showed a seed backup for " + testIMPeer +
			" under the next seed attempt"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := newS12IMRestartSeedBackupFixture(t)
			tc.change(f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, tc.want...)
		})
	}
}

func TestRunS12InstanceManagerRestartSeedBackupPlacesTheKillOnlyWhileUploading(t *testing.T) {
	f, records := newS12IMRestartSeedBackupFixture(t)
	// 512 KiB pass the limit while the record still reads RunningBackup: the
	// kill waits for the first reading of Uploading.
	records.sendAll = true
	records.polled = []instancemanager.OperationState{instancemanager.OpRunningBackup,
		instancemanager.OpRunningBackup, instancemanager.OpRunningBackup, instancemanager.OpUploading}
	restart, err := imRestartOf(t, f)
	if err != nil || restart.Result != evidence.Pass {
		t.Fatalf("S12 instance-manager-restart-seed-backup = %+v, %v; want a pass", restart, err)
	}
	r := readIMRestartRecord(t, f.cfg).IMRestart
	if records.polls != 4 || r.PlacingReading == nil || r.PlacingReading.State != string(instancemanager.OpUploading) {
		t.Fatalf("record reads before the kill = %d, placing = %+v; want the kill at the fourth, Uploading",
			records.polls, r.PlacingReading)
	}
}
