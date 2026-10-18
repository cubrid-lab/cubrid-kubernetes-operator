//go:build linux

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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// fOFDSetlk is Linux's F_OFD_SETLK (golang.org/x/sys/unix): a lock owned by
// the open file description rather than by the process.
const fOFDSetlk = 37

// AcquireMemberLock takes an exclusive write lock over the whole of lock,
// which must be the file at path, and returns once it holds it. The lock is
// an open file description lock: it belongs to the description lock was
// opened with, so it stays held after this process exits for as long as
// another process (the entrypoint, PID 1, which opened the file and passed
// the descriptor on, and the children that inherit it) keeps the file open,
// and the kernel drops it when the last of them ends, also after a SIGKILL
// (docs/poc/RESULTS.md, POC-31). Closing another descriptor of the file does
// not drop it, unlike a POSIX lock. While another container of the member
// holds it, AcquireMemberLock waits without a time limit and logs
// member_lock_busy at once and then every logEvery. It never breaks or
// removes the other holder's lock. A descriptor that is not the file at path,
// or a lock that fails other than by being held, is an error, and so is
// ctx ending first: none of them is ever taken for the lock.
func AcquireMemberLock(ctx context.Context, lock *os.File, path string, logger *slog.Logger, poll, logEvery time.Duration) error {
	got, err := lock.Stat()
	if err != nil {
		return fmt.Errorf("the member lock descriptor: %w", err)
	}
	want, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("the member lock file: %w", err)
	}
	if !os.SameFile(got, want) {
		return fmt.Errorf("the member lock descriptor is not %s", path)
	}
	start := time.Now()
	var logged time.Time
	for {
		flock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
		err := syscall.FcntlFlock(lock.Fd(), fOFDSetlk, &flock)
		if err == nil {
			// A lock file replaced or removed during the wait is one no other
			// container opens: holding a lock on it holds nothing.
			if now, err := os.Stat(path); err != nil || !os.SameFile(got, now) {
				return fmt.Errorf("the member lock file %s was replaced or removed while waiting for it", path)
			}
			logger.Info("Took the member lock on the data volume", "event", "member_lock_acquired",
				"file", path, "waitedSeconds", int(time.Since(start).Seconds()))
			return nil
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EACCES) {
			return fmt.Errorf("lock %s: %w", path, err)
		}
		if logged.IsZero() || time.Since(logged) >= logEvery {
			logged = time.Now()
			logger.Warn("Another container of this member still holds the member lock on the data volume; waiting",
				"event", "member_lock_busy", "file", path, "waitedSeconds", int(time.Since(start).Seconds()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
