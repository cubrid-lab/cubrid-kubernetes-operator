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

package main

import (
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/installed"
)

// The node drain is judged by failover_limit only when -failover-limit was
// given: its 30-second default is the Kind value, and the contract records
// none for the VM lab, so without the flag the limit is unset (a baseline).
func TestNodeDrainFailoverLimitIsUnsetUnlessGiven(t *testing.T) {
	for _, tc := range []struct {
		fault    string
		explicit bool
		want     time.Duration
	}{
		{installed.FaultDrainSlaveNode, false, 0},
		{installed.FaultDrainMasterNode, false, 0},
		{installed.FaultDrainSlaveNode, true, 30 * time.Second},
		{installed.FaultVMPowerOff, false, 30 * time.Second},
	} {
		cfg := installed.HAConfig{Fault: tc.fault, FailoverLimit: 30 * time.Second}
		s15FailoverLimit(&cfg, map[string]bool{"failover-limit": tc.explicit})
		if cfg.FailoverLimit != tc.want {
			t.Errorf("%s with the flag given %t: failover limit %s, want %s", tc.fault, tc.explicit,
				cfg.FailoverLimit, tc.want)
		}
	}
}
