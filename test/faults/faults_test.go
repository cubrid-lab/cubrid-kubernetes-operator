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

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// fakeFault is a fault whose every step is scripted.
type fakeFault struct {
	injectErr    error
	confirmAfter int // Confirm returns true from this call on; 0 never
	confirmErr   error
	removeErr    error
	hangIn       string // a step that ignores its context and never returns

	injected, confirms, removed int
}

func (f *fakeFault) Name() string { return "fake fault" }

func (f *fakeFault) Inject(context.Context) error {
	f.injected++
	if f.hangIn == stepInject {
		select {}
	}
	return f.injectErr
}

func (f *fakeFault) Confirm(context.Context) (bool, error) {
	f.confirms++
	if f.hangIn == stepConfirm {
		select {}
	}
	if f.confirmErr != nil {
		return false, f.confirmErr
	}
	return f.confirmAfter > 0 && f.confirms >= f.confirmAfter, nil
}

func (f *fakeFault) Remove(context.Context) error {
	f.removed++
	if f.hangIn == stepRemove {
		select {}
	}
	return f.removeErr
}

// Steps a fakeFault can hang in.
const (
	stepInject  = "inject"
	stepConfirm = "confirm"
	stepRemove  = "remove"
)

var short = Limits{Confirm: 150 * time.Millisecond, Outcome: 150 * time.Millisecond,
	Cleanup: 150 * time.Millisecond, Poll: 5 * time.Millisecond}

func holds(context.Context) (bool, error)  { return true, nil }
func notYet(context.Context) (bool, error) { return false, nil }
func ok(context.Context) error             { return nil }
func kinds(o Outcome) string {
	out := make([]string, 0, len(o.Timeline))
	for _, s := range o.Timeline {
		out = append(out, s.Kind)
	}
	return strings.Join(out, ",")
}

func TestRun_Pass(t *testing.T) {
	f := &fakeFault{confirmAfter: 3}
	checks := 0
	o := Run(context.Background(), Scenario{Fault: f, Limits: short, Before: ok,
		Check: func(context.Context) (bool, error) { checks++; return checks >= 2, nil }})
	if o.Result != evidence.Pass || o.Reason != "" || !o.FaultConfirmed || !o.CleanupSucceeded {
		t.Fatalf("outcome = %+v", o)
	}
	if f.injected != 1 || f.confirms != 3 || f.removed != 1 || checks != 2 {
		t.Errorf("calls: inject %d, confirm %d, remove %d, check %d", f.injected, f.confirms, f.removed, checks)
	}
	if o.FaultIssuedAt.IsZero() || o.OutcomeAt.Before(o.FaultIssuedAt) {
		t.Errorf("times: issued %v, outcome %v", o.FaultIssuedAt, o.OutcomeAt)
	}
	want := "starting_state_verified,fault_issued,fault_confirmed,outcome_reached,fault_removed"
	if got := kinds(o); got != want {
		t.Errorf("timeline = %s, want %s", got, want)
	}
}

// A fault that was not confirmed can never give a pass, whatever the checks say.
func TestRun_NotConfirmedIsBlocked(t *testing.T) {
	for name, f := range map[string]*fakeFault{
		"never observed":            {},
		"observation keeps failing": {confirmErr: errors.New("kubectl: connection refused")},
	} {
		checked := false
		o := Run(context.Background(), Scenario{Fault: f, Limits: short, Before: ok,
			Check: func(context.Context) (bool, error) { checked = true; return true, nil }})
		if o.Result != evidence.Blocked || !strings.HasPrefix(o.Reason, ReasonFaultNotConfirmed) || o.FaultConfirmed {
			t.Errorf("%s: outcome = %+v", name, o)
		}
		if checked {
			t.Errorf("%s: the outcome was judged although the fault was not confirmed", name)
		}
		if f.removed != 1 {
			t.Errorf("%s: cleanup ran %d times, want 1", name, f.removed)
		}
	}
	// The last error of the observation is part of the reason.
	f := &fakeFault{confirmErr: errors.New("connection refused")}
	o := Run(context.Background(), Scenario{Fault: f, Limits: short, Before: ok, Check: holds})
	if !strings.Contains(o.Reason, "connection refused") {
		t.Errorf("reason = %q, want the observation's error", o.Reason)
	}
}

func TestRun_BlockedBeforeTheFault(t *testing.T) {
	t.Run("starting state not verified", func(t *testing.T) {
		f := &fakeFault{confirmAfter: 1}
		o := Run(context.Background(), Scenario{Fault: f, Limits: short, Check: holds,
			Before: func(context.Context) error { return errors.New("two masters") }})
		if o.Result != evidence.Blocked || !strings.HasPrefix(o.Reason, ReasonStartingState) ||
			!strings.Contains(o.Reason, "two masters") {
			t.Errorf("outcome = %+v", o)
		}
		if f.injected != 0 || f.removed != 0 {
			t.Errorf("the fault was touched: inject %d, remove %d", f.injected, f.removed)
		}
	})
	t.Run("injection failed", func(t *testing.T) {
		f := &fakeFault{injectErr: errors.New("exit status 1"), confirmAfter: 1}
		o := Run(context.Background(), Scenario{Fault: f, Limits: short, Before: ok, Check: holds})
		if o.Result != evidence.Blocked || !strings.HasPrefix(o.Reason, ReasonInjectionFailed) {
			t.Errorf("outcome = %+v", o)
		}
		// A command that failed may still have changed something.
		if f.confirms != 0 || f.removed != 1 {
			t.Errorf("calls: confirm %d, remove %d, want 0 and 1", f.confirms, f.removed)
		}
	})
	t.Run("a time limit is not set", func(t *testing.T) {
		for _, limits := range []Limits{
			{Outcome: time.Second, Cleanup: time.Second, Poll: time.Millisecond},
			{Confirm: time.Second, Cleanup: time.Second, Poll: time.Millisecond},
			{Confirm: time.Second, Outcome: time.Second, Poll: time.Millisecond},
			{Confirm: time.Second, Outcome: time.Second, Cleanup: time.Second},
		} {
			f := &fakeFault{confirmAfter: 1}
			o := Run(context.Background(), Scenario{Fault: f, Limits: limits, Before: ok, Check: holds})
			if o.Result != evidence.Blocked || o.Reason != ReasonTimeLimitUnset || f.injected != 0 {
				t.Errorf("limits %+v: outcome = %+v, injected %d", limits, o, f.injected)
			}
		}
	})
}

func TestRun_Fail(t *testing.T) {
	t.Run("an expectation is broken", func(t *testing.T) {
		f := &fakeFault{confirmAfter: 1}
		o := Run(context.Background(), Scenario{Fault: f, Limits: short, Before: ok,
			Check: func(context.Context) (bool, error) { return false, errors.New("an acknowledged row is missing") }})
		if o.Result != evidence.Fail || !strings.Contains(o.Reason, "an acknowledged row is missing") || !o.FaultConfirmed {
			t.Errorf("outcome = %+v", o)
		}
		if f.removed != 1 {
			t.Errorf("cleanup ran %d times, want 1", f.removed)
		}
	})
	t.Run("the outcome is not reached in time", func(t *testing.T) {
		f := &fakeFault{confirmAfter: 1}
		o := Run(context.Background(), Scenario{Fault: f, Limits: short, Before: ok, Check: notYet})
		if o.Result != evidence.Fail || !strings.Contains(o.Reason, "time limit") || !o.OutcomeAt.IsZero() {
			t.Errorf("outcome = %+v", o)
		}
	})
}

// Cleanup is recorded and never replaces what the run found first.
func TestRun_CleanupFailureKeepsTheFirstFinding(t *testing.T) {
	f := &fakeFault{confirmAfter: 1, removeErr: errors.New("rule not found")}
	o := Run(context.Background(), Scenario{Fault: f, Limits: short, Before: ok,
		Check: func(context.Context) (bool, error) { return false, errors.New("two masters reported healthy") }})
	if o.Result != evidence.Fail || !strings.Contains(o.Reason, "two masters reported healthy") {
		t.Errorf("outcome = %+v, want the first failure", o)
	}
	if o.CleanupSucceeded || strings.Contains(o.Reason, "rule not found") {
		t.Errorf("cleanup: succeeded %v, reason %q", o.CleanupSucceeded, o.Reason)
	}
	if !strings.HasSuffix(kinds(o), "cleanup_failed") {
		t.Errorf("timeline = %s, want it to end with cleanup_failed", kinds(o))
	}

	passed := Run(context.Background(), Scenario{Fault: &fakeFault{confirmAfter: 1, removeErr: errors.New("x")},
		Limits: short, Before: ok, Check: holds})
	if passed.Result != evidence.Pass || passed.CleanupSucceeded {
		t.Errorf("outcome = %+v, want a pass with a failed cleanup recorded", passed)
	}
}

// No wait runs forever, also when a step ignores its context.
func TestRun_NeverHangs(t *testing.T) {
	for step, want := range map[string]evidence.Result{
		stepInject:  evidence.Blocked,
		stepConfirm: evidence.Blocked,
		stepRemove:  evidence.Pass,
	} {
		f := &fakeFault{confirmAfter: 1, hangIn: step}
		done := make(chan Outcome, 1)
		go func() { done <- Run(context.Background(), Scenario{Fault: f, Limits: short, Before: ok, Check: holds}) }()
		select {
		case o := <-done:
			if o.Result != want {
				t.Errorf("hang in %s: result = %q (%s), want %q", step, o.Result, o.Reason, want)
			}
			if step == stepRemove && o.CleanupSucceeded {
				t.Errorf("hang in remove: cleanup reported as succeeded")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("hang in %s: Run did not return", step)
		}
	}
	for name, s := range map[string]Scenario{
		"before": {Before: func(context.Context) error { select {} }, Check: holds},
		"check":  {Before: ok, Check: func(context.Context) (bool, error) { select {} }},
	} {
		s.Fault, s.Limits = &fakeFault{confirmAfter: 1}, short
		done := make(chan Outcome, 1)
		go func() { done <- Run(context.Background(), s) }()
		select {
		case o := <-done:
			if o.Result == evidence.Pass {
				t.Errorf("hang in %s: passed", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("hang in %s: Run did not return", name)
		}
	}
}

// A cancelled run still removes its fault.
func TestRun_CancelledStillCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeFault{confirmAfter: 1}
	o := Run(ctx, Scenario{Fault: f, Limits: short, Before: ok,
		Check: func(context.Context) (bool, error) { cancel(); return false, nil }})
	if o.Result == evidence.Pass || f.removed != 1 || !o.CleanupSucceeded {
		t.Errorf("outcome = %+v, removed %d", o, f.removed)
	}
}

func TestOutcome_Scenario(t *testing.T) {
	issued := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	o := Outcome{Result: evidence.Pass, FaultConfirmed: true, CleanupSucceeded: true,
		FaultIssuedAt: issued, OutcomeAt: issued.Add(21400 * time.Millisecond)}
	s := o.Scenario("S03", "abrupt", short)
	if s.Name() != "S03/abrupt" || s.Result != evidence.Pass || s.FaultConfirmed == nil || !*s.FaultConfirmed ||
		s.CleanupSucceeded == nil || !*s.CleanupSucceeded {
		t.Errorf("scenario = %+v", s)
	}
	if s.Measurements["recoveryTime"] != "21.4s" || s.Limits["outcome"] != "150ms" {
		t.Errorf("measurements = %v, limits = %v", s.Measurements, s.Limits)
	}
	// Not measured is unknown, never zero.
	blocked := Outcome{Result: evidence.Blocked, Reason: ReasonFaultNotConfirmed, FaultIssuedAt: issued}
	if got := blocked.Scenario("S03", "", short).Measurements["recoveryTime"]; got != evidence.Unknown {
		t.Errorf("recoveryTime = %v, want %q", got, evidence.Unknown)
	}
}

// When the limit passes while an observation is still running, the reason is
// what the observations before it reported, not only that time ran out.
func TestRun_NotConfirmedKeepsTheObservationsError(t *testing.T) {
	calls := 0
	f := &slowConfirm{confirm: func(ctx context.Context) (bool, error) {
		calls++
		if calls == 1 {
			return false, errors.New("connection refused")
		}
		<-ctx.Done() // the second observation runs into the limit
		return false, ctx.Err()
	}}
	o := Run(context.Background(), Scenario{Fault: f, Limits: short, Before: ok, Check: holds})
	if o.Result != evidence.Blocked || !strings.Contains(o.Reason, "connection refused") {
		t.Errorf("outcome = %s %q, want blocked with the observation's error", o.Result, o.Reason)
	}
}

type slowConfirm struct {
	confirm func(context.Context) (bool, error)
}

func (f *slowConfirm) Name() string                              { return "slow confirm" }
func (f *slowConfirm) Inject(context.Context) error              { return nil }
func (f *slowConfirm) Confirm(ctx context.Context) (bool, error) { return f.confirm(ctx) }
func (f *slowConfirm) Remove(context.Context) error              { return nil }
