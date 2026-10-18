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
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// envHoldLock makes the test binary a lock holder: it write-locks the file
// the variable names, as a CUBRID server does its active log, says "locked"
// and keeps the lock until its standard input is closed.
const envHoldLock = "IM_TEST_HOLD_POSIX_LOCK"

// envTakeMemberLock makes the test binary take the member lock on its file
// descriptor 3, as "instance-manager wait-volume-free" does on the
// descriptor the entrypoint passes it, say "locked" and exit.
const envTakeMemberLock = "IM_TEST_TAKE_MEMBER_LOCK"

func TestMain(m *testing.M) {
	if path := os.Getenv(envHoldLock); path != "" {
		os.Exit(holdLock(path))
	}
	if path := os.Getenv(envTakeMemberLock); path != "" {
		err := AcquireMemberLock(context.Background(), os.NewFile(3, "member lock"), path,
			slog.New(slog.NewTextHandler(io.Discard, nil)), 10*time.Millisecond, time.Hour)
		if err != nil {
			_, _ = os.Stdout.WriteString(err.Error() + "\n")
			os.Exit(4)
		}
		_, _ = os.Stdout.WriteString("locked\n")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func holdLock(path string) int {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 2
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock); err != nil {
		return 3
	}
	_, _ = os.Stdout.WriteString("locked\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// copiedApplierLock is the log applier's lock file next to the log it
// copies from the peer h-1 (POC-29).
const copiedApplierLock = "appdb_h-1/appdb_lgla__lock"

// lockHolder is another process holding a POSIX write lock on path.
type lockHolder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func holdLockInChild(t *testing.T, path string) *lockHolder {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envHoldLock+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &lockHolder{cmd: cmd, stdin: stdin}
	t.Cleanup(h.release)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		h.release()
		t.Fatalf("the lock holder did not lock %s: %q, %v (exit %v)", path, line, err, cmd.ProcessState)
	}
	return h
}

// release ends the holder; the kernel drops its lock with it.
func (h *lockHolder) release() {
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
}

// The probe sees a lock another process holds on a database's active log, the
// copied log or the applier's lock file, with the holder's PID, and sees the
// file free again once the holder is gone. Volumes nobody locks are not
// reported.
func TestHeldVolumeLocks_ReportsALockHeldByAnotherProcessUntilItEnds(t *testing.T) {
	for _, name := range []string{"appdb/appdb_lgat", "appdb_h-1/appdb_lgat", copiedApplierLock} {
		t.Run(name, func(t *testing.T) {
			databases := t.TempDir()
			for _, other := range []string{"appdb/appdb", "appdb/appdb_lgat", "appdb/appdb_lgat__lock",
				"appdb_h-1/appdb_lgat", copiedApplierLock} {
				writeFile(t, filepath.Join(databases, other), "x")
			}
			file := filepath.Join(databases, name)
			holder := holdLockInChild(t, file)

			held, err := HeldVolumeLocks(databases)
			if err != nil {
				t.Fatal(err)
			}
			if len(held) != 1 || held[0].File != file || int(held[0].PID) != holder.cmd.Process.Pid {
				t.Fatalf("held = %+v, want %s held by pid %d", held, file, holder.cmd.Process.Pid)
			}

			holder.release()
			held, err = HeldVolumeLocks(databases)
			if err != nil || len(held) != 0 {
				t.Fatalf("after the holder ended: held = %+v, err = %v, want none", held, err)
			}
		})
	}
}

// A database whose log directory databases.txt registers outside the root is
// checked too.
func TestHeldVolumeLocks_ChecksTheRegisteredLogDirectory(t *testing.T) {
	databases := t.TempDir()
	logDir := filepath.Join(t.TempDir(), "logs")
	file := filepath.Join(logDir, "appdb_lgat")
	writeFile(t, file, "x")
	line := "appdb\t" + filepath.Join(databases, dbName) + "\th-0\t" + logDir + "\tfile:/x/lob\n"
	if err := os.WriteFile(filepath.Join(databases, "databases.txt"), []byte("#db-name\n"+line), 0o644); err != nil {
		t.Fatal(err)
	}
	holdLockInChild(t, file)
	held, err := HeldVolumeLocks(databases)
	if err != nil || len(held) != 1 || held[0].File != file {
		t.Fatalf("held = %+v, err = %v, want %s", held, err, file)
	}
}

// Nothing there means nothing held: a new volume, or a database directory
// without the files.
func TestHeldVolumeLocks_MissingFilesAreFree(t *testing.T) {
	root := t.TempDir()
	for _, databases := range []string{filepath.Join(root, "missing"), root} {
		held, err := HeldVolumeLocks(databases)
		if err != nil || len(held) != 0 {
			t.Errorf("%s: held = %+v, err = %v, want none", databases, held, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, dbName), 0o755); err != nil {
		t.Fatal(err)
	}
	if held, err := HeldVolumeLocks(root); err != nil || len(held) != 0 {
		t.Errorf("empty database directory: held = %+v, err = %v, want none", held, err)
	}
}

// A database directory or a lock file reached through a symbolic link is
// checked like the real one, and a link that leads nowhere is not taken for
// free.
func TestHeldVolumeLocks_FollowsSymbolicLinksAndFailsClosed(t *testing.T) {
	t.Run("linked database directory", func(t *testing.T) {
		databases, elsewhere := t.TempDir(), t.TempDir()
		file := filepath.Join(elsewhere, dbName, "appdb_lgat")
		writeFile(t, file, "x")
		if err := os.Symlink(filepath.Join(elsewhere, dbName), filepath.Join(databases, dbName)); err != nil {
			t.Fatal(err)
		}
		holdLockInChild(t, file)
		held, err := HeldVolumeLocks(databases)
		if err != nil || len(held) != 1 || held[0].File != filepath.Join(databases, dbName, "appdb_lgat") {
			t.Fatalf("held = %+v, err = %v, want the linked directory's active log", held, err)
		}
	})
	t.Run("linked lock file", func(t *testing.T) {
		databases, elsewhere := t.TempDir(), t.TempDir()
		file := filepath.Join(elsewhere, "appdb_lgat")
		writeFile(t, file, "x")
		link := filepath.Join(databases, dbName, "appdb_lgat")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(file, link); err != nil {
			t.Fatal(err)
		}
		holdLockInChild(t, file)
		held, err := HeldVolumeLocks(databases)
		if err != nil || len(held) != 1 || held[0].File != link {
			t.Fatalf("held = %+v, err = %v, want %s", held, err, link)
		}
	})
	for _, name := range []string{dbName, copiedApplierLock} {
		t.Run("dangling "+name, func(t *testing.T) {
			databases := t.TempDir()
			link := filepath.Join(databases, name)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), link); err != nil {
				t.Fatal(err)
			}
			if held, err := HeldVolumeLocks(databases); err == nil {
				t.Fatalf("held = %+v, err = nil, want an error for the dangling link", held)
			}
		})
	}
}

// A file that cannot be checked is not taken for free.
func TestHeldVolumeLocks_AnUncheckableFileIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without permission")
	}
	databases := t.TempDir()
	file := filepath.Join(databases, dbName, "appdb_lgat")
	writeFile(t, file, "x")
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	if held, err := HeldVolumeLocks(databases); err == nil {
		t.Fatalf("held = %+v, err = nil, want an error", held)
	}
}

func jsonLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

// The wait does not end while the lock is held, says what it waits for, and
// ends once the holder is gone.
func TestWaitVolumeFree_WaitsWhileHeldAndEndsWhenReleased(t *testing.T) {
	databases := t.TempDir()
	file := filepath.Join(databases, dbName, "appdb_lgat")
	writeFile(t, file, "x")
	holder := holdLockInChild(t, file)

	var logs bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- WaitVolumeFree(context.Background(), databases, jsonLogger(&logs), 10*time.Millisecond, time.Hour)
	}()
	select {
	case err := <-done:
		t.Fatalf("the wait ended while the lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	holder.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not end after the lock was released")
	}
	out := logs.String()
	if strings.Count(out, `"event":"volume_in_use"`) != 1 || !strings.Contains(out, `"file":"`+file+`"`) {
		t.Errorf("want one volume_in_use line naming %s (logEvery is an hour):\n%s", file, out)
	}
	if !strings.Contains(out, `"event":"volume_free"`) {
		t.Errorf("want a volume_free line:\n%s", out)
	}
}

// A termination ends the wait with an error, never as free.
func TestWaitVolumeFree_EndsWithTheContextWhileHeld(t *testing.T) {
	databases := t.TempDir()
	file := filepath.Join(databases, dbName, "appdb_lgat")
	writeFile(t, file, "x")
	holdLockInChild(t, file)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var logs bytes.Buffer
	err := WaitVolumeFree(ctx, databases, jsonLogger(&logs), 10*time.Millisecond, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
	if strings.Contains(logs.String(), "volume_free") {
		t.Errorf("logged the volume as free:\n%s", logs.String())
	}
}

// A check that fails keeps the wait going and is logged.
func TestWaitVolumeFree_AFailedCheckWaits(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without permission")
	}
	databases := t.TempDir()
	file := filepath.Join(databases, dbName, "appdb_lgat")
	writeFile(t, file, "x")
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var logs bytes.Buffer
	if err := WaitVolumeFree(ctx, databases, jsonLogger(&logs), 10*time.Millisecond, time.Hour); err == nil {
		t.Fatal("the wait ended as free on a file it could not check")
	}
	if !strings.Contains(logs.String(), `"event":"volume_check_failed"`) {
		t.Errorf("want a volume_check_failed line:\n%s", logs.String())
	}
}

// openMemberLock creates the member lock file below databases and opens it
// as the entrypoint does (read and write), as a description of its own each
// time it is called.
func openMemberLock(t *testing.T, databases string) (*os.File, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("open file description locks are Linux's")
	}
	path := filepath.Join(databases, MemberLockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- a test directory
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, path
}

// takeMemberLockInChild has another process take the member lock on f,
// passed as its descriptor 3, and returns once that process has exited.
func takeMemberLockInChild(t *testing.T, f *os.File, path string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envTakeMemberLock+"="+path)
	cmd.ExtraFiles = []*os.File{f}
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "locked\n" {
		t.Fatalf("the child did not take the member lock: %q, %v", out, err)
	}
}

func memberLockHeld(t *testing.T, path string) bool {
	t.Helper()
	held, _, err := probeLock(path)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// The entrypoint opens the member lock file and keeps it open; the manager
// it starts takes the lock on that descriptor and exits. The lock stays held
// after the process that took it is gone, for as long as the descriptor is
// open, and is released when the last one closes. A POSIX lock would be gone
// with the process that took it (#475).
func TestAcquireMemberLock_HeldAfterTheTakerExitsUntilTheLastDescriptorCloses(t *testing.T) {
	f, path := openMemberLock(t, t.TempDir())
	takeMemberLockInChild(t, f, path)
	if !memberLockHeld(t, path) {
		t.Fatal("the member lock was released when the process that took it exited, while the descriptor was still open")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if memberLockHeld(t, path) {
		t.Fatal("the member lock is still held after the last descriptor was closed")
	}
}

// A second container of the member waits while the first holds the member
// lock, says so, takes nothing from it, and takes the lock once the first is
// gone.
func TestAcquireMemberLock_WaitsWhileAnotherContainerHoldsIt(t *testing.T) {
	databases := t.TempDir()
	first, path := openMemberLock(t, databases)
	takeMemberLockInChild(t, first, path)
	second, _ := openMemberLock(t, databases)

	var logs bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- AcquireMemberLock(context.Background(), second, path, jsonLogger(&logs), 10*time.Millisecond, time.Hour)
	}()
	select {
	case err := <-done:
		t.Fatalf("took the member lock while another container held it: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if !memberLockHeld(t, path) {
		t.Fatal("the first holder's lock is gone while it still holds the descriptor")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the member lock was not taken after the other holder was gone")
	}
	if !memberLockHeld(t, path) {
		t.Error("the member lock is not held after AcquireMemberLock returned")
	}
	out := logs.String()
	if strings.Count(out, `"event":"member_lock_busy"`) != 1 || !strings.Contains(out, `"file":"`+path+`"`) {
		t.Errorf("want one member_lock_busy line naming %s (logEvery is an hour):\n%s", path, out)
	}
	if !strings.Contains(out, `"event":"member_lock_acquired"`) {
		t.Errorf("want a member_lock_acquired line:\n%s", out)
	}
}

// A termination ends the wait with an error, and the lock was not taken.
func TestAcquireMemberLock_EndsWithTheContextWhileHeld(t *testing.T) {
	databases := t.TempDir()
	first, path := openMemberLock(t, databases)
	takeMemberLockInChild(t, first, path)
	second, _ := openMemberLock(t, databases)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var logs bytes.Buffer
	err := AcquireMemberLock(ctx, second, path, jsonLogger(&logs), 10*time.Millisecond, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
	if strings.Contains(logs.String(), "member_lock_acquired") {
		t.Errorf("logged the lock as taken:\n%s", logs.String())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if memberLockHeld(t, path) {
		t.Error("the member lock is held by the description whose wait was ended")
	}
}

// A descriptor that is not the member lock file, or none at all, is refused
// and locks nothing: the entrypoint would not hold the lock it relies on.
func TestAcquireMemberLock_RefusesADescriptorThatIsNotTheLockFile(t *testing.T) {
	databases := t.TempDir()
	_, path := openMemberLock(t, databases)
	other, err := os.Create(filepath.Join(databases, "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	closed, err := os.Open(path) // #nosec G304 -- a test directory
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	readOnly, err := os.Open(path) // #nosec G304 -- a test directory
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readOnly.Close() }()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, f := range map[string]*os.File{
		"another file": other, "a closed descriptor": closed, "a read-only descriptor": readOnly,
	} {
		if err := AcquireMemberLock(context.Background(), f, path, logger, 10*time.Millisecond, time.Hour); err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
	}
	if err := AcquireMemberLock(context.Background(), other, filepath.Join(databases, "missing"), logger,
		10*time.Millisecond, time.Hour); err == nil {
		t.Error("a missing lock file: err = nil, want a refusal")
	}
	if memberLockHeld(t, path) {
		t.Error("the member lock was taken through a descriptor that is not its file")
	}
}

// A lock file that is removed while the wait goes on is not taken for the
// lock: no other container opens the old file any more.
func TestAcquireMemberLock_RefusesALockFileRemovedWhileWaiting(t *testing.T) {
	databases := t.TempDir()
	first, path := openMemberLock(t, databases)
	takeMemberLockInChild(t, first, path)
	second, _ := openMemberLock(t, databases)
	done := make(chan error, 1)
	go func() {
		done <- AcquireMemberLock(context.Background(), second, path,
			slog.New(slog.NewTextHandler(io.Discard, nil)), 10*time.Millisecond, time.Hour)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("err = nil after the lock file was removed, want a refusal")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AcquireMemberLock did not return after the old file was released")
	}
}
