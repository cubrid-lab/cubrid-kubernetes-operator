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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// OperationRebuild is what a restore's record keeps of the member rebuild it
// was started for: the rebuild's operation ID, and the SHA-256 digest of its
// token in hex, so that the record, which GET /v1/operations returns, never
// holds the token.
type OperationRebuild struct {
	OperationID string `json:"operationID"`
	TokenSHA256 string `json:"tokenSHA256"`
}

// operationRebuild is the record of binding b; nil for none.
func operationRebuild(b *RebuildBinding) *OperationRebuild {
	if b == nil {
		return nil
	}
	sum := sha256.Sum256([]byte(b.Token))
	return &OperationRebuild{OperationID: b.OperationID, TokenSHA256: hex.EncodeToString(sum[:])}
}

// RebuildReleaseRequest is the body of POST /v1/rebuild/release: the
// database, and the rebuild whose restore holds it.
type RebuildReleaseRequest struct {
	Database string         `json:"database"`
	Rebuild  RebuildBinding `json:"rebuild"`
}

// errRebuildNotHeld is why a release is refused: the database's marker names
// no completed restore of that rebuild under that token.
var errRebuildNotHeld = errors.New("the database is not held by a completed restore of this member rebuild")

// releaseRebuild removes the ownership marker that a completed restore for
// the member rebuild b left on database (runRestore). The Operator asks for
// it only once it judged the rebuild Completed: the member caught up with its
// source. Until then the marker keeps the image's entrypoint and
// ResumeCompletedRestores from starting the member again after a restart
// (ADR-0006: a rebuild target stays quarantined until it caught up). A
// database without the marker is not held, and the release succeeds; a marker
// of another operation, a restore that did not complete, another rebuild or
// another token is refused, and nothing is changed. A member that is
// stopping, for example after the Operator stopped a failed rebuild, is never
// released.
func (s *Server) releaseRebuild(database string, b RebuildBinding) error {
	if err := s.admit(); err != nil {
		return err
	}
	defer s.ops.Done()
	root, err := os.OpenRoot(s.restoreRoots.Target)
	if err != nil {
		return fmt.Errorf("open the database root: %w", err)
	}
	data, err := root.ReadFile(markerPath(database))
	_ = root.Close()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the ownership marker of %s: %w", database, err)
	}
	if s.store == nil {
		return fmt.Errorf("%w: this manager has no operation store", errRebuildNotHeld)
	}
	op, err := s.store.Get(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("%w: %v", errRebuildNotHeld, err)
	}
	want := operationRebuild(&b)
	if op.Kind != OpRestore || op.State != OpCompleted || op.Database != database || op.Rebuild == nil ||
		op.Rebuild.OperationID != want.OperationID ||
		subtle.ConstantTimeCompare([]byte(op.Rebuild.TokenSHA256), []byte(want.TokenSHA256)) != 1 {
		return errRebuildNotHeld
	}
	return clearOwned(s.restoreRoots.Target, database)
}

// rebuildRelease serves POST /v1/rebuild/release.
func (s *Server) rebuildRelease(w http.ResponseWriter, r *http.Request) {
	var req RebuildReleaseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "unreadable request"})
		return
	}
	if !databaseNamePattern.MatchString(req.Database) {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "database is not a database name"})
		return
	}
	if err := req.Rebuild.validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: err.Error()})
		return
	}
	err := s.releaseRebuild(req.Database, req.Rebuild)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{keyStatus: "released"})
	case errors.Is(err, errRebuildNotHeld):
		writeJSON(w, http.StatusConflict, map[string]string{errKey: err.Error()})
	case errors.Is(err, errStopping), errors.Is(err, errStarting):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{errKey: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{errKey: err.Error()})
	}
}
