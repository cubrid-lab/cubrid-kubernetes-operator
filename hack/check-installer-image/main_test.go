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
	"strings"
	"testing"
)

const (
	testManager = "ghcr.io/cubrid-lab/cubrid-kubernetes-operator/manager:v0.1.0"
	testIM = "ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:v0.1.0"
)

const goodDeployment = `apiVersion: apps/v1
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
        - name: manager
          image: ghcr.io/cubrid-lab/cubrid-kubernetes-operator/manager:v0.1.0
          args:
            - --leader-elect
            - --instance-manager-image=ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:v0.1.0
`

// A reference in an unrelated object, annotation or container is not proof
// that the manager actually uses it. Duplicate or ambiguous flags fail closed.
func TestVerifyInstallerImageBinding(t *testing.T) {
	other := `apiVersion: v1
kind: ConfigMap
metadata:
  name: unrelated-image-reference
  annotations:
    image: ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:v0.1.0
data:
  note: "ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:v0.1.0"
---
`
	change := func(old, replacement string) string {
		return strings.Replace(goodDeployment, old, replacement, 1)
	}
	cases := []struct {
		name     string
		manifest string
		want     string
	}{
		{"valid multi-document installer", other + goodDeployment, ""},
		{"reference only in unrelated YAML", other, "controller-manager Deployments"},
		{"reference only in annotation", other + change(
			"            - --instance-manager-image="+testIM+"\n", ""), "0 Instance Manager image arguments"},
		{"wrong Manager image", change(testManager, "example/wrong-manager:1"), "manager image"},
		{"wrong Instance Manager image", change("--instance-manager-image="+testIM,
			"--instance-manager-image=example/wrong:1"), "not the built Instance Manager"},
		{"two exact flags", goodDeployment + "            - --instance-manager-image="+testIM+"\n", "2 Instance Manager"},
		{"two controller managers", goodDeployment + "---\n" + goodDeployment, "more than one"},
		{"reference on unrelated Deployment", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: unrelated
spec:
  template:
    spec:
      containers:
        - name: manager
          image: ` + testManager + `
          args:
            - --instance-manager-image=` + testIM + "\n", "controller-manager Deployments"},
		{"broken YAML", "kind: Deployment\nmetadata: [this isn't yaml\n", "decode installer"},
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
		})
	}
}
