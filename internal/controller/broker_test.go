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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

var _ = Describe("Broker tier (ADR-0002, #83)", func() {
	const brokerNamespace = "default"
	ctx := context.Background()

	reconcileHA := func(name string) (*CubridClusterReconciler, types.NamespacedName) {
		c := haCluster(name)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			Prober:       &memberProber{master: name + "-0"},
			DefaultImage: "registry.example/cubrid-instance-manager:test",
		}
		key := types.NamespacedName{Name: name, Namespace: brokerNamespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return r, key
	}
	brokerDeployments := func(cluster string) []appsv1.Deployment {
		var list appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(brokerNamespace), client.MatchingLabels{
			"app.kubernetes.io/instance": cluster, "app.kubernetes.io/component": "broker",
		})).To(Succeed())
		return list.Items
	}
	// backends returns the Deployments whose Pods the Service would select,
	// judged from the two objects as they are stored, not from shared constants.
	backends := func(cluster, service string) []appsv1.Deployment {
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: service, Namespace: brokerNamespace}, svc)).To(Succeed())
		Expect(svc.Spec.Selector).NotTo(BeEmpty())
		selector := labels.SelectorFromSet(svc.Spec.Selector)
		var out []appsv1.Deployment
		for _, dep := range brokerDeployments(cluster) {
			if selector.Matches(labels.Set(dep.Spec.Template.Labels)) {
				out = append(out, dep)
			}
		}
		return out
	}

	It("gives the read-write and the read-only Service their own Broker Pods", func() {
		reconcileHA("broker-split")

		rw := backends("broker-split", "broker-split-rw")
		ro := backends("broker-split", "broker-split-ro")
		Expect(rw).To(HaveLen(1), "the -rw Service selects no Broker Pods")
		Expect(ro).To(HaveLen(1), "the -ro Service selects no Broker Pods")
		Expect(rw[0].Name).NotTo(Equal(ro[0].Name), "one Broker cannot be read-write and read-only")

		for mode, dep := range map[string]appsv1.Deployment{"rw": rw[0], "ro": ro[0]} {
			c := dep.Spec.Template.Spec.Containers[0]
			access, ok := envValue(c, "BROKER_ACCESS_MODE")
			Expect(ok).To(BeTrue(), mode)
			Expect(access.Value).To(Equal(mode))
			Expect(c.Command).To(Equal([]string{"/usr/local/bin/broker-entrypoint.sh"}), mode)
			Expect(c.Image).To(Equal("registry.example/cubrid-instance-manager:test"),
				"the Broker runs the image that has the non-root Broker entrypoint")
		}
	})

	It("serves each Service on the port its Broker listens on", func() {
		reconcileHA("broker-ports")
		for service, port := range map[string]int32{"broker-ports-rw": 33000, "broker-ports-ro": 33001} {
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: service, Namespace: brokerNamespace}, svc)).To(Succeed())
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(port))
			deps := backends("broker-ports", service)
			Expect(deps).To(HaveLen(1))
			c := deps[0].Spec.Template.Spec.Containers[0]
			Expect(c.Ports).To(HaveLen(1))
			Expect(c.Ports[0].ContainerPort).To(Equal(port))
			Expect(c.ReadinessProbe.TCPSocket.Port.IntValue()).To(Equal(int(port)), "ready means this Broker listens")
		}
	})

	It("runs two Brokers per access mode as the non-root cubrid user, spread over nodes", func() {
		reconcileHA("broker-ha")
		deps := brokerDeployments("broker-ha")
		Expect(deps).To(HaveLen(2))
		for _, dep := range deps {
			Expect(dep.Spec.Replicas).NotTo(BeNil())
			Expect(*dep.Spec.Replicas).To(BeEquivalentTo(2), dep.Name)
			spec := dep.Spec.Template.Spec
			Expect(spec.SecurityContext.RunAsUser).NotTo(BeNil())
			Expect(*spec.SecurityContext.RunAsUser).To(BeEquivalentTo(1000))
			Expect(spec.Affinity).NotTo(BeNil())
			Expect(spec.Affinity.PodAntiAffinity).NotTo(BeNil(), dep.Name)
			terms := spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
			Expect(terms).To(HaveLen(1))
			Expect(terms[0].PodAffinityTerm.TopologyKey).To(Equal("kubernetes.io/hostname"))
			Expect(labels.SelectorFromSet(terms[0].PodAffinityTerm.LabelSelector.MatchLabels).
				Matches(labels.Set(dep.Spec.Template.Labels))).To(BeTrue(), "the term must select this Deployment's own Pods")
		}
	})

	It("removes the single Broker Deployment of earlier versions", func() {
		old := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "broker-legacy-broker", Namespace: brokerNamespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "legacy"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"tier": "legacy"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "old", Image: "x"}}},
				},
			},
		}
		c := haCluster("broker-legacy")
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		Expect(ctrlSetOwner(c, old)).To(Succeed())
		Expect(k8sClient.Create(ctx, old)).To(Succeed())

		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			Prober: &memberProber{master: "broker-legacy-0"},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name, Namespace: c.Namespace}})
		Expect(err).NotTo(HaveOccurred())
		err = k8sClient.Get(ctx, types.NamespacedName{Name: old.Name, Namespace: brokerNamespace}, &appsv1.Deployment{})
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		Expect(err).To(HaveOccurred(), "the old Deployment is still there")
	})

	It("reports an endpoint ready only when one of its Brokers is available", func() {
		r, key := reconcileHA("broker-ready")
		condition := func(conditionType string) *metav1.Condition {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			return meta.FindStatusCondition(c.Status.Conditions, conditionType)
		}
		By("no Broker Pod is available in envtest")
		for _, conditionType := range []string{conditionBrokerReady, conditionWriteEndpointReady, conditionReadEndpointReady} {
			cond := condition(conditionType)
			Expect(cond).NotTo(BeNil(), conditionType)
			Expect(cond.Status).To(BeEquivalentTo("False"), "%s is claimed with no Broker running", conditionType)
		}

		By("the read-write Brokers become available")
		rw := backends("broker-ready", "broker-ready-rw")[0]
		rw.Status.Replicas, rw.Status.ReadyReplicas, rw.Status.AvailableReplicas = 2, 2, 2
		Expect(k8sClient.Status().Update(ctx, &rw)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(condition(conditionWriteEndpointReady).Status).To(BeEquivalentTo("True"))
		Expect(condition(conditionReadEndpointReady).Status).To(BeEquivalentTo("False"))
		Expect(condition(conditionBrokerReady).Status).To(BeEquivalentTo("False"))
	})
})

// ctrlSetOwner makes cluster the controller owner of obj, as the operator does.
func ctrlSetOwner(cluster *databasev1alpha1.CubridCluster, obj client.Object) error {
	return controllerutil.SetControllerReference(cluster, obj, k8sClient.Scheme())
}
