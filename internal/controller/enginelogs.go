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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// CUBRID writes its own log files under $CUBRID/log, /home/cubrid/CUBRID/log
// in the Instance Manager image: the server, cub_master, the log copiers and
// appliers, the utilities and, in a Broker Pod, the Broker and its CAS
// processes (docs/poc/RESULTS.md, POC-35). The directory is a volume of its
// own, so that the files outlive the container (#252). $CUBRID/var, which
// holds the applier's lock file, stays in the container (POC-21).
const (
	engineLogMountPath = "/home/cubrid/CUBRID/log"
	// engineLogVolume names the StatefulSet's log claim template, so a
	// member's log PVC is log-<cluster>-<ordinal>, and the Broker's emptyDir.
	engineLogVolume = "log"
)

// engineLogClaimSize is the size of each member's log PVC. The entrypoint
// bounds what CUBRID keeps there to about 160 MiB on an HA member
// (build/instance-manager/entrypoint.sh); the rest is headroom for the files
// POC-35 showed no bound for, which stayed under 15 KB each.
var engineLogClaimSize = resource.MustParse("1Gi")

// engineLogPVCTemplate is the second volume claim template of a member: a
// small volume of the data volume's StorageClass for $CUBRID/log. It is only
// ever on a StatefulSet created with it; volumeClaimTemplates cannot be
// changed on an existing one. The StatefulSet's PVC retention policy covers
// it, so it is deleted or retained with the data PVC. It never holds the
// database, and nothing takes it for evidence of one.
func engineLogPVCTemplate(cluster *databasev1alpha1.CubridCluster) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: engineLogVolume},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: cluster.Spec.Storage.Data.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: engineLogClaimSize},
			},
		},
	}
}

// mountEngineLogs mounts the log claim at $CUBRID/log when the StatefulSet has
// one. A StatefulSet an earlier build created has none: its members keep their
// logs in the container, and its Pod template stays as it was.
func mountEngineLogs(sts *appsv1.StatefulSet, spec *corev1.PodSpec) {
	if !slices.ContainsFunc(sts.Spec.VolumeClaimTemplates, func(t corev1.PersistentVolumeClaim) bool {
		return t.Name == engineLogVolume
	}) {
		return
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == appName {
			spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts,
				corev1.VolumeMount{Name: engineLogVolume, MountPath: engineLogMountPath})
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
