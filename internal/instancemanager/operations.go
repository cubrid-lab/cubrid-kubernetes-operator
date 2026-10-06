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
	"net/http"
	"net/url"
	"strings"
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

// Stop stops CUBRID on this member: the one implementation behind every
// trigger of a stop (the preStop hook, a termination signal, a request of the
// operator). It marks the stop as intended before it does anything, and a
// stop that succeeded is not run a second time.
func (s *Server) Stop(ctx context.Context, database string) error {
	s.stopRequested.Store(true)
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.stopped {
		return nil
	}
	if err := Shutdown(ctx, s.cli, database); err != nil {
		return err
	}
	s.stopped = true
	return nil
}

// RequestShutdown is what "instance-manager shutdown" does. It asks the
// manager that listens at managerURL to stop CUBRID, so that the manager
// knows the stop is intended. When no manager answers, for example because
// the container is terminated before the manager was started, it stops
// CUBRID itself with the same procedure.
func RequestShutdown(ctx context.Context, managerURL, database string, cli CLI, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := managerURL + "/v1/shutdown?database=" + url.QueryEscape(database)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return err
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
