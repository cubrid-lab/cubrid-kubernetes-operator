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
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// rebuildCopyFixture is a three-member HA cluster whose member rb-2 lost its
// storage and got an empty replacement claim, with an accepted rebuild of
// rb-2 from rb-0, the active master (#1043).
type rebuildCopyFixture struct {
	t        *testing.T
	now      time.Time
	sch      *runtime.Scheme
	objs     []client.Object
	cluster  *databasev1alpha1.CubridCluster
	obs      map[string]RoleObservation
	seed     *fakeSeedClient
	member   *fakeRebuildMember
	intercep *interceptor.Funcs
	// quarantineErr is the failure of the pass's write quarantine step,
	// which makes the pass one that could not be read.
	quarantineErr error
}

const (
	cpName, cpNS          = "rb", "ns"
	cpSource, cpSlave     = cpName + "-0", cpName + "-1"
	cpTarget              = cpName + "-2"
	cpOperation, cpToken  = "op1", "token1"
	cpBackupKey           = "rebuild-backup-" + cpOperation
	cpRestoreKey          = "rebuild-restore-" + cpOperation + "-" + cpTarget
	cpRestoreOperationID  = "op-restore-" + cpRestoreKey
	cpBackupOperationID   = "op-backup-" + cpBackupKey
	cpClusterUID          = "rb-uid"
	cpSourceLogEndPage    = 100
	cpSourceLogEndOffset  = 40
	cpExpectedSeedDirPart = "/" + cpClusterUID + "/rebuild-" + cpOperation + "/" + testDatabase
)

func newRebuildCopyFixture(t *testing.T) *rebuildCopyFixture {
	t.Helper()
	// fakeSeedClient checks its own bookkeeping with Gomega.
	gomega.RegisterTestingT(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	sch := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(sch), databasev1alpha1.AddToScheme(sch)); err != nil {
		t.Fatal(err)
	}
	f := &rebuildCopyFixture{t: t, now: now, sch: sch}
	for _, m := range []string{cpSource, cpSlave, cpTarget} {
		f.objs = append(f.objs,
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: m, Namespace: cpNS, UID: types.UID("pod-" + m),
				CreationTimestamp: metav1.NewTime(now.Add(-time.Minute))}},
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dataVolume + "-" + m, Namespace: cpNS,
				UID: types.UID("claim-" + m), CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
				Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-" + m}})
	}
	f.cluster = &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: cpName, Namespace: cpNS, UID: cpClusterUID},
		Spec: databasev1alpha1.CubridClusterSpec{
			Version:          cubridVersion,
			Databases:        []databasev1alpha1.CubridDatabase{{Name: testDatabase}},
			HighAvailability: databasev1alpha1.CubridHighAvailability{Enabled: true},
			Topology:         databasev1alpha1.CubridTopology{PromotableMembers: 3},
			ObjectStorage:    &databasev1alpha1.CubridObjectStorage{Bucket: uriBucket, Prefix: "pre"},
		},
		Status: databasev1alpha1.CubridClusterStatus{
			CurrentPrimary: cpSource, LastResolvedPrimary: cpSource,
			Databases: []databasev1alpha1.DatabaseStatus{{Name: testDatabase, PrimaryCreated: true,
				HAConfigured: true, SeededMembers: []string{cpSlave, cpTarget}}},
			MemberDataClaims: []databasev1alpha1.MemberDataClaim{
				{Name: cpSource, ClaimUID: "claim-" + cpSource, VolumeName: "pv-" + cpSource},
				{Name: cpSlave, ClaimUID: "claim-" + cpSlave, VolumeName: "pv-" + cpSlave},
				{Name: cpTarget, ClaimUID: "claim-" + cpTarget + "-lost", VolumeName: "pv-" + cpTarget + "-lost"}},
			MemberRebuild: &databasev1alpha1.MemberRebuild{Member: cpTarget, PodUID: "pod-" + cpTarget,
				ClaimUID: "claim-" + cpTarget, VolumeName: "pv-" + cpTarget, Source: cpSource,
				SourcePodUID: "pod-" + cpSource, Token: cpToken, OperationID: cpOperation,
				AcceptedTime: metav1.NewTime(now.Add(-time.Minute)), State: databasev1alpha1.MemberRebuildAccepted},
		},
	}
	master := answersMaster
	master.LogEnd = &databasev1alpha1.LogAddress{Page: cpSourceLogEndPage, Offset: cpSourceLogEndOffset}
	f.obs = map[string]RoleObservation{cpSource: master, cpSlave: answersSlave, cpTarget: answersUnknown}
	for m, o := range f.obs {
		o.ObservedAt = now
		f.obs[m] = o
	}
	f.member = &fakeRebuildMember{}
	f.seed = &fakeSeedClient{backupState: instancemanager.OpCompleted,
		restore: map[string]instancemanager.OperationState{cpTarget: instancemanager.OpRestoring}}
	return f
}

// restoreRecorded records in the fixture's status that the restore of the
// rebuild was started, as a pass before an Operator restart left it, and has
// the fake hold that restore.
func (f *rebuildCopyFixture) restoreRecorded(state instancemanager.OperationState) {
	f.seed.restoresByKey = map[string]instancemanager.RestoreRequest{cpRestoreKey: {Database: testDatabase,
		Bucket: uriBucket, Prefix: "pre" + cpExpectedSeedDirPart}}
	f.seed.restore[cpTarget] = state
	at := metav1.NewTime(f.now.Add(-20 * time.Second))
	f.cluster.Status.MemberRebuild.Copy = &databasev1alpha1.DatabaseSeeding{Member: cpTarget, Source: cpSource,
		BackupOperationID: cpBackupOperationID, RestoreOperationID: cpRestoreOperationID,
		ActiveMasterSince: &at, LastVerifiedAt: &at}
}

// caughtUpAnswer is rb-2 answering as a slave of rb-0 whose copy and applier
// reached the source's log end.
func (f *rebuildCopyFixture) caughtUpAnswer() RoleObservation {
	end := &databasev1alpha1.LogAddress{Page: cpSourceLogEndPage, Offset: cpSourceLogEndOffset}
	o := answersSlave
	o.ObservedAt = f.now
	o.Replication = &ReplicationObservation{Source: cpSource, CopiedEOF: end, FinalLSA: end}
	return o
}

// pass runs the rebuild step once with a new reconciler, as after an Operator
// restart, and returns the cluster it changed and the Events it recorded.
func (f *rebuildCopyFixture) pass() (*databasev1alpha1.CubridCluster, []string, client.Client) {
	f.t.Helper()
	builder := fake.NewClientBuilder().WithScheme(f.sch).WithObjects(append(slices.Clone(f.objs), f.cluster.DeepCopy())...)
	if f.intercep != nil {
		builder = builder.WithInterceptorFuncs(*f.intercep)
	}
	c := builder.Build()
	recorder := record.NewFakeRecorder(20)
	r := &CubridClusterReconciler{Client: c, APIReader: c, Scheme: f.sch, Recorder: recorder,
		Backup: f.seed, Restore: f.seed, RebuildMember: f.member, Clock: func() time.Time { return f.now }}
	ctx := context.Background()
	members := []string{cpSource, cpSlave, cpTarget}
	before, err := r.memberIdentities(ctx, f.cluster, members)
	if err != nil {
		f.t.Fatal(err)
	}
	got := f.cluster.DeepCopy()
	r.reconcileMemberRebuild(ctx, got, members, f.obs, f.now, cpSource, before, nil, f.quarantineErr)
	var events []string
	for len(recorder.Events) > 0 {
		events = append(events, <-recorder.Events)
	}
	return got, events, c
}

// An accepted rebuild takes its backup on the named source, uploads it to the
// rebuild's own place, and restores it on the member being rebuilt, bound to
// the rebuild's operation ID and token, under keys built from that ID (#1043).
func TestRebuildCopiesFromTheNamedSource(t *testing.T) {
	f := newRebuildCopyFixture(t)
	got, _, _ := f.pass()

	if !slices.Equal(f.seed.backupOn, []string{cpSource}) {
		t.Fatalf("the backup runs on the named source %s only, got %v", cpSource, f.seed.backupOn)
	}
	b := f.seed.backups[cpBackupKey]
	if b.member != cpSource {
		t.Fatalf("the backup is keyed %q, got %+v", cpBackupKey, f.seed.backups)
	}
	up := b.req.Upload
	if up == nil || !up.Seed || up.Rebuild == nil || *up.Rebuild != (instancemanager.RebuildBinding{
		OperationID: cpOperation, Token: cpToken}) || up.SourceInstance != cpSource ||
		up.SourceRole != string(databasev1alpha1.RoleMaster) || up.ClusterUID != cpClusterUID {
		t.Fatalf("the backup is a seed upload bound to the rebuild, got %+v", up)
	}
	if up.Prefix != "pre"+cpExpectedSeedDirPart {
		t.Fatalf("the backup goes to the rebuild's own place, never the bootstrap seed, got %q", up.Prefix)
	}
	if !slices.Equal(f.seed.restoreOn, []string{cpTarget}) || f.seed.restoreKeys[cpTarget] != cpRestoreKey {
		t.Fatalf("the restore runs on %s under %q, got %v %v", cpTarget, cpRestoreKey, f.seed.restoreOn, f.seed.restoreKeys)
	}
	req := f.seed.restores[cpTarget]
	if req.SeedFromMaster != cpSource || req.ExpectedMember != cpTarget || req.ExpectedClusterUID != cpClusterUID ||
		req.Rebuild == nil || *req.Rebuild != (instancemanager.RebuildBinding{OperationID: cpOperation, Token: cpToken}) ||
		req.Prefix != up.Prefix || req.Bucket != uriBucket {
		t.Fatalf("the restore names the source, the member and the rebuild, got %+v", req)
	}
	a := got.Status.MemberRebuild
	if a == nil || a.State != databasev1alpha1.MemberRebuildAccepted || a.OperationID != cpOperation {
		t.Fatalf("the rebuild stays accepted under its operation ID while it restores, got %+v", a)
	}
	if a.Copy == nil || a.Copy.BackupOperationID != cpBackupOperationID ||
		a.Copy.RestoreOperationID != cpRestoreOperationID || a.Copy.Source != cpSource || a.Copy.Member != cpTarget {
		t.Fatalf("the copy's operations are recorded in status.memberRebuild.copy, got %+v", a.Copy)
	}
}

// Nothing is copied before the source and the member being rebuilt both run
// an Instance Manager that binds a restore to a rebuild (ADR-0013).
func TestRebuildCopyWaitsForTheRebuildRestoreCapability(t *testing.T) {
	for _, member := range []string{cpSource, cpTarget} {
		t.Run(member, func(t *testing.T) {
			f := newRebuildCopyFixture(t)
			o := f.obs[member]
			o.Capabilities = slices.DeleteFunc(slices.Clone(o.Capabilities),
				func(c string) bool { return c == instancemanager.CapabilityRebuildRestore })
			f.obs[member] = o
			got, _, _ := f.pass()
			if len(f.seed.backupOn) != 0 || len(f.seed.restoreOn) != 0 {
				t.Fatalf("nothing is started, got backups %v restores %v", f.seed.backupOn, f.seed.restoreOn)
			}
			if got.Status.MemberRebuild == nil || got.Status.MemberRebuild.State != databasev1alpha1.MemberRebuildAccepted {
				t.Fatalf("the rebuild waits, got %+v", got.Status.MemberRebuild)
			}
		})
	}
}

// After an Operator restart in the middle of the restore, the rebuild keeps
// its operation ID and keys and only polls the recorded restore: nothing is
// started again (#1043).
func TestRebuildRestartMidRestoreOnlyPolls(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.restoreRecorded(instancemanager.OpRestoring)
	got, _, _ := f.pass()
	if len(f.seed.backupOn) != 0 || len(f.seed.restoreOn) != 0 {
		t.Fatalf("nothing is started after a restart, got backups %v restores %v", f.seed.backupOn, f.seed.restoreOn)
	}
	if !slices.Contains(f.seed.polls, cpRestoreOperationID) {
		t.Fatalf("the recorded restore is polled, got %v", f.seed.polls)
	}
	a := got.Status.MemberRebuild
	if a == nil || a.OperationID != cpOperation || a.Copy == nil || a.Copy.RestoreOperationID != cpRestoreOperationID ||
		a.State != databasev1alpha1.MemberRebuildAccepted {
		t.Fatalf("the operation ID and the recorded restore stay, got %+v", a)
	}
}

// A restore that completed and a member that caught up with its source end
// the rebuild Completed, also when the member is now seen running the
// database on its new claim, which would refuse a new offer for it (#1043).
func TestRebuildCaughtUpCompletesAndIsNotWithdrawn(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.restoreRecorded(instancemanager.OpCompleted)
	// The first reading after the restore completed records the source's log
	// end; the member is a slave that has not caught up yet.
	behind := f.caughtUpAnswer()
	behind.Replication.CopiedEOF = &databasev1alpha1.LogAddress{Page: 1}
	f.obs[cpTarget] = behind
	got, _, _ := f.pass()
	a := got.Status.MemberRebuild
	if a == nil || a.State != databasev1alpha1.MemberRebuildAccepted || got.Status.LastMemberRebuild != nil {
		t.Fatalf("a finished restore is not the member being healthy: the rebuild waits for the catch-up, got %+v "+
			"last %+v", a, got.Status.LastMemberRebuild)
	}
	if a.Copy.CatchUpTarget == nil || a.Copy.CatchUpTarget.Page != cpSourceLogEndPage {
		t.Fatalf("the catch-up target is the source's log end, got %+v", a.Copy.CatchUpTarget)
	}

	f.cluster = got
	f.obs[cpTarget] = f.caughtUpAnswer()
	got, events, _ := f.pass()
	last := got.Status.LastMemberRebuild
	if got.Status.MemberRebuild != nil || last == nil || last.State != databasev1alpha1.MemberRebuildCompleted ||
		last.OperationID != cpOperation || last.EndTime == nil {
		t.Fatalf("a caught-up member ends the rebuild Completed, got %+v last %+v", got.Status.MemberRebuild, last)
	}
	if !slices.ContainsFunc(events, func(e string) bool { return strings.Contains(e, reasonMemberRebuildCompleted) }) ||
		slices.ContainsFunc(events, func(e string) bool { return strings.Contains(e, reasonMemberRebuildWithdrawn) }) {
		t.Fatalf("one Completed Event and no withdrawal, got %q", events)
	}
	if len(f.seed.restoreOn) != 0 || len(f.seed.backupOn) != 0 {
		t.Fatalf("nothing is started, got %v %v", f.seed.backupOn, f.seed.restoreOn)
	}
}

// A write quarantine in the status just before the restore keeps the restore
// from being started: the rebuild is withdrawn, as nothing was restored yet
// (#1043).
func TestRebuildQuarantineBeforeTheRestoreStartsNone(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.cluster.Status.WriteQuarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMemberUnverified}
	f.seed.backups = map[string]seedBackup{cpBackupKey: {member: cpSource, req: instancemanager.BackupRequest{
		Database: testDatabase}}}
	f.cluster.Status.MemberRebuild.Copy = &databasev1alpha1.DatabaseSeeding{Member: cpTarget, Source: cpSource,
		BackupOperationID: cpBackupOperationID}
	got, _, _ := f.pass()
	if len(f.seed.restoreOn) != 0 {
		t.Fatalf("no restore is started while a quarantine is in force, got %v", f.seed.restoreOn)
	}
	if last := got.Status.LastMemberRebuild; got.Status.MemberRebuild != nil || last == nil ||
		last.State != databasev1alpha1.MemberRebuildWithdrawn {
		t.Fatalf("the rebuild is withdrawn before anything was restored, got %+v", last)
	}
}

// The target's claim, read again immediately before the restore, decides:
// a claim that changed since the pass read it means no restore call (#1043).
func TestRebuildClaimChangeJustBeforeTheRestoreStartsNone(t *testing.T) {
	for _, what := range []string{"rebuilt member claim", "source Pod UID"} {
		t.Run(what, func(t *testing.T) {
			f := newRebuildCopyFixture(t)
			reads := map[string]int{}
			f.intercep = &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				switch o := obj.(type) {
				case *corev1.PersistentVolumeClaim:
					if what == "rebuilt member claim" && key.Name == dataVolume+"-"+cpTarget {
						if reads[key.Name]++; reads[key.Name] > 1 {
							o.UID = "claim-" + cpTarget + "-other"
						}
					}
				case *corev1.Pod:
					if what == "source Pod UID" && key.Name == cpSource {
						// The two reads of the pass and the one before the
						// restore.
						if reads[key.Name]++; reads[key.Name] > 2 {
							o.UID = "pod-" + cpSource + "-new"
						}
					}
				}
				return nil
			}}
			got, _, _ := f.pass()
			if len(f.seed.restoreOn) != 0 {
				t.Fatalf("no restore is started when the %s changed just before it, got %v", what, f.seed.restoreOn)
			}
			if got.Status.MemberRebuild != nil && got.Status.MemberRebuild.Copy != nil &&
				got.Status.MemberRebuild.Copy.RestoreOperationID != "" {
				t.Fatalf("no restore is recorded, got %+v", got.Status.MemberRebuild.Copy)
			}
		})
	}
}

// Once the restore was started, a change of the source's Pod ends the
// rebuild Failed, not Withdrawn, and nothing is deleted (#1043).
func TestRebuildSourcePodChangeAfterTheRestoreFails(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.restoreRecorded(instancemanager.OpRestoring)
	for _, o := range f.objs {
		if p, ok := o.(*corev1.Pod); ok && p.Name == cpSource {
			p.UID = "pod-" + cpSource + "-new"
		}
	}
	got, events, c := f.pass()
	last := got.Status.LastMemberRebuild
	if got.Status.MemberRebuild != nil || last == nil || last.State != databasev1alpha1.MemberRebuildFailed ||
		!strings.Contains(last.Message, "Pod") {
		t.Fatalf("a source Pod change after the restore started ends the rebuild Failed, got %+v", last)
	}
	if !slices.ContainsFunc(events, func(e string) bool { return strings.Contains(e, reasonMemberRebuildFailed) }) {
		t.Fatalf("a Failed Event is recorded, got %q", events)
	}
	// The member is held and stopped, so the text must not say otherwise (#1056).
	if !slices.ContainsFunc(events, func(e string) bool {
		return strings.Contains(e, "held and stopped") && strings.Contains(e, "not trusted") &&
			strings.Contains(e, "status.formerDataClaims") &&
			!strings.Contains(e, "nothing was stopped") && !strings.Contains(e, "may join")
	}) || !strings.Contains(last.Message, "held and stopped") {
		t.Fatalf("the Event and message say the member is held and stopped, got %q and %q", events, last.Message)
	}
	var claims corev1.PersistentVolumeClaimList
	if err := c.List(context.Background(), &claims); err != nil || len(claims.Items) != 3 {
		t.Fatalf("no claim is deleted, got %d (%v)", len(claims.Items), err)
	}
}

// Once the restore was started, a write quarantine or a gap in the readings
// ends the rebuild Failed (#1043).
func TestRebuildQuarantineOrGapAfterTheRestoreFails(t *testing.T) {
	for _, what := range []string{"quarantine", "gap"} {
		t.Run(what, func(t *testing.T) {
			f := newRebuildCopyFixture(t)
			f.restoreRecorded(instancemanager.OpRestoring)
			if what == "quarantine" {
				f.cluster.Status.WriteQuarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMemberUnverified}
			} else {
				at := metav1.NewTime(f.now.Add(-time.Minute))
				f.cluster.Status.MemberRebuild.Copy.LastVerifiedAt = &at
			}
			got, _, _ := f.pass()
			if last := got.Status.LastMemberRebuild; got.Status.MemberRebuild != nil || last == nil ||
				last.State != databasev1alpha1.MemberRebuildFailed {
				t.Fatalf("a %s after the restore started ends the rebuild Failed, got %+v", what, last)
			}
		})
	}
}

// The restore never runs on the current primary or a member that reports
// itself master (#1043).
func TestRebuildRestoreNeverTargetsThePrimary(t *testing.T) {
	f := newRebuildCopyFixture(t)
	master := answersMaster
	master.ObservedAt = f.now
	f.obs[cpTarget] = master
	got, _, _ := f.pass()
	if len(f.seed.restoreOn) != 0 || len(f.seed.backupOn) != 0 {
		t.Fatalf("nothing is copied onto a member that reports itself master, got %v %v", f.seed.backupOn,
			f.seed.restoreOn)
	}
	if got.Status.MemberRebuild != nil {
		t.Fatalf("the rebuild of a member that reports itself master ends, got %+v", got.Status.MemberRebuild)
	}

	f = newRebuildCopyFixture(t)
	f.restoreRecorded(instancemanager.OpRestoring)
	f.obs[cpTarget] = master
	got, _, _ = f.pass()
	if last := got.Status.LastMemberRebuild; last == nil || last.State != databasev1alpha1.MemberRebuildFailed {
		t.Fatalf("a member that reports itself master during its restore ends the rebuild Failed, got %+v", last)
	}
}
