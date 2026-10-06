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
	"sync"
	"testing"
	"time"
)

// countingCLI records the commands it is asked to run.
type countingCLI struct {
	mu    sync.Mutex
	calls []string
}

func (c *countingCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, strings.Join(append([]string{name}, args...), " "))
	return "ok", nil
}

func (c *countingCLI) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

const (
	stopHeartbeat = "cubrid heartbeat stop"
	stopServer    = "cubrid server stop appdb"
)

// The stop of a member is implemented once. Whoever asks, and however often,
// CUBRID is stopped one time, and from then on its absence is intended.
func TestServer_Stop_IsIdempotent(t *testing.T) {
	cli := &countingCLI{}
	s := NewServer(cli, "tok")
	for range 3 {
		if err := s.Stop(context.Background(), "appdb"); err != nil {
			t.Fatal(err)
		}
	}
	// Also through the API, as the preStop hook asks.
	req := httptest.NewRequest(http.MethodPost, "/v1/shutdown?database=appdb", nil)
	req.RemoteAddr = "127.0.0.1:4000"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("/v1/shutdown after a stop = %d, want 200", rr.Code)
	}
	if got := strings.Join(cli.recorded(), "; "); got != stopHeartbeat+"; "+stopServer {
		t.Errorf("commands = %q, want one heartbeat stop and one server stop", got)
	}
	if !s.StopIntended() {
		t.Error("the stop is not recorded as intended")
	}
}

// "instance-manager shutdown" is the one trigger of the preStop hook and of
// the entrypoint. It asks the running manager, so that the manager knows the
// stop is intended; with no manager listening it stops CUBRID itself.
func TestRequestShutdown(t *testing.T) {
	t.Run("a manager is listening", func(t *testing.T) {
		serverCLI, localCLI := &countingCLI{}, &countingCLI{}
		s := NewServer(serverCLI, "tok")
		// The request comes from loopback and needs no token.
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		if err := RequestShutdown(context.Background(), ts.URL, "appdb", localCLI, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if len(serverCLI.recorded()) != 2 || len(localCLI.recorded()) != 0 {
			t.Errorf("manager ran %q, the command itself ran %q", serverCLI.recorded(), localCLI.recorded())
		}
		if !s.StopIntended() {
			t.Error("the manager does not know that the stop is intended")
		}
	})
	t.Run("no manager is listening", func(t *testing.T) {
		localCLI := &countingCLI{}
		// A port nothing listens on.
		ts := httptest.NewServer(http.NotFoundHandler())
		url := ts.URL
		ts.Close()
		if err := RequestShutdown(context.Background(), url, "appdb", localCLI, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(localCLI.recorded(), "; "); got != stopHeartbeat+"; "+stopServer {
			t.Errorf("commands = %q, want the stop run by the command itself", got)
		}
	})
}
