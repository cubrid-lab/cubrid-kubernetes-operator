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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// runSummary collects what the scenarios of this run report. It is written
// once, after the suite, in the format of docs/testing/scenario-contract.md.
var runSummary = evidence.Summary{
	StartedAt:   time.Now().UTC(),
	Environment: evidence.Environment{Level: "kind"},
}

// summaryMu guards runSummary and the state below. A step Ginkgo abandons at
// the suite's deadline keeps running and may still record its result while
// the cleanup nodes and AfterSuite read the summary.
var summaryMu sync.Mutex

// timedOutStep names the step the suite's deadline stopped, or is empty.
// After it, no step's result is recorded: an abandoned step's late result is
// not evidence.
var timedOutStep string

// updateSummary changes the run's summary under summaryMu.
func updateSummary(change func(s *evidence.Summary)) {
	summaryMu.Lock()
	defer summaryMu.Unlock()
	change(&runSummary)
}

// redactor removes credentials from every evidence file of the run, among
// them the cluster Instance Manager tokens the suite reads (S14).
var redactor = evidence.NewRedactor()

// evidenceDir is where the run's evidence goes; empty when none is kept, and
// then no scenario can pass.
func evidenceDir() string { return os.Getenv("E2E_EVIDENCE_DIR") }

// recordScenario adds a scenario's result to the run's summary, in place of
// the placeholder planRun left for it.
func recordScenario(s evidence.Scenario) {
	summaryMu.Lock()
	defer summaryMu.Unlock()
	if timedOutStep != "" {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: %s recorded %s after the suite's deadline; not kept\n",
			s.Name(), s.Result)
		return
	}
	runSummary.Record(s)
}

// suiteDescription is the description the suite runs with; Ginkgo matches
// focus and skip expressions against it and the text of each spec.
const suiteDescription = "e2e suite"

var (
	// selectedSteps holds each scenario and variant the run's filters
	// selected at least one step of.
	selectedSteps = map[evidence.Requirement]bool{}
	// lastFailedStep names the step whose failure the run saw last.
	lastFailedStep string
	// reportedLabels holds the labels of every step the run reported,
	// whether it ran or not.
	reportedLabels = map[string]bool{}
)

// planRun records every scenario and variant of the Kind lane as not_run
// before the suite sets anything up. A step's result takes the place of its
// scenario's placeholder; a scenario none of whose steps recorded a result
// keeps the reason the ReportAfterEach below gives it, or this one.
func planRun() {
	summaryMu.Lock()
	runSummary.RunID = runSummary.StartedAt.Format("2006-01-02T15-04-05Z")
	runSummary.Plan(kindLane.Required,
		"no step of the scenario started: the suite's setup failed or the run stopped before it")
	summaryMu.Unlock()
	checkpointRun()
}

// checkpointRun writes the results recorded so far as a summary.json that
// does not pass, so that a run killed before concludeRun still leaves its
// plan and results. concludeRun replaces it.
func checkpointRun() {
	summaryMu.Lock()
	defer summaryMu.Unlock()
	dir := evidenceDir()
	if dir == "" {
		return
	}
	if err := runSummary.Checkpoint(dir, redactor, runLane().Name, time.Now()); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: run checkpoint: %v\n", err)
	}
}

// The steps of a scenario carry its requirement's label (evidence.Requirement
// Label): "S00", "S03-abrupt". After each spec, whether it ran or not, a
// scenario whose step recorded nothing is given the result that says why.
var _ = ReportAfterEach(func(report SpecReport) {
	defer checkpointRun()
	summaryMu.Lock()
	defer summaryMu.Unlock()
	suite, _ := GinkgoConfiguration()
	selected := evidence.Selected(report, suite, suiteDescription)
	for _, label := range report.Labels() {
		reportedLabels[label] = true
	}
	for _, req := range kindLane.Required {
		if !slices.Contains(report.Labels(), req.Label()) {
			continue
		}
		if selected {
			selectedSteps[req] = true
		}
		if result, reason, ok := evidence.StepOutcome(report, selected, lastFailedStep); ok {
			runSummary.Settle(req, result, reason)
		}
	}
	if report.Failed() {
		lastFailedStep = report.LeafNodeText
		if report.Failure.FailureNodeType == types.NodeTypeBeforeAll {
			lastFailedStep += " (in the group's setup)"
		}
	}
	// No step sets its own timeout: a timed-out step was stopped by the
	// suite's deadline.
	if report.State == types.SpecStateTimedout && timedOutStep == "" {
		timedOutStep = fmt.Sprintf("%q (in its %s node)", report.LeafNodeText, report.Failure.FailureNodeType)
	}
})

// writeScenarioFile stores one evidence file of a scenario and returns its
// path relative to the run directory, or "" when no evidence is kept or the
// file could not be written.
func writeScenarioFile(rel string, data []byte) string {
	dir := evidenceDir()
	if dir == "" {
		return ""
	}
	if err := redactor.WriteFile(dir, rel, data); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: evidence file %s: %v\n", rel, err)
		return ""
	}
	return rel
}

// kindLane is what a full run of this suite on Kind must pass: every scenario
// and variant it records. No result is allowed to be not_applicable: the
// lane runs on linux/amd64, where each of them applies. The list is kept in
// docs/testing/scenario-contract.md, section "Required scenarios"; change it
// there and here together.
var kindLane = evidence.Lane{Name: "kind", Required: kindRequired()}

func kindRequired() []evidence.Requirement {
	required := []evidence.Requirement{{ID: "S00"}, {ID: "S01"}, {ID: "S02"}, {ID: "S14"}}
	for _, scenario := range []struct {
		id       string
		variants []string
	}{{"S03", s03Variants}, {"S05", s05Variants}, {"S06", s06Variants}, {"S14", s14TokenVariants}} {
		for _, v := range scenario.variants {
			required = append(required, evidence.Requirement{ID: scenario.id, Variant: v})
		}
	}
	return required
}

// runLane returns the lane this run is judged against. A run that selects
// specs with a label filter or focus is a local baseline, not a validation of
// the Kind lane: it must select a step of at least one scenario and pass
// every scenario it selected a step of, and its summary names it
// "kind-filtered". The scenarios it did not select stay in the summary as
// not_run.
func runLane() evidence.Lane {
	suite, _ := GinkgoConfiguration()
	if suite.LabelFilter == "" && suite.SemVerFilter == "" && len(suite.FocusStrings) == 0 &&
		len(suite.SkipStrings) == 0 && len(suite.FocusFiles) == 0 && len(suite.SkipFiles) == 0 {
		return kindLane
	}
	lane := evidence.Lane{Name: "kind-filtered"}
	for _, req := range kindLane.Required {
		if selectedSteps[req] {
			lane.Required = append(lane.Required, req)
		}
	}
	return lane
}

// concludeRun writes summary.json and junit.xml to E2E_EVIDENCE_DIR and
// returns an error when the run did not pass its lane or the files could not
// be written. It needs the Kind cluster, so it runs before the suite's
// cleanup.
func concludeRun() error {
	// Queried before the lock: a step abandoned at the deadline may still
	// record, and kubectl is bounded so that a hung API server leaves time
	// to write the summary within the grace period.
	commit, kubeVersion, imageID := "", "", ""
	if rev, err := utils.Run(exec.Command("git", "rev-parse", "HEAD")); err == nil {
		commit = strings.TrimSpace(rev)
	}
	if out, err := utils.Run(exec.Command("kubectl", "version", "-o", "json", "--request-timeout=10s")); err == nil {
		var v struct {
			ServerVersion struct {
				GitVersion string `json:"gitVersion"`
			} `json:"serverVersion"`
		}
		if json.Unmarshal([]byte(out), &v) == nil {
			kubeVersion = v.ServerVersion.GitVersion
		}
	}
	if out, err := utils.Run(exec.Command(utils.ContainerTool(), "image", "inspect",
		"--format", "{{.Id}}", managerImage)); err == nil {
		imageID = strings.TrimSpace(out)
	}

	summaryMu.Lock()
	defer summaryMu.Unlock()
	runSummary.FinishedAt = time.Now().UTC()
	env := &runSummary.Environment
	env.OperatorCommit, env.KubernetesVersion, env.OperatorImageDigest = commit, kubeVersion, imageID
	// A required scenario no step carries the label of can never record a
	// result. When no step was reported at all, the suite's setup failed and
	// the placeholders say so.
	var problems []string
	if len(reportedLabels) > 0 {
		problems = evidence.UnlabelledSteps(kindLane.Required, reportedLabels)
	}
	if timedOutStep != "" {
		problems = append(problems, fmt.Sprintf(
			"the suite's deadline passed during %s: the steps after it did not run", timedOutStep))
	}
	problems = append(problems, credentialLeaks(evidenceDir())...)
	return runSummary.Conclude(evidenceDir(), redactor, runLane(), problems...)
}

// credentialLeaks is a problem for each evidence file of the run that holds a
// credential the run registered with the redactor, such as the Instance
// Manager tokens and the object-storage secret key (#587), and one if the
// files could not be searched. It names the files only. It runs before
// Conclude writes summary.json and junit.xml, which pass through the same
// redactor.
func credentialLeaks(dir string) []string {
	if dir == "" {
		return nil
	}
	leaks, err := redactor.Leaks(dir)
	if err != nil {
		return []string{fmt.Sprintf("the evidence could not be searched for credentials: %v", err)}
	}
	problems := make([]string, 0, len(leaks))
	for _, file := range leaks {
		problems = append(problems, "the evidence file "+file+" holds a credential of the run")
	}
	return problems
}
