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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The endpoints that take a request body.
const (
	pathBackup         = "/v1/backup"
	pathRestorePrepare = "/v1/restore/prepare"
	pathHABootstrap    = "/v1/ha/bootstrap"
)

// validationFixture is a manager with an operation store, a staging root and
// a databases directory, all below one temporary directory.
type validationFixture struct {
	t         *testing.T
	cli       *countingCLI
	storeDir  string
	staging   string
	databases string
	handler   http.Handler
}

func newValidationFixture(t *testing.T) *validationFixture {
	t.Helper()
	base := t.TempDir()
	f := &validationFixture{
		t: t, cli: &countingCLI{},
		storeDir: filepath.Join(base, "operations"), staging: filepath.Join(base, "staging"),
		databases: filepath.Join(base, "databases"),
	}
	store, err := NewOperationStore(f.storeDir)
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	t.Cleanup(func() { awaitTerminalOperations(t, store) })
	objects, _, _ := stageUploadedArtifact(t)
	f.handler = NewServer(f.cli, "tok").WithOperationStore(store).WithObjectStore(objects).
		WithBackupStagingRoot(f.staging).WithReplication(dbName, f.databases).Handler()
	return f
}

func (f *validationFixture) send(method, target, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Idempotency-Key", "key-1")
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, req)
	return rr
}

// wantNoWork fails when the request left anything behind: a command, an
// operation in the store, or a directory below the staging root.
func (f *validationFixture) wantNoWork(what string) {
	f.t.Helper()
	if ran := f.cli.recorded(); len(ran) != 0 {
		f.t.Errorf("%s: commands were run: %v", what, ran)
	}
	if entries, _ := os.ReadDir(f.storeDir); len(entries) != 0 {
		f.t.Errorf("%s: %d operation(s) were recorded", what, len(entries))
	}
	if entries, _ := os.ReadDir(f.staging); len(entries) != 0 {
		f.t.Errorf("%s: a staging directory was created", what)
	}
}

// A backup request is checked before anything is done for it: nothing it
// names reaches a command line, the operation store or the staging root
// unless the whole request is one this manager supports (#301).
func TestServer_Backup_RejectsWhatItDoesNotSupportBeforeAnyWork(t *testing.T) {
	upload := func(change func(*BackupUpload)) *BackupUpload {
		u := &BackupUpload{
			Bucket: "artifacts", Prefix: "prod/uid/appdb/backup-1", ClusterUID: "uid",
			CubridVersion: testUpload.CubridVersion, SourceInstance: "db-1", SourceRole: "slave",
		}
		change(u)
		return u
	}
	cases := []struct {
		name   string
		change func(*BackupRequest)
	}{
		{"no database", func(r *BackupRequest) { r.Database = "" }},
		{"a database name with a path", func(r *BackupRequest) { r.Database = "../appdb" }},
		{"a database name with an option", func(r *BackupRequest) { r.Database = "-S" }},
		{"a database name with a host", func(r *BackupRequest) { r.Database = "appdb@other" }},
		{"an incremental level", func(r *BackupRequest) { r.Level = 1 }},
		{"a negative level", func(r *BackupRequest) { r.Level = -1 }},
		{"an upload without a bucket", func(r *BackupRequest) { r.Upload = upload(func(u *BackupUpload) { u.Bucket = "" }) }},
		{"an upload without a prefix", func(r *BackupRequest) { r.Upload = upload(func(u *BackupUpload) { u.Prefix = "" }) }},
		{"an upload prefix that leaves its place", func(r *BackupRequest) {
			r.Upload = upload(func(u *BackupUpload) { u.Prefix = "prod/../other-cluster/appdb" })
		}},
		{"an absolute upload prefix", func(r *BackupRequest) { r.Upload = upload(func(u *BackupUpload) { u.Prefix = "/prod/appdb" }) }},
		{"an upload without the cluster", func(r *BackupRequest) { r.Upload = upload(func(u *BackupUpload) { u.ClusterUID = "" }) }},
		{"an upload without the engine version", func(r *BackupRequest) {
			r.Upload = upload(func(u *BackupUpload) { u.CubridVersion = "" })
		}},
		{"an upload from a source that is no host name", func(r *BackupRequest) {
			r.Upload = upload(func(u *BackupUpload) { u.SourceInstance = "db 1" })
		}},
		{"an upload from a source with an unknown role", func(r *BackupRequest) {
			r.Upload = upload(func(u *BackupUpload) { u.SourceRole = "primary" })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newValidationFixture(t)
			// Each case changes one field of a supported request, so the
			// answer is about that field and not about one checked earlier.
			req := BackupRequest{Database: dbName, Destination: filepath.Join(f.staging, "bk"), Upload: upload(func(*BackupUpload) {})}
			c.change(&req)
			body, _ := json.Marshal(req)
			rr := f.send(http.MethodPost, pathBackup, string(body))
			if rr.Code != http.StatusBadRequest {
				t.Errorf("/v1/backup = %d (%s), want 400", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			f.wantNoWork(c.name)
		})
	}

	t.Run("a supported request is accepted", func(t *testing.T) {
		f := newValidationFixture(t)
		req := BackupRequest{Database: dbName, Destination: filepath.Join(f.staging, "bk"), Upload: upload(func(*BackupUpload) {})}
		body, _ := json.Marshal(req)
		if rr := f.send(http.MethodPost, pathBackup, string(body)); rr.Code != http.StatusAccepted {
			t.Fatalf("/v1/backup = %d (%s), want 202", rr.Code, strings.TrimSpace(rr.Body.String()))
		}
	})

	// Every source role the operator sends is accepted. unknown is the only
	// member of a single-instance cluster under the PreferStandby fallback
	// (ADR-0007, RoleUnknownSingleInstanceFallback in internal/controller).
	for _, role := range []Role{RoleUnknown, RoleMaster, RoleSlave, RoleReplica, ""} {
		t.Run("a source with the role "+strconv.Quote(string(role))+" is accepted", func(t *testing.T) {
			f := newValidationFixture(t)
			req := BackupRequest{Database: dbName, Destination: filepath.Join(f.staging, "bk"),
				Upload: upload(func(u *BackupUpload) { u.SourceRole = string(role) })}
			body, _ := json.Marshal(req)
			if rr := f.send(http.MethodPost, pathBackup, string(body)); rr.Code != http.StatusAccepted {
				t.Fatalf("/v1/backup = %d (%s), want 202", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
		})
	}
}

// A restore request is checked before it is admitted and recorded: an
// invalid one leaves no operation behind, runs no command and creates no
// directory (#301).
func TestServer_Restore_RejectsAnInvalidRequestBeforeAnyWork(t *testing.T) {
	cases := []struct {
		name   string
		change func(*RestoreRequest)
	}{
		{"a database name that is an option", func(r *RestoreRequest) { r.Database = "-S" }},
		{"no prefix", func(r *RestoreRequest) { r.Prefix = "" }},
		{"a relative target directory", func(r *RestoreRequest) { r.TargetDir = "databases/appdb" }},
		{"a master that is no host name", func(r *RestoreRequest) { r.SeedFromMaster = "db 0" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newValidationFixture(t)
			req := RestoreRequest{Database: dbName, Bucket: "artifacts", Prefix: "prod/uid/appdb/backup-1",
				StagingDir: filepath.Join(f.staging, "restore"), TargetDir: filepath.Join(f.databases, dbName)}
			c.change(&req)
			body, _ := json.Marshal(req)
			rr := f.send(http.MethodPost, pathRestorePrepare, string(body))
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s = %d (%s), want 400", pathRestorePrepare, rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			f.wantNoWork(c.name)
		})
	}
}

// The requests of the operator are a few hundred bytes. A body beyond the
// limit is refused without being read to its end, and nothing is done for it.
func TestServer_RefusesAnOversizedBodyBeforeAnyWork(t *testing.T) {
	padding := strings.Repeat("x", maxRequestBody)
	for _, target := range []string{pathBackup, pathRestorePrepare, pathHABootstrap} {
		t.Run(target, func(t *testing.T) {
			f := newValidationFixture(t)
			body := `{"database":"` + dbName + `","destination":"` + filepath.Join(f.staging, "bk") + `","padding":"` + padding + `"}`
			rr := f.send(http.MethodPost, target, body)
			if rr.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("%s with a body of %d bytes = %d, want 413", target, len(body), rr.Code)
			}
			f.wantNoWork(target)
		})
	}
}

// /v1/ha/convergence runs applyinfo on a path. The path is one of this
// member's copied logs of the named database, below the databases directory
// the manager was started with, and nothing else.
func TestServer_Convergence_ReadsOnlyThisMembersCopiedLogs(t *testing.T) {
	f := newValidationFixture(t)
	get := func(database, path string) *httptest.ResponseRecorder {
		return f.send(http.MethodGet, "/v1/ha/convergence?database="+database+"&copiedLogPath="+path, "")
	}
	refused := []struct{ name, database, path string }{
		{"a path outside the databases directory", dbName, "/etc/" + dbName + "_db-0"},
		{"the databases directory itself", dbName, f.databases},
		{"a path that leaves the databases directory", dbName, f.databases + "/../" + dbName + "_db-0"},
		{"a directory deeper than a copied log", dbName, f.databases + "/" + dbName + "_db-0/x"},
		{"another database's copied log", dbName, f.databases + "/otherdb_db-0"},
		{"the database's own directory", dbName, f.databases + "/" + dbName},
		{"a copied log of something that is no host", dbName, f.databases + "/" + dbName + "_db..0"},
		{"a relative path", dbName, dbName + "_db-0"},
		{"a database name with an option", "-S", f.databases + "/-S_db-0"},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			if rr := get(c.database, c.path); rr.Code != http.StatusBadRequest {
				t.Errorf("convergence of %s at %s = %d, want 400", c.database, c.path, rr.Code)
			}
			f.wantNoWork(c.name)
		})
	}

	want := filepath.Join(f.databases, dbName+"_db-0")
	rr := get(dbName, want)
	if rr.Code != http.StatusOK {
		t.Fatalf("convergence of a copied log = %d (%s), want 200", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if ran := f.cli.recorded(); len(ran) != 1 || ran[0] != "cubrid applyinfo -L "+want+" -a "+dbName {
		t.Errorf("commands = %v, want one applyinfo on %s", ran, want)
	}
}
