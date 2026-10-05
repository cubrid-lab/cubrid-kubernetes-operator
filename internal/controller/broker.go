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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	conditionBrokerReady        = "BrokerReady"
	conditionWriteEndpointReady = "WriteEndpointReady"
	conditionReadEndpointReady  = "ReadEndpointReady"
	conditionRoutingReady       = "RoutingReady"
)

func brokerConfigMapName(cluster string) string        { return cluster + "-broker-config" }
func legacyBrokerDeploymentName(cluster string) string { return cluster + "-broker" }

// brokerDeploymentName is the Deployment of one access mode's Brokers.
func brokerDeploymentName(cluster, mode string) string { return cluster + "-broker-" + mode }

const (
	// brokerReplicas per access mode: losing one Broker Pod leaves another.
	brokerReplicas = 2
	// brokerEntrypoint is the non-root Broker entrypoint in the Instance
	// Manager image (build/instance-manager/broker-entrypoint.sh).
	brokerEntrypoint = "/usr/local/bin/broker-entrypoint.sh"
	// brokerConfMountPath is where the generated configuration is mounted.
	brokerConfMountPath = "/etc/cubrid-broker"
	// brokerDatabasesDir holds the Broker's copy of databases.txt. The image's
	// own database directory is not used: a Broker Pod has no data volume.
	brokerDatabasesDir = "/tmp/cubrid-databases"
)

func rwServiceName(cluster string) string { return cluster + "-rw" }
func roServiceName(cluster string) string { return cluster + "-ro" }

// reconcileBrokerTier reconciles the operator-managed broker tier (ADR-0002): a
// generated config ConfigMap, a broker Deployment, and the -rw/-ro Services
// fronting broker pods (never DB pods). It then sets the broker conditions.
func (r *CubridClusterReconciler) reconcileBrokerTier(ctx context.Context, cluster *databasev1alpha1.CubridCluster, res PrimaryResolution) error {
	if err := r.reconcileBrokerConfig(ctx, cluster); err != nil {
		return err
	}
	if err := r.deleteLegacyBrokerDeployment(ctx, cluster); err != nil {
		return err
	}
	available := map[string]int32{}
	for _, mode := range []string{brokerModeRW, brokerModeRO} {
		dep, err := r.reconcileBrokerDeployment(ctx, cluster, mode)
		if err != nil {
			return err
		}
		available[mode] = dep.Status.AvailableReplicas
	}
	if err := r.reconcileBrokerService(ctx, cluster, rwServiceName(cluster.Name), brokerModeRW); err != nil {
		return err
	}
	if err := r.reconcileBrokerService(ctx, cluster, roServiceName(cluster.Name), brokerModeRO); err != nil {
		return err
	}
	r.setBrokerConditions(cluster, res, available)
	return nil
}

func (r *CubridClusterReconciler) reconcileBrokerConfig(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: brokerConfigMapName(cluster.Name), Namespace: cluster.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = brokerLabelsFor(cluster)
		cm.Data = map[string]string{
			brokerConfKey(brokerModeRW): generateBrokerConf(brokerModeRW),
			brokerConfKey(brokerModeRO): generateBrokerConf(brokerModeRO),
			"databases.txt":             generateBrokerDatabasesTxt(cluster),
		}
		return controllerutil.SetControllerReference(cluster, cm, r.Scheme)
	})
	return err
}

// deleteLegacyBrokerDeployment removes the single read-write Deployment that
// earlier versions created; its Pods served no read-only endpoint. Only a
// Deployment this cluster controls is removed.
func (r *CubridClusterReconciler) deleteLegacyBrokerDeployment(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	old := &appsv1.Deployment{}
	key := types.NamespacedName{Name: legacyBrokerDeploymentName(cluster.Name), Namespace: cluster.Namespace}
	if err := r.Get(ctx, key, old); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(old, cluster) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, old))
}

// brokerPodLabels are the labels of one access mode's Broker Pods, which the
// Deployment and the Service of that mode select.
func brokerPodLabels(cluster *databasev1alpha1.CubridCluster, mode string) map[string]string {
	labels := brokerLabelsFor(cluster)
	labels[brokerAccessModeLabel] = mode
	return labels
}

func (r *CubridClusterReconciler) reconcileBrokerDeployment(ctx context.Context, cluster *databasev1alpha1.CubridCluster, mode string) (*appsv1.Deployment, error) {
	labels := brokerPodLabels(cluster, mode)
	replicas := int32(brokerReplicas)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: brokerDeploymentName(cluster.Name, mode), Namespace: cluster.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		}
		dep.Spec.Replicas = &replicas
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       r.brokerPodSpec(cluster, mode),
		}
		return controllerutil.SetControllerReference(cluster, dep, r.Scheme)
	})
	return dep, err
}

func (r *CubridClusterReconciler) brokerPodSpec(cluster *databasev1alpha1.CubridCluster, mode string) corev1.PodSpec {
	runAsNonRoot := true
	noPrivEscalation := false
	uid := cubridUID
	port := brokerPort(mode)
	return corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &runAsNonRoot,
			// The image's cubrid user; the official image's default is root.
			RunAsUser:      &uid,
			RunAsGroup:     &uid,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		// Prefer different nodes for the Brokers of one mode. It is a
		// preference: with fewer nodes than Brokers they still run.
		Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey:   "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{MatchLabels: brokerPodLabels(cluster, mode)},
				},
			}},
		}},
		Containers: []corev1.Container{{
			Name: componentBroker,
			// The same image as the DB Pods: it carries CUBRID and the non-root
			// Broker entrypoint.
			Image:   r.instanceImage(cluster),
			Command: []string{brokerEntrypoint},
			Env: []corev1.EnvVar{
				{Name: "BROKER_ACCESS_MODE", Value: mode},
				{Name: "BROKER_CONF_DIR", Value: brokerConfMountPath},
				{Name: "CUBRID_DATABASES", Value: brokerDatabasesDir},
			},
			Ports: []corev1.ContainerPort{{Name: mode, ContainerPort: port}},
			// Ready means this Broker listens on its port.
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intOrString(port)},
				},
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				FailureThreshold:    3,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "broker-config", MountPath: brokerConfMountPath, ReadOnly: true},
			},
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             &runAsNonRoot,
				AllowPrivilegeEscalation: &noPrivEscalation,
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}},
		Volumes: []corev1.Volume{{
			Name: "broker-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: brokerConfigMapName(cluster.Name)},
				},
			},
		}},
	}
}

func (r *CubridClusterReconciler) reconcileBrokerService(ctx context.Context, cluster *databasev1alpha1.CubridCluster, name, mode string) error {
	selector := brokerPodLabels(cluster, mode)
	port := brokerPort(mode)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = brokerLabelsFor(cluster)
		svc.Spec.Selector = selector
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: componentBroker, Port: port, TargetPort: intOrString(port)},
		}
		return controllerutil.SetControllerReference(cluster, svc, r.Scheme)
	})
	return err
}

// setBrokerConditions sets the broker/routing conditions (ADR-0002). An
// endpoint is ready when at least one Broker Pod of its access mode is
// available, which means its Broker listens; that the Service object exists
// says nothing. Routing is never claimed safe while the primary is
// ambiguous/unresolved (ADR-0005): an unresolved primary yields
// RoutingReady=False, so the write endpoint is not advertised as safe during
// a split-brain.
func (r *CubridClusterReconciler) setBrokerConditions(cluster *databasev1alpha1.CubridCluster, res PrimaryResolution, available map[string]int32) {
	endpoint := func(conditionType, mode, service string) bool {
		n := available[mode]
		if n > 0 {
			setCondition(cluster, conditionType, metav1.ConditionTrue, "BrokerAvailable",
				fmt.Sprintf("%d of %d %s Broker Pods available behind %s", n, brokerReplicas, mode, service))
			return true
		}
		setCondition(cluster, conditionType, metav1.ConditionFalse, "NoBrokerAvailable",
			fmt.Sprintf("no %s Broker Pod is available behind %s", mode, service))
		return false
	}
	rw := endpoint(conditionWriteEndpointReady, brokerModeRW, rwServiceName(cluster.Name))
	ro := endpoint(conditionReadEndpointReady, brokerModeRO, roServiceName(cluster.Name))
	if rw && ro {
		setCondition(cluster, conditionBrokerReady, metav1.ConditionTrue, "BrokersAvailable",
			"read-write and read-only Brokers are available")
	} else {
		setCondition(cluster, conditionBrokerReady, metav1.ConditionFalse, "BrokersNotAvailable",
			"not every access mode has an available Broker Pod")
	}

	if res.Status == metav1.ConditionTrue && res.CurrentPrimary != "" {
		setCondition(cluster, conditionRoutingReady, metav1.ConditionTrue, "PrimaryResolved",
			"RW routing target resolved: "+res.CurrentPrimary)
	} else {
		reason := "AmbiguousPrimary"
		if res.Reason != "" {
			reason = res.Reason
		}
		setCondition(cluster, conditionRoutingReady, metav1.ConditionFalse, reason,
			"write-endpoint safety not claimed until a single primary is resolved")
	}
}
