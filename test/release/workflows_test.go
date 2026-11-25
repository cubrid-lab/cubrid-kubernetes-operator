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
	Steps       []step            `yaml:"steps"`
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
			if level == "write" && id != "publish-production" {
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
	want := map[string]string{"contents": "write", "packages": "write", "pull-requests": "write"}
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
	if verify.Permissions["contents"] != "read" || len(verify.Permissions) != 1 {
		t.Errorf("verify permissions %v; want contents: read", verify.Permissions)
	}
	// The order of the writing steps: nothing is written before the version
	// check, and the draft is published only after it was drafted and checked.
	order := []string{
		"Refuse a version that already has a tag or a published release",
		"Push the verified images",
		"Draft the release with the verified files",
		"Publish the checked draft",
	}
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
	if strings.Contains(text, "continue-on-error") || strings.Contains(text, "secrets.") {
		t.Error("publish.yml continues after an error or reads a secret")
	}
	if !strings.Contains(wf.Jobs["release-commit"].Steps[len(wf.Jobs["release-commit"].Steps)-2].Run,
		".release-please-manifest.json") {
		t.Error("the release commit is not checked against the manifest")
	}
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
	if app.With["permission-contents"] != "write" || app.With["permission-pull-requests"] != "write" ||
		len(app.With) != 4 {
		t.Errorf("App token inputs %v; want the credential and contents and pull-requests write only", app.With)
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
