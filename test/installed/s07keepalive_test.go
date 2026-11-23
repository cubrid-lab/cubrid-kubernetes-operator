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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

// The tests in this file cover -partition-keepalive (#982): before the cut,
// the network partition lane shortens TCP keepalive in every member's
// network namespace and makes the cut-off member's connection to the master
// again, so that a connection left half-open by the cut can end within
// about a minute instead of the kernel's two hours.

func keepaliveConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testHAConfig(t)
	cfg.NamespaceMode, cfg.Level, cfg.StablePeriod = FreshNamespace, evidence.LevelVMLab, time.Second
	cfg.Fault, cfg.PartitionHold = FaultPartitionHigherRankIsolated, 2*time.Minute
	cfg.PartitionSettle, cfg.PartitionKeepalive, cfg.Timeout = 10*time.Minute, 30*time.Second, 60*time.Minute
	return cfg
}

// The documented lab command is accepted, and the option's own steps count
// in the -timeout check.
func TestValidateS07PartitionKeepalive(t *testing.T) {
	cfg := keepaliveConfig(t)
	if err := cfg.validate(); err != nil {
		t.Fatalf("the documented lab options are refused: %v", err)
	}
	off := keepaliveConfig(t)
	off.PartitionKeepalive = 0
	if got := cfg.s07StepBounds() - off.s07StepBounds(); got != s07KeepaliveBound {
		t.Errorf("-partition-keepalive adds %s to the steps' bounds, want %s", got, s07KeepaliveBound)
	}
	documented := keepaliveConfig(t)
	documented.DataCheckWithin = 2 * time.Minute
	if got := documented.s07StepBounds(); got != 44*time.Minute+20*time.Second {
		t.Errorf("the documented command's own bounds are %s, the guide says 44m20s", got)
	}
	cfg.Timeout = cfg.s07StepBounds()
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "-timeout") {
		t.Errorf("a timeout that only covers the steps' bounds: %v", err)
	}
	refused := map[string]func(*HAConfig){
		"no partition fault":   func(c *HAConfig) { c.Fault = "" },
		"master-isolated":      func(c *HAConfig) { c.Fault = FaultPartitionMasterIsolated },
		"a negative time":      func(c *HAConfig) { c.PartitionKeepalive = -time.Second },
		"part of a second":     func(c *HAConfig) { c.PartitionKeepalive = 1500 * time.Millisecond },
		"the kernel's default": func(c *HAConfig) { c.PartitionKeepalive = 2 * time.Hour },
	}
	for name, change := range refused {
		t.Run(name, func(t *testing.T) {
			c := keepaliveConfig(t)
			change(&c)
			if err := c.validate(); err == nil || !strings.Contains(err.Error(), "-partition-keepalive") {
				t.Errorf("accepted, or refused for another reason: %v", err)
			}
		})
	}
	slave := keepaliveConfig(t)
	slave.Fault = FaultPartitionSlaveIsolated
	if err := slave.validate(); err != nil {
		t.Errorf("slave-isolated with -partition-keepalive: %v", err)
	}
}

// keepaliveRun is the higher-rank-isolated fixture with the master on the
// second member, so that nothing is moved, and every member Pod and the
// manager Pod running with an IP: the run gets as far as the keepalive
// before the cut.
func keepaliveRun(t *testing.T) (*haRun, *haFixture) {
	t.Helper()
	h, f := s07HigherRankRun(t, m1, failOver)
	h.cfg.PartitionSettle, h.cfg.PartitionKeepalive, h.cfg.Timeout = 10*time.Minute, 30*time.Second, 60*time.Minute
	ctx := context.Background()
	for i, name := range append(slices.Clone(testS07Members), testManagerPodName) {
		pod := &corev1.Pod{}
		namespace := h.cfg.Namespace
		if name == testManagerPodName {
			namespace = h.cfg.OperatorNamespace
		}
		if err := f.store.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.PodIP, pod.Status.Phase = fmt.Sprintf("10.42.0.%d", 10+i), corev1.PodRunning
		if err := f.store.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	h.rec.OperatorAfter.Pods = []imageIdentity{{Pod: testManagerPodName, UID: testManagerPodName}}
	setRunPodSecurity(t, f, podSecurityPrivileged)
	return h, f
}

// setRunPodSecurity makes the run namespace enforce level, as the run
// creates it for a partition fault.
func setRunPodSecurity(t *testing.T, f *haFixture, level string) {
	t.Helper()
	ctx := context.Background()
	ns := &corev1.Namespace{}
	err := f.store.Get(ctx, client.ObjectKey{Name: f.cfg.Namespace}, ns)
	if apierrors.IsNotFound(err) {
		ns.Name, ns.Labels = f.cfg.Namespace, map[string]string{podSecurityEnforce: level}
		if err := f.store.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels[podSecurityEnforce] = level
	if err := f.store.Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
}

// kubeletFailed is the reason of the kubelet's Event about a container it
// could not create.
const kubeletFailed = "Failed"

// waitingKeepalive plays the API server and the kubelet for a keepalive
// container that is admitted and never starts: its status waits with the
// kubelet's reason, and the kubelet reports an Event about it.
func waitingKeepalive(f *haFixture, reason, message string) func(context.Context, client.Client, *corev1.Pod) error {
	return func(ctx context.Context, c client.Client, pod *corev1.Pod) error {
		stored := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), stored); err != nil {
			return err
		}
		stored.Spec.EphemeralContainers = pod.Spec.EphemeralContainers
		if err := c.Update(ctx, stored); err != nil {
			return err
		}
		stored.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: netfault.KeepaliveContainer,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}}}
		if err := c.Status().Update(ctx, stored); err != nil {
			return err
		}
		return f.store.Create(ctx, &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name + ".keepalive", Namespace: pod.Namespace},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, Namespace: pod.Namespace,
				UID: pod.UID, FieldPath: "spec.ephemeralContainers{" + netfault.KeepaliveContainer + "}"},
			Reason: kubeletFailed, Message: "Error: " + message, Type: corev1.EventTypeWarning,
			LastTimestamp: metav1.Now()})
	}
}

// The run namespace is created enforcing Pod Security privileged for a
// partition fault, before LAB01; the keepalive step still reads it and adds
// no container to a namespace that would refuse it.
func TestShortenS07KeepaliveNeedsAPrivilegedNamespace(t *testing.T) {
	h, f := keepaliveRun(t)
	setRunPodSecurity(t, f, podSecurityRestricted)
	f.ephemeralAdded = func(context.Context, client.Client, *corev1.Pod) error {
		t.Error("a container was added")
		return errors.New("refused")
	}
	rec := &s07Record{Variant: s07HigherRankIsolated, Master: m1}
	err := h.shortenS07Keepalive(context.Background(), rec, keepalivePartition(t, h))
	if err == nil || !strings.Contains(err.Error(), "Pod Security") {
		t.Fatalf("error %v, want the namespace's Pod Security level", err)
	}
	if ephemeralWrites(f) != 0 {
		t.Error("a container was added")
	}
}

// With less of the run's time left than the step's bound, the container
// could not be given the time to pull and start: the step refuses at once
// and says so, instead of a wait cut short.
func TestShortenS07KeepaliveNeedsItsWholeBound(t *testing.T) {
	h, f := keepaliveRun(t)
	f.ephemeralAdded = func(context.Context, client.Client, *corev1.Pod) error {
		t.Error("a container was added")
		return errors.New("refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rec := &s07Record{Variant: s07HigherRankIsolated, Master: m1}
	err := h.shortenS07Keepalive(ctx, rec, keepalivePartition(t, h))
	if err == nil || !strings.Contains(err.Error(), "left") {
		t.Fatalf("error %v, want the run's time left", err)
	}
}

// A container that is admitted and never starts blocks S07 with the
// kubelet's reason, and record.json keeps each member Pod's container status
// and the Events about the Pod, so that the next lab failure is explained.
func TestRunS07RecordsWhyTheKeepaliveContainerDidNotStart(t *testing.T) {
	h, f := keepaliveRun(t)
	f.ephemeralAdded = waitingKeepalive(f, "CreateContainerConfigError",
		"container's runAsUser breaks non-root policy")
	result := h.runS07(context.Background())
	if result.Result != evidence.Blocked || !strings.Contains(result.Reason, "CreateContainerConfigError") {
		t.Fatalf("result %s %q", result.Result, result.Reason)
	}
	rec := readS07Record(t, h)
	i := slices.IndexFunc(rec.KeepaliveDiagnostics, func(d s07PodDiagnostic) bool { return d.Pod == m0 })
	if i < 0 {
		t.Fatalf("diagnostics %+v", rec.KeepaliveDiagnostics)
	}
	d := rec.KeepaliveDiagnostics[i]
	if d.ContainerState != "waiting" || d.ContainerReason != "CreateContainerConfigError" ||
		!strings.Contains(d.ContainerMessage, "non-root") {
		t.Errorf("container status %+v", d)
	}
	if len(d.Events) != 1 || d.Events[0].Reason != kubeletFailed || !strings.Contains(d.Events[0].Message, "non-root") {
		t.Errorf("events %+v", d.Events)
	}
}

func keepalivePartition(t *testing.T, h *haRun) *netfault.Partition {
	t.Helper()
	endpoint := func(name string) netfault.Endpoint {
		e, err := h.s07Endpoint(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	return &netfault.Partition{Env: netfault.Env{Client: h.Client, Kubectl: h.Command, Poll: time.Millisecond},
		Target: endpoint(m0), Cuts: []netfault.Cut{{Peer: endpoint(m1), Direction: netfault.Both},
			{Peer: endpoint(m2), Direction: netfault.Both},
			{Peer: netfault.Endpoint{Name: testManagerName, IP: "10.42.0.8", Port: s07OperatorPort, NoEnter: true}}}}
}

func ephemeralWrites(f *haFixture) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.writes {
		if w == "ephemeral-container" {
			n++
		}
	}
	return n
}

// The privileged container goes only into Pods the run may enter: the run
// namespace's guard must pass, and each Pod must be the one its HA
// StatefulSet controls, with the UID the partition and the run's member
// identities name. A refusal adds no container, is recorded, and is an error,
// which blocks S07 before anything is cut.
func TestShortenS07KeepaliveEntersOnlyTheRunsOwnMembers(t *testing.T) {
	refused := map[string]struct {
		change func(*haRun, *haFixture, *netfault.Partition, *s07Record)
		want   string
	}{
		"the run namespace guard fails": {func(h *haRun, _ *haFixture, _ *netfault.Partition, _ *s07Record) {
			h.guard = func(context.Context) error { return errors.New("the run namespace was replaced") }
		}, "the run namespace was replaced"},
		"a Pod the HA StatefulSet does not control": {func(_ *haRun, f *haFixture, _ *netfault.Partition, _ *s07Record) {
			pod := &corev1.Pod{}
			ctx := context.Background()
			if err := f.store.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: m2}, pod); err != nil {
				t.Fatal(err)
			}
			pod.OwnerReferences = nil
			if err := f.store.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
		}, "does not belong to the HA StatefulSet"},
		// A member's name in a shared namespace, such as the Operator's,
		// names another Pod, which is never entered.
		"a Pod in another namespace": {func(_ *haRun, _ *haFixture, p *netfault.Partition, _ *s07Record) {
			p.Cuts[1].Peer.Namespace = "cubrid-operator-system"
		}, "not the run namespace"},
		"a Pod with another UID than the partition names": {func(_ *haRun, _ *haFixture, p *netfault.Partition,
			_ *s07Record) {
			p.Cuts[1].Peer.UID = "stale"
		}, "has UID"},
		"a Pod with another UID than the run's member identities": {func(_ *haRun, _ *haFixture,
			_ *netfault.Partition, rec *s07Record) {
			rec.MembersBefore = []imageIdentity{{Pod: m0, UID: m0}, {Pod: m1, UID: "earlier"}, {Pod: m2, UID: m2}}
		}, "has UID"},
		"a master the partition does not name": {func(_ *haRun, _ *haFixture, _ *netfault.Partition,
			rec *s07Record) {
			rec.Master = "ha-5"
		}, "ha-5"},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			h, f := keepaliveRun(t)
			f.ephemeralAdded = func(context.Context, client.Client, *corev1.Pod) error {
				t.Error("a container was added")
				return errors.New("refused")
			}
			p := keepalivePartition(t, h)
			rec := &s07Record{Variant: s07HigherRankIsolated, Master: m1,
				MembersBefore: []imageIdentity{{Pod: m0, UID: m0}, {Pod: m1, UID: m1}, {Pod: m2, UID: m2}}}
			tc.change(h, f, p, rec)
			err := h.shortenS07Keepalive(context.Background(), rec, p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if rec.Keepalive == nil || !strings.Contains(rec.Keepalive.Unmet, tc.want) {
				t.Errorf("record %+v", rec.Keepalive)
			}
			if n := ephemeralWrites(f); n != 0 {
				t.Errorf("%d ephemeral containers added", n)
			}
		})
	}
}

// A keepalive that cannot be set ends S07 blocked, through the scenario,
// before any client starts or anything is cut, and the record keeps what
// happened.
func TestRunS07BlocksWhenTheKeepaliveCannotBeSet(t *testing.T) {
	h, f := keepaliveRun(t)
	f.ephemeralAdded = func(context.Context, client.Client, *corev1.Pod) error {
		return errors.New("privileged ephemeral containers are not allowed")
	}
	result := h.runS07(context.Background())
	if result.Result != evidence.Blocked || !strings.Contains(result.Reason, "-partition-keepalive") ||
		!strings.Contains(result.Reason, "not allowed") {
		t.Errorf("result %s %q, want blocked by the keepalive", result.Result, result.Reason)
	}
	if result.FaultConfirmed != nil || result.CleanupSucceeded != nil {
		t.Errorf("fault confirmed %v, cleanup %v: a fault was reported", result.FaultConfirmed, result.CleanupSucceeded)
	}
	f.mu.Lock()
	for _, ran := range f.ran {
		if strings.Contains(ran, " exec "+clientPodName+" ") || strings.Contains(ran, " -c "+netfault.Container+" ") {
			t.Errorf("ran %q before the cut could start", ran)
		}
	}
	f.mu.Unlock()
	rec := readS07Record(t, h)
	if rec.Keepalive == nil || rec.Keepalive.Unmet == "" || len(rec.Keepalive.Members) != 1 ||
		rec.Keepalive.Members[0].Name != m0 {
		t.Errorf("keepalive record %+v", rec.Keepalive)
	}
}
