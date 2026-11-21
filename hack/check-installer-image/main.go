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

// Command check-installer-image checks which images an installer manifest
// makes the operator run: the manager container's image and the value of its
// --instance-manager-image flag (#124). The release dry run runs it on
// dist/install.yaml.
//
// Exit codes: 0 when both are the expected images, 3 when the manager sets no
// --instance-manager-image (so it falls back to its built-in default), 2 for
// invalid usage, and 1 for anything else that does not match or cannot be
// read.
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

const (
	exitMismatch   = 1
	exitUsage      = 2
	exitFlagAbsent = 3

	imageFlag = "instance-manager-image"
)

// errFlagAbsent means the manager container does not set the Instance Manager
// image flag, so the manager uses its built-in default.
var errFlagAbsent = errors.New("the manager sets no --" + imageFlag + "; it falls back to its built-in default")

// flagName returns the name of a command-line flag in the forms Go's flag
// package accepts (-name, --name, -name=value, --name=value), whether it has
// an inline value, and the value. ok is false for anything that is not a flag.
func flagName(arg string) (name, value string, inline, ok bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", "", false, false
	}
	s := arg[1:]
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" || s[0] == '-' || s[0] == '=' {
		return "", "", false, false
	}
	name, value, inline = strings.Cut(s, "=")
	return name, value, inline, true
}

// imageFlagValues returns every value the manager's arguments give the
// Instance Manager image flag, in the forms Go's flag package parses:
// -flag=v, --flag=v, -flag v and --flag v. Go's flag package stops at "--",
// so the flag after it would be silently ignored: that is an error, as is a
// flag with no value.
func imageFlagValues(args []string) ([]string, error) {
	var values []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			for _, rest := range args[i+1:] {
				if name, _, _, ok := flagName(rest); ok && name == imageFlag {
					return nil, errors.New("--" + imageFlag + " appears after \"--\", where the manager does not parse it")
				}
			}
			break
		}
		name, value, inline, ok := flagName(args[i])
		if !ok || name != imageFlag {
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return nil, errors.New("--" + imageFlag + " has no value")
			}
			i++
			value = args[i]
		}
		values = append(values, value)
	}
	return values, nil
}

// checkManager checks one manager container.
func checkManager(c map[string]any, managerImage, instanceManagerImage string) error {
	image, present, err := unstructured.NestedString(c, "image")
	switch {
	case err != nil:
		return fmt.Errorf("the manager container's image is unreadable: %w", err)
	case !present:
		return errors.New("the manager container has no image")
	case image != managerImage:
		return fmt.Errorf("the manager container runs %q, not the built manager image %q", image, managerImage)
	}
	command, _, err := unstructured.NestedStringSlice(c, "command")
	if err != nil {
		return fmt.Errorf("the manager container's command is not a list of strings: %w", err)
	}
	for _, part := range command {
		if name, _, _, ok := flagName(part); ok && name == imageFlag {
			return errors.New("--" + imageFlag + " is set in the manager's command; set it in args only")
		}
	}
	args, present, err := unstructured.NestedStringSlice(c, "args")
	switch {
	case err != nil:
		return fmt.Errorf("the manager container's args are not a list of strings: %w", err)
	case !present:
		return fmt.Errorf("the manager container has no args: %w", errFlagAbsent)
	}
	values, err := imageFlagValues(args)
	if err != nil {
		return err
	}
	switch {
	case len(values) == 0:
		return errFlagAbsent
	case len(values) > 1:
		return fmt.Errorf("the manager sets --%s %d times; want exactly one", imageFlag, len(values))
	case values[0] != instanceManagerImage:
		return fmt.Errorf("the manager uses Instance Manager image %q, not the built image %q",
			values[0], instanceManagerImage)
	}
	return nil
}

// verifyInstaller checks the manager container of the one controller-manager
// Deployment, not any occurrence of an image string in the manifest. Each
// document must be a single object: a kind: List is not unpacked, so a
// Deployment inside one is not found and the check fails closed.
func verifyInstaller(r io.Reader, managerImage, instanceManagerImage string) error {
	if managerImage == "" || instanceManagerImage == "" {
		return errors.New("both expected image references are required")
	}
	dec := yaml.NewYAMLOrJSONDecoder(r, 4096)
	found := 0
	var result error
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
		switch {
		case err != nil:
			return fmt.Errorf("the controller-manager containers are unreadable: %w", err)
		case !present:
			return errors.New("the controller-manager Deployment has no containers")
		}
		managers := 0
		for _, raw := range containers {
			c, ok := raw.(map[string]any)
			if !ok {
				return errors.New("the controller-manager Deployment has a container that is not an object")
			}
			name, _, err := unstructured.NestedString(c, "name")
			if err != nil {
				return fmt.Errorf("a container name is unreadable: %w", err)
			}
			if name != "manager" {
				continue
			}
			managers++
			result = checkManager(c, managerImage, instanceManagerImage)
		}
		if managers != 1 {
			return fmt.Errorf("the controller-manager Deployment has %d manager containers; want one", managers)
		}
	}
	if found != 1 {
		return fmt.Errorf("installer has %d controller-manager Deployments; want one", found)
	}
	return result
}

func main() {
	manifest := flag.String("manifest", "", "path to the generated multi-document installer YAML")
	manager := flag.String("manager", "", "expected manager image reference")
	instanceManager := flag.String("instance-manager", "", "expected Instance Manager image reference")
	flag.Parse()
	if *manifest == "" {
		fmt.Fprintln(os.Stderr, "installer manifest path is required")
		os.Exit(exitUsage)
	}
	f, err := os.Open(*manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitMismatch)
	}
	checkErr := verifyInstaller(f, *manager, *instanceManager)
	if closeErr := f.Close(); checkErr == nil {
		checkErr = closeErr
	}
	if checkErr != nil {
		fmt.Fprintln(os.Stderr, checkErr)
	}
	os.Exit(exitCode(checkErr))
}

// exitCode maps the result of verifyInstaller to the command's exit code.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errFlagAbsent):
		return exitFlagAbsent
	default:
		return exitMismatch
	}
}
