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

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const unknownKey = "unknown scenario key"

// legacyHAOptions are the options of the installed HA command that selected
// each VM-lab check before a check could be named by its key, written out by
// hand so that the key's selection is compared with them and not with itself.
// backup marks the checks that also need -backup-bucket, which is data as well
// as a selection.
var legacyHAOptions = map[string]struct {
	backup bool
	set    func(*HAConfig)
}{
	"installed-ha-sql-smoke":                    {false, func(*HAConfig) {}},
	"installed-ha-primary-vm-failure-power-off": {false, func(c *HAConfig) { c.Fault = FaultVMPowerOff }},
	"installed-operator-failure-recovery-restart": {false, func(c *HAConfig) {
		c.Fault = FaultOperatorRestart
	}},
	"installed-backup-complete": {true, func(*HAConfig) {}},
	"installed-backup-missing-destination": {true, func(c *HAConfig) {
		c.BackupMissingDestination = true
	}},
	"installed-backup-denied-credentials": {true, func(c *HAConfig) { c.BackupDeniedCredentials = true }},
	"installed-backup-signature-mismatch": {true, func(c *HAConfig) { c.BackupSignatureMismatch = true }},
	"installed-backup-interrupted-upload": {true, func(c *HAConfig) { c.BackupInterruptedUpload = true }},
	"installed-backup-operator-restart":   {true, func(c *HAConfig) { c.BackupOperatorRestart = true }},
	"installed-backup-instance-manager-restart": {true, func(c *HAConfig) {
		c.BackupInstanceManagerRestart = true
	}},
	"installed-backup-overlapping": {true, func(c *HAConfig) { c.BackupOverlapping = true }},
	"installed-backup-operation-store-loss": {true, func(c *HAConfig) {
		c.BackupOperationStoreLoss = true
	}},
	"installed-backup-graceful-stop":      {true, func(c *HAConfig) { c.BackupGracefulStop = true }},
	"installed-restore-new-cluster-exact": {true, func(c *HAConfig) { c.Restore = true }},
	"installed-restore-new-cluster-tampered": {true, func(c *HAConfig) {
		c.Restore, c.RestoreTampered = true, true
	}},
	"installed-restore-new-cluster-wrong-target": {true, func(c *HAConfig) {
		c.Restore, c.RestoreWrongTarget = true, true
	}},
	"installed-restore-new-cluster-operator-restart": {true, func(c *HAConfig) {
		c.Restore, c.RestoreOperatorRestart = true, true
	}},
	"installed-restore-new-cluster-instance-manager-restart-restoring": {true, func(c *HAConfig) {
		c.Restore, c.RestoreInstanceManagerRestart = true, true
	}},
	"installed-restore-new-cluster-instance-manager-restart-preparing": {true, func(c *HAConfig) {
		c.Restore, c.RestoreInstanceManagerRestart, c.RestoreInstanceManagerRestartAt = true, true,
			IMRestartAtPreparing
	}},
	"installed-restore-new-cluster-instance-manager-restart-seeding": {true, func(c *HAConfig) {
		c.Restore, c.RestoreInstanceManagerRestart, c.RestoreInstanceManagerRestartAt = true, true,
			IMRestartAtSeeding
	}},
	"installed-restore-new-cluster-instance-manager-restart-starting": {true, func(c *HAConfig) {
		c.Restore, c.RestoreInstanceManagerRestart, c.RestoreInstanceManagerRestartAt = true, true,
			IMRestartAtStarting
	}},
	"installed-restore-new-cluster-instance-manager-restart-seed-backup": {true, func(c *HAConfig) {
		c.Restore, c.RestoreInstanceManagerRestart, c.RestoreInstanceManagerRestartAt = true, true,
			IMRestartAtSeedBackup
	}},
	"installed-restore-new-cluster-graceful-stop-restoring": {true, func(c *HAConfig) {
		c.Restore, c.RestoreGracefulStop = true, true
	}},
	"installed-restore-new-cluster-graceful-stop-seeding": {true, func(c *HAConfig) {
		c.Restore, c.RestoreGracefulStop, c.RestoreGracefulStopAt = true, true, GracefulStopAtSeeding
	}},
}

// legacySmokeModes are the namespace modes of the installed smoke command
// that selected each of its checks.
var legacySmokeModes = map[string]NamespaceMode{
	"installed-standalone-sql-smoke":          FreshNamespace,
	"installed-preallocated-standalone-smoke": PreallocatedNamespace,
}

// plannedHA is what the HA command assembles for a configuration: its lane,
// requirements and the notes of its summary.
type plannedHA struct {
	lane         string
	requirements []evidence.Requirement
	notes        []string
}

func planOf(cfg HAConfig) plannedHA {
	summary := evidence.Summary{Notes: []string{""}}
	lane, requirements := planHARun(cfg, &summary)
	return plannedHA{lane, requirements, summary.Notes}
}

// Every VM-lab key of the registry is selected by one of the installed
// commands, and naming it selects exactly the configuration, lane,
// requirements and notes its legacy options select. The flag default of the
// Instance Manager restart placement (restoring) and its empty value are both
// checked, since the command and the tests start from each.
func TestEveryInstalledKeySelectsWhatItsLegacyOptionsSelect(t *testing.T) {
	for _, key := range evidence.RegisteredKeys(evidence.LevelVMLab) {
		_, isHA := legacyHAOptions[key]
		_, isSmoke := legacySmokeModes[key]
		if isHA == isSmoke {
			t.Errorf("the VM-lab key %s must be selected by exactly one installed command", key)
		}
	}
	for key, legacy := range legacyHAOptions {
		for _, at := range []string{"", IMRestartAtRestoring} {
			base := testHAConfig(t)
			base.RestoreInstanceManagerRestartAt = at
			if legacy.backup {
				base.BackupBucket = testBackupBucket
			}
			want := base
			legacy.set(&want)
			got := base
			if err := got.SelectCheck(key); err != nil {
				t.Errorf("-check %s: %v", key, err)
				continue
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("-check %s selects a configuration other than its legacy options':\n got %+v\nwant %+v",
					key, got, want)
			}
			gotPlan, wantPlan := planOf(got), planOf(want)
			if !reflect.DeepEqual(gotPlan, wantPlan) {
				t.Errorf("-check %s plans %+v, its legacy options %+v", key, gotPlan, wantPlan)
			}
			resolved, err := evidence.ResolveKey(key)
			if err != nil || !slices.Contains(resolved.Lanes, gotPlan.lane) {
				t.Errorf("-check %s plans the lane %s, which the registry does not give it (%q, %v)",
					key, gotPlan.lane, resolved.Lanes, err)
			}
			problems := (evidence.Lane{Name: gotPlan.lane, Required: gotPlan.requirements}).
				RegistryProblems(evidence.LevelVMLab)
			for _, problem := range problems {
				t.Errorf("-check %s: %s", key, problem)
			}
		}
	}
	for key, want := range legacySmokeModes {
		got, err := SmokeCheck(key)
		if err != nil || got != want {
			t.Errorf("SmokeCheck(%s) = %q, %v, want %q", key, got, err, want)
			continue
		}
		gotLane, gotVariant := Config{NamespaceMode: got}.smokeLane()
		wantLane, wantVariant := Config{NamespaceMode: want}.smokeLane()
		if gotLane != wantLane || gotVariant != wantVariant {
			t.Errorf("-check %s plans %s %s, its namespace mode %s %s", key, gotLane, gotVariant, wantLane, wantVariant)
		}
	}
}

// A name that is not the key of a VM-lab check this command runs is refused,
// with the reason and the keys it does take: a misspelled key, a family key,
// which stands for several checks, a Kind key, a legacy name (S06/restart
// names a Kind and a VM-lab check), and the other command's key. Nothing of
// the configuration changes.
func TestAnUnknownOrAmbiguousCheckIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"installed-backup-complet", unknownKey},
		{"installed-backup", "family"},
		{"installed-restore-new-cluster", "family"},
		{"operator-failure-recovery-restart", "Kind check"},
		{"ha-replication", "Kind check"},
		{"S06/restart", unknownKey},
		{"S11/complete", unknownKey},
		{haScenarioID, unknownKey},
		{"installed-standalone-sql-smoke", "installed smoke command"},
		{" installed-ha-sql-smoke", unknownKey},
		{"INSTALLED-HA-SQL-SMOKE", unknownKey},
	} {
		cfg := testHAConfig(t)
		cfg.BackupBucket = testBackupBucket
		before := cfg
		err := cfg.SelectCheck(tc.name)
		switch {
		case err == nil:
			t.Errorf("-check %q was accepted", tc.name)
		case !strings.Contains(err.Error(), tc.reason) ||
			!strings.Contains(err.Error(), "installed-restore-new-cluster-graceful-stop-seeding"):
			t.Errorf("-check %q: %v; want the reason %q and the keys it takes", tc.name, err, tc.reason)
		}
		if !reflect.DeepEqual(cfg, before) {
			t.Errorf("-check %q changed the configuration", tc.name)
		}
	}
	for _, name := range []string{"installed-ha-sql-smoke", "installed-standalone", "standalone-database-sql",
		"LAB00", "LAB00/preallocated", ""} {
		_, err := SmokeCheck(name)
		if err == nil || !strings.Contains(err.Error(), "installed-preallocated-standalone-smoke") {
			t.Errorf("SmokeCheck(%q) = %v, want a refusal naming the keys it takes", name, err)
		}
	}
}

// A key is not combined with a legacy option that selects a check: the two
// could disagree, and the run would not be the check named. -backup-bucket is
// data as well, so it is required by the backup and restore checks and
// refused by the others.
func TestACheckIsNotCombinedWithALegacySelection(t *testing.T) {
	options := laneOptions(t)
	for _, option := range options {
		cfg := testHAConfig(t)
		cfg.BackupBucket = testBackupBucket
		reflect.ValueOf(&cfg).Elem().FieldByName(option).SetBool(true)
		if err := cfg.SelectCheck("installed-backup-complete"); err == nil {
			t.Errorf("-check was accepted with the option %s", option)
		}
	}
	for name, set := range map[string]func(*HAConfig){
		"-fault": func(c *HAConfig) { c.Fault = FaultOperatorRestart },
		"-restore-instance-manager-restart-at": func(c *HAConfig) {
			c.RestoreInstanceManagerRestartAt = IMRestartAtSeeding
		},
		"-restore-graceful-stop-at": func(c *HAConfig) { c.RestoreGracefulStopAt = GracefulStopAtSeeding },
	} {
		cfg := testHAConfig(t)
		set(&cfg)
		if err := cfg.SelectCheck("installed-ha-sql-smoke"); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("-check with %s = %v, want a refusal naming it", name, err)
		}
	}
	for key, legacy := range legacyHAOptions {
		cfg := testHAConfig(t)
		if !legacy.backup {
			cfg.BackupBucket = testBackupBucket
		}
		if err := cfg.SelectCheck(key); err == nil || !strings.Contains(err.Error(), "-backup-bucket") {
			t.Errorf("-check %s with backup bucket %q = %v, want a refusal naming -backup-bucket",
				key, cfg.BackupBucket, err)
		}
	}
}
