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
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The variants of S06 in docs/testing/scenario-contract.md.
const (
	s06Restart = "restart"
	s06Absent  = "absent-during-failover"
)

var s06Variants = []string{s06Restart, s06Absent}

const (
	operatorDeployment = "deployment/cubrid-kubernetes-operator-controller-manager"
	operatorSelector   = "control-plane=controller-manager"
)

// statusStaleAfter is the reader's rule of docs/observability.md
// ("Freshness"): the HA status is stale when status.lastObservedTime is older.
const statusStaleAfter = 60 * time.Second

// operatorPods lists the Operator's Pods as "name uid" lines, also those
// that are being deleted.
func operatorPods(ctx context.Context) ([]string, error) {
	out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", "pods", "-l", operatorSelector,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.uid}{"\n"}{end}`))
	if err != nil {
		return nil, err
	}
	return utils.GetNonEmptyLines(out), nil
}

// operatorStartedAt returns when the container of the Operator Pod other
// than old started running, or the zero time while there is none.
func operatorStartedAt(ctx context.Context, old string) (time.Time, error) {
	out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", "pods", "-l", operatorSelector,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.uid}{"|"}`+
			`{.status.containerStatuses[0].state.running.startedAt}{"\n"}{end}`))
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range utils.GetNonEmptyLines(out) {
		pod, started, _ := strings.Cut(line, "|")
		if pod == old || started == "" {
			continue
		}
		return time.Parse(time.RFC3339, started)
	}
	return time.Time{}, nil
}

// operatorAvailable reports whether the Operator's Deployment has its Pod
// available.
func operatorAvailable(ctx context.Context) bool {
	out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", operatorDeployment,
		"-o", "jsonpath={.status.availableReplicas}"))
	return err == nil && out == "1"
}

// operatorPodKill is the fault "the Operator's Pod is deleted without a
// grace period". It is confirmed when the deleted Pod no longer exists; its
// Deployment starts another one.
type operatorPodKill struct {
	pod string // "name uid"
}

func (f operatorPodKill) Name() string { return "delete the Operator Pod without a grace period" }

func (f operatorPodKill) Inject(ctx context.Context) error {
	name, _, _ := strings.Cut(f.pod, " ")
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "delete", "pod", name,
		"--wait=false", "--grace-period=0", "--force"))
	return err
}

func (f operatorPodKill) Confirm(ctx context.Context) (bool, error) {
	pods, err := operatorPods(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range pods {
		if p == f.pod {
			return false, nil
		}
	}
	return true, nil
}

func (f operatorPodKill) Remove(context.Context) error { return nil }

// operatorAbsence is the fault "there is no Operator": its Deployment is
// scaled to zero. It is confirmed when no Operator Pod exists, and removed by
// scaling the Deployment back to one.
type operatorAbsence struct {
	// returnedAt is when the Deployment was scaled back.
	returnedAt time.Time
}

func (f *operatorAbsence) Name() string { return "scale the Operator's Deployment to zero" }

func (f *operatorAbsence) Inject(ctx context.Context) error {
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "scale", operatorDeployment, "--replicas=0"))
	return err
}

func (f *operatorAbsence) Confirm(ctx context.Context) (bool, error) {
	pods, err := operatorPods(ctx)
	return err == nil && len(pods) == 0, err
}

func (f *operatorAbsence) Remove(ctx context.Context) error {
	f.returnedAt = time.Now()
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "scale", operatorDeployment, "--replicas=1"))
	return err
}

// s06Step registers one variant of S06 (Operator failure) of the scenario
// contract, on the cluster the earlier steps left.
func s06Step(r *haRun, variant string) {
	labels := Label("S06", evidence.Requirement{ID: "S06", Variant: variant}.Label())
	switch variant {
	case s06Restart:
		It("S06/"+variant+": SQL is not interrupted and no member is restarted when the Operator Pod is killed",
			labels, func() {
				r.enter(nil)
				r.s06Restart()
			})
	case s06Absent:
		It("S06/"+variant+": CUBRID fails over without the Operator, which reports the new master when it returns",
			labels, func() {
				r.enter(nil)
				r.s06Absent()
			})
	}
}

func (r *haRun) s06Restart() {
	result := evidence.Scenario{ID: "S06", Variant: s06Restart, Result: evidence.Fail,
		Reason: "a check of S06 failed; see the test output"}
	defer func() { recordScenario(result) }()

	By("recording the starting state")
	master, _, err := r.master(context.Background())
	Expect(err).NotTo(HaveOccurred())
	membersBefore := r.identities(context.Background(), r.members)
	pods, err := operatorPods(context.Background())
	Expect(err).NotTo(HaveOccurred())
	Expect(pods).To(HaveLen(1), "Operator Pods")

	By("starting clients that keep writing through the read-write Service")
	clients := []string{"s06r1", "s06r2"}
	for _, id := range clients {
		r.startClient(id)
		defer r.killClient(id)
	}

	By("killing the Operator Pod and waiting for its replacement to write a fresh observation")
	// goneAt is when the outcome was first checked: after the old Pod was
	// confirmed gone.
	var goneAt time.Time
	outcome := r.runFault(faults.Scenario{
		Fault:  operatorPodKill{pod: pods[0]},
		Limits: s03FlowLimits,
		Before: func(ctx context.Context) error {
			if !operatorAvailable(ctx) {
				return fmt.Errorf("the Operator is not available")
			}
			_, err := r.formationTime(ctx)
			return err
		},
		// The status must come from the replacement: its observation time
		// is after the old Pod was gone and after the replacement's
		// container started, since the killed process can outlive its Pod
		// object for a moment. The API server stores the time to the
		// second, so "after" is strict.
		Check: func(ctx context.Context) (bool, error) {
			if goneAt.IsZero() {
				goneAt = time.Now()
			}
			now, _, err := r.master(ctx)
			if err != nil {
				return false, nil
			}
			if now != master {
				return false, fmt.Errorf("the master changed from %s to %s when the Operator was restarted", master, now)
			}
			if !operatorAvailable(ctx) {
				return false, nil
			}
			started, err := operatorStartedAt(ctx, pods[0])
			var parseErr *time.ParseError
			if errors.As(err, &parseErr) {
				return false, fmt.Errorf("the replacement Operator's start time: %w", err)
			}
			if err != nil || started.IsZero() {
				return false, nil
			}
			s, err := r.status(ctx)
			if err != nil || s.primary != master || !s.lastObserved.After(goneAt) || !s.lastObserved.After(started) {
				return false, nil
			}
			_, err = r.formationTime(ctx)
			return err == nil, nil
		},
	})
	timeline := writeTimeline("S06/"+s06Restart+"/timeline.jsonl", outcome.Timeline)
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	if outcome.Result != evidence.Pass {
		result.Result, result.Reason = outcome.Result, outcome.Reason
	}
	Expect(outcome.Result).To(Equal(evidence.Pass), "S06/%s: %s", s06Restart, outcome.Reason)

	By("letting the clients run for the stable period, then stopping them")
	time.Sleep(stablePeriod.Value + 2*time.Second)
	operations := evidence.Operations{}
	notAcknowledged := 0
	// The shortest time a client was acknowledged without interruption,
	// from its first acknowledgement after the fault.
	var minStable time.Duration
	measuredStable := false
	for _, id := range clients {
		events, history := r.stopClient(id)
		_, stable, recovered := workload.Recovery(events, outcome.FaultIssuedAt)
		Expect(recovered).To(BeTrue(), "the last answer of client %s after the fault is not an acknowledgement", id)
		if !measuredStable || stable < minStable {
			minStable, measuredStable = stable, true
		}
		for _, e := range events {
			if e.T.After(outcome.FaultIssuedAt) && (e.Event == workload.Failed || e.Event == workload.Unknown) {
				notAcknowledged++
			}
		}
		operations.Attempted += history.Counts[workload.Attempted]
		operations.Acknowledged += history.Counts[workload.Acknowledged]
		operations.Failed += history.Counts[workload.Failed]
		operations.Unknown += history.Counts[workload.Unknown]
	}
	Expect(notAcknowledged).To(BeZero(), "client operations that were not acknowledged after the Operator was killed")

	By("checking that the restarted Operator restarted no member and moved no role")
	now, slaves, err := r.master(context.Background())
	Expect(err).NotTo(HaveOccurred())
	Expect(now).To(Equal(master))
	Expect(faults.CheckUnchanged(membersBefore, r.identities(context.Background(), r.members))).To(Succeed(),
		"the DB Pods and their containers after the Operator was restarted")

	By("applying the data rules to every member and to both Services")
	var report workload.Report
	Eventually(func(g Gomega) {
		var err error
		report, err = r.dataCheck(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
	}, 3*time.Minute, 3*time.Second).Should(Succeed())
	for _, slave := range slaves {
		count, err := r.failCount(context.Background(), slave, master)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeZero(), "fail count of the applier on %s", slave)
	}

	files := r.evidenceFiles("S06/"+s06Restart, report)
	if timeline != "" {
		files = append(files, timeline)
	}
	back := outcome.OutcomeAt.Sub(outcome.FaultIssuedAt)
	lossWindow, lossResolution := r.observedLoss(report, outcome.FaultIssuedAt)
	result = judged(evidence.Scenario{
		ID: "S06", Variant: s06Restart,
		FaultConfirmed: &outcome.FaultConfirmed, CleanupSucceeded: &outcome.CleanupSucceeded,
		Measurements: map[string]any{
			// The Operator Pod killed until another one is available and
			// the status names the master with every condition True, from
			// an observation the replacement made. With an unchanged
			// cluster that observation is written within
			// observationWriteInterval of the replacement's first
			// reconcile, so this is an upper bound of the resync.
			"operatorBackWithin":         back.Round(time.Millisecond).String(),
			"operationsNotAcknowledged":  notAcknowledged,
			"shortestStableFor":          minStable.Round(time.Millisecond).String(),
			"membersRestarted":           0,
			"acknowledgedMissing":        report.AcknowledgedMissing,
			"observedDataLossWindow":     lossWindow,
			"observedDataLossResolution": lossResolution,
			"databaseFailoverObserved":   false,
		},
		Operations: &operations,
		Evidence:   files,
	}, within(operatorResyncLimit, back),
		faults.Bound{Limit: stablePeriod, Measured: minStable, AtLeast: true, Unmeasured: !measuredStable})
}

func (r *haRun) s06Absent() {
	result := evidence.Scenario{ID: "S06", Variant: s06Absent, Result: evidence.Fail,
		Reason: "a check of S06 failed; see the test output"}
	defer func() { recordScenario(result) }()

	const client = "s06a1"

	By("recording the starting state")
	master, slaves, err := r.master(context.Background())
	Expect(err).NotTo(HaveOccurred())
	masterUID, err := r.kubectl(context.Background(), "get", "pod", master, "-o", "jsonpath={.metadata.uid}")
	Expect(err).NotTo(HaveOccurred())

	By("starting a client that keeps writing through the read-write Service")
	r.startClient(client)
	defer r.killClient(client)

	// Whatever happens below, the suite must not be left without an Operator.
	defer func() {
		_, _ = utils.Run(exec.Command("kubectl", "-n", namespace, "scale", operatorDeployment, "--replicas=1"))
	}()

	By("removing the Operator, then deleting the master Pod while it is absent")
	absence := &operatorAbsence{}
	var newMaster, staleStatus string
	var membersBeforeReturn map[string]string
	var masterDeletedAt, lastObservedDuringAbsence time.Time
	var ageBeforeAbsence time.Duration
	outcome := r.runFault(faults.Scenario{
		Fault:  absence,
		Limits: s03FlowLimits,
		Before: func(ctx context.Context) error {
			if !operatorAvailable(ctx) {
				return fmt.Errorf("the Operator is not available")
			}
			// The status is fresh while the Operator runs, also on a cluster
			// in which nothing changes: otherwise a stale one during the
			// absence would prove nothing.
			s, err := r.status(ctx)
			if err != nil {
				return err
			}
			if s.lastObserved.IsZero() {
				return fmt.Errorf("status.lastObservedTime is not set while the Operator runs")
			}
			if ageBeforeAbsence = time.Since(s.lastObserved); ageBeforeAbsence > statusStaleAfter {
				return fmt.Errorf("status.lastObservedTime is %s old while the Operator runs, more than %s",
					ageBeforeAbsence.Round(time.Second), statusStaleAfter)
			}
			_, err = r.formationTime(ctx)
			return err
		},
		Check: func(ctx context.Context) (bool, error) {
			// With no Operator: delete the master Pod, once.
			if masterDeletedAt.IsZero() {
				kill := podDeletion{namespace: r.namespace, pod: master, uid: masterUID}
				if err := kill.Inject(ctx); err != nil {
					return false, fmt.Errorf("deleting the master Pod: %w", err)
				}
				masterDeletedAt = time.Now()
			}
			pods, err := operatorPods(ctx)
			if err != nil {
				return false, nil
			}
			if len(pods) != 0 {
				return false, fmt.Errorf("an Operator Pod exists during the absence: %v", pods)
			}
			// CUBRID's election, without the Operator.
			if newMaster == "" {
				if newMaster = r.activeMasterAmong(ctx, slaves); newMaster == "" {
					return false, nil
				}
			}
			// One master and two slaves again: the StatefulSet recreates the
			// deleted Pod and its member rejoins by itself.
			now, _, err := r.master(ctx)
			if err != nil {
				return false, nil
			}
			if now != newMaster {
				return false, fmt.Errorf("the master changed again, from %s to %s", newMaster, now)
			}
			if !r.lastAnswerAcknowledged(ctx, client) {
				return false, nil
			}
			// What the status says while nobody maintains it: its last
			// observation time stays where the Operator left it, and the
			// absence lasts until a reader can tell from it that it is stale.
			s, err := r.status(ctx)
			if err != nil {
				return false, nil
			}
			if s.lastObserved.IsZero() {
				return false, fmt.Errorf("status.lastObservedTime is not set")
			}
			if lastObservedDuringAbsence.IsZero() {
				lastObservedDuringAbsence = s.lastObserved
			} else if !s.lastObserved.Equal(lastObservedDuringAbsence) {
				return false, fmt.Errorf("status.lastObservedTime moved from %s to %s while no Operator ran",
					lastObservedDuringAbsence.Format(time.RFC3339), s.lastObserved.Format(time.RFC3339))
			}
			age := time.Since(s.lastObserved)
			if age <= statusStaleAfter {
				return false, nil
			}
			staleStatus = fmt.Sprintf("currentPrimary=%s PrimaryResolved=%s lastObservedTime=%s age=%s",
				s.primary, s.conditions["PrimaryResolved"], s.lastObserved.Format(time.RFC3339), age.Round(time.Second))
			// A member whose identity could not be read is read again: the
			// baseline must hold every member.
			membersBeforeReturn = r.identities(ctx, r.members)
			for _, id := range membersBeforeReturn {
				if id == faults.UnknownIdentity {
					return false, nil
				}
			}
			return true, nil
		},
	})
	timeline := writeTimeline("S06/"+s06Absent+"/timeline.jsonl", outcome.Timeline)
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	if outcome.Result != evidence.Pass {
		result.Result, result.Reason = outcome.Result, outcome.Reason
	}
	Expect(outcome.Result).To(Equal(evidence.Pass), "S06/%s: %s", s06Absent, outcome.Reason)
	Expect(outcome.CleanupSucceeded).To(BeTrue(), "the Operator's Deployment was not scaled back")

	By("waiting for the returned Operator to report the new master from a fresh observation")
	Eventually(func(g Gomega) {
		g.Expect(operatorAvailable(context.Background())).To(BeTrue(), "the Operator is available")
		s, err := r.status(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(s.primary).To(Equal(newMaster), "status.currentPrimary")
		// The API server stores the time to the second.
		g.Expect(s.lastObserved).To(BeTemporally(">=", absence.returnedAt.Truncate(time.Second)),
			"status.lastObservedTime is not newer than the Operator's return")
		g.Expect(time.Since(s.lastObserved)).To(BeNumerically("<=", statusStaleAfter), "status.lastObservedTime is stale")
		_, err = r.formationTime(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
	}, 5*time.Minute, time.Second).Should(Succeed())
	resync := time.Since(absence.returnedAt)

	By("letting the client run for the stable period, then stopping it")
	time.Sleep(stablePeriod.Value + 2*time.Second)
	events, history := r.stopClient(client)
	recoveredAt, stable, recovered := workload.Recovery(events, masterDeletedAt)
	Expect(recovered).To(BeTrue(), "the client's last answer after the master was deleted is not an acknowledgement")
	Expect(stable).To(BeNumerically(">=", stablePeriod.Value),
		"the client was acknowledged without interruption for %s only", stable)
	recovery := recoveredAt.Sub(masterDeletedAt)

	By("checking that the returned Operator caused no further failover and restarted no member")
	now, nowSlaves, err := r.master(context.Background())
	Expect(err).NotTo(HaveOccurred())
	Expect(now).To(Equal(newMaster), "the master after the Operator returned")
	Expect(nowSlaves).To(ContainElement(master), "the former master is a slave")
	Expect(faults.CheckUnchanged(membersBeforeReturn, r.identities(context.Background(), r.members))).To(Succeed(),
		"the DB Pods and their containers after the Operator returned")

	By("applying the data rules to every member and to both Services")
	var report workload.Report
	Eventually(func(g Gomega) {
		var err error
		report, err = r.dataCheck(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
	}, 3*time.Minute, 3*time.Second).Should(Succeed())
	for _, slave := range nowSlaves {
		count, err := r.failCount(context.Background(), slave, newMaster)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeZero(), "fail count of the applier on %s", slave)
	}

	files := r.evidenceFiles("S06/"+s06Absent, report)
	if timeline != "" {
		files = append(files, timeline)
	}
	lossWindow, lossResolution := r.observedLoss(report, masterDeletedAt)
	result = judged(evidence.Scenario{
		ID: "S06", Variant: s06Absent,
		FaultConfirmed: &outcome.FaultConfirmed, CleanupSucceeded: &outcome.CleanupSucceeded,
		Measurements: map[string]any{
			// While the Operator was absent.
			"recoveryTime":        recovery.Round(time.Millisecond).String(),
			"stableFor":           stable.Round(time.Millisecond).String(),
			"operatorAbsentFor":   absence.returnedAt.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String(),
			"statusDuringAbsence": staleStatus,
			// Just before the absence, while the Operator ran.
			"lastObservedAgeBeforeAbsence": ageBeforeAbsence.Round(time.Second).String(),
			// After it was started again: until the status named the new
			// master with every condition True.
			"operatorResyncTime":         resync.Round(time.Millisecond).String(),
			"membersRestartedOnReturn":   0,
			"acknowledgedMissing":        report.AcknowledgedMissing,
			"observedDataLossWindow":     lossWindow,
			"observedDataLossResolution": lossResolution,
			"outcomeUnknown":             history.Counts[workload.Unknown],
		},
		Operations: &evidence.Operations{
			Attempted: history.Counts[workload.Attempted], Acknowledged: history.Counts[workload.Acknowledged],
			Failed: history.Counts[workload.Failed], Unknown: history.Counts[workload.Unknown],
		},
		Evidence: files,
	}, within(operatorResyncLimit, resync), within(failoverLimit, recovery), stableFor(stable))
}
