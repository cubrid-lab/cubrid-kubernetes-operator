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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
)

// useCapture installs read as the capture of the server state, and restores the
// earlier one when the test ends.
func useCapture(t *testing.T, read pairCaptureFunc) {
	t.Helper()
	previous := pairCapture.Load()
	pairCapture.Store(&read)
	t.Cleanup(func() { pairCapture.Store(previous) })
}

// The log tail of a record keeps only the last lines, cuts the long ones and
// drops blank ones, so that the record of one pair stays small.
func TestServerLogTailIsBounded(t *testing.T) {
	var out strings.Builder
	for i := range 100 {
		out.WriteString("Time: line " + strings.Repeat("é", i) + "\n\n")
	}
	lines := boundLines(out.String(), serverLogTailLines, 20)
	if len(lines) != serverLogTailLines {
		t.Fatalf("%d lines, want %d", len(lines), serverLogTailLines)
	}
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > 20 {
			t.Errorf("line of %d characters", n)
		}
	}
	if got := boundLines("a\n\n  b  \n", 5, 20); len(got) != 2 || got[1] != "b" {
		t.Errorf("short output gives %q", got)
	}
	if got := cut(strings.Repeat("x", 1000), serverLogLineWidth); len(got) != serverLogLineWidth {
		t.Errorf("an error of %d characters is kept", len(got))
	}
}

// A pair on a member gets the server state read after it, in the file, and the
// read does not delay the call: the capture waits for the call to return, so a
// capture made on the call's path would never finish and the call would hang.
func TestPairRecordsTheServerStateOffTheCallsPath(t *testing.T) {
	ledger := freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	release := make(chan struct{})
	reads := make(chan string, 4)
	useCapture(t, func(_ context.Context, namespace, pod, database string) pairServerState {
		<-release
		reads <- namespace + "/" + pod + "/" + database
		return pairServerState{identity: []string{"cub_server 41 1234", "ha_mode active", "uptime 99.5", "clk_tck 100"},
			logTail: []string{"Time: 11/18/26 10:00:00.000 - a line"}, at: time.Now().UTC(), err: "identity: timed out"}
	})
	run, calls := cannedKubectl(t, s00Answer{out: lostSession}, s00Answer{out: cleanRead})
	sql := csqlRunner(context.Background(), "ns", "hab-1", "appdb", run)
	returned := make(chan error, 1)
	go func() {
		_, err := runSQL(sql, "SELECT 1;")
		returned <- err
	}()
	select {
	case err := <-returned:
		if err == nil {
			t.Error("the pair was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call waited for the read of the server state")
	}
	if _, err := runSQL(sql, "SELECT 1;"); err != nil {
		t.Error(err)
	}
	close(release)
	if !ledger.waitForCaptures(5 * time.Second) {
		t.Fatal("the read did not finish")
	}
	records := lostRecords(t, dir)
	if len(records) != 1 || len(reads) != 1 || *calls != 2 || <-reads != "ns/hab-1/appdb" {
		t.Fatalf("%d records, %d reads, %d calls", len(records), len(reads), *calls)
	}
	got := records[0]
	if len(got.ServerAtPair) != 4 || got.ServerAtPair[0] != "cub_server 41 1234" || len(got.ServerLogTail) != 1 ||
		got.ServerReadError != "identity: timed out" || got.ServerReadAt.IsZero() {
		t.Errorf("the record is %+v", got)
	}
}

// On the guarded path the rule's findings and the server state end up in the
// same record, whichever of the two is written last.
func TestGuardedReadRecordsTheRuleAndTheServerState(t *testing.T) {
	ledger := freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	useCapture(t, func(context.Context, string, string, string) pairServerState {
		return pairServerState{identity: []string{"cub_server 77 123456"}, logTail: []string{"a line"}, at: time.Now().UTC()}
	})
	run, calls := cannedKubectl(t, s00Answer{out: lostSession}, s00Answer{out: cleanRead})
	result, err := fakeMember(run, sameServer()).read(sqlcheck.ReadRowQuery, sqlcheck.RowCheck(s00TestMarker))
	if err != nil || result.Out != cleanRead || *calls != 2 {
		t.Fatalf("%d calls, %+v, error %v", *calls, result, err)
	}
	if !ledger.waitForCaptures(5 * time.Second) {
		t.Fatal("the read did not finish")
	}
	records := lostRecords(t, dir)
	if len(records) != 1 || records[0].Identity != sqlcheck.IdentityUnchanged ||
		records[0].ReRead != sqlcheck.ReReadAnswered || len(records[0].ServerAtPair) != 1 ||
		len(records[0].ServerLogTail) != 1 {
		t.Errorf("the records are %+v", records)
	}
}

// Without a capture, as in every unit test, the fields stay out of the file.
func TestPairWithoutCaptureHasNoServerFields(t *testing.T) {
	freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	previous := pairCapture.Swap(nil)
	t.Cleanup(func() { pairCapture.Store(previous) })
	run, _ := cannedKubectl(t, s00Answer{out: lostSession})
	_, _ = runSQL(csqlRunner(context.Background(), "ns", "hab-1", "appdb", run), "SELECT 1;")
	data, err := os.ReadFile(filepath.Join(dir, lostSessionsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"serverAtPair", "serverLogTail", "serverReadAt", "serverReadError"} {
		if strings.Contains(string(data), field) {
			t.Errorf("the file has %s: %s", field, data)
		}
	}
}

// A credential in a log line is removed by the evidence writer, as in every
// other evidence file.
func TestServerLogTailIsRedacted(t *testing.T) {
	freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	const secret = "hunter2-not-a-real-password"
	old := redactor
	redactor = evidence.NewRedactor(secret)
	t.Cleanup(func() { redactor = old })
	useCapture(t, func(context.Context, string, string, string) pairServerState {
		return pairServerState{logTail: []string{"connect failed password=" + secret}, at: time.Now().UTC()}
	})
	run, _ := cannedKubectl(t, s00Answer{out: lostSession})
	_, _ = runSQL(csqlRunner(context.Background(), "ns", "hab-1", "appdb", run), "SELECT 1;")
	if !csqlLedger.waitForCaptures(5 * time.Second) {
		t.Fatal("the read did not finish")
	}
	data, err := os.ReadFile(filepath.Join(dir, lostSessionsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), "serverLogTail") {
		t.Errorf("the file is %s", data)
	}
}
