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
	"path"
	"sync"
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
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

const (
	conditionAccepted    = "Accepted"
	conditionBackupReady = "Ready"

	backupStagingRoot = "/var/lib/cubrid/backup-staging"
	backupPollAfter   = 10 * time.Second
)

// CubridBackupReconciler reconciles a CubridBackup object (ADR-0007). It selects
// a safe target, drives the Instance Manager backup operation idempotently, and
// records the artifact — it never runs backupdb itself and never picks a master.
type CubridBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Prober / Backup are nil-safe: nil holds the backup Pending with an explicit
	// reason rather than a false Running/Completed (used by envtest).
	Prober RoleProber
	Backup BackupClient
}

// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters,verbs=get;list;watch

func (r *CubridBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	if backup.Status.OperationRef != "" {
		return r.pollBackup(ctx, &backup)
	}

	// No prober/client wired (envtest): hold Pending with an explicit reason,
	// never a false Running/Completed.
	if r.Prober == nil || r.Backup == nil {
		if backup.Status.Phase == "" {
			backup.Status.Phase = databasev1alpha1.BackupPhasePending
		}
		setBackupCondition(&backup, conditionBackupReady, metav1.ConditionFalse, "BackupWorkflowNotConfigured",
			"no role prober / backup client configured on the controller")
		return r.commit(ctx, &backup, ctrl.Result{})
	}

	return r.startBackup(ctx, &backup, &cluster)
}

// startBackup resolves the cluster HA state, selects a safe target, and starts
// the Instance Manager backup operation with a deterministic idempotency key.
func (r *CubridBackupReconciler) startBackup(ctx context.Context, backup *databasev1alpha1.CubridBackup, cluster *databasev1alpha1.CubridCluster) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	count := cluster.Spec.Topology.PromotableMembers
	members := memberNames(cluster, count)
	obs := r.probeAll(ctx, members, cluster.Namespace)
	res := resolvePrimary(members, obs)

	sel := selectBackupTarget(members, obs, res, backup.Spec.Target.Preference, count <= 1)
	if !sel.Selected {
		// Not terminal: HA may resolve later. Surface why and requeue.
		setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, sel.Reason,
			"no safe backup target yet; waiting for a healthy target")
		return r.commit(ctx, backup, ctrl.Result{RequeueAfter: backupPollAfter})
	}

	req := r.buildBackupRequest(backup, cluster, sel)
	op, err := r.Backup.StartBackup(ctx, sel.Instance, backup.Namespace, idempotencyKey(backup), req)
	if err != nil {
		log.Error(err, "Failed to start backup", "instance", sel.Instance)
		setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, "InstanceManagerUnavailable", err.Error())
		return r.commit(ctx, backup, ctrl.Result{RequeueAfter: backupPollAfter})
	}

	now := metav1.Now()
	backup.Status.Phase = databasev1alpha1.BackupPhaseRunning
	backup.Status.OperationRef = op.ID
	backup.Status.TargetInstance = sel.Instance
	backup.Status.TargetRole = string(sel.Role)
	backup.Status.FallbackUsed = sel.FallbackUsed
	backup.Status.StartedAt = &now
	setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, sel.Reason, "backup started on "+sel.Instance)
	return r.commit(ctx, backup, ctrl.Result{RequeueAfter: backupPollAfter})
}

// pollBackup polls the durable operation and maps its state onto the CR.
func (r *CubridBackupReconciler) pollBackup(ctx context.Context, backup *databasev1alpha1.CubridBackup) (ctrl.Result, error) {
	if r.Backup == nil {
		return r.commit(ctx, backup, ctrl.Result{})
	}
	op, err := r.Backup.GetOperation(ctx, backup.Status.TargetInstance, backup.Namespace, backup.Status.OperationRef)
	if err != nil {
		setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, "InstanceManagerUnavailable", err.Error())
		return r.commit(ctx, backup, ctrl.Result{RequeueAfter: backupPollAfter})
	}

	switch op.State {
	case instancemanager.OpCompleted:
		return r.completed(ctx, backup, op)
	case instancemanager.OpFailed:
		return r.failedBackup(ctx, backup, op.FailureReason)
	case instancemanager.OpUploading:
		backup.Status.Phase = databasev1alpha1.BackupPhaseUploading
		setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, "UploadRunning", "uploading artifact")
	default:
		backup.Status.Phase = databasev1alpha1.BackupPhaseRunning
		setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, "BackupRunning", "backup in progress")
	}
	return r.commit(ctx, backup, ctrl.Result{RequeueAfter: backupPollAfter})
}

func (r *CubridBackupReconciler) completed(ctx context.Context, backup *databasev1alpha1.CubridBackup, op instancemanager.Operation) (ctrl.Result, error) {
	now := metav1.Now()
	backup.Status.Phase = databasev1alpha1.BackupPhaseCompleted
	backup.Status.CompletedAt = &now
	if op.Artifact != nil {
		backup.Status.Artifact = &databasev1alpha1.CubridBackupArtifact{
			URI:            op.Artifact.ManifestURI,
			ManifestDigest: op.Artifact.ManifestDigest,
			SizeBytes:      op.Artifact.SizeBytes,
			Database:       op.Artifact.Database,
			Level:          databasev1alpha1.CubridBackupFull,
		}
	}
	setBackupCondition(backup, conditionBackupReady, metav1.ConditionTrue, "BackupCompleted", "backup completed")
	return r.commit(ctx, backup, ctrl.Result{})
}

func (r *CubridBackupReconciler) failedBackup(ctx context.Context, backup *databasev1alpha1.CubridBackup, reason string) (ctrl.Result, error) {
	now := metav1.Now()
	backup.Status.Phase = databasev1alpha1.BackupPhaseFailed
	backup.Status.CompletedAt = &now
	setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, "BackupFailed", reason)
	return r.commit(ctx, backup, ctrl.Result{})
}

func (r *CubridBackupReconciler) buildBackupRequest(backup *databasev1alpha1.CubridBackup, cluster *databasev1alpha1.CubridCluster, sel TargetSelection) instancemanager.BackupRequest {
	store := backup.Spec.Destination.ObjectStorage
	bucket, prefix := "", ""
	if store != nil {
		bucket = store.Bucket
		prefix = path.Join(store.Prefix, string(cluster.UID), backup.Spec.Database, string(backup.UID))
	}
	return instancemanager.BackupRequest{
		Database:    backup.Spec.Database,
		Destination: path.Join(backupStagingRoot, string(backup.UID)),
		Level:       0,
		Upload: &instancemanager.BackupUpload{
			Bucket:         bucket,
			Prefix:         prefix,
			ClusterUID:     string(cluster.UID),
			CubridVersion:  cluster.Spec.Version,
			SourceInstance: sel.Instance,
			SourceRole:     string(sel.Role),
		},
	}
}

func (r *CubridBackupReconciler) probeAll(ctx context.Context, members []string, namespace string) map[string]RoleObservation {
	obs := make(map[string]RoleObservation, len(members))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			o := r.Prober.ProbeRole(ctx, name, namespace)
			mu.Lock()
			obs[name] = o
			mu.Unlock()
		}(m)
	}
	wg.Wait()
	return obs
}

// idempotencyKey is deterministic per CubridBackup so a retry never starts a
// second backup (ADR-0003/0007): cubridbackup:<ns>:<name>:<uid>.
func idempotencyKey(backup *databasev1alpha1.CubridBackup) string {
	return fmt.Sprintf("cubridbackup:%s:%s:%s", backup.Namespace, backup.Name, backup.UID)
}

func (r *CubridBackupReconciler) commit(ctx context.Context, backup *databasev1alpha1.CubridBackup, result ctrl.Result) (ctrl.Result, error) {
	backup.Status.ObservedGeneration = backup.Generation
	if err := r.Status().Update(ctx, backup); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	return result, nil
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
	return r.commit(ctx, backup, ctrl.Result{})
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
