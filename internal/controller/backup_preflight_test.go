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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// createMemberPod creates a member Pod as the StatefulSet would, with env as
// the environment its container started with. There is no kubelet in
// envtest, so the Pod only stands for what the API server holds.
func createMemberPod(ctx context.Context, namespace, name string, env []corev1.EnvVar) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: appName, Image: "registry.example/cubrid:test", Env: env,
		}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	DeferCleanup(func() { deleteMemberPod(ctx, namespace, name) })
}

// deleteMemberPod deletes a member Pod at once, as a restart would replace it.
func deleteMemberPod(ctx context.Context, namespace, name string) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
}

// createAppliedMemberPods creates every member Pod of cluster with the
// object-storage settings of the cluster applied.
func createAppliedMemberPods(ctx context.Context, cluster *databasev1alpha1.CubridCluster) {
	for _, member := range memberNames(cluster, cluster.Spec.Topology.PromotableMembers) {
		createMemberPod(ctx, cluster.Namespace, member, objectStorageEnv(cluster.Spec.ObjectStorage))
	}
}

// #290: what a backup cannot do is found before anything runs on a member.
var _ = Describe("CubridBackup preflight", func() {
	const namespace = "default"
	ctx := context.Background()

	singleCluster := func(name string) *databasev1alpha1.CubridCluster {
		cluster := &databasev1alpha1.CubridCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasev1alpha1.CubridClusterSpec{
				Version:       cubridVersion,
				Topology:      databasev1alpha1.CubridTopology{PromotableMembers: 1},
				Databases:     []databasev1alpha1.CubridDatabase{{Name: demoDB}},
				ObjectStorage: testObjectStorage(),
				Storage: databasev1alpha1.CubridStorage{
					Data: databasev1alpha1.CubridStorageSpec{Size: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		return cluster
	}
	createBackup := func(name, cluster string, destination databasev1alpha1.CubridBackupDestination) *databasev1alpha1.CubridBackup {
		backup := &databasev1alpha1.CubridBackup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasev1alpha1.CubridBackupSpec{
				ClusterRef:  databasev1alpha1.LocalObjectRef{Name: cluster},
				Database:    demoDB,
				Level:       databasev1alpha1.CubridBackupFull,
				Destination: destination,
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
		return backup
	}
	toObjectStorage := databasev1alpha1.CubridBackupDestination{
		Type:          databasev1alpha1.DestinationObjectStorage,
		ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: "preflight-backups"},
	}
	reconcileBackup := func(r *CubridBackupReconciler, backup *databasev1alpha1.CubridBackup) *databasev1alpha1.CubridBackup {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backup)})
		Expect(err).NotTo(HaveOccurred())
		got := &databasev1alpha1.CubridBackup{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), got)).To(Succeed())
		return got
	}
	reasonOf := func(backup *databasev1alpha1.CubridBackup, conditionType string) string {
		cond := meta.FindStatusCondition(backup.Status.Conditions, conditionType)
		Expect(cond).NotTo(BeNil(), conditionType)
		return cond.Reason
	}

	It("refuses a PVC destination, which has no execution path, without calling a member", func() {
		cluster := singleCluster("bk-pvc")
		createAppliedMemberPods(ctx, cluster)
		backup := createBackup("bk-pvc", "bk-pvc", databasev1alpha1.CubridBackupDestination{
			Type: databasev1alpha1.DestinationPVC,
		})
		fake := &fakeBackupClient{}
		r := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Prober: &memberProber{master: "bk-pvc-0"}, Backup: fake,
		}

		got := reconcileBackup(r, backup)
		Expect(fake.started).To(BeFalse(), "a backup to a PVC was dispatched to a member")
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		Expect(got.Status.CompletedAt).NotTo(BeNil())
		Expect(got.Status.TargetInstance).To(BeEmpty())
		Expect(reasonOf(got, conditionAccepted)).To(Equal("UnsupportedDestination"))
		Expect(reasonOf(got, conditionBackupReady)).To(Equal("UnsupportedDestination"))
	})

	It("waits for the selected Pod to pick up the object-storage settings, and goes on once it has", func() {
		cluster := singleCluster("bk-unapplied")
		const member = "bk-unapplied-0"
		By("a Pod that was started before the cluster had object storage")
		createMemberPod(ctx, namespace, member, nil)
		backup := createBackup("bk-unapplied", "bk-unapplied", toObjectStorage)
		fake := &fakeBackupClient{}
		r := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Prober: &memberProber{master: member}, Backup: fake,
		}

		got := reconcileBackup(r, backup)
		Expect(fake.started).To(BeFalse(), "a backup was dispatched to a Pod without object storage")
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending), "a backup that waits is not failed")
		Expect(got.Status.CompletedAt).To(BeNil())
		Expect(got.Status.TargetInstance).To(BeEmpty(), "a target was recorded that nothing was started on")
		Expect(reasonOf(got, conditionAccepted)).To(Equal("BackupAccepted"))
		Expect(reasonOf(got, conditionBackupReady)).To(Equal("ObjectStorageNotApplied"))

		By("a Pod that still has the endpoint the cluster had before")
		deleteMemberPod(ctx, namespace, member)
		stale := testObjectStorage()
		stale.Endpoint = "old-minio.storage.svc:9000"
		createMemberPod(ctx, namespace, member, objectStorageEnv(stale))
		got = reconcileBackup(r, backup)
		Expect(fake.started).To(BeFalse(), "a backup was dispatched to a Pod with other object-storage settings")
		Expect(reasonOf(got, conditionBackupReady)).To(Equal("ObjectStorageNotApplied"))

		By("the Pod restarted with the cluster's settings")
		deleteMemberPod(ctx, namespace, member)
		createMemberPod(ctx, namespace, member, objectStorageEnv(cluster.Spec.ObjectStorage))
		got = reconcileBackup(r, backup)
		Expect(fake.started).To(BeTrue())
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
		Expect(got.Status.TargetInstance).To(Equal(member))
	})

	// A recorded target may run the backup already: the earlier dispatch may
	// have been accepted and only its response lost. The request is sent to
	// it again, which returns the existing operation; its Pod's environment
	// is not judged, and the manager refuses with 412 if it has no store.
	It("sends the request again to a recorded target whatever its Pod's environment", func() {
		cluster := singleCluster("bk-recorded")
		const member = "bk-recorded-0"
		createMemberPod(ctx, namespace, member, nil)
		backup := createBackup("bk-recorded", "bk-recorded", toObjectStorage)
		backup.Status.Phase = databasev1alpha1.BackupPhasePending
		backup.Status.TargetInstance = member
		backup.Status.TargetRole = string(databasev1alpha1.RoleMaster)
		backup.Status.ClusterUID = cluster.UID
		Expect(k8sClient.Status().Update(ctx, backup)).To(Succeed())

		fake := &lostResponseBackupClient{}
		r := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Prober: &memberProber{master: member}, Backup: fake,
		}
		got := reconcileBackup(r, backup)
		Expect(fake.instances).To(Equal([]string{member}), "the recorded target was not asked again")
		Expect(got.Status.TargetInstance).To(Equal(member))
		Expect(got.Status.OperationRef).To(Equal(opFake))
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
	})

	It("refuses a PVC destination at once, also while its cluster is not found", func() {
		backup := createBackup("bk-pvc-nocluster", "bk-pvc-nocluster", databasev1alpha1.CubridBackupDestination{
			Type: databasev1alpha1.DestinationPVC,
		})
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		got := reconcileBackup(r, backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		Expect(reasonOf(got, conditionAccepted)).To(Equal("UnsupportedDestination"))
		Expect(reasonOf(got, conditionBackupReady)).To(Equal("UnsupportedDestination"))
	})

	// A PVC backup that an earlier operator dispatched is followed to the end
	// of its operation, not ended while backupdb may run.
	It("polls a PVC backup whose operation was started instead of refusing it", func() {
		cluster := singleCluster("bk-pvc-started")
		const member = "bk-pvc-started-0"
		backup := createBackup("bk-pvc-started", "bk-pvc-started", databasev1alpha1.CubridBackupDestination{
			Type: databasev1alpha1.DestinationPVC,
		})
		backup.Status.Phase = databasev1alpha1.BackupPhaseRunning
		backup.Status.TargetInstance = member
		backup.Status.TargetRole = string(databasev1alpha1.RoleMaster)
		backup.Status.ClusterUID = cluster.UID
		backup.Status.OperationRef = opFake
		Expect(k8sClient.Status().Update(ctx, backup)).To(Succeed())

		fake := &lostResponseBackupClient{}
		r := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Prober: &memberProber{master: member}, Backup: fake,
		}
		got := reconcileBackup(r, backup)
		Expect(fake.polls).To(Equal(1), "the started operation was not polled")
		Expect(fake.instances).To(BeEmpty())
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
		Expect(reasonOf(got, conditionBackupReady)).To(Equal("BackupRunning"))
	})
})

// A member that answers that it has no object storage is told apart from an
// unavailable one (#290).
func TestHTTPBackupClient_ObjectStorageNotAppliedIsNamed(t *testing.T) {
	c := NewHTTPBackupClient(staticToken("tok"))
	c.Client = &http.Client{Transport: answerStatus(http.StatusPreconditionFailed)}
	_, err := c.StartBackup(context.Background(), "db-0", "ns", "k", instancemanager.BackupRequest{})
	if err == nil {
		t.Fatal("a refused backup start returned no error")
	}
	if got := instanceManagerFailureReason(err); got != "ObjectStorageNotApplied" {
		t.Errorf("reason = %q (err %v), want ObjectStorageNotApplied", got, err)
	}
}
