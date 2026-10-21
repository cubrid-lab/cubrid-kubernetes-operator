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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testManagerName    = "manager"
	testManagerPodName = "manager-pod"
	testReplacementUID = "replacement"
	testBeforeCluster  = "before-cluster"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	commit := strings.Repeat("a", 40)
	return Config{
		Namespace: "cubrid-lab-smoke", OperatorNamespace: "operator", OperatorDeployment: testManagerName,
		OperatorImage:        "example.com/manager:main-" + commit,
		InstanceManagerImage: "example.com/instance-manager:main-" + commit, Commit: commit,
		StorageClass: "test-storage", EvidenceDir: t.TempDir(), Level: "unit",
		ClusterUID: "selected-lab",
		Timeout:    time.Second, CleanupTimeout: time.Second, Poll: time.Millisecond,
	}
}

func owner(kind, name string, uid types.UID) metav1.OwnerReference {
	yes := true
	apiVersion := "apps/v1"
	if kind == "CubridCluster" {
		apiVersion = "database.cubrid.io/v1alpha1"
	}
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: &yes}
}

func testClient(t *testing.T, cfg Config, hooks interceptor.Funcs, extra ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, storagev1.AddToScheme, databasev1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects := make([]client.Object, 0, 5+len(extra))
	objects = append(objects,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testSystemNamespace, UID: types.UID(cfg.ClusterUID)}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: cfg.OperatorDeployment, Namespace: cfg.OperatorNamespace,
			UID: "deployment", Generation: 1},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{testAppLabel: testManagerName}},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: testManagerName, Image: cfg.OperatorImage}}}}},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1,
				ReadyReplicas: 1, AvailableReplicas: 1}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "manager-rs", Namespace: cfg.OperatorNamespace,
			UID:             "replicaset",
			OwnerReferences: []metav1.OwnerReference{owner("Deployment", cfg.OperatorDeployment, "deployment")}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testManagerPodName, Namespace: cfg.OperatorNamespace,
			UID: testManagerPodName, Labels: map[string]string{testAppLabel: testManagerName},
			OwnerReferences: []metav1.OwnerReference{owner("ReplicaSet", "manager-rs", "replicaset")}},
			Spec: corev1.PodSpec{NodeName: "node-1",
				Containers: []corev1.Container{{Name: testManagerName, Image: cfg.OperatorImage}}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: testManagerName, Ready: true,
					ContainerID: "containerd://manager-container",
					State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
					ImageID:     "containerd://sha256:" + strings.Repeat("b", 64)}}}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: cfg.StorageClass}, Provisioner: "example.csi"},
	)
	objects = append(objects, extra...)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(hooks).Build()
}

func readSummary(t *testing.T, dir string) evidence.Summary {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary evidence.Summary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	return summary
}

func TestRunDoesNotOwnAnExistingNamespace(t *testing.T) {
	cfg := testConfig(t)
	deleted := false
	c := testClient(t, cfg, interceptor.Funcs{Delete: func(
		context.Context, client.WithWatch, client.Object, ...client.DeleteOption,
	) error {
		deleted = true
		return nil
	}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace, UID: "someone-else"}})
	r := Runner{Client: c, Command: func(context.Context, ...string) (string, error) {
		return `{"serverVersion":{"gitVersion":"v1.37.0"}}`, nil
	}}
	if err := r.Run(t.Context(), cfg); err == nil {
		t.Fatal("an existing namespace must refuse the smoke")
	}
	if deleted {
		t.Fatal("an existing namespace was deleted")
	}
	if got := readSummary(t, cfg.EvidenceDir); got.Passed || got.Lane != "lab-installed-smoke" || len(got.Scenarios) != 1 {
		t.Fatalf("refusal did not retain a failed planned smoke: %+v", got)
	}
}

type fixture struct {
	cfg         Config
	runner      Runner
	writes      []string
	deletes     int
	marker      string
	commandHook func(context.Context, []string) (string, error)
	deleteHook  func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newConfiguredFixture(t, testConfig(t))
}

func newConfiguredFixture(t *testing.T, cfg Config, extra ...client.Object) *fixture {
	t.Helper()
	f := &fixture{cfg: cfg}
	hooks := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			switch value := obj.(type) {
			case *corev1.Namespace:
				if f.cfg.NamespaceMode == PreallocatedNamespace {
					return errors.New("namespace writes denied")
				}
				if value.Name != f.cfg.Namespace {
					t.Fatalf("runner wrote shared namespace %s", value.Name)
				}
				value.UID = "run-namespace"
				f.writes = append(f.writes, testNamespaceAction)
				return c.Create(ctx, value, opts...)
			case *databasev1alpha1.CubridCluster:
				if value.Namespace != f.cfg.Namespace || value.Name != clusterName {
					t.Fatalf("runner wrote a cluster outside its namespace: %v", client.ObjectKeyFromObject(value))
				}
				value.UID = "smoke-cluster"
				value.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
				f.writes = append(f.writes, testClusterAction)
				if err := c.Create(ctx, value, opts...); err != nil {
					return err
				}
				// Simulated reconciliation, separate from the runner's writes.
				sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: f.cfg.Namespace,
					UID:             "database-sts",
					OwnerReferences: []metav1.OwnerReference{owner("CubridCluster", clusterName, value.UID)}}}
				if err := c.Create(ctx, sts); err != nil {
					return err
				}
				if f.cfg.NamespaceMode == PreallocatedNamespace {
					sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
						ObjectMeta: metav1.ObjectMeta{Name: dataClaimName}}}
					if err := c.Update(ctx, sts); err != nil {
						return err
					}
					claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: smokePVCName,
						Namespace: f.cfg.Namespace, UID: "claim-uid"},
						Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &f.cfg.StorageClass, VolumeName: "pv-smoke",
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}}}}
					if err := c.Create(ctx, claim); err != nil {
						return err
					}
				}
				return c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-0", Namespace: f.cfg.Namespace,
					UID:             "database-pod",
					OwnerReferences: []metav1.OwnerReference{owner("StatefulSet", clusterName, sts.UID)}},
					Spec: corev1.PodSpec{NodeName: "node-1", Volumes: []corev1.Volume{{Name: dataClaimName,
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: smokePVCName}}}},
						Containers: []corev1.Container{{Name: databaseContainer, Image: f.cfg.InstanceManagerImage}}},
					Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
						ContainerStatuses: []corev1.ContainerStatus{{Name: databaseContainer, Ready: true,
							ContainerID: "containerd://database-container",
							State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
							ImageID:     "containerd://sha256:" + strings.Repeat("c", 64)}}}})
			default:
				t.Fatalf("runner attempted a forbidden write: %T", obj)
				return errors.New("forbidden write")
			}
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			f.deletes++
			if f.cfg.NamespaceMode == PreallocatedNamespace {
				cluster, ok := obj.(*databasev1alpha1.CubridCluster)
				if !ok {
					return fmt.Errorf("namespace/PVC deletion denied: %T", obj)
				}
				options := (&client.DeleteOptions{}).ApplyOptions(opts)
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != cluster.UID {
					return errors.New("missing CR UID precondition")
				}
				if f.deleteHook != nil {
					return f.deleteHook(ctx, c, obj, opts...)
				}
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				// Fake-client GC only; real GC is required in isolated Kind.
				for _, child := range []client.Object{&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: clusterName,
					Namespace: f.cfg.Namespace}}, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-0",
					Namespace: f.cfg.Namespace}}} {
					if err := c.Delete(ctx, child); err != nil {
						return err
					}
				}
				return nil
			}
			if _, ok := obj.(*corev1.Namespace); !ok || obj.GetName() != f.cfg.Namespace {
				t.Fatalf("runner deleted a shared object: %T %v", obj, client.ObjectKeyFromObject(obj))
			}
			options := (&client.DeleteOptions{}).ApplyOptions(opts)
			if options.Preconditions == nil || options.Preconditions.UID == nil ||
				*options.Preconditions.UID != "run-namespace" {
				t.Fatal("namespace delete did not carry the saved UID precondition")
			}
			if ctx.Err() != nil {
				t.Fatal("cleanup inherited the cancelled run context")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("cleanup has no independent deadline")
			}
			if f.deleteHook != nil {
				return f.deleteHook(ctx, c, obj, opts...)
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
	f.runner.Client = testClient(t, f.cfg, hooks, extra...)
	f.runner.Command = func(ctx context.Context, args ...string) (string, error) {
		if f.commandHook != nil {
			return f.commandHook(ctx, args)
		}
		return f.answer(t, args)
	}
	return f
}

func (f *fixture) answer(t *testing.T, args []string) (string, error) {
	t.Helper()
	if len(args) == 3 && args[0] == "version" {
		return `{"serverVersion":{"gitVersion":"v1.37.0"}}`, nil
	}
	if len(args) < 8 || args[0] != "-n" || args[1] != f.cfg.Namespace || args[2] != "exec" ||
		args[3] != clusterName+"-0" || args[4] != "-c" || args[5] != databaseContainer || args[6] != "--" {
		t.Fatalf("command escaped the owned database container: %q", args)
	}
	if args[7] == "id" {
		return "1000\n", nil
	}
	if strings.Contains(args[len(args)-1], "cubrid_rel") {
		return "CUBRID 11.4 (11.4.6.1963-0e7d3c1)", nil
	}
	statement := args[len(args)-1]
	if strings.HasPrefix(statement, "CREATE TABLE") {
		match := regexp.MustCompile(`VALUES \(1, '([^']+)'\)`).FindStringSubmatch(statement)
		if len(match) != 2 {
			t.Fatalf("unexpected SQL write: %s", statement)
		}
		f.marker = match[1]
		return "COMMIT WORK", nil
	}
	return fmt.Sprintf("'%s'\n1 row selected", f.marker), nil
}

func TestRunWritesSQLAndRetainsItsScopedEvidence(t *testing.T) {
	f := newFixture(t)
	if err := f.runner.Run(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.writes, ",") != "namespace,cluster" || f.deletes != 1 || f.marker == "" {
		t.Fatalf("unexpected actions: writes=%v deletes=%d marker=%s", f.writes, f.deletes, f.marker)
	}
	summary := readSummary(t, f.cfg.EvidenceDir)
	if !summary.Passed || summary.Lane != laneName || len(summary.Scenarios) != 1 ||
		summary.Scenarios[0].CleanupSucceeded == nil || !*summary.Scenarios[0].CleanupSucceeded {
		t.Fatalf("successful SQL and cleanup were not recorded: %+v", summary)
	}
	if summary.Environment.OperatorImageDigest == "" || summary.Environment.EngineVersion != "11.4.6.1963" {
		t.Fatalf("runtime identity was not observed: %+v", summary.Environment)
	}
}

func TestRunRefusesWrongInstalledIdentityBeforeWrites(t *testing.T) {
	for _, change := range []string{testClusterAction, "template", "pod-image", "image-id", testOwnerName} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			switch change {
			case testClusterAction:
				f.cfg.ClusterUID = "another-cluster"
			case "template":
				deployment := &appsv1.Deployment{}
				key := client.ObjectKey{Namespace: f.cfg.OperatorNamespace, Name: f.cfg.OperatorDeployment}
				if err := f.runner.Client.Get(t.Context(), key, deployment); err != nil {
					t.Fatal(err)
				}
				deployment.Spec.Template.Spec.Containers[0].Image = "wrong-image"
				if err := f.runner.Client.Update(t.Context(), deployment); err != nil {
					t.Fatal(err)
				}
			default:
				pod := &corev1.Pod{}
				key := client.ObjectKey{Namespace: f.cfg.OperatorNamespace, Name: testManagerPodName}
				if err := f.runner.Client.Get(t.Context(), key, pod); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "pod-image":
					pod.Spec.Containers[0].Image = "wrong-image"
				case "image-id":
					pod.Status.ContainerStatuses[0].ImageID = ""
					if err := f.runner.Client.Status().Update(t.Context(), pod); err != nil {
						t.Fatal(err)
					}
				default:
					pod.OwnerReferences = nil
				}
				if err := f.runner.Client.Update(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("wrong installed identity passed")
			}
			if len(f.writes) != 0 || f.deletes != 0 || readSummary(t, f.cfg.EvidenceDir).Passed {
				t.Fatalf("unverified installation caused mutation: %v/%d", f.writes, f.deletes)
			}
		})
	}
}

func TestRunFailsOnSQLOrCleanupErrors(t *testing.T) {
	for _, failure := range []string{"sql-output", "sql-command", "missing-row", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			f.commandHook = func(_ context.Context, args []string) (string, error) {
				last := args[len(args)-1]
				if strings.HasPrefix(last, "CREATE TABLE") {
					if failure == "sql-output" {
						return "ERROR: SQL did not execute", nil
					}
					if failure == "sql-command" {
						return "", errors.New("command failed")
					}
				}
				if failure == "missing-row" && strings.HasPrefix(last, "SELECT") {
					return "0 rows selected", nil
				}
				return f.answer(t, args)
			}
			if failure == "cleanup" {
				f.deleteHook = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					return errors.New("cleanup refused")
				}
			}
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("failed SQL or cleanup passed")
			}
			if f.deletes != 1 || readSummary(t, f.cfg.EvidenceDir).Passed {
				t.Fatalf("failure did not preserve cleanup/verdict: deletes=%d", f.deletes)
			}
		})
	}
}

func TestRunCleansUpAfterCancellation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.commandHook = func(ctx context.Context, args []string) (string, error) {
		if strings.HasPrefix(args[len(args)-1], "CREATE TABLE") {
			cancel()
			<-ctx.Done()
			return "", ctx.Err()
		}
		return f.answer(t, args)
	}
	if err := f.runner.Run(ctx, f.cfg); err == nil {
		t.Fatal("cancelled SQL passed")
	}
	if f.deletes != 1 || readSummary(t, f.cfg.EvidenceDir).Passed {
		t.Fatal("cancelled run did not clean up or retained a pass")
	}
}

func TestRunDoesNotClaimFailedOrUnidentifiedNamespaceCreation(t *testing.T) {
	for _, failure := range []string{"denied", testAmbiguous, "missing-uid"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			f.runner.Client = interceptor.NewClient(f.runner.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*corev1.Namespace); ok {
						switch failure {
						case "denied":
							return errors.New("namespace creation denied")
						case testAmbiguous:
							return context.DeadlineExceeded
						default:
							return nil
						}
					}
					return c.Create(ctx, obj, opts...)
				},
			})
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("unverified namespace creation passed")
			}
			if f.deletes != 0 || len(f.writes) != 0 || readSummary(t, f.cfg.EvidenceDir).Passed {
				t.Fatal("unverified creation granted cleanup or more writes")
			}
		})
	}
}

func TestRunLeavesAReplacementNamespace(t *testing.T) {
	f := newFixture(t)
	f.commandHook = func(_ context.Context, args []string) (string, error) {
		out, err := f.answer(t, args)
		if strings.HasPrefix(args[len(args)-1], "SELECT") {
			// Model a different namespace occupying this name before cleanup.
			ns := &corev1.Namespace{}
			if err := f.runner.Client.Get(t.Context(), client.ObjectKey{Name: f.cfg.Namespace}, ns); err != nil {
				t.Fatal(err)
			}
			ns.UID = testReplacementUID
			if err := f.runner.Client.Update(t.Context(), ns); err != nil {
				t.Fatal(err)
			}
		}
		return out, err
	}
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("replacement namespace was considered cleaned")
	}
	if f.deletes != 0 || readSummary(t, f.cfg.EvidenceDir).Passed {
		t.Fatal("replacement namespace was deleted or the run passed")
	}
}

func TestRunDoesNotFallbackAfterDeleteConflict(t *testing.T) {
	f := newFixture(t)
	f.deleteHook = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		return errors.New("UID precondition conflict")
	}
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("UID conflict passed")
	}
	if f.deletes != 1 || readSummary(t, f.cfg.EvidenceDir).Passed {
		t.Fatal("UID conflict was retried by name or reported as a pass")
	}
}

func TestRunCannotPassWithoutFinalEvidence(t *testing.T) {
	f := newFixture(t)
	f.commandHook = func(_ context.Context, args []string) (string, error) {
		out, err := f.answer(t, args)
		if strings.HasPrefix(args[len(args)-1], "SELECT") {
			path := filepath.Join(f.cfg.EvidenceDir, scenarioID)
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("evidence path blocked"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return out, err
	}
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("SQL without final evidence passed")
	}
	if f.deletes != 1 || readSummary(t, f.cfg.EvidenceDir).Passed {
		t.Fatal("evidence failure prevented cleanup or retained a pass")
	}
}

func TestRunRedactsSQLFailureEvidenceAndErrors(t *testing.T) {
	const secret = "private-token-value"
	f := newFixture(t)
	f.runner.Redactor = evidence.NewRedactor(secret)
	f.commandHook = func(_ context.Context, args []string) (string, error) {
		if strings.HasPrefix(args[len(args)-1], "CREATE TABLE") {
			return "ERROR: " + secret, errors.New(secret)
		}
		return f.answer(t, args)
	}
	err := f.runner.Run(t.Context(), f.cfg)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("failure was missing or exposed credentials: %v", err)
	}
	for _, name := range []string{"summary.json", "junit.xml", recordFile} {
		data, err := os.ReadFile(filepath.Join(f.cfg.EvidenceDir, name))
		if err != nil || strings.Contains(string(data), secret) {
			t.Errorf("evidence %s leaked credentials or was missing: %v", name, err)
		}
	}
}

func TestNamespaceReplacementStopsFurtherWork(t *testing.T) {
	for _, when := range []string{testBeforeCluster, "before-sql"} {
		t.Run(when, func(t *testing.T) {
			f := newFixture(t)
			if when == testBeforeCluster {
				f.runner.Client = interceptor.NewClient(f.runner.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
						obj client.Object, opts ...client.GetOption) error {
						if err := c.Get(ctx, key, obj, opts...); err != nil {
							return err
						}
						if ns, ok := obj.(*corev1.Namespace); ok && ns.Name == f.cfg.Namespace {
							ns.UID = testReplacementUID
						}
						return nil
					},
				})
			} else {
				f.commandHook = func(_ context.Context, args []string) (string, error) {
					out, err := f.answer(t, args)
					if strings.Contains(args[len(args)-1], "cubrid_rel") {
						ns := &corev1.Namespace{}
						if err := f.runner.Client.Get(t.Context(), client.ObjectKey{Name: f.cfg.Namespace}, ns); err != nil {
							t.Fatal(err)
						}
						ns.UID = testReplacementUID
						if err := f.runner.Client.Update(t.Context(), ns); err != nil {
							t.Fatal(err)
						}
					}
					return out, err
				}
			}
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("replacement namespace passed")
			}
			if f.marker != "" || f.deletes != 0 {
				t.Fatalf("replacement namespace received SQL or deletion: marker=%s deletes=%d", f.marker, f.deletes)
			}
			if when == testBeforeCluster && strings.Join(f.writes, ",") != testNamespaceAction {
				t.Fatalf("cluster was created after replacement: %v", f.writes)
			}
		})
	}
}

func TestRunFailsWhenTheDatabaseContainerRestartsDuringSQL(t *testing.T) {
	f := newFixture(t)
	f.commandHook = func(_ context.Context, args []string) (string, error) {
		out, err := f.answer(t, args)
		if strings.HasPrefix(args[len(args)-1], "SELECT") {
			pod := &corev1.Pod{}
			key := client.ObjectKey{Namespace: f.cfg.Namespace, Name: clusterName + "-0"}
			if err := f.runner.Client.Get(t.Context(), key, pod); err != nil {
				t.Fatal(err)
			}
			pod.Status.ContainerStatuses[0].ContainerID = "restarted-container"
			if err := f.runner.Client.Status().Update(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
		}
		return out, err
	}
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("changed database runtime identity passed")
	}
	if f.deletes != 1 || readSummary(t, f.cfg.EvidenceDir).Passed {
		t.Fatal("changed container identity did not clean up and fail")
	}
}

func TestPinnedImageReferencesExcludeCredentials(t *testing.T) {
	commit, digest := strings.Repeat("a", 40), strings.Repeat("b", 64)
	for _, image := range []string{
		"user:password@repo@sha256:" + digest,
		"user:password@repo:main-" + commit,
		"repo:tag@sha256:" + digest,
		"repo:dev",
	} {
		if validPinnedImage(image, commit) {
			t.Errorf("unsupported or credential-bearing image was accepted: %s", image)
		}
	}
	if !validPinnedImage("registry.example:5000/team/manager@sha256:"+digest, commit) {
		t.Fatal("valid digest-pinned image with a registry port was refused")
	}
}

func TestRuntimeImageIDIsRecordedWithoutManifestDigestAssumptions(t *testing.T) {
	cfg := testConfig(t)
	c := testClient(t, cfg, interceptor.Funcs{})
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: testManagerPodName}
	if err := c.Get(t.Context(), key, pod); err != nil {
		t.Fatal(err)
	}
	expected := "example.com/manager@sha256:" + strings.Repeat("d", 64)
	pod.Spec.Containers[0].Image = expected
	identity, err := podIdentity(pod, testManagerName, expected)
	if err != nil || identity.ImageID != pod.Status.ContainerStatuses[0].ImageID {
		t.Fatalf("runtime configuration ID was confused with a repository manifest digest: %+v, %v", identity, err)
	}
}
