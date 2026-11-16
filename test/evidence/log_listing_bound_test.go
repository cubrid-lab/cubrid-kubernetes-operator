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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A listing's retained end holds older files, so an oversized listing cannot
// select the newest logs. Reject it without changing earlier evidence or the
// scenario failure that triggered collection.
func TestEngineListingPastReadBoundDoesNotReadOlderFiles(t *testing.T) {
	f := newFakeLogs()
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsBeforeFault, false)
	before := readIndex(t, dir).Checkpoints[0]
	kept := allBytes(t, filepath.Join(dir, testLogsDir, LogsBeforeFault))
	var listing strings.Builder
	for n := 9999; listing.Len() <= logListingLimit; n-- {
		fmt.Fprintf(&listing, "server/appdb_20261106_%04d.err\n", n)
	}
	listing.WriteString("ha-0_master.err\n")
	f.files, f.execs = listing.String(), nil

	const failure = "the scenario's failed assertion"
	func() {
		defer func() {
			if got := recover(); got != failure {
				t.Errorf("scenario failure changed to %v", got)
			}
		}()
		defer b.Checkpoint(t.Context(), LogsAfterVerdict, true)
		panic(failure)
	}()
	if b.Problem() != nil {
		t.Fatal(b.Problem())
	}
	for _, command := range f.execs {
		if !slices.Equal(command, ListEngineLogs) && !slices.Equal(command, HeartbeatStatus) {
			t.Errorf("an oversized listing selected an older engine log: %q", command)
		}
	}
	index := readIndex(t, dir)
	for _, source := range index.Checkpoints[1].Sources {
		if source.Component != LogEngine && source.Component != LogBroker {
			continue
		}
		if source.Status != LogUnavailable || !strings.Contains(source.Reason, "read bound") ||
			source.File != "" || source.Origin != logFilesRoot {
			t.Errorf("oversized listing source %+v", source)
		}
	}
	if !slices.Equal(index.Checkpoints[0].Sources, before.Sources) {
		t.Error("the earlier checkpoint's sources changed")
	}
	for path, want := range kept {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("earlier evidence %s changed: %v", path, err)
		}
	}
}

// A complete listing at the read bound still identifies the files to read.
func TestEngineListingAtReadBoundIsRead(t *testing.T) {
	f := newFakeLogs()
	f.files = strings.Repeat("\n", logListingLimit-len(testServerLog)-1) + testServerLog + "\n"
	b, dir := newTestBundle(t, f, testLimits(), nil)
	b.Checkpoint(t.Context(), LogsAfterVerdict, true)
	source := findSource(t, readIndex(t, dir), LogEngine, testMaster)
	if source.Status != LogCollected || source.Origin != logFilesRoot+testServerLog {
		t.Fatalf("complete listing source %+v", source)
	}
}
