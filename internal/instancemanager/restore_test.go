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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// restoreCLI records restoredb invocations so tests can assert the command, and
// whether a given file existed when it ran (the staged backup).
type restoreCLI struct {
	calls     []string
	err       error
	checkPath string
	sawFile   bool
	// onRun, when set, runs while the command "executes".
	onRun func()
}

func (c *restoreCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if c.checkPath != "" {
		_, statErr := os.Stat(c.checkPath)
		c.sawFile = statErr == nil
	}
	if c.onRun != nil {
		c.onRun()
	}
	return "ok", c.err
}

// baseRestoreRequest lays out a test database root and staging root under one
// temporary directory, the way the manager confines them in production.
func baseRestoreRequest(t *testing.T, bucket, prefix string) RestoreRequest {
	t.Helper()
	base := t.TempDir()
	target := filepath.Join(base, "databases")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	return RestoreRequest{
		Database:              dbName,
		Bucket:                bucket,
		Prefix:                prefix,
		ExpectedCubridVersion: testCubridVersion,
		StagingDir:            filepath.Join(base, "restore-staging", "op"),
		TargetDir:             target,
	}
}

// rootsFor returns the roots baseRestoreRequest laid out.
func rootsFor(req RestoreRequest) RestoreRoots {
	return RestoreRoots{Target: req.TargetDir, Staging: filepath.Dir(req.StagingDir)}
}

func TestRestore_HappyPath(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	cli := &restoreCLI{checkPath: filepath.Join(req.StagingDir, "backup", stagedFileName)}

	res, err := Restore(context.Background(), cli, store, rootsFor(req), req)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Database != dbName || res.CubridVersion != testCubridVersion {
		t.Errorf("result = %+v", res)
	}
	want := "cubrid restoredb -u -B " + filepath.Join(req.StagingDir, "backup") + " " + dbName
	if len(cli.calls) != 1 || cli.calls[0] != want {
		t.Errorf("calls = %v, want [%s]", cli.calls, want)
	}
	// The staged object was downloaded before restoredb ran, and staging is
	// removed once the restore succeeds.
	if !cli.sawFile {
		t.Error("staged object was not present when restoredb ran")
	}
	if _, err := os.Stat(req.StagingDir); !os.IsNotExist(err) {
		t.Errorf("staging directory left behind after a successful restore (err=%v)", err)
	}
}

func TestRestore_WrongTargetGuard(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	// Pre-populate the target with an existing DB volume.
	if err := os.WriteFile(filepath.Join(req.TargetDir, dbName), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &restoreCLI{}

	if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err == nil {
		t.Error("expected refusal to overwrite an existing DB (ADR-0008 wrong-target guard)")
	}
	if len(cli.calls) != 0 {
		t.Errorf("restoredb must not run when the target is populated: %v", cli.calls)
	}
}

func TestRestore_TrustFailureAborts(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	req.ExpectedCubridVersion = "11.5.0" // manifest says 11.4.6 => trust fails
	cli := &restoreCLI{}

	if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err == nil {
		t.Error("expected restore to abort on a manifest trust failure")
	}
	if len(cli.calls) != 0 {
		t.Errorf("restoredb must not run when trust verification fails: %v", cli.calls)
	}
}

func TestRestore_ChecksumDriftAborts(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	store.corruptKey = stagedFileName
	req := baseRestoreRequest(t, bucket, prefix)
	cli := &restoreCLI{}

	if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err == nil {
		t.Error("expected restore to abort when an object checksum drifts")
	}
	if len(cli.calls) != 0 {
		t.Errorf("restoredb must not run on checksum drift: %v", cli.calls)
	}
}

func TestRestore_RequiresArgs(t *testing.T) {
	if _, err := Restore(context.Background(), &restoreCLI{}, newFakeStore(), RestoreRoots{}, RestoreRequest{Database: dbName}); err == nil {
		t.Error("expected error when required fields are missing")
	}
}

// databasesTxtEntry returns the fields of database's line in the target's
// databases.txt, or nil when there is none.
func databasesTxtEntry(t *testing.T, targetDir, database string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(targetDir, databasesTxt))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == database {
			return fields
		}
	}
	return nil
}

// restoredb needs the database registered at its target before it runs, and
// -u so that it uses that path instead of the one recorded in the backup
// (docs/poc/RESULTS.md, POC-11).
func TestRestore_RegistersTheTargetBeforeRestoredb(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	dbDir := filepath.Join(req.TargetDir, dbName)
	other := "otherdb\t\t/data/otherdb\tlocalhost\t/data/otherdb\tfile:/data/otherdb/lob\n"
	if err := os.WriteFile(filepath.Join(req.TargetDir, databasesTxt), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}

	var entryAtRun []string
	var dirAtRun, lobAtRun bool
	cli := &restoreCLI{}
	cli.onRun = func() {
		entryAtRun = databasesTxtEntry(t, req.TargetDir, dbName)
		info, err := os.Stat(dbDir)
		dirAtRun = err == nil && info.IsDir()
		// restoredb does not create the lob directory the entry names, and
		// a BLOB insert fails without it (docs/poc/RESULTS.md, POC-12).
		info, err = os.Stat(filepath.Join(dbDir, "lob"))
		lobAtRun = err == nil && info.IsDir()
	}

	if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !dirAtRun {
		t.Errorf("%s did not exist when restoredb ran", dbDir)
	}
	if !lobAtRun {
		t.Errorf("%s/lob did not exist when restoredb ran", dbDir)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	wantEntry := []string{dbName, dbDir, host, dbDir, "file:" + filepath.Join(dbDir, "lob")}
	if strings.Join(entryAtRun, "|") != strings.Join(wantEntry, "|") {
		t.Errorf("entry when restoredb ran = %q, want %q", entryAtRun, wantEntry)
	}
	if got := databasesTxtEntry(t, req.TargetDir, dbName); got == nil {
		t.Error("the entry is gone after a successful restore")
	}
	if got := databasesTxtEntry(t, req.TargetDir, "otherdb"); got == nil {
		t.Error("another database's entry was lost")
	}
}

// A failed restore leaves the target as it found it, so a retry is not
// stopped by the wrong-target guard.
func TestRestore_FailureUnregistersTheTarget(t *testing.T) {
	tests := []struct {
		name     string
		existing string // databases.txt before the restore; "" means no file
	}{
		{"no databases.txt before", ""},
		{"another database registered", "otherdb\t\t/data/otherdb\tlocalhost\t/data/otherdb\tfile:/data/otherdb/lob\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, bucket, prefix := stageUploadedArtifact(t)
			req := baseRestoreRequest(t, bucket, prefix)
			file := filepath.Join(req.TargetDir, databasesTxt)
			if tc.existing != "" {
				if err := os.WriteFile(file, []byte(tc.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dbDir := filepath.Join(req.TargetDir, dbName)
			cli := &restoreCLI{err: errors.New("exit status 1")}
			// restoredb left a partial volume behind.
			cli.onRun = func() { _ = os.WriteFile(filepath.Join(dbDir, dbName+"_vinf"), []byte("partial"), 0o600) }

			if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err == nil {
				t.Fatal("expected the restore to fail")
			}
			if len(cli.calls) != 1 {
				t.Fatalf("calls = %v, want one restoredb", cli.calls)
			}
			if _, err := os.Stat(dbDir); !os.IsNotExist(err) {
				t.Errorf("%s is still there after the failure (err=%v)", dbDir, err)
			}
			if got := databasesTxtEntry(t, req.TargetDir, dbName); got != nil {
				t.Errorf("the database is still registered after the failure: %q", got)
			}
			data, err := os.ReadFile(file)
			switch {
			case tc.existing == "" && !os.IsNotExist(err):
				t.Errorf("databases.txt did not exist before and is there now (err=%v, content %q)", err, data)
			case tc.existing != "" && string(data) != tc.existing:
				t.Errorf("databases.txt = %q, want it as before: %q", data, tc.existing)
			}

			// The same request can be retried.
			retry := &restoreCLI{}
			if _, err := Restore(context.Background(), retry, store, rootsFor(req), req); err != nil {
				t.Errorf("retry after a failed restore: %v", err)
			}
		})
	}
}
