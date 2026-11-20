//go:build e2e

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
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

const (
	// identityTimeout bounds one read of cub_server's identity.
	identityTimeout = 10 * time.Second
	// identitySettle is the wait after a pair before that read. In POC-55 a
	// server killed 0.001 s before the read still showed as unchanged.
	identitySettle = 500 * time.Millisecond

	// lostSessionsFile holds one JSON line for each time csql printed the pair.
	lostSessionsFile = "suite/csql-lost-sessions.jsonl"
	// lostSessionsMeasurement is the count of them in a scenario's result. It
	// is unknown when the scenario made no read and printed no pair: a write
	// alone is not counted as a chance to see one, because the decision rests on
	// reads, so a scenario that only wrote and printed none says unknown.
	lostSessionsMeasurement = "csqlLostSessions"
	// reReadIsThisCall is recorded for the pair of a read made once more.
	reReadIsThisCall = "this call was the read made once more"
	// reReadNeverWrite is recorded for the pair of a statement that is not
	// repeated, which a separate check then decides.
	reReadNeverWrite = "not made: it is not repeated, and a separate check of the rows decides"
	// reReadRefusedWrite is recorded for the pair of a write a member must
	// refuse, which the -581 before it and the identity comparison decide.
	reReadRefusedWrite = "not made: a write is not repeated; the -581 refusal before the pair and the identity decide"
)

// lostSessionRecord is one time csql printed -199 and -111 in this run.
type lostSessionRecord struct {
	At     time.Time `json:"at"`
	Step   string    `json:"step"`
	Member string    `json:"member"`
	// Query is the first line of what csql ran.
	Query string `json:"query"`
	// Kind is "read" for one SELECT and "write" for anything else.
	Kind string `json:"kind"`
	// Identity is what the comparison of cub_server's identity found. A retried
	// read does not pay for that comparison and stays "not read".
	Identity sqlcheck.IdentityResult `json:"identity"`
	// ReRead says how the read made once more answered.
	ReRead string `json:"reRead"`
	// ServerAtPair is what the member printed in the background soon after the
	// pair: "now <epoch seconds>" and "uptime <seconds>" of the member, read
	// together first, then cub_server's identity ("cub_server <pid> <start
	// time in clock ticks since boot>" and "ha_mode <mode>"), then "clk_tck
	// <ticks per second>". It is one reading, not a comparison with the start
	// of the step: csql calls are made from many places that know nothing of a
	// step's start. The server's age at "now" is uptime - start/clk_tck. If it
	// is less than now - At, the server started after the pair, so it
	// restarted. A restart within the latency of the exec (about 0.2 s) after
	// the pair can still be missed. "ha_mode ?" is expected on a standalone
	// member, which has no HA mode.
	ServerAtPair []string `json:"serverAtPair,omitempty"`
	// ServerReadAt is when ServerAtPair was read.
	ServerReadAt time.Time `json:"serverReadAt,omitzero"`
	// ServerLogTail is the last lines of the member's newest server error log,
	// read at ServerReadAt, to compare with the pair's time in At.
	ServerLogTail []string `json:"serverLogTail,omitempty"`
	// ServerReadError says why the fields above are missing or partial.
	ServerReadError string `json:"serverReadError,omitempty"`
}

const (
	// serverLogTailLines and serverLogLineWidth bound the log lines kept with a
	// record: at most 40 lines of 400 characters each.
	serverLogTailLines = 40
	serverLogLineWidth = 400
	// serverLogTailScript prints the last serverLogTailLines lines of the
	// newest server error log of database $0, in the member's container.
	serverLogTailScript = `f=$(cd "$CUBRID/log/server" 2>/dev/null && ls -1t -- "$0".err "$0"_*.err 2>/dev/null | head -n 1); ` +
		`[ -n "$f" ] || { echo "(no server error log)"; exit 0; }; ` +
		`timeout 5 tail -n 40 -- "$CUBRID/log/server/$f"`
	// serverAgeScript prints the member's time and uptime from one moment, then
	// the identity script, then the clock rate, from which the server's age
	// follows. The time comes first so that nothing slow runs before it.
	serverAgeScript = `echo "now $(date +%s.%N 2>/dev/null)"; echo "uptime $(cut -d' ' -f1 /proc/uptime 2>/dev/null)"; ` +
		sqlcheck.HAServerIdentityScript + `; echo "clk_tck $(getconf CLK_TCK 2>/dev/null)"`
	// pairCaptureWait bounds the wait for reads still running when the suite
	// ends. It stays well inside the grace period of the suite (1 minute), so
	// that the verdict of concludeRun is not lost.
	pairCaptureWait = 8 * time.Second
)

// pairCapture reads a member's server state when csql printed the pair. It is
// nil, and nothing is read, outside a run of the e2e suite (BeforeSuite sets
// it), so that the unit tests of the ledger make no kubectl call. It runs in
// the background, never on the path of the call that printed the pair.
var pairCapture atomic.Pointer[pairCaptureFunc]

type pairCaptureFunc = func(ctx context.Context, namespace, pod, database string) pairServerState

// pairServerState is what pairCapture read.
type pairServerState struct {
	identity, logTail []string
	at                time.Time
	err               string
}

// cut returns s cut at width characters.
func cut(s string, width int) string {
	if r := []rune(s); len(r) > width {
		return string(r[:width])
	}
	return s
}

// boundLines splits out into trimmed non-empty lines, keeps the last limit of
// them, and cuts each at width characters.
func boundLines(out string, limit, width int) []string {
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
		lines[i] = cut(line, width)
	}
	return lines
}

// readServerState reads cub_server's identity and the server log's tail on a
// member, each bounded in time and size. A failed read is recorded, not
// returned: it must not change the call's result.
func readServerState(ctx context.Context, namespace, pod, database string) pairServerState {
	var state pairServerState
	read := func(script string, timeout time.Duration) (string, error) {
		bounded, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return utils.RunQuiet(exec.CommandContext(bounded, "kubectl", "-n", namespace, "exec", pod, "-c", "cubrid", "--",
			"bash", "-c", script, database))
	}
	var problems []string
	// The identity is read first: the age rule is only as good as the time
	// between the pair and this read.
	state.at = time.Now().UTC()
	if out, err := read(serverAgeScript, 2*identityTimeout); err != nil {
		problems = append(problems, "identity: "+err.Error())
	} else {
		state.identity = boundLines(out, 12, serverLogLineWidth)
	}
	if out, err := read(serverLogTailScript, identityTimeout); err != nil {
		problems = append(problems, "server log: "+err.Error())
	} else {
		state.logTail = boundLines(out, serverLogTailLines, serverLogLineWidth)
	}
	state.err = cut(strings.Join(problems, "; "), serverLogLineWidth)
	return state
}

// attach adds the server state read at the pair to a record, which may be nil.
func (l *lostSessionLedger) attach(record *lostSessionRecord, state pairServerState) {
	if record == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record.ServerAtPair, record.ServerLogTail, record.ServerReadError = state.identity, state.logTail, state.err
	record.ServerReadAt = state.at
	l.store()
}

// capture reads the server state of a member in the background and attaches it
// to record when it is done, so that the call that printed the pair is not
// delayed: some checks are timed. The read does not end with the spec's
// context, which may end first; it is bounded by its own timeouts (at most
// 30 s). It can therefore outlive its spec and send a read-only
// "cubrid changemode" to the member during the next one. A panic in it is
// recorded as the read's error, not raised in a goroutine of the suite.
func (l *lostSessionLedger) capture(ctx context.Context, record *lostSessionRecord, namespace, pod, database string) {
	read := pairCapture.Load()
	if read == nil || record == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	l.pending.Add(1)
	go func() {
		defer l.pending.Done()
		defer func() {
			if r := recover(); r != nil {
				l.attach(record, pairServerState{at: time.Now().UTC(),
					err: cut(fmt.Sprintf("the read of the server state panicked: %v", r), serverLogLineWidth)})
			}
		}()
		l.attach(record, (*read)(ctx, namespace, pod, database))
	}()
}

// waitForCaptures waits for the reads still running, for at most limit, and
// reports whether all of them are done.
func (l *lostSessionLedger) waitForCaptures(limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		l.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

// lostSessionLedger counts the pair over the whole run, for every caller of
// csqlRunner and the session held open on the master, also where a retry
// absorbs it and the step passes. It decides nothing: the count is how the
// rate is seen without reading each log. Its methods take its own lock, so
// a background sampler can add to it.
type lostSessionLedger struct {
	mu      sync.Mutex
	records []*lostSessionRecord
	// pending counts the background reads of the server state.
	pending sync.WaitGroup
	// reads and lost count by the labels of the step that made the call.
	reads, lost map[string]int
}

// csqlLedger is this run's ledger.
var csqlLedger = newLostSessionLedger()

func newLostSessionLedger() *lostSessionLedger {
	return &lostSessionLedger{reads: map[string]int{}, lost: map[string]int{}}
}

// observe counts one csql call and, when it printed the pair, gives the new
// record to seen, which may be nil. The command's error does not matter: a
// call that also failed printed the pair as well.
//
// The call is credited to the labels of the spec running when it returns. A
// background read, such as the sampler of the conditions, that outlives its
// spec is therefore credited to the next one; the run-wide count and the file
// do not depend on that, only the per-scenario measurement does.
func (l *lostSessionLedger) observe(member, sql, out string, seen func(*lostSessionRecord)) {
	read := sqlcheck.ReadOnly(sql)
	report := CurrentSpecReport()
	var record *lostSessionRecord
	l.mu.Lock()
	if read {
		for _, label := range report.Labels() {
			l.reads[label]++
		}
	}
	if sqlcheck.SessionLost(out) {
		kind := "write"
		if read {
			kind = "read"
		}
		record = l.add(report, member, firstLine(sql), kind)
	}
	l.mu.Unlock()
	if record != nil && seen != nil {
		seen(record)
	}
}

// add stores a new record. The caller holds the lock.
func (l *lostSessionLedger) add(report SpecReport, member, query, kind string) *lostSessionRecord {
	record := &lostSessionRecord{At: time.Now().UTC(), Step: report.LeafNodeText, Member: member, Query: query,
		Kind: kind, Identity: sqlcheck.IdentityNotRead, ReRead: sqlcheck.ReReadNotMade}
	l.records = append(l.records, record)
	for _, label := range report.Labels() {
		l.lost[label]++
	}
	l.store()
	_, _ = fmt.Fprintf(GinkgoWriter, "csql printed -199 and -111 on %s (%s, %s): %s\n", member, kind, record.Step, query)
	return record
}

// observeSession records the pair of a csql session that csqlRunner did not
// run, and returns the record.
func (l *lostSessionLedger) observeSession(member, query string) *lostSessionRecord {
	report := CurrentSpecReport()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.add(report, member, query, "write")
}

// settle adds what the rule found to a record, which may be nil.
func (l *lostSessionLedger) settle(record *lostSessionRecord, identity sqlcheck.IdentityResult, reRead string) {
	if record == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record.Identity, record.ReRead = identity, reRead
	l.store()
}

// store rewrites the file, so that a run that is killed still leaves it. The
// caller holds the lock.
func (l *lostSessionLedger) store() {
	var b strings.Builder
	for _, record := range l.records {
		line, err := json.Marshal(record)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	writeScenarioFile(lostSessionsFile, []byte(b.String()))
}

// measure is the count of the steps with label, or unknown when they made no
// csql read and printed no pair: zero would read as a measurement.
func (l *lostSessionLedger) measure(label string) any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reads[label] == 0 && l.lost[label] == 0 {
		return evidence.Unknown
	}
	return l.lost[label]
}

// note is the line of the run's summary, or empty without a pair: a run that
// passed with the pair must show it.
func (l *lostSessionLedger) note() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.records) == 0 {
		return ""
	}
	reads := 0
	for _, record := range l.records {
		if record.Kind == "read" {
			reads++
		}
	}
	return fmt.Sprintf("csql printed -199 and -111 %d time(s) in this run, %d on reads and %d on writes; "+
		"the cause is unknown (#238); each one is in %s", len(l.records), reads, len(l.records)-reads, lostSessionsFile)
}

// serverGuard reads the identity of a member's cub_server, in the container
// csql runs in, and on an HA member also its HA mode.
func serverGuard(ctx context.Context, namespace, pod, database string, ha bool) sqlcheck.Guard {
	script := sqlcheck.ServerIdentityScript
	if ha {
		script = sqlcheck.HAServerIdentityScript
	}
	return sqlcheck.Guard{Settle: identitySettle, HA: ha, Identity: func() (string, error) {
		bounded, cancel := context.WithTimeout(ctx, identityTimeout)
		defer cancel()
		return utils.RunQuiet(exec.CommandContext(bounded, "kubectl", "-n", namespace, "exec", pod, "-c", "cubrid", "--",
			"bash", "-c", script, database))
	}}
}

// oneShotRead is a read made under the rule of sqlcheck.Read.
type oneShotRead func(sql string, check func(string) error) (sqlcheck.ReadResult, error)

// csqlMember is csql on one member for the few calls that pay for the rule of
// sqlcheck: the read of cub_server's identity is one more kubectl exec for each
// call, so a read that a loop retries stays on csqlInPod, where it is only
// counted.
type csqlMember struct {
	ctx                      context.Context
	namespace, pod, database string
	run                      func(*exec.Cmd) (string, error)
	guard                    sqlcheck.Guard
}

func memberCSQL(ctx context.Context, namespace, pod, database string, ha bool) csqlMember {
	return csqlMember{ctx: ctx, namespace: namespace, pod: pod, database: database, run: utils.Run,
		guard: serverGuard(ctx, namespace, pod, database, ha)}
}

// memberCSQL is memberCSQL for a member of this HA cluster.
func (r *haRun) memberCSQL(ctx context.Context, pod string) csqlMember {
	return memberCSQL(ctx, r.namespace, pod, r.database, true)
}

// runner is csqlRunner that also hands over the record of each pair.
func (m csqlMember) runner(lost *[]*lostSessionRecord) sqlRunner {
	return watchedCSQL(m.ctx, m.namespace, m.pod, m.database, m.run,
		func(record *lostSessionRecord) { *lost = append(*lost, record) })
}

// read is sqlcheck.Read on the member, and adds its findings to the records.
func (m csqlMember) read(sql string, check func(string) error) (sqlcheck.ReadResult, error) {
	var lost []*lostSessionRecord
	result, err := sqlcheck.Read(m.guard, m.runner(&lost), sql, check)
	for i, record := range lost {
		if i == 0 {
			csqlLedger.settle(record, result.Identity, result.ReRead)
		} else {
			csqlLedger.settle(record, sqlcheck.IdentityNotRead, reReadIsThisCall)
		}
	}
	return result, err
}

// runOnce is sqlcheck.RunOnce on the member. Only a step whose verdict is a
// check of the rows afterwards may use it, because a nil error does not say
// that the statement took effect.
func (m csqlMember) runOnce(sql string, complete func(string) error) error {
	var lost []*lostSessionRecord
	identity, err := sqlcheck.RunOnce(m.guard, m.runner(&lost), sql, complete)
	for _, record := range lost {
		csqlLedger.settle(record, identity, reReadNeverWrite)
	}
	return err
}

// refusedAsReadOnly is sqlcheck.RunRefusal on the member: it runs a write the
// member must refuse once, and adds what the identity comparison found to the
// record of a pair.
func (m csqlMember) refusedAsReadOnly(sql string) (bool, error) {
	var lost []*lostSessionRecord
	refused, identity, err := sqlcheck.RunRefusal(m.guard, m.runner(&lost), sql)
	for _, record := range lost {
		csqlLedger.settle(record, identity, reReadRefusedWrite)
	}
	return refused, err
}

// statementFinished is the check of a statement that csql ran in autocommit
// mode: its result and the commit are both in the output.
func statementFinished(out string) error {
	if !strings.Contains(out, "Execute OK.") || !strings.Contains(out, "Committed.") {
		return errors.New("csql printed no complete result of the statement")
	}
	return nil
}

// heldRound matches the result of one round of the held transaction.
var heldRound = regexp.MustCompile(fmt.Sprintf(`\b%d rows affected\.`, openTxRows))

// heldSessionFinished checks that the output of the held session has every
// round of inserts and, after the last of them, the commit's Execute OK.
func heldSessionFinished(out string) error {
	rounds := heldRound.FindAllStringIndex(out, -1)
	if len(rounds) != openTxRounds {
		return fmt.Errorf("%d of the %d rounds of the held transaction are in its output", len(rounds), openTxRounds)
	}
	if !strings.Contains(out[rounds[len(rounds)-1][1]:], "Execute OK.") {
		return errors.New("the commit of the held transaction is not in its output")
	}
	return nil
}

// heldSessionVerdict judges the session that held a transaction open on master
// from its output and the error of its command. A command that failed fails it,
// and so does an output with "ERROR", unless the rule of sqlcheck tolerates
// exactly the pair. After that, the row count on every member decides: nothing
// here shows that the rows committed.
func heldSessionVerdict(watch sqlcheck.Watch, master, out string, sessionErr error) error {
	var record *lostSessionRecord
	if sqlcheck.SessionLost(out) {
		record = csqlLedger.observeSession(master, "the session that holds a transaction open")
	}
	if sessionErr != nil {
		return fmt.Errorf("%w: %s", sessionErr, out)
	}
	if !strings.Contains(out, "ERROR") {
		return nil
	}
	identity, err := watch.Judge(out, heldSessionFinished)
	csqlLedger.settle(record, identity, reReadNeverWrite)
	return err
}
