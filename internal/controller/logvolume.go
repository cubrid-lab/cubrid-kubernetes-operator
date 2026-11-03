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
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
)

// The bounds of a log volume: the smallest createdb accepts, and the image's
// own log_volume_size, which a large data volume keeps.
const (
	minLogVolumeMiB = 20
	maxLogVolumeMiB = 512
)

// logVolumeSize is the --log-volume-size, in whole MiB, of a database created
// on a data volume of size: a 32nd of the volume, between minLogVolumeMiB and
// maxLogVolumeMiB.
//
// Every log file of a member is this large: its active log, its temporary
// archive, lgar.removed and its archives, and the same files for each peer
// whose log it copies. In POC-45, CUBRID removed archives only when it
// created a new one, so a member held several at a time. With the image's 512M, a 2Gi HA
// member already used about 1.7 GB before any archive was kept (#666;
// docs/poc/RESULTS.md, POC-45). The members seeded from a backup keep the
// backup's size, so the size chosen at the first createdb holds for the
// whole cluster.
func logVolumeSize(size resource.Quantity) string {
	// Value() is not reliable beyond int64 (100E gives 0), so a size of 16Gi
	// or more is taken as the largest log volume first.
	if size.Cmp(*resource.NewQuantity(maxLogVolumeMiB*32<<20, resource.BinarySI)) >= 0 {
		return strconv.Itoa(maxLogVolumeMiB) + "M"
	}
	mib := size.Value() / 32 / (1 << 20)
	mib = max(minLogVolumeMiB, min(maxLogVolumeMiB, mib))
	return strconv.FormatInt(mib, 10) + "M"
}
