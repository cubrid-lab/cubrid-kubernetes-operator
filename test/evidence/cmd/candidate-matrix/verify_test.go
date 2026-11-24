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

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	candidateCommit    = "cccccccccccccccccccccccccccccccccccccccc"
	candidateOperator  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	candidateCUBRID    = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	kindRunID          = "candidate-kind-run"
	powerOffRunID      = "candidate-vm-power-off-run"
	vmRunID            = "candidate-vm-run"
	overallGo          = "Overall v0.1 verdict: AUTOMATED CHECKS GO — manual gates pending"
	overallNoGo        = "Overall v0.1 verdict: NO-GO"
	overallReview      = "Overall v0.1 verdict: REVIEW REQUIRED"
	oneBrokerPodKey    = "broker-failure-recovery-one-pod"
	restoreExact       = "installed-restore-new-cluster-exact"
	rebuildKey         = "ha-pvc-loss-rebuild"
	labRebuildKey      = "installed-ha-pvc-loss-rebuild"
	judgedBlocked      = "judged blocked"
	noRecord           = "no result was recorded"
	passedButNoGo      = "| PASS | PASS | NO-GO |"
	listedState        = "listed"
	unexplained        = "no judged result accounts for"
	failoverUnmeasured = "failoverWithin is not measured"
	otherImages        = "image digests are not the candidate's"
)

// A candidate whose every registered v0.1 variant passed, with each measured
// variant's measurements recorded, on one commit and one pair of images, is
// the only input that verifies as Go.
func TestVerifierAcceptsACompleteCandidate(t *testing.T) {
	root, manifest := buildCandidate(t, nil)
	out, ok := runVerifier(t, root, manifest, v01Families)
	if !ok || !strings.Contains(out, overallGo) {
		t.Fatalf("a complete candidate did not verify as Go:\n%s", out)
	}
	if !strings.Contains(out, "## Manual gates not checked") || !strings.Contains(out, "24-hour soak") ||
		!strings.Contains(out, "release approval") {
		t.Errorf("the report does not list the manual gates:\n%s", out)
	}
	for _, key := range []string{gracefulKey, powerOffKey, masterIsolatedKey, oneBrokerPodKey, restoreExact} {
		if row := requiredRow(t, out, key); !strings.Contains(row, "| GO |") {
			t.Errorf("required variant %s is not Go: %s", key, row)
		}
	}
}

// Replication lag under repeated faults (S08) is counted through repeated
// S04 and S07 runs (maintainer decision 2026-11-23, #126), so a candidate
// without any S08 result is judged on everything else, the S08 coverage is
// listed as a manual gate, and S08 is not a required variant.
func TestVerifierDoesNotRequireAnS08Result(t *testing.T) {
	root, manifest := buildCandidate(t, nil)
	out, ok := runVerifier(t, root, manifest, v01Families)
	if !ok || !strings.Contains(out, overallGo) {
		t.Fatalf("a candidate without an S08 result was not Go:\n%s", out)
	}
	if strings.Contains(requiredTable(out), "S08") {
		t.Errorf("S08 is listed as a required variant:\n%s", out)
	}
	if !strings.Contains(out, "- S08 coverage: repeated S04 and S07 runs on the candidate; count not yet decided\n") {
		t.Errorf("the S08 coverage is not listed as a manual gate:\n%s", out)
	}
	want, err := os.ReadFile("testdata/verify-sample.md")
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("verifier report differs from testdata/verify-sample.md:\n%s", out)
	}
}

// A recorded S08 result is shown but never counted: a pass or not_run is
// NOT COUNTED, and a fail, blocked or failed cleanup is REVIEW REQUIRED like
// any other result that is not counted.
func TestVerifierDoesNotCountRecordedS08Results(t *testing.T) {
	tests := []struct {
		result  evidence.Result
		overall string
		row     string
	}{
		{evidence.Pass, overallGo, "| PASS | PASS | NOT COUNTED | " + s08Note + " |"},
		{evidence.NotRun, overallGo, "| NOT_RUN | NOT_RUN | NOT COUNTED |"},
		{evidence.Fail, overallReview, "| FAIL | FAIL | REVIEW REQUIRED |"},
		{evidence.Blocked, overallReview, "| BLOCKED | BLOCKED | REVIEW REQUIRED |"},
	}
	for _, tc := range tests {
		t.Run(string(tc.result), func(t *testing.T) {
			root, manifest := buildCandidate(t, func(runs []evidence.Summary) {
				lag := passed("S08", "")
				if tc.result != evidence.Pass {
					setResult(&lag, tc.result, "second failure not recovered")
				}
				if tc.result == evidence.Fail || tc.result == evidence.Blocked {
					runs[0].Passed = false
					runs[0].Problems = []string{lag.Name() + ": " + string(tc.result) + ": second failure not recovered"}
				}
				runs[0].Scenarios = append(runs[0].Scenarios, lag)
			})
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok != (tc.overall == overallGo) || !strings.Contains(out, tc.overall) {
				t.Fatalf("an S08 %s did not give %q:\n%s", tc.result, tc.overall, out)
			}
			if !strings.Contains(out, "COVERED BY REPEATED S04 AND S07 RUNS") || !strings.Contains(out, tc.row) {
				t.Errorf("the S08 result is not shown as %q:\n%s", tc.row, out)
			}
			if strings.Contains(requiredTable(out), "S08") {
				t.Errorf("S08 is listed as a required variant:\n%s", out)
			}
		})
	}
}

// A must-pass variant that is missing, or whose result is anything but a
// pass supported by its evidence, is No-Go.
func TestVerifierBlocksMissingOrUnsuccessfulMustPassResults(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		mutate func([]evidence.Summary)
		want   string
	}{
		{"missing variant", oneBrokerPodKey, func(runs []evidence.Summary) {
			removeScenario(runs, kindRunID, "S05/one-broker-pod")
		}, noRecord},
		{"blocked", restoreExact, func(runs []evidence.Summary) {
			setResult(scenarioOf(runs, vmRunID, "S12/exact"), evidence.Blocked, "measurement_missing")
		}, judgedBlocked},
		// A Kind rebuild result blocked, as with rebuild_limit unset, keeps
		// the candidate No-Go.
		{"rebuild blocked without its limit", rebuildKey, func(runs []evidence.Summary) {
			setResult(scenarioOf(runs, kindRunID, "S10"), evidence.Blocked, "time_limit_unset")
		}, judgedBlocked},
		{"rebuild missing", rebuildKey, func(runs []evidence.Summary) {
			removeScenario(runs, kindRunID, "S10")
		}, noRecord},
		// S10 is required at both levels: a Kind pass does not stand for the
		// installed Operator's VM-lab result, which a blocked or missing
		// record leaves No-Go.
		{"lab rebuild blocked without its limit", labRebuildKey, func(runs []evidence.Summary) {
			setResult(scenarioOf(runs, vmRunID, "S10"), evidence.Blocked, "time_limit_unset")
		}, judgedBlocked},
		{"lab rebuild missing", labRebuildKey, func(runs []evidence.Summary) {
			removeScenario(runs, vmRunID, "S10")
		}, noRecord},
		{"not run", restoreExact, func(runs []evidence.Summary) {
			setResult(scenarioOf(runs, vmRunID, "S12/exact"), evidence.NotRun, "skipped")
		}, "judged not_run"},
		{"fail", oneBrokerPodKey, func(runs []evidence.Summary) {
			setResult(scenarioOf(runs, kindRunID, "S05/one-broker-pod"), evidence.Fail, "no new connection")
		}, "judged fail"},
		{"not applicable", oneBrokerPodKey, func(runs []evidence.Summary) {
			setResult(scenarioOf(runs, kindRunID, "S05/one-broker-pod"), evidence.NotApplicable, "not here")
		}, "judged not_applicable"},
		{"pass without listed evidence", oneBrokerPodKey, func(runs []evidence.Summary) {
			scenarioOf(runs, kindRunID, "S05/one-broker-pod").Evidence = nil
		}, "no evidence is listed"},
		{"cleanup failed", oneBrokerPodKey, func(runs []evidence.Summary) {
			failed := false
			scenarioOf(runs, kindRunID, "S05/one-broker-pod").CleanupSucceeded = &failed
		}, "the cleanup did not succeed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, tc.mutate)
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok || !strings.Contains(out, overallNoGo) {
				t.Fatalf("not No-Go:\n%s", out)
			}
			if row := requiredRow(t, out, tc.key); !strings.Contains(row, "| NO-GO |") ||
				!strings.Contains(row, tc.want) {
				t.Errorf("row of %s does not say %q: %s", tc.key, tc.want, row)
			}
		})
	}
}

// A bundle that is missing, truncated, unreadable, unfinished or changed
// after it was indexed proves nothing, and the candidate is No-Go.
func TestVerifierBlocksBrokenOrTruncatedEvidence(t *testing.T) {
	truncate := func(t *testing.T, root string) {
		path := filepath.Join(root, vmRunID, summaryPath)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		mutate func([]evidence.Summary)
		after  func(*testing.T, string, *index)
		want   string
	}{
		{"truncated summary", nil, func(t *testing.T, root string, _ *index) {
			truncate(t, root)
		}, "summary.json: DIGEST MISMATCH"},
		{"truncated summary indexed with its digest", nil, func(t *testing.T, root string, manifest *index) {
			truncate(t, root)
			data, err := os.ReadFile(filepath.Join(root, vmRunID, summaryPath))
			if err != nil {
				t.Fatal(err)
			}
			for i, file := range manifest.Runs[2].Files {
				if file.Path == summaryPath {
					manifest.Runs[2].Files[i].SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
				}
			}
		}, "summary.json could not be read"},
		{"empty summary", nil, func(t *testing.T, root string, manifest *index) {
			if err := os.WriteFile(filepath.Join(root, vmRunID, summaryPath), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "summary.json: DIGEST MISMATCH"},
		{"missing bundle", nil, func(t *testing.T, root string, manifest *index) {
			if err := os.RemoveAll(filepath.Join(root, vmRunID)); err != nil {
				t.Fatal(err)
			}
		}, "the bundle is missing"},
		{"altered evidence file", nil, func(t *testing.T, root string, manifest *index) {
			path := filepath.Join(root, vmRunID, "S12", "exact", "evidence.txt")
			if err := os.WriteFile(path, []byte("changed after indexing\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "DIGEST MISMATCH"},
		{"unfinished run", func(runs []evidence.Summary) {
			runs[2].FinishedAt = time.Time{}
		}, nil, "the run did not finish"},
		{"missing junit.xml", nil, func(t *testing.T, root string, manifest *index) {
			if err := os.Remove(filepath.Join(root, vmRunID, junitPath)); err != nil {
				t.Fatal(err)
			}
		}, "junit.xml: MISSING EVIDENCE"},
		{"indexed file absent with a bogus digest", nil, func(t *testing.T, root string, manifest *index) {
			manifest.Runs[2].Files = append(manifest.Runs[2].Files,
				indexedFile{Path: "manager.log", SHA256: strings.Repeat("e", 64), State: listedState})
		}, "manager.log: MISSING EVIDENCE"},
		{"empty evidence file", nil, func(t *testing.T, root string, manifest *index) {
			path := filepath.Join("S12", "exact", "evidence.txt")
			if err := os.WriteFile(filepath.Join(root, vmRunID, path), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			for i, file := range manifest.Runs[2].Files {
				if file.Path == path {
					manifest.Runs[2].Files[i].SHA256 = fmt.Sprintf("%x", sha256.Sum256(nil))
				}
			}
		}, "S12/exact/evidence.txt: EMPTY EVIDENCE"},
		{"credential leak", func(runs []evidence.Summary) {
			runs[2].Passed = false
			runs[2].Problems = []string{"the evidence file S12/exact/evidence.txt holds a credential of the run"}
		}, nil, unexplained},
		{"unlabelled step", func(runs []evidence.Summary) {
			runs[2].Passed = false
			runs[2].Problems = []string{"S12/exact: no step of the suite carries the label S12/exact"}
		}, nil, unexplained},
		{"suite deadline", func(runs []evidence.Summary) {
			runs[2].Passed = false
			runs[2].Problems = []string{"the suite's deadline passed during S15: the steps after it did not run"}
		}, nil, unexplained},
		{"not passed without a problem", func(runs []evidence.Summary) {
			runs[2].Passed = false
		}, nil, "the run recorded that it did not pass, with no problem listed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, tc.mutate)
			if tc.after != nil {
				tc.after(t, root, &manifest)
			}
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok || !strings.Contains(out, overallNoGo) {
				t.Fatalf("not No-Go:\n%s", out)
			}
			if row := requiredRow(t, out, restoreExact); !strings.Contains(row, "| NO-GO |") {
				t.Errorf("S12 exact from the broken run is not No-Go: %s", row)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the report does not say %q:\n%s", tc.want, out)
			}
		})
	}
}

// A filtered Kind run selected its own requirements and an unregistered lane
// validates nothing, so neither can stand for the candidate.
func TestVerifierBlocksFilteredOrUnregisteredLanes(t *testing.T) {
	tests := []struct {
		name, key string
		mutate    func([]evidence.Summary)
	}{
		{"filtered Kind run", oneBrokerPodKey, func(runs []evidence.Summary) { runs[0].Lane = "kind-filtered" }},
		{"unregistered lane", restoreExact, func(runs []evidence.Summary) { runs[2].Lane = "lab-unregistered" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, tc.mutate)
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok || !strings.Contains(out, overallNoGo) || !strings.Contains(out, "is not a registered lane") {
				t.Fatalf("not No-Go for its lane:\n%s", out)
			}
			if row := requiredRow(t, out, tc.key); !strings.Contains(row, "| NO-GO |") {
				t.Errorf("%s from that run is not No-Go: %s", tc.key, row)
			}
		})
	}
}

// Every result must come from the candidate's commit and image digests.
func TestVerifierBlocksResultsOfAnotherCandidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]evidence.Summary)
		want   string
	}{
		{"other operator image", func(runs []evidence.Summary) {
			runs[1].Environment.OperatorImageDigest = "sha256:" + strings.Repeat("5", 64)
		}, otherImages},
		{"other CUBRID image", func(runs []evidence.Summary) {
			runs[1].Environment.CubridImageDigest = "sha256:" + strings.Repeat("6", 64)
		}, otherImages},
		{"unknown image", func(runs []evidence.Summary) {
			runs[1].Environment.CubridImageDigest = ""
		}, "image digests are not recorded"},
		{"other commit", func(runs []evidence.Summary) {
			runs[1].Environment.OperatorCommit = strings.Repeat("d", 40)
		}, "source commit is not the candidate's"},
		{"short commit", func(runs []evidence.Summary) {
			runs[1].Environment.OperatorCommit = "ccccccc"
		}, "source commit is not recorded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, tc.mutate)
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok || !strings.Contains(out, overallNoGo) {
				t.Fatalf("not No-Go:\n%s", out)
			}
			if row := requiredRow(t, out, powerOffKey); !strings.Contains(row, "| NO-GO |") ||
				!strings.Contains(row, tc.want) {
				t.Errorf("S04 from the other candidate does not say %q: %s", tc.want, row)
			}
		})
	}
}

// A measured variant must have run with a confirmed fault, record its
// measurements and break no safety rule. A raw fail keeps its raw verdict
// and is not qualified, because no recorded field shows the failure was
// caused by a time limit alone.
func TestVerifierRequiresMeasurementsAndSafetyOfMeasuredVariants(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		mutate func([]evidence.Summary)
		raw    string
		want   string
	}{
		{"unknown measurement", powerOffKey, func(runs []evidence.Summary) {
			scenarioOf(runs, powerOffRunID, "S04/power-off").Measurements["failoverWithin"] = evidence.Unknown
		}, passedButNoGo, failoverUnmeasured},
		{"absent measurement", gracefulKey, func(runs []evidence.Summary) {
			delete(scenarioOf(runs, kindRunID, "S03/graceful").Measurements, "recoveryTime")
		}, passedButNoGo, "recoveryTime is not measured"},
		{"no measurements", masterIsolatedKey, func(runs []evidence.Summary) {
			scenarioOf(runs, vmRunID, "S07/master-isolated").Measurements = nil
		}, passedButNoGo, "is not measured"},
		{"fault not recorded as confirmed", powerOffKey, func(runs []evidence.Summary) {
			scenarioOf(runs, powerOffRunID, "S04/power-off").FaultConfirmed = nil
		}, passedButNoGo, faultUnconfirmed},
		{"acknowledged write after proven closure", masterIsolatedKey, func(runs []evidence.Summary) {
			scenarioOf(runs, vmRunID, "S07/master-isolated").
				Measurements["establishedAcknowledgedAfterPathClosed"] = 2
		}, passedButNoGo, "establishedAcknowledgedAfterPathClosed is 2, not 0"},
		{"never event", masterIsolatedKey, func(runs []evidence.Summary) {
			scenarioOf(runs, vmRunID, "S07/master-isolated").NeverEvents = []string{
				"2 established-session write(s) were acknowledged after the managed -rw path was observed closed"}
		}, "| FAIL | FAIL | NO-GO |", "an outcome that must never happen"},
		{"acknowledged write lost on graceful termination", gracefulKey, func(runs []evidence.Summary) {
			scenarioOf(runs, kindRunID, "S03/graceful").Measurements[acknowledgedMissing] = 1
		}, passedButNoGo, "acknowledgedMissing is 1, not 0"},
		{"timing overrun", gracefulKey, func(runs []evidence.Summary) {
			setResult(scenarioOf(runs, kindRunID, "S03/graceful"), evidence.Fail,
				"failover_limit exceeded: 41.2s > 30s")
		}, "| FAIL | FAIL | NO-GO |", "no recorded field shows that only a time limit failed"},
		{"blocked", powerOffKey, func(runs []evidence.Summary) {
			setResult(scenarioOf(runs, powerOffRunID, "S04/power-off"), evidence.Blocked, "not fenced")
		}, "| BLOCKED | BLOCKED | NO-GO |", judgedBlocked},
		{"empty name", powerOffKey, func(runs []evidence.Summary) {
			scenarioOf(runs, powerOffRunID, "S04/power-off").Measurements["finalMaster"] = ""
		}, passedButNoGo, "finalMaster is not measured"},
		{"empty duration", powerOffKey, func(runs []evidence.Summary) {
			scenarioOf(runs, powerOffRunID, "S04/power-off").Measurements["failoverWithin"] = ""
		}, passedButNoGo, failoverUnmeasured},
		{"duration that is not one", powerOffKey, func(runs []evidence.Summary) {
			scenarioOf(runs, powerOffRunID, "S04/power-off").Measurements["failoverWithin"] = "soon"
		}, passedButNoGo, failoverUnmeasured},
		{"count recorded as a string", gracefulKey, func(runs []evidence.Summary) {
			scenarioOf(runs, kindRunID, "S03/graceful").Measurements[acknowledgedMissing] = "0"
		}, passedButNoGo, "acknowledgedMissing is not measured"},
		{"count recorded as a boolean", masterIsolatedKey, func(runs []evidence.Summary) {
			scenarioOf(runs, vmRunID, "S07/master-isolated").Measurements[membersDiverged] = false
		}, passedButNoGo, "membersDiverged is not measured"},
		{"no new-connection writes", masterIsolatedKey, func(runs []evidence.Summary) {
			scenarioOf(runs, vmRunID, "S07/master-isolated").Measurements["newConnectionWrites"] = map[string]any{}
		}, passedButNoGo, "newConnectionWrites is not measured"},
		{"cleanup failed", powerOffKey, func(runs []evidence.Summary) {
			failed := false
			scenarioOf(runs, powerOffRunID, "S04/power-off").CleanupSucceeded = &failed
		}, passedButNoGo, "the cleanup did not succeed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, tc.mutate)
			before := summaryDigests(t, root)
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok || !strings.Contains(out, overallNoGo) {
				t.Fatalf("not No-Go:\n%s", out)
			}
			if row := requiredRow(t, out, tc.key); !strings.Contains(row, "| NO-GO |") ||
				!strings.Contains(row, tc.want) {
				t.Errorf("row of %s does not say %q: %s", tc.key, tc.want, row)
			}
			if row := recordedRow(t, out, tc.key); !strings.Contains(row, tc.raw) {
				t.Errorf("recorded row of %s does not show %q: %s", tc.key, tc.raw, row)
			}
			if after := summaryDigests(t, root); !slices.Equal(before, after) {
				t.Error("the verifier changed a recorded summary")
			}
		})
	}
}

// Names of the runs and results the disclosed-measurement tests change.
const (
	s03Graceful       = "S03/graceful"
	s04PowerOff       = "S04/power-off"
	s07MasterIsolated = "S07/master-isolated"
	failoverLimit     = "failover_limit"
	quarantineLimit   = "quarantine_limit"
	notQualified      = "not qualified"
	unsetLimit        = "unset"
	thirtySeconds     = "30s"
	faultUnconfirmed  = "the fault is not recorded as confirmed"
	failDisclosed     = "| FAIL | FAIL | MEASURED, DISCLOSED |"
)

// recordedLimits are the limits each runner records, set as a judged run
// sets them.
var recordedLimits = map[string]map[string]string{
	s03Graceful:       {failoverLimit: thirtySeconds, limitRejoin: "5m0s", limitStable: thirtySeconds},
	s04PowerOff:       {failoverLimit: thirtySeconds, limitStable: thirtySeconds, limitVMReturn: "10m0s"},
	s07MasterIsolated: {quarantineLimit: thirtySeconds, limitPartition: "2m0s"},
}

// runnerMeasurement is, by result name, the measurement each runner compares
// with a limit it may record as exceeded.
var runnerMeasurement = map[string]map[string]string{
	s03Graceful:       {failoverLimit: "recoveryTime", limitRejoin: "allMembersBackWithin"},
	s04PowerOff:       {failoverLimit: "failoverWithin"},
	s07MasterIsolated: {quarantineLimit: "quarantineWithin"},
}

// timeLimitOnly records a result as a fail caused by the one limit alone,
// as the runners record it: every limit set, the measurement the limit
// judged recorded as the runner rounds it, the cleanup succeeded, no
// acknowledged write missing, and the run not passed with the result's
// problem line.
func timeLimitOnly(runID, name, limit, value, exceeded string) func([]evidence.Summary) {
	return func(runs []evidence.Summary) {
		sc := scenarioOf(runs, runID, name)
		setResult(sc, evidence.Fail, limit+" exceeded: "+exceeded)
		succeeded := true
		sc.CleanupSucceeded = &succeeded
		sc.Limits = maps.Clone(recordedLimits[name])
		if sc.Limits == nil {
			sc.Limits = map[string]string{}
		}
		sc.Limits[limit] = value
		if _, ok := sc.Measurements[acknowledgedMissing]; !ok && sc.Measurements != nil {
			sc.Measurements[acknowledgedMissing] = 0
		}
		if measurement, ok := runnerMeasurement[name][limit]; ok {
			measuredText, _, _ := strings.Cut(exceeded, " > ")
			measured, err := time.ParseDuration(measuredText)
			if err != nil {
				panic(err)
			}
			sc.Measurements[measurement] = measured.Round(time.Millisecond).String()
		}
		sc.TimeLimitOnly, sc.ExceededLimits = true, map[string]string{limit: exceeded}
		failRun(runs, runID, name+": fail: "+sc.Reason)
	}
}

// noPromotion records the S07 master-isolated result as the runner records a
// run that ended because CUBRID promoted no member.
func noPromotion(runs []evidence.Summary) {
	sc := scenarioOf(runs, vmRunID, s07MasterIsolated)
	setResult(sc, evidence.Blocked, "no sweep pair showed a quarantine trigger while the partition held; "+
		"CUBRID promoted no member")
	succeeded := true
	sc.CleanupSucceeded = &succeeded
	sc.Limits = maps.Clone(recordedLimits[s07MasterIsolated])
	sc.NoPromotion = true
	failRun(runs, vmRunID, s07MasterIsolated+": blocked: "+sc.Reason)
}

// failRun records that a run did not pass, with the problem line of one of
// its results.
func failRun(runs []evidence.Summary, runID, problem string) {
	for i := range runs {
		if runs[i].RunID == runID {
			runs[i].Passed = false
			runs[i].Problems = append(runs[i].Problems, problem)
		}
	}
}

// then applies changes in order.
func then(changes ...func([]evidence.Summary)) func([]evidence.Summary) {
	return func(runs []evidence.Summary) {
		for _, change := range changes {
			change(runs)
		}
	}
}

// A must-measure variant whose raw fail the runner recorded as caused by time
// limits alone, with every required measurement and safety rule recorded, is
// measured and disclosed: Go for the candidate, with the exceeded limits
// shown and the raw fail kept. A master-isolated partition that ended because
// CUBRID promoted no member is disclosed the same way, and shown as no
// failover pass.
func TestVerifierCountsDisclosedMeasurementsAsGo(t *testing.T) {
	tests := []struct {
		name, key string
		mutate    func([]evidence.Summary)
		raw       string
		reason    []string
	}{
		{"graceful termination over failover_limit", gracefulKey,
			timeLimitOnly(kindRunID, s03Graceful, failoverLimit, "30s", "41.2s > 30s"),
			failDisclosed, []string{"failover_limit 41.2s > 30s", "raw fail kept"}},
		{"VM power loss over failover_limit", powerOffKey,
			timeLimitOnly(powerOffRunID, s04PowerOff, failoverLimit, "30s", "48.017s > 30s"),
			failDisclosed, []string{"failover_limit 48.017s > 30s"}},
		{"partition over quarantine_limit", masterIsolatedKey,
			timeLimitOnly(vmRunID, s07MasterIsolated, quarantineLimit, "30s", "41s > 30s"),
			failDisclosed, []string{"quarantine_limit 41s > 30s"}},
		{"graceful termination over rejoin_limit, measured finer than recorded", gracefulKey,
			timeLimitOnly(kindRunID, s03Graceful, limitRejoin, "5m0s", "5m12.3456789s > 5m0s"),
			failDisclosed, []string{"rejoin_limit 5m12.3456789s > 5m0s"}},
		{"master-isolated partition without a promotion", masterIsolatedKey, noPromotion,
			"| BLOCKED | BLOCKED | MEASURED, DISCLOSED — no promotion |",
			[]string{"no member was promoted", "not a failover pass"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, tc.mutate)
			out, ok := runVerifier(t, root, manifest, v01Families)
			if !ok || !strings.Contains(out, overallGo) {
				t.Fatalf("a disclosed measurement did not leave the candidate Go:\n%s", out)
			}
			row := recordedRow(t, out, tc.key)
			if !strings.Contains(row, tc.raw) {
				t.Errorf("recorded row of %s does not show %q: %s", tc.key, tc.raw, row)
			}
			for _, want := range tc.reason {
				if !strings.Contains(row, want) {
					t.Errorf("recorded row of %s does not say %q: %s", tc.key, want, row)
				}
			}
			if row := requiredRow(t, out, tc.key); !strings.Contains(row, "| GO |") {
				t.Errorf("required row of %s is not Go: %s", tc.key, row)
			}
		})
	}
}

// Anything that leaves the cause of a fail, or the end without a promotion,
// unshown keeps the candidate No-Go: a missing field, a must-pass variant, a
// missing measurement or safety rule, a never-event, an unrecorded cleanup,
// an exceeded limit that is not recorded, or a marker on the wrong result.
func TestVerifierRefusesDisclosedMeasurementsWithoutTheirProof(t *testing.T) {
	gracefulLate := timeLimitOnly(kindRunID, s03Graceful, failoverLimit, "30s", "41.2s > 30s")
	partitionLate := timeLimitOnly(vmRunID, s07MasterIsolated, quarantineLimit, "30s", "41s > 30s")
	graceful := func(change func(*evidence.Scenario)) func([]evidence.Summary) {
		return func(runs []evidence.Summary) { change(scenarioOf(runs, kindRunID, s03Graceful)) }
	}
	isolated := func(change func(*evidence.Scenario)) func([]evidence.Summary) {
		return func(runs []evidence.Summary) { change(scenarioOf(runs, vmRunID, s07MasterIsolated)) }
	}
	powerOffLate := timeLimitOnly(powerOffRunID, s04PowerOff, failoverLimit, "30s", "48.017s > 30s")
	powerOff := func(change func(*evidence.Scenario)) func([]evidence.Summary) {
		return func(runs []evidence.Summary) { change(scenarioOf(runs, powerOffRunID, s04PowerOff)) }
	}
	tests := []struct {
		name, key string
		mutate    func([]evidence.Summary)
		want      string
	}{
		{"time-limit-only field absent", gracefulKey, then(gracefulLate, graceful(func(sc *evidence.Scenario) {
			sc.TimeLimitOnly, sc.ExceededLimits = false, nil
		})), notQualified},
		{"a required measurement missing", gracefulKey, then(gracefulLate, graceful(func(sc *evidence.Scenario) {
			delete(sc.Measurements, "recoveryTime")
		})), "recoveryTime is not measured"},
		{"an acknowledged write lost", gracefulKey, then(gracefulLate, graceful(func(sc *evidence.Scenario) {
			sc.Measurements[acknowledgedMissing] = 1
		})), "acknowledgedMissing is 1, not 0"},
		{"a never-event", gracefulKey, then(gracefulLate, graceful(func(sc *evidence.Scenario) {
			sc.NeverEvents = []string{"two masters reported as healthy"}
		})), "an outcome that must never happen"},
		{"the cleanup not recorded", gracefulKey, then(gracefulLate, graceful(func(sc *evidence.Scenario) {
			sc.CleanupSucceeded = nil
		})), "the cleanup is not recorded as succeeded"},
		{"the fault not confirmed", gracefulKey, then(gracefulLate, graceful(func(sc *evidence.Scenario) {
			sc.FaultConfirmed = nil
		})), faultUnconfirmed},
		{"no exceeded limit named", gracefulKey, then(gracefulLate, graceful(func(sc *evidence.Scenario) {
			sc.ExceededLimits = nil
		})), "names no exceeded limit"},
		{"an exceeded limit the result was not judged by", gracefulKey, then(gracefulLate,
			graceful(func(sc *evidence.Scenario) { delete(sc.Limits, failoverLimit) })),
			"limit failover_limit is not recorded"},
		{"an exceeded limit recorded unset", gracefulKey, then(gracefulLate,
			graceful(func(sc *evidence.Scenario) { sc.Limits[failoverLimit] = unsetLimit })),
			`limit failover_limit is "unset"`},
		{"another limit unset beside the exceeded one", gracefulKey, then(gracefulLate,
			graceful(func(sc *evidence.Scenario) { sc.Limits[limitRejoin] = unsetLimit })),
			`limit rejoin_limit is "unset"`},
		{"an overrun that does not exceed the limit", gracefulKey, then(gracefulLate,
			graceful(func(sc *evidence.Scenario) { sc.ExceededLimits[failoverLimit] = "1s > 30s" })),
			"does not exceed the limit"},
		{"an overrun that does not read as durations", gracefulKey, then(gracefulLate,
			graceful(func(sc *evidence.Scenario) { sc.ExceededLimits[failoverLimit] = "late" })),
			"does not read as a measured duration"},
		{"an overrun of another limit value than the recorded one", gracefulKey, then(gracefulLate,
			graceful(func(sc *evidence.Scenario) { sc.ExceededLimits[failoverLimit] = "41.2s > 10s" })),
			"names another limit than the recorded"},
		{"an overrun of a limit that must be reached", gracefulKey, then(gracefulLate,
			graceful(func(sc *evidence.Scenario) { sc.ExceededLimits[limitStable] = "45s > 30s" })),
			"stable_period is not a limit whose overrun alone"},
		{"an overrun of an unknown limit", gracefulKey, then(gracefulLate,
			graceful(func(sc *evidence.Scenario) { sc.ExceededLimits["made_up_limit"] = "2s > 1s" })),
			"made_up_limit is not a limit whose overrun alone"},
		{"VM power loss with an acknowledged write lost", powerOffKey, then(
			timeLimitOnly(powerOffRunID, s04PowerOff, failoverLimit, "30s", "48.017s > 30s"),
			func(runs []evidence.Summary) {
				scenarioOf(runs, powerOffRunID, s04PowerOff).Measurements[acknowledgedMissing] = 3
			}), "acknowledgedMissing is 3, not 0"},
		{"VM power loss without the data check's count", powerOffKey, then(
			timeLimitOnly(powerOffRunID, s04PowerOff, failoverLimit, "30s", "48.017s > 30s"),
			func(runs []evidence.Summary) {
				scenarioOf(runs, powerOffRunID, s04PowerOff).Measurements[acknowledgedMissing] = evidence.Unknown
			}), "acknowledgedMissing is unknown, not 0"},
		{"a partition overrun with rows that differ between members", masterIsolatedKey, then(partitionLate,
			isolated(func(sc *evidence.Scenario) { sc.Measurements[membersDiverged] = 2 })),
			"membersDiverged is 2, not 0"},
		{"no promotion in a baseline run with every limit unset", masterIsolatedKey, then(noPromotion,
			isolated(func(sc *evidence.Scenario) {
				sc.Limits = map[string]string{quarantineLimit: unsetLimit, limitPartition: unsetLimit}
			})), "a baseline run is not a disclosed measurement"},
		{"no promotion without its limits recorded", masterIsolatedKey, then(noPromotion,
			isolated(func(sc *evidence.Scenario) { sc.Limits = nil })), "is not recorded"},
		{"no promotion with rows that differ between members", masterIsolatedKey, then(noPromotion,
			isolated(func(sc *evidence.Scenario) { sc.Measurements[membersDiverged] = 2 })),
			"membersDiverged is 2, not 0"},
		{"the time-limit-only field on a pass", gracefulKey, graceful(func(sc *evidence.Scenario) {
			sc.TimeLimitOnly, sc.ExceededLimits = true, map[string]string{failoverLimit: "41.2s > 30s"}
		}), "marked as caused by a time limit, but recorded pass"},
		{"must-pass variant with the field", oneBrokerPodKey,
			timeLimitOnly(kindRunID, "S05/one-broker-pod", "broker_recovery_limit", "1m0s", "2m0s > 1m0s"),
			"judged fail"},
		{"an acknowledged write after proven closure", masterIsolatedKey, then(partitionLate,
			isolated(func(sc *evidence.Scenario) { sc.Measurements[afterPathClosed] = 2 })),
			"establishedAcknowledgedAfterPathClosed is 2, not 0"},
		{"no promotion without the field", masterIsolatedKey, then(noPromotion,
			isolated(func(sc *evidence.Scenario) { sc.NoPromotion = false })), judgedBlocked},
		{"no promotion with an acknowledged write after closure", masterIsolatedKey, then(noPromotion,
			isolated(func(sc *evidence.Scenario) { sc.Measurements[afterPathClosed] = 1 })),
			"establishedAcknowledgedAfterPathClosed is 1, not 0"},
		{"no promotion without the closure measured", masterIsolatedKey, then(noPromotion,
			isolated(func(sc *evidence.Scenario) { sc.Measurements["rwPodsGoneWithin"] = evidence.Unknown })),
			"rwPodsGoneWithin is not measured"},
		{"no promotion with the fault not confirmed", masterIsolatedKey, then(noPromotion,
			isolated(func(sc *evidence.Scenario) { sc.FaultConfirmed = nil })),
			faultUnconfirmed},
		{"no promotion on a fail", masterIsolatedKey, then(noPromotion, isolated(func(sc *evidence.Scenario) {
			sc.Result = evidence.Fail
		})), "marked as ending without a promotion, but recorded fail"},
		{"both markers", masterIsolatedKey, then(partitionLate, isolated(func(sc *evidence.Scenario) {
			sc.NoPromotion = true
		})), "both"},
		{"no promotion with a partition held shorter than partition_duration", masterIsolatedKey, then(noPromotion,
			isolated(func(sc *evidence.Scenario) { sc.Measurements["partitionHeldFor"] = "10s" })),
			"partitionHeldFor 10s is under partition_duration 2m0s"},
		{"a partition overrun with a partition held shorter than partition_duration", masterIsolatedKey,
			then(partitionLate, isolated(func(sc *evidence.Scenario) { sc.Measurements["partitionHeldFor"] = "10s" })),
			"partitionHeldFor 10s is under partition_duration 2m0s"},
		{"no promotion without the hold measured", masterIsolatedKey, then(noPromotion,
			isolated(func(sc *evidence.Scenario) { sc.Measurements["partitionHeldFor"] = evidence.Unknown })),
			"partitionHeldFor is not measured"},
		{"VM power loss with the clients stable shorter than stable_period", powerOffKey, then(powerOffLate,
			powerOff(func(sc *evidence.Scenario) { sc.Measurements["shortestStableFor"] = "5s" })),
			"shortestStableFor 5s is under stable_period 30s"},
		{"graceful termination with the client stable shorter than stable_period", gracefulKey, then(gracefulLate,
			graceful(func(sc *evidence.Scenario) { sc.Measurements["stableFor"] = "12s" })),
			"stableFor 12s is under stable_period 30s"},
		{"VM power loss with an overrun the recorded failover time contradicts", powerOffKey,
			then(timeLimitOnly(powerOffRunID, s04PowerOff, failoverLimit, "30s", "48s > 30s"),
				powerOff(func(sc *evidence.Scenario) { sc.Measurements["failoverWithin"] = "1s" })),
			`exceeded failover_limit "48s > 30s" does not match the recorded failoverWithin 1s`},
		{"graceful termination with an overrun the recorded recovery time contradicts", gracefulKey,
			then(gracefulLate, graceful(func(sc *evidence.Scenario) { sc.Measurements["recoveryTime"] = "24.512s" })),
			"does not match the recorded recoveryTime 24.512s"},
		{"graceful termination over rejoin_limit without the rejoin measured", gracefulKey,
			then(timeLimitOnly(kindRunID, s03Graceful, limitRejoin, "5m0s", "6m0s > 5m0s"),
				graceful(func(sc *evidence.Scenario) { sc.Measurements["allMembersBackWithin"] = evidence.Unknown })),
			"allMembersBackWithin is not measured"},
		{"a partition overrun with the quarantine time unreadable", masterIsolatedKey, then(partitionLate,
			isolated(func(sc *evidence.Scenario) { sc.Measurements["quarantineWithin"] = "soon" })),
			"quarantineWithin is not measured"},
		{"no promotion on another variant", higherRankKey,
			func(runs []evidence.Summary) {
				sc := scenarioOf(runs, vmRunID, "S07/higher-rank-isolated")
				setResult(sc, evidence.Blocked, "no sweep pair showed a quarantine trigger")
				sc.NoPromotion = true
				failRun(runs, vmRunID, "S07/higher-rank-isolated: blocked: "+sc.Reason)
			}, "only the master-isolated partition"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, tc.mutate)
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok || !strings.Contains(out, overallNoGo) {
				t.Fatalf("not No-Go:\n%s", out)
			}
			if row := requiredRow(t, out, tc.key); !strings.Contains(row, "| NO-GO |") ||
				!strings.Contains(row, tc.want) {
				t.Errorf("row of %s does not say %q: %s", tc.key, tc.want, row)
			}
		})
	}
}

// A result the registry or the v0.1 scope cannot map needs a maintainer's
// review: it is not Go, and it is not silently ignored.
func TestVerifierNeedsReviewForUnmappedResults(t *testing.T) {
	root, manifest := buildCandidate(t, func(runs []evidence.Summary) {
		runs[0].Scenarios = append(runs[0].Scenarios, passed("S05", "not-a-real-variant"))
	})
	out, ok := runVerifier(t, root, manifest, v01Families)
	if ok || !strings.Contains(out, overallReview) {
		t.Fatalf("an unmapped result did not require review:\n%s", out)
	}
	if !strings.Contains(out, "| not-a-real-variant | ") || !strings.Contains(out, "| REVIEW REQUIRED |") {
		t.Errorf("the unmapped result is not shown for review:\n%s", out)
	}
}

// S09 is deferred to v0.2: a recorded S09 result is shown but never
// required. A pass or not_run is not counted; a fail or blocked needs review,
// because the contract keeps S09's held-member safety behavior and a lane
// fails on a failed result it does not require.
func TestVerifierDoesNotCountDeferredResults(t *testing.T) {
	tests := []struct {
		result  evidence.Result
		overall string
		row     string
	}{
		{evidence.Pass, overallGo, "| S09 | clean | v0.2 DEFERRED | PASS | PASS | NOT COUNTED |"},
		{evidence.NotRun, overallGo, "| S09 | clean | v0.2 DEFERRED | NOT_RUN | NOT_RUN | NOT COUNTED |"},
		{evidence.Fail, overallReview, "| S09 | clean | v0.2 DEFERRED | FAIL | FAIL | REVIEW REQUIRED |"},
		{evidence.Blocked, overallReview, "| S09 | clean | v0.2 DEFERRED | BLOCKED | BLOCKED | REVIEW REQUIRED |"},
	}
	for _, tc := range tests {
		t.Run(string(tc.result), func(t *testing.T) {
			root, manifest := buildCandidate(t, func(runs []evidence.Summary) {
				deferred := passed("S09", "clean")
				if tc.result != evidence.Pass {
					setResult(&deferred, tc.result, "rejoin_limit exceeded: 6m > 5m")
				}
				if tc.result == evidence.Fail || tc.result == evidence.Blocked {
					runs[0].Passed = false
					runs[0].Problems = []string{"S09/clean: " + string(tc.result) + ": rejoin_limit exceeded"}
				}
				runs[0].Scenarios = append(runs[0].Scenarios, deferred)
			})
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok != (tc.overall == overallGo) || !strings.Contains(out, tc.overall) {
				t.Fatalf("a deferred S09 %s did not give %q:\n%s", tc.result, tc.overall, out)
			}
			if !strings.Contains(out, tc.row) {
				t.Errorf("the deferred result is not shown as %q:\n%s", tc.row, out)
			}
			if strings.Contains(requiredTable(out), "S09") {
				t.Errorf("S09 is listed as a required variant:\n%s", out)
			}
		})
	}
}

// A problem the runner records is never excused by starting with the name of
// a result without a key: a credential found in a not-counted S09 result's
// evidence still blocks the run.
func TestVerifierBlocksProblemsNamedAfterAKeylessResult(t *testing.T) {
	root, manifest := buildCandidate(t, func(runs []evidence.Summary) {
		runs[0].Scenarios = append(runs[0].Scenarios, passed("S09", "clean"))
		runs[0].Passed = false
		runs[0].Problems = []string{"S09/clean evidence holds a credential of the run"}
	})
	out, ok := runVerifier(t, root, manifest, v01Families)
	if ok || !strings.Contains(out, overallNoGo) || !strings.Contains(out, unexplained) {
		t.Fatalf("an unrelated problem named after S09 did not block the run:\n%s", out)
	}
}

// buildCandidate writes the bundles of testdata/candidate/runs.json, changed
// by mutate, as the evidence package writes a run, and indexes them.
func buildCandidate(t *testing.T, mutate func([]evidence.Summary)) (string, index) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "candidate", "runs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var runs []evidence.Summary
	if err := json.Unmarshal(data, &runs); err != nil {
		t.Fatal(err)
	}
	for i := range runs {
		env := &runs[i].Environment
		env.OperatorCommit, env.OperatorImageDigest, env.CubridImageDigest =
			candidateCommit, candidateOperator, candidateCUBRID
		for j := range runs[i].Scenarios {
			sc := &runs[i].Scenarios[j]
			sc.Evidence = []string{sc.Name() + "/evidence.txt"}
		}
	}
	if mutate != nil {
		mutate(runs)
	}
	root := t.TempDir()
	manifest := index{Version: "1", CreatedAt: runs[0].StartedAt,
		Candidate: candidate{Commit: candidateCommit, images: images{candidateOperator, candidateCUBRID}}}
	for _, rec := range runs {
		dir := filepath.Join(root, rec.RunID)
		for _, sc := range rec.Scenarios {
			for _, path := range sc.Evidence {
				writeFile(t, dir, path, []byte("synthetic evidence of "+sc.Name()+"\n"))
			}
		}
		if err := rec.Write(dir, evidence.NewRedactor()); err != nil {
			t.Fatal(err)
		}
		manifest.Runs = append(manifest.Runs, indexRun(t, dir))
	}
	return root, manifest
}

// indexRun is the index entry of the bundle in dir, as written.
func indexRun(t *testing.T, dir string) run {
	t.Helper()
	read, err := evidence.ReadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := read.Summary
	source := run{ID: rec.RunID, Source: sourceURL, Version: rec.SchemaVersion, Lane: rec.Lane,
		Level: rec.Environment.Level, StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt,
		Environment: runEnvironment{Commit: rec.Environment.OperatorCommit,
			images: images{rec.Environment.OperatorImageDigest, rec.Environment.CubridImageDigest}}}
	paths := []string{summaryPath, junitPath}
	for _, result := range read.Results {
		sc := result.Scenario
		source.Scenarios = append(source.Scenarios, scenario{ID: sc.ID, Key: result.Key, Variant: sc.Variant,
			Recorded: sc.Result, Judged: result.Judged, Evidence: sc.Evidence})
		paths = append(paths, sc.Evidence...)
	}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		source.Files = append(source.Files,
			indexedFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), State: listedState})
	}
	return source
}

func runVerifier(t *testing.T, root string, manifest index, families []string) (string, bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.json")
	writeJSON(t, path, manifest)
	var buf bytes.Buffer
	ok, err := verify(&buf, path, root, families)
	if err != nil {
		t.Fatal(err)
	}
	return buf.String(), ok
}

func passed(id, variant string) evidence.Scenario {
	sc := evidence.Scenario{ID: id, Variant: variant, Result: evidence.Pass}
	sc.Evidence = []string{sc.Name() + "/evidence.txt"}
	return sc
}

func setResult(sc *evidence.Scenario, result evidence.Result, reason string) {
	sc.Result, sc.Reason = result, reason
}

func scenarioOf(runs []evidence.Summary, runID, name string) *evidence.Scenario {
	for i := range runs {
		for j := range runs[i].Scenarios {
			if runs[i].RunID == runID && runs[i].Scenarios[j].Name() == name {
				return &runs[i].Scenarios[j]
			}
		}
	}
	panic("no scenario " + name + " in run " + runID)
}

func removeScenario(runs []evidence.Summary, runID, name string) {
	for i := range runs {
		if runs[i].RunID == runID {
			runs[i].Scenarios = slices.DeleteFunc(runs[i].Scenarios,
				func(sc evidence.Scenario) bool { return sc.Name() == name })
		}
	}
}

func summaryDigests(t *testing.T, root string) []string {
	t.Helper()
	digests := make([]string, 0, 3)
	for _, id := range []string{kindRunID, powerOffRunID, vmRunID} {
		data, err := os.ReadFile(filepath.Join(root, id, summaryPath))
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, fmt.Sprintf("%x", sha256.Sum256(data)))
	}
	return digests
}

// requiredTable is the section of the report with one row per required
// variant.
func requiredTable(out string) string {
	start := strings.Index(out, "## Required variants")
	end := strings.Index(out, "## Recorded results")
	if start < 0 || end < start {
		return ""
	}
	return out[start:end]
}

// requiredRow is the row of a required variant, named by its key or, for a
// family without a registered result, by its ID.
func requiredRow(t *testing.T, out, key string) string {
	t.Helper()
	for line := range strings.SplitSeq(requiredTable(out), "\n") {
		if strings.Contains(line, "| "+key+" |") {
			return line
		}
	}
	t.Fatalf("no required row for %s:\n%s", key, out)
	return ""
}

// recordedRow is the first row of a recorded result with the key.
func recordedRow(t *testing.T, out, key string) string {
	t.Helper()
	start := strings.Index(out, "## Recorded results")
	if start >= 0 {
		for line := range strings.SplitSeq(out[start:], "\n") {
			if strings.Contains(line, "| "+key+" (") {
				return line
			}
		}
	}
	t.Fatalf("no recorded row for %s:\n%s", key, out)
	return ""
}
