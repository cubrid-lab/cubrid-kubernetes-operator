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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The runs in testdata/v1 are frozen schema version 1 results; nothing may
// change their bytes, and only their summary.json and junit.xml are committed.
// kind-run is the Kind suite on main at 72df6b1 (GitHub Actions run
// 42321395120). lab-smoke-run is an installed-Operator smoke run of 2026-10-19
// (#489), which recorded a lab lane at the kind level.
// lab-operator-restart-run was written by the version 1 writer for these
// tests, because no real run of that lane is kept in this repository.
//
// The runs in testdata/v2 are frozen schema version 2 results of the same two
// runs under the same names. Both are derived: the version 2 writer wrote them
// from the version 1 results above, so no CI or lab run produced them.
const (
	version1           = "v1"
	version2           = "v2"
	kindRun            = "kind-run"
	labSmokeRun        = "lab-smoke-run"
	labRestartRun      = "lab-operator-restart-run"
	labRestartLane     = "lab-installed-ha-s06"
	labSmokeLane       = "lab-installed-smoke"
	lab00              = "LAB00"
	endOfSuite         = "</testsuite>"
	s03AbruptCase      = `<testcase name="S03/abrupt" classname="scenario">`
	testReason         = "recorded by this test"
	missingS03Abrupt   = "S03/abrupt: missing"
	failedS03Abrupt    = "S03/abrupt: fail"
	fieldScenarios     = "scenarios"
	fieldID            = "id"
	fieldVariant       = "variant"
	fieldResult        = "result"
	fieldReason        = "reason"
	fieldEvidence      = "evidence"
	fieldLane          = "lane"
	fieldRunID         = "runId"
	fieldSchemaVersion = "schemaVersion"
	fieldKey           = "key"
	fieldDisplayName   = "displayName"
)

// requiredAfterTheFrozenRuns are the results the lanes came to require after
// the frozen runs were written: the deletion of a standalone cluster with each
// retention policy (S13/retain, S13/delete). A frozen run cannot gain results
// it never recorded, so it is judged by the registry frozen, whose lanes are
// the ones it ran under.
var requiredAfterTheFrozenRuns = []string{"cluster-deletion-retention-retain", "cluster-deletion-retention-delete"}

// frozen is the registry with the lanes the frozen runs were written under;
// every result stays registered.
var frozen = func() scenarioRegistry {
	r := registry
	r.lanes = slices.Clone(registry.lanes)
	for i, lane := range r.lanes {
		r.lanes[i].Keys = slices.DeleteFunc(slices.Clone(lane.Keys), func(k string) bool {
			return slices.Contains(requiredAfterTheFrozenRuns, k)
		})
	}
	return r
}()

// readFixture is ReadRun for a frozen run, or one made from it, under the
// lanes it was written under.
func readFixture(dir string) (RecordedRun, error) { return frozen.readRun(dir) }

// copyRun copies a frozen run into a directory of the test's own and adds an
// empty file for each evidence path it lists, then returns the directory.
func copyRun(t *testing.T, name string) string {
	t.Helper()
	return copyRunOf(t, version1, name)
}

// copyRunOf is copyRun for a frozen run of the given schema version.
func copyRunOf(t *testing.T, version, name string) string {
	t.Helper()
	dir := t.TempDir()
	for _, file := range []string{summaryFile, junitFile} {
		data, err := os.ReadFile(filepath.Join("testdata", version, name, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range loadSummary(t, dir)[fieldScenarios].([]any) {
		for _, rel := range item.(map[string]any)[fieldEvidence].([]any) {
			path := filepath.Join(dir, rel.(string))
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func decodeJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var decoded map[string]any
	if err := dec.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func loadSummary(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, summaryFile))
	if err != nil {
		t.Fatal(err)
	}
	return decodeJSON(t, data)
}

func saveSummary(t *testing.T, dir string, summary map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, summaryFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func scenarioNamed(t *testing.T, summary map[string]any, id, variant string) map[string]any {
	t.Helper()
	for _, item := range summary[fieldScenarios].([]any) {
		sc := item.(map[string]any)
		if got, _ := sc[fieldVariant].(string); sc[fieldID] == id && got == variant {
			return sc
		}
	}
	t.Fatalf("the fixture has no scenario %s %s", id, variant)
	return nil
}

func removeScenario(summary map[string]any, id, variant string) {
	summary[fieldScenarios] = slices.DeleteFunc(summary[fieldScenarios].([]any), func(item any) bool {
		sc := item.(map[string]any)
		got, _ := sc[fieldVariant].(string)
		return sc[fieldID] == id && got == variant
	})
}

func editJUnit(t *testing.T, dir, from, to string) {
	t.Helper()
	path := filepath.Join(dir, junitFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), from) {
		t.Fatalf("junit.xml does not contain %q", from)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), from, to)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// snapshot returns the content of every file under dir.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func digest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func resultOf(t *testing.T, run RecordedRun, name string) RecordedResult {
	t.Helper()
	for _, result := range run.Results {
		if result.Scenario.Name() == name {
			return result
		}
	}
	t.Fatalf("no result of %s in %+v", name, run.Results)
	return RecordedResult{}
}

func hasProblem(run RecordedRun, want string) bool {
	return slices.ContainsFunc(run.Problems, func(p string) bool { return strings.Contains(p, want) })
}

// A version 1 bundle resolves under the lane and level it recorded: the one
// legacy name S06/restart is the Kind test in a Kind run and the lab test in a
// lab run, and each run validates its own lane and no other.
func TestReadRunResolvesAVersion1BundleUnderItsOriginalLaneAndLevel(t *testing.T) {
	tests := map[string]struct {
		lane, level, key string
		results          int
	}{
		kindRun:       {kindLaneName, LevelKind, "operator-failure-recovery-restart", 14},
		labRestartRun: {labRestartLane, LevelVMLab, "installed-operator-failure-recovery-restart", 2},
	}
	for fixture, tc := range tests {
		t.Run(fixture, func(t *testing.T) {
			run, err := readFixture(copyRun(t, fixture))
			if err != nil {
				t.Fatal(err)
			}
			if !run.Validated() {
				t.Fatalf("problems = %q, want none", run.Problems)
			}
			if run.Summary.Lane != tc.lane || run.Summary.Environment.Level != tc.level {
				t.Errorf("read lane %s at level %s, want %s at %s",
					run.Summary.Lane, run.Summary.Environment.Level, tc.lane, tc.level)
			}
			if len(run.Results) != tc.results {
				t.Fatalf("%d results, want %d", len(run.Results), tc.results)
			}
			if got := resultOf(t, run, testAlias); got.Key != tc.key || got.Judged != Pass {
				t.Errorf("S06/restart = %+v, want key %s and a pass", got, tc.key)
			}
			lane, _ := frozen.lane(tc.lane)
			keys := make([]string, 0, len(run.Results))
			for _, result := range run.Results {
				keys = append(keys, result.Key)
			}
			if !slices.Equal(slices.Sorted(slices.Values(keys)), slices.Sorted(slices.Values(lane.Keys))) {
				t.Errorf("keys = %q, want the lane's %q", keys, lane.Keys)
			}
		})
	}
}

// Everything the bundle recorded survives the read, the raw files are named by
// their digests, and nothing in the run directory changes.
func TestReadRunKeepsTheRecordIntactAndChangesNothing(t *testing.T) {
	for _, fixture := range []string{kindRun, labSmokeRun, labRestartRun} {
		t.Run(fixture, func(t *testing.T) {
			dir := copyRun(t, fixture)
			before := snapshot(t, dir)
			run, err := readFixture(dir)
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
			if got, want := decodeJSON(t, read), loadSummary(t, dir); !reflect.DeepEqual(got, want) {
				t.Errorf("the read summary differs from the file:\n got %v\nwant %v", got, want)
			}
			if run.SummarySHA256 != digest(t, filepath.Join(dir, summaryFile)) ||
				run.JUnitSHA256 != digest(t, filepath.Join(dir, junitFile)) {
				t.Error("the digests do not name the raw files")
			}
			if run.Dir != dir {
				t.Errorf("Dir = %s, want %s", run.Dir, dir)
			}
		})
	}
}

// If a smoke run recorded a lab lane at the kind level, as the real run of #489
// did, its recorded pass stays readable. The result is neither the Kind suite's
// nor a VM-lab result, so no key may claim it.
func TestReadRunKeepsALabLaneRecordedAtTheKindLevelUnresolved(t *testing.T) {
	run, err := readFixture(copyRun(t, labSmokeRun))
	if err != nil {
		t.Fatal(err)
	}
	if !run.Summary.Passed || run.Summary.Lane != labSmokeLane || run.Summary.Environment.Level != LevelKind {
		t.Errorf("the recorded verdict, lane or level changed: %+v", run.Summary)
	}
	if run.Validated() {
		t.Error("a lab lane recorded at the kind level was validated")
	}
	if got := resultOf(t, run, lab00); got.Key != "" || got.Scenario.Result != Pass {
		t.Errorf("LAB00 = %+v, want the recorded pass with no key", got)
	}
}

// bundleChange damages a copy of the passing Kind bundle.
type bundleChange func(t *testing.T, dir string)

// inSummary is a change to the summary as a whole.
func inSummary(change func(summary map[string]any)) bundleChange {
	return func(t *testing.T, dir string) {
		summary := loadSummary(t, dir)
		change(summary)
		saveSummary(t, dir, summary)
	}
}

// inAbrupt is a change to the required result S03/abrupt.
func inAbrupt(change func(scenario map[string]any)) bundleChange {
	return func(t *testing.T, dir string) {
		summary := loadSummary(t, dir)
		change(scenarioNamed(t, summary, s03, abrupt))
		saveSummary(t, dir, summary)
	}
}

func abruptResult(result string) bundleChange {
	return inAbrupt(func(scenario map[string]any) {
		scenario[fieldResult], scenario[fieldReason] = result, testReason
	})
}

func abruptRenamed(id, variant string) bundleChange {
	return inAbrupt(func(scenario map[string]any) { scenario[fieldID], scenario[fieldVariant] = id, variant })
}

func inJUnit(from, to string) bundleChange {
	return func(t *testing.T, dir string) { editJUnit(t, dir, from, to) }
}

func removeFirstEvidence(t *testing.T, dir string) {
	t.Helper()
	scenario := scenarioNamed(t, loadSummary(t, dir), s03, abrupt)
	if err := os.Remove(filepath.Join(dir, scenario[fieldEvidence].([]any)[0].(string))); err != nil {
		t.Fatal(err)
	}
}

// Each change makes a passing bundle unfit to stand as a validation of its lane,
// in the same words at both schema versions. The bundle stays readable, and the
// read says what is wrong.
func TestReadRunNeverValidatesABundleThatDoesNotProveItsLane(t *testing.T) {
	tests := map[string]struct {
		change bundleChange
		// want lists text that each must appear in one of the problems.
		want []string
	}{
		"an unknown legacy name in place of a required result": {abruptRenamed(s03, testUnknown),
			[]string{"S03/ghost is not a legacy name of a kind result", missingS03Abrupt}},
		"a lab-only legacy name in place of a required result": {abruptRenamed("S11", "complete"),
			[]string{"S11/complete is not a legacy name of a kind result", missingS03Abrupt}},
		"a variant written into the ID to spell the legacy name": {abruptRenamed(s03Abrupt, ""),
			[]string{missingS03Abrupt}},
		"a second record of another result in place of a required one": {func(t *testing.T, dir string) {
			summary := loadSummary(t, dir)
			removeScenario(summary, s03, abrupt)
			summary[fieldScenarios] = append(summary[fieldScenarios].([]any), scenarioNamed(t, summary, s00, ""))
			saveSummary(t, dir, summary)
		}, []string{missingS03Abrupt}},
		"a required result removed": {inSummary(func(summary map[string]any) { removeScenario(summary, s03, abrupt) }),
			[]string{missingS03Abrupt}},
		"a required result failed":           {abruptResult(string(Fail)), []string{failedS03Abrupt}},
		"a required result blocked":          {abruptResult(string(Blocked)), []string{"S03/abrupt: blocked"}},
		"a required result that was not run": {abruptResult(string(NotRun)), []string{"S03/abrupt: not_run"}},
		"a required result ruled out": {abruptResult(string(NotApplicable)),
			[]string{"S03/abrupt: not_applicable"}},
		"a result outside the five defined ones": {abruptResult("passed"), []string{failedS03Abrupt}},
		"a second, failed record of a required result": {func(t *testing.T, dir string) {
			summary := loadSummary(t, dir)
			failed := maps.Clone(scenarioNamed(t, summary, s03, abrupt))
			failed[fieldResult], failed[fieldReason] = string(Fail), testReason
			summary[fieldScenarios] = append(summary[fieldScenarios].([]any), failed)
			saveSummary(t, dir, summary)
		}, []string{failedS03Abrupt}},
		"evidence that was not kept": {removeFirstEvidence, []string{"S03/abrupt: fail: claimed a pass, but evidence"}},
		"a pass that lists no evidence": {inAbrupt(func(scenario map[string]any) { scenario[fieldEvidence] = []any{} }),
			[]string{"S03/abrupt: fail: claimed a pass, but no evidence"}},
		"a pass whose fault was not confirmed": {inAbrupt(func(scenario map[string]any) {
			scenario["faultConfirmed"] = false
		}), []string{"S03/abrupt: fail: claimed a pass, but the fault"}},
		"a pass with an outcome that must never happen": {inAbrupt(func(scenario map[string]any) {
			scenario["neverEvents"] = []any{"two masters"}
		}), []string{"S03/abrupt: fail: an outcome that must never happen"}},
		"a run that says it did not pass": {inSummary(func(summary map[string]any) { summary["passed"] = false }),
			[]string{"recorded that it did not pass lane kind"}},
		"a pass that lists problems": {inSummary(func(summary map[string]any) { summary["problems"] = []any{"S00: x"} }),
			[]string{"claims a pass but lists problems"}},
		"a run that did not finish": {inSummary(func(summary map[string]any) {
			summary["finishedAt"] = time.Time{}.Format(time.RFC3339)
		}), []string{"did not finish"}},
		"a run selected with a filter": {inSummary(func(summary map[string]any) { summary[fieldLane] = filteredKindLane }),
			[]string{"selected its own requirements"}},
		"a lane that is not registered": {inSummary(func(summary map[string]any) { summary[fieldLane] = "kind-nightly" }),
			[]string{"lane kind-nightly is not registered"}},
		"a run with no lane": {inSummary(func(summary map[string]any) { delete(summary, fieldLane) }),
			[]string{"lane  is not registered"}},
		"the Kind lane recorded at the lab level": {inSummary(func(summary map[string]any) {
			summary["environment"].(map[string]any)["level"] = LevelVMLab
		}), []string{"lane kind is judged at level kind, not vm-lab"}},
		"a run with no run ID": {inSummary(func(summary map[string]any) { summary[fieldRunID] = "" }),
			[]string{"records no run ID"}},
		"a run with no start time": {inSummary(func(summary map[string]any) {
			summary["startedAt"] = time.Time{}.Format(time.RFC3339)
		}), []string{"records no start time"}},
		"no junit.xml": {func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, junitFile)); err != nil {
				t.Fatal(err)
			}
		}, []string{"junit.xml could not be read"}},
		"a junit.xml that does not parse": {inJUnit(endOfSuite, ""), []string{"junit.xml could not be read"}},
		"the junit.xml of another run": {func(t *testing.T, dir string) {
			editJUnit(t, dir, `name="`+loadSummary(t, dir)[fieldRunID].(string)+`"`, `name="another-run"`)
		}, []string{"junit.xml belongs to run another-run"}},
		"a JUnit case renamed": {inJUnit(s03AbruptCase, `<testcase name="S03/renamed" classname="scenario">`),
			[]string{"junit.xml has S03/renamed: pass where summary.json has S03/abrupt: pass"}},
		"a JUnit verdict that differs from the summary": {
			inJUnit(s03AbruptCase, s03AbruptCase+`<failure message="x"></failure>`),
			[]string{"junit.xml has S03/abrupt: fail where summary.json has S03/abrupt: pass"}},
		"a JUnit case the summary does not have": {
			inJUnit(endOfSuite, `<testcase name="S99" classname="scenario"></testcase>`+endOfSuite),
			[]string{"junit.xml has 15 scenario cases and summary.json 14"}},
		"a JUnit run case on a run that claims a pass": {
			inJUnit(endOfSuite, `<testcase name="run" classname="run"><error message="x"></error></testcase>`+endOfSuite),
			[]string{"junit.xml has a run case, so the run did not pass"}},
		"a JUnit case of an unknown class": {inJUnit(s03AbruptCase, `<testcase name="S03/abrupt" classname="other">`),
			[]string{"junit.xml case S03/abrupt has the class other"}},
	}
	for _, version := range []string{version1, version2} {
		for name, tc := range tests {
			t.Run(version+"/"+name, func(t *testing.T) {
				dir := copyRunOf(t, version, kindRun)
				tc.change(t, dir)
				run, err := readFixture(dir)
				if err != nil {
					t.Fatalf("a readable bundle was refused: %v", err)
				}
				if run.Validated() {
					t.Fatal("the bundle was validated")
				}
				for _, want := range tc.want {
					if !hasProblem(run, want) {
						t.Errorf("problems = %q, want one containing %q", run.Problems, want)
					}
				}
			})
		}
	}
}

// A problem found on reading sits beside what the run recorded and never
// replaces it: an old verdict is not rewritten.
func TestReadRunKeepsTheOriginalVerdictOfAResultItRejects(t *testing.T) {
	dir := copyRun(t, kindRun)
	removeFirstEvidence(t, dir)
	run, err := readFixture(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := resultOf(t, run, s03Abrupt)
	if got.Scenario.Result != Pass || !run.Summary.Passed {
		t.Errorf("the recorded pass changed: %+v, passed=%v", got.Scenario, run.Summary.Passed)
	}
	if got.Judged != Fail || got.Reason == "" {
		t.Errorf("judged = %s (%s), want a fail with its reason", got.Judged, got.Reason)
	}
}

// A checkpoint of a run that may have been killed stays readable and says so.
func TestReadRunReportsACheckpointAsProvisional(t *testing.T) {
	dir := t.TempDir()
	summary := Summary{RunID: testRunName, StartedAt: time.Unix(1, 0).UTC(), Environment: Environment{Level: LevelKind},
		Scenarios: []Scenario{{ID: s00, Result: NotRun, Reason: "not started"}}}
	if err := summary.Checkpoint(dir, NewRedactor(), kindLaneName, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	run, err := readFixture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Provisional || run.Validated() {
		t.Errorf("provisional = %v, validated = %v, want a provisional run that is not validated",
			run.Provisional, run.Validated())
	}
}

// What the writer produces today is what the reader accepts, so a change to
// the format that forgets the reader fails here.
func TestReadRunReadsWhatTheWriterWrites(t *testing.T) {
	dir := runDir(t, s00Record)
	summary := Summary{RunID: testRunName, StartedAt: time.Unix(1, 0).UTC(), FinishedAt: time.Unix(2, 0).UTC(),
		Environment: Environment{Level: LevelVMLab}, Notes: []string{"a note"},
		Scenarios: []Scenario{{ID: lab00, Result: Pass, Evidence: []string{s00Record}}}}
	lane := Lane{Name: labSmokeLane, Required: []Requirement{{ID: lab00}}}
	if err := summary.Conclude(dir, NewRedactor(), lane); err != nil {
		t.Fatal(err)
	}
	run, err := readFixture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Validated() {
		t.Errorf("problems = %q, want none", run.Problems)
	}
	const key = "installed-standalone-sql-smoke"
	if got := resultOf(t, run, lab00); got.Key != key || got.RecordedKey != key {
		t.Errorf("LAB00 = %+v", got)
	}
}

// A version the reader was not written for is refused, because its fields may
// not mean what version 1's mean. So is a file that is not one whole summary.
func TestReadRunRefusesWhatItCannotInterpret(t *testing.T) {
	version := func(value any) func(map[string]any) {
		return func(summary map[string]any) { summary[fieldSchemaVersion] = value }
	}
	const noVersion = "records no schema version"
	tests := map[string]struct {
		change func(map[string]any)
		raw    string
		want   string
	}{
		"a later version":   {change: version("3"), want: `schema version "3"`},
		"an empty version":  {change: version(""), want: `schema version ""`},
		"a similar version": {change: version("1.0"), want: `schema version "1.0"`},
		"a numeric version": {change: version(1), want: summaryFile},
		"a null version":    {change: version(nil), want: noVersion},
		"no version": {change: func(summary map[string]any) { delete(summary, fieldSchemaVersion) },
			want: noVersion},
		"a field version 1 does not have": {change: func(summary map[string]any) { summary["key"] = "x" },
			want: unknownField},
		"a scenario field version 1 does not have": {change: func(summary map[string]any) {
			summary[fieldScenarios].([]any)[0].(map[string]any)[fieldKey] = testBaseKey
		}, want: unknownField},
		"a display name in a version 1 result": {change: func(summary map[string]any) {
			summary[fieldScenarios].([]any)[0].(map[string]any)[fieldDisplayName] = "x"
		}, want: unknownField},
		"a truncated file":  {raw: `{"schemaVersion":"1","runId":"x"`, want: summaryFile},
		"a second document": {raw: `{"schemaVersion":"1"}{"schemaVersion":"2"}`, want: summaryFile},
		"not an object":     {raw: `[]`, want: summaryFile},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := copyRun(t, kindRun)
			if tc.raw != "" {
				if err := os.WriteFile(filepath.Join(dir, summaryFile), []byte(tc.raw), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				inSummary(tc.change)(t, dir)
			}
			run, err := readFixture(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			if run.Summary.RunID != "" || run.Results != nil {
				t.Errorf("a refused run returned %+v", run)
			}
		})
	}
	if _, err := ReadRun(t.TempDir()); err == nil {
		t.Error("a directory with no summary.json was read")
	}
}

// aliasRun writes a run of the small test registry's Kind lane in which every
// given scenario claims a pass with its evidence. It is turned into schema
// version 1, which records no key in either file: the writer knows only the real
// registry, and what these tests check is how legacy names resolve.
func aliasRun(t *testing.T, scenarios ...Scenario) string {
	t.Helper()
	dir := runDir(t, s00Record)
	summary := Summary{RunID: testRunName, StartedAt: time.Unix(1, 0).UTC(), FinishedAt: time.Unix(2, 0).UTC(),
		Environment: Environment{Level: LevelKind}, Lane: testKindLane, Passed: true, Scenarios: scenarios}
	for i := range summary.Scenarios {
		summary.Scenarios[i].Result, summary.Scenarios[i].Evidence = Pass, []string{s00Record}
	}
	if err := summary.Write(dir, NewRedactor()); err != nil {
		t.Fatal(err)
	}
	written := withoutIdentity(loadSummary(t, dir))
	written[fieldSchemaVersion] = schemaV1
	saveSummary(t, dir, written)
	path := filepath.Join(dir, junitFile)
	junit, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, junitKeyProperties.ReplaceAll(junit, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A legacy name that two results share, or two legacy names of one result,
// must not stand in for a result that is missing. The validated registry
// refuses both; these use a registry that is not valid, so that the read holds
// on its own.
func TestReadRunDoesNotLetACollidingAliasSatisfyARequirement(t *testing.T) {
	smoke := Scenario{ID: testBaseAlias}
	restart := Scenario{ID: s06, Variant: testRestart}
	const reboot = "reboot"

	t.Run("two legacy names of one result are recorded", func(t *testing.T) {
		reg := testRegistry()
		reg.entries[1].Aliases = append(reg.entries[1].Aliases, s06+"/"+reboot)
		run, err := reg.readRun(aliasRun(t, smoke, restart, Scenario{ID: s06, Variant: reboot}))
		if err != nil {
			t.Fatal(err)
		}
		if run.Validated() || !hasProblem(run, testKindKey) {
			t.Errorf("validated = %v, problems = %q, want a problem naming %s", run.Validated(), run.Problems, testKindKey)
		}
	})
	t.Run("one legacy name stands for two required results", func(t *testing.T) {
		reg := testRegistry()
		reg.entries[0].Aliases, reg.entries[0].Family = []string{testAlias}, s06
		run, err := reg.readRun(aliasRun(t, restart))
		if err != nil {
			t.Fatal(err)
		}
		if run.Validated() {
			t.Error("one record was validated as two results")
		}
		if got := resultOf(t, run, testAlias); got.Key != "" {
			t.Errorf("an ambiguous name resolved to %s", got.Key)
		}
	})
	t.Run("a result recorded under the second of its legacy names counts once", func(t *testing.T) {
		reg := testRegistry()
		reg.entries[1].Aliases = []string{s06 + "/" + reboot, testAlias}
		run, err := reg.readRun(aliasRun(t, smoke, restart))
		if err != nil {
			t.Fatal(err)
		}
		if !run.Validated() {
			t.Errorf("problems = %q, want none", run.Problems)
		}
	})
}
