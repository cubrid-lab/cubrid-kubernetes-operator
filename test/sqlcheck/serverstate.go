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

package sqlcheck

import "strings"

// What the Kind suite and the installed harness read from a member when csql
// printed the pair of SessionLost (#238), so that a server restart can be told
// from a client-side hang-up. Both read it the same way.

const (
	// ServerLogTailLines and ServerLogLineWidth bound the log lines kept with
	// a record: at most 40 lines of 400 characters each.
	ServerLogTailLines = 40
	ServerLogLineWidth = 400
	// ServerLogTailScript prints the last ServerLogTailLines lines of the
	// newest server error log of database $0, in the member's container.
	ServerLogTailScript = `f=$(cd "$CUBRID/log/server" 2>/dev/null && ls -1t -- "$0".err "$0"_*.err 2>/dev/null | head -n 1); ` +
		`[ -n "$f" ] || { echo "(no server error log)"; exit 0; }; ` +
		`timeout 5 tail -n 40 -- "$CUBRID/log/server/$f"`
	// ServerAgeScript prints the member's time and uptime from one moment,
	// then the identity script, then the clock rate, from which the server's
	// age follows. The time comes first so that nothing slow runs before it.
	ServerAgeScript = `echo "now $(date +%s.%N 2>/dev/null)"; echo "uptime $(cut -d' ' -f1 /proc/uptime 2>/dev/null)"; ` +
		HAServerIdentityScript + `; echo "clk_tck $(getconf CLK_TCK 2>/dev/null)"`
)

// Cut returns s cut at width characters.
func Cut(s string, width int) string {
	if r := []rune(s); len(r) > width {
		return string(r[:width])
	}
	return s
}

// BoundLines splits out into trimmed non-empty lines, keeps the last limit of
// them, and cuts each at width characters.
func BoundLines(out string, limit, width int) []string {
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for i, line := range lines {
		lines[i] = Cut(line, width)
	}
	return lines
}
