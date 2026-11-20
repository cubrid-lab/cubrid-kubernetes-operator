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
	rbClaim0, rbClaim1, rbClaim1New         = "claim-0", "claim-1", "claim-1-new"
	rbClaim2New, rbClaim2Old, rbClaim2Newer = "claim-2-new", "claim-2-old", "claim-2-newer"
	rbWantCurrentPrimary                    = "current primary"
	rbWantNoHealthyPrimary                  = "no healthy primary"
	rbWantLastResolved                      = "last resolved primary"
	rbWantStale                             = "since the token was published"
	rbOperation                             = "rebuild-op"
	rbPV2New, rbPV2Old                      = "pv-2-new", "pv-2-old"
	rbWantQuarantine                        = "write quarantine"
	rbPV0, rbPV1                            = "pv-0", "pv-1"
	rbWantNotExpendable                     = "not shown expendable"
	rbWantVolume                            = "volume"
	rbWantNameSource                        = "name the source"
)

// formerPrimaryLost is rebuildScenario with r-2 as the last resolved primary:
// it was the primary when it lost its storage, and CUBRID's failover made
// r-0 the active master. r-2 gives no role, so no primary is resolved and
// status.lastResolvedPrimary keeps naming r-2. The write quarantine that
// rule 2 started for it was released by the administrator.
func formerPrimaryLost(in *rebuildInput) {
	in.CurrentPrimary, in.LastResolvedPrimary = "", rbM2
}

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
			rbM0: {UID: rbClaim0, Created: now.Add(-2 * time.Hour), VolumeName: rbPV0},
			rbM1: {UID: rbClaim1, Created: now.Add(-2 * time.Hour), VolumeName: rbPV1},
			rbM2: {UID: rbClaim2New, Created: now.Add(-2 * time.Minute), VolumeName: rbPV2New},
		},
		Observations: map[string]RoleObservation{
			rbM0: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: now},
			rbM1: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now},
			rbM2: {},
		},
		DataClaims: []databasev1alpha1.MemberDataClaim{
			{Name: rbM0, ClaimUID: rbClaim0, VolumeName: rbPV0}, {Name: rbM1, ClaimUID: rbClaim1, VolumeName: rbPV1},
			{Name: rbM2, ClaimUID: rbClaim2Old, VolumeName: rbPV2Old},
		},
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
	got, _ := recordDataClaims(in)
	if dataClaimOf(got, rbM0) != rbClaim0 || dataClaimOf(got, rbM1) != rbClaim1 {
		t.Fatalf("the master's and the slave's claims are recorded, got %+v", got)
	}
	if dataClaimOf(got, rbM2) != "" {
		t.Fatalf("a member that gives no role is not recorded on its claim, got %+v", got)
	}
	if got[0].VolumeName != rbPV0 {
		t.Fatalf("the volume the claim is bound to is recorded with it, got %+v", got)
	}

	in = rebuildScenario(now)
	got, _ = recordDataClaims(in)
	if dataClaimOf(got, rbM2) != rbClaim2Old {
		t.Fatalf("a member that gives no role keeps the claim it was seen running the database on, got %+v", got)
	}

	record := func(mutate func(*rebuildInput)) []databasev1alpha1.MemberDataClaim {
		in := rebuildScenario(now)
		mutate(&in)
		got, _ := recordDataClaims(in)
		return got
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
		in.Claims[rbM1] = claimIdentity{UID: rbClaim1New, Created: now.Add(-2 * time.Hour)}
	})
	if dataClaimOf(got, rbM1) != rbClaim1 {
		t.Fatalf("a stale answer records nothing, got %+v", got)
	}
	got = record(func(in *rebuildInput) {
		in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
		in.Unstable = map[string]bool{rbM2: true}
	})
	if dataClaimOf(got, rbM2) != rbClaim2Old {
		t.Fatalf("the answer of a member whose Pod changed during the observation records nothing, got %+v", got)
	}
	got = record(func(in *rebuildInput) {
		in.Unknown = true
		in.DataClaims = nil
	})
	if len(got) != 0 {
		t.Fatalf("nothing is recorded when the Pods or claims could not be read, got %+v", got)
	}
}

// A rebuild is refused while a write quarantine is in force, for the current
// primary, and for a member whose data has not been shown expendable; only
// r-2 of the scenario is eligible. The last resolved primary that lost its
// storage is eligible beside exactly one other active master, and refused
// without one or while a write quarantine is in force (ADR-0006). The claims are
// recorded from the same observation first, as the reconciler does.
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
		}, want: rbWantNotExpendable},
		{name: "retained claim while another member does not answer", member: rbM1, want: rbWantNoHealthyPrimary},
		{name: "no member", member: "r-7", want: "not a promotable member"},
		// The request for it must still name the source (decideRebuildRequest).
		{name: "last resolved primary that lost its storage beside one other active master", member: rbM2,
			mutate: formerPrimaryLost},
		// A former primary answering as a slave has its claim recorded, so
		// it is refused.
		{name: "last resolved primary answering as a slave", member: rbM2, mutate: func(in *rebuildInput) {
			in.CurrentPrimary, in.LastResolvedPrimary = rbM0, rbM2
			in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
		}, want: rbWantNotExpendable},
		{name: "last resolved primary without another active master", member: rbM2, mutate: func(in *rebuildInput) {
			formerPrimaryLost(in)
			in.Observations[rbM0] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
		}, want: rbWantNoHealthyPrimary},
		{name: "last resolved primary beside two active masters", member: rbM2, mutate: func(in *rebuildInput) {
			formerPrimaryLost(in)
			in.Observations[rbM1] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: now}
		}, want: rbWantNoHealthyPrimary},
		{name: "last resolved primary while another member does not answer", member: rbM2,
			mutate: func(in *rebuildInput) {
				formerPrimaryLost(in)
				in.Observations[rbM1] = RoleObservation{}
			}, want: rbWantNoHealthyPrimary},
		{name: "last resolved primary while the source's Pod changed during the observation", member: rbM2,
			mutate: func(in *rebuildInput) {
				formerPrimaryLost(in)
				in.Unstable = map[string]bool{rbM0: true}
			}, want: rbWantNoHealthyPrimary},
		{name: "last resolved primary while the quarantine for it is in force", member: rbM2,
			mutate: func(in *rebuildInput) {
				formerPrimaryLost(in)
				in.Quarantine = &databasev1alpha1.WriteQuarantine{
					Reason:         databasev1alpha1.WriteQuarantinedMemberUnverified,
					ObservedMaster: rbM0, Members: []databasev1alpha1.WriteQuarantineMember{{Name: rbM2}}}
			}, want: rbWantQuarantine},
		{name: "last resolved primary on a new claim bound to its retained volume", member: rbM2,
			mutate: func(in *rebuildInput) {
				formerPrimaryLost(in)
				in.Claims[rbM2] = claimIdentity{UID: rbClaim2New, Created: now.Add(-2 * time.Minute), VolumeName: rbPV2Old}
			}, want: rbWantVolume},
		{name: "quarantine for two masters", member: rbM2, mutate: func(in *rebuildInput) {
			in.Quarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMultipleMasters,
				Members: []databasev1alpha1.WriteQuarantineMember{{Name: rbM0}, {Name: rbM1}}}
		}, want: rbWantQuarantine},
		{name: "quarantine for a change of master", member: rbM2, mutate: func(in *rebuildInput) {
			in.Quarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMasterChanged}
		}, want: rbWantQuarantine},
		{name: "quarantine naming the target as unverified", member: rbM2, mutate: func(in *rebuildInput) {
			in.Quarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMemberUnverified,
				Members: []databasev1alpha1.WriteQuarantineMember{{Name: rbM2}}}
		}, want: rbWantQuarantine},
		{name: "claim bound to the volume the member ran the database on", member: rbM2, mutate: func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2New, Created: now.Add(-2 * time.Minute), VolumeName: rbPV2Old}
		}, want: rbWantVolume},
		{name: "claim bound to another member's recorded volume", member: rbM2, mutate: func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2New, Created: now.Add(-2 * time.Minute), VolumeName: rbPV1}
		}, want: rbWantVolume},
		{name: "claim not bound", member: rbM2, mutate: func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2New, Created: now.Add(-2 * time.Minute)}
		}, want: "not bound"},
		{name: "claim created after its Pod", member: rbM2, mutate: func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2New, Created: now, VolumeName: rbPV2New}
		}, want: "created after"},
		{name: "holder through a recorded claim", member: rbM2, mutate: func(in *rebuildInput) {
			in.Holders = in.Holders[:2]
		}},
		{name: "source Pod changed during the observation", member: rbM2, mutate: func(in *rebuildInput) {
			in.Unstable = map[string]bool{rbM0: true}
		}, want: rbWantNoHealthyPrimary},
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
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2New, Created: now.Add(-2 * time.Minute), Deleting: true,
				VolumeName: rbPV2New}
		}, want: "being deleted"},
		{name: "no Pod", member: rbM2, mutate: func(in *rebuildInput) { delete(in.Identities, rbM2) },
			want: "has no Pod"},
		{name: "not a holder", member: rbM2, mutate: func(in *rebuildInput) {
			in.Holders, in.DataClaims = in.Holders[:2], in.DataClaims[:2]
		}, want: "not recorded as holding"},
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
			in.DataClaims, in.FormerDataClaims = recordDataClaims(in)
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
		VolumeName: rbPV2New, Source: rbM0, SourcePodUID: rbPod0}
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
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2Newer, Created: now.Add(-2 * time.Minute), VolumeName: "pv-2-newer"}
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

	// The last resolved primary's offer says so, and it binds another
	// token than the same member's offer as an ordinary member.
	in = rebuildScenario(now)
	in.Offers = offers
	formerPrimaryLost(&in)
	former, err := rebuildOffers(in)
	if err != nil {
		t.Fatal(err)
	}
	wantFormer := want
	wantFormer.FormerPrimary = true
	if len(former) == 1 {
		wantFormer.Token = former[0].Token
	}
	if len(former) != 1 || former[0] != wantFormer || former[0].Token == want.Token {
		t.Fatalf("expected the former primary's offer %+v with a new token, got %+v", wantFormer, former)
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
	formerIn := rebuildScenario(now)
	formerPrimaryLost(&formerIn)
	formerOffers, err := rebuildOffers(formerIn)
	if err != nil || len(formerOffers) != 1 || !formerOffers[0].FormerPrimary {
		t.Fatalf("former primary offers: %+v, %v", formerOffers, err)
	}
	formerToken := formerOffers[0].Token
	// asFormer is the pass after the former primary's offer was published.
	asFormer := func(in *rebuildInput) {
		formerPrimaryLost(in)
		in.Offers = formerOffers
	}
	// bothReplaced is the scenario in which r-1 and r-2 answer as slaves on
	// claims that were not recorded holding the database before this pass,
	// so that both are offered.
	bothReplaced := func(in *rebuildInput) {
		in.Claims[rbM1] = claimIdentity{UID: rbClaim1New, Created: now.Add(-2 * time.Hour), VolumeName: "pv-1-new"}
		in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
	}
	cases := []struct {
		name    string
		stored  func() *databasev1alpha1.CubridCluster
		mutate  func(*rebuildInput)
		verdict rebuildVerdict
		msg     string
		// former says that the accepted rebuild is the former primary's.
		former bool
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
			verdict: rebuildRefused, msg: rbWantNotExpendable},
		{name: "wrong target with another member's token", mutate: bothReplaced,
			stored: func() *databasev1alpha1.CubridCluster {
				in := rebuildScenario(now)
				bothReplaced(&in)
				both, _ := rebuildOffers(in)
				return storedRebuild("r-1:"+both[1].Token, both)
			}, verdict: rebuildInert, msg: "not the current rebuild token of r-1"},
		{name: "former primary without the source named", mutate: asFormer,
			stored: func() *databasev1alpha1.CubridCluster {
				return storedRebuild("r-2:"+formerToken, formerOffers)
			}, verdict: rebuildRefused, msg: rbWantNameSource},
		{name: "former primary with another member named as the source", mutate: asFormer,
			stored: func() *databasev1alpha1.CubridCluster {
				return storedRebuild("r-2:"+formerToken+":r-1", formerOffers)
			}, verdict: rebuildInert, msg: "not the source r-0"},
		{name: "former primary with an empty source", mutate: asFormer,
			stored: func() *databasev1alpha1.CubridCluster {
				return storedRebuild("r-2:"+formerToken+":", formerOffers)
			}, verdict: rebuildInert, msg: "not the source r-0"},
		{name: "former primary with the source named while the quarantine for it is in force",
			mutate: func(in *rebuildInput) {
				asFormer(in)
				in.Quarantine = &databasev1alpha1.WriteQuarantine{
					Reason:         databasev1alpha1.WriteQuarantinedMemberUnverified,
					ObservedMaster: rbM0, Members: []databasev1alpha1.WriteQuarantineMember{{Name: rbM2}}}
			}, stored: func() *databasev1alpha1.CubridCluster {
				return storedRebuild("r-2:"+formerToken+":r-0", formerOffers)
			}, verdict: rebuildRefused, msg: rbWantQuarantine},
		{name: "former primary with the source named and no active master left", mutate: func(in *rebuildInput) {
			asFormer(in)
			in.Observations[rbM0] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
		}, stored: func() *databasev1alpha1.CubridCluster {
			return storedRebuild("r-2:"+formerToken+":r-0", formerOffers)
		}, verdict: rebuildRefused, msg: rbWantNoHealthyPrimary},
		{name: "former primary's token from before the member became the last resolved primary",
			mutate:  formerPrimaryLost,
			stored:  func() *databasev1alpha1.CubridCluster { return storedRebuild("r-2:"+token+":r-0", offers) },
			verdict: rebuildInert, msg: rbWantStale},
		{name: "former primary with the source named", mutate: asFormer,
			stored: func() *databasev1alpha1.CubridCluster {
				return storedRebuild("r-2:"+formerToken+":r-0", formerOffers)
			}, verdict: rebuildAccepted, former: true},
		{name: "a source named for an ordinary member", stored: func() *databasev1alpha1.CubridCluster {
			return storedRebuild("r-2:"+token+":r-0", offers)
		}, verdict: rebuildInert, msg: "only for the last resolved primary"},
		{name: "stale token after the member's Pod changed", mutate: func(in *rebuildInput) {
			in.Identities[rbM2] = memberIdentity{PodUID: rbPod2b, Created: now}
		}, stored: func() *databasev1alpha1.CubridCluster { return storedRebuild("r-2:"+token, offers) },
			verdict: rebuildInert, msg: rbWantStale},
		{name: "stale token after the claim changed", mutate: func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2Newer, Created: now.Add(-2 * time.Minute), VolumeName: "pv-2-newer"}
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
		{name: "write quarantine in force", mutate: func(in *rebuildInput) {
			in.Quarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMultipleMasters}
		}, stored: func() *databasev1alpha1.CubridCluster { return storedRebuild("r-2:"+token, offers) },
			verdict: rebuildRefused, msg: rbWantQuarantine},
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
				VolumeName: rbPV2New, Source: rbM0, SourcePodUID: rbPod0, Token: token, OperationID: rebuild.OperationID,
				AcceptedTime: metav1.NewTime(now), State: databasev1alpha1.MemberRebuildAccepted}
			if tc.former {
				want.Token, want.FormerPrimary = formerToken, true
			}
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

// An accepted rebuild is withdrawn when its member Pod, claim or volume,
// source or source Pod changed before anything was restored, and whenever
// the member could no longer be offered a rebuild: a write quarantine, a
// master answer, the member becoming the last resolved primary or its claim
// seen holding the database. A pass that sees no healthy primary among the
// other members keeps it, unless a quarantine is in force.
func TestRebuildWithdrawal(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	active := func() *databasev1alpha1.MemberRebuild {
		return &databasev1alpha1.MemberRebuild{Member: rbM2, PodUID: rbPod2, ClaimUID: rbClaim2New,
			VolumeName: rbPV2New, Source: rbM0, SourcePodUID: rbPod0, Token: "t", OperationID: "op",
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
		{name: "member volume", mutate: func(in *rebuildInput) {
			in.Claims[rbM2] = claimIdentity{UID: rbClaim2New, Created: now.Add(-2 * time.Minute), VolumeName: "pv-other"}
		}, want: rbWantVolume},
		{name: "write quarantine", mutate: func(in *rebuildInput) {
			in.Quarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMemberUnverified,
				Members: []databasev1alpha1.WriteQuarantineMember{{Name: rbM2}}}
		}, want: rbWantQuarantine},
		{name: "write quarantine without a healthy primary", mutate: func(in *rebuildInput) {
			in.Observations[rbM0] = RoleObservation{}
			in.Quarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMasterChanged}
		}, want: rbWantQuarantine},
		{name: "member answers master in the same Pod and claim", mutate: func(in *rebuildInput) {
			in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: now}
		}, want: "reports itself master"},
		{name: "member becomes the last resolved primary", mutate: func(in *rebuildInput) {
			in.LastResolvedPrimary = rbM2
		}, want: rbWantLastResolved},
		// The request named no source, so the member's data was never given
		// up; that it became the last resolved primary withdraws the rebuild
		// even in a pass that sees no other member as the active master
		// (ADR-0006).
		{name: "member becomes the last resolved primary while no other member is master",
			mutate: func(in *rebuildInput) {
				in.CurrentPrimary, in.LastResolvedPrimary = "", rbM2
				in.Observations[rbM0] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
			}, want: rbWantLastResolved},
		{name: "former primary's rebuild while it stays the last resolved primary", mutate: func(in *rebuildInput) {
			formerPrimaryLost(in)
			in.Active.FormerPrimary = true
		}},
		{name: "former primary's rebuild when a write quarantine starts", mutate: func(in *rebuildInput) {
			formerPrimaryLost(in)
			in.Active.FormerPrimary = true
			in.Quarantine = &databasev1alpha1.WriteQuarantine{Reason: databasev1alpha1.WriteQuarantinedMultipleMasters}
		}, want: rbWantQuarantine},
		{name: "claim recorded holding the database", mutate: func(in *rebuildInput) {
			in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
			in.DataClaims, in.FormerDataClaims = recordDataClaims(*in)
		}, want: rbWantNotExpendable},
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

// When a member is seen running the database on a later claim, its earlier
// record is kept in the former records rather than forgotten: that claim and
// volume held the database, and may hold the only copy of writes no other
// member has, such as a lost primary's storage (#117, condition 1 of the
// review of #1018). Nothing is ever removed from the former records; once
// they are full, the later claim is not recorded and the earlier record
// stays.
func TestRecordDataClaimsKeepsFormerRecords(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	in := rebuildScenario(now)
	in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
	claims, former := recordDataClaims(in)
	if dataClaimOf(claims, rbM2) != rbClaim2New {
		t.Fatalf("the later claim is the member's record, got %+v", claims)
	}
	want := databasev1alpha1.MemberDataClaim{Name: rbM2, ClaimUID: rbClaim2Old, VolumeName: rbPV2Old}
	if len(former) != 1 || former[0] != want {
		t.Fatalf("the earlier record is kept as a former record, got %+v", former)
	}

	in.DataClaims, in.FormerDataClaims = claims, former
	claims, former = recordDataClaims(in)
	if len(former) != 1 || dataClaimOf(claims, rbM2) != rbClaim2New {
		t.Fatalf("an unchanged record adds no former record, got %+v and %+v", claims, former)
	}

	in = rebuildScenario(now)
	in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
	for i := range maxFormerDataClaims {
		in.FormerDataClaims = append(in.FormerDataClaims, databasev1alpha1.MemberDataClaim{Name: rbM1,
			ClaimUID: fmt.Sprintf("claim-1-%d", i), VolumeName: fmt.Sprintf("pv-1-%d", i)})
	}
	claims, former = recordDataClaims(in)
	if len(former) != maxFormerDataClaims || former[0].ClaimUID != "claim-1-0" {
		t.Fatalf("full former records are kept as they are, got %d records", len(former))
	}
	if dataClaimOf(claims, rbM2) != rbClaim2Old {
		t.Fatalf("with the former records full, the earlier record stays the member's record, got %+v", claims)
	}
}

// A volume that an earlier record of any member held is never offered as
// a replacement. The second member was rebuilt onto a new claim and volume
// and runs on them; then the third member's storage is replaced, and the
// volume the second member lost is bound to the third member's new claim.
// The record of the second member names its new volume only, so without the
// former records that volume would be offered as new (#117).
func TestRebuildRefusesAVolumeAFormerRecordHeld(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	in := rebuildScenario(now)
	in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
	in.DataClaims, in.FormerDataClaims = recordDataClaims(in)

	in.Observations[rbM1] = RoleObservation{}
	in.Identities[rbM1] = memberIdentity{PodUID: "pod-1b", Created: now.Add(-time.Minute)}
	in.Claims[rbM1] = claimIdentity{UID: rbClaim1New, Created: now.Add(-2 * time.Minute), VolumeName: rbPV2Old}
	in.Observations[rbM2] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now}
	in.DataClaims, in.FormerDataClaims = recordDataClaims(in)
	if got := rebuildRefusal(in, rbM1); !strings.Contains(got, rbPV2Old) {
		t.Fatalf("expected a refusal naming the volume %s an earlier record held, got %q", rbPV2Old, got)
	}
	if offers, err := rebuildOffers(in); err != nil || len(offers) != 0 {
		t.Fatalf("expected no offer, got %+v, %v", offers, err)
	}

	in.Claims[rbM1] = claimIdentity{UID: rbClaim1New, Created: now.Add(-2 * time.Minute), VolumeName: "pv-1-new"}
	if got := rebuildRefusal(in, rbM1); got != "" {
		t.Fatalf("a volume no record held is offered, got %q", got)
	}

	for i := len(in.FormerDataClaims); i < maxFormerDataClaims; i++ {
		in.FormerDataClaims = append(in.FormerDataClaims, databasev1alpha1.MemberDataClaim{Name: rbM0,
			ClaimUID: fmt.Sprintf("claim-0-%d", i), VolumeName: fmt.Sprintf("pv-0-%d", i)})
	}
	if got := rebuildRefusal(in, rbM1); !strings.Contains(got, "former") {
		t.Fatalf("expected a refusal once the former records are full, got %q", got)
	}
}
