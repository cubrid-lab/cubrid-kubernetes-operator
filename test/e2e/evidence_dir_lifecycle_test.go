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
	"os"
	"path/filepath"
	"testing"
)

// useEvidenceDir makes dir the evidence directory of a unit test, as a run
// that acquired it would have, and restores the state afterwards.
func useEvidenceDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("E2E_EVIDENCE_DIR", dir)
	evidenceAcquired = true
	t.Cleanup(func() { evidenceAcquired = false })
}

// snapshotDir returns the name and content of every file below dir.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A run that was refused because E2E_EVIDENCE_DIR holds the files of an
// earlier run must not write there on its way out: AfterSuite still runs
// after the failed BeforeSuite, and writing the summary would replace the
// earlier run's summary.json and junit.xml (#310). The run must still fail.
func TestRefusedEvidenceDirIsNeverWritten(t *testing.T) {
	dir := t.TempDir()
	prior := map[string]string{
		"summary.json":       `{"sentinel":"prior-run"}` + "\n",
		"junit.xml":          "<sentinel>prior-run</sentinel>\n",
		"prior-evidence.txt": "preserve these existing artifacts\n",
	}
	for name, content := range prior {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotDir(t, dir)
	t.Setenv("E2E_EVIDENCE_DIR", dir)
	// No kubectl, git or container tool: concludeRun must not reach a cluster.
	t.Setenv("PATH", t.TempDir())
	oldSummary := runSummary
	t.Cleanup(func() { runSummary, evidenceAcquired = oldSummary, false })

	// What BeforeSuite does first.
	if err := acquireEvidenceDir(); err == nil {
		t.Fatal("a directory holding an earlier run's files was accepted")
	}

	// What the rest of the lifecycle does after the refusal.
	planRun()
	checkpointRun()
	if rel := writeScenarioFile("S00/log.txt", []byte("x")); rel != "" {
		t.Errorf("a scenario file was written to a refused directory: %s", rel)
	}
	if err := concludeRun(); err == nil {
		t.Error("a refused run was reported as passed")
	}

	after := snapshotDir(t, dir)
	if len(after) != len(before) {
		t.Errorf("files before %d, after %d: %v", len(before), len(after), after)
	}
	for path, content := range before {
		if after[path] != content {
			t.Errorf("%s changed:\nbefore: %q\nafter:  %q", filepath.Base(path), content, after[path])
		}
	}
}

// A directory the run acquired gets the final report even though the suite's
// setup failed after that, so a failed run keeps its summary.json and
// junit.xml.
func TestAcquiredEvidenceDirGetsTheFinalReport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("E2E_EVIDENCE_DIR", dir)
	t.Setenv("PATH", t.TempDir())
	oldSummary := runSummary
	t.Cleanup(func() { runSummary, evidenceAcquired = oldSummary, false })

	if err := acquireEvidenceDir(); err != nil {
		t.Fatalf("an empty directory was refused: %v", err)
	}
	planRun()
	if err := concludeRun(); err == nil {
		t.Error("a run none of whose scenarios ran was reported as passed")
	}
	for _, name := range []string{"summary.json", "junit.xml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("the acquired directory has no %s: %v", name, err)
		}
	}
}
