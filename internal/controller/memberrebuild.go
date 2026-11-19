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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// rebuildMemberAnnotation on a CubridCluster asks for the rebuild of one
// member's replacement storage from the current primary (#117). Its value is
// "<member>:<token>", where token is the member's current token in
// status.memberRebuildOffers. For the offer of the last resolved primary
// (formerPrimary) it is "<member>:<token>:<source>": naming the source
// states that the source's data becomes the member's data (ADR-0006).
const rebuildMemberAnnotation = "database.cubrid.io/rebuild-member"

// The Events of a rebuild request.
const (
	reasonMemberRebuildAccepted  = "MemberRebuildAccepted"
	reasonMemberRebuildIgnored   = "MemberRebuildIgnored"
	reasonMemberRebuildRefused   = "MemberRebuildRefused"
	reasonMemberRebuildWithdrawn = "MemberRebuildWithdrawn"
)

// claimIdentity is a member's data PersistentVolumeClaim as one pass read
// it; the zero value for a member without one.
type claimIdentity struct {
	UID      string
	Created  time.Time
	Deleting bool
	// VolumeName is the PersistentVolume the claim is bound to, empty while
	// it is not bound.
	VolumeName string
}

// rebuildInput is what one pass knows for the rebuild of a member.
type rebuildInput struct {
	// Now is the time of the pass.
	Now time.Time
	// Members are the promotable members.
	Members []string
	// Holders are the members recorded as holding the first database: the
	// member the HA bootstrap created or restored it on, and seededMembers.
	Holders []string
	// BootstrapDone says that the HA bootstrap copied the first database to
	// every member (haConfigured).
	BootstrapDone bool
	// CurrentPrimary is the primary this pass resolved, "" for none. A
	// member that lost its storage gives no role, so no primary is resolved
	// while it does not: the source of its rebuild is found among the other
	// members (rebuildSource).
	CurrentPrimary string
	// LastResolvedPrimary is status.lastResolvedPrimary before this pass.
	LastResolvedPrimary string
	// Quarantine is the write quarantine in force after this pass's
	// decision, nil for none.
	Quarantine *databasev1alpha1.WriteQuarantine
	// Identities are the members' Pods read after the probes in this pass.
	Identities map[string]memberIdentity
	// Unstable are the members whose Pod UID differs between the reads
	// before and after the probes: their answers count for nothing.
	Unstable map[string]bool
	// Claims are the members' data claims in this pass.
	Claims map[string]claimIdentity
	// Unknown says that the members' Pods or claims could not be read.
	Unknown bool
	// Observations are this pass's role answers.
	Observations map[string]RoleObservation
	// DataClaims are the claims the members were seen running the database
	// on.
	DataClaims []databasev1alpha1.MemberDataClaim
	// Offers are the offers in the status this pass started from.
	Offers []databasev1alpha1.MemberRebuildOffer
	// Active is the accepted rebuild, nil for none.
	Active *databasev1alpha1.MemberRebuild
	// Nonce returns a new random value of at least 128 bits.
	Nonce func() (string, error)
}

// rebuildVerdict is what decideRebuildRequest made of the annotation.
type rebuildVerdict string

const (
	// rebuildNone: no annotation.
	rebuildNone rebuildVerdict = ""
	// rebuildInert: the annotation names no current token of its member.
	rebuildInert rebuildVerdict = "inert"
	// rebuildConsumed: the annotation names the token of the accepted or the
	// last rebuild; it is removed.
	rebuildConsumed rebuildVerdict = "consumed"
	// rebuildWait: the stored cluster could not be read, or this pass decided
	// from another offer than the stored one; it is tried again.
	rebuildWait rebuildVerdict = "wait"
	// rebuildRefused: the named member cannot be rebuilt in this pass.
	rebuildRefused rebuildVerdict = "refused"
	// rebuildAccepted: the rebuild is accepted.
	rebuildAccepted rebuildVerdict = "accepted"
)

// runsDatabase reports whether o is a fresh, consistent answer of a member
// whose database runs: a master or a slave.
func runsDatabase(o RoleObservation, now time.Time) bool {
	return fresh(o, now) && !o.Conflicting &&
		(o.Role == databasev1alpha1.RoleMaster || o.Role == databasev1alpha1.RoleSlave)
}

// answer is in.Observations[m], or no answer for a member whose Pod UID
// changed between the reads before and after the probes: the answer may come
// from either Pod.
func (in rebuildInput) answer(m string) RoleObservation {
	if in.Unstable[m] {
		return RoleObservation{}
	}
	return in.Observations[m]
}

// recordDataClaims is in.DataClaims with the claim, and the volume it is
// bound to, of every member whose database runs in this pass: a member
// answering as a master or a slave runs on the claim its Pod mounts, which is
// the claim of its name that existed when the Pod was created. Nothing is
// recorded when the Pods or claims could not be read, nor from a member whose
// Pod changed during the observation.
func recordDataClaims(in rebuildInput) []databasev1alpha1.MemberDataClaim {
	out := slices.Clone(in.DataClaims)
	if in.Unknown {
		return out
	}
	for _, m := range in.Members {
		id, claim := in.Identities[m], in.Claims[m]
		if id.PodUID == "" || claim.UID == "" || claim.Deleting || claim.Created.After(id.Created) ||
			!runsDatabase(in.answer(m), in.Now) {
			continue
		}
		record := databasev1alpha1.MemberDataClaim{Name: m, ClaimUID: claim.UID, VolumeName: claim.VolumeName}
		if i := slices.IndexFunc(out, func(c databasev1alpha1.MemberDataClaim) bool { return c.Name == m }); i < 0 {
			out = append(out, record)
		} else {
			out[i] = record
		}
	}
	return out
}

// rebuildRefusal says why member cannot be rebuilt in this pass, "" when it
// can. A rebuild discards what the member's storage holds and copies the
// database from the current primary, so it is refused while a write
// quarantine is in force, for the current primary, without exactly one other
// active master, and for a member whose claim and volume are not shown to be a
// replacement that never held the database (ADR-0005). The Operator never
// deletes a claim: an administrator provides the replacement. The last
// resolved primary is not refused here; its request must name the source
// (decideRebuildRequest, ADR-0006).
func rebuildRefusal(in rebuildInput, member string) string {
	switch {
	case !slices.Contains(in.Members, member):
		return fmt.Sprintf("%s is not a promotable member of this cluster", member)
	case in.Unknown:
		return "the members' Pods or data claims could not be read in this observation"
	case !in.BootstrapDone:
		return "the HA bootstrap has not copied the database to every member yet; it seeds a member that does " +
			"not hold the database without a request"
	case !slices.Contains(in.Holders, member) && !slices.ContainsFunc(in.DataClaims,
		func(c databasev1alpha1.MemberDataClaim) bool { return c.Name == member }):
		return fmt.Sprintf("%s is not recorded as holding the database (status.databases[].seededMembers or "+
			"status.memberDataClaims); a member that never held it is seeded by the HA bootstrap", member)
	case in.Quarantine != nil:
		return fmt.Sprintf("a write quarantine (%s) is in force; a rebuild does not pick between members' data "+
			"while it is, and is offered again once it is released", in.Quarantine.Reason)
	case in.Active != nil:
		return fmt.Sprintf("the rebuild of %s (operation %s) is in progress; one member is rebuilt at a time",
			in.Active.Member, in.Active.OperationID)
	case member == in.CurrentPrimary || isMasterAnswer(in.Observations[member], in.Now):
		return fmt.Sprintf("%s is the current primary or reports itself master; its data is never discarded", member)
	}
	source, why := rebuildSource(in, member)
	switch {
	case source == "":
		return "no healthy primary is observed among the other members (" + why + "); a rebuild copies the " +
			"database from the current healthy primary only"
	case in.Identities[member].PodUID == "":
		return fmt.Sprintf("%s has no Pod", member)
	case in.Identities[source].PodUID == "":
		return fmt.Sprintf("the Pod of the primary %s could not be read", source)
	}
	claim := in.Claims[member]
	switch {
	case claim.UID == "":
		return fmt.Sprintf("%s has no data claim %s-%s", member, dataVolume, member)
	case claim.Deleting:
		return fmt.Sprintf("the data claim %s-%s of %s is being deleted", dataVolume, member, member)
	case claim.VolumeName == "":
		return fmt.Sprintf("the data claim %s-%s of %s is not bound to a volume yet", dataVolume, member, member)
	case claim.Created.After(in.Identities[member].Created):
		return fmt.Sprintf("the data claim %s-%s of %s was created after its Pod, which does not run on it; "+
			"the member's next Pod does", dataVolume, member, member)
	}
	recorded := ""
	for _, c := range in.DataClaims {
		if c.Name == member {
			recorded = c.ClaimUID
		}
		if c.VolumeName == claim.VolumeName {
			return fmt.Sprintf("the data claim of %s is bound to the volume %s, which %s was seen running the "+
				"database on; its data is not shown expendable. A retained volume bound to a new claim is not a "+
				"replacement", member, claim.VolumeName, c.Name)
		}
	}
	switch recorded {
	case "":
		return fmt.Sprintf("no data claim of %s was recorded running the database (status.memberDataClaims), so its "+
			"current claim %s cannot be shown to be a replacement; its data is not shown expendable", member, claim.UID)
	case claim.UID:
		return fmt.Sprintf("%s runs on the data claim %s, the claim it was seen running the database on; its data "+
			"is not shown expendable. Replace the claim with an empty one first: the Operator never deletes a claim",
			member, claim.UID)
	}
	return ""
}

// rebuildSource is the member the database of member would be copied from
// in this pass: among the other members, the only one that answers as an
// active master while every other one answers as a slave, freshly and
// consistently. With a member that does not answer so, no master or two,
// there is none, and why says what was seen. A member whose Pod changed
// during the observation counts as one that did not answer.
func rebuildSource(in rebuildInput, member string) (source, why string) {
	var masters []string
	for _, m := range in.Members {
		o := in.answer(m)
		switch {
		case m == member:
		case in.Unstable[m]:
			return "", "the Pod of " + m + " changed during the observation"
		case !fresh(o, in.Now):
			return "", m + " did not answer"
		case isActiveMasterAnswer(o, in.Now):
			masters = append(masters, m)
		case o.Role == databasev1alpha1.RoleSlave && !o.Conflicting:
		default:
			return "", m + " reports no role that can be relied on"
		}
	}
	switch len(masters) {
	case 0:
		return "", "no other member reports itself an active master"
	case 1:
		return masters[0], ""
	default:
		return "", strings.Join(masters, " and ") + " both report themselves master"
	}
}

// rebuildOffer is the offer for member in this pass without its token.
func rebuildOffer(in rebuildInput, member string) databasev1alpha1.MemberRebuildOffer {
	source, _ := rebuildSource(in, member)
	return databasev1alpha1.MemberRebuildOffer{Member: member, PodUID: in.Identities[member].PodUID,
		ClaimUID: in.Claims[member].UID, VolumeName: in.Claims[member].VolumeName, Source: source,
		SourcePodUID: in.Identities[source].PodUID, FormerPrimary: member == in.LastResolvedPrimary}
}

// sameBinding reports whether a and b offer the rebuild of the same member's
// Pod and claim from the same source Pod.
func sameBinding(a, b databasev1alpha1.MemberRebuildOffer) bool {
	a.Token, b.Token = "", ""
	return a == b
}

// rebuildOffers are the offers of this pass: one for every member that can be
// rebuilt (rebuildRefusal), with the token of the offer in in.Offers when it
// binds the same member Pod and claim, source and source Pod, and a new token
// otherwise. When the Pods or claims could not be read, in.Offers is kept.
func rebuildOffers(in rebuildInput) ([]databasev1alpha1.MemberRebuildOffer, error) {
	if in.Unknown {
		return slices.Clone(in.Offers), nil
	}
	var out []databasev1alpha1.MemberRebuildOffer
	for _, m := range in.Members {
		if rebuildRefusal(in, m) != "" {
			continue
		}
		offer := rebuildOffer(in, m)
		if i := slices.IndexFunc(in.Offers, func(o databasev1alpha1.MemberRebuildOffer) bool {
			return sameBinding(o, offer)
		}); i >= 0 {
			offer.Token = in.Offers[i].Token
		} else {
			token, err := in.Nonce()
			if err != nil {
				return nil, err
			}
			offer.Token = token
		}
		out = append(out, offer)
	}
	return out, nil
}

// rebuildWithdrawal says why the accepted rebuild in.Active no longer
// describes what the administrator approved, "" while it does: a write
// quarantine is in force, the member answers as a master, its Pod, claim or
// volume changed, it became the last resolved primary although the request
// did not name the source, it could no longer be offered a rebuild
// (rebuildRefusal), or the primary or the primary's Pod changed. A pass that could not read
// the Pods or claims keeps it, and so does one that observed no healthy
// primary among the other members while nothing else refuses it.
func rebuildWithdrawal(in rebuildInput) string {
	a := in.Active
	if a == nil || in.Unknown {
		return ""
	}
	in.Active = nil
	claim := in.Claims[a.Member]
	source, _ := rebuildSource(in, a.Member)
	switch {
	case in.Quarantine != nil:
		return fmt.Sprintf("a write quarantine (%s) is in force", in.Quarantine.Reason)
	case isMasterAnswer(in.Observations[a.Member], in.Now):
		return fmt.Sprintf("%s reports itself master", a.Member)
	case in.Identities[a.Member].PodUID != a.PodUID:
		return fmt.Sprintf("the Pod of %s changed since the request was accepted", a.Member)
	case claim.UID != a.ClaimUID:
		return fmt.Sprintf("the data claim of %s changed since the request was accepted", a.Member)
	case claim.VolumeName != a.VolumeName:
		return fmt.Sprintf("the volume of the data claim of %s changed since the request was accepted", a.Member)
	case source == "":
		return ""
	case a.Member == in.LastResolvedPrimary && !a.FormerPrimary:
		return fmt.Sprintf("%s became the last resolved primary; it may hold writes no other member has, and the "+
			"request did not name the source whose data it would get", a.Member)
	}
	if why := rebuildRefusal(in, a.Member); why != "" {
		return why
	}
	switch {
	case source != a.Source:
		return fmt.Sprintf("the primary is %s, not %s, the source the request was accepted with", source, a.Source)
	case source != "" && in.Identities[a.Source].PodUID != a.SourcePodUID:
		return fmt.Sprintf("the Pod of the primary %s changed since the request was accepted", a.Source)
	}
	return ""
}

// decideRebuildRequest decides on the rebuild annotation of the stored
// cluster. It returns the verdict, a message for an inert or refused
// annotation, and the accepted rebuild.
//
// The token is compared with the stored status, never with memory or the
// cache, which may hold an earlier revision. The token of the accepted or
// the last rebuild is consumed. A named member that cannot be rebuilt in this
// pass is refused. A token that is not the stored offer's current token for
// the named member, or whose offer no longer binds this pass's member Pod and
// claim, volume, source and source Pod, is inert. The request for the offer of
// the last resolved primary must name the offer's source as a third field and
// is refused without it; a third field naming another member, or on any other
// offer, is inert. A pass that decided from an older offer than the stored one
// waits.
func decideRebuildRequest(in rebuildInput, clusterUID string, stored *databasev1alpha1.CubridCluster,
	offers []databasev1alpha1.MemberRebuildOffer) (rebuildVerdict, string, *databasev1alpha1.MemberRebuild, error) {
	if stored == nil || string(stored.UID) != clusterUID {
		return rebuildWait, "", nil, nil
	}
	value := stored.Annotations[rebuildMemberAnnotation]
	if value == "" {
		return rebuildNone, "", nil, nil
	}
	member, token, source, named, ok := splitRebuildValue(value)
	if !ok {
		return rebuildInert, "the value must be <member>:<token>, or <member>:<token>:<source> for the last " +
			"resolved primary, with the member's token from status.memberRebuildOffers", nil, nil
	}
	for _, done := range []*databasev1alpha1.MemberRebuild{stored.Status.MemberRebuild, stored.Status.LastMemberRebuild} {
		if done != nil && done.Token == token {
			return rebuildConsumed, "", nil, nil
		}
	}
	if why := rebuildRefusal(in, member); why != "" {
		return rebuildRefused, why, nil, nil
	}
	i := slices.IndexFunc(stored.Status.MemberRebuildOffers, func(o databasev1alpha1.MemberRebuildOffer) bool {
		return o.Member == member
	})
	if i < 0 || stored.Status.MemberRebuildOffers[i].Token != token {
		return rebuildInert, fmt.Sprintf("the value is not the current rebuild token of %s; read "+
			"status.memberRebuildOffers again after checking that %s's data claim is the empty replacement",
			member, member), nil, nil
	}
	so := stored.Status.MemberRebuildOffers[i]
	current := rebuildOffer(in, member)
	if !sameBinding(so, current) {
		msg := fmt.Sprintf("the Pod or data claim of %s, the primary or the primary's Pod changed since the token "+
			"was published; the annotation is ignored, and a new token is published", member)
		return rebuildInert, msg, nil, nil
	}
	switch {
	case so.FormerPrimary && !named:
		return rebuildRefused, fmt.Sprintf("%s is the last resolved primary and may have acknowledged writes %s "+
			"lacks; name the source to accept that %s's data becomes %s's data: set the value to %s:<token>:%s",
			member, so.Source, so.Source, member, member, so.Source), nil, nil
	case so.FormerPrimary && source != so.Source:
		return rebuildInert, fmt.Sprintf("the value names %q, not the source %s of the rebuild of %s",
			source, so.Source, member), nil, nil
	case !so.FormerPrimary && named:
		return rebuildInert, fmt.Sprintf("a source is named only for the last resolved primary; the value for %s "+
			"must be <member>:<token>", member), nil, nil
	}
	if !slices.Contains(offers, so) {
		// Decided from a cache behind the stored status.
		return rebuildWait, "", nil, nil
	}
	operation, err := in.Nonce()
	if err != nil {
		return rebuildNone, "", nil, err
	}
	return rebuildAccepted, "", &databasev1alpha1.MemberRebuild{Member: member, PodUID: so.PodUID,
		ClaimUID: so.ClaimUID, VolumeName: so.VolumeName, Source: so.Source, SourcePodUID: so.SourcePodUID, Token: token,
		FormerPrimary: so.FormerPrimary, OperationID: operation, AcceptedTime: metav1.NewTime(in.Now.UTC()),
		State: databasev1alpha1.MemberRebuildAccepted}, nil
}

// splitRebuildValue splits the value of the rebuild annotation into the
// member, the token and, when named says a third field is present, the
// source. ok is false when the member or the token is empty, or there are
// more than three fields.
func splitRebuildValue(value string) (member, token, source string, named, ok bool) {
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false, false
	}
	if len(parts) == 3 {
		source, named = parts[2], true
	}
	return parts[0], parts[1], source, named, true
}

// memberClaims reads the data claim of each member from the API server, past
// the cache. A member without one has the empty claim.
func (r *CubridClusterReconciler) memberClaims(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	members []string) (map[string]claimIdentity, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	claims := make(map[string]claimIdentity, len(members))
	for _, m := range members {
		var pvc corev1.PersistentVolumeClaim
		err := reader.Get(ctx, client.ObjectKey{Name: dataVolume + "-" + m, Namespace: cluster.Namespace}, &pvc)
		if apierrors.IsNotFound(err) {
			claims[m] = claimIdentity{}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read PersistentVolumeClaim %s-%s: %w", dataVolume, m, err)
		}
		claims[m] = claimIdentity{UID: string(pvc.UID), Created: pvc.CreationTimestamp.Time,
			Deleting: pvc.DeletionTimestamp != nil, VolumeName: pvc.Spec.VolumeName}
	}
	return claims, nil
}

// noteIdentities keeps, for the rest of the reconcile of cluster, the
// members' Pods the write quarantine read after the probes, or the error of
// that read, so that the rebuild decides from the same Pods.
func (r *CubridClusterReconciler) noteIdentities(cluster *databasev1alpha1.CubridCluster,
	ids map[string]memberIdentity, err error) {
	if report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster)); ok {
		report := report.(*reconcileReport)
		report.identities, report.identitiesErr, report.identitiesNoted = ids, err, true
	}
}

// notedIdentities are the Pods noteIdentities kept in this reconcile, or the
// members' Pods read now when none were kept.
func (r *CubridClusterReconciler) notedIdentities(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	members []string) (map[string]memberIdentity, error) {
	if report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster)); ok && report.(*reconcileReport).identitiesNoted {
		report := report.(*reconcileReport)
		return report.identities, report.identitiesErr
	}
	return r.memberIdentities(ctx, cluster, members)
}

// reconcileMemberRebuild records the claims the members run the database on,
// withdraws an accepted rebuild that no longer describes what was approved,
// applies the rebuild annotation (decideRebuildRequest) and publishes the
// offers of this pass in the status. lastResolved is
// status.lastResolvedPrimary before this pass; before are the members' Pods
// read before the probes, or the error of that read. The Pods read after the
// probes are the ones the write quarantine read in this pass
// (noteIdentities), and the decision follows the quarantine in force after
// it.
//
// An accepted request is recorded in status.memberRebuild with its operation
// ID, and the annotation is removed by the next pass, which finds its token
// consumed in the stored status. An inert or refused annotation is reported
// with a Warning Event once per value and message, and left for the
// administrator. Nothing is deleted, and nothing is restored on a member here.
func (r *CubridClusterReconciler) reconcileMemberRebuild(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	members []string, obs map[string]RoleObservation, now time.Time, lastResolved string,
	before map[string]memberIdentity, beforeErr error) stepResult {
	steps := stepResult{}
	ids, idsErr := r.notedIdentities(ctx, cluster, members)
	claims, claimsErr := r.memberClaims(ctx, cluster, members)
	unknown := errors.Join(beforeErr, idsErr, claimsErr)
	if unknown != nil {
		steps.err = fmt.Errorf("read the members' Pods and data claims for a rebuild: %w", unknown)
	}
	unstable := map[string]bool{}
	for _, m := range members {
		if unknown == nil && before[m].PodUID != ids[m].PodUID {
			unstable[m] = true
		}
	}
	in := rebuildInput{Now: now, Members: members, CurrentPrimary: cluster.Status.CurrentPrimary,
		LastResolvedPrimary: lastResolved, Quarantine: cluster.Status.WriteQuarantine, Identities: ids,
		Unstable: unstable, Claims: claims, Unknown: unknown != nil, Observations: obs,
		DataClaims: cluster.Status.MemberDataClaims, Offers: cluster.Status.MemberRebuildOffers,
		Active: cluster.Status.MemberRebuild, Nonce: newQuarantineNonce}
	if len(cluster.Spec.Databases) > 0 {
		status := databaseStatus(cluster, cluster.Spec.Databases[0].Name)
		in.BootstrapDone = status.HAConfigured
		if holder := bootstrapHolder(cluster); holder != "" {
			in.Holders = seedHolders(status, holder)
		}
	}
	in.DataClaims = recordDataClaims(in)
	cluster.Status.MemberDataClaims = in.DataClaims

	if why := rebuildWithdrawal(in); why != "" {
		ended := in.Active.DeepCopy()
		end := metav1.NewTime(now.UTC())
		ended.State, ended.EndTime, ended.Message = databasev1alpha1.MemberRebuildWithdrawn, &end, why
		cluster.Status.MemberRebuild, cluster.Status.LastMemberRebuild, in.Active = nil, ended, nil
		r.statusEvent(cluster, corev1.EventTypeWarning, reasonMemberRebuildWithdrawn, fmt.Sprintf(
			"withdrew the rebuild of %s (operation %s) before anything was restored on it: %s; nothing was deleted, "+
				"and a new token is published once %s can be rebuilt", ended.Member, ended.OperationID, why, ended.Member))
	}

	offers, err := rebuildOffers(in)
	if err != nil {
		return stepResult{err: errors.Join(steps.err, fmt.Errorf("publish the rebuild offers: %w", err))}
	}
	if cluster.Annotations[rebuildMemberAnnotation] == "" {
		r.rebuildReported.Delete(client.ObjectKeyFromObject(cluster))
		cluster.Status.MemberRebuildOffers = offers
		return steps
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var stored *databasev1alpha1.CubridCluster
	if c := (&databasev1alpha1.CubridCluster{}); reader.Get(ctx, client.ObjectKeyFromObject(cluster), c) == nil {
		stored = c
	}
	verdict, msg, rebuild, err := decideRebuildRequest(in, string(cluster.UID), stored, offers)
	if err != nil {
		return stepResult{err: errors.Join(steps.err, fmt.Errorf("decide the rebuild request: %w", err))}
	}
	value := ""
	if stored != nil {
		value = stored.Annotations[rebuildMemberAnnotation]
	}
	reportOnce := func(reason string) {
		key := string(verdict) + "/" + value + "/" + msg
		if last, ok := r.rebuildReported.Load(client.ObjectKeyFromObject(cluster)); ok && last == key {
			return
		}
		r.rebuildReported.Store(client.ObjectKeyFromObject(cluster), key)
		member, token, _, _, _ := splitRebuildValue(value)
		r.event(cluster, corev1.EventTypeWarning, reason, fmt.Sprintf("did not rebuild %s on the annotation %s "+
			"with the token %s: %s", member, rebuildMemberAnnotation, tokenPrefix(token), msg))
	}
	switch verdict {
	case rebuildWait:
		logf.FromContext(ctx).Info("Did not apply the rebuild annotation: the stored cluster could not be read or is "+
			"ahead of this observation", "retryAfter", releaseWaitRetry(0).String())
		if steps.retry == 0 {
			steps.retry = releaseWaitRetry(0)
		}
	case rebuildConsumed:
		r.removeRebuildAnnotation(ctx, cluster, value)
	case rebuildInert:
		reportOnce(reasonMemberRebuildIgnored)
	case rebuildRefused:
		reportOnce(reasonMemberRebuildRefused)
	case rebuildAccepted:
		cluster.Status.MemberRebuild = rebuild
		offers = nil
		accepted := ""
		if rebuild.FormerPrimary {
			accepted = fmt.Sprintf(". %s was the last resolved primary; the request accepted the data of %s as its "+
				"data, and writes only %s's former storage held are not recovered", rebuild.Member, rebuild.Source,
				rebuild.Member)
		}
		r.statusEvent(cluster, corev1.EventTypeNormal, reasonMemberRebuildAccepted, fmt.Sprintf(
			"accepted the rebuild of %s from the primary %s as operation %s, with the token %s; %s's data claim %s "+
				"was not seen holding the database. Nothing was deleted%s", rebuild.Member, rebuild.Source,
			rebuild.OperationID, tokenPrefix(rebuild.Token), rebuild.Member, rebuild.ClaimUID, accepted))
	}
	cluster.Status.MemberRebuildOffers = offers
	return steps
}

// removeRebuildAnnotation removes the rebuild annotation from the cluster if
// it still has value, whose token the stored status shows consumed. A failure
// is logged, not returned: the annotation stays inert, and the next pass
// removes it again.
func (r *CubridClusterReconciler) removeRebuildAnnotation(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	value string) {
	path := "/metadata/annotations/" + strings.ReplaceAll(strings.ReplaceAll(rebuildMemberAnnotation,
		"~", "~0"), "/", "~1")
	// The test operation makes the removal fail when the annotation was
	// changed since it was read, so a newer request is never removed.
	type operation struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value,omitempty"`
	}
	patch, err := json.Marshal([]operation{{Op: "test", Path: path, Value: value}, {Op: "remove", Path: path}})
	if err == nil {
		target := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name,
			Namespace: cluster.Namespace}}
		err = r.Patch(ctx, target, client.RawPatch(types.JSONPatchType, patch))
	}
	if err != nil {
		logf.FromContext(ctx).Info("Could not remove the rebuild annotation of a consumed token; it stays inert",
			"annotation", rebuildMemberAnnotation, "reason", err.Error())
		return
	}
	logf.FromContext(ctx).Info("Removed the rebuild annotation of a consumed token",
		"annotation", rebuildMemberAnnotation)
}
