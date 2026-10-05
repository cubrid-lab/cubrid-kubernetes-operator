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
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

const convergedApplyinfo = ` *** Applied Info. ***
Insert count                   : 5
Update count                   : 0
Delete count                   : 0
Commit count                   : 39
Fail count                     : 0
 *** Delay in Applying Copied Log ***
Delayed log page count         : 0
`

const laggingApplyinfo = ` *** Applied Info. ***
Insert count                   : 0
Commit count                   : 21
Fail count                     : 3
 *** Delay in Applying Copied Log ***
Delayed log page count         : 2
`

func TestParseApplyConvergence_Converged(t *testing.T) {
	c := parseApplyConvergence(convergedApplyinfo)
	if !c.Available {
		t.Fatal("Available = false, want true")
	}
	if c.InsertCount != 5 || c.FailCount != 0 || c.DelayedPageCount != 0 {
		t.Errorf("counters = %+v", c)
	}
	if !c.Converged() {
		t.Error("Converged() = false, want true")
	}
}

func TestParseApplyConvergence_NotConverged(t *testing.T) {
	c := parseApplyConvergence(laggingApplyinfo)
	if !c.Available {
		t.Fatal("Available = false, want true")
	}
	if c.FailCount != 3 || c.DelayedPageCount != 2 {
		t.Errorf("counters = %+v", c)
	}
	if c.Converged() {
		t.Error("Converged() = true, want false (fails + delayed pages)")
	}
}

func TestParseApplyConvergence_Unparseable(t *testing.T) {
	c := parseApplyConvergence("garbage without an applied info block")
	if c.Available {
		t.Error("Available = true, want false for unparseable output")
	}
	if c.Converged() {
		t.Error("unknown convergence must never report Converged")
	}
}

func applyinfoOutput(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The files under testdata are the output of CUBRID 11.4.6 as captured for
// POC-20 (docs/poc/RESULTS.md). Two are edited copies, because the state they
// stand for was not captured as a whole: applyinfo-stalled.txt has the values
// of the slave that was not applying, and applyinfo-with-remote-delays.txt
// has two delays that differ.
func TestParseApplyConvergence_RealOutput(t *testing.T) {
	tests := map[string]struct {
		file      string
		available bool
		changes   int
		commits   int
		fails     int
		delayed   int
		converged bool
	}{
		"a healthy slave":                  {"applyinfo-local.txt", true, 4, 8, 0, 0, true},
		"with -r, both delays zero":        {"applyinfo-with-remote.txt", true, 4, 8, 0, 0, true},
		"with -r, the applying delay":      {"applyinfo-with-remote-delays.txt", true, 4, 8, 0, 2, false},
		"a slave that is not applying":     {"applyinfo-stalled.txt", true, 0, 8, 0, 43, false},
		"a log path that does not exist":   {"applyinfo-bad-path.txt", false, 0, 0, 0, 0, false},
		"the master, which applies no log": {"applyinfo-on-master.txt", true, 0, 30, 0, 0, true},
	}
	for name, tc := range tests {
		c := parseApplyConvergence(applyinfoOutput(t, tc.file))
		if c.Available != tc.available {
			t.Errorf("%s: Available = %v, want %v (%s)", name, c.Available, tc.available, c.Reason)
			continue
		}
		if c.AppliedChanges != tc.changes || c.CommitCount != tc.commits || c.FailCount != tc.fails ||
			c.DelayedPageCount != tc.delayed || c.Converged() != tc.converged {
			t.Errorf("%s: %+v, converged %v", name, c, c.Converged())
		}
	}
}

// roleCLI answers by the CUBRID command, and records the applyinfo call.
type roleCLI struct {
	heartbeat string
	applyinfo string
	applyArgs []string
}

func (c *roleCLI) Run(_ context.Context, _ string, args ...string) (string, error) {
	switch {
	case len(args) > 0 && args[0] == "applyinfo":
		c.applyArgs = args
		return c.applyinfo, nil
	}
	// Everything else the role answer needs is the heartbeat status.
	return c.heartbeat, nil
}

const slaveHeartbeat = ` HA-Node Info (current ha-1, state slave)
   Node ha-2 (priority 3, state slave)
   Node ha-1 (priority 2, state slave)
   Node ha-0 (priority 1, state master)
 HA-Process Info (master 13, state slave)
   Server appdb (pid 30, state registered_and_standby)
`

// A slave's role answer says how its applier is doing for the master's log;
// a master's does not.
func TestServer_Role_ReportsReplicationOfASlave(t *testing.T) {
	cli := &roleCLI{heartbeat: slaveHeartbeat, applyinfo: applyinfoOutput(t, "applyinfo-stalled.txt")}
	h := NewServer(cli, "tok").WithReplication("appdb", "/var/lib/cubrid/databases").Handler()
	rr := doReq(t, h, "/v1/role", "tok", testRemoteAddr)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/role = %d", rr.Code)
	}
	var st HAStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Role != RoleSlave || st.Replication == nil {
		t.Fatalf("status = %+v, want a slave with replication", st)
	}
	r := st.Replication
	if !r.Available || r.Source != "ha-0" || r.DelayedPageCount != 43 || r.AppliedChanges != 0 || r.FailCount != 0 {
		t.Errorf("replication = %+v", r)
	}
	if got := strings.Join(cli.applyArgs, " "); got != "applyinfo -L /var/lib/cubrid/databases/appdb_ha-0 -a appdb" {
		t.Errorf("applyinfo was run as %q", got)
	}
}

func TestServer_Role_NoReplicationWithoutAnApplier(t *testing.T) {
	master := strings.NewReplacer("current ha-1, state slave", "current ha-0, state master",
		"registered_and_standby", "registered_and_active").Replace(slaveHeartbeat)
	twoMasters := strings.Replace(slaveHeartbeat, "Node ha-2 (priority 3, state slave)",
		"Node ha-2 (priority 3, state master)", 1)
	for name, tc := range map[string]struct {
		heartbeat  string
		configured bool
		wantNil    bool
		wantReason string
	}{
		"a master":                       {master, true, true, ""},
		"a slave that sees two masters":  {twoMasters, true, false, "not one"},
		"a member not told its database": {slaveHeartbeat, false, true, ""},
	} {
		cli := &roleCLI{heartbeat: tc.heartbeat, applyinfo: applyinfoOutput(t, "applyinfo-local.txt")}
		server := NewServer(cli, "tok")
		if tc.configured {
			server = server.WithReplication("appdb", "/var/lib/cubrid/databases")
		}
		rr := doReq(t, server.Handler(), "/v1/role", "tok", testRemoteAddr)
		var st HAStatus
		if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		switch {
		case tc.wantNil && st.Replication != nil:
			t.Errorf("%s: replication = %+v, want none", name, st.Replication)
		case !tc.wantNil && (st.Replication == nil || st.Replication.Available ||
			!strings.Contains(st.Replication.Reason, tc.wantReason)):
			t.Errorf("%s: replication = %+v, want it unavailable with a reason", name, st.Replication)
		}
		if tc.wantNil != (cli.applyArgs == nil) && tc.wantReason == "" {
			t.Errorf("%s: applyinfo run = %v", name, cli.applyArgs)
		}
	}
}
