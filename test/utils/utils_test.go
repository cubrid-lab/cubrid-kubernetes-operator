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

// The redaction installed in a suite's BeforeSuite, as the e2e suite does,
// leaves Ginkgo working and redacts what the specs write.
func TestRedactionInASuite(t *testing.T) {
	const secret = "s3cr3t-in-suite"
	previous := GinkgoWriter
	t.Cleanup(func() { GinkgoWriter = previous; redact = nil })
	var captured bytes.Buffer
	BeforeSuite(func() {
		SetRedaction(func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") })
		GinkgoWriter.TeeTo(&captured)
	})
	It("writes the secret", func() {
		_, _ = fmt.Fprintf(GinkgoWriter, "wrote %s\n", secret)
	})
	RunSpecs(t, "redaction")
	got := captured.String()
	if strings.Contains(got, secret) || !strings.Contains(got, "wrote [REDACTED]") {
		t.Errorf("the spec's output was not redacted:\n%s", got)
	}
}
