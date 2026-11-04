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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// testNow is the clock every resolution test reads; observations are stamped
// relative to it so freshness is decided by the test, not the wall clock.
var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func master() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow}
}
func slave() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow}
}
func unknown() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: testNow}
}
func unreach() RoleObservation { return RoleObservation{Reachable: false, ObservedAt: testNow} }

// aged returns the observation as if it had been taken `age` before testNow.
func aged(o RoleObservation, age time.Duration) RoleObservation {
	o.ObservedAt = testNow.Add(-age)
	return o
}

// conflicting returns the observation flagged as contradicting its own HA status.
func conflicting(o RoleObservation) RoleObservation {
	o.Conflicting = true
	return o
}

const (
	c0                = "c-0"
	c1                = "c-1"
	c2                = "c-2"
	incomplete        = "PrimaryObservationIncomplete"
	multiplePrimaries = "MultiplePrimariesObserved"
	singlePrimary     = "SinglePrimaryObserved"
	ambiguous         = "AmbiguousPrimaryObservation"
	noPrimary         = "NoPrimaryObserved"
	cubridVersion     = "11.4"
)

func TestResolvePrimary(t *testing.T) {
	members := []string{c0, c1, c2}
	cases := []struct {
		name   string
		obs    map[string]RoleObservation
		status metav1.ConditionStatus
		reason string
		prim   string
	}{
		{"healthy single primary", map[string]RoleObservation{c0: master(), c1: slave(), c2: slave()},
			metav1.ConditionTrue, singlePrimary, c0},
		{"two masters -> not resolved", map[string]RoleObservation{c0: master(), c1: master(), c2: slave()},
			metav1.ConditionFalse, multiplePrimaries, ""},
		{"no master", map[string]RoleObservation{c0: slave(), c1: slave(), c2: slave()},
			metav1.ConditionFalse, noPrimary, ""},
		{"one unreachable -> incomplete (safety)", map[string]RoleObservation{c0: master(), c1: slave(), c2: unreach()},
			metav1.ConditionFalse, incomplete, ""},
		{"one unknown -> incomplete (safety)", map[string]RoleObservation{c0: master(), c1: unknown(), c2: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"missing member -> incomplete", map[string]RoleObservation{c0: master(), c1: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"multiple masters wins over incomplete", map[string]RoleObservation{c0: master(), c1: master(), c2: unreach()},
			metav1.ConditionFalse, multiplePrimaries, ""},
		{"stale master -> incomplete, not resolved",
			map[string]RoleObservation{c0: aged(master(), roleObservationTTL+time.Second), c1: slave(), c2: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"stale slave -> incomplete", map[string]RoleObservation{c0: master(), c1: slave(), c2: aged(slave(), time.Hour)},
			metav1.ConditionFalse, incomplete, ""},
		{"observation exactly at the TTL is still fresh",
			map[string]RoleObservation{c0: aged(master(), roleObservationTTL), c1: slave(), c2: slave()},
			metav1.ConditionTrue, singlePrimary, c0},
		{"observation without a time -> incomplete",
			map[string]RoleObservation{c0: master(), c1: {Reachable: true, Role: databasev1alpha1.RoleSlave}, c2: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"observation from the future (clock skew) -> incomplete",
			map[string]RoleObservation{c0: aged(master(), -roleObservationTTL-time.Second), c1: slave(), c2: slave()},
			metav1.ConditionFalse, incomplete, ""},
		{"master contradicting its own HA status -> ambiguous",
			map[string]RoleObservation{c0: conflicting(master()), c1: slave(), c2: slave()},
			metav1.ConditionFalse, ambiguous, ""},
		{"slave contradicting its own HA status -> ambiguous",
			map[string]RoleObservation{c0: master(), c1: conflicting(slave()), c2: slave()},
			metav1.ConditionFalse, ambiguous, ""},
		{"multiple masters wins over ambiguous",
			map[string]RoleObservation{c0: master(), c1: master(), c2: conflicting(slave())},
			metav1.ConditionFalse, multiplePrimaries, ""},
		{"ambiguous wins over incomplete",
			map[string]RoleObservation{c0: conflicting(master()), c1: unreach(), c2: slave()},
			metav1.ConditionFalse, ambiguous, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolvePrimary(members, c.obs, testNow)
			if got.Status != c.status || got.Reason != c.reason || got.CurrentPrimary != c.prim {
				t.Errorf("got {%s %s %q}, want {%s %s %q}",
					got.Status, got.Reason, got.CurrentPrimary, c.status, c.reason, c.prim)
			}
		})
	}
}

func TestInstanceStatuses(t *testing.T) {
	members := []string{c0, c1, c2}
	obs := map[string]RoleObservation{c0: master(), c1: slave(), c2: unreach()}
	got := instanceStatuses(members, obs, testNow, nil, nil)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Role != databasev1alpha1.RoleMaster || !got[0].Ready || got[0].Ordinal != 0 {
		t.Errorf("c-0 = %+v", got[0])
	}
	if got[1].Role != databasev1alpha1.RoleSlave || !got[1].Ready {
		t.Errorf("c-1 = %+v", got[1])
	}
	// unreachable -> unknown, not ready
	if got[2].Role != databasev1alpha1.RoleUnknown || got[2].Ready {
		t.Errorf("c-2 = %+v (unreachable must be unknown/not-ready)", got[2])
	}
}

// A stale or conflicting observation is reported as unknown and not ready, the
// same way the cluster verdict treats it (ADR-0005).
func TestInstanceStatusesTreatStaleAndConflictingAsUnknown(t *testing.T) {
	members := []string{c0, c1}
	obs := map[string]RoleObservation{c0: aged(master(), time.Hour), c1: conflicting(slave())}
	for _, got := range instanceStatuses(members, obs, testNow, nil, nil) {
		if got.Role != databasev1alpha1.RoleUnknown || got.Ready {
			t.Errorf("%s = %+v, want unknown and not ready", got.Name, got)
		}
	}
}

// Write routing enters quarantine on any unsafe snapshot and leaves it only when
// the current snapshot alone proves a single primary: no earlier verdict, stored
// status or old observation keeps routing open (ADR-0005 safety rule).
func TestWriteRoutingEntersAndLeavesQuarantine(t *testing.T) {
	members := []string{c0, c1, c2}
	r := &CubridClusterReconciler{}
	cluster := &databasev1alpha1.CubridCluster{}
	steps := []struct {
		name   string
		obs    map[string]RoleObservation
		ready  metav1.ConditionStatus
		reason string
	}{
		{"healthy", map[string]RoleObservation{c0: master(), c1: slave(), c2: slave()},
			metav1.ConditionTrue, "PrimaryResolved"},
		{"two masters enter quarantine", map[string]RoleObservation{c0: master(), c1: master(), c2: slave()},
			metav1.ConditionFalse, multiplePrimaries},
		{"a conflicting member keeps it", map[string]RoleObservation{c0: master(), c1: conflicting(slave()), c2: slave()},
			metav1.ConditionFalse, ambiguous},
		{"a stale member keeps it", map[string]RoleObservation{c0: master(), c1: slave(), c2: aged(slave(), time.Hour)},
			metav1.ConditionFalse, incomplete},
		{"an unreachable member keeps it", map[string]RoleObservation{c0: master(), c1: slave(), c2: unreach()},
			metav1.ConditionFalse, incomplete},
		{"no master is not ready either", map[string]RoleObservation{c0: slave(), c1: slave(), c2: slave()},
			metav1.ConditionFalse, noPrimary},
		{"a fresh, complete single primary leaves it", map[string]RoleObservation{c0: slave(), c1: master(), c2: slave()},
			metav1.ConditionTrue, "PrimaryResolved"},
	}
	for _, step := range steps {
		res := resolvePrimary(members, step.obs, testNow)
		// A read-write Broker is available throughout: this is about the primary.
		r.setBrokerConditions(cluster, res, map[string]int32{brokerModeRW: 1})
		got := findCondition(cluster, conditionRoutingReady)
		if got == nil || got.Status != step.ready || got.Reason != step.reason {
			t.Fatalf("%s: RoutingReady = %+v, want %s/%s", step.name, got, step.ready, step.reason)
		}
	}
}

// A member that answered in a pass carries the pass's time, the time the
// cluster's lastObservedTime is set to, also when its answer was stamped
// earlier by the prober: an equal time means "answered in that pass". A
// member that did not answer keeps its time from before.
func TestInstanceStatuses_LastObservedTimeIsThePass(t *testing.T) {
	members := []string{c0, c1}
	earlier := metav1.NewTime(testNow.Add(-time.Minute))
	previous := []databasev1alpha1.InstanceStatus{{Name: c0, LastObservedTime: &earlier},
		{Name: c1, LastObservedTime: &earlier}}
	answered := master()
	answered.ObservedAt = testNow.Add(-1500 * time.Millisecond)
	obs := map[string]RoleObservation{c0: answered, c1: unreach()}
	got := instanceStatuses(members, obs, testNow, previous, nil)
	if at := got[0].LastObservedTime; at == nil || !at.Time.Equal(testNow) {
		t.Errorf("member that answered: lastObservedTime = %v, want the pass's time %v", at, testNow)
	}
	if at := got[1].LastObservedTime; at == nil || !at.Time.Equal(earlier.Time) {
		t.Errorf("member that did not answer: lastObservedTime = %v, want %v", at, earlier)
	}

	// An answer that contradicts itself or names another member is no
	// answer of this member: its time is kept.
	obs[c1] = conflicting(slave())
	if at := instanceStatuses(members, obs, testNow, previous, nil)[1].LastObservedTime; at == nil || !at.Time.Equal(earlier.Time) {
		t.Errorf("member with a conflicting answer: lastObservedTime = %v, want %v", at, earlier)
	}
}

// A stored observation time is kept only while nothing else changes, it is
// younger than observationWriteInterval, and the new time does not go back.
// When any time is written, all of them are current.
func TestHoldObservationTimes(t *testing.T) {
	stamp := func(at time.Time) *metav1.Time { m := metav1.NewTime(at); return &m }
	// status is a master c0 and a slave c1 whose applier was read, observed
	// at the given times; members limits the members to the ones named.
	status := func(cluster, member, applier time.Time, members ...string) *databasev1alpha1.CubridClusterStatus {
		s := &databasev1alpha1.CubridClusterStatus{CurrentPrimary: c0, LastObservedTime: stamp(cluster)}
		for _, m := range members {
			in := databasev1alpha1.InstanceStatus{Name: m, Role: databasev1alpha1.RoleSlave, LastObservedTime: stamp(member),
				Replication: &databasev1alpha1.InstanceReplication{Source: c0, AppliedChanges: 10, ObservedAt: stamp(applier)}}
			if m == c0 {
				in.Role, in.Replication = databasev1alpha1.RoleMaster, nil
			}
			s.Instances = append(s.Instances, in)
		}
		return s
	}
	both := []string{c0, c1}
	stored := status(testNow, testNow, testNow, both...)
	at := func(d time.Duration) time.Time { return testNow.Add(d) }
	// The API server stores whole seconds; the current times do not have to be.
	soon := at(10*time.Second + 500*time.Millisecond)
	changed := status(soon, soon, soon, both...)
	changed.CurrentPrimary = ""
	cases := []struct {
		name           string
		stored, status *databasev1alpha1.CubridClusterStatus
		now, want      time.Time
	}{
		{"within the interval", stored, status(soon, soon, soon, both...), soon, testNow},
		{"after the interval", stored, status(at(observationWriteInterval), at(observationWriteInterval),
			at(observationWriteInterval), both...), at(observationWriteInterval), at(observationWriteInterval)},
		{"only the applier's time moved", stored, status(testNow, testNow, soon, both...), soon, testNow},
		{"the clock went back", stored, status(at(-time.Second), at(-time.Second), at(-time.Second), both...),
			at(-time.Second), at(-time.Second)},
		{"something else changed", stored, changed, soon, soon},
		{"the cluster's time is due while a member's is not",
			status(at(-observationWriteInterval), at(-5*time.Second), at(-5*time.Second), both...),
			status(soon, soon, soon, both...), soon, soon},
		{"a member was added", status(testNow, testNow, testNow, c0), status(soon, soon, soon, both...), soon, soon},
		{"a member was removed", stored, status(soon, soon, soon, c0), soon, soon},
		{"a first observation", &databasev1alpha1.CubridClusterStatus{CurrentPrimary: c0}, status(soon, soon, soon, both...),
			soon, soon},
	}
	for _, tc := range cases {
		holdObservationTimes(tc.stored, tc.status, tc.now)
		want := stamp(tc.want)
		if !tc.status.LastObservedTime.Equal(want) {
			t.Errorf("%s: lastObservedTime = %v, want %v", tc.name, tc.status.LastObservedTime, want)
		}
		for _, in := range tc.status.Instances {
			if !in.LastObservedTime.Equal(want) {
				t.Errorf("%s: %s lastObservedTime = %v, want %v", tc.name, in.Name, in.LastObservedTime, want)
			}
			if in.Replication != nil && !in.Replication.ObservedAt.Equal(want) {
				t.Errorf("%s: %s replication.observedAt = %v, want %v", tc.name, in.Name, in.Replication.ObservedAt, want)
			}
		}
	}
}

func findCondition(cluster *databasev1alpha1.CubridCluster, condType string) *metav1.Condition {
	for i := range cluster.Status.Conditions {
		if cluster.Status.Conditions[i].Type == condType {
			return &cluster.Status.Conditions[i]
		}
	}
	return nil
}
