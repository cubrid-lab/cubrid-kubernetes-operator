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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// volumeLockSuffixes name the files of a database directory that a running
// CUBRID process holds a POSIX write lock on, over the whole file: the
// server on <db>_lgat, the log copier on <db>_lgat of the copied log
// directory <db>_<peer>, and the log applier on <db>_lgla__lock there
// (docs/poc/RESULTS.md, POC-29). The lock is the kernel's, so it is seen from
// another container on the same volume and is gone when its process is,
// also after a SIGKILL; the PID written into the <db>_lgat__lock file is not
// (it is left behind and names another PID namespace).
var volumeLockSuffixes = []string{"_lgat", "_lgla__lock"}

// VolumeLock is a lock another process holds on a file of the data volume.
type VolumeLock struct {
	File string
	// PID is the holder as the kernel reports it to this process: 0 when the
	// holder runs in another PID namespace, such as another container.
	PID int32
}

// HeldVolumeLocks returns the CUBRID locks that other processes hold on the
// database directories under databases: each directory directly below it
// and each log directory registered in its databases.txt. It only asks
// (F_GETLK): it never takes, waits for or breaks a lock. A file or directory
// that is not there holds no lock. A symbolic link is followed, and one that
// cannot be followed is an error. Any other failure is returned as well, so
// that a caller does not take a volume it could not check for free.
//
// The caller must not hold a lock on these files itself: F_GETLK does not
// report the caller's own locks, and closing a file drops them.
func HeldVolumeLocks(databases string) ([]VolumeLock, error) {
	files, err := volumeLockFiles(databases)
	if err != nil {
		return nil, err
	}
	var held []VolumeLock
	for _, file := range files {
		locked, pid, err := probeLock(file)
		if err != nil {
			return nil, err
		}
		if locked {
			held = append(held, VolumeLock{File: file, PID: pid})
		}
	}
	return held, nil
}

// volumeLockFiles lists the files HeldVolumeLocks asks about.
func volumeLockFiles(databases string) ([]string, error) {
	dirs := map[string]bool{}
	entries, err := os.ReadDir(databases)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("list %s: %w", databases, err)
	}
	for _, entry := range entries {
		mode, err := followedMode(databases, entry)
		if err != nil {
			return nil, err
		}
		if mode.IsDir() {
			dirs[filepath.Join(databases, entry.Name())] = true
		}
	}
	logDirs, err := registeredLogDirs(databases)
	if err != nil {
		return nil, err
	}
	for _, dir := range logDirs {
		dirs[dir] = true
	}
	var files []string
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", dir, err)
		}
		for _, entry := range entries {
			if !hasVolumeLockSuffix(entry.Name()) {
				continue
			}
			mode, err := followedMode(dir, entry)
			if err != nil {
				return nil, err
			}
			if mode.IsRegular() {
				files = append(files, filepath.Join(dir, entry.Name()))
			}
		}
	}
	return files, nil
}

func hasVolumeLockSuffix(name string) bool {
	for _, suffix := range volumeLockSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// followedMode is the type of a directory entry, through a symbolic link: a
// linked database directory or lock file is checked like the real one, and
// a link that cannot be followed is an error, never a free file.
func followedMode(dir string, entry fs.DirEntry) (fs.FileMode, error) {
	if entry.Type()&fs.ModeSymlink == 0 {
		return entry.Type(), nil
	}
	path := filepath.Join(dir, entry.Name())
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("follow %s: %w", path, err)
	}
	return info.Mode(), nil
}

// registeredLogDirs returns the log-path column of databases.txt. A missing
// file registers nothing.
func registeredLogDirs(databases string) ([]string, error) {
	f, err := os.Open(filepath.Join(databases, databasesTxt)) // #nosec G304 -- the database root comes from this process's environment
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", databasesTxt, err)
	}
	defer func() { _ = f.Close() }()
	var dirs []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		dirs = append(dirs, fields[3])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", databasesTxt, err)
	}
	return dirs, nil
}

// probeLock asks the kernel whether another process holds a lock that would
// keep this one from write-locking the whole file.
func probeLock(path string) (bool, int32, error) {
	f, err := os.Open(path) // #nosec G304 -- a file below the database root
	if errors.Is(err, fs.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lock); err != nil {
		return false, 0, fmt.Errorf("query the lock of %s: %w", path, err)
	}
	return lock.Type != syscall.F_UNLCK, lock.Pid, nil
}

// MemberLockFile is the file directly below CUBRID_DATABASES that a member's
// container holds an exclusive lock on for its whole life (#475). Nothing
// removes it.
const MemberLockFile = ".member-lock"

// WaitVolumeFree returns once no other process holds a CUBRID lock on the
// database directories under databases (HeldVolumeLocks): a server, log
// copier or log applier of an earlier container of this member that still
// runs on the volume. It has no time limit; it logs what it waits for at
// once and then every logEvery. A check that fails counts as held. It
// returns ctx's error when ctx ends first.
func WaitVolumeFree(ctx context.Context, databases string, logger *slog.Logger, poll, logEvery time.Duration) error {
	start := time.Now()
	var logged time.Time
	for {
		held, err := HeldVolumeLocks(databases)
		if err == nil && len(held) == 0 {
			logger.Info("Data volume is free of other CUBRID processes", "event", "volume_free",
				"waitedSeconds", int(time.Since(start).Seconds()))
			return nil
		}
		if logged.IsZero() || time.Since(logged) >= logEvery {
			logged = time.Now()
			waited := int(time.Since(start).Seconds())
			if err != nil {
				logger.Warn("Could not check the data volume for other CUBRID processes; waiting",
					"event", "volume_check_failed", "err", err, "waitedSeconds", waited)
			}
			for _, lock := range held {
				logger.Warn("Another process still holds a CUBRID lock on the data volume; waiting",
					"event", "volume_in_use", "file", lock.File, "pid", lock.PID, "waitedSeconds", waited)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
