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
// the empty password, and with the value of the Secret dba-password-old when
// the client namespace holds one, has to be refused for its password; a login
// with the password from the client's Secret has to be accepted and read the
// marker row. With WRITE_MARKER=1 the client first writes that row through -rw. It
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
      imagePullPolicy: Never
      env:
        - {name: RW_HOST, value: "%[3]s-rw.%[4]s.svc"}
        - {name: RO_HOST, value: "%[3]s-ro.%[4]s.svc"}
        - {name: DATABASE, value: "%[5]s"}
        - {name: WRITE_MARKER, value: "%[6]s"}
        - {name: MARKER, value: "%[7]s"}
        - name: DBA_PASSWORD
          valueFrom:
            secretKeyRef: {name: dba-password, key: password}
        - name: OLD_DBA_PASSWORD
          valueFrom:
            secretKeyRef: {name: dba-password-old, key: password, optional: true}
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
          refused = {"empty": ""}
          if os.environ.get("OLD_DBA_PASSWORD"):
              refused["old"] = os.environ["OLD_DBA_PASSWORD"]
          for name in services:
              for which, pw in refused.items():
                  for attempt in range(60):
                      try:
                          login(name, pw).close()
                      except Exception as exc:
                          if getattr(exc, "errno", None) == -171 or "password" in str(exc).lower():
                              print(which, "password refused through", name, ":", repr(exc))
                              break
                          print(which, "password through", name, ", attempt", attempt, ":", repr(exc))
                          time.sleep(2)
                      else:
                          sys.exit("the " + which + " DBA password was accepted through " + name)
                  else:
                      sys.exit("a login with the " + which + " DBA password was never refused for its password "
                               "through " + name)

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
func dbaPasswordScenario() {
	Context("Standalone cluster with a DBA password from a Secret on the real CUBRID image",
		Label("db", "dba-password"), Ordered, func() {
			const (
				dbaNamespace = dbaPasswordStandaloneNamespace
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
				// notRequested is the Instance Manager's reason for a restored
				// database whose password is not the requested one
				// (internal/instancemanager/dbapassword.go).
				notRequested = "DBA password is not the requested one"
			)
			pod := clusterName + "-0"
			manifestURI := ""
			h := dbaPasswordHelpers{namespace: dbaNamespace, clientNamespace: clientNamespace,
				database: database, marker: marker}
			kubectl, apply, clusterField, csqlOn := h.kubectl, h.apply, h.clusterField, h.csqlOn
			passwordSecret, brokerLogins, noLeak := dbaPasswordSecret, h.brokerLogins, expectNoDBAPassword
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
			operatorLog := operatorLogArgs()

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
				out, err := kubectl("exec", pod, "-c", "cubrid", "--", "stat", "-L", "-c", "%a %g", dbaPasswordFile)
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(out)).To(Equal("440 1000"))

				By("logging in as dba with the empty password")
				out, _ = csql("")
				Expect(out).To(ContainSubstring(dbaPasswordRefused), "the database accepted the empty DBA password")

				By("logging in as dba with the password read from the mounted file inside the Pod")
				out, err = csql(`-p "$(cat ` + dbaPasswordFile + `)"`)
				Expect(err).NotTo(HaveOccurred(), out)
				Expect(out).NotTo(ContainSubstring(dbaPasswordRefused))
				Expect(out).NotTo(ContainSubstring("ERROR"))
			})

			It("refuses the empty DBA password and accepts the Secret's through the read-write and the read-only Service",
				func() {
					By("logging in through each Service from another namespace, and writing the marker row through -rw")
					brokerLogins(clusterName, "broker-client-source", true)
				})

			It("writes the password into no status, Event, StatefulSet, Operator or member log line, or file of the member",
				func() {
					noLeak(password, map[string][]string{
						"the cluster":       {"-n", dbaNamespace, "get", "cubridcluster", clusterName, "-o", "json"},
						"the StatefulSet":   {"-n", dbaNamespace, "get", "statefulset", clusterName, "-o", "json"},
						"the Events":        {"-n", dbaNamespace, "get", "events", "-o", "json"},
						"the Operator log":  operatorLog,
						"the log of " + pod: memberLog(pod),
					})
					By("searching the member's operation store, $CUBRID/log and /tmp for the password")
					h.expectNoDBAPasswordOnDisk(pod)
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
				Expect(out).To(ContainSubstring(dbaPasswordRefused), "the restored database accepted the empty DBA password")

				By("reading the marker row as dba with the password read from the mounted file inside the Pod")
				out, err := csqlOn(restoredPod, `-p "$(cat `+dbaPasswordFile+`)"`, "SELECT marker FROM dba_marker WHERE id = 1")
				Expect(err).NotTo(HaveOccurred(), out)
				Expect(out).NotTo(ContainSubstring(dbaPasswordRefused))
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
				places["the log of "+otherName+"-0"] = memberLog(otherName + "-0")
				noLeak(otherPassword, places)
				noLeak(password, places)
				By("searching the member's operation store, $CUBRID/log and /tmp for its password")
				h.expectNoDBAPasswordOnDisk(otherName + "-0")

				By("deleting the cluster, whose restore is retried until it gives up")
				_, err = kubectl("delete", "cubridcluster", otherName, "--timeout=5m")
				Expect(err).NotTo(HaveOccurred())
			})

			It("writes the password into no restored cluster, backup, StatefulSet, Event, Operator or member log line, "+
				"file of the member or object of the bucket", func() {
				restoredPod := restoredName + "-0"
				noLeak(password, map[string][]string{
					"the restored cluster":      {"-n", dbaNamespace, "get", "cubridcluster", restoredName, "-o", "json"},
					"the restored StatefulSet":  {"-n", dbaNamespace, "get", "statefulset", restoredName, "-o", "json"},
					"the backup":                {"-n", dbaNamespace, "get", "cubridbackup", backupName, "-o", "json"},
					"the Events":                {"-n", dbaNamespace, "get", "events", "-o", "json"},
					"the Broker clients":        {"-n", clientNamespace, "get", "pods", "-o", "json"},
					"the Operator log":          operatorLog,
					"the log of " + restoredPod: memberLog(restoredPod),
				})
				By("searching the restored member's operation store, $CUBRID/log and /tmp for the password")
				h.expectNoDBAPasswordOnDisk(restoredPod)
				By("reading every object of the backup bucket, the backup and its manifest, for the password")
				h.expectNoDBAPasswordInBucket(restoredPod, backupBucket)
			})
		})
}

const (
	// dbaPasswordStandaloneNamespace holds the standalone scenario's clusters.
	dbaPasswordStandaloneNamespace = "cubrid-dba-password"
	// dbaPasswordFile is where the Operator mounts the key in the DB
	// container (internal/controller/dbapassword.go).
	dbaPasswordFile = "/etc/cubrid-dba-password/password"
	// dbaPasswordRefused is what csql prints for a wrong password
	// (docs/poc/RESULTS.md, POC-61).
	dbaPasswordRefused = "Incorrect or missing password"
)

// dbaPasswordHelpers runs the commands the DBA password scenarios share,
// against the clusters of one namespace. Its Broker clients run in
// clientNamespace, which holds a copy of the password Secret, and write or
// read marker in database.
type dbaPasswordHelpers struct {
	namespace, clientNamespace, database, marker string
}

func (h dbaPasswordHelpers) kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", append([]string{"-n", h.namespace}, args...)...))
}

func (h dbaPasswordHelpers) apply(manifest string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func (h dbaPasswordHelpers) clusterField(name, jsonPath string) (string, error) {
	return h.kubectl("get", "cubridcluster", name, "-o", "jsonpath="+jsonPath)
}

// csqlOn runs csql in the DB container of member as the cubrid user. login is
// the shell text that gives csql its password option, if any. A test password
// on csql's command line inside the test Pod is acceptable: it is synthetic.
func (h dbaPasswordHelpers) csqlOn(member, login, statement string) (string, error) {
	return h.kubectl(csqlOnArgs(member, h.database, login, statement)...)
}

// csqlOnArgs is the kubectl command line of csqlOn. csql reaches the member's
// own server over TCP by its hostname, not through the local Unix socket that
// "<db>@localhost" uses: over that socket a login that succeeded could still
// print the -199/-111 pair of a lost session (POC-70, #238).
func csqlOnArgs(member, database, login, statement string) []string {
	return []string{"exec", member, "-c", "cubrid", "--", "bash", "-c",
		`PATH="${CUBRID}/bin:${PATH}" csql -u dba ` + login + ` "$0@$(hostname)" -c "$1" </dev/null`,
		database, statement}
}

// brokerLogins waits for a Broker of each access mode of cluster and runs the
// client in clientPod against its two Services.
func (h dbaPasswordHelpers) brokerLogins(cluster, clientPod string, writeMarker bool) {
	for _, mode := range []string{"rw", "ro"} {
		Eventually(func(g Gomega) {
			_, err := h.kubectl("rollout", "status", "deployment/"+cluster+"-broker-"+mode, "--timeout=1m")
			g.Expect(err).NotTo(HaveOccurred())
		}, 5*time.Minute, 5*time.Second).Should(Succeed(), "the %s Brokers of %s did not become available",
			mode, cluster)
	}
	write := "0"
	if writeMarker {
		write = "1"
	}
	h.apply(fmt.Sprintf(dbaPasswordBrokerClientManifest, h.clientNamespace, clientPod, cluster, h.namespace,
		h.database, write, h.marker))
	Eventually(func(g Gomega) {
		phase, err := utils.Run(exec.Command("kubectl", "-n", h.clientNamespace, "get", "pod", clientPod,
			"-o", "jsonpath={.status.phase}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(phase).To(Or(Equal("Succeeded"), Equal("Failed")))
	}, 10*time.Minute, 3*time.Second).Should(Succeed())
	logs, err := utils.Run(exec.Command("kubectl", "-n", h.clientNamespace, "logs", clientPod))
	Expect(err).NotTo(HaveOccurred())
	_, _ = fmt.Fprintf(GinkgoWriter, "broker client output:\n%s\n", logs)
	Expect(logs).To(ContainSubstring("DBA-PASSWORD-BROKER-OK"), "client output:\n%s", logs)
}

// dbaPasswordSecret is a Secret that holds value under the key password.
func dbaPasswordSecret(namespace, name, value string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
stringData:
  password: %s
`, name, namespace, value)
}

// memberLog is the kubectl arguments that print the whole log of all the
// containers of member in the standalone scenario's namespace: the DB container, which runs
// the entrypoint and the Instance Manager, and the others.
func memberLog(member string) []string {
	return []string{"-n", dbaPasswordStandaloneNamespace, "logs", member, "--all-containers", "--tail=-1"}
}

// expectNoDBAPassword checks that value is in none of the places, each the
// arguments of a kubectl command.
func expectNoDBAPassword(value string, places map[string][]string) {
	for what, args := range places {
		out, err := utils.RunQuiet(exec.Command("kubectl", args...))
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), what)
		ExpectWithOffset(1, out).NotTo(ContainSubstring(value), "%s holds a DBA password", what)
	}
}

// diskScanDone ends the output of expectNoDBAPasswordOnDisk's search.
const diskScanDone = "DBA-PASSWORD-DISK-SCAN-DONE"

// expectNoDBAPasswordOnDisk searches, in the DB container of member, the
// Instance Manager's operation store, CUBRID's log directory and /tmp for the
// value of the member's own mounted password file (grep -F -f), so that the
// value is on no command line, also not inside the Pod. A directory that is
// missing, a password file that is empty or cannot be read, and a grep that
// failed (exit 2 or more, such as an unreadable file) are reported, so a
// search that could not look is never taken for a clean one.
func (h dbaPasswordHelpers) expectNoDBAPasswordOnDisk(member string) {
	h.expectNoDBAPasswordUnder(member, `"$IM_OPERATIONS_DIR" "$CUBRID/log" /tmp`)
}

// expectNoDBAPasswordUnder is expectNoDBAPasswordOnDisk's search in dirs, the
// shell words of the directories to search in the DB container.
func (h dbaPasswordHelpers) expectNoDBAPasswordUnder(member, dirs string) {
	out, err := h.kubectl("exec", member, "-c", "cubrid", "--", "bash", "-c",
		`if [ ! -r "$0" ] || [ ! -s "$0" ]; then echo "PATTERN UNREADABLE $0"; exit 0; fi; `+
			`for d in `+dirs+`; do `+
			`if [ -z "$d" ] || [ ! -d "$d" ]; then echo "MISSING directory '$d'"; continue; fi; `+
			`found=$(grep -rlaF -f "$0" -- "$d" 2>&1); rc=$?; `+
			`if [ "$rc" -ge 2 ]; then echo "SCAN ERROR in $d (grep exit $rc): $found"; `+
			`elif [ "$rc" = 0 ]; then printf '%s\n' "$found" | sed 's/^/HOLDS /'; fi; done; echo "$1"`,
		dbaPasswordFile, diskScanDone)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), out)
	ExpectWithOffset(1, strings.TrimSpace(out)).To(Equal(diskScanDone),
		"on %s, one of %s holds the DBA password, or the search could not look:\n%s", member, dirs, out)
}

// bucketScanObjects prefixes the count of objects expectNoDBAPasswordInBucket
// read.
const bucketScanObjects = "OBJECTS "

// expectNoDBAPasswordInBucket lists, from the DB container of member, every
// object of bucket on the mock object store in the client namespace, the
// backups and their manifests, reads each one and searches it for the value
// of the member's own mounted password file. A backup holds the engine's
// stored form of the password (docs/poc/RESULTS.md, POC-64), never the value
// as given. The mock store checks no credentials and lists at most 1000 keys
// at once, more than these scenarios write.
func (h dbaPasswordHelpers) expectNoDBAPasswordInBucket(member, bucket string) {
	endpoint := "http://s3mock." + h.clientNamespace + ".svc:9090/" + bucket
	out, err := h.kubectl("exec", member, "-c", "cubrid", "--", "bash", "-c",
		`if [ ! -r "$0" ] || [ ! -s "$0" ]; then echo "PATTERN UNREADABLE $0"; exit 0; fi; `+
			`set -o pipefail; keys=$(curl -sSf "$1?list-type=2" | grep -o '<Key>[^<]*</Key>' | sed -e 's/<Key>//' -e 's/<\/Key>//') `+
			`|| { echo "LIST FAILED"; exit 1; }; n=0; `+
			`for k in $keys; do n=$((n+1)); curl -sSf "$1/$k" | grep -qaF -f "$0"; st=("${PIPESTATUS[@]}"); `+
			`[ "${st[1]}" = 0 ] && echo "HOLDS $k"; [ "${st[1]}" -ge 2 ] && echo "SCAN ERROR $k (grep exit ${st[1]})"; `+
			`[ "${st[1]}" = 0 ] || [ "${st[0]}" = 0 ] || echo "READ FAILED $k"; `+
			`done; echo "`+bucketScanObjects+`$n"`,
		dbaPasswordFile, endpoint)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), out)
	ExpectWithOffset(1, out).NotTo(ContainSubstring("HOLDS "), "an object of the bucket %s holds the DBA password", bucket)
	ExpectWithOffset(1, out).NotTo(ContainSubstring("READ FAILED"), "an object of the bucket %s could not be read", bucket)
	ExpectWithOffset(1, out).NotTo(Or(ContainSubstring("SCAN ERROR"), ContainSubstring("PATTERN UNREADABLE")),
		"the bucket %s could not be searched:\n%s", bucket, out)
	ExpectWithOffset(1, out).To(MatchRegexp(bucketScanObjects+`[1-9][0-9]*\s*$`),
		"no object of the bucket %s was read:\n%s", bucket, out)
}

// operatorLogArgs are the kubectl arguments that print the whole Operator log.
func operatorLogArgs() []string {
	return []string{"-n", namespace, "logs", "-l", operatorSelector, "--tail=-1", "--all-containers"}
}

// dbaPasswordHAScenario creates a three-member HA cluster whose spec names a
// DBA password Secret, on the real CUBRID image (#834). The Operator starts no
// member while the Secret is missing. Once it exists, one member creates the
// database and sets the password before the other two are seeded from it
// through a mock object store, so they carry the same password. Every member
// then refuses the empty DBA password and accepts the Secret's, in its DB
// container and through the read-write and the read-only Broker Service; a
// row written through -rw is read on every member with the password; and the
// password is in no status, Event, StatefulSet or Operator log line.
//
// The cluster is then backed up and the source deleted, and the backup is
// restored into two new HA clusters at once (#847). The one whose Secret holds
// the source's password restores its first member, seeds the other two from
// it and becomes Ready, with the row and the same login behavior on every
// member and through both Services. The one whose Secret holds another
// password fails its restore on the first member, because the restored
// database keeps its source's password (docs/poc/RESULTS.md, POC-61, Q4;
// #819), and seeds no other member.
//
// Last, the DBA password of the restored cluster is changed by hand on every
// member with the draft procedure of docs/operations/dba-password.md, while
// rows are written, and the Secret is changed after it (#1012).
//
// Kind runs all members on one node and causes no network cut or power loss.
// copylogdb's reconnects with a password set were checked on the VM lab
// through a standby restart and a failover only (#801).
func dbaPasswordHAScenario() {
	Context("HA cluster with a DBA password from a Secret on the real CUBRID image",
		Label("db", "dba-password", "dba-password-ha"), Ordered, func() {
			const (
				haNamespace = "cubrid-dba-password-ha"
				// clientNamespace holds the mock object store and the Broker
				// client: outside the cluster's namespace.
				clientNamespace = "cubrid-dba-password-ha-client"
				clusterName     = "dbaha"
				database        = "appdb"
				secretName      = "dba-password"
				seedBucket      = "seed"
				// The password only this test knows; it is on no command line
				// of the test process.
				password = "E2e-Dba-Password-834"
				// changed is the value the Secret is changed to once the
				// cluster is Ready; it is never the database's password.
				changed = "E2e-Dba-Password-845-changed"
				// marker is the row written through -rw and read on every
				// member.
				marker = "written-through-rw"
				// backupName is the backup of the HA cluster that both recovery
				// clusters restore. It goes to the seed bucket under a prefix of
				// its own; the seed backups are kept below each cluster's UID.
				backupName   = "dbaha-full"
				backupPrefix = "dba-password-ha-backup"
				// restoredName is restored from the backup with the source's
				// password in its Secret, otherName with otherPassword.
				restoredName  = "dbaha-restored"
				otherName     = "dbaha-other"
				otherSecret   = "dba-password-other"
				otherPassword = "E2e-Other-Password-847"
				// notRequested is the Instance Manager's reason for a restored
				// database whose password is not the requested one
				// (internal/instancemanager/dbapassword.go).
				notRequested = "DBA password is not the requested one"
				// newPassword is the value the restored cluster's DBA password
				// is changed to by hand (#1012); oldSecret holds the value it
				// replaces for the Broker client, which must be refused.
				newPassword = "E2e-Dba-Password-1012-new"
				oldSecret   = "dba-password-old"
			)
			members := []string{clusterName + "-0", clusterName + "-1", clusterName + "-2"}
			membersOf := func(cluster string) []string {
				return []string{cluster + "-0", cluster + "-1", cluster + "-2"}
			}
			h := dbaPasswordHelpers{namespace: haNamespace, clientNamespace: clientNamespace,
				database: database, marker: marker}
			withPassword := `-p "$(cat ` + dbaPasswordFile + `)"`
			manifestURI := ""
			// clusterManifest is a three-member HA cluster with the password
			// Secret secret, the mock object store for its seeding, the client
			// namespace as the Brokers' clients and, when recoveryURI is set, a
			// recovery from that backup.
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
    promotableMembers: 3
    readReplicas: 0
  highAvailability:
    enabled: true
  objectStorage:
    endpoint: s3mock.%s.svc:9090
    insecure: true
    bucket: %s
    credentialsSecretRef:
      name: object-storage
  # Kind has a single node: let the members share it (#90).
  placement:
    memberSpread: Preferred
  storage:
    data:
      size: 2Gi
  dbaPasswordSecretRef:
    name: %s
    key: password
  networkPolicy:
    clients:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: %s
%s`, name, haNamespace, repository, tag, database, clientNamespace, seedBucket, secret, clientNamespace, recovery)
			}
			// haHealthy reads whether the cluster is Ready with HAReady and
			// ReplicationHealthy True. Ready alone is not a replication-health
			// oracle: the other two must agree with the seeded data state.
			haHealthy := func(g Gomega, name string) {
				for _, condition := range []string{"Ready", "HAReady", "ReplicationHealthy"} {
					out, err := h.clusterField(name, `{.status.conditions[?(@.type=="`+condition+`")].status}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("True"), "%s of %s", condition, name)
				}
			}
			// memberLogs adds to places, for each member of cluster, the kubectl
			// arguments that print the whole log of all its containers: the DB
			// container, which runs the Instance Manager, and the others.
			memberLogs := func(places map[string][]string, cluster string) {
				for _, member := range membersOf(cluster) {
					places["the log of "+member] = []string{"-n", haNamespace, "logs", member, "--all-containers",
						"--tail=-1"}
				}
			}
			// passwordCondition reads the status and the reason of the DBA
			// password Condition of cluster.
			passwordCondition := func(g Gomega, cluster string) (string, string) {
				status, err := h.clusterField(cluster,
					`{.status.conditions[?(@.type=="DBAPasswordSecretResolved")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				reason, err := h.clusterField(cluster,
					`{.status.conditions[?(@.type=="DBAPasswordSecretResolved")].reason}`)
				g.Expect(err).NotTo(HaveOccurred())
				return status, reason
			}
			// fileHolds waits until the mounted password file of member holds
			// value, compared in the container with the value read from stdin.
			fileHolds := func(member, value string) {
				EventuallyWithOffset(1, func() error {
					cmd := exec.Command("kubectl", "-n", haNamespace, "exec", "-i", member, "-c", "cubrid", "--",
						"bash", "-c", `IFS= read -r v; [ "$v" = "$(cat "$0")" ]`, dbaPasswordFile)
					cmd.Stdin = strings.NewReader(value + "\n")
					_, err := utils.RunQuiet(cmd)
					return err
				}, 3*time.Minute, 3*time.Second).Should(Succeed(), "the kubelet did not update the file on %s", member)
			}
			// restartOperator restarts the Operator, so that it asks a member
			// about the DBA password at once instead of waiting out the
			// 5-minute poll. A restart loses the Operator's log, so it is first
			// searched for each of values.
			restartOperator := func(values ...string) {
				for _, value := range values {
					expectNoDBAPassword(value, map[string][]string{"the Operator log": operatorLogArgs()})
				}
				_, err := utils.Run(exec.Command("kubectl", "-n", namespace, "rollout", "restart", operatorDeployment))
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				_, err = utils.Run(exec.Command("kubectl", "-n", namespace, "rollout", "status", operatorDeployment,
					"--timeout=3m"))
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
			}

			BeforeAll(func() {
				if runtime.GOARCH != "amd64" {
					Skip("needs linux/amd64: the official CUBRID image has no " + runtime.GOARCH + " build. Not run.")
				}
				ensureInstanceManagerImage()

				By("starting a mock object store for the seeding, in the client namespace")
				_, err := utils.Run(exec.Command("kubectl", "create", "ns", clientNamespace))
				Expect(err).NotTo(HaveOccurred())
				h.apply(fmt.Sprintf(objectStoreManifest, clientNamespace, seedBucket))
				_, err = utils.Run(exec.Command("kubectl", "-n", clientNamespace, "rollout", "status",
					"deployment/s3mock", "--timeout=5m"))
				Expect(err).NotTo(HaveOccurred(), "the mock object store did not start")
				// The Broker client reads the password from here.
				h.apply(dbaPasswordSecret(clientNamespace, secretName, password))

				By("creating a namespace that enforces the restricted security policy")
				_, err = utils.Run(exec.Command("kubectl", "create", "ns", haNamespace))
				Expect(err).NotTo(HaveOccurred())
				_, err = utils.Run(exec.Command("kubectl", "label", "--overwrite", "ns", haNamespace,
					"pod-security.kubernetes.io/enforce=restricted"))
				Expect(err).NotTo(HaveOccurred())
				// The mock store checks no credentials.
				_, err = h.kubectl("create", "secret", "generic", "object-storage",
					"--from-literal=accessKey=e2e", "--from-literal=secretKey=e2e")
				Expect(err).NotTo(HaveOccurred())

				By("creating the three-member HA cluster first, so that it waits for its Secret")
				h.apply(clusterManifest(clusterName, secretName, ""))
			})

			AfterAll(func() {
				if CurrentSpecReport().Failed() {
					for _, args := range [][]string{
						{"get", "cubridcluster", "-o", "yaml"},
						{"get", "cubridbackup", "-o", "yaml"},
						{"get", "pods", "-o", "wide"},
						{"logs", members[0], "--tail=200"},
						{"logs", members[1], "--tail=200"},
						{"logs", restoredName + "-0", "--tail=200"},
						{"logs", restoredName + "-1", "--tail=200"},
						{"logs", otherName + "-0", "--tail=200"},
						{"get", "events", "--sort-by=.lastTimestamp"},
					} {
						out, _ := h.kubectl(append(args, "--request-timeout=10s")...)
						_, _ = fmt.Fprintf(GinkgoWriter, "%v:\n%s\n", args, out)
					}
					// The Operator's log shows how it reconciled the cluster after
					// each restart (#1031); the members' logs above hold the
					// Instance Manager's answer to each password check. The
					// passwords are test values, and a step of this scenario
					// checks that the Operator log holds neither.
					out, _ := utils.Run(exec.Command("kubectl", "-n", namespace, "logs", "-l", operatorSelector,
						"--all-containers", "--tail=300", "--request-timeout=10s"))
					_, _ = fmt.Fprintf(GinkgoWriter, "the Operator log:\n%s\n", out)
				}
				_, _ = h.kubectl("delete", "cubridbackup", "--all", "--ignore-not-found", "--timeout=5m")
				_, _ = h.kubectl("delete", "cubridcluster", "--all", "--ignore-not-found", "--timeout=5m")
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", haNamespace, clientNamespace,
					"--ignore-not-found", "--timeout=5m"))
			})

			It("starts no member and no HA bootstrap until the Secret exists, then reports it resolved", func() {
				Eventually(func(g Gomega) {
					out, err := h.clusterField(clusterName,
						`{.status.conditions[?(@.type=="DBAPasswordSecretResolved")].reason}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("DBAPasswordSecretNotFound"))
				}, time.Minute, 2*time.Second).Should(Succeed())
				out, err := h.kubectl("get", "statefulset", "--ignore-not-found", "-o", "name")
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(out)).To(BeEmpty(), "a member would create the database without its password")
				out, err = h.clusterField(clusterName, "{.status.databases}")
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(out)).To(BeEmpty(), "the HA bootstrap started without the password")

				By("creating the Secret")
				h.apply(dbaPasswordSecret(haNamespace, secretName, password))
				Eventually(func(g Gomega) {
					out, err := h.clusterField(clusterName,
						`{.status.conditions[?(@.type=="DBAPasswordSecretResolved")].status}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("True"))
				}, time.Minute, 2*time.Second).Should(Succeed())
			})

			It("creates the database on one member, seeds the others from it and becomes Ready", func() {
				Eventually(func(g Gomega) {
					phase, err := h.clusterField(clusterName, "{.status.databases[0].phase}")
					g.Expect(err).NotTo(HaveOccurred())
					if phase == "Failed" {
						reason, _ := h.clusterField(clusterName,
							`{.status.conditions[?(@.type=="BootstrapReady")].message}`)
						StopTrying("the HA bootstrap failed: " + reason).Now()
					}
					configured, err := h.clusterField(clusterName, "{.status.databases[0].haConfigured}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(configured).To(Equal("true"))
					ready, err := h.clusterField(clusterName, `{.status.conditions[?(@.type=="Ready")].status}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(ready).To(Equal("True"))
					// Ready alone is not a replication-health oracle. HAReady and
					// ReplicationHealthy must agree with the seeded data state.
					haReady, err := h.clusterField(clusterName, `{.status.conditions[?(@.type=="HAReady")].status}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(haReady).To(Equal("True"))
					repl, err := h.clusterField(clusterName, `{.status.conditions[?(@.type=="ReplicationHealthy")].status}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(repl).To(Equal("True"))
				}, 15*time.Minute, 3*time.Second).Should(Succeed())
			})

			It("refuses the empty DBA password and accepts the Secret's on every member", func() {
				for _, member := range members {
					By("checking that only the cubrid user's group can read the password file on " + member)
					out, err := h.kubectl("exec", member, "-c", "cubrid", "--", "stat", "-L", "-c", "%a %g", dbaPasswordFile)
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimSpace(out)).To(Equal("440 1000"), member)

					By("logging in as dba with the empty password on " + member)
					out, _ = h.csqlOn(member, "", "SELECT 1")
					Expect(out).To(ContainSubstring(dbaPasswordRefused), "%s accepted the empty DBA password", member)

					By("logging in as dba with the password read from the mounted file on " + member)
					out, err = h.csqlOn(member, withPassword, "SELECT 1")
					Expect(err).NotTo(HaveOccurred(), "%s: %s", member, out)
					Expect(out).NotTo(ContainSubstring(dbaPasswordRefused), member)
					Expect(out).NotTo(ContainSubstring("ERROR"), member)
				}
			})

			It("refuses the empty DBA password and accepts the Secret's through the read-write and the read-only Service",
				func() {
					By("logging in through each Service from another namespace, and writing the marker row through -rw")
					h.brokerLogins(clusterName, "broker-client-ha", true)
				})

			It("has the row written through -rw on every member, read with the Secret's password", func() {
				for _, member := range members {
					Eventually(func(g Gomega) {
						out, err := h.csqlOn(member, withPassword, "SELECT marker FROM dba_marker WHERE id = 1")
						g.Expect(err).NotTo(HaveOccurred(), out)
						g.Expect(out).NotTo(ContainSubstring(dbaPasswordRefused))
						g.Expect(out).To(ContainSubstring(marker))
					}, 3*time.Minute, 3*time.Second).Should(Succeed(), "%s lacks the row written through -rw", member)
				}
			})

			// The Operator asks every member at most once every 5 minutes, and
			// at once after it restarts (docs/operations/dba-password.md). The
			// step waits until the kubelet has written the changed value into
			// every member's file and then restarts the Operator, so that it
			// does not wait out the poll (#845). A match needs every member to
			// match (#1012), so the value put back must have reached every file.
			It("reports a changed Secret value as an unsupported rotation, and True again once it is put back", func() {
				primary, err := h.clusterField(clusterName, "{.status.currentPrimary}")
				Expect(err).NotTo(HaveOccurred())
				Expect(primary).To(BeElementOf(members), "no primary is resolved")
				resolved := func(g Gomega) (string, string) { return passwordCondition(g, clusterName) }
				// changeTo puts value into the Secret, waits until every member's
				// mounted file holds it and restarts the Operator.
				changeTo := func(value string) {
					h.apply(dbaPasswordSecret(haNamespace, secretName, value))
					for _, member := range members {
						fileHolds(member, value)
					}
					restartOperator(password, changed)
				}

				By("changing the Secret's value after the cluster became Ready")
				changeTo(changed)
				Eventually(func(g Gomega) {
					status, reason := resolved(g)
					g.Expect(status).To(Equal("False"))
					g.Expect(reason).To(Equal("DBAPasswordRotationUnsupported"))
				}, 3*time.Minute, 3*time.Second).Should(Succeed())
				events, err := h.kubectl("get", "events", "--field-selector", "reason=DBAPasswordRotationUnsupported",
					"-o", "jsonpath={.items[*].type}")
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.Fields(events)).To(ContainElement("Warning"))

				// While the Secret holds the changed value, the primary's file holds
				// it too: its operation store, $CUBRID/log and /tmp must not.
				By("searching the primary's operation store, $CUBRID/log and /tmp for the changed value")
				h.expectNoDBAPasswordOnDisk(primary)

				By("putting the database's password back")
				changeTo(password)
				Eventually(func(g Gomega) {
					status, reason := resolved(g)
					g.Expect(status).To(Equal("True"))
					g.Expect(reason).To(Equal("DBAPasswordSecretResolved"))
				}, 3*time.Minute, 3*time.Second).Should(Succeed())
			})

			It("writes the password into no status, Event, StatefulSet, Broker client or Operator log line", func() {
				places := map[string][]string{
					"the cluster":        {"-n", haNamespace, "get", "cubridcluster", clusterName, "-o", "json"},
					"the StatefulSet":    {"-n", haNamespace, "get", "statefulset", clusterName, "-o", "json"},
					"the Events":         {"-n", haNamespace, "get", "events", "-o", "json"},
					"the Broker clients": {"-n", clientNamespace, "get", "pods", "-o", "json"},
					"the Operator log":   operatorLogArgs(),
				}
				for _, member := range members {
					places["the log of "+member] = []string{"-n", haNamespace, "logs", member, "--all-containers",
						"--tail=-1"}
				}
				expectNoDBAPassword(password, places)
				expectNoDBAPassword(changed, places)
				for _, member := range members {
					By("searching the operation store, $CUBRID/log and /tmp of " + member + " for the password")
					h.expectNoDBAPasswordOnDisk(member)
				}
			})

			// The steps below restore a backup of this cluster into two new HA
			// clusters (#847). The step before put the database's password back
			// into the Secret, and the database's own password was never
			// changed, so the backup holds the Secret's password.
			It("backs the HA database up to object storage", func() {
				h.apply(fmt.Sprintf(`apiVersion: database.cubrid.io/v1alpha1
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
      prefix: %s
`, backupName, haNamespace, clusterName, database, seedBucket, backupPrefix))
				Eventually(func(g Gomega) {
					phase, err := h.kubectl("get", "cubridbackup", backupName, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					if phase == "Failed" {
						conditions, _ := h.kubectl("get", "cubridbackup", backupName, "-o", "jsonpath={.status.conditions}")
						StopTrying("the backup failed: " + conditions).Now()
					}
					g.Expect(phase).To(Equal("Completed"))
				}, 10*time.Minute, 3*time.Second).Should(Succeed())
				uri, err := h.kubectl("get", "cubridbackup", backupName, "-o", "jsonpath={.status.artifact.uri}")
				Expect(err).NotTo(HaveOccurred())
				Expect(uri).To(HavePrefix("s3://"+seedBucket+"/"+backupPrefix+"/"), "the backup reports no artifact URI")
				manifestURI = uri

				By("checking that the backup and the members' logs hold no password, before the logs go")
				places := map[string][]string{
					"the backup": {"-n", haNamespace, "get", "cubridbackup", backupName, "-o", "json"},
				}
				memberLogs(places, clusterName)
				expectNoDBAPassword(password, places)
				expectNoDBAPassword(changed, places)

				By("deleting the source cluster, so that the two recovery clusters have room on Kind")
				_, err = h.kubectl("delete", "cubridcluster", clusterName, "--timeout=5m")
				Expect(err).NotTo(HaveOccurred())

				By("creating a recovery cluster with the source's password, and one with another password, at once")
				h.apply(dbaPasswordSecret(haNamespace, otherSecret, otherPassword))
				h.apply(clusterManifest(restoredName, secretName, manifestURI))
				h.apply(clusterManifest(otherName, otherSecret, manifestURI))
			})

			// The restore runs on the first member only, and the other members
			// are seeded only once it completed (internal/controller/bootstrap.go,
			// reconcileRecovery). A restore that fails on the first member is
			// started again there, up to three attempts, and never seeds the
			// others, so this cluster copies no data beyond the first member.
			It("fails the HA restore whose Secret holds another password on its first member, and seeds no other member",
				func() {
					Expect(manifestURI).NotTo(BeEmpty(), "no backup to restore")

					By("waiting for the restore to fail because the database's password is not the requested one")
					// A failed attempt is reported as RestoreFailedRetrying until the
					// next attempt starts, about 10 seconds later, and as a Warning
					// Event of that reason; after the last attempt as RestoreFailed.
					// Either, with the Instance Manager's reason, shows the refusal.
					Eventually(func(g Gomega) {
						condition, err := h.clusterField(otherName,
							`{.status.conditions[?(@.type=="BootstrapReady")].reason}{"|"}`+
								`{.status.conditions[?(@.type=="BootstrapReady")].message}`)
						g.Expect(err).NotTo(HaveOccurred())
						reason, message, _ := strings.Cut(condition, "|")
						if reason == "RestoreFailed" && !strings.Contains(message, notRequested) {
							StopTrying("the restore failed for another reason: " + message).Now()
						}
						events, err := h.kubectl("get", "events", "--field-selector",
							"involvedObject.name="+otherName+",reason=RestoreFailedRetrying",
							"-o", "jsonpath={.items[*].message}")
						g.Expect(err).NotTo(HaveOccurred())
						failed := strings.HasPrefix(reason, "RestoreFailed") && strings.Contains(message, notRequested)
						g.Expect(failed || strings.Contains(events, notRequested)).To(BeTrue(),
							"BootstrapReady: %s; RestoreFailedRetrying Events: %s", condition, events)
					}, 10*time.Minute, 3*time.Second).Should(Succeed())

					By("checking that the recovery seeded no member and the cluster is not Ready")
					phase, err := h.clusterField(otherName, "{.status.bootstrap.phase}")
					Expect(err).NotTo(HaveOccurred())
					Expect(phase).NotTo(BeElementOf("SeedingReplicas", "Complete"))
					restored, err := h.clusterField(otherName, "{.status.databases[*].primaryCreated}")
					Expect(err).NotTo(HaveOccurred())
					Expect(restored).NotTo(ContainSubstring("true"), "the restore is recorded as complete on the first member")
					seeded, err := h.clusterField(otherName, "{.status.databases[*].seededMembers}")
					Expect(err).NotTo(HaveOccurred())
					Expect(strings.TrimSpace(seeded)).To(BeEmpty(), "a member was seeded from a failed restore")
					ready, err := h.clusterField(otherName, `{.status.conditions[?(@.type=="Ready")].status}`)
					Expect(err).NotTo(HaveOccurred())
					Expect(ready).NotTo(Equal("True"), "a cluster whose restore failed is Ready")

					By("checking that neither password is in the cluster, its StatefulSet, the Events, the Operator " +
						"log or its members' logs")
					places := map[string][]string{
						"the cluster":      {"-n", haNamespace, "get", "cubridcluster", otherName, "-o", "json"},
						"the StatefulSet":  {"-n", haNamespace, "get", "statefulset", otherName, "-o", "json"},
						"the Events":       {"-n", haNamespace, "get", "events", "-o", "json"},
						"the Operator log": operatorLogArgs(),
					}
					memberLogs(places, otherName)
					expectNoDBAPassword(otherPassword, places)
					expectNoDBAPassword(password, places)

					By("deleting the cluster, whose restore is retried until it gives up")
					_, err = h.kubectl("delete", "cubridcluster", otherName, "--timeout=5m")
					Expect(err).NotTo(HaveOccurred())
				})

			It("restores the backup into an HA cluster whose Secret holds the source's password, seeds every member "+
				"and becomes Ready", func() {
				Expect(manifestURI).NotTo(BeEmpty(), "no backup to restore")
				Eventually(func(g Gomega) {
					phase, err := h.clusterField(restoredName, "{.status.bootstrap.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					if phase == "Failed" {
						reason, _ := h.clusterField(restoredName,
							`{.status.conditions[?(@.type=="BootstrapReady")].message}`)
						StopTrying("the HA recovery failed: " + reason).Now()
					}
					g.Expect(phase).To(Equal("Complete"))
					haHealthy(g, restoredName)
				}, 15*time.Minute, 3*time.Second).Should(Succeed())
			})

			It("refuses the empty DBA password and accepts the Secret's on every restored member, which has the row",
				func() {
					for _, member := range membersOf(restoredName) {
						By("logging in as dba with the empty password on " + member)
						out, _ := h.csqlOn(member, "", "SELECT 1")
						Expect(out).To(ContainSubstring(dbaPasswordRefused), "%s accepted the empty DBA password", member)

						By("reading the row written before the backup with the password from the mounted file on " +
							member)
						out, err := h.csqlOn(member, withPassword, "SELECT marker FROM dba_marker WHERE id = 1")
						Expect(err).NotTo(HaveOccurred(), "%s: %s", member, out)
						Expect(out).NotTo(ContainSubstring(dbaPasswordRefused), member)
						Expect(out).To(ContainSubstring(marker), "%s lacks the row written before the backup", member)
					}
				})

			It("refuses the empty DBA password and accepts the Secret's through the restored cluster's read-write "+
				"and read-only Service", func() {
				By("logging in through each Service from another namespace and reading the row")
				h.brokerLogins(restoredName, "broker-client-ha-restored", false)
			})

			It("writes neither password into the restored cluster, the backup, a StatefulSet, an Event, a Broker "+
				"client, the Operator log or a member's log", func() {
				places := map[string][]string{
					"the restored cluster":     {"-n", haNamespace, "get", "cubridcluster", restoredName, "-o", "json"},
					"the restored StatefulSet": {"-n", haNamespace, "get", "statefulset", restoredName, "-o", "json"},
					"the backup":               {"-n", haNamespace, "get", "cubridbackup", backupName, "-o", "json"},
					"the Events":               {"-n", haNamespace, "get", "events", "-o", "json"},
					"the Broker clients":       {"-n", clientNamespace, "get", "pods", "-o", "json"},
					"the Operator log":         operatorLogArgs(),
				}
				memberLogs(places, restoredName)
				expectNoDBAPassword(password, places)
				expectNoDBAPassword(otherPassword, places)
				expectNoDBAPassword(changed, places)
				for _, member := range membersOf(restoredName) {
					By("searching the operation store, $CUBRID/log and /tmp of " + member + " for the password")
					h.expectNoDBAPasswordOnDisk(member)
				}
				By("reading every object of the seed bucket, the backups and their manifests, for the password")
				h.expectNoDBAPasswordInBucket(membersOf(restoredName)[0], seedBucket)
			})

			// The steps below change the DBA password of the restored cluster by
			// hand, as docs/operations/dba-password.md, "A manual change on an HA
			// cluster (draft)", describes (#1012, POC-74): the statement file is
			// copied into every member through stdin with mode 0600, csql runs
			// through `kubectl exec -it` on a pseudo-terminal, the current
			// password is typed at its prompt, and the file is loaded with ;read
			// and run with ;run, on the primary and then, with --sysadm
			// --write-on-standby, on each slave. The Secret is changed last. They
			// reuse this running three-member cluster instead of bootstrapping
			// another one. Nothing here restarts a member or fails over.
			var (
				changePrimary string
				changeWriter  *rowWriter
				stopWriter    func()
				before171     = map[string]int{}
				failBefore    = map[string]int{}
			)
			It("changes the DBA password by hand on every member through kubectl exec on a terminal, while rows "+
				"are written", func() {
				restored := membersOf(restoredName)
				primary, err := h.clusterField(restoredName, "{.status.currentPrimary}")
				Expect(err).NotTo(HaveOccurred())
				Expect(primary).To(BeElementOf(restored), "no primary is resolved")
				changePrimary = primary

				By("creating a user without a password, and the table it writes, on the primary " + primary)
				out, err := h.csqlOn(primary, withPassword, "CREATE USER "+pwChangeWriter+"; "+
					"CREATE TABLE dba_pwchange (id INT PRIMARY KEY, phase VARCHAR(16)); "+
					"GRANT SELECT, INSERT ON dba_pwchange TO "+pwChangeWriter)
				Expect(err).NotTo(HaveOccurred(), out)
				Expect(out).NotTo(ContainSubstring("ERROR"), out)

				By("counting the -171 lines of every member's copiers and appliers, and each slave's applier fail count")
				for _, member := range restored {
					lines, files := h.passwordErrors(member)
					before171[member] = lines
					_, _ = fmt.Fprintf(GinkgoWriter, "%s: %d -171 lines in %d copier and applier logs\n", member,
						lines, files)
					if member == primary {
						continue
					}
					Expect(files).To(BeNumerically(">", 0), "%s has no copier or applier log", member)
					Eventually(func(g Gomega) {
						failBefore[member], err = h.applierFailCount(member, primary)
						g.Expect(err).NotTo(HaveOccurred())
					}, time.Minute, 3*time.Second).Should(Succeed())
				}

				By("writing rows before the change, and starting to write while it runs")
				changeWriter = &rowWriter{namespace: haNamespace, member: primary, database: database}
				for id := 1; id <= 5; id++ {
					changeWriter.write(id, "before")
				}
				stopWriter = changeWriter.start(1001, "during")
				DeferCleanup(func() { stopWriter() })

				By("copying the statement file into every member through stdin, readable by its owner only")
				for _, member := range restored {
					cmd := exec.Command("kubectl", "-n", haNamespace, "exec", "-i", member, "-c", "cubrid", "--",
						"bash", "-c", `umask 077; cat > "$0"`, pwChangeFile)
					cmd.Stdin = strings.NewReader(pwChangeStatement(newPassword))
					_, err := utils.Run(cmd)
					Expect(err).NotTo(HaveOccurred())
					out, err := h.kubectl("exec", member, "-c", "cubrid", "--", "stat", "-c", "%a %u", pwChangeFile)
					Expect(err).NotTo(HaveOccurred(), out)
					Expect(strings.TrimSpace(out)).To(Equal("600 1000"), member)
				}

				// change runs csql on member's terminal, logged in with the current
				// password, and loads and runs the file there.
				change := func(member string, options ...string) {
					t, err := runCSQLDialog(kubectlExecTerminal(haNamespace, member, database,
						append(options, "-u", "dba")...),
						password, []string{";read " + pwChangeFile, ";run", ";exit"}, 3*time.Minute)
					shown, found := redactValues(t.all, password, newPassword)
					_, _ = fmt.Fprintf(GinkgoWriter, "csql on a terminal on %s:\n%s\n", member, shown)
					Expect(err).NotTo(HaveOccurred(), "csql on %s: %s", member, shown)
					Expect(found).To(BeZero(), "the terminal of csql on %s showed a DBA password", member)
					Expect(t.afterPassword).NotTo(ContainSubstring(dbaPasswordRefused), member)
					Expect(t.afterPassword).To(ContainSubstring("The file has been read into the command buffer."),
						member)
					Expect(t.afterPassword).To(ContainSubstring(pwChangeDone), member)
				}
				By("changing the password on the primary " + primary)
				change(primary)
				for _, member := range restored {
					if member != primary {
						By("changing the password on the slave " + member + " with --sysadm --write-on-standby")
						change(member, "--sysadm", "--write-on-standby")
					}
				}

				By("removing the statement file from every member")
				for _, member := range restored {
					out, err := h.kubectl("exec", member, "-c", "cubrid", "--", "bash", "-c",
						`rm -f "$0" && [ ! -e "$0" ] && echo REMOVED`, pwChangeFile)
					Expect(err).NotTo(HaveOccurred(), out)
					Expect(out).To(ContainSubstring("REMOVED"), member)
				}

				By("writing a few seconds longer, then stopping")
				time.Sleep(5 * time.Second)
				stopWriter()
			})

			It("accepts the new password and refuses the old one on every member, at csql's prompt", func() {
				for _, member := range membersOf(restoredName) {
					By("logging in with each value on " + member)
					Expect(h.terminalLogin(member, newPassword, password)).To(Equal("accepted"),
						"%s refused the new password", member)
					Expect(h.terminalLogin(member, password, newPassword)).To(Equal("refused"),
						"%s did not refuse the old password", member)
				}
				By("writing rows after the change")
				Expect(changeWriter).NotTo(BeNil(), "the change did not run")
				for id := 2001; id <= 2005; id++ {
					changeWriter.write(id, "after")
				}
			})

			It("has every row written before, during and after the change on every member, with no new -171 line "+
				"and no applier failure", func() {
				Expect(changeWriter).NotTo(BeNil(), "the change did not run")
				acked, unknown := changeWriter.ackedRows()
				_, _ = fmt.Fprintf(GinkgoWriter, "acknowledged writes: %v; writes with an unknown outcome: %d\n",
					acked, unknown)
				phases := map[string]int{}
				for _, id := range acked {
					switch {
					case id < 1000:
						phases["before"]++
					case id < 2000:
						phases["during"]++
					default:
						phases["after"]++
					}
				}
				for _, phase := range []string{"before", "during", "after"} {
					Expect(phases[phase]).To(BeNumerically(">", 0), "no write was acknowledged %s the change", phase)
				}
				for _, member := range membersOf(restoredName) {
					Eventually(func(g Gomega) {
						ids, err := h.memberRowIDs(member)
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(ids).To(ContainElements(acked), "%s lacks acknowledged rows", member)
					}, 3*time.Minute, 3*time.Second).Should(Succeed())
					lines, _ := h.passwordErrors(member)
					Expect(lines).To(Equal(before171[member]),
						"the copiers or appliers of %s logged error -171 after the change", member)
					if member == changePrimary {
						continue
					}
					count, err := h.applierFailCount(member, changePrimary)
					Expect(err).NotTo(HaveOccurred())
					Expect(count).To(Equal(failBefore[member]), "the applier of %s failed after the change", member)
				}
				Eventually(func(g Gomega) { haHealthy(g, restoredName) }, 3*time.Minute, 3*time.Second).
					Should(Succeed())
			})

			// The Operator's check asks every member whether the Secret's value
			// logs in on it (#1012). Until the Secret is changed it reports the
			// database's new password as an unsupported rotation, refused on
			// every member, slaves included; once it holds the new value, every
			// member matches and the Condition is True again, which needs each
			// slave's own answer. The Operator is restarted before each, so
			// that it asks at once.
			It("reports the change until the Secret holds the new password, and True once it does", func() {
				By("restarting the Operator while the Secret still holds the old value")
				restartOperator(password, newPassword)
				Eventually(func(g Gomega) {
					status, reason := passwordCondition(g, restoredName)
					g.Expect(status).To(Equal("False"))
					g.Expect(reason).To(Equal("DBAPasswordRotationUnsupported"))
					msg, err := h.clusterField(restoredName,
						`{.status.conditions[?(@.type=="DBAPasswordSecretResolved")].message}`)
					g.Expect(err).NotTo(HaveOccurred())
					for _, member := range membersOf(restoredName) {
						g.Expect(msg).To(ContainSubstring(member), "the check did not report %s's answer", member)
					}
					g.Expect(msg).NotTo(ContainSubstring("accepted on"))
				}, 3*time.Minute, 3*time.Second).Should(Succeed())

				By("putting the new value into the Secret, last, and waiting until every member's file holds it")
				h.apply(dbaPasswordSecret(haNamespace, secretName, newPassword))
				for _, member := range membersOf(restoredName) {
					fileHolds(member, newPassword)
				}
				restartOperator(password, newPassword)
				Eventually(func(g Gomega) {
					status, reason := passwordCondition(g, restoredName)
					g.Expect(status).To(Equal("True"))
					g.Expect(reason).To(Equal("DBAPasswordSecretResolved"))
				}, 3*time.Minute, 3*time.Second).Should(Succeed())
			})

			It("refuses the old password and accepts the new one through the read-write and the read-only Service",
				func() {
					By("giving the Broker client the new value, and the old one to be refused")
					h.apply(dbaPasswordSecret(clientNamespace, oldSecret, password))
					h.apply(dbaPasswordSecret(clientNamespace, secretName, newPassword))
					h.brokerLogins(restoredName, "broker-client-ha-changed", false)
				})

			It("writes neither value into the cluster, a StatefulSet, an Event, a Broker client, the Operator log, a "+
				"member's log, or a member's files and database", func() {
				places := map[string][]string{
					"the restored cluster":     {"-n", haNamespace, "get", "cubridcluster", restoredName, "-o", "json"},
					"the restored StatefulSet": {"-n", haNamespace, "get", "statefulset", restoredName, "-o", "json"},
					"the Events":               {"-n", haNamespace, "get", "events", "-o", "json"},
					"the Broker clients":       {"-n", clientNamespace, "get", "pods", "-o", "json"},
					"the Operator log":         operatorLogArgs(),
				}
				memberLogs(places, restoredName)
				expectNoDBAPassword(newPassword, places)
				expectNoDBAPassword(password, places)
				// Every member's file holds the new value by now (fileHolds), so
				// the searches below look for it.
				for _, member := range membersOf(restoredName) {
					By("searching the operation store, $CUBRID/log, /tmp and the databases of " + member +
						" for the new password")
					h.expectNoDBAPasswordOnDisk(member)
					h.expectNoDBAPasswordUnder(member, `"$CUBRID_DATABASES"`)
				}
			})
		})
}
