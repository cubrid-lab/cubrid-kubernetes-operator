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
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// /readyz answers anyone who can reach the port, without a token. It says
// whether the member is ready and nothing else: not the role, which /v1/role
// gives only to a caller with the token, and not the text of a failed command
// (#333).
func TestServer_Readyz_SaysOnlyWhetherTheMemberIsReady(t *testing.T) {
	const (
		answerReady    = `{"ready":true}`
		answerNotReady = `{"ready":false}`
	)
	const commandError = "cubrid server status: exit status 1: cannot open /var/lib/cubrid/secret-path"
	standalone := func(cli CLI) *Server { return NewServer(cli, "tok").WithStandaloneDatabase(dbName) }
	ha := func(cli CLI) *Server { return NewServer(cli, "tok") }
	cases := []struct {
		name   string
		server *Server
		status int
		body   string
		// reason is what the manager's own log has to say about a member
		// that is not ready.
		reason string
	}{
		{"an HA master", ha(fakeCLI{out: masterOut}), http.StatusOK, answerReady, ""},
		{"an HA member in a transition", ha(fakeCLI{out: transitionOut}), http.StatusServiceUnavailable, answerNotReady,
			"no HA-Node Info line"},
		{"an HA member whose heartbeat does not answer", ha(fakeCLI{err: errors.New(commandError)}),
			http.StatusServiceUnavailable, answerNotReady, ""},
		{"a standalone server that runs", standalone(fakeCLI{out: serverStatusRunning}), http.StatusOK, answerReady, ""},
		{"a standalone server that is stopped", standalone(fakeCLI{out: serverStatusStopped}),
			http.StatusServiceUnavailable, answerNotReady, "is not running"},
		{"a standalone server whose status command fails", standalone(fakeCLI{err: errors.New(commandError)}),
			http.StatusServiceUnavailable, answerNotReady, "secret-path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var log bytes.Buffer
			h := c.server.WithLogger(slog.New(slog.NewJSONHandler(&log, nil))).Handler()
			rr := doReq(t, h, "/readyz", "", testRemoteAddr)
			if rr.Code != c.status {
				t.Errorf("/readyz = %d, want %d", rr.Code, c.status)
			}
			if got := strings.TrimSpace(rr.Body.String()); got != c.body {
				t.Errorf("/readyz answered %s without a token, want only %s", got, c.body)
			}
			if c.reason != "" && !strings.Contains(log.String(), c.reason) {
				t.Errorf("the manager's log does not say why the member is not ready (%q): %s", c.reason, log.String())
			}
		})
	}
}

// The kubelet asks every few seconds: the reason is logged at info when it
// is new, and below info while it stays the same.
func TestServer_Readyz_LogsAReasonOnceWhileItStays(t *testing.T) {
	var log bytes.Buffer
	cli := &switchCLI{out: serverStatusStopped}
	h := NewServer(cli, "tok").WithStandaloneDatabase(dbName).
		WithLogger(slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelInfo}))).Handler()
	notReady := func() int { return strings.Count(log.String(), `"event":"not_ready"`) }

	for range 3 {
		doReq(t, h, "/readyz", "", testRemoteAddr)
	}
	if got := notReady(); got != 1 {
		t.Fatalf("the same reason was logged %d times at info, want once", got)
	}
	cli.out = serverStatusRunning
	doReq(t, h, "/readyz", "", testRemoteAddr)
	cli.out = serverStatusStopped
	doReq(t, h, "/readyz", "", testRemoteAddr)
	if got := notReady(); got != 2 {
		t.Fatalf("a member that is not ready again was logged %d times in all, want twice", got)
	}
}

func TestServer_Livez_SaysOnlyThatTheManagerIsAlive(t *testing.T) {
	rr := doReq(t, NewServer(fakeCLI{out: masterOut}, "tok").Handler(), "/livez", "", testRemoteAddr)
	if got := strings.TrimSpace(rr.Body.String()); rr.Code != http.StatusOK || got != `{"status":"ok"}` {
		t.Errorf("/livez = %d %s", rr.Code, got)
	}
}
