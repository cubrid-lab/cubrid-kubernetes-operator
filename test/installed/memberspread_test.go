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
	"flag"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// labNodes returns schedulable worker nodes, one cordoned worker and a
// tainted control-plane node; only the schedulable ones can run a member.
func labNodes(schedulable ...string) []client.Object {
	ready := corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}
	nodes := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cordoned"}, Spec: corev1.NodeSpec{Unschedulable: true},
			Status: ready},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "server"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}}, Status: ready},
	}
	for _, name := range schedulable {
		nodes = append(nodes, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: ready})
	}
	return nodes
}

func TestMemberSpreadFlagTakesRequiredOrPreferredOnly(t *testing.T) {
	var spread databasev1alpha1.MemberSpread
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.Var(MemberSpreadFlag(&spread, databasev1alpha1.MemberSpreadRequired), "member-spread", "")
	if spread != databasev1alpha1.MemberSpreadRequired {
		t.Fatalf("default = %q, want Required", spread)
	}
	if err := flags.Parse([]string{"-member-spread", "Preferred"}); err != nil ||
		spread != databasev1alpha1.MemberSpreadPreferred {
		t.Fatalf("-member-spread Preferred = %q, %v", spread, err)
	}
	for _, value := range []string{"", "Sometimes", "required"} {
		if err := flags.Set("member-spread", value); err == nil {
			t.Fatalf("-member-spread %q was accepted", value)
		}
	}
}

func TestHAClusterSpecSetsTheRunsMemberSpread(t *testing.T) {
	for _, spread := range []databasev1alpha1.MemberSpread{
		databasev1alpha1.MemberSpreadRequired, databasev1alpha1.MemberSpreadPreferred,
	} {
		cfg := testHAConfig(t)
		cfg.MemberSpread = spread
		// Every cluster the HA harness creates, the source cluster and the
		// S11 and S12 ones, is built from this spec.
		spec := (&haRun{cfg: cfg}).clusterSpec("prefix", "secret")
		if spec.Placement == nil || spec.Placement.MemberSpread != spread {
			t.Fatalf("placement = %+v, want memberSpread %s", spec.Placement, spread)
		}
	}
}

func TestRunHARefusesRequiredMemberSpreadOnTooFewSchedulableNodes(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.MemberSpread = databasev1alpha1.MemberSpreadRequired
	f := newHAFixture(t, cfg, labNodes("worker-1", "worker-2")...)
	err := f.runner.RunHA(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "not enough schedulable nodes for Required member spread") {
		t.Fatalf("RunHA() = %v, want a refusal for too few schedulable nodes", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("a refused run created %v", f.writes)
	}
	if scenario := onlyScenario(t, readSummary(t, cfg.EvidenceDir)); scenario.Result != evidence.Blocked {
		t.Fatalf("result = %s, want blocked: the cluster could not have been placed", scenario.Result)
	}
	rec := readHARecord(t, cfg.EvidenceDir)
	if rec.MemberSpread != "Required" || !slices.Equal(rec.SchedulableNodes, []string{"worker-1", "worker-2"}) {
		t.Fatalf("record = memberSpread %q, schedulable %v", rec.MemberSpread, rec.SchedulableNodes)
	}
}

func TestRunHAWithRequiredMemberSpreadRunsOnEnoughSchedulableNodes(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.MemberSpread = databasev1alpha1.MemberSpreadRequired
	f := newHAFixture(t, cfg, labNodes("worker-1", "worker-2", "worker-3")...)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	if p := f.cluster.Spec.Placement; p == nil || p.MemberSpread != databasev1alpha1.MemberSpreadRequired {
		t.Fatalf("placement = %+v, want Required", p)
	}
	rec := readHARecord(t, cfg.EvidenceDir)
	if rec.MemberSpread != "Required" || len(rec.SchedulableNodes) != 3 {
		t.Fatalf("record = memberSpread %q, schedulable %v", rec.MemberSpread, rec.SchedulableNodes)
	}
}

func TestRunHAWithPreferredMemberSpreadDoesNotCountNodes(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg, labNodes("worker-1")...)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	if p := f.cluster.Spec.Placement; p == nil || p.MemberSpread != databasev1alpha1.MemberSpreadPreferred {
		t.Fatalf("placement = %+v, want Preferred", p)
	}
	if rec := readHARecord(t, cfg.EvidenceDir); rec.MemberSpread != "Preferred" || rec.SchedulableNodes != nil {
		t.Fatalf("record = memberSpread %q, schedulable %v", rec.MemberSpread, rec.SchedulableNodes)
	}
}
