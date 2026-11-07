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
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
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
// From the start of heartbeat again until ReplicationHealthy is True after
// the repair, the status is read every two seconds together with the count of
// the ledger rows on the stalled slave and on the master, and compared by the
// rules of test/faults (docs/testing/scenario-contract.md, "Conditions during
// the fault"): conditions.jsonl and agreement.txt are kept under
// replication-stall/, and result.txt carries the conditionAgreement
// measurement. A disagreement, or a rule that could not be judged, fails the
// step.
//
// It is not a scenario of the contract. Into the summary it reports only an
// attempt that did not reach its precondition, as a note.
func replicationStallStep(r *haRun) {
	It("reports a slave that looks healthy and applies nothing, and no longer after its Pod is replaced", func() {
		inPod := func(pod, script string) (string, error) {
			return r.kubectl(context.Background(),
				"exec", pod, "--", "bash", "-c", `export PATH="${CUBRID}/bin:${PATH}"; `+script)
		}
		condition := func() (string, string, string) {
			field := func(name string) string {
				out, _ := r.kubectl(context.Background(), "get", "cubridcluster", r.cluster, "-o",
					`jsonpath={.status.conditions[?(@.type=="ReplicationHealthy")].`+name+`}`)
				return out
			}
			return field("status"), field("reason"), field("message")
		}
		// restarts is the restart count of the container of a member's Pod;
		// a count that could not be read is an error, never a count.
		restarts := func(pod string) (string, error) {
			out, err := r.kubectl(context.Background(),
				"get", "pod", pod, "-o", "jsonpath={.status.containerStatuses[0].restartCount}")
			if err == nil && out == "" {
				err = fmt.Errorf("pod %s reports no restart count", pod)
			}
			return out, err
		}
		// observed is what CUBRID, the Pods and the Operator report on every
		// member, for a failure message and for the record of an attempt.
		observed := func() string {
			var roles roleLog
			roles.sample(context.Background(), r)
			instances, _ := r.kubectl(context.Background(), "get", "cubridcluster", r.cluster, "-o",
				`jsonpath={range .status.instances[*]}{.name}: {.role} {.replication}{"\n"}{end}`)
			return fmt.Sprintf("CUBRID: %s\nPods (uid/restarts): %s\nstatus.instances:\n%s",
				roles.last, r.memberIdentities(context.Background()), instances)
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
			master, slaves, err = r.master(context.Background())
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
			masterLog, err := r.kubectl(context.Background(), "logs", master, "--tail=300")
			if err != nil {
				masterLog = err.Error()
			}
			// The deleted master is not a new master: until its Pod ends, it
			// can still report itself as one.
			By("deleting the master Pod " + master + " and waiting for " + candidate + " to be master")
			_, err = r.kubectl(context.Background(), "delete", "pod", master, "--wait=false")
			Expect(err).NotTo(HaveOccurred())
			var roles roleLog
			newMaster = ""
			for time.Since(stopped) < wait && newMaster == "" {
				roles.sample(context.Background(), r)
				newMaster = r.activeMasterAmong(context.Background(), []string{candidate})
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
			note := fmt.Sprintf("%s: attempt %d of %d did not reach its precondition: %s",
				name, attempt, attempts, why)
			updateSummary(func(s *evidence.Summary) { s.Notes = append(s.Notes, note) })
			_, _ = fmt.Fprintln(GinkgoWriter, try)
			if attempt == attempts {
				Fail("the failover this step needs did not come in any attempt:\n" + strings.Join(tries, "\n\n"))
			}
			By("replacing the Pod of " + victim + ", so that it is not left in the state of this step")
			_, err = r.kubectl(context.Background(), "delete", "pod", victim, "--wait=false")
			Expect(err).NotTo(HaveOccurred())
		}

		// The healthy slave is the former master, whose Pod this step
		// deleted: its replacement Pod may still be starting or rejoining.
		var healthySlave string
		for _, m := range r.members {
			if m != victim && m != newMaster {
				healthySlave = m
			}
		}
		By("reading the cluster's Conditions and the rows of " + victim + " and " + newMaster + " every two seconds")
		sampler := r.sampleConditions(conditionSampling{rows: []string{victim, newMaster}})
		defer sampler.stop()

		// No pipe after a command that starts daemons: they would keep it open.
		By("starting heartbeat again in the same container")
		stallStarted := time.Now().UTC()
		_, err := inPod(victim, `cubrid heartbeat start >/tmp/hb-start.log 2>&1`)
		Expect(err).NotTo(HaveOccurred())

		By("writing on the new master")
		Eventually(func() error {
			_, err := r.runWorkload(context.Background(), "CLIENT_ID="+clients+"a OPS=20 ROLLBACK_EVERY=0")
			return err
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
		// Before this, the stall may be reported for the log of the failover
		// alone, while the stalled slave and the master hold the same rows.
		writesAcknowledged := time.Now().UTC()
		Eventually(func(g Gomega) {
			m, slaves, err := r.master(context.Background())
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
		h, err := r.runWorkload(context.Background(), "CLIENT_ID="+clients+"b OPS=40 ROLLBACK_EVERY=0")
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
		stallReported := time.Now().UTC()
		for _, m := range r.members {
			if m != victim && m != newMaster {
				Expect(message).NotTo(ContainSubstring(m), "the healthy slave is named as stalled")
			}
		}

		By("checking that HAReady follows ReplicationHealthy to False (#249)")
		Eventually(func(g Gomega) {
			status, reason := haReadyOf(r)
			g.Expect(status).To(Equal("False"), "HAReady: %s %s", status, reason)
			g.Expect(reason).To(Equal("ReplicationStalled"))
		}, time.Minute, 5*time.Second).Should(Succeed(), observed)

		By("checking that the slave is behind")
		// rows reads a member's rows; a failure is cut short, because the
		// whole output of csql would bury it in the log.
		rows := func(pod string) []workload.Row {
			var out []workload.Row
			Eventually(func() string {
				var err error
				if out, err = r.memberRows(context.Background(), pod); err != nil {
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
		haReady, _ := haReadyOf(r)
		record := fmt.Sprintf("stalled member: %s\nnew master: %s\nrows on the stalled member: %d\nrows on the master: %d\n"+
			"ReplicationHealthy: False ReplicationStalled: %s\nHAReady at the same time: %s\n",
			victim, newMaster, len(behind), len(ahead), message, haReady)

		By("deleting the Pod of the stalled slave")
		repairIssued := time.Now().UTC()
		_, err = r.kubectl(context.Background(), "delete", "pod", victim, "--wait=false")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			status, reason, m := condition()
			g.Expect(status).To(Equal("True"), "ReplicationHealthy after the Pod was replaced: %s %s", reason, m)
			ha, haReason := haReadyOf(r)
			g.Expect(ha).To(Equal("True"), "HAReady after the Pod was replaced: %s", haReason)
			report, err := r.dataCheck(context.Background())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
		}, 5*time.Minute, 5*time.Second).Should(Succeed(), observed)
		after := rows(victim)
		record += fmt.Sprintf("rows on the member after its Pod was replaced: %d\n"+
			"ReplicationHealthy: True\nHAReady: True\n", len(after))

		By("comparing the sampled Conditions with the rows")
		// The stall is the fault, and its report the outcome: the rules need
		// judged reads while the stall waited to be reported, and after.
		steps := []faults.Step{
			{T: stallStarted, Kind: "fault_issued", Detail: "heartbeat started again on " + victim},
			{T: stallReported, Kind: "outcome_reached", Detail: "ReplicationHealthy names " + victim + " as stalled"},
		}
		agreement, _, measured := checkConditions("replication-stall", sampler.stop(), nil, steps,
			slices.Concat(faults.NeverRules(), faults.HAReportRules(), faults.StallRules(faults.Stall{Victim: victim, Master: newMaster,
				HealthySlave: healthySlave, WritesAcknowledged: writesAcknowledged, RepairIssued: repairIssued})))
		if line, err := json.Marshal(measured); err == nil {
			record += "conditionAgreement: " + string(line) + "\n"
		}
		writeScenarioFile("replication-stall/result.txt", []byte(strings.TrimSpace(record)+"\n"))
		Expect(stallVerdict(agreement)).To(Succeed(), "see replication-stall/agreement.txt")
	})

	// A member whose CUBRID processes are stopped and stay stopped is not
	// left that way: its Instance Manager ends, the container is restarted,
	// and the member comes back by the ordinary start (#180).
	It("restarts the container of a member whose CUBRID processes were stopped", func() {
		r.enter(nil)
		_, slaves, err := r.master(context.Background())
		Expect(err).NotTo(HaveOccurred())
		victim := slaves[0]
		restarts := func() string {
			out, _ := r.kubectl(context.Background(),
				"get", "pod", victim, "-o", "jsonpath={.status.containerStatuses[0].restartCount}")
			return out
		}
		before := restarts()

		// The watchdog cares only after it has once seen every process
		// running, about five seconds after the Instance Manager listens. The
		// victim may be the member whose Pod the step before replaced.
		By("waiting until the Instance Manager of " + victim + " watches the CUBRID processes")
		Eventually(func(g Gomega) {
			log, err := r.kubectl(context.Background(), "logs", victim, "-c", "cubrid", "--tail=-1")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(log).To(ContainSubstring(`"event":"process_watch_started"`))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("stopping heartbeat inside the running container of " + victim + " and leaving it stopped")
		stopped := time.Now()
		_, err = r.kubectl(context.Background(), "exec", victim, "--", "bash", "-c",
			`export PATH="${CUBRID}/bin:${PATH}"; cubrid heartbeat stop >/tmp/hb-stop.log 2>&1`)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the container to be restarted")
		Eventually(restarts, 3*time.Minute, 2*time.Second).ShouldNot(Equal(before))
		restartedAfter := time.Since(stopped)

		By("reading why from the log of the container that ended")
		previous, err := r.kubectl(context.Background(), "logs", victim, "--previous", "--tail=-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(previous).To(ContainSubstring(`"event":"process_gone"`))

		By("waiting for the member to be a slave again that holds everything")
		Eventually(func(g Gomega) {
			_, nowSlaves, err := r.master(context.Background())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(nowSlaves).To(ContainElement(victim))
			report, err := r.dataCheck(context.Background())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
		writeScenarioFile("process-watchdog/result.txt", []byte(fmt.Sprintf(
			"member: %s\ncontainer restarted after: %s\nrestart count: %s -> %s\n",
			victim, restartedAfter.Round(time.Second), before, restarts())))
	})
}

// haReadyOf returns the status and reason of the cluster's HAReady condition.
func haReadyOf(r *haRun) (string, string) {
	out, _ := r.kubectl(context.Background(), "get", "cubridcluster", r.cluster, "-o",
		`jsonpath={range .status.conditions[?(@.type=="HAReady")]}{.status}/{.reason}{end}`)
	status, reason, _ := strings.Cut(out, "/")
	return status, reason
}

// activeCSQL matches an active csql transaction in the output of cubrid
// tranlist, capturing its transaction time in seconds: the columns are the
// transaction index and state, user, host, process ID, program, query time
// and transaction time.
var activeCSQL = regexp.MustCompile(`(?m)\(ACTIVE\)\s+\S+\s+\S+\s+\d+\s+csql\s+[\d.]+\s+([\d.]+)`)

// Shape of the transaction that openTransactionStep holds open: rounds of
// openTxRows rows, one round every openTxPause, then the commit. 20 rounds of
// 5 s keep it open for about 100 s, past the 90 s the decision on #249 asks
// for and past the 60-second stall window.
const (
	openTxRounds = 20
	openTxRows   = 200
	openTxPause  = 5 * time.Second
)

// openTransactionStep registers a check that a transaction held open on the
// master does not make its slaves look stalled (#249). While a transaction
// stays open, the applier's counters do not move, but its position in the
// copied log does (docs/poc/RESULTS.md, POC-48), and the Operator judges the
// stall by that position. One csql session without auto-commit inserts into a
// table with a primary key and without AUTO_INCREMENT, whose serial would move
// the update counter on its own (POC-48, parts A and A2), for about 100 s and
// then commits. ReplicationHealthy and HAReady must stay True at every sample
// of that time, and every slave must hold all the rows after the commit.
//
// It is not a scenario of the contract.
func openTransactionStep(r *haRun) {
	It("keeps ReplicationHealthy and HAReady True while a transaction stays open on the master", func() {
		r.enter(nil)
		r.mu.Lock()
		r.markers++
		table := fmt.Sprintf("open_tx_%d", r.markers)
		r.mu.Unlock()
		master, slaves, err := r.master(context.Background())
		Expect(err).NotTo(HaveOccurred())
		countWith := func(read func(context.Context, string, string) ([]string, error), pod string) (string, error) {
			values, err := read(context.Background(), pod, "SELECT TO_CHAR(COUNT(*)) AS v FROM "+table+";")
			if err == nil && len(values) != 1 {
				err = fmt.Errorf("count of %s on %s: %v", table, pod, values)
			}
			if err != nil {
				return "", err
			}
			return values[0], nil
		}
		count := func(pod string) (string, error) { return countWith(r.memberValues, pod) }
		// conditions reads both conditions, asking again on a failed read; an
		// error is returned only when three reads in a row failed.
		conditions := func() (string, error) {
			var out string
			var err error
			for range 3 {
				out, err = r.kubectl(context.Background(), "get", "cubridcluster", r.cluster, "-o",
					`jsonpath={range .status.conditions[?(@.type=="ReplicationHealthy")]}{.type}={.status}/{.reason} {end}`+
						`{range .status.conditions[?(@.type=="HAReady")]}{.type}={.status}/{.reason}{end}`)
				if err == nil {
					return out, nil
				}
				time.Sleep(time.Second)
			}
			return "", err
		}
		healthyNow := func() bool {
			c, err := conditions()
			return err == nil && strings.Contains(c, "ReplicationHealthy=True/") && strings.Contains(c, "HAReady=True/")
		}
		healthy := func(c string) bool {
			return strings.Contains(c, "ReplicationHealthy=True/") && strings.Contains(c, "HAReady=True/")
		}
		// activeFor returns how long, in seconds, the longest active csql
		// transaction on the master has been open, from cubrid tranlist. An
		// idle csql session shows a transaction time of 0 there; one whose
		// statements have run shows the time since its first change.
		activeFor := func() (float64, string, error) {
			// An HA member's databases.txt names several hosts, so tranlist
			// needs the database's host as well (<db>@localhost).
			out, err := r.kubectl(context.Background(), "exec", master, "-c", "cubrid", "--", "bash", "-c",
				`export PATH="${CUBRID}/bin:${PATH}"; cubrid tranlist "$0@localhost"`, r.database)
			if err != nil {
				return 0, out, err
			}
			longest := 0.0
			for _, m := range activeCSQL.FindAllStringSubmatch(out, -1) {
				if t, err := strconv.ParseFloat(m[1], 64); err == nil && t > longest {
					longest = t
				}
			}
			return longest, out, nil
		}

		By("creating a table with a primary key and without AUTO_INCREMENT on " + master)
		// The verdict on this statement is the count on each slave below, so it
		// runs once under the rule of sqlcheck.RunOnce and is never repeated.
		err = memberCSQL(context.Background(), r.namespace, master, r.database).
			runOnce("CREATE TABLE "+table+" (id INT PRIMARY KEY, pad VARCHAR(1000));", statementFinished)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = runSQL(csqlInPod(context.Background(), r.namespace, master, r.database), "DROP TABLE IF EXISTS "+table+";")
		})
		for _, slave := range slaves {
			Eventually(func() (string, error) { return count(slave) }, 2*time.Minute, 3*time.Second).
				Should(Equal("0"), "the table on %s", slave)
		}
		Eventually(healthyNow, 2*time.Minute, 3*time.Second).Should(BeTrue(), "the conditions before the transaction")

		By(fmt.Sprintf("holding one transaction open on %s for %s while it inserts", master,
			time.Duration(openTxRounds)*openTxPause))
		script := fmt.Sprintf(`export PATH="${CUBRID}/bin:${PATH}"
{ for i in $(seq 1 %[1]d); do
    values="$(seq -s, -f "(%%.0f,REPEAT('x',1000))" $(( (i-1)*%[2]d+1 )) $(( i*%[2]d )))"
    printf 'INSERT INTO %[3]s (id, pad) VALUES %%s;\n' "$values"
    sleep %[4]d
  done
  echo 'COMMIT;'
} | csql -u dba --no-auto-commit %[5]s@localhost`,
			openTxRounds, openTxRows, table, int(openTxPause.Seconds()), r.database)
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(openTxRounds)*openTxPause+2*time.Minute)
		defer cancel()
		masterServer := serverGuard(context.Background(), r.namespace, master, r.database).Before()
		type result struct {
			out string
			err error
		}
		done := make(chan result, 1)
		started := time.Now()
		go func() {
			out, err := r.kubectl(ctx, "exec", master, "--", "bash", "-c", script)
			done <- result{out, err}
		}()
		var samples, unhealthy, unreadable []string
		// The samples are kept also when the step fails.
		DeferCleanup(func() {
			writeScenarioFile("open-transaction/samples.txt", []byte(strings.Join(samples, "\n")+"\n"))
		})
		var session result
		midChecked := false
	wait:
		for {
			select {
			case session = <-done:
				break wait
			case <-time.After(5 * time.Second):
			}
			c, err := conditions()
			stamp := fmt.Sprintf("%s +%s", time.Now().UTC().Format(time.RFC3339), time.Since(started).Round(time.Second))
			switch {
			case err != nil:
				// An API read that failed says nothing about the conditions.
				unreadable = append(unreadable, stamp+" unreadable: "+err.Error())
				samples = append(samples, stamp+" unreadable: "+err.Error())
				continue
			case !healthy(c):
				unhealthy = append(unhealthy, stamp+" "+c)
			}
			samples = append(samples, stamp+" "+c)
			// Halfway, the transaction must be open: an active csql
			// transaction on the master that started at least a third of
			// the way back, and none of its rows on a slave. Were csql to
			// hold its input until the end, nothing would run before the
			// commit, and this would fail.
			if !midChecked && time.Since(started) > time.Duration(openTxRounds)*openTxPause/2 {
				midChecked = true
				open, tranlist, err := activeFor()
				samples = append(samples, fmt.Sprintf("longest active csql transaction on %s halfway: %.0f s\n%s",
					master, open, tranlist))
				Expect(err).NotTo(HaveOccurred(), "cubrid tranlist on %s: %s", master, tranlist)
				Expect(open).To(BeNumerically(">=", (time.Duration(openTxRounds)*openTxPause/3).Seconds()),
					"how long the csql transaction on %s has been open halfway", master)
				n, err := countWith(r.memberValuesOnce, slaves[0])
				samples = append(samples, fmt.Sprintf("rows on %s halfway: %s %v", slaves[0], n, err))
				Expect(err).NotTo(HaveOccurred())
				Expect(n).To(Equal("0"), "rows of the open transaction on %s before the commit", slaves[0])
			}
		}
		held := time.Since(started)
		Expect(midChecked).To(BeTrue(), "the transaction was not checked halfway")
		// The count on every member below decides whether the rows committed.
		Expect(heldSessionVerdict(masterServer, master, session.out, session.err)).To(Succeed(), "the csql session")
		Expect(held).To(BeNumerically(">", 90*time.Second), "how long the transaction was open")
		Expect(unhealthy).To(BeEmpty(), "samples in which ReplicationHealthy or HAReady was not True")
		Expect(len(unreadable)).To(BeNumerically("<", len(samples)/2), "samples whose conditions could not be read: %v",
			unreadable)

		By("checking that every slave holds the rows after the commit")
		want := fmt.Sprint(openTxRounds * openTxRows)
		for _, pod := range append([]string{master}, slaves...) {
			Eventually(func() (string, error) { return count(pod) }, 2*time.Minute, 3*time.Second).
				Should(Equal(want), "rows of %s on %s", table, pod)
		}
		Expect(healthyNow()).To(BeTrue(), "the conditions after the commit")
		writeScenarioFile("open-transaction/result.txt", []byte(fmt.Sprintf(
			"master: %s\ntable: %s\nopen for: %s\nrows: %s on every member\n"+
				"samples: %d, every one with ReplicationHealthy and HAReady True\n",
			master, table, held.Round(time.Second), want, len(samples))))
	})
}
