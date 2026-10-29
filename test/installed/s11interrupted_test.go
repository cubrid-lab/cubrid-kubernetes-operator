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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

const (
	testInterruptedUID        = "interrupted-backup-uid"
	testInterruptedClusterUID = "interrupted-cluster-uid"
	testInterruptedPodUID     = "interrupted-pod-uid"
	testInterruptedPodIP      = "10.42.5.20"
	// testStoreIP is the object store Pod's IP, the one endpoint of its
	// headless Service, as on the VM lab; testStorePod is that Pod.
	// testStoreClusterIP is the ClusterIP of a Service that is not headless.
	testStoreIP        = "10.42.1.9"
	testStorePod       = "seaweedfs-0"
	testStoreClusterIP = "10.43.0.50"
	testStorePort      = 8333
	testStorePortName  = "s3"
	// testResetAnswer is what minio-go reports of a reset connection.
	testResetAnswer = `Put "http://seaweedfs.object-store.svc:8333/lab-backups/x": write tcp ` +
		testInterruptedPodIP + `:41234->` + testStoreIP + `:8333: write: connection reset by peer`
	// testUploadPerRead is what the fake upload puts through the limit at
	// each reading of the backup while it is Uploading.
	testUploadPerRead = 300 << 10
)

// Strings the S11 variants' tests share.
const (
	testElsewhereURI   = "s3://elsewhere/manifest.json"
	testInProgress     = "backup in progress"
	testUploadRunning  = "UploadRunning"
	testCompleted      = "BackupCompleted"
	testBackupdbFailed = "backupdb failed: exit status 1"
	testStoreNamespace = "object-store"
	testArtifactField  = "status.artifact"
	testNoBucket       = "without -backup-bucket"
	testWithRestore    = "with -restore"
	testRestoreFlag    = "-restore"
	// The cases of the refusal tables that combine two S11 variants.
	testWithMissing     = "with the missing-destination variant"
	testWithDenied      = "with the denied-credentials variant"
	testWithInterrupted = "with the interrupted-upload variant"
	// testUploadingMessage and testCompletedMessage are the Ready messages of
	// an upload under way and of a completed backup; testStoreService is the
	// object store's Service.
	testUploadingMessage = "uploading artifact"
	testCompletedMessage = "backup completed"
	testStoreService     = "seaweedfs"
	// phaseUploading and phaseCompleted are backup phases as the record
	// keeps them.
	phaseUploading = string(databasev1alpha1.BackupPhaseUploading)
	phaseCompleted = string(databasev1alpha1.BackupPhaseCompleted)
)

func testS11InterruptedConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testS11Config(t)
	cfg.BackupInterruptedUpload = true
	cfg.BackupInterruptedReadyLimit = 5 * time.Second
	return cfg
}

// testStoreServiceObject is the object store's Service the cluster's
// endpoint names, headless as on the VM lab.
func testStoreServiceObject() *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: testStoreService, Namespace: testStoreNamespace},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone,
			Ports: []corev1.ServicePort{{Name: testStorePortName, Port: testStorePort}}}}
}

// testStoreSlice is an EndpointSlice of the object store's Service with a
// ready endpoint at each of ips, on the store's port, and one endpoint that is not ready.
func testStoreSlice(name string, ips ...string) *discoveryv1.EndpointSlice {
	tcp, portName, port, ready, notReady := corev1.ProtocolTCP, testStorePortName, int32(testStorePort), true, false
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testStoreNamespace,
		Labels: map[string]string{discoveryv1.LabelServiceName: testStoreService}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Port: &port, Protocol: &tcp}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.42.3.30"},
			Conditions: discoveryv1.EndpointConditions{Ready: &notReady}}}}
	for i, ip := range ips {
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{Addresses: []string{ip},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: fmt.Sprintf("seaweedfs-%d", i)}})
	}
	return slice
}

// fakeNetns answers the commands the network faults run in the member's
// ephemeral container, with a model of its network namespace: the limit
// counts what passes it, and the reset rule refuses and counts.
type fakeNetns struct {
	mu sync.Mutex
	// member is the Pod the faults act in, s11InterruptedMember unless set.
	member                    string
	throttleIP, resetIP       string
	throttlePort, resetPort   int
	bandBytes, bandPackets    uint64
	rejected, rejectedBytes   uint64
	inertReset, ignoreUnreset bool
	ops                       []string
	// stats counts the reads of the limit's counters; from the failFrom-th,
	// failCount of them fail. onUnthrottle runs when the limit is removed.
	stats, failFrom, failCount int
	onUnthrottle               func()
}

var netnsArg = regexp.MustCompile(`^: netfault (\S+)(?: (\S+) (\d+)(?: (\d+))?)?`)

func (n *fakeNetns) answer(_ context.Context, pod, script string) (string, error) {
	if pod != cmp.Or(n.member, s11InterruptedMember) {
		return "", fmt.Errorf("network fault command in Pod %s", pod)
	}
	m := netnsArg.FindStringSubmatch(strings.SplitN(script, "\n", 2)[0])
	if m == nil {
		return "", fmt.Errorf("unexpected script %q", script)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ops = append(n.ops, m[1])
	port, _ := strconv.Atoi(m[3])
	switch m[1] {
	case "addr":
		return "2: eth0    inet " + testInterruptedPodIP + "/24 brd 10.42.5.255 scope global eth0\n", nil
	case "qdisc":
		if n.throttleIP == "" {
			return "qdisc noqueue 0: root refcnt 2\n", nil
		}
		return "qdisc prio 1: root refcnt 3 bands 4\nqdisc tbf 40: parent 1:4 rate 256Kbit burst 16Kb lat 400ms\n", nil
	case "filter":
		if n.throttleIP == "" {
			return "", nil
		}
		v4 := net.ParseIP(n.throttleIP).To4()
		return fmt.Sprintf("filter parent 1: protocol ip pref 1 u32 chain 0 fh 800::800 flowid 1:4\n"+
			"  match %02x%02x%02x%02x/ffffffff at 16\n  match 00060000/00ff0000 at 8\n"+
			"  match 0000%04x/0000ffff at 20\n", v4[0], v4[1], v4[2], v4[3], n.throttlePort), nil
	case "stats":
		n.stats++
		if n.failFrom > 0 && n.stats >= n.failFrom && n.stats < n.failFrom+n.failCount {
			return "", errors.New("error dialing backend: EOF")
		}
		return fmt.Sprintf("qdisc prio 1: root refcnt 3 bands 4\n Sent 1 bytes 1 pkt (dropped 0, overlimits 0 "+
			"requeues 0)\nqdisc tbf 40: parent 1:4 rate 256Kbit burst 16Kb lat 400ms\n Sent %d bytes %d pkt "+
			"(dropped 0, overlimits 7 requeues 0)\n", n.bandBytes, n.bandPackets), nil
	case "throttle":
		n.throttleIP, n.throttlePort = m[2], port
	case "unthrottle":
		n.throttleIP, n.throttlePort = "", 0
		if n.onUnthrottle != nil {
			n.onUnthrottle()
		}
	case "rules":
		return n.save(), nil
	case "reset":
		n.resetIP, n.resetPort = m[2], port
	case "unreset":
		if !n.ignoreUnreset {
			n.resetIP, n.resetPort = "", 0
		}
	case "unmark":
	case "connect":
		count, _ := strconv.Atoi(m[4])
		var out strings.Builder
		for range count {
			if n.send(m[2], 60) {
				out.WriteString("7|0.000000|Failed to connect: Connection refused\n")
			} else {
				out.WriteString("0|0.000500|\n")
			}
		}
		return out.String(), nil
	default:
		return "", fmt.Errorf("unexpected script %q", script)
	}
	return "", nil
}

// send sends bytes of TCP to addr through the limit and the reset rule, and
// reports whether they were refused. It runs with n.mu held.
func (n *fakeNetns) send(addr string, bytes uint64) bool {
	if n.resetIP == addr && !n.inertReset {
		n.rejected++
		n.rejectedBytes += bytes
		return true
	}
	if n.throttleIP == addr {
		n.bandPackets += 3
		n.bandBytes += bytes
	}
	return false
}

func (n *fakeNetns) save() string {
	var b strings.Builder
	b.WriteString("*filter\n:INPUT ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\n")
	if n.resetIP != "" {
		fmt.Fprintf(&b, ":NETFAULT-OUT - [0:0]\n[0:0] -A OUTPUT -j NETFAULT-OUT\n[%d:%d] -A NETFAULT-OUT -d %s/32 "+
			"-p tcp -m tcp --dport %d -j REJECT --reject-with tcp-reset\n", n.rejected, n.rejectedBytes, n.resetIP,
			n.resetPort)
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// sizedStore is the store as the run's minio-go client lists it: with sizes,
// and with the incomplete multipart uploads of incomplete.
type sizedStore struct {
	*fakeStore
	incomplete []string
}

func (s sizedStore) ListSized(ctx context.Context, bucket, prefix string) ([]storedObject, error) {
	keys, err := s.List(ctx, bucket, prefix)
	objects := make([]storedObject, 0, len(keys))
	for _, k := range keys {
		s.mu.Lock()
		objects = append(objects, storedObject{Key: k, Size: int64(len(s.objects[bucket+"/"+k]))})
		s.mu.Unlock()
	}
	return objects, err
}

func (s sizedStore) ListIncomplete(context.Context, string, string) ([]string, error) {
	return s.incomplete, nil
}

// s11InterruptedFixture plays, after S11 complete, an Operator that forms
// the standalone cluster and takes its backup, a member whose upload passes
// through its network namespace, and the object store. While the backup is
// Uploading, every reading puts uploadPerRead bytes through the limit; the
// first reading after the reset rule is installed has the upload's packets
// rejected, and the next ends the backup as afterCut, Failed by default with
// the Instance Manager's reset error.
type s11InterruptedFixture struct {
	*s11Fixture
	net *fakeNetns
	// readyAt is the reading of the cluster that first sees it Ready.
	readyAt int
	// uploadPerRead is what the upload puts through the limit at a reading;
	// completeAfter ends the backup Completed at that many Uploading readings
	// before the cut, 0 never; failRunning ends it Failed at its first
	// Running reading.
	uploadPerRead uint64
	completeAfter int
	failRunning   bool
	// afterCut is the phase the backup ends in after the cut; holdAfterCut
	// keeps it Uploading instead.
	afterCut     databasev1alpha1.CubridBackupPhase
	holdAfterCut bool
	// completeOnCut ends the backup Completed at the first reading after the
	// cut, at which the reset rule first rejects the upload's packets;
	// reDriven, when set, is the phase of every reading after the backup
	// ended, which otherwise keeps the phase it ended in.
	completeOnCut bool
	reDriven      databasev1alpha1.CubridBackupPhase
	// leave are the objects the member leaves under the attempt prefix when
	// the backup ends; tamper changes the status of a reading.
	leave  []string
	tamper func(s *databasev1alpha1.CubridBackupStatus)
	// incomplete are the multipart uploads the store lists at the end.
	incomplete []string
	// deleteErr answers the deletion of an object of the kind named.
	deleteErr map[string]error

	cluster             *databasev1alpha1.CubridCluster
	backup              *databasev1alpha1.CubridBackup
	clusterReads, reads int
	uploading           int
	cutSeen             bool
	final               databasev1alpha1.CubridBackupPhase
	deleted             []string
}

// newS11InterruptedFixture is the fixture over the object store objects
// store: by default the headless Service with one ready endpoint.
func newS11InterruptedFixture(t *testing.T, cfg HAConfig, store ...client.Object) *s11InterruptedFixture {
	t.Helper()
	if len(store) == 0 {
		store = []client.Object{testStoreServiceObject(), testStoreSlice("seaweedfs-a", testStoreIP)}
	}
	f := &s11InterruptedFixture{s11Fixture: newS11Fixture(t, cfg, store...), net: &fakeNetns{},
		readyAt: 2, uploadPerRead: testUploadPerRead, afterCut: databasev1alpha1.BackupPhaseFailed,
		deleteErr: map[string]error{}}
	f.runner.openStore = func(endpoint, accessKey, secretKey string, secure bool) (objectStore, error) {
		if endpoint != testForwarded || accessKey != testAccessKey || secretKey != testSecretKey || secure {
			t.Errorf("object store client for %s, secure %t, without the seed Secret's keys", endpoint, secure)
		}
		return sizedStore{fakeStore: f.store, incomplete: f.incomplete}, nil
	}
	f.netfault = f.net.answer
	f.ephemeralAdded = f.addEphemeral
	f.interruptedCreated = f.createCluster
	f.interruptedRead = f.advanceCluster
	created, read := f.backupCreated, f.backupRead
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		if backup.Name != s11InterruptedBackupName {
			return created(ctx, c, backup)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		backup.UID = testInterruptedUID
		f.backup = backup.DeepCopy()
		return nil
	}
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name != s11InterruptedBackupName {
			return read(ctx, c, key)
		}
		return f.advanceBackup(ctx, c, key)
	}
	f.backupDeleted = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup,
		opts ...client.DeleteOption) error {
		return f.deleteOwned(ctx, c, "CubridBackup", backup, opts...)
	}
	f.clusterDeleted = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster,
		opts ...client.DeleteOption) error {
		return f.deleteOwned(ctx, c, testKindCluster, cluster, opts...)
	}
	f.markersOf = func(string) []string { return nil }
	return f
}

func (f *s11InterruptedFixture) deleteOwned(ctx context.Context, c client.WithWatch, kind string, obj client.Object,
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

// createCluster stores the standalone cluster and its member's Pod, owned by
// the cluster's StatefulSet, with its database container running, as the
// Operator would.
func (f *s11InterruptedFixture) createCluster(ctx context.Context, c client.WithWatch,
	cluster *databasev1alpha1.CubridCluster) error {
	cluster.UID, cluster.Generation = testInterruptedClusterUID, 1
	f.mu.Lock()
	f.cluster = cluster.DeepCopy()
	f.mu.Unlock()
	if err := c.Create(ctx, cluster); err != nil {
		return err
	}
	return c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name + "-0",
		Namespace: f.cfg.Namespace, UID: testInterruptedPodUID,
		OwnerReferences: []metav1.OwnerReference{owner("StatefulSet", cluster.Name, "interrupted-sts")}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: databaseContainer}}},
		Status: corev1.PodStatus{PodIP: testInterruptedPodIP, HostIP: "172.31.0.5",
			ContainerStatuses: []corev1.ContainerStatus{{Name: databaseContainer, Ready: true,
				ContainerID: testIMContainerID, State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}}})
}

// addEphemeral adds the ephemeral container to the Pod and starts it.
func (f *s11InterruptedFixture) addEphemeral(ctx context.Context, c client.Client, pod *corev1.Pod) error {
	stored := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), stored); err != nil {
		return err
	}
	stored.Spec.EphemeralContainers = pod.Spec.EphemeralContainers
	if err := c.Update(ctx, stored); err != nil {
		return err
	}
	stored.Status.EphemeralContainerStatuses = append(stored.Status.EphemeralContainerStatuses,
		corev1.ContainerStatus{Name: netfault.Container,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
	return c.Status().Update(ctx, stored)
}

// advanceCluster writes the status of the standalone cluster, Ready from
// the readyAt-th reading.
func (f *s11InterruptedFixture) advanceCluster(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
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
	return c.Update(ctx, cluster)
}

// phase is the backup's phase at this reading, as the member's upload and
// the faults make it.
func (f *s11InterruptedFixture) phase() databasev1alpha1.CubridBackupPhase {
	if f.final != "" && f.reDriven != "" {
		return f.reDriven
	}
	if f.final != "" {
		return f.final
	}
	p := f.next()
	if p == databasev1alpha1.BackupPhaseCompleted || p == databasev1alpha1.BackupPhaseFailed {
		f.final = p
	}
	return p
}

// next is the phase at this reading of a backup that has not ended.
func (f *s11InterruptedFixture) next() databasev1alpha1.CubridBackupPhase {
	f.reads++
	switch f.reads {
	case 1:
		return databasev1alpha1.BackupPhasePending
	case 2:
		if f.failRunning {
			return databasev1alpha1.BackupPhaseFailed
		}
		return databasev1alpha1.BackupPhaseRunning
	}
	n := f.net
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.resetIP != "" {
		n.send(n.resetIP, 7300)
		if f.completeOnCut {
			return databasev1alpha1.BackupPhaseCompleted
		}
		if !f.cutSeen || f.holdAfterCut {
			f.cutSeen = true
			return databasev1alpha1.BackupPhaseUploading
		}
		return f.afterCut
	}
	if n.throttleIP != "" {
		n.send(n.throttleIP, f.uploadPerRead)
	}
	f.uploading++
	if f.completeAfter > 0 && f.uploading >= f.completeAfter {
		return databasev1alpha1.BackupPhaseCompleted
	}
	return databasev1alpha1.BackupPhaseUploading
}

// advanceBackup writes the status of the backup's phase at this reading.
func (f *s11InterruptedFixture) advanceBackup(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	backup := &databasev1alpha1.CubridBackup{}
	if err := c.Get(ctx, key, backup); err != nil {
		return nil
	}
	f.mu.Lock()
	phase := f.phase()
	tamper := f.tamper
	f.mu.Unlock()
	started := metav1.NewTime(time.Now().Add(-time.Second))
	dest := backup.Spec.Destination.ObjectStorage
	attempt := path.Join(dest.Prefix, testAttempt)
	s := &backup.Status
	s.Phase, s.ClusterUID, s.StartedAt = phase, testInterruptedClusterUID, &started
	s.Conditions = []metav1.Condition{{Type: backupReady, Status: metav1.ConditionFalse,
		Reason: testRunningReason, Message: testInProgress, LastTransitionTime: started}}
	if phase != databasev1alpha1.BackupPhasePending {
		s.TargetInstance, s.TargetRole = s11InterruptedMember, string(databasev1alpha1.RoleUnknown)
	}
	switch phase {
	case databasev1alpha1.BackupPhaseUploading:
		s.Conditions[0].Reason, s.Conditions[0].Message = testUploadRunning, testUploadingMessage
	case databasev1alpha1.BackupPhaseFailed:
		s.CompletedAt = &started
		s.Conditions[0].Reason, s.Conditions[0].Message = s11MissingReason, "upload failed: put "+dest.Bucket+"/"+
			attempt+"/"+testFirstBackupKey+": "+testResetAnswer
		if f.failRunning {
			s.Conditions[0].Message = testBackupdbFailed
		}
		for _, key := range f.leave {
			f.store.put(dest.Bucket, attempt+"/"+key, []byte("partial"))
		}
	case databasev1alpha1.BackupPhaseCompleted:
		s.CompletedAt = &started
		s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: "s3://" + dest.Bucket + "/" + attempt +
			"/manifest.json", ManifestDigest: strings.Repeat("a", 64), SizeBytes: 1, Database: databaseName,
			Level: databasev1alpha1.CubridBackupFull}
		s.Conditions[0].Status, s.Conditions[0].Reason, s.Conditions[0].Message = metav1.ConditionTrue,
			testCompleted, testCompletedMessage
		f.store.put(dest.Bucket, attempt+"/"+testFirstBackupKey, []byte("v"))
		f.store.put(dest.Bucket, attempt+"/manifest.json", []byte("{}"))
	}
	if tamper != nil {
		tamper(s)
	}
	return c.Update(ctx, backup)
}

// interruptedOf runs the fixture and returns S11 interrupted-upload,
// requiring LAB01 and S11 complete passed.
func interruptedOf(t *testing.T, f *s11InterruptedFixture) (evidence.Scenario, error) {
	t.Helper()
	err := f.runner.RunHA(t.Context(), f.cfg)
	scenarios := interruptedScenarios(t, f.cfg.EvidenceDir)
	for _, s := range scenarios[:2] {
		if s.Result != evidence.Pass {
			t.Fatalf("%s = %+v; want LAB01 and S11 complete passed", s.Name(), s)
		}
	}
	requireOwnEvidence(t, scenarios[2], "S11/interrupted-upload")
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	f.requireForwardStopped(t)
	return scenarios[2], err
}

func interruptedScenarios(t *testing.T, dir string) []evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s11InterruptedLane || len(summary.Scenarios) != 3 || summary.Scenarios[2].ID != s11ID ||
		summary.Scenarios[2].Variant != s11InterruptedVariant {
		t.Fatalf("summary = %+v, want LAB01, S11/complete and S11/interrupted-upload in lane %s", summary,
			s11InterruptedLane)
	}
	return summary.Scenarios
}

func readInterruptedRecord(t *testing.T, dir string) s11InterruptedRecord {
	t.Helper()
	var rec s11InterruptedRecord
	data, err := os.ReadFile(filepath.Join(dir, s11InterruptedRecordFile))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("S11 interrupted-upload record = %s, %v", data, err)
	}
	return rec
}

// wantInterruptedDeleted are the deletions of a run that created both
// objects.
var wantInterruptedDeleted = []string{"CubridBackup/" + s11InterruptedBackupName + "/" + testInterruptedUID,
	testKindCluster + "/" + s11InterruptedClusterName + "/" + testInterruptedClusterUID}

// requireFaultsRemoved fails unless the member's namespace holds neither
// fault.
func (f *s11InterruptedFixture) requireFaultsRemoved(t *testing.T) {
	t.Helper()
	f.net.mu.Lock()
	defer f.net.mu.Unlock()
	if f.net.throttleIP != "" || f.net.resetIP != "" {
		t.Fatalf("faults left in %s: limit %q, reset %q", s11InterruptedMember, f.net.throttleIP, f.net.resetIP)
	}
}

func TestRunS11InterruptedUploadPassesWhenTheCutUploadFails(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	interrupted, err := interruptedOf(t, f)
	if err != nil || interrupted.Result != evidence.Pass {
		t.Fatalf("S11 interrupted-upload = %+v, RunHA() error = %v; want a pass", interrupted, err)
	}
	if !readSummary(t, cfg.EvidenceDir).Passed {
		t.Fatal("summary not passed")
	}
	if !strings.HasPrefix(strings.Join(f.writes, ","),
		"namespace,secret,cluster,client,backup,interrupted-cluster,ephemeral-container,backup") {
		t.Fatalf("writes = %v", f.writes)
	}
	if interrupted.CleanupSucceeded == nil || !*interrupted.CleanupSucceeded ||
		!slices.Equal(f.deleted, wantInterruptedDeleted) {
		t.Fatalf("cleanup = %v, deleted = %v; want the faults removed and both objects deleted by UID",
			interrupted.CleanupSucceeded, f.deleted)
	}
	f.requireFaultsRemoved(t)
	if m := interrupted.Measurements; m["failedWithin"] == evidence.Unknown ||
		m["uploadBytesBeforeCut"] != float64(2*testUploadPerRead) || m["cut.rejectedPackets"] == evidence.Unknown ||
		m["rateLimit.configuredRateBitsPerSecond"] != float64(interruptedRate) {
		t.Fatalf("measurements = %v", m)
	}
	if len(f.store.puts) != 0 || len(f.store.removes) != 0 {
		t.Fatalf("the run wrote %v and removed %v in the object store; want nothing", f.store.puts, f.store.removes)
	}
	// The namespace enforced privileged, with warn and audit at restricted.
	lab01 := readHARecord(t, cfg.EvidenceDir)
	want := map[string]string{podSecurityEnforce: podSecurityPrivileged, podSecurityWarn: podSecurityRestricted,
		podSecurityAudit: podSecurityRestricted}
	rec := readInterruptedRecord(t, cfg.EvidenceDir)
	if fmt.Sprint(lab01.PodSecurity) != fmt.Sprint(want) || fmt.Sprint(rec.PodSecurity) != fmt.Sprint(want) {
		t.Fatalf("Pod Security = %v and %v, want %v", lab01.PodSecurity, rec.PodSecurity, want)
	}
	requireInterruptedRecord(t, f, rec, lab01)
	requireNamespaceGone(t, f.haFixture)
}

// requireInterruptedRecord checks the record of a passed run.
func requireInterruptedRecord(t *testing.T, f *s11InterruptedFixture, rec s11InterruptedRecord, lab01 haRecord) {
	t.Helper()
	requireInterruptedObjects(t, f, rec, lab01)
	requireInterruptedFaults(t, rec)
	requireInterruptedEnd(t, rec)
}

// requireInterruptedObjects checks the cluster and the backup the run
// created.
func requireInterruptedObjects(t *testing.T, f *s11InterruptedFixture, rec s11InterruptedRecord, lab01 haRecord) {
	t.Helper()
	if c := rec.Cluster; c == nil || c.Name != s11InterruptedClusterName || c.UID != testInterruptedClusterUID ||
		c.Secret != seedSecretName || c.ReadyWithin == "" {
		t.Fatalf("cluster = %+v, want it created with the run's Secret and Ready", c)
	}
	if got := f.cluster; got == nil || got.Spec.HighAvailability.Enabled || got.Spec.Topology.PromotableMembers != 1 ||
		got.Spec.ObjectStorage.CredentialsSecretRef.Name != seedSecretName || got.Spec.ObjectStorage.Bucket != "" {
		t.Fatalf("cluster spec = %+v, want one standalone member with the run's Secret", got)
	}
	if dest := f.backup.Spec.Destination.ObjectStorage; dest.Bucket != testBackupBucket ||
		dest.Prefix != "lab-runs/"+lab01.RunID+"/interrupted-upload" || f.backup.Spec.ClusterRef.Name !=
		s11InterruptedClusterName {
		t.Fatalf("backup = %+v, want the standalone cluster to the backup bucket under the variant's prefix",
			f.backup.Spec)
	}
}

// requireInterruptedAddress checks the target and the store's address in the
// record of a passed run.
func requireInterruptedAddress(t *testing.T, fs *s11InterruptedFaults) {
	t.Helper()
	if fs == nil || fs.Target != s11InterruptedMember || fs.TargetUID != testInterruptedPodUID ||
		fs.TargetIP != testInterruptedPodIP || fs.StoreIP != testStoreIP || fs.StorePort != testStorePort ||
		fs.AddressMode != addressHeadless || fs.StorePod != "seaweedfs-0" || fs.StoreCheckedBeforeCut == nil ||
		fs.StoreCheckedAtEnd == nil {
		t.Fatalf("faults = %+v, want the store's headless endpoint, read again before the cut and at the end", fs)
	}
}

// requireInterruptedFaults checks the faults and the failure in the record
// of a passed run.
func requireInterruptedFaults(t *testing.T, rec s11InterruptedRecord) {
	t.Helper()
	requireInterruptedAddress(t, rec.Faults)
	fs := rec.Faults
	if len(fs.ExecErrors) != 0 || fs.RejectedAtEnd != nil ||
		fs.LimitedAt == nil || fs.LimitConfirmedAt == nil || fs.AtRequest == nil || fs.AtCut == nil ||
		fs.CutIssuedAt == nil || fs.CutAt == nil || fs.CutConfirmedAt == nil || fs.Rejected == nil ||
		fs.Rejected.Packets == 0 || fs.SentBeforeCut != 2*testUploadPerRead || fs.PhaseAtCut != phaseUploading ||
		fs.RemovedAt == nil || fs.CutConfirmedAt.Before(*fs.CutAt) || fs.CutAt.Before(*rec.RequestedAt) {
		t.Fatalf("faults = %+v, want the limit, the upload seen, the cut confirmed and both removed", fs)
	}
	if rec.FailedWithin == "" || rec.FailedAfterCut == "" || rec.StoreError != testResetAnswer ||
		rec.StoreErrorKind != storeErrorReset || !rec.EndedAfterCut || rec.LastPhase != databasePhaseFailed ||
		rec.PhaseAtEnd != databasePhaseFailed || rec.ArtifactAtEnd != nil ||
		!strings.HasPrefix(rec.Failure, s11MissingReason+": upload failed: ") ||
		rec.Artifact != nil || rec.CompletedBeforeCut || len(rec.ArtifactsNotDone) != 0 {
		t.Fatalf("record = %+v, want the upload's failure", rec)
	}
}

// requireInterruptedEnd checks the checks at the end, the cleanup and the
// notes in the record of a passed run.
func requireInterruptedEnd(t *testing.T, rec s11InterruptedRecord) {
	t.Helper()
	if rec.CheckedAtEnd == nil || len(rec.ObjectsAtEnd) != 0 || rec.ManifestAtEnd || rec.PartialArtifact ||
		!rec.IncompleteListed {
		t.Fatalf("record = %+v, want the prefix listed empty at the end", rec)
	}
	if rec.Source == nil || !rec.Source.DataRulesHeld || rec.ManagerAfter == nil || rec.Credentials == nil ||
		len(rec.Credentials.FoundIn) != 0 || rec.JudgedAt == nil {
		t.Fatalf("record = %+v, want the source, the manager and the credentials checked", rec)
	}
	for _, want := range []string{"PVC", "ephemeral container", "staging directory"} {
		if !strings.Contains(rec.Residue, want) {
			t.Fatalf("residue = %q, want %s named", rec.Residue, want)
		}
	}
	if !strings.Contains(rec.Cleanup, "the cut is not installed or was seen removed") ||
		!strings.Contains(rec.Cleanup, s11InterruptedClusterName) {
		t.Fatalf("cleanup = %q, want the faults' removal and the deletions", rec.Cleanup)
	}
	if !slices.ContainsFunc(rec.Notes, func(n string) bool {
		return strings.Contains(n, "never saw the connection close")
	}) {
		t.Fatalf("notes = %v, want the local reset named", rec.Notes)
	}
	if !slices.Contains(rec.Notes, logsNote(defaultMaxLogBytes)) {
		t.Fatalf("notes = %v, want both logs named as searched", rec.Notes)
	}
	requireLogsSearched(t, rec.Credentials, s11InterruptedMember)
}

// A partial object left under the prefix is recorded, not failed, and never
// taken for a completed backup.
func TestRunS11InterruptedUploadRecordsAPartialArtifact(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.leave = []string{testFirstBackupKey}
	f.incomplete = []string{"x/backup/appdb_bk0v000 (upload 1)"}
	interrupted, err := interruptedOf(t, f)
	if err != nil || interrupted.Result != evidence.Pass {
		t.Fatalf("S11 interrupted-upload = %+v, %v; want a pass with the partial artifact recorded", interrupted, err)
	}
	rec := readInterruptedRecord(t, cfg.EvidenceDir)
	if !rec.PartialArtifact || rec.ManifestAtEnd || len(rec.ObjectsAtEnd) != 1 || rec.ObjectsAtEnd[0].Size != 7 ||
		!strings.HasSuffix(rec.ObjectsAtEnd[0].Key, testFirstBackupKey) || len(rec.IncompleteUploads) != 1 ||
		!strings.Contains(rec.Residue, "a partial artifact") {
		t.Fatalf("record = %+v, want the partial object and the incomplete upload recorded", rec)
	}
}

func TestRunS11InterruptedUploadFailsOnANeverEvent(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(f *s11InterruptedFixture)
		want []string
	}{
		"Completed after the cut": {set: func(f *s11InterruptedFixture) {
			f.afterCut = databasev1alpha1.BackupPhaseCompleted
		}, want: []string{"ended Completed after its upload was cut", testArtifactField, "manifest.json"}},
		"an artifact on an Uploading backup": {set: func(f *s11InterruptedFixture) {
			f.tamper = func(s *databasev1alpha1.CubridBackupStatus) {
				if s.Phase == databasev1alpha1.BackupPhaseUploading {
					s.Artifact = &databasev1alpha1.CubridBackupArtifact{URI: testElsewhereURI}
				}
			}
		}, want: []string{"status.artifact names s3://elsewhere/manifest.json", "in phase Uploading"}},
		"a manifest under the prefix": {set: func(f *s11InterruptedFixture) {
			f.leave = []string{testFirstBackupKey, "manifest.json"}
		}, want: []string{"holds a manifest.json"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11InterruptedConfig(t)
			f := newS11InterruptedFixture(t, cfg)
			tc.set(f)
			interrupted, err := interruptedOf(t, f)
			if err == nil || interrupted.Result != evidence.Fail || len(interrupted.NeverEvents) == 0 {
				t.Fatalf("S11 interrupted-upload = %+v, want a never-event", interrupted)
			}
			for _, want := range tc.want {
				if !strings.Contains(interrupted.Reason, want) {
					t.Fatalf("reason = %q, want %q", interrupted.Reason, want)
				}
			}
			f.requireFaultsRemoved(t)
			if !slices.Equal(f.deleted, wantInterruptedDeleted) {
				t.Fatalf("deleted = %v", f.deleted)
			}
		})
	}
}

func TestRunS11InterruptedUploadIsBlockedWhenTheCutIsNotShown(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(f *s11InterruptedFixture)
		want string
	}{
		// The upload finished before enough of it was seen: a complete
		// backup, not an interrupted one.
		"the upload finished before the cut": {set: func(f *s11InterruptedFixture) {
			f.uploadPerRead, f.completeAfter = 100<<10, 2
		}, want: "inconclusive"},
		"the backup failed before its upload was seen": {set: func(f *s11InterruptedFixture) {
			f.failRunning = true
		}, want: "before its upload was seen under way"},
		"the reset rule rejected nothing": {set: func(f *s11InterruptedFixture) {
			f.net.inertReset = true
		}, want: "the cut was not confirmed"},
		"the cut was not confirmed in time": {set: func(f *s11InterruptedFixture) {
			f.net.inertReset, f.holdAfterCut = true, true
			old := interruptedCutConfirmLimit
			interruptedCutConfirmLimit = 50 * time.Millisecond
			t.Cleanup(func() { interruptedCutConfirmLimit = old })
		}, want: "was not confirmed within"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11InterruptedConfig(t)
			f := newS11InterruptedFixture(t, cfg)
			tc.set(f)
			interrupted, err := interruptedOf(t, f)
			if err == nil || interrupted.Result != evidence.Blocked || len(interrupted.NeverEvents) != 0 ||
				!strings.Contains(interrupted.Reason, tc.want) {
				t.Fatalf("S11 interrupted-upload = %+v, want blocked: %q", interrupted, tc.want)
			}
			f.requireFaultsRemoved(t)
			if !slices.Equal(f.deleted, wantInterruptedDeleted) {
				t.Fatalf("deleted = %v", f.deleted)
			}
		})
	}
}

// A complete backup made before the cut keeps its manifest: it is not a
// never-event, and the record says the artifact is complete.
func TestRunS11InterruptedUploadKeepsACompleteBackupApart(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.uploadPerRead, f.completeAfter = 100<<10, 2
	if _, err := interruptedOf(t, f); err == nil {
		t.Fatal("RunHA() passed")
	}
	rec := readInterruptedRecord(t, cfg.EvidenceDir)
	if !rec.CompletedBeforeCut || !rec.ManifestAtEnd || rec.PartialArtifact || rec.Faults.CutAt != nil ||
		!strings.Contains(rec.Residue, "an artifact with a manifest.json") {
		t.Fatalf("record = %+v, want the complete backup recorded as complete", rec)
	}
}

func TestRunS11InterruptedUploadFailsWhenTheCutBackupFailsForAnotherReason(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.tamper = func(s *databasev1alpha1.CubridBackupStatus) {
		if s.Phase == databasev1alpha1.BackupPhaseFailed {
			s.Conditions[0].Reason, s.Conditions[0].Message = "RecordedOperationLost", "operation record lost"
		}
	}
	interrupted, err := interruptedOf(t, f)
	if err == nil || interrupted.Result != evidence.Fail || !strings.Contains(interrupted.Reason, "not for its upload") {
		t.Fatalf("S11 interrupted-upload = %+v, want a failure", interrupted)
	}
}

// The variant is refused before anything is created when the run namespace
// does not enforce privileged: the faults' container would not be admitted.
func TestRunS11InterruptedUploadRefusesANamespaceThatIsNotPrivileged(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	created := f.backupCreated
	f.backupCreated = func(ctx context.Context, c client.WithWatch, backup *databasev1alpha1.CubridBackup) error {
		ns := &corev1.Namespace{}
		if err := c.Get(ctx, client.ObjectKey{Name: cfg.Namespace}, ns); err != nil {
			return err
		}
		ns.Labels[podSecurityEnforce] = podSecurityRestricted
		if err := c.Update(ctx, ns); err != nil {
			return err
		}
		return created(ctx, c, backup)
	}
	interrupted, err := interruptedOf(t, f)
	if err == nil || interrupted.Result != evidence.Blocked || !strings.Contains(interrupted.Reason,
		`enforces Pod Security "restricted", not privileged`) {
		t.Fatalf("S11 interrupted-upload = %+v, want refused for the namespace", interrupted)
	}
	if f.cluster != nil || f.backup != nil || len(f.net.ops) != 0 || len(f.deleted) != 0 {
		t.Fatalf("cluster %v, backup %v, fault commands %v, deletions %v; want nothing done", f.cluster, f.backup,
			f.net.ops, f.deleted)
	}
}

func TestRunS11InterruptedUploadRefusesAStoreItCannotAddress(t *testing.T) {
	clusterIP := func(change func(svc *corev1.Service)) []client.Object {
		svc := testStoreServiceObject()
		svc.Spec.ClusterIP = testStoreClusterIP
		change(svc)
		return []client.Object{svc}
	}
	for name, tc := range map[string]struct {
		store []client.Object
		want  string
	}{
		"a headless Service with no ready endpoint": {store: []client.Object{testStoreServiceObject(),
			testStoreSlice("seaweedfs-a")}, want: "has 0 ready IPv4 endpoint addresses"},
		"a headless Service with two ready endpoints": {store: []client.Object{testStoreServiceObject(),
			testStoreSlice("seaweedfs-a", testStoreIP),
			testStoreSlice("seaweedfs-b", "10.42.2.9")}, want: "has 2 ready IPv4 endpoint addresses"},
		"a headless Service whose endpoints lack its port": {store: []client.Object{testStoreServiceObject(),
			func() client.Object {
				slice := testStoreSlice("seaweedfs-a", testStoreIP)
				other := "metrics"
				slice.Ports[0].Name = &other
				return slice
			}()}, want: "has 0 ready IPv4 endpoint addresses"},
		"a headless Service whose endpoint port differs from its port": {store: []client.Object{
			testStoreServiceObject(), func() client.Object {
				slice := testStoreSlice("seaweedfs-a", testStoreIP)
				other := int32(8888)
				slice.Ports[0].Port = &other
				return slice
			}()}, want: "maps its port 8333 to endpoint port 8888"},
		"another port": {store: clusterIP(func(svc *corev1.Service) { svc.Spec.Ports[0].Port = 9000 }),
			want: "has no TCP port 8333"},
		"a UDP port of the same": {store: clusterIP(func(svc *corev1.Service) {
			svc.Spec.Ports[0].Protocol = corev1.ProtocolUDP
		}), want: "has no TCP port 8333"},
		"an IPv6 ClusterIP": {store: clusterIP(func(svc *corev1.Service) { svc.Spec.ClusterIP = "fd00::10" }),
			want: "neither an IPv4 address nor None"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11InterruptedConfig(t)
			f := newS11InterruptedFixture(t, cfg, tc.store...)
			interrupted, err := interruptedOf(t, f)
			if err == nil || interrupted.Result != evidence.Blocked || f.cluster != nil ||
				!strings.Contains(interrupted.Reason, tc.want) {
				t.Fatalf("S11 interrupted-upload = %+v, cluster %v; want refused before anything was created: %q",
					interrupted, f.cluster, tc.want)
			}
		})
	}
}

// A Service that is not headless is addressed by its ClusterIP.
func TestRunS11InterruptedUploadTargetsTheClusterIPOfAServiceThatIsNotHeadless(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	svc := testStoreServiceObject()
	svc.Spec.ClusterIP = testStoreClusterIP
	f := newS11InterruptedFixture(t, cfg, svc)
	interrupted, err := interruptedOf(t, f)
	if err != nil || interrupted.Result != evidence.Pass {
		t.Fatalf("S11 interrupted-upload = %+v, %v; want a pass", interrupted, err)
	}
	fs := readInterruptedRecord(t, cfg.EvidenceDir).Faults
	if fs.AddressMode != addressClusterIP || fs.StoreIP != testStoreClusterIP || fs.StorePort != testStorePort ||
		fs.StorePod != "" {
		t.Fatalf("faults = %+v, want the ClusterIP and the Service's port", fs)
	}
	f.net.mu.Lock()
	defer f.net.mu.Unlock()
	if !slices.Contains(f.net.ops, "throttle") || f.net.rejected == 0 {
		t.Fatalf("fault commands %v, rejected %d; want the faults on the ClusterIP", f.net.ops, f.net.rejected)
	}
}

// The store Pod that moved before the cut would leave the cut on an address
// nothing is sent to.
func TestRunS11InterruptedUploadIsBlockedWhenTheStoreMovesBeforeTheCut(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	read := f.backupRead
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name == s11InterruptedBackupName && f.reads == 2 {
			slice := &discoveryv1.EndpointSlice{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: testStoreNamespace, Name: "seaweedfs-a"},
				slice); err != nil {
				return err
			}
			slice.Endpoints[1].Addresses = []string{"10.42.2.9"}
			if err := c.Update(ctx, slice); err != nil {
				return err
			}
		}
		return read(ctx, c, key)
	}
	interrupted, err := interruptedOf(t, f)
	if err == nil || interrupted.Result != evidence.Blocked || len(interrupted.NeverEvents) != 0 ||
		!strings.Contains(interrupted.Reason, "before the cut: the object store's address is now 10.42.2.9:8333") {
		t.Fatalf("S11 interrupted-upload = %+v, want blocked: the store moved", interrupted)
	}
	f.net.mu.Lock()
	cut := slices.Contains(f.net.ops, "reset")
	f.net.mu.Unlock()
	if cut {
		t.Fatal("the cut was installed on the old address")
	}
	if rec := readInterruptedRecord(t, cfg.EvidenceDir); rec.Faults.CutAt != nil ||
		rec.Faults.StoreCheckedBeforeCut != nil {
		t.Fatalf("faults = %+v, want no cut", rec.Faults)
	}
}

// A fault not seen removed blocks the variant: the member's traffic may
// still be cut.
func TestRunS11InterruptedUploadIsBlockedByAFaultLeftInstalled(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.net.ignoreUnreset = true
	interrupted, err := interruptedOf(t, f)
	if err == nil || interrupted.Result != evidence.Blocked ||
		!strings.Contains(interrupted.Reason, "the faults were not seen removed: the cut was not seen removed") {
		t.Fatalf("S11 interrupted-upload = %+v, %v; want blocked by the fault left", interrupted, err)
	}
	if interrupted.CleanupSucceeded == nil || *interrupted.CleanupSucceeded {
		t.Fatalf("S11 interrupted-upload = %+v, want the failed removal recorded", interrupted)
	}
	rec := readInterruptedRecord(t, cfg.EvidenceDir)
	if !strings.Contains(rec.Faults.Removal, "the cut was not seen removed") || rec.Faults.RemovedAt != nil ||
		!strings.Contains(rec.Cleanup, "the cut was not seen removed") {
		t.Fatalf("faults = %+v, cleanup %q; want the cut left named", rec.Faults, rec.Cleanup)
	}
	// The cluster is deleted all the same.
	if !slices.Equal(f.deleted, wantInterruptedDeleted) {
		t.Fatalf("deleted = %v", f.deleted)
	}
}

// When the wait ends before the backup ended, the upload may go on once the
// faults are removed: a manifest.json found then is recorded, not a
// never-event.
func TestRunS11InterruptedUploadIsBlockedByAManifestAfterTheWaitEndedEarly(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	// The limit's counters are read twice by its confirmation and once at
	// the request; every reading from the second Uploading one fails.
	f.net.failFrom, f.net.failCount = 5, 100
	f.net.onUnthrottle = func() {
		f.store.put(testBackupBucket, f.backup.Spec.Destination.ObjectStorage.Prefix+"/"+testAttempt+
			"/manifest.json", []byte("{}"))
	}
	interrupted, err := interruptedOf(t, f)
	if err == nil || interrupted.Result != evidence.Blocked || len(interrupted.NeverEvents) != 0 ||
		!strings.Contains(interrupted.Reason, "4 errors in a row") ||
		!strings.Contains(interrupted.Reason, "did not see the backup end after a confirmed cut") {
		t.Fatalf("S11 interrupted-upload = %+v, want blocked with the manifest recorded", interrupted)
	}
	rec := readInterruptedRecord(t, cfg.EvidenceDir)
	if !rec.ManifestAtEnd || rec.EndedAfterCut || rec.LastPhase != phaseUploading || len(rec.Faults.ExecErrors) != 4 {
		t.Fatalf("record = %+v, want the manifest, the last phase and the errors recorded", rec)
	}
}

// A few errors of a fault's command in a row are tolerated.
func TestRunS11InterruptedUploadToleratesAFewCommandErrors(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.net.failFrom, f.net.failCount = 4, interruptedExecTolerance
	interrupted, err := interruptedOf(t, f)
	if err != nil || interrupted.Result != evidence.Pass {
		t.Fatalf("S11 interrupted-upload = %+v, %v; want a pass", interrupted, err)
	}
	if rec := readInterruptedRecord(t, cfg.EvidenceDir); len(rec.Faults.ExecErrors) != interruptedExecTolerance {
		t.Fatalf("errors = %v, want the %d tolerated ones recorded", rec.Faults.ExecErrors, interruptedExecTolerance)
	}
}

// An address read that keeps failing before the cut is counted toward the
// limit, even though every counters read in between succeeds.
func TestRunS11InterruptedUploadCountsAStoreAddressThatKeepsFailing(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	read := f.backupRead
	added := false
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name == s11InterruptedBackupName && f.reads == 2 && !added {
			added = true
			if err := c.Create(ctx, testStoreSlice("seaweedfs-b", "10.42.2.9")); err != nil {
				return err
			}
		}
		return read(ctx, c, key)
	}
	interrupted, err := interruptedOf(t, f)
	if err == nil || interrupted.Result != evidence.Blocked || len(interrupted.NeverEvents) != 0 ||
		!strings.Contains(interrupted.Reason, "errors in a row") ||
		!strings.Contains(interrupted.Reason, "has 2 ready IPv4 endpoint addresses") {
		t.Fatalf("S11 interrupted-upload = %+v, want blocked by the store address read failing", interrupted)
	}
	f.net.mu.Lock()
	cut := slices.Contains(f.net.ops, "reset")
	f.net.mu.Unlock()
	if cut {
		t.Fatal("the cut was installed without a readable store address")
	}
}

// A target Pod that changed is never tolerated.
func TestRunS11InterruptedUploadStopsAtOnceWhenTheTargetPodChanges(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	read := f.backupRead
	f.backupRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name == s11InterruptedBackupName && f.reads == 2 {
			pod := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: s11InterruptedMember},
				pod); err != nil {
				return err
			}
			pod.Status.PodIP = "10.42.5.21"
			if err := c.Status().Update(ctx, pod); err != nil {
				return err
			}
		}
		return read(ctx, c, key)
	}
	interrupted, err := interruptedOf(t, f)
	if err == nil || interrupted.Result != evidence.Blocked || !strings.Contains(interrupted.Reason, "has IP") {
		t.Fatalf("S11 interrupted-upload = %+v, want blocked by the changed Pod", interrupted)
	}
	if rec := readInterruptedRecord(t, cfg.EvidenceDir); len(rec.Faults.ExecErrors) != 0 {
		t.Fatalf("errors = %v, want the changed Pod not tolerated", rec.Faults.ExecErrors)
	}
}

// A packet rejected at the reading that first sees the backup Completed may
// have been sent after the upload ended: inconclusive, not a never-event.
func TestRunS11InterruptedUploadDoesNotConfirmTheCutAtTheEndedReading(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.completeOnCut = true
	interrupted, err := interruptedOf(t, f)
	if err == nil || interrupted.Result != evidence.Blocked || len(interrupted.NeverEvents) != 0 ||
		!strings.Contains(interrupted.Reason, "inconclusive") {
		t.Fatalf("S11 interrupted-upload = %+v, want blocked as inconclusive", interrupted)
	}
	rec := readInterruptedRecord(t, cfg.EvidenceDir)
	if !rec.CompletedBeforeCut || rec.Faults.CutConfirmedAt != nil || rec.Faults.RejectedAtEnd == nil ||
		rec.EndedAfterCut {
		t.Fatalf("record = %+v, faults = %+v; want the late rejection kept as evidence only", rec, rec.Faults)
	}
}

// A backup driven on after it was seen Failed is found when it is read
// again at the end.
func TestRunS11InterruptedUploadFailsWhenTheBackupCompletesAfterTheWait(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.reDriven = databasev1alpha1.BackupPhaseCompleted
	interrupted, err := interruptedOf(t, f)
	if err == nil || interrupted.Result != evidence.Fail ||
		!strings.Contains(interrupted.Reason, "is Completed when read again at the end") ||
		!strings.Contains(interrupted.Reason, "read again at the end, after its upload was cut") {
		t.Fatalf("S11 interrupted-upload = %+v, want the backup at the end as never-events", interrupted)
	}
	if rec := readInterruptedRecord(t, cfg.EvidenceDir); rec.PhaseAtEnd != phaseCompleted || rec.ArtifactAtEnd == nil ||
		rec.LastPhase != databasePhaseFailed {
		t.Fatalf("record = %+v, want the backup at the end recorded", rec)
	}
}

func TestRunS11InterruptedUploadRecordsWhatItCouldNotDelete(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.deleteErr[testKindCluster] = apierrors.NewForbidden(
		databasev1alpha1.GroupVersion.WithResource("cubridclusters").GroupResource(), s11InterruptedClusterName,
		errors.New("forbidden"))
	interrupted, err := interruptedOf(t, f)
	if err != nil || interrupted.Result != evidence.Pass || interrupted.CleanupSucceeded == nil ||
		*interrupted.CleanupSucceeded {
		t.Fatalf("S11 interrupted-upload = %+v, %v; want a pass with the failed deletion recorded", interrupted, err)
	}
	if rec := readInterruptedRecord(t, cfg.EvidenceDir); !strings.Contains(rec.Cleanup, testForbidden) {
		t.Fatalf("cleanup = %q, want the cluster left named with the error", rec.Cleanup)
	}
}

func TestRunS11InterruptedUploadIsBlockedWhenS11CompleteDidNotPass(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.phases = []databasev1alpha1.CubridBackupPhase{databasev1alpha1.BackupPhaseFailed}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if interrupted := interruptedScenarios(t, cfg.EvidenceDir)[2]; interrupted.Result != evidence.Blocked ||
		!strings.Contains(interrupted.Reason, "S11 complete did not pass") {
		t.Fatalf("S11 interrupted-upload = %+v, want blocked by S11 complete", interrupted)
	}
	if f.cluster != nil || len(f.net.ops) != 0 {
		t.Fatal("S11 interrupted-upload ran although S11 complete did not pass")
	}
}

func TestRunHARefusesBackupInterruptedUploadWithoutItsLane(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(cfg *HAConfig)
		want   string
	}{
		testNoBucket: {change: func(cfg *HAConfig) { cfg.BackupBucket = "" },
			want: "needs -backup-bucket"},
		testWithRestore: {change: func(cfg *HAConfig) {
			cfg.Restore, cfg.RestoreLimit = true, time.Minute
		}, want: testRestoreFlag},
		"with -backup-missing-destination": {change: func(cfg *HAConfig) {
			cfg.BackupMissingDestination = true
		}, want: "-backup-missing-destination"},
		"with -backup-denied-credentials": {change: func(cfg *HAConfig) {
			cfg.BackupDeniedCredentials, cfg.BackupDeniedReadyLimit = true, time.Minute
		}, want: "-backup-denied-credentials"},
		"without a ready limit": {change: func(cfg *HAConfig) { cfg.BackupInterruptedReadyLimit = 0 },
			want: "-backup-interrupted-ready-limit"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11InterruptedConfig(t)
			tc.change(&cfg)
			f := newS11InterruptedFixture(t, testS11InterruptedConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "-backup-interrupted-upload") {
				t.Fatalf("RunHA() error = %v, want -backup-interrupted-upload refused %s", err, name)
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}

// Without the variant, the run namespace enforces restricted, as before.
func TestRunHAKeepsTheNamespaceRestrictedWithoutInterruptedUpload(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if got := readHARecord(t, cfg.EvidenceDir).PodSecurity; len(got) != 1 ||
		got[podSecurityEnforce] != podSecurityRestricted {
		t.Fatalf("Pod Security = %v, want enforce restricted only", got)
	}
}
