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
		return "1 'marker-123'\n1 row selected", nil
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

// TestSmokeOutcome checks that a csql operation is recorded as failed only
// when csql reported the statement as failed in the Pod; a row that does not
// match and a command that could not be run are recorded with their own
// reason instead (S00). A read whose output holds its row together with
// -199 and -111, the pair POC-44 saw csql print after statements that took
// effect, is recorded with a reason that names POC-44; a -199 without the
// row, and a write that printed the pair, stay failed.
func TestSmokeOutcome(t *testing.T) {
	const marker = "s00-before-pod-delete"
	read := func(out string) error {
		return readS00Row(func(string) (string, error) { return out, nil }, marker)
	}
	statement := func() error { return read("ERROR: no table") }
	// lostSession is csql's output of the read at the end of the smoke checks
	// in the Kind e2e run 41986930319 of PR #698, as its log printed it.
	lostSession := "\n" +
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
		"wrong row": {readS00Row(func(string) (string, error) { return "1 'other'\n1 row selected", nil }, "m"),
			"m", false, "did not show a failed statement"},
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
