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
	"fmt"
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
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testTamperedUID    = "tampered-cluster"
	testTamperedMember = "ha-tampered-0"
	testTamperedSecret = "object-storage-tampered"
	// testOtherBackupKey is the artifact's other object.
	testOtherBackupKey = "backup/appdb_bkvinf"
)

func testS12TamperedConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS12Config(t)
	cfg.RestoreTampered = true
	return cfg
}

// s12TamperedFixture plays, after S12 exact, an Operator that restores the
// tampered copy into a new cluster. Like the Instance Manager, it reads the
// manifest the cluster names and every object it lists from the store, and
// refuses the restore for the first object that does not match: twice it
// starts another attempt, then the bootstrap ends Failed. A copy that
// matches its manifest is restored, and the cluster Ready.
type s12TamperedFixture struct {
	*s12Fixture
	// neverRefuse keeps the restore at its first attempt; tamperTampered
	// changes the status of a reading; onTamperedRead runs first, and its
	// error is the reading's.
	neverRefuse    bool
	tamperTampered func(read int, s *databasev1alpha1.CubridClusterStatus)
	onTamperedRead func(read int) error
	// afterTamperedCreated runs once the cluster and its children exist.
	afterTamperedCreated func(ctx context.Context, c client.WithWatch)

	tampered      *databasev1alpha1.CubridCluster
	tamperedReads int
	// mismatched are the objects of the copy that did not match its
	// manifest, as the fake Operator found them.
	mismatched []string
}

func newS12TamperedFixture(t *testing.T, cfg HAConfig) *s12TamperedFixture {
	t.Helper()
	f := &s12TamperedFixture{s12Fixture: newS12Fixture(t, cfg)}
	f.tamperedCreated = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
		cluster.UID, cluster.Generation = testTamperedUID, 1
		f.mu.Lock()
		f.tampered = cluster.DeepCopy()
		f.mu.Unlock()
		if err := c.Create(ctx, cluster); err != nil {
			return err
		}
		if err := f.createChildren(ctx, c, cluster); err != nil {
			return err
		}
		if f.afterTamperedCreated != nil {
			f.afterTamperedCreated(ctx, c)
		}
		return nil
	}
	f.tamperedRead = f.advanceTampered
	return f
}

// verifyCopy reads the manifest a manifestUri names and every object it
// lists, and returns the keys of those that do not match it.
func (f *s12TamperedFixture) verifyCopy(ctx context.Context, uri string) []string {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(uri, "s3://"), "/")
	data, err := download(ctx, f.store, bucket, key)
	if err != nil {
		f.t.Errorf("read the manifest %s: %v", uri, err)
		return nil
	}
	m, err := instancemanager.ParseManifest(data)
	if err != nil {
		f.t.Errorf("parse the manifest %s: %v", uri, err)
		return nil
	}
	mismatched := []string{}
	for _, obj := range m.Objects {
		sum, size, err := downloadSum(ctx, f.store, bucket, path.Join(path.Dir(key), obj.Key))
		if err != nil || size != obj.SizeBytes || sum != obj.SHA256 {
			mismatched = append(mismatched, obj.Key)
		}
	}
	return mismatched
}

// advanceTampered writes the status of the next reading, as the Operator
// would.
func (f *s12TamperedFixture) advanceTampered(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, key, cluster); err != nil {
		return nil
	}
	f.mu.Lock()
	f.tamperedReads++
	read := f.tamperedReads
	onRead, tamper := f.onTamperedRead, f.tamperTampered
	f.mu.Unlock()
	if onRead != nil {
		if err := onRead(read); err != nil {
			return err
		}
	}
	uri := cluster.Spec.Bootstrap.Recovery.ManifestURI
	if read == 1 {
		f.mismatched = f.verifyCopy(ctx, uri)
	}
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	s := &cluster.Status
	s.ObservedGeneration = 1
	s.Bootstrap = &databasev1alpha1.BootstrapStatus{Mode: "Recovery", Phase: databasev1alpha1.BootstrapPreparing,
		ManifestURI: uri, TargetMember: testTamperedMember, OperationID: "restore-op-1"}
	boot := metav1.Condition{Type: bootstrapReadyCondition, Status: metav1.ConditionFalse, Reason: "RestorePreparing",
		Message: "the backup is being verified and downloaded on " + testTamperedMember, LastTransitionTime: now}
	ready := metav1.ConditionFalse
	switch attempt := min(read, 4) - 1; {
	case f.neverRefuse || read == 1:
	case len(f.mismatched) == 0:
		s.Bootstrap.Phase, boot.Status, boot.Reason, boot.Message = databasev1alpha1.BootstrapComplete,
			metav1.ConditionTrue, "RecoveryComplete", "restore completed on "+testTamperedMember
		ready = metav1.ConditionTrue
	case attempt < 3:
		why := testRefusalCause(f.mismatched[0])
		s.Bootstrap.Attempts, s.Bootstrap.OperationID = int32(attempt), ""
		boot.Reason, boot.Message = "RestoreFailedRetrying", "on "+testTamperedMember+": "+why+
			fmt.Sprintf("; starting attempt %d of 3", attempt+1)
	default:
		why := testRefusalCause(f.mismatched[0])
		s.Bootstrap.Phase, s.Bootstrap.Attempts, s.Bootstrap.OperationID = databasev1alpha1.BootstrapFailed, 2, ""
		boot.Reason, boot.Message = s12RefusedReason, "on "+testTamperedMember+": "+why+"; giving up after 3 attempts"
	}
	s.Conditions = []metav1.Condition{boot}
	for _, condition := range startingConditions {
		s.Conditions = append(s.Conditions, metav1.Condition{Type: condition, Status: ready, Reason: testConditionReason,
			LastTransitionTime: now})
	}
	if tamper != nil {
		tamper(read, s)
	}
	if err := c.Update(ctx, cluster); err != nil {
		f.t.Error(err)
	}
	return nil
}

// tamperedOf runs the fixture and returns S12 tampered, requiring LAB01, S11
// and S12 exact passed.
func tamperedOf(t *testing.T, f *s12TamperedFixture, ctx context.Context) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(ctx, f.cfg)
	scenarios := tamperedScenarios(t, f.cfg.EvidenceDir)
	for _, s := range scenarios[:3] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01, S11 and S12 exact passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, scenarios[2], "S12/exact")
	requireOwnEvidence(t, scenarios[3], "S12/tampered")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	return scenarios[3], err
}

func tamperedScenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s12TamperedLane || len(summary.Scenarios) != 4 || summary.Scenarios[3].ID != s12ID ||
		summary.Scenarios[3].Variant != s12TamperedVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete, S12/exact and S12/tampered in lane %s", summary,
			s12TamperedLane)
	}
	return summary.Scenarios
}

func readTamperedRecord(t *testing.T, dir string) s12TamperedRecord {
	t.Helper()
	var rec s12TamperedRecord
	data, err := os.ReadFile(filepath.Join(dir, s12TamperedRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S12 tampered record = %s, %v", data, err)
	}
	return rec
}

// copyKeys returns the keys left under the copy's prefix.
func (f *s12TamperedFixture) copyKeys(t *testing.T) []string {
	t.Helper()
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	keys, err := f.store.List(t.Context(), testBackupBucket, "lab-runs/"+lab01.RunID+"/tampered/")
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestRunS12TamperedPassesWhenTheCopyIsRefused(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	f := newS12TamperedFixture(t, cfg)
	tampered, err := tamperedOf(t, f, t.Context())
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if summary := readSummary(t, cfg.EvidenceDir); !summary.Passed || tampered.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want every scenario passed", summary)
	}
	if strings.Join(f.writes, ",") !=
		"namespace,secret,cluster,client,backup,secret,restore-cluster,secret,tampered-cluster" {
		t.Fatalf("writes = %v", f.writes)
	}
	for _, rel := range []string{s12TamperedRecordFile, s12TamperedSourceDataCheckFile} {
		if !slices.Contains(tampered.Evidence, rel) {
			t.Fatalf("S12 tampered evidence = %v, want %s", tampered.Evidence, rel)
		}
	}
	if tampered.Limits["restore_limit"] != cfg.RestoreLimit.String() ||
		tampered.Measurements["refusedWithin"] == evidence.Unknown || tampered.Measurements["attempts"] != float64(2) {
		t.Fatalf("S12 tampered limits = %v, measurements = %v", tampered.Limits, tampered.Measurements)
	}
	// The fake Operator found exactly the changed object not matching.
	if !slices.Equal(f.mismatched, []string{testFirstBackupKey}) {
		t.Fatalf("objects not matching in the copy = %v, want %s only", f.mismatched, testFirstBackupKey)
	}
	requireTamperedSpec(t, f)
	requireTamperedRecord(t, f)
	if keys := f.copyKeys(t); len(keys) != 0 {
		t.Fatalf("copy objects left = %v, want the copy removed", keys)
	}
	// S11's artifact is as it was uploaded.
	s11 := readS11Record(t, cfg.EvidenceDir)
	key := strings.TrimPrefix(s11.Artifact.URI, "s3://"+testBackupBucket+"/")
	if mismatched := f.verifyCopy(t.Context(), s11.Artifact.URI); len(mismatched) != 0 ||
		sha256Of(f.store.objects[testBackupBucket+"/"+key]) != s11.Artifact.ManifestDigest {
		t.Fatalf("S11's artifact changed: %v", mismatched)
	}
	f.requireForwardStopped(t)
	requireNamespaceGone(t, f.haFixture)
}

// requireTamperedSpec checks the cluster the run created from the copy.
func requireTamperedSpec(t *testing.T, f *s12TamperedFixture) {
	t.Helper()
	lab01, rec := readHARecord(t, f.cfg.EvidenceDir), readTamperedRecord(t, f.cfg.EvidenceDir)
	got := f.tampered
	if got == nil || got.Name != s12TamperedClusterName || got.Namespace != f.cfg.Namespace {
		t.Fatalf("cluster = %+v, want %s in the run namespace", got, s12TamperedClusterName)
	}
	want := "s3://" + testBackupBucket + "/lab-runs/" + lab01.RunID + "/tampered/manifest.json"
	if got.Spec.Bootstrap == nil || got.Spec.Bootstrap.Recovery == nil ||
		got.Spec.Bootstrap.Recovery.ManifestURI != want || rec.Copy == nil || rec.Copy.URI != want {
		t.Fatalf("bootstrap = %+v, copy %+v; want the copy %s", got.Spec.Bootstrap, rec.Copy, want)
	}
	storage := got.Spec.ObjectStorage
	if storage == nil || storage.Prefix != "lab-runs/"+lab01.RunID+"/tampered-seed" ||
		storage.CredentialsSecretRef.Name != testTamperedSecret {
		t.Fatalf("objectStorage = %+v, want a Secret and a prefix of its own", storage)
	}
	if len(f.secrets) != 3 || f.secrets[2].Name != testTamperedSecret ||
		string(f.secrets[2].Data[seedAccessKey]) != testAccessKey ||
		string(f.secrets[2].Data[seedSecretKey]) != testSecretKey {
		t.Fatalf("Secrets = %v, want the seed Secret copied again for the cluster", f.secrets)
	}
}

// requireTamperedCopy checks the copy of a passed run.
func requireTamperedCopy(t *testing.T, f *s12TamperedFixture) {
	t.Helper()
	rec, s11 := readTamperedRecord(t, f.cfg.EvidenceDir), readS11Record(t, f.cfg.EvidenceDir)
	c := rec.Copy
	original := f.store.objects[testBackupBucket+"/"+path.Dir(strings.TrimPrefix(s11.Artifact.URI,
		"s3://"+testBackupBucket+"/"))+"/"+testFirstBackupKey]
	changed := slices.Clone(original)
	changed[500] ^= 0xff
	if c.SourceURI != s11.Artifact.URI || c.SourceManifestDigest != s11.Artifact.ManifestDigest || c.Objects != 2 ||
		c.ObjectsCopied != 2 || c.TamperedKey != testFirstBackupKey || c.Offset != 500 || c.ByteBefore != "0x76" ||
		c.ByteAfter != "0x89" || c.SHA256Before != sha256Of(original) || c.SHA256After != sha256Of(changed) {
		t.Fatalf("copy = %+v, want one byte of %s changed", c, testFirstBackupKey)
	}
	if !c.Checked || c.ObjectsMatching != 1 || !slices.Equal(c.ObjectsNotMatching, []string{testFirstBackupKey}) ||
		!c.ManifestUnchanged || !c.SourceUnchanged || !c.Removed {
		t.Fatalf("copy = %+v, want it checked, S11's artifact unchanged and the copy removed", c)
	}
}

// requireTamperedRecord checks the record of a passed run.
func requireTamperedRecord(t *testing.T, f *s12TamperedFixture) {
	t.Helper()
	requireTamperedCopy(t, f)
	rec := readTamperedRecord(t, f.cfg.EvidenceDir)
	attempts := make([]int32, 0, len(rec.Bootstrap))
	for _, p := range rec.Bootstrap {
		attempts = append(attempts, p.Attempts)
	}
	if n := len(rec.Bootstrap); n != 4 || rec.Bootstrap[n-1].Phase != "Failed" || !slices.Equal(attempts,
		[]int32{0, 1, 2, 2}) || rec.Bootstrap[0].TargetMember != testTamperedMember {
		t.Fatalf("bootstrap = %+v, want each attempt and the Failed phase", rec.Bootstrap)
	}
	if rec.ClusterUID != testTamperedUID || rec.RefusedWithin == "" || !strings.HasPrefix(rec.Refusal,
		s12RefusedReason+": on "+testTamperedMember+": "+testRefusalCause(testFirstBackupKey)) {
		t.Fatalf("record = %+v, want the cluster and the refusal", rec)
	}
	if !slices.Contains(rec.SecretsReferenced, testTamperedSecret) || slices.Contains(rec.SecretsReferenced,
		seedSecretName) {
		t.Fatalf("Secrets referenced = %v, want the cluster's own", rec.SecretsReferenced)
	}
	if rec.Source == nil || rec.Source.Restarted == nil || *rec.Source.Restarted != 0 ||
		rec.Source.Master != testHAPrimary || !rec.Source.DataRulesHeld {
		t.Fatalf("source = %+v, want it unchanged", rec.Source)
	}
	if rec.Credentials == nil || rec.Credentials.StatusesChecked < 5 || len(rec.Credentials.FoundIn) != 0 {
		t.Fatalf("credentials = %+v, want every status searched", rec.Credentials)
	}
}

func TestRunS12TamperedFailsWhenTheClusterIsReady(t *testing.T) {
	for name, tc := range map[string]struct {
		tamper func(read int, s *databasev1alpha1.CubridClusterStatus)
		want   string
	}{
		"Ready while refused": {tamper: func(read int, s *databasev1alpha1.CubridClusterStatus) {
			if read == 2 {
				for i := range s.Conditions {
					if s.Conditions[i].Type == startingConditions[0] {
						s.Conditions[i].Status = metav1.ConditionTrue
					}
				}
			}
		}, want: startingConditions[0]},
		"Complete": {tamper: func(read int, s *databasev1alpha1.CubridClusterStatus) {
			if read == 2 {
				s.Bootstrap.Phase = databasev1alpha1.BootstrapComplete
			}
		}, want: string(databasev1alpha1.BootstrapComplete)},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12TamperedConfig(t)
			f := newS12TamperedFixture(t, cfg)
			f.tamperTampered = tc.tamper
			tampered, err := tamperedOf(t, f, t.Context())
			if err == nil || tampered.Result != evidence.Fail || len(tampered.NeverEvents) != 1 ||
				!strings.Contains(tampered.NeverEvents[0], tc.want) || !strings.Contains(tampered.Reason, tc.want) {
				t.Fatalf("S12 tampered = %+v, want %s as a never-event", tampered, tc.want)
			}
			if keys := f.copyKeys(t); len(keys) != 0 {
				t.Fatalf("copy objects left = %v, want the copy removed", keys)
			}
		})
	}
}

func TestRunS12TamperedFailsWhenTheRestoreIsRefusedForAnotherReason(t *testing.T) {
	for name, tc := range map[string]struct{ reason, message string }{
		"another object": {reason: s12RefusedReason,
			message: "on ha-tampered-0: object " + testOtherBackupKey + " checksum mismatch; giving up after 3 attempts"},
		"no checksum": {reason: s12RefusedReason,
			message: "on ha-tampered-0: restoredb failed: exit status 1; giving up after 3 attempts"},
		"another reason": {reason: "InvalidManifestURI",
			message: "object " + testFirstBackupKey + " checksum mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12TamperedConfig(t)
			f := newS12TamperedFixture(t, cfg)
			f.tamperTampered = func(_ int, s *databasev1alpha1.CubridClusterStatus) {
				if s.Bootstrap.Phase == databasev1alpha1.BootstrapFailed {
					s.Conditions[0].Reason, s.Conditions[0].Message = tc.reason, tc.message
				}
			}
			tampered, err := tamperedOf(t, f, t.Context())
			if err == nil || tampered.Result != evidence.Fail || !strings.Contains(tampered.Reason, tc.message) ||
				len(tampered.NeverEvents) != 0 {
				t.Fatalf("S12 tampered = %+v, want a failure naming the other reason", tampered)
			}
		})
	}
}

func TestRunS12TamperedFailsWhenNotRefusedWithinTheRestoreLimit(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	cfg.RestoreLimit = 300 * time.Millisecond
	f := newS12TamperedFixture(t, cfg)
	f.neverRefuse = true
	tampered, err := tamperedOf(t, f, t.Context())
	if err == nil || tampered.Result != evidence.Fail || !strings.Contains(tampered.Reason, "restore_limit") ||
		!strings.Contains(tampered.Reason, "Preparing") {
		t.Fatalf("S12 tampered = %+v, want the restore limit exceeded with the last phase", tampered)
	}
	if tampered.Measurements["refusedWithin"] != evidence.Unknown {
		t.Fatalf("S12 tampered measurements = %v, want refusedWithin unknown", tampered.Measurements)
	}
}

func TestRunS12TamperedIsBlockedWhenTheClusterCannotBeRead(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	cfg.RestoreLimit = 300 * time.Millisecond
	f := newS12TamperedFixture(t, cfg)
	f.onTamperedRead = func(int) error { return apierrors.NewServiceUnavailable("etcd is unavailable") }
	tampered, err := tamperedOf(t, f, t.Context())
	if err == nil || tampered.Result != evidence.Blocked || !strings.Contains(tampered.Reason, "etcd is unavailable") {
		t.Fatalf("S12 tampered = %+v, want blocked: the cluster was never read", tampered)
	}
}

func TestRunS12TamperedIsBlockedWhenTheCopyCannotBePrepared(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *s12TamperedFixture)
		want   string
	}{
		"a write refused": {change: func(f *s12TamperedFixture) {
			f.store.putErr = func(key string) error {
				if path.Base(key) == instancemanager.ManifestObjectName {
					return errors.New("access denied")
				}
				return nil
			}
		}, want: "access denied"},
		"S11's artifact changed": {change: func(f *s12TamperedFixture) {
			f.afterRestoreCreated = func(context.Context, client.WithWatch) {
				s11 := f.artifact
				key := path.Dir(strings.TrimPrefix(s11.URI, "s3://"+testBackupBucket+"/")) + "/" + testOtherBackupKey
				f.store.put(testBackupBucket, key, []byte("volume informatioN"))
			}
		}, want: testOtherBackupKey},
		"the copy not as written": {change: func(f *s12TamperedFixture) {
			f.store.afterPut = func(key string) {
				if strings.HasSuffix(key, testOtherBackupKey) && strings.Contains(key, "/tampered/") {
					// The store keeps something else than it was sent.
					f.store.put(testBackupBucket, key, []byte("volume informatioN"))
				}
			}
		}, want: testOtherBackupKey},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12TamperedConfig(t)
			f := newS12TamperedFixture(t, cfg)
			tc.change(f)
			tampered, err := tamperedOf(t, f, t.Context())
			if err == nil || tampered.Result != evidence.Blocked || !strings.Contains(tampered.Reason, tc.want) {
				t.Fatalf("S12 tampered = %+v, want blocked by %q", tampered, tc.want)
			}
			if f.tampered != nil {
				t.Fatal("a cluster was created from a copy that was not prepared")
			}
			if keys := f.copyKeys(t); len(keys) != 0 {
				t.Fatalf("copy objects left = %v, want what was written removed", keys)
			}
			if rec := readTamperedRecord(t, cfg.EvidenceDir); rec.Copy == nil || !rec.Copy.Removed {
				t.Fatalf("copy = %+v, want its removal recorded", rec.Copy)
			}
		})
	}
}

func TestRunS12TamperedIsBlockedWhenS12ExactDidNotPass(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	f := newS12TamperedFixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	scenarios := tamperedScenarios(t, cfg.EvidenceDir)
	if exact := scenarios[2]; exact.Result != evidence.Fail {
		t.Fatalf("S12 exact = %+v, want failed", exact)
	}
	if tampered := scenarios[3]; tampered.Result != evidence.Blocked ||
		!strings.Contains(tampered.Reason, "S12 exact did not pass") {
		t.Fatalf("S12 tampered = %+v, want blocked by S12 exact", tampered)
	}
	if f.tampered != nil || len(f.copyKeys(t)) != 0 {
		t.Fatal("S12 tampered ran although S12 exact did not pass")
	}
}

func TestRunS12TamperedFailsWhenTheSourceChanged(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	f := newS12TamperedFixture(t, cfg)
	f.afterTamperedCreated = func(ctx context.Context, c client.WithWatch) {
		pod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: testMemberLast}, pod); err != nil {
			f.t.Fatal(err)
		}
		pod.Status.ContainerStatuses[0].ContainerID = testRestarted
		if err := c.Status().Update(ctx, pod); err != nil {
			f.t.Fatal(err)
		}
	}
	tampered, err := tamperedOf(t, f, t.Context())
	if err == nil || tampered.Result != evidence.Fail || !strings.Contains(tampered.Reason, "source") ||
		!strings.Contains(tampered.Reason, "restarted") {
		t.Fatalf("S12 tampered = %+v, want the changed source", tampered)
	}
}

func TestRunS12TamperedFailsOnACredentialInTheStatus(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	f := newS12TamperedFixture(t, cfg)
	f.tamperTampered = func(read int, s *databasev1alpha1.CubridClusterStatus) {
		if read == 1 {
			s.Conditions[0].Message = "downloading with " + testSecretKey
		}
	}
	tampered, err := tamperedOf(t, f, t.Context())
	if err == nil || tampered.Result != evidence.Fail || len(tampered.NeverEvents) == 0 ||
		!strings.Contains(tampered.NeverEvents[0], testCredential) {
		t.Fatalf("S12 tampered = %+v, want the credential in the status as a never-event", tampered)
	}
}

func TestRunS12TamperedRecordsACopyItCouldNotRemove(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	f := newS12TamperedFixture(t, cfg)
	f.store.removeErr = func(string) error { return errors.New("access denied") }
	tampered, err := tamperedOf(t, f, t.Context())
	if err != nil || tampered.Result != evidence.Pass {
		t.Fatalf("S12 tampered = %+v, error %v; want a pass: removing the copy is not part of the verdict",
			tampered, err)
	}
	rec := readTamperedRecord(t, cfg.EvidenceDir)
	if rec.Copy == nil || rec.Copy.Removed || !strings.Contains(rec.Residue, "access denied") ||
		!strings.Contains(rec.Residue, "/tampered/") {
		t.Fatalf("record = %+v, want the copy left named in the residue", rec)
	}
}

func TestRunHARefusesRestoreTamperedWithoutRestore(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	cfg.Restore = false
	f := newS12TamperedFixture(t, testS12TamperedConfig(t))
	if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "-restore") {
		t.Fatalf("RunHA() error = %v, want -restore-tampered refused without -restore", err)
	}
	if len(f.writes) != 0 || len(f.ran) != 0 {
		t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
	}
}

func TestRunS12WithoutRestoreTamperedKeepsItsLane(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12TamperedFixture(t, cfg)
	if _, err := s12Of(t, f.s12Fixture, t.Context()); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if f.tampered != nil {
		t.Fatal("S12 tampered ran without -restore-tampered")
	}
}

// testRefusalCause is the failure reason of a restore whose object key does
// not match its checksum, as the Instance Manager words it (restore.go,
// server.go) and the Operator passes it on.
func testRefusalCause(key string) string {
	return "restore failed: artifact verification failed: object " + key + " checksum mismatch"
}

func TestRunS12TamperedFailsWhenS11sArtifactChangesDuringTheRestore(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	f := newS12TamperedFixture(t, cfg)
	f.afterTamperedCreated = func(context.Context, client.WithWatch) {
		s11 := readS11Record(t, cfg.EvidenceDir)
		key := strings.TrimPrefix(s11.Artifact.URI, "s3://"+testBackupBucket+"/")
		f.store.put(testBackupBucket, key, append(slices.Clone(f.store.objects[testBackupBucket+"/"+key]), ' '))
	}
	tampered, err := tamperedOf(t, f, t.Context())
	if err == nil || tampered.Result != evidence.Fail || !strings.Contains(tampered.Reason, "S11's manifest") {
		t.Fatalf("S12 tampered = %+v, want a fail: S11's artifact changed after the cluster was created", tampered)
	}
}

func TestRunS12TamperedRemovesTheCopysManifestFirst(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	f := newS12TamperedFixture(t, cfg)
	var removed []string
	f.store.removeErr = func(key string) error {
		removed = append(removed, key)
		return nil
	}
	if tampered, err := tamperedOf(t, f, t.Context()); err != nil || tampered.Result != evidence.Pass {
		t.Fatalf("S12 tampered = %+v, error %v; want a pass", tampered, err)
	}
	if len(removed) < 2 || path.Base(removed[0]) != instancemanager.ManifestObjectName {
		t.Fatalf("removed = %v, want the copy's manifest removed before its objects", removed)
	}
}
