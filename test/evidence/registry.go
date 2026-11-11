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

package evidence

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The evidence levels of a registered result. A level is what a result can
// prove (docs/testing/scenario-contract.md, "Test levels"), so one key never
// stands for results of two levels.
const (
	LevelKind  = "kind"
	LevelVMLab = "vm-lab"
)

const (
	// kindLaneName is the lane of the whole Kind suite. filteredKindLane is
	// what a run selected with a filter is judged as: its requirements
	// depend on the filter, so it is not registered, and its results are
	// read as the Kind suite's.
	kindLaneName     = "kind"
	filteredKindLane = "kind-filtered"
	// installedPrefix starts every key at the VM-lab level and no other.
	installedPrefix = "installed-"
	labLanePrefix   = "lab-"
	// contractDoc is where the anchor of a ScenarioFamily is.
	contractDoc = "docs/testing/scenario-contract.md"
)

var (
	scenarioKey   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	familyID      = regexp.MustCompile(`^(S[0-9]{2}|LAB[0-9]{2})$`)
	scenarioAlias = regexp.MustCompile(`^(S[0-9]{2}|LAB[0-9]{2})(/[a-z0-9]+(-[a-z0-9]+)*)?$`)
)

// ScenarioFamily is one scenario of the contract, with or without results
// that run today. Its key is not a result: a family with variants is several
// tests.
type ScenarioFamily struct {
	// ID is the ordinal code of the contract: "S03", "LAB01".
	ID string
	// Key is the descriptive name of the family.
	Key string
	// Description says what the family checks, for people reading a result.
	Description string
	// Contract is the anchor of its section in the contract.
	Contract string
}

// ScenarioEntry is one test that records one result. Key is its canonical
// identity. A Kind and a VM-lab test of one contract variant are two entries:
// they prove different things.
type ScenarioEntry struct {
	Key string
	// Name is the result's name for people. It is unique, unlike the text of
	// schema version 1, where S06/restart reads the same at both levels.
	Name   string
	Family string
	Level  string
	// Aliases are the legacy names of the result, "S03/abrupt" or "S00".
	// Earlier results and the gates of today use them. An alias is unique
	// within its level; S06/restart has one entry at each.
	Aliases []string
}

// RequiredLane is a lane with the results a run of it must pass.
type RequiredLane struct {
	Name  string
	Level string
	Keys  []string
}

// ResolvedScenario is a registered result with what the registry says of it.
type ResolvedScenario struct {
	ScenarioEntry
	// Contract is the path and anchor of its section in the contract.
	Contract string
	// Lanes are the lanes that require it.
	Lanes []string
}

type scenarioRegistry struct {
	families []ScenarioFamily
	entries  []ScenarioEntry
	lanes    []RequiredLane
}

// The descriptions of the families that also start the names of their results.
const (
	podRecoveryText = "Primary database Pod termination and recovery"
	brokerText      = "Broker process/Pod failure recovery"
	operatorText    = "Operator interruption and HA continuity"
	backupText      = "Backup completion, faults and recovery"
	restoreText     = "Restore into a new cluster and fault handling"
	vmFailureText   = "Primary database VM failure"
	standaloneText  = "Standalone database installation and real SQL"
	brokerHAText    = "Three-member HA and read-write/read-only Broker access"
	replicationText = "Normal HA replication"
	accessText      = "Instance Manager network isolation, token-checked requests, service-account permissions " +
		"and a search for leaked credentials"
	smokeText   = "Installed Operator standalone connection and SQL smoke"
	haSmokeText = "Installed three-member HA and SQL smoke"
)

// The keys the lanes share.
const (
	haSmokeKey        = "installed-ha-sql-smoke"
	backupCompleteKey = "installed-backup-complete"
	restoreExactKey   = "installed-restore-new-cluster-exact"
	restoreIMKey      = "installed-restore-new-cluster-instance-manager-restart-"
	restoreStopKey    = "installed-restore-new-cluster-graceful-stop-"
	requiredAnchor    = "required-scenarios"
)

// kindResult and labResult are the entry of a result with one legacy name.
func kindResult(key, alias, name string) ScenarioEntry { return result(LevelKind, key, alias, name) }

func labResult(key, alias, name string) ScenarioEntry { return result(LevelVMLab, key, alias, name) }

func result(level, key, alias, name string) ScenarioEntry {
	family, _, _ := strings.Cut(alias, "/")
	return ScenarioEntry{Key: key, Name: name, Family: family, Level: level, Aliases: []string{alias}}
}

// labLane requires LAB01, the base of the lab scenarios, and the given keys;
// backupLane S11 complete as well, and restoreLane S12 exact as well.
func labLane(name string, keys ...string) RequiredLane {
	return RequiredLane{Name: name, Level: LevelVMLab, Keys: append([]string{haSmokeKey}, keys...)}
}

func backupLane(name string, keys ...string) RequiredLane {
	return labLane(name, append([]string{backupCompleteKey}, keys...)...)
}

func restoreLane(name string, keys ...string) RequiredLane {
	return backupLane(name, append([]string{restoreExactKey}, keys...)...)
}

// registry is the one source of scenario identity: the descriptive keys, what
// people read, the contract sections, the legacy names and the lanes.
// docs/testing/scenario-name-migration.md explains how it was derived. Gate
// matches requirements and results by the key their legacy names resolve to.
// The requirements, the results, the labels and the evidence paths still name
// the legacy names, and the lists of requirements stay explicit where the runs
// assemble them; tests compare those lists, and the lane table of the
// contract, with this one.
var registry = scenarioRegistry{
	families: []ScenarioFamily{
		{"S00", "standalone-database-sql", standaloneText, "s00-install-and-single-database-smoke-test"},
		{"S01", "ha-broker-access", brokerHAText, "s01-three-member-ha-with-read-write-and-read-only-access"},
		{"S02", "ha-replication", replicationText, "s02-normal-replication"},
		{"S03", "ha-primary-pod-recovery", podRecoveryText, "s03-primary-pod-termination"},
		{"S04", "installed-ha-primary-vm-failure", vmFailureText, "s04-primary-vm-failure"},
		{"S05", "broker-failure-recovery", brokerText, "s05-broker-failure"},
		{"S06", "operator-failure-recovery", operatorText, "s06-operator-failure"},
		{"S07", "ha-network-partition", "HA network partition", "s07-network-partition"},
		{"S08", "ha-replication-lag-repeated-faults", "Replication lag under repeated faults",
			"s08-replication-lag-followed-by-repeated-failures"},
		{"S09", "ha-former-primary-rejoin", "Rejoin of a former primary member", "s09-former-primary-rejoins"},
		{"S10", "ha-pvc-loss-rebuild", "Rebuild after persistent-volume data loss", "s10-rebuild-after-pvc-loss"},
		{"S11", "installed-backup", backupText, "s11-backup-failure-and-restart"},
		{"S12", "installed-restore-new-cluster", restoreText, "s12-restore-into-a-new-cluster"},
		{"S13", "cluster-deletion-retention", "Cluster deletion and data-retention policy",
			"s13-deletion-and-retention"},
		{"S14", "instance-manager-access-and-secret-protection", accessText, "s14-authentication-and-secrets"},
		{"S15", "node-drain-disruption-policy", "Node drain and PodDisruptionBudget behavior",
			"s15-node-drain-and-poddisruptionbudget"},
		{"LAB00", "installed-standalone-sql-smoke", smokeText, requiredAnchor},
		{"LAB01", haSmokeKey, haSmokeText, requiredAnchor},
	},
	entries: []ScenarioEntry{
		kindResult("standalone-database-sql", "S00", standaloneText),
		kindResult("ha-broker-access", "S01", brokerHAText),
		kindResult("ha-replication", "S02", replicationText),
		kindResult("ha-primary-pod-recovery-abrupt-first-member", "S03/abrupt-first-member",
			podRecoveryText+" - abrupt first member"),
		kindResult("ha-primary-pod-recovery-abrupt-later-master", "S03/abrupt",
			podRecoveryText+" - abrupt, later master"),
		kindResult("ha-primary-pod-recovery-graceful", "S03/graceful", podRecoveryText+" - graceful"),
		kindResult("broker-failure-recovery-process", "S05/broker-process", brokerText+" - broker process"),
		kindResult("broker-failure-recovery-one-pod", "S05/one-broker-pod", brokerText+" - one broker pod"),
		kindResult("broker-failure-recovery-all-rw-pods", "S05/all-rw-brokers", brokerText+" - all rw brokers"),
		kindResult("operator-failure-recovery-restart", "S06/restart", operatorText+" - restart"),
		kindResult("operator-failure-recovery-absent-during-failover", "S06/absent-during-failover",
			operatorText+" - absent during failover"),
		kindResult("instance-manager-access-and-secret-protection", "S14", accessText),
		kindResult("ha-token-rotation", "S14/token-rotation", "HA Instance Manager token rotation"),
		kindResult("ha-legacy-token-migration", "S14/token-migration", "Migration from legacy Instance Manager token"),

		labResult("installed-standalone-sql-smoke", "LAB00", smokeText),
		labResult("installed-preallocated-standalone-smoke", "LAB00/preallocated",
			"Installed standalone preallocated ownership smoke"),
		labResult(haSmokeKey, "LAB01", haSmokeText),
		labResult("installed-ha-primary-vm-failure-power-off", "S04/power-off", vmFailureText+" - power off"),
		labResult("installed-operator-failure-recovery-restart", "S06/restart",
			operatorText+" - restart of the installed manager"),

		labResult(backupCompleteKey, "S11/complete", backupText+" - complete"),
		labResult("installed-backup-missing-destination", "S11/missing-destination",
			backupText+" - missing destination"),
		labResult("installed-backup-denied-credentials", "S11/denied-credentials",
			backupText+" - denied credentials"),
		labResult("installed-backup-signature-mismatch", "S11/signature-mismatch",
			backupText+" - signature mismatch"),
		labResult("installed-backup-interrupted-upload", "S11/interrupted-upload",
			backupText+" - interrupted upload"),
		labResult("installed-backup-operator-restart", "S11/operator-restart", backupText+" - operator restart"),
		labResult("installed-backup-instance-manager-restart", "S11/instance-manager-restart",
			backupText+" - instance manager restart"),
		labResult("installed-backup-overlapping", "S11/overlapping", backupText+" - overlapping"),
		labResult("installed-backup-operation-store-loss", "S11/operation-store-loss",
			backupText+" - operation store loss"),
		labResult("installed-backup-graceful-stop", "S11/graceful-stop", backupText+" - graceful stop"),

		labResult(restoreExactKey, "S12/exact", restoreText+" - exact"),
		labResult("installed-restore-new-cluster-tampered", "S12/tampered", restoreText+" - tampered"),
		labResult("installed-restore-new-cluster-wrong-target", "S12/wrong-target", restoreText+" - wrong target"),
		labResult("installed-restore-new-cluster-operator-restart", "S12/operator-restart",
			restoreText+" - operator restart"),
		labResult(restoreIMKey+"restoring", "S12/instance-manager-restart",
			restoreText+" - instance manager restart, restoring placement"),
		labResult(restoreIMKey+"preparing", "S12/instance-manager-restart-preparing",
			restoreText+" - instance manager restart preparing"),
		labResult(restoreIMKey+"seeding", "S12/instance-manager-restart-seeding",
			restoreText+" - instance manager restart seeding"),
		labResult(restoreIMKey+"starting", "S12/instance-manager-restart-starting",
			restoreText+" - instance manager restart starting"),
		labResult(restoreIMKey+"seed-backup", "S12/instance-manager-restart-seed-backup",
			restoreText+" - instance manager restart seed backup"),
		labResult(restoreStopKey+"restoring", "S12/graceful-stop", restoreText+" - graceful stop"),
		labResult(restoreStopKey+"seeding", "S12/graceful-stop-seeding", restoreText+" - graceful stop seeding"),
	},
	lanes: []RequiredLane{
		{Name: kindLaneName, Level: LevelKind, Keys: []string{
			"standalone-database-sql", "ha-broker-access", "ha-replication",
			"instance-manager-access-and-secret-protection",
			"ha-primary-pod-recovery-abrupt-first-member", "ha-primary-pod-recovery-abrupt-later-master",
			"ha-primary-pod-recovery-graceful",
			"broker-failure-recovery-process", "broker-failure-recovery-one-pod", "broker-failure-recovery-all-rw-pods",
			"operator-failure-recovery-restart", "operator-failure-recovery-absent-during-failover",
			"ha-token-rotation", "ha-legacy-token-migration"}},
		{Name: "lab-installed-smoke", Level: LevelVMLab, Keys: []string{"installed-standalone-sql-smoke"}},
		{Name: "lab-preallocated-component-smoke", Level: LevelVMLab,
			Keys: []string{"installed-preallocated-standalone-smoke"}},
		labLane("lab-installed-ha"),
		labLane("lab-installed-ha-s04", "installed-ha-primary-vm-failure-power-off"),
		labLane("lab-installed-ha-s06", "installed-operator-failure-recovery-restart"),
		backupLane("lab-installed-ha-s11"),
		backupLane("lab-installed-ha-s11-missing-destination", "installed-backup-missing-destination"),
		backupLane("lab-installed-ha-s11-denied-credentials", "installed-backup-denied-credentials"),
		backupLane("lab-installed-ha-s11-signature-mismatch", "installed-backup-signature-mismatch"),
		backupLane("lab-installed-ha-s11-interrupted-upload", "installed-backup-interrupted-upload"),
		backupLane("lab-installed-ha-s11-operator-restart", "installed-backup-operator-restart"),
		backupLane("lab-installed-ha-s11-instance-manager-restart", "installed-backup-instance-manager-restart"),
		backupLane("lab-installed-ha-s11-overlapping", "installed-backup-overlapping"),
		backupLane("lab-installed-ha-s11-operation-store-loss", "installed-backup-operation-store-loss"),
		backupLane("lab-installed-ha-s11-graceful-stop", "installed-backup-graceful-stop"),
		restoreLane("lab-installed-ha-s12"),
		restoreLane("lab-installed-ha-s12-tampered", "installed-restore-new-cluster-tampered"),
		restoreLane("lab-installed-ha-s12-wrong-target", "installed-restore-new-cluster-wrong-target"),
		restoreLane("lab-installed-ha-s12-operator-restart", "installed-restore-new-cluster-operator-restart"),
		restoreLane("lab-installed-ha-s12-instance-manager-restart", restoreIMKey+"restoring"),
		restoreLane("lab-installed-ha-s12-instance-manager-restart-preparing", restoreIMKey+"preparing"),
		restoreLane("lab-installed-ha-s12-instance-manager-restart-seeding", restoreIMKey+"seeding"),
		restoreLane("lab-installed-ha-s12-instance-manager-restart-starting", restoreIMKey+"starting"),
		restoreLane("lab-installed-ha-s12-instance-manager-restart-seed-backup", restoreIMKey+"seed-backup"),
		restoreLane("lab-installed-ha-s12-graceful-stop", restoreStopKey+"restoring"),
		restoreLane("lab-installed-ha-s12-graceful-stop-seeding", restoreStopKey+"seeding"),
	},
}

// ResolveKey returns the result of a canonical key. A key the registry does
// not hold is an error, and so is a family's key: a family is not one test.
func ResolveKey(key string) (ResolvedScenario, error) { return registry.resolveKey(key) }

// ResolveAlias returns the result a legacy name stood for in a run of the
// given lane at the given environment level, both as summary.json records
// them. The legacy name alone is not enough: S06/restart is a Kind test and a
// lab test. It is an error when the name, the lane or the combination is
// unknown, and it never falls back to another level. An installed lane run
// with -level kind has no key: its result is neither the Kind suite's nor a
// VM-lab result.
func ResolveAlias(lane, level, alias string) (ResolvedScenario, error) {
	return registry.resolveAlias(lane, level, alias)
}

// RegistryProblems compares the lane as a run assembled it with the registry,
// one line per difference: a requirement that maps to no result of the lane,
// a registered result with no requirement, or two requirements of one result.
// An empty answer means that the two agree exactly. Tests use it; a lane's
// verdict comes from Gate alone. Gate matches by the key a legacy name
// resolves to, and a result that fails to map satisfies only a requirement of
// its exact legacy name, so it stays missing for a registered requirement.
func (l Lane) RegistryProblems(level string) []string { return registry.laneProblems(l, level) }

// RegisteredLanes returns the names of the lanes of a level.
func RegisteredLanes(level string) []string {
	var names []string
	for _, lane := range registry.lanes {
		if lane.Level == level {
			names = append(names, lane.Name)
		}
	}
	return names
}

// RegisteredKeys returns the keys of the results of a level, in the
// registry's order.
func RegisteredKeys(level string) []string {
	var keys []string
	for _, entry := range registry.entries {
		if entry.Level == level {
			keys = append(keys, entry.Key)
		}
	}
	return keys
}

func (r scenarioRegistry) family(id string) (ScenarioFamily, bool) {
	i := slices.IndexFunc(r.families, func(f ScenarioFamily) bool { return f.ID == id })
	if i < 0 {
		return ScenarioFamily{}, false
	}
	return r.families[i], true
}

func (r scenarioRegistry) lane(name string) (RequiredLane, bool) {
	i := slices.IndexFunc(r.lanes, func(l RequiredLane) bool { return l.Name == name })
	if i < 0 {
		return RequiredLane{}, false
	}
	return r.lanes[i], true
}

func (r scenarioRegistry) resolved(entry ScenarioEntry) ResolvedScenario {
	found := ResolvedScenario{ScenarioEntry: entry}
	if family, ok := r.family(entry.Family); ok {
		found.Contract = contractDoc + "#" + family.Contract
	}
	for _, lane := range r.lanes {
		if slices.Contains(lane.Keys, entry.Key) {
			found.Lanes = append(found.Lanes, lane.Name)
		}
	}
	return found
}

func (r scenarioRegistry) resolveKey(key string) (ResolvedScenario, error) {
	var matches []ScenarioEntry
	for _, entry := range r.entries {
		if entry.Key == key {
			matches = append(matches, entry)
		}
	}
	switch len(matches) {
	case 1:
		return r.resolved(matches[0]), nil
	case 0:
		for _, family := range r.families {
			if family.Key == key {
				return ResolvedScenario{}, fmt.Errorf("%s is the family of %s, not one result", key, family.ID)
			}
		}
		return ResolvedScenario{}, fmt.Errorf("unknown scenario key %s", key)
	}
	return ResolvedScenario{}, fmt.Errorf("scenario key %s names %d results", key, len(matches))
}

// checkLaneLevel returns an error unless the lane is judged at the level.
func (r scenarioRegistry) checkLaneLevel(name, level string) error {
	want := LevelKind
	if name != filteredKindLane {
		lane, ok := r.lane(name)
		if !ok {
			return fmt.Errorf("lane %s is not registered", name)
		}
		want = lane.Level
	}
	if want != level {
		return fmt.Errorf("lane %s is judged at level %s, not %s", name, want, level)
	}
	return nil
}

func (r scenarioRegistry) resolveAlias(lane, level, alias string) (ResolvedScenario, error) {
	if err := r.checkLaneLevel(lane, level); err != nil {
		return ResolvedScenario{}, err
	}
	var matches []ScenarioEntry
	for _, entry := range r.entries {
		if entry.Level == level && slices.Contains(entry.Aliases, alias) {
			matches = append(matches, entry)
		}
	}
	switch len(matches) {
	case 1:
		return r.resolved(matches[0]), nil
	case 0:
		return ResolvedScenario{}, fmt.Errorf("%s is not a legacy name of a %s result", alias, level)
	}
	return ResolvedScenario{}, fmt.Errorf("%s names %d %s results", alias, len(matches), level)
}

// resolveResult returns the registered result a scenario's record stands for
// in a run of the lane and level. The legacy name must also be recorded in its
// fields: S03 with no variant is not S03/abrupt written into the ID.
func (r scenarioRegistry) resolveResult(lane, level string, sc Scenario) (ResolvedScenario, error) {
	entry, err := r.resolveAlias(lane, level, sc.Name())
	if err == nil && entry.Family != sc.ID {
		return ResolvedScenario{}, fmt.Errorf("%s is recorded with the ID %s, not %s", sc.Name(), sc.ID, entry.Family)
	}
	return entry, err
}

func (r scenarioRegistry) laneProblems(l Lane, level string) []string {
	if err := r.checkLaneLevel(l.Name, level); err != nil {
		return []string{err.Error()}
	}
	lane, ok := r.lane(l.Name)
	if !ok {
		return []string{fmt.Sprintf("lane %s is not registered", l.Name)}
	}
	var problems []string
	if len(l.Required) == 0 {
		problems = append(problems, fmt.Sprintf("lane %s: requires nothing", l.Name))
	}
	covered := map[string]bool{}
	for _, req := range l.Required {
		name := Scenario{ID: req.ID, Variant: req.Variant}.Name()
		if req.NotApplicable {
			problems = append(problems,
				fmt.Sprintf("lane %s: %s may be not_applicable, which no registered lane allows", l.Name, name))
		}
		entry, err := r.resolveAlias(l.Name, level, name)
		switch {
		case err != nil:
			problems = append(problems, err.Error())
		case !slices.Contains(lane.Keys, entry.Key):
			problems = append(problems, fmt.Sprintf("lane %s: %s is not registered for it", l.Name, name))
		case covered[entry.Key]:
			problems = append(problems,
				fmt.Sprintf("lane %s: %s and an earlier requirement are both %s", l.Name, name, entry.Key))
		default:
			covered[entry.Key] = true
		}
	}
	for _, key := range lane.Keys {
		if !covered[key] {
			problems = append(problems, fmt.Sprintf("lane %s: registered key %s has no requirement", l.Name, key))
		}
	}
	return problems
}

// description is what people read for a legacy name, in schema version 1: the
// name of its one result, or the description of a family's ID. A name with a
// result at each level, S06/restart, has neither, so that a caller reads it as
// any other variant of its family and neither level's name is shown for the
// other's result.
func (r scenarioRegistry) description(name string) (string, bool) {
	var names []string
	for _, entry := range r.entries {
		if slices.Contains(entry.Aliases, name) {
			names = append(names, entry.Name)
		}
	}
	if len(names) == 1 {
		return names[0], true
	}
	family, ok := r.family(name)
	return family.Description, ok
}

// validate returns everything wrong with the registry, one line each.
func (r scenarioRegistry) validate() []string {
	return slices.Concat(r.validateFamilies(), r.validateEntries(), r.validateLanes())
}

func (r scenarioRegistry) validateFamilies() []string {
	var problems []string
	add := func(id, format string, args ...any) {
		problems = append(problems, "family "+id+": "+fmt.Sprintf(format, args...))
	}
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, family := range r.families {
		switch {
		case !familyID.MatchString(family.ID):
			add(family.ID, "the ID is not of the form S00 or LAB00")
		case ids[family.ID]:
			add(family.ID, "the ID is used twice")
		}
		switch {
		case !scenarioKey.MatchString(family.Key):
			add(family.ID, "the key %s is not a lower-case slug", family.Key)
		case keys[family.Key]:
			add(family.ID, "the key %s is used twice", family.Key)
		}
		ids[family.ID], keys[family.Key] = true, true
		if family.Description == "" {
			add(family.ID, "no description")
		}
		if family.Contract == "" {
			add(family.ID, "no contract pointer")
		}
	}
	return problems
}

func (r scenarioRegistry) validateEntries() []string {
	var problems []string
	add := func(key, format string, args ...any) {
		problems = append(problems, "key "+key+": "+fmt.Sprintf(format, args...))
	}
	keys, names := map[string]bool{}, map[string]bool{}
	for _, entry := range r.entries {
		switch {
		case !scenarioKey.MatchString(entry.Key):
			add(entry.Key, "not a lower-case slug")
		case keys[entry.Key]:
			add(entry.Key, "used by two results")
		}
		keys[entry.Key] = true
		switch {
		case entry.Name == "":
			add(entry.Key, "no readable name")
		case names[entry.Name]:
			add(entry.Key, "the name %q is used twice", entry.Name)
		}
		names[entry.Name] = true
		if _, ok := r.family(entry.Family); !ok {
			add(entry.Key, "unknown family %s", entry.Family)
		}
		for _, family := range r.families {
			if family.Key == entry.Key && !slices.Equal(entry.Aliases, []string{family.ID}) {
				add(entry.Key, "is the key of family %s, but the result is not that family's own", family.ID)
			}
		}
		installed := strings.HasPrefix(entry.Key, installedPrefix)
		switch {
		case entry.Level != LevelKind && entry.Level != LevelVMLab:
			add(entry.Key, "unknown level %s", entry.Level)
		case entry.Level == LevelKind && installed:
			add(entry.Key, "a %s result must not start with %s", LevelKind, installedPrefix)
		case entry.Level == LevelVMLab && !installed:
			add(entry.Key, "a %s result must start with %s", LevelVMLab, installedPrefix)
		}
	}
	return append(problems, r.validateAliases()...)
}

func (r scenarioRegistry) validateAliases() []string {
	var problems []string
	add := func(alias, format string, args ...any) {
		problems = append(problems, "alias "+alias+": "+fmt.Sprintf(format, args...))
	}
	seen := map[string]string{}
	for _, entry := range r.entries {
		for i, alias := range entry.Aliases {
			id, _, _ := strings.Cut(alias, "/")
			switch {
			case !scenarioAlias.MatchString(alias):
				add(alias, "not of the form S00, S00/variant, LAB00 or LAB00/variant")
			case id != entry.Family:
				add(alias, "belongs to %s, not to the family %s of %s", id, entry.Family, entry.Key)
			}
			if slices.Contains(entry.Aliases[:i], alias) {
				add(alias, "listed twice for %s", entry.Key)
				continue
			}
			scope := entry.Level + " " + alias
			if earlier, ok := seen[scope]; ok {
				add(alias, "names two %s results, %s and %s", entry.Level, entry.Key, earlier)
			}
			seen[scope] = entry.Key
		}
	}
	return problems
}

func (r scenarioRegistry) validateLanes() []string {
	var problems []string
	names := map[string]bool{}
	for _, lane := range r.lanes {
		add := func(format string, args ...any) {
			problems = append(problems, "lane "+lane.Name+": "+fmt.Sprintf(format, args...))
		}
		if names[lane.Name] {
			add("registered twice")
		}
		names[lane.Name] = true
		switch {
		case lane.Name == filteredKindLane:
			add("a filtered run selects its own requirements and cannot be registered")
		case lane.Level == LevelKind && lane.Name != kindLaneName:
			add("a %s lane must be named %s", LevelKind, kindLaneName)
		case lane.Level == LevelVMLab && !strings.HasPrefix(lane.Name, labLanePrefix):
			add("a %s lane must start with %s", LevelVMLab, labLanePrefix)
		case lane.Level != LevelKind && lane.Level != LevelVMLab:
			add("unknown level %s", lane.Level)
		}
		if len(lane.Keys) == 0 {
			add("requires nothing")
		}
		for i, key := range lane.Keys {
			if slices.Contains(lane.Keys[:i], key) {
				add("key %s is listed twice", key)
			}
			j := slices.IndexFunc(r.entries, func(e ScenarioEntry) bool { return e.Key == key })
			if j < 0 {
				add("unknown key %s", key)
				continue
			}
			entry := r.entries[j]
			if entry.Level != lane.Level {
				add("key %s is a %s result in a %s lane", key, entry.Level, lane.Level)
			}
			if len(entry.Aliases) == 0 {
				add("key %s has no legacy alias, which the gates match on", key)
			}
		}
	}
	return problems
}
