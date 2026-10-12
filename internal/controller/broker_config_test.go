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
			Version:   cubridVersion,
			Databases: []databasev1alpha1.CubridDatabase{{Name: "demodb"}},
			Topology:  databasev1alpha1.CubridTopology{PromotableMembers: 3},
		},
	}
}

// One Broker per configuration: a Pod serves one access mode.
func TestGenerateBrokerConf_OneAccessModeEach(t *testing.T) {
	tests := map[string]struct{ want, not []string }{
		brokerModeRW: {
			want: []string{"[%RW]", "ACCESS_MODE             =RW", "BROKER_PORT             =33000"},
			not:  []string{"[%RO]", "=RO"},
		},
		brokerModeRO: {
			want: []string{"[%RO]", "ACCESS_MODE             =RO", "BROKER_PORT             =33001"},
			not:  []string{"[%RW]", "=RW"},
		},
	}
	for mode, tc := range tests {
		conf := generateBrokerConf(mode)
		for _, want := range tc.want {
			if !strings.Contains(conf, want) {
				t.Errorf("%s broker conf missing %q\n%s", mode, want, conf)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(conf, not) {
				t.Errorf("%s broker conf contains %q\n%s", mode, not, conf)
			}
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
		r.setBrokerConditions(c, PrimaryResolution{CurrentPrimary: "cc-0", Status: metav1.ConditionTrue, Reason: singlePrimary},
			map[string]int32{brokerModeRW: 1})
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionRoutingReady)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("RoutingReady = %+v, want True", cond)
		}
	})

	t.Run("resolved primary without a read-write Broker -> RoutingReady False", func(t *testing.T) {
		c := brokerTestCluster()
		r.setBrokerConditions(c, PrimaryResolution{CurrentPrimary: "cc-0", Status: metav1.ConditionTrue, Reason: singlePrimary},
			map[string]int32{brokerModeRO: 2})
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionRoutingReady)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "NoBrokerAvailable" {
			t.Fatalf("RoutingReady = %+v, want False/NoBrokerAvailable", cond)
		}
	})

	t.Run("ambiguous primary -> RoutingReady False (never claim write-endpoint safety)", func(t *testing.T) {
		c := brokerTestCluster()
		r.setBrokerConditions(c, PrimaryResolution{Status: metav1.ConditionFalse, Reason: "MultiplePrimariesObserved"}, nil)
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
		r.setBrokerConditions(c, PrimaryResolution{Status: metav1.ConditionFalse, Reason: "NoPrimaryObserved"}, nil)
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionRoutingReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("RoutingReady = %+v, want False", cond)
		}
	})
}

// A Broker reads its configuration once, when its container starts. The hash
// of what a mode's Brokers read is part of their Pod template, so that a
// change of it replaces them and nothing else does (#304).
func TestBrokerConfigHash(t *testing.T) {
	data := func() map[string]string {
		return map[string]string{
			brokerConfKey(brokerModeRW): "rw settings",
			brokerConfKey(brokerModeRO): "ro settings",
			brokerDatabasesKey:          "appdb hosts",
		}
	}
	rw, ro := brokerConfigHash(data(), brokerModeRW), brokerConfigHash(data(), brokerModeRO)
	if rw == "" || rw == ro {
		t.Fatalf("hashes of the two modes: %q and %q, want two different ones", rw, ro)
	}
	if again := brokerConfigHash(data(), brokerModeRW); again != rw {
		t.Errorf("the same configuration hashed to %q and %q", rw, again)
	}

	changed := data()
	changed[brokerConfKey(brokerModeRO)] = "other ro settings"
	if got := brokerConfigHash(changed, brokerModeRW); got != rw {
		t.Error("a change of the read-only settings changed the hash of the read-write Brokers, which do not read them")
	}
	if got := brokerConfigHash(changed, brokerModeRO); got == ro {
		t.Error("a change of the read-only settings did not change the hash of the read-only Brokers")
	}

	hosts := data()
	hosts[brokerDatabasesKey] = "appdb other hosts"
	if brokerConfigHash(hosts, brokerModeRW) == rw || brokerConfigHash(hosts, brokerModeRO) == ro {
		t.Error("a change of databases.txt, which every Broker reads, did not change both hashes")
	}

	// Moving text from one file to the other is a change.
	moved := map[string]string{brokerConfKey(brokerModeRW): "rw settingsappdb", brokerDatabasesKey: " hosts"}
	if brokerConfigHash(moved, brokerModeRW) == rw {
		t.Error("two different configurations have the same hash")
	}
}
