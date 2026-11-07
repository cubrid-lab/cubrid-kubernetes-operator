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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

const (
	// runLogBytes and runLogTime bound what all the scenarios of a run spend
	// on diagnostic logs together. They are sized so that a hung API server
	// costs a run minutes and not its deadline: runLogTime added to the
	// slowest suite without collection still ends before E2E_SUITE_TIMEOUT
	// (docs/testing/scenario-contract.md, section "Bounds").
	runLogBytes int64 = 128 << 20
	runLogTime        = 5 * time.Minute
	// runStateBytes and runStateTime bound what all the scenarios of a run
	// spend on Kubernetes snapshots together. They are apart from the logs'
	// budget, of which a run with every scenario passing used 4.6 minutes of 5,
	// so that neither can use up the other. With the logs' 5 minutes these 90
	// seconds still end the slowest suite without collection before
	// E2E_SUITE_TIMEOUT (docs/testing/scenario-contract.md, section "cluster/").
	runStateBytes int64 = 64 << 20
	runStateTime        = 90 * time.Second
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

// runLogBudget is what the scenarios of this run may still spend on logs, and
// runStateBudget what they may spend on Kubernetes snapshots.
var (
	runLogBudget   = evidence.NewLogBudget(runLogBytes, runLogTime)
	runStateBudget = evidence.NewLogBudget(runStateBytes, runStateTime)
)

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

// kubectlState lists a cluster's objects for a snapshot. Every command it runs
// is a get of a resource of evidence.ClusterStatePlan.
type kubectlState struct{}

func (kubectlState) List(ctx context.Context, list evidence.StateList, w io.Writer) error {
	args := []string{"-n", list.Namespace, "get", list.Resource, "-o", "json"}
	if list.Selector != "" {
		args = append(args, "-l", list.Selector)
	}
	if list.Field != "" {
		args = append(args, "--field-selector", list.Field)
	}
	return runKubectlLogs(ctx, w, args...)
}

// scenarioLogs collects one scenario's diagnostic logs into <scenario>/logs/
// and its Kubernetes snapshots into <scenario>/cluster/. Nothing it does can
// change the scenario's result: a read that fails is named in the bundle's
// index, and an index joins the result's evidence only once it exists
// (Scenario.Judge fails a pass whose listed evidence is missing). A run without
// an evidence directory collects nothing.
type scenarioLogs struct {
	bundle   *evidence.LogBundle
	state    *evidence.StateBundle
	scenario string
	grace    time.Duration
	// duringFault waits for the snapshot taken when the fault was confirmed.
	duringFault sync.WaitGroup
	// concluding, under mu, refuses that snapshot once conclude began, so that
	// duringFault has nothing to add while it is waited for.
	mu         sync.Mutex
	concluding bool
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

// keepState adds the Kubernetes snapshots of plan. The scenario begins now: an
// Event last observed before this call is not evidence of it.
func (l *scenarioLogs) keepState(dir string, plan evidence.StatePlan, reader evidence.StateReader,
	limits evidence.StateLimits, budget *evidence.LogBudget) {
	if l.bundle != nil {
		l.state = evidence.NewStateBundle(dir, l.scenario, redactor, reader, plan, limits, budget, time.Now())
	}
}

// scenarioLogs returns the logs of a scenario of this HA cluster. It is called
// when the scenario begins, which also starts the window of its Events.
func (r *haRun) scenarioLogs(scenario string) *scenarioLogs {
	logs := newScenarioLogs(evidenceDir(), scenario, evidence.ClusterLogPlan(namespace, r.namespace, r.cluster, r.members,
		true), kubectlLogs{}, evidence.DefaultLogLimits(), runLogBudget)
	logs.keepState(evidenceDir(), evidence.ClusterStatePlan(namespace, r.namespace, r.cluster, r.members),
		kubectlState{}, evidence.DefaultStateLimits(), runStateBudget)
	return logs
}

// beforeFault keeps what a Pod the fault replaces would take with it. It runs
// no cubrid heartbeat status, which is not a passive read (LogBundle). The
// snapshot comes last, to be the closest to the fault.
func (l *scenarioLogs) beforeFault(ctx context.Context) {
	if l.bundle != nil {
		l.bundle.Checkpoint(ctx, evidence.LogsBeforeFault, false)
	}
	if l.state != nil {
		l.state.Checkpoint(ctx, evidence.LogsBeforeFault)
	}
}

// observedFault marks a fault's steps in the scenario's snapshots and takes a
// snapshot once the fault was confirmed. It only watches: every result is the
// fault's own, the runner of the fault does not know of it, and the snapshot
// runs beside the scenario, so that it neither delays the check of the outcome
// nor can decide it. A confirmation whose context has ended is the fault's to
// report, and faults.Run discards it, so nothing here records it. The check can
// lose by the moment between it and the runner's: that is why a reader of the
// result goes by the outcome and not by the snapshot's presence.
type observedFault struct {
	faults.Fault
	logs      *scenarioLogs
	confirmed sync.Once
}

// observeFault returns fault with its snapshot, or fault itself in a run that
// collects none.
func (l *scenarioLogs) observeFault(fault faults.Fault) faults.Fault {
	if l.state == nil {
		return fault
	}
	return &observedFault{Fault: fault, logs: l}
}

func (f *observedFault) Inject(ctx context.Context) error {
	f.logs.state.Mark("fault_issued", f.Name())
	return f.Fault.Inject(ctx)
}

func (f *observedFault) Confirm(ctx context.Context) (bool, error) {
	done, err := f.Fault.Confirm(ctx)
	if done && err == nil && ctx.Err() == nil {
		f.confirmed.Do(func() { f.logs.faultConfirmed(f.Name()) })
	}
	return done, err
}

// faultConfirmed marks the confirmation and starts its snapshot, unless the
// scenario is already concluding, as it is for a step the runner left behind.
func (l *scenarioLogs) faultConfirmed(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.concluding {
		return
	}
	l.state.Mark("fault_confirmed", name)
	l.duringFault.Go(func() {
		l.state.Checkpoint(context.Background(), evidence.StateFaultConfirmed)
	})
}

func (f *observedFault) Remove(ctx context.Context) error {
	err := f.Fault.Remove(ctx)
	if err == nil {
		f.logs.state.Mark("fault_removed", f.Name())
	}
	return err
}

// conclude collects the logs once the scenario has its result, and adds the
// bundle's index to result's evidence. faultEnded says that the scenario's
// fault was removed, or that it injected none: only then may cubrid heartbeat
// status run. It is meant for a deferred call, so that a failed check still
// leaves its logs, also once a deadline has cancelled ctx (evidence.LogContext),
// and it reads result and sets nothing but result.Evidence.
//
// The last snapshot comes first because it is the closest to the verdict and
// costs a few reads, but the logs keep the whole grace period they had without
// it. So it ends with ctx, and the grace period starts after it. A scenario
// whose ctx has already ended takes the logs first, and the snapshot gets what
// is left of the grace period.
func (l *scenarioLogs) conclude(ctx context.Context, faultEnded bool, result *evidence.Scenario) {
	if l.bundle == nil {
		return
	}
	l.mu.Lock()
	l.concluding = true
	l.mu.Unlock()
	expired := ctx.Err() != nil
	if !expired {
		l.concludeState(ctx, result)
	}
	logs, stop := evidence.LogContext(ctx, l.grace)
	defer stop()
	l.bundle.Checkpoint(logs, evidence.LogsAfterVerdict, faultEnded)
	l.noteProblem("diagnostic logs", l.bundle.Problem())
	addEvidence(result, l.bundle.IndexFile())
	if expired {
		l.concludeState(logs, result)
	}
}

// concludeState takes the last snapshot. The snapshot of the fault still
// running at the end of ctx is left to its own bound, and the last one is then
// not taken: the run's notes say so.
func (l *scenarioLogs) concludeState(ctx context.Context, result *evidence.Scenario) {
	if l.state == nil {
		return
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		l.duringFault.Wait()
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		l.noteProblem("Kubernetes snapshots", ctx.Err())
		return
	}
	l.state.Checkpoint(ctx, evidence.LogsAfterVerdict)
	l.noteProblem("Kubernetes snapshots", l.state.Problem())
	addEvidence(result, l.state.IndexFile())
}

func (l *scenarioLogs) noteProblem(what string, err error) {
	if err == nil {
		return
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %s of %s: %v\n", what, l.scenario, err)
	updateSummary(func(s *evidence.Summary) {
		s.Notes = append(s.Notes, "The "+what+" of "+l.scenario+" are incomplete: "+err.Error())
	})
}

// addEvidence lists an index once it exists.
func addEvidence(result *evidence.Scenario, file string) {
	if file != "" && !slices.Contains(result.Evidence, file) {
		result.Evidence = append(result.Evidence, file)
	}
}
