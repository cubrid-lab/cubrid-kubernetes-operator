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

package image

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// envStopFile overrides where the entrypoint looks for the stop record that
// "instance-manager shutdown" writes.
const envStopFile = "IM_STOP_FILE"

// The preStop hook runs "instance-manager shutdown" in a process of its own,
// which writes the stop record and, while no manager listens, stops CUBRID
// itself; the entrypoint gets no signal until the hook has returned. A stop
// recorded before the entrypoint starts CUBRID therefore starts nothing,
// neither CUBRID nor the manager (#323).
func TestEntrypoint_StopRecordedBeforeTheStartStartsNothing(t *testing.T) {
	for _, components := range []string{componentsServer, componentsHA} {
		t.Run(components, func(t *testing.T) {
			f := newFixture(t, blockingManagerStub)
			f.env["CUBRID_COMPONENTS"] = components
			if components == componentsHA {
				f.configureHA()
			}
			f.registerDatabase()
			if err := os.WriteFile(f.env[envStopFile], nil, 0o600); err != nil {
				t.Fatal(err)
			}
			code, out := f.run()
			if code != 0 {
				t.Fatalf("exit = %d\n%s", code, out)
			}
			for _, call := range f.recorded() {
				// Nothing was started here, so nothing is stopped either.
				if strings.HasSuffix(call, " start") || strings.Contains(call, " start ") || call == callManagerStart ||
					call == callShutdown {
					t.Errorf("%q ran after the stop was recorded: %q", call, f.recorded())
				}
			}
			if !strings.Contains(out, "a stop was requested") {
				t.Errorf("the log does not say why nothing was started:\n%s", out)
			}
		})
	}
}

// A stop recorded while the start runs is seen when the start returns: what
// was started is stopped, and the manager is not started.
func TestEntrypoint_StopRecordedDuringTheStartStopsWhatWasStarted(t *testing.T) {
	f := newFixture(t, blockingManagerStub)
	f.registerDatabase()
	f.env["CUBRID_START_SECONDS"] = "1"
	cmd := f.command()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return index(f.recorded(), "cubrid server start "+dbName) >= 0 }, &out)
	if err := os.WriteFile(f.env[envStopFile], nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(cmd); err != nil {
		t.Fatalf("entrypoint after a stop recorded during the start: %v\n%s", err, out.String())
	}
	calls := f.recorded()
	if start, stop := index(calls, "cubrid server start "+dbName), index(calls, callShutdown); stop < start {
		t.Errorf("the started server was not stopped: %q", calls)
	}
	if managerStarted(calls) {
		t.Errorf("the manager was started after the stop was recorded: %q", calls)
	}
}

// The entrypoint looks by default where the shutdown command writes the stop
// record.
func TestEntrypoint_LooksForTheStopRecordWhereTheShutdownWritesIt(t *testing.T) {
	script, err := os.ReadFile("entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	want := regexp.MustCompile(`IM_STOP_FILE="\$\{IM_STOP_FILE:-` + regexp.QuoteMeta(instancemanager.StopFile) + `\}"`)
	if !want.Match(script) {
		t.Errorf("entrypoint.sh does not default IM_STOP_FILE to %s", instancemanager.StopFile)
	}
}
