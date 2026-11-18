//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The slave without the row is read twice, a gap apart, and the control
// slave once between them, each with its member and a UTC time.
func TestCaptureMissingRowReadsTheSlaveTwiceAndTheControlOnce(t *testing.T) {
	var reads []string
	waits := 0
	clock := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	out := captureMissingRow(context.Background(), "hab-2", "hab-1", time.Minute,
		func() time.Time { clock = clock.Add(time.Second); return clock },
		func(context.Context, time.Duration) { waits++ },
		func(_ context.Context, member string) string {
			reads = append(reads, member)
			return "state of " + member
		})
	if got := strings.Join(reads, ","); got != "hab-2,hab-1,hab-2" {
		t.Fatalf("members read = %s, want hab-2,hab-1,hab-2", got)
	}
	if waits != 1 {
		t.Fatalf("waited %d times, want 1", waits)
	}
	for _, want := range []string{"member hab-2, taken at 2026-11-18T10:00:01Z", "member hab-1, taken at 2026-11-18T10:00:02Z",
		"member hab-2, taken at 2026-11-18T10:00:03Z", "state of hab-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("capture lacks %q:\n%s", want, out)
		}
	}
}

// With less than the gap left, the wait is skipped and said so, and the
// second reading is still taken, so the capture stays within its limit.
func TestCaptureMissingRowSkipsTheWaitWhenTheLimitIsNear(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waited := false
	out := captureMissingRow(ctx, "hab-1", "hab-2", time.Minute, time.Now,
		func(context.Context, time.Duration) { waited = true },
		func(context.Context, string) string { return "x" })
	if waited || !strings.Contains(out, "second reading was skipped") {
		t.Fatalf("waited = %v, capture:\n%s", waited, out)
	}
}
