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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// maxTerminalOutput bounds what RunOnTerminal keeps of a command's output
// after its prompt; the rest is dropped.
const maxTerminalOutput = 64 << 10

// terminalDrain bounds how long RunOnTerminal reads what a command printed
// after it ended.
const terminalDrain = time.Second

// RunOnTerminal runs a command on a new pseudo-terminal, which becomes the
// controlling terminal of a new session the command leads, and types input
// and a line feed once prompt appears in its output. Echo is off on the
// terminal, so it does not print back what is typed; a command can turn it on
// again, and its output must be treated as holding the input. input is
// written to the terminal only, never to an argv or the environment.
//
// The context bounds the whole run, the wait for the prompt included; without
// a deadline the CLI's default applies, as for Run. The command, and every
// process of its session, are killed when the context ends, and when the
// command ends leaving any behind, and RunOnTerminal returns only once none is
// left: no process of it holds the database or the terminal afterwards.
func (c ExecCLI) RunOnTerminal(ctx context.Context, prompt, input, name string, args ...string) (string, bool, error) {
	if _, ok := ctx.Deadline(); !ok {
		to := c.Timeout
		if to <= 0 {
			to = 10 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	ptmx, pts, err := openTerminal()
	if err != nil {
		return "", false, err
	}
	defer func() { _ = ptmx.Close() }()

	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	// Setsid makes the command lead a new session and process group, whose
	// controlling terminal is its stdin, the pseudo-terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err = cmd.Start()
	// The manager keeps only the master side: once the command and its
	// session end, reading it ends.
	_ = pts.Close()
	if err != nil {
		return "", false, fmt.Errorf("start %s on a terminal: %w", name, err)
	}
	pgid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	reader := newPromptReader(prompt)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		reader.readFrom(ptmx)
	}()

	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// end kills what is left of the command's session, waits until none of
	// it is left, and reads what it printed until then.
	end := func() {
		endGroup(logger, pgid)
		_ = ptmx.SetReadDeadline(time.Now().Add(terminalDrain))
		<-readDone
	}

	var runErr error
	select {
	case <-reader.prompted:
	case runErr = <-exited:
		end()
		return "", false, runErr
	case <-ctx.Done():
		endGroup(logger, pgid)
		<-exited
		end()
		return "", false, ctx.Err()
	}
	if _, err := ptmx.Write([]byte(input + "\n")); err != nil {
		endGroup(logger, pgid)
		<-exited
		end()
		return "", true, fmt.Errorf("type at the prompt of %s: %w", name, err)
	}
	select {
	case runErr = <-exited:
	case <-ctx.Done():
		endGroup(logger, pgid)
		<-exited
		runErr = ctx.Err()
	}
	end()
	return reader.after(), true, runErr
}

// openTerminal opens a new pseudo-terminal: its master side, which the
// manager reads and writes, and its slave side, which the command gets as
// stdin, stdout and stderr. Neither becomes the manager's controlling
// terminal. Echo is turned off on the slave side.
func openTerminal() (ptmx, pts *os.File, err error) {
	ptmx, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open a pseudo-terminal: %w", err)
	}
	defer func() {
		if err != nil {
			_ = ptmx.Close()
		}
	}()
	// The descriptor is used through SyscallConn: Fd would make it blocking,
	// and the read deadline that bounds the drain would no longer apply.
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
	termios, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err == nil {
		termios.Lflag &^= unix.ECHO | unix.ECHONL
		err = unix.IoctlSetTermios(fd, unix.TCSETS, termios)
	}
	if err != nil {
		_ = pts.Close()
		return nil, nil, fmt.Errorf("turn off echo on %s: %w", path, err)
	}
	return ptmx, pts, nil
}

// promptReader reads a command's terminal output, signals prompted once the
// prompt appears, and keeps up to maxTerminalOutput bytes of what follows it.
type promptReader struct {
	prompt   []byte
	prompted chan struct{}

	mu     sync.Mutex
	before []byte
	seen   bool
	rest   []byte
}

func newPromptReader(prompt string) *promptReader {
	return &promptReader{prompt: []byte(prompt), prompted: make(chan struct{})}
}

// readFrom reads f until it fails: the command's session ended (EIO), the
// read deadline passed, or f was closed.
func (r *promptReader) readFrom(f *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			r.add(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (r *promptReader) add(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen {
		r.rest = appendBounded(r.rest, chunk)
		return
	}
	r.before = append(r.before, chunk...)
	if i := bytes.Index(r.before, r.prompt); i >= 0 {
		r.seen = true
		r.rest = appendBounded(nil, r.before[i+len(r.prompt):])
		r.before = nil
		close(r.prompted)
		return
	}
	// Only the end that could still begin the prompt is kept.
	if keep := len(r.prompt) - 1; len(r.before) > keep {
		r.before = append(r.before[:0], r.before[len(r.before)-keep:]...)
	}
}

// after returns what the command printed after the prompt.
func (r *promptReader) after() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.rest)
}

// appendBounded appends chunk to buf up to maxTerminalOutput bytes.
func appendBounded(buf, chunk []byte) []byte {
	if room := maxTerminalOutput - len(buf); room < len(chunk) {
		chunk = chunk[:max(room, 0)]
	}
	return append(buf, chunk...)
}
