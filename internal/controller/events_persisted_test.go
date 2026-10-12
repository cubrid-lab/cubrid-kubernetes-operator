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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// An Event says that the cluster's state changed. The change is in the
// status, so an Event for a status that was not written would be repeated by
// the reconcile that writes it, and an Event for a state that did not change
// says nothing new (docs/observability.md).
var _ = Describe("Events of a change of state (#371)", func() {
	ctx := context.Background()

	// faults are what the next calls to the API server do instead of
	// succeeding.
	type faults struct {
		// statusConflicts is how many status writes are refused as conflicts.
		statusConflicts int
		// statefulSetError fails every creation of a StatefulSet.
		statefulSetError error
	}
	type fixture struct {
		key      types.NamespacedName
		r        *CubridClusterReconciler
		prober   *memberProber
		recorder *record.FakeRecorder
		faults   *faults
	}
	newFixture := func(name string) *fixture {
		c := haCluster(name)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		f := &fixture{
			key:    types.NamespacedName{Name: c.Name, Namespace: c.Namespace},
			prober: &memberProber{master: name + "-0"}, recorder: record.NewFakeRecorder(100), faults: &faults{},
		}
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		faulty := interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*databasev1alpha1.CubridCluster); ok && subResource == "status" && f.faults.statusConflicts > 0 {
					f.faults.statusConflicts--
					return apierrors.NewConflict(schema.GroupResource{Group: databasev1alpha1.GroupVersion.Group, Resource: "cubridclusters"},
						obj.GetName(), errors.New("the object has been modified"))
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); ok && f.faults.statefulSetError != nil {
					return f.faults.statefulSetError
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		f.r = &CubridClusterReconciler{Client: faulty, Scheme: k8sClient.Scheme(), Recorder: f.recorder, Prober: f.prober}
		return f
	}
	reconcileOnce := func(f *fixture) error {
		_, err := f.r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		return err
	}
	// events returns the Events recorded since the last call that carry reason.
	events := func(f *fixture, reason string) []string {
		var out []string
		for {
			select {
			case e := <-f.recorder.Events:
				if strings.Contains(e, " "+reason+" ") {
					out = append(out, e)
				}
			default:
				return out
			}
		}
	}

	It("records a change once, when its status was written, and not before", func() {
		f := newFixture("ev-conflict")
		Expect(reconcileOnce(f)).To(Succeed())
		Expect(events(f, "PrimaryResolved")).To(HaveLen(1))

		By("the primary changing in a reconcile whose status write is refused")
		f.prober.master = f.key.Name + "-1"
		f.faults.statusConflicts = 1
		Expect(reconcileOnce(f)).To(Succeed())
		Expect(events(f, "PrimaryChanged")).To(BeEmpty(), "an Event was recorded for a status that was not written")

		By("the next reconcile writing the status")
		Expect(reconcileOnce(f)).To(Succeed())
		Expect(events(f, "PrimaryChanged")).To(HaveLen(1))

		By("reconciles that change nothing")
		for range 3 {
			Expect(reconcileOnce(f)).To(Succeed())
		}
		Expect(events(f, "PrimaryChanged")).To(BeEmpty())
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		Expect(got.Status.CurrentPrimary).To(Equal(f.key.Name + "-1"))
	})

	It("records a failure that stays the same once", func() {
		f := newFixture("ev-failure")
		f.faults.statefulSetError = errors.New("admission webhook denied the StatefulSet")
		for range 3 {
			Expect(reconcileOnce(f)).To(MatchError(ContainSubstring("admission webhook denied")))
		}
		Expect(events(f, "StatefulSetReconcileFailed")).To(HaveLen(1))

		By("another failure being a change")
		f.faults.statefulSetError = errors.New("quota exceeded")
		Expect(reconcileOnce(f)).To(HaveOccurred())
		Expect(reconcileOnce(f)).To(HaveOccurred())
		recorded := events(f, "StatefulSetReconcileFailed")
		Expect(recorded).To(HaveLen(1))
		Expect(recorded[0]).To(ContainSubstring("quota exceeded"))
	})

	It("records a failure whose status could not be written when it is written", func() {
		f := newFixture("ev-failure-conflict")
		f.faults.statefulSetError = errors.New("admission webhook denied the StatefulSet")
		f.faults.statusConflicts = 1
		Expect(reconcileOnce(f)).To(HaveOccurred())
		Expect(events(f, "StatefulSetReconcileFailed")).To(BeEmpty())
		Expect(reconcileOnce(f)).To(HaveOccurred())
		Expect(events(f, "StatefulSetReconcileFailed")).To(HaveLen(1))
	})
})
