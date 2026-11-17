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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const sourceURL = "https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/1"

func TestCandidateMatrix(t *testing.T) {
	root := t.TempDir()
	source, rec := fixture(t, root)
	proposed := candidate(source.Environment)
	manifest := index{Version: "1", CreatedAt: source.StartedAt, Candidate: proposed, Runs: []run{source}}
	path := filepath.Join(root, "index.json")
	writeJSON(t, path, manifest)
	var buf bytes.Buffer
	if err := generate(&buf, path, root); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/sample.md")
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(want) {
		t.Errorf("matrix differs from sample:\n%s", buf.String())
	}

	// The summary and index both claim a pass; a corrupt file must still
	// fail the current evidence check, without modifying either document.
	raw := filepath.Join(root, source.ID, rec.Scenarios[0].Evidence[0])
	if err := os.WriteFile(raw, []byte("changed after indexing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := generate(&buf, path, root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "PASS | FAIL | DIGEST MISMATCH") {
		t.Fatalf("altered evidence was not refused:\n%s", buf.String())
	}
}

func TestCandidateMatrixSeparatesRunStatusFromVerdicts(t *testing.T) {
	root := t.TempDir()
	source, rec := fixture(t, root)
	dir := filepath.Join(root, source.ID)
	proposed := candidate{Commit: strings.Repeat("b", 40), images: source.Environment.images}
	proposed.Operator = "sha256:" + strings.Repeat("b", 64)
	got := inspect(proposed, source, dir)
	if !strings.Contains(got.status, "OTHER REVISION, OTHER IMAGES") ||
		got.results[0].judged != evidence.Pass || source.Level != evidence.LevelVMLab {
		t.Fatalf("other candidate: %+v", got)
	}
	rec.FinishedAt = rec.StartedAt.AddDate(-1, 0, 0)
	source.FinishedAt = rec.FinishedAt
	if status := inspect(proposed, source, dir).status; status != "INDEX MISMATCH" {
		t.Fatalf("incorrect index metadata: %s", status)
	}
	if status := inspect(proposed, source, filepath.Join(dir, "absent")).status; status != "MISSING BUNDLE" {
		t.Fatalf("missing bundle: %s", status)
	}

	source.Lane = "kind-filtered"
	source.Level = evidence.LevelKind
	gotStatus := runStatus(proposed, source, evidence.RecordedRun{Provisional: true}, "")
	if !strings.HasPrefix(gotStatus, "PARTIAL, FILTERED, ") {
		t.Fatalf("filtered checkpoint: %s", gotStatus)
	}
}

func TestCandidateMatrixRejectsUnsafeSources(t *testing.T) {
	root := t.TempDir()
	source, _ := fixture(t, root)
	manifest := index{Version: "1", CreatedAt: source.StartedAt,
		Candidate: candidate(source.Environment), Runs: []run{source}}
	path := filepath.Join(root, "index.json")
	for _, url := range []string{sourceURL + "?token=secret", "https://private.example/runs/1",
		"https://user:secret@github.com/runs/1", sourceURL + "#private"} {
		manifest.Runs[0].Source = url
		writeJSON(t, path, manifest)
		var buf bytes.Buffer
		if err := generate(&buf, path, root); err == nil || buf.Len() != 0 {
			t.Fatalf("unapproved URL produced a report: err=%v", err)
		}
	}
}

func fixture(t *testing.T, root string) (run, evidence.Summary) {
	t.Helper()
	fixtureDir := filepath.Join("..", "..", "testdata", "v2", "lab-operator-restart-run")
	data, err := os.ReadFile(filepath.Join(fixtureDir, summaryPath))
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	operator := "sha256:" + strings.Repeat("1", 64)
	engine := "sha256:" + strings.Repeat("2", 64)
	data = []byte(strings.NewReplacer("0782259", commit, "sha256:1111111111111111", operator,
		"sha256:2222222222222222", engine).Replace(string(data)))
	var rec evidence.Summary
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, rec.RunID)
	source := run{ID: rec.RunID, Source: sourceURL, Version: rec.SchemaVersion, Lane: rec.Lane,
		Level: rec.Environment.Level, StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt,
		Environment: runEnvironment{Commit: commit, images: images{operator, engine}}}
	source.Files = append(source.Files, writeFile(t, dir, summaryPath, data))
	junit, err := os.ReadFile(filepath.Join(fixtureDir, junitPath))
	if err != nil {
		t.Fatal(err)
	}
	source.Files = append(source.Files, writeFile(t, dir, junitPath, junit))
	for _, sc := range rec.Scenarios {
		for _, path := range sc.Evidence {
			source.Files = append(source.Files, writeFile(t, dir, path, []byte("synthetic evidence\n")))
		}
	}
	read, err := evidence.ReadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	source.Scenarios = make([]scenario, 0, len(read.Results))
	for _, result := range read.Results {
		sc := result.Scenario
		source.Scenarios = append(source.Scenarios, scenario{
			ID: sc.ID, Key: result.Key, Variant: sc.Variant, Recorded: sc.Result, Judged: result.Judged, Evidence: sc.Evidence,
		})
	}
	return source, rec
}

func writeFile(t *testing.T, dir, rel string, data []byte) indexedFile {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return indexedFile{Path: rel, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), State: "listed"}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Dir(path), filepath.Base(path), data)
}
