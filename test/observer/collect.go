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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// MetricsSource returns the raw samples of the series a selector matches.
type MetricsSource interface {
	Range(ctx context.Context, selector string, window time.Duration, at time.Time) ([]Series, error)
}

// readTimeout bounds each read of a tick, so that one source that does not
// answer delays the others by at most this much.
const readTimeout = 10 * time.Second

// Collector reads one CubridCluster, the manager's Pods, Events and
// Prometheus, through clients that only read.
type Collector struct {
	Client  client.Client
	Metrics MetricsSource
	// Namespace and Cluster name the CubridCluster.
	Namespace, Cluster string
	// OperatorNamespace and OperatorLabels select the manager's Pods.
	OperatorNamespace string
	OperatorLabels    map[string]string
	// UpSelector selects Prometheus' up series of the manager's target.
	UpSelector string
	// Window is how far back each query reads raw samples.
	Window time.Duration
	Rules  Rules
	Now    func() time.Time

	sleep func(context.Context, time.Duration) error
	// seen holds the resourceVersion of each Event already recorded.
	seen map[string]string
}

// ClusterSelector selects every series the manager exports for the cluster.
func (c *Collector) ClusterSelector() string {
	return fmt.Sprintf(`{__name__=~"cubrid_(cluster|instance)_.+",namespace=%q,cluster=%q}`, c.Namespace, c.Cluster)
}

func (c *Collector) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now()
}

// Collect takes one sample. A source that cannot be read is recorded with
// its error, and the others are still read.
func (c *Collector) Collect(ctx context.Context) Sample {
	s := Sample{At: c.now()}
	s.Status = c.readStatus(ctx)
	s.Operator = c.readOperator(ctx)
	s.Events, s.EventsError = c.readEvents(ctx)
	s.Metrics = c.readMetrics(ctx, s.At)
	return s
}

func (c *Collector) readStatus(ctx context.Context) StatusReading {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var cluster databasev1alpha1.CubridCluster
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Cluster}, &cluster); err != nil {
		return StatusReading{Error: err.Error()}
	}
	st := cluster.Status
	r := StatusReading{Read: true, CurrentPrimary: st.CurrentPrimary, LastObservedTime: timeOf(st.LastObservedTime),
		Conditions: map[string]Condition{}}
	for _, cond := range st.Conditions {
		r.Conditions[cond.Type] = Condition{Status: string(cond.Status), Reason: cond.Reason,
			LastTransitionTime: cond.LastTransitionTime.UTC()}
	}
	for _, in := range st.Instances {
		r.Instances = append(r.Instances, Instance{Name: in.Name, Role: string(in.Role), Ready: in.Ready,
			LastObservedTime: timeOf(in.LastObservedTime)})
	}
	return r
}

func timeOf(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func (c *Collector) readOperator(ctx context.Context) OperatorReading {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var pods corev1.PodList
	if err := c.Client.List(ctx, &pods, client.InNamespace(c.OperatorNamespace),
		client.MatchingLabels(c.OperatorLabels)); err != nil {
		return OperatorReading{Error: err.Error()}
	}
	r := OperatorReading{Pods: []Pod{}}
	for _, p := range pods.Items {
		pod := Pod{Name: p.Name, UID: string(p.UID), Node: p.Spec.NodeName, Deleting: p.DeletionTimestamp != nil}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady {
				pod.Ready = cond.Status == corev1.ConditionTrue
			}
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != "manager" {
				continue
			}
			pod.Restarts = cs.RestartCount
			if cs.State.Running != nil {
				pod.StartedAt = timeOf(&cs.State.Running.StartedAt)
			}
		}
		r.Pods = append(r.Pods, pod)
	}
	return r
}

// readEvents returns the Events about the cluster, its Pods and the
// manager's Pods that are new or changed since the last tick.
func (c *Collector) readEvents(ctx context.Context) ([]Event, string) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	if c.seen == nil {
		c.seen = map[string]string{}
	}
	var out []Event
	var errs []string
	for _, ns := range []string{c.Namespace, c.OperatorNamespace} {
		var events corev1.EventList
		if err := c.Client.List(ctx, &events, client.InNamespace(ns)); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for _, e := range events.Items {
			if !c.about(ns, e.InvolvedObject) || c.seen[string(e.UID)] == e.ResourceVersion {
				continue
			}
			c.seen[string(e.UID)] = e.ResourceVersion
			last := e.LastTimestamp.Time
			if last.IsZero() {
				last = e.EventTime.Time
			}
			if last.IsZero() {
				last = e.CreationTimestamp.Time
			}
			out = append(out, Event{Object: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name, Type: e.Type,
				Reason: e.Reason, Message: e.Message, Count: e.Count, Last: last.UTC()})
		}
	}
	return out, strings.Join(errs, "; ")
}

func (c *Collector) about(ns string, obj corev1.ObjectReference) bool {
	if ns == c.OperatorNamespace && ns != c.Namespace {
		return true
	}
	switch obj.Kind {
	case "CubridCluster":
		return obj.Name == c.Cluster
	case "Pod":
		return strings.HasPrefix(obj.Name, c.Cluster+"-")
	}
	return false
}

func (c *Collector) readMetrics(ctx context.Context, at time.Time) MetricsReading {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	series, err := c.Metrics.Range(ctx, c.ClusterSelector(), c.Window, at)
	if err != nil {
		return MetricsReading{Error: err.Error()}
	}
	up, err := c.Metrics.Range(ctx, c.UpSelector, c.Window, at)
	if err != nil {
		return MetricsReading{Error: err.Error()}
	}
	return MetricsReading{Queried: true, Series: series, Up: up}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run takes a sample every interval, from now until duration has passed,
// judges it, and writes it to w as one redacted JSON line as soon as it is
// taken, so that what was observed survives a run that is stopped. It
// returns the samples taken, and the context's error when it was cancelled.
func Run(
	ctx context.Context, c *Collector, interval, duration time.Duration, w io.Writer, r *evidence.Redactor,
) ([]Sample, error) {
	rules := c.Rules
	if rules == (Rules{}) {
		rules = DefaultRules()
	}
	sleep := c.sleep
	if sleep == nil {
		sleep = sleepContext
	}
	start := c.now()
	var samples []Sample
	for n := 1; ; n++ {
		s := c.Collect(ctx)
		s.Verdict = Judge(s, rules)
		samples = append(samples, s)
		line, err := json.Marshal(s)
		if err != nil {
			return samples, err
		}
		if _, err := w.Write(append(r.Redact(line), '\n')); err != nil {
			return samples, err
		}
		next := start.Add(time.Duration(n) * interval)
		if next.Sub(start) > duration {
			return samples, nil
		}
		if err := sleep(ctx, next.Sub(c.now())); err != nil {
			return samples, err
		}
	}
}
