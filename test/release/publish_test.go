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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hack/release-publish.sh drafts the GitHub Release with the verified files
// and publishes it only after the draft holds exactly those files (#124).
// These tests run it against a stand-in GitHub API that keeps releases, their
// files and tags, and that can fail a request part-way.

type ghAsset struct {
	ID   int
	Name string
	Data []byte
}

type ghRelease struct {
	ID     int
	Tag    string
	Draft  bool
	Target string
	Body   string
	Assets []*ghAsset
}

// github is the stand-in API. Fields named fail* make one request fail once.
type github struct {
	mu       sync.Mutex
	releases []*ghRelease
	tags     map[string]string
	nextID   int
	writes   []string
	problems []string

	// failCreateAfterApply makes the release POST create the draft but
	// answer 502, as when the answer is lost.
	failCreateAfterApply bool
	// failUpload names a file whose upload answers 500 once.
	failUpload string
	// failPublishAfterApply makes the publishing PATCH apply but answer 502.
	failPublishAfterApply bool
}

func newGitHub() *github { return &github{tags: map[string]string{}, nextID: 100} }

func (g *github) id() int { g.nextID++; return g.nextID }

func (g *github) release(id int) *ghRelease {
	for _, r := range g.releases {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (g *github) releaseJSON(r *ghRelease) map[string]any {
	return map[string]any{"id": r.ID, "tag_name": r.Tag, "draft": r.Draft, "target_commitish": r.Target,
		"name": r.Tag, "body": r.Body}
}

func reply(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	if s, ok := v.(string); ok {
		_, _ = io.WriteString(w, s)
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

var (
	reRelease      = regexp.MustCompile(`^/repos/` + repository + `/releases/(\d+)$`)
	reAssets       = regexp.MustCompile(`^/repos/` + repository + `/releases/(\d+)/assets$`)
	reAsset        = regexp.MustCompile(`^/repos/` + repository + `/releases/assets/(\d+)$`)
	reUpload       = regexp.MustCompile(`^/upload/repos/` + repository + `/releases/(\d+)/assets$`)
	tagRefPrefix   = "/repos/" + repository + "/git/ref/tags/"
	releasesPrefix = "/repos/" + repository + "/releases"
)

func (g *github) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.Header.Get("Authorization") != "Bearer "+token {
		g.problems = append(g.problems, "a request without the token: "+req.Method+" "+req.URL.Path)
	}
	if req.Method != http.MethodGet {
		g.writes = append(g.writes, req.Method+" "+req.URL.Path)
	}
	body, _ := io.ReadAll(req.Body)
	p := req.URL.Path
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch {
	case req.Method == http.MethodGet && strings.HasPrefix(p, tagRefPrefix):
		tag := strings.TrimPrefix(p, tagRefPrefix)
		if target, ok := g.tags[tag]; ok {
			reply(w, 200, map[string]any{"ref": "refs/tags/" + tag, "object": map[string]any{"sha": target, "type": "commit"}})
			return
		}
		reply(w, 404, notFound)
	case req.Method == http.MethodGet && p == releasesPrefix:
		list := []any{}
		if req.URL.Query().Get("page") == "1" {
			for _, r := range g.releases {
				list = append(list, g.releaseJSON(r))
			}
		}
		reply(w, 200, list)
	case req.Method == http.MethodPost && p == releasesPrefix:
		var in struct {
			Tag    string `json:"tag_name"`
			Target string `json:"target_commitish"`
			Body   string `json:"body"`
			Draft  bool   `json:"draft"`
		}
		if err := json.Unmarshal(body, &in); err != nil || !in.Draft {
			g.problems = append(g.problems, "a release created without draft: "+string(body))
		}
		r := &ghRelease{ID: g.id(), Tag: in.Tag, Draft: in.Draft, Target: in.Target, Body: in.Body}
		g.releases = append(g.releases, r)
		if g.failCreateAfterApply {
			g.failCreateAfterApply = false
			reply(w, 502, "bad gateway")
			return
		}
		reply(w, 201, g.releaseJSON(r))
	case reRelease.MatchString(p):
		r := g.release(atoi(reRelease.FindStringSubmatch(p)[1]))
		if r == nil {
			reply(w, 404, notFound)
			return
		}
		if req.Method == http.MethodPatch {
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			if !r.Draft {
				g.problems = append(g.problems, "a published release was edited")
			}
			if b, ok := in["body"].(string); ok {
				r.Body = b
			}
			if d, ok := in["draft"].(bool); ok && !d {
				r.Draft = false
				if _, ok := g.tags[r.Tag]; !ok {
					g.tags[r.Tag] = r.Target
				}
				if g.failPublishAfterApply {
					g.failPublishAfterApply = false
					reply(w, 502, "bad gateway")
					return
				}
			}
		}
		reply(w, 200, g.releaseJSON(r))
	case req.Method == http.MethodGet && reAssets.MatchString(p):
		r := g.release(atoi(reAssets.FindStringSubmatch(p)[1]))
		list := []any{}
		for _, a := range r.Assets {
			list = append(list, map[string]any{"id": a.ID, "name": a.Name, "state": "uploaded", "size": len(a.Data)})
		}
		reply(w, 200, list)
	case reAsset.MatchString(p):
		aid := atoi(reAsset.FindStringSubmatch(p)[1])
		for _, r := range g.releases {
			for i, a := range r.Assets {
				if a.ID != aid {
					continue
				}
				if req.Method == http.MethodDelete {
					if !r.Draft {
						g.problems = append(g.problems, "a file of a published release was deleted")
					}
					r.Assets = append(r.Assets[:i], r.Assets[i+1:]...)
					w.WriteHeader(204)
					return
				}
				if req.Header.Get("Accept") != "application/octet-stream" {
					g.problems = append(g.problems, "an asset download without the octet-stream Accept")
				}
				w.WriteHeader(200)
				_, _ = w.Write(a.Data)
				return
			}
		}
		reply(w, 404, notFound)
	case req.Method == http.MethodPost && reUpload.MatchString(p):
		r := g.release(atoi(reUpload.FindStringSubmatch(p)[1]))
		name := req.URL.Query().Get("name")
		if r == nil || !r.Draft {
			g.problems = append(g.problems, "a file attached to a release that is not a draft")
			reply(w, 422, "{}")
			return
		}
		if name == g.failUpload {
			g.failUpload = ""
			reply(w, 500, "upload failed")
			return
		}
		r.Assets = append(r.Assets, &ghAsset{ID: g.id(), Name: name, Data: body})
		reply(w, 201, map[string]any{"id": r.Assets[len(r.Assets)-1].ID, "name": name})
	default:
		g.problems = append(g.problems, "unexpected request "+req.Method+" "+p)
		reply(w, 404, notFound)
	}
}

func (g *github) snapshot() (releases []ghRelease, tags map[string]string, writes []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.releases {
		releases = append(releases, *r)
	}
	tags = map[string]string{}
	for k, v := range g.tags {
		tags[k] = v
	}
	return releases, tags, append([]string(nil), g.writes...)
}

// assetDir writes the verified files: install.yaml and SHA256SUMS.
func assetDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "install.yaml"), []byte("kind: Namespace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sha256sum install.yaml > SHA256SUMS")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return dir
}

func runScript(t *testing.T, script string, args []string, env ...string) (int, string) {
	t.Helper()
	for _, tool := range []string{"curl", "jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is not installed")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(repoRoot(), "hack", script)}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}, env...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	default:
		t.Fatalf("run %s: %v\n%s", script, err, out)
		return -1, ""
	}
}

func publish(t *testing.T, srv *httptest.Server, mode, assets string) (int, string) {
	t.Helper()
	notes := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(notes, []byte("### Features\n\n* first release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return runScript(t, "release-publish.sh", []string{mode},
		"REPOSITORY="+repository, "TAG="+version, "EXPECTED_SHA="+sha, "GITHUB_TOKEN="+token,
		"API_URL="+srv.URL, "UPLOAD_URL="+srv.URL+"/upload", "ASSET_DIR="+assets, "NOTES_FILE="+notes,
		"ATTEMPTS=1", "RETRY_DELAY=0")
}

func requireClean(t *testing.T, g *github) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.problems) > 0 {
		t.Errorf("unexpected requests: %v", g.problems)
	}
}

// A first attempt drafts the release with the verified files and publishes
// it; GitHub's tag then points at the release commit. Running either step
// again is refused and writes nothing.
func TestPublishDraftsThenPublishes(t *testing.T) {
	g := newGitHub()
	srv := httptest.NewServer(g)
	defer srv.Close()
	assets := assetDir(t)

	if code, out := publish(t, srv, "draft", assets); code != 0 {
		t.Fatalf("draft: exit %d\n%s", code, out)
	}
	rels, tags, _ := g.snapshot()
	if len(rels) != 1 || !rels[0].Draft || rels[0].Target != sha || len(rels[0].Assets) != 2 || len(tags) != 0 {
		t.Fatalf("after the draft: releases %+v, tags %v", rels, tags)
	}
	if !strings.Contains(rels[0].Body, "first release") {
		t.Errorf("the draft's notes are %q", rels[0].Body)
	}
	if code, out := publish(t, srv, "publish", assets); code != 0 {
		t.Fatalf("publish: exit %d\n%s", code, out)
	}
	rels, tags, writes := g.snapshot()
	if len(rels) != 1 || rels[0].Draft || tags[version] != sha {
		t.Fatalf("after publishing: releases %+v, tags %v", rels, tags)
	}
	for _, mode := range []string{"draft", "publish"} {
		code, out := publish(t, srv, mode, assets)
		if code == 0 || !strings.Contains(out, outPublished) {
			t.Errorf("%s again after publication: exit %d\n%s", mode, code, out)
		}
	}
	if _, _, again := g.snapshot(); len(again) != len(writes) {
		t.Errorf("a refused rerun wrote: %v", again[len(writes):])
	}
	requireClean(t, g)
}

// A draft attempt that fails part-way publishes nothing, and a publish step
// started anyway refuses the incomplete draft without changing it. The rerun
// resumes the same draft, never a second one, and then publication succeeds.
func TestPublishRerunAfterAPartWayFailure(t *testing.T) {
	g := newGitHub()
	g.failUpload = "SHA256SUMS"
	srv := httptest.NewServer(g)
	defer srv.Close()
	assets := assetDir(t)

	code, out := publish(t, srv, "draft", assets)
	if code == 0 || !strings.Contains(out, "Could not attach SHA256SUMS") {
		t.Fatalf("a failed upload: exit %d\n%s", code, out)
	}
	_, _, before := g.snapshot()
	code, out = publish(t, srv, "publish", assets)
	if code == 0 || !strings.Contains(out, "not the verified files") {
		t.Errorf("publishing an incomplete draft: exit %d\n%s", code, out)
	}
	rels, tags, after := g.snapshot()
	if len(after) != len(before) || len(tags) != 0 || !rels[0].Draft {
		t.Fatalf("the refused publication wrote %v; tags %v", after[len(before):], tags)
	}

	if code, out := publish(t, srv, "draft", assets); code != 0 || !strings.Contains(out, outResume) {
		t.Fatalf("the rerun: exit %d\n%s", code, out)
	}
	if code, out := publish(t, srv, "publish", assets); code != 0 {
		t.Fatalf("publish after the rerun: exit %d\n%s", code, out)
	}
	rels, tags, _ = g.snapshot()
	if len(rels) != 1 || rels[0].Draft || len(rels[0].Assets) != 2 || tags[version] != sha {
		t.Errorf("after the rerun: releases %+v, tags %v", rels, tags)
	}
	requireClean(t, g)
}

// When the answer to the draft's creation is lost, the rerun finds and
// resumes the one draft the request made.
func TestPublishLostCreateAnswerIsResumed(t *testing.T) {
	g := newGitHub()
	g.failCreateAfterApply = true
	srv := httptest.NewServer(g)
	defer srv.Close()
	assets := assetDir(t)
	if code, out := publish(t, srv, "draft", assets); code == 0 {
		t.Fatalf("a lost answer: exit 0\n%s", out)
	}
	if code, out := publish(t, srv, "draft", assets); code != 0 || !strings.Contains(out, outResume) {
		t.Fatalf("the rerun: exit %d\n%s", code, out)
	}
	if rels, _, _ := g.snapshot(); len(rels) != 1 {
		t.Errorf("%d releases; want the one draft", len(rels))
	}
	requireClean(t, g)
}

// When the answer to the publication is lost after GitHub applied it, what
// GitHub then shows decides.
func TestPublishLostPublishAnswerIsConfirmed(t *testing.T) {
	g := newGitHub()
	g.failPublishAfterApply = true
	srv := httptest.NewServer(g)
	defer srv.Close()
	assets := assetDir(t)
	if code, out := publish(t, srv, "draft", assets); code != 0 {
		t.Fatalf("draft: exit %d\n%s", code, out)
	}
	if code, out := publish(t, srv, "publish", assets); code != 0 || !strings.Contains(out, "Published "+version) {
		t.Fatalf("publish with a lost answer: exit %d\n%s", code, out)
	}
	requireClean(t, g)
}

// A version that exists is refused before anything is written: a published
// release, a tag without a release, or a draft for another commit.
func TestPublishRefusesAnExistingVersion(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(g *github)
		wantOut string
	}{
		{"a published release", func(g *github) {
			g.releases = append(g.releases, &ghRelease{ID: 1, Tag: version, Target: sha})
			g.tags[version] = sha
		}, outPublished},
		{"a tag without a release", func(g *github) { g.tags[version] = otherSHA }, "already exists"},
		{"a draft for another commit", func(g *github) {
			g.releases = append(g.releases, &ghRelease{ID: 1, Tag: version, Draft: true, Target: otherSHA})
		}, "not the candidate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGitHub()
			c.setup(g)
			srv := httptest.NewServer(g)
			defer srv.Close()
			assets := assetDir(t)
			for _, mode := range []string{"draft", "publish"} {
				code, out := publish(t, srv, mode, assets)
				if code == 0 || !strings.Contains(out, c.wantOut) {
					t.Errorf("%s: exit %d, want %q\n%s", mode, code, c.wantOut, out)
				}
			}
			if _, _, writes := g.snapshot(); len(writes) != 0 {
				t.Errorf("a refused version was written: %v", writes)
			}
			requireClean(t, g)
		})
	}
}

// A draft whose file was changed after it was checked is not published.
func TestPublishRefusesAChangedDraft(t *testing.T) {
	g := newGitHub()
	srv := httptest.NewServer(g)
	defer srv.Close()
	assets := assetDir(t)
	if code, out := publish(t, srv, "draft", assets); code != 0 {
		t.Fatalf("draft: exit %d\n%s", code, out)
	}
	g.mu.Lock()
	for _, a := range g.releases[0].Assets {
		if a.Name == "install.yaml" {
			a.Data = []byte("kind: Secret\n")
		}
	}
	g.mu.Unlock()
	code, out := publish(t, srv, "publish", assets)
	if code == 0 || !strings.Contains(out, "is not the verified file") {
		t.Errorf("publishing a changed draft: exit %d\n%s", code, out)
	}
	if rels, tags, _ := g.snapshot(); !rels[0].Draft || len(tags) != 0 {
		t.Error("a changed draft was published")
	}
	requireClean(t, g)
}

// Files that do not match SHA256SUMS are refused before any request.
func TestPublishRefusesUnverifiedFiles(t *testing.T) {
	g := newGitHub()
	srv := httptest.NewServer(g)
	defer srv.Close()
	assets := assetDir(t)
	if err := os.WriteFile(filepath.Join(assets, "install.yaml"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := publish(t, srv, "draft", assets); code == 0 || !strings.Contains(out, "do not match SHA256SUMS") {
		t.Errorf("unverified files: exit %d\n%s", code, out)
	}
	if _, _, writes := g.snapshot(); len(writes) != 0 {
		t.Errorf("writes: %v", writes)
	}
}

// hack/release-push-images.sh pushes the verified archives and never puts
// other bytes under an existing tag. The stand-in registry keeps the tags a
// stand-in skopeo pushed; the stand-in skopeo pushes the digest written next
// to each archive.

type registry struct {
	mu       sync.Mutex
	dir      string // <image>_<tag> files holding a digest
	fail500  bool
	needAuth bool
	tokens   int
}

func (r *registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Path == "/token" {
		user, pass, ok := req.BasicAuth()
		if !ok || user != "ci" || pass != token {
			reply(w, 401, "{}")
			return
		}
		r.tokens++
		reply(w, 200, map[string]string{"token": "pull-token"})
		return
	}
	m := regexp.MustCompile(`^/v2/` + repository + `/([a-z-]+)/manifests/(.+)$`).FindStringSubmatch(req.URL.Path)
	if m == nil {
		reply(w, 404, "{}")
		return
	}
	if r.needAuth && req.Header.Get("Authorization") != "Bearer pull-token" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/token",service="test"`, req.Host))
		reply(w, 401, `{"errors":[{"code":"UNAUTHORIZED"}]}`)
		return
	}
	if r.fail500 {
		reply(w, 500, "oops")
		return
	}
	data, err := os.ReadFile(filepath.Join(r.dir, m[1]+"_"+m[2]))
	if err != nil {
		reply(w, 404, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)
		return
	}
	w.Header().Set("Docker-Content-Digest", strings.TrimSpace(string(data)))
	reply(w, 200, "{}")
}

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

type pushEnv struct {
	reg      *registry
	srv      *httptest.Server
	archives string
	record   string
	pushLog  string
	bin      string
}

// newPushEnv writes two archives, a record of their digests and checksums,
// and a stand-in skopeo that records its pushes in the registry's directory.
func newPushEnv(t *testing.T) *pushEnv {
	t.Helper()
	e := &pushEnv{reg: &registry{dir: t.TempDir()}, archives: t.TempDir(), bin: t.TempDir()}
	e.srv = httptest.NewServer(e.reg)
	t.Cleanup(e.srv.Close)
	e.pushLog = filepath.Join(t.TempDir(), "pushes")
	type img struct {
		Name             string `json:"name"`
		Digest           string `json:"digest"`
		OciArchiveSha256 string `json:"ociArchiveSha256"`
	}
	var images []img
	for i, name := range []string{"manager", "instance-manager"} {
		archive := filepath.Join(e.archives, name+".oci.tar")
		if err := os.WriteFile(archive, []byte("archive of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		d := digestOf("ab"[i])
		if err := os.WriteFile(archive+".digest", []byte(d), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("sha256sum", archive).Output()
		if err != nil {
			t.Skip("sha256sum is not installed")
		}
		images = append(images, img{name, d, strings.Fields(string(out))[0]})
	}
	data, _ := json.Marshal(map[string]any{"images": images})
	e.record = filepath.Join(t.TempDir(), "release-dry-run.json")
	if err := os.WriteFile(e.record, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// skopeo copy [flags] oci-archive:<path> docker://<host>/<repo>/<image>:<tag>
	skopeo := `#!/bin/bash
printf '%s\n' "$*" >> '` + e.pushLog + `'
src=${@: -2:1}; dst=${@: -1}
src=${src#oci-archive:}; ref=${dst##*/}
[ -e "$src.fail" ] && exit 1
cp "$src.digest" '` + e.reg.dir + `'/"${ref%%:*}_${ref#*:}"
`
	if err := os.WriteFile(filepath.Join(e.bin, "skopeo"), []byte(skopeo), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *pushEnv) run(t *testing.T, extra ...string) (int, string) {
	t.Helper()
	env := append([]string{
		"PATH=" + e.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"REGISTRY_HOST=" + strings.TrimPrefix(e.srv.URL, "http://"),
		"REPOSITORY=" + repository, "IMAGES=manager instance-manager", "TAG=" + version,
		"RECORD=" + e.record, "ARCHIVE_DIR=" + e.archives, "INSECURE=true",
		"ATTEMPTS=2", "RETRY_DELAY=0",
	}, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(repoRoot(), "hack", "release-push-images.sh"))
	cmd.Env = env
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	default:
		t.Fatalf("run: %v\n%s", err, out)
		return -1, ""
	}
}

func (e *pushEnv) pushes() []string {
	data, _ := os.ReadFile(e.pushLog)
	return strings.Fields(strings.ReplaceAll(string(data), "\n", " "))
}

func (e *pushEnv) pushed(image string) bool {
	for _, f := range e.pushes() {
		if strings.HasSuffix(f, "/"+image+":"+version) {
			return true
		}
	}
	return false
}

func (e *pushEnv) resetLog() { _ = os.Remove(e.pushLog) }

// A push that fails after the first image leaves only that image; the rerun
// pushes only the missing one, and a further rerun pushes nothing.
func TestPushImagesRerunAfterAPartWayFailure(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	e := newPushEnv(t)
	failMarker := filepath.Join(e.archives, "instance-manager.oci.tar.fail")
	if err := os.WriteFile(failMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := e.run(t); code == 0 || !e.pushed("manager") {
		t.Fatalf("a failing second push: exit %d\n%s", code, out)
	}
	_ = os.Remove(failMarker)
	e.resetLog()
	code, out := e.run(t)
	if code != 0 || e.pushed("manager") || !e.pushed("instance-manager") {
		t.Fatalf("the rerun: exit %d, pushes %v\n%s", code, e.pushes(), out)
	}
	e.resetLog()
	if code, out := e.run(t); code != 0 || len(e.pushes()) != 0 || strings.Count(out, "not pushed again") != 2 {
		t.Errorf("a further rerun: exit %d, pushes %v\n%s", code, e.pushes(), out)
	}
}

// A tag that holds other bytes, an archive that is not the recorded one or a
// registry that cannot answer stops the run before the first push.
func TestPushImagesRefusesBeforeAnyPush(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	t.Run("a tag with other bytes", func(t *testing.T) {
		e := newPushEnv(t)
		// The second image's tag is taken, so the first is not pushed either.
		if err := os.WriteFile(filepath.Join(e.reg.dir, "instance-manager_"+version), []byte(digestOf('c')), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out := e.run(t)
		if code == 0 || !strings.Contains(out, "already holds "+digestOf('c')) || len(e.pushes()) != 0 {
			t.Errorf("exit %d, pushes %v\n%s", code, e.pushes(), out)
		}
	})
	t.Run("an archive that is not the recorded one", func(t *testing.T) {
		e := newPushEnv(t)
		if err := os.WriteFile(filepath.Join(e.archives, "instance-manager.oci.tar"), []byte("other"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out := e.run(t)
		if code == 0 || !strings.Contains(out, "not the one the verified build recorded") || len(e.pushes()) != 0 {
			t.Errorf("exit %d, pushes %v\n%s", code, e.pushes(), out)
		}
	})
	t.Run("a registry error", func(t *testing.T) {
		e := newPushEnv(t)
		e.reg.fail500 = true
		code, out := e.run(t)
		if code == 0 || !strings.Contains(out, "Could not tell") || len(e.pushes()) != 0 {
			t.Errorf("exit %d, pushes %v\n%s", code, e.pushes(), out)
		}
	})
}

// With credentials, a registry that asks for a token gets the credentials
// only at its own token endpoint, and they appear on no command line.
func TestPushImagesUsesARegistryToken(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	e := newPushEnv(t)
	e.reg.needAuth = true
	code, out := e.run(t, "REGISTRY_USER=ci", "REGISTRY_TOKEN="+token)
	if code != 0 || !e.pushed("manager") || !e.pushed("instance-manager") {
		t.Fatalf("exit %d, pushes %v\n%s", code, e.pushes(), out)
	}
	if strings.Contains(strings.Join(e.pushes(), " "), token) || strings.Contains(out, token) {
		t.Error("the registry token is on a command line or in the output")
	}
	if !strings.Contains(strings.Join(e.pushes(), " "), "--dest-authfile") {
		t.Error("skopeo is not given the credentials in a file")
	}
}
