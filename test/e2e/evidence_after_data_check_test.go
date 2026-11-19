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
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const keptHistory = "{\"opId\":\"keep-000001\",\"event\":\"acknowledged\"}\n"

// readKept returns the content of a file of the evidence directory, or "" and
// false when it was not written.
func readKept(t *testing.T, dir, rel string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data), true
}

// When the data check has passed but a later assertion of the scenario fails,
// such as the applier's fail count, the history and the passed report must
// still be stored: the step fails before it reaches its own write, and
// without them the failure cannot be placed in time against the logs (#254).
func TestEvidenceIsKeptWhenAnAssertionAfterThePassedDataCheckFails(t *testing.T) {
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	r := &haRun{history: keptHistory}
	report := workload.Report{OK: true, Members: map[string]workload.MemberCheck{"hab-0": {Rows: 1}}}

	failedAfterTheCheck := func() (failed bool) {
		defer func() { failed = recover() != nil }()
		store := r.evidenceOnce("S05/broker", report)
		defer store()
		panic("fail count of the applier on hab-1: 1")
	}
	if !failedAfterTheCheck() {
		t.Fatal("the simulated assertion did not fail the step")
	}

	if got, ok := readKept(t, dir, "S05/broker/history.jsonl"); !ok || got != keptHistory {
		t.Errorf("history.jsonl = %q (written %t), want the client's history", got, ok)
	}
	got, ok := readKept(t, dir, "S05/broker/data-check.json")
	if !ok {
		t.Fatal("data-check.json was not kept when a later assertion failed")
	}
	var saved workload.Report
	if err := json.Unmarshal([]byte(got), &saved); err != nil {
		t.Fatalf("bad saved data report: %v", err)
	}
	if !saved.OK || saved.Members["hab-0"].Rows != 1 {
		t.Errorf("saved data report = %+v, want the report of the passed check", saved)
	}
}

// On a step that passes, the files are written once, where the step asks for
// them, and the deferred call neither writes them again nor changes them.
func TestEvidenceIsWrittenOnceWhenTheStepPasses(t *testing.T) {
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	r := &haRun{history: keptHistory}
	report := workload.Report{OK: true, Members: map[string]workload.MemberCheck{"hab-0": {Rows: 1}}}

	var files []string
	func() {
		store := r.evidenceOnce("S01", report)
		defer store()
		files = store()
		r.history += "{\"opId\":\"keep-000002\",\"event\":\"acknowledged\"}\n"
	}()

	if want := []string{"S01/history.jsonl", "S01/data-check.json"}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
	if got, _ := readKept(t, dir, "S01/history.jsonl"); got != keptHistory {
		t.Errorf("history.jsonl was written again by the deferred call: %q", got)
	}
}

// A report that no data check returned is still not stored by the deferred
// write, so it cannot read as a check that found no rows.
func TestEvidenceKeptAfterAFailureHasNoEmptyReport(t *testing.T) {
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	r := &haRun{history: keptHistory}

	func() {
		defer func() { _ = recover() }()
		store := r.evidenceOnce("S14", workload.Report{})
		defer store()
		panic("refused requests in the log of hab-0")
	}()

	if _, ok := readKept(t, dir, "S14/history.jsonl"); !ok {
		t.Error("history.jsonl was not kept")
	}
	if _, ok := readKept(t, dir, "S14/data-check.json"); ok {
		t.Error("an empty report was stored as data-check.json")
	}
}
