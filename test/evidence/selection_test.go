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

package evidence

import (
	"slices"
	"strings"
	"testing"

	"github.com/onsi/ginkgo/v2/types"
)

const (
	haSetupLabel  = "ha-setup"
	dbLabel       = "db"
	gracefulLabel = "S03-graceful"
	haBootstrap   = "ha-bootstrap"
	s02Label      = "S02"
	s04Label      = "S04"
)

// kindSuiteSteps returns the labels of the steps of a suite shaped like the
// Kind suite: every step of a registered Kind result labelled the way the
// suite labels it, with KindLabels, and the steps that are no scenario's.
func kindSuiteSteps() [][]string {
	steps := [][]string{
		{"fake-instance-manager"},
		{"placement"},
		KindLabels(dbLabel, s00),
		{dbLabel, haBootstrap, haSetupLabel},
		KindLabels(dbLabel, haBootstrap, "S01"),
		KindLabels(dbLabel, haBootstrap, s02Label),
		KindLabels("S14"),
	}
	for _, entry := range registry.entries {
		family, variant, ok := strings.Cut(entry.Aliases[0], "/")
		if entry.Level != LevelKind || !ok {
			continue
		}
		label := Requirement{ID: family, Variant: variant}.Label()
		if family == "S14" {
			steps = append(steps, KindLabels(label))
			continue
		}
		steps = append(steps, KindLabels(dbLabel, haBootstrap, family, label))
	}
	return steps
}

// Each Kind result's descriptive key is added to the steps that carry its
// legacy label, and each family with a Kind result gets its key beside its
// ID. Other labels are kept as they are, and a family with no Kind result
// gets no key.
func TestKindLabels(t *testing.T) {
	for _, entry := range registry.entries {
		if entry.Level != LevelKind {
			continue
		}
		family, variant, _ := strings.Cut(entry.Aliases[0], "/")
		label := Requirement{ID: family, Variant: variant}.Label()
		if got := KindLabels(label); !slices.Equal(got, []string{label, entry.Key}) {
			t.Errorf("KindLabels(%s) = %v, want [%s %s]", label, got, label, entry.Key)
		}
	}
	if got := KindLabels(dbLabel, s03, gracefulLabel); !slices.Equal(got,
		[]string{dbLabel, s03, "ha-primary-pod-recovery", gracefulLabel, gracefulKey}) {
		t.Errorf("KindLabels(db, S03, S03-graceful) = %v", got)
	}
	for _, label := range []string{haSetupLabel, s04Label, "S11", "LAB01", s99} {
		if got := KindLabels(label); !slices.Equal(got, []string{label}) {
			t.Errorf("KindLabels(%s) = %v, want only the label", label, got)
		}
	}
}

// Every key selects exactly the steps its legacy label selects, through
// Ginkgo's own label matching, and a step that carries one without the other
// is a problem.
func TestKeyLabelProblems(t *testing.T) {
	steps := kindSuiteSteps()
	if problems := KeyLabelProblems(steps); len(problems) != 0 {
		t.Fatalf("problems of a suite labelled with KindLabels:\n%s", strings.Join(problems, "\n"))
	}

	cases := map[string]struct {
		steps [][]string
		want  string
	}{
		"a step with the legacy label only": {
			append(slices.Clone(steps), []string{gracefulLabel}),
			"the key ha-primary-pod-recovery-graceful selects 1 step(s) and its legacy label S03-graceful 2",
		},
		"a step with the key only": {
			append(slices.Clone(steps), []string{"ha-replication"}),
			"the key ha-replication selects 2 step(s) and its legacy label S02 1",
		},
		"a family key on a step without the family ID": {
			append(slices.Clone(steps), []string{"broker-failure-recovery"}),
			"the key broker-failure-recovery selects 4 step(s) and its legacy label S05 3",
		},
		"a result no step carries": {
			slices.DeleteFunc(slices.Clone(steps), func(labels []string) bool {
				return slices.Contains(labels, gracefulLabel)
			}),
			"no step carries ha-primary-pod-recovery-graceful or its legacy label S03-graceful",
		},
	}
	for name, tc := range cases {
		problems := KeyLabelProblems(tc.steps)
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, tc.want) }) {
			t.Errorf("%s: problems %q, want %q among them", name, problems, tc.want)
		}
	}
}

// A label filter whose every term names labels of the suite and that selects
// a step is accepted; an unknown, misspelled, VM-lab, family-only, slash-form
// or empty selection fails with a reason, never selecting nothing quietly.
func TestCheckLabelFilter(t *testing.T) {
	steps := kindSuiteSteps()
	for _, filter := range []string{
		s02Label, "ha-replication", "HA-Replication",
		"ha-setup || S03-graceful", "ha-setup || ha-primary-pod-recovery-graceful",
		"ha-primary-pod-recovery", s03, "!db", "db && !S03", "/token/", "(S00, S01)", "/a:b|token/",
	} {
		if err := CheckLabelFilter(filter, steps); err != nil {
			t.Errorf("CheckLabelFilter(%q) = %v, want accepted", filter, err)
		}
	}

	rejected := map[string]string{
		s99:                         "no step of the Kind suite carries the label S99",
		"ha-replicaton":             "no step of the Kind suite carries the label ha-replicaton",
		"S00 || S99":                "no step of the Kind suite carries the label S99",
		"installed-backup-complete": "installed-backup-complete is a VM-lab check",
		"S11-complete":              "S11-complete is a VM-lab check",
		s04Label:                    "S04 has no Kind check",
		"ha-network-partition":      "ha-network-partition is the family S07, which has no Kind check",
		"S06/restart":               "write the legacy name S06/restart as the label S06-restart",
		"/S99/":                     "the pattern /S99/ matches no label of the Kind suite",
		"os: consumes linux":        "label sets",
		"S00 && S01":                "selects no step of the Kind suite",
		"":                          "",
	}
	for filter, want := range rejected {
		err := CheckLabelFilter(filter, steps)
		switch {
		case filter == "" && err != nil:
			t.Errorf("CheckLabelFilter(\"\") = %v, want no check of an unfiltered run", err)
		case filter == "":
		case err == nil:
			t.Errorf("CheckLabelFilter(%q) accepted, want an error with %q", filter, want)
		case !strings.Contains(err.Error(), want):
			t.Errorf("CheckLabelFilter(%q) = %q, want it to contain %q", filter, err, want)
		}
	}
}

// A run selected by a key requires what the same run selected by the legacy
// label requires, and is judged as kind-filtered: it passes with the selected
// result alone, the full kind lane does not pass on the same summary, and a
// selected result that was skipped, missing, failed, blocked or has no
// evidence still fails the filtered lane. runLane in test/e2e/summary_test.go
// requires a scenario when a selected step carries its label, as here.
func TestAKeySelectedRunIsJudgedAsTheFilteredLane(t *testing.T) {
	steps := kindSuiteSteps()
	kindLane, _ := registry.lane(kindLaneName)
	all := make([]Requirement, 0, len(kindLane.Keys))
	for _, key := range kindLane.Keys {
		entry, err := ResolveKey(key)
		if err != nil {
			t.Fatal(err)
		}
		id, variant, _ := strings.Cut(entry.Aliases[0], "/")
		all = append(all, Requirement{ID: id, Variant: variant})
	}
	selectedBy := func(filter string) []Requirement {
		if err := CheckLabelFilter(filter, steps); err != nil {
			t.Fatal(err)
		}
		selects := types.MustParseLabelFilter(filter)
		var reqs []Requirement
		for _, req := range all {
			if slices.ContainsFunc(steps, func(labels []string) bool {
				return selects(labels) && slices.Contains(labels, req.Label())
			}) {
				reqs = append(reqs, req)
			}
		}
		return reqs
	}
	byKey := selectedBy(haSetupLabel + " || " + gracefulKey)
	byLabel := selectedBy(haSetupLabel + " || " + gracefulLabel)
	gracefulReq := Requirement{ID: s03, Variant: graceful}
	if !slices.Equal(byKey, byLabel) || !slices.Equal(byKey, []Requirement{gracefulReq}) {
		t.Fatalf("the key selected %v and the legacy label %v, want only %v", byKey, byLabel, gracefulReq)
	}

	dir := runDir(t, s03History)
	filtered := Lane{Name: filteredKindLane, Required: byKey}
	summary := func(results ...Scenario) Summary {
		var s Summary
		s.Plan(all, "not selected by the run's focus or label filter")
		for _, sc := range results {
			s.Record(sc)
		}
		return s
	}
	result := func(r Result, reason string, evidence ...string) Scenario {
		return Scenario{ID: s03, Variant: graceful, Result: r, Reason: reason, Evidence: evidence}
	}
	passed := result(Pass, "", s03History)
	if problems := filtered.Gate(summary(passed), dir); len(problems) != 0 {
		t.Errorf("the filtered lane with its selected result passed: problems %q, want none", problems)
	}
	if problems := (Lane{Name: kindLaneName, Required: all}).Gate(summary(passed), dir); len(problems) == 0 {
		t.Error("the full kind lane passed on a run that selected one result")
	}
	missing := summary()
	missing.Scenarios = slices.DeleteFunc(missing.Scenarios, func(sc Scenario) bool {
		return sc.ID == s03 && sc.Variant == graceful
	})
	for name, s := range map[string]Summary{
		"skipped":   summary(result(NotRun, "skipped")),
		"missing":   missing,
		"a failure": summary(result(Fail, "x", s03History)),
		"a block":   summary(result(Blocked, setupFailed)),
		"no proof":  summary(result(Pass, "")),
	} {
		if problems := filtered.Gate(s, dir); len(problems) == 0 {
			t.Errorf("%s: the filtered lane passed", name)
		}
	}
}
