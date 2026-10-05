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

// Package faults runs the steps every failure scenario shares, as
// docs/testing/scenario-contract.md defines them: verify the starting state,
// inject a fault, confirm that it took effect, wait for the outcome, and
// clean up. A scenario supplies its own Fault and its own checks.
package faults

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// Reasons for a blocked result.
const (
	ReasonTimeLimitUnset    = "time_limit_unset"
	ReasonStartingState     = "starting_state_not_verified"
	ReasonInjectionFailed   = "fault_injection_failed"
	ReasonFaultNotConfirmed = "fault_not_confirmed"
)

// Fault is what a scenario plugs in.
type Fault interface {
	// Name says what the fault is, for the timeline.
	Name() string
	// Inject issues the fault.
	Inject(ctx context.Context) error
	// Confirm reports whether the fault took effect, by observing the
	// system. It is called repeatedly until it returns true.
	Confirm(ctx context.Context) (bool, error)
	// Remove takes the fault away. It is called once after every run in
	// which Inject was called, and does nothing for a fault that is permanent.
	Remove(ctx context.Context) error
}

// Limits are the time limits of one run. None may be zero.
type Limits struct {
	// Confirm bounds the wait for the fault to take effect.
	Confirm time.Duration
	// Outcome bounds the wait for the expected behavior.
	Outcome time.Duration
	// Cleanup bounds the removal of the fault.
	Cleanup time.Duration
	// Poll is the pause between two observations.
	Poll time.Duration
}

// Scenario is what a scenario supplies besides its fault.
type Scenario struct {
	Fault  Fault
	Limits Limits
	// Before verifies the starting state.
	Before func(ctx context.Context) error
	// Check observes the expected behavior. It returns true when it holds,
	// false to be asked again, and an error when an expectation is broken.
	Check func(ctx context.Context) (bool, error)
}

// Step is one line of the run's timeline.
type Step struct {
	T      time.Time `json:"t"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail,omitempty"`
}

// Outcome is the result of one run.
type Outcome struct {
	Result           evidence.Result
	Reason           string
	FaultConfirmed   bool
	CleanupSucceeded bool
	// FaultIssuedAt is when Inject was called; zero when it never was.
	FaultIssuedAt time.Time
	// OutcomeAt is when Check first held; zero when it never did.
	OutcomeAt time.Time
	Timeline  []Step
}

// errStepTimeout is returned for a step that did not finish within its limit.
var errStepTimeout = errors.New("the step did not return within its time limit")

// Run executes the scenario. It always returns: no step waits longer than its
// limit, also when a function of the scenario ignores its context. The fault
// is removed after every run in which it was issued, also when ctx is
// cancelled, and a failed removal never replaces what the run found first.
func Run(ctx context.Context, s Scenario) (o Outcome) {
	note := func(kind, detail string) {
		o.Timeline = append(o.Timeline, Step{T: time.Now().UTC(), Kind: kind, Detail: detail})
	}
	end := func(result evidence.Result, kind, reason string) {
		o.Result, o.Reason = result, reason
		note(kind, reason)
	}

	l := s.Limits
	if l.Confirm <= 0 || l.Outcome <= 0 || l.Cleanup <= 0 || l.Poll <= 0 {
		end(evidence.Blocked, ReasonTimeLimitUnset, ReasonTimeLimitUnset)
		return o
	}

	if err := bounded(ctx, l.Confirm, s.Before); err != nil {
		end(evidence.Blocked, ReasonStartingState, ReasonStartingState+": "+err.Error())
		return o
	}
	note("starting_state_verified", "")

	o.FaultIssuedAt = time.Now().UTC()
	note("fault_issued", s.Fault.Name())
	defer func() {
		// Not derived from a cancelled ctx: the fault must not outlive the run.
		if err := bounded(context.WithoutCancel(ctx), l.Cleanup, s.Fault.Remove); err != nil {
			note("cleanup_failed", err.Error())
			return
		}
		o.CleanupSucceeded = true
		note("fault_removed", "")
	}()

	if err := bounded(ctx, l.Confirm, s.Fault.Inject); err != nil {
		end(evidence.Blocked, ReasonInjectionFailed, ReasonInjectionFailed+": "+err.Error())
		return o
	}

	confirmed, lastErr := poll(ctx, l.Confirm, l.Poll, false, s.Fault.Confirm)
	if !confirmed {
		reason := ReasonFaultNotConfirmed
		if lastErr != nil {
			reason += ": " + lastErr.Error()
		}
		end(evidence.Blocked, ReasonFaultNotConfirmed, reason)
		return o
	}
	o.FaultConfirmed = true
	note("fault_confirmed", "")

	reached, err := poll(ctx, l.Outcome, l.Poll, true, s.Check)
	switch {
	case reached:
		o.OutcomeAt = time.Now().UTC()
		end(evidence.Pass, "outcome_reached", "")
	case err != nil && !errors.Is(err, errStepTimeout):
		end(evidence.Fail, "expectation_broken", err.Error())
	case ctx.Err() != nil:
		end(evidence.Blocked, "run_cancelled", "run_cancelled: "+ctx.Err().Error())
	default:
		end(evidence.Fail, "time_limit_exceeded",
			fmt.Sprintf("time limit exceeded: the expected behavior was not observed within %s", l.Outcome))
	}
	return o
}

// bounded calls fn and returns when it does or when the limit passes,
// whichever is first. A fn that ignores its context is left behind.
func bounded(ctx context.Context, limit time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errStepTimeout
	}
}

// poll calls fn until it returns true or the limit passes. With stopOnError
// an error ends the wait; without it the error is remembered and fn is asked
// again. It returns the last error.
func poll(ctx context.Context, limit, every time.Duration, stopOnError bool,
	fn func(context.Context) (bool, error)) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var lastErr error
	for {
		done := false
		err := bounded(ctx, limit, func(ctx context.Context) error {
			var err error
			done, err = fn(ctx)
			return err
		})
		switch {
		case err == nil && done:
			return true, nil
		case err != nil && (stopOnError || errors.Is(err, errStepTimeout)):
			return false, err
		case err != nil:
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return false, lastErr
		case <-time.After(every):
		}
	}
}

// Scenario converts the outcome into the entry of summary.json. A recovery
// time that was not measured is "unknown".
func (o Outcome) Scenario(id, variant string, limits Limits) evidence.Scenario {
	s := evidence.Scenario{
		ID: id, Variant: variant, Result: o.Result, Reason: o.Reason,
		FaultConfirmed: &o.FaultConfirmed,
		Limits: map[string]string{
			"confirm": limits.Confirm.String(),
			"outcome": limits.Outcome.String(),
			"cleanup": limits.Cleanup.String(),
		},
		Measurements: map[string]any{"recoveryTime": evidence.Unknown},
	}
	if !o.FaultIssuedAt.IsZero() {
		s.CleanupSucceeded = &o.CleanupSucceeded
	}
	if !o.FaultIssuedAt.IsZero() && !o.OutcomeAt.IsZero() {
		s.Measurements["recoveryTime"] = o.OutcomeAt.Sub(o.FaultIssuedAt).String()
	}
	return s
}
