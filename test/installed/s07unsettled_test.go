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

package installed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The tests in this file cover what the network partition lane keeps when
// the cluster does not settle after the cut is removed: CUBRID's own
// heartbeat state of every member in each sweep, the roles and conditions
// when the settle wait expires, and a best-effort read of every member's rows
// before the run namespace is deleted. On the VM lab a run of
// higher-rank-isolated ended blocked with a member in a state the lane only
// counted as "other", and whether the acknowledged writes survived was lost
// with the namespace (#115).

// The server process states of cubrid heartbeat status the tests print.
const (
	testToBeActive = "registered_and_to_be_active"
	testStandby    = "registered_and_standby"
	testActive     = "registered_and_active"
)

// heartbeatOutput is cubrid heartbeat status as a member prints it, with the
// node state of the member itself and its server process's state; an empty
// server state prints no server line, as after a stop.
func heartbeatOutput(pod, node, server string) string {
	out := " HA-Node Info (current " + pod + ", state " + node + ")\n" +
		"   Node " + m0 + " (priority 1, state " + node + ")\n" +
		" HA-Process Info (master 63, state " + node + ")\n" +
		"   Copylogdb appdb@ha-1:/var/lib/cubrid/databases/appdb_ha-1 (pid 326, state registered)\n"
	if server != "" {
		out += "   Server appdb (pid 72, state " + server + ")\n"
	}
	return out
}

// The raw state keeps CUBRID's own words for the member's node and server,
// which the classified role folds into "other".
func TestHeartbeatStateKeepsCUBRIDsOwnWords(t *testing.T) {
	tests := map[string]struct{ out, want string }{
		"a member promoting itself": {heartbeatOutput(m0, "to-be-master", testToBeActive),
			"to-be-master (server registered_and_to_be_active)"},
		"an active master": {heartbeatOutput(m0, roleMaster, testActive),
			"master (server registered_and_active)"},
		"a member in maintenance with no server line": {heartbeatOutput(m0, "maintenance", ""), "maintenance"},
		"output without the member's node line": {"\n  cubrid heartbeat is not running.\nmore\n",
			"no node state: cubrid heartbeat is not running."},
		"no output": {"", "no node state: no output"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := heartbeatState(m0, tc.out); got != tc.want {
				t.Errorf("heartbeatState = %q, want %q", got, tc.want)
			}
		})
	}
}

// s07StateAnswers plays kubectl for the members' heartbeat status with the
// node and server state of each member; a member with no entry is not
// reached.
func s07StateAnswers(states map[string][2]string) func(context.Context, ...string) (string, error) {
	return func(_ context.Context, args ...string) (string, error) {
		pod := args[3]
		s, ok := states[pod]
		if !ok {
			return "error: unable to upgrade connection: container not found", errors.New("exit status 1")
		}
		return heartbeatOutput(pod, s[0], s[1]), nil
	}
}

// Each sweep keeps every member's raw state next to its classified role, so a
// member that is neither master nor slave shows what CUBRID called it, and a
// member that could not be read shows why. The role keys stay as they were.
func TestSweepS07RecordsEachMembersRawState(t *testing.T) {
	h, _ := s07PathClient(t, databasev1alpha1.CubridClusterStatus{}, 1, 1, 1)
	h.members, h.guard = testS07Members, func(context.Context) error { return nil }
	h.Command = s07StateAnswers(map[string][2]string{m0: {"to-be-master", testToBeActive},
		m1: {roleMaster, testActive}})
	rec := &s07Record{Target: imageIdentity{Pod: m0}, LastResolvedPrimary: m1}
	h.sweepS07(context.Background(), rec, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), true, true)
	sweep := rec.Sweeps[0]
	if sweep.Roles[m0] != s07OtherRole || sweep.Roles[m1] != s07ActiveMaster || sweep.Roles[m2] != s07Unread {
		t.Errorf("roles %v", sweep.Roles)
	}
	if sweep.States[m0] != "to-be-master (server registered_and_to_be_active)" ||
		sweep.States[m1] != "master (server registered_and_active)" ||
		!strings.HasPrefix(sweep.States[m2], "unread: ") || !strings.Contains(sweep.States[m2], "container not found") {
		t.Errorf("raw states %v", sweep.States)
	}
	data, err := json.Marshal(sweep)
	if err != nil || !strings.Contains(string(data), `"roles":{`) || !strings.Contains(string(data), `"states":{`) {
		t.Errorf("sweep JSON %s, %v: want the roles key kept and the raw states added", data, err)
	}
}

// When the settle wait expires, the reason names each member's raw state, and
// the record keeps the last roles, raw states, conditions and quarantine the
// run saw then.
func TestSettleS07NamesTheRawStatesWhenItDoesNotSettle(t *testing.T) {
	old := s07SettleBound
	s07SettleBound = 30 * time.Millisecond
	t.Cleanup(func() { s07SettleBound = old })
	status := databasev1alpha1.CubridClusterStatus{
		Conditions: []metav1.Condition{condition(primaryResolvedCondition, "False")},
		WriteQuarantine: &databasev1alpha1.WriteQuarantine{GenerationID: "g1",
			Reason: databasev1alpha1.WriteQuarantinedMasterChanged}}
	h, _ := s07PathClient(t, status, 0, 0, 0)
	h.cfg.Poll = 5 * time.Millisecond
	h.members, h.guard = testS07Members, func(context.Context) error { return nil }
	h.Command = s07StateAnswers(map[string][2]string{m0: {"to-be-master", testToBeActive},
		m1: {roleSlave, testStandby}, m2: {roleSlave, testStandby}})
	rec := &s07Record{Target: imageIdentity{Pod: m0}, LastResolvedPrimary: m1}
	h.settleS07(context.Background(), rec)
	if rec.SettledAt != nil {
		t.Fatalf("settled at %v with no active master", rec.SettledAt)
	}
	if !strings.Contains(rec.SettleUnmet, m0+": to-be-master (server registered_and_to_be_active)") ||
		!strings.Contains(rec.SettleUnmet, m1+": slave") {
		t.Errorf("settle unmet %q, want each member's raw state", rec.SettleUnmet)
	}
	e := rec.SettleExpired
	if e == nil || e.At.IsZero() || e.Roles[m0] != s07OtherRole || e.States[m0] == "" ||
		e.Conditions[primaryResolvedCondition] != "False/Test" ||
		e.Quarantine != string(databasev1alpha1.WriteQuarantinedMasterChanged) {
		t.Fatalf("settle wait expiry %+v", e)
	}
	base := s07Base(false)
	base.SettledAt, base.SettleUnmet, base.Data, base.HealthReadings = nil, rec.SettleUnmet, nil, 0
	if v := judgeS07(base, testS07Limits); v.Result != evidence.Blocked ||
		!strings.Contains(v.Reason, m0+": to-be-master") {
		t.Errorf("judged %s %q, want blocked naming %s's raw state", v.Result, v.Reason, m0)
	}
}

// s07LedgerEvent is one line of a client's history for an insert.
func s07LedgerEvent(id, kind string) string {
	return `{"t":"2026-11-17T12:00:01Z","client":"c","seq":` + strings.TrimPrefix(id, "c-") +
		`,"op":"insert","opId":"` + id + `","event":"` + kind + `","amount":1,"note":"n"}` + "\n"
}

// s07LedgerAnswers plays kubectl for a read of the ledger on each member: the
// rows in rows, an error for a member in failing, and for a member in
// hanging nothing until the read's context ends.
func s07LedgerAnswers(rows map[string][]workload.Row, failing, hanging string) func(context.Context,
	...string) (string, error) {
	return func(ctx context.Context, args ...string) (string, error) {
		pod := args[3]
		switch pod {
		case failing:
			return "error: unable to upgrade connection: pod does not exist", errors.New("exit status 1")
		case hanging:
			<-ctx.Done()
			return "", ctx.Err()
		}
		var b strings.Builder
		b.WriteString("=== <Result of SELECT Command in Line 1> ===\n\n  v\n======\n")
		for _, line := range jsonLines(rows[pod]) {
			b.WriteString("  '" + line + "'\n")
		}
		return b.String(), nil
	}
}

func s07Row(id string) workload.Row {
	seq := 0
	for _, c := range strings.TrimPrefix(id, "c-") {
		seq = seq*10 + int(c-'0')
	}
	return workload.Row{OpID: id, Client: "c", Seq: seq, Amount: 1, Note: "n"}
}

// s07SnapshotRun is a run of the three members that keeps its evidence in a
// temporary directory.
func s07SnapshotRun(t *testing.T) *haRun {
	cfg := testHAConfig(t)
	cfg.EvidenceDir = t.TempDir()
	return &haRun{Runner: &Runner{}, cfg: cfg, members: testS07Members, redactor: evidence.NewRedactor(),
		guard: func(context.Context) error { return nil }}
}

// When the cluster did not settle, the run reads every member's rows before
// the namespace is deleted: per member, the rows it holds, the acknowledged
// writes it lacks and the rows that contradict the history. A member that
// could not be read is unreadable, with no count, never zero.
func TestSnapshotS07RowsWhenTheClusterDidNotSettle(t *testing.T) {
	h := s07SnapshotRun(t)
	h.history.WriteString(s07LedgerEvent("c-1", "attempted") + s07LedgerEvent("c-1", "acknowledged") +
		s07LedgerEvent("c-2", "attempted") + s07LedgerEvent("c-2", "acknowledged") +
		s07LedgerEvent("c-3", "attempted") + s07LedgerEvent("c-3", "failed"))
	h.Command = s07LedgerAnswers(map[string][]workload.Row{
		m0: {s07Row("c-1"), s07Row("c-2"), s07Row("c-3")},
		m1: {s07Row("c-1")},
	}, m2, "")
	rec := &s07Record{}
	h.snapshotS07Rows(context.Background(), rec)
	s := rec.UnsettledRows
	if s == nil || s.At.IsZero() {
		t.Fatalf("no snapshot: %+v", s)
	}
	count := func(p *int) int {
		if p == nil {
			return -1
		}
		return *p
	}
	a, b, c := s.Members[m0], s.Members[m1], s.Members[m2]
	if !a.Read || count(a.Rows) != 3 || count(a.AcknowledgedMissing) != 0 || count(a.Conflicting) != 1 {
		t.Errorf("%s: %+v, want 3 rows, none missing, the failed write's row conflicting", m0, a)
	}
	if !b.Read || count(b.Rows) != 1 || count(b.AcknowledgedMissing) != 1 || count(b.Conflicting) != 0 {
		t.Errorf("%s: %+v, want 1 row and 1 acknowledged write missing", m1, b)
	}
	if c.Read || c.Error == "" || c.Rows != nil || c.AcknowledgedMissing != nil || c.Conflicting != nil {
		t.Errorf("%s: %+v, want unreadable with no counts", m2, c)
	}
	data, _ := json.Marshal(c)
	if strings.Contains(string(data), `"rows"`) {
		t.Errorf("an unreadable member has a row count: %s", data)
	}
	if s.Diverged == nil || s.File == "" {
		t.Errorf("snapshot %+v, want the divergence between the members read and the report's file", s)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.EvidenceDir, s.File)); err != nil {
		t.Errorf("the snapshot's report: %v", err)
	}
}

// The extra reads are bounded so that cleanup still happens within the run's
// deadline: a member that does not answer within the bound is unreadable, and
// the others are still read.
func TestSnapshotS07RowsIsBounded(t *testing.T) {
	old := s07SnapshotBound
	s07SnapshotBound = 50 * time.Millisecond
	t.Cleanup(func() { s07SnapshotBound = old })
	h := s07SnapshotRun(t)
	h.history.WriteString(s07LedgerEvent("c-1", "attempted") + s07LedgerEvent("c-1", "acknowledged"))
	h.Command = s07LedgerAnswers(map[string][]workload.Row{m0: {s07Row("c-1")}}, "", m1)
	rec := &s07Record{}
	start := time.Now()
	h.snapshotS07Rows(context.Background(), rec)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the snapshot took %s with a bound of %s", took, s07SnapshotBound)
	}
	s := rec.UnsettledRows
	if s == nil || !s.Members[m0].Read || s.Members[m1].Read || s.Members[m1].Error == "" {
		t.Fatalf("snapshot %+v, want %s read and %s unreadable", s, m0, m1)
	}
}

// The snapshot is reported and never judged: a run that did not settle stays
// blocked with the snapshot in its reason, however the rows look, and a
// never-event judged elsewhere still fails it.
func TestJudgeS07ReportsTheUnsettledSnapshotWithoutJudgingIt(t *testing.T) {
	one, zero, four := 1, 0, 4
	unsettled := func() *s07Record {
		rec := s07Base(false)
		rec.SettledAt, rec.SettleUnmet, rec.Data, rec.HealthReadings = nil, "CUBRID reports "+m0+": to-be-master", nil, 0
		rec.UnsettledRows = &s07RowSnapshot{At: time.Now(), Members: map[string]s07MemberRows{
			m0: {Read: true, Rows: &four, AcknowledgedMissing: &zero, Conflicting: &one},
			m1: {Read: true, Rows: &four, AcknowledgedMissing: &zero, Conflicting: &zero},
			m2: {Error: "csql failed"}}}
		return rec
	}
	v := judgeS07(unsettled(), testS07Limits)
	if v.Result != evidence.Blocked || !strings.Contains(v.Reason, "did not settle") ||
		!strings.Contains(v.Reason, m0+" holds 4 row(s), lacks 0 acknowledged write(s), 1 conflicting") ||
		!strings.Contains(v.Reason, m2+" unreadable") {
		t.Errorf("judged %s %q, want blocked with the snapshot reported", v.Result, v.Reason)
	}
	healthyLooking := unsettled()
	healthyLooking.HealthyAtEnd = true
	if v := judgeS07(healthyLooking, testS07Limits); v.Result != evidence.Blocked {
		t.Errorf("conflicting rows in the snapshot judged %s %q, want blocked (reported only)", v.Result, v.Reason)
	}
	reopened := unsettled()
	reopened.ReopenedWhileQuarantined = []string{"the -rw EndpointSlices had 2 endpoint(s) again"}
	if v := judgeS07(reopened, testS07Limits); v.Result != evidence.Fail {
		t.Errorf("a never-event with a snapshot judged %s %q, want fail", v.Result, v.Reason)
	}
}

// After a heal that did not settle, the run reads the clients and then every
// member's rows before it returns, so the snapshot is in the record when the
// namespace is deleted; the run stays blocked and names the snapshot.
func TestFinishS07ReadsTheRowsOfAClusterThatDidNotSettle(t *testing.T) {
	h := s07SnapshotRun(t)
	h.rec.Client = &imageIdentity{Pod: clientPodName, Node: "node-c"}
	open := s07LedgerEvent("c-1", "attempted") + s07LedgerEvent("c-1", "acknowledged")
	ledger := s07LedgerAnswers(map[string][]workload.Row{m0: {s07Row("c-1")}, m1: {s07Row("c-1")}}, m2, "")
	h.Command = func(ctx context.Context, args ...string) (string, error) {
		if args[3] != clientPodName {
			return ledger(ctx, args...)
		}
		script := args[len(args)-1]
		switch {
		case strings.Contains(script, "echo stopped"):
			return "stopped\n", nil
		case strings.Contains(script, "cat /tmp/"+s07OpenClient+".jsonl"):
			return open, nil
		}
		return "", nil
	}
	issued := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	rec := s07Base(false)
	rec.SettledAt, rec.SettleUnmet, rec.Data, rec.HealthReadings = nil, "CUBRID reports "+m0+": to-be-master", nil, 0
	rec.FaultIssuedAt, rec.ClockOffsets = &issued, map[string]float64{"node-c": 0}
	result := &evidence.Scenario{}
	got := h.finishS07(context.Background(), rec, result, func(r evidence.Result, format string,
		args ...any) evidence.Scenario {
		result.Result, result.Reason = r, fmt.Sprintf(format, args...)
		return *result
	})
	s := rec.UnsettledRows
	if s == nil || !s.Members[m0].Read || !s.Members[m1].Read || s.Members[m2].Read {
		t.Fatalf("snapshot %+v, want %s and %s read and %s unreadable", s, m0, m1, m2)
	}
	if rec.Data != nil {
		t.Errorf("the data check ran on a cluster that did not settle: %+v", rec.Data)
	}
	if got.Result != evidence.Blocked || !strings.Contains(got.Reason, m0+" holds 1 row(s), lacks 0") ||
		!strings.Contains(got.Reason, m2+" unreadable") {
		t.Errorf("finished %s %q, want blocked with the snapshot named", got.Result, got.Reason)
	}
}
