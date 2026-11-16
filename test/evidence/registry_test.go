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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testFamilyKey = "operator-recovery"
	testKindKey   = "operator-recovery-restart"
	testLabKey    = "installed-operator-recovery-restart"
	testBaseKey   = "database-smoke"
	testKindLane  = "kind"
	testLabLane   = "lab-operator"
	testBaseAlias = "S01"
	s06Label      = "S06-restart"
	testRestart   = "restart"
	testUnknown   = "ghost"
	testOther     = "Other"
	testNoRestart = "lane kind: registered key operator-recovery-restart has no requirement"
	testAlias     = "S06/restart"
	realLabLane   = "lab-installed-ha-s06"
	realKindKey   = "operator-failure-recovery-restart"
	realLabKey    = "installed-operator-failure-recovery-restart"
	contractPath  = "../../docs/testing/scenario-contract.md"
)

// testRegistry is a small valid registry. The same legacy name on two levels
// is allowed, because S06/restart is both a Kind test and a lab test.
func testRegistry() scenarioRegistry {
	return scenarioRegistry{
		families: []ScenarioFamily{
			{ID: testBaseAlias, Key: testBaseKey, Description: "Smoke", Contract: "s01-smoke"},
			{ID: s06, Key: testFamilyKey, Description: "Operator", Contract: "s06-operator"},
		},
		entries: []ScenarioEntry{
			{Key: testBaseKey, Name: "Smoke", Family: testBaseAlias, Level: LevelKind, Aliases: []string{testBaseAlias}},
			{Key: testKindKey, Name: "Operator on Kind", Family: s06, Level: LevelKind, Aliases: []string{testAlias}},
			{Key: testLabKey, Name: "Operator on the lab", Family: s06, Level: LevelVMLab, Aliases: []string{testAlias}},
		},
		lanes: []RequiredLane{
			{Name: testKindLane, Level: LevelKind, Keys: []string{testBaseKey, testKindKey}},
			{Name: testLabLane, Level: LevelVMLab, Keys: []string{testLabKey}},
		},
	}
}

func TestRegistryIsValid(t *testing.T) {
	if len(registry.families) == 0 || len(registry.entries) == 0 || len(registry.lanes) == 0 {
		t.Fatal("the registry is empty")
	}
	for _, problem := range registry.validate() {
		t.Error(problem)
	}
	if problems := testRegistry().validate(); len(problems) != 0 {
		t.Fatalf("the test registry must be valid: %q", problems)
	}
}

// Each change makes the registry wrong in one way, and validation must name
// that way: a registry that merely reports something would hide the others.
func TestValidateRejects(t *testing.T) {
	tests := map[string]struct {
		change func(r *scenarioRegistry)
		want   string
	}{
		"two families with one ID": {func(r *scenarioRegistry) { r.families[1].ID = testBaseAlias },
			"family S01: the ID is used twice"},
		"two families with one key": {func(r *scenarioRegistry) { r.families[1].Key = testBaseKey },
			"family S06: the key database-smoke is used twice"},
		"a family ID that is not a contract code": {func(r *scenarioRegistry) { r.families[1].ID = "s6" },
			"family s6: the ID is not of the form S00 or LAB00"},
		"a family key that is not a slug": {func(r *scenarioRegistry) { r.families[1].Key = "Operator_recovery" },
			"family S06: the key Operator_recovery is not a lower-case slug"},
		"a family with no description": {func(r *scenarioRegistry) { r.families[1].Description = "" },
			"family S06: no description"},
		"a family with no contract pointer": {func(r *scenarioRegistry) { r.families[1].Contract = "" },
			"family S06: no contract pointer"},
		"two results with one key": {func(r *scenarioRegistry) { r.entries[2].Key = testKindKey },
			"key operator-recovery-restart: used by two results"},
		"a result key that is not a slug": {func(r *scenarioRegistry) { r.entries[1].Key = s06Label },
			"key S06-restart: not a lower-case slug"},
		"a variant result named like its family": {func(r *scenarioRegistry) { r.entries[1].Key = testFamilyKey },
			"key operator-recovery: is the key of family S06, but the result is not that family's own"},
		"a result named like another family": {func(r *scenarioRegistry) { r.entries[1].Key = testBaseKey },
			"key database-smoke: used by two results"},
		"two results with one name": {func(r *scenarioRegistry) { r.entries[2].Name = "Operator on Kind" },
			"key installed-operator-recovery-restart: the name \"Operator on Kind\" is used twice"},
		"a result with no name": {func(r *scenarioRegistry) { r.entries[1].Name = "" },
			"key operator-recovery-restart: no readable name"},
		"a result of an unknown family": {func(r *scenarioRegistry) { r.entries[1].Family = "S99" },
			"key operator-recovery-restart: unknown family S99"},
		"a result of an unknown level": {func(r *scenarioRegistry) { r.entries[1].Level = "unit" },
			"key operator-recovery-restart: unknown level unit"},
		"a Kind result with the lab prefix": {func(r *scenarioRegistry) { r.entries[1].Key = "installed-restart" },
			"key installed-restart: a kind result must not start with installed-"},
		"a lab result without the lab prefix": {func(r *scenarioRegistry) { r.entries[2].Key = "lab-restart" },
			"key lab-restart: a vm-lab result must start with installed-"},
		"one alias for two results of a level": {
			func(r *scenarioRegistry) { r.entries[0].Aliases = []string{testAlias} },
			"alias S06/restart: names two kind results, operator-recovery-restart and database-smoke"},
		"one alias twice on a result": {
			func(r *scenarioRegistry) { r.entries[1].Aliases = []string{testAlias, testAlias} },
			"alias S06/restart: listed twice for operator-recovery-restart"},
		"an alias of another family": {
			func(r *scenarioRegistry) { r.entries[1].Aliases = []string{testBaseAlias + "/restart"} },
			"alias S01/restart: belongs to S01, not to the family S06 of operator-recovery-restart"},
		"an alias that is not a legacy name": {
			func(r *scenarioRegistry) { r.entries[1].Aliases = []string{s06Label} },
			"alias S06-restart: not of the form S00, S00/variant, LAB00 or LAB00/variant"},
		"two lanes with one name": {func(r *scenarioRegistry) { r.lanes[1].Name = testKindLane },
			"lane kind: registered twice"},
		"a lane of an unknown level": {func(r *scenarioRegistry) { r.lanes[1].Level = "unit" },
			"lane lab-operator: unknown level unit"},
		"a Kind lane with another name": {func(r *scenarioRegistry) { r.lanes[0].Name = "e2e" },
			"lane e2e: a kind lane must be named kind"},
		"a lab lane with another name": {func(r *scenarioRegistry) { r.lanes[1].Name = "operator" },
			"lane operator: a vm-lab lane must start with lab-"},
		"the filtered lane": {func(r *scenarioRegistry) { r.lanes[0].Name = filteredKindLane },
			"lane kind-filtered: a filtered run selects its own requirements and cannot be registered"},
		"a lane with no key": {func(r *scenarioRegistry) { r.lanes[1].Keys = nil },
			"lane lab-operator: requires nothing"},
		"a lane with an unknown key": {func(r *scenarioRegistry) { r.lanes[1].Keys = []string{testUnknown} },
			"lane lab-operator: unknown key ghost"},
		"a lane with a key twice": {func(r *scenarioRegistry) { r.lanes[1].Keys = []string{testLabKey, testLabKey} },
			"lane lab-operator: key installed-operator-recovery-restart is listed twice"},
		"a lane with a key of another level": {func(r *scenarioRegistry) { r.lanes[1].Keys = []string{testKindKey} },
			"lane lab-operator: key operator-recovery-restart is a kind result in a vm-lab lane"},
		"a required result no gate can match": {func(r *scenarioRegistry) { r.entries[2].Aliases = nil },
			"lane lab-operator: key installed-operator-recovery-restart has no legacy alias, which the gates match on"},
	}
	for name, tc := range tests {
		r := testRegistry()
		tc.change(&r)
		if got := r.validate(); !slices.Contains(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

func TestResolveKey(t *testing.T) {
	r := testRegistry()
	got, err := r.resolveKey(testLabKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != testLabKey || got.Level != LevelVMLab || got.Contract != contractDoc+"#s06-operator" ||
		!slices.Equal(got.Lanes, []string{testLabLane}) {
		t.Errorf("resolveKey = %+v", got)
	}
	for key, want := range map[string]string{
		testUnknown:   "unknown scenario key ghost",
		"":            "unknown scenario key ",
		testAlias:     "unknown scenario key S06/restart",
		testFamilyKey: "operator-recovery is the family of S06, not one result",
	} {
		if _, err := r.resolveKey(key); err == nil || err.Error() != want {
			t.Errorf("resolveKey(%q) = %v, want %q", key, err, want)
		}
	}
	// A result that is its family's only one keeps the family's key.
	if got, err := r.resolveKey(testBaseKey); err != nil || got.Key != testBaseKey {
		t.Errorf("resolveKey(%q) = %+v, %v", testBaseKey, got, err)
	}
}

// One legacy name is two tests: the lane and the level say which, and neither
// falls back to the other's result.
func TestResolveAlias(t *testing.T) {
	r := testRegistry()
	for _, tc := range []struct{ lane, level, want string }{
		{testKindLane, LevelKind, testKindKey},
		{filteredKindLane, LevelKind, testKindKey},
		{testLabLane, LevelVMLab, testLabKey},
	} {
		got, err := r.resolveAlias(tc.lane, tc.level, testAlias)
		if err != nil || got.Key != tc.want {
			t.Errorf("resolveAlias(%s, %s) = %q, %v; want %q", tc.lane, tc.level, got.Key, err, tc.want)
		}
	}
	for name, tc := range map[string]struct{ lane, level, alias, want string }{
		"an unknown alias": {testKindLane, LevelKind, "S06/reboot",
			"S06/reboot is not a legacy name of a kind result"},
		"an alias only the other level has": {testKindLane, LevelKind, "S04/power-off",
			"S04/power-off is not a legacy name of a kind result"},
		"a canonical key": {testKindLane, LevelKind, testKindKey,
			"operator-recovery-restart is not a legacy name of a kind result"},
		"a Ginkgo label": {testKindLane, LevelKind, s06Label,
			"S06-restart is not a legacy name of a kind result"},
		"an empty alias": {testKindLane, LevelKind, "", " is not a legacy name of a kind result"},
		"an unknown lane": {"lab-other", LevelVMLab, testAlias,
			"lane lab-other is not registered"},
		"an installed run at the Kind level": {testLabLane, LevelKind, testAlias,
			"lane lab-operator is judged at level vm-lab, not kind"},
		"a Kind run at the lab level": {testKindLane, LevelVMLab, testAlias,
			"lane kind is judged at level kind, not vm-lab"},
		"a filtered run at the lab level": {filteredKindLane, LevelVMLab, testAlias,
			"lane kind-filtered is judged at level kind, not vm-lab"},
	} {
		got, err := r.resolveAlias(tc.lane, tc.level, tc.alias)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: resolveAlias = %+v, %v; want the error %q", name, got, err, tc.want)
		}
	}
}

// Lookups do not rely on validation having run: wrong data fails the lookup.
func TestResolveFailsClosedOnAmbiguousData(t *testing.T) {
	r := testRegistry()
	r.entries = append(r.entries, ScenarioEntry{Key: "other", Name: testOther, Family: s06, Level: LevelKind,
		Aliases: []string{testAlias}})
	want := "S06/restart names 2 kind results"
	if got, err := r.resolveAlias(testKindLane, LevelKind, testAlias); err == nil || err.Error() != want {
		t.Errorf("resolveAlias = %+v, %v; want the error %q", got, err, want)
	}
	r = testRegistry()
	r.entries = append(r.entries, r.entries[1])
	want = "scenario key operator-recovery-restart names 2 results"
	if got, err := r.resolveKey(testKindKey); err == nil || err.Error() != want {
		t.Errorf("resolveKey = %+v, %v; want the error %q", got, err, want)
	}
}

// A requirement that does not map to the registered lane is a problem,
// whichever way it fails to: nothing about a failed mapping leaves it out of
// the comparison.
func TestLaneProblems(t *testing.T) {
	r := testRegistry()
	base, restart := Requirement{ID: testBaseAlias}, Requirement{ID: s06, Variant: testRestart}
	if got := r.laneProblems(Lane{Name: testKindLane, Required: []Requirement{base, restart}}, LevelKind); len(got) != 0 {
		t.Errorf("a registered lane has problems: %q", got)
	}
	r.entries[1].Aliases = []string{testAlias, "S06/reboot"}
	r.families = append(r.families, ScenarioFamily{ID: s07, Key: "other", Description: testOther, Contract: "s07"})
	r.entries = append(r.entries, ScenarioEntry{Key: "installed-other", Name: testOther, Family: s07, Level: LevelVMLab,
		Aliases: []string{s07}})
	reboot := Requirement{ID: s06, Variant: "reboot"}
	for name, tc := range map[string]struct {
		lane  Lane
		level string
		want  []string
	}{
		"a requirement dropped from the lane": {Lane{Name: testKindLane, Required: []Requirement{base}}, LevelKind,
			[]string{testNoRestart}},
		"a requirement renamed to its key": {
			Lane{Name: testKindLane, Required: []Requirement{base, {ID: testKindKey}}}, LevelKind,
			[]string{"operator-recovery-restart is not a legacy name of a kind result",
				testNoRestart}},
		"a requirement with an unknown variant": {
			Lane{Name: testKindLane, Required: []Requirement{base, {ID: s06, Variant: testUnknown}}}, LevelKind,
			[]string{"S06/ghost is not a legacy name of a kind result",
				testNoRestart}},
		"a requirement of another lane": {Lane{Name: testLabLane, Required: []Requirement{restart}}, LevelKind,
			[]string{"lane lab-operator is judged at level vm-lab, not kind"}},
		"a requirement the lane does not register": {
			Lane{Name: testLabLane, Required: []Requirement{{ID: testBaseAlias}, restart}}, LevelVMLab,
			[]string{"S01 is not a legacy name of a vm-lab result"}},
		"two requirements of one result": {
			Lane{Name: testKindLane, Required: []Requirement{base, restart, reboot}}, LevelKind,
			[]string{"lane kind: S06/reboot and an earlier requirement are both operator-recovery-restart"}},
		"a requirement not_applicable may satisfy": {
			Lane{Name: testKindLane, Required: []Requirement{base, {ID: s06, Variant: testRestart, NotApplicable: true}}},
			LevelKind, []string{"lane kind: S06/restart may be not_applicable, which no registered lane allows"}},
		"the filtered lane": {Lane{Name: filteredKindLane, Required: []Requirement{base}}, LevelKind,
			[]string{"lane kind-filtered is not registered"}},
		"a registered result of another lane": {Lane{Name: testLabLane, Required: []Requirement{restart, {ID: s07}}},
			LevelVMLab, []string{"lane lab-operator: S07 is not registered for it"}},
		"a lane that is not registered": {Lane{Name: "kind-nightly", Required: []Requirement{base}}, LevelKind,
			[]string{"lane kind-nightly is not registered"}},
		"a lane with no requirement": {Lane{Name: testKindLane}, LevelKind,
			[]string{"lane kind: requires nothing",
				"lane kind: registered key database-smoke has no requirement",
				testNoRestart}},
	} {
		if got := r.laneProblems(tc.lane, tc.level); !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

// A result is matched through the legacy name it records, never through a
// canonical key written into its ID: a result recorded under the canonical
// key, or under a key that maps to nothing, satisfies no requirement, also in
// a run whose lane and level resolve every legacy name.
func TestGateIsNotSatisfiedByARenamedResult(t *testing.T) {
	dir := runDir(t, s00Record)
	required := Requirement{ID: "S14", Variant: rotation}
	key := Scenario{Result: Pass, Evidence: []string{s00Record}, ID: "ha-token-rotation"}
	got := Lane{Name: testKindLane, Required: []Requirement{required}}.Gate(
		Summary{Environment: Environment{Level: LevelKind}, Scenarios: []Scenario{key}}, dir)
	want := []string{"S14/token-rotation: missing: no result was recorded (HA Instance Manager token rotation)"}
	if !slices.Equal(got, want) {
		t.Errorf("Gate = %q, want %q", got, want)
	}
	// A requirement the registry does not know is not excused either.
	unmapped := Lane{Name: testKindLane, Required: []Requirement{{ID: s98}}}
	if got := unmapped.Gate(Summary{}, dir); !slices.Equal(got, []string{"S98: missing: no result was recorded"}) {
		t.Errorf("Gate of a requirement the registry does not know = %q", got)
	}
}

// laterVariant is the later variant of testKindKey in renamedRegistry.
const laterVariant = "manager-restart"

// renamedRegistry is testRegistry with the Kind restart check renamed: its
// result is recorded as S06/manager-restart from now on, and S06/restart stays
// its alias so that earlier results keep their identity.
func renamedRegistry() scenarioRegistry {
	r := testRegistry()
	r.entries[1].Aliases = []string{testAlias, s06 + "/" + laterVariant}
	return r
}

// A requirement and a result are one check when they resolve to one key under
// the lane and level of the run, so a lane that still names the check by its
// earlier legacy name is satisfied by a result recorded under the later one.
// Everything that fails a required result under its own name fails it under
// the other name too, and a result whose name does not resolve at the run's
// level is matched by its exact legacy name only, as before keys existed.
func TestGateMatchesARequirementAndAResultByRegistryKey(t *testing.T) {
	dir := runDir(t, s00Record)
	r := renamedRegistry()
	lane := Lane{Name: testKindLane, Required: []Requirement{{ID: testBaseAlias}, {ID: s06, Variant: testRestart}}}
	base := Scenario{ID: testBaseAlias, Result: Pass, Evidence: []string{s00Record}}
	renamed := func(result Result, reason string, evidence ...string) Scenario {
		return Scenario{ID: s06, Variant: laterVariant, Result: result, Reason: reason, Evidence: evidence}
	}
	earlier := Scenario{ID: s06, Variant: testRestart, Result: Pass, Evidence: []string{s00Record}}
	const required = "S06/restart: "
	for _, tc := range []struct {
		name      string
		lane      Lane
		level     string
		scenarios []Scenario
		// want starts the one problem expected; "" means the lane passes.
		want string
	}{
		{"a pass recorded under the later name satisfies the earlier one", lane, LevelKind,
			[]Scenario{base, renamed(Pass, "", s00Record)}, ""},
		{"a pass under each name of one check", lane, LevelKind,
			[]Scenario{base, earlier, renamed(Pass, "", s00Record)}, ""},
		{"a failure under the later name fails the requirement", lane, LevelKind,
			[]Scenario{base, renamed(Fail, "x", s00Record)}, required + "fail: x"},
		{"a blocked result under the later name", lane, LevelKind,
			[]Scenario{base, renamed(Blocked, setupFailed)}, required + "blocked: " + setupFailed},
		{"a result under the later name that was not run", lane, LevelKind,
			[]Scenario{base, renamed(NotRun, setupFailed)}, required + "not_run: " + setupFailed},
		{"not_applicable under the later name where the lane does not allow it", lane, LevelKind,
			[]Scenario{base, renamed(NotApplicable, setupFailed)}, required + "not_applicable: " + setupFailed},
		{"a claimed pass under the later name without evidence", lane, LevelKind,
			[]Scenario{base, renamed(Pass, "")}, required + "fail: claimed a pass, but no evidence is listed"},
		{"a pass under one name and a block under the other", lane, LevelKind,
			[]Scenario{base, earlier, renamed(Blocked, setupFailed)}, required + "blocked: " + setupFailed},
		{"no result under either name", lane, LevelKind, []Scenario{base}, required + "missing"},
		{"a run that records no level resolves no key", lane, "",
			[]Scenario{base, renamed(Pass, "", s00Record)}, required + "missing"},
		{"a Kind lane recorded at the VM-lab level resolves no key", lane, LevelVMLab,
			[]Scenario{base, renamed(Pass, "", s00Record)}, required + "missing"},
		{"the later name is a Kind alias, not one of the lab check", Lane{Name: testLabLane,
			Required: []Requirement{{ID: s06, Variant: testRestart}}}, LevelVMLab,
			[]Scenario{renamed(Pass, "", s00Record)}, required + "missing"},
		{"the canonical key in the ID field is not a legacy name", lane, LevelKind,
			[]Scenario{base, {ID: testKindKey, Result: Pass, Evidence: []string{s00Record}}}, required + "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Summary{Environment: Environment{Level: tc.level}, Scenarios: tc.scenarios}
			problems := r.gate(tc.lane, s, dir)
			switch {
			case tc.want == "" && len(problems) != 0:
				t.Errorf("problems = %q, want none", problems)
			case tc.want != "" && (len(problems) != 1 || !strings.HasPrefix(problems[0], tc.want)):
				t.Errorf("problems = %q, want exactly one starting with %q", problems, tc.want)
			}
		})
	}
}

// The real registry holds one legacy name per result, so matching by key
// judges every registered lane as matching by legacy name did, at the lane's
// level and in a run that records no level: all passes pass, and a blocked
// result fails its own requirement and no other.
func TestGateJudgesEveryRegisteredLaneAsItsLegacyNamesDid(t *testing.T) {
	dir := runDir(t, s00Record)
	for _, registered := range registry.lanes {
		lane := Lane{Name: registered.Name}
		var passing []Scenario
		for _, key := range registered.Keys {
			entry, err := ResolveKey(key)
			if err != nil {
				t.Fatal(err)
			}
			id, variant, _ := strings.Cut(entry.Aliases[0], "/")
			lane.Required = append(lane.Required, Requirement{ID: id, Variant: variant})
			passing = append(passing, Scenario{ID: id, Variant: variant, Result: Pass, Evidence: []string{s00Record}})
		}
		for _, level := range []string{registered.Level, ""} {
			gate := func(scenarios []Scenario) []string {
				return lane.Gate(Summary{Environment: Environment{Level: level}, Scenarios: scenarios}, dir)
			}
			if problems := gate(passing); len(problems) != 0 {
				t.Errorf("lane %s at level %q: problems = %q, want none", lane.Name, level, problems)
			}
			for i := range passing {
				scenarios := slices.Clone(passing)
				scenarios[i].Result, scenarios[i].Reason = Blocked, setupFailed
				want := passing[i].Name() + ": blocked: " + setupFailed
				if problems := gate(scenarios); len(problems) != 1 || !strings.HasPrefix(problems[0], want) {
					t.Errorf("lane %s at level %q: problems = %q, want exactly one starting with %q",
						lane.Name, level, problems, want)
				}
			}
		}
	}
}

// Reserved contract scenarios have no runnable key, and an installed run at
// the Kind level never takes the result of the Kind suite.
func TestRegisteredKeys(t *testing.T) {
	for _, alias := range []string{s16, "S17", s07, s99, s06} {
		if got, err := ResolveAlias(testKindLane, LevelKind, alias); err == nil {
			t.Errorf("%s resolved to %q", alias, got.Key)
		}
	}
	kindResult, err := ResolveAlias(testKindLane, LevelKind, testAlias)
	if err != nil || kindResult.Key != realKindKey {
		t.Errorf("S06/restart on Kind = %q, %v", kindResult.Key, err)
	}
	labResult, err := ResolveAlias(realLabLane, LevelVMLab, testAlias)
	if err != nil || labResult.Key != realLabKey {
		t.Errorf("S06/restart on the lab = %q, %v", labResult.Key, err)
	}
	if _, err := ResolveAlias(realLabLane, LevelKind, testAlias); err == nil {
		t.Error("a Kind-level run of an installed lane took a key")
	}
	if kindResult.Name == labResult.Name {
		t.Errorf("both results read %q", kindResult.Name)
	}
	if !slices.Equal(kindResult.Lanes, []string{testKindLane}) {
		t.Errorf("lanes of %s = %q", kindResult.Key, kindResult.Lanes)
	}
	for _, level := range []string{LevelKind, LevelVMLab} {
		keys := RegisteredKeys(level)
		if len(keys) == 0 {
			t.Errorf("no key at level %s", level)
		}
		for _, key := range keys {
			if found, err := ResolveKey(key); err != nil || found.Level != level {
				t.Errorf("key %s listed at level %s resolves to level %q, %v", key, level, found.Level, err)
			}
		}
		lanes := RegisteredLanes(level)
		if len(lanes) == 0 {
			t.Errorf("no lane at level %s", level)
		}
		for _, lane := range lanes {
			if _, err := ResolveAlias(lane, level, s99); err == nil || strings.Contains(err.Error(), "not registered") {
				t.Errorf("lane %s at level %s: %v", lane, level, err)
			}
		}
	}
}

// laneRow matches a row of the required-scenarios table, and laneRequirement
// one scenario in its third column, up to the first colon, which ends the
// list and starts the prose.
var (
	laneRow         = regexp.MustCompile("^\\| `(kind|lab-[a-z0-9-]+)` \\| .*? \\| (.*) \\| [^|]* \\|$")
	laneRequirement = regexp.MustCompile(`(S[0-9]{2}|LAB[0-9]{2})(?: \(([^)]*)\))?`)
	backticked      = regexp.MustCompile("`([^`]+)`")
	heading         = regexp.MustCompile(`^(#+) (.*)$`)
	anchorDropped   = regexp.MustCompile(`[^a-z0-9 -]`)
)

// contractLanes reads the required scenarios of each lane from the table of
// the contract, as names such as "S03/abrupt".
func contractLanes(t *testing.T, contract string) map[string][]string {
	t.Helper()
	lanes := map[string][]string{}
	for line := range strings.Lines(contract) {
		row := laneRow.FindStringSubmatch(strings.TrimRight(line, "\n"))
		if row == nil {
			continue
		}
		list, _, _ := strings.Cut(row[2], ":")
		for _, m := range laneRequirement.FindAllStringSubmatch(list, -1) {
			if m[2] == "" {
				lanes[row[1]] = append(lanes[row[1]], m[1])
			}
			for _, variant := range backticked.FindAllStringSubmatch(m[2], -1) {
				lanes[row[1]] = append(lanes[row[1]], m[1]+"/"+variant[1])
			}
		}
	}
	if len(lanes) == 0 {
		t.Fatal("no lane found in the contract's table")
	}
	return lanes
}

// contractSections maps the anchor of each heading to the text below it, up
// to the next heading of the same or a higher level.
func contractSections(contract string) map[string]string {
	sections := map[string]string{}
	lines := strings.Split(contract, "\n")
	for i, line := range lines {
		m := heading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		anchor := strings.ReplaceAll(anchorDropped.ReplaceAllString(strings.ToLower(m[2]), ""), " ", "-")
		end := i + 1
		for ; end < len(lines); end++ {
			next := heading.FindStringSubmatch(lines[end])
			if next != nil && len(next[1]) <= len(m[1]) {
				break
			}
		}
		sections[anchor] = strings.Join(lines[i:end], "\n")
	}
	return sections
}

// registeredNames is the legacy name of each key the lane requires.
func registeredNames(t *testing.T, r scenarioRegistry, lane RequiredLane) []string {
	t.Helper()
	names := make([]string, 0, len(lane.Keys))
	for _, key := range lane.Keys {
		entry, err := r.resolveKey(key)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, entry.Aliases...)
	}
	return names
}

// The lane table of the contract is normative, so every lane of the registry
// must be a row of it with the same requirements, and every row a lane.
func TestRegistryMatchesTheContractsLanes(t *testing.T) {
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	contract := contractLanes(t, string(data))
	for _, lane := range registry.lanes {
		want, ok := contract[lane.Name]
		if !ok {
			t.Errorf("lane %s is not in the contract's table", lane.Name)
			continue
		}
		got := registeredNames(t, registry, lane)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("lane %s:\nregistry %q\ncontract %q", lane.Name, got, want)
		}
		delete(contract, lane.Name)
	}
	for lane := range contract {
		t.Errorf("lane %s of the contract is not registered", lane)
	}
}

// Every pointer leads to a section of the contract, every variant is named in
// its section, and every scenario of the contract has a family.
func TestRegistryMatchesTheContractsScenarios(t *testing.T) {
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	sections := contractSections(string(data))
	families := map[string]ScenarioFamily{}
	for _, family := range registry.families {
		families[family.ID] = family
		if _, ok := sections[family.Contract]; !ok {
			t.Errorf("family %s points to %q, which is no section of the contract", family.ID, family.Contract)
		}
	}
	for _, entry := range registry.entries {
		family := families[entry.Family]
		for _, alias := range entry.Aliases {
			_, variant, found := strings.Cut(alias, "/")
			if found && !strings.Contains(sections[family.Contract], "`"+variant+"`") {
				t.Errorf("%s: the contract's section %s does not name the variant", alias, family.Contract)
			}
		}
	}
	scenarioHeading := regexp.MustCompile(`(?m)^### (S[0-9]{2}): `)
	for _, m := range scenarioHeading.FindAllStringSubmatch(string(data), -1) {
		if _, ok := families[m[1]]; !ok {
			t.Errorf("scenario %s of the contract has no family", m[1])
		}
	}
}

// The inventory explains the keys, so it names every one of them.
func TestInventoryNamesEveryKey(t *testing.T) {
	data, err := os.ReadFile("../../docs/testing/scenario-name-migration.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range registry.entries {
		if !strings.Contains(string(data), "`"+entry.Key+"`") {
			t.Errorf("the inventory does not name the key %s", entry.Key)
		}
	}
	for _, family := range registry.families {
		if !strings.Contains(string(data), "`"+family.Key+"`") {
			t.Errorf("the inventory does not name the family key %s", family.Key)
		}
	}
}

func sourceIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

func sourceSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	return ok && sourceIdent(sel.X, pkg) && sel.Sel.Name == name
}

func sourceString(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

// kindRequiredErrorCheck accepts only the initialization panic that preserves
// the constructor error, before the declaration can return any requirements.
func kindRequiredErrorCheck(stmt ast.Stmt) bool {
	check, ok := stmt.(*ast.IfStmt)
	if !ok || check.Init != nil || check.Else != nil || len(check.Body.List) != 1 {
		return false
	}
	condition, ok := check.Cond.(*ast.BinaryExpr)
	if !ok || condition.Op != token.NEQ || !sourceIdent(condition.X, "err") || !sourceIdent(condition.Y, "nil") {
		return false
	}
	statement, ok := check.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	stop, ok := statement.X.(*ast.CallExpr)
	if !ok || !sourceIdent(stop.Fun, "panic") || len(stop.Args) != 1 || stop.Ellipsis.IsValid() {
		return false
	}
	context, ok := stop.Args[0].(*ast.CallExpr)
	if !ok || !sourceSelector(context.Fun, "fmt", "Errorf") || len(context.Args) != 2 || context.Ellipsis.IsValid() ||
		!sourceIdent(context.Args[1], "err") {
		return false
	}
	message, ok := sourceString(context.Args[0])
	if !ok || !strings.Contains(message, "%w") || !strings.Contains(message, "Kind") {
		return false
	}
	return true
}

// kindRequiredKeys reads only the supported declaration: one constructor call,
// a contextual panic on error, and an unchanged return. It is deliberately not
// a general interpreter of Go expressions or registry-derived lists.
func kindRequiredKeys(fn *ast.FuncDecl) ([]string, error) {
	bad := func() ([]string, error) { return nil, fmt.Errorf("unsupported kindRequired declaration") }
	if fn.Body == nil || len(fn.Body.List) != 3 {
		return bad()
	}
	assignment, ok := fn.Body.List[0].(*ast.AssignStmt)
	if !ok || assignment.Tok != token.DEFINE || len(assignment.Lhs) != 2 || len(assignment.Rhs) != 1 ||
		!sourceIdent(assignment.Lhs[0], "required") || !sourceIdent(assignment.Lhs[1], "err") {
		return bad()
	}
	call, ok := assignment.Rhs[0].(*ast.CallExpr)
	if !ok || !sourceSelector(call.Fun, "evidence", "RequirementsForKeys") ||
		call.Ellipsis.IsValid() || len(call.Args) != 18 {
		return bad()
	}
	lane, ok := sourceString(call.Args[0])
	if !ok || lane != testKindLane || !sourceSelector(call.Args[1], "evidence", "LevelKind") {
		return bad()
	}
	keys := make([]string, 0, 16)
	for _, arg := range call.Args[2:] {
		key, ok := sourceString(arg)
		if !ok {
			return bad()
		}
		keys = append(keys, key)
	}
	if !slices.Equal(keys, requiredKindKeys()) {
		return nil, fmt.Errorf("kindRequired must list the exact ordered 16 Kind keys, got %q", keys)
	}
	if !kindRequiredErrorCheck(fn.Body.List[1]) {
		return bad()
	}
	returned, ok := fn.Body.List[2].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 || !sourceIdent(returned.Results[0], "required") {
		return bad()
	}
	return keys, nil
}

func kindRequiredInSource(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../e2e/summary_test.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "kindRequired" {
			keys, err := kindRequiredKeys(fn)
			if err != nil {
				t.Fatal(err)
			}
			return keys
		}
	}
	t.Fatal("no function kindRequired in the source of the e2e suite")
	return nil
}

func TestRegistryMatchesKindRequired(t *testing.T) {
	keys := kindRequiredInSource(t)
	got, err := RequirementsForKeys(testKindLane, LevelKind, keys...)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, legacyKindRequirements()) {
		t.Fatalf("source resolved to unexpected legacy requirements: %#v", got)
	}
	if problems := (Lane{Name: testKindLane, Required: got}).RegistryProblems(LevelKind); len(problems) != 0 {
		t.Fatal(problems)
	}
}

func TestKindRequiredSourceGuardRejectsUnsupportedDeclarations(t *testing.T) {
	args := make([]string, 0, 16)
	for _, key := range requiredKindKeys() {
		args = append(args, strconv.Quote(key))
	}
	call := "evidence.RequirementsForKeys(\"kind\", evidence.LevelKind, " + strings.Join(args, ", ") + ")"
	errorCheck := "if err != nil { panic(fmt.Errorf(\"declare Kind requirements: %w\", err)) }; "
	body := "required, err := " + call + "; " + errorCheck + "return required"
	tests := map[string]string{
		"valid":         body,
		"omitted key":   strings.Replace(body, ", "+args[15], "", 1),
		"duplicate key": strings.Replace(body, args[15], args[14], 1),
		"registry derived arguments": "required, err := evidence.RequirementsForKeys(\"kind\", evidence.LevelKind, " +
			"evidence.RegisteredKeys(evidence.LevelKind)...); " + errorCheck + "return required",
		"dynamic key":             strings.Replace(body, args[0], "key", 1),
		"changed lane":            strings.Replace(body, "\"kind\"", "\"kind-filtered\"", 1),
		"changed level":           strings.Replace(body, "evidence.LevelKind", "evidence.LevelVMLab", 1),
		"second constructor call": strings.Replace(body, "return required", "_, _ = "+call+"; return required", 1),
		"appended requirement": strings.Replace(body, "return required",
			"required = append(required, evidence.Requirement{}); return required", 1),
		"ignored error":         strings.Replace(body, errorCheck, "", 1),
		"wrong error condition": strings.Replace(body, "err != nil", "err == nil", 1),
		"return on error": strings.Replace(body,
			"panic(fmt.Errorf(\"declare Kind requirements: %w\", err))", "return required", 1),
		"unrelated return": strings.Replace(body, "return required", "return other", 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			source := "package e2e; func kindRequired() []evidence.Requirement { " + body + " }"
			file, err := parser.ParseFile(token.NewFileSet(), "declaration.go", source, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			_, err = kindRequiredKeys(file.Decls[0].(*ast.FuncDecl))
			if (err == nil) != (name == "valid") {
				t.Fatalf("source guard error = %v", err)
			}
		})
	}
}

// These independent declarations pin both the readable source and the legacy
// runtime identities; registry iteration must not define the expected set.
func requiredKindKeys() []string {
	const replicationKey = "ha-replication"
	return []string{
		"standalone-database-sql", "ha-broker-access", replicationKey,
		"instance-manager-access-and-secret-protection",
		"ha-primary-pod-recovery-abrupt-first-member", "ha-primary-pod-recovery-abrupt-later-master",
		"ha-primary-pod-recovery-graceful", "broker-failure-recovery-process",
		"broker-failure-recovery-one-pod", "broker-failure-recovery-all-rw-pods",
		"operator-failure-recovery-restart", "operator-failure-recovery-absent-during-failover",
		"cluster-deletion-retention-retain", "cluster-deletion-retention-delete",
		"ha-token-rotation", "ha-legacy-token-migration",
	}
}

func legacyKindRequirements() []Requirement {
	const (
		brokerFamily   = "S05"
		deletionFamily = "S13"
		smokeFamily    = "S01"
	)
	return []Requirement{
		{ID: s00}, {ID: smokeFamily}, {ID: s02Label}, {ID: s14},
		{ID: s03, Variant: "abrupt-first-member"}, {ID: s03, Variant: "abrupt"}, {ID: s03, Variant: "graceful"},
		{ID: brokerFamily, Variant: "broker-process"}, {ID: brokerFamily, Variant: "one-broker-pod"},
		{ID: brokerFamily, Variant: "all-rw-brokers"},
		{ID: s06, Variant: "restart"}, {ID: s06, Variant: "absent-during-failover"},
		{ID: deletionFamily, Variant: "retain"}, {ID: deletionFamily, Variant: "delete"},
		{ID: s14, Variant: "token-rotation"}, {ID: s14, Variant: "token-migration"},
	}
}

func TestRequirementsForKeysKeepsLegacyKindRequirements(t *testing.T) {
	got, err := RequirementsForKeys(testKindLane, LevelKind, requiredKindKeys()...)
	if err != nil {
		t.Fatalf("complete explicit Kind keys returned an error: %v", err)
	}
	if want := legacyKindRequirements(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %#v, want ordered legacy requirements %#v", got, want)
	}
}

func TestRequirementsForKeysPreservesCallerOrderAndInputs(t *testing.T) {
	keys, want := requiredKindKeys(), legacyKindRequirements()
	slices.Reverse(keys)
	slices.Reverse(want)
	beforeKeys, beforeRegistry := slices.Clone(keys), fmt.Sprintf("%#v", registry)
	for range 2 {
		got, err := RequirementsForKeys(testKindLane, LevelKind, keys...)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("reordered requirements = %#v, %v", got, err)
		}
	}
	if !slices.Equal(keys, beforeKeys) || fmt.Sprintf("%#v", registry) != beforeRegistry {
		t.Fatal("constructor changed its inputs or registry")
	}
}

func TestRequirementsForKeysRejectsInvalidDeclarations(t *testing.T) {
	tests := map[string]struct {
		change      func(*scenarioRegistry)
		lane, level string
		keys        []string
		want        string
	}{
		"no keys":                          {keys: []string{}, want: "requires nothing"},
		"partial":                          {keys: []string{testBaseKey}, want: "has no requirement"},
		"duplicate":                        {keys: []string{testBaseKey, testKindKey, testKindKey}, want: "duplicate"},
		"unknown key after valid key":      {keys: []string{testBaseKey, "unknown"}, want: "unknown scenario key"},
		"legacy alias is not a key":        {keys: []string{testBaseKey, testAlias}, want: "unknown scenario key"},
		"family without executable result": {keys: []string{testBaseKey, testFamilyKey}, want: "not one result"},
		"unknown lane":                     {lane: "unknown", want: "not registered"},
		"filtered lane":                    {lane: "kind-filtered", want: "not registered"},
		"wrong level":                      {level: LevelVMLab, want: "is judged at level"},
		"empty level":                      {level: "empty-level", want: "is judged at level"},
		"installed key":                    {keys: []string{testBaseKey, testLabKey}, want: "has level"},
		"key outside lane": {
			change: func(r *scenarioRegistry) { r.lanes[0].Keys = []string{testBaseKey} },
			want:   "not registered for lane",
		},
		"ambiguous key": {
			change: func(r *scenarioRegistry) { r.entries = append(r.entries, r.entries[1]) },
			want:   "names 2 results",
		},
		"missing alias": {
			change: func(r *scenarioRegistry) { r.entries[1].Aliases = nil },
			want:   "exactly one",
		},
		"multiple aliases": {
			change: func(r *scenarioRegistry) { r.entries[1].Aliases = []string{testAlias, "S06/later"} },
			want:   "exactly one",
		},
		"colliding alias": {
			change: func(r *scenarioRegistry) { r.entries[0].Aliases = []string{testAlias} },
			want:   "names 2",
		},
		"alias belongs to another family": {
			change: func(r *scenarioRegistry) { r.entries[1].Family = testBaseAlias },
			want:   "not S01",
		},
		"malformed alias": {
			change: func(r *scenarioRegistry) { r.entries[1].Aliases = []string{"S06/Bad_variant"} },
			want:   "malformed legacy alias",
		},
		"missing referenced family": {
			change: func(r *scenarioRegistry) { r.families = r.families[:1] },
			want:   "unknown family",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := testRegistry()
			if tc.change != nil {
				tc.change(&r)
			}
			lane, level, keys := tc.lane, tc.level, tc.keys
			if lane == "" {
				lane = testKindLane
			}
			if level == "" {
				level = LevelKind
			}
			if level == "empty-level" {
				level = ""
			}
			if keys == nil {
				keys = []string{testBaseKey, testKindKey}
			}
			before := fmt.Sprintf("%#v", r)
			for range 2 {
				got, err := r.requirementsForKeys(lane, level, keys...)
				if got != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("requirements = %#v, error = %v; want nil and %q", got, err, tc.want)
				}
			}
			if fmt.Sprintf("%#v", r) != before {
				t.Fatal("constructor changed registry on error")
			}
		})
	}
	// The smoke key is both a family and an executable result. Resolve the
	// result first, while still requiring the complete lane.
	got, err := testRegistry().requirementsForKeys(testKindLane, LevelKind, testBaseKey, testKindKey)
	if err != nil || !reflect.DeepEqual(got, []Requirement{{ID: testBaseAlias}, {ID: s06, Variant: testRestart}}) {
		t.Fatalf("family with actual result = %#v, %v", got, err)
	}
}

func TestRequirementsForKeysKeepsLegacyLifecycle(t *testing.T) {
	required, err := RequirementsForKeys(testKindLane, LevelKind, requiredKindKeys()...)
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacyKindRequirements()
	var summary Summary
	summary.Plan(required, "no step started")
	if len(summary.Scenarios) != len(legacy) {
		t.Fatal("a planned result disappeared")
	}
	for i, req := range legacy {
		sc := summary.Scenarios[i]
		if sc.ID != req.ID || sc.Variant != req.Variant || sc.Result != NotRun {
			t.Fatalf("placeholder %d = %+v", i, sc)
		}
		if required[i].Label() != req.Label() {
			t.Fatalf("label %d changed", i)
		}
	}
	first := legacy[0]
	summary.Record(Scenario{ID: first.ID, Variant: first.Variant, Result: Pass, Evidence: []string{s00Record}})
	if summary.Settle(required[0], Blocked, "setup failed") {
		t.Fatal("settle overwrote a recorded result")
	}
	for _, req := range required[1:] {
		if !summary.Settle(req, Blocked, "setup failed") {
			t.Fatalf("lost placeholder for %s", req.Label())
		}
	}
	if len(summary.Scenarios) != len(legacy) || len(summary.planned) != 0 || summary.Scenarios[0].Result != Pass {
		t.Fatalf("lifecycle left stale placeholders: %+v", summary)
	}
}

func TestRequirementsForKeysKeepsLegacyGateFailures(t *testing.T) {
	required, err := RequirementsForKeys(testKindLane, LevelKind, requiredKindKeys()...)
	if err != nil {
		t.Fatal(err)
	}
	dir := runDir(t, s00Record)
	passing := func() []Scenario {
		scenarios := make([]Scenario, 0, 16)
		for _, req := range legacyKindRequirements() {
			scenarios = append(scenarios, Scenario{ID: req.ID, Variant: req.Variant,
				Result: Pass, Evidence: []string{s00Record}})
		}
		return scenarios
	}
	tests := map[string]struct {
		change func(*Summary)
		passes bool
	}{
		"all pass":                         {passes: true},
		"missing result":                   {change: func(s *Summary) { s.Scenarios = s.Scenarios[1:] }},
		"failed result":                    {change: func(s *Summary) { s.Scenarios[0].Result = Fail }},
		"blocked result":                   {change: func(s *Summary) { s.Scenarios[0].Result = Blocked }},
		"unstarted result":                 {change: func(s *Summary) { s.Scenarios[0].Result = NotRun }},
		"not applicable without exemption": {change: func(s *Summary) { s.Scenarios[0].Result = NotApplicable }},
		"pass without evidence":            {change: func(s *Summary) { s.Scenarios[0].Evidence = nil }},
		"missing evidence file":            {change: func(s *Summary) { s.Scenarios[0].Evidence = []string{"missing.txt"} }},
		"pass and blocked duplicate": {
			change: func(s *Summary) { sc := s.Scenarios[0]; sc.Result = Blocked; s.Scenarios = append(s.Scenarios, sc) },
		},
		"duplicate passes": {
			change: func(s *Summary) { s.Scenarios = append(s.Scenarios, s.Scenarios[0]) },
			passes: true,
		},
		"key used as legacy ID": {change: func(s *Summary) { s.Scenarios[0].ID = requiredKindKeys()[0] }},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			summary := Summary{Environment: Environment{Level: LevelKind}, Scenarios: passing()}
			if tc.change != nil {
				tc.change(&summary)
			}
			modern := Lane{Name: testKindLane, Required: required}.Gate(summary, dir)
			control := Lane{Name: testKindLane, Required: legacyKindRequirements()}.Gate(summary, dir)
			if !slices.Equal(modern, control) || (len(modern) == 0) != tc.passes {
				t.Fatalf("gate = %q; legacy = %q; want pass %t", modern, control, tc.passes)
			}
		})
	}
	// Wrong lane/level combinations remain unresolvable at the registry boundary.
	for _, level := range []string{"", LevelVMLab} {
		summary := Summary{Environment: Environment{Level: level}, Scenarios: passing()}
		for _, lane := range []string{testKindLane, realLabLane} {
			modern := Lane{Name: lane, Required: required}.Gate(summary, dir)
			control := Lane{Name: lane, Required: legacyKindRequirements()}.Gate(summary, dir)
			if !slices.Equal(modern, control) {
				t.Fatalf("lane %s level %s changed gate: %q / %q", lane, level, modern, control)
			}
		}
	}
}

func TestRequirementsForKeysKeepsSerializedOutputsAndReadback(t *testing.T) {
	required, err := RequirementsForKeys(testKindLane, LevelKind, requiredKindKeys()...)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	runs := make([]RecordedRun, 0, 2)
	payloads := make([]map[string]string, 0, 2)
	for _, requirements := range [][]Requirement{legacyKindRequirements(), required} {
		dir := runDir(t, s00Record)
		lane := Lane{Name: testKindLane, Required: requirements}
		summary := Summary{RunID: "requirement-compatibility", StartedAt: at,
			FinishedAt: at.Add(time.Minute), Environment: Environment{Level: LevelKind}}
		summary.Plan(requirements, "no step started")
		for _, req := range legacyKindRequirements() {
			summary.Record(Scenario{ID: req.ID, Variant: req.Variant, Result: Pass, Evidence: []string{s00Record},
				Measurements: map[string]any{"observed": Unknown, "count": 7}, Limits: map[string]string{"count": "10"}})
		}
		if err := summary.Conclude(dir, NewRedactor(), lane); err != nil {
			t.Fatal(err)
		}
		payload := map[string]string{}
		for _, name := range []string{summaryFile, junitFile} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			payload[name] = string(data)
		}
		payloads = append(payloads, payload)
		run, err := ReadRun(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !run.Validated() || run.Summary.SchemaVersion != SchemaVersion {
			t.Fatalf("readback did not validate: %+v", run)
		}
		for i, result := range run.Results {
			legacy := legacyKindRequirements()[i]
			if result.Scenario.ID != legacy.ID || result.Scenario.Variant != legacy.Variant ||
				result.Key != requiredKindKeys()[i] || result.Judged != Pass ||
				!slices.Equal(result.Scenario.Evidence, []string{s00Record}) {
				t.Fatalf("readback changed identity, verdict or evidence: %+v", result)
			}
		}
		runs = append(runs, run)
		if err := summary.Checkpoint(dir, NewRedactor(), lane.Name, at); err != nil {
			t.Fatal(err)
		}
		provisional, err := ReadRun(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !provisional.Provisional || provisional.Validated() || provisional.Summary.Passed {
			t.Fatal("unfinished checkpoint passed")
		}
	}
	if !reflect.DeepEqual(payloads[0], payloads[1]) {
		t.Fatal("summary.json or junit.xml changed between legacy and key declarations")
	}
	if comparison := CompareRuns(runs[0], runs[1]); !comparison.Matches() {
		t.Fatalf("identical legacy and key runs no longer match: %+v", comparison)
	}
}
