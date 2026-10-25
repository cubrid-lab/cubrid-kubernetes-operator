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
	"os"
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
	// Applier identifies the applier process the counters were read beside:
	// its process ID and start time. The counters outlast the process
	// (docs/poc/RESULTS.md, POC-30), so only this tells a restarted applier
	// from one that kept running. The value is opaque and is only compared
	// for equality. Empty when the process or its start time was not found.
	Applier string `json:"applier,omitempty"`
	// FinalLSA is the applier's position in the copied log ("Final LSA") and
	// CopiedEOF the end of the copied log ("EOF LSA" of "Copied Active
	// Info"), both printed by applyinfo with -v. On a healthy slave FinalLSA
	// follows the copied log also through a transaction that stays open on
	// the master; on a stuck applier it stops while CopiedEOF goes on
	// (docs/poc/RESULTS.md, POC-37). Nil when not printed.
	FinalLSA  *LSA `json:"finalLSA,omitempty"`
	CopiedEOF *LSA `json:"copiedEOF,omitempty"`
	// Reason explains Available=false or a non-converged verdict.
	Reason string `json:"reason,omitempty"`
}

// LSA is a CUBRID log sequence address: a log page and an offset in it.
// Addresses compare by page, then offset.
type LSA struct {
	Page   int64 `json:"page"`
	Offset int64 `json:"offset"`
}

// Before reports whether a comes before b in the log.
func (a LSA) Before(b LSA) bool {
	return a.Page < b.Page || (a.Page == b.Page && a.Offset < b.Offset)
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
	reFinalLSA     = regexp.MustCompile(`(?m)^Final LSA\s*:\s*(\d+)\s*\|\s*(\d+)`)
	reEOFLSA       = regexp.MustCompile(`(?m)^EOF LSA\s*:\s*(\d+)\s*\|\s*(\d+)`)
)

// copiedActiveHeading starts the part of the output about the copied log
// itself. The EOF LSA is read only after it, besides the anchored expression,
// so that no "EOF LSA" of another part can be taken for the copied log's.
const copiedActiveHeading = "Copied Active Info"

// applyDelayHeading starts the part of the output about pages that were
// copied and are not applied yet. With -r the output has a part of the same
// form before it, about pages that are not copied yet (POC-20).
const applyDelayHeading = "Delay in Applying Copied Log"

// ApplyConvergenceStatus runs `cubrid applyinfo -L <copied-log-path> -a -v <db>`
// against the copied log of the master and parses the apply-pipeline counters.
// copiedLogPath is the local `<db>_<master>` copy-log directory. A command
// failure or an unparseable "Applied Info" block yields Available=false — never
// a fabricated "converged".
func ApplyConvergenceStatus(ctx context.Context, cli CLI, db, copiedLogPath string) ApplyConvergence {
	out, err := cli.Run(ctx, "cubrid", "applyinfo", "-L", copiedLogPath, "-a", "-v", db)
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
	c.FinalLSA = lsaField(reFinalLSA, out)
	if _, copied, found := strings.Cut(out, copiedActiveHeading); found {
		c.CopiedEOF = lsaField(reEOFLSA, copied)
	}
	if !c.Converged() {
		c.Reason = "apply pipeline not converged (failCount>0 or delayed pages remain)"
	}
	return c
}

// lsaField returns the address the expression finds, or nil.
func lsaField(re *regexp.Regexp, out string) *LSA {
	m := re.FindStringSubmatch(out)
	if m == nil {
		return nil
	}
	page, err1 := strconv.ParseInt(m[1], 10, 64)
	offset, err2 := strconv.ParseInt(m[2], 10, 64)
	if err1 != nil || err2 != nil {
		return nil
	}
	return &LSA{Page: page, Offset: offset}
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

// processStarted reports when the process with the given ID was started, as
// the kernel counts it: clock ticks since boot, field 22 of /proc/<pid>/stat.
// The value stays the same for as long as the process lives.
var processStarted = func(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	return startTicks(string(data))
}

// startTicks takes the start time out of the content of /proc/<pid>/stat.
// The process name, field 2, is in parentheses and may hold spaces and
// parentheses itself, so the fields are counted from the last ")".
func startTicks(stat string) (string, bool) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", false
	}
	// After the name come fields 3, 4, ...; the start time is field 22.
	fields := strings.Fields(stat[end+1:])
	const startTime = 22 - 3
	if len(fields) <= startTime {
		return "", false
	}
	if _, err := strconv.ParseUint(fields[startTime], 10, 64); err != nil {
		return "", false
	}
	return fields[startTime], true
}

// applierOf returns the applier process heartbeat lists for the copied log
// at logPath. The paths are compared after filepath.Clean, so a trailing or
// doubled separator in either does not hide the process.
func applierOf(st HAStatus, logPath string) (ApplierProcess, bool) {
	want := filepath.Clean(logPath)
	for _, a := range st.Appliers {
		if a.PID > 0 && filepath.Clean(a.LogPath) == want {
			return a, true
		}
	}
	return ApplierProcess{}, false
}

// applierIdentity names the applier process of the copied log at logPath:
// its process ID and its start time, which tells it from a later process
// that was given the same ID. Empty when heartbeat lists no applier for that
// log, and also when the start time cannot be read: an identity without it
// would differ from the one read before and look like a restart.
func applierIdentity(st HAStatus, logPath string) string {
	a, ok := applierOf(st, logPath)
	if !ok {
		return ""
	}
	started, ok := processStarted(a.PID)
	if !ok {
		return ""
	}
	return "pid:" + strconv.Itoa(a.PID) + ":" + started
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
	logPath := filepath.Join(databasesDir, database+"_"+master)
	c := ApplyConvergenceStatus(ctx, cli, database, logPath)
	if !c.Available {
		switch err := ctx.Err(); {
		case errors.Is(err, context.Canceled):
			c.Reason = "applyinfo was not read: the request was canceled"
		case errors.Is(err, context.DeadlineExceeded):
			c.Reason = "applyinfo did not answer in time"
		}
	}
	c.Source = master
	c.Applier = applierIdentity(st, logPath)
	return &c
}
