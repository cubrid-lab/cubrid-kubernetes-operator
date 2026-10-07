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
	"strings"
	"testing"

	"github.com/onsi/ginkgo/v2/types"
)

// s03AbruptLabel is the label of the steps of S03/abrupt.
const s03AbruptLabel = "S03-abrupt"

func TestRequirementLabel(t *testing.T) {
	if got := (Requirement{ID: s00}).Label(); got != s00 {
		t.Errorf("Label() = %q, want %q", got, s00)
	}
	if got := (Requirement{ID: s03, Variant: abrupt}).Label(); got != s03AbruptLabel {
		t.Errorf("Label() = %q, want S03-abrupt", got)
	}
}

// A planned scenario keeps a not_run result until a step records one, which
// then takes its place; it never disappears and never keeps a default fail.
func TestPlan(t *testing.T) {
	const planned = "no step of the scenario started"
	const s01ID = "S01"
	s01 := Requirement{ID: s01ID}
	s02 := Requirement{ID: "S02"}
	abruptReq := Requirement{ID: s03, Variant: abrupt}
	var s Summary
	s.Plan([]Requirement{{ID: s00}, s01, s02, abruptReq}, planned)

	s.Record(Scenario{ID: s01ID, Result: Pass, Evidence: []string{s00Record}})
	if !s.Settle(s02, NotRun, "not selected") {
		t.Error("Settle on a placeholder did not record the result")
	}
	if s.Settle(s01, Blocked, "an earlier step failed") {
		t.Error("Settle replaced a result a step recorded")
	}
	s.Record(Scenario{ID: s03, Variant: abrupt, Result: Fail, Reason: "x"})
	s.Record(Scenario{ID: s03, Variant: abrupt, Result: Pass})
	s.Record(Scenario{ID: "S08", Result: Fail, Reason: "y"})

	got := make([]string, 0, len(s.Scenarios))
	for _, sc := range s.Scenarios {
		got = append(got, sc.Name()+"="+string(sc.Result)+":"+sc.Reason)
	}
	want := []string{
		"S00=not_run:" + planned,
		"S01=pass:",
		"S02=not_run:not selected",
		"S03/abrupt=fail:x",
		"S03/abrupt=pass:",
		"S08=fail:y",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("scenarios:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// spec is the report of a step in the HA group.
func spec(leaf string, labels ...string) types.SpecReport {
	return types.SpecReport{
		ContainerHierarchyTexts:  []string{"Manager", "HA bootstrap"},
		ContainerHierarchyLabels: [][]string{nil, {"db", "ha-bootstrap"}},
		LeafNodeText:             leaf,
		LeafNodeLabels:           labels,
	}
}

func TestSelected(t *testing.T) {
	s03Step := spec("S03/abrupt: one member is master again", "S03", s03AbruptLabel)
	s03Step.LeafNodeSemVerConstraints = []string{">= 2.0.0"}
	s01Step := spec("S01: has one master", "S01")
	tests := map[string]struct {
		suite    types.SuiteConfig
		s03, s01 bool
	}{
		"no filter":                     {types.SuiteConfig{}, true, true},
		"label filter on the variant":   {types.SuiteConfig{LabelFilter: "S03-abrupt"}, true, false},
		"label filter on the group":     {types.SuiteConfig{LabelFilter: "ha-bootstrap"}, true, true},
		"label filter that excludes db": {types.SuiteConfig{LabelFilter: "!db"}, false, false},
		"focus":                         {types.SuiteConfig{FocusStrings: []string{"S03"}}, true, false},
		"focus on the suite and spec":   {types.SuiteConfig{FocusStrings: []string{"^e2e suite Manager HA"}}, true, true},
		"skip":                          {types.SuiteConfig{SkipStrings: []string{"S01"}}, true, false},
		"semantic version filter":       {types.SuiteConfig{SemVerFilter: "1.0.0"}, false, true},
		"focus file of another file":    {types.SuiteConfig{FocusFiles: []string{"s00_test.go"}}, false, false},
	}
	for name, tc := range tests {
		if got := Selected(s03Step, tc.suite, "e2e suite"); got != tc.s03 {
			t.Errorf("%s: Selected(S03 step) = %v, want %v", name, got, tc.s03)
		}
		if got := Selected(s01Step, tc.suite, "e2e suite"); got != tc.s01 {
			t.Errorf("%s: Selected(S01 step) = %v, want %v", name, got, tc.s01)
		}
	}
}

// A step that ended without recording a result gives its scenario one that
// says why: never a pass, and blocked only when a step it depends on failed.
func TestStepOutcome(t *testing.T) {
	skipped := func(message string) types.SpecReport {
		r := spec("S01: has one master", "S01")
		r.State, r.Failure.Message = types.SpecStateSkipped, message
		return r
	}
	failed := spec("S01: has one master", "S01")
	failed.State, failed.Failure.Message = types.SpecStateFailed, "Expected true"
	setupFailed := spec("S01: has one master", "S01")
	setupFailed.State, setupFailed.Failure.Message = types.SpecStateFailed, "image build failed"
	setupFailed.Failure.FailureNodeType = types.NodeTypeBeforeAll
	passed := spec("S01: has one master", "S01")
	passed.State = types.SpecStatePassed
	const earlier = "seeds the other members, which join as slaves"

	tests := map[string]struct {
		report   types.SpecReport
		selected bool
		result   Result
		reason   string
	}{
		"filtered out": {skipped(""), false, NotRun, "not selected by the run's focus or label filter"},
		"an earlier step of an ordered group failed": {
			skipped("Spec skipped because an earlier spec in an ordered container failed"), true,
			Blocked, "an earlier step of its group failed: " + earlier},
		"the group's setup failed": {skipped("Spec skipped because a BeforeAll node failed"), true,
			Blocked, "an earlier step of its group failed: " + earlier},
		"skipped by the step itself": {skipped("needs linux/amd64. Not run."), true,
			NotRun, "skipped: needs linux/amd64. Not run."},
		"the run stopped first": {skipped(""), true, NotRun, "the run stopped before this step started"},
		"failed before it recorded": {failed, true, Fail,
			"the step failed before it recorded a result: Expected true"},
		"the group's setup failed in this step": {setupFailed, true, Blocked,
			"the group's setup failed: image build failed"},
	}
	for name, tc := range tests {
		result, reason, ok := StepOutcome(tc.report, tc.selected, earlier)
		if !ok || result != tc.result || reason != tc.reason {
			t.Errorf("%s: StepOutcome = %q, %q, %v; want %q, %q, true", name, result, reason, ok, tc.result, tc.reason)
		}
	}
	if result, reason, ok := StepOutcome(passed, true, ""); ok {
		t.Errorf("a step that passed gave a result of its own: %q, %q", result, reason)
	}
}

func TestUnlabelledSteps(t *testing.T) {
	reqs := []Requirement{{ID: s00}, {ID: s03, Variant: abrupt}, {ID: s03, Variant: graceful}}
	got := UnlabelledSteps(reqs, map[string]bool{s00: true, s03AbruptLabel: true, "S03": true})
	want := "S03/graceful: no step of the suite carries the label S03-graceful"
	if len(got) != 1 || got[0] != want {
		t.Errorf("UnlabelledSteps = %q, want [%q]", got, want)
	}
}
