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
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The variants of S05 in docs/testing/scenario-contract.md.
const (
	s05BrokerProcess = "broker-process"
	s05OneBrokerPod  = "one-broker-pod"
	s05AllRWBrokers  = "all-rw-brokers"
)

var s05Variants = []string{s05BrokerProcess, s05OneBrokerPod, s05AllRWBrokers}

// s05Clients is how many clients write during the fault, so that sessions
// are open on more than one Broker Pod.
const s05Clients = 4

// brokerPod is one read-write Broker Pod.
type brokerPod struct {
	name, uid string
	restarts  int
}

// rwBrokerPods lists the read-write Broker Pods that are not being deleted.
func (r *haRun) rwBrokerPods() ([]brokerPod, error) {
	out, err := r.kubectl("get", "pods",
		"-l", "database.cubrid.io/broker-access-mode=rw,app.kubernetes.io/instance="+r.cluster,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.uid}{" "}`+
			`{.status.containerStatuses[0].restartCount}{" "}{.metadata.deletionTimestamp}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	var pods []brokerPod
	for _, line := range utils.GetNonEmptyLines(out) {
		fields := strings.Fields(line)
		if len(fields) != 3 { // a fourth field is a deletion timestamp
			continue
		}
		restarts, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("restart count of %s: %w", fields[0], err)
		}
		pods = append(pods, brokerPod{name: fields[0], uid: fields[1], restarts: restarts})
	}
	return pods, nil
}

// brokerPodsDeletion is the fault "Broker Pods are deleted without a grace
// period". It is confirmed when none of them exists any more.
type brokerPodsDeletion struct {
	run  *haRun
	pods []brokerPod
}

func (f brokerPodsDeletion) Name() string {
	names := make([]string, 0, len(f.pods))
	for _, p := range f.pods {
		names = append(names, p.name)
	}
	return "delete Broker Pods without a grace period: " + strings.Join(names, ", ")
}

func (f brokerPodsDeletion) Inject(ctx context.Context) error {
	args := []string{"-n", f.run.namespace, "delete", "pod", "--wait=false", "--grace-period=0", "--force"}
	for _, p := range f.pods {
		args = append(args, p.name)
	}
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", args...))
	return err
}

func (f brokerPodsDeletion) Confirm(context.Context) (bool, error) {
	now, err := f.run.rwBrokerPods()
	if err != nil {
		return false, err
	}
	for _, old := range f.pods {
		for _, p := range now {
			if p.uid == old.uid {
				return false, nil
			}
		}
	}
	return true, nil
}

func (f brokerPodsDeletion) Remove(context.Context) error { return nil }

// brokerProcessKill is the fault "the Broker process of one Pod is killed".
// It is confirmed when the Pod's container was restarted.
type brokerProcessKill struct {
	run *haRun
	pod brokerPod
}

func (f brokerProcessKill) Name() string {
	return "kill -9 the cub_broker process in Pod " + f.pod.name
}

func (f brokerProcessKill) Inject(ctx context.Context) error {
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", f.run.namespace, "exec", f.pod.name, "--",
		"bash", "-c", `for d in /proc/[0-9]*; do`+
			` if [ "$(cat "$d/comm" 2>/dev/null)" = cub_broker ]; then kill -9 "${d#/proc/}"; fi; done`))
	return err
}

func (f brokerProcessKill) Confirm(context.Context) (bool, error) {
	now, err := f.run.rwBrokerPods()
	if err != nil {
		return false, err
	}
	for _, p := range now {
		if p.uid == f.pod.uid {
			return p.restarts > f.pod.restarts, nil
		}
	}
	return false, fmt.Errorf("Broker Pod %s is gone", f.pod.name)
}

func (f brokerProcessKill) Remove(context.Context) error { return nil }

// s05Steps registers S05 (Broker failure) of the scenario contract, once per
// variant, on the cluster the earlier steps left.
func s05Steps(r *haRun) {
	for _, variant := range s05Variants {
		It("S05/"+variant+": clients recover from a Broker failure, and it causes no database failover",
			Label("S05", evidence.Requirement{ID: "S05", Variant: variant}.Label()), func() { r.s05(variant) })
	}
}

func (r *haRun) s05(variant string) {
	result := evidence.Scenario{ID: "S05", Variant: variant, Result: evidence.Fail,
		Reason: "a check of S05 failed; see the test output"}
	// Reported also when a check below fails.
	defer func() { recordScenario(result) }()

	By("recording the starting state")
	master, _, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	brokers, err := r.rwBrokerPods()
	Expect(err).NotTo(HaveOccurred())
	Expect(brokers).To(HaveLen(2), "read-write Broker Pods")

	var fault faults.Fault
	switch variant {
	case s05BrokerProcess:
		fault = brokerProcessKill{run: r, pod: brokers[0]}
	case s05OneBrokerPod:
		fault = brokerPodsDeletion{run: r, pods: brokers[:1]}
	default:
		fault = brokerPodsDeletion{run: r, pods: brokers}
	}

	By("starting clients that keep writing through the read-write Service")
	prefix := map[string]string{s05BrokerProcess: "s05p", s05OneBrokerPod: "s05o", s05AllRWBrokers: "s05a"}[variant]
	clients := make([]string, 0, s05Clients)
	for i := 1; i <= s05Clients; i++ {
		id := fmt.Sprintf("%s%d", prefix, i)
		clients = append(clients, id)
		r.startClient(id)
		defer r.killClient(id)
	}

	By("failing the Broker and waiting for two available Brokers and for every client")
	newConnection := "not tried"
	outcome := faults.Run(context.Background(), faults.Scenario{
		Fault:  fault,
		Limits: s03FlowLimits,
		Before: func(context.Context) error {
			now, _, err := r.master()
			if err == nil && now != master {
				err = fmt.Errorf("the master is %s, not %s", now, master)
			}
			return err
		},
		Check: func(context.Context) (bool, error) {
			// New connections right after the fault. With one Broker Pod
			// deleted they have to succeed through the other one; up to
			// three are tried, because the Service may still hold the
			// deleted Pod's address for a moment. In the other variants a
			// failure is what a client can expect, and is only recorded.
			if newConnection == "not tried" {
				attempts := 1
				if variant == s05OneBrokerPod {
					attempts = 3
				}
				newConnection = "failed"
				var lastErr error
				for attempt := 1; attempt <= attempts; attempt++ {
					if _, lastErr = r.client("workload dump > /dev/null"); lastErr == nil {
						newConnection = fmt.Sprintf("succeeded on attempt %d", attempt)
						break
					}
				}
				if lastErr != nil && variant == s05OneBrokerPod {
					return false, fmt.Errorf("three new connections failed while a read-write Broker Pod was left: %w", lastErr)
				}
			}
			// A Broker failure must not move the master.
			now, _, err := r.master()
			if err != nil {
				return false, nil
			}
			if now != master {
				return false, fmt.Errorf("the master changed from %s to %s after a Broker failure", master, now)
			}
			available, err := r.kubectl("get", "deployment", r.cluster+"-broker-rw",
				"-o", "jsonpath={.status.availableReplicas}")
			if err != nil || available != "2" {
				return false, nil
			}
			if _, err := r.formationTime(); err != nil {
				return false, nil // a condition of the starting state is not True yet
			}
			for _, id := range clients {
				if !r.lastAnswerAcknowledged(id) {
					return false, nil
				}
			}
			return true, nil
		},
	})
	timeline := writeTimeline("S05/"+variant+"/timeline.jsonl", outcome.Timeline)
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	if outcome.Result != evidence.Pass {
		result.Result, result.Reason = outcome.Result, outcome.Reason
	}
	Expect(outcome.Result).To(Equal(evidence.Pass), "S05/%s: %s", variant, outcome.Reason)

	By("letting the clients run for the stable period, then stopping them")
	time.Sleep(stablePeriod + 2*time.Second)
	var recovery time.Duration
	operations := evidence.Operations{}
	interrupted := 0
	for _, id := range clients {
		events, history := r.stopClient(id)
		recoveredAt, stable, recovered := workload.Recovery(events, outcome.FaultIssuedAt)
		Expect(recovered).To(BeTrue(), "the last answer of client %s after the fault is not an acknowledgement", id)
		Expect(stable).To(BeNumerically(">=", stablePeriod),
			"client %s was acknowledged without interruption for %s only", id, stable)
		recovery = max(recovery, recoveredAt.Sub(outcome.FaultIssuedAt))
		// A client is interrupted when an operation of it failed, or was
		// left without a known outcome, after the fault.
		for _, e := range events {
			if e.T.After(outcome.FaultIssuedAt) && (e.Event == workload.Failed || e.Event == workload.Unknown) {
				interrupted++
				break
			}
		}
		operations.Attempted += history.Counts[workload.Attempted]
		operations.Acknowledged += history.Counts[workload.Acknowledged]
		operations.Failed += history.Counts[workload.Failed]
		operations.Unknown += history.Counts[workload.Unknown]
	}

	By("checking that the master is the same")
	now, slaves, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	Expect(now).To(Equal(master), "the master after a Broker failure")

	By("applying the data rules to every member and to both Services")
	var report workload.Report
	Eventually(func(g Gomega) {
		var err error
		report, err = r.dataCheck()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
	}, 3*time.Minute, 3*time.Second).Should(Succeed())
	for _, slave := range slaves {
		count, err := r.failCount(slave, master)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeZero(), "fail count of the applier on %s", slave)
	}

	files := r.evidenceFiles("S05/"+variant, report)
	if timeline != "" {
		files = append(files, timeline)
	}
	judgedResult := judged(evidence.Scenario{
		ID: "S05", Variant: variant,
		FaultConfirmed: &outcome.FaultConfirmed, CleanupSucceeded: &outcome.CleanupSucceeded,
		Measurements: map[string]any{
			// The slowest client: the fault until its writes were
			// acknowledged again without interruption.
			"recoveryTime":             recovery.Round(time.Millisecond).String(),
			"brokersAvailableWithin":   outcome.OutcomeAt.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String(),
			"newConnectionAfterFault":  newConnection,
			"clientsInterrupted":       interrupted,
			"clientsNotInterrupted":    len(clients) - interrupted,
			"acknowledgedMissing":      report.AcknowledgedMissing,
			"outcomeUnknown":           operations.Unknown,
			"databaseFailoverObserved": false,
		},
		Operations: &operations,
		Evidence:   files,
	}, "broker_recovery_limit", brokerRecoveryLimit, recovery)
	judgedResult.Limits["stable_period"] = stablePeriod.String()
	result = judgedResult
}
