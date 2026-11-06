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

package installed

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// laneOptions are the HAConfig fields that select a scenario after LAB01. A
// new one is found by its name, so that a lane cannot be added here unseen.
func laneOptions(t *testing.T) []string {
	t.Helper()
	var options []string
	for field := range reflect.TypeFor[HAConfig]().Fields() {
		if field.Type.Kind() == reflect.Bool &&
			(strings.HasPrefix(field.Name, "Backup") || strings.HasPrefix(field.Name, "Restore")) {
			options = append(options, field.Name)
		}
	}
	if len(options) == 0 {
		t.Fatal("no option selects a scenario")
	}
	return options
}

// laneConfig is a configuration that validate accepts, apart from the options
// it is given: every limit is set, and an option has what it follows.
func laneConfig(t *testing.T, options ...string) HAConfig {
	t.Helper()
	cfg := testHAConfig(t)
	cfg.NamespaceMode = FreshNamespace
	value := reflect.ValueOf(&cfg).Elem()
	for _, field := range value.Fields() {
		if field.Type() == reflect.TypeFor[time.Duration]() {
			field.SetInt(int64(time.Hour))
		}
	}
	cfg.BackupBucket, cfg.ObjectStoreService = "backups", "object-store/store:8333"
	cfg.ResyncLimit, cfg.LeaderLease = time.Hour, "leader"
	for _, option := range options {
		value.FieldByName(option).SetBool(true)
		cfg.Restore = cfg.Restore || strings.HasPrefix(option, "Restore")
	}
	return cfg
}

// The lanes the commands assemble are the lanes of the registry, whichever
// option selects them, and every registered lab lane is reached. A pair of
// options validate accepts is judged the same way: such a run must not end in
// a lane with requirements of its own.
func TestInstalledLanesMatchTheRegistry(t *testing.T) {
	seen := map[string]bool{}
	check := func(what, lane string, requirements []evidence.Requirement) {
		t.Helper()
		seen[lane] = true
		for _, problem := range (evidence.Lane{Name: lane, Required: requirements}).RegistryProblems(evidence.LevelVMLab) {
			t.Errorf("%s: %s", what, problem)
		}
	}
	checkHA := func(what string, cfg HAConfig) {
		t.Helper()
		lane, requirements := planHARun(cfg, &evidence.Summary{Notes: []string{""}})
		check(what, lane, requirements)
	}

	for _, mode := range []NamespaceMode{FreshNamespace, PreallocatedNamespace} {
		lane, variant := Config{NamespaceMode: mode}.smokeLane()
		check(string(mode), lane, []evidence.Requirement{{ID: scenarioID, Variant: variant}})
	}
	checkHA("no option", testHAConfig(t))
	for _, fault := range []string{FaultVMPowerOff, FaultOperatorRestart} {
		cfg := testHAConfig(t)
		cfg.Fault = fault
		checkHA(fault, cfg)
	}
	backup := testHAConfig(t)
	backup.BackupBucket = "backups"
	checkHA("a backup bucket", backup)
	restore := backup
	restore.Restore = true
	checkHA("a restore", restore)

	options := laneOptions(t)
	for i, first := range options {
		cfg := laneConfig(t, first)
		if err := cfg.validate(); err != nil {
			t.Errorf("%s alone is refused, so its lane is not checked: %v", first, err)
		}
		checkHA(first, cfg)
		for _, second := range options[i+1:] {
			if cfg := laneConfig(t, first, second); cfg.validate() == nil {
				checkHA(first+" and "+second, cfg)
			}
		}
	}
	for at := range imRestartPlacementNote {
		cfg := laneConfig(t, "RestoreInstanceManagerRestart")
		cfg.RestoreInstanceManagerRestartAt = at
		checkHA("the Instance Manager restart at "+at, cfg)
	}
	for at := range stopPlacements {
		cfg := laneConfig(t, "RestoreGracefulStop")
		cfg.RestoreGracefulStopAt = at
		checkHA("the graceful stop at "+at, cfg)
	}

	for _, lane := range evidence.RegisteredLanes(evidence.LevelVMLab) {
		if !seen[lane] {
			t.Errorf("no option of the commands reaches the registered lane %s", lane)
		}
	}
	if lanes := evidence.RegisteredLanes(evidence.LevelVMLab); len(lanes) == 0 || slices.Contains(lanes, "kind") {
		t.Errorf("registered lab lanes = %q", lanes)
	}
}
