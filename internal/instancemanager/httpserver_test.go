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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func serveTransport(t *testing.T, server *http.Server) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("HTTP server ended: %v", err)
		}
	})
	return listener.Addr().String()
}

func dialTransport(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// A small valid header followed by an incomplete body must expire before it
// can admit an operation or create a CUBRID command or staging directory.
func TestHTTPServer_ExpiresAStalledRequestBodyBeforeWork(t *testing.T) {
	cli := &stepCLI{}
	storeDir, staging := t.TempDir(), t.TempDir()
	store, err := NewOperationStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(cli, "tok").WithOperationStore(store).WithBackupStagingRoot(staging).HTTPServer("")
	// Keep the production policy, shortened so the regression is quick. A
	// missing timeout remains zero and fails by the client's watchdog.
	server.ReadTimeout /= 100
	conn := dialTransport(t, serveTransport(t, server))
	if _, err := fmt.Fprint(conn, "POST /v1/backup HTTP/1.1\r\nHost: member\r\nAuthorization: Bearer tok\r\nContent-Length: 1024\r\n\r\n{\"database\":\"appdb\""); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("server did not expire the incomplete request body before the client watchdog: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("incomplete request = %d, want 400", response.StatusCode)
	}
	if calls := cli.recorded(); len(calls) != 0 {
		t.Errorf("incomplete request ran commands: %v", calls)
	}
	for _, dir := range []string{storeDir, staging} {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("incomplete request changed %s: %v, %v", dir, entries, err)
		}
	}
}

type slowBackupCLI struct{ delay time.Duration }

func (c slowBackupCLI) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	select {
	case <-time.After(c.delay):
		return "ok", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// The read deadline limits incoming bytes, not a valid synchronous backup's
// execution budget. A complete request can still return after that deadline.
func TestHTTPServer_PreservesALongSynchronousBackupResponse(t *testing.T) {
	staging := t.TempDir()
	server := NewServer(slowBackupCLI{delay: 200 * time.Millisecond}, "tok").
		WithBackupStagingRoot(staging).WithTimeouts(Timeouts{Backup: time.Second}).HTTPServer("")
	server.ReadTimeout /= 100
	addr := serveTransport(t, server)
	body, err := json.Marshal(BackupRequest{Database: dbName, Destination: filepath.Join(staging, "backup")})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+pathBackup, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("valid synchronous backup lost its response: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Errorf("backup = %d (%s), want 200", response.StatusCode, data)
	}
}

func TestHTTPServer_ExpiresIdleConnections(t *testing.T) {
	server := NewServer(fakeCLI{}, "tok").HTTPServer("")
	server.IdleTimeout /= 100
	conn := dialTransport(t, serveTransport(t, server))
	if _, err := fmt.Fprint(conn, "GET /livez HTTP/1.1\r\nHost: member\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("idle connection was not closed before the client watchdog: %v", err)
	}
}

func TestHTTPServer_ExpiresResponseWrites(t *testing.T) {
	server := NewServer(fakeCLI{}, "tok").WithTimeouts(Timeouts{Backup: time.Second, Shutdown: time.Second}).HTTPServer("")
	server.WriteTimeout /= 250
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "late response")
	})
	addr := serveTransport(t, server)
	response, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("server returned a response after its write budget should have expired")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("response was ended by the client watchdog instead of the server write limit: %v", err)
	}
}

// Transport writes must cover body reading and the longest synchronous
// backup/shutdown, including when configured beyond the defaults.
func TestHTTPServer_BudgetsCoverSynchronousOperations(t *testing.T) {
	for _, budgets := range []Timeouts{{}, {Backup: 3 * time.Hour}, {Shutdown: 4 * time.Hour}} {
		api := NewServer(fakeCLI{}, "tok").WithTimeouts(budgets)
		server := api.HTTPServer("")
		if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.IdleTimeout <= 0 {
			t.Errorf("HTTP server has unbounded reads or idle connections: %+v", server)
		}
		longest := max(api.timeouts.Backup, api.timeouts.Shutdown)
		if server.WriteTimeout <= longest+server.ReadTimeout {
			t.Errorf("write timeout %s does not cover body reading plus operation %s", server.WriteTimeout, longest)
		}
	}
	const longestDuration = time.Duration(1<<63 - 1)
	server := NewServer(fakeCLI{}, "tok").WithTimeouts(Timeouts{Backup: longestDuration}).HTTPServer("")
	if server.WriteTimeout != longestDuration {
		t.Errorf("extreme backup budget overflowed the write limit: %s", server.WriteTimeout)
	}
}
