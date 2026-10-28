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
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
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
	// reasonBootstrapRecoveryFailed is the Ready and Progressing reason of a
	// cluster whose recovery bootstrap ended as Failed.
	reasonBootstrapRecoveryFailed = "BootstrapRecoveryFailed"
)

// recoveryOperationMissingWait bounds how long a recovery waits for its
// recorded restore operation while the target answers that it holds no
// record of it, from status.bootstrap.operationMissingSince, as long as a
// backup waits (backupOperationMissingWait). The time is the operator's
// clock, written to the API server and compared with the operator's clock
// later; a clock that differs between operator processes moves the bound by
// that difference.
const recoveryOperationMissingWait = 30 * time.Minute

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
// records status.bootstrap. In an HA cluster the other members are then seeded
// from the restored one, and the recovery is complete only when every member
// holds the restored data. It returns a requeue result while in progress; the
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

	// A failed recovery is terminal: the member is not asked again, so the
	// reason it failed with stays.
	if cluster.Status.Bootstrap.Phase == databasev1alpha1.BootstrapFailed {
		return ctrl.Result{}, true
	}

	// A URI outside the accepted form starts no restore. One an earlier
	// release admitted and started a restore for is followed, by that
	// release's reading of it, only while that reads the object it names.
	bucket, prefix, err := parseManifestURI(rec.ManifestURI)
	if err != nil && recoveryStarted(cluster, db) {
		bucket, prefix, err = startedManifestLocation(rec.ManifestURI)
	}
	if err != nil {
		r.recoveryFailed(cluster, "InvalidManifestURI", err.Error())
		return ctrl.Result{}, true
	}

	if r.Restore == nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "RestoreClientNotConfigured",
			"no restore client configured on the controller")
		return ctrl.Result{}, true
	}

	// Once the restore has completed, what remains is seeding the other members.
	if cluster.Spec.HighAvailability.Enabled && restoredOnTarget(cluster, db) {
		return r.reconcileRecoverySeeding(ctx, cluster, db)
	}

	// Start once: begin the restore when no operation is recorded yet.
	if cluster.Status.Bootstrap.OperationID == "" {
		if msg, held := engineSeriesHold(cluster, target); held {
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reasonEngineVersionMismatch, msg)
			return ctrl.Result{RequeueAfter: restorePollAfter}, true
		}
		req := instancemanager.RestoreRequest{
			Database:              db,
			Bucket:                bucket,
			Prefix:                prefix,
			ExpectedCubridVersion: cluster.Spec.Version,
			StagingDir:            path.Join(restoreStagingRoot, string(cluster.UID)),
			TargetDir:             restoreTargetRoot,
		}
		key := recoveryIdempotencyKey(cluster, target)
		op, err := r.Restore.StartRestore(ctx, target, cluster.Namespace, key, req)
		if err != nil {
			op, err = operationOfKey(ctx, err, instancemanager.OpRestore, key, func() (instancemanager.Operation, bool, error) {
				return r.Restore.FindOperation(ctx, target, cluster.Namespace, instancemanager.OpRestore, key)
			})
		}
		// A request the member refuses as such is refused again: it counts
		// as a failed attempt.
		if errors.Is(err, errRequestRefused) {
			return r.restoreAttemptFailed(cluster, target, err.Error())
		}
		if err != nil {
			// A refused token and each cause of a conflict keep a reason of
			// their own; any other failure is a failed start.
			reason := instanceManagerFailureReason(err)
			if reason == reasonInstanceManagerUnavailable {
				reason = "RestoreStartFailed"
			}
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reason, err.Error())
			return ctrl.Result{RequeueAfter: restorePollAfter}, true
		}
		cluster.Status.Bootstrap.OperationID = op.ID
		setRestoreProgress(cluster, op.State, target)
		return ctrl.Result{RequeueAfter: restorePollAfter}, true
	}

	// Poll the running operation.
	op, err := r.Restore.GetOperation(ctx, target, cluster.Namespace, cluster.Status.Bootstrap.OperationID)
	if errors.Is(err, errOperationMissing) {
		return r.restoreOperationMissing(cluster, target, err)
	}
	if err != nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, instanceManagerFailureReason(err), err.Error())
		return ctrl.Result{RequeueAfter: restorePollAfter}, true
	}
	cluster.Status.Bootstrap.OperationMissingSince = nil
	switch op.State {
	case instancemanager.OpCompleted:
		if cluster.Spec.HighAvailability.Enabled {
			return r.reconcileRecoverySeeding(ctx, cluster, db)
		}
		cluster.Status.Bootstrap.Phase = databasev1alpha1.BootstrapComplete
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionTrue, "RecoveryComplete", "restore completed on "+target)
		return ctrl.Result{}, false
	case instancemanager.OpFailed:
		return r.restoreAttemptFailed(cluster, target, op.FailureReason)
	default:
		setRestoreProgress(cluster, op.State, target)
		return ctrl.Result{RequeueAfter: restorePollAfter}, true
	}
}

// restoreAttemptFailed ends an attempt of the recovery restore for why. A
// failed restore, which includes one cut off by a manager restart, is
// started again under the next attempt's key. The Instance Manager removes
// what its own failed restore left before it restores again, and nothing
// else (ADR-0008). After the last attempt the recovery is Failed.
func (r *CubridClusterReconciler) restoreAttemptFailed(cluster *databasev1alpha1.CubridCluster, target, why string) (ctrl.Result, bool) {
	boot := cluster.Status.Bootstrap
	if boot.Attempts+1 < maxBootstrapAttempts {
		boot.Attempts++
		boot.OperationID = ""
		msg := fmt.Sprintf("on %s: %s; starting attempt %d of %d", target, why, boot.Attempts+1, maxBootstrapAttempts)
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "RestoreFailedRetrying", msg)
		r.statusEvent(cluster, corev1.EventTypeWarning, "RestoreFailedRetrying", msg)
		return ctrl.Result{RequeueAfter: restorePollAfter}, true
	}
	r.recoveryFailed(cluster, "RestoreFailed",
		fmt.Sprintf("on %s: %s; giving up after %d attempts", target, why, maxBootstrapAttempts))
	return ctrl.Result{}, true
}

// restoreOperationMissing is the poll of a recovery restore whose target
// answered that it holds no record of the recorded operation (#457). The
// recovery waits, from status.bootstrap.operationMissingSince, which only a
// read of the operation clears, so that an operator restart or a transient
// failure in between does not reset the bound. Once the record has stayed
// missing for recoveryOperationMissingWait the recovery is Failed. A missing
// record does not show that the restore ended, so it is not counted as a
// failed attempt and the restore is never started again.
func (r *CubridClusterReconciler) restoreOperationMissing(cluster *databasev1alpha1.CubridCluster, target string, err error) (ctrl.Result, bool) {
	boot := cluster.Status.Bootstrap
	if boot.OperationMissingSince == nil {
		since := metav1.NewTime(r.now())
		boot.OperationMissingSince = &since
	}
	since := boot.OperationMissingSince.Time
	if r.now().Sub(since) > recoveryOperationMissingWait {
		r.recoveryFailed(cluster, reasonRecordedOperationLost, fmt.Sprintf(
			"%s has held no record of the restore operation %s since %s, for %s or longer (%v). A missing record "+
				"does not show that the restore ended: restoredb may still be running on %s, or have left data "+
				"there. The restore is not started again; check %s before relying on its data, and delete and "+
				"create the CubridCluster again to restore anew (ADR-0008)",
			target, boot.OperationID, since.UTC().Format(time.RFC3339), recoveryOperationMissingWait, err, target, target))
		return ctrl.Result{}, true
	}
	// The answer is left out of this message: it carries the ID of each
	// request, and the condition would change at every poll.
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reasonRecordedOperationMissing, fmt.Sprintf(
		"%s holds no record of the restore operation %s; waiting for at most %s from %s for it to be read again",
		target, boot.OperationID, recoveryOperationMissingWait, since.UTC().Format(time.RFC3339)))
	return ctrl.Result{RequeueAfter: restorePollAfter}, true
}

// setRestoreProgress reports a restore operation that has not ended, by what
// the Instance Manager says it does. The phase is Restoring only while
// restoredb runs or the restored database is started; before that the
// operation verifies and downloads the backup, and the phase stays Preparing.
func setRestoreProgress(cluster *databasev1alpha1.CubridCluster, state instancemanager.OperationState, target string) {
	phase, doing := databasev1alpha1.BootstrapPreparing, "the backup is being verified and downloaded on "
	switch state {
	case instancemanager.OpRestoring:
		phase, doing = databasev1alpha1.BootstrapRestoring, "restoredb is running on "
	case instancemanager.OpStarting:
		phase, doing = databasev1alpha1.BootstrapRestoring, "the restored database is being started on "
	}
	cluster.Status.Bootstrap.Phase = phase
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "RestoreInProgress", doing+target)
}

// restoredOnTarget reports whether the recovery restore of database was
// recorded as complete on the target member.
func restoredOnTarget(cluster *databasev1alpha1.CubridCluster, database string) bool {
	for _, status := range cluster.Status.Databases {
		if status.Name == database {
			return status.PrimaryCreated
		}
	}
	return false
}

// reconcileRecoverySeeding copies the restored database from the member it was
// restored on to the other members, with the same seeding as the HA bootstrap
// of a created database (ADR-0008/0010). The first peer is copied from the
// member the backup was restored on because it is the only member that holds
// the data, not because it stays the master: CUBRID decides that (ADR-0005),
// and every later member is copied from the member observed as the master.
// The recovery is complete when every member is seeded, and Failed when
// seeding gave up.
func (r *CubridClusterReconciler) reconcileRecoverySeeding(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	database string) (ctrl.Result, bool) {
	boot := cluster.Status.Bootstrap
	status := databaseStatus(cluster, database)
	if !status.PrimaryCreated {
		status.Phase = databasePhaseCreated
		status.PrimaryCreated = true
		r.statusEvent(cluster, corev1.EventTypeNormal, "DatabaseRestored", "database "+database+" restored on "+boot.TargetMember+
			"; copying it to the other members")
	}
	if status.Phase != databasePhaseFailed {
		boot.Phase = databasev1alpha1.BootstrapSeedingReplicas
		r.reconcileSeeding(ctx, cluster, status, boot.TargetMember)
	}
	switch {
	case status.HAConfigured:
		boot.Phase = databasev1alpha1.BootstrapComplete
		return ctrl.Result{}, false
	case status.Phase == databasePhaseFailed:
		boot.Phase = databasev1alpha1.BootstrapFailed
		return ctrl.Result{}, true
	default:
		return ctrl.Result{RequeueAfter: restorePollAfter}, true
	}
}

func (r *CubridClusterReconciler) recoveryFailed(cluster *databasev1alpha1.CubridCluster, reason, msg string) {
	if cluster.Status.Bootstrap != nil {
		cluster.Status.Bootstrap.Phase = databasev1alpha1.BootstrapFailed
	}
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reason, msg)
}

// recoveryIdempotencyKey is deterministic per cluster, manifest, member and
// attempt, so a reconcile after an operator restart finds the restore that is
// running instead of starting a second one (ADR-0008). Only a restore that
// failed is followed by one under the next attempt's key.
func recoveryIdempotencyKey(cluster *databasev1alpha1.CubridCluster, member string) string {
	key := fmt.Sprintf("restore:%s:%s:%s", cluster.UID, cluster.Spec.Bootstrap.Recovery.ManifestURI, member)
	if boot := cluster.Status.Bootstrap; boot != nil && boot.Attempts > 0 {
		key += fmt.Sprintf(":a%d", boot.Attempts)
	}
	return key
}

// startedManifestLocation is where a restore that an earlier release started
// for uri reads: the manifest.json of the directory of its key. It is
// returned only when that is the object uri names; the earlier parser read
// another one for another file name, a trailing slash, a query, and ".", ".."
// or empty segments.
func startedManifestLocation(uri string) (bucket, prefix string, err error) {
	rest, isS3 := strings.CutPrefix(uri, "s3://")
	bucket, key, hasKey := strings.Cut(rest, "/")
	if !isS3 || !hasKey || bucket == "" || key == "" {
		return "", "", fmt.Errorf("manifestUri %q is not an s3:// URI with a bucket and a key", uri)
	}
	// A ".", ".." or empty segment is read literally by some stores and
	// resolved by others: such a key is never taken to name one object.
	if slices.ContainsFunc(strings.Split(key, "/"), func(s string) bool { return s == "" || s == "." || s == ".." }) {
		return "", "", fmt.Errorf("manifestUri %q has an empty, \".\" or \"..\" segment in its key %q", uri, key)
	}
	prefix = path.Dir(key)
	if read := path.Join(prefix, instancemanager.ManifestObjectName); read != key {
		return "", "", fmt.Errorf("manifestUri %q names the object %q, but the restore started for it reads %q", uri, key, read)
	}
	return bucket, prefix, nil
}

// recoveryStarted reports whether a restore of the recovery was started:
// one is running or has failed and is started again, or it has completed.
func recoveryStarted(cluster *databasev1alpha1.CubridCluster, database string) bool {
	boot := cluster.Status.Bootstrap
	return boot.OperationID != "" || boot.Attempts > 0 || restoredOnTarget(cluster, database)
}

var (
	manifestURIRe                   = regexp.MustCompile(databasev1alpha1.ManifestURIPattern)
	manifestURIBucketAdjacentDotsRe = regexp.MustCompile(databasev1alpha1.ManifestURIBucketAdjacentDotsPattern)
)

// parseManifestURI splits an s3://<bucket>/<prefix>/manifest.json URI into the
// bucket and the artifact prefix (the directory holding manifest.json). It
// accepts what the API server admits for spec.bootstrap.recovery.manifestUri
// and nothing else, so that a URI that names another object, or the same one
// only after normalization, is refused instead of being read as another
// manifest. The prefix is returned as written: the key the Instance Manager
// reads, path.Join(prefix, manifest.json), is the key the URI names.
func parseManifestURI(uri string) (bucket, prefix string, err error) {
	if utf8.RuneCountInString(uri) > databasev1alpha1.ManifestURIMaxLength ||
		!manifestURIRe.MatchString(uri) || manifestURIBucketAdjacentDotsRe.MatchString(uri) {
		return "", "", fmt.Errorf("manifestUri %q is not of the form s3://<bucket>/<prefix>/manifest.json "+
			"accepted for a recovery (ADR-0008)", uri)
	}
	key := strings.TrimPrefix(uri, "s3://")
	bucket, key, _ = strings.Cut(key, "/")
	return bucket, strings.TrimSuffix(key, "/"+instancemanager.ManifestObjectName), nil
}

// reasonEngineVersionMismatch is the BootstrapReady reason while a backup or
// restore of a bootstrap step waits for a member whose engine is not of the
// series spec.version declares.
const reasonEngineVersionMismatch = "EngineVersionMismatch"

// engineSeriesHold reports why a backup or restore on member waits: the
// engine the member was last observed running is not of the series
// spec.version declares, so its Instance Manager would refuse the step
// (ADR-0011). Such a step is not started and uses up no attempt; it goes on
// once spec.version is corrected or the member runs an engine of the
// declared series. A member whose engine has not been observed is not held:
// its Instance Manager checks for itself, and a refusal it answers uses up an
// attempt like any refused request.
func engineSeriesHold(cluster *databasev1alpha1.CubridCluster, member string) (string, bool) {
	for _, in := range cluster.Status.Instances {
		if in.Name != member || in.ObservedEngineVersion == "" {
			continue
		}
		series := instancemanager.EngineSeries(in.ObservedEngineVersion)
		if series != "" && series == instancemanager.EngineSeries(cluster.Spec.Version) {
			return "", false
		}
		return member + " runs engine " + in.ObservedEngineVersion + ", which is not of spec.version " +
			cluster.Spec.Version + "; the step waits and uses up no attempt (ADR-0011)", true
	}
	return "", false
}
