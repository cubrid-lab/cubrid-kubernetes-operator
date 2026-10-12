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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// The API refuses what the operator would accept and then not do: a volume
// size or class changed on a cluster whose volumes exist (the operator does
// not resize or move them), a size that is no size, and image settings that
// name no image or are put together wrongly (#305).
var _ = Describe("Storage and image settings that cannot be applied (#305)", func() {
	ctx := context.Background()
	const digest = "registry.example/cubrid-instance-manager@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	dryRun := func(c *databasev1alpha1.CubridCluster) error {
		return k8sClient.Create(ctx, c, client.DryRunAll)
	}
	withSize := func(name, size string) *databasev1alpha1.CubridCluster {
		c := standaloneCluster(name)
		c.Spec.Storage.Data.Size = resource.MustParse(size)
		return c
	}
	withImage := func(name, repository, tag string) *databasev1alpha1.CubridCluster {
		c := standaloneCluster(name)
		c.Spec.Image = &databasev1alpha1.CubridImage{Repository: repository, Tag: tag}
		return c
	}
	create := func(c *databasev1alpha1.CubridCluster) types.NamespacedName {
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		return client.ObjectKeyFromObject(c)
	}
	change := func(key types.NamespacedName, edit func(*databasev1alpha1.CubridCluster)) error {
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		edit(c)
		return k8sClient.Update(ctx, c)
	}

	It("refuses a data volume without a size", func() {
		Expect(dryRun(withSize("size-zero", "0"))).To(MatchError(ContainSubstring("size")))
		Expect(dryRun(withSize("size-negative", "-1Gi"))).To(MatchError(ContainSubstring("size")))
		Expect(dryRun(withSize("size-ok", "1Gi"))).To(Succeed())
		Expect(dryRun(withSize("size-small", "500Mi"))).To(Succeed())
	})

	It("refuses a change of the data volume of an existing cluster", func() {
		key := create(withSize("volume-fixed", "1Gi"))
		Expect(change(key, func(c *databasev1alpha1.CubridCluster) {
			c.Spec.Storage.Data.Size = resource.MustParse("2Gi")
		})).To(MatchError(ContainSubstring("size")))
		Expect(change(key, func(c *databasev1alpha1.CubridCluster) {
			class := "fast"
			c.Spec.Storage.Data.StorageClassName = &class
		})).To(MatchError(ContainSubstring("storageClassName")))

		classed := withSize("volume-classed", "1Gi")
		standard := "standard"
		classed.Spec.Storage.Data.StorageClassName = &standard
		classedKey := create(classed)
		Expect(change(classedKey, func(c *databasev1alpha1.CubridCluster) {
			class := "fast"
			c.Spec.Storage.Data.StorageClassName = &class
		})).To(MatchError(ContainSubstring("storageClassName")))
		Expect(change(classedKey, func(c *databasev1alpha1.CubridCluster) {
			c.Spec.Storage.Data.StorageClassName = nil
		})).To(MatchError(ContainSubstring("storageClassName")))

		By("leaving the rest of the cluster changeable")
		Expect(change(key, func(c *databasev1alpha1.CubridCluster) {
			c.Labels = map[string]string{"team": "storage"}
			c.Spec.Image = &databasev1alpha1.CubridImage{Repository: "registry.example/cubrid-instance-manager", Tag: "v2"}
		})).To(Succeed())
	})

	It("refuses image settings that name no image", func() {
		Expect(dryRun(withImage("image-tag-only", "", "11.4"))).To(MatchError(ContainSubstring("repository")))
		Expect(dryRun(withImage("image-digest-and-tag", digest, "11.4"))).To(MatchError(ContainSubstring("digest")))
	})

	It("accepts a repository with a tag, with a digest, or alone, and no image at all", func() {
		Expect(dryRun(withImage("image-tagged", "registry.example/cubrid-instance-manager", "v1"))).To(Succeed())
		Expect(dryRun(withImage("image-digest", digest, ""))).To(Succeed())
		Expect(dryRun(withImage("image-bare", "registry.example/cubrid-instance-manager", ""))).To(Succeed())
		Expect(dryRun(standaloneCluster("image-default"))).To(Succeed())
	})
})
