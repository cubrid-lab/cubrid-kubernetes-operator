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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	testAttempt        = "attempt-00112233445566778899aabbccddeeff"
	testFirstBackupKey = "backup/appdb_bk0v000"
	testClusterUID     = "ha-cluster"
	// testCredential is in every refusal of a credential found.
	testCredential = "credential"
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

// fakeStore is an object store in memory, keyed by bucket/key. getErr, when
// set, answers a Get first.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	getErr  func(ctx context.Context, key string) error
}

func (s *fakeStore) put(bucket, key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[bucket+"/"+key] = data
}

func (s *fakeStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	if s.getErr != nil {
		if err := s.getErr(ctx, key); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("get %s/%s: %w", bucket, key, errObjectNotFound)
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
// reports. encode stores the manifest; nil stores its canonical encoding.
type fakeArtifact struct {
	manifest instancemanager.BackupManifest
	objects  map[string][]byte
	prefix   string
	uri      string
	encode   func(instancemanager.BackupManifest) ([]byte, error)
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
	// tamperPhase changes the status of every phase.
	tamperPhase func(*databasev1alpha1.CubridBackupStatus)
	// onRead runs at each reading of the backup, with its number; its error
	// is the reading's.
	onRead func(read int) error
	// accessKey and secretKey are the keys the run's client must use.
	accessKey, secretKey string

	reads, forwards, forwardsStopped int
	markers                          []string
	backup                           *databasev1alpha1.CubridBackup
	artifact                         *databasev1alpha1.CubridBackupArtifact
}

func newS11Fixture(t *testing.T, cfg HAConfig, extra ...client.Object) *s11Fixture {
	t.Helper()
	f := &s11Fixture{haFixture: newHAFixture(t, cfg, extra...), store: &fakeStore{objects: map[string][]byte{}},
		accessKey: testAccessKey, secretKey: testSecretKey,
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
		if endpoint != testForwarded || accessKey != f.accessKey || secretKey != f.secretKey || secure {
			t.Errorf("object store client for %s, secure %t, without the seed Secret's keys", endpoint, secure)
		}
		return f.store, nil
	}
	return f
}

// advance writes the status of the next phase, as the Operator would. The
// fake client stores the UID the run created the backup with.
func (f *s11Fixture) advance(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	backup := &databasev1alpha1.CubridBackup{}
	if err := c.Get(ctx, key, backup); err != nil {
		return nil
	}
	f.mu.Lock()
	f.reads++
	read := f.reads
	phase := f.phases[min(read, len(f.phases))-1]
	onRead := f.onRead
	f.mu.Unlock()
	if onRead != nil {
		if err := onRead(read); err != nil {
			return err
		}
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
	if f.tamperPhase != nil {
		f.tamperPhase(s)
	}
	if err := c.Update(ctx, backup); err != nil {
		f.t.Error(err)
	}
	return nil
}

// upload writes the artifact of backup into the store once and returns
// what the status reports of it.
func (f *s11Fixture) upload(backup *databasev1alpha1.CubridBackup) *databasev1alpha1.CubridBackupArtifact {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.artifact != nil {
		return f.artifact.DeepCopy()
	}
	bucket := backup.Spec.Destination.ObjectStorage.Bucket
	a := fakeArtifact{objects: map[string][]byte{testFirstBackupKey: bytes.Repeat([]byte("v"), 1000),
		"backup/appdb_bkvinf": []byte("volume information")},
		prefix: path.Join(backup.Spec.Destination.ObjectStorage.Prefix, testClusterUID, databaseName,
			string(backup.UID), testAttempt),
		manifest: instancemanager.BackupManifest{ManifestVersion: instancemanager.ManifestVersion,
			Database: databaseName, ClusterUID: testClusterUID, CubridVersion: "11.4", SourceInstance: testBackupTarget,
			SourceRole: "slave", CreatedAt: time.Now().UTC().Format(time.RFC3339), OperationID: "op-1"}}
	for _, key := range []string{testFirstBackupKey, "backup/appdb_bkvinf"} {
		sum := sha256.Sum256(a.objects[key])
		a.manifest.Objects = append(a.manifest.Objects, instancemanager.ManifestObject{Key: key,
			SizeBytes: int64(len(a.objects[key])), SHA256: hex.EncodeToString(sum[:])})
	}
	a.uri = "s3://" + bucket + "/" + a.prefix + "/manifest.json"
	if f.tamper != nil {
		f.tamper(&a)
	}
	encode := a.encode
	if encode == nil {
		encode = instancemanager.BackupManifest.Marshal
	}
	data, err := encode(a.manifest)
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := a.manifest.Digest()
	if err != nil {
		f.t.Fatal(err)
	}
	f.store.put(bucket, a.prefix+"/manifest.json", data)
	for key, content := range a.objects {
		f.store.put(bucket, a.prefix+"/"+key, content)
	}
	f.artifact = &databasev1alpha1.CubridBackupArtifact{URI: a.uri, ManifestDigest: digest,
		SizeBytes: a.manifest.TotalSize(), Database: databaseName, Level: databasev1alpha1.CubridBackupFull}
	return f.artifact.DeepCopy()
}

// move stores the artifact under another prefix, which the URI names.
func (a *fakeArtifact) move(from, to string) {
	a.prefix = strings.Replace(a.prefix, from, to, 1)
	a.uri = strings.Replace(a.uri, from, to, 1)
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
	requireOwnEvidence(t, lab, haScenarioID)
	requireOwnEvidence(t, s11, "S11/complete")
	if s11.Limits["backup_limit"] != cfg.BackupLimit.String() || s11.Measurements["backupCompletedWithin"] == nil {
		t.Fatalf("S11 limits = %v, measurements = %v", s11.Limits, s11.Measurements)
	}
	if !slices.Equal(f.writes, []string{"namespace", "secret", "cluster", clientContainer, "backup"}) {
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
	if !slices.ContainsFunc(rec.Notes, func(n string) bool { return strings.Contains(n, "logs were not checked") }) {
		t.Fatalf("S11 notes = %v, want them to say the Operator's logs were not checked", rec.Notes)
	}
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
		}, want: testCredential},
		"base64 credential in status": {tamperStatus: func(s *databasev1alpha1.CubridBackupStatus) {
			s.Conditions[0].Message = "uploaded with " + base64.RawURLEncoding.EncodeToString([]byte(testSecretKey))
		}, want: testCredential},
		"credential in the manifest": {tamper: func(a *fakeArtifact) { a.manifest.OperationID = testAccessKey },
			want: testCredential},
		"manifest stored in another layout": {tamper: func(a *fakeArtifact) {
			a.encode = func(m instancemanager.BackupManifest) ([]byte, error) { return json.Marshal(m) }
		}, want: "stored file"},
		"source instance not a member": {tamper: func(a *fakeArtifact) { a.manifest.SourceInstance = "ha-9" },
			tamperStatus: func(s *databasev1alpha1.CubridBackupStatus) { s.TargetInstance = "ha-9" },
			want:         "not a member"},
		"object size in the manifest": {tamper: func(a *fakeArtifact) { a.manifest.Objects[0].SizeBytes++ },
			want: "has 1000 bytes"},
		"no objects":  {tamper: func(a *fakeArtifact) { a.manifest.Objects = nil }, want: "lists no objects"},
		"database":    {tamper: func(a *fakeArtifact) { a.manifest.Database = "otherdb" }, want: "otherdb"},
		"level":       {tamper: func(a *fakeArtifact) { a.manifest.Level = 1 }, want: "level is 1"},
		"source role": {tamper: func(a *fakeArtifact) { a.manifest.SourceRole = "master" }, want: "sourceRole"},
		"fallback without the master": {tamperStatus: func(s *databasev1alpha1.CubridBackupStatus) {
			s.FallbackUsed = true
		}, want: "sourceRole"},
		"cubridVersion": {tamper: func(a *fakeArtifact) { a.manifest.CubridVersion = "11.3" },
			want: "cubridVersion \"11.3\" is not the cluster's spec.version"},
		"status cubridVersion": {tamperStatus: func(s *databasev1alpha1.CubridBackupStatus) {
			s.Artifact.CubridVersion = "11.5"
		}, want: "status.artifact.cubridVersion"},
		"duplicate object key": {tamper: func(a *fakeArtifact) {
			a.manifest.Objects = append(a.manifest.Objects, a.manifest.Objects[0])
		}, want: "twice"},
		"attempt not 32 hex digits": {tamper: func(a *fakeArtifact) { a.move(testAttempt, "attempt-0011") },
			want: "attempt-"},
		"another backup's prefix": {tamper: func(a *fakeArtifact) { a.move(testBackupUID, "another-backup") },
			want: "run's prefix"},
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
	if err == nil || !strings.Contains(err.Error(), testCredential) {
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
	f.onRead = func(read int) error {
		if read == 2 {
			cancel()
		}
		return nil
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

func TestRunS11CompleteFailsOnAJSONEscapedCredentialInTheStatus(t *testing.T) {
	cfg := testS11Config(t)
	cfg.SeedSecret = "escaped"
	// The status is read as JSON, which writes < and > escaped.
	access := "lab<access>key"
	f := newS11Fixture(t, cfg, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "escaped",
		Namespace: cfg.SeedSecretNamespace}, Data: map[string][]byte{seedAccessKey: []byte(access),
		seedSecretKey: []byte(testSecretKey)}})
	f.accessKey = access
	f.tamperStatus = func(s *databasev1alpha1.CubridBackupStatus) { s.Conditions[0].Message = "with " + access }
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), testCredential) {
		t.Fatalf("RunHA() error = %v, want the escaped credential", err)
	}
}

func TestRunS11CompleteFailsOnACredentialInAnEarlierPhase(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	// Only the Running phase carries it; the final status does not.
	f.tamperPhase = func(s *databasev1alpha1.CubridBackupStatus) {
		if s.Phase == databasev1alpha1.BackupPhaseRunning {
			s.Conditions[0].Message = "starting with " + testSecretKey
		}
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), testCredential) || !strings.Contains(err.Error(), "Running") {
		t.Fatalf("RunHA() error = %v, want the credential of the Running phase", err)
	}
	requireNoSecretInEvidence(t, cfg.EvidenceDir)
}

func TestRunS11CompleteIsBlockedWhenTheArtifactCannotBeChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		getErr func(cancel context.CancelFunc) func(context.Context, string) error
		want   string
	}{
		"the run ends during a download": {getErr: func(cancel context.CancelFunc) func(context.Context, string) error {
			return func(ctx context.Context, key string) error {
				if strings.HasSuffix(key, testFirstBackupKey) {
					cancel()
					return ctx.Err()
				}
				return nil
			}
		}, want: "context canceled"},
		"the store answers with another error": {getErr: func(context.CancelFunc) func(context.Context, string) error {
			return func(_ context.Context, key string) error {
				if strings.HasSuffix(key, testFirstBackupKey) {
					return errors.New("connection reset by peer")
				}
				return nil
			}
		}, want: "connection reset"},
		"the manifest cannot be read": {getErr: func(context.CancelFunc) func(context.Context, string) error {
			return func(_ context.Context, key string) error {
				if strings.HasSuffix(key, "/manifest.json") {
					return errors.New("unexpected EOF")
				}
				return nil
			}
		}, want: "unexpected EOF"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11Config(t)
			f := newS11Fixture(t, cfg)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.store.getErr = tc.getErr(cancel)
			err := f.runner.RunHA(ctx, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunHA() error = %v, want %q", err, tc.want)
			}
			_, lab, s11 := s11Scenario(t, cfg.EvidenceDir)
			if lab.Result != evidence.Pass || s11.Result != evidence.Blocked || len(s11.NeverEvents) != 0 {
				t.Fatalf("LAB01 = %s, S11 = %+v; want S11 blocked without an event that must never happen",
					lab.Result, s11)
			}
			f.requireForwardStopped(t)
		})
	}
}

func TestRunS11CompleteIsBlockedWithoutTheObjectStore(t *testing.T) {
	for name, change := range map[string]func(*s11Fixture){
		"port-forward": func(f *s11Fixture) {
			f.runner.forward = func(context.Context, string, string, string) (string, func(), error) {
				return "", nil, errors.New(`services "seaweedfs" not found`)
			}
		},
		"client": func(f *s11Fixture) {
			f.runner.openStore = func(string, string, string, bool) (objectStore, error) {
				return nil, errors.New("invalid endpoint")
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11Config(t)
			f := newS11Fixture(t, cfg)
			change(f)
			if err := f.runner.RunHA(t.Context(), cfg); err == nil {
				t.Fatal("RunHA() passed")
			}
			if _, _, s11 := s11Scenario(t, cfg.EvidenceDir); s11.Result != evidence.Blocked {
				t.Fatalf("S11 = %+v, want blocked", s11)
			}
			f.requireForwardStopped(t)
		})
	}
}

func TestRunS11CompleteIsBlockedByAnAPIErrorReadingTheBackup(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	f.onRead = func(read int) error {
		if read == 2 {
			return apierrors.NewForbidden(databasev1alpha1.GroupVersion.WithResource("cubridbackups").GroupResource(),
				s11BackupName, errors.New("RBAC"))
		}
		return nil
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("RunHA() error = %v, want the refused reading", err)
	}
	if _, _, s11 := s11Scenario(t, cfg.EvidenceDir); s11.Result != evidence.Blocked {
		t.Fatalf("S11 = %+v, want blocked", s11)
	}
}

func TestRunS11CompleteFailsOnAManifestOverTheLimit(t *testing.T) {
	limit := maxManifestSize
	maxManifestSize = 100
	t.Cleanup(func() { maxManifestSize = limit })
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "larger than 100 bytes") {
		t.Fatalf("RunHA() error = %v, want the manifest over the limit", err)
	}
	if _, _, s11 := s11Scenario(t, cfg.EvidenceDir); s11.Result != evidence.Fail {
		t.Fatalf("S11 = %+v, want a failure", s11)
	}
}

func TestRunS11CompleteStaysBlockedWhenItsEvidenceCannotBeWritten(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	f.runner.forward = func(context.Context, string, string, string) (string, func(), error) {
		// A directory where the record goes: the record cannot be written.
		if err := os.MkdirAll(filepath.Join(cfg.EvidenceDir, s11RecordFile, "in-the-way"), 0o755); err != nil {
			t.Fatal(err)
		}
		return "", nil, errors.New("port-forward refused")
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	_, _, s11 := s11Scenario(t, cfg.EvidenceDir)
	if s11.Result != evidence.Blocked || !strings.Contains(s11.Reason, "write S11 evidence") {
		t.Fatalf("S11 = %+v, want blocked, with the evidence that could not be written", s11)
	}
}
