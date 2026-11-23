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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The tests in this file cover the rebuild after the loss of a member's
// volume on the installed Operator (scenario S10): after LAB01, the run
// replaces a slave's data claim and Pod with their UIDs as preconditions,
// confirms the loss, requests the rebuild the Operator offers while a client
// writes, and judges the rebuilt member's rows, its applier and the other
// members' claims. The S10 in the test names is the scenario identifier, kept
// for traceability to the scenario contract.

const (
	testOfferToken   = "offer-token-value"
	testReleaseToken = "release-token-value"
	testQuarantine   = "quarantine-generation-1"
	testRebuildOp    = "rebuild-operation-1"
)

func testS10Config(t *testing.T) HAConfig {
	t.Helper()
	shortS10Bounds(t)
	cfg := testHAConfig(t)
	cfg.Level = evidence.LevelVMLab
	cfg.Fault = FaultPVCLossRebuild
	cfg.StablePeriod = time.Second
	cfg.Timeout = time.Hour
	return cfg
}

// shortS10Bounds shortens S10's own bounds for the test, so that a step
// that never ends ends the wait quickly.
func shortS10Bounds(t *testing.T) {
	t.Helper()
	saved := []time.Duration{s10ReplaceBound, s10ListBound, s10OfferBound, s10RebuildBound, s10SlaveBound}
	s10ReplaceBound, s10ListBound, s10OfferBound, s10RebuildBound, s10SlaveBound =
		50*time.Millisecond, 50*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		s10ReplaceBound, s10ListBound, s10OfferBound, s10RebuildBound, s10SlaveBound =
			saved[0], saved[1], saved[2], saved[3], saved[4]
	})
}

// s10Fake plays the API server, the StatefulSet and the Operator around a
// rebuild. Its knobs change one answer each.
type s10Fake struct {
	mu sync.Mutex
	// keepClaim leaves the claim in place when the run deletes it.
	keepClaim bool
	// listing is the verdict line the new volume's listing prints.
	listing string
	// noRelease never ends the write quarantine.
	noRelease bool
	// rebuiltMember and source are what the ended rebuild names, the
	// target and the master unless set; state is its end state.
	rebuiltMember, source string
	state                 databasev1alpha1.MemberRebuildState
	// neverEnds keeps the rebuild in progress.
	neverEnds bool
	// replaceOther replaces the claim of this member when the rebuild ends.
	replaceOther string
	// failCount is the rebuilt member's applier fail count.
	failCount int
	// onRequest runs when the rebuild is requested.
	onRequest func()

	member, master     string
	accepted           time.Time
	claimDeleted       bool
	podReplaced        bool
	released, finished bool
	requests           []string
	deletes            []string
}

// newS10Fixture plays an HA cluster whose master is master, with data claims,
// an Operator that offers and completes the rebuild of the slave S10 chooses,
// and a client acknowledged throughout the rebuild.
func newS10Fixture(t *testing.T, cfg HAConfig, master string) (*haFixture, *s10Fake) {
	t.Helper()
	f := newHAFixture(t, cfg)
	f.dataClaims = true
	f.primary = master
	f.roles = map[string]string{testMemberHA0: roleSlave, testHAPrimary: roleSlave, testMemberLast: roleSlave}
	f.roles[master] = roleMaster
	slaves := slices.DeleteFunc(clusterMembers(haClusterName), func(m string) bool { return m == master })
	member, _ := rebuildTarget(master, slaves, clusterMembers(haClusterName))
	fake := &s10Fake{member: member, master: master, listing: "verdict: no-database"}
	f.claimDeleted = fake.deleteClaim(t)
	f.podDeleted = fake.deletePod(t)
	f.haClusterRead = fake.operator(t)
	f.memberHook = func(_ context.Context, pod, script string) (string, error, bool) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case strings.Contains(script, "CUBRID_DATABASES"):
			if pod != member {
				t.Errorf("the new volume of %s listed on %s", member, pod)
			}
			return "directory: /var/lib/cubrid/databases, database: appdb\n" + fake.listing + "\n", nil, true
		case strings.Contains(script, "applyinfo") && fake.finished:
			return fmt.Sprintf(" *** Applied Info. *** \n Fail count : %d\n", fake.failCount), nil, true
		}
		return "", nil, false
	}
	f.backgroundHistory = func(id string) string {
		fake.mu.Lock()
		at := fake.acceptedAt().Add(2 * time.Second)
		fake.mu.Unlock()
		history, rows := backgroundWorkloadFrom(id, nil, at)
		f.mu.Lock()
		f.rows = append(f.rows, rows...)
		f.mu.Unlock()
		return history
	}
	return f, fake
}

// acceptedAt is when the fake Operator accepted the request, to the second
// as the API server keeps it, taken once; the client's answers fall inside
// the rebuild it records.
func (s *s10Fake) acceptedAt() time.Time {
	if s.accepted.IsZero() {
		s.accepted = time.Now().UTC().Truncate(time.Second)
	}
	return s.accepted
}

// requireUID fails unless the delete names the object's UID as a
// precondition.
func requireUID(t *testing.T, what string, uid types.UID, opts []client.DeleteOption) {
	t.Helper()
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != uid {
		t.Errorf("%s deleted without its UID %s as a precondition: %+v", what, uid, options.Preconditions)
	}
}

func (s *s10Fake) deleteClaim(t *testing.T) func(context.Context, client.WithWatch, *corev1.PersistentVolumeClaim,
	...client.DeleteOption) error {
	return func(ctx context.Context, c client.WithWatch, pvc *corev1.PersistentVolumeClaim,
		opts ...client.DeleteOption) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.deletes = append(s.deletes, "claim "+pvc.Name)
		if pvc.Name != dataVolumePrefix+s.member {
			t.Errorf("the run deleted the claim %s, not the target's", pvc.Name)
		}
		requireUID(t, "the claim "+pvc.Name, types.UID("claim-"+s.member), opts)
		if s.keepClaim {
			return nil
		}
		s.claimDeleted = true
		return c.Delete(ctx, pvc, opts...)
	}
}

func (s *s10Fake) deletePod(t *testing.T) func(context.Context, client.WithWatch, *corev1.Pod,
	...client.DeleteOption) error {
	return func(ctx context.Context, c client.WithWatch, pod *corev1.Pod, opts ...client.DeleteOption) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.deletes = append(s.deletes, "pod "+pod.Name)
		if pod.Name != s.member {
			t.Errorf("the run deleted the Pod %s, not the target's", pod.Name)
		}
		requireUID(t, "the Pod "+pod.Name, types.UID(s.member), opts)
		old := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), old); err != nil {
			return err
		}
		if err := c.Delete(ctx, pod, opts...); err != nil {
			return err
		}
		// The StatefulSet makes a new Pod and, when the claim is gone, an
		// empty claim bound to a new volume.
		replacement := old.DeepCopy()
		replacement.ResourceVersion, replacement.UID = "", types.UID(s.member+"-new")
		if err := c.Create(ctx, replacement); err != nil {
			return err
		}
		s.podReplaced = true
		if !s.claimDeleted {
			return nil
		}
		return c.Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: dataVolumePrefix + s.member, Namespace: pod.Namespace, UID: types.UID("claim-new-" + s.member)},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-new-" + s.member}})
	}
}

// operator plays the Operator before each read of the HA cluster: once the
// claim was replaced it quarantines a target that outranks the master until
// the release token is set, then offers the rebuild, and on the request with
// the offer's token ends the rebuild.
func (s *s10Fake) operator(t *testing.T) func(context.Context, client.WithWatch, client.ObjectKey) error {
	return func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.claimDeleted || !s.podReplaced {
			return nil
		}
		cluster := &databasev1alpha1.CubridCluster{}
		if err := c.Get(ctx, key, cluster); err != nil {
			return err
		}
		outranks := slices.Index(clusterMembers(haClusterName), s.member) < slices.Index(clusterMembers(haClusterName),
			s.master)
		newClaim := "claim-new-" + s.member
		switch annotations := cluster.Annotations; {
		case outranks && !s.released:
			if annotations[releaseWriteQuarantineAnnotation] == testReleaseToken && !s.noRelease {
				s.released = true
				delete(cluster.Annotations, releaseWriteQuarantineAnnotation)
				cluster.Status.WriteQuarantine = nil
				cluster.Status.LastWriteQuarantineRelease = &databasev1alpha1.WriteQuarantineRelease{
					GenerationID: testQuarantine, Method: databasev1alpha1.WriteQuarantineReleasedManually}
				break
			}
			cluster.Status.WriteQuarantine = &databasev1alpha1.WriteQuarantine{GenerationID: testQuarantine,
				Reason: databasev1alpha1.WriteQuarantinedMemberUnverified, ReleaseToken: testReleaseToken,
				Members: []databasev1alpha1.WriteQuarantineMember{{Name: s.member, PodUID: s.member + "-new"}}}
		case annotations[rebuildMemberAnnotation] != "" && !s.finished:
			value := annotations[rebuildMemberAnnotation]
			s.requests = append(s.requests, value)
			if value != s.member+":"+testOfferToken {
				t.Errorf("the rebuild was requested with %q", value)
				return nil
			}
			if s.onRequest != nil {
				s.onRequest()
			}
			cluster.Status.MemberRebuildOffers = nil
			accepted := metav1.NewTime(s.acceptedAt())
			rebuild := &databasev1alpha1.MemberRebuild{Member: cmp.Or(s.rebuiltMember, s.member),
				Source: cmp.Or(s.source, s.master), ClaimUID: newClaim, Token: testOfferToken,
				OperationID: testRebuildOp, AcceptedTime: accepted, State: databasev1alpha1.MemberRebuildAccepted}
			if s.neverEnds {
				cluster.Status.MemberRebuild = rebuild
				break
			}
			s.finished = true
			ended := metav1.NewTime(accepted.Add(10 * time.Second))
			rebuild.EndTime, rebuild.State = &ended, databasev1alpha1.MemberRebuildCompleted
			if s.state != "" {
				rebuild.State, rebuild.Message = s.state, "the restore failed"
			}
			cluster.Status.LastMemberRebuild = rebuild
			delete(cluster.Annotations, rebuildMemberAnnotation)
			for i, d := range cluster.Status.MemberDataClaims {
				if d.Name == s.member {
					cluster.Status.FormerDataClaims = append(cluster.Status.FormerDataClaims, d)
					cluster.Status.MemberDataClaims[i] = databasev1alpha1.MemberDataClaim{Name: s.member,
						ClaimUID: newClaim, VolumeName: "pv-new-" + s.member}
				}
			}
			if s.replaceOther != "" {
				pvc := &corev1.PersistentVolumeClaim{}
				name := client.ObjectKey{Namespace: key.Namespace, Name: dataVolumePrefix + s.replaceOther}
				if err := c.Get(ctx, name, pvc); err != nil {
					return err
				}
				if err := c.Delete(ctx, pvc); err != nil {
					return err
				}
				pvc.ResourceVersion, pvc.UID = "", types.UID("claim-other-"+s.replaceOther)
				if err := c.Create(ctx, pvc); err != nil {
					return err
				}
			}
		case !s.finished:
			cluster.Status.MemberRebuildOffers = []databasev1alpha1.MemberRebuildOffer{{Member: s.member,
				PodUID: s.member + "-new", ClaimUID: newClaim, VolumeName: "pv-new-" + s.member, Source: s.master,
				Token: testOfferToken}}
		}
		return c.Update(ctx, cluster)
	}
}

func s10Scenarios(t *testing.T, dir string) (evidence.Summary, evidence.Scenario, evidence.Scenario) {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s10Lane || len(summary.Scenarios) != 2 || summary.Scenarios[1].ID != s10ID {
		t.Fatalf("summary = %+v, want LAB01 and S10 in lane %s", summary, s10Lane)
	}
	return summary, summary.Scenarios[0], summary.Scenarios[1]
}

// requireNoTokenInEvidence fails when a token the Operator published is in
// any file of the evidence directory.
func requireNoTokenInEvidence(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, token := range []string{testOfferToken, testReleaseToken} {
			if strings.Contains(string(data), token) {
				t.Errorf("%s holds the token %s", path, token)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRebuildTargetIsASlaveRankedBelowTheMasterAndNeverTheMaster(t *testing.T) {
	members := clusterMembers(haClusterName)
	for _, tc := range []struct {
		master   string
		slaves   []string
		want     string
		outranks bool
	}{
		{testHAPrimary, []string{testMemberHA0, testMemberLast}, testMemberLast, false},
		{testMemberHA0, []string{testHAPrimary, testMemberLast}, testHAPrimary, false},
		// Every slave outranks the last member: the first slave is chosen,
		// and its quarantine has to be released.
		{testMemberLast, []string{testMemberHA0, testHAPrimary}, testMemberHA0, true},
		{"elsewhere-0", []string{testHAPrimary, testMemberLast}, testHAPrimary, true},
	} {
		got, outranks := rebuildTarget(tc.master, tc.slaves, members)
		if got != tc.want || outranks != tc.outranks || got == tc.master {
			t.Errorf("rebuildTarget(%s, %v) = %s, %t, want %s, %t", tc.master, tc.slaves, got, outranks, tc.want,
				tc.outranks)
		}
	}
}

func TestS10ConfigurationIsRefusedWhenItCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*HAConfig)
		reason string
	}{
		{"the kind level", func(c *HAConfig) { c.Level = evidence.LevelKind }, "vm-lab level only"},
		{"a negative limit", func(c *HAConfig) { c.RebuildLimit = -time.Second }, "not negative"},
		{"a timeout without room for S10's own steps", func(c *HAConfig) { c.Timeout = c.s10StepBounds() },
			"-timeout longer than"},
	} {
		cfg := testS10Config(t)
		cfg.NamespaceMode = FreshNamespace
		tc.change(&cfg)
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: validate() = %v, want a refusal naming %q", tc.name, err, tc.reason)
		}
	}
	cfg := testS10Config(t)
	cfg.NamespaceMode = FreshNamespace
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want S10 within an hour accepted", err)
	}
	// The real bounds: the default -timeout of 30 minutes leaves no room.
	s10ReplaceBound, s10ListBound, s10OfferBound, s10RebuildBound, s10SlaveBound =
		5*time.Minute, 2*time.Minute, 10*time.Minute, 10*time.Minute, 3*time.Minute
	cfg.Timeout, cfg.DataCheckWithin = 30*time.Minute, 2*time.Minute
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "-timeout longer than") {
		t.Fatalf("validate() with the default -timeout = %v, want a refusal", err)
	}
}

func TestRunS10RebuildsASlaveAndIsBlockedWithoutRebuildLimit(t *testing.T) {
	cfg := testS10Config(t)
	f, fake := newS10Fixture(t, cfg, testHAPrimary)
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed with rebuild_limit unset")
	}
	summary, lab, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if summary.Passed || lab.Result != evidence.Pass || s10.Result != evidence.Blocked ||
		s10.Reason != faults.ReasonTimeLimitUnset {
		t.Fatalf("S10 = %+v, want blocked with %s after LAB01 passed", s10, faults.ReasonTimeLimitUnset)
	}
	if s10.Limits[faults.RebuildLimit.Name] != "unset" {
		t.Errorf("S10 limits = %v, want rebuild_limit unset", s10.Limits)
	}
	if s10.FaultConfirmed == nil || !*s10.FaultConfirmed || s10.CleanupSucceeded == nil || !*s10.CleanupSucceeded {
		t.Errorf("S10 = %+v, want the fault confirmed and the member back", s10)
	}
	if want := []string{"claim data-" + testMemberLast, "pod " + testMemberLast}; !slices.Equal(fake.deletes, want) {
		t.Errorf("deletes = %v, want the claim, then the Pod, of the slave ranked below the master", fake.deletes)
	}
	if !slices.Equal(fake.requests, []string{testMemberLast + ":" + testOfferToken}) {
		t.Errorf("requests = %v", fake.requests)
	}
	m := s10.Measurements
	if m["newVolumeBeforeRebuild"] != s10NoDatabase || m["rebuildTargetWasMaster"] != false ||
		m["otherMembersClaimsReplaced"] != false || m["masterMoved"] != false ||
		m["writeQuarantineReleasedManually"] != false || m["rebuiltMemberApplierFailCount"] != 0.0 ||
		m["acknowledgedMissing"] != 0.0 || m["insertsAcknowledgedWhileRebuildRan"] == 0.0 ||
		m["rebuiltMemberReportedHealthyBeforeDataMatch"] != s10NotObserved {
		t.Errorf("S10 measurements = %v", m)
	}
	if s10.Operations == nil || s10.Operations.Acknowledged == 0 {
		t.Errorf("S10 operations = %+v, want the client's", s10.Operations)
	}
	for _, file := range []string{s10RecordFile, s10ListingFile, s10DataCheckFile} {
		if !slices.Contains(s10.Evidence, file) {
			t.Errorf("S10 evidence = %v, want %s", s10.Evidence, file)
		}
	}
	requireOwnEvidence(t, s10, s10ID)
	requireNoTokenInEvidence(t, cfg.EvidenceDir)
	requireNamespaceGone(t, f)
}

func TestRunS10PassesWithinRebuildLimit(t *testing.T) {
	cfg := testS10Config(t)
	cfg.RebuildLimit = time.Hour
	f, _ := newS10Fixture(t, cfg, testHAPrimary)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	summary, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if !summary.Passed || s10.Result != evidence.Pass || s10.Limits[faults.RebuildLimit.Name] != "1h0m0s" {
		t.Fatalf("S10 = %+v, want passed within rebuild_limit", s10)
	}
}

func TestRunS10FailsOverRebuildLimit(t *testing.T) {
	cfg := testS10Config(t)
	cfg.RebuildLimit = time.Nanosecond
	f, _ := newS10Fixture(t, cfg, testHAPrimary)
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed over rebuild_limit")
	}
	_, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if s10.Result != evidence.Fail || !strings.Contains(s10.Reason, "rebuild_limit exceeded") {
		t.Fatalf("S10 = %+v, want failed over rebuild_limit", s10)
	}
}

// A claim that keeps its UID after the delete is not a confirmed loss: the
// run is blocked and requests nothing.
func TestRunS10IsBlockedWhenTheClaimIsNotReplaced(t *testing.T) {
	cfg := testS10Config(t)
	cfg.RebuildLimit = time.Hour
	f, fake := newS10Fixture(t, cfg, testHAPrimary)
	fake.keepClaim = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed without the claim replaced")
	}
	_, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if s10.Result != evidence.Blocked || !strings.Contains(s10.Reason, "the fault is not confirmed") ||
		!strings.Contains(s10.Reason, "is not replaced") || s10.FaultConfirmed == nil || *s10.FaultConfirmed {
		t.Fatalf("S10 = %+v, want blocked with the fault not confirmed", s10)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %v, want none", fake.requests)
	}
}

// A new claim whose database directory already holds the database fails S10
// before any rebuild is requested, as in the Kind step; a listing that cannot
// be made leaves the fault unconfirmed and the result blocked.
func TestRunS10JudgesTheListingOfTheNewVolume(t *testing.T) {
	for _, tc := range []struct {
		listing string
		result  evidence.Result
		reason  string
		request bool
	}{
		{"verdict: holds-database", evidence.Fail, "already holds the database", false},
		{"verdict: unknown", evidence.Blocked, "the fault is not confirmed: " + s10NotListed, true},
	} {
		cfg := testS10Config(t)
		cfg.RebuildLimit = time.Hour
		f, fake := newS10Fixture(t, cfg, testHAPrimary)
		fake.listing = tc.listing
		if err := f.runner.RunHA(t.Context(), cfg); err == nil {
			t.Fatalf("%s: RunHA() passed", tc.listing)
		}
		_, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
		if s10.Result != tc.result || !strings.Contains(s10.Reason, tc.reason) || *s10.FaultConfirmed {
			t.Errorf("%s: S10 = %+v, want %s with %q", tc.listing, s10, tc.result, tc.reason)
		}
		if requested := len(fake.requests) > 0; requested != tc.request {
			t.Errorf("%s: requests = %v, want a request: %t", tc.listing, fake.requests, tc.request)
		}
	}
}

// A claim of a member that was not rebuilt with another UID afterwards is an
// outcome that must never happen, and fails S10.
func TestRunS10FailsWhenAnotherMembersClaimIsReplaced(t *testing.T) {
	cfg := testS10Config(t)
	cfg.RebuildLimit = time.Hour
	f, fake := newS10Fixture(t, cfg, testHAPrimary)
	fake.replaceOther = testHAPrimary
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed with the master's claim replaced")
	}
	_, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if s10.Result != evidence.Fail || len(s10.NeverEvents) != 1 ||
		!strings.Contains(s10.NeverEvents[0], "data-"+testHAPrimary) ||
		s10.Measurements["otherMembersClaimsReplaced"] != true {
		t.Fatalf("S10 = %+v, want failed for the master's replaced claim", s10)
	}
}

// A rebuild the Operator records for the master is an outcome that must
// never happen.
func TestRunS10FailsWhenTheRebuildNamesTheMaster(t *testing.T) {
	cfg := testS10Config(t)
	cfg.RebuildLimit = time.Hour
	f, fake := newS10Fixture(t, cfg, testHAPrimary)
	fake.rebuiltMember = testHAPrimary
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed with a rebuild of the master")
	}
	_, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if s10.Result != evidence.Fail || len(s10.NeverEvents) != 1 || s10.Measurements["rebuildTargetWasMaster"] != true {
		t.Fatalf("S10 = %+v, want failed with the master chosen", s10)
	}
}

func TestRunS10FailsOnARebuildThatDidNotComplete(t *testing.T) {
	cfg := testS10Config(t)
	cfg.RebuildLimit = time.Hour
	f, fake := newS10Fixture(t, cfg, testHAPrimary)
	fake.state = databasev1alpha1.MemberRebuildFailed
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed with a failed rebuild")
	}
	_, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if s10.Result != evidence.Fail || !strings.Contains(s10.Reason, "ended Failed") {
		t.Fatalf("S10 = %+v, want failed", s10)
	}
}

// A row the rebuilt member lacks breaks the data rules and fails S10; an
// applier with failures fails it too.
func TestRunS10FailsOnADataViolationOrAnApplierFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*haFixture, *s10Fake)
		reason string
	}{
		{"missing row", func(f *haFixture, _ *s10Fake) { f.missing[testMemberLast] = 0 }, workload.RuleMissing},
		{"fail count", func(_ *haFixture, fake *s10Fake) { fake.failCount = 3 }, "fail count 3"},
	} {
		cfg := testS10Config(t)
		cfg.RebuildLimit = time.Hour
		f, fake := newS10Fixture(t, cfg, testHAPrimary)
		fake.onRequest = func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			tc.change(f, fake)
		}
		if err := f.runner.RunHA(t.Context(), cfg); err == nil {
			t.Fatalf("%s: RunHA() passed", tc.name)
		}
		_, lab, s10 := s10Scenarios(t, cfg.EvidenceDir)
		if lab.Result != evidence.Pass || s10.Result != evidence.Fail || !strings.Contains(s10.Reason, tc.reason) {
			t.Errorf("%s: S10 = %+v, want failed naming %q", tc.name, s10, tc.reason)
		}
	}
}

// When every slave outranks the master, the target's loss starts a write
// quarantine only a person may release: the run releases it with its current
// token before the offer, and no token reaches the evidence.
func TestRunS10ReleasesTheQuarantineOfATargetThatOutranksTheMaster(t *testing.T) {
	cfg := testS10Config(t)
	cfg.RebuildLimit = time.Hour
	f, fake := newS10Fixture(t, cfg, testMemberLast)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	_, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if s10.Result != evidence.Pass || !fake.released || s10.Measurements["writeQuarantineReleasedManually"] != true {
		t.Fatalf("S10 = %+v, released %t, want passed after the manual release", s10, fake.released)
	}
	if want := []string{"claim data-" + testMemberHA0, "pod " + testMemberHA0}; !slices.Equal(fake.deletes, want) {
		t.Errorf("deletes = %v, want the first slave's claim and Pod", fake.deletes)
	}
	requireNoTokenInEvidence(t, cfg.EvidenceDir)
}

// A quarantine that is not released leaves no offer: S10 is blocked, and
// nothing is requested.
func TestRunS10IsBlockedWhenNoRebuildIsOffered(t *testing.T) {
	cfg := testS10Config(t)
	cfg.RebuildLimit = time.Hour
	f, fake := newS10Fixture(t, cfg, testMemberLast)
	fake.noRelease = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed without an offer")
	}
	_, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if s10.Result != evidence.Blocked || !strings.Contains(s10.Reason, "no rebuild was offered") ||
		len(fake.requests) != 0 {
		t.Fatalf("S10 = %+v, requests %v, want blocked with nothing requested", s10, fake.requests)
	}
	requireNoTokenInEvidence(t, cfg.EvidenceDir)
}

// A run that ends while the rebuild runs leaves S10 blocked, not failed, and
// stops the client it started.
func TestRunS10EndedDuringTheRebuildIsBlocked(t *testing.T) {
	cfg := testS10Config(t)
	cfg.RebuildLimit = time.Hour
	f, fake := newS10Fixture(t, cfg, testHAPrimary)
	fake.neverEnds = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fake.onRequest = cancel
	if err := f.runner.RunHA(ctx, cfg); err == nil {
		t.Fatal("RunHA() passed after the run ended")
	}
	_, _, s10 := s10Scenarios(t, cfg.EvidenceDir)
	if s10.Result != evidence.Blocked || !strings.Contains(s10.Reason, "the run ended before S10 was judged") {
		t.Fatalf("S10 = %+v, want blocked by the run's end", s10)
	}
	if f.kills == 0 {
		t.Fatal("the client was not stopped")
	}
}
