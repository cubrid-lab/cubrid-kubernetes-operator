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
	// Amount and Note are the values sent; they are on the attempted line.
	Amount int    `json:"amount,omitempty"`
	Note   string `json:"note,omitempty"`
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
	OpID    string
	Client  string
	Seq     int
	Op      string
	Amount  int
	Note    string
	Outcome string
	// Error is what the client recorded with a failed or unknown outcome.
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
}

// Data rules, as they appear in a Violation.
const (
	RuleMissing           = "missing"
	RuleWrongValues       = "wrong_values"
	RuleAppliedTwice      = "applied_twice"
	RuleRolledBackPresent = "rolled_back_present"
	RuleUnexpected        = "unexpected"
	RuleMembersDiverge    = "members_diverge"
)

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
				Amount: e.Amount, Note: e.Note, Outcome: Unknown}
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
// CAS, so the client had reached the read-only Broker.
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
	report := Report{Members: map[string]MemberCheck{}, Diverged: []Violation{}}
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

	report.OK = len(members) > 0 && len(report.Diverged) == 0
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

// Recovery reads from the events of one client when it recovered from a
// fault issued at faultAt. recoveredAt is the first acknowledged operation
// after which no operation failed or had an unknown outcome, and stable is
// how long the client went on being acknowledged after it. ok is false when
// the last answer the client received after the fault was not an
// acknowledgement, or when it received none.
func Recovery(events []Event, faultAt time.Time) (recoveredAt time.Time, stable time.Duration, ok bool) {
	var lastAcknowledged time.Time
	for _, e := range events {
		if !e.T.After(faultAt) {
			continue
		}
		switch e.Event {
		case Acknowledged:
			if !ok {
				recoveredAt, ok = e.T, true
			}
			lastAcknowledged = e.T
		case Failed, Unknown:
			ok = false
		}
	}
	if !ok {
		return time.Time{}, 0, false
	}
	return recoveredAt, lastAcknowledged.Sub(recoveredAt), true
}
