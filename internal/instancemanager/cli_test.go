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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A caller that brings its own deadline is not cut at the CLI default.
func TestExecCLI_UsesCallerDeadline(t *testing.T) {
	cli := ExecCLI{Timeout: 50 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := cli.Run(ctx, "sleep", "0.4"); err != nil {
		t.Fatalf("a command inside the caller's deadline failed: %v (%s)", err, out)
	}
}

func TestExecCLI_DefaultTimeoutWithoutDeadline(t *testing.T) {
	cli := ExecCLI{Timeout: 50 * time.Millisecond}
	start := time.Now()
	if _, err := cli.Run(context.Background(), "sleep", "5"); err == nil {
		t.Fatal("a command with no deadline ran past the CLI timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the default timeout did not apply: took %s", elapsed)
	}
}

// deadlineCLI records how far away the deadline of each command's context is.
type deadlineCLI struct {
	mu        sync.Mutex
	remaining map[string]time.Duration
}

func (d *deadlineCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.remaining == nil {
		d.remaining = map[string]time.Duration{}
	}
	key := name
	if len(args) > 1 {
		key = args[0] + " " + args[1]
	} else if len(args) == 1 {
		key = args[0]
	}
	if deadline, ok := ctx.Deadline(); ok {
		d.remaining[key] = time.Until(deadline)
	} else {
		d.remaining[key] = 0
	}
	return "", nil
}

func (d *deadlineCLI) get(key string) (time.Duration, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.remaining[key]
	return v, ok
}

func TestServer_ShutdownRunsUnderItsOwnDeadline(t *testing.T) {
	cli := &deadlineCLI{}
	srv := NewServer(cli, "").WithTimeouts(Timeouts{Shutdown: 90 * time.Second})
	req := httptest.NewRequest(http.MethodPost, "/v1/shutdown?database=appdb", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	for _, step := range []string{"heartbeat stop", "server stop"} {
		left, ok := cli.get(step)
		if !ok {
			t.Fatalf("%q was not run", step)
		}
		if left < 60*time.Second || left > 90*time.Second {
			t.Errorf("%q deadline is %s away, want the shutdown deadline (about 90s)", step, left)
		}
	}
}

func TestTimeouts_Defaults(t *testing.T) {
	got := Timeouts{}.withDefaults()
	if got.Backup != 2*time.Hour || got.Restore != 2*time.Hour || got.Shutdown != 100*time.Second ||
		got.Bootstrap != 15*time.Minute {
		t.Errorf("defaults = %+v", got)
	}
	// The shutdown deadline has to end before the Pod's preStop limit (110s).
	if got.Shutdown >= 110*time.Second {
		t.Errorf("shutdown default %s is not below the preStop limit", got.Shutdown)
	}
}

// `cubrid server start` leaves a daemon that inherits the command's output.
// Run must return when the command itself exits, with what it printed, and
// not wait for that daemon (observed with the real image, #178).
func TestExecCLI_DoesNotWaitForADaemonHoldingTheOutput(t *testing.T) {
	cli := ExecCLI{Timeout: 20 * time.Second}
	start := time.Now()
	out, err := cli.Run(context.Background(), "sh", "-c", "sleep 15 & echo started")
	if err != nil {
		t.Fatalf("Run: %v (%s)", err, out)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run waited %s for the background process", elapsed)
	}
	if out != "started\n" {
		t.Errorf("output = %q, want the command's own output", out)
	}
}

// sleepingCLI answers every command with a sleep that ExecCLI's default
// timeout cuts off, as a heartbeat status that hangs would be.
type sleepingCLI struct{}

func (sleepingCLI) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	return ExecCLI{Timeout: 50 * time.Millisecond}.Run(ctx, "sleep", "5")
}

// Heartbeat may be started only on a status that ran to its end without
// reporting the node; a reported state, in any form, and a status that did
// not answer start nothing.
func TestHeartbeatStartable(t *testing.T) {
	cases := []struct {
		name               string
		cli                CLI
		startable, running bool
	}{
		{"a master", fakeCLI{out: masterOut}, false, true},
		{"transition", fakeCLI{out: transitionOut}, false, true},
		{"state with an exit error", fakeCLI{out: transitionOut, err: errors.New("exit status 1")}, false, true},
		{"no node reported", fakeCLI{out: "++ cubrid heartbeat status: fail", err: errors.New("exit status 1")}, true, false},
		{"deadline", fakeCLI{err: context.DeadlineExceeded}, false, false},
		{"killed by the timeout", sleepingCLI{}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			startable, running, reason := heartbeatStartable(context.Background(), c.cli)
			if startable != c.startable || running != c.running {
				t.Errorf("startable, running = %v, %v (%s), want %v, %v", startable, running, reason, c.startable, c.running)
			}
		})
	}
}
