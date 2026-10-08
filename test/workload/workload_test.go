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

package workload

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	memberA = "m-0"
	memberB = "m-1"
	// unknownOp is an operation of the history whose outcome is unknown.
	unknownOp = "c1-4"
	// firstOp is the first operation of the history.
	firstOp = "c1-1"
)

// historyFile is a run of client c1: 1 and 2 acknowledged, 3 failed, 4 with an
// unknown outcome, 5 rolled back, 6 attempted and never answered.
const historyFile = "testdata/history.jsonl"

func mustHistory(t *testing.T) History {
	t.Helper()
	f, err := os.Open(historyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h, err := ReadHistory(f)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	return h
}

func row(seq, amount int, note string) Row {
	return Row{OpID: "c1-" + string(rune('0'+seq)), Client: "c1", Seq: seq, Amount: amount, Note: note}
}

func good() []Row { return []Row{row(1, 10, "n1"), row(2, 20, "n2")} }

func rules(m MemberCheck) string {
	out := make([]string, 0, len(m.Violations))
	for _, v := range m.Violations {
		out = append(out, v.Rule+":"+v.OpID)
	}
	return strings.Join(out, ",")
}

func TestReadHistory_Outcomes(t *testing.T) {
	h := mustHistory(t)
	want := map[string]string{
		firstOp: Acknowledged, "c1-2": Acknowledged, "c1-3": Failed,
		unknownOp: Unknown, "c1-5": Acknowledged,
		// No answer was recorded, so the outcome is not known.
		"c1-6": Unknown,
	}
	if len(h.Operations) != len(want) {
		t.Fatalf("operations = %d, want %d", len(h.Operations), len(want))
	}
	for id, outcome := range want {
		if got := h.Operations[id].Outcome; got != outcome {
			t.Errorf("%s outcome = %q, want %q", id, got, outcome)
		}
	}
	if op := h.Operations["c1-2"]; op.Amount != 20 || op.Note != "n2" || op.Seq != 2 || op.Client != "c1" {
		t.Errorf("c1-2 = %+v, want the values of its attempted line", op)
	}
	if h.Counts[Attempted] != 6 || h.Counts[Acknowledged] != 3 || h.Counts[Failed] != 1 || h.Counts[Unknown] != 2 {
		t.Errorf("counts = %v", h.Counts)
	}
}

// A history that contradicts itself cannot be the basis of a verdict.
func TestReadHistory_Rejects(t *testing.T) {
	// line is one event of operation c1-1, or of another ID when given.
	line := func(event string, id ...string) string {
		opID := firstOp
		if len(id) > 0 {
			opID = id[0]
		}
		return fmt.Sprintf(`{"client":"c1","seq":1,"op":"insert","opId":%q,"event":%q,"amount":1,"note":"n"}`,
			opID, event) + "\n"
	}
	for name, text := range map[string]string{
		"an answer without an attempt":   line(Acknowledged),
		"two different answers":          line(Attempted) + line(Acknowledged) + line(Failed),
		"the same operation ID twice":    line(Attempted) + line(Attempted),
		"the same client sequence twice": line(Attempted) + line(Attempted, "c1-other"),
		"an outcome that does not exist": line("committed"),
		"an operation without an ID":     line(Attempted, ""),
		"a line that is not JSON":        line(Attempted) + "not json",
	} {
		if _, err := ReadHistory(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheck_Rules(t *testing.T) {
	h := mustHistory(t)
	tests := map[string]struct {
		rows                []Row
		want                string
		acknowledgedMissing int
	}{
		"exactly the acknowledged rows": {rows: good()},
		// Present or absent, an unknown outcome is correct.
		"an unknown outcome that was committed": {rows: append(good(), row(4, 40, "n4"))},
		"both unknown outcomes committed":       {rows: append(good(), row(4, 40, "n4"), row(6, 60, "n6"))},
		"an acknowledged row is missing": {
			rows: []Row{row(1, 10, "n1")}, want: "missing:c1-2", acknowledgedMissing: 1},
		"an acknowledged row has other values": {
			rows: []Row{row(1, 10, "n1"), row(2, 21, "n2")}, want: "wrong_values:c1-2"},
		"an unknown outcome present with other values": {
			rows: append(good(), row(4, 41, "n4")), want: "wrong_values:c1-4"},
		"a row is there twice": {
			rows: append(good(), row(2, 20, "n2")), want: "applied_twice:c1-2"},
		"a client sequence is there twice": {
			rows: append(good(), Row{OpID: unknownOp, Client: "c1", Seq: 2, Amount: 40, Note: "n4"}),
			want: "wrong_values:c1-4,applied_twice:c1-4"},
		"a failed operation's row is there": {
			rows: append(good(), row(3, 30, "n3")), want: "rolled_back_present:c1-3"},
		"a rolled-back row is there": {
			rows: append(good(), row(5, 50, RolledBackNote)), want: "rolled_back_present:c1-5"},
		"a row nobody wrote": {
			rows: append(good(), Row{OpID: "x-1", Client: "x", Seq: 1, Amount: 1, Note: "n"}), want: "unexpected:x-1"},
		"a row nobody wrote with the rollback note": {
			rows: append(good(), Row{OpID: "x-2", Client: "x", Seq: 2, Amount: 1, Note: RolledBackNote}),
			want: "rolled_back_present:x-2"},
	}
	for name, tc := range tests {
		report := Check(h, map[string][]Row{memberA: tc.rows})
		if got := rules(report.Members[memberA]); got != tc.want {
			t.Errorf("%s: violations = %q, want %q", name, got, tc.want)
		}
		if report.OK != (tc.want == "") {
			t.Errorf("%s: OK = %v", name, report.OK)
		}
		if report.AcknowledgedMissing != tc.acknowledgedMissing {
			t.Errorf("%s: acknowledgedMissing = %d, want %d", name, report.AcknowledgedMissing, tc.acknowledgedMissing)
		}
	}
}

// An unknown outcome is counted on its own and never as missing data.
func TestCheck_UnknownIsNotLoss(t *testing.T) {
	h := mustHistory(t)
	report := Check(h, map[string][]Row{memberA: append(good(), row(4, 40, "n4"))})
	m := report.Members[memberA]
	if !report.OK || report.AcknowledgedMissing != 0 {
		t.Errorf("report = %+v, want OK with nothing missing", report)
	}
	if m.UnknownPresent != 1 || m.UnknownAbsent != 1 || m.Rows != 3 {
		t.Errorf("member = %+v, want one unknown present and one absent", m)
	}
}

func TestCheck_Divergence(t *testing.T) {
	h := mustHistory(t)
	same := Check(h, map[string][]Row{memberA: good(), memberB: good()})
	if !same.OK || len(same.Diverged) != 0 {
		t.Errorf("equal members reported as diverged: %+v", same)
	}
	// Each member alone satisfies the rules: the unknown row may be present
	// or absent. Together they do not agree.
	differ := Check(h, map[string][]Row{memberA: good(), memberB: append(good(), row(4, 40, "n4"))})
	if differ.OK || len(differ.Diverged) != 1 || differ.Diverged[0].OpID != unknownOp {
		t.Errorf("diverged = %+v, want c1-4", differ.Diverged)
	}
	if len(differ.Members[memberA].Violations)+len(differ.Members[memberB].Violations) != 0 {
		t.Errorf("divergence was also reported as a member's violation: %+v", differ.Members)
	}
	// An acknowledged row missing on one of two members is counted once.
	lost := Check(h, map[string][]Row{memberA: good(), memberB: {row(1, 10, "n1")}})
	if lost.OK || lost.AcknowledgedMissing != 1 {
		t.Errorf("report = %+v, want one acknowledged operation missing", lost)
	}
}

// With no member read there is nothing that proves the data, so it is not OK.
func TestCheck_NoMembers(t *testing.T) {
	if report := Check(mustHistory(t), nil); report.OK {
		t.Error("a check of no member passed")
	}
}

func TestReadRows(t *testing.T) {
	rows, err := ReadRows(strings.NewReader(
		`{"opId":"c1-000001","client":"c1","seq":1,"amount":38,"note":"c1-1"}` + "\n\n" +
			`{"opId":"c1-000002","client":"c1","seq":2,"amount":75,"note":"c1-2"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{OpID: "c1-000001", Client: "c1", Seq: 1, Amount: 38, Note: firstOp},
		{OpID: "c1-000002", Client: "c1", Seq: 2, Amount: 75, Note: "c1-2"},
	}
	if len(rows) != len(want) || rows[0] != want[0] || rows[1] != want[1] {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
	// Output that is not the rows, such as an error message, must not be
	// read as an empty table.
	for name, text := range map[string]string{
		"an error message":      "ERROR: cannot connect\n",
		"a history line":        `{"opId":"c1-1","client":"c1","seq":1,"event":"attempted"}`,
		"a row without its key": `{"client":"c1","seq":1,"amount":1,"note":"n"}`,
	} {
		if _, err := ReadRows(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReadEvents(t *testing.T) {
	f, err := os.Open(historyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	events, err := ReadEvents(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 11 {
		t.Fatalf("events = %d, want 11", len(events))
	}
	want := time.Date(2026, 10, 12, 9, 41, 7, 113_000_000, time.UTC)
	if e := events[0]; !e.T.Equal(want) || e.Event != Attempted || e.OpID != firstOp {
		t.Errorf("first event = %+v", e)
	}
	if _, err := ReadEvents(strings.NewReader("not json\n")); err == nil {
		t.Error("a line that is not JSON was accepted")
	}
}

// events builds a history of one event per second from a base time: a for
// acknowledged, f for failed, u for unknown, - for an attempt.
func eventsOf(base time.Time, pattern string) []Event {
	kinds := map[rune]string{'a': Acknowledged, 'f': Failed, 'u': Unknown, '-': Attempted}
	out := make([]Event, 0, len(pattern))
	for i, c := range pattern {
		out = append(out, Event{T: base.Add(time.Duration(i) * time.Second), Event: kinds[c]})
	}
	return out
}

func TestRecovery(t *testing.T) {
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	fault := base.Add(2500 * time.Millisecond) // between the third and the fourth event
	tests := map[string]struct {
		pattern     string
		ok          bool
		recoveredAt int // seconds after base
		stable      time.Duration
	}{
		"failures, then acknowledged again":      {"aaaffuaaaa", true, 6, 3 * time.Second},
		"an unknown outcome late resets it":      {"aaaffaauaa", true, 8, time.Second},
		"never interrupted":                      {"aaaaaa", true, 3, 2 * time.Second},
		"attempts do not count as answers":       {"aaaf-a-a", true, 5, 2 * time.Second},
		"still failing at the end":               {"aaaffaaf", false, 0, 0},
		"no answer after the fault":              {"aaa---", false, 0, 0},
		"a failure before the fault is not seen": {"faaaaa", true, 3, 2 * time.Second},
	}
	for name, tc := range tests {
		at, stable, ok := Recovery(eventsOf(base, tc.pattern), fault)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", name, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if want := base.Add(time.Duration(tc.recoveredAt) * time.Second); !at.Equal(want) || stable != tc.stable {
			t.Errorf("%s: recovered at +%s, stable %s; want +%ds, %s", name, at.Sub(base), stable, tc.recoveredAt, tc.stable)
		}
	}
}

// readOnlyHistory is a history of one write through the read-only Service
// with the given outcome and error.
func readOnlyHistory(t *testing.T, outcome, errText string) History {
	t.Helper()
	text := `{"client":"ro","seq":1,"op":"insert","opId":"ro-000001","endpoint":"ro","event":"attempted","amount":38,"note":"ro-1"}` + "\n"
	if outcome != "" {
		text += fmt.Sprintf(`{"client":"ro","seq":1,"op":"insert","opId":"ro-000001","endpoint":"ro","event":%q,"error":%q}`,
			outcome, errText) + "\n"
	}
	h, err := ReadHistory(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The read-only Broker answers a write with the error CUBRID returns while
// updates are disabled; the message is the one the JDBC client recorded on
// the pinned image (docs/poc/RESULTS.md, POC-22). Every other outcome of the
// write, including a failure to reach the Broker at all, is not a refusal.
func TestCheckReadOnlyRefusal(t *testing.T) {
	const refusal = "code -581: Attempted to update the database when updates are disabled. [CAS INFO-hab-ro:33001,1,24]"
	if err := CheckReadOnlyRefusal(readOnlyHistory(t, Failed, refusal)); err != nil {
		t.Errorf("a write refused by the read-only Broker: %v", err)
	}
	for name, h := range map[string]History{
		// Any other code, as a connection that failed or a timeout would
		// give; the values stand for "not the refusal" and are not CUBRID's.
		"another code":       readOnlyHistory(t, Failed, "code -1: the Broker could not be reached"),
		"another code again": readOnlyHistory(t, Failed, "code -2: the request timed out"),
		"no error recorded":  readOnlyHistory(t, Failed, ""),
		"code as a prefix":   readOnlyHistory(t, Failed, "code -5810: something else"),
		"write accepted":     readOnlyHistory(t, Acknowledged, ""),
		"outcome unknown":    readOnlyHistory(t, Unknown, refusal),
		"never answered":     readOnlyHistory(t, "", ""),
	} {
		if err := CheckReadOnlyRefusal(h); err == nil {
			t.Errorf("%s: judged as a refusal by the read-only Broker", name)
		}
	}
	two := readOnlyHistory(t, Failed, refusal)
	two.Operations["ro-000002"] = Operation{OpID: "ro-000002", Outcome: Failed, Error: refusal}
	two.Counts[Attempted]++
	two.Counts[Failed]++
	if err := CheckReadOnlyRefusal(two); err == nil {
		t.Error("two writes: judged as one refusal")
	}
}
