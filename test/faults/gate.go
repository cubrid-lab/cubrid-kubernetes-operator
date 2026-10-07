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
// starting state. A variant starts only from a verified starting state. Once
// that state could not be restored, or the system was never formed (Block),
// every later variant is blocked at once, without being started.
type Gate struct {
	// Limit bounds the wait for the starting state; Poll is the pause
	// between two attempts.
	Limit, Poll time.Duration
	// Restore puts back, where that is safe, what an earlier variant may
	// have left behind. It is called before each Verify and may be nil.
	Restore func(ctx context.Context) error
	// Verify returns what of the starting state does not hold, or nil.
	Verify func(ctx context.Context) error

	blocked string
}

// Entry is what the gate decided for one variant, for the run's evidence.
type Entry struct {
	Variant string    `json:"variant"`
	T       time.Time `json:"t"`
	// Verified is true when the starting state held before the variant.
	Verified bool   `json:"startingStateVerified"`
	Waited   string `json:"waited"`
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

	held, err := poll(ctx, g.Limit, g.Poll, false, func(ctx context.Context) (bool, error) {
		if g.Restore != nil {
			if err := g.Restore(ctx); err != nil {
				return false, fmt.Errorf("restoring: %w", err)
			}
		}
		if err := g.Verify(ctx); err != nil {
			return false, err
		}
		return true, nil
	})
	if !held {
		g.Block(fmt.Sprintf("%s before %s within %s: %v", ReasonStartingStateNotRestored, variant, g.Limit, err))
		e.Reason = g.blocked
		return e
	}
	e.Verified = true
	if needs != nil {
		if err := bounded(ctx, g.Limit, needs); err != nil {
			e.Reason = ReasonOrderDependency + ": " + err.Error()
		}
	}
	return e
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
