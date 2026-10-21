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
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
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

const (
	watchTestRole    = "candidate"
	watchTestCluster = "cluster"
	watchTestBackup  = "backup"
	watchTestSecret  = "watch-sentinel"
)

func TestWatchCacheOptions(t *testing.T) {
	global, err := watchCacheOptions("")
	if err != nil || global.DefaultNamespaces != nil {
		t.Fatalf("empty scope = %+v, %v; want unchanged global default", global.DefaultNamespaces, err)
	}
	for _, namespace := range []string{watchTestRole, "team-a", "1candidate", strings.Repeat("a", 63)} {
		opts, err := watchCacheOptions(namespace)
		if err != nil || len(opts.DefaultNamespaces) != 1 {
			t.Errorf("scope %q = %+v, %v; want one namespace", namespace, opts.DefaultNamespaces, err)
		} else if _, ok := opts.DefaultNamespaces[namespace]; !ok {
			t.Errorf("scope %q omitted its namespace", namespace)
		}
	}
	for _, namespace := range []string{
		" ", " candidate ", "team-a,team-b", "*", "Team", "team.a", "-team", strings.Repeat("a", 64),
	} {
		if _, err := watchCacheOptions(namespace); err == nil {
			t.Errorf("invalid namespace %q was accepted", namespace)
		}
	}
}

func TestWatchNamespaceInvalidCLI(t *testing.T) {
	if os.Getenv("CUBRID_WATCH_SCOPE_HELPER") == "1" {
		flag.CommandLine = flag.NewFlagSet("manager", flag.ExitOnError)
		os.Args = []string{"manager", "--watch-namespace=bad,namespace"}
		main()
		t.Fatal("main accepted an invalid watch namespace")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWatchNamespaceInvalidCLI$")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "KUBECONFIG=") && !strings.HasPrefix(value, "KUBERNETES_SERVICE_HOST=") &&
			!strings.HasPrefix(value, "CUBRID_WATCH_SCOPE_HELPER=") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "CUBRID_WATCH_SCOPE_HELPER=1", "KUBECONFIG="+filepath.Join(t.TempDir(), "missing"))
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "invalid watch namespace") ||
		!strings.Contains(string(output), "--watch-namespace") {
		t.Fatalf("invalid CLI namespace did not fail before Kubernetes configuration: %v, %s", err, output)
	}
}

func TestWatchNamespaceManager(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		scoped     bool
		restricted bool
	}{{"scoped-admin", true, false}, {"scoped-role", true, true}, {"default-global", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			inside, outside := "watch-"+tc.name, "outside-"+tc.name
			for _, namespace := range []string{inside, outside} {
				mustWatch(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
				makeWatchObjects(t, ctx, admin, namespace)
			}
			candidateConfig := adminConfig
			if tc.restricted {
				candidateConfig = namespaceRoleConfig(t, ctx, environment, adminConfig, admin, inside, outside)
			}
			scope := ""
			if tc.scoped {
				scope = inside
			}
			options, err := watchCacheOptions(scope)
			mustWatch(t, err)
			skipNameValidation := true
			manager, err := ctrl.NewManager(candidateConfig, ctrl.Options{
				Scheme: scheme, Cache: options, Metrics: metricsserver.Options{BindAddress: "0"},
				HealthProbeBindAddress: "0", Controller: config.Controller{SkipNameValidation: &skipNameValidation},
			})
			mustWatch(t, err)
			const operatorNamespace = "separate-operator-namespace"
			mustWatch(t, (&controller.CubridClusterReconciler{
				Client: manager.GetClient(), Scheme: scheme, OperatorNS: operatorNamespace,
				Recorder: record.NewFakeRecorder(1000),
			}).SetupWithManager(manager))
			mustWatch(t, (&controller.CubridBackupReconciler{
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
			waitWatch(t, ctx, func() bool {
				statefulSet := &appsv1.StatefulSet{}
				backup := &databasev1alpha1.CubridBackup{}
				return admin.Get(ctx, client.ObjectKey{Namespace: inside, Name: watchTestCluster}, statefulSet) == nil &&
					admin.Get(ctx, client.ObjectKey{Namespace: inside, Name: watchTestBackup}, backup) == nil &&
					backup.Status.Phase != ""
			})
			assertWatchPrimaryCache(t, ctx, manager.GetClient(), outside, tc.scoped)
			assertWatchSecretEvents(t, ctx, manager, admin, inside, outside, tc.scoped)
			policy := &networkingv1.NetworkPolicy{}
			mustWatch(t, admin.Get(ctx, client.ObjectKey{Namespace: inside, Name: "cluster-database"}, policy))
			admitted := false
			for _, rule := range policy.Spec.Ingress {
				for _, peer := range rule.From {
					if peer.NamespaceSelector != nil &&
						peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == operatorNamespace {
						admitted = true
					}
				}
			}
			if !admitted {
				t.Error("watch namespace replaced the independent operator namespace")
			}
			if tc.scoped {
				err = admin.Get(ctx, client.ObjectKey{Namespace: outside, Name: watchTestCluster}, &appsv1.StatefulSet{})
				if !apierrors.IsNotFound(err) {
					t.Errorf("out-of-scope cluster was reconciled: %v", err)
				}
				backup := &databasev1alpha1.CubridBackup{}
				mustWatch(t, admin.Get(ctx, client.ObjectKey{Namespace: outside, Name: watchTestBackup}, backup))
				if backup.Status.Phase != "" {
					t.Errorf("out-of-scope backup was reconciled: %+v", backup.Status)
				}
			}
		})
	}
}

func makeWatchObjects(t *testing.T, ctx context.Context, admin client.Client, namespace string) {
	t.Helper()
	cluster := &databasev1alpha1.CubridCluster{}
	backup := &databasev1alpha1.CubridBackup{}
	for path, object := range map[string]client.Object{
		"database_v1alpha1_cubridcluster.yaml": cluster, "database_v1alpha1_cubridbackup.yaml": backup,
	} {
		data, err := os.ReadFile(filepath.Join("..", "config", "samples", path))
		mustWatch(t, err)
		mustWatch(t, yaml.Unmarshal(data, object))
	}
	cluster.Name, cluster.Namespace = watchTestCluster, namespace
	backup.Name, backup.Namespace, backup.Spec.ClusterRef.Name = watchTestBackup, namespace, cluster.Name
	mustWatch(t, admin.Create(ctx, cluster))
	mustWatch(t, admin.Create(ctx, backup))
	owner := metav1.NewControllerRef(cluster, databasev1alpha1.GroupVersion.WithKind("CubridCluster"))
	// Like every Secret the operator writes, the sentinel carries the
	// managed-by label, without which the manager does not cache it.
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: watchTestSecret, Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{*owner},
		Labels:          map[string]string{controller.ManagedByLabel: controller.ManagedByOperator}},
		Data: map[string][]byte{"sequence": []byte("initial")}}
	mustWatch(t, admin.Create(ctx, secret))
}

func namespaceRoleConfig(t *testing.T, ctx context.Context, environment *envtest.Environment,
	adminConfig *rest.Config, admin client.Client, inside, outside string,
) *rest.Config {
	t.Helper()
	// A new certificate identity has no system:masters or other granted groups.
	identity := envtest.User{Name: "watch-non-admin"}
	user, err := environment.AddUser(identity, adminConfig)
	mustWatch(t, err)
	if len(identity.Groups) != 0 || reflect.DeepEqual(user.Config().CertData, adminConfig.CertData) {
		t.Fatal("test identity unexpectedly retained admin credentials/groups")
	}
	data, err := os.ReadFile(filepath.Join("..", "config", "rbac", "role.yaml"))
	mustWatch(t, err)
	generated := &rbacv1.ClusterRole{}
	mustWatch(t, yaml.Unmarshal(data, generated))
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: watchTestRole, Namespace: inside}, Rules: generated.Rules}
	mustWatch(t, admin.Create(ctx, role))
	mustWatch(t, admin.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: watchTestRole, Namespace: inside},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: identity.Name}},
	}))
	limited, err := client.New(user.Config(), client.Options{Scheme: scheme})
	mustWatch(t, err)
	waitWatch(t, ctx, func() bool {
		return limited.Get(ctx, client.ObjectKey{Namespace: inside, Name: watchTestSecret}, &corev1.Secret{}) == nil
	})
	err = limited.Get(ctx, client.ObjectKey{Namespace: outside, Name: watchTestSecret}, &corev1.Secret{})
	if !apierrors.IsForbidden(err) {
		t.Errorf("uncached out-of-scope read = %v, want Forbidden", err)
	}
	err = limited.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "forbidden", Namespace: outside}})
	if !apierrors.IsForbidden(err) {
		t.Errorf("out-of-scope write = %v, want Forbidden", err)
	}
	return user.Config()
}

func assertWatchPrimaryCache(t *testing.T, ctx context.Context, cached client.Client, outside string, scoped bool) {
	t.Helper()
	clusters, backups := &databasev1alpha1.CubridClusterList{}, &databasev1alpha1.CubridBackupList{}
	mustWatch(t, cached.List(ctx, clusters))
	mustWatch(t, cached.List(ctx, backups))
	foundCluster, foundBackup := false, false
	for _, object := range clusters.Items {
		foundCluster = foundCluster || object.Namespace == outside
	}
	for _, object := range backups.Items {
		foundBackup = foundBackup || object.Namespace == outside
	}
	if foundCluster == scoped || foundBackup == scoped {
		t.Errorf("primary cache observed outside namespace: cluster=%v backup=%v scoped=%v",
			foundCluster, foundBackup, scoped)
	}
}

func assertWatchSecretEvents(t *testing.T, ctx context.Context, manager ctrl.Manager, admin client.Client,
	inside, outside string, scoped bool,
) {
	t.Helper()
	informer, err := manager.GetCache().GetInformer(ctx, &corev1.Secret{})
	mustWatch(t, err)
	var foreign atomic.Int64
	barrier := make(chan struct{}, 1)
	observe := func(object any) {
		secret, ok := object.(*corev1.Secret)
		if !ok || secret.Name != watchTestSecret {
			return
		}
		if secret.Namespace == outside {
			foreign.Add(1)
		}
		if secret.Namespace == inside && string(secret.Data["sequence"]) == "barrier" {
			select {
			case barrier <- struct{}{}:
			default:
			}
		}
	}
	registration, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: observe, UpdateFunc: func(_, object any) { observe(object) },
	})
	mustWatch(t, err)
	defer func() { mustWatch(t, informer.RemoveEventHandler(registration)) }()
	for _, namespace := range []string{outside, inside} {
		secret := &corev1.Secret{}
		mustWatch(t, admin.Get(ctx, client.ObjectKey{Namespace: namespace, Name: watchTestSecret}, secret))
		secret.Data["sequence"] = []byte("barrier")
		mustWatch(t, admin.Update(ctx, secret))
	}
	select {
	case <-barrier:
	case <-ctx.Done():
		t.Fatal("secondary watch did not observe the in-scope update")
	}
	if (foreign.Load() == 0) != scoped {
		t.Errorf("secondary Secret watch saw %d outside events; scoped=%v", foreign.Load(), scoped)
	}
}

func waitWatch(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for namespaced reconciliation/authorization")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func mustWatch(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
