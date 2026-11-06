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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
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

// The gates keep matching on the legacy name. A result recorded under the
// canonical key, or under a key that maps to nothing, satisfies no
// requirement, and the registry knowing the name changes nothing about it.
func TestGateIsNotSatisfiedByARenamedResult(t *testing.T) {
	dir := runDir(t, s00Record)
	required := Requirement{ID: "S14", Variant: rotation}
	key := Scenario{Result: Pass, Evidence: []string{s00Record}, ID: "ha-token-rotation"}
	got := Lane{Name: testKindLane, Required: []Requirement{required}}.Gate(Summary{Scenarios: []Scenario{key}}, dir)
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

// stringValue returns the text of a string literal, or of a constant of the
// source.
func stringValue(t *testing.T, expr ast.Expr, constants map[string]string) string {
	t.Helper()
	switch e := expr.(type) {
	case *ast.BasicLit:
		value, err := strconv.Unquote(e.Value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	case *ast.Ident:
		if value, ok := constants[e.Name]; ok {
			return value
		}
	}
	t.Fatalf("not a string constant at position %d of the source", expr.Pos())
	return ""
}

// kindRequiredInSource reads what kindRequired in test/e2e/summary_test.go
// returns from its source. make test leaves the package out, and it needs
// the e2e build tag, so that a lane changed there still fails here. It stops
// on any form of the function it does not know, rather than read less.
func kindRequiredInSource(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("../e2e/*.go")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no source of the e2e suite: %v", err)
	}
	fset := token.NewFileSet()
	constants, lists := map[string]string{}, map[string]*ast.CompositeLit{}
	var function *ast.FuncDecl
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "kindRequired" {
				function = fn
			}
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Values) != 1 {
					continue
				}
				switch v := value.Values[0].(type) {
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						constants[value.Names[0].Name] = stringValue(t, v, nil)
					}
				case *ast.CompositeLit:
					lists[value.Names[0].Name] = v
				}
			}
		}
	}
	if function == nil {
		t.Fatal("no function kindRequired in the source of the e2e suite")
	}
	var names []string
	ast.Inspect(function, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		list, ok := literal.Type.(*ast.ArrayType)
		if !ok {
			return true
		}
		switch list.Elt.(type) {
		case *ast.SelectorExpr: // []evidence.Requirement{{ID: "S00"}, ...}
			for _, element := range literal.Elts {
				fields := element.(*ast.CompositeLit).Elts
				if len(fields) != 1 || fields[0].(*ast.KeyValueExpr).Key.(*ast.Ident).Name != "ID" {
					t.Fatal("kindRequired lists a requirement other than by ID alone")
				}
				names = append(names, stringValue(t, fields[0].(*ast.KeyValueExpr).Value, constants))
			}
		case *ast.StructType: // {"S03", s03Variants}, ...
			for _, element := range literal.Elts {
				fields := element.(*ast.CompositeLit).Elts
				id := stringValue(t, fields[0], constants)
				variants, ok := lists[fields[1].(*ast.Ident).Name]
				if !ok {
					t.Fatalf("kindRequired uses the unknown list %v", fields[1])
				}
				for _, variant := range variants.Elts {
					names = append(names, id+"/"+stringValue(t, variant, constants))
				}
			}
		default:
			t.Fatal("kindRequired builds a list of a type this test does not read")
		}
		return false
	})
	if len(names) == 0 {
		t.Fatal("kindRequired lists no requirement")
	}
	return names
}

// The Kind lane of the registry is what the suite requires, read from the
// suite's source because make test does not build it. TestKindLaneIsRegistered
// in test/e2e compares the value itself.
func TestRegistryMatchesKindRequired(t *testing.T) {
	got := kindRequiredInSource(t)
	for _, lane := range registry.lanes {
		if lane.Name != testKindLane {
			continue
		}
		want := registeredNames(t, registry, lane)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("kindRequired %q\nregistry %q", got, want)
		}
		return
	}
	t.Error("the kind lane is not registered")
}
