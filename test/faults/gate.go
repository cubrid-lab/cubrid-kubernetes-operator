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
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// Reasons for a variant that a Gate keeps from starting.
const (
	ReasonStartingStateNotRestored = "starting_state_not_restored"
	ReasonOrderDependency          = "order_dependency_broken"
)

// Gate lets independent variants share one system without one variant's
// damage deciding another's result. Before each variant it puts back what an
// earlier one may have left and waits, within a limit, for the common
// starting state to hold twice, a settle period apart. A variant starts only
// from a verified starting state. Once that state could not be restored, the
// restore budget of the run is spent, or the system was never formed
// (Block), every later variant is blocked at once, without being started.
type Gate struct {
	// Limit bounds the wait for the starting state before one variant;
	// Poll is the pause between two attempts.
	Limit, Poll time.Duration
	// Settle is how long the starting state must keep holding: Verify must
	// succeed again this long after it first did. Zero checks once.
	Settle time.Duration
	// Budget bounds the time all variants together may wait for the
	// starting state, so that the waits fit in the run's own time limit.
	// Zero is no budget.
	Budget time.Duration
	// Stop bounds the wait for a restore or a verification to return after
	// the limit has passed and its context was cancelled. Zero means
	// DefaultStop.
	Stop time.Duration
	// Restore puts back, where that is safe, what an earlier variant may
	// have left behind. It is called before each Verify and may be nil.
	Restore func(ctx context.Context) error
	// Verify returns what of the starting state does not hold, or nil.
	Verify func(ctx context.Context) error
	// Describe, if set, returns the verified state for the entry, such as
	// the identity of each member.
	Describe func(ctx context.Context) string
	// Redact, if set, removes credentials from a failing condition and a
	// reason before they are cut to their bound. The evidence files are
	// redacted again when they are written.
	Redact func(string) string

	blocked string
	spent   time.Duration
	mu      sync.Mutex
	unmet   *Unmet // the condition the last check found not holding
}

// Entry is what the gate decided for one variant, for the run's evidence.
type Entry struct {
	Variant string    `json:"variant"`
	T       time.Time `json:"t"`
	// Verified is true when the starting state held before the variant.
	Verified bool   `json:"startingStateVerified"`
	Waited   string `json:"waited"`
	// State is what Describe returned for the verified starting state.
	State string `json:"state,omitempty"`
	// Reason is why the variant is blocked; empty when it may start.
	Reason string `json:"reason,omitempty"`
	// Unmet is the condition the last check found not holding, when the
	// starting state was not verified within the limit.
	Unmet *Unmet `json:"lastFailingCondition,omitempty"`
}

// Kinds of a condition of the starting state that does not hold.
const (
	UnmetPod       = "pod"
	UnmetRole      = "role"
	UnmetCondition = "condition"
	UnmetBroker    = "broker"
	UnmetOperator  = "operator"
	UnmetClient    = "client"
	UnmetData      = "data"
	// UnmetOther is a failure that does not say which condition it is,
	// such as a command that could not run.
	UnmetOther = "other"
)

// MaxUnmetDetail bounds the detail kept of a failing condition, so that the
// record stays small whatever the check returned.
const MaxUnmetDetail = 512

// Unmet is the first condition of the starting state that a check found not
// holding: on which member, if it concerns one, of which kind, and what.
// Verify returns it, possibly wrapped, as its error.
type Unmet struct {
	Member string `json:"member,omitempty"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

func (u *Unmet) Error() string {
	if u.Member == "" {
		return u.Kind + ": " + u.Detail
	}
	return u.Kind + " on " + u.Member + ": " + u.Detail
}

// unmetOf returns the failing condition err names, or err itself as one of
// kind UnmetOther, with its detail redacted and bounded.
func (g *Gate) unmetOf(err error) *Unmet {
	u := &Unmet{Kind: UnmetOther, Detail: err.Error()}
	if found := (*Unmet)(nil); errors.As(err, &found) {
		*u = *found
	}
	u.Detail = g.cut(u.Detail, MaxUnmetDetail)
	return u
}

// cut redacts s, then clips it to n bytes. Redacting first keeps a cut from
// leaving part of a secret that the redaction would no longer recognize.
func (g *Gate) cut(s string, n int) string {
	if g.Redact != nil {
		s = g.Redact(s)
	}
	return clip(s, n)
}

// clip cuts s to at most n bytes, at a character boundary, and says so.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (%d bytes cut)", s[:cut], len(s)-cut)
}

// Enter restores and verifies the starting state for a variant. needs is the
// variant's own precondition on top of it, such as an order dependency on
// the variants before it, or nil. A broken one blocks that variant only.
func (g *Gate) Enter(ctx context.Context, variant string, needs func(ctx context.Context) error) (e Entry) {
	start := time.Now()
	e = Entry{Variant: variant, T: start.UTC()}
	defer func() { e.Waited = time.Since(start).Round(time.Millisecond).String() }()
	switch {
	case g.blocked != "":
		e.Reason = g.blocked
		return e
	case g.Limit <= 0 || g.Poll <= 0:
		e.Reason = ReasonTimeLimitUnset
		return e
	}
	limit, budgetNote := g.Limit, ""
	if g.Budget > 0 {
		if left := g.Budget - g.spent; left < limit {
			limit, budgetNote = left, fmt.Sprintf(" (restore budget exhausted: %s for the run)", g.Budget)
		}
		if limit <= 0 {
			g.Block(fmt.Sprintf("%s before %s: restore budget exhausted: %s for the run",
				ReasonStartingStateNotRestored, variant, g.Budget))
			e.Reason = g.blocked
			return e
		}
	}

	stop := g.Stop
	if stop <= 0 {
		stop = DefaultStop
	}
	g.setUnmet(nil)
	held, err := poll(ctx, limit, g.Poll, stop, false, g.attempt)
	g.spent += time.Since(start)
	if !held {
		e.Unmet = g.lastUnmet()
	}
	if errors.Is(err, errStepStillRunning) {
		// Something of the restore or the verification is still running
		// against the system: no variant may start next to it.
		last := ""
		if e.Unmet != nil {
			last = "; the last check that returned found " + e.Unmet.Error()
		}
		g.Block(fmt.Sprintf("%s before %s: restoring or verifying the starting state did not stop after %s%s",
			ReasonStepStillRunning, variant, limit.Round(time.Millisecond), last))
		e.Reason = g.blocked
		return e
	}
	if !held {
		g.Block(fmt.Sprintf("%s before %s within %s%s: %s",
			ReasonStartingStateNotRestored, variant, limit.Round(time.Millisecond), budgetNote,
			g.cut(fmt.Sprint(err), 2*MaxUnmetDetail)))
		e.Reason = g.blocked
		return e
	}
	e.Verified = true
	if g.Describe != nil {
		e.State = g.Describe(ctx)
	}
	if needs != nil {
		switch err := bounded(ctx, g.Limit, stop, needs); {
		case errors.Is(err, errStepStillRunning):
			g.Block(fmt.Sprintf("%s before %s: checking the variant's precondition did not stop",
				ReasonStepStillRunning, variant))
			e.Reason = g.blocked
		case err != nil:
			e.Reason = ReasonOrderDependency + ": " + g.cut(err.Error(), 2*MaxUnmetDetail)
		}
	}
	return e
}

// attempt restores once and verifies the starting state, twice a settle
// period apart. A failed restore does not hide what Verify finds.
func (g *Gate) attempt(ctx context.Context) (bool, error) {
	var restoreErr error
	if g.Restore != nil {
		if err := g.Restore(ctx); err != nil {
			restoreErr = fmt.Errorf("restoring: %w", err)
		}
	}
	if err := errors.Join(restoreErr, g.Verify(ctx)); err != nil {
		g.note(ctx, err)
		return false, err
	}
	if g.Settle <= 0 {
		return true, nil
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(g.Settle):
	}
	if err := g.Verify(ctx); err != nil {
		err = fmt.Errorf("held, and no longer %s later: %w", g.Settle, err)
		g.note(ctx, err)
		return false, err
	}
	return true, nil
}

// note keeps what a check found not holding, unless the check was cut short
// by the limit and so says nothing about the starting state. It replaces
// the condition kept before, so that only the last one is kept.
func (g *Gate) note(ctx context.Context, err error) {
	if ctx.Err() == nil {
		g.setUnmet(g.unmetOf(err))
	}
}

// setUnmet and lastUnmet guard the condition kept: a check that did not
// stop may still return after Enter has.
func (g *Gate) setUnmet(u *Unmet) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unmet = u
}

func (g *Gate) lastUnmet() *Unmet {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.unmet
}

// Block keeps every later variant from starting, with the reason. The first
// reason is kept.
func (g *Gate) Block(reason string) {
	if g.blocked == "" {
		g.blocked = reason
	}
}

// Blocked returns why every later variant is blocked, or "".
func (g *Gate) Blocked() string { return g.blocked }
