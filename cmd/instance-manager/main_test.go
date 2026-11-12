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
	// alterErr makes the run fed on stdin fail and set nothing.
	alterErr error
}

func (c *dbaCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if c.set {
		return "ERROR: Incorrect or missing password.\n", errors.New("exit status 1")
	}
	return "1 row selected.\n", nil
}

func (c *dbaCLI) RunWithStdin(_ context.Context, _, name string, args ...string) (string, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if c.alterErr != nil {
		return "", c.alterErr
	}
	c.set = true
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
		if !cli.set || len(cli.calls) != 3 {
			t.Errorf("commands = %q, want the check, the statement and the check", cli.calls)
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
