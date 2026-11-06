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

package installed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// clusterLogs reads the logs of the run's cluster for a bundle: Pods through
// the API, and the fixed reads of test/evidence in a Pod the way a member's
// commands run, after the check that the namespace is still the run's own.
type clusterLogs struct {
	h *haRun
}

func (c clusterLogs) Pods(ctx context.Context, set evidence.LogPodSet) ([]evidence.LogPod, error) {
	selector, err := labels.Parse(set.Selector)
	if err != nil {
		return nil, err
	}
	list := &corev1.PodList{}
	if err := c.h.Client.List(ctx, list, client.InNamespace(set.Namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	pods := make([]evidence.LogPod, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		found := evidence.LogPod{Name: pod.Name, UID: string(pod.UID), Node: pod.Spec.NodeName}
		if status := containerStatus(pod, set.Container); status != nil {
			found.Restarts = status.RestartCount
		}
		pods = append(pods, found)
	}
	return pods, nil
}

func (c clusterLogs) PodLog(ctx context.Context, set evidence.LogPodSet, pod evidence.LogPod, previous bool,
	tailLines int64, w io.Writer) error {
	if c.h.podLogs == nil {
		return errors.New("no reader of Pod logs is available")
	}
	stream, err := c.h.podLogs(ctx, set.Namespace, pod.Name, &corev1.PodLogOptions{Container: set.Container,
		Previous: previous, Timestamps: true, TailLines: &tailLines})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	_, err = io.Copy(w, stream)
	return err
}

func (c clusterLogs) Exec(ctx context.Context, set evidence.LogPodSet, pod evidence.LogPod, command []string,
	w io.Writer) error {
	if err := c.h.guard(ctx); err != nil {
		return err
	}
	out, err := c.h.Command(ctx, slices.Concat([]string{"-n", set.Namespace, "exec", pod.Name, "-c", set.Container,
		"--"}, command)...)
	if _, werr := io.WriteString(w, out); err == nil {
		err = werr
	}
	return err
}

// failureLogNotes keeps the logs of a run that created its namespace and did
// not pass LAB01, and returns what a reader of the summary should know of them.
func (h *haRun) failureLogNotes(namespaced bool) []string {
	if !namespaced || h.labPassed {
		return nil
	}
	if err := h.keepFailureLogs(); err != nil {
		return []string{string(h.redactor.Redact([]byte(err.Error())))}
	}
	return nil
}

// keepFailureLogs stores a bundle of the Operator's, the members' and the
// Brokers' logs below LAB01's directory, for a run that did not pass LAB01 and
// whose namespace is about to be removed with them. LAB01 injects no fault, so
// a member's cubrid heartbeat status reads nothing a test is measuring. It
// takes the bundle's own bound in its own context: a run that stopped at its
// deadline still has its logs. It returns only why the bundle is incomplete;
// that never changes LAB01's result.
func (h *haRun) keepFailureLogs() error {
	limits := evidence.DefaultLogLimits()
	bundle := evidence.NewLogBundle(h.cfg.EvidenceDir, haScenarioID, h.redactor, clusterLogs{h: h},
		evidence.ClusterLogPlan(h.cfg.OperatorNamespace, h.cfg.Namespace, haClusterName, h.members, true), limits,
		evidence.NewLogBudget(limits.BundleBytes, limits.CheckpointTimeout))
	bundle.Checkpoint(context.Background(), evidence.LogsAfterVerdict, true)
	if file := bundle.IndexFile(); file != "" && !slices.Contains(h.files, file) {
		h.files = append(h.files, file)
	}
	if err := bundle.Problem(); err != nil {
		return fmt.Errorf("the diagnostic logs of %s are incomplete: %w", haScenarioID, err)
	}
	return nil
}
