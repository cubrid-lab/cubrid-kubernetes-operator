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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// releaseWriteQuarantineAnnotation on a CubridCluster asks for the manual
// release of the write quarantine in force. Its value must be the stored
// quarantine's current releaseToken (ADR-0005, section 8).
const releaseWriteQuarantineAnnotation = "database.cubrid.io/release-write-quarantine"

// The Events of the manual release.
const (
	reasonWriteQuarantineReleasedManually = "WriteQuarantineReleasedManually"
	reasonWriteQuarantineReleaseIgnored   = "WriteQuarantineReleaseIgnored"
	reasonWriteQuarantineReleaseRefused   = "WriteQuarantineReleaseRefused"
)

// manualReleaseVerdict is what decideManualRelease made of the annotation.
type manualReleaseVerdict string

const (
	// manualReleaseNone: no annotation, or nothing to do with it this pass.
	manualReleaseNone manualReleaseVerdict = ""
	// manualReleaseInert: the annotation names no current token.
	manualReleaseInert manualReleaseVerdict = "inert"
	// manualReleaseConsumed: the annotation names the token an earlier
	// release consumed; it is removed.
	manualReleaseConsumed manualReleaseVerdict = "consumed"
	// manualReleaseWait: the stored cluster could not be read, or this pass
	// decided from another revision than the stored one; it is tried again.
	manualReleaseWait manualReleaseVerdict = "wait"
	// manualReleaseRefused: the annotation names the current token, but this
	// pass does not see exactly one active master.
	manualReleaseRefused manualReleaseVerdict = "refused"
	// manualReleaseAccepted: the quarantine is released.
	manualReleaseAccepted manualReleaseVerdict = "accepted"
)

// manualReleaseInput is what one pass knows for a manual release.
type manualReleaseInput struct {
	// Now is the time of the pass.
	Now time.Time
	// ClusterUID is the UID of the cluster this pass reconciles.
	ClusterUID string
	// Stored is the cluster read from the API server, past the cache, after
	// the decision; nil when it could not be read.
	Stored *databasev1alpha1.CubridCluster
	// Decision is this pass's decideWriteQuarantine outcome.
	Decision quarantineDecision
	// IdentitiesUnknown says that the members' Pods could not be read.
	IdentitiesUnknown bool
}

// decideManualRelease decides on the release annotation of the stored
// cluster. It returns the verdict, a message for an inert or refused
// annotation, and the release record of an accepted one.
//
// The token is compared with the stored status, never with memory or the
// cache, which may hold an earlier revision. A token an earlier release
// consumed is ignored. A token that is not the stored quarantine's current
// one, a quarantine recorded for another cluster UID, and a quarantine whose
// members this pass revised (which replaced its token) are inert. A pass that
// decided from another revision than the stored one waits. Otherwise the
// release is refused unless the pass saw exactly one active master and the
// members' Pods: a manual release never opens the path beside no active
// master, or two.
func decideManualRelease(in manualReleaseInput) (manualReleaseVerdict, string, *databasev1alpha1.WriteQuarantineRelease) {
	stored := in.Stored
	switch {
	case stored == nil, string(stored.UID) != in.ClusterUID:
		return manualReleaseWait, "", nil
	}
	token := stored.Annotations[releaseWriteQuarantineAnnotation]
	if token == "" {
		return manualReleaseNone, "", nil
	}
	if last := stored.Status.LastWriteQuarantineRelease; last != nil && last.ReleaseToken == token {
		return manualReleaseConsumed, "", nil
	}
	sq, dq := stored.Status.WriteQuarantine, in.Decision.Quarantine
	switch {
	case sq == nil:
		return manualReleaseInert, "no write quarantine is in force", nil
	case token != sq.ReleaseToken:
		return manualReleaseInert, fmt.Sprintf("the value is not the current release token of the quarantine "+
			"(generation %s, revision %d); read status.writeQuarantine.releaseToken again after reviewing its members",
			sq.GenerationID, sq.Revision), nil
	case sq.ClusterUID != in.ClusterUID:
		return manualReleaseInert, fmt.Sprintf("the quarantine was recorded for the cluster UID %s, not %s",
			sq.ClusterUID, in.ClusterUID), nil
	case in.Decision.Change == quarantineRevised:
		return manualReleaseInert, fmt.Sprintf("the quarantine's members were revised in this observation "+
			"(revision %d); its token is replaced, and the new one must be reviewed and set", dq.Revision), nil
	case dq == nil || dq.GenerationID != sq.GenerationID || dq.Revision != sq.Revision ||
		dq.ReleaseToken != sq.ReleaseToken:
		// Decided from a cache behind the stored status, or released in
		// this pass: nothing is accepted from it.
		return manualReleaseWait, "", nil
	case in.IdentitiesUnknown:
		return manualReleaseRefused, "the members' Pods could not be read in this observation", nil
	case len(in.Decision.ActiveMasters) == 0:
		return manualReleaseRefused, "no member is an active master in this observation; a manual release does " +
			"not open the read-write path without exactly one active master", nil
	case len(in.Decision.ActiveMasters) > 1:
		return manualReleaseRefused, fmt.Sprintf("%d members are active masters in this observation (%v); a manual "+
			"release does not open the read-write path without exactly one active master",
			len(in.Decision.ActiveMasters), in.Decision.ActiveMasters), nil
	}
	return manualReleaseAccepted, "", &databasev1alpha1.WriteQuarantineRelease{GenerationID: sq.GenerationID,
		Reason: sq.Reason, StartTime: sq.StartTime, ReleaseTime: metav1.NewTime(in.Now.UTC()),
		Method: databasev1alpha1.WriteQuarantineReleasedManually, Revision: sq.Revision,
		ObservedMaster: in.Decision.ActiveMasters[0], Members: sq.Members, ReleaseToken: token,
		Warning: manualReleaseWarning}
}

// manualReleaseWarning is recorded with a manual release.
const manualReleaseWarning = "released on an administrator's annotation, not on a fence the Operator verified: " +
	"the administrator accepted that acknowledged writes may be lost if any recorded member was not fenced"

// reconcileManualRelease applies the release annotation to this pass's
// decision d (decideManualRelease) and returns d with how soon the pass is
// retried, zero for the usual cadence. The annotation is read from the stored
// cluster, past the cache, and only when the cluster read for this pass
// carries one. An accepted release replaces the quarantine with the release
// in d, as an automatic release does: the read-write Brokers stay at zero in
// this pass and start in the next one, after the status that records the
// release and the consumed token was written; the annotation is then
// removed. An annotation that names a consumed token is removed again. An
// inert or refused annotation is reported with a Warning Event once per value
// and reason, and a refusal is recorded in the quarantine's releaseRefusal.
func (r *CubridClusterReconciler) reconcileManualRelease(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	d quarantineDecision, identitiesUnknown bool, now time.Time) (quarantineDecision, time.Duration) {
	if d.Quarantine != nil {
		d.Quarantine.ReleaseRefusal = ""
	}
	if cluster.Annotations[releaseWriteQuarantineAnnotation] == "" {
		return d, 0
	}
	log := logf.FromContext(ctx)
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var stored *databasev1alpha1.CubridCluster
	if c := (&databasev1alpha1.CubridCluster{}); reader.Get(ctx, client.ObjectKeyFromObject(cluster), c) == nil {
		stored = c
	}
	verdict, msg, rel := decideManualRelease(manualReleaseInput{Now: now, ClusterUID: string(cluster.UID),
		Stored: stored, Decision: d, IdentitiesUnknown: identitiesUnknown})
	value := ""
	if stored != nil {
		value = stored.Annotations[releaseWriteQuarantineAnnotation]
	}
	state := r.quarantineState(cluster)
	reportOnce := func(reason string) {
		if key := string(verdict) + "/" + value + "/" + msg; state.reportedRelease != key {
			state.reportedRelease = key
			r.event(cluster, corev1.EventTypeWarning, reason, fmt.Sprintf("did not release the write quarantine on "+
				"the annotation %s with the value %s: %s", releaseWriteQuarantineAnnotation, tokenPrefix(value), msg))
		}
	}
	switch verdict {
	case manualReleaseWait:
		log.Info("Did not apply the release annotation: the stored cluster could not be read or is ahead of this observation")
		return d, time.Second
	case manualReleaseConsumed:
		r.removeReleaseAnnotation(ctx, cluster, value)
	case manualReleaseInert:
		// The message names the revision, which may change while the
		// value does not: an inert value is reported once.
		if key := string(verdict) + "/" + value; state.reportedRelease != key {
			state.reportedRelease = key
			r.event(cluster, corev1.EventTypeWarning, reasonWriteQuarantineReleaseIgnored, fmt.Sprintf("ignored the "+
				"annotation %s with the value %s: %s", releaseWriteQuarantineAnnotation, tokenPrefix(value), msg))
		}
	case manualReleaseRefused:
		d.Quarantine.ReleaseRefusal = msg
		reportOnce(reasonWriteQuarantineReleaseRefused)
	case manualReleaseAccepted:
		d.Quarantine, d.Released, d.Change = nil, rel, quarantineReleased
		if report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster)); ok {
			report.(*reconcileReport).consumedReleaseToken = rel.ReleaseToken
		}
	}
	return d, 0
}

// removeReleaseAnnotation removes the release annotation from the cluster if
// it still names token, the token a written release consumed. A failure is
// logged, not returned: the stored release names the token, so the
// annotation stays inert, and the next pass removes it again.
func (r *CubridClusterReconciler) removeReleaseAnnotation(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	token string) {
	path := "/metadata/annotations/" + strings.ReplaceAll(strings.ReplaceAll(releaseWriteQuarantineAnnotation,
		"~", "~0"), "/", "~1")
	patch, err := json.Marshal([]map[string]string{
		{"op": "test", "path": path, "value": token},
		{"op": "remove", "path": path},
	})
	if err == nil {
		target := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name,
			Namespace: cluster.Namespace}}
		err = r.Patch(ctx, target, client.RawPatch(types.JSONPatchType, patch))
	}
	if err != nil {
		logf.FromContext(ctx).Info("Could not remove the release annotation of a consumed token; it stays inert",
			"annotation", releaseWriteQuarantineAnnotation, "reason", err.Error())
		return
	}
	logf.FromContext(ctx).Info("Removed the release annotation of a consumed token",
		"annotation", releaseWriteQuarantineAnnotation, "token", tokenPrefix(token))
}

// tokenPrefix is the first 8 characters of a release token or annotation
// value, enough to tell tokens apart in an Event or log without repeating a
// whole token.
func tokenPrefix(v string) string {
	if len(v) <= 8 {
		return fmt.Sprintf("%q", v)
	}
	return fmt.Sprintf("%q...", v[:8])
}
