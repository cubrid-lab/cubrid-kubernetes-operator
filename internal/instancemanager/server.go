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
	"encoding/json"
	"net/http"
)

// DefaultPort is the Instance Manager API port (ADR-0003).
const DefaultPort = 9090

const errKey = "error"

// Server exposes the Instance Manager HTTP/JSON API (ADR-0003). Kubelet probes
// (/livez, /readyz) are unauthenticated; /v1 endpoints require the bearer token.
type Server struct {
	cli   CLI
	token string
}

func NewServer(cli CLI, token string) *Server {
	return &Server{cli: cli, token: token}
}

// Handler builds the API mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.livez)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /v1/role", s.auth(s.role))
	mux.HandleFunc("GET /v1/ha/status", s.auth(s.haStatus))
	mux.HandleFunc("POST /v1/backup", s.auth(s.backup))
	mux.HandleFunc("POST /v1/shutdown", s.auth(s.shutdown))
	return mux
}

// livez reports that the manager process is alive.
func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports DB-instance readiness only (NOT cluster HA health, #14). The
// instance is ready when it holds an authoritative master/slave role.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	st := HeartbeatStatus(r.Context(), s.cli)
	if st.Role == RoleMaster || st.Role == RoleSlave || st.Role == RoleReplica {
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "role": st.Role})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": st.Reason})
}

// role returns the local node's authoritative CUBRID role.
func (s *Server) role(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HeartbeatStatus(r.Context(), s.cli))
}

// haStatus returns the full parsed HA view (role + topology).
func (s *Server) haStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HeartbeatStatus(r.Context(), s.cli))
}

// backup runs a local `cubrid backupdb` (ADR-0007).
func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	var req BackupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "invalid request body"})
		return
	}
	res, err := Backup(r.Context(), s.cli, req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// shutdown performs the ADR-0003 ordered graceful shutdown (withdraw HA, stop server).
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	if err := Shutdown(r.Context(), s.cli, r.URL.Query().Get("database")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutdown"})
}

// auth wraps /v1 handlers with bearer-token authentication (loopback is exempt
// so preStop can call locally without a token; ADR-0003).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" || isLoopback(r.RemoteAddr) {
			next(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{errKey: "unauthorized"})
			return
		}
		next(w, r)
	}
}

func isLoopback(remoteAddr string) bool {
	host := remoteAddr
	if i := indexByte(remoteAddr, ':'); i >= 0 {
		host = remoteAddr[:i]
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func indexByte(s string, b byte) int {
	// last colon splits host:port; use the last so IPv6 hosts without a port
	// are not mis-split. RemoteAddr always has host:port from net/http.
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
