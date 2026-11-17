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
	// before is the data rules' report of RowsBefore.
	before *workload.Report
}

// s07ReleaseBounds is the longest the run's manual release may take, with
// -partition-manual-release: the rows read before and after it, the stale
// token's watch and the wait for the release and the path's return.
func (c HAConfig) s07ReleaseBounds() time.Duration {
	if !c.PartitionManualRelease {
		return 0
	}
	return 2*s07SnapshotBound + s07StaleTokenWindow + s07ReleaseBound
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
func s07StaleToken(tokens []string, current string) (string, string) {
	for _, t := range slices.Backward(tokens) {
		if t != "" && t != current {
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
	case q.ClusterUID != "" && q.ClusterUID != string(cluster.UID):
		notMade("the quarantine was recorded for the cluster UID %s, not the run's %s", q.ClusterUID, cluster.UID)
		return
	}
	r.ClusterUID, r.GenerationID, r.Reason = string(cluster.UID), q.GenerationID, string(q.Reason)
	r.RowsBefore, r.before = h.readS07Rows(ctx, h.s07HistoryNow(ctx, rec),
		s07File(rec.Variant, "rows-before-release.json"))
	defer func() {
		r.RowsAfter, _ = h.readS07Rows(ctx, h.s07HistoryNow(ctx, rec),
			s07File(rec.Variant, "rows-after-release.json"))
		r.Events = h.s07ReleaseEvents(ctx)
	}()
	stale, kind := s07StaleToken(rec.tokens, q.ReleaseToken)
	r.Stale = &s07StaleTokenRecord{Kind: kind, TokenPrefix: s07TokenPrefix(stale)}
	if err := h.setS07ReleaseToken(ctx, stale); err != nil {
		r.Stale.SetError = err.Error()
		notMade("the stale token could not be set: %v", err)
		return
	}
	setAt := time.Now().UTC()
	r.Stale.SetAt = &setAt
	rec.note("stale_release_token_set", kind)
	h.watchS07StaleToken(ctx, rec, r)
	if len(r.NeverEvents) > 0 || !r.Stale.QuarantineHeld {
		r.Unmet = "the current token was not set: the quarantine did not stay in force after the stale token"
		return
	}
	h.applyS07ReleaseToken(ctx, rec, r)
}

// watchS07StaleToken observes the cluster for s07StaleTokenWindow after the
// stale token was set: a release of the quarantine, or Brokers above zero
// while it is in force, is a never-event; the quarantine gone without a
// release ends the watch.
func (h *haRun) watchS07StaleToken(ctx context.Context, rec *s07Record, r *s07ManualRelease) {
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
			if rel := cluster.Status.LastWriteQuarantineRelease; rel != nil && rel.GenerationID == r.GenerationID {
				s.QuarantineHeld = false
				r.NeverEvents = append(r.NeverEvents, fmt.Sprintf("the %s quarantine (generation %s) was released "+
					"(%s) at %s while only a stale token (%s, %s) was set", r.Reason, r.GenerationID, rel.Method,
					now.Format(time.RFC3339), s.Kind, s.TokenPrefix))
			} else if !inForce {
				s.QuarantineHeld = false
				rec.note("stale_release_token_quarantine_gone", "")
			}
			if p := s07LastPath(rec); inForce && s.BrokersHeld && (p.Desired > 0 || p.Pods > 0) {
				s.BrokersHeld = false
				r.NeverEvents = append(r.NeverEvents, fmt.Sprintf("the read-write Brokers were at %d desired "+
					"replica(s) and %d Pod(s) at %s while the %s quarantine was in force and only a stale token "+
					"(%s) was set", p.Desired, p.Pods, now.Format(time.RFC3339), r.Reason, s.Kind))
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
		if master := s07SoleMaster(rec.Sweeps[len(rec.Sweeps)-1], h.members); master != "" {
			rec.FinalMaster = master
			rec.FinalSlaves = slices.DeleteFunc(slices.Clone(h.members), func(m string) bool { return m == master })
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
	if q := cluster.Status.WriteQuarantine; q != nil && q.GenerationID == r.GenerationID && q.ReleaseRefusal != "" &&
		!slices.Contains(r.Refusals, q.ReleaseRefusal) {
		r.Refusals = append(r.Refusals, q.ReleaseRefusal)
		rec.note("manual_release_refused", q.ReleaseRefusal)
	}
	if rel := cluster.Status.LastWriteQuarantineRelease; rel != nil && rel.GenerationID == r.GenerationID &&
		r.AcceptedSeenAt == nil {
		released := rel.ReleaseTime.UTC()
		r.AcceptedSeenAt, r.ReleaseTime, r.ReleasedMaster = &now, &released, rel.ObservedMaster
		r.AcceptedAsLost = s07MissingOn(r.before, rel.ObservedMaster)
		rec.note("manual_release_seen", string(rel.Method)+" "+rel.ObservedMaster)
	}
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
	if r.Unmet != "" {
		notes = append(notes, r.Unmet)
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
