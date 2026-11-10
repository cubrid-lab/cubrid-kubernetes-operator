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
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Change says what became of one result between two runs. It is about the
// identity of the result; the verdicts of a ComparedResult say how it went.
type Change string

const (
	// ScenarioUnchanged is one key under one legacy name in both runs.
	ScenarioUnchanged Change = "unchanged"
	// ScenarioRenamed is one key under two legacy names that the registry
	// holds as aliases of that key. Nothing else shows a rename.
	ScenarioRenamed Change = "renamed"
	// ScenarioAdded is a result only the new run holds.
	ScenarioAdded Change = "added"
	// ScenarioRemoved is a result only the old run holds, which no lane of the
	// two runs requires.
	ScenarioRemoved Change = "removed"
	// ScenarioNotRun is a result a run recorded as not run, so the test still
	// exists and that run says nothing of it.
	ScenarioNotRun Change = "not_run"
	// ScenarioMissing is a result a lane of the two runs requires and the new
	// run, or both runs, hold no record of.
	ScenarioMissing Change = "missing"
	// ScenarioUnresolved is a result with no single identity, which is compared
	// with nothing.
	ScenarioUnresolved Change = "unresolved"
)

// ValueState says how a recorded value of one run stands to that of the other.
type ValueState string

const (
	// ValueSame is one known value in both runs.
	ValueSame ValueState = "same"
	// ValueChanged is two known values.
	ValueChanged ValueState = "changed"
	// ValueUnknown is a value at least one run does not know. It is never the
	// same as anything, not even as Unknown in the other run, and never zero.
	ValueUnknown ValueState = "unknown"
)

// ComparedValue is one value as the two runs recorded it, with Unknown for a
// value a run did not record.
type ComparedValue struct {
	Name  string
	Old   string
	New   string
	State ValueState
}

// ComparedResult is one result across two runs. Old and New are the records as
// ReadRun returned them, with the evidence paths of their own run. A verdict is
// the worst judged result of a run's records, and "" when it holds none. Key is
// "" for a result that resolved to no key, which is one record of one run.
// Values holds what was measured and the limits judged by, when each run has
// one record to pair. Problem says why this result stops the runs from
// matching, and is "" when it is proven in both and is the same check.
type ComparedResult struct {
	Key         string
	DisplayName string
	Change      Change
	Old         []RecordedResult
	New         []RecordedResult
	OldVerdict  Result
	NewVerdict  Result
	Values      []ComparedValue
	Problem     string
}

// Comparison is two runs set side by side. Old and New are the runs as read,
// with their directories and the digests of their source files. Results has one
// entry per key either run holds or either lane requires, sorted by key, then
// one for each record without a key. Environment shows what each run ran on and
// decides nothing: two runs of a test usually differ there. Problems says why
// the two runs do not match, one line per problem.
type Comparison struct {
	Old         RecordedRun
	New         RecordedRun
	Environment []ComparedValue
	Results     []ComparedResult
	Problems    []string
}

// Matches reports whether both runs validated one lane at one level and every
// result of either is the same check, proven by a pass in both. It says nothing
// of the environment or of measurements, which ComparedValue shows.
func (c Comparison) Matches() bool { return len(c.Problems) == 0 && len(c.Results) > 0 }

// CompareRuns sets two runs read by ReadRun side by side and writes nothing.
// Results are paired by the key each resolved to under its own run's lane and
// level, never by a name for people, and results of two levels are not paired
// at all. Anything unproven stays what it is: a missing, skipped, blocked,
// failed or evidence-less result is reported and the runs do not match, also
// when both runs agree on it.
func CompareRuns(older, newer RecordedRun) Comparison { return registry.compareRuns(older, newer) }

func (r scenarioRegistry) compareRuns(older, newer RecordedRun) Comparison {
	c := Comparison{Old: older, New: newer,
		Problems: slices.Concat(runProblems("old", older), runProblems("new", newer))}
	oldEnv, newEnv := older.Summary.Environment, newer.Summary.Environment
	if oldEnv.Level != newEnv.Level {
		c.Problems = append(c.Problems, fmt.Sprintf(
			"the old run is at level %s and the new run at level %s: a result of one level never stands for one of another",
			oldEnv.Level, newEnv.Level))
		return c
	}
	if oldLane, newLane := older.Summary.Lane, newer.Summary.Lane; oldLane != newLane {
		c.Problems = append(c.Problems, fmt.Sprintf("the old run is of lane %s and the new run of lane %s", oldLane, newLane))
	}
	c.Environment = compareEnvironment(oldEnv, newEnv)
	oldKeyed, oldLoose := byKey(older.Results)
	newKeyed, newLoose := byKey(newer.Results)
	required := r.requiredKeys(older.Summary.Lane, newer.Summary.Lane)
	keys := slices.Concat(required, slices.Collect(maps.Keys(oldKeyed)), slices.Collect(maps.Keys(newKeyed)))
	slices.Sort(keys)
	for _, key := range slices.Compact(keys) {
		c.add(r.compareKey(key, oldKeyed[key], newKeyed[key], slices.Contains(required, key)))
	}
	for _, record := range oldLoose {
		c.add(unresolvedRecord("old", record, []RecordedResult{record}, nil))
	}
	for _, record := range newLoose {
		c.add(unresolvedRecord("new", record, nil, []RecordedResult{record}))
	}
	if len(c.Results) == 0 {
		c.Problems = append(c.Problems, "no result was compared")
	}
	return c
}

func (c *Comparison) add(result ComparedResult) {
	c.Results = append(c.Results, result)
	if result.Problem != "" {
		name := result.Key
		if name == "" {
			name = result.DisplayName
		}
		c.Problems = append(c.Problems, name+": "+result.Problem)
	}
}

// runProblems says why a run proves nothing to compare with.
func runProblems(side string, run RecordedRun) []string {
	problems := make([]string, 0, len(run.Problems))
	for _, problem := range run.Problems {
		problems = append(problems, fmt.Sprintf("the %s run %s: %s", side, run.Summary.RunID, problem))
	}
	return problems
}

// byKey groups the records of a run by key, and returns those without one.
func byKey(results []RecordedResult) (map[string][]RecordedResult, []RecordedResult) {
	keyed := map[string][]RecordedResult{}
	var loose []RecordedResult
	for _, result := range results {
		if result.Key == "" {
			loose = append(loose, result)
			continue
		}
		keyed[result.Key] = append(keyed[result.Key], result)
	}
	return keyed, loose
}

// requiredKeys returns what the lanes require as they are registered today. A
// filtered run is read as the Kind suite's, so what it left out stays required.
func (r scenarioRegistry) requiredKeys(names ...string) []string {
	var keys []string
	for _, name := range names {
		if name == filteredKindLane {
			name = kindLaneName
		}
		if lane, ok := r.lane(name); ok {
			keys = append(keys, lane.Keys...)
		}
	}
	return keys
}

// unresolvedRecord is a record with no key. ReadRun has said why; two such
// records are never paired, even under one legacy name.
func unresolvedRecord(side string, record RecordedResult, older, newer []RecordedResult) ComparedResult {
	result := comparedResult(older, newer)
	result.DisplayName, result.Change = record.Scenario.Name(), ScenarioUnresolved
	result.Problem = fmt.Sprintf("in the %s run it resolves to no single registered result, so nothing is compared",
		side)
	return result
}

func comparedResult(older, newer []RecordedResult) ComparedResult {
	return ComparedResult{Old: older, New: newer, OldVerdict: verdict(older), NewVerdict: verdict(newer)}
}

// verdictOrder ranks the results from the best to the worst.
var verdictOrder = []Result{Pass, NotApplicable, NotRun, Blocked, Fail}

// worstRecord returns the record that decides a run's verdict on a result: as
// in Gate, every record of a result must hold.
func worstRecord(records []RecordedResult) RecordedResult {
	return slices.MaxFunc(records, func(a, b RecordedResult) int {
		return slices.Index(verdictOrder, a.Judged) - slices.Index(verdictOrder, b.Judged)
	})
}

func verdict(records []RecordedResult) Result {
	if len(records) == 0 {
		return ""
	}
	return worstRecord(records).Judged
}

func legacyNames(records []RecordedResult) []string {
	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Scenario.Name())
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// compareKey compares what each run holds of one key.
func (r scenarioRegistry) compareKey(key string, older, newer []RecordedResult, required bool) ComparedResult {
	result := comparedResult(older, newer)
	result.Key = key
	entry, err := r.resolveKey(key)
	if err != nil {
		result.Change, result.Problem = ScenarioUnresolved, err.Error()
		return result
	}
	result.DisplayName = entry.Name
	oldNames, newNames := legacyNames(older), legacyNames(newer)
	for _, name := range slices.Concat(oldNames, newNames) {
		if !slices.Contains(entry.Aliases, name) {
			result.Change = ScenarioUnresolved
			result.Problem = fmt.Sprintf("the registry does not hold %s as a legacy name of this key", name)
			return result
		}
	}
	if len(oldNames) > 1 || len(newNames) > 1 {
		result.Change = ScenarioUnresolved
		result.Problem = fmt.Sprintf("a run holds it under more than one legacy name: %s and %s",
			strings.Join(oldNames, ", "), strings.Join(newNames, ", "))
		return result
	}
	result.Change = classify(result, oldNames, newNames, required)
	problems := []string{}
	if line := absenceProblem(result.Change, older, newer); line != "" {
		problems = append(problems, line)
	}
	for _, side := range []struct {
		name    string
		records []RecordedResult
	}{{"old", older}, {"new", newer}} {
		if line := verdictProblem(side.name, side.records); line != "" {
			problems = append(problems, line)
		}
	}
	values, lines := compareRecords(older, newer)
	result.Values = values
	result.Problem = strings.Join(append(problems, lines...), "; ")
	return result
}

// classify tells apart what can happen to one key. A result recorded as not run
// is not_run before anything else, so that a placeholder never reads as an
// added test; and a result a lane requires is never removed, only missing.
func classify(result ComparedResult, oldNames, newNames []string, required bool) Change {
	switch {
	case len(oldNames) == 0 && len(newNames) == 0:
		return ScenarioMissing
	case result.OldVerdict == NotRun || result.NewVerdict == NotRun:
		return ScenarioNotRun
	case len(newNames) == 0 && required:
		return ScenarioMissing
	case len(newNames) == 0:
		return ScenarioRemoved
	case len(oldNames) == 0:
		return ScenarioAdded
	case oldNames[0] != newNames[0]:
		return ScenarioRenamed
	}
	return ScenarioUnchanged
}

// absenceProblem says which run lacks a result.
func absenceProblem(change Change, older, newer []RecordedResult) string {
	switch {
	case change == ScenarioMissing && len(older) == 0 && len(newer) == 0:
		return "neither run holds a result that a lane requires"
	case change == ScenarioMissing:
		return "the new run holds no result, and a lane requires it"
	case change == ScenarioRemoved:
		return "only the old run holds a result, and no lane requires it"
	case change == ScenarioAdded:
		return "only the new run holds a result"
	case len(older) == 0:
		return "the old run holds no result"
	case len(newer) == 0:
		return "the new run holds no result"
	}
	return ""
}

// verdictProblem says why a run's records do not prove a result. A pass is what
// its files support today, not what the run claimed.
func verdictProblem(side string, records []RecordedResult) string {
	if len(records) == 0 {
		return ""
	}
	worst := worstRecord(records)
	if worst.Judged == Pass {
		return ""
	}
	return fmt.Sprintf("the %s run has %s: %s", side, worst.Judged, worst.Reason)
}

// compareRecords pairs the records of two runs. It returns the values side by
// side, and why the two are not the same check under one key: a limit or the
// fault is part of what a pass means, a measurement is not.
func compareRecords(older, newer []RecordedResult) ([]ComparedValue, []string) {
	if len(older) == 0 || len(newer) == 0 {
		return nil, nil
	}
	if len(older) > 1 || len(newer) > 1 {
		return nil, []string{fmt.Sprintf(
			"recorded %d times in the old run and %d in the new run, so the records cannot be paired",
			len(older), len(newer))}
	}
	before, after := older[0].Scenario, newer[0].Scenario
	var problems []string
	if a, b := faultText(before), faultText(after); a != b {
		problems = append(problems, fmt.Sprintf("the fault was %s in the old run and %s in the new run", a, b))
	}
	limits := compareMaps("limits.", before.Limits, after.Limits)
	for _, limit := range limits {
		if limit.State != ValueSame {
			problems = append(problems, fmt.Sprintf("judged by %s %s in the old run and %s in the new run",
				limit.Name, limit.Old, limit.New))
		}
	}
	values := slices.Concat(limits, compareMaps("measurements.", before.Measurements, after.Measurements),
		compareMaps("operations.", operationCounts(before.Operations), operationCounts(after.Operations)))
	return values, problems
}

func faultText(sc Scenario) string {
	switch {
	case sc.FaultConfirmed == nil:
		return "not part of the check"
	case *sc.FaultConfirmed:
		return "confirmed"
	}
	return "not confirmed"
}

func compareMaps[V any](prefix string, older, newer map[string]V) []ComparedValue {
	names := slices.Concat(slices.Collect(maps.Keys(older)), slices.Collect(maps.Keys(newer)))
	slices.Sort(names)
	values := make([]ComparedValue, 0, len(names))
	for _, name := range slices.Compact(names) {
		a, inOld := older[name]
		b, inNew := newer[name]
		values = append(values, compareValue(prefix+name, recorded(a, inOld), recorded(b, inNew)))
	}
	return values
}

// recorded is the text of a value, and Unknown for one that was not recorded.
func recorded(value any, ok bool) string {
	if !ok || value == nil {
		return Unknown
	}
	return fmt.Sprint(value)
}

func compareValue(name, older, newer string) ComparedValue {
	value := ComparedValue{Name: name, Old: older, New: newer, State: ValueChanged}
	for _, side := range []*string{&value.Old, &value.New} {
		if *side == "" {
			*side = Unknown
		}
	}
	switch {
	case value.Old == Unknown || value.New == Unknown:
		value.State = ValueUnknown
	case value.Old == value.New:
		value.State = ValueSame
	}
	return value
}

// operationCounts is the counts of a run's client operations by name. A run that
// kept none has no names, so each reads as Unknown beside the other run's, not
// as zero.
func operationCounts(ops *Operations) map[string]int {
	if ops == nil {
		return nil
	}
	return map[string]int{"attempted": ops.Attempted, "acknowledged": ops.Acknowledged,
		"failed": ops.Failed, "unknown": ops.Unknown}
}

func compareEnvironment(older, newer Environment) []ComparedValue {
	return []ComparedValue{
		compareValue("kubernetesVersion", older.KubernetesVersion, newer.KubernetesVersion),
		compareValue("operatorCommit", older.OperatorCommit, newer.OperatorCommit),
		compareValue("operatorImageDigest", older.OperatorImageDigest, newer.OperatorImageDigest),
		compareValue("cubridImageDigest", older.CubridImageDigest, newer.CubridImageDigest),
		compareValue("engineVersion", older.EngineVersion, newer.EngineVersion),
		compareValue("clientDriver", older.ClientDriver, newer.ClientDriver),
	}
}
