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
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// dialRecorder answers every member as a slave and records the members it
// was asked about. A request to a member in hang is never answered: it waits
// until the request's context ends, as a request to an address nobody serves
// waits out the probe's timeout.
type dialRecorder struct {
	mu     sync.Mutex
	dialed map[string]int
	hang   map[string]bool
}

func (d *dialRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	member, _, _ := strings.Cut(req.URL.Hostname(), ".")
	d.mu.Lock()
	d.dialed[member]++
	d.mu.Unlock()
	if d.hang[member] {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	return (&switchAnswer{status: instancemanager.HAStatus{Role: instancemanager.RoleSlave}}).RoundTrip(req)
}

// A member whose Pod has no IP yet, as a Pod recreated after a node fence
// that stays Pending (#1003), is not asked: its observation is the one an
// unanswered probe gives, recorded at once instead of after the probe's
// timeout. It is no answer: the member stays silent and its role unknown.
// A member whose Pod has an IP, or whose Pod was not found, is asked as
// before.
func TestObserveHAStatus_PodWithoutIPIsNotAsked(t *testing.T) {
	cluster := haCluster("noip")
	m0, m1, m2 := "noip-0", "noip-1", "noip-2"
	rt := &dialRecorder{dialed: map[string]int{}, hang: map[string]bool{m1: true}}
	r := &CubridClusterReconciler{Clock: func() time.Time { return testNow },
		Prober: &HTTPRoleProber{Client: &http.Client{Transport: rt}, Tokens: staticToken(refusedTok),
			Now: func() time.Time { return testNow }}}
	pods := map[string]memberIdentity{
		m0: {PodUID: "uid-0"},
		m1: {PodUID: "uid-1", NoPodIP: true},
		// m2's Pod was not found: it has the empty identity.
	}
	ctx, cancel := context.WithTimeout(context.Background(), roleProbeTimeout)
	defer cancel()
	start := time.Now()
	_, obs, _ := r.observeHAStatus(ctx, cluster, 3, pods)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the pass took %s, want it not to wait for the member without an IP", elapsed)
	}
	if rt.dialed[m1] != 0 {
		t.Errorf("%s was asked %d times, want never: its Pod has no IP", m1, rt.dialed[m1])
	}
	for _, m := range []string{m0, m2} {
		if rt.dialed[m] != 1 || !obs[m].Reachable || obs[m].Role != databasev1alpha1.RoleSlave {
			t.Errorf("%s: asked %d times, observation %+v; want it asked once and answering as a slave",
				m, rt.dialed[m], obs[m])
		}
	}
	o := obs[m1]
	if o.Reachable || o.TokenRefused || o.Responded || o.Role != "" || o.RequestID != "" ||
		o.Reason != reasonPodHasNoIP || !o.ObservedAt.Equal(testNow) {
		t.Errorf("observation of %s = %+v, want the observation of an unanswered probe with the reason %q",
			m1, o, reasonPodHasNoIP)
	}
	if !silent(o, testNow) {
		t.Errorf("observation of %s = %+v, want a silent member that is no evidence", m1, o)
	}
	for _, in := range cluster.Status.Instances {
		if in.Name == m1 && in.Role != databasev1alpha1.RoleUnknown {
			t.Errorf("status of %s = %+v, want its role unknown", m1, in)
		}
	}
}

// Without the Pods read (their read failed, or the caller read none), every
// member is asked as before.
func TestObserveHAStatus_WithoutPodsEveryMemberIsAsked(t *testing.T) {
	rt := &dialRecorder{dialed: map[string]int{}}
	r := &CubridClusterReconciler{Clock: func() time.Time { return testNow },
		Prober: &HTTPRoleProber{Client: &http.Client{Transport: rt}, Tokens: staticToken(refusedTok),
			Now: func() time.Time { return testNow }}}
	r.reconcileHAStatus(context.Background(), haCluster("all"), 3)
	for _, m := range []string{"all-0", "all-1", "all-2"} {
		if rt.dialed[m] != 1 {
			t.Errorf("%s was asked %d times, want once", m, rt.dialed[m])
		}
	}
}

// The members' Pods read for the write quarantine say whether each has an IP.
func TestMemberIdentitiesMarkAPodWithoutIP(t *testing.T) {
	pod := func(name, ip string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID("uid-" + name)},
			Status: corev1.PodStatus{PodIP: ip}}
	}
	r := &CubridClusterReconciler{Client: fake.NewClientBuilder().
		WithObjects(pod("ip-0", "10.0.0.5"), pod("ip-1", "")).Build()}
	cluster := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: "ip", Namespace: "ns"}}
	ids, err := r.memberIdentities(context.Background(), cluster, []string{"ip-0", "ip-1", "ip-2"})
	if err != nil {
		t.Fatal(err)
	}
	if ids["ip-0"].NoPodIP || !ids["ip-1"].NoPodIP || ids["ip-2"] != (memberIdentity{}) {
		t.Fatalf("identities = %+v, want only ip-1 marked without an IP and ip-2 without a Pod", ids)
	}
}
