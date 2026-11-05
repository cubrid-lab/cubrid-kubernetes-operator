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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const testNoFit = "0/4 nodes are available: 3 Insufficient memory, 1 node(s) were unschedulable"

var testUnschedulable = corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
	Reason: corev1.PodReasonUnschedulable, Message: testNoFit}

// A member that is Pending without the scheduler having said it does not
// fit: these readings say nothing about the node's capacity.
var testPendingOtherwise = map[string]*corev1.PodCondition{
	"bound and starting": {Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
	"gated":              {Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "SchedulingGated"},
	"no such condition":  nil,
}

// leavePending stores the member as Pending and not Ready, with scheduled as
// its PodScheduled condition.
func leavePending(ctx context.Context, c client.WithWatch, key client.ObjectKey, scheduled *corev1.PodCondition) error {
	member := &corev1.Pod{}
	if err := c.Get(ctx, key, member); err != nil {
		return nil
	}
	member.Status = corev1.PodStatus{Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}
	if scheduled != nil {
		member.Status.Conditions = append(member.Status.Conditions, *scheduled)
	}
	return c.Status().Update(ctx, member)
}

// keepPending leaves member Pending at every read of it.
func (f *haFixture) keepPending(member string, scheduled *corev1.PodCondition) {
	previous := f.podRead
	f.podRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if previous != nil {
			if err := previous(ctx, c, key); err != nil {
				return err
			}
		}
		if key.Name != member {
			return nil
		}
		return leavePending(ctx, c, key, scheduled)
	}
}

func requireBlockedByScheduler(t *testing.T, s evidence.Scenario, err error) {
	t.Helper()
	if err == nil || s.Result != evidence.Blocked || !strings.Contains(s.Reason, testNoFit) {
		t.Fatalf("%s = %+v, %v; want blocked with the scheduler's message", s.Name(), s, err)
	}
}

// The scheduler's refusal is evidence of the environment, and the record
// keeps it apart from the scenario's reason.
func requireRecordedRefusal(t *testing.T, got []unschedulableMember, member string) {
	t.Helper()
	if len(got) != 1 || got[0].Pod != member || got[0].Message != testNoFit {
		t.Fatalf("unschedulable members = %+v, want %s with the scheduler's message", got, member)
	}
}

func TestRunIsBlockedWhenTheMemberCannotBeScheduled(t *testing.T) {
	f := newFixture(t)
	f.cfg.Timeout = 300 * time.Millisecond
	f.afterCluster = func(ctx context.Context, c client.WithWatch) {
		key := client.ObjectKey{Namespace: f.cfg.Namespace, Name: clusterName + "-0"}
		if err := leavePending(ctx, c, key, &testUnschedulable); err != nil {
			t.Error(err)
		}
	}
	err := f.runner.Run(t.Context(), f.cfg)
	requireBlockedByScheduler(t, readSummary(t, f.cfg.EvidenceDir).Scenarios[0], err)
	requireRecordedRefusal(t, readRecord(t, f.cfg.EvidenceDir).UnschedulableMembers, clusterName+"-0")
}

func TestRunStaysFailedWhenTheMemberIsPendingForAnotherReason(t *testing.T) {
	for name, scheduled := range testPendingOtherwise {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cfg.Timeout = 300 * time.Millisecond
			f.afterCluster = func(ctx context.Context, c client.WithWatch) {
				key := client.ObjectKey{Namespace: f.cfg.Namespace, Name: clusterName + "-0"}
				if err := leavePending(ctx, c, key, scheduled); err != nil {
					t.Error(err)
				}
			}
			err := f.runner.Run(t.Context(), f.cfg)
			if s := readSummary(t, f.cfg.EvidenceDir).Scenarios[0]; err == nil || s.Result != evidence.Fail {
				t.Fatalf("LAB00 = %+v, %v; want the failure it reports today", s, err)
			}
			if got := readRecord(t, f.cfg.EvidenceDir).UnschedulableMembers; len(got) != 0 {
				t.Fatalf("unschedulable members = %+v, want none", got)
			}
		})
	}
}

// A scheduler refusal does not excuse a namespace the run could not clean up.
func TestRunStaysFailedWhenTheCleanupFailsBesideAnUnschedulableMember(t *testing.T) {
	f := newFixture(t)
	f.cfg.Timeout = 300 * time.Millisecond
	f.afterCluster = func(ctx context.Context, c client.WithWatch) {
		key := client.ObjectKey{Namespace: f.cfg.Namespace, Name: clusterName + "-0"}
		if err := leavePending(ctx, c, key, &testUnschedulable); err != nil {
			t.Error(err)
		}
	}
	f.deleteHook = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		return errors.New("cleanup refused")
	}
	err := f.runner.Run(t.Context(), f.cfg)
	s := readSummary(t, f.cfg.EvidenceDir).Scenarios[0]
	if err == nil || s.Result != evidence.Fail || !strings.Contains(s.Reason, testNoFit) {
		t.Fatalf("LAB00 = %+v, %v; want a failure that still names the scheduler's message", s, err)
	}
}

func TestRunHAIsBlockedWhenAMemberCannotBeScheduled(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.Timeout = time.Second
	f := newHAFixture(t, cfg)
	f.keepPending(testMemberLast, &testUnschedulable)
	err := f.runner.RunHA(t.Context(), cfg)
	requireBlockedByScheduler(t, onlyScenario(t, readSummary(t, cfg.EvidenceDir)), err)
	requireRecordedRefusal(t, readHARecord(t, cfg.EvidenceDir).UnschedulableMembers, testMemberLast)
	if f.client != nil {
		t.Fatal("the workload started without the starting state")
	}
	requireNamespaceGone(t, f)
}

func TestRunHAStaysFailedWhenAMemberIsPendingForAnotherReason(t *testing.T) {
	for name, scheduled := range testPendingOtherwise {
		t.Run(name, func(t *testing.T) {
			cfg := testHAConfig(t)
			cfg.Timeout = time.Second
			f := newHAFixture(t, cfg)
			f.keepPending(testMemberLast, scheduled)
			err := f.runner.RunHA(t.Context(), cfg)
			if s := onlyScenario(t, readSummary(t, cfg.EvidenceDir)); err == nil || s.Result != evidence.Fail {
				t.Fatalf("LAB01 = %+v, %v; want the failure it reports today", s, err)
			}
		})
	}
}

func TestRunS12ExactIsBlockedWhenARestoredMemberCannotBeScheduled(t *testing.T) {
	cfg := testS12Config(t)
	cfg.RestoreLimit = 300 * time.Millisecond
	f := newS12Fixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapRestoring}
	f.keepPending(testRestoreSlave, &testUnschedulable)
	s12, err := s12Of(t, f, t.Context())
	requireBlockedByScheduler(t, s12, err)
	requireRecordedRefusal(t, readHARecord(t, cfg.EvidenceDir).UnschedulableMembers, testRestoreSlave)
}

// The run's own deadline, not restore_limit, ended this wait. The result was
// already blocked; the scheduler's message must still reach the evidence.
func TestRunS12ExactNamesTheSchedulerWhenTheRunEnds(t *testing.T) {
	cfg := testS12Config(t)
	cfg.RestoreLimit = time.Minute
	f := newS12Fixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapRestoring}
	f.keepPending(testRestoreSlave, &testUnschedulable)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.afterRestoreCreated = func(context.Context, client.WithWatch) { cancel() }
	s12, err := s12Of(t, f, ctx)
	requireBlockedByScheduler(t, s12, err)
}

func TestRunS12ExactStaysFailedWhenAMemberIsPendingForAnotherReason(t *testing.T) {
	for name, scheduled := range testPendingOtherwise {
		t.Run(name, func(t *testing.T) {
			cfg := testS12Config(t)
			cfg.RestoreLimit = 300 * time.Millisecond
			f := newS12Fixture(t, cfg)
			f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapRestoring}
			f.keepPending(testRestoreSlave, scheduled)
			s12, err := s12Of(t, f, t.Context())
			if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, "restore_limit") {
				t.Fatalf("S12 = %+v, %v; want the restore limit exceeded", s12, err)
			}
		})
	}
}

// A never-event is a confirmed failure, and a member that does not fit
// explains nothing about it.
func TestRunS12ExactStaysFailedOnANeverEventBesideAnUnschedulableMember(t *testing.T) {
	cfg := testS12Config(t)
	cfg.RestoreLimit = 300 * time.Millisecond
	f := newS12Fixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapRestoring}
	f.tamperRestore = func(read int, s *databasev1alpha1.CubridClusterStatus) {
		if read == 1 {
			s.Conditions[0].Message = "downloading with " + testSecretKey
		}
	}
	f.keepPending(testRestoreSlave, &testUnschedulable)
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || len(s12.NeverEvents) == 0 ||
		!strings.Contains(s12.NeverEvents[0], testCredential) || !strings.Contains(s12.Reason, testNoFit) {
		t.Fatalf("S12 = %+v, %v; want the credential as a failure, naming the scheduler's message too", s12, err)
	}
}

func TestRunS12OperatorRestartIsBlockedWhenARestoredMemberCannotBeScheduled(t *testing.T) {
	cfg := testS12RestartConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12RestartFixture(t, cfg)
	f.restartPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing}
	f.keepPending(clusterMembers(s12RestartClusterName)[2], &testUnschedulable)
	restart, err := restartOf(t, f, t.Context())
	requireBlockedByScheduler(t, restart, err)
}

func TestRunS12InstanceManagerRestartIsBlockedWhenARestoredMemberCannotBeScheduled(t *testing.T) {
	f, records := newS12IMRestartStartingFixture(t)
	records.polled = []instancemanager.OperationState{instancemanager.OpRestoring}
	f.keepPending(clusterMembers(s12IMRestartClusterName)[2], &testUnschedulable)
	restart, err := imRestartOf(t, f)
	requireBlockedByScheduler(t, restart, err)
}

func TestRunS12GracefulStopIsBlockedWhenTheTargetCannotBeScheduled(t *testing.T) {
	cfg := testS12GracefulStopConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12GracefulStopFixture(t, cfg)
	f.keepPending(testGSRTarget, &testUnschedulable)
	stop, err := gracefulStopRestoreOf(t, f)
	requireBlockedByScheduler(t, stop, err)
}

func TestRunS12TamperedIsBlockedWhenARestoredMemberCannotBeScheduled(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12TamperedFixture(t, cfg)
	f.neverRefuse = true
	f.keepPending(clusterMembers(s12TamperedClusterName)[2], &testUnschedulable)
	tampered, err := tamperedOf(t, f, t.Context())
	requireBlockedByScheduler(t, tampered, err)
}

func TestRunS12WrongTargetIsBlockedWhenARecoveryMemberCannotBeScheduled(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12WrongTargetFixture(t, cfg)
	f.neverRefuse = true
	member := clusterMembers(s12WrongTargetClusterName)[2]
	// The first ha-wt, which forms and is deleted, uses the same names.
	f.podRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		f.mu.Lock()
		restoring := f.recovery != nil
		f.mu.Unlock()
		if !restoring || key.Name != member {
			return nil
		}
		return leavePending(ctx, c, key, &testUnschedulable)
	}
	wrong, err := wrongTargetOf(t, f, t.Context())
	requireBlockedByScheduler(t, wrong, err)
}

// The cluster wrong-target forms before its restore has no restore_limit: the
// run's deadline ends that wait.
func TestRunS12WrongTargetNamesTheSchedulerWhenTheFirstClusterCannotForm(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	f := newS12WrongTargetFixture(t, cfg)
	member := clusterMembers(s12WrongTargetClusterName)[2]
	f.keepPending(member, &testUnschedulable)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	created := f.wrongTargetCreated
	f.wrongTargetCreated = func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
		time.AfterFunc(300*time.Millisecond, cancel)
		return created(ctx, c, cluster)
	}
	wrong, err := wrongTargetOf(t, f, ctx)
	requireBlockedByScheduler(t, wrong, err)
}
