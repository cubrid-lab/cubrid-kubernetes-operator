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
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

const (
	s11IMRestartLane                = "lab-installed-ha-s11-instance-manager-restart"
	s11IMRestartVariant             = "instance-manager-restart"
	s11IMRestartRecordFile          = "S11/instance-manager-restart/record.json"
	s11IMRestartTimelineFile        = "S11/instance-manager-restart/timeline.jsonl"
	s11IMRestartSourceDataCheckFile = "S11/instance-manager-restart/source-data-check.json"
	s11IMRestartBackupName          = "s11-instance-manager-restart"
	// s11IMRestartClusterName is the standalone cluster whose Instance
	// Manager is killed during its backup's upload; s11IMRestartMember is its
	// one member's Pod.
	s11IMRestartClusterName = "s11-imrestart"
	s11IMRestartMember      = s11IMRestartClusterName + "-0"
)

// s11IMKill is the kill of the member's Instance Manager during the upload,
// the reading that placed it and the member's return.
type s11IMKill struct {
	memberKill
	// PlacedAt is the reading that placed the kill: the backup Running or
	// Uploading with an operation, after interruptedUploadSeen bytes passed
	// the limit since the request. PhaseAtPlacement and OperationRef are its
	// status's, AtKill the limit's counters it read, and SentBeforeKill
	// what passed from the request to them.
	PlacedAt         *time.Time             `json:"placedAt,omitempty"`
	PhaseAtPlacement string                 `json:"phaseAtPlacement,omitempty"`
	OperationRef     string                 `json:"operationRef,omitempty"`
	AtKill           *netfault.BandCounters `json:"atKill,omitempty"`
	SentBeforeKill   uint64                 `json:"sentBeforeKill,omitempty"`
	// OperationRefsSeen are the operationRefs the readings showed from the
	// placement on, in order, each once.
	OperationRefsSeen []string `json:"operationRefsSeen"`
	// ContainerAtRequest and RestartsAtRequest are the member's database
	// container as read right before the backup was requested. A container
	// that is another, or restarted, right before the kill restarted for
	// another reason during the backup (RestartedBeforeKill), and its
	// Instance Manager may already have failed the operation.
	ContainerAtRequest  string `json:"containerIDAtRequest,omitempty"`
	RestartsAtRequest   *int32 `json:"restartCountAtRequest,omitempty"`
	RestartedBeforeKill string `json:"restartedBeforeKill,omitempty"`
	// ReadyAgainAt is the first reading of the cluster Ready taken after the
	// restarted container was ready, and ReadyAgainWithin the time from the
	// kill to it; ReadyTransitionAt is the Ready condition's
	// lastTransitionTime in that reading, which says whether Ready went False
	// and back after the kill or never changed. ReadyWaiting is what did not
	// hold yet at the last reading before.
	ReadyAgainAt      *time.Time `json:"readyAgainAt,omitempty"`
	ReadyAgainWithin  string     `json:"readyAgainWithin,omitempty"`
	ReadyTransitionAt *time.Time `json:"readyTransitionAt,omitempty"`
	ReadyWaiting      string     `json:"readyWaiting,omitempty"`
	// placed is the index in the record's phases of the placing reading;
	// issued is when the kill was about to be issued, from which the ready
	// limit is counted.
	placed int
	issued time.Time
}

// confirmed reports a kill confirmed by the end or restart of the member's
// database container, with the kill's exit code.
func (k *s11IMKill) confirmed() bool {
	return k.EndedAt != nil && k.EndedOtherwise == ""
}

// s11IMRestartRecord is the evidence of S11 instance-manager-restart besides
// the timeline and the source's data check.
type s11IMRestartRecord struct {
	Backup             string `json:"backup"`
	BackupUID          string `json:"backupUID,omitempty"`
	Bucket             string `json:"bucket"`
	Prefix             string `json:"prefix"`
	ObjectStoreService string `json:"objectStoreService"`
	// PodSecurity are the run namespace's Pod Security labels as the run
	// read them before anything was created.
	PodSecurity map[string]string     `json:"podSecurity"`
	Cluster     *s11StandaloneCluster `json:"cluster,omitempty"`
	// ReadyLimit is BackupInstanceManagerRestartReadyLimit, the run's bound
	// on the cluster before the backup and after the kill.
	ReadyLimit  string       `json:"readyLimit"`
	Throttle    *s11Throttle `json:"throttle"`
	Kill        *s11IMKill   `json:"kill"`
	RequestedAt *time.Time   `json:"requestedAt,omitempty"`
	Phases      []s11Phase   `json:"phases"`
	// LastPhase is the phase of the last reading the wait took.
	// EndedAfterKill is set when the wait saw the backup end after the kill
	// was confirmed, and CompletedBeforeKill when it saw it Completed before
	// the kill was placed.
	LastPhase           string `json:"lastPhase,omitempty"`
	EndedAfterKill      bool   `json:"endedAfterKill"`
	CompletedBeforeKill bool   `json:"completedBeforeKill"`
	// FailedWithin runs from the return of the request to the reading that
	// saw Failed, and FailedAfterKill from the kill's issue to it, upper
	// bounds; StatusFailedWithin from the backup's creationTimestamp to its
	// status.completedAt.
	FailedWithin       string `json:"failedWithin,omitempty"`
	FailedAfterKill    string `json:"failedAfterKill,omitempty"`
	StatusFailedWithin string `json:"statusFailedWithin,omitempty"`
	// Failure is the Ready reason and message of the backup as it ended;
	// StoreError and StoreErrorKind are the store's answer when the backup
	// failed for its upload instead.
	Failure          string                                 `json:"failure,omitempty"`
	StoreError       string                                 `json:"storeError,omitempty"`
	StoreErrorKind   string                                 `json:"storeErrorKind,omitempty"`
	PhaseAtEnd       string                                 `json:"phaseAtEnd,omitempty"`
	ArtifactAtEnd    *databasev1alpha1.CubridBackupArtifact `json:"artifactAtEnd,omitempty"`
	Artifact         *databasev1alpha1.CubridBackupArtifact `json:"artifact,omitempty"`
	ArtifactsNotDone []s11ArtifactReading                   `json:"artifactsNotCompleted"`
	s11PrefixAtEnd
	// ManagerBefore is the installed manager as LAB01 read it at its end,
	// ManagerAfter as this variant read it at its end.
	ManagerBefore *managerIdentity `json:"managerBefore,omitempty"`
	ManagerAfter  *managerIdentity `json:"managerAfter,omitempty"`
	Source        *s12Source       `json:"source,omitempty"`
	Credentials   *s11Credentials  `json:"credentials,omitempty"`
	JudgedAt      *time.Time       `json:"judgedAt,omitempty"`
	Notes         []string         `json:"notes,omitempty"`
	// Cleanup says whether the limit was removed and the CubridBackup and
	// the cluster the run created were deleted; Residue is what the run
	// leaves.
	Cleanup string `json:"cleanup"`
	Residue string `json:"residue"`
}

// validateS11InstanceManagerRestart checks the inputs of S11
// instance-manager-restart, when it is selected; BackupBucket is checked by
// validateS11.
func (c HAConfig) validateS11InstanceManagerRestart() error {
	if !c.BackupInstanceManagerRestart {
		return nil
	}
	for _, other := range []struct {
		selected bool
		flag     string
	}{{c.Restore, flagRestore}, {c.BackupMissingDestination, flagMissingDestination},
		{c.BackupDeniedCredentials, flagDeniedCredentials},
		{c.BackupSignatureMismatch, flagSignatureMismatch},
		{c.BackupInterruptedUpload, flagInterruptedUpload},
		{c.BackupOperatorRestart, flagOperatorRestart}} {
		if other.selected {
			return fmt.Errorf("%s and %s select different lanes; a run takes one of them", flagInstanceManagerRestart,
				other.flag)
		}
	}
	if c.BackupInstanceManagerRestartReadyLimit <= 0 {
		return errors.New(flagInstanceManagerRestart + " needs a positive -backup-instance-manager-restart-ready-limit for " +
			"its cluster")
	}
	return nil
}

// planS11InstanceManagerRestart returns the lane and the requirements of a
// run that selects S11 instance-manager-restart after S11 complete, and
// names it and the run namespace's Pod Security in the summary's notes;
// otherwise lane and requirements as they are.
func planS11InstanceManagerRestart(cfg HAConfig, lane string, requirements []evidence.Requirement,
	summary *evidence.Summary) (string, []evidence.Requirement) {
	if !cfg.BackupInstanceManagerRestart {
		return lane, requirements
	}
	summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete and S11 " +
		"instance-manager-restart: a full backup to object storage, downloaded and checked by the run, and a " +
		"backup of a standalone cluster whose upload is slowed by a rate limit in its member's Pod and whose " +
		"Instance Manager is killed during it, which must end Failed for the restart; no other S00-S15 " +
		"qualification"
	summary.Notes = append(summary.Notes, "The run namespace enforces Pod Security privileged, with warn and "+
		"audit at restricted, so that the rate limit's ephemeral container is admitted")
	return s11IMRestartLane, append(requirements, evidence.Requirement{ID: s11ID, Variant: s11IMRestartVariant})
}

// runS11InstanceManagerRestart runs S11 instance-manager-restart after S11
// complete passed. The run creates a standalone cluster with the run's own
// object store Secret, waits until it is Ready, limits the rate of its
// member's TCP traffic to the object store as S11 interrupted-upload does,
// and requests a full backup of its database to BackupBucket. Once a reading
// shows the backup Running or Uploading with an operation and the limit has
// let interruptedUploadSeen bytes through since the request, it kills the
// member's Instance Manager as S12 instance-manager-restart does
// (imKillScript), confirms the kill by the container's end or restart, and
// waits until the container runs again and the cluster is Ready within the
// ready limit. The limit stays until the backup ends. The backup must end
// Failed with BackupFailed and the Instance Manager's restart reason within
// BackupLimit, never Completed and without status.artifact, under the
// operation of the placing reading; no manifest.json may be under its
// prefix at the end, the container must have restarted once, the source and
// the installed manager must be unchanged, and neither seed key may be in a
// status read, an Event or the logs checkS11Logs searches, the killed
// Instance Manager's among them. The run removes the limit, records what is
// left under the prefix as a partial artifact, never writes to or removes
// from the object store, and deletes the backup and the cluster it created.
// A backup that ends before the kill is placed, a kill that is not
// confirmed, the documented escalation RecordedOperationLost, a run that
// ends, or a check that cannot be made blocks it; what was seen to break
// stays a failure.
func (h *haRun) runS11InstanceManagerRestart(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s11ID+"/"+s11IMRestartVariant, &result)()
	result = evidence.Scenario{ID: s11ID, Variant: s11IMRestartVariant, Result: evidence.Fail,
		NeverEvents: []string{}}
	throttle := h.newS11Throttle(s11IMRestartMember)
	rec := s11IMRestartRecord{Backup: s11IMRestartBackupName, Bucket: h.cfg.BackupBucket,
		Prefix: h.rec.Seed.Prefix + "/" + s11IMRestartVariant, ObjectStoreService: h.cfg.ObjectStoreService,
		PodSecurity: map[string]string{},
		Cluster:     &s11StandaloneCluster{Name: s11IMRestartClusterName, Secret: seedSecretName},
		ReadyLimit:  h.cfg.BackupInstanceManagerRestartReadyLimit.String(), Throttle: &throttle,
		Kill: &s11IMKill{OperationRefsSeen: []string{}, placed: -1}, Phases: []s11Phase{},
		ArtifactsNotDone: []s11ArtifactReading{}, s11PrefixAtEnd: newS11PrefixAtEnd(),
		Credentials: &s11Credentials{FoundIn: []string{}},
		Notes: []string{"the rate limit acts in the network namespace of the standalone member's Pod, from an " +
			"ephemeral container with NET_ADMIN and NET_RAW (test/faults/netfault), as in S11 interrupted-upload; " +
			"the run namespace enforces Pod Security privileged for it, with warn and audit at restricted",
			"the kill is SIGKILL to the Instance Manager, the child of the database container's entrypoint shell " +
				"(build/instance-manager/entrypoint.sh), as in S12 instance-manager-restart; the shell exits with " +
				"its status, and the kubelet starts the container again on the same Pod and volume",
			"the kill is placed while the upload is under way: after interruptedUploadSeen bytes passed the " +
				"limit with the backup Running or Uploading with an operation; a kill during backupdb itself is " +
				"not covered",
			"the limit stays until the backup ended, so the store is reached only slowly while the Instance " +
				"Manager restarts; nothing cuts the upload",
			"the member's backup staging directory after the restart is not checked",
			"the run never writes to or removes from the object store; with the seed Secret's keys it lists the " +
				"backup bucket under the prefix at the end",
			"the killed Instance Manager's log is the previous instance of the member's database container, read " +
				"through pods/log with previous=true",
			logsNote(h.logLimit())},
		Cleanup: "nothing to delete: the run created no CubridCluster or CubridBackup",
		Residue: "nothing: the run wrote nothing to the object store" + stagingNotChecked}
	var made s11InterruptedMade
	defer func() {
		removed := h.removeInterruptedFaults(ctx, &made, rec.Throttle)
		deleted := h.deleteInterrupted(ctx, made, rec.Throttle, &rec.Cleanup, &rec.Residue)
		cleaned := removed && deleted
		result.CleanupSucceeded = &cleaned
		if made.throttle != nil {
			maps.Copy(result.Measurements, prefixed("rateLimit.", made.throttle.Measurements()))
		}
		// A pass without its evidence is a failure; a blocked or failed S11
		// instance-manager-restart keeps its result, with the evidence that
		// could not be written.
		if err := h.writeJSON(s11IMRestartRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S11 instance-manager-restart evidence: "+
				err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S11/"+s11IMRestartVariant+"/")
		})
	}()
	result.Limits = map[string]string{backupLimitName: h.cfg.BackupLimit.String(),
		"readyLimit": h.cfg.BackupInstanceManagerRestartReadyLimit.String()}
	result.Measurements = map[string]any{failedWithinKey: evidence.Unknown, "failedAfterKill": evidence.Unknown,
		"uploadBytesBeforeKill": evidence.Unknown, "memberBackWithin": evidence.Unknown,
		"readyAgainWithin": evidence.Unknown, "containerRestarts": evidence.Unknown,
		"operationRefUnchanged": evidence.Unknown}
	err := h.s11IMRestartSteps(ctx, &result, &rec, &made)
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
		errors.As(err, new(dataViolation)):
		result.Result, result.Reason = evidence.Fail, reason
	case never != "":
		result.Result, result.Reason = evidence.Fail, reason
	case err != nil && ctx.Err() != nil:
		result.Result, result.Reason = evidence.Blocked, "the run ended before S11 instance-manager-restart was "+
			"judged: "+reason
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, reason
	default:
		k := rec.Kill
		result.Result = evidence.Pass
		result.Reason = fmt.Sprintf("the upload of %s's backup to %s was seen under way through the rate limit "+
			"(%d bytes) while the backup was %s with operation %s, and the member's Instance Manager was then "+
			"killed, confirmed by its database container's end with exit code %d; the container ran again on "+
			"the same Pod %s after the kill, and a reading of the cluster after the restarted container was "+
			"ready showed Ready %s after the kill, within the ready limit counted from it, with the container "+
			"restarted once and not before the kill; the backup ended Failed within backup_limit with %s and the Instance "+
			"Manager's restart reason, under the same operation in every reading from the placement on, never "+
			"Completed and without status.artifact, also when read again at the end; no manifest.json was under "+
			"its prefix at the end; the limit was seen removed; the source and the installed manager were "+
			"unchanged; no seed key was in the statuses read, the Events, the installed Operator's manager log or "+
			"the member's log, whole, the killed Instance Manager's previous instance included",
			s11IMRestartClusterName, rec.Bucket, k.SentBeforeKill, k.PhaseAtPlacement, k.OperationRef,
			sigkillExitCode, k.BackWithin, k.ReadyAgainWithin, s11MissingReason)
	}
	judged := time.Now().UTC()
	rec.JudgedAt = &judged
	return result
}

// s11IMRestartSteps runs the steps of S11 instance-manager-restart in
// order. Before the request, the first error ends them. After it, the
// backup is judged, the limit is removed, and the checks at the end are all
// made; their errors are joined.
func (h *haRun) s11IMRestartSteps(ctx context.Context, result *evidence.Scenario, rec *s11IMRestartRecord,
	made *s11InterruptedMade) error {
	started := time.Now().UTC()
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S11 instance-manager-restart could not tell whether a credential is in a status "+
			"or an Event", err)
	}
	before := h.rec.OperatorAfter
	rec.ManagerBefore = &before
	if err := h.throttledStandalone(ctx, "S11 instance-manager-restart",
		h.cfg.BackupInstanceManagerRestartReadyLimit, &rec.PodSecurity, rec.Cluster, rec.Credentials, rec.Throttle,
		made); err != nil {
		return err
	}
	err := h.imRestartUpload(ctx, result, rec, made)
	if rec.RequestedAt == nil {
		return err
	}
	errs := []error{err, h.throttleAtEnd(ctx, made, rec.Throttle)}
	errs = append(errs, h.imRestartPrefixAtEnd(ctx, rec, result))
	errs = append(errs, h.imRestartBackupAtEnd(ctx, made.backup, rec, result))
	k := rec.Kill
	if k.confirmed() {
		errs = append(errs, h.targetRestartsAtEnd(ctx, &k.memberKill, result))
	}
	if err := h.checkS11Credentials(ctx, made.backup, nil, rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: %w", err))
	}
	if err := h.standaloneAtEnd(ctx, made.cluster, rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: %w", err))
	}
	if err := h.checkS11Logs(ctx, s11IMRestartClusterName, true, lastTarget(rec.Phases),
		requestedOnServer(made.backup, *rec.RequestedAt), started,
		rec.Credentials, h.rec.OperatorAfter.Pods); err != nil {
		errs = append(errs, fmt.Errorf("search the logs: %w", err))
	}
	if k.confirmed() {
		errs = append(errs, killedLogRead(rec.Credentials))
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	errs = append(errs, h.checkSourceS12(ctx, rec.Source, s11IMRestartSourceDataCheckFile))
	errs = append(errs, h.sameManager(ctx, "S11 instance-manager-restart", rec.ManagerBefore, &rec.ManagerAfter))
	return errors.Join(errs...)
}

// killedLogRead requires that the member's log search read the previous
// instance of its database container, whose Instance Manager was killed.
func killedLogRead(creds *s11Credentials) error {
	for _, l := range creds.Logs {
		if l.Log == instanceManagerLog && l.Pod == s11IMRestartMember && l.PreviousRead {
			return nil
		}
	}
	return fmt.Errorf("the log of the killed Instance Manager, the previous instance of %s's database "+
		"container, was not read", s11IMRestartMember)
}

// imRestartUpload installs and confirms the rate limit, requests the backup
// and waits for it, killing the member's Instance Manager once the upload is
// seen under way, and judges the backup.
func (h *haRun) imRestartUpload(ctx context.Context, result *evidence.Scenario, rec *s11IMRestartRecord,
	made *s11InterruptedMade) error {
	if err := made.throttle.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare the rate limit: %w", err)
	}
	if err := confirmThrottle(ctx, made.throttle, rec.Throttle); err != nil {
		return err
	}
	if err := h.containerAtRequest(ctx, rec.Kill); err != nil {
		return err
	}
	backup, err := h.createBackup(ctx, s11IMRestartBackupName, s11IMRestartClusterName, rec.Bucket, rec.Prefix)
	if err != nil {
		return err
	}
	requested := time.Now().UTC()
	made.backup = backup
	rec.BackupUID, rec.RequestedAt = string(backup.UID), &requested
	backup, err = h.waitBackupWith(ctx, backup, &rec.Phases, rec.Credentials,
		func(wctx context.Context, read *databasev1alpha1.CubridBackup) error {
			return h.imRestartReading(wctx, read, rec, made, result)
		})
	return judgeIMRestartBackup(backup, err, requested, rec, result)
}

// imRestartReading acts on one status read of the backup while the run
// waits for it. An artifact on a backup that is not Completed is recorded.
// Before the kill, a backup that is Running or Uploading with an operation
// has the limit's counters read, and once interruptedUploadSeen bytes
// passed since the request the kill is placed (killDuringUpload). After it,
// readings are only recorded.
func (h *haRun) imRestartReading(ctx context.Context, backup *databasev1alpha1.CubridBackup,
	rec *s11IMRestartRecord, made *s11InterruptedMade, result *evidence.Scenario) error {
	s, f, k := backup.Status, rec.Throttle, rec.Kill
	rec.LastPhase = string(s.Phase)
	if s.Artifact != nil && s.Phase != databasev1alpha1.BackupPhaseCompleted {
		rec.ArtifactsNotDone = append(rec.ArtifactsNotDone, s11ArtifactReading{At: time.Now().UTC(),
			Phase: string(s.Phase), URI: s.Artifact.URI})
	}
	if s.TargetInstance != "" && s.TargetInstance != f.Target {
		return faultStep{fmt.Errorf("the backup's target is %s, not %s, the Pod the limit acts in",
			s.TargetInstance, f.Target)}
	}
	if k.PlacedAt != nil || !inProgress(s.Phase) || s.OperationRef == "" {
		return nil
	}
	counters, seen, err := made.uploadUnderWay(ctx, f, s.Phase)
	if err != nil || !seen {
		return err
	}
	made.errorsInARow = 0
	at := time.Now().UTC()
	k.PlacedAt, k.placed, k.PhaseAtPlacement, k.OperationRef = &at, len(rec.Phases)-1, string(s.Phase),
		s.OperationRef
	k.AtKill, k.SentBeforeKill, f.Waiting = &counters, f.sentSince(counters), ""
	f.observeRate(counters)
	result.Measurements["uploadBytesBeforeKill"] = k.SentBeforeKill
	return h.killDuringUpload(ctx, made.cluster, rec, result)
}

// readyLate fails a reading that found the cluster Ready, at readyAt, more
// than limit after the kill at issued: one found back only after the limit
// is as late as one that was not.
func readyLate(readyAt *time.Time, issued time.Time, limit time.Duration) error {
	if readyAt == nil || readyAt.Sub(issued) <= limit {
		return nil
	}
	return fmt.Errorf("time limit exceeded: the cluster read Ready %s after the kill, later than the ready "+
		"limit %s", readyAt.Sub(issued).Round(time.Millisecond), limit)
}

// killDuringUpload kills the member's Instance Manager (instanceManagerKill)
// once the Pod and its database container are recorded right before
// (beforeIMKill), and waits, for at most the ready limit from the kill, until
// the container runs again on the same Pod (memberBack) and a reading of the
// cluster after the restarted container was ready shows Ready
// (memberBackReady). faults.Run counts its Outcome bound from the kill's
// confirmation, so the bound from the kill is enforced in the check. An error
// it returns ends the wait for the backup: a container that restarted before
// the kill, a kill that is not confirmed, or a Pod that was replaced, is a
// faultStep and blocks the variant; a member not back within the ready limit
// fails it.
func (h *haRun) killDuringUpload(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	rec *s11IMRestartRecord, result *evidence.Scenario) error {
	k := rec.Kill
	limit := h.cfg.BackupInstanceManagerRestartReadyLimit
	outcome := faults.Run(ctx, faults.Scenario{
		Fault: instanceManagerKill{h: h, k: &k.memberKill,
			name: "kill the Instance Manager of the standalone member with SIGKILL"},
		// The Outcome bound runs from the confirmation, which follows the
		// kill: at twice the limit it is only a backstop, and the check's
		// bound from the kill ends the wait first.
		Limits: faults.Limits{Confirm: min(2*time.Minute, limit), Outcome: 2 * limit, Cleanup: time.Minute,
			Poll: h.cfg.Poll},
		Before: func(ctx context.Context) error {
			err := h.beforeIMKill(ctx, cluster, k)
			k.issued = time.Now()
			return err
		},
		Check: func(ctx context.Context) (bool, error) {
			done, err := h.memberBackReady(ctx, cluster, rec)
			if ctx.Err() != nil {
				// The step's limit, or the run's: not a broken expectation.
				return false, nil
			}
			if err == nil && done {
				if late := readyLate(k.ReadyAgainAt, k.issued, limit); late != nil {
					return false, late
				}
			}
			if err == nil && !done && time.Since(k.issued) > limit {
				return false, fmt.Errorf("time limit exceeded: the expected behavior was not observed within %s "+
					"of the kill", limit)
			}
			return done, err
		},
	})
	if !outcome.FaultIssuedAt.IsZero() {
		at := outcome.FaultIssuedAt
		k.KilledAt = &at
	}
	if err := h.writeTimeline(s11IMRestartTimelineFile, outcome.Timeline); err != nil {
		return err
	}
	switch {
	case k.RestartedBeforeKill != "":
		return faultStep{fmt.Errorf("the member's container restarted before the kill, so it cannot be told "+
			"whether the kill interrupted the upload; nothing was killed: %s", k.RestartedBeforeKill)}
	case k.EndedOtherwise != "":
		return faultStep{fmt.Errorf("the kill was not confirmed: the database container of %s ended for another "+
			"reason than the kill: %s", k.Pod, k.EndedOtherwise)}
	case k.PodReplaced:
		return faultStep{fmt.Errorf("the Pod %s was replaced, not only its container restarted: %s", k.Pod,
			outcome.Reason)}
	case !outcome.FaultConfirmed:
		return faultStep{fmt.Errorf("the kill of %s's Instance Manager was not confirmed: %s", k.Pod,
			outcome.Reason)}
	case outcome.Result == evidence.Fail && k.readsAfterKill == 0 && k.lastReadErr != nil:
		return fmt.Errorf("the Pod %s could not be read after the kill was confirmed, so whether its container ran "+
			"again is not known: %w", k.Pod, k.lastReadErr)
	case outcome.Result == evidence.Fail && k.BackAt == nil:
		return backupVerdict{fmt.Errorf("the container of %s did not run again within the ready limit %s after "+
			"its Instance Manager was killed: %s", k.Pod, limit, outcome.Reason)}
	case outcome.Result == evidence.Fail:
		return backupVerdict{fmt.Errorf("%s was not Ready again within the ready limit %s after its Instance "+
			"Manager was killed (%s): %s", cluster.Name, limit, cmp.Or(k.ReadyWaiting, "no reading was taken"),
			outcome.Reason)}
	case outcome.Result != evidence.Pass:
		return errors.New(outcome.Reason)
	}
	k.BackWithin = k.BackAt.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String()
	k.ReadyAgainWithin = k.ReadyAgainAt.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String()
	result.Measurements["memberBackWithin"] = k.BackWithin
	result.Measurements["readyAgainWithin"] = k.ReadyAgainWithin
	return nil
}

// beforeIMKill reads the member's Pod right before the kill, requires the
// cluster's StatefulSet to control it, and records it and its running
// database container; the kill is issued only against them. A container
// that is not the one read at the request, or restarted since, restarted
// for another reason during the backup: the restart reason the backup would
// end with could then come from that restart, not the kill, so nothing is
// killed (RestartedBeforeKill).
func (h *haRun) beforeIMKill(ctx context.Context, cluster *databasev1alpha1.CubridCluster, k *s11IMKill) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: s11IMRestartMember},
		pod); err != nil {
		return fmt.Errorf("read the member %s: %w", s11IMRestartMember, err)
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != kindStatefulSet || owner.Name != cluster.Name {
		return fmt.Errorf("the Pod %s is not controlled by the StatefulSet of %s", pod.Name, cluster.Name)
	}
	if err := k.recordBefore(pod); err != nil {
		return err
	}
	if k.ContainerBefore != k.ContainerAtRequest || k.RestartsAtRequest == nil ||
		*k.RestartsBefore != *k.RestartsAtRequest {
		k.RestartedBeforeKill = fmt.Sprintf("the database container of %s was %s with restart count %s at the "+
			"request and is %s with restart count %d right before the kill", pod.Name, k.ContainerAtRequest,
			countText(k.RestartsAtRequest), k.ContainerBefore, *k.RestartsBefore)
		return errors.New("the member's container restarted before the kill: " + k.RestartedBeforeKill)
	}
	return nil
}

// countText is a restart count, or "unknown".
func countText(n *int32) string {
	if n == nil {
		return "unknown"
	}
	return fmt.Sprint(*n)
}

// containerAtRequest reads the member's Pod right before the backup is
// requested and records its database container's ID and restart count.
func (h *haRun) containerAtRequest(ctx context.Context, k *s11IMKill) error {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: s11IMRestartMember},
		pod); err != nil {
		return fmt.Errorf("read the member %s before the request: %w", s11IMRestartMember, err)
	}
	status := databaseContainerStatus(pod)
	if status == nil || status.ContainerID == "" {
		return fmt.Errorf("the member %s reports no database container before the request", s11IMRestartMember)
	}
	restarts := status.RestartCount
	k.ContainerAtRequest, k.RestartsAtRequest = status.ContainerID, &restarts
	return nil
}

// memberBackReady reports whether the killed member's container runs again
// (memberBack, once) and then a reading of the cluster after the restarted
// container was ready shows Ready (standaloneReady). That Ready may never
// have gone False after the kill; the condition's lastTransitionTime is
// recorded as evidence of which. Every status of the cluster read is searched for a
// credential. What does not hold yet is recorded in ReadyWaiting.
func (h *haRun) memberBackReady(ctx context.Context, created *databasev1alpha1.CubridCluster,
	rec *s11IMRestartRecord) (bool, error) {
	k := rec.Kill
	if k.BackAt == nil {
		back, err := h.memberBack(ctx, &k.memberKill)
		if err != nil || !back {
			k.ReadyWaiting = "the database container of " + k.Pod + " does not run again yet"
			return false, err
		}
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: k.Pod}, pod); err != nil {
		k.ReadyWaiting = "the Pod" + unread + err.Error()
		return false, nil
	}
	if string(pod.UID) != k.PodUID {
		k.PodReplaced = true
		return false, fmt.Errorf("the Pod %s was replaced", k.Pod)
	}
	status := databaseContainerStatus(pod)
	if status == nil || status.ContainerID != k.ContainerAfter || !status.Ready {
		k.ReadyWaiting = "the restarted database container of " + k.Pod + " is not ready"
		return false, nil
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), cluster); err != nil {
		k.ReadyWaiting = "the cluster" + unread + err.Error()
		return false, nil
	}
	if cluster.UID != created.UID {
		return false, fmt.Errorf("the cluster %s was replaced", created.Name)
	}
	rec.Credentials.StatusesChecked++
	if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the status read of "+created.Name,
		cluster.Status); err != nil {
		return false, err
	}
	unmet, err := standaloneReady(cluster)
	if err != nil || unmet != "" {
		k.ReadyWaiting = unmet
		return false, err
	}
	at := time.Now().UTC()
	k.ReadyAgainAt, k.ReadyWaiting = &at, ""
	if c := meta.FindStatusCondition(cluster.Status.Conditions, clusterReadyCondition); c != nil {
		transition := c.LastTransitionTime.UTC()
		k.ReadyTransitionAt = &transition
	}
	return true, nil
}

// judgeIMRestartBackup judges the backup as the wait left it.
//
// The Instance Manager is a child of the database container's entrypoint
// shell, which exits with the manager's status once it is gone
// (build/instance-manager/entrypoint.sh): the SIGKILL ends the container,
// and the kubelet starts it again on the same Pod and volume. The manager
// keeps its operation records on that volume (IM_OPERATIONS_DIR,
// operationsDir in internal/controller/cubridcluster_controller.go) and, at
// its start, fails every operation that had not ended with
// s12IMRestartedReason (reconcileOnStart in
// internal/instancemanager/opstore.go). The Operator polls the recorded
// operation by status.operationRef (pollBackup in
// internal/controller/cubridbackup_controller.go), keeps the phase while the
// manager cannot be reached, and maps Failed onto the backup in failedBackup:
// phase Failed, Ready reason BackupFailed, the operation's reason as
// message. A backup that is Completed or Failed is never reconciled again,
// so nothing starts a second operation. The manifest is uploaded last
// (UploadBackup in internal/instancemanager/upload.go), so an upload killed
// before it leaves none.
//
// So once the kill was confirmed: Completed, a status.artifact on any
// reading, and another operationRef from the placing reading on must never
// happen; Failed must be BackupFailed with s12IMRestartedReason, and any
// other failure fails the variant, with the store's answer when it is an
// upload failure. RecordedOperationLost, the documented escalation of an
// unknown outcome, blocks it. A backup that ended before the kill was
// placed did not show what the kill does and blocks it, and so does an
// error of the kill's step.
func judgeIMRestartBackup(backup *databasev1alpha1.CubridBackup, waitErr error, requested time.Time,
	rec *s11IMRestartRecord, result *evidence.Scenario) error {
	s, k := backup.Status, rec.Kill
	rec.Artifact = s.Artifact
	ended := s.Phase == databasev1alpha1.BackupPhaseCompleted || s.Phase == databasev1alpha1.BackupPhaseFailed
	if ended {
		rec.Failure = readyReason(backup)
		rec.EndedAfterKill = waitErr == nil && k.confirmed()
	}
	events := judgeIMRestartOperation(rec, result)
	if s.Phase == databasev1alpha1.BackupPhaseCompleted && k.confirmed() {
		events = append(events, "the backup of "+s11IMRestartClusterName+" ended Completed after its Instance "+
			"Manager was killed during the upload")
	}
	for _, a := range rec.ArtifactsNotDone {
		event := "status.artifact names " + a.URI + " for a backup of " + s11IMRestartClusterName + " in phase " +
			a.Phase
		if !slices.Contains(events, event) {
			events = append(events, event)
		}
	}
	if s.Phase == databasev1alpha1.BackupPhaseCompleted && k.confirmed() && s.Artifact != nil {
		events = append(events, "status.artifact names "+s.Artifact.URI+" for a backup of "+
			s11IMRestartClusterName+" whose Instance Manager was killed during the upload")
	}
	if len(events) > 0 {
		result.NeverEvents = append(result.NeverEvents, events...)
		errs := make([]error, 0, len(events))
		for _, event := range events {
			errs = append(errs, backupVerdict{errors.New(event)})
		}
		return errors.Join(errs...)
	}
	if waitErr != nil {
		if errors.As(waitErr, new(backupVerdict)) || errors.As(waitErr, new(faultStep)) {
			return waitErr
		}
		return fmt.Errorf("read the CubridBackup: %w", waitErr)
	}
	switch {
	case s.Phase == databasev1alpha1.BackupPhaseCompleted:
		rec.CompletedBeforeKill = true
		return fmt.Errorf("the backup ended Completed before the kill could be placed, with %d bytes through the "+
			"rate limit at the last reading: the upload finished first, S11 instance-manager-restart is "+
			"inconclusive, and the artifact is a complete backup", rec.Throttle.lastSent())
	case k.PlacedAt == nil:
		return fmt.Errorf("the backup ended Failed before its upload was seen under way with an operation (%s): %s",
			cmp.Or(rec.Throttle.Waiting, "no reading of the rate limit was taken"), readyReason(backup))
	case !k.confirmed():
		return fmt.Errorf("the backup ended Failed, but the kill was not confirmed: %s", readyReason(backup))
	}
	c := meta.FindStatusCondition(s.Conditions, backupReady)
	switch {
	case c != nil && c.Reason == s11OperationLostReason:
		return fmt.Errorf("the backup reached the documented escalation %s after its Instance Manager was "+
			"killed, an unknown outcome under which a backup may still run: %s", s11OperationLostReason,
			rec.Failure)
	case c == nil || c.Reason != s11MissingReason || c.Message != s12IMRestartedReason:
		failure := rec.Failure
		if c != nil && strings.HasPrefix(c.Message, uploadFailed) {
			rec.StoreError = strings.TrimPrefix(c.Message, uploadFailed)
			rec.StoreErrorKind = storeErrorKind(rec.StoreError)
			failure += fmt.Sprintf(" (an upload failure, store error kind %s: %s)", rec.StoreErrorKind,
				rec.StoreError)
		}
		return backupVerdict{fmt.Errorf("the backup ended Failed after its Instance Manager was killed, but not "+
			"for the restart (%s %q): %s", s11MissingReason, s12IMRestartedReason, failure)}
	}
	failed := rec.Phases[len(rec.Phases)-1].At
	rec.FailedWithin = failed.Sub(requested).Round(time.Millisecond).String()
	rec.FailedAfterKill = failed.Sub(*k.KilledAt).Round(time.Millisecond).String()
	result.Measurements[failedWithinKey] = rec.FailedWithin
	result.Measurements["failedAfterKill"] = rec.FailedAfterKill
	if done := s.CompletedAt; done != nil {
		rec.StatusFailedWithin = done.Sub(backup.CreationTimestamp.Time).String()
	}
	return nil
}

// judgeIMRestartOperation records the operationRefs the readings showed
// from the placing one on, and returns a never-event for each reading whose
// operationRef is not the placing reading's: a second operation.
func judgeIMRestartOperation(rec *s11IMRestartRecord, result *evidence.Scenario) []string {
	k := rec.Kill
	if k.PlacedAt == nil || k.placed < 0 || k.placed >= len(rec.Phases) {
		return nil
	}
	var events []string
	for _, p := range rec.Phases[k.placed:] {
		if !slices.Contains(k.OperationRefsSeen, p.OperationRef) {
			k.OperationRefsSeen = append(k.OperationRefsSeen, p.OperationRef)
		}
		if p.OperationRef != k.OperationRef {
			events = append(events, fmt.Sprintf("the backup's operationRef was %s, then %q in a reading in phase %s "+
				"at %s: a second operation", k.OperationRef, p.OperationRef, cmp.Or(p.Phase, noBootstrapPhase),
				p.At.Format(time.RFC3339Nano)))
		}
	}
	result.Measurements["operationRefUnchanged"] = len(k.OperationRefsSeen) == 1
	return events
}

// imRestartPrefixAtEnd lists the backup bucket under the prefix and judges
// a manifest.json there (prefixAtEnd): it must never happen once the wait
// saw the backup end after the kill.
func (h *haRun) imRestartPrefixAtEnd(ctx context.Context, rec *s11IMRestartRecord,
	result *evidence.Scenario) error {
	return h.prefixAtEnd(ctx, rec.Bucket, rec.Prefix, &rec.s11PrefixAtEnd, &rec.Residue, s11FaultEnd{
		completedBefore: rec.CompletedBeforeKill, endedAfter: rec.EndedAfterKill, lastPhase: rec.LastPhase,
		fault: "its Instance Manager was killed during the upload", confirmed: "a confirmed kill",
		variant: "S11 instance-manager-restart"}, result)
}

// imRestartBackupAtEnd reads the backup once more, after the limit was
// removed and the prefix was listed, so that a backup driven on after the
// wait is not missed. When the wait saw it end after the kill, Completed, a
// status.artifact or another operationRef then must never happen; otherwise
// either of the first two is recorded and blocks the variant, unless the
// backup completed before the kill.
func (h *haRun) imRestartBackupAtEnd(ctx context.Context, created *databasev1alpha1.CubridBackup,
	rec *s11IMRestartRecord, result *evidence.Scenario) error {
	s, err := h.readBackupAtEnd(ctx, created)
	if err != nil {
		return err
	}
	rec.PhaseAtEnd, rec.ArtifactAtEnd = string(s.Phase), s.Artifact
	completed := s.Phase == databasev1alpha1.BackupPhaseCompleted
	if rec.CompletedBeforeKill || (!rec.EndedAfterKill && !completed && s.Artifact == nil) {
		return nil
	}
	if !rec.EndedAfterKill {
		return fmt.Errorf("the CubridBackup read at the end is %s with status.artifact %t, and the wait did not see "+
			"it end after a confirmed kill: recorded, and S11 instance-manager-restart cannot be judged", s.Phase,
			s.Artifact != nil)
	}
	var events []string
	if completed && rec.LastPhase != string(databasev1alpha1.BackupPhaseCompleted) {
		events = append(events, "the backup of "+s11IMRestartClusterName+", seen "+rec.LastPhase+" after its "+
			"Instance Manager was killed, is Completed when read again at the end")
	}
	if s.Artifact != nil && rec.LastPhase != string(databasev1alpha1.BackupPhaseCompleted) {
		events = append(events, "status.artifact names "+s.Artifact.URI+" for the backup of "+
			s11IMRestartClusterName+" read again at the end, after its Instance Manager was killed")
	}
	if s.OperationRef != rec.Kill.OperationRef {
		events = append(events, fmt.Sprintf("the backup's operationRef was %s, then %q when read again at the "+
			"end: a second operation", rec.Kill.OperationRef, s.OperationRef))
	}
	result.NeverEvents = append(result.NeverEvents, events...)
	errs := make([]error, 0, len(events))
	for _, event := range events {
		errs = append(errs, backupVerdict{errors.New(event)})
	}
	return errors.Join(errs...)
}
