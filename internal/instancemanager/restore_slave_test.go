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
	"path/filepath"
	"strings"
	"testing"
)

const (
	haMemberHosts = "demo-0:demo-1:demo-2"
	seedMaster    = "demo-0"
)

// A member seeded from a running master is restored with restoreslave: a
// plain restoredb leaves out what the master committed between the backup and
// the member's start (docs/poc/RESULTS.md, POC-14).
func TestRestore_SeedingFromAMasterUsesRestoreslave(t *testing.T) {
	store, bucket, prefix := stageSeedArtifact(t, nil)
	req := baseRestoreRequest(t, bucket, prefix)
	req.SeedFromMaster = seedMaster
	roots := rootsFor(req)
	roots.Host = haMemberHosts
	cli := &restoreCLI{}

	if _, err := Restore(context.Background(), cli, store, roots, req); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	want := "cubrid restoreslave -u -s master -m demo-0 -B " + filepath.Join(req.StagingDir, "backup") + " " + dbName
	if len(cli.calls) != 1 || cli.calls[0] != want {
		t.Errorf("calls = %v\nwant    [%s]", cli.calls, want)
	}
}

// Without a master to follow, the restore into a new cluster, restoredb stays.
func TestRestore_WithoutAMasterUsesRestoredb(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	roots := rootsFor(req)
	roots.Host = haMemberHosts
	cli := &restoreCLI{}
	if _, err := Restore(context.Background(), cli, store, roots, req); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(cli.calls) != 1 || !strings.HasPrefix(cli.calls[0], "cubrid restoredb -u -B ") {
		t.Errorf("calls = %v, want one restoredb", cli.calls)
	}
}

// The master's name becomes a command argument, so only a DNS label is taken.
func TestRestore_RejectsAMasterNameThatIsNotAHostName(t *testing.T) {
	for _, name := range []string{"demo_0", "Demo-0", "demo-0.ns.svc", "-m", "demo-0 --list", "../x", "a;b", strings.Repeat("a", 64)} {
		t.Run(name, func(t *testing.T) {
			store, bucket, prefix := stageUploadedArtifact(t)
			req := baseRestoreRequest(t, bucket, prefix)
			req.SeedFromMaster = name
			roots := rootsFor(req)
			roots.Host = haMemberHosts
			cli := &restoreCLI{}
			if _, err := Restore(context.Background(), cli, store, roots, req); err == nil {
				t.Fatal("the request was accepted")
			}
			if len(cli.calls) != 0 {
				t.Errorf("a command ran: %v", cli.calls)
			}
			if got := databasesTxtEntry(t, req.TargetDir, dbName); got != nil {
				t.Errorf("the database was registered: %q", got)
			}
		})
	}
}

// Only an HA member can follow a master.
func TestRestore_RejectsSeedingAMemberThatIsNotInHA(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	req.SeedFromMaster = seedMaster
	cli := &restoreCLI{}
	_, err := Restore(context.Background(), cli, store, rootsFor(req), req)
	if err == nil || !strings.Contains(err.Error(), "not an HA member") {
		t.Fatalf("err = %v, want a refusal that names the reason", err)
	}
	if len(cli.calls) != 0 {
		t.Errorf("a command ran: %v", cli.calls)
	}
}

const seedClusterUID = "cluster-uid"

// stageSeedArtifact uploads an artifact whose manifest says what a seed
// backup on demo-0 says, then lets mutate change the manifest.
func stageSeedArtifact(t *testing.T, mutate func(*BackupManifest)) (*fakeObjectStore, string, string) {
	t.Helper()
	store, bucket, prefix := stageUploadedArtifact(t)
	key := bucket + "/" + prefix + "/" + ManifestObjectName
	manifest, err := ParseManifest(store.objects[key])
	if err != nil {
		t.Fatal(err)
	}
	manifest.ClusterUID = seedClusterUID
	manifest.Level = 0
	manifest.SourceInstance = seedMaster
	manifest.SourceRole = string(RoleMaster)
	if mutate != nil {
		mutate(&manifest)
	}
	if store.objects[key], err = manifest.Marshal(); err != nil {
		t.Fatal(err)
	}
	return store, bucket, prefix
}

// A seed restore follows the master it names from the position of that
// master's backup, so the artifact must be a full backup taken on that
// master of this cluster. Anything else is refused before any download is
// trusted, the database is registered or restoreslave runs (#276).
func TestRestore_SeedRefusesAnArtifactOfAnotherSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*BackupManifest)
		says   string
	}{
		{"an incremental backup", func(m *BackupManifest) { m.Level = 1 }, "level"},
		{"a backup taken on another member", func(m *BackupManifest) { m.SourceInstance = "demo-1" }, "sourceInstance"},
		{"a backup taken on a slave", func(m *BackupManifest) { m.SourceRole = string(RoleSlave) }, "sourceRole"},
		{"a backup of another cluster", func(m *BackupManifest) { m.ClusterUID = "another-cluster" }, "clusterUID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, bucket, prefix := stageSeedArtifact(t, tc.mutate)
			req := baseRestoreRequest(t, bucket, prefix)
			req.SeedFromMaster = seedMaster
			req.ExpectedClusterUID = seedClusterUID
			roots := rootsFor(req)
			roots.Host = haMemberHosts
			cli := &restoreCLI{}
			_, err := Restore(context.Background(), cli, store, roots, req)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want a refusal that names %s", err, tc.says)
			}
			if len(cli.calls) != 0 {
				t.Errorf("a command ran: %v", cli.calls)
			}
			if got := databasesTxtEntry(t, req.TargetDir, dbName); got != nil {
				t.Errorf("the database was registered: %q", got)
			}
		})
	}
}

// A full backup taken on the named master of this cluster seeds the member.
// A request of an operator that does not send the cluster's UID still has
// its source checked.
func TestRestore_SeedAcceptsTheNamedMastersFullBackup(t *testing.T) {
	for _, uid := range []string{seedClusterUID, ""} {
		store, bucket, prefix := stageSeedArtifact(t, nil)
		req := baseRestoreRequest(t, bucket, prefix)
		req.SeedFromMaster = seedMaster
		req.ExpectedClusterUID = uid
		roots := rootsFor(req)
		roots.Host = haMemberHosts
		cli := &restoreCLI{}
		if _, err := Restore(context.Background(), cli, store, roots, req); err != nil {
			t.Fatalf("expected cluster %q: Restore: %v", uid, err)
		}
		if len(cli.calls) != 1 || !strings.HasPrefix(cli.calls[0], "cubrid restoreslave ") {
			t.Errorf("expected cluster %q: calls = %v, want one restoreslave", uid, cli.calls)
		}
	}
}

// A restore into a new cluster takes a full backup of another cluster,
// taken on a standby; a backup that is not full is refused, since restoredb
// would need the backups it builds on.
func TestRestore_RecoveryTakesAFullBackupOfAnotherClustersStandby(t *testing.T) {
	for _, tc := range []struct {
		level  int
		accept bool
	}{{0, true}, {1, false}, {2, false}} {
		store, bucket, prefix := stageSeedArtifact(t, func(m *BackupManifest) {
			m.ClusterUID = "the-lost-cluster"
			m.SourceInstance = "old-1"
			m.SourceRole = string(RoleSlave)
			m.Level = tc.level
		})
		req := baseRestoreRequest(t, bucket, prefix)
		cli := &restoreCLI{}
		_, err := Restore(context.Background(), cli, store, rootsFor(req), req)
		switch {
		case tc.accept && (err != nil || len(cli.calls) != 1):
			t.Errorf("level %d: err = %v, calls = %v; want one restoredb", tc.level, err, cli.calls)
		case !tc.accept && (err == nil || len(cli.calls) != 0 || !strings.Contains(err.Error(), "level")):
			t.Errorf("level %d: err = %v, calls = %v; want a refusal before any command", tc.level, err, cli.calls)
		}
	}
}
