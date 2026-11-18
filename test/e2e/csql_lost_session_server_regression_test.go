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
)

// The log tail of a record keeps only the last lines, cuts the long ones and
// drops blank ones, so that the record of one pair stays small.
func TestServerLogTailIsBounded(t *testing.T) {
	var out strings.Builder
	for i := range 100 {
		out.WriteString("Time: line " + strings.Repeat("x", i) + "\n\n")
	}
	lines := boundLines(out.String(), serverLogTailLines, 20)
	if len(lines) != serverLogTailLines {
		t.Fatalf("%d lines, want %d", len(lines), serverLogTailLines)
	}
	for _, line := range lines {
		if len([]rune(line)) > 20 {
			t.Errorf("line of %d characters", len(line))
		}
	}
	if got := boundLines("a\n\n  b  \n", 5, 20); len(got) != 2 || got[1] != "b" {
		t.Errorf("short output gives %q", got)
	}
}

// A pair on a member gets the server state read right after it, in the file,
// without changing the call's result or making another csql call.
func TestPairRecordsTheServerState(t *testing.T) {
	freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	reads := 0
	pairCapture = func(_ context.Context, namespace, pod, database string) pairServerState {
		reads++
		if namespace != "ns" || pod != "hab-1" || database != "appdb" {
			t.Errorf("read of %s/%s/%s", namespace, pod, database)
		}
		return pairServerState{identity: []string{"cub_server 41 1234", "ha_mode active"},
			logTail: []string{"Time: 11/18/26 10:00:00.000 - a line"}, err: "identity: timed out"}
	}
	t.Cleanup(func() { pairCapture = nil })
	run, calls := cannedKubectl(t, s00Answer{out: lostSession}, s00Answer{out: cleanRead})
	sql := csqlRunner(context.Background(), "ns", "hab-1", "appdb", run)
	if _, err := runSQL(sql, "SELECT 1;"); err == nil {
		t.Error("the pair was accepted")
	}
	if _, err := runSQL(sql, "SELECT 1;"); err != nil {
		t.Error(err)
	}
	records := lostRecords(t, dir)
	if len(records) != 1 || reads != 1 || *calls != 2 {
		t.Fatalf("%d records, %d reads, %d calls", len(records), reads, *calls)
	}
	got := records[0]
	if len(got.ServerAtPair) != 2 || got.ServerAtPair[0] != "cub_server 41 1234" || len(got.ServerLogTail) != 1 ||
		got.ServerReadError != "identity: timed out" {
		t.Errorf("the record is %+v", got)
	}
}

// Without a capture, as in every unit test, the fields stay out of the file.
func TestPairWithoutCaptureHasNoServerFields(t *testing.T) {
	freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	run, _ := cannedKubectl(t, s00Answer{out: lostSession})
	_, _ = runSQL(csqlRunner(context.Background(), "ns", "hab-1", "appdb", run), "SELECT 1;")
	data, err := os.ReadFile(filepath.Join(dir, lostSessionsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "serverAtPair") || strings.Contains(string(data), "serverLogTail") {
		t.Errorf("the file has server fields: %s", data)
	}
}
