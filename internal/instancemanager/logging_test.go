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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	logSecret  = "s3cr3t-token-value"
	logMember  = "demo-1"
	keyEvent   = "event"
	keyLevel   = "level"
	levelWarn  = "WARN"
	levelDebug = "DEBUG"
	levelInfo  = "INFO"
	authHeader = "Authorization"
	givenID    = "op-42"
)

// logLines returns the JSON objects written to buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for raw := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line is not JSON: %q", raw)
		}
		lines = append(lines, line)
	}
	return lines
}

func testLogger(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return NewLogger(buf, LogSettings{Level: level, Member: logMember, Database: "appdb", Secrets: []string{logSecret}})
}

func withEvent(lines []map[string]any, event string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l[keyEvent] == event {
			out = append(out, l)
		}
	}
	return out
}

// Every line says which component and member wrote it, and nothing that was
// registered as a secret is written, wherever it appears.
func TestLogger_CommonFieldsAndSecrets(t *testing.T) {
	var buf bytes.Buffer
	log := testLogger(&buf, slog.LevelInfo)
	log.Info("Started "+logSecret, "detail", "token is "+logSecret, "err", errors.New("failed with "+logSecret))
	lines := logLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	l := lines[0]
	if l["component"] != ComponentName || l["member"] != logMember || l["database"] != "appdb" {
		t.Errorf("common fields = %v", l)
	}
	if strings.Contains(buf.String(), logSecret) {
		t.Errorf("the log shows a secret:\n%s", buf.String())
	}
}

func TestParseLogLevel(t *testing.T) {
	for name, want := range map[string]slog.Level{"": slog.LevelInfo, "debug": slog.LevelDebug,
		"INFO": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, err := ParseLogLevel(name); err != nil || got != want {
			t.Errorf("ParseLogLevel(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := ParseLogLevel("loud"); err == nil {
		t.Error("an unknown level was accepted")
	}
}

type resultCLI struct {
	out string
	err error
}

func (c resultCLI) Run(context.Context, string, ...string) (string, error) { return c.out, c.err }

func TestLoggingCLI(t *testing.T) {
	t.Run("a command that changes something", func(t *testing.T) {
		var buf bytes.Buffer
		cli := LoggingCLI{CLI: resultCLI{out: "++ cubrid heartbeat start: success"}, Logger: testLogger(&buf, slog.LevelDebug)}
		if _, err := cli.Run(context.Background(), "cubrid", "heartbeat", "start"); err != nil {
			t.Fatal(err)
		}
		lines := withEvent(logLines(t, &buf), "command")
		if len(lines) != 1 || lines[0]["command"] != "cubrid heartbeat start" || lines[0][keyLevel] != levelInfo ||
			lines[0]["exitCode"] != float64(0) {
			t.Errorf("lines = %v", lines)
		}
		if _, has := lines[0]["durationMs"]; !has {
			t.Errorf("no duration: %v", lines[0])
		}
		if _, has := lines[0]["outputTail"]; has {
			t.Errorf("the output of a successful command is logged: %v", lines[0])
		}
	})
	t.Run("a command that only reads, run at every probe", func(t *testing.T) {
		var buf bytes.Buffer
		cli := LoggingCLI{CLI: resultCLI{out: "ok"}, Logger: testLogger(&buf, slog.LevelDebug)}
		_, _ = cli.Run(context.Background(), "cubrid", "heartbeat", "status")
		_, _ = cli.Run(context.Background(), "cubrid", "applyinfo", "-L", "/x", "-a", "appdb")
		for _, l := range withEvent(logLines(t, &buf), "command") {
			if l[keyLevel] != levelDebug {
				t.Errorf("a read-only command is logged at %v: %v", l[keyLevel], l)
			}
		}
	})
	t.Run("a command that fails", func(t *testing.T) {
		var buf bytes.Buffer
		out := strings.Repeat("x", 3000) + "\nERROR: cannot restore, key " + logSecret
		cli := LoggingCLI{CLI: resultCLI{out: out, err: errors.New("exit status 1")}, Logger: testLogger(&buf, slog.LevelInfo)}
		if _, err := cli.Run(context.Background(), "cubrid", "restoredb", "appdb"); err == nil {
			t.Fatal("the error was swallowed")
		}
		lines := withEvent(logLines(t, &buf), "command")
		if len(lines) != 1 || lines[0][keyLevel] != levelWarn {
			t.Fatalf("lines = %v", lines)
		}
		tail, _ := lines[0]["outputTail"].(string)
		if !strings.Contains(tail, "ERROR: cannot restore") || len(tail) > 1100 {
			t.Errorf("outputTail (%d characters) = %q", len(tail), tail)
		}
		if strings.Contains(buf.String(), logSecret) {
			t.Errorf("the log shows a secret from the command's output")
		}
	})
	t.Run("a failed read-only command is not hidden", func(t *testing.T) {
		var buf bytes.Buffer
		cli := LoggingCLI{CLI: resultCLI{err: errors.New("exit status 1")}, Logger: testLogger(&buf, slog.LevelInfo)}
		_, _ = cli.Run(context.Background(), "cubrid", "heartbeat", "status")
		if lines := withEvent(logLines(t, &buf), "command"); len(lines) != 1 || lines[0][keyLevel] != levelWarn {
			t.Errorf("lines = %v", lines)
		}
	})
}

func logServer(buf *bytes.Buffer, level slog.Level) http.Handler {
	return NewServer(fakeCLI{out: slaveHeartbeat}, logSecret).WithLogger(testLogger(buf, level)).Handler()
}

func send(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.RemoteAddr = testRemoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// A refused request is logged as a warning with its reason, and neither the
// right token nor the wrong one that was sent is written.
func TestServer_LogsRefusedRequests(t *testing.T) {
	var buf bytes.Buffer
	h := logServer(&buf, slog.LevelInfo)
	send(h, http.MethodPost, "/v1/shutdown?database=appdb", nil)
	send(h, http.MethodGet, "/v1/role", map[string]string{authHeader: "Bearer guessed-token-123"})
	lines := withEvent(logLines(t, &buf), "request_refused")
	if len(lines) != 2 {
		t.Fatalf("refused lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	if lines[0][keyLevel] != levelWarn || lines[0]["reason"] != "NoToken" || lines[0]["path"] != "/v1/shutdown" ||
		lines[0]["method"] != http.MethodPost || lines[0]["status"] != float64(http.StatusUnauthorized) {
		t.Errorf("first refusal = %v", lines[0])
	}
	if lines[1]["reason"] != "WrongToken" {
		t.Errorf("second refusal = %v", lines[1])
	}
	if strings.Contains(buf.String(), logSecret) || strings.Contains(buf.String(), "guessed-token-123") {
		t.Errorf("the log shows a token:\n%s", buf.String())
	}
}

// A call is logged once with its request ID, which is the caller's when it
// sent one and is returned to the caller in either case.
func TestServer_LogsRequestsWithAnID(t *testing.T) {
	var buf bytes.Buffer
	h := logServer(&buf, slog.LevelInfo)
	auth := map[string]string{authHeader: "Bearer " + logSecret}
	given := send(h, http.MethodPost, "/v1/shutdown?database=appdb",
		map[string]string{authHeader: "Bearer " + logSecret, requestIDHeader: givenID})
	made := send(h, http.MethodPost, "/v1/shutdown?database=appdb", auth)
	if given.Header().Get(requestIDHeader) != givenID || made.Header().Get(requestIDHeader) == "" {
		t.Errorf("request IDs returned: %q, %q", given.Header().Get(requestIDHeader), made.Header().Get(requestIDHeader))
	}
	lines := withEvent(logLines(t, &buf), "request")
	if len(lines) != 2 {
		t.Fatalf("request lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	l := lines[0]
	if l["requestID"] != givenID || l["method"] != http.MethodPost || l["path"] != "/v1/shutdown" ||
		l["status"] != float64(http.StatusOK) || l["loopback"] != false {
		t.Errorf("request line = %v", l)
	}
	if lines[1]["requestID"] != made.Header().Get(requestIDHeader) {
		t.Errorf("the made ID differs between the log and the response")
	}
	// An ID from a caller is data from outside: it is not taken as it is.
	odd := send(h, http.MethodGet, "/v1/role",
		map[string]string{authHeader: "Bearer " + logSecret, requestIDHeader: "a\nb" + strings.Repeat("x", 200)})
	if id := odd.Header().Get(requestIDHeader); strings.ContainsAny(id, "\n ") || len(id) > 64 {
		t.Errorf("an unusable request ID was kept: %q", id)
	}
}

// What is asked at every probe is logged below info, unless its answer changes.
func TestServer_PollsAreQuietUntilTheAnswerChanges(t *testing.T) {
	var buf bytes.Buffer
	cli := &switchCLI{out: slaveHeartbeat}
	h := NewServer(cli, logSecret).WithLogger(testLogger(&buf, slog.LevelDebug)).Handler()
	levels := func() []any {
		var out []any
		for _, l := range withEvent(logLines(t, &buf), "request") {
			if l["path"] == "/readyz" {
				out = append(out, l[keyLevel])
			}
		}
		return out
	}
	for range 3 {
		send(h, http.MethodGet, "/readyz", nil)
	}
	cli.out = "" // heartbeat no longer answers: the member is not ready
	send(h, http.MethodGet, "/readyz", nil)
	send(h, http.MethodGet, "/readyz", nil)
	got := levels()
	want := []any{levelInfo, levelDebug, levelDebug, levelInfo, levelDebug}
	if len(got) != len(want) {
		t.Fatalf("levels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("levels = %v, want %v", got, want)
			break
		}
	}
}

type switchCLI struct{ out string }

func (c *switchCLI) Run(context.Context, string, ...string) (string, error) { return c.out, nil }

// An operation is logged when it is created and at each change of state,
// with its ID, and with the reason when it fails.
func TestOperationStore_LogsStateChanges(t *testing.T) {
	var buf bytes.Buffer
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.SetLogger(testLogger(&buf, slog.LevelInfo))
	op, _, err := store.FindOrCreate(OpBackup, "key-1", "hash", "appdb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(op.ID, func(o *Operation) { o.State = OpRunningBackup }); err != nil {
		t.Fatal(err)
	}
	// An update that changes no state is not a transition.
	if _, err := store.Update(op.ID, func(o *Operation) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(op.ID, func(o *Operation) {
		o.State, o.FailureReason = OpFailed, "upload refused with key "+logSecret
	}); err != nil {
		t.Fatal(err)
	}
	lines := withEvent(logLines(t, &buf), "operation_state_changed")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3:\n%s", len(lines), buf.String())
	}
	for _, l := range lines {
		if l["operationID"] != op.ID || l["kind"] != string(OpBackup) {
			t.Errorf("line without the operation: %v", l)
		}
	}
	last := lines[2]
	if last["state"] != string(OpFailed) || last["previousState"] != string(OpRunningBackup) || last[keyLevel] != levelWarn ||
		!strings.Contains(last["reason"].(string), "upload refused") {
		t.Errorf("failure line = %v", last)
	}
	if strings.Contains(buf.String(), logSecret) {
		t.Error("the log shows a secret from a failure reason")
	}
}
