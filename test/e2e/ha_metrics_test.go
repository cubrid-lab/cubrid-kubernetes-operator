//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// The HA steps check that the gauges of docs/observability.md, section
// "Metrics", follow the CubridCluster status they are taken from (#253). One
// Pod in the manager's namespace reads the metrics endpoint the way the
// metrics check of e2e_test.go does, as the manager's ServiceAccount, which
// the ClusterRoleBinding metricsRoleBindingName lets read it. The Pod stays
// for the rest of the suite and every check runs curl in it, so a check costs
// one kubectl exec and two reads of the cluster.

// haMetricsPod is the name of that Pod.
const haMetricsPod = "ha-metrics-client"

var (
	haMetricsOnce sync.Once
	haMetricsErr  error
)

// startHAMetricsClient creates the Pod once, and the ClusterRoleBinding when
// the metrics check did not, as when a label filter left it out.
func startHAMetricsClient() error {
	haMetricsOnce.Do(func() {
		if _, err := utils.Run(exec.Command("kubectl", "get", "clusterrolebinding", metricsRoleBindingName)); err != nil {
			if _, err := utils.Run(exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=cubrid-kubernetes-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName))); err != nil {
				haMetricsErr = err
				return
			}
		}
		// The container only waits; it ignores SIGTERM, so it is ended at
		// once when the namespace is deleted.
		if _, err := utils.Run(exec.Command("kubectl", "run", haMetricsPod, "--restart=Never",
			"--namespace", namespace, "--image=curlimages/curl:latest", "--overrides", fmt.Sprintf(`{
				"spec": {
					"terminationGracePeriodSeconds": 0,
					"automountServiceAccountToken": true,
					"serviceAccountName": %q,
					"containers": [{
						"name": "curl",
						"image": "curlimages/curl:latest",
						"command": ["sleep", "86400"],
						"securityContext": {
							"readOnlyRootFilesystem": true,
							"allowPrivilegeEscalation": false,
							"capabilities": {"drop": ["ALL"]},
							"runAsNonRoot": true,
							"runAsUser": 1000,
							"seccompProfile": {"type": "RuntimeDefault"}
						}
					}]
				}
			}`, serviceAccountName))); err != nil {
			haMetricsErr = err
			return
		}
		_, haMetricsErr = utils.Run(exec.Command("kubectl", "-n", namespace, "wait", "--for=condition=Ready",
			"pod/"+haMetricsPod, "--timeout=2m"))
	})
	return haMetricsErr
}

// scrapeManagerMetrics returns the body of one answer of the metrics
// endpoint. The token is the Pod's projected one, which the kubelet renews.
func scrapeManagerMetrics(ctx context.Context) (string, error) {
	return utils.RunQuiet(exec.CommandContext(ctx, "kubectl", "-n", namespace, "exec", haMetricsPod, "--",
		"sh", "-c", fmt.Sprintf(`curl -sS --fail -k --max-time 10 `+
			`-H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" `+
			`https://%s.%s.svc.cluster.local:8443/metrics`, metricsServiceName, namespace)))
}

// clusterGauges are the gauges the comparison knows. instances_ready is not
// in the status (docs/observability.md), so it is not compared.
var clusterGauges = []string{
	"cubrid_cluster_ready", "cubrid_cluster_instances", "cubrid_cluster_ha_ready",
	"cubrid_cluster_primary_resolved", "cubrid_cluster_routing_ready", "cubrid_cluster_replication_healthy",
	"cubrid_cluster_last_observation_timestamp_seconds",
	"cubrid_instance_ready", "cubrid_instance_role", "cubrid_instance_replication_observation_available",
	"cubrid_instance_replication_delayed_pages", "cubrid_instance_replication_stalled",
	"cubrid_instance_last_observation_timestamp_seconds",
}

// seriesKey names a series of one cluster by its metric and its labels other
// than namespace and cluster, given as name and value pairs in label order:
// cubrid_instance_role{member="ha-0",role="master"}.
func seriesKey(name string, labels ...string) string {
	if len(labels) == 0 {
		return name
	}
	pairs := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		pairs = append(pairs, fmt.Sprintf("%s=%q", labels[i], labels[i+1]))
	}
	return name + "{" + strings.Join(pairs, ",") + "}"
}

func memberSeries(name, member string) string { return seriesKey(name, "member", member) }

func roleSeries(member, role string) string {
	return seriesKey("cubrid_instance_role", "member", member, "role", role)
}

// clusterSamples returns the samples of the compared gauges of one cluster
// in a body of the Prometheus text format, by seriesKey. A body that is not
// that format, or that carries no metric at all, is an error: an answer that
// could not be read is never taken for one without samples.
func clusterSamples(text, namespace, cluster string) (map[string]float64, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		return nil, fmt.Errorf("the metrics endpoint's answer is not the text format: %w", err)
	}
	if len(families) == 0 {
		return nil, errors.New("the metrics endpoint's answer carries no metric")
	}
	samples := map[string]float64{}
	for _, name := range clusterGauges {
		family, ok := families[name]
		if !ok {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["namespace"] != namespace || labels["cluster"] != cluster {
				continue
			}
			var rest []string
			for _, k := range slices.Sorted(maps.Keys(labels)) {
				if k != "namespace" && k != "cluster" {
					rest = append(rest, k, labels[k])
				}
			}
			samples[seriesKey(name, rest...)] = m.GetGauge().GetValue()
		}
	}
	return samples, nil
}

// expectation is what docs/observability.md says a series is for a status:
// a value, or no sample, and why.
type expectation struct {
	present bool
	value   float64
	because string
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// expectedSamples derives every compared series of a cluster from its
// status, by the table of docs/observability.md, section "Metrics". It is
// written from that table and not from the manager's code, so that the two
// can disagree.
func expectedSamples(c *databasev1alpha1.CubridCluster) map[string]expectation {
	want := map[string]expectation{}
	condition := func(gauge, conditionType string, unknownHasNoSample bool) {
		cond := metav1.Condition{}
		for _, x := range c.Status.Conditions {
			if x.Type == conditionType {
				cond = x
			}
		}
		switch {
		case cond.Type == "":
			want[gauge] = expectation{because: "the status has no " + conditionType + " Condition"}
		case unknownHasNoSample && cond.Status == metav1.ConditionUnknown:
			want[gauge] = expectation{because: conditionType + " is Unknown"}
		default:
			want[gauge] = expectation{present: true, value: boolValue(cond.Status == metav1.ConditionTrue),
				because: conditionType + " is " + string(cond.Status)}
		}
	}
	condition("cubrid_cluster_ready", "Ready", false)
	condition("cubrid_cluster_ha_ready", "HAReady", false)
	condition("cubrid_cluster_primary_resolved", "PrimaryResolved", false)
	condition("cubrid_cluster_routing_ready", "RoutingReady", false)
	condition("cubrid_cluster_replication_healthy", "ReplicationHealthy", true)
	want["cubrid_cluster_instances"] = expectation{present: true,
		value: float64(c.Spec.Topology.PromotableMembers), because: "spec.topology.promotableMembers"}
	observedAt := func(key, field string, at *metav1.Time) {
		if at == nil || at.IsZero() {
			want[key] = expectation{because: field + " is not set"}
			return
		}
		want[key] = expectation{present: true, value: float64(at.Unix()),
			because: field + " is " + at.UTC().Format(time.RFC3339)}
	}
	observedAt("cubrid_cluster_last_observation_timestamp_seconds", "status.lastObservedTime", c.Status.LastObservedTime)

	ha := c.Spec.HighAvailability.Enabled
	for _, in := range c.Status.Instances {
		of := "status.instances[" + in.Name + "]"
		want[memberSeries("cubrid_instance_ready", in.Name)] = expectation{present: true, value: boolValue(in.Ready),
			because: fmt.Sprintf("%s.ready is %t", of, in.Ready)}
		role := string(in.Role)
		if role == "" {
			role = string(databasev1alpha1.RoleUnknown)
		}
		for _, r := range []string{"master", "slave", "replica", "unknown"} {
			if ha {
				want[roleSeries(in.Name, r)] = expectation{present: true, value: boolValue(r == role),
					because: of + ".role is " + role}
			} else {
				want[roleSeries(in.Name, r)] = expectation{because: "a standalone member has no HA role"}
			}
		}
		lastObserved := in.LastObservedTime
		if !ha {
			lastObserved = nil
		}
		observedAt(memberSeries("cubrid_instance_last_observation_timestamp_seconds", in.Name),
			of+".lastObservedTime", lastObserved)

		reading := in.Replication
		available := memberSeries("cubrid_instance_replication_observation_available", in.Name)
		delayed := memberSeries("cubrid_instance_replication_delayed_pages", in.Name)
		stalled := memberSeries("cubrid_instance_replication_stalled", in.Name)
		switch {
		case reading == nil && in.Role != databasev1alpha1.RoleSlave:
			want[available] = expectation{because: of + " is no slave and has no replication reading"}
		default:
			want[available] = expectation{present: true, value: boolValue(reading != nil),
				because: fmt.Sprintf("%s.replication is set: %t", of, reading != nil)}
		}
		if reading == nil {
			want[delayed] = expectation{because: of + ".replication is not set"}
			want[stalled] = expectation{because: of + ".replication is not set"}
			continue
		}
		want[delayed] = expectation{present: true, value: float64(reading.DelayedPages),
			because: fmt.Sprintf("%s.replication.delayedPages is %d", of, reading.DelayedPages)}
		if reading.StalledSince != nil {
			want[stalled] = expectation{present: true, value: 1, because: of + ".replication.stalledSince is set"}
		} else {
			want[stalled] = expectation{present: true, value: 0, because: of + ".replication.stalledSince is not set"}
		}
	}
	return want
}

// metricsDisagreements returns every compared series whose sample is not
// what the status implies: a different value, a sample where none is
// expected, or none where one is. A missing sample is never read as 0.
func metricsDisagreements(c *databasev1alpha1.CubridCluster, samples map[string]float64) []string {
	want := expectedSamples(c)
	var out []string
	for _, key := range slices.Sorted(maps.Keys(want)) {
		w := want[key]
		got, ok := samples[key]
		switch {
		case w.present && !ok:
			out = append(out, fmt.Sprintf("%s has no sample, expected %g: %s", key, w.value, w.because))
		case w.present && got != w.value:
			out = append(out, fmt.Sprintf("%s is %g, expected %g: %s", key, got, w.value, w.because))
		case !w.present && ok:
			out = append(out, fmt.Sprintf("%s is %g, expected no sample: %s", key, got, w.because))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(samples)) {
		if _, known := want[key]; !known {
			out = append(out, fmt.Sprintf("%s is %g, expected no sample: the status does not name the member",
				key, samples[key]))
		}
	}
	return out
}

// sampleIs reports whether a series has a sample of the value.
func sampleIs(samples map[string]float64, key string, value float64) error {
	got, ok := samples[key]
	switch {
	case !ok:
		return fmt.Errorf("%s has no sample, expected %g", key, value)
	case got != value:
		return fmt.Errorf("%s is %g, expected %g", key, got, value)
	}
	return nil
}

// conditionIs reports whether the status holds the Condition with the status.
func conditionIs(c *databasev1alpha1.CubridCluster, conditionType string, status metav1.ConditionStatus) error {
	for _, x := range c.Status.Conditions {
		if x.Type == conditionType {
			if x.Status != status {
				return fmt.Errorf("%s is %s (%s), expected %s", conditionType, x.Status, x.Reason, status)
			}
			return nil
		}
	}
	return fmt.Errorf("the status has no %s Condition, expected %s", conditionType, status)
}

// metricsFollowStatus waits, for at most a minute, for one scrape of the
// manager's metrics taken between two reads of the cluster that return the
// same resourceVersion, in which every compared gauge agrees with that status
// and want accepts both. The manager sets the gauges from the status it
// wrote, so a scrape between two equal reads mirrors that status. It fails
// the step with the last disagreement, naming the gauge's value and the
// status field or Condition it should mirror, and stores the agreeing
// samples under file in the evidence.
func (r *haRun) metricsFollowStatus(file string,
	want func(*databasev1alpha1.CubridCluster, map[string]float64) error) {
	Expect(startHAMetricsClient()).To(Succeed(), "starting the Pod that reads the manager's metrics")
	read := func() (*databasev1alpha1.CubridCluster, error) {
		out, err := r.kubectlQuiet(context.Background(), "get", "cubridcluster", r.cluster, "-o", "json")
		if err != nil {
			return nil, err
		}
		c := &databasev1alpha1.CubridCluster{}
		return c, json.Unmarshal([]byte(out), c)
	}
	var last string
	var agreed map[string]float64
	var version string
	Eventually(func() error {
		before, err := read()
		if err != nil {
			return err
		}
		body, err := scrapeManagerMetrics(context.Background())
		if err != nil {
			return err
		}
		after, err := read()
		if err != nil {
			return err
		}
		if before.ResourceVersion != after.ResourceVersion {
			return fmt.Errorf("the status was written during the scrape (resourceVersion %s, then %s)",
				before.ResourceVersion, after.ResourceVersion)
		}
		samples, err := clusterSamples(body, r.namespace, r.cluster)
		if err != nil {
			return err
		}
		if d := metricsDisagreements(after, samples); len(d) > 0 {
			last = fmt.Sprintf("the manager's metrics disagree with the status of %s (resourceVersion %s):\n  %s",
				r.cluster, after.ResourceVersion, strings.Join(d, "\n  "))
			return errors.New(last)
		}
		if err := want(after, samples); err != nil {
			last = fmt.Sprintf("the metrics agree with the status of %s (resourceVersion %s), "+
				"but not in the state this step expects:\n  %v", r.cluster, after.ResourceVersion, err)
			return errors.New(last)
		}
		agreed, version = samples, after.ResourceVersion
		return nil
	}, time.Minute, 2*time.Second).Should(Succeed(), func() string { return last })
	lines := make([]string, 0, len(agreed)+1)
	lines = append(lines, fmt.Sprintf("read at %s, status resourceVersion %s",
		time.Now().UTC().Format(time.RFC3339), version))
	for _, key := range slices.Sorted(maps.Keys(agreed)) {
		lines = append(lines, fmt.Sprintf("%s %g", key, agreed[key]))
	}
	writeScenarioFile(file, []byte(strings.Join(lines, "\n")+"\n"))
}

// afterFailover accepts a status and its metrics in which newMaster is the
// resolved primary and exported as master, and the former master, when it is
// another member, is no longer exported as master.
func afterFailover(formerMaster, newMaster string) func(*databasev1alpha1.CubridCluster, map[string]float64) error {
	return func(c *databasev1alpha1.CubridCluster, samples map[string]float64) error {
		errs := []error{
			conditionIs(c, "PrimaryResolved", metav1.ConditionTrue),
			sampleIs(samples, "cubrid_cluster_primary_resolved", 1),
			sampleIs(samples, roleSeries(newMaster, "master"), 1),
		}
		if c.Status.CurrentPrimary != newMaster {
			errs = append(errs, fmt.Errorf("status.currentPrimary is %q, expected %s", c.Status.CurrentPrimary, newMaster))
		}
		if formerMaster != newMaster {
			errs = append(errs, sampleIs(samples, roleSeries(formerMaster, "master"), 0))
		}
		return errors.Join(errs...)
	}
}
