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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validArgs(dir string) []string {
	return []string{
		"-kubeconfig", "/nonexistent/kubeconfig", "-context", "lab", "-cluster-uid", "uid",
		"-namespace", "db", "-cluster", "ha", "-operator-namespace", "op", "-operator-deployment", "mgr",
		"-prometheus-namespace", "monitoring", "-prometheus-service", "prom",
		"-commit", strings.Repeat("a", 40), "-level", "vm-lab", "-evidence-dir", dir,
	}
}

func without(args []string, flagName string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == flagName {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestRunRefusesAnIncompleteSelection(t *testing.T) {
	dir := t.TempDir()
	for _, missing := range []string{"-kubeconfig", "-context", "-cluster-uid", "-namespace", "-cluster",
		"-operator-namespace", "-operator-deployment", "-prometheus-namespace", "-prometheus-service", "-evidence-dir"} {
		err := run(context.Background(), without(validArgs(dir), missing))
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("run() without %s = %v, want an error naming it", missing, err)
		}
	}
	for name, args := range map[string][]string{
		"short commit":      append(without(validArgs(dir), "-commit"), "-commit", "abc123"),
		"unknown level":     append(without(validArgs(dir), "-level"), "-level", "prod"),
		"window too narrow": append(validArgs(dir), "-interval", "10s", "-window", "15s"),
		"positional":        append(validArgs(dir), "extra"),
	} {
		if err := run(context.Background(), args); err == nil {
			t.Errorf("run() with %s = nil, want an error", name)
		}
	}
}

func TestRunRefusesAnEvidenceDirectoryInUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "samples.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), validArgs(dir))
	if err == nil || !strings.Contains(err.Error(), "samples.jsonl") {
		t.Fatalf("run() = %v, want a refusal naming what the directory holds", err)
	}
}
