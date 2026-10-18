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
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

// readinessCLI holds a status command until released or its request ends.
type readinessCLI struct {
	entered chan struct{}
	release chan struct{}
	output  string
}

func (c *readinessCLI) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	c.entered <- struct{}{}
	select {
	case <-c.release:
		return c.output, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Unauthenticated requests must not create an unbounded number of status
// processes. A busy check returns not-ready immediately, while a finished or
// cancelled check allows the next request to inspect the database again.
func TestServer_Readyz_BoundsConcurrentCommands(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		for _, cancelFirst := range []bool{false, true} {
			name := "ha"
			if standalone {
				name = "standalone"
			}
			if cancelFirst {
				name += "-cancelled"
			}
			t.Run(name, func(t *testing.T) {
				cli := &readinessCLI{entered: make(chan struct{}, 4), release: make(chan struct{}), output: masterOut}
				srv := NewServer(cli, "tok")
				if standalone {
					srv.WithStandaloneDatabase(dbName)
					cli.output = serverStatusRunning
				}
				h := srv.Handler()
				var release sync.Once
				var requests sync.WaitGroup
				t.Cleanup(func() {
					release.Do(func() { close(cli.release) })
					requests.Wait()
				})
				get := func(ctx context.Context) <-chan *httptest.ResponseRecorder {
					result := make(chan *httptest.ResponseRecorder, 1)
					requests.Go(func() {
						rr := httptest.NewRecorder()
						h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
						result <- rr
					})
					return result
				}
				firstCtx, cancel := context.WithCancel(t.Context())
				defer cancel()
				first := get(firstCtx)
				select {
				case <-cli.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("first request did not start its status command")
				}
				second := get(t.Context())
				select {
				case <-cli.entered:
					t.Fatal("a concurrent unauthenticated request started another status command")
				case rr := <-second:
					if rr.Code != http.StatusServiceUnavailable || strings.TrimSpace(rr.Body.String()) != `{"ready":false}` {
						t.Fatalf("busy readiness = %d %s, want 503 with only ready=false", rr.Code, rr.Body.String())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("concurrent readiness request waited for the running command")
				}
				if rr := doReq(t, h, "/livez", "", testRemoteAddr); rr.Code != http.StatusOK {
					t.Errorf("livez during readiness check = %d, want 200", rr.Code)
				}
				wantFirst := http.StatusOK
				if cancelFirst {
					cancel()
					wantFirst = http.StatusServiceUnavailable
				} else {
					release.Do(func() { close(cli.release) })
				}
				select {
				case rr := <-first:
					if rr.Code != wantFirst {
						t.Errorf("first readiness = %d, want %d", rr.Code, wantFirst)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("first request did not finish after release or cancellation")
				}
				release.Do(func() { close(cli.release) })
				if rr := doReq(t, h, "/readyz", "", testRemoteAddr); rr.Code != http.StatusOK {
					t.Errorf("next readiness = %d, want a new successful check", rr.Code)
				}
			})
		}
	}
}
