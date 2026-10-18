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
	networkingv1 "k8s.io/api/networking/v1"
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
	// generates ha_db_list from the database names in order. At most 17
	// characters: CUBRID 11.4 does not create a database with a longer name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=17
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
//
// The image is repository, repository:tag, or a repository pinned by digest
// (repository@sha256:...). A tag without a repository names no image, and a
// tag next to a digest is refused, since it is the digest that is pulled.
//
// +kubebuilder:validation:XValidation:rule="!has(self.tag) || self.tag.size() == 0 || (has(self.repository) && self.repository.size() > 0)",message="image.tag needs image.repository: a tag alone names no image"
// +kubebuilder:validation:XValidation:rule="!has(self.tag) || self.tag.size() == 0 || !has(self.repository) || !self.repository.contains('@')",message="image.repository is pinned by digest and cannot be combined with image.tag"
type CubridImage struct {
	// repository is the container image repository, optionally pinned by
	// digest (repository@sha256:...).
	// +optional
	Repository string `json:"repository,omitempty"`
	// tag is the container image tag. Prefer pinning by digest via repository@sha256.
	// +optional
	Tag string `json:"tag,omitempty"`
}

// CubridStorageSpec configures the per-instance data volume (ADR-0004, storage #17).
//
// The volumes are made when the cluster is created. The operator neither
// resizes nor moves them, so a later change of the size or the class would
// be accepted and never applied; it is refused instead.
//
// +kubebuilder:validation:XValidation:rule="quantity(string(self.size)).isGreaterThan(quantity('0'))",message="storage.data.size must be greater than zero"
// +kubebuilder:validation:XValidation:rule="quantity(string(self.size)).compareTo(quantity(string(oldSelf.size))) == 0",message="storage.data.size cannot be changed after the cluster is created: the operator does not resize volumes"
// +kubebuilder:validation:XValidation:rule="has(self.storageClassName) == has(oldSelf.storageClassName) && (!has(self.storageClassName) || self.storageClassName == oldSelf.storageClassName)",message="storage.data.storageClassName cannot be changed after the cluster is created: the operator does not move volumes"
type CubridStorageSpec struct {
	// size is the requested data volume size. It is fixed when the cluster is
	// created.
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`
	// storageClassName selects the StorageClass for the data volume. It is
	// fixed when the cluster is created.
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

// The accepted form of spec.bootstrap.recovery.manifestUri, and of the
// bucket and prefix a CubridBackup is written under (ADR-0008). The
// XValidation markers on RecoverySource.ManifestURI and
// ObjectStorageDestination carry these expressions verbatim, and the operator
// applies the same ones before it restores or backs up, so the API server and
// the operator accept the same URIs.
const (
	// S3BucketPattern is a bucket name: 3 to 63 lower-case letters, digits,
	// dots and hyphens that starts and ends with a letter or digit. A bucket
	// name has no two adjacent dots either (S3BucketAdjacentDotsPattern).
	S3BucketPattern = `[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]`
	// S3BucketAdjacentDotsPattern must not match a bucket name.
	S3BucketAdjacentDotsPattern = `[.][.]`
	// S3KeySegmentPattern is one segment of a key prefix: not empty, not "."
	// or "..", and without "/", "?", "#", "%" or an ASCII whitespace or
	// control character.
	S3KeySegmentPattern = `([^/?#%[:space:][:cntrl:]]{3,}|[^./?#%[:space:][:cntrl:]][^/?#%[:space:][:cntrl:]]?|[.][^./?#%[:space:][:cntrl:]])`

	// ManifestURIPattern must match a manifestUri: s3://, a bucket, one or
	// more prefix segments, and the file name manifest.json.
	ManifestURIPattern = `^s3://` + S3BucketPattern + `(/` + S3KeySegmentPattern + `)+/manifest[.]json$`
	// ManifestURIBucketAdjacentDotsPattern must not match a manifestUri.
	ManifestURIBucketAdjacentDotsPattern = `^s3://[^/]*` + S3BucketAdjacentDotsPattern
	// ManifestURIMaxLength is the longest manifestUri in characters: s3://,
	// a 63-character bucket, "/" and 1024 characters of key. S3 limits a key
	// to 1024 bytes, which the store enforces for a key with non-ASCII
	// characters.
	ManifestURIMaxLength = 1093

	// S3BucketNamePattern must match a bucket field.
	S3BucketNamePattern = `^` + S3BucketPattern + `$`
	// S3KeyPrefixPattern must match a non-empty CubridBackup destination
	// prefix: segments separated by single slashes, without a leading slash,
	// and with at most one trailing slash, which the key written leaves out.
	S3KeyPrefixPattern = `^` + S3KeySegmentPattern + `(/` + S3KeySegmentPattern + `)*/?$`
	// S3KeyPrefixMaxLength is the longest CubridBackup destination prefix.
	// The operator adds the cluster UID, the database and the backup UID
	// below it, and runs no backup whose artifact URI is longer than
	// ManifestURIMaxLength.
	S3KeyPrefixMaxLength = 1024
)

// RecoverySource points restore-as-bootstrap at an object-storage backup
// manifest (ADR-0008).
type RecoverySource struct {
	// manifestUri is the object-storage URI of the backup manifest.json (the
	// ADR-0007 artifact identity), of the form
	// s3://<bucket>/<prefix>/manifest.json; the object it names is the one
	// read. The bucket has 3 to 63 lower-case letters, digits, dots and
	// hyphens, starts and ends with a letter or digit and has no two adjacent
	// dots. The prefix has one or more segments; a segment is not empty, not
	// "." or "..", and has no "/", "?", "#", "%", or ASCII whitespace or
	// control character, so the URI has no query, fragment or percent
	// escape. The file name is manifest.json. At most 1093 characters. The
	// URI CubridBackup status.artifact.uri reports for a backup has this
	// form. Required for recovery bootstrap.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1093
	// +kubebuilder:validation:XValidation:rule="self.matches('^s3://[a-z0-9][a-z0-9.-]{1,61}[a-z0-9](/([^/?#%[:space:][:cntrl:]]{3,}|[^./?#%[:space:][:cntrl:]][^/?#%[:space:][:cntrl:]]?|[.][^./?#%[:space:][:cntrl:]]))+/manifest[.]json$')",message="manifestUri must be s3://<bucket>/<prefix>/manifest.json: a bucket of 3 to 63 lower-case letters, digits, dots and hyphens, a prefix of one or more segments that are not empty, '.' or '..' and have no '?', '#', '%' or ASCII whitespace or control character, and the file name manifest.json"
	// +kubebuilder:validation:XValidation:rule="!self.matches('^s3://[^/]*[.][.]')",message="the bucket of manifestUri must not contain two adjacent dots"
	ManifestURI string `json:"manifestUri"`
}

// CubridObjectStorage is the S3-compatible object storage the cluster's DB
// Pods use for backup and restore (ADR-0007/0008). It is set per cluster: the
// Instance Manager takes its endpoint and credentials from its own
// environment at Pod start, never from a request, so changing them needs the
// Pods restarted.
type CubridObjectStorage struct {
	// endpoint is the service address as host[:port], without a scheme.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[^/\s]+$`
	Endpoint string `json:"endpoint"`

	// region is the bucket region, when the service needs one.
	// +optional
	Region string `json:"region,omitempty"`

	// insecure makes the DB Pods use plain HTTP instead of TLS. For
	// development object stores only.
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// bucket holds the artifacts the operator creates for itself: the backup
	// the other members of an HA cluster are seeded from. A CubridBackup
	// names its own bucket. Required for an HA cluster: this is what the
	// current way of seeding the members needs (ADR-0010). The seed backups
	// are never read through a manifestUri, so bucket and prefix are not
	// held to its rules (ADR-0008).
	// +optional
	Bucket string `json:"bucket,omitempty"`

	// prefix is the key prefix under bucket for those artifacts.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// credentialsSecretRef names a Secret in the cluster's namespace with the
	// keys accessKey and secretKey. The operator does not read it; the DB Pods
	// receive the two keys as environment variables.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="has(self.name) && self.name.size() > 0",message="credentialsSecretRef.name is required"
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// CubridNetworkPolicy configures the NetworkPolicies the operator keeps for
// the cluster's Pods. They restrict ingress only, and only where the cluster's
// network plugin enforces NetworkPolicy; they do not encrypt traffic or
// authenticate database users.
type CubridNetworkPolicy struct {
	// enabled makes the operator keep the policies: the DB Pods accept the
	// Instance Manager port from the operator only, and the server and HA
	// ports from the cluster's own DB and Broker Pods; the Broker Pods accept
	// their ports from clients. When false, the policies the operator created
	// are removed.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// clients are the peers allowed to connect to the Broker ports. When
	// omitted or empty (clients: []), only Pods in the cluster's namespace
	// are. Clients never reach the DB Pods directly.
	// +optional
	Clients []networkingv1.NetworkPolicyPeer `json:"clients,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="!has(self.bootstrap) || !has(self.bootstrap.recovery) || has(self.objectStorage)",message="bootstrap.recovery requires objectStorage: the DB Pods read the backup from it"
// +kubebuilder:validation:XValidation:rule="!self.highAvailability.enabled || (has(self.objectStorage) && has(self.objectStorage.bucket) && self.objectStorage.bucket.size() > 0)",message="an HA cluster needs objectStorage with a bucket: its first database is copied to the other members through it"
// +kubebuilder:validation:XValidation:rule="self.topology.promotableMembers == oldSelf.topology.promotableMembers",message="topology.promotableMembers cannot be changed after the cluster is created: v1alpha1 has no procedure to add or remove members"
// +kubebuilder:validation:XValidation:rule="self.highAvailability.enabled == oldSelf.highAvailability.enabled",message="highAvailability.enabled cannot be changed after the cluster is created: v1alpha1 has no procedure to switch between a standalone and an HA cluster"
// +kubebuilder:validation:XValidation:rule="(has(self.bootstrap) && has(self.bootstrap.recovery)) == (has(oldSelf.bootstrap) && has(oldSelf.bootstrap.recovery)) && (!(has(self.bootstrap) && has(self.bootstrap.recovery)) || self.bootstrap.recovery.manifestUri == oldSelf.bootstrap.recovery.manifestUri)",message="bootstrap.recovery cannot be added, removed or changed after the cluster is created: it decides whether the first database is restored from a backup or created empty"
// +kubebuilder:validation:XValidation:rule="!has(self.dbaPasswordSecretRef)",message="dbaPasswordSecretRef is not implemented in v1alpha1: the database is created with an empty DBA password, so the field is rejected instead of being ignored"
// +kubebuilder:validation:XValidation:rule="oldSelf.databases.all(o, self.databases.exists(n, n.name == o.name))",message="database names are immutable; existing names must be preserved"
type CubridClusterSpec struct {
	// version is the desired CUBRID engine compatibility version (e.g. "11.4").
	// Effectively immutable for an initialized cluster, HA or standalone (ADR-0009).
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
	// Whether it names a recovery, and which backup, is fixed when the
	// cluster is created.
	// +optional
	Bootstrap *CubridBootstrap `json:"bootstrap,omitempty"`

	// objectStorage configures the object storage the DB Pods use for backup
	// and restore. Required for a recovery bootstrap and for a CubridBackup
	// with an ObjectStorage destination.
	// +optional
	ObjectStorage *CubridObjectStorage `json:"objectStorage,omitempty"`

	// dbaPasswordSecretRef references the DBA password Secret. It is reserved:
	// nothing applies it yet, so a cluster that sets it is rejected.
	// +optional
	DBAPasswordSecretRef *corev1.SecretKeySelector `json:"dbaPasswordSecretRef,omitempty"`

	// resources sets container resource requirements for the DB pods.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// networkPolicy configures the NetworkPolicies of the cluster's Pods.
	// Omitted, they are kept with their defaults.
	// +optional
	NetworkPolicy *CubridNetworkPolicy `json:"networkPolicy,omitempty"`
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
	// replication is what this member's log applier last reported for the
	// master's log. It is set for a slave only, and absent when the applier
	// could not be read.
	// +optional
	Replication *InstanceReplication `json:"replication,omitempty"`
	// lastObservedTime is the time of the observation in which this member
	// last gave the operator a reachable answer to its role query that did not
	// contradict itself or name another member, whatever role it reported: the
	// cluster's lastObservedTime when it answered in that observation. A
	// member that does not answer, or answers so, keeps the time of its last
	// answer. A newer value of this time alone is written at most every
	// 30 seconds, so it can understate the last answer by up to that much
	// (docs/observability.md, "Freshness").
	// +optional
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`
}

// InstanceReplication reports how a slave's log applier is doing (ADR-0006).
// The values are the applier's own counters as last read; the operator
// derives stalledSince from two readings in a row. A reconcile that could not
// read the applier keeps the last reading, for a bounded time, without
// changing it.
type InstanceReplication struct {
	// source is the member whose log is applied: the master this slave sees.
	Source string `json:"source"`
	// appliedChanges is the number of changes the applier has applied since
	// it was started.
	// +optional
	AppliedChanges int64 `json:"appliedChanges,omitempty"`
	// failCount is the number of changes the applier could not apply.
	// +optional
	FailCount int64 `json:"failCount,omitempty"`
	// delayedPages is the number of copied log pages that are not applied yet.
	// +optional
	DelayedPages int64 `json:"delayedPages,omitempty"`
	// applier identifies the applier process these counters were read
	// beside. The value is opaque and is only compared for equality. The
	// counters outlast the process, so a change of it starts a new series: a
	// restarted applier does not inherit the age of stalledSince. Empty when
	// the Instance Manager could not identify the process; an empty value on
	// either side decides nothing.
	// +optional
	Applier string `json:"applier,omitempty"`
	// stalledSince is when the operator first saw log pages waiting while the
	// applier had applied nothing since the reading before. It is cleared
	// as soon as the applier applies something or no page waits, and starts
	// again at the current reading when the applier process changes while
	// pages still wait.
	// +optional
	StalledSince *metav1.Time `json:"stalledSince,omitempty"`
	// observedAt is when the operator last read these counters. A reading
	// with the same values as the stored one advances it at most every 30
	// seconds.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
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
	// seededMembers lists the members the database was copied to during the
	// HA bootstrap. A member in this list is never seeded again.
	// +listType=set
	// +optional
	SeededMembers []string `json:"seededMembers,omitempty"`
	// seeding is the copy to one member that the HA bootstrap is making after
	// the first peer has joined. Its source is recorded before the restore on
	// that member is started, so that a restore in progress keeps its source
	// when the master changes meanwhile.
	// +optional
	Seeding *DatabaseSeeding `json:"seeding,omitempty"`
	// bootstrapAttempts counts the bootstrap steps that failed and were
	// started again. The bootstrap stops as Failed when it reaches the limit.
	// +optional
	BootstrapAttempts int32 `json:"bootstrapAttempts,omitempty"`
}

// DatabaseSeeding is one copy of the database to a member during the HA
// bootstrap.
type DatabaseSeeding struct {
	// member is the member the database is copied to.
	Member string `json:"member"`
	// source is the member it is copied from: the member that was last
	// observed as the master among the members that hold the database.
	Source string `json:"source"`
	// round counts how often the source changed before the restore was
	// started. Every round has a backup of its own.
	// +optional
	Round int32 `json:"round,omitempty"`
	// restoreOperationID is the restore on the member, once it was started.
	// From then on the source is kept.
	// +optional
	RestoreOperationID string `json:"restoreOperationID,omitempty"`
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

	// lastObservedTime is when the operator last asked every member of an HA
	// cluster for its role: the observation that currentPrimary, instances and
	// the PrimaryResolved, HAReady and ReplicationHealthy conditions were
	// derived from. A reconcile that ends before the roles are observed keeps
	// it, and so does a stopped operator, so its age tells how old the HA
	// status is. A newer value of this time alone is written at most every 30
	// seconds: the HA status is stale when this time is more than 60 seconds
	// old (docs/observability.md, "Freshness"). Unset while the roles of the
	// cluster have never been observed.
	// +optional
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`
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
	// BootstrapPreparing means the restore operation is being started, or
	// it verifies and downloads the backup.
	BootstrapPreparing BootstrapPhase = "Preparing"
	// BootstrapRestoring means restoredb is running on the initial master,
	// or the restored database is being started there.
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
	// attempts counts the restores that failed and were started again. The
	// recovery stops as Failed when it reaches the limit.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
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
