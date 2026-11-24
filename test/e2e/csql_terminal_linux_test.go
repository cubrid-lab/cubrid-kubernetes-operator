//go:build e2e && linux
// +build e2e,linux

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

package e2e

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openTestTerminal opens a new pseudo-terminal for csqlOnTerminal: its master
// side, which the test reads and writes, and its slave side, which `kubectl
// exec -it` gets as stdin, stdout and stderr, so that kubectl allocates a
// terminal in the container. It is the Instance Manager's openTerminal
// (internal/instancemanager/terminal_linux.go) with a window size, so that
// csql's line editor in the container gets a usable width. Echo is turned off,
// so nothing typed is printed back before kubectl puts the terminal in raw
// mode.
func openTestTerminal() (ptmx, pts *os.File, err error) {
	ptmx, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open a pseudo-terminal: %w", err)
	}
	defer func() {
		if err != nil {
			_ = ptmx.Close()
		}
	}()
	raw, err := ptmx.SyscallConn()
	if err != nil {
		return nil, nil, fmt.Errorf("open a pseudo-terminal: %w", err)
	}
	var number uint32
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		if ioctlErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ioctlErr != nil {
			return
		}
		number, ioctlErr = unix.IoctlGetUint32(int(fd), unix.TIOCGPTN)
	}); err != nil {
		return nil, nil, fmt.Errorf("unlock the pseudo-terminal: %w", err)
	}
	if ioctlErr != nil {
		return nil, nil, fmt.Errorf("unlock the pseudo-terminal: %w", ioctlErr)
	}
	path := "/dev/pts/" + strconv.FormatUint(uint64(number), 10)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	pts = os.NewFile(uintptr(fd), path)
	err = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 50, Col: 200})
	if err == nil {
		var termios *unix.Termios
		if termios, err = unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
			termios.Lflag &^= unix.ECHO | unix.ECHONL
			err = unix.IoctlSetTermios(fd, unix.TCSETS, termios)
		}
	}
	if err != nil {
		_ = pts.Close()
		return nil, nil, fmt.Errorf("set up %s: %w", path, err)
	}
	return ptmx, pts, nil
}
