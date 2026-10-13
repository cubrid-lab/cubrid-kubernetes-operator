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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// CUBRID 11.4 creates a database whose name has 17 characters and refuses
// one of 18 (docs/poc/RESULTS.md, POC-24). The API refuses what createdb
// would refuse (#284).
var _ = Describe("Database name length (#284)", func() {
	ctx := context.Background()
	longest := "d" + strings.Repeat("a", 16)
	tooLong := longest + "a"

	It("accepts a 17-character database name and refuses an 18-character one in a CubridCluster", func() {
		named := func(clusterName, database string) *databasev1alpha1.CubridCluster {
			c := standaloneCluster(clusterName)
			c.Spec.Databases[0].Name = database
			return c
		}
		Expect(k8sClient.Create(ctx, named("name-17", longest), client.DryRunAll)).To(Succeed())
		Expect(k8sClient.Create(ctx, named("name-18", tooLong), client.DryRunAll)).
			To(MatchError(ContainSubstring("17")))
	})

	It("accepts a 17-character database name and refuses an 18-character one in a CubridBackup", func() {
		named := func(backupName, database string) *databasev1alpha1.CubridBackup {
			return &databasev1alpha1.CubridBackup{
				ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: testNamespace},
				Spec: databasev1alpha1.CubridBackupSpec{
					ClusterRef: databasev1alpha1.LocalObjectRef{Name: "some-cluster"},
					Database:   database,
					Level:      databasev1alpha1.CubridBackupFull,
					Destination: databasev1alpha1.CubridBackupDestination{
						Type:          databasev1alpha1.DestinationObjectStorage,
						ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: "name-limit-backups"},
					},
				},
			}
		}
		Expect(k8sClient.Create(ctx, named("backup-name-17", longest), client.DryRunAll)).To(Succeed())
		Expect(k8sClient.Create(ctx, named("backup-name-18", tooLong), client.DryRunAll)).
			To(MatchError(ContainSubstring("17")))
	})
})
