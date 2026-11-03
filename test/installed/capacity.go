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

package installed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// checkNodeFreeSpace reads, before the run creates anything, the free space
// of kubelet's root filesystem (nodefs) on every Ready node a member could be
// scheduled to. On the VM lab, local-path volumes and container images share
// that filesystem; elsewhere they may not. kubelet evicts Pods when it runs low,
// and a run's members can all land on one node, so a run that starts on a
// full node can evict the shared clusters and its own volumes' delete helpers
// with them. A node under cfg.MinNodeFreeSpace refuses the run; a minimum of
// zero reads nothing, and so does a preallocated namespace's run, whose
// cluster-wide reads are limited to fixed names. The space read is recorded
// in bytes per node.
func (r *Runner) checkNodeFreeSpace(ctx context.Context, cfg Config, rec *record) error {
	if cfg.MinNodeFreeSpace <= 0 || cfg.NamespaceMode == PreallocatedNamespace {
		return nil
	}
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes); err != nil {
		return fmt.Errorf("list the nodes to read their free space: %w", err)
	}
	rec.NodeFreeBytes = map[string]int64{}
	var short []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !schedulable(node) {
			continue
		}
		// A node going down answers no proxy read; the bound keeps it from
		// holding the run until its deadline.
		out, err := r.Command(ctx, "get", "--raw", "/api/v1/nodes/"+node.Name+"/proxy/stats/summary",
			"--request-timeout=30s")
		if err != nil {
			return fmt.Errorf("read the free space of node %s: %w", node.Name, err)
		}
		var summary struct {
			Node struct {
				Fs *struct {
					AvailableBytes *int64 `json:"availableBytes"`
				} `json:"fs"`
			} `json:"node"`
		}
		if err := json.Unmarshal([]byte(out), &summary); err != nil || summary.Node.Fs == nil ||
			summary.Node.Fs.AvailableBytes == nil {
			return fmt.Errorf("the free space of node %s was not reported", node.Name)
		}
		free := *summary.Node.Fs.AvailableBytes
		rec.NodeFreeBytes[node.Name] = free
		if free < cfg.MinNodeFreeSpace {
			short = append(short, fmt.Sprintf("%s has %d bytes free", node.Name, free))
		}
	}
	if len(short) > 0 {
		return fmt.Errorf("a node has less free space than the %d bytes a run needs (-min-node-free-space); no "+
			"resources were created: %s", cfg.MinNodeFreeSpace, strings.Join(short, ", "))
	}
	return nil
}

// FreeSpaceFlag is a command-line flag that sets *bytes from a quantity such
// as 8Gi, starting from def.
func FreeSpaceFlag(bytes *int64, def string) flagValue {
	quantity := resource.MustParse(def)
	*bytes = quantity.Value()
	return flagValue{bytes: bytes}
}

type flagValue struct{ bytes *int64 }

func (f flagValue) String() string {
	if f.bytes == nil {
		return ""
	}
	return resource.NewQuantity(*f.bytes, resource.BinarySI).String()
}

func (f flagValue) Set(value string) error {
	quantity, err := resource.ParseQuantity(value)
	if err != nil || quantity.Sign() < 0 {
		return fmt.Errorf("want a size such as 8Gi, or 0 to read nothing: %q", value)
	}
	*f.bytes = quantity.Value()
	return nil
}

// schedulable reports whether a member Pod, which tolerates no taint of its
// own, could be scheduled to node now: Ready, not cordoned and not tainted.
func schedulable(node *corev1.Node) bool {
	ready := slices.ContainsFunc(node.Status.Conditions, func(c corev1.NodeCondition) bool {
		return c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue
	})
	if !ready || node.Spec.Unschedulable {
		return false
	}
	return !slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool {
		return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
	})
}

// refuseLeftoverVolumes refuses a fresh-namespace run whose namespace name
// already has PersistentVolumes bound to claims in it, such as a previous
// run's that were not reclaimed: the cleanup would wait for them and fail
// this run for what it did not leave.
func (r *Runner) refuseLeftoverVolumes(ctx context.Context, cfg Config) error {
	if cfg.NamespaceMode == PreallocatedNamespace {
		return nil
	}
	left, err := r.runVolumes(ctx, cfg.Namespace)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("namespace %s already has volumes bound to its claims, left by an earlier run; choose a "+
			"new namespace, no resources were created: %s", cfg.Namespace, strings.Join(left, ", "))
	}
	return nil
}

// runVolumes names the PersistentVolumes bound to a claim in namespace that
// the provisioner deletes once their claim is gone, with their phase, claim
// and node. A volume the StorageClass retains is left out: it stays by
// design.
func (r *Runner) runVolumes(ctx context.Context, namespace string) ([]string, error) {
	volumes := &corev1.PersistentVolumeList{}
	if err := r.Client.List(ctx, volumes); err != nil {
		return nil, fmt.Errorf("list the PersistentVolumes: %w", err)
	}
	var left []string
	for i := range volumes.Items {
		volume := &volumes.Items[i]
		if volume.Spec.ClaimRef == nil || volume.Spec.ClaimRef.Namespace != namespace ||
			volume.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain {
			continue
		}
		left = append(left, fmt.Sprintf("%s (%s, claim %s, node %s)", volume.Name, volume.Status.Phase,
			volume.Spec.ClaimRef.Name, volumeNode(volume)))
	}
	return left, nil
}

// awaitVolumesReclaimed waits, after the run's namespace is gone, until no
// PersistentVolume is left that was bound to a claim in it. Deleting the
// claims does not free their space by itself: the provisioner removes each
// volume afterwards, on local-path through a helper Pod on the volume's node,
// which kubelet evicts when that node is short of disk. A volume still there
// when ctx ends is named with its phase and node, and the cleanup is not
// complete.
func (r *Runner) awaitVolumesReclaimed(ctx context.Context, namespace string, poll time.Duration) error {
	var left []string
	err := pollUntil(ctx, poll, func() (bool, error) {
		var err error
		left, err = r.runVolumes(ctx, namespace)
		return err == nil && len(left) == 0, err
	})
	if err != nil && len(left) > 0 {
		return errors.Join(fmt.Errorf("the run namespace's volumes were not reclaimed: %s",
			strings.Join(left, ", ")), err)
	}
	return err
}

// volumeNode names the node a local volume is pinned to, or evidence.Unknown.
func volumeNode(volume *corev1.PersistentVolume) string {
	if volume.Spec.NodeAffinity == nil || volume.Spec.NodeAffinity.Required == nil {
		return evidence.Unknown
	}
	for _, term := range volume.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, expression := range term.MatchExpressions {
			if expression.Key == corev1.LabelHostname && len(expression.Values) == 1 {
				return expression.Values[0]
			}
		}
	}
	return evidence.Unknown
}
