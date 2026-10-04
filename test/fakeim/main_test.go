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

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

const (
	testToken = "tok"
	// remote is a non-loopback caller, so the token is required.
	remote = "10.0.0.9:40000"

	member0  = "demo-0"
	database = "appdb"
)

func get(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func role(t *testing.T, h http.Handler) instancemanager.HAStatus {
	t.Helper()
	rr := get(t, h, "/v1/role", testToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/role = %d: %s", rr.Code, rr.Body.String())
	}
	var st instancemanager.HAStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestInitialRole(t *testing.T) {
	for host, want := range map[string]instancemanager.Role{
		member0: instancemanager.RoleMaster, "demo-1": instancemanager.RoleSlave, "demo-10": instancemanager.RoleSlave,
	} {
		if got := initialRole(host); got != want {
			t.Errorf("initialRole(%q) = %s, want %s", host, got, want)
		}
	}
}

// The scripted output has to pass through the real parser and the real
// handlers, or the wiring test would exercise nothing.
func TestFake_ServesTheRealAPIForEachRole(t *testing.T) {
	cli := &fakeCLI{host: member0, database: database, role: instancemanager.RoleMaster}
	h := newHandler(cli, testToken)

	st := role(t, h)
	if st.Role != instancemanager.RoleMaster || st.Current != member0 || !st.ServerActive {
		t.Errorf("master status = %+v", st)
	}
	if st.EngineVersion != "11.4.6.1963" {
		t.Errorf("engineVersion = %q", st.EngineVersion)
	}
	if rr := get(t, h, "/readyz", ""); rr.Code != http.StatusOK {
		t.Errorf("/readyz as master = %d", rr.Code)
	}

	cli.SetRole(instancemanager.RoleSlave)
	if st := role(t, h); st.Role != instancemanager.RoleSlave || st.ServerActive {
		t.Errorf("slave status = %+v", st)
	}
	if rr := get(t, h, "/readyz", ""); rr.Code != http.StatusOK {
		t.Errorf("/readyz as slave = %d", rr.Code)
	}

	cli.SetRole(instancemanager.RoleUnknown)
	if st := role(t, h); st.Role != instancemanager.RoleUnknown {
		t.Errorf("unknown status = %+v", st)
	}
	if rr := get(t, h, "/readyz", ""); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz without a role = %d, want 503", rr.Code)
	}
}

func TestFake_RoleCanBeChangedOverHTTP(t *testing.T) {
	cli := &fakeCLI{host: "demo-1", database: database, role: instancemanager.RoleSlave}
	h := newHandler(cli, testToken)

	if rr := get(t, h, "/fake/role?set=master", ""); rr.Code != http.StatusOK {
		t.Fatalf("set role = %d: %s", rr.Code, rr.Body.String())
	}
	if st := role(t, h); st.Role != instancemanager.RoleMaster {
		t.Errorf("role after set = %s", st.Role)
	}
	if rr := get(t, h, "/fake/role?set=primary", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("an unknown role = %d, want 400", rr.Code)
	}
	if cli.Role() != instancemanager.RoleMaster {
		t.Errorf("a rejected value changed the role to %s", cli.Role())
	}
}

// The real API keeps its token check: only /fake/role is open.
func TestFake_APIStillNeedsTheToken(t *testing.T) {
	h := newHandler(&fakeCLI{host: member0, database: database, role: instancemanager.RoleMaster}, testToken)
	if rr := get(t, h, "/v1/role", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("/v1/role without a token = %d, want 401", rr.Code)
	}
}
