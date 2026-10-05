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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

func TestClassifyEngineChange(t *testing.T) {
	tests := []struct {
		name     string
		desired  string
		observed string
		want     UpdateClass
	}{
		{"match -> none", cubridVersion, cubridVersion, UpdateNone},
		{"no baseline yet -> none", cubridVersion, "", UpdateNone},
		{"upgrade -> blocked", otherSeries, cubridVersion, UpdateEngineUpgradeBlocked},
		{"downgrade -> blocked", "11.3", cubridVersion, UpdateEngineUpgradeBlocked},
		{"missing desired -> unverifiable", "", cubridVersion, UpdateUnverifiable},
		// spec.version names a series; the engine reports its full version (#158).
		{"full version of the series -> none", cubridVersion, fullEngineVersion, UpdateNone},
		{"other series -> blocked", otherSeries, fullEngineVersion, UpdateEngineUpgradeBlocked},
		{"unparseable observed -> unverifiable", cubridVersion, "garbage", UpdateUnverifiable},
		{"desired without a minor -> unverifiable", "11", fullEngineVersion, UpdateUnverifiable},
	}
	for _, tc := range tests {
		if got := classifyEngineChange(tc.desired, tc.observed); got != tc.want {
			t.Errorf("%s: classifyEngineChange(%q,%q) = %q, want %q", tc.name, tc.desired, tc.observed, got, tc.want)
		}
	}
}

func TestAgreedEngineVersion(t *testing.T) {
	t.Run("all agree", func(t *testing.T) {
		v, ok := agreedEngineVersion([]databasev1alpha1.InstanceStatus{
			{ObservedEngineVersion: cubridVersion}, {ObservedEngineVersion: cubridVersion},
		}, 2)
		if !ok || v != cubridVersion {
			t.Errorf("= %q,%v want 11.4,true", v, ok)
		}
	})
	t.Run("mixed -> not agreed", func(t *testing.T) {
		if _, ok := agreedEngineVersion([]databasev1alpha1.InstanceStatus{
			{ObservedEngineVersion: cubridVersion}, {ObservedEngineVersion: "11.5"},
		}, 2); ok {
			t.Error("mixed versions must not agree")
		}
	})
	t.Run("none reported -> not agreed", func(t *testing.T) {
		if _, ok := agreedEngineVersion([]databasev1alpha1.InstanceStatus{{}, {}}, 2); ok {
			t.Error("no reported versions must not agree")
		}
	})
	t.Run("fewer members than expected -> not agreed", func(t *testing.T) {
		if _, ok := agreedEngineVersion([]databasev1alpha1.InstanceStatus{
			{ObservedEngineVersion: cubridVersion}, {ObservedEngineVersion: cubridVersion},
		}, 3); ok {
			t.Error("two of three members must not set the baseline")
		}
	})
}

func guardCluster(desired, observed string) *databasev1alpha1.CubridCluster {
	c := &databasev1alpha1.CubridCluster{}
	c.Spec.Version = desired
	c.Status.ObservedEngineVersion = observed
	return c
}

func TestReconcileUpdateGuard_BlocksUpgrade(t *testing.T) {
	r := &CubridClusterReconciler{}
	c := guardCluster("11.5", cubridVersion)
	r.reconcileUpdateGuard(c)

	cond := meta.FindStatusCondition(c.Status.Conditions, conditionUpdating)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Updating = %+v, want False", cond)
	}
	if cond.Reason != "UpdateBlockedEngineUpgrade" {
		t.Errorf("reason = %q, want UpdateBlockedEngineUpgrade", cond.Reason)
	}
}

func TestReconcileUpdateGuard_BlocksUnverifiable(t *testing.T) {
	r := &CubridClusterReconciler{}
	c := guardCluster("", cubridVersion)
	r.reconcileUpdateGuard(c)

	cond := meta.FindStatusCondition(c.Status.Conditions, conditionUpdating)
	if cond == nil || cond.Reason != "UpdateBlockedUnverifiableEngineVersion" {
		t.Errorf("Updating = %+v, want UpdateBlockedUnverifiableEngineVersion", cond)
	}
}

func TestReconcileUpdateGuard_UpToDate(t *testing.T) {
	r := &CubridClusterReconciler{}
	c := guardCluster(cubridVersion, cubridVersion)
	if !r.reconcileUpdateGuard(c) {
		t.Error("guard should return true (safe to roll) when versions match")
	}
	cond := meta.FindStatusCondition(c.Status.Conditions, conditionUpdating)
	if cond != nil && cond.Status == metav1.ConditionFalse {
		t.Errorf("guard must not block a matching version: %+v", cond)
	}
}

func TestReconcileUpdateGuard_RecordsBaselineFromInstances(t *testing.T) {
	r := &CubridClusterReconciler{}
	c := guardCluster(cubridVersion, "")
	c.Spec.Topology.PromotableMembers = 2
	c.Status.Instances = []databasev1alpha1.InstanceStatus{
		{Name: "c-0", ObservedEngineVersion: cubridVersion},
		{Name: c1, ObservedEngineVersion: cubridVersion},
	}
	if !r.reconcileUpdateGuard(c) {
		t.Error("guard should return true after recording a matching baseline")
	}
	if c.Status.ObservedEngineVersion != cubridVersion {
		t.Errorf("observed baseline = %q, want %q", c.Status.ObservedEngineVersion, cubridVersion)
	}
}

const otherSeries = "11.5"

// fullEngineVersion is what cubrid_rel reports for the 11.4 image (POC-10).
const fullEngineVersion = "11.4.6.1963"

func TestEngineSeries(t *testing.T) {
	for in, want := range map[string]string{
		cubridVersion: cubridVersion, fullEngineVersion: cubridVersion, " 11.4.6 ": cubridVersion,
		"11": "", "": "", "garbage": "", "11.": "", ".4": "", "v11.4": "", "11.x": "",
	} {
		if got := engineSeries(in); got != want {
			t.Errorf("engineSeries(%q) = %q, want %q", in, got, want)
		}
	}
}

// The members' reported version becomes the baseline; a spec naming that
// series is not a change, another series is blocked.
func TestUpdateGuard_UsesReportedEngineVersion(t *testing.T) {
	now := time.Now()
	members := []string{c0, c1, c2}
	obs := map[string]RoleObservation{}
	for i, m := range members {
		role := databasev1alpha1.RoleSlave
		if i == 0 {
			role = databasev1alpha1.RoleMaster
		}
		obs[m] = RoleObservation{Reachable: true, Role: role, ObservedAt: now, EngineVersion: fullEngineVersion}
	}
	instances := instanceStatuses(members, obs, now)
	for _, in := range instances {
		if in.ObservedEngineVersion != fullEngineVersion {
			t.Fatalf("instance %s observedEngineVersion = %q", in.Name, in.ObservedEngineVersion)
		}
	}

	r := &CubridClusterReconciler{}
	same := &databasev1alpha1.CubridCluster{}
	same.Spec.Version = cubridVersion
	same.Spec.Topology.PromotableMembers = 3
	same.Status.Instances = instances
	if !r.reconcileUpdateGuard(same) {
		t.Errorf("spec %q on engine %q must not be blocked", cubridVersion, fullEngineVersion)
	}
	if same.Status.ObservedEngineVersion != fullEngineVersion {
		t.Errorf("baseline = %q, want the full version", same.Status.ObservedEngineVersion)
	}

	other := &databasev1alpha1.CubridCluster{}
	other.Spec.Version = otherSeries
	other.Spec.Topology.PromotableMembers = 3
	other.Status.Instances = instances
	if r.reconcileUpdateGuard(other) {
		t.Errorf("spec 11.5 on engine %q must be blocked", fullEngineVersion)
	}
}

// A member that cannot be reached, or whose answer is stale, reports no version.
func TestInstanceStatuses_VersionNeedsAFreshAnswer(t *testing.T) {
	now := time.Now()
	obs := map[string]RoleObservation{
		c0: {Reachable: false, ObservedAt: now, EngineVersion: fullEngineVersion},
		c1: {Reachable: true, ObservedAt: now.Add(-time.Hour), EngineVersion: fullEngineVersion},
	}
	for _, in := range instanceStatuses([]string{c0, c1}, obs, now) {
		if in.ObservedEngineVersion != "" {
			t.Errorf("instance %s reports %q from an unusable observation", in.Name, in.ObservedEngineVersion)
		}
	}
}

func TestObservationFromStatus_CarriesEngineVersion(t *testing.T) {
	for _, role := range []instancemanager.Role{instancemanager.RoleMaster, instancemanager.RoleUnknown} {
		o := observationFromStatus(instancemanager.HAStatus{Role: role, EngineVersion: fullEngineVersion}, time.Now())
		if o.EngineVersion != fullEngineVersion {
			t.Errorf("role %s: EngineVersion = %q", role, o.EngineVersion)
		}
	}
}

// threeInstances builds status.instances for a three-member cluster from the
// versions given; "" is a member without a fresh version.
func threeInstances(versions ...string) []databasev1alpha1.InstanceStatus {
	out := make([]databasev1alpha1.InstanceStatus, 0, len(versions))
	for i, v := range versions {
		out = append(out, databasev1alpha1.InstanceStatus{
			Name: []string{c0, c1, c2}[i], Ordinal: int32(i), ObservedEngineVersion: v, //nolint:gosec // a small test index
		})
	}
	return out
}

func haGuardCluster(baseline string, versions ...string) *databasev1alpha1.CubridCluster {
	c := guardCluster(cubridVersion, baseline)
	c.Spec.Topology.PromotableMembers = 3
	c.Status.Instances = threeInstances(versions...)
	return c
}

func updatingReason(c *databasev1alpha1.CubridCluster) string {
	if cond := meta.FindStatusCondition(c.Status.Conditions, conditionUpdating); cond != nil {
		return cond.Reason
	}
	return ""
}

// The baseline is the version every member reported, not that of a subset (#198).
func TestUpdateGuard_BaselineNeedsEveryMember(t *testing.T) {
	r := &CubridClusterReconciler{}
	tests := []struct {
		name     string
		versions []string
	}{
		{"one of three reports", []string{fullEngineVersion, "", ""}},
		{"two of three report", []string{fullEngineVersion, fullEngineVersion, ""}},
		{"none reports", []string{"", "", ""}},
		{"they disagree", []string{fullEngineVersion, fullEngineVersion, "11.4.5.1000"}},
		{"a member is missing from status", []string{fullEngineVersion, fullEngineVersion}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := haGuardCluster("", tc.versions...)
			if r.reconcileUpdateGuard(c) {
				t.Error("the guard let an update through without a verified engine version")
			}
			if c.Status.ObservedEngineVersion != "" {
				t.Errorf("baseline recorded as %q from incomplete observations", c.Status.ObservedEngineVersion)
			}
			if got := updatingReason(c); got != "UpdateBlockedEngineVersionNotObserved" {
				t.Errorf("Updating reason = %q", got)
			}
		})
	}

	t.Run("every member reports the same version", func(t *testing.T) {
		c := haGuardCluster("", fullEngineVersion, fullEngineVersion, fullEngineVersion)
		if !r.reconcileUpdateGuard(c) {
			t.Errorf("a complete, agreeing observation must not block: %s", updatingReason(c))
		}
		if c.Status.ObservedEngineVersion != fullEngineVersion {
			t.Errorf("baseline = %q", c.Status.ObservedEngineVersion)
		}
	})
}

// A recorded baseline does not end the checking: a member whose version is
// unknown now, or belongs to another series, blocks an update (#198).
func TestUpdateGuard_ChecksMembersAgainstTheBaseline(t *testing.T) {
	r := &CubridClusterReconciler{}
	tests := []struct {
		name     string
		versions []string
		reason   string
	}{
		{"a member's version is unknown", []string{fullEngineVersion, "", fullEngineVersion},
			"UpdateBlockedEngineVersionUnknown"},
		{"a member is missing from status", []string{fullEngineVersion, fullEngineVersion},
			"UpdateBlockedEngineVersionUnknown"},
		{"a member runs another series", []string{fullEngineVersion, "11.5.0.0001", fullEngineVersion},
			"UpdateBlockedMixedEngineVersions"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := haGuardCluster(fullEngineVersion, tc.versions...)
			if r.reconcileUpdateGuard(c) {
				t.Error("the guard let an update through")
			}
			if got := updatingReason(c); got != tc.reason {
				t.Errorf("Updating reason = %q, want %q", got, tc.reason)
			}
			if c.Status.ObservedEngineVersion != fullEngineVersion {
				t.Errorf("the baseline changed to %q", c.Status.ObservedEngineVersion)
			}
		})
	}

	t.Run("another patch of the same series is not a mix", func(t *testing.T) {
		c := haGuardCluster(fullEngineVersion, fullEngineVersion, "11.4.7.2000", fullEngineVersion)
		if !r.reconcileUpdateGuard(c) {
			t.Errorf("a compatible patch update in progress must not block itself: %s", updatingReason(c))
		}
	})
}
