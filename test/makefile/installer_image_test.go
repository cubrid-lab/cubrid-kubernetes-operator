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
	"strings"
	"testing"
)

// imageTestEnv is the environment of a child make without an inherited
// IM_IMG, which would otherwise choose the Instance Manager image.
func imageTestEnv() []string {
	var env []string
	for _, kv := range childMakeEnv() {
		if !strings.HasPrefix(kv, "IM_IMG=") {
			env = append(env, kv)
		}
	}
	return env
}

// renderTools returns the repository root, the pinned kustomize (installed
// into bin/ by make when it is missing) and the installer image check built
// from hack/check-installer-image, the same check the release dry run runs.
func renderTools(t *testing.T) (repo, kustomize, checker string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	repo = filepath.Join(filepath.Dir(here), "..", "..")
	install := exec.Command("make", "-s", "-C", repo, "kustomize")
	install.Env = imageTestEnv()
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("make kustomize: %v\n%s", err, out)
	}
	kustomize = filepath.Join(repo, "bin", "kustomize")
	checker = filepath.Join(t.TempDir(), "check-installer-image")
	build := exec.Command("go", "build", "-o", checker, "./hack/check-installer-image")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the installer image check: %v\n%s", err, out)
	}
	return repo, kustomize, checker
}

// workTree copies config/ into a temporary directory, where make renders
// without writing into the repository's dist/.
func workTree(t *testing.T, repo string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, "config"), os.DirFS(filepath.Join(repo, "config"))); err != nil {
		t.Fatal(err)
	}
	return dir
}

// checkInstaller runs the installer image check on a manifest and returns its
// exit code and output.
func checkInstaller(t *testing.T, checker, manifest, manager, instanceManager string) (int, string) {
	t.Helper()
	out, err := exec.Command(checker, "-manifest", manifest,
		"-manager", manager, "-instance-manager", instanceManager).CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	default:
		t.Fatalf("run the installer image check: %v", err)
		return 0, ""
	}
}

// make build-installer must make the manager use the Instance Manager image of
// the release: by default the one of IMG's registry and tag, or IM_IMG when it
// is set. Without it the manager falls back to its built-in development
// default, and the release dry run's check exits 3 (#124).
func TestBuildInstaller_SetsTheInstanceManagerImage(t *testing.T) {
	repo, kustomize, checker := renderTools(t)
	const registry = "registry.example.com/cubrid-lab/cubrid-kubernetes-operator"
	for _, tc := range []struct {
		name, imImg, want string
	}{
		{name: "derived from IMG", want: registry + "/instance-manager:v9.8.7"},
		{name: "set by IM_IMG", imImg: "other.example.com/im@sha256:" + strings.Repeat("a", 64),
			want: "other.example.com/im@sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := workTree(t, repo)
			args := []string{"-C", dir, "-f", filepath.Join(repo, "Makefile"),
				"-o", "manifests", "-o", "generate", "-o", kustomize, "build-installer",
				"KUSTOMIZE=" + kustomize, "IMG=" + registry + "/manager:v9.8.7"}
			if tc.imImg != "" {
				args = append(args, "IM_IMG="+tc.imImg)
			}
			cmd := exec.Command("make", args...)
			cmd.Env = imageTestEnv()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("make build-installer: %v\n%s", err, out)
			}
			code, out := checkInstaller(t, checker, filepath.Join(dir, "dist", "install.yaml"),
				registry+"/manager:v9.8.7", tc.want)
			if code != 0 {
				t.Errorf("the installer does not make the manager use %s: exit %d\n%s", tc.want, code, out)
			}
		})
	}
}

// Without IM_IMG, an IMG whose Instance Manager image cannot be derived (no
// tag, or a digest) stops build-installer before it writes an installer.
func TestBuildInstaller_RefusesAnUnderivableInstanceManagerImage(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")
	tool := filepath.Join(t.TempDir(), "kustomize-stand-in")
	if err := os.WriteFile(tool, []byte(kustomizeStandIn), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, img := range []string{
		"registry.example.com/manager",
		"registry.example.com/manager@sha256:" + strings.Repeat("b", 64),
		"localhost:5000/manager",
	} {
		t.Run(img, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command("make", "-C", dir, "-f", filepath.Join(repo, "Makefile"),
				"-o", "manifests", "-o", "generate", "-o", tool, "build-installer",
				"KUSTOMIZE="+tool, "IMG="+img)
			cmd.Env = imageTestEnv()
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "IM_IMG") {
				t.Errorf("make build-installer IMG=%s: error = %v; want a refusal naming IM_IMG\n%s", img, err, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "dist", "install.yaml")); err == nil {
				t.Error("an installer was written")
			}
		})
	}
}

// make deploy, which the E2E suite and the Kind checks run with IMG only,
// keeps the manager's built-in Instance Manager image unless IM_IMG is set,
// and then sets the flag exactly once.
func TestDeploy_SetsTheInstanceManagerImageOnlyWithIMImg(t *testing.T) {
	repo, kustomize, checker := renderTools(t)
	const manager = "example.com/cubrid-kubernetes-operator:v0.0.1"
	const im = "example.com/cubrid-instance-manager:e2e"
	for _, tc := range []struct {
		name, imImg string
		want        int
	}{
		{name: "IMG only", want: 3},
		{name: "IMG and IM_IMG", imImg: im, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := workTree(t, repo)
			applied := filepath.Join(dir, "applied.yaml")
			kubectl := filepath.Join(t.TempDir(), "kubectl-stand-in")
			if err := os.WriteFile(kubectl, []byte(deployKubectlStandIn), 0o700); err != nil {
				t.Fatal(err)
			}
			args := []string{"-C", dir, "-f", filepath.Join(repo, "Makefile"),
				"-o", "manifests", "-o", kustomize, "deploy",
				"KUSTOMIZE=" + kustomize, "KUBECTL=" + kubectl, "IMG=" + manager}
			if tc.imImg != "" {
				args = append(args, "IM_IMG="+tc.imImg)
			}
			cmd := exec.Command("make", args...)
			cmd.Env = append(imageTestEnv(), "APPLY_ARGS="+filepath.Join(dir, "apply-args"),
				"APPLY_INPUT="+applied, "APPLY_FAIL=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("make deploy: %v\n%s", err, out)
			}
			if code, out := checkInstaller(t, checker, applied, manager, im); code != tc.want {
				t.Errorf("installer image check exit = %d, want %d\n%s", code, tc.want, out)
			}
		})
	}
}
