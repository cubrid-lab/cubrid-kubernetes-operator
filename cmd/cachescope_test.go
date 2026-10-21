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
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/controller"
)

// TestManagedCacheScope runs the manager's cache options against an API
// server that also holds Secrets and Pods the operator does not manage, and
// a Secret under the name of a cluster's token Secret that the operator did
// not write.
func TestManagedCacheScope(t *testing.T) {
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	adminConfig, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	admin, err := client.New(adminConfig, client.Options{Scheme: scheme})
	mustWatch(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const namespace, unrelated = "cache-scope", "unrelated"
	mustWatch(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	managed := map[string]string{controller.ManagedByLabel: controller.ManagedByOperator}
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}
	mustWatch(t, admin.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: unrelated, Namespace: namespace},
		Data:       map[string][]byte{"password": []byte("not the operator's")},
	}))
	mustWatch(t, admin.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: unrelated, Namespace: namespace}, Spec: podSpec,
	}))
	mustWatch(t, admin.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "managed-0", Namespace: namespace, Labels: managed}, Spec: podSpec,
	}))
	foreignData := map[string][]byte{"token": []byte("written by someone else")}
	mustWatch(t, admin.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "collide-im-token", Namespace: namespace},
		Data:       foreignData,
	}))
	for _, name := range []string{"managed", "collide"} {
		cluster := &databasev1alpha1.CubridCluster{}
		data, err := os.ReadFile(filepath.Join("..", "config", "samples", "database_v1alpha1_cubridcluster.yaml"))
		mustWatch(t, err)
		mustWatch(t, yaml.Unmarshal(data, cluster))
		cluster.Name, cluster.Namespace = name, namespace
		mustWatch(t, admin.Create(ctx, cluster))
	}

	options, err := watchCacheOptions("")
	mustWatch(t, err)
	skipNameValidation := true
	manager, err := ctrl.NewManager(adminConfig, ctrl.Options{
		Scheme: scheme, Cache: options, Metrics: metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0", Controller: config.Controller{SkipNameValidation: &skipNameValidation},
	})
	mustWatch(t, err)
	mustWatch(t, (&controller.CubridClusterReconciler{
		Client: manager.GetClient(), Scheme: scheme, Recorder: record.NewFakeRecorder(1000),
	}).SetupWithManager(manager))
	managerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- manager.Start(managerCtx) }()
	defer func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop")
		}
	}()
	if !manager.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("manager cache did not synchronize")
	}
	cached := manager.GetClient()

	// The cluster's own credentials are read through the cache.
	var tokens []string
	waitWatch(t, ctx, func() bool {
		tokens, err = controller.ClusterTokens(cached)(ctx, "managed-0", namespace)
		return err == nil && len(tokens) == 1
	})
	mustWatch(t, cached.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "managed-0"}, &corev1.Pod{}))

	// Objects the operator does not manage stay out of the cache.
	err = cached.Get(ctx, client.ObjectKey{Namespace: namespace, Name: unrelated}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("cached read of an unmanaged Secret = %v, want NotFound", err)
	}
	err = cached.Get(ctx, client.ObjectKey{Namespace: namespace, Name: unrelated}, &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("cached read of an unmanaged Pod = %v, want NotFound", err)
	}

	// A token Secret the operator did not write is neither adopted nor
	// overwritten, and the cluster says why.
	cluster := &databasev1alpha1.CubridCluster{}
	waitWatch(t, ctx, func() bool {
		mustWatch(t, admin.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "collide"}, cluster))
		ready := meta.FindStatusCondition(cluster.Status.Conditions, "Ready")
		return ready != nil && ready.Reason == "SecretReconcileFailed"
	})
	ready := meta.FindStatusCondition(cluster.Status.Conditions, "Ready")
	if !strings.Contains(ready.Message, controller.ManagedByLabel) ||
		!strings.Contains(ready.Message, "collide-im-token") {
		t.Errorf("Ready message %q does not name the Secret and the missing label", ready.Message)
	}
	foreign := &corev1.Secret{}
	mustWatch(t, admin.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "collide-im-token"}, foreign))
	if len(foreign.OwnerReferences) != 0 || len(foreign.Labels) != 0 || !reflect.DeepEqual(foreign.Data, foreignData) {
		t.Errorf("unmanaged Secret was changed: owners=%v labels=%v data=%q",
			foreign.OwnerReferences, foreign.Labels, foreign.Data)
	}
}
