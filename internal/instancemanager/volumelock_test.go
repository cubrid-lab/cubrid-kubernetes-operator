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
	"strings"
	"syscall"
	"testing"
	"time"
)

// envHoldLock makes the test binary a lock holder: it write-locks the file
// the variable names, as a CUBRID server does its active log, says "locked"
// and keeps the lock until its standard input is closed.
const envHoldLock = "IM_TEST_HOLD_POSIX_LOCK"

func TestMain(m *testing.M) {
	if path := os.Getenv(envHoldLock); path != "" {
		os.Exit(holdLock(path))
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
	for _, name := range []string{"appdb/appdb_lgat", "appdb_h-1/appdb_lgat", "appdb_h-1/appdb_lgla__lock"} {
		t.Run(name, func(t *testing.T) {
			databases := t.TempDir()
			for _, other := range []string{"appdb/appdb", "appdb/appdb_lgat", "appdb/appdb_lgat__lock",
				"appdb_h-1/appdb_lgat", "appdb_h-1/appdb_lgla__lock"} {
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
	line := "appdb\t" + filepath.Join(databases, "appdb") + "\th-0\t" + logDir + "\tfile:/x/lob\n"
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
	if err := os.MkdirAll(filepath.Join(root, "appdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if held, err := HeldVolumeLocks(root); err != nil || len(held) != 0 {
		t.Errorf("empty database directory: held = %+v, err = %v, want none", held, err)
	}
}

// A file that cannot be checked is not taken for free.
func TestHeldVolumeLocks_AnUncheckableFileIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without permission")
	}
	databases := t.TempDir()
	file := filepath.Join(databases, "appdb", "appdb_lgat")
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
	file := filepath.Join(databases, "appdb", "appdb_lgat")
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
	file := filepath.Join(databases, "appdb", "appdb_lgat")
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
	file := filepath.Join(databases, "appdb", "appdb_lgat")
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
