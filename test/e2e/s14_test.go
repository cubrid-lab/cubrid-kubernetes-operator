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
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
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

// objectStorageSecretKey is the secret key of the HA cluster's object-storage
// Secret: unique to the run, so that S14 can look for it (#91). The mock
// object store does not check it.
var objectStorageSecretKey = func() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "e2e-objstore-" + hex.EncodeToString(b)
}()

// rbacCheck is one question to the API server about what a service account
// may do, and the answer the deployed RBAC must give.
type rbacCheck struct {
	as, namespace, verb, resource, subresource string
	allowed                                    bool
}

// rbacChecks are what the manager and the DB Pods' service account must and
// must not be allowed (#91): the manager's ClusterRole is config/rbac/role.yaml,
// and the DB Pods' service account is given nothing.
func (r *haRun) rbacChecks(dbServiceAccount string) []rbacCheck {
	manager := "system:serviceaccount:" + namespace + ":cubrid-kubernetes-operator-controller-manager"
	db := "system:serviceaccount:" + r.namespace + ":" + dbServiceAccount
	ns := r.namespace
	return []rbacCheck{
		{manager, ns, "get", "secrets", "", true},
		{manager, ns, "create", "statefulsets", "", true},
		{manager, ns, "delete", "pods", "", true},
		{manager, ns, "patch", "cubridclusters", "status", true},
		{manager, ns, "create", "networkpolicies", "", true},
		{manager, ns, "create", "pods", "", false},
		{manager, ns, "create", "pods", "exec", false},
		{manager, ns, "create", "pods", "attach", false},
		{manager, ns, "delete", "persistentvolumeclaims", "", false},
		{manager, ns, "delete", "secrets", "", false},
		{manager, "", "delete", "namespaces", "", false},
		{manager, "", "create", "clusterrolebindings", "", false},
		{manager, "", "escalate", "clusterroles", "", false},
		{manager, "", "bind", "clusterroles", "", false},
		{manager, ns, "impersonate", "serviceaccounts", "", false},
		{db, ns, "get", "secrets", "", false},
		{db, ns, "list", "pods", "", false},
		{db, ns, "get", "cubridclusters", "", false},
		{db, ns, "create", "pods", "exec", false},
	}
}

// canI asks the API server whether c.as may do what c names. kubectl answers
// "yes" with exit code 0 and "no" with exit code 1 on standard output;
// warnings go to standard error. Anything else is an error, never a "no".
// A check without a namespace is asked across all namespaces, which is also
// how kubectl asks for a cluster-scoped resource without warning.
func canI(c rbacCheck) (bool, error) {
	args := []string{"auth", "can-i", c.verb, c.resource, "--as", c.as}
	if c.subresource != "" {
		args = append(args, "--subresource", c.subresource)
	}
	if c.namespace != "" {
		args = append(args, "-n", c.namespace)
	} else {
		args = append(args, "--all-namespaces")
	}
	cmd := exec.Command("kubectl", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	answer := strings.TrimSpace(lines[len(lines)-1])
	var exit *exec.ExitError
	switch {
	case err == nil && answer == "yes":
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1 && (answer == "no" || strings.HasPrefix(answer, "no - ")):
		return false, nil
	default:
		return false, fmt.Errorf("kubectl %s: %v: %s %s", strings.Join(args, " "), err, out, stderr.String())
	}
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
				ready, err := r.kubectl(context.Background(), "get", "pod", member,
					"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(Equal("True"), "Pod %s", member)
			}
		}, 5*time.Minute, 3*time.Second).Should(Succeed())

		By("recording the starting state")
		master, slaves, err := r.master(context.Background())
		Expect(err).NotTo(HaveOccurred())
		membersBefore := r.identities(context.Background(), r.members)

		By("starting a Pod to call from")
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(fmt.Sprintf(curlPodManifest, r.clientNamespace))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "-n", r.clientNamespace, "wait", "--for=condition=Ready",
			"pod/im-caller", "--timeout=3m"))
		Expect(err).NotTo(HaveOccurred())

		By("reading the cluster's own Instance Manager token")
		encoded, err := r.kubectl(context.Background(),
			"get", "secret", r.cluster+"-im-token", "-o", "jsonpath={.data.token}")
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
		// Pod itself; that origin does not replace the token either.
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
		now, _, err := r.master(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(now).To(Equal(master), "the master after the refused requests")
		Expect(faults.CheckUnchanged(membersBefore, r.identities(context.Background(), r.members))).To(Succeed(),
			"the DB Pods and their containers after the refused requests")
		for _, member := range r.members {
			restarts, err := r.kubectl(context.Background(),
				"get", "pod", member, "-o", "jsonpath={.status.containerStatuses[0].restartCount}")
			Expect(err).NotTo(HaveOccurred())
			Expect(restarts).To(Equal("0"), "restarts of %s", member)
			operations, err := r.kubectl(context.Background(),
				"exec", member, "--", "sh", "-c", "ls /var/lib/cubrid/operations 2>/dev/null | wc -l")
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "operation records on %s: %s\n", member, strings.TrimSpace(operations))
		}
		var report workload.Report
		Eventually(func(g Gomega) {
			var err error
			report, err = r.dataCheck(context.Background())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(report.OK).To(BeTrue(), "data check: %+v", report)
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("reading the refusals from the Instance Manager's own log")
		for _, member := range []string{master, slaves[0]} {
			log, err := r.kubectl(context.Background(), "logs", member, "--tail=-1")
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

		By("looking for the token and the object-storage secret key where they must not be")
		places := r.tokenPlaces()
		for where, args := range places {
			out, err := utils.Run(exec.Command("kubectl", args...))
			Expect(err).NotTo(HaveOccurred(), where)
			Expect(strings.Contains(out, instanceManagerToken)).To(BeFalse(), "the token is shown in %s", where)
			Expect(strings.Contains(out, objectStorageSecretKey)).To(BeFalse(),
				"the object-storage secret key is shown in %s", where)
		}

		By("asking the API server what the manager and the DB Pods' service account may do")
		dbServiceAccount, err := r.kubectl(context.Background(), "get", "pod", master,
			"-o", "jsonpath={.spec.serviceAccountName}")
		Expect(err).NotTo(HaveOccurred())
		dbServiceAccount = strings.TrimSpace(dbServiceAccount)
		Expect(dbServiceAccount).NotTo(BeEmpty())
		checks := r.rbacChecks(dbServiceAccount)
		var granted strings.Builder
		for _, c := range checks {
			allowed, err := canI(c)
			Expect(err).NotTo(HaveOccurred())
			fmt.Fprintf(&granted, "%s %s %s/%s in %q: %t\n", c.as, c.verb, c.resource, c.subresource, c.namespace,
				allowed)
			Expect(allowed).To(Equal(c.allowed), "%s %s %s/%s in %q", c.as, c.verb, c.resource, c.subresource,
				c.namespace)
		}
		for _, as := range []string{checks[0].as, checks[len(checks)-1].as} {
			list, err := utils.Run(exec.Command("kubectl", "auth", "can-i", "--list", "-n", r.namespace, "--as", as))
			Expect(err).NotTo(HaveOccurred())
			fmt.Fprintf(&granted, "\n%s may, in %s:\n%s", as, r.namespace, list)
		}

		// Worded so that the redactor, which removes what follows the word
		// "token" and a colon, leaves the figures.
		summary := fmt.Sprintf("refused requests: %d (6 endpoints, 7 kinds of caller, 2 members), all answered 401\n"+
			"accepted requests, with the valid credential: 2, answered 200\nmaster before and after: %s\n"+
			"DB Pods restarted: 0\nplaces searched: %d\nplaces that showed the credential: 0\n",
			requests, master, len(places))
		summary = fmt.Sprintf("connections from another namespace dropped: %d (ports 9090 and 1523, 2 members)\n",
			dropped) + summary
		summary += fmt.Sprintf("places that showed the object-storage secret key: 0\n"+
			"RBAC answers checked: %d, all as config/rbac/role.yaml grants\n", len(checks))
		files := r.evidenceFiles("S14", report)
		if f := writeScenarioFile("S14/result.txt", []byte(summary)); f != "" {
			files = append(files, f)
		}
		if f := writeScenarioFile("S14/rbac.txt", []byte(granted.String())); f != "" {
			files = append(files, f)
		}
		result = evidence.Scenario{
			ID: "S14", Result: evidence.Pass,
			Measurements: map[string]any{
				"connectionsDropped":       dropped,
				"requestsRefused":          requests,
				"requestsAccepted":         2,
				"placesSearched":           len(places),
				"tokenFound":               0,
				"objectStorageSecretFound": 0,
				"rbacChecks":               len(checks),
				"acknowledgedMissing":      report.AcknowledgedMissing,
			},
			Evidence: files,
		}
	})
}
