//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

func withManagerMetrics(t *testing.T, read func(context.Context) (string, error)) {
	t.Helper()
	previous := readManagerMetrics
	readManagerMetrics = read
	t.Cleanup(func() { readManagerMetrics = previous })
}

func recordedScenario(t *testing.T, name string) evidence.Scenario {
	t.Helper()
	for _, s := range runSummary.Scenarios {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("%s is not in %+v", name, runSummary.Scenarios)
	return evidence.Scenario{}
}

// Each recorded scenario keeps the manager's memory in its measurements and
// in <scenario>/manager-memory.txt, and a scenario that did not run keeps none.
func TestScenarioKeepsTheManagersMemory(t *testing.T) {
	startRunState(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	withManagerMetrics(t, func(context.Context) (string, error) {
		return "go_goroutines 9\nprocess_resident_memory_bytes 5e+07\ngo_memstats_heap_inuse_bytes 2e+07\n" +
			"controller_runtime_reconcile_total{controller=\"x\"} 1\n", nil
	})
	recordScenario(evidence.Scenario{ID: "S03", Variant: "abrupt", Result: evidence.Pass})
	recordScenario(evidence.Scenario{ID: "S06", Variant: "restart", Result: evidence.NotRun, Reason: "not reached"})

	got := recordedScenario(t, "S03/abrupt")
	if got.Measurements["managerRSSBytes"] != 5e7 || got.Measurements["managerHeapInuseBytes"] != 2e7 {
		t.Errorf("measurements %v", got.Measurements)
	}
	if got.Measurements["managerSysBytes"] != evidence.Unknown {
		t.Errorf("a series the answer lacks is %v, want unknown", got.Measurements["managerSysBytes"])
	}
	data, err := os.ReadFile(filepath.Join(dir, "S03", "abrupt", "manager-memory.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "controller_runtime") || !strings.Contains(string(data), "read at ") {
		t.Errorf("manager-memory.txt:\n%s", data)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "S03/abrupt/manager-memory.txt") {
		t.Errorf("evidence %v", got.Evidence)
	}
	if _, ok := recordedScenario(t, "S06/restart").Measurements["managerRSSBytes"]; ok {
		t.Error("a scenario that did not run has a memory measurement")
	}
}

// A read that fails is recorded as failed and measures unknown, never zero.
func TestScenarioRecordsAFailedManagerMemoryRead(t *testing.T) {
	startRunState(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	withManagerMetrics(t, func(context.Context) (string, error) { return "", errors.New("exec refused") })
	recordScenario(evidence.Scenario{ID: "S01", Result: evidence.Pass})

	got := recordedScenario(t, "S01")
	for k, v := range evidence.UnreadManagerMemoryMeasurements() {
		if got.Measurements[k] != v {
			t.Errorf("%s = %v, want %v", k, got.Measurements[k], v)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "S01", "manager-memory.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "not read: exec refused") {
		t.Errorf("manager-memory.txt:\n%s", data)
	}
}
