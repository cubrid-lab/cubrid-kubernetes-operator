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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// aloneHolderOut is the member that holds a new cluster's database while
	// no peer has joined: CUBRID calls it master, and its server stays
	// registered_and_to_be_active (docs/poc/RESULTS.md, POC-13).
	aloneHolderOut = ` HA-Node Info (current cub-0, state master)
   Node cub-1 (priority 2, state unknown)
   Node cub-0 (priority 1, state master)
 HA-Process Info (master 123, state master)
   Server appdb (pid 126, state registered_and_to_be_active)
`
	replicaOut = ` HA-Node Info (current cub-2, state replica)
   Node cub-2 (priority 3, state replica)
   Node cub-0 (priority 1, state master)
`
)

// heartbeatAnswer is one answer of `cubrid heartbeat status`.
type heartbeatAnswer struct {
	out string
	err error
}

var (
	heartbeatMaster   = heartbeatAnswer{out: masterOut}
	heartbeatSlave    = heartbeatAnswer{out: slaveOut}
	heartbeatFailed   = heartbeatAnswer{err: errors.New("exit status 1")}
	heartbeatInFlight = heartbeatAnswer{out: transitionOut}
)

// backupRoleCLI is a writingCLI whose `cubrid heartbeat status` gives the answers
// in turn while backupdb has not run and afterwards the answers of after. The
// last answer of each list repeats.
type backupRoleCLI struct {
	*writingCLI
	mu          sync.Mutex
	before      []heartbeatAnswer
	after       []heartbeatAnswer
	backupRan   bool
	statusCalls int
}

func (c *backupRoleCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	if strings.Join(args, " ") == "heartbeat status" {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.statusCalls++
		answers := &c.before
		if c.backupRan && len(c.after) > 0 {
			answers = &c.after
		}
		a := (*answers)[0]
		if len(*answers) > 1 {
			*answers = (*answers)[1:]
		}
		return a.out, a.err
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "backup") {
		c.mu.Lock()
		c.backupRan = true
		c.mu.Unlock()
	}
	return c.writingCLI.Run(ctx, name, args...)
}

func (c *backupRoleCLI) statusReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusCalls
}

func (c *backupRoleCLI) ranBackup() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backupRan
}

// roleBackup is one backup of a fake member.
type roleBackup struct {
	cli     *backupRoleCLI
	objects *fakeObjectStore
	final   Operation
	root    string
}

// runRoleBackup runs one backup that names role on a member whose heartbeat
// gives before and after, and returns once it ended. The wait for the role is
// short, so a refusal takes milliseconds.
func runRoleBackup(t *testing.T, role Role, seed, standalone bool, before, after []heartbeatAnswer) roleBackup {
	t.Helper()
	r := startRoleBackup(t, role, seed, standalone, before, after, 100*time.Millisecond)
	r.final = pollUntilSettled(t, r.srv, r.h, r.id)
	return r.roleBackup
}

// roleRun is a backup of a fake member that was started and not yet waited
// for.
type roleRun struct {
	roleBackup
	srv *Server
	h   http.Handler
	id  string
}

// startRoleBackup starts the backup of runRoleBackup, with bound as the
// wait for the role, and returns without waiting for it.
func startRoleBackup(t *testing.T, role Role, seed, standalone bool, before, after []heartbeatAnswer, bound time.Duration) roleRun {
	t.Helper()
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	cli := &backupRoleCLI{writingCLI: newWritingCLI(), before: before, after: after}
	objects := newFakeStore()
	root := t.TempDir()
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).WithBackupStagingRoot(root)
	srv.backupRoleBound, srv.backupRolePoll = bound, 5*time.Millisecond
	if standalone {
		srv = srv.WithStandaloneDatabase(dbName)
	}
	t.Cleanup(func() { awaitTerminalOperations(t, store) })

	upload := *testUpload
	upload.SourceRole = string(role)
	if seed {
		upload.Seed = true
		upload.Prefix = "prod/" + upload.ClusterUID + "/seed/" + dbName
	}
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
	return roleRun{roleBackup: roleBackup{cli: cli, objects: objects, root: root}, srv: srv, h: h, id: op.ID}
}

// manifestRole returns the role the manifest of a completed backup records.
func (b roleBackup) manifestRole(t *testing.T) Role {
	t.Helper()
	if b.final.State != OpCompleted {
		t.Fatalf("final state = %s (reason %q), want Completed", b.final.State, b.final.FailureReason)
	}
	for key, data := range b.objects.objects {
		if strings.HasSuffix(key, "/"+ManifestObjectName) {
			m, err := ParseManifest(data)
			if err != nil {
				t.Fatal(err)
			}
			return Role(m.SourceRole)
		}
	}
	t.Fatal("no manifest was uploaded")
	return ""
}

// refused checks that the backup failed, that its reason holds phrase and
// names every role, and that nothing it took reached object storage.
func (b roleBackup) refused(t *testing.T, phrase string, roles ...Role) {
	t.Helper()
	if b.final.State != OpFailed {
		t.Fatalf("final state = %s (reason %q), want Failed", b.final.State, b.final.FailureReason)
	}
	if !strings.Contains(b.final.FailureReason, phrase) {
		t.Errorf("reason %q does not say %q", b.final.FailureReason, phrase)
	}
	for _, role := range roles {
		if !strings.Contains(b.final.FailureReason, string(role)) {
			t.Errorf("reason %q does not name the role %q", b.final.FailureReason, role)
		}
	}
	if len(b.objects.objects) != 0 {
		t.Errorf("objects were uploaded: %d", len(b.objects.objects))
	}
	if b.final.Artifact != nil {
		t.Errorf("a failed backup carries an artifact: %+v", b.final.Artifact)
	}
	entries, err := os.ReadDir(filepath.Join(b.root, "uid-1"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("staging was left behind: %v", entries)
	}
}

// A failover between the Operator's choice and the start of the backup leaves
// the request naming a role the member no longer has. A slave's backup that
// said master would pass the seed check of a restore, so the backup stops
// before backupdb, says which two roles differ, and uploads nothing (#755).
func TestServer_Backup_RoleThatDiffersFromTheRequestIsRefused(t *testing.T) {
	cases := []struct {
		name      string
		requested Role
		member    heartbeatAnswer
		reported  Role
	}{
		{"a request for the master reaches a slave", RoleMaster, heartbeatSlave, RoleSlave},
		{"a request for a slave reaches the new master", RoleSlave, heartbeatMaster, RoleMaster},
		{"a request for a replica reaches a slave", RoleReplica, heartbeatSlave, RoleSlave},
		{"a request for a slave reaches a replica", RoleSlave, heartbeatAnswer{out: replicaOut}, RoleReplica},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := runRoleBackup(t, tc.requested, false, false, []heartbeatAnswer{tc.member}, nil)
			b.refused(t, "backup refused", tc.requested, tc.reported)
			if b.cli.ranBackup() {
				t.Error("backupdb ran on a member that does not have the requested role")
			}
		})
	}
}

// A role that cannot be read is not evidence for the requested one: the
// backup stops, and the reason says that the role was not read.
func TestServer_Backup_RoleThatCannotBeReadDoesNotPassAsTheRequestedOne(t *testing.T) {
	cases := []struct {
		name      string
		requested Role
		member    heartbeatAnswer
		why       string
	}{
		{"heartbeat status fails on a master", RoleMaster, heartbeatFailed, "heartbeat status failed"},
		{"heartbeat status fails on a slave", RoleSlave, heartbeatFailed, "heartbeat status failed"},
		{"a member in a transition", RoleMaster, heartbeatInFlight, "HA-Node Info line"},
		{"empty output", RoleSlave, heartbeatAnswer{}, "HA-Node Info line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := runRoleBackup(t, tc.requested, false, false, []heartbeatAnswer{tc.member}, nil)
			b.refused(t, "backup refused", tc.requested, RoleUnknown)
			if !strings.Contains(b.final.FailureReason, tc.why) {
				t.Errorf("reason %q does not say why: %q", b.final.FailureReason, tc.why)
			}
			if b.cli.ranBackup() {
				t.Error("backupdb ran on a member whose role was not read")
			}
		})
	}
}

// A backup the Operator aimed at a role, on a member that has it, records
// that role. This covers a deliberate backup on a slave, the master fallback,
// and a replica, which the Operator never selects but the API accepts.
func TestServer_Backup_RecordsTheRoleItObserved(t *testing.T) {
	cases := []struct {
		name      string
		requested Role
		member    heartbeatAnswer
		want      Role
	}{
		{"a deliberate backup on a slave", RoleSlave, heartbeatSlave, RoleSlave},
		{"a backup on the master", RoleMaster, heartbeatMaster, RoleMaster},
		{"a backup on a replica", RoleReplica, heartbeatAnswer{out: replicaOut}, RoleReplica},
		// Nothing was claimed: the manifest says what the member said.
		{"no role in the request, on a slave", "", heartbeatSlave, RoleSlave},
		{"the role unknown in the request, on a master", RoleUnknown, heartbeatMaster, RoleMaster},
		{"no role in the request, and none read", "", heartbeatFailed, RoleUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := runRoleBackup(t, tc.requested, false, false, []heartbeatAnswer{tc.member}, nil)
			if got := b.manifestRole(t); got != tc.want {
				t.Errorf("manifest sourceRole = %q, want %q", got, tc.want)
			}
		})
	}
}

// A standalone member has no HA role, so the Operator asks for the role
// unknown (RoleUnknownSingleInstanceFallback) and heartbeat, which does not
// run there, gives none. That backup is valid and records unknown. Nothing is
// read again at its end, since no role was observed to compare with.
func TestServer_Backup_StandaloneMemberRecordsUnknown(t *testing.T) {
	b := runRoleBackup(t, RoleUnknown, false, true, []heartbeatAnswer{heartbeatFailed}, nil)
	if got := b.manifestRole(t); got != RoleUnknown {
		t.Errorf("manifest sourceRole = %q, want unknown", got)
	}
	if n := b.cli.statusReads(); n != 1 {
		t.Errorf("heartbeat status ran %d times, want once", n)
	}
}

// The first peer of a new cluster is copied from the member that holds the
// database, with no peer joined: its server is not active and the Operator
// did not ask for its role. That member is master to CUBRID, and the seed
// backup taken on it is the only way the cluster forms (habootstrap.go).
func TestServer_Backup_SeedOnTheHolderThatIsNotActiveYet(t *testing.T) {
	b := runRoleBackup(t, RoleMaster, true, false, []heartbeatAnswer{{out: aloneHolderOut}}, nil)
	if got := b.manifestRole(t); got != RoleMaster {
		t.Errorf("manifest sourceRole = %q, want master", got)
	}
}

// A seed backup asked for right after heartbeat started, or across a brief
// failure of the status command, is not refused for what passes in seconds:
// a member that returned after a kill went master, slave, master within
// 12.3 s (docs/poc/RESULTS.md, the entry on a master Pod deletion), and each
// refusal would use up one of the bootstrap's three attempts.
func TestServer_Backup_WaitsForTheRequestedRoleWithinTheBound(t *testing.T) {
	b := runRoleBackup(t, RoleMaster, true, false, []heartbeatAnswer{heartbeatFailed, heartbeatInFlight, heartbeatSlave, heartbeatMaster}, nil)
	if got := b.manifestRole(t); got != RoleMaster {
		t.Errorf("manifest sourceRole = %q, want master", got)
	}
}

// A seed backup never records a role other than master: on a slave it fails,
// as any request that differs from the member's role.
func TestServer_Backup_SeedOnASlaveIsRefused(t *testing.T) {
	b := runRoleBackup(t, RoleMaster, true, false, []heartbeatAnswer{heartbeatSlave}, nil)
	b.refused(t, "backup refused", RoleMaster, RoleSlave)
}

// The role is read again once backupdb has ended. A master demoted while it
// ran may hold writes the cluster no longer has (POC-52), and a seed is
// followed as the master's log, so neither reaches object storage unless the
// role held. The reason says both readings.
func TestServer_Backup_RoleChangedWhileBackupdbRanIsNotUploaded(t *testing.T) {
	cases := []struct {
		name      string
		requested Role
		seed      bool
		before    heartbeatAnswer
		after     heartbeatAnswer
		want      []Role
	}{
		{"the master became a slave", RoleMaster, false, heartbeatMaster, heartbeatSlave, []Role{RoleMaster, RoleSlave}},
		{"the seed source became a slave", RoleMaster, true, heartbeatMaster, heartbeatSlave, []Role{RoleMaster, RoleSlave}},
		{"the master's role can no longer be read", RoleMaster, false, heartbeatMaster, heartbeatFailed, []Role{RoleMaster, RoleUnknown}},
		{"a request without a role on a master that became a slave", RoleUnknown, false, heartbeatMaster, heartbeatSlave, []Role{RoleMaster, RoleSlave}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := runRoleBackup(t, tc.requested, tc.seed, false, []heartbeatAnswer{tc.before}, []heartbeatAnswer{tc.after})
			b.refused(t, "not uploaded", tc.want...)
			if !strings.Contains(b.final.FailureReason, "observed "+string(tc.want[0])+" at start, "+string(tc.want[1])+" after backupdb") {
				t.Errorf("reason %q does not give both readings", b.final.FailureReason)
			}
			if !b.cli.ranBackup() {
				t.Fatal("backupdb did not run, so the second reading was not reached")
			}
		})
	}
}

// A master's or a seed source's second reading is waited for like the first,
// so one failed status command after backupdb does not discard the backup.
func TestServer_Backup_SecondReadingWaitsForTheRole(t *testing.T) {
	for _, seed := range []bool{false, true} {
		b := runRoleBackup(t, RoleMaster, seed, false, []heartbeatAnswer{heartbeatMaster}, []heartbeatAnswer{heartbeatFailed, heartbeatMaster})
		if got := b.manifestRole(t); got != RoleMaster {
			t.Errorf("seed %t: manifest sourceRole = %q, want master", seed, got)
		}
	}
}

// A standby's backup is a consistent backup of a standby also when CUBRID
// promotes it, or its role cannot be read, after backupdb. Discarding it
// would lose the backup for good, since a CubridBackup is never started
// again, so it is uploaded with the role observed at the start.
func TestServer_Backup_StandbyBackupIsKeptAfterARoleChange(t *testing.T) {
	cases := []struct {
		name   string
		role   Role
		before heartbeatAnswer
		after  heartbeatAnswer
	}{
		{"the slave became master", RoleSlave, heartbeatSlave, heartbeatMaster},
		{"the slave's role can no longer be read", RoleSlave, heartbeatSlave, heartbeatFailed},
		{"the replica became a slave", RoleReplica, heartbeatAnswer{out: replicaOut}, heartbeatSlave},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := runRoleBackup(t, tc.role, false, false, []heartbeatAnswer{tc.before}, []heartbeatAnswer{tc.after})
			if got := b.manifestRole(t); got != tc.role {
				t.Errorf("manifest sourceRole = %q, want the role at the start, %q", got, tc.role)
			}
		})
	}
}

// A stop that comes while a backup waits for its role ends the backup at
// once, as Failed with a reason that says the member is stopping, and leaves
// no staging; the wait does not run out its bound.
func TestServer_Backup_StopDuringTheRoleWait(t *testing.T) {
	r := startRoleBackup(t, RoleMaster, false, false, []heartbeatAnswer{heartbeatSlave}, nil, time.Hour)
	deadline := time.Now().Add(5 * time.Second)
	for r.cli.statusReads() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the backup never read its role")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	r.srv.endOps()
	r.final = pollUntilSettled(t, r.srv, r.h, r.id)
	if waited := time.Since(started); waited > 5*time.Second {
		t.Errorf("the backup ended %s after the stop", waited)
	}
	r.refused(t, errStopping.Error(), RoleMaster, RoleSlave)
	if !strings.Contains(r.final.FailureReason, "the wait for that role ended") {
		t.Errorf("reason %q does not say the wait ended", r.final.FailureReason)
	}
	if r.cli.ranBackup() {
		t.Error("backupdb ran after the stop")
	}
}

// Without overrides the wait is backupRoleBound, read every backupRolePoll.
func TestBackupRoleTiming_Defaults(t *testing.T) {
	bound, poll := NewServer(&failingCLI{}, "tok").backupRoleTiming()
	if bound != backupRoleBound || poll != backupRolePoll {
		t.Errorf("timing = %s, %s; want %s, %s", bound, poll, backupRoleBound, backupRolePoll)
	}
	if backupRoleBound != 30*time.Second {
		t.Errorf("backupRoleBound = %s, want the 30 s ADR-0007 states", backupRoleBound)
	}
}

// A stop cancels the backup's context, and a stop waits for its operations:
// the wait for the role ends with it and does not run out its bound.
func TestBackupSourceRole_EndsWithItsContext(t *testing.T) {
	srv := NewServer(&failingCLI{}, "tok")
	srv.backupRoleBound, srv.backupRolePoll = time.Hour, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := srv.backupSourceRole(ctx, RoleMaster)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a canceled wait returned a role that was not read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait for the role did not end with its context")
	}
}
