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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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

// A verification that ignores its cancelled context is still running against
// the system: the gate returns, and no later variant starts next to it.
func TestGate_NeverHangs(t *testing.T) {
	s := &fakeSystem{hang: true}
	g := s.gate()
	g.Stop = 50 * time.Millisecond
	start := time.Now()
	e := g.Enter(context.Background(), "S14", nil)
	if time.Since(start) > time.Second {
		t.Fatalf("Enter did not return within its limit")
	}
	if e.Verified || !strings.HasPrefix(e.Reason, ReasonStepStillRunning) {
		t.Errorf("want %s, got %+v", ReasonStepStillRunning, e)
	}
	if later := g.Enter(context.Background(), "S15", nil); later.Verified || later.Reason != e.Reason {
		t.Errorf("a later variant was not blocked for the same reason: %+v", later)
	}
}

// markerMissing is the failing condition of a starting state whose marker
// row did not reach a slave.
var markerMissing = &Unmet{Member: "hab-2", Kind: UnmetData,
	Detail: "missing gate31-000001: acknowledged to the client and not found (2 violations in all)"}

// A check that does not hold within its limit names its last failing
// condition, the member and the rule, in its entry (starting-state.jsonl)
// and in the reason (summary.json), also when the last check did not stop.
func TestGate_ATimeoutNamesTheLastFailingCondition(t *testing.T) {
	for name, hang := range map[string]bool{"not restored": false, "still running": true} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			g := &Gate{Limit: 60 * time.Millisecond, Poll: 5 * time.Millisecond, Stop: 20 * time.Millisecond,
				Verify: func(context.Context) error {
					calls++
					switch {
					case calls == 1:
						return &Unmet{Member: "hab-1", Kind: UnmetRole, Detail: "neither an active master nor a standby slave"}
					case hang && calls > 2:
						select {}
					}
					return fmt.Errorf("the data rules do not hold: %w", markerMissing)
				}}
			e := g.Enter(context.Background(), "S14-token-rotation", nil)
			if e.Verified || e.Unmet == nil || *e.Unmet != *markerMissing {
				t.Fatalf("want the last failing condition %+v in the entry, got %+v", markerMissing, e)
			}
			line, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				`"lastFailingCondition":{`, `"member":"hab-2"`, `"kind":"data"`, `missing gate31-000001`,
			} {
				if !strings.Contains(string(line), want) {
					t.Errorf("the record does not contain %s: %s", want, line)
				}
			}
			for _, want := range []string{"hab-2", "missing gate31-000001"} {
				if !strings.Contains(e.Reason, want) {
					t.Errorf("the reason does not name %q: %s", want, e.Reason)
				}
			}
		})
	}
}

// The failing condition kept is bounded: a long detail, such as a whole data
// report, is cut.
func TestGate_TheFailingConditionIsBounded(t *testing.T) {
	long := strings.Repeat("ö", 4*MaxUnmetDetail)
	g := &Gate{Limit: 30 * time.Millisecond, Poll: 5 * time.Millisecond,
		Verify: func(context.Context) error { return errors.New(long) }}
	e := g.Enter(context.Background(), "S14", nil)
	if e.Unmet == nil {
		t.Fatalf("want a failing condition, got %+v", e)
	}
	if e.Unmet.Kind != UnmetOther || len(e.Unmet.Detail) > MaxUnmetDetail+64 ||
		!utf8.ValidString(e.Unmet.Detail) || !strings.Contains(e.Unmet.Detail, "bytes cut") {
		t.Fatalf("want a bounded detail, got %d bytes: %+v", len(e.Unmet.Detail), e.Unmet)
	}
	if len(e.Reason) > 2*MaxUnmetDetail+256 || !utf8.ValidString(e.Reason) {
		t.Errorf("the reason is not bounded or not valid UTF-8: %d bytes", len(e.Reason))
	}
	// A starting state that holds keeps no failing condition.
	ok := &fakeSystem{holdsFrom: 2}
	if e := ok.gate().Enter(context.Background(), "S02", nil); !e.Verified || e.Unmet != nil {
		t.Errorf("a verified entry carries a failing condition: %+v", e)
	}
	// The reason of a broken order dependency is bounded too.
	needs := func(context.Context) error { return errors.New(long) }
	if e := (&fakeSystem{holdsFrom: 1}).gate().Enter(context.Background(), "S03", needs); !e.Verified ||
		len(e.Reason) > 2*MaxUnmetDetail+256 || !utf8.ValidString(e.Reason) {
		t.Errorf("the order-dependency reason is not bounded or not valid UTF-8: %d bytes", len(e.Reason))
	}
}

// A secret is redacted before the detail is cut: a cut through it would
// leave a part the redaction no longer recognizes.
func TestGate_TheFailingConditionIsRedactedBeforeItIsCut(t *testing.T) {
	const secret = "s3cr3t-token"
	detail := strings.Repeat("a", MaxUnmetDetail-4) + secret + strings.Repeat("b", MaxUnmetDetail)
	g := &Gate{Limit: 30 * time.Millisecond, Poll: 5 * time.Millisecond,
		Redact: func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") },
		Verify: func(context.Context) error { return errors.New(detail) }}
	e := g.Enter(context.Background(), "S14", nil)
	if e.Unmet == nil || strings.Contains(e.Unmet.Detail, "s3cr") || strings.Contains(e.Reason, "s3cr") {
		t.Errorf("a part of the secret is left: %+v", e)
	}
}

// cutShort returns a Verify that reports first and then, on every later
// call, waits for its context to be done, waits after for the given time,
// and reports second. returned is closed when such a call has returned.
func cutShort(first, second *Unmet, after time.Duration) (verify func(context.Context) error, returned chan struct{}) {
	var calls atomic.Int32
	returned = make(chan struct{})
	var once sync.Once
	return func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return first
		}
		<-ctx.Done()
		time.Sleep(after)
		once.Do(func() { close(returned) })
		return second
	}, returned
}

// A check cut short by the limit says nothing about the starting state: what
// it returns does not replace the condition an earlier check found, whether
// it returns within the stop period or after it.
func TestGate_ACheckCutShortKeepsTheEarlierCondition(t *testing.T) {
	later := &Unmet{Member: "hab-0", Kind: UnmetPod, Detail: "cut short"}
	t.Run("returns within the stop period", func(t *testing.T) {
		verify, _ := cutShort(markerMissing, later, 0)
		g := &Gate{Limit: 60 * time.Millisecond, Poll: 5 * time.Millisecond, Stop: 200 * time.Millisecond, Verify: verify}
		e := g.Enter(context.Background(), "S14", nil)
		if e.Verified || !strings.HasPrefix(e.Reason, ReasonStartingStateNotRestored) ||
			e.Unmet == nil || *e.Unmet != *markerMissing {
			t.Errorf("want %+v kept, got %+v", markerMissing, e)
		}
	})
	t.Run("returns after the stop period", func(t *testing.T) {
		verify, returned := cutShort(markerMissing, later, 60*time.Millisecond)
		g := &Gate{Limit: 60 * time.Millisecond, Poll: 5 * time.Millisecond, Stop: 20 * time.Millisecond, Verify: verify}
		e := g.Enter(context.Background(), "S14", nil)
		if !strings.HasPrefix(e.Reason, ReasonStepStillRunning) || e.Unmet == nil || *e.Unmet != *markerMissing {
			t.Fatalf("want %s with %+v kept, got %+v", ReasonStepStillRunning, markerMissing, e)
		}
		<-returned
		time.Sleep(20 * time.Millisecond)
		if u := g.lastUnmet(); u == nil || *u != *markerMissing {
			t.Errorf("the check that returned late replaced the condition kept: %+v", u)
		}
	})
}

// Each Enter starts with no failing condition: one that an earlier variant's
// checks found is not reported for a later one whose only check was cut
// short.
func TestGate_AFailingConditionIsNotCarriedToTheNextVariant(t *testing.T) {
	var calls atomic.Int32
	g := &Gate{Limit: 60 * time.Millisecond, Poll: 5 * time.Millisecond, Stop: 200 * time.Millisecond,
		Verify: func(ctx context.Context) error {
			switch calls.Add(1) {
			case 1:
				return markerMissing
			case 2:
				return nil
			}
			<-ctx.Done()
			return &Unmet{Kind: UnmetPod, Detail: "cut short"}
		}}
	if e := g.Enter(context.Background(), "S02", nil); !e.Verified {
		t.Fatalf("the first variant fails once and then holds, got %+v", e)
	}
	if e := g.Enter(context.Background(), "S03", nil); e.Verified || e.Unmet != nil {
		t.Errorf("want no failing condition for a variant whose only check was cut short, got %+v", e.Unmet)
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
