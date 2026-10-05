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
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metricspkg "github.com/cubrid-lab/cubrid-kubernetes-operator/internal/metrics"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// fakeRestoreClient stands in for the Instance Manager restore endpoint.
type fakeRestoreClient struct {
	started        bool
	completed      bool
	failed         bool
	idempotencyKey string
	keys           []string
}

const (
	opRestore       = "op-restore"
	testManifestURI = "s3://bucket/prod/uid/manifest.json"
)

func (f *fakeRestoreClient) StartRestore(_ context.Context, _, _, key string, _ instancemanager.RestoreRequest) (instancemanager.Operation, error) {
	f.started = true
	f.idempotencyKey = key
	f.keys = append(f.keys, key)
	return instancemanager.Operation{ID: opRestore, State: instancemanager.OpRestoring}, nil
}

func (f *fakeRestoreClient) GetOperation(_ context.Context, _, _, _ string) (instancemanager.Operation, error) {
	if f.failed {
		return instancemanager.Operation{
			ID: opRestore, State: instancemanager.OpFailed,
			FailureReason: "manager restarted while operation was in progress",
		}, nil
	}
	if f.completed {
		return instancemanager.Operation{ID: opRestore, State: instancemanager.OpCompleted}, nil
	}
	return instancemanager.Operation{ID: opRestore, State: instancemanager.OpRestoring}, nil
}

// memberProber reports one member as master and every other as slave.
type memberProber struct{ master string }

func (p *memberProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	role := databasev1alpha1.RoleSlave
	if podName == p.master {
		role = databasev1alpha1.RoleMaster
	}
	return RoleObservation{Reachable: true, Role: role, ObservedAt: time.Now()}
}

// haCluster returns a valid HA CubridCluster (1 master + 2 slaves) per ADR-0001.
func haCluster(name string) *databasev1alpha1.CubridCluster {
	return &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: databasev1alpha1.CubridClusterSpec{
			Version:          cubridVersion,
			Databases:        []databasev1alpha1.CubridDatabase{{Name: "appdb"}},
			Topology:         databasev1alpha1.CubridTopology{PromotableMembers: 3, ReadReplicas: 0},
			HighAvailability: databasev1alpha1.CubridHighAvailability{Enabled: true},
			Storage: databasev1alpha1.CubridStorage{
				Data: databasev1alpha1.CubridStorageSpec{Size: resource.MustParse("1Gi")},
			},
		},
	}
}

var _ = Describe("CubridCluster Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		cubridcluster := &databasev1alpha1.CubridCluster{}

		BeforeEach(func() {
			By("creating a valid HA CubridCluster")
			err := k8sClient.Get(ctx, typeNamespacedName, cubridcluster)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, haCluster(resourceName))).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &databasev1alpha1.CubridCluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance CubridCluster")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &CubridClusterReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
				IMToken:  testIMToken,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("creating the governing headless Service")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: resourceName + "-instances", Namespace: resourceNamespace,
			}, svc)).To(Succeed())
			Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
			Expect(svc.Spec.PublishNotReadyAddresses).To(BeTrue())

			By("publishing one cubrid_ha.conf with the short member names in ordinal order (#105)")
			haConfig := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: resourceName + "-ha-config", Namespace: resourceNamespace,
			}, haConfig)).To(Succeed())
			Expect(haConfig.Data).To(HaveKeyWithValue("cubrid_ha.conf",
				"[common]\n"+
					"ha_node_list=cubrid@test-resource-0:test-resource-1:test-resource-2\n"+
					"ha_db_list=appdb\n"+
					"ha_port_id=59901\n"))
			owner := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, owner)).To(Succeed())
			Expect(metav1.IsControlledBy(haConfig, owner)).To(BeTrue())

			By("creating the StatefulSet with the desired replicas and OnDelete strategy")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(int32(3)))
			Expect(sts.Spec.ServiceName).To(Equal(resourceName + "-instances"))
			Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))
			Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
			Expect(sts.Spec.VolumeClaimTemplates[0].Name).To(Equal("data"))

			By("wiring the PVC retention policy (default Retain; scale-down always retains, #17)")
			Expect(sts.Spec.PersistentVolumeClaimRetentionPolicy).NotTo(BeNil())
			Expect(sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted).To(Equal(appsv1.RetainPersistentVolumeClaimRetentionPolicyType))
			Expect(sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled).To(Equal(appsv1.RetainPersistentVolumeClaimRetentionPolicyType))

			By("setting a Ready condition (False until instances are ready)")
			updated := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			ready := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))

			By("configuring readiness/liveness probes on the Instance Manager port (#14)")
			c := sts.Spec.Template.Spec.Containers[0]
			Expect(c.ReadinessProbe).NotTo(BeNil())
			Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
			Expect(c.ReadinessProbe.HTTPGet.Port.IntValue()).To(Equal(9090))
			Expect(c.LivenessProbe).NotTo(BeNil())
			Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/livez"))

			By("reporting HAReady Unknown when no role prober is configured (#14/#44)")
			haReady := meta.FindStatusCondition(updated.Status.Conditions, "HAReady")
			Expect(haReady).NotTo(BeNil())
			Expect(haReady.Status).To(Equal(metav1.ConditionUnknown))
			Expect(haReady.Reason).To(Equal("RoleDiscoveryDisabled"))

			By("hardening the pod to Pod Security Standards restricted (#18)")
			Expect(sts.Spec.Template.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
			Expect(*sts.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(BeTrue())
			sc := c.SecurityContext
			Expect(*sc.RunAsNonRoot).To(BeTrue())
			Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
			Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
			Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))

			By("recording the cubrid_cluster_instances metric (#23)")
			Expect(testutil.ToFloat64(metricspkg.ClusterInstances.WithLabelValues(resourceNamespace, resourceName))).To(Equal(float64(3)))
		})
	})

	Context("ClusterReady event (#151)", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "ready-event", Namespace: "default"}

		drain := func(recorder *record.FakeRecorder) []string {
			var events []string
			for {
				select {
				case e := <-recorder.Events:
					events = append(events, e)
				default:
					return events
				}
			}
		}

		It("is emitted once, on the transition to Ready", func() {
			Expect(k8sClient.Create(ctx, haCluster(key.Name))).To(Succeed())
			DeferCleanup(func() {
				c := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
				Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			})
			recorder := record.NewFakeRecorder(20)
			r := &CubridClusterReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder, IMToken: testIMToken,
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(drain(recorder)).NotTo(ContainElement(ContainSubstring("ClusterReady")))

			By("reporting every instance ready")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
			sts.Status.Replicas = 3
			sts.Status.ReadyReplicas = 3
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(drain(recorder)).To(ContainElement(ContainSubstring("ClusterReady")))

			By("reconciling an already Ready cluster")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(drain(recorder)).NotTo(ContainElement(ContainSubstring("ClusterReady")))
		})
	})

	Context("Periodic role observation (#155)", func() {
		const resyncNamespace = "default"
		ctx := context.Background()

		It("asks to be reconciled again and follows a failover", func() {
			key := types.NamespacedName{Name: "resync", Namespace: resyncNamespace}
			Expect(k8sClient.Create(ctx, haCluster(key.Name))).To(Succeed())
			DeferCleanup(func() {
				c := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
				Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			})
			prober := &memberProber{master: "resync-0"}
			r := &CubridClusterReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
				IMToken: testIMToken, Prober: prober,
			}

			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0), "a failover changes no Kubernetes object")
			Expect(res.RequeueAfter).To(BeNumerically("<", roleObservationTTL))
			got := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.CurrentPrimary).To(Equal("resync-0"))

			By("CUBRID failing over with no change to any Pod object")
			prober.master = "resync-1"
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.CurrentPrimary).To(Equal("resync-1"))
		})

		It("does not requeue a standalone cluster for role observation", func() {
			key := types.NamespacedName{Name: "resync-single", Namespace: resyncNamespace}
			single := haCluster(key.Name)
			single.Spec.HighAvailability.Enabled = false
			single.Spec.Topology.PromotableMembers = 1
			Expect(k8sClient.Create(ctx, single)).To(Succeed())
			DeferCleanup(func() {
				c := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
				Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			})
			r := &CubridClusterReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
				IMToken: testIMToken, Prober: &memberProber{},
			}
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())
		})
	})

	Context("Per-member alias Services (ADR-0004, #154)", func() {
		const aliasNamespace = "default"
		ctx := context.Background()
		newReconciler := func() *CubridClusterReconciler {
			return &CubridClusterReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20), IMToken: testIMToken,
			}
		}
		create := func(name string) types.NamespacedName {
			key := types.NamespacedName{Name: name, Namespace: aliasNamespace}
			Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
			DeferCleanup(func() {
				c := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
				Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			})
			return key
		}

		It("creates one headless Service per member, named as the pod", func() {
			key := create("alias-ha")
			_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			owner := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, owner)).To(Succeed())
			for _, member := range []string{"alias-ha-0", "alias-ha-1", "alias-ha-2"} {
				svc := &corev1.Service{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member, Namespace: aliasNamespace}, svc)).To(Succeed())
				Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
				Expect(svc.Spec.PublishNotReadyAddresses).To(BeTrue())
				Expect(svc.Spec.Selector).To(Equal(map[string]string{"statefulset.kubernetes.io/pod-name": member}))
				Expect(metav1.IsControlledBy(svc, owner)).To(BeTrue())
			}
		})

		It("does not take over a Service of that name it does not own", func() {
			foreign := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "alias-taken-1", Namespace: aliasNamespace},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": "someone-else"},
					Ports:    []corev1.ServicePort{{Port: 80}},
				},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, foreign)).To(Succeed()) })
			key := create("alias-taken")

			_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).To(MatchError(ContainSubstring("alias-taken-1")))

			kept := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "alias-taken-1", Namespace: aliasNamespace}, kept)).To(Succeed())
			Expect(kept.Spec.Selector).To(Equal(map[string]string{"app": "someone-else"}))
			updated := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			ready := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal("MemberServiceReconcileFailed"))
		})

		It("removes alias Services that no longer match a member", func() {
			key := create("alias-shrink")
			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			cluster := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
			cluster.Spec.Topology.PromotableMembers = 1
			Expect(r.reconcileMemberServices(ctx, cluster)).To(Succeed())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "alias-shrink-0", Namespace: aliasNamespace}, &corev1.Service{})).To(Succeed())
			for _, gone := range []string{"alias-shrink-1", "alias-shrink-2"} {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: gone, Namespace: aliasNamespace}, &corev1.Service{})
				Expect(errors.IsNotFound(err)).To(BeTrue(), gone)
			}
			By("leaving the governing Service alone")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "alias-shrink-instances", Namespace: aliasNamespace}, &corev1.Service{})).To(Succeed())
		})
	})

	Context("CRD validation (ADR-0001 CEL rules)", func() {
		const ns = "default"
		ctx := context.Background()

		It("accepts a valid HA topology (3/0)", func() {
			c := haCluster("valid-ha")
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})

		It("accepts a valid standalone topology (1/0, HA disabled)", func() {
			c := haCluster("valid-standalone")
			c.Spec.HighAvailability.Enabled = false
			c.Spec.Topology.PromotableMembers = 1
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})

		It("rejects readReplicas != 0", func() {
			c := haCluster("bad-replicas")
			c.Spec.Topology.ReadReplicas = 1
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects HA enabled with promotableMembers != 3", func() {
			c := haCluster("bad-ha-count")
			c.Spec.Topology.PromotableMembers = 2
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects HA disabled with promotableMembers != 1", func() {
			c := haCluster("bad-standalone-count")
			c.Spec.HighAvailability.Enabled = false
			c.Spec.Topology.PromotableMembers = 3
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects fencingPolicy Automatic", func() {
			c := haCluster("bad-fencing")
			c.Spec.HighAvailability.FencingPolicy = databasev1alpha1.FencingAutomatic
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects zero databases", func() {
			c := haCluster("bad-nodb")
			c.Spec.Databases = []databasev1alpha1.CubridDatabase{}
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects a database name rename (immutability)", func() {
			c := haCluster("immutable-db")
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			created := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "immutable-db", Namespace: ns}, created)).To(Succeed())
			created.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: "renamed"}}
			Expect(k8sClient.Update(ctx, created)).NotTo(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})

		It("round-trips spec.bootstrap.recovery and status.bootstrap (ADR-0008)", func() {
			c := haCluster("recovery-bootstrap")
			c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
				Recovery: &databasev1alpha1.RecoverySource{
					ManifestURI: "s3://bucket/prefix/manifest.json",
				},
			}
			c.Spec.ObjectStorage = testObjectStorage()
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })

			key := types.NamespacedName{Name: "recovery-bootstrap", Namespace: ns}
			created := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, created)).To(Succeed())
			Expect(created.Spec.Bootstrap).NotTo(BeNil())
			Expect(created.Spec.Bootstrap.Recovery.ManifestURI).To(Equal("s3://bucket/prefix/manifest.json"))

			created.Status.Bootstrap = &databasev1alpha1.BootstrapStatus{
				Mode:         "Recovery",
				Phase:        databasev1alpha1.BootstrapRestoring,
				ManifestURI:  "s3://bucket/prefix/manifest.json",
				OperationID:  "op-restore-1",
				TargetMember: "recovery-bootstrap-0",
			}
			Expect(k8sClient.Status().Update(ctx, created)).To(Succeed())

			updated := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.Bootstrap).NotTo(BeNil())
			Expect(updated.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapRestoring))
			Expect(updated.Status.Bootstrap.TargetMember).To(Equal("recovery-bootstrap-0"))
		})

		It("starts an interrupted restore again, a limited number of times, and is never Ready meanwhile (#120)", func() {
			newCluster := func(name string) types.NamespacedName {
				c := haCluster(name)
				c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
					Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
				}
				c.Spec.ObjectStorage = testObjectStorage()
				Expect(k8sClient.Create(ctx, c)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
				return types.NamespacedName{Name: name, Namespace: ns}
			}
			reconcileOnce := func(r *CubridClusterReconciler, key types.NamespacedName) *databasev1alpha1.CubridCluster {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				got := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
				Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionReady)).To(BeFalse(),
					"a cluster whose restore has not completed must not be Ready")
				return got
			}

			By("an interrupted restore is started again under a new key and then completes")
			restore := &fakeRestoreClient{}
			r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore, IMToken: testIMToken}
			key := newCluster("recovery-resume")
			reconcileOnce(r, key) // starts
			restore.failed = true
			got := reconcileOnce(r, key) // sees the failure
			Expect(got.Status.Bootstrap.Phase).NotTo(Equal(databasev1alpha1.BootstrapFailed))
			Expect(got.Status.Bootstrap.Attempts).To(BeEquivalentTo(1))
			Expect(got.Status.Bootstrap.OperationID).To(BeEmpty())
			Expect(got.Status.Bootstrap.ManifestURI).To(Equal(testManifestURI))
			Expect(got.Status.Bootstrap.TargetMember).To(Equal("recovery-resume-0"))
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
			Expect(cond.Reason).To(Equal("RestoreFailedRetrying"))

			restore.failed = false
			reconcileOnce(r, key) // starts the second attempt
			Expect(restore.keys).To(HaveLen(2))
			Expect(restore.keys[1]).NotTo(Equal(restore.keys[0]))
			restore.completed = true
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			done := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, done)).To(Succeed())
			Expect(done.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapComplete))

			By("a restore that keeps failing ends as Failed")
			failing := &fakeRestoreClient{}
			rf := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: failing, IMToken: testIMToken}
			keyFailing := newCluster("recovery-giveup")
			var last *databasev1alpha1.CubridCluster
			for range maxBootstrapAttempts {
				failing.failed = false
				reconcileOnce(rf, keyFailing) // start
				failing.failed = true
				last = reconcileOnce(rf, keyFailing) // fail
			}
			Expect(last.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
			cond = meta.FindStatusCondition(last.Status.Conditions, conditionBootstrapReady)
			Expect(cond.Reason).To(Equal("RestoreFailed"))
			Expect(failing.keys).To(HaveLen(maxBootstrapAttempts))
			reconcileOnce(rf, keyFailing)
			Expect(failing.keys).To(HaveLen(maxBootstrapAttempts), "no further attempt after giving up")
		})

		It("rejects a recovery bootstrap without objectStorage (#99)", func() {
			c := haCluster("recovery-no-storage")
			c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
				Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
			}
			err := k8sClient.Create(ctx, c)
			Expect(err).To(HaveOccurred(), "the DB Pods would have no object storage to read the backup from")
			Expect(err.Error()).To(ContainSubstring("requires objectStorage"))
		})

		It("drives recovery bootstrap and gates Ready until restore completes (ADR-0008)", func() {
			c := haCluster("recovery-run")
			c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
				Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
			}
			c.Spec.ObjectStorage = testObjectStorage()
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })

			restore := &fakeRestoreClient{}
			r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore, IMToken: testIMToken}
			key := types.NamespacedName{Name: "recovery-run", Namespace: ns}

			By("starting the restore on the initial master and gating Ready")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(restore.started).To(BeTrue())
			Expect(restore.idempotencyKey).To(ContainSubstring("restore:"))

			restoring := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, restoring)).To(Succeed())
			Expect(restoring.Status.Bootstrap).NotTo(BeNil())
			Expect(restoring.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapRestoring))
			Expect(restoring.Status.Bootstrap.TargetMember).To(Equal("recovery-run-0"))
			ready := meta.FindStatusCondition(restoring.Status.Conditions, conditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("BootstrapRecoveryInProgress"))

			By("completing recovery once the restore operation finishes")
			restore.completed = true
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			done := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, done)).To(Succeed())
			Expect(done.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapComplete))
			bootstrapReady := meta.FindStatusCondition(done.Status.Conditions, conditionBootstrapReady)
			Expect(bootstrapReady).NotTo(BeNil())
			Expect(bootstrapReady.Status).To(Equal(metav1.ConditionTrue))
		})
	})
})
