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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// How a cluster's first database comes to be is decided when the cluster is
// created: restored from a backup, or created empty. Changing that later
// would let a recovery that is running, or has failed, turn into an empty
// database, or point a restore at another backup under the same operation.
// A CubridBackup is one request with one idempotency key, so what it asks
// for cannot change either (#283).
var _ = Describe("Intent that is fixed at creation (#283)", func() {
	ctx := context.Background()
	const otherManifest = "s3://backups/prod/other/manifest.json"

	create := func(obj client.Object) types.NamespacedName {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed()) })
		return client.ObjectKeyFromObject(obj)
	}
	recovery := func(uri string) *databasev1alpha1.CubridBootstrap {
		return &databasev1alpha1.CubridBootstrap{Recovery: &databasev1alpha1.RecoverySource{ManifestURI: uri}}
	}
	// change applies edit to the stored cluster and returns the API server's
	// answer.
	change := func(key types.NamespacedName, edit func(*databasev1alpha1.CubridCluster)) error {
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		edit(c)
		return k8sClient.Update(ctx, c)
	}

	Context("the bootstrap of a cluster", func() {
		It("is accepted with or without a recovery when the cluster is created", func() {
			create(haCluster("boot-fresh"))
			restored := haCluster("boot-restored")
			restored.Spec.Bootstrap = recovery(testManifestURI)
			create(restored)
		})

		It("cannot lose its recovery, which would turn a restore into an empty database", func() {
			c := haCluster("boot-remove")
			c.Spec.Bootstrap = recovery(testManifestURI)
			key := create(c)
			Expect(change(key, func(c *databasev1alpha1.CubridCluster) { c.Spec.Bootstrap = nil })).
				To(MatchError(ContainSubstring("bootstrap")))
			Expect(change(key, func(c *databasev1alpha1.CubridCluster) { c.Spec.Bootstrap.Recovery = nil })).
				To(MatchError(ContainSubstring("bootstrap")))
		})

		It("cannot gain a recovery after the cluster was created without one", func() {
			key := create(haCluster("boot-add"))
			Expect(change(key, func(c *databasev1alpha1.CubridCluster) { c.Spec.Bootstrap = recovery(testManifestURI) })).
				To(MatchError(ContainSubstring("bootstrap")))
		})

		It("cannot point its recovery at another backup", func() {
			c := haCluster("boot-change")
			c.Spec.Bootstrap = recovery(testManifestURI)
			key := create(c)
			Expect(change(key, func(c *databasev1alpha1.CubridCluster) { c.Spec.Bootstrap.Recovery.ManifestURI = otherManifest })).
				To(MatchError(ContainSubstring("bootstrap")))
		})

		It("leaves the rest of the spec changeable", func() {
			c := haCluster("boot-other")
			c.Spec.Bootstrap = recovery(testManifestURI)
			key := create(c)
			Expect(change(key, func(c *databasev1alpha1.CubridCluster) {
				c.Spec.ObjectStorage.Region = "eu-west-1"
			})).To(Succeed())
			fresh := create(haCluster("boot-other-fresh"))
			Expect(change(fresh, func(c *databasev1alpha1.CubridCluster) {
				// An empty bootstrap says the same as none.
				c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{}
			})).To(Succeed())
		})
	})

	Context("the spec of a CubridBackup", func() {
		backup := func(name string) *databasev1alpha1.CubridBackup {
			return &databasev1alpha1.CubridBackup{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
				Spec: databasev1alpha1.CubridBackupSpec{
					ClusterRef: databasev1alpha1.LocalObjectRef{Name: "some-cluster"},
					Database:   testDatabase,
					Destination: databasev1alpha1.CubridBackupDestination{
						Type:          databasev1alpha1.DestinationObjectStorage,
						ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: "backups", Prefix: "prod"},
					},
				},
			}
		}
		edit := func(key types.NamespacedName, change func(*databasev1alpha1.CubridBackup)) error {
			b := &databasev1alpha1.CubridBackup{}
			Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
			change(b)
			return k8sClient.Update(ctx, b)
		}

		It("cannot be changed after the request was made", func() {
			key := create(backup("frozen"))
			Expect(edit(key, func(b *databasev1alpha1.CubridBackup) { b.Spec.Database = "otherdb" })).
				To(MatchError(ContainSubstring("cannot be changed")))
			Expect(edit(key, func(b *databasev1alpha1.CubridBackup) { b.Spec.ClusterRef.Name = "other-cluster" })).
				To(MatchError(ContainSubstring("cannot be changed")))
			Expect(edit(key, func(b *databasev1alpha1.CubridBackup) { b.Spec.Destination.ObjectStorage.Prefix = "elsewhere" })).
				To(MatchError(ContainSubstring("cannot be changed")))
			Expect(edit(key, func(b *databasev1alpha1.CubridBackup) {
				b.Spec.Target.Preference = databasev1alpha1.PrimaryOnly
			})).To(MatchError(ContainSubstring("cannot be changed")))
		})

		It("still takes labels, annotations and its status", func() {
			key := create(backup("labelled"))
			Expect(edit(key, func(b *databasev1alpha1.CubridBackup) {
				b.Labels = map[string]string{"team": "data"}
				b.Annotations = map[string]string{"note": "weekly"}
			})).To(Succeed())
			b := &databasev1alpha1.CubridBackup{}
			Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
			b.Status.Phase = databasev1alpha1.BackupPhasePending
			Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		})
	})
})
