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
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// ReasonMeasurementMissing is the reason a result is blocked when a limit is
// set but what it bounds could not be measured. Which measurement is missing
// is in the scenario's measurements, as "unknown".
const ReasonMeasurementMissing = "measurement_missing"

// Limit is a time limit a scenario is judged by, under the name the scenario
// contract gives it. A Value of zero means that the limit is not set: a run
// against it is a baseline.
type Limit struct {
	Name  string
	Value time.Duration
}

// Set reports whether a run can be scored against the limit.
func (l Limit) Set() bool { return l.Value > 0 }

// String is the limit as summary.json records it: its value, or "unset".
func (l Limit) String() string {
	if !l.Set() {
		return "unset"
	}
	return l.Value.String()
}

// LimitSpec names a limit, the environment variable that sets it and its
// default. A zero default means that the limit has no value yet.
type LimitSpec struct {
	Name    string
	Env     string
	Default time.Duration
}

// The limits of the Kind suite. Each default is the value for Kind on a
// GitHub-hosted runner that docs/testing/scenario-contract.md (section "Time
// limits") records, and zero where it records none; change a default there
// and here together.
var (
	// InstallLimit: the Operator installed until its Pod is available.
	InstallLimit = LimitSpec{"install_limit", "E2E_INSTALL_LIMIT", 0}
	// ReadyLimit: a single-member cluster created until SQL succeeds.
	ReadyLimit = LimitSpec{"ready_limit", "E2E_READY_LIMIT", 0}
	// FormationLimit: a three-member cluster created until all conditions
	// are True.
	FormationLimit = LimitSpec{"formation_limit", "E2E_FORMATION_LIMIT", 5 * time.Minute}
	// ReplicationLimit: a commit on the master until every slave returns
	// the row.
	ReplicationLimit = LimitSpec{"replication_limit", "E2E_REPLICATION_LIMIT", 30 * time.Second}
	// FailoverLimit: the fault until the client's writes are acknowledged
	// again without interruption.
	FailoverLimit = LimitSpec{"failover_limit", "E2E_FAILOVER_LIMIT", 30 * time.Second}
	// RejoinLimit: the fault until all three members have a role again.
	RejoinLimit = LimitSpec{"rejoin_limit", "E2E_REJOIN_LIMIT", 5 * time.Minute}
	// StablePeriod: how long the clients must go on being acknowledged
	// before their recovery counts.
	StablePeriod = LimitSpec{"stable_period", "E2E_STABLE_PERIOD", 30 * time.Second}
	// BrokerRecoveryLimit: Broker Pods killed until every client's writes
	// are acknowledged again.
	BrokerRecoveryLimit = LimitSpec{"broker_recovery_limit", "E2E_BROKER_RECOVERY_LIMIT", time.Minute}
	// OperatorResyncLimit: the Operator started until its status matches
	// fresh observations.
	OperatorResyncLimit = LimitSpec{"operator_resync_limit", "E2E_OPERATOR_RESYNC_LIMIT", 2 * time.Minute}
)

// FromEnv reads the limit from its environment variable, as LimitFromEnv
// reads it.
func (s LimitSpec) FromEnv() (Limit, error) {
	value, err := LimitFromEnv(s.Env, s.Default)
	if err != nil {
		return Limit{}, err
	}
	return Limit{Name: s.Name, Value: value}, nil
}

// LimitFromEnv returns the time limit a scenario is judged by: the value of
// the environment variable name when it is set, and def otherwise. The value
// is a Go duration such as "45s" or "2m"; "0" or "unset" means that the
// limit is not set, so that the scenario runs as a baseline and is reported
// as blocked. A value that is not a duration, or is negative, is an error:
// a mistyped limit must not silently become the default.
func LimitFromEnv(name string, def time.Duration) (time.Duration, error) {
	value, set := os.LookupEnv(name)
	if !set || value == "" {
		return def, nil
	}
	if value == "unset" {
		return 0, nil
	}
	limit, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a duration such as 45s or 2m: %w", name, value, err)
	}
	if limit < 0 {
		return 0, fmt.Errorf("%s=%q is negative", name, value)
	}
	return limit, nil
}

// Bound is one measurement of a run against its limit.
type Bound struct {
	Limit Limit
	// Measured is what the run measured.
	Measured time.Duration
	// Unmeasured is true when the measurement could not be taken; Measured
	// is then ignored.
	Unmeasured bool
	// AtLeast is true when the measurement must reach the limit, as the time
	// a client was acknowledged without interruption must reach
	// stable_period; otherwise it must not exceed the limit.
	AtLeast bool
}

// Verdict is what its limits make of a run whose checks all held.
type Verdict struct {
	Result evidence.Result
	Reason string
	// Limits are the limits the run was judged by, as summary.json records
	// them.
	Limits map[string]string
}

// Judge judges a run whose checks all held by its limits:
//
//   - a measurement beyond a set limit, or for AtLeast short of it, is a
//     fail, with the first such limit in the reason; a limit that is not set
//     does not hide one that was exceeded;
//   - otherwise a limit that is not set makes the run a baseline, blocked
//     with time_limit_unset: a run with an unset limit is never a pass;
//   - otherwise a set limit whose measurement could not be taken makes the
//     run blocked with measurement_missing;
//   - otherwise the run passes.
//
// A run judged by no limit at all is blocked: nothing scored it.
func Judge(bounds ...Bound) Verdict {
	v := Verdict{Limits: map[string]string{}}
	unset, unmeasured := false, false
	for _, b := range bounds {
		v.Limits[b.Limit.Name] = b.Limit.String()
		switch {
		case !b.Limit.Set():
			unset = true
		case b.Unmeasured:
			unmeasured = true
		case v.Result != "":
		case !b.AtLeast && b.Measured > b.Limit.Value:
			v.Result = evidence.Fail
			v.Reason = fmt.Sprintf("%s exceeded: %s > %s", b.Limit.Name, b.Measured, b.Limit.Value)
		case b.AtLeast && b.Measured < b.Limit.Value:
			v.Result = evidence.Fail
			v.Reason = fmt.Sprintf("%s not reached: %s < %s", b.Limit.Name, b.Measured, b.Limit.Value)
		}
	}
	switch {
	case v.Result != "":
	case len(bounds) == 0, unset:
		v.Result, v.Reason = evidence.Blocked, ReasonTimeLimitUnset
	case unmeasured:
		v.Result, v.Reason = evidence.Blocked, ReasonMeasurementMissing
	default:
		v.Result = evidence.Pass
	}
	return v
}

// JudgeS00Limits judges a run of S00 by install_limit and ready_limit. The
// limits are always recorded; only a run whose checks all held is judged by
// them.
//
// The limits that are set decide the result as Judge decides it. A limit
// that is not set does not: S00 is required in the kind lane, and blocking it
// would keep every run from passing the lane (see the open question on
// install_limit and ready_limit in docs/testing/scenario-contract.md, "Time
// limits"). What Judge makes of both limits is recorded as the measurement
// timeLimitVerdict, so the summary does not read as a run scored against
// them.
func JudgeS00Limits(result evidence.Scenario, install, ready Bound) evidence.Scenario {
	v := Judge(install, ready)
	if result.Limits == nil {
		result.Limits = map[string]string{}
	}
	maps.Copy(result.Limits, v.Limits)
	if result.Result != evidence.Pass {
		return result
	}
	if result.Measurements == nil {
		result.Measurements = map[string]any{}
	}
	result.Measurements["timeLimitVerdict"] = strings.TrimSuffix(string(v.Result)+": "+v.Reason, ": ")
	var set []Bound
	for _, b := range []Bound{install, ready} {
		if b.Limit.Set() {
			set = append(set, b)
		}
	}
	if len(set) > 0 {
		if scored := Judge(set...); scored.Result != evidence.Pass {
			result.Result, result.Reason = scored.Result, scored.Reason
		}
	}
	return result
}
