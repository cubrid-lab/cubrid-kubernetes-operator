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
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	// runLimit bounds every run of a script under test, so that a script that
	// hangs fails its test instead of the whole package.
	runLimit = 2 * time.Minute

	// envFakeUID is read by the id stand-in: the user the script believes it
	// runs as, whoever runs the tests.
	envFakeUID = "FAKE_UID"
	uidCubrid  = "1000"
	uidRoot    = "0"

	// idStub answers `id -u` with ${FAKE_UID} and passes anything else on. The
	// scripts decide their root or non-root path from `id -u` alone, so the
	// tests take the path they mean to, also when they run as root.
	idStub = `#!/bin/bash
if [ "$1" = "-u" ]; then
  echo "${FAKE_UID:-1000}"
  exit 0
fi
exec /usr/bin/id "$@"
`
)

// writeTools creates a directory of stand-in commands that is put in front of
// PATH, and returns it. It always holds the id stand-in.
func writeTools(t *testing.T, root string, extra map[string]string) string {
	t.Helper()
	tools := filepath.Join(root, "tools")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{"id": idStub}
	maps.Copy(stubs, extra)
	for name, content := range stubs {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return tools
}

// scriptCommand returns a command that runs script with the stand-ins first
// in PATH and only the given environment. The run is bounded by runLimit, and
// when the test ends the process is ended and reaped whether or not the test
// waited for it.
func scriptCommand(t *testing.T, script, tools string, env map[string]string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	cmd := exec.CommandContext(ctx, "bash", script)
	// Do not wait for children that keep the output open after the script ended.
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = []string{"PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH")}
	if _, ok := env[envFakeUID]; !ok {
		cmd.Env = append(cmd.Env, envFakeUID+"="+uidCubrid)
	}
	for k, v := range env {
		if v != "" {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	t.Cleanup(func() {
		cancel()
		if cmd.Process != nil {
			// Reaps a process the test started but did not wait for; after a
			// completed Wait this returns an error that does not matter.
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}
