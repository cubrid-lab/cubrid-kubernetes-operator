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
	"fmt"
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
	WorkingDirectory string            `yaml:"working-directory"`
	Name             string            `yaml:"name"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	With             map[string]any    `yaml:"with"`
	Env              map[string]string `yaml:"env"`
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

// Only a manual run or publish.yml starts it, and its token can only read: a
// push, a pull request or a schedule never builds release artifacts, and no
// step can write a tag, a release, a package or a cache entry.
func TestDryRunCannotPublish(t *testing.T) {
	text, wf, j := readDryRun(t)
	if len(wf.On) != 2 || wf.On["workflow_dispatch"] == nil || wf.On["workflow_call"] == nil {
		t.Errorf("triggers %v; want workflow_dispatch and workflow_call only", wf.On)
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
	_, _, j := readDryRun(t)
	i := stepIndex(t, j, versionStepName)
	for k, s := range j.Steps {
		if strings.HasPrefix(s.Uses, "docker/") && k < i {
			t.Errorf("step %q runs before the duplicate-version check", s.Name)
		}
	}
	tools := j.Steps[stepIndex(t, j, "Clone the version check")]
	sparse, _ := tools.With["sparse-checkout"].(string)
	if tools.With["path"] != ".release-check" || tools.With["ref"] != "${{ github.workflow_sha }}" {
		t.Error("the version check is not cloned from the workflow's own commit into .release-check")
	}
	// The installer image check needs its module files next to it.
	for _, want := range []string{"hack/release-version-check.sh", "hack/check-installer-image/", "go.mod", "go.sum"} {
		if !strings.Contains("\n"+sparse, "\n"+want+"\n") {
			t.Errorf("the workflow's own copy of %s is not cloned into .release-check", want)
		}
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

// The initial checkout is the trusted workflow revision. This step checks
// that it and the requested source are both on main *before* switching to the
// candidate. A tag named origin/main must not stand in for the remote ref.
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
	if code == 0 || !strings.Contains(out, "Workflow revision") {
		t.Errorf("a workflow revision not on main: exit %d\n%s", code, out)
	}

	code, out, _ = run(a, b)
	if code == 0 || !strings.Contains(out, "Commit "+b+" is not on main") {
		t.Errorf("a candidate not on main: exit %d\n%s", code, out)
	}
	if got := git("rev-parse", "HEAD"); got != a {
		t.Errorf("an unverified candidate was checked out: HEAD %s, want %s", got, a)
	}

	code, out, _ = run(b, a)
	if code == 0 || !strings.Contains(out, "Workflow revision") {
		t.Errorf("an untrusted workflow built a main candidate: exit %d\n%s", code, out)
	}

	git("tag", "origin/main", b)
	code, out, _ = run(a, b)
	if code == 0 || !strings.Contains(out, "Commit "+b+" is not on main") {
		t.Errorf("a tag origin/main shadowed the remote main ref: exit %d\n%s", code, out)
	}

	// Once b becomes part of main, the trusted workflow revision a can
	// select it, but only after the ancestry checks have succeeded.
	git("update-ref", "refs/remotes/origin/main", b)
	code, out, written = run(a, b)
	if code != 0 || !strings.Contains(written, "sha="+b+"\n") {
		t.Errorf("a verified main candidate different from the workflow: exit %d, output %q\n%s",
			code, written, out)
	}
}

// No candidate source is checked out before the trusted main-ancestry
// checks; every step that runs make or Docker follows them. No checkout
// leaves credentials, and generated-file changes still stop the dry run.
func TestDryRunStepOrderAndCheckouts(t *testing.T) {
	_, _, j := readDryRun(t)
	meta := stepIndex(t, j, metaStepName)
	clone := j.Steps[stepIndex(t, j, "Clone the code")]
	if clone.With["ref"] != "${{ github.workflow_sha }}" {
		t.Errorf("the first checkout selects %v, not the trusted workflow commit", clone.With["ref"])
	}
	guard := j.Steps[meta].Run
	workflowCheck := strings.Index(guard, "git merge-base --is-ancestor \"$workflow_sha\" refs/remotes/origin/main")
	candidateCheck := strings.Index(guard, "git merge-base --is-ancestor \"$INPUT_SHA\" refs/remotes/origin/main")
	checkout := strings.Index(guard, "git checkout --detach --quiet \"$INPUT_SHA\"")
	if workflowCheck < 0 || candidateCheck <= workflowCheck || checkout <= candidateCheck {
		t.Error("the requested commit can be checked out before verifying both main histories")
	}
	for k, s := range j.Steps {
		runsRepoCode := strings.Contains(s.Run, "make ") || strings.HasPrefix(s.Uses, "docker/") ||
			strings.HasPrefix(s.Uses, "./")
		for _, goCmd := range []string{"go run", "go build", "go test", "go generate"} {
			runsRepoCode = runsRepoCode || strings.Contains(s.Run, goCmd)
		}
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
	// The checker is the workflow's own copy, built from .release-check, not
	// the candidate's, which an older commit does not have.
	buildStep := j.Steps[stepIndex(t, j, "Build the installer image check")]
	if buildStep.WorkingDirectory != ".release-check" ||
		!strings.Contains(buildStep.Run, `go build -o "$RUNNER_TEMP/check-installer-image" ./hack/check-installer-image`) {
		t.Errorf("the installer image check is not built from the workflow's own copy: %q in %q",
			buildStep.Run, buildStep.WorkingDirectory)
	}
	if stepIndex(t, j, "Build the installer image check") > stepIndex(t, j, "Build the installer manifest") {
		t.Error("the installer image check is built after the candidate's make runs")
	}
	if strings.Contains(record, "go run") {
		t.Error("the record runs a checker from the candidate commit")
	}
	runRecordStep(t, record)
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

// The installer step passes make build-installer the Instance Manager image
// this run builds and the record checks for, not an image derived or defaulted
// elsewhere (#124).
func TestDryRunBuildsTheInstallerWithTheBuiltInstanceManagerImage(t *testing.T) {
	_, _, j := readDryRun(t)
	const ref = "$REGISTRY/instance-manager:$VERSION"
	installer := j.Steps[stepIndex(t, j, "Build the installer manifest")].Run
	if !strings.Contains(installer, `make build-installer IMG="$REGISTRY/manager:$VERSION" IM_IMG="`+ref+`"`) {
		t.Errorf("the installer is not built with IM_IMG=%s:\n%s", ref, installer)
	}
	if tags := j.Steps[stepIndex(t, j, "Build the Instance Manager image without pushing")].With["tags"]; tags !=
		"${{ env.REGISTRY }}/instance-manager:${{ inputs.version }}" {
		t.Errorf("the Instance Manager image is built as %v, not the image the installer names", tags)
	}
	if record := j.Steps[stepIndex(t, j, "Record the digests and checksums")].Run; !strings.Contains(record,
		`im_ref="`+ref+`"`) {
		t.Error("the record checks for another Instance Manager image than the installer names")
	}
}

// runRecordStep runs the record step with a checker built from this
// repository on three installers: one that sets the built Instance Manager
// image, one that sets none, and one that sets another image. The record
// must say which image the manager would use in each case.
func runRecordStep(t *testing.T, script string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	const registry = "ghcr.io/cubrid-lab/cubrid-kubernetes-operator"
	runnerTemp := t.TempDir()
	checker := filepath.Join(runnerTemp, "check-installer-image")
	build := exec.Command("go", "build", "-o", checker, "./hack/check-installer-image")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the checker: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(runnerTemp, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manager.oci.tar", "instance-manager.oci.tar"} {
		if err := os.WriteFile(filepath.Join(runnerTemp, "images", name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src, err := os.ReadFile(filepath.Join(repoRoot(), "internal", "controller", "cubridcluster_controller.go"))
	if err != nil {
		t.Fatal(err)
	}
	installer := func(args string) string {
		return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: x-controller-manager
  labels:
    control-plane: controller-manager
spec:
  template:
    spec:
      containers:
        - name: manager
          image: ` + registry + `/manager:` + version + `
          args: [--leader-elect` + args + `]
`
	}
	digest := "sha256:" + strings.Repeat("0", 64)
	cases := []struct {
		name, args string
		built      bool
		used       string
	}{
		{"the built image", ", --instance-manager-image=" + registry + "/instance-manager:" + version,
			true, registry + "/instance-manager:" + version},
		{"no flag", "", false, registry +
			"/instance-manager:dev, the manager's built-in default: install.yaml sets no --instance-manager-image"},
		{"another image", ", -instance-manager-image, example/im:1", false,
			`not verified: the manager uses Instance Manager image "example/im:1"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			work := t.TempDir()
			files := map[string]string{
				"dist/install.yaml": installer(c.args),
				"internal/controller/cubridcluster_controller.go": string(src),
			}
			for path, data := range files {
				full := filepath.Join(work, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(filepath.Join(runnerTemp, "release-dry-run")); err != nil {
				t.Fatal(err)
			}
			outFile := filepath.Join(work, "github-output")
			code, out := runStep(t, work, script, "REGISTRY="+registry, "VERSION="+version, "SHA="+sha,
				"CREATED=c", "BUILT=b", "WORKFLOW_SHA="+otherSHA, "RUN_ID=1", "RUN_ATTEMPT=1",
				"MANAGER_DIGEST="+digest, "MANAGER_ID="+digest, "IM_DIGEST="+digest, "IM_ID="+digest,
				"RUNNER_TEMP="+runnerTemp, "GITHUB_OUTPUT="+outFile)
			if code != 0 {
				t.Fatalf("the record step failed: exit %d\n%s", code, out)
			}
			written, _ := os.ReadFile(outFile)
			if !strings.Contains(string(written), fmt.Sprintf("im_built=%t\n", c.built)) ||
				!strings.Contains(string(written), "im_used="+c.used) {
				t.Errorf("the step output %q does not say im_built=%t and %q", written, c.built, c.used)
			}
			record, _ := os.ReadFile(filepath.Join(runnerTemp, "release-dry-run", "release-dry-run.json"))
			if !strings.Contains(string(record), fmt.Sprintf(`"usesBuiltInstanceManagerImage": %t`, c.built)) {
				t.Errorf("the record does not say usesBuiltInstanceManagerImage %t:\n%s", c.built, record)
			}
		})
	}
}
