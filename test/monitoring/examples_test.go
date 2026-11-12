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
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

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
	if rule.APIVersion != "monitoring.coreos.com/v1" || rule.Kind != "PrometheusRule" {
		t.Errorf("apiVersion/kind = %q/%q, want monitoring.coreos.com/v1/PrometheusRule", rule.APIVersion, rule.Kind)
	}
	if len(rule.Spec.Groups) == 0 {
		t.Error("no alert groups found")
	}
	seen := map[string]bool{}
	for _, g := range rule.Spec.Groups {
		if g.Name == "" || len(g.Rules) == 0 {
			t.Errorf("alert group %q must have a name and at least one rule", g.Name)
		}
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
			if duration, err := time.ParseDuration(r.For); err != nil || duration <= 0 {
				t.Errorf("alert %s has an invalid or nonpositive for duration %q: %v", r.Alert, r.For, err)
			}
			seen[r.Alert] = true
		}
	}
	if len(seen) == 0 {
		t.Error("no alert found")
	}
}
// The metric-name regex above deliberately inspects raw text so a removed
// metric cannot escape checks. Also parse the dashboard structure: invalid
// JSON, duplicate panel IDs, or an empty query must not reach users as a
// supposedly importable Grafana example.
func TestDashboardIsWellFormed(t *testing.T) {
	var dashboard struct {
		Title  string `json:"title"`
		Panels []struct {
			ID      int    `json:"id"`
			Title   string `json:"title"`
			Targets []struct {
				RefID string `json:"refId"`
				Expr  string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(read(t, "cubrid-dashboard.json"), &dashboard); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}
	if dashboard.Title == "" || len(dashboard.Panels) == 0 {
		t.Fatal("dashboard needs a title and panels")
	}
	seen := map[int]bool{}
	for _, panel := range dashboard.Panels {
		if panel.ID <= 0 || seen[panel.ID] {
			t.Errorf("panel %q has a missing or duplicate positive ID %d", panel.Title, panel.ID)
		}
		seen[panel.ID] = true
		if panel.Title == "" || len(panel.Targets) == 0 {
			t.Errorf("panel %d needs a title and at least one query", panel.ID)
		}
		refs := map[string]bool{}
		for _, target := range panel.Targets {
			if target.RefID == "" || refs[target.RefID] || target.Expr == "" {
				t.Errorf("panel %d has a missing/duplicate query refId or empty PromQL expression", panel.ID)
			}
			refs[target.RefID] = true
		}
	}
}
