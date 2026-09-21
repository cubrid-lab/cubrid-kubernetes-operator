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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

func brokerTestCluster() *databasev1alpha1.CubridCluster {
	return &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cc", Namespace: "ns"},
		Spec: databasev1alpha1.CubridClusterSpec{
			Version:   "11.4",
			Databases: []databasev1alpha1.CubridDatabase{{Name: "demodb"}},
			Topology:  databasev1alpha1.CubridTopology{PromotableMembers: 3},
		},
	}
}

func TestGenerateBrokerConf_RWandRO(t *testing.T) {
	conf := generateBrokerConf()
	for _, want := range []string{
		"[%RW]", "ACCESS_MODE             =RW", "BROKER_PORT             =33000",
		"[%RO]", "ACCESS_MODE             =RO", "BROKER_PORT             =33001",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("broker conf missing %q\n%s", want, conf)
		}
	}
}

func TestMemberDNSNames_AllMembers(t *testing.T) {
	got := memberDNSNames(brokerTestCluster())
	want := []string{
		"cc-0.cc-instances.ns.svc",
		"cc-1.cc-instances.ns.svc",
		"cc-2.cc-instances.ns.svc",
	}
	if len(got) != len(want) {
		t.Fatalf("member DNS names = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("member[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGenerateBrokerDatabasesTxt_HostList(t *testing.T) {
	txt := generateBrokerDatabasesTxt(brokerTestCluster())
	// db-host must list every member joined by ':' so the RW broker seeks the
	// master across the whole HA host list and follows failover (ADR-0002).
	wantHosts := "cc-0.cc-instances.ns.svc:cc-1.cc-instances.ns.svc:cc-2.cc-instances.ns.svc"
	if !strings.Contains(txt, wantHosts) {
		t.Errorf("databases.txt missing host list %q\n%s", wantHosts, txt)
	}
	if !strings.Contains(txt, "demodb\t") {
		t.Errorf("databases.txt missing db name: %s", txt)
	}
}

func TestBrokerLabels_ComponentBroker(t *testing.T) {
	labels := brokerLabelsFor(brokerTestCluster())
	if labels["app.kubernetes.io/component"] != componentBroker {
		t.Errorf("component label = %q, want broker", labels["app.kubernetes.io/component"])
	}
	if labels["app.kubernetes.io/instance"] != "cc" {
		t.Errorf("instance label = %q", labels["app.kubernetes.io/instance"])
	}
}

func TestSetBrokerConditions_RoutingGatedOnPrimary(t *testing.T) {
	r := &CubridClusterReconciler{}

	t.Run("resolved primary -> RoutingReady True", func(t *testing.T) {
		c := brokerTestCluster()
		r.setBrokerConditions(c, PrimaryResolution{CurrentPrimary: "cc-0", Status: metav1.ConditionTrue, Reason: singlePrimary})
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionRoutingReady)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("RoutingReady = %+v, want True", cond)
		}
	})

	t.Run("ambiguous primary -> RoutingReady False (never claim write-endpoint safety)", func(t *testing.T) {
		c := brokerTestCluster()
		r.setBrokerConditions(c, PrimaryResolution{Status: metav1.ConditionFalse, Reason: "MultiplePrimariesObserved"})
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionRoutingReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("RoutingReady = %+v, want False", cond)
		}
		if cond.Reason != "MultiplePrimariesObserved" {
			t.Errorf("RoutingReady reason = %q, want MultiplePrimariesObserved", cond.Reason)
		}
	})

	t.Run("no primary -> RoutingReady False", func(t *testing.T) {
		c := brokerTestCluster()
		r.setBrokerConditions(c, PrimaryResolution{Status: metav1.ConditionFalse, Reason: "NoPrimaryObserved"})
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionRoutingReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("RoutingReady = %+v, want False", cond)
		}
	})
}
