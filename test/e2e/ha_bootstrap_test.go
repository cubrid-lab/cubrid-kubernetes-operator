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
	"os/exec"
	"runtime"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// haFirstDatabaseScenario registers the first step of the HA bootstrap on a
// real engine: of three members, exactly one creates the database and becomes
// master; the other two create nothing and wait to be seeded (ADR-0010).
//
// It is not S01: the peers are not seeded, so there is no replication, the
// master accepts no write yet, and the cluster is not Ready. Skipped, which is "not run", on anything but amd64.
func haFirstDatabaseScenario() {
	Context("HA bootstrap: the first database, with the real CUBRID image", Label("db", "ha-bootstrap"), Ordered, func() {
		const (
			haNamespace = "cubrid-ha-bootstrap"
			clusterName = "hab"
			database    = "appdb"
		)
		first := clusterName + "-0"
		peers := []string{clusterName + "-1", clusterName + "-2"}
		ran := false

		kubectl := func(args ...string) (string, error) {
			return utils.Run(exec.Command("kubectl", append([]string{"-n", haNamespace}, args...)...))
		}
		clusterField := func(jsonPath string) (string, error) {
			return kubectl("get", "cubridcluster", clusterName, "-o", "jsonpath="+jsonPath)
		}

		BeforeAll(func() {
			if runtime.GOARCH != "amd64" {
				Skip("needs linux/amd64: the official CUBRID image has no " + runtime.GOARCH + " build. Not run.")
			}
			ran = true
			ensureInstanceManagerImage()

			By("creating a namespace that enforces the restricted security policy")
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", haNamespace))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "label", "--overwrite", "ns", haNamespace,
				"pod-security.kubernetes.io/enforce=restricted"))
			Expect(err).NotTo(HaveOccurred())

			By("creating a three-member HA CubridCluster")
			repository, tag, found := strings.Cut(instanceManagerImage, ":")
			Expect(found).To(BeTrue(), "instanceManagerImage needs a tag")
			manifest := fmt.Sprintf(`apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster
metadata:
  name: %s
  namespace: %s
spec:
  version: "11.4"
  image:
    repository: %s
    tag: %s
  databases:
    - name: %s
  topology:
    promotableMembers: 3
    readReplicas: 0
  highAvailability:
    enabled: true
  storage:
    data:
      size: 2Gi
`, clusterName, haNamespace, repository, tag, database)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the CubridCluster")
		})

		AfterAll(func() {
			if !ran {
				return
			}
			for _, args := range [][]string{
				{"get", "cubridcluster", clusterName, "-o", "yaml"},
				{"get", "pods", "-o", "wide"},
				{"logs", first, "--tail=100"},
				{"logs", peers[0], "--tail=50"},
				{"get", "events", "--sort-by=.lastTimestamp"},
			} {
				out, _ := kubectl(args...)
				_, _ = fmt.Fprintf(GinkgoWriter, "%v:\n%s\n", args, out)
			}
			_, _ = kubectl("delete", "cubridcluster", clusterName, "--ignore-not-found", "--timeout=5m")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", haNamespace, "--ignore-not-found", "--timeout=5m"))
		})

		It("creates the database on one member, which becomes master", func() {
			By("waiting for the operator to record the database")
			Eventually(func(g Gomega) {
				created, err := clusterField("{.status.databases[0].primaryCreated}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(created).To(Equal("true"))
			}, 15*time.Minute, 3*time.Second).Should(Succeed())

			By("asking CUBRID on the first member for its role")
			Eventually(func(g Gomega) {
				out, err := kubectl("exec", first, "--", "bash", "-c",
					`PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("current " + first + ", state master"))
				g.Expect(out).To(ContainSubstring("Server " + database))
			}, 5*time.Minute, 3*time.Second).Should(Succeed())

			// The server of a first member whose peers were never seeded stays
			// "to-be-active" and accepts no write until a peer joins
			// (docs/poc/RESULTS.md, POC-13), so only a read is checked here.
			By("reading from the database on the master")
			Eventually(func(g Gomega) {
				out, err := runSQL(csqlInPod(haNamespace, first, database), "SELECT 1 FROM db_root;")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("1 row selected"))
			}, 2*time.Minute, 3*time.Second).Should(Succeed())

			By("checking that the host of the database is the member list")
			out, err := kubectl("exec", first, "--", "cat", "/var/lib/cubrid/databases/databases.txt")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring(strings.Join(append([]string{first}, peers...), ":")))
		})

		It("creates no database on the other members and keeps them running", func() {
			for _, peer := range peers {
				phase, err := kubectl("get", "pod", peer, "-o", "jsonpath={.status.phase}")
				Expect(err).NotTo(HaveOccurred())
				Expect(phase).To(Equal("Running"), peer)
				restarts, err := kubectl("get", "pod", peer, "-o", "jsonpath={.status.containerStatuses[0].restartCount}")
				Expect(err).NotTo(HaveOccurred())
				Expect(restarts).To(Equal("0"), "%s must wait, not crash-loop", peer)
				_, err = kubectl("exec", peer, "--", "test", "!", "-e", "/var/lib/cubrid/databases/"+database)
				Expect(err).NotTo(HaveOccurred(), "%s has a database directory", peer)
				_, err = kubectl("exec", peer, "--", "test", "!", "-e", "/var/lib/cubrid/databases/databases.txt")
				Expect(err).NotTo(HaveOccurred(), "%s registered a database", peer)
			}
		})

		It("does not report the cluster Ready while the peers are not seeded", func() {
			ready, err := clusterField(`{.status.conditions[?(@.type=="Ready")].status}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(Equal("False"))
			reason, err := clusterField(`{.status.conditions[?(@.type=="BootstrapReady")].reason}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal("PeersNotSeeded"))
		})
	})
}
