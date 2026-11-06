//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

// TestSettledRolesWaitsOutAShortToBeMaster checks the read at the start of the
// S14 token steps (#704): a member that is `to-be-master` beside one active
// master is read again until the roles settle, and the wait is reported; two
// active masters, a change of master, any other state, a failed read and a
// state that outlasts the bound still fail.
func TestSettledRolesWaitsOutAShortToBeMaster(t *testing.T) {
	members := []string{"m-0", "m-1", "m-2"}
	master := memberRole{role: "master", state: "master"}
	slave := memberRole{role: "slave", state: "slave"}
	tbm := memberRole{state: "to-be-master"}
	stable := memberRoles{"m-0": master, "m-1": slave, "m-2": slave}
	waiting := memberRoles{"m-0": master, "m-1": tbm, "m-2": slave}
	readErr := errors.New("exec failed")

	// reads answers each call with the next reading, and the last one after.
	reads := func(seq ...any) func(context.Context) (memberRoles, error) {
		i := 0
		return func(context.Context) (memberRoles, error) {
			next := seq[min(i, len(seq)-1)]
			i++
			if err, ok := next.(error); ok {
				return nil, err
			}
			return next.(memberRoles), nil
		}
	}
	for name, tc := range map[string]struct {
		read      func(context.Context) (memberRoles, error)
		wantErr   string
		wantNoted string
	}{
		"settled at once": {read: reads(stable)},
		"to-be-master that ends": {read: reads(waiting, waiting, stable),
			wantNoted: "m-1 to-be-master"},
		"two active masters": {read: reads(waiting,
			memberRoles{"m-0": master, "m-1": master, "m-2": slave}, stable),
			wantErr: "masters [m-0 m-1]", wantNoted: "m-1 to-be-master"},
		"master changed during the wait": {read: reads(waiting,
			memberRoles{"m-0": slave, "m-1": master, "m-2": slave}),
			wantErr: "master changed from m-0 to m-1", wantNoted: "m-1 to-be-master"},
		"another state": {read: reads(memberRoles{"m-0": master, "m-1": {state: "unknown"}, "m-2": slave}, stable),
			wantErr: "m-1"},
		"to-be-master with no active master": {read: reads(
			memberRoles{"m-0": {state: "to-be-master"}, "m-1": slave, "m-2": slave}, stable),
			wantErr: "m-0", wantNoted: "m-0 to-be-master with no active master"},
		"a failed read": {read: reads(waiting, readErr, stable), wantErr: "exec failed", wantNoted: "m-1 to-be-master"},
		"to-be-master past the bound": {read: reads(waiting),
			wantErr: "still to-be-master", wantNoted: "m-1 to-be-master"},
		"two active masters beside a to-be-master": {read: reads(
			memberRoles{"m-0": master, "m-1": master, "m-2": tbm}, stable),
			wantErr: "masters [m-0 m-1]", wantNoted: "m-2 to-be-master"},
		"the master gone during the wait": {read: reads(waiting,
			memberRoles{"m-0": slave, "m-1": tbm, "m-2": slave}, stable),
			wantErr: "no active master", wantNoted: "m-1 to-be-master"},
		"a master whose server is not active": {read: reads(
			memberRoles{"m-0": master, "m-1": {state: "master"}, "m-2": slave}, stable),
			wantErr: "role on m-1: neither an active master"},
		"to-be-master, then every member a slave": {read: reads(waiting,
			memberRoles{"m-0": slave, "m-1": slave, "m-2": slave}),
			wantErr: "masters [], slaves [m-0 m-1 m-2]", wantNoted: "m-1 to-be-master"},
		"another state beside a to-be-master": {read: reads(
			memberRoles{"m-0": master, "m-1": tbm, "m-2": {state: "unknown"}}, stable),
			wantErr: "m-2", wantNoted: "m-1 to-be-master"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := settledRoles(context.Background(), members, tc.read, 200*time.Millisecond, 10*time.Millisecond)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("error %v", err)
				}
				if got.master != "m-0" || strings.Join(got.slaves, ",") != "m-1,m-2" {
					t.Errorf("got master %q, slaves %v", got.master, got.slaves)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
				}
				var unmet *faults.Unmet
				if !errors.Is(err, readErr) && !errors.As(err, &unmet) {
					t.Errorf("error %T is neither the read's error nor an unmet role", err)
				}
			}
			if !strings.Contains(got.noted, tc.wantNoted) || (tc.wantNoted == "") != (got.noted == "") {
				t.Errorf("noted %q, want %q", got.noted, tc.wantNoted)
			}
		})
	}
}
