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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// returnToServiceAnnotation on a CubridCluster names a member to return to
// service (ADR-0005, section 8, "Return to service").
const returnToServiceAnnotation = "database.cubrid.io/return-to-service"

// The Events of the out-of-service taint and of the return to service.
const (
	reasonWriteQuarantineReleasedOnTaint = "WriteQuarantineReleasedOnOutOfServiceTaint"
	reasonReturnToServiceWaiting         = "ReturnToServiceWaiting"
	reasonOutOfServiceTaintRefused       = "OutOfServiceTaintRefused"
)

// silent reports whether o shows no answer at all from the member in this
// pass: it was not asked, nothing answered, or the answer is not fresh. Any
// HTTP response, a refused token or a server error included, is an answer:
// the member's process runs.
func silent(o RoleObservation, now time.Time) bool {
	if !o.Reachable && !o.TokenRefused && !o.Responded || o.ObservedAt.IsZero() {
		return true
	}
	age := now.Sub(o.ObservedAt)
	return age > roleObservationTTL || age < -roleObservationTTL
}

// answerText says what a member answered, for a refused taint.
func answerText(o RoleObservation, now time.Time) string {
	if !o.Reachable && !o.TokenRefused && o.Reason != "" {
		return o.Reason
	}
	return observedRoleText(o, now)
}

// laterUnseen reports whether t, a Pod of a member deleted without a
// confirmed termination, with the UID uid, is a Pod that may have run on
// another node than the one p maps the member to: not the Pod the node was
// read from (p.NodePodUID), and created no earlier than it, or at a time not
// known. The deletion of the Pod the node was read from, as when Kubernetes
// force-deletes it from a node tainted out of service, and of an earlier Pod,
// do not count.
func laterUnseen(p databasev1alpha1.MemberPod, uid string, t podTermination) bool {
	from := p.NodePodUID
	if from == "" {
		from = p.PodUID
	}
	return t.member == p.Name && uid != from &&
		(p.NodePodCreated == nil || t.created.IsZero() || !t.created.Before(p.NodePodCreated.Time))
}

// unseenLaterPod reports whether the Pod watch holds such a deletion
// (laterUnseen) for the member p describes. recordTerminations then clears the
// mapping in status, so it does not come back when the watch forgets it.
func (r *CubridClusterReconciler) unseenLaterPod(cluster *databasev1alpha1.CubridCluster, p databasev1alpha1.MemberPod) bool {
	key := client.ObjectKeyFromObject(cluster)
	found := false
	r.podUnconfirmed.Range(func(uid, v any) bool {
		if t := v.(podTermination); t.cluster == key && laterUnseen(p, string(uid.(types.UID)), t) {
			found = true
			return false
		}
		return true
	})
	return found
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get

// outOfServiceFences returns the node each member's Pod was last seen
// scheduled on, the members whose node carries the taint
// node.kubernetes.io/out-of-service with the effect NoExecute in this pass
// (ADR-0005, section 8, "Unverified", item 2), and, by member, why a taint
// seen on its node does not fence it. The node is the one of the member's Pod
// read in this pass, or, while it has none or one not scheduled yet, the one
// status.memberPods recorded, as after Kubernetes force-deleted the Pod from
// the tainted node; not when the Pod watch saw a later Pod of the member
// deleted without a confirmed termination, which may have run elsewhere.
//
// Only a member that gave no answer at all in this pass (silent) is fenced.
// Any answer, even one without a trusted role, shows that the node the
// administrator asserted powered off is up: the taint is refused for that
// member. So is a taint without timeAdded, whose continuity cannot be
// checked. The Node is read, from the API server past the cache, for every
// member that gave no active-master or trusted non-master answer, and for a
// member with a fence record. A Node that does not exist fences nothing; a
// failed read is returned, and fences nothing. The Operator only reads Nodes:
// it never adds or removes the taint, and never infers a fence from a Node's
// Lease or Ready condition.
func (r *CubridClusterReconciler) outOfServiceFences(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	members []string, obs map[string]RoleObservation, ids map[string]memberIdentity) (map[string]string,
	map[string]outOfServiceTaint, map[string]string, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	now := r.now()
	nodes := make(map[string]string, len(members))
	fences, refused := map[string]outOfServiceTaint{}, map[string]string{}
	for _, m := range members {
		node := ids[m].NodeName
		if i := slices.IndexFunc(cluster.Status.MemberPods, func(p databasev1alpha1.MemberPod) bool {
			return p.Name == m
		}); node == "" && i >= 0 && !r.unseenLaterPod(cluster, cluster.Status.MemberPods[i]) {
			node = cluster.Status.MemberPods[i].NodeName
		}
		nodes[m] = node
		o := obs[m]
		recorded := slices.ContainsFunc(cluster.Status.MemberFences, func(f databasev1alpha1.MemberFence) bool {
			return f.Name == m
		})
		if node == "" || (!recorded && (activeMaster(o, now) || nonMasterAnswer(o, now))) {
			continue
		}
		var n corev1.Node
		err := reader.Get(ctx, client.ObjectKey{Name: node}, &n)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nodes, nil, refused, fmt.Errorf("read Node %s of member %s for its out-of-service taint: %w", node, m, err)
		}
		i := slices.IndexFunc(n.Spec.Taints, func(t corev1.Taint) bool {
			return t.Key == corev1.TaintNodeOutOfService && t.Effect == corev1.TaintEffectNoExecute
		})
		switch {
		case i < 0:
		case !silent(o, now):
			refused[m] = fmt.Sprintf("%s answered (%s) although its node %s carries the taint %s:%s that asserts the "+
				"node is powered off; the taint contradicts a live answer and does not fence the member",
				m, answerText(o, now), node, corev1.TaintNodeOutOfService, corev1.TaintEffectNoExecute)
		case n.Spec.Taints[i].TimeAdded == nil:
			refused[m] = fmt.Sprintf("the taint %s:%s on the node %s of %s has no timeAdded, so the Operator cannot "+
				"tell whether it was removed and added again; it does not fence the member (kubectl taint sets "+
				"timeAdded on a NoExecute taint)", corev1.TaintNodeOutOfService, corev1.TaintEffectNoExecute, node, m)
		default:
			fences[m] = outOfServiceTaint{Node: node, NodeUID: string(n.UID), TimeAdded: n.Spec.Taints[i].TimeAdded.Time}
		}
	}
	return nodes, fences, refused, nil
}

// returnRequest is the member the return-to-service annotation of cluster
// asks to return, "" when there is none or it is ignored, and why an
// annotation is ignored. It clears status.returnedToService once the
// annotation no longer names it: an annotation whose return was released is
// ignored until its value changes (ADR-0005, section 8, "One use").
func returnRequest(cluster *databasev1alpha1.CubridCluster, members []string) (string, string) {
	value := cluster.Annotations[returnToServiceAnnotation]
	if done := cluster.Status.ReturnedToService; done != "" && value != done {
		cluster.Status.ReturnedToService = ""
	}
	switch {
	case value == "":
		return "", ""
	case value == cluster.Status.ReturnedToService:
		return "", fmt.Sprintf("the return of %s to service was already released; remove the annotation and set "+
			"it again to return the member again", value)
	case !slices.Contains(members, value):
		return "", fmt.Sprintf("%q is not a promotable member of the cluster", value)
	}
	return value, ""
}

// reportRefusedTaints reports each refused out-of-service taint with a
// Warning Event, once per member and message, and logs it.
func (r *CubridClusterReconciler) reportRefusedTaints(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	refused map[string]string) {
	state := r.quarantineState(cluster)
	if state.reportedTaints == nil {
		state.reportedTaints = map[string]string{}
	}
	for m := range state.reportedTaints {
		if refused[m] == "" {
			delete(state.reportedTaints, m)
		}
	}
	for m, msg := range refused {
		if state.reportedTaints[m] == msg {
			continue
		}
		state.reportedTaints[m] = msg
		logf.FromContext(ctx).Info("Did not count an out-of-service taint as a fence", "member", m, "reason", msg)
		r.event(cluster, corev1.EventTypeWarning, reasonOutOfServiceTaintRefused, msg)
	}
}
