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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The release workflows (#124): release-please.yml only maintains the release
// PR, and publish.yml is the only workflow that publishes, after a manual
// start, a check of the release commit, the verified build and the approval
// of the production environment.

type anyJob struct {
	Needs       any               `yaml:"needs"`
	Uses        string            `yaml:"uses"`
	With        map[string]any    `yaml:"with"`
	Environment any               `yaml:"environment"`
	Permissions map[string]string `yaml:"permissions"`
	Outputs     map[string]string `yaml:"outputs"`
	Steps       []step            `yaml:"steps"`
}

// keyGuardStep is release-commit's check that no App key is a repository or
// organization secret.
const keyGuardStep = "Refuse App keys stored outside their environments"

// The values publish.yml trusts (the digests, the archive SHA-256s and the
// checker's SHA-256) come from the job outputs of build-images, which runs no
// make: the candidate's code runs only inside BuildKit there. The make of
// the candidate runs in the separate installer job.
func TestDryRunTrustedOutputsComeFromAJobWithoutMake(t *testing.T) {
	_, wf := readWorkflow(t, dryRunFile)
	build, ok := wf.Jobs["build-images"]
	if !ok {
		t.Fatal("no build-images job")
	}
	for _, s := range build.Steps {
		if strings.Contains(s.Run, "make ") {
			t.Errorf("build-images step %q runs make", s.Name)
		}
		if strings.Contains(s.Run, "go build") && (s.WorkingDirectory != releaseCheckDir || s.Env["GOWORK"] != "off") {
			t.Errorf("build-images step %q builds code that is not the workflow's own copy", s.Name)
		}
	}
	want := map[string]string{
		"manager-digest":                  "${{ steps.archives.outputs.manager-digest }}",
		"manager-archive-sha256":          "${{ steps.archives.outputs.manager-archive-sha256 }}",
		"instance-manager-digest":         "${{ steps.archives.outputs.instance-manager-digest }}",
		"instance-manager-archive-sha256": "${{ steps.archives.outputs.instance-manager-archive-sha256 }}",
		"checker-sha256":                  "${{ steps.checker.outputs.sha256 }}",
	}
	for k, v := range want {
		if build.Outputs[k] != v {
			t.Errorf("build-images output %s is %q; want %q", k, build.Outputs[k], v)
		}
	}
	// The checker is built before the candidate is checked out.
	checker, meta := -1, -1
	for k, s := range build.Steps {
		switch s.Name {
		case "Build the installer image check":
			checker = k
		case "Resolve the commit and check that it is on main":
			meta = k
		}
	}
	if checker < 0 || meta < 0 || checker > meta {
		t.Error("the installer image check is not built before the candidate is checked out")
	}
	makeJobs := 0
	for id, j := range wf.Jobs {
		for _, s := range j.Steps {
			if strings.Contains(s.Run, "make build-installer") {
				makeJobs++
				if id != "installer" {
					t.Errorf("job %s runs make build-installer", id)
				}
			}
		}
	}
	if makeJobs != 1 {
		t.Errorf("make build-installer runs in %d steps; want one, in the installer job", makeJobs)
	}
	call, _ := wf.On["workflow_call"].(map[string]any)
	outputs, _ := call["outputs"].(map[string]any)
	for k := range want {
		o, _ := outputs[k].(map[string]any)
		if o["value"] != "${{ jobs.build-images.outputs."+k+" }}" {
			t.Errorf("workflow output %s is %v; want build-images' output", k, o["value"])
		}
	}
}

// publish-production compares what it downloads with the verify job's
// outputs, on its own runner, before anything is pushed, and pushes from a
// record it writes from those outputs, not from the installer job's record.
func TestPublishComparesAgainstTheBuildOutputs(t *testing.T) {
	_, wf := readWorkflow(t, "publish.yml")
	pub := wf.Jobs["publish-production"]
	check, push := -1, -1
	for k, s := range pub.Steps {
		switch s.Name {
		case "Check the verified files against the build outputs":
			check = k
		case "Push the verified images":
			push = k
		}
	}
	if check < 0 || push < 0 || check > push {
		t.Fatal("the downloaded files are not checked against the build outputs before the push")
	}
	s := pub.Steps[check]
	for env, out := range map[string]string{
		"MANAGER_DIGEST":  "manager-digest",
		"MANAGER_ARCHIVE": "manager-archive-sha256",
		"IM_DIGEST":       "instance-manager-digest",
		"IM_ARCHIVE":      "instance-manager-archive-sha256",
		"CHECKER_SHA256":  "checker-sha256",
	} {
		if s.Env[env] != "${{ needs.verify.outputs."+out+" }}" {
			t.Errorf("%s is %q; want the verify job's output %s", env, s.Env[env], out)
		}
	}
	for _, want := range []string{
		`skopeo inspect --raw "oci-archive:$archive"`,
		`"$(sum "$checker")" != "$CHECKER_SHA256"`,
		`"$checker" -require-digest -manifest "$verified/install.yaml"`,
		`-manager "$REGISTRY/manager:$VERSION@$MANAGER_DIGEST"`,
		`-instance-manager "$REGISTRY/instance-manager:$VERSION@$IM_DIGEST"`,
		`> "$RUNNER_TEMP/trusted/images.json"`,
	} {
		if !strings.Contains(s.Run, want) {
			t.Errorf("the check does not contain %q", want)
		}
	}
	if pub.Steps[push].Env["RECORD"] != "${{ runner.temp }}/trusted/images.json" {
		t.Errorf("the push reads %q, not the record written from the build outputs", pub.Steps[push].Env["RECORD"])
	}
}

// release-commit runs in no environment, so an App key visible there is a
// repository or organization secret, which any branch could read.
func TestPublishRefusesAppKeysOutsideTheirEnvironments(t *testing.T) {
	_, wf := readWorkflow(t, "publish.yml")
	var guard step
	for _, s := range wf.Jobs["release-commit"].Steps {
		if s.Name == keyGuardStep {
			guard = s
		}
	}
	if guard.Env["PUBLISH_KEY_VISIBLE"] != "${{ secrets.RELEASE_APP_PRIVATE_KEY != '' }}" ||
		guard.Env["PR_KEY_VISIBLE"] != "${{ secrets.RELEASE_PR_APP_PRIVATE_KEY != '' }}" {
		t.Fatalf("the key guard reads %v", guard.Env)
	}
	for _, c := range []struct {
		publish, pr string
		ok          bool
	}{{strFalse, strFalse, true}, {strTrue, strFalse, false}, {strFalse, strTrue, false}, {"", strFalse, false}} {
		code, out := runStep(t, t.TempDir(), guard.Run, "PUBLISH_KEY_VISIBLE="+c.publish, "PR_KEY_VISIBLE="+c.pr)
		if (code == 0) != c.ok {
			t.Errorf("visible %q/%q: exit %d\n%s", c.publish, c.pr, code, out)
		}
	}
}

type anyWorkflow struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]any    `yaml:"permissions"`
	Jobs        map[string]anyJob `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) (string, anyWorkflow) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var wf anyWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	return string(data), wf
}

// Permission levels as workflow files spell them, the workflow's own clone
// of its tools, and boolean expression results as a step's environment
// receives them.
const (
	permRead        = "read"
	permWrite       = "write"
	releaseCheckDir = ".release-check"
	strFalse        = "false"
	strTrue         = "true"
)

var pinnedAction = regexp.MustCompile(`^[\w.-]+/[\w.-]+@[0-9a-f]{40}$`)

func checkPinned(t *testing.T, wf anyWorkflow) {
	t.Helper()
	for id, j := range wf.Jobs {
		for _, s := range j.Steps {
			if s.Uses != "" && !pinnedAction.MatchString(s.Uses) {
				t.Errorf("job %s step %q uses %q, not pinned to a full commit SHA", id, s.Name, s.Uses)
			}
			if strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["persist-credentials"] != false {
				t.Errorf("job %s step %q keeps its credentials", id, s.Name)
			}
		}
	}
}

// Only a manual start publishes; the only job with write permissions is
// publish-production in the production environment, after the release
// commit check and the verified build, and every write step comes from the
// workflow's own scripts.
func TestPublishWorkflowIsGated(t *testing.T) {
	text, wf := readWorkflow(t, "publish.yml")
	if len(wf.On) != 1 || wf.On["workflow_dispatch"] == nil {
		t.Errorf("triggers %v; want workflow_dispatch only", wf.On)
	}
	if len(wf.Permissions) != 0 {
		t.Errorf("workflow permissions %v; want none", wf.Permissions)
	}
	checkPinned(t, wf)
	for id, j := range wf.Jobs {
		for perm, level := range j.Permissions {
			if level == permWrite && id != "publish-production" {
				t.Errorf("job %s may write %s", id, perm)
			}
		}
	}
	pub, ok := wf.Jobs["publish-production"]
	if !ok {
		t.Fatal("no publish-production job")
	}
	if pub.Environment != "production" {
		t.Errorf("publish-production runs in environment %v; want production", pub.Environment)
	}
	needs, _ := pub.Needs.([]any)
	if len(needs) != 2 || needs[0] != "release-commit" || needs[1] != "verify" {
		t.Errorf("publish-production needs %v; want release-commit and verify", pub.Needs)
	}
	// The release is written with the App token minted from secrets of the
	// production environment, not with GITHUB_TOKEN, which only pushes
	// images (GHCR takes no App token).
	want := map[string]string{"contents": permRead, "packages": permWrite}
	if len(pub.Permissions) != len(want) {
		t.Errorf("publish-production permissions %v; want %v", pub.Permissions, want)
	}
	for k, v := range want {
		if pub.Permissions[k] != v {
			t.Errorf("publish-production permission %s is %q; want %q", k, pub.Permissions[k], v)
		}
	}
	verify := wf.Jobs["verify"]
	if verify.Uses != "./.github/workflows/release-dry-run.yml" || verify.With["keep-images"] != true {
		t.Errorf("verify is %q with %v; want the release dry run with keep-images", verify.Uses, verify.With)
	}
	if verify.Permissions["contents"] != permRead || len(verify.Permissions) != 1 {
		t.Errorf("verify permissions %v; want contents: read", verify.Permissions)
	}
	if strings.Contains(text, "continue-on-error") {
		t.Error("publish.yml continues after an error")
	}
}

// publishOrder is the order of the writing steps: nothing is written before
// the version check, and the draft is published only after it was drafted
// and checked.
var publishOrder = []string{
	"Refuse a version that already has a tag or a published release",
	"Push the verified images",
	"Draft the release with the verified files",
	"Publish the checked draft",
}

// publish-production writes in the order above, runs only the workflow's own
// scripts, is the only job that reads secrets, and writes the release with
// the App token from the production environment. release-commit runs only
// in this repository from main and checks the release commit.
func TestPublishWritesTheReleaseWithTheEnvironmentAppToken(t *testing.T) {
	_, wf := readWorkflow(t, "publish.yml")
	pub := wf.Jobs["publish-production"]
	order := publishOrder
	last := -1
	for _, name := range order {
		i := -1
		for k, s := range pub.Steps {
			if s.Name == name {
				i = k
			}
		}
		if i <= last {
			t.Errorf("step %q is missing or out of order", name)
		}
		last = i
	}
	for _, s := range pub.Steps {
		if strings.Contains(s.Run, "continue-on-error") || (s.Uses == "" && strings.Contains(s.Run, "hack/") &&
			!strings.Contains(s.Run, ".release-tools/hack/")) {
			t.Errorf("step %q runs a script that is not the workflow's own copy", s.Name)
		}
	}
	// Secrets are read only in publish-production, after approval, except
	// that release-commit asks whether an App key is visible outside its
	// environment.
	for id, j := range wf.Jobs {
		for _, s := range j.Steps {
			raw, _ := yaml.Marshal(s)
			if strings.Contains(string(raw), "secrets.") && id != "publish-production" &&
				s.Name != keyGuardStep {
				t.Errorf("job %s step %q reads a secret", id, s.Name)
			}
		}
	}
	appStep := -1
	for k, s := range pub.Steps {
		if strings.HasPrefix(s.Uses, "actions/create-github-app-token@") {
			appStep = k
			if s.With["client-id"] != "${{ secrets.RELEASE_APP_CLIENT_ID }}" ||
				s.With["private-key"] != "${{ secrets.RELEASE_APP_PRIVATE_KEY }}" ||
				s.With["permission-contents"] != permWrite || s.With["permission-pull-requests"] != permWrite ||
				len(s.With) != 4 {
				t.Errorf("the publishing App token inputs are %v", s.With)
			}
		}
	}
	if appStep < 0 {
		t.Fatal("publish-production mints no App token")
	}
	for _, s := range pub.Steps {
		for _, name := range []string{order[0], order[2], order[3], "Mark the release PR as released"} {
			if s.Name != name {
				continue
			}
			tok := s.Env["GITHUB_TOKEN"] + s.Env["GH_TOKEN"]
			if tok != "${{ steps.app.outputs.token }}" {
				t.Errorf("step %q writes with %q, not the App token", s.Name, tok)
			}
		}
	}
}

// release-commit runs only in this repository from main, and checks the
// commit against the release-please manifest.
func TestPublishReleaseCommitRunsOnlyFromMain(t *testing.T) {
	text, wf := readWorkflow(t, "publish.yml")
	rc := wf.Jobs["release-commit"]
	var manifestCheck bool
	for _, s := range rc.Steps {
		manifestCheck = manifestCheck || strings.Contains(s.Run, "git show \"$1:.release-please-manifest.json\"")
	}
	if !manifestCheck {
		t.Error("the release commit is not checked against the manifest")
	}
	const mainOnly = "if: github.repository == 'cubrid-lab/cubrid-kubernetes-operator' && " +
		"github.ref == 'refs/heads/main'"
	if !strings.Contains(text, mainOnly) {
		t.Error("release-commit does not require the repository and the main ref")
	}
}

// The reviewer guard passes only an environment whose every reviewer rule has
// reviewers and prevents self-review, with no admin bypass and a deployment
// branch policy.
func TestPublishRequiresAProtectedEnvironment(t *testing.T) {
	_, wf := readWorkflow(t, "publish.yml")
	var script string
	for _, s := range wf.Jobs["release-commit"].Steps {
		if s.Name == "Require reviewers on the production environment" {
			script = s.Run
		}
	}
	if script == "" {
		t.Fatal("no reviewer guard")
	}
	rule := func(n int, selfReview bool) string {
		reviewers := strings.TrimSuffix(strings.Repeat(`{"type":"User"},`, n), ",")
		return fmt.Sprintf(`{"type":"required_reviewers","prevent_self_review":%t,"reviewers":[%s]}`, selfReview, reviewers)
	}
	env := func(bypass bool, policy string, rules ...string) string {
		return fmt.Sprintf(`{"name":"production","can_admins_bypass":%t,`+
			`"deployment_branch_policy":%s,"protection_rules":[%s]}`, bypass, policy, strings.Join(rules, ","))
	}
	const policy = `{"protected_branches":false,"custom_branch_policies":true}`
	branches := func(items ...string) string {
		list := make([]string, 0, len(items))
		for _, it := range items {
			name, kind, _ := strings.Cut(it, ":")
			list = append(list, fmt.Sprintf(`{"id":1,"name":%q,"type":%q}`, name, kind))
		}
		return fmt.Sprintf(`{"total_count":%d,"branch_policies":[%s]}`, len(items), strings.Join(list, ","))
	}
	mainOnly := branches("main:branch")
	cases := []struct {
		name, body, policies string
		ok                   bool
	}{
		{"protected", env(false, policy, rule(1, true)), mainOnly, true},
		{"no rules", env(false, policy), mainOnly, false},
		{"no reviewers", env(false, policy, rule(0, true)), mainOnly, false},
		{"self-review allowed", env(false, policy, rule(1, false)), mainOnly, false},
		{"one of two rules allows self-review", env(false, policy, rule(1, true), rule(1, false)), mainOnly, false},
		{"admins bypass", env(true, policy, rule(1, true)), mainOnly, false},
		{"no branch policy", env(false, "null", rule(1, true)), mainOnly, false},
		{"protected branches instead of a list",
			env(false, `{"protected_branches":true,"custom_branch_policies":false}`, rule(1, true)), mainOnly, false},
		{"the field is missing",
			`{"protection_rules":[` + rule(1, true) + `],"deployment_branch_policy":` + policy + `}`, mainOnly, false},
		{"a branch other than main", env(false, policy, rule(1, true)), branches("release/*:branch"), false},
		{"main and another branch", env(false, policy, rule(1, true)), branches("main:branch", "dev:branch"), false},
		{"a tag named main", env(false, policy, rule(1, true)), branches("main:tag"), false},
		{"no branch at all", env(false, policy, rule(1, true)), branches(), false},
		{"policies unreadable", env(false, policy, rule(1, true)), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			body := filepath.Join(bin, "env.json")
			if err := os.WriteFile(body, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			policies := filepath.Join(bin, "policies.json")
			if err := os.WriteFile(policies, []byte(c.policies), 0o644); err != nil {
				t.Fatal(err)
			}
			// The stand-in gh answers the branch policy list from its own file
			// and fails when that answer is empty, as when it cannot be read.
			gh := "#!/bin/sh\ncase \"$2\" in\n*/deployment-branch-policies*) [ -s '" + policies +
				"' ] || exit 1; cat '" + policies + "' ;;\n*) cat '" + body + "' ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			code, out := runStepWithPath(t, bin, script, "REPOSITORY="+repository, "RUNNER_TEMP="+t.TempDir())
			if (code == 0) != c.ok {
				t.Errorf("exit %d, want ok=%t\n%s", code, c.ok, out)
			}
		})
	}
	t.Run("the environment cannot be read", func(t *testing.T) {
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if code, out := runStepWithPath(t, bin, script, "REPOSITORY="+repository, "RUNNER_TEMP="+t.TempDir()); code == 0 {
			t.Errorf("exit 0\n%s", out)
		}
	})
}

// runStepWithPath runs a step's script like runStep with bin first on PATH.
func runStepWithPath(t *testing.T, bin, script string, env ...string) (int, string) {
	t.Helper()
	return runStep(t, t.TempDir(), "export PATH='"+bin+"':\"$PATH\"\n"+script, env...)
}

// release-please only opens or updates the release PR, with the App token:
// it never creates a release, leaves the action's release-type input out so
// the repository's config and manifest are read, and GITHUB_TOKEN gets no
// permission.
func TestReleasePleaseWorkflowNeverPublishes(t *testing.T) {
	_, wf := readWorkflow(t, "release-please.yml")
	if len(wf.Permissions) != 0 {
		t.Errorf("workflow permissions %v; want none", wf.Permissions)
	}
	checkPinned(t, wf)
	j, ok := wf.Jobs["release-pr"]
	if !ok || len(wf.Jobs) != 1 {
		t.Fatalf("jobs %v; want release-pr only", wf.Jobs)
	}
	if len(j.Permissions) != 0 {
		t.Errorf("job permissions %v; want none", j.Permissions)
	}
	// The App key is a secret of an environment limited to main.
	if j.Environment != "release-pr" {
		t.Errorf("release-pr runs in environment %v; want release-pr", j.Environment)
	}
	var rp, app *step
	for i := range j.Steps {
		switch {
		case strings.HasPrefix(j.Steps[i].Uses, "googleapis/release-please-action@"):
			rp = &j.Steps[i]
		case strings.HasPrefix(j.Steps[i].Uses, "actions/create-github-app-token@"):
			app = &j.Steps[i]
		}
	}
	if rp == nil || app == nil {
		t.Fatal("no release-please step or no App token step")
	}
	if _, set := rp.With["release-type"]; set {
		t.Error("the action's release-type input is set; it would ignore the repository's config")
	}
	if rp.With["skip-github-release"] != true {
		t.Errorf("skip-github-release is %v; want true", rp.With["skip-github-release"])
	}
	if rp.With["token"] != "${{ steps.token.outputs.token }}" ||
		rp.With["config-file"] != "release-please-config.json" ||
		rp.With["manifest-file"] != ".release-please-manifest.json" {
		t.Errorf("release-please inputs %v", rp.With)
	}
	if app.With["permission-contents"] != permWrite || app.With["permission-pull-requests"] != permWrite ||
		len(app.With) != 4 {
		t.Errorf("App token inputs %v; want the credential and contents and pull-requests write only", app.With)
	}
	// The release-PR App, never the publishing App, which alone may create
	// release tags; and its key must not be visible here at all.
	if app.With["client-id"] != "${{ secrets.RELEASE_PR_APP_CLIENT_ID }}" ||
		app.With["private-key"] != "${{ secrets.RELEASE_PR_APP_PRIVATE_KEY }}" {
		t.Errorf("release-please mints a token of another App: %v", app.With)
	}
	if j.Steps[0].Env["PUBLISH_KEY_VISIBLE"] != "${{ secrets.RELEASE_APP_PRIVATE_KEY != '' }}" {
		t.Error("the release-pr job does not first refuse a visible publishing App key")
	}
	for _, c := range []struct {
		visible string
		ok      bool
	}{{strFalse, true}, {strTrue, false}, {"", false}} {
		if code, out := runStep(t, t.TempDir(), j.Steps[0].Run, "PUBLISH_KEY_VISIBLE="+c.visible); (code == 0) != c.ok {
			t.Errorf("publishing key visible %q: exit %d\n%s", c.visible, code, out)
		}
	}

	data, err := os.ReadFile(filepath.Join(repoRoot(), "release-please-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["release-type"] != "go" {
		t.Errorf("release-please-config.json release-type is %v; want go", cfg["release-type"])
	}
}
