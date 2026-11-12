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
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// otherSourcePassword is the DBA password of a backup's source that is not
// the requested one.
const otherSourcePassword = "Other-Source-Pw-93d1"

// restorePasswordCLI stands in for a member whose restore brings back the
// DBA password of the backup's source (docs/poc/RESULTS.md, POC-61, Q4):
// restoredb and restoreslave give the database the source's password, and
// csql then behaves as passwordCLI does. Every other cubrid command is
// answered by inner.
type restorePasswordCLI struct {
	t     *testing.T
	inner *stepCLI
	// source is the DBA password of the backup's source; empty is none.
	source string

	mu      sync.Mutex
	calls   []string
	current string
	stdins  []string
	typed   []string
}

func (c *restorePasswordCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	c.mu.Lock()
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if name == "csql" {
		defer c.mu.Unlock()
		if c.current != "" {
			return outRefused, exitStatus1(c.t)
		}
		return outAccepted, nil
	}
	if len(args) > 0 && (args[0] == "restoredb" || args[0] == "restoreslave") {
		c.current = c.source
	}
	c.mu.Unlock()
	return c.inner.Run(ctx, name, args...)
}

func (c *restorePasswordCLI) RunWithStdin(_ context.Context, stdin, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	c.stdins = append(c.stdins, stdin)
	c.current = alterPassword(stdin)
	return "Execute OK.\n", nil
}

func (c *restorePasswordCLI) RunOnTerminal(_ context.Context, _, input, name string, args ...string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "terminal "+name+" "+strings.Join(args, " "))
	c.typed = append(c.typed, input)
	if c.current != "" && input == c.current {
		return "\n" + outAccepted, true, nil
	}
	return "\n" + outRefused, true, exitStatus1(c.t)
}

func (c *restorePasswordCLI) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// passwordFile writes value to a DBA password file and returns its path.
func passwordFile(t *testing.T, value string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "dba-password")
	if err := os.WriteFile(file, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// restoreWithPassword runs a restore on a standalone member, or seeded HA
// member, whose backup's source has the DBA password source, and returns the
// operation, the CLI and the manager's log. requested is the content of the
// DBA password file; empty runs without one.
func restoreWithPassword(t *testing.T, source, requested string, seed bool) (Operation, *restorePasswordCLI, string, string) {
	t.Helper()
	cli := &restorePasswordCLI{t: t, inner: &stepCLI{}, source: source}
	var buf bytes.Buffer
	member := restoreMember{standalone: !seed, seed: seed, logger: testLogger(&buf, slog.LevelDebug), opsDir: t.TempDir()}
	if seed {
		conf := filepath.Join(t.TempDir(), "cubrid_ha.conf")
		if err := os.WriteFile(conf, []byte(testHAConf), 0o600); err != nil {
			t.Fatal(err)
		}
		member.haConf = conf
		cli.inner.failing = map[string]bool{stepHeartbeatStatus: true}
	}
	if requested != "" {
		member.dbaPasswordFile = passwordFile(t, requested)
	}
	final := runRestoreOperation(t, cli, member)
	return final, cli, buf.String(), member.opsDir
}

// wantNoPasswordAnywhere checks that no value reached an argv, the log, the
// failure reason or an operation record.
func wantNoPasswordAnywhere(t *testing.T, final Operation, cli *restorePasswordCLI, logs, opsDir string, values ...string) {
	t.Helper()
	records := ""
	_ = filepath.WalkDir(opsDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path) // #nosec G304 -- the test's own operation store
			records += string(b)
		}
		return nil
	})
	for _, value := range values {
		for _, call := range cli.recorded() {
			if strings.Contains(call, value) {
				t.Errorf("an argv carries a password: %q", call)
			}
		}
		if strings.Contains(logs, value) {
			t.Errorf("the log carries a password:\n%s", logs)
		}
		if strings.Contains(final.FailureReason, value) {
			t.Errorf("the failure reason carries a password: %q", final.FailureReason)
		}
		if strings.Contains(records, value) {
			t.Errorf("an operation record carries a password")
		}
	}
}

// indexOf returns the index of the first call with prefix, or -1.
func indexOf(calls []string, prefix string) int {
	for i, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return i
		}
	}
	return -1
}

// A backup of a database without a DBA password restores a database without
// one: with a password requested, it is set on the stopped database after
// restoredb and before the server starts, as on a database created new.
func TestServer_Restore_SetsTheRequestedPasswordOnASourceWithout(t *testing.T) {
	final, cli, logs, opsDir := restoreWithPassword(t, "", testPassword, false)
	if final.State != OpCompleted {
		t.Fatalf("final = %s (%s), want Completed", final.State, final.FailureReason)
	}
	calls := cli.recorded()
	restore, set, start := indexOf(calls, "cubrid restoredb "), indexOf(calls, callSetPass), indexOf(calls, callServerStart)
	if restore < 0 || set < 0 || start < 0 || restore >= set || set >= start {
		t.Errorf("calls = %q, want restoredb, then the password set, then the server start", calls)
	}
	if len(cli.stdins) != 1 || alterPassword(cli.stdins[0]) != testPassword {
		t.Errorf("stdin = %q, want the requested password set once", cli.stdins)
	}
	if lines := withEvent(logLines(t, bytes.NewBufferString(logs)), eventDBAPasswordApplied); len(lines) != 1 {
		t.Errorf("want one %s line, got %v", eventDBAPasswordApplied, lines)
	}
	wantNoPasswordAnywhere(t, final, cli, logs, opsDir, testPassword)
}

// A backup of a database with the requested password restores it with that
// password: a login with it is accepted, nothing is changed, and the server
// starts after that check.
func TestServer_Restore_AcceptsASourceWithTheRequestedPassword(t *testing.T) {
	final, cli, logs, opsDir := restoreWithPassword(t, testPassword, testPassword, false)
	if final.State != OpCompleted {
		t.Fatalf("final = %s (%s), want Completed", final.State, final.FailureReason)
	}
	calls := cli.recorded()
	login, start := indexOf(calls, callTerminalLogin), indexOf(calls, callServerStart)
	if login < 0 || start < 0 || login > start {
		t.Errorf("calls = %q, want the login on a terminal before the server start", calls)
	}
	if len(cli.stdins) != 0 {
		t.Errorf("an ALTER was fed: %q", cli.stdins)
	}
	wantNoPasswordAnywhere(t, final, cli, logs, opsDir, testPassword)
}

// A backup of a database with another DBA password restores a database whose
// password cannot be changed without that one (POC-61, Q4). The restore
// fails and says why, the server is not started, and the restored data is
// taken back as for a failed restoredb (ADR-0008): a retry restores again
// and fails the same way, and nothing is left that the entrypoint or a
// retry could start without the requested password.
func TestServer_Restore_FailsOnASourceWithAnotherPassword(t *testing.T) {
	final, cli, logs, opsDir := restoreWithPassword(t, otherSourcePassword, testPassword, false)
	if final.State != OpFailed {
		t.Fatalf("final = %s, want Failed", final.State)
	}
	if !strings.Contains(final.FailureReason, "not the requested") {
		t.Errorf("failure reason = %q, want it to say the DBA password is not the requested one", final.FailureReason)
	}
	calls := cli.recorded()
	if indexOf(calls, callServerStart) >= 0 {
		t.Errorf("the server was started: %q", calls)
	}
	if len(cli.stdins) != 0 {
		t.Errorf("an ALTER was fed: %q", cli.stdins)
	}
	if _, err := os.Stat(filepath.Join(lastRestoreTarget, dbName)); !os.IsNotExist(err) {
		t.Errorf("the restored database directory is still there (err=%v)", err)
	}
	if registered, err := registeredInDatabasesTxt(lastRestoreTarget, dbName); err != nil || registered {
		t.Errorf("the database is still registered (%v, %v)", registered, err)
	}
	if final.Restored != nil {
		t.Errorf("the restore was recorded as restored: %+v", final.Restored)
	}
	wantNoPasswordAnywhere(t, final, cli, logs, opsDir, testPassword, otherSourcePassword)
}

// Without a DBA password file a restore runs no csql at all, whatever the
// source's password: nothing changes for a cluster that asks for none.
func TestServer_Restore_WithoutAPasswordFileRunsNoLogin(t *testing.T) {
	for _, seed := range []bool{false, true} {
		final, cli, _, _ := restoreWithPassword(t, otherSourcePassword, "", seed)
		if final.State != OpCompleted {
			t.Fatalf("seed=%v: final = %s (%s), want Completed", seed, final.State, final.FailureReason)
		}
		for _, call := range cli.recorded() {
			if strings.Contains(call, "csql") {
				t.Errorf("seed=%v: csql ran without a password file: %q", seed, cli.recorded())
			}
		}
	}
}

// A password file that cannot be used fails the restore before anything is
// downloaded or restored.
func TestServer_Restore_AnUnusablePasswordFileRestoresNothing(t *testing.T) {
	cli := &restorePasswordCLI{t: t, inner: &stepCLI{}}
	empty := filepath.Join(t.TempDir(), "dba-password")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	final := runRestoreOperation(t, cli, restoreMember{standalone: true, dbaPasswordFile: empty})
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "DBA password") {
		t.Fatalf("final = %s (%s), want Failed naming the DBA password", final.State, final.FailureReason)
	}
	if calls := cli.recorded(); len(calls) != 0 {
		t.Errorf("commands ran: %q", calls)
	}
}

// A slave seeded from its master's backup carries the master's password,
// which replicates and is never set on the slave (POC-61, Q3): the requested
// one is confirmed before heartbeat starts. A seed whose password is another
// one, or none, fails without a statement fed and without heartbeat.
func TestServer_Restore_SeededSlaveOnlyConfirmsThePassword(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		ok     bool
	}{
		{name: "master has the requested password", source: testPassword, ok: true},
		{name: "master has another password", source: otherSourcePassword},
		{name: "master has none", source: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			final, cli, logs, opsDir := restoreWithPassword(t, tc.source, testPassword, true)
			if (final.State == OpCompleted) != tc.ok {
				t.Fatalf("final = %s (%s), want ok=%v", final.State, final.FailureReason, tc.ok)
			}
			calls := cli.recorded()
			if indexOf(calls, "cubrid restoreslave ") < 0 {
				t.Fatalf("calls = %q, want restoreslave", calls)
			}
			if len(cli.stdins) != 0 {
				t.Errorf("a statement was fed on a seeded slave: %q", cli.stdins)
			}
			hb := indexOf(calls, callHeartbeatStart)
			if tc.ok && (hb < 0 || indexOf(calls, callTerminalLogin) > hb) {
				t.Errorf("calls = %q, want the login on a terminal before heartbeat start", calls)
			}
			if !tc.ok && hb >= 0 {
				t.Errorf("heartbeat was started: %q", calls)
			}
			wantNoPasswordAnywhere(t, final, cli, logs, opsDir, testPassword, otherSourcePassword)
		})
	}
}
