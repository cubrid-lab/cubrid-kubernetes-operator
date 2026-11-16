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
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// These HTTP replies exercise credential selection and primary authority together.
// They do not reproduce the engine's incomplete heartbeat output observed in
// scenario S14 (#438), or prove that real members remain healthy during rotation.
func TestPrimaryResolutionDuringTokenOverlap(t *testing.T) {
	ctx := context.Background()
	members := []string{"db-0", "db-1", "db-2"}
	primary := members[1]
	secret := tokenSecret("db", "ns", previousTok)
	reader := fake.NewClientBuilder().WithObjects(secret).Build()
	held := newHolders(map[string]string{})
	answers := map[string]instancemanager.HAStatus{}
	for _, member := range members {
		held.token[member+".ns.svc"] = previousTok
		role := instancemanager.RoleSlave
		if member == primary {
			role = instancemanager.RoleMaster
		}
		answers[member] = instancemanager.HAStatus{
			Current: member, Role: role, ServerActive: member == primary,
			Nodes: []instancemanager.NodeState{
				node(members[0], "slave"), node(primary, "master"), node(members[2], "slave"),
			},
		}
	}
	prober := NewHTTPRoleProber(ClusterTokens(reader))
	prober.Now = func() time.Time { return testNow }
	// Reuse the token-holder transport's authentication and request record. An
	// authenticated reply contains a complete role instead of its default {}.
	// A refusal deliberately contains the same plausible master body: the HTTP
	// status must prevent it from becoming role evidence.
	prober.Client = &http.Client{Transport: toHandler{h: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		response, err := held.RoundTrip(req)
		if err != nil {
			t.Fatalf("authenticate member request: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		member, _, _ := strings.Cut(req.URL.Hostname(), ".")
		w.WriteHeader(response.StatusCode)
		if err := json.NewEncoder(w).Encode(answers[member]); err != nil {
			t.Fatalf("encode member role: %v", err)
		}
	})}}
	check := func(name, wantReason, wantPrimary string, sent map[string][]string) map[string]RoleObservation {
		t.Helper()
		held.sent = map[string][]string{}
		observations := map[string]RoleObservation{}
		t.Run(name, func(t *testing.T) {
			for _, member := range members {
				observations[member] = prober.ProbeRole(ctx, member, "ns")
				if got := held.sent[member+".ns.svc"]; !slices.Equal(got, sent[member]) {
					t.Errorf("%s did not receive the expected credential attempts", member)
				}
			}
			result := resolvePrimary(members, observations, testNow)
			wantStatus := metav1.ConditionFalse
			if wantPrimary != "" {
				wantStatus = metav1.ConditionTrue
			}
			if result.Status != wantStatus || result.Reason != wantReason || result.CurrentPrimary != wantPrimary {
				t.Fatalf("resolution = %s %s %q, want %s %s %q",
					result.Status, result.Reason, result.CurrentPrimary, wantStatus, wantReason, wantPrimary)
			}
			if wantReason == reasonTokenRefused && !slices.Equal(result.TokenRefused, []string{primary}) {
				t.Errorf("refused members = %v, want only %s", result.TokenRefused, primary)
			}
		})
		return observations
	}
	oldAttempts := map[string][]string{}
	currentAttempts := map[string][]string{}
	for _, member := range members {
		oldAttempts[member] = []string{"Bearer " + previousTok}
		currentAttempts[member] = []string{"Bearer " + currentTok}
	}
	check("before rotation", singlePrimary, primary, oldAttempts)

	secret.Data[imTokenKey] = []byte(currentTok)
	secret.Data[imPreviousTokenKey] = []byte(previousTok)
	if err := reader.Update(ctx, secret); err != nil {
		t.Fatalf("start token overlap: %v", err)
	}
	check("overlap keeps calling the token each member accepted", singlePrimary, primary, oldAttempts)

	held.token[members[0]+".ns.svc"] = currentTok
	mixedAttempts := map[string][]string{
		members[0]: {"Bearer " + previousTok, "Bearer " + currentTok},
		primary:    oldAttempts[primary], members[2]: oldAttempts[members[2]],
	}
	check("a member switches to the current token", singlePrimary, primary, mixedAttempts)
	mixedAttempts[members[0]] = currentAttempts[members[0]]
	check("mixed holders receive their last accepted token first", singlePrimary, primary, mixedAttempts)

	for _, member := range members {
		held.token[member+".ns.svc"] = currentTok
	}
	delete(secret.Data, imPreviousTokenKey)
	if err := reader.Update(ctx, secret); err != nil {
		t.Fatalf("end token overlap: %v", err)
	}
	check("retired credentials are ignored despite remembered acceptances", singlePrimary, primary, currentAttempts)

	// Reopen the candidate list to ensure the refusal consumes both attempts,
	// even after this prober previously observed an authenticated primary.
	secret.Data[imPreviousTokenKey] = []byte(previousTok)
	if err := reader.Update(ctx, secret); err != nil {
		t.Fatalf("prepare both token candidates: %v", err)
	}
	held.token[primary+".ns.svc"] = "neither-candidate"
	refusedAttempts := map[string][]string{
		members[0]: currentAttempts[members[0]], primary: {"Bearer " + currentTok, "Bearer " + previousTok},
		members[2]: currentAttempts[members[2]],
	}
	observations := check("a final refusal cannot lend its master body authority", reasonTokenRefused, "", refusedAttempts)
	refused := observations[primary]
	if refused.Reachable || !refused.Responded || !refused.TokenRefused || refused.ActiveMasterClaim || authoritative(refused, testNow) {
		t.Errorf("a refused master reply supplied role authority: %+v", refused)
	}

	held.token[primary+".ns.svc"] = currentTok
	answer := answers[members[0]]
	answer.Role = instancemanager.RoleUnknown
	answer.Reason = "no HA-Node Info line (heartbeat not running or empty output)"
	answers[members[0]] = answer
	observations = check("authenticated missing role keeps the primary unresolved", incomplete, "", currentAttempts)
	unobserved := observations[members[0]]
	if !unobserved.Reachable || unobserved.TokenRefused || authoritative(unobserved, testNow) ||
		!strings.Contains(unobserved.Reason, answer.Reason) {
		t.Errorf("an authenticated unknown role was confused with a token refusal or role authority: %+v", unobserved)
	}
}
