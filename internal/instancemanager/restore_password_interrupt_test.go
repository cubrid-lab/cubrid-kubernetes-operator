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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// diskPassword is the DBA password the database on the member's volume has,
// shared by the managers that run one after the other on that volume. Empty
// is none.
type diskPassword struct {
	mu      sync.Mutex
	current string
}

func (d *diskPassword) get() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

func (d *diskPassword) set(v string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.current = v
}

// Points at which interruptCLI stops a restore, as a kill of the Instance
// Manager there would: the command that follows never returns.
const (
	stopAfterRestoredb = "after restoredb"
	stopAfterAlter     = "after ALTER USER"
	stopBeforeStart    = "after Restored is recorded, before the start"
)

// interruptCLI is a member whose restoredb leaves a volume and gives the
// database the DBA password of the backup's source, and whose csql behaves as
// on the real engine (docs/poc/RESULTS.md, POC-61): an empty login is refused
// once a password is set, an ALTER USER fed on stdin sets it, and a login on
// a terminal is accepted only with it. With stop set, the restore never gets
// past that point, as if the manager had been killed there; reached is closed
// when it gets there.
type interruptCLI struct {
	volumeCLI
	t       *testing.T
	disk    *diskPassword
	source  string
	stop    string
	reached chan struct{}
	once    sync.Once
}

func newInterruptCLI(t *testing.T, f *resumeFixture, disk *diskPassword, stop string) *interruptCLI {
	c := &interruptCLI{volumeCLI: volumeCLI{databases: f.databases}, t: t, disk: disk, stop: stop,
		reached: make(chan struct{})}
	c.afterRestore = func() {
		c.disk.set(c.source)
		c.halt(stopAfterRestoredb)
	}
	return c
}

// halt never returns when the restore is to stop at point.
func (c *interruptCLI) halt(point string) {
	if c.stop != point {
		return
	}
	c.once.Do(func() { close(c.reached) })
	select {}
}

func (c *interruptCLI) record(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *interruptCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == csqlCommand {
		c.record(name + " " + strings.Join(args, " "))
		if c.disk.get() != "" {
			return outRefused, exitStatus1(c.t)
		}
		return outAccepted, nil
	}
	if len(args) > 1 && args[0]+" "+args[1] == stepServerStart {
		c.halt(stopBeforeStart)
	}
	return c.volumeCLI.Run(ctx, name, args...)
}

func (c *interruptCLI) RunWithStdin(_ context.Context, stdin, name string, args ...string) (string, error) {
	c.record(name + " " + strings.Join(args, " "))
	c.disk.set(alterPassword(stdin))
	c.halt(stopAfterAlter)
	return outAlterOK, nil
}

func (c *interruptCLI) RunOnTerminal(_ context.Context, _, input, name string, args ...string) (string, bool, error) {
	c.record("terminal " + name + " " + strings.Join(args, " "))
	if current := c.disk.get(); current != "" && input == current {
		return "\n" + outAccepted, true, nil
	}
	return "\n" + outRefused, true, exitStatus1(c.t)
}

// interruptRestore starts a restore on f with a DBA password requested and
// waits until it stopped at point, as a manager killed there would; the
// fixture's manager is then restarted by the caller.
func interruptRestore(t *testing.T, f *resumeFixture, disk *diskPassword, point string) Operation {
	t.Helper()
	first := newInterruptCLI(t, f, disk, point)
	f.start(first)
	op := f.post("restore-1")
	select {
	case <-first.reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("the restore never reached the point %q; calls: %q", point, first.recorded())
	}
	return op
}

// A restore is interrupted, as by a kill of the Instance Manager, at each
// point of its DBA password step: after restoredb and before the password is
// set, after the ALTER USER and before the restore is recorded, and after the
// restore is recorded as restored but before the server starts. The manager
// restarts, which fails the interrupted operation, and the Operator retries
// the same restore. ADR-0008 decides what the retry does with the data: data
// never recorded as restored is taken back and restored again, and data
// recorded as restored is taken over and started, never restored over. In
// every case the password step runs or is confirmed before the server starts,
// so the database never serves with the empty password or another one (#801).
func TestServer_Restore_RetryAfterAnInterruptedPasswordStep(t *testing.T) {
	for _, tc := range []struct {
		point string
		// restoredAgain says whether the retry restores again (the data was
		// not recorded as restored) or takes the data over.
		restoredAgain bool
	}{
		{point: stopAfterRestoredb, restoredAgain: true},
		{point: stopAfterAlter, restoredAgain: true},
		{point: stopBeforeStart, restoredAgain: false},
	} {
		t.Run(tc.point, func(t *testing.T) {
			f := newResumeFixture(t)
			f.dbaPasswordFile = passwordFile(t, testPassword)
			disk := &diskPassword{}
			interrupted := interruptRestore(t, f, disk, tc.point)

			retry := newInterruptCLI(t, f, disk, "")
			f.start(retry)
			if got := f.marker(); got != interrupted.ID {
				t.Fatalf("marker after the interruption = %q, want the interrupted operation %q", got, interrupted.ID)
			}
			final := f.run("restore-2")
			if final.State != OpCompleted {
				t.Fatalf("retry = %s (%s), want Completed", final.State, final.FailureReason)
			}
			calls := retry.recorded()
			start := indexOf(calls, callServerStart)
			login := indexOf(calls, callTerminalLogin)
			if start < 0 || login < 0 || login > start {
				t.Errorf("retry calls = %q, want a login with the requested password before the server start", calls)
			}
			if restored := count(calls, "cubrid restoredb "); (restored == 1) != tc.restoredAgain {
				t.Errorf("retry ran restoredb %d times, want restored again = %v: %q", restored, tc.restoredAgain, calls)
			}
			if tc.restoredAgain {
				if set := indexOf(calls, callSetPass); set < 0 || set > start {
					t.Errorf("retry calls = %q, want the password set on the newly restored data before the start", calls)
				}
			} else if len(retry.stdins()) != 0 {
				t.Errorf("the retry set a password on data that already had it: %q", calls)
			}
			if got := disk.get(); got != testPassword {
				t.Errorf("the database ends with the DBA password %q, want the requested one", got)
			}
			f.wantRestoredDataKept()
			if got := f.marker(); got != "" {
				t.Errorf("marker after the completed retry = %q, want none", got)
			}
		})
	}
}

// A restore recorded as restored was interrupted before its server started,
// and the Secret's value was changed before the retry. The data has the
// password the first attempt set, which is not the one requested now. The
// retry takes the data over, as ADR-0008 says, but does not start it: it
// fails, names the DBA password, and keeps the data, which may hold the only
// copy of what was restored.
func TestServer_Restore_RetryDoesNotStartTakenOverDataWithAnotherPassword(t *testing.T) {
	f := newResumeFixture(t)
	f.dbaPasswordFile = passwordFile(t, testPassword)
	disk := &diskPassword{}
	interruptRestore(t, f, disk, stopBeforeStart)

	f.dbaPasswordFile = passwordFile(t, otherSourcePassword)
	retry := newInterruptCLI(t, f, disk, "")
	f.start(retry)
	final := f.run("restore-2")
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "not the requested") {
		t.Fatalf("retry = %s (%s), want Failed saying the DBA password is not the requested one", final.State, final.FailureReason)
	}
	calls := retry.recorded()
	if indexOf(calls, callServerStart) >= 0 {
		t.Errorf("the server was started with a password other than the requested one: %q", calls)
	}
	if count(calls, "cubrid restoredb ") != 0 || len(retry.stdins()) != 0 {
		t.Errorf("the retry restored or set a password: %q", calls)
	}
	if got := disk.get(); got != testPassword {
		t.Errorf("the database's password was changed to %q", got)
	}
	f.wantRestoredDataKept()
	for _, v := range []string{testPassword, otherSourcePassword} {
		if strings.Contains(final.FailureReason, v) {
			t.Errorf("the failure reason carries a password: %q", final.FailureReason)
		}
	}
}

// A completed restore whose marker was left is started when the manager
// starts (ResumeCompletedRestores). With a DBA password requested, its
// password is confirmed first: a Secret whose value was changed since leaves
// the database stopped, keeps the marker, and returns the error, so the
// member does not start serving with the old password.
func TestResumeCompletedRestores_ConfirmsTheDBAPasswordFirst(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested string
		ok        bool
	}{
		{name: "the requested password", requested: testPassword, ok: true},
		{name: "a changed Secret value", requested: otherSourcePassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResumeFixture(t)
			f.dbaPasswordFile = passwordFile(t, testPassword)
			disk := &diskPassword{}
			f.start(newInterruptCLI(t, f, disk, ""))
			done := f.run("restore-1")
			if done.State != OpCompleted {
				t.Fatalf("restore = %s (%s), want Completed", done.State, done.FailureReason)
			}
			if err := os.WriteFile(filepath.Join(f.databases, dbName, ownershipMarker), []byte(done.ID+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			f.dbaPasswordFile = passwordFile(t, tc.requested)
			restarted := newInterruptCLI(t, f, disk, "")
			f.start(restarted)
			err := f.server.ResumeCompletedRestores(t.Context())
			calls := restarted.recorded()
			login, start := indexOf(calls, callTerminalLogin), indexOf(calls, callServerStart)
			if tc.ok {
				if err != nil {
					t.Fatalf("ResumeCompletedRestores: %v", err)
				}
				if login < 0 || start < 0 || login > start {
					t.Errorf("calls = %q, want a login with the requested password before the start", calls)
				}
				if got := f.marker(); got != "" {
					t.Errorf("marker = %q, want it cleared", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "not the requested") {
				t.Fatalf("ResumeCompletedRestores = %v, want an error saying the DBA password is not the requested one", err)
			}
			if start >= 0 {
				t.Errorf("the server was started: %q", calls)
			}
			if got := f.marker(); got != done.ID {
				t.Errorf("marker = %q, want it kept (%q)", got, done.ID)
			}
			if strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), otherSourcePassword) {
				t.Errorf("the error carries a password: %v", err)
			}
		})
	}
}

// stdins lists the ALTER USER runs recorded, without their input.
func (c *interruptCLI) stdins() []string {
	var runs []string
	for _, call := range c.recorded() {
		if call == callSetPass {
			runs = append(runs, call)
		}
	}
	return runs
}
