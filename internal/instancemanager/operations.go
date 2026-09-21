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
	"fmt"
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

// Shutdown performs the ADR-0003 ordered graceful shutdown: withdraw HA
// participation first, then stop the local server. Errors are collected but do
// not stop the sequence — the goal is to leave the node cleanly stopped.
func Shutdown(ctx context.Context, cli CLI, database string) error {
	// Withdraw HA/heartbeat first so the peer can react before the server goes.
	if out, err := cli.Run(ctx, "cubrid", "heartbeat", "stop"); err != nil {
		return fmt.Errorf("heartbeat stop failed: %w: %s", err, out)
	}
	if database != "" {
		if out, err := cli.Run(ctx, "cubrid", "server", "stop", database); err != nil {
			return fmt.Errorf("server stop failed: %w: %s", err, out)
		}
	}
	return nil
}
