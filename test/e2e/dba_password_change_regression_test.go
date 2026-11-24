//go:build e2e && linux
// +build e2e,linux

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

package e2e

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeCSQL stands in for csql on a terminal: like csql, it first prints the
// refusal of the empty password, then prompts for the password with echo
// off, refuses any value but $0, and then echoes each line it reads after a
// csql> prompt until ;exit.
const fakeCSQL = `echo "Incorrect or missing password."; printf 'Enter Password : '; IFS= read -rs pw; echo
[ "$pw" = "$0" ] || { echo "Incorrect or missing password."; exit 1; }
while printf 'csql> '; IFS= read -r l; do echo "got:$l"; [ "$l" = ";exit" ] && exit 0; done`

func TestCSQLDialogTypesThePasswordAndEachLineAtItsPrompt(t *testing.T) {
	const secret = "Fake-Secret-1012"
	tr, err := runCSQLDialog(exec.Command("bash", "-c", fakeCSQL, secret), secret,
		[]string{";read /tmp/x.sql", ";run", ";exit"}, 30*time.Second)
	if err != nil {
		t.Fatalf("dialog: %v\n%s", err, tr.all)
	}
	if strings.Contains(tr.all, secret) {
		t.Errorf("the terminal showed the password:\n%s", tr.all)
	}
	if strings.Contains(tr.afterPassword, dbaPasswordRefused) {
		t.Errorf("the refusal printed before the prompt is counted after the password:\n%s", tr.afterPassword)
	}
	read := strings.Index(tr.afterPassword, "got:;read /tmp/x.sql")
	run := strings.Index(tr.afterPassword, "got:;run")
	exit := strings.Index(tr.afterPassword, "got:;exit")
	if read < 0 || run < read || exit < run {
		t.Errorf("the lines were not typed in order:\n%s", tr.afterPassword)
	}
}

func TestCSQLDialogReportsARefusedLogin(t *testing.T) {
	tr, err := runCSQLDialog(exec.Command("bash", "-c", fakeCSQL+"; echo "+loginOK, "right"), "wrong", nil,
		30*time.Second)
	if err != nil {
		t.Fatalf("dialog: %v\n%s", err, tr.all)
	}
	if v := loginVerdict(tr); v != "refused" {
		t.Errorf("verdict %q, want refused:\n%s", v, tr.all)
	}
}

func TestCSQLDialogFailsWithoutAPrompt(t *testing.T) {
	_, err := runCSQLDialog(exec.Command("bash", "-c", "echo no prompt"), "x", nil, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not prompt") {
		t.Errorf("error %v, want that csql did not prompt", err)
	}
	_, err = runCSQLDialog(exec.Command("bash", "-c", "sleep 30"), "x", nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not end") {
		t.Errorf("error %v, want that csql did not end", err)
	}
}
