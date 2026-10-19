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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sameBody is the hash of one request body.
const sameBody = "same-body"

// spoilRecord makes the record of operation id unreadable in one of the ways
// a record on a volume can become so, and returns the way back.
type spoilRecord func(t *testing.T, path string) (repair func())

// overwrite spoils a record by replacing its contents with content.
func overwrite(content string) spoilRecord {
	return func(t *testing.T, path string) func() {
		t.Helper()
		good, err := os.ReadFile(path) // #nosec G304 -- a file of the test
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.WriteFile(path, good, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

var spoiledRecords = map[string]spoilRecord{
	// A record cut short, as a torn write or a damaged block leaves it.
	"not decodable": overwrite("{"),
	// A record that decodes but is no operation's: admission could not
	// match its key or kind.
	"decodes to no operation": overwrite("{}"),
	// A record the manager may not read, as a wrong owner or mode leaves it.
	"permission denied": func(t *testing.T, path string) func() {
		t.Helper()
		if os.Geteuid() == 0 {
			t.Skip("a file mode does not keep root from reading")
		}
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	},
}

// recordFiles counts the operation records in dir.
func recordFiles(t *testing.T, dir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "op-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

// A record that cannot be read may be the one under the request's key: its
// key, kind and database are inside it. The store therefore admits no new
// operation, of any key, kind or database, while one of its records cannot
// be read, and says why with its own error. Once the record can be read
// again, the same key names the operation it holds (#427).
func TestOperationStore_FindOrCreate_RefusesWhileARecordCannotBeRead(t *testing.T) {
	for name, spoil := range spoiledRecords {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			first, _, err := store.FindOrCreate(OpRestore, "restore-key", sameBody, dbName)
			if err != nil {
				t.Fatal(err)
			}
			// It has ended, so no other request is refused as in progress
			// beside it: only the record that cannot be read refuses them.
			if _, err := store.Update(first.ID, func(o *Operation) { o.State = OpCompleted }); err != nil {
				t.Fatal(err)
			}
			repair := spoil(t, filepath.Join(dir, first.ID+".json"))
			t.Cleanup(repair)

			requests := []struct {
				what           string
				kind           OperationKind
				key, database  string
				hashOfThisBody string
			}{
				{"the same request again", OpRestore, "restore-key", dbName, sameBody},
				{"another key", OpRestore, "other-key", dbName, sameBody},
				{"another kind", OpBackup, "backup-key", dbName, sameBody},
				{"another database", OpBackup, "backup-key", "otherdb", sameBody},
			}
			for _, r := range requests {
				second, existed, err := store.FindOrCreate(r.kind, r.key, r.hashOfThisBody, r.database)
				if !errors.Is(err, ErrOperationRecordUnreadable) {
					t.Errorf("%s: FindOrCreate = %+v (existed %v), %v; want ErrOperationRecordUnreadable",
						r.what, second, existed, err)
					continue
				}
				if !strings.Contains(err.Error(), first.ID) {
					t.Errorf("%s: error %q does not name the record that cannot be read", r.what, err)
				}
			}
			if n := recordFiles(t, dir); n != 1 {
				t.Errorf("records = %d, want only the first: another operation was admitted", n)
			}

			repair()
			again, existed, err := store.FindOrCreate(OpRestore, "restore-key", sameBody, dbName)
			if err != nil || !existed || again.ID != first.ID {
				t.Errorf("after the record is readable again = %+v (existed %v), %v; want operation %s",
					again, existed, err, first.ID)
			}
		})
	}
}

// A start under the key of a record that cannot be read is refused with 500
// and the code OperationRecordUnreadable, and runs no command: no second
// backupdb, restore or HA bootstrap writer (#427, ADR-0003).
func TestServer_StartIsRefusedWhileARecordCannotBeRead(t *testing.T) {
	starts := []struct {
		what string
		kind OperationKind
		path string
		body func(t *testing.T) string
	}{
		{"backup", OpBackup, pathBackup, completeBackupBody},
		{"restore", OpRestore, pathRestorePrepare, restoreBody},
		{"HA bootstrap", OpHABootstrap, pathHABootstrap, func(*testing.T) string { return `{"database":"` + dbName + `"}` }},
	}
	for name, spoil := range spoiledRecords {
		for _, s := range starts {
			t.Run(name+"/"+s.what, func(t *testing.T) {
				cli := &recordingCLI{}
				h, store := newLookupServer(t, cli)
				body := s.body(t)
				held, _, err := store.FindOrCreate(s.kind, "k", HashRequest([]byte(body)), dbName)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = store.Update(held.ID, func(o *Operation) { o.State = OpFailed }) })
				t.Cleanup(spoil(t, filepath.Join(store.dir, held.ID+".json")))

				rr := sendOperation(h, s.path, "k", "tok", body)
				if rr.Code != http.StatusInternalServerError {
					t.Fatalf("POST %s = %d %s, want 500", s.path, rr.Code, rr.Body.String())
				}
				var answer ErrorResponse
				if err := json.Unmarshal(rr.Body.Bytes(), &answer); err != nil {
					t.Fatalf("decode %s: %v", rr.Body.String(), err)
				}
				if answer.Code != CodeOperationRecordUnreadable {
					t.Errorf("code = %q, want %q", answer.Code, CodeOperationRecordUnreadable)
				}
				if len(cli.calls) != 0 {
					t.Errorf("commands ran although the start was refused: %q", cli.calls)
				}
				if n := recordFiles(t, store.dir); n != 1 {
					t.Errorf("records = %d, want only the held one: another operation was admitted", n)
				}
			})
		}
	}
}

// A manager that starts on a store with a record it cannot read starts: it
// leaves that record as it is, cannot fail it as an interrupted operation,
// and therefore admits nothing beside it. The record is logged once, with
// its own event, and not again at each request that is refused for it.
func TestOperationStore_StartsOnARecordItCannotRead(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := store.FindOrCreate(OpBackup, "k", "h", dbName)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, running.ID+".json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewOperationStore(dir)
	if err != nil {
		t.Fatalf("NewOperationStore with a record it cannot read: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "{" { // #nosec G304 -- a file of the test
		t.Errorf("the record was changed at start: %q, %v", data, err)
	}
	var logged bytes.Buffer
	reopened.SetLogger(slog.New(slog.NewJSONHandler(&logged, nil)))
	for range 3 {
		if _, _, err := reopened.FindOrCreate(OpBackup, "k", "h", dbName); !errors.Is(err, ErrOperationRecordUnreadable) {
			t.Fatalf("FindOrCreate after the restart = %v, want ErrOperationRecordUnreadable", err)
		}
	}
	if n := strings.Count(logged.String(), `"event":"`+eventRecordUnreadable+`"`); n != 1 {
		t.Errorf("records logged as unreadable = %d, want 1: %s", n, logged.String())
	}
	if !strings.Contains(logged.String(), running.ID) {
		t.Errorf("the log does not name the record: %s", logged.String())
	}
	if n := recordFiles(t, dir); n != 1 {
		t.Errorf("records = %d, want only the first", n)
	}
}

// A record that could not be read when the manager started, and is repaired
// later, is not left running: nothing runs it in this process. Its first
// successful read fails it as the start would have, so a poll answers
// Failed, another request is not refused as in progress, and a repeat of
// its key names the Failed operation (#427).
func TestOperationStore_RepairedRecordOfAnInterruptedOperationIsFailed(t *testing.T) {
	for name, spoil := range spoiledRecords {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			running, _, err := store.FindOrCreate(OpBackup, "k", "h", dbName)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(running.ID, func(o *Operation) { o.State = OpRunningBackup }); err != nil {
				t.Fatal(err)
			}
			repair := spoil(t, filepath.Join(dir, running.ID+".json"))
			t.Cleanup(repair)

			reopened, err := NewOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			repair()

			got, err := reopened.Get(running.ID)
			if err != nil || got.State != OpFailed || got.FailureReason != reasonRestarted {
				t.Fatalf("Get after the repair = %+v, %v; want Failed with %q", got, err, reasonRestarted)
			}
			if again, existed, err := reopened.FindOrCreate(OpBackup, "k", "h", dbName); err != nil || !existed || again.State != OpFailed {
				t.Errorf("repeat of the key = %+v (existed %v), %v; want the Failed operation", again, existed, err)
			}
			if _, _, err := reopened.FindOrCreate(OpBackup, "other-key", "h", dbName); err != nil {
				t.Errorf("another request after the repair = %v; want it admitted", err)
			}
		})
	}
}

// A backup whose staging destination holds an entry of a record that cannot
// be read is refused with OperationRecordUnreadable, not as a destination in
// use: the record may be this request's operation (#427).
func TestServer_Backup_StagingOwnerThatCannotBeReadIsUnreadable(t *testing.T) {
	cli := &recordingCLI{}
	h, store := newLookupServer(t, cli)
	body := completeBackupBody(t)
	held, _, err := store.FindOrCreate(OpBackup, "k", HashRequest([]byte(body)), dbName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Update(held.ID, func(o *Operation) { o.State = OpFailed }) })
	if err := os.MkdirAll(filepath.Join(testStagingRoot, "bk", held.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(overwrite("{")(t, filepath.Join(store.dir, held.ID+".json")))

	rr := sendOperation(h, pathBackup, "k", "tok", body)
	var answer ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	if rr.Code != http.StatusInternalServerError || answer.Code != CodeOperationRecordUnreadable {
		t.Errorf("POST %s = %d %s, want 500 %s", pathBackup, rr.Code, rr.Body.String(), CodeOperationRecordUnreadable)
	}
	if len(cli.calls) != 0 {
		t.Errorf("commands ran although the start was refused: %q", cli.calls)
	}
}
