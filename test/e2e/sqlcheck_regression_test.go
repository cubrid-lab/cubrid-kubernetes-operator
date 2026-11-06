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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// TestSQLChecks preserves the existing Kind helpers' behavior when they forward
// to the installation-independent checks. These cases passed before extraction.
func TestSQLChecks(t *testing.T) {
	var script string
	if err := writeS00Row(func(sql string) (string, error) {
		script = sql
		return "committed", nil
	}, "marker-123"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "INSERT INTO "+s00Table+" (id, marker) SELECT 1, 'marker-123' FROM db_root") {
		t.Fatalf("unexpected write SQL: %q", script)
	}
	if err := readS00Row(func(sql string) (string, error) {
		if sql != "SELECT id, marker FROM "+s00Table+" ORDER BY id;" {
			t.Errorf("unexpected read SQL: %q", sql)
		}
		return "=== <Result of SELECT Command in Line 1> ===\n\n  id  marker\n=====\n  1  'marker-123'\n\n" +
			"1 row selected.\n", nil
	}, "marker-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := runSQL(func(string) (string, error) {
		return "ERROR: statement failed", nil
	}, "SELECT 1;"); err == nil {
		t.Fatal("accepted csql error output")
	}
	if err := readS00Row(func(string) (string, error) {
		return "1 'other'\n1 row selected", nil
	}, "marker-123"); err == nil {
		t.Fatal("accepted wrong marker")
	}
}

// s00TestMarker is the marker of cleanRead and lostSession.
const s00TestMarker = "s00-before-pod-delete"

// cleanRead is csql's output of a read of S00's marker row without an error.
const cleanRead = "=== <Result of SELECT Command in Line 1> ===\n\n" +
	"           id  marker              \n===================================\n" +
	"            1  's00-before-pod-delete'\n\n" +
	"1 row selected. (0.001000 sec) Committed. (0.000000 sec) \n"

// lostSession is csql's output of the read at the end of the smoke checks in
// the Kind e2e run 41986930319 of PR #698, as its log printed it.
const lostSession = "\n" +
	"Time: 11/05/26 08:08:58.431 - NOTIFICATION *** file " +
	"/home/jenkins/workspace/cubrid_release_11.4/src/transaction/boot_cl.c, line 1085  " +
	"CODE = -971, Tran = -1, EID = 1\n" +
	"Program 'csql' (pid 376) connected to database server 'appdb@localhost' on the host 'localhost' (port 1523).\n" +
	"\n" +
	"Time: 11/05/26 08:08:58.445 - ERROR *** file " +
	"/home/jenkins/workspace/cubrid_release_11.4/src/communication/network_cl.c, line 177 " +
	"ERROR CODE = -199, Tran = 3, EID = 2\n" +
	"Server no longer responding.... Invalid argument\n" +
	"\n" +
	"Time: 11/05/26 08:08:58.445 - ERROR *** file " +
	"/home/jenkins/workspace/cubrid_release_11.4/src/transaction/transaction_cl.c, line 600 " +
	"ERROR CODE = -111, Tran = 3, EID = 3\n" +
	"Your transaction has been aborted by the system due to server failure or mode change.\n" +
	"\n" +
	"=== <Result of SELECT Command in Line 1> ===\n" +
	"\n" +
	"           id  marker              \n" +
	"===================================\n" +
	"            1  's00-before-pod-delete'\n" +
	"\n" +
	"1 row selected. (0.001000 sec) Committed. (0.000000 sec) \n"

// TestSmokeOutcome checks that a csql operation is recorded as failed only
// when csql reported the statement as failed in the Pod; a row that does not
// match and a command that could not be run are recorded with their own
// reason instead (S00). A read whose output holds its row together with
// -199 and -111, the pair POC-44 saw csql print after statements that took
// effect, is recorded with a reason that names POC-44; a -199 without the
// row, and a write that printed the pair, stay failed.
func TestSmokeOutcome(t *testing.T) {
	const marker = s00TestMarker
	read := func(out string) error {
		return readS00Row(func(string) (string, error) { return out, nil }, marker)
	}
	statement := func() error { return read("ERROR: no table") }
	alone := strings.Replace(lostSession, "ERROR CODE = -111", "CODE = -971", 1)
	noRow, _, _ := strings.Cut(lostSession, "=== <Result")
	for name, tc := range map[string]struct {
		err        error
		readMarker string
		ok         bool
		reason     string
	}{
		"no error":        {nil, marker, true, ""},
		"statement error": {statement(), marker, false, ""},
		"wrong row": {read(strings.ReplaceAll(cleanRead, marker, "other")), marker, false,
			"did not show a failed statement"},
		"transport error": {readS00Row(func(string) (string, error) { return "", errors.New("transport") }, "m"),
			"m", false, "did not show a failed statement"},
		"wrapped statement error":       {fmt.Errorf("check: %w", statement()), marker, false, ""},
		"the read of the Kind run":      {read(lostSession), marker, false, "POC-44"},
		"-199 alone with the row":       {read(alone), marker, false, ""},
		"-199 and -111 without the row": {read(noRow), marker, false, ""},
		"a write that printed the pair": {read(lostSession), "", false, ""},
		"the pair after a failed command": {readS00Row(func(string) (string, error) {
			return lostSession, errors.New("exit status 1")
		}, marker), marker, false, ""},
	} {
		ok, reason := smokeOutcome(tc.err, tc.readMarker)
		if ok != tc.ok || (reason == "") != (tc.reason == "") || !strings.Contains(reason, tc.reason) {
			t.Errorf("%s: smokeOutcome = %v, %q; want ok %v and a reason with %q", name, ok, reason, tc.ok,
				tc.reason)
		}
	}
}

// TestS00WriteAndReadAreStrict checks that the csql write and the first read
// of S00 stop the step on any error: a read that printed its row with -199
// and -111 is recorded as inconclusive and still fails the step, as it did
// before S00 compared its Conditions with SQL, and only the read at the end
// of the smoke checks reads again.
func TestS00WriteAndReadAreStrict(t *testing.T) {
	const pod = "s00-0"
	for name, tc := range map[string]struct {
		write, read  string
		wantOps      int
		wantErr      bool
		wantLastOK   bool
		wantLastNote string
	}{
		"both succeed":               {"Committed.", cleanRead, 2, false, true, ""},
		"the read printed the pair":  {"Committed.", lostSession, 2, true, false, s00LostSession},
		"the read failed":            {"Committed.", "ERROR: no table", 2, true, false, ""},
		"the write failed":           {"ERROR: read only", cleanRead, 1, true, false, ""},
		"the write printed the pair": {lostSession, cleanRead, 1, true, false, ""},
	} {
		calls := 0
		ops, err := s00WriteAndRead(func(string) (string, error) {
			calls++
			if calls == 1 {
				return tc.write, nil
			}
			return tc.read, nil
		}, pod, s00TestMarker)
		if (err != nil) != tc.wantErr || len(ops) != tc.wantOps || calls != tc.wantOps {
			t.Errorf("%s: %d operations in %d csql calls, error %v; want %d, error %v", name, len(ops), calls, err,
				tc.wantOps, tc.wantErr)
			continue
		}
		last := ops[len(ops)-1]
		if last.OK != tc.wantLastOK || last.Inconclusive != tc.wantLastNote {
			t.Errorf("%s: the last operation is %+v, want OK %v and reason %q", name, last, tc.wantLastOK,
				tc.wantLastNote)
		}
		if !ops[0].Write || (len(ops) == 2 && ops[1].Write) {
			t.Errorf("%s: operations %+v are not a write and then a read", name, ops)
		}
	}
}

// s00Answer is what one csql call returned.
type s00Answer struct {
	out string
	err error
}

// TestS00LastRead checks the read at the end of the smoke checks and what S00
// ends with after it. A read that printed its row with -199 and -111 is never
// conclusive, so it is read once more: a conclusive second read decides, and
// a second read that is inconclusive leaves no conclusive last read, which
// blocks S00 however early Ready was True. An error of the first read that is
// not that pair fails the step, as does a statement that failed on the
// second read.
func TestS00LastRead(t *testing.T) {
	wrongRow := strings.ReplaceAll(cleanRead, s00TestMarker, "other")
	noRow, _, _ := strings.Cut(lostSession, "=== <Result")
	transport := errors.New("the command could not be run")
	const failedReason, otherReason = "", "the check did not show a failed statement"
	for name, tc := range map[string]struct {
		answers []s00Answer
		// reasons are the recorded reasons of the reads, which are as many as
		// the csql calls wanted; a read succeeded when its reason is "ok".
		reasons []string
		stepErr bool
		result  evidence.Result
		lost    int
	}{
		"a conclusive read": {
			answers: []s00Answer{{cleanRead, nil}}, reasons: []string{"ok"}, result: evidence.Pass},
		"the pair, then a conclusive read": {
			answers: []s00Answer{{lostSession, nil}, {cleanRead, nil}},
			reasons: []string{s00LostSession, "ok"}, result: evidence.Pass, lost: 1},
		"the pair twice": {
			answers: []s00Answer{{lostSession, nil}, {lostSession, nil}},
			reasons: []string{s00LostSession, s00LostSession}, result: evidence.Blocked, lost: 2},
		"the pair, then a statement that failed": {
			answers: []s00Answer{{lostSession, nil}, {"ERROR: no table", nil}},
			reasons: []string{s00LostSession, failedReason}, stepErr: true, result: evidence.Fail, lost: 1},
		"the pair, then the pair without the row": {
			answers: []s00Answer{{lostSession, nil}, {noRow, nil}},
			reasons: []string{s00LostSession, failedReason}, stepErr: true, result: evidence.Fail, lost: 1},
		"the pair, then a csql that could not be run": {
			answers: []s00Answer{{lostSession, nil}, {"", transport}},
			reasons: []string{s00LostSession, otherReason}, stepErr: true, result: evidence.Fail, lost: 1},
		"the pair, then another row": {
			answers: []s00Answer{{lostSession, nil}, {wrongRow, nil}},
			reasons: []string{s00LostSession, otherReason}, stepErr: true, result: evidence.Fail, lost: 1},
		"a statement that failed first": {
			answers: []s00Answer{{"ERROR: no table", nil}},
			reasons: []string{failedReason}, stepErr: true, result: evidence.Fail},
		"-199 alone with the row": {
			answers: []s00Answer{{strings.Replace(lostSession, "ERROR CODE = -111", "CODE = -971", 1), nil}},
			reasons: []string{failedReason}, stepErr: true, result: evidence.Fail},
		"the pair after a failed command": {
			answers: []s00Answer{{lostSession, errors.New("exit status 1")}},
			reasons: []string{failedReason}, stepErr: true, result: evidence.Fail},
		"another row first": {
			answers: []s00Answer{{wrongRow, nil}},
			reasons: []string{otherReason}, stepErr: true, result: evidence.Fail},
		"a csql that could not be run first": {
			answers: []s00Answer{{"", transport}},
			reasons: []string{otherReason}, stepErr: true, result: evidence.Fail},
	} {
		t.Run(name, func(t *testing.T) {
			useEvidenceDir(t, t.TempDir())
			calls := 0
			reads, err := s00LastRead(func(sql string) (string, error) {
				if sql != "SELECT id, marker FROM "+s00Table+" ORDER BY id;" {
					t.Errorf("unexpected SQL: %q", sql)
				}
				calls++
				if calls > len(tc.answers) {
					t.Fatalf("csql call %d, want %d", calls, len(tc.answers))
				}
				return tc.answers[calls-1].out, tc.answers[calls-1].err
			}, "s00-0", s00TestMarker)
			if calls != len(tc.reasons) || len(reads) != len(tc.reasons) {
				t.Fatalf("%d csql calls and %d records %+v, want %d", calls, len(reads), reads, len(tc.reasons))
			}
			if (err != nil) != tc.stepErr {
				t.Errorf("the error of the step is %v, want an error: %v", err, tc.stepErr)
			}
			for i, read := range reads {
				want := tc.reasons[i]
				if read.Write || read.Broker || read.OK != (want == "ok") ||
					(want != "ok" && !strings.Contains(read.Inconclusive, want)) ||
					(want == "" && read.Inconclusive != "") || (want == "ok" && read.Inconclusive != "") {
					t.Errorf("read %d is recorded as %+v, want the reason %q", i, read, want)
				}
			}
			rounds, run := s00Fixture()
			for i := range reads {
				second := time.Duration(54+2*i) * time.Second
				reads[i].Start, reads[i].End = rounds[0].Status.ReadStart.Add(second),
					rounds[0].Status.ReadStart.Add(second+time.Second)
			}
			run.SQL = append(run.SQL, reads...)
			ended := evidence.Scenario{ID: "S00", Result: evidence.Pass}
			if err != nil {
				ended = evidence.Scenario{ID: "S00", Result: evidence.Fail, Reason: "a step of the scenario failed"}
			}
			got := s00Conditions(ended, rounds, run, nil)
			if got.Result != tc.result {
				t.Errorf("S00 ended %s (%s), want %s", got.Result, got.Reason, tc.result)
			}
			if tc.result == evidence.Blocked && !strings.HasPrefix(got.Reason, reasonConditionsNotJudged) {
				t.Errorf("S00 was blocked with the reason %q, want %s", got.Reason, reasonConditionsNotJudged)
			}
			if lost := got.Measurements["csqlLostSessionReads"]; lost != tc.lost {
				t.Errorf("csqlLostSessionReads = %v, want %d", lost, tc.lost)
			}
		})
	}
}
