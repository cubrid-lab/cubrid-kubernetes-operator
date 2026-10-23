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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

type fakeMetrics struct {
	queries []string
	series  map[string][]Series
	err     error
}

func (f *fakeMetrics) Range(_ context.Context, selector string, window time.Duration, _ time.Time) ([]Series, error) {
	f.queries = append(f.queries, selector+"/"+window.String())
	if f.err != nil {
		return nil, f.err
	}
	return f.series[selector], nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := databasev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func observedCluster() *databasev1alpha1.CubridCluster {
	observed := metav1.NewTime(at(-5))
	return &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "ha", Namespace: "db"},
		Status: databasev1alpha1.CubridClusterStatus{
			CurrentPrimary:   "ha-1",
			LastObservedTime: &observed,
			Instances: []databasev1alpha1.InstanceStatus{
				{Name: "ha-0", Role: "slave", Ready: true, LastObservedTime: &observed},
				{Name: "ha-1", Role: "master", Ready: true, LastObservedTime: &observed},
			},
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ClusterReady",
				LastTransitionTime: metav1.NewTime(at(-60))}},
		},
	}
}

func managerPod(name string, ready bool, deleting bool) *corev1.Pod {
	started := metav1.NewTime(at(-30))
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "op", UID: types.UID("uid-" + name),
			Labels: map[string]string{"control-plane": "controller-manager"}},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "manager", RestartCount: 2,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}}}},
		},
	}
	if ready {
		p.Status.Conditions[0].Status = corev1.ConditionTrue
	}
	if deleting {
		now := metav1.NewTime(at(-1))
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"test/keep"}
	}
	return p
}

func event(name, object string, count int32, rv string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "db", UID: types.UID("e-" + name), ResourceVersion: rv},
		InvolvedObject: corev1.ObjectReference{Kind: "CubridCluster", Name: object},
		Type:           corev1.EventTypeNormal, Reason: "Observed", Message: "m-" + name, Count: count,
		LastTimestamp: metav1.NewTime(at(-2)),
	}
}

func newCollector(c client.Client, m MetricsSource) *Collector {
	return &Collector{
		Client: c, Metrics: m, Namespace: "db", Cluster: "ha",
		OperatorNamespace: "op", OperatorLabels: map[string]string{"control-plane": "controller-manager"},
		UpSelector: `up{namespace="op"}`, Window: 90 * time.Second,
		Now: func() time.Time { return t0 },
	}
}

func TestCollectReadsStatusManagerPodsEventsAndMetrics(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		observedCluster(), managerPod("mgr-a", false, true), managerPod("mgr-b", true, false),
		event("e1", "ha", 1, "1"), event("other", "other-cluster", 1, "1"),
	).Build()
	m := &fakeMetrics{series: map[string][]Series{
		`{__name__=~"cubrid_(cluster|instance)_.+",namespace="db",cluster="ha"}`: {gaugeSeries("cubrid_cluster_ready", 1, -10)},
		`up{namespace="op"}`: {upSeries(1, -10)},
	}}
	col := newCollector(c, m)
	s := col.Collect(context.Background())

	if !s.At.Equal(t0) {
		t.Errorf("At = %v, want %v", s.At, t0)
	}
	st := s.Status
	if !st.Read || st.Error != "" || st.CurrentPrimary != "ha-1" || st.LastObservedTime == nil || !st.LastObservedTime.Equal(at(-5)) {
		t.Errorf("status = %+v, want the cluster's status", st)
	}
	if got := st.Conditions["Ready"]; got.Status != "True" || got.Reason != "ClusterReady" || !got.LastTransitionTime.Equal(at(-60)) {
		t.Errorf("Ready = %+v, want True/ClusterReady since -60s", got)
	}
	if len(st.Instances) != 2 || st.Instances[1].Role != "master" || !st.Instances[1].Ready {
		t.Errorf("instances = %+v, want both members with their roles", st.Instances)
	}
	if len(s.Operator.Pods) != 2 {
		t.Fatalf("operator pods = %+v, want both", s.Operator.Pods)
	}
	for _, p := range s.Operator.Pods {
		want := p.Name == "mgr-b"
		if p.Ready != want || p.Deleting == want || p.Restarts != 2 || p.StartedAt == nil || p.UID == "" || p.Node != "node-1" {
			t.Errorf("pod = %+v, want ready=%v deleting=%v with its container start and restarts", p, want, !want)
		}
	}
	if len(s.Events) != 1 || s.Events[0].Message != "m-e1" {
		t.Errorf("events = %+v, want only the cluster's event", s.Events)
	}
	if !s.Metrics.Queried || s.Metrics.Error != "" || len(s.Metrics.Series) != 1 || len(s.Metrics.Up) != 1 {
		t.Errorf("metrics = %+v, want the cluster's series and up", s.Metrics)
	}
	if want := []string{
		`{__name__=~"cubrid_(cluster|instance)_.+",namespace="db",cluster="ha"}/1m30s`, `up{namespace="op"}/1m30s`,
	}; strings.Join(m.queries, " ") != strings.Join(want, " ") {
		t.Errorf("queries = %q, want %q", m.queries, want)
	}
}

func TestCollectRecordsAnEventOnlyWhenItIsNewOrChanged(t *testing.T) {
	ev := event("e1", "ha", 1, "1")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(observedCluster(), ev).Build()
	col := newCollector(c, &fakeMetrics{})
	if got := col.Collect(context.Background()).Events; len(got) != 1 {
		t.Fatalf("first tick events = %+v, want one", got)
	}
	if got := col.Collect(context.Background()).Events; len(got) != 0 {
		t.Fatalf("second tick events = %+v, want none for an unchanged event", got)
	}
	ev.Count = 2
	if err := c.Update(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if got := col.Collect(context.Background()).Events; len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("third tick events = %+v, want the repeated event again", got)
	}
}

func TestCollectKeepsReadingWhenOneSourceFails(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build() // no cluster
	col := newCollector(c, &fakeMetrics{err: errors.New("connection refused")})
	s := col.Collect(context.Background())
	if s.Status.Read || !strings.Contains(s.Status.Error, "not found") {
		t.Errorf("status = %+v, want a read error naming the missing cluster", s.Status)
	}
	if s.Metrics.Queried || !strings.Contains(s.Metrics.Error, "connection refused") {
		t.Errorf("metrics = %+v, want the query error, not an empty reading", s.Metrics)
	}
	if s.Operator.Error != "" || s.EventsError != "" {
		t.Errorf("operator = %+v, events error = %q, want the other reads to go on", s.Operator, s.EventsError)
	}
}

func TestRunWritesARedactedJudgedLinePerTickUntilTheDeadline(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(observedCluster()).Build()
	col := newCollector(c, &fakeMetrics{err: errors.New("dial 10.0.0.9:9090: refused")})
	now := t0
	col.Now = func() time.Time { return now }
	col.sleep = func(ctx context.Context, d time.Duration) error { now = now.Add(d); return nil }
	var out bytes.Buffer
	samples, err := Run(context.Background(), col, 2*time.Second, 5*time.Second, &out, evidence.NewRedactor("10.0.0.9"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(samples) != 3 || len(lines) != 3 {
		t.Fatalf("Run() = %d samples, %d lines; want 3 of each (t=0, 2, 4)", len(samples), len(lines))
	}
	if strings.Contains(out.String(), "10.0.0.9") {
		t.Errorf("output holds an address the redactor was given: %s", out.String())
	}
	var first Sample
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.Verdict.State != NotHealthy || len(first.Verdict.Reasons) == 0 {
		t.Errorf("verdict = %+v, want a judged sample", first.Verdict)
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(observedCluster()).Build()
	col := newCollector(c, &fakeMetrics{})
	ctx, cancel := context.WithCancel(context.Background())
	col.sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	var out bytes.Buffer
	samples, err := Run(ctx, col, time.Second, time.Hour, &out, evidence.NewRedactor())
	if !errors.Is(err, context.Canceled) || len(samples) != 1 {
		t.Fatalf("Run() = %d samples, %v; want the one sample taken and context.Canceled", len(samples), err)
	}
}
