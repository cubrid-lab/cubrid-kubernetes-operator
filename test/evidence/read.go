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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	summaryFile = "summary.json"
	junitFile   = "junit.xml"
	// schemaV1 and schemaV2 are the versions of summary.json the reader knows.
	// They are not SchemaVersion: a writer that moves on must not make the
	// reader accept a format no one taught it.
	schemaV1 = "1"
	schemaV2 = "2"
	// junitScenarioClass is the class of a scenario's case in junit.xml.
	junitScenarioClass = "scenario"
)

// RecordedResult is one entry of summary.json as a reader sees it now. What
// the run recorded stays in Scenario; Judged and Reason are what its files
// support today, which is less than it claimed when evidence was not kept. Key
// is the canonical key in the lane and level the run recorded, and "" when
// that resolves to no single result. RecordedKey is what a version 2 result
// recorded of itself, kept even when it is refused, and "" in version 1.
// DisplayName is what version 2 recorded, or for a version 1 result the
// registry's name under Key; it is for people, and nothing matches on it.
type RecordedResult struct {
	Scenario    Scenario
	RecordedKey string
	Key         string
	DisplayName string
	Judged      Result
	Reason      string
}

// RecordedRun is a run read back from its directory. Dir and the digests are
// the reference to the unmodified source files. Summary is what the run
// recorded, including its own verdict, and nothing here replaces it.
type RecordedRun struct {
	Dir           string
	SummarySHA256 string
	// JUnitSHA256 is "" when junit.xml could not be read.
	JUnitSHA256 string
	Summary     Summary
	// Results has one entry for each of Summary.Scenarios, in the same order,
	// with the key and display name a version 2 file recorded.
	Results []RecordedResult
	// Provisional marks a run that did not finish: a checkpoint of one that
	// was stopped or killed.
	Provisional bool
	// Problems says why the run does not prove the registered lane it names,
	// as that lane stands today, one line per problem.
	Problems []string
}

// Validated reports whether the run proves the lane it recorded. It does not
// say what the run recorded of itself: an old run that passed the requirements
// of its day stays passed in Summary, and is not validated when the lane now
// requires more or its evidence was not kept.
func (r RecordedRun) Validated() bool { return len(r.Problems) == 0 }

// ReadRun reads the results of a finished or unfinished run from its directory
// and writes nothing. It returns an error only for a summary.json it cannot
// interpret: a schema version it does not know is refused rather than guessed
// at. Any other defect is left readable and reported in Problems, and each
// result resolves only under the lane and environment level the run recorded.
func ReadRun(dir string) (RecordedRun, error) { return registry.readRun(dir) }

func (r scenarioRegistry) readRun(dir string) (RecordedRun, error) {
	data, err := os.ReadFile(filepath.Join(dir, summaryFile))
	if err != nil {
		return RecordedRun{}, fmt.Errorf("could not read the run's %s: %w", summaryFile, err)
	}
	summary, identities, err := decodeSummary(data)
	if err != nil {
		return RecordedRun{}, fmt.Errorf("could not interpret %s in %s: %w", summaryFile, dir, err)
	}
	run := RecordedRun{Dir: dir, SummarySHA256: sha256Hex(data), Summary: summary,
		Provisional: summary.FinishedAt.IsZero()}
	for i, sc := range summary.Scenarios {
		judged, reason := sc.Judge(dir)
		result := RecordedResult{Scenario: sc, Judged: judged, Reason: reason}
		if identities != nil {
			result.RecordedKey, result.DisplayName = identities[i].Key, identities[i].DisplayName
		}
		run.Results = append(run.Results, result)
	}
	run.checkRecord()
	run.checkJUnit()
	r.checkLane(&run)
	return run, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// decodeSummary reads a summary of a version it knows, and for version 2 the
// entries with what each recorded of its identity. A field its version never
// had is refused as well, because reading on would lose it without a trace, so
// a key in a version 1 file is refused.
func decodeSummary(data []byte) (Summary, []scenarioV2, error) {
	var version struct {
		SchemaVersion *string `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return Summary{}, nil, err
	}
	switch {
	case version.SchemaVersion == nil:
		return Summary{}, nil, errors.New("it records no schema version")
	case *version.SchemaVersion == schemaV1:
		var summary Summary
		err := decodeStrict(data, &summary)
		return summary, nil, err
	case *version.SchemaVersion == schemaV2:
		var versioned summaryV2
		if err := decodeStrict(data, &versioned); err != nil {
			return Summary{}, nil, err
		}
		summary := versioned.Summary
		summary.Scenarios = make([]Scenario, 0, len(versioned.Scenarios))
		for _, sc := range versioned.Scenarios {
			summary.Scenarios = append(summary.Scenarios, sc.Scenario)
		}
		return summary, versioned.Scenarios, nil
	}
	return Summary{}, nil, fmt.Errorf("it has schema version %q, which this reader does not know", *version.SchemaVersion)
}

// decodeStrict refuses a field the target does not have, and a measurement
// keeps its exact digits.
func decodeStrict(data []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// checkRecord compares what the summary says of the run as a whole.
func (r *RecordedRun) checkRecord() {
	s := r.Summary
	if s.RunID == "" {
		r.Problems = append(r.Problems, "the run records no run ID")
	}
	if s.StartedAt.IsZero() {
		r.Problems = append(r.Problems, "the run records no start time")
	}
	if r.Provisional {
		r.Problems = append(r.Problems, "the run did not finish: this is what it recorded before it stopped")
	}
	switch {
	case !s.Passed:
		r.Problems = append(r.Problems, "the run recorded that it did not pass lane "+s.Lane)
	case len(s.Problems) > 0:
		r.Problems = append(r.Problems, "the run claims a pass but lists problems: "+strings.Join(s.Problems, "; "))
	}
}

// junitResult is the verdict a JUnit case carries.
func (c junitCase) junitResult() Result {
	switch {
	case c.Failure != nil:
		return Fail
	case c.Error != nil:
		return Blocked
	case c.Skipped != nil:
		return NotRun
	}
	return Pass
}

// junitLine names a case and its verdict in a problem.
func (c junitCase) junitLine() string { return c.Name + ": " + string(c.junitResult()) }

// junitKey is the canonical key a case records, or "" for a file written before
// cases recorded one.
func (c junitCase) junitKey() string {
	for _, property := range c.Properties {
		if property.Name == junitKeyProperty {
			return property.Value
		}
	}
	return ""
}

// checkJUnit compares junit.xml with summary.json. Both come from one write of
// one run, so they name the same run and hold the same scenarios in the same
// order with the same verdicts; anything else is a mixed or edited bundle. The
// case name is the identity in schema versions 1 and 2, and the system-out text
// and the messages are not compared. A case is not looked up by a looser name, so
// that a renamed one shows up here and not as a check that went missing. A
// case maps to its key through the entry of summary.json at the same place, and
// a key the case records itself must be the one that entry recorded.
func (r *RecordedRun) checkJUnit() {
	data, err := os.ReadFile(filepath.Join(r.Dir, junitFile))
	if err != nil {
		r.Problems = append(r.Problems, fmt.Sprintf("%s could not be read: %v", junitFile, err))
		return
	}
	r.JUnitSHA256 = sha256Hex(data)
	var suite junitSuite
	if err := xml.Unmarshal(data, &suite); err != nil {
		r.Problems = append(r.Problems, fmt.Sprintf("%s could not be read: %v", junitFile, err))
		return
	}
	if suite.Name != r.Summary.RunID {
		r.Problems = append(r.Problems, fmt.Sprintf("%s belongs to run %s, not %s", junitFile, suite.Name, r.Summary.RunID))
	}
	var got []junitCase
	for _, c := range suite.Cases {
		switch {
		case c.Classname == junitScenarioClass:
			got = append(got, c)
		case c.Classname != junitRunCase:
			r.Problems = append(r.Problems, fmt.Sprintf("%s case %s has the class %s", junitFile, c.Name, c.Classname))
		case r.Summary.Passed:
			r.Problems = append(r.Problems, junitFile+" has a run case, so the run did not pass")
		}
	}
	want := r.Summary.junit().Cases[:len(r.Summary.Scenarios)]
	if len(got) != len(want) {
		r.Problems = append(r.Problems, fmt.Sprintf("%s has %d scenario cases and %s %d",
			junitFile, len(got), summaryFile, len(want)))
	}
	for i := range min(len(got), len(want)) {
		if got[i].junitLine() != want[i].junitLine() {
			r.Problems = append(r.Problems, fmt.Sprintf("%s has %s where %s has %s",
				junitFile, got[i].junitLine(), summaryFile, want[i].junitLine()))
		}
		if key := got[i].junitKey(); key != "" && key != r.Results[i].RecordedKey {
			r.Problems = append(r.Problems, fmt.Sprintf("%s case %s has the key %s where %s has %q",
				junitFile, got[i].Name, key, summaryFile, r.Results[i].RecordedKey))
		}
	}
}

// checkLane resolves each result under the lane and level the run recorded and
// judges the run with Gate against that registered lane, so a missing, failed,
// blocked, unproven or not_run required result reads as it does in a run. Gate
// matches a result to a requirement by the key both resolve to, so a result
// whose legacy name does not resolve counts for no requirement, and every
// record of a required key must pass, under any of its legacy names; two names
// of one key in one run are also reported. A result never stands for another
// requirement. A version 2 result must also record the key the registry gives
// it, so a key cannot move a verdict to another result.
func (r scenarioRegistry) checkLane(run *RecordedRun) {
	name, level := run.Summary.Lane, run.Summary.Environment.Level
	if err := r.checkLaneLevel(name, level); err != nil {
		run.Problems = append(run.Problems, err.Error())
		return
	}
	recorded := map[string][]string{}
	for i := range run.Results {
		result := &run.Results[i]
		alias := result.Scenario.Name()
		entry, err := r.resolveResult(name, level, result.Scenario)
		if err == nil && run.Summary.SchemaVersion == schemaV2 {
			err = result.checkRecordedIdentity(entry.Key)
		}
		if err != nil {
			run.Problems = append(run.Problems, err.Error())
			continue
		}
		result.Key = entry.Key
		if result.DisplayName == "" {
			result.DisplayName = entry.Name
		}
		switch names := recorded[entry.Key]; {
		case len(names) == 0:
			recorded[entry.Key] = []string{alias}
		case names[0] != alias:
			run.Problems = append(run.Problems, fmt.Sprintf("%s and %s are both %s", names[0], alias, entry.Key))
		}
	}
	if name == filteredKindLane {
		run.Problems = append(run.Problems, fmt.Sprintf(
			"lane %s selected its own requirements, so it validates no registered lane", name))
		return
	}
	lane, ok := r.lane(name)
	if !ok {
		run.Problems = append(run.Problems, fmt.Sprintf("lane %s is not registered", name))
		return
	}
	gate := Lane{Name: lane.Name}
	for _, key := range lane.Keys {
		entry, err := r.resolveKey(key)
		if err != nil || len(entry.Aliases) == 0 {
			run.Problems = append(run.Problems, fmt.Sprintf("lane %s: key %s has no legacy name", name, key))
			continue
		}
		// The gate matches by key, so any legacy name of the key requires
		// every record the run holds of it, under whichever name.
		id, variant, _ := strings.Cut(entry.Aliases[0], "/")
		gate.Required = append(gate.Required, Requirement{ID: id, Variant: variant})
	}
	run.Problems = append(run.Problems, r.gate(gate, run.Summary, run.Dir)...)
}

// checkRecordedIdentity compares what a version 2 result recorded of itself
// with the key the registry gives its legacy name. The display name is not
// compared: its wording may improve, and an earlier run keeps the words it was
// written with.
func (r RecordedResult) checkRecordedIdentity(key string) error {
	alias := r.Scenario.Name()
	switch {
	case r.RecordedKey == "":
		return fmt.Errorf("%s records no key", alias)
	case r.RecordedKey != key:
		return fmt.Errorf("%s is recorded with the key %s, not %s", alias, r.RecordedKey, key)
	case r.DisplayName == "":
		return fmt.Errorf("%s records no display name", alias)
	}
	return nil
}
