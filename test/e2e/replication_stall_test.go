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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// replicationStallStep registers a check of the ReplicationHealthy condition
// on a real engine (#229). It puts a slave into the state of POC-21 in
// docs/poc/RESULTS.md: heartbeat is stopped inside its running container, the
// master's Pod is deleted, another member becomes master, and heartbeat is
// started again in the same container. The slave then looks healthy and
// applies nothing. Deleting its Pod repairs it.
//
// It is not a scenario of the contract and reports nothing into the summary.
func replicationStallStep(r *haRun) {
	It("reports a slave that looks healthy and applies nothing, and no longer after its Pod is replaced", func() {
		inPod := func(pod, script string) (string, error) {
			return r.kubectl("exec", pod, "--", "bash", "-c", `export PATH="${CUBRID}/bin:${PATH}"; `+script)
		}
		condition := func() (string, string, string) {
			field := func(name string) string {
				out, _ := r.kubectl("get", "cubridcluster", r.cluster, "-o",
					`jsonpath={.status.conditions[?(@.type=="ReplicationHealthy")].`+name+`}`)
				return out
			}
			return field("status"), field("reason"), field("message")
		}

		By("recording the starting state")
		master, slaves, err := r.master()
		Expect(err).NotTo(HaveOccurred())
		victim := slaves[0]
		Eventually(func(g Gomega) {
			status, _, message := condition()
			g.Expect(status).To(Equal("True"), "ReplicationHealthy before the test: %s", message)
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("stopping heartbeat inside the running container of " + victim)
		_, err = inPod(victim, `cubrid heartbeat stop >/tmp/hb-stop.log 2>&1`)
		Expect(err).NotTo(HaveOccurred())

		By("deleting the master Pod " + master + " and waiting for another member to be master")
		_, err = r.kubectl("delete", "pod", master, "--wait=false")
		Expect(err).NotTo(HaveOccurred())
		others := []string{}
		for _, m := range r.members {
			if m != victim {
				others = append(others, m)
			}
		}
		var newMaster string
		Eventually(func(g Gomega) {
			newMaster = r.activeMasterAmong(others)
			g.Expect(newMaster).NotTo(BeEmpty())
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("writing on the new master while the slave is stopped")
		Eventually(func() error {
			_, err := r.runWorkload("CLIENT_ID=st1 OPS=20 ROLLBACK_EVERY=0")
			return err
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		// No pipe after a command that starts daemons: they would keep it open.
		By("starting heartbeat again in the same container")
		_, err = inPod(victim, `cubrid heartbeat start >/tmp/hb-start.log 2>&1`)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			_, _, err := r.master()
			g.Expect(err).NotTo(HaveOccurred())
		}, 3*time.Minute, 3*time.Second).Should(Succeed(), "one master and two slaves, the stopped member among them")

		By("writing more, so that log pages wait on the slave")
		h, err := r.runWorkload("CLIENT_ID=st2 OPS=40 ROLLBACK_EVERY=0")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.Counts[workload.Acknowledged]).To(Equal(40))

		By("waiting for the operator to report the slave as stalled")
		var message string
		Eventually(func(g Gomega) {
			status, reason, m := condition()
			message = m
			g.Expect(status).To(Equal("False"), "ReplicationHealthy: %s %s", reason, m)
			g.Expect(reason).To(Equal("ReplicationStalled"))
			g.Expect(m).To(ContainSubstring(victim))
		}, 4*time.Minute, 5*time.Second).Should(Succeed())
		for _, m := range others {
			if m != newMaster {
				Expect(message).NotTo(ContainSubstring(m), "the healthy slave is named as stalled")
			}
		}

		By("checking that the slave is behind, and that the other conditions do not show it")
		behind, err := r.memberRows(victim)
		Expect(err).NotTo(HaveOccurred())
		ahead, err := r.memberRows(newMaster)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(behind)).To(BeNumerically("<", len(ahead)), "rows on the stalled slave and on the master")
		haReady, err := r.kubectl("get", "cubridcluster", r.cluster, "-o",
			`jsonpath={.status.conditions[?(@.type=="HAReady")].status}`)
		Expect(err).NotTo(HaveOccurred())
		record := fmt.Sprintf("stalled member: %s\nnew master: %s\nrows on the stalled member: %d\nrows on the master: %d\n"+
			"ReplicationHealthy: False ReplicationStalled: %s\nHAReady at the same time: %s\n",
			victim, newMaster, len(behind), len(ahead), message, haReady)

		By("deleting the Pod of the stalled slave")
		_, err = r.kubectl("delete", "pod", victim, "--wait=false")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			status, reason, m := condition()
			g.Expect(status).To(Equal("True"), "ReplicationHealthy after the Pod was replaced: %s %s", reason, m)
			report, err := r.dataCheck()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
		after, err := r.memberRows(victim)
		Expect(err).NotTo(HaveOccurred())
		record += fmt.Sprintf("rows on the member after its Pod was replaced: %d\nReplicationHealthy: True\n", len(after))
		writeScenarioFile("replication-stall/result.txt", []byte(strings.TrimSpace(record)+"\n"))
	})
}
