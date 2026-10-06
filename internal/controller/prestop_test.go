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
	"strings"
	"testing"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

func TestPreStopShutdown(t *testing.T) {
	cluster := &databasev1alpha1.CubridCluster{}
	cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: "appdb"}}

	lc := preStopShutdown(cluster)
	if lc == nil || lc.PreStop == nil || lc.PreStop.Exec == nil {
		t.Fatalf("preStopShutdown = %+v, want an exec PreStop hook", lc)
	}
	cmd := strings.Join(lc.PreStop.Exec.Command, " ")
	// The hook runs the one shutdown command, not a procedure of its own.
	for _, want := range []string{"/usr/local/bin/instance-manager shutdown"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("preStop command %q missing %q", cmd, want)
		}
	}
}

func TestPreStopShutdown_NoDatabase(t *testing.T) {
	lc := preStopShutdown(&databasev1alpha1.CubridCluster{})
	if lc == nil || lc.PreStop == nil || lc.PreStop.Exec == nil {
		t.Fatalf("preStopShutdown = %+v, want an exec PreStop hook even with no database", lc)
	}
	// The database is the member's own business: the command reads it from
	// the Pod's environment, so the hook is the same for every cluster.
	if !strings.Contains(strings.Join(lc.PreStop.Exec.Command, " "), "instance-manager shutdown") {
		t.Error("preStop does not run the shutdown command")
	}
}
