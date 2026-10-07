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

package faults

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSystem is a shared system whose starting state holds from a given
// check on, and which records what the gate asked of it.
type fakeSystem struct {
	holdsFrom  int // Verify succeeds from this call on; 0 never
	restoreErr int // Restore fails on calls up to this one
	hang       bool
	flaky      map[int]bool // calls of Verify that fail even after holdsFrom

	restores, verifies int
	order              []string
}

func (s *fakeSystem) gate() *Gate {
	return &Gate{Limit: 150 * time.Millisecond, Poll: 5 * time.Millisecond, Restore: s.restore, Verify: s.verify}
}

func (s *fakeSystem) restore(context.Context) error {
	s.restores++
	s.order = append(s.order, "restore")
	if s.restores <= s.restoreErr {
		return errors.New("the Operator could not be scaled")
	}
	return nil
}

func (s *fakeSystem) verify(context.Context) error {
	s.verifies++
	s.order = append(s.order, "verify")
	if s.hang {
		select {}
	}
	if s.holdsFrom > 0 && s.verifies >= s.holdsFrom && !s.flaky[s.verifies] {
		return nil
	}
	return errors.New("a read-write Broker is not available")
}

func TestGate_RestoresTheStartingStateThenLetsTheVariantRun(t *testing.T) {
	s := &fakeSystem{holdsFrom: 3}
	e := s.gate().Enter(context.Background(), "S05-all-rw-brokers", nil)
	if e.Reason != "" || !e.Verified {
		t.Fatalf("want the variant to run from a verified starting state, got %+v", e)
	}
	if e.Variant != "S05-all-rw-brokers" || e.T.IsZero() || e.Waited == "" {
		t.Errorf("the entry does not say which variant, when and how long: %+v", e)
	}
	// What an earlier variant left is put back before every check.
	if got := strings.Join(s.order, ","); got != "restore,verify,restore,verify,restore,verify" {
		t.Errorf("calls = %s", got)
	}
}

func TestGate_RetriesAFailedRestore(t *testing.T) {
	s := &fakeSystem{holdsFrom: 1, restoreErr: 2}
	e := s.gate().Enter(context.Background(), "S06-restart", nil)
	if e.Reason != "" || !e.Verified {
		t.Fatalf("want the variant to run once the restore worked, got %+v", e)
	}
	if s.restores != 3 {
		t.Errorf("restores %d, want 3: a starting state that holds is not verified while the restore fails", s.restores)
	}
}

func TestGate_AFailedRestoreStillSaysWhatDoesNotHold(t *testing.T) {
	s := &fakeSystem{restoreErr: 1000}
	e := s.gate().Enter(context.Background(), "S06-restart", nil)
	for _, want := range []string{"the Operator could not be scaled", "a read-write Broker is not available"} {
		if !strings.Contains(e.Reason, want) {
			t.Errorf("the reason does not say %q: %s", want, e.Reason)
		}
	}
}

func TestGate_TheStartingStateMustHoldForTheSettlePeriod(t *testing.T) {
	// Holds on the first check, not on the second one a settle period
	// later, then holds twice in a row.
	s := &fakeSystem{holdsFrom: 1, flaky: map[int]bool{2: true}}
	g := s.gate()
	g.Settle = 10 * time.Millisecond
	start := time.Now()
	e := g.Enter(context.Background(), "S05-broker-process", nil)
	if e.Reason != "" || !e.Verified {
		t.Fatalf("want the variant to run, got %+v", e)
	}
	if s.verifies != 4 {
		t.Errorf("verifies %d, want 4: two checks a settle period apart, twice", s.verifies)
	}
	if time.Since(start) < 2*g.Settle {
		t.Errorf("the gate did not wait for the settle period")
	}
}

func TestGate_TheEntryKeepsTheStateItVerified(t *testing.T) {
	s := &fakeSystem{holdsFrom: 1}
	g := s.gate()
	g.Describe = func(context.Context) string { return "hab-0=uid-a/0 hab-1=uid-b/0" }
	if e := g.Enter(context.Background(), "S14", nil); e.State != "hab-0=uid-a/0 hab-1=uid-b/0" {
		t.Errorf("State = %q", e.State)
	}
}

func TestGate_ARestoreBudgetBoundsTheWholeRun(t *testing.T) {
	s := &fakeSystem{holdsFrom: 1}
	g := s.gate()
	g.Settle = 60 * time.Millisecond
	g.Budget = 100 * time.Millisecond
	if e := g.Enter(context.Background(), "S02", nil); !e.Verified {
		t.Fatalf("the first variant fits the budget, got %+v", e)
	}
	// About 40ms are left, less than a settle period.
	e := g.Enter(context.Background(), "S14", nil)
	if e.Verified || !strings.HasPrefix(e.Reason, ReasonStartingStateNotRestored) ||
		!strings.Contains(e.Reason, "restore budget exhausted") {
		t.Fatalf("want the budget exhausted, got %+v", e)
	}
	start := time.Now()
	if next := g.Enter(context.Background(), "S05-broker-process", nil); next.Reason != e.Reason ||
		time.Since(start) > 20*time.Millisecond {
		t.Errorf("a later variant is blocked at once with the same reason, got %+v", next)
	}
}

func TestGate_AStartingStateThatCannotBeRestoredBlocksEveryLaterVariant(t *testing.T) {
	s := &fakeSystem{}
	g := s.gate()
	first := g.Enter(context.Background(), "S05-all-rw-brokers", nil)
	if first.Verified || !strings.HasPrefix(first.Reason, ReasonStartingStateNotRestored) {
		t.Fatalf("want %s, got %+v", ReasonStartingStateNotRestored, first)
	}
	for _, want := range []string{"S05-all-rw-brokers", "a read-write Broker is not available"} {
		if !strings.Contains(first.Reason, want) {
			t.Errorf("the reason does not name %q: %s", want, first.Reason)
		}
	}

	// The next variant is blocked at once, with the same reason: the gate
	// does not wait again on a system it could not restore.
	verifies := s.verifies
	start := time.Now()
	next := g.Enter(context.Background(), "S06-restart", nil)
	if next.Verified || next.Reason != first.Reason {
		t.Errorf("want the later variant blocked with %q, got %+v", first.Reason, next)
	}
	if s.verifies != verifies || time.Since(start) > 50*time.Millisecond {
		t.Errorf("the later variant waited for the starting state again")
	}
	if g.Blocked() != first.Reason {
		t.Errorf("Blocked() = %q", g.Blocked())
	}
}

func TestGate_ABrokenOrderDependencyBlocksOnlyItsVariant(t *testing.T) {
	s := &fakeSystem{holdsFrom: 1}
	g := s.gate()
	needsFirst := func(context.Context) error { return errors.New("the master is hab-1, not hab-0") }
	e := g.Enter(context.Background(), "S03-abrupt-first-member", needsFirst)
	if !e.Verified || !strings.HasPrefix(e.Reason, ReasonOrderDependency) ||
		!strings.Contains(e.Reason, "the master is hab-1, not hab-0") {
		t.Fatalf("want %s from a verified starting state, got %+v", ReasonOrderDependency, e)
	}
	if next := g.Enter(context.Background(), "S06-absent-during-failover", nil); next.Reason != "" {
		t.Errorf("the next variant does not depend on that order and must run, got %+v", next)
	}
}

func TestGate_AFailedSetupBlocksEveryVariant(t *testing.T) {
	s := &fakeSystem{holdsFrom: 1}
	g := s.gate()
	g.Block("the HA cluster was not formed: seeds the other members failed")
	g.Block("a later reason")
	e := g.Enter(context.Background(), "S02", nil)
	if e.Verified || e.Reason != "the HA cluster was not formed: seeds the other members failed" {
		t.Errorf("want the first reason, got %+v", e)
	}
	if s.verifies != 0 || s.restores != 0 {
		t.Errorf("the gate touched a system that was never formed")
	}
}

func TestGate_NeverHangs(t *testing.T) {
	s := &fakeSystem{hang: true}
	start := time.Now()
	e := s.gate().Enter(context.Background(), "S14", nil)
	if time.Since(start) > time.Second {
		t.Fatalf("Enter did not return within its limit")
	}
	if e.Verified || !strings.HasPrefix(e.Reason, ReasonStartingStateNotRestored) {
		t.Errorf("want %s, got %+v", ReasonStartingStateNotRestored, e)
	}
}

func TestGate_UnsetLimitIsBlocked(t *testing.T) {
	s := &fakeSystem{holdsFrom: 1}
	g := s.gate()
	g.Limit = 0
	e := g.Enter(context.Background(), "S14", nil)
	if e.Verified || e.Reason != ReasonTimeLimitUnset {
		t.Errorf("want %s, got %+v", ReasonTimeLimitUnset, e)
	}
}
