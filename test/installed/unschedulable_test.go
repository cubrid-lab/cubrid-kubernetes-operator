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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const testNoFit = "0/4 nodes are available: 3 Insufficient memory, 1 node(s) were unschedulable"

// testRefusal is a scheduler refusal with message that has held for held.
func testRefusal(message string, held time.Duration) *corev1.PodCondition {
	return &corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: message,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-held))}
}

// A refusal for capacity that has held well past the minimum, which blocks.
var testUnschedulable = *testRefusal(testNoFit, time.Hour)

// Scheduler refusals that do not name memory or CPU: they point at the Pod the
// Operator rendered or at the cluster's storage, not at nodes that are full.
var testOtherRefusals = map[string]string{
	"anti-affinity": "0/4 nodes are available: 1 node(s) were unschedulable, 3 node(s) didn't match pod " +
		"anti-affinity rules",
	"unbound claim": "0/4 nodes are available: pod has unbound immediate PersistentVolumeClaims",
	"node selector": "0/4 nodes are available: 4 node(s) didn't match Pod's node affinity/selector",
}

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

// requireWaitVerdict requires the failure the wait gives on its own. The wait
// does not read a member it does not depend on, so its refusal is not reported.
func requireWaitVerdict(t *testing.T, s evidence.Scenario, err error, verdict string) {
	t.Helper()
	if err == nil || s.Result != evidence.Fail || !strings.Contains(s.Reason, verdict) ||
		strings.Contains(s.Reason, testNoFit) {
		t.Fatalf("%s = %+v, %v; want the failure %q alone", s.Name(), s, err, verdict)
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

func TestRunS12OperatorRestartIsBlockedWhenTheRestoreTargetCannotBeScheduled(t *testing.T) {
	cfg := testS12RestartConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12RestartFixture(t, cfg)
	f.restartPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing}
	f.keepPending(clusterMembers(s12RestartClusterName)[0], &testUnschedulable)
	restart, err := restartOf(t, f, t.Context())
	requireBlockedByScheduler(t, restart, err)
}

// The restore that must start runs on the first member alone, so a later
// member the scheduler cannot place explains nothing about it.
func TestRunS12OperatorRestartKeepsItsVerdictWhenAnotherMemberCannotBeScheduled(t *testing.T) {
	cfg := testS12RestartConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12RestartFixture(t, cfg)
	f.restartPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapPreparing}
	f.keepPending(clusterMembers(s12RestartClusterName)[2], &testUnschedulable)
	restart, err := restartOf(t, f, t.Context())
	requireWaitVerdict(t, restart, err, "restore_limit")
}

func TestRunS12InstanceManagerRestartIsBlockedWhenTheRestoreTargetCannotBeScheduled(t *testing.T) {
	f, records := newS12IMRestartStartingFixture(t)
	records.polled = []instancemanager.OperationState{instancemanager.OpRestoring}
	f.keepPending(clusterMembers(s12IMRestartClusterName)[0], &testUnschedulable)
	restart, err := imRestartOf(t, f)
	requireBlockedByScheduler(t, restart, err)
}

// The restored database starts on the first member alone.
func TestRunS12InstanceManagerRestartKeepsItsVerdictWhenAnotherMemberCannotBeScheduled(t *testing.T) {
	f, records := newS12IMRestartStartingFixture(t)
	records.polled = []instancemanager.OperationState{instancemanager.OpRestoring}
	f.keepPending(clusterMembers(s12IMRestartClusterName)[2], &testUnschedulable)
	restart, err := imRestartOf(t, f)
	requireWaitVerdict(t, restart, err, "restore_limit")
}

func TestRunS12GracefulStopIsBlockedWhenTheTargetCannotBeScheduled(t *testing.T) {
	cfg := testS12GracefulStopConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12GracefulStopFixture(t, cfg)
	f.keepPending(testGSRTarget, &testUnschedulable)
	stop, err := gracefulStopRestoreOf(t, f)
	requireBlockedByScheduler(t, stop, err)
}

func TestRunS12GracefulStopKeepsItsVerdictWhenAnotherMemberCannotBeScheduled(t *testing.T) {
	cfg := testS12GracefulStopConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12GracefulStopFixture(t, cfg)
	f.keepPending(testGSRTarget, &corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue})
	f.keepPending(clusterMembers(s12GracefulStopClusterName)[2], &testUnschedulable)
	stop, err := gracefulStopRestoreOf(t, f)
	requireWaitVerdict(t, stop, err, "restore_limit")
}

func TestRunS12TamperedIsBlockedWhenTheRestoreTargetCannotBeScheduled(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12TamperedFixture(t, cfg)
	f.neverRefuse = true
	f.keepPending(clusterMembers(s12TamperedClusterName)[0], &testUnschedulable)
	tampered, err := tamperedOf(t, f, t.Context())
	requireBlockedByScheduler(t, tampered, err)
}

// The refusal runs on the first member alone: with it healthy, a later member
// the scheduler cannot place does not explain a tampered copy that was not
// refused.
func TestRunS12TamperedKeepsItsVerdictWhenAnotherMemberCannotBeScheduled(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12TamperedFixture(t, cfg)
	f.neverRefuse = true
	f.keepPending(clusterMembers(s12TamperedClusterName)[2], &testUnschedulable)
	tampered, err := tamperedOf(t, f, t.Context())
	requireWaitVerdict(t, tampered, err, "was not refused within restore_limit")
}

// keepRecoveryPending leaves member Pending only once the recovery cluster
// exists: the first ha-wt, which forms and is deleted, uses the same names.
func keepRecoveryPending(f *s12WrongTargetFixture, member string) {
	f.podRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		f.mu.Lock()
		restoring := f.recovery != nil
		f.mu.Unlock()
		if !restoring || key.Name != member {
			return nil
		}
		return leavePending(ctx, c, key, &testUnschedulable)
	}
}

func TestRunS12WrongTargetIsBlockedWhenTheRestoreTargetCannotBeScheduled(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12WrongTargetFixture(t, cfg)
	f.neverRefuse = true
	keepRecoveryPending(f, clusterMembers(s12WrongTargetClusterName)[0])
	wrong, err := wrongTargetOf(t, f, t.Context())
	requireBlockedByScheduler(t, wrong, err)
}

// The refusal runs on the first member alone.
func TestRunS12WrongTargetKeepsItsVerdictWhenAnotherMemberCannotBeScheduled(t *testing.T) {
	cfg := testS12WrongTargetConfig(t)
	cfg.RestoreLimit = time.Second
	f := newS12WrongTargetFixture(t, cfg)
	f.neverRefuse = true
	keepRecoveryPending(f, clusterMembers(s12WrongTargetClusterName)[2])
	wrong, err := wrongTargetOf(t, f, t.Context())
	requireWaitVerdict(t, wrong, err, "was not refused within restore_limit")
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

// runWithRefusedMember runs LAB00 with its member refused by scheduled.
func runWithRefusedMember(t *testing.T, scheduled *corev1.PodCondition) (*fixture, evidence.Scenario, error) {
	t.Helper()
	f := newFixture(t)
	f.cfg.Timeout = 300 * time.Millisecond
	f.afterCluster = func(ctx context.Context, c client.WithWatch) {
		key := client.ObjectKey{Namespace: f.cfg.Namespace, Name: clusterName + "-0"}
		if err := leavePending(ctx, c, key, scheduled); err != nil {
			t.Error(err)
		}
	}
	err := f.runner.Run(t.Context(), f.cfg)
	return f, readSummary(t, f.cfg.EvidenceDir).Scenarios[0], err
}

// Only a refusal for memory or CPU shows full nodes. Any other can come from
// the Pod the Operator rendered, so the wait keeps its verdict, and the
// scheduler's message stays in the evidence for whoever reads the failure.
func TestRunStaysFailedWhenTheSchedulerRefusesForAnotherReason(t *testing.T) {
	for name, message := range testOtherRefusals {
		t.Run(name, func(t *testing.T) {
			f, s, err := runWithRefusedMember(t, testRefusal(message, time.Hour))
			if err == nil || s.Result != evidence.Fail || !strings.Contains(s.Reason, message) {
				t.Fatalf("LAB00 = %+v, %v; want a failure that names the scheduler's message", s, err)
			}
			got := readRecord(t, f.cfg.EvidenceDir).UnschedulableMembers
			if len(got) != 1 || got[0].Message != message || got[0].BlocksRun {
				t.Fatalf("unschedulable members = %+v, want the refusal recorded without blocking", got)
			}
		})
	}
}

// The scheduler sums up its nodes: some nodes being full is enough, whatever
// the others refused for.
func TestRunIsBlockedByARefusalThatAlsoNamesCapacity(t *testing.T) {
	for _, message := range []string{
		"0/4 nodes are available: 1 Insufficient memory, 2 didn't match pod anti-affinity rules",
		"0/4 nodes are available: 1 Insufficient cpu, 3 node(s) didn't match Pod's node affinity/selector",
	} {
		t.Run(message, func(t *testing.T) {
			f, s, err := runWithRefusedMember(t, testRefusal(message, time.Hour))
			if err == nil || s.Result != evidence.Blocked || !strings.Contains(s.Reason, message) {
				t.Fatalf("LAB00 = %+v, %v; want blocked with the scheduler's message", s, err)
			}
			got := readRecord(t, f.cfg.EvidenceDir).UnschedulableMembers
			if len(got) != 1 || !got[0].BlocksRun {
				t.Fatalf("unschedulable members = %+v, want the refusal recorded as blocking", got)
			}
		})
	}
}

// One reading at the deadline must not decide on a refusal seconds old: the
// scheduler may yet place the member, so the run has not shown it does not
// fit. JSON keeps whole seconds, hence the margin around the boundary.
func TestRefusalBlocksOnlyOnceItHasHeld(t *testing.T) {
	const margin = 2 * time.Second
	for name, tc := range map[string]struct {
		held   time.Duration
		blocks bool
	}{
		"just under the minimum": {held: refusalMinimumHold - margin, blocks: false},
		"just over the minimum":  {held: refusalMinimumHold + margin, blocks: true},
	} {
		t.Run(name, func(t *testing.T) {
			scheduled := testRefusal(testNoFit, tc.held)
			f, s, err := runWithRefusedMember(t, scheduled)
			want := evidence.Fail
			if tc.blocks {
				want = evidence.Blocked
			}
			if err == nil || s.Result != want || !strings.Contains(s.Reason, testNoFit) {
				t.Fatalf("LAB00 = %+v, %v; want %s naming the scheduler's message", s, err, want)
			}
			got := readRecord(t, f.cfg.EvidenceDir).UnschedulableMembers
			if len(got) != 1 || got[0].BlocksRun != tc.blocks || got[0].PodUID == "" ||
				!got[0].RefusedSince.Time.Equal(scheduled.LastTransitionTime.Rfc3339Copy().Time) ||
				got[0].ReadAt.IsZero() {
				t.Fatalf("unschedulable members = %+v, want the Pod UID, when the refusal began and when it was read",
					got)
			}
		})
	}
}

// How long a refusal without a transition time has held is unknown, and an
// unknown hold is not taken as long enough.
func TestRefusalWithoutATransitionTimeDoesNotBlock(t *testing.T) {
	scheduled := testRefusal(testNoFit, 0)
	scheduled.LastTransitionTime = metav1.Time{}
	f, s, err := runWithRefusedMember(t, scheduled)
	if err == nil || s.Result != evidence.Fail {
		t.Fatalf("LAB00 = %+v, %v; want the failure the wait gives", s, err)
	}
	if got := readRecord(t, f.cfg.EvidenceDir).UnschedulableMembers; len(got) != 1 || got[0].BlocksRun {
		t.Fatalf("unschedulable members = %+v, want the refusal recorded without blocking", got)
	}
}

// restore_limit ends a wait only when neither the run nor a verdict did.
func TestLimitEnded(t *testing.T) {
	ended, end := context.WithCancel(t.Context())
	end()
	verdict := restoreVerdict{errors.New("the bootstrap ended Failed")}
	for name, tc := range map[string]struct {
		ctx, wctx context.Context
		err       error
		want      bool
	}{
		"the limit passed":                   {t.Context(), ended, context.DeadlineExceeded, true},
		"a verdict read across the limit":    {t.Context(), ended, verdict, false},
		"a wrapped verdict across the limit": {t.Context(), ended, fmt.Errorf("read: %w", verdict), false},
		"the run ended":                      {ended, ended, context.Canceled, false},
		"the wait goes on with an error":     {t.Context(), t.Context(), errors.New("refused"), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := limitEnded(tc.ctx, tc.wctx, tc.err); got != tc.want {
				t.Fatalf("limitEnded = %t, want %t", got, tc.want)
			}
		})
	}
}

// A verdict read by a reading that spans restore_limit is a verdict: the
// limit, and a member the scheduler cannot place, must not replace it.
func TestRunS12ExactKeepsAVerdictReadAcrossTheLimit(t *testing.T) {
	cfg := testS12Config(t)
	cfg.RestoreLimit = 300 * time.Millisecond
	f := newS12Fixture(t, cfg)
	f.bootPhases = []databasev1alpha1.BootstrapPhase{databasev1alpha1.BootstrapRestoring}
	f.tamperRestore = func(_ int, s *databasev1alpha1.CubridClusterStatus) {
		time.Sleep(cfg.RestoreLimit)
		s.Bootstrap.Phase = databasev1alpha1.BootstrapFailed
	}
	f.keepPending(testRestoreSlave, &testUnschedulable)
	s12, err := s12Of(t, f, t.Context())
	if err == nil || s12.Result != evidence.Fail || !strings.Contains(s12.Reason, "bootstrap ended Failed") {
		t.Fatalf("S12 = %+v, %v; want the bootstrap's failure", s12, err)
	}
}

func TestRunS12TamperedKeepsAVerdictReadAcrossTheLimit(t *testing.T) {
	cfg := testS12TamperedConfig(t)
	cfg.RestoreLimit = 300 * time.Millisecond
	f := newS12TamperedFixture(t, cfg)
	f.neverRefuse = true
	f.tamperTampered = func(_ int, s *databasev1alpha1.CubridClusterStatus) {
		time.Sleep(cfg.RestoreLimit)
		s.Bootstrap.Phase = databasev1alpha1.BootstrapFailed
	}
	f.keepPending(clusterMembers(s12TamperedClusterName)[0], &testUnschedulable)
	tampered, err := tamperedOf(t, f, t.Context())
	if err == nil || tampered.Result != evidence.Fail {
		t.Fatalf("S12 tampered = %+v, %v; want the bootstrap's failure", tampered, err)
	}
}

// An error read as the run's deadline passed did not come from that deadline:
// it stays what it is, however the members stand.
func TestRunHAKeepsAnErrorReadAcrossTheDeadline(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.Timeout = 500 * time.Millisecond
	f := newHAFixture(t, cfg)
	f.keepPending(testMemberLast, &testUnschedulable)
	pending := f.podRead
	var once sync.Once
	f.podRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		var failure error
		once.Do(func() {
			<-ctx.Done()
			failure = errors.New("the member's Pod was replaced")
		})
		if failure != nil {
			return failure
		}
		return pending(ctx, c, key)
	}
	err := f.runner.RunHA(t.Context(), cfg)
	if s := onlyScenario(t, readSummary(t, cfg.EvidenceDir)); err == nil || s.Result != evidence.Fail ||
		!strings.Contains(s.Reason, "the member's Pod was replaced") {
		t.Fatalf("LAB01 = %+v, %v; want the error read at the deadline as a failure", s, err)
	}
}
