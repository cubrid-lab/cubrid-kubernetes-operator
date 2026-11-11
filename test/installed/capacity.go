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
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
// reason: the shared clusters' database Pods request none, so the scheduler
// does not count the memory they use, even when the run's own members declare
// requests, and on a full node the kernel kills cub_server (#672). A node
// under cfg.MinNodeFreeSpace or cfg.MinNodeFreeMemory, or one that does not
// report a figure a minimum asks for, refuses the run. A minimum of zero reads
// nothing, and so does a preallocated namespace's run, whose cluster-wide
// reads are limited to fixed names. What was read is recorded in bytes per
// node.
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

// unschedulableMember is a member the scheduler refused when a wait for it
// ended, with the scheduler's message. RefusedSince is the lastTransitionTime
// of the Pod's PodScheduled condition, null when the Pod reports none, and
// ReadAt when the harness read it. The time does not move when only the
// message changes, so it says how long the member has been refused, not how
// long this message has held. BlocksRun is whether this refusal shows nodes
// without room (refusalBlocks).
type unschedulableMember struct {
	Pod          string      `json:"pod"`
	PodUID       string      `json:"podUID"`
	Message      string      `json:"message"`
	RefusedSince metav1.Time `json:"refusedSince"`
	ReadAt       time.Time   `json:"readAt"`
	BlocksRun    bool        `json:"blocksRun"`
}

// membersUnschedulable is the error of a wait that ended while the scheduler
// refused a member for memory or CPU. A run on nodes that are full cannot judge
// the Operator (#671), so the error blocks the scenario instead of failing it.
// It deliberately wraps nothing: the verdict the wait would otherwise have
// given must not be found in it.
type membersUnschedulable struct{ error }

// schedulingReadTimeout bounds the read of the members once a wait ended. It
// uses a context of its own because the end of the run may have ended the
// wait.
const schedulingReadTimeout = 30 * time.Second

// refusalMinimumHold is how long a refusal must have held to block a run. The
// run reads its members once, when the wait ends, and the scheduler tries a
// Pending Pod again as the cluster changes: a refusal seconds old can belong
// to a member just created beside Pods that are going away, and has not shown
// that the member does not fit. kube-scheduler backs off for at most 10 s by
// default, so this leaves room for a few tries. It is a chosen value, not a
// measurement.
const refusalMinimumHold = 30 * time.Second

// blockOnUnschedulable reads the members of a wait that ended with ended. A
// member the scheduler refuses is added to recorded with the scheduler's
// message, whatever the refusal says. When a refusal blocks the run
// (refusalBlocks), the error returned is membersUnschedulable, which a
// confirmed never-event still turns into a failure. Otherwise ended is
// returned, with any refusal and what could not be read added to it.
func (r *Runner) blockOnUnschedulable(namespace string, members []string, recorded *[]unschedulableMember,
	ended error) error {
	ctx, cancel := context.WithTimeout(context.Background(), schedulingReadTimeout)
	defer cancel()
	var found []unschedulableMember
	var failed []error
	for _, name := range members {
		pod := &corev1.Pod{}
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			failed = append(failed, err)
			continue
		}
		if member, refused := schedulerRefusal(pod, time.Now().UTC()); refused {
			found = append(found, member)
		}
	}
	*recorded = append(*recorded, found...)
	blocks := false
	refusals := make([]string, 0, len(found))
	for _, member := range found {
		blocks = blocks || member.BlocksRun
		refusals = append(refusals, member.Pod+": "+member.Message)
	}
	if blocks {
		return membersUnschedulable{fmt.Errorf("%s; the scheduler refused a member for memory or CPU, so the "+
			"environment did not let the run judge the Operator: %s", ended, strings.Join(refusals, "; "))}
	}
	if len(found) > 0 {
		ended = fmt.Errorf("%w; the scheduler refused the member(s), which does not block the run because the "+
			"refusal names neither memory nor CPU or has not held for %s: %s", ended, refusalMinimumHold,
			strings.Join(refusals, "; "))
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w; whether the scheduler could place every member could not be read: %v", ended,
			errors.Join(failed...))
	}
	return ended
}

// waitCut reports whether ctx, and not a reading the wait made as it ended,
// ended a wait with err. A reading can span the end of ctx, and what it found
// stays that reading's error.
func waitCut(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, ctx.Err())
}

// unfinishedResult is the result of LAB00 or LAB01 when runErr ended it. A run
// that created nothing is blocked. One that did has failed, unless nodes
// without room for a member left it with no verdict on the Operator; a cleanup
// that failed, or that saw the manager change, is a failure either way.
func unfinishedResult(created, cleaned bool, runErr error) evidence.Result {
	if created && (!cleaned || !errors.As(runErr, new(membersUnschedulable))) {
		return evidence.Fail
	}
	return evidence.Blocked
}

// schedulerRefusal returns the scheduler's refusal, as read at now, when pod is
// an unready member that the scheduler reported it could not place. Any other
// Pending member, such as one that is bound and starting, says nothing about
// the scheduler.
func schedulerRefusal(pod *corev1.Pod, now time.Time) (unschedulableMember, bool) {
	if podReady(pod) || !pod.DeletionTimestamp.IsZero() {
		return unschedulableMember{}, false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse &&
			condition.Reason == corev1.PodReasonUnschedulable {
			return unschedulableMember{Pod: pod.Name, PodUID: string(pod.UID), Message: condition.Message,
				RefusedSince: condition.LastTransitionTime, ReadAt: now,
				BlocksRun: refusalBlocks(condition, now)}, true
		}
	}
	return unschedulableMember{}, false
}

// refusalBlocks reports whether a PodScheduled refusal shows nodes without
// room: its message names memory or CPU, and it has held for
// refusalMinimumHold. The scheduler counts the nodes per reason, so a message
// that also names another reason still counts. Anti-affinity, an unbound claim
// or a node selector can come from the Pod the Operator rendered, and the
// harness does not read MemberPlacement, so those keep the wait's verdict. The
// message is the scheduler's text, not an API. How long a refusal without a
// transition time has held is unknown, and that is not taken as long enough.
func refusalBlocks(refusal corev1.PodCondition, now time.Time) bool {
	capacity := strings.Contains(refusal.Message, "Insufficient memory") ||
		strings.Contains(refusal.Message, "Insufficient cpu")
	return capacity && !refusal.LastTransitionTime.IsZero() &&
		now.Sub(refusal.LastTransitionTime.Time) >= refusalMinimumHold
}

// The requests a member of a run declares unless a flag says otherwise,
// from LAB01 on the VM lab on 2026-11-11 (#671), read with kubectl top every
// 10 s. Once a member served, to the last reading at 16:10 UTC, it used 85 to
// 131m of CPU, after readings of 165 to 172m just after its bootstrap, and 581
// to 605 MiB of memory, rising by about 5 MiB in 30 s at first and 1 to 2.5 MiB
// in 30 s at the end. 640Mi was above every reading of the run. At the rates
// seen at its end, the 35 MiB above 605 MiB would last about 7 to 17 minutes,
// so whether the growth levels off is not known; the long-lived members of
// 2026-11-03 used about 575 MiB each. 150m covers the
// steady CPU readings, not the readings just after a bootstrap or its peaks of
// 255 to 355m: a request is what the scheduler sets aside, not a limit. The
// nine members a restore lane can hold at once (three clusters: the source,
// S12 exact's and one other S12 variant's, whose flags exclude each other)
// declare 1350m and 5760Mi in total beside the shared clusters on three
// 2 vCPU, 8 GiB workers.
const (
	DefaultMemberMemoryRequest = "640Mi"
	DefaultMemberCPURequest    = "150m"
)

// MemberRequestFlags registers the flags of a run's member requests.
func MemberRequestFlags(flags *flag.FlagSet, cfg *Config) {
	flags.Var(requestFlag(&cfg.MemberMemoryRequest, DefaultMemberMemoryRequest), "member-memory-request",
		"spec.resources.requests.memory of every cluster the run creates: above every reading of a member "+
			"in a measured VM-lab run on 2026-11-11 (#671); must be positive")
	flags.Var(requestFlag(&cfg.MemberCPURequest, DefaultMemberCPURequest), "member-cpu-request",
		"spec.resources.requests.cpu of every cluster the run creates: the steady use of a member in a "+
			"measured VM-lab run on 2026-11-11 (#671); must be positive")
}

// requestFlag is a command-line flag that sets *quantity, starting from def.
func requestFlag(quantity *resource.Quantity, def string) requestValue {
	*quantity = resource.MustParse(def)
	return requestValue{quantity: quantity}
}

type requestValue struct{ quantity *resource.Quantity }

func (v requestValue) String() string {
	if v.quantity == nil {
		return ""
	}
	return v.quantity.String()
}

func (v requestValue) Set(value string) error {
	quantity, err := resource.ParseQuantity(value)
	if err != nil || quantity.Sign() <= 0 {
		return fmt.Errorf("want a positive quantity such as 640Mi or 150m: %q", value)
	}
	*v.quantity = quantity
	return nil
}

// memberResources is spec.resources of every cluster the run creates. It sets
// requests only: a limit would kill a member at a peak nobody has measured.
func (c Config) memberResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceMemory: c.MemberMemoryRequest,
		corev1.ResourceCPU:    c.MemberCPURequest,
	}}
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
