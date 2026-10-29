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
	"path"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

const (
	s11RestartLane                = "lab-installed-ha-s11-operator-restart"
	s11RestartVariant             = "operator-restart"
	s11RestartRecordFile          = "S11/operator-restart/record.json"
	s11RestartTimelineFile        = "S11/operator-restart/timeline.jsonl"
	s11RestartManifestFile        = "S11/operator-restart/manifest.json"
	s11RestartSourceDataCheckFile = "S11/operator-restart/source-data-check.json"
	s11RestartBackupName          = "s11-operator-restart"
	// s11OperationLostReason is the Ready reason of a backup whose recorded
	// operation its target held no record of for backupOperationMissingWait
	// (reasonRecordedOperationLost in
	// internal/controller/cubridbackup_controller.go): the documented
	// escalation of an unknown outcome, under which a backup may still run.
	s11OperationLostReason = "RecordedOperationLost"
)

// notPlaced is an error that says the restart could not be placed while the
// backup was Running or Uploading with an operation: the variant is
// inconclusive.
type notPlaced struct{ error }

// s11Restart is the manager's restart during the backup and the operation
// the backup recorded around it.
type s11Restart struct {
	managerRestart
	// PlacedAt is the reading that placed the restart, the first that showed
	// the backup Running or Uploading with an operation; PhaseAtPlacement,
	// OperationRef, TargetInstance and TargetRole are its status's.
	PlacedAt         *time.Time `json:"placedAt,omitempty"`
	PhaseAtPlacement string     `json:"phaseAtPlacement,omitempty"`
	OperationRef     string     `json:"operationRef,omitempty"`
	TargetInstance   string     `json:"targetInstance,omitempty"`
	TargetRole       string     `json:"targetRole,omitempty"`
	// PhaseAtRestart is the phase of the reading right before the manager Pod
	// was deleted; InProgressAtRestart is set when it was Running or
	// Uploading. Neither shows that the Instance Manager was still working on
	// the backup then.
	PhaseAtRestart      string `json:"phaseAtRestart,omitempty"`
	InProgressAtRestart bool   `json:"inProgressAtRestart"`
	// StatusCompletedAt is the backup's status.completedAt, which the manager
	// that wrote Completed set, to the second. It must not be before the
	// second of the replacement's Lease acquireTime (LeaseAcquiredAt):
	// otherwise the old manager, which keeps running and holding the Lease
	// for a while after a deletion without a grace period, completed it.
	StatusCompletedAt *time.Time `json:"statusCompletedAt,omitempty"`
	// ManifestCreatedAt is the manifest's createdAt, which the Instance
	// Manager sets right before its upload, by the member's clock;
	// UploadStartedBeforeRestart is set when it is before the second of
	// RestartedAt, by the run's clock. It is evidence of where the restart fell in the operation,
	// not a verdict.
	ManifestCreatedAt          *time.Time `json:"manifestCreatedAt,omitempty"`
	UploadStartedBeforeRestart *bool      `json:"uploadStartedBeforeRestart,omitempty"`
	// NotPlaced says why the restart could not be placed during the backup.
	NotPlaced string `json:"notPlaced,omitempty"`
	// OperationRefsSeen are the operationRefs the readings showed from the
	// placement on, in order, each once.
	OperationRefsSeen []string `json:"operationRefsSeen"`
	// placed is the index in the record's phases of the placing reading.
	placed int
}

// s11AttemptPrefix is one attempt prefix of the backup that held objects at
// the end, with how many, and whether a manifest.json was among them.
type s11AttemptPrefix struct {
	Prefix   string `json:"prefix"`
	Objects  int    `json:"objects"`
	Manifest bool   `json:"manifest"`
}

// s11Attempts is what the run's own client listed under the variant's
// prefix at the end. Outside lists the objects that are not under an attempt
// prefix of the backup.
type s11Attempts struct {
	Listed    bool               `json:"listed"`
	Prefixes  []s11AttemptPrefix `json:"prefixes"`
	Manifests int                `json:"manifests"`
	Outside   []string           `json:"outside"`
}

// s11RestartRecord is the evidence of S11 operator-restart besides the
// manifest, the timeline and the source's data check.
type s11RestartRecord struct {
	s11Record
	Restart  *s11Restart  `json:"restart,omitempty"`
	Attempts *s11Attempts `json:"attempts,omitempty"`
	// AttemptsAtEnd is the same listing, made again at the end of the
	// variant, after the source check.
	AttemptsAtEnd *s11Attempts `json:"attemptsAtEnd,omitempty"`
	// Failure is the Ready reason and message of a backup that ended Failed.
	Failure string `json:"failure,omitempty"`
	// ManagerBefore is the installed manager as LAB01 read it at its end,
	// ManagerAfter as this variant read it at its end.
	ManagerBefore *managerIdentity `json:"managerBefore,omitempty"`
	ManagerAfter  *managerIdentity `json:"managerAfter,omitempty"`
	Source        *s12Source       `json:"source,omitempty"`
	// JudgedAt is when the run reached its verdict.
	JudgedAt *time.Time `json:"judgedAt,omitempty"`
	// Cleanup says whether the CubridBackup the run created was deleted.
	Cleanup string `json:"cleanup"`
}

// validateS11OperatorRestart checks the inputs of S11 operator-restart, when
// it is selected; BackupBucket is checked by validateS11.
func (c HAConfig) validateS11OperatorRestart() error {
	if !c.BackupOperatorRestart {
		return nil
	}
	for _, other := range []struct {
		selected bool
		flag     string
	}{{c.Restore, flagRestore}, {c.BackupMissingDestination, flagMissingDestination},
		{c.BackupDeniedCredentials, flagDeniedCredentials},
		{c.BackupSignatureMismatch, flagSignatureMismatch},
		{c.BackupInterruptedUpload, flagInterruptedUpload},
		{c.BackupInstanceManagerRestart, flagInstanceManagerRestart},
		{c.BackupOverlapping, flagOverlapping}} {
		if other.selected {
			return fmt.Errorf("-backup-operator-restart and %s select different lanes; a run takes one of them",
				other.flag)
		}
	}
	if c.ResyncLimit <= 0 || len(validation.IsDNS1123Subdomain(c.LeaderLease)) != 0 {
		return errors.New("-backup-operator-restart needs a positive -resync-limit and the -leader-lease")
	}
	return nil
}

// planS11OperatorRestart returns the lane and the requirements of a run that
// selects S11 operator-restart after S11 complete, and names it and the
// shared manager in the summary's notes; otherwise lane and requirements as
// they are.
func planS11OperatorRestart(cfg HAConfig, lane string, requirements []evidence.Requirement,
	summary *evidence.Summary) (string, []evidence.Requirement) {
	if !cfg.BackupOperatorRestart {
		return lane, requirements
	}
	summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete and S11 " +
		"operator-restart: a full backup to object storage, downloaded and checked by the run, and a second one " +
		"during which the manager Pod is deleted without a grace period, which must end Completed with the same " +
		"operation and one verified artifact; no other S00-S15 qualification"
	summary.Notes = append(summary.Notes, "The installed manager is shared: while its Pod is replaced, no "+
		"cluster or backup it manages is reconciled")
	return s11RestartLane, append(requirements, evidence.Requirement{ID: s11ID, Variant: s11RestartVariant})
}

// runS11OperatorRestart runs S11 operator-restart after S11 complete passed:
// a second full backup of the HA cluster is requested to BackupBucket under
// the variant's prefix, and once a reading shows it Running or Uploading
// with an operation, the manager's Pod is deleted without a grace period, as
// S06 and S12 operator-restart delete it. The replacement must run the same
// image and hold the leader Lease within ResyncLimit. The backup must end
// Completed within BackupLimit of its request, judged written by the replacement
// (completedAfterTakeover), its operation, target and role must never change
// from the placing reading on, its artifact must pass S11 complete's
// verification, exactly one attempt prefix of it may hold objects under the
// variant's prefix, also when listed again at the end, the source must be
// unchanged and no credential may be in a status read, the manifest, an
// Event or the logs searched. A restart that cannot be placed while the
// backup runs, a completion the old manager wrote, the documented escalation
// of an unknown outcome, a run that ends, or a check that cannot be made,
// before S11 operator-restart is judged blocks it; what was seen to break
// stays a failure. The run deletes the CubridBackup it created, and
// leaves the artifact in the bucket.
func (h *haRun) runS11OperatorRestart(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s11ID+"/"+s11RestartVariant, &result)()
	result = evidence.Scenario{ID: s11ID, Variant: s11RestartVariant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s11RestartRecord{s11Record: s11Record{Backup: s11RestartBackupName, Bucket: h.cfg.BackupBucket,
		Prefix: h.rec.Seed.Prefix + "/operator-restart", ObjectStoreService: h.cfg.ObjectStoreService,
		Phases: []s11Phase{}, EngineVersion: h.engineVersion, Credentials: &s11Credentials{FoundIn: []string{}},
		Notes: []string{"the restart is placed after a reading that saw the backup Running or Uploading with an " +
			"operation; the reading right before the deletion is recorded, and neither shows that the Instance " +
			"Manager was still working on the backup when the deletion took effect",
			"the manifest's createdAt is set by the Instance Manager right before its upload, by the member's " +
				"clock, and the restart's time is the run's clock; when createdAt is before the restart " +
				"(uploadStartedBeforeRestart), the replacement discovered and completed an operation that was " +
				"already uploading or done in the Instance Manager, not one restarted in the middle of backupdb; " +
				"this is evidence of where the restart fell, not a verdict",
			"status.completedAt is set to the second by the manager that wrote Completed, and the Lease's " +
				"acquireTime by the replacement, each by its own node's clock; a completedAt before the second of " +
				"the acquireTime blocks the variant, since the deleted manager may still run and hold the Lease " +
				"until the kubelet kills it",
			"the installed manager is shared: while its Pod is replaced, no cluster or backup it manages is " +
				"reconciled",
			"the deleted manager's log was searched right before its deletion; what it logged after that search " +
				"was not searched, since pods/log cannot read the log of a deleted Pod; the replacement's log and " +
				"the Instance Manager's log in the backup's target Pod were searched at the end",
			logsNote(h.logLimit())},
		Residue: "the artifact under the variant's prefix is left in the bucket; this run does not remove it"},
		Restart: &s11Restart{OperationRefsSeen: []string{}}, Cleanup: "nothing to delete: the run created no " +
			"CubridBackup"}
	var created *databasev1alpha1.CubridBackup
	defer func() {
		cleaned := h.deleteMissingBackup(ctx, created, &rec.Cleanup)
		result.CleanupSucceeded = &cleaned
		// A pass without its evidence is a failure; a blocked or failed S11
		// operator-restart keeps its result, with the evidence that could not
		// be written.
		if err := h.writeJSON(s11RestartRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S11 operator-restart evidence: "+
				err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S11/"+s11RestartVariant+"/")
		})
	}()
	result.Limits = map[string]string{backupLimitName: h.cfg.BackupLimit.String(),
		resyncLimitName: h.cfg.ResyncLimit.String()}
	result.Measurements = map[string]any{measBackupCompletedWithin: evidence.Unknown,
		operatorBackMeasurement: evidence.Unknown, "operationRefUnchanged": evidence.Unknown,
		"inProgressAtRestart": evidence.Unknown, "uploadStartedBeforeRestart": evidence.Unknown,
		measObjectsVerified: evidence.Unknown, measArtifactBytes: evidence.Unknown,
		measObjectsNotInManifest: evidence.Unknown, "attemptPrefixes": evidence.Unknown}
	err := h.s11RestartSteps(ctx, &result, &rec, &created)
	// A credential found in any status read, the manifest, an Event or a log
	// is a failure, whichever step ended S11 operator-restart.
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	never := strings.Join(result.NeverEvents, "; ")
	// The reason names each never-event once, then what else went wrong.
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
		errors.As(err, new(dataViolation)):
		result.Result, result.Reason = evidence.Fail, reason
	case never != "":
		// What was not checked keeps a confirmed failure a failure.
		result.Result, result.Reason = evidence.Fail, reason
	case err != nil && ctx.Err() != nil:
		result.Result, result.Reason = evidence.Blocked, "the run ended before S11 operator-restart was judged: "+
			reason
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, reason
	default:
		result.Result, result.Reason = evidence.Pass, s11RestartPassReason(rec.Restart)
	}
	judged := time.Now().UTC()
	rec.JudgedAt = &judged
	return result
}

// s11RestartPassReason is the reason of a pass, from what the run recorded.
func s11RestartPassReason(r *s11Restart) string {
	return fmt.Sprintf("the manager Pod was deleted without a grace period after a reading showed the backup "+
		"%s with operation %s on %s, and the reading right before the deletion showed it %s; the replacement "+
		"ran the same image and held the leader Lease, back %s after the deletion, within operator_resync_limit; "+
		"the backup ended Completed within backup_limit, with status.completedAt not before the second the "+
		"replacement acquired the Lease, and with the same operation, target and role in every reading "+
		"from the placement on; every object the manifest names was downloaded by the run and matches it, the "+
		"status and the cluster, and exactly one attempt prefix of the backup, holding the one manifest.json, held "+
		"objects under the variant's prefix; the source was unchanged; no credential was found in the recorded "+
		"statuses, the manifest, the Events, the deleted manager's log as read right before its deletion, the "+
		"replacement's log or the Instance Manager's log in the backup's target Pod", r.PhaseAtPlacement,
		r.OperationRef, r.TargetInstance, r.PhaseAtRestart, r.BackWithin)
}

// s11RestartSteps runs the steps of S11 operator-restart in order. Before
// the request, the first error ends them. After it, the backup is judged and
// the checks at the end are all made; their errors are joined.
func (h *haRun) s11RestartSteps(ctx context.Context, result *evidence.Scenario, rec *s11RestartRecord,
	created **databasev1alpha1.CubridBackup) error {
	started := time.Now().UTC()
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S11 operator-restart could not tell whether a credential is in a status, the "+
			"manifest or an Event", err)
	}
	before := h.rec.OperatorAfter
	rec.ManagerBefore = &before
	if len(before.Pods) != 1 {
		return errors.New("S11 operator-restart needs exactly one manager Pod")
	}
	r := rec.Restart
	r.ManagerBefore = before.Pods[0]
	backup, err := h.createBackup(ctx, s11RestartBackupName, haClusterName, rec.Bucket, rec.Prefix)
	if err != nil {
		return err
	}
	requested := time.Now().UTC()
	*created = backup
	rec.BackupUID, rec.RequestedAt = string(backup.UID), &requested
	since := started.Add(-logSinceMargin).Truncate(time.Second)
	// oldLog is the search of the deleted manager's log, made right before
	// its deletion; late is a replacement that was back after
	// operator_resync_limit, which stays a failure whatever the backup does.
	var oldLog, late error
	backup, err = h.waitBackupWith(ctx, backup, &rec.Phases, rec.Credentials,
		func(ctx context.Context, read *databasev1alpha1.CubridBackup) error {
			if r.PlacedAt != nil {
				return nil
			}
			return h.placeRestart(ctx, *created, read, rec, result, since, &oldLog, &late)
		})
	errs := []error{late}
	manifest, judged := h.judgeRestartedBackup(ctx, backup, err, requested, rec, result)
	errs = append(errs, judged)
	if err := h.checkS11Credentials(ctx, *created, manifest, rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: %w", err))
	}
	// Without a deletion the manager LAB01 checked is still the one, and its
	// log is searched as the other variants search it.
	managers := before.Pods
	if r.RestartedAt != nil {
		managers = nil
		if r.ManagerAfter != nil {
			managers = []imageIdentity{*r.ManagerAfter}
		}
		if oldLog != nil {
			errs = append(errs, fmt.Errorf("search the deleted manager's log: %w", oldLog))
		}
	}
	if err := h.checkS11Logs(ctx, haClusterName, false, lastTarget(rec.Phases),
		requestedOnServer(*created, requested), started, rec.Credentials, managers); err != nil {
		errs = append(errs, fmt.Errorf("search the logs: %w", err))
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	errs = append(errs, h.checkSourceS12(ctx, rec.Source, s11RestartSourceDataCheckFile))
	// A second writer that was still uploading at the first listing shows up
	// now.
	if rec.Attempts != nil && rec.Attempts.Listed {
		at, err := h.listS11Attempts(ctx, *created, &rec.s11Record)
		rec.AttemptsAtEnd = at
		if err != nil {
			errs = append(errs, fmt.Errorf("list the variant's prefix again at the end: %w", err))
		} else {
			result.NeverEvents = append(result.NeverEvents, secondWriter(at, "at the end")...)
		}
	}
	errs = append(errs, h.managerAfterRestart(ctx, before, r, &rec.ManagerAfter))
	return errors.Join(errs...)
}

// placeRestart places the restart at read, a reading of the backup not yet
// placed: a reading in phase Running or Uploading with an operation, both
// in progress with the operation recorded, deletes the manager's Pod
// (restartDuringBackup); one that is Pending or without a phase, or in
// progress without an operation yet, waits for the next. Completed or Failed
// first means the restart cannot be placed while the backup runs, which
// blocks the variant.
func (h *haRun) placeRestart(ctx context.Context, created, read *databasev1alpha1.CubridBackup,
	rec *s11RestartRecord, result *evidence.Scenario, since time.Time, oldLog, late *error) error {
	r, s := rec.Restart, read.Status
	switch {
	case s.Phase == "" || s.Phase == databasev1alpha1.BackupPhasePending:
		return nil
	case inProgress(s.Phase) && s.OperationRef == "":
		return nil
	case !inProgress(s.Phase):
		r.NotPlaced = fmt.Sprintf("the restart could not be placed while the backup ran: no reading showed it "+
			"Running or Uploading with an operation before one showed it %s (%s)", s.Phase, readyReason(read))
		return notPlaced{errors.New(r.NotPlaced)}
	}
	at := time.Now().UTC()
	r.PlacedAt, r.placed, r.PhaseAtPlacement = &at, len(rec.Phases)-1, string(s.Phase)
	r.OperationRef, r.TargetInstance, r.TargetRole = s.OperationRef, s.TargetInstance, s.TargetRole
	back, err := h.restartDuringBackup(ctx, created, rec, result, since, oldLog)
	if back > h.cfg.ResyncLimit {
		*late = backupVerdict{fmt.Errorf("the replacement manager was back %s after its Pod was deleted, over "+
			"operator_resync_limit %s", back.Round(time.Millisecond), h.cfg.ResyncLimit)}
	}
	return err
}

// restartDuringBackup deletes the manager's Pod as S06 restart does, once
// the manager LAB01 checked is seen again and the backup is read right
// before, and waits for the replacement (managerBack). It returns the time
// from the deletion to the reading that saw the replacement back.
func (h *haRun) restartDuringBackup(ctx context.Context, created *databasev1alpha1.CubridBackup,
	rec *s11RestartRecord, result *evidence.Scenario, since time.Time, oldLog *error) (time.Duration, error) {
	r := rec.Restart
	// seen is what the reading right before the deletion showed that ends
	// the variant.
	var seen error
	var goneAt time.Time
	outcome := faults.Run(ctx, faults.Scenario{
		Fault:  managerPodKill{h: h, pod: r.ManagerBefore},
		Limits: h.s06FlowLimits(),
		Before: func(ctx context.Context) error {
			seen = h.beforeBackupRestart(ctx, created, rec, since, oldLog)
			return seen
		},
		Check: func(ctx context.Context) (bool, error) {
			if goneAt.IsZero() {
				goneAt = time.Now().UTC()
				r.GoneAt = &goneAt
			}
			done, err := h.managerBack(ctx, &r.managerRestart)
			if ctx.Err() != nil {
				// The step's limit, or the run's: not a broken expectation.
				return false, nil
			}
			return done, err
		},
	})
	if !outcome.FaultIssuedAt.IsZero() {
		at := outcome.FaultIssuedAt
		r.RestartedAt = &at
	}
	if err := h.writeTimeline(s11RestartTimelineFile, outcome.Timeline); err != nil {
		return 0, err
	}
	switch {
	case errors.As(seen, new(notPlaced)) || errors.As(seen, new(backupVerdict)):
		return 0, seen
	case r.ImageChanged:
		return 0, fmt.Errorf("the environment changed: %s", outcome.Reason)
	case outcome.Result == evidence.Fail:
		return 0, backupVerdict{fmt.Errorf("the manager was not back after its Pod was deleted: %s", outcome.Reason)}
	case outcome.Result != evidence.Pass:
		return 0, errors.New(outcome.Reason)
	}
	back := outcome.OutcomeAt.Sub(outcome.FaultIssuedAt)
	r.BackWithin = back.Round(time.Millisecond).String()
	result.Measurements[operatorBackMeasurement] = r.BackWithin
	return back, nil
}

// beforeBackupRestart requires the manager Pod LAB01 checked, searches its
// log into oldLog while it can still be read, and reads the backup once more,
// recording the reading as the one right before the deletion. A backup that
// ended by then cannot have the restart placed during it.
func (h *haRun) beforeBackupRestart(ctx context.Context, created *databasev1alpha1.CubridBackup,
	rec *s11RestartRecord, since time.Time, oldLog *error) error {
	r := rec.Restart
	manager, err := h.observeManager(ctx, h.cfg.Config)
	if err != nil {
		return err
	}
	if len(manager.Pods) != 1 || manager.Pods[0] != r.ManagerBefore {
		return errors.New("the manager Pod is not the one LAB01 checked")
	}
	old := r.ManagerBefore
	*oldLog = h.searchLog(ctx, rec.Credentials, s11LogCheck{Log: operatorLog, Namespace: h.cfg.OperatorNamespace,
		Pod: old.Pod, UID: old.UID, Container: managerContainer, ReadFrom: logFromSinceTime, SinceTime: since}, nil)
	backup := &databasev1alpha1.CubridBackup{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), backup); err != nil {
		return err
	}
	if backup.UID != created.UID {
		return backupVerdict{errors.New("the CubridBackup was replaced")}
	}
	if err := h.recordBackup(backup, &rec.Phases, rec.Credentials); err != nil {
		return err
	}
	s := backup.Status
	r.PhaseAtRestart = string(s.Phase)
	if s.Phase == databasev1alpha1.BackupPhaseCompleted || s.Phase == databasev1alpha1.BackupPhaseFailed {
		r.NotPlaced = fmt.Sprintf("the restart could not be placed while the backup ran: it ended %s (%s) before "+
			"the manager Pod was deleted", s.Phase, readyReason(backup))
		return notPlaced{errors.New(r.NotPlaced)}
	}
	r.InProgressAtRestart = inProgress(s.Phase)
	return nil
}

// inProgress reports a backup phase that is not terminal and follows the
// start on the Instance Manager: Running or Uploading.
func inProgress(phase databasev1alpha1.CubridBackupPhase) bool {
	return phase == databasev1alpha1.BackupPhaseRunning || phase == databasev1alpha1.BackupPhaseUploading
}

// judgeRestartedBackup judges the backup as the wait left it, and returns
// the manifest as stored when the artifact could be read.
//
// The Operator records status.operationRef in the same status write that
// makes the backup Running (startBackup in
// internal/controller/cubridbackup_controller.go), so the restart is placed
// after it is stored. A replacement then reconciles a backup with an
// operationRef: it only polls that operation on the recorded target
// (pollBackup) and never selects a target or sends a start again, since
// startBackup and adoptBackup, the only writers of operationRef, are not
// reached with one recorded. The Instance Manager runs in the member's Pod,
// which the restart does not touch. So the code predicts Completed with the
// operation, target and role of the placing reading; another operationRef,
// target or role from that reading on must never happen. Failed for the
// Instance Manager's reason fails the variant. Failed with
// RecordedOperationLost, after the target held no record of the operation
// for backupOperationMissingWait, is the documented escalation of an unknown
// outcome: it blocks the variant.
func (h *haRun) judgeRestartedBackup(ctx context.Context, backup *databasev1alpha1.CubridBackup, waitErr error,
	requested time.Time, rec *s11RestartRecord, result *evidence.Scenario) ([]byte, error) {
	judgeRestartOperation(rec, result)
	switch {
	case errors.As(waitErr, new(notPlaced)) || errors.As(waitErr, new(backupVerdict)):
		return nil, waitErr
	case waitErr != nil:
		// The backup could not be read, or the restart not made: S11
		// operator-restart could not be judged.
		return nil, waitErr
	}
	s := backup.Status
	if s.Phase == databasev1alpha1.BackupPhaseFailed {
		rec.Failure = readyReason(backup)
		if c := meta.FindStatusCondition(s.Conditions, backupReady); c != nil && c.Reason == s11OperationLostReason {
			return nil, fmt.Errorf("the backup reached the documented escalation %s after the manager restarted, "+
				"an unknown outcome under which a backup may still run: %s", s11OperationLostReason, rec.Failure)
		}
		return nil, backupVerdict{fmt.Errorf("the backup ended Failed after the manager restarted: %s",
			rec.Failure)}
	}
	within := rec.Phases[len(rec.Phases)-1].At.Sub(requested)
	rec.CompletedWithin = within.Round(time.Millisecond).String()
	result.Measurements[measBackupCompletedWithin] = rec.CompletedWithin
	if done := s.CompletedAt; done != nil {
		rec.StatusCompletedWithin = done.Sub(backup.CreationTimestamp.Time).String()
	}
	rec.Artifact = s.Artifact
	if rec.Artifact == nil {
		result.NeverEvents = append(result.NeverEvents, "the backup is Completed without an artifact in its status")
		return nil, nil
	}
	manifest, verifyErr := h.verifyS11Artifact(ctx, backup, &rec.s11Record)
	var errs []error
	if manifest != nil {
		if err := h.write(s11RestartManifestFile, manifest); err != nil {
			errs = append(errs, fmt.Errorf("write the manifest: %w", err))
		}
	}
	v := rec.Verification
	if v.ObjectsChecked {
		result.Measurements[measObjectsVerified] = v.ObjectsVerified
		result.Measurements[measArtifactBytes] = v.BytesDownloaded
	}
	if v.Listed {
		result.Measurements[measObjectsNotInManifest] = len(v.NotInManifest)
	}
	if len(v.Unreadable) > 0 {
		result.NeverEvents = append(result.NeverEvents, "the backup is Completed, and objects of its artifact could "+
			"not be read: "+strings.Join(v.Unreadable, "; "))
	}
	if len(v.Problems) > 0 {
		errs = append(errs, backupVerdict{errors.New("the artifact does not match: " +
			strings.Join(v.Problems, "; "))})
	}
	if verifyErr != nil {
		errs = append(errs, fmt.Errorf("verify the artifact: %w", verifyErr))
	}
	if err := h.checkS11Attempts(ctx, backup, rec, result); err != nil {
		errs = append(errs, fmt.Errorf("list the variant's prefix: %w", err))
	}
	uploadStarted(manifest, rec.Restart, result)
	errs = append(errs, completedAfterTakeover(backup, rec.Restart))
	return manifest, errors.Join(errs...)
}

// completedAfterTakeover records status.completedAt and requires it at or
// after the second of the replacement's Lease acquireTime. completed in
// internal/controller/cubridbackup_controller.go sets completedAt when it
// writes Completed, and the API server keeps it to the second. A deletion
// without a grace period leaves the old manager running, and holding the
// Lease, until the kubelet kills it (the manager does not release the Lease
// on cancel, cmd/main.go), so it may still write Completed itself; then the
// replacement's resumption of the operation was not observed, which blocks
// the variant. Both times are servers' clocks of the managers' nodes, not
// the run's.
func completedAfterTakeover(backup *databasev1alpha1.CubridBackup, r *s11Restart) error {
	done := backup.Status.CompletedAt
	if done != nil {
		at := done.UTC()
		r.StatusCompletedAt = &at
	}
	switch {
	case done == nil:
		return errors.New("the backup is Completed without status.completedAt, so which manager completed it " +
			"cannot be told")
	case r.LeaseAcquiredAt == nil:
		return errors.New("the replacement's Lease acquireTime was not read, so which manager completed the " +
			"backup cannot be told")
	case done.Time.Before(r.LeaseAcquiredAt.Truncate(time.Second)):
		return fmt.Errorf("the old manager completed the backup before the replacement led: status.completedAt "+
			"is %s, before the replacement acquired the Lease at %s", done.UTC().Format(time.RFC3339),
			r.LeaseAcquiredAt.Format(time.RFC3339Nano))
	}
	return nil
}

// uploadStarted records the manifest's createdAt, which the Instance Manager
// sets right before it uploads (runBackup in
// internal/instancemanager/server.go), and whether it is before the restart.
// createdAt is the member's clock to the second and the restart's time the
// run's: it is evidence of where the restart fell, not a verdict.
func uploadStarted(manifest []byte, r *s11Restart, result *evidence.Scenario) {
	if manifest == nil || r.RestartedAt == nil {
		return
	}
	parsed, err := instancemanager.ParseManifest(manifest)
	if err != nil {
		return
	}
	created, err := time.Parse(time.RFC3339, parsed.CreatedAt)
	if err != nil {
		return
	}
	created = created.UTC()
	// createdAt is to the second: one in the restart's second is not counted
	// as before it.
	before := created.Before(r.RestartedAt.Truncate(time.Second))
	r.ManifestCreatedAt, r.UploadStartedBeforeRestart = &created, &before
	result.Measurements["uploadStartedBeforeRestart"] = before
	result.Measurements["manifestCreatedAt"] = created.Format(time.RFC3339)
}

// judgeRestartOperation records the operationRefs the readings showed from
// the placing one on, and adds a never-event for each reading whose
// operationRef, target or role is not the placing reading's.
func judgeRestartOperation(rec *s11RestartRecord, result *evidence.Scenario) {
	r := rec.Restart
	if r.PlacedAt == nil || r.placed < 0 || r.placed >= len(rec.Phases) {
		return
	}
	var events []string
	for _, p := range rec.Phases[r.placed:] {
		if !slices.Contains(r.OperationRefsSeen, p.OperationRef) {
			r.OperationRefsSeen = append(r.OperationRefsSeen, p.OperationRef)
		}
		if p.OperationRef != r.OperationRef {
			events = append(events, fmt.Sprintf("the backup's operationRef was %s, then %q in a reading in phase %s "+
				"at %s: a second operation", r.OperationRef, p.OperationRef, cmp.Or(p.Phase, noBootstrapPhase),
				p.At.Format(time.RFC3339Nano)))
		}
		if p.TargetInstance != r.TargetInstance || p.TargetRole != r.TargetRole {
			events = append(events, fmt.Sprintf("the backup's target was %s (%s), then %s (%s) in a reading in phase "+
				"%s at %s", r.TargetInstance, r.TargetRole, p.TargetInstance, p.TargetRole,
				cmp.Or(p.Phase, noBootstrapPhase), p.At.Format(time.RFC3339Nano)))
		}
	}
	result.Measurements["operationRefUnchanged"] = len(r.OperationRefsSeen) == 1
	if r.RestartedAt != nil {
		result.Measurements["inProgressAtRestart"] = r.InProgressAtRestart
	}
	result.NeverEvents = append(result.NeverEvents, events...)
}

// checkS11Attempts lists the variant's prefix with the run's own client and
// groups what it holds by attempt prefix of the backup
// (<prefix>/<clusterUID>/<database>/<backupUID>/attempt-<id>). The one
// operation uploads once, under one attempt prefix: a second attempt prefix
// that holds objects, or objects outside an attempt prefix, show a second
// writer and must never happen. The one attempt prefix must be the
// artifact's and hold its manifest.json.
func (h *haRun) checkS11Attempts(ctx context.Context, backup *databasev1alpha1.CubridBackup,
	rec *s11RestartRecord, result *evidence.Scenario) error {
	a, err := h.listS11Attempts(ctx, backup, &rec.s11Record)
	rec.Attempts = a
	if err != nil {
		return err
	}
	result.Measurements["attemptPrefixes"] = len(a.Prefixes)
	result.NeverEvents = append(result.NeverEvents, secondWriter(a, "")...)
	manifestKey := rec.Verification.ManifestKey
	if len(a.Prefixes) == 1 && manifestKey != "" &&
		(a.Prefixes[0].Prefix != path.Dir(manifestKey) || a.Manifests != 1) {
		return backupVerdict{fmt.Errorf("the one attempt prefix that holds objects, %s, is not the artifact's, "+
			"%s, with its one manifest.json (%d found)", a.Prefixes[0].Prefix, path.Dir(manifestKey), a.Manifests)}
	}
	return nil
}

// secondWriter returns the never-events of a listing: a second attempt
// prefix that holds objects, and objects outside an attempt prefix. when
// says when the listing was made, if not at the first listing.
func secondWriter(a *s11Attempts, when string) []string {
	var events []string
	suffix := ""
	if when != "" {
		suffix = " " + when
	}
	if len(a.Prefixes) > 1 {
		names := make([]string, 0, len(a.Prefixes))
		for _, p := range a.Prefixes {
			names = append(names, fmt.Sprintf("%s (%d objects, manifest %t)", p.Prefix, p.Objects, p.Manifest))
		}
		events = append(events, fmt.Sprintf("a second writer: %d attempt prefixes of the backup hold objects "+
			"under the variant's prefix%s: %s", len(a.Prefixes), suffix, strings.Join(names, ", ")))
	}
	if len(a.Outside) > 0 {
		events = append(events, "objects under the variant's prefix are outside an attempt prefix of the backup"+
			suffix+": "+strings.Join(a.Outside, ", "))
	}
	return events
}

// listS11Attempts lists the variant's prefix with the run's own client and
// groups what it holds by attempt prefix of backup. Only objects are seen: a
// duplicate that is still staging without an object, or an unfinished
// multipart upload, is not.
func (h *haRun) listS11Attempts(ctx context.Context, backup *databasev1alpha1.CubridBackup,
	rec *s11Record) (*s11Attempts, error) {
	a := &s11Attempts{Prefixes: []s11AttemptPrefix{}, Outside: []string{}}
	store, stop, err := h.openS11Store(ctx)
	if err != nil {
		return a, err
	}
	defer stop()
	keys, err := store.List(ctx, rec.Bucket, rec.Prefix+"/")
	if err != nil {
		return a, fmt.Errorf("list %s: %w", rec.Prefix, err)
	}
	a.Listed = true
	dir := path.Join(rec.Prefix, h.sourceOf(rec).clusterUID, databaseName, string(backup.UID))
	index := map[string]int{}
	for _, key := range keys {
		rest, below := strings.CutPrefix(key, dir+"/")
		segment, inside, _ := strings.Cut(rest, "/")
		if !below || !attemptSegment.MatchString(segment) || inside == "" {
			a.Outside = append(a.Outside, key)
			continue
		}
		prefix := dir + "/" + segment
		i, known := index[prefix]
		if !known {
			i = len(a.Prefixes)
			index[prefix] = i
			a.Prefixes = append(a.Prefixes, s11AttemptPrefix{Prefix: prefix})
		}
		a.Prefixes[i].Objects++
		if inside == instancemanager.ManifestObjectName {
			a.Prefixes[i].Manifest = true
			a.Manifests++
		}
	}
	return a, nil
}

// managerAfterRestart reads the installed manager at the end of the variant
// into after. It must be the Deployment LAB01 checked with one Pod: the
// replacement the run saw take the Lease, as managerBack read it, or the Pod
// LAB01 checked when no deletion was issued. Anything else blocks the
// variant: the manager changed other than by the restart.
func (h *haRun) managerAfterRestart(ctx context.Context, before managerIdentity, r *s11Restart,
	after **managerIdentity) error {
	read, err := h.observeManager(ctx, h.cfg.Config)
	*after = &read
	if err != nil {
		return err
	}
	want := before.Pods
	if r.RestartedAt != nil {
		want = nil
		if r.ManagerAfter != nil {
			want = []imageIdentity{*r.ManagerAfter}
		}
	}
	if read.DeploymentUID != before.DeploymentUID || len(want) == 0 || !slices.Equal(read.Pods, want) {
		return errors.New("the installed manager changed during S11 operator-restart other than by the restart")
	}
	return nil
}
