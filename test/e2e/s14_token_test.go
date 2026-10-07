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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The variants of S14 that replace the Instance Manager token, in
// docs/testing/scenario-contract.md.
const (
	s14TokenRotation  = "token-rotation"
	s14TokenMigration = "token-migration"
)

var s14TokenVariants = []string{s14TokenRotation, s14TokenMigration}

const (
	// rotateIMTokenAnnotation on the CubridCluster asks for a new token.
	rotateIMTokenAnnotation = "database.cubrid.io/rotate-im-token"
	// imTokenRotatedAtAnnotation on <cluster>-im-token is when the current
	// token was written; a Secret without it is moved to a new token.
	imTokenRotatedAtAnnotation = "database.cubrid.io/im-token-rotated-at"
)

// overlapQuiet is how long, before any Pod is replaced, the operator must
// keep the primary resolved with the previous token. It spans several
// resyncs of the operator (10 s) and the age after which a role observation
// no longer counts (15 s).
const overlapQuiet = 60 * time.Second

// tokenCallerManifest is a Pod outside the cluster's namespace from which the
// Instance Manager is called with a given token.
const tokenCallerManifest = `apiVersion: v1
kind: Pod
metadata:
  name: token-caller
  namespace: %s
spec:
  restartPolicy: Never
  containers:
    - name: curl
      image: docker.io/curlimages/curl:8.10.1
      command: ["sleep", "3600"]
`

// imTokenSecret is what the test reads of <cluster>-im-token.
type imTokenSecret struct {
	current, previous, rotatedAt string
}

// imTokens reads the cluster's token Secret and registers its tokens with the
// evidence redactor. A token is only ever compared, never printed.
func (r *haRun) imTokens() (imTokenSecret, error) {
	out, err := r.kubectl("get", "secret", r.cluster+"-im-token", "-o", "json")
	if err != nil {
		return imTokenSecret{}, err
	}
	var secret struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Data map[string][]byte `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &secret); err != nil {
		return imTokenSecret{}, fmt.Errorf("the token Secret could not be read")
	}
	t := imTokenSecret{
		current:   string(secret.Data["token"]),
		previous:  string(secret.Data["previousToken"]),
		rotatedAt: secret.Metadata.Annotations[imTokenRotatedAtAnnotation],
	}
	redactor.Add(t.current, t.previous)
	return t, nil
}

// needsNoTokenOverlap is the order dependency of the token variants of S14:
// a rotation is deferred while the previous token is kept.
func (r *haRun) needsNoTokenOverlap(context.Context) error {
	t, err := r.imTokens()
	if err != nil {
		return err
	}
	if t.previous != "" {
		return fmt.Errorf("the token Secret still keeps a previous token: an overlap is open")
	}
	return nil
}

// tokenStatus returns the HTTP status of GET /v1/role on a member, sent from
// the token-caller Pod with the token. The header goes to curl on its
// standard input, so that the token never appears in a command line the suite
// logs.
func (r *haRun) tokenStatus(member, token string) (string, error) {
	cmd := exec.Command("kubectl", "-n", r.clientNamespace, "exec", "-i", "token-caller", "--",
		"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "20", "-H", "@-",
		fmt.Sprintf("http://%s.%s.svc:9090/v1/role", member, r.namespace))
	cmd.Stdin = strings.NewReader("Authorization: Bearer " + token + "\n")
	out, err := utils.Run(cmd)
	return strings.TrimSpace(out), err
}

// eventCount is how many times the operator recorded an Event with the
// reason for the cluster. Repeated Events are counted in one object.
func (r *haRun) eventCount(reason string) (int, error) {
	out, err := r.kubectl("get", "events", "--field-selector",
		"involvedObject.kind=CubridCluster,involvedObject.name="+r.cluster+",reason="+reason,
		"-o", `jsonpath={range .items[*]}{.count}{" "}{end}`)
	if err != nil {
		return 0, err
	}
	n := 0
	for f := range strings.FieldsSeq(out) {
		c, err := strconv.Atoi(f)
		if err != nil || c < 1 {
			c = 1
		}
		n += c
	}
	return n, nil
}

// refusals counts the requests a member's Instance Manager refused since its
// container started, from its own log.
func (r *haRun) refusals(member string) (int, error) {
	log, err := r.kubectl("logs", member, "--tail=-1")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, `"event":"request_refused"`) && strings.Contains(line, `"component":"instance-manager"`) {
			n++
		}
	}
	return n, nil
}

// primaryResolved returns the status, the reason and the last transition time
// of PrimaryResolved, and status.currentPrimary.
func (r *haRun) primaryResolved() (status, reason, since, primary string, err error) {
	out, err := r.kubectl("get", "cubridcluster", r.cluster, "-o",
		`jsonpath={.status.conditions[?(@.type=="PrimaryResolved")].status} `+
			`{.status.conditions[?(@.type=="PrimaryResolved")].reason} `+
			`{.status.conditions[?(@.type=="PrimaryResolved")].lastTransitionTime} {.status.currentPrimary}`)
	if err != nil {
		return "", "", "", "", err
	}
	f := strings.Fields(out)
	if len(f) != 4 {
		return "", "", "", "", fmt.Errorf("PrimaryResolved and currentPrimary: %q", out)
	}
	return f[0], f[1], f[2], f[3], nil
}

// s14TokenStep registers one token variant of S14 (authentication and
// Secrets) of the scenario contract. Each replaces every member's Pod and
// moves the master, so the variants run after the steps that depend on which
// member is the master.
func s14TokenStep(r *haRun, variant string) {
	labels := Label(evidence.Requirement{ID: "S14", Variant: variant}.Label())
	switch variant {
	case s14TokenRotation:
		It("S14/"+variant+": the operator reaches every member through an Instance Manager token rotation,"+
			" and the old token is refused once every member was replaced", labels, func() {
			r.enter(r.needsNoTokenOverlap)
			r.s14Token(variant)
		})
	case s14TokenMigration:
		It("S14/"+variant+": a token Secret without a rotation time is moved to a new token"+
			" without a member refusing the operator", labels, func() {
			r.enter(r.needsNoTokenOverlap)
			r.s14Token(variant)
		})
	}
}

func (r *haRun) s14Token(variant string) {
	result := evidence.Scenario{ID: "S14", Variant: variant, Result: evidence.Fail,
		Reason: "a check of S14/" + variant + " failed; see the test output"}
	defer func() { recordScenario(result) }()
	var summary strings.Builder
	note := func(format string, args ...any) {
		_, _ = fmt.Fprintf(&summary, format+"\n", args...)
	}

	By("starting a Pod to call the Instance Manager from, and admitting its namespace to the port")
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(tokenCallerManifest, r.clientNamespace))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		_, _ = utils.Run(exec.Command("kubectl", "-n", r.clientNamespace, "delete", "pod", "token-caller",
			"--ignore-not-found", "--wait=false"))
	})
	cmd = exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(imCallerPolicyManifest, r.namespace, r.cluster, r.clientNamespace))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		_, _ = utils.Run(exec.Command("kubectl", "-n", r.namespace, "delete", "networkpolicy", "s14-im-caller",
			"--ignore-not-found"))
	})
	_, err = utils.Run(exec.Command("kubectl", "-n", r.clientNamespace, "wait", "--for=condition=Ready",
		"pod/token-caller", "--timeout=3m"))
	Expect(err).NotTo(HaveOccurred())

	// expectStatus checks, asserted on the result alone so that a failure
	// does not print a token, that a member answers a token with the status.
	expectStatus := func(g Gomega, member, token, which, want string) {
		got, err := r.tokenStatus(member, token)
		g.Expect(err).NotTo(HaveOccurred(), "%s with the %s token", member, which)
		g.Expect(got).To(Equal(want), "%s with the %s token", member, which)
	}

	By("recording the starting state")
	before, err := r.imTokens()
	Expect(err).NotTo(HaveOccurred())
	Expect(before.current != "" && before.previous == "").To(BeTrue(), "a token and no previous one")
	master, slaves, err := r.master()
	Expect(err).NotTo(HaveOccurred())
	status, _, since, primary, err := r.primaryResolved()
	Expect(err).NotTo(HaveOccurred())
	Expect(status).To(Equal("True"))
	Expect(primary).To(Equal(master))
	counts := map[string]int{}
	for _, reason := range []string{"InstanceManagerTokenRotated", "InstanceManagerTokenOverlapEnded",
		"InstanceManagerTokenRegenerated", "InstanceManagerTokenRefused", "PrimaryUnresolved"} {
		counts[reason], err = r.eventCount(reason)
		Expect(err).NotTo(HaveOccurred())
	}
	Eventually(func(g Gomega) {
		for _, member := range r.members {
			expectStatus(g, member, before.current, "current", "200")
		}
	}, time.Minute, 3*time.Second).Should(Succeed())

	switch variant {
	case s14TokenRotation:
		By("requesting a rotation with the annotation on the CubridCluster")
		_, err = r.kubectl("annotate", "--overwrite", "cubridcluster", r.cluster,
			fmt.Sprintf("%s=e2e-%d", rotateIMTokenAnnotation, time.Now().Unix()))
		Expect(err).NotTo(HaveOccurred())
		note("rotation requested with the annotation %s on the CubridCluster", rotateIMTokenAnnotation)
	case s14TokenMigration:
		// An operator before #326 left a Secret whose token every member was
		// started with, and no rotation time. Every member holds this
		// Secret's token, so removing the rotation time is all that differs.
		By("removing the rotation time from the token Secret, as an operator before #326 left it")
		_, err = r.kubectl("annotate", "secret", r.cluster+"-im-token", imTokenRotatedAtAnnotation+"-")
		Expect(err).NotTo(HaveOccurred())
		note("rotation time removed from the token Secret, whose token every member holds")
	}
	requestedAt := time.Now()

	By("waiting for the operator to write a new token and keep the one the members hold")
	var during imTokenSecret
	Eventually(func(g Gomega) {
		t, err := r.imTokens()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(t.previous == before.current).To(BeTrue(), "the previous token is the one the members hold")
		g.Expect(t.current != "" && t.current != before.current).To(BeTrue(), "a new current token")
		g.Expect(t.rotatedAt).NotTo(BeEmpty(), "the rotation time")
		during = t
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	Expect(regexp.MustCompile("^[0-9a-f]{64}$").MatchString(during.current)).To(BeTrue(),
		"the new token is not a generated 256-bit hex token")
	Eventually(func(g Gomega) {
		n, err := r.eventCount("InstanceManagerTokenRotated")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(n).To(BeNumerically(">", counts["InstanceManagerTokenRotated"]))
	}, time.Minute, 3*time.Second).Should(Succeed())
	note("new token written, previous token kept, Event InstanceManagerTokenRotated recorded")

	By("checking that every member still holds the previous token")
	for _, member := range r.members {
		expectStatus(Default, member, during.previous, "previous", "200")
		expectStatus(Default, member, during.current, "new", "401")
	}

	By("checking that, before any Pod is replaced, the operator keeps the primary resolved")
	refusalsAt := func() map[string]int {
		n := map[string]int{}
		for _, member := range r.members {
			c, err := r.refusals(member)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			n[member] = c
		}
		return n
	}
	resolved := func(g Gomega) {
		s, reason, at, p, err := r.primaryResolved()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(s + " " + reason).To(Equal("True SinglePrimaryObserved"))
		g.Expect(p).To(Equal(master))
		g.Expect(at).To(Equal(since), "PrimaryResolved changed since the rotation")
	}
	refusedFirst := refusalsAt()
	Consistently(resolved, overlapQuiet/3, 3*time.Second).Should(Succeed())
	refusedSettled := refusalsAt()
	Consistently(resolved, overlapQuiet-overlapQuiet/3, 3*time.Second).Should(Succeed())
	refusedLast := refusalsAt()
	settling, later := 0, 0
	for _, member := range r.members {
		settling += refusedSettled[member] - refusedFirst[member]
		later += refusedLast[member] - refusedSettled[member]
	}
	// The operator calls a member with the token it accepted last, so a
	// member that holds the previous token is not sent the new one again.
	Expect(later).To(BeZero(), "requests the members refused after the operator had found the token each holds")
	t, err := r.imTokens()
	Expect(err).NotTo(HaveOccurred())
	Expect(t.previous == during.previous).To(BeTrue(), "the overlap ended although no member was replaced")
	note("before any Pod was replaced: PrimaryResolved stayed True with %s for %s, master unchanged", master, overlapQuiet)
	note("requests refused by the members: %d in the first %s, %d after", settling, overlapQuiet/3, later)

	order := append(slices.Clone(slaves), master)
	replaced := 0
	for i, member := range order {
		By(fmt.Sprintf("replacing %s, %d of %d, and waiting until it is Ready", member, i+1, len(order)))
		uid, err := r.kubectl("get", "pod", member, "-o", "jsonpath={.metadata.uid}")
		Expect(err).NotTo(HaveOccurred())
		_, err = r.kubectl("delete", "pod", member, "--wait=false")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			out, err := r.kubectl("get", "pod", member, "-o",
				`jsonpath={.metadata.uid} {.status.conditions[?(@.type=="Ready")].status}{.metadata.deletionTimestamp}`)
			g.Expect(err).NotTo(HaveOccurred())
			f := strings.Fields(out)
			g.Expect(f).To(HaveLen(2), "Pod %s: %q", member, out)
			g.Expect(f[0]).NotTo(Equal(uid), "Pod %s was not replaced yet", member)
			g.Expect(f[1]).To(Equal("True"), "Pod %s is not Ready", member)
		}, 10*time.Minute, 3*time.Second).Should(Succeed())
		var now string
		Eventually(func(g Gomega) {
			m, _, err := r.master()
			g.Expect(err).NotTo(HaveOccurred())
			now = m
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
		replaced++

		By("checking that the replaced member holds the new token and the operator resolves the primary")
		Eventually(func(g Gomega) {
			expectStatus(g, member, during.current, "new", "200")
			expectStatus(g, member, during.previous, "previous", "401")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			s, reason, _, p, err := r.primaryResolved()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(s + " " + reason).To(Equal("True SinglePrimaryObserved"))
			g.Expect(p).To(Equal(now))
		}, 3*time.Minute, 3*time.Second).Should(Succeed())
		if i == len(order)-1 {
			note("replaced %s, the master, last: the master is now %s", member, now)
			break
		}
		for _, other := range order[i+1:] {
			expectStatus(Default, other, during.previous, "previous", "200")
		}
		t, err := r.imTokens()
		Expect(err).NotTo(HaveOccurred())
		Expect(t.previous == during.previous && t.current == during.current).To(BeTrue(),
			"the overlap ended while a member held the previous token")
		note("replaced %s: it holds the new token, the others the previous one; the overlap is kept", member)
	}

	By("waiting for the overlap to end")
	Eventually(func(g Gomega) {
		t, err := r.imTokens()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(t.current == during.current).To(BeTrue(), "the current token changed")
		g.Expect(t.previous == "").To(BeTrue(), "the previous token is still kept")
	}, 5*time.Minute, 3*time.Second).Should(Succeed())
	overlap := time.Since(requestedAt)
	Eventually(func(g Gomega) {
		n, err := r.eventCount("InstanceManagerTokenOverlapEnded")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(n).To(BeNumerically(">", counts["InstanceManagerTokenOverlapEnded"]))
	}, time.Minute, 3*time.Second).Should(Succeed())
	note("previous token dropped and Event InstanceManagerTokenOverlapEnded recorded, %s after the request",
		overlap.Round(time.Second))

	By("checking that every member refuses the old token and accepts the new one")
	for _, member := range r.members {
		expectStatus(Default, member, during.previous, "old", "401")
		expectStatus(Default, member, during.current, "new", "200")
	}
	note("old token refused (401) by %d members, new token accepted (200) by %d members", len(r.members), len(r.members))

	By("checking that no member was reported as refusing the operator")
	for _, reason := range []string{"InstanceManagerTokenRegenerated", "InstanceManagerTokenRefused"} {
		n, err := r.eventCount(reason)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(counts[reason]), "Events %s", reason)
	}
	unresolved, err := r.eventCount("PrimaryUnresolved")
	Expect(err).NotTo(HaveOccurred())
	note("Events InstanceManagerTokenRegenerated and InstanceManagerTokenRefused: 0")
	note("Events PrimaryUnresolved while Pods were replaced: %d", unresolved-counts["PrimaryUnresolved"])

	By("applying the data rules to every member and to both Services")
	var report workload.Report
	Eventually(func(g Gomega) {
		var err error
		report, err = r.dataCheck()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
	}, 3*time.Minute, 3*time.Second).Should(Succeed())

	By("looking for the tokens where they must not be")
	places := r.tokenPlaces()
	for where, args := range places {
		out, err := utils.Run(exec.Command("kubectl", args...))
		Expect(err).NotTo(HaveOccurred(), where)
		Expect(strings.Contains(out, during.previous)).To(BeFalse(), "the old token is shown in %s", where)
		Expect(strings.Contains(out, during.current)).To(BeFalse(), "the new token is shown in %s", where)
	}
	note("places searched for both tokens: %d, places that showed one: 0", len(places))

	files := r.evidenceFiles("S14/"+variant, report)
	if f := writeScenarioFile("S14/"+variant+"/result.txt", []byte(summary.String())); f != "" {
		files = append(files, f)
	}
	result = evidence.Scenario{
		ID: "S14", Variant: variant, Result: evidence.Pass,
		Measurements: map[string]any{
			"membersReplaced":            replaced,
			"overlapEndedAfter":          overlap.Round(time.Second).String(),
			"requestsRefusedWhileSettle": settling,
			"requestsRefusedAfterSettle": later,
			"oldTokenRefusedBy":          len(r.members),
			"newTokenAcceptedBy":         len(r.members),
			"tokenRefusedEvents":         0,
			"placesSearched":             len(places),
			"tokenFound":                 0,
			"acknowledgedMissing":        report.AcknowledgedMissing,
		},
		Evidence: files,
	}
}
