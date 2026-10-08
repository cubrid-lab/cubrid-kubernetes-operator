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
	"slices"
)

// UnknownIdentity is what a caller records for a member whose identity could
// not be read. It never matches, so a member that could not be observed
// counts as changed.
const UnknownIdentity = "unknown"

// confirmedMasters returns the members that two consecutive sweeps both
// report as an active master. The members of one sweep are asked one after
// another, so a single sweep can see the old master before and the new one
// after a handover; a member reported in both sweeps is not such an artifact.
// More than one confirmed master is two masters at once.
func confirmedMasters(first, second []string) []string {
	var both []string
	for _, m := range first {
		if slices.Contains(second, m) && !slices.Contains(both, m) {
			both = append(both, m)
		}
	}
	return both
}

// CheckOneMaster fails when two consecutive sweeps confirm more than one
// active master.
func CheckOneMaster(first, second []string) error {
	if both := confirmedMasters(first, second); len(both) > 1 {
		return fmt.Errorf("more than one active master in two consecutive sweeps: %v", both)
	}
	return nil
}

// CheckUnchanged fails when a member's identity (for example its Pod UID and
// container restart count) differs between before and after, is missing from
// after, or could not be read in either. It checks the members of before:
// the ones a fault was not meant to touch.
func CheckUnchanged(before, after map[string]string) error {
	var changed []string
	for member, was := range before {
		now, ok := after[member]
		if !ok || was == UnknownIdentity || now == UnknownIdentity || now != was {
			changed = append(changed, fmt.Sprintf("%s: %s -> %s", member, was, orMissing(now, ok)))
		}
	}
	if len(changed) > 0 {
		slices.Sort(changed)
		return fmt.Errorf("members the fault did not touch were replaced or restarted, or could not be observed: %v",
			changed)
	}
	return nil
}

func orMissing(v string, ok bool) string {
	if !ok {
		return "missing"
	}
	return v
}
