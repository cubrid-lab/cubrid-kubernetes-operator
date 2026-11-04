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
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// reasonConditionDisagreement is the reason of a scenario in which the
// Conditions disagreed with SQL, CUBRID's heartbeat or the Broker Deployment.
const reasonConditionDisagreement = "condition_disagreement"

// conditionSampleEvery is how often the CubridCluster's status is read while
// a fault runs.
const conditionSampleEvery = 2 * time.Second

// conditionReadLimit bounds one round of reads.
const conditionReadLimit = 20 * time.Second

// conditionRound is one line of conditions.jsonl: one read of the status, the
// read-write Broker Deployment's available Pods, and, when asked for, the
// role CUBRID's heartbeat reports on each member.
type conditionRound struct {
	Status *faults.ConditionSample `json:"status,omitempty"`
	// StatusError says why the status could not be read.
	StatusError string            `json:"statusError,omitempty"`
	Roles       *faults.RoleSweep `json:"roles,omitempty"`
}

// conditionSampler reads the status in the background until it is stopped.
type conditionSampler struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	// rounds is written by the sampler's goroutine only, and read after it
	// has ended.
	rounds []conditionRound
}

// sampleConditions starts reading the status every conditionSampleEvery, and
// with roles also asks every member for its role in each round.
func (r *haRun) sampleConditions(roles bool) *conditionSampler {
	ctx, cancel := context.WithCancel(context.Background())
	c := &conditionSampler{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			start := time.Now()
			round := r.conditionRound(ctx, roles)
			if ctx.Err() != nil {
				return // a round cut short by stop is not kept
			}
			c.rounds = append(c.rounds, round)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(start.Add(conditionSampleEvery))):
			}
		}
	}()
	return c
}

// stop ends the sampling and returns the rounds. It is safe to call more than
// once.
func (c *conditionSampler) stop() []conditionRound {
	c.once.Do(func() {
		c.cancel()
		<-c.done
	})
	return c.rounds
}

func (r *haRun) conditionRound(ctx context.Context, roles bool) conditionRound {
	ctx, cancel := context.WithTimeout(ctx, conditionReadLimit)
	defer cancel()
	var round conditionRound
	start := time.Now().UTC()
	if s, err := r.status(ctx); err != nil {
		round.StatusError = err.Error()
	} else {
		sample := s.details
		sample.ReadStart = start
		// availableReplicas is left out of the Deployment's status when it is 0.
		if out, err := r.kubectl(ctx, "get", "deployment", r.cluster+"-broker-rw",
			"-o", "jsonpath={.status.availableReplicas}"); err == nil {
			n, err := 0, error(nil)
			if out = strings.TrimSpace(out); out != "" {
				n, err = strconv.Atoi(out)
			}
			if err == nil {
				sample.RWAvailable = &n
			}
		}
		sample.ReadEnd = time.Now().UTC()
		round.Status = &sample
	}
	if roles {
		sweep := faults.RoleSweep{Start: time.Now().UTC(), Roles: make(map[string]string, len(r.members))}
		for _, pod := range r.members {
			sweep.Roles[pod] = r.heartbeatRole(ctx, pod)
		}
		sweep.End = time.Now().UTC()
		round.Roles = &sweep
	}
	return round
}

// heartbeatRole is "master" for a member CUBRID reports as an active master,
// "slave" for a standby slave, and "" for anything else or no answer.
func (r *haRun) heartbeatRole(ctx context.Context, pod string) string {
	out, err := r.kubectl(ctx, "exec", pod, "--", "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
	switch {
	case err != nil:
		return ""
	case strings.Contains(out, "current "+pod+", state master") && strings.Contains(out, "registered_and_active"):
		return "master"
	case strings.Contains(out, "current "+pod+", state slave") && strings.Contains(out, "registered_and_standby"):
		return "slave"
	}
	return ""
}

// checkConditions compares the sampled Conditions with the clients'
// operations and the fault's timeline by the given rules. It stores
// conditions.jsonl and agreement.txt under dir and returns what it found, the
// files it wrote, and the conditionAgreement measurement of summary.json.
func checkConditions(dir string, rounds []conditionRound, events []workload.Event, steps []faults.Step,
	rules []faults.Rule) (faults.Agreement, []string, map[string]any) {
	obs := faults.Observations{Steps: steps}
	unread := 0
	var lines strings.Builder
	for _, round := range rounds {
		if line, err := json.Marshal(round); err == nil {
			lines.Write(line)
			lines.WriteByte('\n')
		}
		if round.Status != nil {
			obs.Samples = append(obs.Samples, *round.Status)
		} else {
			unread++
		}
		if round.Roles != nil {
			obs.Sweeps = append(obs.Sweeps, *round.Roles)
		}
	}
	for _, e := range events {
		switch e.Event {
		case workload.Acknowledged, workload.Failed, workload.Unknown:
			obs.SQL = append(obs.SQL, faults.SQLEvent{T: e.T, Write: e.Op == workload.OpInsert, Outcome: e.Event})
		}
	}
	agreement := faults.CheckAgreement(obs, rules)

	var text strings.Builder
	fmt.Fprintf(&text, "Agreement of the CubridCluster's Conditions with SQL and the fault, %s\n\n", dir)
	fmt.Fprintf(&text, "Status reads: %d, of which %d could not be read; heartbeat sweeps: %d; "+
		"client operations with an outcome: %d\n", len(rounds), unread, len(obs.Sweeps), len(obs.SQL))
	fmt.Fprintf(&text, "Tolerance of the rules that compare sources: %s\n\nRules:\n", faults.AgreementTolerance)
	for _, rule := range rules {
		fmt.Fprintf(&text, "- %s (%s): %s\n", rule.ID, rule.Kind, rule.Says)
	}
	for _, part := range []struct {
		title string
		lines []string
	}{{"Disagreements", agreement.Disagreements}, {"Notes, which never fail the run", agreement.Notes}} {
		fmt.Fprintf(&text, "\n%s:\n", part.title)
		if len(part.lines) == 0 {
			text.WriteString("- none\n")
		}
		for _, l := range part.lines {
			fmt.Fprintf(&text, "- %s\n", l)
		}
	}

	var files []string
	for _, file := range [][2]string{{"conditions.jsonl", lines.String()}, {"agreement.txt", text.String()}} {
		if f := writeScenarioFile(dir+"/"+file[0], []byte(file[1])); f != "" {
			files = append(files, f)
		}
	}
	return agreement, files, map[string]any{
		"statusReads":   len(rounds),
		"unreadStatus":  unread,
		"disagreements": len(agreement.Disagreements),
		"notes":         len(agreement.Notes),
	}
}

// TestCheckConditionsReadsRoundsAndClientEvents checks that a disagreement in
// the sampled status reaches the scenario, that a round whose status could
// not be read is counted, and that both evidence files are written.
func TestCheckConditionsReadsRoundsAndClientEvents(t *testing.T) {
	t.Setenv("E2E_EVIDENCE_DIR", t.TempDir())
	now := time.Now().UTC()
	read := func(routingReady, writeEndpointReady string) *faults.ConditionSample {
		return &faults.ConditionSample{ReadStart: now, ReadEnd: now, LastObserved: now,
			Conditions: map[string]faults.ConditionState{
				"RoutingReady": {Status: routingReady}, "WriteEndpointReady": {Status: writeEndpointReady},
			}}
	}
	rounds := []conditionRound{
		{Status: read("True", "True")},
		{StatusError: "the API server did not answer"},
		{Status: read("True", "False")},
	}
	events := []workload.Event{
		{T: now.Add(time.Second), Op: workload.OpInsert, Event: workload.Attempted},
		{T: now.Add(time.Second), Op: workload.OpInsert, Event: workload.Failed},
	}
	agreement, files, measured := checkConditions("S05/test", rounds, events, nil,
		append(faults.NeverRules(), faults.S05Rules("m-0", true)...))
	if len(agreement.Disagreements) != 1 || !strings.Contains(agreement.Disagreements[0], "WriteEndpointReady=False") {
		t.Errorf("disagreements = %v, want the read with routing ready and no write endpoint", agreement.Disagreements)
	}
	if !slices.ContainsFunc(agreement.Notes, func(n string) bool { return strings.Contains(n, "1 writes failed") }) {
		t.Errorf("notes = %v, want the failed write while routing was ready", agreement.Notes)
	}
	if measured["statusReads"] != 3 || measured["unreadStatus"] != 1 || measured["disagreements"] != 1 {
		t.Errorf("measurement = %v", measured)
	}
	if want := []string{"S05/test/conditions.jsonl", "S05/test/agreement.txt"}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
}
