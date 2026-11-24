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
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

// -partition-repeat tests, on the VM lab, the repeated failures of #115:
// once the first cut's quarantine was released, manually by the run or
// automatically by the Operator, and the read-write path reopened, the run
// cuts the member the variant picks off again. ADR-0005 section 8 gives each
// new quarantine a fresh generation and token, so the run records whether a
// new quarantine of a new generation closes the path again within
// quarantine_limit, whether a token of the first quarantine is ignored while
// the second one is in force, and whether any write is acknowledged after the
// second closure. The second cut has its own record, written under
// S07/<variant>/repeat/.

const (
	// s07RepeatDir is the directory of the second cut's files.
	s07RepeatDir = "repeat"
	// s07RepeatOpenClient holds one connection to the read-write Service
	// from before the second cut, as s07OpenClient does before the first.
	s07RepeatOpenClient = "s07open2"
	// s07RepeatKeepaliveContainer is the keepalive container of the second
	// cut: the first cut's may have ended, and its name cannot be used again.
	s07RepeatKeepaliveContainer = netfault.KeepaliveContainer + "-" + s07RepeatDir
	// The kinds of the earlier tokens the run sets during the second cut, and
	// the Operator's check each one reaches (decideManualRelease in
	// internal/controller/quarantine_release.go). The consumed token is the
	// one status.lastWriteQuarantineRelease.releaseToken still names
	// (api/v1alpha1 WriteQuarantineRelease.releaseToken): it reaches the
	// consumed-token check, which ignores the annotation and removes it. A
	// token of the first quarantine that no release consumed reaches the
	// check that the annotation is not the quarantine's current
	// status.writeQuarantine.releaseToken, which leaves it inert.
	s07OldConsumed     = "the token the first cut's manual release consumed"
	s07OldUnconsumed   = "a token of the first cut's quarantine that no release consumed"
	s07CheckConsumed   = "consumed token: status.lastWriteQuarantineRelease.releaseToken names it"
	s07CheckNotCurrent = "not the current token: status.writeQuarantine.releaseToken differs"
	// s07SecondCut starts what the verdict says about the second cut.
	s07SecondCut = "on the second cut: "
)

// The bounds of the second cut's own steps; tests shorten them.
var (
	// s07RepeatReadyBound bounds the wait, after the first cut's settle wait
	// and release, for the state the second cut starts from: no quarantine,
	// a ready -rw endpoint, the starting conditions, one master and the
	// slaves, and status naming the master.
	s07RepeatReadyBound = 5 * time.Minute
	// s07OldTokenSlack covers, beyond s07StaleTokenWindow for each earlier
	// token, the tokens' removal and the polls that end their watches.
	s07OldTokenSlack = time.Minute
)

// s07Repeat is what -partition-repeat did and saw. Times are on the run's
// clock.
type s07Repeat struct {
	// NotMade says why the run made no second cut; nothing was cut again
	// then.
	NotMade string `json:"notMade,omitempty"`
	// StartedAt is when the step started, after the first cut's settle wait
	// and release; ReadyAt when the state the second cut starts from held,
	// and Unready the last check that did not hold before it.
	StartedAt *time.Time `json:"startedAt,omitempty"`
	ReadyAt   *time.Time `json:"readySeenAt,omitempty"`
	Unready   string     `json:"unready,omitempty"`
	// MasterBefore is the master the second cut's member was picked
	// against, before any move of the master.
	MasterBefore string `json:"masterBeforeSecondCut,omitempty"`
	// EarlierGenerations are the quarantine generations the run saw before
	// the second cut: the first cut's and any seen while the master moved.
	EarlierGenerations []string `json:"earlierGenerations,omitempty"`
	// SessionStarted is set once the second established session had its
	// first operations acknowledged, at SessionStartedAt: after the second
	// cut's preparation, its move of the master included, as before the
	// first cut.
	SessionStarted   bool       `json:"secondSessionStarted"`
	SessionStartedAt *time.Time `json:"secondSessionStartedAt,omitempty"`
	// OldTokens are the earlier tokens the run set, one after another, while
	// the second quarantine was in force: the one the first release consumed,
	// when the run released, and one that no release consumed, when the run
	// saw one.
	OldTokens []*s07OldToken `json:"oldTokenReplays,omitempty"`
	// EndMaster and EndSlaves are the roles read just before the data check
	// when the second cut's master move started but the cut was not made, so
	// that the rows are compared with the master of then; EndRolesUnmet says
	// why they could not be read.
	EndMaster     string   `json:"masterBeforeDataCheck,omitempty"`
	EndSlaves     []string `json:"slavesBeforeDataCheck,omitempty"`
	EndRolesUnmet string   `json:"rolesBeforeDataCheckUnmet,omitempty"`
	// Cut is the second cut's own record.
	Cut *s07Record `json:"secondCut,omitempty"`
	// earlierTokens are the release tokens of the earlier quarantines, never
	// written to the evidence; olds are those the run sets during the second
	// cut, in order; launched is set once the second established session's
	// client was started.
	earlierTokens []s07SeenToken
	olds          []s07OldTry
	launched      bool
}

// s07OldTry is an earlier token the run sets during the second cut, with its
// kind and the Operator's check it reaches.
type s07OldTry struct {
	token       s07SeenToken
	kind, check string
}

// s07OldToken is the run's replay of an earlier quarantine's token during the
// second cut: which token, when it was set and against which quarantine, the
// readings of the watch after it, whether the second quarantine stayed in
// force with the read-write Brokers at zero, and the token's removal.
type s07OldToken struct {
	Kind        string `json:"kind,omitempty"`
	Check       string `json:"operatorCheck,omitempty"`
	TokenPrefix string `json:"tokenPrefix,omitempty"`
	Generation  string `json:"tokenGenerationID,omitempty"`
	// Against is the second quarantine's generation when the token was set.
	Against        string     `json:"quarantineGenerationID,omitempty"`
	SetAt          *time.Time `json:"setAt,omitempty"`
	SetError       string     `json:"setError,omitempty"`
	WatchedUntil   *time.Time `json:"watchedUntil,omitempty"`
	Readings       int        `json:"readings"`
	QuarantineHeld bool       `json:"quarantineHeldThroughout"`
	BrokersHeld    bool       `json:"rwBrokersHeldAtZeroThroughout"`
	// Refusals are the release refusals the Operator recorded meanwhile.
	Refusals []string `json:"releaseRefusals,omitempty"`
	// OtherRelease is a release of the second quarantine by another token, and
	// QuarantineGone the second quarantine no longer in force without a
	// release the run read; either ends the watch.
	OtherRelease   string   `json:"otherRelease,omitempty"`
	QuarantineGone string   `json:"quarantineGone,omitempty"`
	NeverEvents    []string `json:"neverEvents,omitempty"`
	// NotMade says why the run set no token.
	NotMade                string     `json:"notMade,omitempty"`
	AnnotationRemovedByRun *time.Time `json:"annotationRemovedByRun,omitempty"`
	AnnotationRemoveError  string     `json:"annotationRemoveError,omitempty"`
	// token is the token set, kept from before the patch so that one whose
	// patch timed out is still removed, and never written to the evidence;
	// done is set once the step ended, removed once its removal was made.
	token   string
	done    bool
	removed bool
}

// validateS07Repeat refuses -partition-repeat without a partition variant
// that quarantines, and with -partition-operator-restart: the restart
// replaces the manager Pod LAB01 recorded, which the second cut must name as
// the Operator it cuts off, and a run blocked by one of the two steps would
// hide what the other showed.
func (c HAConfig) validateS07Repeat() error {
	switch {
	case !c.PartitionRepeat:
		return nil
	case !c.partitionFault():
		return errors.New("-partition-repeat needs an S07 partition fault")
	case c.Fault == FaultPartitionSlaveIsolated:
		return errors.New("-partition-repeat cuts again after a released quarantine; " +
			FaultPartitionSlaveIsolated + " is the control, which must not quarantine")
	case c.PartitionOperatorRestart:
		return errors.New("-partition-repeat and -partition-operator-restart are separate runs: the restart " +
			"replaces the manager Pod that LAB01 recorded and the second cut cuts off")
	}
	return nil
}

// s07RepeatBounds is the time the second cut is allowed in the -timeout
// check with -partition-repeat: the wait for its starting state, the second
// established session's start, the move of the master for
// higher-rank-isolated, the shortened keepalive when asked for, the cut's
// preparation, hold with the old token's watch, removal and settle wait. The
// data check is made once and counted already.
func (c HAConfig) s07RepeatBounds() time.Duration {
	if !c.PartitionRepeat {
		return 0
	}
	bounds := s07RepeatReadyBound + s06ClientStart + s07PrepareBound + c.PartitionHold + s07ConfirmSlack +
		s07OldTokenHold() + s07CleanupBound + c.s07Settle()
	if c.Fault == FaultPartitionHigherRankIsolated {
		bounds += s07MoveBound
	}
	if c.PartitionKeepalive > 0 {
		bounds += s07KeepaliveBound
	}
	return bounds
}

// s07OldTokenHold is how much longer than -partition-hold the watches of the
// earlier tokens, two at most, may hold the second cut.
func s07OldTokenHold() time.Duration { return 2*s07StaleTokenWindow + s07OldTokenSlack }

// s07RepeatCut is the second cut's record, or nil.
func s07RepeatCut(rec *s07Record) *s07Record {
	if rec.Repeat == nil {
		return nil
	}
	return rec.Repeat.Cut
}

// s07SessionClients are the established-session clients the run started or
// may have started.
func s07SessionClients(rec *s07Record) []string {
	if rec.Repeat != nil && rec.Repeat.launched {
		return []string{s07OpenClient, s07RepeatOpenClient}
	}
	return []string{s07OpenClient}
}

// s07RepeatUnready says why the first cut's record gives no ground for a
// second cut, or "": the first cut must have settled, quarantined, and had
// that quarantine released, by the run's accepted manual release when one
// was asked for, without a rule already broken. A quarantine only a person
// may release that is still in force names -partition-manual-release.
func s07RepeatUnready(rec *s07Record) string {
	r := rec.ManualRelease
	switch {
	case rec.SettledAt == nil:
		return "the cluster did not settle after the first cut"
	case rec.QuarantineSeenAt == nil:
		return "no quarantine was seen during the first cut, so a second one could not be told apart as new"
	case r != nil && r.AcceptedSeenAt == nil:
		return "the run's manual release of the first quarantine was not accepted: " +
			cmpOr(r.NotMade, cmpOr(r.Unmet, "unknown"))
	case rec.ReleaseSeenAt == nil && manualOnly(rec.QuarantineInForce):
		return "the first cut's " + rec.QuarantineInForce + " quarantine is still in force; only a person may " +
			"release it, which the run does with -partition-manual-release"
	case rec.ReleaseSeenAt == nil:
		return "the first cut's quarantine was not seen released"
	case len(s07NeverEvents(rec)) > 0:
		return "the first cut already broke a rule: " + strings.Join(s07NeverEvents(rec), "; ")
	case s07Broken(rec) != "":
		return "the first cut already broke a rule: " + s07Broken(rec)
	}
	return ""
}

// s07EarlierTokens are the first cut's release tokens, with the token its
// manual release consumed among them, and the tokens the run sets during the
// second cut: the consumed one when the run released, then the newest one no
// release consumed, when there is one.
func s07EarlierTokens(rec *s07Record) ([]s07SeenToken, []s07OldTry) {
	tokens := slices.Clone(rec.tokens)
	var olds []s07OldTry
	consumed := ""
	if r := rec.ManualRelease; r != nil && r.AcceptedSeenAt != nil && r.currentToken != "" {
		consumed = r.currentToken
		t := s07SeenToken{Generation: r.GenerationID, Token: consumed}
		if !slices.Contains(tokens, t) {
			tokens = append(tokens, t)
		}
		olds = append(olds, s07OldTry{token: t, kind: s07OldConsumed, check: s07CheckConsumed})
	}
	for _, t := range slices.Backward(tokens) {
		if t.Token != "" && t.Token != consumed {
			olds = append(olds, s07OldTry{token: t, kind: s07OldUnconsumed, check: s07CheckNotCurrent})
			break
		}
	}
	return tokens, olds
}

// s07MoveGenerations are the quarantine generations seen while a record's
// master was moved.
func s07MoveGenerations(rec *s07Record) []string {
	move := rec.MasterMove
	if move == nil {
		return nil
	}
	var generations []string
	if move.Quarantine != nil {
		generations = append(generations, move.Quarantine.GenerationID)
	}
	return append(generations, move.LaterGenerations...)
}

// repeatS07 makes the second cut of -partition-repeat once the first cut's
// record allows it (s07RepeatUnready): it waits for the state the second cut
// starts from, builds a new record with the master and slaves then, prepares
// the cut as the first one is (the move of the master and the shortened
// keepalive included) and only then starts the second established session
// (prepareS07Second), and runs the cut through the same steps as the first,
// with the old token's watch in its hold; then it settles the cluster. It
// returns the fault flow's steps that were still running after their limit.
func (h *haRun) repeatS07(ctx context.Context, rec *s07Record) []string {
	rep := &s07Repeat{}
	rec.Repeat = rep
	notMade := func(format string, args ...any) []string {
		rep.NotMade = fmt.Sprintf(format, args...)
		rec.note("repeat_not_made", rep.NotMade)
		return nil
	}
	if err := ctx.Err(); err != nil {
		return notMade("the run ended before the second cut: %v", err)
	}
	if why := s07RepeatUnready(rec); why != "" {
		return notMade("%s", why)
	}
	started := time.Now().UTC()
	rep.StartedAt = &started
	rep.EarlierGenerations = append(slices.Clone(rec.Generations), s07MoveGenerations(rec)...)
	rep.earlierTokens, rep.olds = s07EarlierTokens(rec)
	state, members, err := h.waitS07RepeatReady(ctx, rec, rep)
	if err != nil {
		return notMade("%v", err)
	}
	rep.MasterBefore = state.Master
	c, p, err := h.prepareS07Second(ctx, rec, rep, state, members)
	if p != nil {
		defer func() {
			r := p.Record()
			c.mu.Lock()
			c.Partition = &r
			c.mu.Unlock()
		}()
	}
	if err != nil {
		return notMade("%v", err)
	}
	defer h.endS07OldToken(ctx, c)
	stopWatch := h.removeOnEnd(ctx, c, p)
	defer stopWatch()
	outcome := faults.Run(ctx, faults.Scenario{
		Fault: &s07Cut{Partition: p, h: h, rec: c},
		Limits: faults.Limits{Confirm: s07PrepareBound, Outcome: h.cfg.PartitionHold + s07ConfirmSlack +
			s07OldTokenHold(), Cleanup: s07CleanupBound, Poll: h.cfg.Poll},
		Before: func(ctx context.Context) error { return h.beforeS07(ctx, c, p) },
		Check: func(ctx context.Context) (bool, error) {
			done, err := h.whileCut(ctx, c, p)
			if ctx.Err() != nil {
				return false, nil
			}
			return done, err
		},
	})
	stopWatch()
	return h.endS07SecondCut(ctx, c, outcome)
}

// prepareS07Second builds the second cut's record and prepares its cut as
// the first cut is prepared: the member picked against the master of now, the
// move of the master off the first member when needed, and the shortened
// keepalive when asked for. Only then, as before the first cut, does it start
// the second held session and wait for its first acknowledgements: a session
// opened before the move would hold a connection to the master the move
// replaces, and end before the cut. It returns the partition once one was
// built, also with an error.
func (h *haRun) prepareS07Second(ctx context.Context, rec *s07Record, rep *s07Repeat, state *startingState,
	members []imageIdentity) (*s07Record, *netfault.Partition, error) {
	c := &s07Record{Variant: rec.Variant, sub: s07RepeatDir, repeat: rep, Master: state.Master,
		slaves: slices.Clone(state.Slaves), MembersBefore: members, MembersAfter: []imageIdentity{},
		Sweeps: []s07Sweep{}, Conditions: []s07Condition{}, Path: []s07Path{}, Probes: []s07Probe{},
		Observed: []faults.Step{}, ClockOffsets: rec.ClockOffsets}
	rep.Cut = c
	rec.note("repeat_started", "master "+state.Master)
	p, err := h.prepareS07(ctx, c)
	if err != nil {
		return c, p, fmt.Errorf("prepare the second cut: %w", err)
	}
	rep.launched = true
	if err := h.startBackgroundClientWith(ctx, s07RepeatOpenClient, "APP_RECONNECT_POLICY=stop-after-error "+
		"APP_CONNECTION_DIAGNOSTICS=/tmp/"+s07RepeatOpenClient+".diag.jsonl "); err != nil {
		return c, p, fmt.Errorf("start the second established-session client: %w", err)
	}
	now := time.Now().UTC()
	rep.SessionStarted, rep.SessionStartedAt = true, &now
	c.note("second_session_started", s07RepeatOpenClient)
	return c, p, nil
}

// endS07SecondCut ends the second cut once its fault flow returned: it first
// removes any earlier token the run set, so that none is left through the
// settle wait (the deferred removal in repeatS07 only backs this up), then
// records the outcome and the timeline, and settles the cluster when the cut
// was made and removed. It returns the steps still running after their limit.
func (h *haRun) endS07SecondCut(ctx context.Context, c *s07Record, outcome faults.Outcome) []string {
	h.endS07OldToken(ctx, c)
	h.recordS07Outcome(c, outcome)
	if err := h.writeTimeline(s07File(c.dir(), "timeline.jsonl"), outcome.Timeline); err != nil {
		c.note("repeat_timeline_unwritten", err.Error())
	}
	if len(outcome.StillRunning) > 0 {
		c.mu.Lock()
		c.StillRunning = outcome.StillRunning
		c.mu.Unlock()
		return outcome.StillRunning
	}
	if outcome.CleanupSucceeded && !outcome.FaultIssuedAt.IsZero() {
		h.settleS07(ctx, c)
	}
	return nil
}

// waitS07RepeatReady waits, within s07RepeatReadyBound, for the state the
// second cut starts from while it keeps observing the first cut's record: no
// write quarantine, a ready -rw endpoint in the run's last reading, the
// starting conditions, one master with the other members its slaves, and
// status naming that master as current and last resolved primary. It returns
// the roles and the members' identities then.
func (h *haRun) waitS07RepeatReady(ctx context.Context, rec *s07Record, rep *s07Repeat) (*startingState,
	[]imageIdentity, error) {
	deadline := time.Now().Add(s07RepeatReadyBound)
	for {
		now := time.Now().UTC()
		h.observeS07Release(ctx, rec, now)
		state, members, unmet, err := h.s07RepeatUnmet(ctx, rec)
		if err != nil {
			return nil, nil, err
		}
		if unmet == "" {
			rep.ReadyAt, rep.Unready = &now, ""
			return state, members, nil
		}
		rep.Unready = unmet
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("the state the second cut starts from did not hold within %s: %s",
				s07RepeatReadyBound, unmet)
		}
		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("the run ended while it waited for the second cut's starting state: %w",
				ctx.Err())
		case <-time.After(h.cfg.Poll):
		}
	}
}

// s07RepeatUnmet says what of the second cut's starting state does not hold
// yet, or returns the roles and the members' identities once all does.
func (h *haRun) s07RepeatUnmet(ctx context.Context, rec *s07Record) (*startingState, []imageIdentity, string,
	error) {
	cluster, err := h.s07OwnCluster(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	if q := cluster.Status.WriteQuarantine; q != nil {
		return nil, nil, fmt.Sprintf("a write quarantine (%s, generation %s) is in force", q.Reason,
			q.GenerationID), nil
	}
	if p := s07LastPath(rec); p.ReadyEndpoints <= 0 {
		return nil, nil, fmt.Sprintf("the -rw Service has no ready endpoint (%d desired replica(s), %d Ready "+
			"Broker Pod(s))", p.Desired, p.ReadyPods), nil
	}
	if unmet, err := clusterReady(cluster); err != nil || unmet != "" {
		return nil, nil, unmet, err
	}
	state, unmet, err := h.roles(ctx)
	if err != nil || unmet != "" {
		return nil, nil, unmet, err
	}
	if s := cluster.Status; s.CurrentPrimary != state.Master || s.LastResolvedPrimary != state.Master {
		return nil, nil, fmt.Sprintf("CUBRID reports the master %s, status.currentPrimary %q and "+
			"lastResolvedPrimary %q", state.Master, s.CurrentPrimary, s.LastResolvedPrimary), nil
	}
	members, unmet, err := h.memberIdentities(ctx, cluster)
	if err != nil || unmet != "" {
		return nil, nil, unmet, err
	}
	return state, members, "", nil
}

// stepS07OldToken advances the earlier tokens' replay on a poll while the
// second cut holds: it sets the first token once the second quarantine is
// enforced with the read-write path at zero, watches after it, sets the next
// one once a watch ended with the quarantine held, and gives up once the hold
// ended before the first one was due.
func (h *haRun) stepS07OldToken(ctx context.Context, c *s07Record, now time.Time) {
	rep := c.repeat
	n := len(rep.OldTokens)
	switch {
	case n > 0 && !rep.OldTokens[n-1].done:
		h.watchS07OldToken(ctx, c, rep.OldTokens[n-1], now)
	case s07OldTokenSettled(c):
	case n > 0 || s07RestartDue(c):
		h.setS07OldToken(ctx, c)
	case now.Sub(*c.FaultConfirmedAt) >= h.cfg.PartitionHold:
		o := &s07OldToken{NotMade: "no quarantine was seen enforced with the read-write path closed while the " +
			"second cut held for -partition-hold", done: true}
		rep.OldTokens = append(rep.OldTokens, o)
		c.note("old_token_not_made", o.NotMade)
	}
}

// setS07OldToken sets the next earlier token on the run's own cluster while
// the second quarantine is in force, unless that quarantine is not one the
// second cut saw or its own token is the earlier one, which would release it.
// The token is kept before the patch, so that a patch whose answer was lost
// is still removed.
func (h *haRun) setS07OldToken(ctx context.Context, c *s07Record) {
	rep := c.repeat
	o := &s07OldToken{}
	rep.OldTokens = append(rep.OldTokens, o)
	notMade := func(format string, args ...any) {
		o.NotMade, o.done = fmt.Sprintf(format, args...), true
		c.note("old_token_not_made", o.NotMade)
	}
	n := len(rep.OldTokens) - 1
	if n >= len(rep.olds) {
		notMade("no release token of the first cut's quarantine was seen")
		return
	}
	try := rep.olds[n]
	o.Kind, o.Check, o.Generation = try.kind, try.check, try.token.Generation
	o.TokenPrefix = s07TokenPrefix(try.token.Token)
	cluster, err := h.s07OwnCluster(ctx)
	if err != nil {
		notMade("%v", err)
		return
	}
	q := cluster.Status.WriteQuarantine
	switch {
	case q == nil || !slices.Contains(c.Generations, q.GenerationID):
		notMade("the quarantine in force is not one the second cut saw")
		return
	case q.ReleaseToken == try.token.Token:
		notMade("the second quarantine's own token is the earlier one; setting it would release it")
		return
	}
	o.Against, o.token = q.GenerationID, try.token.Token
	if err := h.setS07ReleaseToken(ctx, o.token); err != nil {
		o.SetError = err.Error()
		notMade("the earlier token could not be set: %v", err)
		h.removeS07OldToken(ctx, c, o)
		return
	}
	set := time.Now().UTC()
	o.SetAt, o.QuarantineHeld, o.BrokersHeld = &set, true, true
	c.note("old_token_set", o.Kind)
}

// watchS07OldToken reads the cluster once after an earlier token was set: a
// release of the second quarantine on it, or read-write Brokers above zero
// while that quarantine is in force, is a never-event; a release by another
// token or the quarantine gone ends the watch; so does s07StaleTokenWindow
// after the token was set. The token is removed when the watch ends.
func (h *haRun) watchS07OldToken(ctx context.Context, c *s07Record, o *s07OldToken, now time.Time) {
	if cluster, err := h.s07OwnCluster(ctx); err == nil {
		o.Readings++
		q := cluster.Status.WriteQuarantine
		inForce := q != nil && q.GenerationID == o.Against
		if inForce && q.ReleaseRefusal != "" && !slices.Contains(o.Refusals, q.ReleaseRefusal) {
			o.Refusals = append(o.Refusals, q.ReleaseRefusal)
		}
		rel := cluster.Status.LastWriteQuarantineRelease
		switch {
		case rel != nil && rel.GenerationID == o.Against && rel.ReleaseToken == o.token:
			o.QuarantineHeld = false
			o.NeverEvents = append(o.NeverEvents, fmt.Sprintf(s07SecondCut+"the new quarantine (generation %s) "+
				"was released (%s) at %s on %s (%s), a token of generation %s", o.Against, rel.Method,
				now.Format(time.RFC3339), o.Kind, o.TokenPrefix, o.Generation))
		case rel != nil && rel.GenerationID == o.Against:
			o.QuarantineHeld = false
			o.OtherRelease = fmt.Sprintf("released %s at %s by a token the run did not set (%s)", rel.Method,
				now.Format(time.RFC3339), cmpOr(s07TokenPrefix(rel.ReleaseToken), "none"))
		case !inForce:
			o.QuarantineHeld = false
			o.QuarantineGone = fmt.Sprintf("at %s the quarantine of generation %s was no longer in force, and no "+
				"release of it was read", now.Format(time.RFC3339), o.Against)
		}
		if p := s07LastPath(c); inForce && o.BrokersHeld && (p.Desired > 0 || p.ReadyPods > 0 || p.ActivePods > 0) {
			o.BrokersHeld = false
			o.NeverEvents = append(o.NeverEvents, fmt.Sprintf(s07SecondCut+"the read-write Brokers were at %d "+
				"desired replica(s), %d Broker Pod(s) not terminating and %d Ready at %s while the new quarantine was "+
				"in force and only %s was set", p.Desired, p.ActivePods, p.ReadyPods, now.Format(time.RFC3339), o.Kind))
		}
	}
	if !o.QuarantineHeld || !now.Before(o.SetAt.Add(s07StaleTokenWindow)) || ctx.Err() != nil {
		o.WatchedUntil, o.done = &now, true
		h.removeS07OldToken(ctx, c, o)
	}
}

// endS07OldToken ends every earlier token's step and removes each token the
// run set, unless the Operator already removed it.
func (h *haRun) endS07OldToken(ctx context.Context, c *s07Record) {
	for _, o := range c.repeat.OldTokens {
		o.done = true
		h.removeS07OldToken(ctx, c, o)
	}
}

// removeS07OldToken removes an earlier token the run set when the annotation
// still names it, until an attempt did not fail; it runs within
// s07RemoveTokenBound, also after the run's context ended.
func (h *haRun) removeS07OldToken(ctx context.Context, c *s07Record, o *s07OldToken) {
	if o.token == "" || o.removed {
		return
	}
	r := &s07ManualRelease{lastSet: o.token}
	h.removeS07ReleaseToken(ctx, c, r)
	o.AnnotationRemovedByRun, o.AnnotationRemoveError = r.AnnotationRemovedByRun, r.AnnotationRemoveError
	o.removed = r.AnnotationRemoveError == ""
}

// s07OldTokenSettled reports whether the hold may end as far as the earlier
// tokens are concerned: the record is no second cut's, or the last step
// ended and no other is due: every token was tried, or the last was not set,
// or its watch did not end with the quarantine held.
func s07OldTokenSettled(rec *s07Record) bool {
	if rec.repeat == nil {
		return true
	}
	olds := rec.repeat.OldTokens
	if len(olds) == 0 {
		return false
	}
	last := olds[len(olds)-1]
	return last.done && (len(olds) >= len(rec.repeat.olds) || last.NotMade != "" || !last.QuarantineHeld ||
		len(last.NeverEvents) > 0)
}

// s07OldTokenWatched reports whether an earlier token's watch ran its whole
// window.
func s07OldTokenWatched(o *s07OldToken) bool {
	return o.SetAt != nil && o.WatchedUntil != nil && !o.WatchedUntil.Before(o.SetAt.Add(s07StaleTokenWindow))
}

// s07SplitProbes splits the new-connection writes at the second cut's start:
// those started before it are the first cut's, the others the second's.
func s07SplitProbes(probes []s07Probe, at time.Time) (first, second []s07Probe) {
	first, second = []s07Probe{}, []s07Probe{}
	for _, p := range probes {
		if p.StartedAt.Before(at) {
			first = append(first, p)
		} else {
			second = append(second, p)
		}
	}
	return first, second
}

// s07EndMaster is the master and slaves the run ended with: the second cut's
// when it settled; the roles read before the data check when the second cut's
// master move started and the cut was not made; the first cut's otherwise. A
// master that could not be known is "", with the reason.
func s07EndMaster(rec *s07Record) (string, []string, string) {
	c := s07RepeatCut(rec)
	switch {
	case c != nil && c.SettledAt != nil:
		return c.FinalMaster, c.FinalSlaves, ""
	case c != nil && c.FaultIssuedAt == nil && c.MasterMove != nil && c.MasterMove.DeletedAt != nil:
		if rec.Repeat.EndMaster == "" {
			return "", nil, "the second cut moved the master and was not made, and the roles before the data " +
				"check were not read: " + cmpOr(rec.Repeat.EndRolesUnmet, "unknown")
		}
		return rec.Repeat.EndMaster, rec.Repeat.EndSlaves, ""
	}
	return rec.FinalMaster, rec.FinalSlaves, ""
}

// readS07EndRoles reads the roles before the data check when the second
// cut's master move started and the cut was not made: the first cut's final
// master may not be the master any more.
func (h *haRun) readS07EndRoles(ctx context.Context, rec *s07Record) {
	c := s07RepeatCut(rec)
	if c == nil || c.FaultIssuedAt != nil || c.MasterMove == nil || c.MasterMove.DeletedAt == nil {
		return
	}
	state, unmet, err := h.roles(ctx)
	switch {
	case err != nil:
		rec.Repeat.EndRolesUnmet = err.Error()
	case unmet != "":
		rec.Repeat.EndRolesUnmet = unmet
	default:
		rec.Repeat.EndMaster, rec.Repeat.EndSlaves = state.Master, state.Slaves
	}
}

// s07CleanupSucceeded is the scenario's cleanup: the first cut's, and also the
// second cut's when one was injected, with every earlier token the run set
// removed or found gone.
func s07CleanupSucceeded(rec *s07Record, first *bool) *bool {
	c := s07RepeatCut(rec)
	if first == nil || c == nil || c.FaultIssuedAt == nil {
		return first
	}
	both := *first && c.CleanupSucceeded
	for _, o := range rec.Repeat.OldTokens {
		both = both && o.AnnotationRemoveError == ""
	}
	return &both
}

// s07ReusedGeneration is a generation of the second cut's quarantines that an
// earlier quarantine had, or "".
func s07ReusedGeneration(rep *s07Repeat) string {
	c := rep.Cut
	earlier := append(slices.Clone(rep.EarlierGenerations), s07MoveGenerations(c)...)
	for _, g := range c.Generations {
		if slices.Contains(earlier, g) {
			return g
		}
	}
	return ""
}

// s07ReusedToken reports whether a release token of the second cut's
// quarantines is one of an earlier quarantine.
func s07ReusedToken(rep *s07Repeat) bool {
	for _, t := range rep.Cut.tokens {
		if slices.ContainsFunc(rep.earlierTokens, func(e s07SeenToken) bool { return e.Token == t.Token }) {
			return true
		}
	}
	return false
}

// s07RepeatNeverEvents are the second cut's never-events: those of its own
// record, a new quarantine with an earlier generation or token, and the old
// token's watch.
func s07RepeatNeverEvents(rec *s07Record) []string {
	rep := rec.Repeat
	if rep == nil || rep.Cut == nil {
		return nil
	}
	var never []string
	for _, n := range s07NeverEvents(rep.Cut) {
		never = append(never, s07SecondCut+n)
	}
	if g := s07ReusedGeneration(rep); g != "" {
		never = append(never, fmt.Sprintf(s07SecondCut+"the quarantine has generation %s, which an earlier "+
			"quarantine had; ADR-0005 section 8 gives each new quarantine a fresh generation", g))
	}
	if s07ReusedToken(rep) {
		never = append(never, s07SecondCut+"the quarantine's release token is one an earlier quarantine had")
	}
	for _, o := range rep.OldTokens {
		never = append(never, o.NeverEvents...)
	}
	return never
}

// s07RepeatBroken is the first expectation the second cut breaks, or "".
func s07RepeatBroken(rec *s07Record) string {
	if c := s07RepeatCut(rec); c != nil {
		if broken := s07Broken(c); broken != "" {
			return s07SecondCut + broken
		}
	}
	return ""
}

// s07RepeatBlocked names what keeps the second cut from being judged with
// -partition-repeat: a second cut not reached or not made, what keeps the cut
// itself from being judged, and an old token that was not set or whose watch
// did not show the quarantine held.
func s07RepeatBlocked(rec *s07Record) []string {
	if !rec.RepeatAsked {
		return nil
	}
	rep := rec.Repeat
	switch {
	case rep == nil:
		return []string{"-partition-repeat was given, and the run did not reach the second cut"}
	case rep.NotMade != "":
		return []string{"the second cut was not made: " + rep.NotMade}
	case rep.Cut == nil:
		return []string{"the second cut was not made"}
	}
	var blocked []string
	for _, b := range s07CutBlocked(rep.Cut) {
		blocked = append(blocked, s07SecondCut+b)
	}
	if len(rep.OldTokens) == 0 {
		return append(blocked, s07SecondCut+"no token of the first quarantine was tried against the new one")
	}
	for _, o := range rep.OldTokens {
		if b := s07OldTokenBlocked(o); b != "" {
			blocked = append(blocked, s07SecondCut+b)
		}
	}
	if last := rep.OldTokens[len(rep.OldTokens)-1]; last.done && last.QuarantineHeld && last.NotMade == "" &&
		len(rep.OldTokens) < len(rep.olds) {
		blocked = append(blocked, fmt.Sprintf(s07SecondCut+"%d of the %d tokens of the first quarantine were tried",
			len(rep.OldTokens), len(rep.olds)))
	}
	return blocked
}

// s07OldTokenBlocked says why an earlier token's replay shows nothing, or "".
func s07OldTokenBlocked(o *s07OldToken) string {
	switch {
	case o.NotMade != "":
		return "no token of the first quarantine was tried against the new one: " + o.NotMade
	case !o.done:
		return "the watch after " + o.Kind + " was still running when the run was judged"
	case len(o.NeverEvents) > 0:
		return ""
	case o.OtherRelease != "":
		return "the new quarantine was " + o.OtherRelease + " during the watch after " + o.Kind
	case o.QuarantineGone != "" && s07OldTokenWatched(o):
		return fmt.Sprintf("the new quarantine was gone at the last reading of the watch after %s, at the end of "+
			"its %s window, so whether that token released it is unknown: %s", o.Kind, s07StaleTokenWindow,
			o.QuarantineGone)
	case o.QuarantineGone != "":
		return fmt.Sprintf("the watch after %s ended before its %s window because the new quarantine was gone: %s",
			o.Kind, s07StaleTokenWindow, o.QuarantineGone)
	case o.Readings < 2:
		return fmt.Sprintf("the cluster was read %d time(s) after %s was set, and the run needs two", o.Readings,
			o.Kind)
	case !s07OldTokenWatched(o):
		return fmt.Sprintf("the watch after %s ended at %s, before its %s window", o.Kind,
			s07Seen(o.WatchedUntil), s07StaleTokenWindow)
	}
	return ""
}

// s07SecondBounds are the second cut's bounds: quarantine_limit from its
// trigger window until new connections stopped, and partition_duration for
// its hold. They are judged only once nothing blocks the run, so a cut that
// was not confirmed or held has none.
func s07SecondBounds(rec *s07Record, limits s07Limits) []faults.Bound {
	c := s07RepeatCut(rec)
	if c == nil || c.FaultConfirmedAt == nil || c.HoldEndedAt == nil || c.TriggerWindowStart == nil {
		return nil
	}
	quarantine := faults.Bound{Limit: faults.Limit{Name: quarantineLimitName, Value: limits.Quarantine}}
	if c.NewConnectionsStoppedAt == nil {
		quarantine.Unmeasured = true
	} else {
		quarantine.Measured = c.NewConnectionsStoppedAt.Sub(*c.TriggerWindowStart)
	}
	partition := faults.Bound{Limit: faults.Limit{Name: partitionDurationName, Value: limits.Partition},
		AtLeast: true, Measured: c.HoldEndedAt.Sub(*c.FaultConfirmedAt)}
	return []faults.Bound{quarantine, partition}
}

// s07Exceeded is faults.ExceededOnly over both cuts' bounds, with a limit
// the second cut exceeded named as the second cut's.
func s07Exceeded(first, second []faults.Bound) map[string]string {
	if faults.ExceededOnly(append(slices.Clone(first), second...)...) == nil {
		return nil
	}
	exceeded := faults.ExceededOnly(first...)
	if exceeded == nil {
		exceeded = map[string]string{}
	}
	for _, b := range second {
		if b.AtLeast || b.Measured <= b.Limit.Value {
			continue
		}
		value := fmt.Sprintf("second cut: %s > %s", b.Measured, b.Limit.Value)
		if have, ok := exceeded[b.Limit.Name]; ok {
			value = have + "; " + value
		}
		exceeded[b.Limit.Name] = value
	}
	return exceeded
}

// s07RepeatNote is what a run adds to its reason about the second cut, or "".
func s07RepeatNote(rec *s07Record) string {
	rep := rec.Repeat
	if rep == nil || rep.Cut == nil || rep.Cut.FaultIssuedAt == nil {
		return ""
	}
	c := rep.Cut
	var notes []string
	moved := ""
	if c.MasterMove != nil && c.MasterMove.To != "" {
		moved = fmt.Sprintf(", after the master moved off %s to %s", c.MasterMove.From, c.MasterMove.To)
	}
	notes = append(notes, fmt.Sprintf("with %s cut off again (master %s%s)", cmpOr(c.Target.Pod, "unknown"),
		rep.MasterBefore, moved))
	if len(c.Revisions) > 0 {
		notes = append(notes, fmt.Sprintf("a %s quarantine of generation %s started (earlier: %s)",
			c.Revisions[0].Reason, c.Revisions[0].GenerationID, cmpOr(strings.Join(rep.EarlierGenerations, ", "),
				"none")))
	}
	if c.TriggerWindowStart != nil && c.NewConnectionsStoppedAt != nil {
		notes = append(notes, fmt.Sprintf("new connections stopped %s after the trigger window",
			c.NewConnectionsStoppedAt.Sub(*c.TriggerWindowStart).Round(time.Millisecond)))
	}
	for _, o := range rep.OldTokens {
		if o.SetAt != nil {
			notes = append(notes, fmt.Sprintf("%s was read %d time(s) with the quarantine held %t and the "+
				"read-write Brokers at zero %t", o.Kind, o.Readings, o.QuarantineHeld, o.BrokersHeld))
		}
	}
	if c.SettledAt != nil {
		notes = append(notes, fmt.Sprintf("the cluster settled on master %s with %s", c.FinalMaster,
			cmpOr(c.QuarantineAtEnd, "no quarantine left")))
	}
	return "; second cut: " + strings.Join(notes, ", ")
}

// s07RepeatMeasurements adds the second cut to the measurements: whether it
// was made, its own measurements under secondCut, whether its quarantine is
// of a new generation, and whether the earlier token was ignored, unknown
// where the run did not show it.
func s07RepeatMeasurements(rec *s07Record, m map[string]any) {
	if !rec.RepeatAsked {
		return
	}
	rep := rec.Repeat
	m["repeat"] = "not reached"
	m["secondQuarantineNewGeneration"], m["oldTokenIgnored"] = evidence.Unknown, evidence.Unknown
	switch {
	case rep == nil:
		return
	case rep.NotMade != "" || rep.Cut == nil:
		m["repeat"] = s07NotMade
		return
	}
	m["repeat"] = "made"
	c := rep.Cut
	second := s07Measurements(c)
	second["target"] = cmpOr(c.Target.Pod, evidence.Unknown)
	second["masterBefore"] = cmpOr(rep.MasterBefore, evidence.Unknown)
	second["masterMoved"] = "no"
	if c.MasterMove != nil {
		second["masterMoved"] = c.MasterMove.From + " -> " + cmpOr(c.MasterMove.To, evidence.Unknown)
	}
	m["secondCut"] = second
	if len(c.Generations) > 0 {
		m["secondQuarantineNewGeneration"] = strconv.FormatBool(s07ReusedGeneration(rep) == "" &&
			!s07ReusedToken(rep))
	}
	m["oldTokenIgnored"] = s07OldTokensIgnored(rep.OldTokens)
}

// s07OldTokensIgnored is whether every earlier token the run set was ignored:
// "unknown" unless each was set and watched for its whole window with at
// least two readings, no release by another token and, unless a release on
// it was read, the quarantine not gone without a release the run read.
func s07OldTokensIgnored(olds []*s07OldToken) string {
	if len(olds) == 0 {
		return evidence.Unknown
	}
	ignored := true
	for _, o := range olds {
		if !o.done || o.Readings < 2 || o.OtherRelease != "" ||
			len(o.NeverEvents) == 0 && (!s07OldTokenWatched(o) || o.QuarantineGone != "") {
			return evidence.Unknown
		}
		ignored = ignored && o.QuarantineHeld && o.BrokersHeld && len(o.NeverEvents) == 0
	}
	return strconv.FormatBool(ignored)
}
