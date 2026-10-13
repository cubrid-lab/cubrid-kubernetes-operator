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
	"strings"
	"testing"
)

// sendOperation posts body to path under the Idempotency-Key, with the token
// when one is given.
func sendOperation(h http.Handler, path, key, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Idempotency-Key", key)
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// holdOperation records a Pending operation of kind under key, as a request
// of another body would have, and ends it when the test ends so that no
// operation is left running.
func holdOperation(t *testing.T, store *OperationStore, kind OperationKind, key string) {
	t.Helper()
	op, _, err := store.FindOrCreate(kind, key, "hash-of-another-request", dbName)
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.Update(op.ID, func(o *Operation) { o.State = OpFailed })
	})
}

// completeBackupBody is a backup request with every field a manager checks
// before it records an operation, so that these answers do not depend on
// what is validated before them.
func completeBackupBody(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(BackupRequest{
		Database: dbName, Destination: testStagingRoot + "/bk",
		Upload: &BackupUpload{Bucket: testUpload.Bucket, Prefix: testUpload.Prefix, ClusterUID: "uid-1",
			CubridVersion: "11.4", SourceInstance: "db-0", SourceRole: "slave"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func restoreBody(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(RestoreRequest{
		Database: dbName, Bucket: testUpload.Bucket, Prefix: testUpload.Prefix, ExpectedCubridVersion: "11.4",
		StagingDir: "/var/lib/cubrid/restore-staging/uid-1", TargetDir: "/var/lib/cubrid/databases",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// An error answer names its cause in "code" and keeps the "error" string an
// earlier manager sent, unchanged, so that an operator that reads only
// "error" and the status sees what it saw before (#302, #322). No other
// field is added.
func TestErrorAnswers_CarryACodeAndKeepTheErrorString(t *testing.T) {
	type answer struct {
		rr     *httptest.ResponseRecorder
		status int
		code   string
		error  string
	}
	var cases = map[string]func(t *testing.T) answer{
		"backup under a key held for another body": func(t *testing.T) answer {
			h, store := newLookupServer(t, &recordingCLI{})
			holdOperation(t, store, OpBackup, "k")
			return answer{sendOperation(h, "/v1/backup", "k", "tok", completeBackupBody(t)),
				http.StatusConflict, CodeIdempotencyConflict, ErrIdempotencyConflict.Error()}
		},
		"backup while another operation holds the database": func(t *testing.T) answer {
			h, store := newLookupServer(t, &recordingCLI{})
			holdOperation(t, store, OpRestore, "held")
			return answer{sendOperation(h, "/v1/backup", "k", "tok", completeBackupBody(t)),
				http.StatusConflict, CodeOperationInProgress, ErrOperationInProgress.Error()}
		},
		"restore under a key held for another body": func(t *testing.T) answer {
			h, store := newLookupServer(t, &recordingCLI{})
			holdOperation(t, store, OpRestore, "k")
			return answer{sendOperation(h, "/v1/restore/prepare", "k", "tok", restoreBody(t)),
				http.StatusConflict, CodeIdempotencyConflict, ErrIdempotencyConflict.Error()}
		},
		"restore while another operation holds the database": func(t *testing.T) answer {
			h, store := newLookupServer(t, &recordingCLI{})
			holdOperation(t, store, OpBackup, "held")
			return answer{sendOperation(h, "/v1/restore/prepare", "k", "tok", restoreBody(t)),
				http.StatusConflict, CodeOperationInProgress, ErrOperationInProgress.Error()}
		},
		"HA bootstrap under a key held for another body": func(t *testing.T) answer {
			h, store := newLookupServer(t, &recordingCLI{})
			holdOperation(t, store, OpHABootstrap, "k")
			return answer{sendOperation(h, "/v1/ha/bootstrap", "k", "tok", `{"database":"`+dbName+`"}`),
				http.StatusConflict, CodeIdempotencyConflict, ErrIdempotencyConflict.Error()}
		},
		"HA bootstrap while another operation holds the database": func(t *testing.T) answer {
			h, store := newLookupServer(t, &recordingCLI{})
			holdOperation(t, store, OpRestore, "held")
			return answer{sendOperation(h, "/v1/ha/bootstrap", "k", "tok", `{"database":"`+dbName+`"}`),
				http.StatusConflict, CodeOperationInProgress, ErrOperationInProgress.Error()}
		},
		"HA bootstrap on a standalone member": func(t *testing.T) answer {
			store, err := NewOperationStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			h := NewServer(&recordingCLI{}, "tok").WithOperationStore(store).WithStandaloneDatabase(dbName).Handler()
			return answer{sendOperation(h, "/v1/ha/bootstrap", "k", "tok", `{"database":"`+dbName+`"}`),
				http.StatusConflict, CodeNotAnHAMember, "this member is a standalone server, not an HA member"}
		},
		"backup on a manager without object storage": func(t *testing.T) answer {
			store, err := NewOperationStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			h := NewServer(&recordingCLI{}, "tok").WithOperationStore(store).WithBackupStagingRoot(testStagingRoot).Handler()
			return answer{sendOperation(h, "/v1/backup", "k", "tok", completeBackupBody(t)),
				http.StatusPreconditionFailed, CodeObjectStorageNotApplied,
				"this Instance Manager was started without object storage (IM_S3_ENDPOINT is not set); " +
					"the Pod has to be restarted with the cluster's spec.objectStorage"}
		},
		"a request with the wrong token": func(t *testing.T) answer {
			h, _ := newLookupServer(t, &recordingCLI{})
			return answer{sendOperation(h, "/v1/backup", "k", "not-the-token", completeBackupBody(t)),
				http.StatusUnauthorized, CodeUnauthorized, "unauthorized"}
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			a := run(t)
			if a.rr.Code != a.status {
				t.Fatalf("status = %d (body %s), want %d", a.rr.Code, a.rr.Body.String(), a.status)
			}
			var fields map[string]any
			if err := json.Unmarshal(a.rr.Body.Bytes(), &fields); err != nil {
				t.Fatalf("decode %s: %v", a.rr.Body.String(), err)
			}
			if fields[errKey] != a.error {
				t.Errorf("error = %q, want the string an earlier manager sent, %q", fields[errKey], a.error)
			}
			if fields["code"] != a.code {
				t.Errorf("code = %v, want %q", fields["code"], a.code)
			}
			if len(fields) != 2 {
				t.Errorf("fields = %v, want error and code only", fields)
			}
		})
	}
}

// An error answer that names no cause keeps the body an earlier manager
// sent: "error" alone, without an empty code.
func TestErrorAnswers_WithoutACodeKeepTheEarlierBody(t *testing.T) {
	h, _ := newLookupServer(t, &recordingCLI{})
	rr := sendOperation(h, "/v1/backup", "", "tok", completeBackupBody(t))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"error":"`+msgNoIdempotencyKey+`"}` {
		t.Errorf("body = %s, want the error alone", got)
	}
}
