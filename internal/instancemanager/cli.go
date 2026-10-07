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

// ErrOutputUnread marks a command whose output could not be read. ExecCLI
// returns it whatever the command's exit status was.
var ErrOutputUnread = errors.New("the command's output could not be read")

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
	if readErr != nil {
		// What the command printed is unknown, whatever its exit status.
		return "", errors.Join(fmt.Errorf("%w: %s: %w", ErrOutputUnread, name, readErr), runErr)
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

// heartbeatReport matches any sign of HA node information in heartbeat status
// output: the HA-Node Info header or a node line. It is looser than the
// parser on purpose, so that output whose form differs from what the parser
// expects counts as running heartbeat and starts nothing.
var heartbeatReport = regexp.MustCompile(`(?m)HA-Node Info|^\s*Node \S+ \(priority`)

// heartbeatHeader takes the current node's state from the HA-Node Info line in
// any state, including hyphenated transitions such as "to-be-master" that
// reCurrent does not take.
var heartbeatHeader = regexp.MustCompile(`HA-Node Info \(current [^,]+, state ([^)]+)\)`)

// heartbeatObservation is what one `cubrid heartbeat status` says about
// whether `cubrid heartbeat start` may be issued.
type heartbeatObservation struct {
	// startable is set when the command ran to its end, its output was read
	// and the output carries no HA node information.
	startable bool
	// running is set when the output carries HA node information in any form.
	running bool
	// status is the parsed output when running is set.
	status HAStatus
	// reason says what was seen when startable is not set.
	reason string
}

// observeHeartbeat runs `cubrid heartbeat status` once. An unknown role is
// not enough to start heartbeat: output with HA node information, in any
// state, means heartbeat runs, and a second start while it activates flips it
// off again (docs/poc/RESULTS.md, POC-3/POC-7). A status that did not run to
// its end, by itself and with its output read, answered nothing, and nothing
// is started on it (ADR-0005): a timeout, a cancellation, a signal, a command
// that could not be run, or output that could not be read.
//
// That a status which ran to its end without HA node information means
// inactive heartbeat is an assumption: the output of an HA member whose
// heartbeat is not active is not recorded in docs/poc (#345).
func observeHeartbeat(ctx context.Context, cli CLI) heartbeatObservation {
	out, err := cli.Run(ctx, "cubrid", "heartbeat", "status")
	if heartbeatReport.MatchString(out) {
		obs := heartbeatObservation{running: true, status: ParseHAStatus(out),
			reason: "heartbeat prints HA node information without a readable state of the current node"}
		if m := heartbeatHeader.FindStringSubmatch(out); m != nil {
			obs.reason = "heartbeat reports the current node in state '" + m[1] + "'"
		}
		if obs.status.Role == RoleUnknown && reCurrent.MatchString(out) {
			obs.reason += " (" + obs.status.Reason + ")"
		}
		return obs
	}
	var exitErr *exec.ExitError
	answered := err == nil ||
		(!errors.Is(err, ErrOutputUnread) && errors.As(err, &exitErr) && exitErr.Exited())
	if !answered || ctx.Err() != nil {
		why := err
		if why == nil {
			why = ctx.Err()
		}
		return heartbeatObservation{reason: "heartbeat status did not answer: " + why.Error()}
	}
	return heartbeatObservation{startable: true}
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
