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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// measStagingFilesLeft is the measurement, in the S11 variants whose backup
// ends Failed during its upload, of how many staged entries that are not
// directories the target member's staging directory held after the backup
// ended.
const measStagingFilesLeft = "stagingFilesLeftAfterFailure"

// s11StagingAfterFailure is what the run saw of the target member's backup
// staging directory after a backup ended Failed during its upload, by name
// and size only: the run never reads a staged file's contents.
//
// The Instance Manager removes an operation's staging directory before it
// records the operation Failed, or keeps it, unused, when a process still
// holds a file below it after a bound (runBackup in
// internal/instancemanager/server.go, awaitStagingWriters). So a file that is
// still there after the failure is a leftover, unless the same reading
// recorded the process that held it. The staged files are a full copy of the
// database, and a repeated leftover could fill the member's volume.
type s11StagingAfterFailure struct {
	Dir string `json:"dir"`
	// CheckedAt is when the member was asked, in UTC; it follows the reading
	// of the backup as Failed.
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	// Present says whether Dir was there. Entries are "<type> <bytes>
	// <path>" (find -printf '%y %s %P'), Holders are "<pid> <command>
	// <path>" for each open file below Dir and Unreadable "<pid> <command>"
	// for each CUBRID process whose open files could not be listed.
	Present    *bool    `json:"present,omitempty"`
	Entries    []string `json:"entries"`
	Holders    []string `json:"holders"`
	Unreadable []string `json:"unreadableProcesses"`
	// Files counts the entries that are not directories; Left are the
	// entries of those that no recorded holder excuses.
	Files     int      `json:"files"`
	Left      []string `json:"leftBehind"`
	ReadError string   `json:"readError,omitempty"`
	// root is the directory as the kernel shows it in the open-file links.
	root string
}

// stagingEndMarker is the last line of failedStagingScript's answer: an
// answer without it was cut short and is not read.
const stagingEndMarker = "."

// failedStagingScript prints "present" or "absent" for the directory dir,
// and for a present one "R <resolved dir>", then "E <type> <bytes> <path>"
// for each entry below it, "H <pid> <command> <path>" for each open file at
// or below it (a deleted file included) and "U <pid> <command>" for each
// CUBRID process (command cub*) whose open files it could not list, as
// stagingHolders in internal/instancemanager/stagingwriter.go counts them.
// It ends with stagingEndMarker and prints no file's contents.
//
// The script fails closed: if the directory is there but readlink cannot
// resolve it or find cannot list every entry below it (a permission or an
// I/O error on a child, say), the script exits with a failure before it
// prints the marker, so the reading is an error and never an empty directory.
// The listing of the open files is the one read that may fail for a single
// process, which "U" records.
func failedStagingScript(dir string) string {
	return "w='" + dir + "'; if [ -d \"$w\" ]; then " +
		"r=$(readlink -f \"$w\") && [ -n \"$r\" ] || exit 1; echo present; echo \"R $r\"; " +
		"find \"$w\" -mindepth 1 -printf 'E %y %s %P\\n' || exit 1; " +
		"for p in /proc/[0-9]*; do n=\"\"; read -r n < \"$p/comm\" 2>/dev/null || continue; " +
		"if l=$(ls -l \"$p/fd\" 2>/dev/null); then " +
		"while IFS= read -r x; do case \"$x\" in *\" -> $r\"|*\" -> $r/\"*) echo \"H ${p#/proc/} $n ${x#* -> }\" ;; " +
		"esac; done <<< \"$l\"; " +
		"else [ -d \"$p\" ] && case \"$n\" in cub*) echo \"U ${p#/proc/} $n\" ;; esac; fi; done; " +
		"else echo absent; fi; echo " + stagingEndMarker
}

// parseStagingAfterFailure reads the answer of failedStagingScript into st.
// An answer that does not say whether the directory is there, or that was
// cut short, is a read error, never an empty directory.
func parseStagingAfterFailure(st *s11StagingAfterFailure, out string) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 || lines[len(lines)-1] != stagingEndMarker {
		st.ReadError = "the answer is cut short or does not end with " + strconv.Quote(stagingEndMarker)
		return
	}
	lines = lines[:len(lines)-1]
	switch lines[0] {
	case stagingAbsent:
		absent := false
		st.Present = &absent
		return
	case stagingPresent:
	default:
		st.ReadError = "the answer does not say whether the directory is there"
		return
	}
	present := true
	for _, line := range lines[1:] {
		kind, rest, _ := strings.Cut(line, " ")
		switch kind {
		case "R":
			st.root = rest
		case "E":
			st.Entries = append(st.Entries, rest)
		case "H":
			st.Holders = append(st.Holders, rest)
		case "U":
			st.Unreadable = append(st.Unreadable, rest)
		default:
			st.ReadError = "the answer has a line the run does not know"
			return
		}
	}
	if st.root == "" {
		st.ReadError = "the answer does not name the directory as the kernel shows it"
		return
	}
	st.Present = &present
}

// holds reports whether a recorded holder keeps the staged entry rel (the
// path below Dir) out of the leftovers. The Instance Manager keeps an
// operation's whole directory when a process holds a file anywhere below it
// (stagingHeld), so a holder at or below the entry's first directory, or at
// the entry itself, excuses it.
func (st *s11StagingAfterFailure) holds(rel string) bool {
	top, _, nested := strings.Cut(rel, "/")
	if !nested {
		top = rel
	}
	for _, h := range st.Holders {
		fields := strings.SplitN(h, " ", 3)
		if len(fields) < 3 {
			continue
		}
		target := strings.TrimSuffix(fields[2], " (deleted)")
		if target == st.root {
			return true
		}
		held, ok := strings.CutPrefix(target, st.root+"/")
		if ok && (held == top || strings.HasPrefix(held, top+"/")) {
			return true
		}
	}
	return false
}

// judge says what the reading shows. A directory that could not be read, or
// leftovers whose holders could not be told, are plain errors: the variant
// is blocked. A staged entry that is not a directory and that no recorded
// holder excuses is a backupVerdict: the variant fails.
func (st *s11StagingAfterFailure) judge() error {
	if st.ReadError != "" || st.Present == nil {
		return fmt.Errorf("the target member's backup staging directory %s could not be read after the backup "+
			"ended, so it is not known whether the failed upload left staged files behind: %s", st.Dir,
			cmp.Or(st.ReadError, "no answer"))
	}
	st.Files, st.Left = 0, []string{}
	for _, entry := range st.Entries {
		fields := strings.SplitN(entry, " ", 3)
		if len(fields) < 3 || fields[0] == "d" {
			continue
		}
		st.Files++
		if !st.holds(fields[2]) {
			st.Left = append(st.Left, entry)
		}
	}
	if len(st.Left) == 0 {
		return nil
	}
	if len(st.Unreadable) > 0 {
		return fmt.Errorf("the target member's backup staging directory %s holds %d staged files after the "+
			"backup ended, and the open files of %s could not be listed, so it is not known whether a process "+
			"still held them", st.Dir, len(st.Left), strings.Join(st.Unreadable, ", "))
	}
	return backupVerdict{fmt.Errorf("the target member's backup staging directory %s still held %d staged "+
		"files after the backup ended Failed (%s), and no process was recorded holding them: the failed upload "+
		"left a copy of the database on the member's volume", st.Dir, len(st.Left), strings.Join(st.Left, ", "))}
}

// residue says, for the record's residue, what the run saw.
func (st *s11StagingAfterFailure) residue() string {
	switch {
	case st.Present == nil:
		return "; the target member's backup staging directory " + st.Dir + " could not be read after the " +
			"failed upload"
	case !*st.Present:
		return "; the target member's backup staging directory " + st.Dir + " was not there after the failed upload"
	case st.Files == 0:
		return "; the target member's backup staging directory " + st.Dir + " was there after the failed upload " +
			"without a staged file"
	case len(st.Left) == 0:
		return fmt.Sprintf("; the target member's backup staging directory %s held %d staged files after the "+
			"failed upload that a recorded process still held; the Instance Manager keeps them, unused, until "+
			"none does", st.Dir, st.Files)
	}
	return fmt.Sprintf("; the target member's backup staging directory %s held %d staged files after the "+
		"failed upload; the run does not remove them", st.Dir, len(st.Left))
}

// passClause is what a pass reason says of the staging directory.
func (st *s11StagingAfterFailure) passClause() string {
	switch {
	case st.Present != nil && !*st.Present:
		return "the target member's backup staging directory was not there after the backup ended"
	case st.Files == 0:
		return "the target member's backup staging directory held no staged file after the backup ended"
	}
	return "every staged file in the target member's backup staging directory after the backup ended was held " +
		"by a recorded process"
}

// withStagingResidue replaces the residue's closing statement that the
// staging directory was not checked by what the run saw, when it saw it.
func withStagingResidue(residue string, st *s11StagingAfterFailure) string {
	if st == nil {
		return residue
	}
	return strings.TrimSuffix(residue, stagingNotChecked) + st.residue()
}

// checkStagingAfterFailure reads the staging directory of the backup uid on
// its target member once the backup ended Failed during its upload, and
// judges it (judge). phases are the backup's observed states: a backup that
// is not seen Failed, or whose target member is not known, leaves the
// directory unread, which blocks the variant, so that a variant never passes
// without this record. It records the reading in st and in result's
// measurements.
func (h *haRun) checkStagingAfterFailure(ctx context.Context, phases []s11Phase, uid string,
	st **s11StagingAfterFailure, result *evidence.Scenario) error {
	target := lastTarget(phases)
	if len(phases) == 0 || phases[len(phases)-1].Phase != string(databasev1alpha1.BackupPhaseFailed) {
		return errors.New("the backup was not seen ending Failed, so the target member's backup staging " +
			"directory was not read")
	}
	if target == "" {
		return errors.New("the backup's target member was not recorded, so its backup staging directory was " +
			"not read")
	}
	s := &s11StagingAfterFailure{Dir: imBackupStagingRoot + "/" + uid, Entries: []string{}, Holders: []string{},
		Unreadable: []string{}, Left: []string{}}
	*st = s
	at := time.Now().UTC()
	s.CheckedAt = &at
	if !plainName.MatchString(uid) {
		s.ReadError = "the backup's UID is not a plain name"
	} else if out, err := h.member(ctx, target, "bash", "-c", failedStagingScript(s.Dir)); err != nil {
		s.ReadError = err.Error()
	} else {
		parseStagingAfterFailure(s, out)
	}
	err := s.judge()
	if s.ReadError == "" && s.Present != nil {
		result.Measurements[measStagingFilesLeft] = s.Files
	}
	slices.Sort(s.Entries)
	return err
}
