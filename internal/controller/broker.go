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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	conditionBrokerReady        = "BrokerReady"
	conditionWriteEndpointReady = "WriteEndpointReady"
	conditionReadEndpointReady  = "ReadEndpointReady"
	conditionRoutingReady       = "RoutingReady"
)

func brokerConfigMapName(cluster string) string  { return cluster + "-broker-config" }
func brokerDeploymentName(cluster string) string { return cluster + "-broker" }
func rwServiceName(cluster string) string        { return cluster + "-rw" }
func roServiceName(cluster string) string        { return cluster + "-ro" }

// reconcileBrokerTier reconciles the operator-managed broker tier (ADR-0002): a
// generated config ConfigMap, a broker Deployment, and the -rw/-ro Services
// fronting broker pods (never DB pods). It then sets the broker conditions.
func (r *CubridClusterReconciler) reconcileBrokerTier(ctx context.Context, cluster *databasev1alpha1.CubridCluster, res PrimaryResolution) error {
	if err := r.reconcileBrokerConfig(ctx, cluster); err != nil {
		return err
	}
	if err := r.reconcileBrokerDeployment(ctx, cluster); err != nil {
		return err
	}
	if err := r.reconcileBrokerService(ctx, cluster, rwServiceName(cluster.Name), "rw", brokerRWPort); err != nil {
		return err
	}
	if err := r.reconcileBrokerService(ctx, cluster, roServiceName(cluster.Name), "ro", brokerROPort); err != nil {
		return err
	}
	r.setBrokerConditions(cluster, res)
	return nil
}

func (r *CubridClusterReconciler) reconcileBrokerConfig(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: brokerConfigMapName(cluster.Name), Namespace: cluster.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = brokerLabelsFor(cluster)
		cm.Data = map[string]string{
			"cubrid_broker.conf": generateBrokerConf(),
			"databases.txt":      generateBrokerDatabasesTxt(cluster),
		}
		return controllerutil.SetControllerReference(cluster, cm, r.Scheme)
	})
	return err
}

func (r *CubridClusterReconciler) reconcileBrokerDeployment(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	labels := brokerLabelsFor(cluster)
	labels[brokerAccessModeLabel] = "rw"
	replicas := int32(1)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: brokerDeploymentName(cluster.Name), Namespace: cluster.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = brokerLabelsFor(cluster)
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		}
		dep.Spec.Replicas = &replicas
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       r.brokerPodSpec(cluster),
		}
		return controllerutil.SetControllerReference(cluster, dep, r.Scheme)
	})
	return err
}

func (r *CubridClusterReconciler) brokerPodSpec(cluster *databasev1alpha1.CubridCluster) corev1.PodSpec {
	image := "cubrid/cubrid:11.4"
	if cluster.Spec.Image != nil && cluster.Spec.Image.Repository != "" {
		image = cluster.Spec.Image.Repository
		if cluster.Spec.Image.Tag != "" {
			image = cluster.Spec.Image.Repository + ":" + cluster.Spec.Image.Tag
		}
	}
	runAsNonRoot := true
	noPrivEscalation := false
	return corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   &runAsNonRoot,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:  componentBroker,
			Image: image,
			Ports: []corev1.ContainerPort{
				{Name: "rw", ContainerPort: brokerRWPort},
				{Name: "ro", ContainerPort: brokerROPort},
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intOrString(brokerRWPort)},
				},
				InitialDelaySeconds: 10,
				PeriodSeconds:       5,
				FailureThreshold:    3,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "broker-config", MountPath: "/etc/cubrid-broker", ReadOnly: true},
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

func (r *CubridClusterReconciler) reconcileBrokerService(ctx context.Context, cluster *databasev1alpha1.CubridCluster, name, accessMode string, port int32) error {
	selector := brokerLabelsFor(cluster)
	selector[brokerAccessModeLabel] = accessMode

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

// setBrokerConditions sets the broker/routing conditions (ADR-0002). Routing is
// never claimed safe while the primary is ambiguous/unresolved (ADR-0005): an
// unresolved primary yields RoutingReady=False, so the write endpoint is not
// advertised as safe during a split-brain.
func (r *CubridClusterReconciler) setBrokerConditions(cluster *databasev1alpha1.CubridCluster, res PrimaryResolution) {
	setCondition(cluster, conditionBrokerReady, metav1.ConditionTrue, "BrokerReconciled",
		"broker tier reconciled")
	setCondition(cluster, conditionWriteEndpointReady, metav1.ConditionTrue, "WriteEndpointReconciled",
		"-rw Service reconciled")
	setCondition(cluster, conditionReadEndpointReady, metav1.ConditionTrue, "ReadEndpointReconciled",
		"-ro Service reconciled")

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
