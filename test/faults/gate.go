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
	"time"
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
	// Restore puts back, where that is safe, what an earlier variant may
	// have left behind. It is called before each Verify and may be nil.
	Restore func(ctx context.Context) error
	// Verify returns what of the starting state does not hold, or nil.
	Verify func(ctx context.Context) error
	// Describe, if set, returns the verified state for the entry, such as
	// the identity of each member.
	Describe func(ctx context.Context) string

	blocked string
	spent   time.Duration
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

	held, err := poll(ctx, limit, g.Poll, false, g.attempt)
	g.spent += time.Since(start)
	if !held {
		g.Block(fmt.Sprintf("%s before %s within %s%s: %v",
			ReasonStartingStateNotRestored, variant, limit.Round(time.Millisecond), budgetNote, err))
		e.Reason = g.blocked
		return e
	}
	e.Verified = true
	if g.Describe != nil {
		e.State = g.Describe(ctx)
	}
	if needs != nil {
		if err := bounded(ctx, g.Limit, needs); err != nil {
			e.Reason = ReasonOrderDependency + ": " + err.Error()
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
		return false, fmt.Errorf("held, and no longer %s later: %w", g.Settle, err)
	}
	return true, nil
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
