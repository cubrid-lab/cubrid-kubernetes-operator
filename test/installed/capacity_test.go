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
	"flag"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testGi = int64(1) << 30
	// testRoomyNode has room for a run; testFullNode does not.
	testRoomyNode = "node-1"
	testFullNode  = "node-3"
)

// runVolume is a local volume bound to a claim in namespace, pinned to node,
// as the local-path provisioner leaves one after its claim is deleted.
func runVolume(name, namespace, node string, phase corev1.PersistentVolumePhase) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Namespace: namespace, Name: "data-" + clusterName + "-0"},
			NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}}}}}},
		Status: corev1.PersistentVolumeStatus{Phase: phase}}
}

func TestRunCleanupIsNotCompleteWhileARunVolumeIsLeft(t *testing.T) {
	cfg := testConfig(t)
	// A volume of another namespace is not the run's and is never waited for.
	f := newConfiguredFixture(t, cfg, runVolume("pv-run", cfg.Namespace, testFullNode, corev1.VolumeReleased),
		runVolume("pv-other", "cubrid-test", testFullNode, corev1.VolumeBound))
	if err := f.runner.Run(t.Context(), f.cfg); err == nil {
		t.Fatal("Run() passed with the run's volume left behind")
	}
	rec := readRecord(t, cfg.EvidenceDir)
	if !strings.Contains(rec.Cleanup, "pv-run (Released, claim data-smoke-0, node node-3)") ||
		strings.Contains(rec.Cleanup, "pv-other") {
		t.Fatalf("cleanup = %q, want only the run's Released volume named with its node", rec.Cleanup)
	}
	if summary := readSummary(t, cfg.EvidenceDir); summary.Passed || summary.Scenarios[0].CleanupSucceeded == nil ||
		*summary.Scenarios[0].CleanupSucceeded {
		t.Fatalf("summary = %+v, want a failed run with an incomplete cleanup", summary)
	}
}

func TestRunCleanupCompletesOnceTheRunVolumesAreDeleted(t *testing.T) {
	cfg := testConfig(t)
	f := newConfiguredFixture(t, cfg, runVolume("pv-run", cfg.Namespace, testFullNode, corev1.VolumeReleased))
	// The provisioner removes the volume after the namespace is gone.
	f.deleteHook = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if err := c.Delete(ctx, obj, opts...); err != nil {
			return err
		}
		if _, ok := obj.(*corev1.Namespace); ok {
			return c.Delete(ctx, &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-run"}})
		}
		return nil
	}
	if err := f.runner.Run(t.Context(), f.cfg); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if rec := readRecord(t, cfg.EvidenceDir); rec.Cleanup != "complete" {
		t.Fatalf("cleanup = %q, want complete", rec.Cleanup)
	}
}

// nodesWithFreeSpace returns nodes and a command hook that reports each
// node's free space as kubelet's stats summary does.
func nodesWithFreeSpace(t *testing.T, free map[string]int64, unschedulable string) ([]client.Object,
	func(context.Context, []string) (string, error), *[]string) {
	t.Helper()
	nodes := make([]client.Object, 0, len(free))
	for name := range free {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if name == unschedulable {
			node.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane",
				Effect: corev1.TaintEffectNoSchedule}}
		}
		nodes = append(nodes, node)
	}
	read := &[]string{}
	hook := func(_ context.Context, args []string) (string, error) {
		if len(args) == 3 && args[0] == testVerbGet && args[1] == "--raw" {
			node := strings.TrimSuffix(strings.TrimPrefix(args[2], "/api/v1/nodes/"), "/proxy/stats/summary")
			*read = append(*read, node)
			return fmt.Sprintf(`{"node":{"nodeName":%q,"fs":{"availableBytes":%d,"capacityBytes":%d}}}`, node,
				free[node], 40*testGi), nil
		}
		return "", fmt.Errorf("unexpected command %q", args)
	}
	return nodes, hook, read
}

// answerNodeStats lets hook answer the reads of kubelet's stats summary and
// the fixture answer every other command.
func answerNodeStats(f *fixture, hook func(context.Context, []string) (string, error)) {
	answer := f.runner.Command
	f.runner.Command = func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == testVerbGet {
			return hook(ctx, args)
		}
		return answer(ctx, args...)
	}
}

func TestRunRefusesANodeShortOfFreeSpaceBeforeCreatingAnything(t *testing.T) {
	cfg := testConfig(t)
	cfg.MinNodeFreeSpace = 4 * testGi
	nodes, hook, _ := nodesWithFreeSpace(t, map[string]int64{testRoomyNode: 16 * testGi, testFullNode: 3 * testGi}, "")
	f := newConfiguredFixture(t, cfg, nodes...)
	answerNodeStats(f, hook)
	err := f.runner.Run(t.Context(), f.cfg)
	if err == nil || !strings.Contains(err.Error(), testFullNode+" has 3221225472 bytes free") {
		t.Fatalf("Run() = %v, want a refusal naming node-3", err)
	}
	if len(f.writes) != 0 || f.deletes != 0 {
		t.Fatalf("a refused run created or deleted something: writes %v, deletes %d", f.writes, f.deletes)
	}
	rec := readRecord(t, cfg.EvidenceDir)
	if rec.NodeFreeBytes[testRoomyNode] != 16*testGi || rec.NodeFreeBytes[testFullNode] != 3*testGi {
		t.Fatalf("nodeFreeBytes = %v, want both nodes recorded", rec.NodeFreeBytes)
	}
}

func TestRunRecordsTheFreeSpaceOfSchedulableNodesOnly(t *testing.T) {
	cfg := testConfig(t)
	cfg.MinNodeFreeSpace = 4 * testGi
	// The control-plane node runs no member, so its low space does not count.
	nodes, hook, read := nodesWithFreeSpace(t, map[string]int64{testRoomyNode: 16 * testGi, "server": testGi}, "server")
	f := newConfiguredFixture(t, cfg, nodes...)
	answerNodeStats(f, hook)
	if err := f.runner.Run(t.Context(), f.cfg); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	rec := readRecord(t, cfg.EvidenceDir)
	if len(rec.NodeFreeBytes) != 1 || rec.NodeFreeBytes[testRoomyNode] != 16*testGi || len(*read) != 1 {
		t.Fatalf("nodeFreeBytes = %v, read %v, want node-1 only", rec.NodeFreeBytes, *read)
	}
}

func TestFreeSpaceFlagReadsASizeAndRefusesANegativeOne(t *testing.T) {
	var bytes int64
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.Var(FreeSpaceFlag(&bytes, "8Gi"), "min-node-free-space", "")
	if bytes != 8*testGi {
		t.Fatalf("default = %d, want 8Gi", bytes)
	}
	if err := flags.Parse([]string{"-min-node-free-space", "10Gi"}); err != nil || bytes != 10*testGi {
		t.Fatalf("Parse(10Gi) = %v, %d", err, bytes)
	}
	if err := flags.Parse([]string{"-min-node-free-space", "-1Gi"}); err == nil {
		t.Fatal("a negative size was accepted")
	}
}
