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

package main

import (
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// notADirection opens the reason of a partitionDirection that is not a
// direction name.
const notADirection = "partitionDirection is not a direction name"

// A network-partition (S07) run that cut only one direction of traffic is
// filed under the same variant and lane as a both-ways run. The registered
// S07 variants are both-ways cuts, so a one-way record must never count as
// one of them, and the candidate is No-Go with the reason shown. A record
// without the partitionDirection measurement predates it and cut both ways.
func TestVerifierDoesNotCountAOneWayNetworkCutAsTheRegisteredVariant(t *testing.T) {
	tests := []struct {
		name    string
		variant string
		key     string
		value   any
		want    string
	}{
		{"inbound on the master-isolated cut", s07MasterIsolated, masterIsolatedKey, "inbound",
			"a one-way network cut (partitionDirection inbound) is not the registered both-ways variant"},
		{"outbound on the higher-rank cut", "S07/higher-rank-isolated", higherRankKey, "outbound",
			"a one-way network cut (partitionDirection outbound) is not the registered both-ways variant"},
		{"outbound on the slave-isolated control", "S07/slave-isolated", slaveIsolatedKey, "outbound",
			"a one-way network cut (partitionDirection outbound) is not the registered both-ways variant"},
		{"empty", s07MasterIsolated, masterIsolatedKey, "", notADirection},
		{"a number", s07MasterIsolated, masterIsolatedKey, 2, notADirection},
		{"null", s07MasterIsolated, masterIsolatedKey, nil, notADirection},
		{"a list", s07MasterIsolated, masterIsolatedKey, []any{bothWays}, notADirection},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, func(runs []evidence.Summary) {
				scenarioOf(runs, vmRunID, tc.variant).Measurements[partitionDirection] = tc.value
			})
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok || !strings.Contains(out, overallNoGo) {
				t.Fatalf("a %s network cut did not make the candidate No-Go:\n%s", tc.name, out)
			}
			if row := requiredRow(t, out, tc.key); !strings.Contains(row, "| NO-GO |") ||
				!strings.Contains(row, tc.want) {
				t.Errorf("the required row of %s does not show %q: %s", tc.key, tc.want, row)
			}
			if row := recordedRow(t, out, tc.key); !strings.Contains(row, passedButNoGo) ||
				!strings.Contains(row, tc.want) {
				t.Errorf("the recorded row of %s does not show %q: %s", tc.key, tc.want, row)
			}
		})
	}
}

// A one-way cut also cannot count as a disclosed measurement: the master-
// isolated run that ended without a promotion is judged only when it cut both
// ways.
func TestVerifierDoesNotDiscloseAOneWayNetworkCut(t *testing.T) {
	root, manifest := buildCandidate(t, then(noPromotion, func(runs []evidence.Summary) {
		scenarioOf(runs, vmRunID, s07MasterIsolated).Measurements[partitionDirection] = "inbound"
	}))
	out, ok := runVerifier(t, root, manifest, v01Families)
	if ok || !strings.Contains(out, overallNoGo) {
		t.Fatalf("a one-way cut that ended without a promotion was not No-Go:\n%s", out)
	}
	if row := requiredRow(t, out, masterIsolatedKey); !strings.Contains(row, "| NO-GO |") ||
		!strings.Contains(row, "partitionDirection inbound") {
		t.Errorf("the required row does not show the one-way cut: %s", row)
	}
}

// A both-ways cut, recorded as such or recorded before the measurement
// existed, is judged as before.
func TestVerifierCountsABothWaysNetworkCut(t *testing.T) {
	tests := []struct {
		name string
		set  bool
	}{
		{"recorded as both", true},
		{"recorded before the measurement existed", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, func(runs []evidence.Summary) {
				for _, name := range []string{s07MasterIsolated, "S07/higher-rank-isolated",
					"S07/slave-isolated"} {
					m := scenarioOf(runs, vmRunID, name).Measurements
					delete(m, partitionDirection)
					if tc.set {
						m[partitionDirection] = bothWays
					}
				}
			})
			out, ok := runVerifier(t, root, manifest, v01Families)
			if !ok || !strings.Contains(out, overallGo) {
				t.Fatalf("a both-ways network cut did not verify as Go:\n%s", out)
			}
			for _, key := range []string{masterIsolatedKey, higherRankKey, slaveIsolatedKey} {
				if row := requiredRow(t, out, key); !strings.Contains(row, "| GO |") {
					t.Errorf("required variant %s is not Go: %s", key, row)
				}
			}
		})
	}
}
