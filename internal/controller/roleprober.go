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
		return RoleObservation{Reachable: false}
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return RoleObservation{Reachable: false}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return RoleObservation{Reachable: false}
	}
	var st instancemanager.HAStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown}
	}
	return RoleObservation{Reachable: true, Role: databasev1alpha1.CubridRole(st.Role)}
}
