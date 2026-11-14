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
	"slices"
	"strings"
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
	// Responded is set when the Instance Manager returned an HTTP response
	// other than 200, such as a refused token or a server error: the member
	// is not reachable for its role, but its Pod's process answered.
	Responded bool
	Role      databasev1alpha1.CubridRole
	// ObservedAt is when the answer was received. An observation without it, or
	// older than roleObservationTTL, is not authoritative (ADR-0005).
	ObservedAt time.Time
	// Conflicting is set when the Instance Manager's answer contradicts itself
	// (role vs HA status, ADR-0005 ConflictingLocalHAStatus), names a member
	// other than the one that was asked, or gives a master or slave role
	// without naming any member; the role is then not trusted.
	Conflicting bool
	// ActiveMasterClaim preserves a locally consistent active-master report
	// even when another master in the node list makes the role untrusted.
	// It is diagnostic evidence for Ready, never authority to name a primary.
	ActiveMasterClaim bool
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
	// LogEnd is the end of a master's own log as its Instance Manager read
	// it; nil for another role and when it was not read.
	LogEnd *databasev1alpha1.LogAddress
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
	// Unfinished is the member's database as the Instance Manager reports it
	// when the database carries the marker of an unfinished createdb or
	// restoredb; nil when it reports none. It is independent of Role.
	Unfinished *databasev1alpha1.InstanceUnfinishedDatabase
	// Capabilities are the request checks the Instance Manager reports that
	// it enforces (instancemanager.Capabilities); nil when it reports none.
	Capabilities []string
	// Unreadable is set when the member answered but the answer could not be
	// decoded: it shows neither a role nor the checks the manager enforces.
	Unreadable bool
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

// roleProbeTimeout bounds one GET /v1/role.
const roleProbeTimeout = 5 * time.Second

func NewHTTPRoleProber(tokens TokenSource) *HTTPRoleProber {
	return &HTTPRoleProber{
		Client: &http.Client{Timeout: roleProbeTimeout},
		Tokens: tokens,
	}
}

// reasonAnswerNamesNoMember is the reason of a master or slave answer that
// does not name the member it came from.
const reasonAnswerNamesNoMember = "the answer names no member"

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
			Reason: fmt.Sprintf("status %d", resp.StatusCode), RequestID: id, Responded: true}
	}
	var st instancemanager.HAStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: p.now(),
			Reason: "unreadable answer", RequestID: id, Unreadable: true}
	}
	o := observationFromStatus(st, p.now())
	o.RequestID = id
	if st.Current != podName {
		o.ActiveMasterClaim = false
	}
	// The answer is recorded under the name that was asked. A master or slave
	// role counts only when the answer names that member as current. An
	// answer that names another member came from somewhere else, a wrong DNS
	// record or Service for example. An answer that names no member cannot
	// be told apart from one: an Instance Manager of another build, or an
	// answer changed on the way, may leave the name out. Neither is this
	// member's role. An answer without a role names nobody and is kept as an
	// answer without a role.
	haRole := st.Role == instancemanager.RoleMaster || st.Role == instancemanager.RoleSlave
	switch {
	case st.Current != "" && st.Current != podName:
		o.Conflicting = true
		o.MasterNotYetActive = false
		o.Reason = "the answer names the member " + cleanMessage(st.Current)
	case st.Current == "" && haRole:
		o.Conflicting = true
		o.MasterNotYetActive = false
		o.Reason = reasonAnswerNamesNoMember
	}
	return o
}

// Reasons of a request that is not sent because the member's Instance
// Manager is not shown to enforce a check the request relies on (ADR-0013):
// its fresh answer to GET /v1/role does not report the check, or the member
// answered, but not freshly or not readably, so the answer shows nothing. A
// member that could not be asked keeps the reason of an unavailable manager,
// or of a refused token.
const (
	reasonCapabilityMissing    = "InstanceManagerCapabilityMissing"
	reasonCapabilityUnverified = "InstanceManagerCapabilityUnverified"
)

// capabilityHold says whether the step, a request to member that relies on
// the checks needed, waits, from o, the member's answer to GET /v1/role read
// in this reconcile. It waits unless o is a fresh, readable answer that
// reports every check. A 2xx answer to the request itself would not show that
// the check ran: an Instance Manager of another build may ignore the field
// that carries it. The step uses up no attempt while it waits, and nothing is
// deleted or replaced.
func capabilityHold(member, step string, o RoleObservation, now time.Time, needed ...string) (reason, msg string, held bool) {
	why := ""
	if o.Reason != "" {
		why = " (" + o.Reason + ")"
	}
	switch {
	case !o.Reachable:
		reason = reasonInstanceManagerUnavailable
		if o.TokenRefused {
			reason = reasonTokenRefused
		}
		return reason, fmt.Sprintf(
			"the %s is not sent to %s: its Instance Manager could not be asked GET /v1/role%s, which shows whether it "+
				"enforces the checks %s the request relies on. The step waits and uses up no attempt",
			step, member, why, strings.Join(needed, ", ")), true
	case !fresh(o, now) || o.Unreadable:
		return reasonCapabilityUnverified, fmt.Sprintf(
			"the %s is not sent to %s: it relies on the Instance Manager checks %s, and %s gave no fresh, readable "+
				"answer to GET /v1/role%s that shows them. The step waits and uses up no attempt",
			step, member, strings.Join(needed, ", "), member, why), true
	}
	var missing []string
	for _, c := range needed {
		if !slices.Contains(o.Capabilities, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return "", "", false
	}
	return reasonCapabilityMissing, fmt.Sprintf(
		"the %s is not sent to %s: its Instance Manager does not report the checks %s, which the request relies "+
			"on. An Instance Manager of another build may accept the request and skip them. Replace the Pod of %s "+
			"so that it runs the Instance Manager of the Operator's build; the step waits, uses up no attempt, and "+
			"nothing is deleted",
		step, member, strings.Join(missing, ", "), member), true
}

// TokenChecker asks one member's Instance Manager whether it accepts a token.
type TokenChecker interface {
	AcceptsToken(ctx context.Context, podName, namespace, token string) (bool, error)
}

// AcceptsToken sends GET /v1/role with the token alone, never the previous
// one. Only a 200 shows that the member holds the token. A 401 or 403
// (tokenRefused) is a refusal. Any other answer, such as a server or proxy
// error or another success code, shows neither, and is an error naming the
// Pod and the status, as is a failed request; the caller then keeps the
// previous token in use (#840). The token is never put into the error.
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
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case tokenRefused(resp.StatusCode):
		return false, nil
	default:
		return false, fmt.Errorf("ask %s about its token: unexpected status %d", podName, resp.StatusCode)
	}
}

func (p *HTTPRoleProber) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// logAddress converts a log address the Instance Manager reported; nil stays nil.
func logAddress(a *instancemanager.LSA) *databasev1alpha1.LogAddress {
	if a == nil {
		return nil
	}
	return &databasev1alpha1.LogAddress{Page: a.Page, Offset: a.Offset}
}

// observationFromStatus turns an Instance Manager answer into an observation,
// flagging an answer that contradicts itself (ADR-0005): a master whose server
// is not registered_and_active, a role that differs from the node's own line
// in the HA node list, or a master that sees another master.
func observationFromStatus(st instancemanager.HAStatus, at time.Time) RoleObservation {
	o := RoleObservation{
		Reachable: true, Role: databasev1alpha1.CubridRole(st.Role), ObservedAt: at,
		EngineVersion: st.EngineVersion, Unfinished: unfinishedFromStatus(st.Unfinished),
		Capabilities: st.Capabilities,
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
			FinalLSA:       logAddress(r.FinalLSA),
			CopiedEOF:      logAddress(r.CopiedEOF),
		}
	}
	if st.Role == instancemanager.RoleMaster {
		o.LogEnd = logAddress(st.LogEnd)
	}
	notYetActive := st.Role == instancemanager.RoleMaster && !st.ServerActive
	contradictsNodeList := false
	localEntries := 0
	localMaster := true
	for _, n := range st.Nodes {
		if n.Name == st.Current {
			localEntries++
			localMaster = localMaster && n.State == string(instancemanager.RoleMaster)
			if n.State != string(st.Role) {
				contradictsNodeList = true
			}
		}
		if n.Name != st.Current && st.Role == instancemanager.RoleMaster && n.State == string(instancemanager.RoleMaster) {
			contradictsNodeList = true
		}
	}
	o.ActiveMasterClaim = st.Role == instancemanager.RoleMaster && st.ServerActive &&
		st.Current != "" && localEntries == 1 && localMaster
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

// unfinishedFromStatus turns the Instance Manager's report of an unfinished
// database into the status form. A state other than WaitingForOperation is
// taken as Refused: nothing is promised to take that database up.
func unfinishedFromStatus(u *instancemanager.UnfinishedDatabase) *databasev1alpha1.InstanceUnfinishedDatabase {
	if u == nil {
		return nil
	}
	state := databasev1alpha1.UnfinishedRefused
	if u.State == instancemanager.UnfinishedWaiting {
		state = databasev1alpha1.UnfinishedWaitingForOperation
	}
	return &databasev1alpha1.InstanceUnfinishedDatabase{
		Database: cleanMessage(u.Database), Operation: cleanMessage(u.Operation),
		State: state, Message: cleanMessage(u.Reason),
	}
}
