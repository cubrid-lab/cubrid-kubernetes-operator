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
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	testIMRestartUID    = "imrestart-cluster"
	testIMRestartTarget = "ha-imrestart-0"
	testIMRestartSlave  = "ha-imrestart-2"
	testIMRestartSecret = "object-storage-imrestart"
	testIMRestartOp     = "op-attempt-0"
	testIMRestartOp2    = "op-attempt-1"
	// testIMUnavailable is the BootstrapReady reason while the target's
	// Instance Manager does not answer.
	testIMUnavailable = "InstanceManagerUnavailable"
	// testIMRetryMessage is the Operator's message of the attempt the kill
	// ended.
	testIMRetryMessage = "on " + testIMRestartTarget + ": " + s12IMRestartedReason + "; starting attempt 2 of 3"
)

func testS12IMRestartConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS12Config(t)
	cfg.RestoreInstanceManagerRestart = true
	return cfg
}

// imStatus is the bootstrap the fake Operator reports at one reading.
type imStatus struct {
	phase    databasev1alpha1.BootstrapPhase
	attempts int32
	op       string
	reason   string
	message  string
}

// s12IMRestartFixture plays, after S12 exact, an Operator that restores
// S11's artifact into ha-imrestart and keeps its bootstrap Restoring until
// the target member's Instance Manager is killed, then reports what
// afterKill lists; a kubelet that starts the killed container again on the
// same Pod; CUBRID on ha-imrestart's members and the client writing through
// it.
type s12IMRestartFixture struct {
	*s12Fixture
	// beforeKill are the phases reported until the kill, one per reading,
	// the last kept; Restoring is kept until the kill unless noHold is set.
	// afterKill are the statuses of the readings after it, the last kept.
	// tamper changes the status of a reading.
	beforeKill []databasev1alpha1.BootstrapPhase
	noHold     bool
	afterKill  []imStatus
	tamper     func(read int, killed bool, s *databasev1alpha1.CubridClusterStatus)
	// killAnswer answers the kill command; refuse leaves the Pod as it was.
	// noRestart leaves the container running; endOnly ends it without a new
	// one; extraRestarts adds restarts after the kill; replacePod gives the
	// Pod another UID.
	killAnswer    func() (string, error)
	refuse        bool
	noRestart     bool
	endOnly       bool
	extraRestarts int32
	replacePod    bool
	// exitCode and exitReason are the ended container's, 137 and Error
	// unless set; retryEvent, when set, is the message of a
	// RestoreFailedRetrying Event of ha-imrestart recorded at the kill.
	exitCode   int32
	exitReason string
	retryEvent string
	// dropMarkerOnIM is a member without the marker written through
	// ha-imrestart.
	dropMarkerOnIM string

	imCluster          *databasev1alpha1.CubridCluster
	imReads, imStep    int
	imAfter            int
	killed             bool
	imKills            int
	killedPods         []string
	imWrites           []string
	targetMemberStatus string
}

func newS12IMRestartFixture(t *testing.T, cfg HAConfig) *s12IMRestartFixture {
	t.Helper()
	f := &s12IMRestartFixture{s12Fixture: newS12Fixture(t, cfg),
		beforeKill: []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing,
			databasev1alpha1.BootstrapRestoring},
		afterKill: []imStatus{
			{databasev1alpha1.BootstrapRestoring, 0, testIMRestartOp, testIMUnavailable, "connection refused"},
			{databasev1alpha1.BootstrapRestoring, 1, "", s12RetryReason, testIMRetryMessage},
			{databasev1alpha1.BootstrapRestoring, 1, testIMRestartOp2, testRestoreInProgress,
				"restoredb is running on " + testIMRestartTarget},
			{databasev1alpha1.BootstrapSeedingReplicas, 1, testIMRestartOp2, testSeedInProgress,
				"copying to the other members"},
			{databasev1alpha1.BootstrapComplete, 1, testIMRestartOp2, testPeersSeeded, "copied"},
		}}
	for _, member := range clusterMembers(s12IMRestartClusterName) {
		f.roles[member] = roleSlave
	}
	f.roles[testIMRestartTarget] = roleMaster
	f.imRestartCreated = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
		cluster.UID, cluster.Generation = testIMRestartUID, 1
		f.mu.Lock()
		f.imCluster = cluster.DeepCopy()
		f.mu.Unlock()
		if err := c.Create(ctx, cluster); err != nil {
			return err
		}
		return f.createChildren(ctx, c, cluster)
	}
	f.imRestartRead = f.advanceIMRestart
	f.memberHook = f.kill
	s12Client, s12Markers := f.clientHook, f.markersOf
	f.clientHook = func(script string) (string, error, bool) {
		if !strings.Contains(script, "workload marker "+s12IMRestartMarker+" ") {
			return s12Client(script)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.imWrites = append(f.imWrites, script)
		return "marker " + s12IMRestartMarker + " written\n", nil, true
	}
	f.markersOf = func(pod string) []string {
		rows := s12Markers(pod)
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasPrefix(pod, s12IMRestartClusterName+"-") && len(f.imWrites) > 0 && pod != f.dropMarkerOnIM {
			_, rest, _ := strings.Cut(f.imWrites[0], "workload marker "+s12IMRestartMarker+" ")
			rows = append(rows, s12IMRestartMarker+"="+strings.Fields(rest)[0])
		}
		return rows
	}
	return f
}

// kill plays the kubelet when the run kills a member's Instance Manager: the
// database container ends with exit code 137 and runs again on the same Pod.
func (f *s12IMRestartFixture) kill(ctx context.Context, pod, script string) (string, error, bool) {
	if !strings.Contains(script, "kill -KILL") {
		return "", nil, false
	}
	if script != imKillScript {
		f.t.Errorf("kill script = %q, want imKillScript", script)
	}
	f.mu.Lock()
	f.imKills++
	f.killedPods = append(f.killedPods, pod)
	answer, refuse := f.killAnswer, f.refuse
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
	if f.replacePod {
		if err := c.Delete(ctx, current); err != nil {
			f.t.Error(err)
		}
		current.UID, current.ResourceVersion = types.UID(pod+"-again"), ""
		if err := c.Create(ctx, current); err != nil {
			f.t.Error(err)
		}
	} else if !f.noRestart {
		finished := metav1.NewTime(time.Now().Truncate(time.Second))
		code, reason := cmp.Or(f.exitCode, 137), cmp.Or(f.exitReason, "Error")
		status := &current.Status.ContainerStatuses[0]
		old := status.ContainerID
		status.RestartCount += 1 + f.extraRestarts
		status.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: reason, ContainerID: old, FinishedAt: finished}}
		status.ContainerID = old + "-restarted"
		status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: finished}}
		if f.endOnly {
			status.ContainerID, status.RestartCount, status.LastTerminationState = old, status.RestartCount-1,
				corev1.ContainerState{}
			status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code,
				Reason: reason, ContainerID: old, FinishedAt: finished}}
		}
		if err := c.Status().Update(ctx, current); err != nil {
			f.t.Error(err)
		}
	}
	if f.retryEvent != "" {
		event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: s12IMRestartClusterName + ".retry",
			Namespace: f.cfg.Namespace}, InvolvedObject: corev1.ObjectReference{Kind: "CubridCluster",
			Name: s12IMRestartClusterName, UID: testIMRestartUID}, Type: corev1.EventTypeWarning,
			Reason: s12RetryReason, Message: f.retryEvent}
		if err := c.Create(ctx, event); err != nil {
			f.t.Error(err)
		}
	}
	f.mu.Lock()
	f.killed = true
	f.mu.Unlock()
	if answer != nil {
		out, err := answer()
		return out, err, true
	}
	return "", errors.New("command terminated with exit code 137"), true
}

// advanceIMRestart writes the status of ha-imrestart's next reading, as the
// Operator would: BootstrapReady and Ready are True only when the bootstrap
// is Complete.
func (f *s12IMRestartFixture) advanceIMRestart(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, key, cluster); err != nil {
		return nil
	}
	f.mu.Lock()
	f.imReads++
	read, killed := f.imReads, f.killed
	st := imStatus{phase: f.beforeKill[min(f.imStep, len(f.beforeKill)-1)], op: testIMRestartOp,
		reason: testRestoreInProgress, message: "restoredb is running on " + testIMRestartTarget}
	if killed {
		st = f.afterKill[min(f.imAfter, len(f.afterKill)-1)]
		f.imAfter++
	} else if st.phase != databasev1alpha1.BootstrapRestoring || f.noHold {
		f.imStep++
	}
	tamper := f.tamper
	f.mu.Unlock()
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	status := metav1.ConditionFalse
	if st.phase == databasev1alpha1.BootstrapComplete {
		status = metav1.ConditionTrue
	}
	s := &cluster.Status
	s.ObservedGeneration = 1
	s.Bootstrap = &databasev1alpha1.BootstrapStatus{Mode: testRecoveryMode, Phase: st.phase, Attempts: st.attempts,
		ManifestURI: cluster.Spec.Bootstrap.Recovery.ManifestURI, TargetMember: testIMRestartTarget,
		OperationID: st.op}
	if f.targetMemberStatus != "" {
		s.Bootstrap.TargetMember = f.targetMemberStatus
	}
	boot := metav1.Condition{Type: bootstrapReadyCondition, Status: status, Reason: st.reason, Message: st.message,
		LastTransitionTime: now}
	if st.phase == databasev1alpha1.BootstrapComplete {
		s.CurrentPrimary = testIMRestartTarget
		s.Databases = []databasev1alpha1.DatabaseStatus{{Name: databaseName, Phase: testDatabaseCreated,
			PrimaryCreated: true, HAConfigured: true}}
	}
	s.Conditions = []metav1.Condition{boot}
	for _, condition := range startingConditions {
		s.Conditions = append(s.Conditions, metav1.Condition{Type: condition, Status: status, Reason: testConditionReason,
			LastTransitionTime: now})
	}
	if tamper != nil {
		tamper(read, killed, s)
	}
	if err := c.Update(ctx, cluster); err != nil {
		f.t.Error(err)
	}
	return nil
}

func imRestartScenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s12IMRestartLane || len(summary.Scenarios) != 4 || summary.Scenarios[3].ID != s12ID ||
		summary.Scenarios[3].Variant != s12IMRestartVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete, S12/exact and S12/instance-manager-restart in lane %s",
			summary, s12IMRestartLane)
	}
	return summary.Scenarios
}

// imRestartOf runs the fixture and returns S12 instance-manager-restart,
// requiring LAB01, S11 and S12 exact passed.
func imRestartOf(t *testing.T, f *s12IMRestartFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	scenarios := imRestartScenarios(t, f.cfg.EvidenceDir)
	for _, s := range scenarios[:3] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01, S11 and S12 exact passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, scenarios[2], "S12/exact")
	requireOwnEvidence(t, scenarios[3], "S12/instance-manager-restart")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	return scenarios[3], err
}

func readIMRestartRecord(t *testing.T, dir string) s12IMRestartRecord {
	t.Helper()
	var rec s12IMRestartRecord
	data, err := os.ReadFile(filepath.Join(dir, s12IMRestartRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.IMRestart == nil {
		t.Fatalf("S12 instance-manager-restart record = %s, %v", data, err)
	}
	return rec
}

// requireIMRestart fails unless S12 instance-manager-restart ended as result
// with every part of want in its reason.
func requireIMRestart(t *testing.T, restart evidence.Scenario, err error, result evidence.Result, want ...string) {
	t.Helper()
	if err == nil || restart.Result != result {
		t.Fatalf("S12 instance-manager-restart = %+v, %v; want %s", restart, err, result)
	}
	for _, w := range want {
		if !strings.Contains(restart.Reason, w) {
			t.Fatalf("S12 instance-manager-restart reason = %q, want %q", restart.Reason, w)
		}
	}
}

func TestRunS12InstanceManagerRestartPassesWhenTheRetryCompletesTheRestore(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	restart, err := imRestartOf(t, f)
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if summary := readSummary(t, cfg.EvidenceDir); !summary.Passed || restart.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want every scenario passed", summary)
	}
	if f.imKills != 1 || !slices.Equal(f.killedPods, []string{testIMRestartTarget}) {
		t.Fatalf("kills = %d on %v, want one on %s", f.imKills, f.killedPods, testIMRestartTarget)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,secret,restore-cluster,secret,"+
		"imrestart-cluster" {
		t.Fatalf("writes = %v", f.writes)
	}
	rec := readIMRestartRecord(t, cfg.EvidenceDir)
	reason := imRestartPassReason(rec.IMRestart)
	for _, want := range []string{"Instance Manager on " + testIMRestartTarget + " was killed",
		"Restoring with operation " + testIMRestartOp, "ran again on the same Pod",
		"completed the restore under operation " + testIMRestartOp2 + ", attempt 1, with one retry"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("S12 instance-manager-restart reason = %q, want %q", reason, want)
		}
	}
	for _, rel := range []string{s12IMRestartRecordFile, s12IMRestartTimelineFile, s12IMRestartDataCheckFile,
		s12IMRestartSourceDataCheckFile} {
		if !slices.Contains(restart.Evidence, rel) {
			t.Fatalf("S12 instance-manager-restart evidence = %v, want %s", restart.Evidence, rel)
		}
	}
	m := restart.Measurements
	if restart.Limits[restoreLimitName] != cfg.RestoreLimit.String() || m["restoredWithin"] == evidence.Unknown ||
		m["memberBackWithin"] == evidence.Unknown || m["containerRestarts"] != float64(1) ||
		m["operationIDChanged"] != true || m["attemptsAdded"] != float64(1) || m["retriesAfterKill"] != float64(1) ||
		m["postBackupRowsPresent"] != float64(0) {
		t.Fatalf("limits = %v, measurements = %v", restart.Limits, m)
	}
	requireIMRestartRecord(t, rec)
	if len(f.imWrites) != 1 || !strings.Contains(f.imWrites[0], s12IMRestartClusterName+"-rw") {
		t.Fatalf("normal writes = %v, want one marker through ha-imrestart-rw", f.imWrites)
	}
	report := readReport(t, cfg.EvidenceDir, s12IMRestartDataCheckFile)
	for _, name := range append(clusterMembers(s12IMRestartClusterName), "rw", "ro") {
		if check, ok := report.Members[name]; !report.OK || !ok || check.Rows != 8 {
			t.Fatalf("data check = %+v, want the rows before the backup on %s", report, name)
		}
	}
	if got := f.imCluster; got == nil || got.Spec.ObjectStorage.CredentialsSecretRef.Name != testIMRestartSecret ||
		!strings.HasSuffix(got.Spec.ObjectStorage.Prefix, "/imrestart-seed") {
		t.Fatalf("cluster = %+v, want a Secret and a prefix of its own", got)
	}
	requireNamespaceGone(t, f.haFixture)
}

// requireIMRestartRecord checks the record of a passed run.
func requireIMRestartRecord(t *testing.T, rec s12IMRestartRecord) {
	t.Helper()
	r := rec.IMRestart
	restoring := string(databasev1alpha1.BootstrapRestoring)
	if r.PhaseFirstPastPreparing != restoring || r.PhaseAtRestart != restoring || r.PhaseAfterRestart != restoring ||
		r.OperationIDBefore != testIMRestartOp || r.AttemptsBefore == nil || *r.AttemptsBefore != 0 ||
		r.OperationIDAfter != testIMRestartOp2 || r.AttemptsAfter == nil || *r.AttemptsAfter != 1 ||
		r.MessageAtRestart != "restoredb is running on "+testIMRestartTarget {
		t.Fatalf("restart = %+v, want the operations before and after", r)
	}
	requireIMRestartContainer(t, r)
	requireIMRestartAttempts(t, r)
	if rec.Cluster != s12IMRestartClusterName || rec.ClusterUID != testIMRestartUID || rec.Roles == nil ||
		rec.Roles.Master != testIMRestartTarget || rec.NormalWrite == nil ||
		!slices.Equal(rec.NormalWrite.ReadBackOn, clusterMembers(s12IMRestartClusterName)) ||
		rec.Source == nil || !rec.Source.DataRulesHeld || len(rec.Credentials.FoundIn) != 0 {
		t.Fatalf("record = %+v, want the restored cluster, its roles, the normal write and the source", rec)
	}
}

// requireIMRestartContainer checks the target's container before and after
// the kill of a passed run, with the times.
func requireIMRestartContainer(t *testing.T, r *s12IMRestart) {
	t.Helper()
	if r.Pod != testIMRestartTarget || r.PodUID != testIMRestartTarget || r.ContainerBefore == "" ||
		r.ContainerAfter != r.ContainerBefore+"-restarted" || r.RestartsBefore == nil || *r.RestartsBefore != 0 ||
		r.RestartsAfter == nil || *r.RestartsAfter != 1 || r.RestartsAtEnd == nil || *r.RestartsAtEnd != 1 ||
		r.ExitCode == nil || *r.ExitCode != 137 || r.KilledAt == nil || r.EndedAt == nil || r.BackAt == nil ||
		r.BackWithin == "" || r.StartedBefore == nil || r.StartedAfter == nil || r.FinishedAt == nil ||
		!strings.Contains(r.KillAnswer, "exit code 137") || r.PodReplaced {
		t.Fatalf("restart = %+v, want the container before and after with the times", r)
	}
}

// requireIMRestartAttempts checks the retry, the reasons and the attempts of
// a passed run.
func requireIMRestartAttempts(t *testing.T, r *s12IMRestart) {
	t.Helper()
	if len(r.Retries) != 1 || r.Retries[0].From != 0 || r.Retries[0].To != 1 ||
		r.Retries[0].Message != testIMRetryMessage {
		t.Fatalf("retries = %+v, want the one the kill caused", r.Retries)
	}
	if want := []string{testIMUnavailable, s12RetryReason, testRestoreInProgress, testSeedInProgress,
		testPeersSeeded}; !slices.Equal(r.Reasons, want) {
		t.Fatalf("reasons = %v, want %v", r.Reasons, want)
	}
	if len(r.Attempts) != 2 || r.Attempts[0].Attempt != 0 || !slices.Equal(r.Attempts[0].Operations,
		[]string{testIMRestartOp}) || r.Attempts[1].Attempt != 1 ||
		!slices.Equal(r.Attempts[1].Operations, []string{testIMRestartOp2}) {
		t.Fatalf("attempts = %+v, want one operation per attempt", r.Attempts)
	}
}

func TestRunS12InstanceManagerRestartIsBlockedWhenTheKillCannotBePlaced(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture)
		want   string
	}{
		"Restoring never seen": {change: func(f *s12IMRestartFixture) {
			f.beforeKill = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing,
				databasev1alpha1.BootstrapSeedingReplicas}
		}, want: "the bootstrap was first seen SeedingReplicas, not Restoring"},
		"Restoring over before the kill": {change: func(f *s12IMRestartFixture) {
			f.beforeKill = append(f.beforeKill, databasev1alpha1.BootstrapSeedingReplicas)
			f.noHold = true
		}, want: `right before it the bootstrap phase was "SeedingReplicas"`},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12IMRestartConfig(t)
			f := newS12IMRestartFixture(t, cfg)
			tc.change(f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, "could not be placed during the restore", tc.want)
			if f.imKills != 0 {
				t.Fatal("the Instance Manager was killed although the restore was not Restoring")
			}
			if rec := readIMRestartRecord(t, cfg.EvidenceDir); rec.IMRestart.KilledAt != nil {
				t.Fatalf("restart = %+v, want no kill recorded", rec.IMRestart)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartIsBlockedWhenTheTargetIsNotTheClusters(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	f.targetMemberStatus = testHAPrimary
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Blocked, "starting_state_not_verified",
		"is not a member of "+s12IMRestartClusterName)
	if f.imKills != 0 {
		t.Fatalf("kills = %d, want none", f.imKills)
	}
}

func TestRunS12InstanceManagerRestartWhenTheKillDoesNotTakeEffect(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture)
		result evidence.Result
		want   []string
	}{
		"no single Instance Manager process": {change: func(f *s12IMRestartFixture) { f.refuse = true },
			result: evidence.Blocked, want: []string{"fault_injection_failed", "could not be told apart"}},
		"the container keeps running": {change: func(f *s12IMRestartFixture) {
			f.noRestart = true
			f.killAnswer = func() (string, error) { return "killed instance-manager 42", nil }
		}, result: evidence.Blocked, want: []string{"fault_not_confirmed"}},
		"the Pod replaced": {change: func(f *s12IMRestartFixture) { f.replacePod = true },
			result: evidence.Blocked, want: []string{"was replaced, not only its container restarted"}},
		"the container does not run again": {change: func(f *s12IMRestartFixture) { f.endOnly = true },
			result: evidence.Fail, want: []string{"did not run again within restore_limit", "time limit exceeded"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12IMRestartConfig(t)
			f := newS12IMRestartFixture(t, cfg)
			tc.change(f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, tc.result, tc.want...)
			if f.imKills != 1 || !slices.Contains(restart.Evidence, s12IMRestartTimelineFile) {
				t.Fatalf("kills = %d, evidence = %v; want one kill and its timeline", f.imKills, restart.Evidence)
			}
			if restart.Measurements["memberBackWithin"] != evidence.Unknown {
				t.Fatalf("measurements = %v, want memberBackWithin unknown", restart.Measurements)
			}
		})
	}
}

// onTargetRead changes the target member's Pod as stored, at its read number
// at, before that read.
func onTargetRead(f *s12IMRestartFixture, at int, change func(ctx context.Context, c client.WithWatch,
	pod *corev1.Pod)) {
	var reads int
	f.podRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name != testIMRestartTarget {
			return nil
		}
		if reads++; reads != at {
			return nil
		}
		pod := &corev1.Pod{}
		if err := c.Get(ctx, key, pod); err != nil {
			f.t.Error(err)
			return nil
		}
		change(ctx, c, pod)
		return nil
	}
}

// updatePodStatus stores pod's status.
func updatePodStatus(ctx context.Context, t *testing.T, c client.WithWatch, pod *corev1.Pod) {
	t.Helper()
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Error(err)
	}
}

func TestRunS12InstanceManagerRestartIsBlockedWhenTheTargetChangedBeforeTheKill(t *testing.T) {
	// The target is read once right before the kill and once more by the
	// kill itself; between them one thing about it changes.
	for name, change := range map[string]func(ctx context.Context, c client.WithWatch, pod *corev1.Pod){
		"the Pod's UID only": func(ctx context.Context, c client.WithWatch, pod *corev1.Pod) {
			if err := c.Delete(ctx, pod); err != nil {
				t.Error(err)
			}
			pod.UID, pod.ResourceVersion = types.UID(testIMRestartTarget+"-again"), ""
			if err := c.Create(ctx, pod); err != nil {
				t.Error(err)
			}
		},
		"the container's ID only": func(ctx context.Context, c client.WithWatch, pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].ContainerID += "-other"
			updatePodStatus(ctx, t, c, pod)
		},
		"the restart count only": func(ctx context.Context, c client.WithWatch, pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].RestartCount++
			updatePodStatus(ctx, t, c, pod)
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12IMRestartConfig(t)
			f := newS12IMRestartFixture(t, cfg)
			onTargetRead(f, 2, change)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked, "fault_injection_failed",
				"is not the one read right before the kill")
			if f.imKills != 0 {
				t.Fatalf("kills = %d, want none", f.imKills)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartIsBlockedWhenTheTargetIsNotTheStatefulSets(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	onTargetRead(f, 1, func(ctx context.Context, c client.WithWatch, pod *corev1.Pod) {
		pod.OwnerReferences = []metav1.OwnerReference{owner("StatefulSet", s12IMRestartClusterName, "someone-else")}
		if err := c.Update(ctx, pod); err != nil {
			t.Error(err)
		}
	})
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Blocked, "starting_state_not_verified",
		"does not belong to the restored cluster's StatefulSet")
	if f.imKills != 0 {
		t.Fatalf("kills = %d, want none", f.imKills)
	}
}

func TestRunS12InstanceManagerRestartIsBlockedWhenThePodCannotBeReadAfterTheKill(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	// The kill is confirmed on the third read; every read after it fails.
	var reads int
	f.podRead = func(_ context.Context, _ client.WithWatch, key client.ObjectKey) error {
		if key.Name != testIMRestartTarget {
			return nil
		}
		if reads++; reads > 3 {
			return errors.New("connection refused")
		}
		return nil
	}
	f.endOnly = true
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Blocked, "could not be read after the kill", "connection refused")
}

func TestRunS12InstanceManagerRestartIsBlockedWhenTheContainerEndedOtherwise(t *testing.T) {
	for name, tc := range map[string]struct {
		code   int32
		reason string
	}{
		"another exit code": {code: 1, reason: "Error"},
		"OOMKilled":         {code: 137, reason: "OOMKilled"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12IMRestartConfig(t)
			f := newS12IMRestartFixture(t, cfg)
			f.exitCode, f.exitReason = tc.code, tc.reason
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Blocked,
				"the container ended for another reason than the kill", tc.reason)
		})
	}
}

func TestIMKillScriptMatchesTheManagerByItsFullPath(t *testing.T) {
	const binary = "/usr/local/bin/instance-manager"
	if !strings.Contains(imKillScript, `[ "${argv[0]}" = `+binary+` ]`) || strings.Contains(imKillScript, "##*/") {
		t.Fatalf("imKillScript = %q, want argv[0] compared with %s exactly", imKillScript, binary)
	}
}

func TestRunS12InstanceManagerRestartFailsOnMoreThanOneContainerRestart(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	f.extraRestarts = 1
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Fail, "restarted 2 times after one kill")
	if len(restart.NeverEvents) != 1 || !strings.Contains(restart.NeverEvents[0], "restarted 2 times after one kill") {
		t.Fatalf("never-events = %v, want the second restart", restart.NeverEvents)
	}
	if restart.Measurements["containerRestarts"] != float64(2) {
		t.Fatalf("measurements = %v, want two restarts", restart.Measurements)
	}
}

func TestRunS12InstanceManagerRestartIsBlockedWhenTheKillDidNotInterruptTheRestore(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	// The operation had completed: the Operator reads Completed and seeds.
	f.afterKill = []imStatus{
		{databasev1alpha1.BootstrapSeedingReplicas, 0, testIMRestartOp, testSeedInProgress, "copying"},
		{databasev1alpha1.BootstrapComplete, 0, testIMRestartOp, testPeersSeeded, "copied"},
	}
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Blocked, "the kill did not interrupt the restore",
		"operation "+testIMRestartOp+" completed under attempt 0")
	if restart.Measurements["retriesAfterKill"] != float64(0) || restart.Measurements["operationIDChanged"] != false {
		t.Fatalf("measurements = %v", restart.Measurements)
	}
}

func TestRunS12InstanceManagerRestartIsBlockedByARetryForAnotherReason(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	f.afterKill[1].message = "on " + testIMRestartTarget + ": restore failed: download: connection reset; " +
		"starting attempt 2 of 3"
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Blocked, "not with the Instance Manager's restart",
		"whether the restart ended it was not observed")
}

func TestRunS12InstanceManagerRestartFailsWhenTheRetriedRestoreFailsAgain(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	again := "on " + testIMRestartTarget + ": restore failed: restoredb failed; starting attempt 3 of 3"
	f.afterKill = slices.Insert(f.afterKill, 3,
		imStatus{databasev1alpha1.BootstrapRestoring, 2, "", s12RetryReason, again},
		imStatus{databasev1alpha1.BootstrapRestoring, 2, "op-attempt-2", testRestoreInProgress, "restoredb"})
	for i := 5; i < len(f.afterKill); i++ {
		f.afterKill[i].attempts, f.afterKill[i].op = 2, "op-attempt-2"
	}
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Fail, "the restore failed again after the attempt the kill ended",
		"restoredb failed")
	if rec := readIMRestartRecord(t, cfg.EvidenceDir); len(rec.IMRestart.Retries) != 2 {
		t.Fatalf("retries = %+v, want both", rec.IMRestart.Retries)
	}
}

func TestRunS12InstanceManagerRestartFailsOnNeverEvents(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture)
		want   []string
	}{
		// The record was on the volume before its ID was answered.
		"the operation's record missing": {change: func(f *s12IMRestartFixture) {
			f.afterKill[0] = imStatus{databasev1alpha1.BootstrapRestoring, 0, testIMRestartOp,
				s12OperationMissingReason, testIMRestartTarget + " holds no record of the restore operation"}
		}, want: []string{"holds no record of the restore operation", "lost a record it had answered for"}},
		"the restore started again without a failed attempt": {change: func(f *s12IMRestartFixture) {
			f.afterKill[0].op = testIMRestartOp2
		}, want: []string{"started again without a failed attempt", testIMRestartOp + " became " + testIMRestartOp2}},
		"the operation of the killed attempt completes": {change: func(f *s12IMRestartFixture) {
			for i := 2; i < len(f.afterKill); i++ {
				f.afterKill[i].op = testIMRestartOp
			}
		}, want: []string{"the operation " + testIMRestartOp + " the kill ended is the one that completed"}},
		"attempts beyond the Operator's": {change: func(f *s12IMRestartFixture) {
			f.tamper = func(_ int, killed bool, s *databasev1alpha1.CubridClusterStatus) {
				if killed && s.Bootstrap.Phase == databasev1alpha1.BootstrapSeedingReplicas {
					s.Bootstrap.Attempts = s12MaxAttempts
				}
			}
		}, want: []string{"attempts reached 3"}},
		"attempts falling": {change: func(f *s12IMRestartFixture) {
			f.tamper = func(_ int, killed bool, s *databasev1alpha1.CubridClusterStatus) {
				if killed && s.Bootstrap.Phase == databasev1alpha1.BootstrapComplete {
					s.Bootstrap.Attempts = 0
				}
			}
		}, want: []string{"attempts went from 1 back to 0"}},
		"BootstrapReady True before Complete": {change: func(f *s12IMRestartFixture) {
			f.tamper = func(_ int, _ bool, s *databasev1alpha1.CubridClusterStatus) {
				if s.Bootstrap.Phase == databasev1alpha1.BootstrapSeedingReplicas {
					s.Conditions[0].Status = metav1.ConditionTrue
				}
			}
		}, want: []string{"reported BootstrapReady True", "SeedingReplicas"}},
		"Ready before Complete": {change: func(f *s12IMRestartFixture) {
			f.tamper = func(_ int, killed bool, s *databasev1alpha1.CubridClusterStatus) {
				if killed && s.Bootstrap.Phase == databasev1alpha1.BootstrapRestoring {
					for i := range s.Conditions {
						if s.Conditions[i].Type == startingConditions[0] {
							s.Conditions[i].Status = metav1.ConditionTrue
						}
					}
				}
			}
		}, want: []string{"reported Ready while its bootstrap phase was"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12IMRestartConfig(t)
			f := newS12IMRestartFixture(t, cfg)
			tc.change(f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Fail, tc.want...)
		})
	}
}

func TestRunS12InstanceManagerRestartIsBlockedByAnAttemptWithoutItsReason(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	f.afterKill = slices.Delete(f.afterKill, 1, 2)
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Blocked, "cause is unknown")
}

func TestRunS12InstanceManagerRestartTakesTheRetryFromItsEvent(t *testing.T) {
	for name, tc := range map[string]struct {
		event  string
		result evidence.Result
	}{
		"the Event names the restart":   {event: testIMRetryMessage, result: evidence.Pass},
		"the Event names another cause": {event: "on " + testIMRestartTarget + ": download failed", result: evidence.Blocked},
		// An Event of another retry of the same cluster, with the same cause.
		"the Event is of another attempt": {event: "on " + testIMRestartTarget + ": " + s12IMRestartedReason +
			"; starting attempt 3 of 3", result: evidence.Blocked},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12IMRestartConfig(t)
			f := newS12IMRestartFixture(t, cfg)
			// The reading of RestoreFailedRetrying was missed between two
			// readings; the Operator's Event of it is there.
			f.afterKill = slices.Delete(f.afterKill, 1, 2)
			f.retryEvent = tc.event
			restart, err := imRestartOf(t, f)
			if restart.Result != tc.result {
				t.Fatalf("S12 instance-manager-restart = %+v, %v; want %s", restart, err, tc.result)
			}
			if tc.result == evidence.Blocked {
				requireIMRestart(t, restart, err, evidence.Blocked, "cause is unknown")
				return
			}
			r := readIMRestartRecord(t, cfg.EvidenceDir).IMRestart
			if len(r.Retries) != 1 || r.Retries[0].Message != testIMRetryMessage ||
				!r.Retries[0].FromEvent {
				t.Fatalf("retries = %+v, want the one the Event recorded", r.Retries)
			}
		})
	}
}

func TestRunS12InstanceManagerRestartIsBlockedWhenTheFirstReadingAfterTheKillIsPastRestoring(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	// The restore had completed; the seeding that followed fails.
	f.afterKill = []imStatus{
		{databasev1alpha1.BootstrapSeedingReplicas, 0, testIMRestartOp, testSeedInProgress, "copying"},
		{databasev1alpha1.BootstrapFailed, 0, testIMRestartOp, "SeedFailed", "seeding failed"},
	}
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Blocked, "the kill did not interrupt a restore",
		"the first reading after it was SeedingReplicas", "seeding failed")
}

func TestRunS12InstanceManagerRestartFailsOnABootstrapFailedAfterTheKill(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	f.afterKill = []imStatus{{databasev1alpha1.BootstrapFailed, 0, testIMRestartOp, "RestoreFailed",
		"on " + testIMRestartTarget + ": " + s12IMRestartedReason + "; giving up after 3 attempts"}}
	restart, err := imRestartOf(t, f)
	requireIMRestart(t, restart, err, evidence.Fail, "ended Failed", "giving up after 3 attempts")
	if f.imKills != 1 {
		t.Fatalf("kills = %d, want one", f.imKills)
	}
}

func TestRunS12InstanceManagerRestartFailsOnTheRestoredData(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12IMRestartFixture)
		want   []string
	}{
		"an acknowledged row missing": {change: func(f *s12IMRestartFixture) { f.missing[testIMRestartTarget] = 0 },
			want: []string{workload.RuleMissing, testIMRestartTarget}},
		"the normal write missing on a member": {change: func(f *s12IMRestartFixture) {
			f.dropMarkerOnIM = testIMRestartSlave
		}, want: []string{"marker " + s12IMRestartMarker, testIMRestartSlave}},
		"a credential in the status": {change: func(f *s12IMRestartFixture) {
			f.tamper = func(_ int, killed bool, s *databasev1alpha1.CubridClusterStatus) {
				if killed {
					s.Conditions[0].Message = "using key " + testAccessKey
				}
			}
		}, want: []string{"credential"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12IMRestartConfig(t)
			f := newS12IMRestartFixture(t, cfg)
			tc.change(f)
			restart, err := imRestartOf(t, f)
			requireIMRestart(t, restart, err, evidence.Fail, tc.want...)
		})
	}
}

func TestRunS12InstanceManagerRestartIsBlockedWhenS12ExactDidNotPass(t *testing.T) {
	cfg := testS12IMRestartConfig(t)
	f := newS12IMRestartFixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	scenarios := imRestartScenarios(t, cfg.EvidenceDir)
	if restart := scenarios[3]; restart.Result != evidence.Blocked ||
		!strings.Contains(restart.Reason, "S12 exact did not pass") {
		t.Fatalf("S12 instance-manager-restart = %+v, want blocked by S12 exact", restart)
	}
	if f.imCluster != nil || f.imKills != 0 {
		t.Fatal("S12 instance-manager-restart ran although S12 exact did not pass")
	}
}

func TestRunHARefusesAnInvalidS12InstanceManagerRestartConfiguration(t *testing.T) {
	for name, change := range map[string]func(*HAConfig){
		"no S12 exact":     func(c *HAConfig) { c.Restore = false },
		"and tampered":     func(c *HAConfig) { c.RestoreTampered = true },
		"and wrong-target": func(c *HAConfig) { c.RestoreWrongTarget = true },
		// With what operator-restart needs, so that only the combination is
		// refused.
		"and operator-restart": func(c *HAConfig) {
			c.RestoreOperatorRestart, c.ResyncLimit, c.LeaderLease = true, time.Second, testLeaderLease
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
