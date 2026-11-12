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
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// fakeDBAPasswordChecker stands in for the member's Instance Manager: every
// check answers answer, or fails with err, and is recorded.
type fakeDBAPasswordChecker struct {
	mu     sync.Mutex
	answer instancemanager.DBAPasswordCheck
	err    error
	asked  []string
}

func (f *fakeDBAPasswordChecker) CheckDBAPassword(_ context.Context, podName, namespace string) (instancemanager.DBAPasswordCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, namespace+"/"+podName)
	return f.answer, f.err
}

func (f *fakeDBAPasswordChecker) set(state instancemanager.DBAPasswordState, reason string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer, f.err = instancemanager.DBAPasswordCheck{State: state, Reason: reason}, err
}

func (f *fakeDBAPasswordChecker) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// A changed value of the DBA password Secret is not applied to the running
// database. The operator asks the member's Instance Manager whether the value
// is still the database's DBA password, and reports a value that is not as
// an unsupported rotation, without touching the StatefulSet or its Pods
// (#824).
var _ = Describe("A changed DBA password Secret value (#824)", func() {
	ctx := context.Background()

	var lines struct {
		sync.Mutex
		all []string
	}
	logCtx := logf.IntoContext(ctx, funcr.New(func(prefix, args string) {
		lines.Lock()
		defer lines.Unlock()
		lines.all = append(lines.all, prefix+" "+args)
	}, funcr.Options{Verbosity: 2}))

	const poll = 2 * time.Minute
	newReconciler := func(checker DBAPasswordChecker) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), DBAPassword: checker, DBAPasswordPoll: poll,
		}
	}
	getCluster := func(key types.NamespacedName) *databasev1alpha1.CubridCluster {
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		return c
	}
	resolvedOf := func(key types.NamespacedName) *metav1.Condition {
		c := meta.FindStatusCondition(getCluster(key).Status.Conditions, conditionDBAPasswordSecretResolved)
		Expect(c).NotTo(BeNil())
		return c
	}
	reconcileOnce := func(r *CubridClusterReconciler, key types.NamespacedName) reconcile.Result {
		res, err := r.Reconcile(logCtx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	// runningCluster creates a standalone cluster with a DBA password Secret,
	// lets the first reconcile create its StatefulSet, and marks its one
	// member ready, as a running database is.
	runningCluster := func(name string, r *CubridClusterReconciler) types.NamespacedName {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-secret", Namespace: testNamespace},
			Data: map[string][]byte{dbaTestKey: []byte(dbaTestPassword)}}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
		c := standaloneCluster(name)
		c.Spec.DBAPasswordSecretRef = dbaPasswordRef(secret.Name, dbaTestKey)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		key := client.ObjectKeyFromObject(c)
		reconcileOnce(r, key)
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		sts.Status.Replicas, sts.Status.ReadyReplicas = 1, 1
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
		return key
	}
	// expectNoValue checks that the Secret's value is in no status, Event or
	// log line.
	expectNoValue := func(key types.NamespacedName, events []string) {
		status, err := json.Marshal(getCluster(key).Status)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(status)).NotTo(ContainSubstring(dbaTestPassword), "the status holds the password")
		for _, e := range events {
			Expect(e).NotTo(ContainSubstring(dbaTestPassword), "an Event holds the password")
		}
		lines.Lock()
		defer lines.Unlock()
		for _, line := range lines.all {
			Expect(line).NotTo(ContainSubstring(dbaTestPassword), "a log line holds the password")
		}
	}

	It("reports a value that is not the database's password as an unsupported rotation, and clears it once it is again", func() {
		checker := &fakeDBAPasswordChecker{}
		r := newReconciler(checker)
		key := runningCluster("dba-rotated", r)
		checker.set(instancemanager.DBAPasswordMatches, "", nil)
		reconcileOnce(r, key)
		Expect(resolvedOf(key).Reason).To(Equal(reasonDBAPasswordSecretResolved))
		drain(r.Recorder.(*record.FakeRecorder))
		before := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, before)).To(Succeed())

		By("reporting the value that no longer logs in")
		checker.set(instancemanager.DBAPasswordDiffers, "a dba login with the value was refused for its password", nil)
		res := reconcileOnce(r, key)
		resolved := resolvedOf(key)
		Expect(resolved.Status).To(Equal(metav1.ConditionFalse))
		Expect(resolved.Reason).To(Equal(reasonDBAPasswordRotationUnsupported))
		Expect(resolved.Message).To(ContainSubstring("dba-rotated-secret"))
		Expect(resolved.Message).To(ContainSubstring("not supported"))
		Expect(resolved.Message).To(ContainSubstring("current DBA password back"))
		events := drain(r.Recorder.(*record.FakeRecorder))
		Expect(events).To(ContainElement(And(
			HavePrefix(corev1.EventTypeWarning+" "+reasonDBAPasswordRotationUnsupported),
			ContainSubstring("dba-rotated-secret"))))
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(res.RequeueAfter).To(BeNumerically("<=", poll), "the member is asked again after the poll interval")

		By("leaving the StatefulSet and its Pod template as they are")
		after := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
		Expect(after.UID).To(Equal(before.UID))
		Expect(after.Generation).To(Equal(before.Generation), "the StatefulSet's spec was changed")
		Expect(after.Spec).To(Equal(before.Spec))
		expectNoValue(key, events)

		By("reporting the same problem once")
		transition := resolved.LastTransitionTime
		reconcileOnce(r, key)
		Expect(drain(r.Recorder.(*record.FakeRecorder))).NotTo(ContainElement(ContainSubstring("DBAPassword")))
		Expect(resolvedOf(key).LastTransitionTime).To(Equal(transition))

		By("clearing it once the value logs in again")
		checker.set(instancemanager.DBAPasswordMatches, "", nil)
		reconcileOnce(r, key)
		resolved = resolvedOf(key)
		Expect(resolved.Status).To(Equal(metav1.ConditionTrue))
		Expect(resolved.Reason).To(Equal(reasonDBAPasswordSecretResolved))
		expectNoValue(key, drain(r.Recorder.(*record.FakeRecorder)))
	})

	It("keeps what it reported while the member cannot tell, and an operator that restarts reports the same", func() {
		checker := &fakeDBAPasswordChecker{}
		r := newReconciler(checker)
		key := runningCluster("dba-unknown", r)
		checker.set(instancemanager.DBAPasswordDiffers, "refused", nil)
		reconcileOnce(r, key)
		reported := resolvedOf(key)
		Expect(reported.Reason).To(Equal(reasonDBAPasswordRotationUnsupported))
		drain(r.Recorder.(*record.FakeRecorder))

		for name, set := range map[string]func(){
			"an Unknown answer":    func() { checker.set(instancemanager.DBAPasswordUnknown, "server appdb is not running", nil) },
			"a call that failed":   func() { checker.set("", "", errors.New("connection refused")) },
			"an answer not known":  func() { checker.set("Maybe", "", nil) },
			"no member ready":      func() { checker.set(instancemanager.DBAPasswordMatches, "", nil); setReadyReplicas(ctx, key, 0) },
			"a restarted operator": func() { checker.set(instancemanager.DBAPasswordUnknown, "busy", nil); r = newReconciler(checker) },
		} {
			By("keeping the condition after " + name)
			set()
			reconcileOnce(r, key)
			kept := resolvedOf(key)
			Expect(kept.Status).To(Equal(reported.Status), name)
			Expect(kept.Reason).To(Equal(reported.Reason), name)
			Expect(kept.LastTransitionTime).To(Equal(reported.LastTransitionTime), name)
			Expect(drain(r.Recorder.(*record.FakeRecorder))).NotTo(ContainElement(ContainSubstring("DBAPassword")), name)
			setReadyReplicas(ctx, key, 1)
		}

		By("a restarted operator asking again and reporting the same")
		r = newReconciler(checker)
		checker.set(instancemanager.DBAPasswordDiffers, "refused", nil)
		reconcileOnce(r, key)
		Expect(resolvedOf(key).Reason).To(Equal(reasonDBAPasswordRotationUnsupported))
		expectNoValue(key, drain(r.Recorder.(*record.FakeRecorder)))
	})

	It("never asks about a cluster without the reference, nor a member that is not ready", func() {
		checker := &fakeDBAPasswordChecker{}
		r := newReconciler(checker)
		plain := standaloneCluster("dba-rotation-none")
		Expect(k8sClient.Create(ctx, plain)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, plain))).To(Succeed()) })
		plainKey := client.ObjectKeyFromObject(plain)
		reconcileOnce(r, plainKey)
		setReadyReplicas(ctx, plainKey, 1)
		res := reconcileOnce(r, plainKey)
		Expect(checker.calls()).To(BeEmpty())
		Expect(res.RequeueAfter).To(BeZero(), "a standalone cluster without the reference is reconciled on events only")

		key := runningCluster("dba-rotation-unready", r)
		setReadyReplicas(ctx, key, 0)
		res = reconcileOnce(r, key)
		Expect(checker.calls()).To(BeEmpty())
		Expect(res.RequeueAfter).To(Equal(poll), "the cluster is looked at again until its member is ready")

		setReadyReplicas(ctx, key, 1)
		checker.set(instancemanager.DBAPasswordMatches, "", nil)
		res = reconcileOnce(r, key)
		Expect(checker.calls()).To(Equal([]string{testNamespace + "/dba-rotation-unready-0"}))
		Expect(res.RequeueAfter).To(Equal(poll))
	})
})

// setReadyReplicas sets how many members of the cluster's StatefulSet are
// ready.
func setReadyReplicas(ctx context.Context, key types.NamespacedName, ready int32) {
	sts := &appsv1.StatefulSet{}
	Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
	sts.Status.Replicas, sts.Status.ReadyReplicas = 1, ready
	Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
}
