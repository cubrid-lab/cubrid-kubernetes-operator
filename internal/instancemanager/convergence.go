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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	// AppliedChanges is the sum of the insert, update, delete and schema
	// counters: it rises whenever the applier applies anything.
	AppliedChanges int `json:"appliedChanges"`
	// DelayedPageCount is the copied-log pages not yet applied (0 = caught up).
	DelayedPageCount int `json:"delayedPageCount"`
	// Source is the member whose log these facts are about; set by the
	// caller that chose the log.
	Source string `json:"source,omitempty"`
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
	reUpdateCount  = regexp.MustCompile(`Update count\s*:\s*(\d+)`)
	reDeleteCount  = regexp.MustCompile(`Delete count\s*:\s*(\d+)`)
	reSchemaCount  = regexp.MustCompile(`Schema count\s*:\s*(\d+)`)
	reCommitCount  = regexp.MustCompile(`Commit count\s*:\s*(\d+)`)
	reFailCount    = regexp.MustCompile(`Fail count\s*:\s*(\d+)`)
	reDelayedPages = regexp.MustCompile(`Delayed log page count\s*:\s*(\d+)`)
)

// applyDelayHeading starts the part of the output about pages that were
// copied and are not applied yet. With -r the output has a part of the same
// form before it, about pages that are not copied yet (POC-20).
const applyDelayHeading = "Delay in Applying Copied Log"

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
	var delayed []string
	if _, applying, found := strings.Cut(out, applyDelayHeading); found {
		delayed = reDelayedPages.FindStringSubmatch(applying)
	}
	// Fail count + Delayed log page count together mark a parsed Applied-Info
	// block; without them the apply pipeline info is not present.
	if fail == nil || delayed == nil {
		return ApplyConvergence{Available: false, Reason: "no Applied Info block in applyinfo output"}
	}
	c := ApplyConvergence{
		Available:   true,
		InsertCount: atoiField(reInsertCount, out),
		CommitCount: atoiField(reCommitCount, out),
		AppliedChanges: atoiField(reInsertCount, out) + atoiField(reUpdateCount, out) +
			atoiField(reDeleteCount, out) + atoiField(reSchemaCount, out),
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

// replicationOf returns what a slave's applier reports for the log of the
// master its node list names, or nil when st is not a slave's status or the
// member was not told its database. A node list without exactly one master
// gives no log to ask about, and says so.
func replicationOf(ctx context.Context, cli CLI, st HAStatus, database, databasesDir string) *ApplyConvergence {
	if st.Role != RoleSlave || database == "" || databasesDir == "" {
		return nil
	}
	master := ""
	masters := 0
	for _, n := range st.Nodes {
		if n.State == string(RoleMaster) {
			masters++
			master = n.Name
		}
	}
	if masters != 1 {
		return &ApplyConvergence{Reason: "the node list names " + strconv.Itoa(masters) + " masters, not one"}
	}
	// The name becomes part of a path: accept only a host label.
	if !hostLabelPattern.MatchString(master) {
		return &ApplyConvergence{Reason: "the master's name in the node list is not a host label"}
	}
	c := ApplyConvergenceStatus(ctx, cli, database, filepath.Join(databasesDir, database+"_"+master))
	if !c.Available {
		switch err := ctx.Err(); {
		case errors.Is(err, context.Canceled):
			c.Reason = "applyinfo was not read: the request was canceled"
		case errors.Is(err, context.DeadlineExceeded):
			c.Reason = "applyinfo did not answer in time"
		}
	}
	c.Source = master
	return &c
}
