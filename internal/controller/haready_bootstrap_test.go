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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// The reason of the HA bootstrap that stopped in the tests below, as its
// copy to the last member stops when the master changed during that copy
// (#768).
const stoppedSeedReason = "SeedSourceChangedDuringSeed"

// bootstrapAt returns an HA cluster whose members all answer as healthy, the
// first as master and the others as slaves that apply its log, and whose
// first database's HA bootstrap is recorded as db.
func bootstrapAt(name string, db databasev1alpha1.DatabaseStatus) *databasev1alpha1.CubridCluster {
	cluster := haCluster(name)
	db.Name = testDatabase
	cluster.Status.Databases = []databasev1alpha1.DatabaseStatus{db}
	if db.Phase == databasePhaseFailed {
		setCondition(cluster, conditionBootstrapReady, metav1.ConditionFalse, stoppedSeedReason,
			"while database appdb was copied to "+name+"-2 from "+name+"-1, the master changed")
	}
	return cluster
}

// A member the HA bootstrap has not recorded as holding the master's data
// may lack what the master committed while it applies the master's log with
// nothing waiting (docs/poc/RESULTS.md, POC-57), so HAReady is never True
// for it, however healthy its role and replication look (#770). A bootstrap
// that stopped as Failed keeps HAReady False with its own reason. A cluster
// whose bootstrap completed, or that records no bootstrap, is judged by its
// roles and replication alone.
func TestHAReadyFollowsTheHABootstrap(t *testing.T) {
	const name = "haboot"
	first, peer, last := name+"-0", name+"-1", name+"-2"
	for tc, want := range map[string]struct {
		db       *databasev1alpha1.DatabaseStatus
		recovery bool
		status   metav1.ConditionStatus
		reason   string
		names    []string
	}{
		"no bootstrap recorded": {status: metav1.ConditionTrue, reason: reasonHealthyReplication},
		"database created, no member seeded yet": {
			db:     &databasev1alpha1.DatabaseStatus{Phase: databasePhaseCreated, PrimaryCreated: true},
			status: metav1.ConditionFalse, reason: reasonMemberNotSeeded, names: []string{"not recorded " + peer + ", " + last + " as"},
		},
		"first peer seeded, the last member restored but not caught up": {
			db: &databasev1alpha1.DatabaseStatus{Phase: databasePhaseCreated, PrimaryCreated: true,
				SeededMembers: []string{peer}, Seeding: &databasev1alpha1.DatabaseSeeding{Member: last, Source: peer}},
			status: metav1.ConditionFalse, reason: reasonMemberNotSeeded, names: []string{"not recorded " + last + " as"},
		},
		"recovery bootstrap seeding, the member it was restored on is the holder": {
			db: &databasev1alpha1.DatabaseStatus{Phase: databasePhaseCreated, PrimaryCreated: true,
				SeededMembers: []string{peer}},
			recovery: true, status: metav1.ConditionFalse, reason: reasonMemberNotSeeded, names: []string{"not recorded " + last + " as"},
		},
		"bootstrap stopped as Failed": {
			db: &databasev1alpha1.DatabaseStatus{Phase: databasePhaseFailed, PrimaryCreated: true,
				SeededMembers: []string{peer}, Seeding: &databasev1alpha1.DatabaseSeeding{Member: last, Source: peer}},
			status: metav1.ConditionFalse, reason: stoppedSeedReason, names: []string{testDatabase, last},
		},
		"bootstrap completed": {
			db: &databasev1alpha1.DatabaseStatus{Phase: databasePhaseCreated, PrimaryCreated: true, HAConfigured: true,
				SeededMembers: []string{peer, last}},
			status: metav1.ConditionTrue, reason: reasonHealthyReplication,
		},
		"recovery bootstrap completed": {
			db: &databasev1alpha1.DatabaseStatus{Phase: databasePhaseCreated, PrimaryCreated: true, HAConfigured: true,
				SeededMembers: []string{peer, last}},
			recovery: true, status: metav1.ConditionTrue, reason: reasonHealthyReplication,
		},
	} {
		t.Run(tc, func(t *testing.T) {
			cluster := haCluster(name)
			if want.db != nil {
				cluster = bootstrapAt(name, *want.db)
			}
			if want.recovery {
				cluster.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
					Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI}}
				cluster.Status.Bootstrap = &databasev1alpha1.BootstrapStatus{
					Phase: databasev1alpha1.BootstrapComplete, TargetMember: first}
			}
			r := &CubridClusterReconciler{Prober: &memberProber{master: first}}
			r.reconcileHAStatus(context.Background(), cluster, 3)
			if !meta.IsStatusConditionTrue(cluster.Status.Conditions, conditionReplicationHealthy) {
				t.Fatalf("the members do not answer as healthy: %+v", cluster.Status.Conditions)
			}
			got := meta.FindStatusCondition(cluster.Status.Conditions, conditionHAReady)
			if got == nil || got.Status != want.status || got.Reason != want.reason {
				t.Fatalf("HAReady = %+v, want %s with %s", got, want.status, want.reason)
			}
			for _, m := range want.names {
				if !strings.Contains(got.Message, m) {
					t.Errorf("HAReady's message %q does not say %q", got.Message, m)
				}
			}
		})
	}
}

// While the HA bootstrap is Failed, Ready is False with the bootstrap's
// failure reason, also when every instance is ready, and also when not all
// are (#770): the bootstrap waits for a person and is not starting. Two
// observed masters on ready instances still come first. While the bootstrap
// runs and a member is not yet recorded as seeded, Ready is False: with
// InstancesNotReady while not every instance is ready, and with
// MemberNotSeeded once every member's server runs, as while the last member
// catches up after its seed restore. A completed bootstrap leaves Ready to
// the instances and the roles.
func TestReadyFollowsTheHABootstrap(t *testing.T) {
	const name = "ready-boot"
	failed := databasev1alpha1.DatabaseStatus{Phase: databasePhaseFailed, PrimaryCreated: true,
		SeededMembers: []string{name + "-1"}}
	seeding := databasev1alpha1.DatabaseStatus{Phase: databasePhaseCreated, PrimaryCreated: true,
		SeededMembers: []string{name + "-1"}}
	completed := databasev1alpha1.DatabaseStatus{Phase: databasePhaseCreated, PrimaryCreated: true, HAConfigured: true,
		SeededMembers: []string{name + "-1", name + "-2"}}
	for tc, want := range map[string]struct {
		db       databasev1alpha1.DatabaseStatus
		ready    int32
		multiple bool
		reason   string
	}{
		"failed, every instance ready":    {db: failed, ready: 3, reason: stoppedSeedReason},
		"failed, an instance not ready":   {db: failed, ready: 2, reason: stoppedSeedReason},
		"failed, two masters observed":    {db: failed, ready: 3, multiple: true, reason: reasonMultiplePrimaries},
		"seeding, every instance ready":   {db: seeding, ready: 3, reason: reasonMemberNotSeeded},
		"seeding, an instance not ready":  {db: seeding, ready: 2, reason: "InstancesNotReady"},
		"seeding, two masters observed":   {db: seeding, ready: 3, multiple: true, reason: reasonMultiplePrimaries},
		"completed, every instance ready": {db: completed, ready: 3, reason: "ClusterReady"},
	} {
		t.Run(tc, func(t *testing.T) {
			cluster := bootstrapAt(name, want.db)
			r := &CubridClusterReconciler{Clock: func() time.Time { return testNow }}
			r.setReady(cluster, want.ready, 3, want.multiple)
			ready := meta.FindStatusCondition(cluster.Status.Conditions, conditionReady)
			if ready == nil || ready.Reason != want.reason ||
				(ready.Status == metav1.ConditionTrue) != (want.reason == "ClusterReady") {
				t.Fatalf("Ready = %+v, want reason %s", ready, want.reason)
			}
			if want.reason == reasonMemberNotSeeded && !strings.Contains(ready.Message, name+"-2") {
				t.Errorf("Ready's message %q does not name the member not seeded", ready.Message)
			}
			if want.reason == stoppedSeedReason {
				if !strings.Contains(ready.Message, "the master changed") {
					t.Errorf("Ready's message %q does not carry BootstrapReady's", ready.Message)
				}
				if p := meta.FindStatusCondition(cluster.Status.Conditions, conditionProgressing); p == nil ||
					p.Status != metav1.ConditionFalse {
					t.Errorf("Progressing = %+v, want False: a failed bootstrap is not in progress", p)
				}
			}
		})
	}
}

// A master the HA bootstrap has not recorded as holding the database was
// seeded across a change of master, or not at all, and can lack what an
// earlier master committed (POC-57, #768). A backup of it would keep that
// gap as a backup, so no backup is taken on it, by PrimaryOnly or by the
// fallback of PreferStandby; the backup waits with MasterNotSeeded (#770).
func TestSelectBackupTarget_RefusesMasterNotRecordedAsSeeded(t *testing.T) {
	members := []string{c0, c1, c2}
	onlyHolder := func(m string) bool { return m == c1 }
	noStandby := map[string]RoleObservation{c0: obsMaster(), c1: obsUnknown(), c2: obsUnknown()}
	for _, pref := range []databasev1alpha1.TargetPreference{databasev1alpha1.PrimaryOnly, databasev1alpha1.PreferStandby} {
		got := selectBackupTarget(members, noStandby, nil, nil, onlyHolder, resolvedPrimary(), pref, false, false, testNow)
		if got.Selected || got.Reason != reasonMasterNotSeeded {
			t.Errorf("%s: got %+v, want no target with MasterNotSeeded", pref, got)
		}
		got = selectBackupTarget(members, noStandby, nil, nil, nil, resolvedPrimary(), pref, false, false, testNow)
		if !got.Selected || got.Instance != c0 {
			t.Errorf("%s with every member admitted: got %+v, want the master %s", pref, got, c0)
		}
	}
	logEndUnread := map[string]RoleObservation{c0: masterLogEndUnread(), c1: obsSlave(), c2: obsSlave()}
	got := selectBackupTarget(members, logEndUnread, nil, nil, func(m string) bool { return m != c0 }, resolvedPrimary(),
		databasev1alpha1.PreferStandby, false, true, testNow)
	if got.Selected || got.Reason != reasonMasterNotSeeded {
		t.Errorf("fallback after the wait for the master's log end: got %+v, want no target with MasterNotSeeded", got)
	}
}
