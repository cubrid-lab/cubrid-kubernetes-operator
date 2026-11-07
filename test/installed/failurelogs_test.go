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
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	testFailureManager = "logs-manager"
	testFailureBroker  = "ha-broker-rw-0"
	testBrokerError    = "broker/error_log/rw_1.err"
	testBrokerLine     = "CAS error -581: updates are disabled\n"
)

func labelledPod(name, namespace string, podLabels map[string]string, container string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID("uid-" + name),
		Labels: podLabels},
		Spec:   corev1.PodSpec{NodeName: testRoomyNode},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: container}}}}
}

// When LAB01 fails, its namespace is removed at the end of the run, and the
// Broker's and members' logs would go with it (#662). They are read first, and
// the bundle joins the scenario's evidence.
func TestFailedHARunKeepsItsLogsBeforeItsNamespaceGoes(t *testing.T) {
	cfg := testHAConfig(t)
	manager := labelledPod(testFailureManager, cfg.OperatorNamespace,
		map[string]string{"control-plane": "controller-manager"}, managerContainer)
	f := newHAFixture(t, cfg, manager)
	f.history = strings.Replace(f.history, `"opId":"c1-000003","endpoint":"rw","event":"acknowledged"`,
		`"opId":"c1-000003","endpoint":"rw","event":"failed","error":"broken"`, 1)
	f.rows = slices.DeleteFunc(f.rows, func(r workload.Row) bool { return r.OpID == "c1-000003" })
	read := false
	f.runner.podLogs = func(_ context.Context, namespace, pod string, _ *corev1.PodLogOptions) (io.ReadCloser, error) {
		if len(f.deletes) != 0 {
			t.Errorf("log of %s/%s read after the namespace was deleted", namespace, pod)
		}
		read = true
		return io.NopCloser(strings.NewReader("2026-11-06T09:10:00.000000000Z a line\n")), nil
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("the run passed")
	}
	if !read {
		t.Fatal("no log was read")
	}
	scenario := onlyScenario(t, readSummary(t, cfg.EvidenceDir))
	if scenario.Result != evidence.Fail {
		t.Fatalf("the verdict changed: %+v", scenario)
	}
	const index = haScenarioID + "/logs/index.json"
	if !slices.Contains(scenario.Evidence, index) {
		t.Fatalf("the bundle is not in the scenario's evidence: %v", scenario.Evidence)
	}
	if _, err := os.Stat(filepath.Join(cfg.EvidenceDir, index)); err != nil {
		t.Fatal(err)
	}
}

// A run that passed needs no bundle, and LAB01's files stay what they were.
func TestPassedHARunKeepsNoFailureLogs(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.EvidenceDir, haScenarioID, "logs")); !os.IsNotExist(err) {
		t.Fatalf("logs of a passed run: %v", err)
	}
}

// The read-write Broker's logs and every member's raw heartbeat status, which
// #662 asks a failed run to keep, are in the bundle, and the collection writes
// nothing to the cluster: the client's hooks refuse any write.
func TestFailureLogsHoldTheBrokerAndTheHeartbeat(t *testing.T) {
	const operators, namespace = "operators", "cubrid-lab-logs"
	deny := func(string) error { return errors.New("the collection wrote to the cluster") }
	dir := t.TempDir()
	plan := evidence.ClusterLogPlan(operators, namespace, haClusterName, nil, true)
	memberLabels, err := labels.ConvertSelectorToLabelsMap(plan.Sets[1].Selector)
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{
		labelledPod(testFailureManager, operators, map[string]string{"control-plane": "controller-manager"},
			managerContainer),
		labelledPod(testFailureBroker, namespace, map[string]string{"database.cubrid.io/broker-access-mode": "rw",
			"app.kubernetes.io/instance": haClusterName}, "broker"),
		labelledPod("ha-0", namespace, memberLabels, databaseContainer),
	}
	hooks := interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return deny("create")
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return deny("delete")
		},
	}
	cfg := testHAConfig(t)
	var commands [][]string
	runner := &Runner{Client: testClient(t, cfg.Config, hooks, objects...),
		Command: func(_ context.Context, args ...string) (string, error) {
			commands = append(commands, args)
			script := strings.Join(args, " ")
			switch {
			case strings.Contains(script, "ls -1t"):
				return testBrokerError + "\n", nil
			case strings.Contains(script, "heartbeat status"):
				return "raw heartbeat of " + args[3] + "\n", nil
			case strings.Contains(script, "tail -c"):
				return testBrokerLine, nil
			}
			if slices.Contains(args, "get") && slices.Contains(args, "json") {
				return `{"items":[]}`, nil
			}
			t.Errorf("unexpected command %q", args)
			return "", nil
		},
		podLogs: func(context.Context, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("2026-11-06T09:10:00.000000000Z a line\n")), nil
		}}
	h := &haRun{Runner: runner, redactor: evidence.NewRedactor(),
		members: []string{"ha-0"}, guard: func(context.Context) error { return nil }}
	h.cfg.Namespace, h.cfg.OperatorNamespace, h.cfg.EvidenceDir = namespace, operators, dir
	if err := h.keepFailureLogs(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, haScenarioID, "logs", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index evidence.LogIndex
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, s := range index.Checkpoints[0].Sources {
		if s.Status == evidence.LogCollected {
			found[s.Component+"/"+s.Pod] = s.File
		}
	}
	for _, key := range []string{evidence.LogOperator + "/" + testFailureManager, evidence.LogInstanceManager + "/ha-0",
		evidence.LogEngine + "/ha-0", evidence.LogBrokerProcess + "/" + testFailureBroker,
		evidence.LogBroker + "/" + testFailureBroker, evidence.LogHeartbeat + "/ha-0"} {
		if found[key] == "" {
			t.Errorf("no %s in %+v", key, index.Checkpoints[0].Sources)
		}
	}
	heartbeat, err := os.ReadFile(filepath.Join(dir, found[evidence.LogHeartbeat+"/ha-0"]))
	if err != nil || string(heartbeat) != "raw heartbeat of ha-0\n" {
		t.Errorf("heartbeat file %q, %v", heartbeat, err)
	}
	if !slices.Contains(h.files, haScenarioID+"/logs/index.json") {
		t.Errorf("files %v", h.files)
	}
	if !slices.Contains(h.files, haScenarioID+"/cluster/index.json") {
		t.Errorf("files %v", h.files)
	}
	// Every command is a fixed read in a Pod of the run's namespace, or a get
	// of a Kubernetes snapshot.
	for _, args := range commands {
		line := strings.Join(args, " ")
		if !strings.HasPrefix(line, "-n "+namespace+" exec ") && (args[2] != "get" || !slices.Contains(args, "json")) {
			t.Errorf("neither a command in a Pod of the run's namespace nor a snapshot: %q", args)
		}
	}
}

// A bundle that could not be stored is reported, and nothing is listed as
// evidence: a listed file that does not exist would change a verdict.
func TestFailureLogsThatCannotBeStoredAreNotListed(t *testing.T) {
	cfg := testHAConfig(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &haRun{Runner: &Runner{Client: testClient(t, cfg.Config, interceptor.Funcs{}),
		Command: func(context.Context, ...string) (string, error) { return "", errors.New("no") }},
		redactor: evidence.NewRedactor(), guard: func(context.Context) error { return nil }}
	h.cfg.Namespace, h.cfg.OperatorNamespace, h.cfg.EvidenceDir = "ns", "operators", file
	if err := h.keepFailureLogs(); err == nil {
		t.Fatal("no problem reported")
	}
	if len(h.files) != 0 {
		t.Fatalf("files %v", h.files)
	}
}
