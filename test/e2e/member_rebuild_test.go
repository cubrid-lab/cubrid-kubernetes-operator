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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// rebuildMemberAnnotation is the annotation with which an administrator
// requests the rebuild of a member (docs/observability.md, "Rebuilding a
// member").
const rebuildMemberAnnotation = "database.cubrid.io/rebuild-member"

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
// while the member was gone and after it was rebuilt.
//
// The rebuilt member's Instance Manager asks the source's Instance Manager
// whether it is the active master before it restores and again before it
// starts the member, and refuses the restore (RebuildSourceUnconfirmed)
// when it gets no answer. A Completed rebuild therefore also shows that the
// cluster's NetworkPolicy admits one member's Instance Manager to another's
// (#1045).
//
// The member's Pod and claim are replaced, and the step leaves the cluster
// in the common starting state, which the next step verifies. It is not a
// scenario of the contract and records nothing in the summary. It runs in
// every full Kind run; E2E_LABEL_FILTER='ha-setup || member-rebuild' runs it
// alone on a formed cluster.
func memberRebuildStep(r *haRun) {
	const text = "rebuilds a slave that lost its storage on request, and the rebuilt member holds every acknowledged row"
	It(text, Label("member-rebuild"), func() {
		r.enter(nil)
		r.mu.Lock()
		r.markers++
		clients := fmt.Sprintf("rb%d", r.markers)
		r.mu.Unlock()
		master, slaves, err := r.master(context.Background())
		Expect(err).NotTo(HaveOccurred())
		// Never the master: the Operator refuses to rebuild the current
		// primary, and losing the master's storage is a failover, not this
		// step.
		member := slaves[0]
		claim := "data-" + member

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

		By("deleting the data claim and the Pod of " + member + ", as an administrator replacing lost storage")
		// The claim is deleted first and stays until its Pod is gone; the
		// StatefulSet then creates an empty claim and a new Pod. The
		// manager has no right to delete a claim; the test does it.
		_, err = r.kubectl(context.Background(), "delete", "pvc", claim, "--wait=false")
		Expect(err).NotTo(HaveOccurred())
		_, err = r.kubectl(context.Background(), "delete", "pod", member, "--timeout=5m")
		Expect(err).NotTo(HaveOccurred())
		lostAt := time.Now()
		var newClaimUID string
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
			newClaimUID = uid
		}, 5*time.Minute, 3*time.Second).Should(Succeed())

		By("writing rows while " + member + " has no database")
		h, err = r.runWorkload(context.Background(), "CLIENT_ID="+clients+"-during OPS=10")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.Counts[workload.Acknowledged]).To(BeNumerically(">", 0), "%v", h.Counts)

		By("waiting for the Operator to offer the rebuild of " + member)
		var offer databasev1alpha1.MemberRebuildOffer
		Eventually(func(g Gomega) {
			c, err := readCluster()
			g.Expect(err).NotTo(HaveOccurred())
			i := slices.IndexFunc(c.Status.MemberRebuildOffers, func(o databasev1alpha1.MemberRebuildOffer) bool {
				return o.Member == member
			})
			g.Expect(i).To(BeNumerically(">=", 0), "an offer for %s in %+v", member, c.Status.MemberRebuildOffers)
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
		requested := time.Now()
		annotate(value)

		By("waiting for the rebuild to end")
		var done *databasev1alpha1.MemberRebuild
		Eventually(func(g Gomega) {
			c, err := readCluster()
			g.Expect(err).NotTo(HaveOccurred())
			last := c.Status.LastMemberRebuild
			if last != nil && last.Token == offer.Token && last.State != databasev1alpha1.MemberRebuildCompleted {
				StopTrying(fmt.Sprintf("the rebuild of %s ended %s: %s", member, last.State, last.Message)).Now()
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

		By("writing rows after the rebuild")
		h, err = r.runWorkload(context.Background(), "CLIENT_ID="+clients+"-after OPS=10")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.Counts[workload.Acknowledged]).To(BeNumerically(">", 0), "%v", h.Counts)

		By("checking every acknowledged row on every member and through both Services")
		report := r.passingDataCheck("member-rebuild", 3*time.Minute)
		Expect(report.Members).To(HaveKey(member))
		files := r.evidenceFiles("member-rebuild", report)
		summary := fmt.Sprintf("rebuilt member: %s\nsource: %s\nlost claim: %s (volume %s)\nnew claim: %s\n"+
			"offer published after the loss: %s\nrebuild from the request to Completed: %s\n"+
			"operation: %s\nrows on %s: %d, acknowledged rows missing: %d\n",
			member, master, oldClaimUID, oldVolume, newClaimUID, offeredAfter.Round(time.Second),
			rebuilt.Round(time.Second), done.OperationID, member, report.Members[member].Rows, report.AcknowledgedMissing)
		if f := writeScenarioFile("member-rebuild/result.txt", []byte(summary)); f != "" {
			files = append(files, f)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "%sevidence: %v\n", summary, files)
	})
}
