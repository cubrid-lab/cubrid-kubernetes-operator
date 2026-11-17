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

package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// The members, Pods and claims of rebuildScenario, and the refusals the
// tests look for.
const (
	rbM0, rbM1, rbM2                        = "r-0", "r-1", "r-2"
	rbPod0, rbPod2, rbPod2b                 = "pod-0", "pod-2", "pod-2b"
	rbClaim0, rbClaim1                      = "claim-0", "claim-1"
	rbClaim2New, rbClaim2Old, rbClaim2Newer = "claim-2-new", "claim-2-old", "claim-2-newer"
	rbWantCurrentPrimary                    = "current primary"
	rbWantNoHealthyPrimary                  = "no healthy primary"
	rbWantLastResolved                      = "last resolved primary"
	rbWantStale                             = "since the token was published"
	rbOperation                             = "rebuild-op"
)

// rebuildNonces numbers the tokens of rebuildScenario, so that no two
// scenarios give the same token.
var rebuildNonces int

// rebuildScenario is a three-member cluster whose first member r-0 is the
// current primary and r-1 a slave, both on the claims they were seen running
// the database on. r-2 lost its storage: the administrator provided an empty
// replacement claim, its new Pod started on it, and it gives no role. Only
// r-2's data is shown expendable.
func rebuildScenario(now time.Time) rebuildInput {
	return rebuildInput{
		Now:     now,
		Members: []string{rbM0, rbM1, rbM2},
		Holders: []string{rbM0, rbM1, rbM2}, BootstrapDone: true,
		CurrentPrimary: rbM0, LastResolvedPrimary: rbM0,
		Identities: map[string]memberIdentity{
			rbM0: {PodUID: rbPod0, Created: now.Add(-time.Hour)},
			rbM1: {PodUID: "pod-1", Created: now.Add(-time.Hour)},
			rbM2: {PodUID: rbPod2, Created: now.Add(-time.Minute)},
		},
		Claims: map[string]claimIdentity{
			rbM0: {UID: rbClaim0, Created: now.Add(-2 * time.Hour)},
			rbM1: {UID: rbClaim1, Created: now.Add(-2 * time.Hour)},
			rbM2: {UID: rbClaim2New, Created: now.Add(-2 * time.Minute)},
		},
		Observations: map[string]RoleObservation{
			rbM0: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: now},
			rbM1: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now},
			rbM2: {},
		},
		DataClaims: []databasev1alpha1.MemberDataClaim{
			{Name: rbM0, ClaimUID: rbClaim0}, {Name: rbM1, ClaimUID: rbClaim1}, {Name: rbM2, ClaimUID: rbClaim2Old},
		},
		Verifications: []databasev1alpha1.MemberVerification{{Name: rbM1, PodUID: "pod-1"}},
		Nonce: func() (string, error) {
			rebuildNonces++
			return fmt.Sprintf("token%08d", rebuildNonces), nil
		},
	}
}

func dataClaimOf(claims []databasev1alpha1.MemberDataClaim, member string) string {
	for _, c := range claims {
		if c.Name == member {
			return c.ClaimUID
		}
	}
	return ""
}

// A member is recorded running the database on a claim only from a fresh,
// consistent master or slave answer of a Pod created no earlier than the
// claim: that Pod mounts it. A member that gives no role keeps its record.
func TestRecordDataClaims(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	in := rebuildScenario(now)
	in.DataClaims = nil
	got := recordDataClaims(in)
	if dataClaimOf(got, rbM0) != rbClaim0 || dataClaimOf(got, rbM1) != rbClaim1 {
		t.Fatalf("the master's and the slave's claims are recorded, got %+v", got)
	}
	if dataClaimOf(got, rbM2) != "" {
		t.Fatalf("a member that gives no role is not recorded on its claim, got %+v", got)
	}

	in = rebuildScenario(now)
	got = recordDataClaims(in)
	if dataClaimOf(got, rbM2) != rbClaim2Old {
		t.Fatalf("a member that gives no role keeps the claim it was seen running the database on, got %+v", got)
	}

	record := func(mutate func(*rebuildInput)) []databasev1alpha1.MemberDataClaim {
		in := rebuildScenario(now)
		mutate(&in)
		return recordDataClaims(in)
	}
	got = record(func(in *rebuildInput) {
		in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
	})
	if dataClaimOf(got, rbM2) != rbClaim2New {
		t.Fatalf("a slave answer on the new claim records it, so the claim is no longer expendable, got %+v", got)
	}
	got = record(func(in *rebuildInput) {
		in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
		in.Claims[rbM2] = claimIdentity{UID: rbClaim2New, Created: now}
	})
	if dataClaimOf(got, rbM2) != rbClaim2Old {
		t.Fatalf("a claim created after the Pod that answered is not the one it runs on, got %+v", got)
	}
	got = record(func(in *rebuildInput) {
		in.Observations[rbM1] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave,
			ObservedAt: now.Add(-time.Hour)}
		in.Claims[rbM1] = claimIdentity{UID: "claim-1-new", Created: now.Add(-2 * time.Hour)}
	})
	if dataClaimOf(got, rbM1) != rbClaim1 {
		t.Fatalf("a stale answer records nothing, got %+v", got)
	}
	got = record(func(in *rebuildInput) {
		in.Unknown = true
		in.DataClaims = nil
	})
	if len(got) != 0 {
		t.Fatalf("nothing is recorded when the Pods or claims could not be read, got %+v", got)
	}
}

// A rebuild is refused for the current primary, for the last resolved
// primary while it is not verified, and for a member whose data has not been
// shown expendable; only r-2 of the scenario is eligible.
func TestRebuildRefusal(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		member string
		mutate func(*rebuildInput)
		want   string
	}{
		{name: "eligible", member: rbM2},
		{name: rbWantCurrentPrimary, member: rbM0, want: rbWantCurrentPrimary},
		// r-1 can be rebuilt only while every other member answers.
		{name: "retained claim", member: rbM1, mutate: func(in *rebuildInput) {
			in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
		}, want: "not shown expendable"},
		{name: "retained claim while another member does not answer", member: rbM1, want: rbWantNoHealthyPrimary},
		{name: "no member", member: "r-7", want: "not a promotable member"},
		{name: "last resolved primary unverified", member: rbM2, mutate: func(in *rebuildInput) {
			in.CurrentPrimary, in.LastResolvedPrimary = rbM1, rbM2
		}, want: rbWantLastResolved},
		{name: "last resolved primary verified", member: rbM2, mutate: func(in *rebuildInput) {
			in.CurrentPrimary, in.LastResolvedPrimary = rbM1, rbM2
			in.Verifications = append(in.Verifications, databasev1alpha1.MemberVerification{Name: rbM2, PodUID: rbPod2})
		}},
		{name: "last resolved primary verified in an earlier Pod", member: rbM2, mutate: func(in *rebuildInput) {
			in.CurrentPrimary, in.LastResolvedPrimary = rbM1, rbM2
			in.Verifications = append(in.Verifications, databasev1alpha1.MemberVerification{Name: rbM2, PodUID: "pod-old"})
		}, want: rbWantLastResolved},
		{name: "no primary resolved while the member gives no role", member: rbM2,
			mutate: func(in *rebuildInput) { in.CurrentPrimary = "" }},
		{name: "member reports itself master", member: rbM2, mutate: func(in *rebuildInput) {
			in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: now}
		}, want: rbWantCurrentPrimary},
		{name: "another member does not answer", member: rbM2, mutate: func(in *rebuildInput) {
			in.Observations[rbM1] = RoleObservation{}
		}, want: rbWantNoHealthyPrimary},
		{name: "no other active master", member: rbM2, mutate: func(in *rebuildInput) {
			in.Observations[rbM0] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
		}, want: rbWantNoHealthyPrimary},
		{name: "no claim recorded", member: rbM2, mutate: func(in *rebuildInput) { in.DataClaims = in.DataClaims[:2] },
			want: "no data claim"},
		{name: "claim missing", member: rbM2, mutate: func(in *rebuildInput) { delete(in.Claims, rbM2) },
			want: "has no data claim"},
		{name: "claim being deleted", member: rbM2, mutate: func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2New, Created: now.Add(-2 * time.Minute), Deleting: true}
		}, want: "being deleted"},
		{name: "no Pod", member: rbM2, mutate: func(in *rebuildInput) { delete(in.Identities, rbM2) },
			want: "has no Pod"},
		{name: "not a holder", member: rbM2, mutate: func(in *rebuildInput) { in.Holders = in.Holders[:2] },
			want: "not recorded as holding"},
		{name: "bootstrap not done", member: rbM2, mutate: func(in *rebuildInput) { in.BootstrapDone = false },
			want: "HA bootstrap"},
		{name: "rebuild in progress", member: rbM2, mutate: func(in *rebuildInput) {
			in.Active = &databasev1alpha1.MemberRebuild{Member: rbM1, OperationID: "op"}
		}, want: "in progress"},
		{name: "Pods unreadable", member: rbM2, mutate: func(in *rebuildInput) { in.Unknown = true },
			want: "could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := rebuildScenario(now)
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			got := rebuildRefusal(in, tc.member)
			if tc.want == "" && got != "" {
				t.Fatalf("expected no refusal, got %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("expected a refusal containing %q, got %q", tc.want, got)
			}
		})
	}
}

// The offer binds the member's Pod and claim, the source and its Pod; it
// keeps its token while they stay the same and gets a new one when any
// changes.
func TestRebuildOffers(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	in := rebuildScenario(now)
	offers, err := rebuildOffers(in)
	if err != nil {
		t.Fatal(err)
	}
	want := databasev1alpha1.MemberRebuildOffer{Member: rbM2, PodUID: rbPod2, ClaimUID: rbClaim2New,
		Source: rbM0, SourcePodUID: rbPod0}
	if len(offers) == 1 && offers[0].Token != "" {
		want.Token = offers[0].Token
	}
	if len(offers) != 1 || offers[0] != want {
		t.Fatalf("expected only %+v, got %+v", want, offers)
	}

	in = rebuildScenario(now)
	in.Offers = offers
	again, err := rebuildOffers(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0] != want {
		t.Fatalf("an unchanged offer keeps its token, got %+v", again)
	}

	changes := map[string]func(*rebuildInput){
		"member Pod": func(in *rebuildInput) {
			in.Identities[rbM2] = memberIdentity{PodUID: rbPod2b, Created: now}
		},
		"member claim": func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2Newer, Created: now.Add(-2 * time.Minute)}
		},
		"source": func(in *rebuildInput) {
			in.CurrentPrimary, in.LastResolvedPrimary = rbM1, rbM1
			in.Observations[rbM0] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
			in.Observations[rbM1] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: now}
		},
		"source Pod": func(in *rebuildInput) {
			in.Identities[rbM0] = memberIdentity{PodUID: "pod-0b", Created: now.Add(-time.Hour)}
		},
	}
	for name, mutate := range changes {
		t.Run(name, func(t *testing.T) {
			in := rebuildScenario(now)
			in.Offers = offers
			mutate(&in)
			got, err := rebuildOffers(in)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Member != rbM2 || got[0].Token == want.Token {
				t.Fatalf("a changed %s replaces the token, got %+v", name, got)
			}
		})
	}

	in = rebuildScenario(now)
	in.Offers, in.Unknown = offers, true
	kept, err := rebuildOffers(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0] != want {
		t.Fatalf("the offers are kept when the Pods or claims could not be read, got %+v", kept)
	}
}

// storedRebuild is the stored cluster with the annotation value and the
// offers.
func storedRebuild(value string, offers []databasev1alpha1.MemberRebuildOffer) *databasev1alpha1.CubridCluster {
	c := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid-1")}}
	if value != "" {
		c.Annotations = map[string]string{rebuildMemberAnnotation: value}
	}
	c.Status.MemberRebuildOffers = offers
	return c
}

// A request names the member and the token published for it. A wrong
// target, the current primary, a stale or replayed token is never accepted;
// an accepted one is recorded with its operation ID.
func TestDecideRebuildRequest(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	offers, err := rebuildOffers(rebuildScenario(now))
	if err != nil || len(offers) != 1 {
		t.Fatalf("scenario offers: %+v, %v", offers, err)
	}
	token := offers[0].Token
	// bothReplaced is the scenario in which r-1 and r-2 answer as slaves on
	// claims that were not recorded holding the database before this pass,
	// so that both are offered.
	bothReplaced := func(in *rebuildInput) {
		in.Claims[rbM1] = claimIdentity{UID: "claim-1-new", Created: now.Add(-2 * time.Hour)}
		in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
	}
	cases := []struct {
		name    string
		stored  func() *databasev1alpha1.CubridCluster
		mutate  func(*rebuildInput)
		verdict rebuildVerdict
		msg     string
	}{
		{name: "no annotation", stored: func() *databasev1alpha1.CubridCluster { return storedRebuild("", offers) },
			verdict: rebuildNone},
		{name: "stored cluster not read", stored: func() *databasev1alpha1.CubridCluster { return nil },
			verdict: rebuildWait},
		{name: "another cluster UID", stored: func() *databasev1alpha1.CubridCluster {
			c := storedRebuild("r-2:"+token, offers)
			c.UID = "uid-2"
			return c
		}, verdict: rebuildWait},
		{name: "no member named", stored: func() *databasev1alpha1.CubridCluster { return storedRebuild(token, offers) },
			verdict: rebuildInert, msg: "<member>:<token>"},
		{name: rbWantCurrentPrimary, stored: func() *databasev1alpha1.CubridCluster {
			return storedRebuild("r-0:"+token, offers)
		}, verdict: rebuildRefused, msg: rbWantCurrentPrimary},
		{name: "wrong target with retained data", mutate: func(in *rebuildInput) {
			in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
		},
			stored:  func() *databasev1alpha1.CubridCluster { return storedRebuild("r-1:"+token, offers) },
			verdict: rebuildRefused, msg: "not shown expendable"},
		{name: "wrong target with another member's token", mutate: bothReplaced,
			stored: func() *databasev1alpha1.CubridCluster {
				in := rebuildScenario(now)
				bothReplaced(&in)
				both, _ := rebuildOffers(in)
				return storedRebuild("r-1:"+both[1].Token, both)
			}, verdict: rebuildInert, msg: "not the current rebuild token of r-1"},
		{name: "last resolved primary unverified", mutate: func(in *rebuildInput) {
			in.CurrentPrimary, in.LastResolvedPrimary = rbM1, rbM2
		}, stored: func() *databasev1alpha1.CubridCluster { return storedRebuild("r-2:"+token, offers) },
			verdict: rebuildRefused, msg: rbWantLastResolved},
		{name: "stale token after the member's Pod changed", mutate: func(in *rebuildInput) {
			in.Identities[rbM2] = memberIdentity{PodUID: rbPod2b, Created: now}
		}, stored: func() *databasev1alpha1.CubridCluster { return storedRebuild("r-2:"+token, offers) },
			verdict: rebuildInert, msg: rbWantStale},
		{name: "stale token after the claim changed", mutate: func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2Newer, Created: now.Add(-2 * time.Minute)}
		}, stored: func() *databasev1alpha1.CubridCluster { return storedRebuild("r-2:"+token, offers) },
			verdict: rebuildInert, msg: rbWantStale},
		{name: "stale token after the source changed", mutate: func(in *rebuildInput) {
			in.CurrentPrimary, in.LastResolvedPrimary = rbM1, rbM1
			in.Observations[rbM0] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
			in.Observations[rbM1] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: now}
		}, stored: func() *databasev1alpha1.CubridCluster { return storedRebuild("r-2:"+token, offers) },
			verdict: rebuildInert, msg: rbWantStale},
		{name: "a token that was never published", stored: func() *databasev1alpha1.CubridCluster {
			return storedRebuild("r-2:token99999999", offers)
		}, verdict: rebuildInert, msg: "not the current rebuild token of r-2"},
		{name: "replay of an accepted request", stored: func() *databasev1alpha1.CubridCluster {
			c := storedRebuild("r-2:"+token, nil)
			c.Status.MemberRebuild = &databasev1alpha1.MemberRebuild{Member: rbM2, Token: token, OperationID: rbOperation}
			return c
		}, mutate: func(in *rebuildInput) {
			in.Active = &databasev1alpha1.MemberRebuild{Member: rbM2, Token: token, OperationID: rbOperation}
		}, verdict: rebuildConsumed},
		{name: "replay after completion", stored: func() *databasev1alpha1.CubridCluster {
			c := storedRebuild("r-2:"+token, offers)
			c.Status.LastMemberRebuild = &databasev1alpha1.MemberRebuild{Member: rbM2, Token: token,
				OperationID: rbOperation, State: databasev1alpha1.MemberRebuildCompleted}
			return c
		}, verdict: rebuildConsumed},
		{name: "cache behind the stored offer", stored: func() *databasev1alpha1.CubridCluster {
			newer := offers[0]
			newer.Token = "token77777777"
			return storedRebuild("r-2:token77777777", []databasev1alpha1.MemberRebuildOffer{newer})
		}, verdict: rebuildWait},
		{name: "accepted", stored: func() *databasev1alpha1.CubridCluster { return storedRebuild("r-2:"+token, offers) },
			verdict: rebuildAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := rebuildScenario(now)
			in.Offers = offers
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			passOffers, err := rebuildOffers(in)
			if err != nil {
				t.Fatal(err)
			}
			verdict, msg, rebuild, err := decideRebuildRequest(in, "uid-1", tc.stored(), passOffers)
			if err != nil {
				t.Fatal(err)
			}
			if verdict != tc.verdict {
				t.Fatalf("expected the verdict %q, got %q (%s)", tc.verdict, verdict, msg)
			}
			if !strings.Contains(msg, tc.msg) {
				t.Fatalf("expected a message containing %q, got %q", tc.msg, msg)
			}
			if (verdict == rebuildAccepted) != (rebuild != nil) {
				t.Fatalf("only an accepted request returns a rebuild, got %+v", rebuild)
			}
			if rebuild == nil {
				return
			}
			want := databasev1alpha1.MemberRebuild{Member: rbM2, PodUID: rbPod2, ClaimUID: rbClaim2New,
				Source: rbM0, SourcePodUID: rbPod0, Token: token, OperationID: rebuild.OperationID,
				AcceptedTime: metav1.NewTime(now), State: databasev1alpha1.MemberRebuildAccepted}
			if rebuild.OperationID == "" || rebuild.OperationID == token {
				t.Fatalf("the operation ID is a new value of its own, got %q", rebuild.OperationID)
			}
			if !rebuild.AcceptedTime.Equal(&want.AcceptedTime) {
				t.Fatalf("expected the acceptance time %v, got %v", want.AcceptedTime, rebuild.AcceptedTime)
			}
			rebuild.AcceptedTime = want.AcceptedTime
			if *rebuild != want {
				t.Fatalf("expected %+v, got %+v", want, *rebuild)
			}
		})
	}
}

// An accepted rebuild whose member Pod or claim, source or source Pod
// changed before anything was restored is withdrawn; a pass that resolves no
// primary keeps it.
func TestRebuildWithdrawal(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	active := func() *databasev1alpha1.MemberRebuild {
		return &databasev1alpha1.MemberRebuild{Member: rbM2, PodUID: rbPod2, ClaimUID: rbClaim2New,
			Source: rbM0, SourcePodUID: rbPod0, Token: "t", OperationID: "op",
			State: databasev1alpha1.MemberRebuildAccepted}
	}
	cases := []struct {
		name   string
		mutate func(*rebuildInput)
		want   string
	}{
		{name: "unchanged"},
		{name: "no primary resolved", mutate: func(in *rebuildInput) { in.CurrentPrimary = "" }},
		{name: "no healthy primary observed", mutate: func(in *rebuildInput) {
			in.Observations[rbM0] = RoleObservation{}
		}},
		{name: "Pods unreadable", mutate: func(in *rebuildInput) { in.Unknown = true; in.Identities = nil }},
		{name: "member Pod", mutate: func(in *rebuildInput) {
			in.Identities[rbM2] = memberIdentity{PodUID: rbPod2b}
		}, want: "Pod"},
		{name: "member claim", mutate: func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2Newer}
		}, want: "claim"},
		{name: "source", mutate: func(in *rebuildInput) {
			in.Observations[rbM0] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
			in.Observations[rbM1] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: now}
		}, want: "the primary"},
		{name: "source Pod", mutate: func(in *rebuildInput) {
			in.Identities[rbM0] = memberIdentity{PodUID: "pod-0b"}
		}, want: "the primary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := rebuildScenario(now)
			in.Active = active()
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			got := rebuildWithdrawal(in)
			if tc.want == "" && got != "" {
				t.Fatalf("expected the rebuild kept, got %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("expected a withdrawal naming %q, got %q", tc.want, got)
			}
		})
	}
}
