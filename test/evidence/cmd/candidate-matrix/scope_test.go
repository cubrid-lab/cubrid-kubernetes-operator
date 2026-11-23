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

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	s03Scenario    = "S03"
	s04Scenario    = "S04"
	s05Scenario    = "S05"
	s07Scenario    = "S07"
	unknownVariant = "not-a-real-variant"
)

func TestV01ScopeKeepsMandatoryAndMeasuredResultsSeparate(t *testing.T) {
	tests := []struct {
		id      string
		variant string
		scope   v01Scope
	}{
		{"S00", "", scopeMustPass},
		{"S01", "", scopeMustPass},
		{"S02", "", scopeMustPass},
		{s03Scenario, "abrupt-first-member", scopeMustPass},
		{s03Scenario, "abrupt", scopeMustPass},
		{s03Scenario, "graceful", scopeMustMeasure},
		{s04Scenario, "power-off", scopeMustMeasure},
		{s05Scenario, "one-broker-pod", scopeMustPass},
		{"S06", "restart", scopeMustPass},
		{s07Scenario, "higher-rank-isolated", scopeMustMeasure},
		{"S08", "", scopeMustMeasure},
		{"S09", "clean", scopeDeferred},
		{"S10", "", scopeMustPass},
		{"S11", "complete", scopeMustPass},
		{"S12", "exact", scopeMustPass},
		{"S13", "retain", scopeMustPass},
		{"S14", "token-rotation", scopeMustPass},
		{"S15", "drain-master-node", scopeMustPass},
		{"LAB00", "", scopeMustPass},
		{"LAB01", "", scopeMustPass},
		{"S16", "", scopeConditional},
		{"S17", "", scopeConditional},
		{s03Scenario, "", scopeUnmapped},
		{s03Scenario, "unexpected-variant", scopeUnmapped},
		{"S99", "", scopeUnmapped},
		{"", "", scopeUnmapped},
	}
	for _, tc := range tests {
		t.Run(tc.id+"/"+tc.variant, func(t *testing.T) {
			got := scopeOf(scenario{ID: tc.id, Variant: tc.variant})
			if got != tc.scope {
				t.Errorf("scopeOf(%s/%s) = %q, want %q", tc.id, tc.variant, got, tc.scope)
			}
		})
	}
}

// Reclassifying a scope never changes the raw verdict, including failure
// caused by the historical 30-second S04 limit. Measured is not a pass.
func TestV01ScopeDoesNotRejudgeHistoricalFailure(t *testing.T) {
	rec := scenario{ID: "S04", Variant: "power-off", Recorded: evidence.Fail, Judged: evidence.Fail}
	before := rec
	if got := scopeOf(rec); got != scopeMustMeasure {
		t.Fatalf("S04 scope = %q, want measured", got)
	}
	if rec.ID != before.ID || rec.Variant != before.Variant ||
		rec.Recorded != before.Recorded || rec.Judged != before.Judged ||
		verdict(rec.Recorded) != "FAIL" || verdict(rec.Judged) != "FAIL" {
		t.Fatal("the v0.1 scope mapping changed a historical failed verdict")
	}
}

// A family the registry holds results for accepts only the variants it
// registers. A mistyped or inapplicable variant is shown as unmapped rather
// than as a v0.1 requirement it does not match.
func TestV01ScopeShowsUnregisteredVariantsAsUnmapped(t *testing.T) {
	tests := []struct{ id, variant string }{
		{s05Scenario, unknownVariant},
		{s05Scenario, ""},
		{s07Scenario, unknownVariant},
		{s07Scenario, ""},
		{s04Scenario, unknownVariant},
		{"S06", unknownVariant},
		{"S11", unknownVariant},
		{"S12", unknownVariant},
		{"S13", unknownVariant},
		{"S14", unknownVariant},
		{"S15", unknownVariant},
		{"S10", "slave"},
		{"S00", "abrupt"},
		{"S01", "complete"},
		{"LAB01", "restart"},
		{s05Scenario, "graceful"},
		{s07Scenario, "drain-master-node"},
	}
	for _, tc := range tests {
		t.Run(tc.id+"/"+tc.variant, func(t *testing.T) {
			if got := scopeOf(scenario{ID: tc.id, Variant: tc.variant}); got != scopeUnmapped {
				t.Errorf("scopeOf(%s/%s) = %q, want %q", tc.id, tc.variant, got, scopeUnmapped)
			}
		})
	}
}

// Every variant the registry holds keeps a v0.1 category.
func TestV01ScopeMapsEveryRegisteredVariant(t *testing.T) {
	for _, level := range []string{evidence.LevelKind, evidence.LevelVMLab} {
		for _, key := range evidence.RegisteredKeys(level) {
			resolved, err := evidence.ResolveKey(key)
			if err != nil {
				t.Fatal(err)
			}
			for _, alias := range resolved.Aliases {
				id, variant, _ := strings.Cut(alias, "/")
				if got := scopeOf(scenario{ID: id, Variant: variant}); got == scopeUnmapped {
					t.Errorf("registered result %q (%s) is shown as unmapped", alias, key)
				}
			}
		}
	}
}
