//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// rebuildMemberAnnotation is the annotation with which an administrator
// requests the rebuild of a member (docs/observability.md, "Rebuilding a
// member").
const rebuildMemberAnnotation = "database.cubrid.io/rebuild-member"

// releaseWriteQuarantineAnnotation is the annotation with which an
// administrator releases a write quarantine (docs/observability.md, "Manual
// release").
const releaseWriteQuarantineAnnotation = "database.cubrid.io/release-write-quarantine"

// rebuildTarget picks the slave whose storage the rebuild step empties.
// members are the cluster's members in the order of their ordinals. A member
// that gives no role while it outranks the only active master (has a lower
// ordinal) is an unverified member, and the Operator quarantines the
// read-write path for it (ADR-0005, section 8); a member with an empty data
// volume never answers, so that quarantine is not released automatically. The
// first slave ranked below the master is therefore preferred. When every
// slave outranks the master, the first slave is returned with outranksMaster
// set, and the step has to release the quarantine as an administrator would.
// A master that is not among members counts as ranked below every slave.
func rebuildTarget(master string, slaves, members []string) (member string, outranksMaster bool) {
	if at := slices.Index(members, master); at >= 0 {
		for _, m := range members[at+1:] {
			if slices.Contains(slaves, m) {
				return m, false
			}
		}
	}
	return slaves[0], true
}

// otherQuarantineGrace bounds how long the rebuild step waits while a write
// quarantine records a member besides the one whose storage it replaced. A
// single role probe that the other slave misses while the emptied member
// rejoins is enough to record it (ADR-0005, section 8, rule accepted in
// #1019), and the Operator does not remove a member from a quarantine when it
// answers again: such a quarantine ends automatically only when every member
// other than the active master is verified for about ten seconds
// (releaseCheck in internal/controller/quarantine.go). A member with an empty
// volume never answers, so the quarantine does not end by itself, and no
// rebuild is offered while it is in force (#1139). The step releases it as an
// administrator would only under the conditions missedProbeReleaseRefusal
// checks; it waits this long for them to hold and for the Operator to accept
// the release, then fails with what the quarantine records instead of
// waiting out a longer bound.
const otherQuarantineGrace = 90 * time.Second

// memberIdentity is a member's Pod UID and the restart count of its database
// container, the identity a write quarantine records for each member.
type memberIdentity struct {
	podUID       string
	restartCount int32
}

// missedProbeReleaseRefusal decides whether the rebuild step may release the
// write quarantine in force in c, which records members besides the one whose
// storage the step replaced (emptied, now in the Pod emptiedPodUID), with or
// without that member. Such a quarantine does not end by itself either: the
// automatic release needs every member other than the active master verified,
// emptied included. The maintainer allowed the release on #1139 only for a
// quarantine that records emptied together with another slave, and only when,
// in this read:
//
//  1. one active master is observed, and it is the master the step started
//     with. PrimaryResolved is True with currentPrimary that master, or it is
//     False with the reason PrimaryObservationIncomplete while emptied is the
//     only member in status.instances without a recognised role (master,
//     slave or replica). The emptied member gives no role until it is
//     rebuilt, and the Operator does not resolve a primary while any member
//     lacks an authoritative role (resolvePrimary in
//     internal/controller/haresolve.go); the rebuild is not offered while the
//     quarantine holds, so requiring True would wait for ever (Kind run
//     45403615696; maintainer decision on #1139, comment 7148109339). In
//     both cases status.instances shows exactly one member whose role is
//     master, that member was observed no more than 60 seconds before now,
//     the quarantine's observedMaster is that master, and the quarantine does
//     not record it;
//  2. every recorded member other than emptied answered slave in the latest
//     observation (its lastObservedTime is the cluster's), and its Pod UID and
//     container restart count, read now and recorded by the quarantine, are
//     those it had before the step started (before).
//
// The third condition, the documented annotation with the current token, is
// how the caller releases. It returns why the release is refused, empty when
// it may be made; final is set when the refusal cannot change by waiting, as
// for a member whose Pod was replaced or restarted.
func missedProbeReleaseRefusal(c *databasev1alpha1.CubridCluster, emptied, emptiedPodUID, master string,
	before, current map[string]memberIdentity, now time.Time) (refusal string, final bool) {
	q := c.Status.WriteQuarantine
	switch {
	case q == nil:
		return "no write quarantine is in force", false
	case q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified:
		return fmt.Sprintf("the quarantine's reason is %s, not %s", q.Reason,
			databasev1alpha1.WriteQuarantinedMemberUnverified), true
	}
	if len(otherQuarantineMembers(q, emptied)) == 0 {
		return fmt.Sprintf("the quarantine records no member besides %s", emptied), false
	}
	// A quarantine of other slaves alone, as one started after the emptied
	// member's quarantine was released, is not covered by the decision on
	// #1139, so the step leaves it alone and fails once it outlasts the grace.
	if !slices.ContainsFunc(q.Members, func(m databasev1alpha1.WriteQuarantineMember) bool {
		return m.Name == emptied
	}) {
		return fmt.Sprintf("the quarantine does not record %s, whose storage this step replaced", emptied), false
	}
	// The emptied member, when recorded, is recorded in its new Pod: a record
	// of an earlier Pod is revised, with a new token, once the Operator sees
	// the new one.
	if i := slices.IndexFunc(q.Members, func(m databasev1alpha1.WriteQuarantineMember) bool {
		return m.Name == emptied
	}); i >= 0 && q.Members[i].PodUID != emptiedPodUID {
		return fmt.Sprintf("the quarantine records %s in the Pod %q, not in its new Pod", emptied, q.Members[i].PodUID),
			false
	}

	// Condition 1: one active master is resolved and is not recorded.
	s := c.Status
	instance := func(name string) *databasev1alpha1.InstanceStatus {
		if j := slices.IndexFunc(s.Instances, func(x databasev1alpha1.InstanceStatus) bool {
			return x.Name == name
		}); j >= 0 {
			return &s.Instances[j]
		}
		return nil
	}
	var masters, roleless []string
	for _, x := range s.Instances {
		switch x.Role {
		case databasev1alpha1.RoleMaster:
			masters = append(masters, x.Name)
		case databasev1alpha1.RoleSlave, databasev1alpha1.RoleReplica:
		default:
			roleless = append(roleless, x.Name)
		}
	}
	const unmet = "condition 1 (one active master, the one this step started with) is not met: "
	if resolved := conditionIs(c, "PrimaryResolved", "True"); resolved != nil {
		// While emptied has no database it gives no role, and the Operator
		// reports the observation as incomplete. Another member without a
		// role, or another reason, is not covered by the decision on #1139.
		if !slices.ContainsFunc(s.Conditions, func(x metav1.Condition) bool {
			return x.Type == "PrimaryResolved" && x.Status == metav1.ConditionFalse &&
				x.Reason == "PrimaryObservationIncomplete"
		}) {
			return unmet + resolved.Error(), false
		}
		for _, name := range roleless {
			if name != emptied {
				return fmt.Sprintf(unmet+"PrimaryResolved is False (PrimaryObservationIncomplete) and %s has no "+
					"recognised role (%q) besides %s", name, instance(name).Role, emptied), false
			}
		}
	} else if len(masters) == 1 && masters[0] != s.CurrentPrimary {
		return fmt.Sprintf(unmet+"currentPrimary is %q and the members whose role is master are %v",
			s.CurrentPrimary, masters), false
	}
	switch {
	case len(masters) != 1:
		return fmt.Sprintf(unmet+"currentPrimary is %q and the members whose role is master are %v",
			s.CurrentPrimary, masters), false
	case masters[0] != master:
		return fmt.Sprintf(unmet+"the master is %s, not %s, the master this step started with", masters[0],
			master), true
	}
	// Condition 2 compares each member's answer with the cluster's latest
	// observation, so a status without one is not read further.
	if s.LastObservedTime == nil {
		return unmet + "status.lastObservedTime is unset", false
	}
	if x := instance(master); x.LastObservedTime == nil || now.Sub(x.LastObservedTime.Time) > 60*time.Second {
		return fmt.Sprintf(unmet+"the master %s has no observation time or was last observed more than 60 "+
			"seconds before %s", master, now.UTC().Format(time.RFC3339)), false
	}
	switch {
	case q.ObservedMaster != master:
		return fmt.Sprintf(unmet+"the quarantine's observed master is %q, not %s", q.ObservedMaster, master),
			false
	case slices.ContainsFunc(q.Members, func(m databasev1alpha1.WriteQuarantineMember) bool {
		return m.Name == master
	}):
		return fmt.Sprintf(unmet+"the quarantine records the master %s", master), false
	}

	// Condition 2: every other recorded member answers slave from the Pod
	// and container start it had before the step.
	for _, m := range q.Members {
		if m.Name == emptied {
			continue
		}
		was, known := before[m.Name]
		if !known {
			return fmt.Sprintf("condition 2 (the other recorded members answer slave from the same Pod) is not "+
				"met: the Pod of %s was not read before the step", m.Name), true
		}
		if got, read := current[m.Name]; !read || got != was || m.PodUID != was.podUID ||
			m.RestartCount != was.restartCount {
			return fmt.Sprintf("condition 2 (the other recorded members answer slave from the same Pod) is not "+
				"met: %s was in the Pod %s (restart count %d) before the step; it is now in %q (%d), and the "+
				"quarantine records %q (%d)", m.Name, was.podUID, was.restartCount, got.podUID, got.restartCount,
				m.PodUID, m.RestartCount), true
		}
		x := instance(m.Name)
		switch {
		case x == nil:
			return fmt.Sprintf("condition 2 (the other recorded members answer slave from the same Pod) is not "+
				"met: status.instances has no %s", m.Name), false
		case x.Role != databasev1alpha1.RoleSlave:
			return fmt.Sprintf("condition 2 (the other recorded members answer slave from the same Pod) is not "+
				"met: the role of %s is %q, not slave", m.Name, x.Role), false
		case x.LastObservedTime == nil || !x.LastObservedTime.Equal(s.LastObservedTime):
			return fmt.Sprintf("condition 2 (the other recorded members answer slave from the same Pod) is not "+
				"met: %s did not answer in the latest observation (%s)", m.Name,
				s.LastObservedTime.UTC().Format(time.RFC3339)), false
		}
	}
	return "", false
}

// quarantineSummary describes q for a test message: its reason, start time,
// revision, the members it records with the role each was observed with, and
// the last refusal of a release, if any.
func quarantineSummary(q *databasev1alpha1.WriteQuarantine) string {
	if q == nil {
		return "no write quarantine is in force"
	}
	members := make([]string, 0, len(q.Members))
	for _, m := range q.Members {
		role := m.ObservedRole
		if role == "" {
			role = "role not recorded"
		}
		members = append(members, fmt.Sprintf("%s (%s)", m.Name, role))
	}
	s := fmt.Sprintf("write quarantine %s (reason %s, started %s, revision %d, observed master %q) records %s",
		q.GenerationID, q.Reason, q.StartTime.UTC().Format(time.RFC3339), q.Revision, q.ObservedMaster,
		strings.Join(members, ", "))
	if q.ReleaseRefusal != "" {
		s += fmt.Sprintf("; last release refusal: %q", q.ReleaseRefusal)
	}
	return s
}

// otherQuarantineMembers returns the members q records besides member; none
// when no quarantine is in force.
func otherQuarantineMembers(q *databasev1alpha1.WriteQuarantine, member string) []string {
	if q == nil {
		return nil
	}
	var others []string
	for _, m := range q.Members {
		if m.Name != member {
			others = append(others, m.Name)
		}
	}
	return others
}

// otherMembersWait measures how long, within one wait of the rebuild step, a
// write quarantine has recorded members besides the member whose storage was
// replaced.
type otherMembersWait struct {
	member string
	grace  time.Duration
	since  time.Time
}

// observe takes the quarantine read at now. It returns the other members the
// quarantine records, and a failure message once it has recorded any of them
// for longer than the grace without a break; the measure starts over when a
// read shows none.
func (w *otherMembersWait) observe(q *databasev1alpha1.WriteQuarantine,
	now time.Time) (others []string, failure string) {
	others = otherQuarantineMembers(q, w.member)
	if len(others) == 0 {
		w.since = time.Time{}
		return nil, ""
	}
	if w.since.IsZero() {
		w.since = now
	}
	if waited := now.Sub(w.since); waited > w.grace {
		failure = fmt.Sprintf("a write quarantine has recorded %s besides %s, whose storage this step replaced, "+
			"for %s; it cannot end by itself while %s has no database, and no rebuild is offered while it "+
			"is in force: %s",
			strings.Join(others, ", "), w.member, waited.Round(time.Second), w.member, quarantineSummary(q))
	}
	return others, failure
}

// memberRebuildLimit bounds the wait from the accepted request until the
// rebuild ends. The copy is a backup on the source, an upload to and a
// download from the mock object store, restoredb, the start of the member as
// a slave and its catch-up; with the small database of this run each part
// takes seconds. The bound is generous because a shared CI runner can be
// several times slower than a workstation, and a rebuild that does not end
// within it is still a failure of the step.
const memberRebuildLimit = 10 * time.Minute

// memberRebuildStep registers the rebuild of a member that lost its storage
// on the real engine (#1043): the test, as an administrator would, deletes a
// slave's data claim and Pod, reads the token the Operator offers for the
// member, and requests the rebuild with the annotation. Before the request,
// a value with a wrong token and one naming the master are shown to start
// nothing. The rebuild has to end Completed, the member has to answer as a
// slave again with its ownership marker released, and every member and both
// Services have to hold every acknowledged row, including the rows written
// while the member was gone, while it was rebuilt and after it was rebuilt.
//
// The rebuilt member's Instance Manager asks the source's Instance Manager
// whether it is the active master before it restores and again before it
// starts the member, and refuses the restore (RebuildSourceUnconfirmed)
// when it gets no answer. A Completed rebuild therefore also shows that the
// cluster's NetworkPolicy admits one member's Instance Manager to another's
// (#1045).
//
// The slave emptied is one ranked below the master when there is one. When
// the earlier steps left the member with the highest ordinal as master, every
// slave outranks it, and the emptied slave starts a write quarantine that
// cannot end by itself (rebuildTarget). The step then releases it with the
// documented manual release before it writes again; the output and
// member-rebuild/result.txt say which of the two the run took (#1058).
//
// The member's Pod and claim are replaced, and the step leaves the cluster
// in the common starting state, which the next step verifies. It records the
// rebuild after the loss of a member's volume (scenario S10 of
// docs/testing/scenario-contract.md) in the summary, judged by
// rebuild_limit, 5 minutes by default (E2E_REBUILD_LIMIT); with it unset the
// result is blocked with time_limit_unset. The kind lane's own gate does not
// require it; the release-candidate verifier does. A client keeps
// writing through the read-write Service while the rebuild runs, so the data
// check includes rows written during it; with the limit set, the result is
// still blocked when no such insert was acknowledged or when the new
// volume's database directory could not be listed. It runs in every full
// Kind run; E2E_LABEL_FILTER='ha-setup || member-rebuild' (or
// ha-pvc-loss-rebuild) runs it alone on a formed cluster.
func memberRebuildStep(r *haRun) {
	const text = "rebuilds a slave that lost its storage on request, and the rebuilt member holds every acknowledged row"
	It(text, Label(evidence.KindLabels("S10", "member-rebuild")...), func() {
		r.enter(nil)
		result := evidence.Scenario{ID: "S10", Result: evidence.Fail, Reason: "a check of S10 failed; see the test output"}
		// Recorded also when a check below fails.
		defer func() { recordScenario(result) }()
		operations := evidence.Operations{}
		count := func(h workload.History) {
			operations.Attempted += h.Counts[workload.Attempted]
			operations.Acknowledged += h.Counts[workload.Acknowledged]
			operations.Failed += h.Counts[workload.Failed]
			operations.Unknown += h.Counts[workload.Unknown]
		}
		r.mu.Lock()
		r.markers++
		clients := fmt.Sprintf("rb%d", r.markers)
		r.mu.Unlock()
		master, slaves, err := r.master(context.Background())
		Expect(err).NotTo(HaveOccurred())
		// Never the master: the Operator refuses to rebuild the current
		// primary, and losing the master's storage is a failover, not this
		// step.
		member, outranksMaster := rebuildTarget(master, slaves, r.members)
		claim := "data-" + member
		_, _ = fmt.Fprintf(GinkgoWriter, "rebuilding %s; the master is %s; %s outranks the master and needs "+
			"a manual release of the write quarantine: %t\n", member, master, member, outranksMaster)
		// readIdentity returns a member's Pod UID and the restart count of
		// its database container; both empty when it has no Pod.
		readIdentity := func(m string) (memberIdentity, error) {
			out, err := r.kubectlQuiet(context.Background(), "get", "pod", m, "--ignore-not-found",
				"-o", `jsonpath={.metadata.uid}{"\t"}{.status.containerStatuses[?(@.name=="cubrid")].restartCount}`)
			if err != nil || strings.TrimSpace(out) == "" {
				return memberIdentity{}, err
			}
			f := strings.Split(strings.TrimSpace(out), "\t")
			id := memberIdentity{podUID: f[0]}
			if len(f) == 2 && f[1] != "" {
				n, err := strconv.ParseInt(f[1], 10, 32)
				if err != nil {
					return memberIdentity{}, fmt.Errorf("the restart count of %s: %q: %w", m, out, err)
				}
				id.restartCount = int32(n)
			}
			return id, nil
		}
		// The other members' Pods and container starts before the step: a
		// quarantine that records one of them is released only while it is
		// unchanged (missedProbeReleaseRefusal).
		identitiesBefore := map[string]memberIdentity{}
		for _, m := range r.members {
			if m == member {
				continue
			}
			id, err := readIdentity(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(id.podUID).NotTo(BeEmpty(), "the Pod %s", m)
			identitiesBefore[m] = id
		}

		readCluster := func() (*databasev1alpha1.CubridCluster, error) {
			out, err := r.kubectlQuiet(context.Background(), "get", "cubridcluster", r.cluster,
				"--request-timeout=30s", "-o", "json")
			if err != nil {
				return nil, err
			}
			c := &databasev1alpha1.CubridCluster{}
			return c, json.Unmarshal([]byte(out), c)
		}
		// readClaim returns the UID, the volume and the deletion time of
		// the member's data claim; all empty when there is none.
		readClaim := func() (uid, volume, deleting string, err error) {
			out, err := r.kubectlQuiet(context.Background(), "get", "pvc", claim, "--ignore-not-found",
				"-o", `jsonpath={.metadata.uid}{"\t"}{.spec.volumeName}{"\t"}{.metadata.deletionTimestamp}`)
			if err != nil || strings.TrimSpace(out) == "" {
				return "", "", "", err
			}
			f := strings.Split(out, "\t")
			if len(f) != 3 {
				return "", "", "", fmt.Errorf("unexpected output for the PVC %s: %q", claim, out)
			}
			return f[0], f[1], f[2], nil
		}
		podUID := func() (string, error) {
			return r.kubectlQuiet(context.Background(), "get", "pod", member, "--ignore-not-found",
				"-o", "jsonpath={.metadata.uid}")
		}
		annotate := func(value string) {
			_, err := r.kubectl(context.Background(), "annotate", "cubridcluster", r.cluster,
				rebuildMemberAnnotation+"="+value, "--overwrite")
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
		// No annotation is left behind, whatever happens below: a refused or
		// ignored value stays until it is removed.
		DeferCleanup(func() {
			_, _ = r.kubectl(context.Background(), "annotate", "cubridcluster", r.cluster,
				rebuildMemberAnnotation+"-")
		})
		// What a reader needs to judge a failed rebuild is kept with the
		// evidence of the run.
		DeferCleanup(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			for name, args := range map[string][]string{
				"cluster.yaml": {"-n", r.namespace, "get", "cubridcluster", r.cluster, "-o", "yaml"},
				"events.txt":   {"-n", r.namespace, "get", "events", "--sort-by=.lastTimestamp"},
				"pods.txt":     {"-n", r.namespace, "get", "pods,pvc", "-o", "wide"},
				"member.log":   {"-n", r.namespace, "logs", member, "--tail=500"},
				"source.log":   {"-n", r.namespace, "logs", master, "--tail=500"},
				"operator.log": {"-n", namespace, "logs", "-l", operatorSelector, "--tail=1000"},
			} {
				out, _ := utils.RunQuiet(exec.Command("kubectl", append(args, "--request-timeout=20s")...))
				writeScenarioFile("member-rebuild/"+name, []byte(out))
			}
		})

		By("writing rows through the read-write Service before the storage is lost")
		h, err := r.runWorkload(context.Background(), "CLIENT_ID="+clients+"-before OPS=10")
		Expect(err).NotTo(HaveOccurred())
		count(h)
		Expect(h.Counts[workload.Acknowledged]).To(BeNumerically(">", 0), "%v", h.Counts)

		c, err := readCluster()
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Status.WriteQuarantine).To(BeNil(), "a rebuild is never offered under a write quarantine")
		Expect(c.Status.MemberRebuild).To(BeNil(), "another rebuild is in progress")
		Expect(slices.ContainsFunc(c.Status.MemberDataClaims, func(d databasev1alpha1.MemberDataClaim) bool {
			return d.Name == member
		})).To(BeTrue(), "the claim %s that holds the database is recorded in status.memberDataClaims", claim)
		oldClaimUID, oldVolume, _, err := readClaim()
		Expect(err).NotTo(HaveOccurred())
		Expect(oldClaimUID).NotTo(BeEmpty(), "the claim %s", claim)
		oldPodUID, err := podUID()
		Expect(err).NotTo(HaveOccurred())
		// The claims of the master and of the other slave are read before
		// the loss and again after the data check: a rebuild that replaced
		// or targeted one of them would change its UID.
		otherClaims := func() map[string]string {
			uids := map[string]string{}
			for _, m := range r.members {
				if m == member {
					continue
				}
				uid, err := r.kubectlQuiet(context.Background(), "get", "pvc", "data-"+m, "-o", "jsonpath={.metadata.uid}")
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				ExpectWithOffset(1, uid).NotTo(BeEmpty(), "the claim data-%s", m)
				uids[m] = uid
			}
			return uids
		}
		otherClaimsBefore := otherClaims()

		By("deleting the data claim and the Pod of " + member + ", as an administrator replacing lost storage")
		// The claim is deleted first and stays until its Pod is gone; the
		// StatefulSet then creates an empty claim and a new Pod. The
		// manager has no right to delete a claim; the test does it.
		_, err = r.kubectl(context.Background(), "delete", "pvc", claim, "--wait=false")
		Expect(err).NotTo(HaveOccurred())
		_, err = r.kubectl(context.Background(), "delete", "pod", member, "--timeout=5m")
		Expect(err).NotTo(HaveOccurred())
		lostAt := time.Now()
		var newClaimUID, newPodUID string
		Eventually(func(g Gomega) {
			uid, volume, deleting, err := readClaim()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(uid).NotTo(BeEmpty(), "a new claim %s", claim)
			g.Expect(uid).NotTo(Equal(oldClaimUID), "the claim %s is replaced", claim)
			g.Expect(deleting).To(BeEmpty(), "the new claim is not being deleted")
			g.Expect(volume).NotTo(BeEmpty(), "the new claim is bound")
			g.Expect(volume).NotTo(Equal(oldVolume), "the new claim is bound to a new volume")
			pod, err := podUID()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pod).NotTo(BeEmpty(), "a new Pod %s", member)
			g.Expect(pod).NotTo(Equal(oldPodUID), "the Pod %s is replaced", member)
			newClaimUID, newPodUID = uid, pod
		}, 5*time.Minute, 3*time.Second).Should(Succeed())

		By("listing the database directory of the new volume of " + member + " before any rebuild")
		// The contract confirms the loss by a claim with a new UID that
		// holds no database files. The new Pod's database directory is
		// listed, read-only, before the rebuild is requested: the database
		// has no directory there and no line in databases.txt. The container
		// may not accept a command at first; when it accepts none within two
		// minutes, the listing is not observed and the fault is not
		// confirmed.
		newVolume := "not observed: the database directory of the new Pod could not be listed"
		listing := ""
		const listScript = `d="$CUBRID_DATABASES"; db="$CUBRID_DB"; echo "directory: $d, database: $db"; ` +
			`[ -n "$d" ] && [ -n "$db" ] || { echo "verdict: unknown"; exit 0; }; ls -lA "$d" 2>&1; ` +
			`if [ -e "$d/$db" ] || grep -qs "^$db[[:space:]]" "$d/databases.txt"; ` +
			`then echo "verdict: holds-database"; else echo "verdict: no-database"; fi`
		listDeadline := time.Now().Add(2 * time.Minute)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, err := r.kubectlQuiet(ctx, "exec", member, "-c", "cubrid", "--", "sh", "-c", listScript)
			cancel()
			if err == nil && strings.Contains(out, "verdict: no-database") {
				newVolume, listing = "no database files", out
				break
			}
			if err == nil && strings.Contains(out, "verdict: holds-database") {
				newVolume, listing = "holds database files", out
				break
			}
			if err == nil {
				listing = out
			}
			if time.Now().After(listDeadline) {
				break
			}
			time.Sleep(3 * time.Second)
		}
		listingFile := writeScenarioFile("member-rebuild/new-volume-before-rebuild.txt",
			[]byte(newVolume+"\n"+listing))
		_, _ = fmt.Fprintf(GinkgoWriter, "the new volume of %s before the rebuild: %s\n%s\n", member, newVolume, listing)
		// The loss is confirmed by a claim with a new UID, bound to a new
		// volume, a new Pod and, when it could be listed, a database
		// directory without the database.
		faultConfirmed := newVolume == "no database files"
		result.FaultConfirmed = &faultConfirmed
		Expect(newVolume).NotTo(Equal("holds database files"),
			"the new claim of %s already holds the database before any rebuild", member)

		// No release annotation is left behind either.
		DeferCleanup(func() {
			_, _ = r.kubectl(context.Background(), "annotate", "cubridcluster", r.cluster,
				releaseWriteQuarantineAnnotation+"-")
		})
		// A single role probe that the other slave misses while the emptied
		// member rejoins records it in the write quarantine (#1139); such a
		// quarantine cannot end by itself while the emptied member has no
		// database, and no rebuild is offered while it is in force. The step
		// releases it as an administrator would (docs/observability.md,
		// "Manual release"), only while missedProbeReleaseRefusal finds the
		// conditions the maintainer set on #1139 met in a fresh read. It sets
		// the documented annotation with the current token; the Operator's
		// own release checks still decide. When the conditions stop holding
		// before the Operator accepted the release, the annotation is removed
		// again. The data check at the end of the step still reads every
		// acknowledged row on every member, so a release cannot hide a loss.
		missedSet := ""
		missedProbeReleases := []string{}
		releaseAfterMissedProbe := func(g Gomega, c *databasev1alpha1.CubridCluster) (refusal string) {
			q := c.Status.WriteQuarantine
			current := map[string]memberIdentity{}
			for _, m := range otherQuarantineMembers(q, member) {
				id, err := readIdentity(m)
				g.Expect(err).NotTo(HaveOccurred())
				current[m] = id
			}
			refusal, final := missedProbeReleaseRefusal(c, member, newPodUID, master, identitiesBefore, current,
				time.Now())
			if refusal != "" {
				if missedSet != "" {
					_, err := r.kubectl(context.Background(), "annotate", "cubridcluster", r.cluster,
						releaseWriteQuarantineAnnotation+"-")
					g.Expect(err).NotTo(HaveOccurred())
					missedSet = ""
				}
				if final {
					StopTrying(fmt.Sprintf("a write quarantine records %s besides %s, and the step may not "+
						"release it: %s; released nothing: %s", strings.Join(otherQuarantineMembers(q, member), ", "),
						member, refusal, quarantineSummary(q))).Now()
				}
				return refusal
			}
			g.Expect(q.ReleaseToken).NotTo(BeEmpty(), "status.writeQuarantine.releaseToken")
			// The token is redacted from the test output.
			redactor.Add(q.ReleaseToken)
			if q.ReleaseToken != missedSet {
				_, err := r.kubectl(context.Background(), "annotate", "cubridcluster", r.cluster,
					releaseWriteQuarantineAnnotation+"="+q.ReleaseToken, "--overwrite")
				g.Expect(err).NotTo(HaveOccurred())
				missedSet = q.ReleaseToken
				entry := fmt.Sprintf("set the release annotation with the current token of the %s; the master "+
					"%s is the one active master observed and is not recorded, and every recorded member other "+
					"than %s answered slave in the latest observation from the Pod and container start it had "+
					"before the step; the Operator's own release checks decide",
					quarantineSummary(q), master, member)
				AddReportEntry("write quarantine release requested after a missed role probe", entry)
				_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", entry)
				missedProbeReleases = append(missedProbeReleases,
					fmt.Sprintf("%s revision %d (%s)", q.GenerationID, q.Revision,
						strings.Join(otherQuarantineMembers(q, member), ", ")))
			}
			return ""
		}
		// waitOthers handles, in one read c of a wait, a quarantine that
		// records members besides the emptied one: it releases it when the
		// conditions hold, ends the read while it is in force, and fails the
		// step, with when appended to the message, once others has measured
		// it for longer than otherQuarantineGrace.
		waitOthers := func(g Gomega, c *databasev1alpha1.CubridCluster, others *otherMembersWait, when string) {
			q := c.Status.WriteQuarantine
			if len(otherQuarantineMembers(q, member)) == 0 {
				others.observe(q, time.Now())
				return
			}
			refusal := releaseAfterMissedProbe(g, c)
			if _, failure := others.observe(q, time.Now()); failure != "" {
				if refusal != "" {
					failure += "; the conditions for releasing it were not met: " + refusal + "; released nothing"
				} else {
					failure += "; the release annotation names its current token, and the Operator has not " +
						"accepted it"
				}
				StopTrying(failure + when).Now()
			}
			g.Expect(refusal).To(BeEmpty(), "the release of %s", quarantineSummary(q))
			g.Expect(q).To(BeNil(), "status.writeQuarantine after the release annotation was set: %s",
				quarantineSummary(q))
		}

		releasedGeneration := ""
		if outranksMaster {
			// The member outranks the only active master and gives no role,
			// so the Operator quarantined the read-write path for it
			// (WriteQuarantinedMemberUnverified), and that quarantine never
			// ends by itself because a member with an empty data volume never
			// answers. No rebuild is offered while it is in force. Releasing
			// it is the administrator's documented path (docs/observability.md,
			// "Manual release"), and it is safe here: the member's data claim
			// was replaced by an empty one, so the member holds no database
			// and no acknowledged write can be lost by the release.
			By("releasing the write quarantine " + member + " started, as an administrator who replaced its storage")
			lastSet := ""
			others := &otherMembersWait{member: member, grace: otherQuarantineGrace}
			Eventually(func(g Gomega) {
				c, err := readCluster()
				g.Expect(err).NotTo(HaveOccurred())
				if q := c.Status.WriteQuarantine; q != nil {
					if q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
						StopTrying(fmt.Sprintf("the write quarantine in force has the reason %s, not %s; "+
							"this step releases only the quarantine of a member without storage: %s",
							q.Reason, databasev1alpha1.WriteQuarantinedMemberUnverified, quarantineSummary(q))).Now()
					}
					// A manual release accepts, for every member the
					// quarantine records, that writes only that member
					// acknowledged may be lost. The step accepts that
					// without further conditions only for the member whose
					// storage it replaced, which holds no database. The
					// master and a second slave answered when the member was
					// chosen, so the member is never the last resolved
					// primary. When the other slave missed a role probe while
					// the member rejoined, the quarantine records it too, and
					// the step releases it only under the conditions of
					// waitOthers (#1139).
					// Only a quarantine that records the member is this step's
					// to release and to report as its release.
					if slices.ContainsFunc(q.Members, func(m databasev1alpha1.WriteQuarantineMember) bool {
						return m.Name == member
					}) {
						releasedGeneration = q.GenerationID
					}
					waitOthers(g, c, others, "")
					g.Expect(q.Members).To(HaveLen(1), "the members of %s", quarantineSummary(q))
					// The release covers the Pod the quarantine records; a
					// record made before the new Pod existed is revised, with
					// a new token, once the Operator sees the new Pod.
					g.Expect(q.Members[0].PodUID).To(Equal(newPodUID), "the quarantine records the new Pod of %s", member)
					g.Expect(q.ReleaseToken).NotTo(BeEmpty(), "status.writeQuarantine.releaseToken")
					// The token is new at each revision of the quarantine, so
					// the current one is read and set again on each attempt.
					// It is redacted from the test output.
					redactor.Add(q.ReleaseToken)
					if q.ReleaseToken != lastSet {
						_, err := r.kubectl(context.Background(), "annotate", "cubridcluster", r.cluster,
							releaseWriteQuarantineAnnotation+"="+q.ReleaseToken, "--overwrite")
						g.Expect(err).NotTo(HaveOccurred())
						lastSet = q.ReleaseToken
					}
					releasedGeneration = q.GenerationID
					// A refusal is not the end of the step: "no member is an
					// active master in this observation" is recorded when the
					// master's role probe also timed out in the Operator's
					// pass (#1139), and the Operator decides the annotation,
					// which stays set, again in its next pass. The step keeps
					// reading until the release is accepted or the window ends.
					g.Expect(q).To(BeNil(), "status.writeQuarantine: %s", quarantineSummary(q))
				}
				g.Expect(releasedGeneration).NotTo(BeEmpty(),
					"a %s quarantine naming %s", databasev1alpha1.WriteQuarantinedMemberUnverified, member)
				rel := c.Status.LastWriteQuarantineRelease
				g.Expect(rel).NotTo(BeNil(), "status.lastWriteQuarantineRelease")
				g.Expect(rel.GenerationID).To(Equal(releasedGeneration), "the release is of the quarantine for %s", member)
				g.Expect(rel.Method).To(Equal(databasev1alpha1.WriteQuarantineReleasedManually))
			}, 5*time.Minute, 3*time.Second).Should(Succeed())

			By("waiting for the read-write Brokers to be available again")
			// The other slave can miss a role probe after the release too,
			// which starts a new quarantine that records it alone (#1139).
			// The decision on #1139 does not cover releasing that one, so
			// waitOthers refuses it and the step fails once it outlasts
			// otherQuarantineGrace.
			others = &otherMembersWait{member: member, grace: otherQuarantineGrace}
			Eventually(func(g Gomega) {
				c, err := readCluster()
				g.Expect(err).NotTo(HaveOccurred())
				waitOthers(g, c, others, "; it started after the release")
				g.Expect(c.Status.WriteQuarantine).To(BeNil(), "status.writeQuarantine after the release: %s",
					quarantineSummary(c.Status.WriteQuarantine))
				out, err := r.kubectlQuiet(context.Background(), "get", "deployment", r.cluster+"-broker-rw",
					"-o", "jsonpath={.spec.replicas} {.status.updatedReplicas} {.status.availableReplicas}")
				g.Expect(err).NotTo(HaveOccurred())
				f := strings.Fields(out)
				g.Expect(f).To(HaveLen(3), "replicas, updated, available: %q", out)
				g.Expect(f[0]).NotTo(Equal("0"), "the read-write Brokers are scaled back after the release")
				g.Expect(f[1:]).To(HaveEach(f[0]), "replicas, updated, available: %q", out)
			}, 5*time.Minute, 3*time.Second).Should(Succeed())
		}

		By("writing rows while " + member + " has no database")
		h, err = r.runWorkload(context.Background(), "CLIENT_ID="+clients+"-during OPS=10")
		Expect(err).NotTo(HaveOccurred())
		count(h)
		Expect(h.Counts[workload.Acknowledged]).To(BeNumerically(">", 0), "%v", h.Counts)

		By("waiting for the Operator to offer the rebuild of " + member)
		var offer databasev1alpha1.MemberRebuildOffer
		// No rebuild is offered while a write quarantine is in force. One
		// that records another member besides this one cannot end while
		// this member has no database; waitOthers releases it under the
		// conditions the maintainer set, or reports it once
		// otherQuarantineGrace has passed instead of waiting out this bound
		// (#1139).
		offerOthers := &otherMembersWait{member: member, grace: otherQuarantineGrace}
		Eventually(func(g Gomega) {
			c, err := readCluster()
			g.Expect(err).NotTo(HaveOccurred())
			waitOthers(g, c, offerOthers, "")
			i := slices.IndexFunc(c.Status.MemberRebuildOffers, func(o databasev1alpha1.MemberRebuildOffer) bool {
				return o.Member == member
			})
			g.Expect(i).To(BeNumerically(">=", 0), "an offer for %s in %+v (%s)", member,
				c.Status.MemberRebuildOffers, quarantineSummary(c.Status.WriteQuarantine))
			offer = c.Status.MemberRebuildOffers[i]
			g.Expect(offer.ClaimUID).To(Equal(newClaimUID), "the offer names the new claim")
			g.Expect(offer.Token).NotTo(BeEmpty())
		}, 5*time.Minute, 3*time.Second).Should(Succeed())
		Expect(offer.Source).To(Equal(master), "the source of the offer is the master CUBRID reports")
		offeredAfter := time.Since(lostAt)
		redactor.Add(offer.Token)

		accepted, err := r.eventCount("MemberRebuildAccepted")
		Expect(err).NotTo(HaveOccurred())
		// Each refusal is checked by its Event and by nothing being
		// accepted. A wrong token is ignored; the master is refused before
		// any token is compared.
		for _, bad := range []struct{ value, reason, what string }{
			{member + ":not-the-token-" + clients, "MemberRebuildIgnored", "a wrong token"},
			{master + ":" + offer.Token, "MemberRebuildRefused", "the master"},
		} {
			By("checking that a request naming " + bad.what + " starts no rebuild")
			before, err := r.eventCount(bad.reason)
			Expect(err).NotTo(HaveOccurred())
			annotate(bad.value)
			Eventually(func() (int, error) { return r.eventCount(bad.reason) }, 2*time.Minute, 2*time.Second).
				Should(BeNumerically(">", before), "a %s Event for a request naming %s", bad.reason, bad.what)
			c, err := readCluster()
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Status.MemberRebuild).To(BeNil(), "a request naming %s was accepted", bad.what)
			Expect(r.eventCount("MemberRebuildAccepted")).To(Equal(accepted), "a request naming %s was accepted", bad.what)
		}

		By("requesting the rebuild of " + member + " with its token")
		value := member + ":" + offer.Token
		if offer.FormerPrimary {
			// Only the last resolved primary needs its source named; a
			// slave of the starting state normally is not one.
			value += ":" + offer.Source
		}
		// The contract's data check includes rows written while the rebuild
		// ran: a client keeps writing through the read-write Service from
		// before the request until the member answers as a slave again, and
		// its history joins the run's, so the data check below applies the
		// data rules to its rows on every member.
		By("starting a client that keeps writing through the read-write Service while " + member + " is rebuilt")
		during := clients + "-rebuild"
		r.startClient(during)
		defer r.killClient(during)
		requested := time.Now()
		annotate(value)

		By("waiting for the rebuild to end")
		var done *databasev1alpha1.MemberRebuild
		Eventually(func(g Gomega) {
			c, err := readCluster()
			g.Expect(err).NotTo(HaveOccurred())
			last := c.Status.LastMemberRebuild
			if last != nil && last.Token == offer.Token && last.State != databasev1alpha1.MemberRebuildCompleted {
				msg := fmt.Sprintf("the rebuild of %s ended %s: %s (%s)", member, last.State, last.Message,
					quarantineSummary(c.Status.WriteQuarantine))
				// A quarantine that records the other slave can also start
				// while the restore runs, and fails the rebuild (#1139). The
				// step does not release it and request again: the claim of a
				// rebuild whose restore was started and failed is never
				// offered again (docs/observability.md, "Rebuilding a
				// member"), so only new storage would allow another request.
				if others := otherQuarantineMembers(c.Status.WriteQuarantine, member); len(others) > 0 {
					msg += fmt.Sprintf("; the write quarantine also records %s, and this step does not release "+
						"it to retry: a claim whose restore started and failed is not offered for a rebuild again",
						strings.Join(others, ", "))
				}
				StopTrying(msg).Now()
			}
			g.Expect(last).NotTo(BeNil(), "status.lastMemberRebuild")
			g.Expect(last.Token).To(Equal(offer.Token), "status.lastMemberRebuild is this rebuild")
			done = last
		}, memberRebuildLimit, 3*time.Second).Should(Succeed())
		rebuilt := time.Since(requested)
		Expect(done.Member).To(Equal(member))
		Expect(done.Source).To(Equal(master))
		Expect(done.ClaimUID).To(Equal(newClaimUID))
		Expect(r.eventCount("MemberRebuildRestoreStarted")).To(BeNumerically(">", 0))

		By("checking that " + member + " answers as a slave with its database released")
		Eventually(func(g Gomega) {
			now, slaves, err := r.master(context.Background())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(now).To(Equal(master), "the master did not move")
			g.Expect(slaves).To(ContainElement(member))
			c, err := readCluster()
			g.Expect(err).NotTo(HaveOccurred())
			i := slices.IndexFunc(c.Status.Instances, func(s databasev1alpha1.InstanceStatus) bool {
				return s.Name == member
			})
			g.Expect(i).To(BeNumerically(">=", 0), "status.instances of %s", member)
			// The ownership marker of the restore is cleared only by the
			// release that ends a caught-up rebuild.
			g.Expect(c.Status.Instances[i].UnfinishedDatabase).To(BeNil(), "the marker on %s", member)
			g.Expect(conditionIs(c, "Degraded", "True")).NotTo(Succeed(), "Degraded")
			g.Expect(slices.ContainsFunc(c.Status.MemberDataClaims, func(d databasev1alpha1.MemberDataClaim) bool {
				return d.Name == member && d.ClaimUID == newClaimUID
			})).To(BeTrue(), "the new claim is recorded as holding the database")
			g.Expect(slices.ContainsFunc(c.Status.FormerDataClaims, func(d databasev1alpha1.MemberDataClaim) bool {
				return d.Name == member && d.ClaimUID == oldClaimUID
			})).To(BeTrue(), "the lost claim is kept in status.formerDataClaims")
			annotations, err := r.kubectlQuiet(context.Background(), "get", "cubridcluster", r.cluster,
				"-o", "jsonpath={.metadata.annotations}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(annotations).NotTo(ContainSubstring(rebuildMemberAnnotation), "the consumed request is removed")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())
		// What the fault left is undone once the member answers as a slave
		// again on its new claim and the consumed request is removed.
		cleanedUp := true
		result.CleanupSucceeded = &cleanedUp

		By("stopping the client that wrote while " + member + " was rebuilt")
		events, history := r.stopClient(during)
		count(history)
		// The rebuild ran from its acceptance until its end, as the Operator
		// recorded them. The API server keeps both to the second, so only
		// inserts answered after the second that follows the acceptance and
		// before the recorded end are counted as written while it ran.
		writesWhileRebuilt := map[string]int{}
		if done.EndTime != nil {
			from, to := done.AcceptedTime.Add(time.Second), done.EndTime.Time
			for _, e := range events {
				if e.Op == workload.OpInsert && e.Event != workload.Attempted && e.T.After(from) && e.T.Before(to) {
					writesWhileRebuilt[e.Event]++
				}
			}
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "inserts of client %s answered while the rebuild ran (%s to %v): %v\n",
			during, done.AcceptedTime, done.EndTime, writesWhileRebuilt)

		By("writing rows after the rebuild")
		h, err = r.runWorkload(context.Background(), "CLIENT_ID="+clients+"-after OPS=10")
		Expect(err).NotTo(HaveOccurred())
		count(h)
		Expect(h.Counts[workload.Acknowledged]).To(BeNumerically(">", 0), "%v", h.Counts)

		By("checking every acknowledged row on every member and through both Services")
		report := r.passingDataCheck("member-rebuild", 3*time.Minute)
		// Measured against rebuild_limit, as an upper bound: it includes
		// the rows written after the rebuild and the data check itself.
		allRows := time.Since(requested)
		Expect(report.Members).To(HaveKey(member))
		files := r.evidenceFiles("member-rebuild", report)

		By("checking that the applier of " + member + " has no failure")
		failCount, err := r.failCount(context.Background(), member, master)
		Expect(err).NotTo(HaveOccurred())
		Expect(failCount).To(BeZero(), "fail count of the applier on the rebuilt member %s", member)

		By("checking that the claims of the master and of the other slave were not replaced")
		// A healthy member's data overwritten and the master's volume chosen
		// as the target must never happen. What this step observes of them
		// is that the rebuild named only the requested member and that the
		// master's and the other slave's claims kept their UIDs; a restore
		// into an unchanged claim would not show here, and the data check
		// above is what shows their rows intact.
		for m, uid := range otherClaims() {
			if uid != otherClaimsBefore[m] {
				result.NeverEvents = append(result.NeverEvents, fmt.Sprintf(
					"the claim data-%s of a member that was not rebuilt was replaced (%s, then %s)",
					m, otherClaimsBefore[m], uid))
			}
		}
		Expect(result.NeverEvents).To(BeEmpty())
		if listingFile != "" {
			files = append(files, listingFile)
		}
		release := "not started, " + member + " is ranked below the master"
		if outranksMaster {
			release = "released manually, " + member + " outranks the master (quarantine " + releasedGeneration + ")"
		}
		if len(missedProbeReleases) > 0 {
			release += "; release requested after a missed role probe: " + strings.Join(missedProbeReleases, "; ")
		}
		summary := fmt.Sprintf("rebuilt member: %s\nsource: %s\nwrite quarantine: %s\n"+
			"lost claim: %s (volume %s)\nnew claim: %s\n"+
			"offer published after the loss: %s\nrebuild from the request to Completed: %s\n"+
			"operation: %s\nrows on %s: %d, acknowledged rows missing: %d\n",
			member, master, release, oldClaimUID, oldVolume, newClaimUID, offeredAfter.Round(time.Second),
			rebuilt.Round(time.Second), done.OperationID, member, report.Members[member].Rows, report.AcknowledgedMissing)
		if f := writeScenarioFile("member-rebuild/result.txt", []byte(summary)); f != "" {
			files = append(files, f)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "%sevidence: %v\n", summary, files)

		result = judged(evidence.Scenario{
			ID:             "S10",
			FaultConfirmed: &faultConfirmed, CleanupSucceeded: &cleanedUp,
			Measurements: map[string]any{
				"newVolumeBeforeRebuild":                      newVolume,
				"offerPublishedAfterLoss":                     offeredAfter.Round(time.Millisecond).String(),
				"rebuildRequestedToCompleted":                 rebuilt.Round(time.Millisecond).String(),
				"rebuildRequestedToAllRowsChecked":            allRows.Round(time.Millisecond).String(),
				"writeQuarantineReleasedManually":             outranksMaster,
				"writeQuarantineReleasesAfterMissedProbe":     missedProbeReleases,
				"rebuiltMemberApplierFailCount":               failCount,
				"rowsOnRebuiltMember":                         report.Members[member].Rows,
				"acknowledgedMissing":                         report.AcknowledgedMissing,
				"insertsAcknowledgedWhileRebuildRan":          writesWhileRebuilt[workload.Acknowledged],
				"insertsFailedWhileRebuildRan":                writesWhileRebuilt[workload.Failed],
				"insertsWithUnknownOutcomeWhileRebuildRan":    writesWhileRebuilt[workload.Unknown],
				"requestNamingMasterRefused":                  true,
				"requestWithWrongTokenIgnored":                true,
				"rebuildTargetWasMaster":                      false,
				"otherMembersClaimsReplaced":                  false,
				"masterMoved":                                 false,
				"rebuiltMemberReportedHealthyBeforeDataMatch": "not observed",
			},
			Operations: &operations,
			Evidence:   files,
		}, within(rebuildLimit, allRows))
		// A time-limit pass is not a pass of S10 unless the fault and the
		// rows written while the rebuild ran were observed.
		if result.Result == evidence.Pass {
			switch {
			case !faultConfirmed:
				result.Result, result.Reason = evidence.Blocked, "the fault is not confirmed: "+newVolume
			case writesWhileRebuilt[workload.Acknowledged] == 0:
				result.Result, result.Reason = evidence.Blocked, "no insert was acknowledged while the "+
					"rebuild ran, so the data check includes no row written during it"
			}
		}
	})
}
