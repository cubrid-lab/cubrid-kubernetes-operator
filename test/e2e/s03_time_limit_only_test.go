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
	"maps"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

// The graceful deletion of the master Pod reaches its limits only after
// every other check of S03 held (the data rules, one master, the untouched
// members, the Events and the log). Its fail is recorded as caused by the
// time limits alone only when the Conditions then agreed with heartbeat and
// every rule on them was judged, the limits were set and measured, and only
// limits a measurement must not exceed were exceeded. These tests need no
// cluster.
func TestS03TimeLimitOnly(t *testing.T) {
	confirmed := true
	failover := faults.Limit{Name: "failover_limit", Value: 30 * time.Second}
	rejoin := faults.Limit{Name: "rejoin_limit", Value: 5 * time.Minute}
	stable := faults.Limit{Name: "stable_period", Value: 30 * time.Second}
	late := []faults.Bound{{Limit: failover, Measured: 41200 * time.Millisecond},
		{Limit: stable, Measured: 45 * time.Second, AtLeast: true}, {Limit: rejoin, Measured: time.Minute}}
	tests := map[string]struct {
		variant   string
		bounds    []faults.Bound
		agreement faults.Agreement
		change    func(*evidence.Scenario)
		want      map[string]string
	}{
		"failover_limit exceeded and every other check held": {s03Graceful, late, faults.Agreement{}, nil,
			map[string]string{"failover_limit": "41.2s > 30s"}},
		"within the limits there is no time-limit fail": {s03Graceful, []faults.Bound{
			{Limit: failover, Measured: 5 * time.Second}}, faults.Agreement{}, nil, nil},
		"the Conditions disagreed with heartbeat": {s03Graceful, late,
			faults.Agreement{Disagreements: []string{"never: routing-ready-needs-resolved-primary: read at x"}}, nil, nil},
		"a rule on the Conditions could not be judged": {s03Graceful, late,
			faults.Agreement{NotJudged: []string{"resolved-primary-was-a-heartbeat-master: no sweep"}}, nil, nil},
		"a limit not set beside the exceeded one": {s03Graceful, []faults.Bound{late[0], {Limit: faults.Limit{
			Name: "rejoin_limit"}}}, faults.Agreement{}, nil, nil},
		"the fault not recorded as confirmed": {s03Graceful, late, faults.Agreement{},
			func(s *evidence.Scenario) { s.FaultConfirmed = nil }, nil},
		"an abrupt variant, a strict gate, is not marked": {s03Abrupt, late, faults.Agreement{}, nil, nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			result := judged(evidence.Scenario{ID: "S03", Variant: tc.variant, FaultConfirmed: &confirmed,
				CleanupSucceeded: &confirmed, NeverEvents: []string{}}, tc.bounds...)
			if tc.change != nil {
				tc.change(&result)
			}
			raw := withAgreement(result, tc.agreement)
			got := s03TimeLimitOnly(tc.variant, raw, tc.bounds, tc.agreement)
			if got.Result != raw.Result || got.Reason != raw.Reason {
				t.Errorf("the verdict changed from %s %q to %s %q", raw.Result, raw.Reason, got.Result, got.Reason)
			}
			if got.TimeLimitOnly != (tc.want != nil) || !maps.Equal(got.ExceededLimits, tc.want) {
				t.Errorf("timeLimitOnly %t %v, want %v (result %s %q)", got.TimeLimitOnly, got.ExceededLimits,
					tc.want, got.Result, got.Reason)
			}
		})
	}
}
