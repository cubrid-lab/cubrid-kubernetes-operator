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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// reasonMemberRebuildRestoreStarted is the Normal Event of a rebuild whose
// restore on the member was started.
const reasonMemberRebuildRestoreStarted = "MemberRebuildRestoreStarted"

// rebuildBackupKey and rebuildRestoreKey are the idempotency keys of the copy
// of the accepted rebuild a. They are built from its operation ID, which is
// chosen once when the request is accepted, so every pass, also after an
// Operator restart, addresses the same operations, and no key of the HA
// bootstrap or of another rebuild is ever reused.
func rebuildBackupKey(a *databasev1alpha1.MemberRebuild) string {
	return "rebuild-backup-" + a.OperationID
}

func rebuildRestoreKey(a *databasev1alpha1.MemberRebuild) string {
	return "rebuild-restore-" + a.OperationID + "-" + a.Member
}

// rebuildSeedLocation is where the backup of the rebuild a of database lives
// in object storage: <prefix>/<clusterUID>/rebuild-<operationID>/<database>,
// the only place the Instance Manager accepts a rebuild backup at, never the
// HA bootstrap's seed location.
func rebuildSeedLocation(cluster *databasev1alpha1.CubridCluster, database string,
	a *databasev1alpha1.MemberRebuild) (bucket, prefix string) {
	storage := cluster.Spec.ObjectStorage
	return storage.Bucket, path.Join(strings.TrimLeft(storage.Prefix, "/"), string(cluster.UID),
		instancemanager.RebuildSeedDir(a.OperationID), database)
}

// restoredRebuild reports whether the restore operation op restored the
// backup of the rebuild with operationID for database of the cluster uid.
func restoredRebuild(op instancemanager.Operation, uid, operationID, database string) bool {
	restored := op.Restored
	if restored == nil {
		restored = op.Artifact
	}
	return restored != nil && strings.HasSuffix(restored.ManifestURI,
		"/"+path.Join(uid, instancemanager.RebuildSeedDir(operationID), database, instancemanager.ManifestObjectName))
}

// rebuildRestoreChecks are the Instance Manager checks the restore of a
// rebuild relies on on the member being rebuilt: those of a seed restore, and
// the binding of the restore to the rebuild with the check of its source
// (ADR-0013).
var rebuildRestoreChecks = append(slices.Clone(seedRestoreChecks), instancemanager.CapabilityRebuildRestore)

// rebuildBackupChecks are the checks the backup of a rebuild relies on on its
// source. A manager that reports rebuild-restore also accepts a seed backup
// bound to a rebuild (#1041).
var rebuildBackupChecks = []string{instancemanager.CapabilitySeedUpload, instancemanager.CapabilityBackupSourceRole,
	instancemanager.CapabilityRebuildRestore}

// rebuildHolders are the members that hold the database while member is
// rebuilt: every other member.
func rebuildHolders(in rebuildInput, member string) []string {
	return slices.DeleteFunc(slices.Clone(in.Members), func(m string) bool { return m == member })
}

// rebuildReading is this pass's answers, with no answer for a member whose
// Pod changed during the observation (rebuildInput.answer).
func rebuildReading(in rebuildInput) map[string]RoleObservation {
	obs := make(map[string]RoleObservation, len(in.Members))
	for _, m := range in.Members {
		obs[m] = in.answer(m)
	}
	return obs
}

// rebuildRestoreStarted reports whether the restore of the accepted rebuild a
// was started. It was once its operation is recorded. A restore that was
// started in a pass whose status was not written is found by its key on the
// member, so that the rebuild is never taken for one that restored nothing:
// the member is asked in every pass in which no restore is recorded, also
// when nothing of the copy is, as every status write since the acceptance may
// have been lost. A restore found with no copy recorded is judged from the
// acceptance on, so the readings missing since then count as a gap. An error
// says that this cannot be told now.
func (r *CubridClusterReconciler) rebuildRestoreStarted(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	a *databasev1alpha1.MemberRebuild) (bool, error) {
	switch {
	case a == nil || r.Restore == nil:
		return false, nil
	case a.Copy != nil && a.Copy.RestoreOperationID != "":
		return true, nil
	}
	op, found, err := r.Restore.FindOperation(ctx, a.Member, cluster.Namespace, instancemanager.OpRestore,
		rebuildRestoreKey(a))
	if err != nil {
		return false, fmt.Errorf("look up the restore of the rebuild of %s (operation %s): %w", a.Member,
			a.OperationID, err)
	}
	if found {
		if a.Copy == nil {
			since := a.AcceptedTime
			a.Copy = &databasev1alpha1.DatabaseSeeding{Member: a.Member, Source: a.Source, ActiveMasterSince: &since}
		}
		a.Copy.RestoreOperationID = op.ID
	}
	return found, nil
}

// endRebuild ends the accepted rebuild in.Active with state and why, moves it
// to status.lastMemberRebuild and records an Event.
func (r *CubridClusterReconciler) endRebuild(cluster *databasev1alpha1.CubridCluster, in *rebuildInput,
	state databasev1alpha1.MemberRebuildState, why string) {
	ended := in.Active.DeepCopy()
	end := metav1.NewTime(in.Now.UTC())
	ended.State, ended.EndTime = state, &end
	if state != databasev1alpha1.MemberRebuildCompleted {
		ended.Message = why
	}
	cluster.Status.MemberRebuild, cluster.Status.LastMemberRebuild, in.Active = nil, ended, nil
	switch state {
	case databasev1alpha1.MemberRebuildWithdrawn:
		r.statusEvent(cluster, corev1.EventTypeWarning, reasonMemberRebuildWithdrawn, fmt.Sprintf(
			"withdrew the rebuild of %s (operation %s) before anything was restored on it: %s; nothing was deleted, "+
				"and a new token is published once %s can be rebuilt", ended.Member, ended.OperationID, why, ended.Member))
	case databasev1alpha1.MemberRebuildCompleted:
		r.statusEvent(cluster, corev1.EventTypeNormal, reasonMemberRebuildCompleted, fmt.Sprintf(
			"rebuilt %s from %s (operation %s): %s", ended.Member, ended.Source, ended.OperationID, why))
	case databasev1alpha1.MemberRebuildFailed:
		if ended.Copy == nil || ended.Copy.RestoreOperationID == "" {
			r.statusEvent(cluster, corev1.EventTypeWarning, reasonMemberRebuildFailed, fmt.Sprintf(
				"the rebuild of %s from %s (operation %s) failed before a restore was started on it: %s. Nothing was "+
					"restored or deleted", ended.Member, ended.Source, ended.OperationID, why))
			return
		}
		// The claim may hold what the restore wrote, or a restore still in
		// progress: it is never offered again, so no later request makes the
		// Instance Manager take it over or remove it.
		record := databasev1alpha1.MemberDataClaim{Name: ended.Member, ClaimUID: ended.ClaimUID,
			VolumeName: ended.VolumeName}
		if !slices.Contains(in.FormerDataClaims, record) && len(in.FormerDataClaims) < maxFormerDataClaims {
			in.FormerDataClaims = append(slices.Clone(in.FormerDataClaims), record)
			cluster.Status.FormerDataClaims = in.FormerDataClaims
		}
		r.statusEvent(cluster, corev1.EventTypeWarning, reasonMemberRebuildFailed, fmt.Sprintf(
			"the rebuild of %s from %s (operation %s) failed: %s. Nothing was deleted, and nothing was stopped: a "+
				"restore on %s may still be running, and the member may join with what it restored, which is not "+
				"trusted. Its data claim %s and volume %s are never offered for a rebuild again "+
				"(status.formerDataClaims); a new request needs a new empty claim",
			ended.Member, ended.Source, ended.OperationID, why, ended.Member, ended.ClaimUID, ended.VolumeName))
	}
}

// judgeRebuildRestore decides about the accepted rebuild in.Active, whose
// restore was started. Its outcome is judged before anything that would
// withdraw it: a member that runs the database on its new claim would refuse
// a new offer, and must not end a rebuild that restored it.
//
// A write quarantine in the status, a change of the member's claim or volume
// or of the source's Pod, a failed or unrelated restore, a change of master
// or a gap in the readings ends the rebuild Failed: something may have been
// restored, so it is not Withdrawn, nothing is deleted or stopped, and the
// member's claim and volume are never offered again (endRebuild). A restore
// the member refused because its source was not confirmed as the active
// master wrote nothing, and ends the rebuild Withdrawn. Otherwise it is Completed
// only when the restore completed and the member answers as a slave that
// caught up with its source (seedVerdict, #768): a restore that finished does
// not make the member healthy. A pass whose Pods or claims could not be read
// decides nothing else.
func (r *CubridClusterReconciler) judgeRebuildRestore(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	in *rebuildInput) {
	a := in.Active
	cp := a.Copy
	fail := func(why string) { r.endRebuild(cluster, in, databasev1alpha1.MemberRebuildFailed, why) }
	if in.Quarantine != nil {
		fail(fmt.Sprintf("a write quarantine (%s) is in force after the restore on %s was started", in.Quarantine.Reason,
			a.Member))
		return
	}
	if in.Unknown {
		return
	}
	claim := in.Claims[a.Member]
	switch {
	case claim.UID != a.ClaimUID || claim.VolumeName != a.VolumeName:
		fail(fmt.Sprintf("the data claim of %s or its volume changed after the restore was started", a.Member))
		return
	case in.Identities[a.Source].PodUID != a.SourcePodUID:
		fail(fmt.Sprintf("the Pod of the source %s changed after the restore was started", a.Source))
		return
	}
	if cp.ActiveMasterSince == nil {
		// The source was the active master when the copy began; a rebuild is
		// never judged as a copy without an active master.
		at := metav1.NewTime(in.Now)
		cp.ActiveMasterSince = &at
	}
	database := cluster.Spec.Databases[0].Name
	restore, err := r.Restore.GetOperation(ctx, a.Member, cluster.Namespace, cp.RestoreOperationID)
	switch {
	case errors.Is(err, errOperationMissing):
		if cp.OperationMissingSince == nil {
			at := metav1.NewTime(in.Now)
			cp.OperationMissingSince = &at
		}
		if in.Now.Sub(cp.OperationMissingSince.Time) > bootstrapOperationMissingWait {
			fail(fmt.Sprintf("%s has held no record of the restore operation %s since %s; restoredb may still be "+
				"running there or have left data", a.Member, cp.RestoreOperationID,
				cp.OperationMissingSince.UTC().Format(time.RFC3339)))
		}
		return
	case err == nil:
		cp.OperationMissingSince = nil
		if restore.Kind != instancemanager.OpRestore || restore.Database != "" && restore.Database != database {
			fail(fmt.Sprintf("%s answers for the restore operation %s with a %s operation on database %q",
				a.Member, cp.RestoreOperationID, restore.Kind, restore.Database))
			return
		}
	default:
		logf.FromContext(ctx).Info("Could not poll the restore of a member rebuild", "member", a.Member,
			"operationID", a.OperationID, "reason", err.Error())
	}
	completed := err == nil && restore.State == instancemanager.OpCompleted
	if completed && !restoredRebuild(restore, string(cluster.UID), a.OperationID, database) {
		fail(fmt.Sprintf("the restore operation %s on %s completed without restoring the backup of this rebuild",
			cp.RestoreOperationID, a.Member))
		return
	}
	obs := rebuildReading(*in)
	gap := recordSeedReading(cp, rebuildHolders(*in, a.Member), obs, in.Now, true, completed)
	if err == nil && restore.State == instancemanager.OpFailed {
		if restore.FailureCode == instancemanager.FailureRebuildSourceUnconfirmed ||
			restore.FailureCode == instancemanager.FailureRebuildSourceNotMaster {
			// The Instance Manager asks the source before it touches the
			// target or stages anything (#1047), so nothing was restored.
			r.endRebuild(cluster, in, databasev1alpha1.MemberRebuildWithdrawn, fmt.Sprintf(
				"%s refused the restore before writing anything, because %s was not confirmed as the active master "+
					"(%s: %s)", a.Member, a.Source, restore.FailureCode, restore.FailureReason))
			return
		}
		why := restore.FailureReason
		if restore.FailureCode != "" {
			why = restore.FailureCode + ": " + why
		}
		fail(fmt.Sprintf("the restore on %s failed (%s)", a.Member, why))
		return
	}
	switch seedVerdict(cp, gap, completed, obs[a.Member], in.Now) {
	case seedSourceChanged:
		c := cp.MasterChange
		fail(fmt.Sprintf("%s reported itself %s at %s while %s was rebuilt from %s; a member copied across a change "+
			"of master can lack what the new master committed", c.Member, c.Role, c.ObservedAt.UTC().Format(time.RFC3339),
			a.Member, a.Source))
	case seedSourceUnverified:
		last := "never"
		if cp.LastVerifiedAt != nil {
			last = "last at " + cp.LastVerifiedAt.UTC().Format(time.RFC3339)
		}
		fail(fmt.Sprintf("no reading confirmed %s as the only master for more than %s (%s), so a change of master "+
			"during the copy cannot be ruled out", a.Source, seedReadingGap, last))
	case seedSeeded:
		r.endRebuild(cluster, in, databasev1alpha1.MemberRebuildCompleted, fmt.Sprintf(
			"the restore completed and %s answers as a slave of %s whose copy and applier reached the end of the "+
				"source's log read after the restore", a.Member, a.Source))
	case seedCatchingUp:
		if cp.CatchUpSince == nil {
			at := metav1.NewTime(in.Now)
			cp.CatchUpSince = &at
		}
		logf.FromContext(ctx).Info("Waiting for a rebuilt member to catch up with its source", "member", a.Member,
			"source", a.Source, "missing", catchUpMissing(cp, obs[a.Member], in.Now))
	}
}

// copyForRebuild takes the copy of the accepted rebuild in.Active, whose
// restore was not started, one step further: the backup on the source the
// request was accepted with, never one chosen again, and then the restore on
// the member. It returns why the rebuild is withdrawn instead, "" otherwise.
//
// The copy goes on only in a pass that observes that source as the only
// active master, never onto the current primary or a member that reports
// itself master, and only when both Instance Managers report the checks the
// copy relies on. Immediately before the restore is started, the write
// quarantine in the status, the source's Pod and the member's claim and
// volume, read again from the API server, must still be the accepted ones;
// otherwise nothing is started. A change of master or a gap in the readings
// before the restore withdraws the rebuild: its backup may come from before
// the change.
func (r *CubridClusterReconciler) copyForRebuild(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	in *rebuildInput) string {
	log := logf.FromContext(ctx)
	a := in.Active
	wait := func(msg string) string {
		log.Info("Waiting to copy the database for a member rebuild", "member", a.Member, "source", a.Source,
			"operationID", a.OperationID, "reason", msg)
		return ""
	}
	switch {
	case r.Backup == nil || r.Restore == nil:
		return wait("no backup and restore clients are configured")
	case len(cluster.Spec.Databases) == 0:
		return wait("the cluster has no database")
	case cluster.Spec.ObjectStorage == nil || cluster.Spec.ObjectStorage.Bucket == "":
		return wait("spec.objectStorage with a bucket is needed to copy the database")
	}
	if why := seedPrefixRefusal(cluster.Spec.ObjectStorage.Prefix); why != "" {
		return wait(why)
	}
	obs := rebuildReading(*in)
	if source, why := rebuildSource(*in, a.Member); source != a.Source {
		return wait("the source is not observed as the only active master: " + why)
	}
	if a.Member == in.CurrentPrimary || isMasterAnswer(obs[a.Member], in.Now) {
		return wait(a.Member + " is the current primary or reports itself master")
	}
	if a.Copy == nil {
		// The source answers as the active master in this pass.
		at := metav1.NewTime(in.Now)
		a.Copy = &databasev1alpha1.DatabaseSeeding{Member: a.Member, Source: a.Source, ActiveMasterSince: &at}
	}
	cp := a.Copy
	if gap := recordSeedReading(cp, rebuildHolders(*in, a.Member), obs, in.Now, false, false); gap {
		return fmt.Sprintf("no reading confirmed %s as the only master for more than %s before the restore",
			a.Source, seedReadingGap)
	}
	if c := cp.MasterChange; c != nil {
		return fmt.Sprintf("%s reported itself %s before the restore", c.Member, c.Role)
	}
	database := cluster.Spec.Databases[0].Name
	if cp.BackupOperationID == "" {
		for _, step := range []struct {
			member, name string
			checks       []string
		}{{a.Source, "rebuild backup", rebuildBackupChecks}, {a.Member, "rebuild restore", rebuildRestoreChecks}} {
			if msg, held := engineSeriesHold(cluster, step.member); held {
				return wait(msg)
			}
			if _, msg, held := capabilityHold(step.member, step.name, obs[step.member], in.Now, step.checks...); held {
				return wait(msg)
			}
		}
	}
	done, failed := r.rebuildBackup(ctx, cluster, database, a)
	if failed != "" {
		r.endRebuild(cluster, in, databasev1alpha1.MemberRebuildFailed, failed)
		return ""
	}
	if !done {
		return ""
	}
	if msg, held := engineSeriesHold(cluster, a.Member); held {
		return wait(msg)
	}
	if _, msg, held := capabilityHold(a.Member, "rebuild restore", obs[a.Member], in.Now, rebuildRestoreChecks...); held {
		return wait(msg)
	}
	if why := r.rebuildRestoreRecheck(ctx, cluster, a); why != "" {
		return wait("not starting the restore: " + why)
	}
	bucket, prefix := rebuildSeedLocation(cluster, database, a)
	uid := string(cluster.UID)
	key := rebuildRestoreKey(a)
	restore, err := r.Restore.StartRestore(ctx, a.Member, cluster.Namespace, key, instancemanager.RestoreRequest{
		Database: database, Bucket: bucket, Prefix: prefix,
		ExpectedCubridVersion: cluster.Spec.Version,
		StagingDir:            path.Join(restoreStagingRoot, instancemanager.RebuildSeedDir(a.OperationID)),
		TargetDir:             restoreTargetRoot,
		SeedFromMaster:        a.Source,
		ExpectedClusterUID:    uid,
		ExpectedMember:        a.Member,
		Rebuild:               &instancemanager.RebuildBinding{OperationID: a.OperationID, Token: a.Token},
	})
	if err != nil {
		restore, err = operationOfKey(ctx, err, instancemanager.OpRestore, key, func() (instancemanager.Operation, bool, error) {
			return r.Restore.FindOperation(ctx, a.Member, cluster.Namespace, instancemanager.OpRestore, key)
		})
	}
	if errors.Is(err, errRequestRefused) {
		r.endRebuild(cluster, in, databasev1alpha1.MemberRebuildFailed, fmt.Sprintf("%s refused the restore: %v",
			a.Member, err))
		return ""
	}
	if err != nil {
		return wait("the restore could not be started: " + err.Error())
	}
	cp.RestoreOperationID = restore.ID
	r.statusEvent(cluster, corev1.EventTypeNormal, reasonMemberRebuildRestoreStarted, fmt.Sprintf(
		"started the restore of the backup of %s on %s for the rebuild %s (restore operation %s); the rebuild "+
			"completes once %s caught up with %s", a.Source, a.Member, a.OperationID, restore.ID, a.Member, a.Source))
	return ""
}

// rebuildRestoreRecheck says why the restore of the rebuild a must not be
// started now, "" when it may: a write quarantine in the status, or a source
// Pod, member claim or volume, read again from the API server, that is not
// the accepted one, or that could not be read.
func (r *CubridClusterReconciler) rebuildRestoreRecheck(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	a *databasev1alpha1.MemberRebuild) string {
	if q := cluster.Status.WriteQuarantine; q != nil {
		return fmt.Sprintf("a write quarantine (%s) is in force", q.Reason)
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var stored databasev1alpha1.CubridCluster
	if err := reader.Get(ctx, client.ObjectKeyFromObject(cluster), &stored); err != nil {
		return fmt.Sprintf("the stored cluster could not be read: %v", err)
	}
	if q := stored.Status.WriteQuarantine; q != nil {
		return fmt.Sprintf("the stored status holds a write quarantine (%s)", q.Reason)
	}
	var pod corev1.Pod
	err := reader.Get(ctx, client.ObjectKey{Name: a.Source, Namespace: cluster.Namespace}, &pod)
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("the source %s has no Pod", a.Source)
	case err != nil:
		return fmt.Sprintf("the Pod of the source %s could not be read: %v", a.Source, err)
	case string(pod.UID) != a.SourcePodUID:
		return fmt.Sprintf("the Pod of the source %s changed since the request was accepted", a.Source)
	}
	var pvc corev1.PersistentVolumeClaim
	err = reader.Get(ctx, client.ObjectKey{Name: dataVolume + "-" + a.Member, Namespace: cluster.Namespace}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("%s has no data claim", a.Member)
	case err != nil:
		return fmt.Sprintf("the data claim of %s could not be read: %v", a.Member, err)
	case string(pvc.UID) != a.ClaimUID || pvc.Spec.VolumeName != a.VolumeName || pvc.DeletionTimestamp != nil:
		return fmt.Sprintf("the data claim of %s or its volume changed since the request was accepted", a.Member)
	}
	return ""
}

// rebuildBackup takes the backup of the rebuild a on its source and reports
// whether it completed, or why the rebuild fails. Once started, the backup's
// operation is recorded in status.memberRebuild.copy and only polled.
func (r *CubridClusterReconciler) rebuildBackup(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	database string, a *databasev1alpha1.MemberRebuild) (done bool, failed string) {
	cp := a.Copy
	bucket, prefix := rebuildSeedLocation(cluster, database, a)
	var backup instancemanager.Operation
	var err error
	if id := cp.BackupOperationID; id != "" {
		backup, err = r.Backup.GetOperation(ctx, a.Source, cluster.Namespace, id)
		if errors.Is(err, errOperationMissing) {
			if cp.OperationMissingSince == nil {
				at := metav1.NewTime(r.now())
				cp.OperationMissingSince = &at
			}
			if r.now().Sub(cp.OperationMissingSince.Time) > bootstrapOperationMissingWait {
				return false, fmt.Sprintf("%s has held no record of the backup operation %s since %s; backupdb may "+
					"still be running there and upload to s3://%s", a.Source, id,
					cp.OperationMissingSince.UTC().Format(time.RFC3339), path.Join(bucket, prefix))
			}
			return false, ""
		}
		if err == nil && (backup.Kind != instancemanager.OpBackup || backup.Database != "" && backup.Database != database) {
			return false, fmt.Sprintf("%s answers for the backup operation %s with a %s operation on database %q",
				a.Source, id, backup.Kind, backup.Database)
		}
	} else {
		key := rebuildBackupKey(a)
		uid := string(cluster.UID)
		backup, err = r.Backup.StartBackup(ctx, a.Source, cluster.Namespace, key, instancemanager.BackupRequest{
			Database:    database,
			Destination: path.Join(backupStagingRoot, instancemanager.RebuildSeedDir(a.OperationID)),
			Upload: &instancemanager.BackupUpload{
				Bucket: bucket, Prefix: prefix, ClusterUID: uid, CubridVersion: cluster.Spec.Version,
				SourceInstance: a.Source, SourceRole: string(databasev1alpha1.RoleMaster), Seed: true,
				Rebuild: &instancemanager.RebuildBinding{OperationID: a.OperationID, Token: a.Token},
			},
		})
		if err != nil {
			backup, err = operationOfKey(ctx, err, instancemanager.OpBackup, key, func() (instancemanager.Operation, bool, error) {
				return r.Backup.FindBackupOperation(ctx, a.Source, cluster.Namespace, key)
			})
		}
		if errors.Is(err, errRequestRefused) {
			return false, fmt.Sprintf("%s refused the backup: %v", a.Source, err)
		}
	}
	if err != nil {
		logf.FromContext(ctx).Info("Could not start or poll the backup of a member rebuild", "member", a.Member,
			"source", a.Source, "operationID", a.OperationID, "reason", err.Error())
		return false, ""
	}
	cp.BackupOperationID, cp.OperationMissingSince = backup.ID, nil
	switch backup.State {
	case instancemanager.OpCompleted:
		return true, ""
	case instancemanager.OpFailed:
		return false, fmt.Sprintf("the backup on %s failed: %s", a.Source, backup.FailureReason)
	}
	return false, ""
}
