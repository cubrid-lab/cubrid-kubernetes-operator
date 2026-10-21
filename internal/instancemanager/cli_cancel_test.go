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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeUtility writes a script that behaves as `cubrid <utility>` did on the
// pinned image (docs/poc/RESULTS.md, POC-27): it forks a child that does the
// work, here appending to a file, and waits for it. The child writes its pid
// to pidFile once it runs and has written to the file once, so that a test
// that cancels on the pid finds the file (#516).
func fakeUtility(t *testing.T) (script, pidFile, written string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "cubrid")
	pidFile = filepath.Join(dir, "child.pid")
	written = filepath.Join(dir, "written")
	body := fmt.Sprintf(`#!/bin/sh
sh -c 'echo x >> %[2]q; echo $$ > %[1]q.tmp; mv %[1]q.tmp %[1]q; while :; do echo x >> %[2]q; sleep 0.02; done' &
wait
`, pidFile, written)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, pidFile, written
}

// waitPid waits until pidFile holds a pid.
func waitPid(pidFile string) (int, error) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid, nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, fmt.Errorf("no pid in %s", pidFile)
}

func childPid(t *testing.T, pidFile string) int {
	t.Helper()
	pid, err := waitPid(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// processRuns reports whether pid names a process, a zombie included.
func processRuns(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// A backupdb, restoredb, restoreslave or createdb that is cancelled, by its deadline or
// by its caller (a stop ends every running operation's context), leaves no
// process of the utility behind once Run has returned: the work is done by a
// child that outlives a signal to the direct process alone and goes on writing
// (POC-27), and the caller removes or retries what it wrote right after Run.
func TestExecCLI_CancelledUtilityLeavesNoWriter(t *testing.T) {
	for utility := range strings.FieldsSeq("backupdb restoredb restoreslave createdb") {
		for _, byDeadline := range []bool{true, false} {
			name := utility + "/cancelled"
			if byDeadline {
				name = utility + "/past its deadline"
			}
			t.Run(name, func(t *testing.T) {
				script, pidFile, written := fakeUtility(t)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if byDeadline {
					ctx, cancel = context.WithTimeout(ctx, time.Second)
					defer cancel()
				} else {
					go func() {
						_, _ = waitPid(pidFile)
						cancel()
					}()
				}
				var pid int
				t.Cleanup(func() {
					if pid != 0 {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				})

				done := make(chan error, 1)
				go func() {
					_, err := ExecCLI{}.Run(ctx, script, utility, "-u", dbName)
					done <- err
				}()
				var err error
				select {
				case err = <-done:
				case <-time.After(20 * time.Second):
					t.Fatal("group not reaped: Run did not return 20 s after it was started")
				}
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.Exited() {
					t.Fatalf("err = %v, want the utility ended by a signal", err)
				}
				pid = childPid(t, pidFile)
				if processRuns(pid) {
					t.Fatalf("the utility's child (pid %d) still runs after Run returned", pid)
				}
				size := fileSize(t, written)
				time.Sleep(200 * time.Millisecond)
				if grown := fileSize(t, written); grown != size {
					t.Errorf("the utility's output grew from %d to %d bytes after Run returned", size, grown)
				}
			})
		}
	}
}

// `cubrid server start` and `cubrid heartbeat start` leave daemons on
// purpose; Run neither waits for them nor ends them (#178).
func TestExecCLI_LeavesTheDaemonsOfAStart(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "daemon.pid")
	script := filepath.Join(dir, "cubrid")
	body := fmt.Sprintf("#!/bin/sh\nsleep 30 &\necho $! > %q\necho started\n", pidFile)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{strings.Fields("server start " + dbName), {heartbeatCmd, "start"}} {
		_ = os.Remove(pidFile)
		if out, err := (ExecCLI{Timeout: 10 * time.Second}).Run(context.Background(), script, command...); err != nil {
			t.Fatalf("%v: %v (%s)", command, err, out)
		}
		pid := childPid(t, pidFile)
		if !processRuns(pid) {
			t.Errorf("%v: the daemon it started was ended", command)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// writeScript writes an executable script named name in a new directory.
func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// A utility that ends by itself after waiting for its child returns at once,
// with its output and no error: its group is empty, and nothing is sent to it.
func TestExecCLI_UtilityThatEndsByItselfSucceeds(t *testing.T) {
	script := writeScript(t, "cubrid", "sh -c 'sleep 0.1' &\nwait\necho restored\n")
	start := time.Now()
	out, err := ExecCLI{}.Run(context.Background(), script, "restoredb", "-u", dbName)
	if err != nil {
		t.Fatalf("Run: %v (%s)", err, out)
	}
	if out != "restored\n" {
		t.Errorf("output = %q, want the utility's own output", out)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run took %s for a utility that ended in about 0.1 s", elapsed)
	}
}

// A utility that ends by itself but leaves a process of its group running is
// not a success: the process is ended before Run returns, and Run says so.
// A command that is not `cubrid` runs as before, whatever its first argument.
func TestExecCLI_UtilityThatLeavesAProcessFails(t *testing.T) {
	for _, name := range []string{"cubrid", "notcubrid"} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "left.pid")
			script := writeScript(t, name, fmt.Sprintf("sleep 30 &\necho $! > %q\necho restored\n", pidFile))
			out, err := ExecCLI{Timeout: 10 * time.Second}.Run(context.Background(), script, "restoredb", "-u", dbName)
			pid := childPid(t, pidFile)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if name == "notcubrid" {
				if err != nil || !processRuns(pid) {
					t.Errorf("a command that is not cubrid: err = %v, left process runs = %v; want nil, true", err, processRuns(pid))
				}
				return
			}
			if !errors.Is(err, ErrProcessLeft) {
				t.Errorf("err = %v, want ErrProcessLeft", err)
			}
			if out != "restored\n" {
				t.Errorf("output = %q, want the utility's own output", out)
			}
			if processRuns(pid) {
				t.Errorf("the process the utility left (pid %d) still runs after Run returned", pid)
			}
		})
	}
}

// groupMembers names the processes of a group, for the log of a group that
// has not ended.
func TestGroupMembers(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	members := groupMembers(cmd.Process.Pid)
	want := fmt.Sprintf("%d sleep ", cmd.Process.Pid)
	if len(members) != 1 || !strings.HasPrefix(members[0], want) {
		t.Errorf("members = %q, want one entry starting with %q", members, want)
	}
}
