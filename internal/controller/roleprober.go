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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// RoleObservation is what the operator records for one instance after polling
// its Instance Manager. Reachable=false means "no evidence", never "down"
// (ADR-0005): an unreachable manager must not be treated as a role assertion.
type RoleObservation struct {
	Reachable bool
	Role      databasev1alpha1.CubridRole
	// ObservedAt is when the answer was received. An observation without it, or
	// older than roleObservationTTL, is not authoritative (ADR-0005).
	ObservedAt time.Time
	// Conflicting is set when the Instance Manager's answer contradicts itself
	// (role vs HA status, ADR-0005 ConflictingLocalHAStatus) or names a member
	// other than the one that was asked; the role is then not trusted.
	Conflicting bool
	// EngineVersion is the engine's full version as the Instance Manager
	// reports it ("11.4.6.1963"); empty when it did not report one.
	EngineVersion string
}

// RoleProber polls one instance's Instance Manager /v1/role endpoint.
type RoleProber interface {
	ProbeRole(ctx context.Context, podName, namespace string) RoleObservation
}

// HTTPRoleProber talks to the per-member Instance Manager over the pod's
// stable DNS (ADR-0004 per-member alias Service), port 9090 (ADR-0003).
type HTTPRoleProber struct {
	Client *http.Client
	Token  string
	// Now stamps each observation; nil means time.Now.
	Now func() time.Time
}

func NewHTTPRoleProber(token string) *HTTPRoleProber {
	return &HTTPRoleProber{
		Client: &http.Client{Timeout: 5 * time.Second},
		Token:  token,
	}
}

func (p *HTTPRoleProber) ProbeRole(ctx context.Context, podName, namespace string) RoleObservation {
	// Per-member alias Service resolves the short pod name to its pod IP within
	// the namespace (ADR-0004).
	url := fmt.Sprintf("http://%s.%s.svc:%d/v1/role", podName, namespace, instancemanager.DefaultPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RoleObservation{Reachable: false, ObservedAt: p.now()}
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return RoleObservation{Reachable: false, ObservedAt: p.now()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return RoleObservation{Reachable: false, ObservedAt: p.now()}
	}
	var st instancemanager.HAStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: p.now()}
	}
	o := observationFromStatus(st, p.now())
	// The answer is recorded under the name that was asked. An answer that
	// carries a role but names another member came from somewhere else, a
	// wrong DNS record or Service for example, and is not this member's role.
	if st.Current != "" && st.Current != podName {
		o.Conflicting = true
	}
	return o
}

func (p *HTTPRoleProber) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// observationFromStatus turns an Instance Manager answer into an observation,
// flagging an answer that contradicts itself (ADR-0005): a master whose server
// is not registered_and_active, a role that differs from the node's own line
// in the HA node list, or a master that sees another master.
func observationFromStatus(st instancemanager.HAStatus, at time.Time) RoleObservation {
	o := RoleObservation{
		Reachable: true, Role: databasev1alpha1.CubridRole(st.Role), ObservedAt: at,
		EngineVersion: st.EngineVersion,
	}
	if st.Role != instancemanager.RoleMaster && st.Role != instancemanager.RoleSlave {
		return o
	}
	if st.Role == instancemanager.RoleMaster && !st.ServerActive {
		o.Conflicting = true
	}
	for _, n := range st.Nodes {
		if n.Name == st.Current && n.State != string(st.Role) {
			o.Conflicting = true
		}
		if n.Name != st.Current && st.Role == instancemanager.RoleMaster && n.State == string(instancemanager.RoleMaster) {
			o.Conflicting = true
		}
	}
	return o
}
