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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testLoopbackAddr = "127.0.0.1:6000"

// Every route below /v1, with a body that would start work if it were let in.
var v1Routes = []struct{ method, path, body string }{
	{http.MethodGet, "/v1/role", ""},
	{http.MethodGet, "/v1/ha/status", ""},
	{http.MethodGet, "/v1/ha/convergence?database=appdb&copiedLogPath=/tmp/x", ""},
	{http.MethodPost, "/v1/ha/bootstrap", `{"database":"appdb"}`},
	{http.MethodPost, "/v1/backup", `{"database":"appdb","destination":"` + testStagingRoot + `/bk"}`},
	{http.MethodPost, "/v1/restore/prepare", `{"database":"appdb"}`},
	{http.MethodGet, "/v1/operations/op-1", ""},
	{http.MethodPost, "/v1/shutdown?database=appdb", ""},
}

func sendV1(h http.Handler, method, path, body, authorization, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if authorization != "" {
		req.Header.Set(authHeader, authorization)
	}
	req.RemoteAddr = remoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// A request without the token is refused on every /v1 route, wherever it
// comes from, and it runs no command (#271).
func TestAuth_EveryV1RouteNeedsTheToken(t *testing.T) {
	refused := map[string]string{
		"no token":            "",
		"a wrong token":       "Bearer not-the-token",
		"a prefix of it":      "Bearer to",
		"it with a suffix":    "Bearer tokk",
		"it without a scheme": "tok",
		"an empty bearer":     "Bearer ",
	}
	for _, origin := range []string{testRemoteAddr, testLoopbackAddr, "[::1]:6000"} {
		for name, authorization := range refused {
			for _, route := range v1Routes {
				cli := &countingCLI{}
				s := NewServer(cli, "tok").WithBackupStagingRoot(testStagingRoot)
				rr := sendV1(s.Handler(), route.method, route.path, route.body, authorization, origin)
				if rr.Code != http.StatusUnauthorized {
					t.Errorf("%s %s from %s with %s = %d, want 401", route.method, route.path, origin, name, rr.Code)
				}
				if got := cli.recorded(); len(got) != 0 {
					t.Errorf("%s %s from %s with %s ran %q", route.method, route.path, origin, name, got)
				}
				if s.StopIntended() {
					t.Errorf("%s %s from %s with %s recorded a stop as intended", route.method, route.path, origin, name)
				}
			}
		}
	}
}

// The valid token is accepted from a remote address and from loopback alike.
func TestAuth_TheTokenIsAcceptedFromEveryOrigin(t *testing.T) {
	for _, origin := range []string{testRemoteAddr, testLoopbackAddr} {
		h := NewServer(fakeCLI{out: masterOut}, "tok").Handler()
		if rr := sendV1(h, http.MethodGet, "/v1/role", "", "Bearer tok", origin); rr.Code != http.StatusOK {
			t.Errorf("/v1/role from %s with the token = %d, want 200", origin, rr.Code)
		}
	}
}

// A manager that was given no token lets nobody in: there is no value a
// caller could send that it accepts. The probes do not need a token.
func TestAuth_AnEmptyTokenRefusesEveryV1Request(t *testing.T) {
	for _, origin := range []string{testRemoteAddr, testLoopbackAddr} {
		for _, authorization := range []string{"", "Bearer ", "Bearer tok"} {
			for _, route := range v1Routes {
				cli := &countingCLI{}
				s := NewServer(cli, "").WithBackupStagingRoot(testStagingRoot)
				rr := sendV1(s.Handler(), route.method, route.path, route.body, authorization, origin)
				if rr.Code != http.StatusUnauthorized {
					t.Errorf("%s %s from %s with %q = %d, want 401", route.method, route.path, origin, authorization, rr.Code)
				}
				if got := cli.recorded(); len(got) != 0 {
					t.Errorf("%s %s from %s with %q ran %q", route.method, route.path, origin, authorization, got)
				}
			}
		}
	}
	h := NewServer(fakeCLI{out: masterOut}, "").Handler()
	for _, probe := range []string{"/livez", "/readyz"} {
		if rr := sendV1(h, http.MethodGet, probe, "", "", testRemoteAddr); rr.Code != http.StatusOK {
			t.Errorf("%s without a token = %d, want 200", probe, rr.Code)
		}
	}
}

// "instance-manager shutdown" sends the token of its Pod. When the manager
// refuses it, the command reports that and does not stop CUBRID itself: the
// local stop is only for a manager that does not answer at all.
func TestRequestShutdown_Authorization(t *testing.T) {
	t.Run("the valid token stops CUBRID through the manager", func(t *testing.T) {
		serverCLI, localCLI := &countingCLI{}, &countingCLI{}
		s := NewServer(serverCLI, "tok")
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		if err := RequestShutdown(context.Background(), ts.URL, "tok", "appdb", localCLI, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(serverCLI.recorded(), "; "); got != stopHeartbeat+"; "+stopServer {
			t.Errorf("the manager ran %q, want one heartbeat stop and one server stop", got)
		}
		if len(localCLI.recorded()) != 0 {
			t.Errorf("the command itself ran %q", localCLI.recorded())
		}
	})
	for name, token := range map[string]string{"a wrong token": "not-the-token", "no token": ""} {
		t.Run(name+" is refused and nothing is stopped", func(t *testing.T) {
			serverCLI, localCLI := &countingCLI{}, &countingCLI{}
			s := NewServer(serverCLI, "tok")
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			err := RequestShutdown(context.Background(), ts.URL, token, "appdb", localCLI, 5*time.Second)
			if err == nil || !strings.Contains(err.Error(), "401") {
				t.Fatalf("err = %v, want the manager's 401", err)
			}
			if len(serverCLI.recorded()) != 0 || len(localCLI.recorded()) != 0 {
				t.Errorf("manager ran %q, the command itself ran %q; want neither", serverCLI.recorded(), localCLI.recorded())
			}
			if s.StopIntended() {
				t.Error("a refused shutdown is recorded as intended")
			}
		})
	}
}
