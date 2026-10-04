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
	"os"
	"os/exec"
	"regexp"
	"time"
)

// CLI runs a local CUBRID command and returns combined output. It is the single
// seam the Instance Manager uses to reach CUBRID, so it can be faked in tests
// (ADR-0003: local ops via the manager, never pods/exec).
type CLI interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecCLI runs commands via os/exec with a bounded timeout.
type ExecCLI struct {
	Timeout time.Duration
}

func (c ExecCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	// A caller that set a deadline knows how long its command may take
	// (backupdb, restoredb, server stop); the default bounds everything else.
	if _, ok := ctx.Deadline(); !ok {
		to := c.Timeout
		if to <= 0 {
			to = 10 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}
	// The output goes to a file, not a pipe: `cubrid server start` and
	// `cubrid heartbeat start` leave daemons that inherit the command's
	// output, and reading a pipe would wait for them instead of the command.
	output, err := os.CreateTemp("", "im-cli-*")
	if err != nil {
		return "", fmt.Errorf("output file for %s: %w", name, err)
	}
	defer func() {
		_ = output.Close()
		_ = os.Remove(output.Name())
	}()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = output, output
	runErr := cmd.Run()
	out, readErr := os.ReadFile(output.Name())
	if runErr == nil && readErr != nil {
		return "", fmt.Errorf("read the output of %s: %w", name, readErr)
	}
	return string(out), runErr
}

// HeartbeatStatus runs `cubrid heartbeat status` and parses the local node's
// authoritative HA view. A command failure or timeout yields role=unknown with
// a reason, never a fabricated role (ADR-0005).
func HeartbeatStatus(ctx context.Context, cli CLI) HAStatus {
	out, err := cli.Run(ctx, "cubrid", "heartbeat", "status")
	if err != nil {
		return HAStatus{Role: RoleUnknown, Source: "heartbeat", Reason: "heartbeat status failed: " + err.Error()}
	}
	return ParseHAStatus(out)
}

// engineVersionPattern matches the full version in cubrid_rel output:
// "CUBRID 11.4.6 (11.4.6.1963-0e7d3c1) (64bit ..." (docs/poc/RESULTS.md, POC-10).
var engineVersionPattern = regexp.MustCompile(`\((\d+\.\d+\.\d+\.\d+)[-)]`)

// ParseEngineVersion extracts the full engine version ("11.4.6.1963") from
// cubrid_rel output, or "" when the output does not carry one.
func ParseEngineVersion(out string) string {
	m := engineVersionPattern.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

// serverStatusLine matches one running server in `cubrid server status`:
// " Server appdb (rel 11.4.6, pid 14)" (docs/poc/RESULTS.md, POC-10).
var serverStatusLine = regexp.MustCompile(`(?m)^\s*Server\s+(\S+)\s+\(rel [^,]+, pid \d+\)`)

// ServerRunning reports whether `cubrid server status` lists database. The
// command exits 0 whether or not a server runs, and prints one line per
// running server, so a failed command is an error, not "stopped".
func ServerRunning(ctx context.Context, cli CLI, database string) (bool, error) {
	out, err := cli.Run(ctx, "cubrid", "server", "status")
	if err != nil {
		return false, fmt.Errorf("server status failed: %w", err)
	}
	for _, m := range serverStatusLine.FindAllStringSubmatch(out, -1) {
		if m[1] == database {
			return true, nil
		}
	}
	return false, nil
}
