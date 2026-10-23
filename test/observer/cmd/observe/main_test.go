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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/installed"
)

func validArgs(dir string) []string {
	return []string{
		"-kubeconfig", "/nonexistent/kubeconfig", "-context", "lab", "-cluster-uid", "uid",
		"-namespace", "db", "-cluster", "ha", "-operator-namespace", "op", "-operator-deployment", "mgr",
		"-prometheus-namespace", "monitoring", "-prometheus-service", "prom",
		"-commit", strings.Repeat("a", 40), "-level", "vm-lab", "-evidence-dir", dir,
	}
}

func without(args []string, flagName string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == flagName {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestRunRefusesAnIncompleteSelection(t *testing.T) {
	dir := t.TempDir()
	for _, missing := range []string{"-kubeconfig", "-context", "-cluster-uid", "-namespace", "-cluster",
		"-operator-namespace", "-operator-deployment", "-prometheus-namespace", "-prometheus-service", "-evidence-dir"} {
		err := run(context.Background(), without(validArgs(dir), missing))
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("run() without %s = %v, want an error naming it", missing, err)
		}
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"short commit":      {append(without(validArgs(dir), "-commit"), "-commit", "abc123"), "-commit"},
		"unknown level":     {append(without(validArgs(dir), "-level"), "-level", "prod"), "-level"},
		"window too narrow": {append(validArgs(dir), "-interval", "10s", "-window", "15s"), "-window"},
		"window under 1 s":  {append(validArgs(dir), "-interval", "200ms", "-window", "400ms"), "-window"},
		"positional":        {append(validArgs(dir), "extra"), "positional"},
		"no scrape timeout": {append(validArgs(dir), "-scrape-timeout", "0s"), "-scrape-timeout"},
	} {
		if err := run(context.Background(), tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("run() with %s = %v, want an error naming %s", name, err, tc.want)
		}
	}
}

func TestRunRefusesAnEvidenceDirectoryInUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "samples.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), validArgs(dir))
	if err == nil || !strings.Contains(err.Error(), "samples.jsonl") {
		t.Fatalf("run() = %v, want a refusal naming what the directory holds", err)
	}
}

type fakeTarget struct {
	client  client.Client
	forward string // the script kubectl port-forward is replaced with
}

func (f *fakeTarget) Runner() (*installed.Runner, error) {
	return &installed.Runner{Client: f.client, Redactor: evidence.NewRedactor()}, nil
}
func (f *fakeTarget) Kubectl(ctx context.Context, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", f.forward)
}
func (f *fakeTarget) Close() error { return nil }

// labTarget is a cluster whose kube-system UID is "uid", with the manager
// Deployment and the CubridCluster, and a Prometheus that has no series.
func labTarget(t *testing.T) *fakeTarget {
	t.Helper()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	t.Cleanup(prom.Close)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = databasev1alpha1.AddToScheme(scheme)
	labels := map[string]string{"control-plane": "controller-manager"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uid"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "mgr", Namespace: "op"},
			Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}}},
		&databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: "ha", Namespace: "db"}},
	).Build()
	port := prom.URL[strings.LastIndex(prom.URL, ":")+1:]
	return &fakeTarget{client: c,
		forward: fmt.Sprintf(`echo "Forwarding from 127.0.0.1:%s -> 9090"; exec sleep 30`, port)}
}

func withTarget(t *testing.T, target *fakeTarget) {
	t.Helper()
	saved := openTarget
	openTarget = func(string, string) (targetHandle, error) { return target, nil }
	t.Cleanup(func() { openTarget = saved })
}

func TestRunRefusesAClusterWithAnotherIdentity(t *testing.T) {
	withTarget(t, labTarget(t))
	dir := t.TempDir()
	args := append(without(validArgs(dir), "-cluster-uid"), "-cluster-uid", "another")
	err := run(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "kube-system UID differs") {
		t.Fatalf("run() = %v, want a refusal of the cluster", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("evidence dir holds %d entries, want nothing written for a refused cluster", len(entries))
	}
}

func TestRunStoppedEarlyIsIncompleteAndKeepsItsEvidence(t *testing.T) {
	withTarget(t, labTarget(t))
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err := run(ctx, append(validArgs(dir), "-interval", "1s", "-window", "2s", "-duration", "1h"))
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("run() = %v, want an incomplete run reported as an error", err)
	}
	data, rerr := os.ReadFile(filepath.Join(dir, "observer.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var res struct {
		Verdict string `json:"verdict"`
		Samples int    `json:"samples"`
		Run     struct {
			StoppedEarly string `json:"stoppedEarly"`
		} `json:"run"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Verdict != "incomplete" || res.Samples == 0 || res.Run.StoppedEarly == "" {
		t.Fatalf("observer.json = %s, want incomplete with the samples taken and why it stopped", data)
	}
	lines, _ := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
	if got := strings.Count(string(lines), "\n"); got != res.Samples {
		t.Fatalf("samples.jsonl has %d lines, want the %d samples", got, res.Samples)
	}
}
