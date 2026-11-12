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
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// A reconcile that keeps failing is retried with backoff, but an HA cluster
// must still be observed at least every haResyncInterval: a failover changes
// no Kubernetes object, so only the reconcile brings the new roles in (#799).
func TestClusterRateLimiterNeverWaitsLongerThanTheHAResync(t *testing.T) {
	limiter := clusterRateLimiter()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "c"}}
	if first := limiter.When(req); first != firstReconcileBackoff {
		t.Errorf("first retry after %s, want %s", first, firstReconcileBackoff)
	}
	var last = firstReconcileBackoff
	for range 40 {
		last = limiter.When(req)
		if last > haResyncInterval {
			t.Fatalf("a failing reconcile waits %s, longer than haResyncInterval %s", last, haResyncInterval)
		}
	}
	if last != haResyncInterval {
		t.Errorf("after 40 failures the wait is %s, want the cap %s", last, haResyncInterval)
	}
	limiter.Forget(req)
	if again := limiter.When(req); again != firstReconcileBackoff {
		t.Errorf("after a success the first retry waits %s, want %s", again, firstReconcileBackoff)
	}
}
