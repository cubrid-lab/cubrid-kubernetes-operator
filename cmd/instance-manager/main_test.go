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

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// dbaCLI stands in for csql: an empty dba login is accepted until a password
// was fed on stdin, then refused (docs/poc/RESULTS.md, POC-61).
type dbaCLI struct {
	calls []string
	set   bool
	// current is the password the database has while set is true.
	current string
	// alterErr makes the run fed on stdin fail and set nothing.
	alterErr error
	stdins   []string
}

// RunOnTerminal stands in for a csql login with the password typed at its
// prompt: accepted only with the password the database has. Without a
// password csql logs in with the empty one and does not prompt.
func (c *dbaCLI) RunOnTerminal(_ context.Context, _, input, name string, args ...string) (string, bool, error) {
	c.calls = append(c.calls, "terminal "+name+" "+strings.Join(args, " "))
	if !c.set {
		return "1 row selected.\n", false, nil
	}
	if input == c.current {
		return "\n1 row selected.\n", true, nil
	}
	return "\nERROR: Incorrect or missing password.\n", true, errors.New("exit status 1")
}

func (c *dbaCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if c.set {
		return "ERROR: Incorrect or missing password.\n", errors.New("exit status 1")
	}
	return "1 row selected.\n", nil
}

func (c *dbaCLI) RunWithStdin(_ context.Context, stdin, name string, args ...string) (string, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	c.stdins = append(c.stdins, stdin)
	if c.alterErr != nil {
		return "", c.alterErr
	}
	c.set = true
	c.current = strings.TrimSuffix(strings.TrimPrefix(stdin, "ALTER USER dba PASSWORD '"), "';\n")
	return "Execute OK.\n", nil
}

const subcommandPassword = "Subcommand-Canary-41b2"

// "instance-manager apply-dba-password" sets the password from the file
// CUBRID_DBA_PASSWORD_FILE names; unset, it runs nothing and succeeds, so a
// member without a requested password starts as before.
func TestApplyDBAPasswordCommand(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("unset", func(t *testing.T) {
		t.Setenv(instancemanager.DBAPasswordFileEnv, "")
		cli := &dbaCLI{}
		if err := applyDBAPassword(context.Background(), logger, cli); err != nil {
			t.Fatalf("applyDBAPassword: %v", err)
		}
		if len(cli.calls) != 0 {
			t.Errorf("commands ran without a password file: %q", cli.calls)
		}
	})
	t.Run("set", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "password")
		if err := os.WriteFile(file, []byte(subcommandPassword+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(instancemanager.DBAPasswordFileEnv, file)
		t.Setenv("CUBRID_DB", "appdb")
		cli := &dbaCLI{}
		if err := applyDBAPassword(context.Background(), logger, cli); err != nil {
			t.Fatalf("applyDBAPassword: %v", err)
		}
		if !cli.set || len(cli.calls) != 4 {
			t.Errorf("commands = %q, want the check, the statement, the check and a login with the value", cli.calls)
		}
	})
	t.Run("csql failed", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "password")
		if err := os.WriteFile(file, []byte(subcommandPassword), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(instancemanager.DBAPasswordFileEnv, file)
		cli := &dbaCLI{alterErr: errors.New("exit status 1")}
		if err := applyDBAPassword(context.Background(), logger, cli); err == nil {
			t.Fatal("a failed csql run was reported as success")
		}
	})
	t.Run("unusable file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "password")
		if err := os.WriteFile(file, []byte(subcommandPassword+"\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(instancemanager.DBAPasswordFileEnv, file)
		cli := &dbaCLI{}
		err := applyDBAPassword(context.Background(), logger, cli)
		if err == nil {
			t.Fatal("an unusable password file was accepted")
		}
		if strings.Contains(err.Error(), subcommandPassword) {
			t.Errorf("the error carries the password: %v", err)
		}
		if len(cli.calls) != 0 {
			t.Errorf("commands ran: %q", cli.calls)
		}
	})
}

// "instance-manager check-dba-password" only checks: with the file set, a
// database whose DBA password is the file's value passes, and one with
// another password or none fails, without any statement fed. The entrypoint
// runs it before it starts an HA member or a restored database, so a Secret
// whose value was changed stops the start. Unset, it runs nothing.
func TestCheckDBAPasswordCommand(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("unset", func(t *testing.T) {
		t.Setenv(instancemanager.DBAPasswordFileEnv, "")
		cli := &dbaCLI{}
		if err := checkDBAPassword(context.Background(), logger, cli); err != nil {
			t.Fatalf("checkDBAPassword: %v", err)
		}
		if len(cli.calls) != 0 {
			t.Errorf("commands ran without a password file: %q", cli.calls)
		}
	})
	for _, tc := range []struct {
		name    string
		set     bool
		current string
		ok      bool
	}{
		{name: "the requested password", set: true, current: subcommandPassword, ok: true},
		{name: "another password", set: true, current: "Another-Password-1"},
		{name: "no password", set: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "password")
			if err := os.WriteFile(file, []byte(subcommandPassword+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(instancemanager.DBAPasswordFileEnv, file)
			t.Setenv("CUBRID_DB", "appdb")
			cli := &dbaCLI{set: tc.set, current: tc.current}
			err := checkDBAPassword(context.Background(), logger, cli)
			if (err == nil) != tc.ok {
				t.Fatalf("checkDBAPassword = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), subcommandPassword) {
				t.Errorf("the error carries the password: %v", err)
			}
			if len(cli.stdins) != 0 {
				t.Errorf("a statement was fed: %q", cli.stdins)
			}
			for _, call := range cli.calls {
				if strings.Contains(call, subcommandPassword) {
					t.Errorf("an argv carries the password: %q", call)
				}
			}
		})
	}
}

// The DBA password, read from the file CUBRID_DBA_PASSWORD_FILE names, is one
// of the values the log never writes, as the token and the object-storage keys
// are: a line that would carry it, by any path, carries a placeholder (#801).
func TestNewLogger_RedactsTheDBAPassword(t *testing.T) {
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte(subcommandPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(instancemanager.DBAPasswordFileEnv, file)
	var buf strings.Builder
	logger, err := newLogger(&buf)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("A line that holds "+subcommandPassword, "value", subcommandPassword)
	if strings.Contains(buf.String(), subcommandPassword) {
		t.Errorf("the log carries the DBA password:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "A line that holds") {
		t.Errorf("the line was not written:\n%s", buf.String())
	}
}

// "instance-manager shutdown" records the stop before it asks the manager.
// A manager that does not listen yet, and would start CUBRID once it does,
// finds the record; one that listens already receives the request after the
// record is in place (#323).
func TestShutdown_RecordsTheStopBeforeItAsksTheManager(t *testing.T) {
	stopFile := filepath.Join(t.TempDir(), "stop-requested")
	recordedFirst := make(chan bool, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := os.Stat(stopFile)
		recordedFirst <- err == nil
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	_, port, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IM_ADDR", ":"+port)
	t.Setenv("IM_TOKEN", "tok")

	if err := shutdown(slog.New(slog.NewTextHandler(io.Discard, nil)), stopFile); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case ok := <-recordedFirst:
		if !ok {
			t.Error("the manager was asked before the stop was recorded")
		}
	default:
		t.Fatal("the manager was not asked")
	}
	if _, err := os.Stat(stopFile); err != nil {
		t.Errorf("the stop was not recorded: %v", err)
	}
}

// The HA bootstrap member reads the DBA password file the operator names in
// CUBRID_DBA_PASSWORD_FILE; without the variable it sets no password (#804).
func TestHAConfigFromEnvNamesTheDBAPasswordFile(t *testing.T) {
	t.Setenv(instancemanager.DBAPasswordFileEnv, "/etc/cubrid-dba/password")
	if got := haConfigFromEnv().DBAPasswordFile; got != "/etc/cubrid-dba/password" {
		t.Errorf("DBAPasswordFile = %q, want the path from %s", got, instancemanager.DBAPasswordFileEnv)
	}
	t.Setenv(instancemanager.DBAPasswordFileEnv, "")
	if got := haConfigFromEnv().DBAPasswordFile; got != "" {
		t.Errorf("DBAPasswordFile = %q without the variable, want none", got)
	}
}
