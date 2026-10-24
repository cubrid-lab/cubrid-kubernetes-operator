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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	testAccessKey  = "lab-access-key-value"
	testSecretKey  = "lab-secret-key-value"
	testHAPrimary  = "ha-1"
	testMemberLast = "ha-2"
	testMemberHA0  = "ha-0"
	testSeedSecret = "seed-credentials"
	testROOpID     = "ro-000001"
)

func testHAConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testConfig(t)
	cfg.Namespace = "cubrid-lab-ha"
	return HAConfig{Config: cfg,
		WorkloadImage: "example.com/workload-client@sha256:" + strings.Repeat("d", 64),
		SeedEndpoint:  "store.example:8333", SeedBucket: "seed", SeedInsecure: true,
		SeedSecretNamespace: "object-store", SeedSecret: testSeedSecret,
		Operations: 10, DataCheckWithin: 200 * time.Millisecond}
}

// haFixture plays the Operator, CUBRID and the workload client for an HA run.
type haFixture struct {
	t      *testing.T
	cfg    HAConfig
	runner Runner

	mu sync.Mutex
	// roles is what cubrid heartbeat status reports on each member.
	roles map[string]string
	// primary is the status.currentPrimary the Operator writes.
	primary string
	// history is what the workload client prints for its run.
	history string
	// rows are the ledger rows each member and Service returns; missing
	// holds rows a member lacks for its first missingReads reads.
	rows         []workload.Row
	missing      map[string]int
	missingReads int
	reads        map[string]int
	// roRefusal is the error the write through the read-only Service gets.
	roRefusal string
	// afterRun runs once the workload has run.
	afterRun func(context.Context)
	// initOutput and initErr are what workload init answers.
	initOutput string
	initErr    error
	// clientWaiting is the reason the client container waits, if any.
	clientWaiting string
	// bootstrapFailed marks the database's bootstrap Failed.
	bootstrapFailed bool
	// foreignStatefulSet gives the StatefulSet another owner.
	foreignStatefulSet bool
	// clientCreatedAt counts heartbeat readings before the client was created.
	heartbeats, heartbeatsBeforeClient int
	// managerDeleted is set when the manager Pod was deleted; replacement
	// says what the fake Operator does then.
	managerDeleted bool
	replacement    func(ctx context.Context, c client.WithWatch)
	// backgroundHistory returns what a background client recorded.
	backgroundHistory func(id string) string

	writes  []string
	deletes []string
	store   client.WithWatch
	cluster *databasev1alpha1.CubridCluster
	client  *corev1.Pod
	secret  *corev1.Secret
	ran     []string
}

func newHAFixture(t *testing.T, cfg HAConfig, extra ...client.Object) *haFixture {
	t.Helper()
	f := &haFixture{t: t, cfg: cfg, primary: testHAPrimary,
		roles:   map[string]string{testMemberHA0: roleSlave, testHAPrimary: roleMaster, testMemberLast: roleSlave},
		missing: map[string]int{}, reads: map[string]int{},
		roRefusal: "code -581: updates are disabled"}
	f.history, f.rows = fakeWorkload(cfg.Operations)
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSeedSecret, Namespace: cfg.SeedSecretNamespace},
		Data: map[string][]byte{seedAccessKey: []byte(testAccessKey), seedSecretKey: []byte(testSecretKey)}}
	hooks := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			f.store = c
			if obj.GetNamespace() != "" && obj.GetNamespace() != cfg.Namespace {
				t.Fatalf("runner wrote %T outside its namespace: %s", obj, obj.GetNamespace())
			}
			switch value := obj.(type) {
			case *corev1.Namespace:
				if value.Name != cfg.Namespace {
					t.Fatalf("runner wrote shared namespace %s", value.Name)
				}
				value.UID = testRunNamespaceUID
				f.writes = append(f.writes, "namespace")
				return c.Create(ctx, value, opts...)
			case *corev1.Secret:
				f.secret = value.DeepCopy()
				f.writes = append(f.writes, "secret")
				return c.Create(ctx, value, opts...)
			case *databasev1alpha1.CubridCluster:
				value.UID = "ha-cluster"
				value.Generation = 1
				f.cluster = value.DeepCopy()
				f.writes = append(f.writes, "cluster")
				if err := c.Create(ctx, value, opts...); err != nil {
					return err
				}
				return f.reconcile(ctx, c, value)
			case *corev1.Pod:
				if value.Name != clientPodName {
					t.Fatalf("runner created an unexpected Pod %s", value.Name)
				}
				value.UID = "client-pod"
				value.Spec.NodeName = "node-3"
				f.mu.Lock()
				f.heartbeatsBeforeClient = f.heartbeats
				f.mu.Unlock()
				value.Status = corev1.PodStatus{Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
					ContainerStatuses: []corev1.ContainerStatus{{Name: clientContainer, Ready: true,
						ContainerID: "containerd://client-container",
						State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						ImageID:     "example.com/workload-client@sha256:" + strings.Repeat("d", 64)}}}
				if f.clientWaiting != "" {
					value.Status.Conditions[0].Status = corev1.ConditionFalse
					value.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: clientContainer,
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: f.clientWaiting,
							Message: "back-off pulling image"}}}
				}
				f.client = value.DeepCopy()
				f.writes = append(f.writes, "client")
				return c.Create(ctx, value, opts...)
			default:
				t.Fatalf("runner attempted a forbidden write: %T", obj)
				return errors.New("forbidden write")
			}
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if pod, ok := obj.(*corev1.Pod); ok && pod.Namespace == cfg.OperatorNamespace {
				return f.deleteManager(ctx, c, pod, opts...)
			}
			ns, ok := obj.(*corev1.Namespace)
			if !ok || ns.Name != cfg.Namespace {
				t.Fatalf("runner deleted %T %s", obj, obj.GetName())
			}
			options := (&client.DeleteOptions{}).ApplyOptions(opts)
			if options.Preconditions == nil || options.Preconditions.UID == nil ||
				*options.Preconditions.UID != testRunNamespaceUID {
				t.Fatal("namespace deletion without its UID precondition")
			}
			f.deletes = append(f.deletes, ns.Name)
			return c.Delete(ctx, obj, opts...)
		},
	}
	f.runner = Runner{Client: testClient(t, cfg.Config, hooks, append([]client.Object{source}, extra...)...),
		Command: f.command}
	return f
}

// reconcile creates what the Operator would for the cluster, formed.
func (f *haFixture) reconcile(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
	cluster.Status.ObservedGeneration = 1
	cluster.Status.CurrentPrimary = f.primary
	if f.bootstrapFailed {
		cluster.Status.Databases = []databasev1alpha1.DatabaseStatus{{Name: databaseName,
			Phase: string(databasev1alpha1.BootstrapFailed)}}
		cluster.Status.Conditions = append(cluster.Status.Conditions, metav1.Condition{Type: "BootstrapReady",
			Status: metav1.ConditionFalse, Reason: "SeedBackupFailed", Message: "upload failed: connection refused"})
	}
	for _, condition := range startingConditions {
		cluster.Status.Conditions = append(cluster.Status.Conditions,
			metav1.Condition{Type: condition, Status: metav1.ConditionTrue, Reason: "Test"})
	}
	if err := c.Update(ctx, cluster); err != nil {
		return err
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: haClusterName, Namespace: f.cfg.Namespace,
		UID: "ha-sts", OwnerReferences: []metav1.OwnerReference{owner("CubridCluster", haClusterName, cluster.UID)}}}
	if f.foreignStatefulSet {
		sts.OwnerReferences = []metav1.OwnerReference{owner("CubridCluster", haClusterName, "someone-else")}
	}
	objects := []client.Object{sts}
	for i := range haMemberCount {
		name := fmt.Sprintf("%s-%d", haClusterName, i)
		objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.cfg.Namespace,
			UID: types.UID(name), OwnerReferences: []metav1.OwnerReference{owner("StatefulSet", haClusterName, sts.UID)}},
			Spec: corev1.PodSpec{NodeName: fmt.Sprintf("node-%d", i+1),
				Containers: []corev1.Container{{Name: databaseContainer, Image: f.cfg.InstanceManagerImage}}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: databaseContainer, Ready: true,
					ContainerID: "containerd://" + name,
					State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
					ImageID:     "containerd://sha256:" + strings.Repeat("c", 64)}}}})
	}
	for _, mode := range []string{"rw", "ro"} {
		objects = append(objects, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: haClusterName + "-broker-" + mode, Namespace: f.cfg.Namespace, UID: types.UID(mode),
			OwnerReferences: []metav1.OwnerReference{owner("CubridCluster", haClusterName, cluster.UID)}},
			Status: appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2}})
	}
	return createAll(ctx, c, objects...)
}

// fakeWorkload returns the history of a run of ops operations by client c1,
// every fifth rolled back, and the rows it leaves.
func fakeWorkload(ops int) (string, []workload.Row) {
	var lines []string
	var rows []workload.Row
	at := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	for seq := 1; seq <= ops; seq++ {
		op, note := workload.OpInsert, fmt.Sprintf("c1-%d", seq)
		if seq%5 == 0 {
			op, note = workload.OpRollback, workload.RolledBackNote
		}
		id := fmt.Sprintf("c1-%06d", seq)
		amount := seq*37%1000 + 1
		for _, e := range []workload.Event{
			{T: at, Client: "c1", Seq: seq, Op: op, OpID: id, Endpoint: "rw", Event: workload.Attempted,
				Amount: amount, Note: note},
			{T: at, Client: "c1", Seq: seq, Op: op, OpID: id, Endpoint: "rw", Event: workload.Acknowledged},
		} {
			data, _ := json.Marshal(e)
			lines = append(lines, string(data))
		}
		if op == workload.OpInsert {
			rows = append(rows, workload.Row{OpID: id, Client: "c1", Seq: seq, Amount: amount, Note: note})
		}
	}
	return strings.Join(lines, "\n") + "\n", rows
}

func roRefusalHistory(refusal string) string {
	at := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	lines := make([]string, 0, 2)
	for _, e := range []workload.Event{
		{T: at, Client: "ro", Seq: 1, Op: workload.OpInsert, OpID: testROOpID, Endpoint: "ro", Event: workload.Attempted,
			Amount: 38, Note: "ro-1"},
		{T: at, Client: "ro", Seq: 1, Op: workload.OpInsert, OpID: testROOpID, Endpoint: "ro", Event: workload.Failed,
			Error: refusal},
	} {
		if refusal == "" && e.Event == workload.Failed {
			e.Event, e.Error = workload.Acknowledged, ""
		}
		data, _ := json.Marshal(e)
		lines = append(lines, string(data))
	}
	return strings.Join(lines, "\n") + "\n"
}

// rowsOf returns the rows a member or Service returns on this read.
func (f *haFixture) rowsOf(name string) []workload.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[name]++
	rows := slices.Clone(f.rows)
	if n, lacking := f.missing[name]; lacking && (f.missingReads == 0 || f.reads[name] <= f.missingReads) {
		rows = slices.Delete(rows, n, n+1)
	}
	return rows
}

func jsonLines(rows []workload.Row) []string {
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		data, _ := json.Marshal(row)
		lines = append(lines, string(data))
	}
	return lines
}

func (f *haFixture) command(ctx context.Context, args ...string) (string, error) {
	f.mu.Lock()
	f.ran = append(f.ran, strings.Join(args, " "))
	f.mu.Unlock()
	if slices.Equal(args, []string{"version", "-o", "json"}) {
		return testVersionJSON, nil
	}
	if len(args) < 9 || args[0] != "-n" || args[1] != f.cfg.Namespace || args[2] != "exec" || args[4] != "-c" {
		f.t.Fatalf("unexpected command %q", args)
	}
	pod, container, script := args[3], args[5], args[9]
	if pod == clientPodName {
		if container != clientContainer {
			f.t.Fatalf("client command in container %s", container)
		}
		return f.clientCommand(ctx, script)
	}
	if container != databaseContainer {
		f.t.Fatalf("member command in container %s", container)
	}
	switch {
	case strings.Contains(script, "heartbeat status"):
		f.mu.Lock()
		f.heartbeats++
		role := f.roles[pod]
		f.mu.Unlock()
		state := map[string]string{roleMaster: "registered_and_active", roleSlave: "registered_and_standby"}[role]
		return fmt.Sprintf(" HA-Node Info (current %s, state %s)\n HA-Process Info (master 1, state %s)\n   %s\n",
			pod, role, role, state), nil
	case strings.Contains(script, "cubrid_rel"):
		return "CUBRID 11.4 (11.4.6.1963-0e7d3c1)", nil
	case strings.Contains(script, "csql") && strings.Contains(args[len(args)-1], "FROM ledger"):
		var b strings.Builder
		b.WriteString("=== <Result of SELECT Command in Line 1> ===\n\n  v\n======\n")
		for _, line := range jsonLines(f.rowsOf(pod)) {
			b.WriteString("  '" + line + "'\n")
		}
		return b.String(), nil
	}
	f.t.Fatalf("unexpected member command %q", args)
	return "", nil
}

func (f *haFixture) clientCommand(ctx context.Context, script string) (string, error) {
	switch {
	case strings.HasPrefix(script, "workload init"):
		return f.initOutput, f.initErr
	case strings.Contains(script, "nohup workload run"), strings.HasPrefix(script, "kill "):
		return "", nil
	case strings.HasPrefix(script, `grep -c '"acknowledged"'`):
		return "12\n", nil
	case strings.HasPrefix(script, "sleep 1; cat /tmp/"):
		id := strings.TrimSuffix(strings.TrimPrefix(script, "sleep 1; cat /tmp/"), ".jsonl")
		return f.backgroundHistory(id), nil
	case strings.Contains(script, "RO_JDBC_URL") && strings.Contains(script, "workload run"):
		return roRefusalHistory(f.roRefusal), nil
	case strings.Contains(script, "workload run"):
		if !strings.Contains(script, fmt.Sprintf("OPS=%d", f.cfg.Operations)) {
			f.t.Fatalf("workload run without the configured operations: %q", script)
		}
		if f.afterRun != nil {
			defer f.afterRun(ctx)
		}
		return f.history, nil
	case strings.Contains(script, "RO_JDBC_URL") && strings.Contains(script, "workload dump"):
		return strings.Join(jsonLines(f.rowsOf("ro")), "\n"), nil
	case strings.Contains(script, "workload dump"):
		return strings.Join(jsonLines(f.rowsOf("rw")), "\n"), nil
	}
	f.t.Fatalf("unexpected client command %q", script)
	return "", nil
}

func readHARecord(t *testing.T, dir string) haRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, haRecordFile))
	if err != nil {
		t.Fatal(err)
	}
	var rec haRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func onlyScenario(t *testing.T, summary evidence.Summary) evidence.Scenario {
	t.Helper()
	if summary.Lane != haLane || len(summary.Scenarios) != 1 || summary.Scenarios[0].ID != haScenarioID {
		t.Fatalf("summary = %+v, want the one scenario %s of lane %s", summary, haScenarioID, haLane)
	}
	return summary.Scenarios[0]
}

// requireNoSecretInEvidence fails when a value of the seed Secret is in any
// file of the evidence directory.
func requireNoSecretInEvidence(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{testAccessKey, testSecretKey} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s holds a value of the seed Secret", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func requireNamespaceGone(t *testing.T, f *haFixture) {
	t.Helper()
	if !slices.Equal(f.deletes, []string{f.cfg.Namespace}) {
		t.Fatalf("deletes = %v, want only the run namespace", f.deletes)
	}
	err := f.runner.Client.Get(t.Context(), client.ObjectKey{Name: f.cfg.Namespace}, &corev1.Namespace{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("run namespace after the run: %v", err)
	}
}

func TestRunHAFormsTheClusterRunsTheWorkloadAndChecksEveryMember(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	summary := readSummary(t, cfg.EvidenceDir)
	scenario := onlyScenario(t, summary)
	if !summary.Passed || scenario.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want a pass", summary)
	}
	if scenario.Operations == nil || scenario.Operations.Attempted != cfg.Operations+1 ||
		scenario.Operations.Acknowledged != cfg.Operations || scenario.Operations.Failed != 1 {
		t.Fatalf("operations = %+v, want every workload write acknowledged and the read-only write refused",
			scenario.Operations)
	}
	if summary.Environment.EngineVersion != "11.4.6.1963" || summary.Environment.CubridImageDigest == "" {
		t.Fatalf("environment = %+v, want the engine and image observed on the members", summary.Environment)
	}

	requireHARecordAndDataCheck(t, cfg, scenario)
	requireHAObjects(t, f, cfg)
	if !slices.Equal(f.writes, []string{"namespace", "secret", "cluster", "client"}) {
		t.Fatalf("writes = %v", f.writes)
	}
	requireNamespaceGone(t, f)
	requireNoSecretInEvidence(t, cfg.EvidenceDir)
}

// requireHARecordAndDataCheck checks the record and the data check of a
// passed run.
func requireHARecordAndDataCheck(t *testing.T, cfg HAConfig, scenario evidence.Scenario) {
	t.Helper()
	rec := readHARecord(t, cfg.EvidenceDir)
	if rec.StartingState == nil || rec.StartingState.Master != testHAPrimary ||
		!slices.Equal(rec.StartingState.Slaves, []string{testMemberHA0, testMemberLast}) {
		t.Fatalf("starting state = %+v, want CUBRID's master ha-1, not ordinal 0", rec.StartingState)
	}
	if len(rec.Members) != haMemberCount || !slices.Equal(rec.Members, rec.MembersAfter) {
		t.Fatalf("members = %+v, after = %+v, want three unchanged identities", rec.Members, rec.MembersAfter)
	}
	if rec.Seed.SecretFrom != cfg.SeedSecretNamespace+"/"+cfg.SeedSecret || !strings.Contains(rec.Seed.Prefix, rec.RunID) {
		t.Fatalf("seed = %+v, want the copied Secret's source and a prefix of this run", rec.Seed)
	}
	for _, rel := range []string{haHistoryFile, haDataCheckFile} {
		if !slices.Contains(scenario.Evidence, rel) {
			t.Fatalf("evidence = %v, want %s", scenario.Evidence, rel)
		}
	}
	var report workload.Report
	data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, haDataCheckFile))
	if err != nil || json.Unmarshal(data, &report) != nil || !report.OK {
		t.Fatalf("data check = %s, %v; want a passed report", data, err)
	}
	for _, name := range []string{testMemberHA0, "ha-1", "ha-2", "rw", "ro"} {
		if _, checked := report.Members[name]; !checked {
			t.Fatalf("data check members = %v, want every member and both Services", report.Members)
		}
	}

}

// requireHAObjects checks what a passed run created.
func requireHAObjects(t *testing.T, f *haFixture, cfg HAConfig) {
	t.Helper()
	if f.secret == nil || f.secret.Name != seedSecretName ||
		string(f.secret.Data[seedAccessKey]) != testAccessKey || string(f.secret.Data[seedSecretKey]) != testSecretKey {
		t.Fatalf("seed Secret copy = %+v", f.secret)
	}
	spec := f.cluster.Spec
	if spec.Topology.PromotableMembers != haMemberCount || !spec.HighAvailability.Enabled ||
		spec.ObjectStorage == nil || spec.ObjectStorage.Endpoint != cfg.SeedEndpoint ||
		spec.ObjectStorage.Bucket != cfg.SeedBucket || spec.ObjectStorage.CredentialsSecretRef.Name != seedSecretName ||
		spec.Image == nil || spec.Image.Repository != cfg.InstanceManagerImage {
		t.Fatalf("cluster spec = %+v", spec)
	}
	pod := f.client
	if pod == nil || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != cfg.WorkloadImage {
		t.Fatalf("client Pod = %+v, want the pinned workload image", pod)
	}
	security := pod.Spec.Containers[0].SecurityContext
	if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
		security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
		pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("client Pod = %+v, want it to run without privileges or an API token", pod.Spec)
	}
}

func TestRunHARefusesAnIncompleteConfiguration(t *testing.T) {
	for name, change := range map[string]func(*HAConfig){
		"workload image by tag": func(c *HAConfig) { c.WorkloadImage = "example.com/workload-client:main-" + c.Commit },
		"workload image with credentials": func(c *HAConfig) {
			c.WorkloadImage = "user:pass@example.com/workload-client@sha256:" + strings.Repeat("d", 64)
		},
		"no seed endpoint":            func(c *HAConfig) { c.SeedEndpoint = "" },
		"seed endpoint with a scheme": func(c *HAConfig) { c.SeedEndpoint = "http://store.example:8333" },
		"no seed bucket":              func(c *HAConfig) { c.SeedBucket = "" },
		"no seed Secret":              func(c *HAConfig) { c.SeedSecret = "" },
		"seed Secret in the run namespace": func(c *HAConfig) {
			c.SeedSecretNamespace = c.Namespace
		},
		"no operations":         func(c *HAConfig) { c.Operations = 0 },
		"no data check window":  func(c *HAConfig) { c.DataCheckWithin = 0 },
		"preallocated":          func(c *HAConfig) { c.NamespaceMode = PreallocatedNamespace },
		"no expected lab":       func(c *HAConfig) { c.ClusterUID = "" },
		"namespace not for lab": func(c *HAConfig) { c.Namespace = "default" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testHAConfig(t)
			change(&cfg)
			f := newHAFixture(t, testHAConfig(t))
			if err := f.runner.RunHA(t.Context(), cfg); err == nil {
				t.Fatal("RunHA() accepted the configuration")
			}
			if len(f.writes) != 0 || len(f.ran) != 0 {
				t.Fatalf("writes = %v, commands = %v; want nothing done", f.writes, f.ran)
			}
		})
	}
}

func TestRunHADoesNotOwnAnExistingNamespace(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace, UID: "someone-else"}})
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("an existing namespace must refuse the run")
	}
	if len(f.deletes) != 0 || slices.Contains(f.writes, "secret") {
		t.Fatalf("writes = %v, deletes = %v; want nothing written into or deleted from a namespace not owned",
			f.writes, f.deletes)
	}
	if onlyScenario(t, readSummary(t, cfg.EvidenceDir)).Result == evidence.Pass {
		t.Fatal("a refused run passed")
	}
}

func TestRunHAWaitsForTheStartingStateBeforeTheWorkload(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*haFixture)
		reason string
	}{
		"two masters":                 {func(f *haFixture) { f.roles[testMemberHA0] = roleMaster }, "masters"},
		"status names another master": {func(f *haFixture) { f.primary = testMemberHA0 }, "currentPrimary"},
		"a member without a role":     {func(f *haFixture) { f.roles[testMemberLast] = "unknown" }, testMemberLast},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testHAConfig(t)
			cfg.Timeout = time.Second
			f := newHAFixture(t, cfg)
			tc.change(f)
			err := f.runner.RunHA(t.Context(), cfg)
			if err == nil || !strings.Contains(err.Error(), "starting state") {
				t.Fatalf("RunHA() error = %v, want the starting state not reached", err)
			}
			if f.client != nil {
				t.Fatal("the workload started before the starting state")
			}
			rec := readHARecord(t, cfg.EvidenceDir)
			if !strings.Contains(rec.Waiting, tc.reason) {
				t.Fatalf("waiting = %q, want it to name %q", rec.Waiting, tc.reason)
			}
			if onlyScenario(t, readSummary(t, cfg.EvidenceDir)).Result != evidence.Fail {
				t.Fatal("want a failed scenario")
			}
			requireNamespaceGone(t, f)
		})
	}
}

func TestRunHAFailsWhenAMemberLacksAnAcknowledgedRow(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg)
	f.missing[testMemberLast] = 0
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), workload.RuleMissing) ||
		!strings.Contains(err.Error(), testMemberLast) {
		t.Fatalf("RunHA() error = %v, want the missing row on ha-2", err)
	}
	var report workload.Report
	data, readErr := os.ReadFile(filepath.Join(cfg.EvidenceDir, haDataCheckFile))
	if readErr != nil || json.Unmarshal(data, &report) != nil || report.OK {
		t.Fatalf("data check = %s, %v; want the failed report kept", data, readErr)
	}
	scenario := onlyScenario(t, readSummary(t, cfg.EvidenceDir))
	if scenario.Result != evidence.Fail || !slices.Contains(scenario.Evidence, haDataCheckFile) {
		t.Fatalf("scenario = %+v, want a failure with its data check", scenario)
	}
	requireNamespaceGone(t, f)
}

func TestRunHAWaitsForASlaveThatIsBehind(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.DataCheckWithin = 5 * time.Second
	f := newHAFixture(t, cfg)
	f.missing[testMemberLast] = 0
	f.missingReads = 2
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v, want a pass once the slave caught up", err)
	}
	if f.reads[testMemberLast] != 3 {
		t.Fatalf("ha-2 was read %d times, want three", f.reads[testMemberLast])
	}
}

func TestRunHAFailsUnlessEveryOperationIsAcknowledged(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*haFixture)
		want   string
	}{
		"a failed write": {func(f *haFixture) {
			f.history = strings.Replace(f.history, `"opId":"c1-000003","endpoint":"rw","event":"acknowledged"`,
				`"opId":"c1-000003","endpoint":"rw","event":"failed","error":"broken"`, 1)
			f.rows = slices.DeleteFunc(f.rows, func(r workload.Row) bool { return r.OpID == "c1-000003" })
		}, "failed 1"},
		"an unanswered write": {func(f *haFixture) {
			lines := strings.Split(strings.TrimSpace(f.history), "\n")
			f.history = strings.Join(lines[:len(lines)-1], "\n") + "\n"
		}, "unknown 1"},
		"fewer operations": {func(f *haFixture) { f.history, f.rows = fakeWorkload(f.cfg.Operations - 1) },
			"attempted 9"},
		"a read-only write accepted": {func(f *haFixture) {
			// Accepted, the row is on every member and the data rules hold.
			f.roRefusal = ""
			f.rows = append(f.rows, workload.Row{OpID: testROOpID, Client: "ro", Seq: 1, Amount: 38, Note: "ro-1"})
		}, "outcome acknowledged"},
		"a read-only write failed otherwise": {func(f *haFixture) {
			f.roRefusal = "code -21003: connection refused"
		}, "not the read-only refusal"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testHAConfig(t)
			f := newHAFixture(t, cfg)
			tc.change(f)
			err := f.runner.RunHA(t.Context(), cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunHA() error = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(cfg.EvidenceDir, haHistoryFile)); err != nil {
				t.Fatalf("history not kept: %v", err)
			}
			if onlyScenario(t, readSummary(t, cfg.EvidenceDir)).Result != evidence.Fail {
				t.Fatal("want a failed scenario")
			}
		})
	}
}

func TestRunHAFailsWhenAMemberRestartedDuringTheRun(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg)
	f.afterRun = func(ctx context.Context) {
		pod := &corev1.Pod{}
		if err := f.store.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: testMemberLast}, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.ContainerStatuses[0].ContainerID = "containerd://restarted"
		if err := f.store.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), testMemberLast) {
		t.Fatalf("RunHA() error = %v, want the restarted member named", err)
	}
	if onlyScenario(t, readSummary(t, cfg.EvidenceDir)).Result != evidence.Fail {
		t.Fatal("want a failed scenario")
	}
	requireNamespaceGone(t, f)
}

func TestRunHALabelsItsNamespaceWithTheRunID(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg)
	var labels map[string]string
	f.afterRun = func(ctx context.Context) {
		ns := &corev1.Namespace{}
		if err := f.store.Get(ctx, client.ObjectKey{Name: cfg.Namespace}, ns); err != nil {
			t.Fatal(err)
		}
		labels = ns.Labels
	}
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	rec := readHARecord(t, cfg.EvidenceDir)
	if rec.RunID == "" || labels["app.kubernetes.io/instance"] != rec.RunID ||
		labels["app.kubernetes.io/managed-by"] != installedManagedBy {
		t.Fatalf("namespace labels = %v, want the run ID %q and the installed commands' label", labels, rec.RunID)
	}
}

func TestRunHAStopsWaitingWhenTheBootstrapFailed(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.Timeout = 10 * time.Second
	f := newHAFixture(t, cfg)
	f.bootstrapFailed = true
	start := time.Now()
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "SeedBackupFailed") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("RunHA() error = %v, want the bootstrap's reason", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("RunHA() took %s, want it to stop at the failed bootstrap", elapsed)
	}
	requireNamespaceGone(t, f)
}

func TestRunHAEndsOnAStatefulSetItDoesNotOwn(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.Timeout = 10 * time.Second
	f := newHAFixture(t, cfg)
	f.foreignStatefulSet = true
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("RunHA() error = %v, want the foreign StatefulSet refused", err)
	}
	if f.client != nil {
		t.Fatal("the workload started on a StatefulSet the run does not own")
	}
}

func TestRunHAReadsTheStartingStateAgainAfterTheStablePeriod(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.StableFor = 50 * time.Millisecond
	cfg.Poll = 10 * time.Millisecond
	f := newHAFixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if f.heartbeatsBeforeClient < 2*haMemberCount {
		t.Fatalf("%d heartbeat readings before the workload, want the roles read again after the stable period",
			f.heartbeatsBeforeClient)
	}
}

func TestRunHAFailsWhenTheMasterMovedDuringTheRun(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg)
	f.afterRun = func(context.Context) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.roles[testHAPrimary], f.roles[testMemberHA0] = roleSlave, roleMaster
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "master moved") {
		t.Fatalf("RunHA() error = %v, want the moved master", err)
	}
	if onlyScenario(t, readSummary(t, cfg.EvidenceDir)).Result != evidence.Fail {
		t.Fatal("want a failed scenario")
	}
}

func TestRunHAFailsAtOnceOnAViolationThatLagCannotExplain(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.DataCheckWithin = 20 * time.Second
	cfg.Timeout = time.Minute
	f := newHAFixture(t, cfg)
	f.missing[testMemberLast] = 0
	f.missingReads = 1000
	// A rolled-back row on every member: no slave catches up into that.
	f.rows = append(f.rows, workload.Row{OpID: "c1-000005", Client: "c1", Seq: 5, Amount: 5*37%1000 + 1,
		Note: workload.RolledBackNote})
	start := time.Now()
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), workload.RuleRolledBackPresent) {
		t.Fatalf("RunHA() error = %v, want the rolled-back row", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("RunHA() took %s, want no wait for a violation lag cannot explain", elapsed)
	}
	var report workload.Report
	data, readErr := os.ReadFile(filepath.Join(cfg.EvidenceDir, haDataCheckFile))
	if readErr != nil || json.Unmarshal(data, &report) != nil || report.OK {
		t.Fatalf("data check = %s, %v; want the failed report kept", data, readErr)
	}
}

func TestRunHAStopsOnAClientImageThatCannotBePulled(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.Timeout = 10 * time.Second
	f := newHAFixture(t, cfg)
	f.clientWaiting = "ImagePullBackOff"
	start := time.Now()
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("RunHA() error = %v, want the pull failure", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("RunHA() took %s, want it to stop on the pull failure", elapsed)
	}
}

func TestRunHAKeepsTheSeedSecretOutOfErrorsAndEvidence(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg)
	f.initOutput = "connect with " + testAccessKey + "/" + testSecretKey + " failed"
	f.initErr = errors.New("kubectl failed: " + f.initOutput)
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil {
		t.Fatal("RunHA() passed")
	}
	for _, secret := range []string{testAccessKey, testSecretKey} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("RunHA() error = %v, holds a value of the seed Secret", err)
		}
	}
	requireNoSecretInEvidence(t, cfg.EvidenceDir)
	summary, readErr := os.ReadFile(filepath.Join(cfg.EvidenceDir, "summary.json"))
	if readErr != nil || !strings.Contains(string(summary), "connect with") {
		t.Fatalf("summary = %s, %v; want the failure recorded, redacted", summary, readErr)
	}
}

func TestRunHARefusesASeedSecretWithoutBothKeys(t *testing.T) {
	for name, data := range map[string]map[string][]byte{
		"no secretKey":    {seedAccessKey: []byte(testAccessKey)},
		"short accessKey": {seedAccessKey: []byte("abc"), seedSecretKey: []byte(testSecretKey)},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testHAConfig(t)
			cfg.SeedSecret = "incomplete"
			f := newHAFixture(t, cfg, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "incomplete",
				Namespace: cfg.SeedSecretNamespace}, Data: data})
			if err := f.runner.RunHA(t.Context(), cfg); err == nil {
				t.Fatal("RunHA() accepted the Secret")
			}
			if slices.Contains(f.writes, "secret") || slices.Contains(f.writes, "cluster") {
				t.Fatalf("writes = %v, want no Secret copy and no cluster", f.writes)
			}
			requireNamespaceGone(t, f)
		})
	}
}

func TestRunHAStartsTheStablePeriodOverWhenTheMasterChanges(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.StableFor = 100 * time.Millisecond
	cfg.Poll = 10 * time.Millisecond
	cfg.Timeout = 5 * time.Second
	f := newHAFixture(t, cfg)
	// After the first full reading the master moves once, with the status.
	go func() {
		for {
			f.mu.Lock()
			read := f.heartbeats
			if read >= haMemberCount {
				f.roles[testHAPrimary], f.roles[testMemberHA0] = roleSlave, roleMaster
				f.mu.Unlock()
				if err := f.setPrimary(t.Context(), testMemberHA0); err != nil {
					t.Error(err)
				}
				return
			}
			f.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	if rec := readHARecord(t, cfg.EvidenceDir); rec.StartingState.Master != testMemberHA0 {
		t.Fatalf("starting state = %+v, want the master after the move", rec.StartingState)
	}
}

// setPrimary writes status.currentPrimary as the Operator would.
func (f *haFixture) setPrimary(ctx context.Context, primary string) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := f.store.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return err
	}
	cluster.Status.CurrentPrimary = primary
	return f.store.Update(ctx, cluster)
}
