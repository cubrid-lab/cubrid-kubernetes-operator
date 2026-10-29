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
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

const (
	s11StoreLossLane                = "lab-installed-ha-s11-operation-store-loss"
	s11StoreLossVariant             = "operation-store-loss"
	s11StoreLossRecordFile          = "S11/operation-store-loss/record.json"
	s11StoreLossTimelineFile        = "S11/operation-store-loss/timeline.jsonl"
	s11StoreLossSourceDataCheckFile = "S11/operation-store-loss/source-data-check.json"
	s11StoreLossBackupName          = "s11-operation-store-loss"
	// s11StoreLossClusterName is the standalone cluster whose member loses
	// its backup's operation record and its Instance Manager during the
	// upload; s11StoreLossMember is its one member's Pod.
	s11StoreLossClusterName = "s11-storeloss"
	s11StoreLossMember      = s11StoreLossClusterName + "-0"
	// storeLossMissingWait is backupOperationMissingWait (internal/
	// controller/cubridbackup_controller.go): how long a backup waits, from
	// status.operationMissingSince, for a recorded operation its target holds
	// no record of before it ends Failed with RecordedOperationLost.
	storeLossMissingWait = 30 * time.Minute
	// storeLossMinBackupLimit is the least -backup-limit the variant takes:
	// the bound above, plus the upload up to the fault, the restart and
	// the Operator's polls (backupPollAfter, 10s).
	storeLossMinBackupLimit = 35 * time.Minute
	// storeLossRefused starts the answer of storeLossScript when it killed
	// nothing; storeLossRemovedLine starts each line that names a file it
	// removed.
	storeLossRefused     = "nothing killed:"
	storeLossRemovedLine = "removed "
	// imBackupStagingRoot is where a member's Instance Manager stages a
	// backup, in a directory named after the CubridBackup's UID, with one
	// directory per operation below it (backupStagingRoot in internal/
	// controller/cubridbackup_controller.go, IM_BACKUP_STAGING_ROOT; the
	// layout in internal/instancemanager/staging.go).
	imBackupStagingRoot = "/var/lib/cubrid/backup-staging"
	// stagingPresent and stagingAbsent are the first line of stagingScript's
	// answer.
	stagingPresent = "present"
	stagingAbsent  = "absent"
	// measRecordAbsent and the other keys are measurements of the variant;
	// the last three it shares with S11 instance-manager-restart.
	measRecordAbsent          = "recordAbsentAfterRestart"
	measMissingAfterKill      = "missingAfterKill"
	measLostAfterMissing      = "failedAfterMissingSince"
	measStagingAtRestart      = "stagingPresentAfterRestart"
	measStagingAtEnd          = "stagingPresentAtEnd"
	measContainerRestarts     = "containerRestarts"
	measMemberBackWithin      = "memberBackWithin"
	measOperationRefUnchanged = "operationRefUnchanged"
	// readyLimitName is the result's name of a variant's ready limit, and
	// nothingCreated the cleanup of a run that created nothing.
	readyLimitName = "readyLimit"
	nothingCreated = "nothing to delete: the run created no CubridCluster or CubridBackup"
)

// s11StoreLoss is the removal of the backup's operation record from the
// member's operation store, in the same command as the kill, and what the
// store held once the Instance Manager ran again.
type s11StoreLoss struct {
	// OperationID is the backup's operation; Removed are the files the
	// command answered that it removed, which the container's end may cut
	// short.
	OperationID string   `json:"operationID"`
	Removed     []string `json:"removed"`
	// CheckedAt is when the store was listed after the restart; StoreAfter
	// are the names in it then, and RecordAbsent whether <id>.json was not
	// among them. ReadError says why it could not be listed.
	CheckedAt    *time.Time `json:"checkedAt,omitempty"`
	StoreAfter   []string   `json:"storeAfterRestart"`
	RecordAbsent *bool      `json:"recordAbsent,omitempty"`
	ReadError    string     `json:"readError,omitempty"`
}

// s11Staging is what the run saw of the member's backup staging directory
// of the backup: whether it is there and, by name and size only, what is
// in it ("<type> <bytes> <path>", find -printf '%y %s %P').
type s11Staging struct {
	Dir       string     `json:"dir"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	Present   *bool      `json:"present,omitempty"`
	Entries   []string   `json:"entries"`
	ReadError string     `json:"readError,omitempty"`
}

// s11StoreLossRecord is the evidence of S11 operation-store-loss besides the
// timeline and the source's data check: that of S11
// instance-manager-restart, whose steps it shares, and the record's loss.
type s11StoreLossRecord struct {
	s11IMRestartRecord
	// MinBackupLimit is storeLossMinBackupLimit and MissingWait
	// storeLossMissingWait, as the run took them.
	MinBackupLimit string        `json:"minBackupLimit"`
	MissingWait    string        `json:"missingWait"`
	StoreLoss      *s11StoreLoss `json:"storeLoss"`
	// FirstMissingAt is the first reading after the kill whose Ready reason
	// was RecordedOperationMissing, and MissingAfterKill the time from the
	// kill to it. OperationMissingSince is the backup's
	// status.operationMissingSince as it ended, and FailedAfterMissingSince
	// the time from it to status.completedAt, both by the Operator's clock.
	FirstMissingAt          *time.Time `json:"firstMissingAt,omitempty"`
	MissingAfterKill        string     `json:"missingAfterKill,omitempty"`
	OperationMissingSince   *time.Time `json:"operationMissingSince,omitempty"`
	FailedAfterMissingSince string     `json:"failedAfterMissingSince,omitempty"`
	// StagingAfterRestart and StagingAtEnd are the backup's staging
	// directory on the member once the record's absence was confirmed and
	// at the end. They are recorded, not judged.
	StagingAfterRestart *s11Staging `json:"stagingAfterRestart,omitempty"`
	StagingAtEnd        *s11Staging `json:"stagingAtEnd,omitempty"`
}

// validateS11OperationStoreLoss checks the inputs of S11
// operation-store-loss, when it is selected; BackupBucket is checked by
// validateS11.
func (c HAConfig) validateS11OperationStoreLoss() error {
	if !c.BackupOperationStoreLoss {
		return nil
	}
	for _, other := range []struct {
		selected bool
		flag     string
	}{{c.Restore, flagRestore}, {c.BackupMissingDestination, flagMissingDestination},
		{c.BackupDeniedCredentials, flagDeniedCredentials},
		{c.BackupSignatureMismatch, flagSignatureMismatch},
		{c.BackupInterruptedUpload, flagInterruptedUpload},
		{c.BackupOperatorRestart, flagOperatorRestart},
		{c.BackupInstanceManagerRestart, flagInstanceManagerRestart},
		{c.BackupOverlapping, flagOverlapping}} {
		if other.selected {
			return fmt.Errorf("%s and %s select different lanes; a run takes one of them", flagOperationStoreLoss,
				other.flag)
		}
	}
	if c.BackupOperationStoreLossReadyLimit <= 0 {
		return errors.New(flagOperationStoreLoss + " needs a positive -backup-operation-store-loss-ready-limit for " +
			"its cluster")
	}
	if c.BackupLimit < storeLossMinBackupLimit {
		return fmt.Errorf("%s needs -backup-limit of at least %s, not %s: its backup ends Failed only after its "+
			"record has been missing for %s, and backup_limit counts from the request", flagOperationStoreLoss,
			storeLossMinBackupLimit, c.BackupLimit, storeLossMissingWait)
	}
	return nil
}

// planS11OperationStoreLoss returns the lane and the requirements of a run
// that selects S11 operation-store-loss after S11 complete, and names it and
// the run namespace's Pod Security in the summary's notes; otherwise lane
// and requirements as they are.
func planS11OperationStoreLoss(cfg HAConfig, lane string, requirements []evidence.Requirement,
	summary *evidence.Summary) (string, []evidence.Requirement) {
	if !cfg.BackupOperationStoreLoss {
		return lane, requirements
	}
	summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete and S11 " +
		"operation-store-loss: a full backup to object storage, downloaded and checked by the run, and a " +
		"backup of a standalone cluster whose upload is slowed by a rate limit in its member's Pod, whose " +
		"operation record is removed from the member's operation store and whose Instance Manager is killed " +
		"during it, which must end Failed with RecordedOperationLost after RecordedOperationMissing; no other " +
		"S00-S15 qualification"
	summary.Notes = append(summary.Notes, "The run namespace enforces Pod Security privileged, with warn and "+
		"audit at restricted, so that the rate limit's ephemeral container is admitted")
	return s11StoreLossLane, append(requirements, evidence.Requirement{ID: s11ID, Variant: s11StoreLossVariant})
}

// storeLossScript removes the record of operation id from the Instance
// Manager's operation store and then kills the Instance Manager, in one
// command. It finds the manager as imKillScript does (imFindScript) and
// stops it with SIGSTOP first, so that it writes nothing while the record
// goes: a record write already under way is a temporary file renamed over
// <id>.json (writeLocked in internal/instancemanager/opstore.go). The
// manager is taken as stopped only once every one of its threads
// (/proc/<pid>/task/*/stat) is in state T: a thread inside a write or
// rename system call is not stopped until that call returns, so a rename
// under way has landed before anything is removed. It removes <id>.json and
// any of its temporary files, <id>.*.tmp; the store holds no index or other
// file of an operation. It then kills the stopped manager with SIGKILL, as
// imKillScript does. When the manager does not stop within five seconds,
// the record is not there before, or is still there after, it lets the
// manager go on and answers storeLossRefused, and nothing is killed. id is
// an operation ID, checked by the caller (operationIDPattern).
func storeLossScript(id string) string {
	return imFindScript + `p="$1"; r='` + imOperationsDir + "/" + id + `'; ` +
		`kill -STOP "$p" || { echo "` + storeLossRefused + ` SIGSTOP to $p failed"; exit 4; }; ` +
		`t=""; for ((i = 0; i < 50; i++)); do [ -d "/proc/$p/task" ] || break; a=1; ` +
		`for f in /proc/"$p"/task/*/stat; do s=$(cat "$f" 2>/dev/null) || continue; ` +
		`read -r -a st <<< "${s##*) }"; [ "${st[0]}" = T ] || { a=""; break; }; done; ` +
		`if [ -n "$a" ]; then t=1; break; fi; sleep 0.1; done; ` +
		`if [ -z "$t" ]; then kill -CONT "$p"; echo "` + storeLossRefused + ` instance-manager $p did not stop"; ` +
		`exit 4; fi; ` +
		`if [ ! -f "$r.json" ]; then kill -CONT "$p"; echo "` + storeLossRefused + ` no record ${r##*/}.json"; ` +
		`exit 4; fi; ` +
		`for f in "$r.json" "$r".*.tmp; do [ -e "$f" ] || continue; rm -f -- "$f" && ` +
		`echo "` + storeLossRemovedLine + `${f##*/}"; done; ` +
		`if [ -e "$r.json" ]; then kill -CONT "$p"; echo "` + storeLossRefused + ` ${r##*/}.json is still there"; ` +
		`exit 4; fi; ` +
		`kill -KILL "$p" && echo "killed instance-manager $p"`
}

// operationStoreLoss is the fault of S11 operation-store-loss: the kill of
// the standalone member's Instance Manager (instanceManagerKill) by
// storeLossScript, which removes the backup's operation record first. It is
// confirmed as the kill is, by the end of the database container with exit
// code 137; the record's removal is confirmed after the restart
// (storeLostAfterRestart).
type operationStoreLoss struct {
	instanceManagerKill
	id string
}

// operationIDPattern is the form of an operation ID the Instance Manager
// generates and accepts (newOperationID, isValidOperationID in
// internal/instancemanager/opstore.go).
var operationIDPattern = regexp.MustCompile(`^op-[0-9a-f]{32}$`)

func (f operationStoreLoss) Inject(ctx context.Context) error {
	if !operationIDPattern.MatchString(f.id) {
		return fmt.Errorf("%q is not an operation ID of the Instance Manager; nothing was removed or killed", f.id)
	}
	f.script = storeLossScript(f.id)
	if err := f.instanceManagerKill.Inject(ctx); err != nil {
		return err
	}
	if strings.Contains(f.k.KillAnswer, storeLossRefused) {
		return fmt.Errorf("nothing was killed in %s: %s", f.k.Pod, f.k.KillAnswer)
	}
	return nil
}

// runS11OperationStoreLoss runs S11 operation-store-loss after S11 complete
// passed, with the steps of S11 instance-manager-restart (s11IMRestartSteps)
// on a standalone cluster of its own: the rate limit, the backup, the
// placement once interruptedUploadSeen bytes passed while the backup is
// Running or Uploading with an operation, and the member's return within the
// ready limit. The fault is storeLossScript: the backup's operation record
// is removed from the member's operation store and its Instance Manager is
// killed, in one command. The restarted manager has no record of the
// operation, which the run confirms by listing the store (names only). The
// backup must then show RecordedOperationMissing and end Failed with
// RecordedOperationLost within BackupLimit, never Completed and without
// status.artifact, under the operation of the placing reading, in Running or
// Uploading until it ended; no manifest.json may be under its prefix at the
// end, the container must have restarted once, the source and the installed
// manager must be unchanged, and neither seed key may be in a status read,
// an Event or the logs, the killed Instance Manager's among them. The
// backup's staging directory on the member is recorded after the restart
// and at the end, not judged. The run removes the limit, records what is
// left under the prefix as a partial artifact, never writes to or removes
// from the object store, and deletes the backup and the cluster it created.
func (h *haRun) runS11OperationStoreLoss(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s11ID+"/"+s11StoreLossVariant, &result)()
	result = evidence.Scenario{ID: s11ID, Variant: s11StoreLossVariant, Result: evidence.Fail,
		NeverEvents: []string{}}
	throttle := h.newS11Throttle(s11StoreLossMember)
	rec := s11StoreLossRecord{s11IMRestartRecord: s11IMRestartRecord{Backup: s11StoreLossBackupName,
		Bucket: h.cfg.BackupBucket, Prefix: h.rec.Seed.Prefix + "/" + s11StoreLossVariant,
		ObjectStoreService: h.cfg.ObjectStoreService, PodSecurity: map[string]string{},
		Cluster:    &s11StandaloneCluster{Name: s11StoreLossClusterName, Secret: seedSecretName},
		ReadyLimit: h.cfg.BackupOperationStoreLossReadyLimit.String(), Throttle: &throttle,
		Kill: &s11IMKill{OperationRefsSeen: []string{}, placed: -1}, Phases: []s11Phase{},
		ArtifactsNotDone: []s11ArtifactReading{}, s11PrefixAtEnd: newS11PrefixAtEnd(),
		Credentials: &s11Credentials{FoundIn: []string{}},
		Notes: []string{"the rate limit acts in the network namespace of the standalone member's Pod, from an " +
			"ephemeral container with NET_ADMIN and NET_RAW (test/faults/netfault), as in S11 interrupted-upload; " +
			"the run namespace enforces Pod Security privileged for it, with warn and audit at restricted",
			"the fault is one command in the member's database container: it stops the Instance Manager with " +
				"SIGSTOP, removes <id>.json and <id>.*.tmp of the backup's operation from IM_OPERATIONS_DIR, and " +
				"kills the Instance Manager with SIGKILL, as S12 instance-manager-restart does; the entrypoint " +
				"shell exits, and the kubelet starts the container again on the same Pod and volume",
			"the fault is placed while the upload is under way: after interruptedUploadSeen bytes passed the " +
				"limit with the backup Running or Uploading with an operation",
			"the record's absence is confirmed by listing the operation store, names only, once the restarted " +
				"container is ready and the cluster reads Ready",
			"the limit stays until the backup ended, so the store is reached only slowly while the Instance " +
				"Manager restarts; nothing cuts the upload",
			"the backup's staging directory on the member is listed, names and sizes only, after the restart " +
				"and at the end; it is recorded, not judged",
			"the run never writes to or removes from the object store; with the seed Secret's keys it lists the " +
				"backup bucket under the prefix at the end",
			"the killed Instance Manager's log is the previous instance of the member's database container, read " +
				"through pods/log with previous=true",
			logsNote(h.logLimit())},
		Cleanup: nothingCreated,
		Residue: "nothing: the run wrote nothing to the object store" + stagingNotChecked,
		variant: "S11 operation-store-loss", timeline: s11StoreLossTimelineFile,
		sourceFile: s11StoreLossSourceDataCheckFile, readyLimit: h.cfg.BackupOperationStoreLossReadyLimit},
		MinBackupLimit: storeLossMinBackupLimit.String(), MissingWait: storeLossMissingWait.String(),
		StoreLoss: &s11StoreLoss{Removed: []string{}, StoreAfter: []string{}}}
	r := &rec.s11IMRestartRecord
	r.kill = func() faults.Fault {
		rec.StoreLoss.OperationID = r.Kill.OperationRef
		return operationStoreLoss{instanceManagerKill: instanceManagerKill{h: h, k: &r.Kill.memberKill,
			name: "remove the backup's operation record and kill the Instance Manager of the standalone member " +
				"with SIGKILL"}, id: r.Kill.OperationRef}
	}
	var made s11InterruptedMade
	defer func() {
		removed := h.removeInterruptedFaults(ctx, &made, r.Throttle)
		deleted := h.deleteInterrupted(ctx, made, r.Throttle, &r.Cleanup, &r.Residue)
		if st := rec.StagingAtEnd; st != nil && st.Present != nil {
			r.Residue = strings.TrimSuffix(r.Residue, stagingNotChecked) + stagingResidue(st)
		}
		cleaned := removed && deleted
		result.CleanupSucceeded = &cleaned
		if made.throttle != nil {
			maps.Copy(result.Measurements, prefixed("rateLimit.", made.throttle.Measurements()))
		}
		// A pass without its evidence is a failure; a blocked or failed S11
		// operation-store-loss keeps its result, with the evidence that could
		// not be written.
		if err := h.writeJSON(s11StoreLossRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S11 operation-store-loss evidence: "+
				err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S11/"+s11StoreLossVariant+"/")
		})
	}()
	result.Limits = map[string]string{backupLimitName: h.cfg.BackupLimit.String(),
		readyLimitName: h.cfg.BackupOperationStoreLossReadyLimit.String(), "missingWait": storeLossMissingWait.String()}
	result.Measurements = map[string]any{failedWithinKey: evidence.Unknown, "failedAfterKill": evidence.Unknown,
		"uploadBytesBeforeKill": evidence.Unknown, measMemberBackWithin: evidence.Unknown,
		"readyAgainWithin": evidence.Unknown, measContainerRestarts: evidence.Unknown,
		measOperationRefUnchanged: evidence.Unknown, measRecordAbsent: evidence.Unknown,
		measMissingAfterKill: evidence.Unknown, measLostAfterMissing: evidence.Unknown,
		measStagingAtRestart: evidence.Unknown, measStagingAtEnd: evidence.Unknown}
	err := h.s11IMRestartSteps(ctx, r, &made, func(ctx context.Context) error {
		return h.imRestartUpload(ctx, r, &made,
			func(ctx context.Context, read *databasev1alpha1.CubridBackup) error {
				return h.storeLossReading(ctx, read, &rec, &made, &result)
			},
			func(backup *databasev1alpha1.CubridBackup, waitErr error, requested time.Time) error {
				return judgeStoreLossBackup(backup, waitErr, requested, &rec, &result)
			})
	}, &result)
	if r.RequestedAt != nil && r.Kill.confirmed() {
		rec.StagingAtEnd = h.readStaging(ctx, r.Kill.Pod, r.BackupUID)
		if p := rec.StagingAtEnd.Present; p != nil {
			result.Measurements[measStagingAtEnd] = *p
		}
	}
	h.judgeStoreLossRun(ctx, err, &rec, &result)
	judged := time.Now().UTC()
	r.JudgedAt = &judged
	return result
}

// judgeStoreLossRun sets the result of S11 operation-store-loss from the
// error of its steps and the never-events they recorded.
func (h *haRun) judgeStoreLossRun(ctx context.Context, err error, rec *s11StoreLossRecord,
	result *evidence.Scenario) {
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	never := strings.Join(result.NeverEvents, "; ")
	parts := slices.Clone(result.NeverEvents)
	if err != nil {
		for part := range strings.SplitSeq(err.Error(), "\n") {
			if part != "" && !slices.Contains(parts, part) {
				parts = append(parts, part)
			}
		}
	}
	reason := strings.Join(parts, "; ")
	switch {
	case errors.As(err, new(backupVerdict)) || errors.As(err, new(restoreVerdict)) ||
		errors.As(err, new(dataViolation)) || never != "":
		result.Result, result.Reason = evidence.Fail, reason
	case err != nil && ctx.Err() != nil:
		result.Result, result.Reason = evidence.Blocked, "the run ended before S11 operation-store-loss was "+
			"judged: "+reason
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, reason
	default:
		k := rec.Kill
		result.Result = evidence.Pass
		result.Reason = fmt.Sprintf("the upload of %s's backup to %s was seen under way through the rate limit "+
			"(%d bytes) while the backup was %s with operation %s; one command in the member then removed the "+
			"operation's record%s from the Instance Manager's operation store and killed the Instance "+
			"Manager, confirmed by its database container's end with exit code %d; the container ran again on "+
			"the same Pod %s after the kill, a reading of the cluster after the restarted container was ready "+
			"showed Ready %s after the kill, within the ready limit counted from it, and the store listed after "+
			"the restart held no record of the operation; the backup showed %s %s after the kill and ended "+
			"Failed with %s within backup_limit, %s after status.operationMissingSince, under the same operation "+
			"in every reading from the placement on, Running or Uploading until it ended, never Completed and "+
			"without status.artifact, also when read again at the end; no manifest.json was under its prefix at "+
			"the end; the container restarted once and not before the kill; the limit was seen removed; the "+
			"source and the installed manager were unchanged; no seed key was in the statuses read, the Events, "+
			"the installed Operator's manager log or the member's log, whole, the killed Instance Manager's "+
			"previous instance included", s11StoreLossClusterName, rec.Bucket, k.SentBeforeKill,
			k.PhaseAtPlacement, k.OperationRef, removedText(rec.StoreLoss.Removed), sigkillExitCode,
			k.BackWithin, k.ReadyAgainWithin, s12OperationMissingReason, rec.MissingAfterKill,
			s11OperationLostReason, rec.FailedAfterMissingSince)
	}
}

// removedText names, for the pass reason, the files the fault answered that
// it removed; nothing when its answer was cut short before naming one.
func removedText(removed []string) string {
	if len(removed) == 0 {
		return ""
	}
	return " (" + strings.Join(removed, ", ") + ")"
}

// storeLossReading acts on one status read of the backup as S11
// instance-manager-restart does (imRestartReading), and once the reading
// that placed the fault saw the member back and the cluster Ready, confirms
// that the record is gone (storeLostAfterRestart).
func (h *haRun) storeLossReading(ctx context.Context, backup *databasev1alpha1.CubridBackup,
	rec *s11StoreLossRecord, made *s11InterruptedMade, result *evidence.Scenario) error {
	r := &rec.s11IMRestartRecord
	placed := r.Kill.PlacedAt != nil
	if err := h.imRestartReading(ctx, backup, r, made, result); err != nil || placed || r.Kill.PlacedAt == nil {
		return err
	}
	return h.storeLostAfterRestart(ctx, rec, result)
}

// storeLostAfterRestart lists the restarted member's operation store, names
// only, and requires that the backup's record, <id>.json, is not there. The
// restarted Instance Manager opens the store and fails every readable
// record that has not ended (reconcileOnStart in internal/instancemanager/
// opstore.go); it writes no record it does not find, and nothing else
// creates one for an operation that was started (FindOrCreate). A record
// that is there, or a store that cannot be listed, leaves the record's loss
// unconfirmed and blocks the variant. The backup's staging directory is
// then recorded.
func (h *haRun) storeLostAfterRestart(ctx context.Context, rec *s11StoreLossRecord,
	result *evidence.Scenario) error {
	l, k := rec.StoreLoss, rec.Kill
	for line := range strings.SplitSeq(k.KillAnswer, "\n") {
		// The error of the exec, cut short by the container's end, may follow
		// on the same line.
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), storeLossRemovedLine); ok {
			if name, _, _ := strings.Cut(rest, " "); name != "" {
				l.Removed = append(l.Removed, name)
			}
		}
	}
	out, err := h.member(ctx, k.Pod, "bash", "-c", "ls -1A -- '"+imOperationsDir+"'")
	at := time.Now().UTC()
	l.CheckedAt = &at
	if err != nil {
		l.ReadError = err.Error()
		return faultStep{fmt.Errorf("the operation store of %s could not be listed after the restart, so the "+
			"removal of the record of operation %s is not confirmed: %w", k.Pod, l.OperationID, err)}
	}
	for name := range strings.FieldsSeq(out) {
		l.StoreAfter = append(l.StoreAfter, name)
	}
	absent := !slices.Contains(l.StoreAfter, l.OperationID+".json")
	l.RecordAbsent = &absent
	result.Measurements[measRecordAbsent] = absent
	if !absent {
		return faultStep{fmt.Errorf("the operation store of %s holds %s.json after the restart: the record of the "+
			"backup's operation was not lost", k.Pod, l.OperationID)}
	}
	rec.StagingAfterRestart = h.readStaging(ctx, k.Pod, rec.BackupUID)
	if p := rec.StagingAfterRestart.Present; p != nil {
		result.Measurements[measStagingAtRestart] = *p
	}
	return nil
}

// stagingScript prints stagingPresent and then, one per line, the type,
// size and path of each entry below the backup staging directory dir, or
// stagingAbsent when it is not there. It prints no file's contents.
func stagingScript(dir string) string {
	return "d='" + dir + "'; if [ -d \"$d\" ]; then echo " + stagingPresent +
		"; find \"$d\" -mindepth 1 -printf '%y %s %P\\n'; else echo " + stagingAbsent + "; fi"
}

// readStaging lists the staging directory of the backup uid on member,
// names and sizes only. What could not be read is recorded: the staging
// directory is evidence, not a verdict.
func (h *haRun) readStaging(ctx context.Context, member, uid string) *s11Staging {
	st := &s11Staging{Dir: imBackupStagingRoot + "/" + uid, Entries: []string{}}
	at := time.Now().UTC()
	st.CheckedAt = &at
	if !plainName.MatchString(uid) {
		st.ReadError = "the backup's UID is not a plain name"
		return st
	}
	out, err := h.member(ctx, member, "bash", "-c", stagingScript(st.Dir))
	if err != nil {
		st.ReadError = err.Error()
		return st
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	switch lines[0] {
	case stagingPresent:
		present := true
		st.Present, st.Entries = &present, append(st.Entries, lines[1:]...)
	case stagingAbsent:
		present := false
		st.Present = &present
	default:
		st.ReadError = "the answer does not say whether the directory is there"
	}
	return st
}

// stagingResidue says what the run saw of the staging directory st at the
// end.
func stagingResidue(st *s11Staging) string {
	if !*st.Present {
		return "; the member's backup staging directory " + st.Dir + " was not there at the end"
	}
	return fmt.Sprintf("; the member's backup staging directory %s was there at the end with %d entries; it goes "+
		"with the data PVC", st.Dir, len(st.Entries))
}

// judgeStoreLossBackup judges the backup as the wait left it.
//
// The Operator polls the recorded operation by status.operationRef
// (pollBackup in internal/controller/cubridbackup_controller.go). While the
// member's Instance Manager restarts, the poll fails to reach it and keeps
// the phase. The restarted manager holds no record of the operation:
// GET /v1/operations/<id> answers 404 {"error":"operation not found"}
// (getOperation in internal/instancemanager/server.go, ErrOperationNotFound
// from Get in opstore.go), which the client maps to errOperationMissing
// (GetOperation in internal/controller/imclient.go). The Operator then sets
// status.operationMissingSince once, keeps the phase, sets Ready False with
// RecordedOperationMissing, and polls again; once more than
// backupOperationMissingWait, 30 minutes, has passed since then, it ends the
// backup Failed with RecordedOperationLost (operationMissing, ended). It
// never sends another start for a backup with an operationRef, and a Failed
// backup is never reconciled again.
//
// So once the kill was confirmed and the record's absence after the restart
// too: Completed, a status.artifact, and another operationRef are
// never-events (judgeKilledBackup); Failed must be RecordedOperationLost,
// after a reading of RecordedOperationMissing, with every reading between
// the placement and the end Running or Uploading, and at least
// storeLossMissingWait after status.operationMissingSince. Failed for the
// manager's restart says the restarted manager still had the record, and
// any other failure is named; both fail the variant. A backup_limit that ran
// out before the Operator's bound could be reached (boundNotReached) blocks
// it instead.
func judgeStoreLossBackup(backup *databasev1alpha1.CubridBackup, waitErr error, requested time.Time,
	rec *s11StoreLossRecord, result *evidence.Scenario) error {
	r := &rec.s11IMRestartRecord
	s, k := backup.Status, r.Kill
	if since := s.OperationMissingSince; since != nil {
		at := since.UTC()
		rec.OperationMissingSince = &at
	}
	if done, err := judgeKilledBackup(backup, waitErr, r, result); done {
		// Only the wait's own error: a never-event is joined into another.
		var v backupVerdict
		if err == waitErr && errors.As(err, &v) && errors.Is(v.error, errBackupLimitPassed) {
			if short := boundNotReached(rec, s); short != nil {
				return short
			}
		}
		return err
	}
	if l := rec.StoreLoss; l.RecordAbsent == nil || !*l.RecordAbsent {
		return faultStep{fmt.Errorf("the backup ended Failed after the kill, but the record's absence after the "+
			"restart was not confirmed: %s", r.Failure)}
	}
	c := meta.FindStatusCondition(s.Conditions, backupReady)
	switch {
	case c != nil && c.Reason == s11MissingReason && c.Message == s12IMRestartedReason:
		return backupVerdict{fmt.Errorf("the backup ended Failed with %s %q, the Instance Manager's restart, not "+
			"%s: the restarted Instance Manager still held the record of operation %s", s11MissingReason,
			s12IMRestartedReason, s11OperationLostReason, k.OperationRef)}
	case c == nil || c.Reason != s11OperationLostReason:
		return backupVerdict{fmt.Errorf("the backup ended Failed after its operation record was removed and its "+
			"Instance Manager killed, but not with %s: %s", s11OperationLostReason, failureWithStore(r, c))}
	}
	after := r.Phases[min(max(k.placed+1, 0), len(r.Phases)):]
	for i, p := range after {
		if i < len(after)-1 && !inProgress(databasev1alpha1.CubridBackupPhase(p.Phase)) {
			return backupVerdict{fmt.Errorf("the backup was %s at %s, after the kill and before it ended: a backup "+
				"whose record is missing keeps Running or Uploading until it ends", p.Phase,
				p.At.Format(time.RFC3339Nano))}
		}
	}
	missing := slices.IndexFunc(after, func(p s11Phase) bool {
		pc := meta.FindStatusCondition(p.Conditions, backupReady)
		return pc != nil && pc.Reason == s12OperationMissingReason
	})
	if missing >= 0 {
		first := after[missing].At
		rec.FirstMissingAt = &first
		if k.KilledAt != nil {
			rec.MissingAfterKill = first.Sub(*k.KilledAt).Round(time.Millisecond).String()
			result.Measurements[measMissingAfterKill] = rec.MissingAfterKill
		}
	}
	if missing < 0 {
		return backupVerdict{fmt.Errorf("the backup ended Failed with %s, but no reading after the kill showed %s "+
			"first", s11OperationLostReason, s12OperationMissingReason)}
	}
	since, done := s.OperationMissingSince, s.CompletedAt
	if since == nil || done == nil {
		return fmt.Errorf("the backup ended Failed with %s without status.operationMissingSince and "+
			"status.completedAt both set, so the bound of %s cannot be checked", s11OperationLostReason,
			storeLossMissingWait)
	}
	lost := done.Sub(since.Time)
	rec.FailedAfterMissingSince = lost.String()
	result.Measurements[measLostAfterMissing] = rec.FailedAfterMissingSince
	if lost < storeLossMissingWait {
		return backupVerdict{fmt.Errorf("the backup ended %s %s after status.operationMissingSince, before the "+
			"bound of %s", s11OperationLostReason, lost, storeLossMissingWait)}
	}
	recordFailedTimes(backup, requested, r, result)
	return nil
}

// boundNotReached returns the block of a backup whose backup_limit ran out
// while it read RecordedOperationMissing and less than storeLossMissingWait
// had passed from status.operationMissingSince to the last reading: the
// Operator's bound starts at its first missing answer, after the restart,
// and backup_limit at the request, so a slow restart leaves the outcome
// unobserved, not wrong. The last reading's time is the run's clock and
// operationMissingSince the Operator's. Otherwise it returns nil.
func boundNotReached(rec *s11StoreLossRecord, s databasev1alpha1.CubridBackupStatus) error {
	phases := rec.Phases
	c := meta.FindStatusCondition(s.Conditions, backupReady)
	if len(phases) == 0 || s.OperationMissingSince == nil || !inProgress(s.Phase) || c == nil ||
		c.Reason != s12OperationMissingReason {
		return nil
	}
	waited := phases[len(phases)-1].At.Sub(s.OperationMissingSince.Time)
	if waited >= storeLossMissingWait {
		return nil
	}
	return fmt.Errorf("inconclusive: backup_limit ran out while the backup read %s, %s after "+
		"status.operationMissingSince, before the Operator's bound of %s; a longer -backup-limit is needed to see "+
		"the end", s12OperationMissingReason, waited.Round(time.Second), storeLossMissingWait)
}
