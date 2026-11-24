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

package installed

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

// The tests in this file cover -partition-repeat (#115): once the first
// cut's quarantine was released and the read-write path reopened, the
// network partition lane cuts the variant's member off again and judges
// whether a new quarantine of a new generation closes the path again, whether
// a token of the first quarantine is ignored, and whether any write is
// acknowledged after the second closure.

// repeatConfig is the documented lab command: higher-rank-isolated with the
// shortened keepalive, the run's manual release and the second cut.
func repeatConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := keepaliveConfig(t)
	cfg.PartitionSettle, cfg.PartitionManualRelease, cfg.PartitionRepeat = 0, true, true
	cfg.QuarantineLimit, cfg.PartitionDuration = 30*time.Second, 2*time.Minute
	cfg.DataCheckWithin, cfg.Timeout = 2*time.Minute, 110*time.Minute
	return cfg
}

// The documented lab command is accepted, the second cut's own bounds count
// in the -timeout check, and the option is refused without a variant that
// quarantines and with the Operator restart.
func TestValidateS07PartitionRepeat(t *testing.T) {
	cfg := repeatConfig(t)
	if err := cfg.validate(); err != nil {
		t.Fatalf("the documented lab options are refused: %v", err)
	}
	off := repeatConfig(t)
	off.PartitionRepeat = false
	if got := cfg.s07StepBounds() - off.s07StepBounds(); got != cfg.s07RepeatBounds() || got == 0 {
		t.Errorf("-partition-repeat adds %s to the steps' bounds, want %s", got, cfg.s07RepeatBounds())
	}
	want := s07RepeatReadyBound + s06ClientStart + s07MoveBound + s07KeepaliveBound + s07PrepareBound +
		cfg.PartitionHold + s07ConfirmSlack + 2*s07StaleTokenWindow + s07OldTokenSlack + s07CleanupBound +
		s07SettleBound
	if got := cfg.s07RepeatBounds(); got != want {
		t.Errorf("the second cut's bounds are %s, want %s", got, want)
	}
	if got := cfg.s07StepBounds(); got != 93*time.Minute+50*time.Second {
		t.Errorf("the documented command's own bounds are %s, the guide says 93m50s", got)
	}
	cfg.Timeout = cfg.s07StepBounds()
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "-timeout") {
		t.Errorf("a timeout that only covers the steps' bounds: %v", err)
	}
	master := repeatConfig(t)
	master.Fault, master.PartitionKeepalive = FaultPartitionMasterIsolated, 0
	if err := master.validate(); err != nil {
		t.Errorf("master-isolated with the option is refused: %v", err)
	}
	for name, change := range map[string]func(*HAConfig){
		"without a partition fault": func(c *HAConfig) {
			c.Fault, c.PartitionKeepalive, c.PartitionManualRelease = "", 0, false
		},
		"the control":          func(c *HAConfig) { c.Fault = FaultPartitionSlaveIsolated },
		"the operator restart": func(c *HAConfig) { c.PartitionOperatorRestart = true },
	} {
		c := repeatConfig(t)
		change(&c)
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), "-partition-repeat") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// testFirstConsumedToken is the first quarantine's token that the run's
// manual release consumed.
const testFirstConsumedToken = "token-1b-consumed-by-the-release"

// testFirstUnconsumedToken is a token of the first quarantine that no
// release consumed.
const testFirstUnconsumedToken = "token-1a-revised-away"

// s07FirstCut is a first cut that settled after its quarantine of generation
// g1 was released by the run's own manual release.
func s07FirstCut() *s07Record {
	rec := s07Base(false)
	rec.Variant, rec.RepeatAsked, rec.Generations = s07HigherRankIsolated, true, []string{"g1"}
	released := at(*rec.SettledAt, 40)
	rec.ReleaseSeenAt, rec.ReleaseMethod = &released, string(databasev1alpha1.WriteQuarantineReleasedManually)
	rec.ReleaseReason, rec.QuarantineAtEnd = string(databasev1alpha1.WriteQuarantinedMasterChanged), ""
	rec.tokens = []s07SeenToken{{"g1", testFirstUnconsumedToken}, {"g1", testFirstConsumedToken}}
	rec.ManualRelease = &s07ManualRelease{GenerationID: "g1", AcceptedSeenAt: &released,
		currentToken: testFirstConsumedToken}
	return rec
}

// Only a first cut that settled, quarantined and had its quarantine released,
// by the run's accepted release when one was asked for, without a rule broken
// already, leads to a second cut.
func TestS07RepeatUnready(t *testing.T) {
	if why := s07RepeatUnready(s07FirstCut()); why != "" {
		t.Fatalf("a released first cut is refused: %s", why)
	}
	for name, tc := range map[string]struct {
		change func(*s07Record)
		want   string
	}{
		"not settled":         {func(r *s07Record) { r.SettledAt = nil }, "did not settle"},
		"no first quarantine": {func(r *s07Record) { r.QuarantineSeenAt = nil }, "no quarantine was seen"},
		"the release was not taken": {func(r *s07Record) {
			r.ManualRelease.AcceptedSeenAt, r.ManualRelease.Unmet = nil, "the Operator turned it down"
		}, "the Operator turned it down"},
		"no release asked": {func(r *s07Record) {
			r.ManualRelease, r.ReleaseSeenAt = nil, nil
			r.QuarantineInForce = string(databasev1alpha1.WriteQuarantinedMasterChanged)
		}, "-partition-manual-release"},
		"not released": {func(r *s07Record) { r.ManualRelease, r.ReleaseSeenAt = nil, nil }, "not seen released"},
		"a rule broken": {func(r *s07Record) { r.ReopenedWhileQuarantined = []string{"reopened"} },
			"already broke a rule: reopened"},
		"path never closed": {func(r *s07Record) { r.RWPodsGoneAt = nil }, "already broke a rule"},
	} {
		rec := s07FirstCut()
		tc.change(rec)
		if why := s07RepeatUnready(rec); !strings.Contains(why, tc.want) {
			t.Errorf("%s: %q, want %q", name, why, tc.want)
		}
	}
}

// The tokens tried against the second quarantine are the one the first cut's
// release consumed, then the newest one no release consumed; without a
// release by the run, only the newest one seen.
func TestS07EarlierTokens(t *testing.T) {
	tokens, olds := s07EarlierTokens(s07FirstCut())
	if len(tokens) != 2 || len(olds) != 2 ||
		olds[0] != (s07OldTry{s07SeenToken{"g1", testFirstConsumedToken}, s07OldConsumed, s07CheckConsumed}) ||
		olds[1] != (s07OldTry{s07SeenToken{"g1", testFirstUnconsumedToken}, s07OldUnconsumed, s07CheckNotCurrent}) {
		t.Errorf("tokens %v, olds %v", tokens, olds)
	}
	rec := s07FirstCut()
	rec.ManualRelease = nil
	if _, olds = s07EarlierTokens(rec); len(olds) != 1 || olds[0].token.Token != testFirstConsumedToken ||
		olds[0].kind != s07OldUnconsumed {
		t.Errorf("without a release: olds %v", olds)
	}
	rec.tokens = nil
	if _, olds = s07EarlierTokens(rec); len(olds) != 0 {
		t.Errorf("without tokens: olds %v", olds)
	}
}

// s07RepeatBase is a run in which both cuts held every check: the second cut
// of ha-0, after the master moved off it again, started a quarantine of the
// new generation g2, the first quarantine's token was ignored in nine
// readings, and the cluster settled again.
func s07RepeatBase() *s07Record {
	rec := s07FirstCut()
	start := at(*rec.SettledAt, 120)
	c := s07Base(false)
	c.Variant, c.sub, c.Generations = s07HigherRankIsolated, s07RepeatDir, []string{"g2"}
	shift := start.Sub(*c.FaultIssuedAt)
	for _, t := range []**time.Time{&c.FaultIssuedAt, &c.FaultConfirmedAt, &c.HoldEndedAt, &c.FaultRemovedAt,
		&c.SettledAt, &c.TriggerObservedAt, &c.TriggerWindowStart, &c.QuarantineSeenAt, &c.RWScaledDownAt,
		&c.RWPodsGoneAt, &c.RWEndpointsEmptyAt, &c.NewConnectionsStoppedAt} {
		v := (*t).Add(shift)
		*t = &v
	}
	c.Data, c.HealthReadings = nil, 0
	c.Revisions[0].GenerationID = "g2"
	c.tokens = []s07SeenToken{{"g2", "token-2a"}}
	c.MasterMove = &s07MasterMove{From: m0, To: m1}
	set := at(*c.QuarantineSeenAt, 5)
	watched := at(set, 30)
	rep := &s07Repeat{StartedAt: &start, MasterBefore: m0, EarlierGenerations: []string{"g1"}, SessionStarted: true,
		Cut: c, earlierTokens: rec.tokens,
		olds: []s07OldTry{{s07SeenToken{"g1", testFirstConsumedToken}, s07OldConsumed, s07CheckConsumed}},
		OldTokens: []*s07OldToken{{Kind: s07OldConsumed, Check: s07CheckConsumed, Generation: "g1", Against: "g2",
			SetAt: &set, WatchedUntil: &watched, Readings: 9, QuarantineHeld: true, BrokersHeld: true, done: true}}}
	c.repeat, rec.Repeat = rep, rep
	return rec
}

func TestJudgeS07WithASecondCut(t *testing.T) {
	tests := map[string]struct {
		change func(*s07Record)
		result evidence.Result
		reason string
	}{
		"a new quarantine closed the path again within its limits": {nil, evidence.Pass,
			"second cut: with ha-0 cut off again (master ha-0, after the master moved off ha-0 to ha-1), a " +
				"WriteQuarantinedMemberUnverified quarantine of generation g2 started (earlier: g1)"},
		"a second quarantine of the first generation fails": {func(r *s07Record) {
			r.Repeat.Cut.Generations = []string{"g1"}
		}, evidence.Fail, "the quarantine has generation g1, which an earlier quarantine had"},
		"a generation seen while the master moved again is an earlier one": {func(r *s07Record) {
			r.Repeat.Cut.MasterMove.Quarantine = &s07MoveQuarantine{GenerationID: "g2"}
		}, evidence.Fail, "the quarantine has generation g2"},
		"a token of the first quarantine on the second fails": {func(r *s07Record) {
			r.Repeat.Cut.tokens = []s07SeenToken{{"g2", testFirstUnconsumedToken}}
		}, evidence.Fail, "release token is one an earlier quarantine had"},
		"a release on the first quarantine's token fails": {func(r *s07Record) {
			r.Repeat.OldTokens[0].NeverEvents = []string{s07SecondCut + "the new quarantine (generation g2) was released"}
		}, evidence.Fail, "the new quarantine (generation g2) was released"},
		"a write after the second closure fails": {func(r *s07Record) {
			r.Repeat.Cut.ReopenedWhileQuarantined = []string{"the write s07n90 through a new connection took effect"}
		}, evidence.Fail, "on the second cut: the write s07n90"},
		"an established-session write after the second closure fails": {func(r *s07Record) {
			c := r.Repeat.Cut
			c.QuarantineAtEnd, c.Session.AcknowledgedAfterClosed = string(databasev1alpha1.WriteQuarantinedMasterChanged), 2
		}, evidence.Fail, "on the second cut: 2 established-session write(s) were acknowledged after"},
		"a second path never seen closed fails": {func(r *s07Record) { r.Repeat.Cut.RWEndpointsEmptyAt = nil },
			evidence.Fail, "on the second cut: a write quarantine was recorded, but the run never saw"},
		"no second quarantine fails": {func(r *s07Record) {
			r.Repeat.Cut.QuarantineSeenAt, r.Repeat.Cut.Revisions, r.Repeat.Cut.Generations = nil, nil, nil
		}, evidence.Fail, "on the second cut: no write quarantine was recorded"},
		"new connections that stopped after the limit on the second cut fail": {func(r *s07Record) {
			v := r.Repeat.Cut.TriggerWindowStart.Add(2 * time.Minute)
			r.Repeat.Cut.NewConnectionsStoppedAt = &v
		}, evidence.Fail, "on the second cut, quarantine_limit exceeded: 2m0s > 1m0s"},
		"no trigger on the second cut blocks": {func(r *s07Record) { r.Repeat.Cut.TriggerObservedAt = nil },
			evidence.Blocked, "on the second cut: " + s07NoTrigger},
		"a second cut that did not settle blocks": {func(r *s07Record) { r.Repeat.Cut.SettledAt = nil },
			evidence.Blocked, "on the second cut: the cluster did not settle"},
		"a second session not read blocks": {func(r *s07Record) { r.Repeat.Cut.Session = nil },
			evidence.Blocked, "on the second cut: no established-session SQL history"},
		"a second cut not made blocks": {func(r *s07Record) {
			r.Repeat = &s07Repeat{NotMade: "the first cut's quarantine was not seen released"}
		}, evidence.Blocked, "the second cut was not made: the first cut's quarantine was not seen released"},
		"a second cut never reached blocks": {func(r *s07Record) { r.Repeat = nil }, evidence.Blocked,
			"did not reach the second cut"},
		"an old token never tried blocks": {func(r *s07Record) {
			r.Repeat.OldTokens = []*s07OldToken{{NotMade: "no release token of the first cut's quarantine was seen",
				done: true}}
		}, evidence.Blocked, "no token of the first quarantine was tried against the new one: no release token"},
		"a release by another token during the watch blocks": {func(r *s07Record) {
			r.Repeat.OldTokens[0].OtherRelease = "released Manual at 12:10:00 by a token the run did not set"
		}, evidence.Blocked, "by a token the run did not set during the watch"},
		"one reading after the old token blocks": {func(r *s07Record) { r.Repeat.OldTokens[0].Readings = 1 },
			evidence.Blocked, "read 1 time(s) after the token the first cut's manual release consumed was set"},
		"a watch cut short of its window blocks": {func(r *s07Record) {
			v := r.Repeat.OldTokens[0].SetAt.Add(10 * time.Second)
			r.Repeat.OldTokens[0].WatchedUntil = &v
		}, evidence.Blocked, "before its 30s window"},
		"an unconsumed token not tried blocks": {func(r *s07Record) {
			r.Repeat.olds = append(r.Repeat.olds, s07OldTry{s07SeenToken{"g1", testFirstUnconsumedToken}, s07OldUnconsumed,
				s07CheckNotCurrent})
		}, evidence.Blocked, "1 of the 2 tokens of the first quarantine were tried"},
		"a watch still running blocks": {func(r *s07Record) { r.Repeat.OldTokens[0].done = false },
			evidence.Blocked, "still running when the run was judged"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := s07RepeatBase()
			if tc.change != nil {
				tc.change(rec)
			}
			v := judgeS07(rec, testS07Limits)
			if v.Result != tc.result || !strings.Contains(v.Reason, tc.reason) {
				t.Errorf("judgeS07 = %s %q; want %s with %q", v.Result, v.Reason, tc.result, tc.reason)
			}
		})
	}
}

// A fail on the second cut's limit alone is marked as one, under the
// contract's limit name.
func TestJudgeS07MarksASecondCutTimeLimitOnlyFail(t *testing.T) {
	rec := s07RepeatBase()
	v := rec.Repeat.Cut.TriggerWindowStart.Add(40 * time.Second)
	rec.Repeat.Cut.NewConnectionsStoppedAt = &v
	got := judgeS07(rec, s07VMLabLimits)
	if got.Result != evidence.Fail || !got.TimeLimitOnly ||
		got.ExceededLimits[quarantineLimitName] != "second cut: 40s > 30s" {
		t.Errorf("judged %s %q, time limit only %t, exceeded %v", got.Result, got.Reason, got.TimeLimitOnly,
			got.ExceededLimits)
	}
	if !strings.HasPrefix(got.Reason, "on the second cut, quarantine_limit exceeded") {
		t.Errorf("reason %q", got.Reason)
	}
}

// The second cut's measurements are kept under secondCut, with whether its
// quarantine is of a new generation and whether the old token was ignored,
// and unknown where the run did not show it.
func TestS07RepeatMeasurements(t *testing.T) {
	m := s07Measurements(s07RepeatBase())
	second, ok := m["secondCut"].(map[string]any)
	if m["repeat"] != "made" || !ok || m["secondQuarantineNewGeneration"] != measuredTrue ||
		m["oldTokenIgnored"] != measuredTrue {
		t.Fatalf("measurements %v", m)
	}
	if second["quarantineWithin"] != "14s" || second["target"] != m0 || second["masterMoved"] != m0+" -> "+m1 {
		t.Errorf("second cut %v", second)
	}
	rec := s07RepeatBase()
	rec.Repeat.OldTokens[0].Readings = 1
	if m := s07Measurements(rec); m["oldTokenIgnored"] != evidence.Unknown {
		t.Errorf("one reading: oldTokenIgnored %v", m["oldTokenIgnored"])
	}
	rec.Repeat = &s07Repeat{NotMade: "x"}
	if m := s07Measurements(rec); m["repeat"] != s07NotMade || m["secondQuarantineNewGeneration"] != evidence.Unknown {
		t.Errorf("not made: %v", m)
	}
	if m := s07Measurements(s07Base(false)); m["repeat"] != nil {
		t.Errorf("without the option: %v", m["repeat"])
	}
}

// The new-connection writes are split at the second cut's start, and the data
// check compares the rows with the master the second cut settled on.
func TestS07SplitProbesAndEndMaster(t *testing.T) {
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	first, second := s07SplitProbes([]s07Probe{{Client: "s07n91", StartedAt: at(start, -1)},
		{Client: "s07n92", StartedAt: start}, {Client: "s07n93", StartedAt: at(start, 1)}}, start)
	if len(first) != 1 || first[0].Client != "s07n91" || len(second) != 2 {
		t.Errorf("first %v, second %v", first, second)
	}
	rec := s07RepeatBase()
	rec.FinalMaster, rec.Repeat.Cut.FinalMaster, rec.Repeat.Cut.FinalSlaves = m1, m2, []string{m0, m1}
	if master, slaves, _ := s07EndMaster(rec); master != m2 || !slices.Equal(slaves, []string{m0, m1}) {
		t.Errorf("end master %s %v", master, slaves)
	}
	rec.Repeat.Cut.SettledAt = nil
	if master, _, _ := s07EndMaster(rec); master != m1 {
		t.Errorf("an unsettled second cut: end master %s", master)
	}
}

// oldTokenCut is a second cut's record whose quarantine, g1 in the release
// fixture, is enforced with the read-write path at zero, and which tries the
// first quarantine's token, token-0-of-the-first-quarantine of generation g0.
func oldTokenCut(f *s07ReleaseFixture) *s07Record {
	confirmed := time.Now().UTC()
	enforced := confirmed
	rep := &s07Repeat{olds: []s07OldTry{{s07SeenToken{"g0", "token-0-of-the-first-quarantine"}, s07OldConsumed,
		s07CheckConsumed}}}
	c := &s07Record{Variant: s07HigherRankIsolated, sub: s07RepeatDir, repeat: rep, FaultConfirmedAt: &confirmed,
		QuarantineInForce: string(databasev1alpha1.WriteQuarantinedMasterChanged), EnforcedTime: &enforced,
		Generations: []string{"g1"}, Path: []s07Path{{}}}
	rep.Cut = c
	f.h.cfg.PartitionHold = time.Minute
	return c
}

// stepOldToken runs the old token's step on every poll until it ended.
func stepOldToken(t *testing.T, f *s07ReleaseFixture, c *s07Record) {
	t.Helper()
	for i := 0; !s07OldTokenSettled(c); i++ {
		if i > 1000 {
			t.Fatalf("the old token's step did not end: %+v", c.repeat.OldTokens)
		}
		f.h.stepS07OldToken(context.Background(), c, time.Now().UTC())
		time.Sleep(f.h.cfg.Poll)
	}
}

// The first quarantine's token is set once the second quarantine is enforced
// with the path at zero, watched while the second quarantine holds, and
// removed when the watch ends; it is recorded by prefix only.
func TestStepS07OldTokenIsIgnored(t *testing.T) {
	f := newS07ReleaseFixture(t, releaseOn(testCurrentToken), nil)
	c := oldTokenCut(f)
	stepOldToken(t, f, c)
	o := c.repeat.OldTokens[0]
	if got := f.annotations(); !slices.Equal(got, []string{"token-0-of-the-first-quarantine"}) {
		t.Fatalf("annotations set %q, want the first quarantine's token", got)
	}
	if o.Against != "g1" || o.Generation != "g0" || o.Readings < 2 || !o.QuarantineHeld || !o.BrokersHeld ||
		len(o.NeverEvents) > 0 || o.WatchedUntil == nil || o.AnnotationRemovedByRun == nil || f.removals != 1 {
		t.Errorf("old token %+v, removals %d", o, f.removals)
	}
	if v := f.cluster(t).Annotations[releaseWriteQuarantineAnnotation]; v != "" {
		t.Errorf("the token is still set: %q", v)
	}
	if strings.Contains(string(mustJSON(t, o)), "token-0-of-the-first-quarantine") {
		t.Error("the token itself is in the evidence")
	}
}

// A release of the second quarantine on the first quarantine's token is a
// never-event, and Brokers above zero while it is in force are too.
func TestStepS07OldTokenThatReleases(t *testing.T) {
	f := newS07ReleaseFixture(t, releaseOn(""), nil)
	c := oldTokenCut(f)
	stepOldToken(t, f, c)
	o := c.repeat.OldTokens[0]
	if len(o.NeverEvents) != 1 || !strings.Contains(o.NeverEvents[0], "was released") || o.QuarantineHeld {
		t.Errorf("old token %+v", o)
	}
	f = newS07ReleaseFixture(t, releaseOn(testCurrentToken), nil)
	c = oldTokenCut(f)
	f.h.stepS07OldToken(context.Background(), c, time.Now().UTC())
	if len(c.repeat.OldTokens) != 1 || c.repeat.OldTokens[0].SetAt == nil {
		t.Fatalf("the token was not set: %+v", c.repeat.OldTokens)
	}
	c.Path = append(c.Path, s07Path{Desired: 1})
	stepOldToken(t, f, c)
	if o := c.repeat.OldTokens[0]; len(o.NeverEvents) != 1 || !strings.Contains(o.NeverEvents[0], "1 desired") {
		t.Errorf("old token with the Brokers back %+v", o)
	}
}

// No token is set when the second quarantine's own token is the old one,
// which would release it, when no old token was seen, or when the hold ended
// before the quarantine was enforced.
func TestStepS07OldTokenNotMade(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s07ReleaseFixture, *s07Record)
		want   string
	}{
		"the same token": {func(f *s07ReleaseFixture, c *s07Record) {
			c.repeat.olds[0].token.Token = testCurrentToken
		}, "would release it"},
		"no old token": {func(_ *s07ReleaseFixture, c *s07Record) { c.repeat.olds = nil },
			"no release token"},
		"another quarantine": {func(_ *s07ReleaseFixture, c *s07Record) { c.Generations = []string{"g9"} },
			"not one the second cut saw"},
		"the hold ended first": {func(_ *s07ReleaseFixture, c *s07Record) {
			c.EnforcedTime = nil
			past := time.Now().Add(-2 * time.Minute)
			c.FaultConfirmedAt = &past
		}, "while the second cut held"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newS07ReleaseFixture(t, releaseOn(""), nil)
			c := oldTokenCut(f)
			tc.change(f, c)
			stepOldToken(t, f, c)
			if o := c.repeat.OldTokens[0]; !strings.Contains(o.NotMade, tc.want) || o.SetAt != nil {
				t.Errorf("old token %+v, want not made with %q", o, tc.want)
			}
			if got := f.annotations(); len(got) != 0 {
				t.Errorf("annotations set %q", got)
			}
		})
	}
}

// The hold of a second cut lasts until the old token's step ended; the first
// cut's does not wait for it.
func TestS07OldTokenSettled(t *testing.T) {
	if !s07OldTokenSettled(&s07Record{}) {
		t.Error("a first cut waits for an old token")
	}
	c := &s07Record{repeat: &s07Repeat{}}
	if s07OldTokenSettled(c) {
		t.Error("a second cut ends its hold before the old token's step")
	}
	c.repeat.olds = []s07OldTry{{}, {}}
	c.repeat.OldTokens = []*s07OldToken{{QuarantineHeld: true}}
	if s07OldTokenSettled(c) {
		t.Error("a second cut ends its hold during the watch")
	}
	c.repeat.OldTokens[0].done = true
	if s07OldTokenSettled(c) {
		t.Error("a second cut ends its hold before the second token was tried")
	}
	c.repeat.OldTokens[0].QuarantineHeld = false
	if !s07OldTokenSettled(c) {
		t.Error("a second cut tries another token after the quarantine was not held")
	}
	c.repeat.OldTokens = []*s07OldToken{{QuarantineHeld: true, done: true}, {QuarantineHeld: true, done: true}}
	if !s07OldTokenSettled(c) {
		t.Error("a second cut holds after every token was tried")
	}
}

// The second cut writes its files, and names its keepalive container, apart
// from the first.
func TestS07SecondCutFiles(t *testing.T) {
	c := &s07Record{Variant: s07HigherRankIsolated, sub: s07RepeatDir}
	if got := s07File(c.dir(), "cut.json"); got != "S07/higher-rank-isolated/repeat/cut.json" {
		t.Errorf("second cut file %s", got)
	}
	if got := s07File((&s07Record{Variant: s07HigherRankIsolated}).dir(), "cut.json"); got !=
		"S07/higher-rank-isolated/cut.json" {
		t.Errorf("first cut file %s", got)
	}
	if s07RepeatKeepaliveContainer != "netsysctl-repeat" {
		t.Errorf("keepalive container %s", s07RepeatKeepaliveContainer)
	}
	rec := s07RepeatBase()
	if got := s07SessionClients(rec); !slices.Equal(got, []string{s07OpenClient}) {
		t.Errorf("clients before the second session was launched: %v", got)
	}
	rec.Repeat.launched = true
	if got := s07SessionClients(rec); !slices.Equal(got, []string{s07OpenClient, s07RepeatOpenClient}) {
		t.Errorf("clients %v", got)
	}
}

// A first cut that gives no ground for a second one leaves the second cut
// not made, with nothing started or cut, and S07 blocked.
func TestRepeatS07NotMadeAfterAnUnreleasedFirstCut(t *testing.T) {
	f := newS07ReleaseFixture(t, releaseOn(testCurrentToken), nil)
	rec := s07FirstCut()
	rec.ManualRelease, rec.ReleaseSeenAt = nil, nil
	rec.QuarantineInForce = string(databasev1alpha1.WriteQuarantinedMasterChanged)
	if still := f.h.repeatS07(context.Background(), rec); still != nil {
		t.Fatalf("still running %v", still)
	}
	rep := rec.Repeat
	if rep == nil || !strings.Contains(rep.NotMade, "-partition-manual-release") || rep.Cut != nil ||
		rep.launched || rep.StartedAt != nil {
		t.Fatalf("repeat %+v", rep)
	}
	if v := judgeS07(rec, testS07Limits); v.Result != evidence.Blocked ||
		!strings.Contains(v.Reason, "the second cut was not made") {
		t.Errorf("judged %s %q", v.Result, v.Reason)
	}
}

// The second cut waits for its starting state and is not made when a
// quarantine is still in force at the end of the wait.
func TestRepeatS07WaitsForTheStartingState(t *testing.T) {
	old := s07RepeatReadyBound
	s07RepeatReadyBound = 50 * time.Millisecond
	t.Cleanup(func() { s07RepeatReadyBound = old })
	f := newS07ReleaseFixture(t, releaseOn(testCurrentToken), nil)
	rec := s07FirstCut()
	f.h.repeatS07(context.Background(), rec)
	rep := rec.Repeat
	if rep == nil || rep.StartedAt == nil || rep.ReadyAt != nil || rep.Cut != nil || rep.launched ||
		!strings.Contains(rep.NotMade, "did not hold within 50ms: a write quarantine "+
			"(WriteQuarantinedMasterChanged, generation g1) is in force") {
		t.Fatalf("repeat %+v", rep)
	}
	if !slices.Equal(rep.EarlierGenerations, []string{"g1"}) || len(rep.olds) != 2 ||
		rep.olds[0].token.Token != testFirstConsumedToken {
		t.Errorf("earlier generations %v, old tokens %v", rep.EarlierGenerations, rep.olds)
	}
}

// A second cut that moved the master and was then not made compares the rows
// with the master read before the data check, not with the first cut's final
// master; without that reading the data check is not made.
func TestS07EndMasterAfterAMoveWithoutASecondCut(t *testing.T) {
	f := newS07ReleaseFixture(t, releaseOn(testCurrentToken), nil)
	rec := s07RepeatBase()
	c := rec.Repeat.Cut
	deleted := time.Now().UTC()
	c.FaultIssuedAt, c.SettledAt = nil, nil
	c.MasterMove = &s07MasterMove{From: m0, To: m2, DeletedAt: &deleted}
	rec.FinalMaster = m1
	if master, _, unknown := s07EndMaster(rec); master != "" || !strings.Contains(unknown, "were not read") {
		t.Errorf("before the roles were read: %q %q", master, unknown)
	}
	f.h.readS07EndRoles(context.Background(), rec)
	master, slaves, unknown := s07EndMaster(rec)
	if master != m0 || !slices.Equal(slaves, []string{m1, m2}) || unknown != "" {
		t.Errorf("end master %q %v %q, want the master the fixture's members report", master, slaves, unknown)
	}
	moved := s07RepeatBase()
	moved.Repeat.Cut.FaultIssuedAt, moved.Repeat.Cut.SettledAt = nil, nil
	moved.Repeat.Cut.MasterMove = &s07MasterMove{From: m0, DeletedAt: &deleted}
	moved.Repeat.EndRolesUnmet = "cubrid heartbeat status on ha-1 unread"
	if v := judgeS07(moved, testS07Limits); v.Result != evidence.Blocked {
		t.Errorf("judged %s %q", v.Result, v.Reason)
	}
}

// The second cut removes the earlier token it set as soon as its fault flow
// returned, before the settle wait.
func TestEndS07SecondCutRemovesTheOldTokenBeforeTheSettle(t *testing.T) {
	f := newS07ReleaseFixture(t, releaseOn(testCurrentToken), nil)
	c := oldTokenCut(f)
	f.h.stepS07OldToken(context.Background(), c, time.Now().UTC())
	if len(c.repeat.OldTokens) != 1 || c.repeat.OldTokens[0].SetAt == nil {
		t.Fatalf("the token was not set: %+v", c.repeat.OldTokens)
	}
	now := time.Now().UTC()
	f.h.endS07SecondCut(context.Background(), c, faults.Outcome{FaultIssuedAt: now, FaultConfirmed: true,
		CleanupSucceeded: true, Result: evidence.Pass,
		Timeline: []faults.Step{{T: now, Kind: "fault_removed"}}})
	removed, settled := -1, -1
	for i, step := range c.Observed {
		switch step.Kind {
		case "release_token_removed":
			removed = i
		case "settled":
			settled = i
		}
	}
	if removed < 0 || settled < 0 || removed > settled {
		t.Errorf("the token was removed at step %d and the cluster settled at step %d: %v", removed, settled,
			c.Observed)
	}
	if v := f.cluster(t).Annotations[releaseWriteQuarantineAnnotation]; v != "" {
		t.Errorf("the token is still set: %q", v)
	}
	if o := c.repeat.OldTokens[0]; !o.done || o.AnnotationRemovedByRun == nil {
		t.Errorf("old token %+v", o)
	}
}

// The scenario's cleanup is the first cut's and, once a second cut was
// injected, the second cut's too.
func TestS07CleanupSucceededCoversTheSecondCut(t *testing.T) {
	yes := true
	rec := s07RepeatBase()
	if got := s07CleanupSucceeded(rec, &yes); got == nil || !*got {
		t.Errorf("both removed: %v", got)
	}
	rec.Repeat.Cut.CleanupSucceeded = false
	if got := s07CleanupSucceeded(rec, &yes); got == nil || *got {
		t.Errorf("the second cut not removed: %v", got)
	}
	rec.Repeat.Cut.FaultIssuedAt = nil
	if got := s07CleanupSucceeded(rec, &yes); got == nil || !*got {
		t.Errorf("no second cut injected: %v", got)
	}
	if got := s07CleanupSucceeded(s07Base(false), nil); got != nil {
		t.Errorf("no first cut injected: %v", got)
	}
}
