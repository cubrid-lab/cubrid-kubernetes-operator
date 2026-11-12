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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
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

// acquiredEvidenceDir is the directory this run accepted as its own, which
// acquireEvidenceDir does only for a missing or empty directory, and is empty
// otherwise. Ginkgo runs AfterSuite and the per-spec reports even when
// BeforeSuite refused the directory, so every write goes through this path,
// not through E2E_EVIDENCE_DIR, which can name a directory that was never
// accepted (#310).
var acquiredEvidenceDir string

// acquireEvidenceDir makes E2E_EVIDENCE_DIR this run's evidence directory
// when it is missing or empty. It returns an error when the directory holds
// the files of an earlier run. An earlier acquisition ends first, so that a
// refused run holds no directory and writes nowhere, whatever was accepted
// before in this process.
func acquireEvidenceDir() error {
	acquiredEvidenceDir = ""
	dir := os.Getenv("E2E_EVIDENCE_DIR")
	if dir == "" {
		return nil
	}
	if err := evidence.RequireEmptyDir(dir); err != nil {
		return err
	}
	acquiredEvidenceDir = dir
	return nil
}

// evidenceDir is where the run's evidence goes; empty when none is kept or
// the run did not acquire the directory, and then no scenario can pass.
func evidenceDir() string { return acquiredEvidenceDir }

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
	if s.Result != evidence.NotRun {
		if s.Measurements == nil {
			s.Measurements = map[string]any{}
		}
		s.Measurements[lostSessionsMeasurement] = csqlLedger.measure(
			evidence.Requirement{ID: s.ID, Variant: s.Variant}.Label())
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
	// lastFailureImageLoad is the load error when the step that failed last
	// failed with it, and nil otherwise.
	lastFailureImageLoad *utils.ImageLoadError
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
	suite, _ := GinkgoConfiguration()
	// checkSelection's PreviewSpecs walks the suite as a dry run, which runs
	// no step and records nothing.
	if suite.DryRun {
		return
	}
	defer checkpointRun()
	summaryMu.Lock()
	defer summaryMu.Unlock()
	selected := evidence.Selected(report, suite, suiteDescription)
	// A group whose setup failed to load the Instance Manager image, and the
	// steps it then did not start, are blocked by that environment problem.
	loadErr := blockedByImageLoad(instanceManagerImageErr, report.Failure.Message)
	if loadErr == nil && !report.Failed() {
		loadErr = lastFailureImageLoad
	}
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
		result, reason, ok := evidence.StepOutcome(report, selected, lastFailedStep)
		if ok && result == evidence.Blocked && loadErr != nil {
			reason = imageLoadReason(loadErr, imageLoadEvidence)
			writeScenarioFile(imageLoadEvidence, []byte(loadErr.Error()+"\n"))
		}
		if ok {
			runSummary.Settle(req, result, reason)
		}
	}
	if report.Failed() {
		lastFailedStep = report.LeafNodeText
		if report.Failure.FailureNodeType == types.NodeTypeBeforeAll {
			lastFailedStep += " (in the group's setup)"
		}
		lastFailureImageLoad = loadErr
		if loadErr != nil {
			lastFailedStep = imageLoadReason(loadErr, imageLoadEvidence)
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
	}{{"S03", s03Variants}, {"S05", s05Variants}, {"S06", s06Variants}, {"S13", s13Variants},
		{"S14", s14TokenVariants}} {
		for _, v := range scenario.variants {
			required = append(required, evidence.Requirement{ID: scenario.id, Variant: v})
		}
	}
	return required
}

// A failed Kind run is read by people, so every result the lane requires must
// have a description: a requirement left without one would be reported as a
// bare scenario key. Matching never uses it. test/evidence pins the text.
func TestKindRequirementsHaveReadableNames(t *testing.T) {
	if len(kindLane.Required) == 0 {
		t.Fatal("the Kind lane requires nothing")
	}
	for _, req := range kindLane.Required {
		sc := evidence.Scenario{ID: req.ID, Variant: req.Variant}
		if sc.DisplayName() == sc.Name() {
			t.Errorf("required result %s has no description", sc.Name())
		}
	}
}

// The registry must describe this lane exactly: a requirement that maps to no
// registered result, or a registered result nothing requires, fails here.
func TestKindLaneIsRegistered(t *testing.T) {
	for _, problem := range kindLane.RegistryProblems(evidence.LevelKind) {
		t.Error(problem)
	}
}

// runLane returns the lane this run is judged against. A run that selects
// specs with a label filter or focus is a local baseline, not a validation of
// the Kind lane: it must select a step of at least one scenario and pass
// every scenario it selected a step of, and its summary names it
// "kind-filtered". The scenarios it did not select stay in the summary as
// not_run.
func runLane() evidence.Lane {
	suite, _ := GinkgoConfiguration()
	return laneFor(suite)
}

// laneFor is runLane for the given suite configuration.
func laneFor(suite types.SuiteConfig) evidence.Lane {
	if !filtered(suite) {
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

// filtered reports whether the run selects specs with a label, semantic
// version, focus, skip or file filter.
func filtered(suite types.SuiteConfig) bool {
	return suite.LabelFilter != "" || suite.SemVerFilter != "" || len(suite.FocusStrings) > 0 ||
		len(suite.SkipStrings) > 0 || len(suite.FocusFiles) > 0 || len(suite.SkipFiles) > 0
}

// checkSelection returns an error, before the suite sets anything up, when a
// scenario's descriptive key does not select exactly the steps of its legacy
// label (evidence.KindLabels), when E2E_LABEL_FILTER names a label or pattern
// no step carries, or when the run's filters select no step. Ginkgo runs
// neither BeforeSuite nor AfterSuite, and so no gate, when nothing is
// selected, and such a run would otherwise pass.
func checkSelection(report Report) error {
	suite, _ := GinkgoConfiguration()
	var steps [][]string
	selected := false
	for _, spec := range report.SpecReports {
		if spec.LeafNodeType != types.NodeTypeIt {
			continue
		}
		steps = append(steps, spec.Labels())
		if evidence.Selected(spec, suite, suiteDescription) {
			selected = true
			markSelectedSteps(spec.Labels())
		}
	}
	problems := evidence.KeyLabelProblems(steps)
	if err := evidence.CheckLabelFilter(suite.LabelFilter, steps); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 && filtered(suite) && !selected {
		problems = append(problems, "the run's filters select no step of the suite")
	}
	if len(problems) > 0 {
		return fmt.Errorf("the run's selection of scenarios is not valid:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

// markSelectedSteps records the scenarios a selected step carries the label
// of. checkSelection does it before the suite starts, so that a filtered run
// whose setup fails, and so reports no step, still knows what it selected.
func markSelectedSteps(labels []string) {
	summaryMu.Lock()
	defer summaryMu.Unlock()
	for _, req := range kindLane.Required {
		if slices.Contains(labels, req.Label()) {
			selectedSteps[req] = true
		}
	}
}

// blockSelectedBySetupFailure settles each scenario a filtered run selected a
// step of as blocked when no step was reported at all: Ginkgo reports no step
// when the suite's setup fails. An unfiltered run keeps the not_run
// placeholders of planRun. The caller holds summaryMu.
func blockSelectedBySetupFailure(suite types.SuiteConfig) {
	if !filtered(suite) || len(reportedLabels) > 0 {
		return
	}
	for _, req := range laneFor(suite).Required {
		runSummary.Settle(req, evidence.Blocked,
			"the suite's setup failed before any step started, so this selected scenario could not run")
	}
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
	suite, _ := GinkgoConfiguration()
	blockSelectedBySetupFailure(suite)
	var problems []string
	if len(reportedLabels) > 0 {
		problems = evidence.UnlabelledSteps(kindLane.Required, reportedLabels)
	}
	if timedOutStep != "" {
		problems = append(problems, fmt.Sprintf(
			"the suite's deadline passed during %s: the steps after it did not run", timedOutStep))
	}
	problems = append(problems, credentialLeaks(evidenceDir())...)
	if note := csqlLedger.note(); note != "" {
		runSummary.Notes = append(runSummary.Notes, note)
	}
	err := runSummary.Conclude(evidenceDir(), redactor, runLane(), problems...)
	// The report just written is the run's final one. "go test" runs the
	// package's unit tests in this process after the suite, and their own
	// plans and checkpoints must not replace it (#807): the run gives up its
	// directory, so nothing more is written there.
	acquiredEvidenceDir = ""
	return err
}

// evidenceHoldSuffix names the marker beside the evidence directory that keeps
// the workflow from publishing it (.github/workflows/test-e2e.yml).
const evidenceHoldSuffix = ".held"

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
		problem := fmt.Sprintf("the evidence could not be searched for credentials: %v", err)
		holdEvidence(dir, problem)
		return []string{problem}
	}
	problems := make([]string, 0, len(leaks))
	for _, file := range leaks {
		problems = append(problems, "the evidence file "+file+" holds a credential of the run")
	}
	if len(problems) > 0 {
		holdEvidence(dir, strings.Join(problems, "\n"))
	}
	return problems
}

// holdEvidence writes the marker that keeps the evidence directory from being
// published. The run fails either way: a file that holds a credential is not
// removed, because the workflow's logs name it and the runner discards it. It
// names the files only, and a marker that could not be written is printed.
func holdEvidence(dir, reason string) {
	if err := os.WriteFile(dir+evidenceHoldSuffix, []byte(reason+"\n"), 0o600); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "could not hold the evidence: %v\n", err)
	}
}

// imageLoadEvidence is where the output of the attempts to load the Instance
// Manager image is kept when a group of scenarios is blocked by the failure.
const imageLoadEvidence = "suite/image-load.txt"

// imageLoadReason is the reason of a scenario that the failure to load the
// Instance Manager image into Kind blocked. Loading the image does not
// exercise the Operator, so the failure is a problem of the test environment.
func imageLoadReason(loadErr *utils.ImageLoadError, file string) string {
	return fmt.Sprintf("environment_image_load_failed: the Instance Manager image could not be "+
		"loaded into Kind after %d attempt(s); this is a problem of the test environment, not of the "+
		"Operator (full output in %s)", len(loadErr.Attempts), file)
}

// blockedByImageLoad returns the loading error when a step failed with it:
// the image's outcome is a failed load and the failure message of the step
// quotes that error.
func blockedByImageLoad(imageErr error, failureMessage string) *utils.ImageLoadError {
	var loadErr *utils.ImageLoadError
	if errors.As(imageErr, &loadErr) && strings.Contains(failureMessage, utils.ImageLoadProblem) {
		return loadErr
	}
	return nil
}

// blockByImageLoad records on the S00 record that the scenario is blocked when
// the Instance Manager image could not be loaded, and returns that error.
// Otherwise it changes nothing and returns nil.
func (e *s00Evidence) blockByImageLoad(imageErr error) *utils.ImageLoadError {
	var loadErr *utils.ImageLoadError
	if !errors.As(imageErr, &loadErr) {
		return nil
	}
	e.Result = string(evidence.Blocked)
	e.Reason = imageLoadReason(loadErr, "S00/image-load.txt")
	return loadErr
}
