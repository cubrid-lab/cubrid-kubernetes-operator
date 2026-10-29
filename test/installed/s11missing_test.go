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
	"os"
	"path"
	"path/filepath"
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
	testMissingUID = "missing-backup-uid"
	// testRunningReason is the Ready reason of a running backup (pollBackup).
	testRunningReason = "BackupRunning"
)

func testS11MissingConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupMissingDestination = true
	return cfg
}

// s11MissingFixture plays, after S11 complete, an Operator that takes the
// backup to the missing bucket: its upload fails at the first object, as
// UploadBackup's Put does when the store holds no such bucket, and the
// backup ends Failed with the Instance Manager's reason.
type s11MissingFixture struct {
	*s11Fixture
	// missingPhases are the phases the fake Operator reports, one per
	// reading of the backup; the last is kept. createBucket makes the store
	// create the bucket and keep the artifact, as a store that creates a
	// bucket on its first write would. tamperMissing changes the status of a
	// reading; onMissingRead runs first, and its error is the reading's.
	missingPhases []databasev1alpha1.CubridBackupPhase
	createBucket  bool
	tamperMissing func(read int, s *databasev1alpha1.CubridBackupStatus)
	onMissingRead func(read int) error
	// deleteErr answers the deletion of the backup.
	deleteErr error

	missing      *databasev1alpha1.CubridBackup
	missingReads int
	// deleted are the backups the run deleted, with the UID it required.
	deleted []string
}

func newS11MissingFixture(t *testing.T, cfg HAConfig) *s11MissingFixture {
	t.Helper()
	f := &s11MissingFixture{s11Fixture: newS11Fixture(t, cfg),
		missingPhases: []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhasePending,
			databasev1alpha1.BackupPhaseRunning, databasev1alpha1.BackupPhaseUploading,
			databasev1alpha1.BackupPhaseFailed}}
	created, read := f.backupCreated, f.backupRead
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		if backup.Name != s11MissingBackupName {
			return created(ctx, c, backup)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		backup.UID = testMissingUID
		f.missing = backup.DeepCopy()
		return nil
	}
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name != s11MissingBackupName {
			return read(ctx, c, key)
		}
		return f.advanceMissing(ctx, c, key)
	}
	f.backupDeleted = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup,
		opts ...client.DeleteOption) error {
		options := (&client.DeleteOptions{}).ApplyOptions(opts)
		uid := ""
		if options.Preconditions != nil && options.Preconditions.UID != nil {
			uid = string(*options.Preconditions.UID)
		}
		f.mu.Lock()
		f.deleted = append(f.deleted, backup.Name+"/"+uid)
		f.mu.Unlock()
		if f.deleteErr != nil {
			return f.deleteErr
		}
		return c.Delete(ctx, backup)
	}
	f.markersOf = func(string) []string { return nil }
	return f
}

// advanceMissing writes the status of the next phase of the backup to the
// missing bucket, as the Operator would.
func (f *s11MissingFixture) advanceMissing(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	backup := &databasev1alpha1.CubridBackup{}
	if err := c.Get(ctx, key, backup); err != nil {
		return nil
	}
	f.mu.Lock()
	f.missingReads++
	read := f.missingReads
	phase := f.missingPhases[min(read, len(f.missingPhases))-1]
	onRead, tamper, createBucket := f.onMissingRead, f.tamperMissing, f.createBucket
	f.mu.Unlock()
	if onRead != nil {
		if err := onRead(read); err != nil {
			return err
		}
	}
	started := metav1.NewTime(time.Now().Add(-time.Second))
	dest := backup.Spec.Destination.ObjectStorage
	attempt := path.Join(dest.Prefix, testClusterUID, databaseName, string(backup.UID), testAttempt)
	s := &backup.Status
	s.Phase, s.ClusterUID, s.StartedAt = phase, testClusterUID, &started
	s.Conditions = []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse,
		Reason: testRunningReason, Message: "backup in progress", LastTransitionTime: started}}
	if phase != databasev1alpha1.BackupPhasePending {
		s.TargetInstance, s.TargetRole = testBackupTarget, string(databasev1alpha1.RoleSlave)
	}
	switch phase {
	case databasev1alpha1.BackupPhaseUploading:
		s.Conditions[0].Reason, s.Conditions[0].Message = "UploadRunning", "uploading artifact"
		if createBucket {
			f.store.put(dest.Bucket, attempt+"/"+testFirstBackupKey, []byte("v"))
		}
	case databasev1alpha1.BackupPhaseFailed:
		s.CompletedAt = &started
		s.Conditions[0].Reason, s.Conditions[0].Message = s11MissingReason, "upload failed: put "+dest.Bucket+"/"+
			attempt+"/"+testFirstBackupKey+": The specified bucket does not exist."
	case databasev1alpha1.BackupPhaseCompleted:
		s.CompletedAt = &started
		s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: "s3://" + dest.Bucket + "/" + attempt +
			"/manifest.json", ManifestDigest: strings.Repeat("a", 64), SizeBytes: 1, Database: databaseName,
			Level: databasev1alpha1.CubridBackupFull}
		s.Conditions[0].Status, s.Conditions[0].Reason, s.Conditions[0].Message = metav1.ConditionTrue,
			"BackupCompleted", "backup completed"
		if createBucket {
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

// missingOf runs the fixture and returns S11 missing-destination, requiring
// LAB01 and S11 complete passed.
func missingOf(t *testing.T, f *s11MissingFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	scenarios := missingScenarios(t, f.cfg.EvidenceDir)
	for _, s := range scenarios[:2] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01 and S11 complete passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, scenarios[1], "S11/complete")
	requireOwnEvidence(t, scenarios[2], "S11/missing-destination")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	f.requireForwardStopped(t)
	return scenarios[2], err
}

func missingScenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s11MissingLane || len(summary.Scenarios) != 3 || summary.Scenarios[2].ID != s11ID ||
		summary.Scenarios[2].Variant != s11MissingVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/missing-destination in lane %s", summary,
			s11MissingLane)
	}
	return summary.Scenarios
}

func readMissingRecord(t *testing.T, dir string) s11MissingRecord {
	t.Helper()
	var rec s11MissingRecord
	data, err := os.ReadFile(filepath.Join(dir, s11MissingRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 missing-destination record = %s, %v", data, err)
	}
	return rec
}

// missingBucket is the bucket the run must name for its run ID.
func missingBucket(t *testing.T, dir string) string {
	t.Helper()
	return s11MissingBucketPrefix + readHARecord(t, dir).RunID
}

func TestRunS11MissingDestinationPassesWhenTheBackupFails(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	missing, err := missingOf(t, f)
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if summary := readSummary(t, cfg.EvidenceDir); !summary.Passed || missing.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want every scenario passed", summary)
	}
	// The summary keeps no reason for a pass; the record names both logs.
	if notes := readMissingRecord(t, cfg.EvidenceDir).Notes; !slices.Contains(notes, logsNote()) {
		t.Fatalf("S11 missing-destination notes = %v, want both logs named as searched", notes)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,backup" {
		t.Fatalf("writes = %v", f.writes)
	}
	if missing.Limits["backup_limit"] != cfg.BackupLimit.String() ||
		missing.Measurements["failedWithin"] == evidence.Unknown {
		t.Fatalf("S11 missing-destination limits = %v, measurements = %v", missing.Limits, missing.Measurements)
	}
	if missing.CleanupSucceeded == nil || !*missing.CleanupSucceeded {
		t.Fatalf("S11 missing-destination = %+v, want its cleanup recorded as succeeded", missing)
	}
	for _, rel := range []string{s11MissingRecordFile, s11MissingSourceDataCheckFile} {
		if !slices.Contains(missing.Evidence, rel) {
			t.Fatalf("S11 missing-destination evidence = %v, want %s", missing.Evidence, rel)
		}
	}
	requireMissingSpec(t, f)
	requireMissingRecord(t, f)
	// The run deleted the backup it created, by its UID, and nothing else.
	if !slices.Equal(f.deleted, []string{s11MissingBackupName + "/" + testMissingUID}) {
		t.Fatalf("deleted = %v, want only the backup the variant created, by its UID", f.deleted)
	}
	// No bucket was created or written by the run.
	if exists, _ := f.store.BucketExists(t.Context(), missingBucket(t, cfg.EvidenceDir)); exists {
		t.Fatal("the missing bucket exists after the run")
	}
	if len(f.store.puts) != 0 || len(f.store.removes) != 0 {
		t.Fatalf("the run wrote %v and removed %v in the object store; want nothing", f.store.puts, f.store.removes)
	}
	requireNamespaceGone(t, f.haFixture)
}

// requireMissingSpec checks the backup the run created.
func requireMissingSpec(t *testing.T, f *s11MissingFixture) {
	t.Helper()
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	got := f.missing
	if got == nil || got.Namespace != f.cfg.Namespace || got.Spec.ClusterRef.Name != haClusterName ||
		got.Spec.Database != databaseName || got.Spec.Destination.Type != databasev1alpha1.DestinationObjectStorage {
		t.Fatalf("backup = %+v, want the run's database in the run namespace", got)
	}
	dest := got.Spec.Destination.ObjectStorage
	if dest == nil || dest.Bucket != s11MissingBucketPrefix+lab01.RunID ||
		dest.Prefix != "lab-runs/"+lab01.RunID+"/missing-destination" || !s3Bucket.MatchString(dest.Bucket) {
		t.Fatalf("destination = %+v, want the missing bucket of the run", dest)
	}
}

// requireMissingRecord checks the record of a passed run.
func requireMissingRecord(t *testing.T, f *s11MissingFixture) {
	t.Helper()
	rec := readMissingRecord(t, f.cfg.EvidenceDir)
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	b := rec.Bucket
	if b == nil || b.Name != s11MissingBucketPrefix+lab01.RunID || !b.AbsentBefore || !b.AbsentAtEnd ||
		b.CheckedBefore == nil || b.CheckedAtEnd == nil || len(b.ObjectsAtEnd) != 0 || b.ManifestAtEnd {
		t.Fatalf("bucket = %+v, want it absent before and at the end", b)
	}
	if rec.Backup != s11MissingBackupName || rec.BackupUID != testMissingUID || rec.RequestedAt == nil ||
		rec.JudgedAt == nil || rec.RequestedAt.After(*rec.JudgedAt) || rec.FailedWithin == "" ||
		rec.Artifact != nil || !strings.HasPrefix(rec.Failure, s11MissingReason+": upload failed: put "+b.Name+"/") {
		t.Fatalf("record = %+v, want the request, the failure and the verdict", rec)
	}
	if rec.StoreError != "The specified bucket does not exist." || rec.StoreErrorKind != storeErrorNoSuchBucket {
		t.Fatalf("store error = %q (%s), want the store's answer observed", rec.StoreError, rec.StoreErrorKind)
	}
	if !strings.Contains(rec.Residue, "staging directory") {
		t.Fatalf("residue = %q, want the unchecked staging directory named", rec.Residue)
	}
	requireMissingChecks(t, rec, lab01)
}

// requireMissingChecks checks the phases, the source, the manager, the
// credentials and the cleanup in the record of a passed run.
func requireMissingChecks(t *testing.T, rec s11MissingRecord, lab01 haRecord) {
	t.Helper()
	phases := make([]databasev1alpha1.CubridBackupPhase, 0, len(rec.Phases))
	for _, p := range rec.Phases {
		if p.At.Location() != time.UTC || p.At.Before(*rec.RequestedAt) {
			t.Fatalf("phase %+v, want a UTC time after the request", p)
		}
		phases = append(phases, databasev1alpha1.CubridBackupPhase(p.Phase))
	}
	if !slices.Equal(phases, []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhasePending,
		databasev1alpha1.BackupPhaseRunning, databasev1alpha1.BackupPhaseUploading,
		databasev1alpha1.BackupPhaseFailed}) {
		t.Fatalf("phases = %v, want each change recorded", phases)
	}
	if rec.Source == nil || rec.Source.Restarted == nil || *rec.Source.Restarted != 0 ||
		rec.Source.Master != testHAPrimary || !rec.Source.DataRulesHeld {
		t.Fatalf("source = %+v, want it unchanged", rec.Source)
	}
	if rec.ManagerBefore == nil || rec.ManagerAfter == nil || len(rec.ManagerAfter.Pods) == 0 ||
		rec.ManagerAfter.DeploymentUID != lab01.Operator.DeploymentUID {
		t.Fatalf("manager = %+v, %+v; want the installed manager recorded", rec.ManagerBefore, rec.ManagerAfter)
	}
	if rec.Credentials == nil || rec.Credentials.StatusesChecked < 4 || !rec.Credentials.StatusChecked ||
		len(rec.Credentials.FoundIn) != 0 {
		t.Fatalf("credentials = %+v, want every status searched", rec.Credentials)
	}
	if !strings.Contains(rec.Cleanup, "deleted") || !slices.Contains(rec.Notes, logsNote()) {
		t.Fatalf("record = %+v, want the cleanup and the searched logs recorded", rec)
	}
	requireLogsSearched(t, rec.Credentials, testBackupTarget)
}

func TestRunS11MissingDestinationFailsWhenTheBackupCompletes(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.missingPhases[3], f.createBucket = databasev1alpha1.BackupPhaseCompleted, true
	missing, err := missingOf(t, f)
	if err == nil || missing.Result != evidence.Fail || len(missing.NeverEvents) < 2 {
		t.Fatalf("S11 missing-destination = %+v, want Completed and the artifact as never-events", missing)
	}
	for _, want := range []string{"Completed", "status.artifact", "exists at the end"} {
		if !strings.Contains(missing.Reason, want) {
			t.Fatalf("S11 missing-destination reason = %q, want %q", missing.Reason, want)
		}
	}
	rec := readMissingRecord(t, cfg.EvidenceDir)
	if rec.Bucket == nil || rec.Bucket.AbsentAtEnd || !rec.Bucket.ManifestAtEnd || len(rec.Bucket.ObjectsAtEnd) != 2 ||
		rec.Artifact == nil {
		t.Fatalf("record = %+v, bucket %+v; want what the store held at the end", rec, rec.Bucket)
	}
}

func TestRunS11MissingDestinationFailsWhenTheBucketIsCreated(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.createBucket = true
	missing, err := missingOf(t, f)
	// The scenario contract does not list it as a never-event: it fails.
	if err == nil || missing.Result != evidence.Fail || len(missing.NeverEvents) != 0 ||
		!strings.Contains(missing.Reason, "exists at the end") {
		t.Fatalf("S11 missing-destination = %+v, want a failure for the bucket at the end", missing)
	}
	if rec := readMissingRecord(t, cfg.EvidenceDir); !strings.Contains(rec.Residue, "exists at the end") {
		t.Fatalf("residue = %q, want the bucket named", rec.Residue)
	}
}

func TestRunS11MissingDestinationJudgesAFailureByWhetherItReachedTheUpload(t *testing.T) {
	for name, tc := range map[string]struct {
		reason, message string
		want            evidence.Result
	}{
		// The backup ended before its upload: the missing bucket was not
		// exercised, so S11 missing-destination could not be judged.
		"backupdb": {reason: s11MissingReason, message: "backupdb failed: exit status 1", want: evidence.Blocked},
		"engine refused": {reason: s11MissingReason, message: "backup refused: the engine version of this member " +
			"could not be read from cubrid_rel", want: evidence.Blocked},
		"another Ready reason": {reason: "RecordedTargetNotStarted",
			message: "the backup was not started on its recorded target ha-0", want: evidence.Blocked},
		// The upload ran and failed, but not for its destination.
		"upload verification": {reason: s11MissingReason, message: "upload failed: upload verification failed for " +
			"lab-runs/x/backup/appdb_bk0v000 (size got=0 want=1 err=<nil>)", want: evidence.Fail},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11MissingConfig(t)
			f := newS11MissingFixture(t, cfg)
			f.tamperMissing = func(_ int, s *databasev1alpha1.CubridBackupStatus) {
				if s.Phase == databasev1alpha1.BackupPhaseFailed {
					s.Conditions[0].Reason, s.Conditions[0].Message = tc.reason, tc.message
				}
			}
			missing, err := missingOf(t, f)
			if err == nil || missing.Result != tc.want || !strings.Contains(missing.Reason, tc.message) ||
				len(missing.NeverEvents) != 0 {
				t.Fatalf("S11 missing-destination = %+v, want %s naming the other reason", missing, tc.want)
			}
			if !slices.Equal(f.deleted, []string{s11MissingBackupName + "/" + testMissingUID}) {
				t.Fatalf("deleted = %v, want the backup deleted", f.deleted)
			}
		})
	}
}

func TestRunS11MissingDestinationFailsOnAnArtifactOfAFailedBackup(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.tamperMissing = func(_ int, s *databasev1alpha1.CubridBackupStatus) {
		if s.Phase == databasev1alpha1.BackupPhaseFailed {
			s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: "s3://elsewhere/manifest.json"}
		}
	}
	missing, err := missingOf(t, f)
	if err == nil || missing.Result != evidence.Fail || len(missing.NeverEvents) != 1 ||
		!strings.Contains(missing.NeverEvents[0], "status.artifact") ||
		strings.Count(missing.Reason, "status.artifact") != 1 {
		t.Fatalf("S11 missing-destination = %+v, want the artifact as one never-event, named once", missing)
	}
}

func TestRunS11MissingDestinationIsBlockedWhenTheRunEndsDuringTheWait(t *testing.T) {
	cfg := testS11MissingConfig(t)
	cfg.BackupLimit = time.Minute
	f := newS11MissingFixture(t, cfg)
	f.missingPhases = f.missingPhases[:2]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.onMissingRead = func(read int) error {
		if read == 2 {
			cancel()
		}
		return nil
	}
	if err := f.runner.RunHA(ctx, cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	missing := missingScenarios(t, cfg.EvidenceDir)[2]
	if missing.Result != evidence.Blocked || !strings.Contains(missing.Reason, "the run ended") {
		t.Fatalf("S11 missing-destination = %+v, want blocked: the run ended", missing)
	}
	if !slices.Equal(f.deleted, []string{s11MissingBackupName + "/" + testMissingUID}) ||
		missing.CleanupSucceeded == nil || !*missing.CleanupSucceeded {
		t.Fatalf("deleted = %v, S11 missing-destination = %+v; want the backup deleted after the run ended",
			f.deleted, missing)
	}
}

func TestRunS11MissingDestinationIsBlockedWhenTheManagerChanges(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.onMissingRead = func(read int) error {
		if read == 1 {
			pod := &corev1.Pod{}
			key := client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: testManagerPodName}
			if err := f.haFixture.store.Get(t.Context(), key, pod); err != nil {
				t.Fatal(err)
			}
			pod.Status.ContainerStatuses[0].ContainerID = testRestarted
			if err := f.haFixture.store.Status().Update(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	missing, err := missingOf(t, f)
	if err == nil || missing.Result != evidence.Blocked || !strings.Contains(missing.Reason, "manager") {
		t.Fatalf("S11 missing-destination = %+v, want blocked by the changed manager", missing)
	}
	if rec := readMissingRecord(t, cfg.EvidenceDir); rec.ManagerAfter == nil ||
		rec.ManagerAfter.Pods[0].ContainerID != testRestarted {
		t.Fatalf("manager after = %+v, want the changed manager recorded", rec.ManagerAfter)
	}
}

func TestRunS11MissingDestinationFailsWhenNotFailedWithinTheBackupLimit(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.backupLimitAfterS11(300 * time.Millisecond)
	f.missingPhases = f.missingPhases[:3]
	missing, err := missingOf(t, f)
	if err == nil || missing.Result != evidence.Fail || !strings.Contains(missing.Reason, "backup_limit") ||
		!strings.Contains(missing.Reason, "Uploading") {
		t.Fatalf("S11 missing-destination = %+v, want the backup limit exceeded with the last phase", missing)
	}
	if missing.Measurements["failedWithin"] != evidence.Unknown {
		t.Fatalf("S11 missing-destination measurements = %v, want failedWithin unknown", missing.Measurements)
	}
	if rec := readMissingRecord(t, cfg.EvidenceDir); rec.Bucket == nil || !rec.Bucket.AbsentAtEnd {
		t.Fatalf("record = %+v, want the bucket checked at the end also after the limit", rec)
	}
}

// backupLimitAfterS11 lets S11 complete pass and then bounds the backup to
// the missing bucket by limit: the readings of S11 complete count as many.
func (f *s11MissingFixture) backupLimitAfterS11(limit time.Duration) {
	f.cfg.BackupLimit = limit
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseCompleted}
}

func TestRunS11MissingDestinationIsBlockedWhenTheBackupCannotBeRead(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.backupLimitAfterS11(300 * time.Millisecond)
	f.onMissingRead = func(int) error { return apierrors.NewServiceUnavailable("etcd is unavailable") }
	missing, err := missingOf(t, f)
	if err == nil || missing.Result != evidence.Blocked || !strings.Contains(missing.Reason, "etcd is unavailable") {
		t.Fatalf("S11 missing-destination = %+v, want blocked: the backup was never read", missing)
	}
}

func TestRunS11MissingDestinationIsBlockedWhenTheBucketIsNotKnownMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *s11MissingFixture)
		want   string
	}{
		"the bucket exists": {change: func(f *s11MissingFixture) {
			// The store holds the bucket of the run, whatever its ID.
			f.store.bucketErr = func(b string) error {
				if strings.HasPrefix(b, s11MissingBucketPrefix) && !slices.Contains(f.store.buckets, b) {
					f.store.buckets = append(f.store.buckets, b)
				}
				return nil
			}
		}, want: "exists"},
		"the store cannot be asked": {change: func(f *s11MissingFixture) {
			f.store.bucketErr = func(string) error { return errors.New(testReset) }
		}, want: testReset},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11MissingConfig(t)
			f := newS11MissingFixture(t, cfg)
			tc.change(f)
			missing, err := missingOf(t, f)
			if err == nil || missing.Result != evidence.Blocked || !strings.Contains(missing.Reason, tc.want) {
				t.Fatalf("S11 missing-destination = %+v, want blocked by %q", missing, tc.want)
			}
			if f.missing != nil || len(f.deleted) != 0 {
				t.Fatal("a backup was requested to a bucket not known to be missing")
			}
			if rec := readMissingRecord(t, cfg.EvidenceDir); !strings.Contains(rec.Cleanup, "nothing") {
				t.Fatalf("cleanup = %q, want nothing to delete", rec.Cleanup)
			}
		})
	}
}

func TestRunS11MissingDestinationIsBlockedWhenS11CompleteDidNotPass(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	scenarios := missingScenarios(t, cfg.EvidenceDir)
	if complete := scenarios[1]; complete.Result != evidence.Fail {
		t.Fatalf("S11 complete = %+v, want failed", complete)
	}
	if missing := scenarios[2]; missing.Result != evidence.Blocked ||
		!strings.Contains(missing.Reason, "S11 complete did not pass") {
		t.Fatalf("S11 missing-destination = %+v, want blocked by S11 complete", missing)
	}
	if f.missing != nil {
		t.Fatal("S11 missing-destination ran although S11 complete did not pass")
	}
}

func TestRunS11MissingDestinationFailsOnACredentialInTheStatus(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.tamperMissing = func(_ int, s *databasev1alpha1.CubridBackupStatus) {
		if s.Phase == databasev1alpha1.BackupPhaseFailed {
			s.Conditions[0].Message += " (signed with " + testSecretKey + ")"
		}
	}
	missing, err := missingOf(t, f)
	if err == nil || missing.Result != evidence.Fail || len(missing.NeverEvents) == 0 ||
		!strings.Contains(missing.NeverEvents[0], testCredential) {
		t.Fatalf("S11 missing-destination = %+v, want the credential in the status as a never-event", missing)
	}
}

func TestRunS11MissingDestinationFailsWhenTheSourceChanged(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.onMissingRead = func(read int) error {
		if read == 1 {
			f.restartSourceMember(t.Context(), f.haFixture.store)
		}
		return nil
	}
	missing, err := missingOf(t, f)
	if err == nil || missing.Result != evidence.Fail || !strings.Contains(missing.Reason, "source") ||
		!strings.Contains(missing.Reason, "restarted") {
		t.Fatalf("S11 missing-destination = %+v, want the changed source", missing)
	}
}

func TestRunS11MissingDestinationRecordsABackupItCouldNotDelete(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	f.deleteErr = apierrors.NewForbidden(databasev1alpha1.GroupVersion.WithResource("cubridbackups").GroupResource(),
		s11MissingBackupName, errors.New("forbidden"))
	missing, err := missingOf(t, f)
	if err != nil || missing.Result != evidence.Pass {
		t.Fatalf("S11 missing-destination = %+v, error %v; want a pass: deleting the backup is not part of the "+
			"verdict", missing, err)
	}
	if missing.CleanupSucceeded == nil || *missing.CleanupSucceeded {
		t.Fatalf("S11 missing-destination = %+v, want the failed cleanup recorded", missing)
	}
	if rec := readMissingRecord(t, cfg.EvidenceDir); !strings.Contains(rec.Cleanup, testForbidden) ||
		!strings.Contains(rec.Cleanup, s11MissingBackupName) {
		t.Fatalf("cleanup = %q, want the backup left named with the error", rec.Cleanup)
	}
}

func TestRunHARefusesBackupMissingDestinationWithoutItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(cfg *HAConfig)
		want   string
	}{
		"without -backup-bucket": {change: func(cfg *HAConfig) { cfg.BackupBucket = "" }, want: "-backup-bucket"},
		"with -restore": {change: func(cfg *HAConfig) {
			cfg.Restore, cfg.RestoreLimit = true, time.Minute
		}, want: "-restore"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11MissingConfig(t)
			tc.change(&cfg)
			f := newS11MissingFixture(t, testS11MissingConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "-backup-missing-destination") {
				t.Fatalf("RunHA() error = %v, want -backup-missing-destination refused %s", err, name)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}

func TestRunS11WithoutBackupMissingDestinationKeepsItsLane(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11MissingFixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if summary, _, _ := s11Scenario(t, cfg.EvidenceDir); !summary.Passed || f.missing != nil {
		t.Fatalf("summary = %+v; want S11 complete alone, without S11 missing-destination", summary)
	}
}
