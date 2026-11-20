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
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

const (
	// workloadBuildAttempts is how many times the workload client image is
	// built before the step fails. The build pulls a base image and downloads
	// the JDBC driver, and a passing network error there failed the whole run.
	workloadBuildAttempts = 2
	// workloadBuildPause is the wait before building again.
	workloadBuildPause = 30 * time.Second
	// workloadBuildTail is how many of the last lines of a failed build's
	// output a failure reports: Docker names the cause at the end, and the
	// whole output is longer than a failure message shows.
	workloadBuildTail = 40
)

// buildWorkloadImage builds the test-only workload client image, once more
// after a pause when a build fails.
func buildWorkloadImage() error {
	return buildWithRetry(func() (string, error) {
		return utils.Run(exec.Command("make", "docker-build-workload",
			fmt.Sprintf("WORKLOAD_IMG=%s", workloadImage)))
	}, workloadBuildAttempts, workloadBuildPause, time.Sleep, func(s string) {
		_, _ = fmt.Fprintln(GinkgoWriter, s)
	})
}

// buildWithRetry calls build up to attempts times, pausing between them, and
// returns nil at its first success. Each failure is logged with the end of
// its output; the error after the last one holds the end of that output.
func buildWithRetry(build func() (string, error), attempts int, pause time.Duration,
	sleep func(time.Duration), log func(string)) error {
	var output string
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if output, err = build(); err == nil {
			return nil
		}
		log(fmt.Sprintf("the build failed (attempt %d of %d): %v; the end of its output:\n%s",
			attempt, attempts, buildCause(err), outputTail(output, workloadBuildTail)))
		if attempt < attempts {
			sleep(pause)
		}
	}
	return fmt.Errorf("the build failed %d times (%v); the end of the last output:\n%s",
		attempts, buildCause(err), outputTail(output, workloadBuildTail))
}

// buildCause is the error of a failed build without the build's output.
// utils.Run puts the whole output into its error, and a failure message that
// starts with it is cut off before the end the message adds.
func buildCause(err error) error {
	if inner := errors.Unwrap(err); inner != nil {
		return inner
	}
	return err
}

// outputTail returns the last n lines of s, without a trailing newline.
func outputTail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
