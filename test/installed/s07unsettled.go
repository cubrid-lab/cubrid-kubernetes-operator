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
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// s07SnapshotBound bounds the read of every member's rows when the cluster
// did not settle, so that the run namespace is still deleted within the
// run's deadline. It takes the place of the data check, which is not made
// then, in the bounds of the variant's steps.
var s07SnapshotBound = 2 * time.Minute

// Why the settle wait ended without the cluster settling: its own bound was
// reached, or the run's context ended (a signal, the run's timeout or a lost
// Lease) before it.
const (
	s07SettleBoundReached = "settle bound reached"
	s07SettleRunEnded     = "run ended"
)

// s07StateLength is the longest text, in characters, kept of an answer that
// names no state.
const s07StateLength = 160

var (
	// heartbeatNodeState finds each node's own line in cubrid heartbeat
	// status, as in "HA-Node Info (current ha-0, state to-be-master)".
	heartbeatNodeState = regexp.MustCompile(`current (\S+), state ([^)\s]+)\)`)
	// heartbeatServerState finds each server process's state, as in
	// "Server appdb (pid 72, state registered_and_active)".
	heartbeatServerState = regexp.MustCompile(`(?m)^\s*Server \S+ \(pid \d+, state ([^)\s]+)\)`)
)

// s07SettleExpiry is what the run last saw when the wait for the cluster to
// settle after the heal ended without it: each member's role and raw state,
// the last status and reason of each condition the run follows, and the
// quarantine in force.
type s07SettleExpiry struct {
	At         time.Time         `json:"at"`
	Cause      string            `json:"cause"`
	Roles      map[string]string `json:"roles"`
	States     map[string]string `json:"states,omitempty"`
	Conditions map[string]string `json:"conditions,omitempty"`
	Quarantine string            `json:"quarantineInForce,omitempty"`
}

// s07RowSnapshot is the read of every member's rows, directly and not
// through a Broker, made when the cluster did not settle. Diverged counts the
// operations whose row is not the same on every member that was read, when
// two or more were; File holds the full per-member report.
type s07RowSnapshot struct {
	At       time.Time                `json:"at"`
	Members  map[string]s07MemberRows `json:"members"`
	Diverged *int                     `json:"divergedAmongRead,omitempty"`
	Error    string                   `json:"error,omitempty"`
	File     string                   `json:"file,omitempty"`
}

// s07MemberRows is one member in the snapshot: the rows it holds, the
// acknowledged writes it lacks, and the operations whose rows contradict the
// run's history (a failed or rolled-back write present, unknown to any
// client, applied twice or with other values), each counted once. A member that could not be read has
// Read false, the error, and no count: unreadable, never zero.
type s07MemberRows struct {
	Read                bool   `json:"read"`
	Error               string `json:"error,omitempty"`
	Rows                *int   `json:"rows,omitempty"`
	AcknowledgedMissing *int   `json:"acknowledgedMissing,omitempty"`
	Conflicting         *int   `json:"conflicting,omitempty"`
}

// heartbeatState is a member's own state in cubrid heartbeat status, in
// CUBRID's words: the node state of "current <pod>, state <state>" and, when
// server processes are listed, the state of each, as in "to-be-master
// (server registered_and_to_be_active)". An answer without the member's node
// line is named by its first line.
func heartbeatState(pod, out string) string {
	node := ""
	for _, m := range heartbeatNodeState.FindAllStringSubmatch(out, -1) {
		if m[1] == pod {
			node = m[2]
			break
		}
	}
	if node == "" {
		return "no node state: " + s07Short(cmpOr(firstLine(out), "no output"))
	}
	var servers []string
	for _, m := range heartbeatServerState.FindAllStringSubmatch(out, -1) {
		servers = append(servers, m[1])
	}
	if len(servers) > 0 {
		return node + " (server " + strings.Join(servers, ", ") + ")"
	}
	return node
}

// s07Short keeps the first s07StateLength characters of s.
func s07Short(s string) string {
	if r := []rune(s); len(r) > s07StateLength {
		return string(r[:s07StateLength]) + "..."
	}
	return s
}

// s07StateList names each member's raw state, in the members' order.
func s07StateList(states map[string]string, members []string) string {
	named := make([]string, 0, len(members))
	for _, m := range members {
		named = append(named, m+": "+cmpOr(states[m], "not read"))
	}
	return strings.Join(named, "; ")
}

// noteS07SettleExpired records what the run last saw when the settle wait
// ended without the cluster settling, and why it ended: its bound reached,
// or the run's end (ctx done).
func noteS07SettleExpired(ctx context.Context, rec *s07Record, now time.Time) {
	e := &s07SettleExpiry{At: now, Cause: s07SettleBoundReached, Conditions: map[string]string{},
		Quarantine: rec.QuarantineInForce}
	kind := "settle_wait_expired"
	if ctx.Err() != nil {
		e.Cause, kind = s07SettleRunEnded, "settle_wait_ended_by_run"
	}
	if n := len(rec.Sweeps); n > 0 {
		e.Roles, e.States = rec.Sweeps[n-1].Roles, rec.Sweeps[n-1].States
	}
	for _, c := range rec.Conditions {
		e.Conditions[c.Type] = c.Status + "/" + c.Reason
	}
	rec.SettleExpired = e
	rec.note(kind, rec.SettleUnmet)
}

// snapshotS07Rows reads, within s07SnapshotBound, every member's rows and
// applies the data rules of the run's history to each member on its own. It
// is made when the cluster did not settle, so that what the members hold is
// kept before the namespace is deleted; it is reported and never judged.
//
// The members are read one after another, as the run's csql reads keep
// shared counters, and each gets an equal share of what is left of the
// bound: a member that hangs, as one stuck in to-be-master may, cannot take
// the time of the members read after it, and time a member does not use
// passes to the next.
func (h *haRun) snapshotS07Rows(ctx context.Context, rec *s07Record) {
	rec.UnsettledRows, _ = h.readS07Rows(ctx, h.history.String(), s07File(rec.Variant, "unsettled-rows.json"))
}

// readS07Rows reads every member's rows as snapshotS07Rows does, applies the
// data rules of historyText to each member on its own, and writes the full
// report to file. It returns the snapshot, and the report when the history
// could be read.
func (h *haRun) readS07Rows(ctx context.Context, historyText, file string) (*s07RowSnapshot, *workload.Report) {
	ctx, cancel := context.WithTimeout(ctx, s07SnapshotBound)
	defer cancel()
	s := &s07RowSnapshot{At: time.Now().UTC(), Members: map[string]s07MemberRows{}}
	rows := map[string][]workload.Row{}
	deadline, _ := ctx.Deadline()
	for i, pod := range h.members {
		memberCtx, cancelMember := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(h.members)-i))
		read, err := h.readRowsOf(memberCtx, dataTarget{members: []string{pod}})
		cancelMember()
		if err != nil {
			s.Members[pod] = s07MemberRows{Error: err.Error()}
			continue
		}
		rows[pod] = read[pod]
	}
	history, err := workload.ReadHistory(strings.NewReader(historyText))
	if err != nil {
		s.Error = "read the run's history: " + err.Error()
		for pod, r := range rows {
			n := len(r)
			s.Members[pod] = s07MemberRows{Read: true, Rows: &n}
		}
		return s, nil
	}
	report := workload.Check(history, rows)
	for pod, check := range report.Members {
		n, missing, conflicting := check.Rows, 0, map[string]bool{}
		for _, v := range check.Violations {
			if v.Rule == workload.RuleMissing {
				missing++
			} else {
				conflicting[v.OpID] = true
			}
		}
		c := len(conflicting)
		s.Members[pod] = s07MemberRows{Read: true, Rows: &n, AcknowledgedMissing: &missing, Conflicting: &c}
	}
	if len(rows) >= 2 {
		d := len(report.Diverged)
		s.Diverged = &d
	}
	if err := h.writeJSON(file, report); err != nil {
		s.Error = "write " + file + ": " + err.Error()
		return s, &report
	}
	s.File = file
	return s, &report
}

// s07SnapshotNote reports the snapshot of the members' rows for the reason
// of a run that did not settle, or "".
func s07SnapshotNote(rec *s07Record) string {
	s := rec.UnsettledRows
	if s == nil {
		return ""
	}
	var named []string
	for _, m := range slices.Sorted(maps.Keys(s.Members)) {
		r := s.Members[m]
		switch {
		case !r.Read:
			named = append(named, fmt.Sprintf("%s unreadable (%s)", m, s07Short(firstLine(r.Error))))
		case r.AcknowledgedMissing == nil:
			named = append(named, fmt.Sprintf("%s holds %d row(s)", m, *r.Rows))
		default:
			named = append(named, fmt.Sprintf("%s holds %d row(s), lacks %d acknowledged write(s), %d conflicting operation(s)",
				m, *r.Rows, *r.AcknowledgedMissing, *r.Conflicting))
		}
	}
	note := "; rows read before cleanup, reported and not judged: " + strings.Join(named, "; ")
	if s.Error != "" {
		note += " (" + s.Error + ")"
	}
	return note
}
