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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testSystemNamespace = "kube-system"
	testAppLabel        = "app"
	testVerbGet         = "get"
	testVerbList        = "list"
	testClusterResource = "cubridclusters"
	testBorrowedUID     = "borrowed"
	testClusterAction   = "cluster"
	testNamespaceAction = "namespace"
	testMissingOwner    = "missing-owner"
	testOtherUID        = "other"
	testForeignUID      = "foreign"
	testAmbiguous       = "ambiguous"
	testAmbiguousOwner  = "owner-ambiguity"
	testPodKind         = "pod"
	testPVCKind         = "pvc"
	testBeforeCleanup   = "before-cleanup"
)

const (
	testOwnerName        = "owner"
	testMutableOwner     = "mutable-owner"
	testTerminatingOwner = "terminating-owner"
	testAPIGroup         = "database.cubrid.io"
	testStatefulSetKind  = "statefulset"
	testVolumeKind       = "volume"
	testBeforeRead       = "before-read"
)

const (
	testForeignDeployment  = "foreign-deployment"
	testForeignReplicaSet  = "foreign-replicaset"
	testForeignPod         = "foreign-pod"
	testPVCMount           = "pvc-mount"
	testHostPath           = "hostpath-mount"
	testExtraDeployment    = "extra-deployment"
	testReplacedDeployment = "replaced-deployment"
	testReplacedReplicaSet = "replaced-replicaset"
	testReplacedPod        = "replaced-pod"
)

const (
	testOwnerUID             = "owner-uid"
	testTerminatingNamespace = "terminating-namespace"
	testBeforeWrite          = "before-write"
)

func preallocatedFixture(t *testing.T, extra ...client.Object) *fixture {
	t.Helper()
	cfg := testConfig(t)
	cfg.NamespaceMode = PreallocatedNamespace
	cfg.NamespaceUID, cfg.OwnerConfigMap, cfg.OwnerUID, cfg.OwnerNonce = testBorrowedUID, testOwnerName, testOwnerUID,
		ownerNonceKey
	return newPreallocatedFixture(t, cfg, extra...)
}

func newPreallocatedFixture(t *testing.T, cfg Config, extra ...client.Object) *fixture {
	t.Helper()
	immutable := true
	objects := make([]client.Object, 0, 2+len(extra))
	objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace, UID: testBorrowedUID}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testOwnerName, Namespace: cfg.Namespace, UID: testOwnerUID},
			Immutable: &immutable, Data: map[string]string{ownerNonceKey: ownerNonceKey}})
	return newConfiguredFixture(t, cfg, append(objects, extra...)...)
}

func TestPreallocatedRunNeverWritesNamespace(t *testing.T) {
	f := preallocatedFixture(t)
	if err := f.runner.Run(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.writes, ",") != testClusterAction || f.deletes != 1 || f.marker == "" {
		t.Fatalf("unexpected actions %v/%d/%s", f.writes, f.deletes, f.marker)
	}
	summary := readSummary(t, f.cfg.EvidenceDir)
	if !summary.Passed || summary.Lane != preallocatedLane || summary.Scenarios[0].Variant != "preallocated" {
		t.Fatalf("wrong component verdict %+v", summary)
	}
	rec := readRecord(t, f.cfg.EvidenceDir)
	if rec.NamespaceMode != PreallocatedNamespace || rec.CleanupScope != ownedWorkloadsScope ||
		rec.FixtureReusable == nil || *rec.FixtureReusable || rec.ResetRequired == nil || !*rec.ResetRequired ||
		len(rec.RetainedPVCs) != 1 || rec.RetainedPVCs[0].UID != "claim-uid" {
		t.Fatalf("fixture reset was overstated: %+v", rec)
	}
	for _, obj := range []client.Object{&corev1.Namespace{}, &corev1.PersistentVolumeClaim{}} {
		key := client.ObjectKey{Name: f.cfg.Namespace}
		if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
			key = client.ObjectKey{Name: smokePVCName, Namespace: f.cfg.Namespace}
		}
		if err := f.runner.Client.Get(t.Context(), key, obj); err != nil {
			t.Fatal(err)
		}
	}
}

func readRecord(t *testing.T, dir string) record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		t.Fatal(err)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestPreallocatedInputsAndEpochRefuseBeforeMutation(t *testing.T) {
	for _, failure := range []string{"namespace-input", "owner-name", "owner-input", "nonce-input",
		"wrong-namespace", "wrong-owner", "wrong-nonce", testMutableOwner, testTerminatingNamespace,
		testTerminatingOwner, testMissingOwner} {
		t.Run(failure, func(t *testing.T) {
			f := preallocatedFixture(t)
			switch failure {
			case "namespace-input":
				f.cfg.NamespaceUID = ""
			case "owner-name":
				f.cfg.OwnerConfigMap = ""
			case "owner-input":
				f.cfg.OwnerUID = ""
			case "nonce-input":
				f.cfg.OwnerNonce = ""
			case "wrong-namespace":
				f.cfg.NamespaceUID = testOtherUID
			case "wrong-owner":
				f.cfg.OwnerUID = testOtherUID
			case "wrong-nonce":
				f.cfg.OwnerNonce = testOtherUID
			case testMutableOwner, testMissingOwner:
				marker := &corev1.ConfigMap{}
				if err := f.runner.Client.Get(t.Context(), client.ObjectKey{Namespace: f.cfg.Namespace,
					Name: f.cfg.OwnerConfigMap}, marker); err != nil {
					t.Fatal(err)
				}
				switch failure {
				case testMutableOwner:
					no := false
					marker.Immutable = &no
				case testTerminatingOwner:
					now := metav1.Now()
					marker.DeletionTimestamp = &now
					marker.Finalizers = []string{"external-owner"}
				default:
					// Simulate missing marker without granting the runner delete authority.
					f.cfg.OwnerConfigMap = "missing"
				}
				if failure != testMissingOwner {
					if err := f.runner.Client.Update(t.Context(), marker); err != nil {
						t.Fatal(err)
					}
				}
			case testTerminatingNamespace, testTerminatingOwner:
				f.runner.Client = interceptor.NewClient(f.runner.Client.(client.WithWatch),
					interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
						opts ...client.GetOption) error {
						if err := c.Get(ctx, key, obj, opts...); err != nil {
							return err
						}
						if (failure == testTerminatingNamespace && key.Name == f.cfg.Namespace && key.Namespace == "") ||
							(failure == testTerminatingOwner && key.Name == f.cfg.OwnerConfigMap && key.Namespace == f.cfg.Namespace) {
							now := metav1.Now()
							obj.SetDeletionTimestamp(&now)
						}
						return nil
					}})
			}
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("unverified ownership passed")
			}
			if len(f.writes) != 0 || f.deletes != 0 || f.marker != "" {
				t.Fatalf("unverified epoch granted authority: %v/%d/%s", f.writes, f.deletes, f.marker)
			}
		})
	}
}

func TestPreallocatedExistingWorkloadRefusesBeforeCreate(t *testing.T) {
	ns := "cubrid-lab-smoke"
	for _, obj := range []client.Object{
		&databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: testForeignUID, Namespace: ns,
			UID: testForeignUID}},
		&databasev1alpha1.CubridBackup{ObjectMeta: metav1.ObjectMeta{Name: testForeignUID, Namespace: ns,
			UID: testForeignUID}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: testForeignUID, Namespace: ns, UID: testForeignUID}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: smokePVCName, Namespace: ns, UID: testForeignUID}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "smoke-0", Namespace: ns, UID: testForeignUID}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testForeignUID, Namespace: ns, UID: testForeignUID}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: testForeignUID, Namespace: ns, UID: testForeignUID}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: testForeignUID, Namespace: ns, UID: testForeignUID}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: testForeignUID, Namespace: ns, UID: testForeignUID}},
	} {
		t.Run(fmt.Sprintf("%T", obj), func(t *testing.T) {
			f := preallocatedFixture(t, obj)
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("existing workload was adopted")
			}
			if len(f.writes) != 0 || f.deletes != 0 || f.marker != "" {
				t.Fatal("existing workload granted mutation/cleanup")
			}
		})
	}
}

// A dormant built-in Pod controller is a future writer even with no Pods yet.
func TestPreallocatedDormantControllersRefuseBeforeMutation(t *testing.T) {
	ns := testConfig(t).Namespace
	for _, object := range []client.Object{
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: testForeignUID, Namespace: ns, UID: testForeignUID}},
		&corev1.ReplicationController{ObjectMeta: metav1.ObjectMeta{Name: testForeignUID, Namespace: ns, UID: testForeignUID},
			Spec: corev1.ReplicationControllerSpec{Replicas: new(int32(0))}},
	} {
		t.Run(fmt.Sprintf("%T", object), func(t *testing.T) {
			f := preallocatedFixture(t, object)
			sqlCalls := 0
			f.commandHook = func(_ context.Context, args []string) (string, error) {
				last := args[len(args)-1]
				if strings.HasPrefix(last, "CREATE TABLE") || strings.HasPrefix(last, "SELECT") {
					sqlCalls++
				}
				return f.answer(t, args)
			}
			err := f.runner.Run(t.Context(), f.cfg)
			if err == nil {
				t.Error("dormant Pod controller was not rejected")
			}
			if len(f.writes) != 0 || f.deletes != 0 || sqlCalls != 0 {
				t.Errorf("dormant controller granted authority: writes=%v deletes=%d SQL=%d", f.writes, f.deletes, sqlCalls)
			}
		})
	}
}

func TestPreallocatedFailedCreatesGrantNoCleanup(t *testing.T) {
	for _, failure := range []string{"already-exists", testAmbiguous, "missing-uid"} {
		t.Run(failure, func(t *testing.T) {
			f := preallocatedFixture(t)
			f.runner.Client = interceptor.NewClient(f.runner.Client.(client.WithWatch),
				interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
					opts ...client.CreateOption) error {
					if _, ok := obj.(*databasev1alpha1.CubridCluster); !ok {
						return c.Create(ctx, obj, opts...)
					}
					switch failure {
					case "already-exists":
						return apierrors.NewAlreadyExists(schema.GroupResource{Group: testAPIGroup,
							Resource: testClusterResource}, obj.GetName())
					case testAmbiguous:
						if err := c.Create(ctx, obj, opts...); err != nil {
							return err
						}
						return context.DeadlineExceeded
					default:
						return nil
					}
				}})
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("failed/unidentified create passed")
			}
			if f.deletes != 0 || f.marker != "" {
				t.Fatal("failed/unidentified create granted SQL or cleanup authority")
			}
		})
	}
}

func replaceObserved(t *testing.T, f *fixture, kind string) {
	t.Helper()
	var obj client.Object
	key := client.ObjectKey{Namespace: f.cfg.Namespace}
	switch kind {
	case testNamespaceAction:
		obj = &corev1.Namespace{}
		key = client.ObjectKey{Name: f.cfg.Namespace}
	case testOwnerName, ownerNonceKey:
		obj = &corev1.ConfigMap{}
		key.Name = f.cfg.OwnerConfigMap
	case testClusterAction:
		obj = &databasev1alpha1.CubridCluster{}
		key.Name = clusterName
	case testStatefulSetKind, testAmbiguousOwner:
		obj = &appsv1.StatefulSet{}
		key.Name = clusterName
	case testPodKind:
		obj = &corev1.Pod{}
		key.Name = clusterName + "-0"
	case testPVCKind, testVolumeKind:
		obj = &corev1.PersistentVolumeClaim{}
		key.Name = smokePVCName
	default:
		t.Fatalf("unknown replacement %s", kind)
	}
	if err := f.runner.Client.Get(t.Context(), key, obj); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case ownerNonceKey:
		obj.(*corev1.ConfigMap).Data[ownerNonceKey] = "replacement"
	case testVolumeKind:
		obj.(*corev1.PersistentVolumeClaim).Spec.VolumeName = "different-volume"
	case testAmbiguousOwner:
		obj.SetOwnerReferences(append(obj.GetOwnerReferences(), owner("Other", testForeignUID, testForeignUID)))
	default:
		obj.SetUID(types.UID(testReplacementUID))
	}
	if err := f.runner.Client.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
}

func TestPreallocatedEpochAndWorkloadReplacementStopsSQLAndCleanup(t *testing.T) {
	for _, phase := range []string{testBeforeWrite, testBeforeRead, testBeforeCleanup} {
		for _, kind := range []string{testNamespaceAction, testOwnerName, ownerNonceKey,
			testClusterAction, testStatefulSetKind,
			testAmbiguousOwner, testPodKind, testPVCKind, testVolumeKind} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				f := preallocatedFixture(t)
				executedSQL := 0
				f.commandHook = func(_ context.Context, args []string) (string, error) {
					last := args[len(args)-1]
					if strings.HasPrefix(last, "CREATE TABLE") || strings.HasPrefix(last, "SELECT") {
						executedSQL++
					}
					out, err := f.answer(t, args)
					if (phase == testBeforeWrite && strings.Contains(last, "cubrid_rel")) ||
						(phase == testBeforeRead && strings.HasPrefix(last, "CREATE TABLE")) ||
						(phase == testBeforeCleanup && strings.HasPrefix(last, "SELECT")) {
						replaceObserved(t, f, kind)
					}
					return out, err
				}
				if err := f.runner.Run(t.Context(), f.cfg); err == nil {
					t.Fatal("replacement passed")
				}
				if f.deletes != 0 {
					t.Fatal("ambiguity granted CR cleanup authority")
				}
				rec := readRecord(t, f.cfg.EvidenceDir)
				want := map[string]int{testBeforeWrite: 0, testBeforeRead: 1, testBeforeCleanup: 2}[phase]
				if executedSQL != want {
					t.Fatalf("replacement did not stop next SQL exec: %d want %d", executedSQL, want)
				}
				if phase != testBeforeCleanup && (len(rec.SQL) != want+1 || rec.SQL[len(rec.SQL)-1].Error == "") {
					t.Fatal("blocked SQL attempt did not retain its guard error")
				}
			})
		}
	}
}

func TestPreallocatedCleanupWaitsForStoppedWorkloads(t *testing.T) {
	f := preallocatedFixture(t)
	f.cfg.CleanupTimeout = 30 * time.Millisecond
	f.deleteHook = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		return c.Delete(ctx, obj, opts...) // Deliberately no simulated GC.
	}
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("CR deletion alone was reported as stopped writers")
	}
	summary := readSummary(t, f.cfg.EvidenceDir)
	if summary.Passed || summary.Scenarios[0].CleanupSucceeded == nil || *summary.Scenarios[0].CleanupSucceeded {
		t.Fatal("incomplete GC passed")
	}
}

func TestPreallocatedCleanupPreservesReplacementDuringGC(t *testing.T) {
	for _, kind := range []string{testClusterAction, testStatefulSetKind, testPodKind, testPVCKind} {
		t.Run(kind, func(t *testing.T) {
			f := preallocatedFixture(t)
			f.deleteHook = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				// The CR delete was issued with its original UID; race in an observed replacement.
				replaceObserved(t, f, kind)
				return apierrors.NewConflict(schema.GroupResource{Resource: testClusterResource}, clusterName,
					errors.New("UID conflict"))
			}
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("replacement during deletion passed")
			}
			if f.deletes != 1 {
				t.Fatalf("delete retried by name: %d", f.deletes)
			}
		})
	}
}

func TestPreallocatedSameNamespaceCandidateAndRetainedClaimBlockReuse(t *testing.T) {
	cfg := testConfig(t)
	cfg.NamespaceMode = PreallocatedNamespace
	cfg.NamespaceUID, cfg.OwnerConfigMap, cfg.OwnerUID, cfg.OwnerNonce = testBorrowedUID, testOwnerName, testOwnerUID,
		ownerNonceKey
	cfg.OperatorNamespace = cfg.Namespace
	f := newPreallocatedFixture(t, cfg)
	if err := f.runner.Run(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg.EvidenceDir = t.TempDir()
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("retained claim allowed another fresh-data run")
	}
	if len(f.writes) != 1 || f.deletes != 1 {
		t.Fatal("retained fixture received another write/delete")
	}
}

// The Broker tier the created cluster owns is permitted; a look-alike outside
// that ownership chain, a third owned Deployment, a replacement or a Broker Pod
// with a PVC or hostPath volume is not.
func TestPreallocatedRefusesWorkloadsOutsideTheBrokerChain(t *testing.T) {
	const rw = clusterName + "-broker-rw"
	for _, kind := range []string{testForeignDeployment, testForeignReplicaSet, testForeignPod, testPVCMount,
		testHostPath, testExtraDeployment, testReplacedDeployment, testReplacedReplicaSet, testReplacedPod} {
		t.Run(kind, func(t *testing.T) {
			f := preallocatedFixture(t)
			ns := f.cfg.Namespace
			executedSQL := 0
			f.commandHook = func(_ context.Context, args []string) (string, error) {
				last := args[len(args)-1]
				if strings.HasPrefix(last, "CREATE TABLE") || strings.HasPrefix(last, "SELECT") {
					executedSQL++
				}
				if !strings.Contains(last, "cubrid_rel") {
					return f.answer(t, args)
				}
				c := f.runner.Client
				var err error
				switch kind {
				case testForeignDeployment, testForeignReplicaSet, testForeignPod:
					// A same-named object controlled outside the chain.
					var obj client.Object = &appsv1.Deployment{}
					name, ref := rw, owner("CubridCluster", clusterName, testForeignUID)
					switch kind {
					case testForeignReplicaSet:
						obj, name, ref = &appsv1.ReplicaSet{}, rw+"-rs", owner("Deployment", rw, testForeignUID)
					case testForeignPod:
						obj, name, ref = &corev1.Pod{}, rw+"-rs-a", owner("ReplicaSet", rw+"-rs", testForeignUID)
					}
					if err = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, obj); err == nil {
						obj.SetOwnerReferences([]metav1.OwnerReference{ref})
						err = c.Update(t.Context(), obj)
					}
				case testPVCMount, testHostPath:
					pod := &corev1.Pod{}
					if err = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: rw + "-rs-a"}, pod); err == nil {
						source := corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: smokePVCName}}
						if kind == testHostPath {
							source = corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/cubrid"}}
						}
						pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: dataClaimName, VolumeSource: source})
						err = c.Update(t.Context(), pod)
					}
				case testExtraDeployment:
					// Correctly owned, but not one of the two Broker Deployments.
					err = f.store.Create(t.Context(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
						Name: clusterName + "-broker-x", Namespace: ns, UID: "broker-x",
						OwnerReferences: []metav1.OwnerReference{owner("CubridCluster", clusterName, "smoke-cluster")}}})
				default:
					var obj client.Object = &appsv1.Deployment{}
					name := rw
					switch kind {
					case testReplacedReplicaSet:
						obj, name = &appsv1.ReplicaSet{}, rw+"-rs"
					case testReplacedPod:
						obj, name = &corev1.Pod{}, rw+"-rs-a"
					}
					if err = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, obj); err == nil {
						obj.SetUID(testReplacementUID)
						err = c.Update(t.Context(), obj)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				return f.answer(t, args)
			}
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("a workload outside the created cluster's Broker chain passed")
			}
			if executedSQL != 0 || f.deletes != 0 {
				t.Fatalf("unidentified workload granted SQL=%d or cleanup=%d", executedSQL, f.deletes)
			}
		})
	}
}

func TestPreallocatedCleanupWaitsForBrokerTier(t *testing.T) {
	for _, left := range []client.Object{&appsv1.Deployment{}, &appsv1.ReplicaSet{}, &corev1.Pod{}} {
		t.Run(fmt.Sprintf("%T", left), func(t *testing.T) {
			f := preallocatedFixture(t)
			f.cfg.CleanupTimeout = 30 * time.Millisecond
			f.deleteHook = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				// GC removes the database workload but leaves one Broker object.
				children := []client.Object{&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: clusterName,
					Namespace: f.cfg.Namespace}}, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-0",
					Namespace: f.cfg.Namespace}}}
				kept := false
				for _, child := range brokerTier(f.cfg.Namespace, obj.GetUID()) {
					if !kept && fmt.Sprintf("%T", child) == fmt.Sprintf("%T", left) {
						kept = true
						continue
					}
					children = append(children, child)
				}
				for _, child := range children {
					if err := c.Delete(ctx, child); err != nil {
						return err
					}
				}
				return nil
			}
			err := f.runner.Run(t.Context(), f.cfg)
			if err == nil {
				t.Fatal("a remaining Broker object was reported as stopped workloads")
			}
			if f.marker == "" || f.deletes != 1 || strings.Contains(err.Error(), "unidentified") {
				t.Fatalf("the run did not reach cleanup through the Broker tier: marker=%q deletes=%d err=%v",
					f.marker, f.deletes, err)
			}
			summary := readSummary(t, f.cfg.EvidenceDir)
			if summary.Scenarios[0].CleanupSucceeded == nil || *summary.Scenarios[0].CleanupSucceeded {
				t.Fatal("incomplete Broker GC passed cleanup")
			}
		})
	}
}

// rollBrokerOut acts as the Deployment controller of a rolling update: a new
// ReplicaSet under the pinned read-write Broker Deployment and its two Pods.
// Cleanup's simulated GC also removes them.
func rollBrokerOut(t *testing.T, f *fixture) {
	t.Helper()
	const rw = clusterName + "-broker-rw"
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: rw + "-rs2", Namespace: f.cfg.Namespace,
		UID: rw + "-rs2", OwnerReferences: []metav1.OwnerReference{owner("Deployment", rw, rw)}}}
	ns := f.cfg.Namespace
	rollout := []client.Object{rs, brokerPod(ns, rs.Name+"-a", rs), brokerPod(ns, rs.Name+"-b", rs)}
	if err := createAll(t.Context(), f.store, rollout...); err != nil {
		t.Fatal(err)
	}
	f.deleteHook = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if err := c.Delete(ctx, obj, opts...); err != nil {
			return err
		}
		children := append([]client.Object{&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: clusterName,
			Namespace: f.cfg.Namespace}}, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-0",
			Namespace: f.cfg.Namespace}}}, brokerTier(f.cfg.Namespace, obj.GetUID())...)
		for _, child := range append(children, rollout...) {
			if err := c.Delete(ctx, child); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestPreallocatedAcceptsBrokerRollingUpdate(t *testing.T) {
	f := preallocatedFixture(t)
	f.commandHook = func(_ context.Context, args []string) (string, error) {
		if strings.Contains(args[len(args)-1], "cubrid_rel") && f.deleteHook == nil {
			rollBrokerOut(t, f)
		}
		return f.answer(t, args)
	}
	if err := f.runner.Run(t.Context(), f.cfg); err != nil {
		t.Fatalf("a rolling update under the pinned Broker Deployment was refused: %v", err)
	}
	if !readSummary(t, f.cfg.EvidenceDir).Passed || f.deletes != 1 {
		t.Fatal("the run did not pass through a Broker rolling update")
	}
}

// A ReplicaSet and its Pod created between the guard's ReplicaSet and Pod
// reads must not look like an unidentified Pod.
func TestPreallocatedToleratesBrokerCreatedBetweenReads(t *testing.T) {
	f := preallocatedFixture(t)
	f.runner.Client = interceptor.NewClient(f.runner.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if _, ok := list.(*appsv1.ReplicaSetList); ok && f.store != nil && f.deleteHook == nil {
				rollBrokerOut(t, f)
			}
			return nil
		},
	})
	if err := f.runner.Run(t.Context(), f.cfg); err != nil {
		t.Fatalf("a Broker ReplicaSet created between reads failed the run: %v", err)
	}
	if !readSummary(t, f.cfg.EvidenceDir).Passed || f.deletes != 1 {
		t.Fatal("the run did not pass after the Broker tier changed between reads")
	}
}

func TestFreshModeRejectsBorrowedInputsAndSameNamespace(t *testing.T) {
	for _, failure := range []string{"uid", "same-namespace", "prefix", "mode"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			switch failure {
			case "uid":
				f.cfg.NamespaceUID = "external"
			case "same-namespace":
				f.cfg.OperatorNamespace = f.cfg.Namespace
			case "prefix":
				f.cfg.Namespace = "candidate"
			case "mode":
				f.cfg.NamespaceMode = "implicit"
			}
			if err := f.runner.Run(t.Context(), f.cfg); err == nil {
				t.Fatal("fresh defaults were relaxed")
			}
			if len(f.writes) != 0 || f.deletes != 0 {
				t.Fatal("invalid fresh inputs caused mutation")
			}
		})
	}
}
