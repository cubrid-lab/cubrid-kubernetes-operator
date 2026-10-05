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
	"slices"

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
		r.event(cluster, corev1.EventTypeWarning, reason+"Retrying", msg)
		return
	}
	status.Phase = databasePhaseFailed
	msg = fmt.Sprintf("%s; giving up after %d attempts", msg, maxBootstrapAttempts)
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, reason, msg)
	r.event(cluster, corev1.EventTypeWarning, reason, msg)
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

	op, err := r.HABootstrap.StartHABootstrap(ctx, member, cluster.Namespace,
		bootstrapKey("ha-bootstrap", cluster, status, ""), instancemanager.HABootstrapRequest{Database: database})
	if err != nil {
		// The member is not up yet, or not reachable: ask again later.
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "InstanceManagerUnavailable", err.Error())
		return
	}
	switch op.State {
	case instancemanager.OpCompleted:
		status.Phase = databasePhaseCreated
		status.PrimaryCreated = true
		r.event(cluster, corev1.EventTypeNormal, "DatabaseCreated", "database "+database+" created on "+member)
		r.reconcileSeeding(ctx, cluster, status, member)
	case instancemanager.OpFailed:
		r.bootstrapStepFailed(cluster, status, "DatabaseCreationFailed", "on "+member+": "+op.FailureReason)
	default:
		status.Phase = databasePhaseCreating
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "DatabaseCreationInProgress",
			"creating database "+database+" on "+member)
	}
}

// seedLocation is where the seed backup of database lives in object storage.
func seedLocation(cluster *databasev1alpha1.CubridCluster, database string) (bucket, prefix string) {
	storage := cluster.Spec.ObjectStorage
	return storage.Bucket, path.Join(storage.Prefix, string(cluster.UID), "seed", database)
}

// reconcileSeeding copies the first database to the other members (ADR-0006,
// ADR-0010): one backup of the member that holds it, uploaded to the
// cluster's object storage, then a restore on each other member, one at a
// time. The Instance Manager of a seeded member starts heartbeat itself, so
// the member joins as a slave. Every step is an idempotent operation with a
// fixed key, so a reconcile after any interruption asks for the same step.
//
// The source is the member the database was created on, not a resolved
// master: until a peer has joined, that member's server is not active and no
// primary resolves (docs/poc/RESULTS.md, POC-13).
func (r *CubridClusterReconciler) reconcileSeeding(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	status *databasev1alpha1.DatabaseStatus, source string) {
	database := status.Name
	if r.Backup == nil || r.Restore == nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "PeersNotSeeded",
			"database "+database+" was created on "+source+"; no backup and restore clients are configured to seed the other members")
		return
	}
	if cluster.Spec.ObjectStorage == nil || cluster.Spec.ObjectStorage.Bucket == "" {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedStorageNotConfigured",
			"database "+database+" was created on "+source+
				"; spec.objectStorage with a bucket is needed to copy it to the other members")
		return
	}
	bucket, prefix := seedLocation(cluster, database)
	uid := string(cluster.UID)

	backup, err := r.Backup.StartBackup(ctx, source, cluster.Namespace, bootstrapKey("seed-backup", cluster, status, ""),
		instancemanager.BackupRequest{
			Database:    database,
			Destination: path.Join(backupStagingRoot, "seed-"+uid),
			Upload: &instancemanager.BackupUpload{
				Bucket: bucket, Prefix: prefix, ClusterUID: uid, CubridVersion: cluster.Spec.Version,
				SourceInstance: source, SourceRole: string(databasev1alpha1.RoleMaster),
			},
		})
	if err != nil {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "InstanceManagerUnavailable", err.Error())
		return
	}
	switch backup.State {
	case instancemanager.OpCompleted:
	case instancemanager.OpFailed:
		r.bootstrapStepFailed(cluster, status, "SeedBackupFailed", "on "+source+": "+backup.FailureReason)
		return
	default:
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedBackupInProgress",
			"backing up database "+database+" on "+source)
		return
	}

	for _, peer := range memberNames(cluster, cluster.Spec.Topology.PromotableMembers)[1:] {
		// A member that holds the database is never restored over.
		if slices.Contains(status.SeededMembers, peer) {
			continue
		}
		restore, err := r.Restore.StartRestore(ctx, peer, cluster.Namespace, bootstrapKey("seed-restore", cluster, status, peer),
			instancemanager.RestoreRequest{
				Database: database, Bucket: bucket, Prefix: prefix,
				ExpectedCubridVersion: cluster.Spec.Version,
				StagingDir:            path.Join(restoreStagingRoot, "seed-"+uid),
				TargetDir:             restoreTargetRoot,
				// The member becomes a slave of the member the backup was taken
				// on, and has to pick up what that member committed since.
				SeedFromMaster: source,
			})
		if err != nil {
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "InstanceManagerUnavailable", err.Error())
			return
		}
		switch restore.State {
		case instancemanager.OpCompleted:
			status.SeededMembers = append(status.SeededMembers, peer)
			continue
		case instancemanager.OpFailed:
			r.bootstrapStepFailed(cluster, status, "SeedRestoreFailed", "on "+peer+": "+restore.FailureReason)
			return
		default:
			setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "SeedRestoreInProgress",
				"restoring database "+database+" on "+peer)
			return
		}
	}

	status.HAConfigured = true
	setCondition(cluster, conditionBootstrapReady, metav1.ConditionTrue, "PeersSeeded",
		"database "+database+" was created on "+source+" and copied to the other members")
	r.event(cluster, corev1.EventTypeNormal, "PeersSeeded", "database "+database+" copied to the other members")
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
