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

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// activeCaptureLimit bounds the capture taken from all members when the
// master does not become active after the bootstrap. It runs only after the
// check has failed.
const activeCaptureLimit = 4 * time.Minute

// activeCaptureScript prints, inside a member's container, what shows why a
// master stays registered_and_to_be_active (#1051): the logs under
// $CUBRID/log (cub_master, server error, and the log copiers and appliers,
// whose file names are in docs/poc/RESULTS.md), a listing of the log
// directories, of the copied logs of each peer ($2...) and of the database
// directory, and the TCP connections. $1 is the database. Every command is
// bounded, and a missing tool or file is printed and does not stop the rest.
const activeCaptureScript = `db=$1; shift
PATH="${CUBRID}/bin:${PATH}"
logs="${CUBRID}/log"
echo "== listing of ${logs} and its server directory"; timeout 15 ls -l --time-style=full-iso "$logs" "$logs/server" 2>&1 | head -n 120
for peer in "$@"; do
  echo "== listing of the log copied from $peer: ${CUBRID_DATABASES}/${db}_${peer}"
  timeout 15 ls -l --time-style=full-iso "${CUBRID_DATABASES}/${db}_${peer}" 2>&1 | head -n 60
done
echo "== listing of ${CUBRID_DATABASES}/${db}"; timeout 15 ls -l --time-style=full-iso "${CUBRID_DATABASES}/${db}" 2>&1 | head -n 60
for f in "$logs"/*_master.err "$logs/server/${db}_latest.err" "$logs"/*copylogdb.err "$logs"/*applylogdb*.err; do
  echo "== tail of $f"
  [ -f "$f" ] || { echo "(no file)"; continue; }
  tail -n 80 "$f" 2>&1 | cut -c1-400
done
echo "== TCP connections"
if command -v ss >/dev/null 2>&1; then timeout 15 ss -tnp 2>&1 | head -n 80
else echo "(ss is not in the image; /proc/net/tcp follows)"; timeout 15 cat /proc/net/tcp 2>&1 | head -n 80; fi
true`

// captureActiveWait records, for each member, the output of
// memberReplicationScript (heartbeat status, applyinfo for the log copied from
// each peer) and activeCaptureScript, and returns one text per member, to be
// written as ha-bootstrap/master-not-active-<member>.txt. Every kubectl call
// is bounded by the context and by 20 s per API request. A failed call records
// its error beside the output and the capture goes on, so that it cannot hide
// the failure that started it.
func captureActiveWait(spec context.Context, namespace, database string, members []string) map[string]string {
	ctx, cancel := context.WithTimeout(spec, activeCaptureLimit)
	defer cancel()
	run := func(member, script string, args ...string) string {
		out, err := utils.Run(exec.CommandContext(ctx, "kubectl", append([]string{
			"--request-timeout=20s", "-n", namespace, kubectlExec, member, "-c", "cubrid", "--",
			"bash", "-c", script, "capture", database}, args...)...))
		if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
			err = wrapped.Unwrap()
		}
		return fmt.Sprintf("(err %v)\n%s", err, out)
	}
	texts := map[string]string{}
	for _, member := range members {
		var peers []string
		for _, m := range members {
			if m != member {
				peers = append(peers, m)
			}
		}
		var b strings.Builder
		_, _ = fmt.Fprintf(&b, "Member %s, taken at %s\n", member, time.Now().UTC().Format(time.RFC3339))
		b.WriteString(run(member, memberReplicationScript, peers...))
		b.WriteString("\n")
		b.WriteString(run(member, activeCaptureScript, peers...))
		texts[member] = b.String()
	}
	return texts
}
