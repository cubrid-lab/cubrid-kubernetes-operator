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

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The rule below decides when a step may go on after csql printed the pair of
// SessionLost (#238). The printed text cannot decide it: in POC-55 a real
// server death printed exactly the harmless shape (exit 0, only the two codes,
// complete results) in 260 of 997 forced runs. What separated them was the
// identity of cub_server, read before the call and again after it, so a pair
// is tolerated only with that comparison and a separate check of the outcome.
// The cause of the pair is still unknown; the rule does not call it harmless.

// ServerIdentityScript prints a line "cub_server <pid> <start time>" for each
// live cub_server of a database, with the start time in clock ticks since
// boot from /proc/<pid>/stat. Run it as bash -c script <database>. It walks
// /proc because the image is not known to have ps, and leaves out a zombie,
// which is a server that has already died, even while its other threads end.
const ServerIdentityScript = `for p in /proc/[0-9]*; do ` +
	`{ read -r c < "$p/comm"; } 2>/dev/null || continue; [ "$c" = cub_server ] || continue; ` +
	`a=$( { tr '\0' ' ' < "$p/cmdline"; } 2>/dev/null) || continue; ` +
	`case " $a " in *" $0 "*) ;; *) continue ;; esac; ` +
	`s=$( { cat "$p/stat"; } 2>/dev/null) || continue; s=${s##*) }; set -- $s; ` +
	`case "$1" in Z|X) continue ;; esac; echo "cub_server ${p#/proc/} ${20}"; done`

// IdentityResult is what the comparison of cub_server's identity found.
type IdentityResult string

const (
	// IdentityNotRead says that no comparison was made.
	IdentityNotRead IdentityResult = "not read"
	// IdentityUnchanged is the same process before and after the call, and
	// the only result that lets a pair be tolerated.
	IdentityUnchanged IdentityResult = "unchanged"
	// IdentityChanged is another process after the call.
	IdentityChanged IdentityResult = "changed"
	// IdentityVanished is no running cub_server on one of the two reads.
	IdentityVanished IdentityResult = "vanished"
	// IdentityAmbiguous is several cub_server processes on one of the reads.
	IdentityAmbiguous IdentityResult = "ambiguous"
	// IdentityUnreadable is a read that failed or printed something else.
	IdentityUnreadable IdentityResult = "unreadable"
)

// How the second call of Read answered.
const (
	ReReadNotMade  = "not made"
	ReReadAnswered = "answered"
	ReReadLost     = "printed -199 and -111 again"
	reReadFailed   = "failed: "
)

// identityLine is one line of ServerIdentityScript.
var identityLine = regexp.MustCompile(`^cub_server \d+ \d+$`)

// readIdentity returns the identity in the output of ServerIdentityScript,
// or the result that says why there is none to compare.
func readIdentity(out string, err error) (string, IdentityResult) {
	if err != nil {
		return "", IdentityUnreadable
	}
	var found []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if !identityLine.MatchString(line) {
			return "", IdentityUnreadable
		}
		found = append(found, line)
	}
	switch len(found) {
	case 0:
		return "", IdentityVanished
	case 1:
		return found[0], IdentityUnchanged
	}
	return "", IdentityAmbiguous
}

// Guard is how the rule reads the identity of one member's cub_server.
type Guard struct {
	// Identity returns the output of ServerIdentityScript on the member.
	Identity func() (string, error)
	// Settle is the wait after a pair before the identity is read again: POC-55
	// read one server that had just been killed as unchanged at csql's exit.
	Settle time.Duration
}

// Watch holds the identity read before a call.
type Watch struct {
	guard  Guard
	before string
	result IdentityResult
}

// Before reads the identity. It must come before the call: cub_master revives
// a killed server within 0.04 s, so nothing read afterwards shows the restart.
// A read that failed fails only the rule, and only if a pair is printed.
func (g Guard) Before() Watch {
	out, err := g.Identity()
	before, result := readIdentity(out, err)
	return Watch{guard: g, before: before, result: result}
}

// compare reads the identity again, after the settle, and compares it with the
// one read before the call. A read that failed or found no single process
// never counts as unchanged, whichever of the two it was.
func (w Watch) compare() IdentityResult {
	time.Sleep(w.guard.Settle)
	out, err := w.guard.Identity()
	after, result := readIdentity(out, err)
	for _, worst := range []IdentityResult{IdentityUnreadable, IdentityAmbiguous, IdentityVanished} {
		if w.result == worst || result == worst {
			return worst
		}
	}
	if w.before != after {
		return IdentityChanged
	}
	return IdentityUnchanged
}

// lostSessionHeader is what csql prints before an error's code.
var lostSessionHeader = regexp.MustCompile(` - ERROR \*\*\* `)

// OnlyLostSession reports whether out holds both errors of SessionLost and
// no other error. Each line is looked at, and any "ERROR" or failed connection
// that is not part of the report of those two codes is another error, such as
// a statement that failed with an "ERROR:" line beside the pair.
func OnlyLostSession(out string) bool {
	if !SessionLost(out) {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		if lostSessionCodes.MatchString(line) {
			line = lostSessionHeader.ReplaceAllString(lostSessionCodes.ReplaceAllString(line, ""), "")
		}
		if strings.Contains(line, "ERROR") || strings.Contains(line, "Failed to connect") {
			return false
		}
	}
	return true
}

// LostSessionError is the error of a call that printed only the pair of
// SessionLost and that the rule did not tolerate. It is never a pass.
type LostSessionError struct {
	// Identity is what the comparison found.
	Identity IdentityResult
	// Twice is true when the identity did not change and the read made once
	// more printed the pair again.
	Twice bool
	err   error
}

func (e *LostSessionError) Error() string {
	if e.Twice {
		return fmt.Sprintf("csql printed -199 and -111 on the read and on the read made once more: %v", e.err)
	}
	return fmt.Sprintf("csql printed -199 and -111 and the identity of cub_server is %s, "+
		"so a server death cannot be ruled out: %v", e.Identity, e.err)
}

func (e *LostSessionError) Unwrap() error { return e.err }

// judge applies the rule to out, which a command that exited 0 printed: only
// the pair, a complete result, and an unchanged identity. cause is the error
// of a step that did not meet it. A nil error leaves the outcome to the
// caller's independent check: it is not evidence that the statement worked.
func (w Watch) judge(out string, complete func(string) error, cause error) (IdentityResult, error) {
	if !OnlyLostSession(out) || complete(out) != nil {
		return IdentityNotRead, cause
	}
	identity := w.compare()
	if identity != IdentityUnchanged {
		return identity, &LostSessionError{Identity: identity, err: cause}
	}
	return identity, nil
}

// Judge applies the rule to the output of a csql session that did not go
// through Run and that exited 0, and that printed an error. It never repeats
// anything. A nil error leaves the outcome to the caller's independent check.
func (w Watch) Judge(out string, complete func(string) error) (IdentityResult, error) {
	return w.judge(out, complete, fmt.Errorf("csql printed an error:\n%s", out))
}

// lostSessionOnly reports whether err is an error of Run for a command that
// exited 0 and printed only the pair.
func lostSessionOnly(err error) bool {
	var statement *StatementError
	return errors.As(err, &statement) && !statement.commandFailed && OnlyLostSession(statement.out)
}

// RunOnce runs sql exactly once and applies the rule to a pair. It is for a
// statement whose outcome the caller then checks in a way that does not rest
// on csql's output, such as a row count on every member. Without such a check
// a write stays with Run: the contract counts a write that printed the pair as
// failed, and a pair says nothing about whether it committed.
func RunOnce(g Guard, run Runner, sql string, complete func(string) error) (IdentityResult, error) {
	watch := g.Before()
	out, err := Run(run, sql)
	if !lostSessionOnly(err) {
		return IdentityNotRead, err
	}
	return watch.judge(out, complete, err)
}

// ErrNotARead is the error of Read for text that is not one SELECT.
var ErrNotARead = errors.New("not a single SELECT")

// selectStatement matches the start of a SELECT.
var selectStatement = regexp.MustCompile(`(?i)^SELECT\b`)

// ReadOnly reports whether sql is one SELECT, with at most a final semicolon.
// Any other semicolon, even inside a string, makes it not one: repeating text
// that might hold a second statement is what must never happen.
func ReadOnly(sql string) bool {
	text := strings.TrimSuffix(strings.TrimSpace(sql), ";")
	return selectStatement.MatchString(text) && !strings.Contains(text, ";")
}

// Attempt is one csql call of Read.
type Attempt struct {
	Start, End time.Time
	Out        string
	// Err is the error of Run or of the check for this call alone.
	Err error
}

// ReadResult is what Read did, also when it returned an error.
type ReadResult struct {
	// Out is the output of the last call.
	Out      string
	Attempts []Attempt
	// Identity is what the comparison after a pair found.
	Identity IdentityResult
	// ReRead says how the second call answered.
	ReRead string
}

// Read is a one-shot read under the rule. check says whether an output holds
// the site's complete, expected result. It reads the identity, then reads once.
// A pair is followed by a second read, and the last, only with exit 0, no other
// error, a result that passes check and an unchanged identity; the second read
// then decides. A second pair returns a *LostSessionError with Twice, and a
// changed, missing or unreadable identity one without. Any other outcome,
// such as a -199 alone, is the error of that one call. Run has no part of this
// and keeps failing on the pair, so a write can never reach this retry.
func Read(g Guard, run Runner, sql string, check func(string) error) (ReadResult, error) {
	result := ReadResult{Identity: IdentityNotRead, ReRead: ReReadNotMade}
	if !ReadOnly(sql) {
		return result, fmt.Errorf("%w: %s", ErrNotARead, sql)
	}
	watch := g.Before()
	call := func() Attempt {
		attempt := Attempt{Start: time.Now().UTC()}
		attempt.Out, attempt.Err = Run(run, sql)
		if attempt.Err == nil {
			attempt.Err = check(attempt.Out)
		}
		attempt.End = time.Now().UTC()
		result.Attempts = append(result.Attempts, attempt)
		result.Out = attempt.Out
		return attempt
	}
	first := call()
	if !lostSessionOnly(first.Err) {
		return result, first.Err
	}
	var err error
	if result.Identity, err = watch.judge(first.Out, check, first.Err); err != nil {
		return result, err
	}
	second := call()
	switch {
	case second.Err == nil:
		result.ReRead = ReReadAnswered
		return result, nil
	case lostSessionOnly(second.Err) && check(second.Out) == nil:
		result.ReRead = ReReadLost
		return result, &LostSessionError{Identity: result.Identity, Twice: true, err: second.Err}
	}
	line, _, _ := strings.Cut(second.Err.Error(), "\n")
	result.ReRead = reReadFailed + line
	return result, second.Err
}

// rowsSelected matches the line in which csql counts the rows of a result.
var rowsSelected = regexp.MustCompile(`(?m)^(\d+) rows? selected\b`)

// ValuesComplete checks that Values can read out, and, when out holds the
// pair of SessionLost, that csql counted as many rows as the table shows: a
// pair can come before or in the middle of a result.
func ValuesComplete(out string) error {
	values, err := Values(out)
	if err != nil || !SessionLost(out) {
		return err
	}
	count := rowsSelected.FindStringSubmatch(out)
	if count == nil || count[1] != strconv.Itoa(len(values)) {
		return fmt.Errorf("%d values and no matching count of rows in the output of csql:\n%s", len(values), out)
	}
	return nil
}
