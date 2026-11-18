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

package hack

// These tests check the document roles of AGENTS.md, "Where each kind of text
// belongs" (#991): DESIGN.md describes present behaviour only, so project
// phase and status wording fails, and every ADR file must be listed in both
// ADR indexes. `make check-docs` runs them; `make verify` and CI run that.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const repoRoot = ".."

// designStatusPatterns is the list of project-phase and status wording that
// DESIGN.md must not contain. Matching is case-insensitive and per line.
// There is no escape: status belongs in ROADMAP.md, so a matching sentence is
// reworded as present behaviour or a non-goal, or dropped. The patterns are
// narrow on purpose, so that API terms such as the `phase` status field,
// `status.currentPrimary` or a planned failover still pass.
var designStatusPatterns = []struct {
	pattern *regexp.Regexp
	reason  string
}{
	{regexp.MustCompile(`(?i)\bphase\s+[0-9]`), "a numbered project phase"},
	{regexp.MustCompile(`(?i)\b(later|earlier|next|future|first|initial)\s+phase\b`), "a relative project phase"},
	{regexp.MustCompile(`(?i)\bnot\s+(yet\s+)?implemented\b`), "implementation status"},
	{regexp.MustCompile(`(?i)\bnot\s+yet\s+(supported|available|done|wired|built|validated)\b`), "implementation status"},
	{regexp.MustCompile(`(?i)\bimplementation\s+(status|gate)\b`), "implementation status"},
	{regexp.MustCompile(`(?i)\b(is|are|was|were|been)\s+planned\b|\bplanned\s+for\b`), "planned work"},
	{regexp.MustCompile(`(?i)\b(TODO|FIXME|TBD)\b`), "an open work marker"},
	{regexp.MustCompile(`(?i)\bMVP\b`), "release scope"},
	{regexp.MustCompile(`(?i)\bv0\.[0-9]+`), "a release version"},
	{regexp.MustCompile(`(?i)\b(initial|first)\s+(release|version)\b`), "release scope"},
	{regexp.MustCompile(`(?i)\b(later|future|next|upcoming)\s+(release|version)\b`), "release scope"},
	{regexp.MustCompile(`(?i)\bmilestone\b`), "release scope"},
	{regexp.MustCompile(`(?i)\bdraft\b`), "document status"},
	{regexp.MustCompile(`(?i)\*\*status:`), "a document status header"},
	{regexp.MustCompile(`(?i)\bat\s+this\s+stage\b`), "project status"},
}

// designStatusViolations returns one message per DESIGN.md line that contains
// project-phase or status wording.
func designStatusViolations(text string) []string {
	var out []string
	for i, line := range strings.Split(text, "\n") {
		for _, p := range designStatusPatterns {
			if m := p.pattern.FindString(line); m != "" {
				out = append(out, fmt.Sprintf("DESIGN.md:%d: %q is %s; status belongs in ROADMAP.md", i+1, m, p.reason))
			}
		}
	}
	return out
}

// designADRSection returns the "Architecture Decision Records" section of
// DESIGN.md, from its heading to the next second-level heading.
func designADRSection(design string) string {
	start := regexp.MustCompile(`(?m)^## [0-9]+\. Architecture Decision Records\s*$`).FindStringIndex(design)
	if start == nil {
		return ""
	}
	rest := design[start[1]:]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// adrIndexViolations returns one message per ADR file that the ADR table of
// DESIGN.md or the index of docs/adr/README.md does not link.
func adrIndexViolations(adrFiles []string, design, readme string) []string {
	var out []string
	section := designADRSection(design)
	if section == "" {
		out = append(out, `DESIGN.md has no "## N. Architecture Decision Records" section`)
	}
	for _, name := range adrFiles {
		q := regexp.QuoteMeta(name)
		if !regexp.MustCompile(`\]\((\./)?docs/adr/` + q + `\)`).MatchString(section) {
			out = append(out, fmt.Sprintf("docs/adr/%s is not linked from the ADR table of DESIGN.md", name))
		}
		if !regexp.MustCompile(`\]\((\./)?` + q + `\)`).MatchString(readme) {
			out = append(out, fmt.Sprintf("docs/adr/%s is not linked from docs/adr/README.md", name))
		}
	}
	return out
}

// roadmapViolations is the place for the ROADMAP.md rules of #991, such as a
// limit on its size or on the rows of a table. It checks nothing: those limits
// are defined together with the ROADMAP.md restructure, which fills it in.
func roadmapViolations(_ string) []string {
	return nil
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func adrFileNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repoRoot, "docs", "adr", "[0-9][0-9][0-9][0-9]-*.md"))
	if err != nil {
		t.Fatalf("list ADR files: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("found no docs/adr/NNNN-*.md file, so the ADR index rule would pass without checking anything")
	}
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	return names
}

func TestDocRulesDesignHasNoStatusWording(t *testing.T) {
	for _, v := range designStatusViolations(readRepoFile(t, "DESIGN.md")) {
		t.Error(v)
	}
}

func TestDocRulesEveryADRIsIndexed(t *testing.T) {
	design := readRepoFile(t, "DESIGN.md")
	readme := readRepoFile(t, filepath.Join("docs", "adr", "README.md"))
	for _, v := range adrIndexViolations(adrFileNames(t), design, readme) {
		t.Error(v)
	}
}

func TestDocRulesRoadmap(t *testing.T) {
	for _, v := range roadmapViolations(readRepoFile(t, "ROADMAP.md")) {
		t.Error(v)
	}
}

// Each seeded line must fail the DESIGN.md wording rule, so that a pattern
// that stops matching is noticed; the allowed lines are present-behaviour
// sentences with API terms that a too-wide pattern would reject.
func TestDocRulesStatusWordingRejectsSeededExamples(t *testing.T) {
	rejected := []string{
		"### Kind E2E — Phase 1 skeleton",
		"The broker tier moves in a later phase.",
		"Online resizing is not implemented.",
		"Scale-out is not yet supported.",
		"### Implementation status (#17)",
		"Scheduling is planned for the next cycle.",
		"TODO: describe restore.",
		"### MVP Topology",
		"This ships in v0.1.",
		"The first version will not provide a UI.",
		"Encryption arrives in a future release.",
		"The milestone covers backup.",
		"### 8.1 Spec (draft)",
		"**Status:** Draft (rev. 2)",
		"The project does not promise zero data loss at this stage.",
	}
	for _, line := range rejected {
		if len(designStatusViolations(line)) == 0 {
			t.Errorf("status wording %q passed the DESIGN.md rule", line)
		}
	}
	allowed := []string{
		"      phase: Created          # Pending | Creating | Created | Failed | Blocked",
		"`status.instances[].role` is one of `master`, `slave`, `replica`, or",
		"**explicit planned-failover intent** → fail over to an updated slave →",
		"restore initial master → validate → configure HA master → seed slaves",
		"[ROADMAP.md](ROADMAP.md) owns implementation and validation status.",
	}
	for _, line := range allowed {
		if v := designStatusViolations(line); len(v) != 0 {
			t.Errorf("present-behaviour line %q failed the DESIGN.md rule: %v", line, v)
		}
	}
}

// An ADR missing from either index must fail, and so must a DESIGN.md
// without the ADR section or with the link outside that section.
func TestDocRulesADRIndexRejectsSeededExamples(t *testing.T) {
	files := []string{"0001-a.md", "0002-b.md"}
	design := "## 22. Architecture Decision Records\n\n" +
		"| [0001](./docs/adr/0001-a.md) | A | #1 |\n" +
		"| [0002](./docs/adr/0002-b.md) | B | #2 |\n\n## 23. Release Scope\n"
	readme := "| [0001](./0001-a.md) | A |\n| [0002](./0002-b.md) | B |\n"
	if v := adrIndexViolations(files, design, readme); len(v) != 0 {
		t.Fatalf("complete indexes failed: %v", v)
	}
	cases := map[string]struct{ design, readme string }{
		"missing from the DESIGN.md table": {
			strings.Replace(design, "| [0002](./docs/adr/0002-b.md) | B | #2 |\n", "", 1), readme,
		},
		"missing from docs/adr/README.md": {
			design, strings.Replace(readme, "| [0002](./0002-b.md) | B |\n", "", 1),
		},
		"linked only outside the ADR section": {
			strings.Replace(design, "| [0002](./docs/adr/0002-b.md) | B | #2 |\n", "", 1) +
				"See [0002](./docs/adr/0002-b.md).\n", readme,
		},
		"DESIGN.md without the ADR section": {
			strings.Replace(design, "Architecture Decision Records", "Open Decisions", 1), readme,
		},
	}
	for name, c := range cases {
		if len(adrIndexViolations(files, c.design, c.readme)) == 0 {
			t.Errorf("%s: passed the ADR index rule", name)
		}
	}
}
