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
	"io"
	"os"
	"path/filepath"
	"testing"
)

// countingStore records every object read, so a test can prove that a rejected
// restore never touched the artifact.
type countingStore struct {
	*fakeObjectStore
	gets int
}

func (c *countingStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	c.gets++
	return c.fakeObjectStore.Get(ctx, bucket, key)
}

// assertNoSideEffects fails unless a rejected restore read nothing, ran nothing
// and created no staging directory (#119).
func assertNoSideEffects(t *testing.T, store *countingStore, cli *restoreCLI, staging string) {
	t.Helper()
	if store.gets != 0 {
		t.Errorf("object store read %d times before the request was rejected", store.gets)
	}
	if len(cli.calls) != 0 {
		t.Errorf("CLI ran before the request was rejected: %v", cli.calls)
	}
	if staging != "" {
		if _, err := os.Stat(staging); !os.IsNotExist(err) {
			t.Errorf("staging directory %s exists after rejection (err=%v)", staging, err)
		}
	}
}

// A request is validated before anything is read, written or run: the database
// name cannot carry a path, both directories are clean absolute paths that are
// not the filesystem root and do not overlap (#119, ADR-0008).
func TestRestore_RejectsInvalidRequestWithoutSideEffects(t *testing.T) {
	base := t.TempDir()
	cases := map[string]func(*RestoreRequest){
		"database with a path":       func(r *RestoreRequest) { r.Database = "../appdb" },
		"database with a slash":      func(r *RestoreRequest) { r.Database = "app/db" },
		"database starting with dot": func(r *RestoreRequest) { r.Database = ".appdb" },
		"relative target":            func(r *RestoreRequest) { r.TargetDir = "databases" },
		"unclean target":             func(r *RestoreRequest) { r.TargetDir = r.TargetDir + "/../x" },
		"root target":                func(r *RestoreRequest) { r.TargetDir = "/" },
		"relative staging":           func(r *RestoreRequest) { r.StagingDir = "staging" },
		"root staging":               func(r *RestoreRequest) { r.StagingDir = "/" },
		"staging equals target":      func(r *RestoreRequest) { r.StagingDir = r.TargetDir },
		"staging inside target":      func(r *RestoreRequest) { r.StagingDir = filepath.Join(r.TargetDir, "staging") },
		"target inside staging":      func(r *RestoreRequest) { r.TargetDir = filepath.Join(r.StagingDir, "target") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fake, bucket, prefix := stageUploadedArtifact(t)
			store := &countingStore{fakeObjectStore: fake}
			cli := &restoreCLI{}
			req := RestoreRequest{
				Database:   dbName,
				Bucket:     bucket,
				Prefix:     prefix,
				TargetDir:  filepath.Join(base, name, "databases"),
				StagingDir: filepath.Join(base, name, "staging"),
			}
			mutate(&req)
			if _, err := Restore(context.Background(), cli, store, req); err == nil {
				t.Fatal("expected the request to be rejected")
			}
			staging := req.StagingDir
			if !filepath.IsAbs(staging) || staging == "/" || staging == req.TargetDir {
				staging = ""
			}
			assertNoSideEffects(t, store, cli, staging)
		})
	}
}

// A database already registered in the target's databases.txt is in use even
// when its directory is somewhere else, and is never restored over (ADR-0008).
func TestRestore_RejectsDatabaseRegisteredInTarget(t *testing.T) {
	fake, bucket, prefix := stageUploadedArtifact(t)
	store := &countingStore{fakeObjectStore: fake}
	req := baseRestoreRequest(t, bucket, prefix)
	req.StagingDir = filepath.Join(t.TempDir(), "staging")
	entry := dbName + "\t/elsewhere/" + dbName + "\tlocalhost\t/elsewhere/" + dbName + "/log\tfile:/elsewhere/" + dbName + "/lob\n"
	if err := os.WriteFile(filepath.Join(req.TargetDir, "databases.txt"), []byte("#db-name\tvol-path\n"+entry), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &restoreCLI{}
	if _, err := Restore(context.Background(), cli, store, req); err == nil {
		t.Fatal("expected refusal: the database is registered in the target's databases.txt")
	}
	assertNoSideEffects(t, store, cli, req.StagingDir)
}

// Another database in databases.txt does not block the restore.
func TestRestore_AllowsOtherRegisteredDatabases(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	if err := os.WriteFile(filepath.Join(req.TargetDir, "databases.txt"),
		[]byte("#db-name\tvol-path\nother\t/x/other\tlocalhost\t/x/other/log\tfile:/x/other/lob\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), &restoreCLI{}, store, req); err != nil {
		t.Fatalf("Restore: %v", err)
	}
}

// When restoredb fails, the staging directory the restore created is removed,
// so a failed restore leaves no downloaded artifact behind.
func TestRestore_RemovesItsStagingDirectoryOnFailure(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	req.StagingDir = filepath.Join(t.TempDir(), "staging")
	cli := &restoreCLI{err: io.ErrUnexpectedEOF}
	if _, err := Restore(context.Background(), cli, store, req); err == nil {
		t.Fatal("expected restoredb failure to fail the restore")
	}
	if len(cli.calls) != 1 {
		t.Fatalf("restoredb calls = %v, want exactly one", cli.calls)
	}
	if _, err := os.Stat(req.StagingDir); !os.IsNotExist(err) {
		t.Errorf("staging directory left behind after a failed restore (err=%v)", err)
	}
}

// restoredb runs against the requested database with the verified backup that
// was staged under the request's staging directory, and nothing else.
func TestRestore_RunsRestoredbOnTheStagedBackup(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	cli := &restoreCLI{}
	if _, err := Restore(context.Background(), cli, store, req); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	want := "cubrid restoredb -B " + filepath.Join(req.StagingDir, "backup") + " " + dbName
	if len(cli.calls) != 1 || cli.calls[0] != want {
		t.Errorf("restoredb = %v, want [%s]", cli.calls, want)
	}
}
