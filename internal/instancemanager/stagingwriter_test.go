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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// startHolder starts a process that holds path open for writing until it is
// released, as cub_server holds the file of a cancelled backupdb -C after
// the utility's process group has ended (POC-27). It is a child of the test,
// not of a utility's process group. It returns once the file is open. When
// procDir is set, the process is listed there as a link to its /proc entry,
// so that a scan of procDir sees only the test's own holders, not the host's
// other processes. The release kills the process, waits for it and removes
// the link. It never fails the test itself: it may run on the goroutine of
// a backup, so the caller checks the error on the test's goroutine.
func startHolder(path, procDir string) (release func(), err error) {
	cmd := exec.Command("sh", "-c", `exec 3>"$1"; echo open; exec sleep 60`, "sh", path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the holder: %w", err)
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if procDir != "" {
				_ = os.Remove(filepath.Join(procDir, pid))
			}
		})
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "open" {
		release()
		return nil, fmt.Errorf("the holder did not open %s: %q %v", path, line, err)
	}
	if procDir != "" {
		if err := os.Symlink(filepath.Join("/proc", pid), filepath.Join(procDir, pid)); err != nil {
			release()
			return nil, err
		}
	}
	return release, nil
}

// holdFile is startHolder on the test's goroutine.
func holdFile(t *testing.T, path, procDir string) (release func()) {
	t.Helper()
	release, err := startHolder(path, procDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

// cancelledBackupCLI stands in for a cancelled backupdb -C: its first
// backupdb leaves a process holding the backup volume open in its -D
// directory and fails, as Run does once the utility's group has ended while
// cub_server still writes. Later backupdb runs write a volume and succeed.
// The holder is listed in procDir, the process list the server scans.
type cancelledBackupCLI struct {
	procDir string
	mu      sync.Mutex
	calls   int
	dirs    []string
	release func()
	holdErr error
}

func newCancelledBackupCLI(t *testing.T) *cancelledBackupCLI {
	c := &cancelledBackupCLI{procDir: t.TempDir()}
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.release != nil {
			c.release()
		}
	})
	return c
}

func (c *cancelledBackupCLI) Run(_ context.Context, _ string, args ...string) (string, error) {
	if len(args) == 0 || args[0] != "backupdb" {
		return "ok", nil
	}
	var dir string
	for i, a := range args {
		if a == "-D" && i+1 < len(args) {
			dir = args[i+1]
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.dirs = append(c.dirs, dir)
	volume := filepath.Join(dir, dbName+"_bk0v000")
	if c.calls == 1 {
		c.release, c.holdErr = startHolder(volume, c.procDir)
		return "", errors.New("signal: killed")
	}
	if err := os.WriteFile(volume, []byte("new "+dbName), 0o600); err != nil {
		return "", err
	}
	return "ok", nil
}

func (c *cancelledBackupCLI) first() (dir string, release func(), err error, ran bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.dirs) == 0 {
		return "", nil, nil, false
	}
	return c.dirs[0], c.release, c.holdErr, true
}

func (c *cancelledBackupCLI) runs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func waitFirstBackup(t *testing.T, cli *cancelledBackupCLI) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if dir, release, err, ran := cli.first(); ran {
			if err != nil {
				t.Fatalf("the fake backupdb could not start its holder: %v", err)
			}
			return dir, release
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("backupdb did not run")
	return "", nil
}

// A failed backup does not remove its staging directory while a process
// outside the utility's process group still holds a file in it: the
// operation stays unfinished and the directory stays. Once the file is
// closed, the directory is removed and the operation ends Failed.
func TestServer_Backup_FailedBackupWaitsForTheStagingWriter(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backup-staging")
	destination := filepath.Join(root, "uid-1")
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	cli := newCancelledBackupCLI(t)
	var buf bytes.Buffer
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(newFakeStore()).
		WithBackupStagingRoot(root).WithLogger(testLogger(&buf, slog.LevelInfo))
	srv.procRoot = cli.procDir
	h := srv.Handler()

	rr := postBackup(t, h, "key-1", backupBodyOf(dbName, destination, "appdb"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("/v1/backup = %d (body %s), want 202", rr.Code, rr.Body.String())
	}
	var op Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &op)
	dir, release := waitFirstBackup(t, cli)

	time.Sleep(600 * time.Millisecond)
	if !exists(dir) {
		t.Fatalf("staging %s was removed while a process still held a file in it", dir)
	}
	if got, err := store.Get(op.ID); err != nil || got.State.IsTerminal() {
		t.Errorf("operation = %+v (err %v), want it unfinished while its staging is held", got, err)
	}

	release()
	final := pollUntilSettled(t, srv, h, op.ID)
	if final.State != OpFailed {
		t.Fatalf("final state = %s, want Failed", final.State)
	}
	if exists(dir) || exists(destination) {
		t.Errorf("staging %s (or its destination) is still there after the file was closed", dir)
	}
	if strings.Contains(final.FailureReason, "kept") {
		t.Errorf("reason = %q, want no kept staging once the file was closed", final.FailureReason)
	}
	lines := logLines(t, &buf)
	if len(withEvent(lines, "backup_staging_writer_wait")) == 0 {
		t.Errorf("no backup_staging_writer_wait event; log:\n%s", buf.String())
	}
	if len(withEvent(lines, "backup_staging_writer_closed")) == 0 {
		t.Errorf("no backup_staging_writer_closed event; log:\n%s", buf.String())
	}
}

// A failed backup whose staging is still held when the bound passes leaves
// the directory and its files as they are, and says so in its reason. A
// retry to the same destination neither removes nor reuses them while the
// file is held: it is refused as a conflict and backupdb does not run. Once
// the file is closed, a retry reclaims the old directory and completes with
// only its own volume.
func TestServer_Backup_StagingHeldPastTheBoundIsKeptAndNotReused(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backup-staging")
	destination := filepath.Join(root, "uid-1")
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	cli := newCancelledBackupCLI(t)
	objects := newFakeStore()
	var buf bytes.Buffer
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).
		WithBackupStagingRoot(root).WithLogger(testLogger(&buf, slog.LevelInfo))
	srv.stagingWriterBound = 300 * time.Millisecond
	srv.stagingWriterPoll = 20 * time.Millisecond
	srv.procRoot = cli.procDir
	h := srv.Handler()

	rr := postBackup(t, h, "key-1", backupBodyOf(dbName, destination, "appdb"))
	var op Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &op)
	dir, release := waitFirstBackup(t, cli)
	final := pollUntilSettled(t, srv, h, op.ID)
	volume := filepath.Join(dir, dbName+"_bk0v000")
	if final.State != OpFailed {
		t.Fatalf("final state = %s, want Failed", final.State)
	}
	if !exists(volume) {
		t.Fatalf("staging file %s was removed although a process still held it when the bound passed", volume)
	}
	if !strings.Contains(final.FailureReason, "kept") || !strings.Contains(final.FailureReason, "not uploaded or reused") {
		t.Errorf("reason = %q, want it to say that the held staging is kept, not uploaded or reused", final.FailureReason)
	}
	if len(objects.keys()) != 0 {
		t.Errorf("objects uploaded: %v, want none", objects.keys())
	}
	timeouts := withEvent(logLines(t, &buf), "backup_staging_writer_timeout")
	if len(timeouts) == 0 || timeouts[0][keyLevel] != levelWarn {
		t.Errorf("backup_staging_writer_timeout = %v, want a warning; log:\n%s", timeouts, buf.String())
	}

	retry := postBackup(t, h, "key-2", backupBodyOf(dbName, destination, "appdb-2"))
	if retry.Code != http.StatusConflict {
		t.Errorf("retry while the old staging is held = %d (body %s), want 409", retry.Code, retry.Body.String())
	}
	if !exists(volume) {
		t.Errorf("the retry removed the held staging file %s", volume)
	}
	if n := cli.runs(); n != 1 {
		t.Errorf("backupdb ran %d times, want only the cancelled run while its staging is held", n)
	}

	release()
	again := postBackup(t, h, "key-2", backupBodyOf(dbName, destination, "appdb-2"))
	if again.Code != http.StatusAccepted {
		t.Fatalf("retry after the file was closed = %d (body %s), want 202", again.Code, again.Body.String())
	}
	var second Operation
	_ = json.Unmarshal(again.Body.Bytes(), &second)
	if done := pollUntilSettled(t, srv, h, second.ID); done.State != OpCompleted {
		t.Fatalf("retry = %s (reason %q), want Completed", done.State, done.FailureReason)
	}
	if exists(dir) {
		t.Errorf("the closed staging %s of the failed backup was not reclaimed by the retry", dir)
	}
	got := manifestObjects(t, objects, testUpload.Bucket, "appdb-2")
	if len(got) != 1 || got["backup/"+dbName+"_bk0v000"] != sha256Hex("new "+dbName) {
		t.Errorf("manifest objects = %v, want only the retry's own volume", got)
	}
}

// fakeProc is a process list for stagingHolders: each process has a comm
// file and fd links to the given paths, which need not exist.
func fakeProc(t *testing.T, procs map[string]struct {
	comm string
	fds  []string
}) string {
	t.Helper()
	proc := t.TempDir()
	for pid, p := range procs {
		fd := filepath.Join(proc, pid, "fd")
		if err := os.MkdirAll(fd, 0o750); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(proc, pid, "comm"), p.comm+"\n")
		for i, target := range p.fds {
			if err := os.Symlink(target, filepath.Join(fd, strconv.Itoa(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeFile(t, filepath.Join(proc, "self-not-a-pid"), "")
	return proc
}

// Only a file at or below the directory counts, not one of a sibling whose
// name starts the same; a held file that was deleted still counts.
func TestStagingHolders_MatchesOnlyFilesBelowTheDirectory(t *testing.T) {
	const dir = "/stage/uid-1/op-1"
	proc := fakeProc(t, map[string]struct {
		comm string
		fds  []string
	}{
		"10": {"cub_server", []string{"/stage/uid-1/op-10/appdb_bk0v000", "/dev/null"}},
		"11": {"sleep", []string{"/stage/uid-1/op-1/appdb_bk0v000 (deleted)"}},
	})
	held, err := stagingHolders(proc, procCommReader(proc), dir)
	if err != nil || len(held) != 1 || !strings.HasPrefix(held[0], "11 sleep ") {
		t.Errorf("stagingHolders(%s) = %v, %v; want only process 11, which holds a deleted file below it", dir, held, err)
	}
	held, err = stagingHolders(proc, procCommReader(proc), "/stage/uid-1/op-10")
	if err != nil || len(held) != 1 || !strings.HasPrefix(held[0], "10 cub_server ") {
		t.Errorf("stagingHolders(op-10) = %v, %v; want process 10", held, err)
	}
}

// A CUBRID process whose open files cannot be read counts as a holder (an
// error, fail closed); an unreadable process of any other name is skipped,
// and so is the comm reader's answer for it. A process list that cannot be
// read is an error.
func TestStagingHolders_UnreadableProcesses(t *testing.T) {
	proc := fakeProc(t, map[string]struct {
		comm string
		fds  []string
	}{
		"20": {"cub_server", nil},
		"21": {"sshd", nil},
	})
	// Path digits are not process identity. Keep PID 21 in the wrapped
	// filesystem path even though only PID 20 belongs in the error.
	withDigits := filepath.Join(t.TempDir(), "proc-21")
	if err := os.Rename(proc, withDigits); err != nil {
		t.Fatal(err)
	}
	proc = withDigits
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory without permissions")
	}
	for _, pid := range []string{"20", "21"} {
		lockDir(t, filepath.Join(proc, pid, "fd"))
	}
	held, err := stagingHolders(proc, procCommReader(proc), "/stage/uid-1/op-1")
	if err == nil || !strings.Contains(err.Error(), "read the open files of 20 cub_server:") ||
		strings.Contains(err.Error(), "read the open files of 21 ") || len(held) != 0 {
		t.Errorf("stagingHolders = %v, %v; want an error naming only the unreadable cub_server", held, err)
	}

	onlyOther := fakeProc(t, map[string]struct {
		comm string
		fds  []string
	}{"21": {"sshd", nil}})
	lockDir(t, filepath.Join(onlyOther, "21", "fd"))
	if held, err := stagingHolders(onlyOther, procCommReader(onlyOther), "/stage/uid-1/op-1"); err != nil || len(held) != 0 {
		t.Errorf("stagingHolders with only an unreadable non-CUBRID process = %v, %v; want none and no error", held, err)
	}

	if _, err := stagingHolders(filepath.Join(proc, "missing"), procCommReader(proc), "/stage"); err == nil {
		t.Error("stagingHolders of a process list that cannot be read = nil error, want one")
	}
}

// A destination entry that is a link is not followed: a process that holds
// a file where the link points (here the database directory, which
// cub_server holds for as long as it runs) does not make it held. A backup
// to that destination reclaims the link as before, and nothing it pointed
// to is touched.
func TestServer_Backup_ReclaimsALinkedEntryAlthoughItsTargetIsHeld(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "backup-staging")
	destination := filepath.Join(root, "uid-1")
	databases := filepath.Join(base, "databases")
	volume := filepath.Join(databases, dbName+"_lgat")
	writeFile(t, volume, "database volume")
	const abandoned = "op-0123456789abcdef0123456789abcdef"
	if err := os.MkdirAll(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(destination, abandoned)
	if err := os.Symlink(databases, link); err != nil {
		t.Fatal(err)
	}
	procDir := t.TempDir()
	holdFile(t, volume, procDir)

	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	var buf bytes.Buffer
	srv := NewServer(newWritingCLI(), "tok").WithOperationStore(store).WithObjectStore(newFakeStore()).
		WithBackupStagingRoot(root).WithLogger(testLogger(&buf, slog.LevelInfo))
	srv.procRoot = procDir
	// The scan itself sees the holder of the database volume.
	if held, err := stagingHolders(procDir, procCommReader(procDir), databases); err != nil || len(held) == 0 {
		t.Fatalf("the holder of %s is not seen: %v, %v", volume, held, err)
	}
	h := srv.Handler()

	rr := postBackup(t, h, "key-1", backupBodyOf(dbName, destination, "appdb"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("/v1/backup to a destination with a linked entry = %d (body %s), want 202", rr.Code, rr.Body.String())
	}
	var op Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &op)
	if final := pollUntilSettled(t, srv, h, op.ID); final.State != OpCompleted {
		t.Fatalf("backup = %s (reason %q), want Completed", final.State, final.FailureReason)
	}
	if exists(link) {
		t.Errorf("the linked entry %s was kept; a link is removed, never followed", link)
	}
	if !exists(volume) {
		t.Errorf("%s, where the link pointed, was removed", volume)
	}
	if held := withEvent(logLines(t, &buf), "backup_staging_held"); len(held) != 0 {
		t.Errorf("backup_staging_held logged for a link: %v", held)
	}
}
