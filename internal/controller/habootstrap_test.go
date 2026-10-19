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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// fakeHABootstrapClient records who was asked and answers with a set state.
type fakeHABootstrapClient struct {
	members []string
	keys    []string
	state   instancemanager.OperationState
	reason  string
	err     error
}

func (f *fakeHABootstrapClient) StartHABootstrap(_ context.Context, podName, _, key string, _ instancemanager.HABootstrapRequest) (instancemanager.Operation, error) {
	f.members = append(f.members, podName)
	f.keys = append(f.keys, key)
	if f.err != nil {
		return instancemanager.Operation{}, f.err
	}
	return instancemanager.Operation{ID: "op-ha", State: f.state, FailureReason: f.reason}, nil
}

func (f *fakeHABootstrapClient) FindOperation(_ context.Context, _, _ string, _ instancemanager.OperationKind, _ string) (instancemanager.Operation, bool, error) {
	return instancemanager.Operation{}, false, errors.New("not used by these tests")
}

// fakeSeedClient stands in for the backup and restore side of seeding. It
// answers each member with the state set for it.
type fakeSeedClient struct {
	backupOn    []string
	backupReq   instancemanager.BackupRequest
	restoreOn   []string
	restoreKeys map[string]string
	restoreReq  instancemanager.RestoreRequest
	backupState instancemanager.OperationState
	restore     map[string]instancemanager.OperationState
	reason      string
	// backups and restores keep every request by its idempotency key, as the
	// Instance Managers do.
	backups  map[string]seedBackup
	restores map[string]instancemanager.RestoreRequest
	// backupStates overrides backupState for the backups of single members.
	backupStates map[string]instancemanager.OperationState
	// heldKeys makes the fake answer like an Instance Manager does when a key
	// is sent again with another request: 409 IdempotencyConflict, and the
	// operation stays the one first started under the key. lookups counts
	// the lookups by key.
	heldKeys bool
	lookups  int
	// restoreByKey overrides restore for the operation under one key, and
	// restoredFrom names the manifest an operation under a key restored.
	restoreByKey map[string]instancemanager.OperationState
	restoredFrom map[string]string
	// restoresByKey keeps every restore request by its idempotency key, so
	// that the operations of earlier attempts can be looked up, with the
	// hash of the request body they were started with.
	restoresByKey map[string]instancemanager.RestoreRequest
	// findErr is what a lookup of a restore operation answers instead, and
	// lookupKeys are the keys of every restore lookup.
	findErr    error
	lookupKeys []string
}

// restoreOp is the restore operation the fake holds on podName under key.
func (f *fakeSeedClient) restoreOp(podName, key string) instancemanager.Operation {
	state := f.restore[podName]
	if s, ok := f.restoreByKey[key]; ok {
		state = s
	}
	op := instancemanager.Operation{ID: "op-seed-" + podName, Kind: instancemanager.OpRestore, State: state, FailureReason: f.reason}
	if uri, ok := f.restoredFrom[key]; ok {
		op.Restored = &instancemanager.OperationArtifact{ManifestURI: uri}
	}
	if req, ok := f.restoresByKey[key]; ok {
		body, err := json.Marshal(req)
		Expect(err).NotTo(HaveOccurred())
		op.RequestHash = instancemanager.HashRequest(body)
	}
	return op
}

// seedBackup is one backup the fake was asked for.
type seedBackup struct {
	member string
	req    instancemanager.BackupRequest
}

func (f *fakeSeedClient) StartBackup(_ context.Context, podName, _, key string, req instancemanager.BackupRequest) (instancemanager.Operation, error) {
	f.backupOn = append(f.backupOn, podName)
	if f.backups == nil {
		f.backups = map[string]seedBackup{}
	}
	if held, ok := f.backups[key]; ok && f.heldKeys && !reflect.DeepEqual(held.req, req) {
		return instancemanager.Operation{}, fmt.Errorf("start backup on %s: %w", podName, imIdempotencyConflict())
	}
	f.backupReq = req
	f.backups[key] = seedBackup{member: podName, req: req}
	state := f.backupState
	if s, ok := f.backupStates[podName]; ok {
		state = s
	}
	return instancemanager.Operation{ID: "op-seed-backup", State: state, FailureReason: f.reason}, nil
}

func (f *fakeSeedClient) StartRestore(_ context.Context, podName, _, key string, req instancemanager.RestoreRequest) (instancemanager.Operation, error) {
	f.restoreOn = append(f.restoreOn, podName)
	if f.restoreKeys == nil {
		f.restoreKeys = map[string]string{}
	}
	if f.heldKeys && f.restoreKeys[podName] == key && !reflect.DeepEqual(f.restores[podName], req) {
		return instancemanager.Operation{}, fmt.Errorf("start restore on %s: %w", podName, imIdempotencyConflict())
	}
	f.restoreKeys[podName] = key
	f.restoreReq = req
	if f.restores == nil {
		f.restores = map[string]instancemanager.RestoreRequest{}
	}
	f.restores[podName] = req
	if f.restoresByKey == nil {
		f.restoresByKey = map[string]instancemanager.RestoreRequest{}
	}
	if _, held := f.restoresByKey[key]; !held {
		f.restoresByKey[key] = req
	}
	return f.restoreOp(podName, key), nil
}

func (f *fakeSeedClient) GetOperation(_ context.Context, _, _, _ string) (instancemanager.Operation, error) {
	return instancemanager.Operation{}, errors.New("not used by seeding")
}

func (f *fakeSeedClient) FindBackupOperation(_ context.Context, _, _, key string) (instancemanager.Operation, bool, error) {
	f.lookups++
	held, ok := f.backups[key]
	if !ok {
		return instancemanager.Operation{}, false, nil
	}
	state := f.backupState
	if s, ok := f.backupStates[held.member]; ok {
		state = s
	}
	return instancemanager.Operation{ID: "op-seed-backup", Kind: instancemanager.OpBackup, State: state, FailureReason: f.reason}, true, nil
}

func (f *fakeSeedClient) FindOperation(_ context.Context, podName, _ string, kind instancemanager.OperationKind, key string) (instancemanager.Operation, bool, error) {
	f.lookups++
	f.lookupKeys = append(f.lookupKeys, key)
	if f.findErr != nil {
		return instancemanager.Operation{}, false, f.findErr
	}
	if _, held := f.restoresByKey[key]; kind != instancemanager.OpRestore || !held {
		return instancemanager.Operation{}, false, nil
	}
	return f.restoreOp(podName, key), true, nil
}

// imIdempotencyConflict is what the client makes of an Instance Manager's
// 409 for a key sent again with another request.
func imIdempotencyConflict() error {
	return unexpectedStatus(&http.Response{StatusCode: http.StatusConflict, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"error":"idempotency key reused with a different request","code":"IdempotencyConflict"}`))})
}

// firstMemberMaster answers as the members of a group that is being formed
// without a failover: the first member is the master, with a server that is
// not active before every member has joined, and the others are slaves.
type firstMemberMaster struct{}

func (firstMemberMaster) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	o := answersSlave
	if strings.HasSuffix(podName, "-0") {
		o = answersMasterNotYetActive
	}
	o.ObservedAt = time.Now()
	return o
}

// recoverySeedClient is a fakeSeedClient that also answers the poll of the
// recovery restore on the first member.
type recoverySeedClient struct {
	*fakeSeedClient
	recovered instancemanager.OperationState
}

func newRecoverySeedClient() *recoverySeedClient {
	return &recoverySeedClient{fakeSeedClient: &fakeSeedClient{restore: map[string]instancemanager.OperationState{}}}
}

func (f *recoverySeedClient) GetOperation(_ context.Context, _, _, id string) (instancemanager.Operation, error) {
	return instancemanager.Operation{ID: id, State: f.recovered}, nil
}

var _ = Describe("HA bootstrap of the first database (ADR-0010, #106)", func() {
	ctx := context.Background()

	create := func(c *databasev1alpha1.CubridCluster) types.NamespacedName {
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		return types.NamespacedName{Name: c.Name, Namespace: c.Namespace}
	}
	reconcilerWith := func(boot HABootstrapClient) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			Prober: &memberProber{}, HABootstrap: boot,
		}
	}
	reconcileOnce := func(r *CubridClusterReconciler, key types.NamespacedName) *databasev1alpha1.CubridCluster {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		return got
	}

	It("gives every HA member the cluster's cubrid_ha.conf", func() {
		key := create(haCluster("boot-mount"))
		reconcileOnce(reconcilerWith(nil), key)

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		spec := sts.Spec.Template.Spec
		Expect(spec.Volumes).To(HaveLen(1))
		Expect(spec.Volumes[0].ConfigMap).NotTo(BeNil())
		Expect(spec.Volumes[0].ConfigMap.Name).To(Equal(haConfigMapName("boot-mount")))
		c := spec.Containers[0]
		Expect(c.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name: spec.Volumes[0].Name, MountPath: "/etc/cubrid-ha", ReadOnly: true,
		}))
		conf, ok := envValue(c, "CUBRID_HA_CONF")
		Expect(ok).To(BeTrue())
		Expect(conf.Value).To(Equal("/etc/cubrid-ha/cubrid_ha.conf"))
	})

	It("mounts no HA configuration into a standalone member and does not bootstrap it", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpCompleted}
		single := standaloneCluster("boot-single")
		key := create(single)
		got := reconcileOnce(reconcilerWith(boot), key)

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.Volumes).To(BeEmpty())
		Expect(boot.members).To(BeEmpty())
		Expect(got.Status.Databases).To(BeEmpty())
	})

	It("has the database created on one member only and records it once", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpCreating}
		key := create(haCluster("boot-create"))
		r := reconcilerWith(boot)

		got := reconcileOnce(r, key)
		Expect(got.Status.Databases).To(HaveLen(1))
		Expect(got.Status.Databases[0].Phase).To(Equal("Creating"))
		Expect(got.Status.Databases[0].PrimaryCreated).To(BeFalse())

		boot.state = instancemanager.OpCompleted
		got = reconcileOnce(r, key)
		Expect(got.Status.Databases[0].Phase).To(Equal("Created"))
		Expect(got.Status.Databases[0].PrimaryCreated).To(BeTrue())
		Expect(got.Status.Databases[0].HAConfigured).To(BeFalse(), "the peers are not seeded yet")
		cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(BeEquivalentTo("False"))
		Expect(cond.Reason).To(Equal("PeersNotSeeded"))

		By("asking only the first member, always with the same key")
		Expect(boot.members).To(HaveEach("boot-create-0"))
		Expect(boot.keys).To(HaveEach(boot.keys[0]))

		By("not asking again once the database is recorded")
		asked := len(boot.members)
		reconcileOnce(r, key)
		Expect(boot.members).To(HaveLen(asked))
	})

	It("starts a failed creation again a limited number of times, then stops (#107)", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpFailed, reason: "createdb failed: no space left"}
		key := create(haCluster("boot-failed"))
		r := reconcilerWith(boot)

		By("retrying under a new key while attempts remain")
		got := reconcileOnce(r, key)
		Expect(got.Status.Databases[0].Phase).NotTo(Equal("Failed"))
		Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(1))
		cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("DatabaseCreationFailedRetrying"))
		Expect(cond.Message).To(ContainSubstring("no space left"))

		got = reconcileOnce(r, key)
		Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(2))

		By("giving up at the limit")
		got = reconcileOnce(r, key)
		Expect(got.Status.Databases[0].Phase).To(Equal("Failed"))
		Expect(got.Status.Databases[0].PrimaryCreated).To(BeFalse())
		cond = meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
		Expect(cond.Reason).To(Equal("DatabaseCreationFailed"))
		Expect(cond.Message).To(ContainSubstring("giving up after 3 attempts"))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionReady)).To(BeFalse())

		By("using a different operation for every attempt, and asking no more afterwards")
		Expect(boot.keys).To(HaveLen(maxBootstrapAttempts))
		Expect(boot.keys[0]).NotTo(Equal(boot.keys[1]))
		Expect(boot.keys[1]).NotTo(Equal(boot.keys[2]))
		reconcileOnce(r, key)
		Expect(boot.keys).To(HaveLen(maxBootstrapAttempts))
	})

	It("addresses the same operation again after the operator restarted (#107)", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpCreating}
		key := create(haCluster("boot-restart"))
		reconcileOnce(reconcilerWith(boot), key)

		By("a new reconciler, as after an operator restart")
		again := &fakeHABootstrapClient{state: instancemanager.OpCreating}
		reconcileOnce(reconcilerWith(again), key)
		Expect(again.keys).To(Equal(boot.keys), "the running operation must be found, not a second one started")
		Expect(again.members).To(Equal(boot.members))
	})

	It("keeps asking while the member cannot be reached", func() {
		boot := &fakeHABootstrapClient{err: errors.New("connection refused")}
		key := create(haCluster("boot-unreachable"))
		r := reconcilerWith(boot)
		got := reconcileOnce(r, key)
		Expect(got.Status.Databases[0].Phase).To(Equal("Pending"))
		reconcileOnce(r, key)
		Expect(boot.members).To(HaveLen(2))
	})

	It("counts a creation the member refuses as invalid as a failed attempt, and stops (#302)", func() {
		boot := &fakeHABootstrapClient{err: imAnswer(http.StatusBadRequest, `{"error":"database name is not a plain identifier"}`)}
		key := create(haCluster("boot-invalid"))
		r := reconcilerWith(boot)
		var got *databasev1alpha1.CubridCluster
		for range maxBootstrapAttempts {
			got = reconcileOnce(r, key)
		}
		Expect(got.Status.Databases[0].Phase).To(Equal(databasePhaseFailed))
		cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
		Expect(cond.Reason).To(Equal("DatabaseCreationFailed"))
		Expect(cond.Message).To(ContainSubstring("not a plain identifier"))
		reconcileOnce(r, key)
		Expect(boot.members).To(HaveLen(maxBootstrapAttempts))
	})

	It("waits on a member that answers as no HA member, without using up an attempt (#302)", func() {
		boot := &fakeHABootstrapClient{err: imAnswer(http.StatusConflict,
			`{"error":"this member is a standalone server, not an HA member","code":"NotAnHAMember"}`)}
		key := create(haCluster("boot-not-ha"))
		r := reconcilerWith(boot)
		reconcileOnce(r, key)
		got := reconcileOnce(r, key)
		Expect(got.Status.Databases[0].BootstrapAttempts).To(BeZero())
		Expect(meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady).Reason).To(Equal(reasonNotAnHAMember))
	})

	Context("seeding the other members", func() {
		seededCluster := func(name string) *databasev1alpha1.CubridCluster {
			c := haCluster(name)
			c.Spec.ObjectStorage = testObjectStorage()
			c.Spec.ObjectStorage.Bucket = "cluster-artifacts"
			c.Spec.ObjectStorage.Prefix = "prod"
			return c
		}
		reconcilerFor := func(seed *fakeSeedClient) *CubridClusterReconciler {
			r := reconcilerWith(&fakeHABootstrapClient{state: instancemanager.OpCompleted})
			r.Backup, r.Restore = seed, seed
			r.Prober = firstMemberMaster{}
			return r
		}
		bootstrapCondition := func(c *databasev1alpha1.CubridCluster) (string, string) {
			cond := meta.FindStatusCondition(c.Status.Conditions, conditionBootstrapReady)
			Expect(cond).NotTo(BeNil())
			return string(cond.Status), cond.Reason
		}

		// The API refuses an HA cluster without a bucket (#285). A cluster
		// that an earlier version admitted can still be without one; it is
		// not stored here, because the API server would refuse it too.
		It("needs the cluster's object storage with a bucket", func() {
			seed := &fakeSeedClient{}
			admittedEarlier := haCluster("seed-nostore")
			admittedEarlier.Spec.ObjectStorage = nil
			status := databaseStatus(admittedEarlier, "appdb")
			reconcilerFor(seed).reconcileSeeding(ctx, admittedEarlier, status, "seed-nostore-0")
			_, reason := bootstrapCondition(admittedEarlier)
			Expect(reason).To(Equal("SeedStorageNotConfigured"))
			Expect(seed.backupOn).To(BeEmpty())
			Expect(status.HAConfigured).To(BeFalse())
		})

		// The prefix of spec.objectStorage has no rule in the API, and the
		// Instance Manager refuses an upload whose prefix is not a clean
		// relative key for good (#301). The seed backup is sent with such a
		// key, or, for a prefix that leads above the bucket, not at all.
		It("sends a seed backup whose prefix the Instance Manager accepts", func() {
			for prefix, want := range map[string]string{
				"/prod":    "prod/uid-seed/seed/appdb",
				"//prod/a": "prod/a/uid-seed/seed/appdb",
				"a/../b":   "b/uid-seed/seed/appdb",
				"":         "uid-seed/seed/appdb",
			} {
				seed := &fakeSeedClient{backupState: instancemanager.OpRunningBackup}
				c := seededCluster("seed-prefix")
				c.UID = "uid-seed"
				c.Spec.ObjectStorage.Prefix = prefix
				status := databaseStatus(c, "appdb")
				reconcilerFor(seed).reconcileSeeding(ctx, c, status, "seed-prefix-0")
				_, reason := bootstrapCondition(c)
				Expect(reason).To(Equal("SeedBackupInProgress"), "prefix %q", prefix)
				Expect(seed.backupReq.Upload).NotTo(BeNil())
				Expect(seed.backupReq.Upload.Prefix).To(Equal(want), "prefix %q", prefix)
				Expect(seed.backupReq.Upload.Seed).To(BeTrue())
				passed, answer := instanceManagerChecks(GinkgoT().TempDir(), seed.backupReq)
				Expect(passed).To(BeTrue(), "prefix %q: the Instance Manager answers %s", prefix, answer)
			}
		})

		It("stops with a condition for a prefix that leads above the bucket", func() {
			for _, prefix := range []string{"..", "../prod", "/../prod", "a/../.."} {
				seed := &fakeSeedClient{backupState: instancemanager.OpRunningBackup}
				c := seededCluster("seed-escape")
				c.Spec.ObjectStorage.Prefix = prefix
				status := databaseStatus(c, "appdb")
				reconcilerFor(seed).reconcileSeeding(ctx, c, status, "seed-escape-0")
				_, reason := bootstrapCondition(c)
				Expect(reason).To(Equal("SeedStoragePrefixInvalid"), "prefix %q", prefix)
				Expect(seed.backupOn).To(BeEmpty(), "prefix %q", prefix)
				Expect(status.HAConfigured).To(BeFalse())
			}
		})

		It("backs up the first member, then restores the peers one at a time", func() {
			seed := &fakeSeedClient{
				backupState: instancemanager.OpRunningBackup,
				restore:     map[string]instancemanager.OperationState{},
			}
			c := seededCluster("seed-order")
			key := create(c)
			r := reconcilerFor(seed)

			By("waiting for the seed backup before any restore")
			got := reconcileOnce(r, key)
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedBackupInProgress"))
			Expect(seed.backupOn).To(HaveEach("seed-order-0"))
			Expect(seed.restoreOn).To(BeEmpty())
			Expect(seed.backupReq.Upload).NotTo(BeNil())
			Expect(seed.backupReq.Upload.Bucket).To(Equal("cluster-artifacts"))
			Expect(seed.backupReq.Upload.Prefix).To(Equal("prod/" + string(got.UID) + "/seed/appdb"))
			Expect(seed.backupReq.Destination).To(HavePrefix(backupStagingRoot + "/"))

			By("restoring the first peer only, while it is in progress")
			seed.backupState = instancemanager.OpCompleted
			seed.restore["seed-order-1"] = instancemanager.OpRestoring
			got = reconcileOnce(r, key)
			_, reason = bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreInProgress"))
			Expect(seed.restoreOn).To(HaveEach("seed-order-1"))
			Expect(seed.restoreReq.Bucket).To(Equal("cluster-artifacts"))
			Expect(seed.restoreReq.Prefix).To(Equal(seed.backupReq.Upload.Prefix))
			Expect(seed.restoreReq.TargetDir).To(Equal(restoreTargetRoot))
			Expect(seed.restoreReq.SeedFromMaster).To(Equal("seed-order-0"),
				"a seeded member is restored as a slave of the member the backup came from")
			Expect(got.Status.Databases[0].HAConfigured).To(BeFalse())

			By("moving to the second peer when the first is done, once its source is recorded")
			seed.restore["seed-order-1"] = instancemanager.OpCompleted
			seed.restore["seed-order-2"] = instancemanager.OpRestoring
			seed.restoreOn = nil
			got = reconcileOnce(r, key)
			Expect(seed.restoreOn).To(Equal([]string{"seed-order-1"}))
			Expect(got.Status.Databases[0].Seeding).To(Equal(
				&databasev1alpha1.DatabaseSeeding{Member: "seed-order-2", Source: "seed-order-0"}))
			got = reconcileOnce(r, key)
			Expect(seed.restoreOn).To(Equal([]string{"seed-order-1", "seed-order-2"}))
			Expect(seed.restoreReq.SeedFromMaster).To(Equal("seed-order-0"))
			Expect(seed.restoreReq.Prefix).To(Equal("prod/" + string(got.UID) + "/seed-seed-order-2-from-seed-order-0/appdb"))
			Expect(got.Status.Databases[0].HAConfigured).To(BeFalse())

			By("recording the bootstrap as complete when every peer is seeded")
			seed.restore["seed-order-2"] = instancemanager.OpCompleted
			got = reconcileOnce(r, key)
			Expect(got.Status.Databases[0].HAConfigured).To(BeTrue())
			status, reason := bootstrapCondition(got)
			Expect(status).To(Equal("True"))
			Expect(reason).To(Equal("PeersSeeded"))

			By("asking for nothing more afterwards")
			seed.backupOn, seed.restoreOn = nil, nil
			reconcileOnce(r, key)
			Expect(seed.backupOn).To(BeEmpty())
			Expect(seed.restoreOn).To(BeEmpty())
		})

		// A key names one request. When the spec changes while the step of a
		// key runs, the member refuses the changed request under that key; the
		// operation the key names is followed instead, and the change applies
		// from the next step on (#302).
		It("follows the seed backup its key names when the spec changed meanwhile, and goes on", func() {
			seed := &fakeSeedClient{
				backupState: instancemanager.OpRunningBackup,
				restore:     map[string]instancemanager.OperationState{},
				heldKeys:    true,
			}
			key := create(seededCluster("seed-changed"))
			r := reconcilerFor(seed)
			got := reconcileOnce(r, key)
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedBackupInProgress"))
			Expect(seed.backups).To(HaveLen(1))

			By("a spec.version that the running backup was not started with")
			got.Spec.Version = cubridVersion + ".1"
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			for range 2 {
				got = reconcileOnce(r, key)
				_, reason = bootstrapCondition(got)
				Expect(reason).To(Equal("SeedBackupInProgress"), "the operation the key names is followed")
			}
			Expect(seed.lookups).To(Equal(2))
			Expect(seed.backupOn).To(HaveLen(3), "one start for each reconcile, no more")
			Expect(seed.backups).To(HaveLen(1))
			for _, b := range seed.backups {
				Expect(b.req.Upload.CubridVersion).To(Equal(cubridVersion), "the first request stays the one of its key")
			}

			By("the backup completing: the restore is started with the changed spec")
			seed.backupState = instancemanager.OpCompleted
			seed.restore["seed-changed-1"] = instancemanager.OpRestoring
			got = reconcileOnce(r, key)
			_, reason = bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreInProgress"))
			Expect(seed.restoreReq.ExpectedCubridVersion).To(Equal(cubridVersion + ".1"))
		})

		It("adopts a seed backup recorded before explicit seed purpose was sent", func() {
			seed := &fakeSeedClient{backupState: instancemanager.OpRunningBackup,
				restore: map[string]instancemanager.OperationState{}, heldKeys: true}
			key := create(seededCluster("seed-legacy"))
			r := reconcilerFor(seed)
			reconcileOnce(r, key)
			Expect(seed.backups).To(HaveLen(1))
			for k, b := range seed.backups {
				b.req.Upload.Seed = false
				seed.backups[k] = b
			}
			got := reconcileOnce(r, key)
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedBackupInProgress"))
			Expect(seed.lookups).To(Equal(1))
			Expect(seed.backups).To(HaveLen(1))
			seed.backupState = instancemanager.OpCompleted
			seed.restore["seed-legacy-1"] = instancemanager.OpRestoring
			got = reconcileOnce(r, key)
			_, reason = bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreInProgress"))
			Expect(seed.restoreReq.Prefix).To(Equal("prod/" + string(got.UID) + "/seed/appdb"))
		})

		// The restore of a later member is started from the master that was
		// observed; when the write that recorded its operation is lost and
		// another member is master by the next reconcile, the next round's
		// restore has a key of its own, so the operation from the earlier
		// source is never taken for it (ADR-0005, #302).
		It("does not take an earlier source's restore for a later round after a lost status write", func() {
			const n = "seed-lostwrite"
			seed := &fakeSeedClient{
				backupState:  instancemanager.OpCompleted,
				restore:      map[string]instancemanager.OperationState{n + "-1": instancemanager.OpCompleted, n + "-2": instancemanager.OpRestoring},
				heldKeys:     true,
				restoreByKey: map[string]instancemanager.OperationState{},
			}
			key := create(seededCluster(n))
			r := reconcilerFor(seed)
			prober := &memberProber{master: n + "-0"}
			r.Prober = prober

			got := reconcileOnce(r, key)
			Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{n + "-1"}))
			got = reconcileOnce(r, key)
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreInProgress"))
			Expect(seed.restores[n+"-2"].SeedFromMaster).To(Equal(n + "-0"))
			keyFromA := seed.restoreKeys[n+"-2"]

			By("the write of the restore's operation being lost, that restore completing, and " + n + "-1 becoming the master")
			got.Status.Databases[0].Seeding.RestoreOperationID = ""
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
			seed.restoreByKey[keyFromA] = instancemanager.OpCompleted
			prober.master = n + "-1"
			got = reconcileOnce(r, key)
			Expect(got.Status.Databases[0].Seeding).To(Equal(
				&databasev1alpha1.DatabaseSeeding{Member: n + "-2", Source: n + "-1", Round: 1}))

			lookups := seed.lookups
			got = reconcileOnce(r, key)
			Expect(got.Status.Databases[0].SeededMembers).NotTo(ContainElement(n+"-2"),
				"the member was recorded as seeded from the earlier source's restore")
			_, reason = bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreInProgress"))
			Expect(seed.restoreKeys[n+"-2"]).NotTo(Equal(keyFromA))
			Expect(seed.restores[n+"-2"].SeedFromMaster).To(Equal(n + "-1"))
			Expect(seed.lookups).To(Equal(lookups), "the earlier source's restore was looked up")
		})

		// An operation that is followed instead of a changed request is taken
		// as the step's only when what it restored is the backup of the step
		// (#302).
		It("does not record a member seeded by a followed restore of another backup", func() {
			const n = "seed-mismatch"
			seed := &fakeSeedClient{
				backupState:  instancemanager.OpCompleted,
				restore:      map[string]instancemanager.OperationState{n + "-1": instancemanager.OpRestoring},
				heldKeys:     true,
				restoreByKey: map[string]instancemanager.OperationState{},
				restoredFrom: map[string]string{},
			}
			key := create(seededCluster(n))
			r := reconcilerFor(seed)
			got := reconcileOnce(r, key)
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreInProgress"))
			restoreKey := seed.restoreKeys[n+"-1"]

			By("the spec changing while the restore runs, which reports another backup when it completes")
			got.Spec.Version = cubridVersion + ".1"
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			seed.restoreByKey[restoreKey] = instancemanager.OpCompleted
			base := "s3://cluster-artifacts/prod/" + string(got.UID)
			seed.restoredFrom[restoreKey] = base + "/seed-" + n + "-2-from-" + n + "-0/appdb/manifest.json"
			got = reconcileOnce(r, key)
			Expect(got.Status.Databases[0].SeededMembers).To(BeEmpty())
			_, reason = bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreSourceMismatch"))

			By("the restore reporting the backup of the step")
			seed.restoredFrom[restoreKey] = base + "/seed/appdb/manifest.json"
			got = reconcileOnce(r, key)
			Expect(got.Status.Databases[0].SeededMembers).To(ContainElement(n + "-1"))
		})

		It("starts a failed restore again without touching a seeded member (#107)", func() {
			const seeded, failing = "seed-retry-1", "seed-retry-2"
			seed := &fakeSeedClient{
				backupState: instancemanager.OpCompleted,
				restore: map[string]instancemanager.OperationState{
					seeded:  instancemanager.OpCompleted,
					failing: instancemanager.OpFailed,
				},
				reason: "manager restarted while operation was in progress",
			}
			key := create(seededCluster("seed-retry"))
			r := reconcilerFor(seed)

			reconcileOnce(r, key) // seeds the first peer and records the source of the second
			got := reconcileOnce(r, key)
			Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{seeded}))
			Expect(got.Status.Databases[0].Phase).NotTo(Equal("Failed"))
			Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(1))
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreFailedRetrying"))
			firstKey := seed.restoreKeys[failing]

			By("the next attempt restores only the member that is not seeded, under a new key")
			seed.restore[failing] = instancemanager.OpCompleted
			seed.restoreOn = nil
			reconcileOnce(r, key) // records the source of the new attempt
			got = reconcileOnce(r, key)
			Expect(seed.restoreOn).To(Equal([]string{failing}), "a seeded member must not be restored over")
			Expect(seed.restoreKeys[failing]).NotTo(Equal(firstKey))
			Expect(got.Status.Databases[0].HAConfigured).To(BeTrue())
			Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{seeded, failing}))
		})

		// A seed restore that failed after restoredb leaves whole data, which
		// the next attempt of the same request takes over and only starts
		// (ADR-0008). A seed backup taken for that attempt would never be
		// restored, so none is taken (#463).
		Context("retrying a seed restore (#463)", func() {
			const m = instancemanager.ManifestObjectName
			// seedBackupsAt counts the seed backups the fake was asked for that
			// upload below prefix.
			seedBackupsAt := func(seed *fakeSeedClient, prefix string) int {
				n := 0
				for _, b := range seed.backups {
					if b.req.Upload != nil && b.req.Upload.Prefix == prefix {
						n++
					}
				}
				return n
			}

			It("takes no new seed backup when the retry takes over what the earlier restore of it restored", func() {
				const n = "seed-reuse"
				seed := &fakeSeedClient{
					backupState:  instancemanager.OpCompleted,
					restore:      map[string]instancemanager.OperationState{n + "-1": instancemanager.OpFailed, n + "-2": instancemanager.OpRestoring},
					restoreByKey: map[string]instancemanager.OperationState{},
					restoredFrom: map[string]string{},
					reason:       "heartbeat reports the role unknown",
				}
				key := create(seededCluster(n))
				r := reconcilerFor(seed)
				stored := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
				uid := string(stored.UID)
				prefix := "prod/" + uid + "/seed/appdb"
				firstKey := fmt.Sprintf("seed-restore-%s-appdb-a0-%s-1", uid, n)
				seed.restoredFrom[firstKey] = "s3://cluster-artifacts/" + prefix + "/" + m
				seed.restoreByKey[firstKey] = instancemanager.OpFailed

				got := reconcileOnce(r, key)
				Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(1))
				_, reason := bootstrapCondition(got)
				Expect(reason).To(Equal("SeedRestoreFailedRetrying"))
				Expect(seedBackupsAt(seed, prefix)).To(Equal(1))
				firstReq := seed.restoresByKey[firstKey]

				By("the retry restoring the same backup with the same request, without a new backup")
				seed.restore[n+"-1"] = instancemanager.OpRestoring
				reconcileOnce(r, key)
				Expect(seedBackupsAt(seed, prefix)).To(Equal(1), "the retry took a seed backup that it does not restore")
				Expect(seed.restoreKeys[n+"-1"]).NotTo(Equal(firstKey))
				Expect(seed.restores[n+"-1"]).To(Equal(firstReq),
					"the retry must send the earlier request, which the Instance Manager takes over")

				By("looking up only the retry's own restore while it runs")
				seed.lookupKeys = nil
				reconcileOnce(r, key)
				Expect(seed.lookupKeys).To(Equal([]string{seed.restoreKeys[n+"-1"]}))
				Expect(seedBackupsAt(seed, prefix)).To(Equal(1))

				seed.restore[n+"-1"] = instancemanager.OpCompleted
				got = reconcileOnce(r, key)
				Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{n + "-1"}))
			})

			It("looks nothing up on the first attempt", func() {
				const n = "seed-firstattempt"
				seed := &fakeSeedClient{
					backupState: instancemanager.OpCompleted,
					restore:     map[string]instancemanager.OperationState{n + "-1": instancemanager.OpRestoring},
				}
				key := create(seededCluster(n))
				r := reconcilerFor(seed)
				for range 2 {
					got := reconcileOnce(r, key)
					_, reason := bootstrapCondition(got)
					Expect(reason).To(Equal("SeedRestoreInProgress"))
				}
				Expect(seed.lookups).To(BeZero())
			})

			It("takes a new backup when the latest earlier restore failed without restoring, though an older one restored", func() {
				const n = "seed-latest"
				seed := &fakeSeedClient{
					backupState:  instancemanager.OpCompleted,
					restore:      map[string]instancemanager.OperationState{n + "-1": instancemanager.OpFailed},
					restoreByKey: map[string]instancemanager.OperationState{},
					restoredFrom: map[string]string{},
				}
				key := create(seededCluster(n))
				r := reconcilerFor(seed)
				stored := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
				uid := string(stored.UID)
				prefix := "prod/" + uid + "/seed/appdb"
				firstKey := fmt.Sprintf("seed-restore-%s-appdb-a0-%s-1", uid, n)
				seed.restoredFrom[firstKey] = "s3://cluster-artifacts/" + prefix + "/" + m

				reconcileOnce(r, key) // a0 restores, then fails
				got := reconcileOnce(r, key)
				Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(2))
				Expect(seedBackupsAt(seed, prefix)).To(Equal(1), "a1 skips the backup, then fails before it restores anything")

				seed.restoreByKey[firstKey] = instancemanager.OpFailed
				seed.restoreByKey[seed.restoreKeys[n+"-1"]] = instancemanager.OpFailed
				seed.restore[n+"-1"] = instancemanager.OpRestoring
				reconcileOnce(r, key)
				Expect(seedBackupsAt(seed, prefix)).To(Equal(2), "the latest earlier restore, a1, decides")
			})

			It("takes a new backup when the retry's request differs from the earlier restore's", func() {
				const n = "seed-hash"
				seed := &fakeSeedClient{
					backupState:  instancemanager.OpCompleted,
					restore:      map[string]instancemanager.OperationState{n + "-1": instancemanager.OpFailed},
					restoreByKey: map[string]instancemanager.OperationState{},
					restoredFrom: map[string]string{},
				}
				key := create(seededCluster(n))
				r := reconcilerFor(seed)
				stored := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
				uid := string(stored.UID)
				prefix := "prod/" + uid + "/seed/appdb"
				firstKey := fmt.Sprintf("seed-restore-%s-appdb-a0-%s-1", uid, n)
				seed.restoredFrom[firstKey] = "s3://cluster-artifacts/" + prefix + "/" + m
				seed.restoreByKey[firstKey] = instancemanager.OpFailed
				got := reconcileOnce(r, key)
				Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(1))

				By("a spec.version that the earlier restore was not started with")
				got.Spec.Version = cubridVersion + ".1"
				Expect(k8sClient.Update(ctx, got)).To(Succeed())
				seed.restore[n+"-1"] = instancemanager.OpRestoring
				reconcileOnce(r, key)
				Expect(seedBackupsAt(seed, prefix)).To(Equal(2))
				Expect(seed.restores[n+"-1"].ExpectedCubridVersion).To(Equal(cubridVersion + ".1"))
			})

			It("does not take another member's earlier restore for a member's step", func() {
				const n = "seed-othermember"
				seed := &fakeSeedClient{
					backupState:  instancemanager.OpCompleted,
					restore:      map[string]instancemanager.OperationState{n + "-1": instancemanager.OpFailed, n + "-2": instancemanager.OpRestoring},
					restoreByKey: map[string]instancemanager.OperationState{},
					restoredFrom: map[string]string{},
				}
				key := create(seededCluster(n))
				r := reconcilerFor(seed)
				stored := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
				uid := string(stored.UID)
				firstKey := fmt.Sprintf("seed-restore-%s-appdb-a0-%s-1", uid, n)
				seed.restoredFrom[firstKey] = "s3://cluster-artifacts/prod/" + uid + "/seed/appdb/" + m
				seed.restoreByKey[firstKey] = instancemanager.OpFailed
				reconcileOnce(r, key)

				By(n + "-1 being seeded by the retry, then " + n + "-2 being copied for the first time in attempt 1")
				seed.restore[n+"-1"] = instancemanager.OpCompleted
				seed.lookupKeys = nil
				got := reconcileOnce(r, key)
				Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{n + "-1"}))
				artifact := n + "-2-from-" + n + "-0"
				Expect(seedBackupsAt(seed, "prod/"+uid+"/seed-"+artifact+"/appdb")).To(Equal(1),
					"the step of "+n+"-2 has no earlier restore of its own")
				base := "seed-restore-" + uid + "-appdb-"
				Expect(seed.lookupKeys).To(Equal([]string{
					base + "a1-" + n + "-1", firstKey, // the step of -1: its own restore, then a0's
					base + "a1-" + artifact, base + "a0-" + artifact, // the step of -2: never the restore of -1
				}))
			})

			It("takes a new seed backup when the earlier restore failed before it restored anything", func() {
				const n = "seed-nodata"
				seed := &fakeSeedClient{
					backupState: instancemanager.OpCompleted,
					restore:     map[string]instancemanager.OperationState{n + "-1": instancemanager.OpFailed},
					reason:      "download failed",
				}
				key := create(seededCluster(n))
				r := reconcilerFor(seed)
				got := reconcileOnce(r, key)
				prefix := "prod/" + string(got.UID) + "/seed/appdb"
				Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(1))

				By("the retry backing up again: its restore downloads the new backup from the same place")
				seed.restore[n+"-1"] = instancemanager.OpRestoring
				reconcileOnce(r, key)
				Expect(seedBackupsAt(seed, prefix)).To(Equal(2))
				Expect(seed.restores[n+"-1"].Prefix).To(Equal(prefix))
			})

			It("waits, without a new backup, while the earlier restore cannot be looked up", func() {
				const n = "seed-nolookup"
				seed := &fakeSeedClient{
					backupState: instancemanager.OpCompleted,
					restore:     map[string]instancemanager.OperationState{n + "-1": instancemanager.OpFailed},
				}
				key := create(seededCluster(n))
				r := reconcilerFor(seed)
				got := reconcileOnce(r, key)
				prefix := "prod/" + string(got.UID) + "/seed/appdb"

				seed.findErr = errors.New("connection refused")
				seed.restoreOn = nil
				got = reconcileOnce(r, key)
				Expect(seedBackupsAt(seed, prefix)).To(Equal(1))
				Expect(seed.restoreOn).To(BeEmpty())
				_, reason := bootstrapCondition(got)
				Expect(reason).To(Equal(reasonInstanceManagerUnavailable))
			})

			It("takes no new backup of a later member's source when the retry observes the same master", func() {
				const n = "seed-reuse-later"
				seed := &fakeSeedClient{
					backupState:  instancemanager.OpCompleted,
					restore:      map[string]instancemanager.OperationState{n + "-1": instancemanager.OpCompleted, n + "-2": instancemanager.OpFailed},
					restoreByKey: map[string]instancemanager.OperationState{},
					restoredFrom: map[string]string{},
				}
				key := create(seededCluster(n))
				r := reconcilerFor(seed)
				stored := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
				uid := string(stored.UID)
				artifact := n + "-2-from-" + n + "-0"
				prefix := "prod/" + uid + "/seed-" + artifact + "/appdb"
				firstKey := fmt.Sprintf("seed-restore-%s-appdb-a0-%s", uid, artifact)
				seed.restoredFrom[firstKey] = "s3://cluster-artifacts/" + prefix + "/" + m
				seed.restoreByKey[firstKey] = instancemanager.OpFailed

				reconcileOnce(r, key) // seeds the first peer and records the source of the second
				got := reconcileOnce(r, key)
				Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(1))
				Expect(seedBackupsAt(seed, prefix)).To(Equal(1))
				firstReq := seed.restoresByKey[firstKey]

				By("the retry recording the source it observes before it restores, without a new backup")
				seed.restore[n+"-2"] = instancemanager.OpCompleted
				seed.restoreOn = nil
				got = reconcileOnce(r, key)
				Expect(seed.restoreOn).To(BeEmpty(), "the restore waits until the source is recorded")
				_, reason := bootstrapCondition(got)
				Expect(reason).To(Equal("SeedSourceSelected"))
				got = reconcileOnce(r, key)
				Expect(seedBackupsAt(seed, prefix)).To(Equal(1), "the retry took a seed backup that it does not restore")
				Expect(seed.restores[n+"-2"]).To(Equal(firstReq))
				Expect(got.Status.Databases[0].SeededMembers).To(ContainElement(n + "-2"))
			})

			It("takes a new backup on another master: the earlier backup is of another source", func() {
				const n = "seed-newsource"
				seed := &fakeSeedClient{
					backupState:  instancemanager.OpCompleted,
					restore:      map[string]instancemanager.OperationState{n + "-1": instancemanager.OpCompleted, n + "-2": instancemanager.OpFailed},
					restoreByKey: map[string]instancemanager.OperationState{},
					restoredFrom: map[string]string{},
				}
				key := create(seededCluster(n))
				r := reconcilerFor(seed)
				prober := &memberProber{master: n + "-0"}
				r.Prober = prober
				stored := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
				uid := string(stored.UID)
				artifact := n + "-2-from-" + n + "-0"
				firstKey := fmt.Sprintf("seed-restore-%s-appdb-a0-%s", uid, artifact)
				seed.restoredFrom[firstKey] = "s3://cluster-artifacts/prod/" + uid + "/seed-" + artifact + "/appdb/" + m
				seed.restoreByKey[firstKey] = instancemanager.OpFailed

				reconcileOnce(r, key)
				reconcileOnce(r, key)

				By(n + "-1 being the master at the retry")
				prober.master = n + "-1"
				seed.backupOn = nil
				reconcileOnce(r, key)
				Expect(seed.backupOn).To(Equal([]string{n + "-1"}))
				Expect(seedBackupsAt(seed, "prod/"+uid+"/seed-"+n+"-2-from-"+n+"-1/appdb")).To(Equal(1))
			})
		})

		It("stops after the last attempt of a restore that keeps failing", func() {
			seed := &fakeSeedClient{
				backupState: instancemanager.OpCompleted,
				restore:     map[string]instancemanager.OperationState{"seed-failed-1": instancemanager.OpFailed},
				reason:      "restoredb failed: exit status 1",
			}
			key := create(seededCluster("seed-failed"))
			r := reconcilerFor(seed)
			var got *databasev1alpha1.CubridCluster
			for range maxBootstrapAttempts {
				got = reconcileOnce(r, key)
			}
			Expect(got.Status.Databases[0].Phase).To(Equal("Failed"))
			Expect(got.Status.Databases[0].HAConfigured).To(BeFalse())
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreFailed"))
			Expect(seed.restoreOn).To(HaveEach("seed-failed-1"), "the second peer is not touched")
		})
	})

	Context("seeding after a recovery bootstrap (#268)", func() {
		recoveryCluster := func(name string) types.NamespacedName {
			c := haCluster(name)
			c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
				Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
			}
			c.Spec.ObjectStorage = testObjectStorage()
			c.Spec.ObjectStorage.Bucket = "cluster-artifacts"
			return create(c)
		}
		reconcilerFor := func(seed *recoverySeedClient) *CubridClusterReconciler {
			r := reconcilerWith(&fakeHABootstrapClient{state: instancemanager.OpCompleted})
			r.Backup, r.Restore = seed, seed
			r.Prober = firstMemberMaster{}
			return r
		}
		notReady := func(r *CubridClusterReconciler, key types.NamespacedName) *databasev1alpha1.CubridCluster {
			got := reconcileOnce(r, key)
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionReady)).To(BeFalse(),
				"a recovery whose peers are not seeded must not be Ready")
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionBootstrapReady)).To(BeFalse())
			return got
		}
		peerRestores := func(seed *recoverySeedClient, restored string) []string {
			var peers []string
			for _, m := range seed.restoreOn {
				if m != restored {
					peers = append(peers, m)
				}
			}
			return peers
		}

		It("does not complete the recovery when only the restored member holds the data", func() {
			seed := newRecoverySeedClient()
			key := recoveryCluster("rseed-first")
			r := reconcilerFor(seed)

			notReady(r, key) // starts the restore
			Expect(seed.restoreOn).To(Equal([]string{"rseed-first-0"}))
			Expect(seed.restoreReq.SeedFromMaster).To(BeEmpty())

			By("the restore completes; the peers are still empty")
			seed.recovered = instancemanager.OpCompleted
			seed.backupState = instancemanager.OpRunningBackup
			got := notReady(r, key)
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapSeedingReplicas))
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
			Expect(cond.Reason).To(Equal("SeedBackupInProgress"))
			Expect(got.Status.Databases).To(HaveLen(1))
			Expect(got.Status.Databases[0].PrimaryCreated).To(BeTrue())
			Expect(got.Status.Databases[0].HAConfigured).To(BeFalse())
			Expect(seed.backupOn).To(HaveEach("rseed-first-0"), "the seed backup is taken on the restored member")
			Expect(peerRestores(seed, "rseed-first-0")).To(BeEmpty())
			ready := meta.FindStatusCondition(got.Status.Conditions, conditionReady)
			Expect(ready.Reason).To(Equal("RecoverySeedingReplicas"))
			Expect(ready.Message).NotTo(ContainSubstring("restoring from backup"))
			Expect(drain(r.Recorder.(*record.FakeRecorder))).To(ContainElement(ContainSubstring("DatabaseRestored")))

			By("saying that the database was restored, not created, on the source")
			r.Backup = nil
			got = notReady(r, key)
			cond = meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
			Expect(cond.Reason).To(Equal("PeersNotSeeded"))
			Expect(cond.Message).To(ContainSubstring("was restored on rseed-first-0"))
		})

		It("seeds both peers once from the restored member, also across an operator restart", func() {
			const restored, peer1, peer2 = "rseed-both-0", "rseed-both-1", "rseed-both-2"
			seed := newRecoverySeedClient()
			seed.recovered = instancemanager.OpCompleted
			seed.backupState = instancemanager.OpCompleted
			seed.restore[peer1] = instancemanager.OpRestoring
			key := recoveryCluster("rseed-both")
			r := reconcilerFor(seed)

			notReady(r, key) // starts the restore
			got := notReady(r, key)
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapSeedingReplicas))
			Expect(peerRestores(seed, restored)).To(Equal([]string{peer1}))
			Expect(seed.restoreReq.SeedFromMaster).To(Equal(restored))
			firstKey := seed.restoreKeys[peer1]

			By("a new reconciler, as after an operator restart, finds the same seeding step")
			again := newRecoverySeedClient()
			again.recovered, again.backupState = instancemanager.OpCompleted, instancemanager.OpCompleted
			again.restore[peer1] = instancemanager.OpCompleted
			again.restore[peer2] = instancemanager.OpRestoring
			r2 := reconcilerFor(again)
			got = notReady(r2, key)
			Expect(again.restoreOn).To(Equal([]string{peer1}))
			Expect(again.restoreKeys[peer1]).To(Equal(firstKey))
			Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{peer1}))
			Expect(got.Status.Databases[0].Seeding).To(Equal(
				&databasev1alpha1.DatabaseSeeding{Member: peer2, Source: restored}))
			notReady(r2, key)
			Expect(again.restoreOn).To(Equal([]string{peer1, peer2}),
				"the restored member is not restored again")
			Expect(again.restoreReq.SeedFromMaster).To(Equal(restored))

			By("completing the recovery once every member holds the data")
			again.restore[peer2] = instancemanager.OpCompleted
			again.restoreOn = nil
			got = reconcileOnce(r2, key)
			Expect(again.restoreOn).To(Equal([]string{peer2}), "a seeded member is never restored over")
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapComplete))
			Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{peer1, peer2}))
			Expect(got.Status.Databases[0].HAConfigured).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionBootstrapReady)).To(BeTrue())
			Expect(meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady).Message).
				To(ContainSubstring("was restored on " + restored))

			By("asking for nothing more afterwards")
			again.backupOn, again.restoreOn = nil, nil
			reconcileOnce(r2, key)
			Expect(again.backupOn).To(BeEmpty())
			Expect(again.restoreOn).To(BeEmpty())
		})

		It("ends the recovery as Failed when seeding a peer keeps failing", func() {
			seed := newRecoverySeedClient()
			seed.recovered, seed.backupState = instancemanager.OpCompleted, instancemanager.OpCompleted
			seed.restore["rseed-fail-1"] = instancemanager.OpFailed
			seed.reason = "restoreslave failed: exit status 1"
			key := recoveryCluster("rseed-fail")
			r := reconcilerFor(seed)

			notReady(r, key) // starts the restore
			var got *databasev1alpha1.CubridCluster
			for range maxBootstrapAttempts {
				got = notReady(r, key)
			}
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
			Expect(cond.Reason).To(Equal("SeedRestoreFailed"))
			for _, t := range []string{conditionReady, conditionProgressing} {
				c := meta.FindStatusCondition(got.Status.Conditions, t)
				Expect(c).NotTo(BeNil(), t)
				Expect(c.Status).To(Equal(metav1.ConditionFalse), "%s of a failed recovery (#434)", t)
				Expect(c.Reason).To(Equal(reasonBootstrapRecoveryFailed), t)
				Expect(c.Message).To(ContainSubstring(cond.Message), t)
			}

			By("starting nothing more afterwards")
			seed.backupOn, seed.restoreOn = nil, nil
			got = notReady(r, key)
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
			Expect(seed.backupOn).To(BeEmpty())
			Expect(seed.restoreOn).To(BeEmpty())
		})
	})

	It("leaves a recovery bootstrap to the restore", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpCompleted}
		c := haCluster("boot-recovery")
		c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: "s3://bucket/p/manifest.json"},
		}
		c.Spec.ObjectStorage = haObjectStorage()
		key := create(c)
		reconcileOnce(reconcilerWith(boot), key)
		Expect(boot.members).To(BeEmpty())
	})
})
