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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// Every member Pod, HA or standalone, carries exactly the three keepalive
// sysctls of #982 with the values tested on the VM lab, and nothing else: a
// sysctl outside the safe set would make the kubelet or Pod Security refuse
// the Pod.
func TestPodSpecSetsExactlyTheMemberKeepaliveSysctls(t *testing.T) {
	want := []corev1.Sysctl{
		{Name: "net.ipv4.tcp_keepalive_time", Value: "30"},
		{Name: "net.ipv4.tcp_keepalive_intvl", Value: "10"},
		{Name: "net.ipv4.tcp_keepalive_probes", Value: "3"},
	}
	r := &CubridClusterReconciler{}
	for _, cluster := range []*databasev1alpha1.CubridCluster{haCluster("keepalive"), standaloneCluster("keepalive")} {
		spec := r.podSpec(cluster, "registry.example/cubrid:test")
		if spec.SecurityContext == nil || !equality.Semantic.DeepEqual(spec.SecurityContext.Sysctls, want) {
			t.Errorf("%d-member cluster: Pod sysctls = %+v, want %+v",
				cluster.Spec.Topology.PromotableMembers, spec.SecurityContext, want)
		}
	}
}

// The Broker Pods are left as they were: the server's end of each Broker
// connection is in a member Pod, which already has the short keepalive.
func TestBrokerPodSpecSetsNoSysctls(t *testing.T) {
	spec := (&CubridClusterReconciler{}).brokerPodSpec(brokerTestCluster(), brokerModeRW, "registry.example/cubrid:test")
	if spec.SecurityContext != nil && len(spec.SecurityContext.Sysctls) != 0 {
		t.Errorf("Broker Pod sysctls = %+v, want none", spec.SecurityContext.Sysctls)
	}
}

// The API server admits a member Pod, built from the StatefulSet the
// Operator writes, in a namespace that enforces Pod Security "restricted":
// the keepalive sysctls are in the policy's allowed set (#982). A copy that
// also sets net.ipv4.tcp_retries2, which is not a safe sysctl, is refused,
// which shows the policy checks the Pod's sysctls in the test environment.
var _ = Describe("Member Pod keepalive sysctls (#982)", func() {
	ctx := context.Background()
	const (
		namespace = "keepalive-restricted"
		// The ServiceAccount a Pod gets when it names none.
		defaultServiceAccount = "default"
	)
	key := types.NamespacedName{Name: "keepalive", Namespace: namespace}

	It("are admitted by Pod Security restricted", func() {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
			Labels: map[string]string{
				"pod-security.kubernetes.io/enforce":         "restricted",
				"pod-security.kubernetes.io/enforce-version": "latest",
			},
		}})).To(Succeed())
		// The test environment runs no controller that creates it.
		Expect(k8sClient.Create(ctx, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: defaultServiceAccount, Namespace: namespace},
		})).To(Succeed())

		cluster := haCluster(key.Name)
		cluster.Namespace = namespace
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(50)}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.SecurityContext.Sysctls).To(Equal(memberSysctls()))

		// The Pod the StatefulSet controller would create for member 0: the
		// template plus a claim for each volume claim template.
		memberPod := func(name string) *corev1.Pod {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: sts.Spec.Template.Labels},
				Spec:       *sts.Spec.Template.Spec.DeepCopy(),
			}
			for _, claim := range sts.Spec.VolumeClaimTemplates {
				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
					Name: claim.Name,
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: claim.Name + "-" + sts.Name + "-0",
					}},
				})
			}
			return pod
		}

		By("creating a member Pod with the keepalive sysctls")
		pod := memberPod(sts.Name + "-0")
		Expect(pod.Spec.SecurityContext.Sysctls).To(Equal(memberSysctls()))
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, pod)).To(Succeed()) })

		By("checking that a copy with a sysctl outside the safe set is refused")
		unsafe := memberPod(sts.Name + "-unsafe-sysctl")
		unsafe.Spec.SecurityContext.Sysctls = append(unsafe.Spec.SecurityContext.Sysctls,
			corev1.Sysctl{Name: "net.ipv4.tcp_retries2", Value: "5"})
		err = k8sClient.Create(ctx, unsafe)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("violates PodSecurity"))
	})
})
