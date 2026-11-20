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
	"strings"
	"testing"
	"time"
)

// TestBuildWithRetryTriesAgainAfterAPause checks that a build that fails once,
// as one whose base image or driver download hit a passing network error, is
// built again after the pause and passes.
func TestBuildWithRetryTriesAgainAfterAPause(t *testing.T) {
	calls := 0
	var slept []time.Duration
	err := buildWithRetry(func() (string, error) {
		calls++
		if calls == 1 {
			return "step 1\nERROR: failed to fetch\n", errors.New("exit status 1")
		}
		return "built", nil
	}, 2, 30*time.Second, func(d time.Duration) { slept = append(slept, d) }, func(string) {})
	if err != nil || calls != 2 {
		t.Fatalf("err %v after %d builds, want success on the second", err, calls)
	}
	if len(slept) != 1 || slept[0] != 30*time.Second {
		t.Errorf("paused %v, want once for 30s", slept)
	}
}

// TestBuildWithRetryReportsTheEndOfTheOutput checks that a build that keeps
// failing gives up after its attempts, and that its error holds the end of
// the last output, where Docker names the cause, not the start (#1004's
// sibling: the failure message used to cut the output off before the cause).
func TestBuildWithRetryReportsTheEndOfTheOutput(t *testing.T) {
	var head []string
	for i := range 200 {
		head = append(head, fmt.Sprintf("noise line %d", i))
	}
	output := strings.Join(head, "\n") + "\nERROR: failed to solve: eclipse-temurin:17-jdk: 429 Too Many Requests\n"
	calls := 0
	var logged []string
	err := buildWithRetry(func() (string, error) {
		calls++
		// As utils.Run does: the error repeats the whole output.
		return output, fmt.Errorf("%q failed with error %q: %w", "make docker-build-workload", output,
			errors.New("exit status 2"))
	}, 2, time.Second, func(time.Duration) {}, func(s string) { logged = append(logged, s) })
	if err == nil || calls != 2 {
		t.Fatalf("err %v after %d builds, want a failure after 2", err, calls)
	}
	if !strings.Contains(err.Error(), "429 Too Many Requests") {
		t.Errorf("the error does not hold the cause at the end of the output: %s", err)
	}
	if strings.Contains(err.Error(), "noise line 0") {
		t.Errorf("the error holds the start of a long output: %d bytes", len(err.Error()))
	}
	// Gomega cuts a failure message off after 4000 characters.
	if len(err.Error()) > 4000 {
		t.Errorf("the error is %d bytes, longer than a failure message shows", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("the error does not name the exit status: %s", err)
	}
	if len(logged) != 2 {
		t.Errorf("logged %d failed attempts, want 2", len(logged))
	}
}

func TestOutputTail(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc"},
		{"a\nb", 5, "a\nb"},
		{"", 3, ""},
	} {
		if got := outputTail(tc.in, tc.n); got != tc.want {
			t.Errorf("outputTail(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
