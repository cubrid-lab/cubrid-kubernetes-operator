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

// operatorAvailable reports whether the Operator's Deployment has its Pod
// available.
func operatorAvailable() bool {
	out, err := utils.Run(exec.Command("kubectl", "-n", namespace, "get", operatorDeployment,
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

// memberUIDs returns the UIDs of the DB Pods, in the order of the members.
func (r *haRun) memberUIDs() (string, error) {
	uids := make([]string, 0, len(r.members))
	for _, pod := range r.members {
		uid, err := r.kubectl("get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
		if err != nil {
			return "", err
		}
		uids = append(uids, pod+"="+uid)
	}
	return strings.Join(uids, " "), nil
}

// s06Step registers one variant of S06 (Operator failure) of the scenario
// contract, on the cluster the earlier steps left.
func s06Step(r *haRun, variant string) {
	switch variant {
	case s06Restart:
		It("S06/"+variant+": SQL is not interrupted and no member is restarted when the Operator Pod is killed",
			func() { r.s06Restart() })
	case s06Absent:
		It("S06/"+variant+": CUBRID fails over without the Operator, which reports the new master when it returns",
			func() { r.s06Absent() })
	}
}

func (r *haRun) s06Restart() {
	result := evidence.Scenario{ID: "S06", Variant: s06Restart, Result: evidence.Fail,
		Reason: "a check of S06 failed; see the test output"}
	defer func() { r.later = append(r.later, result) }()

	By("recording the starting state")
	master, _, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	membersBefore, err := r.memberUIDs()
	Expect(err).NotTo(HaveOccurred())
	pods, err := operatorPods(context.Background())
	Expect(err).NotTo(HaveOccurred())
	Expect(pods).To(HaveLen(1), "Operator Pods")

	By("starting clients that keep writing through the read-write Service")
	clients := []string{"s06r1", "s06r2"}
	for _, id := range clients {
		r.startClient(id)
		defer r.killClient(id)
	}

	By("killing the Operator Pod and waiting for its replacement")
	outcome := faults.Run(context.Background(), faults.Scenario{
		Fault:  operatorPodKill{pod: pods[0]},
		Limits: s03FlowLimits,
		Before: func(context.Context) error {
			if !operatorAvailable() {
				return fmt.Errorf("the Operator is not available")
			}
			_, err := r.formationTime()
			return err
		},
		Check: func(context.Context) (bool, error) {
			now, _, err := r.master()
			if err != nil {
				return false, nil
			}
			if now != master {
				return false, fmt.Errorf("the master changed from %s to %s when the Operator was restarted", master, now)
			}
			if !operatorAvailable() {
				return false, nil
			}
			if s, err := r.status(); err != nil || s.primary != master {
				return false, nil
			}
			_, err = r.formationTime()
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
	time.Sleep(stablePeriod + 2*time.Second)
	operations := evidence.Operations{}
	notAcknowledged := 0
	for _, id := range clients {
		events, history := r.stopClient(id)
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
	now, slaves, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	Expect(now).To(Equal(master))
	membersAfter, err := r.memberUIDs()
	Expect(err).NotTo(HaveOccurred())
	Expect(membersAfter).To(Equal(membersBefore), "the DB Pods after the Operator was restarted")

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

	files := r.evidenceFiles("S06/"+s06Restart, report)
	if timeline != "" {
		files = append(files, timeline)
	}
	back := outcome.OutcomeAt.Sub(outcome.FaultIssuedAt)
	result = judged(evidence.Scenario{
		ID: "S06", Variant: s06Restart,
		FaultConfirmed: &outcome.FaultConfirmed, CleanupSucceeded: &outcome.CleanupSucceeded,
		Measurements: map[string]any{
			// The Operator Pod killed until another one is available and
			// the status names the master with every condition True.
			"operatorBackWithin":        back.Round(time.Millisecond).String(),
			"operationsNotAcknowledged": notAcknowledged,
			"membersRestarted":          0,
			"acknowledgedMissing":       report.AcknowledgedMissing,
			"databaseFailoverObserved":  false,
		},
		Operations: &operations,
		Evidence:   files,
	}, "operator_resync_limit", operatorResyncLimit, back)
}

func (r *haRun) s06Absent() {
	result := evidence.Scenario{ID: "S06", Variant: s06Absent, Result: evidence.Fail,
		Reason: "a check of S06 failed; see the test output"}
	defer func() { r.later = append(r.later, result) }()

	const client = "s06a1"

	By("recording the starting state")
	master, slaves, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	masterUID, err := r.kubectl("get", "pod", master, "-o", "jsonpath={.metadata.uid}")
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
	var newMaster, staleStatus, membersBeforeReturn string
	var masterDeletedAt time.Time
	outcome := faults.Run(context.Background(), faults.Scenario{
		Fault:  absence,
		Limits: s03FlowLimits,
		Before: func(context.Context) error {
			if !operatorAvailable() {
				return fmt.Errorf("the Operator is not available")
			}
			_, err := r.formationTime()
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
				if newMaster = r.activeMasterAmong(slaves); newMaster == "" {
					return false, nil
				}
			}
			// One master and two slaves again: the StatefulSet recreates the
			// deleted Pod and its member rejoins by itself.
			now, _, err := r.master()
			if err != nil {
				return false, nil
			}
			if now != newMaster {
				return false, fmt.Errorf("the master changed again, from %s to %s", newMaster, now)
			}
			if !r.lastAnswerAcknowledged(client) {
				return false, nil
			}
			// What the status says while nobody maintains it.
			if s, err := r.status(); err == nil {
				staleStatus = fmt.Sprintf("currentPrimary=%s PrimaryResolved=%s", s.primary, s.conditions["PrimaryResolved"])
			}
			membersBeforeReturn, err = r.memberUIDs()
			return err == nil, nil
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

	By("waiting for the returned Operator to report the new master")
	Eventually(func(g Gomega) {
		g.Expect(operatorAvailable()).To(BeTrue(), "the Operator is available")
		s, err := r.status()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(s.primary).To(Equal(newMaster), "status.currentPrimary")
		_, err = r.formationTime()
		g.Expect(err).NotTo(HaveOccurred())
	}, 5*time.Minute, time.Second).Should(Succeed())
	resync := time.Since(absence.returnedAt)

	By("letting the client run for the stable period, then stopping it")
	time.Sleep(stablePeriod + 2*time.Second)
	events, history := r.stopClient(client)
	recoveredAt, stable, recovered := workload.Recovery(events, masterDeletedAt)
	Expect(recovered).To(BeTrue(), "the client's last answer after the master was deleted is not an acknowledgement")
	Expect(stable).To(BeNumerically(">=", stablePeriod),
		"the client was acknowledged without interruption for %s only", stable)
	recovery := recoveredAt.Sub(masterDeletedAt)

	By("checking that the returned Operator caused no further failover and restarted no member")
	now, nowSlaves, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	Expect(now).To(Equal(newMaster), "the master after the Operator returned")
	Expect(nowSlaves).To(ContainElement(master), "the former master is a slave")
	membersAfter, err := r.memberUIDs()
	Expect(err).NotTo(HaveOccurred())
	Expect(membersAfter).To(Equal(membersBeforeReturn), "the DB Pods after the Operator returned")

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

	files := r.evidenceFiles("S06/"+s06Absent, report)
	if timeline != "" {
		files = append(files, timeline)
	}
	judgedResult := judged(evidence.Scenario{
		ID: "S06", Variant: s06Absent,
		FaultConfirmed: &outcome.FaultConfirmed, CleanupSucceeded: &outcome.CleanupSucceeded,
		Measurements: map[string]any{
			// While the Operator was absent.
			"recoveryTime":        recovery.Round(time.Millisecond).String(),
			"stableFor":           stable.Round(time.Millisecond).String(),
			"operatorAbsentFor":   absence.returnedAt.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String(),
			"statusDuringAbsence": staleStatus,
			// After it was started again: until the status named the new
			// master with every condition True.
			"operatorResyncTime":       resync.Round(time.Millisecond).String(),
			"membersRestartedOnReturn": 0,
			"acknowledgedMissing":      report.AcknowledgedMissing,
			"outcomeUnknown":           history.Counts[workload.Unknown],
		},
		Operations: &evidence.Operations{
			Attempted: history.Counts[workload.Attempted], Acknowledged: history.Counts[workload.Acknowledged],
			Failed: history.Counts[workload.Failed], Unknown: history.Counts[workload.Unknown],
		},
		Evidence: files,
	}, "operator_resync_limit", operatorResyncLimit, resync)
	judgedResult.Limits["failover_limit"] = failoverLimit.String()
	judgedResult.Limits["stable_period"] = stablePeriod.String()
	if judgedResult.Result == evidence.Pass && recovery > failoverLimit {
		judgedResult.Result = evidence.Fail
		judgedResult.Reason = fmt.Sprintf("failover_limit exceeded: %s > %s", recovery, failoverLimit)
	}
	result = judgedResult
}
