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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	testRestartUID    = "restart-cluster"
	testRestartMaster = "ha-restart-0"
	testRestartSlave  = "ha-restart-2"
	testRestartSecret = "object-storage-restart"
	testRestartOp     = "restore-op"
	testRestartOp2    = "restore-op-2"
	// testRestoreInProgress and testDatabaseCreated are the BootstrapReady
	// reason of a running restore and the phase of a created database.
	testRestoreInProgress = "RestoreInProgress"
	testDatabaseCreated   = "Created"
)

func testS12RestartConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS12Config(t)
	cfg.RestoreOperatorRestart = true
	cfg.ResyncLimit = time.Second
	cfg.LeaderLease = testLeaderLease
	return cfg
}

// s12RestartFixture plays, after S12 exact, an Operator that restores S11's
// artifact into ha-restart and keeps its bootstrap Restoring until the
// manager Pod is deleted, a Deployment whose replacement takes the leader
// Lease, CUBRID on ha-restart's members and the client writing through it.
type s12RestartFixture struct {
	*s12Fixture
	// restartPhases are the bootstrap phases the fake Operator reports for
	// ha-restart, one per reading; the last is kept, and Restoring is kept
	// until the manager Pod was deleted unless noHold is set. tamperRestart
	// changes the status of a reading; deleted tells whether the manager Pod
	// was deleted before it.
	restartPhases []databasev1alpha1.BootstrapPhase
	noHold        bool
	tamperRestart func(read int, deleted bool, s *databasev1alpha1.CubridClusterStatus)
	// dropRestartMarkerOn is a member without the marker written through
	// ha-restart; misrouteRestart puts that marker on the source's members
	// too.
	dropRestartMarkerOn string
	misrouteRestart     bool

	restartCluster            *databasev1alpha1.CubridCluster
	restartReads, restartStep int
	restartWrites             []string
}

func newS12RestartFixture(t *testing.T, cfg HAConfig) *s12RestartFixture {
	t.Helper()
	holder := testManagerPodName + "_old"
	acquired := metav1.NewMicroTime(time.Now().Add(-time.Hour))
	f := &s12RestartFixture{s12Fixture: newS12Fixture(t, cfg, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: testLeaderLease, Namespace: cfg.OperatorNamespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, AcquireTime: &acquired}}),
		restartPhases: []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing,
			databasev1alpha1.BootstrapRestoring, databasev1alpha1.BootstrapSeedingReplicas,
			databasev1alpha1.BootstrapComplete}}
	for _, member := range clusterMembers(s12RestartClusterName) {
		f.roles[member] = roleSlave
	}
	f.roles[testRestartMaster] = roleMaster
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.startReplacement(ctx, c)
		f.takeLease(ctx, c)
	}
	f.restartCreated = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
		cluster.UID, cluster.Generation = testRestartUID, 1
		f.mu.Lock()
		f.restartCluster = cluster.DeepCopy()
		f.mu.Unlock()
		if err := c.Create(ctx, cluster); err != nil {
			return err
		}
		return f.createChildren(ctx, c, cluster)
	}
	f.restartRead = f.advanceRestart
	s12Client, s12Markers := f.clientHook, f.markersOf
	f.clientHook = func(script string) (string, error, bool) {
		if !strings.Contains(script, "workload marker "+s12RestartMarker+" ") {
			return s12Client(script)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.restartWrites = append(f.restartWrites, script)
		return "marker " + s12RestartMarker + " written\n", nil, true
	}
	f.markersOf = func(pod string) []string {
		rows := s12Markers(pod)
		f.mu.Lock()
		defer f.mu.Unlock()
		restarted := strings.HasPrefix(pod, s12RestartClusterName+"-")
		if (restarted || f.misrouteRestart) && len(f.restartWrites) > 0 && pod != f.dropRestartMarkerOn {
			_, rest, _ := strings.Cut(f.restartWrites[0], "workload marker "+s12RestartMarker+" ")
			rows = append(rows, s12RestartMarker+"="+strings.Fields(rest)[0])
		}
		return rows
	}
	return f
}

// advanceRestart writes the status of ha-restart's next reading, as the
// Operator would: the operation is recorded from the start, BootstrapReady
// and Ready are True only when the bootstrap is Complete.
func (f *s12RestartFixture) advanceRestart(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, key, cluster); err != nil {
		return nil
	}
	f.mu.Lock()
	f.restartReads++
	read, deleted := f.restartReads, f.managerDeleted
	phase := f.restartPhases[min(f.restartStep, len(f.restartPhases)-1)]
	if phase != databasev1alpha1.BootstrapRestoring || deleted || f.noHold {
		f.restartStep++
	}
	tamper := f.tamperRestart
	f.mu.Unlock()
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	status := metav1.ConditionFalse
	if phase == databasev1alpha1.BootstrapComplete {
		status = metav1.ConditionTrue
	}
	s := &cluster.Status
	s.ObservedGeneration = 1
	s.Bootstrap = &databasev1alpha1.BootstrapStatus{Mode: testRecoveryMode, Phase: phase,
		ManifestURI: cluster.Spec.Bootstrap.Recovery.ManifestURI, TargetMember: testRestartMaster,
		OperationID: testRestartOp}
	boot := metav1.Condition{Type: bootstrapReadyCondition, Status: status, Reason: testRestoreInProgress,
		Message: "restoredb is running on " + testRestartMaster, LastTransitionTime: now}
	switch phase {
	case databasev1alpha1.BootstrapSeedingReplicas:
		boot.Reason, boot.Message = "SeedRestoreInProgress", "copying to the other members"
	case databasev1alpha1.BootstrapComplete:
		boot.Reason, boot.Message = "PeersSeeded", "database appdb was restored on "+testRestartMaster+
			" and copied to the other members"
		s.CurrentPrimary = testRestartMaster
		s.Databases = []databasev1alpha1.DatabaseStatus{{Name: databaseName, Phase: testDatabaseCreated, PrimaryCreated: true,
			HAConfigured: true}}
	}
	s.Conditions = []metav1.Condition{boot}
	for _, condition := range startingConditions {
		s.Conditions = append(s.Conditions, metav1.Condition{Type: condition, Status: status, Reason: testConditionReason,
			LastTransitionTime: now})
	}
	if tamper != nil {
		tamper(read, deleted, s)
	}
	if err := c.Update(ctx, cluster); err != nil {
		f.t.Error(err)
	}
	return nil
}

func restartScenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s12RestartLane || len(summary.Scenarios) != 4 || summary.Scenarios[3].ID != s12ID ||
		summary.Scenarios[3].Variant != s12RestartVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete, S12/exact and S12/operator-restart in lane %s", summary,
			s12RestartLane)
	}
	return summary.Scenarios
}

// restartOf runs the fixture and returns S12 operator-restart, requiring
// LAB01, S11 and S12 exact passed.
func restartOf(t *testing.T, f *s12RestartFixture, ctx context.Context) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(ctx, f.cfg)
	scenarios := restartScenarios(t, f.cfg.EvidenceDir)
	for _, s := range scenarios[:3] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01, S11 and S12 exact passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, scenarios[2], "S12/exact")
	requireOwnEvidence(t, scenarios[3], "S12/operator-restart")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	return scenarios[3], err
}

func readRestartRecord(t *testing.T, dir string) s12RestartRecord {
	t.Helper()
	var rec s12RestartRecord
	data, err := os.ReadFile(filepath.Join(dir, s12RestartRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S12 operator-restart record = %s, %v", data, err)
	}
	return rec
}

// requireRestartFailure fails unless S12 operator-restart failed with every
// part of want in its reason.
func requireRestartFailure(t *testing.T, restart evidence.Scenario, err error, want ...string) {
	t.Helper()
	if err == nil || restart.Result != evidence.Fail {
		t.Fatalf("S12 operator-restart = %+v, want a failure", restart)
	}
	for _, w := range want {
		if !strings.Contains(restart.Reason, w) {
			t.Fatalf("S12 operator-restart reason = %q, want %q", restart.Reason, w)
		}
	}
}

func TestRunS12OperatorRestartPassesWhenTheRestoreSurvivesTheRestart(t *testing.T) {
	cfg := testS12RestartConfig(t)
	f := newS12RestartFixture(t, cfg)
	restart, err := restartOf(t, f, t.Context())
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if summary := readSummary(t, cfg.EvidenceDir); !summary.Passed || restart.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want every scenario passed", summary)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,secret,restore-cluster,secret,"+
		"restart-cluster" || !f.managerDeleted {
		t.Fatalf("writes = %v, manager deleted %t", f.writes, f.managerDeleted)
	}
	for _, rel := range []string{s12RestartRecordFile, s12RestartTimelineFile, s12RestartDataCheckFile,
		s12RestartSourceDataCheckFile} {
		if !slices.Contains(restart.Evidence, rel) {
			t.Fatalf("S12 operator-restart evidence = %v, want %s", restart.Evidence, rel)
		}
	}
	m := restart.Measurements
	if restart.Limits["restore_limit"] != cfg.RestoreLimit.String() ||
		restart.Limits["operator_resync_limit"] != cfg.ResyncLimit.String() || m["restoredWithin"] == evidence.Unknown ||
		m["operatorBackWithin"] == evidence.Unknown || m["operationIDUnchanged"] != true ||
		m["attemptsAdded"] != float64(0) || m["postBackupRowsPresent"] != float64(0) {
		t.Fatalf("S12 operator-restart limits = %v, measurements = %v", restart.Limits, m)
	}
	requireRestartSpec(t, f)
	requireRestartRecord(t, f)
	if len(f.restartWrites) != 1 || !strings.Contains(f.restartWrites[0], s12RestartClusterName+"-rw") {
		t.Fatalf("normal writes = %v, want one marker through ha-restart-rw", f.restartWrites)
	}
	report := readReport(t, cfg.EvidenceDir, s12RestartDataCheckFile)
	for _, name := range append(clusterMembers(s12RestartClusterName), "rw", "ro") {
		if check, ok := report.Members[name]; !report.OK || !ok || check.Rows != 8 {
			t.Fatalf("data check = %+v, want the rows before the backup on %s", report, name)
		}
	}
	requireNamespaceGone(t, f.haFixture)
}

// requireRestartSpec checks the cluster the run restored while the manager
// restarted.
func requireRestartSpec(t *testing.T, f *s12RestartFixture) {
	t.Helper()
	lab01, s11 := readHARecord(t, f.cfg.EvidenceDir), readS11Record(t, f.cfg.EvidenceDir)
	got := f.restartCluster
	if got == nil || got.Spec.Bootstrap == nil || got.Spec.Bootstrap.Recovery == nil ||
		got.Spec.Bootstrap.Recovery.ManifestURI != s11.Artifact.URI {
		t.Fatalf("restarted cluster = %+v, want S11's artifact", got)
	}
	if storage := got.Spec.ObjectStorage; storage.Prefix != "lab-runs/"+lab01.RunID+"/restart-seed" ||
		storage.CredentialsSecretRef.Name != testRestartSecret {
		t.Fatalf("objectStorage = %+v, want a Secret and a prefix of its own", storage)
	}
	if len(f.secrets) != 3 || f.secrets[2].Name != testRestartSecret {
		t.Fatalf("Secrets = %v, want the seed Secret copied again for ha-restart", f.secrets)
	}
}

// requireRestartRecord checks the record of a passed run.
func requireRestartRecord(t *testing.T, f *s12RestartFixture) {
	t.Helper()
	rec := readRestartRecord(t, f.cfg.EvidenceDir)
	phases := make([]string, 0, len(rec.Bootstrap))
	for _, p := range rec.Bootstrap {
		phases = append(phases, p.Phase)
	}
	want := []string{string(databasev1alpha1.BootstrapPreparing), string(databasev1alpha1.BootstrapRestoring),
		string(databasev1alpha1.BootstrapSeedingReplicas), string(databasev1alpha1.BootstrapComplete)}
	if !slices.Equal(phases, want) {
		t.Fatalf("bootstrap = %v, want every phase", phases)
	}
	requireRestartTimes(t, rec.Restart)
	if rec.Cluster != s12RestartClusterName || rec.ClusterUID != testRestartUID || rec.Roles == nil ||
		rec.Roles.Master != testRestartMaster || rec.NormalWrite == nil ||
		!slices.Equal(rec.NormalWrite.ReadBackOn, clusterMembers(s12RestartClusterName)) ||
		!slices.Equal(rec.HANodes[s12RestartClusterName], clusterMembers(s12RestartClusterName)) {
		t.Fatalf("record = %+v, want the restored cluster, its roles, HA nodes and the normal write", rec)
	}
	if rec.Source == nil || !rec.Source.DataRulesHeld || rec.Credentials.StatusesChecked == 0 ||
		len(rec.Credentials.FoundIn) != 0 {
		t.Fatalf("source = %+v, credentials = %+v", rec.Source, rec.Credentials)
	}
}

// requireRestartTimes checks the restart of a passed run: the same operation
// before and after, the manager before and after, and the times.
func requireRestartTimes(t *testing.T, r *s12Restart) {
	t.Helper()
	restoring := string(databasev1alpha1.BootstrapRestoring)
	if r == nil || r.PhaseFirstPastPreparing != restoring || r.PhaseAtRestart != restoring ||
		r.OperationIDBefore != testRestartOp || r.OperationIDAfter != testRestartOp || r.AttemptsBefore == nil ||
		*r.AttemptsBefore != 0 || r.AttemptsAfter == nil || *r.AttemptsAfter != 0 || len(r.Retries) != 0 {
		t.Fatalf("restart = %+v, want the same operation before and after", r)
	}
	if r.ManagerBefore.UID != testManagerPodName || r.ManagerAfter == nil || r.ManagerAfter.UID != testReplacementPod ||
		r.LeaseHolder != testReplacementPod+"_new" || r.RestartedAt == nil || r.GoneAt == nil || r.BackAt == nil ||
		r.BackWithin == "" || r.PhaseAfterRestart == "" || r.ReplacementStartedAt == nil || r.LeaseAcquiredAt == nil {
		t.Fatalf("restart = %+v, want the manager before and after with the times", r)
	}
}

func TestRunS12OperatorRestartIsBlockedWhenTheRestartCannotBePlaced(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12RestartFixture)
		want   string
	}{
		// The first reading past Preparing is already SeedingReplicas.
		"Restoring never seen": {change: func(f *s12RestartFixture) {
			f.restartPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing,
				databasev1alpha1.BootstrapSeedingReplicas, databasev1alpha1.BootstrapComplete}
		}, want: "the bootstrap was first seen SeedingReplicas, not Restoring"},
		// Restoring is seen once, and is over at the reading right before the
		// restart.
		"Restoring over before the restart": {change: func(f *s12RestartFixture) { f.noHold = true },
			want: `right before it the bootstrap phase was "SeedingReplicas"`},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12RestartConfig(t)
			f := newS12RestartFixture(t, cfg)
			tc.change(f)
			restart, err := restartOf(t, f, t.Context())
			if err == nil || restart.Result != evidence.Blocked ||
				!strings.Contains(restart.Reason, "could not be placed during the restore") ||
				!strings.Contains(restart.Reason, tc.want) {
				t.Fatalf("S12 operator-restart = %+v, want blocked: the restart was not placed, %s", restart, tc.want)
			}
			if f.managerDeleted {
				t.Fatal("the manager Pod was deleted although the restore was not Restoring")
			}
			if restart.Measurements["operatorBackWithin"] != evidence.Unknown {
				t.Fatalf("measurements = %v, want operatorBackWithin unknown", restart.Measurements)
			}
			if rec := readRestartRecord(t, cfg.EvidenceDir); rec.Restart.RestartedAt != nil {
				t.Fatalf("restart = %+v, want no restart recorded", rec.Restart)
			}
		})
	}
}

func TestRunS12OperatorRestartWhenTheManagerIsNotBack(t *testing.T) {
	for name, tc := range map[string]struct {
		replacement func(f *s12RestartFixture) func(context.Context, client.WithWatch)
		resync      time.Duration
		result      evidence.Result
		want        string
	}{
		"no replacement": {replacement: func(*s12RestartFixture) func(context.Context, client.WithWatch) {
			return nil
		}, resync: 100 * time.Millisecond, result: evidence.Fail, want: "manager was not back"},
		"replacement without the Lease": {replacement: func(f *s12RestartFixture) func(context.Context,
			client.WithWatch) {
			return f.startReplacement
		}, resync: 100 * time.Millisecond, result: evidence.Fail, want: "time limit exceeded"},
		"back over the resync limit": {replacement: func(f *s12RestartFixture) func(context.Context,
			client.WithWatch) {
			return f.replacement
		}, resync: time.Nanosecond, result: evidence.Fail, want: "over operator_resync_limit"},
		"another image": {replacement: func(f *s12RestartFixture) func(context.Context, client.WithWatch) {
			return func(ctx context.Context, c client.WithWatch) {
				f.startReplacement(ctx, c)
				pod := &corev1.Pod{}
				key := client.ObjectKey{Namespace: f.cfg.OperatorNamespace, Name: testReplacementPod}
				if err := c.Get(ctx, key, pod); err != nil {
					f.t.Error(err)
					return
				}
				pod.Status.ContainerStatuses[0].ImageID = "containerd://sha256:" + strings.Repeat("e", 64)
				if err := c.Status().Update(ctx, pod); err != nil {
					f.t.Error(err)
				}
				f.takeLease(ctx, c)
			}
		}, resync: time.Second, result: evidence.Blocked, want: "environment changed"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12RestartConfig(t)
			cfg.ResyncLimit = tc.resync
			f := newS12RestartFixture(t, cfg)
			f.replacement = tc.replacement(f)
			restart, err := restartOf(t, f, t.Context())
			if err == nil || restart.Result != tc.result || !strings.Contains(restart.Reason, tc.want) {
				t.Fatalf("S12 operator-restart = %+v, want %s with %q", restart, tc.result, tc.want)
			}
			if !f.managerDeleted || !slices.Contains(restart.Evidence, s12RestartTimelineFile) {
				t.Fatalf("manager deleted %t, evidence %v; want the restart and its timeline", f.managerDeleted,
					restart.Evidence)
			}
		})
	}
}

func TestRunS12OperatorRestartFailsWhenTheRestoreIsStartedAgainSilently(t *testing.T) {
	cfg := testS12RestartConfig(t)
	f := newS12RestartFixture(t, cfg)
	f.tamperRestart = func(_ int, deleted bool, s *databasev1alpha1.CubridClusterStatus) {
		if deleted {
			s.Bootstrap.OperationID = testRestartOp2
		}
	}
	restart, err := restartOf(t, f, t.Context())
	requireRestartFailure(t, restart, err, "started again without a failed attempt", testRestartOp+" became "+
		testRestartOp2)
	if len(restart.NeverEvents) != 1 || restart.Measurements["operationIDUnchanged"] != false {
		t.Fatalf("S12 operator-restart = %+v, want the restore started twice as a never-event", restart)
	}
	if rec := readRestartRecord(t, cfg.EvidenceDir); rec.Restart.OperationIDBefore != testRestartOp ||
		rec.Restart.OperationIDAfter != testRestartOp2 {
		t.Fatalf("restart = %+v, want both operations recorded", rec.Restart)
	}
}

func TestRunS12OperatorRestartAcceptsARetryWithItsReason(t *testing.T) {
	cfg := testS12RestartConfig(t)
	f := newS12RestartFixture(t, cfg)
	var retried int
	f.tamperRestart = func(read int, deleted bool, s *databasev1alpha1.CubridClusterStatus) {
		if !deleted {
			return
		}
		if retried == 0 {
			retried = read
		}
		s.Bootstrap.Attempts, s.Bootstrap.OperationID = 1, testRestartOp2
		if read == retried {
			s.Bootstrap.OperationID = ""
			s.Conditions[0].Reason = s12RetryReason
			s.Conditions[0].Message = "on " + testRestartMaster + ": restore failed: interrupted; starting attempt 2 of 3"
		}
	}
	restart, err := restartOf(t, f, t.Context())
	if err != nil || restart.Result != evidence.Pass {
		t.Fatalf("S12 operator-restart = %+v, %v; want a pass: the status gave the attempt's reason", restart, err)
	}
	rec := readRestartRecord(t, cfg.EvidenceDir)
	if r := rec.Restart; len(r.Retries) != 1 || r.Retries[0].From != 0 || r.Retries[0].To != 1 ||
		r.Retries[0].Reason != s12RetryReason || r.OperationIDAfter != testRestartOp2 {
		t.Fatalf("restart = %+v, want the retry recorded", r)
	}
	if restart.Measurements["operationIDUnchanged"] != false || restart.Measurements["attemptsAdded"] != float64(1) {
		t.Fatalf("measurements = %v, want the new operation and attempt", restart.Measurements)
	}
}

func TestRunS12OperatorRestartIsBlockedByAnAttemptWithoutItsReason(t *testing.T) {
	cfg := testS12RestartConfig(t)
	f := newS12RestartFixture(t, cfg)
	f.tamperRestart = func(_ int, deleted bool, s *databasev1alpha1.CubridClusterStatus) {
		if deleted {
			s.Bootstrap.Attempts, s.Bootstrap.OperationID = 1, testRestartOp2
		}
	}
	restart, err := restartOf(t, f, t.Context())
	if err == nil || restart.Result != evidence.Blocked || !strings.Contains(restart.Reason, "cause is unknown") {
		t.Fatalf("S12 operator-restart = %+v, want blocked: the attempt's cause was not seen", restart)
	}
}

func TestRunS12OperatorRestartFailsOnAnAttemptCounterThatFalls(t *testing.T) {
	cfg := testS12RestartConfig(t)
	f := newS12RestartFixture(t, cfg)
	f.tamperRestart = func(_ int, deleted bool, s *databasev1alpha1.CubridClusterStatus) {
		if !deleted {
			s.Bootstrap.Attempts = 1
			s.Conditions[0].Reason = s12RetryReason
		}
	}
	restart, err := restartOf(t, f, t.Context())
	requireRestartFailure(t, restart, err, "attempts went from 1 back to 0")
}

func TestRunS12OperatorRestartFailsOnReadinessBeforeTheBootstrapCompleted(t *testing.T) {
	for name, tc := range map[string]struct {
		condition string
		want      string
	}{
		"Ready condition": {condition: startingConditions[0], want: "reported Ready while its bootstrap phase was"},
		"BootstrapReady":  {condition: bootstrapReadyCondition, want: "reported BootstrapReady True"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12RestartConfig(t)
			f := newS12RestartFixture(t, cfg)
			f.tamperRestart = func(_ int, _ bool, s *databasev1alpha1.CubridClusterStatus) {
				if s.Bootstrap.Phase != databasev1alpha1.BootstrapSeedingReplicas {
					return
				}
				for i := range s.Conditions {
					if s.Conditions[i].Type == tc.condition {
						s.Conditions[i].Status = metav1.ConditionTrue
					}
				}
			}
			restart, err := restartOf(t, f, t.Context())
			requireRestartFailure(t, restart, err, tc.want, "SeedingReplicas")
			if len(restart.NeverEvents) == 0 || !strings.Contains(restart.NeverEvents[0], tc.want) {
				t.Fatalf("never-events = %v, want %q", restart.NeverEvents, tc.want)
			}
		})
	}
}

func TestRunS12OperatorRestartFailsOnTheRestoredData(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12RestartFixture)
		want   []string
	}{
		"an acknowledged row missing": {change: func(f *s12RestartFixture) { f.missing[testRestartMaster] = 0 },
			want: []string{workload.RuleMissing, testRestartMaster}},
		"the normal write missing on a member": {change: func(f *s12RestartFixture) {
			f.dropRestartMarkerOn = testRestartSlave
		}, want: []string{"marker " + s12RestartMarker, testRestartSlave}},
		"the normal write on the source": {change: func(f *s12RestartFixture) { f.misrouteRestart = true },
			want: []string{"the source changed: the marker " + s12RestartMarker}},
		"a credential in the status": {change: func(f *s12RestartFixture) {
			f.tamperRestart = func(_ int, deleted bool, s *databasev1alpha1.CubridClusterStatus) {
				if deleted {
					s.Conditions[0].Message = "using key " + testAccessKey
				}
			}
		}, want: []string{"credential"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12RestartConfig(t)
			f := newS12RestartFixture(t, cfg)
			tc.change(f)
			restart, err := restartOf(t, f, t.Context())
			requireRestartFailure(t, restart, err, tc.want...)
		})
	}
}

func TestRunS12OperatorRestartIsBlockedWhenS12ExactDidNotPass(t *testing.T) {
	cfg := testS12RestartConfig(t)
	f := newS12RestartFixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	scenarios := restartScenarios(t, cfg.EvidenceDir)
	if restart := scenarios[3]; restart.Result != evidence.Blocked ||
		!strings.Contains(restart.Reason, "S12 exact did not pass") {
		t.Fatalf("S12 operator-restart = %+v, want blocked by S12 exact", restart)
	}
	if f.restartCluster != nil || f.managerDeleted {
		t.Fatal("S12 operator-restart ran although S12 exact did not pass")
	}
}

func TestRunHARefusesAnInvalidS12OperatorRestartConfiguration(t *testing.T) {
	for name, change := range map[string]func(*HAConfig){
		"without -restore":       func(c *HAConfig) { c.Restore = false },
		"with -restore-tampered": func(c *HAConfig) { c.RestoreTampered = true },
		"with -restore-wrong-target": func(c *HAConfig) {
			c.RestoreWrongTarget = true
		},
		"without a resync limit": func(c *HAConfig) { c.ResyncLimit = 0 },
		"without the Lease":      func(c *HAConfig) { c.LeaderLease = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12RestartConfig(t)
			change(&cfg)
			f := newS12RestartFixture(t, cfg)
			if err := f.runner.RunHA(t.Context(), cfg); err == nil {
				t.Fatal("RunHA() accepted the configuration")
			}
			if len(f.writes) != 0 {
				t.Fatalf("writes = %v, want nothing done", f.writes)
			}
		})
	}
}
