//go:build !linux

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
	"log/slog"
	"os"
	"time"
)

// AcquireMemberLock needs Linux's open file description locks; the member
// lock is never taken elsewhere, so the wait fails closed and nothing starts.
func AcquireMemberLock(_ context.Context, _ *os.File, _ string, _ *slog.Logger, _, _ time.Duration) error {
	return errors.New("the member lock needs open file description locks, which only Linux has")
}
