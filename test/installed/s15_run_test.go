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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// These tests run the whole S15 lane through RunHA against the HA fixture,
// which here also plays the Eviction API with a members' budget of
// maxUnavailable 1, the StatefulSet's Pending replacement behind a volume
// bound to its node, and the scheduler once the node is schedulable again.

const (
	// testOtherRelease labels Pods of another release in the run namespace.
	testOtherRelease = "another-release"
	// testSlaveDrainNode runs ha-0, the slave drain-slave-node drains in the
	// HA fixture, where the client runs on node-3.
	testSlaveDrainNode = "node-1"
)

func testS15Config(t *testing.T, fault string) HAConfig {
	t.Helper()
	cfg := testHAConfig(t)
	cfg.Level, cfg.Timeout, cfg.Fault = evidence.LevelVMLab, 30*time.Second, fault
	cfg.FailoverLimit, cfg.DrainLimit, cfg.StablePeriod = time.Minute, time.Minute, 10*time.Millisecond
	return cfg
}

// s15Cluster plays what a drain does to the fixture's cluster.
type s15Cluster struct {
	f *haFixture
	// allowSecond lets the second member eviction go, as a cluster without
	// the members' budget would.
	allowSecond bool
	evicted     []string
	// pending is the member whose replacement waits for pendingNode.
	pending, pendingNode string
}

func newS15Fixture(t *testing.T, cfg HAConfig) (*haFixture, *s15Cluster) {
	t.Helper()
	nodes := make([]client.Object, 0, 7)
	// A Broker Pod of the run on each node a variant drains, and one of
	// another release on the same node, which the run must leave alone.
	for i := range 2 {
		node := fmt.Sprintf("node-%d", i+1)
		nodes = append(nodes, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ha-broker-rw-%d", i),
			UID: types.UID(fmt.Sprintf("broker-%d", i)), Namespace: cfg.Namespace, Labels: map[string]string{
				componentLabel: brokerComponent, instanceLabel: haClusterName}}, Spec: corev1.PodSpec{NodeName: node}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("other-broker-%d", i),
				UID: types.UID(fmt.Sprintf("other-%d", i)), Namespace: cfg.Namespace, Labels: map[string]string{
					componentLabel: brokerComponent, instanceLabel: testOtherRelease}}, Spec: corev1.PodSpec{NodeName: node}})
	}
	for i := 1; i <= 3; i++ {
		nodes = append(nodes, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i)},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady,
				Status: corev1.ConditionTrue}}}})
	}
	f := newHAFixture(t, cfg, nodes...)
	s := &s15Cluster{f: f}
	f.backgroundHistory = func(id string) string { return s.history(id) }
	f.podRead = s.podRead
	f.evicted = s.evict
	f.clientHook = s.client
	return f, s
}

func (s *s15Cluster) history(id string) string {
	history, rows := backgroundWorkload(id, nil)
	s.f.mu.Lock()
	s.f.rows = append(s.f.rows, rows...)
	s.f.mu.Unlock()
	return history
}

// client answers the writes through new connections, and the history and
// diagnostics of the client that held one connection open.
func (s *s15Cluster) client(script string) (string, error, bool) {
	switch {
	case strings.HasPrefix(script, "CLIENT_ID="+s15ProbePrefix):
		id, _, _ := strings.Cut(strings.TrimPrefix(script, "CLIENT_ID="), " ")
		at := time.Now().UTC()
		row := workload.Row{OpID: id + "-000001", Client: id, Seq: 1, Amount: 38, Note: id + "-1"}
		lines := make([]string, 0, 2)
		for _, e := range []workload.Event{
			{T: at, Client: id, Seq: 1, Op: workload.OpInsert, OpID: row.OpID, Endpoint: "rw",
				Event: workload.Attempted, Amount: row.Amount, Note: row.Note},
			{T: at, Client: id, Seq: 1, Op: workload.OpInsert, OpID: row.OpID, Endpoint: "rw",
				Event: workload.Acknowledged}} {
			data, _ := json.Marshal(e)
			lines = append(lines, string(data))
		}
		s.f.mu.Lock()
		s.f.rows = append(s.f.rows, row)
		s.f.mu.Unlock()
		return strings.Join(lines, "\n") + "\n", nil, true
	case script == "cat /tmp/"+s15OpenClient+".jsonl":
		return s.history(s15OpenClient), nil, true
	case script == "cat /tmp/"+s15OpenClient+".diag.jsonl":
		return `{"event":"connection_returned"}` + "\n", nil, true
	}
	return "", nil, false
}

// podRead gives the members the Operator's labels and a volume bound to
// their node, creates the cluster's budgets, and schedules a Pending
// replacement once its node is schedulable again.
func (s *s15Cluster) podRead(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	f := s.f
	if !strings.HasPrefix(key.Name, haClusterName+"-") || strings.Contains(key.Name, "broker") {
		return nil
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, key, pod); err != nil {
		return nil
	}
	if pod.Labels[componentLabel] == "" {
		pod.Labels = map[string]string{instanceLabel: haClusterName, componentLabel: memberComponent}
		claim := "data-" + pod.Name
		pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}
		if err := c.Update(ctx, pod); err != nil {
			return err
		}
		pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-" + pod.Name},
			Spec: corev1.PersistentVolumeSpec{NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{Key: hostnameLabel,
						Operator: corev1.NodeSelectorOpIn, Values: []string{pod.Spec.NodeName}}}}}}}}}
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: key.Namespace},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name}}
		if err := createAll(ctx, c, pv, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		one := intstr.FromInt32(1)
		for _, name := range []string{memberComponent, "broker-rw", "broker-ro"} {
			pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: haClusterName + "-" + name,
				Namespace: key.Namespace}, Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &one},
				Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 1, ExpectedPods: 3, CurrentHealthy: 3}}
			if err := c.Create(ctx, pdb); err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
		}
	}
	if key.Name != s.pending || pod.Spec.NodeName != "" {
		return nil
	}
	node := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: s.pendingNode}, node); err != nil || node.Spec.Unschedulable {
		return err
	}
	pod.Spec.NodeName = node.Name
	if err := c.Update(ctx, pod); err != nil {
		return err
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: databaseContainer, Ready: true,
			ContainerID: "containerd://" + pod.Name + "-again",
			State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			ImageID:     "containerd://sha256:" + strings.Repeat("c", 64)}}}
	if err := c.Status().Update(ctx, pod); err != nil {
		return err
	}
	s.pending = ""
	f.mu.Lock()
	f.roles[key.Name] = roleSlave
	primary := f.primary
	f.mu.Unlock()
	return s.setConditions(ctx, c, metav1.ConditionTrue, primary)
}

func (s *s15Cluster) setConditions(ctx context.Context, c client.Client, status metav1.ConditionStatus,
	primary string) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: s.f.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return err
	}
	// Ready and HAReady.
	for _, condition := range startingConditions[:2] {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: condition, Status: status,
			Reason: testConditionReason})
	}
	cluster.Status.CurrentPrimary = primary
	return c.Update(ctx, cluster)
}

// evict plays the Eviction API under the members' budget: the first member
// goes, and its StatefulSet recreates it Pending behind its volume; a second
// member is refused while the first is missing. Brokers go.
func (s *s15Cluster) evict(ctx context.Context, c client.Client, obj client.Object) error {
	f := s.f
	pod := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), pod); err != nil {
		return err
	}
	if pod.Labels[componentLabel] != memberComponent {
		return c.Delete(ctx, pod)
	}
	if len(s.evicted) > 0 && !s.allowSecond {
		return pdbRefusal()
	}
	s.evicted = append(s.evicted, pod.Name)
	if len(s.evicted) > 1 {
		return c.Delete(ctx, pod)
	}
	if err := c.Delete(ctx, pod); err != nil {
		return err
	}
	replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace,
		UID: types.UID(pod.Name + "-replacement"), Labels: pod.Labels, OwnerReferences: pod.OwnerReferences},
		Spec: corev1.PodSpec{Containers: pod.Spec.Containers, Volumes: pod.Spec.Volumes},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled,
			Status: corev1.ConditionFalse, Reason: "Unschedulable",
			Message: "0/3 nodes are available: 1 node(s) were unschedulable, 2 node(s) had volume node affinity conflict"}}}}
	if err := c.Create(ctx, replacement); err != nil {
		return err
	}
	s.pending, s.pendingNode = pod.Name, pod.Spec.NodeName
	f.mu.Lock()
	f.roles[pod.Name] = ""
	if pod.Name == testHAPrimary {
		f.roles[testMemberHA0], f.primary = roleMaster, testMemberHA0
	}
	primary := f.primary
	f.mu.Unlock()
	return s.setConditions(ctx, c, metav1.ConditionFalse, primary)
}

func s15RunScenario(t *testing.T, dir, variant string) evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s15LanePrefix+variant || len(summary.Scenarios) != 2 {
		t.Fatalf("summary = %+v, want LAB01 and S15/%s in lane %s", summary, variant, s15LanePrefix+variant)
	}
	return summary.Scenarios[1]
}

func readS15Record(t *testing.T, dir, variant string) s15Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, s15File(variant, "record.json")))
	if err != nil {
		t.Fatal(err)
	}
	var rec s15Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestRunS15PassesWhenTheBudgetHoldsTheSecondMember(t *testing.T) {
	for _, tc := range []struct{ variant, target, second, final string }{
		{FaultDrainSlaveNode, testMemberHA0, testMemberLast, testHAPrimary},
		// The second eviction waits for the promotion and targets the slave:
		// ha-0 is promoted, so ha-2 is asked for.
		{FaultDrainMasterNode, testHAPrimary, testMemberLast, testMemberHA0},
	} {
		t.Run(tc.variant, func(t *testing.T) {
			cfg := testS15Config(t, tc.variant)
			f, _ := newS15Fixture(t, cfg)
			if err := f.runner.RunHA(t.Context(), cfg); err != nil {
				t.Fatalf("RunHA() = %v", err)
			}
			s15 := s15RunScenario(t, cfg.EvidenceDir, tc.variant)
			if s15.Result != evidence.Pass || s15.Limits[drainLimitName] != "1m0s" ||
				s15.Measurements["secondEvictionOutcome"] != evictionPDBBlocked {
				t.Fatalf("S15 = %+v, want a pass judged by drain_limit with the second eviction refused", s15)
			}
			rec := readS15Record(t, cfg.EvidenceDir, tc.variant)
			if rec.TargetPod.Pod != tc.target || rec.SecondMember != tc.second || rec.FinalMaster != tc.final ||
				!rec.CordonedByRun || !rec.Uncordoned || rec.PendingAt == nil || rec.SQLContinuedAt == nil ||
				rec.OpenConnection == nil || !rec.OpenConnection.OpenAtEnd {
				t.Fatalf("record = %+v, want %s drained, %s refused and master %s at the end", rec, tc.target,
					tc.second, tc.final)
			}
			requireOwnEvidence(t, s15, s15ID+"/"+tc.variant)
			brokers := 0
			for _, e := range rec.Evictions {
				if e.Kind == evictBroker && e.Outcome == evictionEvicted {
					brokers++
				}
			}
			if brokers != 1 || len(rec.ForeignEvictions) != 0 {
				t.Fatalf("evictions %+v, foreign %v; want the run's one Broker on the node evicted and nothing else",
					rec.Evictions, rec.ForeignEvictions)
			}
			other := &corev1.PodList{}
			err := f.runner.Client.List(t.Context(), other, client.MatchingLabels{instanceLabel: testOtherRelease})
			if err != nil || len(other.Items) != 2 {
				t.Fatalf("other release's Pods = %d (%v), want both left", len(other.Items), err)
			}
		})
	}
}

// A run with the default drain limit, unset, is a baseline: every check
// held, and it is blocked, never passed.
func TestRunS15WithoutADrainLimitIsABaseline(t *testing.T) {
	cfg := testS15Config(t, FaultDrainSlaveNode)
	cfg.DrainLimit = 0
	f, _ := newS15Fixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() = nil, want the baseline reported as not passed")
	}
	s15 := s15RunScenario(t, cfg.EvidenceDir, FaultDrainSlaveNode)
	if s15.Result != evidence.Blocked || s15.Reason != faults.ReasonTimeLimitUnset ||
		s15.Limits[drainLimitName] != "unset" || s15.Measurements["drainWithin"] == evidence.Unknown {
		t.Fatalf("S15 = %+v, want blocked with time_limit_unset and drainWithin measured", s15)
	}
}

// Without a budget that refuses it, the second eviction takes a second
// member: S15 fails with the never-event, and the node is uncordoned anyway.
func TestRunS15FailsWhenTheSecondMemberIsEvicted(t *testing.T) {
	cfg := testS15Config(t, FaultDrainSlaveNode)
	f, cluster := newS15Fixture(t, cfg)
	cluster.allowSecond = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() = nil, want S15 failed")
	}
	s15 := s15RunScenario(t, cfg.EvidenceDir, FaultDrainSlaveNode)
	if s15.Result != evidence.Fail || len(s15.NeverEvents) == 0 ||
		!strings.Contains(s15.NeverEvents[0], "two members were evicted") {
		t.Fatalf("S15 = %+v, want a fail naming two members evicted", s15)
	}
	node := &corev1.Node{}
	if err := f.runner.Client.Get(t.Context(), client.ObjectKey{Name: testSlaveDrainNode}, node); err != nil {
		t.Fatal(err)
	}
	if rec := readS15Record(t, cfg.EvidenceDir, FaultDrainSlaveNode); node.Spec.Unschedulable || !rec.Uncordoned {
		t.Fatalf("node unschedulable %t, uncordoned %t; want the run's cordon undone after the failure",
			node.Spec.Unschedulable, rec.Uncordoned)
	}
}
