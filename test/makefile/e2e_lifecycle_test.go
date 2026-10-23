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

package makefile

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// kindStandIn keeps the cluster names in $CLUSTERS, one per line, and records
// each create and delete in $CALLS.
const kindStandIn = `#!/bin/sh
case "$1 $2" in
"get clusters") cat "$CLUSTERS" ;;
"create cluster")
	echo "create $4" >> "$CALLS"
	echo "$4" >> "$CLUSTERS" ;;
"delete cluster")
	echo "delete $4" >> "$CALLS"
	grep -vx "$4" "$CLUSTERS" > "$CLUSTERS.new"
	mv "$CLUSTERS.new" "$CLUSTERS" ;;
esac
`

const e2eCluster = "cubrid-kubernetes-operator-test-e2e"

type e2eRun struct {
	clusters string
	calls    string
	marker   string
	cmd      *exec.Cmd
}

// newE2ERun prepares hack/run-e2e.sh with a kind stand-in that knows the given
// clusters, to run command. The command can write to $MARKER.
func newE2ERun(t *testing.T, existing []string, reuse string, command ...string) *e2eRun {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(here), "..", "..", "hack", "run-e2e.sh")
	dir := t.TempDir()
	r := &e2eRun{
		clusters: filepath.Join(dir, "clusters"),
		calls:    filepath.Join(dir, "calls"),
		marker:   filepath.Join(dir, "marker"),
	}
	list := ""
	for _, c := range existing {
		list += c + "\n"
	}
	if err := os.WriteFile(r.clusters, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	kind := filepath.Join(dir, "kind")
	if err := os.WriteFile(kind, []byte(kindStandIn), 0o700); err != nil {
		t.Fatal(err)
	}
	r.cmd = exec.Command(script, command...)
	r.cmd.Env = append(os.Environ(), "KIND="+kind, "KIND_CLUSTER="+e2eCluster,
		"KIND_NODE_IMAGE=kindest/node:test", "E2E_REUSE_CLUSTER="+reuse,
		"CLUSTERS="+r.clusters, "CALLS="+r.calls, "MARKER="+r.marker)
	return r
}

func (r *e2eRun) exitCode(t *testing.T, err error) int {
	t.Helper()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run hack/run-e2e.sh: %v", err)
	}
	return 0
}

func (r *e2eRun) run(t *testing.T) (int, string) {
	t.Helper()
	out, err := r.cmd.CombinedOutput()
	return r.exitCode(t, err), string(out)
}

func (r *e2eRun) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// A run creates its cluster and deletes it after a passing and a failing
// command, keeping the command's exit code (#536).
func TestRunE2E_DeletesTheClusterItCreated(t *testing.T) {
	for _, exit := range []int{0, 3} {
		r := newE2ERun(t, []string{"kind", e2eCluster + "-other"}, "", "sh", "-c", "exit "+strconv.Itoa(exit))
		code, out := r.run(t)
		if got := r.read(t, r.calls); got != "create "+e2eCluster+"\ndelete "+e2eCluster {
			t.Errorf("command exiting %d: kind calls = %q, want a create and a delete of %s\n%s", exit, got, e2eCluster, out)
		}
		if code != exit {
			t.Errorf("exit code = %d, want %d\n%s", code, exit, out)
		}
		if got := r.read(t, r.clusters); got != "kind\n"+e2eCluster+"-other" {
			t.Errorf("clusters left = %q, want the other clusters untouched", got)
		}
	}
}

// A cluster with the run's exact name that the run did not create is neither
// used nor deleted without the opt-in.
func TestRunE2E_RefusesAnExistingCluster(t *testing.T) {
	r := newE2ERun(t, []string{e2eCluster}, "", "sh", "-c", `echo ran > "$MARKER"`)
	code, out := r.run(t)
	if code == 0 {
		t.Fatalf("run against an existing cluster exited 0\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("output does not say the cluster exists:\n%s", out)
	}
	if got := r.read(t, r.calls); got != "" {
		t.Errorf("kind calls = %q, want none", got)
	}
	if got := r.read(t, r.marker); got != "" {
		t.Errorf("the command ran")
	}
}

// With E2E_REUSE_CLUSTER=1 the existing cluster is used and kept.
func TestRunE2E_ReusesAndKeepsAClusterOnRequest(t *testing.T) {
	r := newE2ERun(t, []string{e2eCluster}, "1", "sh", "-c", `echo ran > "$MARKER"; exit 4`)
	code, out := r.run(t)
	if code != 4 {
		t.Errorf("exit code = %d, want 4\n%s", code, out)
	}
	if got := r.read(t, r.marker); got != "ran" {
		t.Errorf("the command did not run\n%s", out)
	}
	if got := r.read(t, r.calls); got != "" {
		t.Errorf("kind calls = %q, want none", got)
	}
	if got := r.read(t, r.clusters); got != e2eCluster {
		t.Errorf("clusters left = %q, want %s kept", got, e2eCluster)
	}
}

// A signal reaches the command, which can clean up, and the cluster is deleted
// after the command has ended.
func TestRunE2E_DeletesTheClusterOnASignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		// The command records the signal it got, after which the stand-in
		// still has to be able to see the cluster.
		r := newE2ERun(t, nil, "", "sh", "-c",
			`trap 'grep -x `+e2eCluster+` "$CLUSTERS" > "$MARKER"; exit 9' INT TERM
			echo started > "$MARKER"
			while :; do sleep 0.05; done`)
		if err := r.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for r.read(t, r.marker) != "started" {
			if time.Now().After(deadline) {
				t.Fatal("the command did not start")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := r.cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		code := r.exitCode(t, r.cmd.Wait())
		if want := 128 + int(sig); code != want {
			t.Errorf("%v: exit code = %d, want %d", sig, code, want)
		}
		if got := r.read(t, r.marker); got != e2eCluster {
			t.Errorf("%v: the command did not clean up while the cluster existed (marker %q)", sig, got)
		}
		if got := r.read(t, r.calls); got != "create "+e2eCluster+"\ndelete "+e2eCluster {
			t.Errorf("%v: kind calls = %q, want a create and a delete", sig, got)
		}
	}
}

// goStandIn fails "go test" and passes everything else to the real go.
const goStandIn = `#!/bin/sh
if [ "$1" = test ]; then
	echo "go test" >> "$CALLS"
	exit 3
fi
exec "$REAL_GO" "$@"
`

// make test-e2e deletes the cluster it created when the suite fails, and does
// not take a cluster whose name only contains KIND_CLUSTER for its own (#536).
func TestTestE2E_DeletesItsClusterAfterAFailedSuite(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	r := newE2ERun(t, []string{e2eCluster + "-other"}, "")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "hack"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, "hack", "run-e2e.sh"), filepath.Join(dir, "hack", "run-e2e.sh")); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goStandIn), 0o700); err != nil {
		t.Fatal(err)
	}
	kind := filepath.Join(filepath.Dir(r.clusters), "kind")
	// -o keeps make from running vet, checking for Docker or installing kind.
	cmd := exec.Command("make", "-C", dir, "-f", filepath.Join(repo, "Makefile"),
		"-o", "vet", "-o", "container-tool", "-o", kind, "test-e2e",
		"KIND="+kind, "E2E_EVIDENCE_DIR="+filepath.Join(dir, "e2e-evidence"))
	cmd.Env = append(childMakeEnv(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_GO="+realGo, "CLUSTERS="+r.clusters, "CALLS="+r.calls, "MARKER="+r.marker)
	out, err := cmd.CombinedOutput()
	if r.exitCode(t, err) == 0 {
		t.Errorf("make test-e2e exited 0 although the suite failed\n%s", out)
	}
	if got, want := r.read(t, r.calls), "create "+e2eCluster+"\ngo test\ndelete "+e2eCluster; got != want {
		t.Errorf("calls = %q, want %q\n%s", got, want, out)
	}
	if got := r.read(t, r.clusters); got != e2eCluster+"-other" {
		t.Errorf("clusters left = %q, want only %s-other", got, e2eCluster)
	}
}
