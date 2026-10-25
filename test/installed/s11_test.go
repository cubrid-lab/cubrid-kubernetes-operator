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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testBackupBucket   = "lab-backups"
	testForwarded      = "127.0.0.1:18333"
	testBackupUID      = "backup-uid"
	testBackupTarget   = "ha-0"
	testAttempt        = "attempt-0011223344556677"
	testFirstBackupKey = "backup/appdb_bk0v000"
	testClusterUID     = "ha-cluster"
)

func testS11Config(t *testing.T) HAConfig {
	t.Helper()
	cfg := testHAConfig(t)
	cfg.Timeout = 20 * time.Second
	cfg.BackupBucket = testBackupBucket
	cfg.BackupLimit = 5 * time.Second
	cfg.ObjectStoreService = "object-store/seaweedfs:8333"
	return cfg
}

// fakeStore is an object store in memory, keyed by bucket/key.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *fakeStore) put(bucket, key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[bucket+"/"+key] = data
}

func (s *fakeStore) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("get %s/%s: NoSuchKey", bucket, key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeStore) List(_ context.Context, bucket, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for name := range s.objects {
		if key, ok := strings.CutPrefix(name, bucket+"/"); ok && strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// fakeArtifact is what the fake Operator uploads for a backup: the objects
// below the attempt prefix, the manifest naming them, and the URI the status
// reports.
type fakeArtifact struct {
	manifest instancemanager.BackupManifest
	objects  map[string][]byte
	uri      string
}

// s11Fixture plays an Operator that takes a backup and uploads its
// artifact, and an object store reached through a port-forward.
type s11Fixture struct {
	*haFixture
	store *fakeStore
	// phases are the phases the fake Operator reports, one per reading of the
	// backup; the last is kept.
	phases []databasev1alpha1.CubridBackupPhase
	// tamper changes the artifact before its manifest is written and its
	// digest taken; tamperStatus changes the Completed status.
	tamper       func(*fakeArtifact)
	tamperStatus func(*databasev1alpha1.CubridBackupStatus)
	// onRead runs at each reading of the backup, with its number.
	onRead func(read int)

	reads, forwards, forwardsStopped int
	markers                          []string
	backup                           *databasev1alpha1.CubridBackup
	artifact                         *databasev1alpha1.CubridBackupArtifact
}

func newS11Fixture(t *testing.T, cfg HAConfig) *s11Fixture {
	t.Helper()
	f := &s11Fixture{haFixture: newHAFixture(t, cfg), store: &fakeStore{objects: map[string][]byte{}},
		phases: []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhasePending,
			databasev1alpha1.BackupPhaseRunning, databasev1alpha1.BackupPhaseUploading,
			databasev1alpha1.BackupPhaseCompleted}}
	f.marker = func(script string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.backup != nil {
			t.Error("the marker was written after the backup was requested")
		}
		f.markers = append(f.markers, script)
		return "marker " + s11Marker + " written\n", nil
	}
	f.backupCreated = func(_ context.Context, _ client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		backup.UID = testBackupUID
		f.backup = backup.DeepCopy()
		return nil
	}
	f.backupRead = f.advance
	f.runner.forward = func(_ context.Context, namespace, service, port string) (string, func(), error) {
		if namespace != "object-store" || service != "seaweedfs" || port != "8333" {
			t.Errorf("port-forward to %s/%s:%s", namespace, service, port)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.forwards++
		return testForwarded, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.forwardsStopped++
		}, nil
	}
	f.runner.openStore = func(endpoint, accessKey, secretKey string, secure bool) (objectStore, error) {
		if endpoint != testForwarded || accessKey != testAccessKey || secretKey != testSecretKey || secure {
			t.Errorf("object store client for %s, secure %t, without the seed Secret's keys", endpoint, secure)
		}
		return f.store, nil
	}
	return f
}

// advance writes the status of the next phase, as the Operator would. The
// fake client stores the UID the run created the backup with.
func (f *s11Fixture) advance(ctx context.Context, c client.WithWatch, key client.ObjectKey) {
	backup := &databasev1alpha1.CubridBackup{}
	if err := c.Get(ctx, key, backup); err != nil {
		return
	}
	f.mu.Lock()
	f.reads++
	read := f.reads
	phase := f.phases[min(read, len(f.phases))-1]
	onRead := f.onRead
	f.mu.Unlock()
	if onRead != nil {
		onRead(read)
	}
	started := metav1.NewTime(time.Now().Add(-time.Second))
	s := &backup.Status
	s.Phase, s.ClusterUID, s.StartedAt = phase, testClusterUID, &started
	s.Conditions = []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse, Reason: "BackupRunning",
		Message: "backup running", LastTransitionTime: started}}
	if phase != databasev1alpha1.BackupPhasePending {
		s.TargetInstance, s.TargetRole = testBackupTarget, string(databasev1alpha1.RoleSlave)
	}
	switch phase {
	case databasev1alpha1.BackupPhaseCompleted:
		s.Artifact = f.upload(backup)
		s.CompletedAt = &started
		s.Conditions[0].Status, s.Conditions[0].Reason, s.Conditions[0].Message = metav1.ConditionTrue,
			"BackupCompleted", "backup completed"
		if f.tamperStatus != nil {
			f.tamperStatus(s)
		}
	case databasev1alpha1.BackupPhaseFailed:
		s.Conditions[0].Reason, s.Conditions[0].Message = "BackupFailed", "backupdb failed: exit status 1"
	}
	if err := c.Update(ctx, backup); err != nil {
		f.t.Error(err)
	}
}

// upload writes the artifact of backup into the store once and returns
// what the status reports of it.
func (f *s11Fixture) upload(backup *databasev1alpha1.CubridBackup) *databasev1alpha1.CubridBackupArtifact {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.artifact != nil {
		return f.artifact.DeepCopy()
	}
	attempt := path.Join(backup.Spec.Destination.ObjectStorage.Prefix, testClusterUID, databaseName,
		string(backup.UID), testAttempt)
	a := fakeArtifact{objects: map[string][]byte{testFirstBackupKey: bytes.Repeat([]byte("v"), 1000),
		"backup/appdb_bkvinf": []byte("volume information")},
		uri: "s3://" + backup.Spec.Destination.ObjectStorage.Bucket + "/" + attempt + "/manifest.json",
		manifest: instancemanager.BackupManifest{ManifestVersion: instancemanager.ManifestVersion,
			Database: databaseName, ClusterUID: testClusterUID, CubridVersion: "11.4", SourceInstance: testBackupTarget,
			SourceRole: "slave", CreatedAt: time.Now().UTC().Format(time.RFC3339), OperationID: "op-1"}}
	for _, key := range []string{testFirstBackupKey, "backup/appdb_bkvinf"} {
		sum := sha256.Sum256(a.objects[key])
		a.manifest.Objects = append(a.manifest.Objects, instancemanager.ManifestObject{Key: key,
			SizeBytes: int64(len(a.objects[key])), SHA256: hex.EncodeToString(sum[:])})
	}
	if f.tamper != nil {
		f.tamper(&a)
	}
	data, err := a.manifest.Marshal()
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := a.manifest.Digest()
	if err != nil {
		f.t.Fatal(err)
	}
	bucket := backup.Spec.Destination.ObjectStorage.Bucket
	f.store.put(bucket, attempt+"/manifest.json", data)
	for key, content := range a.objects {
		f.store.put(bucket, attempt+"/"+key, content)
	}
	f.artifact = &databasev1alpha1.CubridBackupArtifact{URI: a.uri, ManifestDigest: digest,
		SizeBytes: a.manifest.TotalSize(), Database: databaseName, Level: databasev1alpha1.CubridBackupFull}
	return f.artifact.DeepCopy()
}

func s11Scenario(t *testing.T, dir string) (evidence.Summary, evidence.Scenario, evidence.Scenario) {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s11Lane || len(summary.Scenarios) != 2 {
		t.Fatalf("summary = %+v, want LAB01 and S11/complete in lane %s", summary, s11Lane)
	}
	return summary, summary.Scenarios[0], summary.Scenarios[1]
}

func readS11Record(t *testing.T, dir string) s11Record {
	t.Helper()
	var rec s11Record
	data, err := os.ReadFile(filepath.Join(dir, s11RecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 record = %s, %v", data, err)
	}
	return rec
}

// requireForwardStopped fails unless every port-forward the run opened was
// ended.
func (f *s11Fixture) requireForwardStopped(t *testing.T) {
	t.Helper()
	if f.forwards != f.forwardsStopped {
		t.Fatalf("%d port-forwards opened, %d ended", f.forwards, f.forwardsStopped)
	}
}

func TestRunS11CompletePassesWithAVerifiedArtifact(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	summary, lab, s11 := s11Scenario(t, cfg.EvidenceDir)
	if !summary.Passed || lab.Result != evidence.Pass || s11.Result != evidence.Pass || s11.Variant != s11Variant {
		t.Fatalf("summary = %+v, want both passed", summary)
	}
	if s11.Limits["backup_limit"] != cfg.BackupLimit.String() || s11.Measurements["backupCompletedWithin"] == nil {
		t.Fatalf("S11 limits = %v, measurements = %v", s11.Limits, s11.Measurements)
	}
	if !slices.Equal(f.writes, []string{"namespace", "secret", "cluster", "client", "backup"}) {
		t.Fatalf("writes = %v", f.writes)
	}
	requireS11Backup(t, f)
	requireS11Record(t, cfg)
	for _, rel := range []string{s11RecordFile, s11ManifestFile} {
		if !slices.Contains(s11.Evidence, rel) {
			t.Fatalf("S11 evidence = %v, want %s", s11.Evidence, rel)
		}
	}
	f.requireForwardStopped(t)
	requireNamespaceGone(t, f.haFixture)
	requireNoSecretInEvidence(t, cfg.EvidenceDir)
}

// requireS11Backup checks the marker and the CubridBackup of a passed run.
func requireS11Backup(t *testing.T, f *s11Fixture) {
	t.Helper()
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	spec := f.backup.Spec
	if f.backup.Name != s11BackupName || spec.ClusterRef.Name != haClusterName || spec.Database != databaseName ||
		spec.Destination.Type != databasev1alpha1.DestinationObjectStorage ||
		spec.Destination.ObjectStorage == nil || spec.Destination.ObjectStorage.Bucket != testBackupBucket ||
		spec.Destination.ObjectStorage.Prefix != "lab-runs/"+lab01.RunID+"/backups" {
		t.Fatalf("backup = %+v, want the run's database to the backup bucket under the run's prefix", f.backup)
	}
	if len(f.markers) != 1 || !strings.Contains(f.markers[0], " "+s11Marker+" "+lab01.RunID) {
		t.Fatalf("markers = %v, want one marker naming the run before the backup", f.markers)
	}
}

// requireS11Record checks the record and the manifest copy of a passed run.
func requireS11Record(t *testing.T, cfg HAConfig) {
	t.Helper()
	lab01 := readHARecord(t, cfg.EvidenceDir)
	rec := readS11Record(t, cfg.EvidenceDir)
	if rec.KnownPoint == nil || rec.KnownPoint.Marker != s11Marker || rec.KnownPoint.MarkerValue != lab01.RunID ||
		rec.KnownPoint.Acknowledged != cfg.Operations || rec.KnownPoint.LastAcknowledged != "c1-000010" {
		t.Fatalf("known point = %+v, want the marker and the client history's position", rec.KnownPoint)
	}
	phases := make([]string, 0, len(rec.Phases))
	for _, p := range rec.Phases {
		phases = append(phases, p.Phase)
	}
	if !slices.Equal(phases, []string{"Pending", "Running", "Uploading", "Completed"}) ||
		rec.Phases[1].TargetInstance != testBackupTarget || rec.Phases[1].TargetRole != "slave" {
		t.Fatalf("phases = %+v, want every transition with its target", rec.Phases)
	}
	if rec.Artifact == nil || rec.Verification == nil || rec.Verification.ObjectsVerified != 2 ||
		len(rec.Verification.Problems) != 0 || len(rec.Verification.NotInManifest) != 0 {
		t.Fatalf("artifact = %+v, verification = %+v", rec.Artifact, rec.Verification)
	}
	manifest, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, s11ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instancemanager.ParseManifest(manifest); err != nil {
		t.Fatalf("S11 manifest copy: %v", err)
	}
}

func TestRunS11CompleteRecordsAnObjectTheManifestDoesNotName(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	f.tamper = func(a *fakeArtifact) { a.objects["backup/leftover"] = []byte("left over") }
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v, want an object outside the manifest only recorded", err)
	}
	rec := readS11Record(t, cfg.EvidenceDir)
	if v := rec.Verification; len(v.NotInManifest) != 1 || !strings.HasSuffix(v.NotInManifest[0], "/backup/leftover") {
		t.Fatalf("verification = %+v, want the object outside the manifest recorded", rec.Verification)
	}
}

func TestRunS11CompleteFailsOnTheArtifact(t *testing.T) {
	for name, tc := range map[string]struct {
		tamper       func(*fakeArtifact)
		tamperStatus func(*databasev1alpha1.CubridBackupStatus)
		want         string
	}{
		"manifest digest": {tamperStatus: func(s *databasev1alpha1.CubridBackupStatus) {
			s.Artifact.ManifestDigest = strings.Repeat("0", 64)
		}, want: "manifestDigest"},
		"object checksum": {tamper: func(a *fakeArtifact) {
			a.objects[testFirstBackupKey] = bytes.Repeat([]byte("w"), 1000)
		}, want: "sha256"},
		"object missing": {tamper: func(a *fakeArtifact) { delete(a.objects, testFirstBackupKey) },
			want: "could not be read"},
		"cluster UID": {tamper: func(a *fakeArtifact) { a.manifest.ClusterUID = "another-cluster" },
			want: "clusterUID"},
		"source instance": {tamper: func(a *fakeArtifact) { a.manifest.SourceInstance = testHAPrimary },
			want: "sourceInstance"},
		"outside the bucket": {tamper: func(a *fakeArtifact) {
			a.uri = strings.Replace(a.uri, testBackupBucket, "another-bucket", 1)
		}, want: "bucket"},
		"outside the run's prefix": {tamper: func(a *fakeArtifact) {
			a.uri = strings.Replace(a.uri, "lab-runs/", "elsewhere/", 1)
		}, want: "run's prefix"},
		"size": {tamperStatus: func(s *databasev1alpha1.CubridBackupStatus) { s.Artifact.SizeBytes++ },
			want: "sizeBytes"},
		"credential in status": {tamperStatus: func(s *databasev1alpha1.CubridBackupStatus) {
			s.Conditions[0].Message = "uploaded with " + testAccessKey
		}, want: "credential"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11Config(t)
			f := newS11Fixture(t, cfg)
			f.tamper, f.tamperStatus = tc.tamper, tc.tamperStatus
			err := f.runner.RunHA(t.Context(), cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunHA() error = %v, want %q", err, tc.want)
			}
			_, lab, s11 := s11Scenario(t, cfg.EvidenceDir)
			if lab.Result != evidence.Pass || s11.Result != evidence.Fail {
				t.Fatalf("LAB01 = %s, S11 = %s; want LAB01 kept and S11 failed", lab.Result, s11.Result)
			}
			f.requireForwardStopped(t)
			requireNoSecretInEvidence(t, cfg.EvidenceDir)
		})
	}
}

func TestRunS11CompleteFailsOnACredentialInAnEvent(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	created := f.backupCreated
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "s11.1", Namespace: cfg.Namespace},
			InvolvedObject: corev1.ObjectReference{Kind: "CubridBackup", Name: backup.Name, UID: testBackupUID},
			Reason:         "BackupRunning", Message: "connecting with " + testSecretKey}
		if err := c.Create(ctx, event); err != nil {
			return err
		}
		return created(ctx, c, backup)
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("RunHA() error = %v, want the credential in an Event", err)
	}
	if _, _, s11 := s11Scenario(t, cfg.EvidenceDir); s11.Result != evidence.Fail || len(s11.NeverEvents) == 0 {
		t.Fatalf("S11 = %+v, want a failure with the event that must never happen", s11)
	}
	requireNoSecretInEvidence(t, cfg.EvidenceDir)
}

func TestRunS11CompleteFailsWhenTheBackupFailed(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseRunning,
		databasev1alpha1.BackupPhaseFailed}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "backupdb failed") {
		t.Fatalf("RunHA() error = %v, want the backup's failure", err)
	}
	if _, _, s11 := s11Scenario(t, cfg.EvidenceDir); s11.Result != evidence.Fail {
		t.Fatalf("S11 = %+v, want a failure", s11)
	}
	if f.forwards != 0 {
		t.Fatal("the artifact of a failed backup was downloaded")
	}
}

func TestRunS11CompleteFailsOverTheBackupLimit(t *testing.T) {
	cfg := testS11Config(t)
	cfg.BackupLimit = 50 * time.Millisecond
	f := newS11Fixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseRunning}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "backup_limit") {
		t.Fatalf("RunHA() error = %v, want the backup limit exceeded", err)
	}
	if _, _, s11 := s11Scenario(t, cfg.EvidenceDir); s11.Result != evidence.Fail {
		t.Fatalf("S11 = %+v, want a failure", s11)
	}
}

func TestRunS11CompleteIsBlockedWhenTheRunEnds(t *testing.T) {
	cfg := testS11Config(t)
	cfg.BackupLimit = time.Minute
	f := newS11Fixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseRunning}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.onRead = func(read int) {
		if read == 2 {
			cancel()
		}
	}
	if err := f.runner.RunHA(ctx, cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	_, lab, s11 := s11Scenario(t, cfg.EvidenceDir)
	if lab.Result != evidence.Pass || s11.Result != evidence.Blocked {
		t.Fatalf("LAB01 = %s, S11 = %+v; want LAB01 kept and S11 blocked", lab.Result, s11)
	}
	requireNamespaceGone(t, f.haFixture)
}

func TestRunS11IsBlockedWhenLAB01Failed(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	f.initErr = fmt.Errorf("tables not created")
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if f.backup != nil || len(f.markers) != 0 {
		t.Fatal("the backup was requested although LAB01 did not pass")
	}
	_, lab, s11 := s11Scenario(t, cfg.EvidenceDir)
	if lab.Result != evidence.Fail || s11.Result != evidence.Blocked {
		t.Fatalf("LAB01 = %s, S11 = %s; want fail and blocked", lab.Result, s11.Result)
	}
}

func TestRunHARefusesAnIncompleteS11Configuration(t *testing.T) {
	for name, change := range map[string]func(*HAConfig){
		"with a fault": func(c *HAConfig) {
			// A complete S06 configuration: only the combination is refused.
			c.Fault, c.StablePeriod, c.ResyncLimit, c.LeaderLease = FaultOperatorRestart, time.Second, time.Second,
				testLeaderLease
		},
		"bucket not S3":          func(c *HAConfig) { c.BackupBucket = "Lab_Backups" },
		"no backup limit":        func(c *HAConfig) { c.BackupLimit = 0 },
		"no object store":        func(c *HAConfig) { c.ObjectStoreService = "" },
		"object store no port":   func(c *HAConfig) { c.ObjectStoreService = "object-store/seaweedfs" },
		"object store no ns":     func(c *HAConfig) { c.ObjectStoreService = "seaweedfs:8333" },
		"object store bad port":  func(c *HAConfig) { c.ObjectStoreService = "object-store/seaweedfs:http" },
		"object store bad names": func(c *HAConfig) { c.ObjectStoreService = "Object_Store/seaweedfs:8333" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11Config(t)
			change(&cfg)
			f := newS11Fixture(t, testS11Config(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil {
				t.Fatal("RunHA() accepted the configuration")
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}
