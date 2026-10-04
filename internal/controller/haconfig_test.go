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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

func haTestCluster(name string) *databasev1alpha1.CubridCluster {
	return &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: databasev1alpha1.CubridClusterSpec{
			Version:          cubridVersion,
			Databases:        []databasev1alpha1.CubridDatabase{{Name: "demodb"}},
			Topology:         databasev1alpha1.CubridTopology{PromotableMembers: 3},
			HighAvailability: databasev1alpha1.CubridHighAvailability{Enabled: true},
		},
	}
}

func TestGenerateHAConf_ShortNamesInOrdinalOrder(t *testing.T) {
	conf, err := generateHAConf(haTestCluster("production"))
	if err != nil {
		t.Fatalf("generateHAConf: %v", err)
	}
	want := "[common]\n" +
		"ha_node_list=cubrid@production-0:production-1:production-2\n" +
		"ha_db_list=demodb\n" +
		"ha_port_id=59901\n"
	if conf != want {
		t.Errorf("cubrid_ha.conf =\n%s\nwant\n%s", conf, want)
	}
}

// ADR-0004: cubrid_ha.conf carries short names only, never an FQDN, the
// namespace or a cluster domain.
func TestGenerateHAConf_NoFQDN(t *testing.T) {
	conf, err := generateHAConf(haTestCluster("production"))
	if err != nil {
		t.Fatalf("generateHAConf: %v", err)
	}
	for _, forbidden := range []string{".", "svc", "cluster.local", "production-instances"} {
		if strings.Contains(conf, forbidden) {
			t.Errorf("cubrid_ha.conf contains %q:\n%s", forbidden, conf)
		}
	}
}

// Every member reads the same file: nothing in it depends on which member is
// asked, on the observed roles, or on the current primary (ADR-0001: ordinal
// 0 is not a permanent master).
func TestGenerateHAConf_IndependentOfRuntimeRoles(t *testing.T) {
	base := haTestCluster("production")
	want, err := generateHAConf(base)
	if err != nil {
		t.Fatalf("generateHAConf: %v", err)
	}
	for _, primary := range []string{"production-0", "production-1", "production-2"} {
		cluster := haTestCluster("production")
		cluster.Status.CurrentPrimary = primary
		got, err := generateHAConf(cluster)
		if err != nil {
			t.Fatalf("generateHAConf(primary=%s): %v", primary, err)
		}
		if got != want {
			t.Errorf("primary=%s changed cubrid_ha.conf:\n%s\nwant\n%s", primary, got, want)
		}
	}
	for _, key := range []string{"ha_mode", "master", "slave"} {
		if strings.Contains(want, key) {
			t.Errorf("cubrid_ha.conf names a role (%q):\n%s", key, want)
		}
	}
}

func TestGenerateHAConf_DatabasesInSpecOrder(t *testing.T) {
	cluster := haTestCluster("production")
	cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: "zeta"}, {Name: "alpha"}}
	conf, err := generateHAConf(cluster)
	if err != nil {
		t.Fatalf("generateHAConf: %v", err)
	}
	if !strings.Contains(conf, "ha_db_list=zeta,alpha\n") {
		t.Errorf("ha_db_list does not keep the spec order:\n%s", conf)
	}
}

func TestGenerateHAConf_Rejects(t *testing.T) {
	const notDNSLabel = "DNS label"
	tests := []struct {
		name   string
		mutate func(*databasev1alpha1.CubridCluster)
		want   string
	}{
		{"HA disabled", func(c *databasev1alpha1.CubridCluster) {
			c.Spec.HighAvailability.Enabled = false
		}, "highAvailability.enabled"},
		{"no database", func(c *databasev1alpha1.CubridCluster) {
			c.Spec.Databases = nil
		}, "databases"},
		{"no member", func(c *databasev1alpha1.CubridCluster) {
			c.Spec.Topology.PromotableMembers = 0
		}, "promotableMembers"},
		// <name>-<ordinal> must stay a DNS label: it is the pod name, the OS
		// hostname and the per-member alias Service name (ADR-0004).
		{"member name over 63 characters", func(c *databasev1alpha1.CubridCluster) {
			c.Name = strings.Repeat("a", 62)
		}, notDNSLabel},
		{"upper-case member name", func(c *databasev1alpha1.CubridCluster) {
			c.Name = "Production"
		}, notDNSLabel},
		{"FQDN-shaped member name", func(c *databasev1alpha1.CubridCluster) {
			c.Name = "prod.example"
		}, notDNSLabel},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster := haTestCluster("production")
			tc.mutate(cluster)
			conf, err := generateHAConf(cluster)
			if err == nil {
				t.Fatalf("expected an error, got:\n%s", conf)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The longest accepted cluster name still yields 63-character member names.
func TestGenerateHAConf_LongestName(t *testing.T) {
	name := strings.Repeat("a", 61)
	conf, err := generateHAConf(haTestCluster(name))
	if err != nil {
		t.Fatalf("generateHAConf: %v", err)
	}
	if !strings.Contains(conf, "cubrid@"+name+"-0:"+name+"-1:"+name+"-2\n") {
		t.Errorf("unexpected ha_node_list:\n%s", conf)
	}
}
