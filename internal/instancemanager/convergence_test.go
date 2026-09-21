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

const convergedApplyinfo = ` *** Applied Info. ***
Insert count                   : 5
Update count                   : 0
Delete count                   : 0
Commit count                   : 39
Fail count                     : 0
 *** Delay in Applying Copied Log ***
Delayed log page count         : 0
`

const laggingApplyinfo = ` *** Applied Info. ***
Insert count                   : 0
Commit count                   : 21
Fail count                     : 3
 *** Delay in Applying Copied Log ***
Delayed log page count         : 2
`

func TestParseApplyConvergence_Converged(t *testing.T) {
	c := parseApplyConvergence(convergedApplyinfo)
	if !c.Available {
		t.Fatal("Available = false, want true")
	}
	if c.InsertCount != 5 || c.FailCount != 0 || c.DelayedPageCount != 0 {
		t.Errorf("counters = %+v", c)
	}
	if !c.Converged() {
		t.Error("Converged() = false, want true")
	}
}

func TestParseApplyConvergence_NotConverged(t *testing.T) {
	c := parseApplyConvergence(laggingApplyinfo)
	if !c.Available {
		t.Fatal("Available = false, want true")
	}
	if c.FailCount != 3 || c.DelayedPageCount != 2 {
		t.Errorf("counters = %+v", c)
	}
	if c.Converged() {
		t.Error("Converged() = true, want false (fails + delayed pages)")
	}
}

func TestParseApplyConvergence_Unparseable(t *testing.T) {
	c := parseApplyConvergence("garbage without an applied info block")
	if c.Available {
		t.Error("Available = true, want false for unparseable output")
	}
	if c.Converged() {
		t.Error("unknown convergence must never report Converged")
	}
}
