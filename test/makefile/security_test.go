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
	"runtime"
	"strings"
	"testing"
)

// govulncheckStandIn records its arguments in $CALLS and exits with
// $VULN_EXIT: govulncheck exits 3 when it finds a reachable vulnerability.
const govulncheckStandIn = `#!/bin/sh
echo "$*" >> "$CALLS"
exit "${VULN_EXIT:-0}"
`

// runSecurity runs "make security" with the stand-in as govulncheck and
// returns make's exit code, its output and the stand-in's arguments.
func runSecurity(t *testing.T, vulnExit string) (code int, out string, calls string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")
	dir := t.TempDir()
	tool := filepath.Join(t.TempDir(), "govulncheck-stand-in")
	if err := os.WriteFile(tool, []byte(govulncheckStandIn), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	// -o keeps make from installing govulncheck over the stand-in.
	cmd := exec.Command("make", "-C", dir, "-f", filepath.Join(repo, "Makefile"), "-o", tool, "security",
		"GOVULNCHECK="+tool)
	cmd.Env = append(childMakeEnv(), "CALLS="+log, "VULN_EXIT="+vulnExit)
	output, err := cmd.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run make: %v", err)
	}
	data, _ := os.ReadFile(log)
	return code, string(output), strings.TrimSpace(string(data))
}

// make security passes the whole module to govulncheck and fails when it
// reports a reachable vulnerability (#538).
func TestSecurity_FailsOnAReachableVulnerability(t *testing.T) {
	code, out, calls := runSecurity(t, "3")
	if code == 0 {
		t.Fatalf("make security exited 0 although govulncheck exited 3:\n%s", out)
	}
	if calls != "./..." {
		t.Errorf("govulncheck was called with %q, want ./...", calls)
	}

	code, out, _ = runSecurity(t, "0")
	if code != 0 {
		t.Fatalf("make security exited %d although govulncheck found nothing:\n%s", code, out)
	}
}
