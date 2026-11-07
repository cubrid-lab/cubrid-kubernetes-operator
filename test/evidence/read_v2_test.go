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
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	abruptKey      = "ha-primary-pod-recovery-abrupt-later-master"
	gracefulKey    = "ha-primary-pod-recovery-graceful"
	s03Graceful    = "graceful"
	unknownField   = "unknown field"
	labRestartKey  = "installed-operator-failure-recovery-restart"
	kindRestartKey = "operator-failure-recovery-restart"
)

// frozenSummary decodes the summary.json of a directory with its measurements
// kept exact, as a version 1 reader of the Go type sees it.
func frozenSummary(t *testing.T, dir string) Summary {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, summaryFile))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var summary Summary
	if err := dec.Decode(&summary); err != nil {
		t.Fatal(err)
	}
	return summary
}

func withoutIdentity(summary map[string]any) map[string]any {
	stripped := maps.Clone(summary)
	recorded := summary[fieldScenarios].([]any)
	scenarios := make([]any, 0, len(recorded))
	for _, item := range recorded {
		sc := maps.Clone(item.(map[string]any))
		delete(sc, fieldKey)
		delete(sc, fieldDisplayName)
		scenarios = append(scenarios, sc)
	}
	stripped[fieldScenarios] = scenarios
	return stripped
}

// A version 2 bundle resolves as a version 1 bundle does: the key it recorded
// matches the one the registry gives its legacy name under its own lane and
// level, so S06/restart stays two results.
func TestReadRunResolvesAVersion2Bundle(t *testing.T) {
	tests := map[string]struct{ key, name string }{
		kindRun:       {kindRestartKey, "Operator interruption and HA continuity - restart"},
		labRestartRun: {labRestartKey, "Operator interruption and HA continuity - restart of the installed manager"},
	}
	for fixture, tc := range tests {
		t.Run(fixture, func(t *testing.T) {
			run, err := ReadRun(copyRunOf(t, version2, fixture))
			if err != nil {
				t.Fatal(err)
			}
			if !run.Validated() {
				t.Fatalf("problems = %q, want none", run.Problems)
			}
			if run.Summary.SchemaVersion != schemaV2 {
				t.Errorf("schema version = %s", run.Summary.SchemaVersion)
			}
			got := resultOf(t, run, testAlias)
			if got.Key != tc.key || got.RecordedKey != tc.key || got.DisplayName != tc.name || got.Judged != Pass {
				t.Errorf("S06/restart = %+v, want key %s named %q and a pass", got, tc.key, tc.name)
			}
		})
	}
}

// One run read at either version is the same run: the same record, verdicts and
// evidence paths under the same key and name, so a later comparison has one
// identity for both.
func TestReadRunReadsTheTwoVersionsOfARunTheSame(t *testing.T) {
	for _, fixture := range []string{kindRun, labRestartRun} {
		t.Run(fixture, func(t *testing.T) {
			before, err := ReadRun(copyRunOf(t, version1, fixture))
			if err != nil {
				t.Fatal(err)
			}
			after, err := ReadRun(copyRunOf(t, version2, fixture))
			if err != nil {
				t.Fatal(err)
			}
			if !before.Validated() || !after.Validated() {
				t.Fatalf("problems: version 1 %q, version 2 %q", before.Problems, after.Problems)
			}
			if a, b := before.Summary, after.Summary; a.RunID != b.RunID || !a.StartedAt.Equal(b.StartedAt) ||
				!a.FinishedAt.Equal(b.FinishedAt) || a.Environment != b.Environment || a.Lane != b.Lane || a.Passed != b.Passed {
				t.Errorf("the run differs:\n%+v\n%+v", a, b)
			}
			if len(before.Results) != len(after.Results) {
				t.Fatalf("%d and %d results", len(before.Results), len(after.Results))
			}
			for i, a := range before.Results {
				b := after.Results[i]
				if !reflect.DeepEqual(a.Scenario, b.Scenario) {
					t.Errorf("the record of %s differs:\n%+v\n%+v", a.Scenario.Name(), a.Scenario, b.Scenario)
				}
				if a.Key == "" || a.Key != b.Key || a.DisplayName != b.DisplayName || a.Judged != b.Judged || a.Reason != b.Reason {
					t.Errorf("%s reads differently:\n%+v\n%+v", a.Scenario.Name(), a, b)
				}
				if a.RecordedKey != "" {
					t.Errorf("version 1 recorded no key, but the read says %s", a.RecordedKey)
				}
			}
		})
	}
}

// Everything a version 2 file recorded survives the read, in Summary or beside
// it, and nothing in the run directory changes.
func TestReadRunKeepsAVersion2RecordIntactAndChangesNothing(t *testing.T) {
	dir := copyRunOf(t, version2, kindRun)
	before := snapshot(t, dir)
	run, err := ReadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(snapshot(t, dir), before) {
		t.Error("reading a run changed its directory")
	}
	read, err := json.Marshal(run.Summary)
	if err != nil {
		t.Fatal(err)
	}
	file := loadSummary(t, dir)
	if got, want := decodeJSON(t, read), withoutIdentity(file); !reflect.DeepEqual(got, want) {
		t.Errorf("the read summary differs from the file:\n got %v\nwant %v", got, want)
	}
	for i, item := range file[fieldScenarios].([]any) {
		sc, result := item.(map[string]any), run.Results[i]
		if result.RecordedKey != sc[fieldKey] || result.DisplayName != sc[fieldDisplayName] {
			t.Errorf("result %d = %+v, want the key and name of %v", i, result, sc)
		}
	}
	if run.SummarySHA256 != digest(t, filepath.Join(dir, summaryFile)) ||
		run.JUnitSHA256 != digest(t, filepath.Join(dir, junitFile)) {
		t.Error("the digests do not name the raw files")
	}
}

// Version 2 adds a key and a display name and nothing else: stripped of them,
// the summary is the frozen version 1 file, and junit.xml is the same bytes, so
// a JUnit case keeps its identity across the two versions.
func TestWriterAddsOnlyTheIdentityToTheVersion1Record(t *testing.T) {
	for _, fixture := range []string{kindRun, labRestartRun} {
		t.Run(fixture, func(t *testing.T) {
			dir := copyRun(t, fixture)
			frozen, frozenJUnit := loadSummary(t, dir), snapshot(t, dir)[filepath.Join(dir, junitFile)]
			if err := frozenSummary(t, dir).Write(dir, NewRedactor()); err != nil {
				t.Fatal(err)
			}
			written := loadSummary(t, dir)
			for _, item := range written[fieldScenarios].([]any) {
				if sc := item.(map[string]any); sc[fieldKey] == nil || sc[fieldDisplayName] == nil {
					t.Errorf("a result has no key or display name: %v", sc)
				}
			}
			if written[fieldSchemaVersion] != schemaV2 {
				t.Errorf("schema version = %v", written[fieldSchemaVersion])
			}
			stripped := withoutIdentity(written)
			stripped[fieldSchemaVersion] = schemaV1
			if !reflect.DeepEqual(stripped, frozen) {
				t.Errorf("version 2 changed the version 1 record:\n got %v\nwant %v", stripped, frozen)
			}
			if got := snapshot(t, dir)[filepath.Join(dir, junitFile)]; got != frozenJUnit {
				t.Errorf("junit.xml changed:\n got %s\nwant %s", got, frozenJUnit)
			}
		})
	}
}

// Every registered result is written with its own key and name under each lane
// that requires it. The redactor sees these words as well, and must not take
// one of them for a credential.
func TestWriterRecordsTheRegisteredIdentityOfEveryLane(t *testing.T) {
	for _, lane := range registry.lanes {
		t.Run(lane.Name, func(t *testing.T) {
			dir := t.TempDir()
			summary := Summary{RunID: testRunName, Environment: Environment{Level: lane.Level}, Lane: lane.Name}
			var want []ResolvedScenario
			for _, key := range lane.Keys {
				entry, err := registry.resolveKey(key)
				if err != nil {
					t.Fatal(err)
				}
				id, variant, _ := strings.Cut(entry.Aliases[0], "/")
				summary.Scenarios = append(summary.Scenarios,
					Scenario{ID: id, Variant: variant, Result: NotRun, Reason: testReason})
				want = append(want, entry)
			}
			if err := summary.Write(dir, NewRedactor()); err != nil {
				t.Fatal(err)
			}
			for i, item := range loadSummary(t, dir)[fieldScenarios].([]any) {
				if sc := item.(map[string]any); sc[fieldKey] != want[i].Key || sc[fieldDisplayName] != want[i].Name {
					t.Errorf("result %d = %v, want %s named %q", i, sc, want[i].Key, want[i].Name)
				}
			}
		})
	}
}

// A result the registry does not hold in the run's lane and level gets no key
// from the writer, whatever its name says at another level: a lab lane recorded
// at the kind level, as the real run of #489 was, keeps its recorded verdict and
// is not validated.
func TestWriterRecordsNoIdentityForAResultTheRegistryDoesNotHold(t *testing.T) {
	dir := copyRun(t, labSmokeRun)
	if err := frozenSummary(t, dir).Write(dir, NewRedactor()); err != nil {
		t.Fatal(err)
	}
	for _, item := range loadSummary(t, dir)[fieldScenarios].([]any) {
		sc := item.(map[string]any)
		if _, ok := sc[fieldKey]; ok {
			t.Errorf("a result of a lab lane at the kind level has a key: %v", sc)
		}
		if _, ok := sc[fieldDisplayName]; ok {
			t.Errorf("a result of a lab lane at the kind level has a display name: %v", sc)
		}
	}
	run, err := ReadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if run.Validated() || !run.Summary.Passed || resultOf(t, run, lab00).Key != "" {
		t.Errorf("validated = %v, recorded pass = %v, result = %+v",
			run.Validated(), run.Summary.Passed, resultOf(t, run, lab00))
	}

	dir = runDir(t, s00Record)
	unknown := Summary{RunID: testRunName, Environment: Environment{Level: LevelKind}, Lane: kindLaneName,
		Scenarios: []Scenario{
			{ID: s03, Variant: testUnknown, Result: Pass, Evidence: []string{s00Record}},
			{ID: s03Abrupt, Result: Pass, Evidence: []string{s00Record}},
		}}
	if err := unknown.Write(dir, NewRedactor()); err != nil {
		t.Fatal(err)
	}
	for _, item := range loadSummary(t, dir)[fieldScenarios].([]any) {
		if sc := item.(map[string]any); sc[fieldKey] != nil {
			t.Errorf("an unknown or misrecorded legacy name has a key: %v", sc)
		}
	}
}

// inScenario is a change to one result of a version 2 bundle.
func inScenario(id, variant string, change func(scenario map[string]any)) bundleChange {
	return func(t *testing.T, dir string) {
		summary := loadSummary(t, dir)
		change(scenarioNamed(t, summary, id, variant))
		saveSummary(t, dir, summary)
	}
}

func abruptKeyIs(key string) bundleChange {
	return inScenario(s03, abrupt, func(scenario map[string]any) { scenario[fieldKey] = key })
}

// A key is a claim of the file, and the registry decides it under the run's own
// lane and level. A result whose key does not hold keeps what it recorded,
// counts for no requirement, and the run is not validated, however it passed.
func TestReadRunNeverValidatesAVersion2ResultWhoseIdentityDoesNotHold(t *testing.T) {
	tests := map[string]struct {
		change bundleChange
		want   string
	}{
		"no key": {inScenario(s03, abrupt, func(scenario map[string]any) { delete(scenario, fieldKey) }),
			"S03/abrupt records no key"},
		"an empty key": {abruptKeyIs(""), "S03/abrupt records no key"},
		"an unknown key": {abruptKeyIs("ha-primary-pod-recovery-ghost"),
			"S03/abrupt is recorded with the key ha-primary-pod-recovery-ghost, not " + abruptKey},
		"the key of another required result": {abruptKeyIs(gracefulKey),
			"S03/abrupt is recorded with the key " + gracefulKey},
		"the key of a family, which is not one result": {abruptKeyIs("ha-primary-pod-recovery"),
			"S03/abrupt is recorded with the key ha-primary-pod-recovery,"},
		"the key of the same legacy name at the other level": {
			inScenario(s06, testRestart, func(scenario map[string]any) { scenario[fieldKey] = labRestartKey }),
			"S06/restart is recorded with the key " + labRestartKey},
		"no display name": {inScenario(s03, abrupt, func(scenario map[string]any) { delete(scenario, fieldDisplayName) }),
			"S03/abrupt records no display name"},
		"a key alone, with no legacy name that resolves": {inScenario(s03, abrupt, func(scenario map[string]any) {
			scenario[fieldVariant] = testUnknown
		}), "S03/ghost is not a legacy name of a kind result"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := copyRunOf(t, version2, kindRun)
			tc.change(t, dir)
			run, err := ReadRun(dir)
			if err != nil {
				t.Fatalf("a readable bundle was refused: %v", err)
			}
			if run.Validated() || !hasProblem(run, tc.want) {
				t.Errorf("validated = %v, problems = %q, want one containing %q", run.Validated(), run.Problems, tc.want)
			}
			if !run.Summary.Passed {
				t.Error("the recorded verdict changed")
			}
			for _, result := range run.Results {
				if result.Key != "" && result.Key != result.RecordedKey {
					t.Errorf("%s resolved to %s and recorded %s", result.Scenario.Name(), result.Key, result.RecordedKey)
				}
			}
		})
	}
}

// Two results that exchange their keys cannot stand for each other: neither
// resolves, and what each recorded stays visible.
func TestReadRunDoesNotLetTwoResultsExchangeTheirKeys(t *testing.T) {
	dir := copyRunOf(t, version2, kindRun)
	abruptKeyIs(gracefulKey)(t, dir)
	inScenario(s03, s03Graceful, func(scenario map[string]any) { scenario[fieldKey] = abruptKey })(t, dir)
	run, err := ReadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if run.Validated() {
		t.Error("two results with exchanged keys were validated")
	}
	got := resultOf(t, run, s03Abrupt)
	if got.Key != "" || got.RecordedKey != gracefulKey {
		t.Errorf("S03/abrupt = %+v, want no key and the recorded %s", got, gracefulKey)
	}
}

// The display name is for people. An earlier run keeps the words it was written
// with after the registry's wording improves, and the words prove nothing: only
// the key and the legacy name decide which result it is.
func TestReadRunKeepsTheRecordedDisplayNameAndDoesNotMatchOnIt(t *testing.T) {
	const earlierWords = "Earlier wording of the primary Pod recovery check"
	dir := copyRunOf(t, version2, kindRun)
	inScenario(s03, abrupt, func(scenario map[string]any) { scenario[fieldDisplayName] = earlierWords })(t, dir)
	run, err := ReadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Validated() {
		t.Errorf("problems = %q, want none", run.Problems)
	}
	if got := resultOf(t, run, s03Abrupt); got.Key != abruptKey || got.DisplayName != earlierWords {
		t.Errorf("S03/abrupt = %+v", got)
	}
}

// requirementsOf is the requirements of a registered lane by legacy name.
func requirementsOf(t *testing.T, name string) Lane {
	t.Helper()
	registered, ok := registry.lane(name)
	if !ok {
		t.Fatalf("lane %s is not registered", name)
	}
	lane := Lane{Name: name}
	for _, key := range registered.Keys {
		entry, err := registry.resolveKey(key)
		if err != nil {
			t.Fatal(err)
		}
		id, variant, _ := strings.Cut(entry.Aliases[0], "/")
		lane.Required = append(lane.Required, Requirement{ID: id, Variant: variant})
	}
	return lane
}

// Writing a key decides nothing. A required result that is failed, blocked, not
// run, ruled out, without evidence or missing fails the run in the version 2
// writer and in the read, and carrying a key does not make it a pass.
func TestWriterNeverTurnsAnUnprovenRequiredResultIntoAPass(t *testing.T) {
	result := func(got Result) func(*Scenario) {
		return func(sc *Scenario) { sc.Result, sc.Reason = got, testReason }
	}
	tests := map[string]struct {
		change func(*Scenario)
		// want is the judged result written for S03/abrupt; "" means no record.
		want Result
	}{
		"failed":        {result(Fail), Fail},
		"blocked":       {result(Blocked), Blocked},
		"not run":       {result(NotRun), NotRun},
		"ruled out":     {result(NotApplicable), NotApplicable},
		"no evidence":   {func(sc *Scenario) { sc.Evidence = nil }, Fail},
		"missing file":  {func(sc *Scenario) { sc.Evidence = []string{"gone.txt"} }, Fail},
		"missing":       {nil, ""},
		"never event":   {func(sc *Scenario) { sc.NeverEvents = []string{"two masters"} }, Fail},
		"unknown value": {result("passed"), Fail},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := copyRun(t, kindRun)
			summary := frozenSummary(t, dir)
			i := slices.IndexFunc(summary.Scenarios, func(sc Scenario) bool { return sc.Name() == s03Abrupt })
			if tc.change == nil {
				summary.Scenarios = slices.Delete(summary.Scenarios, i, i+1)
			} else {
				tc.change(&summary.Scenarios[i])
			}
			if err := summary.Conclude(dir, NewRedactor(), requirementsOf(t, kindLaneName)); err == nil {
				t.Error("the run passed its lane")
			}
			run, err := ReadRun(dir)
			if err != nil {
				t.Fatal(err)
			}
			if run.Summary.Passed || run.Validated() {
				t.Errorf("recorded pass = %v, validated = %v", run.Summary.Passed, run.Validated())
			}
			if !hasProblem(run, "S03/abrupt: ") {
				t.Errorf("problems = %q, want one that starts with the legacy name", run.Problems)
			}
			if tc.change == nil {
				return
			}
			if got := resultOf(t, run, s03Abrupt); got.Key != abruptKey || got.Scenario.Result != tc.want {
				t.Errorf("S03/abrupt = %+v, want %s under its key", got, tc.want)
			}
		})
	}
}

// A checkpoint of a version 2 run carries the identities written so far and is
// provisional; its JUnit run case stays.
func TestReadRunReportsAVersion2CheckpointAsProvisional(t *testing.T) {
	dir := copyRun(t, kindRun)
	if err := frozenSummary(t, dir).Checkpoint(dir, NewRedactor(), kindLaneName, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	run, err := ReadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Provisional || run.Validated() || run.Summary.SchemaVersion != schemaV2 {
		t.Errorf("provisional = %v, validated = %v, version = %s",
			run.Provisional, run.Validated(), run.Summary.SchemaVersion)
	}
	if got := resultOf(t, run, s03Abrupt); got.Key != abruptKey {
		t.Errorf("S03/abrupt = %+v", got)
	}
}

// A version number decides how a file is read, so it is not taken on trust in
// either direction, and a later version stays refused.
func TestReadRunRefusesWhatItCannotInterpretAtVersion2(t *testing.T) {
	first := func(summary map[string]any) map[string]any {
		return summary[fieldScenarios].([]any)[0].(map[string]any)
	}
	tests := map[string]struct {
		change func(map[string]any)
		want   string
	}{
		"a later version": {func(summary map[string]any) { summary[fieldSchemaVersion] = "3" }, `schema version "3"`},
		"a similar version": {func(summary map[string]any) { summary[fieldSchemaVersion] = "2.0" },
			`schema version "2.0"`},
		"a numeric version":               {func(summary map[string]any) { summary[fieldSchemaVersion] = 2 }, summaryFile},
		"a field version 2 does not have": {func(summary map[string]any) { summary["key"] = "x" }, unknownField},
		"a scenario field version 2 does not have": {func(summary map[string]any) { first(summary)["semanticKey"] = "x" },
			unknownField},
		"a version 2 file that calls itself version 1": {func(summary map[string]any) {
			summary[fieldSchemaVersion] = schemaV1
		}, unknownField},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := copyRunOf(t, version2, kindRun)
			inSummary(tc.change)(t, dir)
			run, err := ReadRun(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			if run.Summary.RunID != "" || run.Results != nil {
				t.Errorf("a refused run returned %+v", run)
			}
		})
	}
}

// A version 1 file that calls itself version 2 has no key on any result, so it
// reads but proves nothing: the number alone does not make a result new.
func TestReadRunDoesNotValidateAVersion1FileThatCallsItselfVersion2(t *testing.T) {
	dir := copyRun(t, kindRun)
	inSummary(func(summary map[string]any) { summary[fieldSchemaVersion] = schemaV2 })(t, dir)
	run, err := ReadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if run.Validated() || !hasProblem(run, "S03/abrupt records no key") {
		t.Errorf("validated = %v, problems = %q", run.Validated(), run.Problems)
	}
}
