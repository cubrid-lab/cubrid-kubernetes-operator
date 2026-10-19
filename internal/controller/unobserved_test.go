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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// Each way a role query gives no authoritative role carries why, so that a
// member that turns unknown can be told apart: no answer, a refused token,
// an Instance Manager that reported no role, an answer that contradicts
// itself (#438).
func TestProbeRole_SaysWhyThereIsNoRole(t *testing.T) {
	probe := func(rt http.RoundTripper) RoleObservation {
		p := &HTTPRoleProber{Client: &http.Client{Transport: rt}, Tokens: staticToken(refusedTok),
			Now: func() time.Time { return testNow }}
		return p.ProbeRole(context.Background(), c0, "ns")
	}
	cases := []struct {
		name string
		rt   http.RoundTripper
		want string
	}{
		{"no answer", unreachable{}, "connection refused"},
		{"a refused token", answerStatus(http.StatusUnauthorized), "status 401"},
		{"a server error", answerStatus(http.StatusInternalServerError), "status 500"},
		{"no role reported", answerWith{instancemanager.HAStatus{Role: instancemanager.RoleUnknown,
			Reason: "heartbeat status failed: context deadline exceeded"}},
			"heartbeat status failed: context deadline exceeded"},
		{"another member answers", answerWith{instancemanager.HAStatus{Current: c1,
			Role: instancemanager.RoleSlave, Nodes: []instancemanager.NodeState{node(c1, "slave")}}}, c1},
		{"a master whose server is not active", answerWith{haStatus(instancemanager.RoleMaster, false,
			node(c0, "master"))}, "registered_and_active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := probe(c.rt)
			if !strings.Contains(o.Reason, c.want) {
				t.Errorf("reason = %q, want it to name %q", o.Reason, c.want)
			}
			if o.RequestID == "" {
				t.Errorf("the observation of a query that was sent has no request ID")
			}
		})
	}
	if o := probe(answerWith{haStatus(instancemanager.RoleSlave, false, node(c0, "slave"))}); o.Reason != "" {
		t.Errorf("an authoritative answer has the reason %q", o.Reason)
	}
	if o := probe(answerWith{instancemanager.HAStatus{Role: instancemanager.RoleUnknown}}); o.Reason !=
		"the Instance Manager reported no role" {
		t.Errorf("an unknown role without a reason gives the reason %q", o.Reason)
	}
}

// failWith is an HTTP transport whose every request fails with the message.
type failWith string

func (f failWith) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New(string(f)) }

// longMessage is a message from a member that spans lines and is longer than
// maxErrorMessage runes; its end must not reach a log.
func longMessage() string {
	return "first line\nsecond\tline\r\n" + strings.Repeat("é", 2*maxErrorMessage) + " END-OF-MESSAGE"
}

// clean reports why a reason is not fit for a log line: a line break or tab,
// more than maxErrorMessage runes taken from the member, or the end of a
// message that should have been cut.
func clean(reason string, prefix int) string {
	switch {
	case strings.ContainsAny(reason, "\n\r\t"):
		return "it spans lines"
	case utf8.RuneCountInString(reason) > prefix+maxErrorMessage+len("..."):
		return fmt.Sprintf("it is %d runes long", utf8.RuneCountInString(reason))
	case strings.Contains(reason, "END-OF-MESSAGE") || !strings.HasSuffix(reason, "..."):
		return "it was not cut"
	}
	return ""
}

// What a member or the network says is made one line and cut to
// maxErrorMessage runes before it becomes a reason. It is cut, not scrubbed:
// the Operator never puts the token into a reason itself, and text that a
// member sends is kept up to the cut as it is (#438).
func TestProbeRole_ReasonIsOneLineAndCut(t *testing.T) {
	probe := func(rt http.RoundTripper) RoleObservation {
		p := &HTTPRoleProber{Client: &http.Client{Transport: rt}, Tokens: staticToken(refusedTok),
			Now: func() time.Time { return testNow }}
		return p.ProbeRole(context.Background(), c0, "ns")
	}
	imPrefix := len("the Instance Manager reported no role: ")
	transportPrefix := len(`Get "http://c-0.ns.svc:9090/v1/role": `)
	for name, c := range map[string]struct {
		rt     http.RoundTripper
		prefix int
	}{
		"the Instance Manager's reason": {answerWith{instancemanager.HAStatus{Role: instancemanager.RoleUnknown,
			Reason: longMessage()}}, imPrefix},
		"a transport error": {failWith(longMessage()), transportPrefix},
	} {
		if why := clean(probe(c.rt).Reason, c.prefix); why != "" {
			t.Errorf("%s: the reason is not fit for a log line: %s", name, why)
		}
	}
}

// switchAnswer answers every request with the status it holds at the time.
type switchAnswer struct{ status instancemanager.HAStatus }

func (s *switchAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	return answerWith{s.status}.RoundTrip(req)
}

// The line logged for a member that turns unknown carries the cleaned and cut
// reason, never a member's message across lines or at full length (#438).
func TestReconcileHAStatus_LoggedReasonIsOneLineAndCut(t *testing.T) {
	answer := &switchAnswer{status: instancemanager.HAStatus{Role: instancemanager.RoleSlave}}
	r := &CubridClusterReconciler{Clock: func() time.Time { return testNow },
		Prober: &HTTPRoleProber{Client: &http.Client{Transport: answer}, Tokens: staticToken(refusedTok),
			Now: func() time.Time { return testNow }}}
	var reasons []string
	ctx := logf.IntoContext(context.Background(), logr.New(&reasonSink{reasons: &reasons}))
	cluster := haCluster("cut")
	r.reconcileHAStatus(ctx, cluster, 3)
	answer.status = instancemanager.HAStatus{Role: instancemanager.RoleUnknown, Reason: longMessage()}
	r.reconcileHAStatus(ctx, cluster, 3)
	if len(reasons) != 3 {
		t.Fatalf("logged reasons = %q, want one for each of the 3 members", reasons)
	}
	for _, reason := range reasons {
		if why := clean(reason, len("the Instance Manager reported no role: ")); why != "" {
			t.Errorf("logged reason %q is not fit for a log line: %s", reason, why)
		}
	}
}

// reasonSink keeps the reason of each member_role_unobserved line as it was
// passed to the logger, before any encoding.
type reasonSink struct {
	logr.LogSink
	reasons *[]string
}

func (s *reasonSink) Init(logr.RuntimeInfo)          {}
func (s *reasonSink) Enabled(int) bool               { return true }
func (s *reasonSink) WithValues(...any) logr.LogSink { return s }
func (s *reasonSink) WithName(string) logr.LogSink   { return s }
func (s *reasonSink) Error(error, string, ...any)    {}
func (s *reasonSink) Info(_ int, _ string, kv ...any) {
	fields := map[any]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		fields[kv[i]] = kv[i+1]
	}
	if fields["event"] == "member_role_unobserved" {
		*s.reasons = append(*s.reasons, fmt.Sprint(fields["reason"]))
	}
}

// obsProber answers each member with the observation the test set for it.
type obsProber struct{ obs map[string]RoleObservation }

func (p *obsProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	return p.obs[podName]
}

// A member whose role was known and no longer is gets one log line that says
// why its observation does not count; a member that stays unknown, or keeps
// its role, gets none (#438).
func TestReconcileHAStatus_LogsWhyAMemberIsNoLongerObserved(t *testing.T) {
	cluster := haCluster("why")
	m0, m1, m2 := "why-0", "why-1", "why-2"
	prober := &obsProber{obs: map[string]RoleObservation{m0: slave(), m1: slave(), m2: master()}}
	r := &CubridClusterReconciler{Clock: func() time.Time { return testNow }, Prober: prober}
	var lines []string
	ctx := logf.IntoContext(context.Background(), funcr.New(func(_, line string) {
		if strings.Contains(line, `"event"="member_role_unobserved"`) {
			lines = append(lines, line)
		}
	}, funcr.Options{}))

	r.reconcileHAStatus(ctx, cluster, 3)
	if len(lines) != 0 {
		t.Fatalf("members whose roles are known were logged: %q", lines)
	}
	why := "the Instance Manager reported no role: heartbeat status failed: signal: killed"
	prober.obs[m0] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: testNow, Reason: why}
	prober.obs[m1] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow.Add(-time.Minute)}
	r.reconcileHAStatus(ctx, cluster, 3)
	if len(lines) != 2 {
		t.Fatalf("lines = %q, want one for %s and one for %s", lines, m0, m1)
	}
	for member, want := range map[string]string{m0: why, m1: "older than"} {
		found := false
		for _, line := range lines {
			if strings.Contains(line, `"member"="`+member+`"`) {
				found = true
				if !strings.Contains(line, want) || !strings.Contains(line, `"oldRole"="slave"`) {
					t.Errorf("line for %s = %s, want the reason %q and the old role", member, line, want)
				}
			}
		}
		if !found {
			t.Errorf("no line for %s: %q", member, lines)
		}
	}
	r.reconcileHAStatus(ctx, cluster, 3)
	if len(lines) != 2 {
		t.Errorf("a member that stays unknown was logged again: %q", lines[2:])
	}
}
