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
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

// The entrypoint's own lines, as build/instance-manager/entrypoint.sh prints
// them for a standalone member: createdb at the first start, "already
// present" at a start over a registered database.
const (
	s13FirstStartLog = "[im-entrypoint] started as root; continuing as cubrid\n" +
		"[im-entrypoint] createdb 'appdb' (registers databases.txt with absolute paths)\n" +
		"[im-entrypoint] starting Instance Manager (CUBRID_COMPONENTS=SERVER, CUBRID_BOOTSTRAP=new)\n"
	s13RetainedStartLog = "[im-entrypoint] started as root; continuing as cubrid\n" +
		"[im-entrypoint] database 'appdb' already present\n" +
		"[im-entrypoint] starting Instance Manager (CUBRID_COMPONENTS=SERVER, CUBRID_BOOTSTRAP=new)\n"
)

// A member started over retained data passes only when its log says the
// database was already present and never shows createdb; createdb at that
// start is the database created again over retained data, a failure even when
// the log is incomplete. The first start must show createdb, or the later
// check could not see one, and an incomplete log never passes.
func TestS13StartVerdict(t *testing.T) {
	tests := []struct {
		name      string
		log       string
		complete  bool
		recreated bool
		want      evidence.Result
		reason    string
	}{
		{"first start ran createdb", s13FirstStartLog, true, false, evidence.Pass, ""},
		{"first start without the createdb line", s13RetainedStartLog, true, false, evidence.Blocked,
			"does not show the entrypoint's createdb line"},
		{"first start whose earlier log is gone", s13FirstStartLog, false, false, evidence.Blocked,
			"restarted more than once"},
		{"recreated member found the database", s13RetainedStartLog, true, true, evidence.Pass, ""},
		{"recreated member ran createdb", s13FirstStartLog, true, true, evidence.Fail,
			"created again over retained data"},
		{"recreated member ran createdb in a log that is incomplete", s13FirstStartLog, false, true,
			evidence.Fail, "created again over retained data"},
		{"recreated member whose earlier log is gone", s13RetainedStartLog, false, true, evidence.Blocked,
			"restarted more than once"},
		{"recreated member shows neither line", "[im-entrypoint] starting Instance Manager\n", true, true,
			evidence.Fail, "neither createdb nor"},
		{"another database's createdb is not this one's", strings.ReplaceAll(s13FirstStartLog, "'appdb'",
			"'otherdb'") + s13RetainedStartLog, true, true, evidence.Pass, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := s13StartVerdict(tc.log, tc.complete, "appdb", tc.recreated)
			if got != tc.want || !strings.Contains(reason, tc.reason) || (tc.reason == "") != (reason == "") {
				t.Errorf("got %s %q, want %s with %q", got, reason, tc.want, tc.reason)
			}
		})
	}
}

// Under Retain the claim recorded before the deletion must still be there,
// with the same UID and volume and not being deleted; anything else is a PVC
// removed under Retain.
func TestS13RetainedPVCHeld(t *testing.T) {
	before := s13PVC{Exists: true, UID: "uid-1", Volume: "pvc-1", Phase: "Bound"}
	tests := []struct {
		name string
		now  s13PVC
		want string
	}{
		{"same claim", before, ""},
		{"same claim, now unbound from its Pod", s13PVC{Exists: true, UID: "uid-1", Volume: "pvc-1",
			Phase: "Bound"}, ""},
		{"gone", s13PVC{}, "was removed under Retain"},
		{"replaced", s13PVC{Exists: true, UID: "uid-2", Volume: "pvc-1", Phase: "Bound"}, "replaced"},
		{"being deleted", s13PVC{Exists: true, UID: "uid-1", Volume: "pvc-1", Phase: "Bound",
			DeletionTimestamp: "2026-11-12T00:00:00Z"}, "is being deleted under Retain"},
		{"on another volume", s13PVC{Exists: true, UID: "uid-1", Volume: "pvc-2", Phase: "Bound"},
			"is bound to pvc-2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s13RetainedPVCHeld("data-s13-0", before, tc.now)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("got %v, want no error", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// The wait for the removal is the test's own bound; a delete_limit that is set
// and would not fit in it extends it, so that the limit, not the wait, judges.
func TestS13LimitsFitDeleteLimit(t *testing.T) {
	for _, tc := range []struct {
		limit time.Duration
		want  time.Duration
	}{
		{0, s13DeleteWait},
		{time.Minute, s13DeleteWait},
		{s13DeleteWait, s13DeleteWait + time.Minute},
		{20 * time.Minute, 21 * time.Minute},
	} {
		got := s13Limits(faults.Limit{Name: "delete_limit", Value: tc.limit}).Outcome
		if got != tc.want {
			t.Errorf("delete_limit %s: wait %s, want %s", tc.limit, got, tc.want)
		}
	}
}
