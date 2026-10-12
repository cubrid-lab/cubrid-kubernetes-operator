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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// An HA cluster copies its first database to the other members through the
// cluster's object storage. Without a bucket, an endpoint and credentials
// the bootstrap stops after the first member, so such a cluster is refused
// when it is created, before any Pod exists. This is what the current
// seeding transport needs, not a rule about HA as such (#285, #256).
var _ = Describe("Object storage of an HA cluster (#285)", func() {
	ctx := context.Background()

	// dryRun asks the API server to validate the cluster without storing it.
	dryRun := func(c *databasev1alpha1.CubridCluster) error {
		return k8sClient.Create(ctx, c, client.DryRunAll)
	}
	storage := func(change func(*databasev1alpha1.CubridObjectStorage)) *databasev1alpha1.CubridObjectStorage {
		s := haObjectStorage()
		change(s)
		return s
	}
	unchanged := func(*databasev1alpha1.CubridObjectStorage) {}
	haWith := func(name string, s *databasev1alpha1.CubridObjectStorage) *databasev1alpha1.CubridCluster {
		c := haCluster(name)
		c.Spec.ObjectStorage = s
		return c
	}

	It("refuses an HA cluster that cannot copy its database to the other members", func() {
		Expect(dryRun(haWith("ha-no-storage", nil))).To(MatchError(ContainSubstring("objectStorage")))
		Expect(dryRun(haWith("ha-no-bucket", storage(func(s *databasev1alpha1.CubridObjectStorage) { s.Bucket = "" })))).
			To(MatchError(ContainSubstring("bucket")))
		Expect(dryRun(haWith("ha-no-credentials", storage(func(s *databasev1alpha1.CubridObjectStorage) {
			s.CredentialsSecretRef.Name = ""
		})))).To(MatchError(ContainSubstring("credentialsSecretRef")))
		Expect(dryRun(haWith("ha-no-endpoint", storage(func(s *databasev1alpha1.CubridObjectStorage) { s.Endpoint = "" })))).
			To(HaveOccurred())
	})

	It("accepts an HA cluster with an endpoint, credentials and a bucket", func() {
		Expect(dryRun(haWith("ha-storage", storage(unchanged)))).To(Succeed())
	})

	It("leaves a standalone cluster as it was: no object storage, or one without a bucket", func() {
		Expect(dryRun(standaloneCluster("alone-no-storage"))).To(Succeed())
		alone := standaloneCluster("alone-no-bucket")
		alone.Spec.ObjectStorage = storage(func(s *databasev1alpha1.CubridObjectStorage) { s.Bucket = "" })
		Expect(dryRun(alone)).To(Succeed())
	})

	It("refuses credentials without a name for a standalone cluster as well", func() {
		alone := standaloneCluster("alone-no-credentials")
		alone.Spec.ObjectStorage = storage(func(s *databasev1alpha1.CubridObjectStorage) { s.CredentialsSecretRef.Name = "" })
		Expect(dryRun(alone)).To(MatchError(ContainSubstring("credentialsSecretRef")))
	})

	It("accepts the sample cluster", func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "database_v1alpha1_cubridcluster.yaml"))
		Expect(err).NotTo(HaveOccurred())
		sample := &databasev1alpha1.CubridCluster{}
		Expect(yaml.Unmarshal(raw, sample)).To(Succeed())
		sample.Namespace = testNamespace
		Expect(dryRun(sample)).To(Succeed())
	})
})
