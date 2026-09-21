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

import "testing"

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
