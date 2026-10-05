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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The variants of S03 in docs/testing/scenario-contract.md.
const (
	s03Graceful = "graceful"
	s03Abrupt   = "abrupt"
)

var s03Variants = []string{s03Graceful, s03Abrupt}

// The limits of S03 for Kind on a GitHub-hosted runner, recorded in
// docs/testing/scenario-contract.md, section "Time limits". failoverLimit is
// zero until its baseline exists: S03 is then reported as blocked with
// reason time_limit_unset.
const (
	failoverLimit time.Duration = 0
	// stablePeriod is how long the client must go on being acknowledged
	// before its recovery counts.
	stablePeriod = 30 * time.Second
)

// s03FlowLimits bound the steps of the fault flow. They are generous waits
// for the test to end, not the limits the scenario is judged by.
var s03FlowLimits = faults.Limits{
	Confirm: 5 * time.Minute,
	Outcome: 10 * time.Minute,
	Cleanup: time.Minute,
	Poll:    2 * time.Second,
}

// activeMasterAmong returns the first of the given members that CUBRID
// reports as an active master, or "".
func (r *haRun) activeMasterAmong(pods []string) string {
	for _, pod := range pods {
		out, err := r.kubectl("exec", pod, "--", "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
		if err == nil && strings.Contains(out, "current "+pod+", state master") &&
			strings.Contains(out, "registered_and_active") {
			return pod
		}
	}
	return ""
}

// s03Steps registers S03 (primary Pod termination) of the scenario contract,
// once per variant. Each run continues on the cluster the earlier steps left,
// with the workload client writing through the read-write Service while the
// master's Pod is deleted.
func s03Steps(r *haRun) {
	for _, variant := range s03Variants {
		It("S03/"+variant+": a slave takes over when the master Pod is deleted, and no acknowledged write is lost",
			func() { r.s03(variant) })
	}
}

func (r *haRun) s03(variant string) {
	result := evidence.Scenario{ID: "S03", Variant: variant, Result: evidence.Fail,
		Reason: "a check of S03 failed; see the test output"}
	// Reported also when a check below fails.
	defer func() { r.later = append(r.later, result) }()

	client := "s03" + variant[:1]
	file := "/tmp/" + client

	By("recording the starting state")
	master, slaves, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	uid, err := r.kubectl("get", "pod", master, "-o", "jsonpath={.metadata.uid}")
	Expect(err).NotTo(HaveOccurred())

	By("starting a client that keeps writing through the read-write Service")
	_, err = r.client(fmt.Sprintf("CLIENT_ID=%s OPS=100000 INTERVAL_MS=100 ROLLBACK_EVERY=7"+
		" nohup workload run > %[2]s.jsonl 2> %[2]s.err & echo $! > %[2]s.pid", client, file))
	Expect(err).NotTo(HaveOccurred())
	stopClient := func() { _, _ = r.client(fmt.Sprintf("kill $(cat %s.pid) 2>/dev/null; true", file)) }
	defer stopClient()
	Eventually(func(g Gomega) {
		out, err := r.client(fmt.Sprintf("grep -c '\"acknowledged\"' %s.jsonl", file))
		g.Expect(err).NotTo(HaveOccurred())
		acknowledged, err := strconv.Atoi(strings.TrimSpace(out))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(acknowledged).To(BeNumerically(">=", 10))
	}, time.Minute, time.Second).Should(Succeed(), "the client did not get ten operations acknowledged")

	By("deleting the master Pod and waiting for a new master, the operator's status and the client")
	var newMaster string
	var electedAt, noticedAt time.Time
	outcome := faults.Run(context.Background(), faults.Scenario{
		Fault:  podDeletion{namespace: r.namespace, pod: master, uid: uid, force: variant == s03Abrupt},
		Limits: s03FlowLimits,
		Before: func(context.Context) error {
			now, _, err := r.master()
			if err == nil && now != master {
				err = fmt.Errorf("the master is %s, not %s", now, master)
			}
			return err
		},
		Check: func(context.Context) (bool, error) {
			// CUBRID's election: another member reports itself an active master.
			if newMaster == "" {
				if newMaster = r.activeMasterAmong(slaves); newMaster == "" {
					return false, nil
				}
				electedAt = time.Now()
			}
			// The Operator noticing it.
			if noticedAt.IsZero() {
				primary, err := r.kubectl("get", "cubridcluster", r.cluster, "-o", "jsonpath={.status.currentPrimary}")
				if err != nil || primary != newMaster {
					return false, nil
				}
				noticedAt = time.Now()
			}
			// One master and two slaves again, the former master among the
			// slaves. A master other than the elected one is a second
			// change of role, which must not happen by itself.
			now, _, err := r.master()
			if err != nil {
				return false, nil
			}
			if now != newMaster {
				return false, fmt.Errorf("the master changed again, from %s to %s", newMaster, now)
			}
			if _, err := r.formationTime(); err != nil {
				return false, nil // a condition of the starting state is not True yet
			}
			// The client recovered: its latest answer is an acknowledgement.
			last, err := r.client(fmt.Sprintf("grep -v '\"attempted\"' %s.jsonl | tail -n 1", file))
			return err == nil && strings.Contains(last, `"event":"acknowledged"`), nil
		},
	})
	timeline := writeTimeline("S03/"+variant+"/timeline.jsonl", outcome.Timeline)
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	if outcome.Result != evidence.Pass {
		result.Result, result.Reason = outcome.Result, outcome.Reason
	}
	Expect(outcome.Result).To(Equal(evidence.Pass), "S03/%s: %s", variant, outcome.Reason)

	By("letting the client run for the stable period, then stopping it")
	time.Sleep(stablePeriod + 2*time.Second)
	stopClient()
	text, err := r.client(fmt.Sprintf("sleep 1; cat %s.jsonl", file))
	Expect(err).NotTo(HaveOccurred())
	// A client that is stopped while it writes a line leaves that line unfinished.
	if cut := strings.LastIndex(text, "\n"); cut >= 0 {
		text = text[:cut+1]
	}
	events, err := workload.ReadEvents(strings.NewReader(text))
	Expect(err).NotTo(HaveOccurred())
	history, err := workload.ReadHistory(strings.NewReader(text))
	Expect(err).NotTo(HaveOccurred())
	r.history += text
	if errors, err := r.client(fmt.Sprintf("tail -n 5 %s.err", file)); err == nil && strings.TrimSpace(errors) != "" {
		_, _ = fmt.Fprintf(GinkgoWriter, "client %s stderr:\n%s\n", client, errors)
	}

	recoveredAt, stable, recovered := workload.Recovery(events, outcome.FaultIssuedAt)
	Expect(recovered).To(BeTrue(), "the client's last answer after the fault is not an acknowledgement")
	Expect(stable).To(BeNumerically(">=", stablePeriod),
		"the client was acknowledged without interruption for %s only", stable)
	recovery := recoveredAt.Sub(outcome.FaultIssuedAt)

	By("checking that the former master did not become master again")
	now, nowSlaves, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	Expect(now).To(Equal(newMaster), "the master after the stable period")
	Expect(nowSlaves).To(ContainElement(master), "the former master is a slave")
	primary, err := r.kubectl("get", "cubridcluster", r.cluster, "-o", "jsonpath={.status.currentPrimary}")
	Expect(err).NotTo(HaveOccurred())
	Expect(primary).To(Equal(newMaster))

	By("applying the data rules to every member and to both Services")
	var report workload.Report
	Eventually(func(g Gomega) {
		var err error
		report, err = r.dataCheck()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
	}, 3*time.Minute, 3*time.Second).Should(Succeed())
	for _, slave := range nowSlaves {
		count, err := r.failCount(slave, newMaster)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeZero(), "fail count of the applier on %s", slave)
	}

	files := r.evidenceFiles("S03/"+variant, report)
	if timeline != "" {
		files = append(files, timeline)
	}
	since := func(t time.Time) string { return t.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String() }
	judgedResult := judged(evidence.Scenario{
		ID: "S03", Variant: variant,
		FaultConfirmed: &outcome.FaultConfirmed, CleanupSucceeded: &outcome.CleanupSucceeded,
		Measurements: map[string]any{
			"recoveryTime":           recovery.Round(time.Millisecond).String(),
			"stableFor":              stable.Round(time.Millisecond).String(),
			"electionObservedWithin": since(electedAt),
			"operatorNoticedWithin":  since(noticedAt),
			"allMembersBackWithin":   since(outcome.OutcomeAt),
			"acknowledgedMissing":    report.AcknowledgedMissing,
			"outcomeUnknown":         history.Counts[workload.Unknown],
		},
		Operations: &evidence.Operations{
			Attempted: history.Counts[workload.Attempted], Acknowledged: history.Counts[workload.Acknowledged],
			Failed: history.Counts[workload.Failed], Unknown: history.Counts[workload.Unknown],
		},
		Evidence: files,
	}, "failover_limit", failoverLimit, recovery)
	judgedResult.Limits["stable_period"] = stablePeriod.String()
	result = judgedResult
}
