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
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// firstReconcileBackoff is the wait before the first retry of a failed
// reconcile, as in controller-runtime's default rate limiter.
const firstReconcileBackoff = 5 * time.Millisecond

// clusterRateLimiter retries a failed CubridCluster reconcile with an
// exponential backoff that never exceeds haResyncInterval. A CUBRID failover
// changes no Kubernetes object, so a cluster is observed again only when its
// reconcile runs; with controller-runtime's default limit of 1000 s, a write
// that kept failing (an admission webhook or a quota refusing it) would also
// keep the cluster's roles and routing Conditions unobserved for up to that
// long. Each cluster is retried at most once per haResyncInterval while it
// keeps failing.
func clusterRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](firstReconcileBackoff, haResyncInterval)
}
