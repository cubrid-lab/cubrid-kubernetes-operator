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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// instanceManagerToken is the token the suite gives the operator
// (e2e_test.go); the operator copies it to each cluster.
const instanceManagerToken = "e2e-wiring-token"

// curlPodManifest is a Pod outside the cluster's namespace from which the
// Instance Manager is called as any other workload on the Pod network would.
const curlPodManifest = `apiVersion: v1
kind: Pod
metadata:
  name: im-caller
  namespace: %s
spec:
  restartPolicy: Never
  containers:
    - name: curl
      image: docker.io/curlimages/curl:8.10.1
      command: ["sleep", "3600"]
`

// s14Call is one request to a member's Instance Manager.
type s14Call struct {
	method, path string
	// what the request would do if it were accepted
	effect string
}

// s14Step registers S14 (authentication and Secrets) of the scenario
// contract on the cluster the earlier steps formed.
func s14Step(r *haRun) {
	It("S14: the Instance Manager refuses a caller without the token, and the token is not shown anywhere", func() {
		result := evidence.Scenario{ID: "S14", Result: evidence.Fail,
			Reason: "a check of S14 failed; see the test output"}
		defer func() { r.later = append(r.later, result) }()
		redactor = evidence.NewRedactor(instanceManagerToken)

		By("recording the starting state")
		master, slaves, err := r.master()
		Expect(err).NotTo(HaveOccurred())
		membersBefore, err := r.memberUIDs()
		Expect(err).NotTo(HaveOccurred())

		By("starting a Pod to call from")
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(fmt.Sprintf(curlPodManifest, r.clientNamespace))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "-n", r.clientNamespace, "wait", "--for=condition=Ready",
			"pod/im-caller", "--timeout=3m"))
		Expect(err).NotTo(HaveOccurred())

		// call returns the HTTP status of one request to a member.
		call := func(member string, c s14Call, headers ...string) string {
			args := []string{"-n", r.clientNamespace, "exec", "im-caller", "--", "curl", "-s", "-o", "/dev/null",
				"-w", "%{http_code}", "--max-time", "20", "-X", c.method}
			for _, h := range headers {
				args = append(args, "-H", h)
			}
			if c.method == "POST" {
				args = append(args, "-H", "Content-Type: application/json", "-d", "{}")
			}
			args = append(args, fmt.Sprintf("http://%s.%s.svc:9090%s", member, r.namespace, c.path))
			out, err := utils.Run(exec.Command("kubectl", args...))
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "%s %s on %s", c.method, c.path, member)
			return strings.TrimSpace(out)
		}

		calls := []s14Call{
			{"GET", "/v1/role", "reads the member's role"},
			{"GET", "/v1/ha/status", "reads the member's view of the group"},
			{"POST", "/v1/backup", "starts a backup"},
			{"POST", "/v1/restore/prepare", "starts a restore"},
			{"POST", "/v1/ha/bootstrap", "creates a database"},
			{"POST", "/v1/shutdown?database=" + r.database, "stops the member's database"},
		}
		// Callers that must be refused. The last ones claim to come from the
		// Pod itself, which is the one caller that needs no token.
		refused := map[string][]string{
			"no token":                     nil,
			"a wrong token":                {"Authorization: Bearer not-the-token"},
			"the token without its scheme": {"Authorization: " + instanceManagerToken},
			"no token, X-Forwarded-For":    {"X-Forwarded-For: 127.0.0.1"},
			"no token, X-Real-IP":          {"X-Real-IP: 127.0.0.1"},
			"no token, Forwarded":          {"Forwarded: for=127.0.0.1"},
			"no token, Host localhost":     {"Host: localhost:9090"},
		}

		By("calling every protected endpoint of the master and of a slave without a valid token")
		requests := 0
		for _, member := range []string{master, slaves[0]} {
			for _, c := range calls {
				for who, headers := range refused {
					Expect(call(member, c, headers...)).To(Equal("401"),
						"%s %s on %s with %s (accepted, it %s)", c.method, c.path, member, who, c.effect)
					requests++
				}
			}
		}

		By("calling with the valid token")
		Expect(call(master, calls[0], "Authorization: Bearer "+instanceManagerToken)).To(Equal("200"))
		Expect(call(slaves[0], calls[0], "Authorization: Bearer "+instanceManagerToken)).To(Equal("200"))

		By("checking that the refused requests changed nothing")
		now, _, err := r.master()
		Expect(err).NotTo(HaveOccurred())
		Expect(now).To(Equal(master), "the master after the refused requests")
		membersAfter, err := r.memberUIDs()
		Expect(err).NotTo(HaveOccurred())
		Expect(membersAfter).To(Equal(membersBefore), "the DB Pods after the refused requests")
		for _, member := range r.members {
			restarts, err := r.kubectl("get", "pod", member, "-o", "jsonpath={.status.containerStatuses[0].restartCount}")
			Expect(err).NotTo(HaveOccurred())
			Expect(restarts).To(Equal("0"), "restarts of %s", member)
			operations, err := r.kubectl("exec", member, "--", "sh", "-c", "ls /var/lib/cubrid/operations 2>/dev/null | wc -l")
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "operation records on %s: %s\n", member, strings.TrimSpace(operations))
		}
		var report workload.Report
		Eventually(func(g Gomega) {
			var err error
			report, err = r.dataCheck()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("looking for the token where it must not be")
		places := map[string][]string{
			"the CubridCluster":           {"-n", r.namespace, "get", "cubridcluster", r.cluster, "-o", "yaml"},
			"the events of the namespace": {"-n", r.namespace, "get", "events", "-o", "yaml"},
			"the description of the Pods": {"-n", r.namespace, "describe", "pods"},
			"the ConfigMaps":              {"-n", r.namespace, "get", "configmaps", "-o", "yaml"},
			"the operator's log":          {"-n", namespace, "logs", "-l", operatorSelector, "--tail=-1"},
			"the events of the operator":  {"-n", namespace, "get", "events", "-o", "yaml"},
			"the operator's description":  {"-n", namespace, "describe", "pods"},
		}
		for _, member := range r.members {
			places["the log of "+member] = []string{"-n", r.namespace, "logs", member, "--tail=-1"}
		}
		for where, args := range places {
			out, err := utils.Run(exec.Command("kubectl", args...))
			Expect(err).NotTo(HaveOccurred(), where)
			Expect(strings.Contains(out, instanceManagerToken)).To(BeFalse(), "the token is shown in %s", where)
		}

		summary := fmt.Sprintf("refused requests: %d (6 endpoints, 7 kinds of caller, 2 members), all answered 401\n"+
			"requests with the valid token: 2, answered 200\nmaster before and after: %s\n"+
			"DB Pods restarted: 0\nplaces searched for the token: %d, found in: 0\n", requests, master, len(places))
		files := r.evidenceFiles("S14", report)
		if f := writeScenarioFile("S14/result.txt", []byte(summary)); f != "" {
			files = append(files, f)
		}
		result = evidence.Scenario{
			ID: "S14", Result: evidence.Pass,
			Measurements: map[string]any{
				"requestsRefused":     requests,
				"requestsAccepted":    2,
				"placesSearched":      len(places),
				"tokenFound":          0,
				"acknowledgedMissing": report.AcknowledgedMissing,
			},
			Evidence: files,
		}
	})
}
