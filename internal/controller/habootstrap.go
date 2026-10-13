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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

const (
	// haConfMountPath is where the cluster's HA ConfigMap is mounted in a DB
	// Pod; the entrypoint copies cubrid_ha.conf from there into CUBRID's own
	// conf directory.
	haConfMountPath  = "/etc/cubrid-ha"
	haConfVolumeName = "ha-config"

	// Phases of status.databases[] (ADR-0010).
	databasePhasePending  = "Pending"
	databasePhaseCreating = "Creating"
	databasePhaseCreated  = "Created"
	databasePhaseFailed   = "Failed"
)

// HABootstrapClient asks one member's Instance Manager to hold the cluster's
// first database (ADR-0010). It is an interface so the reconciler can be
// tested without a live manager.
type HABootstrapClient interface {
	// StartHABootstrap POSTs /v1/ha/bootstrap with the idempotency key and
	// returns the durable operation. A repeat with the same key returns the
	// same operation in its current state.
	StartHABootstrap(ctx context.Context, podName, namespace, idempotencyKey string, req instancemanager.HABootstrapRequest) (instancemanager.Operation, error)
	OperationFinder
}

// maxBootstrapAttempts bounds how often a failed bootstrap step is started
// again before the bootstrap stops and waits for a person.
const maxBootstrapAttempts = 3

// bootstrapKey is the idempotency key of one bootstrap step. It is stable for
// a cluster, a database and an attempt, so every reconcile addresses the same
// operation, also after the operator restarted; a step that failed is started
// again under the next attempt's key.
func bootstrapKey(step string, cluster *databasev1alpha1.CubridCluster, status *databasev1alpha1.DatabaseStatus, member string) string {
	key := fmt.Sprintf("%s-%s-%s-a%d", step, cluster.UID, status.Name, status.BootstrapAttempts)
	if member != "" {
		key += "-" + member
	}
	return key
}

// bootstrapStepFailed handles a failed step: it is started again under a new
// key while attempts remain, otherwise the bootstrap stops as Failed. The
// Instance Manager removes what its own failed operation left before the next
// attempt, and nothing else (ADR-0006).
func (r *CubridClusterReconciler) bootstrapStepFailed(cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, reason, msg string) {
	if status.BootstrapAttempts+1 < maxBootstrapAttempts {
		status.BootstrapAttempts++
		msg = fmt.Sprintf("%s; starting attempt %d of %d", msg, status.BootstrapAttempts+1, maxBootstrapAttempts)
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reason+"Retrying", msg)
		r.statusEvent(cluster, corev1.EventTypeWarning, reason+"Retrying", msg)
		return
	}
	status.Phase = databasePhaseFailed
	msg = fmt.Sprintf("%s; giving up after %d attempts", msg, maxBootstrapAttempts)
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reason, msg)
	r.statusEvent(cluster, corev1.EventTypeWarning, reason, msg)
}

// databaseStatus returns the status entry of database, adding it when absent.
func databaseStatus(cluster *databasev1alpha1.CubridCluster, database string) *databasev1alpha1.DatabaseStatus {
	for i := range cluster.Status.Databases {
		if cluster.Status.Databases[i].Name == database {
			return &cluster.Status.Databases[i]
		}
	}
	cluster.Status.Databases = append(cluster.Status.Databases,
		databasev1alpha1.DatabaseStatus{Name: database, Phase: databasePhasePending})
	return &cluster.Status.Databases[len(cluster.Status.Databases)-1]
}

// reconcileHABootstrap has the cluster's first database created exactly once,
// on one member, and records it (ADR-0010). The other members never create a
// database; they are seeded from this one.
//
// The first member is ordinal 0 only because one has to be chosen: it is where
// the database is created, not the cluster's master afterwards, which CUBRID
// decides (ADR-0001/0005).
func (r *CubridClusterReconciler) reconcileHABootstrap(ctx context.Context, cluster *databasev1alpha1.CubridCluster) {
	if r.HABootstrap == nil || !cluster.Spec.HighAvailability.Enabled || len(cluster.Spec.Databases) == 0 {
		return
	}
	// A recovery bootstrap brings its database from a backup instead.
	if cluster.Spec.Bootstrap != nil && cluster.Spec.Bootstrap.Recovery != nil {
		return
	}
	database := cluster.Spec.Databases[0].Name
	status := databaseStatus(cluster, database)
	if status.Phase == databasePhaseFailed || status.HAConfigured {
		return
	}
	member := memberNames(cluster, 1)[0]
	if status.PrimaryCreated {
		r.reconcileSeeding(ctx, cluster, status, member)
		return
	}

	key := bootstrapKey("ha-bootstrap", cluster, status, "")
	op, err := r.HABootstrap.StartHABootstrap(ctx, member, cluster.Namespace, key, instancemanager.HABootstrapRequest{Database: database})
	if err != nil {
		op, err = operationOfKey(ctx, err, instancemanager.OpHABootstrap, key, func() (instancemanager.Operation, bool, error) {
			return r.HABootstrap.FindOperation(ctx, member, cluster.Namespace, instancemanager.OpHABootstrap, key)
		})
	}
	if errors.Is(err, errRequestRefused) {
		// The member refuses the request itself, and would again.
		r.bootstrapStepFailed(cluster, status, "DatabaseCreationFailed", err.Error())
		return
	}
	if err != nil {
		// The member is not up yet, or not reachable: ask again later.
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, instanceManagerFailureReason(err), err.Error())
		return
	}
	switch op.State {
	case instancemanager.OpCompleted:
		status.Phase = databasePhaseCreated
		status.PrimaryCreated = true
		r.statusEvent(cluster, corev1.EventTypeNormal, "DatabaseCreated", "database "+database+" created on "+member)
		r.reconcileSeeding(ctx, cluster, status, member)
	case instancemanager.OpFailed:
		r.bootstrapStepFailed(cluster, status, "DatabaseCreationFailed", "on "+member+": "+op.FailureReason)
	default:
		status.Phase = databasePhaseCreating
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "DatabaseCreationInProgress",
			"creating database "+database+" on "+member)
	}
}

// seedSourceAmong names the member a new member is copied from, among the
// members that hold the database: the one that reports itself master while
// every other holder reports itself a slave. Every holder has to answer, and
// freshly; with no master, with two, or with a holder whose answer is missing
// or not understood there is no source, and why says what was seen. A master
// whose server is not active yet counts: no master's server is active before
// every member has joined once (docs/poc/RESULTS.md, POC-23).
func seedSourceAmong(holders []string, obs map[string]RoleObservation, now time.Time) (source, why string) {
	var masters []string
	for _, m := range holders {
		o, ok := obs[m]
		switch {
		case !ok || !fresh(o, now):
			return "", m + " did not answer"
		case o.Role == databasev1alpha1.RoleMaster && (!o.Conflicting || o.MasterNotYetActive):
			masters = append(masters, m)
		case o.Role == databasev1alpha1.RoleSlave && !o.Conflicting:
		default:
			return "", m + " reports no role that can be relied on"
		}
	}
	switch len(masters) {
	case 0:
		return "", "none of " + strings.Join(holders, ", ") + " reports itself master"
	case 1:
		return masters[0], ""
	default:
		return "", strings.Join(masters, " and ") + " both report themselves master"
	}
}

// seedLocation is where a seed backup of database lives in object storage:
// the backup the first peer is copied from, or, with artifact, the one taken
// for a later member on its source, which has a place of its own.
func seedLocation(cluster *databasev1alpha1.CubridCluster, database, artifact string) (bucket, prefix string) {
	storage := cluster.Spec.ObjectStorage
	return storage.Bucket, path.Join(storage.Prefix, string(cluster.UID), seedLocationDir(artifact), database)
}

// seedLocationDir is the directory below the cluster's UID that names the
// seed backup of artifact: its source and round.
func seedLocationDir(artifact string) string {
	if artifact == "" {
		return "seed"
	}
	return "seed-" + artifact
}

// restoredSeed reports whether the restore operation op restored the seed
// backup of artifact for database of the cluster uid. The bucket and the
// prefix above the UID are not compared: they follow spec.objectStorage,
// which may have changed since the restore was started.
func restoredSeed(op instancemanager.Operation, uid, artifact, database string) bool {
	restored := op.Restored
	if restored == nil {
		restored = op.Artifact
	}
	return restored != nil && strings.HasSuffix(restored.ManifestURI,
		"/"+path.Join(uid, seedLocationDir(artifact), database, instancemanager.ManifestObjectName))
}

// selectSeedSource decides which member peer is copied from, or says why it
// cannot be decided yet.
//
// While no peer has joined, it is the member that holds the database: its
// server is not active and no primary resolves (docs/poc/RESULTS.md, POC-13),
// so there is nobody to ask and nobody else to copy from. Once a peer has
// joined, CUBRID may have made any holder the master, and a member copied
// from a member that is not the master, or from a backup taken on one, lacks
// what the master committed without showing it (POC-23). The source is then
// the holder that is observed as the master, and never a guess: an unanswered
// or contradictory observation means waiting (ADR-0005).
func (r *CubridClusterReconciler) selectSeedSource(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, holder string) (source, why string) {
	if len(status.SeededMembers) == 0 {
		return holder, ""
	}
	if r.Prober == nil {
		return "", "no role prober is configured"
	}
	holders := append([]string{holder}, status.SeededMembers...)
	obs := make(map[string]RoleObservation, len(holders))
	for _, m := range holders {
		obs[m] = r.Prober.ProbeRole(ctx, m, cluster.Namespace)
	}
	return seedSourceAmong(holders, obs, r.now())
}

// reconcileSeeding copies the first database to the other members (ADR-0006,
// ADR-0010), one at a time: a backup of the source, uploaded to the cluster's
// object storage, then a restore on the member. The Instance Manager of a
// seeded member starts heartbeat itself, so the member joins as a slave. Every
// step is an idempotent operation with a fixed key, so a reconcile after any
// interruption asks for the same step.
//
// holder is the member the database was created or restored on. It is the
// source of the first peer. The source of every later member is the member
// that is observed as the master (selectSeedSource, seedMember). A member
// that holds the database is never restored over.
func (r *CubridClusterReconciler) reconcileSeeding(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, holder string) {
	database := status.Name
	// A recovery bootstrap restored the database on the holder; otherwise it
	// was created there.
	origin := "database " + database + " was created on " + holder
	if cluster.Spec.Bootstrap != nil && cluster.Spec.Bootstrap.Recovery != nil {
		origin = "database " + database + " was restored on " + holder
	}
	if r.Backup == nil || r.Restore == nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "PeersNotSeeded",
			origin+"; no backup and restore clients are configured to seed the other members")
		return
	}
	if cluster.Spec.ObjectStorage == nil || cluster.Spec.ObjectStorage.Bucket == "" {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedStorageNotConfigured",
			origin+
				"; spec.objectStorage with a bucket is needed to copy it to the other members")
		return
	}

	for _, peer := range memberNames(cluster, cluster.Spec.Topology.PromotableMembers) {
		// A member that holds the database is never restored over.
		if peer == holder || slices.Contains(status.SeededMembers, peer) {
			continue
		}
		if !r.seedMember(ctx, cluster, status, holder, peer) {
			return
		}
	}

	status.Seeding = nil
	status.HAConfigured = true
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionTrue, "PeersSeeded",
		origin+" and copied to the other members")
	r.statusEvent(cluster, corev1.EventTypeNormal, "PeersSeeded", "database "+database+" copied to the other members")
}

// seedMember takes the copy of the database to peer one step further and
// reports whether peer holds it now. The condition says what it waits for
// otherwise.
//
// The first peer is copied from holder, under the key and at the place its
// backup always had. For a later member the master among the holders is asked
// for at every reconcile until its restore is started: a master that changes
// meanwhile is followed, with a backup of its own for every change (a round).
// The restore is started only by a reconcile that found the source it
// observes already recorded in the status, so a status that was not written
// cannot leave a restore running that nobody remembers the source of. Once
// the restore's operation is recorded, the source is kept whatever the roles
// become.
func (r *CubridClusterReconciler) seedMember(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, holder, peer string) bool {
	database := status.Name
	source, artifact := holder, ""
	// sourceRecorded says that the source in use was already in the status
	// when this reconcile read it; restoreStarted that its restore is known
	// to run or to have ended.
	sourceRecorded, restoreStarted := true, false
	if len(status.SeededMembers) > 0 {
		seeding := status.Seeding
		if seeding != nil && seeding.Member != peer {
			seeding = nil
		}
		restoreStarted = seeding != nil && seeding.RestoreOperationID != ""
		if !restoreStarted {
			observed, why := r.selectSeedSource(ctx, cluster, status, holder)
			if observed == "" {
				setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedSourceNotResolved",
					"waiting to copy database "+database+" to "+peer+": the master among the members that hold it is not known ("+why+")")
				return false
			}
			switch {
			case seeding == nil:
				seeding = &databasev1alpha1.DatabaseSeeding{Member: peer, Source: observed}
				sourceRecorded = false
			case seeding.Source != observed:
				seeding = &databasev1alpha1.DatabaseSeeding{Member: peer, Source: observed, Round: seeding.Round + 1}
				sourceRecorded = false
			}
			if !sourceRecorded && observed != holder {
				r.statusEvent(cluster, corev1.EventTypeNormal, "SeedSourceChanged",
					"database "+database+" will be copied to "+peer+" from "+observed+", the master; "+holder+" is no longer the master")
			}
		}
		status.Seeding = seeding
		source = seeding.Source
		artifact = peer + "-from-" + source
		if seeding.Round > 0 {
			artifact += fmt.Sprintf("-r%d", seeding.Round)
		}
	}

	bucket, prefix := seedLocation(cluster, database, artifact)
	uid := string(cluster.UID)
	// A restore that was started has its backup; the source is not asked
	// again, so that it need not be reachable while the member is restored.
	if !restoreStarted {
		staging := "seed-" + uid
		if artifact != "" {
			staging += "-" + peer
		}
		backupKey := bootstrapKey("seed-backup", cluster, status, artifact)
		backup, err := r.Backup.StartBackup(ctx, source, cluster.Namespace, backupKey,
			instancemanager.BackupRequest{
				Database:    database,
				Destination: path.Join(backupStagingRoot, staging),
				Upload: &instancemanager.BackupUpload{
					Bucket: bucket, Prefix: prefix, ClusterUID: uid, CubridVersion: cluster.Spec.Version,
					SourceInstance: source, SourceRole: string(databasev1alpha1.RoleMaster),
				},
			})
		if err != nil {
			backup, err = operationOfKey(ctx, err, instancemanager.OpBackup, backupKey, func() (instancemanager.Operation, bool, error) {
				return r.Backup.FindBackupOperation(ctx, source, cluster.Namespace, backupKey)
			})
		}
		if errors.Is(err, errRequestRefused) {
			status.Seeding = nil
			r.bootstrapStepFailed(cluster, status, "SeedBackupFailed", err.Error())
			return false
		}
		if err != nil {
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, instanceManagerFailureReason(err), err.Error())
			return false
		}
		switch backup.State {
		case instancemanager.OpCompleted:
		case instancemanager.OpFailed:
			status.Seeding = nil
			r.bootstrapStepFailed(cluster, status, "SeedBackupFailed", "on "+source+": "+backup.FailureReason)
			return false
		default:
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedBackupInProgress",
				"backing up database "+database+" on "+source)
			return false
		}
		if !sourceRecorded {
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedSourceSelected",
				"database "+database+" will be copied to "+peer+" from "+source+", the master")
			return false
		}
	}

	// The restore of a later member is keyed by its source and round, as
	// its backup is, so a key never names the restore of another source:
	// one that a lost status write left running is never taken for a
	// later round's (ADR-0005).
	restoreKey := bootstrapKey("seed-restore", cluster, status, peer)
	if artifact != "" {
		restoreKey = bootstrapKey("seed-restore", cluster, status, artifact)
	}
	restore, err := r.Restore.StartRestore(ctx, peer, cluster.Namespace, restoreKey,
		instancemanager.RestoreRequest{
			Database: database, Bucket: bucket, Prefix: prefix,
			ExpectedCubridVersion: cluster.Spec.Version,
			StagingDir:            path.Join(restoreStagingRoot, "seed-"+uid),
			TargetDir:             restoreTargetRoot,
			// The member becomes a slave of the member the backup was taken
			// on, and has to pick up what that member committed since.
			SeedFromMaster: source,
		})
	followed := false
	if err != nil {
		restore, err = operationOfKey(ctx, err, instancemanager.OpRestore, restoreKey, func() (instancemanager.Operation, bool, error) {
			return r.Restore.FindOperation(ctx, peer, cluster.Namespace, instancemanager.OpRestore, restoreKey)
		})
		followed = err == nil
	}
	if errors.Is(err, errRequestRefused) {
		status.Seeding = nil
		r.bootstrapStepFailed(cluster, status, "SeedRestoreFailed", err.Error())
		return false
	}
	if err != nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, instanceManagerFailureReason(err), err.Error())
		return false
	}
	// A followed restore was started with another request than this one.
	// It seeds the member only when what it restored is this step's backup
	// of this source and round; anything else is left for a person.
	if followed && restore.State == instancemanager.OpCompleted && !restoredSeed(restore, string(cluster.UID), artifact, database) {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedRestoreSourceMismatch",
			fmt.Sprintf("the restore operation %s under the key %q on %s completed, but it did not restore the backup "+
				"of this step (%s); %s is not recorded as seeded, and the operation is left for a person",
				restore.ID, restoreKey, peer, seedLocationDir(artifact), peer))
		return false
	}
	if status.Seeding != nil {
		status.Seeding.RestoreOperationID = restore.ID
	}
	switch restore.State {
	case instancemanager.OpCompleted:
		status.SeededMembers = append(status.SeededMembers, peer)
		status.Seeding = nil
		return true
	case instancemanager.OpFailed:
		// The next attempt chooses its source again.
		status.Seeding = nil
		r.bootstrapStepFailed(cluster, status, "SeedRestoreFailed", "on "+peer+": "+restore.FailureReason)
		return false
	default:
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedRestoreInProgress",
			"restoring database "+database+" on "+peer)
		return false
	}
}

// haConfVolume and haConfMount give an HA member the cluster's cubrid_ha.conf.
func haConfVolume(cluster *databasev1alpha1.CubridCluster) corev1.Volume {
	return corev1.Volume{
		Name: haConfVolumeName,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: haConfigMapName(cluster.Name)},
		}},
	}
}

func haConfMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: haConfVolumeName, MountPath: haConfMountPath, ReadOnly: true}
}
