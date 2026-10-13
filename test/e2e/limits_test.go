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
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

// The time limits the scenarios are judged by, read once when the suite
// starts. Their names, variables and defaults are in test/faults
// (limits.go); the values and the baselines they come from are recorded in
// docs/testing/scenario-contract.md, section "Time limits". "0" or "unset"
// runs a scenario as a baseline: it is then reported as blocked with reason
// time_limit_unset, never as a pass.
var (
	installLimit        = limit(faults.InstallLimit)
	readyLimit          = limit(faults.ReadyLimit)
	formationLimit      = limit(faults.FormationLimit)
	replicationLimit    = limit(faults.ReplicationLimit)
	failoverLimit       = limit(faults.FailoverLimit)
	rejoinLimit         = limit(faults.RejoinLimit)
	stablePeriod        = limit(faults.StablePeriod)
	brokerRecoveryLimit = limit(faults.BrokerRecoveryLimit)
	operatorResyncLimit = limit(faults.OperatorResyncLimit)
)

// limit reads one limit. A value that cannot be read stops the suite before
// it starts: a mistyped limit must not silently become the default.
func limit(spec faults.LimitSpec) faults.Limit {
	value, err := spec.FromEnv()
	if err != nil {
		panic(err)
	}
	return value
}

// judged gives a scenario whose checks all held its result by its limits,
// as faults.Judge decides it.
func judged(s evidence.Scenario, bounds ...faults.Bound) evidence.Scenario {
	v := faults.Judge(bounds...)
	s.Result, s.Reason, s.Limits = v.Result, v.Reason, v.Limits
	return s
}

// within is a measurement that must not exceed its limit.
func within(l faults.Limit, measured time.Duration) faults.Bound {
	return faults.Bound{Limit: l, Measured: measured}
}

// stableFor is how long the clients were acknowledged without
// interruption, which must reach stable_period.
func stableFor(measured time.Duration) faults.Bound {
	return faults.Bound{Limit: stablePeriod, Measured: measured, AtLeast: true}
}
