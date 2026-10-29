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

package installed

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSamplerScriptSeesAHolderAndItsDeletedFile runs the sampler script
// itself, with its staging root in a temporary directory, next to a process
// named cub_server (a copy of bash) that holds a file below it. A reading
// must show the process, the operation's entry, the listing of its open files
// and the file it holds; once the directory is removed, a reading must show
// the file held as deleted and no entry. The fake sampler of the other tests
// only compares the script's text, so a shell error that printed no H line
// would pass them unnoticed.
func TestSamplerScriptSeesAHolderAndItsDeletedFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the sampler reads /proc")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not present")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "staging")
	staging := filepath.Join(root, "uid", "op")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(staging, "appdb_bk0v000")
	held, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	holderPID := startCubServer(t, bash, filepath.Join(tmp, "bin"), held)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	sampler := exec.CommandContext(ctx, bash, "-c", samplerScript(root))
	out, err := sampler.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sampler.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = sampler.Wait()
	}()
	watch := &stopWatch{seen: make(chan struct{})}
	go func() { _ = readSamples(out, watch, &s11StopSampler{}) }()

	pid := strconv.Itoa(holderPID)
	holds := func(s stopSample, path string) bool { return slices.Contains(s.Holders, pid+" "+path) }
	first := awaitSample(t, watch, time.Time{}, func(s stopSample) bool {
		return slices.Contains(s.Entries, "uid/op") && slices.Contains(s.FdListed, pid) && holds(s, file) &&
			slices.ContainsFunc(s.Processes, func(p string) bool { return strings.HasPrefix(p, pid+" cub_server ") })
	})
	if first == nil {
		t.Fatalf("no reading showed cub_server %s, its open files, the entry uid/op and the held file %s; "+
			"readings: %+v", pid, file, lastSamples(watch))
	}
	if err := os.RemoveAll(filepath.Join(root, "uid")); err != nil {
		t.Fatal(err)
	}
	removed := time.Now()
	after := awaitSample(t, watch, removed, func(s stopSample) bool {
		return len(s.Entries) == 0 && holds(s, file+" (deleted)")
	})
	if after == nil {
		t.Fatalf("no reading after the removal showed the held file as deleted without an entry; readings: %+v",
			lastSamples(watch))
	}
}

// startCubServer starts a copy of bash named cub_server in dir that holds
// held open on its file descriptor 3 and waits on a pipe nobody writes to,
// and returns its process ID. It is killed when the test ends.
func startCubServer(t *testing.T, bash, dir string, held *os.File) int {
	t.Helper()
	data, err := os.ReadFile(bash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	cub := filepath.Join(dir, "cub_server")
	if err := os.WriteFile(cub, data, 0o700); err != nil {
		t.Fatal(err)
	}
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(cub, "-c", "read -r _")
	holder.Stdin, holder.ExtraFiles = stdin, []*os.File{held}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	_ = held.Close()
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
		_ = w.Close()
	})
	return holder.Process.Pid
}

// awaitSample returns the first reading received after since that matches,
// waiting up to 20 seconds for one; nil when none came.
func awaitSample(t *testing.T, watch *stopWatch, since time.Time, match func(stopSample) bool) *stopSample {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		watch.mu.Lock()
		for _, s := range watch.samples {
			if s.Received.After(since) && match(s) {
				watch.mu.Unlock()
				return &s
			}
		}
		watch.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// lastSamples returns the last three readings, for a failure's message.
func lastSamples(watch *stopWatch) []stopSample {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	return slices.Clone(watch.samples[max(0, len(watch.samples)-3):])
}
