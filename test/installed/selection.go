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
	"fmt"
	"strings"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// haChecks are the options that select each VM-lab check of the installed HA
// command, by the legacy name the registry gives the check. -check <key>
// resolves the key through the registry and sets these options, so that a
// run selected by its key is the run its legacy options select: the same
// lane, requirements, evidence paths and summary. A check of S11 or S12 also
// needs -backup-bucket, which names where the backup goes.
var haChecks = map[string]func(*HAConfig){
	haScenarioID:                               func(*HAConfig) {},
	"S04/power-off":                            func(c *HAConfig) { c.Fault = FaultVMPowerOff },
	"S06/restart":                              func(c *HAConfig) { c.Fault = FaultOperatorRestart },
	"S11/complete":                             func(*HAConfig) {},
	"S11/missing-destination":                  func(c *HAConfig) { c.BackupMissingDestination = true },
	"S11/denied-credentials":                   func(c *HAConfig) { c.BackupDeniedCredentials = true },
	"S11/signature-mismatch":                   func(c *HAConfig) { c.BackupSignatureMismatch = true },
	"S11/interrupted-upload":                   func(c *HAConfig) { c.BackupInterruptedUpload = true },
	"S11/operator-restart":                     func(c *HAConfig) { c.BackupOperatorRestart = true },
	"S11/instance-manager-restart":             func(c *HAConfig) { c.BackupInstanceManagerRestart = true },
	"S11/overlapping":                          func(c *HAConfig) { c.BackupOverlapping = true },
	"S11/operation-store-loss":                 func(c *HAConfig) { c.BackupOperationStoreLoss = true },
	"S11/graceful-stop":                        func(c *HAConfig) { c.BackupGracefulStop = true },
	"S12/exact":                                func(c *HAConfig) { c.Restore = true },
	"S12/tampered":                             func(c *HAConfig) { c.Restore, c.RestoreTampered = true, true },
	"S12/wrong-target":                         func(c *HAConfig) { c.Restore, c.RestoreWrongTarget = true, true },
	"S12/operator-restart":                     func(c *HAConfig) { c.Restore, c.RestoreOperatorRestart = true, true },
	"S12/instance-manager-restart":             imRestartAtCheck(IMRestartAtRestoring),
	"S12/instance-manager-restart-preparing":   imRestartAtCheck(IMRestartAtPreparing),
	"S12/instance-manager-restart-seeding":     imRestartAtCheck(IMRestartAtSeeding),
	"S12/instance-manager-restart-starting":    imRestartAtCheck(IMRestartAtStarting),
	"S12/instance-manager-restart-seed-backup": imRestartAtCheck(IMRestartAtSeedBackup),
	"S12/graceful-stop":                        gracefulStopAtCheck(GracefulStopAtRestoring),
	"S12/graceful-stop-seeding":                gracefulStopAtCheck(GracefulStopAtSeeding),
}

// smokeChecks are the namespace modes that select each check of the installed
// smoke command, by the legacy name the registry gives the check.
var smokeChecks = map[string]NamespaceMode{
	"LAB00":              FreshNamespace,
	"LAB00/preallocated": PreallocatedNamespace,
}

// imRestartAtCheck selects S12 instance-manager-restart at a placement. The
// placement restoring is the option's default and is left as it is, empty or
// written out, as the legacy options leave it.
func imRestartAtCheck(at string) func(*HAConfig) {
	return func(c *HAConfig) {
		c.Restore, c.RestoreInstanceManagerRestart = true, true
		if at != IMRestartAtRestoring {
			c.RestoreInstanceManagerRestartAt = at
		}
	}
}

// gracefulStopAtCheck selects S12 graceful-stop at a placement, leaving the
// default placement restoring as it is.
func gracefulStopAtCheck(at string) func(*HAConfig) {
	return func(c *HAConfig) {
		c.Restore, c.RestoreGracefulStop = true, true
		if at != GracefulStopAtRestoring {
			c.RestoreGracefulStopAt = at
		}
	}
}

// SelectCheck selects the VM-lab check with the registry key, for example
// installed-backup-denied-credentials, by setting the options that select it
// (-check). It refuses a name that is not the key of a VM-lab check this
// command runs, never looking at the Kind level, and a configuration where a
// legacy option already selects a check, since the two could disagree. The
// configuration is left as it was when it refuses.
func (c *HAConfig) SelectCheck(key string) error {
	alias, err := resolveCheck(key, haCommand)
	if err != nil {
		return err
	}
	if options := c.selectedOptions(); len(options) > 0 {
		return fmt.Errorf("-check %s selects the check by itself; remove %s", key, strings.Join(options, ", "))
	}
	backup := strings.HasPrefix(alias, s11ID+"/") || strings.HasPrefix(alias, s12ID+"/")
	switch {
	case backup && c.BackupBucket == "":
		return fmt.Errorf("-check %s backs up the database and needs -backup-bucket", key)
	case !backup && c.BackupBucket != "":
		return fmt.Errorf("-check %s takes no backup; remove -backup-bucket, which would add the backup check", key)
	}
	haChecks[alias](c)
	return nil
}

// SmokeCheck returns the namespace mode that selects the check of the
// installed smoke command with the registry key: installed-standalone-sql-smoke
// or installed-preallocated-standalone-smoke. Any other name is refused.
func SmokeCheck(key string) (NamespaceMode, error) {
	alias, err := resolveCheck(key, smokeCommand)
	if err != nil {
		return "", err
	}
	return smokeChecks[alias], nil
}

// selectedOptions are the options of the configuration that select a check
// after LAB01, by their flag names. A placement's default is not a selection.
func (c HAConfig) selectedOptions() []string {
	var set []string
	for _, option := range []struct {
		flag string
		on   bool
	}{
		{"-fault", c.Fault != ""},
		{"-backup-missing-destination", c.BackupMissingDestination},
		{"-backup-denied-credentials", c.BackupDeniedCredentials},
		{"-backup-signature-mismatch", c.BackupSignatureMismatch},
		{"-backup-interrupted-upload", c.BackupInterruptedUpload},
		{"-backup-operator-restart", c.BackupOperatorRestart},
		{"-backup-instance-manager-restart", c.BackupInstanceManagerRestart},
		{"-backup-overlapping", c.BackupOverlapping},
		{"-backup-operation-store-loss", c.BackupOperationStoreLoss},
		{"-backup-graceful-stop", c.BackupGracefulStop},
		{"-restore", c.Restore},
		{"-restore-tampered", c.RestoreTampered},
		{"-restore-wrong-target", c.RestoreWrongTarget},
		{"-restore-operator-restart", c.RestoreOperatorRestart},
		{"-restore-instance-manager-restart", c.RestoreInstanceManagerRestart},
		{"-restore-instance-manager-restart-at", c.imRestartAt() != IMRestartAtRestoring},
		{"-restore-graceful-stop", c.RestoreGracefulStop},
		{"-restore-graceful-stop-at", c.gracefulStopAt() != GracefulStopAtRestoring},
	} {
		if option.on {
			set = append(set, option.flag)
		}
	}
	return set
}

// The installed commands, as the refusals name them.
const (
	haCommand    = "installed HA command"
	smokeCommand = "installed smoke command"
)

// resolveCheck returns the legacy name of the VM-lab check with the key, when
// the command runs it. The key is resolved through the registry, at the VM-lab
// level only: a Kind key, a family key, a legacy name or a misspelled key is
// refused, with the reason and the keys the command takes.
func resolveCheck(key, command string) (string, error) {
	runs := func(c string, entry evidence.ResolvedScenario) (string, bool) {
		for _, alias := range entry.Aliases {
			_, ha := haChecks[alias]
			_, smoke := smokeChecks[alias]
			if (c == haCommand && ha) || (c == smokeCommand && smoke) {
				return alias, true
			}
		}
		return "", false
	}
	var choices []string
	for _, choice := range evidence.RegisteredKeys(evidence.LevelVMLab) {
		if entry, err := evidence.ResolveKey(choice); err == nil {
			if _, ok := runs(command, entry); ok {
				choices = append(choices, choice)
			}
		}
	}
	refuse := func(reason string) error {
		return fmt.Errorf("-check %q: %s; the %s takes one of: %s", key, reason, command, strings.Join(choices, ", "))
	}
	entry, err := evidence.ResolveKey(key)
	switch {
	case err != nil:
		return "", refuse(err.Error())
	case entry.Level != evidence.LevelVMLab:
		return "", refuse(key + " is a Kind check, which the installed commands do not run")
	}
	if alias, ok := runs(command, entry); ok {
		return alias, nil
	}
	if _, ok := runs(haCommand, entry); ok {
		return "", refuse(key + " is a check of the " + haCommand)
	}
	return "", refuse(key + " is a check of the " + smokeCommand)
}
