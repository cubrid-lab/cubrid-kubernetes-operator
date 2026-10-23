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
	"os/exec"
	"strings"
	"testing"
	"time"
)

func scripted(script string, starts *int) func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		*starts++
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
}

func TestForwarderReadsTheLocalPortKubectlChose(t *testing.T) {
	starts := 0
	f := &forwarder{start: scripted(`echo "Forwarding from 127.0.0.1:41234 -> 9090"; exec sleep 30`, &starts),
		ready: 5 * time.Second}
	defer f.Close()
	url, err := f.ensure(context.Background())
	if err != nil || url != "http://127.0.0.1:41234" {
		t.Fatalf("ensure() = %q, %v; want http://127.0.0.1:41234", url, err)
	}
	if again, err := f.ensure(context.Background()); err != nil || again != url || starts != 1 {
		t.Fatalf("ensure() again = %q, %v after %d starts; want the running forward kept", again, err, starts)
	}
}

func TestForwarderStartsAgainAfterThePortForwardEnded(t *testing.T) {
	starts := 0
	f := &forwarder{start: scripted(`echo "Forwarding from 127.0.0.1:41235 -> 9090"; exit 1`, &starts),
		ready: 5 * time.Second}
	defer f.Close()
	if _, err := f.ensure(context.Background()); err != nil {
		t.Fatalf("ensure() error = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !f.exited() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.ensure(context.Background()); err != nil || starts != 2 {
		t.Fatalf("ensure() after exit = %v after %d starts; want a second start", err, starts)
	}
}

func TestForwarderReportsAPortForwardThatNeverListens(t *testing.T) {
	starts := 0
	f := &forwarder{start: scripted(`echo "error: services not found" >&2; exit 1`, &starts), ready: 5 * time.Second}
	defer f.Close()
	if _, err := f.ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "port-forward") {
		t.Fatalf("ensure() error = %v, want one naming the port-forward", err)
	}
	slow := &forwarder{start: scripted(`exec sleep 30`, &starts), ready: 200 * time.Millisecond}
	defer slow.Close()
	if _, err := slow.ensure(context.Background()); err == nil {
		t.Fatal("ensure() of a port-forward that prints nothing = nil error, want a timeout")
	}
}

func TestForwarderNotesEachStartAndEnd(t *testing.T) {
	starts := 0
	script := `echo "Forwarding from 127.0.0.1:41236 -> 9090"; echo "lost connection to pod" >&2; exit 1`
	f := &forwarder{start: scripted(script, &starts),
		ready: 5 * time.Second}
	defer f.Close()
	if _, err := f.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !f.exited() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	notes := strings.Join(f.DrainNotes(), "\n")
	if !strings.Contains(notes, "started") || !strings.Contains(notes, "ended") ||
		!strings.Contains(notes, "lost connection to pod") {
		t.Fatalf("notes = %q, want the start, the end and what kubectl said", notes)
	}
	if again := f.DrainNotes(); len(again) != 0 {
		t.Fatalf("notes drained twice = %q, want each once", again)
	}
}
