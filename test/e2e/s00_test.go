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
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// s00Evidence is what an S00 run records about itself.
type s00Evidence struct {
	Scenario            string   `json:"scenario"`
	Result              string   `json:"result"`
	Revision            string   `json:"revision"`
	Host                string   `json:"host"`
	ContainerTool       string   `json:"containerTool"`
	NodeImage           string   `json:"nodeImage,omitempty"`
	InstanceManagerRef  string   `json:"instanceManagerImage"`
	InstanceManagerID   string   `json:"instanceManagerImageID,omitempty"`
	EngineVersion       string   `json:"engineVersion,omitempty"`
	PodUID              string   `json:"podUID,omitempty"`
	FirstStartSeconds   float64  `json:"firstStartToReadySeconds,omitempty"`
	RestartSeconds      float64  `json:"podDeleteToReadySeconds,omitempty"`
	ContainerRestarts   string   `json:"containerRestarts,omitempty"`
	ProbeFailureEvents  []string `json:"probeFailureEvents"`
	RowKeptAcrossDelete bool     `json:"rowKeptAcrossPodDelete"`
}

// s00Scenario registers S00: on a clean Kind cluster, a standalone
// CubridCluster with the real CUBRID image becomes Ready, answers SQL, and
// keeps its data when its Pod is deleted. It must be called inside the
// container that deploys the operator.
//
// The official CUBRID image is linux/amd64 only, so on another architecture
// the scenario is skipped: a skip is "not run", never a pass.
func s00Scenario() {
	Context("S00 standalone cluster with the real CUBRID image", Label("db", "S00"), Ordered, func() {
		const (
			s00Namespace = "cubrid-s00"
			clusterName  = "s00"
			database     = "appdb"
			marker       = "s00-before-pod-delete"
		)
		pod := clusterName + "-0"
		record := &s00Evidence{Scenario: "S00", Result: string(evidence.Fail), ProbeFailureEvents: []string{}}
		// podReplaced is set once the deleted Pod is seen to be gone.
		podReplaced := false
		// podDeletionOutcome is the run of the Pod deletion, once it happened.
		var podDeletionOutcome *faults.Outcome

		kubectl := func(args ...string) (string, error) {
			return utils.Run(exec.Command("kubectl", append([]string{"-n", s00Namespace}, args...)...))
		}
		waitPodReady := func(timeout time.Duration) time.Duration {
			start := time.Now()
			Eventually(func(g Gomega) {
				out, err := kubectl("get", "pod", pod,
					"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("True"))
			}, timeout, 2*time.Second).Should(Succeed())
			return time.Since(start)
		}

		BeforeAll(func() {
			if runtime.GOARCH != "amd64" {
				Skip("S00 needs linux/amd64: the official CUBRID image has no " + runtime.GOARCH + " build. Not run.")
			}
			record.Host = runtime.GOOS + "/" + runtime.GOARCH
			record.ContainerTool = utils.ContainerTool()
			record.NodeImage = os.Getenv("KIND_NODE_IMAGE")
			record.InstanceManagerRef = instanceManagerImage
			if rev, err := utils.Run(exec.Command("git", "rev-parse", "HEAD")); err == nil {
				record.Revision = strings.TrimSpace(rev)
			}

			ensureInstanceManagerImage()

			By("creating a namespace that enforces the restricted security policy")
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", s00Namespace))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "label", "--overwrite", "ns", s00Namespace,
				"pod-security.kubernetes.io/enforce=restricted"))
			Expect(err).NotTo(HaveOccurred())

			By("creating a standalone CubridCluster")
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
    promotableMembers: 1
    readReplicas: 0
  highAvailability:
    enabled: false
  storage:
    data:
      size: 2Gi
`, clusterName, s00Namespace, repository, tag, database)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the CubridCluster")
		})

		AfterAll(func() {
			if record.Host == "" {
				// Skipped: nothing ran, and that is not a pass.
				recordScenario(evidence.Scenario{ID: "S00", Result: evidence.NotRun,
					Reason: "needs linux/amd64, ran on " + runtime.GOOS + "/" + runtime.GOARCH})
				return
			}
			// What a reader needs to judge the run, pass or fail.
			if out, err := kubectl("get", "events", "--field-selector", "reason=Unhealthy",
				"-o", `jsonpath={range .items[*]}{.involvedObject.name}{": "}{.message}{"\n"}{end}`); err == nil {
				record.ProbeFailureEvents = append(record.ProbeFailureEvents, utils.GetNonEmptyLines(out)...)
			}
			if out, err := kubectl("get", "pod", pod, "-o", "jsonpath={.status.containerStatuses[0].restartCount}"); err == nil {
				record.ContainerRestarts = out
			}
			for _, args := range [][]string{
				{"get", "cubridcluster", clusterName, "-o", "yaml"},
				{"describe", "pod", pod},
				{"logs", pod, "--tail=200"},
				{"logs", pod, "--previous", "--tail=200"},
				{"get", "events", "--sort-by=.lastTimestamp"},
			} {
				out, _ := kubectl(args...)
				_, _ = fmt.Fprintf(GinkgoWriter, "%v:\n%s\n", args, out)
			}
			result := evidence.Scenario{ID: "S00", Result: evidence.Result(record.Result),
				FaultConfirmed: &podReplaced,
				Measurements: map[string]any{
					"firstStartToReadySeconds": measured(record.FirstStartSeconds),
					"podDeleteToReadySeconds":  measured(record.RestartSeconds),
				}}
			if result.Result != evidence.Pass {
				result.Reason = "a step of the scenario failed; see the test output"
			}
			if file := writeEvidence(record); file != "" {
				result.Evidence = []string{file}
			}
			// The fault's own verdict wins over the default: a deletion that
			// was not confirmed is blocked, not failed.
			if o := podDeletionOutcome; o != nil {
				flow := o.Scenario("S00", "", podDeletionLimits)
				result.Limits, result.CleanupSucceeded = flow.Limits, flow.CleanupSucceeded
				if o.Result != evidence.Pass {
					result.Result, result.Reason = o.Result, o.Reason
				}
				if file := writeTimeline("S00/timeline.jsonl", o.Timeline); file != "" {
					result.Evidence = append(result.Evidence, file)
				}
			}
			recordScenario(result)
			runSummary.Environment.CubridImageDigest = record.InstanceManagerID
			runSummary.Environment.EngineVersion = record.EngineVersion

			_, _ = kubectl("delete", "cubridcluster", clusterName, "--ignore-not-found", "--timeout=5m")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", s00Namespace, "--ignore-not-found", "--timeout=5m"))
		})

		It("becomes Ready as a non-root Pod and answers SQL", func() {
			By("waiting for the first start: createdb, server start, then the manager")
			record.FirstStartSeconds = waitPodReady(15 * time.Minute).Seconds()

			uid, err := kubectl("get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
			Expect(err).NotTo(HaveOccurred())
			record.PodUID = uid
			if id, err := kubectl("get", "pod", pod, "-o", "jsonpath={.status.containerStatuses[0].imageID}"); err == nil {
				record.InstanceManagerID = id
			}

			By("checking the process user")
			out, err := kubectl("exec", pod, "--", "id", "-u")
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(out)).To(Equal("1000"))

			By("recording the engine version")
			out, err = kubectl("exec", pod, "--", "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid_rel`)
			Expect(err).NotTo(HaveOccurred())
			record.EngineVersion = strings.TrimSpace(out)

			By("writing and reading a row through csql")
			sql := csqlInPod(s00Namespace, pod, database)
			Expect(writeS00Row(sql, marker)).To(Succeed())
			Expect(readS00Row(sql, marker)).To(Succeed())

			By("checking that a failing statement fails the check")
			_, err = runSQL(sql, "SELECT * FROM s00_no_such_table;")
			Expect(err).To(HaveOccurred(), "an SQL error must not pass")

			By("checking the cluster's Ready condition and the member's alias Service")
			Eventually(func(g Gomega) {
				ready, err := kubectl("get", "cubridcluster", clusterName,
					"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(Equal("True"))
			}).Should(Succeed())
			_, err = kubectl("get", "service", pod)
			Expect(err).NotTo(HaveOccurred())
		})

		It("keeps the data when the Pod is deleted", func() {
			sql := csqlInPod(s00Namespace, pod, database)
			podReady := func() bool {
				out, err := kubectl("get", "pod", pod,
					"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
				return err == nil && out == "True"
			}

			By("deleting the Pod, confirming its replacement and reading the row again")
			outcome := faults.Run(context.Background(), faults.Scenario{
				Fault:  podDeletion{namespace: s00Namespace, pod: pod, uid: record.PodUID},
				Limits: podDeletionLimits,
				Before: func(context.Context) error { return readS00Row(sql, marker) },
				// The row written before the deletion is read from the new Pod.
				Check: func(context.Context) (bool, error) {
					return podReady() && readS00Row(sql, marker) == nil, nil
				},
			})
			podDeletionOutcome = &outcome
			podReplaced = outcome.FaultConfirmed
			if !outcome.OutcomeAt.IsZero() {
				record.RestartSeconds = outcome.OutcomeAt.Sub(outcome.FaultIssuedAt).Seconds()
			}
			Expect(outcome.Result).To(Equal(evidence.Pass), "pod deletion: %s", outcome.Reason)
			record.RowKeptAcrossDelete = true
			record.Result = string(evidence.Pass)
		})
	})
}

// instanceManagerImageOnce builds and loads the real Instance Manager image
// once for all real-database scenarios of a run.
var instanceManagerImageOnce sync.Once

func ensureInstanceManagerImage() {
	instanceManagerImageOnce.Do(func() {
		By("building the Instance Manager image on the official CUBRID image")
		_, err := utils.Run(exec.Command("make", "docker-build-instance-manager",
			fmt.Sprintf("IM_IMG=%s", instanceManagerImage)))
		Expect(err).NotTo(HaveOccurred(), "Failed to build the Instance Manager image")

		By("loading the Instance Manager image on Kind")
		Expect(utils.LoadImageToKindClusterWithName(instanceManagerImage)).To(Succeed())
	})
}

// writeEvidence prints the S00 record and stores it with the run's evidence.
// It returns the file's path relative to the run directory, or "" when no
// evidence is kept.
func writeEvidence(e *s00Evidence) string {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return ""
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "S00 evidence:\n%s\n", data)
	return writeScenarioFile("S00/record.json", append(data, '\n'))
}

// measured returns a duration in seconds, or "unknown" when the step that
// measures it did not finish: zero would read as a measurement.
func measured(seconds float64) any {
	if seconds == 0 {
		return evidence.Unknown
	}
	return seconds
}

// podDeletionLimits bound the deletion of a DB Pod in S00. They are the waits
// the scenario used before it had named limits, not measured values.
var podDeletionLimits = faults.Limits{
	Confirm: 5 * time.Minute,
	Outcome: 15 * time.Minute,
	Cleanup: time.Minute,
	Poll:    2 * time.Second,
}

// podDeletion is the fault "a Pod is deleted". It is confirmed when a Pod of
// the same name exists with another UID: the exit code of the delete command
// is not relied on. There is nothing to remove afterwards.
//
// With force the Pod gets no termination grace period: its containers are
// killed without the preStop hook and without SIGTERM.
type podDeletion struct {
	namespace, pod, uid string
	force               bool
}

func (f podDeletion) Name() string {
	if f.force {
		return "delete Pod " + f.namespace + "/" + f.pod + " without a grace period"
	}
	return "delete Pod " + f.namespace + "/" + f.pod
}

func (f podDeletion) Inject(ctx context.Context) error {
	args := []string{"-n", f.namespace, "delete", "pod", f.pod, "--wait=false"}
	if f.force {
		args = append(args, "--grace-period=0", "--force")
	}
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", args...))
	return err
}

func (f podDeletion) Confirm(ctx context.Context) (bool, error) {
	uid, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", f.namespace, "get", "pod", f.pod,
		"-o", "jsonpath={.metadata.uid}"))
	if err != nil {
		return false, err
	}
	return uid != "" && uid != f.uid, nil
}

func (f podDeletion) Remove(context.Context) error { return nil }

// writeTimeline stores a run's timeline as JSON lines with the run's evidence.
func writeTimeline(rel string, steps []faults.Step) string {
	var b strings.Builder
	for _, step := range steps {
		line, err := json.Marshal(step)
		if err != nil {
			return ""
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return writeScenarioFile(rel, []byte(b.String()))
}
