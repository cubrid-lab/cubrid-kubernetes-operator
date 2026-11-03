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
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

// A member's log files are each a 32nd of its data volume, in whole MiB,
// between the 20M the engine accepts and the image's 512M. With the image's
// 512M files, a 2Gi member already used about 1.7 GB of its volume before any
// archive was kept (#666; docs/poc/RESULTS.md, POC-45).
func TestLogVolumeSizeIsA32ndOfTheDataVolumeWithinTheEngineLimits(t *testing.T) {
	const largest = "512M"
	for _, tc := range []struct{ size, want string }{
		{"100Mi", "20M"},
		{"640Mi", "20M"},
		{"1Gi", "32M"},
		{"1500Mi", "46M"},
		{"2Gi", "64M"},
		{"16Gi", largest},
		{"100Gi", largest},
		{"3G", "89M"},
		{"100E", largest},
	} {
		if got := logVolumeSize(resource.MustParse(tc.size)); got != tc.want {
			t.Errorf("logVolumeSize(%s) = %s, want %s", tc.size, got, tc.want)
		}
	}
}
