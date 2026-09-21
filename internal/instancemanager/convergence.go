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
	"regexp"
	"strconv"
)

// ApplyConvergence reports the local replication apply-pipeline facts parsed
// from `cubrid applyinfo` (POC-7/POC-9: HA registration does NOT imply the
// replica is caught up). It is deliberately factual, not policy: the operator
// decides what "acceptable for backup" vs "acceptable for rolling update" means.
type ApplyConvergence struct {
	// Available is false when applyinfo could not be run/parsed (e.g. not a
	// slave, command failed); callers must treat unknown as NOT converged.
	Available bool `json:"available"`
	// InsertCount/CommitCount/FailCount are the applied-info counters.
	InsertCount int `json:"insertCount"`
	CommitCount int `json:"commitCount"`
	FailCount   int `json:"failCount"`
	// DelayedPageCount is the copied-log pages not yet applied (0 = caught up).
	DelayedPageCount int `json:"delayedPageCount"`
	// Reason explains Available=false or a non-converged verdict.
	Reason string `json:"reason,omitempty"`
}

// Converged is a conservative verdict: applyinfo parsed, no apply failures, and
// no delayed pages. Unknown/unavailable is never converged (POC-7 safety).
func (c ApplyConvergence) Converged() bool {
	return c.Available && c.FailCount == 0 && c.DelayedPageCount == 0
}

var (
	reInsertCount  = regexp.MustCompile(`Insert count\s*:\s*(\d+)`)
	reCommitCount  = regexp.MustCompile(`Commit count\s*:\s*(\d+)`)
	reFailCount    = regexp.MustCompile(`Fail count\s*:\s*(\d+)`)
	reDelayedPages = regexp.MustCompile(`Delayed log page count\s*:\s*(\d+)`)
)

// ApplyConvergenceStatus runs `cubrid applyinfo -L <copied-log-path> -a <db>`
// against the copied log of the master and parses the apply-pipeline counters.
// copiedLogPath is the local `<db>_<master>` copy-log directory. A command
// failure or an unparseable "Applied Info" block yields Available=false — never
// a fabricated "converged".
func ApplyConvergenceStatus(ctx context.Context, cli CLI, db, copiedLogPath string) ApplyConvergence {
	out, err := cli.Run(ctx, "cubrid", "applyinfo", "-L", copiedLogPath, "-a", db)
	if err != nil {
		return ApplyConvergence{Available: false, Reason: "applyinfo failed: " + err.Error()}
	}
	return parseApplyConvergence(out)
}

func parseApplyConvergence(out string) ApplyConvergence {
	fail := reFailCount.FindStringSubmatch(out)
	delayed := reDelayedPages.FindStringSubmatch(out)
	// Fail count + Delayed log page count together mark a parsed Applied-Info
	// block; without them the apply pipeline info is not present.
	if fail == nil || delayed == nil {
		return ApplyConvergence{Available: false, Reason: "no Applied Info block in applyinfo output"}
	}
	c := ApplyConvergence{
		Available:        true,
		InsertCount:      atoiField(reInsertCount, out),
		CommitCount:      atoiField(reCommitCount, out),
		FailCount:        mustAtoi(fail[1]),
		DelayedPageCount: mustAtoi(delayed[1]),
	}
	if !c.Converged() {
		c.Reason = "apply pipeline not converged (failCount>0 or delayed pages remain)"
	}
	return c
}

func atoiField(re *regexp.Regexp, out string) int {
	if m := re.FindStringSubmatch(out); m != nil {
		return mustAtoi(m[1])
	}
	return 0
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
