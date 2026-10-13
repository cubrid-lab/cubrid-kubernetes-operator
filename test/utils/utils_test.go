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

package utils

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

// What Run logs and the error it returns are redacted; the output it
// returns is not, since callers read values such as a token from it.
func TestRunRedacts(t *testing.T) {
	const secret = "s3cr3t-value"
	previous := GinkgoWriter
	t.Cleanup(func() { GinkgoWriter = previous; redact = nil })
	SetRedaction(func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") })
	var captured bytes.Buffer
	GinkgoWriter.TeeTo(&captured)
	t.Cleanup(GinkgoWriter.ClearTeeWriters)

	out, err := Run(exec.Command("sh", "-c", "echo $0; exit 3", secret))
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error shows the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("the error lost its diagnostics: %v", err)
	}
	if !strings.Contains(out, secret) {
		t.Errorf("the returned output = %q, want it unredacted", out)
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "read %s\n", secret)
	GinkgoWriter.Printf("printf %s\n", secret)
	GinkgoWriter.Println("println", secret)
	GinkgoWriter.Print("print " + secret + "\n")
	got := captured.String()
	if strings.Contains(got, secret) {
		t.Errorf("the captured Ginkgo output shows the secret:\n%s", got)
	}
	for _, kept := range []string{"running:", "sh -c", "read [REDACTED]", "printf [REDACTED]",
		"println [REDACTED]", "print [REDACTED]"} {
		if !strings.Contains(got, kept) {
			t.Errorf("the captured Ginkgo output lost %q:\n%s", kept, got)
		}
	}
}

// A command that ends with exit code 137 was ended by SIGKILL. For a
// "kubectl exec" it is how the exec ends when its container is killed under
// it. Any other failure is not that.
func TestEndedBySIGKILL(t *testing.T) {
	exit := func(code string) error {
		return fmt.Errorf("the command failed: %w", exec.Command("sh", "-c", "exit "+code).Run())
	}
	if !EndedBySIGKILL(exit("137")) {
		t.Error("exit code 137 was not recognized")
	}
	for name, err := range map[string]error{
		"no error":              nil,
		"exit code 1":           exit("1"),
		"exit code 126":         exit("126"),
		"a command not started": exec.Command("/no/such/command").Run(),
		"another error":         errors.New("exit status 137"),
	} {
		if EndedBySIGKILL(err) {
			t.Errorf("%s was taken for a command ended by SIGKILL", name)
		}
	}
}
