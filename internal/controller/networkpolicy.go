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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// operatorPodLabels are the labels of the operator's Pods
// (config/manager/manager.yaml).
var operatorPodLabels = map[string]string{
	"control-plane": "controller-manager",
	nameLabel:       operatorName,
}

const (
	nameLabel    = "app.kubernetes.io/name"
	operatorName = "cubrid-kubernetes-operator"
)

// namespaceNameLabel is the label the API server sets on every namespace to
// its name.
const namespaceNameLabel = "kubernetes.io/metadata.name"

func databaseNetworkPolicyName(cluster string) string { return cluster + "-database" }
func brokerNetworkPolicyName(cluster string) string   { return cluster + "-broker" }

func networkPolicyEnabled(cluster *databasev1alpha1.CubridCluster) bool {
	np := cluster.Spec.NetworkPolicy
	return np == nil || np.Enabled == nil || *np.Enabled
}

// reconcileNetworkPolicies keeps the ingress NetworkPolicies of the cluster's
// DB and Broker Pods, or removes the ones the cluster owns when they are
// disabled. Egress is not restricted: the DB Pods need DNS, their peers and
// object storage. A network plugin that does not enforce NetworkPolicy
// ignores them.
func (r *CubridClusterReconciler) reconcileNetworkPolicies(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	policies := map[string]networkingv1.NetworkPolicySpec{
		databaseNetworkPolicyName(cluster.Name): r.databaseNetworkPolicy(cluster),
		brokerNetworkPolicyName(cluster.Name):   brokerNetworkPolicy(cluster),
	}
	for name, spec := range policies {
		existing := &networkingv1.NetworkPolicy{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, existing)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get NetworkPolicy %s: %w", name, err)
		}
		found := err == nil
		// A policy of that name which this cluster does not control is never
		// taken over or removed: it is someone else's.
		if found && !metav1.IsControlledBy(existing, cluster) {
			if !networkPolicyEnabled(cluster) {
				continue
			}
			return fmt.Errorf("a NetworkPolicy named %q exists and is not owned by this cluster", name)
		}
		if !networkPolicyEnabled(cluster) {
			if found {
				if err := client.IgnoreNotFound(r.Delete(ctx, existing)); err != nil {
					return fmt.Errorf("delete NetworkPolicy %s: %w", name, err)
				}
			}
			continue
		}
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
			np.Labels = labelsFor(cluster)
			np.Spec = spec
			return controllerutil.SetControllerReference(cluster, np, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("NetworkPolicy %s: %w", name, err)
		}
	}
	return nil
}

// databaseNetworkPolicy admits to the DB Pods: the operator to the Instance
// Manager; the cluster's DB Pods to the server and heartbeat ports, for HA
// replication; and the cluster's Brokers to the server port. The kubelet's
// probes come from the node, which NetworkPolicy implementations commonly
// admit; it is not part of the policy.
func (r *CubridClusterReconciler) databaseNetworkPolicy(cluster *databasev1alpha1.CubridCluster) networkingv1.NetworkPolicySpec {
	peers := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: labelsFor(cluster)}}
	brokers := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: brokerLabelsFor(cluster)}}
	rules := []networkingv1.NetworkPolicyIngressRule{
		{From: []networkingv1.NetworkPolicyPeer{peers, brokers}, Ports: []networkingv1.NetworkPolicyPort{
			npPort(corev1.ProtocolTCP, cubridServerPort),
		}},
		{From: []networkingv1.NetworkPolicyPeer{peers}, Ports: []networkingv1.NetworkPolicyPort{
			npPort(corev1.ProtocolUDP, haHeartbeatPort),
		}},
	}
	// Without the operator's namespace no Pod is admitted to the Instance
	// Manager, rather than every namespace.
	if r.OperatorNS != "" {
		rules = append(rules, networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: r.OperatorNS}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: operatorPodLabels},
			}},
			Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolTCP, instanceManagerPort)},
		})
	}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: labelsFor(cluster)},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress:     rules,
	}
}

// brokerNetworkPolicy admits the clients to the Broker ports of both access
// modes: the peers spec.networkPolicy.clients names, or else the Pods of the
// cluster's namespace.
func brokerNetworkPolicy(cluster *databasev1alpha1.CubridCluster) networkingv1.NetworkPolicySpec {
	clients := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
	if np := cluster.Spec.NetworkPolicy; np != nil && len(np.Clients) > 0 {
		clients = np.Clients
	}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: brokerLabelsFor(cluster)},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: clients,
			Ports: []networkingv1.NetworkPolicyPort{
				npPort(corev1.ProtocolTCP, brokerRWPort),
				npPort(corev1.ProtocolTCP, brokerROPort),
			},
		}},
	}
}

func npPort(protocol corev1.Protocol, port int32) networkingv1.NetworkPolicyPort {
	p := intOrString(port)
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &p}
}
