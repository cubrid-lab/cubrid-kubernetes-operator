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
	"os/exec"
	"slices"
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

// The variants of S03 in docs/testing/scenario-contract.md. One abrupt
// variant runs while the first member of the node list is the master: CUBRID
// was observed to elect that member again in most runs, when it is back
// before another one has taken over, and to promote a slave in others
// (docs/poc/RESULTS.md, POC-19). The other abrupt variant runs with
// whichever member is the master by then.
const (
	s03AbruptFirst = "abrupt-first-member"
	s03Graceful    = "graceful"
	s03Abrupt      = "abrupt"
)

// s03Variants lists the variants. The order in which they run is set where
// the steps are registered (ha_bootstrap_test.go).
var s03Variants = []string{s03AbruptFirst, s03Abrupt, s03Graceful}

// s03Clients names the workload client of each variant.
var s03Clients = map[string]string{s03AbruptFirst: "s03f", s03Graceful: "s03g", s03Abrupt: "s03a"}

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
func (r *haRun) activeMasterAmong(ctx context.Context, pods []string) string {
	for _, pod := range pods {
		out, err := r.kubectl(ctx, "exec", pod, "--", "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
		if err == nil && strings.Contains(out, "current "+pod+", state master") &&
			strings.Contains(out, "registered_and_active") {
			return pod
		}
	}
	return ""
}

// activeMasters returns the members that CUBRID reports as an active master,
// asked one after another. A member that does not answer is not counted.
func (r *haRun) activeMasters(ctx context.Context) []string {
	var masters []string
	for _, pod := range r.members {
		if r.activeMasterAmong(ctx, []string{pod}) != "" {
			masters = append(masters, pod)
		}
	}
	return masters
}

// identities returns the Pod UID and container restart count of each of the
// given members, or faults.UnknownIdentity where it could not be read.
func (r *haRun) identities(ctx context.Context, members []string) map[string]string {
	ids := make(map[string]string, len(members))
	for _, member := range members {
		out, err := r.kubectl(ctx, "get", "pod", member,
			"-o", "jsonpath={.metadata.uid}/{.status.containerStatuses[0].restartCount}")
		if err != nil || strings.HasPrefix(out, "/") || strings.HasSuffix(out, "/") {
			out = faults.UnknownIdentity
		}
		ids[member] = out
	}
	return ids
}

// statusSnapshot is the conditions, the primary and the last observation
// time of the CubridCluster as one read returned them.
type statusSnapshot struct {
	primary    string
	conditions map[string]string
	// lastObserved is status.lastObservedTime; zero when it is not set.
	lastObserved time.Time
}

func (r *haRun) status(ctx context.Context) (statusSnapshot, error) {
	out, err := r.kubectl(ctx, "get", "cubridcluster", r.cluster, "-o", "json")
	if err != nil {
		return statusSnapshot{}, err
	}
	var cluster struct {
		Status struct {
			CurrentPrimary   string     `json:"currentPrimary"`
			LastObservedTime *time.Time `json:"lastObservedTime"`
			Conditions       []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &cluster); err != nil {
		return statusSnapshot{}, err
	}
	s := statusSnapshot{primary: cluster.Status.CurrentPrimary, conditions: map[string]string{}}
	if cluster.Status.LastObservedTime != nil {
		s.lastObserved = *cluster.Status.LastObservedTime
	}
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
// read-write Service, and every fourth operation reads its last acknowledged
// write back (#294), until it is stopped. It waits until ten of its operations
// were acknowledged.
func (r *haRun) startClient(id string) {
	file := "/tmp/" + id
	_, err := r.client(context.Background(), fmt.Sprintf("CLIENT_ID=%s OPS=100000 INTERVAL_MS=100 ROLLBACK_EVERY=7"+
		" READ_EVERY=4 nohup workload run > %[2]s.jsonl 2> %[2]s.err & echo $! > %[2]s.pid", id, file))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	r.mu.Lock()
	r.running[id] = true
	r.mu.Unlock()
	EventuallyWithOffset(1, func(g Gomega) {
		out, err := r.client(context.Background(), fmt.Sprintf("grep -c '\"acknowledged\"' %s.jsonl", file))
		g.Expect(err).NotTo(HaveOccurred())
		acknowledged, err := strconv.Atoi(strings.TrimSpace(out))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(acknowledged).To(BeNumerically(">=", 10))
	}, time.Minute, time.Second).Should(Succeed(), "client %s did not get ten operations acknowledged", id)
}

// killClient stops a client started with startClient. It is safe to call
// more than once.
func (r *haRun) killClient(id string) {
	_, _ = r.client(context.Background(), fmt.Sprintf("kill $(cat /tmp/%s.pid) 2>/dev/null; true", id))
}

// lastAnswerAcknowledged reports whether the latest answer a running client
// received is an acknowledgement.
func (r *haRun) lastAnswerAcknowledged(ctx context.Context, id string) bool {
	last, err := r.client(ctx, fmt.Sprintf("grep -v '\"attempted\"' /tmp/%s.jsonl | tail -n 1", id))
	return err == nil && strings.Contains(last, `"event":"acknowledged"`)
}

// stopClient stops a client, adds its history to the run's and returns its
// events and its operations by outcome.
func (r *haRun) stopClient(id string) ([]workload.Event, workload.History) {
	text, err := r.collectClient(id)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	events, err := workload.ReadEvents(strings.NewReader(text))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	history, err := workload.ReadHistory(strings.NewReader(text))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	r.addHistory(text)
	if errors, err := r.client(context.Background(),
		fmt.Sprintf("tail -n 5 /tmp/%s.err", id)); err == nil && strings.TrimSpace(errors) != "" {
		_, _ = fmt.Fprintf(GinkgoWriter, "client %s stderr:\n%s\n", id, errors)
	}
	return events, history
}

// collectClient stops a client and returns the complete lines of what it
// recorded. The client is no longer running once they were read.
func (r *haRun) collectClient(id string) (string, error) {
	r.killClient(id)
	text, err := r.client(context.Background(), fmt.Sprintf("sleep 1; cat /tmp/%s.jsonl", id))
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	delete(r.running, id)
	r.mu.Unlock()
	// A client that is stopped while it writes a line leaves that line unfinished.
	if cut := strings.LastIndex(text, "\n"); cut >= 0 {
		text = text[:cut+1]
	}
	return text, nil
}

// roleLog records what CUBRID reports on every member, each time it changes.
// It is evidence for a run that does not end as expected.
type roleLog struct {
	lines []string
	last  string
}

// sample asks every member for its node and server state.
func (l *roleLog) sample(ctx context.Context, r *haRun) {
	states := make([]string, 0, len(r.members))
	for _, pod := range r.members {
		out, err := r.kubectl(ctx, "exec", pod, "--", "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
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

// replicationStateLimit bounds how long replicationState may take in all;
// each kubectl call it makes is stopped at that deadline, and each API
// request also after 20 s. Each CUBRID command in a member is stopped after
// 15 s (memberReplicationScript), so that a member that is gone or
// restarting cannot use up the budget of the others.
const replicationStateLimit = 2 * time.Minute

// kubectlExec is kubectl's subcommand that runs a command in a container.
const kubectlExec = "exec"

// memberReplicationScript prints, inside a member's container, what bears on
// whether the member applies the master's log (#357): heartbeat status; for
// each other member ($2...) applyinfo for the log copied from it and the
// last lines of its copylogdb and applylogdb error logs, with repeated
// messages counted; the server error log's recovery, checkpoint, HA mode and
// volume lock lines; and the database's active log lock file. $1 is the
// database. Every part is bounded.
const memberReplicationScript = `db=$1; shift
PATH="${CUBRID}/bin:${PATH}"
logs="${CUBRID}/log"
echo "== heartbeat status"; timeout 15 cubrid heartbeat status 2>&1 | head -n 40
for src in "$@"; do
  [ "$src" = "$HOSTNAME" ] && continue
  echo "== applyinfo for the log copied from $src"
  timeout 15 cubrid applyinfo -L "${CUBRID_DATABASES}/${db}_${src}" -r "$src" -a "$db" 2>&1 | head -n 60
  for f in "${logs}/${db}@${src}_copylogdb.err" "${logs}/${db}@localhost_applylogdb_${db}_${src}.err"; do
    echo "== $(basename "$f"): first and last time stamps, then the last messages with repeats counted"
    [ -f "$f" ] || { echo "(no file)"; continue; }
    grep '^Time' "$f" | sed -n '1p;$p' | cut -c1-40
    grep -v -e '^Time' -e '^$' -e '^\*\*\* The previous' "$f" | tail -n 2000 | uniq -c | tail -n 15 | cut -c1-400
  done
done
echo "== server error log: recovery, checkpoint, HA mode and volume lock lines"
cat "${logs}"/server/"${db}"_[0-9]*.err 2>/dev/null \
  | grep -B1 -e 'Log recovery' -e 'Checkpoint' -e 'HA mode is changed' -e 'Unable to mount' \
  | grep -v '^--$' | sed -E 's/^(Time: [^ ]+ [^ ]+) .*/\1/' | tail -n 60 | cut -c1-400
echo "== active log lock file"; head -c 200 "${CUBRID_DATABASES}/${db}/${db}_lgat__lock" 2>&1; echo`

// replicationState records, after every abrupt variant, what each member
// reports about the master's log and when the deleted Pod's containers and
// its replacement's ran, to S03/<variant>/replication-state.txt, which is
// added to the scenario's evidence. #357 asks whether a returning master's
// log can stop continuing what the slaves copied; Kind was seen to start a
// force-deleted StatefulSet Pod's replacement while the old container still
// ran (docs/poc/RESULTS.md, POC-28). It is recorded on success as well, so
// that the margin between the two can be compared across runs. It takes at
// most replicationStateLimit and ends with the spec's context, so it does
// not run past the suite's deadline; what could not be read in time is
// recorded with its error. A failed call records its exit error only: the
// command is known from this code and its output is recorded once. The file
// is redacted.
func (r *haRun) replicationState(spec context.Context, variant string, result *evidence.Scenario) {
	if variant == s03Graceful {
		return
	}
	ctx, cancel := context.WithTimeout(spec, replicationStateLimit)
	defer cancel()
	kubectl := func(args ...string) (string, error) {
		out, err := utils.Run(exec.CommandContext(ctx, "kubectl",
			append([]string{"--request-timeout=20s", "-n", r.namespace}, args...)...))
		// utils.Run wraps the exit error with the command and its output.
		if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
			err = wrapped.Unwrap()
		}
		return out, err
	}
	var b strings.Builder
	add := func(format string, args ...any) { _, _ = fmt.Fprintf(&b, format+"\n", args...) }
	add("taken at: %s", time.Now().UTC().Format(time.RFC3339Nano))
	// The Events of every Pod of the member names, old and new: the deleted
	// Pod's Killing and the replacement's Created and Started, by Pod UID.
	events, err := kubectl("get", "events", "--sort-by=.metadata.creationTimestamp", "-o",
		`jsonpath={range .items[?(@.involvedObject.kind=="Pod")]}`+
			`{.metadata.creationTimestamp} {.eventTime} {.involvedObject.name} {.involvedObject.uid} `+
			`{.reason} {.message}{"\n"}{end}`)
	add("\nPod Events (err %v):\n%s", err, strings.Join(lastLines(events, maxLogLines, nil, r.members...), "\n"))
	for _, member := range r.members {
		started, serr := kubectl("get", "pod", member, "-o",
			`jsonpath={.metadata.uid} {.metadata.creationTimestamp} `+
				`init {.status.initContainerStatuses[*].state.terminated.startedAt} `+
				`{.status.initContainerStatuses[*].state.terminated.finishedAt} `+
				`cubrid {.status.containerStatuses[?(@.name=="cubrid")].restartCount} `+
				`{.status.containerStatuses[?(@.name=="cubrid")].state.running.startedAt}`)
		add("\n%s: Pod uid, created, init container start and end, cubrid restarts and start: %s (err %v)",
			member, started, serr)
		out, err := kubectl(append([]string{kubectlExec, member, "-c", "cubrid", "--", "bash", "-c",
			memberReplicationScript, "replication-state", r.database}, r.members...)...)
		add("%s (err %v):\n%s", member, err, out)
	}
	if file := writeScenarioFile("S03/"+variant+"/replication-state.txt", []byte(b.String())); file != "" &&
		!slices.Contains(result.Evidence, file) {
		result.Evidence = append(result.Evidence, file)
	}
}

// s03Step registers one variant of S03 (primary Pod termination) of the
// scenario contract. The run continues on the cluster the earlier steps left,
// with the workload client writing through the read-write Service while the
// master's Pod is deleted.
func s03Step(r *haRun, variant string) {
	It("S03/"+variant+": one member is master again after the master Pod is deleted, and no acknowledged write is lost",
		Label("S03", evidence.Requirement{ID: "S03", Variant: variant}.Label()), func(ctx SpecContext) {
			var needs func(context.Context) error
			if variant == s03AbruptFirst {
				needs = r.needsFirstMemberAsMaster
			}
			r.enter(needs)
			r.s03(ctx, variant)
		})
}

func (r *haRun) s03(spec context.Context, variant string) {
	result := evidence.Scenario{ID: "S03", Variant: variant, Result: evidence.Fail,
		Reason: "a check of S03 failed; see the test output"}
	// Reported also when a check below fails.
	defer func() { recordScenario(result) }()
	// Runs before the scenario is recorded, also when a check below failed.
	defer func() { r.replicationState(spec, variant, &result) }()

	client := s03Clients[variant]

	By("recording the starting state")
	master, _, err := r.master(context.Background())
	Expect(err).NotTo(HaveOccurred())
	uid, err := r.kubectl(context.Background(), "get", "pod", master, "-o", "jsonpath={.metadata.uid}")
	Expect(err).NotTo(HaveOccurred())
	// The fault deletes the master's Pod only: the other members keep their
	// Pods and containers.
	untouched := r.identities(context.Background(),
		slices.DeleteFunc(slices.Clone(r.members), func(m string) bool { return m == master }))

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
	outcome := r.runFault(faults.Scenario{
		Fault:  podDeletion{namespace: r.namespace, pod: master, uid: uid, force: variant != s03Graceful},
		Limits: s03FlowLimits,
		Before: func(ctx context.Context) error {
			now, _, err := r.master(ctx)
			if err != nil {
				return err
			}
			if now != master {
				return fmt.Errorf("the master is %s, not %s", now, master)
			}
			// This variant is about the first member as master. The other
			// abrupt variant takes whichever member is the master: what the
			// step before it leaves is CUBRID's choice (POC-19).
			if first := r.members[0]; variant == s03AbruptFirst && master != first {
				return fmt.Errorf("this variant needs the first member %s as master, and the master is %s", first, master)
			}
			return nil
		},
		Check: func(ctx context.Context) (bool, error) {
			roles.sample(ctx, r)
			// The operator's own view, from one read of its status. It must
			// never offer the write endpoint while the primary is not
			// resolved (ADR-0005, section 6).
			if s, err := r.status(ctx); err == nil {
				resolved := s.conditions["PrimaryResolved"] == "True"
				routing := s.conditions["RoutingReady"] == "True"
				if routing && !resolved {
					return false, fmt.Errorf("RoutingReady is True while PrimaryResolved is %q",
						s.conditions["PrimaryResolved"])
				}
				unresolved.sample(!resolved)
				notRouting.sample(!routing)
			}
			// Two members that are active masters at once must never happen.
			// One sweep asks the members one after another and can see both
			// sides of a handover, so a second sweep confirms it.
			masters := r.activeMasters(ctx)
			if len(masters) > 1 {
				if err := faults.CheckOneMaster(masters, r.activeMasters(ctx)); err != nil {
					return false, err
				}
				return false, nil // a handover seen mid-sweep
			}
			// CUBRID's election: a member reports itself an active master.
			// It is another member, or the deleted one in its new Pod: the
			// fault is confirmed, so the old Pod no longer answers.
			if newMaster == "" {
				if len(masters) == 0 {
					return false, nil
				}
				newMaster = masters[0]
				electedAt = time.Now()
			}
			// The Operator noticing it.
			if noticedAt.IsZero() {
				primary, err := r.kubectl(ctx, "get", "cubridcluster", r.cluster, "-o", "jsonpath={.status.currentPrimary}")
				if err != nil || primary != newMaster {
					return false, nil
				}
				noticedAt = time.Now()
			}
			// One master and two slaves again. A master other than the
			// elected one is a second change of role, which must not happen
			// by itself.
			now, _, err := r.master(ctx)
			if err != nil {
				return false, nil
			}
			if now != newMaster {
				return false, fmt.Errorf("the master changed again, from %s to %s", newMaster, now)
			}
			if _, err := r.formationTime(ctx); err != nil {
				return false, nil // a condition of the starting state is not True yet
			}
			// The client recovered: its latest answer is an acknowledgement.
			return r.lastAnswerAcknowledged(ctx, client), nil
		},
	})
	timeline := writeTimeline("S03/"+variant+"/timeline.jsonl", outcome.Timeline)
	rolesFile := roles.write("S03/" + variant + "/roles.jsonl")
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	if outcome.Result != evidence.Pass {
		result.Result, result.Reason = outcome.Result, outcome.Reason
		// Keep what the client saw: the scenario ends here.
		if text, err := r.client(context.Background(), fmt.Sprintf("cat /tmp/%s.jsonl", client)); err == nil {
			writeScenarioFile("S03/"+variant+"/client-history.jsonl", []byte(text))
		}
	}
	Expect(outcome.Result).To(Equal(evidence.Pass), "S03/%s: %s", variant, outcome.Reason)

	By("letting the client run for the stable period, with one active master throughout, then stopping it")
	Consistently(func() error {
		masters := r.activeMasters(context.Background())
		if len(masters) < 2 {
			return nil
		}
		return faults.CheckOneMaster(masters, r.activeMasters(context.Background()))
	}, stablePeriod.Value+2*time.Second, 3*time.Second).Should(Succeed())
	events, history := r.stopClient(client)

	recoveredAt, stable, recovered := workload.Recovery(events, outcome.FaultIssuedAt)
	Expect(recovered).To(BeTrue(), "the client's last answer after the fault is not an acknowledgement")
	Expect(stable).To(BeNumerically(">=", stablePeriod.Value),
		"the client was acknowledged without interruption for %s only", stable)
	recovery := recoveredAt.Sub(outcome.FaultIssuedAt)

	By("checking that the master did not change a second time")
	now, nowSlaves, err := r.master(context.Background())
	Expect(err).NotTo(HaveOccurred())
	Expect(now).To(Equal(newMaster), "the master after the stable period")
	if newMaster != master {
		Expect(nowSlaves).To(ContainElement(master), "the former master is a slave")
	}
	primary, err := r.kubectl(context.Background(),
		"get", "cubridcluster", r.cluster, "-o", "jsonpath={.status.currentPrimary}")
	Expect(err).NotTo(HaveOccurred())
	Expect(primary).To(Equal(newMaster))

	By("checking that the members the fault did not touch kept their Pods and containers")
	// A read that fails is retried; one that keeps failing counts as changed.
	var after map[string]string
	Eventually(func() bool {
		after = r.identities(context.Background(), slices.Collect(maps.Keys(untouched)))
		return !slices.Contains(slices.Collect(maps.Values(after)), faults.UnknownIdentity)
	}, time.Minute, 3*time.Second).Should(BeTrue())
	Expect(faults.CheckUnchanged(untouched, after)).To(Succeed())

	// The Operator saw the primary become unresolved and resolved again; that
	// is in its Events and, under the same names, in its log
	// (docs/observability.md, section "Transitions").
	By("reading the change of the primary from the operator's Events and log")
	Eventually(func(g Gomega) {
		recorded, err := r.kubectl(context.Background(),
			"get", "events", "--field-selector", "involvedObject.name="+r.cluster,
			"-o", `jsonpath={range .items[*]}{.reason}{"\n"}{end}`)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(recorded).To(ContainSubstring("PrimaryUnresolved"))
		g.Expect(recorded).To(ContainSubstring("PrimaryResolved"))
		operatorLog, err := utils.Run(exec.Command("kubectl", "-n", namespace, "logs", "-l", operatorSelector, "--tail=-1"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(operatorLog).To(ContainSubstring(`"event":"primary_unresolved"`))
		g.Expect(operatorLog).To(ContainSubstring(`"event":"primary_resolved"`))
		g.Expect(operatorLog).To(ContainSubstring(`"component":"operator"`))
	}, time.Minute, 3*time.Second).Should(Succeed())

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
		if e.Event == workload.Acknowledged && e.Op == workload.OpInsert && !unresolved.from.IsZero() &&
			e.T.After(unresolved.from) &&
			(unresolved.to.IsZero() || e.T.Before(unresolved.to)) {
			acknowledgedWhileUnresolved++
		}
	}
	result = judged(evidence.Scenario{
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
	}, within(failoverLimit, recovery), stableFor(stable),
		within(rejoinLimit, outcome.OutcomeAt.Sub(outcome.FaultIssuedAt)))
}
