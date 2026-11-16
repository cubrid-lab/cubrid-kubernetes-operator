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

package hack

import (
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

// Stub Kind and the container tool: no real cluster or image is changed.
// A failed helper-image pull must not start E2E and must clean up this run's cluster.
func TestRunE2EPreloadsHelperImagesAndKeepsCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Kind run wrapper is a bash script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}

	images := []string{
		"docker.io/adobe/s3mock:5.2.3",
		"docker.io/library/python:3.12-slim",
		"docker.io/curlimages/curl:8.10.1",
	}
	for _, tc := range []struct {
		name      string
		reuse     bool
		failImage string
		failLoad  string
	}{
		{name: "created cluster"},
		{name: "helper pull fails closed", failImage: images[0]},
		{name: "helper load fails closed", failLoad: images[0]},
		{name: "reused cluster", reuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			trace := filepath.Join(dir, "trace.txt")
			kind := filepath.Join(dir, "fake-kind")
			container := filepath.Join(dir, "fake-container")
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(kind, `#!/bin/sh
printf 'kind %s\n' "$*" >> "$TRACE"
if [ "$1" = get ] && [ "$2" = clusters ]; then
  [ "${MOCK_EXISTING_CLUSTER:-}" = 1 ] && printf '%s\n' "$KIND_CLUSTER"
fi
if [ "$1" = load ] && [ "$3" = "${MOCK_FAIL_LOAD:-}" ]; then exit 1; fi
exit 0
`)
			write(container, `#!/bin/sh
printf 'container %s\n' "$*" >> "$TRACE"
if [ "$1" = image ] && [ "$2" = inspect ]; then exit 1; fi
if [ "$1" = pull ] && [ "$2" = "${MOCK_FAIL_PULL:-}" ]; then
  echo '429 Too Many Requests' >&2
  exit 1
fi
exit 0
`)
			cmd := exec.Command("bash", "run-e2e.sh", "sh", "-c", `printf 'suite ran\n' >> "$TRACE"`)
			cmd.Env = append(os.Environ(),
				"KIND="+kind,
				"KIND_CLUSTER=ci-helper-test",
				"KIND_NODE_IMAGE=example.com/kind-node:test",
				"CONTAINER_TOOL="+container,
				"TRACE="+trace,
				"MOCK_FAIL_PULL="+tc.failImage,
				"MOCK_FAIL_LOAD="+tc.failLoad,
				"MOCK_EXISTING_CLUSTER="+map[bool]string{true: "1", false: "0"}[tc.reuse],
				"E2E_REUSE_CLUSTER="+map[bool]string{true: "1", false: ""}[tc.reuse],
			)
			out, err := cmd.CombinedOutput()
			data, readErr := os.ReadFile(trace)
			if readErr != nil {
				t.Fatalf("read fake command trace: %v (output: %s)", readErr, out)
			}
			got := string(data)
			if tc.failImage != "" || tc.failLoad != "" {
				if err == nil {
					t.Fatalf("failing preload did not fail the wrapper:\n%s", out)
				}
				if tc.failImage != "" && !strings.Contains(string(out), "429 Too Many Requests") {
					t.Fatalf("failing pull did not report the registry error:\n%s", out)
				}
				if tc.failLoad != "" && !strings.Contains(string(out), "could not load Kind test helper") {
					t.Fatalf("failing load did not report the Kind error:\n%s", out)
				}
				if strings.Contains(got, "suite ran") {
					t.Fatalf("ran tests after a failed image preload:\n%s", got)
				}
			} else {
				if err != nil {
					t.Fatalf("run wrapper failed: %v\n%s", err, out)
				}
				firstSuite := strings.Index(got, "suite ran")
				if firstSuite < 0 {
					t.Fatalf("the test command did not run:\n%s", got)
				}
				for _, image := range images {
					pulled := strings.Index(got, "container pull "+image)
					loaded := strings.Index(got, "kind load docker-image "+image+" --name ci-helper-test")
					if pulled < 0 || loaded <= pulled || loaded >= firstSuite {
						t.Errorf("image %s was not pulled and loaded before the suite:\n%s", image, got)
					}
				}
			}
			if tc.reuse {
				if strings.Contains(got, "kind create cluster") || strings.Contains(got, "kind delete cluster") {
					t.Fatalf("reused a cluster but created or deleted it:\n%s", got)
				}
			} else if !strings.Contains(got, "kind create cluster --name ci-helper-test") ||
				!strings.Contains(got, "kind delete cluster --name ci-helper-test") {
				t.Fatalf("owned Kind cluster was not created and cleaned up:\n%s", got)
			}
		})
	}
}

// A signal between launching a Kind image loader and recording its PID must
// still stop and reap the loader before cluster cleanup. The earlier pull
// regression sends TERM after startup and cannot exercise this handoff.
func TestRunE2ESignalDuringPreloadLaunchStopsHelperBeforeCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix process signals")
	}
	for _, reuse := range []bool{false, true} {
		for _, sig := range []struct {
			name string
			code int
		}{{"INT", 130}, {"TERM", 143}, {"HUP", 129}} {
			t.Run(sig.name+"/reuse="+strconv.FormatBool(reuse), func(t *testing.T) {
				dir := t.TempDir()
				trace := filepath.Join(dir, "trace")
				pidFile := filepath.Join(dir, "loader-pid")
				kind := filepath.Join(dir, "kind")
				container := filepath.Join(dir, "container")
				hook := filepath.Join(dir, "bash-env")
				for _, stub := range []struct{ path, body string }{
					{kind, `#!/usr/bin/env bash
printf 'kind %s\n' "$*" >> "$TRACE"
if [[ $1 == get && $2 == clusters && $E2E_REUSE_CLUSTER == 1 ]]; then
  printf '%s\n' "$KIND_CLUSTER"
elif [[ $1 == load ]]; then
  trap 'sleep 0.05; echo "load stopped" >> "$TRACE"; exit 0' INT TERM HUP
  echo "$BASHPID" > "$LOADER_PID"
  touch "$LOAD_STARTED"
  for ((i=0; i<200; i++)); do sleep 0.01; done
  echo 'load continued' >> "$TRACE"
fi
exit 0
`},
					{container, "#!/bin/sh\nexit 0\n"},
					// Inject only at the wrapper's PID assignment, after the loader
					// installs its signal handler. BASH_ENV keeps this scheduling
					// control out of the production wrapper.
					{hook, `set -T
trap 'if [[ $BASH_COMMAND == '\''preload_child=$!'\'' && ${2:-} == load ]]; then
  trap - DEBUG
  deadline=$((SECONDS + 3))
  until [[ -e $LOAD_STARTED ]]; do
    if (( SECONDS >= deadline )); then exit 97; fi
    sleep 0.01
  done
  echo "handoff signal" >> "$TRACE"
  kill -s "$MOCK_SIGNAL" "$$"
fi' DEBUG
`},
				} {
					if err := os.WriteFile(stub.path, []byte(stub.body), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				cmd := exec.Command("bash", "run-e2e.sh", "sh", "-c", `echo 'suite ran' >> "$TRACE"`)
				cmd.Env = append(os.Environ(),
					"KIND="+kind, "CONTAINER_TOOL="+container,
					"KIND_CLUSTER=ci-preload-handoff-test", "KIND_NODE_IMAGE=example.com/kind-node:test",
					"E2E_REUSE_CLUSTER="+map[bool]string{true: "1", false: ""}[reuse],
					"TRACE="+trace, "LOADER_PID="+pidFile, "LOAD_STARTED="+filepath.Join(dir, "started"),
					"BASH_ENV="+hook, "MOCK_SIGNAL="+sig.name,
				)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				t.Cleanup(func() {
					if cmd.Process != nil {
						_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					}
					if data, err := os.ReadFile(pidFile); err == nil {
						if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
							_ = syscall.Kill(-pid, syscall.SIGKILL)
						}
					}
				})
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				select {
				case err := <-done:
					status, ok := err.(*exec.ExitError)
					if !ok || status.ExitCode() != sig.code {
						t.Errorf("cancellation returned %v, want exit %d", err, sig.code)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("the wrapper did not finish after the preload launch signal")
				}
				data, err := os.ReadFile(trace)
				if err != nil {
					t.Fatal(err)
				}
				log := string(data)
				stopped := strings.Index(log, "load stopped")
				if !strings.Contains(log, "handoff signal") || stopped < 0 {
					t.Errorf("the signal at PID assignment did not stop the loader:\n%s", log)
				}
				if strings.Contains(log, "suite ran") || strings.Contains(log, "load continued") {
					t.Errorf("the canceled loader or suite continued:\n%s", log)
				}
				deleted := strings.Index(log, "kind delete cluster")
				if reuse {
					if strings.Contains(log, "kind create cluster") || deleted >= 0 {
						t.Errorf("the reused cluster was created or deleted:\n%s", log)
					}
				} else if stopped < 0 || deleted <= stopped {
					t.Errorf("owned-cluster cleanup did not wait for the loader to stop:\n%s", log)
				}
				data, err = os.ReadFile(pidFile)
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, 0); err == nil {
					t.Errorf("loader process %d survived wrapper cleanup", pid)
				}
			})
		}
	}
}

// A canceled CI run must not wait for a blocked registry pull before it
// deletes the Kind cluster that it created. This catches a Bash foreground
// command whose TERM trap would only run after the pull had finished.
func TestRunE2ETermDuringImagePullDeletesOwnedCluster(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix process signals")
	}
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace.txt")
	kind := filepath.Join(dir, "kind")
	containerTool := filepath.Join(dir, "container")
	for _, stub := range []struct{ path, body string }{
		{kind, `#!/bin/sh
printf 'kind %s\n' "$*" >> "$TRACE"
exit 0
`},
		{containerTool, `#!/bin/sh
printf 'container %s\n' "$*" >> "$TRACE"
if [ "$1" = image ] && [ "$2" = inspect ]; then exit 1; fi
if [ "$1" = pull ]; then
  echo 'pull started' >> "$TRACE"
  sleep 20
  echo 'pull finished' >> "$TRACE"
fi
exit 0
`},
	} {
		if err := os.WriteFile(stub.path, []byte(stub.body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "run-e2e.sh", "sh", "-c", `echo 'suite ran' >> "$TRACE"`)
	cmd.Env = append(os.Environ(),
		"KIND="+kind,
		"KIND_CLUSTER=ci-preload-signal-test",
		"KIND_NODE_IMAGE=example.com/kind-node:test",
		"CONTAINER_TOOL="+containerTool,
		"TRACE="+trace,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	// Wait for the fake host pull to start before sending the termination
	// signal. No timing assumption about Kind creation or the runner CPU.
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(trace)
		if strings.Contains(string(data), "pull started") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake image pull never started: %s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		status, ok := err.(*exec.ExitError)
		if !ok || status.ExitCode() != 143 {
			t.Errorf("termination returned %v, want exit 143", err)
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("a blocked pull delayed the termination handler and Kind cleanup")
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, "kind delete cluster --name ci-preload-signal-test") {
		t.Errorf("owned Kind cluster was not deleted after TERM:\n%s", log)
	}
	if strings.Contains(log, "suite ran") || strings.Contains(log, "pull finished") {
		t.Errorf("the canceled preload continued to the suite or returned late:\n%s", log)
	}
}
