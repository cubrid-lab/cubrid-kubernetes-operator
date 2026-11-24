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

import "testing"

// The Kind check of #982 counts the log copy connections with a keepalive
// timer and refuses any with more than 30 seconds left, in every format ss
// prints; loopback connections and connections without a keepalive timer
// are not counted.
func TestMemberKeepaliveCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		out      string
		counted  int
		problems int
	}{
		{"short timers", "0 0 10.244.0.12:43210 10.244.0.13:1523 timer:(keepalive,27sec,0)\n" +
			"0 0 10.244.0.12:43212 10.244.0.14:1523 timer:(keepalive,4.304sec,0)\n", 2, 0},
		{"iproute2 before 6.11", "0 0 10.244.0.12:43210 10.244.0.13:1523 timer:(keepalive,4.304ms,0)", 1, 0},
		{"milliseconds", "0 0 10.244.0.12:43210 10.244.0.13:1523 timer:(keepalive,500ms,0)", 1, 0},
		{"overdue", "0 0 10.244.0.12:43210 10.244.0.13:1523 timer:(keepalive,-1.2sec,0)", 1, 0},
		{"exactly the limit", "0 0 10.244.0.12:43210 10.244.0.13:1523 timer:(keepalive,30sec,0)", 1, 0},
		{"the kernel default", "0 0 10.244.0.12:43210 10.244.0.13:1523 timer:(keepalive,119min,0)", 1, 1},
		{"over the limit", "0 0 10.244.0.12:43210 10.244.0.13:1523 timer:(keepalive,1min5sec,0)", 1, 1},
		{"loopback", "0 0 127.0.0.1:43210 127.0.0.1:1523 timer:(keepalive,119min,0)\n" +
			"0 0 [::ffff:127.0.0.1]:43211 [::ffff:127.0.0.1]:1523 timer:(keepalive,119min,0)", 0, 0},
		{"sending", "0 120 10.244.0.12:43210 10.244.0.13:1523 timer:(on,200ms,0)", 0, 0},
		{"none", "", 0, 0},
	} {
		counted, problems := memberKeepaliveCheck(tc.out)
		if counted != tc.counted || len(problems) != tc.problems {
			t.Errorf("%s: counted %d with problems %q, want %d with %d problems",
				tc.name, counted, problems, tc.counted, tc.problems)
		}
	}
}
