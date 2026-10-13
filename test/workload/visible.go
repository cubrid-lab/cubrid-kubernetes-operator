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
	"slices"
	"strings"
	"sync"
	"time"
)

// WaitVisible reads every member in parallel, every poll, until read reports
// the marker on it, and returns for each member how long after start it was
// first seen. Only the reads are timed: what the caller checks afterwards is
// not part of a member's time, which is an upper bound by the duration of one
// read. A read that fails is retried. When ctx ends first, the error names
// each member not seen, with its last error, and the members seen are still
// returned.
func WaitVisible(ctx context.Context, start time.Time, members []string, poll time.Duration,
	read func(ctx context.Context, member string) (bool, error)) (map[string]time.Duration, error) {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		seen    = map[string]time.Duration{}
		missing []string
	)
	for _, member := range members {
		wg.Go(func() {
			var last error
			for {
				ok, err := read(ctx, member)
				if err == nil && ok {
					mu.Lock()
					seen[member] = time.Since(start)
					mu.Unlock()
					return
				}
				if err != nil {
					last = err
				}
				select {
				case <-ctx.Done():
					why := "the marker was not returned"
					if last != nil {
						why += "; last error: " + last.Error()
					}
					mu.Lock()
					missing = append(missing, member+": "+why)
					mu.Unlock()
					return
				case <-time.After(poll):
				}
			}
		})
	}
	wg.Wait()
	if len(missing) > 0 {
		slices.Sort(missing)
		return seen, errors.New("not every member returned the marker in time:\n" + strings.Join(missing, "\n"))
	}
	return seen, nil
}

// Slowest returns the longest of the times WaitVisible returned, and the
// member it belongs to.
func Slowest(seen map[string]time.Duration) (string, time.Duration) {
	var member string
	var slowest time.Duration
	for m, d := range seen {
		if d > slowest || member == "" {
			member, slowest = m, d
		}
	}
	return member, slowest
}
