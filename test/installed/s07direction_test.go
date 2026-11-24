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

package installed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

// The tests in this file cover -partition-direction (#115): the network
// partition lane cuts the member off the other members in one direction only,
// inbound or outbound, and cuts the manager Pod both ways as before.

// The three directions are accepted with every variant and with every other
// S07 option, an empty one is both, and a value that is none of them, or a
// one-way direction without a partition fault, is refused.
func TestValidateS07PartitionDirection(t *testing.T) {
	base := func(t *testing.T) HAConfig {
		cfg := testHAConfig(t)
		cfg.NamespaceMode, cfg.Level, cfg.StablePeriod = FreshNamespace, evidence.LevelVMLab, time.Second
		cfg.Fault, cfg.PartitionHold = FaultPartitionMasterIsolated, time.Minute
		return cfg
	}
	configs := map[string]func(*testing.T) HAConfig{
		"master-isolated": base,
		"slave-isolated": func(t *testing.T) HAConfig {
			cfg := base(t)
			cfg.Fault = FaultPartitionSlaveIsolated
			return cfg
		},
		"higher-rank-isolated with keepalive":            keepaliveConfig,
		"with the operator restart":                      restartConfig,
		"with keepalive, the manual release and repeats": repeatConfig,
	}
	for name, config := range configs {
		for _, direction := range []string{"", s07DirectionBoth, s07DirectionInbound, s07DirectionOutbound} {
			cfg := config(t)
			cfg.PartitionDirection = direction
			if err := cfg.validate(); err != nil {
				t.Errorf("%s, direction %q: %v", name, direction, err)
			}
		}
	}
	for name, change := range map[string]func(*HAConfig){
		"an unknown direction":           func(c *HAConfig) { c.PartitionDirection = "sideways" },
		"another spelling":               func(c *HAConfig) { c.PartitionDirection = "Inbound" },
		"netfault's own name":            func(c *HAConfig) { c.PartitionDirection = string(netfault.In) },
		"inbound without a partition":    func(c *HAConfig) { c.Fault, c.PartitionDirection = "", s07DirectionInbound },
		"outbound without the partition": func(c *HAConfig) { c.Fault, c.PartitionDirection = "", s07DirectionOutbound },
	} {
		cfg := base(t)
		change(&cfg)
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "-partition-direction") {
			t.Errorf("%s: %v", name, err)
		}
	}
	none := base(t)
	none.Fault, none.PartitionDirection = "", s07DirectionBoth
	if err := none.validateS07Direction(); err != nil {
		t.Errorf("the default without a partition fault is refused: %v", err)
	}
}

// Each direction maps to the netfault cut of the same traffic, and a value
// that is none of them is passed on for the partition to refuse, never made
// into another cut.
func TestS07MemberCut(t *testing.T) {
	for direction, want := range map[string]netfault.Direction{"": netfault.Both, s07DirectionBoth: netfault.Both,
		s07DirectionInbound: netfault.In, s07DirectionOutbound: netfault.Out} {
		if got := s07MemberCut(direction); got != want {
			t.Errorf("direction %q cuts %s, want %s", direction, got, want)
		}
		if direction != "" && s07CutDirection(want) != direction {
			t.Errorf("cut %s is named %q, want %q", want, s07CutDirection(want), direction)
		}
	}
	p := &netfault.Partition{Target: netfault.Endpoint{Name: m0, Namespace: "ns", Pod: m0, UID: m0, IP: "10.42.0.10"},
		Cuts: []netfault.Cut{{Peer: netfault.Endpoint{Name: m1, Namespace: "ns", Pod: m1, UID: m1, IP: "10.42.0.11",
			Port: s07MemberPort}, Direction: s07MemberCut("sideways")}}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "unknown direction") {
		t.Errorf("an unknown direction reached the partition as %v", err)
	}
}

// The partition cuts the member off each other member in the run's
// direction and off the manager Pod both ways; with both, the default, it is
// the cut the lane always made.
func TestPrepareS07CutsInTheRunsDirection(t *testing.T) {
	for _, tc := range []struct {
		direction string
		members   netfault.Direction
	}{
		{"", netfault.Both}, {s07DirectionBoth, netfault.Both}, {s07DirectionInbound, netfault.In},
		{s07DirectionOutbound, netfault.Out},
	} {
		t.Run(tc.direction, func(t *testing.T) {
			h, _ := keepaliveRun(t)
			h.cfg.PartitionKeepalive = 0
			rec := &s07Record{Variant: s07HigherRankIsolated, Master: m1, MembersBefore: h.rec.MembersAfter,
				PartitionDirection: tc.direction}
			p, err := h.prepareS07(context.Background(), rec)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range p.Cuts {
				got = append(got, c.Peer.Name+" "+string(c.Direction))
			}
			want := []string{m1 + " " + string(tc.members), m2 + " " + string(tc.members), "operator both"}
			if p.Target.Name != m0 || !slices.Equal(got, want) {
				t.Errorf("target %s, cuts %v, want %s cut %v", p.Target.Name, got, m0, want)
			}
			var kept []string
			for _, k := range p.Keep {
				kept = append(kept, k.String())
			}
			if wantKept := []string{m1 + " -> " + m2, m2 + " -> " + m1}; !slices.Equal(kept, wantKept) {
				t.Errorf("kept %v, want %v", kept, wantKept)
			}
		})
	}
}

// The direction is in record.json, the measurements and cut.json; a record
// or cut.json written before the option existed has none, and is read as
// both.
func TestS07DirectionEvidence(t *testing.T) {
	old := &s07Record{}
	if err := json.Unmarshal([]byte(`{"variant":"master-isolated","master":"ha-0"}`), old); err != nil {
		t.Fatal(err)
	}
	if old.direction() != s07DirectionBoth || s07Measurements(old)["partitionDirection"] != s07DirectionBoth {
		t.Errorf("an old record reads as %q, measured %v", old.direction(), s07Measurements(old)["partitionDirection"])
	}
	data, err := json.Marshal(&s07Record{PartitionDirection: s07DirectionInbound})
	if err != nil {
		t.Fatal(err)
	}
	again := &s07Record{}
	if err := json.Unmarshal(data, again); err != nil || again.direction() != s07DirectionInbound ||
		!strings.Contains(string(data), `"partitionDirection":"inbound"`) {
		t.Errorf("round trip %s: %q, %v", data, again.direction(), err)
	}
	oldCut := &s07CutFile{}
	if err := json.Unmarshal([]byte(`{"namespace":"ns","target":"ha-0","peers":["ha-1","ha-2"]}`), oldCut); err != nil ||
		s07DirectionOf(oldCut.MemberDirection) != s07DirectionBoth ||
		s07DirectionOf(oldCut.OperatorDirection) != s07DirectionBoth {
		t.Errorf("an old cut.json reads as %+v, %v", oldCut, err)
	}

	cfg := testHAConfig(t)
	cfg.EvidenceDir = t.TempDir()
	ns := cfg.Namespace
	p := &netfault.Partition{Target: netfault.Endpoint{Name: m0, Namespace: ns, Pod: m0, IP: "10.42.1.10"},
		Cuts: []netfault.Cut{
			{Peer: netfault.Endpoint{Name: m1, Namespace: ns, Pod: m1, IP: "10.42.2.11"}, Direction: netfault.Out},
			{Peer: netfault.Endpoint{Name: m2, Namespace: ns, Pod: m2, IP: "10.42.3.12"}, Direction: netfault.Out},
			{Peer: netfault.Endpoint{Name: testManagerName, Pod: testManagerPodName, IP: "10.42.0.8", NoEnter: true},
				Direction: netfault.Both}}}
	h := &haRun{cfg: cfg, redactor: evidence.NewRedactor()}
	rec := &s07Record{Variant: s07MasterIsolated, PartitionDirection: s07DirectionOutbound}
	(&s07Cut{Partition: p, h: h, rec: rec}).writeState(func(*s07CutFile) {})
	written, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, s07File(s07MasterIsolated, "cut.json")))
	if err != nil {
		t.Fatal(err)
	}
	cut := &s07CutFile{}
	if err := json.Unmarshal(written, cut); err != nil || cut.MemberDirection != s07DirectionOutbound ||
		cut.OperatorDirection != s07DirectionBoth || !slices.Equal(cut.Peers, []string{m1, m2}) {
		t.Errorf("cut.json %s: %+v, %v", written, cut, err)
	}
}

// A run blocked before anything is cut still records the direction it was
// asked for.
func TestRunS07RecordsTheDirection(t *testing.T) {
	h, f := s07HigherRankRun(t, m1, failOver)
	h.cfg.PartitionDirection = s07DirectionOutbound
	// The member Pods have no IP in the fixture, so the run stops, blocked,
	// when it reads the target's address.
	result := h.runS07(context.Background())
	rec := requireNothingCut(t, h, f, result)
	measured := result.Measurements["partitionDirection"]
	if rec.PartitionDirection != s07DirectionOutbound || measured != s07DirectionOutbound {
		t.Errorf("recorded %q, measured %v", rec.PartitionDirection, measured)
	}
}
