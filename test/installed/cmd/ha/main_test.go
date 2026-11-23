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
)

const (
	vmLab      = "--level=vm-lab"
	haSQLSmoke = "-check=installed-ha-sql-smoke"
	// rebuildLimit sets rebuild_limit, which judges S10 alone.
	rebuildLimit = "-rebuild-limit=10m"
)

// A check the HA command does not run, or one combined with a legacy option
// that selects a check, is refused before any cluster is opened.
func TestAWrongCheckIsRefusedBeforeACluster(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		reason string
	}{
		{[]string{vmLab, "-check=operator-failure-recovery-restart"}, "Kind check"},
		{[]string{vmLab, "-check=installed-backup"}, "family"},
		{[]string{vmLab, "-check="}, "unknown scenario key"},
		{[]string{"--level=kind", haSQLSmoke}, "-level vm-lab"},
		{[]string{vmLab, "-check=S06/restart"}, "unknown scenario key"},
		{[]string{vmLab, "-check=installed-standalone-sql-smoke"}, "installed smoke command"},
		{[]string{vmLab, "-check=installed-backup-complete"}, "needs -backup-bucket"},
		{[]string{vmLab, haSQLSmoke, "-backup-bucket=b"}, "remove -backup-bucket"},
		{[]string{vmLab, haSQLSmoke, "-fault=operator-restart"}, "remove -fault"},
		{[]string{vmLab, "-check=installed-restore-new-cluster-exact", "-backup-bucket=b", "-restore"},
			"remove -restore"},
	} {
		err := run(t.Context(), tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%q = %v, want a refusal with %q", tc.args, err, tc.reason)
		}
	}
}

// A soak is selected only by a positive -soak-duration: zero or a negative
// length is refused, not read as no soak, and an interval without a soak is
// refused, before any cluster is opened.
func TestASoakWithoutAPositiveDurationIsRefusedBeforeACluster(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		reason string
	}{
		{[]string{vmLab, "-soak-duration=0"}, "-soak-duration must be positive"},
		{[]string{vmLab, "-soak-duration=-1h"}, "-soak-duration must be positive"},
		{[]string{vmLab, "-soak-check-every=10m"}, "-soak-check-every needs -soak-duration"},
		{[]string{vmLab, haSQLSmoke, "-soak-duration=24h"}, "remove -soak-duration"},
	} {
		err := run(t.Context(), tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%q = %v, want a refusal with %q", tc.args, err, tc.reason)
		}
	}
}

// rebuild_limit judges S10 alone: -rebuild-limit without the fault that
// selects S10 is refused before any cluster is opened.
func TestARebuildLimitWithoutTheRebuildIsRefusedBeforeACluster(t *testing.T) {
	for _, args := range [][]string{
		{vmLab, rebuildLimit},
		// An explicit 0, which leaves the limit unset, is refused the same way.
		{vmLab, "-rebuild-limit=0"},
		{vmLab, rebuildLimit, "-fault=operator-restart"},
		{vmLab, haSQLSmoke, rebuildLimit},
	} {
		err := run(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "-rebuild-limit needs -fault pvc-loss-rebuild") {
			t.Errorf("%q = %v, want a refusal of -rebuild-limit", args, err)
		}
	}
}
