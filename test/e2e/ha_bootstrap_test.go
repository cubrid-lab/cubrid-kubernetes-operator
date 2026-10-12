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
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// objectStoreManifest is a throwaway S3-compatible store for the scenario: a
// mock with one bucket, in its own namespace. It keeps nothing and checks no
// credentials, so it shows that the transfer works, not that a real object
// store does. The version matters: the manager's client cannot store an
// object in s3mock 4.7.0 ("The specified key does not exist"), while 5.2.3
// accepts the same calls.
const objectStoreManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: s3mock
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels: {app: s3mock}
  template:
    metadata:
      labels: {app: s3mock}
    spec:
      containers:
        - name: s3mock
          image: docker.io/adobe/s3mock:5.2.3
          env:
            - name: COM_ADOBE_TESTING_S3MOCK_STORE_INITIAL_BUCKETS
              value: %[2]s
          ports:
            - containerPort: 9090
          readinessProbe:
            tcpSocket: {port: 9090}
            periodSeconds: 3
---
apiVersion: v1
kind: Service
metadata:
  name: s3mock
  namespace: %[1]s
spec:
  selector: {app: s3mock}
  ports:
    - port: 9090
      targetPort: 9090
`

// brokerClientManifest is a client outside the cluster's namespace that talks
// to the two Broker Services with pycubrid, the CUBRID driver for Python: a
// committed write through -rw, the same row read through -ro, and a write
// through -ro that has to be refused. It prints BROKER-CHECK-OK when all
// three hold. broker_tester is not used: its statements do not persist
// (docs/poc/RESULTS.md, POC-8).
const brokerClientManifest = `apiVersion: v1
kind: Pod
metadata:
  name: broker-client
  namespace: %[1]s
spec:
  restartPolicy: Never
  containers:
    - name: client
      image: docker.io/library/python:3.12-slim
      env:
        - {name: RW_HOST, value: "%[2]s-rw.%[3]s.svc"}
        - {name: RO_HOST, value: "%[2]s-ro.%[3]s.svc"}
        - {name: DATABASE, value: "%[4]s"}
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

          def connect(host, port):
              return pycubrid.connect(host=host, port=port, database=db, user="dba", password="")

          rw = connect(os.environ["RW_HOST"], 33000)
          cur = rw.cursor()
          cur.execute("CREATE TABLE broker_check (id INT PRIMARY KEY, marker VARCHAR(32))")
          cur.execute("INSERT INTO broker_check VALUES (1, 'through-rw')")
          rw.commit()
          rw.close()
          print("write through -rw committed")

          rows = None
          for attempt in range(60):
              try:
                  ro = connect(os.environ["RO_HOST"], 33001)
                  cur = ro.cursor()
                  cur.execute("SELECT id, marker FROM broker_check")
                  rows = cur.fetchall()
                  if rows:
                      break
                  ro.close()
              except Exception as exc:  # the table may not have replicated yet
                  print("read through -ro, attempt", attempt, ":", exc)
              time.sleep(2)
          if not rows or rows[0][1] != "through-rw":
              sys.exit("the row written through -rw was not read through -ro: %%r" %% (rows,))
          print("read through -ro:", rows)

          try:
              cur.execute("INSERT INTO broker_check VALUES (2, 'through-ro')")
              ro.commit()
          except Exception as exc:
              print("write through -ro refused:", exc)
          else:
              sys.exit("a write through -ro was accepted")
          print("BROKER-CHECK-OK")
          PY
`

// workloadClientManifest is the SQL workload client of the scenario contract,
// outside the cluster's namespace. It creates the tables and runs its
// operations through the read-write Service, keeps the history in a file, and
// then stays up so that the test can read the history and dump the rows
// through either Service.
const workloadClientManifest = `apiVersion: v1
kind: Pod
metadata:
  name: workload-client
  namespace: %[1]s
spec:
  restartPolicy: Never
  containers:
    - name: client
      image: %[5]s
      imagePullPolicy: Never
      env:
        - {name: JDBC_URL, value: "jdbc:cubrid:%[2]s-rw.%[3]s.svc:33000:%[4]s:::?connectTimeout=10&queryTimeout=30"}
        - {name: RO_JDBC_URL, value: "jdbc:cubrid:%[2]s-ro.%[3]s.svc:33001:%[4]s:::?connectTimeout=10&queryTimeout=30"}
        - {name: CLIENT_ID, value: "c1"}
        - {name: OPS, value: "60"}
      command: ["sh", "-c"]
      args:
        - |
          set -e
          workload init
          workload run > /tmp/history.jsonl
          echo WORKLOAD-DONE
          sleep 3600
`

// haSetup labels the steps that form the HA cluster the scenario steps use.
const haSetup = "ha-setup"

// haBootstrapScenario registers the HA bootstrap on a real engine: of three
// members exactly one creates the database; the other two are seeded from it
// through object storage and join as slaves; a row written on the master is
// read on both slaves (ADR-0006, ADR-0010).
//
// Its last steps are S01 and S02 of the scenario contract
// (s01AndS02Steps), which continue on the cluster these steps formed.
// Skipped, which is "not run", on anything but amd64.
//
// The steps that form the cluster carry the label haSetup and each scenario
// step the label of its scenario, so that one scenario runs on a formed
// cluster with E2E_LABEL_FILTER='ha-setup || S03-graceful'.
//
// A failed step does not stop the group: each step after S01 starts only
// from the restored and verified starting state of S01 (haRun.enter), so a
// failure is not carried into the next step. Once a step that forms the
// cluster failed, or the starting state could not be restored, every later
// step is blocked without being started.
func haBootstrapScenario() {
	Context("HA bootstrap with the real CUBRID image", Label("db", "ha-bootstrap"), Ordered, ContinueOnFailure, func() {
		const (
			haNamespace    = "cubrid-ha-bootstrap"
			storeNamespace = "cubrid-ha-bootstrap-store"
			seedBucket     = "seed"
			clusterName    = "hab"
			database       = "appdb"
			marker         = "written-on-the-master"
		)
		first := clusterName + "-0"
		peers := []string{clusterName + "-1", clusterName + "-2"}
		ran := false
		// S01 and S02 of the scenario contract continue on this cluster.
		scenarios := newHARun(haNamespace, storeNamespace, clusterName, database, append([]string{first}, peers...))

		kubectl := func(args ...string) (string, error) {
			return utils.Run(exec.Command("kubectl", append([]string{"-n", haNamespace}, args...)...))
		}
		clusterField := func(jsonPath string) (string, error) {
			return kubectl("get", "cubridcluster", clusterName, "-o", "jsonpath="+jsonPath)
		}
		heartbeatStatus := func(pod string) (string, error) {
			return kubectl("exec", pod, "--", "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
		}
		apply := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		BeforeAll(func() {
			if runtime.GOARCH != "amd64" {
				Skip("needs linux/amd64: the official CUBRID image has no " + runtime.GOARCH + " build. Not run.")
			}
			ran = true
			ensureInstanceManagerImage()

			By("starting a mock object store with the seed bucket")
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", storeNamespace))
			Expect(err).NotTo(HaveOccurred())
			apply(fmt.Sprintf(objectStoreManifest, storeNamespace, seedBucket))
			_, err = utils.Run(exec.Command("kubectl", "-n", storeNamespace, "rollout", "status",
				"deployment/s3mock", "--timeout=5m"))
			Expect(err).NotTo(HaveOccurred(), "the mock object store did not start")

			By("creating a namespace that enforces the restricted security policy")
			_, err = utils.Run(exec.Command("kubectl", "create", "ns", haNamespace))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "label", "--overwrite", "ns", haNamespace,
				"pod-security.kubernetes.io/enforce=restricted"))
			Expect(err).NotTo(HaveOccurred())
			_, err = kubectl("create", "secret", "generic", "object-storage",
				"--from-literal=accessKey=e2e", "--from-literal=secretKey=e2e-secret")
			Expect(err).NotTo(HaveOccurred())

			By("creating a three-member HA CubridCluster")
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
  storage:
    data:
      size: 2Gi
  # The workload clients run in the store namespace.
  networkPolicy:
    clients:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: %s
`, clusterName, haNamespace, repository, tag, database, storeNamespace, seedBucket, storeNamespace))
		})

		AfterAll(func() {
			if !ran {
				scenarios.reportNotRun("needs linux/amd64: the official CUBRID image has no other build")
				return
			}
			for _, args := range [][]string{
				{"get", "cubridcluster", clusterName, "-o", "yaml"},
				{"get", "pods", "-o", "wide"},
				{"logs", first, "--tail=100"},
				{"logs", peers[0], "--tail=100"},
				{"get", "events", "--sort-by=.lastTimestamp"},
			} {
				out, _ := kubectl(args...)
				_, _ = fmt.Fprintf(GinkgoWriter, "%v:\n%s\n", args, out)
			}
			for _, pod := range append([]string{first}, peers...) {
				out, _ := heartbeatStatus(pod)
				_, _ = fmt.Fprintf(GinkgoWriter, "heartbeat status on %s:\n%s\n", pod, out)
			}
			_, _ = kubectl("delete", "cubridcluster", clusterName, "--ignore-not-found", "--timeout=5m")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", haNamespace, storeNamespace,
				"--ignore-not-found", "--timeout=5m"))
		})

		BeforeEach(func() {
			if reason := scenarios.gate.Blocked(); reason != "" {
				blockStep(reason)
			}
		})

		AfterEach(func() {
			report := CurrentSpecReport()
			switch {
			case !report.Failed():
			case slices.Contains(report.Labels(), haSetup):
				scenarios.gate.Block("the HA cluster was not formed: the step \"" + report.LeafNodeText + "\" failed")
			case slices.Contains(report.Labels(), "S01"):
				scenarios.gate.Block("the common starting state was not reached: S01 failed")
			}
		})

		It("creates the database on one member only", Label(haSetup), func() {
			By("waiting for the operator to record the database")
			Eventually(func(g Gomega) {
				created, err := clusterField("{.status.databases[0].primaryCreated}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(created).To(Equal("true"))
			}, 15*time.Minute, 3*time.Second).Should(Succeed())

			By("checking that the host of the database is the member list")
			out, err := kubectl("exec", first, "--", "cat", "/var/lib/cubrid/databases/databases.txt")
			Expect(err).NotTo(HaveOccurred())
			members := strings.Join(append([]string{first}, peers...), ":")
			Expect(out).To(ContainSubstring(members))
		})

		It("seeds the other members, which join as slaves", Label(haSetup), func() {
			By("waiting for the operator to record the seeding")
			Eventually(func(g Gomega) {
				phase, err := clusterField("{.status.databases[0].phase}")
				g.Expect(err).NotTo(HaveOccurred())
				if phase == "Failed" {
					reason, _ := clusterField(`{.status.conditions[?(@.type=="BootstrapReady")].message}`)
					StopTrying("the bootstrap failed: " + reason).Now()
				}
				configured, err := clusterField("{.status.databases[0].haConfigured}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(configured).To(Equal("true"))
			}, 15*time.Minute, 3*time.Second).Should(Succeed())

			By("asking CUBRID on every member for the roles")
			Eventually(func(g Gomega) {
				out, err := heartbeatStatus(first)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("current " + first + ", state master"))
				g.Expect(out).To(ContainSubstring("registered_and_active"))
				for _, peer := range peers {
					g.Expect(out).To(ContainSubstring("Node " + peer + " (priority"))
					g.Expect(out).NotTo(ContainSubstring("Node " + peer + " (priority 2, state unknown)"))
					g.Expect(out).NotTo(ContainSubstring("Node " + peer + " (priority 3, state unknown)"))
					status, err := heartbeatStatus(peer)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(status).To(ContainSubstring("current " + peer + ", state slave"))
					g.Expect(status).To(ContainSubstring("registered_and_standby"))
				}
			}, 10*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("replicates a row written on the master to both slaves", Label(haSetup), func() {
			By("writing on the master")
			Eventually(func() error {
				return writeS00Row(csqlInPod(haNamespace, first, database), marker)
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("reading on each slave")
			for _, peer := range peers {
				Eventually(func() error {
					return readS00Row(csqlInPod(haNamespace, peer, database), marker)
				}, 3*time.Minute, 3*time.Second).Should(Succeed(), peer)
			}

			By("checking that a slave refuses a write")
			_, err := runSQL(csqlInPod(haNamespace, peers[0], database),
				"INSERT INTO "+s00Table+" VALUES (99, 'on-a-slave');")
			Expect(err).To(HaveOccurred())
		})

		It("serves SQL through the read-write and the read-only Service", Label(haSetup), func() {
			By("waiting for a Broker of each access mode")
			for _, mode := range []string{"rw", "ro"} {
				_, err := kubectl("rollout", "status", "deployment/"+clusterName+"-broker-"+mode, "--timeout=5m")
				Expect(err).NotTo(HaveOccurred(), "the %s Brokers did not become available", mode)
				service := clusterName + "-" + mode
				Eventually(func(g Gomega) {
					ready, err := kubectl("get", "endpointslices", "-l", "kubernetes.io/service-name="+service,
						"-o", "jsonpath={.items[*].endpoints[?(@.conditions.ready==true)].addresses[0]}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(strings.Fields(ready)).To(HaveLen(2), "ready endpoints of %s", service)
				}, 2*time.Minute, 3*time.Second).Should(Succeed())
			}

			By("running a client with a CUBRID driver against the two Services")
			apply(fmt.Sprintf(brokerClientManifest, storeNamespace, clusterName, haNamespace, database))
			Eventually(func(g Gomega) {
				phase, err := utils.Run(exec.Command("kubectl", "-n", storeNamespace, "get", "pod", "broker-client",
					"-o", "jsonpath={.status.phase}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Or(Equal("Succeeded"), Equal("Failed")))
			}, 10*time.Minute, 3*time.Second).Should(Succeed())
			logs, err := utils.Run(exec.Command("kubectl", "-n", storeNamespace, "logs", "broker-client"))
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "broker client output:\n%s\n", logs)
			Expect(logs).To(ContainSubstring("BROKER-CHECK-OK"), "client output:\n%s", logs)

			By("checking that the operator reports both endpoints")
			Eventually(func(g Gomega) {
				for _, conditionType := range []string{"BrokerReady", "WriteEndpointReady", "ReadEndpointReady"} {
					status, err := clusterField(`{.status.conditions[?(@.type=="` + conditionType + `")].status}`)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(status).To(Equal("True"), conditionType)
				}
			}, 2*time.Minute, 3*time.Second).Should(Succeed())
		})

		It("holds what a recorded workload was told, on both Services", Label(haSetup), func() {
			By("building and loading the workload client image")
			_, err := utils.Run(exec.Command("make", "docker-build-workload",
				fmt.Sprintf("WORKLOAD_IMG=%s", workloadImage)))
			Expect(err).NotTo(HaveOccurred(), "Failed to build the workload client image")
			Expect(utils.LoadImageToKindClusterWithName(workloadImage)).To(Succeed())

			By("running the workload through the read-write Service")
			apply(fmt.Sprintf(workloadClientManifest, storeNamespace, clusterName, haNamespace, database, workloadImage))
			client := func(args ...string) (string, error) {
				return utils.Run(exec.Command("kubectl", append(
					[]string{"-n", storeNamespace, "exec", "workload-client", "--"}, args...)...))
			}
			Eventually(func(g Gomega) {
				phase, err := utils.Run(exec.Command("kubectl", "-n", storeNamespace, "get", "pod", "workload-client",
					"-o", "jsonpath={.status.phase}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).NotTo(Equal("Failed"), "the workload client failed")
				logs, err := utils.Run(exec.Command("kubectl", "-n", storeNamespace, "logs", "workload-client"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(logs).To(ContainSubstring("WORKLOAD-DONE"), "client output:\n%s", logs)
			}, 10*time.Minute, 3*time.Second).Should(Succeed())

			By("reading the history the client kept")
			text, err := client("cat", "/tmp/history.jsonl")
			Expect(err).NotTo(HaveOccurred())
			history, err := workload.ReadHistory(strings.NewReader(text))
			Expect(err).NotTo(HaveOccurred(), "history:\n%s", text)
			Expect(history.Counts[workload.Attempted]).To(Equal(60))
			Expect(history.Counts[workload.Acknowledged]).To(Equal(60),
				"with no fault every operation is acknowledged: %v", history.Counts)

			By("checking the rows read through each Service against the history")
			var report workload.Report
			Eventually(func(g Gomega) {
				members := map[string][]workload.Row{}
				for name, dump := range map[string]string{
					"rw": "workload dump",
					"ro": `JDBC_URL="$RO_JDBC_URL" workload dump`,
				} {
					out, err := client("sh", "-c", dump)
					g.Expect(err).NotTo(HaveOccurred())
					members[name], err = workload.ReadRows(strings.NewReader(out))
					g.Expect(err).NotTo(HaveOccurred(), "rows through -%s:\n%s", name, out)
				}
				report = workload.Check(history, members)
				g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
			}, 2*time.Minute, 3*time.Second).Should(Succeed())
			// 60 operations, every fifth rolled back.
			Expect(report.Members["rw"].Rows).To(Equal(48))
			Expect(report.Members["ro"].Rows).To(Equal(48))

			runSummary.Environment.ClientDriver = workloadDriver
			scenarios.addHistory(text)
			writeScenarioFile("ha-bootstrap/history.jsonl", []byte(text))
			if data, err := json.MarshalIndent(report, "", "  "); err == nil {
				writeScenarioFile("ha-bootstrap/data-check.json", append(data, '\n'))
			}
		})

		It("reports the cluster Ready with the master as primary", Label(haSetup), func() {
			Eventually(func(g Gomega) {
				ready, err := clusterField(`{.status.conditions[?(@.type=="Ready")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(Equal("True"))
				primary, err := clusterField("{.status.currentPrimary}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(primary).To(Equal(first))
				boot, err := clusterField(`{.status.conditions[?(@.type=="BootstrapReady")].reason}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(boot).To(Equal("PeersSeeded"))
			}, 5*time.Minute, 3*time.Second).Should(Succeed())
			for _, pod := range append([]string{first}, peers...) {
				restarts, err := kubectl("get", "pod", pod, "-o", "jsonpath={.status.containerStatuses[0].restartCount}")
				Expect(err).NotTo(HaveOccurred())
				Expect(restarts).To(Equal("0"), pod)
			}
		})

		s01AndS02Steps(scenarios)
		// Each step starts from the master the one before it left. The
		// Broker failures and the Operator restart move no role, so the
		// first member is still the master for the variant of S03 that needs
		// it; that variant is blocked when it is not. After that the master
		// is whichever member CUBRID chose, and no later step depends on
		// which one it is.
		s14Step(scenarios)
		s05Steps(scenarios)
		s06Step(scenarios, s06Restart)
		s03Step(scenarios, s03AbruptFirst)
		s06Step(scenarios, s06Absent)
		s03Step(scenarios, s03Abrupt)
		s03Step(scenarios, s03Graceful)
		replicationStallStep(scenarios)
		// The token variants of S14 replace every member's Pod, the master
		// last, which moves the master; no step after them depends on which
		// member it is.
		for _, variant := range s14TokenVariants {
			s14TokenStep(scenarios, variant)
		}
	})
}
