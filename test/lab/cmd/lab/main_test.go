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

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExitCodeFollowsTheShell(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   int
	}{
		{"success", "exit 0", 0},
		{"failure", "exit 3", 3},
		{"killed by SIGTERM", "kill -TERM $$", 128 + int(syscall.SIGTERM)},
		{"killed by SIGKILL", "kill -KILL $$", 128 + int(syscall.SIGKILL)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(exec.Command("sh", "-c", tc.script).Run()); got != tc.want {
				t.Fatalf("exitCode() = %d, want %d", got, tc.want)
			}
		})
	}
	if got := exitCode(errors.New("exec: not started")); got != 1 {
		t.Fatalf("exitCode() of a start error = %d, want 1", got)
	}
}

// The copy keeps the credentials kubectl would otherwise redact, is readable
// by the user only, and is removed by the cleanup.
func TestPinnedKubeconfigKeepsTheCredentialsPrivately(t *testing.T) {
	const config = "apiVersion: v1\nusers:\n- name: u\n  user:\n    token: secret-token\n" +
		"    client-key-data: a2V5\n"
	var args []string
	k := func(_ context.Context, _ string, a ...string) (string, error) {
		args = a
		return config, nil
	}
	path, cleanup, err := pinnedKubeconfig(k, "lab-ctx")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--minify", "--flatten", "--raw"} {
		if !slices.Contains(args, flag) {
			t.Errorf("kubectl config view was called without %s: %q", flag, args)
		}
	}
	if !slices.Equal(args[len(args)-2:], []string{"--context", "lab-ctx"}) {
		t.Errorf("kubectl config view did not name the context: %q", args)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the copy has mode %o, want 600", mode)
	}
	if got, _ := os.ReadFile(path); string(got) != config {
		t.Errorf("the copy is %q, want kubectl's output unchanged", got)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy's directory is still there after the cleanup: %v", err)
	}

	if _, _, err := pinnedKubeconfig(k, ""); err == nil || !strings.Contains(err.Error(), "LAB_CONTEXT") {
		t.Errorf("pinnedKubeconfig() without a context = %v", err)
	}
}

// A signal that arrives before the command starts stops the run, with the
// shell's exit code for it.
func TestASignalBeforeTheCommandStopsTheRun(t *testing.T) {
	signals := make(chan os.Signal, 1)
	r := newRelay(signals)
	signals <- syscall.SIGHUP
	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, stop := r.stopped(); stop {
			if code != 128+int(syscall.SIGHUP) {
				t.Fatalf("stopped() = %d, want %d", code, 128+int(syscall.SIGHUP))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the signal was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	if code := r.run([]string{"sh", "-c", `: > "$0"`, marker}, os.Environ()); code != 128+int(syscall.SIGHUP) {
		t.Fatalf("run() = %d after a signal, want %d", code, 128+int(syscall.SIGHUP))
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the command started after the run was stopped")
	}
}

// A command that ends while a job it started in the background still runs
// is over only when that job is.
func TestTheCommandEndsWithItsBackgroundJobs(t *testing.T) {
	done := filepath.Join(t.TempDir(), "done")
	r := newRelay(make(chan os.Signal))
	if code := r.run([]string{"sh", "-c", `(sleep 0.5; : > "$0") & exit 0`, done}, os.Environ()); code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("run() returned while the command's background job still ran: %v", err)
	}
}

// After the command itself has ended, signals still reach what it started.
func TestASignalReachesWhatOutlivesTheCommand(t *testing.T) {
	dir := t.TempDir()
	outer, log := filepath.Join(dir, "outer"), filepath.Join(dir, "log")
	// The outer shell has no trap and ends on the first SIGTERM; its child
	// ends on the second only.
	child := `trap 'n=$((n+1)); echo $n >> "$1"; [ $n -ge 2 ] && exit 0' TERM
while :; do sleep 0.01; done`
	signals := make(chan os.Signal, 1)
	r := newRelay(signals)
	done := make(chan int, 1)
	go func() {
		done <- r.run([]string{"sh", "-c", `echo $$ > "$0"; sh -c "$2" "$0" "$1" && echo next`, outer, log, child},
			os.Environ())
	}()
	waitUntil(t, func() bool { _, err := os.Stat(outer); return err == nil })
	time.Sleep(100 * time.Millisecond) // let the outer shell start its child
	signals <- syscall.SIGTERM
	got, _ := os.ReadFile(outer)
	pid, err := strconv.Atoi(strings.TrimSpace(string(got)))
	if err != nil {
		t.Fatal(err)
	}
	// The outer shell leads the command's process group: whatever the test
	// ends with, nothing of it is left running.
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	// Once the outer shell has been waited for, only its child is left.
	waitUntil(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	signals <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 128+int(syscall.SIGTERM) {
			t.Fatalf("run() = %d, want the outer shell's %d", code, 128+int(syscall.SIGTERM))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second signal did not reach the child left after the command ended")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The command reads /dev/null, not the entry point's standard input: outside
// the terminal's foreground group, a read of the terminal would stop it.
func TestTheCommandReadsNothing(t *testing.T) {
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	saved := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = saved; _ = stdin.Close() }()

	seen := filepath.Join(t.TempDir(), "stdin")
	r := newRelay(make(chan os.Signal))
	if code := r.run([]string{"sh", "-c", `readlink /proc/$$/fd/0 > "$0"`, seen}, os.Environ()); code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if got, _ := os.ReadFile(seen); strings.TrimSpace(string(got)) != os.DevNull {
		t.Fatalf("the command's standard input is %q, want %s", got, os.DevNull)
	}
}

// During the command, a signal reaches its whole process group once: a
// `sh -c` command and what it started get it, and nothing gets it twice.
func TestASignalReachesTheCommandOnce(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			log, ready := filepath.Join(dir, "log"), filepath.Join(dir, "ready")
			// The shell and the command it runs in the foreground, as in
			// `a && b`, each log every delivery: a signal to the shell alone
			// leaves the child's line missing, a second delivery adds lines.
			// (A command a non-interactive shell starts with `&` ignores
			// SIGINT, as POSIX requires.)
			script := `trap 'echo sh >> "$0"' INT TERM HUP
sh -c 'trap "echo child >> \"$0\"; exit 0" INT TERM HUP; : > "$1"; while :; do sleep 0.05; done' "$0" "$1"
exit 7`
			signals := make(chan os.Signal, 2)
			r := newRelay(signals)
			done := make(chan int, 1)
			go func() { done <- r.run([]string{"sh", "-c", script, log, ready}, os.Environ()) }()

			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the command did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			signals <- sig

			select {
			case code := <-done:
				if code != 7 {
					t.Fatalf("run() = %d, want the command's exit code 7", code)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the command did not end after the signal")
			}
			got, _ := os.ReadFile(log)
			lines := strings.Fields(string(got))
			slices.Sort(lines)
			if !slices.Equal(lines, []string{"child", "sh"}) {
				t.Fatalf("the signal was delivered as %q, want once to the shell and once to its child", lines)
			}
		})
	}
}
