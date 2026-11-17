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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const restartLane = "lab-installed-ha-s06"

func TestPlanRequiresExactCommitImagesAndLane(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name   string
		change func(*plan)
		want   string
	}{
		{name: "valid"},
		{name: "abbreviated commit", change: func(p *plan) { p.Commit = head[:12] }, want: "full lowercase Git SHA"},
		{name: "other commit", change: func(p *plan) { p.Commit = strings.Repeat("b", 40) }, want: "local checkout"},
		{name: "mutable tag", change: func(p *plan) { p.OperatorImage = "team/operator:main-" + head },
			want: "operatorImage"},
		{name: "credential in image", change: func(p *plan) { p.InstanceManagerImage = "user:secret@" + p.ClientImage },
			want: "instanceManagerImage"},
		{name: "missing digest", change: func(p *plan) { p.ClientImage = "team/client:latest" }, want: "clientImage"},
		{name: "missing scenario", change: func(p *plan) { p.ScenarioKeys = p.ScenarioKeys[:1] }, want: "all registered"},
		{name: "duplicate scenario", change: func(p *plan) { p.ScenarioKeys[1] = p.ScenarioKeys[0] },
			want: "all registered"},
		{name: "different level", change: func(p *plan) { p.Lane = "kind" }, want: "one VM-lab lane"},
		{name: "missing destination", want: "existing readable local directory",
			change: func(p *plan) { p.EvidenceDirectory = filepath.Join(p.EvidenceDirectory, "absent") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := localPlan(head, t.TempDir())
			if tc.change != nil {
				tc.change(&p)
			}
			err := p.validate(head)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "user:secret") {
				t.Errorf("expected a sanitized %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestPlanDoesNotChangeExistingEvidence(t *testing.T) {
	head := strings.Repeat("a", 40)
	dir := t.TempDir()
	path := filepath.Join(dir, "prior-run-private.json")
	const evidence = "immutable evidence"
	write(t, path, []byte(evidence))
	err := localPlan(head, dir).validate(head)
	if err == nil || !strings.Contains(err.Error(), "must be empty") ||
		strings.Contains(err.Error(), filepath.Base(path)) {
		t.Fatalf("expected a sanitized error for existing evidence, got %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != evidence {
		t.Fatalf("existing evidence was modified: %v", err)
	}
}

func TestReadPlanRejectsUnexpectedInputsWithoutEchoingThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	head := strings.Repeat("a", 40)
	want := localPlan(head, t.TempDir())
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, data)
	got, err := readPlan(path)
	if err != nil || got.Commit != head || got.Lane != restartLane {
		t.Fatalf("valid plan was not read: %v", err)
	}
	for _, input := range []string{`{"kubeconfig":"private-value"}`, `{} {"token":"private-value"}`,
		strings.Repeat(" ", maxPlanBytes+1)} {
		write(t, path, []byte(input))
		if _, err := readPlan(path); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("expected a sanitized error, got %v", err)
		}
	}
}

func localPlan(head, directory string) plan {
	pin := "@sha256:" + strings.Repeat("1", 64)
	return plan{
		Commit: head, Lane: restartLane,
		ScenarioKeys:  []string{"installed-ha-sql-smoke", "installed-operator-failure-recovery-restart"},
		OperatorImage: "registry.example:5000/team/operator" + pin, InstanceManagerImage: "team/member" + pin,
		ClientImage: "team/client" + pin, EvidenceDirectory: directory,
	}
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
