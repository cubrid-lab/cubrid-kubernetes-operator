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
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	recoveryDeleted = "hab-0"
	recoveryOther   = "hab-1"
	recoveryThird   = "hab-2"
	recoveryOldUID  = "599d0cd3-6386-4dcf-81da-9a0ce8fc83e2"
	recoveryNewUID  = "844e9e65-8e3c-42ad-b8cb-5bfa955d45eb"
)

func recoveryProbe() s03RecoveryProbe {
	return s03RecoveryProbe{
		activeMasters: func(context.Context) []string { return []string{recoveryOther} },
		master: func(context.Context) (string, []string, error) {
			return recoveryOther, []string{recoveryDeleted, recoveryThird}, nil
		},
		status:                 func(context.Context) (statusSnapshot, error) { return statusSnapshot{primary: recoveryOther}, nil },
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
		"eventual recovery":    {pass: true},
		"no active master yet": {change: func(p *s03RecoveryProbe) { p.activeMasters = func(context.Context) []string { return nil } }},
		"full roles unavailable": {change: func(p *s03RecoveryProbe) {
			p.master = func(context.Context) (string, []string, error) { return "", nil, errors.New("unreadable roles") }
		}},
		"formation unavailable": {change: func(p *s03RecoveryProbe) {
			p.formationTime = func(context.Context) (time.Duration, error) { return 0, errors.New("formation unavailable") }
		}},
		"client has no acknowledgement": {change: func(p *s03RecoveryProbe) { p.lastAnswerAcknowledged = func(context.Context) bool { return false } }},
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
			if done != tc.pass || tc.errorContains == "" && err != nil || tc.errorContains != "" && (err == nil || !strings.Contains(err.Error(), tc.errorContains)) {
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
		t.Run(map[bool]string{false: "mid-sweep handover retries", true: "confirmed two masters fails"}[confirmed], func(t *testing.T) {
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
	cancel()
	probe := recoveryProbe()
	probe.activeMasters = func(got context.Context) []string {
		if got != ctx || got.Err() == nil {
			t.Fatal("caller cancellation was lost")
		}
		return nil
	}
	check, _ := newS03RecoveryCheck(probe, recoveryDeleted, recoveryOldUID)
	if done, err := check(ctx); done || err != nil {
		t.Fatalf("cancelled observation done=%t err=%v", done, err)
	}
}
