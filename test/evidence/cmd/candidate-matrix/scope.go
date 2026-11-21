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

// v01Scope classifies a recorded result against the experimental release
// claim adopted on 2026-11-21. This is only an inventory label: it cannot
// rejudge a historical scenario, waive a safety failure, or approve a release.
// The current candidate oracles for measured outcomes require independent
// VM/SQL review under #126. See docs/testing/scenario-contract.md.
type v01Scope string

const (
	scopeMustPass       v01Scope = "MUST PASS"
	scopeMustMeasure    v01Scope = "MEASURE + SAFETY REVIEW"
	scopeDeferred       v01Scope = "v0.2 DEFERRED"
	scopeConditional    v01Scope = "CONDITIONAL (NOT CLAIMED)"
	scopeUnmapped       v01Scope = "UNMAPPED (REVIEW REQUIRED)"
)

// scopeOf classifies the result already recorded in summary.json by its
// scenario family and variant. No result or numerical limit is changed.
// Unknown families and unknown S03 variants do not silently gain a v0.1 claim.
func scopeOf(sc scenario) v01Scope {
	switch sc.ID {
	case "S03":
		switch sc.Variant {
		case "abrupt", "abrupt-first-member":
			return scopeMustPass
		case "graceful":
			return scopeMustMeasure
		default:
			return scopeUnmapped
		}
	case "S04", "S07", "S08":
		return scopeMustMeasure
	case "S09":
		return scopeDeferred
	case "S16", "S17":
		return scopeConditional
	case "S00", "S01", "S02", "S05", "S06", "S10", "S11", "S12", "S13", "S14", "S15",
		"LAB00", "LAB01":
		return scopeMustPass
	default:
		return scopeUnmapped
	}
}
