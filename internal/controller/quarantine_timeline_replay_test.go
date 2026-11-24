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
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// These tests replay measured HA role timelines through decideWriteQuarantine,
// one pass at a time, carrying what the status would hold from one pass to the
// next, and check the write-quarantine safety invariants of ADR-0005, section
// 8, on every pass (checkQuarantineInvariants). Every fixture names where its
// times and roles were recorded. A state the record does not give is never
// filled in with a guess: it is either left out of the replay or listed as
// unknown, in which case every listed possibility is replayed.
//
// Provenance of the fixtures:
//
//   - POC-58 (docs/poc/RESULTS.md, "POC-58"; docs/poc/poc58/samples/*.tar.gz),
//     2026-11-10 UTC, Docker, CUBRID 11.4.6.1963-0e7d3c1, no Kubernetes, no
//     Operator and no Brokers. Every member ran `cubrid heartbeat status` once
//     a second and the sampler kept every field the Instance Manager's parser
//     reads (docs/poc/poc58/sampler.py). The replay rebuilds each call's
//     output from those fields and passes it through instancemanager.
//     ParseHAStatus and observationFromStatus, the path a /v1/role answer
//     takes. Observed: with h-1 the master, a full cut of h-0 or of h-1 made
//     h-0 an active server beside h-1 for 36 to 38 s in all 8 full-cut
//     trials; the heartbeat-only cut made h-0 master only after the heal.
//     Limitations: these are each member's own readings, so the replay shows
//     what an Operator that reached every member would have seen. An Operator
//     across a real cut does not reach the cut member, so each trial is also
//     replayed with the cut member unreachable from the cut to the heal.
//     There are no Pods (fixed identities stand in for them), no fence and
//     no Operator pass times (a pass is replayed at each sample).
//   - POC-40 (docs/poc/RESULTS.md, "POC-40"), 2026-10-27 UTC, VM lab, the
//     installed Operator d6df2ac, before any write quarantine existed, CUBRID
//     11.4.6. The master ha-0 was cut off from the other members and the
//     Operator (case 1 and the stale-master case 3) or its packets to the
//     slaves only (case 2). Observed, as seconds after the fault was injected
//     and polled by `kubectl exec` (each up to one poll late): when ha-1
//     reported itself master, when the removal of the cut began, when ha-1
//     was a slave again and when PrimaryResolved was True again. ha-1's server
//     was never registered_and_active; ha-0 stayed master and active. The
//     manager's log recorded ha-0 unreachable in case 1 and an answer whose
//     node list also named ha-1 master in case 2, and for ha-1 a master
//     whose server is not active or an answer without a role. Limitations:
//     the Operator's own pass times and the individual answers were not
//     kept, the replay starts 30 s before each injection with the formed
//     roles, and the states during the transitions are listed as unknown.
//   - The VM-lab runs of the network partition lane recorded on issue #115,
//     the installed Operator 66ab5ee with the quarantine (engine
//     11.4.7.1995, as recorded for that installation on 2026-11-18 UTC):
//     the three `partition-higher-rank-isolated` runs of 2026-11-17 UTC
//     (10:43, 13:00 and 14:20), in which the master was first moved to ha-1
//     and then the higher-ranked ha-0 was cut off; the
//     `partition-master-isolated` run of 2026-11-18 09:02 UTC with raw
//     heartbeat states; and the `partition-slave-isolated` control of
//     2026-11-18 07:51 UTC. Observed, to the second: the master move, when
//     the cut was issued, when the quarantine started, the heal, the
//     Operator's view during the cut, the roles after the heal and, in the
//     14:20 run, the manual release attempts. Limitations: the sweeps kept
//     no Operator pass times, so every run is replayed at several pass
//     cadences between the recorded instants; the time at which the Operator
//     first lost ha-0 after the cut was issued is unknown (it is replayed
//     both ways until the recorded quarantine start); the last resolved
//     primary is not recorded and is taken as ha-1 after the master move,
//     the only value consistent with the recorded absence of a quarantine
//     during that move; no out-of-service taint was set in any run.
//
// The Kind run of #938 is replayed in quarantine_missed_probe_test.go and the
// POC-50 heartbeat samples in quarantine_evidence_replay_test.go.

// replayClusterUID is the cluster UID of every replay.
const replayClusterUID = "replay-cluster-uid"

// replayPass is one replayed pass: its time, the answers it used and the
// decision.
type replayPass struct {
	at       time.Time
	obs      map[string]RoleObservation
	decision quarantineDecision
}

// quarantineReplay carries what the status, and the Operator's memory for
// PendingSince, would hold from one pass to the next.
type quarantineReplay struct {
	t             *testing.T
	label         string
	members       []string
	lastResolved  string
	identities    map[string]memberIdentity
	verifications []databasev1alpha1.MemberVerification
	quarantine    *databasev1alpha1.WriteQuarantine
	lastRelease   *databasev1alpha1.WriteQuarantineRelease
	pendingSince  map[string]pendingMark
	nonces        int
	passes        []replayPass
}

// newQuarantineReplay is a replay of members, each in one Pod for the whole
// replay, with no status recorded yet.
func newQuarantineReplay(t *testing.T, label string, members []string, lastResolved string) *quarantineReplay {
	r := &quarantineReplay{t: t, label: label, members: members, lastResolved: lastResolved,
		identities: map[string]memberIdentity{}}
	for _, m := range members {
		r.identities[m] = memberIdentity{PodUID: m + "-pod"}
	}
	return r
}

// pass decides one pass at now from obs, checks the invariants and keeps what
// the status would hold after it.
func (r *quarantineReplay) pass(now time.Time, obs map[string]RoleObservation) quarantineDecision {
	r.t.Helper()
	in := quarantineInput{
		Now: now, ClusterUID: replayClusterUID, Members: r.members, Observations: obs,
		Identities: r.identities, LastResolvedPrimary: r.lastResolved, Verifications: r.verifications,
		Quarantine: r.quarantine.DeepCopy(), LastRelease: r.lastRelease.DeepCopy(), ExemptionOwned: true,
		PendingSince: r.pendingSince,
		Nonce: func() (string, error) {
			r.nonces++
			return fmt.Sprintf("replay-nonce-%d", r.nonces), nil
		},
	}
	d, err := decideWriteQuarantine(in)
	if err != nil {
		r.t.Fatalf("%s at %s: %v", r.label, now.Format(time.RFC3339Nano), err)
	}
	checkQuarantineInvariants(r.t, fmt.Sprintf("%s at %s", r.label, now.Format(time.RFC3339Nano)), in, d)
	r.passes = append(r.passes, replayPass{at: now, obs: obs, decision: d})
	r.verifications, r.quarantine, r.pendingSince = d.Verifications, d.Quarantine, d.PendingSince
	r.lastRelease = d.LastRelease
	if d.Released != nil {
		r.lastRelease = d.Released
	}
	return d
}

// checkQuarantineInvariants checks one decision against the safety
// invariants of ADR-0005, section 8, that hold on every pass whatever came
// before it.
func checkQuarantineInvariants(t *testing.T, label string, in quarantineInput, d quarantineDecision) {
	t.Helper()
	before := in.Quarantine
	autoRelease := func(q *databasev1alpha1.WriteQuarantine) bool {
		return q.Reason == databasev1alpha1.WriteQuarantinedMemberUnverified ||
			q.Reason == databasev1alpha1.WriteQuarantinedMemberReturning
	}
	// Rule 1: two or more active masters always leave a quarantine in force
	// that is not released automatically.
	if len(d.ActiveMasters) > 1 && (d.Quarantine == nil || d.Released != nil || autoRelease(d.Quarantine)) {
		t.Fatalf("%s: active masters %v, but quarantine %+v and release %+v", label, d.ActiveMasters,
			d.Quarantine, d.Released)
	}
	// The write path is opened again only by a release, and only beside
	// exactly one active master, with the members' Pods read, from a
	// quarantine that has an automatic release.
	if before != nil && d.Quarantine == nil && d.Released == nil {
		t.Fatalf("%s: the %s quarantine ended without a release", label, before.Reason)
	}
	if d.Released != nil {
		switch {
		case before == nil:
			t.Fatalf("%s: a release without a quarantine: %+v", label, d.Released)
		case d.Quarantine != nil:
			t.Fatalf("%s: released, but a quarantine is still in force: %+v", label, d.Quarantine)
		case len(d.ActiveMasters) != 1:
			t.Fatalf("%s: released beside the active masters %v", label, d.ActiveMasters)
		case in.IdentitiesUnknown:
			t.Fatalf("%s: released without the members' Pods", label)
		case !autoRelease(before):
			t.Fatalf("%s: a %s quarantine was released automatically", label, before.Reason)
		case d.Released.Method != databasev1alpha1.WriteQuarantineReleasedAutomatically:
			t.Fatalf("%s: release method %q in a decision", label, d.Released.Method)
		}
	}
	// A quarantine without an automatic release keeps its reason.
	if before != nil && !autoRelease(before) && (d.Quarantine == nil || d.Quarantine.Reason != before.Reason) {
		t.Fatalf("%s: the %s quarantine became %+v", label, before.Reason, d.Quarantine)
	}
	if before == nil {
		checkQuarantineStart(t, label, in, d)
	}
}

// checkQuarantineStart checks a decision taken with no quarantine in force:
// rule 2 starts one when it applies, and nothing but rules 1 and 2 does.
func checkQuarantineStart(t *testing.T, label string, in quarantineInput, d quarantineDecision) {
	t.Helper()
	// Rule 2: exactly one active master M and an unverified member that
	// outranks M or is the last resolved primary, other than a pending one,
	// start a quarantine in this pass.
	if len(d.ActiveMasters) == 1 && !in.IdentitiesUnknown {
		m := d.ActiveMasters[0]
		for _, u := range d.Unverified {
			outranks := slices.Index(in.Members, u) < slices.Index(in.Members, m)
			if (outranks || u == in.LastResolvedPrimary) && !slices.Contains(d.Pending, u) && d.Quarantine == nil {
				t.Fatalf("%s: %s is unverified beside the only active master %s, and no quarantine started",
					label, u, m)
			}
		}
	}
	// Rule 3: nothing else starts one.
	if d.Quarantine != nil {
		named := len(d.ActiveMasters) > 1
		if len(d.ActiveMasters) == 1 {
			for _, member := range d.Quarantine.Members {
				named = named || slices.Contains(d.Unverified, member.Name)
			}
		}
		if !named {
			t.Fatalf("%s: a %s quarantine started without a trigger (active masters %v, unverified %v)",
				label, d.Quarantine.Reason, d.ActiveMasters, d.Unverified)
		}
	}
}

// heartbeatAnswer is the Operator's observation of an Instance Manager answer
// whose `cubrid heartbeat status` printed out: the parser of the Instance
// Manager, then the Operator's conversion of the answer, as for /v1/role.
func heartbeatAnswer(t *testing.T, member, out string, at time.Time) RoleObservation {
	t.Helper()
	st := instancemanager.ParseHAStatus(out)
	if st.Current != "" && st.Current != member {
		t.Fatalf("the answer of %s names %s", member, st.Current)
	}
	return observationFromStatus(st, at)
}

// heartbeatOutput is the output of `cubrid heartbeat status` with the fields
// the POC samplers kept: the local node and its state, the node list as
// name=state in the printed order, and the server's state ("-" for none).
func heartbeatOutput(members []string, current, state, nodes, server string) string {
	if current == "-" || current == "" {
		return "++ cubrid master is not running.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, " HA-Node Info (current %s, state %s)\n", current, state)
	for n := range strings.SplitSeq(nodes, ",") {
		name, nodeState, _ := strings.Cut(n, "=")
		fmt.Fprintf(&b, "   Node %s (priority %d, state %s)\n", name, slices.Index(members, name)+1, nodeState)
	}
	fmt.Fprintf(&b, "\n HA-Process Info (master 1, state %s)\n", state)
	if server != "-" {
		fmt.Fprintf(&b, "   Server appdb (pid 2, state %s)\n", server)
	}
	return b.String()
}

// The members of POC-58, in ha_node_list order: h-1 was the master before
// every cut.
const (
	poc58H0 = "h-0"
	poc58H1 = "h-1"
	poc58H2 = "h-2"
)

var poc58Members = []string{poc58H0, poc58H1, poc58H2}

// poc58Sample is one `cubrid heartbeat status` call of a POC-58 member.
type poc58Sample struct {
	member string
	// done is when the call returned: its start plus its duration.
	done time.Time
	out  string
}

// poc58Trial is one cut trial of POC-58 as its archive holds it.
type poc58Trial struct {
	name, shape string
	// stable is when the harness saw h-1 master with both slaves, cut and
	// heal when it cut and healed the network, end the final table read.
	stable, cut, heal, end time.Time
	samples                []poc58Sample
}

// poc58Trials are the archives of the cut trials replayed: every trial of
// the three new-only shapes and the two controls.
var poc58Trials = []string{
	"full-h0-new-only-n1", "full-h0-new-only-n2", "full-h0-new-only-n3", "full-h0-new-only-n4",
	"full-h1-new-only-n1", "full-h1-new-only-n2", "full-h1-new-only-n3", "full-h1-new-only-n4",
	"hb-new-only-n1", "hb-new-only-n2", "hb-new-only-n3", "hb-new-only-n4",
	"full-h0-all-n1", "full-h0-all-n2",
}

// epochTime is the time of a POC timestamp in epoch seconds.
func epochTime(t *testing.T, s string) time.Time {
	t.Helper()
	sec, frac, _ := strings.Cut(s, ".")
	whole, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		t.Fatalf("timestamp %q: %v", s, err)
	}
	frac = (frac + "000000000")[:9]
	nanos, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		t.Fatalf("timestamp %q: %v", s, err)
	}
	return time.Unix(whole, nanos).UTC()
}

// loadPOC58Trial reads one trial archive of docs/poc/poc58/samples.
func loadPOC58Trial(t *testing.T, name string) poc58Trial {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "docs", "poc", "poc58", "samples", name+".tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	trial := poc58Trial{name: name}
	archive := tar.NewReader(gz)
	for {
		h, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Base(h.Name)
		member := strings.TrimSuffix(base, ".tsv")
		switch {
		case base == "events.tsv":
			readPOC58Events(t, &trial, archive)
		case strings.HasSuffix(base, ".tsv") && slices.Contains(poc58Members, member):
			scanner := bufio.NewScanner(archive)
			for scanner.Scan() {
				// epoch, exit code, call ms, current node, its state, node
				// list, process state, server state, ... (sampler.py)
				fields := strings.Split(scanner.Text(), "\t")
				if len(fields) < 8 {
					t.Fatalf("%s/%s: short sample %q", name, base, fields)
				}
				ms, err := strconv.Atoi(fields[2])
				if err != nil {
					t.Fatalf("%s/%s: call time %q: %v", name, base, fields[2], err)
				}
				trial.samples = append(trial.samples, poc58Sample{member: member,
					done: epochTime(t, fields[0]).Add(time.Duration(ms) * time.Millisecond),
					out:  heartbeatOutput(poc58Members, fields[3], fields[4], fields[5], fields[7])})
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if trial.stable.IsZero() || trial.cut.IsZero() || trial.heal.IsZero() || trial.end.IsZero() || len(trial.samples) == 0 {
		t.Fatalf("%s: incomplete archive: %+v", name, trial)
	}
	slices.SortStableFunc(trial.samples, func(a, b poc58Sample) int { return a.done.Compare(b.done) })
	return trial
}

// readPOC58Events takes the trial's shape and the times of its stable roles,
// cut, heal and final read from events.tsv.
func readPOC58Events(t *testing.T, trial *poc58Trial, r io.Reader) {
	t.Helper()
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 3 {
			continue
		}
		at := epochTime(t, fields[0])
		switch fields[1] {
		case "stable":
			if trial.stable.IsZero() {
				trial.stable = at
			}
		case "cut":
			trial.cut, trial.shape = at, fields[2]
		case "heal":
			trial.heal = at
		case "final-read":
			trial.end = at
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

// cutMember is the member a POC-58 shape cuts off from every peer, "" for
// the heartbeat-only cut.
func (p poc58Trial) cutMember() string {
	switch p.shape {
	case "full-h0":
		return poc58H0
	case "full-h1":
		return poc58H1
	}
	return ""
}

// replayPOC58 replays trial from its stable roles to its final read, one
// pass at every stride-th sample from the phase-th on, each with the latest
// answer of every member returned by then, from the first sample by which
// every member has answered. h-1, the master before the cut,
// is the last resolved primary. With cutUnreachable, the cut member gives no
// answer from the cut to the heal, as for an Operator across the cut.
func replayPOC58(t *testing.T, trial poc58Trial, stride, phase int, cutUnreachable bool) *quarantineReplay {
	t.Helper()
	label := fmt.Sprintf("POC-58 %s (stride %d, phase %d, cut member unreachable %v)", trial.name, stride, phase,
		cutUnreachable)
	r := newQuarantineReplay(t, label, poc58Members, poc58H1)
	latest := map[string]RoleObservation{}
	cut := trial.cutMember()
	n := 0
	for _, s := range trial.samples {
		if s.done.Before(trial.stable) || s.done.After(trial.end) {
			continue
		}
		o := heartbeatAnswer(t, s.member, s.out, s.done)
		if cutUnreachable && s.member == cut && !s.done.Before(trial.cut) && !s.done.After(trial.heal) {
			o = RoleObservation{Reachable: false, ObservedAt: s.done, Reason: "context deadline exceeded"}
		}
		latest[s.member] = o
		// Each pass asks every member: the replay starts once each one
		// has answered.
		if len(latest) < len(poc58Members) {
			continue
		}
		if n++; (n-1)%stride != phase%stride {
			continue
		}
		obs := maps.Clone(latest)
		r.pass(s.done, obs)
	}
	return r
}

// The POC-58 cut trials, replayed sample by sample through the Instance
// Manager's parser and the decision, with every member reachable as its own
// readings show it: every quarantine starts on an observed trigger, the
// recorded two active servers always find one in force that is not released
// automatically, and no trial ends with the write path open.
func TestQuarantineReplayOfPOC58Cuts(t *testing.T) {
	for _, name := range poc58Trials {
		t.Run(name, func(t *testing.T) {
			trial := loadPOC58Trial(t, name)
			r := replayPOC58(t, trial, 1, 0, false)
			var bothActive, otherActive time.Time
			for _, p := range r.passes {
				if len(p.decision.ActiveMasters) > 1 && bothActive.IsZero() {
					bothActive = p.at
				}
				if otherActive.IsZero() && slices.ContainsFunc(p.decision.ActiveMasters, func(m string) bool {
					return m != poc58H1
				}) {
					otherActive = p.at
				}
				if p.decision.Released != nil {
					t.Fatalf("released at %s: %+v", p.at, p.decision.Released)
				}
			}
			// The shapes as POC-58 recorded them: two active servers in the
			// full cuts, and h-0 an active master in every trial. In the
			// heartbeat-only cut a pass may still combine h-1's last active
			// answer with h-0's first one, about a second apart at the
			// heal, as a pass of the Operator also could.
			if trial.cutMember() != "" && bothActive.IsZero() {
				t.Fatalf("two active masters never seen in the %s shape", trial.shape)
			}
			if otherActive.IsZero() {
				t.Fatalf("h-0 was never an active master in the replay")
			}
			var started time.Time
			for _, p := range r.passes {
				if p.decision.Change == quarantineStarted {
					started = p.at
					break
				}
			}
			if started.IsZero() || started.After(otherActive) || started.Before(trial.cut) {
				t.Fatalf("quarantine started at %v, want between the cut %v and h-0's first active-master answer %v",
					started, trial.cut, otherActive)
			}
			last := r.passes[len(r.passes)-1].decision
			if last.Quarantine == nil || last.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged &&
				last.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMultipleMasters {
				t.Fatalf("final quarantine %+v, want one with a manual release only", last.Quarantine)
			}
		})
	}
}

// The full cuts of POC-58 replayed as an Operator across the cut would see
// them: the cut member answers nothing from the cut to the heal. The two
// active servers are then not seen together before the heal (after it,
// both may still answer active for a moment, and rule 1 applies), but
// h-0's takeover is still
// caught by rule 2 before or as h-0 is first seen active: as an unverified
// member ranked above the master (full-h0) or beside the last resolved
// primary h-1 (full-h1). No release happens before the heal.
func TestQuarantineReplayOfPOC58CutsAcrossTheCut(t *testing.T) {
	for _, name := range poc58Trials {
		trial := loadPOC58Trial(t, name)
		if trial.cutMember() == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			r := replayPOC58(t, trial, 1, 0, true)
			var started *replayPass
			for i, p := range r.passes {
				if len(p.decision.ActiveMasters) > 1 && !p.at.After(trial.heal) {
					t.Fatalf("two active masters seen at %s across the cut: %v", p.at, p.decision.ActiveMasters)
				}
				if p.decision.Change == quarantineStarted && started == nil {
					started = &r.passes[i]
				}
				if p.decision.Released != nil && !p.at.After(trial.heal) {
					t.Fatalf("released during the cut at %s", p.at)
				}
				if started == nil && slices.Contains(p.decision.ActiveMasters, poc58H0) {
					t.Fatalf("h-0 was the active master at %s with no quarantine", p.at)
				}
			}
			if started == nil || started.decision.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
				t.Fatalf("quarantine start %+v, want %s", started, databasev1alpha1.WriteQuarantinedMemberUnverified)
			}
		})
	}
}

// The members of the VM-lab and POC-40 timelines, in ha_node_list order.
const (
	labHA0 = "ha-0"
	labHA1 = "ha-1"
	labHA2 = "ha-2"
)

var labMembers = []string{labHA0, labHA1, labHA2}

// labState is what the Operator saw of one member in a pass, as recorded.
type labState string

const (
	// labActive: master, server registered_and_active, its own line master.
	labActive labState = "active master"
	// labActiveListsHA1: as labActive, with ha-1 listed as master too (POC-40
	// case 2: "the role master contradicts the HA node list").
	labActiveListsHA1 labState = "active master listing ha-1 as master"
	// labToBeActive: master, server registered_and_to_be_active.
	labToBeActive labState = "master with a registered_and_to_be_active server"
	// labNoRole: an answer without a role: to-be-master, a node list without
	// the member, no HA-Node Info line, or a state the lane classified as
	// neither master nor slave without keeping it.
	labNoRole labState = "no role"
	// labSlave: slave, server registered_and_standby.
	labSlave labState = "slave"
	// labUnreachable: no answer, as a request cut off by the partition.
	labUnreachable labState = "unreachable"
)

// labObservation is the observation of member in state s at at, through the
// Operator's conversion of an Instance Manager answer.
func labObservation(t *testing.T, member string, s labState, at time.Time) RoleObservation {
	t.Helper()
	nodes := func(own string, others map[string]string) []instancemanager.NodeState {
		out := make([]instancemanager.NodeState, 0, len(labMembers))
		for i, m := range labMembers {
			state := string(instancemanager.RoleSlave)
			if m == member {
				state = own
			} else if o, ok := others[m]; ok {
				state = o
			}
			out = append(out, instancemanager.NodeState{Name: m, Priority: i + 1, State: state})
		}
		return out
	}
	var st instancemanager.HAStatus
	switch s {
	case labActive:
		st = instancemanager.HAStatus{Current: member, Role: instancemanager.RoleMaster, ServerActive: true,
			Nodes: nodes(string(instancemanager.RoleMaster), nil)}
	case labActiveListsHA1:
		st = instancemanager.HAStatus{Current: member, Role: instancemanager.RoleMaster, ServerActive: true,
			Nodes: nodes(string(instancemanager.RoleMaster), map[string]string{labHA1: string(instancemanager.RoleMaster)})}
	case labToBeActive:
		st = instancemanager.HAStatus{Current: member, Role: instancemanager.RoleMaster,
			Nodes: nodes(string(instancemanager.RoleMaster), nil)}
	case labNoRole:
		st = instancemanager.ParseHAStatus(heartbeatOutput(labMembers, member, "to-be-master",
			member+"=to-be-master", "registered_and_standby"))
	case labSlave:
		st = instancemanager.HAStatus{Current: member, Role: instancemanager.RoleSlave, Nodes: nodes(string(instancemanager.RoleSlave), nil)}
	case labUnreachable:
		return RoleObservation{Reachable: false, ObservedAt: at, Reason: "context deadline exceeded"}
	default:
		t.Fatalf("unknown state %q", s)
	}
	return observationFromStatus(st, at)
}

// labSegment is what the Operator saw from from until the next segment.
// A member with more than one state is unknown in the segment: each state
// is replayed in turn, held for the whole segment.
type labSegment struct {
	from   time.Time
	states map[string][]labState
}

// labTimeline is a recorded run as the Operator saw it.
type labTimeline struct {
	name         string
	lastResolved string
	segments     []labSegment
	end          time.Time
	// recorded are the recorded instants replayed as passes in every
	// cadence, such as a recorded quarantine start.
	recorded []time.Time
}

// labCadences are the pass intervals replayed between recorded instants: 3 s
// is unverifiedResyncInterval, 30 s close to haResyncInterval plus a probe.
var labCadences = []time.Duration{time.Second, unverifiedResyncInterval, 7 * time.Second, 30 * time.Second}

// variants are the choices of every unknown member state, one map from
// segment index and member to state per variant.
func (l labTimeline) variants() []map[string]labState {
	out := []map[string]labState{{}}
	for i, seg := range l.segments {
		for _, m := range labMembers {
			if len(seg.states[m]) < 2 {
				continue
			}
			var next []map[string]labState
			for _, v := range out {
				for _, s := range seg.states[m] {
					c := maps.Clone(v)
					c[fmt.Sprintf("%d/%s", i, m)] = s
					next = append(next, c)
				}
			}
			out = next
		}
	}
	return out
}

// replay replays l at cadence, with the unknown states of variant.
func (l labTimeline) replay(t *testing.T, cadence time.Duration, variant map[string]labState) *quarantineReplay {
	t.Helper()
	var times []time.Time
	for at := l.segments[0].from; !at.After(l.end); at = at.Add(cadence) {
		times = append(times, at)
	}
	times = append(times, l.recorded...)
	slices.SortFunc(times, time.Time.Compare)
	times = slices.Compact(times)
	r := newQuarantineReplay(t, fmt.Sprintf("%s (every %s, %v)", l.name, cadence, variant), labMembers, l.lastResolved)
	for _, at := range times {
		i := len(l.segments) - 1
		for i > 0 && at.Before(l.segments[i].from) {
			i--
		}
		obs := map[string]RoleObservation{}
		for _, m := range labMembers {
			states := l.segments[i].states[m]
			s := states[0]
			if len(states) > 1 {
				s = variant[fmt.Sprintf("%d/%s", i, m)]
			}
			obs[m] = labObservation(t, m, s, at)
		}
		r.pass(at, obs)
	}
	return r
}

// forEachReplay replays l in every variant and cadence.
func (l labTimeline) forEachReplay(t *testing.T, check func(t *testing.T, r *quarantineReplay)) {
	t.Helper()
	for _, cadence := range labCadences {
		for _, v := range l.variants() {
			check(t, l.replay(t, cadence, v))
		}
	}
}

// labStates is a member-to-states map in labMembers order.
func labStates(ha0, ha1, ha2 []labState) map[string][]labState {
	return map[string][]labState{labHA0: ha0, labHA1: ha1, labHA2: ha2}
}

func one(s labState) []labState { return []labState{s} }

// poc40Run is one counted POC-40 run: its case, when the fault was
// injected (UTC), and the timeline columns of RESULTS.md in seconds.
type poc40Run struct {
	name string
	// asymmetric is case 2: the master's packets to the slaves dropped,
	// the Operator not cut off.
	asymmetric bool
	fault      string
	// ha1Master, removal: ha-1 reports itself master, and the removal of
	// the cut began, after the injection; ha1Slave and resolved: ha-1 a
	// slave again, and PrimaryResolved=True again, after the removal began.
	ha1Master, removal, ha1Slave, resolved float64
}

// The counted runs of POC-40 (docs/poc/RESULTS.md, "Timeline, seconds after
// the injection"), on 2026-10-27 UTC.
var poc40Runs = []poc40Run{
	{name: "p115-2", fault: "11:32:20", ha1Master: 10.0, removal: 106.5, ha1Slave: 3.3, resolved: 9.4},
	{name: "p115-4", fault: "11:47:28", ha1Master: 8.6, removal: 105.9, ha1Slave: 1.8, resolved: 9.1},
	{name: "p115-7", fault: "12:39:30", ha1Master: 9.0, removal: 108.0, ha1Slave: 4.1, resolved: 9.2},
	{name: "p115-9", fault: "13:13:14", ha1Master: 10.4, removal: 108.3, ha1Slave: 3.7, resolved: 13.4},
	{name: "p115-5", asymmetric: true, fault: "12:03:47", ha1Master: 8.9, removal: 99.6, ha1Slave: 2.4, resolved: 5.1},
	{name: "p115-8", asymmetric: true, fault: "12:57:04", ha1Master: 7.3, removal: 99.9, ha1Slave: 2.7, resolved: 9.4},
	{name: "p115-6", fault: "12:19:39", ha1Master: 9.8, removal: 226.2, ha1Slave: 2.9, resolved: 9.2},
	{name: "p115-11", fault: "13:52:40", ha1Master: 9.9, removal: 948.5, ha1Slave: 2.7, resolved: 13.2},
}

// labTime is a UTC time of day on day (YYYY-MM-DD).
func labTime(t *testing.T, day, clock string) time.Time {
	t.Helper()
	at, err := time.Parse("2006-01-02 15:04:05.999", day+" "+clock)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// timeline is the run as the Operator saw it. Before ha-1 is seen master,
// ha-0 may still answer (or, in case 1, already be cut off) and ha-1 may be
// a slave or on its way to master; after the removal began, until ha-1 is a
// slave again, either side of each change is possible.
func (p poc40Run) timeline(t *testing.T) labTimeline {
	t.Helper()
	fault := labTime(t, "2026-10-27", p.fault)
	removal := fault.Add(seconds(p.removal))
	cutHA0, transitionHA0 := one(labUnreachable), []labState{labActive, labUnreachable}
	if p.asymmetric {
		cutHA0, transitionHA0 = one(labActiveListsHA1), []labState{labActive, labActiveListsHA1}
	}
	return labTimeline{
		name: "POC-40 " + p.name, lastResolved: labHA0,
		segments: []labSegment{
			{from: fault.Add(-30 * time.Second), states: labStates(one(labActive), one(labSlave), one(labSlave))},
			{from: fault, states: labStates(transitionHA0, []labState{labSlave, labNoRole}, one(labSlave))},
			{from: fault.Add(seconds(p.ha1Master)), states: labStates(cutHA0, []labState{labToBeActive, labNoRole},
				one(labSlave))},
			{from: removal, states: labStates(transitionHA0, []labState{labToBeActive, labNoRole, labSlave},
				one(labSlave))},
			{from: removal.Add(seconds(p.ha1Slave)), states: labStates(one(labActive), one(labSlave), one(labSlave))},
		},
		end: removal.Add(seconds(p.resolved)),
	}
}

// POC-40 on the VM lab: the master ha-0, also the last resolved primary, cut
// off with no other member's server ever becoming active. Whatever the
// unknown transitions and the pass cadence, no quarantine starts: in case 1
// the Operator reaches no active master (rule 3), and in case 2 it reaches
// only ha-0, which no member outranks. The writes ha-0 kept taking were all
// kept in that run, as ADR-0005, section 8, records.
func TestQuarantineReplayOfPOC40Partitions(t *testing.T) {
	for _, run := range poc40Runs {
		t.Run(run.name, func(t *testing.T) {
			run.timeline(t).forEachReplay(t, func(t *testing.T, r *quarantineReplay) {
				for _, p := range r.passes {
					if p.decision.Quarantine != nil {
						t.Fatalf("%s: quarantine at %s: %+v", r.label, p.at, p.decision.Quarantine)
					}
				}
			})
		})
	}
}

// The VM-lab runs of 2026-11-18 UTC on #115 that are no-trigger controls: the
// master ha-0 cut off (09:02, raw heartbeat states kept: ha-1 a heartbeat
// master whose server stayed registered_and_to_be_active, ha-0 unreachable
// in 32 of 32 sweeps), and a lower-ranked slave ha-1 cut off (07:51). No
// quarantine started in either, and none starts in the replay.
func TestQuarantineReplayOfVMLabNoTriggerControls(t *testing.T) {
	day := "2026-11-18"
	for _, l := range []labTimeline{
		{
			name: "partition-master-isolated 2026-11-18 09:02", lastResolved: labHA0,
			segments: []labSegment{
				// LAB01 passed with ha-0 master and both slaves.
				{from: labTime(t, day, "08:58:56"), states: labStates(one(labActive), one(labSlave), one(labSlave))},
				// Cut issued; until it was confirmed the Operator's view is
				// not recorded.
				{from: labTime(t, day, "09:02:30.8"), states: labStates([]labState{labActive, labUnreachable},
					[]labState{labSlave, labNoRole, labToBeActive}, one(labSlave))},
				{from: labTime(t, day, "09:02:53.7"), states: labStates(one(labUnreachable), one(labToBeActive),
					one(labSlave))},
				// Seen removed: ha-0 active again, ha-1 and ha-2 slaves.
				{from: labTime(t, day, "09:05:40.3"), states: labStates(one(labActive), one(labSlave), one(labSlave))},
			},
			end: labTime(t, day, "09:07:35.5"),
		},
		{
			name: "partition-slave-isolated 2026-11-18 07:51", lastResolved: labHA0,
			segments: []labSegment{
				{from: labTime(t, day, "07:47:26"), states: labStates(one(labActive), one(labSlave), one(labSlave))},
				{from: labTime(t, day, "07:51:00.1"), states: labStates(one(labActive),
					[]labState{labSlave, labUnreachable}, one(labSlave))},
				{from: labTime(t, day, "07:51:23.8"), states: labStates(one(labActive), one(labUnreachable),
					one(labSlave))},
				{from: labTime(t, day, "07:54:14.4"), states: labStates(one(labActive), one(labSlave), one(labSlave))},
			},
			end: labTime(t, day, "07:56:21"),
		},
	} {
		t.Run(l.name, func(t *testing.T) {
			l.forEachReplay(t, func(t *testing.T, r *quarantineReplay) {
				for _, p := range r.passes {
					if p.decision.Quarantine != nil {
						t.Fatalf("%s: quarantine at %s: %+v", r.label, p.at, p.decision.Quarantine)
					}
				}
			})
		})
	}
}

// higherRankRun is one `partition-higher-rank-isolated` run of 2026-11-17
// UTC on #115: the master moved to ha-1, then the higher-ranked ha-0 cut off.
type higherRankRun struct {
	name string
	// slaveAgain: ha-0 back as a slave after the master move; cut: the cut
	// issued; started: the quarantine's recorded start; heal: the cut
	// removed; end: the last recorded sweep.
	slaveAgain, cut, started, heal, end string
	// healedHA0 is what ha-0 answered after the heal.
	healedHA0 labState
}

var higherRankRuns = []higherRankRun{
	// 10:43: started 10:47:19, new-connection writes stopped within 11.9 s of
	// the cut; after the heal ha-0's state was classified "other" only.
	{name: "10:43", slaveAgain: "10:46:26", cut: "10:47:07", started: "10:47:19", heal: "10:50:22",
		end: "10:55:22", healedHA0: labNoRole},
	// 13:00: started 13:01:30, 12.1 s after the cut was issued.
	{name: "13:00", slaveAgain: "13:00:35", cut: "13:01:17.9", started: "13:01:30", heal: "13:04:35",
		end: "13:09:38", healedHA0: labToBeActive},
	// 14:20: started 14:24:01, 6.6 s after the cut was issued.
	{name: "14:20", slaveAgain: "14:23:15", cut: "14:23:54.4", started: "14:24:01", heal: "14:27:06",
		end: "14:52:45", healedHA0: labToBeActive},
}

func (h higherRankRun) timeline(t *testing.T) labTimeline {
	t.Helper()
	day := "2026-11-17"
	at := func(clock string) time.Time { return labTime(t, day, clock) }
	return labTimeline{
		name: "partition-higher-rank-isolated 2026-11-17 " + h.name, lastResolved: labHA1,
		segments: []labSegment{
			{from: at(h.slaveAgain), states: labStates(one(labSlave), one(labActive), one(labSlave))},
			// From the cut to the recorded start, the Operator had either
			// not lost ha-0 yet or had.
			{from: at(h.cut), states: labStates([]labState{labSlave, labUnreachable}, one(labActive), one(labSlave))},
			// During the cut: ha-1 active master, ha-2 slave, ha-0
			// unreachable.
			{from: at(h.started), states: labStates(one(labUnreachable), one(labActive), one(labSlave))},
			// At the heal ha-1 answered slave and ha-2 slave; ha-0 stayed
			// heartbeat master with no active server.
			{from: at(h.heal), states: labStates(one(h.healedHA0), one(labSlave), one(labSlave))},
		},
		end:      at(h.end),
		recorded: []time.Time{at(h.started)},
	}
}

// The three VM-lab runs of #115 in which the higher-ranked ha-0 was cut off
// beside the master ha-1. As recorded, the quarantine starts as
// WriteQuarantinedMemberUnverified naming ha-0 beside ha-1, at the first pass
// that does not reach ha-0 and no later than the recorded start; it becomes
// WriteQuarantinedMasterChanged once ha-1 is verified as no longer an active
// master after the heal, and is never released automatically, although no
// member is an active master for the rest of the run.
func TestQuarantineReplayOfVMLabHigherRankCuts(t *testing.T) {
	for _, run := range higherRankRuns {
		t.Run(run.name, func(t *testing.T) {
			l := run.timeline(t)
			started, heal := l.recorded[0], l.segments[3].from
			l.forEachReplay(t, func(t *testing.T, r *quarantineReplay) {
				var start *replayPass
				for i, p := range r.passes {
					lost := !p.obs[labHA0].Reachable
					switch {
					case start == nil && !lost && p.decision.Quarantine != nil:
						t.Fatalf("%s: quarantine at %s while ha-0 answered: %+v", r.label, p.at, p.decision.Quarantine)
					case start == nil && lost:
						start = &r.passes[i]
						q := p.decision.Quarantine
						if q == nil || p.decision.Change != quarantineStarted ||
							q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != labHA1 ||
							!slices.Equal(memberNamesOf(q.Members), []string{labHA0}) {
							t.Fatalf("%s: first pass without ha-0 at %s decided %+v", r.label, p.at, p.decision)
						}
					case start != nil && (p.decision.Quarantine == nil || p.decision.Released != nil):
						t.Fatalf("%s: quarantine not in force at %s: %+v", r.label, p.at, p.decision)
					}
					if p.at.Before(heal) && p.decision.Quarantine != nil &&
						p.decision.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
						t.Fatalf("%s: escalated before the heal at %s", r.label, p.at)
					}
				}
				if start == nil || start.at.After(started) {
					t.Fatalf("%s: started %+v, want by the recorded %s", r.label, start, started)
				}
				last := r.passes[len(r.passes)-1].decision
				if last.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged || len(last.ActiveMasters) != 0 {
					t.Fatalf("%s: at the end %+v beside %v, want %s beside no active master", r.label,
						last.Quarantine, last.ActiveMasters, databasev1alpha1.WriteQuarantinedMasterChanged)
				}
			})
		})
	}
}

// The manual release attempts of the 14:20 run on #115, an administrator's
// decision kept apart from the automatic release: at 14:47:09 a token of an
// earlier revision of the quarantine generation was set and ignored; at
// 14:47:42 the current token was set and refused, because no member was an
// active master. The replay reaches the same verdicts, and a release is not
// accepted in any cadence.
func TestQuarantineReplayOfVMLabManualReleaseAttempts(t *testing.T) {
	run := higherRankRuns[2]
	l := run.timeline(t)
	l.end = labTime(t, "2026-11-17", "14:47:07")
	l.forEachReplay(t, func(t *testing.T, r *quarantineReplay) {
		var earlierToken string
		for _, p := range r.passes {
			if q := p.decision.Quarantine; q != nil && q.Reason == databasev1alpha1.WriteQuarantinedMemberUnverified {
				earlierToken = q.ReleaseToken
			}
		}
		stored := r.quarantine.DeepCopy()
		if stored == nil || stored.Reason != databasev1alpha1.WriteQuarantinedMasterChanged || earlierToken == "" ||
			earlierToken == stored.ReleaseToken {
			t.Fatalf("%s: quarantine before the attempts %+v, earlier token %q", r.label, stored, earlierToken)
		}
		for _, attempt := range []struct {
			clock, token string
			want         manualReleaseVerdict
			contains     string
		}{
			{clock: "14:47:09", token: earlierToken, want: manualReleaseInert, contains: "not the current release token"},
			{clock: "14:47:42", token: stored.ReleaseToken, want: manualReleaseRefused,
				contains: "no member is an active master"},
		} {
			at := labTime(t, "2026-11-17", attempt.clock)
			obs := map[string]RoleObservation{labHA0: labObservation(t, labHA0, labToBeActive, at),
				labHA1: labObservation(t, labHA1, labSlave, at), labHA2: labObservation(t, labHA2, labSlave, at)}
			d := r.pass(at, obs)
			cluster := storedWith(attempt.token, r.quarantine.DeepCopy())
			cluster.UID = replayClusterUID
			verdict, msg, rel := decideManualRelease(manualReleaseInput{Now: at, ClusterUID: replayClusterUID,
				Stored: cluster, Decision: d, Identities: r.identities})
			if verdict != attempt.want || !strings.Contains(msg, attempt.contains) || rel != nil {
				t.Fatalf("%s: attempt at %s: %q %q %+v, want %q containing %q", r.label, attempt.clock, verdict, msg,
					rel, attempt.want, attempt.contains)
			}
		}
	})
}

// FuzzQuarantinePOC58Replay replays a POC-58 cut trial at Operator pass
// cadences other than one pass per sample: every stride-th sample from the
// phase-th on, with or without the cut member reachable. Every pass must keep
// the invariants (checkQuarantineInvariants), and two active masters seen in
// a pass must find a quarantine in force. Its seeds cover every trial at
// strides of 1, 3 and 7 samples, so plain go test stays bounded; go test
// -fuzz explores other cadences.
func FuzzQuarantinePOC58Replay(f *testing.F) {
	for i := range poc58Trials {
		for _, stride := range []uint8{1, 3, 7} {
			f.Add(uint8(i), stride, uint8(0), false)
			f.Add(uint8(i), stride, stride/2, true)
		}
	}
	trials := map[uint8]poc58Trial{}
	f.Fuzz(func(t *testing.T, index, stride, phase uint8, cutUnreachable bool) {
		if int(index) >= len(poc58Trials) || stride == 0 || stride > 30 {
			t.Skip()
		}
		trial, ok := trials[index]
		if !ok {
			trial = loadPOC58Trial(t, poc58Trials[index])
			trials[index] = trial
		}
		r := replayPOC58(t, trial, int(stride), int(phase), cutUnreachable)
		for _, p := range r.passes {
			if len(p.decision.ActiveMasters) > 1 && p.decision.Quarantine == nil {
				t.Fatalf("%s: two active masters at %s with no quarantine", r.label, p.at)
			}
		}
	})
}
