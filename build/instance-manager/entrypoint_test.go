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
	"errors"
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
	bootstrapNew      = "new"
	bootstrapRecovery = "recovery"
	callManagerStart  = "manager start"
	// callShutdown is the one command that stops a member; the manager
	// decides what there is to stop.
	callShutdown    = "manager shutdown"
	callManagerTerm = "manager term"
	callHeartbeat   = "cubrid heartbeat start"

	// cubridStub records its arguments. `createdb` behaves as the real one
	// does on the pinned image (docs/poc/RESULTS.md, POC-25): it writes its
	// volumes into the working directory and registers the database in
	// databases.txt near its end; it refuses, with exit 1 and nothing changed,
	// when a volume of the database is already there.
	cubridStub = `#!/bin/bash
echo "cubrid $*" >> "${CALLS}"
# CUBRID_START_SECONDS makes a start take that long, as the real one does.
if [ "$2" = "start" ] && [ -n "${CUBRID_START_SECONDS:-}" ]; then sleep "${CUBRID_START_SECONDS}"; fi
# CUBRID_START_EXIT makes a start fail, as it does on an unfinished database.
if [ "$2" = "start" ] && [ -n "${CUBRID_START_EXIT:-}" ]; then exit "${CUBRID_START_EXIT}"; fi
if [ "$1" = "createdb" ]; then
  [ -e .im-operation ] && cat .im-operation > "${CALLS}.marker"
  if [ -e "${CUBRID_DB}_lgat" ]; then
    echo "Volume \"${PWD}/${CUBRID_DB}_lgat\" already exists." >&2
    exit 1
  fi
  : > "${CUBRID_DB}_lgat"
  # CUBRID_CREATEDB_HANG=unregistered|registered stops createdb before or
  # after its registration until it is killed, as an interrupted one is.
  if [ -n "${CUBRID_CREATEDB_HANG:-}" ]; then
    [ "${CUBRID_CREATEDB_HANG}" = "registered" ] \
      && printf '%s\t%s\tlocalhost\n' "${CUBRID_DB}" "${PWD}" >> "${CUBRID_DATABASES}/databases.txt"
    echo $$ > "${CALLS}.createdb.pid"
    while :; do sleep 0.05; done
  fi
  # CUBRID_CREATEDB_SECONDS makes createdb take that long.
  if [ -n "${CUBRID_CREATEDB_SECONDS:-}" ]; then sleep "${CUBRID_CREATEDB_SECONDS}"; fi
  # CUBRID_CREATEDB_EXIT makes createdb fail after it wrote a volume.
  if [ -n "${CUBRID_CREATEDB_EXIT:-}" ]; then exit "${CUBRID_CREATEDB_EXIT}"; fi
  : > "${CUBRID_DB}"
  printf '%s\t%s\tlocalhost\n' "${CUBRID_DB}" "${PWD}" >> "${CUBRID_DATABASES}/databases.txt"
fi
`
	// waitVolumeStub stands in for "instance-manager wait-volume-free": it
	// returns at once unless VOLUME_HELD is set; then it records the call
	// and waits while the file VOLUME_HELD names exists, as the real one
	// waits while another process holds a CUBRID lock on the volume. It
	// exits with ${WAIT_EXIT} when set.
	waitVolumeStub = `if [ "${1:-}" = "wait-volume-free" ]; then
  [ -n "${VOLUME_HELD:-}" ] || exit "${WAIT_EXIT:-0}"
  echo "manager wait-volume-free" >> "${CALLS}"
  while [ -e "${VOLUME_HELD}" ]; do sleep 0.05; done
  exit "${WAIT_EXIT:-0}"
fi
`
	// managerStub exits at once, with ${IM_EXIT} when set.
	managerStub = `#!/bin/bash
` + waitVolumeStub + `if [ "${1:-}" = "shutdown" ]; then echo "manager shutdown" >> "${CALLS}"; exit 0; fi
echo "manager start" >> "${CALLS}"
exit "${IM_EXIT:-0}"
`
	// blockingManagerStub runs until it is told to stop, like the real one.
	blockingManagerStub = `#!/bin/bash
` + waitVolumeStub + `if [ "${1:-}" = "shutdown" ]; then echo "manager shutdown" >> "${CALLS}"; exit 0; fi
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
	tools     string
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
		tools:     writeTools(t, root, nil),
		databases: filepath.Join(root, "data", "databases"),
		calls:     filepath.Join(root, "calls"),
	}
	f.env = map[string]string{
		"CUBRID":            filepath.Join(root, "cubrid"),
		"CUBRID_DB":         dbName,
		envDatabases:        f.databases,
		"CUBRID_COMPONENTS": componentsServer,
		"CUBRID_BOOTSTRAP":  bootstrapNew,
		"IM_BIN":            im,
		"CALLS":             f.calls,
	}
	return f
}

func (f *fixture) command() *exec.Cmd {
	return scriptCommand(f.t, "entrypoint.sh", f.tools, f.env)
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

// wantHAConfNotInstalled checks that nothing was copied into CUBRID's conf
// directory.
func (f *fixture) wantHAConfNotInstalled() {
	f.t.Helper()
	conf := filepath.Join(f.root, "cubrid", "conf", "cubrid_ha.conf")
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		f.t.Errorf("cubrid_ha.conf was installed (err=%v)", err)
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

// markUnfinished leaves the ownership marker an interrupted createdb or
// restoredb of the Instance Manager leaves in the database directory.
func (f *fixture) markUnfinished() string {
	f.t.Helper()
	dir := filepath.Join(f.databases, dbName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	marker := filepath.Join(dir, ".im-operation")
	if err := os.WriteFile(marker, []byte("op-1\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return marker
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

// An HA member in a recovery bootstrap starts nothing before its restore, but
// gets its mounted configuration installed: the restore then starts heartbeat,
// which fails without it (docs/poc/RESULTS.md POC-12). Without a mounted
// configuration nothing is installed.
func TestEntrypoint_RecoveryInstallsHAConfBeforeRestore(t *testing.T) {
	for _, components := range haComponents {
		for _, haConf := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/haConf=%t", components, haConf), func(t *testing.T) {
				f := newFixture(t, managerStub)
				f.env["CUBRID_COMPONENTS"] = components
				f.env["CUBRID_BOOTSTRAP"] = bootstrapRecovery
				if haConf {
					f.configureHA()
				} else {
					f.env["CUBRID_HA_CONF"] = filepath.Join(f.root, "missing", "cubrid_ha.conf")
				}
				code, out := f.run()
				if code != 0 {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				wantCalls(t, f.recorded(), callManagerStart)
				if haConf {
					f.wantHAConfInstalled()
				} else {
					f.wantHAConfNotInstalled()
				}
				if _, err := os.Stat(filepath.Join(f.databases, "databases.txt")); !os.IsNotExist(err) {
					t.Errorf("recovery start wrote databases.txt (err=%v)", err)
				}
			})
		}
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

// A database whose createdb or restoredb was interrupted carries the
// manager's ownership marker, registered or not. Starting it fails, so the
// entrypoint creates and starts nothing, starts only the manager, which owns
// the cleanup, and leaves the marker and the data as they are. An HA member
// still gets its configuration when it is mounted.
func TestEntrypoint_UnfinishedDatabaseStartsOnlyTheManager(t *testing.T) {
	tests := []struct {
		components string
		bootstrap  string
		haConf     bool
		registered bool
	}{
		{componentsServer, bootstrapNew, false, true},
		{componentsServer, bootstrapRecovery, false, true},
		{componentsServer, bootstrapNew, false, false},
		{componentsHA, bootstrapNew, true, true},
		{componentsHA, bootstrapRecovery, true, true},
		{componentsHA, bootstrapNew, true, false},
		{componentsHA, bootstrapNew, false, true},
	}
	for _, tc := range tests {
		name := fmt.Sprintf("%s/%s/haConf=%t/registered=%t", tc.components, tc.bootstrap, tc.haConf, tc.registered)
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, managerStub)
			f.env["CUBRID_COMPONENTS"] = tc.components
			f.env["CUBRID_BOOTSTRAP"] = tc.bootstrap
			f.env["CUBRID_START_EXIT"] = "1"
			if tc.haConf {
				f.configureHA()
			}
			if tc.registered {
				f.registerDatabase()
			}
			marker := f.markUnfinished()
			code, out := f.run()
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			wantCalls(t, f.recorded(), callManagerStart)
			if data, err := os.ReadFile(marker); err != nil || string(data) != "op-1\n" {
				t.Errorf("the ownership marker was not kept as it was (err=%v): %q", err, data)
			}
			if tc.haConf {
				f.wantHAConfInstalled()
			} else {
				f.wantHAConfNotInstalled()
			}
			if !strings.Contains(out, "is unfinished ("+marker+")") {
				t.Errorf("the log does not say why nothing was started:\n%s", out)
			}
		})
	}
}

// A marker that is a dangling symbolic link still marks the database as
// unfinished; the entrypoint does not decide what it means.
func TestEntrypoint_DanglingMarkerKeepsTheDatabaseStopped(t *testing.T) {
	f := newFixture(t, managerStub)
	f.registerDatabase()
	dir := filepath.Join(f.databases, dbName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, ".im-operation")
	if err := os.Symlink(filepath.Join(f.root, "missing"), marker); err != nil {
		t.Fatal(err)
	}
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), callManagerStart)
	if _, err := os.Lstat(marker); err != nil {
		t.Errorf("the marker was removed: %v", err)
	}
}

// entrypointMarker is what the entrypoint writes into the ownership marker
// around its own createdb: not an operation ID, so that the Instance Manager
// never takes the directory for one of its own.
const entrypointMarker = "entrypoint-createdb\n"

// killed ends the entrypoint and the createdb stand-in it is waiting for with
// SIGKILL, as the loss of the container or the node does.
func (f *fixture) killed(cmd *exec.Cmd, out *bytes.Buffer) {
	f.t.Helper()
	pidFile := f.calls + ".createdb.pid"
	waitFor(f.t, func() bool { _, err := os.Stat(pidFile); return err == nil }, out)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		f.t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(string(data), &pid); err != nil {
		f.t.Fatalf("createdb pid %q: %v", data, err)
	}
	_ = cmd.Process.Kill()
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = waitExit(cmd)
}

// A standalone createdb that is interrupted, before or after it registered
// the database, leaves volumes in the database directory (docs/poc/RESULTS.md,
// POC-25). On the next start the entrypoint neither runs createdb over them
// (which would fail with exit 1 and crash-loop the container) nor starts
// the registered, unfinished database (which fails with a fatal log recovery
// error): it starts only the manager and says what to do. Nothing is
// removed.
func TestEntrypoint_InterruptedStandaloneCreatedbIsNotRecreatedOrStarted(t *testing.T) {
	for _, stage := range []string{"unregistered", "registered"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, managerStub)
			f.env["CUBRID_CREATEDB_HANG"] = stage
			cmd := f.command()
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			f.killed(cmd, &out)
			if data, err := os.ReadFile(f.calls + ".marker"); err != nil || string(data) != entrypointMarker {
				t.Fatalf("createdb ran without the entrypoint's marker (err=%v): %q", err, data)
			}
			if err := os.Remove(f.calls); err != nil {
				t.Fatal(err)
			}

			delete(f.env, "CUBRID_CREATEDB_HANG")
			f.env["CUBRID_START_EXIT"] = "1"
			code, second := f.run()
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, second)
			}
			wantCalls(t, f.recorded(), callManagerStart)
			dir := filepath.Join(f.databases, dbName)
			marker := filepath.Join(dir, ".im-operation")
			if data, err := os.ReadFile(marker); err != nil || string(data) != entrypointMarker {
				t.Errorf("the marker was not kept as it was (err=%v): %q", err, data)
			}
			if _, err := os.Stat(filepath.Join(dir, dbName+"_lgat")); err != nil {
				t.Errorf("the volume createdb left was removed: %v", err)
			}
			for _, want := range []string{"is unfinished (" + marker + ")", "createdb did not complete", "remove " + dir} {
				if !strings.Contains(second, want) {
					t.Errorf("the log does not say %q:\n%s", want, second)
				}
			}
		})
	}
}

// A createdb that fails leaves its marker: the entrypoint starts nothing but
// the manager, now and at the next start, instead of exiting and being
// restarted into a createdb that fails again.
func TestEntrypoint_FailedStandaloneCreatedbStartsOnlyTheManager(t *testing.T) {
	f := newFixture(t, managerStub)
	f.env["CUBRID_CREATEDB_EXIT"] = "1"
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), "cubrid createdb", callManagerStart)
	marker := filepath.Join(f.databases, dbName, ".im-operation")
	if data, err := os.ReadFile(marker); err != nil || string(data) != entrypointMarker {
		t.Errorf("the marker was not kept (err=%v): %q", err, data)
	}
	if !strings.Contains(out, "createdb did not complete") {
		t.Errorf("the log does not say that createdb failed:\n%s", out)
	}
	if err := os.Remove(f.calls); err != nil {
		t.Fatal(err)
	}
	delete(f.env, "CUBRID_CREATEDB_EXIT")
	if code, out := f.run(); code != 0 {
		t.Fatalf("second start: exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), callManagerStart)
}

// A completed createdb removes the marker, so the next start starts the
// database.
func TestEntrypoint_CompletedStandaloneCreatedbClearsTheMarker(t *testing.T) {
	f := newFixture(t, managerStub)
	if code, out := f.run(); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if data, err := os.ReadFile(f.calls + ".marker"); err != nil || string(data) != entrypointMarker {
		t.Errorf("createdb ran without the entrypoint's marker (err=%v): %q", err, data)
	}
	if _, err := os.Lstat(filepath.Join(f.databases, dbName, ".im-operation")); !os.IsNotExist(err) {
		t.Errorf("the marker is left after a completed createdb (err=%v)", err)
	}
	if err := os.Remove(f.calls); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run(); code != 0 {
		t.Fatalf("second start: exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), "cubrid server start "+dbName, callManagerStart)
}

// A database directory that holds files although the database is not
// registered, and carries no marker (an interrupted createdb of an earlier
// image, or someone's data), is not created over: createdb would refuse it
// and exit 1. An empty directory is created into.
func TestEntrypoint_UnregisteredDirectoryWithFilesIsNotCreatedOver(t *testing.T) {
	for _, withFiles := range []bool{true, false} {
		t.Run(fmt.Sprintf("files=%t", withFiles), func(t *testing.T) {
			f := newFixture(t, managerStub)
			dir := filepath.Join(f.databases, dbName)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			volume := filepath.Join(dir, dbName+"_lgat")
			if withFiles {
				if err := os.WriteFile(volume, []byte("data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			code, out := f.run()
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if !withFiles {
				wantCalls(t, f.recorded(), "cubrid createdb", "cubrid server start "+dbName, callManagerStart)
				return
			}
			wantCalls(t, f.recorded(), callManagerStart)
			if data, err := os.ReadFile(volume); err != nil || string(data) != "data" {
				t.Errorf("the files in the directory were changed (err=%v): %q", err, data)
			}
			if !strings.Contains(out, "is not registered") || !strings.Contains(out, dir) {
				t.Errorf("the log does not say why nothing was created:\n%s", out)
			}
		})
	}
}

// A termination that arrives while the entrypoint's own createdb runs is
// handled when createdb returns: nothing is started after it, neither the
// server nor the manager.
func TestEntrypoint_TerminationDuringCreatedb(t *testing.T) {
	f := newFixture(t, blockingManagerStub)
	f.env["CUBRID_CREATEDB_SECONDS"] = "1"
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.recorded()) > 0 && strings.HasPrefix(f.recorded()[0], "cubrid createdb") }, &out)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM during createdb: %v\n%s", err, out.String())
	}
	wantCalls(t, f.recorded(), "cubrid createdb", callShutdown)
	// createdb ran to its end: the database is complete and registered, and
	// the next start starts it.
	if _, err := os.Lstat(filepath.Join(f.databases, dbName, ".im-operation")); !os.IsNotExist(err) {
		t.Errorf("the marker is left after a completed createdb (err=%v)", err)
	}
	data, err := os.ReadFile(filepath.Join(f.databases, "databases.txt"))
	if err != nil || !strings.HasPrefix(string(data), dbName+"\t") {
		t.Errorf("the database is not registered (err=%v): %q", err, data)
	}
}

// A termination during a createdb that then fails starts nothing either, and
// the marker stays.
func TestEntrypoint_TerminationDuringAFailingCreatedb(t *testing.T) {
	f := newFixture(t, blockingManagerStub)
	f.env["CUBRID_CREATEDB_SECONDS"] = "1"
	f.env["CUBRID_CREATEDB_EXIT"] = "1"
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.recorded()) > 0 && strings.HasPrefix(f.recorded()[0], "cubrid createdb") }, &out)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM during a failing createdb: %v\n%s", err, out.String())
	}
	wantCalls(t, f.recorded(), "cubrid createdb", callShutdown)
	marker := filepath.Join(f.databases, dbName, ".im-operation")
	if data, err := os.ReadFile(marker); err != nil || string(data) != entrypointMarker {
		t.Errorf("the marker was not kept (err=%v): %q", err, data)
	}
}

// When the marker cannot be written, createdb is not run: it would run
// unprotected. The log says that the preparation failed, not that createdb
// was interrupted, and only the manager runs.
func TestEntrypoint_UnwritableMarkerRunsNoCreatedb(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	f := newFixture(t, managerStub)
	dir := filepath.Join(f.databases, dbName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), callManagerStart)
	if !strings.Contains(out, "could not prepare createdb") {
		t.Errorf("the log does not say that createdb could not be prepared:\n%s", out)
	}
	if strings.Contains(out, "createdb did not complete") {
		t.Errorf("the log calls a createdb that never ran interrupted:\n%s", out)
	}
}

// On a standalone member, a marker that names no operation of the Instance
// Manager (empty, unreadable, a dangling link) is never resolved by the
// manager: the log gives the way out, not "waiting for the Instance
// Manager". A marker with an operation ID still waits for the manager.
func TestEntrypoint_StandaloneMarkerWithoutAnOperationID(t *testing.T) {
	const opID = "op-0123456789abcdef0123456789abcdef"
	markers := map[string]string{"empty": "", "garbage": "x\n", "dangling link": "", "operation": opID + "\n"}
	for name, value := range markers {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, managerStub)
			f.registerDatabase()
			dir := filepath.Join(f.databases, dbName)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, ".im-operation")
			var err error
			if name == "dangling link" {
				err = os.Symlink(filepath.Join(f.root, "missing"), marker)
			} else {
				err = os.WriteFile(marker, []byte(value), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			code, out := f.run()
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			wantCalls(t, f.recorded(), callManagerStart)
			waits := strings.Contains(out, "waiting for the Instance Manager")
			guides := strings.Contains(out, "remove "+dir)
			if name == "operation" {
				if !waits || guides {
					t.Errorf("a marker with an operation ID must wait for the manager:\n%s", out)
				}
				return
			}
			if waits || !guides {
				t.Errorf("a marker without an operation ID must give the way out:\n%s", out)
			}
		})
	}
}

// A marker that names no operation of the Instance Manager on an HA member
// still waits for the manager: the HA bootstrap refuses it and reports it.
func TestEntrypoint_HAMarkerWithoutAnOperationIDWaitsForTheManager(t *testing.T) {
	for name, value := range map[string]string{"empty": "", "garbage": "x\n"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, managerStub)
			f.env["CUBRID_COMPONENTS"] = componentsHA
			f.configureHA()
			f.registerDatabase()
			dir := filepath.Join(f.databases, dbName)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".im-operation"), []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out := f.run()
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			wantCalls(t, f.recorded(), callManagerStart)
			if !strings.Contains(out, "waiting for the Instance Manager") || strings.Contains(out, "remove "+dir) {
				t.Errorf("an HA member must wait for the manager:\n%s", out)
			}
		})
	}
}

// In a recovery bootstrap a restart waits for a restore, so the way out of an
// unfinished standalone database is to run the restore again.
func TestEntrypoint_UnfinishedStandaloneDatabaseInRecovery(t *testing.T) {
	f := newFixture(t, managerStub)
	f.env["CUBRID_BOOTSTRAP"] = bootstrapRecovery
	dir := filepath.Join(f.databases, dbName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".im-operation"), []byte(entrypointMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), callManagerStart)
	if !strings.Contains(out, "remove "+dir) || !strings.Contains(out, "run the restore again") ||
		strings.Contains(out, "restart this member") {
		t.Errorf("the log does not give the recovery way out:\n%s", out)
	}
}

// A marker this start wrote but could not make durable is removed again:
// createdb never ran, so the next start must not call it interrupted. Only
// that marker goes.
func TestEntrypoint_UnsyncedMarkerIsRemovedAndNoCreatedbRuns(t *testing.T) {
	f := newFixture(t, managerStub)
	f.tools = writeTools(t, f.root, map[string]string{"sync": "#!/bin/bash\nexit 1\n"})
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), callManagerStart)
	if !strings.Contains(out, "could not prepare createdb") {
		t.Errorf("the log does not say that createdb could not be prepared:\n%s", out)
	}
	dir := filepath.Join(f.databases, dbName)
	if _, err := os.Lstat(filepath.Join(dir, ".im-operation")); !os.IsNotExist(err) {
		t.Errorf("the marker this start wrote was left (err=%v)", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the database directory was removed: %v", err)
	}
	if err := os.Remove(f.calls); err != nil {
		t.Fatal(err)
	}
	f.tools = writeTools(t, f.root, map[string]string{"sync": "#!/bin/bash\nexit 0\n"})
	if code, out := f.run(); code != 0 {
		t.Fatalf("second start: exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(), "cubrid createdb", "cubrid server start "+dbName, callManagerStart)
}

// A termination handled after the last check before the server start, but
// before the start itself, is followed by the start: the script must stop
// that server before it exits instead of leaving it to be killed with the
// container. The window is a few builtins wide, so the test holds it open as
// TestEntrypoint_TerminationAroundTheStartOfTheManager does.
func TestEntrypoint_TerminationJustBeforeTheServerStart(t *testing.T) {
	const startLine = "      cubrid server start \"${CUBRID_DB}\" ;;\n"
	f := newFixture(t, blockingManagerStub)
	f.registerDatabase()
	release := filepath.Join(f.root, "release")
	script, err := os.ReadFile("entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(script), startLine) != 1 {
		t.Fatalf("entrypoint.sh has no single line %q to hold the window at", startLine)
	}
	window := `      echo window >> "${CALLS}"; while [ ! -e "` + release + `" ]; do :; done` + "\n"
	path := filepath.Join(f.root, "entrypoint-held.sh")
	held := strings.Replace(string(script), startLine, window+startLine, 1)
	if err := os.WriteFile(path, []byte(held), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := scriptCommand(t, path, f.tools, f.env)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return index(f.recorded(), "window") >= 0 }, &out)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return index(f.recorded(), callShutdown) >= 0 }, &out)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after a termination just before the server start: %v\n%s", err, out.String())
	}
	calls := f.recorded()
	start := index(calls, "cubrid server start "+dbName)
	last := -1
	for i, call := range calls {
		if call == callShutdown {
			last = i
		}
	}
	if start < 0 || last < start {
		t.Errorf("the server started after the termination was not stopped: %q", calls)
	}
	if managerStarted(calls) {
		t.Errorf("the manager was started after the termination: %q", calls)
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
	wantCalls(t, f.recorded(), callManagerStart, callShutdown, callManagerTerm)
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
	wantCalls(t, f.recorded(), callManagerStart, callShutdown, callManagerTerm)
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

// rootStubs stand in for the two commands only the root branch uses. chown
// records its call; gosu records the user it switches to and runs the rest as
// that user, which here means: with the id stand-in answering non-root.
var rootStubs = map[string]string{
	"chown": "#!/bin/bash\necho \"chown $*\" >> \"${CALLS}\"\n",
	"gosu":  "#!/bin/bash\necho \"gosu $1\" >> \"${CALLS}\"\nshift\nexport FAKE_UID=1000\nexec bash \"$@\"\n",
}

// Started as root (a plain `docker run --user 0`), the entrypoint hands the
// data directory to cubrid and starts again as that user; everything after
// that is the non-root path.
func TestEntrypoint_StartedAsRootHandsOverAndContinuesAsCubrid(t *testing.T) {
	f := newFixture(t, managerStub)
	f.tools = writeTools(t, f.root, rootStubs)
	f.env[envFakeUID] = uidRoot
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantCalls(t, f.recorded(),
		"chown -R cubrid:cubrid "+f.databases,
		"gosu cubrid",
		"cubrid createdb", "cubrid server start "+dbName, callManagerStart)
	if !strings.Contains(out, "started as root; continuing as cubrid") {
		t.Errorf("the log does not say that the user was switched:\n%s", out)
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
// On SIGTERM the entrypoint runs the one shutdown command and then ends the
// manager, in that order, whatever kind of member it is. It stops nothing
// itself.
func TestEntrypoint_TerminationRunsTheShutdownCommandThenEndsTheManager(t *testing.T) {
	for _, components := range []string{componentsServer, componentsHA} {
		t.Run(components, func(t *testing.T) {
			f := newFixture(t, blockingManagerStub)
			f.env["CUBRID_COMPONENTS"] = components
			if components != componentsServer {
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
			stop, term := index(calls, callShutdown), index(calls, callManagerTerm)
			if stop < 0 || term < 0 || stop > term {
				t.Errorf("want %q before %q, got %q", callShutdown, callManagerTerm, calls)
			}
			for _, call := range calls {
				if strings.HasSuffix(call, " stop") || strings.Contains(call, " stop ") {
					t.Errorf("the entrypoint stopped CUBRID itself: %q", call)
				}
			}
		})
	}
}

// A termination that arrives while CUBRID is still being started is handled
// when the start returns: the shutdown command runs, the manager is not
// started, and the script exits 0. As PID 1 of a container the script would
// otherwise ignore the signal.
func TestEntrypoint_TerminationDuringTheStart(t *testing.T) {
	f := newFixture(t, blockingManagerStub)
	f.registerDatabase()
	f.env["CUBRID_START_SECONDS"] = "1"
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return index(f.recorded(), "cubrid server start "+dbName) >= 0 }, &out)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM during the start: %v\n%s", err, out.String())
	}
	calls := f.recorded()
	if index(calls, callShutdown) < 0 {
		t.Errorf("the shutdown command was not run: %q", calls)
	}
	if managerStarted(calls) {
		t.Errorf("the manager was started after the termination: %q", calls)
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
	wantCalls(t, f.recorded(), callManagerStart, callShutdown, callManagerTerm)
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
	wantCalls(t, f.recorded(), callManagerStart, callShutdown, callManagerTerm)
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
		// Wait copies the output until the process is gone; the caller
		// reads it next.
		<-done
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

// A termination handled around the start of the Instance Manager finds no
// manager to signal: its PID is recorded in the command after the start. The
// script must still end the manager and exit, instead of waiting for a
// manager that nobody told to stop until the Pod's grace period ends (#377).
// Each window is a few builtins wide, so the test runs a copy of the script
// that holds it open: it records "window" and spins on builtins, during which
// bash runs the trap, until the test releases it.
func TestEntrypoint_TerminationAroundTheStartOfTheManager(t *testing.T) {
	const startLine = "\"${IM_BIN}\" &\n"
	for name, hold := range map[string]func(window string) string{
		// The manager runs, and its PID is not recorded yet.
		"after the start, before its PID is recorded": func(window string) string { return startLine + window },
		// The last check for a termination has passed, and the manager is
		// started next.
		"after the last check, before the start": func(window string) string { return window + startLine },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, blockingManagerStub)
			f.registerDatabase()
			release := filepath.Join(f.root, "release")
			script, err := os.ReadFile("entrypoint.sh")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(script), startLine) != 1 {
				t.Fatalf("entrypoint.sh has no single line %q to hold the window at", startLine)
			}
			window := `echo window >> "${CALLS}"; while [ ! -e "` + release + `" ]; do :; done` + "\n"
			path := filepath.Join(f.root, "entrypoint-held.sh")
			held := strings.Replace(string(script), startLine, hold(window), 1)
			if err := os.WriteFile(path, []byte(held), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := scriptCommand(t, path, f.tools, f.env)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return index(f.recorded(), "window") >= 0 }, &out)
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			// The trap runs the shutdown command in the window; only then is
			// the window released.
			waitFor(t, func() bool {
				calls := f.recorded()
				return index(calls, callShutdown) > index(calls, "window")
			}, &out)
			if err := os.WriteFile(release, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			// The manager may be ended before it could install its own
			// handler, so the script's status may be that of a signal. What
			// matters is that the script exits.
			var exit *exec.ExitError
			if err := waitExit(cmd); err != nil && !errors.As(err, &exit) {
				t.Fatalf("entrypoint after a termination %s: %v\n%s", name, err, out.String())
			}
		})
	}
}

const callWaitVolume = "manager wait-volume-free"

// holdVolume makes the wait stand-in report the volume as held until the
// returned function is called.
func (f *fixture) holdVolume() (release func()) {
	f.t.Helper()
	held := filepath.Join(f.root, "volume-held")
	if err := os.WriteFile(held, nil, 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.env["VOLUME_HELD"] = held
	return func() {
		if err := os.Remove(held); err != nil {
			f.t.Fatal(err)
		}
	}
}

// While another container's CUBRID process still holds its lock on the
// volume, the entrypoint creates, starts and recovers nothing and does not
// start the manager, which would start a restored or bootstrapped database:
// a second server on the same database diverges the log and loses
// acknowledged writes (#357, #472; docs/poc/RESULTS.md, POC-29). Once
// the lock is gone it goes on as before.
func TestEntrypoint_StartsNothingWhileTheVolumeIsHeld(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(f *fixture)
		after []string
	}{
		"new standalone": {func(f *fixture) {},
			[]string{"cubrid createdb", "cubrid server start " + dbName, callManagerStart}},
		"existing standalone": {func(f *fixture) { f.registerDatabase() },
			[]string{"cubrid server start " + dbName, callManagerStart}},
		"HA member": {func(f *fixture) {
			f.env["CUBRID_COMPONENTS"] = componentsHA
			f.configureHA()
			f.registerDatabase()
		}, []string{callHeartbeat, callManagerStart}},
		"HA member waiting for the bootstrap": {func(f *fixture) {
			f.env["CUBRID_COMPONENTS"] = componentsHA
			f.configureHA()
		}, []string{callManagerStart}},
		"recovery": {func(f *fixture) { f.env["CUBRID_BOOTSTRAP"] = bootstrapRecovery },
			[]string{callManagerStart}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, managerStub)
			tc.setup(f)
			release := f.holdVolume()
			cmd := f.command()
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return len(f.recorded()) > 0 }, &out)
			time.Sleep(500 * time.Millisecond)
			wantCalls(t, f.recorded(), callWaitVolume)
			if _, err := os.Stat(filepath.Join(f.databases, dbName)); name == "new standalone" && !os.IsNotExist(err) {
				t.Errorf("the database directory was created while the volume was held (err=%v)", err)
			}
			release()
			if err := waitExit(cmd); err != nil {
				t.Fatalf("entrypoint after the release: %v\n%s", err, out.String())
			}
			wantCalls(t, f.recorded(), append([]string{callWaitVolume}, tc.after...)...)
		})
	}
}

// A termination while the entrypoint waits for the volume ends the wait and
// starts nothing: no createdb, no server, no manager. The shutdown command
// still runs, so the stop is recorded as for any termination (#464).
func TestEntrypoint_TerminationWhileWaitingForTheVolume(t *testing.T) {
	f := newFixture(t, blockingManagerStub)
	f.holdVolume()
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.recorded()) > 0 }, &out)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM while waiting: %v\n%s", err, out.String())
	}
	wantCalls(t, f.recorded(), callWaitVolume, callShutdown)
	if _, err := os.Stat(filepath.Join(f.databases, dbName)); !os.IsNotExist(err) {
		t.Errorf("the database directory was created (err=%v)", err)
	}
}

// A wait that ends without the volume found free (the command failed) starts
// nothing and fails the container, which Kubernetes starts again.
func TestEntrypoint_FailedVolumeWaitStartsNothing(t *testing.T) {
	f := newFixture(t, managerStub)
	f.env["VOLUME_HELD"] = filepath.Join(f.root, "not-held")
	f.env["WAIT_EXIT"] = "1"
	code, out := f.run()
	if code == 0 {
		t.Fatalf("exit 0 after a failed wait:\n%s", out)
	}
	wantCalls(t, f.recorded(), callWaitVolume)
}
