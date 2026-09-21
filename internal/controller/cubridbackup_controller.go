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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	conditionAccepted    = "Accepted"
	conditionBackupReady = "Ready"
)

// CubridBackupReconciler reconciles a CubridBackup object (ADR-0007).
type CubridBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters,verbs=get;list;watch

func (r *CubridBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var backup databasev1alpha1.CubridBackup
	if err := r.Get(ctx, req.NamespacedName, &backup); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if backup.Status.Phase == databasev1alpha1.BackupPhaseCompleted ||
		backup.Status.Phase == databasev1alpha1.BackupPhaseFailed {
		return ctrl.Result{}, nil
	}

	if reason, msg, ok := validateBackupSpec(&backup); !ok {
		return r.rejected(ctx, &backup, reason, msg)
	}

	var cluster databasev1alpha1.CubridCluster
	clusterKey := types.NamespacedName{Namespace: backup.Namespace, Name: backup.Spec.ClusterRef.Name}
	if err := r.Get(ctx, clusterKey, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return r.rejected(ctx, &backup, "ClusterNotFound",
				"referenced CubridCluster "+backup.Spec.ClusterRef.Name+" not found")
		}
		return ctrl.Result{}, err
	}

	setBackupCondition(&backup, conditionAccepted, metav1.ConditionTrue, "BackupAccepted",
		"spec is valid and the referenced cluster exists")

	// The object-storage upload workflow and the Instance Manager /v1/operations
	// async contract are not implemented yet; hold the backup Pending and surface
	// the gap explicitly rather than reporting a false Running/Completed.
	if backup.Status.Phase == "" {
		backup.Status.Phase = databasev1alpha1.BackupPhasePending
	}
	setBackupCondition(&backup, conditionBackupReady, metav1.ConditionFalse, "BackupWorkflowNotImplemented",
		"CubridBackup spec is accepted; artifact execution/upload is not yet wired")

	backup.Status.ObservedGeneration = backup.Generation
	if err := r.Status().Update(ctx, &backup); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		log.Error(err, "Failed to update CubridBackup status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// validateBackupSpec enforces the ADR-0007 destination invariants that CRD
// markers cannot express (ObjectStorage requires the objectStorage block).
func validateBackupSpec(backup *databasev1alpha1.CubridBackup) (reason, msg string, ok bool) {
	if backup.Spec.Destination.Type == databasev1alpha1.DestinationObjectStorage &&
		backup.Spec.Destination.ObjectStorage == nil {
		return "InvalidDestination", "destination.objectStorage is required when type is ObjectStorage", false
	}
	return "", "", true
}

func (r *CubridBackupReconciler) rejected(ctx context.Context, backup *databasev1alpha1.CubridBackup, reason, msg string) (ctrl.Result, error) {
	backup.Status.Phase = databasev1alpha1.BackupPhaseFailed
	setBackupCondition(backup, conditionAccepted, metav1.ConditionFalse, reason, msg)
	setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, reason, msg)
	backup.Status.ObservedGeneration = backup.Generation
	if err := r.Status().Update(ctx, backup); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func setBackupCondition(backup *databasev1alpha1.CubridBackup, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&backup.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: backup.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *CubridBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1alpha1.CubridBackup{}).
		Named("cubridbackup").
		Complete(r)
}
