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
	"strconv"
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
// separated by tabs; it writes the delete options it takes to deleteOptions,
// taking only those that carry the uid and the current resourceVersion,
// and appends every renewal it accepts to renewals, one per line.
type fakeLab struct {
	dir, kubectl, log, deleteOptions, renewals string
}

// newFakeLab writes the script. marker is what the lab marker holds. create
// is how the create of the Lease goes: "ok", "exists" (another run holds the
// lab), or "block" (it stores the Lease, then waits for the file go). renew is
// how a renewal goes: "ok", which takes one that carries the uid and the
// current resourceVersion and raises the version, "flaky", which also stores
// the first one but fails it, as a timeout after the API server stored it
// would, "conflict" (someone else wrote the Lease), "down" (the API server
// cannot be reached), "stall", which takes the first one only after 30s and
// any later one at once, or "hang" (kubectl hangs, its children holding its
// output, one of them in a session of its own). A get of the Lease returns
// the last renewal stored, or else another holder's Lease.
func newFakeLab(t *testing.T, marker, create, renew string) *fakeLab {
	t.Helper()
	dir := t.TempDir()
	f := &fakeLab{
		dir:           dir,
		kubectl:       filepath.Join(dir, "kubectl"),
		log:           filepath.Join(dir, "calls"),
		deleteOptions: filepath.Join(dir, "delete-options"),
		renewals:      filepath.Join(dir, "renewals"),
	}
	script := fmt.Sprintf(`#!/bin/sh
dir=%q marker=%q create=%q renew=%q
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
	echo 7 > "$dir/version"
	printf 'uid-1 7' ;;
*" replace "*)
	body=$(cat)
	v=$(cat "$dir/version")
	case $renew in
	down)
		echo 'Unable to connect to the server: dial tcp 10.0.0.1:6443: i/o timeout' >&2
		exit 1 ;;
	stall)
		if ! [ -e "$dir/stalled" ]; then
			: > "$dir/stalled"
			sleep 30
		fi ;;
	hang)
		echo $$ > "$dir/hung"
		setsid sleep 30 &
		echo $! >> "$dir/hung"
		sleep 30 &
		sleep 30
		exit 1 ;;
	esac
	case $renew:$body in
	conflict:*) ;;
	*'"resourceVersion":"'"$v"'"'*'"uid":"uid-1"'*)
		ok=1 ;;
	esac
	case $ok in
	1) ;;
	*)
		printf '%%s %%s\n' 'Error from server (Conflict): Operation cannot be fulfilled on' \
			'leases.coordination.k8s.io "campaign": the object has been modified' >&2
		exit 1 ;;
	esac
	v=$((v+1))
	echo "$v" > "$dir/version"
	printf '%%s\n' "$body" >> "$dir/renewals"
	printf '%%s' "$body" | sed 's/"resourceVersion":"[0-9]*"/"resourceVersion":"'"$v"'"/' > "$dir/lease"
	if [ "$renew" = flaky ] && ! [ -e "$dir/flaked" ]; then
		: > "$dir/flaked"
		echo 'Unable to connect to the server: net/http: request canceled (Client.Timeout exceeded)' >&2
		exit 1
	fi
	printf '{"metadata":{"uid":"uid-1","resourceVersion":"%%s"}}' "$v" ;;
*"get lease"*)
	if [ -e "$dir/lease" ]; then
		cat "$dir/lease"
		exit
	fi
	printf '{"metadata":{"uid":"uid-0","resourceVersion":"3"},'
	printf '"spec":{"holderIdentity":"other","leaseDurationSeconds":3600,"acquireTime":"2026-10-15T06:00:00.000000Z"}}' ;;
*" delete "*)
	options=$(cat)
	v=$(cat "$dir/version")
	case $options in
	*'"resourceVersion":"'"$v"'"'*'"uid":"uid-1"'*) ;;
	*)
		printf '%%s %%s\n' 'Error from server (Conflict): Operation cannot be fulfilled on' \
			'leases.coordination.k8s.io "campaign": Precondition failed' >&2
		exit 1 ;;
	esac
	printf '%%s' "$options" > "$dir/delete-options" ;;
*)
	echo "unexpected call: $*" >&2
	exit 1 ;;
esac
`, dir, marker, create, renew)
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
	return labArgsFor(time.Hour, command...)
}

// labArgsFor runs the command with a Lease of the given duration.
func labArgsFor(duration time.Duration, command ...string) []string {
	return append([]string{"-context", labCtx, "-target", labA, "-duration", duration.String(), "--"}, command...)
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
	f := newFakeLab(t, labA, "ok", "ok")
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
			f := newFakeLab(t, tc.marker, tc.create, "ok")
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
	f := newFakeLab(t, labA, "ok", "ok")
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
	f := newFakeLab(t, labA, "block", "ok")
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
	f := newFakeLab(t, labA, "ok", "ok")
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

// A run renews its Lease while the command runs, each renewal carrying the
// resourceVersion the last one returned, and releases the Lease with the
// latest one.
func TestRunRenewsTheLeaseWhileTheCommandRuns(t *testing.T) {
	f := newFakeLab(t, labA, "ok", "ok")
	code := run(labArgsFor(3*time.Second, "sleep", "1.5"), callerEnviron(t), f.kubectl, newSignalSource().notify)
	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	out, _ := os.ReadFile(f.renewals)
	renewals := strings.Count(string(out), "\n")
	if renewals < 2 {
		t.Fatalf("the Lease was renewed %d times during a command that outlasted it, want at least 2", renewals)
	}
	version, _ := os.ReadFile(filepath.Join(f.dir, "version"))
	options, _ := os.ReadFile(f.deleteOptions)
	want := `"resourceVersion":"` + strings.TrimSpace(string(version)) + `"`
	if !strings.Contains(string(options), want) || !strings.Contains(string(options), `"uid":"uid-1"`) {
		t.Fatalf("the release carried %s, want the uid and the version of the last renewal (%s)", options, want)
	}
	requireRemoved(t, copyOf(t, f.calls(t)))
}

// A renewal the API server stored although kubectl failed is not a loss: the
// run finds it in the Lease, goes on renewing from it, and releases the Lease.
func TestRunKeepsALeaseWhoseRenewalWasStoredThoughItFailed(t *testing.T) {
	f := newFakeLab(t, labA, "ok", "flaky")
	code := run(labArgsFor(3*time.Second, "sleep", "1.5"), callerEnviron(t), f.kubectl, newSignalSource().notify)
	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "flaked")); err != nil {
		t.Fatal("no renewal was stored and failed")
	}
	version, _ := os.ReadFile(filepath.Join(f.dir, "version"))
	options, _ := os.ReadFile(f.deleteOptions)
	if want := `"resourceVersion":"` + strings.TrimSpace(string(version)) + `"`; !strings.Contains(string(options), want) {
		t.Fatalf("the release carried %q, want %s", options, want)
	}
}

// A renewal that stalls is given up after a third of the Lease's duration,
// so the next one, which succeeds, comes in time and the run keeps the lab.
func TestRunKeepsTheLeaseAfterARenewalThatStalledOnce(t *testing.T) {
	f := newFakeLab(t, labA, "ok", "stall")
	code := run(labArgsFor(3*time.Second, "sleep", "4"), callerEnviron(t), f.kubectl, newSignalSource().notify)
	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "stalled")); err != nil {
		t.Fatal("no renewal stalled")
	}
	if !f.released() {
		t.Error("the Lease was not released")
	}
}

// A kubectl that hangs, with children that hold its output, does not hold
// the run past the Lease's end: the run stops the command once the Lease's
// duration has passed without a renewal.
func TestRunStopsTheCommandWhenARenewalHangs(t *testing.T) {
	f := newFakeLab(t, labA, "ok", "hang")
	t.Cleanup(func() {
		got, _ := os.ReadFile(filepath.Join(f.dir, "hung"))
		if pids := strings.Fields(string(got)); len(pids) == 2 {
			group, _ := strconv.Atoi(pids[0])
			stray, _ := strconv.Atoi(pids[1])
			_ = syscall.Kill(-group, syscall.SIGKILL)
			_ = syscall.Kill(stray, syscall.SIGKILL)
		}
	})
	leader := filepath.Join(t.TempDir(), "leader")
	done := make(chan int, 1)
	go func() {
		done <- run(labArgsFor(3*time.Second, "sh", "-c", `echo $$ > "$0"; sleep 30`, leader),
			callerEnviron(t), f.kubectl, newSignalSource().notify)
	}()
	killGroupOf(t, leader)
	select {
	case code := <-done:
		if code != exitLost {
			t.Fatalf("run() = %d, want %d", code, exitLost)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command still ran after the Lease's duration passed during a renewal that hung")
	}
	if !f.released() {
		t.Error("the release was not sent")
	}
}

// A run whose Lease was written by someone else no longer holds the lab: it
// stops the command's group, with SIGKILL when SIGTERM is not enough, exits
// non-zero, and deletes nothing.
func TestRunStopsTheCommandWhenTheLeaseIsLost(t *testing.T) {
	f := newFakeLab(t, labA, "ok", "conflict")
	leader := filepath.Join(t.TempDir(), "leader")
	done := make(chan int, 1)
	go func() {
		command := `echo $$ > "$0"; trap '' TERM; while :; do sleep 0.05; done`
		done <- run(labArgsFor(3*time.Second, "sh", "-c", command, leader), callerEnviron(t), f.kubectl,
			newSignalSource().notify)
	}()
	killGroupOf(t, leader)
	select {
	case code := <-done:
		if code != exitLost {
			t.Fatalf("run() = %d, want %d", code, exitLost)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command still ran after the Lease was lost")
	}
	if f.released() {
		t.Error("a Lease that was no longer this run's was deleted")
	}
	requireRemoved(t, copyOf(t, f.calls(t)))
}

// A Lease that no renewal reached within its duration may have ended, so the
// run no longer holds the lab; it may also still be as the run wrote it, so
// the release is sent, with the preconditions that keep it from deleting
// anyone else's.
func TestRunStopsTheCommandWhenNoRenewalSucceedsInTime(t *testing.T) {
	f := newFakeLab(t, labA, "ok", "down")
	leader := filepath.Join(t.TempDir(), "leader")
	done := make(chan int, 1)
	go func() {
		done <- run(labArgsFor(3*time.Second, "sh", "-c", `echo $$ > "$0"; sleep 30`, leader),
			callerEnviron(t), f.kubectl, newSignalSource().notify)
	}()
	killGroupOf(t, leader)
	select {
	case code := <-done:
		if code != exitLost {
			t.Fatalf("run() = %d, want %d", code, exitLost)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command still ran after the Lease's duration passed without a renewal")
	}
	options, _ := os.ReadFile(f.deleteOptions)
	if !strings.Contains(string(options), `"uid":"uid-1"`) || !strings.Contains(string(options), `"resourceVersion":"7"`) {
		t.Errorf("the release carried %q, want the uid and resourceVersion of the create", options)
	}
}

// While the run waits for a job the command left in the background, which a
// non-interactive shell starts with SIGINT ignored, a repeated signal ends
// the wait: the second sends SIGTERM to the group, the third SIGKILL. The
// Lease is released after.
func TestARepeatedSignalEndsTheWaitForBackgroundJobs(t *testing.T) {
	for _, tc := range []struct {
		name, job string
		signals   int
	}{
		{"SIGINT ignored", `sleep 30`, 2},
		{"SIGINT and SIGTERM ignored", `trap '' TERM; sleep 30`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLab(t, labA, "ok", "ok")
			signals := newSignalSource()
			leader := filepath.Join(t.TempDir(), "leader")
			done := make(chan int, 1)
			go func() {
				done <- run(labArgs("sh", "-c", `(`+tc.job+`) & echo $$ > "$0"; exit 0`, leader),
					callerEnviron(t), f.kubectl, signals.notify)
			}()
			c := <-signals.ch
			pid := killGroupOf(t, leader)
			waitUntil(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
			for i := 1; i < tc.signals; i++ {
				c <- syscall.SIGINT
				select {
				case <-done:
					t.Fatalf("signal %d ended the wait, want signal %d to", i, tc.signals)
				case <-time.After(500 * time.Millisecond):
				}
			}
			c <- syscall.SIGINT
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("run() = %d, want the command's 0", code)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("signal %d did not end the wait", tc.signals)
			}
			if !f.released() {
				t.Error("the Lease was not released")
			}
		})
	}
}

// killGroupOf waits for the command to write its pid to path, and returns
// it; whatever the test ends with, nothing of its process group is left.
func killGroupOf(t *testing.T, path string) int {
	t.Helper()
	waitUntil(t, func() bool { got, _ := os.ReadFile(path); return strings.HasSuffix(string(got), "\n") })
	got, _ := os.ReadFile(path)
	pid, err := strconv.Atoi(strings.TrimSpace(string(got)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}
