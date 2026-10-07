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

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podTemplateHashAnnotation holds the hash of the Pod template the operator
// last wrote into a StatefulSet or Deployment.
const podTemplateHashAnnotation = "database.cubrid.io/pod-template-hash"

// setPodTemplate writes the desired Pod template into live, the template of
// obj, unless live already carries it. The API server fills defaults into a
// template it stores (pull policy, DNS policy, probe timeouts and more), so
// the stored template never equals the desired one; replacing it on every
// reconcile would write the object every time. live carries the desired
// template when the desired template is the one last written (its hash is
// obj's annotation) and live still holds every value the desired template
// sets. The hash catches a field the desired template no longer sets, which
// the comparison alone would leave in place; the comparison catches a value
// changed by someone else. The comparison skips unset strings, slices, maps
// and pointers but not unset numbers, so the probes' numeric defaults are set
// in the desired template first.
func setPodTemplate(obj metav1.Object, live *corev1.PodTemplateSpec, desired corev1.PodTemplateSpec) error {
	for _, containers := range [][]corev1.Container{desired.Spec.InitContainers, desired.Spec.Containers} {
		for i := range containers {
			c := &containers[i]
			for _, probe := range []*corev1.Probe{c.StartupProbe, c.LivenessProbe, c.ReadinessProbe} {
				defaultProbe(probe)
			}
		}
	}
	raw, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("hash Pod template: %w", err)
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	annotations := obj.GetAnnotations()
	if annotations[podTemplateHashAnnotation] == hash && equality.Semantic.DeepDerivative(desired, *live) {
		return nil
	}
	*live = desired
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[podTemplateHashAnnotation] = hash
	obj.SetAnnotations(annotations)
	return nil
}

// defaultProbe sets the numbers of a probe the API server defaults when they
// are unset.
func defaultProbe(probe *corev1.Probe) {
	if probe == nil {
		return
	}
	if probe.TimeoutSeconds == 0 {
		probe.TimeoutSeconds = 1
	}
	if probe.PeriodSeconds == 0 {
		probe.PeriodSeconds = 10
	}
	if probe.SuccessThreshold == 0 {
		probe.SuccessThreshold = 1
	}
	if probe.FailureThreshold == 0 {
		probe.FailureThreshold = 3
	}
}
