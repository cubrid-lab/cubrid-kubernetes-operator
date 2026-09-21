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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

var _ = Describe("CubridBackup Controller", func() {
	const namespace = "default"

	ctx := context.Background()

	newBackup := func(name, cluster string) *databasev1alpha1.CubridBackup {
		return &databasev1alpha1.CubridBackup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasev1alpha1.CubridBackupSpec{
				ClusterRef: databasev1alpha1.LocalObjectRef{Name: cluster},
				Database:   "demodb",
				Level:      databasev1alpha1.CubridBackupFull,
				Destination: databasev1alpha1.CubridBackupDestination{
					Type: databasev1alpha1.DestinationObjectStorage,
					ObjectStorage: &databasev1alpha1.ObjectStorageDestination{
						Bucket:         "cubrid-backups",
						CredentialsRef: databasev1alpha1.LocalObjectRef{Name: "s3-credentials"},
					},
				},
			},
		}
	}

	reconcileOnce := func(name string) {
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	It("accepts a backup whose cluster exists but holds it Pending (workflow not wired)", func() {
		cluster := &databasev1alpha1.CubridCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-cluster", Namespace: namespace},
			Spec: databasev1alpha1.CubridClusterSpec{
				Version:   "11.4",
				Topology:  databasev1alpha1.CubridTopology{PromotableMembers: 1},
				Databases: []databasev1alpha1.CubridDatabase{{Name: "demodb"}},
				Storage: databasev1alpha1.CubridStorage{
					Data: databasev1alpha1.CubridStorageSpec{Size: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

		backup := newBackup("bk-accepted", "bk-cluster")
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

		reconcileOnce("bk-accepted")

		updated := &databasev1alpha1.CubridBackup{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))

		accepted := meta.FindStatusCondition(updated.Status.Conditions, conditionAccepted)
		Expect(accepted).NotTo(BeNil())
		Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		ready := meta.FindStatusCondition(updated.Status.Conditions, conditionBackupReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("BackupWorkflowNotImplemented"))
	})

	It("fails a backup whose referenced cluster does not exist", func() {
		backup := newBackup("bk-nocluster", "missing-cluster")
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

		reconcileOnce("bk-nocluster")

		updated := &databasev1alpha1.CubridBackup{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))

		accepted := meta.FindStatusCondition(updated.Status.Conditions, conditionAccepted)
		Expect(accepted).NotTo(BeNil())
		Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
		Expect(accepted.Reason).To(Equal("ClusterNotFound"))
	})
})
