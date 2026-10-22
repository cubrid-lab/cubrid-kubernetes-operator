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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// errRestoreMissing is what the client returns for a poll of the recovery
// restore that its target answers 404.
var errRestoreMissing = fmt.Errorf("get operation %s on the target: %w", opRestore, errOperationMissing)

// #457: a recovery whose recorded restore operation can no longer be read
// from its target ends Failed once the record has stayed missing for
// recoveryOperationMissingWait, with the target and the operation in its
// reason. The restore is never started again.
var _ = Describe("Recovery whose recorded restore operation goes missing", func() {
	ctx := context.Background()

	type fixture struct {
		key     types.NamespacedName
		restore *fakeRestoreClient
		now     *time.Time
		target  string
	}
	reconciler := func(f fixture) *CubridClusterReconciler {
		return &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: f.restore,
			Clock: func() time.Time { return *f.now }}
	}
	reconcileOnce := func(f fixture) (reconcile.Result, *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		res, err := reconciler(f).Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(err).NotTo(HaveOccurred())
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		return res, got
	}
	bootstrapReady := func(c *databasev1alpha1.CubridCluster) metav1.Condition {
		GinkgoHelper()
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		return *cond
	}
	// started creates a recovery cluster and lets it start its restore, so
	// that the operation is recorded.
	started := func(name string) fixture {
		GinkgoHelper()
		c := standaloneCluster(name)
		c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
		}
		c.Spec.ObjectStorage = haObjectStorage()
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
		now := time.Now()
		f := fixture{key: types.NamespacedName{Name: name, Namespace: c.Namespace}, restore: &fakeRestoreClient{},
			now: &now, target: name + "-0"}
		_, got := reconcileOnce(f)
		Expect(got.Status.Bootstrap.OperationID).To(Equal(opRestore))
		Expect(f.restore.keys).To(HaveLen(1))
		return f
	}
	advance := func(f fixture, d time.Duration) { *f.now = f.now.Add(d) }

	It("ends the recovery when the operation stays missing past the bound, across an operator restart", func() {
		f := started("recovery-missing")
		f.restore.pollErr = errRestoreMissing

		By("reporting the missing record, within the bound")
		res, got := reconcileOnce(f)
		Expect(res.RequeueAfter).To(Equal(restorePollAfter))
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapRestoring))
		Expect(got.Status.Bootstrap.OperationMissingSince).NotTo(BeNil())
		cond := bootstrapReady(got)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(reasonRecordedOperationMissing))
		Expect(cond.Message).To(And(ContainSubstring(f.target), ContainSubstring(opRestore),
			ContainSubstring(recoveryOperationMissingWait.String())))
		Expect(cond.Message).NotTo(ContainSubstring(errRestoreMissing.Error()),
			"the per-request answer would change the condition at every poll")
		missingSince := got.Status.Bootstrap.OperationMissingSince.Time

		By("keeping the bound in a reconciler created anew")
		advance(f, recoveryOperationMissingWait-time.Minute)
		_, got = reconcileOnce(f)
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapRestoring))
		Expect(got.Status.Bootstrap.OperationMissingSince.Time).To(BeTemporally("==", missingSince))
		Expect(bootstrapReady(got).Reason).To(Equal(reasonRecordedOperationMissing))

		By("ending the recovery once the bound has passed")
		advance(f, 2*time.Minute)
		res, got = reconcileOnce(f)
		Expect(res.RequeueAfter).To(BeZero())
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
		cond = bootstrapReady(got)
		Expect(cond.Reason).To(Equal(reasonRecordedOperationLost))
		Expect(cond.Message).To(And(ContainSubstring(f.target), ContainSubstring(opRestore),
			ContainSubstring("not started again")))
		Expect(got.Status.Bootstrap.Attempts).To(BeZero(), "a missing record is not a failed attempt")

		By("never starting the restore again")
		polls := f.restore.polls
		_, got = reconcileOnce(f)
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
		Expect(f.restore.keys).To(HaveLen(1), "the restore was started again")
		Expect(f.restore.polls).To(Equal(polls), "a failed recovery is not polled again")
	})

	It("keeps the bound across a transient failure, clears it on a read, and still follows the operation", func() {
		f := started("recovery-missing-back")
		f.restore.pollErr = errRestoreMissing
		_, got := reconcileOnce(f)
		Expect(got.Status.Bootstrap.OperationMissingSince).NotTo(BeNil())
		missingSince := got.Status.Bootstrap.OperationMissingSince.Time

		By("a transient failure within the bound keeps the time the record went missing")
		advance(f, 10*time.Minute)
		f.restore.pollErr = errors.New("dial tcp: connection refused")
		_, got = reconcileOnce(f)
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapRestoring))
		Expect(got.Status.Bootstrap.OperationMissingSince).NotTo(BeNil())
		Expect(got.Status.Bootstrap.OperationMissingSince.Time).To(BeTemporally("==", missingSince))
		Expect(bootstrapReady(got).Reason).To(Equal(reasonInstanceManagerUnavailable))

		By("a readable operation clears the missing record")
		advance(f, 10*time.Minute)
		f.restore.pollErr = nil
		_, got = reconcileOnce(f)
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapRestoring))
		Expect(got.Status.Bootstrap.OperationMissingSince).To(BeNil())
		Expect(bootstrapReady(got).Reason).To(Equal("RestoreInProgress"))

		By("a record missing again starts a new bound")
		firstSince := missingSince
		advance(f, 20*time.Minute)
		f.restore.pollErr = errRestoreMissing
		_, got = reconcileOnce(f)
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapRestoring))
		Expect(got.Status.Bootstrap.OperationMissingSince.Time).To(BeTemporally(">", firstSince))

		By("a failed operation is still a failed attempt, started again under the next key")
		f.restore.pollErr = nil
		f.restore.failed = true
		_, got = reconcileOnce(f)
		Expect(bootstrapReady(got).Reason).To(Equal("RestoreFailedRetrying"))
		Expect(got.Status.Bootstrap.Attempts).To(Equal(int32(1)))
		Expect(got.Status.Bootstrap.OperationMissingSince).To(BeNil())

		By("a completed operation still completes the recovery")
		f.restore.failed = false
		f.restore.completed = true
		_, got = reconcileOnce(f) // starts the next attempt
		Expect(f.restore.keys).To(HaveLen(2))
		Expect(got.Status.Bootstrap.OperationID).To(Equal(opRestore))
		_, got = reconcileOnce(f)
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapComplete))
		Expect(bootstrapReady(got).Reason).To(Equal("RecoveryComplete"))
	})
})
