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
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// ManagerMemorySeries are the only series of the manager's metrics that the
// evidence keeps of its memory, in the order manager-memory.txt lists them:
// the resident set size of the process, the Go heap in use and allocated, the
// memory the Go runtime obtained from the system, and the goroutines.
var ManagerMemorySeries = []string{
	"process_resident_memory_bytes",
	"go_memstats_heap_inuse_bytes",
	"go_memstats_alloc_bytes",
	"go_memstats_sys_bytes",
	"go_goroutines",
}

// ManagerMemoryMeasurements names, for each series, the measurement of a
// scenario's result in summary.json that carries its value.
var ManagerMemoryMeasurements = map[string]string{
	"process_resident_memory_bytes": "managerRSSBytes",
	"go_memstats_heap_inuse_bytes":  "managerHeapInuseBytes",
	"go_memstats_alloc_bytes":       "managerHeapAllocBytes",
	"go_memstats_sys_bytes":         "managerSysBytes",
	"go_goroutines":                 "managerGoroutines",
}

// ManagerMemory is the manager's memory series from one read of its metrics.
type ManagerMemory struct {
	ReadAt time.Time
	// Values holds the series the answer had; one it lacked is not there.
	Values map[string]float64
}

// ParseManagerMemory keeps the memory series of a body of the Prometheus text
// format, read at the time given. A body that is not that format, or that has
// none of the series, is an error: an answer that could not be read is never
// taken for a memory of zero.
func ParseManagerMemory(text string, at time.Time) (ManagerMemory, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		return ManagerMemory{}, fmt.Errorf("the metrics endpoint's answer is not the text format: %w", err)
	}
	values := map[string]float64{}
	for _, name := range ManagerMemorySeries {
		family, ok := families[name]
		if !ok || len(family.GetMetric()) == 0 {
			continue
		}
		m := family.GetMetric()[0]
		switch {
		case m.Gauge != nil:
			values[name] = m.GetGauge().GetValue()
		case m.Counter != nil:
			values[name] = m.GetCounter().GetValue()
		case m.Untyped != nil:
			values[name] = m.GetUntyped().GetValue()
		}
	}
	if len(values) == 0 {
		return ManagerMemory{}, errors.New("the metrics endpoint's answer has none of the manager's memory series")
	}
	return ManagerMemory{ReadAt: at, Values: values}, nil
}

// Text is the content of manager-memory.txt: the read time and each series
// the answer had.
func (m ManagerMemory) Text() string {
	lines := []string{"read at " + m.ReadAt.UTC().Format(time.RFC3339)}
	for _, name := range ManagerMemorySeries {
		if v, ok := m.Values[name]; ok {
			lines = append(lines, name+" "+strconv.FormatFloat(v, 'f', -1, 64))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// Measurements are the measurements of a scenario's result for the series; a
// series the answer lacked is Unknown.
func (m ManagerMemory) Measurements() map[string]any {
	out := map[string]any{}
	for _, name := range ManagerMemorySeries {
		if v, ok := m.Values[name]; ok {
			out[ManagerMemoryMeasurements[name]] = v
		} else {
			out[ManagerMemoryMeasurements[name]] = Unknown
		}
	}
	return out
}

// UnreadManagerMemoryText is the content of manager-memory.txt when the
// metrics could not be read.
func UnreadManagerMemoryText(at time.Time, err error) string {
	return "read at " + at.UTC().Format(time.RFC3339) + "\nnot read: " + err.Error() + "\n"
}

// UnreadManagerMemoryMeasurements are the measurements when the metrics could
// not be read: each is Unknown, never zero.
func UnreadManagerMemoryMeasurements() map[string]any {
	out := map[string]any{}
	for _, name := range ManagerMemorySeries {
		out[ManagerMemoryMeasurements[name]] = Unknown
	}
	return out
}
