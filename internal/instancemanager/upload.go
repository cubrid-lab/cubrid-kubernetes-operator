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
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// UploadSpec describes where a staged backup lives locally and where its objects
// go in object storage (ADR-0007).
type UploadSpec struct {
	// StagingDir is the local directory holding the `backupdb` output files.
	StagingDir string
	// Bucket / Prefix locate the artifact in object storage. Objects land under
	// <prefix>/ and the completion marker is <prefix>/manifest.json.
	Bucket string
	Prefix string
	// Manifest carries the backup metadata; its Objects list is filled in here.
	Manifest BackupManifest
}

// UploadResult reports the completed artifact (ADR-0007).
type UploadResult struct {
	ManifestURI    string
	ManifestDigest string
	SizeBytes      int64
}

// UploadBackup stages-then-uploads a completed local backup to object storage
// and writes manifest.json LAST as the atomic completion marker (ADR-0007).
// It uploads every regular file under StagingDir, records each object's size +
// SHA-256 in the manifest, verifies each upload landed (StatSize), then uploads
// the manifest. The manifest is NEVER written before all data objects succeed,
// so a partial upload can never be mistaken for a complete backup.
func UploadBackup(ctx context.Context, store ObjectStore, spec UploadSpec) (UploadResult, error) {
	files, err := stagedFiles(spec.StagingDir)
	if err != nil {
		return UploadResult{}, err
	}
	if len(files) == 0 {
		return UploadResult{}, fmt.Errorf("no backup files staged in %s", spec.StagingDir)
	}

	// Every file is opened through a handle on the staging directory, so
	// nothing outside it can be read.
	root, err := os.OpenRoot(spec.StagingDir)
	if err != nil {
		return UploadResult{}, fmt.Errorf("open the staging directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	manifest := spec.Manifest
	manifest.ManifestVersion = ManifestVersion
	manifest.Objects = make([]ManifestObject, 0, len(files))

	for _, rel := range files {
		sum, size, err := hashFile(root, rel)
		if err != nil {
			return UploadResult{}, err
		}
		key := path.Join(spec.Prefix, "backup", filepath.ToSlash(rel))
		if err := putFile(ctx, store, spec.Bucket, key, root, rel, size); err != nil {
			return UploadResult{}, err
		}
		// Verify the object landed with the expected size (ADR-0007 stage-then-
		// verify); a missing/short object must fail the whole backup.
		if got, err := store.StatSize(ctx, spec.Bucket, key); err != nil || got != size {
			return UploadResult{}, fmt.Errorf("upload verification failed for %s (size got=%d want=%d err=%v)", key, got, size, err)
		}
		manifest.Objects = append(manifest.Objects, ManifestObject{
			Key:       path.Join("backup", filepath.ToSlash(rel)),
			SizeBytes: size,
			SHA256:    sum,
		})
	}

	// Write manifest.json LAST — the atomic completion marker (ADR-0007).
	data, err := manifest.Marshal()
	if err != nil {
		return UploadResult{}, err
	}
	manifestKey := path.Join(spec.Prefix, ManifestObjectName)
	if _, err := store.Put(ctx, spec.Bucket, manifestKey, strings.NewReader(string(data)), int64(len(data)), "application/json"); err != nil {
		return UploadResult{}, fmt.Errorf("upload manifest: %w", err)
	}

	digest, err := manifest.Digest()
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{
		ManifestURI:    fmt.Sprintf("s3://%s/%s", spec.Bucket, manifestKey),
		ManifestDigest: digest,
		SizeBytes:      manifest.TotalSize(),
	}, nil
}

// stagedFiles returns the regular files under dir as sorted relative paths.
func stagedFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
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
		// A backup consists of ordinary files. A symbolic link would upload
		// whatever it points at, a named pipe would wait for a writer, and a
		// device is never backup output: each fails the backup.
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file (%s); a backup uploads regular files only", rel, describeFileType(d.Type()))
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan staging dir: %w", err)
	}
	slices.Sort(out)
	return out, nil
}

func describeFileType(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "symbolic link"
	case mode&os.ModeNamedPipe != 0:
		return "named pipe"
	case mode&os.ModeDevice != 0:
		return "device"
	case mode&os.ModeSocket != 0:
		return "socket"
	default:
		return "special file"
	}
}

// openStaged opens one staged file for reading through the staging root. The
// entry was a regular file when the directory was listed; it is checked again
// on the open file, and the open does not block, so an entry swapped for a
// link or a pipe in between is refused rather than followed or waited on.
func openStaged(root *os.Root, rel string) (*os.File, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", rel, err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file; a backup uploads regular files only", rel)
	}
	return f, nil
}

func hashFile(root *os.Root, rel string) (string, int64, error) {
	f, err := openStaged(root, rel)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", rel, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func putFile(ctx context.Context, store ObjectStore, bucket, key string, root *os.Root, rel string, size int64) error {
	f, err := openStaged(root, rel)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := store.Put(ctx, bucket, key, f, size, "application/octet-stream"); err != nil {
		return err
	}
	return nil
}
