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

// Package monitoring checks the example alert rules and dashboard in
// docs/examples/monitoring against the metrics the Operator exports. It does
// not run the expressions: promtool and a live Prometheus are not used.
package monitoring

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/yaml"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/metrics"
)

const examplesDir = "../../docs/examples/monitoring"

var (
	metricName = regexp.MustCompile(`\bcubrid_[a-z0-9_]+`)
	fqName     = regexp.MustCompile(`fqName: "([^"]+)"`)
	roleValue  = regexp.MustCompile(`role="([^"]*)"`)
)

// exportedMetrics returns the names of the metrics internal/metrics registers.
func exportedMetrics(t *testing.T) []string {
	t.Helper()
	vecs := []*prometheus.GaugeVec{
		metrics.ClusterReady, metrics.ClusterInstances, metrics.InstanceReady,
		metrics.ClusterHAReady, metrics.ClusterPrimaryResolved, metrics.ClusterRoutingReady,
		metrics.ClusterReplicationHealthy, metrics.ClusterLastObservation,
		metrics.MemberReady, metrics.MemberRole, metrics.MemberReplicationAvailable,
		metrics.MemberDelayedPages, metrics.MemberStalled, metrics.MemberLastObservation,
	}
	var names []string
	for _, vec := range vecs {
		ch := make(chan *prometheus.Desc, 1)
		vec.Describe(ch)
		close(ch)
		for d := range ch {
			m := fqName.FindStringSubmatch(d.String())
			if m == nil {
				t.Fatalf("cannot read the metric name from %q", d.String())
			}
			names = append(names, m[1])
		}
	}
	return names
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(examplesDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A renamed or removed metric must fail here, not silently stop an alert.
func TestExamplesUseOnlyExportedMetrics(t *testing.T) {
	exported := exportedMetrics(t)
	for _, file := range []string{"cubrid-alerts.yaml", "cubrid-dashboard.json"} {
		raw := read(t, file)
		found := metricName.FindAll(raw, -1)
		if len(found) == 0 {
			t.Errorf("%s names no cubrid_ metric", file)
		}
		for _, n := range found {
			if !slices.Contains(exported, string(n)) {
				t.Errorf("%s uses %s, which internal/metrics does not export", file, n)
			}
		}
		for _, m := range roleValue.FindAllSubmatch(raw, -1) {
			if !slices.Contains(metrics.Roles, string(m[1])) {
				t.Errorf("%s selects role=%q, which is not one of %v", file, m[1], metrics.Roles)
			}
		}
	}
}

func TestAlertRulesAreWellFormed(t *testing.T) {
	var rule struct {
		APIVersion string         `json:"apiVersion"`
		Kind       string         `json:"kind"`
		Metadata   map[string]any `json:"metadata"`
		Spec       struct {
			Groups []struct {
				Name  string `json:"name"`
				Rules []struct {
					Alert       string            `json:"alert"`
					Expr        string            `json:"expr"`
					For         string            `json:"for"`
					Labels      map[string]string `json:"labels"`
					Annotations map[string]string `json:"annotations"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"spec"`
	}
	if err := yaml.UnmarshalStrict(read(t, "cubrid-alerts.yaml"), &rule); err != nil {
		t.Fatal(err)
	}
	if rule.Kind != "PrometheusRule" {
		t.Errorf("kind = %q, want PrometheusRule", rule.Kind)
	}
	seen := map[string]bool{}
	for _, g := range rule.Spec.Groups {
		for _, r := range g.Rules {
			switch {
			case r.Alert == "" || r.Expr == "":
				t.Errorf("rule %+v needs an alert name and an expression", r)
			case seen[r.Alert]:
				t.Errorf("alert %s is defined twice", r.Alert)
			case r.For == "":
				t.Errorf("alert %s has no for: it would fire on one observation", r.Alert)
			case r.Labels["severity"] == "" || r.Annotations["summary"] == "":
				t.Errorf("alert %s needs a severity label and a summary", r.Alert)
			}
			seen[r.Alert] = true
		}
	}
	if len(seen) == 0 {
		t.Error("no alert found")
	}
}
