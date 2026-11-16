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
	"fmt"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

type s03RecoveryProbe struct {
	activeMasters          func(context.Context) []string
	master                 func(context.Context) (string, []string, error)
	status                 func(context.Context) (statusSnapshot, error)
	formationTime          func(context.Context) (time.Duration, error)
	lastAnswerAcknowledged func(context.Context) bool
	now                    func() time.Time
}

type s03RecoveryState struct {
	newMaster            string
	electedAt, noticedAt time.Time
}

// newS03RecoveryCheck is the single recovery callback used by the Pod-deletion
// scenario and its finite observation tests.
func newS03RecoveryCheck(probe s03RecoveryProbe, deletedMember, originalUID string) (
	func(context.Context) (bool, error), *s03RecoveryState) {
	state := &s03RecoveryState{}
	return func(ctx context.Context) (bool, error) {

		// The Operator's own view is read by the sampler, and judged after
		// the run (conditions_test.go): it must never offer the write
		// endpoint while the primary is not resolved (ADR-0005, section 6).
		// Two members that are active masters at once must never happen.
		// One sweep asks the members one after another and can see both
		// sides of a handover, so a second sweep confirms it.
		masters := probe.activeMasters(ctx)
		if len(masters) > 1 {
			if err := faults.CheckOneMaster(masters, probe.activeMasters(ctx)); err != nil {
				return false, err
			}
			return false, nil // a handover seen mid-sweep
		}
		// CUBRID's election: a member reports itself an active master.
		// It is another member, or the deleted one in its new Pod: the
		// fault is confirmed, so the old Pod no longer answers.
		if state.newMaster == "" {
			if len(masters) == 0 {
				return false, nil
			}
			state.newMaster = masters[0]
			state.electedAt = probe.now()
		}
		// The Operator noticing it.
		if state.noticedAt.IsZero() {
			status, err := probe.status(ctx)
			if err != nil || status.primary != state.newMaster {
				return false, nil
			}
			state.noticedAt = probe.now()
		}
		// One master and two slaves again. A master other than the
		// elected one is a second change of role, which must not happen
		// by itself.
		now, _, err := probe.master(ctx)
		if err != nil {
			return false, nil
		}
		if now != state.newMaster {
			return false, fmt.Errorf("the master changed again, from %s to %s", state.newMaster, now)
		}
		if _, err := probe.formationTime(ctx); err != nil {
			return false, nil // a condition of the starting state is not True yet
		}
		// The client recovered: its latest answer is an acknowledgement.
		return probe.lastAnswerAcknowledged(ctx), nil
	}, state
}
