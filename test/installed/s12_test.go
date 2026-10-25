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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	testRestoreUID    = "restore-cluster"
	testRestoreMaster = "ha-restore-0"
	testRestoreSlave  = "ha-restore-2"
	testThirdOpID     = "c1-000003"
	testRestoreSecret = "object-storage-restore"
	testRestarted     = "containerd://restarted"
)

func testS12Config(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.Timeout = 30 * time.Second
	cfg.Restore = true
	cfg.RestoreLimit = 5 * time.Second
	return cfg
}

// s12Fixture plays, after S11, an Operator that restores the artifact into
// a new cluster, CUBRID on its members and the client writing through either
// cluster.
type s12Fixture struct {
	*s11Fixture
	// bootPhases are the bootstrap phases the fake Operator reports, one per
	// reading of the restored cluster; the last is kept. tamperRestore
	// changes the status of a reading; onRestoreRead runs first, and its
	// error is the reading's.
	bootPhases    []databasev1alpha1.BootstrapPhase
	tamperRestore func(read int, s *databasev1alpha1.CubridClusterStatus)
	onRestoreRead func(read int) error
	// afterRestoreCreated runs once the restored cluster and its children
	// exist.
	afterRestoreCreated func(ctx context.Context, c client.WithWatch)
	// leakPostBackup puts a row written after the backup on the restored
	// cluster; dropMarkerOn and dropNormalOn are a restored member without
	// S11's marker and without the marker written through the restored
	// cluster; normalWriteAnswer answers that write.
	leakPostBackup             bool
	dropMarkerOn, dropNormalOn string
	normalWriteAnswer          func() (string, error)
	// wrongMarkerOn holds S11's marker with another value; postBackupHistory
	// changes what the writes after the backup recorded.
	wrongMarkerOn     string
	postBackupHistory func(string) string
	// misroute puts the marker written through the restored cluster on the
	// source's members too.
	misroute bool

	restored       *databasev1alpha1.CubridCluster
	restoreReads   int
	postBackupRuns []string
	normalWrites   []string
}

func newS12Fixture(t *testing.T, cfg HAConfig) *s12Fixture {
	t.Helper()
	f := &s12Fixture{s11Fixture: newS11Fixture(t, cfg),
		bootPhases: []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing,
			databasev1alpha1.BootstrapRestoring, databasev1alpha1.BootstrapSeedingReplicas,
			databasev1alpha1.BootstrapComplete}}
	for _, member := range clusterMembers(s12ClusterName) {
		f.roles[member] = roleSlave
	}
	f.roles[testRestoreMaster] = roleMaster
	f.restoreCreated = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
		cluster.UID, cluster.Generation = testRestoreUID, 1
		f.mu.Lock()
		f.restored = cluster.DeepCopy()
		f.mu.Unlock()
		if err := c.Create(ctx, cluster); err != nil {
			return err
		}
		if err := f.createChildren(ctx, c, cluster); err != nil {
			return err
		}
		if f.afterRestoreCreated != nil {
			f.afterRestoreCreated(ctx, c)
		}
		return nil
	}
	f.restoreRead = f.advanceRestore
	f.clientHook = f.clientS12
	f.markersOf = f.markerRows
	return f
}

// advanceRestore writes the status of the next bootstrap phase, as the
// Operator would.
func (f *s12Fixture) advanceRestore(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, key, cluster); err != nil {
		return nil
	}
	f.mu.Lock()
	f.restoreReads++
	read := f.restoreReads
	phase := f.bootPhases[min(read, len(f.bootPhases))-1]
	onRead, tamper := f.onRestoreRead, f.tamperRestore
	f.mu.Unlock()
	if onRead != nil {
		if err := onRead(read); err != nil {
			return err
		}
	}
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	complete := phase == databasev1alpha1.BootstrapComplete
	status := metav1.ConditionFalse
	if complete {
		status = metav1.ConditionTrue
	}
	s := &cluster.Status
	s.ObservedGeneration = 1
	s.Bootstrap = &databasev1alpha1.BootstrapStatus{Mode: "Recovery", Phase: phase,
		ManifestURI: cluster.Spec.Bootstrap.Recovery.ManifestURI, TargetMember: testRestoreMaster,
		OperationID: "restore-op"}
	boot := metav1.Condition{Type: bootstrapReadyCondition, Status: status, Reason: "RestoreInProgress",
		Message: "restoredb is running on " + testRestoreMaster, LastTransitionTime: now}
	switch phase {
	case databasev1alpha1.BootstrapComplete:
		boot.Reason, boot.Message = "RecoveryComplete", "restore completed"
		s.CurrentPrimary = testRestoreMaster
		s.Databases = []databasev1alpha1.DatabaseStatus{{Name: databaseName, Phase: "Created", PrimaryCreated: true,
			HAConfigured: true}}
	case databasev1alpha1.BootstrapFailed:
		boot.Reason, boot.Message = s12RefusedReason, "on ha-restore-0: restoredb failed: exit status 1"
	}
	s.Conditions = []metav1.Condition{boot}
	for _, condition := range startingConditions {
		s.Conditions = append(s.Conditions, metav1.Condition{Type: condition, Status: status, Reason: testConditionReason,
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

// clientS12 answers the writes after the backup through the source and the
// marker written through the restored cluster.
func (f *s12Fixture) clientS12(script string) (string, error, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(script, "CLIENT_ID="+s12PostBackupClient):
		f.postBackupRuns = append(f.postBackupRuns, script)
		// The restored cluster holds what the source held at the backup.
		f.restoredRows = slices.Clone(f.rows)
		history, rows := fakeClientWorkload(s12PostBackupClient, s12PostBackupOperations)
		f.rows = append(f.rows, rows...)
		if f.leakPostBackup {
			f.restoredRows = append(f.restoredRows, rows[0])
		}
		if f.postBackupHistory != nil {
			history = f.postBackupHistory(history)
		}
		return history, nil, true
	case strings.Contains(script, "workload marker "+s12Marker+" "):
		f.normalWrites = append(f.normalWrites, script)
		if f.normalWriteAnswer != nil {
			out, err := f.normalWriteAnswer()
			return out, err, true
		}
		return "marker " + s12Marker + " written\n", nil, true
	}
	return "", nil, false
}

// markerRows is the marker table of a restored member: S11's marker, and the
// one written through the restored cluster once it was.
func (f *s12Fixture) markerRows(pod string) []string {
	var rows []string
	switch {
	case pod == f.wrongMarkerOn:
		rows = append(rows, s11Marker+"=another-run")
	case len(f.markers) == 1 && pod != f.dropMarkerOn:
		rows = append(rows, s11Marker+"="+strings.Fields(f.markers[0])[3])
	}
	restored := strings.HasPrefix(pod, s12ClusterName+"-")
	if len(f.normalWrites) > 0 && pod != f.dropNormalOn && (restored || f.misroute) {
		_, rest, _ := strings.Cut(f.normalWrites[0], "workload marker "+s12Marker+" ")
		rows = append(rows, s12Marker+"="+strings.Fields(rest)[0])
	}
	return rows
}

func s12Scenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s12Lane || len(summary.Scenarios) != 3 || summary.Scenarios[2].ID != s12ID {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S12/exact in lane %s", summary, s12Lane)
	}
	return summary.Scenarios
}

// s12Of runs the fixture and returns S12, requiring LAB01 and S11 passed.
func s12Of(t *testing.T, f *s12Fixture, ctx context.Context) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(ctx, f.cfg)
	scenarios := s12Scenarios(t, f.cfg.EvidenceDir)
	if scenarios[0].Result != evidence.Pass || scenarios[1].Result != evidence.Pass {
		t.Fatalf("LAB01 = %+v, S11 = %+v; want both passed", scenarios[0], scenarios[1])
	}
	requireOwnEvidence(t, scenarios[2], "S12/exact")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	return scenarios[2], err
}

func readS12Record(t *testing.T, dir string) s12Record {
	t.Helper()
	var rec s12Record
	data, err := os.ReadFile(filepath.Join(dir, s12RecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S12 record = %s, %v", data, err)
	}
	return rec
}

func readReport(t *testing.T, dir, rel string) workload.Report {
	t.Helper()
	var report workload.Report
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil || json.Unmarshal(data, &report) != nil {
		t.Fatalf("%s = %s, %v", rel, data, err)
	}
	return report
}

func TestRunS12ExactPassesWithTheRestoredData(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	s12, err := s12Of(t, f, t.Context())
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if summary := readSummary(t, cfg.EvidenceDir); !summary.Passed || s12.Result != evidence.Pass ||
		s12.Variant != s12Variant {
		t.Fatalf("summary = %+v, want every scenario passed", summary)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,secret,restore-cluster" {
		t.Fatalf("writes = %v", f.writes)
	}
	for _, rel := range []string{s12RecordFile, s12DataCheckFile, s12SourceDataCheckFile} {
		if !slices.Contains(s12.Evidence, rel) {
			t.Fatalf("S12 evidence = %v, want %s", s12.Evidence, rel)
		}
	}
	if s12.Limits["restore_limit"] != cfg.RestoreLimit.String() ||
		s12.Measurements["restoredWithin"] == evidence.Unknown || s12.Measurements["postBackupRowsPresent"] != float64(0) {
		t.Fatalf("S12 limits = %v, measurements = %v", s12.Limits, s12.Measurements)
	}
	requireRestoredSpec(t, f)
	requireS12Writes(t, f)
	requireS12Record(t, f)
	requireS12DataChecks(t, cfg)
	requireHANodes(t, cfg)
	requireNamespaceGone(t, f.haFixture)
}

func TestRunS12KeepsLAB01sOperationsItsOwn(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	s12, err := s12Of(t, f, t.Context())
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	requireOwnOperations(t, cfg, s12)
}

// requireOwnOperations fails unless LAB01 counts its own client's operations
// and S12 those written after the backup.
func requireOwnOperations(t *testing.T, cfg HAConfig, s12 evidence.Scenario) {
	t.Helper()
	lab := s12Scenarios(t, cfg.EvidenceDir)[0]
	if ops := lab.Operations; ops == nil || ops.Attempted != cfg.Operations+1 || ops.Acknowledged != cfg.Operations {
		t.Fatalf("LAB01 operations = %+v, want LAB01's client only", lab.Operations)
	}
	if n := readHARecord(t, cfg.EvidenceDir).Operations[workload.Attempted]; n != cfg.Operations+1 {
		t.Fatalf("LAB01 record attempted = %d, want LAB01's client only", n)
	}
	if ops := s12.Operations; ops == nil || ops.Attempted != s12PostBackupOperations ||
		ops.Acknowledged != s12PostBackupOperations {
		t.Fatalf("S12 operations = %+v, want the writes after the backup", s12.Operations)
	}
}

// requireHANodes fails unless the record holds each cluster's own node list.
func requireHANodes(t *testing.T, cfg HAConfig) {
	t.Helper()
	var rec struct {
		HANodes map[string][]string `json:"haNodes"`
	}
	data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, s12RecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S12 record = %s, %v", data, err)
	}
	for _, cluster := range []string{haClusterName, s12ClusterName} {
		if !slices.Equal(rec.HANodes[cluster], clusterMembers(cluster)) {
			t.Fatalf("HA nodes = %v, want each cluster's own members", rec.HANodes)
		}
	}
}

// requireRestoredSpec checks the restored cluster the run created.
func requireRestoredSpec(t *testing.T, f *s12Fixture) {
	t.Helper()
	lab01, s11 := readHARecord(t, f.cfg.EvidenceDir), readS11Record(t, f.cfg.EvidenceDir)
	got, source := f.restored, f.cluster
	if got == nil || got.Name != s12ClusterName || got.Namespace != f.cfg.Namespace {
		t.Fatalf("restored cluster = %+v, want %s in the run namespace", got, s12ClusterName)
	}
	if got.Spec.Bootstrap == nil || got.Spec.Bootstrap.Recovery == nil || s11.Artifact == nil ||
		got.Spec.Bootstrap.Recovery.ManifestURI != s11.Artifact.URI {
		t.Fatalf("bootstrap = %+v, want S11's artifact %+v", got.Spec.Bootstrap, s11.Artifact)
	}
	storage := got.Spec.ObjectStorage
	if storage == nil || storage.Endpoint != f.cfg.SeedEndpoint || storage.Insecure != f.cfg.SeedInsecure ||
		storage.Bucket != f.cfg.SeedBucket || storage.Prefix != "lab-runs/"+lab01.RunID+"/restore-seed" ||
		storage.CredentialsSecretRef.Name != testRestoreSecret {
		t.Fatalf("objectStorage = %+v, want the seed store, a Secret and a prefix of its own", storage)
	}
	if len(f.secrets) != 2 || f.secrets[1].Name != testRestoreSecret ||
		string(f.secrets[1].Data[seedAccessKey]) != testAccessKey ||
		string(f.secrets[1].Data[seedSecretKey]) != testSecretKey {
		t.Fatalf("Secrets = %v, want the seed Secret copied again for the restored cluster", f.secrets)
	}
	if got.Spec.Version != source.Spec.Version || *got.Spec.Image != *source.Spec.Image ||
		!slices.Equal(got.Spec.Databases, source.Spec.Databases) || got.Spec.Topology != source.Spec.Topology ||
		got.Spec.HighAvailability != source.Spec.HighAvailability ||
		!got.Spec.Storage.Data.Size.Equal(source.Spec.Storage.Data.Size) ||
		*got.Spec.Storage.Data.StorageClassName != *source.Spec.Storage.Data.StorageClassName {
		t.Fatalf("restored spec = %+v, want the source's version, image, databases, members and storage", got.Spec)
	}
}

// requireS12Writes checks the client writes of a passed run.
func requireS12Writes(t *testing.T, f *s12Fixture) {
	t.Helper()
	if len(f.postBackupRuns) != 1 ||
		!strings.Contains(f.postBackupRuns[0], fmt.Sprintf("OPS=%d", s12PostBackupOperations)) ||
		strings.Contains(f.postBackupRuns[0], s12ClusterName) {
		t.Fatalf("post-backup runs = %v, want one run through the source", f.postBackupRuns)
	}
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	if len(f.normalWrites) != 1 || !strings.Contains(f.normalWrites[0], s12ClusterName+"-rw") ||
		!strings.Contains(f.normalWrites[0], " "+s12Marker+" "+lab01.RunID) {
		t.Fatalf("normal writes = %v, want one marker through the restored read-write Service", f.normalWrites)
	}
	for _, name := range []string{s12ClusterName + "-rw", s12ClusterName + "-ro"} {
		if f.haFixture.reads[name] == 0 {
			t.Fatalf("reads = %v, want the restored Services read", f.haFixture.reads)
		}
	}
	history, err := os.ReadFile(filepath.Join(f.cfg.EvidenceDir, haHistoryFile))
	if err != nil || !strings.Contains(string(history), `"client":"`+s12PostBackupClient+`"`) {
		t.Fatalf("history = %v, want the writes after the backup in it", err)
	}
}

// requireS12Record checks the record of a passed run.
func requireS12Record(t *testing.T, f *s12Fixture) {
	t.Helper()
	rec, s11 := readS12Record(t, f.cfg.EvidenceDir), readS11Record(t, f.cfg.EvidenceDir)
	if rec.KnownPoint == nil || *rec.KnownPoint != *s11.KnownPoint || rec.ArtifactURI != s11.Artifact.URI ||
		rec.ManifestDigest != s11.Artifact.ManifestDigest {
		t.Fatalf("known point = %+v, artifact %s %s; want S11's", rec.KnownPoint, rec.ArtifactURI, rec.ManifestDigest)
	}
	if rec.PostBackup == nil || rec.PostBackup.Acknowledged != s12PostBackupOperations ||
		rec.PostBackup.LastAcknowledged != fmt.Sprintf("%s-%06d", s12PostBackupClient, s12PostBackupOperations) {
		t.Fatalf("post-backup writes = %+v", rec.PostBackup)
	}
	phases := make([]string, 0, len(rec.Bootstrap))
	for _, p := range rec.Bootstrap {
		phases = append(phases, p.Phase)
	}
	if !slices.Equal(phases, []string{"Preparing", "Restoring", "SeedingReplicas", "Complete"}) ||
		rec.Bootstrap[0].TargetMember != testRestoreMaster || rec.Bootstrap[0].OperationID != "restore-op" ||
		rec.Bootstrap[0].ManifestURI != s11.Artifact.URI {
		t.Fatalf("bootstrap = %+v, want every phase with its target, operation and manifest", rec.Bootstrap)
	}
	if rec.ClusterUID != testRestoreUID || rec.RestoredWithin == "" || rec.Roles == nil ||
		rec.Roles.Master != testRestoreMaster || len(rec.Members) != haMemberCount {
		t.Fatalf("record = %+v, want the restored cluster, its roles and when it was Ready", rec)
	}
	if rec.NormalWrite == nil || !rec.NormalWrite.Acknowledged ||
		!slices.Equal(rec.NormalWrite.ReadBackOn, clusterMembers(s12ClusterName)) {
		t.Fatalf("normal write = %+v, want it read back on every member", rec.NormalWrite)
	}
	if rec.Source == nil || rec.Source.Restarted == nil || *rec.Source.Restarted != 0 ||
		rec.Source.Master != testHAPrimary || !rec.Source.DataRulesHeld {
		t.Fatalf("source = %+v, want it unchanged", rec.Source)
	}
	if rec.Credentials == nil || rec.Credentials.StatusesChecked == 0 || len(rec.Credentials.FoundIn) != 0 {
		t.Fatalf("credentials = %+v, want every status searched", rec.Credentials)
	}
}

// requireS12DataChecks checks the data checks of a passed run.
func requireS12DataChecks(t *testing.T, cfg HAConfig) {
	t.Helper()
	restored := readReport(t, cfg.EvidenceDir, s12DataCheckFile)
	if !restored.OK || len(restored.Members) != haMemberCount+2 {
		t.Fatalf("restored data check = %+v, want every restored member and Service", restored)
	}
	for _, name := range append(clusterMembers(s12ClusterName), "rw", "ro") {
		if check, ok := restored.Members[name]; !ok || check.Rows != 8 {
			t.Fatalf("restored data check = %+v, want the rows before the backup on %s", restored.Members, name)
		}
	}
	source := readReport(t, cfg.EvidenceDir, s12SourceDataCheckFile)
	if check := source.Members[testHAPrimary]; !source.OK || check.Rows != 8+16 {
		t.Fatalf("source data check = %+v, want every row the source acknowledged", source)
	}
}

func TestRunS12IsBlockedWhenAnEarlierScenarioDidNotPass(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12Fixture)
		want   string
	}{
		"LAB01": {change: func(f *s12Fixture) { f.initErr = errors.New("tables not created") }, want: "LAB01"},
		"S11": {change: func(f *s12Fixture) {
			f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
		}, want: "S11 did not pass"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12Config(t)
			f := newS12Fixture(t, cfg)
			tc.change(f)
			if err := f.runner.RunHA(t.Context(), cfg); err == nil {
				t.Fatal("RunHA() passed")
			}
			scenarios := s12Scenarios(t, cfg.EvidenceDir)
			if s12 := scenarios[2]; s12.Result != evidence.Blocked || !strings.Contains(s12.Reason, tc.want) {
				t.Fatalf("S12 = %+v, want blocked by %s", s12, tc.want)
			}
			if f.restored != nil || len(f.postBackupRuns) != 0 {
				t.Fatal("S12 ran although an earlier scenario did not pass")
			}
		})
	}
}

func TestRunS12ExactFailsWhenTheBootstrapFailed(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapRestoring,
		databasev1alpha1.BootstrapFailed}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, s12RefusedReason) ||
		!strings.Contains(s12.Reason, "exit status 1") {
		t.Fatalf("S12 = %+v, want a failure with the bootstrap's reason", s12)
	}
	if rec := readS12Record(t, cfg.EvidenceDir); len(rec.Bootstrap) != 2 || rec.Bootstrap[1].Phase != "Failed" {
		t.Fatalf("bootstrap = %+v, want the Failed phase recorded", rec.Bootstrap)
	}
	if s12.Measurements["restoredWithin"] != evidence.Unknown {
		t.Fatalf("S12 measurements = %v, want restoredWithin unknown", s12.Measurements)
	}
}

func TestRunS12ExactFailsOverTheRestoreLimit(t *testing.T) {
	cfg := testS12Config(t)
	cfg.RestoreLimit = 100 * time.Millisecond
	f := newS12Fixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapRestoring}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, "restore_limit") ||
		!strings.Contains(s12.Reason, "Restoring") {
		t.Fatalf("S12 = %+v, want the restore limit exceeded with the last phase", s12)
	}
}

func TestRunS12ExactIsBlockedWhenTheRestoredClusterCannotBeRead(t *testing.T) {
	cfg := testS12Config(t)
	cfg.RestoreLimit = 100 * time.Millisecond
	f := newS12Fixture(t, cfg)
	f.onRestoreRead = func(int) error { return apierrors.NewServiceUnavailable("etcd is unavailable") }
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Blocked || !strings.Contains(s12.Reason, "etcd is unavailable") {
		t.Fatalf("S12 = %+v, want blocked: the restored cluster was never read", s12)
	}
}

func TestRunS12ExactIsBlockedWhenTheRunEnds(t *testing.T) {
	cfg := testS12Config(t)
	cfg.RestoreLimit = time.Minute
	f := newS12Fixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapRestoring}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.onRestoreRead = func(read int) error {
		if read == 2 {
			cancel()
		}
		return nil
	}
	s12, err := s12Of(t, f, ctx)
	if err == nil || s12.Result != evidence.Blocked {
		t.Fatalf("S12 = %+v, want blocked", s12)
	}
	requireNamespaceGone(t, f.haFixture)
}

func TestRunS12ExactFailsOnReadyBeforeTheBootstrapCompleted(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.tamperRestore = func(read int, s *databasev1alpha1.CubridClusterStatus) {
		if s.Bootstrap.Phase == databasev1alpha1.BootstrapRestoring {
			for i := range s.Conditions {
				if s.Conditions[i].Type == startingConditions[0] {
					s.Conditions[i].Status = metav1.ConditionTrue
				}
			}
		}
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || len(s12.NeverEvents) == 0 ||
		!strings.Contains(s12.NeverEvents[0], "Ready") || !strings.Contains(s12.NeverEvents[0], "Restoring") {
		t.Fatalf("S12 = %+v, want the Ready cluster before its bootstrap completed as a never-event", s12)
	}
}

func TestRunS12ExactFailsOnTheRestoredData(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12Fixture)
		want   []string
	}{
		"an acknowledged row before the backup missing": {change: func(f *s12Fixture) {
			f.missing[testRestoreMaster] = 0
		}, want: []string{workload.RuleMissing, testRestoreMaster}},
		"a row written after the backup present": {change: func(f *s12Fixture) { f.leakPostBackup = true },
			want: []string{workload.RuleUnexpected, "written after the backup", s12PostBackupClient + "-000001"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12Config(t)
			f := newS12Fixture(t, cfg)
			tc.change(f)
			s12, err := s12Of(t, f, t.Context())
			if err == nil || s12.Result != evidence.Fail {
				t.Fatalf("S12 = %+v, want a failure", s12)
			}
			for _, want := range tc.want {
				if !strings.Contains(s12.Reason, want) {
					t.Fatalf("S12 reason = %q, want %q", s12.Reason, want)
				}
			}
			if report := readReport(t, cfg.EvidenceDir, s12DataCheckFile); report.OK {
				t.Fatal("the failed data check was not kept")
			}
			if len(f.normalWrites) != 0 {
				t.Fatal("a normal write went to a restored cluster whose data does not match")
			}
		})
	}
}

func TestRunS12ExactCountsTheRowsWrittenAfterTheBackup(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.leakPostBackup = true
	s12, _ := s12Of(t, f, t.Context())
	if s12.Measurements["postBackupRowsPresent"] != float64(1) {
		t.Fatalf("S12 measurements = %v, want one row written after the backup present", s12.Measurements)
	}
}

func TestRunS12ExactFailsWithoutTheMarker(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.dropMarkerOn = testRestoreSlave
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, s11Marker) ||
		!strings.Contains(s12.Reason, testRestoreSlave) {
		t.Fatalf("S12 = %+v, want S11's marker missing on %s", s12, testRestoreSlave)
	}
}

func TestRunS12ExactFailsWithAnotherRunsMarker(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.wrongMarkerOn = testRestoreSlave
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, testRestoreSlave) {
		t.Fatalf("S12 = %+v, want S11's marker with another value on %s refused", s12, testRestoreSlave)
	}
	if rec := readS12Record(t, cfg.EvidenceDir); rec.DataCheck == nil ||
		rec.DataCheck.Markers[testRestoreSlave] != "another-run" {
		t.Fatalf("S12 data check = %+v, want the marker found recorded", rec.DataCheck)
	}
}

func TestRunS12ExactIsBlockedUnlessEveryWriteAfterTheBackupIsAcknowledged(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.postBackupHistory = func(history string) string {
		return strings.Replace(history, `"opId":"s12p-000003","endpoint":"rw","event":"acknowledged"`,
			`"opId":"s12p-000003","endpoint":"rw","event":"failed","error":"broken"`, 1)
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Blocked || !strings.Contains(s12.Reason, "19 of 20") {
		t.Fatalf("S12 = %+v, want blocked: the writes after the backup were not all acknowledged", s12)
	}
	if f.restored != nil {
		t.Fatal("the restored cluster was created without the writes after the backup")
	}
}

func TestPostBackupWritesRefusesAHistoryThatIsNotTheKnownPoint(t *testing.T) {
	history, _ := fakeClientWorkload("c1", 3)
	// No client command may run: the run has no Runner here.
	h := &haRun{cfg: testS12Config(t), redactor: evidence.NewRedactor()}
	h.history.WriteString(history)
	rec := &s12Record{KnownPoint: &s11KnownPoint{Acknowledged: 2, LastAcknowledged: testThirdOpID}}
	if _, err := h.postBackupWrites(t.Context(), rec); err == nil || !strings.Contains(err.Error(), "known point") {
		t.Fatalf("postBackupWrites() error = %v, want the history refused against the known point", err)
	}
}

func TestRunS12ExactFailsWhenANormalWriteFails(t *testing.T) {
	for name, change := range map[string]func(*s12Fixture){
		"refused": func(f *s12Fixture) {
			f.normalWriteAnswer = func() (string, error) {
				return "Exception in thread \"main\" java.sql.SQLException: connection refused\n" +
					"command terminated with exit code 1\n", errors.New("kubectl failed: exit status 1")
			}
		},
		"not acknowledged": func(f *s12Fixture) {
			f.normalWriteAnswer = func() (string, error) { return "", nil }
		},
		"not on a member": func(f *s12Fixture) { f.dropNormalOn = testRestoreSlave },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12Config(t)
			f := newS12Fixture(t, cfg)
			change(f)
			s12, err := s12Of(t, f, t.Context())
			if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, s12Marker) {
				t.Fatalf("S12 = %+v, want the normal write's failure", s12)
			}
			if rec := readS12Record(t, cfg.EvidenceDir); rec.NormalWrite == nil {
				t.Fatal("the normal write was not recorded")
			}
		})
	}
}

func TestRunS12ExactFailsWhenTheSourceChanged(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*s12Fixture)
		want   string
	}{
		"a member restarted": {change: func(f *s12Fixture) {
			f.afterRestoreCreated = func(ctx context.Context, c client.WithWatch) {
				pod := &corev1.Pod{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: testMemberLast}, pod); err != nil {
					f.t.Fatal(err)
				}
				pod.Status.ContainerStatuses[0].ContainerID = testRestarted
				if err := c.Status().Update(ctx, pod); err != nil {
					f.t.Fatal(err)
				}
			}
		}, want: "restarted"},
		"a row lost": {change: func(f *s12Fixture) {
			f.afterRestoreCreated = func(context.Context, client.WithWatch) {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.missing[testHAPrimary] = 0
			}
		}, want: workload.RuleMissing},
		"the master moved": {change: func(f *s12Fixture) {
			f.afterRestoreCreated = func(context.Context, client.WithWatch) {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.roles[testHAPrimary], f.roles[testMemberHA0] = roleSlave, roleMaster
			}
		}, want: "master"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12Config(t)
			f := newS12Fixture(t, cfg)
			tc.change(f)
			s12, err := s12Of(t, f, t.Context())
			if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, "source") ||
				!strings.Contains(s12.Reason, tc.want) {
				t.Fatalf("S12 = %+v, want the changed source (%s)", s12, tc.want)
			}
		})
	}
}

func TestRunS12ExactFailsOnACredentialInTheRestoredStatus(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	// Only an early reading carries it.
	f.tamperRestore = func(read int, s *databasev1alpha1.CubridClusterStatus) {
		if read == 1 {
			s.Conditions[0].Message = "downloading with " + testSecretKey
		}
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || len(s12.NeverEvents) == 0 ||
		!strings.Contains(s12.NeverEvents[0], testCredential) {
		t.Fatalf("S12 = %+v, want the credential in the restored status as a never-event", s12)
	}
}

func TestRunS12ExactFailsWhenTheRestoredClusterUsesASecretOfTheSource(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.afterRestoreCreated = func(ctx context.Context, c client.WithWatch) {
		sts := &appsv1.StatefulSet{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: s12ClusterName}, sts); err != nil {
			f.t.Fatal(err)
		}
		sts.Spec.Template.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name = haClusterName + "-im-token"
		if err := c.Update(ctx, sts); err != nil {
			f.t.Fatal(err)
		}
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || len(s12.NeverEvents) == 0 ||
		!strings.Contains(s12.NeverEvents[0], haClusterName+"-im-token") {
		t.Fatalf("S12 = %+v, want the source's Secret named as a never-event", s12)
	}
}

func TestRunS12ExactFailsWhenItsRecordCannotBeWritten(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.afterRestoreCreated = func(context.Context, client.WithWatch) {
		if err := os.MkdirAll(filepath.Join(cfg.EvidenceDir, s12RecordFile, "in-the-way"), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, "write S12 evidence") {
		t.Fatalf("S12 = %+v, want a failure, with the record that could not be written", s12)
	}
}

func TestRunS12ExactStaysBlockedWhenItsRecordCannotBeWritten(t *testing.T) {
	cfg := testS12Config(t)
	cfg.RestoreLimit = 100 * time.Millisecond
	f := newS12Fixture(t, cfg)
	f.onRestoreRead = func(int) error { return apierrors.NewServiceUnavailable("etcd is unavailable") }
	f.afterRestoreCreated = func(context.Context, client.WithWatch) {
		if err := os.MkdirAll(filepath.Join(cfg.EvidenceDir, s12RecordFile, "in-the-way"), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Blocked || !strings.Contains(s12.Reason, "write S12 evidence") {
		t.Fatalf("S12 = %+v, want blocked, with the record that could not be written", s12)
	}
}

func TestRunHARefusesAnIncompleteS12Configuration(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*HAConfig)
		want   string
	}{
		"without a backup bucket": {change: func(c *HAConfig) { c.BackupBucket = "" }, want: "-backup-bucket"},
		"with a fault": {change: func(c *HAConfig) {
			// A complete S06 configuration: only the combination is refused.
			c.BackupBucket = ""
			c.Fault, c.StablePeriod, c.ResyncLimit, c.LeaderLease = FaultOperatorRestart, time.Second, time.Second,
				testLeaderLease
		}, want: "-fault"},
		"no restore limit": {change: func(c *HAConfig) { c.RestoreLimit = 0 }, want: "restore limit"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12Config(t)
			tc.change(&cfg)
			f := newS12Fixture(t, testS12Config(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunHA() error = %v, want the configuration refused for %q", err, tc.want)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}

func TestRunS12ExactIsBlockedWhenTheNormalWriteCannotBeSent(t *testing.T) {
	for name, out := range map[string]string{
		"container not reached": "error: unable to upgrade connection: container not found (\"client\")\n",
		"killed":                "command terminated with exit code 137\n",
		"command not found":     "sh: workload: not found\ncommand terminated with exit code 127\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12Config(t)
			f := newS12Fixture(t, cfg)
			f.normalWriteAnswer = func() (string, error) {
				return out, errors.New("kubectl failed: exit status 1: " + out)
			}
			s12, err := s12Of(t, f, t.Context())
			if err == nil || s12.Result != evidence.Blocked {
				t.Fatalf("S12 = %+v, want blocked: the workload did not answer", s12)
			}
		})
	}
}

func TestRunS12ExactIsBlockedWithoutAnHANodeList(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.haNodes = func(pod string, nodes []string) []string {
		if pod == testRestoreMaster {
			return nil
		}
		return nodes
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Blocked || !strings.Contains(s12.Reason, "lists no node") {
		t.Fatalf("S12 = %+v, want blocked: no node list to check", s12)
	}
}

func TestRunS12ExactKeepsTheHistoriesItWasJudgedAgainst(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	s12, err := s12Of(t, f, t.Context())
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	read := func(rel string) string {
		if !slices.Contains(s12.Evidence, rel) {
			t.Fatalf("S12 evidence = %v, want %s", s12.Evidence, rel)
		}
		data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	expected, post := read("S12/exact/expected-history.jsonl"), read("S12/exact/post-backup-history.jsonl")
	all, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, haHistoryFile))
	if err != nil {
		t.Fatal(err)
	}
	if expected+post != string(all) || strings.Contains(expected, s12PostBackupClient) ||
		!strings.Contains(post, `"client":"`+s12PostBackupClient+`"`) || !strings.Contains(expected, testROOpID) {
		t.Fatalf("expected history = %q, post-backup = %q; want LAB01's history and the writes after the backup",
			expected, post)
	}
}

func TestPostBackupWritesKeepsAnOperationWithAnUnknownOutcome(t *testing.T) {
	at := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	history, rows := fakeClientWorkload("c1", 3)
	row := workload.Row{OpID: "c1-000004", Client: "c1", Seq: 4, Amount: 149, Note: "c1-4"}
	for _, e := range []workload.Event{
		{T: at, Client: "c1", Seq: 4, Op: workload.OpInsert, OpID: row.OpID, Endpoint: "rw", Event: workload.Attempted,
			Amount: row.Amount, Note: row.Note},
		{T: at, Client: "c1", Seq: 4, Op: workload.OpInsert, OpID: row.OpID, Endpoint: "rw", Event: workload.Unknown,
			Error: "connection reset"},
	} {
		data, _ := json.Marshal(e)
		history += string(data) + "\n"
	}
	post, _ := fakeClientWorkload(s12PostBackupClient, s12PostBackupOperations)
	cfg := testS12Config(t)
	h := &haRun{Runner: &Runner{Command: func(context.Context, ...string) (string, error) { return post, nil }},
		cfg: cfg, redactor: evidence.NewRedactor(), guard: func(context.Context) error { return nil }}
	h.history.WriteString(history)
	rec := &s12Record{KnownPoint: &s11KnownPoint{Acknowledged: 3, LastAcknowledged: testThirdOpID}}
	expected, err := h.postBackupWrites(t.Context(), rec)
	if err != nil {
		t.Fatalf("postBackupWrites() error = %v", err)
	}
	parsed, err := workload.ReadHistory(strings.NewReader(expected))
	if err != nil {
		t.Fatal(err)
	}
	for name, restored := range map[string][]workload.Row{"committed": append(slices.Clone(rows), row),
		"not committed": rows} {
		if report := workload.Check(parsed, map[string][]workload.Row{testRestoreMaster: restored}); !report.OK {
			t.Fatalf("%s: report = %+v, want an operation with an unknown outcome allowed either way", name, report)
		}
	}
}

func TestRunS12ExactFailsWhenTheRestoredClusterUsesTheSourcesStorageSecret(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.afterRestoreCreated = func(ctx context.Context, c client.WithWatch) {
		sts := &appsv1.StatefulSet{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: s12ClusterName}, sts); err != nil {
			f.t.Fatal(err)
		}
		sts.Spec.Template.Spec.Containers[0].Env[1].ValueFrom.SecretKeyRef.Name = seedSecretName
		if err := c.Update(ctx, sts); err != nil {
			f.t.Fatal(err)
		}
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || len(s12.NeverEvents) == 0 ||
		!strings.Contains(s12.NeverEvents[0], seedSecretName) {
		t.Fatalf("S12 = %+v, want the source's object store Secret named as a never-event", s12)
	}
}

func TestRunS12ExactRereadsATransientSourceRole(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	flipped := false
	f.heartbeatRole = func(pod, role string) string {
		if pod == testMemberHA0 && len(f.normalWrites) > 0 && !flipped {
			flipped = true
			return "unknown"
		}
		return role
	}
	s12, err := s12Of(t, f, t.Context())
	if err != nil || s12.Result != evidence.Pass || !flipped {
		t.Fatalf("S12 = %+v, error %v; want a pass after one transient reading", s12, err)
	}
}

func TestRunS12ExactFailsOnCrossClusterHAMembership(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.haNodes = func(pod string, nodes []string) []string {
		if pod == testRestoreMaster {
			return append(nodes, testHAPrimary)
		}
		return nodes
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || len(s12.NeverEvents) == 0 ||
		!strings.Contains(s12.NeverEvents[0], "HA membership") || !strings.Contains(s12.NeverEvents[0], testHAPrimary) {
		t.Fatalf("S12 = %+v, want the shared HA membership as a never-event", s12)
	}
}

func TestRunS12ExactFailsWhenTheNormalWriteReachedTheSource(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.misroute = true
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, "source") ||
		!strings.Contains(s12.Reason, s12Marker) {
		t.Fatalf("S12 = %+v, want the marker on the source failed", s12)
	}
}

func TestRunS12ExactKeepsTheMemberStateWhenAMarkerReadFails(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	const dropped = "Server no longer responding.... Invalid argument"
	f.markerAnswer = func(pod string) (string, error, bool) {
		if pod != testRestoreSlave {
			return "", nil, false
		}
		return dropped + "\n", errors.New("kubectl failed: exit status 1"), true
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Blocked || !strings.Contains(s12.Reason, dropped) {
		t.Fatalf("S12 = %+v, want blocked: the marker could not be read", s12)
	}
	if !strings.Contains(s12.Reason, "HA-Node Info (current ha-restore-2, state slave)") ||
		!strings.Contains(s12.Reason, "restarts of ha-restore-2: ") {
		t.Fatalf("S12 reason = %q, want the state of ha-restore-2 when its read failed", s12.Reason)
	}
}

func TestRunS12ExactFailsOnACredentialEvenWhenALaterStepCannotBeMade(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.tamperRestore = func(read int, s *databasev1alpha1.CubridClusterStatus) {
		if read == 1 {
			s.Conditions[0].Message = "downloading with " + testSecretKey
		}
	}
	// The step after the restored cluster is ready cannot be made.
	f.haNodes = func(pod string, nodes []string) []string {
		if pod == testRestoreMaster {
			return nil
		}
		return nodes
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || len(s12.NeverEvents) == 0 ||
		!strings.Contains(s12.NeverEvents[0], testCredential) || !strings.Contains(s12.Reason, "lists no node") {
		t.Fatalf("S12 = %+v, want the credential as a never-event and the step that could not be made", s12)
	}
}

func TestRunS12ExactNamesEveryNeverEventInItsReason(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.tamperRestore = func(read int, s *databasev1alpha1.CubridClusterStatus) {
		if read == 1 {
			s.Conditions[0].Message = "downloading with " + testSecretKey
		}
		if s.Bootstrap.Phase == databasev1alpha1.BootstrapRestoring {
			for i := range s.Conditions {
				if s.Conditions[i].Type == startingConditions[0] {
					s.Conditions[i].Status = metav1.ConditionTrue
				}
			}
		}
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || len(s12.NeverEvents) != 2 ||
		!strings.Contains(s12.Reason, testCredential) || !strings.Contains(s12.Reason, "Restoring") {
		t.Fatalf("S12 = %+v, want both never-events in its reason", s12)
	}
}

func TestRunS12ExactReportsAMemberStateThatCannotBeRead(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.markerAnswer = func(pod string) (string, error, bool) {
		if pod != testRestoreSlave {
			return "", nil, false
		}
		return "", errors.New("kubectl failed: exit status 1: Server no longer responding"), true
	}
	inner := f.runner.Command
	f.runner.Command = func(ctx context.Context, args ...string) (string, error) {
		if script := args[len(args)-1]; strings.Contains(script, "changemode") && args[3] == testRestoreSlave {
			// As Target.command reports it: the output, and the error with it.
			return "state timed out\n", errors.New("kubectl failed: exit status 124: state timed out")
		}
		return inner(ctx, args...)
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Blocked || !strings.Contains(s12.Reason, "Server no longer responding") ||
		strings.Count(s12.Reason, "state timed out") != 1 ||
		!strings.Contains(s12.Reason, "state of ha-restore-2 could not be read in full") {
		t.Fatalf("S12 = %+v, want blocked with the read error and the state that could not be read, once", s12)
	}
}

// testLostSession is what csql printed when its session was lost after the
// read answered (#238, POC-36).
const testLostSession = "=== <Result of SELECT Command in Line 1> ===\n\n  v\n======\n  's11=x'\n\n" +
	"1 row selected. (0.004000 sec) Committed. (0.000000 sec) \n\n" +
	"Time: 10/25/26 07:10:39.992 - ERROR *** file network_cl.c, line 177 ERROR CODE = -199, Tran = 6, EID = 2\n" +
	"Server no longer responding.... Invalid argument\n\n" +
	"Time: 10/25/26 07:10:39.992 - ERROR *** file transaction_cl.c, line 600 ERROR CODE = -111, Tran = 6, EID = 3\n" +
	"Your transaction has been aborted by the system due to server failure or mode change.\n"

func TestRunS12ExactReadsAMarkerTableAgainAfterALostSession(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	lost := 0
	f.markerAnswer = func(pod string) (string, error, bool) {
		if pod != testRestoreSlave || lost > 0 {
			return "", nil, false
		}
		lost++
		return testLostSession, nil, true
	}
	s12, err := s12Of(t, f, t.Context())
	if err != nil || s12.Result != evidence.Pass {
		t.Fatalf("S12 = %+v, error %v; want a pass: the read was made again", s12, err)
	}
	var rec haRecord
	data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, haRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("LAB01 record = %s, %v", data, err)
	}
	if len(rec.LostSessions) != 1 || rec.LostSessions[0].Pod != testRestoreSlave ||
		!strings.Contains(rec.LostSessions[0].Output, "ERROR CODE = -199") {
		t.Fatalf("lost sessions = %+v, want the one on %s", rec.LostSessions, testRestoreSlave)
	}
	if summary := readSummary(t, cfg.EvidenceDir); !slices.ContainsFunc(summary.Notes, func(n string) bool {
		return strings.Contains(n, "#238")
	}) {
		t.Fatalf("summary notes = %v, want the lost session named", summary.Notes)
	}
}

func TestRunS12ExactIsBlockedWhenTheReadIsLostTwice(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12Fixture(t, cfg)
	f.markerAnswer = func(pod string) (string, error, bool) {
		if pod != testRestoreSlave {
			return "", nil, false
		}
		return testLostSession, nil, true
	}
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Blocked || !strings.Contains(s12.Reason, "-199") {
		t.Fatalf("S12 = %+v, want blocked: the read was lost again", s12)
	}
}
