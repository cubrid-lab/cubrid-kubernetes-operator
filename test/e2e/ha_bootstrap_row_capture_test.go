//go:build e2e
// +build e2e

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
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// rowCaptureLimit bounds the capture taken when a slave does not receive the
// row written on the master: both readings of the slave, the control and the
// wait between the readings fit in it. The capture runs only after the check
// has failed, so a passing run never waits for it.
const rowCaptureLimit = 3 * time.Minute

// rowCaptureGap is the wait between the two readings of the slave that lacks
// the row. A copy that moves while the applier stands still, a copy that has
// stopped, and an applier that is late but advancing look the same in one
// reading and differ between two (#416).
const rowCaptureGap = 60 * time.Second

// applierLockScript prints the database's applier lock file under
// $CUBRID/var/APPLYLOGDB, which one log applier holds at a time (POC-16,
// POC-17, POC-21): its size and time, its content, the kernel locks on it,
// and each applylogdb process with whether it holds the file open. $1 is the
// database. A missing file is printed as such.
const applierLockScript = `L="${CUBRID}/var/APPLYLOGDB/$1"; echo "== applier lock $L"; i=
if [ -f "$L" ]; then ls -l --time-style=full-iso "$L"; echo "content:"; head -c 400 "$L"; echo; i=$(stat -L -c %i "$L" 2>/dev/null); else echo "(no file)"; fi
echo "== kernel locks on the file"
if [ -n "$i" ]; then grep -E ":$i " /proc/locks || echo "(none)"; else echo "(none)"; fi
echo "== applylogdb processes, and whether each holds the lock file open"; n=0
for p in /proc/[0-9]*; do c=$( { tr '\0' ' ' < "$p/cmdline"; } 2>/dev/null); case "$c" in "cub_admin applylogdb"*)
  h=no; for fd in "$p"/fd/*; do [ -n "$i" ] && [ "$(stat -L -c %i "$fd" 2>/dev/null)" = "$i" ] && h=yes; done
  echo "${p#/proc/} holds=$h $c"; n=$((n+1));; esac; done
[ "$n" -eq 0 ] && echo "(none)"; true`

// captureMissingRow writes down what a slave on which the row could not be
// read reports: what read says of that slave, then, after gap, again, and
// last of the control slave, each with its member and UTC time. lastError is
// why the read failed: the row may be missing, or the read itself may have
// failed (a lost csql session, a kubectl error). The control comes last, and
// the wait is taken only if the time left covers it and a second reading as
// long as the first, so that the second reading is not cut short by the
// context's deadline.
func captureMissingRow(ctx context.Context, lacking, control, lastError string, gap time.Duration,
	now func() time.Time, wait func(context.Context, time.Duration), read func(context.Context, string) string) string {
	var b strings.Builder
	reading := func(label, member string) time.Duration {
		started := now()
		_, _ = fmt.Fprintf(&b, "\n##### %s: member %s, taken at %s\n%s\n",
			label, member, started.UTC().Format(time.RFC3339Nano), read(ctx, member))
		return now().Sub(started)
	}
	_, _ = fmt.Fprintf(&b, "The row could not be read on %s within 3m; last error: %s\nControl: %s.\n",
		lacking, lastError, control)
	took := reading("first reading of the slave", lacking)
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > gap+took {
		wait(ctx, gap)
		reading("second reading of the slave", lacking)
	} else {
		_, _ = fmt.Fprintf(&b, "\nThe %s wait and the second reading of %s were skipped: the first reading took %s and too little of the capture limit was left.\n",
			gap, lacking, took)
	}
	reading("control slave", control)
	return b.String()
}

// captureFromCluster is captureMissingRow on the members of a cluster, with
// memberReplicationScript and applierLockScript run by kubectl exec in the
// cubrid container. The lock reading is a copy of appliersScript in
// test/installed/s12.go (another package); change them together. A failed
// call records its error beside the output.
func captureFromCluster(spec context.Context, namespace, database, lacking, control, lastError string, members []string) string {
	ctx, cancel := context.WithTimeout(spec, rowCaptureLimit)
	defer cancel()
	run := func(ctx context.Context, member, script string, args ...string) string {
		out, err := utils.Run(exec.CommandContext(ctx, "kubectl", append([]string{
			"--request-timeout=20s", "-n", namespace, kubectlExec, member, "-c", "cubrid", "--",
			"bash", "-c", script, "capture", database}, args...)...))
		// utils.Run wraps the exit error with the command and its output,
		// which are recorded once (as replicationState in s03_test.go does).
		if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
			err = wrapped.Unwrap()
		}
		return fmt.Sprintf("(err %v)\n%s", err, out)
	}
	return captureMissingRow(ctx, lacking, control, lastError, rowCaptureGap, time.Now,
		func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
		func(ctx context.Context, member string) string {
			return run(ctx, member, memberReplicationScript, members...) + "\n" + run(ctx, member, applierLockScript)
		})
}

// rowCaptureFailure is a Gomega whose failure first runs capture, which is
// given the failure message, and then fails the spec with that message
// unchanged. The capture does not decide the verdict: the failure is
// deferred first, so it is reported also if the capture panics.
func rowCaptureFailure(capture func(message string)) types.Gomega {
	return NewGomega(func(message string, skip ...int) {
		offset := 1
		if len(skip) > 0 {
			offset += skip[0]
		}
		defer Fail(message, offset)
		capture(message)
	})
}
