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
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func repoRoot() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..")
}

// dockerfiles returns the repository's Dockerfiles, relative to its root.
func dockerfiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot()
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "vendor", "node_modules", ".claude", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), "Dockerfile") {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Every image that downloads Go modules does so through
// build/go-mod-download.sh, so a download that the Go proxy cuts off is
// retried (#544).
func TestDockerfiles_DownloadModulesWithRetries(t *testing.T) {
	images := 0
	for _, name := range dockerfiles(t) {
		data, err := os.ReadFile(filepath.Join(repoRoot(), name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, "go mod download") {
			t.Errorf("%s runs go mod download itself, without retries", name)
		}
		if !strings.Contains(text, "go-mod-download.sh") {
			continue
		}
		images++
		if !strings.Contains(text, "COPY build/go-mod-download.sh /usr/local/bin/go-mod-download.sh\n") ||
			!strings.Contains(text, "RUN sh /usr/local/bin/go-mod-download.sh\n") {
			t.Errorf("%s does not copy build/go-mod-download.sh and run it with sh", name)
		}
	}
	// The manager, the Instance Manager and the fake Instance Manager.
	if images < 3 {
		t.Errorf("%d images download modules through build/go-mod-download.sh, want at least 3", images)
	}
	ignore, err := os.ReadFile(filepath.Join(repoRoot(), ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^!build/go-mod-download\.sh$`).Match(ignore) {
		t.Error(".dockerignore leaves build/go-mod-download.sh out of the build context")
	}
}

// goModStandIn fails "go mod download" until it has been called $SUCCEED_ON
// times (never when it is 0), and counts the calls in $CALLS.
const goModStandIn = `#!/bin/sh
echo "$*" >> "$CALLS"
n=$(wc -l < "$CALLS")
if [ "$SUCCEED_ON" -ne 0 ] && [ "$n" -ge "$SUCCEED_ON" ]; then
	exit 0
fi
echo 'go: k8s.io/api@v0.37.1: read "https://proxy.golang.org/...": stream error: INTERNAL_ERROR' >&2
exit 1
`

func runGoModDownload(t *testing.T, succeedOn int, env ...string) (code, calls int, out string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goModStandIn), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	// A run without a bound fails the test instead of hanging it.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(repoRoot(), "build", "go-mod-download.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CALLS="+log, "SUCCEED_ON="+strconv.Itoa(succeedOn), "GO_MOD_DOWNLOAD_BACKOFF=0")
	cmd.Env = append(cmd.Env, env...)
	output, err := cmd.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			if line != "mod download" {
				t.Errorf("go was called with %q, want mod download", line)
			}
			calls++
		}
	}
	return code, calls, string(output)
}

// A download that fails and then succeeds passes; one that keeps failing,
// as a module that does not match go.sum does, fails after the last attempt.
func TestGoModDownload_RetriesABoundedNumberOfTimes(t *testing.T) {
	if code, calls, out := runGoModDownload(t, 2); code != 0 || calls != 2 {
		t.Errorf("one cut-off download: exit %d after %d calls, want 0 after 2\n%s", code, calls, out)
	}
	code, calls, out := runGoModDownload(t, 0)
	if code != 1 || calls != 3 || !strings.Contains(out, "go mod download failed 3 times; giving up") {
		t.Errorf("a download that keeps failing: exit %d after %d calls, want 1 after 3, giving up\n%s", code, calls, out)
	}
	if !strings.Contains(out, "INTERNAL_ERROR") {
		t.Errorf("go's error is not in the output:\n%s", out)
	}
	if code, calls, _ := runGoModDownload(t, 0, "GO_MOD_DOWNLOAD_ATTEMPTS=5"); code != 1 || calls != 5 {
		t.Errorf("GO_MOD_DOWNLOAD_ATTEMPTS=5: exit %d after %d calls, want 1 after 5", code, calls)
	}
}

// A setting that could take the bound away, or that the shell would misread,
// is refused before any download.
func TestGoModDownload_RefusesABadSetting(t *testing.T) {
	cases := map[string]string{}
	for _, v := range []string{"0", "-1", "x", "11", "010", "08", " 3", "99999999999999999999"} {
		cases["GO_MOD_DOWNLOAD_ATTEMPTS="+v] = "GO_MOD_DOWNLOAD_ATTEMPTS must be"
	}
	for _, v := range []string{"-1", "x", "010", "08", "1000", "99999999999999999999"} {
		cases["GO_MOD_DOWNLOAD_BACKOFF="+v] = "GO_MOD_DOWNLOAD_BACKOFF must be"
	}
	for setting, message := range cases {
		code, calls, out := runGoModDownload(t, 1, setting)
		if code != 2 || calls != 0 || !strings.Contains(out, message) {
			t.Errorf("%s: exit %d after %d calls, want 2 before any call with %q\n%s", setting, code, calls, message, out)
		}
	}
}
