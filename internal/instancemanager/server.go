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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"
)

// DefaultPort is the Instance Manager API port (ADR-0003).
const DefaultPort = 9090

const errKey = "error"

// Server exposes the Instance Manager HTTP/JSON API (ADR-0003). Kubelet probes
// (/livez, /readyz) are unauthenticated; /v1 endpoints require the bearer token.
type Server struct {
	cli   CLI
	token string
	// store is the durable operation store; nil disables the async operation
	// endpoints (so unit tests can construct a store-less server).
	store *OperationStore
	// objects uploads backups to S3-compatible storage; nil means a backup
	// cannot complete (bare backupdb is never a false Completed, ADR-0007).
	objects ObjectStore
}

func NewServer(cli CLI, token string) *Server {
	return &Server{cli: cli, token: token}
}

// WithOperationStore attaches a durable operation store, enabling the async
// /v1/backup + /v1/operations endpoints (ADR-0003). Returns the server for
// chaining.
func (s *Server) WithOperationStore(store *OperationStore) *Server {
	s.store = store
	return s
}

// WithObjectStore attaches the object-storage backend used to upload backups
// and write the manifest completion marker (ADR-0007). Returns the server for
// chaining.
func (s *Server) WithObjectStore(objects ObjectStore) *Server {
	s.objects = objects
	return s
}

// Handler builds the API mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.livez)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /v1/role", s.auth(s.role))
	mux.HandleFunc("GET /v1/ha/status", s.auth(s.haStatus))
	mux.HandleFunc("GET /v1/ha/convergence", s.auth(s.convergence))
	mux.HandleFunc("POST /v1/backup", s.auth(s.backup))
	mux.HandleFunc("POST /v1/restore/prepare", s.auth(s.restorePrepare))
	mux.HandleFunc("GET /v1/operations/{id}", s.auth(s.getOperation))
	mux.HandleFunc("POST /v1/shutdown", s.auth(s.shutdown))
	return mux
}

// livez reports that the manager process is alive.
func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports DB-instance readiness only (NOT cluster HA health, #14). The
// instance is ready when it holds an authoritative master/slave role.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	st := HeartbeatStatus(r.Context(), s.cli)
	if st.Role == RoleMaster || st.Role == RoleSlave || st.Role == RoleReplica {
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "role": st.Role})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": st.Reason})
}

// role returns the local node's authoritative CUBRID role.
func (s *Server) role(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HeartbeatStatus(r.Context(), s.cli))
}

// haStatus returns the full parsed HA view (role + topology).
func (s *Server) haStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HeartbeatStatus(r.Context(), s.cli))
}

// backup runs a local `cubrid backupdb` (ADR-0007). With an operation store
// attached it is an async, idempotent operation: the request must carry an
// Idempotency-Key; a repeat with the same key returns the existing operation,
// a repeat with a different body is a 409, and a concurrent op on the same
// database is a 409. Without a store it stays the original synchronous path.
func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "cannot read request body"})
		return
	}
	var req BackupRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "invalid request body"})
		return
	}

	if s.store == nil {
		res, err := Backup(r.Context(), s.cli, req)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "Idempotency-Key header is required"})
		return
	}

	op, existed, err := s.store.FindOrCreate(OpBackup, key, HashRequest(body), req.Database)
	switch {
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSON(w, http.StatusConflict, map[string]string{errKey: err.Error()})
		return
	case errors.Is(err, ErrOperationInProgress):
		writeJSON(w, http.StatusConflict, map[string]string{errKey: err.Error()})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
		return
	}
	if existed {
		writeJSON(w, http.StatusAccepted, op)
		return
	}

	s.runBackup(op.ID, req)
	writeJSON(w, http.StatusAccepted, op)
}

// runBackup executes the backup in the background and records the durable state
// transitions. A successful `backupdb` is NOT Completed on its own: the staged
// output must upload and manifest.json (the atomic completion marker) must be
// written before the operation reaches Completed. On any failure it terminates
// Failed and removes the staging directory (ADR-0003/0007).
func (s *Server) runBackup(id string, req BackupRequest) {
	go func() {
		ctx := context.Background()
		fail := func(reason string) {
			_, _ = s.store.Update(id, func(op *Operation) {
				op.State = OpFailed
				op.FailureReason = reason
			})
		}

		if _, err := s.store.Update(id, func(op *Operation) { op.State = OpRunningBackup }); err != nil {
			return
		}
		if _, err := Backup(ctx, s.cli, req); err != nil {
			fail("backupdb failed: " + err.Error())
			return
		}
		if s.objects == nil || req.Upload == nil {
			fail("backupdb succeeded but no object-storage destination is configured")
			return
		}

		if _, err := s.store.Update(id, func(op *Operation) { op.State = OpUploading }); err != nil {
			return
		}
		res, err := UploadBackup(ctx, s.objects, UploadSpec{
			StagingDir: req.Destination,
			Bucket:     req.Upload.Bucket,
			Prefix:     req.Upload.Prefix,
			Manifest: BackupManifest{
				Database:       req.Database,
				ClusterUID:     req.Upload.ClusterUID,
				CubridVersion:  req.Upload.CubridVersion,
				Level:          req.Level,
				SourceInstance: req.Upload.SourceInstance,
				SourceRole:     req.Upload.SourceRole,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
			},
		})
		if err != nil {
			fail("upload failed: " + err.Error())
			return
		}
		// Upload + manifest succeeded: remove staging, then mark Completed.
		_ = os.RemoveAll(req.Destination)
		_, _ = s.store.Update(id, func(op *Operation) {
			op.State = OpCompleted
			op.Artifact = &OperationArtifact{
				ManifestURI:    res.ManifestURI,
				ManifestDigest: res.ManifestDigest,
				SizeBytes:      res.SizeBytes,
				Database:       req.Database,
				Level:          req.Level,
			}
		})
	}()
}

// restorePrepare verifies + downloads + restoredb's a backup artifact into an
// empty target as an async, idempotent operation (ADR-0008). It requires an
// operation store and an object store; the request must carry an Idempotency-Key.
func (s *Server) restorePrepare(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.objects == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{errKey: "restore is not enabled (no operation/object store)"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "cannot read request body"})
		return
	}
	var req RestoreRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "invalid request body"})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "Idempotency-Key header is required"})
		return
	}

	op, existed, err := s.store.FindOrCreate(OpRestore, key, HashRequest(body), req.Database)
	switch {
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSON(w, http.StatusConflict, map[string]string{errKey: err.Error()})
		return
	case errors.Is(err, ErrOperationInProgress):
		writeJSON(w, http.StatusConflict, map[string]string{errKey: err.Error()})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
		return
	}
	if existed {
		writeJSON(w, http.StatusAccepted, op)
		return
	}

	s.runRestore(op.ID, req)
	writeJSON(w, http.StatusAccepted, op)
}

// runRestore executes the restore in the background and records the durable
// state transitions. It reaches Completed only after restoredb succeeds against
// a verified artifact; any failure (trust, download, wrong-target, restoredb)
// terminates Failed with an explicit reason (ADR-0003/0008).
func (s *Server) runRestore(id string, req RestoreRequest) {
	go func() {
		ctx := context.Background()
		fail := func(reason string) {
			_, _ = s.store.Update(id, func(op *Operation) {
				op.State = OpFailed
				op.FailureReason = reason
			})
		}

		if _, err := s.store.Update(id, func(op *Operation) { op.State = OpDownloading }); err != nil {
			return
		}
		if _, err := s.store.Update(id, func(op *Operation) { op.State = OpRestoring }); err != nil {
			return
		}
		res, err := Restore(ctx, s.cli, s.objects, req)
		if err != nil {
			fail("restore failed: " + err.Error())
			return
		}
		_ = os.RemoveAll(req.StagingDir)
		_, _ = s.store.Update(id, func(op *Operation) {
			op.State = OpCompleted
			op.Artifact = &OperationArtifact{
				ManifestURI:   res.ManifestURI,
				Database:      res.Database,
				CubridVersion: res.CubridVersion,
			}
		})
	}()
}

// getOperation returns a durable operation by ID (ADR-0003 poll endpoint).
func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{errKey: "operations are not enabled"})
		return
	}
	op, err := s.store.Get(r.PathValue("id"))
	if errors.Is(err, ErrOperationNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{errKey: "operation not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, op)
}

// convergence reports the local replication apply-pipeline facts (POC-7/9:
// HA registration does not imply caught up). database + copiedLogPath identify
// the master's copy-log directory to inspect.
func (s *Server) convergence(w http.ResponseWriter, r *http.Request) {
	db := r.URL.Query().Get("database")
	path := r.URL.Query().Get("copiedLogPath")
	if db == "" || path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "database and copiedLogPath are required"})
		return
	}
	writeJSON(w, http.StatusOK, ApplyConvergenceStatus(r.Context(), s.cli, db, path))
}

// shutdown performs the ADR-0003 ordered graceful shutdown (withdraw HA, stop server).
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	if err := Shutdown(r.Context(), s.cli, r.URL.Query().Get("database")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutdown"})
}

// auth wraps /v1 handlers with bearer-token authentication (loopback is exempt
// so preStop can call locally without a token; ADR-0003).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" || isLoopback(r.RemoteAddr) {
			next(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{errKey: "unauthorized"})
			return
		}
		next(w, r)
	}
}

func isLoopback(remoteAddr string) bool {
	host := remoteAddr
	if i := indexByte(remoteAddr, ':'); i >= 0 {
		host = remoteAddr[:i]
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func indexByte(s string, b byte) int {
	// last colon splits host:port; use the last so IPv6 hosts without a port
	// are not mis-split. RemoteAddr always has host:port from net/http.
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
