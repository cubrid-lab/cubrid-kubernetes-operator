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
