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
	"context"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	// runLogBytes and runLogTime bound what all the scenarios of a run spend
	// on diagnostic logs together. They are sized so that a hung API server
	// costs a run minutes and not its deadline: runLogTime added to the
	// slowest suite without collection still ends before E2E_SUITE_TIMEOUT
	// (docs/testing/scenario-contract.md, section "Bounds").
	runLogBytes int64 = 128 << 20
	runLogTime        = 5 * time.Minute
	// kubectlLogTimeout bounds one request of a log read.
	kubectlLogTimeout = "15s"
	// kubectlStderrLimit bounds what is kept of kubectl's error output.
	kubectlStderrLimit = 4096
	// kubectlStopDelay is how long kubectl's output may stay open after it
	// was killed.
	kubectlStopDelay = time.Second
)

// concludeLogGrace is how long a scenario's concluding collection goes on after
// the scenario's context ended: half of the suite's grace period, because a
// step that takes a context has that period to return, and it still has to
// record its result after collecting. The slowest checkpoint measured took 10
// seconds, so the default of 30 reads a working cluster to its end.
func concludeLogGrace() time.Duration {
	suite, _ := GinkgoConfiguration()
	return suite.GracePeriod / 2
}

// runLogBudget is what the scenarios of this run may still spend on logs.
var runLogBudget = evidence.NewLogBudget(runLogBytes, runLogTime)

// kubectlLogs reads a cluster's logs for a bundle. Every command it runs is a
// get, a logs or one of the fixed reads of test/evidence. Output goes straight
// to the bundle's memory: not through utils.Run, which would hold all of it
// and print the command as it does for the scenario's own steps.
type kubectlLogs struct{}

func runKubectlLogs(ctx context.Context, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--request-timeout=" + kubectlLogTimeout}, args...)...)
	stderr := evidence.NewLogTail(kubectlStderrLimit)
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = w, stderr, kubectlStopDelay
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr.Bytes())))
	}
	return nil
}

func (kubectlLogs) Pods(ctx context.Context, set evidence.LogPodSet) ([]evidence.LogPod, error) {
	var out strings.Builder
	err := runKubectlLogs(ctx, &out, "-n", set.Namespace, "get", "pods", "-l", set.Selector, "-o",
		`jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.uid}{"\t"}{.spec.nodeName}{"\t"}`+
			`{.status.containerStatuses[?(@.name=="`+set.Container+`")].restartCount}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	var pods []evidence.LogPod
	for line := range strings.Lines(out.String()) {
		fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("unexpected Pod line %q", line)
		}
		// A container without a status has not started: no restart to read.
		restarts, _ := strconv.ParseInt(fields[3], 10, 32)
		pods = append(pods, evidence.LogPod{Name: fields[0], UID: fields[1], Node: fields[2], Restarts: int32(restarts)})
	}
	return pods, nil
}

func (kubectlLogs) PodLog(ctx context.Context, set evidence.LogPodSet, pod evidence.LogPod, previous bool,
	tailLines int64, w io.Writer) error {
	args := []string{"-n", set.Namespace, "logs", pod.Name, "-c", set.Container, "--timestamps",
		"--tail=" + strconv.FormatInt(tailLines, 10)}
	if previous {
		args = append(args, "--previous")
	}
	return runKubectlLogs(ctx, w, args...)
}

func (kubectlLogs) Exec(ctx context.Context, set evidence.LogPodSet, pod evidence.LogPod, command []string,
	w io.Writer) error {
	return runKubectlLogs(ctx, w, append([]string{"-n", set.Namespace, kubectlExec, pod.Name, "-c", set.Container, "--"},
		command...)...)
}

// scenarioLogs collects one scenario's diagnostic logs into <scenario>/logs/.
// Nothing it does can change the scenario's result: a read that fails is named
// in the bundle's index, and the index joins the result's evidence only once
// it exists (Scenario.Judge fails a pass whose listed evidence is missing). A
// run without an evidence directory collects nothing.
type scenarioLogs struct {
	bundle   *evidence.LogBundle
	scenario string
	grace    time.Duration
}

// newScenarioLogs returns the logs of scenario below the run directory dir.
func newScenarioLogs(dir, scenario string, plan evidence.LogPlan, reader evidence.LogReader,
	limits evidence.LogLimits, budget *evidence.LogBudget) *scenarioLogs {
	if dir == "" {
		return &scenarioLogs{scenario: scenario}
	}
	return &scenarioLogs{scenario: scenario, grace: concludeLogGrace(),
		bundle: evidence.NewLogBundle(dir, scenario, redactor, reader, plan, limits, budget)}
}

// scenarioLogs returns the logs of a scenario of this HA cluster.
func (r *haRun) scenarioLogs(scenario string) *scenarioLogs {
	return newScenarioLogs(evidenceDir(), scenario, evidence.ClusterLogPlan(namespace, r.namespace, r.cluster, r.members,
		true), kubectlLogs{}, evidence.DefaultLogLimits(), runLogBudget)
}

// beforeFault keeps what a Pod the fault replaces would take with it. It runs
// no cubrid heartbeat status, which is not a passive read (LogBundle).
func (l *scenarioLogs) beforeFault(ctx context.Context) {
	if l.bundle != nil {
		l.bundle.Checkpoint(ctx, evidence.LogsBeforeFault, false)
	}
}

// conclude collects the logs once the scenario has its result, and adds the
// bundle's index to result's evidence. faultEnded says that the scenario's
// fault was removed, or that it injected none: only then may cubrid heartbeat
// status run. It is meant for a deferred call, so that a failed check still
// leaves its logs, also once a deadline has cancelled ctx (evidence.LogContext),
// and it reads result and sets nothing but result.Evidence.
func (l *scenarioLogs) conclude(ctx context.Context, faultEnded bool, result *evidence.Scenario) {
	if l.bundle == nil {
		return
	}
	ctx, stop := evidence.LogContext(ctx, l.grace)
	defer stop()
	l.bundle.Checkpoint(ctx, evidence.LogsAfterVerdict, faultEnded)
	if err := l.bundle.Problem(); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: diagnostic logs of %s: %v\n", l.scenario, err)
		updateSummary(func(s *evidence.Summary) {
			s.Notes = append(s.Notes, "The diagnostic logs of "+l.scenario+" are incomplete: "+err.Error())
		})
	}
	if file := l.bundle.IndexFile(); file != "" && !slices.Contains(result.Evidence, file) {
		result.Evidence = append(result.Evidence, file)
	}
}
