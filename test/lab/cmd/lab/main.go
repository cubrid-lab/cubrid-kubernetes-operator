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
// SIGINT, SIGTERM and SIGHUP are handled from the start. One that arrives
// before the command starts stops the run there, after releasing the Lease if
// it was taken. During the command, each one is passed on once to the
// command's process group, so a `sh -c` command and everything it started
// get it, and the Lease is released when the command has ended.
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

func main() {
	os.Exit(run())
}

func run() int {
	// Installed before anything else, so that no signal can end the process
	// between taking the Lease and releasing it, or before the copy of the
	// kubeconfig is removed.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	relay := newRelay(signals)

	var target lab.Target
	flag.StringVar(&target.Context, "context", "", "kubectl context of the lab (required)")
	flag.StringVar(&target.Name, "target", "", "name of the lab the context must lead to (required)")
	duration := flag.Duration("duration", 2*time.Hour, "how long the campaign is expected to take at most")
	flag.Parse()
	command := flag.Args()
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

	lock, err := target.Acquire(ctx, kubectl, holder(), *duration, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "lab: %q (context %q) is held for this run\n", target.Name, target.Context)

	code, stop := relay.stopped()
	if !stop {
		code = relay.run(command, append(os.Environ(),
			"KUBECONFIG="+kubeconfig,
			"LAB_CONTEXT="+target.Context,
			"LAB_TARGET="+target.Name,
		))
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

// relay owns the signals of the entry point for its whole life: before the
// command runs it records the first one, so that the run stops at the next
// step; while the command runs it passes each one on to the command's process
// group.
type relay struct {
	mu       sync.Mutex
	received os.Signal
	group    int // the command's process group while it runs, else 0
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
			r.mu.Unlock()
			if group > 0 {
				_ = syscall.Kill(-group, sig.(syscall.Signal))
			}
		}
	}()
	return r
}

// stopped reports whether a signal has arrived, and the exit code for it.
func (r *relay) stopped() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
// the cleanup". A command that reads the terminal is stopped (SIGTTIN); lab
// commands are not interactive.
func (r *relay) run(command []string, env []string) int {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	r.mu.Lock()
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
	r.mu.Lock()
	r.group = 0
	r.mu.Unlock()
	return exitCode(err)
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

func kubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	// Out of the terminal's foreground process group: a Ctrl-C does not cut a
	// call short, for example a create the API server has already stored; the
	// relay stops the run after the call instead.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
