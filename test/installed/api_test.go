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

package installed

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envtest establishes API-server precondition behavior. It has no namespace
// garbage collector, so this test makes no namespace-teardown claim.
func TestNamespaceUIDPreconditionOnRealAPI(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("make test provides the envtest API/etcd assets")
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cubrid-lab-uid-precondition"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	wrong := types.UID("a-different-namespace")
	opts := &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &wrong}}
	if err := c.Delete(ctx, ns, opts); !apierrors.IsConflict(err) {
		t.Fatalf("API accepted a mismatched namespace UID: %v", err)
	}
	current := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ns), current); err != nil || current.UID != ns.UID {
		t.Fatalf("UID conflict changed the namespace: uid=%s, error=%v", current.UID, err)
	}
}
