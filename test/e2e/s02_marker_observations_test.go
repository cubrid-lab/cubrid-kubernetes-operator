//go:build e2e

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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
	. "github.com/onsi/ginkgo/v2"
)

const s02MarkerSelect = "SELECT v FROM s02_marker WHERE id = 1;"
const s02MarkerReceiptFile = "S02/marker-write-failure.json"

type s02MarkerProbe struct {
	writer    string
	members   []string
	selectSQL func(context.Context, string, string) (string, error)
	identity  func(context.Context, string) (string, error)
	now       func() time.Time
	redact    *evidence.Redactor
	writeFile func(string, []byte) string
}

// writeS02Marker retains the existing strict one-command INSERT/COMMIT. Its
// optional observations run only after that exact write has returned an error.
func (r *haRun) writeS02Marker(ctx context.Context, master, marker string, started time.Time) (
	string, string, string, error) {
	return writeWithS02MarkerObservations(ctx, started, marker, func() (string, error) {
		return runSQL(csqlInPod(ctx, r.namespace, master, r.database),
			"INSERT INTO s02_marker VALUES (1, '"+marker+"'); COMMIT;")
	}, s02MarkerProbe{
		writer: master, members: r.members,
		selectSQL: func(ctx context.Context, member, query string) (string, error) {
			return runSQL(csqlInPod(ctx, r.namespace, member, r.database), query)
		},
		identity: func(ctx context.Context, member string) (string, error) {
			return serverGuard(ctx, r.namespace, member, r.database, true).Identity()
		},
		now: time.Now, redact: redactor, writeFile: writeScenarioFile,
	})
}

const (
	s02NotObserved = "not_observed"
	s02Answered    = "answered"
	s02Unavailable = "unavailable"
	s02NoValues    = "no_values_returned"
)

type s02MarkerCommand struct {
	StartedAt     *time.Time `json:"queryStartedAt"`
	FinishedAt    *time.Time `json:"queryFinishedAt"`
	Status        string     `json:"commandStatus"`
	Output        string     `json:"output"`
	Error         string     `json:"error"`
	OutputBytes   int        `json:"outputBytes"`
	ErrorBytes    int        `json:"errorBytes"`
	TextTruncated bool       `json:"textTruncated"`
}

type s02MarkerMember struct {
	Member            string           `json:"member"`
	Select            s02MarkerCommand `json:"select"`
	MarkerObservation string           `json:"markerObservation"`
	Values            []string         `json:"values"`
	ValueCount        *int             `json:"valueCount"`
	ValuesTruncated   bool             `json:"valuesTruncated"`
	Identity          s02MarkerCommand `json:"afterOnlyIdentityCommand"`
}

type s02MarkerReceipt struct {
	WriteMember        string            `json:"writeMember"`
	Scope              string            `json:"scope"`
	ExpectedMarker     string            `json:"expectedMarker"`
	Write              s02MarkerCommand  `json:"originalWrite"`
	SelectQuery        string            `json:"selectQuery"`
	Members            []s02MarkerMember `json:"members"`
	CommitOutcome      string            `json:"commitOutcome"`
	IdentityContinuity string            `json:"identityContinuity"`
	IdentityBinding    string            `json:"selectIdentityBinding"`
	Truncated          bool              `json:"textTruncated"`
}

// Only the original write decides the returned error. Diagnostics neither
// replay it nor settle its lost-session ledger entry, even if rows are seen.
func writeWithS02MarkerObservations(ctx context.Context, started time.Time, marker string,
	write func() (string, error), probe s02MarkerProbe) (string, string, string, error) {
	out, original := write()
	if original == nil {
		return out, "", "", nil
	}
	receipt := s02MarkerReceipt{WriteMember: probe.writer, Scope: "after_write_error_only", ExpectedMarker: marker,
		Write: s02MarkerCommand{StartedAt: s02MarkerTime(started), FinishedAt: s02MarkerTime(probe.now()),
			Status: "failed", Output: out, Error: original.Error(), OutputBytes: len(out), ErrorBytes: len(original.Error())},
		SelectQuery: s02MarkerSelect, CommitOutcome: evidence.Unknown, IdentityContinuity: evidence.Unknown,
		IdentityBinding: "not atomic; no pre-write identity was observed"}
	diagnostic, cancel := context.WithTimeout(ctx, evidence.DefaultLogLimits().CheckpointTimeout)
	defer cancel()
	seen := map[string]bool{}
	for _, member := range probe.members {
		if seen[member] {
			continue
		}
		seen[member] = true
		receipt.Members = append(receipt.Members, observeS02MarkerMember(diagnostic, member, marker, probe))
	}
	file, note := storeS02MarkerReceipt(receipt, probe)
	return out, file, note, original
}

func s02MarkerTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}

// Commands are synchronous and receive the existing per-source timeout under
// one checkpoint deadline. No later command starts after that context ends.
func observeS02MarkerCommand(ctx context.Context, now func() time.Time,
	call func(context.Context) (string, error)) s02MarkerCommand {
	observation := s02MarkerCommand{Status: s02NotObserved}
	if ctx.Err() != nil {
		observation.Error = ctx.Err().Error()
		observation.ErrorBytes = len(observation.Error)
		return observation
	}
	bounded, cancel := context.WithTimeout(ctx, evidence.DefaultLogLimits().SourceTimeout)
	defer cancel()
	observation.StartedAt = s02MarkerTime(now())
	out, err := call(bounded)
	observation.FinishedAt = s02MarkerTime(now())
	if err == nil {
		err = bounded.Err()
	}
	observation.Output, observation.OutputBytes = out, len(out)
	observation.Status = s02Answered
	if err != nil {
		observation.Status = s02Unavailable
		observation.Error = err.Error()
		observation.ErrorBytes = len(observation.Error)
	}
	return observation
}

func observeS02MarkerMember(ctx context.Context, member, marker string, probe s02MarkerProbe) s02MarkerMember {
	observation := s02MarkerMember{Member: member, MarkerObservation: evidence.Unknown}
	observation.Select = observeS02MarkerCommand(ctx, probe.now, func(ctx context.Context) (string, error) {
		return probe.selectSQL(ctx, member, s02MarkerSelect)
	})
	if observation.Select.Status == s02Answered {
		values, err := completeS02MarkerValues(observation.Select.Output)
		if err != nil {
			observation.Select.Status = s02Unavailable
			observation.Select.Error = err.Error()
			observation.Select.ErrorBytes = len(err.Error())
		} else {
			observation.Values = append([]string{}, values...)
			count := len(values)
			observation.ValueCount = &count
			switch {
			case len(values) == 0:
				observation.MarkerObservation = s02NoValues
			case slices.Equal(values, []string{marker}):
				observation.MarkerObservation = "expected_value_returned"
			default:
				observation.MarkerObservation = "different_values_returned"
			}
		}
	}
	observation.Identity = observeS02MarkerCommand(ctx, probe.now, func(ctx context.Context) (string, error) {
		return probe.identity(ctx, member)
	})
	return observation
}

// These receipt-only checks require a complete native response. They never
// tolerate a failed command or change the shared SQL reader/re-read policy.
var s02SelectedRows = regexp.MustCompile(`(?m)^[ \t]*([0-9]+) rows? selected\.(?:[ \t]*\([^\r\n()]*\))?` +
	`(?:[ \t]+Committed\.(?:[ \t]*\([^\r\n()]*\))?)?[ \t]*\r?$`)
var s02MarkerRow = regexp.MustCompile(`^'(?:[^']|'')*'$`)
var s02MarkerSeparator = regexp.MustCompile(`^={2,}$`)
var s02MarkerCommit = regexp.MustCompile(`^Committed\.(?:[ \t]*\([^\r\n()]*\))?[ \t]*$`)

func completeS02MarkerValues(out string) ([]string, error) {
	invalid := fmt.Errorf("marker SELECT is not one complete one-column response")
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 0 && lines[0] == "=== <Result of SELECT Command in Line 1> ===" {
		lines = lines[1:]
	}
	if len(lines) > 0 && lines[0] == "There are no results." {
		if len(lines) == 1 || len(lines) == 2 && s02MarkerCommit.MatchString(lines[1]) {
			return []string{}, nil
		}
		return nil, invalid
	}
	if len(lines) < 3 || !strings.EqualFold(lines[0], "v") || !s02MarkerSeparator.MatchString(lines[1]) {
		return nil, invalid
	}
	countLine := s02SelectedRows.FindStringSubmatch(lines[len(lines)-1])
	if countLine == nil {
		return nil, invalid
	}
	for _, line := range lines[2 : len(lines)-1] {
		if !s02MarkerRow.MatchString(line) {
			return nil, invalid
		}
	}
	values, err := sqlcheck.Values(out)
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(countLine[1])
	if err != nil || count != len(values) || count != len(lines)-3 {
		return nil, invalid
	}
	return values, nil
}

func storeS02MarkerReceipt(receipt s02MarkerReceipt, probe s02MarkerProbe) (string, string) {
	serialize := func() ([]byte, error) {
		data, err := json.MarshalIndent(receipt, "", "  ")
		return probe.redact.Redact(data), err
	}
	data, err := serialize()
	if err != nil {
		return "", "receipt serialization failed"
	}
	note := ""
	limit := evidence.DefaultLogLimits().SourceBytes
	if int64(len(data))+1 > limit {
		// Keep every member and its status/times; only oversized text is clipped.
		// The derived quota leaves room for JSON escaping and fixed receipt fields.
		quota := max(1, int(limit)/(12*(4+8*len(receipt.Members))))
		receipt.Truncated = true
		receipt.ExpectedMarker, _ = clipS02MarkerText(receipt.ExpectedMarker, quota, probe.redact)
		clipS02MarkerCommand(&receipt.Write, quota, probe.redact)
		for i := range receipt.Members {
			member := &receipt.Members[i]
			clipS02MarkerCommand(&member.Select, quota, probe.redact)
			clipS02MarkerCommand(&member.Identity, quota, probe.redact)
			values := make([]string, 0, min(len(member.Values), quota))
			left := quota
			for _, value := range member.Values {
				if left == 0 {
					member.ValuesTruncated = true
					break
				}
				clipped, truncated := clipS02MarkerText(value, left, probe.redact)
				member.ValuesTruncated = member.ValuesTruncated || truncated
				values = append(values, clipped)
				left = max(0, left-len(clipped))
			}
			member.Values = values
		}
		data, err = serialize()
		note = "receipt text was truncated at the existing source-size limit"
	}
	if err != nil || !json.Valid(data) || int64(len(data))+1 > limit {
		return "", "receipt unavailable after redaction or size limiting"
	}
	file := probe.writeFile(s02MarkerReceiptFile, append(data, '\n'))
	if file == "" {
		return "", "receipt file was not stored"
	}
	return file, note
}

func clipS02MarkerText(value string, limit int, redactor *evidence.Redactor) (string, bool) {
	redacted := redactor.Redact([]byte(value))
	if len(redacted) <= limit {
		return string(redacted), false
	}
	return strings.ToValidUTF8(string(redacted[:limit]), "?"), true
}

func clipS02MarkerCommand(command *s02MarkerCommand, limit int, redactor *evidence.Redactor) {
	var output, errorText bool
	command.Output, output = clipS02MarkerText(command.Output, limit, redactor)
	command.Error, errorText = clipS02MarkerText(command.Error, limit, redactor)
	command.TextTruncated = output || errorText
}

func noteS02MarkerReceipt(note string) {
	if note == "" {
		return
	}
	_, _ = fmt.Fprintln(GinkgoWriter, "warning: S02 marker failure observations: "+note)
	updateSummary(func(summary *evidence.Summary) {
		summary.Notes = append(summary.Notes, "S02 marker failure observations: "+note)
	})
}
