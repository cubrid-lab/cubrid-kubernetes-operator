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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

func master() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster}
}
func slave() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave}
}
func unknown() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown}
}
func unreach() RoleObservation { return RoleObservation{Reachable: false} }

const (
	c0         = "c-0"
	c1         = "c-1"
	c2         = "c-2"
	incomplete = "PrimaryObservationIncomplete"
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
			metav1.ConditionTrue, "SinglePrimaryObserved", c0},
		{"two masters -> not resolved", map[string]RoleObservation{c0: master(), c1: master(), c2: slave()},
			metav1.ConditionFalse, "MultiplePrimariesObserved", ""},
		{"no master", map[string]RoleObservation{c0: slave(), c1: slave(), c2: slave()},
			metav1.ConditionFalse, "NoPrimaryObserved", ""},
		{"one unreachable -> incomplete (safety)", map[string]RoleObservation{c0: master(), c1: slave(), c2: unreach()},
			metav1.ConditionFalse, incomplete, ""},
		{"one unknown -> incomplete (safety)", map[string]RoleObservation{c0: master(), c1: unknown(), c2: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"missing member -> incomplete", map[string]RoleObservation{c0: master(), c1: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"multiple masters wins over incomplete", map[string]RoleObservation{c0: master(), c1: master(), c2: unreach()},
			metav1.ConditionFalse, "MultiplePrimariesObserved", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolvePrimary(members, c.obs)
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
	got := instanceStatuses(members, obs)
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
