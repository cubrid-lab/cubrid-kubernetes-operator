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
	"strings"
	"testing"
)

// The step to break on purpose may be named by its label or by its key in the
// scenario registry, and both name the same step: the one-broker-Pod variant
// of Broker failure recovery, and the graceful variant of primary Pod
// recovery.
func TestBreakStepTakesTheLabelOrTheKey(t *testing.T) {
	for value, want := range map[string]string{
		"S05-one-broker-pod":               "S05-one-broker-pod",
		"broker-failure-recovery-one-pod":  "S05-one-broker-pod",
		"S03-graceful":                     "S03-graceful",
		"ha-primary-pod-recovery-graceful": "S03-graceful",
		"ha-replication":                   "S02",
		"ha-token-rotation":                "S14-token-rotation",
	} {
		t.Setenv(breakStepEnv, value)
		t.Setenv(breakUnrestorableEnv, "true")
		if err := checkBreakSettings(); err != nil {
			t.Errorf("%s=%s was refused: %v", breakStepEnv, value, err)
		}
		if got, err := breakStepLabel(); err != nil || got != want {
			t.Errorf("%s=%s breaks %q (%v), want %q", breakStepEnv, value, got, err, want)
		}
	}
}

// A value that names no HA step after the HA cluster is formed is refused
// before the suite starts, with the labels and keys it takes: a misspelling,
// the standalone and cluster-forming steps, a family that stands for several
// steps, the legacy slash form, and a VM-lab key. Without it the run would
// break nothing.
func TestBreakStepRefusesANameOfNoHAStep(t *testing.T) {
	for _, value := range []string{"S99", "broker-failure-recovery-one-pd", "S00", "standalone-database-sql",
		"S01", "ha-broker-access", "S03", "ha-primary-pod-recovery", "S05/one-broker-pod",
		"installed-operator-failure-recovery-restart", "S04-power-off", "S13-retain",
		"cluster-deletion-retention-delete"} {
		t.Setenv(breakStepEnv, value)
		err := checkBreakSettings()
		if err == nil {
			t.Errorf("%s=%s was accepted", breakStepEnv, value)
			continue
		}
		if !strings.Contains(err.Error(), "S05-one-broker-pod or broker-failure-recovery-one-pod") {
			t.Errorf("%s=%s: %v; want the labels and keys it takes", breakStepEnv, value, err)
		}
	}
	t.Setenv(breakStepEnv, "")
	t.Setenv(breakUnrestorableEnv, "true")
	if err := checkBreakSettings(); err == nil {
		t.Errorf("%s without %s was accepted", breakUnrestorableEnv, breakStepEnv)
	}
}
