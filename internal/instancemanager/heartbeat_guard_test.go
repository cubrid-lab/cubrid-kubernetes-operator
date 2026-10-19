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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// procPL and procAdmin are the comm names of cub_pl and of copylogdb and
	// applylogdb in POC-26.
	procPL    = "cub_pl"
	procAdmin = "cub_admin"
)

// unreachableOut is what heartbeat status printed, with exit 1, while a start
// with the current host missing from ha_node_list ran: shape C of
// docs/poc/RESULTS.md, POC-26, stdout and stderr in one stream.
const unreachableOut = "@ cubrid heartbeat status\nCould not connect to master server on localhost\n"

// processTable returns a process table that answers each read with the next
// snapshot, and with the last one again once they are used up. No snapshot
// is a table in which no CUBRID process runs.
func processTable(snapshots ...[]string) func() (map[string]bool, error) {
	var mu sync.Mutex
	next := 0
	return func() (map[string]bool, error) {
		mu.Lock()
		defer mu.Unlock()
		running := map[string]bool{}
		if len(snapshots) == 0 {
			return running, nil
		}
		for _, name := range snapshots[min(next, len(snapshots)-1)] {
			running[name] = true
		}
		next++
		return running, nil
	}
}

// statusAnswer is one answer of heartbeat status: what it printed and
// whether it exited 1.
type statusAnswer struct {
	out   string
	exit1 bool
}

var (
	// answerB, answerC and answerE are the shapes B, C and E of POC-26.
	answerB = statusAnswer{noNodeOut, true}
	answerC = statusAnswer{unreachableOut, true}
	answerE = statusAnswer{slaveOut, false}
)

// sequenceCLI answers heartbeat status with the next of its answers, and the
// last one again once they are used up, and records when each status ran.
// Every other command succeeds.
type sequenceCLI struct {
	t        *testing.T
	mu       sync.Mutex
	answers  []statusAnswer
	statuses []time.Time
	calls    []string
}

func (c *sequenceCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if len(args) < 2 || args[0] != heartbeatCmd || args[1] != "status" {
		return "ok", nil
	}
	a := c.answers[min(len(c.statuses), len(c.answers)-1)]
	c.statuses = append(c.statuses, time.Now())
	if a.exit1 {
		return a.out, exitStatus1(c.t)
	}
	return a.out, nil
}

func (c *sequenceCLI) statusCalls() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.statuses...)
}

func guardedServer(cli CLI, processes func() (map[string]bool, error), settle time.Duration) *Server {
	s := NewServer(cli, "tok")
	s.heartbeatProcesses, s.heartbeatSettle = processes, settle
	return s
}

// The sequences of docs/poc/RESULTS.md, POC-26. Status printed B (exit 1, no
// HA node information) while heartbeat was never started, and also while a
// start or a stop was under way. A start is issued only when two
// observations, the settle interval apart, both print no HA node information
// and see none of cub_master, cub_server and cub_pl.
func TestObserveStartable_POC26Sequences(t *testing.T) {
	cases := []struct {
		name      string
		answers   []statusAnswer
		processes [][]string
		startable bool
		reason    string
	}{
		// 1a, 1b: nothing started.
		{"never started", []statusAnswer{answerB, answerB}, [][]string{{}, {}}, true, ""},
		// 3: B in the first 10 to 115 ms of a start, in 10 of 11 runs with
		// cub_master already running.
		{"a start with cub_master up", []statusAnswer{answerB}, [][]string{{procMaster}}, false, procMaster},
		// 3: the one run whose B came before any process; E from 37 to 141 ms on.
		{"a start before any process", []statusAnswer{answerB, answerE}, [][]string{{}, {procMaster, procServer}}, false, "state 'slave'"},
		// 5b: B once cub_master has exited, while the server still runs.
		{"a stop with the server up", []statusAnswer{answerB}, [][]string{{procServer, procAdmin, procPL}}, false, procServer},
		// 5b: copylogdb and applylogdb (cub_admin) and cub_pl run for about
		// 1 s more; cub_pl covers that tail.
		{"a stop with the log processes up", []statusAnswer{answerB}, [][]string{{procAdmin, procPL}}, false, procPL},
		// 4d: a start right after service stop, B while the old server ran.
		{"a start after a stop", []statusAnswer{answerB}, [][]string{{procServer}}, false, procServer},
		// 4b: only B and C for the 180 s of the start, cub_master coming and
		// going about once a second.
		{"a start that cannot bring cub_master up", []statusAnswer{answerB, answerC}, [][]string{{}, {procMaster}}, false, procMaster},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := &sequenceCLI{t: t, answers: c.answers}
			hb := guardedServer(cli, processTable(c.processes...), time.Millisecond).observeStartable(t.Context())
			if hb.startable != c.startable {
				t.Fatalf("startable = %v (%s), want %v", hb.startable, hb.reason, c.startable)
			}
			if !c.startable && !strings.Contains(hb.reason, c.reason) {
				t.Errorf("reason = %q, want it to name %q", hb.reason, c.reason)
			}
		})
	}
}

// A process table that cannot be read says nothing about a start or a stop
// under way: nothing is started on it.
func TestObserveStartable_UnreadableProcessTable(t *testing.T) {
	cli := &sequenceCLI{t: t, answers: []statusAnswer{answerB}}
	unreadable := func() (map[string]bool, error) { return nil, errors.New("open /proc: permission denied") }
	hb := guardedServer(cli, unreadable, time.Millisecond).observeStartable(t.Context())
	if hb.startable || hb.running || !strings.Contains(hb.reason, "process") {
		t.Errorf("startable, running = %v, %v (%s), want neither, naming the process table", hb.startable, hb.running, hb.reason)
	}
}

// The two observations are the settle interval apart.
func TestObserveStartable_WaitsTheSettleInterval(t *testing.T) {
	const settle = 100 * time.Millisecond
	cli := &sequenceCLI{t: t, answers: []statusAnswer{answerB}}
	if hb := guardedServer(cli, processTable(), settle).observeStartable(t.Context()); !hb.startable {
		t.Fatalf("startable = false (%s), want true", hb.reason)
	}
	calls := cli.statusCalls()
	if len(calls) != 2 || calls[1].Sub(calls[0]) < settle {
		t.Errorf("status calls at %v, want two, %v apart", calls, settle)
	}
}

// A veto ends with the processes: once the stop is over, a start is issued.
func TestObserveStartable_AStopThatEndedNoLongerVetoes(t *testing.T) {
	cli := &sequenceCLI{t: t, answers: []statusAnswer{answerB}}
	s := guardedServer(cli, processTable([]string{procPL}, []string{}), time.Millisecond)
	if hb := s.observeStartable(t.Context()); hb.startable {
		t.Fatal("startable while cub_pl ran")
	}
	if hb := s.observeStartable(t.Context()); !hb.startable {
		t.Errorf("startable = false (%s) once no CUBRID process ran", hb.reason)
	}
}

// cub_admin is not a veto: the manager's own utilities may run as cub_admin
// (applyinfo for /v1/ha/convergence), which would fail operations for nothing.
func TestObserveStartable_CubAdminAloneIsNoVeto(t *testing.T) {
	cli := &sequenceCLI{t: t, answers: []statusAnswer{answerB}}
	if hb := guardedServer(cli, processTable([]string{procAdmin}), time.Millisecond).observeStartable(t.Context()); !hb.startable {
		t.Errorf("startable = false (%s) with cub_admin alone", hb.reason)
	}
}

// Process names match exactly: a process whose name only begins with one of
// the vetoed names is no veto.
func TestObserveStartable_MatchesProcessNamesExactly(t *testing.T) {
	cli := &sequenceCLI{t: t, answers: []statusAnswer{answerB}}
	if hb := guardedServer(cli, processTable([]string{procServer + "_xyz"}), time.Millisecond).observeStartable(t.Context()); !hb.startable {
		t.Errorf("startable = false (%s) with %s_xyz alone", hb.reason, procServer)
	}
}

// A second round that finds heartbeat running with a confirmed role follows
// the rule for running heartbeat: the bootstrap completes without a start.
func TestHABootstrap_SecondRoundWithARoleCompletesWithoutAStart(t *testing.T) {
	f := newHAFixture(t)
	cli := &sequenceCLI{t: t, answers: []statusAnswer{answerB, {masterOut, false}}}
	f.srv.cli = cli
	final := f.run("boot-1")
	if final.State != OpCompleted {
		t.Fatalf("final = %s (%s), want Completed", final.State, final.FailureReason)
	}
	cli.mu.Lock()
	calls := append([]string(nil), cli.calls...)
	cli.mu.Unlock()
	if count(calls, callHeartbeatStart) != 0 || len(cli.statusCalls()) != 2 {
		t.Errorf("calls = %q, want two statuses and no heartbeat start", calls)
	}
}

// The HA bootstrap issues no heartbeat start while cub_master runs.
func TestHABootstrap_DoesNotStartHeartbeatWhileCUBRIDRuns(t *testing.T) {
	f := newHAFixture(t)
	f.srv.heartbeatProcesses = processTable([]string{procMaster})
	final := f.run("boot-1")
	if final.State != OpFailed || !strings.Contains(final.FailureReason, procMaster) {
		t.Fatalf("final = %s (%s), want Failed naming cub_master", final.State, final.FailureReason)
	}
	if calls := f.cli.recorded(); count(calls, callHeartbeatStart) != 0 {
		t.Errorf("heartbeat was started while cub_master ran: %q", calls)
	}
}

// A stop during the settle interval ends the wait: nothing is started, and
// the bootstrap fails because the member is stopping.
func TestHABootstrap_StopDuringTheSettleIntervalStartsNothing(t *testing.T) {
	f := newHAFixture(t)
	f.srv.heartbeatSettle = time.Hour
	read := make(chan struct{}, 1)
	f.srv.heartbeatProcesses = func() (map[string]bool, error) {
		select {
		case read <- struct{}{}:
		default:
		}
		return map[string]bool{}, nil
	}
	rr := f.post("boot-1")
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("the bootstrap never read the process table")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := f.srv.Stop(ctx, ""); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatalf("/v1/ha/bootstrap = %d: %s", rr.Code, rr.Body.String())
	}
	final := pollUntilTerminal(t, f.handler, op.ID)
	if final.State != OpFailed || !strings.Contains(final.FailureReason, errStopping.Error()) ||
		!strings.Contains(final.FailureReason, "the wait between two heartbeat status calls ended") {
		t.Fatalf("final = %s (%s), want Failed because the member is stopping, naming the wait", final.State, final.FailureReason)
	}
	if calls := f.cli.recorded(); count(calls, callHeartbeatStart) != 0 {
		t.Errorf("heartbeat was started after the stop: %q", calls)
	}
}

// The restore of an HA member issues no heartbeat start while the server of
// a stop still runs, and keeps the restored data.
func TestServer_Restore_DoesNotStartHeartbeatWhileCUBRIDRuns(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(conf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}
	final := runRestoreOperation(t, cli, restoreMember{haConf: conf, processes: processTable([]string{procServer})})
	if final.State != OpFailed || !strings.Contains(final.FailureReason, procServer) {
		t.Fatalf("final = %s (%s), want Failed naming cub_server", final.State, final.FailureReason)
	}
	if calls := cli.recorded(); count(calls, callHeartbeatStart) != 0 {
		t.Errorf("heartbeat was started while cub_server ran: %q", calls)
	}
}

// The start of a completed restore's database, when the manager starts,
// issues no heartbeat start while cub_master runs, and keeps the marker.
func TestResumeCompletedRestores_DoesNotStartHeartbeatWhileCUBRIDRuns(t *testing.T) {
	f := newResumeFixture(t)
	f.haConf = filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(f.haConf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}, databases: f.databases})
	done := f.run("restore-1")
	if done.State != OpCompleted {
		t.Fatalf("restore = %s (%s), want Completed", done.State, done.FailureReason)
	}
	if err := os.WriteFile(filepath.Join(f.databases, dbName, ownershipMarker), []byte(done.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := &volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}, databases: f.databases}
	f.processes = processTable([]string{procMaster})
	f.start(restarted)
	if err := f.server.ResumeCompletedRestores(t.Context()); err == nil || !strings.Contains(err.Error(), procMaster) {
		t.Errorf("ResumeCompletedRestores = %v, want an error naming cub_master", err)
	}
	if calls := restarted.recorded(); count(calls, callHeartbeatStart) != 0 {
		t.Errorf("heartbeat was started while cub_master ran: %q", calls)
	}
	if got := f.marker(); got != done.ID {
		t.Errorf("marker = %q, want it kept as %q", got, done.ID)
	}
}
