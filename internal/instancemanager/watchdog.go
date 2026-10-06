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
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrProcessGone is why the Instance Manager ends itself: a CUBRID process
// the member needs is gone and nobody asked for that. The container ends
// with the manager, and Kubernetes starts it again (ADR-0003).
var ErrProcessGone = errors.New("a CUBRID process this member needs is gone")

// Watchdog watches the CUBRID processes of a member. It restarts nothing: a
// process that is started again in place can leave the member in a state a
// new container does not have (docs/poc/RESULTS.md, POC-7 and POC-21).
type Watchdog struct {
	// Required are the process names that must all be running.
	Required []string
	// Running reports the names of the processes that run now.
	Running func() (map[string]bool, error)
	// Busy reports that their absence is intended at the moment: a stop was
	// requested, or an operation that stops and starts CUBRID is running.
	Busy func() bool
	// Grace is how long a required process may be missing.
	Grace time.Duration
	// Interval is the time between two checks.
	Interval time.Duration
	Logger   *slog.Logger

	armed        bool
	missingSince time.Time
}

// check looks once and reports whether the member has to end. It starts to
// care only after it has seen every required process running: a member that
// has not been started yet, for example one that waits to be seeded, is left
// alone.
func (w *Watchdog) check(now time.Time) bool {
	running, err := w.Running()
	if err != nil {
		// Not being able to look is not the same as seeing nothing.
		w.Logger.Debug("Could not read the process list", "err", err)
		return false
	}
	missing := ""
	for _, name := range w.Required {
		if !running[name] {
			missing = name
			break
		}
	}
	switch {
	case missing == "":
		if !w.armed {
			w.armed = true
			w.Logger.Info("Watching the CUBRID processes", "event", "process_watch_started", "processes", w.Required)
		}
		w.missingSince = time.Time{}
		return false
	case !w.armed:
		return false
	case w.Busy != nil && w.Busy():
		w.missingSince = time.Time{}
		return false
	case w.missingSince.IsZero():
		w.missingSince = now
		w.Logger.Warn("A CUBRID process is not running", "event", "process_missing", "process", missing,
			"graceSeconds", int(w.Grace.Seconds()))
		return false
	case now.Sub(w.missingSince) > w.Grace:
		w.Logger.Error("A CUBRID process is gone; ending the Instance Manager so that the container is restarted",
			"event", "process_gone", "process", missing, "missingSeconds", int(now.Sub(w.missingSince).Seconds()))
		return true
	}
	return false
}

// Run checks until ctx ends, and returns ErrProcessGone when a required
// process has been missing for longer than Grace.
func (w *Watchdog) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if w.check(now) {
				return ErrProcessGone
			}
		}
	}
}

// RunningProcesses lists the names of the processes of this container, from
// /proc. The image has no ps.
func RunningProcesses() (map[string]bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	running := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		// A process may end between the listing and the read.
		if comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm")); err == nil {
			running[strings.TrimSpace(string(comm))] = true
		}
	}
	return running, nil
}

// StopIntended reports whether CUBRID's processes may be absent now: a stop
// was requested through this manager, or one of its operations is running.
func (s *Server) StopIntended() bool {
	if s.stopRequested.Load() {
		return true
	}
	return s.store != nil && s.store.AnyInProgress()
}
