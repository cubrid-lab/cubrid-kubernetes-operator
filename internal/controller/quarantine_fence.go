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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// returnToServiceAnnotation on a CubridCluster names a member to return to
// service (ADR-0005, section 8, "Return to service").
const returnToServiceAnnotation = "database.cubrid.io/return-to-service"

// The Events of the out-of-service taint and of the return to service.
const (
	reasonWriteQuarantineReleasedOnTaint = "WriteQuarantineReleasedOnOutOfServiceTaint"
	reasonReturnToServiceWaiting         = "ReturnToServiceWaiting"
)

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get

// outOfServiceFences returns the node each member's Pod was last seen
// scheduled on, and the members whose node carries the taint
// node.kubernetes.io/out-of-service with the effect NoExecute in this pass
// (ADR-0005, section 8, "Unverified", item 2). The node is the one of the
// member's Pod read in this pass, or, while it has none or one not scheduled
// yet, the one status.memberPods recorded, as after Kubernetes force-deleted
// the Pod from the tainted node. Only the Node of a member that gave no
// active-master or non-master answer in this pass is read, from the API
// server past the cache. A Node that does not exist fences nothing; a failed
// read is returned, and fences nothing. The Operator only reads Nodes: it
// never adds or removes the taint, and never infers a fence from a Node's
// Lease or Ready condition.
func (r *CubridClusterReconciler) outOfServiceFences(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	members []string, obs map[string]RoleObservation, ids map[string]memberIdentity) (map[string]string,
	map[string]outOfServiceTaint, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	now := r.now()
	nodes := make(map[string]string, len(members))
	fences := map[string]outOfServiceTaint{}
	for _, m := range members {
		node := ids[m].NodeName
		if node == "" {
			if i := slices.IndexFunc(cluster.Status.MemberPods, func(p databasev1alpha1.MemberPod) bool {
				return p.Name == m
			}); i >= 0 {
				node = cluster.Status.MemberPods[i].NodeName
			}
		}
		nodes[m] = node
		if o := obs[m]; node == "" || activeMaster(o, now) || nonMasterAnswer(o, now) {
			continue
		}
		var n corev1.Node
		err := reader.Get(ctx, client.ObjectKey{Name: node}, &n)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nodes, nil, fmt.Errorf("read Node %s of member %s for its out-of-service taint: %w", node, m, err)
		}
		for _, t := range n.Spec.Taints {
			if t.Key == corev1.TaintNodeOutOfService && t.Effect == corev1.TaintEffectNoExecute {
				f := outOfServiceTaint{Node: node}
				if t.TimeAdded != nil {
					f.TimeAdded = t.TimeAdded.Time
				}
				fences[m] = f
				break
			}
		}
	}
	return nodes, fences, nil
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
	case value == "", value == cluster.Status.ReturnedToService:
		return "", ""
	case !slices.Contains(members, value):
		return "", fmt.Sprintf("%q is not a promotable member of the cluster", value)
	}
	return value, ""
}
