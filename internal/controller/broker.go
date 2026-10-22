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
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

// brokerMaxApplServers is the Brokers' MAX_NUM_APPL_SERVER: the manager's
// --broker-max-appl-servers, or DefaultBrokerMaxApplServers when unset.
func (r *CubridClusterReconciler) brokerMaxApplServers() int {
	if r.BrokerMaxApplServers > 0 {
		return r.BrokerMaxApplServers
	}
	return DefaultBrokerMaxApplServers
}

func rwServiceName(cluster string) string { return cluster + "-rw" }
func roServiceName(cluster string) string { return cluster + "-ro" }

// reconcileBrokerTier reconciles the operator-managed broker tier (ADR-0002): a
// generated config ConfigMap, a broker Deployment per access mode, and the
// -rw/-ro Services fronting broker pods (never DB pods). It returns how many
// Broker Pods of each access mode are available. An HA and a standalone
// cluster get the same tier; only how their routing is judged differs.
func (r *CubridClusterReconciler) reconcileBrokerTier(ctx context.Context, cluster *databasev1alpha1.CubridCluster, image string) (map[string]int32, error) {
	config := brokerConfigData(cluster, r.brokerMaxApplServers())
	if err := r.reconcileBrokerConfig(ctx, cluster, config); err != nil {
		return nil, err
	}
	if err := r.deleteLegacyBrokerDeployment(ctx, cluster); err != nil {
		return nil, err
	}
	available := map[string]int32{}
	for _, mode := range []string{brokerModeRW, brokerModeRO} {
		dep, err := r.reconcileBrokerDeployment(ctx, cluster, mode, image, brokerConfigHash(config, mode))
		if err != nil {
			return nil, err
		}
		available[mode] = dep.Status.AvailableReplicas
	}
	if err := r.reconcileBrokerService(ctx, cluster, rwServiceName(cluster.Name), brokerModeRW); err != nil {
		return nil, err
	}
	if err := r.reconcileBrokerService(ctx, cluster, roServiceName(cluster.Name), brokerModeRO); err != nil {
		return nil, err
	}
	return available, nil
}

// brokerTier reconciles the Broker tier with the image the DB Pods run and
// reports whether the caller is to set the conditions from what it returns.
// A conflicting write that is still young, or a write the API server refused
// because the namespace is being deleted, leaves the conditions as they are;
// after a conflict the next reconcile writes again. Any other failure marks
// the Brokers unobserved (res is nil for a standalone cluster).
func (r *CubridClusterReconciler) brokerTier(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	sts *appsv1.StatefulSet, res *PrimaryResolution) (map[string]int32, bool) {
	available, err := r.reconcileBrokerTier(ctx, cluster, r.imageToRun(cluster, sts))
	young, err := r.lastingConflict(cluster, brokerWrites, err)
	switch {
	case young:
		logf.FromContext(ctx).V(1).Info("Retrying broker tier after a conflicting write")
	case namespaceTerminating(err):
		logf.FromContext(ctx).V(1).Info("Skipped broker tier because the namespace is being deleted")
	case err != nil:
		logf.FromContext(ctx).Error(err, "Failed to reconcile broker tier")
		setBrokerConditionsUnobserved(cluster, res, err)
	default:
		return available, true
	}
	return nil, false
}

func (r *CubridClusterReconciler) reconcileBrokerConfig(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	data map[string]string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: brokerConfigMapName(cluster.Name), Namespace: cluster.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = brokerLabelsFor(cluster)
		cm.Data = data
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

// reconcileBrokerDeployment keeps the Deployment of one access mode's Brokers.
//
// A Broker reads its configuration once, when its container starts: the
// entrypoint copies it from the mounted ConfigMap. The hash of what this
// mode's Brokers read is therefore an annotation of the Pod template, so that
// a change of it replaces them and an unchanged configuration replaces
// nothing. They are replaced one at a time, a new Broker being available
// before an old one is stopped, so the mode never has fewer Brokers than
// asked for.
func (r *CubridClusterReconciler) reconcileBrokerDeployment(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	mode, image, configHash string) (*appsv1.Deployment, error) {
	labels := brokerPodLabels(cluster, mode)
	replicas := int32(brokerReplicas)
	noneUnavailable, oneMore := intstr.FromInt32(0), intstr.FromInt32(1)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: brokerDeploymentName(cluster.Name, mode), Namespace: cluster.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		}
		dep.Spec.Replicas = &replicas
		dep.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: &noneUnavailable, MaxSurge: &oneMore,
			},
		}
		if err := setPodTemplate(dep, &dep.Spec.Template, corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      labels,
				Annotations: map[string]string{brokerConfigHashAnnotation: configHash},
			},
			Spec: r.brokerPodSpec(cluster, mode, image),
		}); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(cluster, dep, r.Scheme)
	})
	return dep, err
}

func (r *CubridClusterReconciler) brokerPodSpec(cluster *databasev1alpha1.CubridCluster, mode, image string) corev1.PodSpec {
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
			// The image the DB Pods run: it carries CUBRID and the non-root
			// Broker entrypoint, and an image that is not accepted for the
			// database is not used for its Brokers either.
			Image:   image,
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
			{Name: componentBroker, Protocol: corev1.ProtocolTCP, Port: port, TargetPort: intOrString(port)},
		}
		return controllerutil.SetControllerReference(cluster, svc, r.Scheme)
	})
	return err
}

// setBrokerConditions sets the broker/routing conditions of an HA cluster
// (ADR-0002). Routing is never claimed safe while the primary is
// ambiguous/unresolved (ADR-0005): an unresolved primary yields
// RoutingReady=False, so the write endpoint is not advertised as safe during
// a split-brain. With a resolved primary, routing is ready when a read-write
// Broker is available to carry writes to it.
func (r *CubridClusterReconciler) setBrokerConditions(cluster *databasev1alpha1.CubridCluster, res PrimaryResolution, available map[string]int32) {
	rw := setEndpointConditions(cluster, available)
	switch {
	case !setRoutingUnresolved(cluster, res):
	case !rw:
		// A resolved master without a Broker in front of it is no route.
		setCondition(cluster, conditionRoutingReady, metav1.ConditionFalse, "NoBrokerAvailable",
			"the primary is "+res.CurrentPrimary+", but no rw Broker Pod is available to route writes to it")
	default:
		setCondition(cluster, conditionRoutingReady, metav1.ConditionTrue, "PrimaryResolved",
			"RW routing target resolved: "+res.CurrentPrimary)
	}
}

// setStandaloneBrokerConditions sets the broker/routing conditions of a
// standalone cluster. Its Brokers have one server to connect to and no HA
// role to seek (docs/poc/RESULTS.md, POC-33): write routing is ready when that
// server's member is ready, which for a standalone member means its server
// runs, and a read-write Broker is available. No primary is resolved for it.
func setStandaloneBrokerConditions(cluster *databasev1alpha1.CubridCluster, serverReady bool, available map[string]int32) {
	rw := setEndpointConditions(cluster, available)
	server := memberNames(cluster, cluster.Spec.Topology.PromotableMembers)[0]
	switch {
	case !serverReady:
		setCondition(cluster, conditionRoutingReady, metav1.ConditionFalse, "InstancesNotReady",
			"the standalone server "+server+" is not ready")
	case !rw:
		setCondition(cluster, conditionRoutingReady, metav1.ConditionFalse, "NoBrokerAvailable",
			"the standalone server "+server+" is ready, but no rw Broker Pod is available to route writes to it")
	default:
		setCondition(cluster, conditionRoutingReady, metav1.ConditionTrue, "StandaloneServerReady",
			"RW routing target: the standalone server "+server)
	}
}

// setEndpointConditions sets BrokerReady, WriteEndpointReady and
// ReadEndpointReady, and reports whether a read-write Broker is available. An
// endpoint is ready when at least one Broker Pod of its access mode is
// available, which means its Broker listens; that the Service object exists
// says nothing.
func setEndpointConditions(cluster *databasev1alpha1.CubridCluster, available map[string]int32) bool {
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
	return rw
}

// setRoutingUnresolved reports whether a single primary is resolved. When
// none is, it sets RoutingReady=False with the primary's reason: that is
// known whatever is known about the Brokers.
func setRoutingUnresolved(cluster *databasev1alpha1.CubridCluster, res PrimaryResolution) bool {
	if res.Status == metav1.ConditionTrue && res.CurrentPrimary != "" {
		return true
	}
	reason := "AmbiguousPrimary"
	if res.Reason != "" {
		reason = res.Reason
	}
	setCondition(cluster, conditionRoutingReady, metav1.ConditionFalse, reason,
		"write-endpoint safety not claimed until a single primary is resolved")
	return false
}

// setBrokerConditionsUnobserved is for a reconcile in which the Broker tier
// could not be reconciled, so the Brokers were not observed. What an earlier
// reconcile saw of them is not kept: the endpoints are Unknown, and so is the
// routing unless the primary is unresolved, which is reported either way.
// Nothing is invented: Unknown says that the Brokers were not looked at.
// res is nil for a standalone cluster, which has no primary: its routing is
// then Unknown.
func setBrokerConditionsUnobserved(cluster *databasev1alpha1.CubridCluster, res *PrimaryResolution, cause error) {
	const reason = "BrokerReconcileFailed"
	msg := "the Brokers were not observed in this reconcile: " + cause.Error()
	setCondition(cluster, conditionBrokerReady, metav1.ConditionFalse, reason, cause.Error())
	setCondition(cluster, conditionWriteEndpointReady, metav1.ConditionUnknown, reason, msg)
	setCondition(cluster, conditionReadEndpointReady, metav1.ConditionUnknown, reason, msg)
	if res == nil || setRoutingUnresolved(cluster, *res) {
		setCondition(cluster, conditionRoutingReady, metav1.ConditionUnknown, reason, msg)
	}
}
