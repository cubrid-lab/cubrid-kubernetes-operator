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

// crdKustomizeStandIn renders config/crd as RENDER says: "ok" prints a CRD,
// "empty" prints nothing, "fail" prints part of a manifest, then an error,
// and exits 42, as a kustomize that cannot render does.
const crdKustomizeStandIn = `#!/bin/sh
case "$RENDER" in
ok) echo "kind: CustomResourceDefinition" ;;
empty) ;;
fail) echo "kind: CustomResourceDefinition"; echo "Error: accumulating resources: boom" >&2; exit 42 ;;
fail-empty) echo "Error: accumulating resources: boom" >&2; exit 42 ;;
esac
`

// crdKubectlStandIn records that it ran, with its arguments and input, and
// fails when KUBECTL_FAIL is 1.
const crdKubectlStandIn = `#!/bin/sh
printf '%s\n' "$*" > "$KUBECTL_ARGS"
cat > "$KUBECTL_INPUT"
[ "$KUBECTL_FAIL" != 1 ]
`

// make install and make uninstall report a CRD render that failed, and never
// pass a failed render to kubectl (#868). A render that succeeds is applied
// or deleted, and a kubectl failure fails the target.
func TestInstallAndUninstall_ReportARenderFailure(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")
	bin := t.TempDir()
	kustomize := filepath.Join(bin, "kustomize-stand-in")
	kubectl := filepath.Join(bin, "kubectl-stand-in")
	for file, script := range map[string]string{kustomize: crdKustomizeStandIn, kubectl: crdKubectlStandIn} {
		if err := os.WriteFile(file, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"install", "uninstall"} {
		for _, tc := range []struct {
			name, render string
			kubectlFails bool
			wantFail     bool
			wantKubectl  bool
			wantOutput   string
		}{
			{name: "render fails after partial output", render: "fail", wantFail: true,
				wantOutput: "boom"},
			{name: "render fails with no output", render: "fail-empty", wantFail: true,
				wantOutput: "boom"},
			{name: "render succeeds", render: "ok", wantKubectl: true},
			{name: "kubectl fails", render: "ok", kubectlFails: true, wantFail: true, wantKubectl: true},
			{name: "render is empty", render: "empty", wantOutput: "No CRDs to"},
		} {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				args := filepath.Join(dir, "kubectl-args")
				input := filepath.Join(dir, "kubectl-input")
				fail := "0"
				if tc.kubectlFails {
					fail = "1"
				}
				// -o keeps make from regenerating manifests or installing
				// kustomize; no real cluster command runs.
				cmd := exec.Command("make", "-C", dir, "-f", filepath.Join(repo, "Makefile"),
					"-o", "manifests", "-o", "kustomize", target,
					"KUSTOMIZE="+kustomize, "KUBECTL="+kubectl)
				cmd.Env = append(childMakeEnv(), "RENDER="+tc.render, "KUBECTL_FAIL="+fail,
					"KUBECTL_ARGS="+args, "KUBECTL_INPUT="+input)
				out, err := cmd.CombinedOutput()
				if failed := err != nil; failed != tc.wantFail {
					t.Fatalf("make %s: err = %v, want failure %v\n%s", target, err, tc.wantFail, out)
				}
				if !strings.Contains(string(out), tc.wantOutput) {
					t.Errorf("make %s output does not contain %q:\n%s", target, tc.wantOutput, out)
				}
				_, statErr := os.Stat(args)
				if ran := statErr == nil; ran != tc.wantKubectl {
					t.Fatalf("kubectl ran = %v, want %v\n%s", ran, tc.wantKubectl, out)
				}
				if !tc.wantKubectl {
					return
				}
				got, err := os.ReadFile(input)
				if err != nil || !strings.Contains(string(got), "kind: CustomResourceDefinition") {
					t.Errorf("kubectl input = %q, %v; want the rendered CRDs", got, err)
				}
				verb := "apply"
				if target == "uninstall" {
					verb = "delete"
				}
				if a, _ := os.ReadFile(args); !strings.HasPrefix(string(a), verb+" ") {
					t.Errorf("kubectl args = %q, want %s", a, verb)
				}
			})
		}
	}
}
