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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

const (
	conditionAccepted    = "Accepted"
	conditionBackupReady = "Ready"

	// reasonRecordedTargetSelected / reasonRecordedTargetNotEligible are the
	// Ready reasons while a backup start is retried on its recorded target.
	reasonRecordedTargetSelected    = "RecordedTargetSelected"
	reasonRecordedTargetNotEligible = "RecordedTargetNotEligible"

	// reasonUnsupportedDestination ends a backup whose destination type has
	// no execution path (PVC).
	reasonUnsupportedDestination = "UnsupportedDestination"
	// reasonObjectStorageNotApplied holds a backup whose target Pod does not
	// run with the cluster's object-storage settings yet.
	reasonObjectStorageNotApplied = "ObjectStorageNotApplied"

	// The terminal reasons of a backup whose wait for an operation ran out
	// (backupTargetWait). reasonRecordedTargetNotStarted: the recorded target
	// answered that it holds no operation for the backup.
	// reasonRecordedTargetUnverified: the recorded target could not be asked,
	// so a backup may still run on it. reasonObjectStorageWaitExpired: no
	// target was recorded, because the selected Pod did not run with the
	// cluster's object-storage settings; nothing was started.
	reasonRecordedTargetNotStarted = "RecordedTargetNotStarted"
	reasonRecordedTargetUnverified = "RecordedTargetUnverified"
	reasonObjectStorageWaitExpired = "ObjectStorageWaitExpired"

	// objectStorageEnvPrefix starts the names of the manager's object-storage
	// environment (objectStorageEnv).
	objectStorageEnvPrefix = "IM_S3_"

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
	// Recorder records an Event for each phase a backup reaches; nil records
	// none.
	Recorder record.EventRecorder
	// Clock tells the time; nil means time.Now.
	Clock func() time.Time

	// phases holds, per backup that is being reconciled, the phase its
	// stored status has (writeStatus).
	phases sync.Map
}

// backupClusterWait is how long a backup waits for a cluster that does not
// exist, counted from the backup's creation.
const backupClusterWait = 10 * time.Minute

// backupTargetWait bounds how long a backup waits for its operation to be
// started: on its recorded target, counted from status.targetRecordedAt,
// and for a selected Pod that does not run with the cluster's object-storage
// settings, counted from the backup's creation.
const backupTargetWait = 30 * time.Minute

// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *CubridBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var backup databasev1alpha1.CubridBackup
	if err := r.Get(ctx, req.NamespacedName, &backup); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	r.phases.Store(backup.UID, backup.Status.Phase)
	defer r.phases.Delete(backup.UID)
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
			return r.clusterMissing(ctx, &backup)
		}
		return ctrl.Result{}, err
	}
	// The backup was started on one cluster. A cluster of the same name that
	// was created since is another one: its members hold another database
	// and know nothing of this backup's operation.
	if backup.Status.ClusterUID != "" && backup.Status.ClusterUID != cluster.UID {
		return r.ended(ctx, &backup, "ClusterReplaced", "CubridCluster "+cluster.Name+
			" was deleted and created again after this backup was started; the backup is not continued on the new cluster")
	}

	setBackupCondition(&backup, conditionAccepted, metav1.ConditionTrue, "BackupAccepted",
		"spec is valid and the referenced cluster exists")

	if backup.Status.OperationRef != "" {
		return r.pollBackup(ctx, &backup)
	}

	// The DB Pods get their object-storage endpoint and credentials from the
	// cluster at Pod start (ADR-0007); a backup cannot bring its own. Not
	// terminal: the cluster can be given the setting.
	if cluster.Spec.ObjectStorage == nil {
		if backup.Status.Phase == "" {
			backup.Status.Phase = databasev1alpha1.BackupPhasePending
		}
		setBackupCondition(&backup, conditionBackupReady, metav1.ConditionFalse, "ObjectStorageNotConfigured",
			"CubridCluster "+cluster.Name+" has no spec.objectStorage; its DB Pods cannot upload the backup")
		return r.commit(ctx, &backup, ctrl.Result{RequeueAfter: backupPollAfter})
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

// startBackup resolves the cluster HA state, selects a safe target, records it
// in status, and only then starts the Instance Manager backup operation with a
// deterministic idempotency key. A dispatch whose response is lost may still
// have started the backup, so once a target is recorded only that target is
// ever dispatched to; it is never replaced by a fresh selection. For the same
// reason spec.target.preference is read only for that first selection: a later
// change does not move a backup whose target is already recorded. Nothing is
// dispatched to a target whose Pod does not run with the cluster's
// object-storage settings. Both waits are bounded by backupTargetWait.
func (r *CubridBackupReconciler) startBackup(ctx context.Context, backup *databasev1alpha1.CubridBackup, cluster *databasev1alpha1.CubridCluster) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	count := cluster.Spec.Topology.PromotableMembers
	members := memberNames(cluster, count)
	obs := r.probeAll(ctx, members, cluster.Namespace)
	observedAt := time.Now()
	res := resolvePrimary(members, obs, observedAt)
	single := count <= 1

	var sel TargetSelection
	if backup.Status.TargetInstance != "" {
		sel = TargetSelection{
			Instance:     backup.Status.TargetInstance,
			Role:         databasev1alpha1.CubridRole(backup.Status.TargetRole),
			FallbackUsed: backup.Status.FallbackUsed,
			Selected:     true,
			Reason:       reasonRecordedTargetSelected,
		}
		// A target recorded by an operator that did not record the time is
		// given the present time, so that its wait is never cut short.
		if backup.Status.TargetRecordedAt == nil {
			recordedAt := metav1.NewTime(r.now())
			backup.Status.TargetRecordedAt = &recordedAt
		}
		// Once the bound has passed the start is not sent again.
		if r.now().Sub(backup.Status.TargetRecordedAt.Time) > backupTargetWait {
			return r.endRecordedTargetWait(ctx, backup)
		}
		if !recordedTargetEligible(sel, members, obs, res, single, observedAt) {
			// The start is not sent again to a target that is not eligible,
			// and never to another member, since the earlier dispatch may be
			// running. The target is only asked whether it holds the
			// operation; one it holds is followed from now on.
			op, found, err := r.Backup.FindBackupOperation(ctx, sel.Instance, backup.Namespace, idempotencyKey(backup))
			if err == nil && found {
				return r.adoptBackup(ctx, backup, op)
			}
			msg := "recorded target " + sel.Instance + " is no longer eligible for its recorded role " + backup.Status.TargetRole
			if err != nil {
				msg += "; it could not be asked whether it runs the backup: " + err.Error()
			} else {
				msg += "; it holds no operation of this backup"
			}
			return r.waitOnRecordedTarget(ctx, backup, reasonRecordedTargetNotEligible, msg)
		}
	} else {
		sel = selectBackupTarget(members, obs, res, backup.Spec.Target.Preference, single, observedAt)
		if !sel.Selected {
			// Not terminal: HA may resolve later. Surface why and requeue.
			setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, sel.Reason,
				"no safe backup target yet; waiting for a healthy target")
			return r.commit(ctx, backup, ctrl.Result{RequeueAfter: backupPollAfter})
		}
	}

	if backup.Status.TargetInstance == "" {
		// Not terminal: the Pod picks the settings up when it is restarted,
		// and the backup goes on then. The target stays unrecorded, so a
		// later selection may take another member. A recorded target is not
		// judged here: an earlier dispatch to it may run already, and asking
		// it again returns that operation; a manager without object storage
		// refuses with 412 (ObjectStorageNotApplied).
		notApplied, err := r.objectStorageNotApplied(ctx, cluster, sel.Instance)
		if err != nil {
			return ctrl.Result{}, err
		}
		if notApplied != "" {
			// Nothing was started, so ending the backup leaves nothing
			// running anywhere.
			if r.now().Sub(backup.CreationTimestamp.Time) > backupTargetWait {
				return r.ended(ctx, backup, reasonObjectStorageWaitExpired, fmt.Sprintf(
					"%s; no selected Pod ran with the cluster's spec.objectStorage within %s of the backup's creation, "+
						"nothing was started, and the backup is not continued", notApplied, backupTargetWait))
			}
			if backup.Status.Phase == "" {
				backup.Status.Phase = databasev1alpha1.BackupPhasePending
			}
			setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, reasonObjectStorageNotApplied,
				fmt.Sprintf("%s; waiting for at most %s from the backup's creation", notApplied, backupTargetWait))
			return r.commit(ctx, backup, ctrl.Result{RequeueAfter: backupPollAfter})
		}

		if backup.Status.Phase == "" {
			backup.Status.Phase = databasev1alpha1.BackupPhasePending
		}
		backup.Status.TargetInstance = sel.Instance
		backup.Status.TargetRole = string(sel.Role)
		backup.Status.FallbackUsed = sel.FallbackUsed
		// Recorded with the target, before anything is started on it.
		backup.Status.ClusterUID = cluster.UID
		recordedAt := metav1.NewTime(r.now())
		backup.Status.TargetRecordedAt = &recordedAt
		setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, sel.Reason, "starting the backup on "+sel.Instance)
		backup.Status.ObservedGeneration = backup.Generation
		if err := r.writeStatus(ctx, backup); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}

	req := r.buildBackupRequest(backup, cluster, sel)
	op, err := r.Backup.StartBackup(ctx, sel.Instance, backup.Namespace, idempotencyKey(backup), req)
	if err != nil {
		log.Error(err, "Failed to start backup", "instance", sel.Instance)
		return r.waitOnRecordedTarget(ctx, backup, instanceManagerFailureReason(err), err.Error())
	}

	now := metav1.Now()
	backup.Status.Phase = databasev1alpha1.BackupPhaseRunning
	backup.Status.OperationRef = op.ID
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
		setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, instanceManagerFailureReason(err), err.Error())
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

// waitOnRecordedTarget holds a backup whose recorded target runs no operation
// of it that the operator knows of. The next reconcile sends the start to the
// target again if it is eligible; once backupTargetWait from
// status.targetRecordedAt has passed, endRecordedTargetWait decides instead.
func (r *CubridBackupReconciler) waitOnRecordedTarget(ctx context.Context, backup *databasev1alpha1.CubridBackup, reason, msg string) (ctrl.Result, error) {
	setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, reason,
		fmt.Sprintf("%s; waiting for at most %s from when the target was recorded", msg, backupTargetWait))
	return r.commit(ctx, backup, ctrl.Result{RequeueAfter: backupPollAfter})
}

// endRecordedTargetWait ends the wait of a backup on its recorded target
// once backupTargetWait has passed. The target is asked, without sending the
// start again, whether it holds the backup's operation. One it holds is
// followed. When it holds none the backup is Failed: nothing runs. When it
// cannot be asked the backup is Failed as well, but a backup may still run on
// the target, and the reason says so. Either way no other member is started.
func (r *CubridBackupReconciler) endRecordedTargetWait(ctx context.Context, backup *databasev1alpha1.CubridBackup) (ctrl.Result, error) {
	target := backup.Status.TargetInstance
	op, found, err := r.Backup.FindBackupOperation(ctx, target, backup.Namespace, idempotencyKey(backup))
	if err == nil && found {
		return r.adoptBackup(ctx, backup, op)
	}
	last := ""
	if ready := meta.FindStatusCondition(backup.Status.Conditions, conditionBackupReady); ready != nil {
		last = fmt.Sprintf(" (last: %s: %s)", ready.Reason, ready.Message)
	}
	if err != nil {
		return r.ended(ctx, backup, reasonRecordedTargetUnverified, fmt.Sprintf(
			"the backup was not started on its recorded target %s within %s%s, and %s could not be asked whether it "+
				"holds the backup's operation: %v. A backup may still be running on %s and upload under this backup's "+
				"prefix; check that member before relying on that prefix. The backup is not started on another member",
			target, backupTargetWait, last, target, err, target))
	}
	return r.ended(ctx, backup, reasonRecordedTargetNotStarted, fmt.Sprintf(
		"the backup was not started on its recorded target %s within %s%s, and %s holds no operation of it; "+
			"nothing runs, and the backup is not started on another member", target, backupTargetWait, last, target))
}

// adoptBackup records the operation that the recorded target holds under the
// backup's idempotency key, when the answer to its start was lost, and
// follows it from then on like any started backup.
func (r *CubridBackupReconciler) adoptBackup(ctx context.Context, backup *databasev1alpha1.CubridBackup, op instancemanager.Operation) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("Adopted backup operation of the recorded target",
		"instance", backup.Status.TargetInstance, "operationID", op.ID)
	backup.Status.Phase = databasev1alpha1.BackupPhaseRunning
	backup.Status.OperationRef = op.ID
	if backup.Status.StartedAt == nil {
		started := metav1.NewTime(r.now())
		if !op.CreatedAt.IsZero() {
			started = metav1.NewTime(op.CreatedAt)
		}
		backup.Status.StartedAt = &started
	}
	return r.pollBackup(ctx, backup)
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

// objectStorageNotApplied says why the Pod of instance does not run with the
// object-storage settings of the cluster; it is empty when it does. The
// manager reads them from its environment when it starts (ADR-0007), and a
// Pod's environment does not change while the Pod lives: under the OnDelete
// update strategy a Pod started before spec.objectStorage was set or changed
// keeps the old settings until it is deleted. The Pod is compared, not the
// StatefulSet template. What the referenced Secret holds is not compared:
// the operator does not read it.
func (r *CubridBackupReconciler) objectStorageNotApplied(ctx context.Context, cluster *databasev1alpha1.CubridCluster, instance string) (string, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: instance}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return "Pod " + instance + " is not found; waiting for it to run with the cluster's spec.objectStorage", nil
		}
		return "", err
	}
	applied := map[string]corev1.EnvVar{}
	for _, c := range pod.Spec.Containers {
		if c.Name != appName {
			continue
		}
		for _, e := range c.Env {
			if strings.HasPrefix(e.Name, objectStorageEnvPrefix) {
				applied[e.Name] = e
			}
		}
	}
	desired := objectStorageEnv(cluster.Spec.ObjectStorage)
	same := len(applied) == len(desired)
	for _, e := range desired {
		if got, ok := applied[e.Name]; !ok || !equality.Semantic.DeepEqual(got, e) {
			same = false
		}
	}
	if same {
		return "", nil
	}
	return "Pod " + instance + " was started without the cluster's current spec.objectStorage; " +
		"it picks the settings up when it is restarted", nil
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
	if err := r.writeStatus(ctx, backup); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	return result, nil
}

// backupPhaseEvents names the Event and the log event of each phase a backup
// reaches (docs/observability.md).
var backupPhaseEvents = map[databasev1alpha1.CubridBackupPhase]struct{ reason, logEvent string }{
	databasev1alpha1.BackupPhasePending:   {"BackupPending", "backup_pending"},
	databasev1alpha1.BackupPhaseRunning:   {"BackupStarted", "backup_started"},
	databasev1alpha1.BackupPhaseUploading: {"BackupUploading", "backup_uploading"},
	databasev1alpha1.BackupPhaseCompleted: {"BackupCompleted", "backup_completed"},
	databasev1alpha1.BackupPhaseFailed:    {"BackupFailed", "backup_failed"},
}

// writeStatus writes the backup's status. When the write brought the stored
// status to another phase, it records one Event and one log line for it, with
// the message of the Ready condition. A phase whose write failed is reported
// by the reconcile that writes it.
func (r *CubridBackupReconciler) writeStatus(ctx context.Context, backup *databasev1alpha1.CubridBackup) error {
	if err := r.Status().Update(ctx, backup); err != nil {
		return err
	}
	phase := backup.Status.Phase
	stored, known := r.phases.Swap(backup.UID, phase)
	named, ok := backupPhaseEvents[phase]
	if !known || stored == phase || !ok {
		return nil
	}
	msg := string(phase)
	if ready := meta.FindStatusCondition(backup.Status.Conditions, conditionBackupReady); ready != nil {
		msg = ready.Message
	}
	logf.FromContext(ctx).Info("Backup reached phase "+string(phase), "event", named.logEvent,
		"target", backup.Status.TargetInstance, "operationID", backup.Status.OperationRef, "message", msg)
	if r.Recorder != nil {
		eventType := corev1.EventTypeNormal
		if phase == databasev1alpha1.BackupPhaseFailed {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Event(backup, eventType, named.reason, msg)
	}
	return nil
}

// validateBackupSpec enforces the ADR-0007 destination invariants that CRD
// markers cannot express (ObjectStorage requires the objectStorage block).
//
// Only object storage has an execution path: the Instance Manager uploads
// the staged backup and writes its manifest there. Any other destination
// (PVC) is refused before a member is asked for anything, and before the
// cluster is looked up. A backup whose operation was started, by an earlier
// operator, is followed to the end of that operation instead.
func validateBackupSpec(backup *databasev1alpha1.CubridBackup) (reason, msg string, ok bool) {
	if backup.Spec.Destination.Type != databasev1alpha1.DestinationObjectStorage && backup.Status.OperationRef == "" {
		return reasonUnsupportedDestination, "destination type " + string(backup.Spec.Destination.Type) +
			" is not supported: a backup is stored in object storage only", false
	}
	if backup.Spec.Destination.Type == databasev1alpha1.DestinationObjectStorage &&
		backup.Spec.Destination.ObjectStorage == nil {
		return "InvalidDestination", "destination.objectStorage is required when type is ObjectStorage", false
	}
	return "", "", true
}

// rejected ends a backup that is not accepted. It is Failed, with the time
// it ended.
func (r *CubridBackupReconciler) rejected(ctx context.Context, backup *databasev1alpha1.CubridBackup, reason, msg string) (ctrl.Result, error) {
	setBackupCondition(backup, conditionAccepted, metav1.ConditionFalse, reason, msg)
	return r.ended(ctx, backup, reason, msg)
}

// ended makes a backup Failed for good, with the reason and the time.
func (r *CubridBackupReconciler) ended(ctx context.Context, backup *databasev1alpha1.CubridBackup, reason, msg string) (ctrl.Result, error) {
	now := metav1.NewTime(r.now())
	backup.Status.Phase = databasev1alpha1.BackupPhaseFailed
	backup.Status.CompletedAt = &now
	setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, reason, msg)
	return r.commit(ctx, backup, ctrl.Result{})
}

// clusterMissing decides what a backup does whose cluster is not found.
//
// A backup that was started on a cluster ends: the members that ran it are
// gone with the cluster. A backup that was not started waits. The cluster may
// be created a moment after the backup, or the operator may read from a cache
// that does not hold it yet, so not finding it is no failure; the wait is
// bounded by backupClusterWait from the backup's creation, after which the
// backup is refused.
func (r *CubridBackupReconciler) clusterMissing(ctx context.Context, backup *databasev1alpha1.CubridBackup) (ctrl.Result, error) {
	name := backup.Spec.ClusterRef.Name
	if backup.Status.ClusterUID != "" {
		return r.ended(ctx, backup, "ClusterDeleted",
			"CubridCluster "+name+" was deleted after this backup was started on it")
	}
	if waited := r.now().Sub(backup.CreationTimestamp.Time); waited > backupClusterWait {
		return r.rejected(ctx, backup, "ClusterNotFound",
			fmt.Sprintf("referenced CubridCluster %s was not found within %s of the backup's creation", name, backupClusterWait))
	}
	if backup.Status.Phase == "" {
		backup.Status.Phase = databasev1alpha1.BackupPhasePending
	}
	msg := fmt.Sprintf("referenced CubridCluster %s is not found; waiting for it for at most %s from the backup's creation",
		name, backupClusterWait)
	setBackupCondition(backup, conditionAccepted, metav1.ConditionFalse, "ClusterNotFound", msg)
	setBackupCondition(backup, conditionBackupReady, metav1.ConditionFalse, "ClusterNotFound", msg)
	return r.commit(ctx, backup, ctrl.Result{RequeueAfter: backupPollAfter})
}

func (r *CubridBackupReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
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
