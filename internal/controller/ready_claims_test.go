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
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

// heartbeatAnswers exercises the parser and the HTTP prober's identity check
// before an observation reaches the controller. These are synthetic reports,
// not evidence that the engine produces two active masters.
type heartbeatAnswers map[string]string

func (a heartbeatAnswers) RoundTrip(req *http.Request) (*http.Response, error) {
	member, _, _ := strings.Cut(req.URL.Hostname(), ".")
	return answerWith{instancemanager.ParseHAStatus(a[member])}.RoundTrip(req)
}

func masterClaimReports(name string, twoMasters bool) heartbeatAnswers {
	roles := []instancemanager.Role{instancemanager.RoleMaster, instancemanager.RoleSlave, instancemanager.RoleSlave}
	if twoMasters {
		roles[1] = instancemanager.RoleMaster
	}
	answers := heartbeatAnswers{}
	for i, role := range roles {
		server := "registered_and_standby"
		if role == instancemanager.RoleMaster {
			server = "registered_and_active"
		}
		answers[fmt.Sprintf("%s-%d", name, i)] = fmt.Sprintf(` HA-Node Info (current %s-%d, state %s)
   Node %s-0 (priority 1, state %s)
   Node %s-1 (priority 2, state %s)
   Node %s-2 (priority 3, state %s)
   Server appdb (pid 272, state %s)
`, name, i, role, name, roles[0], name, roles[1], name, roles[2], server)
	}
	return answers
}

func TestReadyMasterClaims(t *testing.T) {
	cases := []struct {
		name       string
		oneMaster  bool
		editReport func(heartbeatAnswers)
		editObs    func(map[string]RoleObservation)
		multiple   bool
		reason     string
	}{
		{name: "two local active masters that see each other", multiple: true, reason: ambiguous},
		{name: "one authoritative and one conflicting active master", multiple: true, reason: ambiguous,
			editReport: func(a heartbeatAnswers) {
				a[c0] = strings.ReplaceAll(a[c0], "priority 2, state master", "priority 2, state slave")
			}},
		{name: "one master", oneMaster: true, reason: singlePrimary},
		{name: "ordinary incomplete observation", oneMaster: true, reason: incomplete,
			editReport: func(a heartbeatAnswers) { a[c2] = "" }},
		{name: "one report sees a remote master", reason: ambiguous,
			editReport: func(a heartbeatAnswers) { a[c1] = masterClaimReports("c", false)[c1] }},
		{name: "another member answers the second request", reason: ambiguous,
			editReport: func(a heartbeatAnswers) { a[c1] = a[c0] }},
		{name: "second master is inactive", reason: ambiguous,
			editReport: func(a heartbeatAnswers) {
				a[c1] = strings.ReplaceAll(a[c1], "registered_and_active", "registered_and_standby")
			}},
		{name: "second master contradicts its own node line", reason: ambiguous,
			editReport: func(a heartbeatAnswers) {
				a[c1] = strings.ReplaceAll(a[c1], "priority 2, state master", "priority 2, state slave")
			}},
		{name: "second master is missing its own node line", reason: ambiguous,
			editReport: func(a heartbeatAnswers) {
				a[c1] = strings.ReplaceAll(a[c1], "Node c-1 (priority 2, state master)", "")
			}},
		{name: "second master has duplicate local entries", reason: ambiguous,
			editReport: func(a heartbeatAnswers) { a[c1] += "   Node c-1 (priority 2, state master)\n" }},
		{name: "second report has no local identity", reason: ambiguous,
			editReport: func(a heartbeatAnswers) {
				a[c1] = strings.ReplaceAll(a[c1], "current c-1", "current ")
			}},
		{name: "stale second claim", reason: ambiguous,
			editObs: func(obs map[string]RoleObservation) { obs[c1] = aged(obs[c1], roleObservationTTL+time.Second) }},
		{name: "future second claim", reason: ambiguous,
			editObs: func(obs map[string]RoleObservation) { obs[c1] = aged(obs[c1], -roleObservationTTL-time.Second) }},
		{name: "unstamped second claim", reason: ambiguous,
			editObs: func(obs map[string]RoleObservation) {
				o := obs[c1]
				o.ObservedAt = time.Time{}
				obs[c1] = o
			}},
		{name: "unreachable second claimant", reason: ambiguous,
			editObs: func(obs map[string]RoleObservation) {
				o := obs[c1]
				o.Reachable = false
				obs[c1] = o
			}},
		{name: "unexpected claimant outside the member set", reason: ambiguous,
			editObs: func(obs map[string]RoleObservation) {
				obs["unexpected"] = obs[c1]
				delete(obs, c1)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answers := masterClaimReports("c", !tc.oneMaster)
			if tc.editReport != nil {
				tc.editReport(answers)
			}
			prober := &HTTPRoleProber{Client: &http.Client{Transport: answers}, Tokens: staticToken("tok"),
				Now: func() time.Time { return testNow }}
			members := []string{c0, c1, c2}
			obs := map[string]RoleObservation{}
			for _, m := range members {
				obs[m] = prober.ProbeRole(context.Background(), m, "default")
			}
			if tc.editObs != nil {
				tc.editObs(obs)
			}
			res := resolvePrimary(members, obs, testNow)
			if res.MultipleMasters != tc.multiple || res.Reason != tc.reason {
				t.Fatalf("resolution = %+v, want multiple=%t reason=%s", res, tc.multiple, tc.reason)
			}
			if tc.reason != singlePrimary && (res.CurrentPrimary != "" || res.Status != metav1.ConditionFalse) {
				t.Fatalf("an unresolved snapshot authorized a primary: %+v", res)
			}
			cluster := haCluster("c")
			r := &CubridClusterReconciler{}
			r.setReady(cluster, 3, 3, res.MultipleMasters)
			if meta.IsStatusConditionTrue(cluster.Status.Conditions, conditionReady) == tc.multiple {
				t.Fatalf("Ready must be false exactly when multiple masters are observed: %+v", cluster.Status.Conditions)
			}
			for _, in := range instanceStatuses(members, obs, testNow, nil) {
				if obs[in.Name].Conflicting && in.Role != databasev1alpha1.RoleUnknown {
					t.Errorf("conflicting member %s acquired authority: %+v", in.Name, in)
				}
			}
		})
	}
}

// The HTTP API can return a status that did not pass through ParseHAStatus.
// A role alone remains subject to the existing primary-resolution rules, but
// cannot substitute for the explicit local evidence used by Ready.
func TestReadyMasterClaimsWithoutLocalEvidence(t *testing.T) {
	cases := []struct {
		name   string
		status instancemanager.HAStatus
	}{
		{"missing identity and nodes", instancemanager.HAStatus{Role: instancemanager.RoleMaster, ServerActive: true}},
		{"missing local entry", instancemanager.HAStatus{Current: c1, Role: instancemanager.RoleMaster, ServerActive: true}},
		{"duplicate local entries", instancemanager.HAStatus{Current: c1, Role: instancemanager.RoleMaster, ServerActive: true,
			Nodes: []instancemanager.NodeState{node(c1, string(instancemanager.RoleMaster)), node(c1, string(instancemanager.RoleMaster))}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := func(member string, status instancemanager.HAStatus) RoleObservation {
				p := &HTTPRoleProber{Client: &http.Client{Transport: answerWith{status}}, Tokens: staticToken("tok"),
					Now: func() time.Time { return testNow }}
				return p.ProbeRole(context.Background(), member, "default")
			}
			obs := map[string]RoleObservation{
				c0: probe(c0, instancemanager.ParseHAStatus(masterClaimReports("c", false)[c0])),
				c1: probe(c1, tc.status),
			}
			res := resolvePrimary([]string{c0, c1}, obs, testNow)
			if res.MultipleMasters || obs[c1].ActiveMasterClaim {
				t.Fatalf("a response without local evidence counted as a second active master: %+v", obs[c1])
			}
			if res.Reason != reasonMultiplePrimaries || res.Status != metav1.ConditionFalse || res.CurrentPrimary != "" {
				t.Fatalf("existing primary authority rules changed: %+v", res)
			}
		})
	}
}

var _ = Describe("Ready from conflicting active-master claims (#339)", func() {
	It("keeps Ready and its metric off without authorizing either master", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "conflicting-masters", Namespace: metav1.NamespaceDefault}
		Expect(k8sClient.Create(ctx, haCluster(key.Name))).To(Succeed())
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		prober := &HTTPRoleProber{
			Client: &http.Client{Transport: masterClaimReports(key.Name, false)}, Tokens: staticToken("tok"),
			Now: func() time.Time { return testNow },
		}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: prober, Clock: func() time.Time { return testNow }}
		reconcileStatus := func() databasev1alpha1.CubridClusterStatus {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			got := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			return got.Status
		}
		_ = reconcileStatus()
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		sts.Status.Replicas, sts.Status.ReadyReplicas = 3, 3
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(reconcileStatus().Conditions, conditionReady)).To(BeTrue())

		prober.Client.Transport = masterClaimReports(key.Name, true)
		for range 2 {
			status := reconcileStatus()
			primary := meta.FindStatusCondition(status.Conditions, conditionPrimaryResolved)
			Expect(primary.Status).To(Equal(metav1.ConditionFalse))
			Expect(primary.Reason).To(Equal("AmbiguousPrimaryObservation"))
			Expect(status.CurrentPrimary).To(BeEmpty())
			Expect(meta.IsStatusConditionTrue(status.Conditions, conditionHAReady)).To(BeFalse())
			Expect(meta.IsStatusConditionTrue(status.Conditions, conditionRoutingReady)).To(BeFalse())
			ready := meta.FindStatusCondition(status.Conditions, conditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(reasonMultiplePrimaries))
			Expect(testutil.ToFloat64(metricspkg.ClusterReady.WithLabelValues(key.Namespace, key.Name))).To(BeZero())
		}

		prober.Client.Transport = masterClaimReports(key.Name, false)
		Expect(meta.IsStatusConditionTrue(reconcileStatus().Conditions, conditionReady)).To(BeTrue())
		Expect(testutil.ToFloat64(metricspkg.ClusterReady.WithLabelValues(key.Namespace, key.Name))).To(Equal(float64(1)))
	})
})
