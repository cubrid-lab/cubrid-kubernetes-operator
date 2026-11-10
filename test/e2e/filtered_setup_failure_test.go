//go:build e2e
// +build e2e

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

package e2e

import (
	"strings"
	"testing"

	"github.com/onsi/ginkgo/v2/types"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// startSetupFailedRun gives a test the state of a run whose suite setup failed
// after the run was planned: the selection of the steps was made before the
// suite started, and Ginkgo then reported no step. It restores the earlier
// state afterwards.
func startSetupFailedRun(t *testing.T, selectedLabel string) {
	t.Helper()
	startRunState(t)
	summaryMu.Lock()
	previous := selectedSteps
	selectedSteps = map[evidence.Requirement]bool{}
	summaryMu.Unlock()
	t.Cleanup(func() {
		summaryMu.Lock()
		selectedSteps = previous
		summaryMu.Unlock()
	})
	planRun()
	markSelectedSteps([]string{selectedLabel})
}

// scenarioResult returns the recorded result of a scenario. The caller holds
// summaryMu.
func scenarioResult(t *testing.T, req evidence.Requirement) evidence.Scenario {
	t.Helper()
	for _, sc := range runSummary.Scenarios {
		if sc.ID == req.ID && sc.Variant == req.Variant {
			return sc
		}
	}
	t.Fatalf("the summary has no result for %s", req.Label())
	return evidence.Scenario{}
}

// A run selected with a label filter whose suite setup failed reports no
// step. The scenarios its selection names are blocked by the setup failure,
// the filtered lane still fails, and the full Kind lane does not pass on the
// same summary.
func TestFilteredRunWhoseSetupFailedBlocksTheSelectedScenarios(t *testing.T) {
	selected := evidence.Requirement{ID: "S03", Variant: s03Graceful}
	startSetupFailedRun(t, selected.Label())
	suite := types.SuiteConfig{LabelFilter: selected.Label()}

	summaryMu.Lock()
	blockSelectedBySetupFailure(suite)
	got := scenarioResult(t, selected)
	other := scenarioResult(t, evidence.Requirement{ID: "S00"})
	summary := runSummary
	summaryMu.Unlock()

	if got.Result != evidence.Blocked || !strings.Contains(got.Reason, "setup failed") {
		t.Errorf("selected scenario = %s %q, want blocked with the setup failure as the reason", got.Result, got.Reason)
	}
	if other.Result != evidence.NotRun {
		t.Errorf("a scenario the filter did not select = %s, want not_run", other.Result)
	}
	lane := laneFor(suite)
	if lane.Name != "kind-filtered" || len(lane.Required) != 1 || lane.Required[0] != selected {
		t.Fatalf("lane = %s requiring %v, want kind-filtered requiring only %v", lane.Name, lane.Required, selected)
	}
	problems := lane.Gate(summary, t.TempDir())
	if len(problems) == 0 {
		t.Error("the filtered lane passed although the setup failed")
	}
	for _, p := range problems {
		if strings.Contains(p, "requires nothing") || strings.Contains(p, "no scenario recorded") {
			t.Errorf("problem %q reports no requirement instead of the blocked scenario", p)
		}
	}
	if len(kindLane.Gate(summary, t.TempDir())) == 0 {
		t.Error("the full Kind lane passed on a filtered run")
	}
}

// An unfiltered run whose setup failed keeps the not_run placeholders that
// planRun recorded and is judged against the full Kind lane.
func TestUnfilteredRunWhoseSetupFailedIsUnchanged(t *testing.T) {
	selected := evidence.Requirement{ID: "S03", Variant: s03Graceful}
	startSetupFailedRun(t, selected.Label())
	suite := types.SuiteConfig{}

	summaryMu.Lock()
	blockSelectedBySetupFailure(suite)
	got := scenarioResult(t, selected)
	summaryMu.Unlock()

	if got.Result != evidence.NotRun || !strings.Contains(got.Reason, "setup failed") {
		t.Errorf("scenario = %s %q, want the not_run placeholder naming the setup", got.Result, got.Reason)
	}
	if lane := laneFor(suite); lane.Name != "kind" || len(lane.Required) != len(kindLane.Required) {
		t.Errorf("lane = %s requiring %d, want the full kind lane", lane.Name, len(lane.Required))
	}
}

// A filtered run whose steps were reported, so whose setup did not fail,
// keeps what ReportAfterEach recorded.
func TestFilteredRunThatReportedStepsIsUnchanged(t *testing.T) {
	selected := evidence.Requirement{ID: "S03", Variant: s03Graceful}
	startSetupFailedRun(t, selected.Label())
	suite := types.SuiteConfig{LabelFilter: selected.Label()}

	summaryMu.Lock()
	reportedLabels[selected.Label()] = true
	blockSelectedBySetupFailure(suite)
	got := scenarioResult(t, selected)
	summaryMu.Unlock()

	if got.Result != evidence.NotRun {
		t.Errorf("scenario = %s, want the placeholder to stay not_run", got.Result)
	}
}
