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

// Package suitecheck runs a Ginkgo suite with the redaction of test/utils
// installed. It is a package of its own because a Ginkgo suite cannot run
// with go test -count, which the tests of test/utils support.
package suitecheck

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// The redaction installed in a suite's BeforeSuite, as the e2e suite does,
// leaves Ginkgo working and redacts what the specs write.
func TestRedactionInASuite(t *testing.T) {
	const secret = "s3cr3t-in-suite"
	var captured bytes.Buffer
	BeforeSuite(func() {
		utils.SetRedaction(func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") })
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
