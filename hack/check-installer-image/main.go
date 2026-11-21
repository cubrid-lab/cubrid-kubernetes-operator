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
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// verifyInstaller checks the effective manager container, not arbitrary
// occurrences of an image string in a multi-document Kubernetes manifest.
// Release-build images are recorded separately by digest; this check verifies
// which reference an installation would actually pass to its manager.
func verifyInstaller(r io.Reader, managerImage, instanceManagerImage string) error {
	if managerImage == "" || instanceManagerImage == "" {
		return errors.New("both expected image references are required")
	}
	dec := yaml.NewYAMLOrJSONDecoder(r, 4096)
	found := 0
	for {
		var obj unstructured.Unstructured
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decode installer manifest: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		// Kustomize prefixes the name; the controller-manager suffix and
		// control-plane label identify the one intended Deployment.
		if obj.GetAPIVersion() != "apps/v1" || obj.GetKind() != "Deployment" ||
			!strings.HasSuffix(obj.GetName(), "controller-manager") ||
			obj.GetLabels()["control-plane"] != "controller-manager" {
			continue
		}
		found++
		if found != 1 {
			return errors.New("installer contains more than one controller-manager Deployment")
		}
		containers, present, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		if err != nil || !present {
			return fmt.Errorf("controller-manager has no readable containers: %v", err)
		}
		managerCount := 0
		for _, raw := range containers {
			c, ok := raw.(map[string]interface{})
			if !ok {
				return errors.New("controller-manager contains an invalid container object")
			}
			name, _, err := unstructured.NestedString(c, "name")
			if err != nil {
				return fmt.Errorf("read container name: %w", err)
			}
			if name != "manager" {
				continue
			}
			managerCount++
			image, present, err := unstructured.NestedString(c, "image")
			if err != nil || !present || image != managerImage {
				return fmt.Errorf("manager image %q is not the built image %q (read error: %v)", image, managerImage, err)
			}
			args, present, err := unstructured.NestedStringSlice(c, "args")
			if err != nil || !present {
				return fmt.Errorf("manager arguments are unavailable: %v", err)
			}
			count := 0
			for _, arg := range args {
				if arg == "--instance-manager-image" {
					return errors.New("manager image flag has no inline value")
				}
				if strings.HasPrefix(arg, "--instance-manager-image=") {
					count++
					if arg != "--instance-manager-image="+instanceManagerImage {
						return fmt.Errorf("manager uses %q, not the built Instance Manager image", arg)
					}
				}
			}
			if count != 1 {
				return fmt.Errorf("manager has %d Instance Manager image arguments; want exactly one", count)
			}
		}
		if managerCount != 1 {
			return fmt.Errorf("controller-manager has %d manager containers; want one", managerCount)
		}
	}
	if found != 1 {
		return fmt.Errorf("installer has %d controller-manager Deployments; want one", found)
	}
	return nil
}

func main() {
	manifest := flag.String("manifest", "", "path to the generated multi-document installer YAML")
	manager := flag.String("manager", "", "expected manager image reference")
	instanceManager := flag.String("instance-manager", "", "expected Instance Manager image reference")
	flag.Parse()
	if *manifest == "" {
		fmt.Fprintln(os.Stderr, "installer manifest path is required")
		os.Exit(2)
	}
	f, err := os.Open(*manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	if err := verifyInstaller(f, *manager, *instanceManager); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
