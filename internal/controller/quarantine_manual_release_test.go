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
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// An administrator releases a write quarantine by setting the annotation
// database.cubrid.io/release-write-quarantine to the stored quarantine's
// current release token. The Operator accepts it only beside exactly one
// active master, records the release before the read-write Brokers start, and
// never accepts the same token twice (ADR-0005, section 8: "Manual: risk
// acceptance, not a verified fence"; #113). The Instance Managers' answers are
// made up; the engine's behavior is not reproduced.
var _ = Describe("Manual release of a write quarantine (#113)", func() {
	ctx := context.Background()

	type fixture struct {
		key, rw  types.NamespacedName
		answers  *reachableReports
		clock    *time.Time
		prober   *HTTPRoleProber
		recorder *record.FakeRecorder
	}
	newFixture := func(name string) fixture {
		GinkgoHelper()
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		f := fixture{key: key, answers: &reachableReports{}, clock: new(time.Now().Truncate(time.Second)),
			rw:       types.NamespacedName{Name: brokerDeploymentName(name, brokerModeRW), Namespace: key.Namespace},
			recorder: record.NewFakeRecorder(200)}
		f.answers.set(roleReports(name, [3]string{wqMaster, wqSlave, wqSlave}))
		f.prober = &HTTPRoleProber{Client: &http.Client{Transport: f.answers}, Tokens: staticToken("tok"),
			Now: func() time.Time { return *f.clock }}
		return f
	}
	reconcilerFor := func(f fixture, c client.Client) *CubridClusterReconciler {
		return &CubridClusterReconciler{Client: c, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: f.recorder, Prober: f.prober, Clock: func() time.Time { return *f.clock }}
	}
	// pass advances the clock by 3 seconds, the cadence while a quarantine
	// is in force, and reconciles once.
	pass := func(r *CubridClusterReconciler, f fixture) {
		GinkgoHelper()
		*f.clock = f.clock.Add(unverifiedResyncInterval)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(err).NotTo(HaveOccurred())
	}
	stored := func(f fixture) *databasev1alpha1.CubridCluster {
		GinkgoHelper()
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, c)).To(Succeed())
		return c
	}
	rwReplicas := func(f fixture) int32 {
		GinkgoHelper()
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, f.rw, dep)).To(Succeed())
		return *dep.Spec.Replicas
	}
	annotate := func(f fixture, value string) {
		GinkgoHelper()
		c := stored(f)
		patch := client.MergeFrom(c.DeepCopy())
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations[releaseWriteQuarantineAnnotation] = value
		Expect(k8sClient.Patch(ctx, c, patch)).To(Succeed())
	}
	annotation := func(f fixture) (string, bool) {
		GinkgoHelper()
		v, ok := stored(f).Annotations[releaseWriteQuarantineAnnotation]
		return v, ok
	}
	// running runs the cluster with the first member as the only master until
	// every member is verified and the read-write Brokers are asked for.
	running := func(r *CubridClusterReconciler, f fixture) {
		GinkgoHelper()
		pass(r, f)
		pass(r, f)
		Expect(rwReplicas(f)).To(Equal(int32(brokerReplicas)))
		Expect(stored(f).Status.LastResolvedPrimary).To(Equal(f.key.Name + "-0"))
	}
	// twoMasters quarantines the cluster with the first two members as
	// active masters, and returns the stored quarantine.
	twoMasters := func(r *CubridClusterReconciler, f fixture) *databasev1alpha1.WriteQuarantine {
		GinkgoHelper()
		f.answers.set(roleReports(f.key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		pass(r, f)
		q := stored(f).Status.WriteQuarantine
		Expect(q).NotTo(BeNil())
		Expect(q.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMultipleMasters))
		Expect(rwReplicas(f)).To(Equal(int32(0)))
		return q
	}
	// secondFenced is the cluster after the administrator fenced the second
	// member: the first one is the only active master, and the second one,
	// which ranks below it and was not the last resolved primary, does not
	// answer. No rule quarantines the path again.
	secondFenced := func(f fixture) {
		f.answers.set(roleReports(f.key.Name, [3]string{wqMaster, "", wqSlave}))
	}

	It("releases on the current token: the status first, then the read-write Brokers, and the annotation is removed", func() {
		f := newFixture("wq-manual")
		r := reconcilerFor(f, k8sClient)
		running(r, f)
		q := twoMasters(r, f)
		drain(f.recorder)

		By("the administrator fences the second member and sets the current token")
		secondFenced(f)
		annotate(f, q.ReleaseToken)
		pass(r, f)
		c := stored(f)
		Expect(c.Status.WriteQuarantine).To(BeNil())
		rel := c.Status.LastWriteQuarantineRelease
		Expect(rel).NotTo(BeNil())
		Expect(rel.Method).To(Equal(databasev1alpha1.WriteQuarantineReleasedManually))
		Expect(rel.ReleaseToken).To(Equal(q.ReleaseToken))
		Expect(rel.GenerationID).To(Equal(q.GenerationID))
		Expect(rel.Revision).To(Equal(q.Revision))
		Expect(rel.ObservedMaster).To(Equal(f.key.Name + "-0"))
		Expect(memberNamesOf(rel.Members)).To(Equal([]string{f.key.Name + "-0", f.key.Name + "-1"}))
		Expect(rel.Warning).To(ContainSubstring("acknowledged writes may be lost"))
		stamp, err := rel.ReleaseTime.MarshalJSON()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(stamp)).To(HaveSuffix(`Z"`), "the release time is stored in UTC")
		Expect(rwReplicas(f)).To(Equal(int32(0)), "the release is written before the read-write Brokers start")
		_, present := annotation(f)
		Expect(present).To(BeFalse(), "the consumed annotation is removed")
		Expect(drain(f.recorder)).To(ContainElement(SatisfyAll(
			ContainSubstring("Warning "+reasonWriteQuarantineReleasedManually),
			ContainSubstring(q.ReleaseToken[:8]),
			ContainSubstring(f.key.Name+"-1"),
			ContainSubstring("acknowledged writes may be lost"))))

		By("the pass after the release")
		pass(r, f)
		Expect(stored(f).Status.WriteQuarantine).To(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(brokerReplicas)))
	})

	It("refuses a release beside no active master or two, and keeps the path closed", func() {
		f := newFixture("wq-manual-refused")
		r := reconcilerFor(f, k8sClient)
		running(r, f)
		q := twoMasters(r, f)
		drain(f.recorder)

		By("the current token while both masters still claim the role")
		annotate(f, q.ReleaseToken)
		pass(r, f)
		c := stored(f)
		Expect(c.Status.WriteQuarantine).NotTo(BeNil())
		Expect(c.Status.WriteQuarantine.ReleaseRefusal).To(ContainSubstring("2 members are active masters"))
		Expect(rwReplicas(f)).To(Equal(int32(0)))
		Expect(drain(f.recorder)).To(ContainElement(ContainSubstring("Warning " + reasonWriteQuarantineReleaseRefused)))
		v, _ := annotation(f)
		Expect(v).To(Equal(q.ReleaseToken), "a refused annotation is left for the administrator")

		By("no member answering as an active master")
		f.answers.set(roleReports(f.key.Name, [3]string{"", "", wqSlave}))
		pass(r, f)
		pass(r, f)
		c = stored(f)
		Expect(c.Status.WriteQuarantine).NotTo(BeNil())
		Expect(c.Status.WriteQuarantine.ReleaseRefusal).To(ContainSubstring("no member is an active master"))
		Expect(rwReplicas(f)).To(Equal(int32(0)))
		Expect(meta.FindStatusCondition(c.Status.Conditions, conditionRoutingReady).Message).To(ContainSubstring("no member is an active master"))
		Expect(drain(f.recorder)).To(HaveLen(1), "the refusal is reported once per reason")

		By("one active master again: the same token is accepted")
		secondFenced(f)
		pass(r, f)
		Expect(stored(f).Status.WriteQuarantine).To(BeNil())
	})

	It("makes an annotation set before the member set expanded inert, and accepts the new token", func() {
		f := newFixture("wq-manual-expanded")
		r := reconcilerFor(f, k8sClient)
		running(r, f)

		By("the first member cut off beside the second one as master")
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		pass(r, f)
		first := stored(f).Status.WriteQuarantine
		Expect(first).NotTo(BeNil())
		Expect(first.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMemberUnverified))
		drain(f.recorder)

		By("the token is set, and in the same observation the third member claims master too")
		annotate(f, first.ReleaseToken)
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqMaster}))
		pass(r, f)
		revised := stored(f).Status.WriteQuarantine
		Expect(revised).NotTo(BeNil())
		Expect(revised.Revision).To(Equal(first.Revision + 1))
		Expect(revised.ReleaseToken).NotTo(Equal(first.ReleaseToken))
		Expect(revised.GenerationID).To(Equal(first.GenerationID))
		Expect(drain(f.recorder)).To(ContainElement(ContainSubstring("Warning " + reasonWriteQuarantineReleaseIgnored)))

		By("one active master again: the old token stays inert, reported once")
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		pass(r, f)
		pass(r, f)
		Expect(stored(f).Status.WriteQuarantine).NotTo(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(0)))
		Expect(drain(f.recorder)).NotTo(ContainElement(ContainSubstring(reasonWriteQuarantineReleaseIgnored)))

		By("the new token, after the revised set was reviewed")
		annotate(f, revised.ReleaseToken)
		pass(r, f)
		rel := stored(f).Status.LastWriteQuarantineRelease
		Expect(rel).NotTo(BeNil())
		Expect(rel.ReleaseToken).To(Equal(revised.ReleaseToken))
		Expect(rel.Revision).To(Equal(revised.Revision))
		Expect(memberNamesOf(rel.Members)).To(Equal([]string{f.key.Name + "-0", f.key.Name + "-2"}))

		By("the next pass: the first member still outranks the master and is unverified, so a new quarantine starts")
		pass(r, f)
		next := stored(f).Status.WriteQuarantine
		Expect(next).NotTo(BeNil())
		Expect(next.GenerationID).NotTo(Equal(revised.GenerationID))
		Expect(rwReplicas(f)).To(Equal(int32(0)))
	})

	It("ignores a consumed token whose annotation could not be removed, also after a restart", func() {
		f := newFixture("wq-manual-consumed")
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		patches := 0
		noPatch := interceptor.NewClient(base, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption) error {
				// The annotation removal is the only JSON patch of a cluster.
				if _, ok := obj.(*databasev1alpha1.CubridCluster); ok && patch.Type() == types.JSONPatchType {
					patches++
					return apierrors.NewServiceUnavailable("the API server is not available")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		r := reconcilerFor(f, noPatch)
		running(r, f)
		q := twoMasters(r, f)

		By("a release whose annotation removal fails")
		secondFenced(f)
		annotate(f, q.ReleaseToken)
		pass(r, f)
		Expect(stored(f).Status.WriteQuarantine).To(BeNil())
		Expect(patches).To(BeNumerically(">", 0))
		v, _ := annotation(f)
		Expect(v).To(Equal(q.ReleaseToken))

		By("both members active masters again: a new quarantine, which the consumed token does not release")
		f.answers.set(roleReports(f.key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		pass(r, f)
		again := stored(f).Status.WriteQuarantine
		Expect(again).NotTo(BeNil())
		Expect(again.GenerationID).NotTo(Equal(q.GenerationID))
		secondFenced(f)
		pass(r, f)
		Expect(stored(f).Status.WriteQuarantine).NotTo(BeNil(), "the consumed token never works again")
		Expect(stored(f).Status.LastWriteQuarantineRelease.ReleaseToken).To(Equal(q.ReleaseToken))
		Expect(rwReplicas(f)).To(Equal(int32(0)))

		By("a restarted Operator with no memory, whose patches succeed")
		restarted := reconcilerFor(f, k8sClient)
		pass(restarted, f)
		Expect(stored(f).Status.WriteQuarantine).NotTo(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(0)))
		_, present := annotation(f)
		Expect(present).To(BeFalse(), "the consumed annotation is removed")

		By("the restarted Operator accepts the new quarantine's current token")
		annotate(f, stored(f).Status.WriteQuarantine.ReleaseToken)
		pass(restarted, f)
		Expect(stored(f).Status.WriteQuarantine).To(BeNil())
		Expect(stored(f).Status.LastWriteQuarantineRelease.GenerationID).To(Equal(again.GenerationID))
	})

	It("compares the token with the stored status, not with a cache that is behind", func() {
		f := newFixture("wq-manual-stale")
		r := reconcilerFor(f, k8sClient)
		running(r, f)
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		pass(r, f)
		behind := stored(f)
		first := behind.Status.WriteQuarantine
		Expect(first).NotTo(BeNil())
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqMaster}))
		pass(r, f)
		current := stored(f).Status.WriteQuarantine
		Expect(current.ReleaseToken).NotTo(Equal(first.ReleaseToken))
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))

		// A cache that still holds the first revision, with the annotation
		// the stored cluster carries.
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		stale := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if cluster, ok := obj.(*databasev1alpha1.CubridCluster); ok && key == f.key {
					now := &databasev1alpha1.CubridCluster{}
					if err := c.Get(ctx, key, now, opts...); err != nil {
						return err
					}
					behind.DeepCopyInto(cluster)
					cluster.Annotations = now.Annotations
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		lagging := reconcilerFor(f, stale)

		By("the token of the cached, earlier revision")
		annotate(f, first.ReleaseToken)
		pass(lagging, f)
		Expect(stored(f).Status.WriteQuarantine).NotTo(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(0)))

		By("the stored token, while this pass decided from the earlier revision")
		annotate(f, current.ReleaseToken)
		pass(lagging, f)
		Expect(stored(f).Status.WriteQuarantine).NotTo(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(0)))
		Expect(stored(f).Status.LastWriteQuarantineRelease).To(BeNil())
	})
})
