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
	"net/http"
	"net/http/httptest"
	"os/exec"
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
	srv := NewServer(cli, "tok").WithTimeouts(Timeouts{Shutdown: 90 * time.Second})
	req := httptest.NewRequest(http.MethodPost, "/v1/shutdown?database=appdb", nil)
	req.Header.Set("Authorization", "Bearer tok")
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

// noNodeOut is what heartbeat status is assumed to print on an HA member
// whose heartbeat is not active. The real output is not recorded in docs/poc
// (#345); it stands for any output without HA node information.
const noNodeOut = "++ cubrid heartbeat status: fail"

// Heartbeat may be started only on a status that ran to its end by itself,
// with its output read, without HA node information; a reported state, in any
// form, and a status that did not answer start nothing.
func TestObserveHeartbeat(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name               string
		ctx                context.Context
		cli                CLI
		startable, running bool
	}{
		{"a master", nil, fakeCLI{out: masterOut}, false, true},
		{"transition", nil, fakeCLI{out: transitionOut}, false, true},
		{"state with an exit error", nil, fakeCLI{out: transitionOut, err: exitStatus1(t)}, false, true},
		{"node lines without a header", nil, fakeCLI{out: "   Node cub-1 (priority 2, state slave)"}, false, true},
		{"a header in another form", nil, fakeCLI{out: " HA-Node Info (current: cub-0)"}, false, true},
		{"no node reported, exit 1", nil, fakeCLI{out: noNodeOut, err: exitStatus1(t)}, true, false},
		{"no node reported, exit 0", nil, fakeCLI{out: noNodeOut}, true, false},
		{"deadline", nil, fakeCLI{err: context.DeadlineExceeded}, false, false},
		{"killed by the timeout", nil, sleepingCLI{}, false, false},
		{"not an exit", nil, fakeCLI{err: errors.New(`exec: "cubrid": executable file not found in $PATH`)}, false, false},
		{"output unread", nil, fakeCLI{err: fmt.Errorf("%w: cubrid: read failed", ErrOutputUnread)}, false, false},
		{"output unread after an exit", nil, fakeCLI{err: errors.Join(ErrOutputUnread, exitStatus1(t))}, false, false},
		{"cancelled caller", cancelled, fakeCLI{out: noNodeOut, err: exitStatus1(t)}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := c.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			hb := observeHeartbeat(ctx, c.cli)
			if hb.startable != c.startable || hb.running != c.running {
				t.Errorf("startable, running = %v, %v (%s), want %v, %v", hb.startable, hb.running, hb.reason, c.startable, c.running)
			}
		})
	}
}

// exitStatus1 returns the error of a command that ran to its end and exited
// 1, as ExecCLI reports it.
func exitStatus1(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	if err == nil {
		t.Fatal("sh -c 'exit 1' succeeded")
	}
	return err
}
