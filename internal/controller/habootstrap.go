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

// haBootstrapIdempotencyKey is stable for one cluster and database, so every
// reconcile addresses the same operation.
func haBootstrapIdempotencyKey(cluster *databasev1alpha1.CubridCluster, database string) string {
	return "ha-bootstrap-" + string(cluster.UID) + "-" + database
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
	if status.PrimaryCreated || status.Phase == databasePhaseFailed {
		return
	}

	member := memberNames(cluster, 1)[0]
	op, err := r.HABootstrap.StartHABootstrap(ctx, member, cluster.Namespace,
		haBootstrapIdempotencyKey(cluster, database), instancemanager.HABootstrapRequest{Database: database})
	if err != nil {
		// The member is not up yet, or not reachable: ask again later.
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "InstanceManagerUnavailable", err.Error())
		return
	}
	switch op.State {
	case instancemanager.OpCompleted:
		status.Phase = databasePhaseCreated
		status.PrimaryCreated = true
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "PeersNotSeeded",
			"database "+database+" was created on "+member+"; the other members are not seeded yet")
		r.event(cluster, corev1.EventTypeNormal, "DatabaseCreated", "database "+database+" created on "+member)
	case instancemanager.OpFailed:
		status.Phase = databasePhaseFailed
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "DatabaseCreationFailed", op.FailureReason)
		r.event(cluster, corev1.EventTypeWarning, "DatabaseCreationFailed", op.FailureReason)
	default:
		status.Phase = databasePhaseCreating
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, "DatabaseCreationInProgress",
			"creating database "+database+" on "+member)
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
