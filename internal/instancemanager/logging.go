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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ComponentName is how the Instance Manager names itself in its log
// (docs/observability.md).
const ComponentName = "instance-manager"

// requestIDHeader carries the ID of one call, from the caller or made here.
const requestIDHeader = "X-Request-ID"

// Events and reasons of the Instance Manager's log.
const (
	eventRequest        = "request"
	eventRequestRefused = "request_refused"
	eventCommand        = "command"
	eventOperationState = "operation_state_changed"

	reasonNoToken    = "NoToken"
	reasonWrongToken = "WrongToken"

	// keyStatus is the log key of an HTTP status; a CUBRID command with
	// this word as its second argument only reads state.
	keyStatus = "status"
)

// redactedValue replaces a secret in the log.
const redactedValue = "[REDACTED]"

// maxOutputTail is how much of a failed command's output is logged.
const maxOutputTail = 1000

// LogSettings is what every line of the Instance Manager's log carries, and
// what it must never show.
type LogSettings struct {
	Level    slog.Level
	Member   string
	Database string
	// Secrets are values that are removed from anything that is logged: a
	// message, a field, an error, a command's output.
	Secrets []string
}

// NewLogger returns the Instance Manager's structured logger: JSON lines
// that each name the component, the member and the database.
func NewLogger(w io.Writer, s LogSettings) *slog.Logger {
	secrets := make([]string, 0, len(s.Secrets))
	for _, secret := range s.Secrets {
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	redact := func(text string) string {
		for _, secret := range secrets {
			text = strings.ReplaceAll(text, secret, redactedValue)
		}
		return text
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: s.Level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch v := a.Value.Any().(type) {
			case string:
				a.Value = slog.StringValue(redact(v))
			case error:
				a.Value = slog.StringValue(redact(v.Error()))
			}
			return a
		},
	})
	return slog.New(handler).With("component", ComponentName, "member", s.Member, "database", s.Database)
}

// ParseLogLevel reads a level name: debug, info, warn or error. An empty
// name is info.
func ParseLogLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("log level %q is not debug, info, warn or error", name)
}

// discardLogger is used until a logger is set.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// LoggingCLI logs every command that the wrapped CLI runs: the command, its
// exit code and how long it took, and the end of its output when it failed.
type LoggingCLI struct {
	CLI    CLI
	Logger *slog.Logger
}

// readOnlyCommand reports whether a command only reads state. Those run at
// every probe and are logged below info when they succeed.
func readOnlyCommand(name string, args []string) bool {
	if name == "cubrid_rel" {
		return true
	}
	if len(args) == 0 {
		return false
	}
	return args[0] == "applyinfo" || (len(args) > 1 && args[1] == keyStatus)
}

func (c LoggingCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	start := time.Now()
	out, err := c.CLI.Run(ctx, name, args...)
	attrs := []any{
		"event", eventCommand,
		"command", strings.Join(append([]string{name}, args...), " "),
		"durationMs", time.Since(start).Milliseconds(),
	}
	if err == nil {
		level := slog.LevelInfo
		if readOnlyCommand(name, args) {
			level = slog.LevelDebug
		}
		c.Logger.Log(ctx, level, "Ran command", append(attrs, "exitCode", 0)...)
		return out, nil
	}
	exitCode := -1
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		exitCode = exit.ExitCode()
	}
	tail := strings.TrimSpace(out)
	if len(tail) > maxOutputTail {
		tail = tail[len(tail)-maxOutputTail:]
	}
	c.Logger.Warn("Command failed", append(attrs, "exitCode", exitCode, "err", err, "outputTail", tail)...)
	return out, err
}

// WithLogger makes the server log its requests, refused callers and
// operations. Returns the server for chaining.
func (s *Server) WithLogger(logger *slog.Logger) *Server {
	s.logger = logger
	if s.store != nil {
		s.store.SetLogger(logger)
	}
	return s
}

func (s *Server) log() *slog.Logger {
	if s.logger == nil {
		return discardLogger()
	}
	return s.logger
}

// polledPaths are asked at every probe or reconcile. They are logged below
// info unless the status of the answer changed.
var polledPaths = map[string]bool{"/livez": true, "/readyz": true, "/v1/role": true, "/v1/ha/status": true}

// usableRequestID is what a caller's request ID must look like to be kept.
var usableRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}

// statusRecorder remembers the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests gives every call a request ID, returns it to the caller, and
// logs the call once it is answered. A refused call is logged where it is
// refused, with its reason.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !usableRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusUnauthorized {
			return
		}

		level := slog.LevelInfo
		if r.Method == http.MethodGet && polledPaths[r.URL.Path] {
			s.pollMu.Lock()
			if last, seen := s.pollStatus[r.URL.Path]; seen && last == rec.status {
				level = slog.LevelDebug
			}
			s.pollStatus[r.URL.Path] = rec.status
			s.pollMu.Unlock()
		} else if rec.status >= http.StatusBadRequest {
			level = slog.LevelWarn
		}
		s.log().Log(r.Context(), level, "Answered request",
			"event", eventRequest, "requestID", id, "method", r.Method, "path", r.URL.Path,
			keyStatus, rec.status, "durationMs", time.Since(start).Milliseconds(), "loopback", isLoopback(r.RemoteAddr))
	})
}

// logRefused records a caller that was refused. Nothing of what the caller
// sent as a credential is written.
func (s *Server) logRefused(w http.ResponseWriter, r *http.Request, reason string) {
	s.log().Warn("Refused request",
		"event", eventRequestRefused, "reason", reason, "requestID", w.Header().Get(requestIDHeader),
		"method", r.Method, "path", r.URL.Path, keyStatus, http.StatusUnauthorized, "loopback", false)
}
