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

// readinessCLI holds each status command until released or its context
// ends, and counts the commands that ran.
type readinessCLI struct {
	entered  chan struct{}
	release  chan struct{}
	canceled chan struct{}
	output   string
	// unwind, when set, holds a cancelled command until it is closed, as a
	// command that takes a while to end after its kill.
	unwind chan struct{}
}

func newReadinessCLI(output string) *readinessCLI {
	return &readinessCLI{
		entered: make(chan struct{}, 8), release: make(chan struct{}),
		canceled: make(chan struct{}, 8), output: output,
	}
}

func (c *readinessCLI) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	c.entered <- struct{}{}
	select {
	case <-c.release:
		return c.output, nil
	case <-ctx.Done():
		c.canceled <- struct{}{}
		if c.unwind != nil {
			<-c.unwind
		}
		return "", ctx.Err()
	}
}

// readinessHarness sends /readyz requests in the background.
type readinessHarness struct {
	t        *testing.T
	cli      *readinessCLI
	h        http.Handler
	release  sync.Once
	requests sync.WaitGroup
}

func newReadinessHarness(t *testing.T, standalone bool, output string, logger *slog.Logger) *readinessHarness {
	t.Helper()
	cli := newReadinessCLI(output)
	srv := NewServer(cli, "tok")
	if standalone {
		srv.WithStandaloneDatabase(dbName)
	}
	if logger != nil {
		srv.WithLogger(logger)
	}
	rh := &readinessHarness{t: t, cli: cli, h: srv.Handler()}
	t.Cleanup(func() {
		rh.releaseCommands()
		rh.requests.Wait()
	})
	return rh
}

func (rh *readinessHarness) releaseCommands() { rh.release.Do(func() { close(rh.cli.release) }) }

func (rh *readinessHarness) get(ctx context.Context) <-chan *httptest.ResponseRecorder {
	result := make(chan *httptest.ResponseRecorder, 1)
	rh.requests.Go(func() {
		rr := httptest.NewRecorder()
		rh.h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
		result <- rr
	})
	return result
}

func (rh *readinessHarness) waitCommand() {
	rh.t.Helper()
	select {
	case <-rh.cli.entered:
	case <-time.After(5 * time.Second):
		rh.t.Fatal("no status command started")
	}
}

func (rh *readinessHarness) noNewCommand() {
	rh.t.Helper()
	select {
	case <-rh.cli.entered:
		rh.t.Fatal("a concurrent readiness request started another status command")
	case <-time.After(100 * time.Millisecond):
	}
}

func (rh *readinessHarness) answer(result <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	rh.t.Helper()
	select {
	case rr := <-result:
		return rr
	case <-time.After(5 * time.Second):
		rh.t.Fatal("readiness request did not finish")
		return nil
	}
}

func readinessCases(t *testing.T, run func(t *testing.T, standalone bool, healthy string)) {
	t.Run("ha", func(t *testing.T) { run(t, false, masterOut) })
	t.Run("standalone", func(t *testing.T) { run(t, true, serverStatusRunning) })
}

// A request that arrives while a check runs, such as the kubelet's probe
// beside another caller, waits for that check and gets its answer: it is
// never told that a healthy member is not ready, and it starts no command of
// its own (#494).
func TestServer_Readyz_ConcurrentRequestsShareTheRunningCheck(t *testing.T) {
	readinessCases(t, func(t *testing.T, standalone bool, healthy string) {
		rh := newReadinessHarness(t, standalone, healthy, nil)
		first := rh.get(t.Context())
		rh.waitCommand()
		others := []<-chan *httptest.ResponseRecorder{rh.get(t.Context()), rh.get(t.Context())}
		rh.noNewCommand()
		if rr := doReq(t, rh.h, "/livez", "", testRemoteAddr); rr.Code != http.StatusOK {
			t.Errorf("livez during readiness check = %d, want 200", rr.Code)
		}
		rh.releaseCommands()
		for i, result := range append([]<-chan *httptest.ResponseRecorder{first}, others...) {
			rr := rh.answer(result)
			if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != `{"ready":true}` {
				t.Errorf("request %d = %d %s, want 200 with ready=true", i, rr.Code, rr.Body.String())
			}
		}
		// The answer is not kept: the next request reads the engine again.
		next := rh.get(t.Context())
		rh.waitCommand()
		if rr := rh.answer(next); rr.Code != http.StatusOK {
			t.Errorf("next readiness = %d, want a new successful check", rr.Code)
		}
	})
}

// A caller that leaves does not take the check away from the callers that
// still wait for it; once no caller waits, the command is cancelled and the
// next request starts a fresh one.
func TestServer_Readyz_CheckOutlivesACallerThatLeaves(t *testing.T) {
	readinessCases(t, func(t *testing.T, standalone bool, healthy string) {
		rh := newReadinessHarness(t, standalone, healthy, nil)
		firstCtx, cancelFirst := context.WithCancel(t.Context())
		defer cancelFirst()
		first := rh.get(firstCtx)
		rh.waitCommand()
		secondCtx, cancelSecond := context.WithCancel(t.Context())
		defer cancelSecond()
		second := rh.get(secondCtx)
		rh.noNewCommand()

		cancelFirst()
		if rr := rh.answer(first); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("cancelled request = %d, want 503", rr.Code)
		}
		select {
		case <-rh.cli.canceled:
			t.Fatal("the check was cancelled while another caller still waited for it")
		case <-time.After(100 * time.Millisecond):
		}

		cancelSecond()
		rh.answer(second)
		select {
		case <-rh.cli.canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("the check went on after every caller had left")
		}

		rh.releaseCommands()
		next := rh.get(t.Context())
		rh.waitCommand()
		if rr := rh.answer(next); rr.Code != http.StatusOK {
			t.Errorf("readiness after an abandoned check = %d, want a new successful check", rr.Code)
		}
	})
}

// A request that arrives while an abandoned command is still ending starts
// no command of its own until that one has ended: unauthenticated callers
// never run two status commands at once.
func TestServer_Readyz_WaitsForAnAbandonedCommandToEnd(t *testing.T) {
	readinessCases(t, func(t *testing.T, standalone bool, healthy string) {
		rh := newReadinessHarness(t, standalone, healthy, nil)
		unwind := make(chan struct{})
		var unwound sync.Once
		rh.cli.unwind = unwind
		t.Cleanup(func() { unwound.Do(func() { close(unwind) }) })

		firstCtx, cancelFirst := context.WithCancel(t.Context())
		first := rh.get(firstCtx)
		rh.waitCommand()
		cancelFirst()
		rh.answer(first)
		select {
		case <-rh.cli.canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("the abandoned check was not cancelled")
		}

		next := rh.get(t.Context())
		rh.noNewCommand()
		rh.releaseCommands()
		unwound.Do(func() { close(unwind) })
		rh.waitCommand()
		if rr := rh.answer(next); rr.Code != http.StatusOK {
			t.Errorf("readiness after the abandoned command ended = %d, want a new successful check", rr.Code)
		}
	})
}

// The reason in the log is the reason of the check, logged once for the check
// however many callers shared it, and never replaced by the mere overlap of
// two requests.
func TestServer_Readyz_SharedCheckLogsItsOwnReason(t *testing.T) {
	cases := []struct {
		name       string
		standalone bool
		output     string
		reason     string
	}{
		{"ha", false, transitionOut, "no HA-Node Info line"},
		{"standalone", true, serverStatusStopped, "is not running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			log, logger := lockedLog()
			rh := newReadinessHarness(t, c.standalone, c.output, logger)
			first := rh.get(t.Context())
			rh.waitCommand()
			second := rh.get(t.Context())
			rh.noNewCommand()
			rh.releaseCommands()
			for _, result := range []<-chan *httptest.ResponseRecorder{first, second} {
				if rr := rh.answer(result); rr.Code != http.StatusServiceUnavailable {
					t.Errorf("readiness of a member that is not ready = %d, want 503", rr.Code)
				}
			}
			got := log()
			if n := strings.Count(got, `"event":"not_ready"`); n != 1 {
				t.Errorf("a shared check was logged %d times, want once: %s", n, got)
			}
			if !strings.Contains(got, c.reason) || strings.Contains(got, "in progress") {
				t.Errorf("the log does not carry the check's own reason (%q): %s", c.reason, got)
			}
		})
	}
}

// A check that every caller left, such as a status command that hangs past
// the kubelet's timeout, is logged: a member that cannot answer its probe is
// not left silent.
func TestServer_Readyz_LogsAnAbandonedCheck(t *testing.T) {
	log, logger := lockedLog()
	rh := newReadinessHarness(t, true, serverStatusRunning, logger)
	ctx, cancel := context.WithCancel(t.Context())
	result := rh.get(ctx)
	rh.waitCommand()
	cancel()
	rh.answer(result)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log(), `"event":"not_ready"`) {
		if time.Now().After(deadline) {
			t.Fatalf("an abandoned check left no not_ready line: %s", log())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := log(); !strings.Contains(got, "no request waited") {
		t.Errorf("the log does not say that the check was abandoned: %s", got)
	}
}

// A check whose command answered keeps that answer when its last caller
// leaves before the answer is taken: it is not logged as abandoned.
func TestServer_Readyz_AnAnsweredCheckIsNotLoggedAsAbandoned(t *testing.T) {
	log, logger := lockedLog()
	srv := NewServer(fakeCLI{out: serverStatusStopped}, "tok").WithStandaloneDatabase(dbName).WithLogger(logger)
	answered, resume := make(chan struct{}), make(chan struct{})
	srv.afterReadinessCheck = func() {
		close(answered)
		<-resume
	}
	h := srv.Handler()
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
		result <- rr
	}()
	<-answered
	cancel()
	<-result
	close(resume)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log(), `"event":"not_ready"`) {
		if time.Now().After(deadline) {
			t.Fatalf("the answered check left no not_ready line: %s", log())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := log(); strings.Contains(got, readinessAbandoned) || !strings.Contains(got, "is not running") {
		t.Errorf("the answered check was not logged with its own reason: %s", got)
	}
}

// panickingCLI panics in its command, as a parser bug would.
type panickingCLI struct{}

func (panickingCLI) Run(context.Context, string, ...string) (string, error) { panic("parser bug") }

// A panic in the readiness check, which runs apart from the request, answers
// not ready and leaves the manager serving.
func TestServer_Readyz_APanicInTheCheckAnswersNotReady(t *testing.T) {
	log, logger := lockedLog()
	h := NewServer(panickingCLI{}, "tok").WithLogger(logger).Handler()
	for range 2 {
		if rr := doReq(t, h, "/readyz", "", testRemoteAddr); rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz after a panic in the check = %d, want 503", rr.Code)
		}
	}
	if rr := doReq(t, h, "/livez", "", testRemoteAddr); rr.Code != http.StatusOK {
		t.Errorf("/livez after a panic in the check = %d, want 200", rr.Code)
	}
	if got := log(); !strings.Contains(got, "parser bug") || strings.Count(got, `"stack"`) != 1 {
		t.Errorf("the panic is not logged with its stack once: %s", got)
	}
}

// lockedLog is a JSON logger at debug level and a reader of what it wrote,
// safe to use from the readiness check's goroutine.
func lockedLog() (read func() string, logger *slog.Logger) {
	var buf bytes.Buffer
	var mu sync.Mutex
	logger = slog.New(slog.NewJSONHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}, logger
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
