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
// to pidFile once it runs.
func fakeUtility(t *testing.T) (script, pidFile, written string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "cubrid")
	pidFile = filepath.Join(dir, "child.pid")
	written = filepath.Join(dir, "written")
	body := fmt.Sprintf(`#!/bin/sh
sh -c 'echo $$ > %[1]q.tmp; mv %[1]q.tmp %[1]q; while :; do echo x >> %[2]q; sleep 0.02; done' &
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

// A backupdb, restoredb or restoreslave that is cancelled, by its deadline or
// by its caller (a stop ends every running operation's context), leaves no
// process of the utility behind once Run has returned: the work is done by a
// child that outlives a signal to the direct process alone and goes on writing
// (POC-27), and the caller removes or retries what it wrote right after Run.
func TestExecCLI_CancelledUtilityLeavesNoWriter(t *testing.T) {
	for utility := range strings.FieldsSeq("backupdb restoredb restoreslave") {
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

				_, err := ExecCLI{}.Run(ctx, script, utility, "-u", dbName)
				if err == nil {
					t.Fatal("the cancelled utility reported success")
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
