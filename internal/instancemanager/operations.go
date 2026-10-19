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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// BackupRequest asks the manager to run `cubrid backupdb` locally (ADR-0007).
type BackupRequest struct {
	Database    string `json:"database"`
	Destination string `json:"destination"`
	Level       int    `json:"level,omitempty"`
	// Upload, when set, uploads the staged backup to object storage and writes
	// manifest.json as the atomic completion marker (async /v1/backup only).
	Upload *BackupUpload `json:"upload,omitempty"`
}

// BackupUpload describes the object-storage destination + source metadata used
// to build the manifest (ADR-0007). Credentials come from the manager's env,
// never the request body.
type BackupUpload struct {
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	ClusterUID     string `json:"clusterUID"`
	CubridVersion  string `json:"cubridVersion"`
	SourceInstance string `json:"sourceInstance"`
	SourceRole     string `json:"sourceRole"`
}

// BackupResult reports the outcome of a local backup.
type BackupResult struct {
	Database    string `json:"database"`
	Destination string `json:"destination"`
	Level       int    `json:"level"`
	Output      string `json:"output,omitempty"`
}

// Backup runs `cubrid backupdb` in client-server mode against the running
// server (POC-4: SA mode conflicts a running server, so CS mode is required).
// It writes to Destination; upload to object storage is the operator's job
// (ADR-0007).
func Backup(ctx context.Context, cli CLI, req BackupRequest) (BackupResult, error) {
	if req.Database == "" || req.Destination == "" {
		return BackupResult{}, fmt.Errorf("database and destination are required")
	}
	args := []string{
		"backupdb",
		"-D", req.Destination,
		"-C",
		"-l", fmt.Sprintf("%d", req.Level),
		req.Database + "@localhost",
	}
	out, err := cli.Run(ctx, "cubrid", args...)
	if err != nil {
		return BackupResult{}, fmt.Errorf("backupdb failed: %w: %s", err, out)
	}
	return BackupResult{
		Database:    req.Database,
		Destination: req.Destination,
		Level:       req.Level,
		Output:      out,
	}, nil
}

// haNotConfigured is what `cubrid heartbeat stop` prints on a standalone
// server, where there is no HA participation to withdraw (observed on CUBRID
// 11.4.6, #148).
const haNotConfigured = "not configured for HA"

// Shutdown performs the ADR-0003 ordered graceful shutdown: withdraw HA
// participation first, then stop the local server. A failed step does not stop
// the sequence — the goal is to leave the node cleanly stopped — and every
// failure is reported. A standalone server has no heartbeat to stop; that is
// not a failure.
func Shutdown(ctx context.Context, cli CLI, database string) error {
	var errs []error
	// Withdraw HA/heartbeat first so the peer can react before the server goes.
	if out, err := cli.Run(ctx, "cubrid", "heartbeat", "stop"); err != nil && !strings.Contains(out, haNotConfigured) {
		errs = append(errs, fmt.Errorf("heartbeat stop failed: %w: %s", err, out))
	}
	if database != "" {
		if out, err := cli.Run(ctx, "cubrid", "server", "stop", database); err != nil {
			errs = append(errs, fmt.Errorf("server stop failed: %w: %s", err, out))
		}
	}
	return errors.Join(errs...)
}

// shutdownReserve is the part of a stop's deadline kept for the ordered
// shutdown: waiting for running operations never takes it. A shorter budget
// keeps half of itself instead.
const shutdownReserve = 30 * time.Second

// errStopping is why a mutating operation is refused, or does not start
// CUBRID, once the member is stopping.
var errStopping = errors.New("the member is stopping")

// Stop stops CUBRID on this member: the one implementation behind every
// trigger of a stop (the preStop hook, a termination signal, a request of the
// operator). It marks the stop as intended before it does anything. From then
// on no mutating operation is admitted and none starts CUBRID; the running
// ones are cancelled, and the shutdown waits for them to end, so that none of
// them starts CUBRID after it. The wait ends early enough to leave the
// shutdown shutdownReserve of ctx's deadline. A stop that succeeded is not
// run a second time; one whose wait ran out still runs the shutdown and
// reports the operations it did not see end, and the next stop runs again.
func (s *Server) Stop(ctx context.Context, database string) error {
	s.stopRequested.Store(true)
	s.lifeMu.Lock()
	s.stopping = true
	s.lifeMu.Unlock()
	s.endOps()

	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.stopped {
		return nil
	}
	waitCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		reserve := min(shutdownReserve, time.Until(deadline)/2)
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithDeadline(ctx, deadline.Add(-reserve))
		defer cancel()
	}
	waitErr := s.awaitOperations(waitCtx)
	if err := Shutdown(ctx, s.cli, database); err != nil || waitErr != nil {
		return errors.Join(waitErr, err)
	}
	s.stopped = true
	return nil
}

// admit counts a new mutating operation as running, or says why it is
// refused: the member is stopping, or the manager is still starting. An
// admitted operation calls s.ops.Done when it ends.
func (s *Server) admit() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.stopping {
		return errStopping
	}
	if s.starting {
		return errStarting
	}
	s.ops.Add(1)
	return nil
}

// admitResume is admit for the resume of completed restores, which is what
// the starting manager does.
func (s *Server) admitResume() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.stopping {
		return errStopping
	}
	s.ops.Add(1)
	return nil
}

// admitDisruptive is admit for an operation that stops and starts CUBRID
// itself. Until it calls endDisruptive, instead of s.ops.Done, the process
// watch accepts that CUBRID's processes are absent.
func (s *Server) admitDisruptive() error {
	if err := s.admit(); err != nil {
		return err
	}
	s.disruptive.Add(1)
	return nil
}

func (s *Server) endDisruptive() {
	s.disruptive.Add(-1)
	s.ops.Done()
}

// errStarting is why a mutating operation is refused while the manager
// starts: it still resumes completed restores, and no request races that.
var errStarting = errors.New("the manager is starting")

// ResumeAndServe brings a starting manager into service. It calls serve,
// which starts to listen and returns, removes the backup staging that a
// restart abandoned (reclaimBackupStaging), and then starts the databases of
// completed restores (ResumeCompletedRestores). The manager listens first so
// that a shutdown during the resume reaches it and is ordered after the
// start that is running; were nobody listening, "instance-manager shutdown"
// would stop CUBRID itself and the resume could start it again afterwards.
// That is what a shutdown before the listen does: the command records the
// stop (RecordStopRequest) before it asks, and the manager looks for the
// record after it listens. A stop recorded before that look makes the member
// stopping, and nothing is reclaimed or started; one recorded after it was
// recorded after the listen, so the command's request reaches the manager.
// Until the resume has ended every other mutating operation is refused. A
// resume that fails is logged; the manager serves all the same.
func (s *Server) ResumeAndServe(ctx context.Context, serve func() error) error {
	s.lifeMu.Lock()
	s.starting = true
	s.lifeMu.Unlock()
	defer func() {
		s.lifeMu.Lock()
		s.starting = false
		s.lifeMu.Unlock()
	}()
	if err := serve(); err != nil {
		return err
	}
	if s.honorRecordedStop() {
		return nil
	}
	// Backup staging that ended or unknown operations left is removed; that
	// of a live operation is judged by the store and kept.
	s.reclaimBackupStaging()
	if err := s.ResumeCompletedRestores(ctx); err != nil {
		s.log().Error("Could not start the database of a completed restore", "err", err)
	}
	return nil
}

// awaitOperations waits until every admitted operation has ended, or ctx
// ends.
func (s *Server) awaitOperations(ctx context.Context) error {
	done := make(chan struct{})
	// When ctx ends first this goroutine stays parked until the operations
	// end; the member is stopping, so nothing accumulates.
	go func() {
		s.ops.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("operations were still running when the stop proceeded: %w", ctx.Err())
	}
}

// stoppingReason prefixes the failure reason of an operation that ended
// while the member is stopping, so that the reason says so.
func (s *Server) stoppingReason(reason string) string {
	if s.opsCtx.Err() == nil || strings.Contains(reason, errStopping.Error()) {
		return reason
	}
	return errStopping.Error() + ": " + reason
}

// activate runs a `cubrid` command that starts CUBRID (server start,
// heartbeat start) for an admitted operation, unless the member is stopping
// or a stop was recorded (honorRecordedStop). A stop that begins after the
// check cancels the command and waits for the operation, so the start is
// over before the shutdown runs.
func (s *Server) activate(ctx context.Context, args ...string) (string, error) {
	if s.honorRecordedStop() {
		return "", errStopping
	}
	s.lifeMu.Lock()
	stopping := s.stopping
	s.lifeMu.Unlock()
	if stopping {
		return "", errStopping
	}
	return s.cli.Run(ctx, "cubrid", args...)
}

// StopFile is where "instance-manager shutdown" records a stop. It is a
// fixed path on the container's own file system, which a restarted container
// does not keep, so a recorded stop holds for the container it was asked for
// and for no later one. It must never be on a volume: a record that outlived
// its container would keep every later container from starting CUBRID.
const StopFile = "/tmp/instance-manager-stop-requested"

// WithStopFile sets where the manager looks for a stop that
// "instance-manager shutdown" recorded (RecordStopRequest): once it listens
// and before every start of CUBRID. The manager passes StopFile; tests pass
// their own path.
func (s *Server) WithStopFile(path string) *Server {
	s.stopFile = path
	return s
}

// RecordStopRequest records that a stop of this member was asked for.
// "instance-manager shutdown" calls it before it asks the manager, so that a
// manager that does not listen yet, and would later, finds the stop and
// starts nothing (#323). Only the file's existence counts: an existing file
// is neither truncated nor followed when it is a symbolic link. Nothing
// removes the record: a stop is final for the container.
func RecordStopRequest(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- the fixed StopFile, or a test's path
	if err != nil {
		return err
	}
	return f.Close()
}

// honorRecordedStop says whether a stop was recorded in the stop file and,
// if so, makes the member stopping as Stop does, without running the
// shutdown: the command that recorded the stop runs it, or asks this manager
// to. A stop file that cannot be examined counts as a recorded stop, so that
// a doubt never ends in a start; it is logged as such, not as a stop that
// was asked for.
func (s *Server) honorRecordedStop() bool {
	if s.stopFile == "" {
		return false
	}
	_, err := os.Lstat(s.stopFile)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	s.stopRequested.Store(true)
	s.lifeMu.Lock()
	wasStopping := s.stopping
	s.stopping = true
	s.lifeMu.Unlock()
	s.endOps()
	switch {
	case err != nil:
		s.log().Error("Could not examine the stop file, so CUBRID is not started", "event", "stop_file_unreadable", "path", s.stopFile, "err", err)
	case !wasStopping:
		s.log().Info("Found a recorded stop, so CUBRID is not started", "event", "stop_recorded", "path", s.stopFile)
	}
	return true
}

// RequestShutdown is what "instance-manager shutdown" does. It asks the
// manager that listens at managerURL to stop CUBRID, so that the manager
// knows the stop is intended, and it sends the token of its Pod like every
// other caller. When no manager answers, for example because the container is
// terminated before the manager was started, it stops CUBRID itself with the
// same procedure. A manager that answers and refuses is not that case: the
// refusal is returned and nothing is stopped here. Its caller records the
// stop first (RecordStopRequest), so that a manager that listens only after
// this stop starts nothing.
func RequestShutdown(ctx context.Context, managerURL, token, database string, cli CLI, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := managerURL + "/v1/shutdown?database=" + url.QueryEscape(database)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set(authHeader, "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Nobody listens: this process is the only one that can stop CUBRID.
		return Shutdown(ctx, cli, database)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("the manager answered the shutdown with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
