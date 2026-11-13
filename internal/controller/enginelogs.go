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
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// CUBRID writes its own log files under $CUBRID/log, /home/cubrid/CUBRID/log
// in the Instance Manager image: the server, cub_master, the log copiers and
// appliers, the utilities and, in a Broker Pod, the Broker and its CAS
// processes (docs/poc/RESULTS.md, POC-35). On a member the directory is on a
// volume, so that the files outlive the container (#252). $CUBRID/var, which
// holds the applier's lock file, stays in the container (POC-21).
//
// A member's StatefulSet keeps the volume it was created with, because
// volumeClaimTemplates cannot change and a new Pod template would restart its
// members (#812). That gives three layouts:
//   - a StatefulSet this Operator creates mounts the subdirectory engineLogSubPath of the
//     data volume at $CUBRID/log, so each member has one claim. With a second
//     claim, the scheduler could bind one claim to a node and fail on the
//     other, which left a member unschedulable for good (#856);
//   - a StatefulSet created with the log claim template (#849) keeps mounting
//     its log claim, log-<cluster>-<ordinal>;
//   - a StatefulSet created before either mounts nothing there, and its
//     members keep their logs in the container.
const (
	engineLogMountPath = "/home/cubrid/CUBRID/log"
	// engineLogVolume names the log claim template of a StatefulSet created
	// with one, so its member's log PVC is log-<cluster>-<ordinal>, and the
	// Broker's emptyDir.
	engineLogVolume = "log"
	// engineLogSubPath is the directory of the data volume that holds
	// $CUBRID/log. Nothing else on the data volume uses this name: its root
	// holds databases, operations, backup-staging and restore-staging. The
	// name must never change: an existing StatefulSet is recognized by this
	// exact mount, and a member mounting another name would lose its earlier
	// logs on its next restart.
	engineLogSubPath = "cubrid-log"
	// dataVolume names the data claim template and its mount.
	dataVolume = "data"
)

// engineLogMount is where the cubrid container of a member mounts its log
// files, judged from the StatefulSet as it is: the subdirectory of the data
// volume on a StatefulSet this Operator creates or one already mounting it, the log claim
// on one created with it, and nothing on one created before either. The
// existing Pod template decides, so a reconcile never changes its mounts.
func engineLogMount(sts *appsv1.StatefulSet) (corev1.VolumeMount, bool) {
	onData := corev1.VolumeMount{Name: dataVolume, MountPath: engineLogMountPath, SubPath: engineLogSubPath}
	if sts.CreationTimestamp.IsZero() {
		return onData, true
	}
	if slices.ContainsFunc(sts.Spec.VolumeClaimTemplates, func(t corev1.PersistentVolumeClaim) bool {
		return t.Name == engineLogVolume
	}) {
		return corev1.VolumeMount{Name: engineLogVolume, MountPath: engineLogMountPath}, true
	}
	for _, c := range sts.Spec.Template.Spec.Containers {
		if c.Name == appName && slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.Name == onData.Name && m.MountPath == onData.MountPath && m.SubPath == onData.SubPath
		}) {
			return onData, true
		}
	}
	return corev1.VolumeMount{}, false
}

// mountEngineLogs adds the mount engineLogMount chooses for sts to the cubrid
// container of spec.
func mountEngineLogs(sts *appsv1.StatefulSet, spec *corev1.PodSpec) {
	mount, ok := engineLogMount(sts)
	if !ok {
		return
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == appName {
			spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts, mount)
		}
	}
}

// The Broker's log bound. Each CAS process keeps an SQL log and a slow-query
// log, each rotated at SQL_LOG_MAX_SIZE to one .bak, and an error log rotated
// at the error_log_size of the Broker's cubrid.conf, 4 MiB
// (build/instance-manager/broker-entrypoint.sh), to one .bak: at most
// 2 x 4 MiB each, and the files passed their limit by up to 1.5 KB before
// rotating (POC-35). That is under 25 MiB per CAS. cubrid_utility.log, also
// rotated at error_log_size, the admin log and the empty files the utilities
// create stay under 32 MiB. ACCESS_LOG stays OFF: its rotated copies were
// never removed in POC-35.
const (
	brokerSQLLogMaxSizeKB = 4096
	brokerLogPerCASMiB    = 25
	brokerLogFixedMiB     = 32
	brokerLogMinMiB       = 256
)

// brokerLogSizeLimit is the sizeLimit of a Broker Pod's log emptyDir: at least
// 256Mi, and more when the Brokers run more CAS processes than that holds.
// The kubelet evicts a Pod whose emptyDir passes its limit.
func brokerLogSizeLimit(maxApplServers int) resource.Quantity {
	mib := max(brokerLogMinMiB, brokerLogFixedMiB+brokerLogPerCASMiB*maxApplServers)
	return *resource.NewQuantity(int64(mib)<<20, resource.BinarySI)
}
