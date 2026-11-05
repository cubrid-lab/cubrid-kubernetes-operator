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

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// checkNodeFreeSpace reads, before the run creates anything, the free space
// of kubelet's root filesystem (nodefs) and the available memory on every
// Ready node a member could be scheduled to. On the VM lab, local-path
// volumes and container images share that filesystem; elsewhere they may not.
// kubelet evicts Pods when it runs low, and a run's members can all land on
// one node, so a run that starts on a full node can evict the shared clusters
// and its own volumes' delete helpers with them. Memory is read for the same
// reason: database Pods request none, so the scheduler does not avoid a node
// whose memory the shared clusters already use, and there the kernel kills
// cub_server (#672). A node under cfg.MinNodeFreeSpace or
// cfg.MinNodeFreeMemory, or one that does not report a figure a minimum asks
// for, refuses the run. A minimum of zero reads nothing, and so does a
// preallocated namespace's run, whose cluster-wide reads are limited to fixed
// names. What was read is recorded in bytes per node.
func (r *Runner) checkNodeFreeSpace(ctx context.Context, cfg Config, rec *record) error {
	readSpace, readMemory := cfg.MinNodeFreeSpace > 0, cfg.MinNodeFreeMemory > 0
	if !readSpace && !readMemory || cfg.NamespaceMode == PreallocatedNamespace {
		return nil
	}
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes); err != nil {
		return fmt.Errorf("list the nodes to read their free space: %w", err)
	}
	if readSpace {
		rec.NodeFreeBytes = map[string]int64{}
	}
	if readMemory {
		rec.NodeAvailableMemoryBytes = map[string]int64{}
	}
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
				Fs     *struct{ AvailableBytes *int64 } `json:"fs"`
				Memory *struct{ AvailableBytes *int64 } `json:"memory"`
			} `json:"node"`
		}
		if err := json.Unmarshal([]byte(out), &summary); err != nil {
			return fmt.Errorf("the stats summary of node %s could not be read: %w", node.Name, err)
		}
		if readSpace {
			if summary.Node.Fs == nil || summary.Node.Fs.AvailableBytes == nil {
				return fmt.Errorf("the free space of node %s was not reported; no resources were created", node.Name)
			}
			free := *summary.Node.Fs.AvailableBytes
			rec.NodeFreeBytes[node.Name] = free
			if free < cfg.MinNodeFreeSpace {
				short = append(short, fmt.Sprintf("%s has %d bytes free", node.Name, free))
			}
		}
		if readMemory {
			if summary.Node.Memory == nil || summary.Node.Memory.AvailableBytes == nil {
				return fmt.Errorf("the available memory of node %s was not reported; no resources were created",
					node.Name)
			}
			available := *summary.Node.Memory.AvailableBytes
			rec.NodeAvailableMemoryBytes[node.Name] = available
			if available < cfg.MinNodeFreeMemory {
				short = append(short, fmt.Sprintf("%s has %d bytes of memory available", node.Name, available))
			}
		}
	}
	if len(short) > 0 {
		return fmt.Errorf("a node has less free space or memory than a run needs (-min-node-free-space %d, "+
			"-min-node-free-memory %d bytes); no resources were created: %s", cfg.MinNodeFreeSpace,
			cfg.MinNodeFreeMemory, strings.Join(short, ", "))
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

// MemberSpreadFlag is a command-line flag that sets *spread, starting from def.
func MemberSpreadFlag(spread *databasev1alpha1.MemberSpread, def databasev1alpha1.MemberSpread) memberSpreadValue {
	*spread = def
	return memberSpreadValue{spread: spread}
}

type memberSpreadValue struct {
	spread *databasev1alpha1.MemberSpread
}

func (v memberSpreadValue) String() string {
	if v.spread == nil {
		return ""
	}
	return string(*v.spread)
}

func (v memberSpreadValue) Set(value string) error {
	spread := databasev1alpha1.MemberSpread(value)
	if !validMemberSpread(spread) {
		return fmt.Errorf("want Required or Preferred: %q", value)
	}
	*v.spread = spread
	return nil
}

func validMemberSpread(spread databasev1alpha1.MemberSpread) bool {
	return spread == databasev1alpha1.MemberSpreadRequired || spread == databasev1alpha1.MemberSpreadPreferred
}

// checkMemberSpreadNodes records the run's member spread and, under
// Required, refuses before anything is created a run with fewer schedulable
// nodes than members: a member with no node of its own would stay Pending
// and the cluster would never form, which says nothing about the Operator
// (#596). The schedulable nodes read are recorded.
func (r *Runner) checkMemberSpreadNodes(ctx context.Context, cfg HAConfig, rec *haRecord) error {
	rec.MemberSpread = string(cfg.MemberSpread)
	if cfg.MemberSpread != databasev1alpha1.MemberSpreadRequired {
		return nil
	}
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes); err != nil {
		return fmt.Errorf("list the nodes to count those a member can be scheduled to: %w", err)
	}
	rec.SchedulableNodes = []string{}
	for i := range nodes.Items {
		if schedulable(&nodes.Items[i]) {
			rec.SchedulableNodes = append(rec.SchedulableNodes, nodes.Items[i].Name)
		}
	}
	slices.Sort(rec.SchedulableNodes)
	if len(rec.SchedulableNodes) < haMemberCount {
		return fmt.Errorf("not enough schedulable nodes for Required member spread: %d members need a node "+
			"each, and %d node(s) are Ready, not cordoned and not tainted (%s); no resources were created; "+
			"make more nodes schedulable or run with -member-spread Preferred", haMemberCount,
			len(rec.SchedulableNodes), strings.Join(rec.SchedulableNodes, ", "))
	}
	return nil
}
