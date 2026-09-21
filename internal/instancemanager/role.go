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
	"regexp"
	"strings"
)

// Role is the CUBRID HA runtime role of the local node (ADR-0001).
type Role string

const (
	RoleMaster  Role = "master"
	RoleSlave   Role = "slave"
	RoleReplica Role = "replica"
	RoleUnknown Role = "unknown"
)

// HAStatus is the parsed view of `cubrid heartbeat status` for the local node.
type HAStatus struct {
	// Current is the local node's short HA hostname (the "current <name>").
	Current string `json:"current"`
	// Role is the local node's runtime role; unknown when not authoritative.
	Role Role `json:"role"`
	// ServerActive reports whether the local DB server is registered_and_active
	// (a real writable master) rather than merely a master-scored node.
	ServerActive bool `json:"serverActive"`
	// Nodes is the observed per-node topology (all nodes in ha_node_list).
	Nodes []NodeState `json:"nodes"`
	// Source explains how Role was derived (feeds ADR-0005 non-authoritative handling).
	Source string `json:"source"`
	// Reason is a human-readable explanation, set when Role is unknown.
	Reason string `json:"reason,omitempty"`
}

// NodeState is one node's line in the HA-Node Info block.
type NodeState struct {
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	State    string `json:"state"`
}

var (
	// " HA-Node Info (current cub-0, state master)"
	reCurrent = regexp.MustCompile(`HA-Node Info \(current ([^,]+), state (\w+)\)`)
	// "   Node cub-1 (priority 2, state slave)"
	reNode = regexp.MustCompile(`Node (\S+) \(priority (\d+), state (\w+)\)`)
	// "   Server pocdb (pid 272, state registered_and_active)"
	reServer = regexp.MustCompile(`Server \S+ \(pid \d+, state (\w+)\)`)
)

// ParseHAStatus parses `cubrid heartbeat status` output into an authoritative
// HAStatus. Role is master|slave only when the local node's state is
// unambiguous; anything else (missing/empty output, unrecognized state,
// current-node not present in the node list) yields unknown per ADR-0005.
func ParseHAStatus(out string) HAStatus {
	s := HAStatus{Role: RoleUnknown, Source: "heartbeat"}

	m := reCurrent.FindStringSubmatch(out)
	if m == nil {
		s.Reason = "no HA-Node Info line (heartbeat not running or empty output)"
		return s
	}
	s.Current = strings.TrimSpace(m[1])
	currentState := m[2]

	for _, nm := range reNode.FindAllStringSubmatch(out, -1) {
		s.Nodes = append(s.Nodes, NodeState{
			Name:     nm[1],
			Priority: atoiSafe(nm[2]),
			State:    nm[3],
		})
	}

	if sm := reServer.FindStringSubmatch(out); sm != nil {
		s.ServerActive = sm[1] == "registered_and_active"
	}

	switch currentState {
	case "master":
		s.Role = RoleMaster
	case "slave":
		s.Role = RoleSlave
	case "replica":
		s.Role = RoleReplica
	default:
		// to-be-master, registered_and_standby transitions, etc. are not
		// authoritative roles.
		s.Reason = "current node state '" + currentState + "' is not an authoritative role"
	}
	return s
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}
