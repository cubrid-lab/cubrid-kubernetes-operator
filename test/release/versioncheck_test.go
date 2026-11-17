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

// Package release runs hack/release-version-check.sh, the check that decides
// whether a release version may be drafted, resumed or must be refused,
// against a stand-in GitHub API (#124). No real tag or release is read or
// written.
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	repository = "cubrid-lab/cubrid-kubernetes-operator"
	version    = "v0.1.0"
	token      = "ci-secret"
	sha        = "0123456789abcdef0123456789abcdef01234567"
	otherSHA   = "89abcdef0123456789abcdef0123456789abcdef"
	notFound   = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`

	outCreate    = "state=create"
	outResume    = "state=resume"
	outUnknown   = "Could not tell"
	outPublished = "already published"
)

// answer is one stand-in response.
type answer struct {
	code int
	body string
}

// api answers the tag reference and the release list pages. A list holds the
// answers of successive lookups; its last entry repeats.
type api struct {
	mu       sync.Mutex
	ref      []answer
	pages    map[int][]answer
	seen     map[string]int
	problems []string
}

func (a *api) next(key string, list []answer) answer {
	n := a.seen[key]
	a.seen[key]++
	if len(list) == 0 {
		return answer{http.StatusTeapot, "no answer configured"}
	}
	if n >= len(list) {
		n = len(list) - 1
	}
	return list[n]
}

func (a *api) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if req.Method != http.MethodGet {
		a.problems = append(a.problems, "a "+req.Method+" request: the check only reads")
	}
	if req.Header.Get("Authorization") != "Bearer "+token {
		a.problems = append(a.problems, "a request without the token")
	}
	var ans answer
	switch req.URL.Path {
	case "/repos/" + repository + "/git/ref/tags/" + version:
		ans = a.next("ref", a.ref)
	case "/repos/" + repository + "/releases":
		page, err := strconv.Atoi(req.URL.Query().Get("page"))
		if err != nil || req.URL.Query().Get("per_page") != "100" {
			a.problems = append(a.problems, "release list request "+req.URL.RawQuery)
		}
		ans = a.next("page"+strconv.Itoa(page), a.pages[page])
		if len(a.pages[page]) == 0 {
			ans = answer{http.StatusOK, "[]"}
		}
	default:
		ans = answer{http.StatusNotFound, "unexpected path " + req.URL.Path}
		a.problems = append(a.problems, "unexpected path "+req.URL.Path)
	}
	w.WriteHeader(ans.code)
	_, _ = fmt.Fprint(w, ans.body)
}

func (a *api) count(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[key]
}

// run runs the script against url with TAG and EXPECTED_SHA and returns its
// exit code, its output and what it wrote to GITHUB_OUTPUT.
func run(t *testing.T, url, tag, expected string) (int, string, string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(here), "..", "..", "hack", "release-version-check.sh")
	outFile := filepath.Join(t.TempDir(), "github-output")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"REPOSITORY=" + repository,
		"TAG=" + tag,
		"EXPECTED_SHA=" + expected,
		"GITHUB_TOKEN=" + token,
		"API_URL=" + url,
		"ATTEMPTS=3",
		"RETRY_DELAY=0",
		"GITHUB_OUTPUT=" + outFile,
	}
	out, err := cmd.CombinedOutput()
	written, _ := os.ReadFile(outFile)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out), string(written)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out), string(written)
	default:
		t.Fatalf("run the script: %v\n%s", err, out)
		return -1, "", ""
	}
}

func release(tag string, draft bool, target string) string {
	return fmt.Sprintf(`{"id":1,"tag_name":%q,"draft":%t,"target_commitish":%q,"name":"x"}`, tag, draft, target)
}

func list(items ...string) []answer {
	return []answer{{http.StatusOK, "[" + strings.Join(items, ",") + "]"}}
}

func tagAt(target, kind string) []answer {
	return []answer{{http.StatusOK, fmt.Sprintf(
		`{"ref":"refs/tags/%s","object":{"sha":%q,"type":%q}}`, version, target, kind)}}
}

// fullPage is a page of 100 releases of other versions, so the check must ask
// for the next page.
func fullPage() []answer {
	items := make([]string, 100)
	for i := range items {
		items[i] = release(fmt.Sprintf("v0.0.%d", i+1), false, otherSHA)
	}
	return list(items...)
}

func TestReleaseVersionCheck(t *testing.T) {
	absentTag := []answer{{http.StatusNotFound, notFound}}
	cases := []struct {
		name     string
		ref      []answer
		pages    map[int][]answer
		tag      string
		expected string
		wantExit int
		wantOut  string
		// wantState is the line written to GITHUB_OUTPUT, empty for none.
		wantState string
		// wantRefTries is the number of tag lookups expected, 0 to skip.
		wantRefTries int
	}{
		{name: "new version: no tag, no release", ref: absentTag, pages: map[int][]answer{1: list()},
			wantOut: outCreate, wantState: outCreate, wantRefTries: 1},
		{name: "other versions only", ref: absentTag,
			pages:   map[int][]answer{1: list(release("v0.0.9", false, otherSHA))},
			wantOut: outCreate, wantState: outCreate},
		{name: "a rerun finds its own draft for the same commit", ref: absentTag,
			pages:   map[int][]answer{1: list(release(version, true, sha))},
			wantOut: outResume, wantState: outResume},
		{name: "the draft is on the second page", ref: absentTag,
			pages:   map[int][]answer{1: fullPage(), 2: list(release(version, true, sha))},
			wantOut: outResume, wantState: outResume},
		{name: "a published release of the version on the second page", ref: absentTag,
			pages:    map[int][]answer{1: fullPage(), 2: list(release(version, false, sha))},
			wantExit: 1, wantOut: outPublished},
		{name: "the version is already published", ref: tagAt(sha, "commit"),
			pages:    map[int][]answer{1: list(release(version, false, sha))},
			wantExit: 1, wantOut: outPublished},
		{name: "a draft of the version for another commit", ref: absentTag,
			pages:    map[int][]answer{1: list(release(version, true, otherSHA))},
			wantExit: 1, wantOut: "targets " + otherSHA},
		{name: "a draft of the version for a branch name", ref: absentTag,
			pages:    map[int][]answer{1: list(release(version, true, "main"))},
			wantExit: 1, wantOut: "targets main"},
		{name: "two drafts of the version", ref: absentTag,
			pages:    map[int][]answer{1: list(release(version, true, sha), release(version, true, sha))},
			wantExit: 1, wantOut: "2 releases"},
		{name: "the tag exists at the same commit without a release", ref: tagAt(sha, "commit"),
			pages: map[int][]answer{1: list()}, wantExit: 1, wantOut: "tag " + version + " already exists"},
		{name: "the tag exists at another commit", ref: tagAt(otherSHA, "commit"),
			pages:    map[int][]answer{1: list(release(version, true, sha))},
			wantExit: 1, wantOut: "tag " + version + " already exists"},
		{name: "an annotated tag", ref: tagAt(otherSHA, "tag"), pages: map[int][]answer{1: list()},
			wantExit: 1, wantOut: "tag " + version + " already exists"},
		{name: "tag lookup server error", ref: []answer{{http.StatusBadGateway, "bad gateway"}},
			pages: map[int][]answer{1: list()}, wantExit: 1, wantOut: outUnknown, wantRefTries: 3},
		{name: "tag lookup refused",
			ref:   []answer{{http.StatusForbidden, `{"message":"Resource not accessible by integration"}`}},
			pages: map[int][]answer{1: list()}, wantExit: 1, wantOut: outUnknown, wantRefTries: 3},
		{name: "tag 404 that is not a GitHub not-found answer", ref: []answer{{http.StatusNotFound, "<html>gone</html>"}},
			pages: map[int][]answer{1: list()}, wantExit: 1, wantOut: outUnknown},
		{name: "tag answer for another ref", ref: []answer{{http.StatusOK,
			`{"ref":"refs/tags/v0.1.0-rc","object":{"sha":"` + sha + `","type":"commit"}}`}},
			pages: map[int][]answer{1: list()}, wantExit: 1, wantOut: outUnknown},
		{name: "release list server error", ref: absentTag,
			pages: map[int][]answer{1: {{http.StatusServiceUnavailable, "unavailable"}}}, wantExit: 1, wantOut: outUnknown},
		{name: "release list is not an array", ref: absentTag,
			pages: map[int][]answer{1: {{http.StatusOK, `{"message":"x"}`}}}, wantExit: 1, wantOut: outUnknown},
		{name: "second page fails", ref: absentTag,
			pages:    map[int][]answer{1: fullPage(), 2: {{http.StatusBadGateway, "bad gateway"}}},
			wantExit: 1, wantOut: outUnknown},
		{name: "a transient error, then a new version",
			ref:   []answer{{http.StatusBadGateway, "x"}, absentTag[0]},
			pages: map[int][]answer{1: list()}, wantOut: outCreate, wantState: outCreate, wantRefTries: 2},
		{name: "a transient error, then a published version",
			ref:   []answer{{http.StatusBadGateway, "x"}, tagAt(sha, "commit")[0]},
			pages: map[int][]answer{1: list(release(version, false, sha))}, wantExit: 1, wantOut: outPublished},
		{name: "a version without the v prefix", tag: "0.1.0", ref: absentTag, pages: map[int][]answer{1: list()},
			wantExit: 2, wantOut: "not a release tag"},
		{name: "a short commit", expected: "0123456", ref: absentTag, pages: map[int][]answer{1: list()},
			wantExit: 2, wantOut: "not a full commit SHA"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &api{ref: c.ref, pages: c.pages, seen: map[string]int{}}
			srv := httptest.NewServer(a)
			t.Cleanup(func() {
				srv.Close()
				if len(a.problems) > 0 {
					t.Errorf("unexpected requests: %v", a.problems)
				}
			})
			tag, expected := c.tag, c.expected
			if tag == "" {
				tag = version
			}
			if expected == "" {
				expected = sha
			}
			code, out, written := run(t, srv.URL, tag, expected)
			if code != c.wantExit || !strings.Contains(out, c.wantOut) {
				t.Fatalf("exit %d, want %d with %q; output:\n%s", code, c.wantExit, c.wantOut, out)
			}
			if strings.TrimSpace(written) != c.wantState {
				t.Errorf("GITHUB_OUTPUT %q, want %q", written, c.wantState)
			}
			if c.wantRefTries > 0 {
				if got := a.count("ref"); got != c.wantRefTries {
					t.Errorf("tag lookups %d, want %d", got, c.wantRefTries)
				}
			}
			if strings.Contains(out, token) {
				t.Errorf("output holds the token:\n%s", out)
			}
		})
	}
}
