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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// refusedTok is the token the operator sends in these tests; it must never
// appear in an observation, an error, a condition or an Event.
const refusedTok = "s3cr3t-refused-token"

// answerStatus answers every request with the given status code.
type answerStatus int

func (a answerStatus) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: int(a), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"error":"refused"}`)), Request: req}, nil
}

// A member whose Instance Manager refuses the cluster's token (401) or forbids
// the call (403) is reported as a refusal, not as unreachable, and is still no
// evidence of its role (ADR-0005) (#325).
func TestProbeRole_RefusedTokenIsNotUnreachable(t *testing.T) {
	probe := func(rt http.RoundTripper) RoleObservation {
		p := &HTTPRoleProber{Client: &http.Client{Transport: rt}, Tokens: staticToken(refusedTok),
			Now: func() time.Time { return testNow }}
		return p.ProbeRole(context.Background(), c0, "ns")
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		o := probe(answerStatus(status))
		if o.Reachable || !o.TokenRefused {
			t.Errorf("status %d: got %+v, want a refused member that is no evidence", status, o)
		}
	}
	for name, rt := range map[string]http.RoundTripper{
		"unreachable": unreachable{}, "server error": answerStatus(http.StatusInternalServerError),
	} {
		if o := probe(rt); o.Reachable || o.TokenRefused {
			t.Errorf("%s: got %+v, want an unreachable member that refused nothing", name, o)
		}
	}
}

// A backup or restore call the member refuses names the refusal, never the
// token (#325).
func TestHTTPBackupClient_RefusedTokenIsNamed(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		c := NewHTTPBackupClient(staticToken(refusedTok))
		c.Client = &http.Client{Transport: answerStatus(status)}
		calls := map[string]func() error{
			"StartBackup": func() error {
				_, err := c.StartBackup(context.Background(), "db-0", "ns", "k", instancemanager.BackupRequest{})
				return err
			},
			"StartRestore": func() error {
				_, err := c.StartRestore(context.Background(), "db-0", "ns", "k", instancemanager.RestoreRequest{})
				return err
			},
			"StartHABootstrap": func() error {
				_, err := c.StartHABootstrap(context.Background(), "db-0", "ns", "k", instancemanager.HABootstrapRequest{})
				return err
			},
			"GetOperation": func() error {
				_, err := c.GetOperation(context.Background(), "db-0", "ns", "op-1")
				return err
			},
		}
		for name, call := range calls {
			err := call()
			if !errors.Is(err, errTokenRefused) {
				t.Errorf("%s answered %d: err = %v, want a token refusal", name, status, err)
				continue
			}
			if !strings.Contains(err.Error(), "refused the cluster's Instance Manager token") {
				t.Errorf("%s answered %d: %q does not name the refusal", name, status, err)
			}
			if strings.Contains(err.Error(), refusedTok) {
				t.Errorf("%s answered %d: the error carries the token", name, status)
			}
			if got := instanceManagerFailureReason(err); got != reasonTokenRefused {
				t.Errorf("%s answered %d: reason = %q, want %q", name, status, got, reasonTokenRefused)
			}
		}
	}
	c := NewHTTPBackupClient(staticToken(refusedTok))
	c.Client = &http.Client{Transport: answerStatus(http.StatusInternalServerError)}
	if _, err := c.GetOperation(context.Background(), "db-0", "ns", "op-1"); errors.Is(err, errTokenRefused) ||
		instanceManagerFailureReason(err) != "InstanceManagerUnavailable" {
		t.Errorf("a server error was reported as a refusal: %v", err)
	}
}

// A refused member keeps the primary unresolved like an unreachable one; the
// reason names the refusal, unless a more severe observation decides it.
func TestResolvePrimary_RefusedToken(t *testing.T) {
	refused := RoleObservation{TokenRefused: true, ObservedAt: testNow}
	members := []string{c0, c1, c2}
	cases := []struct {
		name   string
		obs    map[string]RoleObservation
		reason string
	}{
		{"a refused slave", map[string]RoleObservation{c0: master(), c1: slave(), c2: refused}, reasonTokenRefused},
		{"refused wins over unreachable", map[string]RoleObservation{c0: master(), c1: unreach(), c2: refused}, reasonTokenRefused},
		{"ambiguous wins over refused",
			map[string]RoleObservation{c0: conflicting(master()), c1: slave(), c2: refused}, ambiguous},
		{"multiple masters wins over refused",
			map[string]RoleObservation{c0: master(), c1: master(), c2: refused}, multiplePrimaries},
	}
	for _, c := range cases {
		got := resolvePrimary(members, c.obs, testNow)
		if got.Status != metav1.ConditionFalse || got.CurrentPrimary != "" || got.Reason != c.reason {
			t.Errorf("%s: got %+v, want False/%s without a primary", c.name, got, c.reason)
		}
	}
}

// The reconciler reports a refusal in PrimaryResolved and with one Warning
// Event, and neither carries the token (#325).
func TestReconcileHAStatus_ReportsRefusedToken(t *testing.T) {
	recorder := record.NewFakeRecorder(20)
	r := &CubridClusterReconciler{
		Recorder: recorder, Clock: func() time.Time { return testNow },
		Prober: &HTTPRoleProber{Client: &http.Client{Transport: answerStatus(http.StatusUnauthorized)},
			Tokens: staticToken(refusedTok), Now: func() time.Time { return testNow }},
	}
	cluster := haCluster("refused")
	reconcile := func() []string {
		before := cluster.Status.DeepCopy()
		res := r.reconcileHAStatus(context.Background(), cluster, 2)
		if res.CurrentPrimary != "" || res.Reason != reasonTokenRefused {
			t.Fatalf("resolution = %+v, want no primary with %s", res, reasonTokenRefused)
		}
		r.reportTransitions(context.Background(), cluster, before)
		return drain(recorder)
	}

	events := reconcile()
	c := meta.FindStatusCondition(cluster.Status.Conditions, conditionPrimaryResolved)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonTokenRefused ||
		!strings.Contains(c.Message, "Instance Managers of refused-0, refused-1 refused") {
		t.Fatalf("PrimaryResolved = %+v, want False/%s naming both members", c, reasonTokenRefused)
	}
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+reasonTokenRefused+" ") {
		t.Fatalf("events = %q, want one Warning %s", events, reasonTokenRefused)
	}
	for _, s := range append(events, c.Message) {
		if strings.Contains(s, refusedTok) {
			t.Errorf("%q carries the token", s)
		}
	}
	if again := reconcile(); len(again) != 0 {
		t.Errorf("an unchanged refusal was reported again: %q", again)
	}
	for _, in := range cluster.Status.Instances {
		if in.Role != databasev1alpha1.RoleUnknown || in.Ready {
			t.Errorf("refused member %s = %+v, want unknown and not ready", in.Name, in)
		}
	}
}
