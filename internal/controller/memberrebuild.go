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
// status.memberRebuildOffers.
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
	// Verifications are the members' verifications after this pass.
	Verifications []databasev1alpha1.MemberVerification
	// Identities are the members' Pods in this pass.
	Identities map[string]memberIdentity
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

// recordDataClaims is in.DataClaims with the claim of every member whose
// database runs in this pass: a member answering as a master or a slave runs
// on the claim its Pod mounts, which is the claim of its name that existed
// when the Pod was created. Nothing is recorded when the Pods or claims could
// not be read.
func recordDataClaims(in rebuildInput) []databasev1alpha1.MemberDataClaim {
	out := slices.Clone(in.DataClaims)
	if in.Unknown {
		return out
	}
	for _, m := range in.Members {
		id, claim := in.Identities[m], in.Claims[m]
		if id.PodUID == "" || claim.UID == "" || claim.Deleting || claim.Created.After(id.Created) ||
			!runsDatabase(in.Observations[m], in.Now) {
			continue
		}
		i := slices.IndexFunc(out, func(c databasev1alpha1.MemberDataClaim) bool { return c.Name == m })
		if i < 0 {
			out = append(out, databasev1alpha1.MemberDataClaim{Name: m, ClaimUID: claim.UID})
		} else {
			out[i].ClaimUID = claim.UID
		}
	}
	return out
}

// rebuildRefusal says why member cannot be rebuilt in this pass, "" when it
// can. A rebuild discards what the member's storage holds and copies the
// database from the current primary, so it is refused for the current
// primary, for the last resolved primary while it is not verified as a
// non-master in its current Pod, and for a member whose claim is not shown
// to be a replacement that never held the database (ADR-0005). The Operator
// never deletes a claim: an administrator provides the replacement.
func rebuildRefusal(in rebuildInput, member string) string {
	switch {
	case !slices.Contains(in.Members, member):
		return fmt.Sprintf("%s is not a promotable member of this cluster", member)
	case in.Unknown:
		return "the members' Pods or data claims could not be read in this observation"
	case !in.BootstrapDone:
		return "the HA bootstrap has not copied the database to every member yet; it seeds a member that does " +
			"not hold the database without a request"
	case !slices.Contains(in.Holders, member):
		return fmt.Sprintf("%s is not recorded as holding the database; the HA bootstrap seeds it", member)
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
	case member == in.LastResolvedPrimary && !slices.ContainsFunc(in.Verifications,
		func(v databasev1alpha1.MemberVerification) bool {
			return v.Name == member && v.PodUID != "" && v.PodUID == in.Identities[member].PodUID
		}):
		return fmt.Sprintf("%s was the last resolved primary and is not verified as a non-master in its current "+
			"Pod; it may hold writes no other member has", member)
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
	}
	recorded := ""
	for _, c := range in.DataClaims {
		if c.Name == member {
			recorded = c.ClaimUID
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
// there is none, and why says what was seen.
func rebuildSource(in rebuildInput, member string) (source, why string) {
	var masters []string
	for _, m := range in.Members {
		o := in.Observations[m]
		switch {
		case m == member:
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
		ClaimUID: in.Claims[member].UID, Source: source, SourcePodUID: in.Identities[source].PodUID}
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
// describes what the administrator approved, "" while it does: the member's
// Pod or claim, the primary or the primary's Pod changed. A pass that could
// not read the Pods or claims, or observed no healthy primary among the other
// members, keeps it.
func rebuildWithdrawal(in rebuildInput) string {
	a := in.Active
	if a == nil || in.Unknown {
		return ""
	}
	source, _ := rebuildSource(in, a.Member)
	switch {
	case in.Identities[a.Member].PodUID != a.PodUID:
		return fmt.Sprintf("the Pod of %s changed since the request was accepted", a.Member)
	case in.Claims[a.Member].UID != a.ClaimUID:
		return fmt.Sprintf("the data claim of %s changed since the request was accepted", a.Member)
	case source != "" && source != a.Source:
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
// claim, source and source Pod, is inert. A pass that decided from an older
// offer than the stored one waits.
func decideRebuildRequest(in rebuildInput, clusterUID string, stored *databasev1alpha1.CubridCluster,
	offers []databasev1alpha1.MemberRebuildOffer) (rebuildVerdict, string, *databasev1alpha1.MemberRebuild, error) {
	if stored == nil || string(stored.UID) != clusterUID {
		return rebuildWait, "", nil, nil
	}
	value := stored.Annotations[rebuildMemberAnnotation]
	if value == "" {
		return rebuildNone, "", nil, nil
	}
	member, token, ok := strings.Cut(value, ":")
	if !ok || member == "" || token == "" {
		return rebuildInert, "the value must be <member>:<token>, with the member's token from " +
			"status.memberRebuildOffers", nil, nil
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
	if !slices.Contains(offers, so) {
		// Decided from a cache behind the stored status.
		return rebuildWait, "", nil, nil
	}
	operation, err := in.Nonce()
	if err != nil {
		return rebuildNone, "", nil, err
	}
	return rebuildAccepted, "", &databasev1alpha1.MemberRebuild{Member: member, PodUID: so.PodUID,
		ClaimUID: so.ClaimUID, Source: so.Source, SourcePodUID: so.SourcePodUID, Token: token,
		OperationID: operation, AcceptedTime: metav1.NewTime(in.Now.UTC()),
		State: databasev1alpha1.MemberRebuildAccepted}, nil
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
			Deleting: pvc.DeletionTimestamp != nil}
	}
	return claims, nil
}

// reconcileMemberRebuild records the claims the members run the database on,
// withdraws an accepted rebuild that no longer describes what was approved,
// applies the rebuild annotation (decideRebuildRequest) and publishes the
// offers of this pass in the status. lastResolved is
// status.lastResolvedPrimary before this pass.
//
// An accepted request is recorded in status.memberRebuild with its operation
// ID, and the annotation is removed by the next pass, which finds its token
// consumed in the stored status. An inert or refused annotation is reported
// with a Warning Event once per value and message, and left for the
// administrator. Nothing is deleted, and nothing is restored on a member here.
func (r *CubridClusterReconciler) reconcileMemberRebuild(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	members []string, obs map[string]RoleObservation, now time.Time, lastResolved string) stepResult {
	steps := stepResult{}
	ids, idsErr := r.memberIdentities(ctx, cluster, members)
	claims, claimsErr := r.memberClaims(ctx, cluster, members)
	unknown := errors.Join(idsErr, claimsErr)
	if unknown != nil {
		steps.err = fmt.Errorf("read the members' Pods and data claims for a rebuild: %w", unknown)
	}
	in := rebuildInput{Now: now, Members: members, CurrentPrimary: cluster.Status.CurrentPrimary,
		LastResolvedPrimary: lastResolved, Verifications: cluster.Status.MemberVerifications, Identities: ids,
		Claims: claims, Unknown: unknown != nil, Observations: obs, DataClaims: cluster.Status.MemberDataClaims,
		Offers: cluster.Status.MemberRebuildOffers, Active: cluster.Status.MemberRebuild, Nonce: newQuarantineNonce}
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
		r.rebuildReported.Delete(cluster.UID)
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
		if last, ok := r.rebuildReported.Load(cluster.UID); ok && last == key {
			return
		}
		r.rebuildReported.Store(cluster.UID, key)
		member, token, _ := strings.Cut(value, ":")
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
		r.statusEvent(cluster, corev1.EventTypeNormal, reasonMemberRebuildAccepted, fmt.Sprintf(
			"accepted the rebuild of %s from the primary %s as operation %s, with the token %s; %s's data claim %s "+
				"was not seen holding the database. Nothing was deleted", rebuild.Member, rebuild.Source,
			rebuild.OperationID, tokenPrefix(rebuild.Token), rebuild.Member, rebuild.ClaimUID))
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
