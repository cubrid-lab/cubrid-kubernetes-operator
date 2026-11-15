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
	"strings"
	"testing"
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
	}{
		{name: "created cluster"},
		{name: "helper pull fails closed", failImage: images[0]},
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
				"MOCK_EXISTING_CLUSTER="+map[bool]string{true: "1", false: "0"}[tc.reuse],
				"E2E_REUSE_CLUSTER="+map[bool]string{true: "1", false: ""}[tc.reuse],
			)
			out, err := cmd.CombinedOutput()
			data, readErr := os.ReadFile(trace)
			if readErr != nil {
				t.Fatalf("read fake command trace: %v (output: %s)", readErr, out)
			}
			got := string(data)
			if tc.failImage != "" {
				if err == nil || !strings.Contains(string(out), "429 Too Many Requests") {
					t.Fatalf("failing pull did not fail visibly: %v\n%s", err, out)
				}
				if strings.Contains(got, "suite ran") {
					t.Fatalf("ran tests after a failed image pull:\n%s", got)
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
