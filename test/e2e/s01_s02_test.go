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
	"os/exec"
	"regexp"
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

// The time limits of S01 and S02 for Kind on a GitHub-hosted runner. The
// values and the baseline they come from are recorded in
// docs/testing/scenario-contract.md, section "Time limits"; change them there
// and here together. Zero means "not set": the scenario then runs as a
// baseline and is reported as blocked with reason time_limit_unset, never as
// a pass.
const (
	formationLimit   = 5 * time.Minute
	replicationLimit = 30 * time.Second
)

// conditionsOfS01 are the conditions the common starting state requires.
var conditionsOfS01 = []string{"Ready", "HAReady", "PrimaryResolved", "RoutingReady"}

// haRun is the state the S01 and S02 steps share with the HA bootstrap steps
// they follow: the same cluster, the same workload client and one history.
type haRun struct {
	namespace       string
	clientNamespace string
	cluster         string
	database        string
	members         []string

	// history is everything the workload client recorded so far.
	history string
	// s01 and s02 are reported after the steps; until a step sets them they
	// say that the scenario did not get that far.
	s01, s02 evidence.Scenario
	// later holds the results of the scenarios that follow, in their order.
	later []evidence.Scenario
}

func newHARun(namespace, clientNamespace, cluster, database string, members []string) *haRun {
	unreached := "an earlier step failed; see the test output"
	return &haRun{
		namespace: namespace, clientNamespace: clientNamespace, cluster: cluster, database: database, members: members,
		s01: evidence.Scenario{ID: "S01", Result: evidence.Fail, Reason: unreached},
		s02: evidence.Scenario{ID: "S02", Result: evidence.Fail, Reason: unreached},
	}
}

// report adds S01 and S02 to the run's summary.
func (r *haRun) report(ran bool) {
	if !ran {
		reason := "needs linux/amd64: the official CUBRID image has no other build"
		recordScenario(evidence.Scenario{ID: "S01", Result: evidence.NotRun, Reason: reason})
		recordScenario(evidence.Scenario{ID: "S02", Result: evidence.NotRun, Reason: reason})
		recordScenario(evidence.Scenario{ID: "S14", Result: evidence.NotRun, Reason: reason})
		for _, variant := range s03Variants {
			recordScenario(evidence.Scenario{ID: "S03", Variant: variant, Result: evidence.NotRun, Reason: reason})
		}
		for _, variant := range s05Variants {
			recordScenario(evidence.Scenario{ID: "S05", Variant: variant, Result: evidence.NotRun, Reason: reason})
		}
		for _, variant := range s06Variants {
			recordScenario(evidence.Scenario{ID: "S06", Variant: variant, Result: evidence.NotRun, Reason: reason})
		}
		return
	}
	recordScenario(r.s01)
	recordScenario(r.s02)
	for _, s := range r.later {
		recordScenario(s)
	}
}

func (r *haRun) kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", append([]string{"-n", r.namespace}, args...)...))
}

// client runs a command in the workload client Pod.
func (r *haRun) client(script string) (string, error) {
	return utils.Run(exec.Command("kubectl", "-n", r.clientNamespace, "exec", "workload-client", "--",
		"sh", "-c", script))
}

// runWorkload runs client operations and adds their history to the run's.
func (r *haRun) runWorkload(settings string) (workload.History, error) {
	out, err := r.client(settings + " workload run")
	if err != nil {
		return workload.History{}, err
	}
	h, err := workload.ReadHistory(strings.NewReader(out))
	if err != nil {
		return workload.History{}, fmt.Errorf("history: %w\n%s", err, out)
	}
	r.history += out
	return h, nil
}

// master asks CUBRID on every member for its role and returns the one master
// and the slaves. Any other distribution of roles is an error.
func (r *haRun) master() (string, []string, error) {
	var masters, slaves []string
	for _, pod := range r.members {
		out, err := r.kubectl("exec", pod, "--", "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
		if err != nil {
			return "", nil, err
		}
		switch {
		case strings.Contains(out, "current "+pod+", state master") && strings.Contains(out, "registered_and_active"):
			masters = append(masters, pod)
		case strings.Contains(out, "current "+pod+", state slave") && strings.Contains(out, "registered_and_standby"):
			slaves = append(slaves, pod)
		default:
			return "", nil, fmt.Errorf("%s is neither an active master nor a standby slave:\n%s", pod, out)
		}
	}
	if len(masters) != 1 || len(slaves) != len(r.members)-1 {
		return "", nil, fmt.Errorf("masters %v, slaves %v", masters, slaves)
	}
	return masters[0], slaves, nil
}

// quotedValue matches one string value in csql's result table.
var quotedValue = regexp.MustCompile(`^\s*'(.*)'\s*$`)

// memberValues runs a query that returns one string column on a member, with
// csql inside its Pod, and returns the values.
func (r *haRun) memberValues(pod, query string) ([]string, error) {
	out, err := runSQL(csqlInPod(r.namespace, pod, r.database), query)
	if err != nil {
		return nil, err
	}
	// The values follow the line of "=" under the column title. The title
	// itself is the text of the expression and may be quoted too.
	_, table, found := strings.Cut(out, "\n==")
	if !found {
		if strings.Contains(out, "There are no results.") {
			return nil, nil
		}
		return nil, fmt.Errorf("no result table in the output of csql:\n%s", out)
	}
	var values []string
	for _, line := range strings.Split(table, "\n") {
		if m := quotedValue.FindStringSubmatch(line); m != nil {
			values = append(values, m[1])
		}
	}
	return values, nil
}

// memberRows reads the ledger rows a member holds, not through a Broker.
func (r *haRun) memberRows(pod string) ([]workload.Row, error) {
	values, err := r.memberValues(pod, `SELECT '{"opId":"' || op_id || '","client":"' || client_id ||`+
		` '","seq":' || CAST(seq AS VARCHAR) || ',"amount":' || CAST(amount AS VARCHAR) ||`+
		` ',"note":"' || note || '"}' AS v FROM ledger ORDER BY client_id, seq;`)
	if err != nil {
		return nil, err
	}
	return workload.ReadRows(strings.NewReader(strings.Join(values, "\n")))
}

// catalog lists the columns of the workload's tables on a member.
func (r *haRun) catalog(pod string) (string, error) {
	values, err := r.memberValues(pod, `SELECT class_name || '.' || attr_name || ':' || data_type AS v`+
		` FROM db_attribute WHERE class_name IN ('ledger', 'marker', 's02_added') ORDER BY 1;`)
	return strings.Join(values, "\n"), err
}

// dataCheck applies the data rules to every member and to both Services.
func (r *haRun) dataCheck() (workload.Report, error) {
	history, err := workload.ReadHistory(strings.NewReader(r.history))
	if err != nil {
		return workload.Report{}, err
	}
	rows := map[string][]workload.Row{}
	for _, pod := range r.members {
		if rows[pod], err = r.memberRows(pod); err != nil {
			return workload.Report{}, fmt.Errorf("%s: %w", pod, err)
		}
	}
	for name, dump := range map[string]string{"rw": "workload dump", "ro": `JDBC_URL="$RO_JDBC_URL" workload dump`} {
		out, err := r.client(dump)
		if err != nil {
			return workload.Report{}, fmt.Errorf("-%s: %w", name, err)
		}
		if rows[name], err = workload.ReadRows(strings.NewReader(out)); err != nil {
			return workload.Report{}, fmt.Errorf("-%s: %w\n%s", name, err, out)
		}
	}
	return workload.Check(history, rows), nil
}

// failCount is the fail count of a slave's applier for the master's log.
func (r *haRun) failCount(slave, master string) (int, error) {
	out, err := r.kubectl("exec", slave, "--", "bash", "-c",
		`PATH="${CUBRID}/bin:${PATH}" cubrid applyinfo -L "${CUBRID_DATABASES}/$0_$1" -r "$1" -a "$0"`,
		r.database, master)
	if err != nil {
		return 0, err
	}
	m := regexp.MustCompile(`Fail count\s*:\s*(\d+)`).FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no fail count in the output of applyinfo:\n%s", out)
	}
	return strconv.Atoi(m[1])
}

// formationTime is the time from the creation of the CubridCluster until the
// last of the conditions of the starting state became True, as the API server
// recorded both.
func (r *haRun) formationTime() (time.Duration, error) {
	out, err := r.kubectl("get", "cubridcluster", r.cluster, "-o", "json")
	if err != nil {
		return 0, err
	}
	var cluster struct {
		Metadata struct {
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type               string    `json:"type"`
				Status             string    `json:"status"`
				LastTransitionTime time.Time `json:"lastTransitionTime"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &cluster); err != nil {
		return 0, err
	}
	var last time.Time
	for _, want := range conditionsOfS01 {
		found := false
		for _, c := range cluster.Status.Conditions {
			if c.Type != want {
				continue
			}
			if c.Status != "True" {
				return 0, fmt.Errorf("condition %s is %s", want, c.Status)
			}
			found = true
			if c.LastTransitionTime.After(last) {
				last = c.LastTransitionTime
			}
		}
		if !found {
			return 0, fmt.Errorf("condition %s is not reported", want)
		}
	}
	return last.Sub(cluster.Metadata.CreationTimestamp), nil
}

// judged gives a scenario whose checks all held its result: a pass within
// the limit, a fail beyond it, and blocked when the limit is not set.
func judged(s evidence.Scenario, limitName string, limit, measured time.Duration) evidence.Scenario {
	s.Limits = map[string]string{limitName: "unset"}
	switch {
	case limit <= 0:
		s.Result, s.Reason = evidence.Blocked, faults.ReasonTimeLimitUnset
	case measured > limit:
		s.Limits[limitName] = limit.String()
		s.Result, s.Reason = evidence.Fail, fmt.Sprintf("%s exceeded: %s > %s", limitName, measured, limit)
	default:
		s.Limits[limitName] = limit.String()
		s.Result, s.Reason = evidence.Pass, ""
	}
	return s
}

// evidenceFiles stores the history and a data check of a scenario and
// returns the files that were written.
func (r *haRun) evidenceFiles(id string, report workload.Report) []string {
	var files []string
	if f := writeScenarioFile(id+"/history.jsonl", []byte(r.history)); f != "" {
		files = append(files, f)
	}
	if data, err := json.MarshalIndent(report, "", "  "); err == nil {
		if f := writeScenarioFile(id+"/data-check.json", append(data, '\n')); f != "" {
			files = append(files, f)
		}
	}
	return files
}

// s01AndS02Steps registers S01 (three-member HA with read-write and
// read-only access) and S02 (normal replication) of the scenario contract.
// They continue on the cluster the HA bootstrap steps formed, after the
// workload client has run through the read-write Service.
func s01AndS02Steps(r *haRun) {
	It("S01: has one master and two slaves, and accepts writes only through the read-write Service", func() {
		r.s01.Reason = "a check of S01 failed; see the test output"

		By("asking CUBRID for the roles and comparing them with the operator's status")
		master, _, err := r.master()
		Expect(err).NotTo(HaveOccurred())
		primary, err := r.kubectl("get", "cubridcluster", r.cluster, "-o", "jsonpath={.status.currentPrimary}")
		Expect(err).NotTo(HaveOccurred())
		Expect(primary).To(Equal(master), "status.currentPrimary is not the member CUBRID reports as master")
		formation, err := r.formationTime()
		Expect(err).NotTo(HaveOccurred())

		By("attempting a write through the read-only Service")
		refused, err := r.runWorkload(`JDBC_URL="$RO_JDBC_URL" ENDPOINT=ro CLIENT_ID=ro OPS=1 ROLLBACK_EVERY=0`)
		Expect(err).NotTo(HaveOccurred())
		Expect(refused.Counts[workload.Failed]).To(Equal(1),
			"a write through the read-only Service must be answered with an error: %v", refused.Counts)

		By("applying the data rules to every member and to both Services")
		var report workload.Report
		Eventually(func(g Gomega) {
			var err error
			report, err = r.dataCheck()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
		}, 2*time.Minute, 3*time.Second).Should(Succeed())
		Expect(report.Members).To(HaveLen(len(r.members) + 2))

		history, err := workload.ReadHistory(strings.NewReader(r.history))
		Expect(err).NotTo(HaveOccurred())
		r.s01 = judged(evidence.Scenario{
			ID:           "S01",
			Measurements: map[string]any{"formationTime": formation.String(), "acknowledgedMissing": report.AcknowledgedMissing},
			Operations: &evidence.Operations{
				Attempted: history.Counts[workload.Attempted], Acknowledged: history.Counts[workload.Acknowledged],
				Failed: history.Counts[workload.Failed], Unknown: history.Counts[workload.Unknown],
			},
			Evidence: r.evidenceFiles("S01", report),
		}, "formation_limit", formationLimit, formation)
	})

	It("S02: replicates rows, schema changes and rollbacks, also to a replaced slave", func() {
		r.s02.Reason = "a check of S02 failed; see the test output"
		master, slaves, err := r.master()
		Expect(err).NotTo(HaveOccurred())

		By("changing the schema on the master")
		_, err = runSQL(csqlInPod(r.namespace, master, r.database),
			"ALTER TABLE ledger ADD COLUMN extra VARCHAR(16);"+
				" CREATE TABLE s02_added (id INT PRIMARY KEY, v VARCHAR(16));"+
				" INSERT INTO s02_added VALUES (1, 'added'); COMMIT;")
		Expect(err).NotTo(HaveOccurred())

		By("running inserts and rolled-back transactions through the read-write Service")
		run, err := r.runWorkload("CLIENT_ID=c2 OPS=40")
		Expect(err).NotTo(HaveOccurred())
		Expect(run.Counts[workload.Acknowledged]).To(Equal(40), "with no fault every operation is acknowledged")
		committed := time.Now()

		By("waiting until every member holds the same rows and the same schema")
		var report workload.Report
		converged := func(g Gomega) {
			var err error
			report, err = r.dataCheck()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
			want, err := r.catalog(master)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(want).To(ContainSubstring("ledger.extra:"))
			g.Expect(want).To(ContainSubstring("s02_added.v:"))
			for _, slave := range slaves {
				got, err := r.catalog(slave)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).To(Equal(want), "catalog on %s", slave)
				added, err := r.memberValues(slave, "SELECT v FROM s02_added;")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(added).To(Equal([]string{"added"}), "the new table's row on %s", slave)
			}
		}
		Eventually(converged, 2*time.Minute, time.Second).Should(Succeed())
		// An upper bound: it includes the time the checks themselves take.
		replication := time.Since(committed)

		By("reading the fail count of each slave's applier")
		for _, slave := range slaves {
			count, err := r.failCount(slave, master)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(BeZero(), "fail count of the applier on %s", slave)
		}

		By("checking what the operator reports about each slave's applier")
		Eventually(func(g Gomega) {
			healthy, err := r.kubectl("get", "cubridcluster", r.cluster,
				"-o", `jsonpath={.status.conditions[?(@.type=="ReplicationHealthy")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(healthy).To(Equal("True"), "the ReplicationHealthy condition")
			for _, slave := range slaves {
				source, err := r.kubectl("get", "cubridcluster", r.cluster,
					"-o", `jsonpath={.status.instances[?(@.name=="`+slave+`")].replication.source}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(source).To(Equal(master), "the log %s applies, as the operator's status reports it", slave)
				stalled, err := r.kubectl("get", "cubridcluster", r.cluster,
					"-o", `jsonpath={.status.instances[?(@.name=="`+slave+`")].replication.stalledSince}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(stalled).To(BeEmpty(), "%s is reported as stalled", slave)
			}
		}, time.Minute, 3*time.Second).Should(Succeed())

		By("replacing a slave Pod and checking that it holds everything again")
		replaced := slaves[0]
		uid, err := r.kubectl("get", "pod", replaced, "-o", "jsonpath={.metadata.uid}")
		Expect(err).NotTo(HaveOccurred())
		outcome := faults.Run(context.Background(), faults.Scenario{
			Fault:  podDeletion{namespace: r.namespace, pod: replaced, uid: uid},
			Limits: podDeletionLimits,
			Before: func(context.Context) error { _, _, err := r.master(); return err },
			// The same master, the replaced member a standby slave again, and
			// every data rule holding on it.
			Check: func(context.Context) (bool, error) {
				now, _, err := r.master()
				if err != nil {
					return false, nil
				}
				if now != master {
					return false, fmt.Errorf("the master changed from %s to %s when a slave was replaced", master, now)
				}
				check, err := r.dataCheck()
				return err == nil && check.OK, nil
			},
		})
		timeline := writeTimeline("S02/timeline.jsonl", outcome.Timeline)
		Expect(outcome.Result).To(Equal(evidence.Pass), "replacing %s: %s", replaced, outcome.Reason)

		By("writing again and checking the replaced slave with the others")
		run, err = r.runWorkload("CLIENT_ID=c3 OPS=20")
		Expect(err).NotTo(HaveOccurred())
		Expect(run.Counts[workload.Acknowledged]).To(Equal(20))
		Eventually(converged, 2*time.Minute, time.Second).Should(Succeed())
		count, err := r.failCount(replaced, master)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeZero(), "fail count of the applier on the replaced slave %s", replaced)

		history, err := workload.ReadHistory(strings.NewReader(r.history))
		Expect(err).NotTo(HaveOccurred())
		files := r.evidenceFiles("S02", report)
		if timeline != "" {
			files = append(files, timeline)
		}
		r.s02 = judged(evidence.Scenario{
			ID: "S02",
			Measurements: map[string]any{
				"replicationObservedWithin": replication.Round(time.Millisecond).String(),
				"slaveReplacementTime":      outcome.OutcomeAt.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String(),
				"acknowledgedMissing":       report.AcknowledgedMissing,
			},
			Operations: &evidence.Operations{
				Attempted: history.Counts[workload.Attempted], Acknowledged: history.Counts[workload.Acknowledged],
				Failed: history.Counts[workload.Failed], Unknown: history.Counts[workload.Unknown],
			},
			Evidence: files,
		}, "replication_limit", replicationLimit, replication)
	})
}
