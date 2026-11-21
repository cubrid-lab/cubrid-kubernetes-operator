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
		"continue-on-error", "secrets.", "docker/login-action", "cache-to", "actions/cache@",
		"gh release", "git push", "git tag", "docker push",
	}
	for _, bad := range forbidden {
		if strings.Contains(text, bad) {
			t.Errorf("the workflow contains %q", bad)
		}
	}
	if regexp.MustCompile(`(?m):\s*write\s*$`).MatchString(text) {
		t.Error("the workflow grants a write permission")
	}
	if regexp.MustCompile(`gh api\b.*(-X|--method)[ =]?(POST|PUT|PATCH|DELETE)`).MatchString(text) {
		t.Error("the workflow calls the GitHub API with a write method")
	}
	builds := 0
	for _, s := range j.Steps {
		// setup-go saves a module cache unless told not to.
		if strings.HasPrefix(s.Uses, "actions/setup-go@") && s.With["cache"] != false {
			t.Errorf("step %q: cache is %v; want false", s.Name, s.With["cache"])
		}
		if strings.HasPrefix(s.Uses, "docker/build-push-action@") {
			builds++
			// The action uploads a build record artifact unless told not to.
			if s.Env["DOCKER_BUILD_RECORD_UPLOAD"] != "false" {
				t.Errorf("step %q: DOCKER_BUILD_RECORD_UPLOAD is %q; want \"false\"",
					s.Name, s.Env["DOCKER_BUILD_RECORD_UPLOAD"])
			}
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

const metaStepName = "Resolve the commit and check that it is on main"

// gitRepo makes a repository with commit a on main and its child b, which is
// not on main, and returns a function that runs git in it.
func gitRepo(t *testing.T) (dir, a, b string, git func(args ...string) string) {
	t.Helper()
	dir = t.TempDir()
	git = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_COMMITTER_DATE=2026-11-20T10:00:00+09:00")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "on main")
	a = git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "not on main")
	b = git("rev-parse", "HEAD")
	// What actions/checkout leaves for main: a remote-tracking ref.
	git("update-ref", "refs/remotes/origin/main", a)
	return dir, a, b, git
}

// The step that confirms the checked-out commit and refuses one that is not
// on main runs as the runner would, in a repository whose origin/main is
// known. A tag named origin/main must not stand in for main.
func TestDryRunBuildsOnlyMainCommits(t *testing.T) {
	_, _, j := readDryRun(t)
	script := j.Steps[stepIndex(t, j, metaStepName)].Run
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir, a, b, git := gitRepo(t)
	run := func(head, input string) (int, string, string) {
		t.Helper()
		git("checkout", "-q", "--detach", head)
		tmp := t.TempDir()
		outFile := filepath.Join(tmp, "github-output")
		code, out := runStep(t, dir, script, "INPUT_SHA="+input, "RUNNER_TEMP="+tmp,
			"GITHUB_OUTPUT="+outFile, "HOME="+tmp, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		written, _ := os.ReadFile(outFile)
		return code, out, string(written)
	}

	code, out, written := run(a, a)
	if code != 0 || !strings.Contains(written, "sha="+a+"\n") {
		t.Errorf("a main commit: exit %d, output %q\n%s", code, written, out)
	}
	// The creation label is the commit time in UTC, not the time of the run.
	if !strings.Contains(written, "created=2026-11-20T01:00:00Z\n") {
		t.Errorf("a main commit: created is not the commit time in UTC: %q", written)
	}

	code, out, _ = run(b, b)
	if code == 0 || !strings.Contains(out, "is not on main") {
		t.Errorf("a commit not on main: exit %d\n%s", code, out)
	}

	code, out, _ = run(a, b)
	if code == 0 || !strings.Contains(out, "not the requested commit") {
		t.Errorf("a checkout that is not the requested commit: exit %d\n%s", code, out)
	}

	git("tag", "origin/main", b)
	code, out, _ = run(b, b)
	if code == 0 || !strings.Contains(out, "is not on main") {
		t.Errorf("a commit not on main with a tag named origin/main at it: exit %d\n%s", code, out)
	}
}

// No code from the checked-out commit runs before it is known to be on main:
// every step that runs make or a Docker action comes after the ancestry
// check. No checkout leaves a credential in the repository, and the stale
// generated file check follows make build-installer.
func TestDryRunStepOrderAndCheckouts(t *testing.T) {
	_, _, j := readDryRun(t)
	meta := stepIndex(t, j, metaStepName)
	for k, s := range j.Steps {
		runsRepoCode := strings.Contains(s.Run, "make ") || strings.HasPrefix(s.Uses, "docker/") ||
			strings.HasPrefix(s.Uses, "./")
		if runsRepoCode && k < meta {
			t.Errorf("step %q runs before the check that the commit is on main", s.Name)
		}
		if strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["persist-credentials"] != false {
			t.Errorf("step %q: persist-credentials is %v; want false", s.Name, s.With["persist-credentials"])
		}
	}
	installer := j.Steps[stepIndex(t, j, "Build the installer manifest")].Run
	build := strings.Index(installer, "make build-installer")
	status := strings.Index(installer, "git status --porcelain")
	if build < 0 || status < build || !strings.Contains(installer[status:], "exit 1") {
		t.Error("the installer step does not fail on tracked files that make build-installer changed")
	}
}

// The installer must make the manager use the Instance Manager image built in
// the same run. When install.yaml does not name it, the record says which
// image the manager falls back to, and the run fails after the upload.
func TestDryRunRequiresTheBuiltInstanceManagerImage(t *testing.T) {
	_, _, j := readDryRun(t)
	record := j.Steps[stepIndex(t, j, "Record the digests and checksums")].Run
	if !strings.Contains(record, `grep -Fq "$im_ref" dist/install.yaml`) ||
		!strings.Contains(record, "DefaultInstanceManagerImage") {
		t.Error("the record does not check install.yaml for the built Instance Manager image")
	}
	src, err := os.ReadFile(filepath.Join(repoRoot(), "internal", "controller", "cubridcluster_controller.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s*DefaultInstanceManagerImage = "[^"]+"$`).Match(src) {
		t.Error("the record cannot read DefaultInstanceManagerImage from the controller")
	}
	require := stepIndex(t, j, "Require the installer to use the Instance Manager image built here")
	upload := stepIndex(t, j, "Upload the installer manifest and the record")
	if require < upload {
		t.Error("the Instance Manager image requirement runs before the record is uploaded")
	}
	script := j.Steps[require].Run
	if code, out := runStep(t, t.TempDir(), script, "IM_BUILT=true"); code != 0 {
		t.Errorf("the built image is used: exit %d\n%s", code, out)
	}
	code, out := runStep(t, t.TempDir(), script, "IM_BUILT=false", "IM_USED=example/instance-manager:dev")
	if code == 0 || !strings.Contains(out, "example/instance-manager:dev") {
		t.Errorf("the built image is not used: exit %d\n%s", code, out)
	}
}
