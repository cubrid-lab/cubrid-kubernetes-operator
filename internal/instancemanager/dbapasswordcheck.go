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
	"log/slog"
	"net/http"
	"time"
)

// DBAPasswordState says whether the value of the mounted DBA password file is
// the database's DBA password (GET /v1/dba-password).
type DBAPasswordState string

const (
	// DBAPasswordMatches: a dba login with the file's value was accepted.
	DBAPasswordMatches DBAPasswordState = "Matches"
	// DBAPasswordDiffers: a dba login with the file's value was refused for
	// its password, so the file no longer holds the database's password.
	DBAPasswordDiffers DBAPasswordState = "Differs"
	// DBAPasswordUnknown: the check could not tell; its reason says why.
	DBAPasswordUnknown DBAPasswordState = "Unknown"
)

// DBAPasswordCheck is the answer of GET /v1/dba-password. Its reason never
// holds the password or anything csql printed.
type DBAPasswordCheck struct {
	State  DBAPasswordState `json:"state"`
	Reason string           `json:"reason,omitempty"`
}

// dbaPasswordCheckBound bounds one check, the server status and the login
// included. It stays below the 30 seconds after which the operator gives up
// on an Instance Manager call (NewHTTPBackupClient).
const dbaPasswordCheckBound = 20 * time.Second

// eventDBAPasswordChecked is logged with the answer of each check.
const eventDBAPasswordChecked = "dba_password_checked"

// dbaPassword answers GET /v1/dba-password (checkDBAPassword). The answer is
// always 200: a check that cannot tell is an Unknown answer, not an error.
func (s *Server) dbaPassword(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.checkDBAPassword(r.Context()))
}

// checkDBAPassword tells whether the value of the DBA password file is the
// running database's DBA password, by a dba login with it in client mode,
// typed at csql's prompt on a pseudo-terminal (runningDBAPasswordAccepted).
// The login runs SELECT 1 and changes nothing, and no record of the value is
// kept, so a password changed by hand with ALTER USER is seen as well.
//
// A standalone member is checked while its server runs. An HA member is
// checked only while heartbeat reports it as the master with its server
// registered_and_active: the operator asks the cluster's primary, the password
// was set on it, and a slave or a member in transition answers Unknown. The
// same login in client mode, csql -u dba <db>@localhost, logs in on every
// member of an HA cluster in the Kind HA scenario with a DBA password
// (test/e2e/dba_password_test.go, CUBRID 11.4.7.1995).
//
// A check that cannot tell answers Unknown, never Differs: the member was
// told no database, the file is not set or cannot be used, the server does
// not run or is not the active master, an operation that stops the server
// runs, or the login was neither accepted nor refused for its password. One
// check runs at a time; one that arrives while another runs answers Unknown at
// once rather than wait, so callers cannot pile up logins. The operator asks
// again later.
func (s *Server) checkDBAPassword(ctx context.Context) DBAPasswordCheck {
	unknown := func(reason string) DBAPasswordCheck {
		return DBAPasswordCheck{State: DBAPasswordUnknown, Reason: reason}
	}
	if s.standaloneDB == "" && s.replicationDB == "" {
		return unknown("the member was told no database")
	}
	if s.ha.DBAPasswordFile == "" {
		return unknown("no DBA password file is set (" + DBAPasswordFileEnv + "): the cluster asks for no DBA password")
	}
	if !s.dbaCheckMu.TryLock() {
		return unknown("another check of the DBA password is running")
	}
	defer s.dbaCheckMu.Unlock()
	bound := s.dbaCheckBound
	if bound <= 0 {
		bound = dbaPasswordCheckBound
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	check := s.runDBAPasswordCheck(ctx)
	s.logDBAPasswordCheck(ctx, check)
	return check
}

// runDBAPasswordCheck is checkDBAPassword once its preconditions hold.
func (s *Server) runDBAPasswordCheck(ctx context.Context) DBAPasswordCheck {
	unknown := func(reason string) DBAPasswordCheck {
		return DBAPasswordCheck{State: DBAPasswordUnknown, Reason: reason}
	}
	if s.stopRequested.Load() || s.disruptive.Load() > 0 {
		return unknown("the member is stopping, or an operation that stops and starts the server is running")
	}
	terminal, ok := s.cli.(TerminalCLI)
	if !ok {
		return unknown(errNoTerminal.Error())
	}
	// ReadDBAPassword's errors name the file, never its content.
	password, err := ReadDBAPassword(s.ha.DBAPasswordFile)
	if err != nil {
		return unknown(err.Error())
	}
	database, reason := s.dbaCheckDatabase(ctx)
	if database == "" {
		return unknown(reason)
	}
	// dbaPasswordAccepted's errors carry nothing csql printed.
	accepted, err := runningDBAPasswordAccepted(ctx, terminal, database, password)
	switch {
	case err != nil:
		return unknown(err.Error())
	case !accepted:
		return DBAPasswordCheck{State: DBAPasswordDiffers,
			Reason: "a dba login with the value of the DBA password file was refused for its password"}
	}
	return DBAPasswordCheck{State: DBAPasswordMatches}
}

// dbaCheckDatabase returns the database to log in to when its server may be
// asked now, or "" and why not. A standalone server must be listed by
// `cubrid server status`. An HA member must be reported by `cubrid heartbeat
// status` as the master whose server is registered_and_active; the server
// status of an HA member is not used, since no POC records its form there.
func (s *Server) dbaCheckDatabase(ctx context.Context) (string, string) {
	if s.standaloneDB != "" {
		running, err := ServerRunning(ctx, s.cli, s.standaloneDB)
		switch {
		case err != nil:
			return "", err.Error()
		case !running:
			return "", "server " + s.standaloneDB + " is not running"
		}
		return s.standaloneDB, ""
	}
	st := HeartbeatStatus(ctx, s.cli)
	if st.Role != RoleMaster || !st.ServerActive {
		why := "heartbeat reports role " + string(st.Role)
		if st.Reason != "" {
			why += " (" + st.Reason + ")"
		}
		return "", "the member is not the active master of its HA cluster: " + why
	}
	return s.replicationDB, ""
}

// logDBAPasswordCheck logs a check's answer, at info when it differs from the
// last one and below info otherwise: the operator asks every few minutes.
// Called with dbaCheckMu held.
func (s *Server) logDBAPasswordCheck(ctx context.Context, check DBAPasswordCheck) {
	level := slog.LevelDebug
	if check != s.dbaCheckLast {
		level = slog.LevelInfo
	}
	s.dbaCheckLast = check
	s.log().Log(ctx, level, "Checked the DBA password file against the database",
		"event", eventDBAPasswordChecked, "state", string(check.State), reasonKey, check.Reason)
}
