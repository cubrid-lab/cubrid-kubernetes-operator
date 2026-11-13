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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

	// reasonMemberLogClaimDeleted is the Warning Event recorded when the
	// log claim and the Pod of a split member were deleted.
	reasonMemberLogClaimDeleted = "MemberLogClaimDeleted"
	// reasonMemberClaimRecoveryFailed is the Ready reason when the log claim
	// or the Pod of a split member could not be deleted.
	reasonMemberClaimRecoveryFailed = "MemberClaimRecoveryFailed"
)

// recoverSplitMemberClaims deletes the log claim and the Pod of each member
// whose claims are split (splitMember). It returns when the members should be
// checked again: the end of the grace period of a split member that has not
// waited that long yet, or zero.
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

// recoverSplitMember deletes the log claim and the Pod of the member when its
// claims are split, and otherwise leaves everything as it is. The claims are
// split when all of these hold:
//
//   - the Pod is controlled by the cluster's StatefulSet, has no node, is
//     Pending and has PodScheduled=False with reason Unschedulable;
//   - no container of the Pod ever started;
//   - the data claim belongs to the StatefulSet and is Pending, with no
//     selected node and no volume;
//   - the log claim belongs to the StatefulSet and has a selected node, a
//     volume or the phase Bound;
//   - the Pod has been unschedulable for splitClaimsGrace.
//
// The wait until the grace period ends is returned when only the last does
// not hold yet.
func (r *CubridClusterReconciler) recoverSplitMember(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	sts *appsv1.StatefulSet, name string) (time.Duration, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: cluster.Namespace}, &pod); err != nil {
		return 0, client.IgnoreNotFound(err)
	}
	unschedulable := unschedulableCondition(&pod)
	if unschedulable == nil || !metav1.IsControlledBy(&pod, sts) || pod.DeletionTimestamp != nil ||
		pod.Spec.NodeName != "" || pod.Status.Phase != corev1.PodPending || everStarted(&pod) {
		return 0, nil
	}
	var data, log corev1.PersistentVolumeClaim
	for _, c := range []struct {
		pvc  *corev1.PersistentVolumeClaim
		name string
	}{{&data, "data-" + name}, {&log, engineLogVolume + "-" + name}} {
		if err := r.Get(ctx, client.ObjectKey{Name: c.name, Namespace: cluster.Namespace}, c.pvc); err != nil {
			return 0, client.IgnoreNotFound(err)
		}
		if !claimOfStatefulSet(c.pvc, sts) {
			return 0, nil
		}
	}
	dataUnbound := data.Status.Phase == corev1.ClaimPending && data.Spec.VolumeName == "" &&
		data.Annotations[selectedNodeAnnotation] == ""
	logNode := log.Annotations[selectedNodeAnnotation]
	logPinned := logNode != "" || log.Spec.VolumeName != "" || log.Status.Phase == corev1.ClaimBound
	if !dataUnbound || !logPinned {
		return 0, nil
	}
	if waited := r.now().Sub(unschedulable.LastTransitionTime.Time); waited < splitClaimsGrace {
		return splitClaimsGrace - waited, nil
	}

	// The preconditions refuse the delete if the log claim changed since it
	// was read, as when the scheduler is writing it after all. Kubernetes
	// also keeps a claim a scheduled Pod uses until that Pod is gone, and
	// this member's Pod has no node.
	if err := r.Delete(ctx, &log, client.Preconditions{UID: &log.UID, ResourceVersion: &log.ResourceVersion}); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return time.Second, nil
		}
		return 0, fmt.Errorf("delete PersistentVolumeClaim %s of member %s: %w", log.Name, name, err)
	}
	if err := r.Delete(ctx, &pod, client.Preconditions{UID: &pod.UID}); client.IgnoreNotFound(err) != nil {
		return 0, fmt.Errorf("delete Pod %s after deleting its log claim %s: %w", name, log.Name, err)
	}
	where := "on node " + logNode
	if logNode == "" {
		where = "to volume " + log.Spec.VolumeName
	}
	logf.FromContext(ctx).Info("Deleted the log claim and the Pod of a member whose log claim was bound without its data claim",
		"event", "member_log_claim_deleted", "namespace", cluster.Namespace, "cluster", cluster.Name,
		"member", name, "logClaim", log.Name, "logClaimNode", logNode, "logClaimVolume", log.Spec.VolumeName,
		"dataClaim", data.Name, "scheduler", unschedulable.Message)
	r.event(cluster, corev1.EventTypeWarning, reasonMemberLogClaimDeleted,
		fmt.Sprintf("member %s stayed unschedulable for %s: its log claim %s was bound %s while its data claim %s has no node; "+
			"deleted the log claim and the Pod so the StatefulSet creates them again and both claims are bound together; "+
			"the data claim was not changed. Scheduler: %s",
			name, splitClaimsGrace, log.Name, where, data.Name, unschedulable.Message))
	return 0, nil
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
