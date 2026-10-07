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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HABootstrapRequest asks an HA member to hold the cluster's first database
// and start HA (ADR-0010). The operator sends it to exactly one member; the
// other members are seeded from that one and never create a database.
type HABootstrapRequest struct {
	// Database is the CUBRID database name to create.
	Database string `json:"database"`
}

// HAConfig is what the manager needs to initialize an HA member. All of it
// comes from the manager's own environment, never from a request.
type HAConfig struct {
	// ConfPath is the member's cubrid_ha.conf (the operator's ConfigMap mount).
	ConfPath string
	// VolumeSize is the --db-volume-size of a database this member creates.
	VolumeSize string
	// Locale is the locale of a database this member creates.
	Locale string
}

func (c HAConfig) withDefaults() HAConfig {
	if c.VolumeSize == "" {
		c.VolumeSize = "512M"
	}
	if c.Locale == "" {
		c.Locale = "en_US"
	}
	return c
}

// WithHAConfig enables the HA bootstrap endpoint. Returns the server for
// chaining.
func (s *Server) WithHAConfig(c HAConfig) *Server {
	s.ha = c.withDefaults()
	return s
}

// haNodeListPattern matches "ha_node_list=<group>@<host>:<host>..." and
// captures the hosts. Host names are DNS labels (ADR-0004).
var haNodeListPattern = regexp.MustCompile(`(?m)^\s*ha_node_list\s*=\s*[A-Za-z0-9_]+@([a-z0-9]([-a-z0-9]*[a-z0-9])?(:[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)\s*$`)

// haHosts returns the member host names of cubrid_ha.conf as CUBRID writes
// them in databases.txt: "host0:host1:host2".
func haHosts(confPath string) (string, error) {
	if confPath == "" {
		return "", errors.New("no HA configuration path is set (CUBRID_HA_CONF)")
	}
	data, err := os.ReadFile(confPath) // #nosec G304 -- the manager's own configuration path
	if err != nil {
		return "", fmt.Errorf("read the HA configuration: %w", err)
	}
	m := haNodeListPattern.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("%s has no usable ha_node_list", confPath)
	}
	return string(m[1]), nil
}

// haBootstrap serves POST /v1/ha/bootstrap: create the first database if this
// member has none, then start heartbeat. It is asynchronous and idempotent
// like backup and restore.
func (s *Server) haBootstrap(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{errKey: "HA bootstrap is not enabled (no operation store)"})
		return
	}
	if s.standaloneDB != "" {
		writeJSON(w, http.StatusConflict, map[string]string{errKey: "this member is a standalone server, not an HA member"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgUnreadableBody})
		return
	}
	var req HABootstrapRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgInvalidBody})
		return
	}
	if !databaseNamePattern.MatchString(req.Database) {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "database name is not a plain identifier"})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: msgNoIdempotencyKey})
		return
	}
	if !s.admit() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{errKey: errStopping.Error()})
		return
	}

	op, existed, err := s.store.FindOrCreate(OpHABootstrap, key, HashRequest(body), req.Database)
	if err != nil || existed {
		s.ops.Done()
	}
	switch {
	case errors.Is(err, ErrIdempotencyConflict), errors.Is(err, ErrOperationInProgress):
		writeJSON(w, http.StatusConflict, map[string]string{errKey: err.Error()})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
		return
	}
	if !existed {
		s.runHABootstrap(op.ID, req)
	}
	writeJSON(w, http.StatusAccepted, op)
}

// runHABootstrap creates the database when the target has none and starts
// heartbeat when it is not running, recording each step durably.
func (s *Server) runHABootstrap(id string, req HABootstrapRequest) {
	go func() {
		defer s.ops.Done()
		ctx, cancel := context.WithTimeout(s.opsCtx, s.timeouts.Bootstrap)
		defer cancel()
		fail := func(reason string) {
			reason = s.stoppingReason(reason)
			_, _ = s.store.Update(id, func(op *Operation) {
				op.State = OpFailed
				op.FailureReason = reason
			})
		}

		// What an interrupted attempt of this manager left is removed first;
		// anything else that is in the way stops the bootstrap below.
		if err := s.reclaimIncomplete(req.Database); err != nil {
			fail(err.Error())
			return
		}
		registered, err := registeredInDatabasesTxt(s.restoreRoots.Target, req.Database)
		if err != nil {
			fail(err.Error())
			return
		}
		if !registered {
			if _, err := s.store.Update(id, func(op *Operation) { op.State = OpCreating }); err != nil {
				return
			}
			if err := createHADatabase(ctx, s.cli, s.restoreRoots.Target, s.ha, req.Database, id); err != nil {
				fail(err.Error())
				return
			}
		}

		// `cubrid heartbeat start` is issued once: repeating it while HA is
		// activating flips it off again (docs/poc/RESULTS.md, POC-3).
		if HeartbeatStatus(ctx, s.cli).Role == RoleUnknown {
			if _, err := s.store.Update(id, func(op *Operation) { op.State = OpStarting }); err != nil {
				return
			}
			if out, err := s.activate(ctx, "heartbeat", "start"); err != nil {
				fail("heartbeat start failed: " + err.Error() + ": " + out)
				return
			}
		}
		_, _ = s.store.Update(id, func(op *Operation) { op.State = OpCompleted })
	}()
}

// createHADatabase runs `cubrid createdb` for an HA member: volumes below
// <target>/<database>, and the HA member list as the server host, which is
// what databases.txt has to carry in HA (docs/poc/RESULTS.md, POC-13).
//
// A directory that exists although the database is not registered is what an
// interrupted createdb, or someone else's data, looks like. It is never
// removed here: the bootstrap stops and says so.
func createHADatabase(ctx context.Context, cli CLI, target string, ha HAConfig, database, owner string) error {
	hosts, err := haHosts(ha.ConfPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		return fmt.Errorf("create the database root: %w", err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return fmt.Errorf("open the database root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(database, 0o750); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s exists although database %q is not registered; refusing to create over it",
				filepath.Join(target, database), database)
		}
		return fmt.Errorf("create the database directory: %w", err)
	}
	// Marked until createdb has succeeded: an interruption leaves a directory
	// a later attempt may remove.
	if err := markOwned(root, database, owner); err != nil {
		_ = root.RemoveAll(database)
		return err
	}
	dir := filepath.Join(target, database)
	out, err := cli.Run(ctx, "cubrid", "createdb",
		"--db-volume-size="+ha.VolumeSize,
		"--server-name="+hosts,
		"-F", dir,
		database, ha.Locale)
	if err != nil {
		// Nothing but this attempt wrote into the directory it just created.
		if registered, regErr := registeredInDatabasesTxt(target, database); regErr == nil && !registered {
			_ = root.RemoveAll(database)
		}
		return fmt.Errorf("createdb failed: %w: %s", err, strings.TrimSpace(out))
	}
	return clearOwned(target, database)
}
