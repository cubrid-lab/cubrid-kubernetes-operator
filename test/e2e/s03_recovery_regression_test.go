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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	recoveryDeleted = "hab-0"
	recoveryOther   = "hab-1"
	recoveryThird   = "hab-2"
	recoveryOldUID  = "599d0cd3-6386-4dcf-81da-9a0ce8fc83e2"
	recoveryNewUID  = "844e9e65-8e3c-42ad-b8cb-5bfa955d45eb"
)

func recoveryProbe() s03RecoveryProbe {
	statusRead := 0
	return s03RecoveryProbe{
		members:       []string{recoveryDeleted, recoveryOther, recoveryThird},
		replacement:   func(context.Context) (s03ReplacementObservation, error) { return readyS03Replacement(), nil },
		activeMasters: func(context.Context) []string { return []string{recoveryOther} },
		master: func(context.Context) (string, []string, error) {
			return recoveryOther, []string{recoveryDeleted, recoveryThird}, nil
		},
		status: func(context.Context) (statusSnapshot, error) {
			statusRead++
			return statusSnapshot{primary: recoveryOther, lastObserved: time.Unix(int64(100+statusRead), 0),
				conditions: map[string]string{s03PrimaryResolved: s03ConditionTrue}}, nil
		},
		formationTime:          func(context.Context) (time.Duration, error) { return time.Second, nil },
		lastAnswerAcknowledged: func(context.Context) bool { return true },
		now:                    func() time.Time { return time.Date(2026, 11, 16, 7, 40, 15, 0, time.UTC) },
	}
}

func TestS03RecoveryCharacterization(t *testing.T) {
	tests := map[string]struct {
		change        func(*s03RecoveryProbe)
		pass          bool
		errorContains string
	}{
		"eventual recovery": {pass: true},
		"no active master yet": {change: func(p *s03RecoveryProbe) {
			p.activeMasters = func(context.Context) []string { return nil }
		}},
		"full roles unavailable": {change: func(p *s03RecoveryProbe) {
			p.master = func(context.Context) (string, []string, error) { return "", nil, errors.New("unreadable roles") }
		}},
		"formation unavailable": {change: func(p *s03RecoveryProbe) {
			p.formationTime = func(context.Context) (time.Duration, error) { return 0, errors.New("formation unavailable") }
		}},
		"client has no acknowledgement": {change: func(p *s03RecoveryProbe) {
			p.lastAnswerAcknowledged = func(context.Context) bool { return false }
		}},
		"status unavailable": {change: func(p *s03RecoveryProbe) {
			p.status = func(context.Context) (statusSnapshot, error) {
				return statusSnapshot{}, errors.New("unreadable status")
			}
		}},
		"real second master change": {change: func(p *s03RecoveryProbe) {
			p.master = func(context.Context) (string, []string, error) { return recoveryThird, nil, nil }
		}, errorContains: "the master changed again"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			probe := recoveryProbe()
			if tc.change != nil {
				tc.change(&probe)
			}
			check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			done, err := check(context.Background())
			if err == nil && !done {
				done, err = check(context.Background())
			}
			if done != tc.pass || tc.errorContains == "" && err != nil ||
				tc.errorContains != "" && (err == nil || !strings.Contains(err.Error(), tc.errorContains)) {
				t.Fatalf("done=%t err=%v", done, err)
			}
			if tc.pass && (state.newMaster != recoveryOther || state.electedAt.IsZero() || state.noticedAt.IsZero()) {
				t.Fatalf("lost recovery state: %+v", state)
			}
		})
	}
}

func TestS03RecoveryCharacterizationConfirmsMultipleClaims(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		name := map[bool]string{false: "mid-sweep handover retries", true: "confirmed two masters fails"}[confirmed]
		t.Run(name, func(t *testing.T) {
			probe := recoveryProbe()
			sweeps := 0
			probe.activeMasters = func(context.Context) []string {
				sweeps++
				if sweeps == 1 || confirmed {
					return []string{recoveryDeleted, recoveryOther}
				}
				return []string{recoveryOther}
			}
			check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			done, err := check(context.Background())
			if done || (err != nil) != confirmed || sweeps != 2 || state.newMaster != "" {
				t.Fatalf("done=%t err=%v sweeps=%d state=%+v", done, err, sweeps, state)
			}
		})
	}
}

func TestS03RecoveryCharacterizationPassesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := recoveryProbe()
	probe.activeMasters = func(got context.Context) []string {
		if got != ctx {
			t.Fatal("caller context was lost")
		}
		cancel()
		return nil
	}
	check, _ := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
	if done, err := check(ctx); done || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled observation done=%t err=%v", done, err)
	}
}

func readyS03Replacement() s03ReplacementObservation {
	return s03ReplacementObservation{UID: recoveryNewUID, PodReady: true, ContainerID: "containerd://replacement",
		ContainerReady: true, RunningStarted: time.Date(2026, 11, 15, 7, 40, 14, 0, time.UTC)}
}

// The Pending replacement UID and old exported timestamp are retained facts
// from failed run 43688936431. The first active claim and matching primary are
// control-flow-derived inputs, not retained callback stdout. Later resolved
// status samples are synthetic controls; they do not reconstruct missing data.
func pendingReplacementReplay(frame *int) s03RecoveryProbe {
	probe := recoveryProbe()
	probe.replacement = func(context.Context) (s03ReplacementObservation, error) {
		return s03ReplacementObservation{UID: recoveryNewUID}, nil
	}
	probe.activeMasters = func(context.Context) []string {
		if *frame == 0 {
			return []string{recoveryDeleted}
		}
		return []string{recoveryOther}
	}
	probe.master = func(context.Context) (string, []string, error) {
		if *frame == 0 {
			return "", nil, errors.New("full roles unavailable in replay")
		}
		return recoveryOther, []string{recoveryDeleted, recoveryThird}, nil
	}
	probe.status = func(context.Context) (statusSnapshot, error) {
		if *frame == 0 {
			return statusSnapshot{primary: recoveryDeleted, lastObserved: time.Date(2026, 11, 15, 7, 40, 3, 0, time.UTC)}, nil
		}
		return statusSnapshot{primary: recoveryOther, lastObserved: time.Date(2026, 11, 16, 7, 40, 14+*frame, 0, time.UTC),
			conditions: map[string]string{s03PrimaryResolved: s03ConditionTrue}}, nil
	}
	return probe
}

func TestS03RecoveryDoesNotLatchPendingReplacement(t *testing.T) {
	frame := 0
	check, state := newS03RecoveryCheck(pendingReplacementReplay(&frame), recoveryDeleted, recoveryOldUID)
	done, err := check(context.Background())
	if err != nil || done {
		t.Fatalf("Pending replay done=%t err=%v", done, err)
	}
	if state.newMaster != "" || !state.electedAt.IsZero() {
		t.Fatalf("Pending replacement latched an unqualified recovered master: %+v", state)
	}
}

func TestS03RecoveryDoesNotCallFirstRealElectionASecondChange(t *testing.T) {
	frame := 0
	check, state := newS03RecoveryCheck(pendingReplacementReplay(&frame), recoveryDeleted, recoveryOldUID)
	_, _ = check(context.Background())
	frame = 1
	_, err := check(context.Background())
	if err != nil {
		t.Fatalf("first eligible hab-1 election was called a second change: %v", err)
	}
	if state.newMaster != recoveryOther {
		t.Fatalf("eligible recovered master = %q", state.newMaster)
	}
	frame = 2
	done, err := check(context.Background())
	if !done || err != nil {
		t.Fatalf("synthetic advancing status control did not finish: done=%t err=%v", done, err)
	}
}

func TestS03RecoverySameNameRequiresStableReplacement(t *testing.T) {
	tests := map[string]func(*s03ReplacementObservation){
		"original Pod UID":           func(o *s03ReplacementObservation) { o.UID = recoveryOldUID },
		"missing UID":                func(o *s03ReplacementObservation) { o.UID = "" },
		"deleting replacement":       func(o *s03ReplacementObservation) { o.Deleting = true },
		"Pod not ready":              func(o *s03ReplacementObservation) { o.PodReady = false },
		"container not ready":        func(o *s03ReplacementObservation) { o.ContainerReady = false },
		"missing container identity": func(o *s03ReplacementObservation) { o.ContainerID = "" },
		"container not running":      func(o *s03ReplacementObservation) { o.RunningStarted = time.Time{} },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			probe := recoveryProbe()
			observation := readyS03Replacement()
			change(&observation)
			probe.replacement = func(context.Context) (s03ReplacementObservation, error) { return observation, nil }
			probe.activeMasters = func(context.Context) []string { return []string{recoveryDeleted} }
			check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			done, err := check(context.Background())
			if done || err != nil || state.newMaster != "" || !state.electedAt.IsZero() {
				t.Fatalf("unproven same-name candidate accepted: done=%t err=%v state=%+v", done, err, state)
			}
		})
	}
	for name, change := range tests {
		t.Run("after sweep "+name, func(t *testing.T) {
			probe := recoveryProbe()
			reads := 0
			probe.replacement = func(context.Context) (s03ReplacementObservation, error) {
				reads++
				observation := readyS03Replacement()
				if reads == 2 {
					change(&observation)
				}
				return observation, nil
			}
			probe.activeMasters = func(context.Context) []string { return []string{recoveryDeleted} }
			check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			_, err := check(context.Background())
			if err != nil || state.newMaster != "" {
				t.Fatalf("unstable replacement accepted: err=%v state=%+v", err, state)
			}
		})
	}
	for name, change := range map[string]func(*s03ReplacementObservation){
		"another replacement UID": func(o *s03ReplacementObservation) { o.UID = "another-uid" },
		"another container ID":    func(o *s03ReplacementObservation) { o.ContainerID = "containerd://another" },
		"container restart":       func(o *s03ReplacementObservation) { o.Restarts++ },
		"different running start": func(o *s03ReplacementObservation) {
			o.RunningStarted = o.RunningStarted.Add(time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			probe := recoveryProbe()
			reads := 0
			probe.replacement = func(context.Context) (s03ReplacementObservation, error) {
				reads++
				observation := readyS03Replacement()
				if reads == 2 {
					change(&observation)
				}
				return observation, nil
			}
			probe.activeMasters = func(context.Context) []string { return []string{recoveryDeleted} }
			check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			_, err := check(context.Background())
			if err != nil || state.newMaster != "" {
				t.Fatalf("changed identity accepted: err=%v state=%+v", err, state)
			}
		})
	}
}

func TestS03RecoverySameNameReadinessMustPrecedeSweep(t *testing.T) {
	probe := recoveryProbe()
	swept := false
	reads := 0
	probe.replacement = func(context.Context) (s03ReplacementObservation, error) {
		reads++
		if !swept {
			return s03ReplacementObservation{UID: recoveryNewUID}, nil
		}
		return readyS03Replacement(), nil
	}
	probe.activeMasters = func(context.Context) []string { swept = true; return []string{recoveryDeleted} }
	probe.master = func(context.Context) (string, []string, error) {
		return recoveryDeleted, []string{recoveryOther, recoveryThird}, nil
	}
	probe.status = func(context.Context) (statusSnapshot, error) {
		return statusSnapshot{primary: recoveryDeleted,
			lastObserved: time.Unix(100, 0), conditions: map[string]string{s03PrimaryResolved: s03ConditionTrue}}, nil
	}
	check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
	_, err := check(context.Background())
	if err != nil || state.newMaster != "" || reads != 1 {
		t.Fatalf("readiness after sweep qualified earlier role: reads=%d state=%+v err=%v", reads, state, err)
	}
	_, err = check(context.Background())
	if err != nil || state.newMaster != recoveryDeleted || reads != 3 {
		t.Fatalf("stable ready replacement was not accepted: reads=%d state=%+v err=%v", reads, state, err)
	}
	if state.observations.Accepted == nil || state.observations.Accepted.After == nil {
		t.Fatal("same-name acceptance lost its before/after identities")
	}
}

func TestS03RecoveryMetadataErrorsNeverHideActiveClaims(t *testing.T) {
	for _, metadataUnavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(metadataUnavailable), func(t *testing.T) {
			probe := recoveryProbe()
			probe.replacement = func(context.Context) (s03ReplacementObservation, error) {
				if metadataUnavailable {
					return s03ReplacementObservation{}, errors.New("metadata unavailable")
				}
				return s03ReplacementObservation{UID: recoveryNewUID}, nil
			}
			check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			for range 2 {
				_, err := check(context.Background())
				if err != nil {
					t.Fatal(err)
				}
			}
			if state.newMaster != recoveryOther || state.noticedAt.IsZero() {
				t.Fatal("another member waited for replacement readiness")
			}
			sweeps := 0
			probe.activeMasters = func(context.Context) []string { sweeps++; return []string{recoveryDeleted, recoveryOther} }
			check, state = newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			done, err := check(context.Background())
			if done || err == nil || sweeps != 2 || state.newMaster != "" {
				t.Fatalf("global claims were hidden: done=%t err=%v sweeps=%d", done, err, sweeps)
			}
		})
	}
}

func TestS03RecoveryRejectsRealSecondChangeWhileStatusWaits(t *testing.T) {
	probe := recoveryProbe()
	statusReads := 0
	probe.status = func(context.Context) (statusSnapshot, error) {
		statusReads++
		return statusSnapshot{
			primary: recoveryOther, lastObserved: time.Unix(100, 0),
			conditions: map[string]string{s03PrimaryResolved: s03ConditionTrue}}, nil
	}
	probeMaster := recoveryOther
	probe.master = func(context.Context) (string, []string, error) { return probeMaster, nil, nil }
	check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
	_, _ = check(context.Background())
	readsBefore := statusReads
	probeMaster = recoveryThird
	done, err := check(context.Background())
	if done || err == nil || !strings.Contains(err.Error(), "the master changed again") ||
		state.newMaster != recoveryOther || !state.noticedAt.IsZero() || statusReads != readsBefore {
		t.Fatalf("pending status hid a real second change: done=%t err=%v state=%+v statusReads=%d",
			done, err, state, statusReads)
	}
}

func TestS03RecoveryStatusMustAdvanceFromFixedCompleteBaseline(t *testing.T) {
	probe := recoveryProbe()
	sample := statusSnapshot{primary: recoveryOther, lastObserved: time.Unix(100, 0),
		conditions: map[string]string{s03PrimaryResolved: s03ConditionTrue}}
	var readErr error
	probe.status = func(context.Context) (statusSnapshot, error) { return sample, readErr }
	receipt := time.Unix(1000, 0)
	probe.now = func() time.Time { return receipt }
	check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
	checkWaiting := func() {
		t.Helper()
		if done, err := check(context.Background()); done || err != nil || !state.noticedAt.IsZero() {
			t.Fatalf("unproven status was noticed: done=%t err=%v state=%+v", done, err, state)
		}
	}
	sample.lastObserved = time.Time{}
	checkWaiting()
	if state.observations.Baseline != nil {
		t.Fatal("zero exported timestamp established baseline")
	}
	sample.lastObserved = time.Unix(100, 0)
	sample.conditions = nil
	checkWaiting()
	if state.observations.Baseline != nil {
		t.Fatal("missing condition established complete baseline")
	}
	sample.conditions = map[string]string{s03PrimaryResolved: s03ConditionTrue}
	checkWaiting()
	baseline := state.observations.Baseline
	if baseline == nil || !baseline.LastObserved.Equal(time.Unix(100, 0)) {
		t.Fatal("baseline not captured")
	}
	for _, tc := range []struct {
		stamp             int64
		primary, resolved string
		unreadable        bool
	}{
		{100, recoveryOther, s03ConditionTrue, false}, // Equal, including another GET in the same second.
		{99, recoveryOther, s03ConditionTrue, false},
		{101, recoveryDeleted, s03ConditionTrue, false},
		{102, recoveryOther, s03ConditionFalse, false},
		{103, recoveryOther, "", false},
		{104, recoveryOther, s03ConditionTrue, true},
		{0, recoveryOther, s03ConditionTrue, false},
	} {
		sample.lastObserved = time.Unix(tc.stamp, 0)
		if tc.stamp == 0 {
			sample.lastObserved = time.Time{}
		}
		sample.primary = tc.primary
		sample.conditions = map[string]string{s03PrimaryResolved: tc.resolved}
		readErr = nil
		if tc.unreadable {
			readErr = errors.New("status read unavailable")
		}
		receipt = receipt.Add(time.Second)
		checkWaiting()
		if state.observations.Baseline != baseline || !baseline.LastObserved.Equal(time.Unix(100, 0)) {
			t.Fatal("baseline reset on a later poll")
		}
	}
	// A later matching export establishes external agreement, not exact
	// controller probe or container-consumption timing.
	readErr = nil
	sample = statusSnapshot{primary: recoveryOther, lastObserved: time.Unix(105, 0),
		conditions: map[string]string{s03PrimaryResolved: s03ConditionTrue}}
	receipt = receipt.Add(time.Second)
	done, err := check(context.Background())
	if !done || err != nil || !state.noticedAt.Equal(receipt) || state.noticedAt.Equal(sample.lastObserved) {
		t.Fatalf("external agreement not recorded at receipt: done=%t err=%v state=%+v", done, err, state)
	}
}

func TestS03RecoveryUsesOriginalUIDAndRechecksSameNameMetadata(t *testing.T) {
	for _, unavailableRead := range []int{1, 2} {
		t.Run(fmt.Sprint(unavailableRead), func(t *testing.T) {
			probe := recoveryProbe()
			reads := 0
			probe.replacement = func(context.Context) (s03ReplacementObservation, error) {
				reads++
				if reads == unavailableRead {
					return s03ReplacementObservation{}, errors.New("metadata unavailable")
				}
				return readyS03Replacement(), nil
			}
			probe.activeMasters = func(context.Context) []string { return []string{recoveryDeleted} }
			check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			_, err := check(context.Background())
			if err != nil || state.newMaster != "" {
				t.Fatalf("unavailable identity was accepted: err=%v state=%+v", err, state)
			}
		})
	}
	// The original UID is an immutable constructor argument, not the most
	// recently fetched Pod identity. A stable new replacement must qualify.
	probe := recoveryProbe()
	probe.activeMasters = func(context.Context) []string { return []string{recoveryDeleted} }
	probe.master = func(context.Context) (string, []string, error) { return recoveryDeleted, nil, nil }
	check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
	_, err := check(context.Background())
	if err != nil || state.newMaster != recoveryDeleted {
		t.Fatalf("original UID was reconstructed from replacement: err=%v state=%+v", err, state)
	}
}

func TestS03RecoveryStopsAfterCallerContextEnds(t *testing.T) {
	const statusStage = "status"
	for _, stage := range []string{"replacement", "active roles", "full roles", statusStage, "formation", "client"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probe := recoveryProbe()
			observe := func(name string, got context.Context) {
				t.Helper()
				if got != ctx {
					t.Fatal("observation used another context")
				}
				if got.Err() != nil {
					t.Fatalf("started %s after cancellation", name)
				}
				if name == stage {
					cancel()
				}
			}
			probe.replacement = func(got context.Context) (s03ReplacementObservation, error) {
				observe("replacement", got)
				return readyS03Replacement(), nil
			}
			probe.activeMasters = func(got context.Context) []string {
				observe("active roles", got)
				return []string{recoveryOther}
			}
			probe.master = func(got context.Context) (string, []string, error) {
				observe("full roles", got)
				return recoveryOther, nil, nil
			}
			reads := 0
			probe.status = func(got context.Context) (statusSnapshot, error) {
				observe(statusStage, got)
				reads++
				return statusSnapshot{primary: recoveryOther, lastObserved: time.Unix(int64(100+reads), 0),
					conditions: map[string]string{s03PrimaryResolved: s03ConditionTrue}}, nil
			}
			probe.formationTime = func(got context.Context) (time.Duration, error) {
				observe("formation", got)
				return time.Second, nil
			}
			probe.lastAnswerAcknowledged = func(got context.Context) bool { observe("client", got); return true }
			check, _ := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			done, err := check(ctx)
			if ctx.Err() == nil {
				done, err = check(ctx)
			} // Formation and client follow the fixed baseline.
			if done || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s: done=%t err=%v", stage, done, err)
			}
		})
	}
}

const replacementPodJSON = `{"metadata":{"uid":"844e9e65-8e3c-42ad-b8cb-5bfa955d45eb"},
 "spec":{"containers":[{"name":"cubrid","env":[{"name":"TOKEN","value":"private-spec-value"}]}]},
 "status":{"conditions":[{"type":"Ready","status":"True"}],
 "initContainerStatuses":[{"name":"cubrid","containerID":"wrong-init-container","ready":true}],
 "containerStatuses":[{"name":"sidecar","containerID":"wrong-sidecar","ready":true},
 {"name":"cubrid","containerID":"containerd://replacement","restartCount":0,"ready":true,
 "state":{"running":{"startedAt":"2026-11-15T07:40:14Z"}}}]}}`

func TestS03RecoveryReplacementReaderFiltersNamedContainer(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > '" + calls + "'\ncat <<'POD'\n" + replacementPodJSON + "\nPOD\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := newHARun("recovery-ns", "client-ns", "hab", "db", []string{recoveryDeleted, recoveryOther, recoveryThird})
	got, err := run.readS03Replacement(context.Background(), recoveryDeleted)
	if err != nil || got != readyS03Replacement() {
		t.Fatalf("replacement = %+v, err=%v", got, err)
	}
	command, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(command), "get pod hab-0 -o json") || strings.Contains(string(command), "exec") {
		t.Fatalf("unexpected metadata command %q", command)
	}
	filtered, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"private-spec-value", "wrong-sidecar", "wrong-init-container", "TOKEN", "spec"} {
		if strings.Contains(string(filtered), raw) {
			t.Fatalf("metadata filter retained %q", raw)
		}
	}
}

func TestS03RecoveryReplacementReaderRejectsIncompleteContainer(t *testing.T) {
	for _, text := range []string{
		"not JSON",
		`{ "metadata":{"uid":"new"},"status":{"containerStatuses":[]}}`,
		strings.Replace(replacementPodJSON, `"restartCount":0,`, "", 1),
		strings.Replace(replacementPodJSON, `"name":"sidecar"`, `"name":"cubrid"`, 1),
	} {
		if _, err := parseS03Replacement(text); err == nil {
			t.Fatalf("incomplete or ambiguous cubrid status accepted: %s", text)
		}
	}
	for _, text := range []string{
		strings.Replace(replacementPodJSON, `"ready":true,`+"\n "+`"state"`, `"ready":false,`+"\n "+`"state"`, 1),
		strings.Replace(replacementPodJSON, `"running":{"startedAt":"2026-11-15T07:40:14Z"}`, `"waiting":{}`, 1),
		strings.Replace(replacementPodJSON, `"startedAt":"2026-11-15T07:40:14Z"`, `"startedAt":null`, 1),
		strings.Replace(replacementPodJSON, `"uid":"844e9e65-8e3c-42ad-b8cb-5bfa955d45eb"`,
			`"uid":"844e9e65-8e3c-42ad-b8cb-5bfa955d45eb","deletionTimestamp":"2026-11-15T07:40:15Z"`, 1),
	} {
		observation, err := parseS03Replacement(text)
		if err == nil && observation.eligible(recoveryOldUID) {
			t.Fatalf("ineligible container metadata qualified: %s", text)
		}
	}
}

func TestS03RecoveryReplacementReaderHonorsContext(t *testing.T) {
	hangingKubectl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	run := newHARun("recovery-ns", "client-ns", "hab", "db", []string{recoveryDeleted})
	_, err := run.readS03Replacement(ctx, recoveryDeleted)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("metadata reader escaped caller context: %v", err)
	}
}

// A synthetic no-op fault exercises the existing bounded runner without a
// cluster, engine command or additional recovery deadline in production.
type s03ObservationFault struct{}

func (s03ObservationFault) Name() string                          { return "scripted recovery observation" }
func (s03ObservationFault) Inject(context.Context) error          { return nil }
func (s03ObservationFault) Confirm(context.Context) (bool, error) { return true, nil }
func (s03ObservationFault) Remove(context.Context) error          { return nil }

func TestS03RecoveryNeverAdvancingStatusUsesExistingDeadline(t *testing.T) {
	probe := recoveryProbe()
	probe.status = func(context.Context) (statusSnapshot, error) {
		return statusSnapshot{primary: recoveryOther,
			lastObserved: time.Unix(100, 0), conditions: map[string]string{s03PrimaryResolved: s03ConditionTrue}}, nil
	}
	check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
	outcome := faults.Run(context.Background(), faults.Scenario{
		Fault: s03ObservationFault{}, Check: check, Before: func(context.Context) error { return nil },
		Limits: faults.Limits{Confirm: time.Second, Outcome: 30 * time.Millisecond,
			Cleanup: time.Second, Poll: time.Millisecond, Stop: time.Second}})
	if outcome.Result != evidence.Fail || !strings.Contains(outcome.Reason, "time limit exceeded") ||
		!state.noticedAt.IsZero() || state.observations.Baseline == nil {
		t.Fatalf("nonadvancing status escaped deadline: %+v state=%+v", outcome, state)
	}
	if s03FlowLimits.Confirm != 5*time.Minute || s03FlowLimits.Outcome != 10*time.Minute ||
		s03FlowLimits.Cleanup != time.Minute || s03FlowLimits.Poll != 2*time.Second ||
		s03FailoverLimit(s03Graceful).Value != 30*time.Second || s03FailoverLimit(s03Abrupt).Value != 6*time.Minute ||
		stablePeriod.Value != 30*time.Second || rejoinLimit.Value != 5*time.Minute {
		t.Fatal("existing production limits changed")
	}
}

func TestS03RecoveryUnresolvedCompleteStatusIsTheFixedBaseline(t *testing.T) {
	for _, resolved := range []string{s03ConditionFalse, s03ConditionUnknown} {
		t.Run(resolved, func(t *testing.T) {
			probe := recoveryProbe()
			status := statusSnapshot{lastObserved: time.Unix(100, 0),
				conditions: map[string]string{s03PrimaryResolved: resolved}}
			probe.status = func(context.Context) (statusSnapshot, error) { return status, nil }
			check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
			if done, err := check(context.Background()); done || err != nil ||
				state.observations.Baseline == nil || !state.noticedAt.IsZero() {
				t.Fatalf("unresolved complete snapshot did not establish baseline: done=%t err=%v state=%+v", done, err, state)
			}
			baseline := state.observations.Baseline
			status.primary = recoveryOther
			status.lastObserved = time.Unix(101, 0)
			status.conditions = map[string]string{s03PrimaryResolved: s03ConditionTrue}
			if done, err := check(context.Background()); !done || err != nil || state.observations.Baseline != baseline {
				t.Fatalf("first resolved advancing status needed another publication: done=%t err=%v state=%+v", done, err, state)
			}
		})
	}
}

func TestS03RecoveryDiagnosticsAreBoundedRedactedAndOptional(t *testing.T) {
	const secret = "private-recovery-token"
	oldDir, oldRedactor := acquiredEvidenceDir, redactor
	acquiredEvidenceDir = t.TempDir()
	redactor = evidence.NewRedactor(secret)
	t.Cleanup(func() { acquiredEvidenceDir, redactor = oldDir, oldRedactor })
	state := &s03RecoveryState{newMaster: recoveryOther, electedAt: time.Unix(100, 0)}
	state.observations.Accepted = &s03RecoveryFrame{Member: recoveryOther, Before: readyS03Replacement(),
		Reason: "candidate_accepted"}
	state.observations.Accepted.Before.ContainerID = "containerd://" + secret
	outcome := faults.Outcome{Result: evidence.Fail, Reason: "original safety failure"}
	before := outcome
	snapshot, file, note := finishS03Recovery(outcome, state, s03AbruptFirst, redactor, writeScenarioFile)
	if file == "" || note != "" || snapshot.newMaster != recoveryOther {
		t.Fatalf("diagnostic result file=%q note=%q snapshot=%+v", file, note, snapshot)
	}
	data, err := os.ReadFile(filepath.Join(acquiredEvidenceDir, file))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || int64(len(data)) > evidence.DefaultLogLimits().SourceBytes ||
		strings.Contains(string(data), secret) ||
		!strings.Contains(string(data), "[REDACTED]") {
		t.Fatalf("diagnostic was not valid bounded redacted JSON: %s", data)
	}
	_, file, note = finishS03Recovery(outcome, state, s03AbruptFirst, redactor, func(string, []byte) string { return "" })
	if file != "" || note == "" || !reflect.DeepEqual(outcome, before) {
		t.Fatalf("failed diagnostic write changed outcome: file=%q note=%q outcome=%+v", file, note, outcome)
	}
	if len(s03FailedOutcomeFiles(s03AbruptFirst, "", errors.New("history unavailable"), workload.Report{},
		errors.New("data not read"), func(string, []byte) string { return "" })) != 0 {
		t.Fatal("failure evidence counted an unstored file")
	}
}

func TestS03RecoveryDiagnosticsUseValidUnavailableRecords(t *testing.T) {
	for _, kind := range []string{"too large after redaction", "serialization failure"} {
		t.Run(kind, func(t *testing.T) {
			state := &s03RecoveryState{}
			state.observations.Accepted = &s03RecoveryFrame{Member: recoveryOther, Before: readyS03Replacement()}
			redact := evidence.NewRedactor("secret")
			want := "source_size_limit"
			if kind == "serialization failure" {
				state.observations.Accepted.StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				want = "serialization_failed"
			} else {
				// Redaction expands each short credential, so the limit must be checked
				// on the redacted bytes, not the smaller unredacted serialization.
				state.observations.Accepted.Before.UID = strings.Repeat("secret", 110000)
			}
			var data []byte
			_, file, note := finishS03Recovery(faults.Outcome{Result: evidence.Blocked}, state, s03AbruptFirst, redact,
				func(path string, bytes []byte) string { data = bytes; return path })
			if file == "" || note == "" || !json.Valid(data) || int64(len(data)) > evidence.DefaultLogLimits().SourceBytes ||
				!strings.Contains(string(data), want) {
				t.Fatalf("bad unavailable diagnostic: file=%q note=%q data=%s", file, note, data)
			}
		})
	}
}

func TestS03RecoveryUnfinishedCallbackStateIsNeverRead(t *testing.T) {
	outcome := faults.Outcome{Result: evidence.Blocked, Reason: faults.ReasonStepStillRunning,
		StillRunning: []string{"check"}}
	writes := 0
	// A nil pointer proves the unfinished branch returns before any state
	// dereference or copy, including the values routed to condition reporting.
	snapshot, file, note := finishS03Recovery(outcome, nil, s03AbruptFirst, evidence.NewRedactor(),
		func(string, []byte) string { writes++; return "unexpected" })
	if snapshot.newMaster != "" || !snapshot.electedAt.IsZero() || !snapshot.noticedAt.IsZero() || file != "" ||
		note == "" || writes != 0 {
		t.Fatalf("unfinished state was consumed: snapshot=%+v file=%q note=%q writes=%d", snapshot, file, note, writes)
	}
	if s03ObservedWithin(snapshot.electedAt, time.Unix(100, 0)) != evidence.Unknown ||
		s03ObservedWithin(time.Unix(101, 0), time.Time{}) != evidence.Unknown {
		t.Fatal("unavailable time became an elapsed measurement")
	}
	if got := s03ObservedWithin(time.Unix(102, 0), time.Unix(100, 0)); got != "2s" {
		t.Fatalf("observed duration=%q", got)
	}
	if s03DataCheckBlocked(outcome) == "" {
		t.Fatal("unfinished callback permitted failure-path SQL data checks")
	}
}

func TestS03RecoveryProductionReadsOnlyFinishedLocalState(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("recovery test source path unavailable")
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(source), "s03_test.go"), nil,
		parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	factoryCalls, finishCalls := 0, 0
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if id, ok := selector.X.(*ast.Ident); ok && id.Name == "recoveryState" {
				t.Fatal("production reads mutable factory state outside the completion guard")
			}
		}
		if unary, ok := node.(*ast.StarExpr); ok {
			if id, ok := unary.X.(*ast.Ident); ok && id.Name == "recoveryState" {
				t.Fatal("production copies mutable factory state outside the completion guard")
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		if name.Name == "newS03RecoveryCheck" {
			factoryCalls++
			original, ok := call.Args[len(call.Args)-1].(*ast.Ident)
			if !ok || original.Name != "uid" {
				t.Fatal("factory lost the original pre-deletion UID")
			}
		}
		if name.Name == "finishS03Recovery" {
			finishCalls++
		}
		return true
	})
	if factoryCalls != 1 || finishCalls != 1 {
		t.Fatalf("factory/completion wiring=%d/%d", factoryCalls, finishCalls)
	}
}

func TestS03RecoveryDiagnosticsKeepFixedObservationSlots(t *testing.T) {
	probe := recoveryProbe()
	frame := 0
	probe.replacement = func(context.Context) (s03ReplacementObservation, error) {
		return s03ReplacementObservation{UID: recoveryNewUID}, nil
	}
	probe.activeMasters = func(context.Context) []string {
		if frame == 0 {
			return []string{recoveryDeleted}
		}
		return []string{recoveryOther}
	}
	probe.status = func(context.Context) (statusSnapshot, error) {
		return statusSnapshot{primary: recoveryOther,
			lastObserved: time.Unix(100, 0), conditions: map[string]string{s03PrimaryResolved: s03ConditionTrue}}, nil
	}
	check, state := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
	_, _ = check(context.Background())
	frame = 1
	_, _ = check(context.Background())
	rejected, accepted, baseline := state.observations.Rejected, state.observations.Accepted, state.observations.Baseline
	before, err := json.Marshal(state.observations)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if done, err := check(context.Background()); done || err != nil {
			t.Fatalf("pending status done=%t err=%v", done, err)
		}
	}
	after, err := json.Marshal(state.observations)
	if err != nil {
		t.Fatal(err)
	}
	if rejected == nil || accepted == nil || baseline == nil || state.observations.Rejected != rejected ||
		state.observations.Accepted != accepted || state.observations.Baseline != baseline ||
		state.observations.Agreement != nil ||
		len(after)-len(before) > 32 {
		t.Fatal("repeated polls grew or overwrote fixed diagnostic slots")
	}
	claims := knownS03Claims([]string{recoveryDeleted, recoveryOther, "not-a-member", recoveryOther}, probe.members)
	if !reflect.DeepEqual(claims, []string{recoveryDeleted, recoveryOther}) {
		t.Fatalf("diagnostic claims escaped member bounds: %q", claims)
	}
}
