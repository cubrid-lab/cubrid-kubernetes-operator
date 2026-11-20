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

package installed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
)

// failFirstSQL makes the smoke's first statement print the pair, and answers
// the reads of the failure state with stateRead. The script of a read is the
// argument before the database name.
func failFirstSQL(t *testing.T, f *fixture, stateRead func(script string) (string, error)) {
	t.Helper()
	f.commandHook = func(_ context.Context, args []string) (string, error) {
		script := args[len(args)-2]
		switch {
		case strings.HasPrefix(args[len(args)-1], "CREATE TABLE"):
			return "ERROR *** -199 Server no longer responding\n ERROR *** -111 transaction aborted\n",
				errors.New("exit status 1")
		case script == sqlcheck.ServerAgeScript || script == sqlcheck.ServerLogTailScript:
			return stateRead(script)
		}
		return f.answer(t, args)
	}
}

func TestRunRecordsTheServerStateWhenCSQLLosesItsSession(t *testing.T) {
	f := newFixture(t)
	failFirstSQL(t, f, func(script string) (string, error) {
		if script == sqlcheck.ServerAgeScript {
			return "now 100.5\nuptime 3.2\ncub_server 77 123\nha_mode ?\nclk_tck 100\n", nil
		}
		return "Time: 11/18/26 10:00:00.000 - last line\n", nil
	})
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("a lost csql session passed")
	}
	if f.deletes != 1 || readSummary(t, f.cfg.EvidenceDir).Passed {
		t.Fatal("the lost session changed cleanup or the verdict")
	}
	state := readRecord(t, f.cfg.EvidenceDir).SQLFailureState
	if state == nil || state.Step != 0 || state.ReadAt.IsZero() || state.ReadError != "" {
		t.Fatalf("state = %+v", state)
	}
	if state.RestartCount == nil || state.RestartCountAtStart == nil || state.ContainerID == "" ||
		state.ContainerID != state.ContainerIDAtStart {
		t.Errorf("the Pod's restart count and container ID were not compared: %+v", state)
	}
	if len(state.ServerAtPair) != 5 || state.ServerAtPair[2] != "cub_server 77 123" ||
		len(state.ServerLogTail) != 1 {
		t.Errorf("server state = %q, log = %q", state.ServerAtPair, state.ServerLogTail)
	}
}

func TestRunRecordsNoServerStateWhenNoSQLFails(t *testing.T) {
	f := newFixture(t)
	if err := f.runner.Run(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	if state := readRecord(t, f.cfg.EvidenceDir).SQLFailureState; state != nil {
		t.Fatalf("state = %+v", state)
	}
}

func TestFailedServerStateReadIsOnlyRecorded(t *testing.T) {
	f := newFixture(t)
	failFirstSQL(t, f, func(string) (string, error) { return "", errors.New("exec failed") })
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("a lost csql session passed")
	}
	state := readRecord(t, f.cfg.EvidenceDir).SQLFailureState
	if state == nil || !strings.Contains(state.ReadError, "identity: exec failed") ||
		!strings.Contains(state.ReadError, "server log: exec failed") || f.deletes != 1 {
		t.Fatalf("state = %+v, deletes = %d", state, f.deletes)
	}
}

func TestServerStateIsBoundedAndRedacted(t *testing.T) {
	const secret = "private-token-value"
	f := newFixture(t)
	f.runner.Redactor = evidence.NewRedactor(secret)
	failFirstSQL(t, f, func(script string) (string, error) {
		if script == sqlcheck.ServerAgeScript {
			return "", nil
		}
		var log strings.Builder
		for i := range 3 * sqlcheck.ServerLogTailLines {
			fmt.Fprintf(&log, "line %d %s %s\n", i, secret, strings.Repeat("x", 2*sqlcheck.ServerLogLineWidth))
		}
		return log.String(), nil
	})
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("a lost csql session passed")
	}
	data, err := os.ReadFile(filepath.Join(f.cfg.EvidenceDir, recordFile))
	if err != nil || strings.Contains(string(data), secret) {
		t.Fatalf("the record leaked the secret or was missing: %v", err)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	tail := rec.SQLFailureState.ServerLogTail
	if len(tail) != sqlcheck.ServerLogTailLines || !strings.HasPrefix(tail[len(tail)-1], "line 119 ") {
		t.Fatalf("%d lines, last %q", len(tail), tail[len(tail)-1])
	}
	for _, line := range tail {
		if len(line) > sqlcheck.ServerLogLineWidth {
			t.Fatalf("line of %d characters", len(line))
		}
	}
}

func TestServerStateReadsEndWithinTheirBounds(t *testing.T) {
	if total := podReadTimeout + serverAgeTimeout + serverLogTailTimeout; total > 20*time.Second {
		t.Fatalf("the capture can take %s", total)
	}
}
