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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
//
// The spec/status below encode the accepted P0 ADRs:
//   ADR-0001 topology (promotableMembers/readReplicas; runtime-decided master)
//   ADR-0010 database lifecycle / ha_db_list
//   ADR-0005 fencingPolicy (Disabled|Manual|Automatic; Automatic rejected in v1alpha1)
//   ADR-0008 restore-as-bootstrap (spec.bootstrap.recovery.manifestUri)
//   status model: currentPrimary (nullable), instances[].role, PrimaryResolved condition.

// CubridRole is the CUBRID HA runtime role of an instance, decided at runtime
// by CUBRID heartbeat (ADR-0001). It is reported in status only; the spec never
// pins a role.
// +kubebuilder:validation:Enum=master;slave;replica;unknown
type CubridRole string

const (
	// RoleMaster is the currently writable CUBRID HA node.
	RoleMaster CubridRole = "master"
	// RoleSlave is a failover-capable HA node (ha_mode=on).
	RoleSlave CubridRole = "slave"
	// RoleReplica is a non-promotable replication target (ha_mode=replica).
	RoleReplica CubridRole = "replica"
	// RoleUnknown means the role could not be authoritatively determined.
	RoleUnknown CubridRole = "unknown"
)

// FencingPolicy controls operator fencing behavior (ADR-0005). v1alpha1 honors
// only Disabled and Manual; Automatic is rejected by validation.
// +kubebuilder:validation:Enum=Disabled;Manual;Automatic
type FencingPolicy string

const (
	// FencingDisabled performs no operator fencing.
	FencingDisabled FencingPolicy = "Disabled"
	// FencingManual surfaces conditions/events for manual intervention (default).
	FencingManual FencingPolicy = "Manual"
	// FencingAutomatic is reserved for a future active-fencing ADR (rejected in v1alpha1).
	FencingAutomatic FencingPolicy = "Automatic"
)

// CubridDatabase identifies one CUBRID database participating in HA (ADR-0010).
// v1alpha1 supports exactly one database; the list shape is reserved for
// multi-database support.
type CubridDatabase struct {
	// name is the CUBRID database name. Immutable after creation; the operator
	// generates ha_db_list from the database names in order.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	Name string `json:"name"`
}

// CubridTopology declares the HA pool size, not runtime roles (ADR-0001).
type CubridTopology struct {
	// promotableMembers is the total number of promotable CUBRID HA members
	// (listed in ha_node_list, ha_mode=on). Exactly one is master at runtime,
	// chosen by CUBRID heartbeat; the rest are runtime slaves. v1alpha1 allows
	// 1 (standalone) or 3 (HA).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	PromotableMembers int32 `json:"promotableMembers"`

	// readReplicas is the number of non-promotable CUBRID replica-role members
	// (ha_replica_list, ha_mode=replica). Reserved; must be 0 in v1alpha1.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=0
	ReadReplicas int32 `json:"readReplicas"`
}

// CubridHighAvailability configures CUBRID HA (ADR-0001, ADR-0005).
type CubridHighAvailability struct {
	// enabled turns on CUBRID HA configuration. When false, v1alpha1 supports
	// only a single standalone promotable member (ha_mode=off).
	// +kubebuilder:validation:Required
	Enabled bool `json:"enabled"`

	// fencingPolicy controls operator fencing (ADR-0005). Automatic is rejected
	// in v1alpha1.
	// +kubebuilder:default=Manual
	// +optional
	FencingPolicy FencingPolicy `json:"fencingPolicy,omitempty"`
}

// CubridImage selects the operator-compatible CUBRID image (ADR-0003).
type CubridImage struct {
	// repository is the container image repository.
	// +optional
	Repository string `json:"repository,omitempty"`
	// tag is the container image tag. Prefer pinning by digest via repository@sha256.
	// +optional
	Tag string `json:"tag,omitempty"`
}

// CubridStorageSpec configures the per-instance data volume (ADR-0004, storage #17).
type CubridStorageSpec struct {
	// size is the requested data volume size.
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`
	// storageClassName selects the StorageClass for the data volume.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// CubridStorage groups storage settings.
type CubridStorage struct {
	// data is the primary database data volume.
	// +kubebuilder:validation:Required
	Data CubridStorageSpec `json:"data"`
	// retentionPolicy governs PVC retention on cluster deletion (default Retain).
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	RetentionPolicy string `json:"retentionPolicy,omitempty"`
}

// RecoverySource points restore-as-bootstrap at an object-storage backup
// manifest (ADR-0008).
type RecoverySource struct {
	// manifestUri is the object-storage URI of the backup manifest.json (the
	// ADR-0007 artifact identity). Required for recovery bootstrap.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ManifestURI string `json:"manifestUri"`
	// storageSecretRef references object-storage credentials on THIS cluster
	// (never the source cluster's secrets).
	// +optional
	StorageSecretRef *corev1.LocalObjectReference `json:"storageSecretRef,omitempty"`
}

// CubridBootstrap selects how a new cluster is initialized (ADR-0008/0010).
type CubridBootstrap struct {
	// recovery, when set, restores the initial master from a backup manifest
	// instead of running createdb (ADR-0008). Omit for a fresh cluster.
	// +optional
	Recovery *RecoverySource `json:"recovery,omitempty"`
}

// CubridClusterSpec defines the desired state of CubridCluster.
//
// +kubebuilder:validation:XValidation:rule="self.databases.all(d, d.name.size() > 0)",message="each database must have a non-empty name"
// +kubebuilder:validation:XValidation:rule="self.topology.readReplicas == 0",message="readReplicas must be 0 in v1alpha1 (CUBRID replica role not supported)"
// +kubebuilder:validation:XValidation:rule="!self.highAvailability.enabled ? self.topology.promotableMembers == 1 : true",message="when highAvailability.enabled is false, topology.promotableMembers must be 1"
// +kubebuilder:validation:XValidation:rule="self.highAvailability.enabled ? self.topology.promotableMembers == 3 : true",message="when highAvailability.enabled is true, v1alpha1 requires exactly 3 promotable members"
// +kubebuilder:validation:XValidation:rule="self.highAvailability.fencingPolicy != 'Automatic'",message="fencingPolicy Automatic is not supported in v1alpha1"
// +kubebuilder:validation:XValidation:rule="oldSelf.databases.all(o, self.databases.exists(n, n.name == o.name))",message="database names are immutable; existing names must be preserved"
type CubridClusterSpec struct {
	// version is the desired CUBRID engine compatibility version (e.g. "11.4").
	// Effectively immutable for an initialized HA cluster (ADR-0009).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// databases lists the CUBRID databases under HA (ADR-0010). v1alpha1 allows
	// exactly one; the list shape is reserved for multi-database support.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	// +listType=map
	// +listMapKey=name
	Databases []CubridDatabase `json:"databases"`

	// topology declares the HA pool size (ADR-0001).
	// +kubebuilder:validation:Required
	Topology CubridTopology `json:"topology"`

	// highAvailability configures CUBRID HA (ADR-0001, ADR-0005).
	// +kubebuilder:validation:Required
	HighAvailability CubridHighAvailability `json:"highAvailability"`

	// image selects the operator-compatible CUBRID image (ADR-0003).
	// +optional
	Image *CubridImage `json:"image,omitempty"`

	// storage configures the per-instance data volume.
	// +kubebuilder:validation:Required
	Storage CubridStorage `json:"storage"`

	// bootstrap selects cluster initialization (fresh vs recovery; ADR-0008).
	// +optional
	Bootstrap *CubridBootstrap `json:"bootstrap,omitempty"`

	// dbaPasswordSecretRef references the DBA password Secret.
	// +optional
	DBAPasswordSecretRef *corev1.SecretKeySelector `json:"dbaPasswordSecretRef,omitempty"`

	// resources sets container resource requirements for the DB pods.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// InstanceStatus reports the observed state of one CUBRID instance (ADR-0001).
type InstanceStatus struct {
	// name is the instance (StatefulSet pod / short HA hostname) name.
	Name string `json:"name"`
	// ordinal is the StatefulSet ordinal.
	// +optional
	Ordinal int32 `json:"ordinal,omitempty"`
	// role is the CUBRID runtime role; unknown when not authoritatively observed.
	// +optional
	Role CubridRole `json:"role,omitempty"`
	// ready indicates the instance passed its per-instance readiness (ADR-0006 catch-up).
	// +optional
	Ready bool `json:"ready,omitempty"`
	// observedEngineVersion is the CUBRID engine version observed on this member
	// (ADR-0009 update-vs-upgrade guard). Never inferred from the image tag.
	// +optional
	ObservedEngineVersion string `json:"observedEngineVersion,omitempty"`
	// imageID is the resolved container image ID/digest of this member.
	// +optional
	ImageID string `json:"imageID,omitempty"`
}

// DatabaseStatus reports coarse per-database state (ADR-0010).
type DatabaseStatus struct {
	// name is the database name.
	Name string `json:"name"`
	// phase is a coarse database lifecycle phase.
	// +kubebuilder:validation:Enum=Pending;Creating;Created;Failed;Blocked
	// +optional
	Phase string `json:"phase,omitempty"`
	// primaryCreated indicates the DB exists on the master.
	// +optional
	PrimaryCreated bool `json:"primaryCreated,omitempty"`
	// haConfigured indicates the DB is in ha_db_list on all nodes.
	// +optional
	HAConfigured bool `json:"haConfigured,omitempty"`
}

// CubridClusterStatus defines the observed state of CubridCluster.
type CubridClusterStatus struct {
	// observedGeneration is the most recent generation observed by the operator.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// currentPrimary is the currently observed master instance name. Unset when
	// no primary is resolved or the primary is ambiguous (ADR-0001/0005).
	// +optional
	CurrentPrimary string `json:"currentPrimary,omitempty"`

	// instances reports per-instance observed state.
	// +listType=map
	// +listMapKey=name
	// +optional
	Instances []InstanceStatus `json:"instances,omitempty"`

	// databases reports coarse per-database state.
	// +listType=map
	// +listMapKey=name
	// +optional
	Databases []DatabaseStatus `json:"databases,omitempty"`

	// conditions represent the current state of the CubridCluster. Conditions
	// are the source of truth (phase, if any, is informational only). Notable
	// types: Ready, HAReady, PrimaryResolved, RoutingReady, Degraded,
	// FencingRequired, Progressing, Updating (ADR-0005/0006/0009).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// bootstrap reports recovery-bootstrap progress (ADR-0008). Set only while a
	// cluster is being restored from a backup manifest.
	// +optional
	Bootstrap *BootstrapStatus `json:"bootstrap,omitempty"`

	// observedEngineVersion is the CUBRID engine version observed across the
	// cluster (ADR-0009). Recorded once initialized and used as the immutable
	// baseline the desired spec.version is checked against.
	// +optional
	ObservedEngineVersion string `json:"observedEngineVersion,omitempty"`

	// update reports rolling-update progress (ADR-0009).
	// +optional
	Update *UpdateStatus `json:"update,omitempty"`
}

// UpdateStatus reports database-aware rolling-update progress (ADR-0009).
type UpdateStatus struct {
	// desiredRevision is the StatefulSet revision the operator is rolling toward.
	// +optional
	DesiredRevision string `json:"desiredRevision,omitempty"`
	// currentRevision is the revision currently observed.
	// +optional
	CurrentRevision string `json:"currentRevision,omitempty"`
	// engineVersion is the engine version the update targets (must equal the
	// observed baseline; a change is an upgrade, not an update).
	// +optional
	EngineVersion string `json:"engineVersion,omitempty"`
}

// BootstrapPhase is the recovery-bootstrap lifecycle phase (ADR-0008).
type BootstrapPhase string

const (
	// BootstrapPreparing means the restore operation is being started.
	BootstrapPreparing BootstrapPhase = "Preparing"
	// BootstrapRestoring means restoredb is running on the initial master.
	BootstrapRestoring BootstrapPhase = "Restoring"
	// BootstrapValidating means the restored DB is being validated.
	BootstrapValidating BootstrapPhase = "Validating"
	// BootstrapSeedingReplicas means slaves are being seeded (ADR-0006).
	BootstrapSeedingReplicas BootstrapPhase = "SeedingReplicas"
	// BootstrapComplete means recovery bootstrap finished successfully.
	BootstrapComplete BootstrapPhase = "Complete"
	// BootstrapFailed means recovery bootstrap failed terminally.
	BootstrapFailed BootstrapPhase = "Failed"
)

// BootstrapStatus reports recovery-bootstrap progress (ADR-0008). It never
// advertises a half-restored DB as healthy; Ready stays False until the gate
// passes.
type BootstrapStatus struct {
	// mode is the bootstrap mode; "Recovery" for restore-from-backup.
	// +optional
	Mode string `json:"mode,omitempty"`
	// phase is the recovery lifecycle phase.
	// +optional
	Phase BootstrapPhase `json:"phase,omitempty"`
	// manifestUri is the artifact being restored (provenance).
	// +optional
	ManifestURI string `json:"manifestUri,omitempty"`
	// operationID is the Instance Manager restore operation on targetMember.
	// +optional
	OperationID string `json:"operationID,omitempty"`
	// targetMember is the initial master pod being restored into.
	// +optional
	TargetMember string `json:"targetMember,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cubrid;cc
// +kubebuilder:printcolumn:name="Primary",type=string,JSONPath=`.status.currentPrimary`
// +kubebuilder:printcolumn:name="HA",type=string,JSONPath=`.spec.highAvailability.enabled`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CubridCluster is the Schema for the cubridclusters API.
type CubridCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CubridCluster
	// +required
	Spec CubridClusterSpec `json:"spec"`

	// status defines the observed state of CubridCluster
	// +optional
	Status CubridClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CubridClusterList contains a list of CubridCluster.
type CubridClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CubridCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CubridCluster{}, &CubridClusterList{})
		return nil
	})
}
