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
	"strings"
	"testing"
)

// recordingCLI records the exact commands issued so tests can assert the CUBRID
// invocation and ordering (POC-4 backup / ADR-0003 shutdown).
type recordingCLI struct {
	calls []string
	err   error
}

func (r *recordingCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return "ok", r.err
}

const dbName = "appdb"

func TestBackup_CSMode(t *testing.T) {
	rc := &recordingCLI{}
	res, err := Backup(context.Background(), rc, BackupRequest{Database: dbName, Destination: "/tmp/bk", Level: 0})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(rc.calls) != 1 {
		t.Fatalf("calls = %v", rc.calls)
	}
	got := rc.calls[0]
	// POC-4: CS mode (-C) against the running server, destination via -D, db@localhost.
	for _, want := range []string{"cubrid backupdb", "-D /tmp/bk", "-C", "appdb@localhost"} {
		if !strings.Contains(got, want) {
			t.Errorf("backup cmd %q missing %q", got, want)
		}
	}
	if res.Database != dbName || res.Destination != "/tmp/bk" {
		t.Errorf("result = %+v", res)
	}
}

func TestBackup_RequiresArgs(t *testing.T) {
	if _, err := Backup(context.Background(), &recordingCLI{}, BackupRequest{Database: dbName}); err == nil {
		t.Error("expected error when destination is empty")
	}
}

func TestShutdown_OrderHAThenServer(t *testing.T) {
	rc := &recordingCLI{}
	if err := Shutdown(context.Background(), rc, dbName); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if len(rc.calls) != 2 {
		t.Fatalf("calls = %v, want 2", rc.calls)
	}
	// ADR-0003: withdraw HA first, then stop the server.
	if !strings.Contains(rc.calls[0], "heartbeat stop") {
		t.Errorf("first call = %q, want heartbeat stop", rc.calls[0])
	}
	if !strings.Contains(rc.calls[1], "server stop appdb") {
		t.Errorf("second call = %q, want server stop appdb", rc.calls[1])
	}
}
