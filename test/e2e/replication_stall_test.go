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
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// stallForceRetryEnv, set to "true", makes the first attempt of the
// replication-stall step give up on the failover at once, so that a run
// shows the path of a retry.
const stallForceRetryEnv = "E2E_STALL_FORCE_RETRY"

// replicationStallStep registers a check of the ReplicationHealthy condition
// on a real engine (#229). It puts a slave into the state of POC-21 in
// docs/poc/RESULTS.md: heartbeat is stopped inside its running container, the
// master's Pod is deleted, the remaining member becomes master, and heartbeat
// is started again in the same container. The slave then looks healthy and
// applies nothing. Deleting its Pod repairs it.
//
// It is not a scenario of the contract. Into the summary it reports only an
// attempt that did not reach its precondition, as a note.
func replicationStallStep(r *haRun) {
	It("reports a slave that looks healthy and applies nothing, and no longer after its Pod is replaced", func() {
		inPod := func(pod, script string) (string, error) {
			return r.kubectl("exec", pod, "--", "bash", "-c", `export PATH="${CUBRID}/bin:${PATH}"; `+script)
		}
		condition := func() (string, string, string) {
			field := func(name string) string {
				out, _ := r.kubectl("get", "cubridcluster", r.cluster, "-o",
					`jsonpath={.status.conditions[?(@.type=="ReplicationHealthy")].`+name+`}`)
				return out
			}
			return field("status"), field("reason"), field("message")
		}
		// restarts is the restart count of the container of a member's Pod;
		// a count that could not be read is an error, never a count.
		restarts := func(pod string) (string, error) {
			out, err := r.kubectl("get", "pod", pod, "-o", "jsonpath={.status.containerStatuses[0].restartCount}")
			if err == nil && out == "" {
				err = fmt.Errorf("pod %s reports no restart count", pod)
			}
			return out, err
		}
		// observed is what CUBRID, the Pods and the Operator report on every
		// member, for a failure message and for the record of an attempt.
		observed := func() string {
			var roles roleLog
			roles.sample(r)
			instances, _ := r.kubectl("get", "cubridcluster", r.cluster, "-o",
				`jsonpath={range .status.instances[*]}{.name}: {.role} {.replication}{"\n"}{end}`)
			return fmt.Sprintf("CUBRID: %s\nPods (uid/restarts): %s\nstatus.instances:\n%s",
				roles.last, r.memberIdentities(), instances)
		}
		// Each run of the step writes with client IDs of its own.
		r.mu.Lock()
		r.markers++
		clients := fmt.Sprintf("st%d", r.markers)
		r.mu.Unlock()

		// The member whose heartbeat is stopped has to be in the same
		// container when it is started again: its Instance Manager ends a
		// member whose CUBRID processes stay away for longer than its grace
		// (30 s, checked every 5 s), and a new container is not in this
		// state. The failover therefore has to come within that time, and
		// CUBRID does not always make it: a run saw the remaining member stay
		// a slave until the stopped member's container was restarted and
		// became master (#342). Such an attempt does not reach the state; its
		// stopped member's Pod is replaced, and the step starts again from
		// the common starting state.
		const attempts = 3
		// failoverWait counts from the start of the heartbeat stop. It keeps
		// the start of heartbeat again, whose processes come up within a few
		// seconds, inside the grace. Passing runs saw the failover about 8 s
		// after the stop.
		const failoverWait = 20 * time.Second
		name := stepName(CurrentSpecReport())
		var master, victim, newMaster, restartsBefore string
		var tries []string
		for attempt := 1; ; attempt++ {
			if attempt == 1 {
				r.enter(nil)
			} else if e := r.admit(fmt.Sprintf("%s (attempt %d)", name, attempt), nil); e.Reason != "" {
				Fail(fmt.Sprintf("attempt %d could not start: the starting state was not restored after attempt %d: %s\n%s",
					attempt, attempt-1, e.Reason, strings.Join(tries, "\n\n")))
			}
			By(fmt.Sprintf("recording the starting state (attempt %d of %d)", attempt, attempts))
			var slaves []string
			var err error
			master, slaves, err = r.master()
			Expect(err).NotTo(HaveOccurred())
			victim = slaves[0]
			var candidate string
			for _, m := range r.members {
				if m != victim && m != master {
					candidate = m
				}
			}
			Eventually(func(g Gomega) {
				status, _, message := condition()
				g.Expect(status).To(Equal("True"), "ReplicationHealthy before the test: %s", message)
			}, 2*time.Minute, 3*time.Second).Should(Succeed())
			restartsBefore, err = restarts(victim)
			Expect(err).NotTo(HaveOccurred())
			wait := failoverWait
			forced := attempt == 1 && os.Getenv(stallForceRetryEnv) == "true"
			if forced {
				wait = 0
			}

			By("stopping heartbeat inside the running container of " + victim)
			stopped := time.Now()
			_, err = inPod(victim, `cubrid heartbeat stop >/tmp/hb-stop.log 2>&1`)
			Expect(err).NotTo(HaveOccurred())

			// What the master logged before it ends, for an attempt in which
			// the failover does not come.
			masterLog, err := r.kubectl("logs", master, "--tail=300")
			if err != nil {
				masterLog = err.Error()
			}
			// The deleted master is not a new master: until its Pod ends, it
			// can still report itself as one.
			By("deleting the master Pod " + master + " and waiting for " + candidate + " to be master")
			_, err = r.kubectl("delete", "pod", master, "--wait=false")
			Expect(err).NotTo(HaveOccurred())
			var roles roleLog
			newMaster = ""
			for time.Since(stopped) < wait && newMaster == "" {
				roles.sample(r)
				newMaster = r.activeMasterAmong([]string{candidate})
				if newMaster == "" {
					time.Sleep(time.Second)
				}
			}
			now, err := restarts(victim)
			if newMaster != "" && err == nil && now == restartsBefore {
				break
			}
			if err != nil {
				now = "unknown (" + err.Error() + ")"
			}
			why := fmt.Sprintf("%s was not master within %s of the stop", candidate, wait)
			switch {
			case forced:
				why = fmt.Sprintf("given up at once on purpose (%s=true)", stallForceRetryEnv)
			case newMaster != "":
				why = fmt.Sprintf("%s was master, but the restart count of %s was %s, and %s before the stop",
					candidate, victim, now, restartsBefore)
			}
			try := fmt.Sprintf("attempt %d of %d: heartbeat stopped on %s at %s, master %s deleted; %s; "+
				"restarts of %s: %s -> %s\nroles during the wait:\n%s\nafter the wait:\n%s",
				attempt, attempts, victim, stopped.UTC().Format(time.RFC3339), master, why, victim, restartsBefore, now,
				strings.Join(roles.lines, "\n"), observed())
			tries = append(tries, try)
			writeScenarioFile("replication-stall/attempts.txt", []byte(strings.Join(tries, "\n\n")+"\n"))
			writeScenarioFile(fmt.Sprintf("replication-stall/attempt-%d-master-%s.log", attempt, master), []byte(masterLog))
			AddReportEntry("replication-stall attempt without the failover", try)
			runSummary.Notes = append(runSummary.Notes, fmt.Sprintf(
				"%s: attempt %d of %d did not reach its precondition: %s", name, attempt, attempts, why))
			_, _ = fmt.Fprintln(GinkgoWriter, try)
			if attempt == attempts {
				Fail("the failover this step needs did not come in any attempt:\n" + strings.Join(tries, "\n\n"))
			}
			By("replacing the Pod of " + victim + ", so that it is not left in the state of this step")
			_, err = r.kubectl("delete", "pod", victim, "--wait=false")
			Expect(err).NotTo(HaveOccurred())
		}

		// No pipe after a command that starts daemons: they would keep it open.
		By("starting heartbeat again in the same container")
		_, err := inPod(victim, `cubrid heartbeat start >/tmp/hb-start.log 2>&1`)
		Expect(err).NotTo(HaveOccurred())

		By("writing on the new master")
		Eventually(func() error {
			_, err := r.runWorkload("CLIENT_ID=" + clients + "a OPS=20 ROLLBACK_EVERY=0")
			return err
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			m, slaves, err := r.master()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(m).To(Equal(newMaster))
			g.Expect(slaves).To(ContainElement(victim))
		}, 3*time.Minute, 3*time.Second).Should(Succeed(), func() string {
			return fmt.Sprintf("%s master, and the other two slaves, the stopped member %s among them\n%s",
				newMaster, victim, observed())
		})

		restartsAfter, err := restarts(victim)
		Expect(err).NotTo(HaveOccurred())
		Expect(restartsAfter).To(Equal(restartsBefore),
			"the container of %s was restarted, so the member is not in the state this step is about", victim)

		By("writing more, so that log pages wait on the slave")
		h, err := r.runWorkload("CLIENT_ID=" + clients + "b OPS=40 ROLLBACK_EVERY=0")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.Counts[workload.Acknowledged]).To(Equal(40))

		By("waiting for the operator to report the slave as stalled")
		var message string
		Eventually(func(g Gomega) {
			status, reason, m := condition()
			message = m
			g.Expect(status).To(Equal("False"), "ReplicationHealthy: %s %s", reason, m)
			g.Expect(reason).To(Equal("ReplicationStalled"))
			g.Expect(m).To(ContainSubstring(victim))
		}, 4*time.Minute, 5*time.Second).Should(Succeed(), func() string {
			return fmt.Sprintf("the stopped member %s, new master %s\n%s", victim, newMaster, observed())
		})
		for _, m := range r.members {
			if m != victim && m != newMaster {
				Expect(message).NotTo(ContainSubstring(m), "the healthy slave is named as stalled")
			}
		}

		By("checking that the slave is behind, and that the other conditions do not show it")
		// rows reads a member's rows; a failure is cut short, because the
		// whole output of csql would bury it in the log.
		rows := func(pod string) []workload.Row {
			var out []workload.Row
			Eventually(func() string {
				var err error
				if out, err = r.memberRows(pod); err != nil {
					message := err.Error()
					if len(message) > 1500 {
						message = message[:1500] + " ..."
					}
					writeScenarioFile("replication-stall/rows-error-"+pod+".txt", []byte(err.Error()))
					return message
				}
				return ""
			}, time.Minute, 5*time.Second).Should(BeEmpty(), "reading the rows of %s", pod)
			return out
		}
		behind := rows(victim)
		ahead := rows(newMaster)
		Expect(len(behind)).To(BeNumerically("<", len(ahead)), "rows on the stalled slave and on the master")
		haReady, err := r.kubectl("get", "cubridcluster", r.cluster, "-o",
			`jsonpath={.status.conditions[?(@.type=="HAReady")].status}`)
		Expect(err).NotTo(HaveOccurred())
		record := fmt.Sprintf("stalled member: %s\nnew master: %s\nrows on the stalled member: %d\nrows on the master: %d\n"+
			"ReplicationHealthy: False ReplicationStalled: %s\nHAReady at the same time: %s\n",
			victim, newMaster, len(behind), len(ahead), message, haReady)

		By("deleting the Pod of the stalled slave")
		_, err = r.kubectl("delete", "pod", victim, "--wait=false")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			status, reason, m := condition()
			g.Expect(status).To(Equal("True"), "ReplicationHealthy after the Pod was replaced: %s %s", reason, m)
			report, err := r.dataCheck()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
		}, 5*time.Minute, 5*time.Second).Should(Succeed(), observed)
		after := rows(victim)
		record += fmt.Sprintf("rows on the member after its Pod was replaced: %d\nReplicationHealthy: True\n", len(after))
		writeScenarioFile("replication-stall/result.txt", []byte(strings.TrimSpace(record)+"\n"))
	})

	// A member whose CUBRID processes are stopped and stay stopped is not
	// left that way: its Instance Manager ends, the container is restarted,
	// and the member comes back by the ordinary start (#180).
	It("restarts the container of a member whose CUBRID processes were stopped", func() {
		r.enter(nil)
		_, slaves, err := r.master()
		Expect(err).NotTo(HaveOccurred())
		victim := slaves[0]
		restarts := func() string {
			out, _ := r.kubectl("get", "pod", victim, "-o", "jsonpath={.status.containerStatuses[0].restartCount}")
			return out
		}
		before := restarts()

		// The watchdog cares only after it has once seen every process
		// running, about five seconds after the Instance Manager listens. The
		// victim may be the member whose Pod the step before replaced.
		By("waiting until the Instance Manager of " + victim + " watches the CUBRID processes")
		Eventually(func(g Gomega) {
			log, err := r.kubectl("logs", victim, "-c", "cubrid", "--tail=-1")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(log).To(ContainSubstring(`"event":"process_watch_started"`))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("stopping heartbeat inside the running container of " + victim + " and leaving it stopped")
		stopped := time.Now()
		_, err = r.kubectl("exec", victim, "--", "bash", "-c",
			`export PATH="${CUBRID}/bin:${PATH}"; cubrid heartbeat stop >/tmp/hb-stop.log 2>&1`)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the container to be restarted")
		Eventually(restarts, 3*time.Minute, 2*time.Second).ShouldNot(Equal(before))
		restartedAfter := time.Since(stopped)

		By("reading why from the log of the container that ended")
		previous, err := r.kubectl("logs", victim, "--previous", "--tail=-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(previous).To(ContainSubstring(`"event":"process_gone"`))

		By("waiting for the member to be a slave again that holds everything")
		Eventually(func(g Gomega) {
			_, nowSlaves, err := r.master()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(nowSlaves).To(ContainElement(victim))
			report, err := r.dataCheck()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
		writeScenarioFile("process-watchdog/result.txt", []byte(fmt.Sprintf(
			"member: %s\ncontainer restarted after: %s\nrestart count: %s -> %s\n",
			victim, restartedAfter.Round(time.Second), before, restarts())))
	})
}
