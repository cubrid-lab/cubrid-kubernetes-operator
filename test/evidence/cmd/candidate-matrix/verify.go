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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// v01Families are the scenarios the v0.1 candidate acceptance table of
// docs/testing/scenario-contract.md requires, by must-pass or must-measure.
// S09 is deferred to v0.2 and is not listed. Every registered result of a
// listed family whose scope is a v0.1 requirement must be recorded, and a
// listed family without a registered result cannot be proven by any run.
var v01Families = []string{"S00", "S01", "S02", "S03", "S04", "S05", "S06", "S07", s08Family,
	s10Family, "S11", "S12", "S13", "S14", "S15"}

// S08 and S10 are v0.1 families without a registered result.
const (
	s08Family = "S08"
	s10Family = "S10"
	// acknowledgedMissing counts acknowledged writes a member lacks.
	acknowledgedMissing = "acknowledgedMissing"
)

// measuredRule is what a must-measure result records besides its verdict:
// measurements that must be present and not unknown, and the ones of them
// that are a safety rule and must be zero.
type measuredRule struct {
	measured []string
	zero     []string
}

var (
	s07Common = []string{"faultConfirmedWithin", "partitionHeldFor", "establishedAcknowledgedAfterPathClosed",
		acknowledgedMissing, "membersDiverged", "finalMaster"}
	s07Closure = []string{"quarantineSeenWithin", "rwPodsGoneWithin", "rwEndpointsEmptyWithin",
		"newConnectionsStoppedWithin"}
	// No write may be acknowledged on an established session after the
	// managed -rw path was proven closed.
	s07Safety = []string{"establishedAcknowledgedAfterPathClosed"}
)

// v01Measured holds the rule of each registered must-measure result. A
// must-measure result without a rule here is No-Go: what it must record has
// not been decided.
var v01Measured = map[string]measuredRule{
	"ha-primary-pod-recovery-graceful": {
		measured: []string{"recoveryTime", "stableFor", acknowledgedMissing, "outcomeUnknown",
			"primaryUnresolvedFor", "routingNotReadyFor"},
		zero: []string{acknowledgedMissing},
	},
	"installed-ha-primary-vm-failure-power-off": {
		measured: []string{"confirmedOffAfter", "fencedAfter", "promotedWithin", "failoverWithin",
			"returnWithin", "convergedWithin", "finalMaster", "operationsNotAcknowledged"},
	},
	"installed-ha-network-partition-master-isolated": {
		measured: slices.Concat(s07Common, s07Closure), zero: s07Safety},
	"installed-ha-network-partition-higher-rank-isolated": {
		measured: slices.Concat(s07Common, s07Closure), zero: s07Safety},
	"installed-ha-network-partition-slave-isolated": {
		measured: append(slices.Clone(s07Common), "establishedAcknowledgedAfterFault"), zero: s07Safety},
}

// releaseVerdict is the v0.1 verdict of one result, one required variant or
// the candidate. It never replaces the raw verdict a run recorded.
type releaseVerdict string

const (
	verdictGo         releaseVerdict = "GO"
	verdictNoGo       releaseVerdict = "NO-GO"
	verdictReview     releaseVerdict = "REVIEW REQUIRED"
	verdictNotCounted releaseVerdict = "NOT COUNTED"
)

// verifiedResult is one recorded result with its separate v0.1 verdict.
type verifiedResult struct {
	run     string
	result  evidence.RecordedResult
	scope   v01Scope
	verdict releaseVerdict
	reason  string
}

// verifiedRun is what the verifier makes of one indexed run.
type verifiedRun struct {
	source  run
	problem string
	results []verifiedResult
}

// verifyCandidate writes the v0.1 verdict of the candidate in the index and
// reports whether it is Go.
func verifyCandidate(out io.Writer, indexPath, runRoot string) (bool, error) {
	return verify(out, indexPath, runRoot, v01Families)
}

func verify(out io.Writer, indexPath, runRoot string, families []string) (bool, error) {
	manifest, err := readIndex(indexPath)
	if err != nil {
		return false, err
	}
	runs := make([]verifiedRun, 0, len(manifest.Runs))
	byKey := map[string][]verifiedResult{}
	overall := verdictGo
	for _, source := range manifest.Runs {
		vr := verifyRun(manifest.Candidate, source, filepath.Join(runRoot, source.ID))
		if vr.problem != "" {
			overall = verdictNoGo
		}
		for _, r := range vr.results {
			byKey[r.result.Key] = append(byKey[r.result.Key], r)
			overall = worse(overall, r.verdict)
		}
		runs = append(runs, vr)
	}
	required := requiredVariants(families, byKey)
	for _, row := range required {
		overall = worse(overall, row.verdict)
	}

	var report strings.Builder
	fmt.Fprintf(&report, "# Candidate v0.1 verdict\n\n- Candidate commit: `%s`\n- Operator image: `%s`\n"+
		"- CUBRID image: `%s`\n\n**Overall v0.1 verdict: %s**\n\n",
		manifest.Candidate.Commit, manifest.Candidate.Operator, manifest.Candidate.CUBRID, overall)
	report.WriteString("GO needs every required variant recorded by runs of this commit and these images, " +
		"each record a pass its evidence supports, and every must-measure record measured without a safety " +
		"violation. Raw and judged verdicts are shown unchanged. A raw fail of a must-measure variant is " +
		"disclosed and not qualified: no recorded field shows that only a time limit failed. Repetitions, " +
		"the soak run, real-fault review and release approval are not checked here.\n\n")
	report.WriteString("## Runs\n\n| Run | Level | Lane | Run check |\n| --- | --- | --- | --- |\n")
	for _, vr := range runs {
		check := "MATCHING CANDIDATE"
		if vr.problem != "" {
			check = "NO-GO: " + vr.problem
		}
		fmt.Fprintf(&report, "| %s | %s | %s | %s |\n", vr.source.ID, vr.source.Level, vr.source.Lane, check)
	}
	report.WriteString("\n## Required variants\n\n| Scenario | Variant | Key | v0.1 scope | Records | " +
		"v0.1 verdict | Reason |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, row := range required {
		fmt.Fprintf(&report, "| %s | %s | %s | %s | %d | %s | %s |\n", row.id, row.variant, row.key, row.scope,
			row.records, row.verdict, cell(row.reason))
	}
	report.WriteString("\n## Recorded results\n\n| Run | Scenario | Variant | v0.1 scope | Recorded | Judged now | " +
		"v0.1 verdict | Reason |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, vr := range runs {
		for _, r := range vr.results {
			sc := r.result.Scenario
			label := scenarioLabel(scenario{Key: r.result.Key, ID: sc.ID})
			fmt.Fprintf(&report, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", r.run, label, sc.Variant, r.scope,
				rawVerdict(sc.Result), rawVerdict(r.result.Judged), r.verdict, cell(r.reason))
		}
	}
	if _, err := io.WriteString(out, report.String()); err != nil {
		return false, err
	}
	return overall == verdictGo, nil
}

// worse is the verdict of a candidate that already has a and gains b: one
// No-Go makes it No-Go, and an unmapped result needs review.
func worse(a, b releaseVerdict) releaseVerdict {
	switch {
	case a == verdictNoGo || b == verdictNoGo:
		return verdictNoGo
	case a == verdictReview || b == verdictReview:
		return verdictReview
	}
	return a
}

// verifyRun reads one indexed bundle, checks that it is the indexed one and
// was run on the candidate, and judges each of its results. A problem with
// the bundle makes every result in it No-Go.
func verifyRun(c candidate, source run, dir string) verifiedRun {
	vr := verifiedRun{source: source}
	root, err := os.OpenRoot(dir)
	if err != nil {
		vr.problem = "the bundle is missing"
		return vr
	}
	defer func() { _ = root.Close() }()
	files := make(map[string]indexedFile, len(source.Files))
	for _, file := range source.Files {
		files[file.Path] = file
	}
	if status := checkFile(root, files[summaryPath]); status != "" {
		vr.problem = summaryPath + ": " + status
		return vr
	}
	actual, err := evidence.ReadRun(dir)
	if err != nil {
		vr.problem = summaryPath + " could not be read"
		return vr
	}
	if actual.SummarySHA256 != files[summaryPath].SHA256 || !matchesSummary(source, actual) {
		vr.problem = summaryPath + " does not match the index"
		return vr
	}
	vr.problem = runProblem(c, source, actual, checkFile(root, files[junitPath]))
	checked := map[string]string{}
	for _, result := range actual.Results {
		problem := vr.problem
		if problem == "" {
			problem = evidenceProblem(root, files, checked, result.Scenario.Evidence)
		}
		scope, verdict, reason := judgeV01(result, problem)
		vr.results = append(vr.results, verifiedResult{run: source.ID, result: result, scope: scope,
			verdict: verdict, reason: reason})
	}
	return vr
}

// runProblem is why the whole run cannot stand for the candidate, or "".
func runProblem(c candidate, source run, actual evidence.RecordedRun, junitStatus string) string {
	env := source.Environment
	switch {
	case junitStatus != "":
		return junitPath + ": " + junitStatus
	case actual.Provisional:
		return "the run did not finish"
	case !commitPattern.MatchString(env.Commit):
		return "the source commit is not recorded"
	case env.Commit != c.Commit:
		return "the source commit is not the candidate's"
	case !digestPattern.MatchString(env.Operator) || !digestPattern.MatchString(env.CUBRID):
		return "the image digests are not recorded"
	case env.images != c.images:
		return "the image digests are not the candidate's"
	}
	// junit.xml is one write with summary.json; a disagreement is a mixed or
	// edited bundle.
	for _, p := range actual.Problems {
		if strings.HasPrefix(p, junitPath+" ") {
			return p
		}
	}
	return ""
}

// evidenceProblem is the first listed evidence file of a result that is not
// the indexed one, or "".
func evidenceProblem(root *os.Root, files map[string]indexedFile, checked map[string]string, paths []string) string {
	if len(paths) == 0 {
		return "no evidence is listed"
	}
	for _, path := range paths {
		status, ok := checked[path]
		if !ok {
			status = checkFile(root, files[path])
			checked[path] = status
		}
		if status != "" {
			return "evidence " + path + ": " + status
		}
	}
	return ""
}

// judgeV01 is the v0.1 verdict of one recorded result, given why its run or
// evidence cannot be relied on ("" when they can). The raw verdict is read,
// never changed.
func judgeV01(result evidence.RecordedResult, problem string) (v01Scope, releaseVerdict, string) {
	sc := result.Scenario
	scope := scopeOf(scenario{ID: sc.ID, Variant: sc.Variant})
	switch {
	case scope == scopeDeferred:
		return scope, verdictNotCounted, "deferred to v0.2: not required and not counted"
	case scope == scopeConditional:
		return scope, verdictNotCounted, "not claimed for v0.1: not required and not counted"
	case scope == scopeUnmapped || result.Key == "":
		return scopeUnmapped, verdictReview, "the registry or the v0.1 scope does not map this result"
	case problem != "":
		return scope, verdictNoGo, problem
	case scope == scopeMustMeasure && result.Judged == evidence.Fail:
		return scope, verdictNoGo, "raw fail, disclosed and not qualified: no recorded field shows that only a " +
			"time limit failed (" + result.Reason + ")"
	case result.Judged != evidence.Pass:
		return scope, verdictNoGo, fmt.Sprintf("judged %s: %s", result.Judged, result.Reason)
	case scope == scopeMustMeasure:
		if reason := measuredProblem(result); reason != "" {
			return scope, verdictNoGo, reason
		}
		return scope, verdictGo, "executed, measured and no safety rule broken"
	}
	return scope, verdictGo, "pass supported by its evidence"
}

// measuredProblem is why a passed must-measure result does not qualify, or "".
func measuredProblem(result evidence.RecordedResult) string {
	sc := result.Scenario
	if sc.FaultConfirmed == nil || !*sc.FaultConfirmed {
		return "the fault is not recorded as confirmed"
	}
	rule, ok := v01Measured[result.Key]
	if !ok {
		return "evidence not recorded: no measurement rule is defined for this variant"
	}
	for _, name := range rule.measured {
		if value, ok := sc.Measurements[name]; !ok || value == nil || value == evidence.Unknown {
			return name + " is not measured"
		}
	}
	for _, name := range rule.zero {
		if value := sc.Measurements[name]; !isZero(value) {
			return fmt.Sprintf("%s is %v, not 0", name, value)
		}
	}
	return ""
}

// isZero reports whether a measurement is the number zero, as summary.json
// records it (json.Number) or as a caller built it.
func isZero(value any) bool {
	switch v := value.(type) {
	case json.Number:
		return v.String() == "0"
	case float64:
		return v == 0
	case int:
		return v == 0
	}
	return false
}

// requiredVerdict is the v0.1 verdict of one required variant.
type requiredVerdict struct {
	id, variant, key string
	scope            v01Scope
	records          int
	verdict          releaseVerdict
	reason           string
}

// requiredVariants lists every registered result of the families whose scope
// is a v0.1 requirement, at both levels, and a row for each family without a
// registered result. A variant is Go only when it has a record and every
// record of it is Go.
func requiredVariants(families []string, byKey map[string][]verifiedResult) []requiredVerdict {
	var rows []requiredVerdict
	for _, family := range families {
		found := false
		for _, level := range []string{evidence.LevelKind, evidence.LevelVMLab} {
			for _, key := range evidence.RegisteredKeys(level) {
				entry, err := evidence.ResolveKey(key)
				if err != nil || entry.Family != family || len(entry.Aliases) == 0 {
					continue
				}
				found = true
				id, variant, _ := strings.Cut(entry.Aliases[0], "/")
				scope := scopeOf(scenario{ID: id, Variant: variant})
				if scope != scopeMustPass && scope != scopeMustMeasure {
					continue
				}
				rows = append(rows, requiredVariant(id, variant, key, scope, byKey[key]))
			}
		}
		if !found {
			rows = append(rows, requiredVerdict{id: family, key: "-", scope: scopeOf(scenario{ID: family}),
				verdict: verdictNoGo, reason: "evidence not recorded: no registered result of this scenario exists, " +
					"so no recorded run can prove it"})
		}
	}
	return rows
}

func requiredVariant(id, variant, key string, scope v01Scope, records []verifiedResult) requiredVerdict {
	row := requiredVerdict{id: id, variant: variant, key: key, scope: scope, records: len(records), verdict: verdictGo}
	if len(records) == 0 {
		row.verdict, row.reason = verdictNoGo, "no result was recorded"
		return row
	}
	for _, r := range records {
		if r.verdict != verdictGo {
			row.verdict, row.reason = verdictNoGo, r.run+": "+r.reason
			return row
		}
	}
	row.reason = fmt.Sprintf("every record is Go (%d)", len(records))
	return row
}

// rawVerdict shows a raw verdict as the matrix does, and one that is not a
// contract verdict as it was recorded.
func rawVerdict(value evidence.Result) string {
	if shown := verdict(value); shown != "" {
		return shown
	}
	return cell(string(value))
}

// cell keeps a value on one Markdown table row.
func cell(value string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ", "\r", " ").Replace(value)
}
