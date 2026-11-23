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
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

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
	// afterPathClosed counts established-session writes acknowledged after
	// the managed -rw path was observed closed.
	afterPathClosed = "establishedAcknowledgedAfterPathClosed"
	finalMaster     = "finalMaster"
)

// measuredRule is what a must-measure result records besides its verdict:
// measurements that must be present with a value of their kind, and the
// ones of them that are a safety rule and must be zero.
type measuredRule struct {
	measured []string
	zero     []string
}

// measureKind is the form a measurement's value must have.
type measureKind int

const (
	// durationValue is a duration string ("24.512s") or a number of seconds.
	durationValue measureKind = iota + 1
	// countValue is a non-negative whole number.
	countValue
	// nameValue is a non-empty name, such as a member.
	nameValue
	// classesValue is a non-empty map of outcome classes to counts.
	classesValue
)

// measurementKinds gives the kind of every measurement a rule lists. A listed
// measurement without a kind here is never accepted.
var measurementKinds = map[string]measureKind{
	"recoveryTime": durationValue, "stableFor": durationValue, "primaryUnresolvedFor": durationValue,
	"routingNotReadyFor": durationValue, "confirmedOffAfter": durationValue, "fencedAfter": durationValue,
	"promotedWithin": durationValue, "failoverWithin": durationValue, "vmReturnWithin": durationValue,
	"returnWithin": durationValue, "returnWriteOutage": durationValue, "convergedWithin": durationValue,
	"shortestStableFor": durationValue, "faultConfirmedWithin": durationValue, "partitionHeldFor": durationValue,
	"quarantineSeenWithin": durationValue, "rwScaledDownWithin": durationValue, "rwPodsGoneWithin": durationValue,
	"rwEndpointsEmptyWithin": durationValue, "newConnectionsStoppedWithin": durationValue,
	acknowledgedMissing: countValue, "outcomeUnknown": countValue, "operationsNotAcknowledged": countValue,
	"membersDiverged": countValue, afterPathClosed: countValue,
	"establishedAcknowledgedAfterFault": countValue,
	finalMaster:                         nameValue,
	"newConnectionWrites":               classesValue,
}

var (
	// newConnectionWrites is recorded for every S07 variant: the run reads the
	// new-connection probes before it judges any of them.
	s07Common = []string{"faultConfirmedWithin", "partitionHeldFor", afterPathClosed,
		acknowledgedMissing, "membersDiverged", finalMaster, "newConnectionWrites"}
	// The closure of the managed -rw path is measured only where the variant
	// quarantines; the slave-isolated control case must not.
	s07Closure = []string{"rwScaledDownWithin", "quarantineSeenWithin", "rwPodsGoneWithin",
		"rwEndpointsEmptyWithin", "newConnectionsStoppedWithin"}
	// No write may be acknowledged on an established session after the
	// managed -rw path was proven closed.
	s07Safety = []string{afterPathClosed}
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
	// A passed S04 run has recorded each of these: the pass needs the node
	// back, every client acknowledged after the taint and after the return,
	// and the stable period reached.
	"installed-ha-primary-vm-failure-power-off": {
		measured: []string{"confirmedOffAfter", "fencedAfter", "promotedWithin", "failoverWithin",
			"vmReturnWithin", "returnWithin", "returnWriteOutage", "convergedWithin", "shortestStableFor",
			finalMaster, "operationsNotAcknowledged"},
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

// candidateGo is the overall verdict of a candidate that passed every
// automated check. It is not release approval: manualGates remain.
const candidateGo = "AUTOMATED CHECKS GO — manual gates pending"

// manualGates are the release gates of #126 the verifier does not check.
var manualGates = []string{
	"Conditions, Events and metrics as separate v0.1 requirements",
	"repetitions of the core fault and recovery variants",
	"the 24-hour soak run",
	"real-fault review of the VM-lab runs",
	"index completeness: every failure and rerun of the candidate linked in the index; " +
		"a run left out of the index cannot be detected here",
	"independent reproduction of the procedures (#127)",
	"release approval",
}

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

	shown := string(overall)
	if overall == verdictGo {
		shown = candidateGo
	}
	var report strings.Builder
	fmt.Fprintf(&report, "# Candidate v0.1 verdict\n\n- Candidate commit: `%s`\n- Operator image: `%s`\n"+
		"- CUBRID image: `%s`\n\n**Overall v0.1 verdict: %s**\n\n",
		manifest.Candidate.Commit, manifest.Candidate.Operator, manifest.Candidate.CUBRID, shown)
	report.WriteString("The automated checks pass only when every required variant is recorded by finished, " +
		"indexed runs of registered lanes on this commit and these images, each record is a pass its evidence " +
		"supports, and every must-measure record is measured without a safety violation. Raw and judged " +
		"verdicts are shown unchanged. A raw fail of a must-measure variant is disclosed and not qualified: no " +
		"recorded field shows that only a time limit failed. This is not release approval.\n\n")
	report.WriteString("## Manual gates not checked\n\n")
	for _, gate := range manualGates {
		report.WriteString("- " + gate + "\n")
	}
	report.WriteString("\n")
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
	checked := map[string]string{}
	vr.problem = runProblem(c, source, actual, checkIndexed(root, files[junitPath]))
	// Every file the index lists or archives must be the indexed one, whether
	// a result names it or not. The index gives those a digest and an
	// unavailable file the unknown one (run.validateFiles).
	for _, file := range source.Files {
		if vr.problem != "" {
			break
		}
		if file.SHA256 != evidence.Unknown {
			if status := cachedCheck(root, files, checked, file.Path); status != "" {
				vr.problem = "indexed file " + file.Path + ": " + status
			}
		}
	}
	for _, result := range actual.Results {
		problem := vr.problem
		if problem == "" {
			problem = evidenceProblem(root, files, checked, result.Scenario.Evidence)
		}
		scope, verdict, reason := judgeV01(result, problem)
		vr.results = append(vr.results, verifiedResult{run: source.ID, result: result, scope: scope,
			verdict: verdict, reason: reason})
	}
	if vr.problem == "" {
		vr.problem = unexplainedProblem(actual, vr.results)
		for i := range vr.results {
			if vr.problem != "" && vr.results[i].verdict == verdictGo {
				vr.results[i].verdict, vr.results[i].reason = verdictNoGo, vr.problem
			}
		}
	}
	return vr
}

// unexplainedProblem is the first problem the run recorded, or the evidence
// reader found, that no result the verifier judged as not Go accounts for,
// or "". A credential found in the evidence, a step without its label or a
// suite deadline belongs to no result's verdict, so it blocks the run. A
// problem line of a result starts with its legacy name and ": ". Of the
// evidence reader's problems, an identity message the registry writes for a
// result without a key is accepted as well, and only in its exact form
// (identityProblem); the runner's own problems never are.
func unexplainedProblem(actual evidence.RecordedRun, results []verifiedResult) string {
	s := actual.Summary
	switch {
	case s.Passed && len(s.Problems) > 0:
		return "the run claims a pass but lists problems"
	case !s.Passed && len(s.Problems) == 0:
		return "the run recorded that it did not pass, with no problem listed"
	}
	explained := func(p string, reader bool) bool {
		for _, r := range results {
			if reader && r.result.Key == "" && identityProblem(r.result, s.Environment.Level).MatchString(p) {
				return true
			}
			name := r.result.Scenario.Name()
			if r.verdict != verdictGo && r.verdict != verdictNotCounted && strings.HasPrefix(p, name+": ") {
				return true
			}
		}
		return false
	}
	for _, p := range s.Problems {
		if !explained(p, false) {
			return "the run recorded a problem no judged result accounts for: " + p
		}
	}
	for _, p := range actual.Problems {
		if p != "the run recorded that it did not pass lane "+s.Lane && !explained(p, true) {
			return "the evidence reader found a problem no judged result accounts for: " + p
		}
	}
	return ""
}

// identityProblem matches exactly the messages the evidence registry writes
// when a result's legacy name does not resolve to one key at the run's level
// (resolveAlias, resolveResult and checkRecordedIdentity in
// test/evidence/registry.go and read.go). The registry has no key for an
// unmapped or deferred result, and the verifier already shows its verdict.
func identityProblem(r evidence.RecordedResult, level string) *regexp.Regexp {
	name := regexp.QuoteMeta(r.Scenario.Name())
	lvl := regexp.QuoteMeta(level)
	return regexp.MustCompile("^" + name + "(?:" +
		" is not a legacy name of a " + lvl + " result" +
		"| names [0-9]+ " + lvl + " results" +
		"| is recorded with the ID " + regexp.QuoteMeta(r.Scenario.ID) + ", not [A-Z]+[0-9]+" +
		"| records no key" +
		"| is recorded with the key " + regexp.QuoteMeta(r.RecordedKey) + ", not [a-z0-9-]+" +
		"| records no display name" +
		")$")
}

// runProblem is why the whole run cannot stand for the candidate, or "".
func runProblem(c candidate, source run, actual evidence.RecordedRun, junitStatus string) string {
	env := source.Environment
	switch {
	case junitStatus != "":
		return junitPath + ": " + junitStatus
	case !registeredLane(source.Lane):
		return fmt.Sprintf("lane %s is not a registered lane: a filtered or unregistered run validates no lane",
			source.Lane)
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
	return ""
}

// registeredLane reports whether lane is a lane of the registry at either
// level; kind-filtered is not one.
func registeredLane(lane string) bool {
	return slices.Contains(evidence.RegisteredLanes(evidence.LevelKind), lane) ||
		slices.Contains(evidence.RegisteredLanes(evidence.LevelVMLab), lane)
}

// evidenceProblem is the first listed evidence file of a result that is not
// the indexed one, or "".
func evidenceProblem(root *os.Root, files map[string]indexedFile, checked map[string]string, paths []string) string {
	if len(paths) == 0 {
		return "no evidence is listed"
	}
	for _, path := range paths {
		if status := cachedCheck(root, files, checked, path); status != "" {
			return "evidence " + path + ": " + status
		}
	}
	return ""
}

// cachedCheck checks an indexed file once per run.
func cachedCheck(root *os.Root, files map[string]indexedFile, checked map[string]string, path string) string {
	status, ok := checked[path]
	if !ok {
		status = checkIndexed(root, files[path])
		checked[path] = status
	}
	return status
}

// checkIndexed is checkFile, and an empty file is refused as well: empty
// evidence proves nothing.
func checkIndexed(root *os.Root, file indexedFile) string {
	if status := checkFile(root, file); status != "" {
		return status
	}
	if info, err := root.Stat(file.Path); err != nil || info.Size() == 0 {
		return "EMPTY EVIDENCE"
	}
	return ""
}

// judgeV01 is the v0.1 verdict of one recorded result, given why its run or
// evidence cannot be relied on ("" when they can). The raw verdict is read,
// never changed.
func judgeV01(result evidence.RecordedResult, problem string) (v01Scope, releaseVerdict, string) {
	sc := result.Scenario
	scope := scopeOf(scenario{ID: sc.ID, Variant: sc.Variant})
	cleanupFailed := sc.CleanupSucceeded != nil && !*sc.CleanupSucceeded
	switch {
	case (scope == scopeDeferred || scope == scopeConditional) &&
		(result.Judged == evidence.Fail || result.Judged == evidence.Blocked || cleanupFailed):
		// The contract keeps S09's held-member safety behavior, and a lane
		// fails on a failed result it does not require.
		return scope, verdictReview, fmt.Sprintf("not required, but judged %s (cleanup failed: %t): %s",
			result.Judged, cleanupFailed, result.Reason)
	case scope == scopeDeferred:
		return scope, verdictNotCounted, "deferred to v0.2: not required and not counted"
	case scope == scopeConditional:
		return scope, verdictNotCounted, "not claimed for v0.1: not required and not counted"
	case scope == scopeUnmapped || result.Key == "":
		return scopeUnmapped, verdictReview, "the registry or the v0.1 scope does not map this result"
	case problem != "":
		return scope, verdictNoGo, problem
	case cleanupFailed:
		return scope, verdictNoGo, "the cleanup did not succeed"
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
		if !validMeasurement(measurementKinds[name], sc.Measurements[name]) {
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

// validMeasurement reports whether value is a measured value of the kind:
// "unknown", an empty string, a value of another type or an empty map is not.
func validMeasurement(kind measureKind, value any) bool {
	switch kind {
	case durationValue:
		if s, ok := value.(string); ok {
			d, err := time.ParseDuration(s)
			return err == nil && d >= 0
		}
		n, ok := number(value)
		return ok && n >= 0
	case countValue:
		return isCount(value)
	case nameValue:
		s, ok := value.(string)
		return ok && s != "" && s != evidence.Unknown
	case classesValue:
		classes, ok := value.(map[string]any)
		if !ok || len(classes) == 0 {
			return false
		}
		for _, count := range classes {
			if !isCount(count) {
				return false
			}
		}
		return true
	}
	return false
}

// number is a JSON number as a float, as summary.json records it
// (json.Number) or as a caller built it.
func number(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

// isCount reports whether value is a non-negative whole number.
func isCount(value any) bool {
	n, ok := number(value)
	return ok && n >= 0 && n == math.Trunc(n)
}

// isZero reports whether a measurement is the number zero, as summary.json
// records it (json.Number) or as a caller built it. Only the literal JSON
// number 0 is accepted, not "0.0", a string or false: a safety count in any
// other form fails closed.
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
