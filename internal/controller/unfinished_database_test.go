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
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

const (
	unfinishedOp  = "op-0123456789abcdef0123456789abcdef"
	unfinishedWhy = "why"
)

func unfinished(state databasev1alpha1.UnfinishedDatabaseState, op string) *databasev1alpha1.InstanceUnfinishedDatabase {
	return &databasev1alpha1.InstanceUnfinishedDatabase{Database: testDatabase, Operation: op, State: state, Message: unfinishedWhy}
}

// The Instance Manager's report of an unfinished database is carried next to
// the role, which it does not change. A state this Operator does not know is
// taken as Refused: nothing is promised to clear it.
func TestObservationFromStatus_UnfinishedDatabase(t *testing.T) {
	master := haStatus(instancemanager.RoleMaster, true, node(c0, "master"), node(c1, "slave"))
	for name, tc := range map[string]struct {
		report *instancemanager.UnfinishedDatabase
		want   *databasev1alpha1.InstanceUnfinishedDatabase
	}{
		"no marker": {nil, nil},
		"waiting": {&instancemanager.UnfinishedDatabase{Database: testDatabase, Operation: unfinishedOp,
			State: instancemanager.UnfinishedWaiting, Reason: unfinishedWhy},
			unfinished(databasev1alpha1.UnfinishedWaitingForOperation, unfinishedOp)},
		"refused": {&instancemanager.UnfinishedDatabase{Database: testDatabase, State: instancemanager.UnfinishedRefused,
			Reason: unfinishedWhy}, unfinished(databasev1alpha1.UnfinishedRefused, "")},
		"a state this Operator does not know": {&instancemanager.UnfinishedDatabase{Database: testDatabase,
			State: "Later", Reason: unfinishedWhy}, unfinished(databasev1alpha1.UnfinishedRefused, "")},
	} {
		st := master
		st.Unfinished = tc.report
		got := observationFromStatus(st, testNow)
		if got.Role != databasev1alpha1.RoleMaster || got.Conflicting {
			t.Errorf("%s: role = %s (conflicting %v), want an untouched master", name, got.Role, got.Conflicting)
		}
		switch {
		case tc.want == nil && got.Unfinished != nil:
			t.Errorf("%s: unfinished = %+v, want none", name, got.Unfinished)
		case tc.want != nil && (got.Unfinished == nil || *got.Unfinished != *tc.want):
			t.Errorf("%s: unfinished = %+v, want %+v", name, got.Unfinished, tc.want)
		}
	}
}

// instances[] carries the report of an answer that counts as the member's,
// and nothing of an answer that is stale, unreachable or names another member.
func TestInstanceStatuses_UnfinishedDatabase(t *testing.T) {
	report := unfinished(databasev1alpha1.UnfinishedRefused, "")
	for name, tc := range map[string]struct {
		obs  RoleObservation
		want bool
	}{
		"a fresh answer": {RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow,
			Unfinished: report}, true},
		"a fresh answer without a marker": {RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave,
			ObservedAt: testNow}, false},
		"a stale answer": {RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave,
			ObservedAt: testNow.Add(-2 * roleObservationTTL), Unfinished: report}, false},
		"an answer naming another member": {RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave,
			ObservedAt: testNow, Conflicting: true, Unfinished: report}, false},
	} {
		got := instanceStatuses([]string{c1}, map[string]RoleObservation{c1: tc.obs}, testNow, nil)[0]
		if (got.UnfinishedDatabase != nil) != tc.want {
			t.Errorf("%s: unfinishedDatabase = %+v, want present %v", name, got.UnfinishedDatabase, tc.want)
		}
		if tc.want && got.Role != databasev1alpha1.RoleSlave {
			t.Errorf("%s: role = %s, want slave: the report hides no role", name, got.Role)
		}
	}
}

// A member that gave no answer that counts keeps its last report, so that a
// missed probe neither clears it nor, when the member answers again, makes
// its Event again. An answer without the marker clears it.
func TestInstanceStatuses_UnfinishedDatabaseKeptWithoutAnAnswer(t *testing.T) {
	report := unfinished(databasev1alpha1.UnfinishedRefused, "")
	answer := func(u *databasev1alpha1.InstanceUnfinishedDatabase) map[string]RoleObservation {
		return map[string]RoleObservation{c1: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow,
			Unfinished: u}}
	}
	reported := instanceStatuses([]string{c1}, answer(report), testNow, nil)
	for name, obs := range map[string]map[string]RoleObservation{
		"no observation":   {},
		"unreachable":      {c1: {Reachable: false, ObservedAt: testNow}},
		"a stale answer":   {c1: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow.Add(-2 * roleObservationTTL)}},
		"another member's": {c1: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow, Conflicting: true}},
	} {
		missed := instanceStatuses([]string{c1}, obs, testNow, reported)
		if got := missed[0].UnfinishedDatabase; got == nil || *got != *report {
			t.Errorf("%s: unfinishedDatabase = %+v, want the last report %+v kept", name, got, report)
		}
		before := &databasev1alpha1.CubridClusterStatus{Instances: reported}
		again := &databasev1alpha1.CubridClusterStatus{Instances: instanceStatuses([]string{c1}, answer(report), testNow,
			missed)}
		if got := events(transitions(&databasev1alpha1.CubridClusterStatus{Instances: missed}, again)); strings.Contains(got, "unfinished") {
			t.Errorf("%s: answering again after the miss made %q, want no unfinished-database transition", name, got)
		}
		if got := events(transitions(before, &databasev1alpha1.CubridClusterStatus{Instances: missed})); strings.Contains(got, "unfinished") {
			t.Errorf("%s: the miss made %q, want no unfinished-database transition", name, got)
		}
	}
	if got := instanceStatuses([]string{c1}, answer(nil), testNow, reported)[0].UnfinishedDatabase; got != nil {
		t.Errorf("an answer without the marker: unfinishedDatabase = %+v, want none", got)
	}
}

// A member that begins to report an unfinished database, or reports it in
// another state or for another operation, makes an Event naming the member
// and the database: a Warning when no operation takes it up now. One that
// answers without it is logged only.
func TestTransitions_UnfinishedDatabase(t *testing.T) {
	member := func(u *databasev1alpha1.InstanceUnfinishedDatabase) *databasev1alpha1.CubridClusterStatus {
		return &databasev1alpha1.CubridClusterStatus{Instances: []databasev1alpha1.InstanceStatus{
			{Name: replSlaveA, Role: databasev1alpha1.RoleSlave, UnfinishedDatabase: u}}}
	}
	waiting := unfinished(databasev1alpha1.UnfinishedWaitingForOperation, unfinishedOp)
	refused := unfinished(databasev1alpha1.UnfinishedRefused, "")
	other := unfinished(databasev1alpha1.UnfinishedWaitingForOperation, "op-ffffffffffffffffffffffffffffffff")
	for name, tc := range map[string]struct {
		before, after *databasev1alpha1.CubridClusterStatus
		want          string
		warning       bool
	}{
		"waiting begins":        {member(nil), member(waiting), "member_database_unfinished/UnfinishedDatabaseWaiting", false},
		"refused begins":        {member(nil), member(refused), "member_database_unfinished/UnfinishedDatabaseRefused", true},
		"waiting turns refused": {member(waiting), member(refused), "member_database_unfinished/UnfinishedDatabaseRefused", true},
		"another operation":     {member(waiting), member(other), "member_database_unfinished/UnfinishedDatabaseWaiting", false},
		"unchanged":             {member(refused), member(refused), "", false},
		"cleared":               {member(refused), member(nil), "member_database_unfinished_cleared/", false},
		"never reported":        {member(nil), member(nil), "", false},
	} {
		got := transitions(tc.before, tc.after)
		if events(got) != tc.want {
			t.Errorf("%s: transitions = %q, want %q", name, events(got), tc.want)
			continue
		}
		if len(got) != 1 {
			continue
		}
		if got[0].Warning != tc.warning {
			t.Errorf("%s: warning = %v, want %v", name, got[0].Warning, tc.warning)
		}
		if !strings.Contains(got[0].Message, replSlaveA) || !strings.Contains(got[0].Message, testDatabase) {
			t.Errorf("%s: the message does not name the member and the database: %q", name, got[0].Message)
		}
	}
}

// unfinishedProber reports one master and slaves, and an unfinished database
// on the members named in reports.
type unfinishedProber struct {
	mu      sync.Mutex
	master  string
	reports map[string]*databasev1alpha1.InstanceUnfinishedDatabase
}

func (p *unfinishedProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	role := databasev1alpha1.RoleSlave
	if podName == p.master {
		role = databasev1alpha1.RoleMaster
	}
	return RoleObservation{Reachable: true, Role: role, ObservedAt: time.Now(), Unfinished: p.reports[podName]}
}

func (p *unfinishedProber) report(member string, u *databasev1alpha1.InstanceUnfinishedDatabase) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports[member] = u
}

var _ = Describe("A member whose database waits on an unfinished operation (#315)", func() {
	ctx := context.Background()

	It("is reported in instances[] and by an Event, without hiding its role", func() {
		key := types.NamespacedName{Name: "unfinished", Namespace: testNamespace}
		c := haCluster(key.Name)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		prober := &unfinishedProber{master: key.Name + "-0",
			reports: map[string]*databasev1alpha1.InstanceUnfinishedDatabase{}}
		recorder := record.NewFakeRecorder(50)
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder, Prober: prober}
		reconcileOnce := func() []string {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			return drain(recorder)
		}
		instances := func() []databasev1alpha1.InstanceStatus {
			got := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			return got.Status.Instances
		}

		By("no member reports a marker: nothing new")
		Expect(reconcileOnce()).NotTo(ContainElement(ContainSubstring("UnfinishedDatabase")))
		for _, in := range instances() {
			Expect(in.UnfinishedDatabase).To(BeNil(), in.Name)
		}

		By("a slave reports a marker no operation removes")
		refused := unfinished(databasev1alpha1.UnfinishedRefused, "")
		prober.report(key.Name+"-2", refused)
		Expect(reconcileOnce()).To(ContainElement(And(HavePrefix("Warning UnfinishedDatabaseRefused"),
			ContainSubstring(key.Name+"-2"), ContainSubstring(testDatabase))))
		got := instances()
		Expect(got[2].UnfinishedDatabase).To(Equal(refused))
		Expect(got[2].Role).To(Equal(databasev1alpha1.RoleSlave), "the report hides no role")
		Expect(got[0].UnfinishedDatabase).To(BeNil())
		Expect(got[1].UnfinishedDatabase).To(BeNil())

		By("the same report again makes no Event")
		Expect(reconcileOnce()).NotTo(ContainElement(ContainSubstring("UnfinishedDatabase")))

		By("the marker is gone")
		prober.report(key.Name+"-2", nil)
		Expect(reconcileOnce()).NotTo(ContainElement(ContainSubstring("UnfinishedDatabase")))
		Expect(instances()[2].UnfinishedDatabase).To(BeNil())
	})

	It("is reported for the member of a standalone cluster", func() {
		key := types.NamespacedName{Name: "sa-unfinished", Namespace: testNamespace}
		c := standaloneCluster(key.Name)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		waiting := unfinished(databasev1alpha1.UnfinishedWaitingForOperation, unfinishedOp)
		prober := &unfinishedProber{reports: map[string]*databasev1alpha1.InstanceUnfinishedDatabase{key.Name + "-0": waiting}}
		recorder := record.NewFakeRecorder(50)
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder, Prober: prober}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(drain(recorder)).To(ContainElement(And(HavePrefix("Normal UnfinishedDatabaseWaiting"),
			ContainSubstring(key.Name+"-0"))))
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Instances).To(HaveLen(1))
		Expect(got.Status.Instances[0].UnfinishedDatabase).To(Equal(waiting))
	})
})
