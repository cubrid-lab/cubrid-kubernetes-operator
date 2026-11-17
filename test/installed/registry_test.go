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
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
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
		lane, requirements, err := planHARun(cfg, &evidence.Summary{Notes: []string{""}})
		if err != nil {
			t.Fatal(err)
		}
		check(what, lane, requirements)
	}

	for _, mode := range []NamespaceMode{FreshNamespace, PreallocatedNamespace} {
		cfg := Config{NamespaceMode: mode}
		lane, _ := cfg.smokeLane()
		requirements, err := cfg.smokeRequirements()
		if err != nil {
			t.Fatal(err)
		}
		check(string(mode), lane, requirements)
	}
	checkHA("no option", testHAConfig(t))
	for _, fault := range []string{FaultVMPowerOff, FaultOperatorRestart, FaultDrainSlaveNode, FaultDrainMasterNode,
		FaultPartitionMasterIsolated, FaultPartitionSlaveIsolated, FaultPartitionHigherRankIsolated} {
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

// Independent controls pin the two smoke lanes without deriving their required
// identities from the registry or from the declaration under test.
const legacySmokeID = "LAB00"
const legacySmokeRecord = "LAB00/record.json"
const smokeUnitLevel = "unit"

func smokeDeclarationCases() []struct {
	mode               NamespaceMode
	lane, key, variant string
} {
	const (
		freshKey            = "installed-standalone-sql-smoke"
		preallocatedKey     = "installed-preallocated-standalone-smoke"
		preallocatedVariant = "preallocated"
	)
	return []struct {
		mode               NamespaceMode
		lane, key, variant string
	}{
		{FreshNamespace, "lab-installed-smoke", freshKey, ""},
		{PreallocatedNamespace, "lab-preallocated-component-smoke", preallocatedKey, preallocatedVariant},
	}
}

func TestSmokeRequirementsKeepLegacyDeclarations(t *testing.T) {
	for _, tc := range smokeDeclarationCases() {
		for _, level := range []string{smokeUnitLevel, evidence.LevelKind, evidence.LevelVMLab} {
			t.Run(string(tc.mode)+"/"+level, func(t *testing.T) {
				cfg := Config{NamespaceMode: tc.mode, Level: level}
				got, err := cfg.smokeRequirements()
				if err != nil {
					t.Fatalf("complete smoke declaration returned an error: %v", err)
				}
				want := []evidence.Requirement{{ID: legacySmokeID, Variant: tc.variant}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("requirements=%#v, want independent legacy control %#v", got, want)
				}
				lane, variant := cfg.smokeLane()
				if lane != tc.lane || variant != tc.variant || cfg.Level != level {
					t.Fatal("smoke selection or reporting level changed")
				}
				resolved, err := evidence.ResolveAlias(lane, evidence.LevelVMLab,
					(evidence.Scenario{ID: got[0].ID, Variant: got[0].Variant}).Name())
				if err != nil || resolved.Key != tc.key {
					t.Fatalf("wrong declared result key %q: %v", resolved.Key, err)
				}
			})
		}
	}
	cfg := Config{}
	got, err := cfg.smokeRequirements()
	if err != nil || !reflect.DeepEqual(got, []evidence.Requirement{{ID: legacySmokeID}}) {
		t.Fatalf("default namespace declaration=%#v err=%v", got, err)
	}
}

// newerHAKeys are the checks added after the 24-lane baseline below was
// frozen; their plans are covered by their own tests.
var newerHAKeys = []string{"installed-node-drain-slave-node", "installed-node-drain-master-node",
	"installed-ha-network-partition-master-isolated", "installed-ha-network-partition-slave-isolated",
	"installed-ha-network-partition-higher-rank-isolated"}

const plainHALegacyID = "LAB01"
const plainHALane = "lab-installed-ha"
const plainHASummaryFile = "summary.json"
const plainHAJUnitFile = "junit.xml"

// The inputs are the existing hand-written legacy option controls, not a
// registry-generated list. The frozen digest below protects every lane's
// requirement order and notes across key declaration changes.
func TestHALaneDeclarationsPreserveExistingPlans(t *testing.T) {
	type snapshot struct {
		Lane     string
		Required []evidence.Requirement
		Notes    []string
	}
	plans := make(map[string]snapshot, len(legacyHAOptions))
	for key, legacy := range legacyHAOptions {
		if slices.Contains(newerHAKeys, key) {
			continue
		}
		cfg := testHAConfig(t)
		if legacy.backup {
			cfg.BackupBucket = testBackupBucket
		}
		legacy.set(&cfg)
		summary := evidence.Summary{Notes: []string{""}}
		lane, required, err := planHARun(cfg, &summary)
		if err != nil {
			t.Fatalf("complete HA declaration returned an error: %v", err)
		}
		plans[key] = snapshot{lane, required, summary.Notes}
	}
	if len(plans) != 24 {
		t.Fatalf("planned %d HA lanes, want the existing 24", len(plans))
	}
	data, err := json.Marshal(plans)
	if err != nil {
		t.Fatal(err)
	}
	// Captured before production edits at 1b75f11 on 2026-11-16 UTC.
	const baseline = "d7b794e4c24831d70c368cfddad36168c60ec7282c939b73e3ddf3c63da0c63b"
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != baseline {
		t.Fatalf("HA requirements or notes changed from the 24-lane legacy baseline: %s\n%s", got, data)
	}
}

func TestPlainHADeclarationKeepsLegacyRequirements(t *testing.T) {
	for _, mode := range []NamespaceMode{"", FreshNamespace} {
		for _, level := range []string{smokeUnitLevel, evidence.LevelKind, evidence.LevelVMLab} {
			cfg := testHAConfig(t)
			cfg.NamespaceMode = mode
			cfg.Level = level
			summary := evidence.Summary{Notes: []string{"original note"}}
			lane, required, err := planHARun(cfg, &summary)
			if err != nil {
				t.Fatalf("complete HA declaration returned an error: %v", err)
			}
			if lane != plainHALane || !reflect.DeepEqual(required, []evidence.Requirement{{ID: plainHALegacyID}}) ||
				!slices.Equal(summary.Notes, []string{"original note"}) ||
				cfg.Level != level || cfg.NamespaceMode != mode {
				t.Fatalf("plain plan changed legacy identity/notes/config: lane=%s required=%#v notes=%q",
					lane, required, summary.Notes)
			}
		}
	}
}

func TestPlainHADeclarationPreservesRunnerOutputs(t *testing.T) {
	for _, mode := range []NamespaceMode{"", FreshNamespace} {
		for _, level := range []string{smokeUnitLevel, evidence.LevelKind, evidence.LevelVMLab} {
			t.Run(string(mode)+"/"+level, func(t *testing.T) {
				cfg := testHAConfig(t)
				cfg.NamespaceMode = mode
				cfg.Level = level
				fixture := newHAFixture(t, cfg)
				if err := fixture.runner.RunHA(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
				summary := readSummary(t, cfg.EvidenceDir)
				scenario := onlyScenario(t, summary)
				if !summary.Passed || summary.Lane != plainHALane || summary.SchemaVersion != "2" ||
					summary.Environment.Level != level || scenario.ID != plainHALegacyID || scenario.Variant != "" ||
					scenario.Result != evidence.Pass {
					t.Fatalf("plain HA output identity/level/verdict changed: %+v", summary)
				}
				requireHARecordAndDataCheck(t, cfg, scenario)
				requireHAObjects(t, fixture, cfg)
				requireNamespaceGone(t, fixture)
				requireNoSecretInEvidence(t, cfg.EvidenceDir)
				if strings.Join(fixture.writes, ",") != "namespace,secret,cluster,client" {
					t.Fatalf("plain HA ownership writes changed: %q", fixture.writes)
				}
				if !slices.Equal(scenario.Evidence, []string{"LAB01/record.json", "LAB01/history.jsonl", "LAB01/data-check.json"}) {
					t.Fatalf("plain HA legacy evidence order changed: %q", scenario.Evidence)
				}
				data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, plainHAJUnitFile))
				if err != nil {
					t.Fatal(err)
				}
				// Use the actual XML attribute names, retaining the historical case identity.
				var wire struct {
					Cases []struct {
						Name  string `xml:"name,attr"`
						Class string `xml:"classname,attr"`
					} `xml:"testcase"`
				}
				if err := xml.Unmarshal(data, &wire); err != nil {
					t.Fatal(err)
				}
				if len(wire.Cases) != 1 || wire.Cases[0].Name != plainHALegacyID || wire.Cases[0].Class != "scenario" {
					t.Fatalf("JUnit identity changed: %s", data)
				}
				run, err := evidence.ReadRun(cfg.EvidenceDir)
				if err != nil {
					t.Fatal(err)
				}
				if run.Validated() != (level == evidence.LevelVMLab) {
					t.Fatalf("reporting level %s changed qualification: %q", level, run.Problems)
				}
				checkPlainHAOutputCompatibility(t, cfg, summary, scenario)
			})
		}
	}
}

func checkPlainHAOutputCompatibility(t *testing.T, cfg HAConfig, recorded evidence.Summary,
	scenario evidence.Scenario) {
	t.Helper()
	planned := evidence.Summary{Notes: []string{""}}
	lane, required, err := planHARun(cfg, &planned)
	if err != nil {
		t.Fatal(err)
	}
	recorded.RunID = "plain-ha-declaration-compatibility"
	recorded.StartedAt = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	recorded.FinishedAt = recorded.StartedAt.Add(time.Minute)
	recorded.Scenarios = nil
	current, legacy := recorded, recorded
	const notStarted = "not started"
	current.Plan(required, notStarted)
	legacy.Plan([]evidence.Requirement{{ID: plainHALegacyID}}, notStarted)
	current.Record(scenario)
	control := scenario
	control.ID, control.Variant = plainHALegacyID, ""
	legacy.Record(control)
	dirs := []string{t.TempDir(), t.TempDir()}
	for i, summary := range []evidence.Summary{current, legacy} {
		for _, file := range scenario.Evidence {
			data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, file))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dirs[i], file)
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := summary.Write(dirs[i], evidence.NewRedactor()); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{plainHASummaryFile, plainHAJUnitFile} {
		actual, err := os.ReadFile(filepath.Join(dirs[0], file))
		if err != nil {
			t.Fatal(err)
		}
		control, err := os.ReadFile(filepath.Join(dirs[1], file))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, control) {
			t.Fatalf("%s changed from the independent legacy requirement control", file)
		}
	}
	actual, err := evidence.ReadRun(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	controlRun, err := evidence.ReadRun(dirs[1])
	if err != nil {
		t.Fatal(err)
	}
	qualified := cfg.Level == evidence.LevelVMLab
	if actual.Validated() != qualified || controlRun.Validated() != qualified ||
		evidence.CompareRuns(controlRun, actual).Matches() != qualified {
		t.Fatalf("reporting-level qualification changed: actual=%q legacy=%q", actual.Problems, controlRun.Problems)
	}
	if err := current.Checkpoint(dirs[0], evidence.NewRedactor(), lane, current.FinishedAt); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := evidence.ReadRun(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !checkpoint.Provisional || checkpoint.Validated() || checkpoint.Summary.Passed {
		t.Fatal("plain HA checkpoint passed")
	}
}
