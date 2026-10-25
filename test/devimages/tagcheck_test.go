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

// Package devimages runs hack/dev-image-tag-check.sh, the step of the
// development image workflow that refuses to overwrite a tag, against a
// stand-in registry and packages API (#462).
package devimages

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
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	repository = "cubrid-lab/cubrid-kubernetes-operator"
	tag        = "main-0123456789abcdef0123456789abcdef01234567"
	user       = "ci-actor"
	secret     = "ci-secret"
	bearer     = "registry-bearer"
	manifest   = `{"schemaVersion":2}`
	unknownTag = `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`
	unknownPkg = `{"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known"}]}`
	denied     = `{"errors":[{"code":"DENIED","message":"requested access to the resource is denied"}]}`

	testImage  = "manager"
	outAbsent  = "is absent"
	outUnknown = "Could not tell"
)

// answer is one stand-in response.
type answer struct {
	code int
	body string
}

// registry answers the token endpoint, the manifest endpoint and the packages
// API per image. A list holds the answers of successive lookups; its last
// entry repeats.
type registry struct {
	mu       sync.Mutex
	token    map[string][]answer
	manifest map[string][]answer
	packages map[string][]answer
	seen     map[string]int
	authErr  []string
}

func newRegistry() *registry {
	return &registry{
		token:    map[string][]answer{},
		manifest: map[string][]answer{},
		packages: map[string][]answer{},
		seen:     map[string]int{},
	}
}

func (r *registry) next(kind, img string, from map[string][]answer) answer {
	key := kind + "/" + img
	n := r.seen[key]
	r.seen[key]++
	list := from[img]
	if len(list) == 0 {
		return answer{http.StatusTeapot, "no answer configured"}
	}
	if n >= len(list) {
		n = len(list) - 1
	}
	return list[n]
}

func (r *registry) count(kind, img string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[kind+"/"+img]
}

func (r *registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var a answer
	switch {
	case req.URL.Path == "/token":
		scope := req.URL.Query().Get("scope")
		img := strings.TrimSuffix(strings.TrimPrefix(scope, "repository:"+repository+"/"), ":pull")
		if u, p, ok := req.BasicAuth(); !ok || u != user || p != secret {
			r.authErr = append(r.authErr, "token request without the expected basic auth")
		}
		a = r.next("token", img, r.token)
	case strings.HasPrefix(req.URL.Path, "/v2/"+repository+"/"):
		rest := strings.TrimPrefix(req.URL.Path, "/v2/"+repository+"/")
		img, ref, _ := strings.Cut(rest, "/manifests/")
		if ref != tag {
			r.authErr = append(r.authErr, "manifest request for "+ref)
		}
		if req.Header.Get("Authorization") != "Bearer "+bearer {
			r.authErr = append(r.authErr, "manifest request without the registry token")
		}
		if req.Method == http.MethodHead {
			a = r.next("head", img, r.manifest)
			w.WriteHeader(a.code)
			return
		}
		// The script reads the body of a 404 with a GET after the HEAD: give
		// it the answer of that HEAD.
		r.seen["get/"+img]++
		if r.seen["get/"+img] > r.seen["head/"+img] {
			r.authErr = append(r.authErr, "manifest GET without a HEAD before it")
		}
		r.seen["head/"+img]--
		a = r.next("head", img, r.manifest)
	case strings.HasPrefix(req.URL.Path, "/api/orgs/cubrid-lab/packages/container/"):
		name := strings.TrimPrefix(req.URL.EscapedPath(), "/api/orgs/cubrid-lab/packages/container/")
		img := strings.TrimPrefix(name, "cubrid-kubernetes-operator%2F")
		if req.Header.Get("Authorization") != "Bearer "+secret {
			r.authErr = append(r.authErr, "packages request without the workflow token")
		}
		a = r.next("packages", img, r.packages)
	default:
		a = answer{http.StatusNotFound, "unexpected path " + req.URL.Path}
	}
	w.WriteHeader(a.code)
	_, _ = fmt.Fprint(w, a.body)
}

func tokenOK() []answer { return []answer{{http.StatusOK, `{"token":"` + bearer + `"}`}} }

// run runs the script with images against url and returns its exit code and
// output.
func run(t *testing.T, url string, images ...string) (int, string) {
	t.Helper()
	return runWith(t, url, "", images...)
}

// runWith is run with BOOTSTRAP_IMAGES set to bootstrap.
func runWith(t *testing.T, url, bootstrap string, images ...string) (int, string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(here), "..", "..", "hack", "dev-image-tag-check.sh")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"REPOSITORY=" + repository,
		"IMAGES=" + strings.Join(images, " "),
		"TAG=" + tag,
		"REGISTRY_USER=" + user,
		"REGISTRY_TOKEN=" + secret,
		"REGISTRY_URL=" + url,
		"API_URL=" + url + "/api",
		"ATTEMPTS=3",
		"RETRY_DELAY=0",
		"BOOTSTRAP_IMAGES=" + bootstrap,
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	default:
		t.Fatalf("run the script: %v\n%s", err, out)
		return -1, ""
	}
}

func serve(t *testing.T, r *registry) string {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(func() {
		srv.Close()
		if len(r.authErr) > 0 {
			t.Errorf("unexpected requests: %v", r.authErr)
		}
	})
	return srv.URL
}

func TestTagCheck(t *testing.T) {
	type tc struct {
		name     string
		token    []answer
		manifest []answer
		packages []answer
		// bootstrap is BOOTSTRAP_IMAGES.
		bootstrap string
		wantExit  int
		wantOut   string
		// wantTries is the number of token requests expected, 0 to skip.
		wantTries int
	}
	cases := []tc{
		{name: "existing tag", token: tokenOK(), manifest: []answer{{200, manifest}},
			wantExit: 1, wantOut: "already exists", wantTries: 1},
		{name: "absent tag", token: tokenOK(), manifest: []answer{{404, unknownTag}},
			wantExit: 0, wantOut: outAbsent},
		{name: "absent package in the registry", token: tokenOK(), manifest: []answer{{404, unknownPkg}},
			wantExit: 0, wantOut: outAbsent},
		{name: "first publication: token denied, package not found", bootstrap: testImage,
			token: []answer{{403, denied}}, packages: []answer{{404, `{"message":"Package not found."}`}},
			wantExit: 0, wantOut: outAbsent},
		{name: "token denied, package not found, image not declared new",
			token: []answer{{403, denied}}, packages: []answer{{404, `{"message":"Package not found."}`}},
			wantExit: 1, wantOut: outUnknown, wantTries: 3},
		{name: "token denied for a declared new package that exists", bootstrap: "other " + testImage,
			token: []answer{{403, denied}}, packages: []answer{{200, `{"name":"x"}`}},
			wantExit: 1, wantOut: outUnknown, wantTries: 3},
		{name: "token denied, packages API refuses", bootstrap: testImage,
			token: []answer{{403, denied}}, packages: []answer{{403, `{"message":"Resource not accessible by integration"}`}},
			wantExit: 1, wantOut: outUnknown, wantTries: 3},
		{name: "image left in the bootstrap list after its package exists", bootstrap: testImage,
			token: tokenOK(), manifest: []answer{{404, unknownTag}},
			wantExit: 1, wantOut: "remove manager from BOOTSTRAP_IMAGES", wantTries: 1},
		{name: "bootstrap image whose package the registry does not know", bootstrap: testImage,
			token: tokenOK(), manifest: []answer{{404, unknownPkg}},
			wantExit: 0, wantOut: outAbsent},
		{name: "token 401 without DENIED", bootstrap: testImage, token: []answer{{401, `unauthorized`}},
			packages: []answer{{404, `{}`}}, wantExit: 1, wantOut: outUnknown},
		{name: "token endpoint unavailable", token: []answer{{503, `unavailable`}},
			wantExit: 1, wantOut: "answered 503", wantTries: 3},
		{name: "token answer without a token", token: []answer{{200, `{}`}},
			wantExit: 1, wantOut: "has no token"},
		{name: "manifest server error", token: tokenOK(), manifest: []answer{{503, `unavailable`}},
			wantExit: 1, wantOut: outUnknown, wantTries: 3},
		{name: "manifest unauthorized", token: tokenOK(), manifest: []answer{{401, denied}},
			wantExit: 1, wantOut: outUnknown},
		{name: "404 without a registry error code", token: tokenOK(), manifest: []answer{{404, `<html>not found</html>`}},
			wantExit: 1, wantOut: "without a recognized error code"},
		{name: "a transient error, then absent", token: tokenOK(),
			manifest: []answer{{502, `bad gateway`}, {404, unknownTag}},
			wantExit: 0, wantOut: outAbsent, wantTries: 2},
		{name: "a transient error, then present", token: tokenOK(),
			manifest: []answer{{502, `bad gateway`}, {200, manifest}},
			wantExit: 1, wantOut: "already exists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRegistry()
			r.token[testImage] = c.token
			r.manifest[testImage] = c.manifest
			r.packages[testImage] = c.packages
			code, out := runWith(t, serve(t, r), c.bootstrap, testImage)
			if code != c.wantExit || !strings.Contains(out, c.wantOut) {
				t.Fatalf("exit %d, want %d with %q; output:\n%s", code, c.wantExit, c.wantOut, out)
			}
			if c.wantTries > 0 {
				if got := r.count("token", testImage); got != c.wantTries {
					t.Errorf("token requests %d, want %d", got, c.wantTries)
				}
			}
			// The registry token appears only in the runner's mask command.
			shown := strings.ReplaceAll(out, "::add-mask::"+bearer, "")
			if strings.Contains(shown, secret) || strings.Contains(shown, bearer) {
				t.Errorf("output holds a credential:\n%s", out)
			}
		})
	}
}

// Every image is checked: an absent first image does not let an existing
// second one through, and an unknown answer for the last one stops the run.
func TestTagCheckChecksEveryImage(t *testing.T) {
	r := newRegistry()
	for _, img := range []string{"manager", "instance-manager", "workload-client"} {
		r.token[img] = tokenOK()
		r.manifest[img] = []answer{{404, unknownTag}}
	}
	r.manifest["instance-manager"] = []answer{{200, manifest}}
	code, out := run(t, serve(t, r), "manager", "instance-manager", "workload-client")
	if code != 1 || !strings.Contains(out, "instance-manager:"+tag+" already exists") {
		t.Fatalf("exit %d; output:\n%s", code, out)
	}

	r = newRegistry()
	for _, img := range []string{"manager", "instance-manager", "workload-client"} {
		r.token[img] = tokenOK()
		r.manifest[img] = []answer{{404, unknownTag}}
	}
	r.manifest["workload-client"] = []answer{{500, `oops`}}
	code, out = run(t, serve(t, r), "manager", "instance-manager", "workload-client")
	if code != 1 || !strings.Contains(out, "workload-client:"+tag+" exists after 3 attempts") {
		t.Fatalf("exit %d; output:\n%s", code, out)
	}
}

// A registry that does not answer at all is not absence.
func TestTagCheckNoAnswer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	code, out := run(t, url, testImage)
	if code != 1 || !strings.Contains(out, outUnknown) {
		t.Fatalf("exit %d; output:\n%s", code, out)
	}
}

// The workflow runs this script, taken from the workflow's own commit, with the
// three images, before its first push.
func TestWorkflowRunsTheCheckBeforeAnyPush(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")
	data, err := os.ReadFile(filepath.Join(repo, ".github", "workflows", "dev-images.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wf := string(data)
	clone := strings.Index(wf, "ref: ${{ github.workflow_sha }}")
	if clone < 0 || !strings.Contains(wf, "path: .tag-check") ||
		!strings.Contains(wf, "sparse-checkout: hack/dev-image-tag-check.sh") {
		t.Error("the check is not cloned from the workflow's own commit into .tag-check")
	}
	check := strings.Index(wf, "run: .tag-check/hack/dev-image-tag-check.sh")
	if clone > check {
		t.Error("the check runs before it is cloned")
	}
	push := strings.Index(wf, "push: true")
	if check < 0 || push < 0 || check > push {
		t.Fatalf("check at %d, first push at %d", check, push)
	}
	if !strings.Contains(wf, "IMAGES: manager instance-manager workload-client") {
		t.Error("the check does not name all three images")
	}
	if strings.Contains(wf, "imagetools inspect") {
		t.Error("the workflow still decides absence from imagetools inspect")
	}
	// The step runs the script directly, not through bash.
	info, err := os.Stat(filepath.Join(repo, "hack", "dev-image-tag-check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("hack/dev-image-tag-check.sh is not executable")
	}
}
