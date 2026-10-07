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
	"io"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// staticToken gives every member the same token.
func staticToken(token string) TokenSource {
	return func(context.Context, string, string) (string, error) { return token, nil }
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
