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
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// allCapabilities are the request checks an Instance Manager of the
// Operator's own build reports.
var allCapabilities = instancemanager.Capabilities()

// olderManagers answers as the members of a group being formed, like
// firstMemberMaster, from Instance Managers of a build that reports no
// safety checks in its GET /v1/role answer: a build older than the
// capabilities list, which decodes a request field it does not know and
// ignores it.
type olderManagers struct{}

func (olderManagers) ProbeRole(ctx context.Context, podName, namespace string) RoleObservation {
	o := firstMemberMaster{}.ProbeRole(ctx, podName, namespace)
	o.Capabilities = nil
	return o
}

// capabilityProber answers like firstMemberMaster, with the checks set for
// each member; a member without an entry reports none, and a silent member
// does not answer.
type capabilityProber struct {
	reports map[string][]string
	silent  map[string]bool
}

func (p *capabilityProber) ProbeRole(ctx context.Context, podName, namespace string) RoleObservation {
	if p.silent[podName] {
		return RoleObservation{Reason: "dial tcp: connection refused"}
	}
	o := firstMemberMaster{}.ProbeRole(ctx, podName, namespace)
	o.Capabilities = p.reports[podName]
	return o
}

// An Instance Manager of an older build answers a seed backup or a seed
// restore with 202 and runs it without the checks the request relies on:
// it ignores seed, seedFromMaster and expectedClusterUID. Its answer is the
// same as when it enforced them, so the Operator sends such a request only to
// a member whose GET /v1/role answer reports each check, and otherwise waits
// with a condition that names the member and the missing check, using up no
// attempt (ADR-0013, #733).
var _ = Describe("A seed step for an Instance Manager that does not report the checks it relies on (#733)", func() {
	ctx := context.Background()

	reconcilerFor := func(seed *fakeSeedClient, prober RoleProber) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			Prober: prober, Backup: seed, Restore: seed,
		}
	}
	created := func(name string) (*databasev1alpha1.CubridCluster, *databasev1alpha1.DatabaseStatus) {
		c := haCluster(name)
		c.UID = "uid-cap"
		status := databaseStatus(c, "appdb")
		status.Phase, status.PrimaryCreated = databasePhaseCreated, true
		return c, status
	}
	condition := func(c *databasev1alpha1.CubridCluster) (string, string) {
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		return cond.Reason, cond.Message
	}

	It("sends no seed backup to a source whose Instance Manager reports no checks", func() {
		seed := &fakeSeedClient{backupState: instancemanager.OpCompleted,
			restore: map[string]instancemanager.OperationState{}}
		c, status := created("cap-older")

		reconcilerFor(seed, olderManagers{}).reconcileSeeding(ctx, c, status, "cap-older-0")

		Expect(seed.backupOn).To(BeEmpty(), "a seed backup was sent to a manager that does not report seed-upload")
		Expect(seed.restoreOn).To(BeEmpty())
		reason, msg := condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(msg).To(And(ContainSubstring("cap-older-0"), ContainSubstring("seed-upload"),
			ContainSubstring("backup-source-role"), ContainSubstring("Replace the Pod")))
		Expect(status.BootstrapAttempts).To(BeZero())
		Expect(status.SeededMembers).To(BeEmpty())

		By("a new reconciler, as after an operator restart, sends nothing either")
		reconcilerFor(seed, olderManagers{}).reconcileSeeding(ctx, c, status, "cap-older-0")
		Expect(seed.backupOn).To(BeEmpty())
		Expect(seed.restoreOn).To(BeEmpty())
		reason, _ = condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(status.BootstrapAttempts).To(BeZero())
	})

	It("sends no seed restore to a peer that does not report every check the restore relies on", func() {
		seed := &fakeSeedClient{backupState: instancemanager.OpCompleted,
			restore: map[string]instancemanager.OperationState{}}
		c, status := created("cap-peer")
		prober := &capabilityProber{reports: map[string][]string{"cap-peer-0": allCapabilities}}
		r := reconcilerFor(seed, prober)

		By("a peer that reports no checks")
		r.reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.backupOn).To(Equal([]string{"cap-peer-0"}), "the source reports the checks of a seed backup")
		Expect(seed.restoreOn).To(BeEmpty(), "a seed restore was sent to a manager that does not report its checks")
		reason, msg := condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(msg).To(And(ContainSubstring("cap-peer-1"), ContainSubstring("seed-from-master"),
			ContainSubstring("expected-cluster-uid")))

		By("a peer that reports only some of them is held for the missing one")
		prober.reports["cap-peer-1"] = []string{instancemanager.CapabilitySeedFromMaster}
		r.reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.restoreOn).To(BeEmpty())
		reason, msg = condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(msg).To(And(ContainSubstring("cap-peer-1"), ContainSubstring("expected-cluster-uid"),
			Not(ContainSubstring("seed-from-master,"))))

		By("a peer that gives no answer is not shown to enforce them")
		prober.silent = map[string]bool{"cap-peer-1": true}
		r.reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.restoreOn).To(BeEmpty())
		reason, msg = condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityUnverified"))
		Expect(msg).To(And(ContainSubstring("cap-peer-1"), ContainSubstring("connection refused")))

		By("a new reconciler, as after an operator restart, holds it too")
		prober.silent = nil
		reconcilerFor(seed, prober).reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.restoreOn).To(BeEmpty())
		Expect(status.BootstrapAttempts).To(BeZero())
		Expect(status.SeededMembers).To(BeEmpty())

		By("the restore is sent once the peer reports every check")
		prober.reports["cap-peer-1"] = allCapabilities
		r.reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.restoreOn).To(Equal([]string{"cap-peer-1"}))
		Expect(seed.restoreReq.SeedFromMaster).To(Equal("cap-peer-0"))
		Expect(seed.restoreReq.ExpectedClusterUID).To(Equal(string(c.UID)))
		Expect(seed.backupOn).To(HaveLen(1), "the completed seed backup is not taken again")
		Expect(status.BootstrapAttempts).To(BeZero())
	})
})

// The capabilities a member reports reach the observation the seed steps are
// held on, and an Instance Manager of the Operator's own build reports every
// check a seed step relies on (ADR-0013, #733).
func TestProbeRole_ReadsTheChecksTheInstanceManagerReports(t *testing.T) {
	h := instancemanager.NewServer(noHeartbeat{}, "tok").Handler()
	p := &HTTPRoleProber{Client: &http.Client{Transport: toHandler{h}}, Tokens: staticToken("tok"), Now: func() time.Time { return testNow }}
	o := p.ProbeRole(context.Background(), c0, "ns")
	if !o.Reachable {
		t.Fatalf("the member did not answer: %+v", o)
	}
	for _, c := range []string{instancemanager.CapabilitySeedUpload, instancemanager.CapabilityBackupSourceRole,
		instancemanager.CapabilitySeedFromMaster, instancemanager.CapabilityExpectedClusterUID} {
		if !slices.Contains(o.Capabilities, c) {
			t.Errorf("capabilities = %v, want %q among them", o.Capabilities, c)
		}
		if _, _, held := capabilityHold(c0, "step", o, testNow, c); held {
			t.Errorf("a step relying on %q was held for a member of the same build", c)
		}
	}

	older := observationFromStatus(instancemanager.HAStatus{Current: c0, Role: instancemanager.RoleSlave}, testNow)
	if reason, _, held := capabilityHold(c0, "step", older, testNow, instancemanager.CapabilitySeedUpload); !held ||
		reason != reasonCapabilityMissing {
		t.Errorf("an answer without capabilities: held=%v reason=%s, want held for %s", held, reason, reasonCapabilityMissing)
	}
	stale := o
	stale.ObservedAt = testNow.Add(-2 * roleObservationTTL)
	if reason, _, held := capabilityHold(c0, "step", stale, testNow, instancemanager.CapabilitySeedUpload); !held ||
		reason != reasonCapabilityUnverified {
		t.Errorf("a stale answer: held=%v reason=%s, want held for %s", held, reason, reasonCapabilityUnverified)
	}
}

// noHeartbeat is a CUBRID CLI on which no command runs: the member has no
// database and no heartbeat, as a member before its seed restore.
type noHeartbeat struct{}

func (noHeartbeat) Run(context.Context, string, ...string) (string, error) {
	return "", errors.New("not running")
}
