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

// dbaPasswordScenario creates a standalone cluster whose spec names a DBA
// password Secret, on the real CUBRID image, and checks that the database
// refuses the empty DBA password and accepts the Secret's, and that the
// password is in no status, Event, StatefulSet or Operator log line (#805).
func dbaPasswordScenario() {
	Context("Standalone cluster with a DBA password from a Secret on the real CUBRID image",
		Label("db", "dba-password"), Ordered, func() {
			const (
				dbaNamespace = "cubrid-dba-password"
				clusterName  = "dba"
				database     = "appdb"
				secretName   = "dba-password"
				// The password only this test knows; it is on no command line
				// of the test process.
				password = "E2e-Dba-Password-805"
				// passwordFile is where the Operator mounts the key in the DB
				// container (internal/controller/dbapassword.go).
				passwordFile = "/etc/cubrid-dba-password/password"
				// refused is what csql prints for a wrong password
				// (docs/poc/RESULTS.md, POC-61).
				refused = "Incorrect or missing password"
			)
			pod := clusterName + "-0"
			kubectl := func(args ...string) (string, error) {
				return utils.Run(exec.Command("kubectl", append([]string{"-n", dbaNamespace}, args...)...))
			}
			apply := func(manifest string) {
				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(manifest)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}
			// csql runs in the DB container as the cubrid user. login is the
			// shell text that gives csql its password option, if any.
			csql := func(login string) (string, error) {
				return kubectl("exec", pod, "--", "bash", "-c",
					`PATH="${CUBRID}/bin:${PATH}" csql -u dba `+login+` "$0" -c 'SELECT 1' </dev/null`,
					database+"@localhost")
			}

			BeforeAll(func() {
				if runtime.GOARCH != "amd64" {
					Skip("needs linux/amd64: the official CUBRID image has no " + runtime.GOARCH + " build. Not run.")
				}
				ensureInstanceManagerImage()

				By("creating a namespace that enforces the restricted security policy")
				_, err := utils.Run(exec.Command("kubectl", "create", "ns", dbaNamespace))
				Expect(err).NotTo(HaveOccurred())
				_, err = utils.Run(exec.Command("kubectl", "label", "--overwrite", "ns", dbaNamespace,
					"pod-security.kubernetes.io/enforce=restricted"))
				Expect(err).NotTo(HaveOccurred())

				By("creating the cluster first, so that it waits for its Secret")
				repository, tag, found := strings.Cut(instanceManagerImage, ":")
				Expect(found).To(BeTrue(), "instanceManagerImage needs a tag")
				apply(fmt.Sprintf(`apiVersion: database.cubrid.io/v1alpha1
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
    promotableMembers: 1
    readReplicas: 0
  highAvailability:
    enabled: false
  storage:
    data:
      size: 2Gi
  dbaPasswordSecretRef:
    name: %s
    key: password
`, clusterName, dbaNamespace, repository, tag, database, secretName))
			})

			AfterAll(func() {
				if CurrentSpecReport().Failed() {
					for _, args := range [][]string{
						{"get", "cubridcluster", clusterName, "-o", "yaml"},
						{"describe", "pod", pod},
						{"logs", pod, "--tail=200"},
						{"get", "events", "--sort-by=.lastTimestamp"},
					} {
						out, _ := kubectl(args...)
						_, _ = fmt.Fprintf(GinkgoWriter, "%v:\n%s\n", args, out)
					}
				}
				_, _ = kubectl("delete", "cubridcluster", clusterName, "--ignore-not-found", "--timeout=5m")
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", dbaNamespace, "--ignore-not-found", "--timeout=5m"))
			})

			It("creates no database until the Secret exists, then reports it resolved", func() {
				Eventually(func(g Gomega) {
					out, err := kubectl("get", "cubridcluster", clusterName, "-o",
						`jsonpath={.status.conditions[?(@.type=="DBAPasswordSecretResolved")].reason}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("DBAPasswordSecretNotFound"))
				}, time.Minute, 2*time.Second).Should(Succeed())
				out, err := kubectl("get", "statefulset", "--ignore-not-found", "-o", "name")
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(out)).To(BeEmpty(), "a database would be created without its password")

				By("creating the Secret; the operator's watch takes the cluster further")
				apply(fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
stringData:
  password: %s
`, secretName, dbaNamespace, password))
				Eventually(func(g Gomega) {
					out, err := kubectl("get", "cubridcluster", clusterName, "-o",
						`jsonpath={.status.conditions[?(@.type=="DBAPasswordSecretResolved")].status}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("True"))
				}, time.Minute, 2*time.Second).Should(Succeed())
			})

			It("refuses the empty DBA password and accepts the Secret's", func() {
				By("waiting for the first start: createdb, the password, the server, then the manager")
				Eventually(func(g Gomega) {
					out, err := kubectl("get", "pod", pod,
						"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("True"))
				}, 15*time.Minute, 2*time.Second).Should(Succeed())

				By("checking that only the cubrid user's group can read the password file")
				out, err := kubectl("exec", pod, "--", "stat", "-L", "-c", "%a %g", passwordFile)
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(out)).To(Equal("440 1000"))

				By("logging in as dba with the empty password")
				out, _ = csql("")
				Expect(out).To(ContainSubstring(refused), "the database accepted the empty DBA password")

				By("logging in as dba with the password read from the mounted file inside the Pod")
				out, err = csql(`-p "$(cat ` + passwordFile + `)"`)
				Expect(err).NotTo(HaveOccurred(), out)
				Expect(out).NotTo(ContainSubstring(refused))
				Expect(out).NotTo(ContainSubstring("ERROR"))
			})

			It("writes the password into no status, Event, StatefulSet or Operator log line", func() {
				for what, args := range map[string][]string{
					"the cluster":     {"-n", dbaNamespace, "get", "cubridcluster", clusterName, "-o", "json"},
					"the StatefulSet": {"-n", dbaNamespace, "get", "statefulset", clusterName, "-o", "json"},
					"the Events":      {"-n", dbaNamespace, "get", "events", "-o", "json"},
					"the Operator log": {"-n", namespace, "logs", "-l", operatorSelector, "--tail=-1",
						"--all-containers"},
				} {
					out, err := utils.RunQuiet(exec.Command("kubectl", args...))
					Expect(err).NotTo(HaveOccurred(), what)
					Expect(out).NotTo(ContainSubstring(password), "%s holds the DBA password", what)
				}
			})
		})
}
