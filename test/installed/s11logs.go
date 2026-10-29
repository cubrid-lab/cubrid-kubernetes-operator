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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// logSinceMargin is how far before a variant's start each log is read,
	// so that a node clock behind the run's does not cut off its first lines.
	logSinceMargin = time.Minute
	// The names of the logs in the record and in a credential's place.
	operatorLog        = "the installed Operator's manager log"
	instanceManagerLog = "the Instance Manager's log"
	// kindStatefulSet is the kind of a member Pod's controller.
	kindStatefulSet = "StatefulSet"
)

// maxLogBytes bounds what the run reads of one container instance's log. A
// log that holds more is truncated: its search is incomplete.
var maxLogBytes int64 = 16 << 20

// podLogReader opens a container's log through the API server (pods/log).
type podLogReader func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser,
	error)

// s11LogCheck is one container log the run searched for the object store's
// keys. Neither the log nor a key is recorded, only where a key was found.
type s11LogCheck struct {
	// Log names whose log it is: operatorLog or instanceManagerLog.
	Log       string `json:"log"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	UID       string `json:"uid,omitempty"`
	Container string `json:"container"`
	// SinceTime is where each read started: logSinceMargin before the
	// variant started.
	SinceTime    time.Time `json:"sinceTime"`
	RestartCount int32     `json:"restartCount"`
	// BytesRead and Truncated are of the current container instance, which
	// is read up to maxLogBytes; PreviousRead is set once the previous
	// instance, which a restart count above zero leaves, was read, with its
	// own bytes and truncation.
	BytesRead         int64 `json:"bytesRead"`
	Truncated         bool  `json:"truncated"`
	PreviousRead      bool  `json:"previousRead"`
	PreviousBytesRead int64 `json:"previousBytesRead"`
	PreviousTruncated bool  `json:"previousTruncated"`
	// Found is set when a key was found in what was read, Unchecked says what
	// of the log could not be searched, and why.
	Found     bool   `json:"found"`
	Unchecked string `json:"unchecked,omitempty"`
}

// logsNote is the record's note on what of the logs a variant searched.
func logsNote() string {
	return fmt.Sprintf("the installed Operator's manager log, in each manager Pod LAB01 recorded, and the "+
		"Instance Manager's log, in the Pod the backup ran on (status.targetInstance), were searched for the keys "+
		"through pods/log, each from %s before the variant started, at most %d bytes per container instance, and "+
		"with the previous instance of a container that restarted; what either logged before then, and the "+
		"Instance Managers of other Pods, were not searched", logSinceMargin, maxLogBytes)
}

// lastTarget is the last status.targetInstance the recorded phases name.
func lastTarget(phases []s11Phase) string {
	for _, p := range slices.Backward(phases) {
		if p.TargetInstance != "" {
			return p.TargetInstance
		}
	}
	return ""
}

// checkS11Logs searches for the object store's keys the installed manager's
// log in each manager Pod LAB01 recorded, by its UID, and the Instance
// Manager's log in target, the Pod the backup ran on, which the StatefulSet
// of cluster must control and which must not have been created after the
// backup was requested. The Instance Manager is the main process of the
// member's database container. Each log is read through pods/log from
// logSinceMargin before started, with the previous instance of a container
// that restarted. A key found is added to creds.FoundIn under the log's
// name, never the key. What could not be read, or was truncated, is
// returned: the search is incomplete.
func (h *haRun) checkS11Logs(ctx context.Context, cluster, target string, requested, started time.Time,
	creds *s11Credentials) error {
	if creds.Logs == nil {
		creds.Logs = []s11LogCheck{}
	}
	since := started.Add(-logSinceMargin).Truncate(time.Second)
	var errs []error
	if len(h.rec.OperatorAfter.Pods) == 0 {
		errs = append(errs, errors.New("no installed manager Pod was recorded, so the Operator's log was not searched"))
	}
	for _, m := range h.rec.OperatorAfter.Pods {
		errs = append(errs, h.searchLog(ctx, creds, s11LogCheck{Log: operatorLog, Namespace: h.cfg.OperatorNamespace,
			Pod: m.Pod, UID: m.UID, Container: managerContainer, SinceTime: since}, nil))
	}
	if target == "" {
		return errors.Join(append(errs, errors.New("the backup named no status.targetInstance, so no Instance "+
			"Manager's log was searched"))...)
	}
	errs = append(errs, h.searchLog(ctx, creds, s11LogCheck{Log: instanceManagerLog, Namespace: h.cfg.Namespace,
		Pod: target, Container: databaseContainer, SinceTime: since}, func(pod *corev1.Pod) error {
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != kindStatefulSet || owner.Name != cluster {
			return fmt.Errorf("the Pod is not controlled by the StatefulSet of %s", cluster)
		}
		if pod.CreationTimestamp.After(requested) {
			return fmt.Errorf("the Pod was created at %s, after the backup was requested at %s, so the log of the "+
				"Pod the backup ran on is gone", pod.CreationTimestamp.UTC().Format(time.RFC3339),
				requested.Format(time.RFC3339))
		}
		return nil
	}))
	return errors.Join(errs...)
}

// searchLog reads the Pod of check, refuses one whose UID is not the one
// check names or that accept refuses, and searches its container's log, and
// the previous instance's when the container restarted. check is added to
// creds.Logs as it ends.
func (h *haRun) searchLog(ctx context.Context, creds *s11Credentials, check s11LogCheck,
	accept func(*corev1.Pod) error) error {
	var unchecked []string
	defer func() {
		check.Unchecked = strings.Join(unchecked, "; ")
		creds.Logs = append(creds.Logs, check)
	}()
	incomplete := func() error {
		return fmt.Errorf("%s of container %s in Pod %s/%s was not fully searched: %s", check.Log, check.Container,
			check.Namespace, check.Pod, strings.Join(unchecked, "; "))
	}
	if h.podLogs == nil {
		unchecked = append(unchecked, "no reader of Pod logs is available")
		return incomplete()
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: check.Namespace, Name: check.Pod}, pod); err != nil {
		unchecked = append(unchecked, "read the Pod: "+err.Error())
		return incomplete()
	}
	if check.UID != "" && string(pod.UID) != check.UID {
		unchecked = append(unchecked, fmt.Sprintf("the Pod's UID is %s, not %s, the one recorded", pod.UID,
			check.UID))
		return incomplete()
	}
	check.UID = string(pod.UID)
	if accept != nil {
		if err := accept(pod); err != nil {
			unchecked = append(unchecked, err.Error())
			return incomplete()
		}
	}
	var status *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == check.Container {
			status = &pod.Status.ContainerStatuses[i]
		}
	}
	if status == nil {
		unchecked = append(unchecked, "the Pod reports no status of the container, so its restarts are not known")
		return incomplete()
	}
	check.RestartCount = status.RestartCount
	found := false
	n, truncated, holds, err := h.readLog(ctx, check, false)
	check.BytesRead, check.Truncated, found = n, truncated, holds
	switch {
	case err != nil:
		unchecked = append(unchecked, "read the log: "+err.Error())
	case truncated:
		unchecked = append(unchecked, fmt.Sprintf("the log holds more than %d bytes since %s", maxLogBytes,
			check.SinceTime.Format(time.RFC3339)))
	}
	if status.RestartCount > 0 {
		n, truncated, holds, err := h.readLog(ctx, check, true)
		check.PreviousRead, check.PreviousBytesRead, check.PreviousTruncated = err == nil, n, truncated
		found = found || holds
		switch {
		case err != nil:
			unchecked = append(unchecked, "read the previous instance's log: "+err.Error())
		case truncated:
			unchecked = append(unchecked, fmt.Sprintf("the previous instance's log holds more than %d bytes since %s",
				maxLogBytes, check.SinceTime.Format(time.RFC3339)))
		}
		// Only the previous instance can be read; one before it is gone.
		if last := status.LastTerminationState.Terminated; status.RestartCount > 1 &&
			(last == nil || last.StartedAt.After(check.SinceTime)) {
			unchecked = append(unchecked, fmt.Sprintf("the container restarted %d times, and an instance before the "+
				"previous one, which cannot be read, may have run since %s", status.RestartCount,
				check.SinceTime.Format(time.RFC3339)))
		}
	}
	if found {
		check.Found = true
		creds.FoundIn = append(creds.FoundIn, fmt.Sprintf("%s (container %s in Pod %s/%s)", check.Log,
			check.Container, check.Namespace, check.Pod))
	}
	if len(unchecked) > 0 {
		return incomplete()
	}
	return nil
}

// readLog reads the log of check's container, or of its previous instance,
// from check.SinceTime, at most maxLogBytes and one more to see a
// truncation, and searches what it read, also when the read failed part
// way. It returns the bytes read.
func (h *haRun) readLog(ctx context.Context, check s11LogCheck, previous bool) (int64, bool, bool, error) {
	since := metav1.NewTime(check.SinceTime)
	limit := maxLogBytes + 1
	r, err := h.podLogs(ctx, check.Namespace, check.Pod, &corev1.PodLogOptions{Container: check.Container,
		SinceTime: &since, LimitBytes: &limit, Previous: previous})
	if err != nil {
		return 0, false, false, err
	}
	defer func() { _ = r.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(r, limit))
	holds, err := h.holdsCredential(data)
	return int64(len(data)), int64(len(data)) > maxLogBytes, holds, errors.Join(readErr, err)
}
