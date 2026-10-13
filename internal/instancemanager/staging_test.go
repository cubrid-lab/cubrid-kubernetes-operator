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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// writingCLI stands in for backupdb: it writes one backup volume,
// <database>_bk0v000 holding "new <database>", into its -D directory. It
// records, per database, the -D directory and how many entries that
// directory held when backupdb started. A database in hold waits for its
// channel to close before backupdb returns; one in fail returns an error
// after it wrote its volume.
type writingCLI struct {
	mu      sync.Mutex
	dirs    map[string]string
	entries map[string]int
	started chan string
	hold    map[string]chan struct{}
	fail    map[string]bool
}

func newWritingCLI() *writingCLI {
	return &writingCLI{
		dirs: map[string]string{}, entries: map[string]int{}, started: make(chan string, 8),
		hold: map[string]chan struct{}{}, fail: map[string]bool{},
	}
}

func (c *writingCLI) Run(ctx context.Context, _ string, args ...string) (string, error) {
	if len(args) == 0 || args[0] != "backupdb" {
		return "ok", nil
	}
	var dir, database string
	for i, a := range args {
		if a == "-D" && i+1 < len(args) {
			dir = args[i+1]
		}
		if name, ok := strings.CutSuffix(a, "@localhost"); ok {
			database = name
		}
	}
	existing, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.dirs[database], c.entries[database] = dir, len(existing)
	hold, fail := c.hold[database], c.fail[database]
	c.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, database+"_bk0v000"), []byte("new "+database), 0o600); err != nil {
		return "", err
	}
	c.started <- database
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if fail {
		return "backupdb: failed", errors.New("exit status 1")
	}
	return "ok", nil
}

func (c *writingCLI) dirOf(database string) (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirs[database], c.entries[database]
}

// manifestObjects reads the manifest the fake object store holds under
// bucket/prefix and returns its object keys and their checksums.
func manifestObjects(t *testing.T, objects *fakeObjectStore, bucket, prefix string) map[string]string {
	t.Helper()
	data, ok := objects.objects[bucket+"/"+prefix+"/"+ManifestObjectName]
	if !ok {
		t.Fatalf("no manifest at %s/%s; objects: %v", bucket, prefix, objects.keys())
	}
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	got := map[string]string{}
	for _, o := range m.Objects {
		got[o.Key] = o.SHA256
	}
	return got
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func backupBodyOf(database, destination, prefix string) string {
	body, _ := json.Marshal(BackupRequest{Database: database, Destination: destination,
		Upload: &BackupUpload{Bucket: testUpload.Bucket, Prefix: prefix}})
	return string(body)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// A backup interrupted by a restart of the manager leaves its files in the
// staging destination. A later backup to the same destination, as a seed
// retry sends, uploads only what its own backupdb wrote: no file of the
// interrupted attempt enters the new manifest.
func TestServer_Backup_RetryAfterRestartUploadsOnlyItsOwnFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backup-staging")
	destination := filepath.Join(root, "seed-uid-1")
	storeDir := t.TempDir()

	// The interrupted attempt: recorded as running when the manager died.
	before, err := NewOperationStore(storeDir)
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	old, _, err := before.FindOrCreate(OpBackup, "seed-backup-r0", "hash-r0", dbName)
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	if _, err := before.Update(old.ID, func(op *Operation) { op.State = OpRunningBackup }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// What it left: a volume directly in the destination, as the manager
	// staged before, and one in a directory named after its operation.
	writeFile(t, filepath.Join(destination, dbName+"_bk0v001"), "stale volume")
	writeFile(t, filepath.Join(destination, old.ID, dbName+"_bk0v002"), "stale volume")

	// The restart: the interrupted operation is recorded as Failed.
	store, err := NewOperationStore(storeDir)
	if err != nil {
		t.Fatalf("NewOperationStore after the restart: %v", err)
	}
	cli := newWritingCLI()
	objects := newFakeStore()
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).WithBackupStagingRoot(root)
	h := srv.Handler()

	rr := postBackup(t, h, "seed-backup-r1", backupBodyOf(dbName, destination, "seed/r1"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("/v1/backup = %d (body %s), want 202", rr.Code, rr.Body.String())
	}
	var op Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &op)
	final := pollUntilSettled(t, srv, h, op.ID)
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (reason %q), want Completed", final.State, final.FailureReason)
	}

	got := manifestObjects(t, objects, testUpload.Bucket, "seed/r1")
	want := map[string]string{"backup/" + dbName + "_bk0v000": sha256Hex("new " + dbName)}
	if len(got) != len(want) || got["backup/"+dbName+"_bk0v000"] != want["backup/"+dbName+"_bk0v000"] {
		t.Errorf("manifest objects = %v, want only what this backupdb wrote: %v", got, want)
	}
	dir, entries := cli.dirOf(dbName)
	if entries != 0 {
		t.Errorf("backupdb started in %s, which already held %d entries; want an empty directory", dir, entries)
	}
	if filepath.Dir(dir) != destination || filepath.Base(dir) != op.ID {
		t.Errorf("backupdb -D = %s, want %s/<operation id %s>", dir, destination, op.ID)
	}
	if final.StagingDir != dir {
		t.Errorf("operation records stagingDir %q, want the backupdb destination %q", final.StagingDir, dir)
	}
}

// Two backups of different databases whose requests name the same staging
// destination never remove each other's files: the second is refused while
// the first runs, and the first uploads exactly what its backupdb wrote.
func TestServer_Backup_SharedDestinationKeepsTheRunningBackupsFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backup-staging")
	destination := filepath.Join(root, "uid-1")
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	cli := newWritingCLI()
	release := make(chan struct{})
	cli.hold[dbName] = release
	cli.fail["otherdb"] = true
	objects := newFakeStore()
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).WithBackupStagingRoot(root)
	h := srv.Handler()

	first := postBackup(t, h, "key-appdb", backupBodyOf(dbName, destination, "appdb"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first /v1/backup = %d (body %s), want 202", first.Code, first.Body.String())
	}
	var opA Operation
	_ = json.Unmarshal(first.Body.Bytes(), &opA)
	waitStarted(t, cli, dbName)
	dirA, _ := cli.dirOf(dbName)
	volumeA := filepath.Join(dirA, dbName+"_bk0v000")

	second := postBackup(t, h, "key-otherdb", backupBodyOf("otherdb", destination, "otherdb"))
	if second.Code == http.StatusAccepted {
		var opB Operation
		_ = json.Unmarshal(second.Body.Bytes(), &opB)
		pollUntilTerminal(t, h, opB.ID)
	}
	if !exists(volumeA) {
		t.Errorf("the running backup's volume %s was removed by the backup of another database", volumeA)
	}

	close(release)
	final := pollUntilSettled(t, srv, h, opA.ID)
	if final.State != OpCompleted {
		t.Fatalf("first backup = %s (reason %q), want Completed", final.State, final.FailureReason)
	}
	got := manifestObjects(t, objects, testUpload.Bucket, "appdb")
	if len(got) != 1 || got["backup/"+dbName+"_bk0v000"] != sha256Hex("new "+dbName) {
		t.Errorf("manifest objects = %v, want only %s_bk0v000 of the first backup", got, dbName)
	}
}

// A request whose staging destination is owned by another running operation
// is refused with a conflict of its own, before an operation is recorded and
// before backupdb runs, and nothing in the destination changes. The same
// request repeated under its own key is not refused.
func TestServer_Backup_RefusesADestinationOwnedByARunningOperation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backup-staging")
	destination := filepath.Join(root, "uid-1")
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	cli := newWritingCLI()
	release := make(chan struct{})
	cli.hold[dbName] = release
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(newFakeStore()).WithBackupStagingRoot(root)
	h := srv.Handler()
	defer func() {
		close(release)
		srv.ops.Wait()
	}()

	bodyA := backupBodyOf(dbName, destination, "appdb")
	first := postBackup(t, h, "key-appdb", bodyA)
	var opA Operation
	_ = json.Unmarshal(first.Body.Bytes(), &opA)
	waitStarted(t, cli, dbName)
	dirA, _ := cli.dirOf(dbName)

	rr := postBackup(t, h, "key-otherdb", backupBodyOf("otherdb", destination, "otherdb"))
	if rr.Code != http.StatusConflict {
		t.Errorf("/v1/backup to a destination a running operation owns = %d (body %s), want 409", rr.Code, rr.Body.String())
	}
	var answer map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &answer)
	if !strings.Contains(answer[errKey], "staging destination") || !strings.Contains(answer[errKey], "running") {
		t.Errorf("error = %q, want one that names the staging destination owned by a running operation", answer[errKey])
	}
	if ops, err := store.FindByKey("key-otherdb"); err != nil || len(ops) != 0 {
		t.Errorf("operations recorded for the refused request: %d (err %v)", len(ops), err)
	}
	if dir, _ := cli.dirOf("otherdb"); dir != "" {
		t.Errorf("backupdb ran for the refused request with -D %s", dir)
	}
	entries, _ := os.ReadDir(destination)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{filepath.Base(dirA)}) {
		t.Errorf("destination holds %v, want only the running operation's directory %s", names, filepath.Base(dirA))
	}

	repeat := postBackup(t, h, "key-appdb", bodyA)
	var again Operation
	_ = json.Unmarshal(repeat.Body.Bytes(), &again)
	if repeat.Code != http.StatusAccepted || again.ID != opA.ID {
		t.Errorf("repeat of the running request = %d %q, want 202 with operation %s", repeat.Code, again.ID, opA.ID)
	}
}

// After a restart the manager removes the staging that operations which the
// store records as ended, or does not know, left behind; staging of an
// operation that is running, or whose record cannot be read, is never
// touched, nor is anything else in its destination, and nothing outside the
// staging root is.
func TestResumeAndServe_ReclaimsAbandonedBackupStaging(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backup-staging")
	storeDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "precious")
	writeFile(t, outside, "not staging")

	before, err := NewOperationStore(storeDir)
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	completed, _, _ := before.FindOrCreate(OpBackup, "key-completed", "h1", "db1")
	_, _ = before.Update(completed.ID, func(op *Operation) { op.State = OpCompleted })
	interrupted, _, _ := before.FindOrCreate(OpBackup, "key-interrupted", "h2", "db2")
	_, _ = before.Update(interrupted.ID, func(op *Operation) { op.State = OpUploading })

	store, err := NewOperationStore(storeDir) // the restart
	if err != nil {
		t.Fatalf("NewOperationStore after the restart: %v", err)
	}
	running, _, err := store.FindOrCreate(OpBackup, "key-running", "h3", "db3")
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	const unknown = "op-0123456789abcdef0123456789abcdef"
	const unreadable = "op-fedcba9876543210fedcba9876543210"
	writeFile(t, filepath.Join(storeDir, unreadable+".json"), "{not json")

	writeFile(t, filepath.Join(root, "uid-1", completed.ID, "db1_bk0v000"), "x")
	writeFile(t, filepath.Join(root, "uid-1", interrupted.ID, "db2_bk0v000"), "x")
	writeFile(t, filepath.Join(root, "uid-2", unknown, "db_bk0v000"), "x")
	writeFile(t, filepath.Join(root, "uid-2", "db_bk0v000"), "left directly in the destination")
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "uid-2", "link")); err != nil {
		t.Fatal(err)
	}
	runningVolume := filepath.Join(root, "uid-3", running.ID, "db3_bk0v000")
	writeFile(t, runningVolume, "x")
	runningLeftover := filepath.Join(root, "uid-3", "db3_bk0v001")
	writeFile(t, runningLeftover, "left directly in the destination")
	unreadableVolume := filepath.Join(root, "uid-4", unreadable, "db_bk0v000")
	writeFile(t, unreadableVolume, "x")
	notADestination := filepath.Join(root, "README")
	writeFile(t, notADestination, "not a staging directory")

	srv := NewServer(fakeCLI{out: "ok"}, "tok").WithOperationStore(store).WithBackupStagingRoot(root)
	if err := srv.ResumeAndServe(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("ResumeAndServe: %v", err)
	}

	for _, gone := range []string{
		filepath.Join(root, "uid-1"),
		filepath.Join(root, "uid-2"),
	} {
		if exists(gone) {
			t.Errorf("%s is still there; staging of an ended or unknown operation is removed after a restart", gone)
		}
	}
	for _, kept := range []string{runningVolume, runningLeftover, unreadableVolume, notADestination, outside} {
		if !exists(kept) {
			t.Errorf("%s was removed; it belongs to a running operation, one that cannot be judged, or is not staging", kept)
		}
	}
	if final, err := store.Get(running.ID); err != nil || final.State.IsTerminal() {
		t.Errorf("running operation = %+v (err %v), want it untouched", final, err)
	}
}

func waitStarted(t *testing.T, cli *writingCLI, database string) {
	t.Helper()
	select {
	case got := <-cli.started:
		if got != database {
			t.Fatalf("backupdb started for %s, want %s", got, database)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("backupdb did not start for %s", database)
	}
}
