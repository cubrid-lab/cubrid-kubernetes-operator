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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testSignatureSecretUID = "signature-secret-uid"
	// testUnknownKeyCode is the S3 error code of an access key the store
	// does not know.
	testUnknownKeyCode = "InvalidAccessKeyId"
	// testKindBackup is the kind of a CubridBackup an Event involves.
	testKindBackup = "CubridBackup"
	// testSignatureAnswer is how an S3 store answers a request signed with
	// another secret key than the one it holds for the access key.
	testSignatureAnswer = "The request signature we calculated does not match the signature you provided. " +
		"Check your key and signing method."
)

// testSignatureDeleted are the deletions of a run that created all three
// objects.
var testSignatureDeleted = []string{"CubridBackup/" + s11SignatureBackupName + "/" + testDeniedUID,
	testKindCluster + "/" + s11SignatureClusterName + "/" + testDeniedClusterUID,
	"Secret/" + s11SignatureSecretName + "/" + testSignatureSecretUID}

func testS11SignatureConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupSignatureMismatch = true
	cfg.BackupSignatureMismatchReadyLimit = 5 * time.Second
	return cfg
}

// signatureRefusal is the store's answer to the seed access key with a
// wrong secret key, as minio-go returns it.
func signatureRefusal() error {
	return minio.ErrorResponse{Code: signatureDoesNotMatch, Message: testSignatureAnswer, StatusCode: 403}
}

// newS11SignatureFixture is the S11 denied-credentials fixture with the
// names of S11 signature-mismatch: the store refuses a wrong secret key with
// SignatureDoesNotMatch, and the member's upload is refused with the same
// answer.
func newS11SignatureFixture(t *testing.T, cfg HAConfig) *s11DeniedFixture {
	t.Helper()
	f := newS11DeniedFixture(t, cfg)
	f.refusal = signatureRefusal()
	created, read := f.backupCreated, f.backupRead
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		if backup.Name != s11SignatureBackupName {
			return created(ctx, c, backup)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		backup.UID = testDeniedUID
		f.denied = backup.DeepCopy()
		return nil
	}
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name != s11SignatureBackupName {
			return read(ctx, c, key)
		}
		return f.advanceDenied(ctx, c, key)
	}
	f.tamperDenied = signatureStatus(nil)
	return f
}

// signatureStatus returns a tamperDenied that names the standalone member
// of S11 signature-mismatch and quotes the store's signature answer in the
// failure, then runs then.
func signatureStatus(then func(read int, s *databasev1alpha1.CubridBackupStatus)) func(int,
	*databasev1alpha1.CubridBackupStatus) {
	return func(read int, s *databasev1alpha1.CubridBackupStatus) {
		if s.TargetInstance != "" {
			s.TargetInstance = s11SignatureClusterName + "-0"
		}
		if s.Phase == databasev1alpha1.BackupPhaseFailed {
			s.Conditions[0].Message = strings.Replace(s.Conditions[0].Message, testDeniedAnswer, testSignatureAnswer, 1)
		}
		if then != nil {
			then(read, s)
		}
	}
}

// signatureOf runs the fixture and returns S11 signature-mismatch, requiring
// LAB01 and S11 complete passed.
func signatureOf(t *testing.T, f *s11DeniedFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	scenarios := signatureScenarios(t, f.cfg.EvidenceDir)
	for _, s := range scenarios[:2] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01 and S11 complete passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, scenarios[1], "S11/complete")
	requireOwnEvidence(t, scenarios[2], "S11/signature-mismatch")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	f.requireNoDeniedKeyInEvidence(t)
	f.requireForwardStopped(t)
	return scenarios[2], err
}

func signatureScenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s11SignatureLane || len(summary.Scenarios) != 3 || summary.Scenarios[2].ID != s11ID ||
		summary.Scenarios[2].Variant != s11SignatureVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/signature-mismatch in lane %s", summary,
			s11SignatureLane)
	}
	return summary.Scenarios
}

func readSignatureRecord(t *testing.T, dir string) s11DeniedRecord {
	t.Helper()
	var rec s11DeniedRecord
	data, err := os.ReadFile(filepath.Join(dir, s11SignatureRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 signature-mismatch record = %s, %v", data, err)
	}
	return rec
}

// signatureSecret is the Secret the run created with the seed access key
// and a wrong secret key.
func signatureSecret(f *s11DeniedFixture) *corev1.Secret {
	for _, s := range f.secrets {
		if s.Name == s11SignatureSecretName {
			return s
		}
	}
	return nil
}

func TestRunS11SignatureMismatchPassesWhenTheUploadIsRefused(t *testing.T) {
	cfg := testS11SignatureConfig(t)
	f := newS11SignatureFixture(t, cfg)
	wrong, err := signatureOf(t, f)
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	summary := readSummary(t, cfg.EvidenceDir)
	if !summary.Passed || wrong.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want every scenario passed", summary)
	}
	if !strings.Contains(summary.Notes[0], "S11 signature-mismatch: a full backup") {
		t.Fatalf("summary notes = %v, want S11 signature-mismatch named", summary.Notes)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,secret,signature-cluster,backup" {
		t.Fatalf("writes = %v", f.writes)
	}
	if wrong.CleanupSucceeded == nil || !*wrong.CleanupSucceeded ||
		wrong.Measurements["failedWithin"] == evidence.Unknown {
		t.Fatalf("S11 signature-mismatch = %+v, want its cleanup and failedWithin recorded", wrong)
	}
	for _, rel := range []string{s11SignatureRecordFile, s11SignatureSourceDataCheckFile} {
		if !slices.Contains(wrong.Evidence, rel) {
			t.Fatalf("S11 signature-mismatch evidence = %v, want %s", wrong.Evidence, rel)
		}
	}
	requireSignatureObjects(t, f)
	requireSignatureRecord(t, f)
	if !slices.Equal(f.deleted, testSignatureDeleted) {
		t.Fatalf("deleted = %v, want the backup, the cluster and the Secret the variant created, by UID", f.deleted)
	}
	if len(f.store.puts) != 0 || len(f.store.removes) != 0 {
		t.Fatalf("the run wrote %v and removed %v in the object store; want nothing", f.store.puts, f.store.removes)
	}
	requireNamespaceGone(t, f.haFixture)
	requireNothingRedacted(t, cfg.EvidenceDir)
}

// requireSignatureObjects checks the Secret, the cluster and the backup
// the run created.
func requireSignatureObjects(t *testing.T, f *s11DeniedFixture) {
	t.Helper()
	// The Secret holds the seed access key and a random secret key.
	secret := signatureSecret(f)
	if secret == nil || len(f.keys) != 2 || f.keys[0] != testAccessKey || f.keys[1] == testSecretKey ||
		len(f.keys[1]) < minSecretLength || string(secret.Data[seedAccessKey]) != testAccessKey ||
		string(secret.Data[seedSecretKey]) != f.keys[1] {
		t.Fatalf("Secret = %+v, keys = %d; want the seed access key with a wrong secret key", secret, len(f.keys))
	}
	if c := f.deniedCluster; c == nil || c.Spec.ObjectStorage == nil ||
		c.Spec.ObjectStorage.CredentialsSecretRef.Name != s11SignatureSecretName ||
		c.Spec.Topology.PromotableMembers != 1 || c.Spec.HighAvailability.Enabled {
		t.Fatalf("cluster = %+v, want a standalone cluster naming the signature-mismatch Secret", c)
	}
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	if dest := f.denied.Spec.Destination.ObjectStorage; dest == nil || dest.Bucket != testBackupBucket ||
		dest.Prefix != "lab-runs/"+lab01.RunID+"/signature-mismatch" ||
		f.denied.Spec.ClusterRef.Name != s11SignatureClusterName {
		t.Fatalf("backup = %+v, want the signature-mismatch cluster to the backup bucket under the variant's prefix",
			f.denied.Spec)
	}
}

// requireSignatureRecord checks the record of a passed run.
func requireSignatureRecord(t *testing.T, f *s11DeniedFixture) {
	t.Helper()
	cfg := f.cfg
	lab01 := readHARecord(t, cfg.EvidenceDir)
	rec := readSignatureRecord(t, cfg.EvidenceDir)
	if fault := rec.Fault; fault == nil || !fault.Refused || fault.Code != signatureDoesNotMatch ||
		fault.Answer != testSignatureAnswer || fault.AnswerKind != storeErrorAccessDenied {
		t.Fatalf("fault = %+v, want the store's SignatureDoesNotMatch recorded", fault)
	}
	if rec.Backup != s11SignatureBackupName || rec.StoreError != testSignatureAnswer ||
		rec.StoreErrorKind != storeErrorAccessDenied || rec.Artifact != nil || rec.CheckedAtEnd == nil ||
		len(rec.ObjectsAtEnd) != 0 || rec.ReadyLimit != cfg.BackupSignatureMismatchReadyLimit.String() ||
		rec.Cluster == nil || rec.Cluster.SecretUID != testSignatureSecretUID ||
		!strings.Contains(rec.Residue, "PVC of "+s11SignatureClusterName) {
		t.Fatalf("record = %+v, want the refused upload and nothing left under the prefix", rec)
	}
	requireDeniedChecks(t, rec, lab01)
}

// requireNothingRedacted fails when the redactor blanked anything in the
// evidence of a run whose keys never reached it: text such as "secret: x"
// would be blanked as a credential.
func requireNothingRedacted(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "[REDACTED]") {
			t.Errorf("%s holds a redaction: %s", path, data)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRunS11SignatureMismatchKeepsItsVerdictInTheProblems checks that the
// summary's problems and junit.xml name a blocked or failed S11
// signature-mismatch with its result, which the redactor would blank after
// a name that ends in "secret".
func TestRunS11SignatureMismatchKeepsItsVerdictInTheProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *s11DeniedFixture)
		want   evidence.Result
	}{
		"blocked": {change: func(f *s11DeniedFixture) { f.refusal = nil }, want: evidence.Blocked},
		"failed": {change: func(f *s11DeniedFixture) {
			f.deniedPhases[3] = databasev1alpha1.BackupPhaseCompleted
		}, want: evidence.Fail},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11SignatureConfig(t)
			f := newS11SignatureFixture(t, cfg)
			tc.change(f)
			if got, err := signatureOf(t, f); err == nil || got.Result != tc.want {
				t.Fatalf("S11 signature-mismatch = %+v, want %s", got, tc.want)
			}
			line := "S11/" + s11SignatureVariant + ": " + string(tc.want) + ": "
			summary := readSummary(t, cfg.EvidenceDir)
			if !slices.ContainsFunc(summary.Problems, func(p string) bool {
				return strings.HasPrefix(p, line) && !strings.Contains(p, "[REDACTED]")
			}) {
				t.Fatalf("problems = %q, want a line starting %q without a redaction", summary.Problems, line)
			}
			junit, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, "junit.xml"))
			if err != nil || !strings.Contains(string(junit), line) || strings.Contains(string(junit), "[REDACTED]") {
				t.Fatalf("junit.xml = %s, %v; want %q without a redaction", junit, err, line)
			}
			// The record, its notes and the reasons hold no text the
			// redactor takes for a credential either.
			requireNothingRedacted(t, cfg.EvidenceDir)
		})
	}
}

func TestRunS11SignatureMismatchIsBlockedWhenTheFaultIsNotInPlace(t *testing.T) {
	for name, tc := range map[string]struct {
		refusal  error
		want     string
		wantCode string
	}{
		// A store that does not check secret keys lists the bucket.
		"the store accepts the keys": {want: "accepted the mismatched keys"},
		// A refusal, but not of the signature: the access key is not known.
		"another refusal": {refusal: minio.ErrorResponse{Code: testUnknownKeyCode, Message: testDeniedAnswer},
			want:     `with the S3 error code "` + testUnknownKeyCode + `", not ` + signatureDoesNotMatch,
			wantCode: testUnknownKeyCode},
		"no S3 answer": {refusal: errors.New(testReset), want: testReset},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11SignatureConfig(t)
			f := newS11SignatureFixture(t, cfg)
			f.refusal = tc.refusal
			wrong, err := signatureOf(t, f)
			if err == nil || wrong.Result != evidence.Blocked || !strings.Contains(wrong.Reason, tc.want) {
				t.Fatalf("S11 signature-mismatch = %+v, want blocked by %q", wrong, tc.want)
			}
			if signatureSecret(f) != nil || f.deniedCluster != nil || f.denied != nil || len(f.deleted) != 0 {
				t.Fatal("something was created before the store was known to refuse the wrong secret key")
			}
			rec := readSignatureRecord(t, cfg.EvidenceDir)
			if !strings.Contains(rec.Cleanup, "nothing") || rec.Fault == nil || rec.Fault.Refused ||
				rec.Fault.Code != tc.wantCode {
				t.Fatalf("record = %+v, fault = %+v; want the unconfirmed fault and nothing to delete", rec, rec.Fault)
			}
		})
	}
}

func TestRunS11SignatureMismatchFailsWhenTheBackupCompletes(t *testing.T) {
	cfg := testS11SignatureConfig(t)
	f := newS11SignatureFixture(t, cfg)
	f.deniedPhases[3], f.writeObjects = databasev1alpha1.BackupPhaseCompleted, true
	wrong, err := signatureOf(t, f)
	if err == nil || wrong.Result != evidence.Fail || len(wrong.NeverEvents) < 2 {
		t.Fatalf("S11 signature-mismatch = %+v, want Completed and the artifact as never-events", wrong)
	}
	for _, want := range []string{"the backup of " + s11SignatureClusterName + ", whose object store keys the " +
		"store refused, ended Completed", testArtifactField, "holds 2 objects"} {
		if !strings.Contains(wrong.Reason, want) {
			t.Fatalf("S11 signature-mismatch reason = %q, want %q", wrong.Reason, want)
		}
	}
	if !slices.Equal(f.deleted, testSignatureDeleted) {
		t.Fatalf("deleted = %v, want everything the variant created deleted", f.deleted)
	}
}

func TestRunS11SignatureMismatchFailsOnAnArtifactOfAFailedBackup(t *testing.T) {
	cfg := testS11SignatureConfig(t)
	f := newS11SignatureFixture(t, cfg)
	f.tamperDenied = signatureStatus(func(_ int, s *databasev1alpha1.CubridBackupStatus) {
		if s.Phase == databasev1alpha1.BackupPhaseFailed {
			s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: testElsewhereURI}
		}
	})
	wrong, err := signatureOf(t, f)
	if err == nil || wrong.Result != evidence.Fail || len(wrong.NeverEvents) != 1 ||
		!strings.Contains(wrong.NeverEvents[0], testArtifactField+" names "+testElsewhereURI) {
		t.Fatalf("S11 signature-mismatch = %+v, want the artifact as a never-event", wrong)
	}
}

func TestRunS11SignatureMismatchFailsWhenAnObjectIsLeftUnderThePrefix(t *testing.T) {
	cfg := testS11SignatureConfig(t)
	f := newS11SignatureFixture(t, cfg)
	f.writeObjects = true
	wrong, err := signatureOf(t, f)
	if err == nil || wrong.Result != evidence.Fail || len(wrong.NeverEvents) != 0 ||
		!strings.Contains(wrong.Reason, "holds 1 objects") || !strings.Contains(wrong.Reason, "/signature-mismatch") {
		t.Fatalf("S11 signature-mismatch = %+v, want a failure for the object at the end", wrong)
	}
	if rec := readSignatureRecord(t, cfg.EvidenceDir); len(rec.ObjectsAtEnd) != 1 {
		t.Fatalf("record = %+v, want the object listed", rec)
	}
}

func TestRunS11SignatureMismatchIsBlockedWhenTheUploadAnswerIsNotARefusal(t *testing.T) {
	cfg := testS11SignatureConfig(t)
	f := newS11SignatureFixture(t, cfg)
	f.tamperDenied = signatureStatus(func(_ int, s *databasev1alpha1.CubridBackupStatus) {
		if s.Phase == databasev1alpha1.BackupPhaseFailed {
			s.Conditions[0].Message = "upload failed: put " + testBackupBucket +
				"/x/backup/appdb_bk0v000: The specified bucket does not exist."
		}
	})
	wrong, err := signatureOf(t, f)
	if err == nil || wrong.Result != evidence.Blocked || len(wrong.NeverEvents) != 0 ||
		!strings.Contains(wrong.Reason, "does not show a refusal of the keys") ||
		!strings.Contains(wrong.Reason, "the mismatched keys' effect is not confirmed") {
		t.Fatalf("S11 signature-mismatch = %+v, want blocked: a missing bucket is not a refusal", wrong)
	}
}

// An access refusal for another reason, such as an unknown access key, does
// not show that the mismatched secret key failed the upload.
func TestRunS11SignatureMismatchIsBlockedWhenTheUploadIsRefusedForAnotherReason(t *testing.T) {
	cfg := testS11SignatureConfig(t)
	f := newS11SignatureFixture(t, cfg)
	f.tamperDenied = signatureStatus(func(_ int, s *databasev1alpha1.CubridBackupStatus) {
		if s.Phase == databasev1alpha1.BackupPhaseFailed {
			s.Conditions[0].Message = "upload failed: put " + testBackupBucket +
				"/x/backup/appdb_bk0v000: The access key ID you provided does not exist in our records."
		}
	})
	wrong, err := signatureOf(t, f)
	if err == nil || wrong.Result != evidence.Blocked || len(wrong.NeverEvents) != 0 ||
		!strings.Contains(wrong.Reason, "was refused, but not with \""+signatureAnswer+"\"") {
		t.Fatalf("S11 signature-mismatch = %+v, want blocked: an unknown key is not a signature mismatch", wrong)
	}
}

func TestRunS11SignatureMismatchFailsOnAKeyInAnEventOrAStatus(t *testing.T) {
	accessKey := func(*s11DeniedFixture) string { return testAccessKey }
	wrongKey := func(f *s11DeniedFixture) string { return f.keys[1] }
	const leak = "s11-signature.leak"
	for name, tc := range map[string]struct {
		key     func(f *s11DeniedFixture) string
		inEvent bool
		where   string
	}{
		// The access key is the seed Secret's and the variant's: the place is
		// named once.
		"the access key in an Event":       {key: accessKey, inEvent: true, where: "Event " + leak},
		"the wrong secret key in an Event": {key: wrongKey, inEvent: true, where: "Event " + leak},
		"the wrong secret key in the backup's status": {key: wrongKey,
			where: "the status read in phase " + string(databasev1alpha1.BackupPhaseRunning)},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11SignatureConfig(t)
			f := newS11SignatureFixture(t, cfg)
			if !tc.inEvent {
				f.tamperDenied = signatureStatus(func(_ int, s *databasev1alpha1.CubridBackupStatus) {
					if s.Phase == databasev1alpha1.BackupPhaseRunning {
						s.Conditions[0].Message += " (key " + tc.key(f) + ")"
					}
				})
			}
			f.onDeniedRead = func(read int) error {
				if read != 1 || !tc.inEvent {
					return nil
				}
				event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: leak, Namespace: cfg.Namespace},
					InvolvedObject: corev1.ObjectReference{Kind: testKindBackup, Name: s11SignatureBackupName,
						UID: testDeniedUID},
					Reason: s11MissingReason, Message: "upload refused for key " + tc.key(f)}
				if err := f.haFixture.store.Create(t.Context(), event); err != nil {
					t.Fatal(err)
				}
				return nil
			}
			wrong, err := signatureOf(t, f)
			if err == nil || wrong.Result != evidence.Fail || len(wrong.NeverEvents) != 1 ||
				!strings.Contains(wrong.NeverEvents[0], testCredential) ||
				strings.Count(wrong.NeverEvents[0], tc.where) != 1 {
				t.Fatalf("S11 signature-mismatch = %+v, want the key in %s as one never-event, named once", wrong, tc.where)
			}
		})
	}
}

func TestRunS11SignatureMismatchIsBlockedWhenS11CompleteDidNotPass(t *testing.T) {
	cfg := testS11SignatureConfig(t)
	f := newS11SignatureFixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if wrong := signatureScenarios(t, cfg.EvidenceDir)[2]; wrong.Result != evidence.Blocked ||
		!strings.Contains(wrong.Reason, "S11 complete did not pass") {
		t.Fatalf("S11 signature-mismatch = %+v, want blocked by S11 complete", wrong)
	}
	if f.deniedCluster != nil || len(f.keys) != 0 {
		t.Fatal("S11 signature-mismatch ran although S11 complete did not pass")
	}
}

func TestRunHARefusesBackupSignatureMismatchWithoutItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(cfg *HAConfig)
		want   string
	}{
		testNoBucket: {change: func(cfg *HAConfig) { cfg.BackupBucket = "" },
			want: "-backup-signature-mismatch needs -backup-bucket"},
		testWithRestore: {change: func(cfg *HAConfig) {
			cfg.Restore, cfg.RestoreLimit = true, time.Minute
		}, want: testRestoreFlag},
		testWithMissing: {change: func(cfg *HAConfig) {
			cfg.BackupMissingDestination = true
		}, want: "and -backup-missing-destination select"},
		testWithDenied: {change: func(cfg *HAConfig) {
			cfg.BackupDeniedCredentials, cfg.BackupDeniedReadyLimit = true, time.Minute
		}, want: "and -backup-denied-credentials select"},
		testWithInterrupted: {change: func(cfg *HAConfig) {
			cfg.BackupInterruptedUpload, cfg.BackupInterruptedReadyLimit = true, time.Minute
		}, want: "and -backup-interrupted-upload select"},
		"without its ready limit": {change: func(cfg *HAConfig) { cfg.BackupSignatureMismatchReadyLimit = 0 },
			want: "-backup-signature-mismatch-ready-limit"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11SignatureConfig(t)
			tc.change(&cfg)
			f := newS11SignatureFixture(t, testS11SignatureConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "-backup-signature-mismatch") {
				t.Fatalf("RunHA() error = %v, want -backup-signature-mismatch refused %s", err, name)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}
