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
	"net"
	"net/http"
	"net/http/httptest"
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

// A stop file that cannot be read says nothing about whether a stop was asked
// for: the manager starts nothing (fail closed).
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
	if err := f.server.ResumeAndServe(context.Background(), listenFor(t, f.handler)); err != nil {
		t.Fatal(err)
	}
	wantNoStart(t, cli.recorded())
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
