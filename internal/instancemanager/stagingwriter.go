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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The file of `cubrid backupdb -C` is written by cub_server, which is not in
// the utility's process group that a cancellation ends (cli.go). After the
// group was killed, cub_server kept the file open for about 0.3 to 2 s, and
// then removed it or left it at its full size (docs/poc/RESULTS.md, POC-27).
// A failed backup therefore removes its staging only once no process holds a
// file below it, and leaves it, unused, when one still does after a bound.
// cub_server is never signalled for this.
const (
	// stagingWriterBound is how long a failed backup waits for its staging
	// files to be closed: 15 times the longest hold POC-27 saw after the
	// group was killed (about 2 s), for a server that is slower under I/O
	// pressure. It stays well inside the stop's wait for the operations
	// (Timeouts.Shutdown, 100 s by default), which runs before CUBRID stops.
	stagingWriterBound = 30 * time.Second
	// stagingWriterPoll is how often the wait looks again.
	stagingWriterPoll = 200 * time.Millisecond
)

// stagingHolders lists, as "pid comm path", the processes that hold a file
// open at or below dir, read from the /proc/<pid>/fd links. The Instance
// Manager runs in the CUBRID container as the same user as cub_server, as
// the process watch and cli.go rely on, so it reads the links of CUBRID's
// processes. A held file that was deleted still counts ("<path> (deleted)").
//
// A process that ended during the scan is skipped. A process whose links
// cannot be read is skipped too, unless it is a CUBRID process (comm cub*):
// that one is the writer this guards against, and is reported as an error,
// so that the caller counts the staging as held (fail closed). Another
// user's process, or a non-dumpable one of the same user, cannot be one
// that CUBRID started for the backup; refusing on it would block a
// destination for good because of an unrelated process. A process list that
// cannot be read is an error.
func stagingHolders(dir string) ([]string, error) {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	dir = filepath.Clean(dir)
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read the process list: %w", err)
	}
	var holders []string
	var unreadable error
	for _, p := range procs {
		pid := p.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", pid, "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			comm := procComm(pid)
			if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ESRCH) && strings.HasPrefix(comm, "cub") {
				unreadable = errors.Join(unreadable, fmt.Errorf("read the open files of %s %s: %w", pid, comm, err))
			}
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if target == dir || strings.HasPrefix(target, dir+"/") {
				holders = append(holders, pid+" "+procComm(pid)+" "+target)
				break
			}
		}
	}
	return holders, unreadable
}

// procComm is the command name of the process pid, empty once it ended.
func procComm(pid string) string {
	b, err := os.ReadFile(filepath.Join("/proc", pid, "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// describeHolders is what a log line or a reason says about the holders of
// a staging directory and the error of looking for them.
func describeHolders(held []string, err error) string {
	what := strings.Join(held, ", ")
	if err != nil {
		what = strings.TrimPrefix(what+"; "+err.Error(), "; ")
	}
	return what
}

// awaitStagingWriters returns nil once no process holds a file below the
// staging directory dir of operation id, which may then be removed. A
// directory still held, or whose holders cannot be told, after the bound is
// an error that says the directory is kept: the caller neither removes it
// nor uploads it, and the staging claim keeps it out of reuse while it is
// held (stagingOwnerLive). It waits without a context: it runs after the
// backup's context ended, and a stop waits for the operations before it
// stops CUBRID.
func (s *Server) awaitStagingWriters(id, dir string) error {
	bound, poll := s.stagingWriterBound, s.stagingWriterPoll
	if bound <= 0 {
		bound = stagingWriterBound
	}
	if poll <= 0 {
		poll = stagingWriterPoll
	}
	start := time.Now()
	held, err := stagingHolders(dir)
	if err == nil && len(held) == 0 {
		return nil
	}
	s.log().Info("Waiting for processes to close backup staging files", "event", "backup_staging_writer_wait",
		"operationID", id, "stagingDir", dir, "holders", describeHolders(held, err))
	for time.Since(start) < bound {
		time.Sleep(poll)
		held, err = stagingHolders(dir)
		if err == nil && len(held) == 0 {
			s.log().Info("Processes closed backup staging files", "event", "backup_staging_writer_closed",
				"operationID", id, "stagingDir", dir, "waitedMilliseconds", time.Since(start).Milliseconds())
			return nil
		}
	}
	what := describeHolders(held, err)
	s.log().Warn("Kept backup staging that a process still held after the wait", "event", "backup_staging_writer_timeout",
		"operationID", id, "stagingDir", dir, "holders", what, "waitedSeconds", int(time.Since(start).Seconds()))
	return fmt.Errorf("the backup staging %s is kept and is not uploaded or reused: a process still held a file in it after %s (%s)",
		dir, bound, what)
}
