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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// The copy of a member rebuild is stored in status.memberRebuild.copy, so
// that its backup and restore operation IDs, its readings and its catch-up
// target are still known after an Operator restart. The API server prunes
// status fields its CRD schema does not declare, so reading the record back
// unchanged shows that the schema keeps every part of it.
var _ = Describe("The copy recorded in a member rebuild", func() {
	ctx := context.Background()

	It("is stored in the status and read back unchanged", func() {
		c := haCluster("rebuild-copy-status")
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })

		at := metav1.NewTime(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Local())
		want := &databasev1alpha1.DatabaseSeeding{
			Member:                "rebuild-copy-status-2",
			Source:                "rebuild-copy-status-0",
			Round:                 2,
			BackupOperationID:     "rebuild-backup-op",
			RestoreOperationID:    "rebuild-restore-op",
			OperationMissingSince: &at,
			ActiveMasterSince:     &at,
			MasterChange: &databasev1alpha1.SeedMasterChange{
				Member: "rebuild-copy-status-1", Role: databasev1alpha1.RoleMaster, ObservedAt: at},
			CatchUpSince:    &at,
			CatchUpWarnedAt: &at,
			LastVerifiedAt:  &at,
			CatchUpTarget:   &databasev1alpha1.LogAddress{Page: 42, Offset: 4096},
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		c.Status.MemberRebuild = &databasev1alpha1.MemberRebuild{
			Member: "rebuild-copy-status-2", PodUID: "pod", ClaimUID: "claim", VolumeName: "pv",
			Source: "rebuild-copy-status-0", SourcePodUID: "source-pod", Token: "token",
			OperationID: "rebuild-op", AcceptedTime: at,
			State: databasev1alpha1.MemberRebuildAccepted,
			Copy:  want,
		}
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())

		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), got)).To(Succeed())
		Expect(got.Status.MemberRebuild).NotTo(BeNil())
		Expect(got.Status.MemberRebuild.Copy).To(Equal(want))
	})
})
