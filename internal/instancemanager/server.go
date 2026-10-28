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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPort is the Instance Manager API port (ADR-0003).
const DefaultPort = 9090

// maxRequestBody is the largest request body the manager reads. The
// operator's requests are a few hundred bytes of JSON.
const maxRequestBody = 16 << 10

const (
	authHeader = "Authorization"
	errKey     = "error"
	readyKey   = "ready"
	reasonKey  = "reason"

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
	// readyMu guards readyCheck, the readiness check that runs now; nil when
	// none does. Concurrent /readyz requests share it, so unauthenticated
	// callers run at most one status command at a time.
	readyMu    sync.Mutex
	readyCheck *readinessCheck
	// afterReadinessCheck, when set, runs after a readiness check's command
	// answered and before the answer is taken; tests use it to make the last
	// caller leave in between.
	afterReadinessCheck func()
	// readinessPanicLogged is set once a panic in a readiness check was
	// logged with its stack; later ones are known by their reason.
	readinessPanicLogged atomic.Bool
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
	// stagingMu serializes the claim, release and reclaim of backup staging.
	stagingMu sync.Mutex
	// stagingWriterBound and stagingWriterPoll override how long and how
	// often a failed backup waits for its staging files to be closed; zero
	// keeps the defaults (awaitStagingWriters).
	stagingWriterBound time.Duration
	stagingWriterPoll  time.Duration
	// procRoot is the process list that wait reads; empty is defaultProcRoot.
	procRoot string
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
	// stopMu and stopped make the stop run once.
	stopMu  sync.Mutex
	stopped bool
	// stopFile is where "instance-manager shutdown" records a stop it was
	// asked for (RecordStopRequest); empty when the manager does not look.
	stopFile string
	// lifeMu guards stopping, starting and the admission into ops. Once
	// stopping is set no operation is admitted and none starts CUBRID. While
	// starting is set only the resume of completed restores is admitted.
	lifeMu   sync.Mutex
	stopping bool
	starting bool
	// ops counts the admitted mutating operations until they have ended.
	ops sync.WaitGroup
	// disruptive counts those of them that stop and start CUBRID themselves
	// (restore, HA bootstrap): while one runs, the absence of CUBRID's
	// processes is intended.
	disruptive atomic.Int32
	// opsCtx is the parent of every operation's context; endOps cancels it
	// when the member stops.
	opsCtx context.Context
	endOps context.CancelFunc
	// logger writes the structured log; nil discards it.
	logger *slog.Logger
	// pollStatus is the last status answered on each polled path.
	pollMu     sync.Mutex
	pollStatus map[string]int
	// notReady is why /readyz last answered that the member is not ready;
	// empty when it answered that it is. Guarded by pollMu.
	notReady string
	// applierMissing is the copied log for which the last role answer found
	// no applier process; empty when it found one. Guarded by pollMu.
	applierMissing string
	// timeouts are the per-operation deadlines.
	timeouts Timeouts
	// heartbeatProcesses lists the processes that run now, and
	// heartbeatSettle is the time between the two observations a heartbeat
	// start needs (observeStartable).
	heartbeatProcesses func() (map[string]bool, error)
	heartbeatSettle    time.Duration
}

func NewServer(cli CLI, token string) *Server {
	opsCtx, endOps := context.WithCancel(context.Background())
	return &Server{
		cli: cli, token: token, timeouts: Timeouts{}.withDefaults(), pollStatus: map[string]int{},
		opsCtx: opsCtx, endOps: endOps,
		heartbeatProcesses: RunningProcesses, heartbeatSettle: heartbeatSettle,
	}
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

// ShutdownBudget is how long the stop of a member may take, with the
// defaults applied. The Pod's termination grace period has to be longer.
func ShutdownBudget(t Timeouts) time.Duration { return t.withDefaults().Shutdown }

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

// HTTPServer builds the HTTP transport for the Instance Manager API.
func (s *Server) HTTPServer(addr string) *http.Server {
	const readTimeout = 10 * time.Second
	const responseAllowance = readTimeout + 5*time.Second
	const maxTimeout = time.Duration(1<<63 - 1)
	// A store-less backup and an ordered shutdown answer synchronously.
	// Their configured budgets must fit after the body is read, with time
	// left to send the response. Extreme valid durations saturate rather
	// than overflow into a negative (and therefore disabled) timeout.
	writeTimeout := max(s.timeouts.Backup, s.timeouts.Shutdown, 10*time.Second)
	if writeTimeout > maxTimeout-responseAllowance {
		writeTimeout = maxTimeout
	} else {
		writeTimeout += responseAllowance
	}
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       time.Minute,
	}
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
	mux.HandleFunc("GET /v1/operations", s.auth(s.findOperations))
	mux.HandleFunc("GET /v1/operations/{id}", s.auth(s.getOperation))
	mux.HandleFunc("POST /v1/shutdown", s.auth(s.shutdown))
	return s.logRequests(mux)
}

// livez reports that the manager process is alive.
func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports DB-instance readiness only (NOT cluster HA health, #14). The
// instance is ready when it holds an authoritative master/slave role, or,
// standalone, when its server runs.
//
// The kubelet asks without a token, and so can anyone who reaches the port.
// The answer therefore says only whether the member is ready: the role is
// what /v1/role gives to a caller with the token, and why a member is not
// ready goes to the manager's log (#333).
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	reason, answered := s.sharedReadiness(r.Context())
	if !answered || reason != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]bool{readyKey: false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{readyKey: true})
}

// readinessCheck is one run of notReadyReason, shared by every /readyz
// request that arrives while it runs.
type readinessCheck struct {
	done   chan struct{}
	cancel context.CancelFunc
	// reason is the check's result, set before done is closed.
	reason string
	// waiters counts the requests that wait for the check; abandoned is set
	// when the last of them left before it ended, and the check cancelled.
	// Both are guarded by Server.readyMu.
	waiters   int
	abandoned bool
}

// sharedReadiness joins the readiness check that runs, or starts one, and
// returns its not-ready reason. answered is false when ctx ended first. The
// check does not end with the request that started it: it is cancelled only
// once no request waits for it, and a request that finds an abandoned check
// waits for its command to end before it starts the next one. No result is
// kept past its check.
func (s *Server) sharedReadiness(ctx context.Context) (reason string, answered bool) {
	for {
		s.readyMu.Lock()
		c := s.readyCheck
		if c != nil && c.abandoned {
			s.readyMu.Unlock()
			select {
			case <-c.done:
				continue
			case <-ctx.Done():
				return "", false
			}
		}
		if c == nil {
			c = s.startReadinessCheck(ctx)
		}
		c.waiters++
		s.readyMu.Unlock()

		select {
		case <-c.done:
			return c.reason, true
		case <-ctx.Done():
			s.readyMu.Lock()
			c.waiters--
			if c.waiters == 0 && s.readyCheck == c {
				c.abandoned = true
				c.cancel()
			}
			s.readyMu.Unlock()
			return "", false
		}
	}
}

// startReadinessCheck runs a check apart from the request that asked for it.
// It is called with readyMu held.
func (s *Server) startReadinessCheck(ctx context.Context) *readinessCheck {
	checkCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &readinessCheck{done: make(chan struct{}), cancel: cancel}
	s.readyCheck = c
	go func() {
		defer cancel()
		reason := s.checkReadiness(checkCtx)
		// Whether the command was cancelled is taken before the answer: a
		// caller that leaves after the command answered cancels nothing that
		// ran, although it marks the check abandoned.
		cancelled := checkCtx.Err() != nil
		if s.afterReadinessCheck != nil {
			s.afterReadinessCheck()
		}
		s.readyMu.Lock()
		abandoned := c.abandoned
		c.reason = reason
		if s.readyCheck == c {
			s.readyCheck = nil
		}
		s.readyMu.Unlock()
		// A cancelled check says nothing about the member but that no request
		// waited for it, as when its command outlasts the kubelet's timeout.
		// A check that ended before its cancellation keeps its own answer.
		if abandoned && cancelled {
			reason = readinessAbandoned
		}
		s.logReadiness(checkCtx, reason)
		close(c.done)
	}()
	return c
}

// readinessAbandoned is the logged reason of a check that every request left
// before it ended.
const readinessAbandoned = "no request waited for the readiness check to end; it was cancelled"

// checkReadiness is notReadyReason run apart from any request: a panic in it
// answers not ready instead of ending the manager.
func (s *Server) checkReadiness(ctx context.Context) (reason string) {
	defer func() {
		if r := recover(); r != nil {
			reason = fmt.Sprintf("the readiness check failed: %v", r)
			if !s.readinessPanicLogged.Swap(true) {
				s.log().Error("Recovered a panic in the readiness check", "panic", fmt.Sprint(r),
					"stack", string(debug.Stack()))
			}
		}
	}()
	return s.notReadyReason(ctx)
}

// reasonOperationStoreWriteFailed starts the not-ready reason of a member
// whose operation store holds an end it could not write (#473).
const reasonOperationStoreWriteFailed = "OperationStoreWriteFailed"

// notReadyReason says why the member is not ready; it is empty for a member
// that is. A member whose operation store keeps an operation's end that it
// could not write is not ready, whatever its engine says: its starts and
// lookups answer 500 until the end is written (#299), and readiness is what
// makes that visible on the Pod. The end is tried again on every check and is
// dropped once it is written or its record no longer exists
// (ErrOperationNotFound). An end whose record exists but cannot be read or
// written stays, so the member stays not ready until it restarts.
func (s *Server) notReadyReason(ctx context.Context) string {
	if s.store != nil {
		if ids := s.store.UnwrittenEnds(); len(ids) > 0 {
			noun := "operation"
			if len(ids) > 1 {
				noun = "operations"
			}
			return fmt.Sprintf("%s: the end of %s %s could not be written to the operation store",
				reasonOperationStoreWriteFailed, noun, strings.Join(ids, ", "))
		}
	}
	if s.standaloneDB != "" {
		running, err := ServerRunning(ctx, s.cli, s.standaloneDB)
		switch {
		case err != nil:
			return err.Error()
		case !running:
			return "server " + s.standaloneDB + " is not running"
		}
		return ""
	}
	st := HeartbeatStatus(ctx, s.cli)
	if st.Role == RoleMaster || st.Role == RoleSlave || st.Role == RoleReplica {
		return ""
	}
	if st.Reason == "" {
		return "the member has no HA role"
	}
	return st.Reason
}

// logReadiness writes why the member is not ready. The kubelet asks every few
// seconds, so the same reason is logged below info after the first time.
func (s *Server) logReadiness(ctx context.Context, reason string) {
	s.pollMu.Lock()
	changed := reason != s.notReady
	s.notReady = reason
	s.pollMu.Unlock()
	if reason == "" {
		return
	}
	level := slog.LevelDebug
	if changed {
		level = slog.LevelInfo
	}
	s.log().Log(ctx, level, "Reported the member not ready", "event", eventNotReady, reasonKey, reason)
}

const (
	// roleBudget bounds a role answer as a whole. The operator gives up on
	// it after five seconds (NewHTTPRoleProber), and a request it canceled
	// carries no deadline here, so the answer sets its own.
	roleBudget = 4 * time.Second
	// replicationBudget bounds applyinfo within the role answer, so a slow
	// applier leaves the replication unknown instead of losing the role.
	replicationBudget = 2 * time.Second
)

// logMissingApplier logs once that heartbeat lists no applier process for
// the master's log a slave's replication was read for: its reading then
// carries no applier identity. It logs again only after a reading in between
// that found an applier, was not of a slave, or was of another master's log.
func (s *Server) logMissingApplier(ctx context.Context, st HAStatus) {
	missing := ""
	if r := st.Replication; r != nil && r.Source != "" {
		logPath := filepath.Join(s.databasesDir, s.replicationDB+"_"+r.Source)
		if _, ok := applierOf(st, logPath); !ok {
			missing = logPath
		}
	}
	s.pollMu.Lock()
	changed := missing != s.applierMissing
	s.applierMissing = missing
	s.pollMu.Unlock()
	if missing == "" || !changed {
		return
	}
	s.log().InfoContext(ctx, "Found no log applier process for the master's log",
		"source", st.Replication.Source, "logPath", missing)
}

// role returns the local node's authoritative CUBRID role.
func (s *Server) role(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), roleBudget)
	defer cancel()
	st := HeartbeatStatus(ctx, s.cli)
	st.EngineVersion = s.engineVersion(ctx)
	replCtx, cancelRepl := context.WithTimeout(ctx, replicationBudget)
	defer cancelRepl()
	st.Replication = replicationOf(replCtx, s.cli, st, s.replicationDB, s.databasesDir)
	s.logMissingApplier(ctx, st)
	st.Unfinished = s.unfinishedDatabase()
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
	s.version = readEngineVersion(ctx, s.cli)
	return s.version
}

// readEngineVersion reads the engine's full version from cubrid_rel, or ""
// when it cannot. The package's tests replace it, so that their CLI
// stand-ins, which do not answer cubrid_rel, run on the pinned engine.
var readEngineVersion = func(ctx context.Context, cli CLI) string {
	out, err := cli.Run(ctx, "cubrid_rel")
	if err != nil {
		return ""
	}
	return ParseEngineVersion(out)
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
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req BackupRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgInvalidBody})
		return
	}
	// Nothing is done for a request this manager does not support: no
	// directory, no operation and no command.
	if err := validateBackupRequest(req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: err.Error()})
		return
	}

	// From here on only the path built from the manager's own root is used.
	destination, err := confineBackupDestination(req.Destination, s.backupStagingRoot)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: err.Error()})
		return
	}
	req.Destination = destination

	if err := s.admit(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{errKey: err.Error()})
		return
	}
	if s.store == nil {
		defer s.ops.Done()
		ctx, cancel := context.WithTimeout(r.Context(), s.timeouts.Backup)
		defer cancel()
		defer context.AfterFunc(s.opsCtx, cancel)()
		if req.Upload != nil {
			if _, err := s.backupEngineVersion(ctx, req); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: "backup refused: " + err.Error()})
				return
			}
		}
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
		s.ops.Done()
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgNoIdempotencyKey})
		return
	}
	// A backup that cannot be uploaded never completes, so it is refused
	// before backupdb runs and before an operation is recorded: a repeat
	// once the manager has object storage starts it afresh (#290).
	if status, err := s.backupPreflight(req); err != nil {
		s.ops.Done()
		writeErr(w, status, err)
		return
	}

	// The operation stages in a directory of its own below the destination.
	op, staging, err := s.recordBackup(key, HashRequest(body), req)
	if err != nil || staging == "" {
		s.ops.Done()
	}
	switch {
	case errors.Is(err, ErrIdempotencyConflict):
		writeErr(w, http.StatusConflict, err)
		return
	case errors.Is(err, ErrOperationInProgress):
		writeErr(w, http.StatusConflict, err)
		return
	case errors.Is(err, ErrBackupStagingInUse):
		writeErr(w, http.StatusConflict, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if staging == "" {
		writeJSON(w, http.StatusAccepted, op)
		return
	}

	req.Destination = staging
	s.runBackup(op.ID, req)
	writeJSON(w, http.StatusAccepted, op)
}

// backupPreflight checks that an asynchronous backup can be uploaded: the
// request names a bucket, and this process was started with object storage.
// The second is answered 412, apart from a malformed request, so the
// operator can tell a Pod that has not picked up the cluster's settings.
func (s *Server) backupPreflight(req BackupRequest) (int, error) {
	if req.Upload == nil || req.Upload.Bucket == "" {
		return http.StatusBadRequest, errors.New("a backup needs an upload with a bucket; it is not run without one")
	}
	if s.objects == nil {
		return http.StatusPreconditionFailed, errors.New("this Instance Manager was started without object storage " +
			"(IM_S3_ENDPOINT is not set); the Pod has to be restarted with the cluster's spec.objectStorage")
	}
	return 0, nil
}

// findOperations returns the operations the store holds under the
// idempotencyKey query parameter, as an OperationList; an empty list means
// none. It only reads the store and starts nothing, so the operator can ask a
// member whether a start whose answer was lost was taken, without sending the
// start again (ADR-0007, #365). A manager without a store cannot say that no
// operation exists and answers 503.
func (s *Server) findOperations(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("idempotencyKey")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "idempotencyKey is required"})
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{errKey: "operations are not enabled"})
		return
	}
	ops, err := s.store.FindByKey(key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
		return
	}
	list := OperationList{Operations: make([]Operation, 0, len(ops))}
	for _, op := range ops {
		list.Operations = append(list.Operations, *op)
	}
	writeJSON(w, http.StatusOK, list)
}

// notRecorded is the failure reason of an operation that stops because its
// next state could not be written: it does nothing that is not on record.
func notRecorded(state OperationState, err error) string {
	return "could not record the state " + string(state) + ", so the operation went no further: " + err.Error()
}

// runBackup executes the backup in the background and records the durable state
// transitions. A successful `backupdb` is NOT Completed on its own: the staged
// output must upload and manifest.json (the atomic completion marker) must be
// written before the operation reaches Completed. On any failure it terminates
// Failed and removes the staging directory once no process holds a file in it,
// or keeps it, unused, when one still does after a bound (ADR-0003/0007).
func (s *Server) runBackup(id string, req BackupRequest) {
	go func() {
		defer s.ops.Done()
		ctx, cancel := context.WithTimeout(s.opsCtx, s.timeouts.Backup)
		defer cancel()
		fail := func(reason string) {
			reason = s.stoppingReason(reason)
			// cub_server writes the file of backupdb -C and may still hold
			// it after Run returned (POC-27): remove the staging only once
			// nothing does, and keep it, unused, when that does not happen.
			if err := s.awaitStagingWriters(id, req.Destination); err != nil {
				reason += "; " + err.Error()
			} else {
				s.removeBackupStaging(req.Destination)
			}
			_ = s.store.Settle(id, func(op *Operation) {
				op.State = OpFailed
				op.FailureReason = reason
			})
		}

		if _, err := s.store.Update(id, func(op *Operation) { op.State = OpRunningBackup }); err != nil {
			fail(notRecorded(OpRunningBackup, err))
			return
		}
		engine, err := s.backupEngineVersion(ctx, req)
		if err != nil {
			fail("backup refused: " + err.Error())
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
		// backupPreflight refused this before; kept so that the upload below
		// never runs without its store.
		if s.objects == nil || req.Upload == nil {
			fail("backupdb succeeded but no object-storage destination is configured")
			return
		}

		if _, err := s.store.Update(id, func(op *Operation) { op.State = OpUploading }); err != nil {
			fail(notRecorded(OpUploading, err))
			return
		}
		res, err := UploadBackup(ctx, s.objects, UploadSpec{
			StagingDir: req.Destination,
			Bucket:     req.Upload.Bucket,
			Prefix:     req.Upload.Prefix,
			Seed:       req.Upload.Seed,
			Manifest: BackupManifest{
				Database:       req.Database,
				ClusterUID:     req.Upload.ClusterUID,
				CubridVersion:  engine,
				Level:          req.Level,
				SourceInstance: req.Upload.SourceInstance,
				SourceRole:     req.Upload.SourceRole,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				OperationID:    id,
			},
		})
		if err != nil {
			fail("upload failed: " + err.Error())
			return
		}
		// Upload + manifest succeeded: remove staging, then mark Completed.
		s.removeBackupStaging(req.Destination)
		_ = s.store.Settle(id, func(op *Operation) {
			op.State = OpCompleted
			op.Artifact = &OperationArtifact{
				ManifestURI:    res.ManifestURI,
				ManifestDigest: res.ManifestDigest,
				SizeBytes:      res.SizeBytes,
				Database:       req.Database,
				CubridVersion:  engine,
				Level:          req.Level,
			}
		})
	}()
}

// backupEngineVersion returns the engine version a backup of req records in
// its manifest: the one this member runs, read from cubrid_rel, never the
// version the request declares (ADR-0011). It refuses the backup when that
// version cannot be read, or when it and the declared one cannot be reduced
// to a series or are of different series: the member then runs another
// engine than its cluster declares, and the artifact would be mislabeled
// either way.
func (s *Server) backupEngineVersion(ctx context.Context, req BackupRequest) (string, error) {
	engine := s.engineVersion(ctx)
	if engine == "" {
		return "", errors.New("the engine version of this member could not be read from cubrid_rel")
	}
	declared := ""
	if req.Upload != nil {
		declared = req.Upload.CubridVersion
	}
	series := EngineSeries(engine)
	if series == "" {
		return "", fmt.Errorf("the engine version %q that cubrid_rel gives has no series", engine)
	}
	if series != EngineSeries(declared) {
		return "", fmt.Errorf("this member runs engine %s, which is not of the declared version %q", engine, declared)
	}
	return engine, nil
}

// restorePrepare verifies + downloads + restoredb's a backup artifact into an
// empty target as an async, idempotent operation (ADR-0008). It requires an
// operation store and an object store; the request must carry an Idempotency-Key.
func (s *Server) restorePrepare(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.objects == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{errKey: "restore is not enabled (no operation/object store)"})
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req RestoreRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgInvalidBody})
		return
	}
	// The request is checked before it is admitted and recorded, so an
	// invalid one leaves no operation behind. Restore checks it again.
	if err := validateRestoreRequest(req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: err.Error()})
		return
	}
	if err := s.validateRestoreDatabase(req.Database); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: err.Error()})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgNoIdempotencyKey})
		return
	}
	if err := s.admitDisruptive(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{errKey: err.Error()})
		return
	}

	op, existed, err := s.store.FindOrCreate(OpRestore, key, HashRequest(body), req.Database)
	if err != nil || existed {
		s.endDisruptive()
	}
	switch {
	case errors.Is(err, ErrIdempotencyConflict):
		writeErr(w, http.StatusConflict, err)
		return
	case errors.Is(err, ErrOperationInProgress):
		writeErr(w, http.StatusConflict, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if existed {
		writeJSON(w, http.StatusAccepted, op)
		return
	}

	s.runRestore(op.ID, req)
	writeJSON(w, http.StatusAccepted, op)
}

// A standalone member restores only the database it is configured to start.
func (s *Server) validateRestoreDatabase(database string) error {
	if s.standaloneDB != "" && s.standaloneDB != database {
		return fmt.Errorf("restore database %q does not match configured standalone database %q", database, s.standaloneDB)
	}
	return nil
}

// runRestore executes the restore in the background and records the durable
// state transitions. It reaches Completed only after restoredb succeeds against
// a verified artifact and the database is started; any failure (HA
// configuration, trust, download, wrong-target, restoredb, start) terminates
// Failed with an explicit reason (ADR-0003/0008).
//
// The database directory keeps this operation's ownership marker until
// Completed is recorded. Once restoredb has succeeded the restored artifact is
// recorded before anything is started, and a later attempt of the same
// request starts that data again instead of restoring over it; data whose
// restore was not recorded was never started, and may be removed (#267).
func (s *Server) runRestore(id string, req RestoreRequest) {
	go func() {
		defer s.endDisruptive()
		ctx, cancel := context.WithTimeout(s.opsCtx, s.timeouts.Restore)
		defer cancel()
		fail := func(reason string) {
			reason = s.stoppingReason(reason)
			_ = s.store.Settle(id, func(op *Operation) {
				op.State = OpFailed
				op.FailureReason = reason
			})
		}

		// An HA member registers the restored database under the member list,
		// as createdb does on the first member. A member that is not
		// standalone and has no usable HA configuration restores nothing:
		// the result could neither be registered for HA nor join it.
		roots := s.restoreRoots
		roots.Owner = id
		roots.EngineVersion = s.engineVersion(ctx)
		haMember := s.standaloneDB == ""
		if haMember {
			hosts, err := haHosts(s.ha.ConfPath)
			if err != nil {
				fail("restore refused: this member is not standalone and has no usable HA configuration: " + err.Error())
				return
			}
			roots.Host = hosts
		}
		if _, err := s.store.Update(id, func(op *Operation) { op.State = OpDownloading }); err != nil {
			fail(notRecorded(OpDownloading, err))
			return
		}
		if _, err := s.store.Update(id, func(op *Operation) { op.State = OpRestoring }); err != nil {
			fail(notRecorded(OpRestoring, err))
			return
		}
		// Nothing is taken over, removed or restored on an engine the
		// restore may not run on (ADR-0011).
		if err := checkRestoreEngine(roots.EngineVersion, req.ExpectedCubridVersion); err != nil {
			fail("restore refused: " + err.Error())
			return
		}
		// Data that a failed attempt of this same request restored is taken
		// over and started, never restored over.
		restored, err := s.adoptRestored(id, req.Database)
		if err != nil {
			fail("restore failed: " + err.Error())
			return
		}
		if restored != nil && EngineSeries(restored.CubridVersion) != EngineSeries(roots.EngineVersion) {
			fail("restore refused: the data an earlier attempt restored is of engine " + restored.CubridVersion +
				", not of the series of this member's engine " + roots.EngineVersion + "; it is kept and not started (ADR-0011)")
			return
		}
		if restored == nil {
			// What an interrupted restore of this manager left is removed first;
			// anything else in the target is refused by the restore's own guard.
			if err := s.reclaimIncomplete(req.Database); err != nil {
				fail("restore failed: " + err.Error())
				return
			}
			res, err := Restore(ctx, s.cli, s.objects, roots, req)
			if err != nil {
				fail("restore failed: " + err.Error())
				return
			}
			restored = &OperationArtifact{
				ManifestURI:   res.ManifestURI,
				Database:      res.Database,
				CubridVersion: res.CubridVersion,
			}
			// Nothing is started unless the restore is on record: data that
			// was never started is what a later attempt may remove.
			if _, err := s.store.Update(id, func(op *Operation) { op.Restored = restored }); err != nil {
				fail("record the restored database: " + err.Error())
				return
			}
		}
		// The restored data is kept when the start fails.
		storeFailed := false
		err = s.startRestoredDatabase(ctx, req.Database, haMember, func() error {
			_, err := s.store.Update(id, func(op *Operation) { op.State = OpStarting })
			storeFailed = err != nil
			return err
		})
		if err != nil {
			if storeFailed {
				err = errors.New(notRecorded(OpStarting, err))
			}
			fail(err.Error())
			return
		}
		// A Completed that cannot be written leaves the ownership marker:
		// the database runs, and its restore is not on record as complete.
		if err := s.store.Settle(id, func(op *Operation) {
			op.State = OpCompleted
			op.Artifact = restored
		}); err != nil {
			return
		}
		// A marker left by a failure here belongs to a Completed operation:
		// ResumeCompletedRestores or the next operation on this database
		// clears it.
		if err := clearOwned(s.restoreRoots.Target, req.Database); err != nil {
			s.log().Warn("Could not clear the ownership marker of a completed restore",
				"operationID", id, "database", req.Database, "err", err)
		}
	}()
}

// startRestoredDatabase starts a restored database, which the entrypoint left
// stopped: in a recovery bootstrap it started nothing and runs only once, and
// it starts no database that carries an ownership marker. A standalone
// member's server is started; a configured HA member joins HA, with
// `cubrid heartbeat start` issued only when observeStartable allows it: two
// rounds, heartbeatSettle apart, of a heartbeat status that exited by itself
// without HA node information and a process table without any of
// heartbeatVeto. A round that did not answer, or that sees such a process,
// starts nothing and is an error. Heartbeat that already runs is not started
// again and counts as joined only in a confirmed role; any other state is an
// error, by the rule the HA bootstrap shares (heartbeatObservation.startError).
// before runs ahead of
// each start command, and an error from it stops before that command; nil
// runs nothing.
func (s *Server) startRestoredDatabase(ctx context.Context, database string, haMember bool, before func() error) error {
	if err := s.validateRestoreDatabase(database); err != nil {
		return err
	}
	if before == nil {
		before = func() error { return nil }
	}
	if s.standaloneDB != "" {
		if err := before(); err != nil {
			return err
		}
		if out, err := s.activate(ctx, "server", "start", database); err != nil {
			return fmt.Errorf("server start failed: %w: %s", err, out)
		}
	}
	if !haMember {
		return nil
	}
	hb := s.observeStartable(ctx)
	if err := hb.startError(); err != nil {
		return err
	}
	if hb.startable {
		if err := before(); err != nil {
			return err
		}
		if out, err := s.activate(ctx, "heartbeat", "start"); err != nil {
			return fmt.Errorf("heartbeat start failed: %w: %s", err, out)
		}
	}
	return nil
}

// ResumeCompletedRestores runs once when the manager starts. A database whose
// directory still carries the ownership marker of a restore recorded as
// Completed is whole, but the entrypoint did not start it because of that
// marker, and no operation will come for it. It is started the way the
// restore started it, and the marker is cleared afterwards, so that a start
// that fails is tried again at the next start of the manager. A marker of any
// other operation is left for the next operation on that database to judge.
// A member that is not standalone needs a usable HA configuration for it;
// without one nothing is started, the marker stays and the error is returned.
// It counts as a running operation: once the member is stopping it starts
// nothing, and a stop waits for it.
func (s *Server) ResumeCompletedRestores(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	if err := s.admitResume(); err != nil {
		return err
	}
	defer s.ops.Done()
	ctx, cancel := context.WithTimeout(ctx, s.timeouts.Bootstrap)
	defer cancel()
	defer context.AfterFunc(s.opsCtx, cancel)()
	target := s.restoreRoots.Target
	entries, err := os.ReadDir(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the database root: %w", err)
	}
	// A member that is not standalone joins HA, which takes its HA
	// configuration: without a usable one its database is not started.
	haMember := s.standaloneDB == ""
	var haErr error
	if haMember {
		_, haErr = haHosts(s.ha.ConfPath)
	}
	var errs []error
	for _, e := range entries {
		database := e.Name()
		if !e.IsDir() || !databaseNamePattern.MatchString(database) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(target, markerPath(database))) // #nosec G304 -- a plain name below the database root
		if err != nil {
			continue
		}
		op, err := s.store.Get(strings.TrimSpace(string(data)))
		if err != nil || op.Kind != OpRestore || op.State != OpCompleted || op.Restored == nil || op.Database != database {
			continue
		}
		if haErr != nil {
			errs = append(errs, fmt.Errorf("start %s of completed restore %s: the HA configuration is not usable: %w", database, op.ID, haErr))
			continue
		}
		s.log().Info("Starting the database of a completed restore", "operationID", op.ID, "database", database)
		if err := s.startRestoredDatabase(ctx, database, haMember, nil); err != nil {
			errs = append(errs, fmt.Errorf("start %s of completed restore %s: %w", database, op.ID, err))
			continue
		}
		if err := clearOwned(target, database); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
	requested := r.URL.Query().Get("copiedLogPath")
	if db == "" || requested == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "database and copiedLogPath are required"})
		return
	}
	copied, err := confineCopiedLogPath(db, requested, s.databasesDir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ApplyConvergenceStatus(r.Context(), s.cli, db, copied))
}

// confineCopiedLogPath maps the copied-log path of a convergence request onto
// the manager's databases directory and returns a path built from that
// directory: `<databases>/<database>_<host>`, where a slave keeps the log it
// copies from the member <host> (docs/poc/RESULTS.md, POC-14). Any other path
// is refused, so the request cannot make applyinfo read elsewhere. <host> is
// a single DNS label: HA members are named by their short StatefulSet pod
// names, not by FQDNs (ADR-0004), so a name with a dot is no member's.
func confineCopiedLogPath(database, copiedLogPath, databasesDir string) (string, error) {
	if databasesDir == "" || !filepath.IsAbs(databasesDir) || filepath.Clean(databasesDir) != databasesDir {
		return "", fmt.Errorf("the databases directory %q is not configured as a clean absolute path", databasesDir)
	}
	if !databaseNamePattern.MatchString(database) {
		return "", fmt.Errorf("database name %q is not a plain identifier", database)
	}
	host, isCopiedLog := strings.CutPrefix(filepath.Base(copiedLogPath), database+"_")
	if filepath.Clean(copiedLogPath) != copiedLogPath || filepath.Dir(copiedLogPath) != databasesDir ||
		!isCopiedLog || !hostLabelPattern.MatchString(host) {
		return "", fmt.Errorf("copiedLogPath %q is not a copied log of %s directly below %q", copiedLogPath, database, databasesDir)
	}
	return filepath.Join(databasesDir, database+"_"+host), nil
}

// readBody reads a request body of at most maxRequestBody bytes. It answers
// a larger or unreadable one itself and reports false.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge,
			map[string]string{errKey: fmt.Sprintf("the request body is larger than %d bytes", maxRequestBody)})
		return nil, false
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgUnreadableBody})
		return nil, false
	}
	return body, true
}

// validateBackupRequest rejects a backup request this manager does not carry
// out as asked (#301). The database name becomes a command argument and part
// of file names, so it is a plain identifier. Only a full backup is
// supported: an incremental one needs the levels before it, which nothing
// here keeps track of. An upload names where the artifact goes and what its
// manifest says, so each of its fields is present and of the expected form;
// the prefix stays below the place it names.
func validateBackupRequest(req BackupRequest) error {
	if req.Database == "" || req.Destination == "" {
		return errors.New("database and destination are required")
	}
	if !databaseNamePattern.MatchString(req.Database) {
		return fmt.Errorf("database name %q is not a plain identifier", req.Database)
	}
	if req.Level != 0 {
		return fmt.Errorf("backup level %d is not supported: only a full backup (level 0) is", req.Level)
	}
	up := req.Upload
	if up == nil {
		return nil
	}
	if up.Bucket == "" || up.Prefix == "" || up.ClusterUID == "" || up.CubridVersion == "" || up.SourceInstance == "" {
		return errors.New("upload needs bucket, prefix, clusterUID, cubridVersion and sourceInstance")
	}
	if path.IsAbs(up.Prefix) || path.Clean(up.Prefix) != up.Prefix || up.Prefix == ".." || strings.HasPrefix(up.Prefix, "../") {
		return fmt.Errorf("upload prefix %q must be a clean relative key prefix", up.Prefix)
	}
	if !hostLabelPattern.MatchString(up.SourceInstance) {
		return fmt.Errorf("upload sourceInstance %q is not a host name (a DNS label)", up.SourceInstance)
	}
	if up.Seed {
		if err := validateSeedUpload(up.Prefix, up.ClusterUID, req.Database, up.SourceRole); err != nil {
			return err
		}
	}
	manifestPrefix := up.Prefix
	if !up.Seed {
		manifestPrefix = BackupAttemptPrefix(up.Prefix, strings.Repeat("0", BackupAttemptIDLength))
	}
	if len(path.Join(manifestPrefix, ManifestObjectName)) > 1024 {
		return errors.New("upload manifest key exceeds the S3 1024-byte limit, including the attempt suffix")
	}
	// unknown is what the operator records for the only member of a
	// single-instance cluster, whose role is not resolved (ADR-0007,
	// RoleUnknownSingleInstanceFallback).
	switch Role(up.SourceRole) {
	case "", RoleMaster, RoleSlave, RoleReplica, RoleUnknown:
	default:
		return fmt.Errorf("upload sourceRole %q is not master, slave, replica or unknown", up.SourceRole)
	}
	return nil
}

// shutdown performs the ADR-0003 ordered graceful shutdown (withdraw HA, stop server).
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.timeouts.Shutdown)
	defer cancel()
	if err := s.Stop(ctx, r.URL.Query().Get("database")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutdown"})
}

// auth wraps /v1 handlers with bearer-token authentication (ADR-0003). Every
// caller needs the token, the "instance-manager shutdown" command of the same
// Pod included; a request from loopback is not an exception. A manager that
// was given no token, or only white space, refuses every request, so that an
// unset token can never mean "open". The SHA-256 digests of the two values
// are compared in constant time, which hides the token's length as well.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	tokenSet := strings.TrimSpace(s.token) != ""
	want := sha256.Sum256([]byte("Bearer " + s.token))
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get(authHeader)
		got := sha256.Sum256([]byte(header))
		if !tokenSet || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			reason := reasonWrongToken
			switch {
			case !tokenSet:
				reason = reasonTokenNotSet
			case header == "":
				reason = reasonNoToken
			}
			s.logRefused(w, r, reason)
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// isLoopback reports whether a request's RemoteAddr is the Pod itself. It is
// written to the log only; it gives no access.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
// destination does not exist (docs/poc/RESULTS.md, POC-12). destination is a
// directory below the root: the request's destination, or an operation's
// own directory below it. It is created through an os.Root on the staging
// root, so it cannot land elsewhere. For an asynchronous backup the claim
// (recordBackup) has already created the operation's directory under
// stagingMu, so this finds it in place and changes nothing; it runs outside
// stagingMu for that reason.
func (s *Server) createBackupStaging(destination string) error {
	rel, err := s.stagingRel(destination)
	if err != nil {
		return err
	}
	root, err := s.openStagingRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(rel, 0o750); err != nil {
		return fmt.Errorf("create the backup staging directory: %w", err)
	}
	return nil
}

// removeBackupStaging deletes a backup's staging directory, and the
// destination it is in once that is empty. It removes through an os.Root on
// the staging root, so it cannot reach outside it, and does not follow links.
func (s *Server) removeBackupStaging(destination string) {
	rel, err := s.stagingRel(destination)
	if err != nil {
		return
	}
	s.stagingMu.Lock()
	defer s.stagingMu.Unlock()
	root, err := os.OpenRoot(s.backupStagingRoot)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	_ = root.RemoveAll(rel)
	if parent := filepath.Dir(rel); parent != "." {
		_ = root.Remove(parent)
	}
}
