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

// capturedReads runs captureMissingRow with a clock that each reading
// advances by took, a deadline of limit from now, and returns the members
// read in order, the number of waits and the text.
func capturedReads(limit, took time.Duration) ([]string, int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var reads []string
	waits := 0
	clock := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	out := captureMissingRow(ctx, "hab-2", "hab-1", "csql: the table is unknown", time.Minute,
		func() time.Time { return clock },
		func(context.Context, time.Duration) { waits++ },
		func(_ context.Context, member string) string {
			reads = append(reads, member)
			clock = clock.Add(took)
			return "state of " + member
		})
	return reads, waits, out
}

// The slave is read, after the wait read again, and the control comes last,
// each with its member, a UTC time and the error of the failed read.
func TestCaptureMissingRowReadsTheSlaveTwiceThenTheControl(t *testing.T) {
	reads, waits, out := capturedReads(3*time.Minute, time.Second)
	if got := strings.Join(reads, ","); got != "hab-2,hab-2,hab-1" {
		t.Fatalf("members read = %s, want hab-2,hab-2,hab-1", got)
	}
	if waits != 1 {
		t.Fatalf("waited %d times, want 1", waits)
	}
	for _, want := range []string{"The row could not be read on hab-2 within 3m; last error: csql: the table is unknown",
		"member hab-2, taken at 2026-11-18T10:00:00Z", "member hab-2, taken at 2026-11-18T10:00:01Z",
		"member hab-1, taken at 2026-11-18T10:00:02Z", "state of hab-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("capture lacks %q:\n%s", want, out)
		}
	}
}

// A first reading that used much of the time leaves too little for the wait
// and a second reading as long as the first: the wait and that reading are
// skipped, said so, and the control is still read.
func TestCaptureMissingRowSkipsTheWaitWhenTheFirstReadingUsedTheTime(t *testing.T) {
	reads, waits, out := capturedReads(100*time.Second, 50*time.Second)
	if got := strings.Join(reads, ","); got != "hab-2,hab-1" {
		t.Fatalf("members read = %s, want hab-2,hab-1", got)
	}
	if waits != 0 || !strings.Contains(out, "1m0s wait and the second reading of hab-2 were skipped") {
		t.Fatalf("waits = %d, capture:\n%s", waits, out)
	}
}
