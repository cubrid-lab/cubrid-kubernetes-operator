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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The variants of S03 in docs/testing/scenario-contract.md. The two abrupt
// ones differ in which member is the master when its Pod is deleted: CUBRID
// was observed to elect the first member of the node list again when that
// member is back before another one has taken over (docs/poc/RESULTS.md,
// POC-19).
const (
	s03AbruptFirst = "abrupt-first-member"
	s03Graceful    = "graceful"
	s03Abrupt      = "abrupt"
)

// s03Variants is the order in which the variants run. The first runs while
// the first member is the master; the last after the graceful one moved the
// master away from it.
var s03Variants = []string{s03AbruptFirst, s03Graceful, s03Abrupt}

// s03Clients names the workload client of each variant.
var s03Clients = map[string]string{s03AbruptFirst: "s03f", s03Graceful: "s03g", s03Abrupt: "s03a"}

// The limits of S03 for Kind on a GitHub-hosted runner. The values and the
// baseline they come from are recorded in docs/testing/scenario-contract.md,
// section "Time limits"; change them there and here together. A limit of
// zero is "not set": S03 is then reported as blocked with reason
// time_limit_unset.
const (
	// failoverLimit bounds the time from the fault until the client's
	// writes are acknowledged again without interruption.
	failoverLimit = 30 * time.Second
	// rejoinLimit bounds the time from the fault until all three members
	// have a role again, the former master as a slave.
	rejoinLimit = 5 * time.Minute
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

// statusSnapshot is the conditions and the primary of the CubridCluster as
// one read returned them.
type statusSnapshot struct {
	primary    string
	conditions map[string]string
}

func (r *haRun) status() (statusSnapshot, error) {
	out, err := r.kubectl("get", "cubridcluster", r.cluster, "-o", "json")
	if err != nil {
		return statusSnapshot{}, err
	}
	var cluster struct {
		Status struct {
			CurrentPrimary string `json:"currentPrimary"`
			Conditions     []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &cluster); err != nil {
		return statusSnapshot{}, err
	}
	s := statusSnapshot{primary: cluster.Status.CurrentPrimary, conditions: map[string]string{}}
	for _, c := range cluster.Status.Conditions {
		s.conditions[c.Type] = c.Status
	}
	return s, nil
}

// window is a span of time seen by sampling: from the first sample in which
// something held to the first later sample in which it no longer did.
type window struct {
	from, to time.Time
}

func (w *window) sample(holds bool) {
	switch {
	case holds && w.from.IsZero():
		w.from = time.Now()
	case !holds && !w.from.IsZero() && w.to.IsZero():
		w.to = time.Now()
	}
}

// length is the window's duration, or "not observed" when no sample fell in
// it and "not ended" when it was still open at the last sample.
func (w window) length() string {
	switch {
	case w.from.IsZero():
		return "not observed"
	case w.to.IsZero():
		return "not ended"
	}
	return w.to.Sub(w.from).Round(time.Millisecond).String()
}

// startClient starts a workload client that keeps writing through the
// read-write Service until it is stopped, and waits until ten of its
// operations were acknowledged.
func (r *haRun) startClient(id string) {
	file := "/tmp/" + id
	_, err := r.client(fmt.Sprintf("CLIENT_ID=%s OPS=100000 INTERVAL_MS=100 ROLLBACK_EVERY=7"+
		" nohup workload run > %[2]s.jsonl 2> %[2]s.err & echo $! > %[2]s.pid", id, file))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		out, err := r.client(fmt.Sprintf("grep -c '\"acknowledged\"' %s.jsonl", file))
		g.Expect(err).NotTo(HaveOccurred())
		acknowledged, err := strconv.Atoi(strings.TrimSpace(out))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(acknowledged).To(BeNumerically(">=", 10))
	}, time.Minute, time.Second).Should(Succeed(), "client %s did not get ten operations acknowledged", id)
}

// killClient stops a client started with startClient. It is safe to call
// more than once.
func (r *haRun) killClient(id string) {
	_, _ = r.client(fmt.Sprintf("kill $(cat /tmp/%s.pid) 2>/dev/null; true", id))
}

// lastAnswerAcknowledged reports whether the latest answer a running client
// received is an acknowledgement.
func (r *haRun) lastAnswerAcknowledged(id string) bool {
	last, err := r.client(fmt.Sprintf("grep -v '\"attempted\"' /tmp/%s.jsonl | tail -n 1", id))
	return err == nil && strings.Contains(last, `"event":"acknowledged"`)
}

// stopClient stops a client, adds its history to the run's and returns its
// events and its operations by outcome.
func (r *haRun) stopClient(id string) ([]workload.Event, workload.History) {
	r.killClient(id)
	text, err := r.client(fmt.Sprintf("sleep 1; cat /tmp/%s.jsonl", id))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	// A client that is stopped while it writes a line leaves that line unfinished.
	if cut := strings.LastIndex(text, "\n"); cut >= 0 {
		text = text[:cut+1]
	}
	events, err := workload.ReadEvents(strings.NewReader(text))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	history, err := workload.ReadHistory(strings.NewReader(text))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	r.history += text
	if errors, err := r.client(fmt.Sprintf("tail -n 5 /tmp/%s.err", id)); err == nil && strings.TrimSpace(errors) != "" {
		_, _ = fmt.Fprintf(GinkgoWriter, "client %s stderr:\n%s\n", id, errors)
	}
	return events, history
}

// roleLog records what CUBRID reports on every member, each time it changes.
// It is evidence for a run that does not end as expected.
type roleLog struct {
	lines []string
	last  string
}

// sample asks every member for its node and server state.
func (l *roleLog) sample(r *haRun) {
	states := make([]string, 0, len(r.members))
	for _, pod := range r.members {
		out, err := r.kubectl("exec", pod, "--", "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
		state := "unreachable"
		if err == nil {
			state = "no state"
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "HA-Node Info") || strings.HasPrefix(line, "Server ") {
					state = strings.TrimPrefix(state+" | "+line, "no state | ")
				}
			}
		}
		states = append(states, pod+": "+state)
	}
	now := strings.Join(states, "; ")
	if now == l.last {
		return
	}
	l.last = now
	line, _ := json.Marshal(map[string]string{"t": time.Now().UTC().Format(time.RFC3339Nano), "roles": now})
	l.lines = append(l.lines, string(line))
}

func (l *roleLog) write(rel string) string {
	return writeScenarioFile(rel, []byte(strings.Join(l.lines, "\n")+"\n"))
}

// s03Steps registers S03 (primary Pod termination) of the scenario contract,
// once per variant. Each run continues on the cluster the earlier steps left,
// with the workload client writing through the read-write Service while the
// master's Pod is deleted.
func s03Steps(r *haRun) {
	for _, variant := range s03Variants {
		s03Step(r, variant)
	}
}

// s03Step registers one variant of S03.
func s03Step(r *haRun, variant string) {
	It("S03/"+variant+": one member is master again after the master Pod is deleted, and no acknowledged write is lost",
		func() { r.s03(variant) })
}

func (r *haRun) s03(variant string) {
	result := evidence.Scenario{ID: "S03", Variant: variant, Result: evidence.Fail,
		Reason: "a check of S03 failed; see the test output"}
	// Reported also when a check below fails.
	defer func() { r.later = append(r.later, result) }()

	client := s03Clients[variant]

	By("recording the starting state")
	master, _, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	uid, err := r.kubectl("get", "pod", master, "-o", "jsonpath={.metadata.uid}")
	Expect(err).NotTo(HaveOccurred())

	By("starting a client that keeps writing through the read-write Service")
	r.startClient(client)
	defer r.killClient(client)

	By("deleting the master Pod and waiting for a new master, the operator's status and the client")
	var newMaster string
	var electedAt, noticedAt time.Time
	// How long the operator reported the primary as not resolved and the
	// routing as not ready, as far as the samples of the wait show.
	var unresolved, notRouting window
	roles := &roleLog{}
	outcome := faults.Run(context.Background(), faults.Scenario{
		Fault:  podDeletion{namespace: r.namespace, pod: master, uid: uid, force: variant != s03Graceful},
		Limits: s03FlowLimits,
		Before: func(context.Context) error {
			now, _, err := r.master()
			if err != nil {
				return err
			}
			if now != master {
				return fmt.Errorf("the master is %s, not %s", now, master)
			}
			// Each abrupt variant is about one position of the master.
			first := r.members[0]
			switch {
			case variant == s03AbruptFirst && master != first:
				return fmt.Errorf("this variant needs the first member %s as master, and the master is %s", first, master)
			case variant == s03Abrupt && master == first:
				return fmt.Errorf("this variant needs a master other than the first member %s", first)
			}
			return nil
		},
		Check: func(context.Context) (bool, error) {
			roles.sample(r)
			// The operator's own view, from one read of its status. It must
			// never offer the write endpoint while the primary is not
			// resolved (ADR-0005, section 6).
			if s, err := r.status(); err == nil {
				resolved := s.conditions["PrimaryResolved"] == "True"
				routing := s.conditions["RoutingReady"] == "True"
				if routing && !resolved {
					return false, fmt.Errorf("RoutingReady is True while PrimaryResolved is %q",
						s.conditions["PrimaryResolved"])
				}
				unresolved.sample(!resolved)
				notRouting.sample(!routing)
			}
			// CUBRID's election: a member reports itself an active master.
			// It is another member, or the deleted one in its new Pod: the
			// fault is confirmed, so the old Pod no longer answers.
			if newMaster == "" {
				if newMaster = r.activeMasterAmong(r.members); newMaster == "" {
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
			// One master and two slaves again. A master other than the
			// elected one is a second change of role, which must not happen
			// by itself.
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
			return r.lastAnswerAcknowledged(client), nil
		},
	})
	timeline := writeTimeline("S03/"+variant+"/timeline.jsonl", outcome.Timeline)
	rolesFile := roles.write("S03/" + variant + "/roles.jsonl")
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	if outcome.Result != evidence.Pass {
		result.Result, result.Reason = outcome.Result, outcome.Reason
		// Keep what the client saw: the scenario ends here.
		if text, err := r.client(fmt.Sprintf("cat /tmp/%s.jsonl", client)); err == nil {
			writeScenarioFile("S03/"+variant+"/client-history.jsonl", []byte(text))
		}
	}
	Expect(outcome.Result).To(Equal(evidence.Pass), "S03/%s: %s", variant, outcome.Reason)

	By("letting the client run for the stable period, then stopping it")
	time.Sleep(stablePeriod + 2*time.Second)
	events, history := r.stopClient(client)

	recoveredAt, stable, recovered := workload.Recovery(events, outcome.FaultIssuedAt)
	Expect(recovered).To(BeTrue(), "the client's last answer after the fault is not an acknowledgement")
	Expect(stable).To(BeNumerically(">=", stablePeriod),
		"the client was acknowledged without interruption for %s only", stable)
	recovery := recoveredAt.Sub(outcome.FaultIssuedAt)

	By("checking that the master did not change a second time")
	now, nowSlaves, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	Expect(now).To(Equal(newMaster), "the master after the stable period")
	if newMaster != master {
		Expect(nowSlaves).To(ContainElement(master), "the former master is a slave")
	}
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
	for _, f := range []string{timeline, rolesFile} {
		if f != "" {
			files = append(files, f)
		}
	}
	since := func(t time.Time) string { return t.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String() }
	// Writes the client was told are committed while the operator reported
	// the primary as not resolved: open question Q2 of the contract.
	acknowledgedWhileUnresolved := 0
	for _, e := range events {
		if e.Event == workload.Acknowledged && !unresolved.from.IsZero() && e.T.After(unresolved.from) &&
			(unresolved.to.IsZero() || e.T.Before(unresolved.to)) {
			acknowledgedWhileUnresolved++
		}
	}
	judgedResult := judged(evidence.Scenario{
		ID: "S03", Variant: variant,
		FaultConfirmed: &outcome.FaultConfirmed, CleanupSucceeded: &outcome.CleanupSucceeded,
		Measurements: map[string]any{
			"recoveryTime":           recovery.Round(time.Millisecond).String(),
			"masterBefore":           master,
			"masterAfter":            newMaster,
			"sameMemberMasterAgain":  newMaster == master,
			"stableFor":              stable.Round(time.Millisecond).String(),
			"electionObservedWithin": since(electedAt),
			"operatorNoticedWithin":  since(noticedAt),
			"allMembersBackWithin":   since(outcome.OutcomeAt),
			"acknowledgedMissing":    report.AcknowledgedMissing,
			"outcomeUnknown":         history.Counts[workload.Unknown],
			// From samples taken every few seconds during the wait.
			"primaryUnresolvedFor":               unresolved.length(),
			"routingNotReadyFor":                 notRouting.length(),
			"acknowledgedWhilePrimaryUnresolved": acknowledgedWhileUnresolved,
		},
		Operations: &evidence.Operations{
			Attempted: history.Counts[workload.Attempted], Acknowledged: history.Counts[workload.Acknowledged],
			Failed: history.Counts[workload.Failed], Unknown: history.Counts[workload.Unknown],
		},
		Evidence: files,
	}, "failover_limit", failoverLimit, recovery)
	judgedResult.Limits["stable_period"] = stablePeriod.String()
	rejoin := outcome.OutcomeAt.Sub(outcome.FaultIssuedAt)
	switch {
	case rejoinLimit <= 0:
		judgedResult.Limits["rejoin_limit"] = "unset"
		judgedResult.Result, judgedResult.Reason = evidence.Blocked, faults.ReasonTimeLimitUnset
	case judgedResult.Result == evidence.Pass && rejoin > rejoinLimit:
		judgedResult.Limits["rejoin_limit"] = rejoinLimit.String()
		judgedResult.Result = evidence.Fail
		judgedResult.Reason = fmt.Sprintf("rejoin_limit exceeded: %s > %s", rejoin, rejoinLimit)
	default:
		judgedResult.Limits["rejoin_limit"] = rejoinLimit.String()
	}
	result = judgedResult
}
