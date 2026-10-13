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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// waitBucket is the bucket of the backups of these tests.
const waitBucket = "wait-backups"

// downProber answers like memberProber, except that the member down does not
// answer at all.
type downProber struct{ master, down string }

func (p *downProber) ProbeRole(ctx context.Context, podName, namespace string) RoleObservation {
	if podName == p.down {
		return RoleObservation{Reachable: false, ObservedAt: time.Now()}
	}
	return (&memberProber{master: p.master}).ProbeRole(ctx, podName, namespace)
}

// #365: a backup whose start answer was lost, and whose recorded target can
// no longer be sent the start again, reaches a defined outcome. It is never
// started on another member.
var _ = Describe("CubridBackup wait on its recorded target", func() {
	const namespace = "default"
	ctx := context.Background()

	type fixture struct {
		backup *databasev1alpha1.CubridBackup
		fake   *lostResponseBackupClient
		prober *downProber
		r      *CubridBackupReconciler
		now    *time.Time
	}

	// lostStart creates an HA cluster and a backup, and dispatches the backup
	// to its first standby with the answer lost: the standby is recorded, and
	// the backup may or may not run on it.
	lostStart := func(name string) fixture {
		cluster := haCluster(name)
		cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
		cluster.Spec.ObjectStorage = haObjectStorage()
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		createAppliedMemberPods(ctx, cluster)

		backup := &databasev1alpha1.CubridBackup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasev1alpha1.CubridBackupSpec{
				ClusterRef: databasev1alpha1.LocalObjectRef{Name: name},
				Database:   demoDB,
				Level:      databasev1alpha1.CubridBackupFull,
				Destination: databasev1alpha1.CubridBackupDestination{
					Type:          databasev1alpha1.DestinationObjectStorage,
					ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: waitBucket},
				},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

		now := time.Now()
		f := fixture{
			backup: backup,
			fake:   &lostResponseBackupClient{lose: 1},
			prober: &downProber{master: name + "-0"},
			now:    &now,
		}
		f.r = &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: f.prober, Backup: f.fake,
			Clock: func() time.Time { return *f.now },
		}
		reconcileOnce(ctx, f.r, backup)
		Expect(f.fake.instances).To(Equal([]string{name + "-1"}))
		Expect(stored(ctx, backup).Status.TargetInstance).To(Equal(name + "-1"))
		return f
	}

	It("adopts the operation the ineligible target holds and follows it to its end", func() {
		f := lostStart("bk-adopt")
		const standby = "bk-adopt-1"
		f.fake.held = &instancemanager.Operation{
			ID: opFake, Kind: instancemanager.OpBackup, State: instancemanager.OpRunningBackup,
		}

		By("the recorded standby becoming the master")
		f.prober.master = standby
		reconcileOnce(ctx, f.r, f.backup)
		got := stored(ctx, f.backup)
		Expect(got.Status.OperationRef).To(Equal(opFake), "the operation was not adopted")
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
		Expect(got.Status.TargetInstance).To(Equal(standby))
		Expect(f.fake.lookups).To(HaveEach(standby))
		Expect(f.fake.lookupKeys).To(HaveEach(f.fake.keys[0]), "the lookup did not use the backup's key")

		By("the operation completing")
		f.fake.held = &instancemanager.Operation{
			ID: opFake, Kind: instancemanager.OpBackup, State: instancemanager.OpCompleted,
			Artifact: &instancemanager.OperationArtifact{ManifestURI: "s3://cubrid-backups/a/manifest.json", Database: demoDB},
		}
		reconcileOnce(ctx, f.r, f.backup)
		got = stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseCompleted))
		Expect(got.Status.Artifact).NotTo(BeNil())
		Expect(got.Status.Artifact.URI).To(Equal("s3://cubrid-backups/a/manifest.json"))
		Expect(f.fake.instances).To(Equal([]string{standby}), "a start was sent after the first one")
	})

	It("fails after the bound when the ineligible target holds no operation, and starts nothing", func() {
		f := lostStart("bk-none")
		const standby = "bk-none-1"
		recordedAt := stored(ctx, f.backup).Status.TargetRecordedAt
		Expect(recordedAt).NotTo(BeNil(), "the time the target was recorded is not in status")

		By("the recorded standby becoming the master")
		f.prober.master = standby
		reconcileOnce(ctx, f.r, f.backup)
		got := stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))
		Expect(readyOf(got).Reason).To(Equal(reasonRecordedTargetNotEligible))

		By("the wait going on until just before the bound")
		*f.now = recordedAt.Add(backupTargetWait - time.Minute)
		reconcileOnce(ctx, f.r, f.backup)
		Expect(stored(ctx, f.backup).Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))

		By("the bound passing")
		*f.now = recordedAt.Add(backupTargetWait + time.Second)
		reconcileOnce(ctx, f.r, f.backup)
		got = stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		Expect(got.Status.CompletedAt).NotTo(BeNil())
		Expect(readyOf(got).Reason).To(Equal("RecordedTargetNotStarted"))
		Expect(readyOf(got).Message).To(ContainSubstring(standby))
		Expect(got.Status.OperationRef).To(BeEmpty())

		By("nothing being started afterwards, also when the target is eligible again")
		f.prober.master = "bk-none-0"
		reconcileOnce(ctx, f.r, f.backup)
		Expect(f.fake.instances).To(Equal([]string{standby}), "a start was sent to a member")
		Expect(f.fake.lookups).To(HaveEach(standby), "another member was asked")
	})

	It("fails after the bound when the target cannot be asked, and never starts another member", func() {
		f := lostStart("bk-down")
		const standby = "bk-down-1"
		recordedAt := stored(ctx, f.backup).Status.TargetRecordedAt
		Expect(recordedAt).NotTo(BeNil())

		By("the recorded standby going down, with the other standby healthy")
		f.prober.down = standby
		f.fake.lookupErr = errors.New("dial tcp: i/o timeout")
		for _, at := range []time.Duration{0, 10 * time.Minute, backupTargetWait - time.Second} {
			*f.now = recordedAt.Add(at)
			reconcileOnce(ctx, f.r, f.backup)
			got := stored(ctx, f.backup)
			Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending), "at %s", at)
			Expect(readyOf(got).Reason).To(Equal(reasonRecordedTargetNotEligible))
		}
		Expect(f.fake.instances).To(Equal([]string{standby}), "a start was sent during the wait")

		By("the bound passing")
		*f.now = recordedAt.Add(backupTargetWait + time.Second)
		reconcileOnce(ctx, f.r, f.backup)
		got := stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		Expect(readyOf(got).Reason).To(Equal("RecordedTargetUnverified"))
		Expect(readyOf(got).Message).To(ContainSubstring(standby))
		Expect(readyOf(got).Message).To(ContainSubstring("may still"))

		By("nothing being started afterwards, also when the target answers again")
		f.prober.down = ""
		f.fake.lookupErr = nil
		reconcileOnce(ctx, f.r, f.backup)
		Expect(stored(ctx, f.backup).Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		Expect(f.fake.instances).To(Equal([]string{standby}), "a start was sent to a member")
		Expect(f.fake.lookups).To(HaveEach(standby), "another member was asked")
	})

	It("bounds a recorded target that keeps refusing the start without object storage", func() {
		f := lostStart("bk-412")
		const standby = "bk-412-1"
		recordedAt := stored(ctx, f.backup).Status.TargetRecordedAt
		Expect(recordedAt).NotTo(BeNil())
		f.fake.lose = 1000
		f.fake.startErr = fmt.Errorf("start backup on %s: %w", standby, errObjectStorageNotApplied)

		*f.now = recordedAt.Add(backupTargetWait - time.Second)
		reconcileOnce(ctx, f.r, f.backup)
		got := stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))
		Expect(readyOf(got).Reason).To(Equal(reasonObjectStorageNotApplied))

		*f.now = recordedAt.Add(backupTargetWait + time.Second)
		reconcileOnce(ctx, f.r, f.backup)
		got = stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		Expect(readyOf(got).Reason).To(Equal("RecordedTargetNotStarted"))
		Expect(f.fake.instances).To(HaveEach(standby), "a start was sent to another member")
	})

	It("keeps the bound across an operator restart", func() {
		f := lostStart("bk-restart")
		const standby = "bk-restart-1"
		recordedAt := stored(ctx, f.backup).Status.TargetRecordedAt
		Expect(recordedAt).NotTo(BeNil())
		f.prober.master = standby
		reconcileOnce(ctx, f.r, f.backup)

		By("a new operator process, with nothing in memory, after the bound")
		later := recordedAt.Add(backupTargetWait + time.Minute)
		fake := &lostResponseBackupClient{}
		restarted := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: &memberProber{master: standby}, Backup: fake,
			Clock: func() time.Time { return later },
		}
		reconcileOnce(ctx, restarted, f.backup)
		got := stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed), "the wait started again after the restart")
		Expect(readyOf(got).Reason).To(Equal("RecordedTargetNotStarted"))
		Expect(fake.instances).To(BeEmpty())
	})

	It("does not cut short the wait of a target recorded without its time", func() {
		f := lostStart("bk-legacy")
		const standby = "bk-legacy-1"
		By("a status written by an operator that did not record the time")
		legacy := stored(ctx, f.backup)
		legacy.Status.TargetRecordedAt = nil
		Expect(k8sClient.Status().Update(ctx, legacy)).To(Succeed())

		f.prober.master = standby
		*f.now = time.Now().Add(24 * time.Hour)
		reconcileOnce(ctx, f.r, f.backup)
		got := stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))
		Expect(got.Status.TargetRecordedAt).NotTo(BeNil())
		Expect(got.Status.TargetRecordedAt.Time).To(BeTemporally("~", *f.now, time.Second))
	})

	It("fails a backup whose selected Pod does not pick up object storage within the bound", func() {
		cluster := haCluster("bk-osna")
		cluster.Spec.Topology.PromotableMembers = 1
		cluster.Spec.HighAvailability.Enabled = false
		cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
		cluster.Spec.ObjectStorage = haObjectStorage()
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		const member = "bk-osna-0"
		createMemberPod(ctx, namespace, member, nil)

		backup := &databasev1alpha1.CubridBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-osna", Namespace: namespace},
			Spec: databasev1alpha1.CubridBackupSpec{
				ClusterRef: databasev1alpha1.LocalObjectRef{Name: "bk-osna"},
				Database:   demoDB,
				Level:      databasev1alpha1.CubridBackupFull,
				Destination: databasev1alpha1.CubridBackupDestination{
					Type:          databasev1alpha1.DestinationObjectStorage,
					ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: waitBucket},
				},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
		created := stored(ctx, backup).CreationTimestamp.Time

		fake := &lostResponseBackupClient{}
		now := created.Add(backupTargetWait - time.Second)
		r := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: &memberProber{master: member}, Backup: fake,
			Clock: func() time.Time { return now },
		}
		reconcileOnce(ctx, r, backup)
		got := stored(ctx, backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))
		Expect(readyOf(got).Reason).To(Equal(reasonObjectStorageNotApplied))

		now = created.Add(backupTargetWait + time.Second)
		reconcileOnce(ctx, r, backup)
		got = stored(ctx, backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		Expect(got.Status.CompletedAt).NotTo(BeNil())
		Expect(readyOf(got).Reason).To(Equal("ObjectStorageWaitExpired"))
		Expect(got.Status.TargetInstance).To(BeEmpty())
		Expect(fake.instances).To(BeEmpty(), "a start was sent")
		Expect(fake.lookups).To(BeEmpty(), "a member was asked although nothing was sent to it")
	})
})

func reconcileOnce(ctx context.Context, r *CubridBackupReconciler, backup *databasev1alpha1.CubridBackup) {
	GinkgoHelper()
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backup)})
	Expect(err).NotTo(HaveOccurred())
}

func stored(ctx context.Context, backup *databasev1alpha1.CubridBackup) *databasev1alpha1.CubridBackup {
	GinkgoHelper()
	got := &databasev1alpha1.CubridBackup{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), got)).To(Succeed())
	return got
}

func readyOf(backup *databasev1alpha1.CubridBackup) metav1.Condition {
	GinkgoHelper()
	cond := meta.FindStatusCondition(backup.Status.Conditions, conditionBackupReady)
	Expect(cond).NotTo(BeNil())
	return *cond
}

// answerBody answers every call with status and body.
type answerBody struct {
	status int
	body   string
	asked  []string
}

func (a *answerBody) RoundTrip(req *http.Request) (*http.Response, error) {
	a.asked = append(a.asked, req.Method+" "+req.URL.RequestURI())
	return &http.Response{StatusCode: a.status, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(a.body)), Request: req}, nil
}

// The lookup tells "the member holds none" from every other answer: only an
// explicit empty list is none. A manager without the endpoint answers 404,
// which is not an answer about the operation (#365).
func TestHTTPBackupClient_FindBackupOperation(t *testing.T) {
	const key = "cubridbackup:ns:bk:uid"
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantFound bool
		wantErr   bool
	}{
		{name: "none", status: http.StatusOK, body: `{"operations":[]}`},
		{name: "held", status: http.StatusOK, wantFound: true,
			body: `{"operations":[{"id":"op-1","kind":"backup","idempotencyKey":"` + key + `","state":"RunningBackup"}]}`},
		{name: "only another kind", status: http.StatusOK,
			body: `{"operations":[{"id":"op-1","kind":"restore","idempotencyKey":"` + key + `","state":"Completed"}]}`},
		{name: "a manager without the endpoint", status: http.StatusNotFound, body: "404 page not found", wantErr: true},
		{name: "a manager without a store", status: http.StatusServiceUnavailable, body: `{"error":"x"}`, wantErr: true},
		{name: "an unreadable answer", status: http.StatusOK, body: `not json`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &answerBody{status: tc.status, body: tc.body}
			c := NewHTTPBackupClient(staticToken("tok"))
			c.Client = &http.Client{Transport: transport}
			op, found, err := c.FindBackupOperation(context.Background(), "db-0", "ns", key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if found && op.ID != "op-1" {
				t.Errorf("op = %+v", op)
			}
			if len(transport.asked) != 1 || !strings.HasPrefix(transport.asked[0], "GET /v1/operations?idempotencyKey=") {
				t.Errorf("asked %v, want one GET /v1/operations?idempotencyKey=", transport.asked)
			}
		})
	}
}
