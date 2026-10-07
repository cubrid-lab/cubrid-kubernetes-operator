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
	"io"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// staticToken gives every member the same token.
func staticToken(token string) TokenSource {
	return func(context.Context, string, string) ([]string, error) { return []string{token}, nil }
}

// authRecorder is a transport that records the Authorization header sent to
// each host and answers 200 with an empty JSON object.
type authRecorder struct{ sent map[string]string }

func (a *authRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	a.sent[req.URL.Hostname()] = req.Header.Get("Authorization")
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

func tokenSecret(cluster, namespace, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: imTokenSecretName(cluster), Namespace: namespace},
		Data:       map[string][]byte{imTokenKey: []byte(token)},
	}
}

// Each cluster's members are called with that cluster's token only: two
// clusters of the same name in two namespaces, and a cluster whose name ends
// like another member's, never receive each other's credential (#270).
func TestClusterTokens_CallEachMemberWithItsOwnClusterToken(t *testing.T) {
	reader := fake.NewClientBuilder().WithObjects(
		tokenSecret("db", "team-a", "token-a"),
		tokenSecret("db", "team-b", "token-b"),
		tokenSecret("db-1", "team-a", "token-a1"),
	).Build()
	tokens := ClusterTokens(reader)
	sent := &authRecorder{sent: map[string]string{}}
	prober := NewHTTPRoleProber(tokens)
	prober.Client = &http.Client{Transport: sent}
	backup := NewHTTPBackupClient(tokens)
	backup.Client = &http.Client{Transport: sent}

	prober.ProbeRole(context.Background(), "db-0", "team-a")
	prober.ProbeRole(context.Background(), "db-0", "team-b")
	prober.ProbeRole(context.Background(), "db-1-0", "team-a")
	if _, err := backup.GetOperation(context.Background(), "db-1", "team-b", "op-1"); err != nil {
		t.Fatalf("GetOperation: %v", err)
	}

	want := map[string]string{
		"db-0.team-a.svc":   "Bearer token-a",
		"db-0.team-b.svc":   "Bearer token-b",
		"db-1-0.team-a.svc": "Bearer token-a1",
		"db-1.team-b.svc":   "Bearer token-b",
	}
	for host, auth := range want {
		if sent.sent[host] != auth {
			t.Errorf("%s was called with %q, want %q", host, sent.sent[host], auth)
		}
	}
}

// A member whose cluster token cannot be read is not called at all: an
// unauthenticated call or another cluster's token is never the fallback.
func TestClusterTokens_NoTokenNoCall(t *testing.T) {
	reader := fake.NewClientBuilder().WithObjects(tokenSecret("empty", "ns", "")).Build()
	tokens := ClusterTokens(reader)
	sent := &authRecorder{sent: map[string]string{}}
	prober := NewHTTPRoleProber(tokens)
	prober.Client = &http.Client{Transport: sent}
	backup := NewHTTPBackupClient(tokens)
	backup.Client = &http.Client{Transport: sent}

	for _, member := range []string{"missing-0", "empty-0", "notamember", "db-x"} {
		if o := prober.ProbeRole(context.Background(), member, "ns"); o.Reachable {
			t.Errorf("%s: probe without a token reported reachable", member)
		}
		if _, err := backup.GetOperation(context.Background(), member, "ns", "op-1"); err == nil {
			t.Errorf("%s: GetOperation without a token succeeded", member)
		}
	}
	if len(sent.sent) != 0 {
		t.Errorf("members were called without their token: %v", sent.sent)
	}

	unset := &HTTPRoleProber{Client: &http.Client{Transport: sent}}
	if o := unset.ProbeRole(context.Background(), "db-0", "ns"); o.Reachable || len(sent.sent) != 0 {
		t.Errorf("a prober without a token source called %v", sent.sent)
	}
}

const (
	currentTok  = "new"
	previousTok = "old"
	// The hosts of member db-0, which holds the previous token, and db-1,
	// which holds the current one.
	holderOfPrevious = "db-0.ns.svc"
	holderOfCurrent  = "db-1.ns.svc"
)

// rotatedSecret is a cluster token Secret during a rotation: the current token
// and the previous one, which Pods started before the rotation still hold.
func rotatedSecret(cluster, namespace, current, previous string) *corev1.Secret {
	s := tokenSecret(cluster, namespace, current)
	s.Data["previousToken"] = []byte(previous)
	return s
}

// holders answers for each member's host as a manager that holds one token:
// it refuses any other with 401 before it acts, as the Instance Manager's auth
// does. It records every Authorization header a member was sent and the body
// of the request it accepted.
type holders struct {
	token map[string]string
	sent  map[string][]string
	body  map[string]string
}

func newHolders(token map[string]string) *holders {
	return &holders{token: token, sent: map[string][]string{}, body: map[string]string{}}
}

func (h *holders) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	got := req.Header.Get("Authorization")
	h.sent[host] = append(h.sent[host], got)
	status, answer := http.StatusUnauthorized, `{"error":"unauthorized"}`
	if got == "Bearer "+h.token[host] {
		status, answer = http.StatusOK, "{}"
		if req.Body != nil {
			b, _ := io.ReadAll(req.Body)
			h.body[host] = string(b)
		}
		if req.Method == http.MethodPost {
			status, answer = http.StatusAccepted, `{"id":"op-1"}`
		}
	}
	return &http.Response{StatusCode: status, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(answer)), Request: req}, nil
}

// During a rotation the operator reaches a member that still holds the
// previous token as well as one that holds the current token (#324). A member
// that accepted a token is sent that one first next time, so it does not log a
// refused request on every call.
func TestClusterTokens_OverlapReachesMembersHoldingEitherToken(t *testing.T) {
	reader := fake.NewClientBuilder().WithObjects(rotatedSecret("db", "ns", currentTok, previousTok)).Build()
	tokens := ClusterTokens(reader)
	h := newHolders(map[string]string{holderOfPrevious: previousTok, holderOfCurrent: currentTok})
	prober := NewHTTPRoleProber(tokens)
	prober.Client = &http.Client{Transport: h}
	backup := NewHTTPBackupClient(tokens)
	backup.Client = &http.Client{Transport: h}

	for _, member := range []string{"db-0", "db-1"} {
		if o := prober.ProbeRole(context.Background(), member, "ns"); !o.Reachable {
			t.Errorf("%s was not reached during the overlap", member)
		}
	}
	if got := h.sent[holderOfCurrent]; len(got) != 1 || got[0] != "Bearer "+currentTok {
		t.Errorf("the member holding the current token was sent %v, want the current token once", got)
	}
	if got := h.sent[holderOfPrevious]; len(got) != 2 || got[0] != "Bearer "+currentTok || got[1] != "Bearer "+previousTok {
		t.Errorf("the member holding the previous token was sent %v, want the current then the previous token", got)
	}

	prober.ProbeRole(context.Background(), "db-0", "ns")
	if got := h.sent[holderOfPrevious]; len(got) != 3 || got[2] != "Bearer "+previousTok {
		t.Errorf("a member that accepted the previous token was sent %v, want the previous token first", got)
	}
	if _, err := backup.GetOperation(context.Background(), "db-1", "ns", "op-1"); err != nil {
		t.Errorf("GetOperation on the member holding the current token: %v", err)
	}
}

// A request with a body that the member refuses with the current token is
// sent again whole with the previous one.
func TestHTTPBackupClient_ResendsTheBodyWithThePreviousToken(t *testing.T) {
	reader := fake.NewClientBuilder().WithObjects(rotatedSecret("db", "ns", currentTok, previousTok)).Build()
	h := newHolders(map[string]string{holderOfPrevious: previousTok})
	backup := NewHTTPBackupClient(ClusterTokens(reader))
	backup.Client = &http.Client{Transport: h}

	if _, err := backup.StartBackup(context.Background(), "db-0", "ns", "key-1",
		instancemanager.BackupRequest{Database: testDatabase}); err != nil {
		t.Fatalf("StartBackup on a member holding the previous token: %v", err)
	}
	if got := h.sent[holderOfPrevious]; len(got) != 2 {
		t.Errorf("the member was sent %v, want the current then the previous token", got)
	}
	if !strings.Contains(h.body[holderOfPrevious], `"database":"`+testDatabase+`"`) {
		t.Errorf("the request sent with the previous token arrived as %q", h.body[holderOfPrevious])
	}
}

// After the overlap the previous token is gone from the Secret and is never
// sent again: a member that still holds it refuses the operator, which then
// has no evidence for it (#324).
func TestClusterTokens_PreviousTokenNotSentAfterOverlap(t *testing.T) {
	reader := fake.NewClientBuilder().WithObjects(tokenSecret("db", "ns", currentTok)).Build()
	h := newHolders(map[string]string{holderOfPrevious: previousTok})
	prober := NewHTTPRoleProber(ClusterTokens(reader))
	prober.Client = &http.Client{Transport: h}

	if o := prober.ProbeRole(context.Background(), "db-0", "ns"); o.Reachable {
		t.Error("a member holding only the dropped token was reported reachable")
	}
	for _, auth := range h.sent[holderOfPrevious] {
		if auth != "Bearer "+currentTok {
			t.Errorf("a token other than the current one was sent: %q", auth)
		}
	}
}

// AcceptsToken asks with the given token alone: a member holding another token
// refuses it, and one that cannot be reached is an error, not an acceptance.
func TestHTTPRoleProber_AcceptsToken(t *testing.T) {
	h := newHolders(map[string]string{holderOfPrevious: previousTok, holderOfCurrent: currentTok})
	prober := NewHTTPRoleProber(nil)
	prober.Client = &http.Client{Transport: h}

	if ok, err := prober.AcceptsToken(context.Background(), "db-1", "ns", currentTok); err != nil || !ok {
		t.Errorf("the member holding the token: accepted=%v err=%v", ok, err)
	}
	if ok, err := prober.AcceptsToken(context.Background(), "db-0", "ns", currentTok); err != nil || ok {
		t.Errorf("the member holding the previous token: accepted=%v err=%v", ok, err)
	}
	if got := h.sent[holderOfPrevious]; len(got) != 1 || got[0] != "Bearer "+currentTok {
		t.Errorf("the member was sent %v, want the asked token only", got)
	}

	prober.Client = &http.Client{Transport: unreachable{}}
	if ok, err := prober.AcceptsToken(context.Background(), "db-1", "ns", currentTok); err == nil || ok {
		t.Errorf("an unreachable member: accepted=%v err=%v", ok, err)
	}
}

type unreachable struct{}

func (unreachable) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}
