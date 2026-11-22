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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	summaryPath = "summary.json"
	junitPath   = "junit.xml"
	missing     = "MISSING EVIDENCE"
)

var (
	commitPattern = regexp.MustCompile("^[a-f0-9]{40}$")
	digestPattern = regexp.MustCompile("^sha256:[a-f0-9]{64}$")
	hashPattern   = regexp.MustCompile("^[a-f0-9]{64}$")
	namePattern   = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
	sourcePattern = regexp.MustCompile(
		`^https://github\.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/[0-9]+(?:/artifacts/[0-9]+)?$`)
)

type images struct {
	Operator string `json:"operatorImageDigest"`
	CUBRID   string `json:"cubridImageDigest"`
}

type candidate struct {
	Commit string `json:"commit"`
	images
}

type runEnvironment struct {
	Commit string `json:"operatorCommit"`
	images
}

type index struct {
	Version   string    `json:"indexVersion"`
	CreatedAt time.Time `json:"createdAt"`
	Candidate candidate `json:"candidate"`
	Runs      []run     `json:"runs"`
}

type run struct {
	ID          string         `json:"runId"`
	Source      string         `json:"source"`
	Version     string         `json:"schemaVersion"`
	Lane        string         `json:"lane"`
	Level       string         `json:"level"`
	StartedAt   time.Time      `json:"startedAt"`
	FinishedAt  time.Time      `json:"finishedAt"`
	Environment runEnvironment `json:"environment"`
	Scenarios   []scenario     `json:"scenarios"`
	Files       []indexedFile  `json:"files"`
}

type scenario struct {
	Key      string          `json:"key,omitempty"`
	ID       string          `json:"id"`
	Variant  string          `json:"variant,omitempty"`
	Recorded evidence.Result `json:"recorded"`
	Judged   evidence.Result `json:"judged"`
	Evidence []string        `json:"evidence"`
}

type indexedFile struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	State    string `json:"state"`
	Location string `json:"location,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func main() {
	indexPath := flag.String("index", "", "immutable release evidence index")
	runRoot := flag.String("runs", "", "directory containing one evidence bundle per run ID")
	flag.Parse()
	if *indexPath == "" || *runRoot == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := generate(os.Stdout, *indexPath, *runRoot); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(out io.Writer, indexPath, runRoot string) error {
	manifest, err := readIndex(indexPath)
	if err != nil {
		return err
	}
	var matrix strings.Builder
	fmt.Fprintf(&matrix, "# Candidate evidence matrix\n\n"+
		"- Candidate commit: `%s`\n- Operator image: `%s`\n- CUBRID image: `%s`\n\n",
		manifest.Candidate.Commit, manifest.Candidate.Operator, manifest.Candidate.CUBRID)
	matrix.WriteString("Run identities come from the index. Raw and judged verdicts stay unchanged. " +
		"The v0.1 scope column classifies requirements; it is not a release acceptance verdict. " +
		"Measured results always need independent SQL/safety review. A missing or failed " +
		"mandatory observation cannot be approved by this matrix.\n\n")
	matrix.WriteString("| Run | Level | Lane | Commit | Operator image | CUBRID image | Run status | Source |\n")
	matrix.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	var rows strings.Builder
	for _, source := range manifest.Runs {
		report := inspect(manifest.Candidate, source, filepath.Join(runRoot, source.ID))
		fmt.Fprintf(&matrix, "| %s | %s | %s | `%s` | `%s` | `%s` | %s | [Evidence](%s) |\n",
			source.ID, source.Level, source.Lane, displayIdentity(source.Environment.Commit, commitPattern),
			displayIdentity(source.Environment.Operator, digestPattern),
			displayIdentity(source.Environment.CUBRID, digestPattern), report.status, source.Source)
		for _, result := range report.results {
			fmt.Fprintf(&rows, "| %s | %s | %s | %s | %s | %s | %s |\n",
				source.ID, scenarioLabel(result.scenario), result.Variant, scopeOf(result.scenario),
				verdict(result.Recorded), verdict(result.judged), result.status)
		}
	}
	matrix.WriteString("\n| Run | Scenario | Variant | v0.1 scope | Recorded | Judged now | Evidence status |\n")
	matrix.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	matrix.WriteString(rows.String())
	_, err = io.WriteString(out, matrix.String())
	return err
}

func readIndex(path string) (index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return index{}, err
	}
	var manifest index
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return index{}, fmt.Errorf("read release evidence index: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return index{}, errors.New("release evidence index must contain one JSON object")
	}
	if manifest.Version != "1" || manifest.CreatedAt.IsZero() || len(manifest.Runs) == 0 {
		return index{}, errors.New("index version 1, creation time and at least one run are required")
	}
	c := manifest.Candidate
	if !commitPattern.MatchString(c.Commit) || !digestPattern.MatchString(c.Operator) ||
		!digestPattern.MatchString(c.CUBRID) {
		return index{}, errors.New("the candidate must specify a full commit and both image digests")
	}
	seen := map[string]bool{}
	for i, source := range manifest.Runs {
		if err := source.validate(); err != nil {
			return index{}, fmt.Errorf("run %d: %w", i+1, err)
		}
		if seen[source.ID] {
			return index{}, fmt.Errorf("run %d repeats a run ID", i+1)
		}
		seen[source.ID] = true
	}
	return manifest, nil
}

func (r run) validate() error {
	if !namePattern.MatchString(r.ID) || !namePattern.MatchString(r.Lane) || !namePattern.MatchString(r.Level) {
		return errors.New("run ID, lane and level must be simple names")
	}
	if !sourcePattern.MatchString(r.Source) {
		return errors.New("source must be a GitHub Actions run or artifact URL in this repository")
	}
	if (r.Version != "1" && r.Version != "2") || r.StartedAt.IsZero() {
		return errors.New("a supported summary schema version and start time are required")
	}
	for _, sc := range r.Scenarios {
		if !namePattern.MatchString(sc.ID) || !optionalName(sc.Key) || !optionalName(sc.Variant) ||
			verdict(sc.Recorded) == "" || verdict(sc.Judged) == "" {
			return errors.New("an indexed scenario has an invalid identity or verdict")
		}
	}
	return r.validateFiles()
}

func (r run) validateFiles() error {
	seen := map[string]bool{}
	for _, file := range r.Files {
		if !filepath.IsLocal(file.Path) || filepath.Clean(file.Path) != file.Path || seen[file.Path] {
			return errors.New("indexed files need unique paths inside the bundle")
		}
		seen[file.Path] = true
		switch file.State {
		case "listed", "archived":
			if !hashPattern.MatchString(file.SHA256) || (file.State == "archived" && file.Location == "") {
				return errors.New("listed or archived files need a full digest; archived files also need a locator")
			}
		case "unavailable":
			if file.SHA256 != evidence.Unknown || file.Reason == "" {
				return errors.New("unavailable files need an unknown digest and a reason")
			}
		default:
			return errors.New("unrecognized indexed file state")
		}
	}
	return nil
}

func optionalName(value string) bool {
	return value == "" || namePattern.MatchString(value)
}

func verdict(value evidence.Result) string {
	switch value {
	case evidence.Pass, evidence.Fail, evidence.Blocked, evidence.NotRun, evidence.NotApplicable:
		return strings.ToUpper(string(value))
	default:
		return ""
	}
}

func displayIdentity(value string, pattern *regexp.Regexp) string {
	if pattern.MatchString(value) {
		return value
	}
	return evidence.Unknown
}

func scenarioLabel(sc scenario) string {
	if sc.Key != "" {
		return sc.Key + " (" + sc.ID + ")"
	}
	return sc.ID
}

type resultRow struct {
	scenario
	judged evidence.Result
	status string
}

type runReport struct {
	status  string
	results []resultRow
}

func inspect(c candidate, source run, dir string) runReport {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return unavailable(source, "MISSING BUNDLE")
	}
	defer func() { _ = root.Close() }()
	files := make(map[string]indexedFile, len(source.Files))
	for _, file := range source.Files {
		files[file.Path] = file
	}
	if status := checkFile(root, files[summaryPath]); status != "" {
		return unavailable(source, "SUMMARY "+status)
	}
	actual, err := evidence.ReadRun(dir)
	if err != nil {
		return unavailable(source, "UNREADABLE SUMMARY")
	}
	if actual.SummarySHA256 != files[summaryPath].SHA256 || !matchesSummary(source, actual) {
		return unavailable(source, "INDEX MISMATCH")
	}
	rows := make([]resultRow, 0, len(actual.Results))
	checked := map[string]string{}
	incomplete := false
	for i, result := range actual.Results {
		row := resultRow{scenario: source.Scenarios[i], judged: result.Judged, status: "PRESENT"}
		for _, path := range result.Scenario.Evidence {
			selected, ok := checked[path]
			if !ok {
				selected = checkFile(root, files[path])
				checked[path] = selected
			}
			if selected != "" {
				row.status = selected
				incomplete = true
				if row.judged == evidence.Pass {
					row.judged = evidence.Fail
				}
				break
			}
		}
		if len(result.Scenario.Evidence) == 0 {
			row.status = "NO EVIDENCE LISTED"
		}
		rows = append(rows, row)
	}
	status := runStatus(c, source, actual, checkFile(root, files[junitPath]))
	if incomplete {
		status += ", INCOMPLETE EVIDENCE"
	}
	return runReport{status: status, results: rows}
}

func matchesSummary(source run, actual evidence.RecordedRun) bool {
	summary := actual.Summary
	env := summary.Environment
	observed := runEnvironment{Commit: env.OperatorCommit, images: images{env.OperatorImageDigest, env.CubridImageDigest}}
	if source.ID != summary.RunID || source.Version != summary.SchemaVersion || source.Lane != summary.Lane ||
		source.Level != env.Level || source.Environment != observed || !source.StartedAt.Equal(summary.StartedAt) ||
		!source.FinishedAt.Equal(summary.FinishedAt) || len(source.Scenarios) != len(summary.Scenarios) {
		return false
	}
	for i, result := range actual.Results {
		record := source.Scenarios[i]
		sc := result.Scenario
		if record.Key != result.Key || record.ID != sc.ID || record.Variant != sc.Variant ||
			record.Recorded != sc.Result || !slices.Equal(record.Evidence, sc.Evidence) {
			return false
		}
	}
	return true
}

func runStatus(c candidate, source run, actual evidence.RecordedRun, junitStatus string) string {
	env := source.Environment
	var states []string
	if actual.Provisional {
		states = append(states, "PARTIAL")
	}
	if source.Lane == "kind-filtered" {
		states = append(states, "FILTERED")
	}
	switch {
	case !commitPattern.MatchString(env.Commit):
		states = append(states, "UNKNOWN REVISION")
	case env.Commit != c.Commit:
		states = append(states, "OTHER REVISION")
	}
	switch {
	case !digestPattern.MatchString(env.Operator) || !digestPattern.MatchString(env.CUBRID):
		states = append(states, "UNKNOWN IMAGES")
	case env.images != c.images:
		states = append(states, "OTHER IMAGES")
	}
	if junitStatus != "" {
		states = append(states, "JUNIT "+junitStatus)
	}
	if !actual.Validated() {
		states = append(states, "LANE UNVERIFIED")
	}
	if len(states) == 0 {
		return "MATCHING CANDIDATE"
	}
	return strings.Join(states, ", ")
}

func unavailable(source run, status string) runReport {
	rows := make([]resultRow, 0, len(source.Scenarios))
	for _, sc := range source.Scenarios {
		rows = append(rows, resultRow{scenario: sc, status: status})
	}
	return runReport{status: status, results: rows}
}

func checkFile(root *os.Root, file indexedFile) string {
	if file.Path == "" || file.State == "unavailable" {
		return missing
	}
	info, err := root.Stat(file.Path)
	if err != nil || !info.Mode().IsRegular() {
		return missing
	}
	f, err := root.Open(file.Path)
	if err != nil {
		return missing
	}
	defer func() { _ = f.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return "UNREADABLE EVIDENCE"
	}
	if hex.EncodeToString(digest.Sum(nil)) != file.SHA256 {
		return "DIGEST MISMATCH"
	}
	return ""
}
