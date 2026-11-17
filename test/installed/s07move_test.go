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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

// The tests in this file cover how the network partition lane moves the
// master off the first member before it cuts off a member that outranks the
// master (higher-rank-isolated): the deletion of the master's Pod, what the
// run waits for, and that any refusal blocks the scenario before anything is
// cut or any client starts.

// failOver plays CUBRID and the Operator after the first member's Pod was
// replaced: the second member becomes the active master and the cluster's
// resolved primary, and the first member a slave.
func failOver(f *haFixture, c client.WithWatch) {
	f.mu.Lock()
	f.roles[m0], f.roles[m1] = roleSlave, roleMaster
	f.mu.Unlock()
	setS07Primary(f.t, f, c, m1)
}

// setS07Quarantine writes status.writeQuarantine, nil to release it.
func setS07Quarantine(t *testing.T, f *haFixture, c client.Client, q *databasev1alpha1.WriteQuarantine) {
	t.Helper()
	cluster := &databasev1alpha1.CubridCluster{}
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		t.Error(err)
		return
	}
	cluster.Status.WriteQuarantine = q
	if err := c.Update(ctx, cluster); err != nil {
		t.Error(err)
	}
}

func memberDeletes(f *haFixture) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.writes {
		if w == "delete-member-pod" {
			n++
		}
	}
	return n
}

// Before it cuts off a member that outranks the master, the run moves the
// master off the first member by a graceful deletion of its Pod; CUBRID, not
// the run, picks the new master. The run goes on only once another member is
// the active master, the first member is back in a new Pod as a slave, and
// the cluster names the new master as its current and last resolved primary.
// LAB01's member identities are kept; the new ones are the move's own.
func TestMoveS07MasterWaitsForCUBRIDToFailOver(t *testing.T) {
	h, f := s07MoveFixture(t, failOver)
	lab01 := []imageIdentity{{Pod: m0, UID: m0}, {Pod: m1, UID: m1}, {Pod: m2, UID: m2}}
	rec := &s07Record{Variant: s07HigherRankIsolated, Master: m0, MembersBefore: slices.Clone(lab01)}
	slaves, err := h.moveS07Master(context.Background(), rec)
	if err != nil {
		t.Fatalf("the move was refused: %v", err)
	}
	if rec.Master != m1 || len(slaves) != 2 || !slices.Contains(slaves, m0) || !slices.Contains(slaves, m2) {
		t.Errorf("master %s and slaves %v after the move, want %s with %s and %s", rec.Master, slaves, m1, m0, m2)
	}
	move := rec.MasterMove
	if move == nil || move.From != m0 || move.FromUID != m0 || move.To != m1 || move.DeletedAt == nil ||
		move.SettledAt == nil || move.Unmet != "" || move.Quarantine != nil {
		t.Fatalf("move %+v", move)
	}
	if !slices.Equal(rec.MembersBefore, lab01) {
		t.Errorf("LAB01's member identities were changed: %+v", rec.MembersBefore)
	}
	i := slices.IndexFunc(move.Members, func(m imageIdentity) bool { return m.Pod == m0 })
	if len(move.Members) != 3 || i < 0 || move.Members[i].UID != m0+"-replaced" {
		t.Errorf("members after the move %+v, want the first member's new Pod", move.Members)
	}
	if memberDeletes(f) != 1 {
		t.Errorf("%d member Pod deletions, want 1", memberDeletes(f))
	}
}

// When the first member comes back as the master, or CUBRID does not fail
// over, the master did not move: the run refuses at the bound, says what did
// not hold, and deletes no other Pod.
func TestMoveS07MasterRefusesWhenTheFirstMemberStaysMaster(t *testing.T) {
	old := s07MoveBound
	s07MoveBound = 100 * time.Millisecond
	t.Cleanup(func() { s07MoveBound = old })
	h, f := s07MoveFixture(t, func(*haFixture, client.WithWatch) {})
	rec := &s07Record{Variant: s07HigherRankIsolated, Master: m0}
	_, err := h.moveS07Master(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "did not move") {
		t.Fatalf("the move with the first member still master: %v", err)
	}
	if rec.Master != m0 || rec.MasterMove == nil || rec.MasterMove.SettledAt != nil ||
		!strings.Contains(rec.MasterMove.Unmet, m0) {
		t.Errorf("master %s, move %+v", rec.Master, rec.MasterMove)
	}
	if memberDeletes(f) != 1 {
		t.Errorf("%d member Pod deletions, want 1", memberDeletes(f))
	}
}

// The deletion of the master's Pod can itself start a write quarantine, as
// #938 saw on Kind. The cut must start from a cluster with none in force, so
// the move waits for its release; one that stays blocks S07 at the bound,
// even with the conditions reported True.
func TestMoveS07MasterWaitsForAQuarantineItStarted(t *testing.T) {
	old := s07MoveBound
	s07MoveBound = 100 * time.Millisecond
	t.Cleanup(func() { s07MoveBound = old })
	h, _ := s07MoveFixture(t, func(f *haFixture, c client.WithWatch) {
		failOver(f, c)
		setS07Quarantine(t, f, c, &databasev1alpha1.WriteQuarantine{GenerationID: "g1",
			Reason: databasev1alpha1.WriteQuarantinedMemberUnverified})
	})
	rec := &s07Record{Variant: s07HigherRankIsolated, Master: m0}
	_, err := h.moveS07Master(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "WriteQuarantinedMemberUnverified") {
		t.Fatalf("the move with a quarantine in force: %v", err)
	}
	q := rec.MasterMove.Quarantine
	if rec.MasterMove.SettledAt != nil || q == nil || q.GenerationID != "g1" || q.ReleasedAt != nil {
		t.Errorf("move %+v, quarantine %+v", rec.MasterMove, q)
	}
}

// A quarantine that only a person may release cannot end by itself: the
// move blocks at once instead of waiting for its bound.
func TestMoveS07MasterBlocksAtOnceOnAManualOnlyQuarantine(t *testing.T) {
	h, _ := s07MoveFixture(t, func(f *haFixture, c client.WithWatch) {
		failOver(f, c)
		setS07Quarantine(t, f, c, &databasev1alpha1.WriteQuarantine{GenerationID: "g1",
			Reason: databasev1alpha1.WriteQuarantinedMasterChanged})
	})
	rec := &s07Record{Variant: s07HigherRankIsolated, Master: m0}
	start := time.Now()
	_, err := h.moveS07Master(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "only a person may release") {
		t.Fatalf("the move with a manual-only quarantine: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the move waited %s for a quarantine no automatic step releases", time.Since(start))
	}
	if q := rec.MasterMove.Quarantine; q == nil || q.Reason != string(databasev1alpha1.WriteQuarantinedMasterChanged) {
		t.Errorf("quarantine %+v not recorded", q)
	}
}

// A quarantine the move started and the Operator released before the move
// settled is kept in the record with its release, and the move settles.
func TestMoveS07MasterRecordsAQuarantineReleasedBeforeItSettled(t *testing.T) {
	h, f := s07MoveFixture(t, func(f *haFixture, c client.WithWatch) {
		failOver(f, c)
		setS07Quarantine(t, f, c, &databasev1alpha1.WriteQuarantine{GenerationID: "g1",
			Reason: databasev1alpha1.WriteQuarantinedMemberUnverified})
	})
	reads := 0
	f.haClusterRead = func(ctx context.Context, c client.WithWatch, _ client.ObjectKey) error {
		reads++
		if reads == 4 {
			setS07Quarantine(t, f, c, nil)
		}
		return nil
	}
	rec := &s07Record{Variant: s07HigherRankIsolated, Master: m0}
	if _, err := h.moveS07Master(context.Background(), rec); err != nil {
		t.Fatalf("the move after the quarantine's release: %v", err)
	}
	q := rec.MasterMove.Quarantine
	if q == nil || q.GenerationID != "g1" || q.ReleasedAt == nil || q.ReleasedAt.Before(q.SeenAt) ||
		rec.MasterMove.SettledAt == nil {
		t.Errorf("move %+v, quarantine %+v", rec.MasterMove, q)
	}
	kinds := make([]string, 0, len(rec.Observed))
	for _, o := range rec.Observed {
		kinds = append(kinds, o.Kind)
	}
	if !slices.Contains(kinds, "master_move_quarantine_seen") ||
		!slices.Contains(kinds, "master_move_quarantine_released") {
		t.Errorf("observed %v", kinds)
	}
}

// The run deletes nothing when its namespace guard fails, when the Pod is
// not controlled by the run's HA StatefulSet, or when the master is not the
// first member.
func TestMoveS07MasterDeletesNothingItMayNotDelete(t *testing.T) {
	tests := map[string]struct {
		change func(h *haRun, f *haFixture)
		master string
		want   string
	}{
		"the namespace guard fails": {func(h *haRun, _ *haFixture) {
			h.guard = func(context.Context) error { return errors.New("the run namespace was replaced") }
		}, m0, "the run namespace was replaced"},
		"a Pod the HA StatefulSet does not control": {func(_ *haRun, f *haFixture) {
			ctx := context.Background()
			pod := &corev1.Pod{}
			if err := f.store.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: m0}, pod); err != nil {
				t.Fatal(err)
			}
			pod.OwnerReferences = nil
			if err := f.store.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
		}, m0, "does not belong to the HA StatefulSet"},
		"a master that is not the first member": {func(*haRun, *haFixture) {}, m1, "not the first member"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h, f := s07MoveFixture(t, failOver)
			tc.change(h, f)
			rec := &s07Record{Variant: s07HigherRankIsolated, Master: tc.master}
			_, err := h.moveS07Master(context.Background(), rec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want %q", err, tc.want)
			}
			if memberDeletes(f) != 0 {
				t.Errorf("%d member Pod deletions, want none", memberDeletes(f))
			}
		})
	}
}

// s07HigherRankRun is the fixture of the move set up to run S07
// higher-rank-isolated after LAB01 with master as LAB01's master.
func s07HigherRankRun(t *testing.T, master string, failover func(f *haFixture, c client.WithWatch)) (*haRun,
	*haFixture) {
	t.Helper()
	h, f := s07MoveFixture(t, failover)
	h.cfg.Fault, h.cfg.EvidenceDir = FaultPartitionHigherRankIsolated, t.TempDir()
	h.redactor = evidence.NewRedactor()
	slaves := slices.DeleteFunc(slices.Clone(testS07Members), func(m string) bool { return m == master })
	h.rec.StartingState = &startingState{Master: master, Slaves: slaves}
	h.rec.MembersAfter = []imageIdentity{{Pod: m0, UID: m0}, {Pod: m1, UID: m1}, {Pod: m2, UID: m2}}
	if master != m0 {
		f.roles = map[string]string{m0: roleSlave, m1: roleSlave, m2: roleSlave}
		f.roles[master] = roleMaster
		setS07Primary(t, f, f.store, master)
	}
	return h, f
}

func readS07Record(t *testing.T, h *haRun) *s07Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.cfg.EvidenceDir, s07File(s07HigherRankIsolated, "record.json")))
	if err != nil {
		t.Fatal(err)
	}
	rec := &s07Record{}
	if err := json.Unmarshal(data, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// requireNothingCut checks that S07 ended blocked before any fault or client:
// no partition was built, no ephemeral container added and no command run
// in the workload client.
func requireNothingCut(t *testing.T, h *haRun, f *haFixture, result evidence.Scenario) *s07Record {
	t.Helper()
	if result.Result != evidence.Blocked {
		t.Errorf("result %s %q, want blocked", result.Result, result.Reason)
	}
	if result.FaultConfirmed != nil || result.CleanupSucceeded != nil {
		t.Errorf("fault confirmed %v, cleanup %v: a fault was reported", result.FaultConfirmed, result.CleanupSucceeded)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.Contains(f.writes, "ephemeral-container") {
		t.Errorf("writes %v: a fault container was added", f.writes)
	}
	for _, ran := range f.ran {
		if strings.Contains(ran, " exec "+clientPodName+" ") || strings.Contains(ran, netfault.Container) {
			t.Errorf("ran %q before the cut could start", ran)
		}
	}
	rec := readS07Record(t, h)
	if rec.Partition != nil {
		t.Errorf("a partition was built: %+v", rec.Partition)
	}
	return rec
}

// Every refusal of the move blocks S07 through the scenario itself, never
// fails it, and leaves nothing cut and no client started.
func TestRunS07BlocksWhenTheMasterDoesNotMove(t *testing.T) {
	old := s07MoveBound
	s07MoveBound = 100 * time.Millisecond
	t.Cleanup(func() { s07MoveBound = old })
	refused := map[string]error{
		"a UID conflict": apierrors.NewConflict(corev1.Resource("pods"), m0,
			errors.New("the Pod's UID changed")),
		"a forbidden deletion": apierrors.NewForbidden(corev1.Resource("pods"), m0,
			errors.New("no delete on Pods")),
	}
	for name, refusal := range refused {
		t.Run(name, func(t *testing.T) {
			h, f := s07HigherRankRun(t, m0, failOver)
			f.podDeleted = func(context.Context, client.WithWatch, *corev1.Pod, ...client.DeleteOption) error {
				return refusal
			}
			rec := requireNothingCut(t, h, f, h.runS07(context.Background()))
			if rec.MasterMove == nil || rec.MasterMove.DeletedAt != nil {
				t.Errorf("move %+v, want one with no deletion", rec.MasterMove)
			}
		})
	}
	t.Run("the first member stays master", func(t *testing.T) {
		h, f := s07HigherRankRun(t, m0, func(*haFixture, client.WithWatch) {})
		result := h.runS07(context.Background())
		requireNothingCut(t, h, f, result)
		if !strings.Contains(result.Reason, "did not move") {
			t.Errorf("reason %q", result.Reason)
		}
	})
	t.Run("the run's context ends during the wait", func(t *testing.T) {
		s07MoveBound = time.Minute
		ctx, cancel := context.WithCancel(context.Background())
		h, f := s07HigherRankRun(t, m0, func(*haFixture, client.WithWatch) { time.AfterFunc(20*time.Millisecond, cancel) })
		start := time.Now()
		requireNothingCut(t, h, f, h.runS07(ctx))
		if time.Since(start) > 10*time.Second {
			t.Errorf("the scenario waited %s after its context ended", time.Since(start))
		}
	})
}

// With the master already off the first member, the run deletes nothing and
// cuts off the first member, which outranks it; LAB01's identities name it.
func TestRunS07CutsTheFirstMemberWithoutAMoveWhenTheMasterIsElsewhere(t *testing.T) {
	for _, master := range []string{m1, m2} {
		t.Run(master, func(t *testing.T) {
			h, f := s07HigherRankRun(t, master, failOver)
			// The member Pods have no IP in the fixture, so the run stops,
			// blocked, when it reads the target's address: after it picked
			// the target and before anything is cut.
			rec := requireNothingCut(t, h, f, h.runS07(context.Background()))
			if memberDeletes(f) != 0 || rec.MasterMove != nil {
				t.Errorf("%d deletions, move %+v", memberDeletes(f), rec.MasterMove)
			}
			if rec.Target.Pod != m0 || rec.Target.UID != m0 {
				t.Errorf("target %+v, want %s from LAB01", rec.Target, m0)
			}
		})
	}
}

// The variant expects a quarantine: a trigger in two sweeps with no
// quarantine recorded fails it, as it fails master-isolated.
func TestJudgeS07HigherRankIsolatedFailsWithoutAQuarantine(t *testing.T) {
	rec := s07Base(false)
	rec.Variant, rec.Master, rec.LastResolvedPrimary, rec.Target = s07HigherRankIsolated, m1, m1,
		imageIdentity{Pod: m0}
	rec.QuarantineSeenAt, rec.Revisions = nil, nil
	v := judgeS07(rec, testS07Limits)
	if v.Result != evidence.Fail || !strings.Contains(v.Reason, "no write quarantine was recorded") {
		t.Errorf("judged %s %q", v.Result, v.Reason)
	}
}

// The variant's run must leave room for its own steps beyond LAB01.
func TestValidateS07HigherRankIsolatedTimeout(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.NamespaceMode, cfg.Level, cfg.StablePeriod = FreshNamespace, evidence.LevelVMLab, time.Second
	cfg.Fault, cfg.PartitionHold = FaultPartitionHigherRankIsolated, 2*time.Minute
	cfg.Timeout = cfg.s07StepBounds()
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "-timeout") {
		t.Errorf("a timeout that only covers the steps' bounds: %v", err)
	}
	cfg.Timeout = 45 * time.Minute
	if err := cfg.validate(); err != nil {
		t.Errorf("a 45-minute timeout is refused: %v", err)
	}
	cfg.Fault, cfg.Timeout = FaultPartitionMasterIsolated, time.Second
	if err := cfg.validate(); err != nil {
		t.Errorf("master-isolated is now refused on its timeout: %v", err)
	}
}
