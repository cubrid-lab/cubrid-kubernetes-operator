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
	"fmt"
	"reflect"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// childPath is one object a cluster reconciles under a name of its own.
type childPath struct {
	what string
	// foreign builds an object of the name the cluster needs, as someone other
	// than the cluster would have made it.
	foreign func(cluster string) client.Object
	// reason is the reason a refusal gives to Ready; empty for a Broker object,
	// which is reported on BrokerReady and leaves Ready to the members.
	reason string
}

const (
	foreignName      = "someone-elses"
	foreignContainer = "foreign-app"
	ownerKind        = "CubridCluster"
)

var (
	foreignLabels = map[string]string{"claimed-by": "unrelated-tool"}
	driftedLabels = map[string]string{"drifted-by": "hand"}
)

func foreignMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: metav1.NamespaceDefault, Labels: foreignLabels}
}

func foreignConfigMap(name string) client.Object {
	return &corev1.ConfigMap{ObjectMeta: foreignMeta(name), Data: map[string]string{"kept": "as it was"}}
}

func foreignService(name string) client.Object {
	return &corev1.Service{ObjectMeta: foreignMeta(name), Spec: corev1.ServiceSpec{
		Selector: foreignLabels,
		Ports:    []corev1.ServicePort{{Name: foreignContainer, Protocol: corev1.ProtocolTCP, Port: 8080}},
	}}
}

func foreignPodTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: foreignLabels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: foreignContainer, Image: "registry.example/app:1"}}},
	}
}

func foreignDeployment(name string) client.Object {
	return &appsv1.Deployment{ObjectMeta: foreignMeta(name), Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: foreignLabels},
		Template: foreignPodTemplate(),
	}}
}

func foreignStatefulSet(name string) client.Object {
	return &appsv1.StatefulSet{ObjectMeta: foreignMeta(name), Spec: appsv1.StatefulSetSpec{
		ServiceName: foreignName,
		Selector:    &metav1.LabelSelector{MatchLabels: foreignLabels},
		Template:    foreignPodTemplate(),
	}}
}

func foreignNetworkPolicy(name string) client.Object {
	return &networkingv1.NetworkPolicy{ObjectMeta: foreignMeta(name), Spec: networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: foreignLabels},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
	}}
}

// foreignSecret carries the managed-by label: the manager's cache holds only
// such Secrets, so this is the Secret the token path reads rather than the one
// it is refused to create (imtoken_collision_test.go).
func foreignSecret(name string) client.Object {
	objectMeta := foreignMeta(name)
	objectMeta.Labels = map[string]string{ManagedByLabel: ManagedByOperator}
	return &corev1.Secret{ObjectMeta: objectMeta, Data: map[string][]byte{imTokenKey: []byte("someone else's")}}
}

// childPaths lists every object that a cluster writes under a fixed name.
func childPaths() []childPath {
	return []childPath{
		{"HA ConfigMap", func(c string) client.Object { return foreignConfigMap(haConfigMapName(c)) }, "HAConfigReconcileFailed"},
		{"governing headless Service", func(c string) client.Object { return foreignService(instancesServiceName(c)) }, "ServiceReconcileFailed"},
		{"member alias Service", func(c string) client.Object { return foreignService(c + "-0") }, "MemberServiceReconcileFailed"},
		{"Instance Manager token Secret", func(c string) client.Object { return foreignSecret(imTokenSecretName(c)) }, "SecretReconcileFailed"},
		{"database NetworkPolicy", func(c string) client.Object { return foreignNetworkPolicy(databaseNetworkPolicyName(c)) }, "NetworkPolicyReconcileFailed"},
		{"Broker NetworkPolicy", func(c string) client.Object { return foreignNetworkPolicy(brokerNetworkPolicyName(c)) }, "NetworkPolicyReconcileFailed"},
		{"StatefulSet", foreignStatefulSet, "StatefulSetReconcileFailed"},
		{"Broker ConfigMap", func(c string) client.Object { return foreignConfigMap(brokerConfigMapName(c)) }, ""},
		{"read-write Broker Deployment", func(c string) client.Object { return foreignDeployment(brokerDeploymentName(c, brokerModeRW)) }, ""},
		{"read-only Broker Deployment", func(c string) client.Object { return foreignDeployment(brokerDeploymentName(c, brokerModeRO)) }, ""},
		{"read-write Broker Service", func(c string) client.Object { return foreignService(rwServiceName(c)) }, ""},
		{"read-only Broker Service", func(c string) client.Object { return foreignService(roServiceName(c)) }, ""},
	}
}

// Who can hold a same-name object apart from the cluster that reconciles.
var childHolders = []struct {
	what   string
	owners func(cluster *databasev1alpha1.CubridCluster) []metav1.OwnerReference
}{
	{"no owner", func(*databasev1alpha1.CubridCluster) []metav1.OwnerReference { return nil }},
	{"another controller", func(*databasev1alpha1.CubridCluster) []metav1.OwnerReference {
		yes := true
		return []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: foreignName, UID: "other-controller", Controller: &yes,
		}}
	}},
	// A cluster of the same name that was deleted: only the UID tells it from
	// this one, so a comparison by name would take the object over.
	{"an earlier cluster of the same name", func(c *databasev1alpha1.CubridCluster) []metav1.OwnerReference {
		yes := true
		return []metav1.OwnerReference{{
			APIVersion: databasev1alpha1.GroupVersion.String(), Kind: ownerKind, Name: c.Name,
			UID: "deleted-cluster", Controller: &yes,
		}}
	}},
	// An owner that is not the controller gives no right to overwrite.
	{"this cluster as an owner that is not the controller", func(c *databasev1alpha1.CubridCluster) []metav1.OwnerReference {
		return []metav1.OwnerReference{{
			APIVersion: databasev1alpha1.GroupVersion.String(), Kind: ownerKind, Name: c.Name, UID: c.UID,
		}}
	}},
}

// Reconcile must not change an object of a name it needs unless its cluster
// controls that object (#657): nothing but a controller reference to this
// cluster's UID makes the object the cluster's.
var _ = Describe("Children that already exist under the cluster's names", func() {
	const refusal = "exists and is not owned by this cluster"

	reconciler := func(name string) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100),
			Prober: &memberProber{master: name + "-0"},
		}
	}
	request := func(cluster *databasev1alpha1.CubridCluster) reconcile.Request {
		return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}
	}
	createCluster := func(cluster *databasev1alpha1.CubridCluster) {
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed())
		})
	}
	createObject := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		})
	}
	// stored returns the object as the API server keeps it. The metadata
	// includes the resource version, so two equal results mean that nothing
	// was written in between, not even a change that left the content alike.
	stored := func(obj client.Object) string {
		fresh := obj.DeepCopyObject().(client.Object)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), fresh)).To(Succeed())
		data, err := json.Marshal(fresh)
		Expect(err).NotTo(HaveOccurred())
		return string(data)
	}
	warnings := func(r *CubridClusterReconciler) []string {
		events := r.Recorder.(*record.FakeRecorder).Events
		var out []string
		for len(events) > 0 {
			if e := <-events; strings.HasPrefix(e, corev1.EventTypeWarning) {
				out = append(out, e)
			}
		}
		return out
	}
	// content is what reconciliation decides about an object: everything but
	// its metadata and status.
	content := func(obj client.Object) map[string]any {
		fresh := obj.DeepCopyObject().(client.Object)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), fresh)).To(Succeed())
		data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(fresh)
		Expect(err).NotTo(HaveOccurred())
		delete(data, "metadata")
		delete(data, "status")
		return data
	}
	expectControlled := func(cluster *databasev1alpha1.CubridCluster, obj client.Object) {
		fresh := obj.DeepCopyObject().(client.Object)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), fresh)).To(Succeed())
		Expect(metav1.IsControlledBy(fresh, cluster)).To(BeTrue(), "the cluster does not control %s", obj.GetName())
	}
	condition := func(cluster *databasev1alpha1.CubridCluster, conditionType string) *metav1.Condition {
		fresh := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), fresh)).To(Succeed())
		return meta.FindStatusCondition(fresh.Status.Conditions, conditionType)
	}

	// behindLabelledCache makes the reconciler read as the manager does: the
	// client finds only objects with the managed-by label, as the manager's
	// cache holds no others, and APIReader finds every object.
	behindLabelledCache := func(r *CubridClusterReconciler) {
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		r.APIReader = k8sClient
		r.Client = interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, cluster := obj.(*databasev1alpha1.CubridCluster); cluster {
					return c.Get(ctx, key, obj, opts...)
				}
				probe := obj.DeepCopyObject().(client.Object)
				if err := c.Get(ctx, key, probe, opts...); err != nil {
					return err
				}
				if probe.GetLabels()[ManagedByLabel] != ManagedByOperator {
					return apierrors.NewNotFound(schema.GroupResource{Resource: "not in the cache"}, key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	}

	for p, path := range childPaths() {
		// The token Secret has a path of its own for a Secret the cache does
		// not hold (imtoken_collision_test.go).
		if _, secret := path.foreign("").(*corev1.Secret); !secret {
			// The cache does not hold an object without the label, so the
			// read finds nothing, yet the name is taken. Without a read past
			// the cache the cluster would try to create the object for ever
			// and report only the API server's AlreadyExists.
			It(fmt.Sprintf("refuses a %s the cache does not hold and leaves it unchanged", path.what), func() {
				name := fmt.Sprintf("child-uncached-%d", p)
				cluster := haCluster(name)
				createCluster(cluster)
				foreign := path.foreign(name)
				createObject(foreign)
				before := stored(foreign)

				r := reconciler(name)
				behindLabelledCache(r)
				_, err := r.Reconcile(ctx, request(cluster))

				Expect(stored(foreign)).To(Equal(before), "the object was changed")
				reported := condition(cluster, conditionBrokerReady)
				if path.reason != "" {
					Expect(err).To(MatchError(ContainSubstring(refusal)))
					reported = condition(cluster, conditionReady)
				}
				Expect(reported.Status).To(Equal(metav1.ConditionFalse))
				Expect(reported.Message).To(And(ContainSubstring(refusal), ContainSubstring(foreign.GetName())))
			})

			// A label removed by hand takes the cluster's own object out of
			// the cache. It is still the cluster's: the write restores the
			// label, and the object returns to the cache.
			It(fmt.Sprintf("restores the label of its own %s the cache stopped holding", path.what), func() {
				name := fmt.Sprintf("child-unlabelled-%d", p)
				cluster := haCluster(name)
				createCluster(cluster)
				r := reconciler(name)
				_, err := r.Reconcile(ctx, request(cluster))
				Expect(err).NotTo(HaveOccurred())

				child := path.foreign(name)
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(child), child)).To(Succeed())
				uid, labels := child.GetUID(), child.GetLabels()
				child.SetLabels(nil)
				Expect(k8sClient.Update(ctx, child)).To(Succeed())

				behindLabelledCache(r)
				_, err = r.Reconcile(ctx, request(cluster))
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(child), child)).To(Succeed())
				Expect(child.GetLabels()).To(Equal(labels))
				Expect(child.GetUID()).To(Equal(uid), "the object was replaced")
			})
		}

		for h, holder := range childHolders {
			It(fmt.Sprintf("refuses a %s held by %s and leaves it unchanged", path.what, holder.what), func() {
				name := fmt.Sprintf("child-refused-%d-%d", p, h)
				cluster := haCluster(name)
				createCluster(cluster)
				foreign := path.foreign(name)
				foreign.SetOwnerReferences(holder.owners(cluster))
				createObject(foreign)
				before := stored(foreign)

				r := reconciler(name)
				_, err := r.Reconcile(ctx, request(cluster))

				Expect(stored(foreign)).To(Equal(before), "the object was changed")
				var reported *metav1.Condition
				if path.reason != "" {
					Expect(err).To(MatchError(ContainSubstring(refusal)))
					reported = condition(cluster, conditionReady)
					Expect(reported.Reason).To(Equal(path.reason))
				} else {
					Expect(err).NotTo(HaveOccurred())
					reported = condition(cluster, conditionBrokerReady)
					Expect(reported.Reason).To(Equal("BrokerReconcileFailed"))
					// The Brokers were not observed, which is what a failed
					// Broker write has always reported.
					Expect(condition(cluster, conditionWriteEndpointReady).Status).To(Equal(metav1.ConditionUnknown))
					Expect(condition(cluster, conditionReadEndpointReady).Status).To(Equal(metav1.ConditionUnknown))
				}
				Expect(reported.Status).To(Equal(metav1.ConditionFalse))
				Expect(reported.Message).To(And(ContainSubstring(refusal), ContainSubstring(foreign.GetName())))
				Expect(warnings(r)).To(ContainElement(And(ContainSubstring(refusal), ContainSubstring(foreign.GetName()))))

				By("reporting a refusal that stays the same only once")
				_, _ = r.Reconcile(ctx, request(cluster))
				Expect(stored(foreign)).To(Equal(before), "the object was changed by the retry")
				Expect(warnings(r)).NotTo(ContainElement(ContainSubstring(refusal)))

				By("creating the object once its name is free")
				Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
				_, err = r.Reconcile(ctx, request(cluster))
				Expect(err).NotTo(HaveOccurred())
				expectControlled(cluster, foreign)
			})
		}

		It(fmt.Sprintf("corrects drift of its own %s and creates it again when it is deleted", path.what), func() {
			name := fmt.Sprintf("child-owned-%d", p)
			cluster := haCluster(name)
			createCluster(cluster)
			r := reconciler(name)
			_, err := r.Reconcile(ctx, request(cluster))
			Expect(err).NotTo(HaveOccurred())

			child := path.foreign(name)
			expectControlled(cluster, child)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(child), child)).To(Succeed())
			want, labels := content(child), child.GetLabels()

			By("someone changing the object")
			child.SetLabels(driftedLabels)
			switch o := child.(type) {
			case *corev1.ConfigMap:
				o.Data = driftedLabels
			case *corev1.Service:
				o.Spec.Ports[0].Port = 18080
			case *appsv1.Deployment:
				replicas := int32(7)
				o.Spec.Replicas = &replicas
			case *appsv1.StatefulSet:
				replicas := int32(7)
				o.Spec.Replicas = &replicas
			case *networkingv1.NetworkPolicy:
				o.Spec.Ingress = nil
			}
			Expect(k8sClient.Update(ctx, child)).To(Succeed())

			_, err = r.Reconcile(ctx, request(cluster))
			Expect(err).NotTo(HaveOccurred())
			Expect(content(child)).To(Equal(want), "the drift was not corrected")
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(child), child)).To(Succeed())
			Expect(child.GetLabels()).To(Equal(labels))
			expectControlled(cluster, child)

			By("someone deleting the object")
			Expect(k8sClient.Delete(ctx, child)).To(Succeed())
			_, err = r.Reconcile(ctx, request(cluster))
			Expect(err).NotTo(HaveOccurred())
			expectControlled(cluster, child)
		})

		// A read that misses an object, as a cache behind the API server does,
		// ends in a create that the API server refuses. The object that won
		// the race is not touched, and the next reconcile sees it. Only the
		// first read misses: a check made by a read of its own would pass, and
		// the read that the write rests on would then find the object.
		It(fmt.Sprintf("leaves a %s alone that a create finds the API server already holds", path.what), func() {
			name := fmt.Sprintf("child-create-race-%d", p)
			cluster := haCluster(name)
			createCluster(cluster)
			foreign := path.foreign(name)
			createObject(foreign)
			before := stored(foreign)

			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			r := reconciler(name)
			r.APIReader = k8sClient
			missed := false
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if !missed && reflect.TypeOf(obj) == reflect.TypeOf(foreign) && key.Name == foreign.GetName() {
						missed = true
						return apierrors.NewNotFound(schema.GroupResource{Resource: "behind the cache"}, key.Name)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			_, _ = r.Reconcile(ctx, request(cluster))
			Expect(missed).To(BeTrue(), "the reconcile never read the object")
			Expect(stored(foreign)).To(Equal(before), "the object was changed")
		})

		// The read and the write of one reconcile are not one step: an object
		// that replaced the cluster's own in between must not be overwritten
		// by a write that rests on the read.
		It(fmt.Sprintf("does not overwrite a %s that replaced its own after it was read", path.what), func() {
			name := fmt.Sprintf("child-write-race-%d", p)
			cluster := haCluster(name)
			createCluster(cluster)
			r := reconciler(name)
			_, err := r.Reconcile(ctx, request(cluster))
			Expect(err).NotTo(HaveOccurred())

			child := path.foreign(name)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(child), child)).To(Succeed())
			child.SetLabels(driftedLabels)
			Expect(k8sClient.Update(ctx, child)).To(Succeed())

			replacement := path.foreign(name)
			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			var before string
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if before == "" && reflect.TypeOf(obj) == reflect.TypeOf(child) && obj.GetName() == child.GetName() {
						Expect(k8sClient.Delete(ctx, child)).To(Succeed())
						createObject(replacement)
						before = stored(replacement)
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			_, _ = r.Reconcile(ctx, request(cluster))
			Expect(before).NotTo(BeEmpty(), "the reconcile wrote no object to race with")
			Expect(stored(replacement)).To(Equal(before), "the object was overwritten")
		})
	}

	// Two paths remove an object rather than write it, so no write brings an
	// object whose label was removed by hand back: the read must find it.
	It("removes its own NetworkPolicy the cache stopped holding when policies are disabled", func() {
		const name = "child-uncached-policy"
		cluster := haCluster(name)
		createCluster(cluster)
		r := reconciler(name)
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())

		policy := &networkingv1.NetworkPolicy{}
		key := client.ObjectKey{Name: databaseNetworkPolicyName(name), Namespace: cluster.Namespace}
		Expect(k8sClient.Get(ctx, key, policy)).To(Succeed())
		policy.Labels = nil
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		disabled := false
		cluster.Spec.NetworkPolicy = &databasev1alpha1.CubridNetworkPolicy{Enabled: &disabled}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		behindLabelledCache(r)
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, policy))).To(BeTrue(), "the NetworkPolicy was left in place")
	})

	It("removes the legacy Broker Deployment it controls that the cache does not hold", func() {
		const name = "child-uncached-legacy"
		cluster := haCluster(name)
		createCluster(cluster)
		legacy := foreignDeployment(legacyBrokerDeploymentName(name))
		yes := true
		legacy.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: databasev1alpha1.GroupVersion.String(), Kind: ownerKind, Name: name, UID: cluster.UID, Controller: &yes,
		}})
		createObject(legacy)

		r := reconciler(name)
		behindLabelledCache(r)
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(legacy), &appsv1.Deployment{}))).
			To(BeTrue(), "the Deployment was left in place")
	})

	// Nothing about the removal of an object without an owner reference starts
	// a reconcile, and a standalone cluster has no periodic one.
	It("tries a refused Broker object of a standalone cluster again without a new event", func() {
		const name = "child-standalone-retry"
		cluster := standaloneCluster(name)
		createCluster(cluster)
		foreign := foreignService(rwServiceName(name))
		createObject(foreign)
		before := stored(foreign)

		r := reconciler(name)
		r.Prober = nil
		result, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(stored(foreign)).To(Equal(before), "the object was changed")
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(condition(cluster, conditionBrokerReady).Reason).To(Equal("BrokerReconcileFailed"))
		Expect(warnings(r)).To(ContainElement(ContainSubstring(refusal)))

		Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
		result, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		expectControlled(cluster, foreign)
	})

	// A failed read of a Broker object is no collision, so it is reported as
	// it always was: by the conditions, with no Warning Event.
	It("records no refusal for a Broker write that failed for another reason", func() {
		const name = "child-broker-write-failed"
		cluster := haCluster(name)
		createCluster(cluster)
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		r := reconciler(name)
		r.Client = interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && key.Name == brokerConfigMapName(name) {
					return apierrors.NewInternalError(fmt.Errorf("etcd is unavailable"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(condition(cluster, conditionBrokerReady).Reason).To(Equal("BrokerReconcileFailed"))
		Expect(warnings(r)).To(BeEmpty())
	})

	// Every operator version wrote the controller reference with the same call,
	// so an object it left behind is this cluster's own: refusing it would stop
	// a running cluster at the upgrade.
	It("accepts an object an earlier operator wrote for this cluster", func() {
		const name = "child-from-earlier-operator"
		cluster := haCluster(name)
		createCluster(cluster)
		yes := true
		secret := foreignSecret(imTokenSecretName(name)).(*corev1.Secret)
		secret.Labels = labelsFor(cluster)
		secret.Annotations = map[string]string{imTokenRotatedAtAnnotation: "2026-01-02T03:04:05Z"}
		secret.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: databasev1alpha1.GroupVersion.String(), Kind: ownerKind, Name: name, UID: cluster.UID,
			Controller: &yes, BlockOwnerDeletion: &yes,
		}}
		createObject(secret)

		r := reconciler(name)
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		got := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, got)).To(Succeed())
		Expect(got.Data[imTokenKey]).To(Equal(secret.Data[imTokenKey]))
		Expect(warnings(r)).To(BeEmpty())
	})
})
