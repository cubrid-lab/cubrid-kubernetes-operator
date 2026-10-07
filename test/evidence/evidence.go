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

// Package evidence writes the result of a validation run in the format of
// docs/testing/scenario-contract.md: summary.json, junit.xml, and files with
// credentials removed.
package evidence

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the version of summary.json this package writes.
const SchemaVersion = "1"

// Unknown is the value of a measurement that could not be taken. It is never
// replaced by zero.
const Unknown = "unknown"

// Result is the outcome of one scenario run.
type Result string

const (
	Pass          Result = "pass"
	Fail          Result = "fail"
	Blocked       Result = "blocked"
	NotRun        Result = "not_run"
	NotApplicable Result = "not_applicable"
)

// Environment says what a run tested and where.
type Environment struct {
	Level               string `json:"level"`
	KubernetesVersion   string `json:"kubernetesVersion"`
	OperatorCommit      string `json:"operatorCommit"`
	OperatorImageDigest string `json:"operatorImageDigest"`
	CubridImageDigest   string `json:"cubridImageDigest"`
	EngineVersion       string `json:"engineVersion"`
	ClientDriver        string `json:"clientDriver"`
}

// Operations counts the client operations of a scenario by outcome.
type Operations struct {
	Attempted    int `json:"attempted"`
	Acknowledged int `json:"acknowledged"`
	Failed       int `json:"failed"`
	Unknown      int `json:"unknown"`
}

// Scenario is the result of one scenario, or of one of its variants.
type Scenario struct {
	ID      string `json:"id"`
	Variant string `json:"variant,omitempty"`
	Result  Result `json:"result"`
	Reason  string `json:"reason"`
	// FaultConfirmed is nil for a scenario that injects no fault.
	FaultConfirmed   *bool             `json:"faultConfirmed,omitempty"`
	CleanupSucceeded *bool             `json:"cleanupSucceeded,omitempty"`
	Limits           map[string]string `json:"limits,omitempty"`
	// Measurements holds numbers or durations; a value that was not measured
	// is Unknown.
	Measurements map[string]any `json:"measurements,omitempty"`
	Operations   *Operations    `json:"operations,omitempty"`
	NeverEvents  []string       `json:"neverEvents"`
	// Evidence lists files relative to the run directory.
	Evidence []string `json:"evidence"`
}

// Summary is the content of summary.json.
type Summary struct {
	SchemaVersion string      `json:"schemaVersion"`
	RunID         string      `json:"runId"`
	StartedAt     time.Time   `json:"startedAt"`
	FinishedAt    time.Time   `json:"finishedAt"`
	Environment   Environment `json:"environment"`
	// Lane names the set of required scenarios the run was judged against;
	// Passed says whether it passed them, and Problems why not.
	Lane      string     `json:"lane"`
	Passed    bool       `json:"passed"`
	Problems  []string   `json:"problems"`
	Scenarios []Scenario `json:"scenarios"`

	// planned maps each scenario and variant that still holds the placeholder
	// Plan recorded to its index in Scenarios.
	planned map[Requirement]int
}

// Name is the scenario's name in reports: "S03/abrupt", or "S00" without a
// variant.
func (s Scenario) Name() string {
	if s.Variant == "" {
		return s.ID
	}
	return s.ID + "/" + s.Variant
}

// Judge returns the result a reader may rely on, given the run directory. A
// claimed pass becomes a fail when the fault was not confirmed, when an
// outcome that must never happen was seen, or when its evidence is missing.
func (s Scenario) Judge(dir string) (Result, string) {
	switch s.Result {
	case Pass:
	case Fail, Blocked, NotRun, NotApplicable:
		if s.Reason == "" {
			return s.Result, "no reason recorded"
		}
		return s.Result, s.Reason
	default:
		return Fail, fmt.Sprintf("result %q is not one of pass, fail, blocked, not_run, not_applicable", s.Result)
	}
	if s.FaultConfirmed != nil && !*s.FaultConfirmed {
		return Fail, "claimed a pass, but the fault was not confirmed"
	}
	if len(s.NeverEvents) > 0 {
		return Fail, "an outcome that must never happen was observed: " + strings.Join(s.NeverEvents, "; ")
	}
	if len(s.Evidence) == 0 {
		return Fail, "claimed a pass, but no evidence is listed"
	}
	for _, rel := range s.Evidence {
		if !filepath.IsLocal(rel) {
			return Fail, fmt.Sprintf("evidence %s is outside the run directory", rel)
		}
		if info, err := os.Stat(filepath.Join(dir, rel)); err != nil || info.IsDir() {
			return Fail, fmt.Sprintf("claimed a pass, but evidence %s is missing", rel)
		}
	}
	return Pass, ""
}

// Requirement is one scenario, or one variant of it, that a lane requires.
type Requirement struct {
	ID      string
	Variant string
	// NotApplicable lets a not_applicable result satisfy the requirement. A
	// lane sets it only where its environment rules the scenario out, never
	// for work that was skipped.
	NotApplicable bool
}

// Lane is the set of scenarios a run must pass to validate one environment.
type Lane struct {
	Name     string
	Required []Requirement
}

// Gate returns why the summary does not pass the lane, one line per problem,
// or nothing when it passes. A required scenario passes when it has at least
// one result and every result of it is judged a pass, or not_applicable where
// the lane allows that. A scenario judged a fail is a problem even when the
// lane does not require it. A lane that requires nothing never passes: such
// a run proved nothing.
func (l Lane) Gate(s Summary, dir string) []string {
	var problems []string
	if len(l.Required) == 0 {
		problems = append(problems, "no scenario recorded: the lane requires nothing, so the run proved nothing")
	}
	for _, req := range l.Required {
		name := Scenario{ID: req.ID, Variant: req.Variant}.Name()
		seen := false
		for _, sc := range s.Scenarios {
			if sc.ID != req.ID || sc.Variant != req.Variant {
				continue
			}
			seen = true
			result, reason := sc.Judge(dir)
			if result == Pass || (result == NotApplicable && req.NotApplicable) {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: %s: %s", name, result, reason))
		}
		if !seen {
			problems = append(problems, name+": missing: no result was recorded")
		}
	}
	for _, sc := range s.Scenarios {
		if l.requires(sc) {
			continue
		}
		if result, reason := sc.Judge(dir); result == Fail {
			problems = append(problems, fmt.Sprintf("%s: %s: %s", sc.Name(), result, reason))
		}
	}
	return problems
}

func (l Lane) requires(sc Scenario) bool {
	return slices.ContainsFunc(l.Required, func(req Requirement) bool {
		return req.ID == sc.ID && req.Variant == sc.Variant
	})
}

// Conclude judges the run against the lane, writes summary.json and
// junit.xml with that verdict into dir, and returns an error when the run did
// not pass or the files could not be written. The files are written before
// the verdict is returned, so that a failed run keeps them. Other problems
// the run found, such as UnlabelledSteps, also fail it.
func (s Summary) Conclude(dir string, r *Redactor, lane Lane, other ...string) error {
	if dir == "" {
		return errors.New("no run directory is set: the evidence of a pass cannot be kept")
	}
	problems := append(lane.Gate(s, dir), other...)
	s.Lane, s.Passed, s.Problems = lane.Name, len(problems) == 0, problems
	var errs []error
	if err := s.Write(dir, r); err != nil {
		errs = append(errs, fmt.Errorf("could not write the run summary: %w", err))
	}
	if len(problems) > 0 {
		errs = append(errs, fmt.Errorf("the run did not pass lane %s:\n%s", lane.Name, strings.Join(problems, "\n")))
	}
	return errors.Join(errs...)
}

// Write stores summary.json and junit.xml in dir. Both hold the judged result
// of each scenario, and what the environment does not record as "unknown".
func (s Summary) Write(dir string, r *Redactor) error {
	out := s
	out.SchemaVersion = SchemaVersion
	for _, field := range []*string{
		&out.Environment.Level, &out.Environment.KubernetesVersion, &out.Environment.OperatorCommit,
		&out.Environment.OperatorImageDigest, &out.Environment.CubridImageDigest,
		&out.Environment.EngineVersion, &out.Environment.ClientDriver,
	} {
		if *field == "" {
			*field = Unknown
		}
	}
	out.Scenarios = make([]Scenario, len(s.Scenarios))
	for i, sc := range s.Scenarios {
		sc.Result, sc.Reason = sc.Judge(dir)
		if sc.NeverEvents == nil {
			sc.NeverEvents = []string{}
		}
		if sc.Evidence == nil {
			sc.Evidence = []string{}
		}
		out.Scenarios[i] = sc
	}
	if out.Problems == nil {
		out.Problems = []string{}
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := r.WriteFile(dir, "summary.json", append(data, '\n')); err != nil {
		return err
	}
	report, err := xml.MarshalIndent(out.junit(), "", "  ")
	if err != nil {
		return err
	}
	return r.WriteFile(dir, "junit.xml", append([]byte(xml.Header), append(report, '\n')...))
}

type junitSuite struct {
	XMLName  xml.Name    `xml:"testsuite"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Error     *junitMessage `xml:"error,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
}

// junit maps judged results to JUnit. Blocked is an error, not a skip, so
// that CI tooling does not show an unproven scenario as green.
func (s Summary) junit() junitSuite {
	suite := junitSuite{Name: s.RunID, Tests: len(s.Scenarios)}
	for _, sc := range s.Scenarios {
		c := junitCase{Name: sc.Name(), Classname: "scenario"}
		switch sc.Result {
		case Pass:
		case Fail:
			c.Failure = &junitMessage{Message: sc.Reason}
			suite.Failures++
		case Blocked:
			c.Error = &junitMessage{Message: string(sc.Result) + ": " + sc.Reason}
			suite.Errors++
		default:
			c.Skipped = &junitMessage{Message: string(sc.Result) + ": " + sc.Reason}
			suite.Skipped++
		}
		suite.Cases = append(suite.Cases, c)
	}
	return suite
}

// redacted replaces every credential this package removes.
const redacted = "[REDACTED]"

// minSecretLength keeps a short registered value from blanking ordinary text.
const minSecretLength = 4

var (
	bearerPattern = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
	// A credential written as name=value or name: value, quoted or not.
	fieldPattern = regexp.MustCompile(
		`(?i)((?:password|passwd|token|secret|secret[_-]?access[_-]?key|access[_-]?key(?:[_-]?id)?)"?\s*[=:]\s*"?)[^\s"',}]+`)
	accessKeyPattern = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	urlUserPattern   = regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+@`)
)

// Redactor removes credentials from text: the values it was given, and text
// that has the form of a credential.
type Redactor struct {
	secrets []string
}

// NewRedactor returns a Redactor that also removes the given secret values.
// Values shorter than four characters are ignored.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(s) >= minSecretLength {
			r.secrets = append(r.secrets, s)
		}
	}
	// Longest first, so that a value containing another is removed whole.
	slices.SortFunc(r.secrets, func(a, b string) int { return len(b) - len(a) })
	return r
}

// Redact returns data with credentials replaced by "[REDACTED]".
func (r *Redactor) Redact(data []byte) []byte {
	text := string(data)
	if r != nil {
		for _, s := range r.secrets {
			text = strings.ReplaceAll(text, s, redacted)
		}
	}
	text = bearerPattern.ReplaceAllString(text, "${1}"+redacted)
	text = fieldPattern.ReplaceAllString(text, "${1}"+redacted)
	text = accessKeyPattern.ReplaceAllString(text, redacted)
	text = urlUserPattern.ReplaceAllString(text, "${1}"+redacted+"@")
	return []byte(text)
}

// WriteFile stores redacted data at the path rel below dir, creating the
// directories on the way. A path that leaves dir is refused.
func (r *Redactor) WriteFile(dir, rel string, data []byte) error {
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("evidence path %q is outside the run directory", rel)
	}
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, r.Redact(data), 0o600)
}
