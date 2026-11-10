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
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	renamedVariant = "abrupt-later-master"
	renamedAlias   = "S03/abrupt-later-master"
	fieldLimits    = "limits"
	fieldFault     = "faultConfirmed"
	fieldOperation = "operations"
	fieldMeasured  = "measurements"
	failoverLimit  = "failover_limit"
)

// withAlias is the registry after a result took another legacy name, as a
// rename of its scenario would register it.
func withAlias(key, alias string) scenarioRegistry {
	renamed := registry
	renamed.entries = slices.Clone(registry.entries)
	for i, entry := range renamed.entries {
		if entry.Key == key {
			renamed.entries[i].Aliases = append(slices.Clone(entry.Aliases), alias)
		}
	}
	return renamed
}

// outOfLanes is the registry after a result left its lanes. The entry stays, so
// that the results earlier runs recorded of it still have an identity.
func outOfLanes(key string) scenarioRegistry {
	retired := registry
	retired.lanes = slices.Clone(registry.lanes)
	for i, lane := range retired.lanes {
		retired.lanes[i].Keys = slices.DeleteFunc(slices.Clone(lane.Keys), func(k string) bool { return k == key })
	}
	return retired
}

// readChanged reads a frozen run, changed as given, under the registry.
func readChanged(t *testing.T, r scenarioRegistry, version, fixture string, changes ...bundleChange) RecordedRun {
	t.Helper()
	dir := copyRunOf(t, version, fixture)
	for _, change := range changes {
		change(t, dir)
	}
	run, err := r.readRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func resultWithKey(t *testing.T, c Comparison, key string) ComparedResult {
	t.Helper()
	for _, result := range c.Results {
		if result.Key == key {
			return result
		}
	}
	t.Fatalf("no result with the key %s in %+v", key, c.Results)
	return ComparedResult{}
}

func comparedValue(t *testing.T, values []ComparedValue, name string) ComparedValue {
	t.Helper()
	for _, value := range values {
		if value.Name == name {
			return value
		}
	}
	t.Fatalf("no value %s in %+v", name, values)
	return ComparedValue{}
}

func comparisonSays(c Comparison, want string) bool {
	return slices.ContainsFunc(c.Problems, func(p string) bool { return strings.Contains(p, want) })
}

func abruptRemoved() bundleChange {
	return func(t *testing.T, dir string) {
		inSummary(func(summary map[string]any) { removeScenario(summary, s03, abrupt) })(t, dir)
		editJUnit(t, dir, s03AbruptCase+"\n    <system-out>"+
			"Primary database Pod termination and recovery - abrupt, later master (S03/abrupt)"+
			"</system-out>\n  </testcase>\n  ", "")
	}
}

// abruptRenamedInBothFiles renames the case in summary.json and junit.xml, as
// a writer that took the new legacy name would have written it.
func abruptRenamedInBothFiles() bundleChange {
	return func(t *testing.T, dir string) {
		abruptRenamed(s03, renamedVariant)(t, dir)
		editJUnit(t, dir, `name="S03/abrupt"`, `name="`+renamedAlias+`"`)
	}
}

// One run read at the two schema versions is the same results under the same
// keys, each with a reference to its own raw files, and comparing reads and
// changes nothing in either directory.
func TestCompareRunsMatchesARunAcrossTheFormatVersions(t *testing.T) {
	for _, fixture := range []string{kindRun, labRestartRun} {
		t.Run(fixture, func(t *testing.T) {
			older := readChanged(t, registry, version1, fixture)
			newer := readChanged(t, registry, version2, fixture)
			oldFiles, newFiles := snapshot(t, older.Dir), snapshot(t, newer.Dir)
			got := CompareRuns(older, newer)
			if !got.Matches() {
				t.Fatalf("problems = %q, want none", got.Problems)
			}
			if !maps.Equal(snapshot(t, older.Dir), oldFiles) || !maps.Equal(snapshot(t, newer.Dir), newFiles) {
				t.Error("comparing changed a run directory")
			}
			if len(got.Results) != len(older.Results) {
				t.Fatalf("%d compared results, want %d", len(got.Results), len(older.Results))
			}
			if got.Old.SummarySHA256 != digest(t, filepath.Join(older.Dir, summaryFile)) ||
				got.New.SummarySHA256 != digest(t, filepath.Join(newer.Dir, summaryFile)) {
				t.Error("the comparison lost the digest of a source file")
			}
			for _, result := range got.Results {
				if result.Change != ScenarioUnchanged || result.OldVerdict != Pass || result.NewVerdict != Pass {
					t.Errorf("%s = %s, %s to %s", result.Key, result.Change, result.OldVerdict, result.NewVerdict)
				}
				for _, rel := range result.Old[0].Scenario.Evidence {
					if _, err := os.Stat(filepath.Join(got.Old.Dir, rel)); err != nil {
						t.Errorf("the reference to the raw evidence of %s is lost: %v", result.Key, err)
					}
				}
				if !reflect.DeepEqual(result.Old[0].Scenario, resultOf(t, older, result.Old[0].Scenario.Name()).Scenario) {
					t.Errorf("the record of %s changed", result.Key)
				}
			}
		})
	}
}

// Each change leaves a required result unproven on one side. It must stay
// visible under its own key with what happened to it, on whichever side it is,
// and the two runs must not match.
func TestCompareRunsNeverMatchesAnUnprovenRequiredResult(t *testing.T) {
	tests := map[string]struct {
		change  bundleChange
		state   Change
		verdict Result
	}{
		"no result":           {abruptRemoved(), ScenarioMissing, ""},
		"recorded as not run": {abruptResult("not_run"), ScenarioNotRun, NotRun},
		"blocked":             {abruptResult(string(Blocked)), ScenarioUnchanged, Blocked},
		"failed":              {abruptResult("fail"), ScenarioUnchanged, Fail},
		"not applicable":      {abruptResult("not_applicable"), ScenarioUnchanged, NotApplicable},
		"an unknown result":   {abruptResult("flaky"), ScenarioUnchanged, Fail},
		"evidence file gone":  {removeFirstEvidence, ScenarioUnchanged, Fail},
		"no evidence listed": {inAbrupt(func(scenario map[string]any) { scenario[fieldEvidence] = []any{} }),
			ScenarioUnchanged, Fail},
		"unconfirmed fault": {inAbrupt(func(scenario map[string]any) { scenario[fieldFault] = false }),
			ScenarioUnchanged, Fail},
		"a never event": {inAbrupt(func(scenario map[string]any) { scenario["neverEvents"] = []any{testReason} }),
			ScenarioUnchanged, Fail},
	}
	for name, tc := range tests {
		for _, version := range []string{version1, version2} {
			t.Run(name+" at version "+version, func(t *testing.T) {
				good := readChanged(t, registry, version, kindRun)
				bad := readChanged(t, registry, version, kindRun, tc.change)
				later := CompareRuns(good, bad)
				if later.Matches() {
					t.Error("the damaged new run matched")
				}
				if got := resultWithKey(t, later, abruptKey); got.Change != tc.state || got.NewVerdict != tc.verdict ||
					got.OldVerdict != Pass || got.Problem == "" {
					t.Errorf("new side: %+v, want %s with verdict %q", got, tc.state, tc.verdict)
				}
				earlier := CompareRuns(bad, good)
				if earlier.Matches() {
					t.Error("the damaged old run matched")
				}
				got := resultWithKey(t, earlier, abruptKey)
				if got.OldVerdict != tc.verdict || got.NewVerdict != Pass || got.Problem == "" {
					t.Errorf("old side: %+v, want verdict %q", got, tc.verdict)
				}
			})
		}
	}
}

// Two runs that agree on an unproven result agree on nothing that can be relied
// on: the same blocked result in both is not a match, and neither is the same
// missing one.
func TestCompareRunsDoesNotTakeTheSameUnprovenResultForAMatch(t *testing.T) {
	tests := map[string]struct {
		change bundleChange
		state  Change
	}{
		"blocked in both":        {abruptResult(string(Blocked)), ScenarioUnchanged},
		"not run in both":        {abruptResult("not_run"), ScenarioNotRun},
		"not applicable in both": {abruptResult("not_applicable"), ScenarioUnchanged},
		"missing from both":      {abruptRemoved(), ScenarioMissing},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := CompareRuns(readChanged(t, registry, version2, kindRun, tc.change),
				readChanged(t, registry, version2, kindRun, tc.change))
			if got.Matches() {
				t.Error("two runs with the same unproven result matched")
			}
			if result := resultWithKey(t, got, abruptKey); result.Change != tc.state || result.Problem == "" {
				t.Errorf("result = %+v, want %s with a problem", result, tc.state)
			}
		})
	}
}

// A rename is what the registry says: the two names are aliases of one key. The
// result keeps its verdicts on both sides, and is shown as renamed rather than
// as one result removed and another added.
func TestCompareRunsShowsARenameTheRegistryRecords(t *testing.T) {
	renamed := withAlias(abruptKey, renamedAlias)
	for _, version := range []string{version1, version2} {
		t.Run(version, func(t *testing.T) {
			older := readChanged(t, renamed, version, kindRun)
			newer := readChanged(t, renamed, version, kindRun, abruptRenamedInBothFiles())
			if !older.Validated() || !newer.Validated() {
				t.Fatalf("problems: old %q, new %q", older.Problems, newer.Problems)
			}
			got := renamed.compareRuns(older, newer)
			if !got.Matches() {
				t.Fatalf("problems = %q", got.Problems)
			}
			result := resultWithKey(t, got, abruptKey)
			if result.Change != ScenarioRenamed || result.Old[0].Scenario.Name() != s03Abrupt ||
				result.New[0].Scenario.Name() != renamedAlias {
				t.Errorf("result = %+v, want S03/abrupt renamed to %s", result, renamedAlias)
			}
			if len(got.Results) != 14 {
				t.Errorf("%d results, want one for each of the 14 keys", len(got.Results))
			}
		})
	}
}

// Without an alias in the registry nothing says the two names are one test,
// however alike they read, so the old result is missing, the new one has no
// identity, and neither is a rename. The same holds for a new name whose
// display name is the old one's: nothing matches on a display name.
func TestCompareRunsNeverGuessesARename(t *testing.T) {
	older := readChanged(t, registry, version2, kindRun)
	newer := readChanged(t, registry, version2, kindRun, abruptRenamedInBothFiles())
	got := CompareRuns(older, newer)
	if got.Matches() {
		t.Error("an unregistered new name matched")
	}
	for _, result := range got.Results {
		if result.Change == ScenarioRenamed {
			t.Errorf("a rename was guessed: %+v", result)
		}
	}
	if result := resultWithKey(t, got, abruptKey); result.Change != ScenarioMissing {
		t.Errorf("the old result = %+v, want it missing", result)
	}
	if result := resultWithKey(t, got, ""); result.Change != ScenarioUnresolved ||
		result.New[0].Scenario.Name() != renamedAlias || result.Problem == "" {
		t.Errorf("the new result = %+v, want it unresolved", result)
	}
}

// If a run was read under a registry with an alias that the comparing registry
// does not hold, the comparing registry decides: without the alias there, the
// result has no single identity and this is no rename either.
func TestCompareRunsRequiresBothNamesAmongTheAliasesOfTheKey(t *testing.T) {
	renamed := withAlias(abruptKey, renamedAlias)
	older := readChanged(t, renamed, version2, kindRun)
	newer := readChanged(t, renamed, version2, kindRun, abruptRenamedInBothFiles())
	got := CompareRuns(older, newer)
	if got.Matches() {
		t.Error("a name the registry does not hold matched")
	}
	if result := resultWithKey(t, got, abruptKey); result.Change != ScenarioUnresolved || result.Problem == "" {
		t.Errorf("result = %+v, want it unresolved", result)
	}
}

// A result only the new run holds is added, and only the old run holds is
// removed once no lane requires it; while a lane requires it, it is missing.
// A result recorded as not run in the new run is neither of these. None of
// them lets the runs match.
func TestCompareRunsTellsAddedRemovedAndNotRunApart(t *testing.T) {
	retired := outOfLanes(abruptKey)
	tests := map[string]struct {
		r            scenarioRegistry
		older, newer []bundleChange
		state        Change
	}{
		"only the new run holds it": {r: registry, older: []bundleChange{abruptRemoved()}, state: ScenarioAdded},
		"only the old run holds it, and no lane requires it": {r: retired, newer: []bundleChange{abruptRemoved()},
			state: ScenarioRemoved},
		"only the old run holds it, and its lane requires it": {r: registry, newer: []bundleChange{abruptRemoved()},
			state: ScenarioMissing},
		"the new run records that it did not run": {r: registry, newer: []bundleChange{abruptResult("not_run")},
			state: ScenarioNotRun},
		"the old run records that it did not run": {r: registry, older: []bundleChange{abruptResult("not_run")},
			state: ScenarioNotRun},
		"a new placeholder is not an addition": {r: registry, older: []bundleChange{abruptRemoved()},
			newer: []bundleChange{abruptResult("not_run")}, state: ScenarioNotRun},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := tc.r.compareRuns(readChanged(t, tc.r, version2, kindRun, tc.older...),
				readChanged(t, tc.r, version2, kindRun, tc.newer...))
			if got.Matches() {
				t.Error("the runs matched")
			}
			if result := resultWithKey(t, got, abruptKey); result.Change != tc.state || result.Problem == "" {
				t.Errorf("result = %+v, want %s with a problem", result, tc.state)
			}
		})
	}
}

// A result that resolves to no single registered result is never paired, also
// when both runs hold the same legacy name: a name shared by two results, or
// two names of one result in one run, say nothing certain of either.
func TestCompareRunsNeverPairsAResultWithoutOneIdentity(t *testing.T) {
	t.Run("a legacy name of two results", func(t *testing.T) {
		collision := withAlias(gracefulKey, s03Abrupt)
		got := collision.compareRuns(readChanged(t, collision, version1, kindRun),
			readChanged(t, collision, version1, kindRun))
		if got.Matches() {
			t.Error("a colliding alias matched")
		}
		if result := resultWithKey(t, got, ""); result.Change != ScenarioUnresolved || result.Problem == "" {
			t.Errorf("result = %+v", result)
		}
	})
	t.Run("two names of one result in one run", func(t *testing.T) {
		renamed := withAlias(abruptKey, renamedAlias)
		both := inSummary(func(summary map[string]any) {
			extra := maps.Clone(scenarioNamed(t, summary, s03, abrupt))
			extra[fieldVariant] = renamedVariant
			summary[fieldScenarios] = append(summary[fieldScenarios].([]any), extra)
		})
		got := renamed.compareRuns(readChanged(t, renamed, version2, kindRun),
			readChanged(t, renamed, version2, kindRun, both))
		if got.Matches() {
			t.Error("a run with two names of one result matched")
		}
		if result := resultWithKey(t, got, abruptKey); result.Change != ScenarioUnresolved || result.Problem == "" {
			t.Errorf("result = %+v", result)
		}
	})
	t.Run("a lab lane recorded at the kind level", func(t *testing.T) {
		run := readChanged(t, registry, version1, labSmokeRun)
		got := CompareRuns(run, readChanged(t, registry, version1, labSmokeRun))
		if got.Matches() {
			t.Error("results with no key matched")
		}
		if result := resultWithKey(t, got, ""); result.Change != ScenarioUnresolved {
			t.Errorf("result = %+v", result)
		}
	})
}

// A result recorded twice in one run has records no one can pair with the
// other run's, so the comparison says so rather than pick one.
func TestCompareRunsDoesNotPairARepeatedResult(t *testing.T) {
	twice := inSummary(func(summary map[string]any) {
		sc := scenarioNamed(t, summary, s03, abrupt)
		summary[fieldScenarios] = append(summary[fieldScenarios].([]any), maps.Clone(sc))
	})
	got := CompareRuns(readChanged(t, registry, version2, kindRun), readChanged(t, registry, version2, kindRun, twice))
	if got.Matches() {
		t.Error("a repeated result matched")
	}
	if result := resultWithKey(t, got, abruptKey); len(result.New) != 2 || result.Problem == "" {
		t.Errorf("result = %+v, want both records and a problem", result)
	}

	// As in the gate, every record must hold: a pass beside a blocked record is
	// a blocked result, whichever comes first.
	blockedLast := inSummary(func(summary map[string]any) {
		extra := maps.Clone(scenarioNamed(t, summary, s03, abrupt))
		extra[fieldResult], extra[fieldReason] = string(Blocked), testReason
		summary[fieldScenarios] = append(summary[fieldScenarios].([]any), extra)
	})
	blockedFirst := func(t *testing.T, dir string) {
		blockedLast(t, dir)
		inSummary(func(summary map[string]any) { slices.Reverse(summary[fieldScenarios].([]any)) })(t, dir)
	}
	for _, change := range []bundleChange{blockedLast, blockedFirst} {
		got := CompareRuns(readChanged(t, registry, version2, kindRun), readChanged(t, registry, version2, kindRun, change))
		if result := resultWithKey(t, got, abruptKey); result.NewVerdict != Blocked {
			t.Errorf("result = %+v, want it blocked", result)
		}
	}
}

// S06/restart is a Kind test and a lab test. Results of two levels are never
// compared, so neither reads as the other, and as nothing added or removed.
func TestCompareRunsRefusesRunsOfTwoLevels(t *testing.T) {
	got := CompareRuns(readChanged(t, registry, version2, kindRun), readChanged(t, registry, version2, labRestartRun))
	if got.Matches() {
		t.Error("runs of two levels matched")
	}
	if len(got.Results) != 0 || !comparisonSays(got, "level") {
		t.Errorf("results = %+v, problems = %q; want no results and the reason", got.Results, got.Problems)
	}
}

// A filtered run proves only what it selected, so it never matches the full
// lane, and what it did not select stays not run. Two lab lanes share results,
// but a run of one is no run of the other.
func TestCompareRunsDoesNotMatchRunsOfTwoLanes(t *testing.T) {
	filtered := []bundleChange{inSummary(func(summary map[string]any) { summary[fieldLane] = filteredKindLane }),
		abruptResult("not_run")}
	got := CompareRuns(readChanged(t, registry, version2, kindRun),
		readChanged(t, registry, version2, kindRun, filtered...))
	if got.Matches() {
		t.Error("a filtered run matched the full lane")
	}
	if !comparisonSays(got, filteredKindLane) {
		t.Errorf("problems = %q, want the lanes named", got.Problems)
	}
	if result := resultWithKey(t, got, abruptKey); result.Change != ScenarioNotRun {
		t.Errorf("result = %+v, want it not run", result)
	}
	onlyLane := filtered[:1:1]
	missing := CompareRuns(readChanged(t, registry, version2, kindRun, onlyLane...),
		readChanged(t, registry, version2, kindRun, append(onlyLane, abruptRemoved())...))
	if result := resultWithKey(t, missing, abruptKey); result.Change != ScenarioMissing {
		t.Errorf("a result the Kind suite requires and a filtered run lacks = %+v, want it missing", result)
	}

	shorter := inSummary(func(summary map[string]any) { summary[fieldLane] = "lab-installed-ha" })
	wider := readChanged(t, registry, version2, labRestartRun)
	narrower := readChanged(t, registry, version2, labRestartRun, shorter)
	if !wider.Validated() || !narrower.Validated() {
		t.Fatalf("problems: %q and %q, want two runs that each prove their own lane", wider.Problems, narrower.Problems)
	}
	if got := CompareRuns(wider, narrower); got.Matches() || !comparisonSays(got, "lane lab-installed-ha") {
		t.Errorf("runs of two lab lanes: problems = %q, want the lanes named", got.Problems)
	}
}

// A run that did not finish, that failed its lane or whose junit.xml is another
// run's proves nothing to compare with, whatever its results say.
func TestCompareRunsNeverMatchesARunThatProvesNothing(t *testing.T) {
	tests := map[string]bundleChange{
		"a checkpoint": inSummary(func(summary map[string]any) { delete(summary, "finishedAt") }),
		"a run that says it failed": inSummary(func(summary map[string]any) {
			summary["passed"] = false
		}),
		"junit.xml of another run": inJUnit(`<testsuite name="2026-11-07T01-00-38Z"`, `<testsuite name="another"`),
		"a case with another name": inJUnit(`name="S03/abrupt"`, `name="S03/renamed-silently"`),
		"no junit.xml": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, junitFile)); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			good, bad := readChanged(t, registry, version2, kindRun), readChanged(t, registry, version2, kindRun, change)
			if CompareRuns(good, bad).Matches() || CompareRuns(bad, good).Matches() {
				t.Error("a run that proves nothing matched")
			}
			if got := CompareRuns(good, bad); !comparisonSays(got, "new run") {
				t.Errorf("problems = %q, want the new run named", got.Problems)
			}
		})
	}
}

// An unknown value is not zero and not equal to itself: a measurement that was
// not taken, or an environment value nobody recorded, stays unknown beside any
// other. A value one run lacks is unknown too.
func TestCompareRunsKeepsUnknownApartFromZero(t *testing.T) {
	measured := func(value any) bundleChange {
		return inAbrupt(func(scenario map[string]any) {
			scenario[fieldMeasured].(map[string]any)["acknowledgedMissing"] = value
		})
	}
	unmeasured := inAbrupt(func(scenario map[string]any) {
		delete(scenario[fieldMeasured].(map[string]any), "acknowledgedMissing")
	})
	tests := map[string]struct {
		older, newer bundleChange
		state        ValueState
	}{
		"zero and unknown":   {measured(0), measured(Unknown), ValueUnknown},
		"unknown and zero":   {measured(Unknown), measured(0), ValueUnknown},
		"unknown in both":    {measured(Unknown), measured(Unknown), ValueUnknown},
		"zero and not taken": {measured(0), unmeasured, ValueUnknown},
		"zero in both":       {measured(0), measured(0), ValueSame},
		"zero and one":       {measured(0), measured(1), ValueChanged},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := CompareRuns(readChanged(t, registry, version2, kindRun, tc.older),
				readChanged(t, registry, version2, kindRun, tc.newer))
			value := comparedValue(t, resultWithKey(t, got, abruptKey).Values, "measurements.acknowledgedMissing")
			if value.State != tc.state {
				t.Errorf("value = %+v, want %s", value, tc.state)
			}
		})
	}

	noOperations := inAbrupt(func(scenario map[string]any) { delete(scenario, fieldOperation) })
	got := CompareRuns(readChanged(t, registry, version2, kindRun),
		readChanged(t, registry, version2, kindRun, noOperations))
	value := comparedValue(t, resultWithKey(t, got, abruptKey).Values, "operations.unknown")
	if value.State != ValueUnknown || value.Old != "0" || value.New != Unknown {
		t.Errorf("operations not recorded = %+v, want unknown, not 0", value)
	}

	unknownImage := inSummary(func(summary map[string]any) {
		summary["environment"].(map[string]any)["operatorImageDigest"] = Unknown
	})
	got = CompareRuns(readChanged(t, registry, version2, kindRun, unknownImage),
		readChanged(t, registry, version2, kindRun, unknownImage))
	if value := comparedValue(t, got.Environment, "operatorImageDigest"); value.State != ValueUnknown {
		t.Errorf("an image digest unknown in both = %+v, want unknown", value)
	}
	got = CompareRuns(readChanged(t, registry, version2, kindRun), readChanged(t, registry, version2, kindRun))
	if value := comparedValue(t, got.Environment, "operatorCommit"); value.State != ValueSame {
		t.Errorf("one commit in both = %+v, want same", value)
	}
}

// A pass under another limit, or without the fault the old run injected, is a
// different check under the same key, and must not read as the same baseline.
func TestCompareRunsDoesNotTakeAChangedCheckForTheSameBaseline(t *testing.T) {
	tests := map[string]bundleChange{
		"a longer limit": inAbrupt(func(scenario map[string]any) {
			scenario[fieldLimits].(map[string]any)[failoverLimit] = "5m0s"
		}),
		"an unset limit": inAbrupt(func(scenario map[string]any) {
			scenario[fieldLimits].(map[string]any)[failoverLimit] = "unset"
		}),
		"a limit no longer recorded": inAbrupt(func(scenario map[string]any) {
			delete(scenario[fieldLimits].(map[string]any), failoverLimit)
		}),
		"no fault injected": inAbrupt(func(scenario map[string]any) { delete(scenario, fieldFault) }),
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			newer := readChanged(t, registry, version2, kindRun, change)
			if !newer.Validated() {
				t.Fatalf("the changed run must stay valid on its own: %q", newer.Problems)
			}
			got := CompareRuns(readChanged(t, registry, version2, kindRun), newer)
			if got.Matches() {
				t.Error("a changed check read as the same baseline")
			}
			if result := resultWithKey(t, got, abruptKey); result.Change != ScenarioUnchanged || result.Problem == "" {
				t.Errorf("result = %+v, want the same identity with a problem", result)
			}
		})
	}
}

// A comparison depends on the runs and not on the order of a map: results come
// sorted by key, with those without one last, and two comparisons are equal.
func TestCompareRunsIsDeterministic(t *testing.T) {
	older := readChanged(t, registry, version2, kindRun)
	newer := readChanged(t, registry, version2, kindRun, abruptRenamedInBothFiles())
	first := CompareRuns(older, newer)
	for range 20 {
		if again := CompareRuns(older, newer); !reflect.DeepEqual(first, again) {
			t.Fatal("two comparisons of one pair differ")
		}
	}
	keys := make([]string, 0, len(first.Results))
	for _, result := range first.Results {
		if result.Key != "" {
			keys = append(keys, result.Key)
		}
	}
	if !slices.IsSorted(keys) {
		t.Errorf("keys = %q, want them sorted", keys)
	}
	if last := first.Results[len(first.Results)-1]; last.Key != "" {
		t.Errorf("the last result = %+v, want one without a key", last)
	}
}

// Nothing compared is no match.
func TestAnEmptyComparisonDoesNotMatch(t *testing.T) {
	if (Comparison{}).Matches() {
		t.Error("the zero comparison matched")
	}
	if got := CompareRuns(RecordedRun{}, RecordedRun{}); got.Matches() {
		t.Error("two empty runs matched")
	}
}
