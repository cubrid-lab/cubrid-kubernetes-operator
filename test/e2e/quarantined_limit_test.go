//go:build e2e

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

// A master Pod deleted with --force is not confirmed terminated, so the write
// quarantine keeps the read-write path closed for longer: the abrupt variants
// of S03 are judged by quarantined_failover_limit, and the graceful one keeps
// failover_limit (ADR-0005, section 8, as amended by #870).
func TestS03FailoverLimitByVariant(t *testing.T) {
	for variant, want := range map[string]string{
		s03Graceful:    "failover_limit",
		s03Abrupt:      "quarantined_failover_limit",
		s03AbruptFirst: "quarantined_failover_limit",
	} {
		if got := s03FailoverLimit(variant).Name; got != want {
			t.Errorf("S03/%s is judged by %s, want %s", variant, got, want)
		}
	}
}
