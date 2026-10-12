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
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// eventSink keeps what a broadcaster sends to the API server, which is what
// is left after the broadcaster's own limit per object.
type eventSink struct {
	mu      sync.Mutex
	reasons map[string][]string
}

func (s *eventSink) Create(event *corev1.Event) (*corev1.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasons[event.InvolvedObject.Name] = append(s.reasons[event.InvolvedObject.Name], event.Reason)
	return event, nil
}

func (s *eventSink) Update(event *corev1.Event) (*corev1.Event, error) { return s.Create(event) }

func (s *eventSink) Patch(event *corev1.Event, _ []byte) (*corev1.Event, error) {
	return s.Create(event)
}

func (s *eventSink) recorded(object string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reasons[object]...)
}

func clusterReference(name string) *corev1.ObjectReference {
	return &corev1.ObjectReference{
		APIVersion: "database.cubrid.io/v1alpha1", Kind: "CubridCluster",
		Namespace: "team-a", Name: name, UID: types.UID(name + "-uid"),
	}
}

// recordInARow records count Events about one cluster through the
// Operator's broadcaster and returns the reasons of the ones that were sent
// on, in order. Every Event has its own reason, so that none is merged into
// another.
func recordInARow(t *testing.T, count int) []string {
	t.Helper()
	sink := &eventSink{reasons: map[string][]string{}}
	broadcaster := NewEventBroadcaster()
	defer broadcaster.Shutdown()
	broadcaster.StartRecordingToSink(sink)
	recorder := broadcaster.NewRecorder(runtime.NewScheme(), corev1.EventSource{Component: "cubridcluster-controller"})

	for i := range count {
		reason := fmt.Sprintf("Change%03d", i)
		recorder.Event(clusterReference("busy"), corev1.EventTypeNormal, reason, "change "+reason)
	}
	// Events are sent in the order they were recorded, and the allowance is
	// kept per object: this one arrives once the ones above were handled.
	recorder.Event(clusterReference("other"), corev1.EventTypeNormal, "Last", "the last one")
	deadline := time.Now().Add(10 * time.Second)
	for len(sink.recorded("other")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the Event of the other cluster was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return sink.recorded("busy")
}

// A cluster that goes through several failovers and a token rotation records
// more Events than client-go's default allowance of 25 per object, after
// which one Event in five minutes is let through and the others are dropped
// without a trace (#386).
func TestEventBroadcaster_RecordsABusyClustersEvents(t *testing.T) {
	const changes = 100
	got := recordInARow(t, changes)
	if len(got) != changes {
		t.Fatalf("%d of %d Events of one cluster were recorded", len(got), changes)
	}
	for i, reason := range got {
		if want := fmt.Sprintf("Change%03d", i); reason != want {
			t.Fatalf("Event %d is %s, want %s", i, reason, want)
		}
	}
}

// The allowance is still bounded, so that a cluster whose state keeps
// changing cannot flood the API server: what exceeds it is dropped.
func TestEventBroadcaster_KeepsALimitPerCluster(t *testing.T) {
	got := recordInARow(t, eventBurst+10)
	if len(got) != eventBurst {
		t.Fatalf("%d Events of one cluster were recorded in a row, want the allowance of %d", len(got), eventBurst)
	}
}
