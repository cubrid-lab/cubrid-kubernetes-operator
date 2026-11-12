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
// Only a standalone member is checked: an HA cluster cannot set a DBA
// password. A check that cannot tell answers Unknown, never Differs: the
// file is not set or cannot be used, the server does not run, an operation
// that stops the server runs, or the login was neither accepted nor refused
// for its password. One check runs at a time; one that arrives while another
// runs answers Unknown at once rather than wait, so callers cannot pile up
// logins. The operator asks again later.
func (s *Server) checkDBAPassword(ctx context.Context) DBAPasswordCheck {
	unknown := func(reason string) DBAPasswordCheck {
		return DBAPasswordCheck{State: DBAPasswordUnknown, Reason: reason}
	}
	if s.standaloneDB == "" {
		return unknown("the DBA password is not checked on an HA member")
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
	running, err := ServerRunning(ctx, s.cli, s.standaloneDB)
	switch {
	case err != nil:
		return unknown(err.Error())
	case !running:
		return unknown("server " + s.standaloneDB + " is not running")
	}
	// dbaPasswordAccepted's errors carry nothing csql printed.
	accepted, err := runningDBAPasswordAccepted(ctx, terminal, s.standaloneDB, password)
	switch {
	case err != nil:
		return unknown(err.Error())
	case !accepted:
		return DBAPasswordCheck{State: DBAPasswordDiffers,
			Reason: "a dba login with the value of the DBA password file was refused for its password"}
	}
	return DBAPasswordCheck{State: DBAPasswordMatches}
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
