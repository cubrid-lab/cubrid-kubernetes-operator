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

package installed

import (
	"errors"
	"strings"
	"testing"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testStagingDir = imBackupStagingRoot + "/some-backup-uid"
	// testStagedFile is an entry of a staged backup file below an operation's directory.
	testStagedFile = "E f 9 op-1/data"
)

// stagingAnswer builds the output of failedStagingScript from its lines.
func stagingAnswer(lines ...string) string {
	return strings.Join(append(lines, stagingEndMarker), "\n") + "\n"
}

// readTestStaging parses an answer and judges it, as checkStagingAfterFailure does.
func readTestStaging(out string) (*s11StagingAfterFailure, error) {
	st := &s11StagingAfterFailure{Dir: testStagingDir, Entries: []string{}, Holders: []string{},
		Unreadable: []string{}, Left: []string{}}
	parseStagingAfterFailure(st, out)
	return st, st.judge()
}

func TestStagingAfterFailureIsCleanWhenTheDirectoryIsGoneOrHoldsNoFile(t *testing.T) {
	for name, out := range map[string]string{
		"absent":               stagingAnswer(stagingAbsent),
		"present and empty":    stagingAnswer(stagingPresent, "R "+testStagingDir),
		"empty directory kept": stagingAnswer(stagingPresent, "R "+testStagingDir, "E d 4096 op-1"),
	} {
		st, err := readTestStaging(out)
		if err != nil || st.Present == nil || st.Files != 0 || len(st.Left) != 0 {
			t.Errorf("%s: staging = %+v, err = %v; want a clean reading", name, st, err)
		}
	}
}

func TestStagingAfterFailureFailsOnAFileLeftBehind(t *testing.T) {
	st, err := readTestStaging(stagingAnswer(stagingPresent, "R "+testStagingDir, "E d 4096 op-1",
		"E f 1048576 op-1/appdb_bk0v000", "E f 200 op-1/appdb_bkvinf"))
	if !errors.As(err, new(backupVerdict)) {
		t.Fatalf("err = %v, want a verdict that fails the variant", err)
	}
	if st.Files != 2 || len(st.Left) != 2 {
		t.Fatalf("staging = %+v, want both files left", st)
	}
	for _, want := range []string{"op-1/appdb_bk0v000", "no process was recorded holding them"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want %q", err, want)
		}
	}
}

func TestStagingAfterFailureIsBlockedWhenTheDirectoryCannotBeRead(t *testing.T) {
	for name, out := range map[string]string{
		"empty answer":            "",
		"cut short":               stagingAnswer(stagingPresent)[:len("present\n")],
		"no marker":               "absent\n",
		"no marker after entries": "present\nR " + testStagingDir + "\nE f 9 op-1/data\n",
		"unknown first":           stagingAnswer("maybe"),
		"unknown line":            stagingAnswer(stagingPresent, "R "+testStagingDir, "X what"),
		"no resolved root":        stagingAnswer(stagingPresent, "E f 1 a"),
	} {
		st, err := readTestStaging(out)
		if err == nil || errors.As(err, new(backupVerdict)) || st.Present != nil || st.ReadError == "" {
			t.Errorf("%s: staging = %+v, err = %v; want an error that blocks, and not an empty directory", name, st, err)
		}
	}
	// An exec that failed is recorded the same way.
	st := &s11StagingAfterFailure{Dir: testStagingDir, ReadError: "exec: connection refused"}
	if err := st.judge(); err == nil || errors.As(err, new(backupVerdict)) {
		t.Errorf("err = %v, want an error that blocks", err)
	}
}

func TestStagingAfterFailureExcusesOnlyWhatARecordedProcessHeld(t *testing.T) {
	root := "R " + testStagingDir
	tests := []struct {
		name     string
		lines    []string
		wantLeft int
		verdict  bool
		blocked  bool
	}{
		{name: "held file", lines: []string{testStagedFile, "H 42 cub_server " + testStagingDir + "/op-1/data"}},
		{name: "deleted but held", lines: []string{testStagedFile,
			"H 42 cub_server " + testStagingDir + "/op-1/data (deleted)"}},
		{name: "another file of the directory is held", lines: []string{testStagedFile, "E f 9 op-1/other",
			"H 42 cub_server " + testStagingDir + "/op-1/data"}},
		{name: "holder of another operation's directory", lines: []string{testStagedFile, "E f 9 op-2/data",
			"H 42 cub_server " + testStagingDir + "/op-2/data"}, wantLeft: 1, verdict: true},
		{name: "similar name is not the directory", lines: []string{testStagedFile,
			"H 42 cub_server " + testStagingDir + "/op-10/data"}, wantLeft: 1, verdict: true},
		{name: "no holder", lines: []string{testStagedFile}, wantLeft: 1, verdict: true},
		{name: "unreadable holders", lines: []string{testStagedFile, "U 7 cub_server"}, wantLeft: 1, blocked: true},
		{name: "unreadable holders do not matter without a file", lines: []string{"E d 4096 op-1", "U 7 cub_server"}},
		{name: "file held directly", lines: []string{"E f 9 data", "H 42 cub_server " + testStagingDir + "/data"}},
	}
	for _, tt := range tests {
		st, err := readTestStaging(stagingAnswer(append([]string{stagingPresent, root}, tt.lines...)...))
		isVerdict := errors.As(err, new(backupVerdict))
		if len(st.Left) != tt.wantLeft || isVerdict != tt.verdict || (err != nil) != (tt.verdict || tt.blocked) {
			t.Errorf("%s: left = %v, err = %v; want %d left, verdict %t, blocked %t", tt.name, st.Left, err,
				tt.wantLeft, tt.verdict, tt.blocked)
		}
	}
}

func TestFailedStagingScriptListsNamesAndSizesNeverContents(t *testing.T) {
	script := failedStagingScript(testStagingDir)
	if !strings.HasPrefix(script, "w='"+testStagingDir+"'") {
		t.Fatalf("script = %q, want it to name the backup's staging directory", script)
	}
	for _, reader := range []string{"cat ", "head ", "tail ", "strings ", "xxd ", "od ", "base64", "cp ", "tar ",
		"< \"$w", "<\"$w", "/*\""} {
		if strings.Contains(script, reader) {
			t.Errorf("script contains %q, which could read a staged file", reader)
		}
	}
	if !strings.Contains(script, "-printf 'E %y %s %P") || !strings.HasSuffix(script, "echo "+stagingEndMarker) {
		t.Errorf("script = %q, want names and sizes only, ended by the marker", script)
	}
}

// stagingVariant plays one S11 failure variant with the staging reading
// answered by answer, and returns its result, its record's staging reading
// and residue, and the reads of the staging directory.
type stagingVariant struct {
	name string
	run  func(t *testing.T, answer func(pod string) (string, error)) (evidence.Scenario, *s11StagingAfterFailure,
		string, []string)
}

func TestS11FailureVariantsRecordTheStagingDirectoryAfterTheBackupEnded(t *testing.T) {
	for _, v := range stagingVariants() {
		t.Run(v.name, func(t *testing.T) {
			v.requireGone(t)
			v.requireLeftFails(t)
			v.requireUnreadableBlocks(t)
			v.requireHeldPasses(t)
		})
	}
}

// requireGone: the Instance Manager removed the directory, the variant
// passes and records that, with the read's time in UTC and where it read.
func (v stagingVariant) requireGone(t *testing.T) {
	t.Helper()
	s, st, residue, reads := v.run(t, nil)
	if s.Result != evidence.Pass || st == nil || st.Present == nil || *st.Present || st.CheckedAt == nil ||
		st.CheckedAt.Location().String() != "UTC" || len(reads) != 1 ||
		!strings.HasPrefix(st.Dir, imBackupStagingRoot+"/") || st.Dir == imBackupStagingRoot+"/" ||
		!strings.Contains(reads[0], " w='"+st.Dir+"';") ||
		s.Measurements[measStagingFilesLeft] != float64(0) || strings.Contains(residue, stagingNotChecked) ||
		!strings.Contains(residue, "was not there after the failed upload") {
		t.Fatalf("absent: scenario = %+v, staging = %+v, residue = %q, reads = %v; want a pass that "+
			"recorded the directory as gone", s, st, residue, reads)
	}
}

// requireLeftFails: a staged file with no recorded holder fails the variant.
func (v stagingVariant) requireLeftFails(t *testing.T) {
	t.Helper()
	left := stagingAnswer(stagingPresent, "R "+imBackupStagingRoot+"/x", "E d 4096 op-1", "E f 4096 op-1/appdb_bk0v000")
	s, st, residue, _ := v.run(t, func(string) (string, error) { return left, nil })
	if s.Result != evidence.Fail || st == nil || st.Files != 1 || len(st.Left) != 1 ||
		!strings.Contains(s.Reason, "op-1/appdb_bk0v000") || s.Measurements[measStagingFilesLeft] != float64(1) ||
		!strings.Contains(residue, "held 1 staged files") {
		t.Fatalf("left: scenario = %+v, staging = %+v, residue = %q; want a failure naming the file",
			s, st, residue)
	}
}

// requireUnreadableBlocks: a directory that could not be read blocks the
// variant; it is neither passed nor failed.
func (v stagingVariant) requireUnreadableBlocks(t *testing.T) {
	t.Helper()
	s, st, residue, _ := v.run(t, func(string) (string, error) { return "", errors.New("exec refused") })
	if s.Result != evidence.Blocked || st == nil || st.Present != nil || st.ReadError == "" ||
		s.Measurements[measStagingFilesLeft] != evidence.Unknown || !strings.Contains(s.Reason, "exec refused") ||
		!strings.Contains(residue, "could not be read") {
		t.Fatalf("unreadable: scenario = %+v, staging = %+v, residue = %q; want it blocked", s, st, residue)
	}
}

// requireHeldPasses: a recorded process that holds the file excuses it, and
// the record says so.
func (v stagingVariant) requireHeldPasses(t *testing.T) {
	t.Helper()
	held := stagingAnswer(stagingPresent, "R "+imBackupStagingRoot+"/x", "E f 4096 op-1/appdb_bk0v000",
		"H 42 cub_server "+imBackupStagingRoot+"/x/op-1/appdb_bk0v000")
	s, st, residue, _ := v.run(t, func(string) (string, error) { return held, nil })
	if s.Result != evidence.Pass || st == nil || len(st.Holders) != 1 || len(st.Left) != 0 ||
		!strings.Contains(residue, "recorded process still held") {
		t.Fatalf("held: scenario = %+v, staging = %+v, residue = %q; want a pass with the holder recorded",
			s, st, residue)
	}
}

// stagingVariants are the four S11 variants whose backup ends Failed during
// its upload.
func stagingVariants() []stagingVariant {
	read := func(f *haFixture, answer func(string) (string, error)) {
		f.stagingAfterFailure = answer
	}
	return []stagingVariant{
		{name: "missing-destination", run: func(t *testing.T, answer func(string) (string, error)) (evidence.Scenario,
			*s11StagingAfterFailure, string, []string) {
			cfg := testS11MissingConfig(t)
			f := newS11MissingFixture(t, cfg)
			read(f.haFixture, answer)
			s, _ := missingOf(t, f)
			rec := readMissingRecord(t, cfg.EvidenceDir)
			return s, rec.StagingAfterFailure, rec.Residue, f.stagingReads
		}},
		{name: "denied-credentials", run: func(t *testing.T, answer func(string) (string, error)) (evidence.Scenario,
			*s11StagingAfterFailure, string, []string) {
			cfg := testS11DeniedConfig(t)
			f := newS11DeniedFixture(t, cfg)
			read(f.haFixture, answer)
			s, _ := deniedOf(t, f)
			rec := readDeniedRecord(t, cfg.EvidenceDir)
			return s, rec.StagingAfterFailure, rec.Residue, f.stagingReads
		}},
		{name: "signature-mismatch", run: func(t *testing.T, answer func(string) (string, error)) (evidence.Scenario,
			*s11StagingAfterFailure, string, []string) {
			cfg := testS11SignatureConfig(t)
			f := newS11SignatureFixture(t, cfg)
			read(f.haFixture, answer)
			s, _ := signatureOf(t, f)
			rec := readSignatureRecord(t, cfg.EvidenceDir)
			return s, rec.StagingAfterFailure, rec.Residue, f.stagingReads
		}},
		{name: "interrupted-upload", run: func(t *testing.T, answer func(string) (string, error)) (evidence.Scenario,
			*s11StagingAfterFailure, string, []string) {
			cfg := testS11InterruptedConfig(t)
			f := newS11InterruptedFixture(t, cfg)
			read(f.haFixture, answer)
			s, _ := interruptedOf(t, f)
			rec := readInterruptedRecord(t, cfg.EvidenceDir)
			return s, rec.StagingAfterFailure, rec.Residue, f.stagingReads
		}},
	}
}

func TestCheckStagingAfterFailureReadsNothingUnlessTheBackupWasSeenFailed(t *testing.T) {
	h := &haRun{}
	target := []s11Phase{{Phase: string(databasev1alpha1.BackupPhaseUploading), TargetInstance: testHAPrimary}}
	for name, phases := range map[string][]s11Phase{
		"no phase":      nil,
		"still running": target,
		"completed":     {{Phase: string(databasev1alpha1.BackupPhaseCompleted), TargetInstance: testHAPrimary}},
		"no target":     {{Phase: string(databasev1alpha1.BackupPhaseFailed)}},
	} {
		var st *s11StagingAfterFailure
		result := evidence.Scenario{Measurements: map[string]any{measStagingFilesLeft: evidence.Unknown}}
		err := h.checkStagingAfterFailure(t.Context(), phases, "some-backup-uid", &st, &result)
		if err == nil || errors.As(err, new(backupVerdict)) || st != nil ||
			result.Measurements[measStagingFilesLeft] != evidence.Unknown {
			t.Errorf("%s: err = %v, staging = %+v; want an error that blocks and nothing read", name, err, st)
		}
	}
}
