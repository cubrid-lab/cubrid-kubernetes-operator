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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestMemberRequestFlagsDefaultToTheDocumentedBaseline(t *testing.T) {
	var cfg Config
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	MemberRequestFlags(flags, &cfg)
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if got := cfg.MemberMemoryRequest.String(); got != "640Mi" {
		t.Errorf("memory default = %s, want 640Mi", got)
	}
	if got := cfg.MemberCPURequest.String(); got != "150m" {
		t.Errorf("CPU default = %s, want 150m", got)
	}
}

// A member without a request is what let the scheduler fill a worker unseen
// (#671), so no flag value may turn the request off.
func TestMemberRequestFlagsTakeAPositiveQuantityOnly(t *testing.T) {
	var cfg Config
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	MemberRequestFlags(flags, &cfg)
	args := []string{"-member-memory-request", "1Gi", "-member-cpu-request", "250m"}
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	if cfg.MemberMemoryRequest.String() != "1Gi" || cfg.MemberCPURequest.String() != "250m" {
		t.Fatalf("parsed memory %s, CPU %s", cfg.MemberMemoryRequest.String(), cfg.MemberCPURequest.String())
	}
	for _, name := range []string{"member-memory-request", "member-cpu-request"} {
		for _, value := range []string{"", "0", "0Mi", "-1", "plenty"} {
			if err := flags.Set(name, value); err == nil {
				t.Errorf("-%s %q was accepted", name, value)
			}
		}
	}
}

func TestConfigRefusesAMemberRequestThatIsNotPositive(t *testing.T) {
	for name, clear := range map[string]func(*Config){
		"no memory request":       func(c *Config) { c.MemberMemoryRequest = resource.Quantity{} },
		"no CPU request":          func(c *Config) { c.MemberCPURequest = resource.Quantity{} },
		"negative memory request": func(c *Config) { c.MemberMemoryRequest = resource.MustParse("-1Mi") },
		"negative CPU request":    func(c *Config) { c.MemberCPURequest = resource.MustParse("-1m") },
	} {
		smoke, ha := testConfig(t), testHAConfig(t)
		ha.NamespaceMode = FreshNamespace
		clear(&smoke)
		clear(&ha.Config)
		if err := smoke.validate(); err == nil || !strings.Contains(err.Error(), "request") {
			t.Errorf("%s: smoke validate() = %v", name, err)
		}
		if err := ha.validate(); err == nil || !strings.Contains(err.Error(), "request") {
			t.Errorf("%s: HA validate() = %v", name, err)
		}
	}
}

// Every cluster the HA harness creates, the source cluster and the S11 and
// S12 ones, is built from one of these two specs.
func TestHAClusterSpecsSetTheRunsMemberRequestsAndNoLimit(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.MemberMemoryRequest, cfg.MemberCPURequest = resource.MustParse("700Mi"), resource.MustParse("150m")
	h := &haRun{cfg: cfg}
	for name, spec := range map[string]corev1.ResourceRequirements{
		"HA":         h.clusterSpec("prefix", "secret").Resources,
		"standalone": h.standaloneSpec("secret").Resources,
	} {
		memory, cpu := spec.Requests[corev1.ResourceMemory], spec.Requests[corev1.ResourceCPU]
		if memory.String() != "700Mi" || cpu.String() != "150m" {
			t.Errorf("%s requests = %v", name, spec.Requests)
		}
		if len(spec.Limits) != 0 {
			t.Errorf("%s limits = %v: a limit would kill a member at a peak nobody has measured", name, spec.Limits)
		}
	}
}

func TestRunHARecordsEachMembersRequestsAndNode(t *testing.T) {
	cfg := testHAConfig(t)
	f := newHAFixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	rec := readHARecord(t, cfg.EvidenceDir)
	if len(rec.Members) != haMemberCount {
		t.Fatalf("members = %+v", rec.Members)
	}
	for _, member := range rec.Members {
		if member.MemoryRequest != DefaultMemberMemoryRequest || member.CPURequest != DefaultMemberCPURequest ||
			member.Node == "" {
			t.Errorf("member = %+v, want its requests and node", member)
		}
	}
}

func TestRunRecordsTheSmokeMembersRequests(t *testing.T) {
	f := newFixture(t)
	if err := f.runner.Run(t.Context(), f.cfg); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	database := readRecord(t, f.cfg.EvidenceDir).Database
	if database.MemoryRequest != DefaultMemberMemoryRequest || database.CPURequest != DefaultMemberCPURequest {
		t.Fatalf("database = %+v, want its requests", database)
	}
}

// The record states what the Pod declares: a container that declares no
// request has no figure, not a figure of zero.
func TestPodIdentityRecordsOnlyTheRequestsTheContainerDeclares(t *testing.T) {
	pod := &corev1.Pod{}
	pod.UID, pod.Name = "member-uid", "member-0"
	pod.Spec = corev1.PodSpec{NodeName: "worker-1", Containers: []corev1.Container{{
		Name: databaseContainer, Image: "image",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(DefaultMemberMemoryRequest),
			corev1.ResourceCPU:    resource.MustParse(DefaultMemberCPURequest)}}}}}
	pod.Status = corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: databaseContainer, Ready: true, ImageID: "member-image-id",
			ContainerID: "container-id", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}
	identity, err := podIdentity(pod, databaseContainer, "image")
	if err != nil || identity.MemoryRequest != DefaultMemberMemoryRequest ||
		identity.CPURequest != DefaultMemberCPURequest {
		t.Fatalf("identity = %+v, %v", identity, err)
	}
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	identity, err = podIdentity(pod, databaseContainer, "image")
	if err != nil || identity.MemoryRequest != "" || identity.CPURequest != "" {
		t.Fatalf("identity without requests = %+v, %v", identity, err)
	}
}
