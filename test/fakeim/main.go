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

// Command fakeim is a test-only stand-in for the Instance Manager image. It
// serves the real Instance Manager API over a scripted CUBRID CLI, so the
// operator's cluster wiring (Services, role probing, status) can run on any
// architecture without CUBRID. Nothing it reports comes from a database: a
// passing test that uses it is never real-database evidence.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// fakeEngineRelease is what cubrid_rel prints in cubrid/cubrid:11.4
// (docs/poc/RESULTS.md, POC-10).
const fakeEngineRelease = "\nCUBRID 11.4.6 (11.4.6.1963-0e7d3c1) " +
	"(64bit release build for Linux) (Sep  7 2026 17:45:11)\n\n"

// fakeCLI answers the commands the Instance Manager runs, from a role that a
// test can change. It implements instancemanager.CLI.
type fakeCLI struct {
	host     string
	database string

	mu   sync.Mutex
	role instancemanager.Role
}

// initialRole makes the first member (ordinal 0) the master and every other
// member a slave: a formed HA cluster, the starting point of a wiring test.
func initialRole(host string) instancemanager.Role {
	if strings.HasSuffix(host, "-0") {
		return instancemanager.RoleMaster
	}
	return instancemanager.RoleSlave
}

func (f *fakeCLI) Role() instancemanager.Role {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.role
}

func (f *fakeCLI) SetRole(role instancemanager.Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.role = role
}

func (f *fakeCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	switch command := strings.Join(append([]string{name}, args...), " "); command {
	case "cubrid heartbeat status":
		return f.heartbeatStatus()
	case "cubrid server status":
		return fmt.Sprintf("@ cubrid server status\n Server %s (rel 11.4.6, pid 1)\n", f.database), nil
	case "cubrid_rel":
		return fakeEngineRelease, nil
	case "cubrid heartbeat stop", "cubrid server stop " + f.database:
		return "", nil
	default:
		return "", fmt.Errorf("fakeim: %q is not scripted", command)
	}
}

// heartbeatStatus prints what `cubrid heartbeat status` prints for the current
// role, in the format instancemanager.ParseHAStatus reads. An unknown role is
// a node that is not in HA.
func (f *fakeCLI) heartbeatStatus() (string, error) {
	role := f.Role()
	serverState := "registered_and_standby"
	switch role {
	case instancemanager.RoleMaster:
		serverState = "registered_and_active"
	case instancemanager.RoleSlave:
	default:
		return "++ cubrid heartbeat status: fail\n", errors.New("exit status 1")
	}
	return fmt.Sprintf(" HA-Node Info (current %s, state %s)\n"+
		"   Node %s (priority 1, state %s)\n"+
		" HA-Process Info (master 1, state %s)\n"+
		"   Server %s (pid 1, state %s)\n",
		f.host, role, f.host, role, role, f.database, serverState), nil
}

// handleRole reports the scripted role, or changes it with ?set=master|slave|unknown.
// It is not authenticated: the image exists only for tests.
func (f *fakeCLI) handleRole(w http.ResponseWriter, r *http.Request) {
	if set := r.URL.Query().Get("set"); set != "" {
		role := instancemanager.Role(set)
		switch role {
		case instancemanager.RoleMaster, instancemanager.RoleSlave, instancemanager.RoleUnknown:
			f.SetRole(role)
		default:
			http.Error(w, "set must be master, slave or unknown", http.StatusBadRequest)
			return
		}
	}
	_, _ = fmt.Fprintln(w, f.Role())
}

// newHandler serves the Instance Manager API over cli, plus /fake/role.
func newHandler(cli *fakeCLI, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/fake/role", cli.handleRole)
	mux.Handle("/", instancemanager.NewServer(cli, token).Handler())
	return mux
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeim:", err)
		os.Exit(1)
	}
}

func run() error {
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	database := os.Getenv("CUBRID_DB")
	if database == "" {
		database = "appdb"
	}
	cli := &fakeCLI{host: host, database: database, role: initialRole(host)}
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", instancemanager.DefaultPort),
		Handler:           newHandler(cli, os.Getenv("IM_TOKEN")),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	fmt.Printf("fakeim (no CUBRID) listening on %s as %s, role %s\n", srv.Addr, host, cli.Role())

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
