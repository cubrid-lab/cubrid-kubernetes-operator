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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// goModDownloadImages are the images whose builds download the Go modules.
var goModDownloadImages = []string{
	"Dockerfile",
	"build/instance-manager/Dockerfile",
	"test/fakeim/Dockerfile",
}

func repoRoot() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..")
}

// Every image downloads its modules through build/go-mod-download.sh, so a
// download that the Go proxy cuts off is retried (#544).
func TestDockerfiles_DownloadModulesWithRetries(t *testing.T) {
	bare := regexp.MustCompile(`(?m)^RUN\s+go\s+mod\s+download\b`)
	for _, name := range goModDownloadImages {
		data, err := os.ReadFile(filepath.Join(repoRoot(), name))
		if err != nil {
			t.Fatal(err)
		}
		if bare.Match(data) {
			t.Errorf("%s runs go mod download without retries", name)
		}
		if !strings.Contains(string(data), "COPY build/go-mod-download.sh ") ||
			!regexp.MustCompile(`(?m)^RUN\s+.*go-mod-download\.sh`).Match(data) {
			t.Errorf("%s does not download the modules through build/go-mod-download.sh", name)
		}
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

func runGoModDownload(t *testing.T, succeedOn int, attempts string) (code, calls int, out string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goModStandIn), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	cmd := exec.Command("sh", filepath.Join(repoRoot(), "build", "go-mod-download.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CALLS="+log, "SUCCEED_ON="+strconv.Itoa(succeedOn), "GO_MOD_DOWNLOAD_BACKOFF=0")
	if attempts != "" {
		cmd.Env = append(cmd.Env, "GO_MOD_DOWNLOAD_ATTEMPTS="+attempts)
	}
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
	if code, calls, out := runGoModDownload(t, 2, ""); code != 0 || calls != 2 {
		t.Errorf("one cut-off download: exit %d after %d calls, want 0 after 2\n%s", code, calls, out)
	}
	code, calls, out := runGoModDownload(t, 0, "")
	if code == 0 || calls != 3 {
		t.Errorf("a download that keeps failing: exit %d after %d calls, want non-zero after 3\n%s", code, calls, out)
	}
	if !strings.Contains(out, "INTERNAL_ERROR") {
		t.Errorf("go's error is not in the output:\n%s", out)
	}
	if code, calls, _ := runGoModDownload(t, 0, "5"); code == 0 || calls != 5 {
		t.Errorf("GO_MOD_DOWNLOAD_ATTEMPTS=5: exit %d after %d calls, want non-zero after 5", code, calls)
	}
	for _, bad := range []string{"0", "-1", "x", "11"} {
		if code, calls, out := runGoModDownload(t, 1, bad); code == 0 || calls != 0 {
			t.Errorf("GO_MOD_DOWNLOAD_ATTEMPTS=%s: exit %d after %d calls, want a refusal before any call\n%s",
				bad, code, calls, out)
		}
	}
}
