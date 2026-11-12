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

// dbaPasswordBrokerClientManifest is a client outside the cluster's
// namespace that logs in as dba through the two Broker Services with
// pycubrid, the CUBRID driver for Python. Through each Service a login with
// the empty password has to be refused for its password; a login with the
// password from the client's Secret has to be accepted and read the marker
// row. With WRITE_MARKER=1 the client first writes that row through -rw. It
// prints DBA-PASSWORD-BROKER-OK when all of this holds. The password reaches
// the client only from its Secret and is never printed. The arguments are the
// client namespace, the Pod name, the cluster, the cluster's namespace, the
// database, WRITE_MARKER and the marker.
const dbaPasswordBrokerClientManifest = `apiVersion: v1
kind: Pod
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  restartPolicy: Never
  containers:
    - name: client
      image: docker.io/library/python:3.12-slim
      env:
        - {name: RW_HOST, value: "%[3]s-rw.%[4]s.svc"}
        - {name: RO_HOST, value: "%[3]s-ro.%[4]s.svc"}
        - {name: DATABASE, value: "%[5]s"}
        - {name: WRITE_MARKER, value: "%[6]s"}
        - {name: MARKER, value: "%[7]s"}
        - name: DBA_PASSWORD
          valueFrom:
            secretKeyRef: {name: dba-password, key: password}
        - {name: PIP_DISABLE_PIP_VERSION_CHECK, value: "1"}
      command: ["sh", "-c"]
      args:
        - |
          set -e
          pip install --quiet --no-cache-dir pycubrid==1.9.0
          python - <<'PY'
          import os, sys, time
          import pycubrid

          db = os.environ["DATABASE"]
          password = os.environ["DBA_PASSWORD"]
          marker = os.environ["MARKER"]
          services = {"-rw": (os.environ["RW_HOST"], 33000), "-ro": (os.environ["RO_HOST"], 33001)}

          def login(name, pw):
              host, port = services[name]
              conn = pycubrid.connect(host=host, port=port, database=db, user="dba", password=pw)
              cur = conn.cursor()
              cur.execute("SELECT 1 FROM db_root")
              cur.fetchall()
              return conn

          # The driver's report of a refused password was not observed before
          # (docs/poc/RESULTS.md, POC-61 tested no Broker): CUBRID's code for
          # it is -171, and its message names the password. Any other failure
          # says nothing about the password and is tried again, as a Broker
          # may not accept connections yet.
          for name in services:
              for attempt in range(60):
                  try:
                      login(name, "").close()
                  except Exception as exc:
                      if getattr(exc, "errno", None) == -171 or "password" in str(exc).lower():
                          print("empty password refused through", name, ":", repr(exc))
                          break
                      print("empty password through", name, ", attempt", attempt, ":", repr(exc))
                      time.sleep(2)
                  else:
                      sys.exit("the empty DBA password was accepted through " + name)
              else:
                  sys.exit("a login with the empty DBA password was never refused for its password through " + name)

          if os.environ["WRITE_MARKER"] == "1":
              rw = login("-rw", password)
              cur = rw.cursor()
              cur.execute("CREATE TABLE dba_marker (id INT PRIMARY KEY, marker VARCHAR(64))")
              cur.execute("INSERT INTO dba_marker VALUES (1, '" + marker + "')")
              rw.commit()
              rw.close()
              print("marker written through -rw with the Secret's password")

          for name in services:
              rows = None
              for attempt in range(30):
                  try:
                      conn = login(name, password)
                      cur = conn.cursor()
                      cur.execute("SELECT marker FROM dba_marker WHERE id = 1")
                      rows = cur.fetchall()
                      conn.close()
                      break
                  except Exception as exc:
                      print("login with the Secret's password through", name, ", attempt", attempt, ":", repr(exc))
                      time.sleep(2)
              if not rows or rows[0][0] != marker:
                  sys.exit("the marker row was not read through " + name + " with the Secret's password: " + repr(rows))
              print("marker read through", name, "with the Secret's password")
          print("DBA-PASSWORD-BROKER-OK")
          PY
`

// dbaPasswordScenario creates a standalone cluster whose spec names a DBA
// password Secret, on the real CUBRID image, and checks that the database
// refuses the empty DBA password and accepts the Secret's, in the DB
// container and through the read-write and read-only Broker Services, and
// that the password is in no status, Event, StatefulSet or Operator log line
// (#805). It then backs the database up to a mock object store and restores
// the backup into two new standalone clusters: one whose Secret holds the
// source's password, which becomes Ready with the source's row and the same
// login behavior, and one whose Secret holds another password, whose restore
// has to fail because the restored database keeps its source's password
// (docs/poc/RESULTS.md, POC-61, Q4; #819).
//
// HA clusters are not covered: the API admits dbaPasswordSecretRef only on a
// standalone cluster.
func dbaPasswordScenario() {
	Context("Standalone cluster with a DBA password from a Secret on the real CUBRID image",
		Label("db", "dba-password"), Ordered, func() {
			const (
				dbaNamespace = "cubrid-dba-password"
				// clientNamespace holds the mock object store and the Broker
				// clients: outside the clusters' namespace.
				clientNamespace = "cubrid-dba-password-client"
				clusterName     = "dba"
				// restoredName is restored from the backup with the source's
				// password, otherName with another one.
				restoredName = "dba-restored"
				otherName    = "dba-other"
				database     = "appdb"
				secretName   = "dba-password"
				otherSecret  = "dba-password-other"
				backupName   = "dba-full"
				backupBucket = "backups"
				// The password only this test knows; it is on no command line
				// of the test process.
				password = "E2e-Dba-Password-805"
				// otherPassword is not the source database's password.
				otherPassword = "E2e-Other-Password-805"
				// marker is the row written through -rw on the source and
				// read on the restored cluster.
				marker = "written-before-the-backup"
				// passwordFile is where the Operator mounts the key in the DB
				// container (internal/controller/dbapassword.go).
				passwordFile = "/etc/cubrid-dba-password/password"
				// refused is what csql prints for a wrong password
				// (docs/poc/RESULTS.md, POC-61).
				refused = "Incorrect or missing password"
				// notRequested is the Instance Manager's reason for a restored
				// database whose password is not the requested one
				// (internal/instancemanager/dbapassword.go).
				notRequested = "DBA password is not the requested one"
			)
			pod := clusterName + "-0"
			manifestURI := ""
			kubectl := func(args ...string) (string, error) {
				return utils.Run(exec.Command("kubectl", append([]string{"-n", dbaNamespace}, args...)...))
			}
			apply := func(manifest string) {
				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(manifest)
				_, err := utils.Run(cmd)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
			}
			clusterField := func(name, jsonPath string) (string, error) {
				return kubectl("get", "cubridcluster", name, "-o", "jsonpath="+jsonPath)
			}
			// csqlOn runs csql in the DB container of member as the cubrid
			// user. login is the shell text that gives csql its password
			// option, if any. A test password on csql's command line inside
			// the test Pod is acceptable: it is synthetic.
			csqlOn := func(member, login, statement string) (string, error) {
				return kubectl("exec", member, "--", "bash", "-c",
					`PATH="${CUBRID}/bin:${PATH}" csql -u dba `+login+` "$0" -c "$1" </dev/null`,
					database+"@localhost", statement)
			}
			csql := func(login string) (string, error) {
				return csqlOn(pod, login, "SELECT 1")
			}
			// clusterManifest is a standalone cluster with the password Secret
			// secret, the mock object store, the client namespace as the
			// Brokers' clients and, when recoveryURI is set, a recovery from
			// that backup.
			clusterManifest := func(name, secret, recoveryURI string) string {
				repository, tag, found := strings.Cut(instanceManagerImage, ":")
				ExpectWithOffset(1, found).To(BeTrue(), "instanceManagerImage needs a tag")
				recovery := ""
				if recoveryURI != "" {
					recovery = fmt.Sprintf(`  bootstrap:
    recovery:
      manifestUri: %s
`, recoveryURI)
				}
				return fmt.Sprintf(`apiVersion: database.cubrid.io/v1alpha1
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
  objectStorage:
    endpoint: s3mock.%s.svc:9090
    insecure: true
    credentialsSecretRef:
      name: object-storage
  networkPolicy:
    clients:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: %s
%s`, name, dbaNamespace, repository, tag, database, secret, clientNamespace, clientNamespace, recovery)
			}
			passwordSecret := func(namespace, name, value string) string {
				return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
stringData:
  password: %s
`, name, namespace, value)
			}
			// brokerLogins waits for a Broker of each access mode of cluster
			// and runs the client in clientPod against its two Services.
			brokerLogins := func(cluster, clientPod string, writeMarker bool) {
				for _, mode := range []string{"rw", "ro"} {
					Eventually(func(g Gomega) {
						_, err := kubectl("rollout", "status", "deployment/"+cluster+"-broker-"+mode, "--timeout=1m")
						g.Expect(err).NotTo(HaveOccurred())
					}, 5*time.Minute, 5*time.Second).Should(Succeed(), "the %s Brokers of %s did not become available",
						mode, cluster)
				}
				write := "0"
				if writeMarker {
					write = "1"
				}
				apply(fmt.Sprintf(dbaPasswordBrokerClientManifest, clientNamespace, clientPod, cluster, dbaNamespace,
					database, write, marker))
				Eventually(func(g Gomega) {
					phase, err := utils.Run(exec.Command("kubectl", "-n", clientNamespace, "get", "pod", clientPod,
						"-o", "jsonpath={.status.phase}"))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Or(Equal("Succeeded"), Equal("Failed")))
				}, 10*time.Minute, 3*time.Second).Should(Succeed())
				logs, err := utils.Run(exec.Command("kubectl", "-n", clientNamespace, "logs", clientPod))
				Expect(err).NotTo(HaveOccurred())
				_, _ = fmt.Fprintf(GinkgoWriter, "broker client output:\n%s\n", logs)
				Expect(logs).To(ContainSubstring("DBA-PASSWORD-BROKER-OK"), "client output:\n%s", logs)
			}
			// noLeak checks that value is in none of the places.
			noLeak := func(value string, places map[string][]string) {
				for what, args := range places {
					out, err := utils.RunQuiet(exec.Command("kubectl", args...))
					ExpectWithOffset(1, err).NotTo(HaveOccurred(), what)
					ExpectWithOffset(1, out).NotTo(ContainSubstring(value), "%s holds a DBA password", what)
				}
			}
			operatorLog := []string{"-n", namespace, "logs", "-l", operatorSelector, "--tail=-1", "--all-containers"}

			BeforeAll(func() {
				if runtime.GOARCH != "amd64" {
					Skip("needs linux/amd64: the official CUBRID image has no " + runtime.GOARCH + " build. Not run.")
				}
				ensureInstanceManagerImage()

				By("starting a mock object store for the backup, in the client namespace")
				_, err := utils.Run(exec.Command("kubectl", "create", "ns", clientNamespace))
				Expect(err).NotTo(HaveOccurred())
				apply(fmt.Sprintf(objectStoreManifest, clientNamespace, backupBucket))
				_, err = utils.Run(exec.Command("kubectl", "-n", clientNamespace, "rollout", "status",
					"deployment/s3mock", "--timeout=5m"))
				Expect(err).NotTo(HaveOccurred(), "the mock object store did not start")
				// The Broker clients read the source's password from here.
				apply(passwordSecret(clientNamespace, secretName, password))

				By("creating a namespace that enforces the restricted security policy")
				_, err = utils.Run(exec.Command("kubectl", "create", "ns", dbaNamespace))
				Expect(err).NotTo(HaveOccurred())
				_, err = utils.Run(exec.Command("kubectl", "label", "--overwrite", "ns", dbaNamespace,
					"pod-security.kubernetes.io/enforce=restricted"))
				Expect(err).NotTo(HaveOccurred())
				// The mock store checks no credentials.
				_, err = kubectl("create", "secret", "generic", "object-storage",
					"--from-literal=accessKey=e2e", "--from-literal=secretKey=e2e")
				Expect(err).NotTo(HaveOccurred())

				By("creating the cluster first, so that it waits for its Secret")
				apply(clusterManifest(clusterName, secretName, ""))
			})

			AfterAll(func() {
				if CurrentSpecReport().Failed() {
					for _, args := range [][]string{
						{"get", "cubridcluster", "-o", "yaml"},
						{"get", "cubridbackup", "-o", "yaml"},
						{"get", "pods", "-o", "wide"},
						{"logs", pod, "--tail=200"},
						{"logs", restoredName + "-0", "--tail=200"},
						{"logs", otherName + "-0", "--tail=200"},
						{"get", "events", "--sort-by=.lastTimestamp"},
					} {
						out, _ := kubectl(append(args, "--request-timeout=10s")...)
						_, _ = fmt.Fprintf(GinkgoWriter, "%v:\n%s\n", args, out)
					}
				}
				_, _ = kubectl("delete", "cubridbackup", "--all", "--ignore-not-found", "--timeout=5m")
				_, _ = kubectl("delete", "cubridcluster", "--all", "--ignore-not-found", "--timeout=5m")
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", dbaNamespace, clientNamespace,
					"--ignore-not-found", "--timeout=5m"))
			})

			It("creates no database until the Secret exists, then reports it resolved", func() {
				Eventually(func(g Gomega) {
					out, err := clusterField(clusterName,
						`{.status.conditions[?(@.type=="DBAPasswordSecretResolved")].reason}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("DBAPasswordSecretNotFound"))
				}, time.Minute, 2*time.Second).Should(Succeed())
				out, err := kubectl("get", "statefulset", "--ignore-not-found", "-o", "name")
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(out)).To(BeEmpty(), "a database would be created without its password")

				By("creating the Secret; the operator's watch takes the cluster further")
				apply(passwordSecret(dbaNamespace, secretName, password))
				Eventually(func(g Gomega) {
					out, err := clusterField(clusterName,
						`{.status.conditions[?(@.type=="DBAPasswordSecretResolved")].status}`)
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

			It("refuses the empty DBA password and accepts the Secret's through the read-write and the read-only Service",
				func() {
					By("logging in through each Service from another namespace, and writing the marker row through -rw")
					brokerLogins(clusterName, "broker-client-source", true)
				})

			It("writes the password into no status, Event, StatefulSet or Operator log line", func() {
				noLeak(password, map[string][]string{
					"the cluster":      {"-n", dbaNamespace, "get", "cubridcluster", clusterName, "-o", "json"},
					"the StatefulSet":  {"-n", dbaNamespace, "get", "statefulset", clusterName, "-o", "json"},
					"the Events":       {"-n", dbaNamespace, "get", "events", "-o", "json"},
					"the Operator log": operatorLog,
				})
			})

			It("backs the database up to object storage", func() {
				apply(fmt.Sprintf(`apiVersion: database.cubrid.io/v1alpha1
kind: CubridBackup
metadata:
  name: %s
  namespace: %s
spec:
  clusterRef:
    name: %s
  database: %s
  level: Full
  destination:
    type: ObjectStorage
    objectStorage:
      provider: S3Compatible
      bucket: %s
      prefix: dba-password
`, backupName, dbaNamespace, clusterName, database, backupBucket))
				Eventually(func(g Gomega) {
					phase, err := kubectl("get", "cubridbackup", backupName, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					if phase == "Failed" {
						conditions, _ := kubectl("get", "cubridbackup", backupName, "-o", "jsonpath={.status.conditions}")
						StopTrying("the backup failed: " + conditions).Now()
					}
					g.Expect(phase).To(Equal("Completed"))
				}, 10*time.Minute, 3*time.Second).Should(Succeed())
				uri, err := kubectl("get", "cubridbackup", backupName, "-o", "jsonpath={.status.artifact.uri}")
				Expect(err).NotTo(HaveOccurred())
				Expect(uri).To(HavePrefix("s3://"+backupBucket+"/"), "the backup reports no artifact URI")
				manifestURI = uri

				By("deleting the source cluster, so that the restored clusters have room on Kind")
				_, err = kubectl("delete", "cubridcluster", clusterName, "--timeout=5m")
				Expect(err).NotTo(HaveOccurred())
			})

			It("restores the backup into a new cluster whose Secret holds the source's password", func() {
				Expect(manifestURI).NotTo(BeEmpty(), "no backup to restore")
				restoredPod := restoredName + "-0"

				By("creating a cluster restored with the source's password, and one with another password")
				apply(passwordSecret(dbaNamespace, otherSecret, otherPassword))
				apply(clusterManifest(restoredName, secretName, manifestURI))
				apply(clusterManifest(otherName, otherSecret, manifestURI))

				By("waiting for the recovery to complete and the cluster to be Ready")
				Eventually(func(g Gomega) {
					phase, err := clusterField(restoredName, "{.status.bootstrap.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					if phase == "Failed" {
						reason, _ := clusterField(restoredName, `{.status.conditions[?(@.type=="BootstrapReady")].message}`)
						StopTrying("the recovery failed: " + reason).Now()
					}
					g.Expect(phase).To(Equal("Complete"))
					ready, err := clusterField(restoredName, `{.status.conditions[?(@.type=="Ready")].status}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(ready).To(Equal("True"))
				}, 15*time.Minute, 3*time.Second).Should(Succeed())

				By("logging in as dba with the empty password")
				out, _ := csqlOn(restoredPod, "", "SELECT 1")
				Expect(out).To(ContainSubstring(refused), "the restored database accepted the empty DBA password")

				By("reading the marker row as dba with the password read from the mounted file inside the Pod")
				out, err := csqlOn(restoredPod, `-p "$(cat `+passwordFile+`)"`, "SELECT marker FROM dba_marker WHERE id = 1")
				Expect(err).NotTo(HaveOccurred(), out)
				Expect(out).NotTo(ContainSubstring(refused))
				Expect(out).To(ContainSubstring(marker), "the restored database lacks the row written before the backup")

				By("logging in through each Service from another namespace and reading the marker row")
				brokerLogins(restoredName, "broker-client-restored", false)
			})

			It("fails the restore into a cluster whose Secret holds another password, without showing it", func() {
				Expect(manifestURI).NotTo(BeEmpty(), "no backup to restore")

				By("waiting for the restore to fail because the database's password is not the requested one")
				// A failed attempt is reported as RestoreFailedRetrying until the
				// next attempt starts, about 10 seconds later, and as a Warning
				// Event of that reason; after the last attempt as RestoreFailed.
				// Either, with the Instance Manager's reason, shows the refusal.
				Eventually(func(g Gomega) {
					condition, err := clusterField(otherName,
						`{.status.conditions[?(@.type=="BootstrapReady")].reason}{"|"}`+
							`{.status.conditions[?(@.type=="BootstrapReady")].message}`)
					g.Expect(err).NotTo(HaveOccurred())
					reason, message, _ := strings.Cut(condition, "|")
					if reason == "RestoreFailed" && !strings.Contains(message, notRequested) {
						StopTrying("the restore failed for another reason: " + message).Now()
					}
					events, err := kubectl("get", "events", "--field-selector",
						"involvedObject.name="+otherName+",reason=RestoreFailedRetrying",
						"-o", "jsonpath={.items[*].message}")
					g.Expect(err).NotTo(HaveOccurred())
					failed := strings.HasPrefix(reason, "RestoreFailed") && strings.Contains(message, notRequested)
					g.Expect(failed || strings.Contains(events, notRequested)).To(BeTrue(),
						"BootstrapReady: %s; RestoreFailedRetrying Events: %s", condition, events)
				}, 15*time.Minute, 3*time.Second).Should(Succeed())
				phase, err := clusterField(otherName, "{.status.bootstrap.phase}")
				Expect(err).NotTo(HaveOccurred())
				Expect(phase).NotTo(Equal("Complete"))
				ready, err := clusterField(otherName, `{.status.conditions[?(@.type=="Ready")].status}`)
				Expect(err).NotTo(HaveOccurred())
				Expect(ready).NotTo(Equal("True"), "a cluster whose restore failed is Ready")

				By("checking that neither password is in the cluster, its StatefulSet, the Events or the Operator log")
				places := map[string][]string{
					"the cluster":      {"-n", dbaNamespace, "get", "cubridcluster", otherName, "-o", "json"},
					"the StatefulSet":  {"-n", dbaNamespace, "get", "statefulset", otherName, "-o", "json"},
					"the Events":       {"-n", dbaNamespace, "get", "events", "-o", "json"},
					"the Operator log": operatorLog,
				}
				noLeak(otherPassword, places)
				noLeak(password, places)

				By("deleting the cluster, whose restore is retried until it gives up")
				_, err = kubectl("delete", "cubridcluster", otherName, "--timeout=5m")
				Expect(err).NotTo(HaveOccurred())
			})

			It("writes the password into no restored cluster, backup, StatefulSet, Event or Operator log line", func() {
				noLeak(password, map[string][]string{
					"the restored cluster":     {"-n", dbaNamespace, "get", "cubridcluster", restoredName, "-o", "json"},
					"the restored StatefulSet": {"-n", dbaNamespace, "get", "statefulset", restoredName, "-o", "json"},
					"the backup":               {"-n", dbaNamespace, "get", "cubridbackup", backupName, "-o", "json"},
					"the Events":               {"-n", dbaNamespace, "get", "events", "-o", "json"},
					"the Broker clients":       {"-n", clientNamespace, "get", "pods", "-o", "json"},
					"the Operator log":         operatorLog,
				})
			})
		})
}
