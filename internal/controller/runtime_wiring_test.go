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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	testDatabase  = "appdb"
	testNamespace = "default"

	testStorageSecret = "object-storage-credentials"
)

// standaloneCluster returns a single-member CubridCluster (HA disabled, ADR-0001).
func standaloneCluster(name string) *databasev1alpha1.CubridCluster {
	return &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: databasev1alpha1.CubridClusterSpec{
			Version:   cubridVersion,
			Databases: []databasev1alpha1.CubridDatabase{{Name: testDatabase}},
			Topology:  databasev1alpha1.CubridTopology{PromotableMembers: 1},
			Storage: databasev1alpha1.CubridStorage{
				Data: databasev1alpha1.CubridStorageSpec{Size: resource.MustParse("1Gi")},
			},
		},
	}
}

// testObjectStorage is a cluster-level object-storage setting for tests.
func testObjectStorage() *databasev1alpha1.CubridObjectStorage {
	return &databasev1alpha1.CubridObjectStorage{
		Endpoint:             "minio.storage.svc:9000",
		Region:               "us-east-1",
		Insecure:             true,
		CredentialsSecretRef: corev1.LocalObjectReference{Name: testStorageSecret},
	}
}

func envValue(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

// The DB Pod runs the Instance Manager image with the configuration, paths and
// token Secret the image's entrypoint expects (#98, runtime contract in ADR-0003).
var _ = Describe("Instance Manager runtime wiring (#98)", func() {
	ctx := context.Background()

	reconcileCluster := func(r *CubridClusterReconciler, cluster *databasev1alpha1.CubridCluster) {
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed())
		})
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: cluster.Name, Namespace: cluster.Namespace,
		}})
		Expect(err).NotTo(HaveOccurred())
	}

	newReconciler := func() *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     record.NewFakeRecorder(10),
			DefaultImage: "registry.example/cubrid-instance-manager:test",
		}
	}

	statefulSet := func(name string) *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, sts)).To(Succeed())
		return sts
	}

	It("runs the Instance Manager image by default and honours spec.image", func() {
		reconcileCluster(newReconciler(), standaloneCluster("wiring-image"))
		c := statefulSet("wiring-image").Spec.Template.Spec.Containers[0]
		Expect(c.Image).To(Equal("registry.example/cubrid-instance-manager:test"))

		custom := standaloneCluster("wiring-image-custom")
		custom.Spec.Image = &databasev1alpha1.CubridImage{Repository: "my.registry/im", Tag: "1.2.3"}
		reconcileCluster(newReconciler(), custom)
		Expect(statefulSet("wiring-image-custom").Spec.Template.Spec.Containers[0].Image).To(Equal("my.registry/im:1.2.3"))
	})

	// The entrypoint creates and starts the database before the manager
	// listens, and /readyz runs a CUBRID command (#174).
	It("lets the first start finish before liveness applies and gives readiness time to answer", func() {
		reconcileCluster(newReconciler(), standaloneCluster("wiring-probes"))
		c := statefulSet("wiring-probes").Spec.Template.Spec.Containers[0]

		Expect(c.StartupProbe).NotTo(BeNil(), "liveness would restart a container that is still creating its database")
		Expect(c.StartupProbe.HTTPGet).NotTo(BeNil())
		Expect(c.StartupProbe.HTTPGet.Path).To(Equal("/livez"))
		allowed := c.StartupProbe.PeriodSeconds * c.StartupProbe.FailureThreshold
		Expect(allowed).To(BeNumerically(">=", 600), "seconds the first start may take")

		Expect(c.LivenessProbe).NotTo(BeNil())
		Expect(c.LivenessProbe.InitialDelaySeconds).To(BeZero(), "the startup probe replaces the delay")
		Expect(c.ReadinessProbe).NotTo(BeNil())
		Expect(c.ReadinessProbe.TimeoutSeconds).To(BeNumerically(">=", 5))
	})

	// With OrderedReady a full restart would start member 0 alone, whatever
	// its role was; the field cannot be changed after creation (#156).
	It("creates all members in parallel", func() {
		reconcileCluster(newReconciler(), standaloneCluster("wiring-parallel"))
		Expect(statefulSet("wiring-parallel").Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))
	})

	It("passes the database, its path on the data volume and the start mode", func() {
		reconcileCluster(newReconciler(), standaloneCluster("wiring-env"))
		c := statefulSet("wiring-env").Spec.Template.Spec.Containers[0]

		db, ok := envValue(c, "CUBRID_DB")
		Expect(ok).To(BeTrue())
		Expect(db.Value).To(Equal(testDatabase))

		databases, ok := envValue(c, "CUBRID_DATABASES")
		Expect(ok).To(BeTrue(), "the image's own CUBRID_DATABASES is not on the PVC")
		Expect(c.VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: "data", MountPath: dataMountPath}))
		Expect(databases.Value).To(HavePrefix(dataMountPath + "/"))

		components, ok := envValue(c, "CUBRID_COMPONENTS")
		Expect(ok).To(BeTrue())
		Expect(components.Value).To(Equal("SERVER"))

		mode, ok := envValue(c, "CUBRID_BOOTSTRAP")
		Expect(ok).To(BeTrue())
		Expect(mode.Value).To(Equal("new"))
	})

	// Without the operation store the manager answers /v1/backup
	// synchronously and refuses /v1/restore/prepare, while the operator
	// expects 202 from both (#99).
	It("gives the manager its operation store and the staging roots the operator uses", func() {
		reconcileCluster(newReconciler(), standaloneCluster("wiring-operations"))
		c := statefulSet("wiring-operations").Spec.Template.Spec.Containers[0]

		ops, ok := envValue(c, "IM_OPERATIONS_DIR")
		Expect(ok).To(BeTrue())
		Expect(ops.Value).To(HavePrefix(dataMountPath+"/"), "operation records must survive a restart")

		for name, want := range map[string]string{
			"IM_BACKUP_STAGING_ROOT":  backupStagingRoot,
			"IM_RESTORE_STAGING_ROOT": restoreStagingRoot,
		} {
			got, ok := envValue(c, name)
			Expect(ok).To(BeTrue(), name)
			Expect(got.Value).To(Equal(want), "%s must be the root the operator builds requests with", name)
		}
		databases, _ := envValue(c, "CUBRID_DATABASES")
		Expect(databases.Value).To(Equal(restoreTargetRoot))

		for _, name := range []string{"IM_S3_ENDPOINT", "IM_S3_ACCESS_KEY", "IM_S3_SECRET_KEY"} {
			_, ok := envValue(c, name)
			Expect(ok).To(BeFalse(), "%s without spec.objectStorage", name)
		}
	})

	It("passes the cluster's object storage, with credentials only as Secret references", func() {
		cluster := standaloneCluster("wiring-storage")
		cluster.Spec.ObjectStorage = testObjectStorage()
		reconcileCluster(newReconciler(), cluster)
		c := statefulSet("wiring-storage").Spec.Template.Spec.Containers[0]

		for name, want := range map[string]string{
			"IM_S3_ENDPOINT": "minio.storage.svc:9000",
			"IM_S3_REGION":   "us-east-1",
			"IM_S3_INSECURE": "true",
		} {
			got, ok := envValue(c, name)
			Expect(ok).To(BeTrue(), name)
			Expect(got.Value).To(Equal(want), name)
		}
		for name, key := range map[string]string{"IM_S3_ACCESS_KEY": "accessKey", "IM_S3_SECRET_KEY": "secretKey"} {
			got, ok := envValue(c, name)
			Expect(ok).To(BeTrue(), name)
			Expect(got.Value).To(BeEmpty(), "%s must not carry the credential itself", name)
			Expect(got.ValueFrom).NotTo(BeNil(), name)
			Expect(got.ValueFrom.SecretKeyRef).NotTo(BeNil(), name)
			Expect(got.ValueFrom.SecretKeyRef.Name).To(Equal(testStorageSecret))
			Expect(got.ValueFrom.SecretKeyRef.Key).To(Equal(key))
		}
	})

	It("starts in recovery mode when the cluster bootstraps from a backup", func() {
		cluster := standaloneCluster("wiring-recovery")
		cluster.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: "s3://bucket/backups/appdb/manifest.json"},
		}
		cluster.Spec.ObjectStorage = testObjectStorage()
		reconcileCluster(newReconciler(), cluster)
		mode, ok := envValue(statefulSet("wiring-recovery").Spec.Template.Spec.Containers[0], "CUBRID_BOOTSTRAP")
		Expect(ok).To(BeTrue())
		Expect(mode.Value).To(Equal("recovery"))
	})

	It("gives each cluster a token of its own through a Secret it owns", func() {
		// The same cluster name in another namespace: only the namespace
		// separates the two, as it separates their users.
		other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "wiring-token-other"}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, other))).To(Succeed())
		second := standaloneCluster("wiring-token")
		second.Namespace = other.Name
		reconcileCluster(newReconciler(), standaloneCluster("wiring-token"))
		reconcileCluster(newReconciler(), second)

		tokens := map[string]string{}
		for _, ns := range []string{testNamespace, other.Name} {
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "wiring-token-im-token", Namespace: ns}, secret)).To(Succeed())
			Expect(secret.OwnerReferences).To(HaveLen(1))
			Expect(secret.OwnerReferences[0].Kind).To(Equal("CubridCluster"))
			tokens[ns] = string(secret.Data[imTokenKey])
			Expect(tokens[ns]).To(MatchRegexp("^[0-9a-f]{64}$"), "a random 256-bit token in %s", ns)
		}
		Expect(tokens[testNamespace]).NotTo(Equal(tokens[other.Name]), "two clusters share a token")

		token, ok := envValue(statefulSet("wiring-token").Spec.Template.Spec.Containers[0], "IM_TOKEN")
		Expect(ok).To(BeTrue())
		Expect(token.Value).To(BeEmpty(), "the token must come from the Secret, never inline")
		Expect(token.ValueFrom).NotTo(BeNil())
		Expect(token.ValueFrom.SecretKeyRef).NotTo(BeNil())
		Expect(token.ValueFrom.SecretKeyRef.Name).To(Equal("wiring-token-im-token"))
		Expect(token.ValueFrom.SecretKeyRef.Key).To(Equal(imTokenKey))
	})

	It("keeps a cluster's token, which its running Pods hold, across reconciles", func() {
		cluster := standaloneCluster("wiring-token-kept")
		reconcileCluster(newReconciler(), cluster)
		secret := &corev1.Secret{}
		key := types.NamespacedName{Name: "wiring-token-kept-im-token", Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
		created := string(secret.Data[imTokenKey])
		Expect(created).NotTo(BeEmpty())

		reconcileAgain := func() {
			_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
				Name: cluster.Name, Namespace: cluster.Namespace,
			}})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
		}
		reconcileAgain()
		Expect(string(secret.Data[imTokenKey])).To(Equal(created))

		// A token put there by hand is the one Pods started since then hold.
		secret.Data[imTokenKey] = []byte("a-token-set-by-hand")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		reconcileAgain()
		Expect(string(secret.Data[imTokenKey])).To(Equal("a-token-set-by-hand"))

		// An empty one would switch the Instance Manager's authentication off.
		delete(secret.Data, imTokenKey)
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		reconcileAgain()
		Expect(string(secret.Data[imTokenKey])).To(MatchRegexp("^[0-9a-f]{64}$"))
	})

	It("runs as the image's cubrid user with a data volume that user can write", func() {
		reconcileCluster(newReconciler(), standaloneCluster("wiring-user"))
		pod := statefulSet("wiring-user").Spec.Template.Spec
		Expect(pod.SecurityContext.RunAsUser).NotTo(BeNil())
		Expect(*pod.SecurityContext.RunAsUser).To(Equal(cubridUID))
		Expect(*pod.SecurityContext.RunAsGroup).To(Equal(cubridUID))
		Expect(*pod.SecurityContext.FSGroup).To(Equal(cubridUID))
		Expect(*pod.SecurityContext.RunAsNonRoot).To(BeTrue())
	})
})
