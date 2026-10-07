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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// A cluster's Instance Manager token is replaced with an overlap (#324): the
// Secret keeps the previous token, which running Pods hold, until every member
// was started after the rotation. The Pods here are API objects without a
// kubelet; that a started container holds the Secret's token is the
// kubelet's behavior and is not shown by these tests.
var _ = Describe("Instance Manager token rotation (#324)", func() {
	ctx := context.Background()
	rotation := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	newReconciler := func(now time.Time) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			Clock: func() time.Time { return now },
		}
	}
	reconcileAt := func(now time.Time, name string) *record.FakeRecorder {
		r := newReconciler(now)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace}})
		Expect(err).NotTo(HaveOccurred())
		return r.Recorder.(*record.FakeRecorder)
	}
	createCluster := func(name string) *databasev1alpha1.CubridCluster {
		cluster := standaloneCluster(name)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed())
		})
		return cluster
	}
	tokenSecretOf := func(name string) *corev1.Secret {
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-im-token", Namespace: testNamespace}, secret)).To(Succeed())
		return secret
	}
	// asOlderOperator leaves the Secret as an operator before #326 wrote it:
	// one token shared by every cluster, and no record of a rotation.
	asOlderOperator := func(name, shared string) {
		secret := tokenSecretOf(name)
		secret.Annotations = nil
		secret.Data = map[string][]byte{"token": []byte(shared)}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
	}
	// memberStartedAt creates the member Pod with its container running since
	// startedAt, or moves an existing one's start there, as a restart would.
	memberStartedAt := func(cluster *databasev1alpha1.CubridCluster, ordinal int, startedAt time.Time) {
		pod := &corev1.Pod{}
		key := types.NamespacedName{Name: fmt.Sprintf("%s-%d", cluster.Name, ordinal), Namespace: testNamespace}
		if err := k8sClient.Get(ctx, key, pod); err != nil {
			pod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labelsFor(cluster)},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: appName, Image: "registry.example/im:token"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod))).To(Succeed()) })
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: appName, Image: "registry.example/im:token",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(startedAt)}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	It("moves a cluster off the token an older operator shared, keeping the shared one for its running Pods", func() {
		cluster := createCluster("rotate-migrate")
		Expect(receivedEvent(reconcileAt(rotation.Add(-time.Hour), cluster.Name), "InstanceManagerTokenRotated")).
			To(BeFalse(), "a new cluster's token was rotated")
		asOlderOperator(cluster.Name, "shared-token")

		recorder := reconcileAt(rotation, cluster.Name)
		secret := tokenSecretOf(cluster.Name)
		current := string(secret.Data["token"])
		Expect(current).To(MatchRegexp("^[0-9a-f]{64}$"))
		Expect(string(secret.Data["previousToken"])).To(Equal("shared-token"),
			"the Pods that hold the shared token must stay reachable")
		Expect(receivedEvent(recorder, "InstanceManagerTokenRotated")).To(BeTrue())

		// No member was started with the new token yet: the overlap stays,
		// and the token is not rotated again.
		reconcileAt(rotation.Add(time.Hour), cluster.Name)
		secret = tokenSecretOf(cluster.Name)
		Expect(string(secret.Data["token"])).To(Equal(current))
		Expect(string(secret.Data["previousToken"])).To(Equal("shared-token"))
	})

	It("drops the previous token only when every member was started after the rotation", func() {
		cluster := createCluster("rotate-overlap")
		reconcileAt(rotation.Add(-time.Hour), cluster.Name)
		asOlderOperator(cluster.Name, "shared-token")
		memberStartedAt(cluster, 0, rotation.Add(-time.Minute))
		reconcileAt(rotation, cluster.Name)
		current := string(tokenSecretOf(cluster.Name).Data["token"])

		recorder := reconcileAt(rotation.Add(time.Hour), cluster.Name)
		Expect(string(tokenSecretOf(cluster.Name).Data["previousToken"])).To(Equal("shared-token"),
			"a member started before the rotation holds the previous token")
		Expect(receivedEvent(recorder, "InstanceManagerTokenOverlapEnded")).To(BeFalse())

		memberStartedAt(cluster, 0, rotation.Add(30*time.Second))
		recorder = reconcileAt(rotation.Add(time.Hour), cluster.Name)
		secret := tokenSecretOf(cluster.Name)
		Expect(secret.Data).NotTo(HaveKey("previousToken"))
		Expect(string(secret.Data["token"])).To(Equal(current))
		Expect(receivedEvent(recorder, "InstanceManagerTokenOverlapEnded")).To(BeTrue())
	})

	It("rotates once per request and starts a new request only after the overlap", func() {
		cluster := createCluster("rotate-request")
		reconcileAt(rotation.Add(-time.Hour), cluster.Name)
		first := string(tokenSecretOf(cluster.Name).Data["token"])
		request := func(value string) {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
			cluster.Annotations = map[string]string{"database.cubrid.io/rotate-im-token": value}
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		}

		request("2026-10-07")
		Expect(receivedEvent(reconcileAt(rotation, cluster.Name), "InstanceManagerTokenRotated")).To(BeTrue())
		secret := tokenSecretOf(cluster.Name)
		second := string(secret.Data["token"])
		Expect(second).NotTo(Equal(first))
		Expect(string(secret.Data["previousToken"])).To(Equal(first))

		reconcileAt(rotation.Add(time.Minute), cluster.Name)
		Expect(string(tokenSecretOf(cluster.Name).Data["token"])).To(Equal(second), "one request rotated twice")

		// A second rotation now would drop the token the running Pod holds.
		request("2026-10-08")
		reconcileAt(rotation.Add(2*time.Minute), cluster.Name)
		secret = tokenSecretOf(cluster.Name)
		Expect(string(secret.Data["token"])).To(Equal(second))
		Expect(string(secret.Data["previousToken"])).To(Equal(first))

		memberStartedAt(cluster, 0, rotation.Add(time.Minute))
		reconcileAt(rotation.Add(3*time.Minute), cluster.Name) // ends the overlap
		reconcileAt(rotation.Add(4*time.Minute), cluster.Name) // the waiting request
		secret = tokenSecretOf(cluster.Name)
		Expect(string(secret.Data["token"])).NotTo(Equal(second))
		Expect(string(secret.Data["previousToken"])).To(Equal(second))
	})
})
