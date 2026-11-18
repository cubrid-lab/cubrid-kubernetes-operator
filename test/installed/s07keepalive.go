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
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

// -partition-keepalive tests on the VM lab whether shorter TCP keepalive
// ends the log copy that a partition leaves waiting on a half-open
// connection (#982, #716). The Operator's StatefulSet owns the member Pods'
// spec, so the run sets the values at run time in each Pod's network
// namespace, which the members' containers share, instead of as Pod sysctls.
const (
	// s07KeepaliveInterval and s07KeepaliveProbes are the fixed interval and
	// probe count next to the option's time: a half-open connection ends
	// about 30 seconds after the first probe.
	s07KeepaliveInterval = 10 * time.Second
	s07KeepaliveProbes   = 3
)

var (
	// s07KeepaliveBound bounds the whole setting: adding the keepalive
	// container to each member, which may pull its image, as the cut's own
	// preparation allows, and s07KeepaliveReconnect.
	s07KeepaliveBound = s07PrepareBound + s07KeepaliveReconnect
	// s07KeepaliveReconnect bounds the wait for CUBRID to make the cut-off
	// member's connection to the master again after the run reset it.
	s07KeepaliveReconnect = 3 * time.Minute
)

// s07PodDiagnostic is what the run read about one member Pod after the
// keepalive step failed: the keepalive container's status and the Pod's
// Events.
type s07PodDiagnostic struct {
	Pod              string     `json:"pod"`
	ContainerState   string     `json:"containerState,omitempty"`
	ContainerReason  string     `json:"containerReason,omitempty"`
	ContainerMessage string     `json:"containerMessage,omitempty"`
	Events           []s07Event `json:"events,omitempty"`
	Error            string     `json:"error,omitempty"`
}

// s07Event is one Event about a Pod.
type s07Event struct {
	T       time.Time `json:"t"`
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Count   int32     `json:"count,omitempty"`
	Field   string    `json:"fieldPath,omitempty"`
}

// validateS07Keepalive refuses -partition-keepalive without a partition that
// cuts off a member other than the master, and a value the kernel would
// refuse or that is not shorter than its two-hour default.
func (c HAConfig) validateS07Keepalive() error {
	switch {
	case c.PartitionKeepalive == 0:
		return nil
	case !c.partitionFault():
		return errors.New("-partition-keepalive needs an S07 partition fault")
	case c.Fault == FaultPartitionMasterIsolated:
		return errors.New("-partition-keepalive makes the cut-off member's connection to the master again; with " +
			FaultPartitionMasterIsolated + " the cut-off member is the master")
	}
	if err := c.s07Keepalive().Validate(); err != nil {
		return fmt.Errorf("-partition-keepalive: %w", err)
	}
	return nil
}

func (c HAConfig) s07Keepalive() netfault.Keepalive {
	return netfault.Keepalive{Time: c.PartitionKeepalive, Interval: s07KeepaliveInterval, Probes: s07KeepaliveProbes}
}

// shortenS07Keepalive sets the option's keepalive in every member the
// partition enters and makes the cut-off member's connection to the master
// again under it, within s07KeepaliveBound. It runs after the master was
// moved, because the move replaces the first member's Pod and with it the
// network namespace; and its reset is needed because a connection keeps the
// keepalive schedule of when it was made (netfault.ShortenKeepalive). What it
// did is kept in the record; an error blocks S07 before anything is cut.
func (h *haRun) shortenS07Keepalive(ctx context.Context, rec *s07Record, p *netfault.Partition) error {
	members := []netfault.Endpoint{p.Target}
	var master *netfault.Endpoint
	for _, c := range p.Cuts {
		if !c.Peer.Entered() {
			continue
		}
		members = append(members, c.Peer)
		if c.Peer.Name == rec.Master {
			master = &c.Peer
		}
	}
	k := h.cfg.s07Keepalive()
	refuse := func(err error) error {
		rec.mu.Lock()
		rec.Keepalive = &netfault.KeepaliveRecord{Target: p.Target.Name, Master: rec.Master, Unmet: err.Error()}
		rec.mu.Unlock()
		return err
	}
	if master == nil {
		return refuse(fmt.Errorf("the partition does not name the master %s: its connection cannot be made again",
			rec.Master))
	}
	// The step's bound covers an image pull on a node that has not pulled
	// the faults' image yet; a wait cut short by the run's own deadline
	// would say nothing about the container.
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < s07KeepaliveBound {
		return refuse(fmt.Errorf("only %s of the run's time is left, and the keepalive step needs %s",
			time.Until(deadline).Round(time.Second), s07KeepaliveBound))
	}
	if err := h.s07KeepaliveMembers(ctx, rec, members); err != nil {
		return refuse(err)
	}
	stepCtx, cancel := context.WithTimeout(ctx, s07KeepaliveBound)
	defer cancel()
	got, err := netfault.ShortenKeepalive(stepCtx, p.Env, k, members, p.Target, *master, s07KeepaliveReconnect)
	rec.mu.Lock()
	rec.Keepalive = &got
	rec.mu.Unlock()
	if err != nil {
		diagnostics := h.s07KeepaliveDiagnostics(ctx, members)
		rec.mu.Lock()
		rec.KeepaliveDiagnostics = diagnostics
		rec.mu.Unlock()
		return err
	}
	rec.note("keepalive_shortened", fmt.Sprintf("%s: %d members; %s's connection %s to %s made again",
		k.Time, len(got.Members), p.Target.Name, got.Reconnected.Local, got.Reconnected.Peer))
	return nil
}

// s07DiagnosticsBound bounds the reads after a failed keepalive step, which
// may run after the step's own context ended.
const s07DiagnosticsBound = 30 * time.Second

// s07KeepaliveDiagnostics reads, for each member Pod, the keepalive
// container's status and the Events about the Pod, best-effort. The API
// error of the container's addition, if any, is already in the step's
// record.
func (h *haRun) s07KeepaliveDiagnostics(ctx context.Context, members []netfault.Endpoint) []s07PodDiagnostic {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s07DiagnosticsBound)
	defer cancel()
	events := &corev1.EventList{}
	eventsErr := h.Client.List(ctx, events, client.InNamespace(h.cfg.Namespace))
	diagnostics := make([]s07PodDiagnostic, 0, len(members))
	for _, e := range members {
		d := s07PodDiagnostic{Pod: e.Pod}
		pod := &corev1.Pod{}
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: e.Pod}, pod); err != nil {
			d.Error = "pod unread: " + err.Error()
		}
		for _, s := range pod.Status.EphemeralContainerStatuses {
			if s.Name != netfault.KeepaliveContainer {
				continue
			}
			switch {
			case s.State.Running != nil:
				d.ContainerState = "running"
			case s.State.Terminated != nil:
				d.ContainerState, d.ContainerReason, d.ContainerMessage = "terminated", s.State.Terminated.Reason,
					s.State.Terminated.Message
			case s.State.Waiting != nil:
				d.ContainerState, d.ContainerReason, d.ContainerMessage = "waiting", s.State.Waiting.Reason,
					s.State.Waiting.Message
			}
		}
		if d.ContainerState == "" && d.Error == "" {
			d.ContainerState = "no status reported"
		}
		if eventsErr != nil {
			d.Error = strings.TrimPrefix(d.Error+"; events unread: "+eventsErr.Error(), "; ")
		}
		for _, ev := range events.Items {
			if ev.InvolvedObject.Name != e.Pod || string(ev.InvolvedObject.UID) != e.UID {
				continue
			}
			t := ev.LastTimestamp.Time
			if t.IsZero() {
				t = ev.EventTime.Time
			}
			d.Events = append(d.Events, s07Event{T: t.UTC(), Type: ev.Type, Reason: ev.Reason, Message: ev.Message,
				Count: ev.Count, Field: ev.InvolvedObject.FieldPath})
		}
		slices.SortFunc(d.Events, func(a, b s07Event) int { return a.T.Compare(b.T) })
		if len(d.Events) > s07DiagnosticEvents {
			d.Events = d.Events[len(d.Events)-s07DiagnosticEvents:]
		}
		diagnostics = append(diagnostics, d)
	}
	return diagnostics
}

// s07DiagnosticEvents is how many of a Pod's latest Events are kept.
const s07DiagnosticEvents = 20

// s07KeepaliveMembers checks, before a privileged container goes into any
// member Pod, that the run namespace's guard passes and that each Pod is
// controlled by the run's HA StatefulSet, itself the HA cluster's, with the
// UID the partition names and, when the run recorded one for it, the UID of
// the run's member identities: after the move of the master its own, else
// LAB01's.
func (h *haRun) s07KeepaliveMembers(ctx context.Context, rec *s07Record, members []netfault.Endpoint) error {
	// The run namespace is created enforcing Pod Security privileged for a
	// partition fault (prepareNamespace), before LAB01; read it again, so
	// that a namespace that would refuse the privileged container blocks the
	// step with its level instead of an API error.
	var podSecurity map[string]string
	if err := h.privilegedNamespace(ctx, "-partition-keepalive", &podSecurity); err != nil {
		return fmt.Errorf("check the run namespace before adding the keepalive container: %w", err)
	}
	identities := rec.MembersBefore
	if rec.MasterMove != nil && len(rec.MasterMove.Members) > 0 {
		identities = rec.MasterMove.Members
	}
	for _, e := range members {
		pod, err := h.s07RunMember(ctx, e.Pod)
		if err != nil {
			return err
		}
		if string(pod.UID) != e.UID {
			return fmt.Errorf("member Pod %s has UID %s, not %s as the partition names it", e.Pod, pod.UID, e.UID)
		}
		for _, id := range identities {
			if id.Pod == e.Pod && id.UID != e.UID {
				return fmt.Errorf("member Pod %s has UID %s, not %s as the run recorded it", e.Pod, e.UID, id.UID)
			}
		}
	}
	return nil
}
