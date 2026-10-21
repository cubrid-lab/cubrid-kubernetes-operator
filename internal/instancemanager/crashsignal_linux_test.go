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
	"syscall"
	"unsafe"
)

// defaultFileSizeSignal gives SIGXFSZ its default action, so that the
// process ends inside the write that passes its file size limit, before any
// code after the write runs. The Go runtime keeps its own handler for the
// signal, which os/signal cannot remove; the action is therefore set
// directly. An all-zero sigaction is SIG_DFL.
func defaultFileSizeSignal() error {
	var dfl [4]uint64
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_RT_SIGACTION, uintptr(syscall.SIGXFSZ),
		uintptr(unsafe.Pointer(&dfl)), 0, 8, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
