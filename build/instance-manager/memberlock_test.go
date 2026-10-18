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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The tests in this file run the entrypoint with the real Instance Manager
// for "wait-volume-free", built from ./cmd/instance-manager, against a real
// member lock: another container of the member is this test process holding
// an open file description lock on <databases>/.member-lock (#475).

const (
	memberLockName = ".member-lock"
	// caseNewStandalone names the setup of a new standalone member.
	caseNewStandalone = "new standalone"
	// fOFDSetlk and fOFDGetlk are Linux's F_OFD_SETLK and F_OFD_GETLK.
	fOFDSetlk = 37
	fOFDGetlk = 36

	// envProbeMemberLock makes the test binary print "held" or "free" for
	// the member lock file it names, as seen by a process of its own.
	envProbeMemberLock = "IM_TEST_PROBE_MEMBER_LOCK"

	// realWaitManagerStub hands "wait-volume-free" to the real manager
	// (${REAL_IM}), with the descriptors the entrypoint gave it, and stands in
	// for the rest as blockingManagerStub does.
	realWaitManagerStub = `#!/bin/bash
if [ "${1:-}" = "wait-volume-free" ]; then
  echo "manager wait-volume-free" >> "${CALLS}"
  exec "${REAL_IM}" wait-volume-free
fi
if [ "${1:-}" = "shutdown" ]; then echo "manager shutdown" >> "${CALLS}"; exit 0; fi
echo "manager start" >> "${CALLS}"
trap 'echo "manager term" >> "${CALLS}"; exit 0' TERM
while :; do sleep 0.05; done
`
	// probingCubridStub records, before each CUBRID command, whether a
	// process of its own sees the member lock held, then runs cubridStub.
	probingCubridStub = `#!/bin/bash
echo "member lock at cubrid $1 $2: $("${PROBE}")" >> "${CALLS}.lock"
exec "$(dirname "$0")/cubrid-real" "$@"
`
)

func TestMain(m *testing.M) {
	if path := os.Getenv(envProbeMemberLock); path != "" {
		os.Exit(probeMemberLock(path))
	}
	os.Exit(m.Run())
}

func probeMemberLock(path string) int {
	f, err := os.Open(path) // #nosec G304 -- a test directory
	if err != nil {
		_, _ = os.Stdout.WriteString("error: " + err.Error())
		return 0
	}
	defer func() { _ = f.Close() }()
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), fOFDGetlk, &lock); err != nil {
		_, _ = os.Stdout.WriteString("error: " + err.Error())
		return 0
	}
	if lock.Type == syscall.F_UNLCK {
		_, _ = os.Stdout.WriteString("free")
	} else {
		_, _ = os.Stdout.WriteString("held")
	}
	return 0
}

// syncBuffer is the entrypoint's output, read by the test while the
// entrypoint still writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForCalls waits until done holds for the recorded calls.
func (f *fixture) waitForCalls(done func([]string) bool, out *syncBuffer) {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !done(f.recorded()) {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for the calls, got %q:\n%s", f.recorded(), out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var (
	buildOnce sync.Once
	builtIM   string
	buildErr  error
	buildOut  []byte
)

// realInstanceManager builds ./cmd/instance-manager once for the package.
func realInstanceManager(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("open file description locks are Linux's")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "im-entrypoint-test-")
		if err != nil {
			buildErr = err
			return
		}
		builtIM = filepath.Join(dir, "instance-manager")
		cmd := exec.Command("go", "build", "-o", builtIM, "../../cmd/instance-manager")
		buildOut, buildErr = cmd.CombinedOutput()
	})
	if buildErr != nil {
		t.Fatalf("build the Instance Manager: %v\n%s", buildErr, buildOut)
	}
	return builtIM
}

// newMemberLockFixture is a fixture whose "wait-volume-free" is the real one.
func newMemberLockFixture(t *testing.T) *fixture {
	t.Helper()
	im := realInstanceManager(t)
	f := newFixture(t, realWaitManagerStub)
	f.env["REAL_IM"] = im
	f.env["IM_LOG_LEVEL"] = "info"
	if err := os.MkdirAll(f.databases, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// holdMemberLock makes this test process another container of the member
// that holds the member lock, until the returned function is called.
func (f *fixture) holdMemberLock() (release func()) {
	f.t.Helper()
	lock, err := os.OpenFile(filepath.Join(f.databases, memberLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	flock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(lock.Fd(), fOFDSetlk, &flock); err != nil {
		f.t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = lock.Close() }) }
	f.t.Cleanup(release)
	return release
}

// memberLockHeld says whether a process other than the entrypoint's sees the
// member lock held.
func (f *fixture) memberLockHeld() bool {
	f.t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envProbeMemberLock+"="+filepath.Join(f.databases, memberLockName))
	out, err := cmd.Output()
	if err != nil || (string(out) != "held" && string(out) != "free") {
		f.t.Fatalf("probe the member lock: %q, %v", out, err)
	}
	return string(out) == "held"
}

// While another container of the member holds the member lock, the
// entrypoint writes nothing to the volume and starts nothing: no createdb,
// no databases.txt, no server, heartbeat or manager. This holds also when
// the other container is mid-start and none of CUBRID's own locks is held
// yet (#475). Once the lock is free it goes on as before.
func TestEntrypoint_StartsNothingWhileAnotherContainerHoldsTheMemberLock(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(f *fixture)
		after []string
	}{
		caseNewStandalone: {func(f *fixture) {},
			[]string{"cubrid createdb", "cubrid server start " + dbName, callManagerStart}},
		"HA member": {func(f *fixture) {
			f.env["CUBRID_COMPONENTS"] = componentsHA
			f.configureHA()
			f.registerDatabase()
		}, []string{callHeartbeat, callManagerStart}},
		"recovery": {func(f *fixture) { f.env["CUBRID_BOOTSTRAP"] = bootstrapRecovery },
			[]string{callManagerStart}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMemberLockFixture(t)
			tc.setup(f)
			release := f.holdMemberLock()
			cmd := f.command()
			var out syncBuffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			f.waitForCalls(func(calls []string) bool { return len(calls) > 0 }, &out)
			// Long enough for an entrypoint that does not wait to create and
			// start everything with the stand-ins.
			time.Sleep(time.Second)
			calls := f.recorded()
			for _, call := range calls {
				if call != callWaitVolume {
					t.Fatalf("calls = %q while another container held the member lock, want none but %q\n%s",
						calls, callWaitVolume, out.String())
				}
			}
			if name == caseNewStandalone {
				for _, p := range []string{filepath.Join(f.databases, dbName), filepath.Join(f.databases, "databases.txt")} {
					if _, err := os.Stat(p); !os.IsNotExist(err) {
						t.Errorf("%s was written while another container held the member lock (err=%v)", p, err)
					}
				}
			}
			if !strings.Contains(out.String(), `"event":"member_lock_busy"`) {
				t.Errorf("the log does not say that the member lock is held:\n%s", out.String())
			}

			release()
			f.waitForCalls(managerStarted, &out)
			wantCalls(t, f.recorded(), append([]string{callWaitVolume}, tc.after...)...)
			if !strings.Contains(out.String(), `"event":"member_lock_acquired"`) {
				t.Errorf("the log does not say that the member lock was taken:\n%s", out.String())
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if err := waitExit(cmd); err != nil {
				t.Fatalf("entrypoint after SIGTERM: %v\n%s", err, out.String())
			}
		})
	}
}

// The entrypoint holds the member lock from before its first write to the
// volume until it exits: it is held when createdb and the server start run,
// after the process that took it has exited, and while the manager runs; it
// is gone once the entrypoint has exited.
func TestEntrypoint_HoldsTheMemberLockFromBeforeTheFirstWriteUntilItExits(t *testing.T) {
	f := newMemberLockFixture(t)
	bin := filepath.Join(f.root, "cubrid", "bin")
	if err := os.Rename(filepath.Join(bin, "cubrid"), filepath.Join(bin, "cubrid-real")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "cubrid"), []byte(probingCubridStub), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env["PROBE"] = os.Args[0]
	f.env[envProbeMemberLock] = filepath.Join(f.databases, memberLockName)
	cmd := f.command()
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.waitForCalls(managerStarted, &out)
	probes, err := os.ReadFile(f.calls + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	want := "member lock at cubrid createdb --db-volume-size=512M: held\n" +
		"member lock at cubrid server start: held\n"
	if string(probes) != want {
		t.Errorf("what the CUBRID commands saw of the member lock:\n%s\nwant:\n%s\nentrypoint output:\n%s",
			probes, want, out.String())
	}
	if !f.memberLockHeld() {
		t.Error("the member lock is not held while the manager runs")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM: %v\n%s", err, out.String())
	}
	// Outside a container nothing ends the stand-ins' children with the
	// entrypoint: a sleep of the manager stand-in, which inherited the
	// descriptor, may outlive it by its 50 ms.
	deadline := time.Now().Add(5 * time.Second)
	for f.memberLockHeld() {
		if time.Now().After(deadline) {
			t.Fatal("the member lock is still held after the entrypoint exited")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A termination while the entrypoint waits for the member lock starts
// nothing and still records the stop, as for any termination (#464).
func TestEntrypoint_TerminationWhileWaitingForTheMemberLock(t *testing.T) {
	f := newMemberLockFixture(t)
	f.holdMemberLock()
	cmd := f.command()
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.waitForCalls(func(calls []string) bool { return len(calls) > 0 }, &out)
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after SIGTERM while waiting: %v\n%s", err, out.String())
	}
	wantCalls(t, f.recorded(), callWaitVolume, callShutdown)
	if !strings.Contains(out.String(), "terminated while waiting for the data volume: nothing is started") {
		t.Errorf("the log does not say that nothing was started:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(f.databases, dbName)); !os.IsNotExist(err) {
		t.Errorf("the database directory was created (err=%v)", err)
	}
}

// A member lock file that cannot be opened for writing stops the container
// with nothing written or started: an error never counts as the lock.
func TestEntrypoint_UnopenableMemberLockStartsNothing(t *testing.T) {
	f := newFixture(t, managerStub)
	if err := os.MkdirAll(filepath.Join(f.databases, memberLockName), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out := f.run()
	if code == 0 {
		t.Fatalf("exit 0 with a member lock that cannot be opened:\n%s", out)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Errorf("calls = %q, want none", calls)
	}
	if !strings.Contains(out, "member lock") {
		t.Errorf("the log does not name the member lock:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(f.databases, dbName)); !os.IsNotExist(err) {
		t.Errorf("the database directory was created (err=%v)", err)
	}
}

// Started as root, the entrypoint takes the member lock and waits for the
// volume before its chown, which writes to the volume, and again as cubrid.
func TestEntrypoint_StartedAsRootWaitsBeforeTheChown(t *testing.T) {
	f := newFixture(t, managerStub)
	f.tools = writeTools(t, f.root, rootStubs)
	f.env[envFakeUID] = uidRoot
	release := f.holdVolume()
	cmd := f.command()
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.waitForCalls(func(calls []string) bool { return len(calls) > 0 }, &out)
	time.Sleep(500 * time.Millisecond)
	wantCalls(t, f.recorded(), callWaitVolume)
	release()
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after the release: %v\n%s", err, out.String())
	}
	wantCalls(t, f.recorded(), callWaitVolume, "chown -R cubrid:cubrid "+f.databases, "gosu cubrid",
		callWaitVolume, "cubrid createdb", "cubrid server start "+dbName, callManagerStart)
}
