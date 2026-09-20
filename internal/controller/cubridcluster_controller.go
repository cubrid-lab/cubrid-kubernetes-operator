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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
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
)

// CubridClusterReconciler reconciles a CubridCluster object.
type CubridClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
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
		"app.kubernetes.io/name":       "cubrid",
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
	return corev1.PodSpec{
		// terminationGracePeriodSeconds >= 120s for ordered HA shutdown (ADR-0003).
		TerminationGracePeriodSeconds: ptr(int64(120)),
		SecurityContext:               &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot},
		Containers: []corev1.Container{{
			Name:      "cubrid",
			Image:     image,
			Resources: cluster.Spec.Resources,
			Ports: []corev1.ContainerPort{
				{Name: "cubrid", ContainerPort: cubridServerPort},
				{Name: "broker", ContainerPort: cubridBrokerPort},
				{Name: "manager", ContainerPort: instanceManagerPort},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "data", MountPath: "/var/lib/cubrid"},
			},
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             &runAsNonRoot,
				AllowPrivilegeEscalation: ptr(false),
			},
		}},
	}
}

// updateStatus computes Conditions from the StatefulSet's observed state.
func (r *CubridClusterReconciler) updateStatus(ctx context.Context, cluster *databasev1alpha1.CubridCluster, sts *appsv1.StatefulSet) (ctrl.Result, error) {
	desired := cluster.Spec.Topology.PromotableMembers
	ready := sts.Status.ReadyReplicas

	cluster.Status.ObservedGeneration = cluster.Generation

	if ready >= desired && desired > 0 {
		setCondition(cluster, conditionReady, metav1.ConditionTrue, "ClusterReady",
			fmt.Sprintf("%d/%d instances ready", ready, desired))
		setCondition(cluster, conditionProgressing, metav1.ConditionFalse, "Reconciled", "cluster reconciled")
	} else {
		setCondition(cluster, conditionReady, metav1.ConditionFalse, "InstancesNotReady",
			fmt.Sprintf("%d/%d instances ready", ready, desired))
		setCondition(cluster, conditionProgressing, metav1.ConditionTrue, "InstancesStarting",
			fmt.Sprintf("waiting for %d/%d instances", ready, desired))
	}

	if err := r.Status().Update(ctx, cluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// failed records a failure condition and returns the error for requeue.
func (r *CubridClusterReconciler) failed(ctx context.Context, cluster *databasev1alpha1.CubridCluster, reason string, cause error) (ctrl.Result, error) {
	setCondition(cluster, conditionReady, metav1.ConditionFalse, reason, cause.Error())
	setCondition(cluster, conditionProgressing, metav1.ConditionTrue, reason, cause.Error())
	// Best-effort status update; return the original cause for requeue.
	_ = r.Status().Update(ctx, cluster)
	return ctrl.Result{}, cause
}

func setCondition(cluster *databasev1alpha1.CubridCluster, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cluster.Generation,
	}
	for i := range cluster.Status.Conditions {
		if cluster.Status.Conditions[i].Type == condType {
			// Preserve LastTransitionTime unless the status actually changed.
			if cluster.Status.Conditions[i].Status == status {
				meta.LastTransitionTime = cluster.Status.Conditions[i].LastTransitionTime
			} else {
				meta.LastTransitionTime = metav1.Now()
			}
			cluster.Status.Conditions[i] = meta
			return
		}
	}
	meta.LastTransitionTime = metav1.Now()
	cluster.Status.Conditions = append(cluster.Status.Conditions, meta)
}

func intOrString(port int32) intstr.IntOrString { return intstr.FromInt32(port) }

func ptr[T any](v T) *T { return &v }

// SetupWithManager sets up the controller with the Manager.
func (r *CubridClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1alpha1.CubridCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Named("cubridcluster").
		Complete(r)
}
