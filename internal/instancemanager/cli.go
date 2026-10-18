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
	"syscall"
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
	grouped := len(args) > 0 && groupUtilities[args[0]]
	if grouped {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	}
	runErr := cmd.Run()
	if grouped && cmd.Process != nil {
		endGroup(cmd.Process.Pid)
	}
	out, readErr := os.ReadFile(output.Name())
	if readErr != nil {
		// What the command printed is unknown, whatever its exit status.
		return "", errors.Join(fmt.Errorf("%w: %s: %w", ErrOutputUnread, name, readErr), runErr)
	}
	return string(out), runErr
}

// groupUtilities are the `cubrid` utilities that run in a process group of
// their own, which a cancellation ends as a whole. `cubrid <utility>` forks
// cub_admin, which does the work; a signal to `cubrid` alone (SIGKILL, as the
// default cancellation sends, or SIGTERM) leaves cub_admin running on its own,
// and it goes on writing the restored database or driving the backup to its
// end (docs/poc/RESULTS.md, POC-27). The starts of the server and of
// heartbeat leave daemons on purpose and are never among them. A backupdb -C
// file is written by cub_server, which is not in the group: it kept the file
// open for up to about 1 s after the group had ended, and Run does not wait
// for that.
var groupUtilities = map[string]bool{"backupdb": true, "restoredb": true, "restoreslave": true}

// groupPoll is how often endGroup looks whether a process group has ended.
const groupPoll = 20 * time.Millisecond

// killGroup sends SIGKILL to the process group pgid. A group that has already
// ended is not an error.
func killGroup(pgid int) error {
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

// endGroup returns once no process of the group pgid exists, after sending it
// SIGKILL: the caller removes or retries what the utility wrote right after
// Run, so nothing of it may still write then. After a SIGKILL to the group,
// cub_admin was still seen for up to 3 s (POC-27). A process that SIGKILL
// cannot end keeps the operation waiting here; a stop does not wait for that
// operation beyond its own deadline. The orphaned members are reaped by PID 1
// (the entrypoint), which is when the group ceases to exist.
func endGroup(pgid int) {
	_ = killGroup(pgid)
	for !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		time.Sleep(groupPoll)
	}
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

// startError says why nothing may follow this observation, or nil. A status
// that did not answer, or a CUBRID process that runs, issues no start; nor
// does heartbeat that runs without reporting the current node in a confirmed
// role (master, slave or replica), which neither is started again nor counts
// as joined: a transition, or the stable `state unknown` after a heartbeat
// stop or a failed start (docs/poc/RESULTS.md, POC-26). nil means a start may
// be issued (startable) or heartbeat already runs in a confirmed role. The HA
// bootstrap and the start of a restored database share this rule.
func (hb heartbeatObservation) startError() error {
	switch {
	case !hb.startable && !hb.running:
		return errors.New(hb.reason + "; heartbeat start was not issued")
	case hb.running && hb.status.Role == RoleUnknown:
		return errors.New(hb.reason + ": heartbeat runs without a confirmed role; heartbeat start was not issued")
	}
	return nil
}

// observeHeartbeat runs `cubrid heartbeat status` once. An unknown role is
// not enough to start heartbeat: output with HA node information, in any
// state, means heartbeat runs, and a second start while it activates flips it
// off again (docs/poc/RESULTS.md, POC-3/POC-7). A status that did not run to
// its end, by itself and with its output read, answered nothing, and nothing
// is started on it (ADR-0005): a timeout, a cancellation, a signal, a command
// that could not be run, or output that could not be read.
//
// A status that ran to its end without HA node information is necessary for a
// start, not enough: it printed the same while heartbeat was never started and
// while a start or a stop was under way (docs/poc/RESULTS.md, POC-26).
// observeStartable adds what tells them apart.
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

// heartbeatSettle is the default time between the two observations a
// heartbeat start needs. In POC-26 status printed no HA node information up
// to 159 ms into a start (state 4d, a start right after a service stop), and
// the processes of a stop ran for about 1 s after it did; 2 s is above both,
// on one local host without load.
const heartbeatSettle = 2 * time.Second

// heartbeatVeto are the processes that forbid a heartbeat start while status
// prints no HA node information (docs/poc/RESULTS.md, POC-26): cub_master ran
// in the first B answers of 10 of 11 starts, and cub_server and cub_pl ran on
// after cub_master during a service stop, cub_pl for about 1 s, as long as
// copylogdb and applylogdb. Those two run as cub_admin, which is no veto: the
// manager's own utilities, such as applyinfo for /v1/ha/convergence, may run
// under that name too, and would fail operations for nothing. Nor does the
// manager's own createdb or restoredb leave a veto behind: on cubrid/cubrid:11.4
// (local Docker, one standalone container, a few runs) /proc showed no cub*
// process when sampled every 100 ms for 1 s after createdb and 0.6 s after
// each of 3 restoredb -u runs exited 0. Names match exactly.
var heartbeatVeto = []string{"cub_master", "cub_server", "cub_pl"}

// observeStartable decides whether `cubrid heartbeat start` may be issued
// now. The start is issued only when two observations, heartbeatSettle apart,
// each print no HA node information (observeHeartbeat) and see none of
// heartbeatVeto running: one answer cannot tell a member whose heartbeat was
// never started from one whose start or stop is under way (POC-26). A process
// table that cannot be read starts nothing, and the wait ends with ctx, so a
// stop ends it. Known limits: a start issued outside the manager while
// cub_master comes and goes (POC-26 state 4b) passes when both process
// snapshots miss it, and a start or a stop from outside the manager that
// begins after the second snapshot and before the start command is not seen
// (check, then act). The shutdown fallback that runs in another process
// before the manager listens is #323.
func (s *Server) observeStartable(ctx context.Context) heartbeatObservation {
	hb := s.observeQuiet(ctx)
	if !hb.startable {
		return hb
	}
	settle := time.NewTimer(s.heartbeatSettle)
	defer settle.Stop()
	select {
	case <-ctx.Done():
		return heartbeatObservation{reason: "the wait between two heartbeat status calls ended: " + ctx.Err().Error()}
	case <-settle.C:
	}
	return s.observeQuiet(ctx)
}

// observeQuiet is one observation of observeStartable.
func (s *Server) observeQuiet(ctx context.Context) heartbeatObservation {
	hb := observeHeartbeat(ctx, s.cli)
	if !hb.startable {
		return hb
	}
	running, err := s.heartbeatProcesses()
	if err != nil {
		return heartbeatObservation{reason: "the process list could not be read: " + err.Error()}
	}
	for _, name := range heartbeatVeto {
		if running[name] {
			return heartbeatObservation{reason: "heartbeat status printed no HA node information, but " + name +
				" runs: a start or a stop may be under way"}
		}
	}
	return hb
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
