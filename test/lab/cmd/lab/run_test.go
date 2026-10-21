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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	labCtx      = "lab-ctx"
	labA        = "lab-a"
	contextFlag = "--context"
)

// fakeLab is a kubectl stand-in, a shell script, for one lab cluster. It logs
// every call, one line each: its pid, its process group, and its arguments,
// separated by tabs; it writes the delete options it gets to deleteOptions.
type fakeLab struct {
	dir, kubectl, log, deleteOptions string
}

// newFakeLab writes the script. marker is what the lab marker holds. create
// is how the create of the Lease goes: "ok", "exists" (another run holds the
// lab), or "block" (it stores the Lease, then waits for the file go).
func newFakeLab(t *testing.T, marker, create string) *fakeLab {
	t.Helper()
	dir := t.TempDir()
	f := &fakeLab{
		dir:           dir,
		kubectl:       filepath.Join(dir, "kubectl"),
		log:           filepath.Join(dir, "calls"),
		deleteOptions: filepath.Join(dir, "delete-options"),
	}
	script := fmt.Sprintf(`#!/bin/sh
dir=%q marker=%q create=%q
{
	printf '%%s\t%%s' "$$" "$(cut -d' ' -f5 /proc/$$/stat)"
	for a; do printf '\t%%s' "$a"; done
	echo
} >> "$dir/calls"
case "$*" in
*"config view"*)
	printf 'apiVersion: v1\nkind: Config\ncurrent-context: %%s\n' "$7" ;;
*"get configmap"*)
	printf '%%s' "$marker" ;;
*" create "*)
	cat > /dev/null
	case $create in
	exists)
		echo 'Error from server (AlreadyExists): leases.coordination.k8s.io "campaign" already exists' >&2
		exit 1 ;;
	block)
		: > "$dir/created"
		while ! [ -e "$dir/go" ]; do sleep 0.01; done ;;
	esac
	printf 'uid-1 7' ;;
*"get lease"*)
	printf '{"metadata":{"uid":"uid-0","resourceVersion":"3"},'
	printf '"spec":{"holderIdentity":"other","leaseDurationSeconds":3600,"acquireTime":"2026-10-15T06:00:00.000000Z"}}' ;;
*" delete "*)
	cat > "$dir/delete-options" ;;
*)
	echo "unexpected call: $*" >&2
	exit 1 ;;
esac
`, dir, marker, create)
	if err := os.WriteFile(f.kubectl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

type call struct {
	pid, group string
	args       []string
}

func (f *fakeLab) calls(t *testing.T) []call {
	t.Helper()
	out, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	var calls []call
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		calls = append(calls, call{pid: fields[0], group: fields[1], args: fields[2:]})
	}
	return calls
}

func (f *fakeLab) released() bool {
	_, err := os.Stat(f.deleteOptions)
	return err == nil
}

// signalSource stands for signal.Notify: it records the signals asked for and
// hands the test the channel to send them on.
type signalSource struct {
	asked []os.Signal
	ch    chan chan<- os.Signal
}

func newSignalSource() *signalSource {
	return &signalSource{ch: make(chan chan<- os.Signal, 1)}
}

func (s *signalSource) notify(c chan<- os.Signal, sigs ...os.Signal) {
	s.asked = sigs
	s.ch <- c
}

// send passes sig to the entry point and returns once it has taken it in. The
// channel holds one signal, so the third send returns only after the relay
// has received the second, that is, after it finished with the first.
func (s *signalSource) send(c chan<- os.Signal, sig os.Signal) {
	for range 3 {
		c <- sig
	}
}

// The environment the command starts from: the caller's KUBECONFIG names
// another file, which neither the entry point nor the command may use.
func callerEnviron(t *testing.T) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "KUBECONFIG=" + filepath.Join(t.TempDir(), "caller-kubeconfig")}
}

func labArgs(command ...string) []string {
	return append([]string{"-context", labCtx, "-target", labA, "-duration", "1h", "--"}, command...)
}

// copyOf returns the pinned copy every call after the first names, and checks
// that every call ran in a process group of its own.
func copyOf(t *testing.T, calls []call) string {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("kubectl was not called")
	}
	for _, c := range calls {
		if c.pid != c.group {
			t.Errorf("kubectl %q ran in the process group %s, not one of its own (%s)", c.args, c.group, c.pid)
		}
	}
	first := calls[0].args
	if !slices.Contains(first, "config") || slices.Contains(first, "--kubeconfig") ||
		!slices.Equal(first[len(first)-2:], []string{contextFlag, labCtx}) {
		t.Fatalf("the first call should copy the context from the caller's kubeconfig: %q", first)
	}
	if len(calls) < 2 || len(calls[1].args) < 2 || calls[1].args[0] != "--kubeconfig" {
		t.Fatalf("the calls after the copy do not name it: %q", calls)
	}
	pinned := calls[1].args[1]
	want := []string{"--kubeconfig", pinned, contextFlag, labCtx}
	for _, c := range calls[1:] {
		if len(c.args) < len(want) || !slices.Equal(c.args[:len(want)], want) {
			t.Errorf("call %q does not start with %q", c.args, want)
		}
	}
	return pinned
}

func requireRemoved(t *testing.T, pinned string) {
	t.Helper()
	if _, err := os.Stat(filepath.Dir(pinned)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the pinned copy %s is still there after the run: %v", pinned, err)
	}
}

func TestRunHoldsTheLabThroughThePinnedCopy(t *testing.T) {
	f := newFakeLab(t, labA, "ok")
	signals := newSignalSource()
	seen := filepath.Join(t.TempDir(), "seen")
	command := `printf '%s\n%s\n' "$KUBECONFIG" "$LAB_CONTEXT" > "$0"; cat "$KUBECONFIG" >> "$0"`
	code := run(labArgs("sh", "-c", command, seen), callerEnviron(t), f.kubectl, signals.notify)
	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if !slices.Equal(signals.asked, []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}) {
		t.Errorf("the entry point handles %v, want SIGINT, SIGTERM and SIGHUP", signals.asked)
	}

	calls := f.calls(t)
	pinned := copyOf(t, calls)
	verbs := make([]string, 0, len(calls)-1)
	for _, c := range calls[1:] {
		verbs = append(verbs, c.args[4])
	}
	if !slices.Equal(verbs, []string{"-n", "create", "delete"}) {
		t.Errorf("the calls after the copy were %q, want the check, the create and the delete", calls[1:])
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	want := pinned + "\n" + labCtx + "\napiVersion: v1\nkind: Config\ncurrent-context: " + labCtx + "\n"
	if string(got) != want {
		t.Errorf("the command saw %q, want KUBECONFIG naming the pinned copy %q", got, want)
	}
	options, _ := os.ReadFile(f.deleteOptions)
	if !strings.Contains(string(options), `"uid":"uid-1"`) || !strings.Contains(string(options), `"resourceVersion":"7"`) {
		t.Errorf("the release carried %s, want the uid and resourceVersion of the create", options)
	}
	requireRemoved(t, pinned)
}

func TestRunStopsBeforeTheCommandWhenTheLabIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, marker, create string
		wantCreate           bool
	}{
		{"another lab", "lab-b", "ok", false},
		{"held by another run", labA, "exists", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLab(t, tc.marker, tc.create)
			ran := filepath.Join(t.TempDir(), "ran")
			code := run(labArgs("sh", "-c", `: > "$0"`, ran), callerEnviron(t), f.kubectl, newSignalSource().notify)
			if code != 1 {
				t.Fatalf("run() = %d, want 1", code)
			}
			if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
				t.Error("the command ran although the lab was refused")
			}
			calls := f.calls(t)
			created := slices.ContainsFunc(calls, func(c call) bool { return slices.Contains(c.args, "create") })
			if created != tc.wantCreate {
				t.Errorf("create called = %v, want %v: %q", created, tc.wantCreate, calls)
			}
			if f.released() {
				t.Error("a Lease this run did not take was deleted")
			}
			requireRemoved(t, copyOf(t, calls))
		})
	}
}

// A signal while the command runs reaches it, and the run releases the Lease
// and removes the copy once the command has ended.
func TestRunReleasesTheLabAfterASignalDuringTheCommand(t *testing.T) {
	f := newFakeLab(t, labA, "ok")
	signals := newSignalSource()
	ready := filepath.Join(t.TempDir(), "ready")
	done := make(chan int, 1)
	go func() {
		done <- run(labArgs("sh", "-c", `trap 'exit 9' TERM; : > "$0"; while :; do sleep 0.01; done`, ready),
			callerEnviron(t), f.kubectl, signals.notify)
	}()
	c := <-signals.ch
	waitFor(t, ready)
	c <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 9 {
			t.Fatalf("run() = %d, want the command's exit code 9", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end after the signal")
	}
	if !f.released() {
		t.Error("the Lease was not released")
	}
	requireRemoved(t, copyOf(t, f.calls(t)))
}

// A signal that arrives while the Lease is being taken stops the run before
// the command, and the Lease is still released.
func TestRunReleasesTheLabAfterASignalBeforeTheCommand(t *testing.T) {
	f := newFakeLab(t, labA, "block")
	signals := newSignalSource()
	ran := filepath.Join(t.TempDir(), "ran")
	done := make(chan int, 1)
	go func() {
		done <- run(labArgs("sh", "-c", `: > "$0"`, ran), callerEnviron(t), f.kubectl, signals.notify)
	}()
	c := <-signals.ch
	waitFor(t, filepath.Join(f.dir, "created"))
	signals.send(c, syscall.SIGTERM)
	if err := os.WriteFile(filepath.Join(f.dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 128+int(syscall.SIGTERM) {
			t.Fatalf("run() = %d, want %d", code, 128+int(syscall.SIGTERM))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end after the signal")
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Error("the command started after the signal")
	}
	if !f.released() {
		t.Error("the Lease taken before the signal was not released")
	}
	requireRemoved(t, copyOf(t, f.calls(t)))
}

// A `sh -c` without a trap ends at once on a signal, while what it started
// may still be cleaning up, with kubectl: the Lease and the copy stay until
// all of it has ended.
func TestRunHoldsTheLabUntilEverythingTheCommandStartedHasEnded(t *testing.T) {
	f := newFakeLab(t, labA, "ok")
	signals := newSignalSource()
	dir := t.TempDir()
	ready, seen := filepath.Join(dir, "ready"), filepath.Join(dir, "seen")
	// The child's cleanup records, after a while, whether the Lease and the
	// copy are still there.
	child := `trap 'sleep 0.5
if [ -e "$2" ]; then echo released; elif [ -e "$KUBECONFIG" ]; then echo held; else echo removed; fi > "$1"
exit 0' TERM
: > "$0"; while :; do sleep 0.01; done`
	done := make(chan int, 1)
	go func() {
		done <- run(labArgs("sh", "-c", `sh -c "$3" "$0" "$1" "$2" && echo next`, ready, seen, f.deleteOptions, child),
			callerEnviron(t), f.kubectl, signals.notify)
	}()
	c := <-signals.ch
	waitFor(t, ready)
	c <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 128+int(syscall.SIGTERM) {
			t.Fatalf("run() = %d, want the shell's %d", code, 128+int(syscall.SIGTERM))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end after the signal")
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the run ended before the command's child finished its cleanup: %v", err)
	}
	if strings.TrimSpace(string(got)) != "held" {
		t.Fatalf("during the child's cleanup the lab was %q, want held", got)
	}
	if !f.released() {
		t.Error("the Lease was not released")
	}
	requireRemoved(t, copyOf(t, f.calls(t)))
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
