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

package installed

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	// testKindCluster is how the fixture names a deleted CubridCluster.
	testKindCluster      = "CubridCluster"
	testDeniedUID        = "denied-backup-uid"
	testDeniedClusterUID = "denied-cluster-uid"
	testDeniedSecretUID  = "denied-secret-uid"
	// testDeniedAnswer is how an S3 store answers an unknown access key.
	testDeniedAnswer = "The Access Key Id you provided does not exist in our records."
)

func testS11DeniedConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupDeniedCredentials = true
	cfg.BackupDeniedReadyLimit = 5 * time.Second
	return cfg
}

// refusingStore is the object store as the run's client sees it with the
// denied keys: a List is answered with refusal, or, when refusal is nil, as
// a store that does not check keys would.
type refusingStore struct {
	*fakeStore
	refusal error
}

func (s refusingStore) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	if s.refusal != nil {
		return nil, s.refusal
	}
	return s.fakeStore.List(ctx, bucket, prefix)
}

// s11DeniedFixture plays, after S11 complete, an Operator that forms the
// standalone cluster and takes its backup: the member's upload is refused
// at the first object, as UploadBackup's Put is when the store refuses the
// keys of its environment, and the backup ends Failed with the Instance
// Manager's reason.
type s11DeniedFixture struct {
	*s11Fixture
	// refusal is the store's answer to the denied keys; nil accepts them.
	refusal error
	// readyAt is the reading of the standalone cluster that first sees it
	// Ready; 0 never does.
	readyAt int
	// deniedPhases are the phases the fake Operator reports, one per
	// reading of the backup; the last is kept. writeObjects makes the store
	// keep what the member uploads. tamperDenied changes the status of a
	// reading; onDeniedRead runs first, and its error is the reading's.
	deniedPhases []databasev1alpha1.CubridBackupPhase
	writeObjects bool
	tamperDenied func(read int, s *databasev1alpha1.CubridBackupStatus)
	onDeniedRead func(read int) error
	// deleteErr answers the deletion of an object of the kind named.
	deleteErr map[string]error

	denied        *databasev1alpha1.CubridBackup
	deniedCluster *databasev1alpha1.CubridCluster
	deniedReads   int
	clusterReads  int
	// keys are the keys the run opened the store with other than the seed
	// Secret's; deleted are the objects the run deleted, as kind/name/UID.
	keys    []string
	deleted []string
}

func newS11DeniedFixture(t *testing.T, cfg HAConfig) *s11DeniedFixture {
	t.Helper()
	f := &s11DeniedFixture{s11Fixture: newS11Fixture(t, cfg), readyAt: 2, deleteErr: map[string]error{},
		refusal: errors.New(testDeniedAnswer),
		deniedPhases: []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhasePending,
			databasev1alpha1.BackupPhaseRunning, databasev1alpha1.BackupPhaseUploading,
			databasev1alpha1.BackupPhaseFailed}}
	f.runner.openStore = func(endpoint, accessKey, secretKey string, secure bool) (objectStore, error) {
		if endpoint != testForwarded || secure {
			t.Errorf("object store client for %s, secure %t", endpoint, secure)
		}
		if accessKey == testAccessKey && secretKey == testSecretKey {
			return f.store, nil
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.keys = []string{accessKey, secretKey}
		return refusingStore{fakeStore: f.store, refusal: f.refusal}, nil
	}
	f.deniedCreated = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
		cluster.UID, cluster.Generation = testDeniedClusterUID, 1
		f.mu.Lock()
		f.deniedCluster = cluster.DeepCopy()
		f.mu.Unlock()
		return c.Create(ctx, cluster)
	}
	f.deniedRead = f.advanceCluster
	created, read := f.backupCreated, f.backupRead
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		if backup.Name != s11DeniedBackupName {
			return created(ctx, c, backup)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		backup.UID = testDeniedUID
		f.denied = backup.DeepCopy()
		return nil
	}
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name != s11DeniedBackupName {
			return read(ctx, c, key)
		}
		return f.advanceDenied(ctx, c, key)
	}
	f.backupDeleted = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup,
		opts ...client.DeleteOption) error {
		return f.deleteOwned(ctx, c, "CubridBackup", backup, opts...)
	}
	f.clusterDeleted = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster,
		opts ...client.DeleteOption) error {
		return f.deleteOwned(ctx, c, testKindCluster, cluster, opts...)
	}
	f.secretDeleted = func(ctx context.Context, c client.WithWatch, secret *corev1.Secret,
		opts ...client.DeleteOption) error {
		return f.deleteOwned(ctx, c, "Secret", secret, opts...)
	}
	f.markersOf = func(string) []string { return nil }
	return f
}

// deleteOwned records a deletion with the UID the run required, and
// answers it with deleteErr of its kind.
func (f *s11DeniedFixture) deleteOwned(ctx context.Context, c client.WithWatch, kind string, obj client.Object,
	opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	uid := ""
	if options.Preconditions != nil && options.Preconditions.UID != nil {
		uid = string(*options.Preconditions.UID)
	}
	f.mu.Lock()
	f.deleted = append(f.deleted, kind+"/"+obj.GetName()+"/"+uid)
	f.mu.Unlock()
	if err := f.deleteErr[kind]; err != nil {
		return err
	}
	return c.Delete(ctx, obj)
}

// advanceCluster writes the status of the standalone cluster, Ready from
// the readyAt-th reading, as the Operator would. The Secret the run created
// for it is given a UID first, as the API server would have.
func (f *s11DeniedFixture) advanceCluster(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, key, cluster); err != nil {
		return nil
	}
	f.mu.Lock()
	f.clusterReads++
	ready := f.readyAt > 0 && f.clusterReads >= f.readyAt
	f.mu.Unlock()
	cluster.Status.ObservedGeneration = 1
	condition := metav1.Condition{Type: clusterReadyCondition, Status: metav1.ConditionFalse,
		Reason: "InstancesNotReady", Message: "0/1 instances ready", LastTransitionTime: metav1.Now()}
	if ready {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "ClusterReady",
			"1/1 instances ready"
	}
	cluster.Status.Conditions = []metav1.Condition{condition}
	if err := c.Update(ctx, cluster); err != nil {
		f.t.Error(err)
	}
	return nil
}

// advanceDenied writes the status of the next phase of the backup with the
// denied keys, as the Operator would.
func (f *s11DeniedFixture) advanceDenied(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	backup := &databasev1alpha1.CubridBackup{}
	if err := c.Get(ctx, key, backup); err != nil {
		return nil
	}
	f.mu.Lock()
	f.deniedReads++
	read := f.deniedReads
	phase := f.deniedPhases[min(read, len(f.deniedPhases))-1]
	onRead, tamper, write := f.onDeniedRead, f.tamperDenied, f.writeObjects
	f.mu.Unlock()
	if onRead != nil {
		if err := onRead(read); err != nil {
			return err
		}
	}
	started := metav1.NewTime(time.Now().Add(-time.Second))
	dest := backup.Spec.Destination.ObjectStorage
	attempt := path.Join(dest.Prefix, testDeniedClusterUID, databaseName, string(backup.UID), testAttempt)
	s := &backup.Status
	s.Phase, s.ClusterUID, s.StartedAt = phase, testDeniedClusterUID, &started
	s.Conditions = []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse,
		Reason: testRunningReason, Message: "backup in progress", LastTransitionTime: started}}
	if phase != databasev1alpha1.BackupPhasePending {
		s.TargetInstance, s.TargetRole = s11DeniedClusterName+"-0", string(databasev1alpha1.RoleUnknown)
	}
	switch phase {
	case databasev1alpha1.BackupPhaseUploading:
		s.Conditions[0].Reason, s.Conditions[0].Message = "UploadRunning", "uploading artifact"
		if write {
			f.store.put(dest.Bucket, attempt+"/"+testFirstBackupKey, []byte("v"))
		}
	case databasev1alpha1.BackupPhaseFailed:
		s.CompletedAt = &started
		s.Conditions[0].Reason, s.Conditions[0].Message = s11MissingReason, "upload failed: put "+dest.Bucket+"/"+
			attempt+"/"+testFirstBackupKey+": "+testDeniedAnswer
	case databasev1alpha1.BackupPhaseCompleted:
		s.CompletedAt = &started
		s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: "s3://" + dest.Bucket + "/" + attempt +
			"/manifest.json", ManifestDigest: strings.Repeat("a", 64), SizeBytes: 1, Database: databaseName,
			Level: databasev1alpha1.CubridBackupFull}
		s.Conditions[0].Status, s.Conditions[0].Message = metav1.ConditionTrue, "backup completed, unexpectedly"
		if write {
			f.store.put(dest.Bucket, attempt+"/manifest.json", []byte("{}"))
		}
	}
	if tamper != nil {
		tamper(read, s)
	}
	if err := c.Update(ctx, backup); err != nil {
		f.t.Error(err)
	}
	return nil
}

// deniedOf runs the fixture and returns S11 denied-credentials, requiring
// LAB01 and S11 complete passed.
func deniedOf(t *testing.T, f *s11DeniedFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	scenarios := deniedScenarios(t, f.cfg.EvidenceDir)
	for _, s := range scenarios[:2] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01 and S11 complete passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, scenarios[1], "S11/complete")
	requireOwnEvidence(t, scenarios[2], "S11/denied-credentials")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	f.requireNoDeniedKeyInEvidence(t)
	f.requireForwardStopped(t)
	return scenarios[2], err
}

func deniedScenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s11DeniedLane || len(summary.Scenarios) != 3 || summary.Scenarios[2].ID != s11ID ||
		summary.Scenarios[2].Variant != s11DeniedVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/denied-credentials in lane %s", summary,
			s11DeniedLane)
	}
	return summary.Scenarios
}

func readDeniedRecord(t *testing.T, dir string) s11DeniedRecord {
	t.Helper()
	var rec s11DeniedRecord
	data, err := os.ReadFile(filepath.Join(dir, s11DeniedRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 denied-credentials record = %s, %v", data, err)
	}
	return rec
}

// requireNoDeniedKeyInEvidence fails when an evidence file holds a denied
// key in any encoding the redactor removes.
func (f *s11DeniedFixture) requireNoDeniedKeyInEvidence(t *testing.T) {
	t.Helper()
	forms := make([]string, 0, 8*len(f.keys))
	for _, key := range f.keys {
		forms = append(forms, evidence.SecretForms(key)...)
	}
	err := filepath.WalkDir(f.cfg.EvidenceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(forms, func(form string) bool { return strings.Contains(string(data), form) }) {
			t.Errorf("%s holds a denied key", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// deniedSecret is the Secret the run created with the denied keys.
func (f *s11DeniedFixture) deniedSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	for _, s := range f.secrets {
		if s.Name == s11DeniedSecretName {
			return s
		}
	}
	return nil
}

// wantDeleted are the deletions of a run that created all three objects.
var wantDeleted = []string{"CubridBackup/" + s11DeniedBackupName + "/" + testDeniedUID,
	testKindCluster + "/" + s11DeniedClusterName + "/" + testDeniedClusterUID,
	"Secret/" + s11DeniedSecretName + "/" + testDeniedSecretUID}

func TestRunS11DeniedCredentialsPassesWhenTheUploadIsRefused(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	denied, err := deniedOf(t, f)
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if summary := readSummary(t, cfg.EvidenceDir); !summary.Passed || denied.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want every scenario passed", summary)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,secret,denied-cluster,backup" {
		t.Fatalf("writes = %v", f.writes)
	}
	if denied.Limits[backupLimitName] != cfg.BackupLimit.String() ||
		denied.Measurements["failedWithin"] == evidence.Unknown {
		t.Fatalf("S11 denied-credentials limits = %v, measurements = %v", denied.Limits, denied.Measurements)
	}
	if denied.CleanupSucceeded == nil || !*denied.CleanupSucceeded {
		t.Fatalf("S11 denied-credentials = %+v, want its cleanup recorded as succeeded", denied)
	}
	for _, rel := range []string{s11DeniedRecordFile, s11DeniedSourceDataCheckFile} {
		if !slices.Contains(denied.Evidence, rel) {
			t.Fatalf("S11 denied-credentials evidence = %v, want %s", denied.Evidence, rel)
		}
	}
	requireDeniedObjects(t, f)
	requireDeniedRecord(t, f)
	// The run deleted what it created, by UID, and nothing else.
	if !slices.Equal(f.deleted, wantDeleted) {
		t.Fatalf("deleted = %v, want the backup, the cluster and the Secret the variant created, by UID",
			f.deleted)
	}
	if len(f.store.puts) != 0 || len(f.store.removes) != 0 {
		t.Fatalf("the run wrote %v and removed %v in the object store; want nothing", f.store.puts, f.store.removes)
	}
	requireNamespaceGone(t, f.haFixture)
}

// requireDeniedObjects checks the Secret, the cluster and the backup the run
// created.
func requireDeniedObjects(t *testing.T, f *s11DeniedFixture) {
	t.Helper()
	requireDeniedCluster(t, f)
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	got := f.denied
	if got == nil || got.Namespace != f.cfg.Namespace || got.Spec.ClusterRef.Name != s11DeniedClusterName ||
		got.Spec.Database != databaseName || got.Spec.Destination.Type != databasev1alpha1.DestinationObjectStorage {
		t.Fatalf("backup = %+v, want the standalone cluster's database in the run namespace", got)
	}
	if dest := got.Spec.Destination.ObjectStorage; dest == nil || dest.Bucket != testBackupBucket ||
		dest.Prefix != "lab-runs/"+lab01.RunID+"/denied-credentials" {
		t.Fatalf("destination = %+v, want the backup bucket under the variant's prefix", dest)
	}
}

// requireDeniedCluster checks the Secret with the denied keys and the
// standalone cluster that names it.
func requireDeniedCluster(t *testing.T, f *s11DeniedFixture) {
	t.Helper()
	secret := f.deniedSecret(t)
	if secret == nil || len(f.keys) != 2 || string(secret.Data[seedAccessKey]) != f.keys[0] ||
		string(secret.Data[seedSecretKey]) != f.keys[1] || f.keys[0] == f.keys[1] ||
		slices.Contains(f.keys, testAccessKey) || slices.Contains(f.keys, testSecretKey) ||
		len(f.keys[0]) < minSecretLength || len(f.keys[1]) < minSecretLength {
		t.Fatalf("Secret = %+v, keys = %d; want the keys the store refused, not the seed Secret's", secret,
			len(f.keys))
	}
	cluster := f.deniedCluster
	if cluster == nil {
		t.Fatal("the standalone cluster was not created")
	}
	// The HA cluster's spec, standalone.
	want := cluster.Spec.DeepCopy()
	want.HighAvailability.Enabled, want.Topology.PromotableMembers = true, haMemberCount
	want.ObjectStorage = f.cluster.Spec.ObjectStorage
	if !reflect.DeepEqual(*want, f.cluster.Spec) {
		t.Fatalf("cluster = %+v, want the HA cluster's spec, standalone", cluster.Spec)
	}
	store := cluster.Spec.ObjectStorage
	if store == nil || store.Endpoint != f.cfg.SeedEndpoint || !store.Insecure || store.Bucket != "" ||
		store.Prefix != "" || store.CredentialsSecretRef.Name != s11DeniedSecretName {
		t.Fatalf("object storage = %+v, want the seed endpoint with the denied Secret and no bucket", store)
	}
}

// requireDeniedRecord checks the record of a passed run.
func requireDeniedRecord(t *testing.T, f *s11DeniedFixture) {
	t.Helper()
	rec := readDeniedRecord(t, f.cfg.EvidenceDir)
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	if fault := rec.Fault; fault == nil || !fault.Refused || fault.CheckedAt == nil ||
		fault.Answer != testDeniedAnswer || fault.AnswerKind != storeErrorAccessDenied {
		t.Fatalf("fault = %+v, want the store's refusal of the denied keys", fault)
	}
	if c := rec.Cluster; c == nil || c.Name != s11DeniedClusterName || c.UID != testDeniedClusterUID ||
		c.Secret != s11DeniedSecretName || c.SecretUID != testDeniedSecretUID || c.CreatedAt == nil ||
		c.ReadyWithin == "" || c.Waiting != "" {
		t.Fatalf("cluster = %+v, want it created and Ready", c)
	}
	requireDeniedBackup(t, f, rec)
	requireDeniedChecks(t, rec, lab01)
}

// requireDeniedBackup checks the backup, its failure, the prefix at the end,
// the residue and the cleanup in the record of a passed run.
func requireDeniedBackup(t *testing.T, f *s11DeniedFixture, rec s11DeniedRecord) {
	t.Helper()
	if rec.Backup != s11DeniedBackupName || rec.BackupUID != testDeniedUID || rec.RequestedAt == nil ||
		rec.JudgedAt == nil || rec.RequestedAt.After(*rec.JudgedAt) || rec.FailedWithin == "" ||
		rec.Artifact != nil || rec.Bucket != testBackupBucket || rec.ReadyLimit != f.cfg.BackupDeniedReadyLimit.String() ||
		!strings.HasPrefix(rec.Failure, s11MissingReason+": upload failed: put "+testBackupBucket+"/") {
		t.Fatalf("record = %+v, want the request, the failure and the verdict", rec)
	}
	if rec.StoreError != testDeniedAnswer || rec.StoreErrorKind != storeErrorAccessDenied {
		t.Fatalf("store error = %q (%s), want the store's answer observed", rec.StoreError, rec.StoreErrorKind)
	}
	if rec.CheckedAtEnd == nil || len(rec.ObjectsAtEnd) != 0 || rec.ManifestAtEnd {
		t.Fatalf("record = %+v, want the prefix listed empty at the end", rec)
	}
	if !strings.Contains(rec.Residue, "staging directory") || !strings.Contains(rec.Residue, "PVC") {
		t.Fatalf("residue = %q, want the unchecked staging directory and the retained PVC named", rec.Residue)
	}
	for _, want := range wantDeleted {
		name := strings.Split(want, "/")[1]
		if !strings.Contains(rec.Cleanup, name) {
			t.Fatalf("cleanup = %q, want %s named", rec.Cleanup, name)
		}
	}
}

// requireDeniedChecks checks the source, the manager, the credentials and
// the notes in the record of a passed run.
func requireDeniedChecks(t *testing.T, rec s11DeniedRecord, lab01 haRecord) {
	t.Helper()
	if rec.Source == nil || rec.Source.Restarted == nil || *rec.Source.Restarted != 0 ||
		rec.Source.Master != testHAPrimary || !rec.Source.DataRulesHeld {
		t.Fatalf("source = %+v, want it unchanged", rec.Source)
	}
	if rec.ManagerBefore == nil || rec.ManagerAfter == nil || len(rec.ManagerAfter.Pods) == 0 ||
		rec.ManagerAfter.DeploymentUID != lab01.Operator.DeploymentUID {
		t.Fatalf("manager = %+v, %+v; want the installed manager recorded", rec.ManagerBefore, rec.ManagerAfter)
	}
	// Two cluster readings and four backup readings at least.
	if rec.Credentials == nil || rec.Credentials.StatusesChecked < 7 || !rec.Credentials.StatusChecked ||
		len(rec.Credentials.FoundIn) != 0 {
		t.Fatalf("credentials = %+v, want every status searched", rec.Credentials)
	}
	if !slices.Contains(rec.Notes, "the Operator's and the Instance Manager's logs were not checked") {
		t.Fatalf("notes = %v, want both logs named as not checked", rec.Notes)
	}
}

func TestRunS11DeniedCredentialsIsBlockedWhenTheFaultIsNotInPlace(t *testing.T) {
	for name, tc := range map[string]struct {
		refusal error
		want    string
	}{
		// A store that does not check keys lists the bucket.
		"the store accepts the denied keys": {want: "accepted the denied keys"},
		"the store cannot be reached":       {refusal: errors.New(testReset), want: testReset},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11DeniedConfig(t)
			f := newS11DeniedFixture(t, cfg)
			f.refusal = tc.refusal
			denied, err := deniedOf(t, f)
			if err == nil || denied.Result != evidence.Blocked || !strings.Contains(denied.Reason, tc.want) {
				t.Fatalf("S11 denied-credentials = %+v, want blocked by %q", denied, tc.want)
			}
			if f.deniedSecret(t) != nil || f.deniedCluster != nil || f.denied != nil || len(f.deleted) != 0 {
				t.Fatal("something was created before the store was known to refuse the denied keys")
			}
			rec := readDeniedRecord(t, cfg.EvidenceDir)
			if !strings.Contains(rec.Cleanup, "nothing") || rec.Fault == nil || rec.Fault.Refused {
				t.Fatalf("record = %+v, want the unconfirmed fault and nothing to delete", rec)
			}
			if denied.CleanupSucceeded == nil || !*denied.CleanupSucceeded {
				t.Fatalf("S11 denied-credentials = %+v, want nothing left to clean up", denied)
			}
		})
	}
}

func TestRunS11DeniedCredentialsIsBlockedWhenTheClusterIsNotReady(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	cfg.BackupDeniedReadyLimit = 300 * time.Millisecond
	f := newS11DeniedFixture(t, cfg)
	f.readyAt = 0
	denied, err := deniedOf(t, f)
	if err == nil || denied.Result != evidence.Blocked || !strings.Contains(denied.Reason, "was not Ready within") ||
		!strings.Contains(denied.Reason, "InstancesNotReady") {
		t.Fatalf("S11 denied-credentials = %+v, want blocked by the cluster that was not Ready", denied)
	}
	if f.denied != nil {
		t.Fatal("a backup was requested of a cluster that was not Ready")
	}
	// The cluster and the Secret are deleted all the same.
	if !slices.Equal(f.deleted, wantDeleted[1:]) || denied.CleanupSucceeded == nil || !*denied.CleanupSucceeded {
		t.Fatalf("deleted = %v, S11 denied-credentials = %+v; want the cluster and the Secret deleted", f.deleted,
			denied)
	}
	if rec := readDeniedRecord(t, cfg.EvidenceDir); rec.Cluster == nil || !strings.Contains(rec.Cluster.Waiting,
		"InstancesNotReady") {
		t.Fatalf("record = %+v, want what the cluster waited for", rec.Cluster)
	}
}

func TestRunS11DeniedCredentialsFailsWhenTheBackupCompletes(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	f.deniedPhases[3], f.writeObjects = databasev1alpha1.BackupPhaseCompleted, true
	denied, err := deniedOf(t, f)
	if err == nil || denied.Result != evidence.Fail || len(denied.NeverEvents) < 2 {
		t.Fatalf("S11 denied-credentials = %+v, want Completed and the artifact as never-events", denied)
	}
	for _, want := range []string{string(databasev1alpha1.BackupPhaseCompleted), "status.artifact",
		"holds 2 objects"} {
		if !strings.Contains(denied.Reason, want) {
			t.Fatalf("S11 denied-credentials reason = %q, want %q", denied.Reason, want)
		}
	}
	rec := readDeniedRecord(t, cfg.EvidenceDir)
	if !rec.ManifestAtEnd || len(rec.ObjectsAtEnd) != 2 || rec.Artifact == nil ||
		!strings.Contains(rec.Residue, "2 objects") {
		t.Fatalf("record = %+v; want what the store held at the end", rec)
	}
}

func TestRunS11DeniedCredentialsFailsWhenAnObjectIsLeftUnderThePrefix(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	f.writeObjects = true
	denied, err := deniedOf(t, f)
	// The scenario contract does not list it as a never-event: it fails.
	if err == nil || denied.Result != evidence.Fail || len(denied.NeverEvents) != 0 ||
		!strings.Contains(denied.Reason, "holds 1 objects") {
		t.Fatalf("S11 denied-credentials = %+v, want a failure for the object at the end", denied)
	}
	if rec := readDeniedRecord(t, cfg.EvidenceDir); rec.ManifestAtEnd || len(rec.ObjectsAtEnd) != 1 {
		t.Fatalf("record = %+v, want the object listed", rec)
	}
}

func TestRunS11DeniedCredentialsFailsOnAnArtifactOfAFailedBackup(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	f.tamperDenied = func(_ int, s *databasev1alpha1.CubridBackupStatus) {
		if s.Phase == databasev1alpha1.BackupPhaseFailed {
			s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: "s3://elsewhere/manifest.json"}
		}
	}
	denied, err := deniedOf(t, f)
	if err == nil || denied.Result != evidence.Fail || len(denied.NeverEvents) != 1 ||
		!strings.Contains(denied.NeverEvents[0], "status.artifact") ||
		strings.Count(denied.Reason, "status.artifact") != 1 {
		t.Fatalf("S11 denied-credentials = %+v, want the artifact as one never-event, named once", denied)
	}
}

func TestRunS11DeniedCredentialsJudgesAFailureByWhetherItReachedTheUpload(t *testing.T) {
	for name, tc := range map[string]struct {
		reason, message string
		want            evidence.Result
	}{
		// The backup ended before its upload: the denied keys were not
		// exercised, so S11 denied-credentials could not be judged.
		"backupdb": {reason: s11MissingReason, message: "backupdb failed: exit status 2", want: evidence.Blocked},
		"another Ready reason": {reason: "RecordedTargetNotStarted",
			message: "the backup was not started on its recorded target s11-denied-0", want: evidence.Blocked},
		// The upload ran and failed, but not for its destination.
		"another bucket": {reason: s11MissingReason, message: "upload failed: put elsewhere/x/backup/appdb_bk0v000: " +
			testDeniedAnswer, want: evidence.Fail},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11DeniedConfig(t)
			f := newS11DeniedFixture(t, cfg)
			f.tamperDenied = func(_ int, s *databasev1alpha1.CubridBackupStatus) {
				if s.Phase == databasev1alpha1.BackupPhaseFailed {
					s.Conditions[0].Reason, s.Conditions[0].Message = tc.reason, tc.message
				}
			}
			denied, err := deniedOf(t, f)
			if err == nil || denied.Result != tc.want || !strings.Contains(denied.Reason, tc.message) ||
				len(denied.NeverEvents) != 0 {
				t.Fatalf("S11 denied-credentials = %+v, want %s naming the other reason", denied, tc.want)
			}
			if !slices.Equal(f.deleted, wantDeleted) {
				t.Fatalf("deleted = %v, want the backup, the cluster and the Secret deleted", f.deleted)
			}
		})
	}
}

func TestRunS11DeniedCredentialsIsBlockedWhenTheUploadAnswerIsNotARefusal(t *testing.T) {
	for name, tc := range map[string]struct{ answer, want string }{
		// The upload to the bucket failed, but for something the denied keys
		// do not explain.
		"a timeout":        {answer: ": context deadline exceeded", want: "context deadline exceeded"},
		"a missing bucket": {answer: ": The specified bucket does not exist.", want: "does not exist"},
		"no answer":        {want: "no answer quoted"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11DeniedConfig(t)
			f := newS11DeniedFixture(t, cfg)
			f.tamperDenied = func(_ int, s *databasev1alpha1.CubridBackupStatus) {
				if s.Phase == databasev1alpha1.BackupPhaseFailed {
					s.Conditions[0].Message = "upload failed: put " + testBackupBucket + "/x/backup/appdb_bk0v000" +
						tc.answer
				}
			}
			denied, err := deniedOf(t, f)
			if err == nil || denied.Result != evidence.Blocked || len(denied.NeverEvents) != 0 ||
				!strings.Contains(denied.Reason, "does not show a refusal of the keys") ||
				!strings.Contains(denied.Reason, tc.want) {
				t.Fatalf("S11 denied-credentials = %+v, want blocked: the answer %q is not a refusal", denied,
					tc.want)
			}
			if !slices.Equal(f.deleted, wantDeleted) {
				t.Fatalf("deleted = %v, want the backup, the cluster and the Secret deleted", f.deleted)
			}
		})
	}
}

func TestRunS11DeniedCredentialsFailsOnADeniedKeyInTheClusterStatusAfterReady(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	read := f.deniedRead
	f.deniedRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if err := read(ctx, c, key); err != nil {
			return err
		}
		f.mu.Lock()
		reads := f.clusterReads
		f.mu.Unlock()
		// The wait ends at the readyAt-th reading; the reading at the end
		// follows it.
		if reads <= f.readyAt {
			return nil
		}
		cluster := &databasev1alpha1.CubridCluster{}
		if err := c.Get(ctx, key, cluster); err != nil {
			return err
		}
		cluster.Status.Conditions[0].Message += " (key " + f.keys[1] + ")"
		return c.Update(ctx, cluster)
	}
	denied, err := deniedOf(t, f)
	if err == nil || denied.Result != evidence.Fail || len(denied.NeverEvents) != 1 ||
		!strings.Contains(denied.NeverEvents[0], "the status of "+s11DeniedClusterName+" at the end") ||
		strings.Contains(denied.NeverEvents[0], "the status read of") {
		t.Fatalf("S11 denied-credentials = %+v, want the key written after Ready found at the end", denied)
	}
}

func TestRunS11DeniedCredentialsFailsOnAKeyInAnEvent(t *testing.T) {
	for name, key := range map[string]func(f *s11DeniedFixture) string{
		"a denied key": func(f *s11DeniedFixture) string { return f.keys[1] },
		"a seed key":   func(*s11DeniedFixture) string { return testSecretKey },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11DeniedConfig(t)
			f := newS11DeniedFixture(t, cfg)
			f.onDeniedRead = func(read int) error {
				if read != 1 {
					return nil
				}
				event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "s11-denied.leak", Namespace: cfg.Namespace},
					InvolvedObject: corev1.ObjectReference{Kind: "CubridBackup", Name: s11DeniedBackupName,
						UID: testDeniedUID},
					Reason: "BackupFailed", Message: "upload refused for key " + key(f)}
				if err := f.haFixture.store.Create(t.Context(), event); err != nil {
					t.Fatal(err)
				}
				return nil
			}
			denied, err := deniedOf(t, f)
			if err == nil || denied.Result != evidence.Fail || len(denied.NeverEvents) != 1 ||
				!strings.Contains(denied.NeverEvents[0], testCredential) ||
				!strings.Contains(denied.NeverEvents[0], "Event s11-denied.leak") {
				t.Fatalf("S11 denied-credentials = %+v, want the key in the Event as a never-event", denied)
			}
		})
	}
}

func TestRunS11DeniedCredentialsFailsOnADeniedKeyInTheClusterStatus(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	read := f.deniedRead
	f.deniedRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if err := read(ctx, c, key); err != nil {
			return err
		}
		cluster := &databasev1alpha1.CubridCluster{}
		if err := c.Get(ctx, key, cluster); err != nil {
			return err
		}
		cluster.Status.Conditions[0].Message += " (key " + f.keys[0] + ")"
		return c.Update(ctx, cluster)
	}
	denied, err := deniedOf(t, f)
	if err == nil || denied.Result != evidence.Fail || len(denied.NeverEvents) != 1 ||
		!strings.Contains(denied.NeverEvents[0], "the status read of "+s11DeniedClusterName) {
		t.Fatalf("S11 denied-credentials = %+v, want the denied key in the cluster's status as a never-event",
			denied)
	}
}

func TestRunS11DeniedCredentialsRecordsWhatItCouldNotDelete(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	f.deleteErr[testKindCluster] = apierrors.NewForbidden(
		databasev1alpha1.GroupVersion.WithResource("cubridclusters").GroupResource(), s11DeniedClusterName,
		errors.New("forbidden"))
	denied, err := deniedOf(t, f)
	if err != nil || denied.Result != evidence.Pass {
		t.Fatalf("S11 denied-credentials = %+v, error %v; want a pass: deleting is not part of the verdict",
			denied, err)
	}
	if denied.CleanupSucceeded == nil || *denied.CleanupSucceeded {
		t.Fatalf("S11 denied-credentials = %+v, want the failed cleanup recorded", denied)
	}
	// The Secret is still deleted after the cluster could not be.
	if !slices.Equal(f.deleted, wantDeleted) {
		t.Fatalf("deleted = %v, want every deletion attempted", f.deleted)
	}
	rec := readDeniedRecord(t, cfg.EvidenceDir)
	if !strings.Contains(rec.Cleanup, testForbidden) || !strings.Contains(rec.Cleanup, "cluster "+
		s11DeniedClusterName+" (UID "+testDeniedClusterUID+") could not be deleted") ||
		!strings.Contains(rec.Cleanup, "Secret "+s11DeniedSecretName+" (UID "+testDeniedSecretUID+") was accepted") {
		t.Fatalf("cleanup = %q, want the cluster left named with the error and the rest deleted", rec.Cleanup)
	}
}

func TestRunS11DeniedCredentialsIsBlockedWhenTheRunEndsDuringTheWait(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	cfg.BackupLimit = time.Minute
	f := newS11DeniedFixture(t, cfg)
	f.deniedPhases = f.deniedPhases[:2]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.onDeniedRead = func(read int) error {
		if read == 2 {
			cancel()
		}
		return nil
	}
	if err := f.runner.RunHA(ctx, cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	denied := deniedScenarios(t, cfg.EvidenceDir)[2]
	if denied.Result != evidence.Blocked || !strings.Contains(denied.Reason, "the run ended") {
		t.Fatalf("S11 denied-credentials = %+v, want blocked: the run ended", denied)
	}
	if !slices.Equal(f.deleted, wantDeleted) || denied.CleanupSucceeded == nil || !*denied.CleanupSucceeded {
		t.Fatalf("deleted = %v, S11 denied-credentials = %+v; want everything deleted after the run ended",
			f.deleted, denied)
	}
}

func TestRunS11DeniedCredentialsFailsWhenTheSourceChanged(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	f.onDeniedRead = func(read int) error {
		if read == 1 {
			f.restartSourceMember(t.Context(), f.haFixture.store)
		}
		return nil
	}
	denied, err := deniedOf(t, f)
	if err == nil || denied.Result != evidence.Fail || !strings.Contains(denied.Reason, "source") ||
		!strings.Contains(denied.Reason, "restarted") {
		t.Fatalf("S11 denied-credentials = %+v, want the changed source", denied)
	}
}

func TestRunS11DeniedCredentialsIsBlockedWhenS11CompleteDidNotPass(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if denied := deniedScenarios(t, cfg.EvidenceDir)[2]; denied.Result != evidence.Blocked ||
		!strings.Contains(denied.Reason, "S11 complete did not pass") {
		t.Fatalf("S11 denied-credentials = %+v, want blocked by S11 complete", denied)
	}
	if f.deniedCluster != nil || len(f.keys) != 0 {
		t.Fatal("S11 denied-credentials ran although S11 complete did not pass")
	}
}

func TestRunHARefusesBackupDeniedCredentialsWithoutItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(cfg *HAConfig)
		want   string
	}{
		"without -backup-bucket": {change: func(cfg *HAConfig) { cfg.BackupBucket = "" },
			want: "needs -backup-bucket"},
		"with -restore": {change: func(cfg *HAConfig) {
			cfg.Restore, cfg.RestoreLimit = true, time.Minute
		}, want: "-restore"},
		"with -backup-missing-destination": {change: func(cfg *HAConfig) {
			cfg.BackupMissingDestination = true
		}, want: "-backup-missing-destination"},
		"without a ready limit": {change: func(cfg *HAConfig) { cfg.BackupDeniedReadyLimit = 0 },
			want: "-backup-denied-ready-limit"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11DeniedConfig(t)
			tc.change(&cfg)
			f := newS11DeniedFixture(t, testS11DeniedConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "-backup-denied-credentials") {
				t.Fatalf("RunHA() error = %v, want -backup-denied-credentials refused %s", err, name)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}

func TestStoreErrorKindNamesWhatTheStoreAnswered(t *testing.T) {
	for answer, want := range map[string]string{
		"The specified bucket does not exist.": storeErrorNoSuchBucket,
		"NoSuchBucket":                         storeErrorNoSuchBucket,
		"Access Denied.":                       storeErrorAccessDenied,
		"AccessDenied":                         storeErrorAccessDenied,
		"InvalidAccessKeyId":                   storeErrorAccessDenied,
		"The Access Key Id you provided does not exist in our records.": storeErrorAccessDenied,
		"The access key ID you provided does not exist in our records.": storeErrorAccessDenied,
		"SignatureDoesNotMatch": storeErrorAccessDenied,
		"The request signature we calculated does not match the signature you provided. Check your key and " +
			"signing method.": storeErrorAccessDenied,
		"connection reset by peer":                             storeErrorReset,
		"dial tcp 10.42.1.9:8333: connect: connection refused": storeErrorReset,
		"write: broken pipe":                                   storeErrorReset,
		"context deadline exceeded":                            storeErrorOther,
		"":                                                     storeErrorOther,
	} {
		if got := storeErrorKind(answer); got != want {
			t.Errorf("storeErrorKind(%q) = %s, want %s", answer, got, want)
		}
	}
}
