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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// Reasons and values the S07 verdict tests share.
const (
	s07PassReason      = "quarantine"
	s07OneGeneration   = "generation-1"
	s07NoEvents        = "0 event(s)"
	s07IncompleteEvent = "incomplete event"
)

// s07SessionFault is when the fault is issued in these tests, on the client's
// clock, and s07SessionClosed when the run saw the -rw path closed.
var (
	s07SessionFault  = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	s07SessionClosed = s07SessionFault.Add(30 * time.Second)
)

// s07SessionEvent is one event of client's operation seq, sec seconds from
// the fault (negative before it).
func s07SessionEvent(client string, seq, sec int, kind string) string {
	t := s07SessionFault.Add(time.Duration(sec) * time.Second)
	return fmt.Sprintf(`{"t":"%s","client":"%s","seq":%d,"op":"insert","opId":"%s-%d","event":"%s",`+
		`"amount":1,"note":"n"}`+"\n", t.Format(time.RFC3339), client, seq, client, seq, kind)
}

// s07SessionAcked is n operations of s07OpenClient, each attempted and
// acknowledged, the first at sec seconds from the fault and one a second
// after another, numbered from first.
func s07SessionAcked(first, n, sec int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(s07SessionEvent(s07OpenClient, first+i, sec+i, workload.Attempted))
		b.WriteString(s07SessionEvent(s07OpenClient, first+i, sec+i, workload.Acknowledged))
	}
	return b.String()
}

// readS07SessionFrom has readS07Session read text as the established
// client's history.
func readS07SessionFrom(t *testing.T, text string) (*haRun, *s07Record) {
	t.Helper()
	h := s07SnapshotRun(t)
	h.rec.Client = &imageIdentity{Pod: clientPodName, Node: testClientNode}
	h.Command = func(_ context.Context, args ...string) (string, error) {
		if strings.Contains(args[len(args)-1], "cat /tmp/"+s07OpenClient+".jsonl") {
			return text, nil
		}
		return "", nil
	}
	rec := &s07Record{Variant: s07MasterIsolated}
	h.readS07Session(context.Background(), rec, s07OpenClient, s07SessionFault, s07SessionClosed)
	return h, rec
}

// An established-session history that does not show the session, complete
// and consistent, from before the fault blocks S07 with a reason naming what
// is missing; none of these histories may let S07 pass (#1086).
func TestReadS07SessionRejectsHistoriesThatProveNoSession(t *testing.T) {
	established := s07SessionAcked(1, s06Acknowledged, -60)
	tests := map[string]struct {
		text   string
		reason string
	}{
		"an empty file":                      {"", "holds no complete event"},
		"only whitespace":                    {" \n\n\t\n", "holds no complete event"},
		"one event without its newline":      {strings.TrimSuffix(s07SessionAcked(1, 1, -60), "\n"), s07IncompleteEvent},
		"a history without its last newline": {strings.TrimSuffix(established, "\n"), s07IncompleteEvent},
		"a truncated last event":             {established + `{"t":"2026-11-17T12:00:0`, s07IncompleteEvent},
		"a malformed line":                   {established + "not json\n", "could not be read"},
		"another client's history": {strings.ReplaceAll(established, `"client":"`+s07OpenClient+`"`,
			`"client":"s07n1"`), `of client "s07n1", not s07open`},
		"one event of another client": {established + s07SessionEvent("s07n1", 99, 1, workload.Attempted),
			`of client "s07n1"`},
		"only attempts": {s07SessionEvent(s07OpenClient, 1, -1, workload.Attempted),
			"0 acknowledged operation(s) before the fault"},
		"an outcome never attempted": {established + s07SessionEvent(s07OpenClient, 99, 1, workload.Acknowledged),
			"never attempted"},
		"two outcomes of one operation": {established +
			s07SessionEvent(s07OpenClient, 1, 1, workload.Acknowledged), "more than one outcome"},
		"an operation started before the previous one was answered": {established +
			s07SessionEvent(s07OpenClient, 20, 1, workload.Attempted) +
			s07SessionEvent(s07OpenClient, 21, 2, workload.Attempted), "started before " + s07OpenClient + "-20"},
		"an event after the session ended": {established +
			s07SessionEvent(s07OpenClient, 20, 1, workload.Attempted) +
			s07SessionEvent(s07OpenClient, 20, 2, workload.Failed) +
			s07SessionEvent(s07OpenClient, 21, 3, workload.Attempted), "follows the session's end"},
		"too few acknowledgements before the fault": {s07SessionAcked(1, s06Acknowledged-1, -60) +
			s07SessionAcked(s06Acknowledged, 1, 5),
			fmt.Sprintf("had %d acknowledged operation(s) before the fault", s06Acknowledged-1)},
		"a session that ended before the fault": {established +
			s07SessionEvent(s07OpenClient, 20, -5, workload.Attempted) +
			s07SessionEvent(s07OpenClient, 20, -4, workload.Failed), "ended before the fault"},
		"a client that went silent before the fault": {established, "is before the fault: the client stopped"},
		"an operation left unanswered long before the fault": {established +
			s07SessionEvent(s07OpenClient, 20, -50, workload.Attempted), "is before the fault: the client stopped"},
		"acknowledgements only after the fault": {s07SessionAcked(1, s06Acknowledged, 1),
			"0 acknowledged operation(s) before the fault"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, rec := readS07SessionFrom(t, tc.text)
			problem := s07SessionProblem(rec.Session)
			if !strings.Contains(problem, tc.reason) {
				t.Fatalf("session %+v: problem %q, want one naming %q", rec.Session, problem, tc.reason)
			}
			for _, control := range []bool{false, true} {
				r := s07Base(control)
				r.Session = rec.Session
				if v := judgeS07(r, testS07Limits); v.Result != evidence.Blocked || !strings.Contains(v.Reason, tc.reason) {
					t.Errorf("control %t judged %s %q, want blocked naming %q", control, v.Result, v.Reason, tc.reason)
				}
			}
		})
	}
}

// An interrupted collector's file is kept as read, its last event included,
// beside the record, and only its complete events reach the run's history.
func TestReadS07SessionKeepsAnIncompleteHistory(t *testing.T) {
	text := s07SessionAcked(1, s06Acknowledged, -60) + `{"t":"2026-11-17T12:00:0`
	h, rec := readS07SessionFrom(t, text)
	s := rec.Session
	if s.IncompleteTailBytes != len(`{"t":"2026-11-17T12:00:0`) || s.RawFile == "" ||
		s.Events != 2*s06Acknowledged {
		t.Fatalf("session %+v, want the incomplete tail counted, the raw file kept and the complete events read", s)
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.EvidenceDir, s.RawFile))
	if err != nil || string(raw) != text {
		t.Fatalf("raw file %q (%v), want the history as read", raw, err)
	}
	if strings.Contains(h.history.String(), `"t":"2026-11-17T12:00:0`+"\n") ||
		!strings.HasSuffix(h.history.String(), "\n") {
		t.Errorf("run history %q, want only complete events", h.history.String())
	}
}

// A complete history of the established session is judged on what it shows:
// no acknowledgement after closure passes on the limits, one while the same
// quarantine stayed in force fails, and one with a release seen stays
// inconclusive. A session that ended with an error, or was stopped during an
// operation, is still a consistent history.
func TestReadS07SessionJudgesACompleteHistory(t *testing.T) {
	established := s07SessionAcked(1, s06Acknowledged, -60)
	afterFault := s07SessionAcked(s06Acknowledged+1, 2, 5)
	tests := map[string]struct {
		text    string
		change  func(*s07Record)
		result  evidence.Result
		reason  string
		session func(*s07Session) bool
	}{
		"open at the end with nothing acknowledged after closure": {established + afterFault, nil,
			evidence.Pass, s07PassReason, func(s *s07Session) bool {
				return s.OpenAtEnd && s.AcknowledgedAfterFault == 2 && s.AcknowledgedAfterClosed == 0
			}},
		"stopped during an operation": {established + s07SessionEvent(s07OpenClient, 20, -1, workload.Attempted),
			nil, evidence.Pass, s07PassReason, func(s *s07Session) bool { return s.OpenAtEnd }},
		"ended by an error after the fault": {established +
			s07SessionEvent(s07OpenClient, 20, 5, workload.Attempted) +
			s07SessionEvent(s07OpenClient, 20, 6, workload.Unknown), nil, evidence.Pass, s07PassReason,
			func(s *s07Session) bool { return !s.OpenAtEnd && strings.HasPrefix(s.EndedBy, "unknown") }},
		"an acknowledgement after closure while one quarantine generation stayed in force": {
			established + s07SessionAcked(s06Acknowledged+1, 1, 40),
			func(r *s07Record) { r.Generations = []string{s07OneGeneration} }, evidence.Fail,
			"without an accepted release", func(s *s07Session) bool { return s.AcknowledgedAfterClosed == 1 }},
		"an acknowledgement after closure with a release seen": {
			established + s07SessionAcked(s06Acknowledged+1, 1, 40), func(r *s07Record) {
				v := at(*r.FaultIssuedAt, 150)
				r.ReleaseSeenAt = &v
			}, evidence.Blocked, "prevents attributing them",
			func(s *s07Session) bool { return s.AcknowledgedAfterClosed == 1 }},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, rec := readS07SessionFrom(t, tc.text)
			if p := s07SessionProblem(rec.Session); p != "" || !tc.session(rec.Session) {
				t.Fatalf("session %+v, problem %q", rec.Session, p)
			}
			r := s07Base(false)
			r.Session = rec.Session
			if tc.change != nil {
				tc.change(r)
			}
			if v := judgeS07(r, testS07Limits); v.Result != tc.result || !strings.Contains(v.Reason, tc.reason) {
				t.Errorf("judged %s %q, want %s with %q", v.Result, v.Reason, tc.result, tc.reason)
			}
		})
	}
}
