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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testFormedUID      = "wt-formed"
	testRecoveryUID    = "wt-recovery"
	testWrongTarget0   = "ha-wt-0"
	testWrongTargetRW  = "ha-wt-rw."
	testRecoverySecret = "object-storage-wt-recovery"
	testRecoveryMode   = "Recovery"
)

func testS12WrongTargetConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS12Config(t)
	cfg.RestoreWrongTarget = true
	return cfg
}

// testHoldsCause and testRegisteredCause are the failure reasons of a
// restore into a target that already holds the database, as the Instance
// Manager words them (restore.go, server.go) and the Operator passes them on.
func testHoldsCause() string {
	return `restore failed: target /var/lib/cubrid/databases already holds database "appdb"; refusing to ` +
		`overwrite (ADR-0008)`
}

func testRegisteredCause() string {
	return `restore failed: database "appdb" is already registered in /var/lib/cubrid/databases/databases.txt; ` +
		`refusing to overwrite (ADR-0008)`
}

// s12WrongTargetFixture plays, after S12 exact, an Operator that forms ha-wt,
// a garbage collector that removes what it owns when it is deleted and keeps
// its PVCs, and an Operator whose restore into those PVCs is refused by the
// Instance Manager's guard: twice it starts another attempt, then the
// bootstrap ends Failed. CUBRID on the members restored over the PVCs serves
// the data the first ha-wt left.
type s12WrongTargetFixture struct {
	*s12Fixture
	// cause is the restore's failure reason; neverRefuse keeps the restore
	// at its first attempt; tamperRecovery changes the status of a reading;
	// onRecoveryRead runs first, and its error is the reading's.
	cause          string
	neverRefuse    bool
	tamperRecovery func(read int, s *databasev1alpha1.CubridClusterStatus)
	onRecoveryRead func(read int) error
	// afterRecoveryCreated runs once the cluster restored over the PVCs and
	// its children exist.
	afterRecoveryCreated func(ctx context.Context, c client.WithWatch)
	// dropVolumes removes the PVCs with the first cluster; replaceVolume
	// gives data-ha-wt-0 another UID when the second is created.
	dropVolumes, replaceVolume bool
	// overwrite puts S11's data on the members restored over the PVCs, in
	// place of the first cluster's; dropFormedOn and dropExistingOn are a
	// member without the marker before the deletion and after the refusal.
	overwrite                    bool
	dropFormedOn, dropExistingOn string
	// markerWriteAnswer answers the marker written through the first
	// cluster; existingReadErr answers a read of the marker table after the
	// refusal.
	markerWriteAnswer func() (string, error)
	existingReadErr   error
	// existingReadFailures fail the first reads of the marker table after
	// the refusal, as a member that is still joining answers.
	existingReadFailures int

	formed, recovery *databasev1alpha1.CubridCluster
	recoveryReads    int
	inits, marked    []string
	deleteOptions    *client.DeleteOptions
}

func newS12WrongTargetFixture(t *testing.T, cfg HAConfig) *s12WrongTargetFixture {
	t.Helper()
	f := &s12WrongTargetFixture{s12Fixture: newS12Fixture(t, cfg), cause: testHoldsCause()}
	for _, member := range clusterMembers(s12WrongTargetClusterName) {
		f.roles[member] = roleSlave
	}
	f.roles[testWrongTarget0] = roleMaster
	f.wrongTargetCreated = f.createWrongTarget
	f.wrongTargetRead = f.advanceRecovery
	f.clusterDeleted = f.deleteWrongTarget
	s12Client, s12Markers := f.clientHook, f.markersOf
	f.clientHook = func(script string) (string, error, bool) {
		if out, err, done := f.clientWrongTarget(script); done {
			return out, err, true
		}
		return s12Client(script)
	}
	f.markersOf = func(pod string) []string {
		if strings.HasPrefix(pod, s12WrongTargetClusterName+"-") {
			return f.wrongTargetMarkers(pod)
		}
		return s12Markers(pod)
	}
	f.markerAnswer = func(pod string) (string, error, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.recovery == nil || !strings.HasPrefix(pod, s12WrongTargetClusterName+"-") {
			return "", nil, false
		}
		if f.existingReadErr != nil {
			return "", f.existingReadErr, true
		}
		if f.existingReadFailures > 0 {
			f.existingReadFailures--
			return "ERROR: Failed to connect to database server, 'appdb', on the following host(s): localhost\n",
				errors.New("command terminated with exit code 1"), true
		}
		return "", nil, false
	}
	return f
}

// createWrongTarget stores a cluster named ha-wt: the first is formed at
// once, with its PVCs; the second, with a recovery bootstrap, gets its
// children over the PVCs that are there.
func (f *s12WrongTargetFixture) createWrongTarget(ctx context.Context, c client.WithWatch,
	cluster *databasev1alpha1.CubridCluster) error {
	recovery := cluster.Spec.Bootstrap != nil && cluster.Spec.Bootstrap.Recovery != nil
	cluster.UID, cluster.Generation = testFormedUID, 1
	if recovery {
		cluster.UID = testRecoveryUID
	}
	f.mu.Lock()
	if recovery {
		f.recovery = cluster.DeepCopy()
	} else {
		f.formed = cluster.DeepCopy()
	}
	f.mu.Unlock()
	if err := c.Create(ctx, cluster); err != nil {
		return err
	}
	if !recovery {
		cluster.Status.ObservedGeneration = 1
		cluster.Status.CurrentPrimary = testWrongTarget0
		cluster.Status.Databases = []databasev1alpha1.DatabaseStatus{{Name: databaseName, Phase: "Created",
			PrimaryCreated: true, HAConfigured: true}}
		for _, condition := range append([]string{bootstrapReadyCondition}, startingConditions...) {
			cluster.Status.Conditions = append(cluster.Status.Conditions, metav1.Condition{Type: condition,
				Status: metav1.ConditionTrue, Reason: testConditionReason})
		}
		if err := c.Update(ctx, cluster); err != nil {
			return err
		}
	}
	if recovery && f.replaceVolume {
		pvc := &corev1.PersistentVolumeClaim{}
		key := client.ObjectKey{Namespace: f.cfg.Namespace, Name: "data-" + testWrongTarget0}
		if err := c.Get(ctx, key, pvc); err == nil {
			if err := c.Delete(ctx, pvc); err != nil {
				return err
			}
		}
		if err := c.Create(ctx, testVolume(f.cfg.Namespace, testWrongTarget0, "-new")); err != nil {
			return err
		}
	}
	// The StatefulSet controller creates a PVC that is missing.
	for _, member := range clusterMembers(cluster.Name) {
		err := c.Create(ctx, testVolume(f.cfg.Namespace, member, ""))
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	if err := f.createChildren(ctx, c, cluster); err != nil {
		return err
	}
	if recovery && f.afterRecoveryCreated != nil {
		f.afterRecoveryCreated(ctx, c)
	}
	return nil
}

// testVolume is the data PVC of a member; suffix changes its UID.
func testVolume(namespace, member, suffix string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-" + member, Namespace: namespace,
		UID: types.UID("pvc-" + member + suffix)}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-" + member},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
}

// deleteWrongTarget deletes ha-wt and what it owns, keeping its PVCs unless
// dropVolumes is set.
func (f *s12WrongTargetFixture) deleteWrongTarget(ctx context.Context, c client.WithWatch,
	cluster *databasev1alpha1.CubridCluster, opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if cluster.Name != s12WrongTargetClusterName || options.Preconditions == nil ||
		options.Preconditions.UID == nil || *options.Preconditions.UID != testFormedUID {
		f.t.Fatalf("runner deleted cluster %s with %+v, want ha-wt with its UID precondition", cluster.Name, options)
	}
	f.mu.Lock()
	f.deleteOptions = options
	f.mu.Unlock()
	if err := c.Delete(ctx, cluster, opts...); err != nil {
		return err
	}
	name, ns := cluster.Name, f.cfg.Namespace
	objects := []client.Object{&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}}
	for _, mode := range []string{"rw", "ro"} {
		objects = append(objects, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name + "-broker-" + mode,
			Namespace: ns}})
	}
	for _, member := range clusterMembers(name) {
		objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: member, Namespace: ns}})
		if f.dropVolumes {
			objects = append(objects, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-" + member,
				Namespace: ns}})
		}
	}
	for _, obj := range objects {
		if err := c.Delete(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}

// advanceRecovery writes the status of the next reading of the cluster
// restored over the PVCs, as the Operator would; the first ha-wt keeps its
// status.
func (f *s12WrongTargetFixture) advanceRecovery(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, key, cluster); err != nil || cluster.Spec.Bootstrap == nil {
		return nil
	}
	f.mu.Lock()
	f.recoveryReads++
	read := f.recoveryReads
	onRead, tamper := f.onRecoveryRead, f.tamperRecovery
	f.mu.Unlock()
	if onRead != nil {
		if err := onRead(read); err != nil {
			return err
		}
	}
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	s := &cluster.Status
	s.ObservedGeneration = 1
	s.Bootstrap = &databasev1alpha1.BootstrapStatus{Mode: testRecoveryMode, Phase: databasev1alpha1.BootstrapPreparing,
		ManifestURI: cluster.Spec.Bootstrap.Recovery.ManifestURI, TargetMember: testWrongTarget0,
		OperationID: "restore-op-1"}
	boot := metav1.Condition{Type: bootstrapReadyCondition, Status: metav1.ConditionFalse, Reason: "RestoreInProgress",
		Message: "the backup is being verified and downloaded on " + testWrongTarget0, LastTransitionTime: now}
	switch attempt := min(read, 4) - 1; {
	case f.neverRefuse || read == 1:
	case attempt < 3:
		s.Bootstrap.Attempts, s.Bootstrap.OperationID = int32(attempt), ""
		boot.Reason, boot.Message = "RestoreFailedRetrying", "on "+testWrongTarget0+": "+f.cause+
			fmt.Sprintf("; starting attempt %d of 3", attempt+1)
	default:
		s.Bootstrap.Phase, s.Bootstrap.Attempts, s.Bootstrap.OperationID = databasev1alpha1.BootstrapFailed, 2, ""
		boot.Reason, boot.Message = s12RefusedReason, "on "+testWrongTarget0+": "+f.cause+"; giving up after 3 attempts"
	}
	s.Conditions = []metav1.Condition{boot}
	for _, condition := range startingConditions {
		s.Conditions = append(s.Conditions, metav1.Condition{Type: condition, Status: metav1.ConditionFalse,
			Reason: testConditionReason, LastTransitionTime: now})
	}
	if tamper != nil {
		tamper(read, s)
	}
	if err := c.Update(ctx, cluster); err != nil {
		f.t.Error(err)
	}
	return nil
}

// clientWrongTarget answers workload init and the marker written through
// ha-wt.
func (f *s12WrongTargetFixture) clientWrongTarget(script string) (string, error, bool) {
	if !strings.Contains(script, testWrongTargetRW) {
		return "", nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(script, "workload init"):
		f.inits = append(f.inits, script)
		return "", nil, true
	case strings.Contains(script, "workload marker "+s12WrongTargetMarker+" "):
		f.marked = append(f.marked, script)
		if f.markerWriteAnswer != nil {
			out, err := f.markerWriteAnswer()
			return out, err, true
		}
		return "marker " + s12WrongTargetMarker + " written\n", nil, true
	}
	return "", nil, false
}

// wrongTargetMarkers is the marker table of a member of ha-wt: the marker
// written through it, or S11's when its data was overwritten.
func (f *s12WrongTargetFixture) wrongTargetMarkers(pod string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	recovered := f.recovery != nil
	if f.overwrite && recovered && len(f.markers) == 1 {
		return []string{s11Marker + "=" + strings.Fields(f.markers[0])[3]}
	}
	if len(f.marked) == 0 || (!recovered && pod == f.dropFormedOn) || (recovered && pod == f.dropExistingOn) {
		return nil
	}
	_, rest, _ := strings.Cut(f.marked[0], "workload marker "+s12WrongTargetMarker+" ")
	return []string{s12WrongTargetMarker + "=" + strings.Fields(rest)[0]}
}

func wrongTargetScenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s12WrongTargetLane || len(summary.Scenarios) != 4 || summary.Scenarios[3].ID != s12ID ||
		summary.Scenarios[3].Variant != s12WrongTargetVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete, S12/exact and S12/wrong-target in lane %s", summary,
			s12WrongTargetLane)
	}
	return summary.Scenarios
}

// wrongTargetOf runs the fixture and returns S12 wrong-target, requiring
// LAB01, S11 and S12 exact passed.
func wrongTargetOf(t *testing.T, f *s12WrongTargetFixture, ctx context.Context) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(ctx, f.cfg)
	scenarios := wrongTargetScenarios(t, f.cfg.EvidenceDir)
	for _, s := range scenarios[:3] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01, S11 and S12 exact passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, scenarios[2], "S12/exact")
	requireOwnEvidence(t, scenarios[3], "S12/wrong-target")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	return scenarios[3], err
}

func readWrongTargetRecord(t *testing.T, dir string) s12WrongTargetRecord {
	t.Helper()
	var rec s12WrongTargetRecord
	data, err := os.ReadFile(filepath.Join(dir, s12WrongTargetRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S12 wrong-target record = %s, %v", data, err)
	}
	return rec
}

func TestRunS12WrongTargetPassesWhenTheRestoreIsRefused(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	f := newS12WrongTargetFixture(t, cfg)
	wrong, err := wrongTargetOf(t, f, t.Context())
	if err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if summary := readSummary(t, cfg.EvidenceDir); !summary.Passed || wrong.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want every scenario passed", summary)
	}
	if strings.Join(f.writes, ",") != "namespace,secret,cluster,client,backup,secret,restore-cluster,"+
		"secret,wrong-target-cluster,secret,wrong-target-cluster" {
		t.Fatalf("writes = %v", f.writes)
	}
	for _, rel := range []string{s12WrongTargetRecordFile, s12WrongTargetSourceDataCheckFile} {
		if !slices.Contains(wrong.Evidence, rel) {
			t.Fatalf("S12 wrong-target evidence = %v, want %s", wrong.Evidence, rel)
		}
	}
	if wrong.Limits["restore_limit"] != cfg.RestoreLimit.String() ||
		wrong.Measurements["refusedWithin"] == evidence.Unknown || wrong.Measurements["attempts"] != float64(2) {
		t.Fatalf("S12 wrong-target limits = %v, measurements = %v", wrong.Limits, wrong.Measurements)
	}
	requireWrongTargetSpecs(t, f)
	requireWrongTargetRecord(t, f)
	if len(f.inits) != 1 || len(f.marked) != 1 {
		t.Fatalf("init = %v, marker = %v; want each once through ha-wt-rw", f.inits, f.marked)
	}
	if f.deleteOptions.PropagationPolicy == nil ||
		*f.deleteOptions.PropagationPolicy != metav1.DeletePropagationBackground {
		t.Fatalf("delete options = %+v, want background propagation", f.deleteOptions)
	}
	requireNamespaceGone(t, f.haFixture)
}

// requireWrongTargetSpecs checks the two clusters the run created.
func requireWrongTargetSpecs(t *testing.T, f *s12WrongTargetFixture) {
	t.Helper()
	lab01, s11 := readHARecord(t, f.cfg.EvidenceDir), readS11Record(t, f.cfg.EvidenceDir)
	prefix := "lab-runs/" + lab01.RunID + "/wt-seed"
	formed, recovery := f.formed, f.recovery
	if formed == nil || formed.Spec.Bootstrap != nil || formed.Spec.ObjectStorage.Prefix != prefix ||
		formed.Spec.ObjectStorage.CredentialsSecretRef.Name != s12WrongTargetSecretName {
		t.Fatalf("first cluster = %+v, want a new cluster with a Secret and a prefix of its own", formed)
	}
	if recovery == nil || recovery.Name != s12WrongTargetClusterName || recovery.Spec.Bootstrap == nil ||
		recovery.Spec.Bootstrap.Recovery == nil || recovery.Spec.Bootstrap.Recovery.ManifestURI != s11.Artifact.URI ||
		recovery.Spec.ObjectStorage.Prefix != prefix ||
		recovery.Spec.ObjectStorage.CredentialsSecretRef.Name != testRecoverySecret {
		t.Fatalf("second cluster = %+v, want S11's artifact restored into ha-wt with a Secret of its own", recovery)
	}
	if len(f.secrets) != 4 || f.secrets[2].Name != s12WrongTargetSecretName || f.secrets[3].Name != testRecoverySecret ||
		string(f.secrets[3].Data[seedAccessKey]) != testAccessKey {
		t.Fatalf("Secrets = %v, want the seed Secret copied for each ha-wt", f.secrets)
	}
}

// requireWrongTargetRecord checks the record of a passed run.
func requireWrongTargetRecord(t *testing.T, f *s12WrongTargetFixture) {
	t.Helper()
	rec := readWrongTargetRecord(t, f.cfg.EvidenceDir)
	formed := rec.Formed
	if formed == nil || formed.ClusterUID != testFormedUID || formed.Roles == nil ||
		formed.Roles.Master != testWrongTarget0 || len(formed.Members) != haMemberCount ||
		formed.Marker == nil || !formed.Marker.Acknowledged || len(formed.Marker.ReadBackOn) != haMemberCount ||
		len(formed.S11MarkerOn) != 0 || !formed.VolumesRetained || formed.GoneWithin == "" {
		t.Fatalf("formed = %+v, want it formed, written to, deleted and its volumes retained", formed)
	}
	requireRefusalRecord(t, f, rec)
}

// requireRefusalRecord checks what a passed run recorded from the refusal on.
func requireRefusalRecord(t *testing.T, f *s12WrongTargetFixture, rec s12WrongTargetRecord) {
	t.Helper()
	lab01 := readHARecord(t, f.cfg.EvidenceDir)
	wantVolumes := make([]s12Volume, 0, haMemberCount)
	for _, member := range clusterMembers(s12WrongTargetClusterName) {
		wantVolumes = append(wantVolumes, s12Volume{Name: "data-" + member, UID: "pvc-" + member,
			VolumeName: "pv-" + member, Phase: string(corev1.ClaimBound)})
	}
	if !slices.Equal(rec.Formed.Volumes, wantVolumes) || !slices.Equal(rec.VolumesAfterRefusal, wantVolumes) ||
		!rec.SameVolumes {
		t.Fatalf("volumes = %+v, after the refusal %+v; want the same three PVCs", rec.Formed.Volumes,
			rec.VolumesAfterRefusal)
	}
	attempts := make([]int32, 0, len(rec.Bootstrap))
	for _, p := range rec.Bootstrap {
		attempts = append(attempts, p.Attempts)
	}
	if n := len(rec.Bootstrap); n != 4 || rec.Bootstrap[n-1].Phase != string(databasev1alpha1.BootstrapFailed) ||
		!slices.Equal(attempts, []int32{0, 1, 2, 2}) {
		t.Fatalf("bootstrap = %+v, want each attempt and the Failed phase", rec.Bootstrap)
	}
	if rec.RecoveryUID != testRecoveryUID || rec.RefusedWithin == "" || !strings.HasPrefix(rec.Refusal,
		s12RefusedReason+": on "+testWrongTarget0+": "+testHoldsCause()) {
		t.Fatalf("record = %+v, want the cluster and the refusal", rec)
	}
	data := rec.ExistingData
	if data == nil || !data.Checked || len(data.MarkerOn) != haMemberCount || len(data.MissingOn) != 0 ||
		len(data.S11MarkerOn) != 0 {
		t.Fatalf("existing data = %+v, want the marker on every member and S11's on none", data)
	}
	if !slices.Contains(rec.SecretsReferenced, testRecoverySecret) || slices.Contains(rec.SecretsReferenced,
		seedSecretName) {
		t.Fatalf("Secrets referenced = %v, want the cluster's own", rec.SecretsReferenced)
	}
	if rec.Source == nil || rec.Source.Restarted == nil || *rec.Source.Restarted != 0 || !rec.Source.DataRulesHeld {
		t.Fatalf("source = %+v, want it unchanged", rec.Source)
	}
	if rec.Credentials == nil || rec.Credentials.StatusesChecked < 6 || len(rec.Credentials.FoundIn) != 0 {
		t.Fatalf("credentials = %+v, want every status searched", rec.Credentials)
	}
	if !strings.Contains(f.marked[0], lab01.RunID) {
		t.Fatalf("marker = %q, want the run ID as its value", f.marked[0])
	}
}

func TestRunS12WrongTargetAcceptsTheRegisteredGuard(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	f := newS12WrongTargetFixture(t, cfg)
	f.cause = testRegisteredCause()
	wrong, err := wrongTargetOf(t, f, t.Context())
	if err != nil || wrong.Result != evidence.Pass {
		t.Fatalf("S12 wrong-target = %+v, error %v; want a pass", wrong, err)
	}
	if rec := readWrongTargetRecord(t, cfg.EvidenceDir); !strings.Contains(rec.Refusal, testRegisteredCause()) {
		t.Fatalf("refusal = %q, want the registration guard", rec.Refusal)
	}
}

func TestRunS12WrongTargetFailsWhenTheClusterIsReady(t *testing.T) {
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
		"phase Complete": {tamper: func(read int, s *databasev1alpha1.CubridClusterStatus) {
			if read == 2 {
				s.Bootstrap.Phase = databasev1alpha1.BootstrapComplete
			}
		}, want: string(databasev1alpha1.BootstrapComplete)},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12WrongTargetConfig(t)
			f := newS12WrongTargetFixture(t, cfg)
			f.tamperRecovery = tc.tamper
			wrong, err := wrongTargetOf(t, f, t.Context())
			if err == nil || wrong.Result != evidence.Fail || len(wrong.NeverEvents) != 1 ||
				!strings.Contains(wrong.NeverEvents[0], tc.want) || !strings.Contains(wrong.Reason, tc.want) {
				t.Fatalf("S12 wrong-target = %+v, want %s as a never-event", wrong, tc.want)
			}
		})
	}
}

func TestRunS12WrongTargetFailsWhenTheRestoreIsRefusedForAnotherReason(t *testing.T) {
	for name, tc := range map[string]struct{ reason, message string }{
		"checksum": {reason: s12RefusedReason, message: "on ha-wt-0: " + testRefusalCause(testFirstBackupKey) +
			"; giving up after 3 attempts"},
		"restoredb": {reason: s12RefusedReason,
			message: "on ha-wt-0: restore failed: restoredb failed: exit status 1; giving up after 3 attempts"},
		"another database": {reason: s12RefusedReason, message: "on ha-wt-0: " +
			strings.Replace(testHoldsCause(), `"appdb"`, `"otherdb"`, 1) + "; giving up after 3 attempts"},
		"another reason": {reason: "InvalidManifestURI", message: testHoldsCause()},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12WrongTargetConfig(t)
			f := newS12WrongTargetFixture(t, cfg)
			f.tamperRecovery = func(_ int, s *databasev1alpha1.CubridClusterStatus) {
				if s.Bootstrap.Phase == databasev1alpha1.BootstrapFailed {
					s.Conditions[0].Reason, s.Conditions[0].Message = tc.reason, tc.message
				}
			}
			wrong, err := wrongTargetOf(t, f, t.Context())
			if err == nil || wrong.Result != evidence.Fail || !strings.Contains(wrong.Reason, tc.message) ||
				len(wrong.NeverEvents) != 0 {
				t.Fatalf("S12 wrong-target = %+v, want a failure naming the other reason", wrong)
			}
		})
	}
}

func TestRunS12WrongTargetFailsWhenNotRefusedWithinTheRestoreLimit(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	cfg.RestoreLimit = 300 * time.Millisecond
	f := newS12WrongTargetFixture(t, cfg)
	f.neverRefuse = true
	wrong, err := wrongTargetOf(t, f, t.Context())
	if err == nil || wrong.Result != evidence.Fail || !strings.Contains(wrong.Reason, "restore_limit") ||
		!strings.Contains(wrong.Reason, "Preparing") {
		t.Fatalf("S12 wrong-target = %+v, want the restore limit exceeded with the last phase", wrong)
	}
	if wrong.Measurements["refusedWithin"] != evidence.Unknown || wrong.Measurements["attempts"] != evidence.Unknown {
		t.Fatalf("S12 wrong-target measurements = %v, want them unknown", wrong.Measurements)
	}
}

func TestRunS12WrongTargetIsBlockedWhenTheClusterCannotBeRead(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	cfg.RestoreLimit = 300 * time.Millisecond
	f := newS12WrongTargetFixture(t, cfg)
	f.onRecoveryRead = func(int) error { return apierrors.NewServiceUnavailable("etcd is unavailable") }
	wrong, err := wrongTargetOf(t, f, t.Context())
	if err == nil || wrong.Result != evidence.Blocked || !strings.Contains(wrong.Reason, "etcd is unavailable") {
		t.Fatalf("S12 wrong-target = %+v, want blocked: the cluster was never read", wrong)
	}
}

func TestRunS12WrongTargetIsBlockedWhenTheVolumesAreNotTheFirstClusters(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *s12WrongTargetFixture)
		want   string
	}{
		"a PVC replaced": {change: func(f *s12WrongTargetFixture) { f.replaceVolume = true },
			want: "data-" + testWrongTarget0},
		"the PVCs removed with the cluster": {change: func(f *s12WrongTargetFixture) { f.dropVolumes = true },
			want: "not retained"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12WrongTargetConfig(t)
			f := newS12WrongTargetFixture(t, cfg)
			tc.change(f)
			wrong, err := wrongTargetOf(t, f, t.Context())
			if err == nil || wrong.Result != evidence.Blocked || !strings.Contains(wrong.Reason, tc.want) {
				t.Fatalf("S12 wrong-target = %+v, want blocked by %q", wrong, tc.want)
			}
			if rec := readWrongTargetRecord(t, cfg.EvidenceDir); rec.SameVolumes {
				t.Fatalf("record = %+v, want the volumes not the same", rec)
			}
		})
	}
}

func TestRunS12WrongTargetFailsWhenTheExistingDataChanged(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *s12WrongTargetFixture)
		want   []string
	}{
		"overwritten with the backup": {change: func(f *s12WrongTargetFixture) { f.overwrite = true },
			want: []string{"S11's marker", "is not on"}},
		"the marker gone from a member": {change: func(f *s12WrongTargetFixture) { f.dropExistingOn = "ha-wt-2" },
			want: []string{"ha-wt-2"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12WrongTargetConfig(t)
			f := newS12WrongTargetFixture(t, cfg)
			tc.change(f)
			wrong, err := wrongTargetOf(t, f, t.Context())
			if err == nil || wrong.Result != evidence.Fail || len(wrong.NeverEvents) != 1 {
				t.Fatalf("S12 wrong-target = %+v, want a failure: the existing database changed", wrong)
			}
			for _, want := range tc.want {
				if !strings.Contains(wrong.Reason, want) {
					t.Fatalf("S12 wrong-target reason = %q, want %q", wrong.Reason, want)
				}
			}
		})
	}
}

func TestRunS12WrongTargetIsBlockedWhenTheExistingDataCannotBeRead(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	f := newS12WrongTargetFixture(t, cfg)
	f.existingReadErr = errors.New("command terminated with exit code 1: server is not running")
	wrong, err := wrongTargetOf(t, f, t.Context())
	if err == nil || wrong.Result != evidence.Blocked || !strings.Contains(wrong.Reason, "server is not running") {
		t.Fatalf("S12 wrong-target = %+v, want blocked: the data could not be read", wrong)
	}
}

func TestRunS12WrongTargetIsBlockedWithoutItsStartingState(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *s12WrongTargetFixture)
		want   string
	}{
		"marker refused": {change: func(f *s12WrongTargetFixture) {
			f.markerWriteAnswer = func() (string, error) {
				return "SQLException: connection refused", errors.New("command terminated with exit code 1")
			}
		}, want: "connection refused"},
		"marker not read back": {change: func(f *s12WrongTargetFixture) { f.dropFormedOn = "ha-wt-1" },
			want: "ha-wt-1"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12WrongTargetConfig(t)
			f := newS12WrongTargetFixture(t, cfg)
			tc.change(f)
			wrong, err := wrongTargetOf(t, f, t.Context())
			if err == nil || wrong.Result != evidence.Blocked || !strings.Contains(wrong.Reason, tc.want) {
				t.Fatalf("S12 wrong-target = %+v, want blocked by %q", wrong, tc.want)
			}
			if f.recovery != nil {
				t.Fatal("a restore was started over volumes the run could not prepare")
			}
		})
	}
}

func TestRunS12WrongTargetIsBlockedWhenS12ExactDidNotPass(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	f := newS12WrongTargetFixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	scenarios := wrongTargetScenarios(t, cfg.EvidenceDir)
	if wrong := scenarios[3]; wrong.Result != evidence.Blocked ||
		!strings.Contains(wrong.Reason, "S12 exact did not pass") {
		t.Fatalf("S12 wrong-target = %+v, want blocked by S12 exact", wrong)
	}
	if f.formed != nil || f.recovery != nil {
		t.Fatal("S12 wrong-target ran although S12 exact did not pass")
	}
}

func TestRunS12WrongTargetFailsWhenTheSourceChanged(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	f := newS12WrongTargetFixture(t, cfg)
	f.afterRecoveryCreated = f.restartSourceMember
	wrong, err := wrongTargetOf(t, f, t.Context())
	if err == nil || wrong.Result != evidence.Fail || !strings.Contains(wrong.Reason, "source") ||
		!strings.Contains(wrong.Reason, "restarted") {
		t.Fatalf("S12 wrong-target = %+v, want the changed source", wrong)
	}
}

func TestRunS12WrongTargetFailsOnACredentialInTheStatus(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	f := newS12WrongTargetFixture(t, cfg)
	f.tamperRecovery = func(read int, s *databasev1alpha1.CubridClusterStatus) {
		if read == 1 {
			s.Conditions[0].Message = "downloading with " + testSecretKey
		}
	}
	wrong, err := wrongTargetOf(t, f, t.Context())
	if err == nil || wrong.Result != evidence.Fail || len(wrong.NeverEvents) == 0 ||
		!strings.Contains(wrong.NeverEvents[0], testCredential) {
		t.Fatalf("S12 wrong-target = %+v, want the credential in the status as a never-event", wrong)
	}
}

func TestRunHARefusesAnInvalidS12WrongTargetConfiguration(t *testing.T) {
	for name, change := range map[string]func(*HAConfig){
		"without -restore":       func(c *HAConfig) { c.Restore = false },
		"with -restore-tampered": func(c *HAConfig) { c.RestoreTampered = true },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS12WrongTargetConfig(t)
			change(&cfg)
			f := newS12WrongTargetFixture(t, testS12WrongTargetConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil ||
				!strings.Contains(err.Error(), "-restore-wrong-target") {
				t.Fatalf("RunHA() error = %v, want -restore-wrong-target refused", err)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}

func TestRunS12WithoutRestoreWrongTargetKeepsItsLane(t *testing.T) {
	cfg := testS12Config(t)
	f := newS12WrongTargetFixture(t, cfg)
	if _, err := s12Of(t, f.s12Fixture, t.Context()); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if f.formed != nil || f.recovery != nil {
		t.Fatal("S12 wrong-target ran without -restore-wrong-target")
	}
}

func TestRunS12WrongTargetWaitsForTheExistingDataToAnswer(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	f := newS12WrongTargetFixture(t, cfg)
	f.existingReadFailures = 2
	wrong, err := wrongTargetOf(t, f, t.Context())
	if err != nil || wrong.Result != evidence.Pass {
		t.Fatalf("S12 wrong-target = %+v, error %v; want a pass once every member answered", wrong, err)
	}
	if rec := readWrongTargetRecord(t, cfg.EvidenceDir); rec.ExistingData == nil ||
		!strings.Contains(rec.ExistingData.FirstReadError, "Failed to connect") {
		t.Fatalf("existing data = %+v, want the reads that failed first recorded", rec.ExistingData)
	}
}

func TestRunS12WrongTargetCountsItsLostSessions(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	f := newS12WrongTargetFixture(t, cfg)
	wrong, err := wrongTargetOf(t, f, t.Context())
	if err != nil || wrong.Result != evidence.Pass || wrong.Measurements["lostSessions"] != float64(0) {
		t.Fatalf("S12 wrong-target = %+v, error %v; want a pass with no lost session counted", wrong, err)
	}
}
