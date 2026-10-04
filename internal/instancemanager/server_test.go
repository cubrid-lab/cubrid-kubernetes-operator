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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCLI returns canned output, standing in for a live CUBRID (ADR-0003 seam).
type fakeCLI struct {
	out string
	err error
}

func (f fakeCLI) Run(_ context.Context, _ string, _ ...string) (string, error) {
	return f.out, f.err
}

func doReq(t *testing.T, h http.Handler, path, token, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestServer_Livez(t *testing.T) {
	h := NewServer(fakeCLI{out: masterOut}, "tok").Handler()
	rr := doReq(t, h, "/livez", "", testRemoteAddr)
	if rr.Code != http.StatusOK {
		t.Errorf("/livez = %d, want 200", rr.Code)
	}
}

func TestServer_Readyz_MasterReady(t *testing.T) {
	h := NewServer(fakeCLI{out: masterOut}, "tok").Handler()
	rr := doReq(t, h, "/readyz", "", testRemoteAddr)
	if rr.Code != http.StatusOK {
		t.Errorf("/readyz(master) = %d, want 200", rr.Code)
	}
}

func TestServer_Readyz_UnknownNotReady(t *testing.T) {
	h := NewServer(fakeCLI{out: transitionOut}, "tok").Handler()
	rr := doReq(t, h, "/readyz", "", testRemoteAddr)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz(transition) = %d, want 503", rr.Code)
	}
}

func TestServer_Role_RequiresToken(t *testing.T) {
	h := NewServer(fakeCLI{out: masterOut}, "tok").Handler()

	// remote without token -> 401
	if rr := doReq(t, h, "/v1/role", "", testRemoteAddr); rr.Code != http.StatusUnauthorized {
		t.Errorf("/v1/role no-token = %d, want 401", rr.Code)
	}
	// remote with a wrong token -> 401: a present but different token is not accepted
	if rr := doReq(t, h, "/v1/role", "not-the-token", testRemoteAddr); rr.Code != http.StatusUnauthorized {
		t.Errorf("/v1/role wrong-token = %d, want 401", rr.Code)
	}
	// remote with token -> 200, parsed master
	rr := doReq(t, h, "/v1/role", "tok", testRemoteAddr)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/role with-token = %d, want 200", rr.Code)
	}
	var st HAStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Role != RoleMaster {
		t.Errorf("role = %q, want master", st.Role)
	}
}

func TestServer_Role_LoopbackExempt(t *testing.T) {
	h := NewServer(fakeCLI{out: masterOut}, "tok").Handler()
	// loopback without token -> allowed (preStop path, ADR-0003)
	if rr := doReq(t, h, "/v1/role", "", "127.0.0.1:6000"); rr.Code != http.StatusOK {
		t.Errorf("/v1/role loopback no-token = %d, want 200", rr.Code)
	}
}

func TestServer_Backup(t *testing.T) {
	h := NewServer(fakeCLI{out: "Backup Volume Label: Level: 0"}, "tok").WithBackupStagingRoot("/im-test-staging").Handler()

	req := httptest.NewRequest("POST", "/v1/backup", strings.NewReader(`{"database":"appdb","destination":"/im-test-staging/bk"}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/backup = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var res BackupResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Database != dbName {
		t.Errorf("backup result = %+v", res)
	}
}

func TestServer_Backup_RequiresToken(t *testing.T) {
	h := NewServer(fakeCLI{}, "tok").Handler()
	req := httptest.NewRequest("POST", "/v1/backup", strings.NewReader(`{}`))
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("/v1/backup no-token = %d, want 401", rr.Code)
	}
}

func TestServer_Shutdown_LoopbackExempt(t *testing.T) {
	h := NewServer(fakeCLI{out: "ok"}, "tok").Handler()
	req := httptest.NewRequest("POST", "/v1/shutdown?database=appdb", nil)
	req.RemoteAddr = "127.0.0.1:6000"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("/v1/shutdown loopback = %d, want 200", rr.Code)
	}
}

// Recorded on CUBRID 11.4.6 from a standalone server (docs/poc/RESULTS.md, POC-10).
const (
	serverStatusRunning = "@ cubrid server status\n Server appdb (rel 11.4.6, pid 14)\n"
	serverStatusStopped = "@ cubrid server status\n"
)

func TestServerRunning_ParsesRecordedStatus(t *testing.T) {
	tests := []struct {
		name     string
		out      string
		database string
		want     bool
	}{
		{"running", serverStatusRunning, dbName, true},
		{"stopped", serverStatusStopped, dbName, false},
		{"another database is running", serverStatusRunning, "otherdb", false},
		{"a longer name is not a match", serverStatusRunning, "app", false},
		{"empty output", "", dbName, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ServerRunning(context.Background(), fakeCLI{out: tc.out}, tc.database)
			if err != nil {
				t.Fatalf("ServerRunning: %v", err)
			}
			if got != tc.want {
				t.Errorf("ServerRunning = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestServerRunning_CommandFailureIsAnError(t *testing.T) {
	if _, err := ServerRunning(context.Background(), fakeCLI{err: errors.New("exit status 1")}, dbName); err == nil {
		t.Fatal("a failed status command must not be read as stopped or running")
	}
}

// A standalone server has no HA role; it is ready when its server runs (#147).
func TestServer_Readyz_Standalone(t *testing.T) {
	tests := []struct {
		name string
		cli  fakeCLI
		want int
	}{
		{"server running", fakeCLI{out: serverStatusRunning}, http.StatusOK},
		{"server stopped", fakeCLI{out: serverStatusStopped}, http.StatusServiceUnavailable},
		{"status command fails", fakeCLI{err: errors.New("exit status 1")}, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewServer(tc.cli, "tok").WithStandaloneDatabase(dbName).Handler()
			if rr := doReq(t, h, "/readyz", "", testRemoteAddr); rr.Code != tc.want {
				t.Errorf("/readyz = %d, want %d (body %s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// An HA member with no role stays not ready, whatever the server status says.
func TestServer_Readyz_HAMemberStillNeedsARole(t *testing.T) {
	h := NewServer(fakeCLI{out: serverStatusRunning}, "tok").Handler()
	if rr := doReq(t, h, "/readyz", "", testRemoteAddr); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 for a member without an HA role", rr.Code)
	}
}
