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
	"encoding/base64"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

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

// imCallerPolicyManifest admits the Pods of the im-caller's namespace to the
// Instance Manager port of the cluster's DB Pods, in addition to the
// operator's own policy.
const imCallerPolicyManifest = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: s14-im-caller
  namespace: %s
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/instance: %s
      app.kubernetes.io/component: database
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: %s
      ports:
        - {port: 9090, protocol: TCP}
`

// connectProbe connects to the URL in $0 and prints the connect time, the
// HTTP status and curl's exit code. A connection a NetworkPolicy drops prints
// "0.000000 000 28": no connection, then the connect timeout. This assumes the
// network plugin drops denied packets, as Kind's kindnet does; a plugin that
// rejects them would make curl fail at once with another exit code.
const connectProbe = `curl -s -o /dev/null -w "%{time_connect} %{http_code}" ` +
	`--connect-timeout 5 --max-time 10 "$0"; echo " $?"`

// s14Call is one request to a member's Instance Manager.
type s14Call struct {
	method, path string
	// what the request would do if it were accepted
	effect string
}

// tokenPlaces are the kubectl commands whose output must never show an
// Instance Manager token, by what they show.
func (r *haRun) tokenPlaces() map[string][]string {
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
	return places
}

// s14Step registers S14 (authentication and Secrets) of the scenario
// contract on the cluster the earlier steps formed.
func s14Step(r *haRun) {
	const text = "S14: the Instance Manager refuses a caller without the token, and the token is not shown anywhere"
	It(text, Label("S14"), func() {
		r.enter(nil)
		result := evidence.Scenario{ID: "S14", Result: evidence.Fail,
			Reason: "a check of S14 failed; see the test output"}
		defer func() { recordScenario(result) }()

		// A member has a role before its Pod is Ready: CUBRID is started
		// before the Instance Manager listens. The step before this one
		// replaces a Pod, so wait for all of them.
		By("waiting for every member's Pod to be Ready")
		Eventually(func(g Gomega) {
			for _, member := range r.members {
				ready, err := r.kubectl("get", "pod", member,
					"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(Equal("True"), "Pod %s", member)
			}
		}, 5*time.Minute, 3*time.Second).Should(Succeed())

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

		By("reading the cluster's own Instance Manager token")
		encoded, err := r.kubectl("get", "secret", r.cluster+"-im-token", "-o", "jsonpath={.data.token}")
		Expect(err).NotTo(HaveOccurred())
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		Expect(err).NotTo(HaveOccurred())
		instanceManagerToken := string(decoded)
		redactor.Add(instanceManagerToken)
		// Asserted as a boolean, so that a failure does not print the token.
		Expect(regexp.MustCompile("^[0-9a-f]{64}$").MatchString(instanceManagerToken)).To(BeTrue(),
			"the cluster's token is not a generated 256-bit hex token")

		// call returns the HTTP status of one request to a member. The headers
		// go to curl on its standard input, so that a token never appears in a
		// command line the suite logs.
		call := func(member string, c s14Call, headers ...string) string {
			args := []string{"-n", r.clientNamespace, "exec", "-i", "im-caller", "--", "curl", "-s", "-o", "/dev/null",
				"-w", "%{http_code}", "--max-time", "20", "-X", c.method, "-H", "@-"}
			if c.method == "POST" {
				args = append(args, "-H", "Content-Type: application/json", "-d", "{}")
			}
			args = append(args, fmt.Sprintf("http://%s.%s.svc:9090%s", member, r.namespace, c.path))
			cmd := exec.Command("kubectl", args...)
			cmd.Stdin = strings.NewReader(strings.Join(headers, "\n") + "\n")
			out, err := utils.Run(cmd)
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

		// The cluster's NetworkPolicy drops connections from outside the
		// cluster to the DB Pods. Kind's network plugin enforces it; another
		// plugin may not.
		By("checking that a Pod in another namespace cannot connect to the Instance Manager or the database")
		dropped := 0
		for _, member := range []string{master, slaves[0]} {
			for _, port := range []int{9090, 1523} {
				out, err := utils.Run(exec.Command("kubectl", "-n", r.clientNamespace, "exec", "im-caller", "--",
					"sh", "-c", connectProbe, fmt.Sprintf("http://%s.%s.svc:%d/", member, r.namespace, port)))
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(out)).To(Equal("0.000000 000 28"),
					"port %d of %s: a connection timeout, never a connection", port, member)
				dropped++
			}
		}

		// The token is checked behind the policy too: a policy of the user's
		// that admits a Pod must not admit its requests.
		By("admitting the calling Pod's namespace to the Instance Manager port with a NetworkPolicy of its own")
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(fmt.Sprintf(imCallerPolicyManifest, r.namespace, r.cluster, r.clientNamespace))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "-n", r.namespace, "delete", "networkpolicy", "s14-im-caller",
				"--ignore-not-found"))
		})
		Eventually(func(g Gomega) {
			for _, member := range []string{master, slaves[0]} {
				out, err := utils.Run(exec.Command("kubectl", "-n", r.clientNamespace, "exec", "im-caller", "--",
					"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "5",
					fmt.Sprintf("http://%s.%s.svc:9090/v1/role", member, r.namespace)))
				g.Expect(err).NotTo(HaveOccurred(), "%s through the added policy", member)
				g.Expect(strings.TrimSpace(out)).To(Equal("401"), "%s through the added policy", member)
			}
		}, time.Minute, 3*time.Second).Should(Succeed())

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

		By("reading the refusals from the Instance Manager's own log")
		for _, member := range []string{master, slaves[0]} {
			log, err := r.kubectl("logs", member, "--tail=-1")
			Expect(err).NotTo(HaveOccurred())
			refusals := 0
			for _, line := range strings.Split(log, "\n") {
				if strings.Contains(line, `"event":"request_refused"`) && strings.Contains(line, `"component":"instance-manager"`) {
					refusals++
				}
			}
			Expect(refusals).To(BeNumerically(">=", len(calls)*len(refused)),
				"refused requests in the log of %s", member)
			Expect(log).To(ContainSubstring(`"reason":"NoToken"`), "log of %s", member)
			Expect(log).To(ContainSubstring(`"reason":"WrongToken"`), "log of %s", member)
		}

		By("looking for the token where it must not be")
		places := r.tokenPlaces()
		for where, args := range places {
			out, err := utils.Run(exec.Command("kubectl", args...))
			Expect(err).NotTo(HaveOccurred(), where)
			Expect(strings.Contains(out, instanceManagerToken)).To(BeFalse(), "the token is shown in %s", where)
		}

		// Worded so that the redactor, which removes what follows the word
		// "token" and a colon, leaves the figures.
		summary := fmt.Sprintf("refused requests: %d (6 endpoints, 7 kinds of caller, 2 members), all answered 401\n"+
			"accepted requests, with the valid credential: 2, answered 200\nmaster before and after: %s\n"+
			"DB Pods restarted: 0\nplaces searched: %d\nplaces that showed the credential: 0\n",
			requests, master, len(places))
		summary = fmt.Sprintf("connections from another namespace dropped: %d (ports 9090 and 1523, 2 members)\n",
			dropped) + summary
		files := r.evidenceFiles("S14", report)
		if f := writeScenarioFile("S14/result.txt", []byte(summary)); f != "" {
			files = append(files, f)
		}
		result = evidence.Scenario{
			ID: "S14", Result: evidence.Pass,
			Measurements: map[string]any{
				"connectionsDropped":  dropped,
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
