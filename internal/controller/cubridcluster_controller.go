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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/prometheus/client_golang/prometheus"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/metrics"
)

const (
	// cubridBrokerPort is the default CUBRID broker/CAS port.
	cubridBrokerPort = 33000
	// cubridServerPort is the default CUBRID master/server port.
	cubridServerPort = 1523
	// instanceManagerPort is the Instance Manager HTTP API port (ADR-0003).
	instanceManagerPort = 9090

	// Condition types (ADR-0005/0006).
	conditionReady       = "Ready"
	conditionProgressing = "Progressing"
	conditionHAReady     = "HAReady"

	appName = "cubrid"
)

// CubridClusterReconciler reconciles a CubridCluster object.
type CubridClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Prober is nil-safe: nil skips HA role discovery (HAReady=Unknown).
	Prober RoleProber
	// Restore is nil-safe: nil skips recovery-bootstrap orchestration
	// (BootstrapReady=False/RestoreClientNotConfigured).
	Restore RestoreClient
}

// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives the CubridCluster toward its desired state. Phase 1 scope:
// a headless governing Service and a StatefulSet with a data PVC template, plus
// Ready/Progressing Conditions. HA role discovery, broker tier, backup/restore,
// and failover are later phases (see the accepted ADRs).
func (r *CubridClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cluster databasev1alpha1.CubridCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		// Ignore not-found: the object was deleted; owned resources are GC'd
		// via owner references.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Reconcile the governing headless Service (ADR-0004: stable per-pod DNS).
	if err := r.reconcileHeadlessService(ctx, &cluster); err != nil {
		log.Error(err, "Failed to reconcile headless Service")
		return r.failed(ctx, &cluster, "ServiceReconcileFailed", err)
	}

	// Reconcile the StatefulSet (DB instances + per-ordinal data PVC).
	sts, err := r.reconcileStatefulSet(ctx, &cluster)
	if err != nil {
		log.Error(err, "Failed to reconcile StatefulSet")
		return r.failed(ctx, &cluster, "StatefulSetReconcileFailed", err)
	}

	return r.updateStatus(ctx, &cluster, sts)
}

// instancesServiceName is the governing headless Service (<cluster>-instances).
func instancesServiceName(name string) string { return name + "-instances" }

// labelsFor returns the standard selector labels for a cluster's DB instances.
func labelsFor(cluster *databasev1alpha1.CubridCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       appName,
		"app.kubernetes.io/instance":   cluster.Name,
		"app.kubernetes.io/component":  "database",
		"app.kubernetes.io/managed-by": "cubrid-kubernetes-operator",
	}
}

func (r *CubridClusterReconciler) reconcileHeadlessService(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instancesServiceName(cluster.Name),
			Namespace: cluster.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labelsFor(cluster)
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		// publishNotReadyAddresses keeps peer DNS resolvable during startup/
		// failover when a pod is briefly NotReady (ADR-0004).
		svc.Spec.PublishNotReadyAddresses = true
		svc.Spec.Selector = labelsFor(cluster)
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "cubrid", Port: cubridServerPort, TargetPort: intOrString(cubridServerPort)},
			{Name: "broker", Port: cubridBrokerPort, TargetPort: intOrString(cubridBrokerPort)},
			{Name: "manager", Port: instanceManagerPort, TargetPort: intOrString(instanceManagerPort)},
		}
		return controllerutil.SetControllerReference(cluster, svc, r.Scheme)
	})
	return err
}

func (r *CubridClusterReconciler) reconcileStatefulSet(ctx context.Context, cluster *databasev1alpha1.CubridCluster) (*appsv1.StatefulSet, error) {
	replicas := cluster.Spec.Topology.PromotableMembers
	labels := labelsFor(cluster)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = labels
		// Immutable fields (selector, volumeClaimTemplates, serviceName) are only
		// set on creation; CreateOrUpdate skips them on update by leaving the
		// existing values in place for a freshly-fetched object.
		if sts.CreationTimestamp.IsZero() {
			sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
			sts.Spec.ServiceName = instancesServiceName(cluster.Name)
			sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{r.dataPVCTemplate(cluster)}
		}
		sts.Spec.Replicas = &replicas
		// OnDelete: the operator owns pod replacement sequencing (ADR-0003/0009).
		sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
		sts.Spec.PersistentVolumeClaimRetentionPolicy = pvcRetentionPolicy(cluster)
		sts.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       r.podSpec(cluster),
		}
		return controllerutil.SetControllerReference(cluster, sts, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	return sts, nil
}

// pvcRetentionPolicy maps spec.storage.retentionPolicy to StatefulSet PVC
// retention. WhenScaled is ALWAYS Retain: ADR-0006 forbids implicit PVC
// deletion on scale-down, even when retentionPolicy is Delete.
func pvcRetentionPolicy(cluster *databasev1alpha1.CubridCluster) *appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy {
	whenDeleted := appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	if cluster.Spec.Storage.RetentionPolicy == "Delete" {
		whenDeleted = appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	}
	return &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: whenDeleted,
		WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}
}

func (r *CubridClusterReconciler) dataPVCTemplate(cluster *databasev1alpha1.CubridCluster) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: cluster.Spec.Storage.Data.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: cluster.Spec.Storage.Data.Size},
			},
		},
	}
}

func (r *CubridClusterReconciler) podSpec(cluster *databasev1alpha1.CubridCluster) corev1.PodSpec {
	image := "cubrid/cubrid:11.4"
	if cluster.Spec.Image != nil && cluster.Spec.Image.Repository != "" {
		image = cluster.Spec.Image.Repository
		if cluster.Spec.Image.Tag != "" {
			image = fmt.Sprintf("%s:%s", cluster.Spec.Image.Repository, cluster.Spec.Image.Tag)
		}
	}
	runAsNonRoot := true
	noPrivEscalation := false
	gracePeriod := int64(120)
	return corev1.PodSpec{
		// terminationGracePeriodSeconds >= 120s for ordered HA shutdown (ADR-0003).
		TerminationGracePeriodSeconds: &gracePeriod,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   &runAsNonRoot,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:      appName,
			Image:     image,
			Resources: cluster.Spec.Resources,
			// preStop triggers the ADR-0003 ordered graceful shutdown via the
			// local Instance Manager (loopback is token-exempt).
			Lifecycle: preStopShutdown(cluster),
			Ports: []corev1.ContainerPort{
				{Name: "cubrid", ContainerPort: cubridServerPort},
				{Name: "broker", ContainerPort: cubridBrokerPort},
				{Name: "manager", ContainerPort: instanceManagerPort},
			},
			// Readiness reflects DB-instance readiness only (Instance Manager
			// /readyz). Cluster HAReady is a SEPARATE Condition and must never
			// gate Pod readiness, else failover instability evicts every pod
			// from Services (ADR-0003/0005, #14).
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        httpGet("/livez"),
				InitialDelaySeconds: 30,
				PeriodSeconds:       10,
				FailureThreshold:    6,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        httpGet("/readyz"),
				InitialDelaySeconds: 10,
				PeriodSeconds:       5,
				FailureThreshold:    3,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "data", MountPath: "/var/lib/cubrid"},
			},
			// Pod Security Standards "restricted" (#18).
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             &runAsNonRoot,
				AllowPrivilegeEscalation: &noPrivEscalation,
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}},
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (r *CubridClusterReconciler) event(cluster *databasev1alpha1.CubridCluster, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(cluster, eventType, reason, msg)
	}
}

func httpGet(path string) corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intOrString(instanceManagerPort)},
	}
}

func preStopShutdown(cluster *databasev1alpha1.CubridCluster) *corev1.Lifecycle {
	db := ""
	if len(cluster.Spec.Databases) > 0 {
		db = cluster.Spec.Databases[0].Name
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/shutdown?database=%s", instanceManagerPort, db)
	return &corev1.Lifecycle{
		PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/sh", "-c",
					fmt.Sprintf("curl -fsS -m 110 -X POST '%s' || true", url)},
			},
		},
	}
}

// updateStatus computes Conditions from the StatefulSet's observed state.
func (r *CubridClusterReconciler) updateStatus(ctx context.Context, cluster *databasev1alpha1.CubridCluster, sts *appsv1.StatefulSet) (ctrl.Result, error) {
	desired := cluster.Spec.Topology.PromotableMembers
	ready := sts.Status.ReadyReplicas

	cluster.Status.ObservedGeneration = cluster.Generation

	// Recovery bootstrap gates Ready: while restoring, the operator must never
	// advertise a half-restored DB as healthy (ADR-0008).
	if recoveryActive(cluster) {
		res, active := r.reconcileRecovery(ctx, cluster)
		if active {
			setCondition(cluster, conditionReady, metav1.ConditionFalse, "BootstrapRecoveryInProgress",
				"restoring from backup; cluster not ready")
			setCondition(cluster, conditionProgressing, metav1.ConditionTrue, "BootstrapRecoveryInProgress",
				"restoring from backup")
			if err := r.Status().Update(ctx, cluster); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				return ctrl.Result{}, err
			}
			return res, nil
		}
	}

	if ready >= desired && desired > 0 {
		setCondition(cluster, conditionReady, metav1.ConditionTrue, "ClusterReady",
			fmt.Sprintf("%d/%d instances ready", ready, desired))
		setCondition(cluster, conditionProgressing, metav1.ConditionFalse, "Reconciled", "cluster reconciled")
		if !meta.IsStatusConditionTrue(cluster.Status.Conditions, conditionReady) {
			r.event(cluster, corev1.EventTypeNormal, "ClusterReady",
				fmt.Sprintf("all %d instances ready", desired))
		}
	} else {
		setCondition(cluster, conditionReady, metav1.ConditionFalse, "InstancesNotReady",
			fmt.Sprintf("%d/%d instances ready", ready, desired))
		setCondition(cluster, conditionProgressing, metav1.ConditionTrue, "InstancesStarting",
			fmt.Sprintf("waiting for %d/%d instances", ready, desired))
	}

	labels := prometheus.Labels{"namespace": cluster.Namespace, "cluster": cluster.Name}
	metrics.ClusterInstances.With(labels).Set(float64(desired))
	metrics.InstanceReady.With(labels).Set(float64(ready))
	metrics.ClusterReady.With(labels).Set(boolToFloat(ready >= desired && desired > 0))

	// HAReady is separate from Ready and from Pod readiness (#14). Role
	// discovery polls each member's Instance Manager /v1/role (ADR-0003/0005).
	if !cluster.Spec.HighAvailability.Enabled {
		setCondition(cluster, conditionHAReady, metav1.ConditionFalse, "HADisabled",
			"highAvailability.enabled is false")
	} else if r.Prober == nil {
		setCondition(cluster, conditionHAReady, metav1.ConditionUnknown, "RoleDiscoveryDisabled",
			"no role prober configured")
	} else {
		res := r.reconcileHAStatus(ctx, cluster, desired)
		// Reconcile the broker tier and set routing conditions from the same
		// safety-first primary resolution (ADR-0002/0005).
		if err := r.reconcileBrokerTier(ctx, cluster, res); err != nil {
			logf.FromContext(ctx).Error(err, "Failed to reconcile broker tier")
			setCondition(cluster, conditionBrokerReady, metav1.ConditionFalse, "BrokerReconcileFailed", err.Error())
		}
	}

	if err := r.Status().Update(ctx, cluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// failed records a failure condition and returns the error for requeue.
func (r *CubridClusterReconciler) failed(ctx context.Context, cluster *databasev1alpha1.CubridCluster, reason string, cause error) (ctrl.Result, error) {
	setCondition(cluster, conditionReady, metav1.ConditionFalse, reason, cause.Error())
	setCondition(cluster, conditionProgressing, metav1.ConditionTrue, reason, cause.Error())
	r.event(cluster, corev1.EventTypeWarning, reason, cause.Error())
	// Best-effort status update; return the original cause for requeue.
	_ = r.Status().Update(ctx, cluster)
	return ctrl.Result{}, cause
}

func setCondition(cluster *databasev1alpha1.CubridCluster, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cluster.Generation,
	})
}

func intOrString(port int32) intstr.IntOrString { return intstr.FromInt32(port) }

// SetupWithManager sets up the controller with the Manager.
func (r *CubridClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1alpha1.CubridCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named("cubridcluster").
		Complete(r)
}
