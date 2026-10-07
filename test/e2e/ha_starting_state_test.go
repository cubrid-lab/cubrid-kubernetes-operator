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
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// startingStateLimit bounds the wait for the common starting state before an
// HA step. A failed variant leaves at most a Pod or a Broker being replaced,
// or the Operator scaled down; each comes back well within it.
const startingStateLimit = 10 * time.Minute

// startingStateBudget bounds all those waits together, so that the run ends
// within the suite's time limit and writes its summary also when steps keep
// leaving damage behind.
const startingStateBudget = 15 * time.Minute

// startingStateSettle is how long the starting state must keep holding: a
// Pod or a Broker that is about to go away is not verified as back.
const startingStateSettle = 15 * time.Second

// The steps of the HA group that form the cluster, and those that follow,
// run in order on one cluster. A failed step does not keep the steps after it
// from starting: each of them first restores and verifies the starting state
// of S01 (enter), and is blocked when that state cannot be restored.

// breakStepEnv names the requirement label of one HA step (for example
// S05-one-broker-pod) that is made to fail on purpose, once its starting state
// is verified, leaving damage behind as a failed variant could: a client that
// keeps writing, a read-write Broker Pod deleted without a grace period, and
// the Operator scaled to zero. It shows that the steps after it start from a
// restored starting state. A run that sets it does not pass its lane.
const breakStepEnv = "E2E_BREAK_STEP"

// breakUnrestorableEnv, set to "true" with breakStepEnv, also deletes the
// workload client Pod, without which the starting state cannot be verified,
// so that every later step is blocked.
const breakUnrestorableEnv = "E2E_BREAK_STEP_UNRESTORABLE"

// newGate returns the gate in front of the HA steps after S01.
func (r *haRun) newGate() *faults.Gate {
	return &faults.Gate{
		Limit:    startingStateLimit,
		Poll:     5 * time.Second,
		Settle:   startingStateSettle,
		Budget:   startingStateBudget,
		Restore:  func(context.Context) error { return r.restoreStartingState() },
		Verify:   func(context.Context) error { return r.verifyStartingState() },
		Describe: func(context.Context) string { return r.memberIdentities() },
	}
}

// checkBreakSettings fails the run when breakStepEnv does not name an HA
// step that starts through the gate, or when breakUnrestorableEnv is set
// without it: a mistyped name would break nothing and the run would look
// like a demonstration that was never made.
func checkBreakSettings() error {
	step, unrestorable := os.Getenv(breakStepEnv), os.Getenv(breakUnrestorableEnv)
	if step == "" {
		if unrestorable != "" {
			return fmt.Errorf("%s is set without %s", breakUnrestorableEnv, breakStepEnv)
		}
		return nil
	}
	for _, req := range kindLane.Required {
		if req.Label() == step && req.ID != "S00" && req.ID != "S01" {
			return nil
		}
	}
	return fmt.Errorf("%s=%q is not the label of an HA step after S01", breakStepEnv, step)
}

// memberIdentities is the UID and the restart count of each member's Pod.
func (r *haRun) memberIdentities() string {
	ids := make([]string, 0, len(r.members))
	for _, member := range r.members {
		out, err := r.kubectl("get", "pod", member,
			"-o", "jsonpath={.metadata.uid}/{.status.containerStatuses[0].restartCount}")
		if err != nil {
			out = "unknown"
		}
		ids = append(ids, member+"="+out)
	}
	return strings.Join(ids, " ")
}

// restoreStartingState puts back what a failed step may have left, where
// that is safe: it stops the clients it left writing and adds what they
// recorded to the run's history, and it scales the Operator back to one
// replica. Pods and Brokers are brought back by their controllers and the
// Operator; the database is never touched.
func (r *haRun) restoreStartingState() error {
	r.mu.Lock()
	running := slices.Sorted(maps.Keys(r.running))
	r.mu.Unlock()
	for _, id := range running {
		text, err := r.collectClient(id)
		if err != nil {
			return fmt.Errorf("collecting the history of client %s: %w", id, err)
		}
		if _, err := workload.ReadHistory(strings.NewReader(text)); err != nil {
			return fmt.Errorf("history of client %s: %w", id, err)
		}
		r.addHistory(text)
		_, _ = fmt.Fprintf(GinkgoWriter, "stopped client %s, left running by an earlier step\n", id)
	}
	replicas, err := utils.Run(exec.Command("kubectl", "-n", namespace, "get", operatorDeployment,
		"-o", "jsonpath={.spec.replicas}"))
	if err != nil {
		return err
	}
	if replicas != "1" {
		_, _ = fmt.Fprintf(GinkgoWriter, "scaling the Operator from %s replicas back to 1\n", replicas)
		if _, err := utils.Run(exec.Command("kubectl", "-n", namespace, "scale", operatorDeployment,
			"--replicas=1")); err != nil {
			return err
		}
	}
	return nil
}

// verifyStartingState returns what of the common starting state of S01 does
// not hold: every member's Pod Ready, one active master and standby slaves,
// the conditions of S01 True with the master as primary, the Operator and
// every Broker available, the workload client running, and the data rules
// holding on every member and through both Services, which also shows that
// both endpoints answer.
func (r *haRun) verifyStartingState() error {
	for _, member := range r.members {
		// A Pod that is being deleted is still Ready for a while.
		ready, err := r.kubectl("get", "pod", member,
			"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}{.metadata.deletionTimestamp}`)
		if err != nil {
			return err
		}
		if ready != "True" {
			return fmt.Errorf("Pod %s is not Ready, or is being deleted: %q", member, ready)
		}
	}
	deleting, err := r.kubectl("get", "pods",
		"-l", "database.cubrid.io/broker-access-mode,app.kubernetes.io/instance="+r.cluster,
		"-o", `jsonpath={range .items[?(@.metadata.deletionTimestamp)]}{.metadata.name}{" "}{end}`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(deleting) != "" {
		return fmt.Errorf("Broker Pods are being deleted: %s", deleting)
	}
	master, _, err := r.master()
	if err != nil {
		return err
	}
	if _, err := r.formationTime(); err != nil {
		return err
	}
	s, err := r.status()
	if err != nil {
		return err
	}
	if s.primary != master {
		return fmt.Errorf("status.currentPrimary is %q, CUBRID reports %s as master", s.primary, master)
	}
	if !operatorAvailable() {
		return fmt.Errorf("the Operator is not available")
	}
	for _, mode := range []string{"rw", "ro"} {
		out, err := r.kubectl("get", "deployment", r.cluster+"-broker-"+mode,
			"-o", "jsonpath={.spec.replicas} {.status.updatedReplicas} {.status.availableReplicas}")
		if err != nil {
			return err
		}
		f := strings.Fields(out)
		if len(f) != 3 || f[0] == "0" || f[1] != f[0] || f[2] != f[0] {
			return fmt.Errorf("the %s Brokers are not all available (replicas, updated, available: %s)", mode, out)
		}
	}
	// A client Pod that is being deleted still answers for a while.
	client, err := utils.Run(exec.Command("kubectl", "-n", r.clientNamespace, "get", "pod", "workload-client",
		"-o", "jsonpath={.status.phase}{.metadata.deletionTimestamp}"))
	if err != nil {
		return err
	}
	if client != "Running" {
		return fmt.Errorf("the workload client Pod is not running, or is being deleted: %q", client)
	}
	// A marker written through the read-write Service must reach every
	// member: a slave whose applier stalled is not in the starting state.
	r.mu.Lock()
	r.markers++
	marker := fmt.Sprintf("gate%d", r.markers)
	r.mu.Unlock()
	h, err := r.runWorkload("CLIENT_ID=" + marker + " OPS=1 ROLLBACK_EVERY=0")
	if err != nil {
		return fmt.Errorf("marker write through -rw: %w", err)
	}
	if h.Counts[workload.Acknowledged] != 1 {
		return fmt.Errorf("the marker write through -rw was not acknowledged: %v", h.Counts)
	}
	var report workload.Report
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(3 * time.Second) {
		if report, err = r.dataCheck(); err == nil && report.OK {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("the data rules do not hold, or marker %s did not reach every member within a minute: %+v",
		marker, report)
}

// needsFirstMemberAsMaster is the order dependency of S03
// abrupt-first-member: the steps before it move no role.
func (r *haRun) needsFirstMemberAsMaster(context.Context) error {
	master, _, err := r.master()
	if err != nil {
		return err
	}
	if first := r.members[0]; master != first {
		return fmt.Errorf("this variant needs the first member %s as master, and the master is %s", first, master)
	}
	return nil
}

// stepName is the requirement label of the running step, or its text.
func stepName(report SpecReport) string {
	for _, req := range kindLane.Required {
		if slices.Contains(report.Labels(), req.Label()) {
			return req.Label()
		}
	}
	return report.LeafNodeText
}

// blockStep records the scenario of the running step as blocked with the
// reason, and skips the step.
func blockStep(reason string) {
	labels := CurrentSpecReport().Labels()
	for _, req := range kindLane.Required {
		if slices.Contains(labels, req.Label()) {
			recordScenario(evidence.Scenario{ID: req.ID, Variant: req.Variant, Result: evidence.Blocked, Reason: reason})
		}
	}
	Skip("blocked: " + reason)
}

// enter starts an HA step from the verified starting state, or blocks it.
// needs is the step's own precondition, or nil. Each decision is kept in
// ha-bootstrap/starting-state.jsonl.
func (r *haRun) enter(needs func(context.Context) error) {
	name := stepName(CurrentSpecReport())
	if e := r.admit(name, needs); e.Reason != "" {
		blockStep(e.Reason)
	}
	if os.Getenv(breakStepEnv) == name {
		r.breakOnPurpose(name)
	}
}

// admit restores and verifies the common starting state before the step or
// the part of a step that name stands for, keeps the decision in
// ha-bootstrap/starting-state.jsonl, and returns it.
func (r *haRun) admit(name string, needs func(context.Context) error) faults.Entry {
	By("restoring and verifying the common starting state")
	e := r.gate.Enter(context.Background(), name, needs)
	r.entries = append(r.entries, e)
	lines := make([]string, 0, len(r.entries))
	for _, entry := range r.entries {
		if line, err := json.Marshal(entry); err == nil {
			lines = append(lines, string(line))
		}
	}
	writeScenarioFile("ha-bootstrap/starting-state.jsonl", []byte(strings.Join(lines, "\n")+"\n"))
	_, _ = fmt.Fprintf(GinkgoWriter, "starting state for %s: verified=%t after %s %s\n",
		name, e.Verified, e.Waited, e.Reason)
	return e
}

// breakOnPurpose damages the cluster as a failed variant could, and fails the
// step (breakStepEnv).
func (r *haRun) breakOnPurpose(name string) {
	By("breaking the step on purpose: " + breakStepEnv + "=" + name)
	r.startClient("broken")
	brokers, err := r.rwBrokerPods()
	Expect(err).NotTo(HaveOccurred())
	Expect(brokers).NotTo(BeEmpty())
	_, err = r.kubectl("delete", "pod", brokers[0].name, "--wait=false", "--grace-period=0", "--force")
	Expect(err).NotTo(HaveOccurred())
	_, err = utils.Run(exec.Command("kubectl", "-n", namespace, "scale", operatorDeployment, "--replicas=0"))
	Expect(err).NotTo(HaveOccurred())
	if os.Getenv(breakUnrestorableEnv) == "true" {
		_, err = utils.Run(exec.Command("kubectl", "-n", r.clientNamespace, "delete", "pod", "workload-client",
			"--wait=false"))
		Expect(err).NotTo(HaveOccurred())
	}
	Fail(fmt.Sprintf("failed on purpose (%s=%s), leaving a client writing, a Broker Pod deleted"+
		" and the Operator scaled to zero", breakStepEnv, name))
}
