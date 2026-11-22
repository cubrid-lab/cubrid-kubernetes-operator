// Copyright 2026.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	testManager  = "ghcr.io/cubrid-lab/cubrid-kubernetes-operator/manager:v0.1.0"
	testIM       = "ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:v0.1.0"
	noDeployment = "0 controller-manager Deployments"
	goodFlag     = "            - --instance-manager-image=" + testIM + "\n"
)

const deploymentHead = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: cubrid-kubernetes-operator-controller-manager
  labels:
    control-plane: controller-manager
spec:
  template:
    spec:
      containers:
        - name: sidecar
          image: example/other:1
          args:
            - --instance-manager-image=` + testIM + `
        - name: manager
          image: ` + testManager + "\n"

// withArgs is the controller-manager Deployment whose manager container has
// the given argument lines.
func withArgs(lines ...string) string {
	s := deploymentHead + "          args:\n            - --leader-elect\n"
	for _, l := range lines {
		s += "            - " + l + "\n"
	}
	return s
}

var goodDeployment = withArgs("--instance-manager-image=" + testIM)

// A reference in an unrelated object, annotation or container is not proof
// that the manager actually uses it. Every flag form Go's flag package parses
// counts, and anything ambiguous or unreadable fails closed.
func TestVerifyInstallerImageBinding(t *testing.T) {
	other := `apiVersion: v1
kind: ConfigMap
metadata:
  name: unrelated-image-reference
  annotations:
    image: ` + testIM + `
data:
  note: "` + testIM + `"
---
`
	change := func(old, replacement string) string {
		return strings.Replace(goodDeployment, old, replacement, 1)
	}
	deployment := func(kind, name, labels string) string {
		return `apiVersion: apps/v1
kind: ` + kind + `
metadata:
  name: ` + name + labels + `
spec:
  template:
    spec:
      containers:
        - name: manager
          image: ` + testManager + `
          args:
            - --instance-manager-image=` + testIM + "\n"
	}
	label := "\n  labels:\n    control-plane: controller-manager"
	cases := []struct {
		name     string
		manifest string
		want     string // "" for a valid installer
		absent   bool   // the error must be errFlagAbsent
	}{
		{name: "valid multi-document installer", manifest: other + goodDeployment},
		{name: "single dash with value", manifest: withArgs("-instance-manager-image=" + testIM)},
		{name: "double dash, separate value", manifest: withArgs("--instance-manager-image", testIM)},
		{name: "single dash, separate value", manifest: withArgs("-instance-manager-image", testIM)},
		{name: "reference only in unrelated YAML", manifest: other, want: "controller-manager Deployments"},
		{name: "no flag at all", manifest: other + withArgs(), want: "sets no", absent: true},
		{name: "no args", manifest: deploymentHead, want: "has no args", absent: true},
		{name: "args not strings", manifest: deploymentHead + "          args: [1]\n", want: "not a list of strings"},
		{name: "wrong Manager image", manifest: change(testManager, "example/wrong-manager:1"),
			want: "not the built manager image"},
		{name: "no Manager image", manifest: change("          image: "+testManager+"\n", ""),
			want: "has no image"},
		{name: "wrong Instance Manager image",
			manifest: withArgs("--instance-manager-image=example/wrong:1"), want: "not the built image"},
		{name: "wrong image, separate value",
			manifest: withArgs("-instance-manager-image", "example/wrong:1"), want: "not the built image"},
		{name: "two flags, same value", manifest: goodDeployment + goodFlag, want: "2 times"},
		{name: "a later flag in another form overrides",
			manifest: withArgs("--instance-manager-image="+testIM, "-instance-manager-image", "example/wrong:1"),
			want:     "2 times"},
		{name: "bare flag with no value", manifest: withArgs("--instance-manager-image"), want: "has no value"},
		{name: "flag after the terminator", manifest: withArgs("--", "--instance-manager-image="+testIM),
			want: "after \"--\""},
		{name: "flag in another form after the terminator",
			manifest: withArgs("--instance-manager-image="+testIM, "--", "-instance-manager-image", "x"),
			want:     "after \"--\""},
		// The manager container is the last one, so its args come last.
		{name: "flag in command", manifest: goodDeployment[:strings.LastIndex(goodDeployment, "          args:\n")] +
			"          command: [/manager, -instance-manager-image=example/wrong:1]\n" +
			goodDeployment[strings.LastIndex(goodDeployment, "          args:\n"):],
			want: "set in the manager's command"},
		{name: "a longer flag name is another flag",
			manifest: withArgs("--instance-manager-image-pull-policy=Always"), want: "sets no", absent: true},
		// Go's flag package gives a string flag the next argument as its value,
		// even when that argument looks like a flag, so the manager would not
		// see --instance-manager-image here.
		{name: "image flag consumed as another flag's value",
			manifest: withArgs("--metrics-bind-address", "--instance-manager-image="+testIM),
			want:     "not parsed as its own flag"},
		{name: "image flag consumed by a single-dash string flag",
			manifest: withArgs("-watch-namespace", "--instance-manager-image="+testIM),
			want:     "not parsed as its own flag"},
		{name: "string flag with an inline value, then the image flag",
			manifest: withArgs("--metrics-bind-address=:8080", "--instance-manager-image="+testIM)},
		{name: "boolean flag does not consume the image flag",
			manifest: withArgs("--metrics-secure", "--instance-manager-image="+testIM)},
		{name: "string flag with a separate value, then the image flag",
			manifest: withArgs("--metrics-bind-address", ":8080", "--instance-manager-image="+testIM)},
		{name: "invalid zap value is rejected like the manager does",
			manifest: withArgs("--zap-log-level=bogus", "--instance-manager-image="+testIM),
			want:     "not parsed as its own flag"},
		{name: "valid zap flags before the image flag",
			manifest: withArgs("--zap-log-level=debug", "--zap-devel", "--instance-manager-image="+testIM)},
		{name: "kubeconfig flag before the image flag",
			manifest: withArgs("--kubeconfig=/k", "--instance-manager-image="+testIM)},
		{name: "kubeconfig flag consumes the image flag",
			manifest: withArgs("--kubeconfig", "--instance-manager-image="+testIM),
			want:     "built-in default"},
		{name: "unknown flag fails closed",
			manifest: withArgs("--no-such-flag=1", "--instance-manager-image="+testIM),
			want:     "not parsed as its own flag"},
		{name: "positional argument before the image flag",
			manifest: withArgs("stray", "--instance-manager-image="+testIM), want: "not parsed as its own flag"},
		{name: "no manager container", manifest: strings.Replace(goodDeployment, "- name: manager", "- name: other", 1),
			want: "0 manager containers"},
		{name: "two manager containers", manifest: goodDeployment + `        - name: manager
          image: ` + testManager + `
          args:
            - --instance-manager-image=` + testIM + "\n", want: "2 manager containers"},
		{name: "no containers", manifest: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: x-controller-manager` + label + `
spec:
  template:
    spec:
      hostNetwork: false
`, want: "has no containers"},
		{name: "containers not a list", manifest: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: x-controller-manager` + label + `
spec:
  template:
    spec:
      containers: manager
`, want: "containers are unreadable"},
		{name: "two controller managers", manifest: goodDeployment + "---\n" + goodDeployment, want: "more than one"},
		{name: "right name without the label", manifest: deployment("Deployment", "x-controller-manager", ""),
			want: noDeployment},
		{name: "the label on another name", manifest: deployment("Deployment", "unrelated", label),
			want: noDeployment},
		{name: "another kind with the right name and label",
			manifest: deployment("StatefulSet", "x-controller-manager", label),
			want:     noDeployment},
		{name: "a Deployment inside a List is not unpacked", manifest: `apiVersion: v1
kind: List
items:
  - apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: x-controller-manager
      labels:
        control-plane: controller-manager
`, want: noDeployment},
		{name: "broken YAML", manifest: "kind: Deployment\nmetadata: [this isn't yaml\n", want: "decode installer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyInstaller(strings.NewReader(tc.manifest), testManager, testIM)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected a valid installer: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verifyInstaller error = %v; want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "<nil>") {
				t.Errorf("the error prints <nil>: %v", err)
			}
			if got := errors.Is(err, errFlagAbsent); got != tc.absent {
				t.Errorf("errors.Is(err, errFlagAbsent) = %t; want %t", got, tc.absent)
			}
		})
	}
}

// The workflow reads the exit code: 3 says the installer leaves the manager on
// its built-in default image, 1 any other mismatch.
func TestExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{errFlagAbsent, exitFlagAbsent},
		{errors.New("the manager container has no args: " + errFlagAbsent.Error()), exitMismatch},
		{errors.New("other"), exitMismatch},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("exitCode(%v) = %d; want %d", c.err, got, c.want)
		}
	}
}

// managerFlags is kept by hand, so it must name every flag cmd/main.go
// registers with the same kind; otherwise a new flag would be rejected as
// unknown, or a new string flag would not consume its value. The zap and
// --kubeconfig flags come from controller-runtime and are not listed. A flag
// registration this test cannot classify fails it, so a new form is never
// skipped silently.
func TestManagerFlagsMatchManagerMain(t *testing.T) {
	src, err := os.ReadFile("../../cmd/main.go")
	if err != nil {
		t.Fatal(err)
	}
	// Var and TextVar targets are not typed here; a consuming flag is a string.
	kinds := map[string]string{
		"String": "string", "Int": "int", "Bool": "bool", "Duration": "duration",
		"Func": "string", "BoolFunc": "bool", "Var": "string", "TextVar": "string",
	}
	// Calls that do not register a flag.
	notRegistration := map[string]bool{"Parse": true, "Parsed": true, "Args": true, "NArg": true, "Arg": true,
		"CommandLine": true}
	call := regexp.MustCompile(`\bflag\.([A-Z]\w*)`)
	// The name is the first argument, or the second after a target such as
	// &x or &cfg.X (the Var forms).
	named := regexp.MustCompile(
		`^flag\.(String|Int|Bool|Duration|Func|BoolFunc|Var|TextVar)(?:Var)?\(\s*(?:&?[\w.]+\s*,\s*)?"([^"]+)"`)
	found := map[string]string{}
	for _, loc := range call.FindAllStringSubmatchIndex(string(src), -1) {
		if notRegistration[string(src[loc[2]:loc[3]])] {
			continue
		}
		m := named.FindStringSubmatch(string(src[loc[0]:]))
		if m == nil {
			t.Fatalf("cmd/main.go has a flag registration this test cannot classify: %.60q", src[loc[0]:])
		}
		found[m[2]] = kinds[m[1]]
	}
	if len(found) == 0 {
		t.Fatal("found no flags in cmd/main.go")
	}
	for name, kind := range found {
		if managerFlags[name] != kind {
			t.Errorf("flag --%s is a %s flag in cmd/main.go but %q in managerFlags", name, kind, managerFlags[name])
		}
	}
	for name := range managerFlags {
		if _, ok := found[name]; !ok {
			t.Errorf("managerFlags lists --%s, which cmd/main.go does not register", name)
		}
	}
}
