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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The bucket and the object keys of the artifacts these tests store.
const (
	inputBucket    = "cubrid-backups"
	inputVolumeKey = "backup/appdb_bk0v000"
	inputInfoKey   = "backup/appdb_bkvinf"
)

// putArtifact stores a manifest at inputBucket/prefix that lists files (key to
// content) with their correct sizes and checksums, and the files themselves.
// mutate, when set, changes the manifest before it is stored.
func putArtifact(t *testing.T, store *fakeObjectStore, prefix string, files map[string]string, mutate func(*BackupManifest)) {
	t.Helper()
	m := BackupManifest{
		ManifestVersion: ManifestVersion, Database: dbName, CubridVersion: testCubridVersion,
		SourceRole: string(RoleSlave),
	}
	// In key order, so that which object fails is the same in every run.
	for _, key := range slices.Sorted(maps.Keys(files)) {
		content := files[key]
		m.Objects = append(m.Objects, ManifestObject{Key: key, SizeBytes: int64(len(content)), SHA256: checksumOf(content)})
		store.objects[inputBucket+"/"+path.Join(prefix, key)] = []byte(content)
	}
	if mutate != nil {
		mutate(&m)
	}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	store.objects[inputBucket+"/"+path.Join(prefix, ManifestObjectName)] = data
}

// restoreInput returns the directory a restore command was given with -B.
func restoreInput(t *testing.T, call string) string {
	t.Helper()
	fields := strings.Fields(call)
	for i, f := range fields {
		if f == "-B" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	t.Fatalf("the restore command %q has no -B directory", call)
	return ""
}

// filesBelow returns every entry below dir that is not a directory, by its
// slash-separated path relative to dir, with its content. A symbolic link is
// listed by where it points.
func filesBelow(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			to, _ := os.Readlink(p)
			got[filepath.ToSlash(rel)] = "link to " + to
			return nil
		}
		data, err := os.ReadFile(p) // #nosec G304 -- a test's temporary directory
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	return got
}

// inputCLI records, while the restore command runs, what the directory it
// was given holds.
func inputCLI(t *testing.T) (*restoreCLI, *map[string]string) {
	t.Helper()
	var seen map[string]string
	cli := &restoreCLI{}
	cli.onRun = func() {
		seen = filesBelow(t, restoreInput(t, cli.calls[len(cli.calls)-1]))
	}
	return cli, &seen
}

// keptFile is a file the restore must leave exactly as it was.
type keptFile struct {
	path    string
	content string
	modTime time.Time
}

func writeKeptFile(t *testing.T, p, content string) keptFile {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return keptFile{path: p, content: content, modTime: old}
}

func (k keptFile) check(t *testing.T) {
	t.Helper()
	fi, err := os.Stat(k.path)
	if err != nil {
		t.Errorf("the file %s that was in the staging directory before the restore is gone: %v", k.path, err)
		return
	}
	if data, _ := os.ReadFile(k.path); string(data) != k.content || !fi.ModTime().Equal(k.modTime) {
		t.Errorf("the file %s was changed to %q (modified %s), want %q (modified %s)",
			k.path, data, fi.ModTime(), k.content, k.modTime)
	}
}

// A file that an earlier attempt left in the staging directory, and that the
// current manifest does not list, was never verified against it. The restore
// command must not be given it, and the restore must not remove or change it
// either: the restore did not write it (#866).
func TestRestore_DoesNotGiveTheRestoreCommandALeftoverFileTheManifestDoesNotList(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	leftover := writeKeptFile(t, filepath.Join(req.StagingDir, "backup", "appdb_bk0v001"), "left by an earlier attempt")
	cli, seen := inputCLI(t)

	if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if want := map[string]string{stagedFileName: backupContent}; !maps.Equal(*seen, want) {
		t.Errorf("the restore command was given %v, want only the verified objects %v", *seen, want)
	}
	leftover.check(t)
}

// A download that failed, followed by a retry of another artifact into the
// same staging directory, cannot mix the two artifacts: the retry's restore
// command is given only the retry's objects, although the failed attempt
// staged one the retry's manifest does not list. A process killed during its
// download leaves its files behind the same way, and they are not given to
// the next attempt either (#866).
func TestRestore_ARetryAfterAFailedOrInterruptedDownloadDoesNotMixArtifacts(t *testing.T) {
	store := newFakeStore()
	putArtifact(t, store, "first", map[string]string{
		inputVolumeKey: "first volume",
		inputInfoKey:   "first info",
	}, nil)
	putArtifact(t, store, "second", map[string]string{
		inputInfoKey: "second info",
	}, nil)
	// The first artifact's info file arrives corrupted, after its volume
	// may already be staged.
	store.corruptKey = "first/backup/appdb_bkvinf"

	req := baseRestoreRequest(t, inputBucket, "first")
	// The staging directory existed before: the failed attempt does not
	// remove it.
	unrelated := writeKeptFile(t, filepath.Join(req.StagingDir, "unrelated"), "not the restore's")
	// What a process killed during its download left behind.
	killed := writeKeptFile(t,
		filepath.Join(req.StagingDir, "op-"+strings.Repeat("c", 32), "backup", "appdb_bk0v001"), "killed attempt's volume")

	first := rootsFor(req)
	first.Owner = "op-" + strings.Repeat("a", 32)
	if _, err := Restore(context.Background(), &restoreCLI{}, store, first, req); err == nil {
		t.Fatal("the restore of a corrupted artifact succeeded")
	}

	req.Prefix = "second"
	second := rootsFor(req)
	second.Owner = "op-" + strings.Repeat("b", 32)
	cli, seen := inputCLI(t)
	if _, err := Restore(context.Background(), cli, store, second, req); err != nil {
		t.Fatalf("Restore of the second artifact: %v", err)
	}
	if want := map[string]string{"appdb_bkvinf": "second info"}; !maps.Equal(*seen, want) {
		t.Errorf("the retry's restore command was given %v, want only the second artifact's objects %v", *seen, want)
	}
	unrelated.check(t)
	killed.check(t)
}

// An HA member's seed staging directory is named after the cluster and is
// the same whichever master or backup round it is seeded from. A seed from
// another source, after one that failed, is given only its own artifact's
// objects, not those of the earlier source that its manifest does not list
// (#866).
func TestRestore_ASeedFromAnotherSourceDoesNotCarryTheEarlierSourcesFiles(t *testing.T) {
	store := newFakeStore()
	seed := func(master string) func(*BackupManifest) {
		return func(m *BackupManifest) {
			m.ClusterUID = seedClusterUID
			m.SourceInstance = master
			m.SourceRole = string(RoleMaster)
		}
	}
	putArtifact(t, store, "demo-0/round-1", map[string]string{
		inputVolumeKey:         "round 1 volume",
		"backup/appdb_bk0v001": "round 1 second volume",
		inputInfoKey:           "round 1 info",
	}, seed("demo-0"))
	putArtifact(t, store, "demo-1/round-2", map[string]string{
		inputVolumeKey: "round 2 volume",
		inputInfoKey:   "round 2 info",
	}, seed("demo-1"))

	req := baseRestoreRequest(t, inputBucket, "demo-0/round-1")
	req.StagingDir = filepath.Join(filepath.Dir(req.StagingDir), "seed-"+seedClusterUID)
	req.SeedFromMaster = "demo-0"
	req.ExpectedClusterUID = seedClusterUID
	// The seed staging directory existed before the first round.
	if err := os.MkdirAll(req.StagingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	roots := rootsFor(req)
	roots.Host = haMemberHosts
	roots.Owner = "op-" + strings.Repeat("a", 32)
	failing := &restoreCLI{err: errors.New("restoreslave failed")}
	if _, err := Restore(context.Background(), failing, store, roots, req); err == nil {
		t.Fatal("the first round succeeded although restoreslave failed")
	}

	req.Prefix = "demo-1/round-2"
	req.SeedFromMaster = "demo-1"
	roots.Owner = "op-" + strings.Repeat("b", 32)
	cli, seen := inputCLI(t)
	if _, err := Restore(context.Background(), cli, store, roots, req); err != nil {
		t.Fatalf("Restore of the second round: %v", err)
	}
	want := map[string]string{"appdb_bk0v000": "round 2 volume", "appdb_bkvinf": "round 2 info"}
	if !maps.Equal(*seen, want) {
		t.Errorf("the second round's restoreslave was given %v, want only its own objects %v", *seen, want)
	}
}

// A process killed during a restore leaves its attempt's directory in the
// staging directory. Before the next restore runs, the manager removes the
// directory of an attempt whose operation has ended or that the store does
// not know, so that killed attempts do not fill the staging volume. It keeps
// the directory of an operation that is still running, and every entry that
// is not named after an operation (#866).
func TestServer_Restore_RemovesOnlyTheStagingOfEndedAttempts(t *testing.T) {
	objects, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ended, _, err := store.FindOrCreate(OpRestore, "ended", "h1", dbName)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Settle(ended.ID, func(o *Operation) { o.State = OpFailed; o.FailureReason = "killed" }); err != nil {
		t.Fatal(err)
	}
	running, _, err := store.FindOrCreate(OpBackup, "running", "h2", "otherdb")
	if err != nil {
		t.Fatal(err)
	}
	unknown := "op-" + strings.Repeat("d", 32)
	endedDir := filepath.Join(req.StagingDir, ended.ID)
	unknownDir := filepath.Join(req.StagingDir, unknown)
	writeKeptFile(t, filepath.Join(endedDir, "backup", "appdb_bk0v000"), "ended attempt")
	writeKeptFile(t, filepath.Join(unknownDir, "backup", "appdb_bk0v000"), "unknown attempt")
	kept := []keptFile{
		writeKeptFile(t, filepath.Join(req.StagingDir, running.ID, "backup", "appdb_bk0v000"), "running operation"),
		writeKeptFile(t, filepath.Join(req.StagingDir, "backup", "appdb_bk0v001"), "earlier layout"),
		writeKeptFile(t, filepath.Join(req.StagingDir, "unrelated"), "not the restore's"),
	}

	cli := &stepCLI{}
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).
		WithRestoreRoots(rootsFor(req)).WithStandaloneDatabase(dbName)
	h := srv.Handler()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRequest(http.MethodPost, pathRestorePrepare, strings.NewReader(string(body)))
	post.Header.Set("Authorization", "Bearer tok")
	post.Header.Set("Idempotency-Key", "restore-1")
	post.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("restore = %d: %s", rr.Code, rr.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	if final := pollUntilSettled(t, srv, h, op.ID); final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}

	for _, dir := range []string{endedDir, unknownDir, filepath.Join(req.StagingDir, op.ID)} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("the staging of an attempt that has ended, %s, is still there (err=%v)", dir, err)
		}
	}
	for _, k := range kept {
		k.check(t)
	}
}

// The reclaim before a restore removes the directory of an earlier attempt
// only when nothing may still use it. An operation whose record cannot be
// read may be running, and a directory a process holds a file in is still
// written, so both are kept. A link named like an operation is removed, and
// what it points to is not touched. The holders are looked for by the
// resolved path, so a staging root reached through a link still finds them
// (#866).
func TestServer_ReclaimRestoreAttempts_KeepsWhatMayStillBeUsed(t *testing.T) {
	const otherOp = "op-dddddddddddddddddddddddddddddddd"
	for _, tc := range []struct {
		name string
		// prepare lays out the entry otherOp of the staging directory dir,
		// whose links resolved are at resolved, and returns the process
		// list to scan.
		prepare func(t *testing.T, store *OperationStore, storeDir, dir, resolved string) (proc string, id string)
		kept    bool
		// linkedRoot reaches the staging root through a link.
		linkedRoot bool
	}{
		{name: "an operation whose record cannot be read", kept: true,
			prepare: func(t *testing.T, store *OperationStore, storeDir, dir, _ string) (string, string) {
				op, _, err := store.FindOrCreate(OpRestore, "earlier", "h", dbName)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Settle(op.ID, func(o *Operation) { o.State = OpFailed }); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(storeDir, op.ID+".json"), "{not json")
				writeKeptFile(t, filepath.Join(dir, op.ID, "backup", "appdb_bk0v000"), "volume")
				return t.TempDir(), op.ID
			}},
		{name: "a directory a process holds a file in", kept: true,
			prepare: func(t *testing.T, _ *OperationStore, _, dir, resolved string) (string, string) {
				writeKeptFile(t, filepath.Join(dir, otherOp, "backup", "appdb_bk0v000"), "volume")
				return fakeProc(t, map[string]struct {
					comm string
					fds  []string
				}{"42": {comm: "cub_admin", fds: []string{filepath.Join(resolved, otherOp, "backup", "appdb_bk0v000")}}}), otherOp
			}},
		{name: "a held directory below a staging root reached through a link", kept: true, linkedRoot: true,
			prepare: func(t *testing.T, _ *OperationStore, _, dir, resolved string) (string, string) {
				writeKeptFile(t, filepath.Join(dir, otherOp, "backup", "appdb_bk0v000"), "volume")
				return fakeProc(t, map[string]struct {
					comm string
					fds  []string
				}{"42": {comm: "cub_admin", fds: []string{filepath.Join(resolved, otherOp, "backup", "appdb_bk0v000")}}}), otherOp
			}},
		{name: "an ended attempt nothing holds", kept: false,
			prepare: func(t *testing.T, _ *OperationStore, _, dir, _ string) (string, string) {
				writeKeptFile(t, filepath.Join(dir, otherOp, "backup", "appdb_bk0v000"), "volume")
				return t.TempDir(), otherOp
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			realRoot := filepath.Join(base, "restore-staging")
			stagingRoot := realRoot
			if tc.linkedRoot {
				stagingRoot = filepath.Join(base, "linked-staging")
				if err := os.MkdirAll(realRoot, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(realRoot, stagingRoot); err != nil {
					t.Skipf("cannot create a symbolic link here: %v", err)
				}
			}
			req := RestoreRequest{Database: dbName, Bucket: "b", Prefix: "p",
				StagingDir: filepath.Join(stagingRoot, "uid-1"), TargetDir: filepath.Join(base, "databases")}
			storeDir := t.TempDir()
			store, err := NewOperationStore(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			proc, id := tc.prepare(t, store, storeDir, req.StagingDir, filepath.Join(realRoot, "uid-1"))
			srv := NewServer(&restoreCLI{}, "tok").WithOperationStore(store).
				WithRestoreRoots(RestoreRoots{Target: req.TargetDir, Staging: stagingRoot})
			srv.procRoot = proc

			srv.reclaimRestoreAttempts(req, "op-"+strings.Repeat("e", 32))

			_, err = os.Lstat(filepath.Join(req.StagingDir, id))
			if kept := err == nil; kept != tc.kept {
				t.Errorf("the earlier attempt's directory kept = %v (err=%v), want %v", kept, err, tc.kept)
			}
		})
	}
}

// A link named like an earlier attempt's directory is removed as a link: the
// directory it points to, outside the staging directory, is not touched.
func TestServer_ReclaimRestoreAttempts_RemovesALinkWithoutFollowingIt(t *testing.T) {
	const otherOp = "op-dddddddddddddddddddddddddddddddd"
	base := t.TempDir()
	req := RestoreRequest{Database: dbName, Bucket: "b", Prefix: "p",
		StagingDir: filepath.Join(base, "restore-staging", "uid-1"), TargetDir: filepath.Join(base, "databases")}
	outside := writeKeptFile(t, filepath.Join(base, "outside", "backup", "appdb_bk0v000"), "not staging")
	if err := os.MkdirAll(req.StagingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(req.StagingDir, otherOp)
	if err := os.Symlink(filepath.Join(base, "outside"), link); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&restoreCLI{}, "tok").WithOperationStore(store).
		WithRestoreRoots(RestoreRoots{Target: req.TargetDir, Staging: filepath.Dir(req.StagingDir)})
	srv.procRoot = t.TempDir()

	srv.reclaimRestoreAttempts(req, "op-"+strings.Repeat("e", 32))

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the link named like an earlier attempt is still there (err=%v)", err)
	}
	outside.check(t)
}

// Backup files an Instance Manager built before attempts had their own
// directories staged directly in the staging directory are kept, and the
// reclaim says so in a warning a person can act on (#866).
func TestServer_ReclaimRestoreAttempts_KeepsAndReportsTheEarlierLayout(t *testing.T) {
	base := t.TempDir()
	req := RestoreRequest{Database: dbName, Bucket: "b", Prefix: "p",
		StagingDir: filepath.Join(base, "restore-staging", "uid-1"), TargetDir: filepath.Join(base, "databases")}
	legacy := writeKeptFile(t, filepath.Join(req.StagingDir, "backup", "appdb_bk0v000"), "earlier layout")
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	srv := NewServer(&restoreCLI{}, "tok").WithOperationStore(store).
		WithRestoreRoots(RestoreRoots{Target: req.TargetDir, Staging: filepath.Dir(req.StagingDir)}).
		WithLogger(slog.New(slog.NewJSONHandler(&logged, nil)))
	srv.procRoot = t.TempDir()

	srv.reclaimRestoreAttempts(req, "op-"+strings.Repeat("e", 32))

	legacy.check(t)
	if !strings.Contains(logged.String(), `"event":"restore_staging_legacy"`) ||
		!strings.Contains(logged.String(), filepath.Join(req.StagingDir, "backup")) {
		t.Errorf("no warning names the earlier layout's directory: %s", logged.String())
	}
}
