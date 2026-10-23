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

// Package observer records, from outside the cluster, what an installed
// Operator reports about one CubridCluster: its status, the manager Pods,
// Events and the Prometheus series the manager exports. It judges each
// reading by docs/observability.md and never reads a missing value as zero.
package observer

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// HealthConditions are the Conditions a healthy HA cluster has True.
var HealthConditions = []string{"Ready", "HAReady", "PrimaryResolved", "RoutingReady", "ReplicationHealthy"}

// conditionGauges mirror HealthConditions, in the same order.
var conditionGauges = []string{
	"cubrid_cluster_ready", "cubrid_cluster_ha_ready", "cubrid_cluster_primary_resolved",
	"cubrid_cluster_routing_ready", "cubrid_cluster_replication_healthy",
}

// The state of a metric reading.
const (
	Measured    = "measured"
	Stale       = "stale"
	Absent      = "absent"
	Unavailable = "unavailable"
)

// The state of a sample.
const (
	Healthy    = "healthy"
	NotHealthy = "not-healthy"
)

// Verdicts of a run.
const (
	VerdictPassed     = "passed"
	VerdictFailed     = "failed"
	VerdictIncomplete = "incomplete"
)

// Rules are the thresholds a reading is judged by.
type Rules struct {
	StatusMaxAge time.Duration `json:"statusMaxAge"`
	MetricMaxAge time.Duration `json:"metricMaxAge"`
	// Tolerance covers the clock difference between this host, Prometheus
	// and the manager.
	Tolerance time.Duration `json:"tolerance"`
	// ScrapeTimeout is how long after Prometheus stamps a scrape the manager
	// may still gather its gauges: Prometheus' scrape_timeout.
	ScrapeTimeout time.Duration `json:"scrapeTimeout"`
}

// DefaultRules returns the rules of docs/observability.md: the reader's
// 60 seconds for the status, and for a metric two of the lab's 30-second
// scrape intervals plus a margin. The scrape timeout is Prometheus' default.
func DefaultRules() Rules {
	return Rules{StatusMaxAge: 60 * time.Second, MetricMaxAge: 75 * time.Second, Tolerance: 5 * time.Second,
		ScrapeTimeout: 10 * time.Second}
}

// Point is one raw sample of a series, at the time Prometheus scraped it.
type Point struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// Series is one Prometheus series with its raw samples in the queried window.
type Series struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels,omitempty"`
	Samples []Point           `json:"samples"`
}

// MetricsReading is what one query of Prometheus returned.
type MetricsReading struct {
	Queried bool     `json:"queried"`
	Error   string   `json:"error,omitempty"`
	Up      []Series `json:"up,omitempty"`
	Series  []Series `json:"series,omitempty"`
	// Notes are what the source reported since the last tick, such as a
	// port-forward that ended or was started again.
	Notes []string `json:"notes,omitempty"`
}

// Condition is one status Condition as read.
type Condition struct {
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// Instance is one member as the status names it.
type Instance struct {
	Name             string     `json:"name"`
	Role             string     `json:"role,omitempty"`
	Ready            bool       `json:"ready"`
	LastObservedTime *time.Time `json:"lastObservedTime,omitempty"`
}

// StatusReading is the CubridCluster's status as read.
type StatusReading struct {
	Read  bool   `json:"read"`
	Error string `json:"error,omitempty"`
	// ReadAt is when the read was answered; the tick's At is when it began.
	ReadAt           *time.Time           `json:"readAt,omitempty"`
	LastObservedTime *time.Time           `json:"lastObservedTime,omitempty"`
	CurrentPrimary   string               `json:"currentPrimary,omitempty"`
	Conditions       map[string]Condition `json:"conditions,omitempty"`
	Instances        []Instance           `json:"instances,omitempty"`
}

// Sample is everything read in one tick.
type Sample struct {
	At          time.Time       `json:"at"`
	Status      StatusReading   `json:"status"`
	Metrics     MetricsReading  `json:"metrics"`
	Operator    OperatorReading `json:"operator"`
	Events      []Event         `json:"events,omitempty"`
	EventsError string          `json:"eventsError,omitempty"`
	Verdict     Verdict         `json:"verdict"`
}

// Pod is one manager Pod as read.
type Pod struct {
	Name      string     `json:"name"`
	UID       string     `json:"uid"`
	Node      string     `json:"node,omitempty"`
	Ready     bool       `json:"ready"`
	Deleting  bool       `json:"deleting"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	Restarts  int32      `json:"restarts"`
}

// OperatorReading is the manager's Pods as read.
type OperatorReading struct {
	Error string `json:"error,omitempty"`
	Pods  []Pod  `json:"pods"`
}

// Event is a Kubernetes Event about the cluster or one of its Pods, recorded
// in the tick that first saw it or saw it change.
type Event struct {
	Object  string    `json:"object"`
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Count   int32     `json:"count,omitempty"`
	Last    time.Time `json:"last"`
}

// Verdict is the judgement of one sample.
type Verdict struct {
	State   string   `json:"state"`
	Reasons []string `json:"reasons,omitempty"`
}

// Latest is the classification of a series' last sample.
type Latest struct {
	State string     `json:"state"`
	Value *float64   `json:"value,omitempty"`
	At    *time.Time `json:"at,omitempty"`
}

// ClassifyLatest classifies the last sample of s, judged at now. A series
// without a sample is absent and has no value: a missing value is never 0.
func ClassifyLatest(s *Series, now time.Time, rules Rules) Latest {
	if s == nil || len(s.Samples) == 0 {
		return Latest{State: Absent}
	}
	last := s.Samples[0]
	for _, p := range s.Samples[1:] {
		if p.At.After(last.At) {
			last = p
		}
	}
	l := Latest{State: Measured, Value: &last.Value, At: &last.At}
	if now.Sub(last.At) > rules.MetricMaxAge {
		l.State = Stale
	}
	return l
}

// latestOf returns the series whose last sample is the newest, nil when no
// series has a sample.
func latestOf(series []Series) *Series {
	var newest *Series
	var newestAt time.Time
	for i := range series {
		for _, p := range series[i].Samples {
			if newest == nil || p.At.After(newestAt) {
				newest, newestAt = &series[i], p.At
			}
		}
	}
	return newest
}

// answered is when a sample's status read was answered, or when its tick
// began for a sample recorded without that time.
func answered(s Sample) time.Time {
	if s.Status.ReadAt != nil {
		return *s.Status.ReadAt
	}
	return s.At
}

func ptrTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Judge returns whether a sample shows a healthy cluster: a status observed
// at most StatusMaxAge before the read was answered, and not ahead of it by
// more than Tolerance, with the five Conditions True; a manager whose last
// scrape succeeded within MetricMaxAge; and in that scrape the gauges that
// mirror the Conditions, each 1. Every other reading is not healthy, and the
// reasons say why: a stale status, a failed read and a missing series are
// never taken for a healthy one.
func Judge(s Sample, rules Rules) Verdict {
	var reasons []string
	st := s.Status
	switch {
	case st.Error != "":
		reasons = append(reasons, "status unavailable: "+st.Error)
	case !st.Read:
		reasons = append(reasons, "status not read")
	default:
		age := answered(s).Sub(ptrTime(st.LastObservedTime))
		switch {
		case st.LastObservedTime == nil:
			reasons = append(reasons, "status has no lastObservedTime")
		case age > rules.StatusMaxAge:
			reasons = append(reasons, fmt.Sprintf("status stale: lastObservedTime is %s old", age.Round(time.Second)))
		case age < -rules.Tolerance:
			reasons = append(reasons, fmt.Sprintf("status lastObservedTime is %s in the future", (-age).Round(time.Second)))
		}
		for _, c := range HealthConditions {
			cond, ok := st.Conditions[c]
			switch {
			case !ok:
				reasons = append(reasons, c+" is missing")
			case cond.Status != "True":
				reasons = append(reasons, c+" is "+cond.Status)
			}
		}
	}
	m := s.Metrics
	switch {
	case m.Error != "":
		reasons = append(reasons, "metrics unavailable: "+m.Error)
	case !m.Queried:
		reasons = append(reasons, "metrics not queried")
	default:
		target := latestOf(m.Up)
		up := ClassifyLatest(target, s.At, rules)
		switch {
		case up.State == Absent:
			reasons = append(reasons, "up absent: the manager target was not scraped in the window")
		case up.State == Stale:
			reasons = append(reasons, fmt.Sprintf("up stale: last scraped %s ago", s.At.Sub(*up.At).Round(time.Second)))
		case *up.Value != 1:
			reasons = append(reasons, "the last scrape of the manager failed (up is 0)")
		default:
			for _, g := range conditionGauges {
				reasons = append(reasons, judgeGauge(g, m.Series, target.Labels, *up.At)...)
			}
		}
	}
	if len(reasons) == 0 {
		return Verdict{State: Healthy}
	}
	return Verdict{State: NotHealthy, Reasons: reasons}
}

// targetLabels identify the scrape target a series came from.
var targetLabels = []string{"instance", "pod"}

// judgeGauge judges a gauge by the manager's last successful scrape, the one
// at scraped from the target with the labels target: Prometheus gives every
// sample of one scrape the scrape's time. A gauge that scrape did not carry
// is absent, even when an earlier scrape or another manager Pod exported it:
// a replaced manager exports nothing until it has written the status.
func judgeGauge(name string, series []Series, target map[string]string, scraped time.Time) []string {
	for _, s := range series {
		if s.Name != name || !sameTarget(s.Labels, target) {
			continue
		}
		for _, p := range s.Samples {
			if !p.At.Equal(scraped) {
				continue
			}
			if p.Value != 1 {
				return []string{fmt.Sprintf("%s is %g", name, p.Value)}
			}
			return nil
		}
	}
	from := target["pod"]
	if from == "" {
		from = target["instance"]
	}
	return []string{fmt.Sprintf("%s absent from the last scrape of the manager (%s at %s)",
		name, from, scraped.UTC().Format(time.RFC3339Nano))}
}

func sameTarget(labels, target map[string]string) bool {
	for _, k := range targetLabels {
		if v, ok := target[k]; ok && labels[k] != v {
			return false
		}
	}
	return true
}

// point is one raw sample of a named series.
type point struct {
	name, labels string
	Point
}

// rawPoints returns every raw sample of the samples' cluster series once,
// in the order they were first read: ticks whose windows overlap return the
// same scrape more than once.
func rawPoints(samples []Sample) []point {
	seen := map[string]bool{}
	var out []point
	for _, s := range samples {
		for _, series := range s.Metrics.Series {
			labels := labelString(series.Labels)
			for _, p := range series.Samples {
				key := series.Name + labels + p.At.Format(time.RFC3339Nano)
				if !seen[key] {
					seen[key] = true
					out = append(out, point{series.Name, labels, p})
				}
			}
		}
	}
	return out
}

func labelString(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%s=%q", k, labels[k])
	}
	b.WriteString("}")
	return b.String()
}

const lastObservationGauge = "cubrid_cluster_last_observation_timestamp_seconds"

// Check returns each contradiction between a metric and the status. The
// manager sets its gauges from the status it wrote, so a gauge scraped while
// its Condition held one value must mirror it. A scrape stamped at T is
// judged only between two status reads: one answered by T-Tolerance, and one
// begun after T+Tolerance+ScrapeTimeout, since the manager gathers the
// gauges after Prometheus stamps the scrape. Both reads must carry the
// Condition with the same status and the same lastTransitionTime, older than
// T by Tolerance: otherwise the gauge may mirror a write the observer did not
// see. The observation time a gauge carries must not be newer than the
// status read after it, since the time only moves forward.
func Check(samples []Sample, rules Rules) []string {
	r := statusReads{rules: rules}
	for _, s := range samples {
		if s.Status.Read && s.Status.Error == "" {
			r.reads = append(r.reads, s)
		}
	}
	contradictions := []string{}
	for _, p := range rawPoints(samples) {
		var found string
		if p.name == lastObservationGauge {
			found = r.checkObservationTime(p)
		} else if i := slices.Index(conditionGauges, p.name); i >= 0 {
			found = r.checkCondition(p, HealthConditions[i])
		}
		if found != "" {
			contradictions = append(contradictions, found)
		}
	}
	return contradictions
}

// statusReads are the successful status reads of a run.
type statusReads struct {
	rules Rules
	reads []Sample
}

// before is the last read answered by t-Tolerance.
func (r statusReads) before(t time.Time) *Sample {
	var found *Sample
	for i := range r.reads {
		if done := answered(r.reads[i]); !done.After(t.Add(-r.rules.Tolerance)) &&
			(found == nil || done.After(answered(*found))) {
			found = &r.reads[i]
		}
	}
	return found
}

// after is the first read begun at t+Tolerance+ScrapeTimeout or later.
func (r statusReads) after(t time.Time) *Sample {
	var found *Sample
	for i := range r.reads {
		if begun := r.reads[i].At; !begun.Before(t.Add(r.rules.Tolerance+r.rules.ScrapeTimeout)) &&
			(found == nil || begun.Before(found.At)) {
			found = &r.reads[i]
		}
	}
	return found
}

func (r statusReads) checkObservationTime(p point) string {
	a := r.after(p.At)
	if a == nil || a.Status.LastObservedTime == nil {
		return ""
	}
	if held := a.Status.LastObservedTime.Unix(); p.Value < float64(held+1) {
		return ""
	}
	return fmt.Sprintf("%s%s=%g scraped at %s is newer than the lastObservedTime %s the status held at %s",
		p.name, p.labels, p.Value, stamp(p.At), a.Status.LastObservedTime.UTC().Format(time.RFC3339), stamp(a.At))
}

func (r statusReads) checkCondition(p point, cond string) string {
	b, a := r.before(p.At), r.after(p.At)
	if b == nil || a == nil {
		return ""
	}
	cb, okb := b.Status.Conditions[cond]
	ca, oka := a.Status.Conditions[cond]
	scraped := fmt.Sprintf("%s%s=%g scraped at %s", p.name, p.labels, p.Value, stamp(p.At))
	reads := fmt.Sprintf("read at %s and %s", stamp(b.At), stamp(a.At))
	switch {
	case !okb && !oka:
		return fmt.Sprintf("%s, but the status carried no %s (%s)", scraped, cond, reads)
	case !okb || !oka || cb.Status != ca.Status || !cb.LastTransitionTime.Equal(ca.LastTransitionTime):
		return ""
	case cb.LastTransitionTime.After(p.At.Add(-r.rules.Tolerance)):
		return ""
	}
	since := cb.LastTransitionTime.UTC().Format(time.RFC3339)
	if cond == "ReplicationHealthy" && cb.Status == "Unknown" {
		return fmt.Sprintf("%s, but %s was Unknown since %s, which has no sample (%s)", scraped, cond, since, reads)
	}
	want := 0.0
	if cb.Status == "True" {
		want = 1
	}
	if p.Value == want {
		return ""
	}
	return fmt.Sprintf("%s, but %s was %s since %s (%s)", scraped, cond, cb.Status, since, reads)
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Summary is the result of a run.
type Summary struct {
	Verdict        string         `json:"verdict"`
	Reason         string         `json:"reason,omitempty"`
	Rules          Rules          `json:"rules"`
	Samples        int            `json:"samples"`
	Counts         map[string]int `json:"counts"`
	Contradictions []string       `json:"contradictions"`
}

// Summarize judges a run: failed on any contradiction, incomplete without a
// single healthy sample, since then nothing was shown to be observable, and
// passed otherwise. It does not judge how long the cluster was not healthy.
func Summarize(samples []Sample, rules Rules) Summary {
	s := Summary{Rules: rules, Samples: len(samples), Counts: map[string]int{Healthy: 0, NotHealthy: 0}}
	for _, sample := range samples {
		s.Counts[Judge(sample, rules).State]++
	}
	s.Contradictions = Check(samples, rules)
	switch {
	case len(s.Contradictions) > 0:
		s.Verdict = VerdictFailed
		s.Reason = fmt.Sprintf("%d contradictions between metrics and status", len(s.Contradictions))
	case s.Counts[Healthy] == 0:
		s.Verdict, s.Reason = VerdictIncomplete, "no sample showed a fresh status and measured metrics that agree"
	default:
		s.Verdict = VerdictPassed
	}
	return s
}
