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
	"errors"
	"strings"
	"testing"
)

// serverNotRunningOut is what CUBRID prints when `cubrid server stop appdb`
// finds no running server of appdb (seen on the VM lab, #1063).
const serverNotRunningOut = "@ cubrid server stop: appdb\n++ cubrid server 'appdb' is not running.\n"

// heldMemberCLI answers as an HA member whose `cubrid heartbeat stop` already
// stopped the database server: cub_master still runs, so heartbeat status
// still answers with HA node information, and `cubrid server stop` exits 1.
type heldMemberCLI struct {
	heartbeatStopErr error  // what `cubrid heartbeat stop` returns
	serverStopOut    string // what `cubrid server stop` prints, exiting 1
	stops            int    // how many `cubrid server stop` calls ran
}

func (c *heldMemberCLI) Run(_ context.Context, _ string, args ...string) (string, error) {
	switch strings.Join(args, " ") {
	case "heartbeat stop":
		return "", c.heartbeatStopErr
	case stepHeartbeatStatus:
		return "HA-Node Info (current rebuild-2, state unknown)\n", nil
	}
	if len(args) >= 2 && args[0] == "server" && args[1] == "stop" {
		c.stops++
		return c.serverStopOut, errors.New("exit status 1")
	}
	return "", nil
}

func newHeldMemberServer(cli *heldMemberCLI) *Server {
	s := NewServer(cli, "tok")
	s.heartbeatProcesses = func() (map[string]bool, error) { return map[string]bool{"cub_master": true}, nil }
	return s
}

// When heartbeat stop succeeded and stopped the server, and server stop then
// answers that this database's server is not running, the member is stopped:
// Stop succeeds and records it, so a later stop does not run the commands
// again (#1063).
func TestServer_Stop_HeartbeatStopAlreadyStoppedServer(t *testing.T) {
	cli := &heldMemberCLI{serverStopOut: serverNotRunningOut}
	s := newHeldMemberServer(cli)
	if err := s.Stop(context.Background(), "appdb"); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	if !s.stopped {
		t.Fatal("Stop was not recorded as stopped")
	}
	if err := s.Stop(context.Background(), "appdb"); err != nil || cli.stops != 1 {
		t.Fatalf("second Stop = %v after %d server stops, want nil after 1", err, cli.stops)
	}
}

// A server stop that fails for any other reason, or a "not running" answer
// after heartbeat stop itself failed, is still an error. A failed heartbeat
// stop means HA participation was not positively withdrawn, so a missing
// server alone does not prove the member is stopped. A "not running" answer
// about another database does not count either.
func TestServer_Stop_ServerStopFailureStaysAnError(t *testing.T) {
	cases := map[string]*heldMemberCLI{
		"server stop fails otherwise": {serverStopOut: "@ cubrid server stop: appdb\n++ cubrid server stop: fail\n"},
		"heartbeat stop failed": {heartbeatStopErr: errors.New("exit status 1"),
			serverStopOut: serverNotRunningOut},
		"another database is not running": {serverStopOut: "@ cubrid server stop: appdb\n++ cubrid server 'otherdb' is not running.\n"},
	}
	for name, cli := range cases {
		t.Run(name, func(t *testing.T) {
			s := newHeldMemberServer(cli)
			if err := s.Stop(context.Background(), "appdb"); err == nil {
				t.Fatal("Stop = nil, want an error")
			}
			if s.stopped {
				t.Fatal("a failed Stop was recorded as stopped")
			}
		})
	}
}
