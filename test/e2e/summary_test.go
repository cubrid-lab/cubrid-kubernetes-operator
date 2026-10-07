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
	"time"

	. "github.com/onsi/ginkgo/v2"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// runSummary collects what the scenarios of this run report. It is written
// once, after the suite, in the format of docs/testing/scenario-contract.md.
var runSummary = evidence.Summary{
	StartedAt:   time.Now().UTC(),
	Environment: evidence.Environment{Level: "kind"},
}

// redactor removes credentials from every evidence file of the run, among
// them the Instance Manager token the suite itself sets.
var redactor = evidence.NewRedactor(instanceManagerToken)

// evidenceDir is where the run's evidence goes; empty when none is kept, and
// then no scenario can pass.
func evidenceDir() string { return os.Getenv("E2E_EVIDENCE_DIR") }

// recordScenario adds a scenario's result to the run's summary.
func recordScenario(s evidence.Scenario) {
	runSummary.Scenarios = append(runSummary.Scenarios, s)
}

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
	}{{"S03", s03Variants}, {"S05", s05Variants}, {"S06", s06Variants}} {
		for _, v := range scenario.variants {
			required = append(required, evidence.Requirement{ID: scenario.id, Variant: v})
		}
	}
	return required
}

// runLane returns the lane this run is judged against. A run that selects
// specs with a label filter or focus is a local baseline, not a validation of
// the Kind lane: it must pass every scenario it recorded, and its summary
// names it "kind-filtered".
func runLane() evidence.Lane {
	suite, _ := GinkgoConfiguration()
	if suite.LabelFilter == "" && len(suite.FocusStrings) == 0 && len(suite.SkipStrings) == 0 &&
		len(suite.FocusFiles) == 0 && len(suite.SkipFiles) == 0 {
		return kindLane
	}
	lane := evidence.Lane{Name: "kind-filtered"}
	for _, sc := range runSummary.Scenarios {
		if req := (evidence.Requirement{ID: sc.ID, Variant: sc.Variant}); !slices.Contains(lane.Required, req) {
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
	runSummary.FinishedAt = time.Now().UTC()
	runSummary.RunID = runSummary.StartedAt.Format("2006-01-02T15-04-05Z")
	env := &runSummary.Environment
	if rev, err := utils.Run(exec.Command("git", "rev-parse", "HEAD")); err == nil {
		env.OperatorCommit = strings.TrimSpace(rev)
	}
	if out, err := utils.Run(exec.Command("kubectl", "version", "-o", "json")); err == nil {
		var v struct {
			ServerVersion struct {
				GitVersion string `json:"gitVersion"`
			} `json:"serverVersion"`
		}
		if json.Unmarshal([]byte(out), &v) == nil {
			env.KubernetesVersion = v.ServerVersion.GitVersion
		}
	}
	if out, err := utils.Run(exec.Command(utils.ContainerTool(), "image", "inspect",
		"--format", "{{.Id}}", managerImage)); err == nil {
		env.OperatorImageDigest = strings.TrimSpace(out)
	}
	return runSummary.Conclude(evidenceDir(), redactor, runLane())
}
