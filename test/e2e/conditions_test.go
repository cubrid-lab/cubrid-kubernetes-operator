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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// reasonConditionDisagreement is the reason of a scenario in which the
// Conditions disagreed with SQL, CUBRID's heartbeat or the Broker Deployment.
const reasonConditionDisagreement = "condition_disagreement"

// reasonConditionsNotJudged is the reason of a blocked scenario in which a
// rule it requires could not be judged, because its evidence is missing.
const reasonConditionsNotJudged = "condition_agreement_not_judged"

// conditionSampleEvery is how often the CubridCluster's status is read while
// a fault runs.
const conditionSampleEvery = 2 * time.Second

// conditionReadLimit bounds one round of reads.
const conditionReadLimit = 20 * time.Second

// rowCountLimit bounds one count of the ledger rows on one member.
const rowCountLimit = 8 * time.Second

// conditionRound is one line of conditions.jsonl: one read of the status, the
// read-write Broker Deployment's available Pods, and, when asked for, the
// role CUBRID's heartbeat reports on each member and the count of the ledger
// rows on some members.
type conditionRound struct {
	Status *faults.ConditionSample `json:"status,omitempty"`
	// StatusError says why the status could not be read.
	StatusError string            `json:"statusError,omitempty"`
	Roles       *faults.RoleSweep `json:"roles,omitempty"`
	Rows        *faults.RowCounts `json:"rows,omitempty"`
}

// conditionSampling says what a round reads besides the status and the
// read-write Broker Deployment.
type conditionSampling struct {
	// roles asks every member for its role with cubrid heartbeat status.
	roles bool
	// rows are the members whose ledger rows are counted, each on the member
	// itself.
	rows []string
}

// conditionSampler reads the status in the background until it is stopped.
type conditionSampler struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	// rounds is written by the sampler's goroutine only, and read after it
	// has ended.
	rounds []conditionRound
}

// sampleConditions starts reading the status every conditionSampleEvery, and
// in each round what the sampling asks for besides.
func (r *haRun) sampleConditions(sampling conditionSampling) *conditionSampler {
	ctx, cancel := context.WithCancel(context.Background())
	c := &conditionSampler{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			start := time.Now()
			round := r.conditionRound(ctx, sampling)
			if ctx.Err() != nil {
				return // a round cut short by stop is not kept
			}
			c.rounds = append(c.rounds, round)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(start.Add(conditionSampleEvery))):
			}
		}
	}()
	return c
}

// stop ends the sampling and returns the rounds. It is safe to call more than
// once.
func (c *conditionSampler) stop() []conditionRound {
	c.once.Do(func() {
		c.cancel()
		<-c.done
	})
	return c.rounds
}

func (r *haRun) conditionRound(ctx context.Context, sampling conditionSampling) conditionRound {
	ctx, cancel := context.WithTimeout(ctx, conditionReadLimit)
	defer cancel()
	var round conditionRound
	start := time.Now().UTC()
	out, err := r.kubectlQuiet(ctx, "get", "cubridcluster", r.cluster, "-o", "json")
	var s statusSnapshot
	if err == nil {
		s, err = parseStatus(out)
	}
	if err != nil {
		round.StatusError = err.Error()
	} else {
		sample := s.details
		sample.ReadStart = start
		// availableReplicas is left out of the Deployment's status when it is 0.
		if out, err := r.kubectlQuiet(ctx, "get", "deployment", r.cluster+"-broker-rw",
			"-o", "jsonpath={.status.availableReplicas}"); err == nil {
			n, err := 0, error(nil)
			if out = strings.TrimSpace(out); out != "" {
				n, err = strconv.Atoi(out)
			}
			if err == nil {
				sample.RWAvailable = &n
			}
		}
		sample.ReadEnd = time.Now().UTC()
		round.Status = &sample
	}
	if len(sampling.rows) > 0 {
		counts := faults.RowCounts{Start: time.Now().UTC(), Rows: make(map[string]int, len(sampling.rows))}
		for _, pod := range sampling.rows {
			n, err := r.ledgerRows(ctx, pod)
			if err != nil {
				if counts.Errors == nil {
					counts.Errors = map[string]string{}
				}
				// The whole output of csql would bury the reason.
				why := err.Error()
				if len(why) > 300 {
					why = why[:300] + " ..."
				}
				counts.Errors[pod] = why
				continue
			}
			counts.Rows[pod] = n
		}
		counts.End = time.Now().UTC()
		round.Rows = &counts
	}
	if sampling.roles {
		sweep := faults.RoleSweep{Start: time.Now().UTC(), Roles: make(map[string]string, len(r.members)),
			NodeStates: make(map[string]string, len(r.members))}
		for _, pod := range r.members {
			sweep.Roles[pod], sweep.NodeStates[pod] = r.heartbeatRole(ctx, pod)
		}
		sweep.End = time.Now().UTC()
		round.Roles = &sweep
	}
	return round
}

// ledgerRows counts the workload's ledger rows on a member, with csql inside
// its Pod, without announcing the command in the test output.
// Each count is given rowCountLimit, so that a member whose Pod is being
// replaced does not hold up the round.
func (r *haRun) ledgerRows(ctx context.Context, pod string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, rowCountLimit)
	defer cancel()
	out, err := runSQL(csqlInPodQuiet(ctx, r.namespace, pod, r.database), "SELECT TO_CHAR(COUNT(*)) AS v FROM ledger;")
	if err != nil {
		return 0, err
	}
	values, err := sqlcheck.Values(out)
	if err != nil {
		return 0, err
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("the count of the ledger rows on %s: %v", pod, values)
	}
	return strconv.Atoi(values[0])
}

// heartbeatRole is the role CUBRID's heartbeat reports on pod, as
// heartbeatRoleIn reads it, and the state of its own node, as
// heartbeatNodeStateIn reads it; both "" when the member did not answer.
func (r *haRun) heartbeatRole(ctx context.Context, pod string) (role, nodeState string) {
	out, err := r.kubectlQuiet(ctx, "exec", pod, "--", "bash", "-c", heartbeatStatus)
	if err != nil {
		return "", ""
	}
	return heartbeatRoleIn(pod, out), heartbeatNodeStateIn(pod, out)
}

// checkConditions compares the sampled Conditions with the clients'
// operations and the fault's timeline by the given rules. It stores
// conditions.jsonl and agreement.txt under dir and returns what it found, the
// files it wrote, and the conditionAgreement measurement of summary.json.
func checkConditions(dir string, rounds []conditionRound, events []workload.Event, steps []faults.Step,
	rules []faults.Rule) (faults.Agreement, []string, map[string]any) {
	obs := faults.Observations{Steps: steps}
	unread := 0
	var lines strings.Builder
	for _, round := range rounds {
		if line, err := json.Marshal(round); err == nil {
			lines.Write(line)
			lines.WriteByte('\n')
		}
		if round.Status != nil {
			obs.Samples = append(obs.Samples, *round.Status)
		} else {
			unread++
		}
		if round.Roles != nil {
			obs.Sweeps = append(obs.Sweeps, *round.Roles)
		}
		if round.Rows != nil {
			obs.Rows = append(obs.Rows, *round.Rows)
		}
	}
	for _, e := range events {
		switch e.Event {
		case workload.Acknowledged, workload.Failed, workload.Unknown:
			obs.SQL = append(obs.SQL, faults.SQLEvent{T: e.T, Write: e.Op == workload.OpInsert, Outcome: e.Event})
		}
	}
	agreement := faults.CheckAgreement(obs, rules)

	var text strings.Builder
	fmt.Fprintf(&text, "Agreement of the CubridCluster's Conditions with SQL and the fault, %s\n\n", dir)
	fmt.Fprintf(&text, "Status reads: %d, of which %d could not be read; heartbeat sweeps: %d; "+
		"client operations with an outcome: %d\n", len(rounds), unread, len(obs.Sweeps), len(obs.SQL))
	fmt.Fprintf(&text, "Tolerance of the rules that compare sources: %s\n\nRules:\n", faults.AgreementTolerance)
	for _, rule := range rules {
		judged := ""
		if n, ok := agreement.Evaluated[rule.ID]; ok {
			judged = fmt.Sprintf(", %d status reads judged", n)
		}
		fmt.Fprintf(&text, "- %s (%s%s): %s\n", rule.ID, rule.Kind, judged, rule.Says)
	}
	for _, part := range []struct {
		title string
		lines []string
	}{
		{"Disagreements, each with its kind: a Never or a tolerance rule", agreement.Disagreements},
		{"Rules that could not be judged, which block the scenario", agreement.NotJudged},
		{"Notes, which never fail the run", agreement.Notes},
	} {
		fmt.Fprintf(&text, "\n%s:\n", part.title)
		if len(part.lines) == 0 {
			text.WriteString("- none\n")
		}
		for _, l := range part.lines {
			fmt.Fprintf(&text, "- %s\n", l)
		}
	}

	var files []string
	for _, file := range [][2]string{{"conditions.jsonl", lines.String()}, {"agreement.txt", text.String()}} {
		if f := writeScenarioFile(dir+"/"+file[0], []byte(file[1])); f != "" {
			files = append(files, f)
		}
	}
	return agreement, files, map[string]any{
		"statusReads":   len(rounds),
		"unreadStatus":  unread,
		"disagreements": len(agreement.Disagreements),
		"notJudged":     len(agreement.NotJudged),
		"evaluated":     agreement.Evaluated,
		"notes":         len(agreement.Notes),
	}
}

// TestCheckConditionsReadsRoundsAndClientEvents checks that a disagreement in
// the sampled status reaches the scenario, that a round whose status could
// not be read is counted, and that both evidence files are written.
func TestCheckConditionsReadsRoundsAndClientEvents(t *testing.T) {
	t.Setenv("E2E_EVIDENCE_DIR", t.TempDir())
	now := time.Now().UTC()
	read := func(routingReady, writeEndpointReady string) *faults.ConditionSample {
		return &faults.ConditionSample{ReadStart: now, ReadEnd: now, LastObserved: now,
			Conditions: map[string]faults.ConditionState{
				"RoutingReady": {Status: routingReady}, "WriteEndpointReady": {Status: writeEndpointReady},
			}}
	}
	rounds := []conditionRound{
		{Status: read("True", "True")},
		{StatusError: "the API server did not answer"},
		{Status: read("True", "False")},
	}
	events := []workload.Event{
		{T: now.Add(time.Second), Op: workload.OpInsert, Event: workload.Attempted},
		{T: now.Add(time.Second), Op: workload.OpInsert, Event: workload.Failed},
	}
	agreement, files, measured := checkConditions("S05/test", rounds, events, nil,
		append(faults.NeverRules(), faults.S05Rules("m-0", true)...))
	if len(agreement.Disagreements) != 1 || !strings.Contains(agreement.Disagreements[0], "WriteEndpointReady=False") {
		t.Errorf("disagreements = %v, want the read with routing ready and no write endpoint", agreement.Disagreements)
	}
	if !slices.ContainsFunc(agreement.Notes, func(n string) bool { return strings.Contains(n, "1 writes failed") }) {
		t.Errorf("notes = %v, want the failed write while routing was ready", agreement.Notes)
	}
	if measured["statusReads"] != 3 || measured["unreadStatus"] != 1 || measured["disagreements"] != 1 {
		t.Errorf("measurement = %v", measured)
	}
	if want := []string{"S05/test/conditions.jsonl", "S05/test/agreement.txt"}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
}

// TestHeartbeatRoleIn checks the reading of cubrid heartbeat status that S05's
// sweeps and master() share: a member is a master or a slave only when the
// line names that member and its state is active or standby.
func TestHeartbeatRoleIn(t *testing.T) {
	for out, want := range map[string]string{
		" HA-Node Info (current c-0, state master)\n  Node c-0 (priority 1, state master)\n" +
			" HA-Process Info (master 1, state master)\n  Server db (pid 1, state registered_and_active)": "master",
		" HA-Node Info (current c-0, state slave)\n  Server db (pid 1, state registered_and_standby)": "slave",
		" HA-Node Info (current c-0, state master)\n  Server db (pid 1, state registered)":            "",
		" HA-Node Info (current c-1, state master)\n  Server db (pid 1, state registered_and_active)": "",
		"cubrid heartbeat is not running": "",
	} {
		if got := heartbeatRoleIn("c-0", out); got != want {
			t.Errorf("heartbeatRoleIn(%q) = %q, want %q", out, got, want)
		}
	}
}

// TestS05CannotPassWithARuleNeverEvaluated checks that a run whose status
// reads never carried a Condition a required rule reads is blocked, not
// passed, and that a disagreement fails a run that passed everything else.
func TestS05CannotPassWithARuleNeverEvaluated(t *testing.T) {
	t.Setenv("E2E_EVIDENCE_DIR", t.TempDir())
	start := time.Now().UTC()
	var rounds []conditionRound
	for i := range 20 {
		at := start.Add(time.Duration(i) * 2 * time.Second)
		available := 2
		rounds = append(rounds, conditionRound{
			Status: &faults.ConditionSample{ReadStart: at, ReadEnd: at, LastObserved: at, CurrentPrimary: "m-0",
				RWAvailable: &available,
				// No read carries WriteEndpointReady or BrokerReady.
				Conditions: map[string]faults.ConditionState{
					"RoutingReady": {Status: "True"}, "PrimaryResolved": {Status: "True"}, "HAReady": {Status: "True"},
					"ReplicationHealthy": {Status: "True"}, "ReadEndpointReady": {Status: "True"},
				},
				Instances: []faults.MemberStatus{{Name: "m-0", Role: "master", LastObserved: at}}},
			Roles: &faults.RoleSweep{Start: at, End: at, Roles: map[string]string{"m-0": "master"}},
		})
	}
	agreement, _, measured := checkConditions("S05/test", rounds, nil, nil,
		slices.Concat(faults.NeverRules(), faults.HAReportRules(), faults.S05Rules("m-0", true)))
	passed := evidence.Scenario{ID: "S05", Variant: "test", Result: evidence.Pass}
	got := withAgreement(passed, agreement)
	if got.Result != evidence.Blocked || !strings.HasPrefix(got.Reason, reasonConditionsNotJudged) {
		t.Errorf("result %s (%s), want blocked with %s; not judged: %v", got.Result, got.Reason,
			reasonConditionsNotJudged, agreement.NotJudged)
	}
	if evaluated, ok := measured["evaluated"].(map[string]int); !ok || len(evaluated) == 0 {
		t.Errorf("measurement %v has no evaluated count per rule", measured)
	}

	failed := withAgreement(passed, faults.Agreement{Disagreements: []string{"never: a: b"}})
	if failed.Result != evidence.Fail || failed.Reason != reasonConditionDisagreement || len(failed.NeverEvents) != 1 {
		t.Errorf("a disagreement gave %+v", failed)
	}
	if kept := withAgreement(evidence.Scenario{Result: evidence.Fail, Reason: "data check"},
		faults.Agreement{NotJudged: []string{"a: no evidence"}}); kept.Result != evidence.Fail {
		t.Errorf("a failed run became %s", kept.Result)
	}
}

// withAgreement gives a scenario's result what the comparison of its
// Conditions found. A disagreement fails it and is listed in neverEvents. A
// rule the scenario requires that could not be judged leaves no verdict on
// the Conditions, since the evidence it needs is missing: a result that
// passed everything else becomes blocked, never pass. A result that is
// already not a pass keeps its own verdict.
func withAgreement(result evidence.Scenario, agreement faults.Agreement) evidence.Scenario {
	if len(agreement.Disagreements) > 0 {
		result.NeverEvents = append(result.NeverEvents, agreement.Disagreements...)
		if result.Reason == "" {
			result.Reason = reasonConditionDisagreement
		} else {
			result.Reason = reasonConditionDisagreement + "; " + result.Reason
		}
		result.Result = evidence.Fail
	}
	if len(agreement.NotJudged) > 0 && result.Result == evidence.Pass {
		result.Result = evidence.Blocked
		result.Reason = reasonConditionsNotJudged + ": " + strings.Join(agreement.NotJudged, "; ")
	}
	return result
}

// TestStallVerdictFailsOnADisagreementOrARuleNotJudged checks the verdict of
// the replication-stall step, which is not a scenario of the contract and so
// has no blocked result: a disagreement fails it, and so does a rule that
// could not be judged, with a message that says which of the two it was.
func TestStallVerdictFailsOnADisagreementOrARuleNotJudged(t *testing.T) {
	if err := stallVerdict(faults.Agreement{}); err != nil {
		t.Errorf("an agreement without findings: %v", err)
	}
	for want, a := range map[string]faults.Agreement{
		"disagreed":     {Disagreements: []string{"never: healthy-slave-never-named-unhealthy: read at x"}},
		"not be judged": {NotJudged: []string{"stall-reported-while-the-slave-lacks-rows: no row count"}},
	} {
		if err := stallVerdict(a); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("verdict of %+v = %v, want an error saying %q", a, err, want)
		}
	}
}

// stallVerdict is the replication-stall step's verdict on its Conditions. The
// step is not a scenario of the contract and has no blocked result, so a
// rule that could not be judged fails it too: the step cannot say that the
// Conditions agreed.
func stallVerdict(a faults.Agreement) error {
	switch {
	case len(a.Disagreements) > 0:
		return fmt.Errorf("the Conditions disagreed with the rows or with each other: %s",
			strings.Join(a.Disagreements, "; "))
	case len(a.NotJudged) > 0:
		return fmt.Errorf("a rule on the Conditions could not be judged: %s", strings.Join(a.NotJudged, "; "))
	}
	return nil
}

// TestNotTrueWindowFromTheSampledReads checks how S03 measures from the
// sampled reads how long a Condition was not True after the fault: from the
// first read between the fault and the outcome in which it was not True to the
// first later read in which it was again, also when that read started after
// the outcome.
func TestNotTrueWindowFromTheSampledReads(t *testing.T) {
	start := time.Now().UTC()
	read := func(second int, status string) conditionRound {
		at := start.Add(time.Duration(second) * time.Second)
		return conditionRound{Status: &faults.ConditionSample{ReadStart: at, ReadEnd: at,
			Conditions: map[string]faults.ConditionState{"PrimaryResolved": {Status: status}}}}
	}
	rounds := []conditionRound{read(0, "False"), read(2, "True"), read(4, "False"), {StatusError: "no answer"},
		read(8, "Unknown"), read(10, "True"), read(12, "False")}
	w := notTrueWindow(rounds, "PrimaryResolved", start.Add(time.Second), start.Add(11*time.Second))
	if !w.from.Equal(start.Add(4*time.Second)) || !w.to.Equal(start.Add(10*time.Second)) {
		t.Errorf("window = %s to %s, want 4s to 10s after the start", w.from.Sub(start), w.to.Sub(start))
	}
	// The read at 12s opens a window that no later read closes.
	open := notTrueWindow(rounds, "PrimaryResolved", start.Add(11*time.Second), start.Add(20*time.Second))
	if open.length() != "not ended" {
		t.Errorf("a window still open at the end of the reads: %s", open.length())
	}
	// The window opened before the outcome at 9s is closed by the read at
	// 10s, which started after the outcome.
	late := notTrueWindow(rounds, "PrimaryResolved", start.Add(3*time.Second), start.Add(9*time.Second))
	if !late.from.Equal(start.Add(4*time.Second)) || !late.to.Equal(start.Add(10*time.Second)) {
		t.Errorf("window = %s to %s, want 4s to 10s after the start: the True read after the outcome closes it",
			late.from.Sub(start), late.to.Sub(start))
	}
	// A read that is not True after the outcome opens no window.
	none := notTrueWindow(rounds, "PrimaryResolved", start.Add(5*time.Second), start.Add(7*time.Second))
	if none.length() != "not observed" {
		t.Errorf("a window opened after the outcome: %s", none.length())
	}
}

func TestHeartbeatNodeStateIn(t *testing.T) {
	for out, want := range map[string]string{
		" HA-Node Info (current c-0, state master)\n  Server db (pid 1, state registered_and_to_be_active)": "master",
		" HA-Node Info (current c-0, state to-be-slave)":                                                    "to-be-slave",
		" HA-Node Info (current c-1, state master)":                                                         "",
		"cubrid heartbeat is not running":                                                                   "",
	} {
		if got := heartbeatNodeStateIn("c-0", out); got != want {
			t.Errorf("heartbeatNodeStateIn(%q) = %q, want %q", out, got, want)
		}
	}
}

// notTrueWindow is the window from the first read that started between from
// and to in which the Condition was not True, to the first later read in which
// it was True again. That later read may start after to: the sampling sees the
// Condition become True again only at its next read, which may come after the
// outcome.
func notTrueWindow(rounds []conditionRound, condition string, from, to time.Time) window {
	var w window
	for _, round := range rounds {
		s := round.Status
		if s == nil || s.ReadStart.Before(from) {
			continue
		}
		holds := s.Conditions[condition].Status != "True"
		switch {
		case holds && w.from.IsZero() && !s.ReadStart.After(to):
			w.from = s.ReadStart
		case !holds && !w.from.IsZero() && w.to.IsZero():
			w.to = s.ReadStart
		}
	}
	return w
}

// writeRoleSweeps stores the heartbeat sweeps of the rounds as JSON lines,
// one per sweep with its start and end, and returns the file written.
func writeRoleSweeps(rel string, rounds []conditionRound) string {
	var b strings.Builder
	for _, round := range rounds {
		if round.Roles == nil {
			continue
		}
		if line, err := json.Marshal(round.Roles); err == nil {
			b.Write(line)
			b.WriteByte('\n')
		}
	}
	return writeScenarioFile(rel, []byte(b.String()))
}

// TestS03ConditionsAreJudgedOnEveryResult checks that S03 judges its sampled
// reads also when the wait for the outcome ended without it: a read in the
// wait with RoutingReady=True and PrimaryResolved not True fails the scenario
// with the disagreement as its reason (ADR-0005, section 6), whatever the wait
// ended with, and the reads and the verdict are kept as evidence.
func TestS03ConditionsAreJudgedOnEveryResult(t *testing.T) {
	t.Setenv("E2E_EVIDENCE_DIR", t.TempDir())
	start := time.Now().UTC()
	rounds := make([]conditionRound, 0, 10)
	for i := range 10 {
		at := start.Add(time.Duration(2*i) * time.Second)
		resolved := "True"
		if i == 5 {
			resolved = "False"
		}
		rounds = append(rounds, conditionRound{
			Status: &faults.ConditionSample{ReadStart: at, ReadEnd: at, LastObserved: at, CurrentPrimary: "m-0",
				Conditions: map[string]faults.ConditionState{
					"RoutingReady": {Status: "True"}, "PrimaryResolved": {Status: resolved},
				}},
			Roles: &faults.RoleSweep{Start: at, End: at, Roles: map[string]string{"m-0": "master"}},
		})
	}
	timeline := func(end string) []faults.Step {
		return []faults.Step{{T: start.Add(time.Second), Kind: "fault_issued"},
			{T: start.Add(time.Second), Kind: "fault_confirmed"}, {T: start.Add(19 * time.Second), Kind: end}}
	}
	for _, ended := range []evidence.Scenario{
		{ID: "S03", Variant: "test", Result: evidence.Blocked, Reason: faults.ReasonStepStillRunning},
		{ID: "S03", Variant: "test", Result: evidence.Blocked, Reason: "run_cancelled: context canceled"},
		{ID: "S03", Variant: "test", Result: evidence.Fail, Reason: "a check of S03 failed; see the test output"},
	} {
		got, _ := s03Conditions("test", ended, rounds, nil, timeline(ended.Reason), "")
		if got.Result != evidence.Fail || !strings.HasPrefix(got.Reason, reasonConditionDisagreement) ||
			len(got.NeverEvents) == 0 {
			t.Errorf("a wait ending with %q gave %s (%s), never events %v", ended.Reason, got.Result, got.Reason,
				got.NeverEvents)
		}
		for _, file := range []string{"S03/test/conditions.jsonl", "S03/test/agreement.txt", "S03/test/roles.jsonl"} {
			if !slices.Contains(got.Evidence, file) {
				t.Errorf("a wait ending with %q kept the evidence %v, without %s", ended.Reason, got.Evidence, file)
			}
		}
	}
}

// TestS06ConditionsAreJudgedOnEveryResult checks that S06 judges its sampled
// reads also when a variant ended without the outcome or failed a later
// check: a read with HAReady=True and PrimaryResolved not True, which the
// Operator never writes, fails the variant with the disagreement as its
// reason, and the reads and the verdict are kept as evidence. A run whose
// outcome was not reached has no master to compare with after the Operator's
// return, so it cannot pass on the Conditions.
func TestS06ConditionsAreJudgedOnEveryResult(t *testing.T) {
	t.Setenv("E2E_EVIDENCE_DIR", t.TempDir())
	start := time.Now().UTC()
	rounds := make([]conditionRound, 0, 10)
	for i := range 10 {
		at := start.Add(time.Duration(2*i) * time.Second)
		resolved := "True"
		if i == 5 {
			resolved = "False"
		}
		rounds = append(rounds, conditionRound{
			Status: &faults.ConditionSample{ReadStart: at, ReadEnd: at, LastObserved: at, CurrentPrimary: "m-0",
				Conditions: map[string]faults.ConditionState{
					"HAReady": {Status: "True"}, "PrimaryResolved": {Status: resolved},
				}},
			Roles: &faults.RoleSweep{Start: at, End: at, Roles: map[string]string{"m-0": "master"}},
		})
	}
	timeline := []faults.Step{{T: start.Add(time.Second), Kind: "fault_issued"},
		{T: start.Add(time.Second), Kind: "fault_confirmed"}, {T: start.Add(19 * time.Second), Kind: "time_limit_exceeded"}}
	for _, variant := range s06Variants {
		for _, ended := range []evidence.Scenario{
			{ID: "S06", Variant: variant, Result: evidence.Blocked, Reason: faults.ReasonStepStillRunning},
			{ID: "S06", Variant: variant, Result: evidence.Fail, Reason: "a check of S06 failed; see the test output"},
		} {
			got, _ := s06Conditions(variant, ended, rounds, nil, timeline, "", time.Time{})
			if got.Result != evidence.Fail || !strings.HasPrefix(got.Reason, reasonConditionDisagreement) ||
				len(got.NeverEvents) == 0 {
				t.Errorf("%s ending with %q gave %s (%s), never events %v", variant, ended.Reason, got.Result,
					got.Reason, got.NeverEvents)
			}
			for _, file := range []string{"conditions.jsonl", "agreement.txt", "roles.jsonl"} {
				if file = "S06/" + variant + "/" + file; !slices.Contains(got.Evidence, file) {
					t.Errorf("%s ending with %q kept the evidence %v, without %s", variant, ended.Reason,
						got.Evidence, file)
				}
			}
			if _, ok := got.Measurements["conditionAgreement"]; !ok {
				t.Errorf("%s ending with %q has no conditionAgreement measurement", variant, ended.Reason)
			}
		}
	}
	// Every read agrees, but without the outcome the rule after the
	// Operator's return has nothing to judge: a run that passed everything
	// else is blocked, not passed.
	agreeing := slices.Clone(rounds)
	agreeing[5] = rounds[4]
	passed := evidence.Scenario{ID: "S06", Variant: s06Absent, Result: evidence.Pass}
	if got, _ := s06Conditions(s06Absent, passed, agreeing, nil, timeline, "", time.Time{}); got.Result != evidence.Blocked ||
		!strings.HasPrefix(got.Reason, reasonConditionsNotJudged) {
		t.Errorf("a run without the outcome gave %s (%s), want blocked with %s", got.Result, got.Reason,
			reasonConditionsNotJudged)
	}
}
