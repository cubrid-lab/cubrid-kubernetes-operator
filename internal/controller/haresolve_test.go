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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// testNow is the clock every resolution test reads; observations are stamped
// relative to it so freshness is decided by the test, not the wall clock.
var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func master() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow}
}
func slave() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow}
}
func unknown() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: testNow}
}
func unreach() RoleObservation { return RoleObservation{Reachable: false, ObservedAt: testNow} }

// aged returns the observation as if it had been taken `age` before testNow.
func aged(o RoleObservation, age time.Duration) RoleObservation {
	o.ObservedAt = testNow.Add(-age)
	return o
}

// conflicting returns the observation flagged as contradicting its own HA status.
func conflicting(o RoleObservation) RoleObservation {
	o.Conflicting = true
	return o
}

const (
	c0                = "c-0"
	c1                = "c-1"
	c2                = "c-2"
	incomplete        = "PrimaryObservationIncomplete"
	multiplePrimaries = "MultiplePrimariesObserved"
	singlePrimary     = "SinglePrimaryObserved"
	ambiguous         = "AmbiguousPrimaryObservation"
	noPrimary         = "NoPrimaryObserved"
	cubridVersion     = "11.4"
)

func TestResolvePrimary(t *testing.T) {
	members := []string{c0, c1, c2}
	cases := []struct {
		name   string
		obs    map[string]RoleObservation
		status metav1.ConditionStatus
		reason string
		prim   string
	}{
		{"healthy single primary", map[string]RoleObservation{c0: master(), c1: slave(), c2: slave()},
			metav1.ConditionTrue, singlePrimary, c0},
		{"two masters -> not resolved", map[string]RoleObservation{c0: master(), c1: master(), c2: slave()},
			metav1.ConditionFalse, multiplePrimaries, ""},
		{"no master", map[string]RoleObservation{c0: slave(), c1: slave(), c2: slave()},
			metav1.ConditionFalse, noPrimary, ""},
		{"one unreachable -> incomplete (safety)", map[string]RoleObservation{c0: master(), c1: slave(), c2: unreach()},
			metav1.ConditionFalse, incomplete, ""},
		{"one unknown -> incomplete (safety)", map[string]RoleObservation{c0: master(), c1: unknown(), c2: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"missing member -> incomplete", map[string]RoleObservation{c0: master(), c1: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"multiple masters wins over incomplete", map[string]RoleObservation{c0: master(), c1: master(), c2: unreach()},
			metav1.ConditionFalse, multiplePrimaries, ""},
		{"stale master -> incomplete, not resolved",
			map[string]RoleObservation{c0: aged(master(), roleObservationTTL+time.Second), c1: slave(), c2: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"stale slave -> incomplete", map[string]RoleObservation{c0: master(), c1: slave(), c2: aged(slave(), time.Hour)},
			metav1.ConditionFalse, incomplete, ""},
		{"observation exactly at the TTL is still fresh",
			map[string]RoleObservation{c0: aged(master(), roleObservationTTL), c1: slave(), c2: slave()},
			metav1.ConditionTrue, singlePrimary, c0},
		{"observation without a time -> incomplete",
			map[string]RoleObservation{c0: master(), c1: {Reachable: true, Role: databasev1alpha1.RoleSlave}, c2: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"observation from the future (clock skew) -> incomplete",
			map[string]RoleObservation{c0: aged(master(), -roleObservationTTL-time.Second), c1: slave(), c2: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"master contradicting its own HA status -> ambiguous",
			map[string]RoleObservation{c0: conflicting(master()), c1: slave(), c2: slave()},
			metav1.ConditionFalse, ambiguous, ""},
		{"slave contradicting its own HA status -> ambiguous",
			map[string]RoleObservation{c0: master(), c1: conflicting(slave()), c2: slave()},
			metav1.ConditionFalse, ambiguous, ""},
		{"multiple masters wins over ambiguous",
			map[string]RoleObservation{c0: master(), c1: master(), c2: conflicting(slave())},
			metav1.ConditionFalse, multiplePrimaries, ""},
		{"ambiguous wins over incomplete",
			map[string]RoleObservation{c0: conflicting(master()), c1: unreach(), c2: slave()},
			metav1.ConditionFalse, ambiguous, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolvePrimary(members, c.obs, testNow)
			if got.Status != c.status || got.Reason != c.reason || got.CurrentPrimary != c.prim {
				t.Errorf("got {%s %s %q}, want {%s %s %q}",
					got.Status, got.Reason, got.CurrentPrimary, c.status, c.reason, c.prim)
			}
		})
	}
}

func TestInstanceStatuses(t *testing.T) {
	members := []string{c0, c1, c2}
	obs := map[string]RoleObservation{c0: master(), c1: slave(), c2: unreach()}
	got := instanceStatuses(members, obs, testNow)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Role != databasev1alpha1.RoleMaster || !got[0].Ready || got[0].Ordinal != 0 {
		t.Errorf("c-0 = %+v", got[0])
	}
	if got[1].Role != databasev1alpha1.RoleSlave || !got[1].Ready {
		t.Errorf("c-1 = %+v", got[1])
	}
	// unreachable -> unknown, not ready
	if got[2].Role != databasev1alpha1.RoleUnknown || got[2].Ready {
		t.Errorf("c-2 = %+v (unreachable must be unknown/not-ready)", got[2])
	}
}

// A stale or conflicting observation is reported as unknown and not ready, the
// same way the cluster verdict treats it (ADR-0005).
func TestInstanceStatusesTreatStaleAndConflictingAsUnknown(t *testing.T) {
	members := []string{c0, c1}
	obs := map[string]RoleObservation{c0: aged(master(), time.Hour), c1: conflicting(slave())}
	for _, got := range instanceStatuses(members, obs, testNow) {
		if got.Role != databasev1alpha1.RoleUnknown || got.Ready {
			t.Errorf("%s = %+v, want unknown and not ready", got.Name, got)
		}
	}
}

// Write routing enters quarantine on any unsafe snapshot and leaves it only when
// the current snapshot alone proves a single primary: no earlier verdict, stored
// status or old observation keeps routing open (ADR-0005 safety rule).
func TestWriteRoutingEntersAndLeavesQuarantine(t *testing.T) {
	members := []string{c0, c1, c2}
	r := &CubridClusterReconciler{}
	cluster := &databasev1alpha1.CubridCluster{}
	steps := []struct {
		name   string
		obs    map[string]RoleObservation
		ready  metav1.ConditionStatus
		reason string
	}{
		{"healthy", map[string]RoleObservation{c0: master(), c1: slave(), c2: slave()},
			metav1.ConditionTrue, "PrimaryResolved"},
		{"two masters enter quarantine", map[string]RoleObservation{c0: master(), c1: master(), c2: slave()},
			metav1.ConditionFalse, multiplePrimaries},
		{"a conflicting member keeps it", map[string]RoleObservation{c0: master(), c1: conflicting(slave()), c2: slave()},
			metav1.ConditionFalse, ambiguous},
		{"a stale member keeps it", map[string]RoleObservation{c0: master(), c1: slave(), c2: aged(slave(), time.Hour)},
			metav1.ConditionFalse, incomplete},
		{"an unreachable member keeps it", map[string]RoleObservation{c0: master(), c1: slave(), c2: unreach()},
			metav1.ConditionFalse, incomplete},
		{"no master is not ready either", map[string]RoleObservation{c0: slave(), c1: slave(), c2: slave()},
			metav1.ConditionFalse, noPrimary},
		{"a fresh, complete single primary leaves it", map[string]RoleObservation{c0: slave(), c1: master(), c2: slave()},
			metav1.ConditionTrue, "PrimaryResolved"},
	}
	for _, step := range steps {
		res := resolvePrimary(members, step.obs, testNow)
		r.setBrokerConditions(cluster, res)
		got := findCondition(cluster, conditionRoutingReady)
		if got == nil || got.Status != step.ready || got.Reason != step.reason {
			t.Fatalf("%s: RoutingReady = %+v, want %s/%s", step.name, got, step.ready, step.reason)
		}
	}
}

func findCondition(cluster *databasev1alpha1.CubridCluster, condType string) *metav1.Condition {
	for i := range cluster.Status.Conditions {
		if cluster.Status.Conditions[i].Type == condType {
			return &cluster.Status.Conditions[i]
		}
	}
	return nil
}
