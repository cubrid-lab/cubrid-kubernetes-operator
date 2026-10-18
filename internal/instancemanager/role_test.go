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

package instancemanager

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// masterOut / slaveOut are verbatim `cubrid heartbeat status` output captured
// from the real 2-node HA POC (docs/poc/RESULTS.md).
const masterOut = ` HA-Node Info (current cub-0, state master)
   Node cub-1 (priority 2, state slave)
   Node cub-0 (priority 1, state master)
 HA-Process Info (master 91, state master)
   Copylogdb pocdb@cub-1:/home/cubrid/CUBRID/databases/pocdb_cub-1 (pid 526, state registered)
   Applylogdb pocdb@localhost:/home/cubrid/CUBRID/databases/pocdb_cub-1 (pid 528, state registered)
   Server pocdb (pid 272, state registered_and_active)`

const slaveOut = ` HA-Node Info (current cub-1, state slave)
   Node cub-1 (priority 2, state slave)
   Node cub-0 (priority 1, state master)
 HA-Process Info (master 135, state slave)
   Server pocdb (pid 144, state registered_and_standby)`

// transitionOut is the transient state observed during startup/failover.
const transitionOut = ` HA-Node Info (current cub-0, state to-be-master)
   Node cub-1 (priority 2, state unknown)
   Node cub-0 (priority 1, state slave)`

func TestParseHAStatus_Master(t *testing.T) {
	s := ParseHAStatus(masterOut)
	if s.Current != "cub-0" {
		t.Errorf("current = %q, want cub-0", s.Current)
	}
	if s.Role != RoleMaster {
		t.Errorf("role = %q, want master", s.Role)
	}
	if !s.ServerActive {
		t.Error("serverActive = false, want true (registered_and_active)")
	}
	if len(s.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(s.Nodes))
	}
}

func TestParseHAStatus_Slave(t *testing.T) {
	s := ParseHAStatus(slaveOut)
	if s.Current != "cub-1" {
		t.Errorf("current = %q, want cub-1", s.Current)
	}
	if s.Role != RoleSlave {
		t.Errorf("role = %q, want slave", s.Role)
	}
	if s.ServerActive {
		t.Error("serverActive = true, want false (registered_and_standby)")
	}
}

func TestParseHAStatus_TransitionIsUnknown(t *testing.T) {
	s := ParseHAStatus(transitionOut)
	if s.Role != RoleUnknown {
		t.Errorf("role = %q, want unknown for to-be-master transition", s.Role)
	}
	if s.Reason == "" {
		t.Error("reason should explain why the transition state is not authoritative")
	}
}

func TestParseHAStatus_EmptyIsUnknown(t *testing.T) {
	for _, out := range []string{"", "@ cubrid heartbeat status\n", "garbage"} {
		s := ParseHAStatus(out)
		if s.Role != RoleUnknown {
			t.Errorf("role = %q, want unknown for %q", s.Role, out)
		}
	}
}

func TestParseHAStatus_TopologyPriorities(t *testing.T) {
	s := ParseHAStatus(masterOut)
	byName := map[string]NodeState{}
	for _, n := range s.Nodes {
		byName[n.Name] = n
	}
	if byName["cub-0"].Priority != 1 || byName["cub-0"].State != "master" {
		t.Errorf("cub-0 = %+v, want priority 1 / master", byName["cub-0"])
	}
	if byName["cub-1"].Priority != 2 || byName["cub-1"].State != string(RoleSlave) {
		t.Errorf("cub-1 = %+v, want priority 2 / slave", byName["cub-1"])
	}
}

// The role is taken only from an answer that is consistent in itself: the
// local node appears exactly once in the node list, with the state the header
// gives (#199).
func TestParseHAStatus_NeedsExactlyOneMatchingLocalNode(t *testing.T) {
	tests := []struct {
		name string
		out  string
	}{
		{"the local node is missing from the list", ` HA-Node Info (current cub-0, state master)
   Node cub-1 (priority 2, state slave)
 HA-Process Info (master 91, state master)
   Server pocdb (pid 272, state registered_and_active)`},
		{"the local node is listed twice", ` HA-Node Info (current cub-0, state master)
   Node cub-0 (priority 2, state slave)
   Node cub-0 (priority 1, state master)
 HA-Process Info (master 91, state master)
   Server pocdb (pid 272, state registered_and_active)`},
		{"the list gives the local node another state", ` HA-Node Info (current cub-0, state master)
   Node cub-1 (priority 2, state master)
   Node cub-0 (priority 1, state slave)
 HA-Process Info (master 91, state master)
   Server pocdb (pid 272, state registered_and_active)`},
		{"there is no node list", ` HA-Node Info (current cub-1, state slave)
 HA-Process Info (master 135, state slave)
   Server pocdb (pid 144, state registered_and_standby)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := ParseHAStatus(tc.out)
			if s.Role != RoleUnknown {
				t.Errorf("role = %q, want unknown for an inconsistent answer", s.Role)
			}
			if s.Reason == "" {
				t.Error("an answer that is not accepted needs a reason")
			}
		})
	}

	// The recorded outputs stay authoritative.
	if s := ParseHAStatus(masterOut); s.Role != RoleMaster || s.Reason != "" {
		t.Errorf("recorded master output: role %q, reason %q", s.Role, s.Reason)
	}
	if s := ParseHAStatus(slaveOut); s.Role != RoleSlave || s.Reason != "" {
		t.Errorf("recorded slave output: role %q, reason %q", s.Role, s.Reason)
	}
}

// slaveWithAppliersOut is the status of a slave of a three-member group: it
// applies the log of each of the two other members. It is hand-written, not
// captured: POC-30 recorded the applier's process IDs but not the verbatim
// output. Its Applylogdb lines follow the verbatim line in masterOut (POC-3)
// and the layout of `cubrid heartbeat status` in the CUBRID manual.
const slaveWithAppliersOut = ` HA-Node Info (current hab-2, state slave)
   Node hab-2 (priority 3, state slave)
   Node hab-1 (priority 2, state slave)
   Node hab-0 (priority 1, state master)

 HA-Process Info (master 56, state slave)
   Applylogdb appdb@localhost:/var/lib/cubrid/databases/appdb_hab-1 (pid 329, state registered)
   Applylogdb appdb@localhost:/var/lib/cubrid/databases/appdb_hab-0 (pid 327, state registered)
   Copylogdb appdb@hab-1:/var/lib/cubrid/databases/appdb_hab-1 (pid 325, state registered)
   Copylogdb appdb@hab-0:/var/lib/cubrid/databases/appdb_hab-0 (pid 323, state registered)
   Server appdb (pid 65, state registered_and_standby)`

func TestParseHAStatus_Appliers(t *testing.T) {
	got := ParseHAStatus(slaveWithAppliersOut).Appliers
	want := []ApplierProcess{
		{LogPath: "/var/lib/cubrid/databases/appdb_hab-1", PID: 329},
		{LogPath: "/var/lib/cubrid/databases/appdb_hab-0", PID: 327},
	}
	if !slices.Equal(got, want) {
		t.Errorf("appliers = %+v, want %+v", got, want)
	}
	if got := ParseHAStatus(transitionOut).Appliers; len(got) != 0 {
		t.Errorf("appliers of a status without applier lines = %+v, want none", got)
	}
}

// The reading of a slave's replication names the applier process of the
// master's log, by its process ID and start time (#360).
func TestReplicationOf_NamesTheApplierOfTheMastersLog(t *testing.T) {
	const want = "pid:327:48213"
	started := map[int]string{327: "48213"}
	defer func(old func(int) (string, bool)) { processStarted = old }(processStarted)
	processStarted = func(pid int) (string, bool) {
		at, ok := started[pid]
		return at, ok
	}
	st := ParseHAStatus(slaveWithAppliersOut)
	cli := fakeCLI{out: applyinfoOutput(t, "applyinfo-local.txt")}
	got := replicationOf(context.Background(), cli, st, "appdb", "/var/lib/cubrid/databases")
	if got == nil || got.Source != "hab-0" {
		t.Fatalf("replication = %+v, want a reading of hab-0's log", got)
	}
	if got.Applier != want {
		t.Errorf("applier = %q, want the process of hab-0's log with its start time", got.Applier)
	}

	// A process whose start cannot be read has no identity: one without the
	// start time would differ from the one read before and look like a
	// restart.
	delete(started, 327)
	if got := replicationOf(context.Background(), cli, st, "appdb", "/var/lib/cubrid/databases"); got.Applier != "" {
		t.Errorf("applier without a readable start = %q, want none", got.Applier)
	}
	started[327] = "48213"
	// The paths are compared after filepath.Clean.
	if got := replicationOf(context.Background(), cli, st, "appdb", "/var/lib/cubrid/databases/"); got.Applier != want {
		t.Errorf("applier for a databases directory with a trailing slash = %q, want %s", got.Applier, want)
	}
	unclean := ParseHAStatus(strings.Replace(slaveWithAppliersOut, "databases/appdb_hab-0", "databases//appdb_hab-0/", 1))
	if got := replicationOf(context.Background(), cli, unclean, "appdb", "/var/lib/cubrid/databases"); got.Applier != want {
		t.Errorf("applier listed with an unclean path = %q, want %s", got.Applier, want)
	}
	// No applier line for the master's log: the identity is not known.
	st.Appliers = st.Appliers[:1]
	if got := replicationOf(context.Background(), cli, st, "appdb", "/var/lib/cubrid/databases"); got.Applier != "" {
		t.Errorf("applier without a line for the log = %q, want none", got.Applier)
	}
}

// A slave without an applier line for its master's log is logged once, and
// again only after an applier was found in between.
func TestLogMissingApplier_LogsOnce(t *testing.T) {
	var buf bytes.Buffer
	s := NewServer(fakeCLI{}, "tok").WithReplication("appdb", "/var/lib/cubrid/databases")
	s.WithLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	withApplier := ParseHAStatus(slaveWithAppliersOut)
	withApplier.Replication = &ApplyConvergence{Source: "hab-0"}
	without := withApplier
	without.Appliers = withApplier.Appliers[:1]
	const msg = "Found no log applier process for the master's log"
	for i, st := range []HAStatus{without, without, without, withApplier, without} {
		s.logMissingApplier(context.Background(), st)
		if i == 2 && strings.Count(buf.String(), msg) != 1 {
			t.Fatalf("after three readings without an applier: log = %q, want the message once", buf.String())
		}
	}
	if got := strings.Count(buf.String(), msg); got != 2 {
		t.Errorf("log has the message %d times, want 2: %s", got, buf.String())
	}
	if !strings.Contains(buf.String(), `"logPath":"/var/lib/cubrid/databases/appdb_hab-0"`) {
		t.Errorf("log = %s, want the master's log path", buf.String())
	}
	// A reading of no slave logs nothing.
	buf.Reset()
	s.logMissingApplier(context.Background(), HAStatus{})
	s.logMissingApplier(context.Background(), without)
	if strings.Count(buf.String(), msg) != 1 {
		t.Errorf("after a reading of no slave: log = %q, want the message once more", buf.String())
	}
}

// The start time is field 22 of /proc/<pid>/stat, counted past a process
// name that may itself hold spaces and parentheses.
func TestStartTicks(t *testing.T) {
	const rest = " S 1 327 327 0 -1 4194560 2143 0 0 0 12 7 0 0 20 0 3 0 48213 123456789 2048 18446744073709551615 1 1 0 0 0 0"
	for name, stat := range map[string]string{
		"a plain name":            "327 (cub_admin)" + rest,
		"a name with parentheses": "327 (cub (ad) min)" + rest,
	} {
		if got, ok := startTicks(stat); !ok || got != "48213" {
			t.Errorf("%s: start = %q (%v), want 48213", name, got, ok)
		}
	}
	for name, stat := range map[string]string{
		"an empty file":  "",
		"too few fields": "327 (cub_admin) S 1 327",
		"not a number":   "327 (cub_admin)" + strings.Replace(rest, "48213", "soon", 1),
	} {
		if got, ok := startTicks(stat); ok {
			t.Errorf("%s: start = %q, want none", name, got)
		}
	}
}
