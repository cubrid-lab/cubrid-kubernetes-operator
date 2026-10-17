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

package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// "instance-manager shutdown" records the stop before it asks the manager.
// A manager that does not listen yet, and would start CUBRID once it does,
// finds the record; one that listens already receives the request after the
// record is in place (#323).
func TestShutdown_RecordsTheStopBeforeItAsksTheManager(t *testing.T) {
	stopFile := filepath.Join(t.TempDir(), "stop-requested")
	recordedFirst := make(chan bool, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := os.Stat(stopFile)
		recordedFirst <- err == nil
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	_, port, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IM_ADDR", ":"+port)
	t.Setenv("IM_TOKEN", "tok")
	t.Setenv("IM_STOP_FILE", stopFile)

	if err := shutdown(slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case ok := <-recordedFirst:
		if !ok {
			t.Error("the manager was asked before the stop was recorded")
		}
	default:
		t.Fatal("the manager was not asked")
	}
	if _, err := os.Stat(stopFile); err != nil {
		t.Errorf("the stop was not recorded: %v", err)
	}
}
