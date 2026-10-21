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

package instancemanager

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// envCrashWrite makes the test binary run one metadata write with a file
// size limit and die by SIGXFSZ when the write passes it, as a crash or a
// power loss interrupts a write: "<mode> <limit> <dir>".
const envCrashWrite = "IM_TEST_CRASH_WRITE"

const (
	crashRegister   = "register"
	crashUnregister = "unregister"
	crashRemark     = "remark"
	crashDatabase   = "newdb"
	crashMarked     = "markeddb"
)

var (
	crashOldOwner = "op-" + strings.Repeat("a", 32)
	crashNewOwner = "op-" + strings.Repeat("b", 32)
)

// crashWrite is the child's side of envCrashWrite. It returns only when the
// write did not die; the parent then reports that the fault was not hit.
func crashWrite(spec string) int {
	parts := strings.SplitN(spec, " ", 3)
	if len(parts) != 3 {
		return 2
	}
	limit, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 2
	}
	dir := parts[2]
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 2
	}
	if err := defaultFileSizeSignal(); err != nil {
		return 2
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: limit, Max: limit}); err != nil {
		return 2
	}
	switch parts[0] {
	case crashRegister:
		_, err = registerRestoreTarget(dir, crashDatabase, "host-0", crashNewOwner)
	case crashUnregister:
		err = unregisterDatabase(root, "db2")
	case crashRemark:
		err = markOwned(root, crashMarked, crashNewOwner)
	default:
		return 2
	}
	if err != nil {
		_, _ = os.Stdout.WriteString(err.Error() + "\n")
		return 3
	}
	return 0
}

// registrations is databases.txt with three databases, each line longer than
// the size limit of the crash.
func registrations(dir string) string {
	var b strings.Builder
	for _, db := range []string{"db1", "db2", "db3"} {
		path := filepath.Join(dir, db)
		b.WriteString(db + "\t\t" + path + "\thost-0\t" + path + "\tfile:" + filepath.Join(path, "lob") + "\n")
	}
	return b.String()
}

// runCrash runs the write in a child that the kernel kills once the write
// passes limit bytes in one file.
func runCrash(t *testing.T, mode string, limit int, dir string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the crash is injected with Linux's SIGXFSZ default action")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envCrashWrite+"="+mode+" "+strconv.Itoa(limit)+" "+dir)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the write was not interrupted (err %v): %s", err, out)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGXFSZ {
		t.Fatalf("the write ended with %v, not by the size limit (SIGXFSZ): %s", err, out)
	}
}

// A crash in the middle of a metadata write leaves the file as it was before
// the write, never truncated or half written: databases.txt keeps every
// other database's registration (#298), and an ownership marker keeps naming
// an operation, so that the next start neither loses a database nor reads an
// unreadable marker.
func TestMetadataWrites_ACrashLeavesTheEarlierContent(t *testing.T) {
	t.Run("registering a restore target", func(t *testing.T) {
		dir := t.TempDir()
		before := registrations(dir)
		writeFile(t, filepath.Join(dir, databasesTxt), before)
		runCrash(t, crashRegister, 64, dir)
		if got := readFile(t, filepath.Join(dir, databasesTxt)); got != before {
			t.Errorf("databases.txt after a crash in its write =\n%q\nwant the earlier content\n%q", got, before)
		}
	})
	t.Run("unregistering a database", func(t *testing.T) {
		dir := t.TempDir()
		before := registrations(dir)
		writeFile(t, filepath.Join(dir, databasesTxt), before)
		runCrash(t, crashUnregister, 64, dir)
		if got := readFile(t, filepath.Join(dir, databasesTxt)); got != before {
			t.Errorf("databases.txt after a crash in its write =\n%q\nwant the earlier content\n%q", got, before)
		}
	})
	t.Run("handing a marked database to another operation", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, crashMarked, ownershipMarker)
		writeFile(t, marker, crashOldOwner+"\n")
		runCrash(t, crashRemark, 10, dir)
		if got := readFile(t, marker); got != crashOldOwner+"\n" {
			t.Errorf("marker after a crash in its write = %q, want the earlier owner %q", got, crashOldOwner+"\n")
		}
	})
}

// After a crash, the next write of the same file succeeds and leaves only
// the new content.
func TestMetadataWrites_TheNextWriteAfterACrashSucceeds(t *testing.T) {
	dir := t.TempDir()
	before := registrations(dir)
	writeFile(t, filepath.Join(dir, databasesTxt), before)
	runCrash(t, crashUnregister, 64, dir)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := unregisterDatabase(root, "db2"); err != nil {
		t.Fatalf("unregister after a crash: %v", err)
	}
	got := readFile(t, filepath.Join(dir, databasesTxt))
	if strings.Contains(got, "db2\t") || !strings.Contains(got, "db1\t") || !strings.Contains(got, "db3\t") {
		t.Errorf("databases.txt = %q, want db1 and db3 only", got)
	}
}
