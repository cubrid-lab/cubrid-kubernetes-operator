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

package controller

import (
	"testing"
	"time"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

func FuzzQuarantineAutomaticRelease(f *testing.F) {
	const (
		oneFreshMaster uint8 = iota
		noMaster
		twoMasters
		staleMaster
	)
	const (
		stablePod uint8 = iota
		unreadablePods
		restartedMaster
	)
	// Standard go test exercises every combination at the release boundary
	// and just before it. All inputs describe one bounded observation.
	for master := oneFreshMaster; master <= staleMaster; master++ {
		for identity := stablePod; identity <= restartedMaster; identity++ {
			for _, multiple := range []bool{false, true} {
				f.Add(master, identity, multiple, false)
				f.Add(master, identity, multiple, true)
			}
		}
	}

	f.Fuzz(func(t *testing.T, master, identity uint8, multiple, intervalReached bool) {
		if master > staleMaster || identity > restartedMaster {
			t.Skip()
		}
		s := startUnverified(t)
		if multiple {
			s = newQuarantineScenario(t, q1)
			obs := releasable(testNow)
			obs[q0] = activeMasterAt(testNow)
			d := s.pass(testNow, obs)
			if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMultipleMasters {
				t.Fatalf("failed to establish a multiple-master quarantine: %+v", d)
			}
		}
		first := testNow.Add(3 * time.Second)
		s.pass(first, releasable(first))
		ready := first.Add(quarantineVerifyGap + time.Second)
		before := s.pass(ready, releasable(ready))
		if before.Quarantine == nil || (!multiple && before.Quarantine.ReleaseCheckSince == nil) {
			t.Fatalf("failed to establish release-check state: %+v", before)
		}
		now := ready.Add(quarantineReleaseInterval)
		if !intervalReached {
			now = now.Add(-time.Second)
		}
		obs := releasable(now)
		switch master {
		case noMaster:
			obs[q1] = noAnswerAt(now)
		case twoMasters:
			obs[q2] = activeMasterAt(now)
		case staleMaster:
			obs[q1] = activeMasterAt(now.Add(-time.Hour))
		}
		switch identity {
		case unreadablePods:
			s.identitiesUnknown = true
		case restartedMaster:
			s.identities[q1] = memberIdentity{PodUID: uid1, RestartCount: 1}
		}
		d := s.pass(now, obs)
		wantRelease := master == oneFreshMaster && identity == stablePod && !multiple && intervalReached
		if wantRelease {
			if d.Quarantine != nil || d.Released == nil ||
				d.Released.Method != databasev1alpha1.WriteQuarantineReleasedAutomatically {
				t.Fatalf("expected release with sufficient current evidence: %+v", d)
			}
		} else if d.Quarantine == nil || d.Released != nil {
			t.Fatalf("quarantine released with an unsatisfied condition: %+v", d)
		}
	})
}
