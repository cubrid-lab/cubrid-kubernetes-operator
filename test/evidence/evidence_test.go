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

package evidence

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	s00         = "S00"
	s03         = "S03"
	graceful    = "graceful"
	abrupt      = "abrupt"
	kind        = "kind"
	vmLab       = "needs the VM lab"
	s00Record   = "S00/record.json"
	s03History  = "S03/history.jsonl"
	notProven   = "fault_not_confirmed"
	testRunName = "run-1"
)

// runDir returns a run directory holding the given evidence files.
func runDir(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		path := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestScenarioName(t *testing.T) {
	if got := (Scenario{ID: s03, Variant: abrupt}).Name(); got != "S03/abrupt" {
		t.Errorf("name = %q, want S03/abrupt", got)
	}
	if got := (Scenario{ID: s00}).Name(); got != s00 {
		t.Errorf("name = %q, want S00", got)
	}
}

// A pass is only a pass when the fault was confirmed, nothing forbidden was
// seen and every listed evidence file exists.
func TestJudge(t *testing.T) {
	dir := runDir(t, s03History)
	tests := map[string]struct {
		in         Scenario
		want       Result
		wantReason string
	}{
		"pass with a confirmed fault and its evidence": {
			in:   Scenario{Result: Pass, FaultConfirmed: new(true), Evidence: []string{s03History}},
			want: Pass,
		},
		"pass without a fault to confirm": {
			in:   Scenario{Result: Pass, Evidence: []string{s03History}},
			want: Pass,
		},
		"pass with an unconfirmed fault": {
			in:         Scenario{Result: Pass, FaultConfirmed: new(false), Evidence: []string{s03History}},
			want:       Fail,
			wantReason: "fault was not confirmed",
		},
		"pass with a forbidden outcome": {
			in: Scenario{Result: Pass, NeverEvents: []string{"two masters reported healthy"},
				Evidence: []string{s03History}},
			want:       Fail,
			wantReason: "two masters reported healthy",
		},
		"pass with a missing evidence file": {
			in:         Scenario{Result: Pass, Evidence: []string{s03History, "S03/timeline.jsonl"}},
			want:       Fail,
			wantReason: "S03/timeline.jsonl",
		},
		"pass with no evidence listed": {
			in:         Scenario{Result: Pass},
			want:       Fail,
			wantReason: "no evidence",
		},
		"pass with evidence outside the run directory": {
			in:         Scenario{Result: Pass, Evidence: []string{"../outside.json"}},
			want:       Fail,
			wantReason: "../outside.json",
		},
		"a result that is not one of the five": {
			in:         Scenario{Result: "passed", Evidence: []string{s03History}},
			want:       Fail,
			wantReason: `"passed"`,
		},
		"blocked keeps its reason": {
			in:         Scenario{Result: Blocked, Reason: notProven},
			want:       Blocked,
			wantReason: notProven,
		},
		"not run without a reason says so": {
			in:         Scenario{Result: NotRun},
			want:       NotRun,
			wantReason: "no reason recorded",
		},
	}
	for name, tc := range tests {
		got, reason := tc.in.Judge(dir)
		if got != tc.want {
			t.Errorf("%s: result = %q, want %q", name, got, tc.want)
		}
		if !strings.Contains(reason, tc.wantReason) || (tc.wantReason == "" && reason != "") {
			t.Errorf("%s: reason = %q, want it to contain %q", name, reason, tc.wantReason)
		}
	}
}

// A lane passes only when each required scenario and variant has a result
// and every result of it is judged a pass. A spec that ends without a Gomega
// failure can still record any of these verdicts.
func TestGate(t *testing.T) {
	dir := runDir(t, s00Record, s03History)
	abruptReq := Requirement{ID: s03, Variant: abrupt}
	gracefulReq := Requirement{ID: s03, Variant: graceful}
	lane := Lane{Name: kind, Required: []Requirement{{ID: s00}, abruptReq, gracefulReq}}
	pass := func(id, variant string) Scenario {
		return Scenario{ID: id, Variant: variant, Result: Pass, Evidence: []string{s00Record}}
	}
	passing := []Scenario{pass(s00, ""), pass(s03, abrupt), pass(s03, graceful)}
	// with returns the passing scenarios with the one of the same name replaced.
	with := func(sc Scenario) []Scenario {
		out := []Scenario{sc}
		for _, p := range passing {
			if p.Name() != sc.Name() {
				out = append(out, p)
			}
		}
		return out
	}
	tests := map[string]struct {
		lane      Lane
		scenarios []Scenario
		// want is part of the single problem expected; "" means the lane passes.
		want string
	}{
		"every required scenario and variant passed": {lane, passing, ""},
		"over the time limit": {lane, with(Scenario{ID: s03, Variant: abrupt, Result: Fail,
			Reason: "failover_limit exceeded: 41s > 30s", Evidence: []string{s03History}}), "failover_limit exceeded"},
		"time limit unset": {lane, with(Scenario{ID: s03, Variant: abrupt, Result: Blocked,
			Reason: "time_limit_unset"}), "S03/abrupt: blocked: time_limit_unset"},
		"a required variant missing": {lane, passing[:2], "S03/graceful: missing"},
		"a required scenario absent": {lane, passing[1:], "S00: missing"},
		"a variant recorded in place of the scenario": {Lane{Required: []Requirement{{ID: s03}}},
			passing[1:], "S03: missing"},
		"not run": {lane, with(Scenario{ID: s00, Result: NotRun, Reason: "linux/arm64"}),
			"S00: not_run: linux/arm64"},
		"not applicable where the lane does not allow it": {lane,
			with(Scenario{ID: s00, Result: NotApplicable, Reason: vmLab}), "S00: not_applicable"},
		"not applicable where the lane allows it": {
			Lane{Required: []Requirement{{ID: s00, NotApplicable: true}, abruptReq, gracefulReq}},
			with(Scenario{ID: s00, Result: NotApplicable, Reason: vmLab}), ""},
		"a pass without its evidence": {lane, with(Scenario{ID: s00, Result: Pass}), "S00: fail: claimed a pass"},
		"a result that is not one of the five": {lane,
			with(Scenario{ID: s00, Result: "passed", Evidence: []string{s00Record}}), `S00: fail: result "passed"`},
		"one of two results of a variant failed": {lane,
			append(passing, Scenario{ID: s03, Variant: abrupt, Result: Fail, Reason: "x"}), "S03/abrupt: fail: x"},
		"a lane that requires nothing": {Lane{Name: "kind-filtered"}, nil, "no scenario recorded"},
		"a scenario the lane does not require failed": {lane,
			append(passing, Scenario{ID: "S07", Result: Fail, Reason: "x"}), "S07: fail: x"},
	}
	for name, tc := range tests {
		problems := tc.lane.Gate(Summary{Scenarios: tc.scenarios}, dir)
		switch {
		case tc.want == "" && len(problems) != 0:
			t.Errorf("%s: problems = %q, want none", name, problems)
		case tc.want != "" && (len(problems) != 1 || !strings.Contains(problems[0], tc.want)):
			t.Errorf("%s: problems = %q, want one containing %q", name, problems, tc.want)
		}
	}
}

// Conclude writes the verdict before it reports it, and a summary that could
// not be written fails the run.
func TestConclude(t *testing.T) {
	lane := Lane{Name: kind, Required: []Requirement{{ID: s00}, {ID: s03, Variant: graceful}}}
	passing := Summary{Scenarios: []Scenario{
		{ID: s00, Result: Pass, Evidence: []string{s00Record}},
		{ID: s03, Variant: graceful, Result: Pass, Evidence: []string{s00Record}},
	}}
	read := func(dir string) Summary {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got Summary
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	dir := runDir(t, s00Record)
	if err := passing.Conclude(dir, NewRedactor(), lane); err != nil {
		t.Errorf("a passing run: %v", err)
	}
	if got := read(dir); got.Lane != kind || !got.Passed || len(got.Problems) != 0 {
		t.Errorf("a passing run wrote lane=%q passed=%v problems=%q", got.Lane, got.Passed, got.Problems)
	}

	dir = runDir(t, s00Record)
	failing := Summary{Scenarios: passing.Scenarios[:1]}
	err := failing.Conclude(dir, NewRedactor(), lane)
	if err == nil || !strings.Contains(err.Error(), "S03/graceful: missing") {
		t.Errorf("a run missing a required variant: err = %v", err)
	}
	if got := read(dir); got.Passed || len(got.Problems) != 1 {
		t.Errorf("a failing run wrote passed=%v problems=%q, want its problem", got.Passed, got.Problems)
	}
	if _, err := os.Stat(filepath.Join(dir, "junit.xml")); err != nil {
		t.Errorf("a failing run must keep junit.xml: %v", err)
	}

	dir = runDir(t, s00Record)
	err = passing.Conclude(dir, NewRedactor(), lane, "S03/abrupt: no step of the suite carries the label S03-abrupt")
	if err == nil || !strings.Contains(err.Error(), "S03-abrupt") {
		t.Errorf("a run with another problem: err = %v", err)
	}
	if got := read(dir); got.Passed || len(got.Problems) != 1 {
		t.Errorf("a run with another problem wrote passed=%v problems=%q", got.Passed, got.Problems)
	}

	// A regular file where the run directory should be cannot hold the summary.
	unwritable := filepath.Join(runDir(t, s00Record), s00Record)
	if err := passing.Conclude(unwritable, NewRedactor(), lane); err == nil {
		t.Error("a summary that could not be written passed the run")
	}
	if err := passing.Conclude("", NewRedactor(), lane); err == nil {
		t.Error("a run without a run directory passed")
	}
}

func testSummary() Summary {
	return Summary{
		RunID:      testRunName,
		StartedAt:  time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 10, 12, 10, 5, 12, 0, time.UTC),
		Environment: Environment{
			Level: kind, KubernetesVersion: "v1.37.0", OperatorCommit: "abc1234",
			OperatorImageDigest: "sha256:aaa", CubridImageDigest: "sha256:bbb", EngineVersion: "11.4.6",
		},
		Scenarios: []Scenario{
			{ID: s00, Result: Pass, Evidence: []string{s00Record},
				Measurements: map[string]any{"recoveryTime": Unknown, "acknowledgedMissing": 0}},
			{ID: s03, Variant: abrupt, Result: Pass, FaultConfirmed: new(false), Evidence: []string{s00Record}},
			{ID: "S04", Result: NotApplicable, Reason: vmLab},
			{ID: "S07", Result: Blocked, Reason: notProven},
			{ID: "S10", Result: NotRun, Reason: "linux/arm64"},
		},
	}
}

func TestWriteSummaryJSON(t *testing.T) {
	dir := runDir(t, s00Record)
	summary := testSummary()
	summary.Notes = []string{"a step needed a second attempt"}
	if err := summary.Write(dir, NewRedactor()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Summary
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("summary.json: %v\n%s", err, data)
	}
	if got.SchemaVersion != SchemaVersion || got.RunID != testRunName || got.Environment.OperatorCommit != "abc1234" {
		t.Errorf("summary header = %+v", got)
	}
	// What was not recorded is unknown, not empty.
	if got.Environment.ClientDriver != Unknown {
		t.Errorf("clientDriver = %q, want %q", got.Environment.ClientDriver, Unknown)
	}
	if len(got.Scenarios) != 5 {
		t.Fatalf("scenarios = %d, want 5", len(got.Scenarios))
	}
	if got.Scenarios[0].Result != Pass || got.Scenarios[0].Measurements["recoveryTime"] != Unknown {
		t.Errorf("S00 = %+v", got.Scenarios[0])
	}
	// The file holds the judged result, so a reader cannot take the claim for it.
	if got.Scenarios[1].Result != Fail || !strings.Contains(got.Scenarios[1].Reason, "fault was not confirmed") {
		t.Errorf("S03 = %+v, want a fail with its reason", got.Scenarios[1])
	}
	if !strings.Contains(string(data), `"neverEvents": []`) {
		t.Errorf("neverEvents must be written as an empty list:\n%s", data)
	}
	if len(got.Notes) != 1 || got.Notes[0] != summary.Notes[0] {
		t.Errorf("notes = %q, want %q", got.Notes, summary.Notes)
	}
}

func TestWriteJUnit(t *testing.T) {
	dir := runDir(t, s00Record)
	if err := testSummary().Write(dir, NewRedactor()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "junit.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		Name     string `xml:"name,attr"`
		Tests    int    `xml:"tests,attr"`
		Failures int    `xml:"failures,attr"`
		Errors   int    `xml:"errors,attr"`
		Skipped  int    `xml:"skipped,attr"`
		Cases    []struct {
			Name    string `xml:"name,attr"`
			Failure *struct {
				Message string `xml:"message,attr"`
			} `xml:"failure"`
			Error *struct {
				Message string `xml:"message,attr"`
			} `xml:"error"`
			Skipped *struct {
				Message string `xml:"message,attr"`
			} `xml:"skipped"`
		} `xml:"testcase"`
	}
	if err := xml.Unmarshal(data, &suite); err != nil {
		t.Fatalf("junit.xml: %v\n%s", err, data)
	}
	if suite.Name != testRunName || suite.Tests != 5 || suite.Failures != 1 || suite.Errors != 1 || suite.Skipped != 2 {
		t.Errorf("suite = %+v", suite)
	}
	c := suite.Cases
	if len(c) != 5 {
		t.Fatalf("test cases = %d, want 5", len(c))
	}
	if c[0].Name != s00 || c[0].Failure != nil || c[0].Error != nil || c[0].Skipped != nil {
		t.Errorf("pass must have no child element: %+v", c[0])
	}
	if c[1].Name != "S03/abrupt" || c[1].Failure == nil {
		t.Errorf("fail must be a failure: %+v", c[1])
	}
	if c[2].Skipped == nil || c[2].Skipped.Message != "not_applicable: needs the VM lab" {
		t.Errorf("not applicable must be skipped with its reason: %+v", c[2])
	}
	// Blocked is an error, so that an unproven scenario is not shown as green.
	if c[3].Error == nil || c[3].Error.Message != "blocked: "+notProven || c[3].Skipped != nil {
		t.Errorf("blocked must be an error: %+v", c[3])
	}
	if c[4].Skipped == nil || c[4].Skipped.Message != "not_run: linux/arm64" {
		t.Errorf("not run must be skipped with its reason: %+v", c[4])
	}
}

func TestRedact(t *testing.T) {
	r := NewRedactor("s3cr3t-token-value", "", "ab")
	tests := map[string]struct{ in, gone, kept string }{
		"a value that was registered": {
			in: "calling with s3cr3t-token-value now", gone: "s3cr3t-token-value", kept: "calling with [REDACTED] now"},
		"a bearer header": {
			in: "Authorization: Bearer abc.def-123\nnext", gone: "abc.def-123", kept: "Authorization: Bearer [REDACTED]\nnext"},
		"an environment variable": {
			in: "IM_TOKEN=tok123 OTHER=1", gone: "tok123", kept: "OTHER=1"},
		"a JSON field": {
			in: `{"password": "hunter2", "name": "cc"}`, gone: "hunter2", kept: `"name": "cc"`},
		"a YAML field": {
			in: "secretAccessKey: wJalrXUtnFEMI\nbucket: backups", gone: "wJalrXUtnFEMI", kept: "bucket: backups"},
		"an access key id anywhere": {
			in: "used AKIAIOSFODNN7EXAMPLE for upload", gone: "AKIAIOSFODNN7EXAMPLE", kept: "for upload"},
		"a URL with credentials": {
			in: "s3 endpoint http://user:pw12345@s3mock:9090/bucket", gone: "pw12345", kept: "@s3mock:9090/bucket"},
	}
	for name, tc := range tests {
		got := string(r.Redact([]byte(tc.in)))
		if strings.Contains(got, tc.gone) {
			t.Errorf("%s: %q still contains %q", name, got, tc.gone)
		}
		if !strings.Contains(got, tc.kept) {
			t.Errorf("%s: %q lost %q", name, got, tc.kept)
		}
	}
	// A reference to a Secret is a name, not a secret, and a short registered
	// value must not blank ordinary text.
	plain := "credentialsSecretRef:\n  name: backup-credentials\nabout a table\n"
	if got := string(r.Redact([]byte(plain))); got != plain {
		t.Errorf("redaction changed text without credentials:\n%s", got)
	}
}

// A value learned during the run, a cluster's generated token for example, is
// removed from everything written after it was added.
func TestRedactAdded(t *testing.T) {
	r := NewRedactor()
	r.Add("generated-cluster-token", "ab")
	got := string(r.Redact([]byte("calling with generated-cluster-token, about a table")))
	if got != "calling with [REDACTED], about a table" {
		t.Errorf("got %q", got)
	}
}

func TestWriteRedacts(t *testing.T) {
	dir := runDir(t, s00Record)
	r := NewRedactor("s3cr3t-token-value")
	s := testSummary()
	s.Scenarios[3].Reason = "request with s3cr3t-token-value was refused"
	if err := s.Write(dir, r); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile(dir, "S07/logs/manager.log", []byte("token=s3cr3t-token-value\n")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"summary.json", "junit.xml", "S07/logs/manager.log"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "s3cr3t-token-value") {
			t.Errorf("%s contains the secret:\n%s", f, data)
		}
	}
	if err := r.WriteFile(dir, "../escape.log", []byte("x")); err == nil {
		t.Error("a path outside the run directory was accepted")
	}
}
