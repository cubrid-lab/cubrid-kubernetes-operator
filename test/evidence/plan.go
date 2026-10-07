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
	"regexp"
	"strings"

	"github.com/onsi/ginkgo/v2/types"
)

// Label is the Ginkgo label of the steps whose result is this requirement's:
// the scenario ID, followed by "-" and the variant if there is one
// ("S03-abrupt"). Ginkgo does not allow "/" in a label.
func (r Requirement) Label() string {
	if r.Variant == "" {
		return r.ID
	}
	return r.ID + "-" + r.Variant
}

// Plan records every requirement as not_run with the reason, in the given
// order, before any step runs. A result recorded later for the same scenario
// and variant takes the place of that placeholder, so that a scenario whose
// steps never start still has a result that says so.
func (s *Summary) Plan(reqs []Requirement, reason string) {
	if s.planned == nil {
		s.planned = map[Requirement]int{}
	}
	for _, req := range reqs {
		key := Requirement{ID: req.ID, Variant: req.Variant}
		if _, ok := s.planned[key]; ok {
			continue
		}
		s.planned[key] = len(s.Scenarios)
		s.Scenarios = append(s.Scenarios, Scenario{ID: req.ID, Variant: req.Variant, Result: NotRun, Reason: reason})
	}
}

// Record adds a scenario's result. The first result of a planned scenario or
// variant replaces its placeholder; any other result is added after the
// results already recorded.
func (s *Summary) Record(sc Scenario) {
	key := Requirement{ID: sc.ID, Variant: sc.Variant}
	if i, ok := s.planned[key]; ok {
		delete(s.planned, key)
		s.Scenarios[i] = sc
		return
	}
	s.Scenarios = append(s.Scenarios, sc)
}

// Settle records a result for a planned scenario or variant whose placeholder
// is still in place, and reports whether it did. A result a step recorded is
// never replaced.
func (s *Summary) Settle(req Requirement, result Result, reason string) bool {
	if _, ok := s.planned[Requirement{ID: req.ID, Variant: req.Variant}]; !ok {
		return false
	}
	s.Record(Scenario{ID: req.ID, Variant: req.Variant, Result: result, Reason: reason})
	return true
}

// Selected reports whether the run's label filter, focus, skip and file
// filters select the spec, the way Ginkgo applies them to a suite with the
// given description.
func Selected(report types.SpecReport, suite types.SuiteConfig, description string) bool {
	if suite.LabelFilter != "" {
		if ok, err := report.MatchesLabelFilter(suite.LabelFilter); err != nil || !ok {
			return false
		}
	}
	text := description + " " + report.FullText()
	if len(suite.FocusStrings) > 0 && !regexp.MustCompile(strings.Join(suite.FocusStrings, "|")).MatchString(text) {
		return false
	}
	if len(suite.SkipStrings) > 0 && regexp.MustCompile(strings.Join(suite.SkipStrings, "|")).MatchString(text) {
		return false
	}
	locations := append(append([]types.CodeLocation{}, report.ContainerHierarchyLocations...), report.LeafNodeLocation)
	if len(suite.FocusFiles) > 0 {
		if filters, err := types.ParseFileFilters(suite.FocusFiles); err != nil || !filters.Matches(locations) {
			return false
		}
	}
	if len(suite.SkipFiles) > 0 {
		if filters, err := types.ParseFileFilters(suite.SkipFiles); err != nil || filters.Matches(locations) {
			return false
		}
	}
	return true
}

// Ginkgo's messages for a spec it did not start because of another one.
const (
	skippedAfterFailure   = "Spec skipped because an earlier spec in an ordered container failed"
	skippedAfterBeforeAll = "Spec skipped because a BeforeAll node failed"
)

// StepOutcome returns the result of the scenario of a step that ended
// without recording one, and false when the step passed: then the result is
// the step's to record. earlierFailure names the step whose failure the run
// saw last. A step that was not selected, or skipped, is not_run; one that
// did not start because an earlier step of its ordered group failed is
// blocked.
func StepOutcome(report types.SpecReport, selected bool, earlierFailure string) (Result, string, bool) {
	message := report.Failure.Message
	switch {
	case !selected:
		return NotRun, "not selected by the run's focus or label filter", true
	case report.State.Is(types.SpecStateSkipped) &&
		(strings.HasPrefix(message, skippedAfterFailure) || strings.HasPrefix(message, skippedAfterBeforeAll)):
		return Blocked, "an earlier step of its group failed: " + earlierFailure, true
	case report.State.Is(types.SpecStateSkipped) && message == "":
		return NotRun, "the run stopped before this step started", true
	case report.State.Is(types.SpecStateSkipped):
		return NotRun, "skipped: " + message, true
	case report.State.Is(types.SpecStateFailureStates):
		return Fail, "the step failed before it recorded a result: " + message, true
	}
	return "", "", false
}
