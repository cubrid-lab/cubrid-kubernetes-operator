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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// fakeRebuildMember records the releases and stops it was asked for.
type fakeRebuildMember struct {
	releases   []string
	releaseReq []instancemanager.RebuildReleaseRequest
	shutdowns  []string
	releaseErr error
}

func (f *fakeRebuildMember) ReleaseRebuild(_ context.Context, podName, _ string,
	req instancemanager.RebuildReleaseRequest) error {
	f.releases = append(f.releases, podName)
	f.releaseReq = append(f.releaseReq, req)
	return f.releaseErr
}

func (f *fakeRebuildMember) Shutdown(_ context.Context, podName, _, _ string) error {
	f.shutdowns = append(f.shutdowns, podName)
	return nil
}

// failSourcePod gives the source a new Pod, which ends a rebuild whose
// restore was started Failed.
func (f *rebuildCopyFixture) failSourcePod() {
	for _, o := range f.objs {
		if p, ok := o.(*corev1.Pod); ok && p.Name == cpSource {
			p.UID = "pod-" + cpSource + "-new"
		}
	}
}

// A rebuild judged Completed releases the member's held database with the
// rebuild's operation ID and token first. While the release fails, the
// rebuild is not Completed (ADR-0006: the target stays quarantined until it
// caught up).
func TestRebuildCompletedReleasesTheMember(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.restoreRecorded(instancemanager.OpCompleted)
	f.cluster.Status.MemberRebuild.Copy.CatchUpTarget = &databasev1alpha1.LogAddress{Page: cpSourceLogEndPage,
		Offset: cpSourceLogEndOffset}
	f.obs[cpTarget] = f.caughtUpAnswer()
	f.member.releaseErr = errors.New("the member did not answer")
	got, _, _ := f.pass()
	if got.Status.MemberRebuild == nil || got.Status.LastMemberRebuild != nil {
		t.Fatalf("a rebuild whose member was not released is not Completed, got %+v last %+v",
			got.Status.MemberRebuild, got.Status.LastMemberRebuild)
	}
	f.member.releaseErr = nil
	got, _, _ = f.pass()
	if last := got.Status.LastMemberRebuild; last == nil || last.State != databasev1alpha1.MemberRebuildCompleted {
		t.Fatalf("a released, caught-up rebuild is Completed, got %+v", last)
	}
	want := instancemanager.RebuildReleaseRequest{Database: testDatabase,
		Rebuild: instancemanager.RebuildBinding{OperationID: cpOperation, Token: cpToken}}
	if len(f.member.releases) != 2 || f.member.releases[1] != cpTarget || f.member.releaseReq[1] != want {
		t.Fatalf("the member is released with the rebuild's binding, got %v %+v", f.member.releases,
			f.member.releaseReq)
	}
	if len(f.member.shutdowns) != 0 {
		t.Fatalf("a completed rebuild stops nothing, got %v", f.member.shutdowns)
	}
}

// A new Pod of the member does not start the database the restore holds, so
// the rebuild cannot complete: it ends Failed, and the new Pod is stopped.
func TestRebuildMemberPodReplacedAfterTheRestoreFails(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.restoreRecorded(instancemanager.OpCompleted)
	for _, o := range f.objs {
		if p, ok := o.(*corev1.Pod); ok && p.Name == cpTarget {
			p.UID = "pod-" + cpTarget + "-new"
		}
	}
	got, _, _ := f.pass()
	if last := got.Status.LastMemberRebuild; last == nil || last.State != databasev1alpha1.MemberRebuildFailed {
		t.Fatalf("a replaced member Pod ends the rebuild Failed, got %+v", last)
	}
	if len(f.member.releases) != 0 || len(f.member.shutdowns) != 1 {
		t.Fatalf("the member is stopped and never released, got releases %v stops %v", f.member.releases,
			f.member.shutdowns)
	}
}

// A rebuild that fails while its restore runs stops the member, as the
// restore may still finish and start heartbeat with data copied across a
// change of master (POC-57). The stop is sent again in every pass while the
// member answers as running, the cluster reports it Degraded, and the member
// is left for a person. A new empty claim ends the hold.
func TestRebuildFailedAfterTheRestoreStopsTheMember(t *testing.T) {
	f := newRebuildCopyFixture(t)
	f.restoreRecorded(instancemanager.OpRestoring)
	f.failSourcePod()
	got, _, _ := f.pass()
	if last := got.Status.LastMemberRebuild; last == nil || last.State != databasev1alpha1.MemberRebuildFailed {
		t.Fatalf("the rebuild ends Failed, got %+v", last)
	}
	if len(f.member.shutdowns) != 1 || f.member.shutdowns[0] != cpTarget {
		t.Fatalf("the member of the failed rebuild is stopped, got %v", f.member.shutdowns)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, conditionDegraded)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonRebuildFailed {
		t.Fatalf("the cluster is Degraded with %s, got %+v", reasonRebuildFailed, c)
	}
	if len(f.member.releases) != 0 {
		t.Fatalf("the member of a failed rebuild is never released, got %v", f.member.releases)
	}

	// The member answers as a running slave: it is stopped again.
	f.cluster = got
	running := answersSlave
	running.ObservedAt = f.now
	f.obs[cpTarget] = running
	got, _, _ = f.pass()
	if len(f.member.shutdowns) != 2 {
		t.Fatalf("a member that runs again is stopped again, got %v", f.member.shutdowns)
	}

	// The administrator replaced the claim: the hold ends.
	f.cluster = got
	f.obs[cpTarget] = answersUnknown
	for _, o := range f.objs {
		if c, ok := o.(*corev1.PersistentVolumeClaim); ok && c.Name == dataVolume+"-"+cpTarget {
			c.UID, c.Spec.VolumeName = "claim-"+cpTarget+"-new", "pv-"+cpTarget+"-new"
		}
	}
	got, _, _ = f.pass()
	if len(f.member.shutdowns) != 2 {
		t.Fatalf("a member on a new claim is not stopped, got %v", f.member.shutdowns)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, conditionDegraded); c != nil {
		t.Fatalf("the hold ends with a new claim, got %+v", c)
	}
}
