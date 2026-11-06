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
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	// memberSpreadDefaultAnnotation on a CubridCluster records the
	// memberSpread that applies while spec.placement.memberSpread is unset.
	// The operator writes it once, before it creates the StatefulSet, and
	// never changes it.
	memberSpreadDefaultAnnotation = "database.cubrid.io/member-spread-default"
	// selectedNodeAnnotation is the node the scheduler chose for a PVC whose
	// StorageClass waits for the first consumer; a volume bound to one node,
	// such as local-path, stays on it.
	selectedNodeAnnotation = "volume.kubernetes.io/selected-node"

	// conditionMemberPlacement reports where the members of an HA cluster
	// run. It is a report only: the operator never moves, deletes or
	// replaces a Pod or a PVC because of it.
	conditionMemberPlacement = "MemberPlacement"

	reasonMembersOnSeparateNodes = "MembersOnSeparateNodes"
	reasonMembersShareNode       = "MembersShareNode"
	reasonSpreadBlockedByVolume  = "SpreadBlockedByVolume"
	reasonMemberUnschedulable    = "MemberUnschedulable"
	reasonMembersNotScheduled    = "MembersNotScheduled"
	reasonPlacementNotObserved   = "PlacementNotObserved"
)

// spreadsMembers reports whether the cluster's members get a placement rule:
// a standalone cluster has one member and none.
func spreadsMembers(cluster *databasev1alpha1.CubridCluster) bool {
	return cluster.Spec.Topology.PromotableMembers >= 2
}

// memberSpread is the memberSpread that applies: the spec's when it is set,
// else the one recorded in memberSpreadDefaultAnnotation. A cluster with no
// recorded Required only prefers separate nodes, which never leaves a member
// Pending behind a volume bound to a node another member uses.
func memberSpread(cluster *databasev1alpha1.CubridCluster) databasev1alpha1.MemberSpread {
	if p := cluster.Spec.Placement; p != nil && p.MemberSpread != "" {
		return p.MemberSpread
	}
	if cluster.Annotations[memberSpreadDefaultAnnotation] == string(databasev1alpha1.MemberSpreadRequired) {
		return databasev1alpha1.MemberSpreadRequired
	}
	return databasev1alpha1.MemberSpreadPreferred
}

// memberAffinity keeps the members of an HA cluster on different nodes, so
// that one node's failure takes at most one member (#90). Required is a
// scheduling rule: a member with no node of its own stays Pending. Preferred
// is a scheduler preference only. A standalone cluster gets none. The rule
// selects this cluster's own DB Pods only.
func memberAffinity(cluster *databasev1alpha1.CubridCluster) *corev1.Affinity {
	if !spreadsMembers(cluster) {
		return nil
	}
	term := corev1.PodAffinityTerm{
		TopologyKey:   corev1.LabelHostname,
		LabelSelector: &metav1.LabelSelector{MatchLabels: labelsFor(cluster)},
	}
	if memberSpread(cluster) == databasev1alpha1.MemberSpreadPreferred {
		return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 100, PodAffinityTerm: term}},
		}}
	}
	return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{term},
	}}
}

// recordMemberSpreadDefault writes memberSpreadDefaultAnnotation once, before
// the StatefulSet is created: Required for a cluster that has neither a
// StatefulSet it controls nor a data PVC, Preferred for one that has either.
// Such a cluster was created by an operator without the rule, and its members
// may share a node their volumes are bound to; under Required a recreated
// member would stay Pending there. An annotation that is already present is
// kept.
func (r *CubridClusterReconciler) recordMemberSpreadDefault(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	if !spreadsMembers(cluster) {
		return nil
	}
	if _, ok := cluster.Annotations[memberSpreadDefaultAnnotation]; ok {
		return nil
	}
	existing, err := r.memberStateExists(ctx, cluster)
	if err != nil {
		return err
	}
	value := databasev1alpha1.MemberSpreadRequired
	if existing != "" {
		value = databasev1alpha1.MemberSpreadPreferred
	}
	patch := client.MergeFromWithOptions(cluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[memberSpreadDefaultAnnotation] = string(value)
	if err := r.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("record annotation %s: %w", memberSpreadDefaultAnnotation, err)
	}
	logf.FromContext(ctx).Info("Recorded the default member spread of the CubridCluster",
		"memberSpread", string(value), "existingObject", existing)
	return nil
}

// memberStateExists names the cluster's StatefulSet or one of its data PVCs
// (data-<cluster>-<ordinal>) when one exists, read past the cache. It is
// empty when there is none.
//
// A StatefulSet counts only when this cluster controls it, by UID, as when
// the reconcile decides whether to change it: a StatefulSet of the same name
// that someone else, or an earlier cluster, holds is no evidence of this
// cluster's members, and the reconcile refuses it. Counting it would record
// Preferred for good on a new cluster.
//
// A data PVC counts by its name alone. The StatefulSet controller creates it
// from the volume claim template with no owner reference, so a claim of an
// earlier cluster of the same name cannot be told from one of this cluster's
// members. If it is the claim of an older member, its volume may bind that
// member to a node another member uses, and Required would leave it Pending;
// Preferred never does. The cost is that a stale or unrelated claim with such
// a name makes a new cluster record Preferred until memberSpread is set in
// the spec.
func (r *CubridClusterReconciler) memberStateExists(ctx context.Context, cluster *databasev1alpha1.CubridCluster) (string, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var sts appsv1.StatefulSet
	err := reader.Get(ctx, client.ObjectKeyFromObject(cluster), &sts)
	if err == nil {
		if metav1.IsControlledBy(&sts, cluster) {
			return "StatefulSet " + cluster.Name, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("read StatefulSet %s: %w", cluster.Name, err)
	}
	var pvcs corev1.PersistentVolumeClaimList
	if err := reader.List(ctx, &pvcs, client.InNamespace(cluster.Namespace)); err != nil {
		return "", fmt.Errorf("list PersistentVolumeClaims: %w", err)
	}
	prefix := "data-" + cluster.Name + "-"
	for _, pvc := range pvcs.Items {
		ordinal, ok := strings.CutPrefix(pvc.Name, prefix)
		if ok && ordinal != "" && strings.Trim(ordinal, "0123456789") == "" {
			return "PersistentVolumeClaim " + pvc.Name, nil
		}
	}
	return "", nil
}

// placementEntry is the last decisive MemberPlacement reason of a cluster.
type placementEntry struct {
	uid    types.UID
	reason string
}

// reportMemberPlacement sets MemberPlacement from the members' Pods and data
// PVCs. An Event is recorded when a True or False observation differs from
// the last True or False one: a Warning for a problem, a Normal one for the
// return to separate nodes. An Unknown observation, as while a member's Pod
// is recreated, records none and is not compared with. A standalone cluster
// has no such condition.
func (r *CubridClusterReconciler) reportMemberPlacement(ctx context.Context, cluster *databasev1alpha1.CubridCluster) {
	if !spreadsMembers(cluster) {
		meta.RemoveStatusCondition(&cluster.Status.Conditions, conditionMemberPlacement)
		return
	}
	last := ""
	if was, ok := r.placementReported.Load(client.ObjectKeyFromObject(cluster)); ok &&
		was.(placementEntry).uid == cluster.UID {
		last = was.(placementEntry).reason
	} else if was := meta.FindStatusCondition(cluster.Status.Conditions, conditionMemberPlacement); decisive(was) {
		// After a restart of the manager, the stored condition is all that
		// is known of what was reported.
		last = was.Reason
	}
	status, reason, msg := r.observeMemberPlacement(ctx, cluster)
	setCondition(cluster, conditionMemberPlacement, status, reason, msg)
	if status == metav1.ConditionUnknown || reason == last {
		return
	}
	switch {
	case status == metav1.ConditionFalse:
		r.statusEvent(cluster, corev1.EventTypeWarning, reason, msg)
	case placementProblem(last):
		r.statusEvent(cluster, corev1.EventTypeNormal, reason, msg)
	}
	entry := placementEntry{uid: cluster.UID, reason: reason}
	if report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster)); ok {
		report.(*reconcileReport).placement = &entry
		return
	}
	r.placementReported.Store(client.ObjectKeyFromObject(cluster), entry)
}

func placementProblem(reason string) bool {
	return reason == reasonMembersShareNode || reason == reasonSpreadBlockedByVolume || reason == reasonMemberUnschedulable
}

// observeMemberPlacement reads the members' Pods and, under Required, their
// data PVCs. Members on one node are reported first, then volumes that keep
// a member from a node of its own, then members the scheduler found no node
// for; the message names every finding.
func (r *CubridClusterReconciler) observeMemberPlacement(ctx context.Context,
	cluster *databasev1alpha1.CubridCluster) (metav1.ConditionStatus, string, string) {
	members := memberNames(cluster, cluster.Spec.Topology.PromotableMembers)
	onNode := map[string][]string{}
	var unschedulable, unscheduled []string
	for _, name := range members {
		var pod corev1.Pod
		if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: cluster.Namespace}, &pod); apierrors.IsNotFound(err) {
			unscheduled = append(unscheduled, name)
			continue
		} else if err != nil {
			return metav1.ConditionUnknown, reasonPlacementNotObserved, fmt.Sprintf("could not read Pod %s: %v", name, err)
		}
		if pod.Spec.NodeName != "" {
			onNode[pod.Spec.NodeName] = append(onNode[pod.Spec.NodeName], name)
			continue
		}
		unscheduled = append(unscheduled, name)
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
				unschedulable = append(unschedulable, fmt.Sprintf("member %s is Pending: %s", name, c.Message))
			}
		}
	}

	var shared, blocked []string
	for _, node := range slices.Sorted(maps.Keys(onNode)) {
		if names := onNode[node]; len(names) > 1 {
			shared = append(shared, fmt.Sprintf("members %s run on node %s", strings.Join(names, ", "), node))
		}
	}
	if memberSpread(cluster) == databasev1alpha1.MemberSpreadRequired {
		volumes := map[string][]string{}
		for _, name := range members {
			var pvc corev1.PersistentVolumeClaim
			err := r.Get(ctx, client.ObjectKey{Name: "data-" + name, Namespace: cluster.Namespace}, &pvc)
			if apierrors.IsNotFound(err) {
				continue
			} else if err != nil {
				return metav1.ConditionUnknown, reasonPlacementNotObserved,
					fmt.Sprintf("could not read PersistentVolumeClaim data-%s: %v", name, err)
			}
			if node := pvc.Annotations[selectedNodeAnnotation]; node != "" {
				volumes[node] = append(volumes[node], pvc.Name)
			}
		}
		for _, node := range slices.Sorted(maps.Keys(volumes)) {
			if names := volumes[node]; len(names) > 1 {
				blocked = append(blocked, fmt.Sprintf("the volumes of %s are bound to node %s, "+
					"so under memberSpread Required only one of their members can run; "+
					"to spread the members, restore a backup into a new cluster", strings.Join(names, ", "), node))
			}
		}
	}

	findings := slices.Concat(shared, blocked, unschedulable)
	msg := strings.Join(findings, "; ")
	switch {
	case len(shared) > 0:
		return metav1.ConditionFalse, reasonMembersShareNode, msg
	case len(blocked) > 0:
		return metav1.ConditionFalse, reasonSpreadBlockedByVolume, msg
	case len(unschedulable) > 0:
		return metav1.ConditionFalse, reasonMemberUnschedulable, msg
	case len(unscheduled) > 0:
		return metav1.ConditionUnknown, reasonMembersNotScheduled,
			fmt.Sprintf("members %s have no node yet", strings.Join(unscheduled, ", "))
	}
	return metav1.ConditionTrue, reasonMembersOnSeparateNodes,
		fmt.Sprintf("each of the %d members runs on a node of its own", len(members))
}
