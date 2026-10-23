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

// Command lab runs one command against the shared lab, the entry point
// `make test-lab` uses:
//
//	lab -context <kubectl context> -target <lab> -- <command> [args]
//
// It copies that one context into a private kubeconfig first, and from then
// on uses only the copy: it checks that the context leads to the named lab,
// takes the lab's campaign Lease, runs the command with KUBECONFIG pointing at
// the copy, and releases the Lease when the command ends. Switching or
// redefining contexts during the run cannot redirect the run or the release.
//
// The Lease lasts -duration, and is renewed every sixth of that until it is
// released. When it is lost, because it was removed or someone else wrote
// it, or because no renewal succeeded within its duration of the last one,
// the run no longer holds the lab: it stops the command's process group with
// SIGTERM, and SIGKILL after killGrace, and exits with exitLost.
//
// SIGINT, SIGTERM and SIGHUP are handled from the start. One that arrives
// before the command starts stops the run there, after releasing the Lease if
// it was taken. During the command, the first one is passed on to the
// command's process group, so a `sh -c` command and everything it started
// get it; the second sends SIGTERM to the group, and any later one SIGKILL.
// The Lease is released, and the copy removed, only when no process is left
// in that group: the command, what it started, and what it left in the
// background.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/lab"
)

// callTimeout bounds each kubectl call of the entry point itself, one by one:
// after a create that timed out, the read that tells whether it was stored
// still gets its own time.
const callTimeout = time.Minute

// kubectlWaitDelay is how long a kubectl call that was cancelled may take to
// release its output.
const kubectlWaitDelay = time.Second

// exitLost is the exit code of a run that lost its Lease, whatever the
// command's: the run stopped the command.
const exitLost = 1

// killGrace is how long the command's process group has to end on SIGTERM
// after the Lease is lost, before it gets SIGKILL.
const killGrace = 2 * time.Second

// handled are the signals the entry point owns for its whole run.
var handled = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), "kubectl", signal.Notify))
}

// run is the whole entry point: args are its arguments, environ the
// environment the command starts from, kubectlPath the kubectl it runs, and
// notify registers the channel the handled signals arrive on.
func run(args, environ []string, kubectlPath string, notify func(chan<- os.Signal, ...os.Signal)) int {
	// Installed before anything else, so that no signal can end the process
	// between taking the Lease and releasing it, or before the copy of the
	// kubeconfig is removed.
	signals := make(chan os.Signal, 1)
	notify(signals, handled...)
	defer signal.Stop(signals)
	relay := newRelay(signals)
	kubectl := kubectlAt(kubectlPath)

	var target lab.Target
	flags := flag.NewFlagSet("lab", flag.ContinueOnError)
	flags.StringVar(&target.Context, "context", "", "kubectl context of the lab (required)")
	flags.StringVar(&target.Name, "target", "", "name of the lab the context must lead to (required)")
	duration := flags.Duration("duration", time.Minute,
		"how long the campaign Lease lasts without a renewal, in whole seconds; it is renewed every sixth of that")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	command := flags.Args()
	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "lab: no command to run; give it after --")
		return 2
	}

	kubeconfig, cleanup, err := pinnedKubeconfig(kubectl, target.Context)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}
	defer cleanup()
	target.Kubeconfig = kubeconfig
	if code, stop := relay.stopped(); stop {
		return code
	}

	ctx := context.Background()
	if err := target.Check(ctx, kubectl); err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}
	if code, stop := relay.stopped(); stop {
		return code
	}

	acquired := time.Now()
	lock, err := target.Acquire(ctx, kubectl, holder(), *duration, acquired)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "lab: %q (context %q) is held for this run\n", target.Name, target.Context)
	stopRenewing, lost := make(chan struct{}), make(chan error, 1)
	go func() {
		lost <- renew(kubectl, lock, duration.Truncate(time.Second), acquired, stopRenewing, relay.lose)
	}()

	code, stop := relay.stopped()
	if !stop {
		code = relay.run(command, append(environ,
			"KUBECONFIG="+kubeconfig,
			"LAB_CONTEXT="+target.Context,
			"LAB_TARGET="+target.Name,
		))
	}
	close(stopRenewing)
	if err := <-lost; err != nil {
		code = exitLost
		if errors.Is(err, lab.ErrLost) {
			// Not this run's any more: there is nothing of it to delete.
			return code
		}
		// Not renewed in time, but possibly still as this run wrote it: the
		// release deletes it only then.
	}

	if err := lock.Release(ctx, kubectl); err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		if code == 0 {
			code = 1
		}
	} else {
		fmt.Fprintf(os.Stderr, "lab: %q released\n", target.Name)
	}
	return code
}

// renew renews the Lease every sixth of its duration, from acquired on, until
// stop is closed, and then returns nil, cancelling a renewal still in flight.
// When the Lease is lost, it says so, calls lose and returns why: an error
// that wraps lab.ErrLost when the Lease is gone or someone else wrote it, any
// other when no renewal succeeded within its duration of the last one. The
// end of that duration is a timer of its own, so a renewal that hangs does
// not carry the run past it; and each renewal is given up after a third of
// the duration, so that one slow call leaves time for the next.
func renew(
	k lab.Kubectl, lock *lab.Lock, duration time.Duration, acquired time.Time, stop <-chan struct{}, lose func(),
) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticker := time.NewTicker(duration / 6)
	defer ticker.Stop()
	end := time.NewTimer(time.Until(acquired.Add(duration)))
	defer end.Stop()
	type renewal struct {
		at  time.Time
		err error
	}
	// Room for the one renewal in flight, which may end after renew returns.
	renewed := make(chan renewal, 1)
	inFlight := false
	var err error
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			if !inFlight {
				inFlight = true
				go func(now time.Time) {
					call, cancel := context.WithTimeout(ctx, duration/3)
					defer cancel()
					renewed <- renewal{now, lock.Renew(call, k, now)}
				}(time.Now())
			}
			continue
		case r := <-renewed:
			inFlight = false
			switch {
			case r.err == nil:
				end.Reset(time.Until(r.at.Add(duration)))
				continue
			case !errors.Is(r.err, lab.ErrLost):
				fmt.Fprintf(os.Stderr, "lab: %v; retrying\n", r.err)
				continue
			}
			err = r.err
		case <-end.C:
			err = fmt.Errorf("the campaign Lease was not renewed within %s", duration)
		}
		fmt.Fprintf(os.Stderr, "lab: %v; the run no longer holds the lab, stopping the command\n", err)
		lose()
		return err
	}
}

// relay owns the signals of the entry point for its whole life: before the
// command runs it records the first one, so that the run stops at the next
// step; while the command runs it passes the first one on to the command's
// process group, and turns later ones into SIGTERM, then SIGKILL.
type relay struct {
	mu       sync.Mutex
	received os.Signal
	group    int // the command's process group while it runs, else 0
	relayed  int // the signals that arrived while it ran
	lost     bool
}

func newRelay(signals <-chan os.Signal) *relay {
	r := &relay{}
	go func() {
		for sig := range signals {
			r.mu.Lock()
			if r.received == nil {
				r.received = sig
			}
			group := r.group
			if group > 0 {
				r.relayed++
			}
			n := r.relayed
			r.mu.Unlock()
			if group == 0 {
				continue
			}
			send := sig.(syscall.Signal)
			switch n {
			case 1:
				fmt.Fprintf(os.Stderr, "lab: passing %v on to process group %d; "+
					"a second signal sends it SIGTERM, a third SIGKILL\n", sig, group)
			case 2:
				send = syscall.SIGTERM
				fmt.Fprintf(os.Stderr, "lab: %v again; sending SIGTERM to process group %d\n", sig, group)
			default:
				send = syscall.SIGKILL
				fmt.Fprintf(os.Stderr, "lab: %v again; sending SIGKILL to process group %d\n", sig, group)
			}
			_ = syscall.Kill(-group, send)
		}
	}()
	return r
}

// lose records that the run no longer holds the lab, so that the command does
// not start, and stops the command's process group if it runs: SIGTERM now,
// SIGKILL after killGrace if the group is still there.
func (r *relay) lose() {
	r.mu.Lock()
	r.lost = true
	group := r.group
	r.mu.Unlock()
	if group == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "lab: sending SIGTERM to process group %d\n", group)
	_ = syscall.Kill(-group, syscall.SIGTERM)
	time.AfterFunc(killGrace, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.group == group {
			fmt.Fprintf(os.Stderr, "lab: sending SIGKILL to process group %d\n", group)
			_ = syscall.Kill(-group, syscall.SIGKILL)
		}
	})
}

// stopped reports whether a signal has arrived, or the Lease is lost, and the
// exit code for it.
func (r *relay) stopped() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lost {
		return exitLost, true
	}
	if r.received == nil {
		return 0, false
	}
	fmt.Fprintf(os.Stderr, "lab: stopped by %v before the command started\n", r.received)
	return signalExit(r.received.(syscall.Signal)), true
}

// run runs the command to its end, in a process group of its own, and
// returns its exit code.
//
// The command does not share the terminal's foreground process group, so a
// Ctrl-C reaches it once, from the relay, and not twice (from the terminal
// and again from the relay), which `go test` and Ginkgo would take as "skip
// the cleanup". Its standard input is /dev/null: from outside the foreground
// group, a read of the terminal would stop it (SIGTTIN) with the lab held.
//
// The command has ended only when its whole process group has: a `sh -c`
// without a trap ends at once on a signal while the commands it started may
// still be cleaning up, and a job it left in the background still runs.
// Until then, signals still reach the group, and a repeated one ends it. A
// process that left the group (setsid, setpgid) is not waited for.
func (r *relay) run(command []string, env []string) int {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	r.mu.Lock()
	if r.lost {
		r.mu.Unlock()
		return exitLost
	}
	if r.received != nil {
		// A signal arrived after the last check: do not start.
		sig := r.received
		r.mu.Unlock()
		return signalExit(sig.(syscall.Signal))
	}
	err := cmd.Start()
	if err == nil {
		r.group = cmd.Process.Pid
	}
	r.mu.Unlock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}

	err = cmd.Wait()
	group := cmd.Process.Pid
	for told := false; groupExists(group); time.Sleep(50 * time.Millisecond) {
		if !told {
			fmt.Fprintf(os.Stderr, "lab: the command ended; waiting for the processes it started (process group %d); "+
				"a second signal sends them SIGTERM, a third SIGKILL\n", group)
			told = true
		}
	}
	r.mu.Lock()
	r.group = 0
	r.mu.Unlock()
	return exitCode(err)
}

// groupExists reports whether any process is left in the process group.
func groupExists(group int) bool {
	return !errors.Is(syscall.Kill(-group, 0), syscall.ESRCH)
}

// exitCode maps the end of a command to an exit code the way a shell does: a
// command killed by a signal exits with 128 plus its number.
func exitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return signalExit(status.Signal())
		}
		if code := exit.ExitCode(); code > 0 {
			return code
		}
		return 1
	default:
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}
}

func signalExit(sig syscall.Signal) int {
	return 128 + int(sig)
}

// pinnedKubeconfig writes the named context, and only it, with its
// credentials, to a file only the user can read, and returns its path and a
// function that removes it. The credentials go from kubectl's output to the
// file and are never shown: --raw keeps embedded tokens, certificates and
// keys, which kubectl otherwise redacts, and --flatten embeds those that the
// kubeconfig names as files.
func pinnedKubeconfig(k lab.Kubectl, name string) (string, func(), error) {
	if name == "" {
		return "", nil, errors.New("a lab run needs both a kubectl context and the name of the lab (LAB_CONTEXT, LAB_TARGET)")
	}
	out, err := k(context.Background(), "", "config", "view", "--minify", "--flatten", "--raw", "--context", name)
	if err != nil {
		return "", nil, fmt.Errorf("copying the context %q: %w", name, err)
	}
	dir, err := os.MkdirTemp("", "cubrid-lab-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// holder identifies this run in the Lease: who started it, where, and when.
func holder() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return lab.Holder(name, host, os.Getpid(), time.Now())
}

// kubectlAt runs the kubectl at path for the entry point's own calls.
func kubectlAt(path string) lab.Kubectl {
	return func(ctx context.Context, stdin string, args ...string) (string, error) {
		return runKubectl(ctx, path, stdin, args...)
	}
}

func runKubectl(ctx context.Context, path, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	// Out of the terminal's foreground process group: a Ctrl-C does not cut a
	// call short, for example a create the API server has already stored; the
	// relay stops the run after the call instead.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// When ctx ends, the call's whole group is killed, and what still holds
	// its output after kubectlWaitDelay, having left the group, is cut off.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = kubectlWaitDelay
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.String(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}
