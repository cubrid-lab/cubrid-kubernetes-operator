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

package evidence

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StateSchemaVersion is the version of a snapshot's index.json.
const StateSchemaVersion = "1"

// StateFaultConfirmed is the checkpoint taken once a fault was confirmed. The
// others are LogsBeforeFault and LogsAfterVerdict.
const StateFaultConfirmed = "fault-confirmed"

// StateObsolete says that a source answered with nothing current: what it
// holds is older than the scenario, or behind the object's generation.
const StateObsolete LogStatus = "obsolete"

// The sources of a snapshot.
const (
	StateCluster         = "cubridcluster"
	StatePods            = "pods"
	StateOperatorPods    = "operator-pods"
	StateServices        = "services"
	StateEndpointSlices  = "endpointslices"
	StateEvents          = "events"
	StateOperatorMetrics = "operator-metrics"
)

// The clock domains of a snapshot's observations.
const (
	ClockRunner   = "test-runner"
	ClockOperator = "operator"
	// clockNode and clockReporter start the domain of one node or reporter.
	clockNode     = "node/"
	clockReporter = "reporter/"
)

// StateLimits bound what a scenario's snapshots collect.
type StateLimits struct {
	// ReadBytes bounds what is read of one listing. A listing is parsed as a
	// whole, so a longer one is not kept at all and the index says so.
	ReadBytes int64 `json:"readBytes"`
	// SourceBytes bounds what is stored of one source in one checkpoint.
	SourceBytes int64 `json:"sourceBytes"`
	// BundleBytes bounds what a scenario stores, observations.jsonl included.
	BundleBytes int64 `json:"bundleBytes"`
	// ObservationBytes bounds observations.jsonl.
	ObservationBytes int64 `json:"observationBytes"`
	// Checkpoints bounds the checkpoints of a scenario.
	Checkpoints int `json:"checkpoints"`
	// SourceTimeout bounds the read of one source.
	SourceTimeout time.Duration `json:"sourceTimeoutNanoseconds"`
	// CheckpointTimeout bounds one checkpoint.
	CheckpointTimeout time.Duration `json:"checkpointTimeoutNanoseconds"`
}

// DefaultStateLimits are the limits of a run.
func DefaultStateLimits() StateLimits {
	return StateLimits{ReadBytes: 8 << 20, SourceBytes: 512 << 10, BundleBytes: 4 << 20, ObservationBytes: 512 << 10,
		Checkpoints: 3, SourceTimeout: 10 * time.Second, CheckpointTimeout: 20 * time.Second}
}

const (
	stateDirName          = "cluster"
	stateIndexName        = "index.json"
	stateObservationsName = "observations.jsonl"
	// stateOrdering says how far the times of a snapshot can be compared.
	stateOrdering = "Each time is in UTC by its own clock. The clocks were not compared, so times of different " +
		"clocks are not proven to be in the order the file shows, and an interval between them is not a measured " +
		"recovery time."
	// metricsNotRead says why no checkpoint holds the Operator's metrics.
	metricsNotRead = "not read: the Operator's metrics endpoint answers only a bearer token of an authorized " +
		"service account, which a read-only snapshot does not hold or create"
)

// StateList is one listing a snapshot reads.
type StateList struct {
	// Source names the listing in the snapshot.
	Source    string
	Namespace string
	// Resource is what kubectl get takes.
	Resource string
	// Selector and Field are label and field selectors, in kubectl's form.
	Selector string
	Field    string
	// Names are the objects that must exist; one that does not is named.
	Names []string
}

// StatePlan is what a snapshot reads, in order.
type StatePlan struct {
	Cluster   string
	Namespace string
	Lists     []StateList
}

// ClusterStatePlan is the plan of one cluster: the CubridCluster, its Pods, the
// Operator's Pods by label, so that a Pod that replaced another is found, the
// cluster's Services and EndpointSlices, and the Events of its namespace.
func ClusterStatePlan(operatorNamespace, namespace, cluster string, members []string) StatePlan {
	instance := "app.kubernetes.io/instance=" + cluster
	return StatePlan{Cluster: cluster, Namespace: namespace, Lists: []StateList{
		{Source: StateCluster, Namespace: namespace, Resource: "cubridclusters.database.cubrid.io",
			Field: "metadata.name=" + cluster, Names: []string{cluster}},
		{Source: StatePods, Namespace: namespace, Resource: "pods", Selector: instance, Names: members},
		{Source: StateOperatorPods, Namespace: operatorNamespace, Resource: "pods",
			Selector: "control-plane=controller-manager"},
		{Source: StateServices, Namespace: namespace, Resource: "services", Selector: instance},
		{Source: StateEndpointSlices, Namespace: namespace, Resource: "endpointslices.discovery.k8s.io",
			Selector: instance},
		{Source: StateEvents, Namespace: namespace, Resource: "events"},
	}}
}

// StateReader lists a cluster's objects for a snapshot. It writes the JSON of
// one Kubernetes list to w, and stops when ctx ends.
type StateReader interface {
	List(ctx context.Context, list StateList, w io.Writer) error
}

// StateObject identifies one object a source held.
type StateObject struct {
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
	Node string `json:"node,omitempty"`
}

// StateExcluded counts the Events of a listing that are no evidence of this
// checkpoint, by why.
type StateExcluded struct {
	// OlderThanScenario: last observed before the scenario began.
	OlderThanScenario int `json:"olderThanScenario"`
	// UnchangedSinceEarlierCheckpoint: already stored, with the same count.
	UnchangedSinceEarlierCheckpoint int `json:"unchangedSinceEarlierCheckpoint"`
	// Undated: no timestamp to tell when it was observed.
	Undated int `json:"undated"`
	// OtherCluster: about a former CubridCluster of the same name.
	OtherCluster int `json:"otherCluster"`
}

// StateSource is one entry of a snapshot's index: one source, read or not.
type StateSource struct {
	Order     int    `json:"order"`
	Source    string `json:"source"`
	Namespace string `json:"namespace,omitempty"`
	Selector  string `json:"selector,omitempty"`
	// File is relative to the run directory; empty when nothing was stored.
	File   string    `json:"file,omitempty"`
	Status LogStatus `json:"status"`
	Reason string    `json:"reason,omitempty"`
	// Listed is how many objects the listing returned, Stored how many the
	// file holds.
	Listed int `json:"listed"`
	Stored int `json:"stored"`
	// Objects identifies the objects stored, except for Events.
	Objects []StateObject `json:"objects,omitempty"`
	// Missing are expected objects that were not found.
	Missing []string `json:"missing,omitempty"`
	// Gone are objects an earlier checkpoint saw that are no longer there
	// under the same UID.
	Gone []StateObject `json:"gone,omitempty"`
	// Excluded is set for Events.
	Excluded   *StateExcluded `json:"excluded,omitempty"`
	StartedAt  time.Time      `json:"startedAt"`
	FinishedAt time.Time      `json:"finishedAt"`
}

// StateCheckpoint is one snapshot of a scenario.
type StateCheckpoint struct {
	Name       string        `json:"name"`
	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt time.Time     `json:"finishedAt"`
	Sources    []StateSource `json:"sources"`
}

// StateClock is one clock domain of the observations.
type StateClock struct {
	Domain string `json:"domain"`
	Source string `json:"source"`
	// Skew against another domain, or Unknown when it was not measured.
	Skew string `json:"skew"`
}

// StateObservation is one line of observations.jsonl.
type StateObservation struct {
	T      time.Time `json:"t"`
	Clock  string    `json:"clock"`
	Kind   string    `json:"kind"`
	Object string    `json:"object,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// StateIndex is the content of a snapshot's index.json.
type StateIndex struct {
	SchemaVersion string `json:"schemaVersion"`
	Scenario      string `json:"scenario"`
	Cluster       string `json:"cluster"`
	Namespace     string `json:"namespace"`
	// ClusterUID is the UID the last read of the CubridCluster returned.
	ClusterUID string `json:"clusterUID,omitempty"`
	// Since is when the scenario began, by the runner's clock.
	Since  *time.Time  `json:"since,omitempty"`
	Limits StateLimits `json:"limits"`
	// Clocks and Ordering say how far the times can be compared.
	Clocks   []StateClock `json:"clocks"`
	Ordering string       `json:"ordering"`
	// Observations is the merged file, relative to the run directory.
	Observations        string            `json:"observations,omitempty"`
	DroppedObservations int               `json:"droppedObservations"`
	Checkpoints         []StateCheckpoint `json:"checkpoints"`
	RefusedCheckpoints  int               `json:"refusedCheckpoints"`
}

// StateBundle collects one scenario's snapshots of a cluster into
// <scenario>/cluster/ of the run directory. It has the rules of LogBundle: it
// only lists, it copies only the fields of the types below, so that a Pod's
// environment or an annotation never reaches a file, everything is written
// through Redactor.WriteFile, and the index is rewritten after every source.
// Nothing here can fail the caller, and a problem is only reported by Problem.
//
// Checkpoint is for one goroutine at a time; Mark may be called from any.
type StateBundle struct {
	dir      string
	scenario string
	redactor *Redactor
	reader   StateReader
	plan     StatePlan
	limits   StateLimits
	budget   *LogBudget
	since    time.Time

	mu    sync.Mutex
	marks []StateObservation

	index        StateIndex
	stored       int64
	order        int
	seen         map[string]map[string]StateObject
	events       map[string]string
	observations map[StateObservation]bool
	written      bool
	problem      error
}

// NewStateBundle returns the snapshots of scenario ("S03/abrupt") below the run
// directory dir. since is when the scenario began: an Event last observed
// before it is not evidence of this scenario. A zero since excludes no Event by
// age, for a namespace the run created itself.
func NewStateBundle(dir, scenario string, r *Redactor, reader StateReader, plan StatePlan, limits StateLimits,
	budget *LogBudget, since time.Time) *StateBundle {
	index := StateIndex{SchemaVersion: StateSchemaVersion, Scenario: scenario, Cluster: plan.Cluster,
		Namespace: plan.Namespace, Limits: limits, Ordering: stateOrdering, Checkpoints: []StateCheckpoint{},
		Clocks: []StateClock{
			{Domain: ClockRunner, Source: "the machine that runs the suite, for checkpoints and fault steps",
				Skew: Unknown},
			{Domain: ClockOperator, Source: "the Operator's Pod, for the transition of a Condition", Skew: Unknown},
			{Domain: clockNode + "<node>", Source: "the kubelet of the node, for a container's start and end",
				Skew: Unknown},
			{Domain: clockReporter + "<component>", Source: "the component that reported an Event", Skew: Unknown},
		}}
	if !since.IsZero() {
		since = since.UTC()
		index.Since = &since
	}
	return &StateBundle{dir: dir, scenario: scenario, redactor: r, reader: reader, plan: plan, limits: limits,
		budget: budget, since: since, index: index, seen: map[string]map[string]StateObject{},
		events: map[string]string{}, observations: map[StateObservation]bool{}}
}

// IndexFile is the index's path relative to the run directory, or "" until
// one was written.
func (b *StateBundle) IndexFile() string {
	if !b.written {
		return ""
	}
	return b.rel(stateIndexName)
}

// Problem is the last reason a file of the snapshots could not be written.
func (b *StateBundle) Problem() error { return b.problem }

func (b *StateBundle) rel(name string) string { return b.scenario + "/" + stateDirName + "/" + name }

// Mark records a step of the scenario by the runner's clock, for the next
// checkpoint to store. It does no I/O, so it cannot delay the step.
func (b *StateBundle) Mark(kind, detail string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.marks = append(b.marks, StateObservation{T: time.Now().UTC(), Clock: ClockRunner, Kind: kind, Detail: detail})
}

// Checkpoint collects every source of the plan once. It takes at most the
// limits' CheckpointTimeout, plus logReadGrace for a read that does not stop,
// and what the run's budget still allows; what it did not read is named in the
// index. It runs in a deferred call of a scenario, also while a failed
// assertion panics: the recover below ends only a panic of its own.
func (b *StateBundle) Checkpoint(ctx context.Context, name string) {
	defer func() {
		if p := recover(); p != nil {
			b.problem = fmt.Errorf("the snapshot panicked: %v", p)
			b.writeIndex()
		}
	}()
	if len(b.index.Checkpoints) >= b.limits.Checkpoints {
		b.index.RefusedCheckpoints++
		b.writeIndex()
		return
	}
	start := time.Now().UTC()
	b.index.Checkpoints = append(b.index.Checkpoints, StateCheckpoint{Name: name, StartedAt: start,
		Sources: []StateSource{}})
	b.observe(StateObservation{T: start, Clock: ClockRunner, Kind: "checkpoint_started", Detail: name})
	_, wall := b.budget.left()
	ctx, cancel := context.WithTimeout(ctx, max(0, min(b.limits.CheckpointTimeout, wall)))
	defer cancel()
	for _, list := range b.plan.Lists {
		b.collect(ctx, list)
	}
	b.note(StateSource{Source: StateOperatorMetrics, Status: LogUnavailable, Reason: metricsNotRead})
	took := time.Since(start)
	b.budget.spendTime(took)
	finished := start.Add(took)
	b.checkpoint().FinishedAt = finished
	b.observe(StateObservation{T: finished, Clock: ClockRunner, Kind: "checkpoint_finished", Detail: name})
	b.writeObservations()
	b.writeIndex()
}

func (b *StateBundle) checkpoint() *StateCheckpoint {
	return &b.index.Checkpoints[len(b.index.Checkpoints)-1]
}

// room is how many bytes the next source may store.
func (b *StateBundle) room() int64 {
	run, _ := b.budget.left()
	return min(b.limits.SourceBytes, b.limits.BundleBytes-b.limits.ObservationBytes-b.stored, run)
}

// collect reads one listing and stores what is allowed of it.
func (b *StateBundle) collect(ctx context.Context, list StateList) {
	source := StateSource{Source: list.Source, Namespace: list.Namespace, StartedAt: time.Now().UTC(),
		Selector: cmp.Or(list.Selector, list.Field)}
	unavailable := func(reason string) {
		source.Status, source.Reason = LogUnavailable, reason
		source.Missing = list.Names
		b.note(source)
	}
	if err := collectionStopped(ctx); err != nil {
		unavailable(indexReason(b.redactor, err.Error()))
		return
	}
	room := b.room()
	if room <= 0 {
		unavailable("not read: the size bound of the scenario or of the run is used up")
		return
	}
	tail := NewLogTail(int(b.limits.ReadBytes))
	err := boundedRead(ctx, b.limits.SourceTimeout, func(ctx context.Context) error {
		return b.reader.List(ctx, list, tail)
	})
	tail.seal()
	switch {
	case err != nil:
		unavailable("the listing could not be read: " + indexReason(b.redactor, err.Error()))
		return
	case tail.Total() > b.limits.ReadBytes:
		unavailable(fmt.Sprintf("the listing of %d bytes is past the read bound of %d, and a part of it cannot be "+
			"parsed", tail.Total(), b.limits.ReadBytes))
		return
	}
	var listing struct {
		Items *[]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(tail.Bytes(), &listing); err != nil || listing.Items == nil {
		unavailable("the answer is not a Kubernetes list")
		return
	}
	found, err := b.parse(list, *listing.Items, &source)
	if err != nil {
		unavailable("the answer holds an object that could not be read: " + indexReason(b.redactor, err.Error()))
		return
	}
	source.Listed = len(*listing.Items)
	b.store(list, found, room, &source)
	b.note(source)
}

// parse turns a listing into the objects to store, oldest first for Events,
// and sets what they say about the source.
func (b *StateBundle) parse(list StateList, items []json.RawMessage, source *StateSource) ([]stateItem, error) {
	switch list.Source {
	case StateCluster:
		return parseItems(items, b.clusterItem(source))
	case StatePods, StateOperatorPods:
		return parseItems(items, b.podItem)
	case StateServices:
		return parseItems(items, func(s stateService) (stateItem, error) {
			return stateItem{object: StateObject{Name: s.Metadata.Name, UID: s.Metadata.UID}, value: s}, nil
		})
	case StateEndpointSlices:
		return parseItems(items, func(s stateEndpointSlice) (stateItem, error) {
			return stateItem{object: StateObject{Name: s.Metadata.Name, UID: s.Metadata.UID}, value: s}, nil
		})
	case StateEvents:
		return b.currentEvents(items, source)
	}
	return nil, fmt.Errorf("unknown source %q", list.Source)
}

// stateItem is one object of a source.
type stateItem struct {
	object StateObject
	value  any
	// observations are the times the object carries.
	observations []StateObservation
}

func parseItems[T any](items []json.RawMessage, convert func(T) (stateItem, error)) ([]stateItem, error) {
	out := make([]stateItem, 0, len(items))
	for _, raw := range items {
		var value T
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		item, err := convert(value)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// store keeps the newest objects that fit, whole, and names what is missing.
func (b *StateBundle) store(list StateList, items []stateItem, room int64, source *StateSource) {
	for _, item := range items {
		if item.object.Name != "" && list.Source != StateEvents {
			source.Objects = append(source.Objects, item.object)
		}
		b.observe(item.observations...)
	}
	for _, name := range list.Names {
		if !slices.ContainsFunc(items, func(i stateItem) bool { return i.object.Name == name }) {
			source.Missing = append(source.Missing, name)
		}
	}
	source.Gone = b.lost(list.Source, items)

	lines, kept := b.encode(items, room)
	source.Stored = kept
	var clean []byte
	if kept > 0 {
		var err error
		clean, err = json.Marshal(stateFile{Source: list.Source, Checkpoint: b.checkpoint().Name,
			CollectedAt: time.Now().UTC(), Clock: ClockRunner, Items: lines})
		if err != nil {
			source.Status, source.Reason = LogUnavailable, "the snapshot could not be encoded"
			return
		}
		rel := b.rel(b.checkpoint().Name + "/" + fileName(list.Source) + ".json")
		if err := b.redactor.WriteFile(b.dir, rel, append(clean, '\n')); err != nil {
			b.problem = err
			source.Status, source.Reason = LogUnavailable, "the file could not be written: "+
				indexReason(b.redactor, err.Error())
			return
		}
		source.File = rel
		b.stored += int64(len(clean))
		b.budget.spendBytes(int64(len(clean)))
	}
	switch {
	case source.Status != "":
		// parse found the source obsolete; it says why.
	case kept < len(items):
		source.Status = LogTruncated
		source.Reason = fmt.Sprintf("only the newest %d of %d objects fit the size bound of %d bytes", kept, len(items), room)
	case len(source.Missing) > 0:
		source.Status = LogTruncated
		source.Reason = "expected objects were not found: " + strings.Join(source.Missing, ", ")
	case len(items) == 0 && list.Source == StateEvents && *source.Excluded != (StateExcluded{}):
		source.Status = StateObsolete
		source.Reason = "the listing held Events, none of them observed during this scenario since the last checkpoint"
	case len(items) == 0:
		source.Status, source.Reason = LogEmpty, "the source answered and held nothing"
	default:
		source.Status = LogCollected
	}
	if list.Source == StateCluster && len(items) == 0 {
		source.Status, source.Reason = LogUnavailable, "the CubridCluster was not found"
	}
}

// encode redacts each object and returns the newest that fit in room.
func (b *StateBundle) encode(items []stateItem, room int64) ([]json.RawMessage, int) {
	var lines []json.RawMessage
	for _, item := range slices.Backward(items) {
		data, err := json.Marshal(item.value)
		if err != nil {
			break
		}
		clean := b.redactor.Redact(data)
		if room -= int64(len(clean)); room < 0 {
			break
		}
		lines = append(lines, clean)
	}
	slices.Reverse(lines)
	return lines, len(lines)
}

// lost returns the objects of a source an earlier checkpoint saw that are not
// among items under the same UID, once each, and remembers items.
func (b *StateBundle) lost(source string, items []stateItem) []StateObject {
	if source == StateEvents {
		return nil
	}
	seen := b.seen[source]
	if seen == nil {
		seen = map[string]StateObject{}
		b.seen[source] = seen
	}
	var gone []StateObject
	for _, uid := range slices.Sorted(maps.Keys(seen)) {
		if !slices.ContainsFunc(items, func(i stateItem) bool { return i.object.UID == uid }) {
			gone = append(gone, seen[uid])
			delete(seen, uid)
		}
	}
	for _, item := range items {
		if item.object.UID != "" {
			seen[item.object.UID] = item.object
		}
	}
	return gone
}

// clusterItem reads the CubridCluster: its UID and generation, and whether its
// status still describes that generation.
func (b *StateBundle) clusterItem(source *StateSource) func(stateCluster) (stateItem, error) {
	return func(c stateCluster) (stateItem, error) {
		b.index.ClusterUID = c.Metadata.UID
		var status struct {
			ObservedGeneration int64            `json:"observedGeneration"`
			Conditions         []stateCondition `json:"conditions"`
		}
		if len(c.Status) > 0 {
			if err := json.Unmarshal(c.Status, &status); err != nil {
				return stateItem{}, err
			}
		}
		object := "CubridCluster/" + c.Metadata.Name
		item := stateItem{object: StateObject{Name: c.Metadata.Name, UID: c.Metadata.UID}, value: c}
		var behind []string
		if status.ObservedGeneration < c.Metadata.Generation {
			behind = append(behind, "status.observedGeneration "+strconv.FormatInt(status.ObservedGeneration, 10))
		}
		for _, cond := range status.Conditions {
			if cond.LastTransitionTime != nil {
				item.observations = append(item.observations, StateObservation{T: cond.LastTransitionTime.UTC(),
					Clock: ClockOperator, Kind: "condition", Object: object,
					Detail: cond.Type + "=" + cond.Status + " " + cond.Reason})
			}
			if cond.ObservedGeneration != 0 && cond.ObservedGeneration < c.Metadata.Generation {
				behind = append(behind, cond.Type+" "+strconv.FormatInt(cond.ObservedGeneration, 10))
			}
		}
		if len(behind) > 0 {
			source.Status = StateObsolete
			source.Reason = fmt.Sprintf("the status describes an older specification than generation %d: %s",
				c.Metadata.Generation, strings.Join(behind, ", "))
		}
		return item, nil
	}
}

// podItem reads a Pod and the container times it carries.
func (b *StateBundle) podItem(p statePod) (stateItem, error) {
	item := stateItem{object: StateObject{Name: p.Metadata.Name, UID: p.Metadata.UID, Node: p.Spec.NodeName}, value: p}
	clock := clockNode + p.Spec.NodeName
	for _, c := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
		object := "Pod/" + p.Metadata.Name + "/" + c.Name
		for _, state := range []stateContainerState{c.LastState, c.State} {
			if state.Running != nil && state.Running.StartedAt != nil {
				item.observations = append(item.observations, StateObservation{T: state.Running.StartedAt.UTC(),
					Clock: clock, Kind: "container_running", Object: object, Detail: "uid " + p.Metadata.UID})
			}
			if t := state.Terminated; t != nil && t.FinishedAt != nil {
				item.observations = append(item.observations, StateObservation{T: t.FinishedAt.UTC(), Clock: clock,
					Kind: "container_terminated", Object: object,
					Detail: fmt.Sprintf("uid %s exit %d %s", p.Metadata.UID, t.ExitCode, t.Reason)})
			}
		}
	}
	return item, nil
}

// currentEvents keeps the Events observed during the scenario and not yet
// stored, oldest first, and counts the others. The last observation decides, so
// an aggregated Event that began earlier and repeated since is kept and marked,
// and one that only began earlier is not. A timestamp of an Event has a
// resolution of a second, hence the rounded since. The comparison with since
// crosses clocks; the comparison with an earlier checkpoint does not, because
// it only asks whether the Event changed.
func (b *StateBundle) currentEvents(items []json.RawMessage, source *StateSource) ([]stateItem, error) {
	excluded := &StateExcluded{}
	source.Excluded = excluded
	since := b.since.Truncate(time.Second)
	keep, err := parseItems(items, func(e stateEvent) (stateItem, error) {
		at := e.lastObserved()
		switch {
		case at.IsZero():
			excluded.Undated++
			return stateItem{}, nil
		case e.InvolvedObject.Kind == "CubridCluster" && e.InvolvedObject.Name == b.plan.Cluster &&
			b.index.ClusterUID != "" && e.InvolvedObject.UID != "" && e.InvolvedObject.UID != b.index.ClusterUID:
			excluded.OtherCluster++
			return stateItem{}, nil
		case at.Before(since):
			excluded.OlderThanScenario++
			return stateItem{}, nil
		}
		state := fmt.Sprintf("%d %s", e.times(), at.Format(time.RFC3339Nano))
		if b.events[e.Metadata.UID] == state {
			excluded.UnchangedSinceEarlierCheckpoint++
			return stateItem{}, nil
		}
		b.events[e.Metadata.UID] = state
		e.ObservedAt = &at
		first := e.firstObserved()
		e.BeganBeforeScenario = !first.IsZero() && first.Before(since)
		clock := clockReporter + cmp.Or(e.ReportingComponent, e.Source.Component, Unknown)
		object := e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
		return stateItem{object: StateObject{Name: e.Metadata.Name, UID: e.Metadata.UID}, value: e,
			observations: []StateObservation{{T: at, Clock: clock, Kind: "event", Object: object,
				Detail: e.Reason + ": " + e.Message}}}, nil
	})
	if err != nil {
		return nil, err
	}
	keep = slices.DeleteFunc(keep, func(i stateItem) bool { return i.value == nil })
	slices.SortStableFunc(keep, func(x, y stateItem) int {
		return x.value.(stateEvent).ObservedAt.Compare(*y.value.(stateEvent).ObservedAt)
	})
	return keep, nil
}

// observe adds timed observations, each once.
func (b *StateBundle) observe(lines ...StateObservation) {
	for _, line := range lines {
		line.T = line.T.UTC()
		line.Detail = indexReason(b.redactor, line.Detail)
		b.observations[line] = true
	}
}

// writeObservations stores the observations by time, as many of the newest as
// the bound allows. The time order is a reading aid: see stateOrdering.
func (b *StateBundle) writeObservations() {
	b.mu.Lock()
	marks := b.marks
	b.marks = nil
	b.mu.Unlock()
	b.observe(marks...)
	lines := slices.SortedFunc(maps.Keys(b.observations), func(x, y StateObservation) int {
		return cmp.Or(x.T.Compare(y.T), cmp.Compare(x.Clock, y.Clock), cmp.Compare(x.Kind, y.Kind),
			cmp.Compare(x.Object, y.Object), cmp.Compare(x.Detail, y.Detail))
	})
	room := b.limits.ObservationBytes
	var encoded [][]byte
	for _, line := range slices.Backward(lines) {
		data, err := json.Marshal(line)
		if err != nil {
			continue
		}
		data = append(b.redactor.Redact(data), '\n')
		if room -= int64(len(data)); room < 0 {
			break
		}
		encoded = append(encoded, data)
	}
	slices.Reverse(encoded)
	b.index.DroppedObservations = len(lines) - len(encoded)
	rel := b.rel(stateObservationsName)
	if err := b.redactor.WriteFile(b.dir, rel, slices.Concat(encoded...)); err != nil {
		b.problem = errors.Join(b.problem, err)
		return
	}
	b.index.Observations = rel
}

// note adds a source to the index and stores the index.
func (b *StateBundle) note(source StateSource) {
	b.order++
	source.Order = b.order
	if source.StartedAt.IsZero() {
		source.StartedAt = time.Now().UTC()
	}
	source.FinishedAt = time.Now().UTC()
	cp := b.checkpoint()
	cp.Sources = append(cp.Sources, source)
	b.writeIndex()
}

func (b *StateBundle) writeIndex() {
	data, err := json.MarshalIndent(b.index, "", "  ")
	if err == nil {
		err = b.redactor.WriteFile(b.dir, b.rel(stateIndexName), append(data, '\n'))
	}
	if err != nil {
		b.problem = err
		return
	}
	b.written = true
}

// stateFile is the content of one source's file.
type stateFile struct {
	Source      string            `json:"source"`
	Checkpoint  string            `json:"checkpoint"`
	CollectedAt time.Time         `json:"collectedAt"`
	Clock       string            `json:"clock"`
	Items       []json.RawMessage `json:"items"`
}

// The types below read and write the same JSON: a field that is not named here
// is not copied.

type stateMeta struct {
	Name              string            `json:"name"`
	UID               string            `json:"uid,omitempty"`
	Generation        int64             `json:"generation,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	CreationTimestamp *time.Time        `json:"creationTimestamp,omitempty"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
}

type stateCondition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Reason             string     `json:"reason,omitempty"`
	Message            string     `json:"message,omitempty"`
	ObservedGeneration int64      `json:"observedGeneration,omitempty"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
}

type stateCluster struct {
	Metadata stateMeta       `json:"metadata"`
	Status   json.RawMessage `json:"status,omitempty"`
}

type stateContainerState struct {
	Running *struct {
		StartedAt *time.Time `json:"startedAt,omitempty"`
	} `json:"running,omitempty"`
	Waiting *struct {
		Reason  string `json:"reason,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"waiting,omitempty"`
	Terminated *struct {
		ExitCode   int32      `json:"exitCode"`
		Reason     string     `json:"reason,omitempty"`
		Message    string     `json:"message,omitempty"`
		StartedAt  *time.Time `json:"startedAt,omitempty"`
		FinishedAt *time.Time `json:"finishedAt,omitempty"`
	} `json:"terminated,omitempty"`
}

type stateContainer struct {
	Name         string              `json:"name"`
	Ready        bool                `json:"ready"`
	Started      *bool               `json:"started,omitempty"`
	RestartCount int32               `json:"restartCount"`
	Image        string              `json:"image,omitempty"`
	ImageID      string              `json:"imageID,omitempty"`
	State        stateContainerState `json:"state"`
	LastState    stateContainerState `json:"lastState"`
}

type statePod struct {
	Metadata stateMeta `json:"metadata"`
	Spec     struct {
		NodeName string `json:"nodeName,omitempty"`
	} `json:"spec"`
	Status struct {
		Phase                 string           `json:"phase,omitempty"`
		Reason                string           `json:"reason,omitempty"`
		Message               string           `json:"message,omitempty"`
		PodIP                 string           `json:"podIP,omitempty"`
		StartTime             *time.Time       `json:"startTime,omitempty"`
		Conditions            []stateCondition `json:"conditions,omitempty"`
		InitContainerStatuses []stateContainer `json:"initContainerStatuses,omitempty"`
		ContainerStatuses     []stateContainer `json:"containerStatuses,omitempty"`
	} `json:"status"`
}

type stateService struct {
	Metadata stateMeta `json:"metadata"`
	Spec     struct {
		Type                     string            `json:"type,omitempty"`
		ClusterIP                string            `json:"clusterIP,omitempty"`
		PublishNotReadyAddresses bool              `json:"publishNotReadyAddresses,omitempty"`
		Selector                 map[string]string `json:"selector,omitempty"`
		Ports                    json.RawMessage   `json:"ports,omitempty"`
	} `json:"spec"`
}

type stateEndpointSlice struct {
	Metadata    stateMeta `json:"metadata"`
	AddressType string    `json:"addressType,omitempty"`
	Endpoints   []struct {
		Addresses  []string        `json:"addresses,omitempty"`
		Conditions json.RawMessage `json:"conditions,omitempty"`
		NodeName   string          `json:"nodeName,omitempty"`
		TargetRef  *struct {
			Kind string `json:"kind,omitempty"`
			Name string `json:"name,omitempty"`
			UID  string `json:"uid,omitempty"`
		} `json:"targetRef,omitempty"`
	} `json:"endpoints,omitempty"`
	Ports json.RawMessage `json:"ports,omitempty"`
}

type stateEvent struct {
	Metadata       stateMeta `json:"metadata"`
	InvolvedObject struct {
		Kind string `json:"kind,omitempty"`
		Name string `json:"name,omitempty"`
		UID  string `json:"uid,omitempty"`
	} `json:"involvedObject"`
	Type           string     `json:"type,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	Message        string     `json:"message,omitempty"`
	Count          int32      `json:"count,omitempty"`
	FirstTimestamp *time.Time `json:"firstTimestamp,omitempty"`
	LastTimestamp  *time.Time `json:"lastTimestamp,omitempty"`
	EventTime      *time.Time `json:"eventTime,omitempty"`
	Series         *struct {
		Count            int32      `json:"count,omitempty"`
		LastObservedTime *time.Time `json:"lastObservedTime,omitempty"`
	} `json:"series,omitempty"`
	ReportingComponent string `json:"reportingComponent,omitempty"`
	ReportingInstance  string `json:"reportingInstance,omitempty"`
	Source             struct {
		Component string `json:"component,omitempty"`
		Host      string `json:"host,omitempty"`
	} `json:"source"`

	// ObservedAt is the last time the Event was observed, by its reporter.
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	// BeganBeforeScenario marks an aggregated Event whose first occurrence is
	// older than the scenario: only its later occurrences belong to it.
	BeganBeforeScenario bool `json:"beganBeforeScenario,omitempty"`
}

// lastObserved is the latest time of the Event in UTC, or zero when it has none.
func (e stateEvent) lastObserved() time.Time {
	var last time.Time
	times := []*time.Time{e.FirstTimestamp, e.LastTimestamp, e.EventTime}
	if e.Series != nil {
		times = append(times, e.Series.LastObservedTime)
	}
	for _, t := range times {
		if t != nil && t.After(last) {
			last = t.UTC()
		}
	}
	return last
}

// firstObserved is the time of the Event's first occurrence, or zero.
func (e stateEvent) firstObserved() time.Time {
	switch {
	case e.FirstTimestamp != nil:
		return e.FirstTimestamp.UTC()
	case e.EventTime != nil:
		return e.EventTime.UTC()
	}
	return time.Time{}
}

// times is how often the Event occurred, as far as it says.
func (e stateEvent) times() int32 {
	if e.Series != nil {
		return max(e.Count, e.Series.Count)
	}
	return e.Count
}
