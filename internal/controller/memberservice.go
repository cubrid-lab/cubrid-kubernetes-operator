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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	// serviceRoleLabel tells a member alias Service from the governing one,
	// which carries the same instance labels.
	serviceRoleLabel  = "database.cubrid.io/service-role"
	serviceRoleMember = "member"

	podNameLabel = "statefulset.kubernetes.io/pod-name"

	// Port names shared by the governing and the member Services.
	portNameCubrid  = "cubrid"
	portNameManager = "manager"

	// haHeartbeatPort is CUBRID's HA heartbeat port (UDP). A headless Service
	// forwards nothing, so the entry only documents what peers exchange.
	haHeartbeatPort = 59901
)

func memberServiceLabels(cluster *databasev1alpha1.CubridCluster) map[string]string {
	labels := labelsFor(cluster)
	labels[serviceRoleLabel] = serviceRoleMember
	return labels
}

// reconcileMemberServices keeps one headless alias Service per promotable
// member, named exactly as the pod (ADR-0004). The bare name then resolves to
// that Pod's IP: it is the host name in ha_node_list and the name the
// operator calls the Instance Manager by. A Service of that name which this
// cluster does not own is never taken over; alias Services of members that no
// longer exist are removed.
func (r *CubridClusterReconciler) reconcileMemberServices(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	members := memberNames(cluster, cluster.Spec.Topology.PromotableMembers)
	wanted := make(map[string]bool, len(members))
	for _, name := range members {
		wanted[name] = true

		existing := &corev1.Service{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, existing)
		if err == nil && !metav1.IsControlledBy(existing, cluster) {
			return fmt.Errorf("a Service named %q exists and is not owned by this cluster; "+
				"member %q needs that name for its HA host name", name, name)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get member Service %s: %w", name, err)
		}

		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			svc.Labels = memberServiceLabels(cluster)
			svc.Spec.ClusterIP = corev1.ClusterIPNone
			// Peers and the operator must reach a member that is not Ready yet.
			svc.Spec.PublishNotReadyAddresses = true
			svc.Spec.Selector = map[string]string{podNameLabel: name}
			svc.Spec.Ports = []corev1.ServicePort{
				{Name: portNameCubrid, Protocol: corev1.ProtocolTCP, Port: cubridServerPort, TargetPort: intOrString(cubridServerPort)},
				{Name: portNameManager, Protocol: corev1.ProtocolTCP, Port: instanceManagerPort, TargetPort: intOrString(instanceManagerPort)},
				{
					Name: "heartbeat", Protocol: corev1.ProtocolUDP,
					Port: haHeartbeatPort, TargetPort: intOrString(haHeartbeatPort),
				},
			}
			return controllerutil.SetControllerReference(cluster, svc, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("member Service %s: %w", name, err)
		}
	}

	var services corev1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(cluster.Namespace),
		client.MatchingLabels(memberServiceLabels(cluster))); err != nil {
		return fmt.Errorf("list member Services: %w", err)
	}
	for i := range services.Items {
		svc := &services.Items[i]
		if wanted[svc.Name] || !metav1.IsControlledBy(svc, cluster) {
			continue
		}
		if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete member Service %s: %w", svc.Name, err)
		}
	}
	return nil
}
