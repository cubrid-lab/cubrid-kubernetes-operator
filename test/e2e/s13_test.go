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
	"maps"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// The variants of S13 in docs/testing/scenario-contract.md: the cluster is
// deleted with spec.storage.retentionPolicy left at its default, Retain, and
// set to Delete.
const (
	s13Retain = "retain"
	s13Delete = "delete"
)

var s13Variants = []string{s13Retain, s13Delete}

const (
	// s13Cluster is the name of the standalone cluster of both variants; the
	// retain variant creates it again under the same name.
	s13Cluster  = "s13"
	s13Database = "appdb"
	s13Marker   = "s13-before-cluster-delete"
	// s13ReadyWait bounds a first start or a start over retained data until
	// the member's Pod is Ready, as S00 bounds its first start.
	s13ReadyWait = 15 * time.Minute
	// s13DeleteWait is the test's own bound on the removal of what the
	// Operator created, so that the wait ends. It is not delete_limit, which
	// judges the time measured within it; a set delete_limit at or above it
	// extends the wait (s13Limits).
	s13DeleteWait = 5 * time.Minute
)

// The lines build/instance-manager/entrypoint.sh prints for a standalone
// member (init_db): the first when it runs createdb, the second when
// databases.txt already registers the database and createdb is not run.
func s13CreatedbLine(database string) string { return "[im-entrypoint] createdb '" + database + "'" }

func s13PresentLine(database string) string {
	return "[im-entrypoint] database '" + database + "' already present"
}

// s13StartVerdict judges a member's container log for one start of the
// standalone cluster. complete is false when the container restarted more
// than once, so that kubectl no longer keeps the log of its first start.
//
// At the first start (recreated false) the log must show createdb: it is the
// control that the line looked for below is the one the entrypoint prints,
// and without it the later check could not tell a createdb. At the start over
// retained data (recreated true) createdb must never appear, since it would
// create the database again over the retained volume, and the log must say
// that the database was already present. A log that cannot show either is
// blocked, not passed.
func s13StartVerdict(log string, complete bool, database string, recreated bool) (evidence.Result, string) {
	created := strings.Contains(log, s13CreatedbLine(database))
	present := strings.Contains(log, s13PresentLine(database))
	switch {
	case recreated && created:
		return evidence.Fail, "the member of the recreated cluster ran createdb for '" + database +
			"' over the retained volume: the database was created again over retained data"
	case !complete:
		return evidence.Blocked, "the member's container restarted more than once, so the log of its first " +
			"start is gone and cannot show whether createdb ran"
	case !recreated && !created:
		return evidence.Blocked, "the log of the cluster's first start does not show the entrypoint's createdb " +
			"line, so the log of the recreated member cannot show whether createdb ran again"
	case recreated && !present:
		return evidence.Fail, "the log of the recreated member shows neither createdb nor that '" + database +
			"' was already present"
	}
	return evidence.Pass, ""
}

// s13PVC is what S13 reads of one PersistentVolumeClaim. Exists is false when
// the claim was not found.
type s13PVC struct {
	Exists            bool   `json:"exists"`
	UID               string `json:"uid,omitempty"`
	Volume            string `json:"volumeName,omitempty"`
	Phase             string `json:"phase,omitempty"`
	DeletionTimestamp string `json:"deletionTimestamp,omitempty"`
	ReadAt            string `json:"readAt"`
}

// s13RetainedPVCHeld returns an error when the claim recorded before the
// deletion is gone, was replaced by another of the same name, is being
// deleted, or is bound to another volume. Each is a PVC removed, or about to
// be, under Retain.
func s13RetainedPVCHeld(name string, before, now s13PVC) error {
	switch {
	case !now.Exists:
		return fmt.Errorf("the PVC %s was removed under Retain", name)
	case now.UID != before.UID:
		return fmt.Errorf("the PVC %s was removed under Retain and replaced: its UID was %s and is %s",
			name, before.UID, now.UID)
	case now.DeletionTimestamp != "":
		return fmt.Errorf("the PVC %s is being deleted under Retain (deletionTimestamp %s)", name,
			now.DeletionTimestamp)
	case now.Volume != before.Volume:
		return fmt.Errorf("the PVC %s was bound to %s and is bound to %s", name, before.Volume, now.Volume)
	}
	return nil
}

// s13ReadPVC reads one claim of the namespace.
func s13ReadPVC(ctx context.Context, namespace, name string) (s13PVC, error) {
	out, err := utils.RunQuiet(exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", "pvc", name,
		"--ignore-not-found", "-o",
		`jsonpath={.metadata.uid}{"\t"}{.spec.volumeName}{"\t"}{.status.phase}{"\t"}{.metadata.deletionTimestamp}`))
	pvc := s13PVC{ReadAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err != nil {
		return pvc, err
	}
	if strings.TrimSpace(out) == "" {
		return pvc, nil
	}
	fields := strings.Split(out, "\t")
	if len(fields) != 4 {
		return pvc, fmt.Errorf("unexpected output for the PVC %s: %q", name, out)
	}
	pvc.Exists = true
	pvc.UID, pvc.Volume, pvc.Phase, pvc.DeletionTimestamp = fields[0], fields[1], fields[2], fields[3]
	return pvc, nil
}

// s13Remaining lists what is left in the namespace of the kinds the Operator
// creates for a cluster, the CubridCluster itself included, and with pvcs the
// claims too. The namespace holds this one cluster and nothing else, so each
// object found is the cluster's.
func s13Remaining(ctx context.Context, namespace string, pvcs bool) ([]string, error) {
	kinds := "cubridclusters,statefulsets,deployments,services,pods"
	if pvcs {
		kinds += ",persistentvolumeclaims"
	}
	out, err := utils.RunQuiet(exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", kinds, "-o", "name"))
	if err != nil {
		return nil, err
	}
	return utils.GetNonEmptyLines(out), nil
}

// s13ContainerLog returns the log of each start of the member's container
// that kubectl still keeps: the current one, and the previous one after a
// restart. complete is false when the container restarted more than once.
func s13ContainerLog(ctx context.Context, namespace, pod string) (log string, complete bool, err error) {
	out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", "pod", pod, "-o",
		`jsonpath={.status.containerStatuses[?(@.name=="cubrid")].restartCount}`))
	if err != nil {
		return "", false, err
	}
	restarts, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return "", false, fmt.Errorf("restart count of %s: %w", pod, err)
	}
	current, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "logs", pod, "-c", "cubrid"))
	if err != nil {
		return "", false, err
	}
	if restarts == 0 {
		return current, true, nil
	}
	previous, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "logs", pod, "-c", "cubrid",
		"--previous"))
	if err != nil {
		return "", false, err
	}
	return previous + current, restarts == 1, nil
}

// clusterDeletion is the fault of S13: the CubridCluster is deleted. It is
// confirmed when the CubridCluster can no longer be read; what it owned is
// the scenario's check. Nothing is put back afterwards.
type clusterDeletion struct {
	namespace, cluster string
}

func (f clusterDeletion) Name() string {
	return "delete CubridCluster " + f.namespace + "/" + f.cluster
}

func (f clusterDeletion) Inject(ctx context.Context) error {
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", f.namespace, "delete", "cubridcluster",
		f.cluster, "--wait=false"))
	return err
}

func (f clusterDeletion) Confirm(ctx context.Context) (bool, error) {
	out, err := utils.RunQuiet(exec.CommandContext(ctx, "kubectl", "-n", f.namespace, "get", "cubridcluster",
		f.cluster, "--ignore-not-found", "-o", "name"))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

func (f clusterDeletion) Remove(context.Context) error { return nil }

// s13Limits bounds the deletion: the CubridCluster has no finalizer, so it is
// gone at once, and the test waits s13DeleteWait for the rest, or longer when
// a set delete_limit would not fit in it.
func s13Limits(deleteLimit faults.Limit) faults.Limits {
	outcome := s13DeleteWait
	if deleteLimit.Set() && deleteLimit.Value >= outcome {
		outcome = deleteLimit.Value + time.Minute
	}
	return faults.Limits{Confirm: 2 * time.Minute, Outcome: outcome, Cleanup: time.Minute, Poll: 2 * time.Second}
}

// s13Record is what one variant of S13 records about itself in
// S13/<variant>/record.json.
type s13Record struct {
	Variant         string   `json:"variant"`
	Namespace       string   `json:"namespace"`
	RetentionPolicy string   `json:"retentionPolicy,omitempty"`
	PVCWhenDeleted  string   `json:"statefulSetPVCWhenDeleted,omitempty"`
	PVC             string   `json:"pvc"`
	PVCBefore       s13PVC   `json:"pvcBeforeDelete"`
	PVCAfterDelete  *s13PVC  `json:"pvcAfterDelete,omitempty"`
	PVCAfterCreate  *s13PVC  `json:"pvcAfterRecreate,omitempty"`
	PVReclaimPolicy string   `json:"pvReclaimPolicy,omitempty"`
	ObjectsBefore   []string `json:"objectsBeforeDelete"`
	LastRemaining   []string `json:"objectsLeftAtLastRead"`
	FirstPodUID     string   `json:"firstPodUID,omitempty"`
	RecreatedPodUID string   `json:"recreatedPodUID,omitempty"`
	FirstStart      string   `json:"firstStartLogVerdict,omitempty"`
	RecreatedStart  string   `json:"recreatedStartLogVerdict,omitempty"`
	CleanupProblems []string `json:"cleanupProblems"`
}

// s13Scenario registers S13 (deletion and retention) for a standalone cluster,
// one step per variant, each in a namespace of its own that it deletes at the
// end. The HA case, a three-member cluster created again over its retained
// PVCs, is not run here: its bootstrap fails over retained data (#798).
func s13Scenario() {
	Context("S13 deletion and retention of a standalone cluster with the real CUBRID image",
		Label("db", "S13"), Ordered, ContinueOnFailure, func() {
			BeforeAll(func() {
				if runtime.GOARCH != "amd64" {
					Skip("S13 needs linux/amd64: the official CUBRID image has no " + runtime.GOARCH +
						" build. Not run.")
				}
				ensureInstanceManagerImage()
			})

			AfterAll(func() {
				// Each step deletes its own namespace; this catches a step that
				// stopped before. Only these namespaces, and so only their PVCs,
				// are deleted.
				for _, variant := range s13Variants {
					_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", s13Namespace(variant),
						"--ignore-not-found", "--timeout=5m"))
				}
			})

			for _, variant := range s13Variants {
				It("S13/"+variant+": deleting the cluster removes what the Operator created and "+
					s13Expectation(variant),
					Label(evidence.KindLabels("S13", evidence.Requirement{ID: "S13", Variant: variant}.Label())...),
					func(ctx SpecContext) { s13Run(ctx, variant) })
			}
		})
}

// s13Namespace is the namespace of one variant.
func s13Namespace(variant string) string { return "cubrid-s13-" + variant }

// s13Expectation says what a variant expects of the PVC, for the step's name.
func s13Expectation(variant string) string {
	if variant == s13Retain {
		return "keeps the PVC, and a cluster created again with the same name serves the earlier row"
	}
	return "removes the PVC"
}

// s13Manifest is the standalone CubridCluster of a variant. The retain
// variant leaves retentionPolicy out, to use the default.
func s13Manifest(namespace, variant string) string {
	repository, tag, found := strings.Cut(instanceManagerImage, ":")
	ExpectWithOffset(1, found).To(BeTrue(), "instanceManagerImage needs a tag")
	retention := ""
	if variant == s13Delete {
		retention = "\n    retentionPolicy: Delete"
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
      size: 2Gi%s
`, s13Cluster, namespace, repository, tag, s13Database, retention)
}

// s13Run runs one variant of S13 and records its result, also when a check
// fails.
func s13Run(ctx context.Context, variant string) {
	ns := s13Namespace(variant)
	pod := s13Cluster + "-0"
	pvcName := "data-" + pod
	wantRetention := "Retain"
	if variant == s13Delete {
		wantRetention = "Delete"
	}
	record := &s13Record{Variant: variant, Namespace: ns, PVC: pvcName, LastRemaining: []string{},
		ObjectsBefore: []string{}, CleanupProblems: []string{}}
	result := evidence.Scenario{ID: "S13", Variant: variant, Result: evidence.Fail,
		Limits:       map[string]string{deleteLimit.Name: deleteLimit.String()},
		Measurements: map[string]any{"deleteSeconds": evidence.Unknown}}
	var steps []faults.Step
	step := func(kind, detail string) {
		steps = append(steps, faults.Step{T: time.Now().UTC(), Kind: kind, Detail: detail})
	}
	// failing names the step under way, for the reason of a check that fails
	// in it.
	failing := func(what string) {
		result.Reason = "the scenario failed while " + what + "; see the test output"
	}
	never := func(event string) {
		result.NeverEvents = append(result.NeverEvents, event)
	}
	// stop ends the step with its result: a blocked one is skipped, as
	// blockStep does, and a failed one fails.
	stop := func(r evidence.Result, reason string) {
		result.Result, result.Reason = r, reason
		if r == evidence.Blocked {
			Skip("blocked: " + reason)
		}
		Fail(reason)
	}
	kubectl := func(args ...string) (string, error) {
		return utils.Run(exec.CommandContext(ctx, "kubectl", append([]string{"-n", ns}, args...)...))
	}
	apply := func() {
		cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(s13Manifest(ns, variant))
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "could not create the CubridCluster")
	}
	waitReady := func() time.Duration {
		start := time.Now()
		EventuallyWithOffset(1, func(g Gomega) {
			out, err := kubectl("get", "pod", pod, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"))
		}, s13ReadyWait, 2*time.Second).Should(Succeed())
		EventuallyWithOffset(1, func(g Gomega) {
			out, err := kubectl("get", "cubridcluster", s13Cluster, "-o",
				`jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"))
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		return time.Since(start)
	}
	readRow := func(ctx context.Context) error {
		_, err := memberCSQL(ctx, ns, pod, s13Database, false).read(sqlcheck.ReadRowQuery,
			sqlcheck.RowCheck(s13Marker))
		return err
	}
	// The log of a container that ended a moment ago may not be served yet
	// (#851), so the read is retried for a bounded time.
	startVerdict := func(recreated bool) (evidence.Result, string) {
		var (
			log      string
			complete bool
		)
		EventuallyWithOffset(1, func() error {
			var err error
			log, complete, err = s13ContainerLog(ctx, ns, pod)
			return err
		}, 30*time.Second, 2*time.Second).Should(Succeed(), "could not read the member's log")
		return s13StartVerdict(log, complete, s13Database, recreated)
	}

	defer func() {
		ok := s13Cleanup(ns, pvcName, record)
		result.CleanupSucceeded = &ok
		if data, err := json.MarshalIndent(record, "", "  "); err == nil {
			addEvidence(&result, writeScenarioFile("S13/"+variant+"/record.json", append(data, '\n')))
		}
		if len(steps) > 0 {
			addEvidence(&result, writeTimeline("S13/"+variant+"/timeline.jsonl", steps))
		}
		recordScenario(result)
	}()
	logs := newScenarioLogs(evidenceDir(), "S13/"+variant, evidence.ClusterLogPlan(namespace, ns, s13Cluster,
		[]string{pod}, false), kubectlLogs{}, evidence.DefaultLogLimits(), runLogBudget)
	logs.keepState(evidenceDir(), evidence.ClusterStatePlan(namespace, ns, s13Cluster, []string{pod}),
		kubectlState{}, evidence.DefaultStateLimits(), runStateBudget)
	defer func() { logs.conclude(context.Background(), true, &result) }()

	failing("creating the standalone cluster")
	By("creating a namespace that enforces the restricted security policy")
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", "create", "ns", ns))
	Expect(err).NotTo(HaveOccurred())
	_, err = utils.Run(exec.CommandContext(ctx, "kubectl", "label", "--overwrite", "ns", ns,
		"pod-security.kubernetes.io/enforce=restricted"))
	Expect(err).NotTo(HaveOccurred())

	By("creating a standalone CubridCluster with retentionPolicy " + wantRetention)
	apply()
	step("cluster_created", "CubridCluster "+ns+"/"+s13Cluster+" applied")
	waitReady()
	step("cluster_ready", "Pod "+pod+" and the CubridCluster are Ready")

	failing("recording the starting state")
	By("checking the retention policy the cluster and its StatefulSet were given")
	record.RetentionPolicy, err = kubectl("get", "cubridcluster", s13Cluster, "-o",
		"jsonpath={.spec.storage.retentionPolicy}")
	Expect(err).NotTo(HaveOccurred())
	Expect(record.RetentionPolicy).To(Equal(wantRetention))
	record.PVCWhenDeleted, err = kubectl("get", "statefulset", s13Cluster, "-o",
		"jsonpath={.spec.persistentVolumeClaimRetentionPolicy.whenDeleted}")
	Expect(err).NotTo(HaveOccurred())
	Expect(record.PVCWhenDeleted).To(Equal(wantRetention))

	By("writing the marker row through csql")
	Expect(writeS00Row(csqlInPod(ctx, ns, pod, s13Database), s13Marker)).To(Succeed())
	step("row_written", "marker row "+s13Marker)

	By("recording the PVC, the first Pod and what the Operator created")
	record.PVCBefore, err = s13ReadPVC(ctx, ns, pvcName)
	Expect(err).NotTo(HaveOccurred())
	Expect(record.PVCBefore.Exists).To(BeTrue(), "the PVC %s does not exist", pvcName)
	Expect(record.PVCBefore.Phase).To(Equal("Bound"))
	record.FirstPodUID, err = kubectl("get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
	Expect(err).NotTo(HaveOccurred())
	for _, mode := range []string{"rw", "ro"} {
		_, err = kubectl("get", "deployment", s13Cluster+"-broker-"+mode)
		Expect(err).NotTo(HaveOccurred(), "the %s Broker Deployment does not exist", mode)
	}
	record.ObjectsBefore, err = s13Remaining(ctx, ns, false)
	Expect(err).NotTo(HaveOccurred())
	step("starting_state", fmt.Sprintf("PVC %s uid %s on %s; %d objects", pvcName, record.PVCBefore.UID,
		record.PVCBefore.Volume, len(record.ObjectsBefore)))
	if variant == s13Retain {
		firstResult, firstReason := startVerdict(false)
		record.FirstStart = strings.TrimSuffix(string(firstResult)+": "+firstReason, ": ")
		if firstResult != evidence.Pass {
			stop(firstResult, firstReason)
		}
	}

	By("keeping the logs the deleted Pod would take with it")
	logs.beforeFault(ctx)

	failing("deleting the cluster")
	By("deleting the CubridCluster and waiting until what the Operator created is gone")
	// pvcRemoved is the PVC's removal under Retain, once a read saw it.
	var pvcRemoved string
	outcome := faults.Run(ctx, faults.Scenario{
		Fault:  logs.observeFault(clusterDeletion{namespace: ns, cluster: s13Cluster}),
		Limits: s13Limits(deleteLimit),
		Before: readRow,
		Check: func(ctx context.Context) (bool, error) {
			if variant == s13Retain {
				pvc, err := s13ReadPVC(ctx, ns, pvcName)
				if err != nil {
					return false, nil
				}
				record.PVCAfterDelete = &pvc
				if err := s13RetainedPVCHeld(pvcName, record.PVCBefore, pvc); err != nil {
					pvcRemoved = err.Error()
					return false, err
				}
			}
			left, err := s13Remaining(ctx, ns, variant == s13Delete)
			if err != nil {
				return false, nil
			}
			record.LastRemaining = left
			return len(left) == 0, nil
		},
	})
	steps = append(steps, outcome.Timeline...)
	flow := outcome.Scenario("S13", variant, s13Limits(deleteLimit))
	maps.Copy(result.Limits, flow.Limits)
	result.FaultConfirmed = flow.FaultConfirmed
	var deleted time.Duration
	if !outcome.OutcomeAt.IsZero() {
		deleted = outcome.OutcomeAt.Sub(outcome.FaultIssuedAt)
		result.Measurements["deleteSeconds"] = deleted.Seconds()
	}
	if outcome.Result != evidence.Pass {
		reason := outcome.Reason
		if len(record.LastRemaining) > 0 {
			reason += "; still present at the last read: " + strings.Join(record.LastRemaining, ", ")
		}
		if pvcRemoved != "" {
			never(pvcRemoved)
		}
		stop(outcome.Result, reason)
	}

	if variant == s13Retain {
		failing("creating the cluster again over the retained PVC")
		By("checking that the retained PVC's volume still exists")
		record.PVReclaimPolicy, err = utils.Run(exec.CommandContext(ctx, "kubectl", "get", "pv",
			record.PVCBefore.Volume, "-o", "jsonpath={.spec.persistentVolumeReclaimPolicy}"))
		Expect(err).NotTo(HaveOccurred(), "the volume %s of the retained PVC is gone", record.PVCBefore.Volume)

		By("creating the CubridCluster again with the same name")
		recreatedAt := time.Now()
		apply()
		step("cluster_recreated", "CubridCluster "+ns+"/"+s13Cluster+" applied again")
		if err := InterceptGomegaFailure(func() { waitReady() }); err != nil {
			// A member that ran createdb over the retained volume may never
			// become Ready; its log says so, and that is the failure to report.
			if log, complete, lerr := s13ContainerLog(ctx, ns, pod); lerr == nil {
				if again, reason := s13StartVerdict(log, complete, s13Database, true); again == evidence.Fail &&
					strings.Contains(log, s13CreatedbLine(s13Database)) {
					never(reason)
					stop(again, reason)
				}
			}
			Fail("the recreated cluster did not become Ready: " + err.Error())
		}
		result.Measurements["recreatedToReadySeconds"] = time.Since(recreatedAt).Seconds()
		step("cluster_ready", "Pod "+pod+" and the recreated CubridCluster are Ready")
		record.RecreatedPodUID, err = kubectl("get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
		Expect(err).NotTo(HaveOccurred())

		By("checking that the recreated member uses the same PVC and volume")
		pvc, err := s13ReadPVC(ctx, ns, pvcName)
		Expect(err).NotTo(HaveOccurred())
		record.PVCAfterCreate = &pvc
		if err := s13RetainedPVCHeld(pvcName, record.PVCBefore, pvc); err != nil {
			never(err.Error())
			stop(evidence.Fail, err.Error())
		}

		By("checking in the member's log that the database was not created again")
		again, reason := startVerdict(true)
		record.RecreatedStart = strings.TrimSuffix(string(again)+": "+reason, ": ")
		if again != evidence.Pass {
			if again == evidence.Fail {
				never(reason)
			}
			stop(again, reason)
		}

		failing("reading the marker row from the recreated cluster")
		By("reading the marker row written before the deletion")
		Expect(readRow(ctx)).To(Succeed())
		step("row_read", "marker row "+s13Marker+" read from the recreated cluster")
	}

	// Every check held: the run is judged by delete_limit.
	v := faults.Judge(within(deleteLimit, deleted))
	maps.Copy(result.Limits, v.Limits)
	result.Result, result.Reason = v.Result, v.Reason
}

// s13Cleanup deletes the variant's cluster, its PVC and its namespace, and
// reports whether all of it succeeded; each problem is kept in the record.
// The PVC is deleted by name in the variant's own namespace, and only while
// it is the one recorded before the deletion; the namespace deletion removes
// anything else the variant left. Nothing outside that namespace is touched.
func s13Cleanup(ns, pvcName string, record *s13Record) bool {
	problem := func(what string, err error) {
		record.CleanupProblems = append(record.CleanupProblems, what+": "+err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", ns, "delete", "cubridcluster", s13Cluster,
		"--ignore-not-found", "--timeout=3m")); err != nil {
		problem("delete the CubridCluster", err)
	}
	if pvc, err := s13ReadPVC(ctx, ns, pvcName); err != nil {
		problem("read the PVC", err)
	} else if pvc.Exists && pvc.UID == record.PVCBefore.UID {
		if _, err := utils.Run(exec.CommandContext(ctx, "kubectl", "-n", ns, "delete", "pvc", pvcName,
			"--timeout=5m")); err != nil {
			problem("delete the PVC "+pvcName, err)
		}
	}
	if _, err := utils.Run(exec.CommandContext(ctx, "kubectl", "delete", "ns", ns, "--ignore-not-found",
		"--timeout=5m")); err != nil {
		problem("delete the namespace", err)
	}
	return len(record.CleanupProblems) == 0
}
