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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// terminatingPod is a database Pod of cluster qc deleted at deletedAt with a
// grace period of grace, whose container is in state.
func terminatingPod(uid string, deletedAt time.Time, grace int64, state corev1.ContainerState) *corev1.Pod {
	deletion := metav1.NewTime(deletedAt.Add(time.Duration(grace) * time.Second))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: q0, Namespace: "ns", UID: types.UID(uid),
			DeletionTimestamp: &deletion, DeletionGracePeriodSeconds: &grace,
			Labels: labelsFor(&databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: "qc"}})},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: appName, State: state}}},
	}
}

// Only a termination the kubelet reported after the deletion was requested
// counts: a terminated state left from a crash before it, or one the kubelet
// could not read, does not.
func TestPodTerminatedNeedsAFreshKubeletReport(t *testing.T) {
	finished := func(at time.Time, reason string) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.NewTime(at), Reason: reason}}
	}
	for name, tc := range map[string]struct {
		state corev1.ContainerState
		want  bool
	}{
		"finished after the deletion": {finished(testNow.Add(3*time.Second), "Completed"), true},
		"finished a moment before it": {finished(testNow.Add(-time.Second), "Completed"), true},
		"finished long before it":     {finished(testNow.Add(-time.Minute), "Error"), false},
		"no finish time":              {corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}, false},
		"status unknown":              {finished(testNow.Add(3*time.Second), "ContainerStatusUnknown"), false},
		"still running":               {corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := podTerminated(terminatingPod("a", testNow, 120, tc.state)); got != tc.want {
				t.Errorf("podTerminated = %t, want %t", got, tc.want)
			}
		})
	}
}

// recordFor runs recordTerminations for q0 of a cluster whose status names
// last as q0's last Pod and holds records, with q0 read as now in this pass.
func recordFor(r *CubridClusterReconciler, last string, records []databasev1alpha1.MemberTermination,
	now memberIdentity) []databasev1alpha1.MemberTermination {
	cluster := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: "qc", Namespace: "ns"}}
	cluster.Status.MemberPods = []databasev1alpha1.MemberPod{{Name: q0, PodUID: last}}
	cluster.Status.MemberTerminations = records
	ids := map[string]memberIdentity{q0: now}
	return r.recordTerminations(context.Background(), cluster, []string{q0}, ids, ids)
}

// A termination is recorded only for the Pod the Operator last saw of the
// member: a Pod read terminated in its first read may follow another Pod the
// Operator never saw.
func TestRecordTerminationsOnlyForTheLastPodSeen(t *testing.T) {
	r := &CubridClusterReconciler{Clock: func() time.Time { return testNow }}
	if got := recordFor(r, "a", nil, memberIdentity{PodUID: "b", Terminated: true}); len(got) != 0 {
		t.Fatalf("recorded %+v for a Pod after one the Operator saw last", got)
	}
	if got := recordFor(r, "b", nil, memberIdentity{PodUID: "b", Terminated: true}); len(got) != 1 || got[0].PodUID != "b" {
		t.Fatalf("records = %+v, want b", got)
	}
}

// A database Pod of the member deleted without a termination the kubelet
// confirmed ends the member's record.
func TestRecordTerminationsEndOnAnUnconfirmedDeletion(t *testing.T) {
	r := &CubridClusterReconciler{Clock: func() time.Time { return testNow }}
	record := []databasev1alpha1.MemberTermination{{Name: q0, PodUID: "a", ConfirmedAt: metav1.NewTime(testNow)}}
	next := terminatingPod("b", testNow, 0, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
	r.notePodDeleted(next, false)
	if got := recordFor(r, "a", record, memberIdentity{PodUID: "c"}); len(got) != 0 {
		t.Fatalf("records = %+v, want none after %s was deleted unconfirmed", got, next.UID)
	}
}
