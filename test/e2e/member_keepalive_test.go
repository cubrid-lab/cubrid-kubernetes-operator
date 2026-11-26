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

// memberKeepaliveSysctls are the sysctls the Operator sets on every member
// Pod (#982) and the values the member's network namespace must show: the
// idle time, the interval between probes and the number of probes. Together
// they end a connection of a silent peer after about a minute.
var memberKeepaliveSysctls = []struct {
	file  string
	value int
}{
	{"tcp_keepalive_time", memberKeepaliveLimitSeconds},
	{"tcp_keepalive_intvl", 10},
	{"tcp_keepalive_probes", 3},
}

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
		// The time left, in milliseconds, so that 30.999sec is not read as
		// 30sec. A timer that is overdue has none left.
		left := 0
		if m[1] == "" {
			minutes, _ := strconv.Atoi(m[2])
			n, _ := strconv.Atoi(m[3])
			fraction := 0
			if m[4] != "" {
				digits := (m[4][1:] + "000")[:3]
				fraction, _ = strconv.Atoi(digits)
			}
			switch {
			case m[5] == "ms" && m[4] == "":
				// Whole milliseconds without a fraction are under a second.
				left = minutes*60000 + n
			default:
				// Seconds: "<s>sec", "<s>.<ms>sec", or, before iproute2
				// 6.11, "<s>.<ms>ms".
				left = minutes*60000 + n*1000 + fraction
			}
		}
		if left > memberKeepaliveLimitSeconds*1000 {
			problems = append(problems, fmt.Sprintf("the connection to %s has a keepalive timer of %d.%03ds, more than %ds: %s",
				fields[3], left/1000, left%1000, memberKeepaliveLimitSeconds, strings.TrimSpace(line)))
		}
	}
	return counted, problems
}
