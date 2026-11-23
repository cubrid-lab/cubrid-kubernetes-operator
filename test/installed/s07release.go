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
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The bounds of the run's own manual release (-partition-manual-release);
// tests shorten them. s07StaleTokenWindow is how long the run watches the
// quarantine after it set a stale token: several of the Operator's 3-second
// observations of a quarantine in force. s07ReleaseBound bounds the wait,
// from setting the current token, for the release and the read-write path's
// return; s07ReleaseWriteWindow is how long the run keeps observing once the
// -rw endpoints are ready, so that a write through a new connection can be
// acknowledged before the clients stop.
var (
	s07StaleTokenWindow   = 30 * time.Second
	s07ReleaseBound       = 5 * time.Minute
	s07ReleaseWriteWindow = 30 * time.Second
	// s07ReleaseSlack covers the release's steps that have no bound of their
	// own: the reads of the clients' histories, the sweep before the current
	// token, the removal of an unconsumed token, the Events read and the last
	// poll of each watch running over its bound.
	s07ReleaseSlack = time.Minute
	// s07RemoveTokenBound bounds the removal of an unconsumed token, which is
	// made also after the run's context ended.
	s07RemoveTokenBound = 10 * time.Second
)

const (
	// releaseWriteQuarantineAnnotation asks the Operator for the manual
	// release of the write quarantine in force; its value must be the
	// quarantine's current status.writeQuarantine.releaseToken
	// (docs/observability.md, "Manual release").
	releaseWriteQuarantineAnnotation = "database.cubrid.io/release-write-quarantine"
	// The stale tokens the run sets before the current one.
	s07StaleEarlier = "an earlier token of the quarantine"
	s07StaleAltered = "the current token altered"
	// s07ServerActive is CUBRID's state of a server process that serves as
	// master.
	s07ServerActive = "registered_and_active"
	// s07ReleaseEventPrefix starts the reasons of the Operator's Events about
	// the release annotation.
	s07ReleaseEventPrefix = "WriteQuarantineRelease"
)

// s07ActiveServers is a sweep after the heal and the members whose server
// answered registered_and_active in it, in name order.
type s07ActiveServers struct {
	T       time.Time `json:"t"`
	Members []string  `json:"members"`
}

// s07StaleTokenRecord is the stale token the run set before the current one:
// which kind, its first characters, when it was set and until when the run
// watched, how many times the run read the cluster then, and whether the
// quarantine stayed in force and the read-write Brokers at zero through
// every reading.
type s07StaleTokenRecord struct {
	Kind           string     `json:"kind"`
	TokenPrefix    string     `json:"tokenPrefix"`
	SetAt          *time.Time `json:"setAt,omitempty"`
	SetError       string     `json:"setError,omitempty"`
	WatchedUntil   *time.Time `json:"watchedUntil,omitempty"`
	Readings       int        `json:"readings"`
	QuarantineHeld bool       `json:"quarantineHeldThroughout"`
	BrokersHeld    bool       `json:"rwBrokersHeldAtZeroThroughout"`
	// PathBefore is the read-write path just before the stale token was set,
	// which must be closed: no desired replica and no Broker Pod that is Ready
	// or not terminating.
	PathBefore *s07Path `json:"rwPathBefore,omitempty"`
}

// s07ManualRelease is the run's own release of a quarantine only a person
// may release, made after the settle wait with -partition-manual-release. It
// names the quarantine and the run's cluster, the rows of every member before
// the release, the stale token set first, and, for the current token: when it
// was set and which member answered as the active master then, each refusal
// the Operator recorded, the release as status recorded it, the acknowledged
// writes the released master lacked before it (accepted as lost), the
// read-write path's return, the first acknowledged write through a new
// connection after it, and every member's rows after it. Times are on the
// run's clock, except ReleaseTime, which is the Operator's.
type s07ManualRelease struct {
	// NotMade says why the run set no token; nothing was annotated then.
	NotMade      string `json:"notMade,omitempty"`
	ClusterUID   string `json:"clusterUID,omitempty"`
	GenerationID string `json:"generationID,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Revision     int64  `json:"revision,omitempty"`
	// TokenPrefix is the first characters of the current token the run set.
	TokenPrefix string               `json:"currentTokenPrefix,omitempty"`
	RowsBefore  *s07RowSnapshot      `json:"rowsBeforeRelease,omitempty"`
	Stale       *s07StaleTokenRecord `json:"staleToken,omitempty"`
	AppliedAt   *time.Time           `json:"currentTokenSetAt,omitempty"`
	SetError    string               `json:"currentTokenSetError,omitempty"`
	// MasterAtApply is the one member that answered as the active master in
	// the sweep made just before the current token was set, "" when none or
	// more than one did; StatesAtApply is each member's raw state then.
	MasterAtApply  string            `json:"activeMasterWhenSet,omitempty"`
	StatesAtApply  map[string]string `json:"statesWhenSet,omitempty"`
	Refusals       []string          `json:"releaseRefusals,omitempty"`
	AcceptedSeenAt *time.Time        `json:"releaseSeenAt,omitempty"`
	ReleaseTime    *time.Time        `json:"releaseTime,omitempty"`
	ReleasedMaster string            `json:"releasedObservedMaster,omitempty"`
	// AcceptedAsLost are the acknowledged writes the released master lacked
	// in RowsBefore: the release accepts that they are lost.
	AcceptedAsLost      []string        `json:"acknowledgedWritesAcceptedAsLost,omitempty"`
	AnnotationRemovedAt *time.Time      `json:"annotationRemovedAt,omitempty"`
	RWScaledUpAt        *time.Time      `json:"rwScaledUpAt,omitempty"`
	RWPodsReadyAt       *time.Time      `json:"rwPodsReadyAt,omitempty"`
	RWEndpointsReadyAt  *time.Time      `json:"rwEndpointsReadyAt,omitempty"`
	MasterAfter         string          `json:"activeMasterAfterRelease,omitempty"`
	FirstWriteAfter     *s07Probe       `json:"firstAcknowledgedWriteAfterSet,omitempty"`
	RowsAfter           *s07RowSnapshot `json:"rowsAfterRelease,omitempty"`
	// Events are the Operator's Events about the release annotation.
	Events []string `json:"events,omitempty"`
	// Unmet is what had not happened when the run stopped watching.
	Unmet       string   `json:"unmet,omitempty"`
	NeverEvents []string `json:"neverEvents,omitempty"`
	// AnnotationRemovedByRun is when the run removed a token it had set that
	// no release consumed, as when the release was refused or the run ended
	// first, so the Operator cannot release on it later; AnnotationRemoveError
	// says why that removal failed.
	AnnotationRemovedByRun *time.Time `json:"annotationRemovedByRun,omitempty"`
	AnnotationRemoveError  string     `json:"annotationRemoveError,omitempty"`
	// TokenReplaced says that the quarantine's token changed after the run set
	// it and before any release, so the token set went stale.
	TokenReplaced string `json:"currentTokenReplaced,omitempty"`
	// OtherRelease is a release of the quarantine by a token the run did not
	// set; it ends the run's release and blocks S07.
	OtherRelease string `json:"otherRelease,omitempty"`
	// currentToken is the current token the run set, and lastSet the last
	// token it set; neither is written to the evidence.
	currentToken, lastSet string
	// before is the data rules' report of RowsBefore.
	before *workload.Report
}

// s07ReleaseBounds is the time the run's manual release is allowed in the
// -timeout check, with -partition-manual-release: the rows read before and
// after it, the stale token's watch, the wait for the release and the path's
// return, and s07ReleaseSlack for its steps without a bound of their own.
func (c HAConfig) s07ReleaseBounds() time.Duration {
	if !c.PartitionManualRelease {
		return 0
	}
	return 2*s07SnapshotBound + s07StaleTokenWindow + s07ReleaseBound + s07ReleaseSlack
}

// s07ServerIsActive reports whether a member's raw state lists a server
// process registered_and_active, as in "master (server
// registered_and_active)". A member that could not be read, or answered
// without its node line, has no server state.
func s07ServerIsActive(state string) bool {
	if strings.HasPrefix(state, "unread:") || strings.HasPrefix(state, "no node state:") {
		return false
	}
	_, servers, ok := strings.Cut(state, "(server ")
	if !ok {
		return false
	}
	return slices.Contains(strings.Split(strings.TrimSuffix(servers, ")"), ", "), s07ServerActive)
}

// noteS07ActiveServers records, from the last sweep, which members' servers
// are active, when that changed, and the first sweep in which one was. It is
// called for sweeps made after the heal only.
func noteS07ActiveServers(rec *s07Record) {
	if len(rec.Sweeps) == 0 {
		return
	}
	sweep := rec.Sweeps[len(rec.Sweeps)-1]
	active := []string{}
	for _, m := range slices.Sorted(maps.Keys(sweep.States)) {
		if s07ServerIsActive(sweep.States[m]) {
			active = append(active, m)
		}
	}
	if n := len(rec.ActiveServerChanges); n == 0 || !slices.Equal(rec.ActiveServerChanges[n-1].Members, active) {
		rec.ActiveServerChanges = append(rec.ActiveServerChanges, s07ActiveServers{T: sweep.T, Members: active})
	}
	if len(active) > 0 && rec.ActiveServerAfterHeal == nil {
		rec.ActiveServerAfterHeal = &s07ActiveServers{T: sweep.T, Members: active}
		rec.note("server_active_after_heal", strings.Join(active, ","))
	}
}

// s07StaleToken is the token the run sets before the current one: the newest
// earlier token of the run's quarantine, which an observation revised away,
// or else the current token with its last character changed.
func s07StaleToken(tokens []s07SeenToken, generation, current string) (string, string) {
	for _, seen := range slices.Backward(tokens) {
		if t := seen.Token; seen.Generation == generation && t != "" && t != current {
			return t, s07StaleEarlier
		}
	}
	altered := []byte(current + "0")
	if len(current) > 0 {
		altered = []byte(current)
	}
	if altered[len(altered)-1] == '0' {
		altered[len(altered)-1] = '1'
	} else {
		altered[len(altered)-1] = '0'
	}
	return string(altered), s07StaleAltered
}

// s07SeenToken is a release token of the run's quarantine and its generation.
type s07SeenToken struct{ Generation, Token string }

// noteS07RunRelease records rel as the run's release when it released the
// run's quarantine generation with the current token the run set: when, the
// Operator's release time and master, and the acknowledged writes that master
// lacked before the release, which the release accepts as lost. It is called
// whenever the run reads the release, also after its own watch ended.
func noteS07RunRelease(rec *s07Record, rel *databasev1alpha1.WriteQuarantineRelease, now time.Time) {
	r := rec.ManualRelease
	if r == nil || rel == nil || r.currentToken == "" || r.AcceptedSeenAt != nil ||
		rel.GenerationID != r.GenerationID || rel.ReleaseToken != r.currentToken {
		return
	}
	released := rel.ReleaseTime.UTC()
	r.AcceptedSeenAt, r.ReleaseTime, r.ReleasedMaster = &now, &released, rel.ObservedMaster
	r.AcceptedAsLost = s07MissingOn(r.before, rel.ObservedMaster)
	rec.note("manual_release_seen", string(rel.Method)+" "+rel.ObservedMaster)
}

// s07ReleasedByRun reports whether the run's own current token released the
// quarantine: only then are the writes accepted as lost left out of the data
// rules' never-event.
func s07ReleasedByRun(rec *s07Record) bool {
	return rec.ManualRelease != nil && rec.ManualRelease.AcceptedSeenAt != nil
}

// noteS07OtherRelease records a release of the run's quarantine generation by
// a token the run did not set, once.
func noteS07OtherRelease(rec *s07Record, rel *databasev1alpha1.WriteQuarantineRelease, now time.Time) {
	r := rec.ManualRelease
	if r == nil || rel == nil || r.OtherRelease != "" || rel.GenerationID != r.GenerationID ||
		(r.currentToken != "" && rel.ReleaseToken == r.currentToken) {
		return
	}
	r.OtherRelease = fmt.Sprintf("released %s at %s by a token the run did not set (%s)", rel.Method,
		now.Format(time.RFC3339), cmpOr(s07TokenPrefix(rel.ReleaseToken), "none"))
	rec.note("quarantine_released_by_another_token", r.OtherRelease)
}

// s07TokenPrefix is the first 8 characters of a token, as the Operator names
// it in its Events: enough to tell tokens apart without recording one.
func s07TokenPrefix(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:8] + "..."
}

// s07SoleMaster is the one member a sweep read as the active master, or ""
// when none or more than one was.
func s07SoleMaster(sweep s07Sweep, members []string) string {
	var masters []string
	for _, m := range members {
		if sweep.Roles[m] == s07ActiveMaster {
			masters = append(masters, m)
		}
	}
	if len(masters) != 1 {
		return ""
	}
	return masters[0]
}

// s07OwnCluster reads the run's HA cluster after the namespace guard passed,
// and refuses one whose UID is not the one LAB01 recorded.
func (h *haRun) s07OwnCluster(ctx context.Context) (*databasev1alpha1.CubridCluster, error) {
	if err := h.guard(ctx); err != nil {
		return nil, fmt.Errorf("check the run namespace: %w", err)
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return nil, fmt.Errorf("read the HA cluster: %w", err)
	}
	if h.rec.ClusterUID == "" || string(cluster.UID) != h.rec.ClusterUID {
		return nil, fmt.Errorf("the HA cluster has UID %s, not the run's %q", cluster.UID, h.rec.ClusterUID)
	}
	return cluster, nil
}

// setS07ReleaseToken sets the release annotation to token on the run's own
// cluster: the namespace guard and the cluster's UID are checked first, and
// the patch names the UID, so the API server refuses it for another
// cluster of the same name.
func (h *haRun) setS07ReleaseToken(ctx context.Context, token string) error {
	cluster, err := h.s07OwnCluster(ctx)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": string(cluster.UID),
		"annotations": map[string]string{releaseWriteQuarantineAnnotation: token}}})
	if err != nil {
		return err
	}
	target := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name,
		Namespace: cluster.Namespace}}
	return h.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch))
}

// removeS07ReleaseToken removes the release annotation the run set when it
// still names token and no release consumed it, as the Operator removes a
// consumed one: a JSON patch that tests the run's cluster UID and the value
// before it removes the annotation. It runs within s07RemoveTokenBound, also
// after ctx ended, so that a refused release or a run ended by a signal does
// not leave a token the Operator could accept later.
func (h *haRun) removeS07ReleaseToken(ctx context.Context, rec *s07Record, r *s07ManualRelease) {
	if r.lastSet == "" || r.AcceptedSeenAt != nil && r.lastSet == r.currentToken {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s07RemoveTokenBound)
	defer cancel()
	cluster, err := h.s07OwnCluster(ctx)
	if err == nil && cluster.Annotations[releaseWriteQuarantineAnnotation] != r.lastSet {
		return
	}
	if err == nil {
		annotation := "/metadata/annotations/" + strings.ReplaceAll(strings.ReplaceAll(
			releaseWriteQuarantineAnnotation, "~", "~0"), "/", "~1")
		type operation struct {
			Op    string `json:"op"`
			Path  string `json:"path"`
			Value string `json:"value,omitempty"`
		}
		var patch []byte
		patch, err = json.Marshal([]operation{{jsonPatchTest, "/metadata/uid", string(cluster.UID)},
			{jsonPatchTest, annotation, r.lastSet}, {"remove", annotation, ""}})
		if err == nil {
			target := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name,
				Namespace: cluster.Namespace}}
			err = h.Client.Patch(ctx, target, client.RawPatch(types.JSONPatchType, patch))
		}
	}
	if err != nil {
		r.AnnotationRemoveError = err.Error()
		rec.note("release_token_not_removed", err.Error())
		return
	}
	now := time.Now().UTC()
	r.AnnotationRemovedByRun = &now
	rec.note("release_token_removed", "")
}

// s07HistoryNow is the run's history with what both S07 clients have written
// so far, without stopping them, so that the rows read before the release are
// judged against the writes those clients acknowledged. A line still being
// written is left out; a client file that cannot be read is noted.
func (h *haRun) s07HistoryNow(ctx context.Context, rec *s07Record) string {
	var b strings.Builder
	b.WriteString(h.history.String())
	for _, file := range []string{"/tmp/" + s07OpenClient + ".jsonl", "/tmp/s07n.jsonl"} {
		text, err := h.clientRun(ctx, "cat "+file+" 2>/dev/null; true")
		if err != nil {
			rec.note("client_history_unread", file+": "+err.Error())
			continue
		}
		b.WriteString(text[:strings.LastIndex(text, "\n")+1])
	}
	return b.String()
}

// releaseS07 releases, with -partition-manual-release, the quarantine the
// settle wait ended with, when it is one only a person may release, the
// run saw it, and it is on the run's own cluster in the run's namespace.
// Before the release it reads every member's rows against the history so
// far. It then sets a stale token and watches for s07StaleTokenWindow that
// the Operator ignores it: the quarantine stays and the read-write Brokers
// stay at zero; a release in that window is a never-event, and the run sets
// nothing more. Otherwise it sets the current token, read again, and watches
// within s07ReleaseBound for the release, the Brokers' return and the -rw
// endpoints, then reads every member's rows again. The release changes no
// verdict rule: whether the cluster settled is what the settle wait saw.
func (h *haRun) releaseS07(ctx context.Context, rec *s07Record) {
	r := &s07ManualRelease{}
	rec.ManualRelease = r
	notMade := func(format string, args ...any) {
		r.NotMade = fmt.Sprintf(format, args...)
		rec.note("manual_release_not_made", r.NotMade)
	}
	if err := ctx.Err(); err != nil {
		notMade("the run ended before the release: %v", err)
		return
	}
	if !manualOnly(rec.QuarantineInForce) {
		notMade("no quarantine only a person may release was in force after the heal (%s)",
			cmpOr(rec.QuarantineInForce, "none"))
		return
	}
	cluster, err := h.s07OwnCluster(ctx)
	if err != nil {
		notMade("%v", err)
		return
	}
	q := cluster.Status.WriteQuarantine
	switch {
	case q == nil || !manualOnly(string(q.Reason)):
		notMade("no quarantine only a person may release is in force any more")
		return
	case !slices.Contains(rec.Generations, q.GenerationID):
		notMade("the quarantine in force, generation %s, is not one the run saw", q.GenerationID)
		return
	case q.ClusterUID != string(cluster.UID):
		notMade("the quarantine was recorded for the cluster UID %q, not the run's %s", q.ClusterUID, cluster.UID)
		return
	}
	r.ClusterUID, r.GenerationID, r.Reason = string(cluster.UID), q.GenerationID, string(q.Reason)
	h.observeS07Release(ctx, rec, time.Now().UTC())
	before := s07LastPath(rec)
	if before.Desired != 0 || before.ActivePods != 0 || before.ReadyPods != 0 {
		notMade("the read-write path was not closed before the release: %d desired replica(s), %d Broker Pod(s) "+
			"not terminating, %d Ready", before.Desired, before.ActivePods, before.ReadyPods)
		return
	}
	r.RowsBefore, r.before = h.readS07Rows(ctx, h.s07HistoryNow(ctx, rec),
		s07File(rec.Variant, "rows-before-release.json"))
	defer func() {
		r.RowsAfter, _ = h.readS07Rows(ctx, h.s07HistoryNow(ctx, rec),
			s07File(rec.Variant, "rows-after-release.json"))
		r.Events = h.s07ReleaseEvents(ctx)
	}()
	defer h.removeS07ReleaseToken(ctx, rec, r)
	stale, kind := s07StaleToken(rec.tokens, q.GenerationID, q.ReleaseToken)
	r.Stale = &s07StaleTokenRecord{Kind: kind, TokenPrefix: s07TokenPrefix(stale), PathBefore: &before}
	if err := h.setS07ReleaseToken(ctx, stale); err != nil {
		r.Stale.SetError = err.Error()
		notMade("the stale token could not be set: %v", err)
		return
	}
	r.lastSet = stale
	setAt := time.Now().UTC()
	r.Stale.SetAt = &setAt
	rec.note("stale_release_token_set", kind)
	h.watchS07StaleToken(ctx, rec, r, stale)
	switch {
	case r.OtherRelease != "":
		r.Unmet = "the current token was not set: the quarantine was " + r.OtherRelease
		return
	case len(r.NeverEvents) > 0 || !r.Stale.QuarantineHeld:
		r.Unmet = "the current token was not set: the quarantine did not stay in force after the stale token"
		return
	}
	h.applyS07ReleaseToken(ctx, rec, r)
}

// watchS07StaleToken observes the cluster for s07StaleTokenWindow after the
// stale token was set: a release of the quarantine, or Brokers above zero
// while it is in force, is a never-event; the quarantine gone without a
// release ends the watch.
func (h *haRun) watchS07StaleToken(ctx context.Context, rec *s07Record, r *s07ManualRelease, stale string) {
	s := r.Stale
	s.QuarantineHeld, s.BrokersHeld = true, true
	deadline := s.SetAt.Add(s07StaleTokenWindow)
	for {
		now := time.Now().UTC()
		h.observeS07Release(ctx, rec, now)
		if cluster, err := h.s07OwnCluster(ctx); err == nil {
			s.Readings++
			q := cluster.Status.WriteQuarantine
			inForce := q != nil && q.GenerationID == r.GenerationID
			rel := cluster.Status.LastWriteQuarantineRelease
			switch {
			case rel != nil && rel.GenerationID == r.GenerationID && rel.ReleaseToken == stale:
				s.QuarantineHeld = false
				r.NeverEvents = append(r.NeverEvents, fmt.Sprintf("the %s quarantine (generation %s) was released "+
					"(%s) at %s on the stale token (%s, %s) the run set", r.Reason, r.GenerationID, rel.Method,
					now.Format(time.RFC3339), s.Kind, s.TokenPrefix))
			case rel != nil && rel.GenerationID == r.GenerationID:
				s.QuarantineHeld = false
				noteS07OtherRelease(rec, rel, now)
			case !inForce:
				s.QuarantineHeld = false
				rec.note("stale_release_token_quarantine_gone", "")
			}
			// A Broker Pod still terminating from the enforcement is not the
			// path coming back.
			if p := s07LastPath(rec); inForce && s.BrokersHeld && r.OtherRelease == "" &&
				(p.Desired > 0 || p.ReadyPods > 0 || p.ActivePods > 0) {
				s.BrokersHeld = false
				r.NeverEvents = append(r.NeverEvents, fmt.Sprintf("the read-write Brokers were at %d desired "+
					"replica(s), %d Broker Pod(s) not terminating and %d Ready at %s while the %s quarantine was in "+
					"force and only a stale token (%s) was set", p.Desired, p.ActivePods, p.ReadyPods,
					now.Format(time.RFC3339), r.Reason, s.Kind))
			}
		}
		if !s.QuarantineHeld || !now.Before(deadline) || ctx.Err() != nil {
			s.WatchedUntil = &now
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Poll):
		}
	}
}

// applyS07ReleaseToken sets the quarantine's current token, read again, and
// watches for the release and the read-write path's return.
func (h *haRun) applyS07ReleaseToken(ctx context.Context, rec *s07Record, r *s07ManualRelease) {
	cluster, err := h.s07OwnCluster(ctx)
	if err != nil {
		r.Unmet = "the current token was not set: " + err.Error()
		return
	}
	q := cluster.Status.WriteQuarantine
	if q == nil || q.GenerationID != r.GenerationID || !manualOnly(string(q.Reason)) {
		r.Unmet = "the current token was not set: the quarantine is no longer in force"
		return
	}
	r.Reason, r.Revision, r.TokenPrefix = string(q.Reason), q.Revision, s07TokenPrefix(q.ReleaseToken)
	now := time.Now().UTC()
	h.sweepS07(ctx, rec, now, false, false)
	noteS07ActiveServers(rec)
	sweep := rec.Sweeps[len(rec.Sweeps)-1]
	r.MasterAtApply, r.StatesAtApply = s07SoleMaster(sweep, h.members), sweep.States
	if err := h.setS07ReleaseToken(ctx, q.ReleaseToken); err != nil {
		r.SetError = err.Error()
		r.Unmet = "the current token was not set: " + err.Error()
		return
	}
	r.currentToken, r.lastSet = q.ReleaseToken, q.ReleaseToken
	applied := time.Now().UTC()
	r.AppliedAt = &applied
	rec.note("release_token_set", cmpOr(r.MasterAtApply, "no active master"))
	deadline := applied.Add(s07ReleaseBound)
	for {
		now := time.Now().UTC()
		h.observeS07Release(ctx, rec, now)
		h.noteS07Release(ctx, rec, r, now)
		if r.RWEndpointsReadyAt != nil && now.Sub(*r.RWEndpointsReadyAt) >= s07ReleaseWriteWindow {
			break
		}
		if r.OtherRelease != "" {
			r.Unmet = "the quarantine was " + r.OtherRelease
			break
		}
		if !now.Before(deadline) || ctx.Err() != nil {
			r.Unmet = s07ReleaseUnmet(r)
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Poll):
		}
	}
	if rec.SettledAt != nil {
		rec.QuarantineAtEnd = rec.QuarantineInForce
		last := rec.Sweeps[len(rec.Sweeps)-1]
		if master := s07SoleMaster(last, h.members); master != "" {
			rec.FinalMaster, rec.FinalSlaves = master, nil
			for _, m := range h.members {
				if last.Roles[m] == s07Slave {
					rec.FinalSlaves = append(rec.FinalSlaves, m)
				}
			}
		}
	}
}

// observeS07Release makes one sweep and one observation of the path, as the
// settle wait does.
func (h *haRun) observeS07Release(ctx context.Context, rec *s07Record, now time.Time) {
	h.sweepS07(ctx, rec, now, false, false)
	noteS07ActiveServers(rec)
	h.observeS07Path(ctx, rec, now, false, false)
}

// noteS07Release records, after the current token was set, each refusal the
// Operator recorded, the release, the annotation's removal, and the
// read-write path's return once the release was seen.
func (h *haRun) noteS07Release(ctx context.Context, rec *s07Record, r *s07ManualRelease, now time.Time) {
	cluster, err := h.s07OwnCluster(ctx)
	if err != nil {
		rec.note("cluster_unread", err.Error())
		return
	}
	if q := cluster.Status.WriteQuarantine; q != nil && q.GenerationID == r.GenerationID {
		if q.ReleaseRefusal != "" && !slices.Contains(r.Refusals, q.ReleaseRefusal) {
			r.Refusals = append(r.Refusals, q.ReleaseRefusal)
			rec.note("manual_release_refused", q.ReleaseRefusal)
		}
		if q.ReleaseToken != r.currentToken && r.TokenReplaced == "" {
			r.TokenReplaced = fmt.Sprintf("revision %d replaced the token the run set at %s, before any release",
				q.Revision, now.Format(time.RFC3339))
			rec.note("release_token_replaced", r.TokenReplaced)
		}
	}
	noteS07RunRelease(rec, cluster.Status.LastWriteQuarantineRelease, now)
	noteS07OtherRelease(rec, cluster.Status.LastWriteQuarantineRelease, now)
	if r.AcceptedSeenAt == nil {
		return
	}
	if cluster.Annotations[releaseWriteQuarantineAnnotation] == "" && r.AnnotationRemovedAt == nil {
		r.AnnotationRemovedAt = &now
	}
	p := s07LastPath(rec)
	if p.Desired > 0 && r.RWScaledUpAt == nil {
		r.RWScaledUpAt = &now
	}
	if p.ReadyPods > 0 && r.RWPodsReadyAt == nil {
		r.RWPodsReadyAt = &now
	}
	if p.ReadyEndpoints > 0 && r.RWEndpointsReadyAt == nil {
		r.RWEndpointsReadyAt = &now
		rec.note("rw_endpoints_ready_after_release", fmt.Sprintf("%d endpoint(s)", p.ReadyEndpoints))
	}
	r.MasterAfter = s07SoleMaster(rec.Sweeps[len(rec.Sweeps)-1], h.members)
}

// s07LastPath is the read-write path as the run last recorded it, every value
// -1 when it has none.
func s07LastPath(rec *s07Record) s07Path {
	if len(rec.Path) == 0 {
		return s07Path{Desired: -1, Pods: -1, ReadyPods: -1, Endpoints: -1, ReadyEndpoints: -1}
	}
	return rec.Path[len(rec.Path)-1]
}

// s07ReleaseUnmet names what had not happened when the watch after the
// current token ended.
func s07ReleaseUnmet(r *s07ManualRelease) string {
	switch {
	case r.AcceptedSeenAt == nil && len(r.Refusals) > 0:
		return "the release was not accepted; the Operator refused it: " + r.Refusals[len(r.Refusals)-1]
	case r.AcceptedSeenAt == nil:
		return "the release was not accepted: no release of the quarantine was recorded in status"
	case r.RWEndpointsReadyAt == nil:
		return "the read-write path did not return: no ready -rw endpoint was seen"
	}
	return ""
}

// s07MissingOn names the acknowledged writes member lacked in report.
func s07MissingOn(report *workload.Report, member string) []string {
	if report == nil {
		return nil
	}
	var missing []string
	for _, v := range report.Members[member].Violations {
		if v.Rule == workload.RuleMissing {
			missing = append(missing, v.OpID)
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing)
}

// s07ReleaseEvents lists the Operator's Events about the release annotation
// on the run's cluster, best-effort.
func (h *haRun) s07ReleaseEvents(ctx context.Context) []string {
	events := &corev1.EventList{}
	if err := h.Client.List(ctx, events, client.InNamespace(h.cfg.Namespace)); err != nil {
		return []string{"events unread: " + err.Error()}
	}
	var named []string
	for _, e := range events.Items {
		if string(e.InvolvedObject.UID) == h.rec.ClusterUID && strings.HasPrefix(e.Reason, s07ReleaseEventPrefix) {
			named = append(named, e.LastTimestamp.UTC().Format(time.RFC3339)+" "+e.Reason+": "+e.Message)
		}
	}
	return named
}

// s07FirstAcknowledgedAfter is the write through a new connection that
// started after t and was acknowledged first, or nil.
func s07FirstAcknowledgedAfter(probes []s07Probe, t time.Time) *s07Probe {
	var first *s07Probe
	for i := range probes {
		p := probes[i]
		if p.Outcome != workload.Acknowledged || !p.StartedAt.After(t) {
			continue
		}
		if first == nil || p.AnsweredAt.Before(first.AnsweredAt) {
			first = &p
		}
	}
	return first
}

// s07AcceptedLoss counts, of the writes accepted as lost, those the final
// master's rows lack, and on each other member the rows of them it holds
// while the master does not.
func s07AcceptedLoss(master string, rows map[string][]workload.Row, accepted []string) (int, map[string]int) {
	held := map[string]bool{}
	for _, r := range rows[master] {
		held[r.OpID] = true
	}
	missing := 0
	for _, id := range accepted {
		if !held[id] {
			missing++
		}
	}
	others := map[string]int{}
	for member, memberRows := range rows {
		if member == master {
			continue
		}
		for _, r := range memberRows {
			if !held[r.OpID] && slices.Contains(accepted, r.OpID) {
				others[member]++
			}
		}
	}
	return missing, others
}

// s07ReleaseNote reports the run's manual release for the reason of a run
// that passed or is blocked, or "".
func s07ReleaseNote(rec *s07Record) string {
	r := rec.ManualRelease
	if r == nil {
		return ""
	}
	if r.NotMade != "" {
		return "; the run made no manual release: " + r.NotMade
	}
	var notes []string
	if s := r.Stale; s != nil {
		notes = append(notes, fmt.Sprintf("the stale token (%s) set first was read %d time(s): quarantine held "+
			"%t, read-write Brokers held at zero %t", s.Kind, s.Readings, s.QuarantineHeld, s.BrokersHeld))
	}
	if r.AppliedAt != nil {
		if r.MasterAtApply == "" {
			notes = append(notes, "no member answered as the active master when the current token was set")
		} else {
			notes = append(notes, r.MasterAtApply+" answered as the active master when the current token was set")
		}
	}
	if r.AcceptedSeenAt != nil {
		notes = append(notes, fmt.Sprintf("the release was accepted with %s as master, and %d acknowledged "+
			"write(s) it lacked before the release are accepted as lost", cmpOr(r.ReleasedMaster, "unknown"),
			len(r.AcceptedAsLost)))
	}
	if r.TokenReplaced != "" {
		notes = append(notes, "the current token went stale: "+r.TokenReplaced)
	}
	if r.Unmet != "" {
		notes = append(notes, r.Unmet)
	}
	if r.AnnotationRemovedByRun != nil {
		notes = append(notes, "the run removed the token it set, which no release consumed")
	}
	return "; manual release by the run: " + strings.Join(notes, "; ")
}

// s07ReleaseMeasurements adds the run's manual release to the measurements:
// the times from setting the current token, the masters, and the writes
// accepted as lost.
func s07ReleaseMeasurements(r *s07ManualRelease, m map[string]any, since func(string, *time.Time, *time.Time)) {
	m["manualRelease"] = "accepted"
	switch {
	case r.NotMade != "":
		m["manualRelease"] = "not made"
	case r.AcceptedSeenAt == nil:
		m["manualRelease"] = "not accepted"
	}
	since("manualReleaseSeenWithin", r.AppliedAt, r.AcceptedSeenAt)
	since("rwScaledUpAfterReleaseWithin", r.AppliedAt, r.RWScaledUpAt)
	since("rwPodsReadyAfterReleaseWithin", r.AppliedAt, r.RWPodsReadyAt)
	since("rwEndpointsReadyAfterReleaseWithin", r.AppliedAt, r.RWEndpointsReadyAt)
	var answered *time.Time
	if r.FirstWriteAfter != nil {
		answered = &r.FirstWriteAfter.AnsweredAt
	}
	since("firstAcknowledgedWriteAfterReleaseWithin", r.AppliedAt, answered)
	m["activeMasterWhenReleaseSet"] = cmpOr(r.MasterAtApply, "none")
	m["releasedMaster"] = cmpOr(r.ReleasedMaster, "unknown")
	m["acknowledgedWritesAcceptedAsLost"] = len(r.AcceptedAsLost)
	if r.Stale != nil {
		m["staleTokenIgnored"] = r.Stale.QuarantineHeld && r.Stale.BrokersHeld && len(r.NeverEvents) == 0
	}
}
