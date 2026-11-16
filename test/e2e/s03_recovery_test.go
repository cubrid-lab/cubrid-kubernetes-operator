//go:build e2e

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
	"fmt"
	"slices"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

const (
	s03PrimaryResolved  = "PrimaryResolved"
	s03ConditionFalse   = "False"
	s03ConditionTrue    = "True"
	s03ConditionUnknown = "Unknown"
	s03PodReady         = "Ready"
)

// Only current Pod/container identity and readiness leave the metadata reader.
// In particular, Pod specs, environment variables and raw errors are not kept.
type s03ReplacementObservation struct {
	UID            string    `json:"uid"`
	Deleting       bool      `json:"deleting"`
	PodReady       bool      `json:"podReady"`
	ContainerID    string    `json:"containerID"`
	Restarts       int32     `json:"restarts"`
	RunningStarted time.Time `json:"runningStarted"`
	ContainerReady bool      `json:"containerReady"`
}

func (o s03ReplacementObservation) eligible(originalUID string) bool {
	return originalUID != "" && o.UID != "" && o.UID != originalUID && !o.Deleting && o.PodReady &&
		o.ContainerID != "" && o.ContainerReady && !o.RunningStarted.IsZero()
}

type s03RecoveryProbe struct {
	members                []string
	replacement            func(context.Context) (s03ReplacementObservation, error)
	activeMasters          func(context.Context) []string
	master                 func(context.Context) (string, []string, error)
	status                 func(context.Context) (statusSnapshot, error)
	formationTime          func(context.Context) (time.Duration, error)
	lastAnswerAcknowledged func(context.Context) bool
	now                    func() time.Time
}

type s03RecoveryFrame struct {
	StartedAt     time.Time                  `json:"queryStartedAt"`
	FinishedAt    time.Time                  `json:"queryFinishedAt"`
	Member        string                     `json:"member"`
	Before        s03ReplacementObservation  `json:"before"`
	After         *s03ReplacementObservation `json:"after,omitempty"`
	ActiveMasters []string                   `json:"activeMasters"`
	Reason        string                     `json:"reason"`
}

type s03RecoveryStatus struct {
	StartedAt       time.Time `json:"queryStartedAt"`
	FinishedAt      time.Time `json:"queryFinishedAt"`
	Primary         string    `json:"primary"`
	PrimaryResolved string    `json:"primaryResolved"`
	LastObserved    time.Time `json:"lastObservedTime"`
}

type s03RecoveryObservations struct {
	Rejected            *s03RecoveryFrame  `json:"firstRejectedSameName,omitempty"`
	Accepted            *s03RecoveryFrame  `json:"acceptedCandidate,omitempty"`
	Baseline            *s03RecoveryStatus `json:"statusBaseline,omitempty"`
	Agreement           *s03RecoveryStatus `json:"statusAgreement,omitempty"`
	Reason              string             `json:"latestReason"`
	Polls               int                `json:"polls"`
	MetadataUnavailable int                `json:"metadataUnavailable"`
	StatusUnavailable   int                `json:"statusUnavailable"`
}

type s03RecoveryState struct {
	newMaster            string
	electedAt, noticedAt time.Time
	observations         s03RecoveryObservations
}

// newS03RecoveryCheck qualifies only the initial same-name candidate. Global
// active claims and every subsequent full-role master comparison stay intact.
func newS03RecoveryCheck(probe s03RecoveryProbe, deletedMember, originalUID string) (
	func(context.Context) (bool, error), *s03RecoveryState) {
	state := &s03RecoveryState{}
	return func(ctx context.Context) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		state.observations.Polls++
		started := probe.now()
		before, metadataErr := probe.replacement(ctx)
		if metadataErr != nil {
			state.observations.MetadataUnavailable++
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		// Keep every claim, including the deleted name while its replacement is
		// Pending. A confirming sweep distinguishes overlap from a mid-sweep handover.
		masters := probe.activeMasters(ctx)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if len(masters) > 1 {
			confirmed := probe.activeMasters(ctx)
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if err := faults.CheckOneMaster(masters, confirmed); err != nil {
				state.observations.Reason = "multiple_active_masters"
				return false, err
			}
			state.observations.Reason = "handover_mid_sweep"
			return false, nil
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if state.newMaster == "" && !state.acceptCandidate(ctx, probe, deletedMember, originalUID,
			masters, before, metadataErr, started) {
			return false, ctx.Err()
		}

		// Check real second changes before any pending status-freshness return.
		now, _, err := probe.master(ctx)
		if err != nil {
			state.observations.Reason = "full_roles_unavailable"
			return false, ctx.Err()
		}
		if now != state.newMaster {
			state.observations.Reason = "master_changed_again"
			return false, fmt.Errorf("the master changed again, from %s to %s", state.newMaster, now)
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !state.statusAgreed(ctx, probe) {
			return false, ctx.Err()
		}
		if _, err := probe.formationTime(ctx); err != nil {
			state.observations.Reason = "formation_unavailable"
			return false, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		done := probe.lastAnswerAcknowledged(ctx)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		state.observations.Reason = "client_not_acknowledged"
		if done {
			state.observations.Reason = "recovered"
		}
		return done, nil
	}, state
}

func (s *s03RecoveryState) acceptCandidate(ctx context.Context, probe s03RecoveryProbe,
	deletedMember, originalUID string, masters []string, before s03ReplacementObservation,
	metadataErr error, started time.Time) bool {

	if len(masters) == 0 {
		s.observations.Reason = "no_active_master"
		return false
	}
	frame := s03RecoveryFrame{StartedAt: started, Member: masters[0], Before: before,
		ActiveMasters: knownS03Claims(masters, probe.members)}
	if masters[0] == deletedMember {
		if metadataErr != nil || !before.eligible(originalUID) {
			frame.Reason = "replacement_unproven"
		} else {
			after, err := probe.replacement(ctx)
			if err != nil {
				s.observations.MetadataUnavailable++
			}
			frame.After = &after
			if err != nil || !after.eligible(originalUID) || after != before {
				frame.Reason = "replacement_changed"
			}
		}
	}
	frame.FinishedAt = probe.now()
	if frame.Reason != "" {
		if s.observations.Rejected == nil {
			s.observations.Rejected = &frame
		}
		s.observations.Reason = frame.Reason
		return false
	}
	if err := ctx.Err(); err != nil {
		return false
	}
	frame.Reason = "candidate_accepted"
	s.observations.Accepted = &frame
	s.newMaster = masters[0]
	s.electedAt = probe.now()
	return true
}

// The first complete exported status is a fixed baseline, never a notice. A
// later matching, resolved export supplies only an external agreement bound.
func (s *s03RecoveryState) statusAgreed(ctx context.Context, probe s03RecoveryProbe) bool {
	if !s.noticedAt.IsZero() {
		return true
	}
	started := probe.now()
	status, err := probe.status(ctx)
	finished := probe.now()
	if ctx.Err() != nil {
		return false
	}
	resolved := status.conditions[s03PrimaryResolved]
	if err != nil || status.lastObserved.IsZero() || (status.primary == "" && resolved == s03ConditionTrue) ||
		(resolved != s03ConditionTrue && resolved != s03ConditionFalse && resolved != s03ConditionUnknown) {
		s.observations.StatusUnavailable++
		s.observations.Reason = "status_unavailable"
		return false
	}
	frame := s03RecoveryStatus{StartedAt: started, FinishedAt: finished, Primary: status.primary,
		PrimaryResolved: resolved, LastObserved: status.lastObserved}
	if s.observations.Baseline == nil {
		s.observations.Baseline = &frame
		s.observations.Reason = "status_baseline"
		return false
	}
	if !status.lastObserved.After(s.observations.Baseline.LastObserved) ||
		status.primary != s.newMaster || resolved != s03ConditionTrue {
		s.observations.Reason = "status_not_advanced_and_resolved"
		return false
	}
	s.observations.Agreement = &frame
	s.noticedAt = finished
	return true
}

func knownS03Claims(claims, members []string) []string {
	kept := make([]string, 0, len(members))
	for _, member := range members {
		if slices.Contains(claims, member) && !slices.Contains(kept, member) {
			kept = append(kept, member)
		}
	}
	return kept
}

func (r *haRun) readS03Replacement(ctx context.Context, member string) (s03ReplacementObservation, error) {
	out, err := r.kubectl(ctx, "get", "pod", member, "-o", "json")
	if err != nil {
		return s03ReplacementObservation{}, err
	}
	return parseS03Replacement(out)
}

func parseS03Replacement(out string) (s03ReplacementObservation, error) {
	var pod struct {
		Metadata struct {
			UID               string     `json:"uid"`
			DeletionTimestamp *time.Time `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				Name         string `json:"name"`
				ContainerID  string `json:"containerID"`
				Ready        bool   `json:"ready"`
				RestartCount *int32 `json:"restartCount"`
				State        struct {
					Running *struct {
						StartedAt time.Time `json:"startedAt"`
					} `json:"running"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &pod); err != nil {
		return s03ReplacementObservation{}, err
	}
	got := s03ReplacementObservation{UID: pod.Metadata.UID, Deleting: pod.Metadata.DeletionTimestamp != nil}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == s03PodReady {
			got.PodReady = condition.Status == s03ConditionTrue
		}
	}
	found := 0
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name != "cubrid" {
			continue
		}
		found++
		if container.RestartCount == nil {
			return got, fmt.Errorf("cubrid restart count unavailable")
		}
		got.ContainerID, got.ContainerReady, got.Restarts = container.ContainerID, container.Ready, *container.RestartCount
		if container.State.Running != nil {
			got.RunningStarted = container.State.Running.StartedAt
		}
	}
	if found != 1 {
		return got, fmt.Errorf("expected one cubrid container status, got %d", found)
	}
	return got, nil
}

// finishS03Recovery must check StillRunning before copying ANY callback state.
// The returned local values are unavailable while the callback may still write.
func finishS03Recovery(outcome faults.Outcome, state *s03RecoveryState, variant string,
	redact *evidence.Redactor, write func(string, []byte) string) (s03RecoveryState, string, string) {
	if len(outcome.StillRunning) != 0 {
		return s03RecoveryState{}, "", "Recovery observations unavailable: fault callback is still running"
	}
	snapshot := *state
	data, err := json.MarshalIndent(snapshot.observations, "", "  ")
	note := ""
	if err != nil {
		data = []byte(`{"unavailable":"serialization_failed"}`)
		note = "Recovery observations unavailable: serialization failed"
	}
	data = redact.Redact(data)
	if int64(len(data))+1 > evidence.DefaultLogLimits().SourceBytes {
		data = []byte(`{"unavailable":"source_size_limit"}`)
		note = "Recovery observations unavailable: source size limit"
	}
	file := write("S03/"+variant+"/recovery-observations.json", append(data, '\n'))
	if file == "" {
		note = "Recovery observations unavailable: file was not stored"
	}
	return snapshot, file, note
}

func s03ObservedWithin(at, issued time.Time) string {
	if at.IsZero() || issued.IsZero() {
		return evidence.Unknown
	}
	return at.Sub(issued).Round(time.Millisecond).String()
}
