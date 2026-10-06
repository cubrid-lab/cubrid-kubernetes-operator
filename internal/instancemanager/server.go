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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPort is the Instance Manager API port (ADR-0003).
const DefaultPort = 9090

const (
	errKey    = "error"
	readyKey  = "ready"
	reasonKey = "reason"

	// Request errors shared by the operation endpoints.
	msgUnreadableBody   = "cannot read request body"
	msgInvalidBody      = "invalid request body"
	msgNoIdempotencyKey = "Idempotency-Key header is required"
)

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
	// version caches the engine version reported by cubrid_rel.
	versionMu sync.Mutex
	version   string
	// standaloneDB is the database of a standalone (non-HA) member; empty
	// for an HA member.
	standaloneDB string
	// backupStagingRoot confines the destination a backup request names.
	backupStagingRoot string
	// restoreRoots confine every path a restore request names (#119).
	restoreRoots RestoreRoots
	// ha is what the HA bootstrap needs (#106).
	ha HAConfig
	// replicationDB and databasesDir locate the logs a slave copies (#229).
	replicationDB string
	databasesDir  string
	// stopRequested is set once a shutdown was asked for: from then on the
	// absence of CUBRID's processes is intended.
	stopRequested atomic.Bool
	// logger writes the structured log; nil discards it.
	logger *slog.Logger
	// pollStatus is the last status answered on each polled path.
	pollMu     sync.Mutex
	pollStatus map[string]int
	// timeouts are the per-operation deadlines.
	timeouts Timeouts
}

func NewServer(cli CLI, token string) *Server {
	return &Server{cli: cli, token: token, timeouts: Timeouts{}.withDefaults(), pollStatus: map[string]int{}}
}

// Timeouts bound each long-running operation as a whole. Their commands
// (backupdb, restoredb, server stop) take far longer than the CLI default.
type Timeouts struct {
	// Backup covers backupdb and the upload. Default 2h.
	Backup time.Duration
	// Restore covers the download and restoredb. Default 2h.
	Restore time.Duration
	// Shutdown covers the ordered stop. Default 100s, below the Pod's
	// preStop limit so the hook gets an answer.
	Shutdown time.Duration
	// Bootstrap covers createdb and the HA start. Default 15m.
	Bootstrap time.Duration
}

func (t Timeouts) withDefaults() Timeouts {
	if t.Backup <= 0 {
		t.Backup = 2 * time.Hour
	}
	if t.Restore <= 0 {
		t.Restore = 2 * time.Hour
	}
	if t.Shutdown <= 0 {
		t.Shutdown = 100 * time.Second
	}
	if t.Bootstrap <= 0 {
		t.Bootstrap = 15 * time.Minute
	}
	return t
}

// WithTimeouts sets the operation deadlines; zero values keep the defaults.
// Returns the server for chaining.
func (s *Server) WithTimeouts(t Timeouts) *Server {
	s.timeouts = t.withDefaults()
	return s
}

// WithBackupStagingRoot sets the directory below which a backup may stage its
// output. Returns the server for chaining.
func (s *Server) WithBackupStagingRoot(root string) *Server {
	s.backupStagingRoot = root
	return s
}

// WithReplication tells an HA member its database and where the logs it
// copies from the other members are, so that a slave can report how its
// applier is doing. Without it the role answer carries no replication facts.
func (s *Server) WithReplication(database, databasesDir string) *Server {
	s.replicationDB, s.databasesDir = database, databasesDir
	return s
}

// WithStandaloneDatabase marks this member as a standalone server of database
// (CUBRID_COMPONENTS=SERVER): it has no HA role, so readiness comes from the
// server status. Returns the server for chaining.
func (s *Server) WithStandaloneDatabase(database string) *Server {
	s.standaloneDB = database
	return s
}

// WithOperationStore attaches a durable operation store, enabling the async
// /v1/backup + /v1/operations endpoints (ADR-0003). Returns the server for
// chaining.
func (s *Server) WithOperationStore(store *OperationStore) *Server {
	s.store = store
	if s.logger != nil {
		store.SetLogger(s.logger)
	}
	return s
}

// WithObjectStore attaches the object-storage backend used to upload backups
// and write the manifest completion marker (ADR-0007). Returns the server for
// chaining.
func (s *Server) WithObjectStore(objects ObjectStore) *Server {
	s.objects = objects
	return s
}

// WithRestoreRoots sets the directories a restore may touch; a request naming a
// path outside them is rejected. Returns the server for chaining.
func (s *Server) WithRestoreRoots(roots RestoreRoots) *Server {
	s.restoreRoots = roots
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
	mux.HandleFunc("POST /v1/ha/bootstrap", s.auth(s.haBootstrap))
	mux.HandleFunc("POST /v1/backup", s.auth(s.backup))
	mux.HandleFunc("POST /v1/restore/prepare", s.auth(s.restorePrepare))
	mux.HandleFunc("GET /v1/operations/{id}", s.auth(s.getOperation))
	mux.HandleFunc("POST /v1/shutdown", s.auth(s.shutdown))
	return s.logRequests(mux)
}

// livez reports that the manager process is alive.
func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports DB-instance readiness only (NOT cluster HA health, #14). The
// instance is ready when it holds an authoritative master/slave role.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.standaloneDB != "" {
		running, err := ServerRunning(r.Context(), s.cli, s.standaloneDB)
		switch {
		case err != nil:
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{readyKey: false, reasonKey: err.Error()})
		case !running:
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{readyKey: false, reasonKey: "server " + s.standaloneDB + " is not running"})
		default:
			writeJSON(w, http.StatusOK, map[string]any{readyKey: true, "mode": "standalone"})
		}
		return
	}
	st := HeartbeatStatus(r.Context(), s.cli)
	if st.Role == RoleMaster || st.Role == RoleSlave || st.Role == RoleReplica {
		writeJSON(w, http.StatusOK, map[string]any{readyKey: true, "role": st.Role})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{readyKey: false, reasonKey: st.Reason})
}

// role returns the local node's authoritative CUBRID role.
func (s *Server) role(w http.ResponseWriter, r *http.Request) {
	st := HeartbeatStatus(r.Context(), s.cli)
	st.EngineVersion = s.engineVersion(r.Context())
	st.Replication = replicationOf(r.Context(), s.cli, st, s.replicationDB, s.databasesDir)
	writeJSON(w, http.StatusOK, st)
}

// engineVersion reads the engine's version once and keeps it: the binaries in
// the image do not change while this process runs. A failed read is not kept,
// so a later request tries again.
func (s *Server) engineVersion(ctx context.Context) string {
	s.versionMu.Lock()
	defer s.versionMu.Unlock()
	if s.version != "" {
		return s.version
	}
	out, err := s.cli.Run(ctx, "cubrid_rel")
	if err != nil {
		return ""
	}
	s.version = ParseEngineVersion(out)
	return s.version
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
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgUnreadableBody})
		return
	}
	var req BackupRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgInvalidBody})
		return
	}

	// From here on only the path built from the manager's own root is used.
	destination, err := confineBackupDestination(req.Destination, s.backupStagingRoot)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: err.Error()})
		return
	}
	req.Destination = destination

	if s.store == nil {
		ctx, cancel := context.WithTimeout(r.Context(), s.timeouts.Backup)
		defer cancel()
		if err := s.createBackupStaging(req.Destination); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
			return
		}
		res, err := Backup(ctx, s.cli, req)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgNoIdempotencyKey})
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
		ctx, cancel := context.WithTimeout(context.Background(), s.timeouts.Backup)
		defer cancel()
		fail := func(reason string) {
			s.removeBackupStaging(req.Destination)
			_, _ = s.store.Update(id, func(op *Operation) {
				op.State = OpFailed
				op.FailureReason = reason
			})
		}

		if _, err := s.store.Update(id, func(op *Operation) { op.State = OpRunningBackup }); err != nil {
			return
		}
		if err := s.createBackupStaging(req.Destination); err != nil {
			fail(err.Error())
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
		s.removeBackupStaging(req.Destination)
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
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgUnreadableBody})
		return
	}
	var req RestoreRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgInvalidBody})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgNoIdempotencyKey})
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
		ctx, cancel := context.WithTimeout(context.Background(), s.timeouts.Restore)
		defer cancel()
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
		// What an interrupted restore of this manager left is removed first;
		// anything else in the target is refused by the restore's own guard.
		if err := s.reclaimIncomplete(req.Database); err != nil {
			fail("restore failed: " + err.Error())
			return
		}
		// An HA member registers the restored database under the member list,
		// as createdb does on the first member.
		roots := s.restoreRoots
		roots.Owner = id
		haMember := false
		if s.standaloneDB == "" {
			if hosts, err := haHosts(s.ha.ConfPath); err == nil {
				roots.Host = hosts
				haMember = true
			}
		}
		res, err := Restore(ctx, s.cli, s.objects, roots, req)
		if err != nil {
			fail("restore failed: " + err.Error())
			return
		}
		// In a recovery bootstrap the entrypoint started nothing and runs only
		// once, so a standalone member's server is started here: the operation
		// is not complete until it runs. An HA member is started by the HA
		// bootstrap. The restored data is kept when the start fails.
		if s.standaloneDB != "" && s.standaloneDB == req.Database {
			if _, err := s.store.Update(id, func(op *Operation) { op.State = OpStarting }); err != nil {
				return
			}
			if out, err := s.cli.Run(ctx, "cubrid", "server", "start", req.Database); err != nil {
				fail("server start failed: " + err.Error() + ": " + out)
				return
			}
		}
		// A configured HA member joins HA with the restored database: the
		// entrypoint started nothing for it either. Issued once, and only when
		// heartbeat is not already running (docs/poc/RESULTS.md, POC-3/POC-13).
		if haMember && HeartbeatStatus(ctx, s.cli).Role == RoleUnknown {
			if _, err := s.store.Update(id, func(op *Operation) { op.State = OpStarting }); err != nil {
				return
			}
			if out, err := s.cli.Run(ctx, "cubrid", "heartbeat", "start"); err != nil {
				fail("heartbeat start failed: " + err.Error() + ": " + out)
				return
			}
		}
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
	s.stopRequested.Store(true)
	ctx, cancel := context.WithTimeout(r.Context(), s.timeouts.Shutdown)
	defer cancel()
	if err := Shutdown(ctx, s.cli, r.URL.Query().Get("database")); err != nil {
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
		if got := r.Header.Get("Authorization"); got != "Bearer "+s.token {
			reason := reasonWrongToken
			if got == "" {
				reason = reasonNoToken
			}
			s.logRefused(w, r, reason)
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

// confineBackupDestination maps a backup request's destination onto the
// manager's staging root and returns a path built from that root: it must be
// one plain directory directly below the root. Nothing after this uses the
// path as the request spelled it, and removing it on failure cannot reach
// outside the root.
func confineBackupDestination(destination, root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return "", fmt.Errorf("backup staging root %q is not configured as a clean absolute path", root)
	}
	if destination == "" {
		return "", errors.New("destination is required")
	}
	clean := filepath.Clean(destination)
	name := filepath.Base(clean)
	if filepath.Dir(clean) != root || !stagingNamePattern.MatchString(name) {
		return "", fmt.Errorf("destination %q must be one directory directly below %q", destination, root)
	}
	return filepath.Join(root, name), nil
}

// createBackupStaging creates a backup's staging directory, and the staging
// root when the data volume does not have it yet: backupdb fails when its
// destination does not exist (docs/poc/RESULTS.md, POC-12). The directory is
// created through an os.Root on the staging root, so it cannot land elsewhere.
func (s *Server) createBackupStaging(destination string) error {
	if err := os.MkdirAll(s.backupStagingRoot, 0o750); err != nil {
		return fmt.Errorf("create the backup staging root: %w", err)
	}
	root, err := os.OpenRoot(s.backupStagingRoot)
	if err != nil {
		return fmt.Errorf("open the backup staging root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(filepath.Base(destination), 0o750); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create the backup staging directory: %w", err)
	}
	return nil
}

// removeBackupStaging deletes a backup's staging directory. It removes only a
// directory that it finds by listing the staging root, so the path it deletes
// is built from the root and a name the file system returned, never from the
// request's spelling.
func (s *Server) removeBackupStaging(destination string) {
	entries, err := os.ReadDir(s.backupStagingRoot)
	if err != nil {
		return
	}
	want := filepath.Base(destination)
	for _, entry := range entries {
		if entry.Name() == want {
			_ = os.RemoveAll(filepath.Join(s.backupStagingRoot, entry.Name()))
			return
		}
	}
}
