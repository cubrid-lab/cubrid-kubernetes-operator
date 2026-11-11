//go:build e2e

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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
)

// freshLedger gives a test its own ledger, and returns it.
func freshLedger(t *testing.T) *lostSessionLedger {
	t.Helper()
	previous := csqlLedger
	csqlLedger = newLostSessionLedger()
	t.Cleanup(func() { csqlLedger = previous })
	return csqlLedger
}

// cannedKubectl answers csql commands in order, in place of utils.Run. A call
// beyond the answers fails the test.
func cannedKubectl(t *testing.T, answers ...s00Answer) (func(*exec.Cmd) (string, error), *int) {
	t.Helper()
	calls := 0
	return func(*exec.Cmd) (string, error) {
		calls++
		if calls > len(answers) {
			t.Errorf("csql call %d, want at most %d", calls, len(answers))
			return "", errors.New("too many calls")
		}
		return answers[calls-1].out, answers[calls-1].err
	}, &calls
}

// fakeMember is csql on a member with canned answers.
func fakeMember(run func(*exec.Cmd) (string, error), guard sqlcheck.Guard) csqlMember {
	return csqlMember{ctx: context.Background(), namespace: "ns", pod: "hab-0", database: "appdb", run: run, guard: guard}
}

// lostRecords reads the ledger's file from dir.
func lostRecords(t *testing.T, dir string) []lostSessionRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, lostSessionsFile))
	if err != nil {
		t.Fatal(err)
	}
	var records []lostSessionRecord
	for line := range strings.Lines(string(data)) {
		var record lostSessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

// A retried read that a loop absorbs is counted like any other, with nothing
// to hide it: it passes on the next round and no identity was read.
func TestRetriedReadIsCountedOnly(t *testing.T) {
	ledger := freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	run, calls := cannedKubectl(t, s00Answer{out: lostSession}, s00Answer{out: cleanRead})
	sql := csqlRunner(context.Background(), "ns", "hab-1", "appdb", run)
	const query = "SELECT TO_CHAR(COUNT(*)) AS v\nFROM t;"
	if _, err := runSQL(sql, query); err == nil {
		t.Error("the runner of a retried read accepted the pair")
	}
	if _, err := runSQL(sql, query); err != nil {
		t.Error(err)
	}
	records := lostRecords(t, dir)
	if len(records) != 1 || *calls != 2 {
		t.Fatalf("%d calls, records %+v", *calls, records)
	}
	got := records[0]
	if got.At.IsZero() || got.Member != "hab-1" || got.Query != "SELECT TO_CHAR(COUNT(*)) AS v" ||
		got.Kind != "read" || got.Identity != sqlcheck.IdentityNotRead || got.ReRead != sqlcheck.ReReadNotMade {
		t.Errorf("the record is %+v", got)
	}
	if note := ledger.note(); !strings.Contains(note, "1 time(s)") || !strings.Contains(note, "#238") ||
		!strings.Contains(note, lostSessionsFile) {
		t.Errorf("the note is %q", note)
	}
}

// A statement of any other kind counts as a write and is never repeated, and
// a pair with a failed command or without a -111 is told apart at the count.
func TestRunnerCountsWritesAndOnlyThePair(t *testing.T) {
	freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	alone := strings.Replace(lostSession, "ERROR CODE = -111", "CODE = -971", 1)
	run, calls := cannedKubectl(t, s00Answer{out: lostSession}, s00Answer{out: alone},
		s00Answer{out: lostSession, err: errors.New("exit status 1")})
	sql := csqlRunner(context.Background(), "ns", "s00-0", "appdb", run)
	for _, statement := range []string{"SELECT 1; INSERT INTO t VALUES (1);", "SELECT 1;", "SELECT 2;"} {
		if _, err := runSQL(sql, statement); err == nil {
			t.Errorf("%s passed", statement)
		}
	}
	records := lostRecords(t, dir)
	if *calls != 3 || len(records) != 2 || records[0].Kind != "write" || records[1].Kind != "read" {
		t.Errorf("%d calls, records %+v", *calls, records)
	}
}

// A one-shot read that lost one session counts one, with what the rule found.
func TestOneShotReadRecordsThePair(t *testing.T) {
	ledger := freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	run, calls := cannedKubectl(t, s00Answer{out: lostSession}, s00Answer{out: cleanRead})
	result, err := fakeMember(run, sameServer()).read(sqlcheck.ReadRowQuery, sqlcheck.RowCheck(s00TestMarker))
	if err != nil || result.Out != cleanRead || *calls != 2 {
		t.Fatalf("%d calls, %+v, error %v", *calls, result, err)
	}
	records := lostRecords(t, dir)
	if len(records) != 1 || records[0].Identity != sqlcheck.IdentityUnchanged ||
		records[0].ReRead != sqlcheck.ReReadAnswered {
		t.Errorf("the records are %+v", records)
	}
	if ledger.note() == "" {
		t.Error("a pass with a pair left no note")
	}
}

// Two pairs are two occurrences and a typed error, never nil.
func TestOneShotReadPairTwice(t *testing.T) {
	freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	run, calls := cannedKubectl(t, s00Answer{out: lostSession}, s00Answer{out: lostSession})
	_, err := fakeMember(run, sameServer()).read(sqlcheck.ReadRowQuery, sqlcheck.RowCheck(s00TestMarker))
	var lost *sqlcheck.LostSessionError
	if !errors.As(err, &lost) || !lost.Twice || *calls != 2 {
		t.Fatalf("%d calls, error %v", *calls, err)
	}
	records := lostRecords(t, dir)
	if len(records) != 2 || records[0].ReRead != sqlcheck.ReReadLost || records[1].ReRead != reReadIsThisCall {
		t.Errorf("the records are %+v", records)
	}
}

// A changed server is in the record and fails the read: the record shows it
// though the second read would have answered.
func TestOneShotReadRecordsAChangedServer(t *testing.T) {
	freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	run, _ := cannedKubectl(t, s00Answer{out: lostSession}, s00Answer{out: cleanRead})
	reads := 0
	guard := sqlcheck.Guard{Identity: func() (string, error) {
		reads++
		if reads == 1 {
			return "cub_server 77 123456\n", nil
		}
		return "cub_server 90 124000\n", nil
	}}
	_, err := fakeMember(run, guard).read(sqlcheck.ReadRowQuery, sqlcheck.RowCheck(s00TestMarker))
	var lost *sqlcheck.LostSessionError
	if !errors.As(err, &lost) || lost.Identity != sqlcheck.IdentityChanged {
		t.Fatalf("the error is %v", err)
	}
	if records := lostRecords(t, dir); len(records) != 1 || records[0].Identity != sqlcheck.IdentityChanged {
		t.Errorf("the records are %+v", records)
	}
}

// The statement of the open-transaction check that printed the pair in E2E
// Tests run 42336007508: its output held Execute OK. and Committed. It runs
// once, whatever happens.
func TestRunOnceOnTheMember(t *testing.T) {
	lost := "Execute OK. (0.012 sec) Committed. (0.000 sec)\n\n" + lostPair
	const create = "CREATE TABLE t (id INT PRIMARY KEY);"
	for name, tc := range map[string]struct {
		out     string
		wantErr bool
		records int
	}{
		"a clean statement":         {"Execute OK. (0.012 sec) Committed. (0.000 sec)\n", false, 0},
		"the pair":                  {lost, false, 1},
		"the pair and no commit":    {strings.ReplaceAll(lost, "Committed.", ""), true, 1},
		"the pair and an ERROR:":    {lost + "ERROR: in line 1\n", true, 1},
		"-199 without the -111":     {strings.Replace(lost, "ERROR CODE = -111", "CODE = -971", 1), true, 0},
		"an error without the pair": {"ERROR: table exists\n", true, 0},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := freshLedger(t)
			useEvidenceDir(t, t.TempDir())
			run, calls := cannedKubectl(t, s00Answer{out: tc.out})
			err := fakeMember(run, sameServer()).runOnce(create, statementFinished)
			if (err != nil) != tc.wantErr || *calls != 1 || len(ledger.records) != tc.records {
				t.Errorf("%d calls, %d records, error %v", *calls, len(ledger.records), err)
			}
		})
	}
}

// lostPair is the pair as the Kind runs print it.
const lostPair = "Time: 11/07/26 01:26:15.045 - ERROR *** file /home/jenkins/workspace/cubrid_release_11.4/src/" +
	"communication/network_cl.c, line 177 ERROR CODE = -199, Tran = 3, EID = 2\n" +
	"Server no longer responding.... Invalid argument\n\n" +
	"Time: 11/07/26 01:26:15.045 - ERROR *** file /home/jenkins/workspace/cubrid_release_11.4/src/" +
	"transaction/transaction_cl.c, line 600 ERROR CODE = -111, Tran = 3, EID = 3\n" +
	"Your transaction has been aborted by the system due to server failure or mode change.\n"

// heldOutput is the output of the held session, as E2E Tests run 42321363816
// printed it at the end of a session.
func heldOutput() string {
	return strings.Repeat("200 rows affected. (0.005000 sec) \n", openTxRounds) +
		"Execute OK. (0.011999 sec) \n\n"
}

// The session's verdict: a failed command and every other ERROR still fail, and
// a tolerated pair is counted, passes this check and leaves the row counts to
// decide.
func TestHeldSessionVerdict(t *testing.T) {
	changed := sqlcheck.Guard{Identity: func() func() (string, error) {
		reads := 0
		return func() (string, error) {
			reads++
			if reads == 1 {
				return "cub_server 77 1\n", nil
			}
			return "cub_server 78 2\n", nil
		}
	}()}
	session := heldOutput()
	for name, tc := range map[string]struct {
		out      string
		err      error
		guard    sqlcheck.Guard
		wantErr  bool
		recorded int
		identity sqlcheck.IdentityResult
	}{
		"a clean session": {session, nil, sameServer(), false, 0, ""},
		"the pair after every result": {session + lostPair, nil, sameServer(), false, 1,
			sqlcheck.IdentityUnchanged},
		"the pair with the command failed": {session + lostPair, errors.New("exit status 1"), sameServer(), true, 1,
			sqlcheck.IdentityNotRead},
		"another ERROR: beside the pair": {session + "ERROR: In line 3, deadlock\n" + lostPair, nil, sameServer(),
			true, 1, sqlcheck.IdentityNotRead},
		"another code beside the pair": {session + "ERROR CODE = -72, Tran = 3\n" + lostPair, nil, sameServer(),
			true, 1, sqlcheck.IdentityNotRead},
		"an unexpected ERROR beside the pair": {session + "csql> ERROR something\n" + lostPair, nil, sameServer(),
			true, 1, sqlcheck.IdentityNotRead},
		"the pair with a round missing": {strings.Replace(session, "200 rows affected. (0.005000 sec) \n", "", 1) +
			lostPair, nil, sameServer(), true, 1, sqlcheck.IdentityNotRead},
		"the pair before the commit's result": {strings.ReplaceAll(session, "Execute OK.", "") + lostPair, nil,
			sameServer(), true, 1, sqlcheck.IdentityNotRead},
		"the pair with the server changed": {session + lostPair, nil, changed, true, 1, sqlcheck.IdentityChanged},
		"another error alone":              {session + "ERROR: failed\n", nil, sameServer(), true, 0, ""},
		"-199 alone": {session + strings.Replace(lostPair, "ERROR CODE = -111", "CODE = -971", 1), nil, sameServer(),
			true, 0, ""},
		"a failed command":                {session, errors.New("exit status 1"), sameServer(), true, 0, ""},
		"a failed command with no output": {"", errors.New("exit status 137"), sameServer(), true, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := freshLedger(t)
			useEvidenceDir(t, t.TempDir())
			err := heldSessionVerdict(tc.guard.Before(), "hab-0", tc.out, tc.err)
			if (err != nil) != tc.wantErr {
				t.Errorf("the error is %v, want an error: %v", err, tc.wantErr)
			}
			if len(ledger.records) != tc.recorded {
				t.Fatalf("%d records, want %d", len(ledger.records), tc.recorded)
			}
			if tc.recorded == 1 && ledger.records[0].Identity != tc.identity {
				t.Errorf("the identity is recorded as %s, want %s", ledger.records[0].Identity, tc.identity)
			}
		})
	}
}

// A scenario's measurement is unknown without reads, and keeps its count on a
// run that passed.
func TestLostSessionMeasurement(t *testing.T) {
	startRunState(t)
	ledger := freshLedger(t)
	ledger.reads["S02"], ledger.lost["S02"] = 4, 2
	ledger.reads["S14"] = 3
	ledger.reads["S03-abrupt"], ledger.lost["S03-abrupt"] = 1, 1
	recordScenario(evidence.Scenario{ID: "S01", Result: evidence.Pass})
	recordScenario(evidence.Scenario{ID: "S14", Result: evidence.Pass})
	recordScenario(evidence.Scenario{ID: "S02", Result: evidence.Pass, Measurements: map[string]any{"other": 1}})
	recordScenario(evidence.Scenario{ID: "S03", Variant: "abrupt", Result: evidence.Fail})
	ledger.lost["S05-writes-only"] = 1
	recordScenario(evidence.Scenario{ID: "S05", Variant: "writes-only", Result: evidence.Pass})
	recordScenario(evidence.Scenario{ID: "S06", Variant: "restart", Result: evidence.NotRun, Reason: "not reached"})
	want := map[string]any{"S01": evidence.Unknown, "S02": 2, "S03/abrupt": 1, "S05/writes-only": 1, "S14": 0}
	found := 0
	for _, s := range runSummary.Scenarios {
		got, measured := s.Measurements[lostSessionsMeasurement]
		wanted, ok := want[s.Name()]
		if !ok {
			if measured {
				t.Errorf("%s has the measurement %v", s.Name(), got)
			}
			continue
		}
		found++
		if got != wanted {
			t.Errorf("%s: %s = %v, want %v", s.Name(), lostSessionsMeasurement, got, wanted)
		}
	}
	if found != len(want) {
		t.Errorf("%d of %d scenarios found in %+v", found, len(want), runSummary.Scenarios)
	}
	if ledger.note() != "" {
		t.Error("a note with no record of a pair")
	}
}

// A run that ended with a pair says so in its summary, whatever its verdict.
func TestConcludeRunNotesThePair(t *testing.T) {
	startRunState(t)
	freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	planRun()
	run, _ := cannedKubectl(t, s00Answer{out: lostSession})
	_, _ = runSQL(csqlRunner(context.Background(), "ns", "s00-0", "appdb", run), "SELECT 1;")
	_ = concludeRun()
	data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary evidence.Summary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Notes) != 1 || !strings.Contains(summary.Notes[0], "#238") {
		t.Errorf("the notes are %q", summary.Notes)
	}
}

// The read at the end of S00's smoke checks fails on a server that did not
// hold, even though the second read would have shown its row.
func TestS00LastReadNeedsTheSameServer(t *testing.T) {
	reads := 0
	guard := sqlcheck.Guard{Identity: func() (string, error) {
		reads++
		if reads == 1 {
			return "cub_server 77 1\n", nil
		}
		return "", nil
	}}
	calls := 0
	records, err := s00LastRead(readThrough(guard, func(string) (string, error) {
		calls++
		if calls == 1 {
			return lostSession, nil
		}
		return cleanRead, nil
	}), "s00-0", s00TestMarker)
	var lost *sqlcheck.LostSessionError
	if !errors.As(err, &lost) || lost.Identity != sqlcheck.IdentityVanished {
		t.Fatalf("the error is %v, want a server that vanished", err)
	}
	if len(records) != 1 || records[0].OK {
		t.Errorf("the records are %+v", records)
	}
}

// S00 compares cub_server across the smoke checks: a server that died and was
// revived before a call's own identity read is seen only that way.
func TestS00ServerHeld(t *testing.T) {
	sequence := func(outs ...string) sqlcheck.Guard {
		reads := 0
		return sqlcheck.Guard{Identity: func() (string, error) {
			reads++
			if reads > len(outs) {
				return "", errors.New("the exec could not be run")
			}
			return outs[reads-1], nil
		}}
	}
	const a, b = "cub_server 77 100\n", "cub_server 91 160\n"
	for name, tc := range map[string]struct {
		guard sqlcheck.Guard
		want  sqlcheck.IdentityResult
	}{
		"the same server":         {sequence(a, a), sqlcheck.IdentityUnchanged},
		"a revived server":        {sequence(a, b), sqlcheck.IdentityChanged},
		"no server at the end":    {sequence(a, ""), sqlcheck.IdentityVanished},
		"an unreadable end":       {sequence(a), sqlcheck.IdentityUnreadable},
		"an unreadable start":     {sequence("bash: error\n", a), sqlcheck.IdentityUnreadable},
		"never read at the write": {sqlcheck.Guard{}, sqlcheck.IdentityUnreadable},
	} {
		held, err := s00ServerHeld(tc.guard.Before())
		if held != tc.want || (err == nil) != (tc.want == sqlcheck.IdentityUnchanged) {
			t.Errorf("%s: %s, error %v; want %s", name, held, err, tc.want)
		}
	}
	if held, err := s00ServerHeld(sqlcheck.Watch{}); err == nil || held != sqlcheck.IdentityUnreadable {
		t.Errorf("a Watch that was never started: %s, error %v", held, err)
	}
}

// A member of the HA cluster has its HA mode read with its identity, by
// cubrid changemode; the standalone member of S00 has none.
func TestHAMembersReadTheirMode(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := newHARun("ns", "client-ns", "cluster", "appdb", []string{pod0, "pod-1", "pod-2"})
	ha := r.memberCSQL(context.Background(), pod0).guard
	standalone := memberCSQL(context.Background(), "ns", "s00-0", "appdb", false).guard
	if !ha.HA || standalone.HA {
		t.Fatalf("HA %v for an HA member and %v for a standalone one", ha.HA, standalone.HA)
	}
	for name, tc := range map[string]struct {
		guard sqlcheck.Guard
		want  bool
	}{"an HA member": {ha, true}, "a standalone member": {standalone, false}} {
		out, err := tc.guard.Identity()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "cubrid changemode") != tc.want || !strings.Contains(out, "/proc/[0-9]*") {
			t.Errorf("%s runs:\n%s", name, out)
		}
	}
}

// The check of the starting state before S00 deletes the Pod reads the marker
// row once more when csql printed -199 and -111 beside it, as the smoke reads
// do, and passes only on an unchanged server whose second read shows the row
// (#238; E2E run 42985228360 was blocked on that pair).
func TestS00ReadBeforePodDeletion(t *testing.T) {
	calls := 0
	err := s00ReadBeforePodDeletion(readThrough(sameServer(), func(string) (string, error) {
		calls++
		if calls == 1 {
			return lostSession, nil
		}
		return cleanRead, nil
	}), "s00-0", s00TestMarker)
	if err != nil {
		t.Fatalf("a pair on an unchanged server, then the row: %v", err)
	}
	if calls != 2 {
		t.Errorf("csql ran %d times, want 2", calls)
	}
}

// The starting state stays unverified when the server did not hold, when the
// second read prints the pair too, and when the row is not there.
func TestS00ReadBeforePodDeletionStaysStrict(t *testing.T) {
	vanished := func() sqlcheck.Guard {
		reads := 0
		return sqlcheck.Guard{Identity: func() (string, error) {
			reads++
			if reads == 1 {
				return "cub_server 77 1\n", nil
			}
			return "", nil
		}}
	}
	twice := func(string) (string, error) { return lostSession, nil }
	cases := map[string]struct {
		guard sqlcheck.Guard
		run   sqlRunner
	}{
		"the server vanished": {vanished(), func() sqlRunner {
			calls := 0
			return func(string) (string, error) {
				calls++
				if calls == 1 {
					return lostSession, nil
				}
				return cleanRead, nil
			}
		}()},
		"the pair twice":  {sameServer(), twice},
		"no such row":     {sameServer(), func(string) (string, error) { return "0 rows selected.\n", nil }},
		"transport error": {sameServer(), func(string) (string, error) { return "", errors.New("transport") }},
	}
	for name, c := range cases {
		if err := s00ReadBeforePodDeletion(readThrough(c.guard, c.run), "s00-0", s00TestMarker); err == nil {
			t.Errorf("%s: the starting state was verified", name)
		}
	}
}
