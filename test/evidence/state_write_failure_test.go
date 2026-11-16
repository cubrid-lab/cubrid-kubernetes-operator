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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed Pod snapshot write must not claim stored objects in the writable
// index, and must leave the other sources available to diagnose the scenario.
func TestSnapshotWriteFailureRecordsNoStoredObjects(t *testing.T) {
	dir := t.TempDir()
	podsPath := filepath.Join(dir, testStateDir, LogsAfterVerdict, "pods.json")
	if err := os.MkdirAll(podsPath, 0o750); err != nil {
		t.Fatal(err)
	}
	b := testState(dir, newFakeState())
	b.Checkpoint(t.Context(), LogsAfterVerdict)

	if b.Problem() == nil {
		t.Fatal("the failed snapshot write was not reported")
	}
	if got, want := b.IndexFile(), testStateDir+"/index.json"; got != want {
		t.Fatalf("index file %q, want %q", got, want)
	}
	index := readStateIndex(t, dir)
	pods := stateSource(t, index, LogsAfterVerdict, StatePods)
	if pods.Status != LogUnavailable || !strings.Contains(pods.Reason, "the file could not be written") {
		t.Errorf("the failed Pod snapshot was not recorded as unavailable: %+v", pods)
	}
	if pods.Listed == nil || *pods.Listed != 1 {
		t.Errorf("the failed Pod snapshot lost its listed count: %+v", pods)
	}
	if pods.Stored != 0 || pods.File != "" || len(pods.Objects) != 0 {
		t.Errorf("the failed Pod snapshot claims stored evidence: %+v", pods)
	}
	for _, name := range []string{StateCluster, StateOperatorPods, StateServices, StateEndpointSlices, StateEvents} {
		source := stateSource(t, index, LogsAfterVerdict, name)
		if source.Status != LogCollected || source.Stored != 1 || source.File == "" {
			t.Errorf("the available source %s was not collected: %+v", name, source)
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, source.File)); err != nil {
			t.Errorf("the available source %s has no snapshot file: %v", name, err)
		}
	}
}
