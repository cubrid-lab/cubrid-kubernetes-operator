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
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
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
	// Notes are what a reader should know of steps outside the scenarios,
	// such as an attempt of a step that did not reach its precondition.
	Notes []string `json:"notes,omitempty"`

	// provisional marks a summary Checkpoint writes: its junit.xml carries
	// an error, so that CI tooling does not show an unfinished run as green.
	provisional bool

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

// Checkpoint writes summary.json and junit.xml with the results recorded so
// far, for a run that may be killed before it concludes. The checkpoint never
// passes: its one problem says the run did not finish and when it was
// written, and finishedAt is left unset. Conclude replaces it.
func (s Summary) Checkpoint(dir string, r *Redactor, lane string, at time.Time) error {
	if dir == "" {
		return errors.New("no run directory is set: the checkpoint cannot be kept")
	}
	s.Lane, s.Passed, s.FinishedAt, s.provisional = lane, false, time.Time{}, true
	s.Problems = []string{fmt.Sprintf("the run did not finish: this is what it recorded by %s",
		at.UTC().Format(time.RFC3339))}
	return s.Write(dir, r)
}

// Write stores summary.json and junit.xml in dir. Both hold the judged result
// of each scenario, and what the environment does not record as "unknown".
// junit.xml is written first, so that a process killed between the two
// leaves a summary.json no newer than junit.xml.
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

	report, err := xml.MarshalIndent(out.junit(), "", "  ")
	if err != nil {
		return err
	}
	if err := r.WriteFile(dir, "junit.xml", append([]byte(xml.Header), append(report, '\n')...)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return r.WriteFile(dir, "summary.json", append(data, '\n'))
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

// junitRunCase names the JUnit case of the run as a whole.
const junitRunCase = "run"

// junit maps judged results to JUnit. Blocked is an error, not a skip, so
// that CI tooling does not show an unproven scenario as green. The run
// itself is a case of its own when it did not pass: an error for a
// checkpoint, a failure for a concluded run with problems.
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
	switch {
	case s.provisional:
		suite.Tests++
		suite.Errors++
		suite.Cases = append(suite.Cases, junitCase{Name: junitRunCase, Classname: junitRunCase,
			Error: &junitMessage{Message: strings.Join(s.Problems, "; ")}})
	case !s.Passed && len(s.Problems) > 0:
		// A concluded run that did not pass its lane fails as a whole, also
		// when no scenario failed: a required scenario that was not run,
		// is missing, or another problem of the run (#460).
		suite.Tests++
		suite.Failures++
		suite.Cases = append(suite.Cases, junitCase{Name: junitRunCase, Classname: junitRunCase,
			Failure: &junitMessage{Message: strings.Join(s.Problems, "; ")}})
	}
	return suite
}

// redacted replaces every credential this package removes.
const redacted = "[REDACTED]"

// minSecretLength keeps a short registered value from blanking ordinary text.
const minSecretLength = 4

// minEncodedSecretLength is the shortest registered value whose base64 is
// also removed.
const minEncodedSecretLength = 8

var (
	bearerPattern = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
	// A word after "Basic" is a credential when it decodes as base64 to
	// user:password; prose such as "basic authentication" is left alone.
	basicPattern = regexp.MustCompile(`(?i)(\bbasic\s+)([A-Za-z0-9+/]{4,}={0,2})`)
	// curl -u user:password and --user[=]user:password.
	curlUserPattern = regexp.MustCompile(`(\bcurl\b[^\n]*?\s(?:-u|--user)(?:=|\s+)[^\s:]+:)\S+`)
	// A private key in PEM form, also with its line breaks escaped in JSON,
	// and to the end of the text when the end marker was cut off.
	pemPattern = regexp.MustCompile(
		`-----BEGIN [A-Z ]*PRIVATE KEY-----(?:[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----|[\s\S]*\z)`)
	accessKeyPattern = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	urlUserPattern   = regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+@`)
	// A CUBRID JDBC URL: jdbc:cubrid:host:port:database:user:password:.
	jdbcPattern = regexp.MustCompile(`(?i)(jdbc:cubrid:[^:\s]*:[^:\s]*:[^:\s]*:[^:\s]*:)[^:\s]+`)
)

// fieldPatterns find a credential written as name=value or name: value. A
// value in escaped JSON (\"...\") ends at its closing \" or at the end of
// the line. A double- or single-quoted value ends at its closing quote on the
// same line, honoring escapes and SQL's doubled quote; without one it ends
// like an unquoted value.
// An unquoted value ends at a space, a quote, a comma, a brace, or a URL
// query's & or ;. An empty value is left as it is.
var fieldPatterns = func() []*regexp.Regexp {
	const key = `(?i)((?:password|passwd|token|secret[_-]?access[_-]?key|secret[_-]?key|secret|` +
		`access[_-]?key(?:[_-]?id)?|api[_-]?key|private[_-]?key)(?:\\?["'])?\s*[=:]\s*`
	return []*regexp.Regexp{
		// Inside escaped JSON: \\ and \" stand for an escaped backslash and
		// quote of the inner JSON, \ and a letter for its other escapes.
		regexp.MustCompile(key + `\\")()(?:\\\\\\\\|\\\\\\"|\\\\[^\\"\n]|\\[^\\"\n]|[^\\"\n])+`),
		regexp.MustCompile(key + `")(?:\\.|[^"\\\n])+(")`),
		// SQL writes a quote inside a string as ''.
		regexp.MustCompile(key + `')(?:''|[^'\n])+(')`),
		regexp.MustCompile(key + `["'])()[^\s"']+`),
		regexp.MustCompile(key + `)()[^\s"'\\,;&}]+`),
	}
}()

// Redactor removes credentials from text: the values it was given, and text
// that has the form of a credential. It is safe for concurrent use.
type Redactor struct {
	mu      sync.Mutex
	secrets []string
}

// NewRedactor returns a Redactor that also removes the given secret values.
// Values shorter than four characters are ignored.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	r.Add(secrets...)
	return r
}

// Add registers further secret values, such as one generated during the run.
// Values shorter than four characters are ignored. Each value is also
// removed in the encodings it is commonly shown in: base64, as a Secret's
// data holds it, for values of at least eight bytes, URL encoding and JSON
// escaping. Base64 is matched reliably only as the encoding of the value
// alone, not inside the encoding of a longer text.
func (r *Redactor) Add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		for _, v := range SecretForms(s) {
			if !slices.Contains(r.secrets, v) {
				r.secrets = append(r.secrets, v)
			}
		}
	}
	// Longest first, so that a value containing another is removed whole.
	slices.SortFunc(r.secrets, func(a, b string) int { return len(b) - len(a) })
}

// SecretForms returns a secret value and the encodings Redactor removes it
// in, or nothing for a value shorter than four characters.
func SecretForms(s string) []string {
	if len(s) < minSecretLength {
		return nil
	}
	quoted, _ := json.Marshal(s)
	forms := []string{s, url.QueryEscape(s), url.PathEscape(s), string(quoted[1 : len(quoted)-1])}
	// The base64 of a short value is short enough to occur by chance.
	if len(s) >= minEncodedSecretLength {
		forms = append(forms,
			base64.StdEncoding.EncodeToString([]byte(s)), base64.RawStdEncoding.EncodeToString([]byte(s)),
			base64.URLEncoding.EncodeToString([]byte(s)), base64.RawURLEncoding.EncodeToString([]byte(s)))
	}
	return forms
}

// Leaks returns the files under dir, relative to it and sorted, that hold a
// credential registered with Add, raw or in one of the encodings Redact
// removes (#587). It names files only, never the credential. The evidence
// should have passed through Redact; this checks that it did.
func (r *Redactor) Leaks(dir string) ([]string, error) {
	r.mu.Lock()
	secrets := slices.Clone(r.secrets)
	r.mu.Unlock()
	leaks := []string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		if slices.ContainsFunc(secrets, func(s string) bool { return strings.Contains(text, s) }) {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			leaks = append(leaks, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(leaks)
	return leaks, err
}

// Redact returns data with credentials replaced by "[REDACTED]".
func (r *Redactor) Redact(data []byte) []byte {
	text := string(data)
	if r != nil {
		r.mu.Lock()
		for _, s := range r.secrets {
			text = strings.ReplaceAll(text, s, redacted)
		}
		r.mu.Unlock()
	}
	text = pemPattern.ReplaceAllString(text, redacted)
	text = bearerPattern.ReplaceAllString(text, "${1}"+redacted)
	text = basicPattern.ReplaceAllStringFunc(text, func(m string) string {
		parts := basicPattern.FindStringSubmatch(m)
		decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(parts[2], "="))
		if err != nil || !strings.Contains(string(decoded), ":") {
			return m
		}
		return parts[1] + redacted
	})
	text = curlUserPattern.ReplaceAllString(text, "${1}"+redacted)
	text = jdbcPattern.ReplaceAllString(text, "${1}"+redacted)
	for _, p := range fieldPatterns {
		text = p.ReplaceAllString(text, "${1}"+redacted+"${2}")
	}
	text = accessKeyPattern.ReplaceAllString(text, redacted)
	text = urlUserPattern.ReplaceAllString(text, "${1}"+redacted+"@")
	return []byte(text)
}

// WriteFile stores redacted data at the path rel below dir, creating the
// directories on the way. A path that leaves dir is refused. The file is
// replaced by a rename, so a process killed during the write leaves the
// previous content or the new one, never a part of it.
func (r *Redactor) WriteFile(dir, rel string, data []byte) error {
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("evidence path %q is outside the run directory", rel)
	}
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(r.Redact(data))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// RequireEmptyDir reports an error when dir holds anything, naming what it
// holds. A directory that does not exist yet is as good as an empty one, and
// nothing is created or removed: the evidence of an earlier run is the
// caller's to keep or to delete.
func RequireEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("the evidence directory %s cannot be read: %w", dir, err)
	case len(entries) == 0:
		return nil
	}
	const shown = 5
	names := make([]string, 0, shown)
	for _, entry := range entries[:min(shown, len(entries))] {
		names = append(names, entry.Name())
	}
	more := ""
	if len(entries) > shown {
		more = fmt.Sprintf(" and %d more", len(entries)-shown)
	}
	return fmt.Errorf("the evidence directory %s holds the files of an earlier run (%s%s): "+
		"remove it, or set E2E_EVIDENCE_DIR to a directory of this run's own",
		dir, strings.Join(names, ", "), more)
}
