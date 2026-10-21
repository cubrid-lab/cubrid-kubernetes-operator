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
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// hangingKubectl puts a kubectl on PATH that never returns by itself, as a
// kubectl does that waits on an unreachable API server.
func hangingKubectl(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const pod0 = "pod-0"

// TestCallbackHelpersEndWithTheStepContext checks that every helper a Before or
// Check callback calls ends its command when the context of the step ends. A
// kubectl that stays would outlive the step the flow cancelled.
func TestCallbackHelpersEndWithTheStepContext(t *testing.T) {
	r := newHARun("ns", "client-ns", "cluster", "db", []string{pod0, "pod-1", "pod-2"})
	helpers := map[string]func(ctx context.Context){
		"kubectl":       func(ctx context.Context) { _, _ = r.kubectl(ctx, "get", "pods") },
		"client":        func(ctx context.Context) { _, _ = r.client(ctx, "true") },
		"master":        func(ctx context.Context) { _, _, _ = r.master(ctx) },
		"status":        func(ctx context.Context) { _, _ = r.status(ctx) },
		"dataCheck":     func(ctx context.Context) { _, _ = r.dataCheck(ctx) },
		"formationTime": func(ctx context.Context) { _, _ = r.formationTime(ctx) },
		"activeMasters": func(ctx context.Context) { _ = r.activeMasters(ctx) },
		"activeMasterAmong": func(ctx context.Context) {
			_ = r.activeMasterAmong(ctx, []string{pod0})
		},
		"lastAnswerAcknowledged": func(ctx context.Context) { _ = r.lastAnswerAcknowledged(ctx, "c") },
		"memberValues": func(ctx context.Context) {
			_, _ = r.memberValues(ctx, pod0, "SELECT 1;")
		},
		"identities": func(ctx context.Context) { _ = r.identities(ctx, []string{pod0}) },
		"roleLog.sample": func(ctx context.Context) {
			var roles roleLog
			roles.sample(ctx, r)
		},
		"operatorAvailable": func(ctx context.Context) { _ = operatorAvailable(ctx) },
		"readS00Row": func(ctx context.Context) {
			_ = readS00Row(csqlInPod(ctx, "ns", pod0, "db"), "m")
		},
	}
	hangingKubectl(t)
	for name, call := range helpers {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			done := make(chan struct{})
			go func() { call(ctx); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not return within 5s after its context ended", name)
			}
		})
	}
}
