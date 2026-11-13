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
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
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

// TestManagedCacheScope runs the manager's cache options, under an identity
// that holds only config/rbac/role.yaml, against an API server that also holds
// objects of every namespaced kind the operator does not manage, and a Secret
// under the name of a cluster's token Secret that the operator did not write.
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const namespace, unrelated, managedCluster = "cache-scope", "unrelated", "managed"
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
	claimSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
		},
	}
	mustWatch(t, admin.Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: unrelated, Namespace: namespace}, Spec: claimSpec,
	}))
	mustWatch(t, admin.Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-managed-0", Namespace: namespace, Labels: managed}, Spec: claimSpec,
	}))
	foreignData := map[string][]byte{"token": []byte("written by someone else")}
	mustWatch(t, admin.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "collide-im-token", Namespace: namespace},
		Data:       foreignData,
	}))
	for _, name := range []string{managedCluster, "collide"} {
		cluster := &databasev1alpha1.CubridCluster{}
		data, err := os.ReadFile(filepath.Join("..", "config", "samples", "database_v1alpha1_cubridcluster.yaml"))
		mustWatch(t, err)
		mustWatch(t, yaml.Unmarshal(data, cluster))
		cluster.Name, cluster.Namespace = name, namespace
		mustWatch(t, admin.Create(ctx, cluster))
	}

	for _, object := range unmanagedObjects(unrelated, namespace) {
		mustWatch(t, admin.Create(ctx, object))
	}

	options, err := watchCacheOptions("")
	mustWatch(t, err)
	skipNameValidation := true
	manager, err := ctrl.NewManager(generatedRoleConfig(t, ctx, environment, adminConfig, admin), ctrl.Options{
		Scheme: scheme, Cache: options, Metrics: metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0", Controller: config.Controller{SkipNameValidation: &skipNameValidation},
	})
	mustWatch(t, err)
	mustWatch(t, (&controller.CubridClusterReconciler{
		Client: manager.GetClient(), APIReader: manager.GetAPIReader(), Scheme: scheme,
		Recorder: record.NewFakeRecorder(1000),
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

	// Every member Pod carries the label the cache selects on.
	sts := &appsv1.StatefulSet{}
	waitWatch(t, ctx, func() bool {
		return admin.Get(ctx, client.ObjectKey{Namespace: namespace, Name: managedCluster}, sts) == nil
	})
	if got := sts.Spec.Template.Labels[controller.ManagedByLabel]; got != controller.ManagedByOperator {
		t.Errorf("StatefulSet Pod template label %s = %q, want %q",
			controller.ManagedByLabel, got, controller.ManagedByOperator)
	}
	// The StatefulSet controller labels each data PVC with the selector's
	// labels, so the members' PVCs carry the label too.
	if got := sts.Spec.Selector.MatchLabels[controller.ManagedByLabel]; got != controller.ManagedByOperator {
		t.Errorf("StatefulSet selector label %s = %q, want %q",
			controller.ManagedByLabel, got, controller.ManagedByOperator)
	}
	mustWatch(t, cached.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "data-managed-0"},
		&corev1.PersistentVolumeClaim{}))

	// Objects the operator does not manage stay out of the cache.
	err = cached.Get(ctx, client.ObjectKey{Namespace: namespace, Name: unrelated}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("cached read of an unmanaged Secret = %v, want NotFound", err)
	}
	err = cached.Get(ctx, client.ObjectKey{Namespace: namespace, Name: unrelated}, &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("cached read of an unmanaged Pod = %v, want NotFound", err)
	}
	err = cached.Get(ctx, client.ObjectKey{Namespace: namespace, Name: unrelated}, &corev1.PersistentVolumeClaim{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("cached read of an unmanaged PersistentVolumeClaim = %v, want NotFound", err)
	}

	for _, object := range unmanagedObjects(unrelated, namespace) {
		err = cached.Get(ctx, client.ObjectKeyFromObject(object), object)
		if !apierrors.IsNotFound(err) {
			t.Errorf("cached read of an unmanaged %T = %v, want NotFound", object, err)
		}
	}

	// The cluster's own objects of those kinds are still cached.
	for _, list := range []client.ObjectList{
		&corev1.ConfigMapList{}, &corev1.ServiceList{}, &appsv1.StatefulSetList{}, &appsv1.DeploymentList{},
		&networkingv1.NetworkPolicyList{}, &policyv1.PodDisruptionBudgetList{},
	} {
		waitWatch(t, ctx, func() bool {
			mustWatch(t, cached.List(ctx, list, client.InNamespace(namespace),
				client.MatchingLabels{"app.kubernetes.io/instance": managedCluster}))
			return meta.LenList(list) > 0
		})
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

// unmanagedObjects returns one object of each kind the manager writes, none
// with the managed-by label.
func unmanagedObjects(name, namespace string) []client.Object {
	objectMeta := metav1.ObjectMeta{Name: name, Namespace: namespace}
	selected := map[string]string{"app": name}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: selected},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}
	return []client.Object{
		&corev1.ConfigMap{ObjectMeta: objectMeta},
		&corev1.Service{ObjectMeta: objectMeta, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}},
		&appsv1.StatefulSet{ObjectMeta: objectMeta, Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selected}, Template: template,
		}},
		&appsv1.Deployment{ObjectMeta: objectMeta, Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selected}, Template: template,
		}},
		&networkingv1.NetworkPolicy{ObjectMeta: objectMeta},
		&policyv1.PodDisruptionBudget{ObjectMeta: objectMeta},
	}
}

// generatedRoleConfig returns the credentials of an identity bound to the
// generated ClusterRole and nothing else, so that a call the role does not
// allow fails as it would for the deployed manager.
func generatedRoleConfig(t *testing.T, ctx context.Context, environment *envtest.Environment,
	adminConfig *rest.Config, admin client.Client,
) *rest.Config {
	t.Helper()
	identity := envtest.User{Name: "generated-role"}
	user, err := environment.AddUser(identity, adminConfig)
	mustWatch(t, err)
	role := generatedRole(t)
	role.Name = identity.Name
	mustWatch(t, admin.Create(ctx, role))
	mustWatch(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: identity.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: identity.Name}},
	}))
	return user.Config()
}

func generatedRole(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "config", "rbac", "role.yaml"))
	mustWatch(t, err)
	role := &rbacv1.ClusterRole{}
	mustWatch(t, yaml.Unmarshal(data, role))
	return role
}

// TestGeneratedRoleGrantsOnlyWhatTheManagerCalls pins the verbs of the
// generated ClusterRole to the calls of cmd/ and internal/controller. A verb
// that nothing calls is authority a stolen manager token would have for no
// reason, and a verb removed from here must have lost its last caller.
func TestGeneratedRoleGrantsOnlyWhatTheManagerCalls(t *testing.T) {
	const (
		read     = "get list watch"
		update   = "update"
		writes   = read + " create " + update
		removals = writes + " delete"
	)
	want := map[string]string{
		// reconcileChild creates and updates; a ConfigMap is never removed.
		"/configmaps": writes,
		// Member Services of members that no longer exist are removed.
		"/services": removals,
		// The token Secret is created and updated, never removed.
		"/secrets": writes,
		// Replacing a member during an update deletes its Pod.
		"/pods": read + " delete",
		// The log claim of a new member whose log claim was bound to a node
		// without its data claim is deleted (#856); a data claim never is.
		"/persistentvolumeclaims": read + " delete",
		// The recorder creates an Event and patches a repeated one.
		"/events":           "create patch",
		"apps/statefulsets": writes,
		// The single Deployment of earlier versions is removed.
		"apps/deployments": removals,
		// Policies are removed when spec.networkPolicy turns them off.
		"networking.k8s.io/networkpolicies": removals,
		"policy/poddisruptionbudgets":       writes,
		// The member spread default is recorded in an annotation by a patch.
		"database.cubrid.io/cubridclusters":        read + " patch",
		"database.cubrid.io/cubridclusters/status": update,
		// The owner references of a cluster's children block its deletion,
		// which the API server may check against this subresource.
		"database.cubrid.io/cubridclusters/finalizers": update,
		"database.cubrid.io/cubridbackups":             read,
		"database.cubrid.io/cubridbackups/status":      update,
	}
	got := map[string]string{}
	for _, rule := range generatedRole(t).Rules {
		verbs := strings.Join(rule.Verbs, " ")
		for _, group := range rule.APIGroups {
			for _, name := range rule.Resources {
				got[group+"/"+name] = verbs
			}
		}
	}
	for granted, verbs := range want {
		wanted := strings.Fields(verbs)
		slices.Sort(wanted)
		if strings.Join(wanted, " ") != got[granted] {
			t.Errorf("verbs on %s = %q, want %q", granted, got[granted], strings.Join(wanted, " "))
		}
		delete(got, granted)
	}
	for granted, verbs := range got {
		t.Errorf("verbs on %s = %q, want none", granted, verbs)
	}
}
