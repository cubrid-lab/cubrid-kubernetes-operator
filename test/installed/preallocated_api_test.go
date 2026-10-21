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

// Package installed runs a namespace-scoped SQL connection smoke against an
// existing Operator. It never installs or removes shared infrastructure.
package installed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// This checks actual authorization and UID preconditions, with externally
// simulated readiness/child removal. Only Kind can prove SQL and real GC.
func TestPreallocatedNonAdminAPIPath(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("make test provides envtest assets")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets,
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	adminConfig, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := databasev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(adminConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig(t)
	cfg.NamespaceMode = PreallocatedNamespace
	cfg.OperatorNamespace = cfg.Namespace
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace}}
	must(admin.Create(ctx, ns))
	cfg.NamespaceUID = string(ns.UID)
	system := &corev1.Namespace{}
	must(admin.Get(ctx, client.ObjectKey{Name: testSystemNamespace}, system))
	cfg.ClusterUID = string(system.UID)
	yes := true
	marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testOwnerName, Namespace: cfg.Namespace},
		Immutable: &yes, Data: map[string]string{ownerNonceKey: "api-run"}}
	must(admin.Create(ctx, marker))
	cfg.OwnerConfigMap, cfg.OwnerUID, cfg.OwnerNonce = marker.Name, string(marker.UID), marker.Data[ownerNonceKey]
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: cfg.StorageClass},
		Provisioner: "test.example.invalid"}
	must(admin.Create(ctx, sc))
	labels := map[string]string{testAppLabel: "candidate"}
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: cfg.OperatorImage}}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: cfg.OperatorDeployment,
		Namespace: cfg.Namespace}, Spec: appsv1.DeploymentSpec{Replicas: new(int32(1)),
		Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template}}
	must(admin.Create(ctx, deployment))
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: deployment.Generation, Replicas: 1,
		UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	must(admin.Status().Update(ctx, deployment))
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "candidate-rs", Namespace: cfg.Namespace,
		OwnerReferences: []metav1.OwnerReference{owner("Deployment", deployment.Name, deployment.UID)}},
		Spec: appsv1.ReplicaSetSpec{Replicas: new(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: template}}
	must(admin.Create(ctx, rs))
	manager := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testManagerPodName, Namespace: cfg.Namespace,
		Labels: labels, OwnerReferences: []metav1.OwnerReference{owner("ReplicaSet", rs.Name, rs.UID)}},
		Spec: template.Spec}
	manager.Spec.NodeName = "test-node"
	must(admin.Create(ctx, manager))
	setAPIPodReady(manager, "manager")
	must(admin.Status().Update(ctx, manager))
	identity := envtest.User{Name: "confined-smoke"}
	user, err := environment.AddUser(identity, adminConfig)
	must(err)
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps", "pods", "persistentvolumeclaims"},
			Verbs: []string{testVerbGet, testVerbList}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments", "replicasets", "statefulsets"},
			Verbs: []string{testVerbGet, testVerbList}},
		{APIGroups: []string{"apps"}, Resources: []string{"daemonsets"}, Verbs: []string{testVerbList}},
		{APIGroups: []string{""}, Resources: []string{"replicationcontrollers"}, Verbs: []string{testVerbList}},
		{APIGroups: []string{"batch"}, Resources: []string{"jobs", "cronjobs"}, Verbs: []string{testVerbList}},
		{APIGroups: []string{testAPIGroup}, Resources: []string{testClusterResource},
			Verbs: []string{testVerbGet, testVerbList, "create", "delete"}},
		{APIGroups: []string{testAPIGroup}, Resources: []string{"cubridbackups"}, Verbs: []string{testVerbList}},
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: cfg.Namespace}, Rules: rules}
	must(admin.Create(ctx, role))
	subject := []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: identity.Name}}
	must(admin.Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterName,
		Namespace: cfg.Namespace}, Subjects: subject, RoleRef: rbacv1.RoleRef{Kind: "Role",
		APIGroup: rbacv1.GroupName, Name: role.Name}}))
	fixed := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "smoke-fixed-reads"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, ResourceNames: []string{cfg.Namespace,
			testSystemNamespace}, Verbs: []string{testVerbGet}},
		{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"},
			ResourceNames: []string{cfg.StorageClass}, Verbs: []string{testVerbGet}},
	}}
	must(admin.Create(ctx, fixed))
	must(admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: fixed.Name},
		Subjects: subject, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", APIGroup: rbacv1.GroupName, Name: fixed.Name}}))
	limited, err := client.NewWithWatch(user.Config(), client.Options{Scheme: scheme})
	must(err)
	must(pollUntil(ctx, time.Millisecond*20, func() (bool, error) {
		err := limited.Get(ctx, client.ObjectKeyFromObject(marker), &corev1.ConfigMap{})
		return err == nil, nil
	}))
	if err := limited.Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "forbidden"}}); !apierrors.IsForbidden(err) {
		t.Fatalf("namespace create was not denied: %v", err)
	}
	if err := limited.Delete(ctx, ns); !apierrors.IsForbidden(err) {
		t.Fatalf("namespace delete was not denied: %v", err)
	}
	if err := limited.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: smokePVCName,
		Namespace: cfg.Namespace}}); !apierrors.IsForbidden(err) {
		t.Fatalf("PVC delete was not denied: %v", err)
	}
	for _, object := range []client.Object{
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "forbidden-controller", Namespace: cfg.Namespace}},
		&corev1.ReplicationController{ObjectMeta: metav1.ObjectMeta{Name: "forbidden-controller", Namespace: cfg.Namespace}},
	} {
		if err := limited.Create(ctx, object); !apierrors.IsForbidden(err) {
			t.Fatalf("Pod-controller write was not denied: %T %v", object, err)
		}
	}
	f := &fixture{cfg: cfg}
	runner := Runner{Client: limited, Command: func(_ context.Context, args ...string) (string,
		error) {
		return f.answer(t, args)
	}}
	fresh := cfg
	fresh.NamespaceMode = FreshNamespace
	fresh.Namespace = "cubrid-lab-denied"
	fresh.NamespaceUID, fresh.OwnerUID, fresh.OwnerConfigMap, fresh.OwnerNonce = "", "", "", ""
	fresh.EvidenceDir = t.TempDir()
	if err := runner.Run(ctx, fresh); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("fresh mode must fail on denied namespace create: %v", err)
	}
	runner.Client = interceptor.NewClient(limited, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			cluster, ok := obj.(*databasev1alpha1.CubridCluster)
			if !ok {
				return nil
			}
			selector := &metav1.LabelSelector{MatchLabels: map[string]string{"database": clusterName}}
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: cfg.Namespace,
				OwnerReferences: []metav1.OwnerReference{owner("CubridCluster", clusterName, cluster.UID)}},
				Spec: appsv1.StatefulSetSpec{ServiceName: clusterName, Selector: selector,
					Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: selector.MatchLabels},
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: databaseContainer,
							Image: cfg.InstanceManagerImage}}}},
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: dataClaimName},
						Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}}}}}}}
			if err := admin.Create(ctx, sts); err != nil {
				return err
			}
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: smokePVCName,
				Namespace: cfg.Namespace}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &cfg.StorageClass,
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}}}}
			if err := admin.Create(ctx, claim); err != nil {
				return err
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-0", Namespace: cfg.Namespace,
				OwnerReferences: []metav1.OwnerReference{owner("StatefulSet", sts.Name, sts.UID)}},
				Spec: corev1.PodSpec{NodeName: "test-node", Containers: []corev1.Container{{Name: databaseContainer,
					Image: cfg.InstanceManagerImage}}, Volumes: []corev1.Volume{{Name: dataClaimName,
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}}}
			if err := admin.Create(ctx, pod); err != nil {
				return err
			}
			setAPIPodReady(pod, databaseContainer)
			if err := admin.Status().Update(ctx, pod); err != nil {
				return err
			}
			cluster.Status.ObservedGeneration = cluster.Generation
			cluster.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue,
				Reason: "TestFixture", Message: "API fixture", LastTransitionTime: metav1.Now()}}
			return admin.Status().Update(ctx, cluster)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := c.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			for _, child := range []client.Object{&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: clusterName,
				Namespace: cfg.Namespace}}, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-0",
				Namespace: cfg.Namespace}}} {
				if err := admin.Delete(ctx, child, client.GracePeriodSeconds(0)); err != nil {
					return err
				}
			}
			return nil
		},
	})
	cfg.Timeout = 10 * time.Second
	if err := runner.Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !readSummary(t, cfg.EvidenceDir).Passed {
		t.Fatal("confined component path did not pass")
	}
	retained := &corev1.PersistentVolumeClaim{}
	must(admin.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: smokePVCName}, retained))
	wrong := retained.UID
	wrong += "-wrong"
	if err := admin.Delete(ctx, retained,
		&client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &wrong}}); !apierrors.IsConflict(err) {
		t.Fatalf("PVC replacement precondition was not enforced: %v", err)
	}
	if err := limited.Delete(ctx, retained); !apierrors.IsForbidden(err) {
		t.Fatalf("confined identity acquired PVC deletion: %v", err)
	}
	must(admin.Get(ctx, client.ObjectKeyFromObject(ns), &corev1.Namespace{}))
}

func setAPIPodReady(pod *corev1.Pod, container string) {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: container,
		Image: pod.Spec.Containers[0].Image, ImageID: "test-runtime://sha256:" + strings.Repeat("d", 64),
		ContainerID: "test-runtime://" + container, Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
}
