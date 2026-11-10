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
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// GET /v1/role reports every request check this manager enforces, whatever
// the member's role, so that the Operator can ask before it sends a request
// that relies on one (ADR-0013, #733). A member without a running database
// answers too: that is the member a seed restore is sent to.
func TestServer_Role_ReportsTheChecksItEnforces(t *testing.T) {
	st := roleAnswer(t, NewServer(&countingCLI{}, "tok"))
	if st.Role != RoleUnknown {
		t.Fatalf("role = %s, want unknown for a member with no heartbeat", st.Role)
	}
	for _, c := range []string{CapabilityExpectedClusterUID, CapabilitySeedFromMaster, CapabilitySeedUpload, CapabilityBackupSourceRole} {
		if !slices.Contains(st.Capabilities, c) {
			t.Errorf("capabilities = %v, want %q among them", st.Capabilities, c)
		}
	}
}

// A request with a field this manager does not know is refused with 400
// before anything is done for it. A manager that ignored the field would skip
// the check the field carries and answer as if it had applied it (ADR-0013,
// #733). The field here stands for one a later Operator adds.
func TestServer_RefusesARequestFieldItDoesNotKnow(t *testing.T) {
	cases := []struct {
		target string
		body   func(f *validationFixture) any
	}{
		{pathBackup, func(f *validationFixture) any {
			return BackupRequest{Database: dbName, Destination: filepath.Join(f.staging, "bk")}
		}},
		{pathRestorePrepare, func(f *validationFixture) any {
			return RestoreRequest{Database: dbName, Bucket: testUpload.Bucket, Prefix: testUpload.Prefix,
				StagingDir: filepath.Join(f.staging, "restore"), TargetDir: filepath.Join(f.databases, dbName)}
		}},
		{pathHABootstrap, func(*validationFixture) any { return HABootstrapRequest{Database: dbName} }},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			f := newValidationFixture(t)
			known, err := json.Marshal(c.body(f))
			if err != nil {
				t.Fatal(err)
			}
			body := strings.TrimSuffix(string(known), "}") + `,"expectedSomethingNew":"value"}`
			rr := f.send(http.MethodPost, c.target, body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s with an unknown field = %d (%s), want 400", c.target, rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			var answer ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &answer); err != nil || !strings.Contains(answer.Error, "expectedSomethingNew") {
				t.Errorf("%s answered %q, want the unknown field named", c.target, rr.Body.String())
			}
			if answer.Code != "UnknownRequestField" {
				t.Errorf("%s answered code %q, want UnknownRequestField", c.target, answer.Code)
			}
			f.wantNoWork(c.target)
		})
	}

	// A body that is more than one JSON value stays refused as before.
	t.Run("two JSON values", func(t *testing.T) {
		f := newValidationFixture(t)
		rr := f.send(http.MethodPost, pathHABootstrap, `{"database":"`+dbName+`"}{}`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s with two JSON values = %d, want 400", pathHABootstrap, rr.Code)
		}
		f.wantNoWork("two JSON values")
	})
}
