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
	"sync/atomic"
	"testing"
	"time"
)

const (
	procMaster = "cub_master"
	procServer = "cub_server"
)

// dog returns a watchdog over a process list and a busy flag the test changes.
func dog(running *map[string]bool, busy *bool) *Watchdog {
	return &Watchdog{
		Required: []string{procMaster, procServer},
		Running:  func() (map[string]bool, error) { return *running, nil },
		Busy:     func() bool { return *busy },
		Grace:    30 * time.Second,
		Logger:   slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	}
}

func TestWatchdog(t *testing.T) {
	both := map[string]bool{procMaster: true, procServer: true}
	onlyMaster := map[string]bool{procMaster: true}
	none := map[string]bool{}
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	at := func(seconds int) time.Time { return start.Add(time.Duration(seconds) * time.Second) }

	t.Run("a member that never started is left alone", func(t *testing.T) {
		running, busy := none, false
		w := dog(&running, &busy)
		for s := 0; s <= 300; s += 5 {
			if w.check(at(s)) {
				t.Fatalf("ended after %d s although CUBRID never ran", s)
			}
		}
	})
	t.Run("a process gone for longer than the grace ends the member", func(t *testing.T) {
		running, busy := both, false
		w := dog(&running, &busy)
		if w.check(at(0)) {
			t.Fatal("ended while everything runs")
		}
		running = onlyMaster // the state after "cubrid heartbeat stop"
		if w.check(at(5)) || w.check(at(30)) {
			t.Fatal("ended within the grace")
		}
		if !w.check(at(36)) {
			t.Error("did not end after the grace")
		}
	})
	t.Run("a process that comes back in time is forgiven", func(t *testing.T) {
		running, busy := both, false
		w := dog(&running, &busy)
		w.check(at(0))
		running = none
		w.check(at(5))
		running = both
		w.check(at(20))
		running = none
		// The grace counts from the new absence, not from the first one.
		if w.check(at(40)) || w.check(at(65)) {
			t.Error("ended although the second absence is shorter than the grace")
		}
		if !w.check(at(71)) {
			t.Error("did not end after the second absence outlasted the grace")
		}
	})
	t.Run("an intended absence does not end the member, however long", func(t *testing.T) {
		running, busy := both, false
		w := dog(&running, &busy)
		w.check(at(0))
		running, busy = none, true
		for s := 5; s <= 600; s += 5 {
			if w.check(at(s)) {
				t.Fatalf("ended after %d s of an intended absence", s)
			}
		}
		// When it is no longer intended, the grace starts then.
		busy = false
		if w.check(at(605)) || w.check(at(630)) {
			t.Error("ended within the grace after the operation finished")
		}
		if !w.check(at(640)) {
			t.Error("did not end after the grace")
		}
	})
	t.Run("a process list that cannot be read is not an absence", func(t *testing.T) {
		running, busy := both, false
		w := dog(&running, &busy)
		w.check(at(0))
		w.Running = func() (map[string]bool, error) { return nil, errors.New("open /proc: permission denied") }
		for s := 5; s <= 300; s += 5 {
			if w.check(at(s)) {
				t.Fatalf("ended after %d s although the processes could not be read", s)
			}
		}
	})
}

func TestWatchdog_Run(t *testing.T) {
	var serverUp atomic.Bool
	serverUp.Store(true)
	newDog := func() *Watchdog {
		return &Watchdog{
			Required: []string{procMaster, procServer},
			Running: func() (map[string]bool, error) {
				return map[string]bool{procMaster: true, procServer: serverUp.Load()}, nil
			},
			Grace: 30 * time.Millisecond, Interval: 5 * time.Millisecond,
			Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		}
	}
	done := make(chan error, 1)
	go func() { done <- newDog().Run(t.Context()) }()
	time.Sleep(20 * time.Millisecond)
	serverUp.Store(false)
	select {
	case err := <-done:
		if !errors.Is(err, ErrProcessGone) {
			t.Errorf("Run returned %v, want ErrProcessGone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the server was gone")
	}

	// A cancelled context ends it without an error.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := newDog().Run(ctx2); err != nil {
		t.Errorf("Run after cancel returned %v", err)
	}
}

// A stop that was asked for, or an operation in progress, makes the absence
// of CUBRID's processes intended.
func TestServer_StopIntended(t *testing.T) {
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(fakeCLI{out: "ok"}, "tok").WithOperationStore(store)
	if s.StopIntended() {
		t.Fatal("intended before anything was asked")
	}
	op, _, err := store.FindOrCreate(OpBackup, "k", "h", "appdb")
	if err != nil {
		t.Fatal(err)
	}
	if !s.StopIntended() {
		t.Error("not intended while an operation is in progress")
	}
	if _, err := store.Update(op.ID, func(o *Operation) { o.State = OpCompleted }); err != nil {
		t.Fatal(err)
	}
	if s.StopIntended() {
		t.Error("still intended after the operation completed")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/shutdown?database=appdb", nil)
	req.RemoteAddr = "127.0.0.1:4000"
	req.Header.Set("Authorization", "Bearer tok")
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if !s.StopIntended() {
		t.Error("not intended after a shutdown was requested")
	}
}
