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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
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

	// mu guards history, running and markers: the gate's checks run in
	// a goroutine of their own.
	mu sync.Mutex
	// history is everything the workload client recorded so far.
	history string
	// running holds the clients started and not stopped yet.
	running map[string]bool
	// markers counts the marker writes of the gate's checks.
	markers int

	// gate restores and verifies the starting state before each step after
	// S01; entries are its decisions.
	gate    *faults.Gate
	entries []faults.Entry
}

func newHARun(namespace, clientNamespace, cluster, database string, members []string) *haRun {
	r := &haRun{
		namespace: namespace, clientNamespace: clientNamespace, cluster: cluster, database: database, members: members,
		running: map[string]bool{},
	}
	r.gate = r.newGate()
	return r
}

// reportNotRun records every scenario of the group as not_run with the
// reason, for a run in which the group's setup was skipped.
func (r *haRun) reportNotRun(reason string) {
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
	for _, variant := range s14TokenVariants {
		recordScenario(evidence.Scenario{ID: "S14", Variant: variant, Result: evidence.NotRun, Reason: reason})
	}
}

func (r *haRun) kubectl(ctx context.Context, args ...string) (string, error) {
	return utils.Run(exec.CommandContext(ctx, "kubectl", append([]string{"-n", r.namespace}, args...)...))
}

// client runs a command in the workload client Pod.
func (r *haRun) client(ctx context.Context, script string) (string, error) {
	return utils.Run(exec.CommandContext(ctx, "kubectl", "-n", r.clientNamespace, "exec", "workload-client", "--",
		"sh", "-c", script))
}

// runWorkload runs client operations and adds their history to the run's.
func (r *haRun) runWorkload(ctx context.Context, settings string) (workload.History, error) {
	out, err := r.client(ctx, settings+" workload run")
	if err != nil {
		return workload.History{}, err
	}
	h, err := workload.ReadHistory(strings.NewReader(out))
	if err != nil {
		return workload.History{}, fmt.Errorf("history: %w\n%s", err, out)
	}
	r.addHistory(out)
	return h, nil
}

// addHistory adds what a client recorded to the run's history.
func (r *haRun) addHistory(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history += text
}

// historyText returns everything the clients recorded so far.
func (r *haRun) historyText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.history
}

// master asks CUBRID on every member for its role and returns the one master
// and the slaves. Any other distribution of roles is an error.
func (r *haRun) master(ctx context.Context) (string, []string, error) {
	var masters, slaves []string
	for _, pod := range r.members {
		out, err := r.kubectl(ctx, "exec", pod, "--", "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
		if err != nil {
			return "", nil, err
		}
		switch {
		case strings.Contains(out, "current "+pod+", state master") && strings.Contains(out, "registered_and_active"):
			masters = append(masters, pod)
		case strings.Contains(out, "current "+pod+", state slave") && strings.Contains(out, "registered_and_standby"):
			slaves = append(slaves, pod)
		default:
			return "", nil, &faults.Unmet{Member: pod, Kind: faults.UnmetRole,
				Detail: "neither an active master nor a standby slave:\n" + out}
		}
	}
	if len(masters) != 1 || len(slaves) != len(r.members)-1 {
		return "", nil, &faults.Unmet{Kind: faults.UnmetRole, Detail: fmt.Sprintf("masters %v, slaves %v", masters, slaves)}
	}
	return masters[0], slaves, nil
}

// quotedValue matches one string value in csql's result table.
var quotedValue = regexp.MustCompile(`^\s*'(.*)'\s*$`)

// memberValues runs a query that returns one string column on a member, with
// csql inside its Pod, and returns the values.
func (r *haRun) memberValues(ctx context.Context, pod, query string) ([]string, error) {
	out, err := runSQL(csqlInPod(ctx, r.namespace, pod, r.database), query)
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
func (r *haRun) memberRows(ctx context.Context, pod string) ([]workload.Row, error) {
	values, err := r.memberValues(ctx, pod, `SELECT '{"opId":"' || op_id || '","client":"' || client_id ||`+
		` '","seq":' || CAST(seq AS VARCHAR) || ',"amount":' || CAST(amount AS VARCHAR) ||`+
		` ',"note":"' || note || '"}' AS v FROM ledger ORDER BY client_id, seq;`)
	if err != nil {
		return nil, err
	}
	return workload.ReadRows(strings.NewReader(strings.Join(values, "\n")))
}

// catalog lists the columns of the workload's tables on a member.
func (r *haRun) catalog(ctx context.Context, pod string) (string, error) {
	values, err := r.memberValues(ctx, pod, `SELECT class_name || '.' || attr_name || ':' || data_type AS v`+
		` FROM db_attribute WHERE class_name IN ('ledger', 'marker', 's02_added', 's02_marker') ORDER BY 1;`)
	return strings.Join(values, "\n"), err
}

// dataCheck applies the data rules to every member and to both Services.
func (r *haRun) dataCheck(ctx context.Context) (workload.Report, error) {
	history, err := workload.ReadHistory(strings.NewReader(r.historyText()))
	if err != nil {
		return workload.Report{}, err
	}
	rows := map[string][]workload.Row{}
	for _, pod := range r.members {
		if rows[pod], err = r.memberRows(ctx, pod); err != nil {
			return workload.Report{}, fmt.Errorf("%s: %w", pod, err)
		}
	}
	for name, dump := range map[string]string{"rw": "workload dump", "ro": `JDBC_URL="$RO_JDBC_URL" workload dump`} {
		out, err := r.client(ctx, dump)
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
func (r *haRun) failCount(ctx context.Context, slave, master string) (int, error) {
	out, err := r.kubectl(ctx, "exec", slave, "--", "bash", "-c",
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
func (r *haRun) formationTime(ctx context.Context) (time.Duration, error) {
	out, err := r.kubectl(ctx, "get", "cubridcluster", r.cluster, "-o", "json")
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
				return 0, &faults.Unmet{Kind: faults.UnmetCondition, Detail: fmt.Sprintf("%s is %s", want, c.Status)}
			}
			found = true
			if c.LastTransitionTime.After(last) {
				last = c.LastTransitionTime
			}
		}
		if !found {
			return 0, &faults.Unmet{Kind: faults.UnmetCondition, Detail: want + " is not reported"}
		}
	}
	return last.Sub(cluster.Metadata.CreationTimestamp), nil
}

// evidenceFiles stores the history and a data check of a scenario and
// returns the files that were written.
func (r *haRun) evidenceFiles(id string, report workload.Report) []string {
	var files []string
	if f := writeScenarioFile(id+"/history.jsonl", []byte(r.historyText())); f != "" {
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
	It("S01: has one master and two slaves, and accepts writes only through the read-write Service", Label("S01"), func() {
		result := evidence.Scenario{ID: "S01", Result: evidence.Fail, Reason: "a check of S01 failed; see the test output"}
		// Recorded also when a check below fails.
		defer func() { recordScenario(result) }()

		By("asking CUBRID for the roles and comparing them with the operator's status")
		master, _, err := r.master(context.Background())
		Expect(err).NotTo(HaveOccurred())
		primary, err := r.kubectl(context.Background(), "get", "cubridcluster", r.cluster, "-o", "jsonpath={.status.currentPrimary}")
		Expect(err).NotTo(HaveOccurred())
		Expect(primary).To(Equal(master), "status.currentPrimary is not the member CUBRID reports as master")
		formation, err := r.formationTime(context.Background())
		Expect(err).NotTo(HaveOccurred())

		By("attempting a write through the read-only Service")
		refused, err := r.runWorkload(context.Background(), `JDBC_URL="$RO_JDBC_URL" ENDPOINT=ro CLIENT_ID=ro OPS=1 ROLLBACK_EVERY=0`)
		Expect(err).NotTo(HaveOccurred())
		// Refused by the read-only Broker itself; a Broker that could not be
		// reached is not a refusal.
		Expect(workload.CheckReadOnlyRefusal(refused)).To(Succeed())

		By("applying the data rules to every member and to both Services")
		var report workload.Report
		Eventually(func(g Gomega) {
			var err error
			report, err = r.dataCheck(context.Background())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
		}, 2*time.Minute, 3*time.Second).Should(Succeed())
		Expect(report.Members).To(HaveLen(len(r.members) + 2))

		history, err := workload.ReadHistory(strings.NewReader(r.historyText()))
		Expect(err).NotTo(HaveOccurred())
		result = judged(evidence.Scenario{
			ID:           "S01",
			Measurements: map[string]any{"formationTime": formation.String(), "acknowledgedMissing": report.AcknowledgedMissing},
			Operations: &evidence.Operations{
				Attempted: history.Counts[workload.Attempted], Acknowledged: history.Counts[workload.Acknowledged],
				Failed: history.Counts[workload.Failed], Unknown: history.Counts[workload.Unknown],
			},
			Evidence: r.evidenceFiles("S01", report),
		}, within(formationLimit, formation))
	})

	It("S02: replicates rows, schema changes and rollbacks, also to a replaced slave", Label("S02"), func() {
		r.enter(nil)
		result := evidence.Scenario{ID: "S02", Result: evidence.Fail, Reason: "a check of S02 failed; see the test output"}
		// Recorded also when a check below fails.
		defer func() { recordScenario(result) }()

		master, slaves, err := r.master(context.Background())
		Expect(err).NotTo(HaveOccurred())

		By("changing the schema on the master")
		_, err = runSQL(csqlInPod(context.Background(), r.namespace, master, r.database),
			"ALTER TABLE ledger ADD COLUMN extra VARCHAR(16);"+
				" CREATE TABLE s02_added (id INT PRIMARY KEY, v VARCHAR(16));"+
				" INSERT INTO s02_added VALUES (1, 'added');"+
				" CREATE TABLE s02_marker (id INT PRIMARY KEY, v VARCHAR(40)); COMMIT;")
		Expect(err).NotTo(HaveOccurred())

		By("running inserts and rolled-back transactions through the read-write Service")
		run, err := r.runWorkload(context.Background(), "CLIENT_ID=c2 OPS=40")
		Expect(err).NotTo(HaveOccurred())
		Expect(run.Counts[workload.Acknowledged]).To(Equal(40), "with no fault every operation is acknowledged")

		// replication_limit is timed on one row, committed after everything
		// above, until each slave returns it and holds as many ledger rows as
		// the master. The clock starts before the commit is sent, so the time
		// is an upper bound; the full check below is not part of it.
		By("committing a marker row on the master and timing until every slave returns it with every row")
		ledgerRows := "SELECT CAST(COUNT(*) AS VARCHAR(20)) AS n FROM ledger;"
		wantRows, err := r.memberValues(context.Background(), master, ledgerRows)
		Expect(err).NotTo(HaveOccurred())
		Expect(wantRows).To(HaveLen(1), "the ledger row count on the master")
		marker := fmt.Sprintf("marker-%d", time.Now().UnixNano())
		committed := time.Now()
		_, err = runSQL(csqlInPod(context.Background(), r.namespace, master, r.database),
			"INSERT INTO s02_marker VALUES (1, '"+marker+"'); COMMIT;")
		Expect(err).NotTo(HaveOccurred())
		visibleCtx, cancelVisible := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancelVisible()
		seen, err := workload.WaitVisible(visibleCtx, committed, slaves, 200*time.Millisecond,
			func(_ context.Context, slave string) (bool, error) {
				values, err := r.memberValues(context.Background(), slave, "SELECT v FROM s02_marker WHERE id = 1;")
				if err != nil || !slices.Equal(values, []string{marker}) {
					return false, err
				}
				rows, err := r.memberValues(context.Background(), slave, ledgerRows)
				return slices.Equal(rows, wantRows), err
			})
		Expect(err).NotTo(HaveOccurred())
		_, replication := workload.Slowest(seen)

		By("waiting until every member holds the same rows and the same schema")
		var report workload.Report
		converged := func(g Gomega) {
			var err error
			report, err = r.dataCheck(context.Background())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
			want, err := r.catalog(context.Background(), master)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(want).To(ContainSubstring("ledger.extra:"))
			g.Expect(want).To(ContainSubstring("s02_added.v:"))
			for _, slave := range slaves {
				got, err := r.catalog(context.Background(), slave)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).To(Equal(want), "catalog on %s", slave)
				added, err := r.memberValues(context.Background(), slave, "SELECT v FROM s02_added;")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(added).To(Equal([]string{"added"}), "the new table's row on %s", slave)
			}
		}
		Eventually(converged, 2*time.Minute, time.Second).Should(Succeed())
		// Recorded, not judged: it includes the time the checks take.
		fullCheck := time.Since(committed)

		By("reading the fail count of each slave's applier")
		for _, slave := range slaves {
			count, err := r.failCount(context.Background(), slave, master)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(BeZero(), "fail count of the applier on %s", slave)
		}

		By("checking what the operator reports about each slave's applier")
		Eventually(func(g Gomega) {
			healthy, err := r.kubectl(context.Background(), "get", "cubridcluster", r.cluster,
				"-o", `jsonpath={.status.conditions[?(@.type=="ReplicationHealthy")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(healthy).To(Equal("True"), "the ReplicationHealthy condition")
			for _, slave := range slaves {
				source, err := r.kubectl(context.Background(), "get", "cubridcluster", r.cluster,
					"-o", `jsonpath={.status.instances[?(@.name=="`+slave+`")].replication.source}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(source).To(Equal(master), "the log %s applies, as the operator's status reports it", slave)
				stalled, err := r.kubectl(context.Background(), "get", "cubridcluster", r.cluster,
					"-o", `jsonpath={.status.instances[?(@.name=="`+slave+`")].replication.stalledSince}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(stalled).To(BeEmpty(), "%s is reported as stalled", slave)
			}
		}, time.Minute, 3*time.Second).Should(Succeed())

		By("replacing a slave Pod and checking that it holds everything again")
		replaced := slaves[0]
		uid, err := r.kubectl(context.Background(), "get", "pod", replaced, "-o", "jsonpath={.metadata.uid}")
		Expect(err).NotTo(HaveOccurred())
		outcome := r.runFault(faults.Scenario{
			Fault:  podDeletion{namespace: r.namespace, pod: replaced, uid: uid},
			Limits: podDeletionLimits,
			Before: func(ctx context.Context) error { _, _, err := r.master(ctx); return err },
			// The same master, the replaced member a standby slave again, and
			// every data rule holding on it.
			Check: func(ctx context.Context) (bool, error) {
				now, _, err := r.master(ctx)
				if err != nil {
					return false, nil
				}
				if now != master {
					return false, fmt.Errorf("the master changed from %s to %s when a slave was replaced", master, now)
				}
				check, err := r.dataCheck(ctx)
				return err == nil && check.OK, nil
			},
		})
		timeline := writeTimeline("S02/timeline.jsonl", outcome.Timeline)
		Expect(outcome.Result).To(Equal(evidence.Pass), "replacing %s: %s", replaced, outcome.Reason)

		By("writing again and checking the replaced slave with the others")
		run, err = r.runWorkload(context.Background(), "CLIENT_ID=c3 OPS=20")
		Expect(err).NotTo(HaveOccurred())
		Expect(run.Counts[workload.Acknowledged]).To(Equal(20))
		Eventually(converged, 2*time.Minute, time.Second).Should(Succeed())
		count, err := r.failCount(context.Background(), replaced, master)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeZero(), "fail count of the applier on the replaced slave %s", replaced)

		history, err := workload.ReadHistory(strings.NewReader(r.historyText()))
		Expect(err).NotTo(HaveOccurred())
		files := r.evidenceFiles("S02", report)
		if timeline != "" {
			files = append(files, timeline)
		}
		result = judged(evidence.Scenario{
			ID: "S02",
			Measurements: map[string]any{
				"replicationObservedWithin": replication.Round(time.Millisecond).String(),
				"fullCheckPassedWithin":     fullCheck.Round(time.Millisecond).String(),
				"slaveReplacementTime":      outcome.OutcomeAt.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String(),
				"acknowledgedMissing":       report.AcknowledgedMissing,
			},
			Operations: &evidence.Operations{
				Attempted: history.Counts[workload.Attempted], Acknowledged: history.Counts[workload.Acknowledged],
				Failed: history.Counts[workload.Failed], Unknown: history.Counts[workload.Unknown],
			},
			Evidence: files,
		}, within(replicationLimit, replication))
	})
}
