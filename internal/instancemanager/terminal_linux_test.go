//go:build linux

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

package instancemanager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// terminalCSQL stands in for csql on a pseudo-terminal. It records its argv
// and environment, refuses to run without a terminal, and then, by ${MODE}:
// prompts as csql does and checks the line typed against ${CAPTURE}/password
// (the default); prompts, then fails as a database in use does ("mount");
// or never prompts, with a child process of its own ("silent"), recording
// both process IDs.
const terminalCSQL = `#!/bin/bash
printf '%s\n' "$*" >> "${CAPTURE}/argv"
env >> "${CAPTURE}/env"
[ -t 0 ] || { echo "not a terminal"; exit 9; }
case "${MODE:-}" in
silent)
  sleep 600 &
  echo "$!" > "${CAPTURE}/child"
  echo "$$" > "${CAPTURE}/pid"
  sleep 600
  exit 0 ;;
mount)
  printf 'Enter Password : '
  IFS= read -r typed
  echo
  echo "ERROR: Unable to mount disk volume."
  exit 1 ;;
esac
printf 'Enter Password : '
IFS= read -r typed
echo
if [ "${typed}" = "$(cat "${CAPTURE}/password")" ]; then echo "1 row selected."; exit 0; fi
echo "ERROR: Incorrect or missing password."
exit 1
`

// terminalFixture puts terminalCSQL first on PATH as csql, and writes the
// password it accepts.
func terminalFixture(t *testing.T, mode string) (capture string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no pseudo-terminal device")
	}
	bin, capture := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "csql"), []byte(terminalCSQL), 0o700); err != nil { // #nosec G306 -- an executable test stand-in
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capture, "password"), []byte(testPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE", capture)
	t.Setenv("MODE", mode)
	return capture
}

// wantNoPasswordIn checks that neither the argv nor the environment of the
// stand-in csql carried the password.
func wantNoPasswordIn(t *testing.T, capture string) {
	t.Helper()
	for _, name := range []string{"argv", "env"} {
		b, err := os.ReadFile(filepath.Join(capture, name)) // #nosec G304 -- the test's own capture directory
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(b), testPassword) {
			t.Errorf("the password is in csql's %s:\n%s", name, b)
		}
	}
}

// Through a real pseudo-terminal, the password typed at the prompt is
// accepted when it is the database's password and refused otherwise. It is
// on no argv and in no environment, and with echo off the terminal does not
// print it back.
func TestExecCLI_RunOnTerminal_TypesThePasswordAtThePrompt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typed    string
		accepted bool
	}{
		{name: "the password", typed: testPassword, accepted: true},
		{name: "another password", typed: testPassword + "-wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := terminalFixture(t, "")
			out, prompted, err := ExecCLI{}.RunOnTerminal(context.Background(), csqlPasswordPrompt, tc.typed,
				"csql", emptyLoginArgs(dbName)...)
			if !prompted {
				t.Fatalf("no prompt seen: out=%q err=%v", out, err)
			}
			if strings.Contains(out, testPassword) {
				t.Errorf("the terminal printed the typed password back: %q", out)
			}
			accepted, err := dbaPasswordAccepted(context.Background(), ExecCLI{}, dbName, tc.typed)
			if err != nil || accepted != tc.accepted {
				t.Fatalf("dbaPasswordAccepted = %v, %v; want %v", accepted, err, tc.accepted)
			}
			wantNoPasswordIn(t, capture)
		})
	}
}

// A csql that prompts, then fails for another reason than the password, is
// an error, and the error does not carry what it printed.
func TestExecCLI_RunOnTerminal_AnotherFailureIsAnError(t *testing.T) {
	terminalFixture(t, "mount")
	accepted, err := dbaPasswordAccepted(context.Background(), ExecCLI{}, dbName, testPassword)
	if err == nil || accepted {
		t.Fatalf("dbaPasswordAccepted = %v, %v; want an error", accepted, err)
	}
	if strings.Contains(err.Error(), "mount") || strings.Contains(err.Error(), testPassword) {
		t.Errorf("the error carries csql's output: %v", err)
	}
}

// A csql that never prompts cannot hold the manager: the context ends the
// wait, nothing is typed, and csql and every process it started are killed
// and gone when RunOnTerminal returns.
func TestExecCLI_RunOnTerminal_ACommandThatNeverPromptsIsEnded(t *testing.T) {
	capture := terminalFixture(t, "silent")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	accepted, err := dbaPasswordAccepted(ctx, ExecCLI{}, dbName, testPassword)
	if err == nil || accepted {
		t.Fatalf("dbaPasswordAccepted = %v, %v; want an error", accepted, err)
	}
	if !strings.Contains(err.Error(), msgNoPrompt) {
		t.Errorf("err = %v, want it to say csql did not prompt", err)
	}
	if waited := time.Since(start); waited > 15*time.Second {
		t.Errorf("RunOnTerminal returned after %v, long after its context ended", waited)
	}
	for _, name := range []string{"pid", "child"} {
		b, err := os.ReadFile(filepath.Join(capture, name)) // #nosec G304 -- the test's own capture directory
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("process %s (%d) is still there after RunOnTerminal returned (kill 0: %v)", name, pid, err)
		}
	}
	wantNoPasswordIn(t, capture)
}

// A command that cannot be started is an error, not a refusal.
func TestExecCLI_RunOnTerminal_AMissingCommandIsAnError(t *testing.T) {
	_, prompted, err := ExecCLI{}.RunOnTerminal(context.Background(), csqlPasswordPrompt, testPassword,
		filepath.Join(t.TempDir(), "no-such-csql"))
	if err == nil || prompted {
		t.Fatalf("RunOnTerminal = prompted %v, %v; want an error", prompted, err)
	}
}
