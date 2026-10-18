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

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// noCommandCLI fails the contract check if the Instance Manager runs a
// command: a request it accepts is stopped before any work (see below).
type noCommandCLI struct{ ran []string }

func (c *noCommandCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.ran = append(c.ran, name+" "+strings.Join(args, " "))
	return "", nil
}

// instanceManagerChecks sends a backup request the operator built to an
// Instance Manager's /v1/backup and reports whether the manager's request
// checks let it through. The manager has an operation store in storeDir but no
// object storage, so a request that passes every check is answered 412
// ObjectStorageNotApplied by the step after them, before an operation is
// recorded or a command runs; a request the checks refuse is answered 400.
func instanceManagerChecks(storeDir string, req instancemanager.BackupRequest) (passed bool, answer string) {
	store, err := instancemanager.NewOperationStore(storeDir)
	if err != nil {
		return false, "NewOperationStore: " + err.Error()
	}
	cli := &noCommandCLI{}
	h := instancemanager.NewServer(cli, "tok").WithOperationStore(store).
		WithBackupStagingRoot(backupStagingRoot).Handler()
	body, err := json.Marshal(req)
	if err != nil {
		return false, err.Error()
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/backup", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer tok")
	r.Header.Set("Idempotency-Key", "contract")
	r.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	answer = strings.TrimSpace(rr.Body.String())
	return rr.Code == http.StatusPreconditionFailed && len(cli.ran) == 0 &&
		strings.Contains(answer, instancemanager.CodeObjectStorageNotApplied), answer
}

// Every backup request buildBackupRequest sends passes the Instance Manager's
// request checks (#301): a 400 there is final, so a refused shape would fail
// every backup of that kind. The selections are the ones selectBackupTarget
// returns (backuptarget.go), including the only member of a single-instance
// cluster with the role unknown under the PreferStandby fallback (ADR-0007).
func TestOperatorBackupRequestsPassTheInstanceManagersChecks(t *testing.T) {
	const contractSource = "source-0"
	cluster := sourceCluster()
	selections := []TargetSelection{
		{Instance: contractSource, Role: databasev1alpha1.RoleUnknown, Selected: true, Reason: "RoleUnknownSingleInstanceFallback"},
		{Instance: "source-1", Role: databasev1alpha1.RoleSlave, Selected: true, Reason: "StandbySelected"},
		{Instance: contractSource, Role: databasev1alpha1.RoleMaster, Selected: true, Reason: "PrimarySelected"},
		{Instance: contractSource, Role: databasev1alpha1.RoleMaster, FallbackUsed: true, Selected: true, Reason: "MasterFallbackSelected"},
	}
	for _, sel := range selections {
		for _, tc := range destinationCases {
			if !tc.runs {
				continue
			}
			t.Run(sel.Reason+"/"+tc.name, func(t *testing.T) {
				backup := destinationBackup("source-backup", cluster.Name, tc.bucket, tc.prefix)
				req := (&CubridBackupReconciler{}).buildBackupRequest(backup, cluster, sel)
				if passed, answer := instanceManagerChecks(t.TempDir(), req); !passed {
					t.Fatalf("the Instance Manager refuses the backup request %+v (upload %+v): %s", req, *req.Upload, answer)
				}
			})
		}
	}
}
