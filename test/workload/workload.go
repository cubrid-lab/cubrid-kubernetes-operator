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

// Package workload defines the record a SQL workload client keeps of what it
// did, and checks a database against that record, following
// docs/testing/scenario-contract.md. It uses no code from the controller.
package workload

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// Outcomes of a client operation.
const (
	Attempted    = "attempted"
	Acknowledged = "acknowledged"
	Failed       = "failed"
	Unknown      = "unknown"
)

// Client operations.
const (
	OpInsert   = "insert"
	OpRollback = "rollback"
	OpRead     = "read"
)

// RolledBackNote is the note of a row that a rollback operation writes and
// then rolls back. A row carrying it must never be found.
const RolledBackNote = "must-not-exist"

// Event is one line of history.jsonl.
type Event struct {
	T        time.Time `json:"t"`
	Client   string    `json:"client"`
	Seq      int       `json:"seq"`
	Op       string    `json:"op"`
	OpID     string    `json:"opId"`
	Endpoint string    `json:"endpoint,omitempty"`
	Event    string    `json:"event"`
	// Amount and Note are the values sent; they are on the attempted line. For
	// a read they are the values expected of the row read back, and Target is
	// that row's operation ID.
	Amount int    `json:"amount,omitempty"`
	Note   string `json:"note,omitempty"`
	Target string `json:"target,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Row is one row of the ledger table as read from a member.
type Row struct {
	OpID   string `json:"opId"`
	Client string `json:"client"`
	Seq    int    `json:"seq"`
	Amount int    `json:"amount"`
	Note   string `json:"note"`
}

// Operation is what the history says about one client operation.
type Operation struct {
	OpID   string
	Client string
	Seq    int
	Op     string
	Amount int
	Note   string
	// Target is the operation whose row a read reads back.
	Target  string
	Outcome string
	// AcknowledgedAt is when the client recorded the acknowledgement, for an
	// acknowledged operation.
	AcknowledgedAt time.Time
	// Error is what the client recorded with the outcome; it is empty for an
	// acknowledged one.
	Error string
}

// History is the client operations of a run, by operation ID.
type History struct {
	Operations map[string]Operation
	Counts     map[string]int
}

// Violation is one broken data rule.
type Violation struct {
	Rule   string `json:"rule"`
	OpID   string `json:"opId,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// MemberCheck is the result of the data rules for one member or endpoint.
type MemberCheck struct {
	Rows           int         `json:"rows"`
	UnknownPresent int         `json:"unknownPresent"`
	UnknownAbsent  int         `json:"unknownAbsent"`
	Violations     []Violation `json:"violations"`
}

// Report is the content of data-check.json.
type Report struct {
	OK                  bool                   `json:"ok"`
	AcknowledgedMissing int                    `json:"acknowledgedMissing"`
	Members             map[string]MemberCheck `json:"members"`
	Diverged            []Violation            `json:"diverged"`
	// ReadMismatches are the client's reads that found the row they read back
	// missing or different during the run.
	ReadMismatches []Violation `json:"readMismatches"`
}

// FirstViolation returns the first violation of the report whose operation
// ID starts with prefix, or else the first one. Violations are taken of the
// members and endpoints named in order, then of the others in name order,
// then of the divergence between them and the read mismatches, where member
// is empty. total counts
// every violation; it is 0 when there is none.
func (r Report) FirstViolation(order []string, prefix string) (member string, first Violation, total int) {
	names := slices.Clone(order)
	for _, name := range slices.Sorted(maps.Keys(r.Members)) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	type found struct {
		member string
		v      Violation
	}
	var all []found
	for _, name := range slices.Compact(names) {
		for _, v := range r.Members[name].Violations {
			all = append(all, found{name, v})
		}
	}
	for _, v := range r.Diverged {
		all = append(all, found{"", v})
	}
	for _, v := range r.ReadMismatches {
		all = append(all, found{"", v})
	}
	if len(all) == 0 {
		return "", Violation{}, 0
	}
	pick := all[0]
	if i := slices.IndexFunc(all, func(f found) bool {
		return prefix != "" && strings.HasPrefix(f.v.OpID, prefix)
	}); i >= 0 {
		pick = all[i]
	}
	return pick.member, pick.v, len(all)
}

// LedgerQuery returns every ledger row as one JSON line of a Row, for a
// client such as csql that reads a member directly, not through a Broker.
const LedgerQuery = `SELECT '{"opId":"' || op_id || '","client":"' || client_id ||` +
	` '","seq":' || CAST(seq AS VARCHAR) || ',"amount":' || CAST(amount AS VARCHAR) ||` +
	` ',"note":"' || note || '"}' AS v FROM ledger ORDER BY client_id, seq;`

// Data rules, as they appear in a Violation.
const (
	RuleMissing           = "missing"
	RuleWrongValues       = "wrong_values"
	RuleAppliedTwice      = "applied_twice"
	RuleRolledBackPresent = "rolled_back_present"
	RuleUnexpected        = "unexpected"
	RuleMembersDiverge    = "members_diverge"
	RuleReadMismatch      = "read_mismatch"
)

// ReadMismatch starts the error the client records for a read that found the
// row it reads back missing or different (client/Workload.java). Any other
// failed read failed to read, and is no data violation.
const ReadMismatch = "read mismatch:"

type clientSeq struct {
	client string
	seq    int
}

// ReadHistory parses history.jsonl and reduces it to one outcome per
// operation. An operation whose answer was never recorded has an unknown
// outcome. A history that contradicts itself is an error: it cannot be the
// basis of a verdict.
func ReadHistory(r io.Reader) (History, error) {
	h := History{Operations: map[string]Operation{}, Counts: map[string]int{}}
	answered := map[string]bool{}
	sequences := map[clientSeq]string{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			return History{}, fmt.Errorf("history line %d: %w", line, err)
		}
		if e.OpID == "" {
			return History{}, fmt.Errorf("history line %d: no operation ID", line)
		}
		switch e.Event {
		case Attempted:
			if _, seen := h.Operations[e.OpID]; seen {
				return History{}, fmt.Errorf("history line %d: operation %s was attempted twice", line, e.OpID)
			}
			key := clientSeq{e.Client, e.Seq}
			if other, seen := sequences[key]; seen {
				return History{}, fmt.Errorf("history line %d: client %s used sequence %d for %s and %s",
					line, e.Client, e.Seq, other, e.OpID)
			}
			sequences[key] = e.OpID
			h.Operations[e.OpID] = Operation{OpID: e.OpID, Client: e.Client, Seq: e.Seq, Op: e.Op,
				Amount: e.Amount, Note: e.Note, Target: e.Target, Outcome: Unknown}
		case Acknowledged, Failed, Unknown:
			op, seen := h.Operations[e.OpID]
			if !seen {
				return History{}, fmt.Errorf("history line %d: operation %s is %s but was never attempted",
					line, e.OpID, e.Event)
			}
			if answered[e.OpID] {
				return History{}, fmt.Errorf("history line %d: operation %s has more than one outcome", line, e.OpID)
			}
			answered[e.OpID] = true
			op.Outcome = e.Event
			op.Error = e.Error
			if e.Event == Acknowledged {
				op.AcknowledgedAt = e.T
			}
			h.Operations[e.OpID] = op
		default:
			return History{}, fmt.Errorf("history line %d: event %q is not attempted, acknowledged, failed or unknown",
				line, e.Event)
		}
	}
	if err := scanner.Err(); err != nil {
		return History{}, err
	}
	h.Counts[Attempted] = len(h.Operations)
	for _, op := range h.Operations {
		h.Counts[op.Outcome]++
	}
	return h, nil
}

// ReadOnlyRefusal is how the client records the error CUBRID returns for a
// write while updates are disabled, as the read-only Broker does
// (docs/poc/RESULTS.md, POC-8 and POC-22). The error comes from the Broker's
// CAS, so the client had reached the read-only Broker. The "code N:" form is
// the one Workload.describe in client/Workload.java writes.
const ReadOnlyRefusal = "code -581:"

// CheckReadOnlyRefusal judges a write sent through the read-only Service: the
// history holds exactly one operation, and it failed with ReadOnlyRefusal.
// Any other outcome, including a failure to reach the Broker at all, is not a
// refusal by the read-only Broker.
func CheckReadOnlyRefusal(h History) error {
	if len(h.Operations) != 1 {
		return fmt.Errorf("want one write through the read-only Service, got %d", len(h.Operations))
	}
	for _, op := range h.Operations {
		if op.Outcome != Failed {
			return fmt.Errorf("write %s through the read-only Service: outcome %s, want %s", op.OpID, op.Outcome, Failed)
		}
		if !strings.HasPrefix(op.Error, ReadOnlyRefusal) {
			return fmt.Errorf("write %s through the read-only Service failed with %q, not the read-only refusal %q",
				op.OpID, op.Error, ReadOnlyRefusal)
		}
	}
	return nil
}

// Check applies the data rules to the ledger rows read from each member or
// endpoint. An operation with an unknown outcome may be present or absent;
// it is counted and is never reported as missing.
func Check(h History, members map[string][]Row) Report {
	report := Report{Members: map[string]MemberCheck{}, Diverged: []Violation{}, ReadMismatches: readMismatches(h)}
	missing := map[string]bool{}
	present := map[string]map[string]Row{}
	for name, rows := range members {
		check, found := checkMember(h, rows)
		for _, v := range check.Violations {
			if v.Rule == RuleMissing {
				missing[v.OpID] = true
			}
		}
		report.Members[name] = check
		present[name] = found
	}
	report.AcknowledgedMissing = len(missing)
	report.Diverged = diverged(present)

	report.OK = len(members) > 0 && len(report.Diverged) == 0 && len(report.ReadMismatches) == 0
	for _, check := range report.Members {
		if len(check.Violations) > 0 {
			report.OK = false
		}
	}
	return report
}

// checkMember applies the rules that concern one member, and returns the
// rows it holds by operation ID.
func checkMember(h History, rows []Row) (MemberCheck, map[string]Row) {
	check := MemberCheck{Rows: len(rows), Violations: []Violation{}}
	found := map[string]Row{}
	sequences := map[clientSeq]string{}
	violate := func(rule, opID, detail string) {
		check.Violations = append(check.Violations, Violation{Rule: rule, OpID: opID, Detail: detail})
	}
	for _, row := range rows {
		if _, seen := found[row.OpID]; seen {
			violate(RuleAppliedTwice, row.OpID, "the operation ID is on more than one row")
			continue
		}
		found[row.OpID] = row
		op, known := h.Operations[row.OpID]
		switch {
		case !known && row.Note == RolledBackNote:
			violate(RuleRolledBackPresent, row.OpID, "a row with the rollback note exists")
		case !known:
			violate(RuleUnexpected, row.OpID, "no client operation has this ID")
		case op.Op == OpRead:
			violate(RuleUnexpected, row.OpID, "a read writes no row")
		case op.Op == OpRollback:
			violate(RuleRolledBackPresent, row.OpID, "the client rolled this change back")
		case op.Outcome == Failed:
			violate(RuleRolledBackPresent, row.OpID, "the client was told this operation failed")
		case row.Client != op.Client || row.Seq != op.Seq || row.Amount != op.Amount || row.Note != op.Note:
			violate(RuleWrongValues, row.OpID, fmt.Sprintf("sent %s/%d/%d/%q, found %s/%d/%d/%q",
				op.Client, op.Seq, op.Amount, op.Note, row.Client, row.Seq, row.Amount, row.Note))
		}
		key := clientSeq{row.Client, row.Seq}
		if other, seen := sequences[key]; seen {
			violate(RuleAppliedTwice, row.OpID, fmt.Sprintf("client %s sequence %d is also on %s", row.Client, row.Seq, other))
		} else {
			sequences[key] = row.OpID
		}
	}

	for _, id := range slices.Sorted(maps.Keys(h.Operations)) {
		op := h.Operations[id]
		if op.Op != OpInsert {
			continue
		}
		_, here := found[id]
		switch {
		case op.Outcome == Acknowledged && !here:
			violate(RuleMissing, id, "acknowledged to the client and not found")
		case op.Outcome == Unknown && here:
			check.UnknownPresent++
		case op.Outcome == Unknown:
			check.UnknownAbsent++
		}
	}
	return check, found
}

// readMismatches lists the reads that found the row they read back missing or
// different.
func readMismatches(h History) []Violation {
	out := []Violation{}
	for _, id := range slices.Sorted(maps.Keys(h.Operations)) {
		op := h.Operations[id]
		if op.Op == OpRead && op.Outcome == Failed && strings.HasPrefix(op.Error, ReadMismatch) {
			out = append(out, Violation{Rule: RuleReadMismatch, OpID: id, Detail: op.Error})
		}
	}
	return out
}

// diverged lists the operations whose row is not the same on every member.
func diverged(present map[string]map[string]Row) []Violation {
	out := []Violation{}
	names := slices.Sorted(maps.Keys(present))
	ids := map[string]bool{}
	for _, rows := range present {
		for id := range rows {
			ids[id] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		var with, without []string
		differ := false
		var first *Row
		for _, name := range names {
			row, here := present[name][id]
			if !here {
				without = append(without, name)
				continue
			}
			with = append(with, name)
			if first == nil {
				first = &row
			} else if row != *first {
				differ = true
			}
		}
		switch {
		case len(without) > 0:
			out = append(out, Violation{Rule: RuleMembersDiverge, OpID: id,
				Detail: "present on " + strings.Join(with, ", ") + "; absent on " + strings.Join(without, ", ")})
		case differ:
			out = append(out, Violation{Rule: RuleMembersDiverge, OpID: id, Detail: "the row's values differ between members"})
		}
	}
	return out
}

// ReadRows parses the ledger rows a client printed with "dump": one JSON
// object per line.
func ReadRows(r io.Reader) ([]Row, error) {
	rows := []Row{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var row Row
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&row); err != nil {
			return nil, fmt.Errorf("rows line %d: %w", line, err)
		}
		if row.OpID == "" {
			return nil, fmt.Errorf("rows line %d: no operation ID", line)
		}
		rows = append(rows, row)
	}
	return rows, scanner.Err()
}

// ReadEvents parses history.jsonl into its events, in the order they were
// written. It applies no rule; ReadHistory does.
func ReadEvents(r io.Reader) ([]Event, error) {
	events := []Event{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			return nil, fmt.Errorf("history line %d: %w", line, err)
		}
		events = append(events, e)
	}
	return events, scanner.Err()
}

// LossWindow is the observed data loss window of a fault (observed RPO in
// docs/testing/scenario-contract.md).
type LossWindow struct {
	// Measured is false when the history cannot show the window; Reason says
	// why.
	Measured bool
	Reason   string
	// Window is the time from the oldest acknowledged write that is missing
	// to the fault, and zero when none is missing.
	Window time.Duration
	// Resolution is how long before the fault the last write was
	// acknowledged: a loss of writes acknowledged within it could not be seen.
	Resolution time.Duration
}

func (l LossWindow) String() string {
	if !l.Measured {
		return "unknown: " + l.Reason
	}
	return l.Window.String()
}

// ObservedLoss measures the loss window of a fault issued at faultAt from the
// history and its data check (#294). It is measured only from acknowledged
// writes: it needs a member checked and a write acknowledged before the
// fault, and a missing write acknowledged after the fault leaves it unknown.
// Operations with an unknown outcome are never counted as lost.
func ObservedLoss(h History, r Report, faultAt time.Time) LossWindow {
	if len(r.Members) == 0 {
		return LossWindow{Reason: "no member was checked"}
	}
	var lastBefore time.Time
	for _, op := range h.Operations {
		at := op.AcknowledgedAt
		if op.Op == OpInsert && !at.IsZero() && !at.After(faultAt) && at.After(lastBefore) {
			lastBefore = at
		}
	}
	if lastBefore.IsZero() {
		return LossWindow{Reason: "no write was acknowledged before the fault"}
	}
	var oldest time.Time
	for _, check := range r.Members {
		for _, v := range check.Violations {
			if v.Rule != RuleMissing {
				continue
			}
			at := h.Operations[v.OpID].AcknowledgedAt
			if at.IsZero() {
				return LossWindow{Reason: "a missing write has no acknowledgement time"}
			}
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
			}
		}
	}
	loss := LossWindow{Measured: true, Resolution: faultAt.Sub(lastBefore)}
	if oldest.IsZero() {
		return loss
	}
	if oldest.After(faultAt) {
		return LossWindow{Reason: "the oldest missing write was acknowledged after the fault"}
	}
	loss.Window = faultAt.Sub(oldest)
	return loss
}

// Recovery reads from the events of one client when it recovered from a
// fault issued at faultAt (#294). Only a committed write (an acknowledged
// insert) and, for a client that reads at all, a verified read (an
// acknowledged read) are progress; an acknowledged rollback committed nothing
// and is neither progress nor an interruption. Any failed or unknown outcome
// is an interruption. recoveredAt is the first time after the last
// interruption by which the client had a committed write and, if it reads, a
// verified read; stable is how long it went on progressing after that. ok is
// false when the client did not recover in that sense, including when its
// last answer after the fault was a failure or unknown.
func Recovery(events []Event, faultAt time.Time) (recoveredAt time.Time, stable time.Duration, ok bool) {
	reads := slices.ContainsFunc(events, func(e Event) bool { return e.Op == OpRead })
	var wrote, read bool
	var lastProgress time.Time
	for _, e := range events {
		if !e.T.After(faultAt) {
			continue
		}
		switch {
		case e.Event == Failed || e.Event == Unknown:
			wrote, read, ok = false, false, false
		case e.Event == Acknowledged && (e.Op == OpInsert || e.Op == OpRead):
			wrote = wrote || e.Op == OpInsert
			read = read || e.Op == OpRead
			lastProgress = e.T
			if !ok && wrote && (read || !reads) {
				recoveredAt, ok = e.T, true
			}
		}
	}
	if !ok {
		return time.Time{}, 0, false
	}
	return recoveredAt, lastProgress.Sub(recoveredAt), true
}
