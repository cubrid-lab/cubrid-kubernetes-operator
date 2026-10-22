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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// otherDatabase is a database a restore registers beside the bootstrap's.
const otherDatabase = "restored"

// createdbCLI stands for `cubrid createdb`, which registers the database in
// databases.txt itself: it reads the file, works, and writes back what it
// read with its own line added.
type createdbCLI struct {
	target  string
	started chan struct{}
	release chan struct{}
}

// Run treats every call as createdb, the only command createHADatabase runs.
func (c createdbCLI) Run(_ context.Context, _ string, args ...string) (string, error) {
	file := filepath.Join(c.target, databasesTxt)
	before, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	close(c.started)
	<-c.release
	database := args[len(args)-2]
	line := database + "\t\t" + filepath.Join(c.target, database) + "\thosts\n"
	return "", os.WriteFile(file, append(before, line...), 0o600)
}

// A restore of another database that registers itself while the HA
// bootstrap's createdb runs must not lose its line, nor make createdb's
// line disappear: the manager's registration waits for createdb (#298).
func TestCreateHADatabase_KeepsARegistrationMadeWhileCreatedbRuns(t *testing.T) {
	target := t.TempDir()
	conf := filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(conf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := createdbCLI{target: target, started: make(chan struct{}), release: make(chan struct{})}
	created := make(chan error, 1)
	go func() {
		created <- createHADatabase(context.Background(), cli, target,
			HAConfig{ConfPath: conf, VolumeSize: "64M", Locale: "en_US"}, dbName, "op-bootstrap")
	}()
	<-cli.started

	registered := make(chan error, 1)
	go func() {
		_, err := registerRestoreTarget(target, otherDatabase, "host", "op-restore")
		registered <- err
	}()
	// Give the registration the time it needs to finish if nothing holds it;
	// it must still be waiting for createdb.
	select {
	case err := <-registered:
		t.Errorf("the registration of %s ended while createdb ran (err=%v)", otherDatabase, err)
		registered <- err
	case <-time.After(200 * time.Millisecond):
	}
	close(cli.release)
	if err := <-created; err != nil {
		t.Fatalf("createHADatabase: %v", err)
	}
	if err := <-registered; err != nil {
		t.Fatalf("registerRestoreTarget: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(target, databasesTxt))
	if err != nil {
		t.Fatal(err)
	}
	for _, database := range []string{dbName, otherDatabase} {
		found := false
		for line := range strings.SplitSeq(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) > 0 && fields[0] == database {
				found = true
			}
		}
		if !found {
			t.Errorf("databases.txt lost the line of %s:\n%s", database, data)
		}
	}
}
