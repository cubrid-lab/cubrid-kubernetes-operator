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

// Package image tests the Instance Manager image's entrypoint script with
// stand-ins for the CUBRID CLI and the manager binary. It checks which
// commands the script issues and in what order; what CUBRID does with them
// is verified with the real image (issue #97).
package image

import (
	"bytes"
	"fmt"
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
	dbName = "appdb"

	envDatabases      = "CUBRID_DATABASES"
	componentsServer  = "SERVER"
	componentsHA      = "HA"
	bootstrapRecovery = "recovery"
	callManagerStart  = "manager start"
	callHeartbeat     = "cubrid heartbeat start"

	// cubridStub records its arguments; `createdb` registers the database the
	// way the real command does.
	cubridStub = `#!/bin/bash
echo "cubrid $*" >> "${CALLS}"
if [ "$1" = "createdb" ]; then
  printf '%s\t%s\tlocalhost\n' "${CUBRID_DB}" "${PWD}" >> "${CUBRID_DATABASES}/databases.txt"
fi
`
	// managerStub exits at once, with ${IM_EXIT} when set.
	managerStub = `#!/bin/bash
echo "manager start" >> "${CALLS}"
exit "${IM_EXIT:-0}"
`
	// blockingManagerStub runs until it is told to stop, like the real one.
	blockingManagerStub = `#!/bin/bash
echo "manager start" >> "${CALLS}"
trap 'echo "manager term" >> "${CALLS}"; exit 0' TERM
while :; do sleep 0.05; done
`
)

// haComponents are the CUBRID_COMPONENTS values of an HA member.
var haComponents = []string{componentsHA, "MASTER", "SLAVE"}

type fixture struct {
	t         *testing.T
	root      string
	databases string
	calls     string
	env       map[string]string
}

func newFixture(t *testing.T, manager string) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the entrypoint is a bash script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	// Read the script in this process too, so the test cache notices a change.
	if _, err := os.ReadFile("entrypoint.sh"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "cubrid", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "cubrid"), cubridStub)
	im := filepath.Join(root, "instance-manager")
	write(im, manager)
	f := &fixture{
		t:         t,
		root:      root,
		databases: filepath.Join(root, "data", "databases"),
		calls:     filepath.Join(root, "calls"),
	}
	f.env = map[string]string{
		"CUBRID":            filepath.Join(root, "cubrid"),
		"CUBRID_DB":         dbName,
		envDatabases:        f.databases,
		"CUBRID_COMPONENTS": componentsServer,
		"CUBRID_BOOTSTRAP":  "new",
		"IM_BIN":            im,
		"CALLS":             f.calls,
	}
	return f
}

func (f *fixture) command() *exec.Cmd {
	cmd := exec.Command("bash", "entrypoint.sh")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range f.env {
		if v != "" {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	return cmd
}

// run executes the entrypoint to completion and returns its exit code and output.
func (f *fixture) run() (int, string) {
	f.t.Helper()
	out, err := f.command().CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	}
	f.t.Fatalf("run entrypoint: %v\n%s", err, out)
	return 0, ""
}

func (f *fixture) recorded() []string {
	data, err := os.ReadFile(f.calls)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// configureHA puts an HA configuration file where the entrypoint looks for it.
func (f *fixture) configureHA() {
	f.t.Helper()
	conf := filepath.Join(f.root, "cubrid_ha.conf")
	if err := os.WriteFile(conf, []byte("[common]\nha_node_list=cubrid@a:b\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.env["CUBRID_HA_CONF"] = conf
}

// wantHAConfInstalled checks that the mounted configuration was copied into
// CUBRID's conf directory and HA mode switched on.
func (f *fixture) wantHAConfInstalled() {
	f.t.Helper()
	conf := filepath.Join(f.root, "cubrid", "conf")
	installed, err := os.ReadFile(filepath.Join(conf, "cubrid_ha.conf"))
	if err != nil {
		f.t.Fatalf("cubrid_ha.conf was not installed: %v", err)
	}
	if !strings.Contains(string(installed), "ha_node_list=cubrid@a:b") {
		f.t.Errorf("installed cubrid_ha.conf = %q", installed)
	}
	main, err := os.ReadFile(filepath.Join(conf, "cubrid.conf"))
	if err != nil || !strings.Contains(string(main), "ha_mode=on") {
		f.t.Errorf("cubrid.conf does not switch HA mode on (err=%v): %q", err, main)
	}
}

// registerDatabase makes the database look already created on the volume.
func (f *fixture) registerDatabase() {
	f.t.Helper()
	if err := os.MkdirAll(f.databases, 0o755); err != nil {
		f.t.Fatal(err)
	}
	line := fmt.Sprintf("%s\t%s\tlocalhost\n", dbName, filepath.Join(f.databases, dbName))
	if err := os.WriteFile(filepath.Join(f.databases, "databases.txt"), []byte(line), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func wantCalls(t *testing.T, got []string, wantPrefixes ...string) {
	t.Helper()
	if len(got) != len(wantPrefixes) {
		t.Fatalf("calls = %q, want %d calls starting with %q", got, len(wantPrefixes), wantPrefixes)
	}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(got[i], prefix) {
			t.Errorf("call %d = %q, want prefix %q", i, got[i], prefix)
		}
	}
}

func TestEntrypoint_NewServerCreatesThenStarts(t *testing.T) {
	f := newFixture(t, managerStub)
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), "cubrid createdb", "cubrid server start "+dbName, callManagerStart)
	if strings.Contains(out, "gosu") || strings.Contains(out, "chown") {
		t.Errorf("a non-root start must not need gosu or chown:\n%s", out)
	}
}

func TestEntrypoint_ExistingDatabaseIsNotRecreated(t *testing.T) {
	f := newFixture(t, managerStub)
	f.registerDatabase()
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), "cubrid server start "+dbName, callManagerStart)
}

// Restore startup: no empty database may be created first (ADR-0008).
func TestEntrypoint_RecoveryStartsNothingBeforeRestore(t *testing.T) {
	f := newFixture(t, managerStub)
	f.env["CUBRID_BOOTSTRAP"] = bootstrapRecovery
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), callManagerStart)
	if _, err := os.Stat(filepath.Join(f.databases, "databases.txt")); !os.IsNotExist(err) {
		t.Errorf("recovery start wrote databases.txt (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(f.databases, dbName)); !os.IsNotExist(err) {
		t.Errorf("recovery start created the database directory (err=%v)", err)
	}
}

// After the restore registered the database, a restart starts it and still
// never runs createdb.
func TestEntrypoint_RecoveryStartsRestoredDatabase(t *testing.T) {
	f := newFixture(t, managerStub)
	f.env["CUBRID_BOOTSTRAP"] = bootstrapRecovery
	f.registerDatabase()
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), "cubrid server start "+dbName, callManagerStart)
}

// An initialized HA member (configuration mounted, database on the volume)
// is restarted with heartbeat; the entrypoint never creates its database.
func TestEntrypoint_HAMemberWithDatabaseStartsHeartbeat(t *testing.T) {
	for _, components := range haComponents {
		t.Run(components, func(t *testing.T) {
			f := newFixture(t, managerStub)
			f.env["CUBRID_COMPONENTS"] = components
			f.configureHA()
			f.registerDatabase()
			code, out := f.run()
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			wantCalls(t, f.recorded(), callHeartbeat, callManagerStart)
			f.wantHAConfInstalled()
		})
	}
}

// A configured HA member without a database waits: the first database is
// created once, on one member, through the Instance Manager, and the others
// are seeded from it (ADR-0010). The configuration is installed so that the
// manager's commands find it.
func TestEntrypoint_HAMemberWithoutDatabaseWaitsForTheBootstrap(t *testing.T) {
	for _, components := range haComponents {
		t.Run(components, func(t *testing.T) {
			f := newFixture(t, managerStub)
			f.env["CUBRID_COMPONENTS"] = components
			f.configureHA()
			code, out := f.run()
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			wantCalls(t, f.recorded(), callManagerStart)
			f.wantHAConfInstalled()
			if _, err := os.Stat(filepath.Join(f.databases, "databases.txt")); !os.IsNotExist(err) {
				t.Errorf("an HA member created or registered a database (err=%v)", err)
			}
		})
	}
}

// ha_mode is appended once, also when the member restarts on the same files.
func TestEntrypoint_HAModeIsNotAppendedTwice(t *testing.T) {
	f := newFixture(t, managerStub)
	f.env["CUBRID_COMPONENTS"] = componentsHA
	f.configureHA()
	for range 2 {
		if code, out := f.run(); code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	}
	data, err := os.ReadFile(filepath.Join(f.root, "cubrid", "conf", "cubrid.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "ha_mode=on"); n != 1 {
		t.Errorf("ha_mode=on appears %d times:\n%s", n, data)
	}
}

// After the bootstrap the Instance Manager has started heartbeat while this
// script runs, so termination has to stop it.
func TestEntrypoint_TerminationAfterHABootstrapStopsHeartbeat(t *testing.T) {
	f := newFixture(t, blockingManagerStub)
	f.env["CUBRID_COMPONENTS"] = componentsHA
	f.configureHA()
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return managerStarted(f.recorded()) }, &out)
	f.registerDatabase() // what the bootstrap does
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM: %v\n%s", err, out.String())
	}
	wantCalls(t, f.recorded(), callManagerStart, "cubrid heartbeat stop", "manager term")
}

// `cubrid heartbeat start` exits 1 on a member without HA configuration
// (docs/poc/RESULTS.md, POC-12), which would end the entrypoint. Until the HA
// bootstrap supplies the configuration the member stays up with the manager
// only, creates nothing and reports no role.
func TestEntrypoint_HAWithoutConfigurationStartsOnlyTheManager(t *testing.T) {
	for _, components := range haComponents {
		t.Run(components, func(t *testing.T) {
			f := newFixture(t, managerStub)
			f.env["CUBRID_COMPONENTS"] = components
			code, out := f.run()
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			wantCalls(t, f.recorded(), callManagerStart)
			if !strings.Contains(out, "heartbeat is not started") {
				t.Errorf("the log does not say why nothing was started:\n%s", out)
			}
		})
	}
}

// Nothing was started for an unconfigured HA member, so nothing is stopped.
func TestEntrypoint_TerminationOfUnconfiguredHAMemberStopsOnlyTheManager(t *testing.T) {
	f := newFixture(t, blockingManagerStub)
	f.env["CUBRID_COMPONENTS"] = componentsHA
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return managerStarted(f.recorded()) }, &out)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM: %v\n%s", err, out.String())
	}
	wantCalls(t, f.recorded(), callManagerStart, "manager term")
}

func TestEntrypoint_RejectsBadConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		message string
	}{
		{"unknown components", "CUBRID_COMPONENTS", "BROKER", "unknown CUBRID_COMPONENTS 'BROKER'"},
		{"unknown bootstrap", "CUBRID_BOOTSTRAP", "clone", "unknown CUBRID_BOOTSTRAP 'clone'"},
		{"no database root", envDatabases, "", "CUBRID_DATABASES is not set"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, managerStub)
			f.env[tc.key] = tc.value
			code, out := f.run()
			if code == 0 {
				t.Fatalf("expected a failure:\n%s", out)
			}
			if !strings.Contains(out, tc.message) {
				t.Errorf("output does not say %q:\n%s", tc.message, out)
			}
			if calls := f.recorded(); len(calls) != 0 {
				t.Errorf("nothing may be started, got %q", calls)
			}
		})
	}
}

// The data volume belongs to another user and the Pod has no fsGroup access.
func TestEntrypoint_UnwritableDataVolumeFailsClearly(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	f := newFixture(t, managerStub)
	locked := filepath.Join(f.root, "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	f.env[envDatabases] = filepath.Join(locked, "databases")
	code, out := f.run()
	if code == 0 {
		t.Fatalf("expected a failure:\n%s", out)
	}
	if !strings.Contains(out, "must be writable by this user") {
		t.Errorf("output does not explain the volume permission:\n%s", out)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Errorf("nothing may be started, got %q", calls)
	}
}

func TestEntrypoint_ExitStatusIsTheManagers(t *testing.T) {
	f := newFixture(t, managerStub)
	f.env["IM_EXIT"] = "7"
	if code, out := f.run(); code != 7 {
		t.Fatalf("exit %d, want the manager's 7:\n%s", code, out)
	}
}

// On SIGTERM the shell stops CUBRID first, then the manager, and exits with
// the manager's status.
func TestEntrypoint_TerminationStopsCubridThenManager(t *testing.T) {
	tests := []struct {
		components string
		stop       string
	}{
		{componentsServer, "cubrid server stop " + dbName},
		{"HA", "cubrid heartbeat stop"},
	}
	for _, tc := range tests {
		t.Run(tc.components, func(t *testing.T) {
			f := newFixture(t, blockingManagerStub)
			f.env["CUBRID_COMPONENTS"] = tc.components
			if tc.components != componentsServer {
				f.configureHA()
			}
			f.registerDatabase()
			cmd := f.command()
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return managerStarted(f.recorded()) }, &out)
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if err := waitExit(cmd); err != nil {
				t.Fatalf("entrypoint after SIGTERM: %v\n%s", err, out.String())
			}
			calls := f.recorded()
			stop, term := index(calls, tc.stop), index(calls, "manager term")
			if stop < 0 || term < 0 || stop > term {
				t.Errorf("want %q before %q, got %q", tc.stop, "manager term", calls)
			}
		})
	}
}

// Nothing was started in a recovery bootstrap, so nothing is stopped.
func TestEntrypoint_TerminationBeforeRestoreStopsOnlyTheManager(t *testing.T) {
	f := newFixture(t, blockingManagerStub)
	f.env["CUBRID_BOOTSTRAP"] = bootstrapRecovery
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return managerStarted(f.recorded()) }, &out)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM: %v\n%s", err, out.String())
	}
	wantCalls(t, f.recorded(), callManagerStart, "manager term")
}

// A restore registers the database and the Instance Manager starts its server
// while this script runs (#178), so termination has to stop that server too.
func TestEntrypoint_TerminationAfterRestoreStopsTheRestoredServer(t *testing.T) {
	f := newFixture(t, blockingManagerStub)
	f.env["CUBRID_BOOTSTRAP"] = bootstrapRecovery
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return managerStarted(f.recorded()) }, &out)
	f.registerDatabase() // what the restore does
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM: %v\n%s", err, out.String())
	}
	wantCalls(t, f.recorded(), callManagerStart, "cubrid server stop "+dbName, "manager term")
}

func waitFor(t *testing.T, done func() bool, out *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the manager to start:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// exitLimit is how long the entrypoint may take to exit after a signal.
const exitLimit = 20 * time.Second

func waitExit(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(exitLimit):
		_ = cmd.Process.Kill()
		return fmt.Errorf("did not exit within %s", exitLimit)
	}
}

func managerStarted(calls []string) bool { return index(calls, callManagerStart) >= 0 }

func index(calls []string, want string) int {
	for i, call := range calls {
		if call == want {
			return i
		}
	}
	return -1
}
