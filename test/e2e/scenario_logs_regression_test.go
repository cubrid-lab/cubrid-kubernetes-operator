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
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testLogScenario = "S03/abrupt"
	testLogIndex    = testLogScenario + "/logs/index.json"
	// fakeKubectl plays a cluster of one Operator Pod, one member and one
	// Broker, and records each command it is given.
	fakeKubectl = `#!/bin/sh
echo "$*" | tr '\n' ' ' >> "$KUBECTL_CALLS"
echo >> "$KUBECTL_CALLS"
case "$*" in
*"get pods -l control-plane"*) printf 'manager-1\tuid-m\tcontrol\t0\n' ;;
*"get pods -l app.kubernetes.io/instance"*) printf 'ha-0\tuid-0\tn0\t1\n' ;;
*"get pods -l database.cubrid.io"*) printf 'ha-broker-rw-1\tuid-b\tn1\t\n' ;;
*"logs "*"--previous"*) echo '2026-11-06T09:09:00.000000000Z before the restart' ;;
*"logs "*) echo '2026-11-06T09:10:00.000000000Z a line' ;;
*"ls -1t"*) echo 'server/appdb_20261106_0910.err' ;;
*"heartbeat status"*) echo 'HA-Node Info (current ha-0, state master)' ;;
*"tail -c"*) echo 'Time: 11/06/26 09:10:11.123 - an engine line' ;;
*) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	// hangingKubectl never answers, and neither does the child that holds its
	// output open.
	silentKubectl = "#!/bin/sh\nsleep 60\n"
)

// useKubectl puts a stand-in kubectl first on PATH and returns the file its
// commands are recorded in.
func useKubectl(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("KUBECTL_CALLS", calls)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func testLogs(dir string) *scenarioLogs {
	return newScenarioLogs(dir, testLogScenario, evidence.ClusterLogPlan("system", "ns", "ha", []string{"ha-0"}, true),
		kubectlLogs{}, evidence.DefaultLogLimits(), evidence.NewLogBudget(1<<30, time.Hour))
}

// recorded is the command lines the stand-in kubectl received.
func recorded(t *testing.T, calls string) []string {
	t.Helper()
	data, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// The bundle of a scenario holds the Operator's, the Instance Manager's and
// the engine's logs and the raw heartbeat output, and the reader only reads:
// a list, a log, or one of the fixed commands in a Pod.
func TestScenarioLogsCollectEverySourceAndOnlyRead(t *testing.T) {
	calls := useKubectl(t, fakeKubectl)
	dir := t.TempDir()
	logs := testLogs(dir)
	logs.beforeFault(t.Context())
	var result evidence.Scenario
	logs.conclude(t.Context(), true, &result)

	lines := recorded(t, calls)
	if len(lines) == 0 {
		t.Fatal("no kubectl command ran")
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "--request-timeout=") {
			t.Fatalf("command without a request timeout: %q", line)
		}
		if verb := fields[3]; !slices.Contains([]string{"get", "logs", "exec"}, verb) {
			t.Errorf("command that is not a read: %q", line)
		}
		if strings.Contains(line, " logs ") && !strings.Contains(line, "--timestamps") {
			t.Errorf("a log read without timestamps cannot be merged: %q", line)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, testLogIndex))
	if err != nil {
		t.Fatal(err)
	}
	var index evidence.LogIndex
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Checkpoints) != 2 {
		t.Fatalf("%d checkpoints", len(index.Checkpoints))
	}
	seen := map[string]evidence.LogStatus{}
	for _, s := range index.Checkpoints[1].Sources {
		seen[s.Component] = s.Status
	}
	for _, component := range []string{evidence.LogOperator, evidence.LogInstanceManager, evidence.LogEngine,
		evidence.LogHeartbeat, evidence.LogBrokerProcess, evidence.LogBroker} {
		if seen[component] != evidence.LogCollected {
			t.Errorf("%s: %q, want collected", component, seen[component])
		}
	}
}

// Before the fault has ended, no command asks CUBRID's heartbeat anything.
func TestScenarioLogsAskNoHeartbeatBeforeTheFaultEnded(t *testing.T) {
	calls := useKubectl(t, fakeKubectl)
	logs := testLogs(t.TempDir())
	logs.beforeFault(t.Context())
	var result evidence.Scenario
	logs.conclude(t.Context(), false, &result)
	for _, line := range recorded(t, calls) {
		if strings.Contains(line, "heartbeat") {
			t.Errorf("ran while a fault may be active: %q", line)
		}
	}
}

// A kubectl that hangs costs a bounded time, and the scenario goes on.
func TestScenarioLogsBoundAHungKubectl(t *testing.T) {
	useKubectl(t, silentKubectl)
	limits := evidence.DefaultLogLimits()
	limits.SourceTimeout, limits.CheckpointTimeout = 100*time.Millisecond, 500*time.Millisecond
	dir := t.TempDir()
	logs := newScenarioLogs(dir, testLogScenario, evidence.ClusterLogPlan("system", "ns", "ha", []string{"ha-0"}, true),
		kubectlLogs{}, limits, evidence.NewLogBudget(1<<30, time.Hour))
	var result evidence.Scenario
	start := time.Now()
	logs.conclude(t.Context(), true, &result)
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("took %s", took)
	}
	if !slices.Contains(result.Evidence, testLogIndex) {
		t.Fatalf("evidence %v", result.Evidence)
	}
}

// The verdict is the scenario's own. Whether the logs can be read or written,
// and whatever the result is, the result and its reason stay as they were, and
// the judged result does not change: a listed index always exists.
func TestScenarioLogsNeverChangeAVerdict(t *testing.T) {
	const listed = "S03/timeline.jsonl"
	yes := true
	for name, setup := range map[string]func(t *testing.T, dir string){
		"a working cluster": func(t *testing.T, _ string) { useKubectl(t, fakeKubectl) },
		"no kubectl":        func(t *testing.T, _ string) { t.Setenv("PATH", t.TempDir()) },
		"a kubectl that fails": func(t *testing.T, _ string) {
			useKubectl(t, "#!/bin/sh\necho refused >&2\nexit 1\n")
		},
		"a directory that cannot be written": func(t *testing.T, dir string) {
			useKubectl(t, fakeKubectl)
			// A file where the scenario's directory has to be.
			if err := os.WriteFile(filepath.Join(dir, testLogScenario), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		for _, start := range []evidence.Result{evidence.Pass, evidence.Fail, evidence.Blocked} {
			t.Run(name+"/"+string(start), func(t *testing.T) {
				dir := t.TempDir()
				if err := os.MkdirAll(filepath.Join(dir, "S03"), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, listed), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				setup(t, dir)
				oldNotes := runSummary.Notes
				t.Cleanup(func() { runSummary.Notes = oldNotes })
				result := evidence.Scenario{ID: "S03", Variant: "abrupt", Result: start, Reason: "the scenario's reason",
					FaultConfirmed: &yes, Evidence: []string{listed}}
				wantResult, wantReason := result.Judge(dir)

				testLogs(dir).conclude(t.Context(), true, &result)

				if result.Result != start || result.Reason != "the scenario's reason" || len(result.NeverEvents) != 0 {
					t.Fatalf("the result changed: %+v", result)
				}
				if got, reason := result.Judge(dir); got != wantResult || reason != wantReason {
					t.Fatalf("judged %q (%s), want %q (%s)", got, reason, wantResult, wantReason)
				}
			})
		}
	}
}

// A run without an evidence directory neither collects nor lists anything.
func TestScenarioLogsNeedAnEvidenceDirectory(t *testing.T) {
	calls := useKubectl(t, fakeKubectl)
	logs := testLogs("")
	logs.beforeFault(t.Context())
	var result evidence.Scenario
	logs.conclude(t.Context(), true, &result)
	if got := recorded(t, calls); len(got) != 0 {
		t.Fatalf("ran %q", got)
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("evidence %v", result.Evidence)
	}
}
