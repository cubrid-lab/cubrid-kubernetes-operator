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
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// closedManagerURL is the address of a manager that does not listen yet.
func closedManagerURL(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	return url
}

// listenFor is the serve function of ResumeAndServe for handler.
func listenFor(t *testing.T, handler http.Handler) func() error {
	return func() error {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		ts := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}}
		ts.Start()
		t.Cleanup(ts.Close)
		return nil
	}
}

// captureLog makes s write its log into the returned buffer.
func captureLog(s *Server) *bytes.Buffer {
	var buf bytes.Buffer
	s.WithLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	return &buf
}

// logged says whether a line of buf has the field key set to value.
func logged(t *testing.T, buf *bytes.Buffer, key, value string) bool {
	t.Helper()
	for _, line := range logLines(t, buf) {
		if line[key] == value {
			return true
		}
	}
	return false
}

func wantNoStart(t *testing.T, calls []string) {
	t.Helper()
	for _, call := range calls {
		if call == callServerStart || call == callHeartbeatStart {
			t.Errorf("calls = %q: %q ran after a stop that was asked for", calls, call)
		}
	}
}

// A shutdown that arrives after the manager was launched but before it
// listens finds nobody, and "instance-manager shutdown" stops CUBRID itself.
// The manager, which then listens, starts nothing: not the database of a
// completed restore, and no operation asked for afterwards. It behaves as a
// member that is stopping, and the restore's marker stays (#323).
func TestResumeAndServe_StartsNothingAfterAShutdownBeforeItListened(t *testing.T) {
	f := completedRestoreWithMarker(t)
	marker := f.marker()
	stopFile := filepath.Join(t.TempDir(), "stop-requested")
	cli := &stepCLI{}
	f.start(cli)
	f.server = f.server.WithStopFile(stopFile)
	log := captureLog(f.server)

	// "instance-manager shutdown": it records the stop, finds no manager
	// and stops CUBRID itself.
	if err := RecordStopRequest(stopFile); err != nil {
		t.Fatal(err)
	}
	fallback := &countingCLI{}
	if err := RequestShutdown(context.Background(), closedManagerURL(t), "tok", dbName, fallback, 5*time.Second); err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if got := strings.Join(fallback.recorded(), "; "); got != stopHeartbeat+"; "+stopServer {
		t.Fatalf("fallback commands = %q, want the stop run by the command itself", got)
	}

	if err := f.server.ResumeAndServe(context.Background(), listenFor(t, f.handler)); err != nil {
		t.Fatal(err)
	}
	wantNoStart(t, cli.recorded())
	// Only the look right after the listen keeps the resume from running at
	// all; the look before each start would refuse the start alone.
	if logged(t, log, "msg", "Starting the database of a completed restore") {
		t.Error("the resume ran although a stop was recorded before the manager listened")
	}
	if !logged(t, log, "event", "stop_recorded") {
		t.Errorf("no stop_recorded line in the log: %s", log)
	}
	if !f.server.StopIntended() {
		t.Error("the manager does not treat the absence of CUBRID as intended")
	}
	body, _ := json.Marshal(f.req)
	rr := postOperation(f.handler, "/v1/restore/prepare", string(body), "restore-2")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), errStopping.Error()) {
		t.Errorf("/v1/restore/prepare after the stop = %d (%s), want 503 because the member is stopping", rr.Code, rr.Body.String())
	}
	if got := f.marker(); got != marker {
		t.Errorf("marker = %q, want it kept as %q", got, marker)
	}
}

// A stop recorded while an operation runs keeps that operation from starting
// CUBRID, even when it did not reach the manager.
func TestResumeCompletedRestores_StartsNothingOnceAStopIsRecorded(t *testing.T) {
	f := completedRestoreWithMarker(t)
	stopFile := filepath.Join(t.TempDir(), "stop-requested")
	cli := &stepCLI{}
	f.start(cli)
	f.server = f.server.WithStopFile(stopFile)
	if err := RecordStopRequest(stopFile); err != nil {
		t.Fatal(err)
	}
	if err := f.server.ResumeCompletedRestores(t.Context()); err == nil {
		t.Error("ResumeCompletedRestores reported no error after a stop was recorded")
	}
	wantNoStart(t, cli.recorded())
	if !f.server.StopIntended() {
		t.Error("the manager does not treat the absence of CUBRID as intended")
	}
}

// On an HA member, a recorded stop keeps the start of a completed restore's
// database from issuing `cubrid heartbeat start`; without one, heartbeat is
// started.
func TestResumeCompletedRestores_HAMemberStartsNoHeartbeatOnceAStopIsRecorded(t *testing.T) {
	for name, recorded := range map[string]bool{"stop recorded": true, "no stop recorded": false} {
		t.Run(name, func(t *testing.T) {
			f := newResumeFixture(t)
			f.haConf = filepath.Join(t.TempDir(), "cubrid_ha.conf")
			if err := os.WriteFile(f.haConf, []byte(testHAConf), 0o600); err != nil {
				t.Fatal(err)
			}
			f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}, databases: f.databases})
			done := f.run("restore-1")
			if done.State != OpCompleted {
				t.Fatalf("restore = %s (%s), want Completed", done.State, done.FailureReason)
			}
			if err := os.WriteFile(filepath.Join(f.databases, dbName, ownershipMarker), []byte(done.ID+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			// Restarted with heartbeat stopped, so a start would be issued.
			restarted := &volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}, databases: f.databases}
			f.start(restarted)
			stopFile := filepath.Join(t.TempDir(), "stop-requested")
			f.server = f.server.WithStopFile(stopFile)
			if recorded {
				if err := RecordStopRequest(stopFile); err != nil {
					t.Fatal(err)
				}
			}
			err := f.server.ResumeCompletedRestores(t.Context())
			starts := count(restarted.recorded(), callHeartbeatStart)
			if !recorded {
				if err != nil || starts != 1 {
					t.Errorf("ResumeCompletedRestores = %v, calls = %q; want one heartbeat start", err, restarted.recorded())
				}
				return
			}
			if err == nil || starts != 0 {
				t.Errorf("ResumeCompletedRestores = %v, calls = %q; want an error and no heartbeat start", err, restarted.recorded())
			}
			if got := f.marker(); got != done.ID {
				t.Errorf("marker = %q, want it kept as %q", got, done.ID)
			}
		})
	}
}

// A stop file that cannot be read says nothing about whether a stop was asked
// for: the manager starts nothing (fail closed), and logs the broken path
// rather than a recorded stop.
func TestResumeAndServe_StartsNothingWhenTheStopFileCannotBeRead(t *testing.T) {
	f := completedRestoreWithMarker(t)
	cli := &stepCLI{}
	f.start(cli)
	// A path below a regular file cannot be examined: stat fails with
	// ENOTDIR, not "does not exist".
	notADir := filepath.Join(t.TempDir(), "file")
	if err := RecordStopRequest(notADir); err != nil {
		t.Fatal(err)
	}
	f.server = f.server.WithStopFile(filepath.Join(notADir, "stop-requested"))
	log := captureLog(f.server)
	if err := f.server.ResumeAndServe(context.Background(), listenFor(t, f.handler)); err != nil {
		t.Fatal(err)
	}
	wantNoStart(t, cli.recorded())
	if !logged(t, log, "event", "stop_file_unreadable") || logged(t, log, "event", "stop_recorded") {
		t.Errorf("log = %s, want stop_file_unreadable and no stop_recorded", log)
	}
}

// Without a recorded stop the manager starts the database of a completed
// restore as before.
func TestResumeAndServe_StartsWithoutARecordedStop(t *testing.T) {
	f := completedRestoreWithMarker(t)
	cli := &stepCLI{}
	f.start(cli)
	f.server = f.server.WithStopFile(filepath.Join(t.TempDir(), "stop-requested"))
	if err := f.server.ResumeAndServe(context.Background(), listenFor(t, f.handler)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cli.recorded(), callServerStart) {
		t.Errorf("calls = %q, want the database of the completed restore started", cli.recorded())
	}
}

// The record is only created: an existing file is not truncated, and a
// symbolic link in its place is not followed.
func TestRecordStopRequest_CreatesWithoutTruncatingOrFollowing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stop-requested")
	if err := RecordStopRequest(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordStopRequest(path); err != nil {
		t.Fatalf("a second record: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "kept" {
		t.Errorf("content = %q, want the existing file untouched", data)
	}
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RecordStopRequest(link); err == nil {
		t.Error("a symbolic link was followed")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("the link's target was created (err=%v)", err)
	}
}
