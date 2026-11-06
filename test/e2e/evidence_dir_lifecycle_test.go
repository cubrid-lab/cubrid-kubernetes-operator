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
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// useEvidenceDir makes dir the evidence directory of a unit test, as a run
// that acquired it would have, and restores the state afterwards.
func useEvidenceDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("E2E_EVIDENCE_DIR", dir)
	previous := acquiredEvidenceDir
	acquiredEvidenceDir = dir
	t.Cleanup(func() { acquiredEvidenceDir = previous })
}

// startRunState gives a test the empty run summary of a new run and restores
// the earlier state afterwards. "go test" runs these tests in the process of
// TestE2E, unless -skip leaves it out, and a finished suite leaves its results
// and labels behind: a planned scenario keeps the result the suite recorded,
// so a test would judge the suite's run instead of its own. Which directory
// the process has acquired is deliberately left as the suite left it, because
// a refusal must hold after a successful acquisition (#310).
func startRunState(t *testing.T) {
	t.Helper()
	summaryMu.Lock()
	defer summaryMu.Unlock()
	acquired, summary, labels, timedOut := acquiredEvidenceDir, runSummary, reportedLabels, timedOutStep
	t.Cleanup(func() {
		summaryMu.Lock()
		defer summaryMu.Unlock()
		acquiredEvidenceDir, runSummary, reportedLabels, timedOutStep = acquired, summary, labels, timedOut
	})
	runSummary = evidence.Summary{StartedAt: time.Now().UTC(), Environment: evidence.Environment{Level: "kind"}}
	reportedLabels, timedOutStep = map[string]bool{}, ""
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

// checkDirUnchanged reports every file of dir that differs from before, and
// any file added or removed.
func checkDirUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
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

// earlierRunDir returns a directory holding the files of an earlier run.
func earlierRunDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"summary.json":       `{"sentinel":"prior-run"}` + "\n",
		"junit.xml":          "<sentinel>prior-run</sentinel>\n",
		"prior-evidence.txt": "preserve these existing artifacts\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// checkRunIsRefused points the run at dir, which holds the files of an earlier
// run, and calls the suite's helpers in the order Ginkgo calls its hooks after
// BeforeSuite refuses the directory: nothing may be written there, and the run
// must fail. No kubectl, git or container tool is on the PATH, so that
// concludeRun cannot reach a cluster.
func checkRunIsRefused(t *testing.T, dir string) {
	t.Helper()
	before := snapshotDir(t, dir)
	t.Setenv("E2E_EVIDENCE_DIR", dir)
	t.Setenv("PATH", t.TempDir())

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

	checkDirUnchanged(t, dir, before)
}

// A run that was refused because E2E_EVIDENCE_DIR holds the files of an
// earlier run must not write there on its way out: AfterSuite still runs
// after the failed BeforeSuite, and writing the summary would replace the
// earlier run's summary.json and junit.xml (#310). The run must still fail.
// In a full "go test" this test runs after TestE2E has acquired the run's own
// directory, which is the order that exposed writes into the refused directory
// in E2E run 42214654727.
func TestRefusedEvidenceDirIsNeverWritten(t *testing.T) {
	startRunState(t)
	checkRunIsRefused(t, earlierRunDir(t))
}

// A successful acquisition must not outlive a later refusal. A run that owned
// directory A and is then refused populated directory B holds no directory at
// all: B keeps every byte, and nothing more reaches A either. The accepted
// directory is also not re-read from the environment, so that changing
// E2E_EVIDENCE_DIR after an acquisition does not redirect the evidence.
func TestRefusedEvidenceDirAfterAnAcquiredOneIsNeverWritten(t *testing.T) {
	startRunState(t)
	acquired := t.TempDir()
	t.Setenv("E2E_EVIDENCE_DIR", acquired)
	if err := acquireEvidenceDir(); err != nil {
		t.Fatalf("an empty directory was refused: %v", err)
	}
	planRun()
	acquiredFiles := snapshotDir(t, acquired)
	if len(acquiredFiles) == 0 {
		t.Fatal("the acquired directory has no checkpoint: this test would prove nothing about it")
	}

	refused := earlierRunDir(t)
	t.Setenv("E2E_EVIDENCE_DIR", refused)
	if got := evidenceDir(); got != acquired {
		t.Errorf("after E2E_EVIDENCE_DIR changed, evidenceDir() = %q, want the acquired %q", got, acquired)
	}

	checkRunIsRefused(t, refused)

	if got := evidenceDir(); got != "" {
		t.Errorf("after the refusal, evidenceDir() = %q, want none", got)
	}
	checkDirUnchanged(t, acquired, acquiredFiles)
}

// A directory the run acquired gets the final report even though the suite's
// setup failed after that, so a failed run keeps its summary.json and
// junit.xml.
func TestAcquiredEvidenceDirGetsTheFinalReport(t *testing.T) {
	startRunState(t)
	dir := t.TempDir()
	t.Setenv("E2E_EVIDENCE_DIR", dir)
	t.Setenv("PATH", t.TempDir())

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
