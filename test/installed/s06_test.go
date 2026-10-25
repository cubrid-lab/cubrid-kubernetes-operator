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
	testReplacementPod = "manager-pod-2"
	testLeaderLease    = "91d4fe35.cubrid.io"
)

func testS06Config(t *testing.T) HAConfig {
	t.Helper()
	cfg := testHAConfig(t)
	cfg.Timeout = 20 * time.Second
	cfg.Fault = FaultOperatorRestart
	cfg.StablePeriod = 10 * time.Millisecond
	cfg.ResyncLimit = time.Second
	cfg.LeaderLease = testLeaderLease
	return cfg
}

// newS06Fixture plays an Operator whose replacement writes a fresh status,
// and clients that are acknowledged throughout.
func newS06Fixture(t *testing.T, cfg HAConfig) *haFixture {
	t.Helper()
	holder := testManagerPodName + "_old"
	acquired := metav1.NewMicroTime(time.Now().Add(-time.Hour))
	f := newHAFixture(t, cfg, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: testLeaderLease, Namespace: cfg.OperatorNamespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, AcquireTime: &acquired}})
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.startReplacement(ctx, c)
		f.takeLease(ctx, c)
		f.writeStatus(ctx, c)
	}
	f.backgroundHistory = func(id string) string {
		history, rows := backgroundWorkload(id, nil)
		f.mu.Lock()
		f.rows = append(f.rows, rows...)
		f.mu.Unlock()
		return history
	}
	return f
}

// deleteManager deletes the manager Pod as the fake API server would, after
// checking that the run asked for no grace period and named the Pod's UID.
func (f *haFixture) deleteManager(ctx context.Context, c client.WithWatch, pod *corev1.Pod,
	opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if options.GracePeriodSeconds == nil || *options.GracePeriodSeconds != 0 ||
		options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != testManagerPodName {
		f.t.Fatalf("manager Pod deleted without a zero grace period and its UID: %+v", options)
	}
	if err := c.Delete(ctx, pod, opts...); err != nil {
		return err
	}
	f.mu.Lock()
	f.managerDeleted = true
	f.mu.Unlock()
	if f.replacement != nil {
		f.replacement(ctx, c)
	}
	return nil
}

// startReplacement starts the manager Pod the Deployment would replace the
// deleted one with.
func (f *haFixture) startReplacement(ctx context.Context, c client.WithWatch) {
	f.startReplacementAt(ctx, c, time.Now())
}

// startReplacementAt starts the replacement with its container started at.
func (f *haFixture) startReplacementAt(ctx context.Context, c client.WithWatch, at time.Time) {
	started := metav1.NewTime(at.Truncate(time.Second))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testReplacementPod, Namespace: f.cfg.OperatorNamespace,
		UID: testReplacementPod, Labels: map[string]string{testAppLabel: testManagerName},
		OwnerReferences: []metav1.OwnerReference{owner("ReplicaSet", "manager-rs", "replicaset")}},
		Spec: corev1.PodSpec{NodeName: "node-2",
			Containers: []corev1.Container{{Name: testManagerName, Image: f.cfg.OperatorImage}}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: testManagerName, Ready: true,
				ContainerID: "containerd://replacement",
				State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
				ImageID:     "containerd://sha256:" + strings.Repeat("b", 64)}}}}
	if err := c.Create(ctx, pod); err != nil {
		f.t.Error(err)
	}
}

// takeLease makes the replacement the leader, as it would once the old
// holder's Lease expired.
func (f *haFixture) takeLease(ctx context.Context, c client.WithWatch) {
	f.takeLeaseAt(ctx, c, time.Now())
}

// takeLeaseAt makes the replacement the leader from at.
func (f *haFixture) takeLeaseAt(ctx context.Context, c client.WithWatch, at time.Time) {
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.OperatorNamespace, Name: testLeaderLease}, lease); err != nil {
		f.t.Error(err)
		return
	}
	holder := testReplacementPod + "_new"
	acquired := metav1.NewMicroTime(at)
	lease.Spec.HolderIdentity, lease.Spec.AcquireTime = &holder, &acquired
	if err := c.Update(ctx, lease); err != nil {
		f.t.Error(err)
	}
}

// writeStatus writes the replacement's first observation, later than its
// start; the API server keeps whole seconds.
func (f *haFixture) writeStatus(ctx context.Context, c client.WithWatch) {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		f.t.Error(err)
		return
	}
	observed := metav1.NewTime(time.Now().Add(2 * time.Second).Truncate(time.Second))
	cluster.Status.LastObservedTime = &observed
	f.mu.Lock()
	cluster.Status.CurrentPrimary = f.primary
	f.mu.Unlock()
	if err := c.Update(ctx, cluster); err != nil {
		f.t.Error(err)
	}
}

// backgroundWorkload returns the history of a background client whose
// answers all come after now, every seventh operation rolled back, and the
// rows it leaves; fail names sequences that fail.
func backgroundWorkload(id string, fail map[int]bool) (string, []workload.Row) {
	var lines []string
	var rows []workload.Row
	at := time.Now().Add(time.Second).UTC()
	for seq := 1; seq <= 40; seq++ {
		op, note := workload.OpInsert, fmt.Sprintf("%s-%d", id, seq)
		if seq%7 == 0 {
			op, note = workload.OpRollback, workload.RolledBackNote
		}
		opID := fmt.Sprintf("%s-%06d", id, seq)
		amount := seq*37%1000 + 1
		answer := workload.Event{T: at, Client: id, Seq: seq, Op: op, OpID: opID, Endpoint: "rw",
			Event: workload.Acknowledged}
		if fail[seq] {
			answer.Event, answer.Error = workload.Failed, "connection reset"
		}
		for _, e := range []workload.Event{{T: at, Client: id, Seq: seq, Op: op, OpID: opID, Endpoint: "rw",
			Event: workload.Attempted, Amount: amount, Note: note}, answer} {
			data, _ := json.Marshal(e)
			lines = append(lines, string(data))
		}
		if op == workload.OpInsert && !fail[seq] {
			rows = append(rows, workload.Row{OpID: opID, Client: id, Seq: seq, Amount: amount, Note: note})
		}
		at = at.Add(100 * time.Millisecond)
	}
	return strings.Join(lines, "\n") + "\n", rows
}

func s06Scenario(t *testing.T, dir string) (evidence.Summary, evidence.Scenario, evidence.Scenario) {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s06Lane || len(summary.Scenarios) != 2 {
		t.Fatalf("summary = %+v, want LAB01 and S06/restart in lane %s", summary, s06Lane)
	}
	return summary, summary.Scenarios[0], summary.Scenarios[1]
}

func TestRunS06RestartPassesWhenTheReplacementWritesAFreshStatus(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	summary, lab, s06 := s06Scenario(t, cfg.EvidenceDir)
	if !summary.Passed || lab.Result != evidence.Pass || s06.Result != evidence.Pass || s06.Variant != s06Variant {
		t.Fatalf("summary = %+v, want both passed", summary)
	}
	if !f.managerDeleted || s06.FaultConfirmed == nil || !*s06.FaultConfirmed {
		t.Fatalf("S06 = %+v, want the manager Pod deleted and the fault confirmed", s06)
	}
	if s06.Operations == nil || s06.Operations.Acknowledged == 0 || s06.Measurements["operationsNotAcknowledged"] != 0.0 {
		t.Fatalf("S06 operations = %+v, measurements = %v", s06.Operations, s06.Measurements)
	}
	var rec s06Record
	data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, s06RecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S06 record = %s, %v", data, err)
	}
	if rec.ManagerAfter == nil || rec.ManagerAfter.Pod != testReplacementPod || rec.LastObserved == nil ||
		rec.GoneAt == nil || !rec.LastObserved.After(*rec.GoneAt) || !rec.LastObserved.After(*rec.ReplacementAt) {
		t.Fatalf("S06 record = %+v, want the replacement's own status after it started and after the Pod was gone", rec)
	}
	for _, rel := range []string{s06RecordFile, s06TimelineFile, s06DataCheckFile} {
		if !strings.Contains(strings.Join(s06.Evidence, ","), rel) {
			t.Fatalf("S06 evidence = %v, want %s", s06.Evidence, rel)
		}
	}
	requireOwnEvidence(t, lab, haScenarioID)
	requireOwnEvidence(t, s06, "S06/restart")
	requireNamespaceGone(t, f)
}

func TestRunS06RestartFailsWithoutAStatusFromTheReplacement(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	// The replacement starts but writes no observation of its own.
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.startReplacement(ctx, c)
		f.takeLease(ctx, c)
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "time limit exceeded") {
		t.Fatalf("RunHA() error = %v, want no fresh status within the limit", err)
	}
	_, lab, s06 := s06Scenario(t, cfg.EvidenceDir)
	if lab.Result != evidence.Pass || s06.Result != evidence.Fail {
		t.Fatalf("LAB01 = %s, S06 = %s; want LAB01 kept and S06 failed", lab.Result, s06.Result)
	}
}

func TestRunS06RestartFailsWhenTheMasterMoves(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.mu.Lock()
		f.roles[testHAPrimary], f.roles[testMemberHA0] = roleSlave, roleMaster
		f.primary = testMemberHA0
		f.mu.Unlock()
		f.startReplacement(ctx, c)
		f.takeLease(ctx, c)
		f.writeStatus(ctx, c)
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "master changed") {
		t.Fatalf("RunHA() error = %v, want the moved master", err)
	}
	if _, _, s06 := s06Scenario(t, cfg.EvidenceDir); s06.Result != evidence.Fail {
		t.Fatalf("S06 = %+v, want a failure", s06)
	}
}

func TestRunS06RestartFailsOnAClientOperationThatFailedAfterTheFault(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	f.backgroundHistory = func(id string) string {
		history, rows := backgroundWorkload(id, map[int]bool{3: id == s06Clients[0]})
		f.mu.Lock()
		f.rows = append(f.rows, rows...)
		f.mu.Unlock()
		return history
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "failed or stayed unanswered") {
		t.Fatalf("RunHA() error = %v, want the failed operation", err)
	}
	_, _, s06 := s06Scenario(t, cfg.EvidenceDir)
	if s06.Result != evidence.Fail || len(s06.NeverEvents) == 0 {
		t.Fatalf("S06 = %+v, want a failure with the event that must never happen", s06)
	}
}

func TestRunS06RestartFailsWhenAMemberRestarted(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		pod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: testMemberLast}, pod); err != nil {
			t.Error(err)
			return
		}
		pod.Status.ContainerStatuses[0].ContainerID = "containerd://restarted"
		if err := c.Status().Update(ctx, pod); err != nil {
			t.Error(err)
		}
		f.startReplacement(ctx, c)
		f.takeLease(ctx, c)
		f.writeStatus(ctx, c)
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "members were replaced or restarted") {
		t.Fatalf("RunHA() error = %v, want the restarted member", err)
	}
}

func TestRunS06IsBlockedWhenLAB01Failed(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	f.initErr = errors.New("tables not created")
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if f.managerDeleted {
		t.Fatal("the manager Pod was deleted although LAB01 did not pass")
	}
	_, lab, s06 := s06Scenario(t, cfg.EvidenceDir)
	if lab.Result != evidence.Fail || s06.Result != evidence.Blocked {
		t.Fatalf("LAB01 = %s, S06 = %s; want fail and blocked", lab.Result, s06.Result)
	}
}

func TestRunHARefusesAnUnknownFault(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	cfg.Fault = "node-power-off"
	if err := f.runner.RunHA(t.Context(), cfg); err == nil || len(f.writes) != 0 {
		t.Fatalf("RunHA() error = %v, writes = %v; want the fault refused before anything is done", err, f.writes)
	}
}

func TestRunS06RestartWaitsForAStatusWrittenUnderTheReplacementsLease(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	// A fresh status time, but the old process still holds the Lease: it may
	// have written it.
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.startReplacement(ctx, c)
		f.writeStatus(ctx, c)
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "time limit exceeded") {
		t.Fatalf("RunHA() error = %v, want no status the replacement wrote", err)
	}
}

func TestRunS06RestartWaitsForAStatusLaterThanTheReplacementsStart(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	// The status time is after the fault, but not after the replacement
	// started.
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.writeStatus(ctx, c)
		f.startReplacementAt(ctx, c, time.Now().Add(time.Minute))
		f.takeLease(ctx, c)
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "time limit exceeded") {
		t.Fatalf("RunHA() error = %v, want no status later than the replacement's start", err)
	}
}

func TestRunS06RestartIsBlockedByAClockOffTheRuns(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	f.clockSkew = 3 * time.Second
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "off the run's") {
		t.Fatalf("RunHA() error = %v, want the clock offset refused", err)
	}
	if f.managerDeleted {
		t.Fatal("the manager Pod was deleted with clocks the run cannot compare")
	}
	if _, _, s06 := s06Scenario(t, cfg.EvidenceDir); s06.Result != evidence.Blocked {
		t.Fatalf("S06 = %+v, want blocked", s06)
	}
}

func TestRunS06RestartFailsOverTheResyncLimit(t *testing.T) {
	cfg := testS06Config(t)
	cfg.ResyncLimit = time.Nanosecond
	f := newS06Fixture(t, cfg)
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "over operator_resync_limit") {
		t.Fatalf("RunHA() error = %v, want the resync limit exceeded", err)
	}
}

func TestRunS06RestartFailsUnderTheStablePeriod(t *testing.T) {
	cfg := testS06Config(t)
	// The clients' answers span 3.9 seconds.
	cfg.StablePeriod = 5 * time.Second
	f := newS06Fixture(t, cfg)
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "under stable_period") {
		t.Fatalf("RunHA() error = %v, want the stable period not reached", err)
	}
}

func TestRunS06RestartFailsWhenAClientsLastAnswerIsAFailure(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	f.backgroundHistory = func(id string) string {
		history, rows := backgroundWorkload(id, map[int]bool{40: id == s06Clients[1]})
		f.mu.Lock()
		f.rows = append(f.rows, rows...)
		f.mu.Unlock()
		return history
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "last answer was not an acknowledgement: ["+s06Clients[1]+"]") {
		t.Fatalf("RunHA() error = %v, want the client that did not recover", err)
	}
}

func TestRunS06RestartStopsTheClientsWhenItEndsEarly(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.mu.Lock()
		f.roles[testHAPrimary], f.roles[testMemberHA0] = roleSlave, roleMaster
		f.mu.Unlock()
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if f.kills != len(s06Clients) {
		t.Fatalf("clients stopped %d times, want each of %d once", f.kills, len(s06Clients))
	}
}

func TestRunS06RestartWaitsForAStatusLaterThanTheLeaseAcquisition(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	// The replacement holds the Lease, but the status time comes before it
	// acquired it: the old process may have written it.
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.startReplacement(ctx, c)
		f.writeStatus(ctx, c)
		f.takeLeaseAt(ctx, c, time.Now().Add(time.Minute))
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "time limit exceeded") {
		t.Fatalf("RunHA() error = %v, want no status later than the Lease acquisition", err)
	}
}

func TestRunS06RestartFailsWhenAClientDoesNotStop(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	f.stopAnswer = "running"
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("RunHA() error = %v, want the client that did not stop", err)
	}
}

func TestRunS06RestartIsBlockedWhenTheReplacementRunsAnotherImage(t *testing.T) {
	cfg := testS06Config(t)
	f := newS06Fixture(t, cfg)
	f.replacement = func(ctx context.Context, c client.WithWatch) {
		f.startReplacement(ctx, c)
		pod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: testReplacementPod}, pod); err != nil {
			t.Error(err)
			return
		}
		pod.Status.ContainerStatuses[0].ImageID = "containerd://sha256:" + strings.Repeat("e", 64)
		if err := c.Status().Update(ctx, pod); err != nil {
			t.Error(err)
		}
		f.takeLease(ctx, c)
		f.writeStatus(ctx, c)
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if _, _, s06 := s06Scenario(t, cfg.EvidenceDir); s06.Result != evidence.Blocked {
		t.Fatalf("S06 = %+v, want blocked by the changed image", s06)
	}
}
