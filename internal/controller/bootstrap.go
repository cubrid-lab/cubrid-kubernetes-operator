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
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

const (
	conditionBootstrapReady = "BootstrapReady"
	restorePollAfter        = 10 * time.Second
	restoreStagingRoot      = "/var/lib/cubrid/restore-staging"
	restoreTargetRoot       = "/var/lib/cubrid/databases"
)

// recoveryActive reports whether this cluster is a recovery-bootstrap that has
// not yet completed. While active, the operator must not report Ready
// (ADR-0008: never advertise a half-restored DB as healthy).
func recoveryActive(cluster *databasev1alpha1.CubridCluster) bool {
	if cluster.Spec.Bootstrap == nil || cluster.Spec.Bootstrap.Recovery == nil {
		return false
	}
	return cluster.Status.Bootstrap == nil || cluster.Status.Bootstrap.Phase != databasev1alpha1.BootstrapComplete
}

// reconcileRecovery drives the ADR-0008 restore-as-bootstrap on the initial
// master. It starts the restore operation once (idempotently), polls it, and
// records status.bootstrap. It returns a requeue result while in progress; the
// caller gates Ready on BootstrapReady until Complete.
func (r *CubridClusterReconciler) reconcileRecovery(ctx context.Context, cluster *databasev1alpha1.CubridCluster) (ctrl.Result, bool) {
	rec := cluster.Spec.Bootstrap.Recovery
	target := fmt.Sprintf("%s-0", cluster.Name)
	db := ""
	if len(cluster.Spec.Databases) > 0 {
		db = cluster.Spec.Databases[0].Name
	}

	if cluster.Status.Bootstrap == nil {
		cluster.Status.Bootstrap = &databasev1alpha1.BootstrapStatus{
			Mode:         "Recovery",
			Phase:        databasev1alpha1.BootstrapPreparing,
			ManifestURI:  rec.ManifestURI,
			TargetMember: target,
		}
	}

	if r.Restore == nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "RestoreClientNotConfigured",
			"no restore client configured on the controller")
		return ctrl.Result{}, true
	}

	// Start once: begin the restore when no operation is recorded yet.
	if cluster.Status.Bootstrap.OperationID == "" {
		bucket, prefix, err := parseManifestURI(rec.ManifestURI)
		if err != nil {
			r.recoveryFailed(cluster, "InvalidManifestURI", err.Error())
			return ctrl.Result{}, true
		}
		req := instancemanager.RestoreRequest{
			Database:              db,
			Bucket:                bucket,
			Prefix:                prefix,
			ExpectedCubridVersion: cluster.Spec.Version,
			StagingDir:            path.Join(restoreStagingRoot, string(cluster.UID)),
			TargetDir:             restoreTargetRoot,
		}
		op, err := r.Restore.StartRestore(ctx, target, cluster.Namespace, recoveryIdempotencyKey(cluster, target), req)
		if err != nil {
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "RestoreStartFailed", err.Error())
			return ctrl.Result{RequeueAfter: restorePollAfter}, true
		}
		cluster.Status.Bootstrap.OperationID = op.ID
		cluster.Status.Bootstrap.Phase = databasev1alpha1.BootstrapRestoring
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "RestoreInProgress", "restore started on "+target)
		return ctrl.Result{RequeueAfter: restorePollAfter}, true
	}

	// Poll the running operation.
	op, err := r.Restore.GetOperation(ctx, target, cluster.Namespace, cluster.Status.Bootstrap.OperationID)
	if err != nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "InstanceManagerUnavailable", err.Error())
		return ctrl.Result{RequeueAfter: restorePollAfter}, true
	}
	switch op.State {
	case instancemanager.OpCompleted:
		cluster.Status.Bootstrap.Phase = databasev1alpha1.BootstrapComplete
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionTrue, "RecoveryComplete", "restore completed on "+target)
		return ctrl.Result{}, false
	case instancemanager.OpFailed:
		r.recoveryFailed(cluster, "RestoreFailed", op.FailureReason)
		return ctrl.Result{}, true
	default:
		cluster.Status.Bootstrap.Phase = databasev1alpha1.BootstrapRestoring
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "RestoreInProgress", "restore in progress on "+target)
		return ctrl.Result{RequeueAfter: restorePollAfter}, true
	}
}

func (r *CubridClusterReconciler) recoveryFailed(cluster *databasev1alpha1.CubridCluster, reason, msg string) {
	if cluster.Status.Bootstrap != nil {
		cluster.Status.Bootstrap.Phase = databasev1alpha1.BootstrapFailed
	}
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reason, msg)
}

// recoveryIdempotencyKey is deterministic per cluster+manifest+member so a
// retry never starts a second restore (ADR-0008).
func recoveryIdempotencyKey(cluster *databasev1alpha1.CubridCluster, member string) string {
	return fmt.Sprintf("restore:%s:%s:%s", cluster.UID, cluster.Spec.Bootstrap.Recovery.ManifestURI, member)
}

// parseManifestURI splits an s3://bucket/prefix/manifest.json URI into the
// bucket and the artifact prefix (the directory holding manifest.json).
func parseManifestURI(uri string) (bucket, prefix string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	if !ok {
		return "", "", fmt.Errorf("manifestUri %q is not an s3:// URI", uri)
	}
	b, key, ok := strings.Cut(rest, "/")
	if !ok || b == "" || key == "" {
		return "", "", fmt.Errorf("manifestUri %q is missing a bucket or key", uri)
	}
	return b, path.Dir(key), nil
}
