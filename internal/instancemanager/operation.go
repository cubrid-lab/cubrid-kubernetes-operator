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

package instancemanager

import "time"

// OperationKind identifies the mutating operation an Operation tracks.
type OperationKind string

const (
	// OpBackup is a `cubrid backupdb` + upload operation (ADR-0007).
	OpBackup OperationKind = "backup"
)

// OperationState is the durable lifecycle state of an Operation (ADR-0003).
// Successful `backupdb` alone is NOT Completed; Completed requires the terminal
// artifact facts (upload + manifest) to exist.
type OperationState string

const (
	OpPending       OperationState = "Pending"
	OpRunningBackup OperationState = "RunningBackup"
	OpUploading     OperationState = "Uploading"
	OpCompleted     OperationState = "Completed"
	OpFailed        OperationState = "Failed"
	OpCleaningUp    OperationState = "CleaningUp"
)

// IsTerminal reports whether the state is a final resting state.
func (s OperationState) IsTerminal() bool {
	return s == OpCompleted || s == OpFailed
}

// Operation is a durable, idempotent record of one mutating Instance Manager
// operation (ADR-0003). It is persisted to the PVC so that an operator or
// manager restart can re-poll by ID / idempotency key and reconstruct state
// from durable facts rather than memory.
type Operation struct {
	// ID is the server-generated operation identifier (never caller-supplied,
	// so it can be used as a filesystem name without traversal risk).
	ID string `json:"id"`
	// Kind is the operation type.
	Kind OperationKind `json:"kind"`
	// IdempotencyKey is the caller-supplied key; a repeat with the same key and
	// the same RequestHash returns this record instead of starting a new run.
	IdempotencyKey string `json:"idempotencyKey"`
	// RequestHash is a stable hash of the request body; a same-key/different-hash
	// repeat is a conflict, not a new attempt.
	RequestHash string `json:"requestHash"`
	// Database is the target CUBRID database (for the single-active-op guard).
	Database string `json:"database"`
	// State is the durable lifecycle state.
	State OperationState `json:"state"`
	// FailureReason is set when State is Failed.
	FailureReason string `json:"failureReason,omitempty"`
	// Artifact holds the completed backup facts (set only on Completed).
	Artifact *OperationArtifact `json:"artifact,omitempty"`
	// CreatedAt / UpdatedAt are RFC3339 timestamps.
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// OperationArtifact records the durable facts that make an operation trustworthy
// as Completed (ADR-0007). ManifestURI is the object-storage manifest.json (the
// atomic completion marker), written last; upload itself is a later PR.
type OperationArtifact struct {
	ManifestURI    string `json:"manifestURI,omitempty"`
	ManifestDigest string `json:"manifestDigest,omitempty"`
	SizeBytes      int64  `json:"sizeBytes,omitempty"`
	Database       string `json:"database,omitempty"`
	Level          int    `json:"level,omitempty"`
}
