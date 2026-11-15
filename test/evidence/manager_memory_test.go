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

package evidence

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const sampleManagerMetrics = `# HELP controller_runtime_reconcile_total Total number of reconciliations per controller
# TYPE controller_runtime_reconcile_total counter
controller_runtime_reconcile_total{controller="cubridcluster",result="success"} 42
# HELP go_goroutines Number of goroutines that currently exist.
# TYPE go_goroutines gauge
go_goroutines 87
# TYPE go_memstats_alloc_bytes gauge
go_memstats_alloc_bytes 1.2e+07
# TYPE go_memstats_heap_inuse_bytes gauge
go_memstats_heap_inuse_bytes 2.5165824e+07
# TYPE go_memstats_sys_bytes gauge
go_memstats_sys_bytes 4.5e+07
# TYPE process_resident_memory_bytes gauge
process_resident_memory_bytes 6.8157440e+07
# TYPE cubrid_cluster_ready gauge
cubrid_cluster_ready{cluster="a",namespace="n"} 1
`

var memoryReadAt = time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)

// Only the memory series are kept, so the evidence stays bounded.
func TestParseManagerMemoryKeepsOnlyTheMemorySeries(t *testing.T) {
	m, err := ParseManagerMemory(sampleManagerMetrics, memoryReadAt)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"process_resident_memory_bytes": 68157440,
		"go_memstats_heap_inuse_bytes":  25165824,
		"go_memstats_alloc_bytes":       12000000,
		"go_memstats_sys_bytes":         45000000,
		"go_goroutines":                 87,
	}
	if len(m.Values) != len(want) {
		t.Errorf("kept %v, want %v", m.Values, want)
	}
	for k, v := range want {
		if m.Values[k] != v {
			t.Errorf("%s = %v, want %v", k, m.Values[k], v)
		}
	}
	text := m.Text()
	for _, banned := range []string{"controller_runtime", "cubrid_cluster"} {
		if strings.Contains(text, banned) {
			t.Errorf("text keeps %s:\n%s", banned, text)
		}
	}
	if !strings.HasPrefix(text, "read at 2026-11-15T03:00:00Z\n") ||
		!strings.Contains(text, "process_resident_memory_bytes 68157440\n") {
		t.Errorf("unexpected text:\n%s", text)
	}
	got := m.Measurements()
	if got["managerRSSBytes"] != 68157440.0 || got["managerHeapInuseBytes"] != 25165824.0 ||
		got["managerGoroutines"] != 87.0 {
		t.Errorf("measurements %v", got)
	}
}

// A series the answer lacks is unknown, never zero.
func TestParseManagerMemoryMissingSeriesIsUnknown(t *testing.T) {
	m, err := ParseManagerMemory("# TYPE go_goroutines gauge\ngo_goroutines 5\n", memoryReadAt)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Measurements()
	if got["managerRSSBytes"] != Unknown || got["managerHeapInuseBytes"] != Unknown || got["managerGoroutines"] != 5.0 {
		t.Errorf("measurements %v", got)
	}
	if strings.Contains(m.Text(), "process_resident_memory_bytes") {
		t.Errorf("text names an unread series:\n%s", m.Text())
	}
}

// An answer that is not metrics, or holds no memory series, is an error.
func TestParseManagerMemoryRejectsAnswersWithoutMemory(t *testing.T) {
	for name, text := range map[string]string{
		"empty":     "",
		"not text":  "<html>503</html>",
		"no series": "# TYPE cubrid_cluster_ready gauge\ncubrid_cluster_ready 1\n",
	} {
		if _, err := ParseManagerMemory(text, memoryReadAt); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// A failed read says so and measures nothing; it never records zero.
func TestUnreadManagerMemory(t *testing.T) {
	text := UnreadManagerMemoryText(memoryReadAt, errors.New("exec failed"))
	if !strings.Contains(text, "read at 2026-11-15T03:00:00Z") || !strings.Contains(text, "not read: exec failed") {
		t.Errorf("unexpected text:\n%s", text)
	}
	for k, v := range UnreadManagerMemoryMeasurements() {
		if v != Unknown {
			t.Errorf("%s = %v, want %s", k, v, Unknown)
		}
	}
	if len(UnreadManagerMemoryMeasurements()) != len(ManagerMemoryMeasurements) {
		t.Errorf("%v", UnreadManagerMemoryMeasurements())
	}
}
