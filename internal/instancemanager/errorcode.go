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

import (
	"errors"
	"net/http"
)

// ErrorResponse is the body of an error answer (ADR-0003). Error is the
// message every manager sends. Code names the cause for a caller to act on;
// it is additive, a manager that predates it sends Error alone, and a caller
// that does not know a code acts on the HTTP status.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// Error codes (ADR-0003). The class a caller derives from each one is
// documented there: retryable, terminal for the request or the key, or
// unauthorized.
const (
	// CodeUnauthorized answers 401: the bearer token is missing or not the
	// manager's.
	CodeUnauthorized = "Unauthorized"
	// CodeOperationInProgress answers 409: another operation that has not
	// ended holds the database. Retryable once it ends.
	CodeOperationInProgress = "OperationInProgress"
	// CodeIdempotencyConflict answers 409: the manager holds an operation of
	// this kind under the Idempotency-Key for another request body. Sending
	// the request again never succeeds; the operation the key names is read
	// with GET /v1/operations?idempotencyKey=.
	CodeIdempotencyConflict = "IdempotencyConflict"
	// CodeStagingDestinationInUse answers 409: a running operation of another
	// request owns the backup's staging destination. Retryable once it ends.
	CodeStagingDestinationInUse = "StagingDestinationInUse"
	// CodeNotAnHAMember answers 409: an HA request reached a standalone
	// member. Terminal for this member.
	CodeNotAnHAMember = "NotAnHAMember"
	// CodeObjectStorageNotApplied answers 412: the manager was started
	// without object storage. Terminal until the Pod is restarted with it.
	CodeObjectStorageNotApplied = "ObjectStorageNotApplied"
	// CodeOperationRecordUnreadable answers 500: the manager holds an
	// operation record it cannot read, which may be the operation the key
	// names, so it starts none. Retryable once the record is readable or
	// removed.
	CodeOperationRecordUnreadable = "OperationRecordUnreadable"
)

// writeError answers with status and an ErrorResponse; an empty code is left
// out.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg, Code: code})
}

// writeErr answers err with status and the code of its cause.
func writeErr(w http.ResponseWriter, status int, err error) {
	writeError(w, status, errorCode(status, err), err.Error())
}

// errorCode is the code of a refusal with status for err, or "" for one that
// has none.
func errorCode(status int, err error) string {
	switch {
	case errors.Is(err, ErrIdempotencyConflict):
		return CodeIdempotencyConflict
	case errors.Is(err, ErrOperationInProgress):
		return CodeOperationInProgress
	case errors.Is(err, ErrBackupStagingInUse):
		return CodeStagingDestinationInUse
	case errors.Is(err, ErrOperationRecordUnreadable):
		return CodeOperationRecordUnreadable
	case status == http.StatusPreconditionFailed:
		return CodeObjectStorageNotApplied
	}
	return ""
}
