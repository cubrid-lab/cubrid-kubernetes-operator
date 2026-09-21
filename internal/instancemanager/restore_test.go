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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// restoreCLI records restoredb invocations so tests can assert the command.
type restoreCLI struct {
	calls []string
	err   error
}

func (c *restoreCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	return "ok", c.err
}

func baseRestoreRequest(t *testing.T, bucket, prefix string) RestoreRequest {
	t.Helper()
	return RestoreRequest{
		Database:              dbName,
		Bucket:                bucket,
		Prefix:                prefix,
		ExpectedCubridVersion: testCubridVersion,
		StagingDir:            t.TempDir(),
		TargetDir:             t.TempDir(),
	}
}

func TestRestore_HappyPath(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	cli := &restoreCLI{}
	req := baseRestoreRequest(t, bucket, prefix)

	res, err := Restore(context.Background(), cli, store, req)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Database != dbName || res.CubridVersion != testCubridVersion {
		t.Errorf("result = %+v", res)
	}
	if len(cli.calls) != 1 || !strings.Contains(cli.calls[0], "restoredb -B") {
		t.Errorf("restoredb not invoked as expected: %v", cli.calls)
	}
	// The staged object must have been downloaded.
	if _, err := os.Stat(filepath.Join(req.StagingDir, "backup", stagedFileName)); err != nil {
		t.Errorf("staged object not downloaded: %v", err)
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

	if _, err := Restore(context.Background(), cli, store, req); err == nil {
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

	if _, err := Restore(context.Background(), cli, store, req); err == nil {
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

	if _, err := Restore(context.Background(), cli, store, req); err == nil {
		t.Error("expected restore to abort when an object checksum drifts")
	}
	if len(cli.calls) != 0 {
		t.Errorf("restoredb must not run on checksum drift: %v", cli.calls)
	}
}

func TestRestore_RequiresArgs(t *testing.T) {
	if _, err := Restore(context.Background(), &restoreCLI{}, newFakeStore(), RestoreRequest{Database: dbName}); err == nil {
		t.Error("expected error when required fields are missing")
	}
}
