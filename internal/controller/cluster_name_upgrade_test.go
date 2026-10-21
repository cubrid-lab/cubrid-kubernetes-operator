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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// A CubridCluster created before the name rule existed keeps its immutable
// name. Installing the CRD with the rule must not block updates to it, or
// neither users nor the operator could maintain it, while a new object with
// such a name is still refused (#459).
var _ = Describe("Cluster name validation across a CRD upgrade (#459)", func() {
	ctx := context.Background()

	const crdName = "cubridclusters.database.cubrid.io"
	crdGVK := schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
	versionsPath := []string{"spec", "versions"}
	rulesPath := []string{"schema", "openAPIV3Schema", "x-kubernetes-validations"}

	getCRD := func() *unstructured.Unstructured {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK)
		ExpectWithOffset(2, k8sClient.Get(ctx, client.ObjectKey{Name: crdName}, crd)).To(Succeed())
		return crd
	}
	// getRootRules and setRootRules read and replace the root
	// x-kubernetes-validations of the installed CRD's only version.
	getRootRules := func() []any {
		versions, _, err := unstructured.NestedSlice(getCRD().Object, versionsPath...)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, versions).To(HaveLen(1))
		rules, _, err := unstructured.NestedSlice(versions[0].(map[string]any), rulesPath...)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return rules
	}
	setRootRules := func(rules []any) {
		crd := getCRD()
		versions, _, err := unstructured.NestedSlice(crd.Object, versionsPath...)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		version := versions[0].(map[string]any)
		if len(rules) == 0 {
			unstructured.RemoveNestedField(version, rulesPath...)
		} else {
			ExpectWithOffset(1, unstructured.SetNestedSlice(version, rules, rulesPath...)).To(Succeed())
		}
		ExpectWithOffset(1, unstructured.SetNestedSlice(crd.Object, versions, versionsPath...)).To(Succeed())
		ExpectWithOffset(1, k8sClient.Update(ctx, crd)).To(Succeed())
	}

	// legacyRules returns the installed root rules and the same rules without
	// the one on metadata.name. The CRD before #459 differs from this one only
	// by that rule, so removing it stands in for installing main's CRD.
	legacyRules := func() (installed, legacy []any) {
		installed = getRootRules()
		for _, r := range installed {
			if !strings.Contains(fmt.Sprint(r.(map[string]any)["rule"]), "metadata.name") {
				legacy = append(legacy, r)
			}
		}
		ExpectWithOffset(1, legacy).To(HaveLen(len(installed) - 1))
		return installed, legacy
	}

	// createLegacy creates a CubridCluster under the CRD without the name rule
	// and then installs the rule again, as an upgrade does.
	createLegacy := func(cluster *databasev1alpha1.CubridCluster) {
		installed, legacy := legacyRules()
		probe := func() error { return k8sClient.Create(ctx, cluster.DeepCopy(), client.DryRunAll) }
		setRootRules(legacy)
		DeferCleanup(func() { setRootRules(installed) })
		EventuallyWithOffset(1, probe).Should(Succeed())
		ExpectWithOffset(1, k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed()) })
		setRootRules(installed)
		EventuallyWithOffset(1, probe).ShouldNot(Succeed())
	}

	reconcileOnce := func(cluster *databasev1alpha1.CubridCluster, recorder *record.FakeRecorder) error {
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		return err
	}
	readyReason := func(cluster *databasev1alpha1.CubridCluster) string {
		c := &databasev1alpha1.CubridCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), c)).To(Succeed())
		if ready := meta.FindStatusCondition(c.Status.Conditions, conditionReady); ready != nil {
			return ready.Reason
		}
		return ""
	}

	// Every child name and label the operator writes for a 53-character
	// cluster fits in 63 characters, so such a cluster, which the API accepted
	// before the 52-character rule, stays managed.
	It("keeps managing an existing CubridCluster with a 53-character name", func() {
		cluster := standaloneCluster(strings.Repeat("m", 53))
		createLegacy(cluster)

		Expect(reconcileOnce(cluster, record.NewFakeRecorder(100))).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: instancesServiceName(cluster.Name), Namespace: cluster.Namespace},
			&corev1.Service{})).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), &appsv1.StatefulSet{})).To(Succeed())
		Expect(readyReason(cluster)).NotTo(Equal(reasonInvalidClusterName))
	})

	// A longer or non-DNS-1035 name cannot be managed: the operator refuses it
	// once, creates nothing and does not retry.
	It("refuses an existing CubridCluster whose name the operator cannot manage", func() {
		for _, name := range []string{strings.Repeat("x", 54), "9" + strings.Repeat("x", 20)} {
			cluster := standaloneCluster(name)
			createLegacy(cluster)

			recorder := record.NewFakeRecorder(100)
			err := reconcileOnce(cluster, recorder)
			Expect(err).To(HaveOccurred(), name)
			Expect(errors.Is(err, reconcile.TerminalError(nil))).To(BeTrue(), "a terminal error for %s: %v", name, err)
			Expect(readyReason(cluster)).To(Equal(reasonInvalidClusterName), name)

			for _, child := range []struct {
				name string
				obj  client.Object
			}{
				{instancesServiceName(name), &corev1.Service{}},
				{imTokenSecretName(name), &corev1.Secret{}},
				{name, &appsv1.StatefulSet{}},
			} {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: child.name, Namespace: cluster.Namespace}, child.obj)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s: %v", child.obj, child.name, err)
			}

			Expect(reconcileOnce(cluster, recorder)).To(HaveOccurred())
			Expect(recorder.Events).To(HaveLen(1), name)
			Expect(<-recorder.Events).To(HavePrefix("Warning " + reasonInvalidClusterName))
		}
	})

	It("allows updates to an existing CubridCluster with a too-long name and refuses new ones", func() {
		installed, legacy := legacyRules()

		probe := standaloneCluster(strings.Repeat("p", databasev1alpha1.MaxClusterNameLength+1))
		admitted := func() error { return k8sClient.Create(ctx, probe.DeepCopy(), client.DryRunAll) }

		setRootRules(legacy)
		DeferCleanup(func() { setRootRules(installed) })
		Eventually(admitted).Should(Succeed())

		existing := standaloneCluster(strings.Repeat("l", 60))
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, existing))).To(Succeed()) })

		maintain := func(step string) {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(existing), c)).To(Succeed())

			base := c.DeepCopy()
			c.Annotations = map[string]string{"example.com/maintained": step}
			Expect(k8sClient.Patch(ctx, c, client.MergeFrom(base))).To(Succeed(), "annotation patch "+step)

			base = c.DeepCopy()
			c.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
			if step == "after" {
				c.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
			}
			Expect(k8sClient.Patch(ctx, c, client.MergeFrom(base))).To(Succeed(), "spec patch "+step)

			base = c.DeepCopy()
			c.Status.ObservedGeneration = c.Generation
			Expect(k8sClient.Status().Patch(ctx, c, client.MergeFrom(base))).To(Succeed(), "status patch "+step)
		}
		maintain("before")

		setRootRules(installed)
		Eventually(admitted).Should(MatchError(ContainSubstring("at most 52 characters")))

		maintain("after")
		Expect(k8sClient.Create(ctx, standaloneCluster(strings.Repeat("n", 60)))).
			To(MatchError(ContainSubstring("at most 52 characters")))
	})
})
