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

package sqlcheck

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	serverA = "cub_server 77 123456\n"
	serverB = "cub_server 78 123999\n"
	// A pid the kernel reused for a new process: only the start time differs.
	serverReused = "cub_server 77 123999\n"
)

// lostValue is the output of a one-value read that printed the pair after its
// result, in the order the Kind runs printed it.
const lostValue = "=== <Result of SELECT Command in Line 1> ===\n\n" +
	"  v                   \n======================\n  '0'                 \n\n" +
	"1 row selected. (0.001000 sec) Committed. (0.000000 sec) \n\n" +
	"Time: 11/07/26 01:26:15.045 - ERROR *** file network_cl.c, line 177 ERROR CODE = -199, Tran = 3, EID = 2\n" +
	"Server no longer responding.... Invalid argument\n\n" +
	"Time: 11/07/26 01:26:15.045 - ERROR *** file transaction_cl.c, line 600 ERROR CODE = -111, Tran = 3, EID = 3\n" +
	"Your transaction has been aborted by the system due to server failure or mode change.\n"

const cleanValue = "=== <Result of SELECT Command in Line 1> ===\n\n" +
	"  v                   \n======================\n  '0'                 \n\n" +
	"1 row selected. (0.001000 sec) Committed. (0.000000 sec) \n"

// answer is what one csql call returns.
type answer struct {
	out string
	err error
}

// fakeRunner answers the calls in order and counts them. A call beyond the
// answers fails the test: that is the check that a read makes no third call.
func fakeRunner(t *testing.T, answers ...answer) (Runner, *int) {
	t.Helper()
	calls := 0
	return func(string) (string, error) {
		calls++
		if calls > len(answers) {
			t.Errorf("csql call %d, want at most %d", calls, len(answers))
			return "", errors.New("too many calls")
		}
		a := answers[calls-1]
		return a.out, a.err
	}, &calls
}

// identities returns a guard whose reads give the answers in order.
func identities(t *testing.T, answers ...answer) Guard {
	t.Helper()
	reads := 0
	return Guard{Identity: func() (string, error) {
		reads++
		if reads > len(answers) {
			t.Errorf("identity read %d, want at most %d", reads, len(answers))
			return "", errors.New("too many reads")
		}
		a := answers[reads-1]
		return a.out, a.err
	}}
}

func stable(t *testing.T) Guard {
	return identities(t, answer{out: serverA}, answer{out: serverA})
}

const readSQL = "SELECT TO_CHAR(COUNT(*)) AS v FROM t;"

// wantLost checks that err is a typed error of the rule, never nil.
func wantLost(t *testing.T, err error, identity IdentityResult, twice bool) {
	t.Helper()
	var lost *LostSessionError
	if !errors.As(err, &lost) {
		t.Fatalf("the error is %v, want a *LostSessionError", err)
	}
	if lost.Identity != identity || lost.Twice != twice {
		t.Errorf("the error is %+v, want identity %s and twice %v", lost, identity, twice)
	}
}

// One pair with an unchanged server and a clean second read passes with the
// second read's answer, and says what happened.
func TestReadPairThenCleanRead(t *testing.T) {
	run, calls := fakeRunner(t, answer{out: lostValue}, answer{out: cleanValue})
	got, err := Read(stable(t), run, readSQL, ValuesComplete)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || got.Out != cleanValue || got.Identity != IdentityUnchanged || got.ReRead != ReReadAnswered ||
		len(got.Attempts) != 2 {
		t.Errorf("%d calls and %+v", *calls, got)
	}
}

// A clean read costs one identity read and one csql call.
func TestReadCleanIsOneCall(t *testing.T) {
	run, calls := fakeRunner(t, answer{out: cleanValue})
	got, err := Read(identities(t, answer{out: serverA}), run, readSQL, ValuesComplete)
	if err != nil || *calls != 1 || got.Identity != IdentityNotRead || got.ReRead != ReReadNotMade {
		t.Errorf("%d calls, %+v, error %v", *calls, got, err)
	}
}

// Only the answer without a pair reaches the caller as a pass. A second pair is
// a typed error and a read makes no third call.
func TestReadSecondAnswer(t *testing.T) {
	wrong := errors.New("the row does not match")
	check := func(out string) error {
		if strings.Contains(out, "'1'") {
			return wrong
		}
		return ValuesComplete(out)
	}
	noResult := lostValue[strings.Index(lostValue, "\nTime:"):]
	for name, tc := range map[string]answer{
		"a failed statement":          {out: "ERROR: the statement failed"},
		"a wrong row":                 {out: strings.Replace(cleanValue, "'0'", "'1'", 1)},
		"a transport error":           {err: errors.New("the command could not be run")},
		"a pair with no result":       {out: noResult},
		"the pair with a failed exit": {out: lostValue, err: errors.New("exit status 1")},
	} {
		t.Run(name, func(t *testing.T) {
			run, calls := fakeRunner(t, answer{out: lostValue}, tc)
			got, err := Read(stable(t), run, readSQL, check)
			if err == nil || *calls != 2 {
				t.Fatalf("%d calls, error %v", *calls, err)
			}
			if _, lost := errors.AsType[*LostSessionError](err); lost {
				t.Errorf("the error %v is the report of a second pair", err)
			}
			if !slices.Contains([]string{reReadStatement, reReadCommand, reReadCheck}, got.ReRead) {
				t.Errorf("the second read is recorded as %q, want one of the fixed reasons", got.ReRead)
			}
		})
	}
	t.Run("the pair again", func(t *testing.T) {
		run, calls := fakeRunner(t, answer{out: lostValue}, answer{out: lostValue})
		got, err := Read(stable(t), run, readSQL, check)
		wantLost(t, err, IdentityUnchanged, true)
		if *calls != 2 || got.ReRead != ReReadLost {
			t.Errorf("%d calls, the second read is recorded as %q", *calls, got.ReRead)
		}
	})
}

// Every first answer below stands as it is today: one call and an error.
func TestReadOutsideTheRuleIsOneCall(t *testing.T) {
	alone199 := strings.Replace(lostValue, "ERROR CODE = -111", "CODE = -971", 1)
	alone111 := strings.Replace(lostValue, "ERROR CODE = -199", "CODE = -971", 1)
	beside := func(line string) string { return strings.Replace(lostValue, "\nTime:", "\n"+line+"\nTime:", 1) }
	noResult := lostValue[strings.Index(lostValue, "\nTime:"):]
	for name, tc := range map[string]answer{
		"only -199":                      {out: alone199},
		"only -111":                      {out: alone111},
		"the pair and another code":      {out: beside("ERROR CODE = -1, Tran = 3, EID = 4")},
		"the pair and a longer code":     {out: beside("ERROR CODE = -1990, Tran = 3, EID = 4")},
		"the pair and an ERROR: line":    {out: beside("ERROR: In line 1, column 1, there is no table")},
		"the pair and an unexpected one": {out: beside("csql> ERROR: something else")},
		"the pair and no connection":     {out: beside("Failed to connect to the server")},
		"the pair with exit 1":           {out: lostValue, err: errors.New("exit status 1")},
		"the result is missing":          {out: noResult},
		"the pair and fewer rows than counted": {
			out: strings.Replace(lostValue, "1 row selected", "2 rows selected", 1)},
		"the pair and no count": {
			out: strings.Replace(lostValue, "1 row selected", "the output ended", 1)},
		"a failed statement": {out: "ERROR: the first statement failed"},
	} {
		t.Run(name, func(t *testing.T) {
			run, calls := fakeRunner(t, tc)
			_, err := Read(identities(t, answer{out: serverA}), run, readSQL, ValuesComplete)
			if err == nil || *calls != 1 {
				t.Errorf("%d calls, error %v", *calls, err)
			}
		})
	}
}

// A changed server fails the read even though the next read would be clean,
// because a clean read says nothing about the server that just died.
func TestReadIdentityMustHold(t *testing.T) {
	transport := errors.New("the exec could not be run")
	for name, tc := range map[string]struct {
		before, after answer
		want          IdentityResult
	}{
		"another pid":                 {answer{out: serverA}, answer{out: serverB}, IdentityChanged},
		"a reused pid":                {answer{out: serverA}, answer{out: serverReused}, IdentityChanged},
		"gone after":                  {answer{out: serverA}, answer{}, IdentityVanished},
		"none before":                 {answer{}, answer{out: serverA}, IdentityVanished},
		"none on both reads":          {answer{}, answer{}, IdentityVanished},
		"two after":                   {answer{out: serverA}, answer{out: serverA + serverB}, IdentityAmbiguous},
		"two before":                  {answer{out: serverA + serverB}, answer{out: serverA}, IdentityAmbiguous},
		"two on both reads":           {answer{out: serverA + serverB}, answer{out: serverA + serverB}, IdentityAmbiguous},
		"a failed read after":         {answer{out: serverA}, answer{err: transport}, IdentityUnreadable},
		"a failed read before":        {answer{err: transport}, answer{out: serverA}, IdentityUnreadable},
		"failed reads on both":        {answer{err: transport}, answer{err: transport}, IdentityUnreadable},
		"a failed read with a line":   {answer{out: serverA}, answer{out: serverA, err: transport}, IdentityUnreadable},
		"something else printed":      {answer{out: serverA}, answer{out: "bash: tr: not found\n"}, IdentityUnreadable},
		"something else beside a pid": {answer{out: serverA}, answer{out: serverA + "warning\n"}, IdentityUnreadable},
		"no start time":               {answer{out: serverA}, answer{out: "cub_server 77 \n"}, IdentityUnreadable},
	} {
		t.Run(name, func(t *testing.T) {
			run, calls := fakeRunner(t, answer{out: lostValue}, answer{out: cleanValue})
			got, err := Read(identities(t, tc.before, tc.after), run, readSQL, ValuesComplete)
			wantLost(t, err, tc.want, false)
			if got.Identity != tc.want || *calls > 2 {
				t.Errorf("%d calls, recorded %+v", *calls, got)
			}
		})
	}
}

// The identity is read after the settle, not as csql exits.
func TestReadSettlesBeforeTheIdentity(t *testing.T) {
	const settle = 40 * time.Millisecond
	var returned time.Time
	run := func(string) (string, error) {
		if returned.IsZero() {
			returned = time.Now()
			return lostValue, nil
		}
		return cleanValue, nil
	}
	guard := Guard{Settle: settle, Identity: func() (string, error) {
		// The server that was killed still shows until the settle has passed.
		if !returned.IsZero() && time.Since(returned) >= settle {
			return serverB, nil
		}
		return serverA, nil
	}}
	_, err := Read(guard, run, readSQL, ValuesComplete)
	wantLost(t, err, IdentityChanged, false)
}

// Text that is not one SELECT is refused before anything runs.
func TestReadRefusesWhatIsNotASelect(t *testing.T) {
	for _, sql := range []string{
		"INSERT INTO t VALUES (1);",
		"SELECT 1; INSERT INTO t VALUES (1);",
		"SELECT 1;\nINSERT INTO t VALUES (1)",
		"SELECT 1;;",
		"insert into t select * from u;",
		"COMMIT;",
		"UPDATE t SET a = 1;",
		"SELECTED;",
		"",
		";",
		"-- SELECT\nDROP TABLE t;",
		"DELETE FROM t WHERE a IN (SELECT 1);",
	} {
		reads := 0
		guard := Guard{Identity: func() (string, error) { reads++; return serverA, nil }}
		calls := 0
		_, err := Read(guard, func(string) (string, error) { calls++; return lostValue, nil }, sql, ValuesComplete)
		if !errors.Is(err, ErrNotARead) || calls != 0 || reads != 0 {
			t.Errorf("%q: %d csql calls, %d identity reads, error %v", sql, calls, reads, err)
		}
	}
	for _, sql := range []string{"SELECT 1;", " select 1 ; \n", "SELECT 1", readSQL, ReadRowQuery} {
		if !ReadOnly(sql) {
			t.Errorf("%q is not read as one SELECT", sql)
		}
	}
}

// Run is what every write goes through, and it has no rule: a pair fails it.
func TestRunStaysStrictOnThePair(t *testing.T) {
	run, calls := fakeRunner(t, answer{out: lostValue})
	if _, err := Run(run, readSQL); err == nil || *calls != 1 {
		t.Errorf("%d calls, error %v", *calls, err)
	}
}

// RunOnce never repeats the statement, whatever the output says.
func TestRunOnce(t *testing.T) {
	complete := func(out string) error {
		if !strings.Contains(out, "Committed.") {
			return errors.New("no commit")
		}
		return nil
	}
	const sql = "CREATE TABLE t (id INT PRIMARY KEY);"
	beside := strings.Replace(lostValue, "\nTime:", "\nERROR: in the statement\nTime:", 1)
	for name, tc := range map[string]struct {
		first    answer
		guard    Guard
		identity IdentityResult
		wantErr  bool
		lost     bool
	}{
		"a clean statement": {answer{out: "Execute OK. Committed."}, identities(t, answer{out: serverA}),
			IdentityNotRead, false, false},
		"the pair with the server unchanged": {answer{out: lostValue}, stable(t), IdentityUnchanged, false, false},
		"the pair with the server changed": {answer{out: lostValue},
			identities(t, answer{out: serverA}, answer{out: serverB}), IdentityChanged, true, true},
		"the pair and an unreadable server": {answer{out: lostValue},
			identities(t, answer{out: serverA}, answer{err: errors.New("exec")}), IdentityUnreadable, true, true},
		"the pair and another error": {answer{out: beside}, identities(t, answer{out: serverA}),
			IdentityNotRead, true, false},
		"the pair with exit 1": {answer{out: lostValue, err: errors.New("exit status 1")},
			identities(t, answer{out: serverA}), IdentityNotRead, true, false},
		"no commit in the output": {answer{out: strings.ReplaceAll(lostValue, "Committed.", "")},
			identities(t, answer{out: serverA}), IdentityNotRead, true, false},
		"just -199": {answer{out: strings.Replace(lostValue, "ERROR CODE = -111", "CODE = -971", 1)},
			identities(t, answer{out: serverA}), IdentityNotRead, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			run, calls := fakeRunner(t, tc.first)
			identity, err := RunOnce(tc.guard, run, sql, complete)
			if (err != nil) != tc.wantErr || identity != tc.identity || *calls != 1 {
				t.Errorf("%d calls, identity %s, error %v", *calls, identity, err)
			}
			if tc.lost {
				wantLost(t, err, tc.identity, false)
			}
		})
	}
}

// Judge is RunOnce for output that did not go through Run.
func TestJudge(t *testing.T) {
	complete := func(string) error { return nil }
	identity, err := stable(t).Before().Judge(lostValue, complete)
	if err != nil || identity != IdentityUnchanged {
		t.Errorf("identity %s, error %v", identity, err)
	}
	guard := identities(t, answer{out: serverA}, answer{out: serverB})
	_, err = guard.Before().Judge(lostValue, complete)
	wantLost(t, err, IdentityChanged, false)
	beside := strings.Replace(lostValue, "\nTime:", "\nERROR: in the statement\nTime:", 1)
	if identity, err = identities(t, answer{out: serverA}).Before().Judge(beside, complete); err == nil ||
		identity != IdentityNotRead {
		t.Errorf("another error: identity %s, error %v", identity, err)
	}
	if _, err = identities(t, answer{out: serverA}).Before().Judge(lostValue, func(string) error {
		return errors.New("incomplete")
	}); err == nil {
		t.Error("an incomplete session was tolerated")
	}
}

// startServerCopy starts a copy of bash named cub_server, with database among
// its arguments as the real server has it, and returns the command.
func startServerCopy(t *testing.T, bash, database string) *exec.Cmd {
	t.Helper()
	data, err := os.ReadFile(bash)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cub := filepath.Join(dir, "cub_server")
	if err := os.WriteFile(cub, data, 0o700); err != nil {
		t.Fatal(err)
	}
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(cub, "-c", "read -r _", database)
	server.Stdin = stdin
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_ = server.Wait()
		_ = w.Close()
	})
	return server
}

// awaitComm reads until the script prints what is wanted: until exec has
// replaced the test's fork, a started process has the test's name in /proc.
func awaitComm(t *testing.T, script func() string, want *regexp.Regexp) string {
	t.Helper()
	var out string
	for range 200 {
		if out = script(); want.MatchString(out) {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the script printed %q, want %s", out, want)
	return ""
}

// The script runs here against processes named cub_server, so the /proc walk
// and its field numbers are checked for real, on this host's kernel.
func TestServerIdentityScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	database := fmt.Sprintf("identitydb%d", os.Getpid())
	script := func() string {
		out, err := exec.Command(bash, "-c", ServerIdentityScript, database).CombinedOutput()
		if err != nil {
			t.Fatalf("the script failed: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := script(); out != "" {
		t.Fatalf("with no server the script printed %q", out)
	}
	startServerCopy(t, bash, database+"x")
	time.Sleep(50 * time.Millisecond)
	if out := script(); out != "" {
		t.Fatalf("the server of another database was found: %q", out)
	}

	one := regexp.MustCompile(`^cub_server \d+ \d+\n$`)
	first := startServerCopy(t, bash, database)
	before := awaitComm(t, script, one)
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", first.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	// The fields after the name, which ends at the last ")", start at field 3,
	// and the start time is field 22.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if want := fmt.Sprintf("cub_server %d %s\n", first.Process.Pid, fields[19]); before != want {
		t.Errorf("the script printed %q, want %q", before, want)
	}
	if id, _, result := readIdentity(before, nil, false); id == "" || result != IdentityUnchanged {
		t.Errorf("readIdentity(%q) = %q, %s", before, id, result)
	}

	second := startServerCopy(t, bash, database)
	two := awaitComm(t, script, regexp.MustCompile(`^(cub_server \d+ \d+\n){2}$`))
	if _, _, result := readIdentity(two, nil, false); result != IdentityAmbiguous {
		t.Errorf("two servers: %s", result)
	}
	_ = second.Process.Kill()
	_ = second.Wait()

	// Killed and not reaped, the process is a zombie: it is no running server.
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if out := awaitComm(t, script, regexp.MustCompile(`^$`)); out != "" {
		t.Errorf("a dead server was found: %q", out)
	}
	_ = first.Wait()

	// A new process has another identity.
	startServerCopy(t, bash, database)
	after := awaitComm(t, script, one)
	if after == before {
		t.Errorf("the new process has the identity of the old one: %q", after)
	}
}

// ValuesComplete asks more of an output with the pair and leaves a clean
// output as Values judges it.
func TestValuesComplete(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		want bool
	}{
		"clean":                         {cleanValue, true},
		"clean with no rows":            {"There are no results.\n", true},
		"clean with no count, as today": {"\n==\n  'a'\n", true},
		"the pair with the result":      {lostValue, true},
		"the pair with fewer rows":      {strings.Replace(lostValue, "1 row selected", "2 rows selected", 1), false},
		"the pair with no count":        {strings.Replace(lostValue, "1 row selected", "", 1), false},
		"the pair and no results": {
			"There are no results.\n" + lostValue[strings.Index(lostValue, "\nTime:"):], false},
		"no table": {"nothing here", false},
	} {
		if err := ValuesComplete(tc.out); (err == nil) != tc.want {
			t.Errorf("%s: error %v, want a pass: %v", name, err, tc.want)
		}
	}
}

// The second read's failure is recorded by kind, never with its output, which
// holds the command line and everything csql printed.
func TestReadRecordsHowTheSecondReadFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		second answer
		want   string
	}{
		"a statement error": {answer{out: "ERROR: In line 1, a long message about the table t"}, reReadStatement},
		"a failed command": {answer{err: errors.New(`"kubectl -n ns exec pod -- csql" failed with output`)},
			reReadCommand},
		"a result that fails": {answer{out: strings.Replace(cleanValue, "'0'", "'1'", 1)}, reReadCheck},
	} {
		run, _ := fakeRunner(t, answer{out: lostValue}, tc.second)
		got, err := Read(stable(t), run, readSQL, func(out string) error {
			if strings.Contains(out, "'1'") {
				return errors.New("the row does not match")
			}
			return ValuesComplete(out)
		})
		if err == nil || got.ReRead != tc.want {
			t.Errorf("%s: the second read is recorded as %q, want %q; error %v", name, got.ReRead, tc.want, err)
		}
	}
}

// On an HA member the HA mode is part of the identity: a standby promoted to
// active keeps its process, and -111's message names a mode change.
func TestReadNeedsTheSameHAMode(t *testing.T) {
	const standby, active = "ha_mode standby\n", "ha_mode active\n"
	for name, tc := range map[string]struct {
		before, after string
		want          IdentityResult
	}{
		"the same process and mode":     {serverA + standby, serverA + standby, IdentityUnchanged},
		"the same process, promoted":    {serverA + standby, serverA + active, IdentityModeChanged},
		"to-be-active after":            {serverA + standby, serverA + "ha_mode to-be-active\n", IdentityModeChanged},
		"another process, same mode":    {serverA + standby, serverB + standby, IdentityChanged},
		"no mode after":                 {serverA + standby, serverA, IdentityUnreadable},
		"no mode before":                {serverA, serverA + standby, IdentityUnreadable},
		"a mode that could not be read": {serverA + standby, serverA + "ha_mode ?\n", IdentityUnreadable},
		"two modes":                     {serverA + standby, serverA + standby + active, IdentityUnreadable},
		"a mode and no server":          {serverA + standby, standby, IdentityVanished},
		"the mode and two servers":      {serverA + standby, serverA + serverB + standby, IdentityAmbiguous},
	} {
		t.Run(name, func(t *testing.T) {
			run, calls := fakeRunner(t, answer{out: lostValue}, answer{out: cleanValue})
			guard := identities(t, answer{out: tc.before}, answer{out: tc.after})
			guard.HA = true
			got, err := Read(guard, run, readSQL, ValuesComplete)
			if tc.want == IdentityUnchanged {
				if err != nil || got.Identity != IdentityUnchanged || *calls != 2 {
					t.Errorf("%d calls, %+v, error %v", *calls, got, err)
				}
				return
			}
			wantLost(t, err, tc.want, false)
			if *calls != 1 {
				t.Errorf("%d calls after a mode that did not hold", *calls)
			}
		})
	}
	// A member that is not HA has no mode line, and one in its output is
	// something else.
	if _, _, result := readIdentity(serverA+standby, nil, false); result != IdentityUnreadable {
		t.Errorf("a mode line from a member that is not HA reads as %s", result)
	}
	if got := (Watch{}).Compare(); got != IdentityUnreadable {
		t.Errorf("a Watch that was never started compares as %s", got)
	}
	if got := (Guard{}).Before().Compare(); got != IdentityUnreadable {
		t.Errorf("a Guard without a reader compares as %s", got)
	}
}

// The HA script runs here with a stand-in cubrid on $CUBRID/bin that prints
// what POC-52 recorded from cubrid changemode.
func TestHAServerIdentityScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout")
	}
	database := fmt.Sprintf("hadb%d", os.Getpid())
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CUBRID", home)
	server := startServerCopy(t, bash, database)
	for name, tc := range map[string]struct {
		cubrid string
		want   *regexp.Regexp
	}{
		"active": {`echo "The server '$2''s current HA running mode is active."`,
			regexp.MustCompile(`^cub_server \d+ \d+\nha_mode active\n$`)},
		"to-be-active": {`echo "The server '$2''s current HA running mode is to-be-active."`,
			regexp.MustCompile(`\nha_mode to-be-active\n$`)},
		"no server": {`echo "Failed to connect to database server, '$2', on the following host(s): localhost"; exit 1`,
			regexp.MustCompile(`\nha_mode \?\n$`)},
		"not HA": {`echo "The server was not configured for HA."; exit 1`, regexp.MustCompile(`\nha_mode \?\n$`)},
	} {
		script := "#!/bin/sh\n[ \"$1\" = changemode ] && [ \"$2\" = \"" + database + "@localhost\" ] || exit 2\n" +
			tc.cubrid + "\n"
		if err := os.WriteFile(filepath.Join(home, "bin", "cubrid"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		var out []byte
		for range 200 {
			if out, err = exec.Command(bash, "-c", HAServerIdentityScript, database).CombinedOutput(); err != nil {
				t.Fatalf("%s: the script failed: %v\n%s", name, err, out)
			}
			if strings.HasPrefix(string(out), fmt.Sprintf("cub_server %d ", server.Process.Pid)) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !tc.want.Match(out) {
			t.Errorf("%s: the script printed %q", name, out)
		}
	}
}

// Only the header just before a code of the pair goes with it: another header
// on the line is another error.
func TestOnlyLostSessionKeepsAnotherHeader(t *testing.T) {
	line := "Time: 11/07/26 01:26:15.045 - ERROR *** file a.c, line 1 - ERROR *** file network_cl.c, line 177 " +
		"ERROR CODE = -199, Tran = 3, EID = 2"
	if OnlyLostSession(strings.Replace(lostValue, lostValue[strings.Index(lostValue, "Time:"):strings.Index(lostValue,
		"\nServer no longer")], line, 1)) {
		t.Error("a second header on the line of -199 was taken as part of the pair")
	}
	if !OnlyLostSession(lostValue) {
		t.Error("the pair as the Kind runs print it is not only the pair")
	}
}
