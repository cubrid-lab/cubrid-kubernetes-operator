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

// evidenceDir is where the run's evidence goes; empty when none is kept.
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

// writeRunSummary stores summary.json and junit.xml when E2E_EVIDENCE_DIR is
// set. It needs the Kind cluster, so it runs before the suite's cleanup.
func writeRunSummary() {
	dir := evidenceDir()
	if dir == "" {
		return
	}
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
	if err := runSummary.Write(dir, redactor); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: run summary: %v\n", err)
	}
}
