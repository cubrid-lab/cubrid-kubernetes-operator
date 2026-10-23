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

package observer

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func at(seconds float64) time.Time { return t0.Add(time.Duration(seconds * float64(time.Second))) }

// healthyStatus is a status read at the given time whose observation is
// observedAgo old and whose five Conditions are True since t0.
func healthyStatus(read float64, observedAgo float64) StatusReading {
	conds := map[string]Condition{}
	for _, c := range HealthConditions {
		conds[c] = Condition{Status: "True", LastTransitionTime: t0.Add(-time.Hour)}
	}
	return StatusReading{Read: true, LastObservedTime: new(at(read - observedAgo)), Conditions: conds}
}

func gaugeSeries(name string, value float64, sampled ...float64) Series {
	return podGauge("manager-a", name, value, sampled...)
}

// podGauge is a gauge as the manager Pod pod exported it.
func podGauge(pod, name string, value float64, sampled ...float64) Series {
	s := Series{Name: name, Labels: map[string]string{"namespace": "ns", "cluster": "ha", "pod": pod}}
	for _, ts := range sampled {
		s.Samples = append(s.Samples, Point{At: at(ts), Value: value})
	}
	return s
}

func upSeries(value float64, sampled ...float64) Series { return podUp("manager-a", value, sampled...) }

func podUp(pod string, value float64, sampled ...float64) Series {
	s := podGauge(pod, "up", value, sampled...)
	s.Labels = map[string]string{"namespace": "op", "pod": pod}
	return s
}

// healthyMetrics holds every mirrored gauge as 1, sampled at the given times,
// and a manager target that was scraped successfully then.
func healthyMetrics(sampled ...float64) MetricsReading {
	m := MetricsReading{Queried: true, Up: []Series{upSeries(1, sampled...)}}
	for _, name := range conditionGauges {
		m.Series = append(m.Series, gaugeSeries(name, 1, sampled...))
	}
	return m
}

func TestJudgeHealthyOnlyWithFreshStatusAndMeasuredMetrics(t *testing.T) {
	rules := DefaultRules()
	healthy := Sample{At: at(100), Status: healthyStatus(100, 10), Metrics: healthyMetrics(80)}
	if v := Judge(healthy, rules); v.State != Healthy {
		t.Fatalf("Judge(fresh status, measured metrics) = %+v, want healthy", v)
	}

	cases := map[string]struct {
		mutate func(*Sample)
		want   string
	}{
		"status older than the reader's 60 s": {func(s *Sample) { s.Status = healthyStatus(100, 61) }, "status stale"},
		"status never observed": {func(s *Sample) { s.Status.LastObservedTime = nil },
			"status has no lastObservedTime"},
		"status read failed": {func(s *Sample) { s.Status = StatusReading{Error: "timeout"} },
			"status unavailable"},
		"a Condition is False": {func(s *Sample) {
			c := s.Status.Conditions["RoutingReady"]
			c.Status = "False"
			s.Status.Conditions["RoutingReady"] = c
		}, "RoutingReady is False"},
		"a Condition is missing": {func(s *Sample) { delete(s.Status.Conditions, "PrimaryResolved") },
			"PrimaryResolved is missing"},
		"Prometheus could not be queried": {func(s *Sample) {
			s.Metrics = MetricsReading{Error: "connection refused"}
		}, "metrics unavailable"},
		"a mirrored gauge has no sample": {func(s *Sample) {
			s.Metrics.Series = s.Metrics.Series[1:]
		}, conditionGauges[0] + " absent"},
		"a mirrored gauge was last scraped too long ago": {func(s *Sample) {
			s.Metrics = healthyMetrics(20)
		}, "stale"},
		"a mirrored gauge reads 0": {func(s *Sample) {
			s.Metrics.Series[0] = gaugeSeries(conditionGauges[0], 0, 80)
		}, conditionGauges[0] + " is 0"},
		"the manager's last scrape failed": {func(s *Sample) {
			s.Metrics.Up = []Series{upSeries(1, 50), upSeries(0, 80)}
		}, "last scrape of the manager failed"},
		"the manager was never scraped": {func(s *Sample) { s.Metrics.Up = nil }, "up absent"},
		"a replaced manager's last scrape has no gauge yet": {func(s *Sample) {
			// The old Pod's gauges are recent, but the manager scraped last
			// is a new Pod that has not written the status yet.
			s.Metrics.Up = append(s.Metrics.Up, podUp("manager-b", 1, 90))
		}, conditionGauges[0] + " absent from the last scrape"},
		"a gauge is missing from the last scrape only": {func(s *Sample) {
			s.Metrics = healthyMetrics(70, 80)
			s.Metrics.Series[0] = gaugeSeries(conditionGauges[0], 1, 70)
		}, conditionGauges[0] + " absent from the last scrape"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := Sample{At: at(100), Status: healthyStatus(100, 10), Metrics: healthyMetrics(80)}
			tc.mutate(&s)
			v := Judge(s, rules)
			if v.State == Healthy {
				t.Fatalf("Judge() = healthy, want not healthy (%s)", tc.want)
			}
			if !strings.Contains(strings.Join(v.Reasons, "; "), tc.want) {
				t.Fatalf("Judge() reasons = %q, want one containing %q", v.Reasons, tc.want)
			}
		})
	}
}

func TestClassifyMetricNeverReadsAMissingValueAsZero(t *testing.T) {
	rules := DefaultRules()
	now := at(100)
	if got := ClassifyLatest(nil, now, rules); got.State != Absent || got.Value != nil {
		t.Fatalf("ClassifyLatest(no series) = %+v, want absent without a value", got)
	}
	empty := Series{Name: "cubrid_cluster_ready"}
	if got := ClassifyLatest(&empty, now, rules); got.State != Absent || got.Value != nil {
		t.Fatalf("ClassifyLatest(series without samples) = %+v, want absent without a value", got)
	}
	old := gaugeSeries("cubrid_cluster_ready", 1, 10)
	if got := ClassifyLatest(&old, now, rules); got.State != Stale {
		t.Fatalf("ClassifyLatest(sample 90 s old) = %+v, want stale", got)
	}
	zero := gaugeSeries("cubrid_cluster_ready", 0, 90)
	got := ClassifyLatest(&zero, now, rules)
	if got.State != Measured || got.Value == nil || *got.Value != 0 {
		t.Fatalf("ClassifyLatest(a measured 0) = %+v, want measured 0", got)
	}
}

func TestCheckFindsAMetricThatContradictsAStableCondition(t *testing.T) {
	rules := DefaultRules()
	before := Sample{At: at(10), Status: healthyStatus(10, 5)}
	after := Sample{At: at(40), Status: healthyStatus(40, 5)}
	scraped := Sample{At: at(40), Metrics: MetricsReading{Queried: true,
		Series: []Series{gaugeSeries("cubrid_cluster_routing_ready", 0, 25)}}}
	got := Check([]Sample{before, scraped, after}, rules)
	if len(got) != 1 || !strings.Contains(got[0], "cubrid_cluster_routing_ready") {
		t.Fatalf("Check() = %q, want one contradiction for cubrid_cluster_routing_ready", got)
	}
}

func TestCheckDoesNotJudgeAMetricScrapedAcrossATransition(t *testing.T) {
	rules := DefaultRules()
	before := Sample{At: at(10), Status: healthyStatus(10, 5)}
	after := Sample{At: at(40), Status: healthyStatus(40, 5)}
	c := after.Status.Conditions["RoutingReady"]
	c.Status, c.LastTransitionTime = "False", at(30)
	after.Status.Conditions["RoutingReady"] = c
	// Scraped while the Condition was still True or after it changed: the
	// observer cannot tell which status the gauge mirrors.
	scraped := Sample{At: at(40), Metrics: MetricsReading{Queried: true,
		Series: []Series{gaugeSeries("cubrid_cluster_routing_ready", 0, 25)}}}
	if got := Check([]Sample{before, scraped, after}, rules); len(got) != 0 {
		t.Fatalf("Check() = %q, want none across a transition", got)
	}
	// A gauge scraped just after the Condition's last change may come from the
	// write before it.
	recent := healthyStatus(10, 5)
	c = recent.Conditions["RoutingReady"]
	c.LastTransitionTime = at(24)
	recent.Conditions["RoutingReady"] = c
	afterRecent := healthyStatus(40, 5)
	afterRecent.Conditions["RoutingReady"] = c
	window := []Sample{{At: at(10), Status: recent}, scraped, {At: at(40), Status: afterRecent}}
	if got := Check(window, rules); len(got) != 0 {
		t.Fatalf("Check() = %q, want none within the tolerance of the last change", got)
	}
}

func TestCheckNeedsAStatusReadOnEachSideOfTheScrape(t *testing.T) {
	rules := DefaultRules()
	scraped := Sample{At: at(40), Metrics: MetricsReading{Queried: true,
		Series: []Series{gaugeSeries("cubrid_cluster_routing_ready", 0, 25)}}}
	failed := Sample{At: at(40), Status: StatusReading{Error: "timeout"}}
	if got := Check([]Sample{{At: at(10), Status: healthyStatus(10, 5)}, scraped, failed}, rules); len(got) != 0 {
		t.Fatalf("Check() = %q, want none without a status read after the scrape", got)
	}
}

func TestCheckReportsAReplicationGaugeWhileItsConditionIsUnknown(t *testing.T) {
	rules := DefaultRules()
	before := Sample{At: at(10), Status: healthyStatus(10, 5)}
	after := Sample{At: at(40), Status: healthyStatus(40, 5)}
	for _, s := range []*Sample{&before, &after} {
		s.Status.Conditions["ReplicationHealthy"] = Condition{Status: "Unknown", LastTransitionTime: t0.Add(-time.Hour)}
	}
	scraped := Sample{At: at(40), Metrics: MetricsReading{Queried: true,
		Series: []Series{gaugeSeries("cubrid_cluster_replication_healthy", 0, 25)}}}
	got := Check([]Sample{before, scraped, after}, rules)
	if len(got) != 1 || !strings.Contains(got[0], "Unknown") {
		t.Fatalf("Check() = %q, want one contradiction for a sample while Unknown", got)
	}
}

func TestCheckFindsAnObservationTimeNewerThanTheStatusHolds(t *testing.T) {
	rules := DefaultRules()
	after := Sample{At: at(40), Status: healthyStatus(40, 20)} // holds at(20)
	ok := gaugeSeries("cubrid_cluster_last_observation_timestamp_seconds", float64(at(20).Unix()), 25)
	held := Sample{Metrics: MetricsReading{Queried: true, Series: []Series{ok}}}
	if got := Check([]Sample{held, after}, rules); len(got) != 0 {
		t.Fatalf("Check() = %q, want none for the time the status holds", got)
	}
	ahead := gaugeSeries("cubrid_cluster_last_observation_timestamp_seconds", float64(at(30).Unix()), 25)
	got := Check([]Sample{{Metrics: MetricsReading{Queried: true, Series: []Series{ahead}}}, after}, rules)
	if len(got) != 1 || !strings.Contains(got[0], "lastObservedTime") {
		t.Fatalf("Check() = %q, want one contradiction for a newer observation time", got)
	}
}

func TestCheckCountsEachScrapeOnce(t *testing.T) {
	rules := DefaultRules()
	before := Sample{At: at(10), Status: healthyStatus(10, 5)}
	after := Sample{At: at(40), Status: healthyStatus(40, 5)}
	wrong := gaugeSeries("cubrid_cluster_routing_ready", 0, 25)
	// Two ticks return the same raw sample in their windows.
	first := Sample{At: at(30), Metrics: MetricsReading{Queried: true, Series: []Series{wrong}}}
	second := Sample{At: at(35), Metrics: MetricsReading{Queried: true, Series: []Series{wrong}}}
	if got := Check([]Sample{before, first, second, after}, rules); len(got) != 1 {
		t.Fatalf("Check() = %q, want the one scrape reported once", got)
	}
}

func TestSummarizeIsIncompleteWithoutAHealthyObservation(t *testing.T) {
	rules := DefaultRules()
	stale := Sample{At: at(100), Status: healthyStatus(100, 90), Metrics: healthyMetrics(80)}
	s := Summarize([]Sample{stale}, rules)
	if s.Verdict != VerdictIncomplete {
		t.Fatalf("Summarize(only stale) verdict = %q, want %q", s.Verdict, VerdictIncomplete)
	}
	if s.Counts["not-healthy"] != 1 {
		t.Fatalf("Summarize counts = %v, want one not-healthy tick", s.Counts)
	}
	if got := Summarize(nil, rules); got.Verdict != VerdictIncomplete {
		t.Fatalf("Summarize(no samples) verdict = %q, want %q", got.Verdict, VerdictIncomplete)
	}
	healthy := Sample{At: at(100), Status: healthyStatus(100, 10), Metrics: healthyMetrics(80)}
	if got := Summarize([]Sample{healthy}, rules); got.Verdict != VerdictPassed {
		t.Fatalf("Summarize(one healthy) verdict = %q, want %q", got.Verdict, VerdictPassed)
	}
}

func TestSummarizeFailsOnAContradiction(t *testing.T) {
	rules := DefaultRules()
	healthy := Sample{At: at(10), Status: healthyStatus(10, 5), Metrics: healthyMetrics(8)}
	after := Sample{At: at(40), Status: healthyStatus(40, 5)}
	wrong := Sample{At: at(40), Metrics: MetricsReading{Queried: true,
		Series: []Series{gaugeSeries("cubrid_cluster_routing_ready", 0, 25)}}}
	s := Summarize([]Sample{healthy, wrong, after}, rules)
	if s.Verdict != VerdictFailed || len(s.Contradictions) != 1 {
		t.Fatalf("Summarize() = %+v, want failed with one contradiction", s)
	}
}
