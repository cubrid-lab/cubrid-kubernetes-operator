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

package faults

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// unset is how summary.json records a limit that is not set.
const unset = "unset"

// limitsOf reads "name=value name=value" as the limits a run records.
func limitsOf(text string) map[string]string {
	limits := map[string]string{}
	for pair := range strings.FieldsSeq(text) {
		name, value, _ := strings.Cut(pair, "=")
		limits[name] = value
	}
	return limits
}

func TestLimitFromEnv(t *testing.T) {
	const name = "E2E_TEST_LIMIT"
	tests := map[string]struct {
		value   string
		set     bool
		want    time.Duration
		wantErr bool
	}{
		"not set: the default":        {"", false, 30 * time.Second, false},
		"empty: the default":          {"", true, 30 * time.Second, false},
		"a duration":                  {"2m", true, 2 * time.Minute, false},
		"zero: the limit is not set":  {"0", true, 0, false},
		"unset: the limit is not set": {"unset", true, 0, false},
		"a number without a unit":     {"45", true, 0, true},
		"negative":                    {"-5s", true, 0, true},
		"not a duration":              {"soon", true, 0, true},
	}
	for label, tc := range tests {
		t.Run(label, func(t *testing.T) {
			if tc.set {
				t.Setenv(name, tc.value)
			}
			got, err := LimitFromEnv(name, 30*time.Second)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("got %s, %v; want %s, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestLimitDefaults pins the defaults to the values the scenario contract
// records (section "Time limits"): a default that changes here without the
// contract fails.
func TestLimitDefaults(t *testing.T) {
	tests := []struct {
		spec LimitSpec
		name string
		env  string
		// want is the default; zero means that the contract records none.
		want time.Duration
	}{
		{InstallLimit, "install_limit", "E2E_INSTALL_LIMIT", 0},
		{ReadyLimit, "ready_limit", "E2E_READY_LIMIT", 0},
		{FormationLimit, "formation_limit", "E2E_FORMATION_LIMIT", 5 * time.Minute},
		{ReplicationLimit, "replication_limit", "E2E_REPLICATION_LIMIT", 30 * time.Second},
		{FailoverLimit, "failover_limit", "E2E_FAILOVER_LIMIT", 30 * time.Second},
		{RejoinLimit, "rejoin_limit", "E2E_REJOIN_LIMIT", 5 * time.Minute},
		{StablePeriod, "stable_period", "E2E_STABLE_PERIOD", 30 * time.Second},
		{BrokerRecoveryLimit, "broker_recovery_limit", "E2E_BROKER_RECOVERY_LIMIT", time.Minute},
		{OperatorResyncLimit, "operator_resync_limit", "E2E_OPERATOR_RESYNC_LIMIT", 2 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.spec.Name != tc.name || tc.spec.Env != tc.env {
				t.Fatalf("got %s from %s, want %s from %s", tc.spec.Name, tc.spec.Env, tc.name, tc.env)
			}
			// The variable is not set: the default.
			got, err := tc.spec.FromEnv()
			if err != nil || got.Name != tc.name || got.Value != tc.want || got.Set() != (tc.want > 0) {
				t.Errorf("default: got %+v (%s), %v; want %s", got, got, err, tc.want)
			}
			for value, want := range map[string]string{"0": unset, unset: unset, "45s": "45s"} {
				t.Setenv(tc.env, value)
				got, err := tc.spec.FromEnv()
				if err != nil || got.String() != want || got.Set() != (want != unset) {
					t.Errorf("%s=%s: got %+v (%s), %v; want %s", tc.env, value, got, got, err, want)
				}
			}
			// What stops the suite before it starts.
			for _, value := range []string{"-5s", "45", "soon"} {
				t.Setenv(tc.env, value)
				if got, err := tc.spec.FromEnv(); err == nil {
					t.Errorf("%s=%s: got %+v, want an error", tc.env, value, got)
				}
			}
		})
	}
}

func TestJudge(t *testing.T) {
	lim := func(spec LimitSpec, value time.Duration) Limit { return Limit{Name: spec.Name, Value: value} }
	def := func(spec LimitSpec) Limit { return lim(spec, spec.Default) }
	within := func(l Limit) Bound { return Bound{Limit: l, Measured: l.Value / 2} }
	// A client acknowledged without interruption for longer than the stable
	// period of the defaults.
	stableFor := func(l Limit, d time.Duration) Bound { return Bound{Limit: l, Measured: d, AtLeast: true} }
	const stable = 32 * time.Second

	tests := []struct {
		name       string
		bounds     []Bound
		want       evidence.Result
		wantReason string
		wantLimits map[string]string
	}{
		{
			name: "S00: install_limit and ready_limit unset is a baseline",
			bounds: []Bound{{Limit: def(InstallLimit), Measured: 40 * time.Second},
				{Limit: def(ReadyLimit), Measured: 2 * time.Minute}},
			want: evidence.Blocked, wantReason: ReasonTimeLimitUnset,
			wantLimits: limitsOf("install_limit=unset ready_limit=unset"),
		},
		{
			name:       "S00: both set, both within",
			bounds:     []Bound{within(lim(InstallLimit, time.Minute)), within(lim(ReadyLimit, 5*time.Minute))},
			want:       evidence.Pass,
			wantLimits: limitsOf("install_limit=1m0s ready_limit=5m0s"),
		},
		{
			name: "S00: ready_limit exceeded",
			bounds: []Bound{within(lim(InstallLimit, time.Minute)),
				{Limit: lim(ReadyLimit, time.Minute), Measured: 61 * time.Second}},
			want: evidence.Fail, wantReason: "ready_limit exceeded: 1m1s > 1m0s",
		},
		{
			name: "S00: install_limit set, the install time not measured",
			bounds: []Bound{{Limit: lim(InstallLimit, time.Minute), Unmeasured: true},
				within(lim(ReadyLimit, 5*time.Minute))},
			want: evidence.Blocked, wantReason: ReasonMeasurementMissing + ": [install_limit]",
		},
		{
			name: "S00: ready_limit exceeded while install_limit is unset",
			bounds: []Bound{{Limit: def(InstallLimit), Measured: 40 * time.Second},
				{Limit: lim(ReadyLimit, time.Minute), Measured: 2 * time.Minute}},
			want: evidence.Fail, wantReason: "ready_limit exceeded: 2m0s > 1m0s",
		},
		{
			name: "S03: the defaults, within",
			bounds: []Bound{within(def(FailoverLimit)), stableFor(def(StablePeriod), stable),
				within(def(RejoinLimit))},
			want:       evidence.Pass,
			wantLimits: limitsOf("failover_limit=30s stable_period=30s rejoin_limit=5m0s"),
		},
		{
			name: "S03: a recovery exactly at the limit is within it",
			bounds: []Bound{{Limit: def(FailoverLimit), Measured: 30 * time.Second},
				stableFor(def(StablePeriod), 30*time.Second), within(def(RejoinLimit))},
			want: evidence.Pass,
		},
		{
			name: "S03: failover_limit zero is a baseline",
			bounds: []Bound{{Limit: lim(FailoverLimit, 0), Measured: time.Second},
				stableFor(def(StablePeriod), stable), within(def(RejoinLimit))},
			want: evidence.Blocked, wantReason: ReasonTimeLimitUnset,
			wantLimits: limitsOf("failover_limit=unset stable_period=30s rejoin_limit=5m0s"),
		},
		{
			name: "S03: stable_period zero is a baseline, not a pass",
			bounds: []Bound{within(def(FailoverLimit)), stableFor(lim(StablePeriod, 0), 0),
				within(def(RejoinLimit))},
			want: evidence.Blocked, wantReason: ReasonTimeLimitUnset,
			wantLimits: limitsOf("failover_limit=30s stable_period=unset rejoin_limit=5m0s"),
		},
		{
			name: "S03: stable_period not reached",
			bounds: []Bound{within(def(FailoverLimit)), stableFor(def(StablePeriod), 10*time.Second),
				within(def(RejoinLimit))},
			want: evidence.Fail, wantReason: "stable_period not reached: 10s < 30s",
		},
		{
			name: "S03: rejoin_limit exceeded",
			bounds: []Bound{within(def(FailoverLimit)), stableFor(def(StablePeriod), stable),
				{Limit: def(RejoinLimit), Measured: 6 * time.Minute}},
			want: evidence.Fail, wantReason: "rejoin_limit exceeded: 6m0s > 5m0s",
		},
		{
			name: "S03: failover_limit exceeded while rejoin_limit is unset stays a fail",
			bounds: []Bound{{Limit: def(FailoverLimit), Measured: 45 * time.Second},
				stableFor(def(StablePeriod), stable), {Limit: lim(RejoinLimit, 0), Measured: time.Minute}},
			want: evidence.Fail, wantReason: "failover_limit exceeded: 45s > 30s",
		},
		{
			name:       "S05: the defaults, within",
			bounds:     []Bound{within(def(BrokerRecoveryLimit)), stableFor(def(StablePeriod), stable)},
			want:       evidence.Pass,
			wantLimits: limitsOf("broker_recovery_limit=1m0s stable_period=30s"),
		},
		{
			name: "S05: broker_recovery_limit exceeded",
			bounds: []Bound{{Limit: def(BrokerRecoveryLimit), Measured: 90 * time.Second},
				stableFor(def(StablePeriod), stable)},
			want: evidence.Fail, wantReason: "broker_recovery_limit exceeded: 1m30s > 1m0s",
		},
		{
			name:   "S05: stable_period zero is a baseline",
			bounds: []Bound{within(def(BrokerRecoveryLimit)), stableFor(lim(StablePeriod, 0), 2*time.Second)},
			want:   evidence.Blocked, wantReason: ReasonTimeLimitUnset,
		},
		{
			name:   "S05: broker_recovery_limit zero is a baseline",
			bounds: []Bound{{Limit: lim(BrokerRecoveryLimit, 0), Measured: time.Second}, stableFor(def(StablePeriod), stable)},
			want:   evidence.Blocked, wantReason: ReasonTimeLimitUnset,
		},
		{
			name:       "S06 restart: the defaults, within",
			bounds:     []Bound{within(def(OperatorResyncLimit)), stableFor(def(StablePeriod), stable)},
			want:       evidence.Pass,
			wantLimits: limitsOf("operator_resync_limit=2m0s stable_period=30s"),
		},
		{
			name:   "S06 restart: stable_period zero is a baseline",
			bounds: []Bound{within(def(OperatorResyncLimit)), stableFor(lim(StablePeriod, 0), 2*time.Second)},
			want:   evidence.Blocked, wantReason: ReasonTimeLimitUnset,
		},
		{
			name: "S06 absent-during-failover: the defaults, within",
			bounds: []Bound{within(def(OperatorResyncLimit)), within(def(FailoverLimit)),
				stableFor(def(StablePeriod), stable)},
			want:       evidence.Pass,
			wantLimits: limitsOf("operator_resync_limit=2m0s failover_limit=30s stable_period=30s"),
		},
		{
			name: "S06 absent-during-failover: failover_limit zero is a baseline, not a fail",
			bounds: []Bound{within(def(OperatorResyncLimit)), {Limit: lim(FailoverLimit, 0), Measured: 2 * time.Second},
				stableFor(def(StablePeriod), stable)},
			want: evidence.Blocked, wantReason: ReasonTimeLimitUnset,
			wantLimits: limitsOf("operator_resync_limit=2m0s failover_limit=unset stable_period=30s"),
		},
		{
			name: "S06 absent-during-failover: operator_resync_limit exceeded",
			bounds: []Bound{{Limit: def(OperatorResyncLimit), Measured: 3 * time.Minute}, within(def(FailoverLimit)),
				stableFor(def(StablePeriod), stable)},
			want: evidence.Fail, wantReason: "operator_resync_limit exceeded: 3m0s > 2m0s",
		},
		{
			name: "S06 absent-during-failover: failover_limit exceeded",
			bounds: []Bound{within(def(OperatorResyncLimit)), {Limit: def(FailoverLimit), Measured: 31 * time.Second},
				stableFor(def(StablePeriod), stable)},
			want: evidence.Fail, wantReason: "failover_limit exceeded: 31s > 30s",
		},
		{
			name: "no limit at all is not a pass",
			want: evidence.Blocked, wantReason: ReasonTimeLimitUnset, wantLimits: map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Judge(tc.bounds...)
			if got.Result != tc.want || got.Reason != tc.wantReason {
				t.Errorf("got %s %q, want %s %q", got.Result, got.Reason, tc.want, tc.wantReason)
			}
			if tc.wantLimits != nil && !maps.Equal(got.Limits, tc.wantLimits) {
				t.Errorf("limits: got %v, want %v", got.Limits, tc.wantLimits)
			}
			// Every limit the run was judged by is recorded, and an unset one
			// never with a value.
			for _, b := range tc.bounds {
				recorded, ok := got.Limits[b.Limit.Name]
				if !ok || (!b.Limit.Set() && recorded != unset) {
					t.Errorf("%s recorded as %q (%v)", b.Limit.Name, recorded, ok)
				}
			}
			if got.Result == evidence.Pass && slices.Contains(slices.Collect(maps.Values(got.Limits)), unset) {
				t.Errorf("a pass with an unset limit: %v", got.Limits)
			}
		})
	}
}
