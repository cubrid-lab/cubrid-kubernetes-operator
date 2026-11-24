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
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// The manual DBA password change of docs/operations/dba-password.md, section
// "A manual change on an HA cluster (draft)" (#1012, POC-74), as the HA
// scenario runs it on Kind.
const (
	// pwChangeFile is where the statement file is put in each member.
	pwChangeFile = "/tmp/pwchange.sql"
	// pwChangeDone is what csql prints once it ran both statements of the
	// file.
	pwChangeDone = "2 command(s) successfully processed."
	// pwChangeWriter is a user without a password that writes and reads the
	// rows of pwChangeTable, so that the writes during the change need no
	// DBA password.
	pwChangeWriter = "e2e_writer"
	// pwChangeTable holds the rows written before, during and after the
	// change. Its primary key makes HA replicate it.
	pwChangeTable = "dba.dba_pwchange"
	// loginOK is what a login check's statement prints once csql logged in.
	loginOK = "login-ok"
)

// pwChangeStatement is the statement file of the change: CUBRID's
// set_password method on the dba user object, which is neither replicated nor
// written as SQL text into the log (POC-74). value must hold no single quote.
func pwChangeStatement(value string) string {
	return "CALL find_user('dba') ON CLASS db_user TO u;\nCALL set_password('" + value + "') ON u;\n"
}

// csqlTranscript is what a csql run on a terminal printed: all of it, and
// what it printed after the password was typed. Before it prompts, csql tries
// the empty password and prints the refusal, so only what follows the typed
// password decides a login.
type csqlTranscript struct {
	all, afterPassword string
}

// csqlPrompt matches csql's prompt, and its prompt with --sysadm.
var csqlPrompt = regexp.MustCompile(`(csql|sysadm)> `)

// runCSQLDialog runs cmd, a csql started through `kubectl exec -it`, on a new
// pseudo-terminal, as an operator runs it from a terminal. Once csql prints
// "Enter Password", it types password and a line feed: the value reaches csql
// only through the terminal, which csql reads with echo off, and is on no
// command line, in no environment and in no file. Each of lines is then typed
// after the next csql prompt, in order. The run ends when cmd ends, or after
// timeout, when its process group is killed. A csql that never prompts for
// the password, or that does not end in time, is an error; how csql answered
// is left to the caller.
func runCSQLDialog(cmd *exec.Cmd, password string, lines []string, timeout time.Duration) (csqlTranscript, error) {
	ptmx, pts, err := openTestTerminal()
	if err != nil {
		return csqlTranscript{}, err
	}
	defer func() { _ = ptmx.Close() }()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	// A new session whose controlling terminal is the pseudo-terminal, so
	// that kubectl sees a terminal on its stdin and asks for one in the
	// container.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	err = cmd.Start()
	_ = pts.Close()
	if err != nil {
		return csqlTranscript{}, fmt.Errorf("start %s on a terminal: %w", cmd.Path, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var mu sync.Mutex
	var out []byte
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				out = append(out, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	printed := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return bytes.Clone(out)
	}

	typedAt, sentAt := -1, 0
	pending := lines
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var runErr error
wait:
	for {
		select {
		case <-exited:
			break wait
		case <-deadline.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-exited
			runErr = fmt.Errorf("csql did not end within %s", timeout)
			break wait
		case <-tick.C:
		}
		o := printed()
		if typedAt < 0 {
			if bytes.Contains(o, []byte("Enter Password")) {
				if _, err := ptmx.Write([]byte(password + "\n")); err != nil {
					runErr = fmt.Errorf("type the password: %w", err)
				}
				typedAt, sentAt = len(o), len(o)
			}
			continue
		}
		if len(pending) > 0 && csqlPrompt.Match(o[sentAt:]) {
			if _, err := ptmx.Write([]byte(pending[0] + "\n")); err != nil {
				runErr = fmt.Errorf("type %q: %w", pending[0], err)
			}
			pending, sentAt = pending[1:], len(o)
		}
	}
	// What csql printed until it ended is read until the terminal closes.
	_ = ptmx.SetReadDeadline(time.Now().Add(time.Second))
	<-readDone
	o := printed()
	transcript := csqlTranscript{all: string(o)}
	if typedAt >= 0 {
		transcript.afterPassword = string(o[typedAt:])
	} else if runErr == nil {
		runErr = errors.New("csql did not prompt for the password")
	}
	return transcript, runErr
}

// kubectlExecTerminal is the documented `kubectl exec -it` command line that
// runs csql with args in the DB container of member.
func kubectlExecTerminal(namespace, member string, args ...string) *exec.Cmd {
	return exec.Command("kubectl", append([]string{"-n", namespace, "exec", "-it", member, "-c", "cubrid", "--",
		"bash", "-c", `exec csql "$@"`, "csql"}, args...)...)
}

// redactValues counts each of values in text and replaces it, so that text
// can be printed.
func redactValues(text string, values ...string) (string, int) {
	found := 0
	for _, v := range values {
		found += strings.Count(text, v)
		text = strings.ReplaceAll(text, v, "<redacted>")
	}
	return text, found
}

// loginVerdict is how csql answered a login check, from what it printed after
// the password was typed: "refused", "accepted" (it printed loginOK) or
// "unknown".
func loginVerdict(t csqlTranscript) string {
	switch {
	case strings.Contains(t.afterPassword, dbaPasswordRefused):
		return "refused"
	case strings.Contains(t.afterPassword, loginOK):
		return "accepted"
	default:
		return "unknown"
	}
}

// terminalLogin logs in to member as dba with value typed at csql's prompt,
// with the documented check of step 6, and returns csql's answer. Neither
// value nor any of others is printed: the transcript is printed with them
// redacted, and a transcript that holds one of them fails the step.
func (h dbaPasswordHelpers) terminalLogin(member, value string, others ...string) string {
	t, err := runCSQLDialog(kubectlExecTerminal(h.namespace, member, "-u", "dba", "-c",
		"SELECT 'login-' || 'ok' FROM db_root", h.database+"@localhost"), value, nil, 2*time.Minute)
	shown, found := redactValues(t.all, append([]string{value}, others...)...)
	_, _ = fmt.Fprintf(GinkgoWriter, "csql on a terminal on %s:\n%s\n", member, shown)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "csql on %s: %s", member, shown)
	ExpectWithOffset(1, found).To(BeZero(), "the terminal of csql on %s showed a DBA password", member)
	return loginVerdict(t)
}

// rowWriter writes rows into pwChangeTable as pwChangeWriter on one member,
// one autocommitted INSERT per csql, and records which were acknowledged. A
// row whose INSERT printed no acknowledgement has an unknown outcome: it may
// or may not exist.
type rowWriter struct {
	namespace, member, database string

	mu      sync.Mutex
	acked   []int
	unknown []int
}

// writerArgs is the kubectl command line that runs statement as
// pwChangeWriter in the DB container of member, over TCP by the member's
// hostname, as csqlOnArgs does.
func writerArgs(namespace, member, database, statement string) []string {
	return []string{"-n", namespace, "exec", member, "-c", "cubrid", "--", "bash", "-c",
		`PATH="${CUBRID}/bin:${PATH}" csql -u ` + pwChangeWriter + ` "$0@$(hostname)" -c "$1" </dev/null`,
		database, statement}
}

func (w *rowWriter) write(id int, phase string) {
	out, _ := utils.RunQuiet(exec.Command("kubectl", writerArgs(w.namespace, w.member, w.database,
		fmt.Sprintf("INSERT INTO %s VALUES (%d, '%s')", pwChangeTable, id, phase))...))
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.Contains(out, "1 row affected") {
		w.acked = append(w.acked, id)
	} else {
		w.unknown = append(w.unknown, id)
	}
}

// start writes rows from id first on in the background, one about every
// half second, until the returned function is called; that function returns
// once the last write ended, and may be called more than once.
func (w *rowWriter) start(first int, phase string) (stop func()) {
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		for id := first; ; id++ {
			select {
			case <-quit:
				return
			default:
			}
			w.write(id, phase)
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return sync.OnceFunc(func() {
		close(quit)
		<-done
	})
}

// ackedRows returns the acknowledged ids and the number of writes whose
// outcome is unknown.
func (w *rowWriter) ackedRows() ([]int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int(nil), w.acked...), len(w.unknown)
}

// memberRowIDs reads every id of pwChangeTable on member, as pwChangeWriter.
func (h dbaPasswordHelpers) memberRowIDs(member string) ([]int, error) {
	out, err := utils.RunQuiet(exec.Command("kubectl", writerArgs(h.namespace, member, h.database,
		"SELECT id FROM "+pwChangeTable+" ORDER BY id")...))
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, m := range regexp.MustCompile(`(?m)^\s+(\d+)\s*$`).FindAllStringSubmatch(out, -1) {
		id, _ := strconv.Atoi(m[1])
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

// passwordErrorsScript prints, in a member, how many lines of its copylogdb
// and applylogdb error logs name error -171 (Incorrect or missing password),
// as "FILES <n> LINES <m>".
const passwordErrorsScript = `shopt -s nullglob; files=("$CUBRID"/log/*logdb*.err); n=0; ` +
	`for f in "${files[@]}"; do c=$(grep -c -e '-171' -e 'Incorrect or missing password' "$f"); n=$((n+c)); done; ` +
	`echo "FILES ${#files[@]} LINES $n"`

// passwordErrors returns the number of -171 lines in the copier and applier
// logs of member, and the number of those logs.
func (h dbaPasswordHelpers) passwordErrors(member string) (lines, files int) {
	out, err := h.kubectl("exec", member, "-c", "cubrid", "--", "bash", "-c", passwordErrorsScript)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), out)
	m := regexp.MustCompile(`FILES (\d+) LINES (\d+)`).FindStringSubmatch(out)
	ExpectWithOffset(1, m).NotTo(BeNil(), "no count of -171 lines on %s: %s", member, out)
	files, _ = strconv.Atoi(m[1])
	lines, _ = strconv.Atoi(m[2])
	return lines, files
}

// applierFailCount is the fail count that applyinfo reports for the applier
// of the log member copies from primary.
func (h dbaPasswordHelpers) applierFailCount(member, primary string) (int, error) {
	out, err := h.kubectl("exec", member, "-c", "cubrid", "--", "bash", "-c",
		`PATH="${CUBRID}/bin:${PATH}" cubrid applyinfo -L "${CUBRID_DATABASES}/$0_$1" -r "$1" -a "$0"`,
		h.database, primary)
	if err != nil {
		return 0, err
	}
	m := regexp.MustCompile(`Fail count\s*:\s*(\d+)`).FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no fail count in the output of applyinfo on %s:\n%s", member, out)
	}
	return strconv.Atoi(m[1])
}
