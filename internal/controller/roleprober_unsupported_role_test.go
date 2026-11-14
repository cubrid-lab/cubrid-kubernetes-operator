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
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

// unsupportedRoles are role values that the Instance Manager contract
// (ADR-0003, internal/instancemanager/role.go) does not define. Only the exact
// lowercase values master, slave, replica and unknown are defined: a value
// that differs in case or carries whitespace is not one of them.
var unsupportedRoles = []instancemanager.Role{
	"primary", "MASTER", "Slave", " master ", "\tslave", "slave\n", "leader", "standby",
	instancemanager.Role(strings.Repeat("x", 4*maxErrorMessage)),
}

// reasonUnrecognizedRolePrefix starts the reason of an answer whose role the
// Operator does not recognize.
const reasonUnrecognizedRolePrefix = "the Instance Manager reported an unrecognized role "

// An answer that names the asked member and carries a role the contract does
// not define is an answer without a role: its role is unknown, it neither
// contradicts itself nor claims an active master, and its reason names the
// value in one bounded line, so that a CRD enum never sees it (#867).
func TestProbeRole_UnrecognizedRoleIsNoRole(t *testing.T) {
	for _, role := range unsupportedRoles {
		t.Run(fmt.Sprintf("%.20q", role), func(t *testing.T) {
			answer := haStatus(role, true, node(c0, string(role)), node(c1, "slave"))
			p := &HTTPRoleProber{Client: &http.Client{Transport: answerWith{answer}}, Tokens: staticToken("tok"),
				Now: func() time.Time { return testNow }}
			got := p.ProbeRole(context.Background(), c0, "ns")
			if !got.Reachable || got.Role != databasev1alpha1.RoleUnknown {
				t.Errorf("an unrecognized role %q gives role %q (reachable %v), want unknown", role, got.Role,
					got.Reachable)
			}
			if got.Conflicting || got.ActiveMasterClaim || got.MasterNotYetActive || got.Unreadable {
				t.Errorf("an unrecognized role %q is taken as more than no role: %+v", role, got)
			}
			if authoritative(got, testNow) {
				t.Errorf("an unrecognized role %q is authoritative: %+v", role, got)
			}
			if !strings.HasPrefix(got.Reason, reasonUnrecognizedRolePrefix) {
				t.Errorf("reason = %q, want it to start with %q", got.Reason, reasonUnrecognizedRolePrefix)
			}
			if strings.ContainsAny(got.Reason, "\n\r\t") {
				t.Errorf("reason %q spans lines", got.Reason)
			}
			if n := utf8.RuneCountInString(got.Reason); n > len(reasonUnrecognizedRolePrefix)+maxErrorMessage {
				t.Errorf("reason is %d runes long, not bounded: %q", n, got.Reason)
			}
		})
	}
}

// A recognized role keeps its meaning: the exact values still count, and an
// unknown or empty role still gives the reason of an answer without a role.
func TestProbeRole_RecognizedRolesKeepTheirMeaning(t *testing.T) {
	for _, role := range []instancemanager.Role{instancemanager.RoleMaster, instancemanager.RoleSlave,
		instancemanager.RoleReplica} {
		o := observationFromStatus(haStatus(role, true, node(c0, string(role))), testNow)
		if o.Role != databasev1alpha1.CubridRole(role) || o.Reason != "" || !authoritative(o, testNow) {
			t.Errorf("the recognized role %q gives %+v", role, o)
		}
	}
	for _, role := range []instancemanager.Role{instancemanager.RoleUnknown, ""} {
		o := observationFromStatus(instancemanager.HAStatus{Current: c0, Role: role}, testNow)
		if o.Role == databasev1alpha1.RoleMaster || o.Reason != reasonNoRoleReported {
			t.Errorf("the role %q gives %+v", role, o)
		}
	}
}

// The resolver refuses an unrecognized role in an observation given to it
// directly, not only one that came through the prober: it is not
// authoritative, cannot resolve a primary beside a recognized master, and is
// published as unknown, the only value of the CRD enum it may stand for.
func TestResolvePrimary_UnrecognizedRoleObservationIsRefused(t *testing.T) {
	for _, role := range unsupportedRoles {
		t.Run(fmt.Sprintf("%.20q", role), func(t *testing.T) {
			odd := RoleObservation{Reachable: true, Role: databasev1alpha1.CubridRole(role), ObservedAt: testNow}
			if authoritative(odd, testNow) {
				t.Errorf("an observation with the role %q is authoritative", role)
			}
			members := []string{c0, c1, c2}
			obs := map[string]RoleObservation{c0: master(), c1: odd, c2: slave()}
			res := resolvePrimary(members, obs, testNow)
			if res.Status == metav1.ConditionTrue || res.CurrentPrimary != "" || res.Reason != incomplete {
				t.Errorf("a master beside the role %q resolved %+v, want %s", role, res, incomplete)
			}
			for _, in := range instanceStatuses(members, obs, testNow, nil, nil) {
				if in.Name == c1 && (in.Role != databasev1alpha1.RoleUnknown || in.Ready) {
					t.Errorf("the member with the role %q is published as %q (ready %v)", role, in.Role, in.Ready)
				}
			}
		})
	}
}

// For the managed write quarantine an unrecognized role is neither a
// verified non-master answer nor an active-master claim, even if an
// observation carries the claim flag; yet it is an answer, so the member is
// not silent for the out-of-service taint rule.
func TestQuarantine_UnrecognizedRoleIsAnAnswerWithoutARole(t *testing.T) {
	for _, role := range unsupportedRoles {
		t.Run(fmt.Sprintf("%.20q", role), func(t *testing.T) {
			probed := observationFromStatus(haStatus(role, true, node(c0, string(role))), testNow)
			direct := RoleObservation{Reachable: true, Role: databasev1alpha1.CubridRole(role), ObservedAt: testNow,
				ActiveMasterClaim: true}
			for name, o := range map[string]RoleObservation{"probed": probed, "direct": direct} {
				if nonMasterAnswer(o, testNow) {
					t.Errorf("%s: the role %q counts as a verified non-master answer", name, role)
				}
				if activeMaster(o, testNow) {
					t.Errorf("%s: the role %q counts as an active-master claim", name, role)
				}
				if silent(o, testNow) {
					t.Errorf("%s: a member that answered the role %q is silent", name, role)
				}
			}
			if txt := observedRoleText(probed, testNow); txt != "no role" {
				t.Errorf("the role %q is recorded as %q, want no role", role, txt)
			}
		})
	}
}

// Through the real prober and reconcile: a cluster whose status holds a
// resolved primary sees one slave answer the unrecognized role "primary".
// The next status is written, with that member unknown and the primary
// unresolved, instead of failing the CRD's role enum and keeping the old
// status (#867). The answers are synthetic; no engine was seen sending such a
// role.
var _ = Describe("An Instance Manager answer with an unrecognized role (#867)", func() {
	ctx := context.Background()

	It("stores an unresolved status with the member's role unknown", func() {
		const name = "unrecognized-role"
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
			m0: {Current: m0, Role: instancemanager.RoleMaster, ServerActive: true, Nodes: nodes},
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

		healthy := pass()
		Expect(healthy.Status.CurrentPrimary).To(Equal(m0))
		Expect(meta.IsStatusConditionTrue(healthy.Status.Conditions, conditionPrimaryResolved)).To(BeTrue())

		answers[m1] = instancemanager.HAStatus{Current: m1, Role: "primary",
			Nodes: []instancemanager.NodeState{node(m0, "master"), node(m1, "primary"), node(m2, "slave")}}
		clock = clock.Add(time.Second)
		got := pass()
		Expect(got.Status.CurrentPrimary).To(BeEmpty())
		resolved := meta.FindStatusCondition(got.Status.Conditions, conditionPrimaryResolved)
		Expect(resolved).NotTo(BeNil())
		Expect(resolved.Status).To(Equal(metav1.ConditionFalse))
		Expect(resolved.Reason).To(Equal("PrimaryObservationIncomplete"))
		roles := map[string]databasev1alpha1.CubridRole{}
		for _, in := range got.Status.Instances {
			roles[in.Name] = in.Role
		}
		Expect(roles).To(Equal(map[string]databasev1alpha1.CubridRole{
			m0: databasev1alpha1.RoleMaster, m1: databasev1alpha1.RoleUnknown, m2: databasev1alpha1.RoleSlave}))
	})
})
