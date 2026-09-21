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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
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
		{"upgrade -> blocked", "11.5", cubridVersion, UpdateEngineUpgradeBlocked},
		{"downgrade -> blocked", "11.3", cubridVersion, UpdateEngineUpgradeBlocked},
		{"missing desired -> unverifiable", "", cubridVersion, UpdateUnverifiable},
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
		})
		if !ok || v != cubridVersion {
			t.Errorf("= %q,%v want 11.4,true", v, ok)
		}
	})
	t.Run("mixed -> not agreed", func(t *testing.T) {
		if _, ok := agreedEngineVersion([]databasev1alpha1.InstanceStatus{
			{ObservedEngineVersion: cubridVersion}, {ObservedEngineVersion: "11.5"},
		}); ok {
			t.Error("mixed versions must not agree")
		}
	})
	t.Run("none reported -> not agreed", func(t *testing.T) {
		if _, ok := agreedEngineVersion([]databasev1alpha1.InstanceStatus{{}, {}}); ok {
			t.Error("no reported versions must not agree")
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
	r.reconcileUpdateGuard(c)

	cond := meta.FindStatusCondition(c.Status.Conditions, conditionUpdating)
	if cond == nil || cond.Reason != "UpToDate" {
		t.Errorf("Updating = %+v, want UpToDate", cond)
	}
}

func TestReconcileUpdateGuard_RecordsBaselineFromInstances(t *testing.T) {
	r := &CubridClusterReconciler{}
	c := guardCluster(cubridVersion, "")
	c.Status.Instances = []databasev1alpha1.InstanceStatus{
		{Name: "c-0", ObservedEngineVersion: cubridVersion},
		{Name: "c-1", ObservedEngineVersion: cubridVersion},
	}
	r.reconcileUpdateGuard(c)

	if c.Status.ObservedEngineVersion != cubridVersion {
		t.Errorf("observed baseline = %q, want 11.4", c.Status.ObservedEngineVersion)
	}
	cond := meta.FindStatusCondition(c.Status.Conditions, conditionUpdating)
	if cond == nil || cond.Reason != "UpToDate" {
		t.Errorf("Updating = %+v, want UpToDate after recording matching baseline", cond)
	}
}
