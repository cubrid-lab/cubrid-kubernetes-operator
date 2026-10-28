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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Engine versions of the tests below: the series the cluster declares,
// another patch and another series of the pinned engine, and a version
// without a series.
const (
	declaredSeries    = "11.4"
	anotherPatch      = "11.4.7.2000"
	otherSeriesEngine = "11.2.0.0480"
	noSeries          = "latest"
	backupContentSHA  = "0000"
)

// relCLI answers cubrid_rel with out and err and passes every other command
// to CLI: the engine version is read from it, not taken as pinned (runTests).
type relCLI struct {
	CLI
	out string
	err error
}

func (*relCLI) answersCubridRel() {}

func (c *relCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "cubrid_rel" {
		return c.out, c.err
	}
	return c.CLI.Run(ctx, name, args...)
}

func TestEngineSeries(t *testing.T) {
	for in, want := range map[string]string{
		declaredSeries: declaredSeries, reportedEngineVersion: declaredSeries, " 11.4.6 ": declaredSeries,
		"11": "", "": "", "11.x.6": "", "11.": "", ".4": "", "v11.4": "", "11.x": "",
	} {
		if got := EngineSeries(in); got != want {
			t.Errorf("EngineSeries(%q) = %q, want %q", in, got, want)
		}
	}
}

// A manifest's engine version is compared by series (ADR-0011): another patch
// of the series matches, another series or a version without one does not.
func TestManifestVerify_ComparesTheEngineBySeries(t *testing.T) {
	for _, tc := range []struct {
		manifest, expected string
		ok                 bool
	}{
		{reportedEngineVersion, declaredSeries, true},
		{declaredSeries, reportedEngineVersion, true},
		{"11.4.6", anotherPatch, true},
		{otherSeriesEngine, reportedEngineVersion, false},
		{"", reportedEngineVersion, false},
		{noSeries, reportedEngineVersion, false},
		{noSeries, noSeries, false},
	} {
		m := BackupManifest{ManifestVersion: ManifestVersion, Database: dbName, CubridVersion: tc.manifest,
			Objects: []ManifestObject{{Key: "backup/" + stagedFileName, SizeBytes: 3, SHA256: backupContentSHA}}}
		exp := goodManifestExpectation()
		exp.CubridVersion = tc.expected
		if err := m.Verify(exp); (err == nil) != tc.ok {
			t.Errorf("manifest %q, expected %q: err = %v, want ok=%v", tc.manifest, tc.expected, err, tc.ok)
		}
	}
}

// runEngineBackup runs an asynchronous backup that declares declared, on a
// member whose cubrid_rel answers rel and relErr, and returns the final
// operation, the backupdb stand-in and the object store.
func runEngineBackup(t *testing.T, declared, rel string, relErr error) (Operation, *writingCLI, *fakeObjectStore) {
	t.Helper()
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	objects := newFakeStore()
	root := t.TempDir()
	backupdb := newWritingCLI()
	h := NewServer(&relCLI{CLI: backupdb, out: rel, err: relErr}, "tok").
		WithOperationStore(store).WithObjectStore(objects).WithBackupStagingRoot(root).Handler()
	upload := *testUpload
	upload.CubridVersion = declared
	body, _ := json.Marshal(BackupRequest{Database: dbName, Destination: filepath.Join(root, "uid-1"), Upload: &upload})
	rr := postBackup(t, h, "key-1", string(body))
	var op Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &op)
	return pollUntilTerminal(t, h, op.ID), backupdb, objects
}

// The manifest records the engine the member runs, read from cubrid_rel,
// not the version the request declares (#276, ADR-0011).
func TestServer_Backup_RecordsTheObservedEngineVersion(t *testing.T) {
	final, _, objects := runEngineBackup(t, declaredSeries, cubridRelOut, nil)
	if final.State != OpCompleted || final.Artifact == nil {
		t.Fatalf("final = %s (%q), artifact %+v; want Completed", final.State, final.FailureReason, final.Artifact)
	}
	if final.Artifact.CubridVersion != reportedEngineVersion {
		t.Errorf("operation artifact cubridVersion = %q, want %q", final.Artifact.CubridVersion, reportedEngineVersion)
	}
	prefix := uploadedPrefix(t, final.Artifact.ManifestURI, testUpload.Bucket)
	m, err := ParseManifest(objects.objects[testUpload.Bucket+"/"+prefix+"/"+ManifestObjectName])
	if err != nil {
		t.Fatal(err)
	}
	if m.CubridVersion != reportedEngineVersion {
		t.Errorf("manifest cubridVersion = %q, want the observed %q", m.CubridVersion, reportedEngineVersion)
	}
}

// Without a known engine of the declared series no backupdb runs: the
// artifact would be labeled with an engine that did not produce it.
func TestServer_Backup_RefusesWithoutAKnownEngineOfTheDeclaredSeries(t *testing.T) {
	for name, tc := range map[string]struct {
		declared, rel string
		relErr        error
	}{
		"cubrid_rel fails":        {declaredSeries, "", errors.New("exit status 127")},
		"cubrid_rel has no build": {declaredSeries, "CUBRID 11.4.6", nil},
		"another series":          {"11.2", cubridRelOut, nil},
		"no series declared":      {noSeries, cubridRelOut, nil},
	} {
		t.Run(name, func(t *testing.T) {
			final, backupdb, objects := runEngineBackup(t, tc.declared, tc.rel, tc.relErr)
			if final.State != OpFailed || !strings.Contains(final.FailureReason, "backup refused") {
				t.Fatalf("final = %s (%q), want Failed with backup refused", final.State, final.FailureReason)
			}
			if len(backupdb.dirs) != 0 {
				t.Errorf("backupdb ran: %v", backupdb.dirs)
			}
			if len(objects.objects) != 0 {
				t.Errorf("objects were uploaded: %d", len(objects.objects))
			}
		})
	}
}

// withManifestVersion rewrites the engine version of the uploaded artifact
// at bucket/prefix.
func withManifestVersion(t *testing.T, objects *fakeObjectStore, bucket, prefix, version string) {
	t.Helper()
	key := bucket + "/" + prefix + "/" + ManifestObjectName
	m, err := ParseManifest(objects.objects[key])
	if err != nil {
		t.Fatal(err)
	}
	m.CubridVersion = version
	if objects.objects[key], err = m.Marshal(); err != nil {
		t.Fatal(err)
	}
}

// A restore is refused, before anything is staged, registered or run, unless
// this member's engine, the declared version and the artifact's engine are
// of one series (ADR-0011).
func TestRestore_RefusesAnEngineOfAnotherOrUnknownSeries(t *testing.T) {
	for name, tc := range map[string]struct {
		engine, declared, manifest string
	}{
		"member engine unknown":           {"", declaredSeries, testCubridVersion},
		"member engine without a series":  {"11.x.6", "", testCubridVersion},
		"member runs another series":      {otherSeriesEngine, declaredSeries, otherSeriesEngine},
		"artifact of another series":      {reportedEngineVersion, declaredSeries, otherSeriesEngine},
		"artifact without a version":      {reportedEngineVersion, declaredSeries, ""},
		"artifact version without series": {reportedEngineVersion, "", noSeries},
	} {
		t.Run(name, func(t *testing.T) {
			objects, bucket, prefix := stageUploadedArtifact(t)
			withManifestVersion(t, objects, bucket, prefix, tc.manifest)
			req := baseRestoreRequest(t, bucket, prefix)
			req.ExpectedCubridVersion = tc.declared
			roots := rootsFor(req)
			roots.EngineVersion = tc.engine
			cli := &restoreCLI{}
			if _, err := Restore(context.Background(), cli, objects, roots, req); err == nil {
				t.Fatal("Restore succeeded, want a refusal")
			}
			if len(cli.calls) != 0 {
				t.Errorf("commands ran: %v", cli.calls)
			}
			if registered, _ := registeredInDatabasesTxt(req.TargetDir, dbName); registered {
				t.Error("the database was registered")
			}
			if _, err := os.Stat(req.StagingDir); !os.IsNotExist(err) {
				t.Errorf("staging directory is there (err=%v)", err)
			}
		})
	}
}

// Another patch of the series is restored, and so is a manifest written by
// an earlier manager, which carries the declared series; a request without a
// declared version is checked against the member's engine alone.
func TestRestore_AcceptsAnEngineOfTheSameSeries(t *testing.T) {
	for name, tc := range map[string]struct {
		engine, declared, manifest string
	}{
		"same full version":      {reportedEngineVersion, declaredSeries, reportedEngineVersion},
		"another patch":          {anotherPatch, declaredSeries, reportedEngineVersion},
		"manifest of the series": {reportedEngineVersion, declaredSeries, declaredSeries},
		"request declares none":  {reportedEngineVersion, "", reportedEngineVersion},
	} {
		t.Run(name, func(t *testing.T) {
			objects, bucket, prefix := stageUploadedArtifact(t)
			withManifestVersion(t, objects, bucket, prefix, tc.manifest)
			req := baseRestoreRequest(t, bucket, prefix)
			req.ExpectedCubridVersion = tc.declared
			roots := rootsFor(req)
			roots.EngineVersion = tc.engine
			cli := &restoreCLI{}
			if _, err := Restore(context.Background(), cli, objects, roots, req); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if len(cli.calls) != 1 || !strings.HasPrefix(cli.calls[0], "cubrid restoredb ") {
				t.Errorf("calls = %v, want one restoredb", cli.calls)
			}
		})
	}
}

// The manager restores with the engine version it reads itself: one that
// cubrid_rel does not give fails the operation without a command.
func TestServer_Restore_ReadsItsOwnEngineVersion(t *testing.T) {
	steps := &stepCLI{}
	final := runRestoreOperation(t, &relCLI{CLI: steps, err: errors.New("exit status 127")}, restoreMember{standalone: true})
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "engine version of this member is not known") {
		t.Fatalf("final = %s (%q), want Failed for the unknown engine", final.State, final.FailureReason)
	}
	if calls := steps.recorded(); len(calls) != 0 {
		t.Errorf("calls = %q, want none", calls)
	}
}

// otherSeriesRelOut is cubrid_rel output of an engine of another series.
const otherSeriesRelOut = "\nCUBRID 11.2 (11.2.0.0480-1a2b3c4) (64bit release build for Linux)\n\n"

// A restore operation reads the engine from cubrid_rel: one of the declared
// series restores, one of another series fails before any command.
func TestServer_Restore_ChecksTheEngineCubridRelGives(t *testing.T) {
	steps := &stepCLI{}
	final := runRestoreOperation(t, &relCLI{CLI: steps, out: cubridRelOut}, restoreMember{standalone: true})
	if final.State != OpCompleted {
		t.Fatalf("final = %s (%q), want Completed", final.State, final.FailureReason)
	}
	steps = &stepCLI{}
	final = runRestoreOperation(t, &relCLI{CLI: steps, out: otherSeriesRelOut}, restoreMember{standalone: true})
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "11.2.0.0480") {
		t.Fatalf("final = %s (%q), want Failed naming the engine", final.State, final.FailureReason)
	}
	if calls := steps.recorded(); len(calls) != 0 {
		t.Errorf("calls = %q, want none", calls)
	}
}

// Data an earlier attempt restored is not taken over and started on an
// engine of another series: it is kept, and nothing runs (ADR-0011).
func TestServer_Restore_DoesNotTakeOverDataOfAnotherSeries(t *testing.T) {
	f := newResumeFixture(t)
	f.req.ExpectedCubridVersion = "" // so that only the member's engine is compared
	f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepServerStart: true}}, databases: f.databases})
	failed := f.run("restore-1")
	if failed.State != OpFailed {
		t.Fatalf("first attempt = %s, want Failed", failed.State)
	}

	second := &volumeCLI{databases: f.databases}
	f.start(&relCLI{CLI: second, out: otherSeriesRelOut})
	final := f.run("restore-2")
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "an earlier attempt restored") {
		t.Fatalf("retry = %s (%q), want Failed as refused", final.State, final.FailureReason)
	}
	if calls := second.recorded(); len(calls) != 0 {
		t.Errorf("retry calls = %q, want none", calls)
	}
	f.wantRestoredDataKept()
}
