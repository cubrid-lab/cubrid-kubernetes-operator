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
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

// RestoreRequest asks the manager to restore a backup artifact into an empty,
// operator-owned target (ADR-0008). Credentials come from the manager env, not
// the request body.
type RestoreRequest struct {
	// Database is the CUBRID database name to restore.
	Database string `json:"database"`
	// Bucket / Prefix locate the artifact (prefix holds manifest.json + backup/).
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
	// ExpectedCubridVersion, when set, must equal the manifest's engine version
	// (no cross-version restore in v1alpha1, ADR-0008/0009).
	ExpectedCubridVersion string `json:"expectedCubridVersion,omitempty"`
	// StagingDir is where downloaded objects land before restoredb.
	StagingDir string `json:"stagingDir"`
	// TargetDir is the empty DB directory to restore into; a pre-existing DB
	// there is a hard block (ADR-0008 wrong-target guard).
	TargetDir string `json:"targetDir"`
}

// RestoreResult reports a completed restore (ADR-0008).
type RestoreResult struct {
	Database      string `json:"database"`
	CubridVersion string `json:"cubridVersion"`
	ManifestURI   string `json:"manifestURI"`
}

// Restore verifies the artifact's trust (ADR-0007/0008), downloads its objects
// to staging, and runs `cubrid restoredb -B` into the empty target directory.
// It refuses to overwrite a pre-existing DB (wrong-target guard) and never
// reports success unless restoredb succeeds against a verified artifact.
func Restore(ctx context.Context, cli CLI, store ObjectStore, req RestoreRequest) (RestoreResult, error) {
	if req.Database == "" || req.Bucket == "" || req.Prefix == "" || req.StagingDir == "" || req.TargetDir == "" {
		return RestoreResult{}, fmt.Errorf("database, bucket, prefix, stagingDir and targetDir are required")
	}

	// ADR-0008 wrong-target guard: restore only into an empty target. A
	// pre-existing DB volume is a hard block, never dropped or overwritten.
	if hasExistingDB(req.TargetDir, req.Database) {
		return RestoreResult{}, fmt.Errorf("target %s already holds database %q; refusing to overwrite (ADR-0008)", req.TargetDir, req.Database)
	}

	exp := ManifestExpectation{Database: req.Database, CubridVersion: req.ExpectedCubridVersion}
	manifest, err := VerifyArtifact(ctx, store, req.Bucket, req.Prefix, exp)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("artifact verification failed: %w", err)
	}

	if err := downloadArtifact(ctx, store, req.Bucket, req.Prefix, manifest, req.StagingDir); err != nil {
		return RestoreResult{}, err
	}

	backupDir := filepath.Join(req.StagingDir, "backup")
	out, err := cli.Run(ctx, "cubrid", "restoredb", "-B", backupDir, req.Database)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restoredb failed: %w: %s", err, out)
	}

	return RestoreResult{
		Database:      req.Database,
		CubridVersion: manifest.CubridVersion,
		ManifestURI:   fmt.Sprintf("s3://%s/%s", req.Bucket, path.Join(req.Prefix, ManifestObjectName)),
	}, nil
}

// hasExistingDB reports whether targetDir already contains the database's main
// volume, indicating a populated DB that must not be overwritten.
func hasExistingDB(targetDir, database string) bool {
	if _, err := os.Stat(filepath.Join(targetDir, database)); err == nil {
		return true
	}
	return false
}

// downloadArtifact fetches every manifest-listed object into stagingDir,
// preserving its relative key. Verification already validated checksums.
func downloadArtifact(ctx context.Context, store ObjectStore, bucket, prefix string, manifest BackupManifest, stagingDir string) error {
	for _, obj := range manifest.Objects {
		dst := filepath.Join(stagingDir, filepath.FromSlash(obj.Key))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return fmt.Errorf("stage dir for %s: %w", obj.Key, err)
		}
		if err := downloadObject(ctx, store, bucket, path.Join(prefix, obj.Key), dst); err != nil {
			return err
		}
	}
	return nil
}

func downloadObject(ctx context.Context, store ObjectStore, bucket, key, dst string) error {
	r, err := store.Get(ctx, bucket, key)
	if err != nil {
		return fmt.Errorf("download %s: %w", key, err)
	}
	defer func() { _ = r.Close() }()
	f, err := os.Create(dst) // #nosec G304 -- dst is under a manager-owned staging dir
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}
