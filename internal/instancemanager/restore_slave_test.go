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

const haMemberHosts = "demo-0:demo-1:demo-2"

// A member seeded from a running master is restored with restoreslave: a
// plain restoredb leaves out what the master committed between the backup and
// the member's start (docs/poc/RESULTS.md, POC-14).
func TestRestore_SeedingFromAMasterUsesRestoreslave(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	req.SeedFromMaster = "demo-0"
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
	req.SeedFromMaster = "demo-0"
	cli := &restoreCLI{}
	_, err := Restore(context.Background(), cli, store, rootsFor(req), req)
	if err == nil || !strings.Contains(err.Error(), "not an HA member") {
		t.Fatalf("err = %v, want a refusal that names the reason", err)
	}
	if len(cli.calls) != 0 {
		t.Errorf("a command ran: %v", cli.calls)
	}
}
