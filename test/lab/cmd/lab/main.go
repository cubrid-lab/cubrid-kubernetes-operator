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
//	go run ./test/lab/cmd/lab -context <kubectl context> -target <lab> -- <command> [args]
//
// It checks that the context leads to the named lab, takes the lab's campaign
// Lease, and runs the command with KUBECONFIG pointing at a copy of that one
// context, so that switching contexts during the run cannot redirect it. The
// Lease is released when the command ends, also after an interrupt.
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
	"syscall"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/lab"
)

func main() {
	os.Exit(run())
}

func run() int {
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

	ctx := context.Background()
	if err := target.Check(ctx, kubectl); err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}

	kubeconfig, cleanup, err := pinnedKubeconfig(ctx, target.Context)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}
	defer cleanup()

	lock, err := target.Acquire(ctx, kubectl, holder(), *duration, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "lab: %q (context %q) is held for this run\n", target.Name, target.Context)

	code := runCommand(command, kubeconfig, target)

	// A fresh context: the release must happen even when the command was
	// interrupted.
	releaseCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := lock.Release(releaseCtx, kubectl); err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		if code == 0 {
			code = 1
		}
	} else {
		fmt.Fprintf(os.Stderr, "lab: %q released\n", target.Name)
	}
	return code
}

// runCommand runs the command to its end and returns its exit code. An
// interrupt or termination is passed on to the command, which is expected to
// clean up and exit; the Lease is released afterwards.
func runCommand(command []string, kubeconfig string, target lab.Target) int {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+kubeconfig,
		"LAB_CONTEXT="+target.Context,
		"LAB_TARGET="+target.Name,
	)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		return 1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case sig := <-signals:
			_ = cmd.Process.Signal(sig)
		case err := <-done:
			var exit *exec.ExitError
			switch {
			case err == nil:
				return 0
			case errors.As(err, &exit):
				if code := exit.ExitCode(); code > 0 {
					return code
				}
				return 1
			default:
				fmt.Fprintln(os.Stderr, "lab:", err)
				return 1
			}
		}
	}
}

// pinnedKubeconfig writes the named context, and only it, to a private file
// and returns its path.
func pinnedKubeconfig(ctx context.Context, name string) (string, func(), error) {
	out, err := kubectl(ctx, "", "config", "view", "--minify", "--flatten", "--context", name)
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
	return fmt.Sprintf("%s@%s/%d/%s", name, host, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
}

func kubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
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
