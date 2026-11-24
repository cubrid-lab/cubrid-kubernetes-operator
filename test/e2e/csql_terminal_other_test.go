//go:build e2e && !linux
// +build e2e,!linux

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
	"errors"
	"os"
)

// openTestTerminal is implemented on Linux only, where the e2e suite runs.
func openTestTerminal() (ptmx, pts *os.File, err error) {
	return nil, nil, errors.New("a pseudo-terminal for kubectl exec -it is implemented on Linux only")
}
