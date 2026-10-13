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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CubridBackupSpec defines the desired state of CubridBackup (ADR-0007).
// v1alpha1 backups are Instance Manager-executed, single-shot, full backups.
//
// A CubridBackup is one request: the operator addresses its backup by one
// idempotency key, so what it asks for cannot change once it exists. Another
// backup is another CubridBackup.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="the spec of a CubridBackup cannot be changed after it is created: create another CubridBackup"
type CubridBackupSpec struct {
	// clusterRef selects the CubridCluster to back up (same namespace).
	// +kubebuilder:validation:Required
	ClusterRef LocalObjectRef `json:"clusterRef"`

	// database is the CUBRID database to back up. Explicit even though
	// v1alpha1 supports exactly one database per cluster (ADR-0010). At most
	// 17 characters, like the name in a CubridCluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=17
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	Database string `json:"database"`

	// target selects which HA member runs the backup (ADR-0005/0007).
	// +optional
	// +kubebuilder:default={preference: PreferStandby}
	Target CubridBackupTarget `json:"target,omitempty"`

	// level is the backup level. v1alpha1 supports Full only (CUBRID level 0);
	// incremental chains are deferred.
	// +optional
	// +kubebuilder:default=Full
	// +kubebuilder:validation:Enum=Full
	Level CubridBackupLevel `json:"level,omitempty"`

	// destination is where the artifact is stored (ADR-0007).
	// +kubebuilder:validation:Required
	Destination CubridBackupDestination `json:"destination"`
}

// LocalObjectRef references another object by name in the same namespace.
type LocalObjectRef struct {
	// name is the referenced object's name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// CubridBackupLevel enumerates supported backup levels.
type CubridBackupLevel string

const (
	// CubridBackupFull is a full backup (CUBRID level 0).
	CubridBackupFull CubridBackupLevel = "Full"
)

// TargetPreference selects which HA role runs the backup.
type TargetPreference string

const (
	// PreferStandby prefers a caught-up standby, falling back to master only on
	// fresh, unambiguous HA evidence (ADR-0007).
	PreferStandby TargetPreference = "PreferStandby"
	// StandbyOnly requires a standby and fails if none is eligible.
	StandbyOnly TargetPreference = "StandbyOnly"
	// PrimaryOnly forces the backup onto the resolved master.
	PrimaryOnly TargetPreference = "PrimaryOnly"
)

// CubridBackupTarget controls target member selection.
type CubridBackupTarget struct {
	// preference selects which HA role runs the backup.
	// +optional
	// +kubebuilder:default=PreferStandby
	// +kubebuilder:validation:Enum=PreferStandby;StandbyOnly;PrimaryOnly
	Preference TargetPreference `json:"preference,omitempty"`
}

// DestinationType enumerates backup destinations.
type DestinationType string

const (
	// DestinationObjectStorage is S3-compatible object storage (production).
	DestinationObjectStorage DestinationType = "ObjectStorage"
	// DestinationPVC is reserved: no backup is run for it. The controller
	// ends a backup to it as Failed with reason UnsupportedDestination.
	DestinationPVC DestinationType = "PVC"
)

// CubridBackupDestination is where the backup artifact is stored.
type CubridBackupDestination struct {
	// type selects the destination backend. ObjectStorage is the only one a
	// backup is run for. PVC is reserved: a backup to it is not run and ends
	// as Failed with reason UnsupportedDestination.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=ObjectStorage;PVC
	Type DestinationType `json:"type"`

	// objectStorage configures the S3-compatible destination. Required when
	// type is ObjectStorage.
	// +optional
	ObjectStorage *ObjectStorageDestination `json:"objectStorage,omitempty"`
}

// ObjectStorageDestination configures an S3-compatible artifact store.
type ObjectStorageDestination struct {
	// provider identifies the object-storage provider.
	// +optional
	// +kubebuilder:default=S3Compatible
	// +kubebuilder:validation:Enum=S3Compatible
	Provider string `json:"provider,omitempty"`

	// bucket is the target bucket. It follows the bucket rules of a
	// CubridCluster manifestUri (ADR-0008), so the artifact URI the backup
	// reports is one a recovery accepts.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == '' || (self.matches('^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$') && !self.matches('[.][.]'))",message="bucket must be an S3 bucket name: 3 to 63 lower-case letters, digits, dots and hyphens, starting and ending with a letter or digit, without two adjacent dots"
	Bucket string `json:"bucket"`

	// prefix is the key prefix under the bucket. It follows the prefix rules
	// of a CubridCluster manifestUri, without a leading or trailing slash
	// (ADR-0008).
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="self == '' || self.matches('^([^/?#%[:space:][:cntrl:]]{3,}|[^./?#%[:space:][:cntrl:]][^/?#%[:space:][:cntrl:]]?|[.][^./?#%[:space:][:cntrl:]])(/([^/?#%[:space:][:cntrl:]]{3,}|[^./?#%[:space:][:cntrl:]][^/?#%[:space:][:cntrl:]]?|[.][^./?#%[:space:][:cntrl:]]))*$')",message="prefix must be segments separated by single slashes, without a leading or trailing slash, and no segment may be empty, '.' or '..' or have '?', '#', '%' or an ASCII whitespace or control character: a recovery accepts no other manifestUri"
	Prefix string `json:"prefix,omitempty"`
}

// CubridBackupPhase is a high-level lifecycle phase (ADR-0007).
type CubridBackupPhase string

const (
	// BackupPhasePending means the backup has been accepted but not started.
	BackupPhasePending CubridBackupPhase = "Pending"
	// BackupPhaseRunning means backupdb is executing.
	BackupPhaseRunning CubridBackupPhase = "Running"
	// BackupPhaseUploading means the artifact is being uploaded.
	BackupPhaseUploading CubridBackupPhase = "Uploading"
	// BackupPhaseCompleted means upload + manifest validation succeeded.
	BackupPhaseCompleted CubridBackupPhase = "Completed"
	// BackupPhaseFailed means the backup failed terminally.
	BackupPhaseFailed CubridBackupPhase = "Failed"
)

// CubridBackupArtifact describes a completed backup artifact.
type CubridBackupArtifact struct {
	// uri points at the manifest.json (the atomic completion marker).
	// +optional
	URI string `json:"uri,omitempty"`
	// manifestDigest is the manifest checksum.
	// +optional
	ManifestDigest string `json:"manifestDigest,omitempty"`
	// sizeBytes is the total artifact size.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// cubridVersion is the engine version that produced the backup.
	// +optional
	CubridVersion string `json:"cubridVersion,omitempty"`
	// database is the backed-up database name.
	// +optional
	Database string `json:"database,omitempty"`
	// level is the backup level.
	// +optional
	Level CubridBackupLevel `json:"level,omitempty"`
}

// CubridBackupStatus defines the observed state of CubridBackup (ADR-0007).
type CubridBackupStatus struct {
	// observedGeneration is the spec generation reflected by this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase is the high-level backup lifecycle phase.
	// +optional
	Phase CubridBackupPhase `json:"phase,omitempty"`

	// operationRef is the Instance Manager operation ID (idempotent contract).
	// +optional
	OperationRef string `json:"operationRef,omitempty"`

	// targetInstance is the pod selected to run the backup.
	// +optional
	TargetInstance string `json:"targetInstance,omitempty"`

	// targetRole is the ADR-0005 role of the target at backup start.
	// +optional
	TargetRole string `json:"targetRole,omitempty"`

	// fallbackUsed is true when a standby-preferred backup fell back to master.
	// +optional
	FallbackUsed bool `json:"fallbackUsed,omitempty"`

	// clusterUID is the UID of the CubridCluster the backup was started on. A
	// cluster of the same name with another UID is another cluster, and the
	// backup does not go on with it.
	// +optional
	ClusterUID types.UID `json:"clusterUID,omitempty"`

	// targetRecordedAt is when targetInstance was recorded, before anything
	// was started on it. The wait for an operation on the recorded target is
	// bounded from this time (ADR-0007).
	// +optional
	TargetRecordedAt *metav1.Time `json:"targetRecordedAt,omitempty"`

	// objectStorageNotAppliedSince is when the operator first found the
	// selected Pod running without the cluster's object-storage settings,
	// while no target was recorded. That wait is bounded from this time
	// (ADR-0007).
	// +optional
	ObjectStorageNotAppliedSince *metav1.Time `json:"objectStorageNotAppliedSince,omitempty"`

	// startedAt is when the backup started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// completedAt is when the backup reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// artifact describes the completed backup (set on success).
	// +optional
	Artifact *CubridBackupArtifact `json:"artifact,omitempty"`

	// conditions represent the current state of the CubridBackup resource
	// (Accepted/Ready with open-ended reasons, ADR-0007).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".status.targetInstance"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// CubridBackup is the Schema for the cubridbackups API
type CubridBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CubridBackup
	// +required
	Spec CubridBackupSpec `json:"spec"`

	// status defines the observed state of CubridBackup
	// +optional
	Status CubridBackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CubridBackupList contains a list of CubridBackup
type CubridBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CubridBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CubridBackup{}, &CubridBackupList{})
		return nil
	})
}
