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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The rebuild the restores in these tests run for, and the binding of
// another, later rebuild request.
var (
	testRebuild  = RebuildBinding{OperationID: "rebuild-op-1", Token: "0123456789abcdef0123456789abcdef"}
	otherRebuild = RebuildBinding{OperationID: "rebuild-op-2", Token: "fedcba9876543210fedcba9876543210"}
)

// sourceIM stands in for the Instance Manager of the rebuild's source member.
// It answers GET /v1/role with answer, and records who asked and with which
// token.
type sourceIM struct {
	mu     sync.Mutex
	asked  []string
	tokens []string
	// answer is what the source reports; nil answers 500.
	answer *HAStatus
	// delay holds every answer back this long.
	delay time.Duration
	// raw, when set, answers instead of answer.
	raw http.HandlerFunc
	// then, when set, answers every request after the first instead of
	// answer: the source's role changed after the restore asked it first.
	then *HAStatus
}

func (f *sourceIM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.asked = append(f.asked, r.Method+" "+r.URL.Path)
	f.tokens = append(f.tokens, r.Header.Get("Authorization"))
	answer, delay, raw := f.answer, f.delay, f.raw
	if f.then != nil && len(f.asked) > 1 {
		answer = f.then
	}
	f.mu.Unlock()
	if raw != nil {
		raw(w, r)
		return
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if answer == nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

func (f *sourceIM) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// activeMasterAnswer is what the source's manager reports while it is the
// active master; slaveAnswer is what it reports once CUBRID made it a slave.
func activeMasterAnswer() *HAStatus {
	return &HAStatus{Current: seedMaster, Role: RoleMaster, ServerActive: true, Source: heartbeatCmd,
		Nodes: []NodeState{{Name: seedMaster, Priority: 1, State: string(RoleMaster)}}}
}

func slaveAnswer() *HAStatus {
	return &HAStatus{Current: seedMaster, Role: RoleSlave, Source: heartbeatCmd,
		Nodes: []NodeState{{Name: seedMaster, Priority: 1, State: "slave"}}}
}

// rebuildRestore is one restore of a member being rebuilt, and what it left.
type rebuildRestore struct {
	final   Operation
	cli     *stepCLI
	objects *countingStore
	req     RestoreRequest
	source  *sourceIM
	srv     *Server
	h       http.Handler
	store   *OperationStore
}

// rebuildCase says how a rebuild restore is set up.
type rebuildCase struct {
	// request is the binding the restore request carries; nil sends none.
	request *RebuildBinding
	// manifest is the binding the artifact's manifest carries; nil has none.
	manifest *RebuildBinding
	// source is the source member's manager.
	source *sourceIM
	// prepareTarget, when set, lays out the target and the operation store
	// before the restore.
	prepareTarget func(t *testing.T, target string, store *OperationStore)
	// sourceDown closes the source's manager before the restore, so that a
	// connection to it is refused.
	sourceDown bool
	// sourceTimeout bounds the question to the source; zero keeps the default.
	sourceTimeout time.Duration
}

// runRebuildRestore posts a seed restore of member demo-2 from demo-0 and
// waits for it to settle. The member is an HA member whose heartbeat is not
// running, so a restore that goes through completes.
func runRebuildRestore(t *testing.T, c rebuildCase) rebuildRestore {
	t.Helper()
	fake, bucket, prefix := stageSeedArtifact(t, func(m *BackupManifest) {
		if c.manifest != nil {
			b := *c.manifest
			m.Rebuild = &b
		}
	})
	objects := &countingStore{fakeObjectStore: fake}
	req := baseRestoreRequest(t, bucket, prefix)
	req.SeedFromMaster, req.ExpectedClusterUID, req.ExpectedMember = seedMaster, seedClusterUID, namedMember
	req.Rebuild = c.request
	ts := httptest.NewServer(c.source)
	t.Cleanup(ts.Close)
	if c.sourceDown {
		ts.Close()
	}

	conf := filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(conf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { awaitTerminalOperations(t, store) })
	if c.prepareTarget != nil {
		c.prepareTarget(t, req.TargetDir, store)
	}
	roots := rootsFor(req)
	roots.Member = namedMember
	cli := &stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).WithRestoreRoots(roots).
		WithHAConfig(HAConfig{ConfPath: conf})
	srv.heartbeatProcesses, srv.heartbeatSettle = processTable(), 0
	srv.sourceRoleURL = func(member string) string { return ts.URL + "/v1/role?member=" + member }
	srv.sourceRoleTimeout = c.sourceTimeout
	h := srv.Handler()

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRequest(http.MethodPost, pathRestorePrepare, bytes.NewReader(body))
	post.Header.Set("Authorization", "Bearer tok")
	post.Header.Set("Idempotency-Key", "rebuild-restore")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("/v1/restore/prepare = %d: %s", rr.Code, rr.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	return rebuildRestore{final: pollUntilSettled(t, srv, h, op.ID), cli: cli, objects: objects, req: req, source: c.source,
		srv: srv, h: h, store: store}
}

// leftNothing checks that a refused rebuild restore ran no command, staged
// nothing and left the database root as it was before: no directory, no
// ownership marker and no databases.txt entry.
func (r rebuildRestore) leftNothing(t *testing.T) {
	t.Helper()
	if calls := r.cli.recorded(); len(calls) != 0 {
		t.Errorf("a refused restore ran commands: %q", calls)
	}
	if _, err := os.Stat(r.req.StagingDir); !os.IsNotExist(err) {
		t.Errorf("a refused restore left its staging directory (err=%v)", err)
	}
	if entries, err := os.ReadDir(r.req.TargetDir); err != nil || len(entries) != 0 {
		t.Errorf("a refused restore changed the database root: %v, %v", entries, err)
	}
}

// A restore for an accepted rebuild asks the named source's Instance Manager
// for its role first. A source that answers as a slave is no longer the
// master the backup was taken on, and following it would copy a stale or
// diverged database, so the restore is refused before the artifact is read,
// anything is staged or the target gets an ownership marker.
func TestServer_RebuildRestore_RefusesASourceThatAnswersAsASlave(t *testing.T) {
	r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: &testRebuild,
		source: &sourceIM{answer: slaveAnswer()}})
	if r.final.State != OpFailed || r.final.FailureCode != FailureRebuildSourceNotMaster {
		t.Fatalf("final = %s with code %q (%s), want Failed with %q", r.final.State, r.final.FailureCode,
			r.final.FailureReason, FailureRebuildSourceNotMaster)
	}
	if !strings.Contains(r.final.FailureReason, seedMaster) {
		t.Errorf("reason %q does not name the source", r.final.FailureReason)
	}
	if r.objects.gets != 0 {
		t.Errorf("the refused restore read the artifact %d times", r.objects.gets)
	}
	r.leftNothing(t)
}

// Every other answer that does not show an active master is refused the same
// way: a master whose server is not active, an answer that names another
// member, and an answer without a role.
func TestServer_RebuildRestore_RefusesAnyAnswerThatIsNoActiveMaster(t *testing.T) {
	notActive := activeMasterAnswer()
	notActive.ServerActive = false
	otherName := activeMasterAnswer()
	otherName.Current = "demo-3"
	noRole := &HAStatus{Role: RoleUnknown, Reason: "heartbeat not running"}
	for name, answer := range map[string]*HAStatus{
		"a master whose server is not active": notActive,
		"an answer of another member":         otherName,
		"an answer without a role":            noRole,
	} {
		t.Run(name, func(t *testing.T) {
			r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: &testRebuild,
				source: &sourceIM{answer: answer}})
			if r.final.State != OpFailed || r.final.FailureCode != FailureRebuildSourceNotMaster {
				t.Fatalf("final = %s with code %q (%s), want Failed with %q", r.final.State, r.final.FailureCode,
					r.final.FailureReason, FailureRebuildSourceNotMaster)
			}
			if r.objects.gets != 0 {
				t.Errorf("the refused restore read the artifact %d times", r.objects.gets)
			}
			r.leftNothing(t)
		})
	}
}

// A source whose Instance Manager does not answer, answers with an error or
// answers too late shows nothing about its role, so the restore is refused
// with a code of its own, again before anything is read or written.
func TestServer_RebuildRestore_RefusesASourceThatCannotBeAsked(t *testing.T) {
	for name, c := range map[string]rebuildCase{
		"an error answer": {source: &sourceIM{}},
		"a timeout": {source: &sourceIM{answer: activeMasterAnswer(), delay: 2 * time.Second},
			sourceTimeout: 100 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			c.request, c.manifest = &testRebuild, &testRebuild
			r := runRebuildRestore(t, c)
			if r.final.State != OpFailed || r.final.FailureCode != FailureRebuildSourceUnconfirmed {
				t.Fatalf("final = %s with code %q (%s), want Failed with %q", r.final.State, r.final.FailureCode,
					r.final.FailureReason, FailureRebuildSourceUnconfirmed)
			}
			if r.objects.gets != 0 {
				t.Errorf("the refused restore read the artifact %d times", r.objects.gets)
			}
			r.leftNothing(t)
		})
	}
}

// An artifact taken for another rebuild, or under another token of the same
// one, or for no rebuild at all, is refused once its manifest is read and
// before any object is staged: a restore for one request cannot reuse the
// copy of another.
func TestServer_RebuildRestore_RefusesAnArtifactOfAnotherRebuild(t *testing.T) {
	sameIDOtherToken := RebuildBinding{OperationID: testRebuild.OperationID, Token: otherRebuild.Token}
	for name, manifest := range map[string]*RebuildBinding{
		"another rebuild":                      &otherRebuild,
		"the same rebuild under another token": &sameIDOtherToken,
		"no rebuild":                           nil,
	} {
		t.Run(name, func(t *testing.T) {
			r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: manifest,
				source: &sourceIM{answer: activeMasterAnswer()}})
			if r.final.State != OpFailed {
				t.Fatalf("final = %s (%s), want Failed", r.final.State, r.final.FailureReason)
			}
			if !strings.Contains(r.final.FailureReason, "rebuild") {
				t.Errorf("reason %q does not say the artifact belongs to another rebuild", r.final.FailureReason)
			}
			if strings.Contains(r.final.FailureReason, testRebuild.Token) || strings.Contains(r.final.FailureReason, otherRebuild.Token) {
				t.Errorf("reason %q carries a token", r.final.FailureReason)
			}
			r.leftNothing(t)
		})
	}
}

// The target-holds-database guard still runs after the source was confirmed.
// A replacement claim can be bound to a retained volume whose database the
// Operator has no record of; its files are refused with TargetHoldsDatabase
// and left exactly as they were, whatever the source answers.
func TestServer_RebuildRestore_RefusesATargetHoldingUnrecordedDatabaseFiles(t *testing.T) {
	files := map[string]string{
		filepath.Join(dbName, dbName):           "data volume",
		filepath.Join(dbName, dbName+"_lgat"):   "active log",
		filepath.Join(dbName, dbName+"_vinf"):   "volume information",
		filepath.Join(dbName, "lob", "ces_123"): "lob",
	}
	r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: &testRebuild,
		source: &sourceIM{answer: activeMasterAnswer()},
		prepareTarget: func(t *testing.T, target string, _ *OperationStore) {
			for name, content := range files {
				path := filepath.Join(target, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}})
	if r.final.State != OpFailed || r.final.FailureCode != FailureTargetHoldsDatabase {
		t.Fatalf("final = %s with code %q (%s), want Failed with %q", r.final.State, r.final.FailureCode,
			r.final.FailureReason, FailureTargetHoldsDatabase)
	}
	if calls := r.cli.recorded(); len(calls) != 0 {
		t.Errorf("calls = %q, want none", calls)
	}
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(r.req.TargetDir, name)) // #nosec G304 -- the test's own target
		if err != nil || string(got) != content {
			t.Errorf("%s = %q, %v; want it untouched (%q)", name, got, err, content)
		}
	}
	if _, err := os.Stat(filepath.Join(r.req.TargetDir, dbName, ownershipMarker)); !os.IsNotExist(err) {
		t.Errorf("the refused restore marked the database directory (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(r.req.TargetDir, databasesTxt)); !os.IsNotExist(err) {
		t.Errorf("the refused restore wrote databases.txt (err=%v)", err)
	}
	if got := r.source.requests(); len(got) != 1 {
		t.Errorf("the source was asked %d times (%q), want once before the guard ran", len(got), got)
	}
}

// With the source confirmed as the active master and an artifact of the same
// rebuild, the restore runs as a seed restore: the member is restored as a
// slave of the source. The source was asked for its role once, at the
// member-name address, with the cluster's token.
func TestServer_RebuildRestore_ProceedsWithAMatchingArtifactAndAnActiveMaster(t *testing.T) {
	r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: &testRebuild,
		source: &sourceIM{answer: activeMasterAnswer()}})
	if r.final.State != OpCompleted {
		t.Fatalf("final = %s (%s), want Completed", r.final.State, r.final.FailureReason)
	}
	calls := r.cli.recorded()
	if len(calls) == 0 || !strings.HasPrefix(calls[0], "cubrid restoreslave -u -s master -m "+seedMaster+" ") {
		t.Errorf("calls = %q, want restoreslave from %s first", calls, seedMaster)
	}
	// Once before anything is touched, and again right before heartbeat
	// is started.
	asked := r.source.requests()
	if len(asked) != 2 || asked[0] != "GET /v1/role" || asked[1] != "GET /v1/role" {
		t.Errorf("source requests = %q, want GET /v1/role before the restore and before the start", asked)
	}
	if r.source.tokens[0] != "Bearer tok" {
		t.Errorf("the source was asked with %q, want the cluster's token", r.source.tokens[0])
	}
}

// A restore that names no rebuild works as before: a seed restore of the HA
// bootstrap neither asks the source nor needs a rebuild in the manifest, and
// an artifact that carries one is still taken.
func TestServer_RestoreWithoutRebuild_KeepsTheBootstrapSeedBehaviour(t *testing.T) {
	for name, manifest := range map[string]*RebuildBinding{"a plain seed artifact": nil, "a rebuild artifact": &testRebuild} {
		t.Run(name, func(t *testing.T) {
			r := runRebuildRestore(t, rebuildCase{manifest: manifest, source: &sourceIM{answer: slaveAnswer()}})
			if r.final.State != OpCompleted {
				t.Fatalf("final = %s (%s), want Completed", r.final.State, r.final.FailureReason)
			}
			if got := r.source.requests(); len(got) != 0 {
				t.Errorf("a restore without a rebuild asked the source: %q", got)
			}
		})
	}
}

// The address of the source's manager is its per-member Service qualified
// with the namespace, as the Operator names it (ADR-0004): a bare member name
// would be completed by the DNS search list, and the token could go to
// whatever host that list makes of it.
func TestSourceRoleURL_IsTheMemberServiceInTheNamespace(t *testing.T) {
	got, err := defaultSourceRoleURL("demo-0", "shop")
	if want := "http://demo-0.shop.svc:9090/v1/role"; err != nil || got != want {
		t.Errorf("defaultSourceRoleURL = %q, %v; want %q", got, err, want)
	}
}

// names that are no DNS label: each could name another host, a user, a port
// or a path in an address.
var notMemberNames = []string{"demo-0.evil.example", "demo-0/x", "user@demo-0", "demo-0:80", "Demo-0",
	strings.Repeat("a", 64), "", "-demo", "demo-"}

// No address is built from a source or a namespace that is not a DNS label.
func TestSourceRoleURL_RefusesNamesThatAreNoDNSLabel(t *testing.T) {
	for _, name := range notMemberNames {
		if got, err := defaultSourceRoleURL(name, "shop"); err == nil {
			t.Errorf("defaultSourceRoleURL(source %q) = %q, want a refusal", name, got)
		}
		if got, err := defaultSourceRoleURL("demo-0", name); err == nil {
			t.Errorf("defaultSourceRoleURL(namespace %q) = %q, want a refusal", name, got)
		}
	}
}

// A rebuild restore whose source is not a member name is answered 400 before
// it is recorded, so no request is ever sent to an address built from it.
func TestServer_RebuildRestore_RefusesASourceThatIsNoMemberName(t *testing.T) {
	for _, name := range notMemberNames {
		t.Run(name, func(t *testing.T) {
			fake, bucket, prefix := stageUploadedArtifact(t)
			objects := &countingStore{fakeObjectStore: fake}
			req := baseRestoreRequest(t, bucket, prefix)
			req.SeedFromMaster, req.ExpectedMember = name, namedMember
			b := testRebuild
			req.Rebuild = &b
			rr, cli, storeDir, _ := postRestore(t, namedMember, req, objects)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("rebuild restore from %q = %d (%s), want 400", name, rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if calls := cli.recorded(); len(calls) != 0 || objects.gets != 0 {
				t.Errorf("refused request ran %q and read the artifact %d times", calls, objects.gets)
			}
			if entries, err := os.ReadDir(storeDir); err != nil || len(entries) != 0 {
				t.Errorf("refused request changed the operation store: %v, %v", entries, err)
			}
		})
	}
}

// A manager whose POD_NAMESPACE is not a DNS label asks nobody either.
func TestServer_RebuildRestore_WithAnInvalidNamespaceAsksNobody(t *testing.T) {
	srv := NewServer(&stepCLI{}, "tok").WithNamespace("shop.evil.example")
	code, err := srv.confirmRebuildSource(t.Context(), seedMaster)
	if code != FailureRebuildSourceUnconfirmed || err == nil || !strings.Contains(err.Error(), "POD_NAMESPACE") {
		t.Errorf("confirmRebuildSource with an invalid namespace = %q, %v; want %q naming POD_NAMESPACE",
			code, err, FailureRebuildSourceUnconfirmed)
	}
}

// A manager that does not know its namespace cannot name the source's
// address, so it asks nobody and refuses the restore before anything is read
// or written.
func TestServer_RebuildRestore_WithoutANamespaceAsksNobody(t *testing.T) {
	srv := NewServer(&stepCLI{}, "tok")
	code, err := srv.confirmRebuildSource(t.Context(), seedMaster)
	if code != FailureRebuildSourceUnconfirmed || err == nil || !strings.Contains(err.Error(), "POD_NAMESPACE") {
		t.Errorf("confirmRebuildSource without a namespace = %q, %v; want %q naming POD_NAMESPACE",
			code, err, FailureRebuildSourceUnconfirmed)
	}
}

// What a failed restore of this manager left in the target (its ownership
// marker, with no data recorded as restored) is removed by the next restore
// once it goes on. A rebuild restore whose source answers as a slave does not
// go on: the source is asked before the leftover is reclaimed or taken over,
// so the leftover stays exactly as it was.
func TestServer_RebuildRestore_RefusedSourceLeavesAFailedRestoresLeftoversAlone(t *testing.T) {
	files := map[string]string{
		filepath.Join(dbName, dbName):         "partial data volume",
		filepath.Join(dbName, dbName+"_lgat"): "partial active log",
	}
	var marker string
	r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: &testRebuild,
		source: &sourceIM{answer: slaveAnswer()},
		prepareTarget: func(t *testing.T, target string, store *OperationStore) {
			op, _, err := store.FindOrCreate(OpRestore, "an-earlier-restore", "hash", dbName)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Settle(op.ID, func(o *Operation) { o.State, o.FailureReason = OpFailed, "restoredb failed" }); err != nil {
				t.Fatal(err)
			}
			marker = op.ID + "\n"
			files[filepath.Join(dbName, ownershipMarker)] = marker
			for name, content := range files {
				path := filepath.Join(target, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}})
	if r.final.State != OpFailed || r.final.FailureCode != FailureRebuildSourceNotMaster {
		t.Fatalf("final = %s with code %q (%s), want Failed with %q", r.final.State, r.final.FailureCode,
			r.final.FailureReason, FailureRebuildSourceNotMaster)
	}
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(r.req.TargetDir, name)) // #nosec G304 -- the test's own target
		if err != nil || string(got) != content {
			t.Errorf("%s = %q, %v; want the leftover untouched (%q)", name, got, err, content)
		}
	}
	if calls := r.cli.recorded(); len(calls) != 0 {
		t.Errorf("calls = %q, want none", calls)
	}
	if r.objects.gets != 0 {
		t.Errorf("the refused restore read the artifact %d times", r.objects.gets)
	}
}

// Answers that are no readable role answer of the source show nothing about
// its role: a refused connection, a redirect (which is not followed, so the
// answer cannot come from another host), a refused token and a body that is
// not JSON. Each is refused as unconfirmed, with nothing staged or written.
func TestServer_RebuildRestore_RefusesUnusableSourceAnswers(t *testing.T) {
	for name, c := range map[string]rebuildCase{
		"a refused connection": {source: &sourceIM{answer: activeMasterAnswer()}, sourceDown: true},
		"a redirect": {source: &sourceIM{raw: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.invalid/v1/role", http.StatusFound)
		}}},
		"a refused token": {source: &sourceIM{raw: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}}},
		"a body that is not JSON": {source: &sourceIM{raw: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{\"role\": master"))
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			c.request, c.manifest = &testRebuild, &testRebuild
			r := runRebuildRestore(t, c)
			if r.final.State != OpFailed || r.final.FailureCode != FailureRebuildSourceUnconfirmed {
				t.Fatalf("final = %s with code %q (%s), want Failed with %q", r.final.State, r.final.FailureCode,
					r.final.FailureReason, FailureRebuildSourceUnconfirmed)
			}
			if r.objects.gets != 0 {
				t.Errorf("the refused restore read the artifact %d times", r.objects.gets)
			}
			r.leftNothing(t)
		})
	}
}

// A rebuild restore is a seed restore that names the member it is for and a
// source other than that member; anything else is refused as a request,
// before it is recorded.
func TestValidateRestoreRequest_RebuildNeedsASeedAndANamedMember(t *testing.T) {
	base := func() RestoreRequest {
		req := baseRestoreRequest(t, "bucket", "prefix")
		req.SeedFromMaster, req.ExpectedMember = seedMaster, namedMember
		b := testRebuild
		req.Rebuild = &b
		return req
	}
	if err := validateRestoreRequest(base()); err != nil {
		t.Fatalf("a complete rebuild restore was refused: %v", err)
	}
	for name, mutate := range map[string]func(*RestoreRequest){
		"no seedFromMaster":            func(r *RestoreRequest) { r.SeedFromMaster = "" },
		"no expectedMember":            func(r *RestoreRequest) { r.ExpectedMember = "" },
		"the member is its own source": func(r *RestoreRequest) { r.ExpectedMember = seedMaster },
		"no operation ID":              func(r *RestoreRequest) { r.Rebuild.OperationID = "" },
		"an empty token":               func(r *RestoreRequest) { r.Rebuild.Token = "" },
		"an operation ID with a path":  func(r *RestoreRequest) { r.Rebuild.OperationID = "../x" },
		"a token with white space":     func(r *RestoreRequest) { r.Rebuild.Token = "a b" },
	} {
		t.Run(name, func(t *testing.T) {
			req := base()
			mutate(&req)
			if err := validateRestoreRequest(req); err == nil {
				t.Errorf("validateRestoreRequest accepted %+v", req)
			}
		})
	}
}

// A seed backup for a rebuild records the rebuild in its manifest, which is
// what the restore compares with its own request.
func TestServer_RebuildBackup_RecordsTheRebuildInTheManifest(t *testing.T) {
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &backupRoleCLI{writingCLI: newWritingCLI(), before: []heartbeatAnswer{heartbeatMaster}}
	objects := newFakeStore()
	root := t.TempDir()
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).WithBackupStagingRoot(root)
	srv.backupRoleBound, srv.backupRolePoll = 100*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { awaitTerminalOperations(t, store) })
	upload := *testUpload
	upload.SourceRole = string(RoleMaster)
	upload.Seed = true
	upload.Prefix = "prod/" + upload.ClusterUID + "/" + RebuildSeedDir(testRebuild.OperationID) + "/" + dbName
	b := testRebuild
	upload.Rebuild = &b
	body, err := json.Marshal(BackupRequest{Database: dbName, Destination: filepath.Join(root, "uid-1"), Upload: &upload})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	rr := postBackup(t, h, "key-1", string(body))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /v1/backup = %d: %s", rr.Code, rr.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	final := pollUntilSettled(t, srv, h, op.ID)
	if final.State != OpCompleted {
		t.Fatalf("final = %s (%s), want Completed", final.State, final.FailureReason)
	}
	for key, data := range objects.objects {
		if strings.HasSuffix(key, "/"+ManifestObjectName) {
			m, err := ParseManifest(data)
			if err != nil {
				t.Fatal(err)
			}
			if m.Rebuild == nil || *m.Rebuild != testRebuild {
				t.Errorf("manifest rebuild = %+v, want %+v", m.Rebuild, testRebuild)
			}
			return
		}
	}
	t.Fatal("no manifest was uploaded")
}

// Only a seed backup is taken for a rebuild: the restore that reads it
// follows the master it was taken on.
func TestValidateBackupRequest_RebuildNeedsASeedUpload(t *testing.T) {
	upload := *testUpload
	b := testRebuild
	upload.Rebuild = &b
	if err := validateBackupRequest(BackupRequest{Database: dbName, Destination: "/x", Upload: &upload}); err == nil ||
		!strings.Contains(err.Error(), "seed") {
		t.Errorf("validateBackupRequest of a non-seed rebuild backup = %v, want a refusal that names the seed", err)
	}
	upload.Seed, upload.SourceRole = true, string(RoleMaster)
	upload.Prefix = "prod/" + upload.ClusterUID + "/" + RebuildSeedDir(testRebuild.OperationID) + "/" + dbName
	if err := validateBackupRequest(BackupRequest{Database: dbName, Destination: "/x", Upload: &upload}); err != nil {
		t.Errorf("validateBackupRequest refused a rebuild backup to its own location: %v", err)
	}
	upload.Rebuild = &RebuildBinding{OperationID: testRebuild.OperationID}
	if err := validateBackupRequest(BackupRequest{Database: dbName, Destination: "/x", Upload: &upload}); err == nil {
		t.Error("validateBackupRequest accepted a rebuild without a token")
	}
}

// GET /v1/role reports the check, so that the Operator sends a rebuild
// restore only to a manager that enforces it (ADR-0013).
func TestCapabilities_ReportTheRebuildRestoreCheck(t *testing.T) {
	if !slices.Contains(Capabilities(), CapabilityRebuildRestore) {
		t.Errorf("capabilities = %v, want %q among them", Capabilities(), CapabilityRebuildRestore)
	}
}

// A rebuild's seed backup goes only to the location of that rebuild,
// <clusterUID>/rebuild-<operationID>/<database>. The HA bootstrap's seed
// locations ("seed" and "seed-<artifact>") and another rebuild's location are
// refused, at the request and again at the upload, so a rebuild's backup can
// never replace the bootstrap's seed or another rebuild's copy.
func TestRebuildBackup_RefusesEveryLocationButItsOwn(t *testing.T) {
	for _, dir := range []string{"seed", "seed-demo-1-from-demo-0", "seed-" + RebuildSeedDir(testRebuild.OperationID),
		RebuildSeedDir(otherRebuild.OperationID), "rebuild-"} {
		t.Run(dir, func(t *testing.T) {
			upload := *testUpload
			b := testRebuild
			upload.Seed, upload.SourceRole, upload.Rebuild = true, string(RoleMaster), &b
			upload.Prefix = "prod/" + upload.ClusterUID + "/" + dir + "/" + dbName
			if err := validateBackupRequest(BackupRequest{Database: dbName, Destination: "/x", Upload: &upload}); err == nil ||
				!strings.Contains(err.Error(), RebuildSeedDir(testRebuild.OperationID)) {
				t.Errorf("validateBackupRequest to %s = %v, want a refusal naming the rebuild's location", dir, err)
			}
			staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
			store := newFakeStore()
			spec := baseSpec(staging)
			spec.Seed, spec.Prefix = true, upload.Prefix
			spec.Manifest.ClusterUID, spec.Manifest.SourceRole, spec.Manifest.Rebuild = upload.ClusterUID, string(RoleMaster), &b
			if _, err := UploadBackup(t.Context(), store, spec); err == nil {
				t.Errorf("UploadBackup of a rebuild seed to %s succeeded", dir)
			}
			if len(store.objects) != 0 {
				t.Errorf("UploadBackup to %s wrote %d objects", dir, len(store.objects))
			}
		})
	}
}

// An HA bootstrap seed, which names no rebuild, still goes to its stable
// location.
func TestBootstrapSeedUpload_KeepsItsStableLocation(t *testing.T) {
	upload := *testUpload
	upload.Seed, upload.SourceRole = true, string(RoleMaster)
	upload.Prefix = "prod/" + upload.ClusterUID + "/seed/" + dbName
	if err := validateBackupRequest(BackupRequest{Database: dbName, Destination: "/x", Upload: &upload}); err != nil {
		t.Errorf("validateBackupRequest refused the bootstrap seed location: %v", err)
	}
}
