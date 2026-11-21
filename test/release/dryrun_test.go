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

package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// The release dry run (.github/workflows/release-dry-run.yml) builds the
// release artifacts of one main commit and must never publish them (#124).
// These tests read the workflow file and run its input and duplicate-version
// steps as the runner would.

type workflow struct {
	On          map[string]any `yaml:"on"`
	Permissions map[string]any `yaml:"permissions"`
	Jobs        map[string]job `yaml:"jobs"`
}

type job struct {
	Permissions map[string]string `yaml:"permissions"`
	Steps       []step            `yaml:"steps"`
}

type step struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

const (
	dryRunFile      = "release-dry-run.yml"
	formatStepName  = "Check the format of the commit and the version"
	versionStepName = "Refuse a version that already has a tag or a release"
)

func repoRoot() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..")
}

func readDryRun(t *testing.T) (string, workflow, job) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", dryRunFile))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	if len(wf.Jobs) != 1 {
		t.Fatalf("the dry run has %d jobs; want one", len(wf.Jobs))
	}
	for _, j := range wf.Jobs {
		return string(data), wf, j
	}
	return "", wf, job{}
}

func stepIndex(t *testing.T, j job, name string) int {
	t.Helper()
	for i, s := range j.Steps {
		if s.Name == name {
			return i
		}
	}
	t.Fatalf("no step named %q", name)
	return -1
}

// Only a manual run starts it, and its token can only read: a push, a pull
// request or a schedule never builds release artifacts, and no step can write
// a tag, a release, a package or a cache entry.
func TestDryRunCannotPublish(t *testing.T) {
	text, wf, j := readDryRun(t)
	if len(wf.On) != 1 || wf.On["workflow_dispatch"] == nil {
		t.Errorf("triggers %v; want workflow_dispatch only", wf.On)
	}
	if len(wf.Permissions) != 0 {
		t.Errorf("workflow permissions %v; want none", wf.Permissions)
	}
	if len(j.Permissions) != 1 || j.Permissions["contents"] != "read" {
		t.Errorf("job permissions %v; want contents: read only", j.Permissions)
	}
	forbidden := []string{
		"continue-on-error", "secrets.", "docker/login-action", "cache-to", "gh release", "git push", "git tag",
	}
	for _, bad := range forbidden {
		if strings.Contains(text, bad) {
			t.Errorf("the workflow contains %q", bad)
		}
	}
	if regexp.MustCompile(`(?m):\s*write\s*$`).MatchString(text) {
		t.Error("the workflow grants a write permission")
	}
	builds := 0
	for _, s := range j.Steps {
		if strings.HasPrefix(s.Uses, "docker/build-push-action@") {
			builds++
			if s.With["push"] != false {
				t.Errorf("step %q: push is %v; want false", s.Name, s.With["push"])
			}
			out, _ := s.With["outputs"].(string)
			if !strings.HasPrefix(out, "type=oci,dest=") {
				t.Errorf("step %q writes to %q; want a local OCI archive", s.Name, out)
			}
		}
	}
	if builds != 2 {
		t.Errorf("%d image builds; want the manager and the Instance Manager", builds)
	}
	if !strings.Contains(text, "make build-installer") {
		t.Error("the installer manifest is not built with make build-installer")
	}
}

// Every action is pinned to a full commit SHA that another workflow of this
// repository already uses, so the dry run adds no new third-party code.
func TestDryRunReusesPinnedActions(t *testing.T) {
	_, _, j := readDryRun(t)
	dir := filepath.Join(repoRoot(), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var others strings.Builder
	for _, e := range entries {
		if e.Name() == dryRunFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		others.Write(data)
	}
	pinned := regexp.MustCompile(`^[\w.-]+/[\w.-]+@[0-9a-f]{40}$`)
	for _, s := range j.Steps {
		if s.Uses == "" {
			continue
		}
		if !pinned.MatchString(s.Uses) {
			t.Errorf("step %q uses %q, not pinned to a full commit SHA", s.Name, s.Uses)
		}
		if !strings.Contains(others.String(), "uses: "+s.Uses) {
			t.Errorf("step %q uses %q, which no other workflow uses", s.Name, s.Uses)
		}
	}
}

// runStep runs a step's script with bash -e, as the runner does, in dir with
// the environment entries env.
func runStep(t *testing.T, dir, script string, env ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", script)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	default:
		t.Fatalf("run the step: %v\n%s", err, out)
		return -1, ""
	}
}

// The commit and version are checked before anything is cloned, and a value
// that is not exactly a full SHA or vMAJOR.MINOR.PATCH stops the run without
// being echoed.
func TestDryRunChecksInputFormats(t *testing.T) {
	_, _, j := readDryRun(t)
	i := stepIndex(t, j, formatStepName)
	if i != 0 {
		t.Errorf("the format check is step %d; want the first step", i)
	}
	script := j.Steps[i].Run
	const injectedCmd = "::warning::injected"
	cases := []struct {
		sha, version string
		ok           bool
	}{
		{sha, version, true},
		{sha, "v10.20.30", true},
		{"", version, false},
		{sha[:39], version, false},
		{strings.ToUpper(sha), version, false},
		{"main", version, false},
		{sha + "\n" + injectedCmd, version, false},
		{sha, "", false},
		{sha, "0.1.0", false},
		{sha, "v0.1", false},
		{sha, "v01.1.0", false},
		{sha, version + "-rc.1", false},
		{sha, version + "\n" + injectedCmd, false},
	}
	for _, c := range cases {
		code, out := runStep(t, t.TempDir(), script, "INPUT_SHA="+c.sha, "INPUT_VERSION="+c.version)
		if (code == 0) != c.ok {
			t.Errorf("sha %q version %q: exit %d, want ok=%t\n%s", c.sha, c.version, code, c.ok, out)
		}
		if strings.Contains(out, injectedCmd) {
			t.Errorf("sha %q version %q: the input was echoed\n%s", c.sha, c.version, out)
		}
	}
}

// The duplicate-version step runs hack/release-version-check.sh, taken from
// the workflow's own commit, before the first build, and continues only when
// the version has no tag and no release. A draft for the same commit, which a
// publishing rerun would resume, also stops a dry run.
func TestDryRunRefusesAnExistingVersion(t *testing.T) {
	text, _, j := readDryRun(t)
	i := stepIndex(t, j, versionStepName)
	for k, s := range j.Steps {
		if strings.HasPrefix(s.Uses, "docker/") && k < i {
			t.Errorf("step %q runs before the duplicate-version check", s.Name)
		}
	}
	if !strings.Contains(text, "path: .release-check") ||
		!strings.Contains(text, "ref: ${{ github.workflow_sha }}") ||
		!strings.Contains(text, "sparse-checkout: hack/release-version-check.sh") {
		t.Error("the version check is not cloned from the workflow's own commit into .release-check")
	}
	s := j.Steps[i]
	if s.Env["TAG"] != "${{ inputs.version }}" || s.Env["EXPECTED_SHA"] != "${{ steps.meta.outputs.sha }}" {
		t.Errorf("the check is given TAG %q and EXPECTED_SHA %q", s.Env["TAG"], s.Env["EXPECTED_SHA"])
	}

	// The step's working directory holds the script where the clone puts it.
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, ".release-check"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repoRoot(), "hack"), filepath.Join(work, ".release-check", "hack")); err != nil {
		t.Fatal(err)
	}
	absentTag := []answer{{http.StatusNotFound, notFound}}
	cases := []struct {
		name    string
		ref     []answer
		pages   map[int][]answer
		ok      bool
		wantOut string
	}{
		{name: "a new version", ref: absentTag, pages: map[int][]answer{1: list()}, ok: true},
		{name: "a draft for the same commit", ref: absentTag,
			pages: map[int][]answer{1: list(release(version, true, sha))}, wantOut: "already has a draft release"},
		{name: "a published release", ref: absentTag,
			pages: map[int][]answer{1: list(release(version, false, sha))}, wantOut: outPublished},
		{name: "a tag without a release", ref: tagAt(sha, "commit"),
			pages: map[int][]answer{1: list()}, wantOut: "already exists"},
		{name: "an API that does not answer", ref: []answer{{http.StatusInternalServerError, "oops"}},
			pages: map[int][]answer{1: list()}, wantOut: outUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &api{ref: c.ref, pages: c.pages, seen: map[string]int{}}
			srv := httptest.NewServer(a)
			defer srv.Close()
			code, out := runStep(t, work, s.Run,
				"REPOSITORY="+repository, "TAG="+version, "EXPECTED_SHA="+sha, "GITHUB_TOKEN="+token,
				"API_URL="+srv.URL, "RETRY_DELAY=0", "TMPDIR="+t.TempDir())
			if (code == 0) != c.ok || !strings.Contains(out, c.wantOut) {
				t.Errorf("exit %d, want ok=%t and %q in the output:\n%s", code, c.ok, c.wantOut, out)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if len(a.problems) > 0 {
				t.Errorf("unexpected requests: %v", a.problems)
			}
		})
	}
}
