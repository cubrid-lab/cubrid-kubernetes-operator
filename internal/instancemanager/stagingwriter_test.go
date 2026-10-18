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
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// holdFile starts a process that holds path open for writing until it is
// released, as cub_server holds the file of a cancelled backupdb -C after
// the utility's process group has ended (POC-27). It is a child of the test,
// not of a utility's process group. It returns once the file is open, and
// the release kills the process and waits for it.
func holdFile(t *testing.T, path string) (release func()) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `exec 3>"$1"; echo open; exec sleep 60`, "sh", path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the holder: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	t.Cleanup(release)
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "open" {
		t.Fatalf("the holder did not open %s: %q %v", path, line, err)
	}
	return release
}

// cancelledBackupCLI stands in for a cancelled backupdb -C: its first
// backupdb leaves a process holding the backup volume open in its -D
// directory and fails, as Run does once the utility's group has ended while
// cub_server still writes. Later backupdb runs write a volume and succeed.
type cancelledBackupCLI struct {
	t       *testing.T
	mu      sync.Mutex
	calls   int
	dirs    []string
	release func()
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
		c.release = holdFile(c.t, volume)
		return "", errors.New("signal: killed")
	}
	if err := os.WriteFile(volume, []byte("new "+dbName), 0o600); err != nil {
		return "", err
	}
	return "ok", nil
}

func (c *cancelledBackupCLI) first() (string, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.dirs) == 0 {
		return "", nil
	}
	return c.dirs[0], c.release
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
		if dir, release := cli.first(); release != nil {
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
	cli := &cancelledBackupCLI{t: t}
	var buf bytes.Buffer
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(newFakeStore()).
		WithBackupStagingRoot(root).WithLogger(testLogger(&buf, slog.LevelInfo))
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
	cli := &cancelledBackupCLI{t: t}
	objects := newFakeStore()
	var buf bytes.Buffer
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).
		WithBackupStagingRoot(root).WithLogger(testLogger(&buf, slog.LevelInfo))
	srv.stagingWriterBound = 300 * time.Millisecond
	srv.stagingWriterPoll = 20 * time.Millisecond
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

// Only a file below the directory counts, not one of a sibling whose name
// starts the same; a held file that was deleted still counts.
func TestStagingHolders_MatchesOnlyFilesBelowTheDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "op-1")
	sibling := filepath.Join(base, "op-10")
	writeFile(t, filepath.Join(sibling, "f"), "")
	holdFile(t, filepath.Join(sibling, "f"))

	if held, err := stagingHolders(dir); err != nil || len(held) != 0 {
		t.Errorf("stagingHolders(%s) = %v, %v; want none for a file of %s", dir, held, err, sibling)
	}
	if held, err := stagingHolders(sibling); err != nil || len(held) == 0 {
		t.Errorf("stagingHolders(%s) = %v, %v; want the holder", sibling, held, err)
	}
	if err := os.Remove(filepath.Join(sibling, "f")); err != nil {
		t.Fatal(err)
	}
	if held, err := stagingHolders(sibling); err != nil || len(held) == 0 {
		t.Errorf("stagingHolders(%s) after the held file was deleted = %v, %v; want the holder", sibling, held, err)
	}
}
