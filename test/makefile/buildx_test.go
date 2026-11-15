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

// Package makefile runs targets of the repository's Makefile against stand-in
// commands, so that their exit status can be checked without a container
// runtime or a registry.
package makefile

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// containerStandIn records each call in $CALLS, fails "buildx build" with exit
// status 42 when $FAIL_BUILD is set and "buildx create" with 44 when
// $FAIL_CREATE is set. With $HANG_BUILD set, "buildx build" creates the file
// named by that variable and then blocks, so a test can interrupt the run.
const containerStandIn = `#!/bin/sh
echo "$*" >> "$CALLS"
if [ "$1" = buildx ] && [ -n "$INTERRUPT_CLEANUP" ]; then
  case "$2" in
    build) kill -TERM "$PPID"; exit 143 ;;
    rm) kill -TERM "$PPID" ;;
  esac
fi
if [ "$1" = buildx ] && [ "$2" = build ] && [ -n "$FAIL_BUILD" ]; then
  echo 'SIMULATED BUILD/PUSH FAILURE' >&2
  exit 42
fi
if [ "$1" = buildx ] && [ "$2" = build ] && [ -n "$HANG_BUILD" ]; then
  : > "$HANG_BUILD"
  exec sleep 30
fi
if [ "$1" = buildx ] && [ "$2" = create ] && [ -n "$FAIL_CREATE" ]; then
  echo 'SIMULATED BUILDER CREATION FAILURE: builder already exists' >&2
  exit 44
fi
exit 0
`

// runBuildx runs "make docker-buildx" in a directory of its own with the
// stand-in as the container tool, and returns make's exit code, its output
// and the calls the stand-in saw.
func runBuildx(t *testing.T, failEnv string) (code int, out string, calls []string, dir string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")
	dir = t.TempDir()
	dockerfile, err := os.ReadFile(filepath.Join(repo, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), dockerfile, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(t.TempDir(), "container-stand-in")
	if err := os.WriteFile(tool, []byte(containerStandIn), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	cmd := exec.Command("make", "-C", dir, "-f", filepath.Join(repo, "Makefile"), "docker-buildx",
		"CONTAINER_TOOL="+tool, "IMG=registry.invalid/operator:test")
	cmd.Env = append(childMakeEnv(), "CALLS="+log)
	if failEnv != "" {
		cmd.Env = append(cmd.Env, failEnv+"=1")
	}
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("make did not run: %v: %s", err, output)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the container tool was never called: %v: %s", err, output)
	}
	return code, string(output), strings.Split(strings.TrimSpace(string(data)), "\n"), dir
}

// assertOneBuilderThroughout checks that the builder this run created is the
// one it built through and the one it removed, and that the build read the
// run's own mktemp copy of the Dockerfile, not a shared Dockerfile.cross (#869).
func assertOneBuilderThroughout(t *testing.T, calls []string) {
	t.Helper()
	var created, built, removed, file string
	for _, c := range calls {
		f := strings.Fields(c)
		switch {
		case strings.HasPrefix(c, "buildx create --name ") && len(f) > 3:
			created = f[3]
		case strings.HasPrefix(c, "buildx build --builder ") && len(f) > 3:
			built = f[3]
			for i, a := range f {
				if a == "-f" && i+1 < len(f) {
					file = f[i+1]
				}
			}
		case strings.HasPrefix(c, "buildx rm ") && len(f) > 2:
			removed = f[2]
		}
	}
	if created == "" || built != created || removed != created {
		t.Errorf("created %q, built through %q, removed %q: want one builder throughout\ncalls = %q",
			created, built, removed, calls)
	}
	if !strings.HasPrefix(file, "Dockerfile.cross.") {
		t.Errorf("the build read %q, want the run's own Dockerfile.cross.XXXXXX copy", file)
	}
}

func hasCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// A failed "buildx build --push" fails the target, with the failure in its
// output, after the builder this run created and its Dockerfile.cross copy have
// been cleaned up; a build that succeeds leaves the target successful (#461).
func TestDockerBuildx_ReportsTheBuildsExitStatus(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	for _, tc := range []struct {
		name    string
		failEnv string
		marker  string
		build   bool
	}{
		{"a failed build and push", "FAIL_BUILD", "SIMULATED BUILD/PUSH FAILURE", true},
		{"a successful build and push", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, calls, dir := runBuildx(t, tc.failEnv)
			if tc.failEnv != "" && (code == 0 || !strings.Contains(out, tc.marker)) {
				t.Errorf("make docker-buildx exited %d after %s, want a failure that shows it:\n%s", code, tc.name, out)
			}
			if tc.failEnv == "" && code != 0 {
				t.Errorf("make docker-buildx exited %d after a successful build and push:\n%s", code, out)
			}
			if tc.build && !hasCall(calls, "buildx build --builder") {
				t.Errorf("calls = %q, want the build", calls)
			}
			if !tc.build && hasCall(calls, "buildx build") {
				t.Errorf("calls = %q, want no build after %s", calls, tc.name)
			}
			if !hasCall(calls, "buildx rm cubrid-kubernetes-operator-builder") {
				t.Errorf("calls = %q, want the removal of the builder", calls)
			}
			assertOneBuilderThroughout(t, calls)
			if hasCall(calls, "buildx use") {
				t.Errorf("calls = %q, want the user's selected builder left alone", calls)
			}
			assertNoDockerfileCross(t, dir)
		})
	}
}

func assertNoDockerfileCross(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, "Dockerfile.cross*"))
	if len(left) != 0 {
		t.Errorf("Dockerfile.cross copy left behind: %v", left)
	}
}

// A further cancellation during builder removal must not abort the EXIT trap
// before it removes the temporary Dockerfile. The original build stays failed.
func TestDockerBuildx_CancellationDuringCleanupRemovesOwnedResources(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	code, out, calls, dir := runBuildx(t, "INTERRUPT_CLEANUP")
	if code == 0 {
		t.Errorf("make docker-buildx succeeded after cancellation:\n%s", out)
	}
	assertOneBuilderThroughout(t, calls)
	assertNoDockerfileCross(t, dir)
}

// If the project's builder cannot be created, for example because a builder of
// that name already exists, the target fails with that message and neither
// builds through nor removes any builder: a builder this run did not create
// must survive it.
func TestDockerBuildx_FailedCreationLeavesExistingBuildersAlone(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	code, out, calls, dir := runBuildx(t, "FAIL_CREATE")
	if code == 0 || !strings.Contains(out, "SIMULATED BUILDER CREATION FAILURE") {
		t.Errorf("make docker-buildx exited %d after a failed builder creation, want a failure that shows it:\n%s", code, out)
	}
	for _, forbidden := range []string{"buildx use", "buildx build", "buildx rm"} {
		if hasCall(calls, forbidden) {
			t.Errorf("calls = %q, want no %q after a failed creation", calls, forbidden)
		}
	}
	assertNoDockerfileCross(t, dir)
}

// An interrupted run (SIGINT or SIGTERM to the whole process group, as a
// terminal or a CI job sends it) fails the target and still removes the builder
// this run created and its Dockerfile.cross copy, and no other builder.
func TestDockerBuildx_InterruptionCleansUpOnlyThisRunsResources(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			_, here, _, _ := runtime.Caller(0)
			repo := filepath.Join(filepath.Dir(here), "..", "..")
			dir := t.TempDir()
			dockerfile, err := os.ReadFile(filepath.Join(repo, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), dockerfile, 0o600); err != nil {
				t.Fatal(err)
			}
			scratch := t.TempDir()
			tool := filepath.Join(scratch, "container-stand-in")
			if err := os.WriteFile(tool, []byte(containerStandIn), 0o700); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(scratch, "calls")
			started := filepath.Join(scratch, "build-started")
			cmd := exec.Command("make", "-C", dir, "-f", filepath.Join(repo, "Makefile"), "docker-buildx",
				"CONTAINER_TOOL="+tool, "IMG=registry.invalid/operator:test")
			cmd.Env = append(childMakeEnv(), "CALLS="+log, "HANG_BUILD="+started)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pgid := cmd.Process.Pid
			t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(started); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the build never started")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := syscall.Kill(-pgid, sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("make docker-buildx succeeded after being interrupted")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("make docker-buildx did not stop after the interruption")
			}
			// The signal reaches make and the recipe's shell together, and make
			// may exit while the shell's EXIT trap is still removing the builder
			// and the Dockerfile.cross copy. The cleanup is the shell's, so the
			// check waits until every process of the group has ended.
			for deadline := time.Now().Add(10 * time.Second); syscall.Kill(-pgid, 0) == nil; {
				if time.Now().After(deadline) {
					t.Fatal("the recipe's processes did not end after the interruption")
				}
				time.Sleep(20 * time.Millisecond)
			}

			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			calls := strings.Split(strings.TrimSpace(string(data)), "\n")
			var created, removed []string
			for _, c := range calls {
				if name, ok := strings.CutPrefix(c, "buildx create --name "); ok {
					created = append(created, name)
				}
				if name, ok := strings.CutPrefix(c, "buildx rm "); ok {
					removed = append(removed, name)
				}
			}
			if len(created) != 1 || len(removed) != 1 || created[0] != removed[0] {
				t.Errorf("created %q, removed %q, want exactly this run's builder removed (calls %q)", created, removed, calls)
			}
			assertNoDockerfileCross(t, dir)
		})
	}
}

// childMakeEnv returns the environment without the caller's make flags
// (MAKEFLAGS=i, -n, ...), which must not reach the make under test.
func childMakeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "MAKEFLAGS", "MFLAGS", "MAKELEVEL", "MAKEOVERRIDES", "GNUMAKEFLAGS", "MAKEFILES":
		default:
			env = append(env, kv)
		}
	}
	return env
}
