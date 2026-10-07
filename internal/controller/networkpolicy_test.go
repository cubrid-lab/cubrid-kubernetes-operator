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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// npSource is a Pod that opens a connection: its labels and its namespace's.
type npSource struct {
	pod       map[string]string
	namespace string
}

// npAdmits reports whether the NetworkPolicies of a namespace admit a
// connection from src to a Pod with the labels dst on port/protocol, by the
// NetworkPolicy semantics of the Kubernetes API: a Pod no policy selects
// accepts everything; a selected Pod accepts what any selecting policy
// allows. It judges the stored objects, not what a network plugin enforces.
func npAdmits(policies []networkingv1.NetworkPolicy, dst map[string]string, src npSource,
	port int32, protocol corev1.Protocol) bool {
	selects := func(s *metav1.LabelSelector, set map[string]string) bool {
		sel, err := metav1.LabelSelectorAsSelector(s)
		Expect(err).NotTo(HaveOccurred())
		return sel.Matches(labels.Set(set))
	}
	isolated := false
	for _, p := range policies {
		if !selects(&p.Spec.PodSelector, dst) {
			continue
		}
		hasIngress := false
		for _, t := range p.Spec.PolicyTypes {
			hasIngress = hasIngress || t == networkingv1.PolicyTypeIngress
		}
		if !hasIngress {
			continue
		}
		isolated = true
		for _, rule := range p.Spec.Ingress {
			portOK := len(rule.Ports) == 0
			for _, rp := range rule.Ports {
				proto := corev1.ProtocolTCP
				if rp.Protocol != nil {
					proto = *rp.Protocol
				}
				if proto == protocol && (rp.Port == nil || rp.Port.IntValue() == int(port)) {
					portOK = true
				}
			}
			fromOK := len(rule.From) == 0
			for _, peer := range rule.From {
				if peer.IPBlock != nil {
					continue
				}
				nsOK := src.namespace == p.Namespace
				if peer.NamespaceSelector != nil {
					nsOK = selects(peer.NamespaceSelector,
						map[string]string{"kubernetes.io/metadata.name": src.namespace})
				}
				podOK := peer.PodSelector == nil || selects(peer.PodSelector, src.pod)
				fromOK = fromOK || (nsOK && podOK)
			}
			if portOK && fromOK {
				return true
			}
		}
	}
	return !isolated
}

var _ = Describe("NetworkPolicies of the cluster's Pods (#272)", func() {
	const (
		ns         = "default"
		operatorNS = "cubrid-operator-system"
		appsNS     = "apps"
	)
	someClient := map[string]string{"workload": "client"}
	ctx := context.Background()
	operator := npSource{namespace: operatorNS, pod: map[string]string{
		"control-plane": "controller-manager", "app.kubernetes.io/name": "cubrid-kubernetes-operator",
	}}

	reconcileCluster := func(c *databasev1alpha1.CubridCluster) {
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			IMToken: testIMToken, Prober: &memberProber{master: c.Name + "-0"},
			OperatorNS: operatorNS,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
	}
	create := func(c *databasev1alpha1.CubridCluster) {
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		reconcileCluster(c)
	}
	policies := func() []networkingv1.NetworkPolicy {
		var list networkingv1.NetworkPolicyList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
		return list.Items
	}
	// The Pod labels are read from the workloads the operator created.
	dbPod := func(cluster string) map[string]string {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cluster, Namespace: ns}, sts)).To(Succeed())
		return sts.Spec.Template.Labels
	}
	brokerPods := func(cluster string) []map[string]string {
		out := make([]map[string]string, 0, 2)
		for _, mode := range []string{"rw", "ro"} {
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cluster + "-broker-" + mode, Namespace: ns}, dep)).
				To(Succeed())
			out = append(out, dep.Spec.Template.Labels)
		}
		return out
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP

	It("admits the operator to the Instance Manager and only the cluster's own Pods to the database", func() {
		create(haCluster("np-db"))
		create(haCluster("np-other"))
		db := dbPod("np-db")
		nps := policies()

		Expect(npAdmits(nps, db, operator, 9090, tcp)).To(BeTrue(), "the operator calls the Instance Manager")
		Expect(npAdmits(nps, db, operator, 1523, tcp)).To(BeFalse(), "the operator never opens the database")

		peer := npSource{namespace: ns, pod: db}
		Expect(npAdmits(nps, db, peer, 1523, tcp)).To(BeTrue(), "HA peers replicate through cub_master")
		Expect(npAdmits(nps, db, peer, 59901, udp)).To(BeTrue(), "HA peers exchange heartbeats")
		Expect(npAdmits(nps, db, peer, 9090, tcp)).To(BeFalse(), "a peer does not call the Instance Manager")

		for _, broker := range brokerPods("np-db") {
			b := npSource{namespace: ns, pod: broker}
			Expect(npAdmits(nps, db, b, 1523, tcp)).To(BeTrue(), "the cluster's Brokers open the database")
			Expect(npAdmits(nps, db, b, 9090, tcp)).To(BeFalse())
		}

		strangers := map[string]npSource{
			"a Pod in the same namespace":           {namespace: ns, pod: someClient},
			"a Pod in another namespace":            {namespace: appsNS, pod: someClient},
			"a DB Pod of another cluster":           {namespace: ns, pod: dbPod("np-other")},
			"a Broker Pod of another cluster":       {namespace: ns, pod: brokerPods("np-other")[0]},
			"the operator's labels outside its own": {namespace: appsNS, pod: operator.pod},
		}
		for who, src := range strangers {
			for _, port := range []int32{1523, 9090} {
				Expect(npAdmits(nps, db, src, port, tcp)).To(BeFalse(), "%s reaches TCP %d", who, port)
			}
			Expect(npAdmits(nps, db, src, 59901, udp)).To(BeFalse(), "%s reaches the heartbeat port", who)
		}
	})

	It("admits clients in the cluster's namespace to the Brokers by default, and to nothing else", func() {
		create(haCluster("np-broker"))
		nps := policies()
		local := npSource{namespace: ns, pod: someClient}
		remote := npSource{namespace: appsNS, pod: someClient}
		for _, broker := range brokerPods("np-broker") {
			for _, port := range []int32{33000, 33001} {
				Expect(npAdmits(nps, broker, local, port, tcp)).To(BeTrue(), "a client in the namespace, port %d", port)
				Expect(npAdmits(nps, broker, remote, port, tcp)).To(BeFalse(), "a client elsewhere, port %d", port)
			}
			Expect(npAdmits(nps, broker, local, 1523, tcp)).To(BeFalse())
		}
	})

	It("admits exactly the clients the spec names to the Brokers", func() {
		c := haCluster("np-clients")
		c.Spec.NetworkPolicy = &databasev1alpha1.CubridNetworkPolicy{Clients: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": appsNS}},
		}}}
		create(c)
		nps := policies()
		named := npSource{namespace: appsNS, pod: someClient}
		local := npSource{namespace: ns, pod: someClient}
		for _, broker := range brokerPods("np-clients") {
			Expect(npAdmits(nps, broker, named, 33000, tcp)).To(BeTrue())
			Expect(npAdmits(nps, broker, named, 33001, tcp)).To(BeTrue())
			Expect(npAdmits(nps, broker, local, 33000, tcp)).To(BeFalse(), "the list replaces the namespace default")
		}
		db := dbPod("np-clients")
		Expect(npAdmits(nps, db, named, 1523, tcp)).To(BeFalse(), "a named client still goes through the Brokers")
		Expect(npAdmits(nps, db, named, 9090, tcp)).To(BeFalse())
	})

	It("keeps the policies with the cluster and removes them when they are disabled", func() {
		c := haCluster("np-off")
		create(c)
		owned := func() []string {
			var names []string
			for _, p := range policies() {
				if metav1.IsControlledBy(&p, c) {
					names = append(names, p.Name)
				}
			}
			return names
		}
		Expect(owned()).To(ConsistOf("np-off-database", "np-off-broker"))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: c.Name, Namespace: ns}, c)).To(Succeed())
		disabled := false
		c.Spec.NetworkPolicy = &databasev1alpha1.CubridNetworkPolicy{Enabled: &disabled}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileCluster(c)
		Expect(owned()).To(BeEmpty())
	})
})
