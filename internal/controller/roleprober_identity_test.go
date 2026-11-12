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

package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	metricspkg "github.com/cubrid-lab/cubrid-kubernetes-operator/internal/metrics"
)

// statusPerMember answers each member's GET /v1/role with the status the test
// set for it, sent as JSON as it is, without ParseHAStatus. It stands for an
// Instance Manager of another build, or an answer changed on the way, that
// does not fill in the member's name.
type statusPerMember map[string]instancemanager.HAStatus

func (a statusPerMember) RoundTrip(req *http.Request) (*http.Response, error) {
	member, _, _ := strings.Cut(req.URL.Hostname(), ".")
	return answerWith{a[member]}.RoundTrip(req)
}

// A master or slave role is the asked member's role only when the answer names
// that member as current. An answer that names no member, or another one,
// cannot be told apart from an answer of another member reached through a
// wrong DNS record or Service, so it is not trusted (#839, #200). An answer
// without a role names nobody and stays an ordinary answer without a role.
func TestProbeRole_HARoleAnswerMustNameTheAskedMember(t *testing.T) {
	probe := func(answer instancemanager.HAStatus) RoleObservation {
		p := &HTTPRoleProber{Client: &http.Client{Transport: answerWith{answer}}, Tokens: staticToken("tok"),
			Now: func() time.Time { return testNow }}
		return p.ProbeRole(context.Background(), c0, "ns")
	}
	cases := []struct {
		name        string
		status      instancemanager.HAStatus
		role        databasev1alpha1.CubridRole
		conflicting bool
		reason      string
	}{
		{"a master that names no member", instancemanager.HAStatus{Role: instancemanager.RoleMaster, ServerActive: true},
			databasev1alpha1.RoleMaster, true, reasonAnswerNamesNoMember},
		{"a master that names no member and has a node list", instancemanager.HAStatus{
			Role: instancemanager.RoleMaster, ServerActive: true,
			Nodes: []instancemanager.NodeState{node(c1, "slave")}},
			databasev1alpha1.RoleMaster, true, reasonAnswerNamesNoMember},
		{"a slave that names no member", instancemanager.HAStatus{Role: instancemanager.RoleSlave},
			databasev1alpha1.RoleSlave, true, reasonAnswerNamesNoMember},
		{"a master that names another member", instancemanager.HAStatus{Current: c1, Role: instancemanager.RoleMaster,
			ServerActive: true, Nodes: []instancemanager.NodeState{node(c1, "master")}},
			databasev1alpha1.RoleMaster, true, "the answer names the member " + c1},
		{"a master that names the asked member", haStatus(instancemanager.RoleMaster, true, node(c0, "master"),
			node(c1, "slave")), databasev1alpha1.RoleMaster, false, ""},
		{"a slave that names the asked member", haStatus(instancemanager.RoleSlave, false, node(c0, "slave"),
			node(c1, "master")), databasev1alpha1.RoleSlave, false, ""},
		{"an unknown role that names no member", instancemanager.HAStatus{Role: instancemanager.RoleUnknown},
			databasev1alpha1.RoleUnknown, false, "the Instance Manager reported no role"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := probe(c.status)
			if !got.Reachable || got.Role != c.role || got.Conflicting != c.conflicting || got.Reason != c.reason {
				t.Errorf("got %+v, want role %s conflicting %v reason %q", got, c.role, c.conflicting, c.reason)
			}
			if c.conflicting && (got.ActiveMasterClaim || got.MasterNotYetActive) {
				t.Errorf("an answer that is not this member's carries a master claim: %+v", got)
			}
		})
	}
}

// A master that names no member and a slave that names itself must not resolve
// a primary: the master's answer is not shown to be the asked member's (#839).
func TestResolvePrimary_MasterThatNamesNoMemberIsNoPrimary(t *testing.T) {
	answers := statusPerMember{
		c0: {Role: instancemanager.RoleMaster, ServerActive: true},
		c1: {Current: c1, Role: instancemanager.RoleSlave,
			Nodes: []instancemanager.NodeState{node(c0, "master"), node(c1, "slave")}},
	}
	p := &HTTPRoleProber{Client: &http.Client{Transport: answers}, Tokens: staticToken("tok"),
		Now: func() time.Time { return testNow }}
	obs := map[string]RoleObservation{}
	for _, m := range []string{c0, c1} {
		obs[m] = p.ProbeRole(context.Background(), m, "ns")
	}
	if authoritative(obs[c0], testNow) {
		t.Errorf("a master answer that names no member is authoritative: %+v", obs[c0])
	}
	if !authoritative(obs[c1], testNow) {
		t.Errorf("a slave answer that names the asked member is not authoritative: %+v", obs[c1])
	}
	res := resolvePrimary([]string{c0, c1}, obs, testNow)
	if res.Status == metav1.ConditionTrue || res.CurrentPrimary != "" || res.Reason == "SinglePrimaryObserved" {
		t.Errorf("a master that names no member resolved a primary: %+v", res)
	}
}

// Through the real prober and reconcile: when the first member answers master
// without naming itself and the two others answer slave in their own names,
// the cluster reports no primary, and neither PrimaryResolved, HAReady nor
// RoutingReady, nor their metrics, turn on (#839). Ready is not checked: while
// PrimaryResolved has been False for less than primaryUnresolvedGrace, Ready
// follows the Pods' readiness (#716). The answers are synthetic;
// they do not show that the engine sends such an answer.
var _ = Describe("An HA role answer that names no member (#839)", func() {
	ctx := context.Background()

	It("resolves no primary and reports no HA or routing readiness", func() {
		const name = "unnamed-master"
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			metricspkg.DeleteCluster(key.Namespace, key.Name)
		})
		m0, m1, m2 := name+"-0", name+"-1", name+"-2"
		nodes := []instancemanager.NodeState{node(m0, "master"), node(m1, "slave"), node(m2, "slave")}
		answers := statusPerMember{
			// No node list: a list that shows the master under a name would
			// already contradict an answer that names no member.
			m0: {Role: instancemanager.RoleMaster, ServerActive: true},
			m1: {Current: m1, Role: instancemanager.RoleSlave, Nodes: nodes},
			m2: {Current: m2, Role: instancemanager.RoleSlave, Nodes: nodes},
		}
		clock := time.Now().Truncate(time.Second)
		now := func() time.Time { return clock }
		prober := &HTTPRoleProber{Client: &http.Client{Transport: answers}, Tokens: staticToken("tok"), Now: now}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: prober, Clock: now}
		pass := func() *databasev1alpha1.CubridCluster {
			GinkgoHelper()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			got := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			return got
		}

		_ = pass()
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		sts.Status.Replicas, sts.Status.ReadyReplicas = 3, 3
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

		got := pass()
		Expect(got.Status.CurrentPrimary).To(BeEmpty())
		for _, condition := range []string{conditionPrimaryResolved, conditionHAReady, conditionRoutingReady} {
			c := meta.FindStatusCondition(got.Status.Conditions, condition)
			Expect(c).NotTo(BeNil(), condition)
			Expect(c.Status).NotTo(Equal(metav1.ConditionTrue), condition)
		}
		Expect(meta.FindStatusCondition(got.Status.Conditions, conditionPrimaryResolved).Reason).
			To(Equal("AmbiguousPrimaryObservation"))
		for _, gauge := range []*prometheus.GaugeVec{metricspkg.ClusterPrimaryResolved, metricspkg.ClusterHAReady,
			metricspkg.ClusterRoutingReady} {
			Expect(testutil.ToFloat64(gauge.WithLabelValues(key.Namespace, key.Name))).To(BeZero())
		}
	})
})
