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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// A file that holds a credential of the run must keep the whole evidence
// directory from being published, and the marker must not repeat the value.
func TestCredentialLeakHoldsTheEvidence(t *testing.T) {
	const secret = "a-credential-of-the-run"
	dir := filepath.Join(t.TempDir(), "e2e-evidence")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clean.txt"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := redactor
	redactor = evidence.NewRedactor(secret)
	t.Cleanup(func() { redactor = old })

	if problems := credentialLeaks(dir); len(problems) != 0 {
		t.Fatalf("clean evidence: problems = %v", problems)
	}
	if _, err := os.Stat(dir + evidenceHoldSuffix); !os.IsNotExist(err) {
		t.Fatalf("clean evidence was held: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "leak.txt"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	if problems := credentialLeaks(dir); len(problems) != 1 {
		t.Fatalf("leaked evidence: problems = %v", problems)
	}
	held, err := os.ReadFile(dir + evidenceHoldSuffix)
	if err != nil {
		t.Fatalf("leaked evidence was not held: %v", err)
	}
	if !strings.Contains(string(held), "leak.txt") || strings.Contains(string(held), secret) {
		t.Fatalf("the hold marker = %q; it names the file and never the value", held)
	}
}

// Evidence that could not be searched is not known to be clean, so it is held
// too.
func TestUnsearchableEvidenceIsHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "e2e-evidence")
	old := redactor
	redactor = evidence.NewRedactor("a-credential-of-the-run")
	t.Cleanup(func() { redactor = old })

	if problems := credentialLeaks(dir); len(problems) != 1 {
		t.Fatalf("missing directory: problems = %v", problems)
	}
	if _, err := os.Stat(dir + evidenceHoldSuffix); err != nil {
		t.Fatalf("evidence that could not be searched was not held: %v", err)
	}
}
