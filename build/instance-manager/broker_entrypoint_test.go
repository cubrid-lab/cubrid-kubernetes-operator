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
	write(filepath.Join(bin, "cubrid"), "#!/bin/bash\necho \"cubrid $*\" >> \"${CALLS}\"\n", 0o755)
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
			if calls := f.recorded(); len(calls) != 1 || calls[0] != callBrokerUp {
				t.Errorf("calls = %q, want one %q", calls, callBrokerUp)
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
	if calls := f.recorded(); len(calls) != 2 || calls[1] != callBrokerOff {
		t.Errorf("calls = %q, want start then stop", calls)
	}
}
