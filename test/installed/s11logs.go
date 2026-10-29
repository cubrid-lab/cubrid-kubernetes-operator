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

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	// logSinceMargin is how far before a variant's start the log of a Pod
	// that existed before it is read, so that a node clock behind the run's
	// does not cut off its first lines. A larger skew is not measured.
	logSinceMargin = time.Minute
	// The names of the logs in the record and in a credential's place.
	operatorLog        = "the installed Operator's manager log"
	instanceManagerLog = "the Instance Manager's log"
	// kindStatefulSet is the kind of a member Pod's controller.
	kindStatefulSet = "StatefulSet"
	// defaultMaxLogBytes bounds what the run reads of one container
	// instance's log, unless Runner.maxLogBytes is set. A log the API
	// returns more of is truncated: its search is incomplete.
	defaultMaxLogBytes int64 = 16 << 20
	// How a log was read (s11LogCheck.ReadFrom): from SinceTime, for a Pod
	// that existed before the variant started, or whole, for a Pod of a
	// cluster the variant created, all of whose log is in scope.
	logFromSinceTime = "sinceTime"
	logWhole         = "whole-log"
)

// podLogReader opens a container's log through the API server (pods/log).
type podLogReader func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser,
	error)

// logLimit is the bound on what the run reads of one container instance's
// log.
func (r *Runner) logLimit() int64 {
	if r.maxLogBytes > 0 {
		return r.maxLogBytes
	}
	return defaultMaxLogBytes
}

// redactError is err's text without any credential the redactor knows.
func (h *haRun) redactError(err error) string {
	if h.redactor == nil {
		return err.Error()
	}
	return string(h.redactor.Redact([]byte(err.Error())))
}

// s11LogCheck is one container log the run searched for the object store's
// keys. Neither the log nor a key is recorded, only where a key was found.
type s11LogCheck struct {
	// Log names whose log it is: operatorLog or instanceManagerLog.
	Log       string `json:"log"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	UID       string `json:"uid,omitempty"`
	Container string `json:"container"`
	// ReadFrom is logFromSinceTime, with SinceTime logSinceMargin before the
	// variant started, or logWhole, without a SinceTime.
	ReadFrom  string    `json:"readFrom"`
	SinceTime time.Time `json:"sinceTime,omitzero"`
	// RestartCount and ContainerID are the container's before the reads. The
	// Pod is read again after them; a change leaves the log unsearched.
	RestartCount int32  `json:"restartCount"`
	ContainerID  string `json:"containerID,omitempty"`
	// BytesRead and Truncated are of the current container instance, which
	// is read up to the run's log limit; PreviousRead is set once the
	// previous instance, which a restart count above zero leaves, was read,
	// with its own bytes and truncation.
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

// logsNote is the record's note on what of the logs a variant searched,
// with limit the bound on one container instance's log.
func logsNote(limit int64) string {
	return fmt.Sprintf("the installed Operator's manager log, in each manager Pod LAB01 recorded, and the "+
		"Instance Manager's log, in the Pod the backup ran on (status.targetInstance), were searched for the keys "+
		"through pods/log, at most %d bytes per container instance, with the previous instance of a container "+
		"that restarted; the log of a Pod that existed before the variant started was read from %s before it "+
		"(readFrom sinceTime), and a node clock more than that behind the run's is not measured; the log of a "+
		"Pod of a cluster the variant created was read whole (readFrom whole-log); pods/log returns only the "+
		"container's current log file, so what the kubelet rotated away within that window was not searched "+
		"and is not reported as truncated, and the bound applies only to what the API returned; what the "+
		"Operator logged before the window, and the Instance Managers of other Pods, were not searched",
		limit, logSinceMargin)
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
// log in each manager Pod of managers, by its UID: those LAB01 recorded,
// unless the variant replaced the manager, and the Instance
// Manager's log in target, the Pod the backup ran on, which the StatefulSet
// of cluster must control and which must not have been created after the
// backup was requested. The Instance Manager is a process of the member's
// database container. The manager's log is read from logSinceMargin before
// started; the member's too, unless the variant created cluster (created),
// when it is read whole. The previous instance of a container that
// restarted is read the same way. A key found is added to creds.FoundIn
// under the log's name, never the key. What could not be read, or was
// truncated, is returned: the search is incomplete.
// requestedOnServer is when the API server created backup, by its own clock,
// which also sets a Pod's creationTimestamp; the runner's clock would let a
// Pod replaced just after the request pass for the one the backup ran on.
// fallback, the runner's time of the request, is used only for an object
// without a creationTimestamp, which an API server never returns.
func requestedOnServer(backup *databasev1alpha1.CubridBackup, fallback time.Time) time.Time {
	if backup == nil || backup.CreationTimestamp.IsZero() {
		return fallback
	}
	return backup.CreationTimestamp.Time
}

// createdBefore reports whether a Pod created at created existed before the
// request at requested. A creationTimestamp is to the second: one in the
// request's second counts as after it.
func createdBefore(created, requested time.Time) bool {
	return created.Before(requested.Truncate(time.Second))
}

func (h *haRun) checkS11Logs(ctx context.Context, cluster string, created bool, target string, requested,
	started time.Time, creds *s11Credentials, managers []imageIdentity) error {
	if creds.Logs == nil {
		creds.Logs = []s11LogCheck{}
	}
	since := started.Add(-logSinceMargin).Truncate(time.Second)
	var errs []error
	if len(managers) == 0 {
		errs = append(errs, errors.New("no installed manager Pod was recorded, so the Operator's log was not searched"))
	}
	for _, m := range managers {
		errs = append(errs, h.searchLog(ctx, creds, s11LogCheck{Log: operatorLog, Namespace: h.cfg.OperatorNamespace,
			Pod: m.Pod, UID: m.UID, Container: managerContainer, ReadFrom: logFromSinceTime, SinceTime: since}, nil))
	}
	if target == "" {
		return errors.Join(append(errs, errors.New("the backup named no status.targetInstance, so no Instance "+
			"Manager's log was searched"))...)
	}
	member := s11LogCheck{Log: instanceManagerLog, Namespace: h.cfg.Namespace, Pod: target,
		Container: databaseContainer, ReadFrom: logFromSinceTime, SinceTime: since}
	if created {
		member.ReadFrom, member.SinceTime = logWhole, time.Time{}
	}
	errs = append(errs, h.searchLog(ctx, creds, member, func(pod *corev1.Pod) error {
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != kindStatefulSet || owner.Name != cluster {
			return fmt.Errorf("the Pod is not controlled by the StatefulSet of %s", cluster)
		}
		if !createdBefore(pod.CreationTimestamp.Time, requested) {
			return fmt.Errorf("the Pod was created at %s, not before the backup was requested at %s, so the log "+
				"of the Pod the backup ran on may be gone", pod.CreationTimestamp.UTC().Format(time.RFC3339),
				requested.Format(time.RFC3339))
		}
		return nil
	}))
	return errors.Join(errs...)
}

// searchLog reads the Pod of check, refuses one whose UID is not the one
// check names or that accept refuses, searches its container's log, and the
// previous instance's when the container restarted, and reads the Pod again
// to see that the container did not restart meanwhile. check is added to
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
		unchecked = append(unchecked, "read the Pod: "+h.redactError(err))
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
	status := containerStatus(pod, check.Container)
	if status == nil {
		unchecked = append(unchecked, "the Pod reports no status of the container, so its restarts are not known")
		return incomplete()
	}
	check.RestartCount, check.ContainerID = status.RestartCount, status.ContainerID
	window := "since " + check.SinceTime.Format(time.RFC3339)
	if check.ReadFrom == logWhole {
		window = "in its whole log"
	}
	limit := h.logLimit()
	n, truncated, found, err := h.readLog(ctx, check, false)
	check.BytesRead, check.Truncated = n, truncated
	switch {
	case err != nil:
		unchecked = append(unchecked, "read the log: "+h.redactError(err))
	case truncated:
		unchecked = append(unchecked, fmt.Sprintf("the API returned more than %d bytes of the log %s", limit,
			window))
	}
	if status.RestartCount > 0 {
		n, truncated, holds, err := h.readLog(ctx, check, true)
		check.PreviousRead, check.PreviousBytesRead, check.PreviousTruncated = err == nil, n, truncated
		found = found || holds
		switch {
		case err != nil:
			unchecked = append(unchecked, "read the previous instance's log: "+h.redactError(err))
		case truncated:
			unchecked = append(unchecked, fmt.Sprintf("the API returned more than %d bytes of the previous "+
				"instance's log %s", limit, window))
		}
		// Only the previous instance can be read; one before it is gone. In a
		// whole log every instance is in scope.
		if last := status.LastTerminationState.Terminated; status.RestartCount > 1 && (last == nil ||
			check.ReadFrom == logWhole || !last.StartedAt.Time.Before(check.SinceTime)) {
			unchecked = append(unchecked, fmt.Sprintf("the container restarted %d times, and an instance before the "+
				"previous one, which cannot be read, may have run %s", status.RestartCount, window))
		}
	}
	// A restart after the Pod was read would leave the instance read as
	// current unsearched once it became the previous one.
	if changed := h.restartedSince(ctx, check); changed != "" {
		unchecked = append(unchecked, changed)
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

// containerStatus is the status of the named container in pod, or nil.
func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// restartedSince reads check's Pod again after its logs were read and says
// why the reads may have missed an instance: a Pod that cannot be read or
// has another UID, or a container whose restart count or ID changed. It is
// empty when nothing changed.
func (h *haRun) restartedSince(ctx context.Context, check s11LogCheck) string {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: check.Namespace, Name: check.Pod}, pod); err != nil {
		return "read the Pod again after its log: " + h.redactError(err)
	}
	status := containerStatus(pod, check.Container)
	switch {
	case string(pod.UID) != check.UID:
		return fmt.Sprintf("the Pod's UID was %s after its log was read, not %s", pod.UID, check.UID)
	case status == nil:
		return "the Pod reported no status of the container after its log was read"
	case status.RestartCount != check.RestartCount || status.ContainerID != check.ContainerID:
		return fmt.Sprintf("the container restarted while its log was read (restart count %d, then %d)",
			check.RestartCount, status.RestartCount)
	}
	return ""
}

// readLog reads the log of check's container, or of its previous instance,
// from check.SinceTime or whole, at most the run's log limit and one more
// byte to see a truncation, and searches what it read, also when the read
// failed part way. It returns the bytes read.
func (h *haRun) readLog(ctx context.Context, check s11LogCheck, previous bool) (int64, bool, bool, error) {
	limit := h.logLimit()
	asked := limit + 1
	opts := &corev1.PodLogOptions{Container: check.Container, LimitBytes: &asked, Previous: previous}
	if check.ReadFrom != logWhole {
		since := metav1.NewTime(check.SinceTime)
		opts.SinceTime = &since
	}
	r, err := h.podLogs(ctx, check.Namespace, check.Pod, opts)
	if err != nil {
		return 0, false, false, err
	}
	defer func() { _ = r.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(r, asked))
	holds, err := h.holdsCredential(data)
	return int64(len(data)), int64(len(data)) > limit, holds, errors.Join(readErr, err)
}
