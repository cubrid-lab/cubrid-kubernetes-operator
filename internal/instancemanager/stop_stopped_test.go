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

// stoppedCLI answers as a member whose cub_master is not running: the stop
// commands exit non-zero, and heartbeat status prints no node.
type stoppedCLI struct{}

func (stoppedCLI) Run(_ context.Context, _ string, args ...string) (string, error) {
	if strings.Join(args, " ") == "heartbeat status" {
		return noNodeOut, nil
	}
	return "++ cubrid master is not running.\n", errors.New("exit status 1")
}

// Stopping a member that has nothing running is answered as success, and the
// next stop looks again: a member that runs CUBRID again is stopped, and a
// failing stop of a member that still runs something stays an error.
func TestServer_Stop_AlreadyStoppedMember(t *testing.T) {
	s := NewServer(stoppedCLI{}, "tok")
	running := map[string]bool{}
	s.heartbeatProcesses = func() (map[string]bool, error) { return running, nil }
	for range 2 {
		if err := s.Stop(context.Background(), "appdb"); err != nil {
			t.Fatalf("Stop of a stopped member = %v, want nil", err)
		}
	}
	running["cub_server"] = true
	if err := s.Stop(context.Background(), "appdb"); err == nil {
		t.Fatal("Stop with cub_server running and failing commands = nil, want an error")
	}
	s.heartbeatProcesses = func() (map[string]bool, error) { return nil, errors.New("no /proc") }
	if err := s.Stop(context.Background(), "appdb"); err == nil {
		t.Fatal("Stop with an unreadable process table = nil, want an error")
	}
}
