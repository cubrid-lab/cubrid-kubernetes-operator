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
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
	// GetOperation polls /v1/operations/{id}.
	GetOperation(ctx context.Context, podName, namespace, id string) (instancemanager.Operation, error)
	OperationFinder
}

// bootstrapOperationMissingWait bounds how long a step of the HA bootstrap
// waits for its recorded operation while the member answers that it holds no
// record of it, as long as a recovery restore waits
// (recoveryOperationMissingWait).
const bootstrapOperationMissingWait = recoveryOperationMissingWait

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

// stepOperationMissing is the poll of a recorded step of the HA bootstrap
// whose member answered that it holds no record of the operation id (#534),
// for example after its operation store was lost or replaced. The step waits
// from *since, which only a read of the operation clears, so that an operator
// restart or a transient failure in between does not reset the bound. Once
// the record has stayed missing for bootstrapOperationMissingWait the
// database is Failed. A missing record does not show that the step ended:
// running says what may still run, and left what is left for a person. It is
// not counted as a failed attempt, and the step is never started again.
func (r *CubridClusterReconciler) stepOperationMissing(cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, since **metav1.Time, member, step, id string, err error, running, left string) {
	if *since == nil {
		t := metav1.NewTime(r.now())
		*since = &t
	}
	at := (*since).Time
	if r.now().Sub(at) > bootstrapOperationMissingWait {
		r.bootstrapStopped(cluster, status, reasonRecordedOperationLost, fmt.Sprintf(
			"%s has held no record of the %s operation %s since %s, for %s or longer (%v). A missing record does "+
				"not show that the %s ended: %s. The %s is not started again; %s",
			member, step, id, at.UTC().Format(time.RFC3339), bootstrapOperationMissingWait, err, step, running, step, left))
		return
	}
	// The answer is left out of this message: it carries the ID of each
	// request, and the condition would change at every poll.
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reasonRecordedOperationMissing, fmt.Sprintf(
		"%s holds no record of the %s operation %s; waiting for at most %s from %s for it to be read again",
		member, step, id, bootstrapOperationMissingWait, at.UTC().Format(time.RFC3339)))
}

// bootstrapRemedy is what a person does about a database whose bootstrap
// stopped on a recorded operation or record it cannot rely on.
const bootstrapRemedy = "delete and create the CubridCluster again to bootstrap it anew. Do not clear the " +
	"recorded operation or the seeding record in the status by hand: the step would be started again under " +
	"the same key (ADR-0010)"

// bootstrapStopped ends the bootstrap of a database as Failed for reason,
// without using an attempt: the step is not started again, and the database
// waits for a person. msg ends with what that person checks; the remedy is
// added to it.
func (r *CubridClusterReconciler) bootstrapStopped(cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, reason, msg string) {
	status.Phase = databasePhaseFailed
	msg += ", and " + bootstrapRemedy
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reason, msg)
	r.statusEvent(cluster, corev1.EventTypeWarning, reason, msg)
}

// recordedOperationMatches reports whether op, read by the ID recorded for a
// step on member, is an operation of kind on the database of status. An
// operation that names no database is not compared by it. A mismatch does
// not show what the step did, so the database is stopped for a person, with
// no attempt used and nothing started.
func (r *CubridClusterReconciler) recordedOperationMatches(cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, op instancemanager.Operation, kind instancemanager.OperationKind, member, step string) bool {
	why := ""
	switch {
	case op.Kind != kind:
		why = fmt.Sprintf("a %s operation", op.Kind)
	case op.Database != "" && op.Database != status.Name:
		why = "an operation on database " + op.Database
	default:
		return true
	}
	r.bootstrapStopped(cluster, status, reasonRecordedOperationMismatch, fmt.Sprintf(
		"%s answers for the %s operation %s recorded for database %s with %s. It does not show whether the %s "+
			"ran or ended. The %s is not started again; check %s before relying on its data",
		member, step, op.ID, status.Name, why, step, step, member))
	return false
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

	// A started operation is only polled: a member that lost its record
	// would answer a start under the same key as new.
	var op instancemanager.Operation
	var err error
	if id := status.BootstrapOperationID; id != "" {
		op, err = r.HABootstrap.GetOperation(ctx, member, cluster.Namespace, id)
		if errors.Is(err, errOperationMissing) {
			r.stepOperationMissing(cluster, status, &status.BootstrapOperationMissingSince, member, "HA bootstrap", id, err,
				"createdb may still be running on "+member+", or have left data there",
				"the database is not recorded as created; check "+member+" before relying on its data")
			return
		}
		if err == nil && !r.recordedOperationMatches(cluster, status, op, instancemanager.OpHABootstrap, member, "HA bootstrap") {
			status.BootstrapOperationMissingSince = nil
			return
		}
	} else {
		key := bootstrapKey("ha-bootstrap", cluster, status, "")
		op, err = r.HABootstrap.StartHABootstrap(ctx, member, cluster.Namespace, key, instancemanager.HABootstrapRequest{Database: database})
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
	}
	if err != nil {
		// The member is not up yet, or not reachable: ask again later.
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, instanceManagerFailureReason(err), err.Error())
		return
	}
	status.BootstrapOperationID, status.BootstrapOperationMissingSince = op.ID, nil
	switch op.State {
	case instancemanager.OpCompleted:
		status.Phase = databasePhaseCreated
		status.PrimaryCreated = true
		r.statusEvent(cluster, corev1.EventTypeNormal, "DatabaseCreated", "database "+database+" created on "+member)
		r.reconcileSeeding(ctx, cluster, status, member)
	case instancemanager.OpFailed:
		status.BootstrapOperationID = ""
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
// for a later member on its source, which has a place of its own. The prefix
// is a clean key relative to the bucket, the only form the Instance Manager
// accepts: a leading slash of spec.objectStorage.prefix, which the API does
// not refuse, is left out. A prefix that leads above the bucket is refused
// before (seedPrefixRefusal).
func seedLocation(cluster *databasev1alpha1.CubridCluster, database, artifact string) (bucket, prefix string) {
	storage := cluster.Spec.ObjectStorage
	return storage.Bucket, path.Join(strings.TrimLeft(storage.Prefix, "/"), string(cluster.UID), seedLocationDir(artifact), database)
}

// seedPrefixRefusal says why spec.objectStorage.prefix cannot hold the seed
// backups, or "" when it can. A ".." inside the prefix is resolved, as for a
// CubridBackup destination; one that leads above the bucket names no key in
// it, and the Instance Manager refuses such an upload for good.
func seedPrefixRefusal(prefix string) string {
	cleaned := path.Clean(strings.TrimLeft(prefix, "/"))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Sprintf("spec.objectStorage.prefix %q leads above the bucket with \"..\" and names no key for the seed backups", prefix)
	}
	return ""
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
	if why := seedPrefixRefusal(cluster.Spec.ObjectStorage.Prefix); why != "" {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedStoragePrefixInvalid", origin+"; "+why)
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
//
// Every operation of a copy is recorded in status.databases[].seeding once
// it was started, and only polled from then on: a member that lost its
// record would answer a start under the same key as new. Once the restore is
// recorded, the seed backup is not asked for again (#534).
func (r *CubridClusterReconciler) seedMember(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, holder, peer string) bool {
	database := status.Name
	seeding := status.Seeding
	if seeding != nil && seeding.Member != peer {
		seeding = nil
	}
	// sourceRecorded says that the source in use was already in the status
	// when this reconcile read it; restoreStarted that its restore is known
	// to run or to have ended.
	sourceRecorded, restoreStarted := true, seeding != nil && seeding.RestoreOperationID != ""
	artifact := ""
	if len(status.SeededMembers) == 0 {
		switch {
		case seeding == nil:
			seeding = &databasev1alpha1.DatabaseSeeding{Member: peer, Source: holder}
		case seeding.Source != holder:
			// The record and its operations are kept: they contradict how a
			// first peer is copied, and are left for a person.
			r.bootstrapStopped(cluster, status, reasonSeedSourceContradicted, fmt.Sprintf(
				"the copy of database %s to %s is recorded with %s as its source, but the first peer is copied only "+
					"from %s, which holds the database. %s is not recorded as seeded, and the copy is not started "+
					"again; check %s before relying on its data",
				database, peer, seeding.Source, holder, peer, peer))
			return false
		}
	} else {
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
		artifact = peer + "-from-" + seeding.Source
		if seeding.Round > 0 {
			artifact += fmt.Sprintf("-r%d", seeding.Round)
		}
	}
	status.Seeding = seeding
	source := seeding.Source

	bucket, prefix := seedLocation(cluster, database, artifact)
	uid := string(cluster.UID)
	// The restore of a later member is keyed by its source and round, as
	// its backup is, so a key never names the restore of another source:
	// one that a lost status write left running is never taken for a
	// later round's (ADR-0005).
	keyMember := peer
	if artifact != "" {
		keyMember = artifact
	}
	restoreKey := bootstrapKey("seed-restore", cluster, status, keyMember)
	restoreReq := instancemanager.RestoreRequest{
		Database: database, Bucket: bucket, Prefix: prefix,
		ExpectedCubridVersion: cluster.Spec.Version,
		StagingDir:            path.Join(restoreStagingRoot, "seed-"+uid),
		TargetDir:             restoreTargetRoot,
		// The member becomes a slave of the member the backup was taken
		// on, and has to pick up what that member committed since.
		SeedFromMaster: source,
		// The member refuses a backup that was not taken in this cluster.
		ExpectedClusterUID: uid,
	}
	var restore instancemanager.Operation
	var err error
	// followed says that the restore may have been started with another
	// request than this one.
	followed := false
	if restoreStarted {
		// A restore that was started has its backup; the source is not asked
		// again, so that it need not be reachable while the member is
		// restored. The restore may have been followed when it was recorded.
		var polled bool
		if restore, polled, err = r.recordedSeedRestore(ctx, cluster, status, peer); !polled {
			return false
		}
		followed = true
	} else {
		// A retry whose restore is predicted to take over what an earlier
		// attempt's restore of this step's backup restored takes no new
		// backup: the Instance Manager would only start that data and never
		// restore the new backup (ADR-0008, #463).
		var reused bool
		reused, err = r.retrySkipsSeedBackup(ctx, cluster, status, peer, keyMember, artifact, restoreKey, restoreReq)
		if err != nil {
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, instanceManagerFailureReason(err),
				"waiting to copy database "+database+" to "+peer+": a restore of this step could not be looked up: "+err.Error())
			return false
		}
		if !reused && !r.seedBackup(ctx, cluster, status, source, peer, artifact) {
			return false
		}
		if !sourceRecorded {
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedSourceSelected",
				"database "+database+" will be copied to "+peer+" from "+source+", the master")
			return false
		}
		restore, err = r.Restore.StartRestore(ctx, peer, cluster.Namespace, restoreKey, restoreReq)
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
	}
	if err != nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, instanceManagerFailureReason(err), err.Error())
		return false
	}
	seeding.OperationMissingSince = nil
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
	seeding.RestoreOperationID = restore.ID
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

// recordedSeedRestore polls the restore recorded for the copy to peer. It
// reports false when the member holds no record of it, or the operation it
// reads contradicts the step; the condition says so. Any other failed read
// is returned.
func (r *CubridClusterReconciler) recordedSeedRestore(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, peer string) (instancemanager.Operation, bool, error) {
	seeding := status.Seeding
	id := seeding.RestoreOperationID
	restore, err := r.Restore.GetOperation(ctx, peer, cluster.Namespace, id)
	if errors.Is(err, errOperationMissing) {
		r.stepOperationMissing(cluster, status, &seeding.OperationMissingSince, peer, "seed restore", id, err,
			"restoredb may still be running on "+peer+", or have left data there",
			peer+" is not recorded as seeded; check it before relying on its data")
		return restore, false, nil
	}
	if err == nil && !r.recordedOperationMatches(cluster, status, restore, instancemanager.OpRestore, peer, "seed restore") {
		seeding.OperationMissingSince = nil
		return restore, false, nil
	}
	return restore, true, err
}

// seedBackup takes the seed backup of artifact on source, for peer, and
// reports whether it has completed. The condition says what it waits for
// otherwise; a backup that failed ends the attempt. Once the backup was
// started, its operation is recorded in status.databases[].seeding, which
// holds the copy to peer, and only polled.
func (r *CubridClusterReconciler) seedBackup(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, source, peer, artifact string) bool {
	database := status.Name
	uid := string(cluster.UID)
	bucket, prefix := seedLocation(cluster, database, artifact)
	seeding := status.Seeding
	var backup instancemanager.Operation
	var err error
	if id := seeding.BackupOperationID; id != "" {
		backup, err = r.Backup.GetOperation(ctx, source, cluster.Namespace, id)
		if errors.Is(err, errOperationMissing) {
			r.stepOperationMissing(cluster, status, &seeding.OperationMissingSince, source, "seed backup", id, err,
				"backupdb may still be running on "+source+" and upload to s3://"+path.Join(bucket, prefix),
				peer+" is not recorded as seeded; check "+source+" and that prefix")
			return false
		}
		if err == nil && !r.recordedOperationMatches(cluster, status, backup, instancemanager.OpBackup, source, "seed backup") {
			seeding.OperationMissingSince = nil
			return false
		}
	} else {
		staging := "seed-" + uid
		if artifact != "" {
			staging += "-" + peer
		}
		backupKey := bootstrapKey("seed-backup", cluster, status, artifact)
		backup, err = r.Backup.StartBackup(ctx, source, cluster.Namespace, backupKey,
			instancemanager.BackupRequest{
				Database:    database,
				Destination: path.Join(backupStagingRoot, staging),
				Upload: &instancemanager.BackupUpload{
					Bucket: bucket, Prefix: prefix, ClusterUID: uid, CubridVersion: cluster.Spec.Version,
					SourceInstance: source, SourceRole: string(databasev1alpha1.RoleMaster),
					Seed: true,
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
	}
	if err != nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, instanceManagerFailureReason(err), err.Error())
		return false
	}
	seeding.BackupOperationID, seeding.OperationMissingSince = backup.ID, nil
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
	return true
}

// retrySkipsSeedBackup reports whether a retry of the seed step of peer needs
// no new seed backup. That is so once this attempt has a restore under its own
// key, restoreKey: its backup step is behind it. Otherwise it is so only when
// the Instance Manager of peer is predicted to take over, for req, the data
// that an earlier attempt's restore of this step restored, and so to restore
// nothing for it (ADR-0008). The latest earlier attempt that has a restore
// under this step's key decides. The key names the member and, for a later
// member, the source and the round, but a round counts only within one
// attempt, because a failed attempt clears status.databases[].seeding: an
// earlier attempt of the same key may have copied from the same source before
// a failover and back (ADR-0010). The earlier restore counts only when it
// failed after it recorded this step's seed backup as restored, and was
// started with the same request body, which the manager compares by its hash.
// The prediction rests on that operation record; the manager itself decides
// by the ownership marker on the data, which is not visible here. Anything
// else, also an earlier attempt without a restore, answers false: the step
// then takes a new backup and restores it from the same place. The first
// attempt looks nothing up.
func (r *CubridClusterReconciler) retrySkipsSeedBackup(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, peer, keyMember, artifact, restoreKey string, req instancemanager.RestoreRequest) (bool, error) {
	if status.BootstrapAttempts == 0 {
		return false, nil
	}
	_, started, err := r.Restore.FindOperation(ctx, peer, cluster.Namespace, instancemanager.OpRestore, restoreKey)
	if err != nil || started {
		return started, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	hash := instancemanager.HashRequest(body)
	earlier := *status
	for earlier.BootstrapAttempts > 0 {
		earlier.BootstrapAttempts--
		key := bootstrapKey("seed-restore", cluster, &earlier, keyMember)
		op, found, err := r.Restore.FindOperation(ctx, peer, cluster.Namespace, instancemanager.OpRestore, key)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		reused := op.State == instancemanager.OpFailed && op.Restored != nil && op.RequestHash == hash &&
			restoredSeed(op, string(cluster.UID), artifact, status.Name)
		if reused {
			logf.FromContext(ctx).V(1).Info("Reusing the seed backup that an earlier attempt restored",
				"member", peer, "source", req.SeedFromMaster, "location", seedLocationDir(artifact), "operationID", op.ID)
		}
		return reused, nil
	}
	return false, nil
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
