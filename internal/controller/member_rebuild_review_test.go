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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// offerFor is the offer for member in offers, nil for none.
func offerFor(offers []databasev1alpha1.MemberRebuildOffer, member string) *databasev1alpha1.MemberRebuildOffer {
	for i := range offers {
		if offers[i].Member == member {
			return &offers[i]
		}
	}
	return nil
}

// A rebuild that failed after its restore was started, here while the
// restore still runs, leaves its claim holding a partial copy or a restore in
// progress. That claim and its volume are never offered again, so a new
// token never makes the Instance Manager take over or remove what the failed
// restore left; a new empty claim is offered.
func TestRebuildFailedAfterTheRestoreNeverOffersItsClaimAgain(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.restoreRecorded(instancemanager.OpRestoring)
	for _, o := range f.objs {
		if p, ok := o.(*corev1.Pod); ok && p.Name == cpSource {
			p.UID = "pod-" + cpSource + "-new"
		}
	}
	got, events, _ := f.pass()
	if last := got.Status.LastMemberRebuild; last == nil || last.State != databasev1alpha1.MemberRebuildFailed {
		t.Fatalf("the rebuild ends Failed, got %+v", last)
	}
	failed := databasev1alpha1.MemberDataClaim{Name: cpTarget, ClaimUID: "claim-" + cpTarget, VolumeName: "pv-" + cpTarget}
	if !slices.Contains(got.Status.FormerDataClaims, failed) {
		t.Fatalf("the failed rebuild's claim is kept in status.formerDataClaims, got %+v", got.Status.FormerDataClaims)
	}
	if o := offerFor(got.Status.MemberRebuildOffers, cpTarget); o != nil {
		t.Fatalf("the claim of a failed rebuild is not offered, got %+v", o)
	}
	if !slices.ContainsFunc(events, func(e string) bool { return strings.Contains(e, "may still be running") }) {
		t.Fatalf("the Failed Event says the restore may still be running, got %q", events)
	}

	// A later pass, with the restore still running, offers nothing either.
	f.cluster = got
	got, _, _ = f.pass()
	if o := offerFor(got.Status.MemberRebuildOffers, cpTarget); o != nil {
		t.Fatalf("no offer while the claim holds the failed restore, got %+v", o)
	}

	// A new empty claim is offered.
	f.cluster = got
	for _, o := range f.objs {
		if c, ok := o.(*corev1.PersistentVolumeClaim); ok && c.Name == dataVolume+"-"+cpTarget {
			c.UID, c.Spec.VolumeName = "claim-"+cpTarget+"-new", "pv-"+cpTarget+"-new"
		}
	}
	got, _, _ = f.pass()
	if o := offerFor(got.Status.MemberRebuildOffers, cpTarget); o == nil || o.ClaimUID != "claim-"+cpTarget+"-new" {
		t.Fatalf("a new empty claim is offered, got %+v", got.Status.MemberRebuildOffers)
	}
}

// A restore that the member's Instance Manager refused because it could not
// confirm the source, or found it not the master, wrote nothing: the rebuild
// is Withdrawn, and its claim is not kept from a new offer.
func TestRebuildRestoreRefusedForItsSourceIsWithdrawn(t *testing.T) {
	for _, code := range []string{instancemanager.FailureRebuildSourceUnconfirmed,
		instancemanager.FailureRebuildSourceNotMaster} {
		t.Run(code, func(t *testing.T) {
			f := newRebuildCopyFixture(t)
			f.restoreRecorded(instancemanager.OpFailed)
			f.seed.code = code
			got, _, _ := f.pass()
			if last := got.Status.LastMemberRebuild; got.Status.MemberRebuild != nil || last == nil ||
				last.State != databasev1alpha1.MemberRebuildWithdrawn {
				t.Fatalf("a restore refused for its source ends the rebuild Withdrawn, got %+v", last)
			}
			if slices.ContainsFunc(got.Status.FormerDataClaims, func(c databasev1alpha1.MemberDataClaim) bool {
				return c.ClaimUID == "claim-"+cpTarget
			}) {
				t.Fatalf("the claim is not kept from a new offer, got %+v", got.Status.FormerDataClaims)
			}
		})
	}
}

// The same refusal after restoredb, from the check of the source right before
// the member is started, leaves restored data: the rebuild is Failed, and the
// member is stopped.
func TestRebuildSourceRefusedAfterRestoredbFails(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.restoreRecorded(instancemanager.OpFailed)
	f.seed.code = instancemanager.FailureRebuildSourceNotMaster
	f.seed.restoredFrom = map[string]string{cpRestoreKey: "s3://" + uriBucket + "/pre" + cpExpectedSeedDirPart + "/" +
		instancemanager.ManifestObjectName}
	got, _, _ := f.pass()
	if last := got.Status.LastMemberRebuild; last == nil || last.State != databasev1alpha1.MemberRebuildFailed {
		t.Fatalf("a refusal after restoredb ends the rebuild Failed, got %+v", last)
	}
	if len(f.member.shutdowns) != 1 {
		t.Fatalf("the member is stopped, got %v", f.member.shutdowns)
	}
}

// A restore started in passes whose status writes were lost is found by its
// key, whether the backup was recorded or nothing of the copy was, and the
// rebuild is judged, never Withdrawn as one that restored nothing.
func TestRebuildRestoreStartedUnderLostStatusIsJudged(t *testing.T) {
	for _, recorded := range []string{"backup recorded", "nothing recorded"} {
		t.Run(recorded, func(t *testing.T) {
			f := newRebuildCopyFixture(t)
			f.restoreRecorded(instancemanager.OpRestoring)
			if recorded == "backup recorded" {
				f.cluster.Status.MemberRebuild.Copy.RestoreOperationID = ""
			} else {
				f.cluster.Status.MemberRebuild.Copy = nil
			}
			f.cluster.Status.WriteQuarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMemberUnverified}
			got, _, _ := f.pass()
			if len(f.seed.restoreOn) != 0 || len(f.seed.backupOn) != 0 {
				t.Fatalf("nothing is started again, got %v %v", f.seed.backupOn, f.seed.restoreOn)
			}
			if last := got.Status.LastMemberRebuild; last == nil || last.State != databasev1alpha1.MemberRebuildFailed ||
				last.Copy == nil || last.Copy.RestoreOperationID != cpRestoreOperationID {
				t.Fatalf("the restore found by its key is recorded and the rebuild ends Failed, got %+v", last)
			}
		})
	}
}

// The copy never starts a restore on the current primary, even when nothing
// before it withdrew the rebuild.
func TestRebuildCopyRefusesTheCurrentPrimaryItself(t *testing.T) {
	f := newRebuildCopyFixture(t)
	c := fake.NewClientBuilder().WithScheme(f.sch).WithObjects(append(slices.Clone(f.objs), f.cluster.DeepCopy())...).Build()
	r := &CubridClusterReconciler{Client: c, APIReader: c, Scheme: f.sch, Recorder: record.NewFakeRecorder(20),
		Backup: f.seed, Restore: f.seed, Clock: func() time.Time { return f.now }}
	ctx := context.Background()
	members := []string{cpSource, cpSlave, cpTarget}
	ids, err := r.memberIdentities(ctx, f.cluster, members)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := r.memberClaims(ctx, f.cluster, members)
	if err != nil {
		t.Fatal(err)
	}
	cluster := f.cluster.DeepCopy()
	in := rebuildInput{Now: f.now, Members: members, CurrentPrimary: cpTarget, Identities: ids, Claims: claims,
		Observations: f.obs, Active: cluster.Status.MemberRebuild}
	if why := r.copyForRebuild(ctx, cluster, &in); why != "" {
		t.Fatalf("the copy waits, it does not withdraw, got %q", why)
	}
	if len(f.seed.restoreOn) != 0 || len(f.seed.backupOn) != 0 {
		t.Fatalf("nothing is copied onto the current primary, got %v %v", f.seed.backupOn, f.seed.restoreOn)
	}
}

// The write quarantine is read again from the API server immediately before
// the restore: one stored there, not yet in this pass's status, means no
// restore call.
func TestRebuildRecheckReadsTheStoredQuarantine(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.intercep = &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if cl, ok := obj.(*databasev1alpha1.CubridCluster); ok {
			cl.Status.WriteQuarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMemberUnverified}
		}
		return nil
	}}
	got, _, _ := f.pass()
	if len(f.seed.restoreOn) != 0 {
		t.Fatalf("no restore is started while the stored status holds a quarantine, got %v", f.seed.restoreOn)
	}
	if a := got.Status.MemberRebuild; a == nil || a.Copy == nil || a.Copy.RestoreOperationID != "" {
		t.Fatalf("the rebuild waits with no restore recorded, got %+v", a)
	}
}
