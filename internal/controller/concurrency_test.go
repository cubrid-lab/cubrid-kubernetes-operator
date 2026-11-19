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

import "testing"

// The manager reconciles several clusters at once by default (#1003), and
// refuses a worker count that would run no reconcile at all.
func TestValidateClusterMaxConcurrentReconciles(t *testing.T) {
	if DefaultClusterMaxConcurrentReconciles != 4 {
		t.Fatalf("default is %d, want 4", DefaultClusterMaxConcurrentReconciles)
	}
	for _, n := range []int{1, 4, 16} {
		if err := ValidateClusterMaxConcurrentReconciles(n); err != nil {
			t.Errorf("%d workers refused: %v", n, err)
		}
	}
	for _, n := range []int{0, -1} {
		if err := ValidateClusterMaxConcurrentReconciles(n); err == nil {
			t.Errorf("%d workers accepted; the cluster controller would reconcile nothing", n)
		}
	}
}
