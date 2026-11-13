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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// claimSplitImage is the image of the clusters of these tests.
const claimSplitImage = "registry.example/cubrid-instance-manager:claimsplit"

// envtest runs no scheduler, no StatefulSet controller and no volume
// provisioner, so these tests write the member's Pod, its status and both of
// its claims by hand, in the state the VM lab run left them in (#856): the
// scheduler set the log claim's node, then failed on the data claim, and the
// required member spread forbids that node to the member.
var _ = Describe("A new member whose log claim was bound to a node without its data claim (#856)", func() {
	ctx := context.Background()

	var (
		recorder *record.FakeRecorder
		now      time.Time
	)

	BeforeEach(func() {
		recorder = record.NewFakeRecorder(500)
		now = time.Now()
	})

	reconciler := func(c *databasev1alpha1.CubridCluster) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			Prober:       &memberProber{master: c.Name + "-0"},
			DefaultImage: claimSplitImage,
			Clock:        func() time.Time { return now },
		}
	}
	reconcileOnce := func(c *databasev1alpha1.CubridCluster) reconcile.Result {
		GinkgoHelper()
		res, err := reconciler(c).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	// removeWithFinalizers deletes an object a test wrote, also when the API
	// server's PVC protection left a finalizer on it.
	removeWithFinalizers := func(obj client.Object) {
		GinkgoHelper()
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); apierrors.IsNotFound(err) {
			return
		}
		if len(obj.GetFinalizers()) > 0 {
			obj.SetFinalizers(nil)
			Expect(client.IgnoreNotFound(k8sClient.Update(ctx, obj))).To(Succeed())
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj, client.GracePeriodSeconds(0)))).To(Succeed())
	}
	// gone reports whether the object was deleted or is being deleted.
	gone := func(obj client.Object) bool {
		GinkgoHelper()
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if apierrors.IsNotFound(err) {
			return true
		}
		Expect(err).NotTo(HaveOccurred())
		return obj.GetDeletionTimestamp() != nil
	}
	newCluster := func(name string) (*databasev1alpha1.CubridCluster, appsv1.StatefulSet) {
		GinkgoHelper()
		c := haCluster(name)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		reconcileOnce(c)
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), &sts)).To(Succeed())
		return c, sts
	}

	// member is what a test writes for member 1 of the cluster.
	type member struct {
		pod       *corev1.Pod
		data, log *corev1.PersistentVolumeClaim
	}
	// claim writes a member claim as the StatefulSet controller creates it,
	// with the StatefulSet's selector labels, then gives it the node and the
	// phase the scheduler and the provisioner would.
	claim := func(c *databasev1alpha1.CubridCluster, name, node, volume string, phase corev1.PersistentVolumeClaimPhase,
		edit func(*corev1.PersistentVolumeClaim)) *corev1.PersistentVolumeClaim {
		GinkgoHelper()
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labelsFor(c)},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				VolumeName:  volume,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		if node != "" {
			pvc.Annotations = map[string]string{selectedNodeKey: node}
		}
		if edit != nil {
			edit(pvc)
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		DeferCleanup(removeWithFinalizers, pvc)
		pvc.Status.Phase = phase
		Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
		return pvc
	}
	// pendingPod writes member 1's Pod as the StatefulSet controller creates
	// it, Pending since unschedulableFor with no container ever started.
	pendingPod := func(c *databasev1alpha1.CubridCluster, sts appsv1.StatefulSet, unschedulableFor time.Duration,
		edit func(*corev1.Pod)) *corev1.Pod {
		GinkgoHelper()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: c.Name + "-1", Namespace: c.Namespace, Labels: labelsFor(c),
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(&sts, appsv1.SchemeGroupVersion.WithKind("StatefulSet")),
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: appName, Image: claimSplitImage}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(removeWithFinalizers, pod)
		pod.Status.Phase = corev1.PodPending
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			LastTransitionTime: metav1.NewTime(now.Add(-unschedulableFor)),
			Message: "0/3 nodes are available: 1 node(s) didn't match pod anti-affinity rules, " +
				"2 node(s) didn't match PersistentVolume's node affinity.",
		}}
		if edit != nil {
			edit(pod)
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		return pod
	}
	// split writes member 1 as the VM lab run left it: the log claim on
	// worker-3 with its volume provisioned, the data claim Pending with no
	// node, and the Pod unschedulable for two minutes.
	split := func(c *databasev1alpha1.CubridCluster, sts appsv1.StatefulSet) member {
		GinkgoHelper()
		return member{
			data: claim(c, "data-"+c.Name+"-1", "", "", corev1.ClaimPending, nil),
			log:  claim(c, "log-"+c.Name+"-1", "worker-3", "pv-log-1", corev1.ClaimBound, nil),
			pod:  pendingPod(c, sts, 2*time.Minute, nil),
		}
	}
	expectUntouched := func(m member) {
		GinkgoHelper()
		Expect(gone(m.log)).To(BeFalse(), "the log claim must be left in place")
		Expect(gone(m.pod)).To(BeFalse(), "the Pod must be left in place")
		Expect(gone(m.data)).To(BeFalse(), "the data claim must never be deleted")
		Expect(eventsWith(recorder, "MemberLogClaimDeleted")).To(BeEmpty())
	}

	It("deletes only the log claim and the Pod once the member has been unschedulable for the grace period", func() {
		c, sts := newCluster("split-past-grace")
		m := split(c, sts)
		dataBefore := m.data.DeepCopy()

		reconcileOnce(c)

		Expect(gone(m.log)).To(BeTrue(), "the pinned log claim must be deleted so the StatefulSet recreates it")
		Expect(gone(m.pod)).To(BeTrue(), "the Pod must be deleted so the StatefulSet recreates it")
		Expect(gone(m.data)).To(BeFalse(), "the data claim must never be deleted")
		Expect(m.data.UID).To(Equal(dataBefore.UID))
		Expect(m.data.ResourceVersion).To(Equal(dataBefore.ResourceVersion), "the data claim must not be modified")
		events := eventsWith(recorder, "MemberLogClaimDeleted")
		Expect(events).To(HaveLen(1))
		Expect(events[0]).To(HavePrefix("Warning"))
		Expect(events[0]).To(ContainSubstring(c.Name + "-1"))
		Expect(events[0]).To(ContainSubstring("worker-3"))
	})

	It("deletes nothing while the member has been unschedulable for less than the grace period, and checks again", func() {
		c, sts := newCluster("split-within-grace")
		m := member{
			data: claim(c, "data-"+c.Name+"-1", "", "", corev1.ClaimPending, nil),
			log:  claim(c, "log-"+c.Name+"-1", "worker-3", "", corev1.ClaimPending, nil),
			pod:  pendingPod(c, sts, 10*time.Second, nil),
		}

		res := reconcileOnce(c)

		expectUntouched(m)
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "the member must be checked again when the grace period ends")
		Expect(res.RequeueAfter).To(BeNumerically("<=", 50*time.Second))
	})

	It("deletes nothing when the data claim has a node", func() {
		c, sts := newCluster("split-data-node")
		m := member{
			data: claim(c, "data-"+c.Name+"-1", "worker-2", "", corev1.ClaimPending, nil),
			log:  claim(c, "log-"+c.Name+"-1", "worker-3", "pv-log-1", corev1.ClaimBound, nil),
			pod:  pendingPod(c, sts, 2*time.Minute, nil),
		}
		reconcileOnce(c)
		expectUntouched(m)
	})

	It("deletes nothing when the data claim is bound", func() {
		c, sts := newCluster("split-data-bound")
		m := member{
			data: claim(c, "data-"+c.Name+"-1", "", "pv-data-1", corev1.ClaimBound, nil),
			log:  claim(c, "log-"+c.Name+"-1", "worker-3", "pv-log-1", corev1.ClaimBound, nil),
			pod:  pendingPod(c, sts, 2*time.Minute, nil),
		}
		reconcileOnce(c)
		expectUntouched(m)
	})

	It("deletes nothing when a container of the Pod ever started", func() {
		c, sts := newCluster("split-started")
		m := member{
			data: claim(c, "data-"+c.Name+"-1", "", "", corev1.ClaimPending, nil),
			log:  claim(c, "log-"+c.Name+"-1", "worker-3", "pv-log-1", corev1.ClaimBound, nil),
			pod: pendingPod(c, sts, 2*time.Minute, func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: appName, Image: claimSplitImage, RestartCount: 1,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
				}}
			}),
		}
		reconcileOnce(c)
		expectUntouched(m)
	})

	It("deletes nothing when the log claim has no node and no volume", func() {
		c, sts := newCluster("split-log-free")
		m := member{
			data: claim(c, "data-"+c.Name+"-1", "", "", corev1.ClaimPending, nil),
			log:  claim(c, "log-"+c.Name+"-1", "", "", corev1.ClaimPending, nil),
			pod:  pendingPod(c, sts, 2*time.Minute, nil),
		}
		reconcileOnce(c)
		expectUntouched(m)
	})

	It("deletes nothing when the log claim carries the labels of another cluster", func() {
		c, sts := newCluster("split-log-foreign")
		m := member{
			data: claim(c, "data-"+c.Name+"-1", "", "", corev1.ClaimPending, nil),
			log: claim(c, "log-"+c.Name+"-1", "worker-3", "pv-log-1", corev1.ClaimBound, func(p *corev1.PersistentVolumeClaim) {
				p.Labels["app.kubernetes.io/instance"] = "another-cluster"
			}),
			pod: pendingPod(c, sts, 2*time.Minute, nil),
		}
		reconcileOnce(c)
		expectUntouched(m)
	})

	It("deletes nothing when the log claim is owned by an object other than the member's StatefulSet", func() {
		c, sts := newCluster("split-log-owner")
		m := member{
			data: claim(c, "data-"+c.Name+"-1", "", "", corev1.ClaimPending, nil),
			log: claim(c, "log-"+c.Name+"-1", "worker-3", "pv-log-1", corev1.ClaimBound, func(p *corev1.PersistentVolumeClaim) {
				p.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "StatefulSet", Name: "another-statefulset", UID: "8a4f0c4e-0000-4000-8000-000000000856",
				}}
			}),
			pod: pendingPod(c, sts, 2*time.Minute, nil),
		}
		reconcileOnce(c)
		expectUntouched(m)
	})

	It("deletes nothing when the Pod is not controlled by the cluster's StatefulSet", func() {
		c, sts := newCluster("split-pod-foreign")
		m := member{
			data: claim(c, "data-"+c.Name+"-1", "", "", corev1.ClaimPending, nil),
			log:  claim(c, "log-"+c.Name+"-1", "worker-3", "pv-log-1", corev1.ClaimBound, nil),
		}
		// The Pod's controller is a StatefulSet of the same name with another
		// UID, as for a Pod an earlier StatefulSet created.
		sts.UID = "8a4f0c4e-0000-4000-8000-000000000857"
		m.pod = pendingPod(c, sts, 2*time.Minute, nil)
		reconcileOnce(c)
		expectUntouched(m)
	})
})

// eventsWith drains the recorder and returns the Events whose text names reason.
func eventsWith(recorder *record.FakeRecorder, reason string) []string {
	var out []string
	for _, e := range drain(recorder) {
		if strings.Contains(e, " "+reason+" ") {
			out = append(out, e)
		}
	}
	return out
}
