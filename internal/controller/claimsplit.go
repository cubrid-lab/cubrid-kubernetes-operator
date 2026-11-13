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
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// A member has two claims, data and log, both of the data volume's
// StorageClass. With a StorageClass that waits for the first consumer, the
// scheduler writes the selected node on one claim after the other, and does
// not undo the first when a later write fails. On the VM lab the write of the
// data claim lost a race with the StatefulSet controller, which was adding the
// owner references of the Delete retention policy. The log claim's volume was
// then provisioned on a node the required member spread forbids to the
// member, and the Pod stayed unschedulable for good (#856).
//
// The Operator undoes only that split: it deletes the log claim and the Pod,
// and the StatefulSet controller creates both again, so the scheduler binds
// both claims together. A data claim is never deleted or changed.
const (
	// splitClaimsGrace is how long a member's Pod must have been
	// unschedulable before its claims count as split. A binding in progress
	// sets the nodes of both claims within seconds; the grace keeps the
	// Operator from deleting a claim the scheduler is still writing.
	splitClaimsGrace = 60 * time.Second
	// splitClaimsRepairInterval is the least time between two repairs of
	// the same member. A split that comes back sooner is reported and left
	// alone, so that a cause the repair does not remove cannot make the
	// Operator delete the member's log claim again and again.
	splitClaimsRepairInterval = 10 * time.Minute
	// claimCreationSkew is how much older than the data claim the log claim
	// may be. Creation times have a resolution of one second, and the
	// StatefulSet controller creates both claims of a member together.
	claimCreationSkew = 2 * time.Second

	// reasonMemberLogClaimDeleted is the Warning Event recorded when the
	// log claim and the Pod of a split member were deleted.
	reasonMemberLogClaimDeleted = "MemberLogClaimDeleted"
	// reasonMemberClaimsStillSplit is the Warning Event recorded once when a
	// member is found split again within splitClaimsRepairInterval of its
	// last repair.
	reasonMemberClaimsStillSplit = "MemberClaimsStillSplit"
	// reasonMemberClaimRecoveryFailed is the Ready reason when the log claim
	// or the Pod of a split member could not be deleted.
	reasonMemberClaimRecoveryFailed = "MemberClaimRecoveryFailed"
)

// claimRepairKey names one member of one cluster; the UID keeps a cluster
// created again under the same name apart.
type claimRepairKey struct {
	cluster types.UID
	member  string
}

// claimRepair is the last repair of a member's claims.
type claimRepair struct {
	at time.Time
	// reported is set once the split was seen again and reported.
	reported bool
}

// claimRepairs holds the last repair of each member, in the manager's memory.
// A restart of the manager forgets them, so a member can be repaired once
// more after a restart.
type claimRepairs struct {
	mu   sync.Mutex
	last map[claimRepairKey]claimRepair
}

// recent returns the last repair of the member when it was made less than
// splitClaimsRepairInterval before now.
func (c *claimRepairs) recent(key claimRepairKey, now time.Time) (claimRepair, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.last[key]
	if !ok || now.Sub(last.at) >= splitClaimsRepairInterval {
		delete(c.last, key)
		return claimRepair{}, false
	}
	return last, true
}

func (c *claimRepairs) record(key claimRepairKey, repair claimRepair) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[claimRepairKey]claimRepair{}
	}
	c.last[key] = repair
}

// recoverSplitMemberClaims deletes the log claim and the Pod of each member
// whose claims are split (splitMember). It returns when the members should be
// checked again: the end of the grace period or of the repair interval of a
// split member, or zero.
func (r *CubridClusterReconciler) recoverSplitMemberClaims(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	sts *appsv1.StatefulSet) (time.Duration, error) {
	if !hasEngineLogClaims(sts) || sts.Spec.Selector == nil {
		return 0, nil
	}
	var recheck time.Duration
	for _, name := range memberNames(cluster, cluster.Spec.Topology.PromotableMembers) {
		wait, err := r.recoverSplitMember(ctx, cluster, sts, name)
		if err != nil {
			return 0, err
		}
		if wait > 0 && (recheck == 0 || wait < recheck) {
			recheck = wait
		}
	}
	return recheck, nil
}

// hasEngineLogClaims reports whether the StatefulSet gives each member a log
// claim. A StatefulSet an earlier build created has none.
func hasEngineLogClaims(sts *appsv1.StatefulSet) bool {
	for _, t := range sts.Spec.VolumeClaimTemplates {
		if t.Name == engineLogVolume {
			return true
		}
	}
	return false
}

// splitMember is a member whose claims are split, as read.
type splitMember struct {
	pod           corev1.Pod
	data, log     corev1.PersistentVolumeClaim
	unschedulable *corev1.PodCondition
}

// readSplitMember reads the member's Pod and claims through reader and
// returns them when the claims are split, nil otherwise. They are split when
// all of these hold:
//
//   - the Pod is controlled by the cluster's StatefulSet, is not being
//     deleted, has no node, is Pending and has PodScheduled=False with reason
//     Unschedulable (splitPod);
//   - no container of the Pod ever started;
//   - both claims belong to the StatefulSet (claimOfStatefulSet);
//   - the data claim is Pending, with no selected node and no volume;
//   - the log claim has the node the scheduler selected for it;
//   - the log claim is not older than the data claim, so it is not the log
//     claim of a member that ran before its data claim was replaced.
//
// The grace period is not checked here.
func readSplitMember(ctx context.Context, reader client.Reader, sts *appsv1.StatefulSet,
	namespace, name string) (*splitMember, error) {
	m := &splitMember{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &m.pod); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if m.unschedulable = splitPod(&m.pod, sts); m.unschedulable == nil {
		return nil, nil
	}
	for _, c := range []struct {
		pvc  *corev1.PersistentVolumeClaim
		name string
	}{{&m.data, "data-" + name}, {&m.log, engineLogVolume + "-" + name}} {
		if err := reader.Get(ctx, client.ObjectKey{Name: c.name, Namespace: namespace}, c.pvc); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		if !claimOfStatefulSet(c.pvc, sts) {
			return nil, nil
		}
	}
	dataUnbound := m.data.Status.Phase == corev1.ClaimPending && m.data.Spec.VolumeName == "" &&
		m.data.Annotations[selectedNodeAnnotation] == ""
	logPinned := m.log.Annotations[selectedNodeAnnotation] != ""
	logNotOlder := !m.log.CreationTimestamp.Add(claimCreationSkew).Before(m.data.CreationTimestamp.Time)
	if !dataUnbound || !logPinned || !logNotOlder {
		return nil, nil
	}
	return m, nil
}

// splitPod returns the Pod's Unschedulable condition when the Pod is one the
// repair may delete: controlled by the StatefulSet, not being deleted, with
// no node, Pending, and with no container that ever started. It returns nil
// otherwise.
func splitPod(pod *corev1.Pod, sts *appsv1.StatefulSet) *corev1.PodCondition {
	if !metav1.IsControlledBy(pod, sts) || pod.DeletionTimestamp != nil || pod.Spec.NodeName != "" ||
		pod.Status.Phase != corev1.PodPending || everStarted(pod) {
		return nil
	}
	return unschedulableCondition(pod)
}

// recoverSplitMember deletes the log claim and the Pod of the member when its
// claims are split (readSplitMember), its Pod has been unschedulable for
// splitClaimsGrace, and the member was not repaired in the last
// splitClaimsRepairInterval. The cache is read first; the API server is then
// read again, and every condition is checked again on what it returns, so a
// cache that has not caught up with the scheduler cannot cause a delete.
//
// It returns the wait until the member should be checked again, when it
// only waits for the grace period or the repair interval to end.
func (r *CubridClusterReconciler) recoverSplitMember(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	sts *appsv1.StatefulSet, name string) (time.Duration, error) {
	cached, err := readSplitMember(ctx, r.Client, sts, cluster.Namespace, name)
	if cached == nil || err != nil {
		return 0, err
	}
	now := r.now()
	if waited := now.Sub(cached.unschedulable.LastTransitionTime.Time); waited < splitClaimsGrace {
		return splitClaimsGrace - waited, nil
	}
	key := claimRepairKey{cluster: cluster.UID, member: name}
	if last, ok := r.claimRepairs.recent(key, now); ok {
		if !last.reported {
			last.reported = true
			r.claimRepairs.record(key, last)
			r.event(cluster, corev1.EventTypeWarning, reasonMemberClaimsStillSplit,
				fmt.Sprintf("member %s is unschedulable again with its log claim %s on node %s and its data claim %s with no node, "+
					"%s after its log claim and Pod were deleted for the same reason; the Operator repairs a member at most once in %s "+
					"and leaves it as it is until then. Scheduler: %s",
					name, cached.log.Name, cached.log.Annotations[selectedNodeAnnotation], cached.data.Name,
					now.Sub(last.at).Round(time.Second), splitClaimsRepairInterval, cached.unschedulable.Message))
		}
		return splitClaimsRepairInterval - now.Sub(last.at), nil
	}

	fresh, err := readSplitMember(ctx, r.apiReader(), sts, cluster.Namespace, name)
	if fresh == nil || err != nil {
		return 0, err
	}
	if waited := now.Sub(fresh.unschedulable.LastTransitionTime.Time); waited < splitClaimsGrace {
		return splitClaimsGrace - waited, nil
	}
	return 0, r.deleteSplitMember(ctx, cluster, sts, key, fresh)
}

// apiReader reads from the API server, or through Client when the reconciler
// has no APIReader.
func (r *CubridClusterReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// deleteSplitMember deletes the log claim and then the Pod of a split member,
// each only if it has not changed since the API server's read. The log claim
// goes first, as in the manual repair that worked on the VM lab: a Pod
// deleted first is created again at once and would be scheduled against the
// same pinned claim. If the Pod's delete then fails, the error is returned and
// the Pod stays Pending without its log claim; the StatefulSet controller
// creates the missing claims of a Pending Pod, with no node, so the scheduler
// can bind both claims together (upstream StatefulSet behavior, not tested
// here). The member is recorded as repaired once its log claim is deleted.
func (r *CubridClusterReconciler) deleteSplitMember(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	sts *appsv1.StatefulSet, key claimRepairKey, m *splitMember) error {
	log, pod := &m.log, &m.pod
	if err := r.Delete(ctx, log, client.Preconditions{UID: &log.UID, ResourceVersion: &log.ResourceVersion}); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// Changed or gone since the read: the next reconcile reads again.
			return nil
		}
		return fmt.Errorf("delete PersistentVolumeClaim %s of member %s: %w", log.Name, key.member, err)
	}
	r.claimRepairs.record(key, claimRepair{at: r.now()})
	podErr := client.IgnoreNotFound(r.Delete(ctx, pod,
		client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}))
	if apierrors.IsConflict(podErr) {
		// The Pod's status changed since the read; it is deleted if it still
		// is the Pod of the split member.
		podErr = r.deleteSplitPod(ctx, sts, pod)
	}
	node := log.Annotations[selectedNodeAnnotation]
	logf.FromContext(ctx).Info("Deleted the log claim of a member whose log claim was bound without its data claim",
		"event", "member_log_claim_deleted", "namespace", cluster.Namespace, "cluster", cluster.Name,
		"member", key.member, "logClaim", log.Name, "logClaimNode", node, "logClaimVolume", log.Spec.VolumeName,
		"dataClaim", m.data.Name, "podDeleted", podErr == nil, "scheduler", m.unschedulable.Message)
	r.event(cluster, corev1.EventTypeWarning, reasonMemberLogClaimDeleted,
		fmt.Sprintf("member %s stayed unschedulable for %s: its log claim %s was bound on node %s while its data claim %s has no node; "+
			"deleted the log claim and the Pod so the StatefulSet creates them again and both claims are bound together; "+
			"the data claim was not changed. Scheduler: %s",
			key.member, splitClaimsGrace, log.Name, node, m.data.Name, m.unschedulable.Message))
	if podErr != nil {
		return fmt.Errorf("delete Pod %s after deleting its log claim %s: %w", key.member, log.Name, podErr)
	}
	return nil
}

// deleteSplitPod reads the Pod from the API server again and deletes it if it
// still is the same Pod and still is one the repair may delete (splitPod).
func (r *CubridClusterReconciler) deleteSplitPod(ctx context.Context, sts *appsv1.StatefulSet, was *corev1.Pod) error {
	var pod corev1.Pod
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(was), &pod); err != nil {
		return client.IgnoreNotFound(err)
	}
	if pod.UID != was.UID || splitPod(&pod, sts) == nil {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &pod,
		client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}))
}

// unschedulableCondition is the Pod's PodScheduled condition when it is False
// with reason Unschedulable, nil otherwise.
func unschedulableCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// everStarted reports whether a container of the Pod runs, ended or was
// restarted: the Pod may then have written to its log volume.
func everStarted(pod *corev1.Pod) bool {
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, s := range statuses {
			if s.RestartCount > 0 || s.State.Running != nil || s.State.Terminated != nil ||
				s.LastTerminationState.Running != nil || s.LastTerminationState.Terminated != nil {
				return true
			}
		}
	}
	return false
}

// claimOfStatefulSet reports whether a member claim was created by the
// cluster's StatefulSet: it carries the StatefulSet's selector labels, it was
// created no earlier than the StatefulSet, it is not being deleted, and every
// owner it names is that StatefulSet. The StatefulSet controller sets no
// owner under the Retain policy, and the StatefulSet itself under Delete.
func claimOfStatefulSet(pvc *corev1.PersistentVolumeClaim, sts *appsv1.StatefulSet) bool {
	if pvc.DeletionTimestamp != nil || pvc.CreationTimestamp.Before(&sts.CreationTimestamp) ||
		!labels.SelectorFromSet(sts.Spec.Selector.MatchLabels).Matches(labels.Set(pvc.Labels)) {
		return false
	}
	for _, ref := range pvc.OwnerReferences {
		if ref.UID != sts.UID {
			return false
		}
	}
	return true
}
