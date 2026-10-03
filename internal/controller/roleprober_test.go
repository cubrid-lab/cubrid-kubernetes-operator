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

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

func haStatus(role instancemanager.Role, serverActive bool, nodes ...instancemanager.NodeState) instancemanager.HAStatus {
	return instancemanager.HAStatus{Current: c0, Role: role, ServerActive: serverActive, Nodes: nodes}
}

func node(name, state string) instancemanager.NodeState {
	return instancemanager.NodeState{Name: name, State: state}
}

// An Instance Manager answer that contradicts itself is flagged, never trusted
// as a role (ADR-0005: role vs ha/status conflict -> ConflictingLocalHAStatus).
func TestObservationFromStatus(t *testing.T) {
	cases := []struct {
		name        string
		status      instancemanager.HAStatus
		role        databasev1alpha1.CubridRole
		conflicting bool
	}{
		{"consistent master", haStatus(instancemanager.RoleMaster, true, node(c0, "master"), node(c1, "slave")),
			databasev1alpha1.RoleMaster, false},
		{"consistent slave", haStatus(instancemanager.RoleSlave, false, node(c0, "slave"), node(c1, "master")),
			databasev1alpha1.RoleSlave, false},
		{"master whose server is not active", haStatus(instancemanager.RoleMaster, false, node(c0, "master")),
			databasev1alpha1.RoleMaster, true},
		{"role differs from its own node line", haStatus(instancemanager.RoleMaster, true, node(c0, "slave")),
			databasev1alpha1.RoleMaster, true},
		{"master that also sees another master", haStatus(instancemanager.RoleMaster, true,
			node(c0, "master"), node(c1, "master")), databasev1alpha1.RoleMaster, true},
		{"unknown stays unknown, not conflicting", haStatus(instancemanager.RoleUnknown, false),
			databasev1alpha1.RoleUnknown, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := observationFromStatus(c.status, testNow)
			if !got.Reachable || got.Role != c.role || got.Conflicting != c.conflicting || !got.ObservedAt.Equal(testNow) {
				t.Errorf("got %+v, want role %s conflicting %v at %s", got, c.role, c.conflicting, testNow)
			}
		})
	}
}
