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
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/onsi/ginkgo/v2/types"
)

// labelKey is a Ginkgo label the Kind suite has always used for a result or a
// family ("S03-graceful", "S03") and the descriptive key that selects the same
// steps ("ha-primary-pod-recovery-graceful", "ha-primary-pod-recovery").
type labelKey struct{ label, key string }

// kindLabelKeys returns the legacy label and key of every Kind result, and of
// every family with a Kind result, in the registry's order. A family without
// variants has its result's key, so its label appears once.
func (r scenarioRegistry) kindLabelKeys() []labelKey {
	var pairs []labelKey
	add := func(label, key string) {
		if !slices.ContainsFunc(pairs, func(p labelKey) bool { return p.label == label }) {
			pairs = append(pairs, labelKey{label, key})
		}
	}
	for _, family := range r.families {
		if slices.ContainsFunc(r.entries, func(e ScenarioEntry) bool {
			return e.Family == family.ID && e.Level == LevelKind
		}) {
			add(family.ID, family.Key)
		}
	}
	for _, entry := range r.entries {
		if entry.Level != LevelKind {
			continue
		}
		for _, alias := range entry.Aliases {
			id, variant, _ := strings.Cut(alias, "/")
			add(Requirement{ID: id, Variant: variant}.Label(), entry.Key)
		}
	}
	return pairs
}

// KindLabels returns the Ginkgo labels of a step of the Kind suite: the given
// labels, each legacy scenario label followed by its descriptive key, so that
// E2E_LABEL_FILTER=ha-replication selects what E2E_LABEL_FILTER=S02 does. The
// legacy labels stay: gates, evidence paths and E2E_BREAK_STEP use them.
func KindLabels(labels ...string) []string {
	pairs := registry.kindLabelKeys()
	out := make([]string, 0, 2*len(labels))
	for _, label := range labels {
		out = append(out, label)
		i := slices.IndexFunc(pairs, func(p labelKey) bool { return p.label == label })
		if i >= 0 && !slices.Contains(labels, pairs[i].key) {
			out = append(out, pairs[i].key)
		}
	}
	return out
}

// KeyLabelProblems returns a problem for each Kind result or family whose key
// does not select exactly the steps its legacy label selects, given the labels
// of every step of the suite, and for each that no step carries at all.
// Selection uses Ginkgo's own label matching.
func KeyLabelProblems(steps [][]string) []string {
	var problems []string
	for _, pair := range registry.kindLabelKeys() {
		byKey, byLabel := types.MustParseLabelFilter(pair.key), types.MustParseLabelFilter(pair.label)
		var keySteps, labelSteps []int
		for i, labels := range steps {
			if byKey(labels) {
				keySteps = append(keySteps, i)
			}
			if byLabel(labels) {
				labelSteps = append(labelSteps, i)
			}
		}
		switch {
		case len(keySteps) == 0 && len(labelSteps) == 0:
			problems = append(problems,
				fmt.Sprintf("no step carries %s or its legacy label %s", pair.key, pair.label))
		case !slices.Equal(keySteps, labelSteps):
			problems = append(problems, fmt.Sprintf(
				"the key %s selects %d step(s) and its legacy label %s %d; they must select the same steps",
				pair.key, len(keySteps), pair.label, len(labelSteps)))
		}
	}
	return problems
}

// CheckLabelFilter returns an error unless every label and pattern of a Kind
// suite label filter names a label one of the given steps carries, and the
// filter selects at least one of them. Without this check Ginkgo runs a
// misspelled or unknown label as a filter that selects nothing. The error says
// when a name belongs to a VM-lab check or to a family with no Kind check. An
// empty filter is an unfiltered run and is not checked.
func CheckLabelFilter(filter string, steps [][]string) error {
	if strings.TrimSpace(filter) == "" {
		return nil
	}
	var problems []string
	for _, entry := range registry.entries {
		for _, alias := range entry.Aliases {
			id, variant, ok := strings.Cut(alias, "/")
			if ok && strings.Contains(filter, alias) {
				problems = append(problems, fmt.Sprintf(
					"%s is not a Ginkgo label: write the legacy name %s as the label %s, or use the key of the Kind check",
					alias, alias, Requirement{ID: id, Variant: variant}.Label()))
			}
		}
	}
	if len(problems) > 0 {
		return selectionError(filter, slices.Compact(problems))
	}
	if strings.Contains(filter, ":") {
		return selectionError(filter, []string{"label sets (key: operation) are not used by the Kind suite"})
	}
	selects, err := types.ParseLabelFilter(filter)
	if err != nil {
		return selectionError(filter, []string{err.Error()})
	}
	carried := func(term string) bool {
		match := types.MustParseLabelFilter(term)
		return slices.ContainsFunc(steps, func(labels []string) bool { return match(labels) })
	}
	for _, term := range filterTerms(filter) {
		switch {
		case strings.HasPrefix(term, "/") && !carried(term):
			problems = append(problems, fmt.Sprintf("the pattern %s matches no label of the Kind suite", term))
		case !strings.HasPrefix(term, "/") && !carried(term):
			problems = append(problems, unknownLabel(term))
		}
	}
	if len(problems) == 0 && !slices.ContainsFunc(steps, func(labels []string) bool { return selects(labels) }) {
		problems = append(problems, "it selects no step of the Kind suite")
	}
	if len(problems) > 0 {
		return selectionError(filter, problems)
	}
	return nil
}

func selectionError(filter string, problems []string) error {
	return errors.New("label filter " + fmt.Sprintf("%q", filter) + ": " + strings.Join(problems, "; "))
}

// filterTerms returns the labels and the /patterns/ of a label filter, the way
// Ginkgo's parser splits it: at &&, ||, !, ",", parentheses and patterns.
func filterTerms(filter string) []string {
	var terms []string
	var current strings.Builder
	flush := func() {
		if term := strings.TrimSpace(current.String()); term != "" {
			terms = append(terms, term)
		}
		current.Reset()
	}
	for i := 0; i < len(filter); i++ {
		switch c := filter[i]; c {
		case '&', '|', '!', ',', '(', ')':
			flush()
		case '/':
			flush()
			end := strings.IndexByte(filter[i+1:], '/')
			if end < 0 {
				end = len(filter) - i - 1
			}
			terms = append(terms, "/"+filter[i+1:i+1+end]+"/")
			i += end + 1
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return terms
}

// unknownLabel says why no step of the Kind suite carries a label: it names a
// VM-lab check, a family with no Kind check, or nothing registered.
func unknownLabel(term string) string {
	for _, entry := range registry.entries {
		if entry.Level != LevelVMLab {
			continue
		}
		for _, alias := range entry.Aliases {
			id, variant, _ := strings.Cut(alias, "/")
			if strings.EqualFold(term, entry.Key) {
				return fmt.Sprintf("%s is a VM-lab check; the Kind suite does not run it", term)
			}
			if strings.EqualFold(term, Requirement{ID: id, Variant: variant}.Label()) {
				return fmt.Sprintf("%s is a VM-lab check (%s); the Kind suite does not run it", term, entry.Key)
			}
		}
	}
	for _, family := range registry.families {
		switch {
		case strings.EqualFold(term, family.ID):
			return fmt.Sprintf("%s has no Kind check", term)
		case strings.EqualFold(term, family.Key):
			return fmt.Sprintf("%s is the family %s, which has no Kind check", term, family.ID)
		}
	}
	return "no step of the Kind suite carries the label " + term
}
