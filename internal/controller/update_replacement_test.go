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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

var _ = Describe("Replacement of outdated members (ADR-0009, #196)", func() {
	const (
		replaceNamespace = "default"
		oldRevision      = "rev-old"
		newRevision      = "rev-new"
	)
	ctx := context.Background()

	// outdatedCluster lays out the state in which the planner chooses a slave
	// to replace: a resolved master, two ready slaves, every Pod on the old
	// revision.
	outdatedCluster := func(name string) (*databasev1alpha1.CubridCluster, *appsv1.StatefulSet) {
		cluster := haCluster(name)
		members := memberNames(cluster, 3)
		for i, member := range members {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: member, Namespace: replaceNamespace,
					Labels: map[string]string{"controller-revision-hash": oldRevision},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "cubrid", Image: "registry.example/im:old"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod))).To(Succeed()) })
			role := databasev1alpha1.RoleSlave
			if i == 0 {
				role = databasev1alpha1.RoleMaster
			}
			cluster.Status.Instances = append(cluster.Status.Instances, databasev1alpha1.InstanceStatus{
				Name: member, Ordinal: int32(i), Role: role, Ready: true, //nolint:gosec // a small test index
			})
		}
		sts := &appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: newRevision, CurrentRevision: oldRevision}}
		return cluster, sts
	}
	podsLeft := func(cluster *databasev1alpha1.CubridCluster) []string {
		var left []string
		for _, member := range memberNames(cluster, 3) {
			pod := &corev1.Pod{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: member, Namespace: replaceNamespace}, pod)
			if err == nil && pod.DeletionTimestamp == nil {
				left = append(left, member)
			}
		}
		return left
	}
	resolved := PrimaryResolution{Status: metav1.ConditionTrue, Reason: singlePrimary}

	It("does not delete a Pod by itself: the gates that would make it safe are not complete", func() {
		cluster, sts := outdatedCluster("replace-off")
		resolved.CurrentPrimary = "replace-off-0"
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20)}

		r.reconcileRollingUpdate(ctx, cluster, sts, resolved)

		Expect(podsLeft(cluster)).To(HaveLen(3), "an outdated slave was deleted")
		cond := meta.FindStatusCondition(cluster.Status.Conditions, conditionUpdating)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("AutomaticReplacementDisabled"))
		Expect(cond.Message).To(ContainSubstring("replace-off-1"), "the condition names the member that would be replaced")
		Expect(cluster.Status.Update).NotTo(BeNil())
		Expect(cluster.Status.Update.DesiredRevision).To(Equal(newRevision))
	})

	It("keeps the replacement itself working for when it is switched on (#96)", func() {
		cluster, sts := outdatedCluster("replace-on")
		resolved.CurrentPrimary = "replace-on-0"
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			AutomaticReplacement: true,
		}

		r.reconcileRollingUpdate(ctx, cluster, sts, resolved)

		Expect(podsLeft(cluster)).To(ConsistOf("replace-on-0", "replace-on-2"), "one slave, never the master")
	})
})
