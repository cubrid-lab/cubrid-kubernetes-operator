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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// kustomizeStandIn edits the kustomization.yaml of its working directory on
// "edit", as kustomize does, and prints the directory it builds on "build".
const kustomizeStandIn = `#!/bin/sh
case "$1" in
edit) echo "$*" >> kustomization.yaml ;;
build) echo "built $2" ;;
esac
`

// make build-installer leaves every tracked file alone and removes the
// overlay it rendered through (#537).
func TestBuildInstaller_LeavesConfigUnchanged(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")
	dir := t.TempDir()
	manager := filepath.Join(dir, "config", "manager", "kustomization.yaml")
	original, err := os.ReadFile(filepath.Join(repo, "config", "manager", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(manager), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager, original, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(t.TempDir(), "kustomize-stand-in")
	if err := os.WriteFile(tool, []byte(kustomizeStandIn), 0o700); err != nil {
		t.Fatal(err)
	}
	// -o keeps make from regenerating manifests or installing kustomize.
	cmd := exec.Command("make", "-C", dir, "-f", filepath.Join(repo, "Makefile"),
		"-o", "manifests", "-o", "generate", "-o", tool, "build-installer",
		"KUSTOMIZE="+tool, "IMG=example.com/manager:test")
	cmd.Env = childMakeEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make build-installer: %v\n%s", err, out)
	}

	if got, _ := os.ReadFile(manager); string(got) != string(original) {
		t.Errorf("config/manager/kustomization.yaml changed:\n%s", got)
	}
	installer, err := os.ReadFile(filepath.Join(dir, "dist", "install.yaml"))
	if err != nil || !strings.HasPrefix(string(installer), "built ") {
		t.Errorf("dist/install.yaml = %q, %v; want the rendered output", installer, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "dist", "overlay.*")); len(left) != 0 {
		t.Errorf("overlay left behind: %v", left)
	}
}

// deployKustomizeStandIn produces the overlay's text instead of applying real
// manifests. This lets the test verify both image selection and input piping
// without touching a Kubernetes cluster.
const deployKustomizeStandIn = `#!/bin/sh
case "$1" in
edit) echo "$*" >> kustomization.yaml ;;
build) cat "$2/kustomization.yaml" ;;
esac
`

// deployKubectlStandIn records the manifest stream received by "apply" and
// optionally fails to verify that make reports an unsuccessful deployment.
const deployKubectlStandIn = `#!/bin/sh
printf '%s\n' "$*" > "$APPLY_ARGS"
cat > "$APPLY_INPUT"
[ "$APPLY_FAIL" != 1 ]
`

// make deploy must not modify the tracked manager config or leave a temporary
// overlay behind, even when the kubectl apply step fails (#529, #537).
func TestDeploy_LeavesConfigUnchanged(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")
	original, err := os.ReadFile(filepath.Join(repo, "config", "manager", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		fail bool
	}{
		{name: "apply succeeds"},
		{name: "apply fails", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			manager := filepath.Join(dir, "config", "manager", "kustomization.yaml")
			if err := os.MkdirAll(filepath.Dir(manager), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manager, original, 0o600); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			kustomize := filepath.Join(bin, "kustomize-stand-in")
			kubectl := filepath.Join(bin, "kubectl-stand-in")
			for file, script := range map[string]string{
				kustomize: deployKustomizeStandIn,
				kubectl:   deployKubectlStandIn,
			} {
				if err := os.WriteFile(file, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			applied := filepath.Join(dir, "applied")
			args := filepath.Join(dir, "apply-args")
			fail := "0"
			if tc.fail {
				fail = "1"
			}
			// Neither manifests generation nor a real cluster command runs.
			cmd := exec.Command("make", "-C", dir, "-f", filepath.Join(repo, "Makefile"),
				"-o", "manifests", "-o", kustomize, "deploy",
				"KUSTOMIZE="+kustomize, "KUBECTL="+kubectl, "IMG=example.com/manager:test")
			cmd.Env = append(childMakeEnv(), "APPLY_ARGS="+args, "APPLY_INPUT="+applied, "APPLY_FAIL="+fail)
			out, err := cmd.CombinedOutput()
			if tc.fail != (err != nil) {
				t.Errorf("make deploy error = %v (expect failure: %v)\n%s", err, tc.fail, out)
			}
			if got, err := os.ReadFile(manager); err != nil || string(got) != string(original) {
				t.Errorf("tracked manager config changed or was lost: %v\n%s", err, got)
			}
			if got, err := os.ReadFile(args); err != nil || strings.TrimSpace(string(got)) != "apply -f -" {
				t.Errorf("kubectl arguments = %q, error = %v; want apply -f -", got, err)
			}
			if got, err := os.ReadFile(applied); err != nil ||
				!strings.Contains(string(got), "resources:\n- ../../config/default\n") ||
				!strings.Contains(string(got), "edit set image controller=example.com/manager:test") {
				t.Errorf("kubectl did not receive the image-overridden overlay: %v\n%s", err, got)
			}
			if left, _ := filepath.Glob(filepath.Join(dir, "dist", "overlay.*")); len(left) != 0 {
				t.Errorf("temporary overlay left after deploy: %v", left)
			}
		})
	}
}
