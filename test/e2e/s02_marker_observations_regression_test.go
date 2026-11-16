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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
)

// Copied from the retained 2026-11-16 marker-write failure in run43873608011,
// job133320916522. The positive-looking result still accompanies -199/-111;
// it is not evidence that the failed write committed or remained durable.
const failedS02MarkerOutput = "\nTime: 11/16/26 13:34:27.498 - NOTIFICATION *** file /home/jenkins/wor" +
	"kspace/cubrid_release_11.4/src/transaction/boot_cl.c, line 1085  CODE " +
	"= -971, Tran = -1, EID = 1\nProgram 'csql' (pid 812) connected to datab" +
	"ase server 'appdb@localhost' on the host 'localhost' (port 1523).\n1 ro" +
	"w affected. (0.001000 sec) Committed. (0.004000 sec) \nExecute OK. (0.0" +
	"00000 sec) \n\nTime: 11/16/26 13:34:27.511 - ERROR *** file /home/jenkin" +
	"s/workspace/cubrid_release_11.4/src/communication/network_cl.c, line 1" +
	"77 ERROR CODE = -199, Tran = 7, EID = 2\nServer no longer responding..." +
	". Invalid argument\n\nTime: 11/16/26 13:34:27.511 - ERROR *** file /home" +
	"/jenkins/workspace/cubrid_release_11.4/src/transaction/transaction_cl." +
	"c, line 600 ERROR CODE = -111, Tran = 7, EID = 3\nYour transaction has " +
	"been aborted by the system due to server failure or mode change.\n"

func TestS02MarkerWriteCharacterizationKeepsStrictErrorAndLedger(t *testing.T) {
	ledger := freshLedger(t)
	useEvidenceDir(t, t.TempDir())
	calls := 0
	var original error
	write := func() (string, error) {
		sql := csqlRunner(context.Background(), "ns", markerFirstMember, "appdb", func(cmd *exec.Cmd) (string, error) {
			calls++
			if query := cmd.Args[len(cmd.Args)-1]; query != "INSERT INTO s02_marker VALUES (1, 'marker'); COMMIT;" {
				t.Fatalf("write changed: %q", query)
			}
			return failedS02MarkerOutput, nil
		})
		out, err := runSQL(sql, "INSERT INTO s02_marker VALUES (1, 'marker'); COMMIT;")
		original = err
		return out, err
	}
	probe := s02MarkerTestProbe()
	probe.selectSQL = func(context.Context, string, string) (string, error) { return "", errors.New("read unavailable") }
	probe.identity = func(context.Context, string) (string, error) { return "", errors.New("identity unavailable") }
	probe.writeFile = func(string, []byte) string { return "" }
	out, _, _, err := writeWithS02MarkerObservations(context.Background(), time.Now(), "marker", write, probe)
	var statement *sqlcheck.StatementError
	if calls != 1 || err != original || !errors.As(err, &statement) || out != failedS02MarkerOutput {
		t.Fatalf("strict write changed: calls=%d err=%v", calls, err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.records) != 1 || ledger.records[0].Kind != markerWriteKind ||
		ledger.records[0].Identity != sqlcheck.IdentityNotRead || ledger.records[0].ReRead != sqlcheck.ReReadNotMade {
		t.Fatalf("original ledger was relabelled: %+v", ledger.records)
	}
}

func TestS02MarkerWriteCharacterizationDoesNotProbeOnSuccess(t *testing.T) {
	calls := 0
	write := func() (string, error) { calls++; return "Committed. Execute OK.", nil }
	unexpected := func(context.Context, string) (string, error) { t.Fatal("probe on successful write"); return "", nil }
	probe := s02MarkerProbe{members: []string{markerFirstMember}, identity: unexpected,
		selectSQL: func(ctx context.Context, member, query string) (string, error) { return unexpected(ctx, member) },
		now:       func() time.Time { t.Fatal("diagnostic clock on success"); return time.Time{} },
		writeFile: func(string, []byte) string { t.Fatal("diagnostic file on success"); return "" }}
	out, file, note, err := writeWithS02MarkerObservations(context.Background(), time.Now(), "marker", write, probe)
	if calls != 1 || err != nil || file != "" || note != "" || !strings.Contains(out, "Committed.") {
		t.Fatalf("success path changed: calls=%d err=%v file=%q note=%q", calls, err, file, note)
	}
}

const (
	markerFirstMember  = "hab-0"
	markerSecondMember = "hab-1"
	markerThirdMember  = "hab-2"
	markerWriteKind    = "write"
	markerReadKind     = "read"
)

func s02MarkerTestProbe() s02MarkerProbe {
	return s02MarkerProbe{
		writer: markerFirstMember, members: []string{markerFirstMember, markerSecondMember, markerThirdMember},
		selectSQL: func(context.Context, string, string) (string, error) {
			return "\n  v\n=====\n  'marker'\n1 row selected.\n", nil
		},
		identity: func(context.Context, string) (string, error) { return "cub_server 41 500\nha_mode active\n", nil },
		now:      func() time.Time { return time.Date(2026, 11, 16, 13, 34, 28, 0, time.UTC) },
		redact:   evidence.NewRedactor(), writeFile: func(path string, _ []byte) string { return path },
	}
}

func TestS02MarkerFailureStoresAnAfterErrorReceipt(t *testing.T) {
	original := errors.New("original strict write error")
	writes := 0
	write := func() (string, error) { writes++; return failedS02MarkerOutput, original }
	probe := s02MarkerTestProbe()
	var stored []byte
	probe.writeFile = func(path string, data []byte) string { stored = data; return path }
	out, file, _, err := writeWithS02MarkerObservations(context.Background(), time.Now(), "marker", write, probe)
	if writes != 1 || err != original || out != failedS02MarkerOutput {
		t.Fatal("failure receipt changed the original write")
	}
	if file != s02MarkerReceiptFile || len(stored) == 0 {
		t.Fatalf("failed marker write produced no after-error receipt: file=%q bytes=%d", file, len(stored))
	}
}

func runFailedS02Marker(t *testing.T, probe s02MarkerProbe) (s02MarkerReceipt, string, string) {
	t.Helper()
	original := errors.New("original strict marker failure")
	calls := 0
	var data []byte
	save := probe.writeFile
	probe.writeFile = func(path string, content []byte) string { data = content; return save(path, content) }
	started := time.Date(2026, 11, 16, 13, 34, 27, 0, time.UTC)
	out, file, note, err := writeWithS02MarkerObservations(context.Background(), started,
		"marker", func() (string, error) { calls++; return failedS02MarkerOutput, original }, probe)
	if calls != 1 || err != original || out != failedS02MarkerOutput {
		t.Fatalf("diagnostics changed original write: calls=%d err=%v", calls, err)
	}
	result := evidence.Scenario{Result: evidence.Fail, Reason: original.Error()}
	addEvidence(&result, file)
	if result.Result != evidence.Fail || result.Reason != original.Error() || (len(result.Evidence) == 1) != (file != "") {
		t.Fatal("optional receipt changed failure or linked an unstored artifact")
	}
	var receipt s02MarkerReceipt
	if file != "" {
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
	}
	return receipt, file, note
}

func s02MarkerRows(values ...string) string {
	if len(values) == 0 {
		return "There are no results.\n"
	}
	return "\n  v\n=====\n  '" + strings.Join(values, "'\n  '") + fmt.Sprintf("'\n%d rows selected.\n", len(values))
}

func TestS02MarkerObservationsKeepRowsAndAfterOnlyIdentity(t *testing.T) {
	probe := s02MarkerTestProbe()
	queries, identities := map[string]int{}, map[string]int{}
	outputs := map[string]string{markerFirstMember: s02MarkerRows("marker"),
		markerSecondMember: s02MarkerRows(), markerThirdMember: s02MarkerRows("other", "marker")}
	clock := time.Date(2026, 11, 16, 13, 34, 28, 0, time.UTC)
	probe.now = func() time.Time { clock = clock.Add(time.Millisecond); return clock }
	probe.selectSQL = func(ctx context.Context, member, query string) (string, error) {
		if _, ok := ctx.Deadline(); !ok || query != s02MarkerSelect {
			t.Fatal("unbounded or non-SELECT diagnostic")
		}
		queries[member]++
		return outputs[member], nil
	}
	probe.identity = func(ctx context.Context, member string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("identity has no deadline")
		}
		identities[member]++
		if member == markerSecondMember {
			return "cub_server 900 901\nha_mode standby\n", nil
		}
		if member == markerThirdMember {
			return "partial identity", errors.New("identity unavailable")
		}
		return "cub_server 41 500\nha_mode active\n", nil
	}
	receipt, file, note := runFailedS02Marker(t, probe)
	if file == "" || note != "" || receipt.Scope != "after_write_error_only" ||
		receipt.Write.Output != failedS02MarkerOutput ||
		receipt.WriteMember != markerFirstMember || receipt.IdentityContinuity != evidence.Unknown ||
		receipt.CommitOutcome != evidence.Unknown {
		t.Fatalf("wrong failure/after-only scope: %+v note=%q", receipt, note)
	}
	want := []string{"expected_value_returned", s02NoValues, "different_values_returned"}
	for i, member := range receipt.Members {
		if queries[member.Member] != 1 || identities[member.Member] != 1 || member.MarkerObservation != want[i] ||
			member.ValueCount == nil || member.Select.StartedAt == nil ||
			member.Select.FinishedAt == nil || member.Identity.StartedAt == nil ||
			member.Select.StartedAt.Before(*receipt.Write.FinishedAt) ||
			member.Identity.StartedAt.Before(*member.Select.FinishedAt) {
			t.Fatalf("observation lost its values/order/interval: %+v", member)
		}
	}
	if receipt.Members[1].Values == nil || *receipt.Members[1].ValueCount != 0 ||
		!strings.Contains(receipt.Members[1].Identity.Output, "900 901") ||
		receipt.Members[2].Identity.Status != s02Unavailable || receipt.Members[2].Identity.Output != "partial identity" {
		t.Fatal("absence, different after-only identity or partial identity error was lost")
	}
}

func TestS02MarkerUnreadableSelectsRemainUnknown(t *testing.T) {
	for _, kind := range []string{"paired error", "unparseable output", "command failure"} {
		t.Run(kind, func(t *testing.T) {
			probe := s02MarkerTestProbe()
			probe.selectSQL = func(context.Context, string, string) (string, error) {
				switch kind {
				case "paired error":
					return runSQL(func(string) (string, error) {
						return s02MarkerRows("marker") + failedS02MarkerOutput, nil
					}, s02MarkerSelect)
				case "command failure":
					return s02MarkerRows("marker"), errors.New("read command failed")
				default:
					return "unreadable csql output", nil
				}
			}
			receipt, _, _ := runFailedS02Marker(t, probe)
			for _, member := range receipt.Members {
				if member.MarkerObservation != evidence.Unknown || member.ValueCount != nil || member.Values != nil ||
					member.Select.Status != s02Unavailable || member.Select.Output == "" || member.Select.Error == "" {
					t.Fatalf("unreadable SELECT was converted into absence or a pass: %+v", member)
				}
			}
		})
	}
}

func TestS02MarkerCancellationStopsFurtherCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := s02MarkerTestProbe()
	reads, identities, writes := 0, 0, 0
	probe.selectSQL = func(context.Context, string, string) (string, error) {
		reads++
		cancel()
		return "partial read", context.Canceled
	}
	probe.identity = func(context.Context, string) (string, error) {
		identities++
		t.Fatal("identity after cancellation")
		return "", nil
	}
	var data []byte
	probe.writeFile = func(path string, content []byte) string { data = content; return path }
	original := errors.New("original write failure")
	_, file, _, err := writeWithS02MarkerObservations(ctx, time.Now(), "marker", func() (string, error) {
		writes++
		return failedS02MarkerOutput, original
	}, probe)
	if err != original || writes != 1 || reads != 1 || identities != 0 || file == "" {
		t.Fatalf("cancelled diagnostics changed original: err=%v counts=%d/%d/%d", err, writes, reads, identities)
	}
	var receipt s02MarkerReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Members) != 3 || receipt.Members[0].Select.Output != "partial read" {
		t.Fatal("cancellation discarded available observations")
	}
	for _, member := range receipt.Members[1:] {
		if member.Select.Status != s02NotObserved || member.Identity.Status != s02NotObserved ||
			member.Select.StartedAt != nil || member.ValueCount != nil {
			t.Fatal("unstarted command gained an observation or zero row count")
		}
	}
}

func TestS02MarkerReceiptStorageFailureKeepsTheWriteFailure(t *testing.T) {
	dir := t.TempDir()
	useEvidenceDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "S02"), []byte("blocked path"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := s02MarkerTestProbe()
	probe.writeFile = writeScenarioFile
	_, file, note := runFailedS02Marker(t, probe)
	if file != "" || note == "" {
		t.Fatalf("failed storage returned an evidence path: file=%q note=%q", file, note)
	}
}

func TestS02MarkerReceiptRedactsAndBoundsTextWithoutDroppingMembers(t *testing.T) {
	probe := s02MarkerTestProbe()
	const secret = "private-marker-token"
	probe.redact = evidence.NewRedactor(secret)
	probe.identity = func(context.Context, string) (string, error) { return secret, errors.New(secret) }
	probe.selectSQL = func(context.Context, string, string) (string, error) {
		return s02MarkerRows(strings.Repeat(secret, 30000)), nil
	}
	var stored []byte
	probe.writeFile = func(path string, data []byte) string { stored = data; return path }
	receipt, file, note := runFailedS02Marker(t, probe)
	if file == "" || note == "" || !receipt.Truncated || len(receipt.Members) != 3 ||
		int64(len(stored)) > evidence.DefaultLogLimits().SourceBytes || strings.Contains(string(stored), secret) {
		t.Fatalf("size/redaction lost the receipt: file=%q note=%q bytes=%d members=%d",
			file, note, len(stored), len(receipt.Members))
	}
	for _, member := range receipt.Members {
		if !member.Select.TextTruncated || !member.ValuesTruncated || member.ValueCount == nil || *member.ValueCount != 1 ||
			member.Select.StartedAt == nil || member.Identity.Status != s02Unavailable {
			t.Fatalf("bounded text lost metadata: %+v", member)
		}
	}
}

func TestS02MarkerReceiptSerializationFailureKeepsTheWriteFailure(t *testing.T) {
	probe := s02MarkerTestProbe()
	probe.now = func() time.Time { return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }
	_, file, note := runFailedS02Marker(t, probe)
	if file != "" || note == "" {
		t.Fatal("invalid timestamp serialized as a usable receipt")
	}
}

func TestS02MarkerProductionAdaptersExecuteOnlyOneWriteAndOneReadPerMember(t *testing.T) {
	freshLedger(t)
	dir := t.TempDir()
	useEvidenceDir(t, filepath.Join(dir, "evidence"))
	files := map[string]string{markerWriteKind: failedS02MarkerOutput, "row": s02MarkerRows("marker"),
		"pair": s02MarkerRows("marker") + failedS02MarkerOutput}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv("S02_DIAGNOSTIC_CALLS", calls)
	t.Setenv("S02_DIAGNOSTIC_WRITE", filepath.Join(dir, markerWriteKind))
	t.Setenv("S02_DIAGNOSTIC_ROW", filepath.Join(dir, "row"))
	t.Setenv("S02_DIAGNOSTIC_PAIR", filepath.Join(dir, "pair"))
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$S02_DIAGNOSTIC_CALLS"
case "$*" in
 *"INSERT INTO s02_marker"*) cat "$S02_DIAGNOSTIC_WRITE" ;;
 *"SELECT v FROM s02_marker"*)
  case "$*" in *"exec hab-1 "*) cat "$S02_DIAGNOSTIC_PAIR" ;; *) cat "$S02_DIAGNOSTIC_ROW" ;; esac ;;
 *"ha_mode"*) printf 'cub_server 41 500\nha_mode active\n' ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := newHARun("marker-ns", "client-ns", "ha", "appdb",
		[]string{markerFirstMember, markerSecondMember, markerThirdMember})
	out, file, _, err := run.writeS02Marker(context.Background(), markerFirstMember, "marker", time.Now())
	var statement *sqlcheck.StatementError
	if out != failedS02MarkerOutput || !errors.As(err, &statement) || file != s02MarkerReceiptFile {
		t.Fatalf("strict production adapter changed write: file=%q err=%v", file, err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 7 || !strings.Contains(lines[0], "INSERT INTO s02_marker") {
		t.Fatalf("unexpected command/replay count: %q", lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, " -c cubrid ") {
			t.Fatalf("command used an implicit container: %q", line)
		}
	}
	data, err = os.ReadFile(filepath.Join(dir, "evidence", file))
	if err != nil {
		t.Fatal(err)
	}
	var receipt s02MarkerReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Members[1].MarkerObservation != evidence.Unknown || receipt.Members[1].Select.Status != s02Unavailable {
		t.Fatal("paired diagnostic SELECT was tolerated or reread")
	}
	records := lostRecords(t, filepath.Join(dir, "evidence"))
	if len(records) != 2 || records[0].Kind != markerWriteKind || records[0].Identity != sqlcheck.IdentityNotRead ||
		records[0].ReRead != sqlcheck.ReReadNotMade || records[1].Kind != markerReadKind {
		t.Fatalf("ledger was rewritten: %+v", records)
	}
}

func TestS02MarkerPartialDiagnosticResultsStayUnavailable(t *testing.T) {
	for name, output := range map[string]string{
		"header without footer":      "\n  v\n=====\n",
		"row without footer":         "\n  v\n=====\n  'marker'\n",
		"mismatched count":           "\n  v\n=====\n  'marker'\n2 rows selected.\n",
		"truncated footer":           "\n  v\n=====\n  'marker'\n1 row selected",
		"multiple results":           s02MarkerRows("marker") + s02MarkerRows("marker"),
		"conflicting empty response": "There are no results.\n" + s02MarkerRows("marker"),
	} {
		t.Run(name, func(t *testing.T) {
			probe := s02MarkerTestProbe()
			probe.selectSQL = func(context.Context, string, string) (string, error) { return output, nil }
			receipt, _, _ := runFailedS02Marker(t, probe)
			for _, member := range receipt.Members {
				if member.Select.Status != s02Unavailable || member.MarkerObservation != evidence.Unknown ||
					member.ValueCount != nil || member.Select.Output != output || member.Select.StartedAt == nil {
					t.Fatalf("incomplete SELECT became an observation: %+v", member)
				}
			}
		})
	}
}

func TestS02MarkerCompleteEmptyResultIsKnown(t *testing.T) {
	for _, out := range []string{"There are no results.\n", "\n  v\n=====\n0 rows selected.\n"} {
		probe := s02MarkerTestProbe()
		probe.selectSQL = func(context.Context, string, string) (string, error) { return out, nil }
		receipt, _, _ := runFailedS02Marker(t, probe)
		for _, member := range receipt.Members {
			if member.MarkerObservation != s02NoValues || member.ValueCount == nil ||
				*member.ValueCount != 0 || member.Values == nil {
				t.Fatalf("complete empty response became unknown: %+v", member)
			}
		}
	}
}

func TestS02MarkerCompleteNativeReadFooterIsRetained(t *testing.T) {
	probe := s02MarkerTestProbe()
	// This existing one-column native SELECT receipt includes an autocommit
	// suffix (POC-56, samples/e2e-image/setup/b-row-before-backup.txt). It says
	// nothing about the separate failed marker write or this run's engine.
	const native = "\n=== <Result of SELECT Command in Line 1> ===\n\n  'row='||origin      \n==========" +
		"============\n  'row=cluster-b-only'\n\n1 row selected. (0.000000 sec) Committed. (" +
		"0.000000 sec) \n"
	probe.selectSQL = func(context.Context, string, string) (string, error) { return native, nil }
	receipt, _, _ := runFailedS02Marker(t, probe)
	for _, member := range receipt.Members {
		if member.Select.Status != s02Answered || member.ValueCount == nil || *member.ValueCount != 1 ||
			!reflect.DeepEqual(member.Values, []string{"row=cluster-b-only"}) {
			t.Fatalf("complete native SELECT was lost: %+v", member)
		}
	}
}
