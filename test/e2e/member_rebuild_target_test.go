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

import "testing"

// The rebuild step empties a slave ranked below the master whenever there is
// one, because a member without a database that outranks the master starts a
// write quarantine that never ends by itself. Only when every slave outranks
// the master, as when the earlier steps left hab-2 as master (#1058), does it
// empty one that outranks it and report that a manual release is needed.
func TestRebuildTargetIsRankedBelowTheMasterWhenPossible(t *testing.T) {
	members := []string{"hab-0", "hab-1", "hab-2"}
	for _, tc := range []struct {
		master, want   string
		slaves         []string
		outranksMaster bool
	}{
		{master: "hab-0", slaves: []string{"hab-1", "hab-2"}, want: "hab-1"},
		{master: "hab-1", slaves: []string{"hab-0", "hab-2"}, want: "hab-2"},
		{master: "hab-2", slaves: []string{"hab-0", "hab-1"}, want: "hab-0", outranksMaster: true},
		// A master that is not a known member gives no ranking to rely on, so
		// the step expects the quarantine and releases it.
		{master: "other", slaves: []string{"hab-0", "hab-1"}, want: "hab-0", outranksMaster: true},
	} {
		got, outranks := rebuildTarget(tc.master, tc.slaves, members)
		if got != tc.want || outranks != tc.outranksMaster {
			t.Errorf("master %s, slaves %v: got %s (outranks the master: %t), want %s (%t)",
				tc.master, tc.slaves, got, outranks, tc.want, tc.outranksMaster)
		}
	}
}
