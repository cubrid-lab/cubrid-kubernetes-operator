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
	"testing"
)

// containerStandIn records each call in $CALLS and fails "buildx build" with
// exit status 42 when $FAIL_BUILD is set.
const containerStandIn = `#!/bin/sh
echo "$*" >> "$CALLS"
if [ "$1" = buildx ] && [ "$2" = build ] && [ -n "$FAIL_BUILD" ]; then
  echo 'SIMULATED BUILD/PUSH FAILURE' >&2
  exit 42
fi
exit 0
`

// runBuildx runs "make docker-buildx" in a directory of its own with the
// stand-in as the container tool, and returns make's exit code, its output
// and the calls the stand-in saw.
func runBuildx(t *testing.T, failBuild bool) (code int, out string, calls []string, dir string) {
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
	cmd.Env = append(os.Environ(), "CALLS="+log)
	if failBuild {
		cmd.Env = append(cmd.Env, "FAIL_BUILD=1")
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

func hasCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// A failed "buildx build --push" fails the target, with the failure in its
// output, after the builder and Dockerfile.cross have been cleaned up; a
// build that succeeds leaves the target successful (#461).
func TestDockerBuildx_ReportsTheBuildsExitStatus(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	for _, tc := range []struct {
		name      string
		failBuild bool
	}{{"a failed build and push", true}, {"a successful build and push", false}} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, calls, dir := runBuildx(t, tc.failBuild)
			if tc.failBuild && (code == 0 || !strings.Contains(out, "SIMULATED BUILD/PUSH FAILURE")) {
				t.Errorf("make docker-buildx exited %d after a failed build and push, want a failure that shows it:\n%s", code, out)
			}
			if !tc.failBuild && code != 0 {
				t.Errorf("make docker-buildx exited %d after a successful build and push:\n%s", code, out)
			}
			if !hasCall(calls, "buildx build --push") || !hasCall(calls, "buildx rm cubrid-kubernetes-operator-builder") {
				t.Errorf("calls = %q, want the build and then the removal of the builder", calls)
			}
			if _, err := os.Stat(filepath.Join(dir, "Dockerfile.cross")); !os.IsNotExist(err) {
				t.Errorf("Dockerfile.cross was left behind (stat: %v)", err)
			}
		})
	}
}
