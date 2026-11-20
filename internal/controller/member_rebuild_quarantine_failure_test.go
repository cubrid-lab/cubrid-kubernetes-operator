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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// When the write quarantine step of a pass fails, the status may still hold
// the quarantine of an earlier pass, for example none while this pass's
// observation would start one. The rebuild then decides nothing from that
// status: a request that a pass with a working quarantine step accepts is
// refused as unreadable, and the offer and its token stay as they were (#117,
// condition 4 of the review of #1018).
func TestRebuildTreatsAFailedQuarantineStepAsUnreadable(t *testing.T) {
	const name, ns = "rbq", "ns"
	m0, m1, m2 := name+"-0", name+"-1", name+"-2"
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	sch := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(sch), databasev1alpha1.AddToScheme(sch)); err != nil {
		t.Fatal(err)
	}
	objs := []client.Object{}
	for _, m := range []string{m0, m1, m2} {
		objs = append(objs,
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: m, Namespace: ns, UID: types.UID("pod-" + m),
				CreationTimestamp: metav1.NewTime(now.Add(-time.Minute))}},
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dataVolume + "-" + m, Namespace: ns,
				UID: types.UID("claim-" + m), CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
				Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-" + m}})
	}
	offer := databasev1alpha1.MemberRebuildOffer{Member: m2, PodUID: "pod-" + m2, ClaimUID: "claim-" + m2,
		VolumeName: "pv-" + m2, Source: m0, SourcePodUID: "pod-" + m0, Token: "token-of-the-offer"}
	cluster := &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: "rbq-uid",
			Annotations: map[string]string{rebuildMemberAnnotation: m2 + ":" + offer.Token}},
		Spec: databasev1alpha1.CubridClusterSpec{Databases: []databasev1alpha1.CubridDatabase{{Name: testDatabase}}},
		Status: databasev1alpha1.CubridClusterStatus{
			CurrentPrimary: m0, LastResolvedPrimary: m0,
			Databases: []databasev1alpha1.DatabaseStatus{{Name: testDatabase, PrimaryCreated: true,
				HAConfigured: true, SeededMembers: []string{m1, m2}}},
			MemberDataClaims: []databasev1alpha1.MemberDataClaim{
				{Name: m0, ClaimUID: "claim-" + m0, VolumeName: "pv-" + m0},
				{Name: m1, ClaimUID: "claim-" + m1, VolumeName: "pv-" + m1},
				{Name: m2, ClaimUID: "claim-" + m2 + "-lost", VolumeName: "pv-" + m2 + "-lost"}},
			MemberRebuildOffers: []databasev1alpha1.MemberRebuildOffer{offer},
		},
	}
	objs = append(objs, cluster.DeepCopy())
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	obs := map[string]RoleObservation{
		m0: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: now},
		m1: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now},
		m2: {},
	}
	pass := func(quarantineErr error) (*databasev1alpha1.CubridCluster, []string) {
		t.Helper()
		recorder := record.NewFakeRecorder(10)
		r := &CubridClusterReconciler{Client: c, APIReader: c, Scheme: sch, Recorder: recorder,
			Clock: func() time.Time { return now }}
		ctx := context.Background()
		members := []string{m0, m1, m2}
		before, err := r.memberIdentities(ctx, cluster, members)
		if err != nil {
			t.Fatal(err)
		}
		got := cluster.DeepCopy()
		steps := r.reconcileMemberRebuild(ctx, got, members, obs, now, m0, before, nil, quarantineErr)
		if steps.err != nil {
			t.Fatalf("the rebuild step returns no error of its own, got %v", steps.err)
		}
		var events []string
		for len(recorder.Events) > 0 {
			events = append(events, <-recorder.Events)
		}
		return got, events
	}

	got, events := pass(errors.New("decide the write quarantine: no random value"))
	if got.Status.MemberRebuild != nil {
		t.Fatalf("a pass whose quarantine step failed accepts nothing, got %+v", got.Status.MemberRebuild)
	}
	if len(got.Status.MemberRebuildOffers) != 1 || got.Status.MemberRebuildOffers[0] != offer {
		t.Fatalf("the offer and its token are kept, got %+v", got.Status.MemberRebuildOffers)
	}
	if len(events) != 1 || !strings.Contains(events[0], reasonMemberRebuildRefused) ||
		!strings.Contains(events[0], "write quarantine could not be decided") {
		t.Fatalf("expected one refusal saying the quarantine could not be decided, got %q", events)
	}

	got, _ = pass(nil)
	if got.Status.MemberRebuild == nil || got.Status.MemberRebuild.Token != offer.Token {
		t.Fatalf("a pass whose quarantine step worked accepts the request, got %+v", got.Status.MemberRebuild)
	}
}
