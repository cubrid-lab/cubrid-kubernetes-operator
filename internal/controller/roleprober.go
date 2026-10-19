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

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// RoleObservation is what the operator records for one instance after polling
// its Instance Manager. Reachable=false means "no evidence", never "down"
// (ADR-0005): an unreachable manager must not be treated as a role assertion.
type RoleObservation struct {
	Reachable bool
	// TokenRefused is set when the Instance Manager answered 401 or 403 to
	// the cluster's token. The member is then not reachable for the operator
	// and its role stays unknown, as for an unreachable one.
	TokenRefused bool
	Role         databasev1alpha1.CubridRole
	// ObservedAt is when the answer was received. An observation without it, or
	// older than roleObservationTTL, is not authoritative (ADR-0005).
	ObservedAt time.Time
	// Conflicting is set when the Instance Manager's answer contradicts itself
	// (role vs HA status, ADR-0005 ConflictingLocalHAStatus) or names a member
	// other than the one that was asked; the role is then not trusted.
	Conflicting bool
	// MasterNotYetActive is set when the answer contradicts itself in one way
	// only: the member reports itself master while its server is not
	// registered_and_active. That is the state of the master of a group
	// whose members have not all joined once (docs/poc/RESULTS.md, POC-13 and
	// POC-23). Conflicting is set as well, so the role stays untrusted for
	// everything but the HA bootstrap's choice of a seeding source.
	MasterNotYetActive bool
	// EngineVersion is the engine's full version as the Instance Manager
	// reports it ("11.4.6.1963"); empty when it did not report one.
	EngineVersion string
	// Replication is what a slave's Instance Manager read from its log
	// applier; nil for another role and when the applier could not be read.
	Replication *ReplicationObservation
	// Reason says why the answer gives no role that counts: the request that
	// failed, the status the member answered, what its Instance Manager said
	// of a role it does not report, or how the answer contradicts itself.
	// It is empty for an answer with a role. The Operator never puts a token
	// into it. Text from the member or the network is made one line and cut
	// to maxErrorMessage runes (cleanMessage), not scrubbed: what a member
	// sends is kept up to the cut as it is.
	Reason string
	// RequestID is the ID the role query was sent with; empty when none was
	// sent.
	RequestID string
}

// RoleProber polls one instance's Instance Manager /v1/role endpoint.
type RoleProber interface {
	ProbeRole(ctx context.Context, podName, namespace string) RoleObservation
}

// HTTPRoleProber talks to the per-member Instance Manager over the pod's
// stable DNS (ADR-0004 per-member alias Service), port 9090 (ADR-0003).
type HTTPRoleProber struct {
	Client *http.Client
	Tokens TokenSource
	// Now stamps each observation; nil means time.Now.
	Now func() time.Time

	accepted acceptedTokens
}

func NewHTTPRoleProber(tokens TokenSource) *HTTPRoleProber {
	return &HTTPRoleProber{
		Client: &http.Client{Timeout: 5 * time.Second},
		Tokens: tokens,
	}
}

func (p *HTTPRoleProber) ProbeRole(ctx context.Context, podName, namespace string) RoleObservation {
	// Per-member alias Service resolves the short pod name to its pod IP within
	// the namespace (ADR-0004).
	url := fmt.Sprintf("http://%s.%s.svc:%d/v1/role", podName, namespace, instancemanager.DefaultPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RoleObservation{Reachable: false, ObservedAt: p.now(), Reason: cleanMessage(err.Error())}
	}
	tokens, err := memberTokens(ctx, p.Tokens, podName, namespace)
	if err != nil {
		// The error names the Pod and the Secret, never a token value.
		logf.FromContext(ctx).V(1).Info("Could not resolve Instance Manager token; member not probed",
			"pod", podName, "namespace", namespace, "reason", err.Error())
		return RoleObservation{Reachable: false, ObservedAt: p.now(), Reason: cleanMessage(err.Error())}
	}
	resp, err := p.accepted.send(p.Client, req, namespace+"/"+podName, tokens)
	// send gave the query its ID, which the member's log carries if the
	// query reached it.
	id := req.Header.Get(requestIDHeader)
	if err != nil {
		// The error names the URL, which carries no token.
		return RoleObservation{Reachable: false, ObservedAt: p.now(), Reason: cleanMessage(err.Error()), RequestID: id}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return RoleObservation{Reachable: false, TokenRefused: tokenRefused(resp.StatusCode), ObservedAt: p.now(),
			Reason: fmt.Sprintf("status %d", resp.StatusCode), RequestID: id}
	}
	var st instancemanager.HAStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: p.now(),
			Reason: "unreadable answer", RequestID: id}
	}
	o := observationFromStatus(st, p.now())
	o.RequestID = id
	// The answer is recorded under the name that was asked. An answer that
	// carries a role but names another member came from somewhere else, a
	// wrong DNS record or Service for example, and is not this member's role.
	if st.Current != "" && st.Current != podName {
		o.Conflicting = true
		o.MasterNotYetActive = false
		o.Reason = "the answer names the member " + cleanMessage(st.Current)
	}
	return o
}

// TokenChecker asks one member's Instance Manager whether it accepts a token.
type TokenChecker interface {
	AcceptsToken(ctx context.Context, podName, namespace, token string) (bool, error)
}

// AcceptsToken sends GET /v1/role with the token alone. Only the manager's
// authentication answers 401, so any other answer means the member holds the
// token. A failed request is an error, not a refusal. A 403, which
// tokenRefused reports as a refusal, is not one here.
func (p *HTTPRoleProber) AcceptsToken(ctx context.Context, podName, namespace, token string) (bool, error) {
	url := fmt.Sprintf("http://%s.%s.svc:%d/v1/role", podName, namespace, instancemanager.DefaultPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	setRequestID(req)
	resp, err := p.Client.Do(req)
	if err != nil {
		return false, fmt.Errorf("ask %s about its token: %w", podName, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode != http.StatusUnauthorized, nil
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
		if st.Role == instancemanager.RoleUnknown || st.Role == "" {
			o.Reason = "the Instance Manager reported no role"
			if why := cleanMessage(st.Reason); why != "" {
				o.Reason += ": " + why
			}
		}
		return o
	}
	if r := st.Replication; st.Role == instancemanager.RoleSlave && r != nil && r.Available {
		o.Replication = &ReplicationObservation{
			Source:         r.Source,
			AppliedChanges: int64(r.AppliedChanges),
			FailCount:      int64(r.FailCount),
			DelayedPages:   int64(r.DelayedPageCount),
			Applier:        r.Applier,
		}
	}
	notYetActive := st.Role == instancemanager.RoleMaster && !st.ServerActive
	contradictsNodeList := false
	for _, n := range st.Nodes {
		if n.Name == st.Current && n.State != string(st.Role) {
			contradictsNodeList = true
		}
		if n.Name != st.Current && st.Role == instancemanager.RoleMaster && n.State == string(instancemanager.RoleMaster) {
			contradictsNodeList = true
		}
	}
	o.Conflicting = notYetActive || contradictsNodeList
	o.MasterNotYetActive = notYetActive && !contradictsNodeList
	switch {
	case contradictsNodeList:
		o.Reason = "the role " + string(st.Role) + " contradicts the HA node list"
	case notYetActive:
		o.Reason = "a master whose server is not registered_and_active"
	}
	return o
}
