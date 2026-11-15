//go:build e2e
// +build e2e

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
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// A failed fault must keep its first verdict. Evidence collection records the
// independent SQL result when one is available; it must not turn a failed read
// into a synthetic empty/zero-loss report (#897).
func TestS03FailedOutcomeFilesKeepRealDataAndReadFailures(t *testing.T) {
	const history = "{\"opId\":\"s03f-000001\",\"event\":\"acknowledged\"}\n"
	for _, tc := range []struct {
		name           string
		history        string
		historyErr     error
		report         workload.Report
		dataErr        error
		wantFiles      []string
		wantError      string
		wantReport     bool
		missingRecords int
	}{
		{
			name:    "row divergence is not hidden",
			history: history,
			report: workload.Report{
				OK:                  false,
				AcknowledgedMissing: 2,
				Members:             map[string]workload.MemberCheck{"hab-0": {Rows: 2}, "hab-1": {Rows: 1}},
			},
			wantFiles:      []string{"client-history.jsonl", "data-check.json"},
			wantReport:     true,
			missingRecords: 2,
		},
		{
			name:      "member read fails",
			history:   history,
			dataErr:   errors.New("hab-2: member not reachable"),
			wantFiles: []string{"client-history.jsonl", "data-check-error.txt"},
			wantError: "member not reachable",
		},
		{
			name:       "client history is unavailable",
			historyErr: errors.New("client pod unavailable"),
			dataErr:    errors.New("not run: client history was unavailable"),
			wantFiles:  []string{"client-history-error.txt", "data-check-error.txt"},
			wantError:  "not run",
		},
		{
			name:      "empty data report is not a pass",
			history:   history,
			wantFiles: []string{"client-history.jsonl", "data-check-error.txt"},
			wantError: "no member data report",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			write := func(path string, data []byte) string {
				files[path] = string(data)
				return path
			}
			got := s03FailedOutcomeFiles(s03AbruptFirst, tc.history, tc.historyErr, tc.report, tc.dataErr, write)
			if len(got) != len(tc.wantFiles) {
				t.Fatalf("saved %v, want %v", got, tc.wantFiles)
			}
			for i, want := range tc.wantFiles {
				path := "S03/" + s03AbruptFirst + "/" + want
				if got[i] != path {
					t.Errorf("file %d = %s, want %s", i, got[i], path)
				}
			}
			if tc.wantReport {
				var saved workload.Report
				if err := json.Unmarshal([]byte(files["S03/"+s03AbruptFirst+"/data-check.json"]), &saved); err != nil {
					t.Fatalf("bad saved data report: %v", err)
				}
				if saved.OK || saved.AcknowledgedMissing != tc.missingRecords {
					t.Errorf("data-check was made to look healthy: %+v", saved)
				}
			} else if _, found := files["S03/"+s03AbruptFirst+"/data-check.json"]; found {
				t.Error("an incomplete/unavailable data check was written as a report")
			}
			if tc.wantError != "" {
				errtext := files["S03/"+s03AbruptFirst+"/data-check-error.txt"]
				if !strings.Contains(errtext, tc.wantError) {
					t.Errorf("error evidence = %q, want %q", errtext, tc.wantError)
				}
			}
		})
	}
}

func TestS03FailureEvidenceCountsOnlyFilesActuallyWritten(t *testing.T) {
	got := s03FailedOutcomeFiles(s03Graceful, "", errors.New("no client history"), workload.Report{},
		errors.New("not run"), func(path string, _ []byte) string {
			if strings.HasSuffix(path, "client-history-error.txt") {
				return ""
			}
			return path
		})
	if len(got) != 1 || got[0] != "S03/graceful/data-check-error.txt" {
		t.Errorf("recorded files that were not written: %v", got)
	}
}
