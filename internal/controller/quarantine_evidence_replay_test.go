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
	"bufio"
	"compress/gzip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// These observations come from the committed heartbeat samples of the
// higher-priority member experiment (POC-50), CUBRID 11.4.6.1963-0e7d3c1
// on Docker. See docs/poc/RESULTS.md and docs/poc/poc50/README.md. The
// samplers ran independently and the retained files are thinned. Each
// frame uses exactly the listed local observations, with decision time at
// the latest recorded call completion. An omitted sample stays unknown.
// This Docker experiment had no Operator, Pod identities or fence evidence.
func TestQuarantineReplayOfRecordedHeartbeatSamples(t *testing.T) {
	members := []string{"h-0", "h-1", "h-2"}
	frames := []struct {
		name    string
		starts  [3]string
		masters []string
		reason  databasev1alpha1.WriteQuarantineReason
	}{
		{
			name:    "only the existing master has a local sample",
			starts:  [3]string{"", "1793892998.368", ""},
			masters: []string{members[1]},
		},
		{
			name:    "two local samples report active masters",
			starts:  [3]string{"1793892998.489", "1793892998.509", ""},
			masters: members[:2], reason: databasev1alpha1.WriteQuarantinedMultipleMasters,
		},
		{
			name:    "one active master remains and quarantine stays in force",
			starts:  [3]string{"1793893000.178", "1793893000.174", "1793893000.170"},
			masters: []string{members[0]}, reason: databasev1alpha1.WriteQuarantinedMultipleMasters,
		},
	}
	input := quarantineInput{
		Members: members, IdentitiesUnknown: true,
		Nonce: func() (string, error) { return "evidence-replay-nonce", nil },
	}
	for _, frame := range frames {
		input.Now = time.Time{}
		input.Observations = make(map[string]RoleObservation, len(members))
		for i, start := range frame.starts {
			if start == "" {
				continue
			}
			observation := replayHeartbeatObservation(t, members[i], start)
			input.Observations[members[i]] = observation
			if observation.ObservedAt.After(input.Now) {
				input.Now = observation.ObservedAt
			}
		}
		decision, err := decideWriteQuarantine(input)
		if err != nil {
			t.Fatalf("%s: %v", frame.name, err)
		}
		if !slices.Equal(decision.ActiveMasters, frame.masters) {
			t.Fatalf("%s: active masters = %v, want %v", frame.name, decision.ActiveMasters, frame.masters)
		}
		var reason databasev1alpha1.WriteQuarantineReason
		if decision.Quarantine != nil {
			reason = decision.Quarantine.Reason
		}
		if reason != frame.reason || decision.Released != nil {
			t.Fatalf("%s: reason = %q, want %q with no release", frame.name, reason, frame.reason)
		}
		input.Quarantine = decision.Quarantine
	}
}

func replayHeartbeatObservation(t *testing.T, member, start string) RoleObservation {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "poc", "poc50", "samples", "prio", member+".tsv.gz")
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	replay, err := gzip.NewReader(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replay.Close() }()
	scanner := bufio.NewScanner(replay)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if fields[0] != start {
			continue
		}
		if len(fields) < 8 || fields[1] != "0" || fields[3] != member {
			t.Fatalf("invalid archived observation for %s at %s: %q", member, start, fields)
		}
		ms, err := strconv.ParseInt(strings.Replace(start, ".", "", 1), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		elapsed, err := time.ParseDuration(fields[2] + "ms")
		if err != nil {
			t.Fatal(err)
		}
		role := databasev1alpha1.CubridRole(fields[4])
		localMaster := slices.Contains(strings.Split(fields[5], ","), member+"=master")
		return RoleObservation{
			Reachable: true, ObservedAt: time.UnixMilli(ms).Add(elapsed), Role: role,
			ActiveMasterClaim: role == databasev1alpha1.RoleMaster && localMaster && fields[7] == wqActiveServer,
		}
	}
	t.Fatalf("archived observation for %s at %s is missing: %v", member, start, scanner.Err())
	return RoleObservation{}
}
