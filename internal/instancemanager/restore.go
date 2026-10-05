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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
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
	// SeedFromMaster is the host name of the running master the backup was
	// taken on. When set, this member is restored as a slave of that master.
	// Empty for a restore into a new cluster, which has no master to follow.
	SeedFromMaster string `json:"seedFromMaster,omitempty"`
}

// RestoreRoots are the only directories a restore may touch, set from the
// manager's own configuration, never from a request (#119): TargetDir must be
// Target itself, and StagingDir must be one directory directly below Staging.
type RestoreRoots struct {
	// Target is the database root, $CUBRID_DATABASES.
	Target string
	// Staging is the parent of per-restore staging directories.
	Staging string
	// Host is the host column of the databases.txt entry a restore writes:
	// the HA member list for an HA member (docs/poc/RESULTS.md, POC-13).
	// Empty means this member's own host name.
	Host string
	// Owner is the ID of the durable operation this restore runs for. The
	// database directory is marked with it until restoredb has succeeded, so
	// that a later attempt can tell an interrupted restore of its own from
	// someone's data. Empty writes no marker.
	Owner string
}

// RestoreResult reports a completed restore (ADR-0008).
type RestoreResult struct {
	Database      string `json:"database"`
	CubridVersion string `json:"cubridVersion"`
	ManifestURI   string `json:"manifestURI"`
}

// Restore verifies the artifact's trust (ADR-0007/0008), downloads its objects
// to staging, registers the database at the empty target and runs
// `cubrid restoredb -u -B` into it.
// It refuses to overwrite a pre-existing DB (wrong-target guard) and never
// reports success unless restoredb succeeds against a verified artifact.
func Restore(ctx context.Context, cli CLI, store ObjectStore, roots RestoreRoots, req RestoreRequest) (result RestoreResult, err error) {
	// Everything below is checked before anything is read, written or run, so a
	// rejected request leaves no side effects (#119). From here on only the
	// paths confine builds from the manager's roots are used, never the
	// request's fields.
	if err := validateRestoreRequest(req); err != nil {
		return RestoreResult{}, err
	}
	targetDir, stagingDir, err := confine(req, roots)
	if err != nil {
		return RestoreResult{}, err
	}
	// Only an HA member can be restored as the slave of a master; roots.Host
	// is the HA member list, which the manager sets for such a member.
	if req.SeedFromMaster != "" && roots.Host == "" {
		return RestoreResult{}, fmt.Errorf("seedFromMaster %q was given, but this member is not an HA member", req.SeedFromMaster)
	}

	// ADR-0008 wrong-target guard: restore only into an empty target. A
	// pre-existing DB volume, or the database already registered in the
	// target's databases.txt, is a hard block, never dropped or overwritten.
	if hasExistingDB(targetDir, req.Database) {
		return RestoreResult{}, fmt.Errorf("target %s already holds database %q; refusing to overwrite (ADR-0008)", targetDir, req.Database)
	}
	registered, err := registeredInDatabasesTxt(targetDir, req.Database)
	if err != nil {
		return RestoreResult{}, err
	}
	if registered {
		return RestoreResult{}, fmt.Errorf("database %q is already registered in %s; refusing to overwrite (ADR-0008)",
			req.Database, filepath.Join(targetDir, databasesTxt))
	}

	// The staging directory is removed after a successful restore, and after a
	// failed one when this restore created it, so no downloaded artifact is left
	// behind. It is always strictly below roots.Staging.
	_, statErr := os.Stat(stagingDir)
	created := os.IsNotExist(statErr)
	defer func() {
		if err == nil || created {
			_ = os.RemoveAll(stagingDir)
		}
	}()

	// Every object is read once, into the staging directory, and checked
	// against the manifest as it is written: what restoredb reads below is
	// what was verified, not a second copy (#195).
	exp := ManifestExpectation{Database: req.Database, CubridVersion: req.ExpectedCubridVersion}
	manifest, err := stageArtifact(ctx, store, req.Bucket, req.Prefix, exp, stagingDir)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("artifact verification failed: %w", err)
	}

	// restoredb needs the database registered at its target, and -u to place
	// the volumes there instead of at the paths recorded in the backup
	// (docs/poc/RESULTS.md, POC-11). A failed restore takes the registration
	// and the directory back, so a retry finds an empty target again.
	unregister, err := registerRestoreTarget(targetDir, req.Database, roots.Host, roots.Owner)
	if err != nil {
		return RestoreResult{}, err
	}
	backupDir := filepath.Join(stagingDir, "backup")
	// A member seeded from a running master is restored with restoreslave,
	// which also writes the replication catalog from the backup: the member
	// then applies the master's log from the backup's position. With a plain
	// restoredb it would start at the master's current position and never
	// receive what was committed in between (docs/poc/RESULTS.md, POC-14).
	command := []string{"restoredb", "-u", "-B", backupDir, req.Database}
	if req.SeedFromMaster != "" {
		command = []string{"restoreslave", "-u", "-s", "master", "-m", req.SeedFromMaster, "-B", backupDir, req.Database}
	}
	out, err := cli.Run(ctx, "cubrid", command...)
	if err != nil {
		unregister()
		return RestoreResult{}, fmt.Errorf("%s failed: %w: %s", command[0], err, out)
	}
	if err := clearOwned(targetDir, req.Database); err != nil {
		return RestoreResult{}, err
	}

	return RestoreResult{
		Database:      req.Database,
		CubridVersion: manifest.CubridVersion,
		ManifestURI:   fmt.Sprintf("s3://%s/%s", req.Bucket, path.Join(req.Prefix, ManifestObjectName)),
	}, nil
}

// databasesTxt is CUBRID's database location file in $CUBRID_DATABASES.
const databasesTxt = "databases.txt"

// databaseNamePattern is a database name that cannot carry a path: a letter,
// then letters, digits or underscores.
var databaseNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// hostLabelPattern is a DNS label, the form of an HA member's host name
// (ADR-0004): lower-case letters, digits and hyphens, at most 63 characters,
// not starting or ending with a hyphen.
var hostLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// validateRestoreRequest rejects a request that could restore somewhere other
// than an explicit, separate target (#119): every field is required, the
// database name cannot carry a path, and the staging and target directories are
// clean absolute paths, not the filesystem root, and do not contain each other.
func validateRestoreRequest(req RestoreRequest) error {
	if req.Database == "" || req.Bucket == "" || req.Prefix == "" || req.StagingDir == "" || req.TargetDir == "" {
		return fmt.Errorf("database, bucket, prefix, stagingDir and targetDir are required")
	}
	if !databaseNamePattern.MatchString(req.Database) {
		return fmt.Errorf("database name %q is not a plain identifier", req.Database)
	}
	// The master's name becomes a command argument and an HA host name.
	if req.SeedFromMaster != "" && !hostLabelPattern.MatchString(req.SeedFromMaster) {
		return fmt.Errorf("seedFromMaster %q is not a host name (a DNS label)", req.SeedFromMaster)
	}
	for name, dir := range map[string]string{"targetDir": req.TargetDir, "stagingDir": req.StagingDir} {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == string(filepath.Separator) {
			return fmt.Errorf("%s %q must be a clean absolute path other than the root", name, dir)
		}
	}
	if within(req.StagingDir, req.TargetDir) || within(req.TargetDir, req.StagingDir) {
		return fmt.Errorf("stagingDir %q and targetDir %q must not contain each other", req.StagingDir, req.TargetDir)
	}
	return nil
}

// stagingNamePattern is one path element: no separator and no "..".
var stagingNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// confine maps the request's directories onto the manager's roots and returns
// paths built from those roots, so nothing after this point uses a path taken
// from the request: the target must be roots.Target itself, and the staging
// directory one plain name directly below roots.Staging.
func confine(req RestoreRequest, roots RestoreRoots) (targetDir, stagingDir string, err error) {
	for _, root := range []string{roots.Target, roots.Staging} {
		if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
			return "", "", fmt.Errorf("restore root %q is not configured as a clean absolute path", root)
		}
	}
	if filepath.Clean(req.TargetDir) != roots.Target {
		return "", "", fmt.Errorf("targetDir %q must be the database root %q", req.TargetDir, roots.Target)
	}
	staging := filepath.Clean(req.StagingDir)
	name := filepath.Base(staging)
	if filepath.Dir(staging) != roots.Staging || !stagingNamePattern.MatchString(name) {
		return "", "", fmt.Errorf("stagingDir %q must be one directory directly below %q", req.StagingDir, roots.Staging)
	}
	return roots.Target, filepath.Join(roots.Staging, name), nil
}

// within reports whether target is dir or lies below it.
func within(target, dir string) bool {
	rel, err := filepath.Rel(dir, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// registeredInDatabasesTxt reports whether targetDir's databases.txt already
// lists database. A missing file means nothing is registered.
func registeredInDatabasesTxt(targetDir, database string) (bool, error) {
	f, err := os.Open(filepath.Join(targetDir, databasesTxt)) // #nosec G304 -- targetDir is validated above
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", databasesTxt, err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Fields(line); fields[0] == database {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read %s: %w", databasesTxt, err)
	}
	return false, nil
}

// registerRestoreTarget creates <targetDir>/<database> and registers the
// database there in databases.txt, in the form `cubrid createdb` writes. The
// caller has checked that neither exists. The returned function undoes both:
// it removes the directory and restores databases.txt to what it was.
//
// Every file operation goes through an os.Root opened on targetDir, so
// nothing here can reach outside the manager's database root whatever the
// database name is.
func registerRestoreTarget(targetDir, database, host, owner string) (undo func(), err error) {
	if !databaseNamePattern.MatchString(database) {
		return nil, fmt.Errorf("database name %q is not a plain identifier", database)
	}
	if host == "" {
		host, err = os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("host name for %s: %w", databasesTxt, err)
		}
	}
	root, err := os.OpenRoot(targetDir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", targetDir, err)
	}
	defer func() { _ = root.Close() }()

	before, err := root.ReadFile(databasesTxt)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", databasesTxt, err)
	}
	if err := root.Mkdir(database, 0o750); err != nil {
		return nil, fmt.Errorf("create %s in %s: %w", database, targetDir, err)
	}
	if err := markOwned(root, database, owner); err != nil {
		_ = root.RemoveAll(database)
		return nil, err
	}
	// createdb makes the lob directory the entry names; restoredb does not,
	// and a LOB write fails without it (docs/poc/RESULTS.md, POC-12).
	if err := root.Mkdir(filepath.Join(database, "lob"), 0o750); err != nil {
		_ = root.RemoveAll(database)
		return nil, fmt.Errorf("create %s/lob in %s: %w", database, targetDir, err)
	}
	undo = func() {
		root, err := os.OpenRoot(targetDir)
		if err != nil {
			return
		}
		defer func() { _ = root.Close() }()
		_ = root.RemoveAll(database)
		if existed {
			_ = root.WriteFile(databasesTxt, before, 0o600)
		} else {
			_ = root.Remove(databasesTxt)
		}
	}

	dbDir := filepath.Join(targetDir, database)
	content := string(before)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += fmt.Sprintf("%s\t\t%s\t%s\t%s\tfile:%s\n", database, dbDir, host, dbDir, filepath.Join(dbDir, "lob"))
	if err := root.WriteFile(databasesTxt, []byte(content), 0o600); err != nil {
		undo()
		return nil, fmt.Errorf("register %s in %s: %w", database, databasesTxt, err)
	}
	return undo, nil
}

// hasExistingDB reports whether targetDir already contains the database's main
// volume, indicating a populated DB that must not be overwritten.
func hasExistingDB(targetDir, database string) bool {
	if _, err := os.Stat(filepath.Join(targetDir, database)); err == nil {
		return true
	}
	return false
}

// stageArtifact reads the artifact's manifest, checks it, and downloads each
// object it lists into stagingDir exactly once, verifying size and SHA-256 of
// the bytes as they are written. It returns only when every staged file
// matches the manifest; a file that does not is removed.
//
// The manifest comes from the bucket and is not trusted for paths: a key must
// be a clean path below backup/, and files are created through an os.Root on
// the staging directory, so neither a key nor a symbolic link inside the
// directory can place a file outside it.
func stageArtifact(ctx context.Context, store ObjectStore, bucket, prefix string, exp ManifestExpectation, stagingDir string) (BackupManifest, error) {
	data, err := readObject(ctx, store, bucket, path.Join(prefix, ManifestObjectName))
	if err != nil {
		return BackupManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		return BackupManifest{}, err
	}
	if err := manifest.Verify(exp); err != nil {
		return BackupManifest{}, err
	}
	// All keys are checked before anything is written.
	if err := manifest.ValidateObjectKeys(); err != nil {
		return BackupManifest{}, err
	}

	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		return BackupManifest{}, fmt.Errorf("create the staging directory: %w", err)
	}
	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("open the staging directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	for _, obj := range manifest.Objects {
		if err := stageObject(ctx, store, bucket, prefix, obj, root); err != nil {
			return BackupManifest{}, err
		}
	}
	return manifest, nil
}

// stageObject downloads one object into the staging root and checks what was
// written against the manifest entry.
func stageObject(ctx context.Context, store ObjectStore, bucket, prefix string, obj ManifestObject, root *os.Root) (err error) {
	name := filepath.FromSlash(obj.Key)
	if err := root.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		return fmt.Errorf("stage directory for %s: %w", obj.Key, err)
	}
	r, err := store.Get(ctx, bucket, path.Join(prefix, obj.Key))
	if err != nil {
		return fmt.Errorf("download %s: %w", obj.Key, err)
	}
	defer func() { _ = r.Close() }()
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create staged %s: %w", obj.Key, err)
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = root.Remove(name)
		}
	}()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, hash), r)
	if err != nil {
		return fmt.Errorf("write staged %s: %w", obj.Key, err)
	}
	if size != obj.SizeBytes {
		return fmt.Errorf("object %s size %d does not match manifest %d", obj.Key, size, obj.SizeBytes)
	}
	if sum := hex.EncodeToString(hash.Sum(nil)); sum != obj.SHA256 {
		return fmt.Errorf("object %s checksum mismatch", obj.Key)
	}
	return nil
}
