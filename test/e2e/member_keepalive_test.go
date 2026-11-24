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
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// memberKeepaliveLimitSeconds is the keepalive idle time the Operator sets
// on every member Pod (#982); a log copy connection's keepalive timer never
// has more time left than that.
const memberKeepaliveLimitSeconds = 30

// ssMissing is what memberSocketsScript prints when the image has no ss.
const ssMissing = "NO-SS"

// memberSocketsScript lists a member's established TCP connections to port
// 1523 of another host: the log copy (copylogdb) of each other member's log.
// The official image has ss in /usr/sbin, which kubectl exec's PATH may lack.
const memberSocketsScript = `PATH="$PATH:/usr/sbin:/sbin"
command -v ss >/dev/null 2>&1 || { echo ` + ssMissing + `; exit 0; }
ss -tnoH state established '( dport = :1523 )'`

// ssKeepaliveTimer is the keepalive timer ss -o prints, with the time left
// as [<m>min][<s>sec | <s>.<ms>sec | <ms>ms] (iproute2 6.11 and later) or
// <s>.<ms>ms for seconds (earlier releases).
var ssKeepaliveTimer = regexp.MustCompile(`timer:\(keepalive,(-?)(?:(\d+)min)?(?:(\d+)(\.\d+)?(sec|ms))?,\d+\)`)

// memberKeepaliveCheck reads `ss -tnoH state established` of a member and
// returns how many connections to another host show a keepalive timer, and
// a problem for each timer with more than memberKeepaliveLimitSeconds left.
// A connection to a loopback address is the member's own, and one without a
// keepalive timer is sending (its retransmission timer is shown instead):
// neither is counted.
func memberKeepaliveCheck(out string) (int, []string) {
	counted := 0
	var problems []string
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		peer, err := netip.ParseAddrPort(fields[3])
		if err == nil && peer.Addr().Unmap().IsLoopback() {
			continue
		}
		m := ssKeepaliveTimer.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		counted++
		seconds := 0
		if m[1] == "" {
			minutes, _ := strconv.Atoi(m[2])
			n, _ := strconv.Atoi(m[3])
			// Whole milliseconds without a fraction are under a second.
			if m[5] == "ms" && m[4] == "" {
				n = 0
			}
			seconds = minutes*60 + n
		}
		if seconds > memberKeepaliveLimitSeconds {
			problems = append(problems, fmt.Sprintf("the connection to %s has a keepalive timer of %ds, more than %ds: %s",
				fields[3], seconds, memberKeepaliveLimitSeconds, strings.TrimSpace(line)))
		}
	}
	return counted, problems
}
