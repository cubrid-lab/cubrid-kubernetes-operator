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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// An administrator asks for the rebuild of a member whose storage was lost by
// setting the annotation database.cubrid.io/rebuild-member to the member and
// the token the Operator published for it. A token is published only for a
// member running on a replacement claim that was never seen holding the
// database, beside a resolved primary; the request names the member and is
// accepted once, with an operation ID recorded in the status that an
// Operator restart keeps (#117). The Instance Managers' answers are made up;
// there is no kubelet, so the Pods and claims stand for what the API server
// holds, and nothing is restored.
var _ = Describe("Rebuild of a member's replacement storage on request (#117)", func() {
	ctx := context.Background()

	It("publishes a token for a replaced claim only, accepts the current token once, and keeps the operation across a restart", func() {
		const name = "rebuild-request"
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		m0, m1, m2 := name+"-0", name+"-1", name+"-2"
		// deleteClaim deletes a member's data claim. There is no controller
		// in envtest to remove the claim's protection finalizer once no Pod
		// uses it, so the test removes it.
		deleteClaim := func(member string) {
			GinkgoHelper()
			pvc := &corev1.PersistentVolumeClaim{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: dataVolume + "-" + member, Namespace: key.Namespace}, pvc)
			if apierrors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pvc))).To(Succeed())
			patch := client.MergeFrom(pvc.DeepCopy())
			pvc.Finalizers = nil
			Expect(client.IgnoreNotFound(k8sClient.Patch(ctx, pvc, patch))).To(Succeed())
		}
		createClaim := func(member, volume string) {
			GinkgoHelper()
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: dataVolume + "-" + member, Namespace: key.Namespace},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					VolumeName:  volume,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
				},
			}
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
			DeferCleanup(func() { deleteClaim(member) })
		}
		for _, m := range []string{m0, m1, m2} {
			createClaim(m, "pv-"+m)
			createMemberPod(ctx, key.Namespace, m, nil)
		}
		stored := func() *databasev1alpha1.CubridCluster {
			GinkgoHelper()
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			return c
		}
		// The HA bootstrap created the database on the first member and
		// seeded the other two.
		c := stored()
		c.Status.Databases = []databasev1alpha1.DatabaseStatus{{Name: testDatabase, Phase: databasePhaseCreated,
			PrimaryCreated: true, HAConfigured: true, SeededMembers: []string{m1, m2}}}
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())

		answers := &reachableReports{}
		answers.set(roleReports(name, [3]string{wqMaster, wqSlave, wqSlave}))
		clock := new(time.Now().Truncate(time.Second))
		recorder := record.NewFakeRecorder(200)
		prober := &HTTPRoleProber{Client: &http.Client{Transport: answers}, Tokens: staticToken("tok"),
			Now: func() time.Time { return *clock }}
		reconciler := func() *CubridClusterReconciler {
			return &CubridClusterReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
				Recorder: recorder, Prober: prober, Clock: func() time.Time { return *clock }}
		}
		pass := func(r *CubridClusterReconciler) {
			GinkgoHelper()
			*clock = clock.Add(unverifiedResyncInterval)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		annotate := func(value string) {
			GinkgoHelper()
			c := stored()
			patch := client.MergeFrom(c.DeepCopy())
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations[rebuildMemberAnnotation] = value
			Expect(k8sClient.Patch(ctx, c, patch)).To(Succeed())
		}
		annotated := func() bool {
			GinkgoHelper()
			_, ok := stored().Annotations[rebuildMemberAnnotation]
			return ok
		}
		replacePod := func(member string) {
			GinkgoHelper()
			deleteMemberPod(ctx, key.Namespace, member)
			createMemberPod(ctx, key.Namespace, member, nil)
		}

		By("every member runs the database on its claim: nothing is offered")
		r := reconciler()
		pass(r)
		c = stored()
		Expect(c.Status.MemberDataClaims).To(HaveLen(3))
		Expect(c.Status.MemberRebuildOffers).To(BeEmpty())

		By("a request for a member on the claim it ran the database on is refused")
		annotate(m2 + ":0123456789abcdef")
		pass(r)
		Expect(stored().Status.MemberRebuild).To(BeNil())
		Expect(drain(recorder)).To(ContainElement(SatisfyAll(
			ContainSubstring("Warning "+reasonMemberRebuildRefused), ContainSubstring("not shown expendable"))))
		Expect(annotated()).To(BeTrue(), "a refused annotation is left for the administrator")

		By("a new claim bound to the retained volume the member ran the database on is not offered")
		answers.set(roleReports(name, [3]string{wqMaster, wqSlave, ""}))
		deleteMemberPod(ctx, key.Namespace, m2)
		deleteClaim(m2)
		createClaim(m2, "pv-"+m2)
		createMemberPod(ctx, key.Namespace, m2, nil)
		pass(r)
		Expect(stored().Status.MemberRebuildOffers).To(BeEmpty())

		By("the administrator replaces the third member's claim with an empty one; its new Pod gives no role")
		deleteMemberPod(ctx, key.Namespace, m2)
		deleteClaim(m2)
		createClaim(m2, "pv-"+m2+"-replacement")
		createMemberPod(ctx, key.Namespace, m2, nil)
		pass(r)
		offers := stored().Status.MemberRebuildOffers
		Expect(offers).To(HaveLen(1))
		Expect(offers[0].Member).To(Equal(m2))
		Expect(offers[0].Source).To(Equal(m0))
		Expect(offers[0].Token).NotTo(BeEmpty())

		By("an Operator restart publishes the same token")
		r = reconciler()
		pass(r)
		Expect(stored().Status.MemberRebuildOffers).To(Equal(offers))

		By("a request naming the current primary is refused")
		drain(recorder)
		annotate(m0 + ":" + offers[0].Token)
		pass(r)
		Expect(stored().Status.MemberRebuild).To(BeNil())
		Expect(drain(recorder)).To(ContainElement(SatisfyAll(
			ContainSubstring("Warning "+reasonMemberRebuildRefused), ContainSubstring("current primary"))))

		By("the token published before the member's Pod changed is inert")
		annotate(m2 + ":" + offers[0].Token)
		replacePod(m2)
		pass(r)
		c = stored()
		Expect(c.Status.MemberRebuild).To(BeNil())
		Expect(drain(recorder)).To(ContainElement(SatisfyAll(
			ContainSubstring("Warning "+reasonMemberRebuildIgnored), ContainSubstring("since the token was published"))))
		Expect(c.Status.MemberRebuildOffers).To(HaveLen(1))
		token := c.Status.MemberRebuildOffers[0].Token
		Expect(token).NotTo(Equal(offers[0].Token), "a new Pod gets a new token")

		By("a pass whose status write fails after accepting records nothing: the token stays current")
		annotate(m2 + ":" + token)
		drain(recorder)
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		failing := reconciler()
		failing.Client = interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*databasev1alpha1.CubridCluster); ok && subResource == statusSubresource {
					return apierrors.NewServiceUnavailable("the status write fails in this test")
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
		})
		*clock = clock.Add(unverifiedResyncInterval)
		_, err = failing.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred())
		c = stored()
		Expect(c.Status.MemberRebuild).To(BeNil(), "no accepted request is stored")
		Expect(c.Status.MemberRebuildOffers).To(HaveLen(1))
		Expect(c.Status.MemberRebuildOffers[0].Token).To(Equal(token), "the token is not consumed")
		Expect(annotated()).To(BeTrue(), "the annotation is left for the next pass")
		Expect(drain(recorder)).NotTo(ContainElement(ContainSubstring(reasonMemberRebuildAccepted)),
			"no acceptance is reported for a status that was not written")

		By("the current token is accepted and its operation recorded before anything is restored")
		pass(r)
		c = stored()
		rebuild := c.Status.MemberRebuild
		Expect(rebuild).NotTo(BeNil())
		Expect(rebuild.Member).To(Equal(m2))
		Expect(rebuild.Source).To(Equal(m0))
		Expect(rebuild.Token).To(Equal(token))
		Expect(rebuild.State).To(Equal(databasev1alpha1.MemberRebuildAccepted))
		Expect(rebuild.OperationID).NotTo(BeEmpty())
		Expect(c.Status.MemberRebuildOffers).To(BeEmpty(), "one member is rebuilt at a time")
		Expect(drain(recorder)).To(ContainElement(ContainSubstring("Normal " + reasonMemberRebuildAccepted)))

		By("after an Operator restart the consumed annotation is removed and the operation is kept")
		r = reconciler()
		pass(r)
		Expect(annotated()).To(BeFalse())
		Expect(stored().Status.MemberRebuild).To(Equal(rebuild))

		By("a replayed token is removed again and changes nothing")
		annotate(m2 + ":" + token)
		pass(r)
		Expect(annotated()).To(BeFalse())
		Expect(stored().Status.MemberRebuild).To(Equal(rebuild))
		Expect(drain(recorder)).NotTo(ContainElement(ContainSubstring(reasonMemberRebuildAccepted)))
	})
})
