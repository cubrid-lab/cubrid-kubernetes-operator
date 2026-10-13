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

package workload

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowMember is the member that returns the marker last.
const slowMember = "m-2"

// Each member's time is when it first returned the marker, measured from
// the commit: a slow member does not delay the others, and nothing the
// caller does afterwards is part of it.
func TestWaitVisible(t *testing.T) {
	start := time.Now()
	due := map[string]time.Duration{memberB: 20 * time.Millisecond, slowMember: 120 * time.Millisecond}
	var mu sync.Mutex
	calls := map[string]int{}
	read := func(_ context.Context, member string) (bool, error) {
		mu.Lock()
		calls[member]++
		first := calls[member] == 1
		mu.Unlock()
		if first {
			return false, errors.New("a transient error is read again")
		}
		return time.Since(start) >= due[member], nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	seen, err := WaitVisible(ctx, start, []string{memberB, slowMember}, 5*time.Millisecond, read)
	if err != nil {
		t.Fatal(err)
	}
	for member, at := range due {
		if seen[member] < at || seen[member] > at+500*time.Millisecond {
			t.Errorf("%s seen after %v, want soon after %v", member, seen[member], at)
		}
	}
	if seen[memberB] >= due[slowMember] {
		t.Errorf("the fast member waited for the slow one: %v", seen[memberB])
	}
	if member, d := Slowest(seen); member != slowMember || d != seen[slowMember] {
		t.Errorf("Slowest = %s %v, want m-2 %v", member, d, seen[slowMember])
	}
}

// With no member to read, nothing has been seen.
func TestWaitVisibleNoMembers(t *testing.T) {
	read := func(context.Context, string) (bool, error) { return true, nil }
	if _, err := WaitVisible(context.Background(), time.Now(), nil, time.Millisecond, read); err == nil {
		t.Error("no member to read was reported as every member seen")
	}
}

// A member that never returns the marker is named, with its last error.
func TestWaitVisibleTimesOut(t *testing.T) {
	read := func(_ context.Context, member string) (bool, error) {
		if member == slowMember {
			return false, errors.New("csql failed")
		}
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	seen, err := WaitVisible(ctx, time.Now(), []string{memberB, slowMember}, 5*time.Millisecond, read)
	if err == nil || !strings.Contains(err.Error(), slowMember) || !strings.Contains(err.Error(), "csql failed") {
		t.Errorf("err = %v, want m-2 named with its last error", err)
	}
	if _, ok := seen[memberB]; !ok || len(seen) != 1 {
		t.Errorf("seen = %v, want only m-1", seen)
	}
}
