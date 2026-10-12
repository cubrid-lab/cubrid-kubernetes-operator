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

package image

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	brokerConfRW  = "[%RW]\nBROKER_PORT=33000\nACCESS_MODE=RW\n"
	brokerConfRO  = "[%RO]\nBROKER_PORT=33001\nACCESS_MODE=RO\n"
	brokerDBsTxt  = "appdb\t/data/appdb\tdemo-0:demo-1:demo-2\t/data/appdb\tfile:/data/appdb/lob\n"
	callBrokerUp  = "cubrid broker start"
	callBrokerOff = "cubrid broker stop"
)

// brokerStandIn records its calls. With BROKER_SHM set it also behaves like
// the real command around a Broker's shared memory, which that file stands
// for (docs/poc/RESULTS.md, POC-18): "broker start" refuses while it exists,
// and "broker stop" removes it or fails when there is none.
const brokerStandIn = `#!/bin/bash
echo "cubrid $*" >> "${CALLS}"
# BROKER_START_SECONDS makes "broker start" take that long, as the real one does.
if [ "$*" = "broker start" ] && [ -n "${BROKER_START_SECONDS:-}" ]; then sleep "${BROKER_START_SECONDS}"; fi
[ -n "${BROKER_SHM:-}" ] || exit 0
case "$*" in
  "broker start")
    if [ -e "${BROKER_SHM}" ]; then echo "++ cubrid broker is running."; exit 1; fi
    : > "${BROKER_SHM}" ;;
  "broker stop")
    if [ ! -e "${BROKER_SHM}" ]; then echo "++ cubrid broker is not running."; exit 1; fi
    rm -f "${BROKER_SHM}" ;;
esac
`

// brokerFixture runs broker-entrypoint.sh with a stand-in for the cubrid CLI.
type brokerFixture struct {
	t     *testing.T
	root  string
	tools string
	calls string
	env   map[string]string
}

func newBrokerFixture(t *testing.T) *brokerFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the entrypoint is a bash script")
	}
	// Read the script in this process too, so the test cache notices a change.
	if _, err := os.ReadFile("broker-entrypoint.sh"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "cubrid", "bin")
	conf := filepath.Join(root, "mounted")
	for _, dir := range []string{bin, conf} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "cubrid"), brokerStandIn, 0o755)
	write(filepath.Join(conf, "cubrid_broker_rw.conf"), brokerConfRW, 0o644)
	write(filepath.Join(conf, "cubrid_broker_ro.conf"), brokerConfRO, 0o644)
	write(filepath.Join(conf, "databases.txt"), brokerDBsTxt, 0o644)
	f := &brokerFixture{t: t, root: root, tools: writeTools(t, root, nil), calls: filepath.Join(root, "calls")}
	f.env = map[string]string{
		"CUBRID":                filepath.Join(root, "cubrid"),
		envDatabases:            filepath.Join(root, "databases"),
		"BROKER_CONF_DIR":       conf,
		"BROKER_ACCESS_MODE":    "rw",
		"BROKER_CHECK_INTERVAL": "0.2",
		"CALLS":                 f.calls,
	}
	return f
}

func (f *brokerFixture) command() *exec.Cmd {
	return scriptCommand(f.t, "broker-entrypoint.sh", f.tools, f.env)
}

func (f *brokerFixture) recorded() []string {
	data, err := os.ReadFile(f.calls)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (f *brokerFixture) read(parts ...string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{f.root}, parts...)...))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// The stand-in starts no cub_broker process, so the script sees the Broker as
// gone at its first check: it must have installed the configuration of its
// access mode, started the Broker once, and then exit non-zero so that the
// container is restarted.
func TestBrokerEntrypoint_InstallsItsModeAndExitsWhenTheBrokerIsGone(t *testing.T) {
	for mode, want := range map[string]string{"rw": brokerConfRW, "ro": brokerConfRO} {
		t.Run(mode, func(t *testing.T) {
			f := newBrokerFixture(t)
			f.env["BROKER_ACCESS_MODE"] = mode
			out, err := f.command().CombinedOutput()
			if err == nil {
				t.Fatalf("the script exited 0 although no Broker runs:\n%s", out)
			}
			if !strings.Contains(string(out), "not running any more") {
				t.Errorf("output does not say why it stopped:\n%s", out)
			}
			if got := f.read("cubrid", "conf", "cubrid_broker.conf"); got != want {
				t.Errorf("installed cubrid_broker.conf = %q, want the %s configuration", got, mode)
			}
			if got := f.read("databases", "databases.txt"); got != brokerDBsTxt {
				t.Errorf("installed databases.txt = %q", got)
			}
			if calls := f.recorded(); len(calls) != 2 || calls[0] != callBrokerOff || calls[1] != callBrokerUp {
				t.Errorf("calls = %q, want %q then %q", calls, callBrokerOff, callBrokerUp)
			}
		})
	}
}

// A Broker that was killed leaves its shared memory behind, and in a Pod the
// memory outlives the container. The next container must start the Broker
// all the same, also when there is nothing to clear.
func TestBrokerEntrypoint_StartsWhateverAnEarlierBrokerLeft(t *testing.T) {
	for name, stale := range map[string]bool{"after a killed Broker": true, "on a clean start": false} {
		t.Run(name, func(t *testing.T) {
			f := newBrokerFixture(t)
			shm := filepath.Join(f.root, "broker-shm")
			f.env["BROKER_SHM"] = shm
			if stale {
				if err := os.WriteFile(shm, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, _ := f.command().CombinedOutput()
			// The stand-in runs no process, so the script ends at its first
			// check. Before that it must have started the Broker.
			if !strings.Contains(string(out), "not running any more") {
				t.Errorf("the script did not get as far as watching the Broker:\n%s", out)
			}
			if _, err := os.Stat(shm); err != nil {
				t.Errorf("the Broker was not started:\n%s", out)
			}
			if calls := f.recorded(); index(calls, callBrokerUp) < 0 || calls[len(calls)-1] != callBrokerUp {
				t.Errorf("calls = %q, want them to end with %q", calls, callBrokerUp)
			}
		})
	}
}

func TestBrokerEntrypoint_RejectsBadConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		change  func(f *brokerFixture)
		message string
	}{
		{"no access mode", func(f *brokerFixture) { f.env["BROKER_ACCESS_MODE"] = "" }, "must be rw or ro"},
		{"unknown access mode", func(f *brokerFixture) { f.env["BROKER_ACCESS_MODE"] = "so" }, "must be rw or ro"},
		{"no database root", func(f *brokerFixture) { f.env[envDatabases] = "" }, "CUBRID_DATABASES is not set"},
		{"no mounted configuration", func(f *brokerFixture) { f.env["BROKER_CONF_DIR"] = f.root + "/missing" },
			"no Broker configuration"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newBrokerFixture(t)
			tc.change(f)
			out, err := f.command().CombinedOutput()
			if err == nil {
				t.Fatalf("expected a failure:\n%s", out)
			}
			if !strings.Contains(string(out), tc.message) {
				t.Errorf("output does not say %q:\n%s", tc.message, out)
			}
			if calls := f.recorded(); len(calls) != 0 {
				t.Errorf("nothing may be started, got %q", calls)
			}
		})
	}
}

// A Broker is never started as root: there is no branch that hands over.
func TestBrokerEntrypoint_RefusesToRunAsRoot(t *testing.T) {
	f := newBrokerFixture(t)
	f.env[envFakeUID] = uidRoot
	out, err := f.command().CombinedOutput()
	if err == nil {
		t.Fatalf("the Broker entrypoint ran as root:\n%s", out)
	}
	if !strings.Contains(string(out), "must not run as root") {
		t.Errorf("output does not say why it stopped:\n%s", out)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Errorf("nothing may be started, got %q", calls)
	}
}

// On SIGTERM the Broker is stopped and the script exits 0.
func TestBrokerEntrypoint_TerminationStopsTheBroker(t *testing.T) {
	f := newBrokerFixture(t)
	// Long enough that the first liveness check does not come before the signal.
	f.env["BROKER_CHECK_INTERVAL"] = "30"
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(exitLimit)
	for index(f.recorded(), callBrokerUp) < 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the Broker was not started:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM: %v\n%s", err, out.String())
	}
	if calls := f.recorded(); len(calls) != 3 || calls[1] != callBrokerUp || calls[2] != callBrokerOff {
		t.Errorf("calls = %q, want the Broker started and then stopped", calls)
	}
}

// A termination handled between the start of the liveness sleep and the
// recording of its PID finds no sleep to kill. The script must still exit at
// once instead of waiting out the interval (#347). The window lasts a few
// builtins, so the test runs a copy of the script that holds it open: it
// records "window" and spins on builtins, during which bash runs the trap,
// until the test releases it.
func TestBrokerEntrypoint_TerminationBeforeTheSleepIsRecorded(t *testing.T) {
	f := newBrokerFixture(t)
	f.env["BROKER_CHECK_INTERVAL"] = "30"
	release := filepath.Join(f.root, "release")
	script, err := os.ReadFile("broker-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	const sleepLine = "  sleep \"${BROKER_CHECK_INTERVAL}\" &\n"
	if strings.Count(string(script), sleepLine) != 1 {
		t.Fatalf("broker-entrypoint.sh has no single line %q to hold the window after", sleepLine)
	}
	held := strings.Replace(string(script), sleepLine, sleepLine+
		`  echo window >> "${CALLS}"; while [ ! -e "`+release+`" ]; do :; done`+"\n", 1)
	path := filepath.Join(f.root, "broker-entrypoint-held.sh")
	if err := os.WriteFile(path, []byte(held), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := scriptCommand(t, path, f.tools, f.env)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, ok func([]string) bool) {
		t.Helper()
		deadline := time.Now().Add(exitLimit)
		for !ok(f.recorded()) {
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("%s:\n%s", what, out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("the script did not reach the window", func(c []string) bool { return index(c, "window") >= 0 })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The trap stops the Broker; only then is the window released.
	waitFor("the termination was not handled in the window", func(c []string) bool {
		return index(c, "window") < len(c)-1 && c[len(c)-1] == callBrokerOff
	})
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM before the sleep was recorded: %v\n%s", err, out.String())
	}
}

// A termination that arrives while the Broker is still being started must be
// handled like any other: the Broker is stopped and the script exits 0. As
// PID 1 of a container the script would otherwise ignore the signal, and the
// Pod would be killed at the end of its grace period.
func TestBrokerEntrypoint_TerminationDuringTheStart(t *testing.T) {
	f := newBrokerFixture(t)
	f.env["BROKER_CHECK_INTERVAL"] = "30"
	f.env["BROKER_START_SECONDS"] = "1"
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(exitLimit)
	for index(f.recorded(), callBrokerUp) < 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the Broker was not started:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The start is still running: the stand-in sleeps for a second.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM during the start: %v\n%s", err, out.String())
	}
	calls := f.recorded()
	if calls[len(calls)-1] != callBrokerOff || index(calls, callBrokerUp) < 0 {
		t.Errorf("calls = %q, want the Broker stopped after its start", calls)
	}
}
