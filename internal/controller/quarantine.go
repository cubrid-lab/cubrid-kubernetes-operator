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

package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// memberIdentity is the Pod a member's answer came from: its UID and the
// restart count of its database container. An empty UID means that no Pod of
// the member's name was found.
type memberIdentity struct {
	PodUID       string
	RestartCount int32
	// Terminated is set when the Pod is being deleted and the kubelet
	// reported its database container terminated (podTerminated).
	Terminated bool
	// Created is the Pod's creationTimestamp; zero when unknown.
	Created time.Time
	// Deleting is set when the Pod has a deletionTimestamp. That alone does
	// not show the process ended: a Pod evicted from a node cut off from
	// the API server is marked so while its database runs.
	Deleting bool
	// DeletionGrace is the Pod's deletion grace period.
	DeletionGrace time.Duration
	// TerminatedAt and DeletionRequested are terminatedAt and
	// deletionRequested of the Pod.
	TerminatedAt, DeletionRequested time.Time
}

// terminationSkew is how much earlier than the deletion request the kubelet
// may stamp a container's end and still have reported it for that deletion:
// the API server and the node stamp the two times with different clocks.
const terminationSkew = 5 * time.Second

// quarantineStartBound is how long after its termination was confirmed a
// member stays not unverified without its next Pod being verified. It
// covers the next Pod's start and first answers: on Kind a deleted slave's
// Pod was a standby slave again 10.1 to 10.4 s after the deletion (S01/S02),
// and all three members had a role again 21.4 to 26.8 s after a master Pod
// was deleted (S03, rejoin_limit's baseline; docs/testing/scenario-contract.md,
// "Baselines"). 90 s is more than three times the slowest, and far under
// quarantined_failover_limit. A next Pod that stays Pending, crash-loops or
// cannot be reached is then unverified as any member.
const quarantineStartBound = 90 * time.Second

// deletionGrace is pod's deletion grace period; zero when not set.
func deletionGrace(pod *corev1.Pod) time.Duration {
	if pod.DeletionGracePeriodSeconds == nil {
		return 0
	}
	return time.Duration(*pod.DeletionGracePeriodSeconds) * time.Second
}

// deletionRequested is when pod's deletion was requested: its
// deletionTimestamp less its grace period; zero while it is not deleted.
func deletionRequested(pod *corev1.Pod) time.Time {
	if pod.DeletionTimestamp == nil {
		return time.Time{}
	}
	requested := pod.DeletionTimestamp.Time
	if grace := pod.DeletionGracePeriodSeconds; grace != nil {
		requested = requested.Add(-time.Duration(*grace) * time.Second)
	}
	return requested
}

// terminatedAt is when the kubelet reported pod's database process ended for
// its deletion, zero when it did not: the Pod is being deleted and the
// kubelet reported its database container terminated no earlier than the
// deletion was requested (deletionRequested, with terminationSkew). A
// terminated state left from a crash before the deletion, one without an end
// time, and ContainerStatusUnknown, which the kubelet reports when it could
// not read the container, do not count. A Pod deleted with --force is
// removed before the kubelet reports anything, so it never is.
func terminatedAt(pod *corev1.Pod) time.Time {
	requested := deletionRequested(pod)
	if requested.IsZero() {
		return time.Time{}
	}
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name != appName {
			continue
		}
		t := c.State.Terminated
		if t != nil && t.Reason != "ContainerStatusUnknown" && !t.FinishedAt.IsZero() &&
			!t.FinishedAt.Time.Before(requested.Add(-terminationSkew)) {
			return t.FinishedAt.Time
		}
		return time.Time{}
	}
	return time.Time{}
}

// podTerminated reports whether the kubelet confirmed the termination of
// pod's database process for its deletion (terminatedAt).
func podTerminated(pod *corev1.Pod) bool { return !terminatedAt(pod).IsZero() }

// podTermination is a database Pod the Pod watch saw: one the kubelet
// reported terminated while it was deleted (notePodTermination), or one
// deleted without that (notePodDeleted).
type podTermination struct {
	cluster types.NamespacedName
	member  string
	// at is when the watch saw it.
	at time.Time
	// created is the Pod's creationTimestamp, zero when unknown; finished
	// and requested are terminatedAt and deletionRequested of a confirmed
	// one.
	created, finished, requested time.Time
}

// podFacts are the times of pod that recordTerminations compares.
func podFacts(cluster types.NamespacedName, pod *corev1.Pod, at time.Time) podTermination {
	return podTermination{cluster: cluster, member: pod.Name, at: at, created: pod.CreationTimestamp.Time,
		finished: terminatedAt(pod), requested: deletionRequested(pod)}
}

// databasePod is the cluster of pod, a database Pod of a CubridCluster, and
// whether it is one.
func databasePod(pod *corev1.Pod) (types.NamespacedName, bool) {
	cluster := pod.Labels["app.kubernetes.io/instance"]
	if cluster == "" || pod.Labels["app.kubernetes.io/component"] != "database" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: pod.Namespace, Name: cluster}, true
}

// notePodDeleted is the Pod watch's handling of a deleted Pod, and reports
// whether it was a database Pod. One whose last state the watch knows shows
// the termination the kubelet confirmed is kept as notePodTermination keeps
// it. Any other, or one whose last state is not known, is kept as deleted
// unconfirmed: it ends a record of its member that covers another Pod
// (recordTerminations), since it may be a Pod the record would otherwise
// cover.
func (r *CubridClusterReconciler) notePodDeleted(pod *corev1.Pod, stateUnknown bool) bool {
	cluster, ok := databasePod(pod)
	if !ok {
		return false
	}
	if !stateUnknown && r.notePodTermination(pod) {
		return true
	}
	r.podUnconfirmed.Store(pod.UID, podFacts(cluster, pod, r.now()))
	return true
}

// podTerminationMemory is how long a termination the watch saw is kept in
// memory for the next pass to record.
const podTerminationMemory = 10 * time.Minute

// notePodTermination keeps in memory a database Pod of a cluster that the
// kubelet reported terminated while it was being deleted, and reports
// whether it was one. The Pod watch calls it on every change, so that the
// state is kept even when the Pod is gone before the next pass reads it.
// A change the watch missed leaves the member unverified.
func (r *CubridClusterReconciler) notePodTermination(pod *corev1.Pod) bool {
	cluster, ok := databasePod(pod)
	if !ok || !podTerminated(pod) {
		return false
	}
	r.podTerminations.Store(pod.UID, podFacts(cluster, pod, r.now()))
	return true
}

// recordTerminations is status.memberTerminations with the terminations
// seen for each member's last Pod added: in the reads of this pass (before
// and ids) or by the watch. It updates status.memberPods with the Pods read
// in this pass. Only a termination of the Pod the member was last seen in
// is recorded: a Pod deleted in between without being seen could have been
// force-deleted. A Pod of the member the watch saw deleted unconfirmed and
// created after the confirmed one, or created at a time not known, keeps
// that termination from being recorded and ends its record: it may be the
// next Pod, deleted with --force before a pass read it. One created before
// the confirmed Pod, as in an earlier fault, ends nothing. A termination
// older than quarantineStartBound is not recorded, so the bound holds from
// the kubelet's report.
func (r *CubridClusterReconciler) recordTerminations(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	members []string, before, ids map[string]memberIdentity) []databasev1alpha1.MemberTermination {
	key := client.ObjectKeyFromObject(cluster)
	kept := func(store *sync.Map) map[string]podTermination {
		pods := map[string]podTermination{}
		store.Range(func(uid, v any) bool {
			t := v.(podTermination)
			switch {
			case r.now().Sub(t.at) > podTerminationMemory:
				store.Delete(uid)
			case t.cluster == key:
				pods[string(uid.(types.UID))] = t
			}
			return true
		})
		return pods
	}
	confirmed, unconfirmed := kept(&r.podTerminations), kept(&r.podUnconfirmed)
	for _, read := range []map[string]memberIdentity{before, ids} {
		for m, id := range read {
			if _, watched := confirmed[id.PodUID]; id.Terminated && !watched {
				confirmed[id.PodUID] = podTermination{cluster: key, member: m, at: r.now(), created: id.Created,
					finished: id.TerminatedAt, requested: id.DeletionRequested}
			}
		}
	}
	// laterUnconfirmed is a Pod of member deleted unconfirmed that is not
	// uid and not known to be created before created.
	laterUnconfirmed := func(member, uid string, created time.Time) string {
		for u, t := range unconfirmed {
			if t.member == member && u != uid && (created.IsZero() || t.created.IsZero() || !t.created.Before(created)) {
				return u
			}
		}
		return ""
	}
	out := slices.DeleteFunc(slices.Clone(cluster.Status.MemberTerminations), func(t databasev1alpha1.MemberTermination) bool {
		var created time.Time
		if t.PodCreated != nil {
			created = t.PodCreated.Time
		}
		if deleted := laterUnconfirmed(t.Name, t.PodUID, created); deleted != "" {
			logf.FromContext(ctx).Info("Ended the confirmed termination of a member's Pod: a later Pod of it was deleted unconfirmed",
				"member", t.Name, "podUID", t.PodUID, "deletedPodUID", deleted)
			return true
		}
		return false
	})
	last := map[string]string{}
	for _, p := range cluster.Status.MemberPods {
		last[p.Name] = p.PodUID
	}
	var pods []databasev1alpha1.MemberPod
	for _, m := range members {
		uid := lastPodConfirmed(m, last[m], confirmed)
		c := confirmed[uid]
		c.finished = trustedEnd(c.finished, c.at)
		if uid != "" && (c.finished.IsZero() || r.now().Sub(c.finished) > quarantineStartBound ||
			laterUnconfirmed(m, uid, c.created) != "" || unconfirmed[uid].member != "") {
			uid = ""
		}
		if uid != "" && !slices.ContainsFunc(out, func(t databasev1alpha1.MemberTermination) bool {
			return t.Name == m && t.PodUID == uid
		}) {
			record := databasev1alpha1.MemberTermination{Name: m, PodUID: uid, ConfirmedAt: metav1.NewTime(c.finished)}
			if !c.created.IsZero() {
				record.PodCreated = &metav1.Time{Time: c.created}
			}
			if !c.requested.IsZero() {
				record.DeletionRequestedAt = &metav1.Time{Time: c.requested}
			}
			out = slices.DeleteFunc(out, func(t databasev1alpha1.MemberTermination) bool { return t.Name == m })
			out = append(out, record)
			logf.FromContext(ctx).Info("Recorded the confirmed termination of a member's Pod", "member", m, "podUID", uid)
		}
		seen := ids[m].PodUID
		if seen == "" {
			seen = last[m]
		}
		if seen != "" {
			pods = append(pods, databasev1alpha1.MemberPod{Name: m, PodUID: seen})
		}
	}
	cluster.Status.MemberPods = pods
	return out
}

// trustedEnd is the time a confirmed termination runs from: the end the
// node stamped, finished, bounded by the Operator's observation of it,
// observed. An end later than the observation by more than terminationSkew
// is not trusted and is zero, so a node clock ahead cannot lengthen
// quarantineStartBound.
func trustedEnd(finished, observed time.Time) time.Time {
	switch {
	case !finished.After(observed):
		return finished
	case finished.Sub(observed) > terminationSkew:
		return time.Time{}
	}
	return observed
}

// lastPodConfirmed is the UID of member's last Pod seen before this pass,
// last, when its termination was confirmed; empty otherwise. The Pod read in
// this pass is that Pod or a later one.
func lastPodConfirmed(member, last string, confirmed map[string]podTermination) string {
	if last == "" || confirmed[last].member != member {
		return ""
	}
	return last
}

// quarantineInput is what one pass knows for the decision on the read-write
// quarantine (ADR-0005, section 8).
type quarantineInput struct {
	// Now is the time of the pass.
	Now time.Time
	// ClusterUID is the CubridCluster's UID.
	ClusterUID string
	// Members are the promotable members in ordinal order: a member outranks
	// every member after it.
	Members []string
	// Observations are this pass's answers, by member.
	Observations map[string]RoleObservation
	// Identities are the members' Pods in this pass.
	Identities map[string]memberIdentity
	// Unstable are the members whose Pod differed between the reads before
	// and after the probes: their answers verify nothing in this pass.
	Unstable map[string]bool
	// IdentitiesUnknown says that the members' Pods could not be read. Only
	// the rule of two active masters is applied then; the verifications are
	// kept as they were, no quarantine is released, and the caller keeps the
	// read-write Brokers at zero for the pass.
	IdentitiesUnknown bool
	// LastResolvedPrimary is status.lastResolvedPrimary before this pass.
	LastResolvedPrimary string
	// Terminations are the Pods whose termination the kubelet confirmed, as
	// the status holds them with this pass's observations added.
	Terminations []databasev1alpha1.MemberTermination
	// Verifications, Quarantine and LastRelease are what the status held
	// before this pass.
	Verifications []databasev1alpha1.MemberVerification
	Quarantine    *databasev1alpha1.WriteQuarantine
	LastRelease   *databasev1alpha1.WriteQuarantineRelease
	// EndedExemptions are the members of LastRelease's exemptMembers whose
	// exemption this reconciler ended in an earlier pass whose status write
	// may have failed; anyExemption stands for all of them. It can only end
	// an exemption, never grant one.
	EndedExemptions map[string]bool
	// Nonce returns a new random value of at least 128 bits.
	Nonce func() (string, error)
}

// quarantineChange says what a decision changed about the quarantine.
type quarantineChange string

const (
	quarantineUnchanged quarantineChange = ""
	quarantineStarted   quarantineChange = "started"
	quarantineRevised   quarantineChange = "revised"
	quarantineReleased  quarantineChange = "released"
)

// quarantineDecision is the outcome of decideWriteQuarantine.
type quarantineDecision struct {
	// Verifications replace status.memberVerifications.
	Verifications []databasev1alpha1.MemberVerification
	// Terminations replace status.memberTerminations.
	Terminations []databasev1alpha1.MemberTermination
	// Confirmed are the members that are not unverified because their old
	// Pod's termination was confirmed and their next Pod has not shown
	// otherwise.
	Confirmed []string
	// Quarantine is the quarantine in force after this pass; nil for none.
	Quarantine *databasev1alpha1.WriteQuarantine
	// Released is set on the pass that released a quarantine.
	Released *databasev1alpha1.WriteQuarantineRelease
	// LastRelease replaces status.lastWriteQuarantineRelease when Released
	// is not set: the members a manual release exempts, less those this pass
	// verified or found in another Pod or container start.
	LastRelease *databasev1alpha1.WriteQuarantineRelease
	// ExemptionEnded describes each end of a manual release's exemption in
	// this pass.
	ExemptionEnded []string
	// Change says what changed.
	Change quarantineChange
	// ActiveMasters are the active masters of this pass, and Unverified the
	// other members that are not verified, in ordinal order.
	ActiveMasters []string
	Unverified    []string
}

const (
	// quarantineVerifyGap is how far apart the first and the latest of a
	// member's uninterrupted non-master answers must be for the member to be
	// verified: two probes a few seconds apart (ADR-0005, section 8).
	quarantineVerifyGap = 2 * time.Second
	// quarantineReleaseInterval is how far apart the two passes that release
	// a WriteQuarantinedMemberUnverified quarantine must be.
	quarantineReleaseInterval = 10 * time.Second
	// quarantineAnswerGap is the longest time between two consecutive
	// answers, or releasable passes, that still counts as uninterrupted. It
	// follows the cadence of a healthy cluster, not the observation TTL: the
	// next pass starts haResyncInterval after the previous one ended, a pass
	// that waits for an unreachable member lasts up to roleProbeTimeout, the
	// answer of the next pass can come up to roleProbeTimeout after it
	// starts, and the queue and the API server add some seconds (the margin).
	// With 30 s, one slow member does not unverify a healthy slave and
	// quarantine the path without a fault; a longer gap, as while the
	// Operator was stopped, starts a verification or a release check over.
	quarantineAnswerGap = haResyncInterval + 2*roleProbeTimeout + 10*time.Second
	// unverifiedResyncInterval is how soon an HA cluster is observed again
	// while a member is unverified or a quarantine is in force, in place of
	// haResyncInterval: it shortens the time until a second active master
	// starts a quarantine and until a quarantine is released.
	unverifiedResyncInterval = 3 * time.Second
)

// newQuarantineNonce returns 128 random bits, hex-encoded.
func newQuarantineNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a write quarantine nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// activeMaster reports whether o is a fresh answer that claims an active
// master: the master role, a registered_and_active server and the member's
// own line in its node list saying master (ActiveMasterClaim). It closes the
// write path only, and never names a primary.
func activeMaster(o RoleObservation, now time.Time) bool {
	return fresh(o, now) && o.Role == databasev1alpha1.RoleMaster && o.ActiveMasterClaim
}

// nonMasterAnswer reports whether o is a fresh, authenticated and readable
// answer from the member that was asked, with a slave or replica role that
// does not contradict itself. An answer without a role, a refused token, an
// untrusted role or an old answer is no evidence that the member is not an
// active master.
func nonMasterAnswer(o RoleObservation, now time.Time) bool {
	return fresh(o, now) && !o.TokenRefused && !o.Unreadable && !o.Conflicting && !o.ActiveMasterClaim &&
		(o.Role == databasev1alpha1.RoleSlave || o.Role == databasev1alpha1.RoleReplica)
}

// observedRoleText says what o showed of a member, for the quarantine's
// records and Events.
func observedRoleText(o RoleObservation, now time.Time) string {
	switch {
	case activeMaster(o, now):
		return "active master"
	case o.ObservedAt.IsZero():
		return "not asked"
	case o.TokenRefused:
		return "token refused"
	case !o.Reachable:
		return "no answer"
	case !fresh(o, now):
		return "no fresh answer"
	case o.Unreadable:
		return "unreadable answer"
	case o.Role == "" || o.Role == databasev1alpha1.RoleUnknown:
		return "no role"
	case o.Conflicting:
		return "untrusted " + string(o.Role)
	}
	return string(o.Role)
}

// ceilSecond is t rounded up to the second. The API server stores a time to
// the second; rounding up keeps a stored time from making two answers look
// further apart than they were.
func ceilSecond(t time.Time) metav1.Time {
	c := t.Truncate(time.Second)
	if c.Before(t) {
		c = c.Add(time.Second)
	}
	return metav1.NewTime(c)
}

// decideWriteQuarantine decides the read-write quarantine from one pass's
// observations and the status before it (ADR-0005, section 8). It is pure but
// for in.Nonce.
//
// A member is verified when it gave a non-master answer (nonMasterAnswer) in
// this pass and in every pass since one at least quarantineVerifyGap earlier,
// from the same Pod and container start; any other answer, or another Pod or
// restart count, starts over. An active master is a fresh active-master
// claim (activeMaster), and a member outranks the members after it.
//
// The rules, in order: two or more active masters start a quarantine with
// WriteQuarantinedMultipleMasters; exactly one active master M and an
// unverified member that outranks M or is the last resolved primary start
// one with WriteQuarantinedMemberUnverified, recording M and those members;
// otherwise none starts, and a quarantine in force stays.
//
// While a quarantine is in force, a new member that the rules would name, or
// a recorded member removed from the cluster or found in another Pod or
// container start, revises
// it: its revision increments and its release token is replaced, its
// generation stays. A WriteQuarantinedMemberUnverified quarantine becomes
// WriteQuarantinedMasterChanged, which has no automatic release, when an
// active master other than M is seen or M is verified as no active master.
// It is released automatically after two passes at least
// quarantineReleaseInterval apart that each see M, in the same Pod, as the
// only active master and every other member verified since before the first
// of them; any other pass in between starts over.
func decideWriteQuarantine(in quarantineInput) (quarantineDecision, error) {
	d, verified := verifyMembers(in)
	var exempt map[string]bool
	d.LastRelease, exempt, d.ExemptionEnded = releaseExemption(in, d.ActiveMasters, verified)
	ordinal := make(map[string]int, len(in.Members))
	for i, m := range in.Members {
		ordinal[m] = i
	}
	// The members the rules name in this pass. Without the members' Pods
	// only two active masters do.
	var named []string
	switch {
	case len(d.ActiveMasters) == 0:
	case len(d.ActiveMasters) == 1 && in.IdentitiesUnknown:
	case len(d.ActiveMasters) == 1:
		m := d.ActiveMasters[0]
		for _, u := range d.Unverified {
			if !exempt[u] && (ordinal[u] < ordinal[m] || u == in.LastResolvedPrimary) {
				named = append(named, u)
			}
		}
	default:
		named = d.ActiveMasters
	}
	if in.Quarantine == nil {
		q, err := startQuarantine(in, d.ActiveMasters, named)
		if q != nil {
			d.Quarantine, d.Change = q, quarantineStarted
		}
		return d, err
	}
	q := in.Quarantine.DeepCopy()
	d.Quarantine = q
	revised, err := reviseQuarantine(in, q, d.ActiveMasters, named, verified, ordinal)
	if err != nil || revised {
		if revised {
			d.Change = quarantineRevised
		}
		return d, err
	}
	switch {
	case in.IdentitiesUnknown || (q.ClusterUID != "" && q.ClusterUID != in.ClusterUID):
		// Without the members' Pods, or for a quarantine recorded for another
		// cluster UID, nothing is released.
		q.ReleaseCheckSince, q.ReleaseCheckLastTime = nil, nil
	case q.Reason == databasev1alpha1.WriteQuarantinedMemberUnverified:
		if d.Released = releaseCheck(in, q, d, verified); d.Released != nil {
			d.Quarantine, d.Change = nil, quarantineReleased
		}
	}
	return d, nil
}

// verifyMembers renews the members' verifications from this pass's answers
// and sorts the members into active masters, verified and unverified ones.
func verifyMembers(in quarantineInput) (quarantineDecision, map[string]bool) {
	var d quarantineDecision
	previous := make(map[string]databasev1alpha1.MemberVerification, len(in.Verifications))
	for _, v := range in.Verifications {
		previous[v.Name] = v
	}
	terminations := make(map[string]databasev1alpha1.MemberTermination, len(in.Terminations))
	for _, t := range in.Terminations {
		terminations[t.Name] = t
	}
	verified := map[string]bool{}
	if in.IdentitiesUnknown {
		// Nothing is verified, renewed or ended without the members' Pods.
		d.Verifications = slices.Clone(in.Verifications)
		d.Terminations = slices.Clone(in.Terminations)
	}
	for _, m := range in.Members {
		o := in.Observations[m]
		if activeMaster(o, in.Now) {
			// An active-master claim ends a confirmed termination.
			d.ActiveMasters = append(d.ActiveMasters, m)
			continue
		}
		answered := !in.IdentitiesUnknown && !in.Unstable[m] && nonMasterAnswer(o, in.Now)
		t, recorded := terminations[m]
		if recorded && !in.IdentitiesUnknown && confirmedDown(&t, in.Identities[m], o, answered, in.Now) {
			if !answered || !verifiedAnswer(in, &d, previous[m], m) {
				d.Terminations = append(d.Terminations, t)
				d.Confirmed = append(d.Confirmed, m)
				continue
			}
			// Verified: the ordinary verification takes over.
			verified[m] = true
			continue
		}
		if !answered {
			d.Unverified = append(d.Unverified, m)
			continue
		}
		if verifiedAnswer(in, &d, previous[m], m) {
			verified[m] = true
		} else {
			d.Unverified = append(d.Unverified, m)
		}
	}
	return d, verified
}

// confirmedDown reports whether a member with the record t of a Pod whose
// termination the kubelet confirmed still does not count as unverified,
// from its Pod id and its answer o in this pass (ADR-0005, section 8, as
// amended by #870). The old Pod still listed is confirmed down. A later Pod
// that has not answered yet is starting. Its first non-master answer
// (answered) sets t.Answered; from then on, any answer that is not one, or
// none, ends the record. A master role, or an answer that contradicts
// itself, ends it at once.
func confirmedDown(t *databasev1alpha1.MemberTermination, id memberIdentity, o RoleObservation, answered bool,
	now time.Time) bool {
	switch {
	case now.Sub(t.ConfirmedAt.Time) > quarantineStartBound:
		return false
	case id.PodUID == t.PodUID:
		return true
	case id.PodUID == "":
		// Between the Pods; once the next Pod was seen, it is gone.
		return t.NextPodUID == ""
	case t.NextPodUID == "":
		// Only a Pod created no earlier than the confirmed Pod's deletion
		// was requested can be the Pod that replaced it.
		if t.DeletionRequestedAt == nil || id.Created.IsZero() || id.Created.Before(t.DeletionRequestedAt.Time) {
			return false
		}
		t.NextPodUID = id.PodUID
	case id.PodUID != t.NextPodUID:
		// A third Pod: the next one may have been deleted unseen.
		return false
	}
	switch {
	case answered:
		t.Answered = true
		return true
	case fresh(o, now) && (o.Role == databasev1alpha1.RoleMaster || o.Conflicting):
		return false
	}
	return !t.Answered
}

// verifiedAnswer renews member m's verification v from its non-master
// answer in this pass, adds it to d, and reports whether it verifies m.
func verifiedAnswer(in quarantineInput, d *quarantineDecision, v databasev1alpha1.MemberVerification, m string) bool {
	id := in.Identities[m]
	last := v.NonMasterSince.Time
	if v.LastNonMasterAt != nil {
		last = v.LastNonMasterAt.Time
	}
	if v.Name != m || v.PodUID != id.PodUID || v.RestartCount != id.RestartCount || in.Now.Sub(last) > quarantineAnswerGap {
		v = databasev1alpha1.MemberVerification{Name: m, PodUID: id.PodUID, RestartCount: id.RestartCount,
			NonMasterSince: ceilSecond(in.Now)}
	}
	v.LastNonMasterAt = &metav1.Time{Time: in.Now}
	d.Verifications = append(d.Verifications, v)
	return in.Now.Sub(v.NonMasterSince.Time) >= quarantineVerifyGap
}

// quarantineMember records member m as this pass saw it.
func quarantineMember(in quarantineInput, m string) databasev1alpha1.WriteQuarantineMember {
	return databasev1alpha1.WriteQuarantineMember{Name: m, PodUID: in.Identities[m].PodUID,
		RestartCount: in.Identities[m].RestartCount, ObservedRole: observedRoleText(in.Observations[m], in.Now)}
}

// anyExemption, in quarantineInput.EndedExemptions, ends every member's
// exemption.
const anyExemption = "*"

// releaseExemption is the members the last manual release accepted that
// rule 2 does not name in this pass, with the release record whose
// exemptMembers lose for good the members this pass ends the exemption for,
// and a description of each ending for the log and Events. It reads only the
// persisted release record (in.LastRelease), never memory.
//
// A member leaves exemptMembers once it is verified, found in another Pod or
// container start, or gone from the cluster. The whole list is cleared once
// an active master other than the release's master, which includes an exempt
// member, or the release's master in another Pod or container start, is
// seen, or once the release's master gives a fresh, readable answer that
// claims no active master; a pass with no active master and no such answer
// changes nothing. Ends kept in memory (in.EndedExemptions) apply too, so an
// end whose status write failed is not undone. The exemption applies
// only while the release's master, in the same Pod and container start, is
// the only active master; rule 1 does not consult it. Without the members'
// Pods only an active master other than the release's clears it.
func releaseExemption(in quarantineInput, masters []string,
	verified map[string]bool) (*databasev1alpha1.WriteQuarantineRelease, map[string]bool, []string) {
	rel := in.LastRelease.DeepCopy()
	if rel == nil || rel.Method != databasev1alpha1.WriteQuarantineReleasedManually || len(rel.ExemptMembers) == 0 {
		return rel, nil, nil
	}
	if why := exemptionEndedForAll(in, rel, masters); why != "" {
		ended := fmt.Sprintf("the exemption of %s ended: %s", strings.Join(rel.ExemptMembers, ", "), why)
		rel.ExemptMembers = nil
		return rel, nil, []string{ended}
	}
	if in.IdentitiesUnknown {
		// Only the ends kept in memory apply without the members' Pods.
		var ended []string
		kept := slices.DeleteFunc(slices.Clone(rel.ExemptMembers), func(m string) bool { return in.EndedExemptions[m] })
		for _, m := range rel.ExemptMembers {
			if in.EndedExemptions[m] {
				ended = append(ended, fmt.Sprintf("the exemption of %s ended in an earlier observation whose status "+
					"was not written", m))
			}
		}
		if len(kept) == 0 {
			kept = nil
		}
		rel.ExemptMembers = kept
		return rel, nil, ended
	}
	var ended []string
	kept := make([]string, 0, len(rel.ExemptMembers))
	for _, m := range rel.ExemptMembers {
		i := slices.IndexFunc(rel.Members, func(r databasev1alpha1.WriteQuarantineMember) bool { return r.Name == m })
		why := ""
		switch {
		case in.EndedExemptions[m]:
			why = "it ended in an earlier observation whose status was not written"
		case i < 0 || !slices.Contains(in.Members, m):
			why = "it is no longer a member"
		case verified[m]:
			why = "it was verified as no active master"
		// A Pod being deleted (Terminated) ends the exemption too: its next
		// Pod would end it anyway.
		case in.Identities[m] != (memberIdentity{PodUID: rel.Members[i].PodUID, RestartCount: rel.Members[i].RestartCount}):
			why = "it runs in another Pod or container start than the one accepted"
		}
		if why != "" {
			ended = append(ended, fmt.Sprintf("the exemption of %s ended: %s", m, why))
			continue
		}
		kept = append(kept, m)
	}
	rel.ExemptMembers = kept
	if len(kept) == 0 {
		rel.ExemptMembers = nil
	}
	if len(masters) != 1 {
		return rel, nil, ended
	}
	exempt := make(map[string]bool, len(kept))
	for _, m := range kept {
		exempt[m] = true
	}
	return rel, exempt, ended
}

// exemptionEndedForAll says why this pass ends the exemption of every member
// of the manual release rel, "" when it does not: an end kept in memory, the
// release's master answering as no active master, another member seen as an
// active master, or the release's master seen as one in another Pod or
// container start.
func exemptionEndedForAll(in quarantineInput, rel *databasev1alpha1.WriteQuarantineRelease, masters []string) string {
	if in.EndedExemptions[anyExemption] {
		return "it ended in an earlier observation whose status was not written"
	}
	if o := in.Observations[rel.ObservedMaster]; fresh(o, in.Now) && o.Reachable && !o.TokenRefused &&
		!o.Unreadable && !activeMaster(o, in.Now) && o.Role != "" && o.Role != databasev1alpha1.RoleUnknown {
		return fmt.Sprintf("the release's master %s answered as %s, no longer an active master", rel.ObservedMaster,
			observedRoleText(o, in.Now))
	}
	master := memberIdentity{PodUID: rel.ObservedMasterPodUID, RestartCount: rel.ObservedMasterRestartCount}
	for _, m := range masters {
		switch {
		case m != rel.ObservedMaster:
			return fmt.Sprintf("%s was seen as an active master, not the release's master %s", m, rel.ObservedMaster)
		case !in.IdentitiesUnknown && in.Identities[m] != master:
			return fmt.Sprintf("the release's master %s was seen as an active master in another Pod or container "+
				"start", m)
		}
	}
	return ""
}

// startQuarantine is the quarantine rules 1 and 2 start, or nil under rule 3.
func startQuarantine(in quarantineInput, masters, named []string) (*databasev1alpha1.WriteQuarantine, error) {
	reason, master := databasev1alpha1.WriteQuarantinedMultipleMasters, ""
	switch {
	case len(masters) > 1:
	case len(named) > 0:
		reason, master = databasev1alpha1.WriteQuarantinedMemberUnverified, masters[0]
	default:
		return nil, nil
	}
	generation, err := in.Nonce()
	if err != nil {
		return nil, err
	}
	token, err := in.Nonce()
	if err != nil {
		return nil, err
	}
	q := &databasev1alpha1.WriteQuarantine{GenerationID: generation, ClusterUID: in.ClusterUID, Reason: reason,
		StartTime: metav1.NewTime(in.Now), ObservedMaster: master, Revision: 1, ReleaseToken: token}
	if master != "" {
		q.ObservedMasterPodUID = in.Identities[master].PodUID
		q.ObservedMasterRestartCount = in.Identities[master].RestartCount
	}
	for _, m := range named {
		q.Members = append(q.Members, quarantineMember(in, m))
	}
	return q, nil
}

// reviseQuarantine escalates q, adds the members this pass names, removes
// members no longer in the cluster and follows the members' Pods. A change
// increments the revision, replaces the release token and resets the release
// check; it reports whether anything changed.
func reviseQuarantine(in quarantineInput, q *databasev1alpha1.WriteQuarantine, masters, named []string,
	verified map[string]bool, ordinal map[string]int) (bool, error) {
	revised := false
	others := slices.DeleteFunc(slices.Clone(masters), func(m string) bool { return m == q.ObservedMaster })
	unverifiedQuarantine := q.Reason == databasev1alpha1.WriteQuarantinedMemberUnverified
	departure := departureNone
	if unverifiedQuarantine {
		departure = masterDeparture(in, q, masters)
	}
	switch {
	case !unverifiedQuarantine:
	case departure == departureConfirmed && !in.IdentitiesUnknown && len(masters) == 1 && len(others) == 1:
		// The kubelet confirmed the termination of the recorded master's
		// Pod, and one other member is the only active master: CUBRID moved
		// the role. The quarantine is rebased on the new master, and the
		// former one is a member like any other.
		former, next := q.ObservedMaster, masters[0]
		id := in.Identities[next]
		q.ObservedMaster, q.ObservedMasterPodUID, q.ObservedMasterRestartCount = next, id.PodUID, id.RestartCount
		q.ObservedMasterLeavingPodUID, q.ObservedMasterLeavingConfirmed, q.ObservedMasterHoldUntil = "", false, nil
		q.Members = slices.DeleteFunc(q.Members, func(r databasev1alpha1.WriteQuarantineMember) bool { return r.Name == next })
		if !slices.ContainsFunc(q.Members, func(r databasev1alpha1.WriteQuarantineMember) bool { return r.Name == former }) {
			q.Members = append(q.Members, quarantineMember(in, former))
		}
		others, revised = nil, true
	case departure == departureLost, len(masters) > 1 && len(others) > 0:
		q.Reason, revised = databasev1alpha1.WriteQuarantinedMasterChanged, true
	case departure == departureHeld, departure == departureConfirmed:
		// The master's Pod is being deleted: neither rebased nor
		// escalated until its termination is confirmed, it is gone without
		// that, or the hold ends.
	case len(others) > 0 || verified[q.ObservedMaster]:
		q.Reason, revised = databasev1alpha1.WriteQuarantinedMasterChanged, true
	}
	for _, m := range slices.Concat(named, others) {
		if m == q.ObservedMaster || slices.ContainsFunc(q.Members, func(r databasev1alpha1.WriteQuarantineMember) bool {
			return r.Name == m
		}) {
			continue
		}
		q.Members, revised = append(q.Members, quarantineMember(in, m)), true
	}
	kept := q.Members[:0]
	for _, r := range q.Members {
		if _, member := ordinal[r.Name]; !member {
			revised = true
			continue
		}
		if !in.IdentitiesUnknown && (in.Identities[r.Name].PodUID != r.PodUID ||
			in.Identities[r.Name].RestartCount != r.RestartCount) {
			r, revised = quarantineMember(in, r.Name), true
		}
		kept = append(kept, r)
	}
	q.Members = kept
	slices.SortFunc(q.Members, func(a, b databasev1alpha1.WriteQuarantineMember) int {
		return ordinal[a.Name] - ordinal[b.Name]
	})
	if id := in.Identities[q.ObservedMaster]; q.ObservedMaster != "" && !in.IdentitiesUnknown &&
		(id.PodUID != q.ObservedMasterPodUID || id.RestartCount != q.ObservedMasterRestartCount) {
		q.ObservedMasterPodUID, q.ObservedMasterRestartCount, revised = id.PodUID, id.RestartCount, true
	}
	if !revised {
		return false, nil
	}
	token, err := in.Nonce()
	if err != nil {
		return false, err
	}
	q.Revision++
	q.ReleaseToken, q.ReleaseCheckSince, q.ReleaseCheckLastTime = token, nil, nil
	return true, nil
}

// quarantineHoldGraceCap is the most of a Pod's deletion grace period that
// holds a quarantine back while its master's Pod is only marked for
// deletion: a grace period of hours would otherwise keep a manual-only
// decision from being reported for as long.
const quarantineHoldGraceCap = 10 * time.Minute

// withHeldDeparture is stored, the quarantine the status read holds, with the
// mark of its master's Pod taken from held, the one this reconciler decided
// and holds in memory, when the status read does not carry it, as after a
// failed status write: so the hold runs from the first sight of the mark,
// not from a later pass. stored is not changed.
func withHeldDeparture(stored, held *databasev1alpha1.WriteQuarantine) *databasev1alpha1.WriteQuarantine {
	if stored == nil || held == nil || held.GenerationID != stored.GenerationID ||
		stored.ObservedMasterLeavingPodUID != "" || held.ObservedMasterLeavingPodUID == "" ||
		held.ObservedMasterLeavingPodUID != stored.ObservedMasterPodUID {
		return stored
	}
	out := stored.DeepCopy()
	out.ObservedMasterLeavingPodUID = held.ObservedMasterLeavingPodUID
	out.ObservedMasterLeavingConfirmed = held.ObservedMasterLeavingConfirmed
	out.ObservedMasterHoldUntil = held.ObservedMasterHoldUntil.DeepCopy()
	return out
}

// departure is what the Operator saw of the recorded master's Pod leaving.
type departure int

const (
	// departureNone: its Pod is not seen being deleted.
	departureNone departure = iota
	// departureHeld: its Pod is marked for deletion, the termination not
	// confirmed, within the hold.
	departureHeld
	// departureConfirmed: the kubelet confirmed the termination of the Pod.
	departureConfirmed
	// departureLost: its Pod was marked for deletion and is gone without a
	// confirmed termination, or the hold ended.
	departureLost
)

// masterDeparture judges the recorded master's Pod leaving, and keeps what
// must last in q: the Pod seen marked for deletion
// (ObservedMasterLeavingPodUID), only the Pod the quarantine recorded for the
// master; until when that mark holds the quarantine back
// (ObservedMasterHoldUntil, the Operator's time of the first sight, plus the
// Pod's grace period and quarantineStartBound); and whether the kubelet
// confirmed the termination of that Pod (ObservedMasterLeavingConfirmed), by
// a terminated state read or a termination record of that Pod UID. A mark
// alone does not show the process ended: a Pod evicted from a node cut off
// from the API server is marked while its database runs. The master
// claiming an active master from another Pod clears it. Without the Pods'
// reads nothing is set or confirmed (ADR-0005, section 8, as amended by
// #870).
func masterDeparture(in quarantineInput, q *databasev1alpha1.WriteQuarantine, masters []string) departure {
	m := q.ObservedMaster
	if m == "" {
		return departureNone
	}
	id := in.Identities[m]
	recorded := func(uid string) bool {
		return uid != "" && slices.ContainsFunc(in.Terminations, func(t databasev1alpha1.MemberTermination) bool {
			return t.Name == m && t.PodUID == uid
		})
	}
	if !in.IdentitiesUnknown {
		if slices.Contains(masters, m) && q.ObservedMasterLeavingPodUID != "" && id.PodUID != q.ObservedMasterLeavingPodUID {
			// The master claims again from another Pod. After a confirmed end
			// of the old Pod that is the master back; otherwise the old
			// process may still run beside it.
			confirmed := q.ObservedMasterLeavingConfirmed
			q.ObservedMasterLeavingPodUID, q.ObservedMasterLeavingConfirmed, q.ObservedMasterHoldUntil = "", false, nil
			if confirmed {
				return departureNone
			}
			return departureLost
		}
		switch {
		case q.ObservedMasterLeavingPodUID != "":
		case id.PodUID != "" && id.PodUID == q.ObservedMasterPodUID && id.Deleting:
			until := metav1.NewTime(in.Now.Add(min(id.DeletionGrace, quarantineHoldGraceCap) + quarantineStartBound))
			q.ObservedMasterLeavingPodUID, q.ObservedMasterHoldUntil = id.PodUID, &until
		case recorded(q.ObservedMasterPodUID) || (id.PodUID == q.ObservedMasterPodUID && id.Terminated):
			q.ObservedMasterLeavingPodUID = q.ObservedMasterPodUID
		case id.PodUID != q.ObservedMasterPodUID:
			// The recorded master's Pod is gone or replaced, and the
			// Operator neither saw it marked for deletion nor its end: it
			// may have been deleted with --force on a node cut off, its
			// process still running.
			return departureLost
		}
		leaving := q.ObservedMasterLeavingPodUID
		if leaving != "" && ((id.PodUID == leaving && id.Terminated) || recorded(leaving)) {
			q.ObservedMasterLeavingConfirmed = true
		}
	}
	switch {
	case q.ObservedMasterLeavingPodUID == "":
		return departureNone
	case q.ObservedMasterLeavingConfirmed:
		return departureConfirmed
	case q.ObservedMasterHoldUntil == nil || in.Now.After(q.ObservedMasterHoldUntil.Time):
		return departureLost
	case !in.IdentitiesUnknown && id.PodUID != q.ObservedMasterLeavingPodUID:
		return departureLost
	}
	return departureHeld
}

// releaseCheck advances the automatic release of the
// WriteQuarantinedMemberUnverified quarantine q by one pass, and returns the
// release when this pass completes it.
func releaseCheck(in quarantineInput, q *databasev1alpha1.WriteQuarantine, d quarantineDecision,
	verified map[string]bool) *databasev1alpha1.WriteQuarantineRelease {
	releasable := len(d.ActiveMasters) == 1 && d.ActiveMasters[0] == q.ObservedMaster
	for _, m := range in.Members {
		answeredConfirmed := slices.Contains(d.Confirmed, m) &&
			slices.ContainsFunc(d.Terminations, func(t databasev1alpha1.MemberTermination) bool {
				return t.Name == m && t.Answered
			})
		if m != q.ObservedMaster && !verified[m] && !answeredConfirmed {
			releasable = false
		}
	}
	if !releasable {
		q.ReleaseCheckSince, q.ReleaseCheckLastTime = nil, nil
		return nil
	}
	previous := q.ReleaseCheckLastTime
	q.ReleaseCheckLastTime = &metav1.Time{Time: in.Now}
	switch {
	case q.ReleaseCheckSince == nil || previous == nil || in.Now.Sub(previous.Time) > quarantineAnswerGap ||
		slices.ContainsFunc(d.Verifications, func(v databasev1alpha1.MemberVerification) bool {
			return v.NonMasterSince.After(q.ReleaseCheckSince.Time)
		}) || slices.ContainsFunc(d.Terminations, func(t databasev1alpha1.MemberTermination) bool {
		return t.ConfirmedAt.After(q.ReleaseCheckSince.Time)
	}):
		// The first releasable pass, a gap since the last one, or a member
		// verified anew since the first one: start over.
		since := ceilSecond(in.Now)
		q.ReleaseCheckSince = &since
	case in.Now.Sub(q.ReleaseCheckSince.Time) >= quarantineReleaseInterval:
		return &databasev1alpha1.WriteQuarantineRelease{GenerationID: q.GenerationID, Reason: q.Reason,
			StartTime: q.StartTime, ReleaseTime: metav1.NewTime(in.Now),
			Method: databasev1alpha1.WriteQuarantineReleasedAutomatically, Revision: q.Revision,
			ObservedMaster: q.ObservedMaster, Members: q.Members}
	}
	return nil
}

// Reasons of the write quarantine's Events and conditions other than the
// quarantine's own reasons.
const (
	reasonWriteQuarantineEnforced          = "WriteQuarantineEnforced"
	reasonWriteQuarantineReleased          = "WriteQuarantineReleased"
	reasonWriteQuarantineEnforcementFailed = "WriteQuarantineEnforcementFailed"
)

// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=list

// reasonReadWriteBrokersHeld is Ready's reason in a pass that keeps the
// read-write Brokers at zero without a quarantine in its status: the pass
// that released one, before the release is written.
const reasonReadWriteBrokersHeld = "ReadWriteBrokersHeld"

// reasonWriteQuarantineOtherCluster is the Warning Event of a stored
// quarantine recorded for another cluster UID.
const reasonWriteQuarantineOtherCluster = "WriteQuarantineClusterUIDMismatch"

// writesHeld reports whether the read-write Brokers are kept at zero in this
// pass: while a quarantine is in force; while this reconciler holds one it
// decided and has not seen released (quarantineHolds), whatever the status
// read says; on the pass that releases one, whose release is written to the
// status before the Brokers are started again.
func (r *CubridClusterReconciler) writesHeld(cluster *databasev1alpha1.CubridCluster) bool {
	if cluster.Status.WriteQuarantine != nil || r.heldQuarantine(cluster) != nil {
		return true
	}
	report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster))
	return ok && report.(*reconcileReport).before.WriteQuarantine != nil
}

// scaleUpHeld reports whether this pass keeps the read-write Brokers from
// scaling up, without stopping running ones: it could not read the members'
// Pods, so no quarantine could be decided, but none is known either.
func (r *CubridClusterReconciler) scaleUpHeld(cluster *databasev1alpha1.CubridCluster) bool {
	report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster))
	return ok && report.(*reconcileReport).noScaleUp
}

// quarantineHold is what a reconciler keeps in memory about one cluster's
// write quarantine (quarantineHolds).
type quarantineHold struct {
	cluster types.NamespacedName
	// quarantine is the last quarantine this reconciler decided or read,
	// nil once its release was written.
	quarantine *databasev1alpha1.WriteQuarantine
	// released is the generation whose release was written: a stale read of
	// it is not held again.
	released string
	// verifications are the member verifications of the last decision, with
	// the times of the answers to the nanosecond, which the status holds back
	// for up to observationWriteInterval.
	verifications []databasev1alpha1.MemberVerification
	// reportedUID is the generation whose other cluster UID was reported.
	reportedUID string
	// reportedRelease is the release annotation, with what was made of it,
	// last reported with an Event: an inert or refused annotation is
	// reported once.
	reportedRelease string
	// endedExemptions are, by the release token of the manual release whose
	// exemption they end, the members whose exemption this reconciler ended,
	// anyExemption for all of them. They may only end an exemption: a status
	// write that failed does not bring it back. A restart of the Operator
	// before the end is written loses them.
	endedExemptions map[string]map[string]bool
	// releaseWaits counts the consecutive passes that could not apply the
	// release annotation, for their back-off (releaseWaitRetry).
	releaseWaits int
}

// quarantineState is the cluster's entry in quarantineHolds.
func (r *CubridClusterReconciler) quarantineState(cluster *databasev1alpha1.CubridCluster) *quarantineHold {
	h, _ := r.quarantineHolds.LoadOrStore(cluster.UID, &quarantineHold{cluster: client.ObjectKeyFromObject(cluster)})
	return h.(*quarantineHold)
}

// heldQuarantine is the quarantine this reconciler holds for cluster in
// memory, nil for none. It can only keep the read-write path closed: a
// quarantine missing from it never opens the path while the status, or the
// API server, holds one.
func (r *CubridClusterReconciler) heldQuarantine(cluster *databasev1alpha1.CubridCluster) *databasev1alpha1.WriteQuarantine {
	h, ok := r.quarantineHolds.Load(cluster.UID)
	if !ok {
		return nil
	}
	return h.(*quarantineHold).quarantine
}

// holdQuarantine keeps q in memory, unless its release was written.
func (r *CubridClusterReconciler) holdQuarantine(cluster *databasev1alpha1.CubridCluster, q *databasev1alpha1.WriteQuarantine) {
	h := r.quarantineState(cluster)
	if q.GenerationID != h.released {
		h.quarantine = q.DeepCopy()
	}
}

// forgetQuarantines drops what is kept in memory for the deleted cluster key.
func (r *CubridClusterReconciler) forgetQuarantines(key types.NamespacedName) {
	for _, store := range []*sync.Map{&r.podTerminations, &r.podUnconfirmed} {
		store.Range(func(uid, t any) bool {
			if t.(podTermination).cluster == key {
				store.Delete(uid)
			}
			return true
		})
	}
	r.quarantineHolds.Range(func(uid, h any) bool {
		if h.(*quarantineHold).cluster == key {
			r.quarantineHolds.Delete(uid)
		}
		return true
	})
}

// restoreWriteQuarantine runs first in every pass. A quarantine the status
// read holds is kept in memory. A quarantine held in memory that the status
// read does not hold, because its status write failed or the cache is
// behind, is put back into the status, so that the pass enforces it and
// writes it again. A quarantine recorded for another cluster UID is reported
// once and enforced.
func (r *CubridClusterReconciler) restoreWriteQuarantine(ctx context.Context, cluster *databasev1alpha1.CubridCluster) {
	q := cluster.Status.WriteQuarantine
	switch held := r.heldQuarantine(cluster); {
	case q != nil:
		r.holdQuarantine(cluster, q)
	case held != nil:
		cluster.Status.WriteQuarantine = held.DeepCopy()
		q = cluster.Status.WriteQuarantine
		logf.FromContext(ctx).Info("Kept a write quarantine the stored status does not hold",
			"reason", string(q.Reason), "generationID", q.GenerationID, "revision", q.Revision)
		r.statusEvent(cluster, corev1.EventTypeWarning, string(q.Reason),
			"kept the read-write Brokers stopped for a quarantine whose status was not written or not read back yet: "+
				quarantineDescription(q, nil))
	default:
		return
	}
	if h := r.quarantineState(cluster); q.ClusterUID != "" && q.ClusterUID != string(cluster.UID) &&
		h.reportedUID != q.GenerationID {
		h.reportedUID = q.GenerationID
		r.event(cluster, corev1.EventTypeWarning, reasonWriteQuarantineOtherCluster, fmt.Sprintf(
			"the stored write quarantine %s was recorded for the cluster UID %s, not %s; it is enforced and is not "+
				"released automatically", q.GenerationID, q.ClusterUID, cluster.UID))
	}
}

// storedQuarantine reads the cluster from the API server, past the cache,
// before the read-write Brokers are asked for. A stored quarantine keeps them
// at zero (zero). A failed read, or a stored cluster of another UID, shows no
// quarantine either way: running Brokers are left running and stopped ones
// are not started in this pass (noScaleUp).
func (r *CubridClusterReconciler) storedQuarantine(ctx context.Context, cluster *databasev1alpha1.CubridCluster) (zero, noScaleUp bool) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	stored := &databasev1alpha1.CubridCluster{}
	err := reader.Get(ctx, client.ObjectKeyFromObject(cluster), stored)
	switch {
	case err != nil:
		logf.FromContext(ctx).Info("Did not scale the read-write Brokers up: the stored cluster could not be read",
			"reason", err.Error())
		return false, true
	case stored.UID != cluster.UID:
		logf.FromContext(ctx).Info("Did not scale the read-write Brokers up: the stored cluster has another UID",
			"storedUID", string(stored.UID))
		return false, true
	case stored.Status.WriteQuarantine != nil:
		logf.FromContext(ctx).Info("Kept the read-write Brokers at zero for the stored write quarantine",
			"generationID", stored.Status.WriteQuarantine.GenerationID)
		return true, false
	}
	return false, false
}

// enforceWriteQuarantine keeps the read-write Broker Deployment of a cluster
// with a quarantine in force at zero replicas, whatever changed it, and
// records the quarantine as enforced only once no read-write Broker Pod
// exists and the read-write Service's EndpointSlices hold no endpoint. Until
// then, or when either is seen again, enforcedTime is unset: the quarantine
// is pending, never reported as enforced. It acts on persisted status alone,
// so it runs first in every pass, the first after a restart of the Operator
// included. It touches no database member, read-only Broker or Service. A
// Deployment the cluster does not control is not changed; its Pods keep the
// quarantine pending.
func (r *CubridClusterReconciler) enforceWriteQuarantine(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	q := cluster.Status.WriteQuarantine
	if q == nil {
		return nil
	}
	dep := &appsv1.Deployment{}
	key := client.ObjectKey{Name: brokerDeploymentName(cluster.Name, brokerModeRW), Namespace: cluster.Namespace}
	err := r.childClient().Get(ctx, key, dep)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("read the read-write Broker Deployment %s: %w", key.Name, err)
	case metav1.IsControlledBy(dep, cluster) && (dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0):
		dep.Spec.Replicas = new(int32(0))
		if err := r.Update(ctx, dep); err != nil {
			return fmt.Errorf("scale the read-write Broker Deployment %s to zero: %w", key.Name, err)
		}
		logf.FromContext(ctx).Info("Scaled the read-write Broker Deployment to zero for the write quarantine",
			"deployment", key.Name, "reason", string(q.Reason), "generationID", q.GenerationID)
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(cluster.Namespace),
		client.MatchingLabels(brokerPodLabels(cluster, brokerModeRW))); err != nil {
		return fmt.Errorf("list the read-write Broker Pods: %w", err)
	}
	var endpointSlices discoveryv1.EndpointSliceList
	if err := reader.List(ctx, &endpointSlices, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: rwServiceName(cluster.Name)}); err != nil {
		return fmt.Errorf("list the EndpointSlices of Service %s: %w", rwServiceName(cluster.Name), err)
	}
	endpoints := 0
	for _, s := range endpointSlices.Items {
		endpoints += len(s.Endpoints)
	}
	if len(pods.Items) > 0 || endpoints > 0 {
		if q.EnforcedTime != nil {
			logf.FromContext(ctx).Info("Found read-write Brokers of a quarantined cluster again",
				"pods", len(pods.Items), "endpoints", endpoints, "generationID", q.GenerationID)
		}
		q.EnforcedTime = nil
		return nil
	}
	if q.EnforcedTime == nil {
		at := metav1.NewTime(r.now())
		q.EnforcedTime = &at
		r.statusEvent(cluster, corev1.EventTypeWarning, reasonWriteQuarantineEnforced, fmt.Sprintf(
			"no read-write Broker Pod exists and Service %s has no endpoint: the managed read-write path is closed (%s)",
			rwServiceName(cluster.Name), q.Reason))
	}
	return nil
}

// memberIdentities reads the Pod of each member from the API server, past
// the cache. A member without a Pod has the empty identity.
func (r *CubridClusterReconciler) memberIdentities(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	members []string) (map[string]memberIdentity, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	ids := make(map[string]memberIdentity, len(members))
	for _, m := range members {
		var pod corev1.Pod
		err := reader.Get(ctx, client.ObjectKey{Name: m, Namespace: cluster.Namespace}, &pod)
		if apierrors.IsNotFound(err) {
			ids[m] = memberIdentity{}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read Pod %s: %w", m, err)
		}
		id := memberIdentity{PodUID: string(pod.UID), Terminated: podTerminated(&pod), Created: pod.CreationTimestamp.Time,
			Deleting: pod.DeletionTimestamp != nil, DeletionGrace: deletionGrace(&pod),
			TerminatedAt: terminatedAt(&pod), DeletionRequested: deletionRequested(&pod)}
		for _, c := range pod.Status.ContainerStatuses {
			if c.Name == appName {
				id.RestartCount = c.RestartCount
			}
		}
		ids[m] = id
	}
	return ids, nil
}

// reconcileWriteQuarantine decides the read-write quarantine from this pass's
// observations (decideWriteQuarantine), puts the outcome in the status and in
// memory, reports a start, a revision and a release with an Event, and
// enforces a quarantine in force. lastResolved is status.lastResolvedPrimary
// before this pass; before are the members' Pods read before the probes, or
// the error of that read. A member whose Pod differs in the read after the
// probes verifies nothing in this pass. When either read failed, only the
// rule of two active masters is applied and the read-write Brokers are not
// scaled up in the pass; running ones keep running. A failure leaves the quarantine in the status as it
// was, so that the read-write Brokers stay at zero, and is returned for a
// retry.
func (r *CubridClusterReconciler) reconcileWriteQuarantine(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	members []string, obs map[string]RoleObservation, now time.Time, lastResolved string,
	before map[string]memberIdentity, beforeErr error) stepResult {
	steps := stepResult{}
	ids, err := r.memberIdentities(ctx, cluster, members)
	unknown := errors.Join(beforeErr, err)
	unstable := map[string]bool{}
	for _, m := range members {
		if unknown == nil && before[m] != ids[m] {
			unstable[m] = true
		}
	}
	if unknown != nil {
		logf.FromContext(ctx).Error(unknown, "Did not scale the read-write Brokers up: the members' Pods could not be read")
		if report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster)); ok {
			report.(*reconcileReport).noScaleUp = true
		}
		steps.err = fmt.Errorf("read the members' Pods for the write quarantine: %w", unknown)
	}
	state := r.quarantineState(cluster)
	terminations := cluster.Status.MemberTerminations
	if unknown == nil {
		terminations = r.recordTerminations(ctx, cluster, members, before, ids)
	}
	d, err := decideWriteQuarantine(quarantineInput{
		Now: now, ClusterUID: string(cluster.UID), Members: members, Observations: obs, Identities: ids,
		Unstable: unstable, IdentitiesUnknown: unknown != nil, LastResolvedPrimary: lastResolved,
		Terminations:    terminations,
		Verifications:   withAnswerTimes(cluster.Status.MemberVerifications, state.verifications),
		Quarantine:      withHeldDeparture(cluster.Status.WriteQuarantine, r.heldQuarantine(cluster)),
		LastRelease:     cluster.Status.LastWriteQuarantineRelease,
		EndedExemptions: state.endedExemptions[releaseTokenOf(cluster.Status.LastWriteQuarantineRelease)],
		Nonce:           newQuarantineNonce,
	})
	if err != nil {
		return stepResult{err: errors.Join(steps.err, fmt.Errorf("decide the write quarantine: %w", err))}
	}
	state.verifications = d.Verifications
	var releaseRetry time.Duration
	d, releaseRetry = r.reconcileManualRelease(ctx, cluster, d, ids, unknown != nil, now)
	cluster.Status.MemberVerifications = d.Verifications
	cluster.Status.MemberTerminations = d.Terminations
	if d.Released == nil {
		before := cluster.Status.LastWriteQuarantineRelease
		cluster.Status.LastWriteQuarantineRelease = d.LastRelease
		state.rememberEndedExemptions(before, d.LastRelease)
		for _, msg := range d.ExemptionEnded {
			logf.FromContext(ctx).Info("Ended the exemption of a manual write quarantine release",
				"event", "write_quarantine_exemption_ended", "detail", msg)
			r.statusEvent(cluster, corev1.EventTypeNormal, reasonWriteQuarantineExemptionEnded, msg)
		}
	}
	cluster.Status.WriteQuarantine = d.Quarantine
	if d.Quarantine != nil {
		r.holdQuarantine(cluster, d.Quarantine)
	}
	log := logf.FromContext(ctx)
	switch d.Change {
	case quarantineStarted, quarantineRevised:
		q := d.Quarantine
		log.Info("Quarantined the read-write path", "event", "write_quarantine_"+string(d.Change),
			"reason", string(q.Reason), "generationID", q.GenerationID, "revision", q.Revision,
			"members", quarantineMemberText(q.Members), "observedMaster", q.ObservedMaster)
		r.statusEvent(cluster, corev1.EventTypeWarning, string(q.Reason),
			"stopped the read-write Brokers: "+quarantineDescription(q, d.ActiveMasters))
	case quarantineReleased:
		rel := d.Released
		cluster.Status.LastWriteQuarantineRelease = rel
		// The memory lets the path open only once the release is written.
		if report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster)); ok {
			report.(*reconcileReport).releasedQuarantine = rel.GenerationID
		}
		log.Info("Released the write quarantine", "event", "write_quarantine_released", "reason", string(rel.Reason),
			"generationID", rel.GenerationID, "method", string(rel.Method), "observedMaster", rel.ObservedMaster)
		if rel.Method == databasev1alpha1.WriteQuarantineReleasedManually {
			r.statusEvent(cluster, corev1.EventTypeWarning, reasonWriteQuarantineReleasedManually, fmt.Sprintf(
				"released the %s quarantine (generation %s, revision %d) on the annotation %s with the token %s, "+
					"accepting the members %s; %s was the only active master; %s; the read-write Brokers start again",
				rel.Reason, rel.GenerationID, rel.Revision, releaseWriteQuarantineAnnotation, tokenPrefix(rel.ReleaseToken),
				quarantineMemberText(rel.Members), rel.ObservedMaster, rel.Warning))
			break
		}
		r.statusEvent(cluster, corev1.EventTypeNormal, reasonWriteQuarantineReleased, fmt.Sprintf(
			"released the %s quarantine started at %s automatically: %s was the only active master and every other "+
				"member answered as no active master in two observations %s apart; the read-write Brokers start again",
			rel.Reason, rel.StartTime.UTC().Format(time.RFC3339), rel.ObservedMaster, quarantineReleaseInterval))
	}
	steps.retry = quarantineRetry(d.Released != nil, releaseRetry,
		d.Quarantine != nil || unknown != nil || (len(d.Unverified) > 0 && cluster.Status.LastResolvedPrimary != ""))
	if err := r.enforceWriteQuarantine(ctx, cluster); err != nil {
		steps.err = errors.Join(steps.err, err)
	}
	if q := cluster.Status.WriteQuarantine; q != nil {
		r.holdQuarantine(cluster, q)
	}
	return steps
}

// releaseTokenOf is the release token of rel, "" for none.
func releaseTokenOf(rel *databasev1alpha1.WriteQuarantineRelease) string {
	if rel == nil {
		return ""
	}
	return rel.ReleaseToken
}

// rememberEndedExemptions keeps in memory the exemptions of the manual
// release before that this pass ended in after, before the status that
// records it is written.
func (h *quarantineHold) rememberEndedExemptions(before, after *databasev1alpha1.WriteQuarantineRelease) {
	if before == nil || before.ReleaseToken == "" || len(before.ExemptMembers) == 0 {
		return
	}
	var kept []string
	if after != nil && after.ReleaseToken == before.ReleaseToken {
		kept = after.ExemptMembers
	}
	if len(kept) == len(before.ExemptMembers) {
		return
	}
	if h.endedExemptions == nil {
		h.endedExemptions = map[string]map[string]bool{}
	}
	ended := h.endedExemptions[before.ReleaseToken]
	if ended == nil {
		// Only the last manual release's exemptions are kept.
		ended = map[string]bool{}
		h.endedExemptions = map[string]map[string]bool{before.ReleaseToken: ended}
	}
	if len(kept) == 0 {
		ended[anyExemption] = true
		return
	}
	for _, m := range before.ExemptMembers {
		if !slices.Contains(kept, m) {
			ended[m] = true
		}
	}
}

// quarantineRetry is how soon the pass is retried for the write quarantine,
// zero for the usual cadence: a second after a release, so that the next pass
// starts the read-write Brokers; unverifiedResyncInterval while a quarantine
// is in force or a member is unverified (watchful); releaseRetry after a
// release annotation that could not be applied, but never later than a
// watchful pass would be.
func quarantineRetry(released bool, releaseRetry time.Duration, watchful bool) time.Duration {
	switch {
	case released:
		return time.Second
	case releaseRetry > 0 && watchful:
		return min(releaseRetry, unverifiedResyncInterval)
	case releaseRetry > 0:
		return releaseRetry
	case watchful:
		return unverifiedResyncInterval
	}
	return 0
}

// releaseWritten lets the read-write path open after the status that
// records the release of generation was written.
func (r *CubridClusterReconciler) releaseWritten(cluster *databasev1alpha1.CubridCluster, generation string) {
	h := r.quarantineState(cluster)
	h.released = generation
	if h.quarantine != nil && h.quarantine.GenerationID == generation {
		h.quarantine = nil
	}
}

// withAnswerTimes is stored with the time of each member's latest
// non-master answer taken from kept, this reconciler's last decision, where
// kept records the same answers: the status holds that time back for up to
// observationWriteInterval. stored is not changed.
func withAnswerTimes(stored, kept []databasev1alpha1.MemberVerification) []databasev1alpha1.MemberVerification {
	out := make([]databasev1alpha1.MemberVerification, len(stored))
	for i, v := range stored {
		out[i] = *v.DeepCopy()
		for _, k := range kept {
			if k.Name == v.Name && k.PodUID == v.PodUID && k.RestartCount == v.RestartCount &&
				k.NonMasterSince.Equal(&v.NonMasterSince) && k.LastNonMasterAt != nil &&
				(v.LastNonMasterAt == nil || k.LastNonMasterAt.After(v.LastNonMasterAt.Time)) {
				out[i].LastNonMasterAt = k.LastNonMasterAt.DeepCopy()
			}
		}
	}
	return out
}

// quarantineMemberText names the members and what was observed of them.
func quarantineMemberText(members []databasev1alpha1.WriteQuarantineMember) string {
	parts := make([]string, 0, len(members))
	for _, m := range members {
		parts = append(parts, fmt.Sprintf("%s (%s)", m.Name, m.ObservedRole))
	}
	return strings.Join(parts, ", ")
}

// quarantineDescription says why the quarantine q is in force, for its Event
// and conditions; masters are the active masters of the pass, when known.
func quarantineDescription(q *databasev1alpha1.WriteQuarantine, masters []string) string {
	var why string
	switch q.Reason {
	case databasev1alpha1.WriteQuarantinedMultipleMasters:
		why = "more than one active master: " + quarantineMemberText(q.Members)
	case databasev1alpha1.WriteQuarantinedMemberUnverified:
		why = fmt.Sprintf("%s is the only active master, and not verified as no active master: %s",
			q.ObservedMaster, quarantineMemberText(q.Members))
	default:
		why = fmt.Sprintf("the master changed during a quarantine started beside %s; members: %s",
			q.ObservedMaster, quarantineMemberText(q.Members))
	}
	if masters != nil {
		why += "; active masters now: " + strings.Join(masters, ", ")
	}
	return fmt.Sprintf("%s (generation %s, revision %d, since %s)", why, q.GenerationID, q.Revision,
		q.StartTime.UTC().Format(time.RFC3339))
}

// quarantineConditionMessage is the message of Ready and RoutingReady while q
// is in force.
func quarantineConditionMessage(q *databasev1alpha1.WriteQuarantine) string {
	enforced := "stopping the read-write Brokers is not verified yet: a read-write Broker Pod or endpoint may remain"
	if q.EnforcedTime != nil {
		enforced = "no read-write Broker Pod or endpoint remains since " + q.EnforcedTime.UTC().Format(time.RFC3339)
	}
	msg := "the read-write path is quarantined: " + quarantineDescription(q, nil) + "; " + enforced
	if q.ReleaseRefusal != "" {
		msg += "; the manual release was refused: " + q.ReleaseRefusal
	}
	return msg
}

// setRoutingQuarantined sets RoutingReady=False with the quarantine's reason
// and reports whether a quarantine is in force.
func setRoutingQuarantined(cluster *databasev1alpha1.CubridCluster) bool {
	q := cluster.Status.WriteQuarantine
	if q == nil {
		return false
	}
	setCondition(cluster, conditionRoutingReady, metav1.ConditionFalse, string(q.Reason), quarantineConditionMessage(q))
	return true
}
