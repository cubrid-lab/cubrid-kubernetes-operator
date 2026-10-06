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

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

// The time limits the scenarios are judged by. Each has a default, which is
// the value for Kind on a GitHub-hosted runner, and can be set for another
// environment through its variable, as a Go duration such as "45s" or "2m".
// "0" or "unset" runs the scenario as a baseline: it is then reported as
// blocked with reason time_limit_unset, never as a pass.
//
// The defaults and the baselines they come from are recorded in
// docs/testing/scenario-contract.md, section "Time limits"; change a default
// there and here together.
var (
	// formationLimit: a three-member cluster created until all conditions are True.
	formationLimit = limit("E2E_FORMATION_LIMIT", 5*time.Minute)
	// replicationLimit: a commit on the master until every slave returns the row.
	replicationLimit = limit("E2E_REPLICATION_LIMIT", 30*time.Second)
	// failoverLimit: the fault until the client's writes are acknowledged
	// again without interruption.
	failoverLimit = limit("E2E_FAILOVER_LIMIT", 30*time.Second)
	// rejoinLimit: the fault until all three members have a role again.
	rejoinLimit = limit("E2E_REJOIN_LIMIT", 5*time.Minute)
	// stablePeriod: how long the client must go on being acknowledged before
	// its recovery counts.
	stablePeriod = limit("E2E_STABLE_PERIOD", 30*time.Second)
	// brokerRecoveryLimit: Broker Pods killed until every client's writes
	// are acknowledged again.
	brokerRecoveryLimit = limit("E2E_BROKER_RECOVERY_LIMIT", time.Minute)
	// operatorResyncLimit: the Operator started until its status matches
	// fresh observations.
	operatorResyncLimit = limit("E2E_OPERATOR_RESYNC_LIMIT", 2*time.Minute)
)

// limit reads one limit. A value that cannot be read stops the suite before
// it starts: a mistyped limit must not silently become the default.
func limit(name string, def time.Duration) time.Duration {
	value, err := faults.LimitFromEnv(name, def)
	if err != nil {
		panic(err)
	}
	return value
}
