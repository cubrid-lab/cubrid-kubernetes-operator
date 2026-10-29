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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	s11OverlappingLane                = "lab-installed-ha-s11-overlapping"
	s11OverlappingVariant             = "overlapping"
	s11OverlappingRecordFile          = "S11/overlapping/record.json"
	s11OverlappingSourceDataCheckFile = "S11/overlapping/source-data-check.json"
	// s11OverlappingClusterName is the standalone cluster both backups are
	// of, and s11OverlappingMember its one member's Pod.
	s11OverlappingClusterName = "s11-overlapping"
	s11OverlappingMember      = s11OverlappingClusterName + "-0"
	// The Ready reasons of a backup whose start its recorded target refused
	// with 409 (instanceManagerFailureReason in
	// internal/controller/imclient.go): another operation holds the
	// database, a running operation of another request owns the staging
	// destination, the target holds an operation under the backup's own key
	// for another request body, or a conflict without a code.
	s11OperationInProgressReason = "InstanceManagerOperationInProgress"
	s11StagingInUseReason        = "InstanceManagerStagingInUse"
	s11RequestConflictReason     = "InstanceManagerRequestConflict"
	s11ConflictReason            = "InstanceManagerConflict"
	// s11TargetNotStartedReason ends a backup whose start its recorded target
	// did not accept within backupTargetWait (reasonRecordedTargetNotStarted
	// in internal/controller/cubridbackup_controller.go).
	s11TargetNotStartedReason = "RecordedTargetNotStarted"
)

// s11OverlappingBackups are the two backups of S11 overlapping, in the order
// they are requested: each one's name, the last segment of its prefix, the
// letter its measurements start with, and the file its manifest is copied
// to.
var s11OverlappingBackups = [2]struct{ name, prefix, letter, manifest string }{
	{"s11-overlapping-a", "overlapping-a", "a", "S11/overlapping/manifest-a.json"},
	{"s11-overlapping-b", "overlapping-b", "b", "S11/overlapping/manifest-b.json"},
}

// s11Hold is a reading that showed a backup waiting on its recorded target
// after the target refused its start as a conflict, with what the other
// backup's reading in the same round showed.
type s11Hold struct {
	At                time.Time `json:"at"`
	Reason            string    `json:"reason"`
	Message           string    `json:"message"`
	TargetInstance    string    `json:"targetInstance,omitempty"`
	OtherPhase        string    `json:"otherPhase"`
	OtherOperationRef string    `json:"otherOperationRef,omitempty"`
	OtherTarget       string    `json:"otherTargetInstance,omitempty"`
}

// s11OverlapBackup is the evidence of one of the two backups.
type s11OverlapBackup struct {
	s11Record
	// Attempts is the listing of the backup's prefix after its artifact was
	// verified, AttemptsAtEnd the same listing made again at the end.
	Attempts      *s11Attempts `json:"attempts,omitempty"`
	AttemptsAtEnd *s11Attempts `json:"attemptsAtEnd,omitempty"`
	// Holds are the readings that showed the backup held after a conflict,
	// one for each change of reason, target or the other backup's phase; the
	// message, which names each request, is the first reading's.
	Holds []s11Hold `json:"holds"`
	// OperationRefsSeen and TargetsSeen are the operationRefs and the
	// targets, with their roles, the readings showed, in order, each once.
	OperationRefsSeen []string `json:"operationRefsSeen"`
	TargetsSeen       []string `json:"targetsSeen"`
	// StatusStartedAt is status.startedAt, which the Operator sets once the
	// target accepted the start. ManifestCreatedAt and ManifestOperationID
	// are the manifest's createdAt, which the Instance Manager sets right
	// before its upload, and operationID.
	StatusStartedAt     *time.Time `json:"statusStartedAt,omitempty"`
	ManifestCreatedAt   *time.Time `json:"manifestCreatedAt,omitempty"`
	ManifestOperationID string     `json:"manifestOperationID,omitempty"`
	// Failure is the Ready reason and message of a backup that ended Failed.
	Failure string `json:"failure,omitempty"`
	// OperationRecord is the member's record of the backup's operation.
	OperationRecord *s11OperationRecord `json:"operationRecord,omitempty"`
	// Cleanup says whether the run deleted the backup.
	Cleanup string `json:"cleanup"`
	// letter starts the backup's measurements, and manifestFile names the
	// copy of its manifest; manifest is manifest.json as stored, once read.
	letter, manifestFile string
	manifest             []byte
}

// s11OperationRecord is what the run read of the Instance Manager's record
// of a backup's operation on its target (IM_OPERATIONS_DIR/<id>.json): its
// state, and its createdAt and updatedAt, both by the member's clock. For an
// operation that ended, updatedAt is when its end was written, after its
// upload ended and its staging was removed (runBackup in
// internal/instancemanager/server.go).
type s11OperationRecord struct {
	Pod         string     `json:"pod"`
	OperationID string     `json:"operationID"`
	State       string     `json:"state,omitempty"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
	ReadError   string     `json:"readError,omitempty"`
}

// s11Overlap is what the readings showed of the two backups together.
type s11Overlap struct {
	// Started are the backups in the order a reading first showed each one
	// with an operation. It is evidence; it does not order the pair.
	Started []string `json:"started"`
	// First and Second are the backups in the order they ran on the member:
	// the one held after a conflict is the second; when neither or both were,
	// the one with the earlier status.startedAt is the first.
	First  string `json:"first,omitempty"`
	Second string `json:"second,omitempty"`
	// FirstEndedAt is the updatedAt of the first's operation record, written
	// when it ended, and SecondCreatedAt the createdAt of the second's, both
	// by the member's clock: the rule the variant judges.
	FirstEndedAt    *time.Time `json:"firstEndedAt,omitempty"`
	SecondCreatedAt *time.Time `json:"secondCreatedAt,omitempty"`
	// HeldWithConflict is set when a reading showed a backup held because
	// another operation held its target's database.
	HeldWithConflict bool `json:"heldWithConflict"`
	// BothInProgressFrom and BothInProgressTo are the first and the last
	// round of readings that showed both Running or Uploading with an
	// operation: evidence of the Operator's polling, not judged.
	BothInProgressFrom *time.Time `json:"bothInProgressFrom,omitempty"`
	BothInProgressTo   *time.Time `json:"bothInProgressTo,omitempty"`
	// As evidence only: SecondStartedAt is status.startedAt of
	// the second, by the Operator node's clock, and FirstManifestCreatedAt the
	// manifest's createdAt of the first, by the member's, each to the second.
	SecondStartedAt        *time.Time `json:"secondStartedAt,omitempty"`
	FirstManifestCreatedAt *time.Time `json:"firstManifestCreatedAt,omitempty"`
	// NotContended says why the run did not see the two backups contend.
	NotContended string `json:"notContended,omitempty"`
}

// s11OverlappingRecord is the evidence of S11 overlapping besides the
// manifests and the source's data check.
type s11OverlappingRecord struct {
	// PodSecurity are the run namespace's Pod Security labels as the run
	// read them before anything was created.
	PodSecurity map[string]string     `json:"podSecurity"`
	Cluster     *s11StandaloneCluster `json:"cluster,omitempty"`
	// ReadyLimit is BackupOverlappingReadyLimit, the run's bound on the
	// cluster before the backups.
	ReadyLimit string       `json:"readyLimit"`
	Throttle   *s11Throttle `json:"throttle"`
	// Credentials is where the cluster's statuses were searched for the keys.
	Credentials *s11Credentials     `json:"credentials"`
	Backups     []*s11OverlapBackup `json:"backups"`
	// RequestGap is the time between the returns of the two requests.
	RequestGap string      `json:"requestGap,omitempty"`
	Overlap    *s11Overlap `json:"overlap"`
	// ManagerBefore is the installed manager as LAB01 read it at its end,
	// ManagerAfter as this variant read it at its end.
	ManagerBefore *managerIdentity `json:"managerBefore,omitempty"`
	ManagerAfter  *managerIdentity `json:"managerAfter,omitempty"`
	Source        *s12Source       `json:"source,omitempty"`
	Notes         []string         `json:"notes"`
	JudgedAt      *time.Time       `json:"judgedAt,omitempty"`
	// Cleanup says what became of the rate limit and the cluster; each
	// backup's own is in its record. Residue is what the run leaves.
	Cleanup string `json:"cleanup"`
	Residue string `json:"residue"`
}

// validateS11Overlapping checks the inputs of S11 overlapping, when it is
// selected; BackupBucket is checked by validateS11.
func (c HAConfig) validateS11Overlapping() error {
	if !c.BackupOverlapping {
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
		{c.BackupInstanceManagerRestart, flagInstanceManagerRestart}} {
		if other.selected {
			return fmt.Errorf("%s and %s select different lanes; a run takes one of them", flagOverlapping,
				other.flag)
		}
	}
	if c.BackupOverlappingReadyLimit <= 0 {
		return errors.New(flagOverlapping + " needs a positive -backup-overlapping-ready-limit for its cluster")
	}
	return nil
}

// planS11Overlapping returns the lane and the requirements of a run that
// selects S11 overlapping after S11 complete, and names it and the run
// namespace's Pod Security in the summary's notes; otherwise lane and
// requirements as they are.
func planS11Overlapping(cfg HAConfig, lane string, requirements []evidence.Requirement,
	summary *evidence.Summary) (string, []evidence.Requirement) {
	if !cfg.BackupOverlapping {
		return lane, requirements
	}
	summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete and S11 " +
		"overlapping: a full backup to object storage, downloaded and checked by the run, and two backups of a " +
		"standalone cluster whose upload is slowed by a rate limit in its member's Pod, requested back to back, " +
		"which must both end Completed, the second held until the first ended, each with its own operation and " +
		"one verified artifact; no other S00-S15 qualification"
	summary.Notes = append(summary.Notes, "The run namespace enforces Pod Security privileged, with warn and "+
		"audit at restricted, so that the rate limit's ephemeral container is admitted")
	return s11OverlappingLane, append(requirements, evidence.Requirement{ID: s11ID, Variant: s11OverlappingVariant})
}

// runS11Overlapping runs S11 overlapping after S11 complete passed. The run
// creates a standalone cluster with the run's own object store Secret, waits
// until it is Ready, limits the rate of its member's TCP traffic to the object
// store as S11 interrupted-upload does, and requests two full backups of its
// database to BackupBucket back to back, each under a prefix of its own. The
// limit stays until both ended, within BackupLimit. Both target the one
// member, and the first upload takes minutes through the limit, so the second
// start meets the first's operation and is held with
// InstanceManagerOperationInProgress until it ended: a hold is required.
// Both must end Completed, each with its own operation, target and role that
// never change, an artifact that passes S11 complete's verification for that
// cluster, a manifest that names its own operation, and exactly one attempt
// prefix with one manifest and no other object under its prefix, also when
// listed again at the end. The member's record of the second's operation must
// not have been created before the record of the first's was written as
// ended. The limit must be seen removed, the source and the installed manager
// unchanged, and no credential may be in a status read, a manifest, an Event
// or the logs searched for each backup. No hold, a run that ends, or a check
// that cannot be made blocks it; what was seen to break stays a failure. The
// run removes the limit, deletes both CubridBackups and the cluster it
// created, by UID, and leaves the artifacts in the bucket.
func (h *haRun) runS11Overlapping(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s11ID+"/"+s11OverlappingVariant, &result)()
	result = evidence.Scenario{ID: s11ID, Variant: s11OverlappingVariant, Result: evidence.Fail, NeverEvents: []string{}}
	throttle := h.newS11Throttle(s11OverlappingMember)
	rec := s11OverlappingRecord{PodSecurity: map[string]string{},
		Cluster:    &s11StandaloneCluster{Name: s11OverlappingClusterName, Secret: seedSecretName},
		ReadyLimit: h.cfg.BackupOverlappingReadyLimit.String(), Throttle: &throttle,
		Credentials: &s11Credentials{FoundIn: []string{}}, Overlap: &s11Overlap{Started: []string{}},
		Notes: []string{"the rate limit acts in the network namespace of the standalone member's Pod, from an " +
			"ephemeral container with NET_ADMIN and NET_RAW (test/faults/netfault), as in S11 interrupted-upload; " +
			"the run namespace enforces Pod Security privileged for it, with warn and audit at restricted",
			"the limit is confirmed before the requests and stays until both backups ended, so each upload takes " +
				"minutes and the second start meets the first's operation; the cluster has one member, so both " +
				"backups target it, and two backups on different members at once are not exercised",
			"the run reads the two backups in turn in each round, so the readings of one round are one read apart",
			"the Operator sees that an operation ended at its next poll of it, and a backup held after a conflict " +
				"sends its start again at its next reconcile, both about 10 s apart (backupPollAfter), so the " +
				"readings can show the second backup Running while the first still shows Running or Uploading; " +
				"that is recorded (bothInProgressFrom and bothInProgressTo), not judged; what is judged is that the " +
				"member's record of the second's operation was created at or after the record of the first's " +
				"operation was written as ended (firstEndedAt, secondCreatedAt), both by the member's clock; the " +
				"Instance Manager writes that end only after the upload ended and the staging was removed, and " +
				"starts no other operation of the database before; the second's status.startedAt and the first's " +
				"manifest createdAt are recorded as evidence only",
			"a manifest names its objects by keys relative to its own attempt prefix, so it cannot name an object of " +
				"the other backup; an object of the other backup under this backup's prefix shows in the listing as " +
				"outside an attempt prefix of the backup; no upload is interrupted in this variant, so an object in a " +
				"backup's own attempt prefix that its manifest does not name came from another writer and must never " +
				"happen",
			logsNote(h.logLimit())},
		Cleanup: "nothing to delete: the run created no CubridCluster",
		Residue: "the artifacts under the backups' prefixes are left in the bucket; this run does not remove them"}
	for _, b := range s11OverlappingBackups {
		rec.Backups = append(rec.Backups, &s11OverlapBackup{s11Record: s11Record{Backup: b.name,
			Bucket: h.cfg.BackupBucket, Prefix: h.rec.Seed.Prefix + "/" + b.prefix,
			ObjectStoreService: h.cfg.ObjectStoreService, Phases: []s11Phase{}, EngineVersion: h.engineVersion,
			Credentials: &s11Credentials{FoundIn: []string{}},
			Residue:     "the artifact under the backup's prefix is left in the bucket; this run does not remove it"},
			Holds: []s11Hold{}, OperationRefsSeen: []string{}, TargetsSeen: []string{},
			Cleanup: "nothing to delete: the run did not create it", letter: b.letter, manifestFile: b.manifest})
	}
	var created [2]*databasev1alpha1.CubridBackup
	var made s11InterruptedMade
	defer func() {
		// The limit goes first, then the backups, then the cluster.
		cleaned := h.removeInterruptedFaults(ctx, &made, rec.Throttle)
		for i, b := range rec.Backups {
			cleaned = h.deleteMissingBackup(ctx, created[i], &b.Cleanup) && cleaned
		}
		made.backup = nil
		cleaned = h.deleteInterrupted(ctx, made, rec.Throttle, &rec.Cleanup, &rec.Residue) && cleaned
		result.CleanupSucceeded = &cleaned
		if made.throttle != nil {
			maps.Copy(result.Measurements, prefixed("rateLimit.", made.throttle.Measurements()))
		}
		// A pass without its evidence is a failure; a blocked or failed S11
		// overlapping keeps its result, with the evidence that could not be
		// written.
		if err := h.writeJSON(s11OverlappingRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S11 overlapping evidence: "+err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S11/"+s11OverlappingVariant+"/")
		})
	}()
	result.Limits = map[string]string{backupLimitName: h.cfg.BackupLimit.String(),
		"readyLimit": h.cfg.BackupOverlappingReadyLimit.String()}
	result.Measurements = map[string]any{"heldWithConflict": evidence.Unknown,
		"bothInProgressInReadings": evidence.Unknown, "operationRefsDistinct": evidence.Unknown,
		"uploadBytesThroughLimit": evidence.Unknown}
	for _, b := range rec.Backups {
		for _, m := range []string{measBackupCompletedWithin, measObjectsVerified, measArtifactBytes,
			measObjectsNotInManifest, "attemptPrefixes"} {
			result.Measurements[b.letter+"."+m] = evidence.Unknown
		}
	}
	err := h.overlappingSteps(ctx, &result, &rec, &created, &made)
	// A credential found in any status read, a manifest, an Event or a log
	// is a failure, whichever step ended S11 overlapping.
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, s11OverlappingClusterName+": "+credentialEvent(found))
	}
	for _, b := range rec.Backups {
		if found := b.Credentials.FoundIn; len(found) > 0 {
			result.NeverEvents = append(result.NeverEvents, b.Backup+": "+credentialEvent(found))
		}
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
		result.Result, result.Reason = evidence.Blocked, "the run ended before S11 overlapping was judged: "+reason
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, reason
	default:
		result.Result, result.Reason = evidence.Pass, s11OverlappingPassReason(&rec)
	}
	judged := time.Now().UTC()
	rec.JudgedAt = &judged
	return result
}

// s11OverlappingPassReason is the reason of a pass, from what the run
// recorded.
func s11OverlappingPassReason(rec *s11OverlappingRecord) string {
	a, b, o := rec.Backups[0], rec.Backups[1], rec.Overlap
	return fmt.Sprintf("the two backups of %s requested back to back, %s and %s, through the rate limit on %s, "+
		"both ended Completed within backup_limit, each with its own operation (%s and %s), target and role in "+
		"every reading; %s was held with %s (%d holds recorded), and the member's record of its operation was "+
		"created at %s, not before the record of %s's operation was written as ended at %s, by the member's "+
		"clock; every object each manifest names was downloaded by the run and matches it, the status and the "+
		"cluster, each manifest names its own operation, and exactly one attempt prefix of each backup, holding "+
		"its one manifest.json and no other object, held objects under its prefix, also at the end; the limit "+
		"was seen removed; the source and the installed manager were unchanged; no credential was found in the "+
		"recorded statuses, the manifests, the Events, the installed Operator's manager log or the member's log",
		s11OverlappingClusterName, a.Backup, b.Backup, rec.Throttle.Target, a.OperationRefsSeen[0],
		b.OperationRefsSeen[0], o.Second, s11OperationInProgressReason, len(a.Holds)+len(b.Holds),
		timeOrUnknown(o.SecondCreatedAt), o.First, timeOrUnknown(o.FirstEndedAt))
}

// timeOrUnknown formats a recorded time, or says it is unknown.
func timeOrUnknown(t *time.Time) string {
	if t == nil {
		return evidence.Unknown
	}
	return t.Format(time.RFC3339Nano)
}

// overlappingSteps runs the steps of S11 overlapping in order. Before the
// requests, the first error ends them. After them, the backups are judged,
// the limit is removed, and the checks at the end are all made; their errors
// are joined.
func (h *haRun) overlappingSteps(ctx context.Context, result *evidence.Scenario, rec *s11OverlappingRecord,
	created *[2]*databasev1alpha1.CubridBackup, made *s11InterruptedMade) error {
	started := time.Now().UTC()
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S11 overlapping could not tell whether a credential is in a status, a manifest or "+
			"an Event", err)
	}
	before := h.rec.OperatorAfter
	rec.ManagerBefore = &before
	if err := h.throttledStandalone(ctx, "S11 overlapping", h.cfg.BackupOverlappingReadyLimit, &rec.PodSecurity,
		rec.Cluster, rec.Credentials, rec.Throttle, made); err != nil {
		return err
	}
	// A standalone member is selected in the role unknown
	// (RoleUnknownSingleInstanceFallback in internal/controller/backuptarget.go).
	source := &s11Source{clusterUID: rec.Cluster.UID, members: []string{rec.Throttle.Target},
		role: string(databasev1alpha1.RoleUnknown)}
	if err := made.throttle.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare the rate limit: %w", err)
	}
	if err := confirmThrottle(ctx, made.throttle, rec.Throttle); err != nil {
		return err
	}
	// The two requests are sent one right after the other, nothing between.
	var requested [2]time.Time
	for i, b := range rec.Backups {
		b.source = source
		backup, err := h.createBackup(ctx, b.Backup, s11OverlappingClusterName, b.Bucket, b.Prefix)
		if err != nil {
			return err
		}
		requested[i] = time.Now().UTC()
		created[i] = backup
		b.BackupUID, b.RequestedAt = string(backup.UID), &requested[i]
	}
	rec.RequestGap = requested[1].Sub(requested[0]).Round(time.Millisecond).String()
	read, err := h.waitOverlapping(ctx, *created, rec)
	h.limitAfterBackups(ctx, made, rec.Throttle, result)
	errs := []error{h.judgeOverlapping(ctx, read, err, requested, rec, result)}
	errs = append(errs, h.throttleAtEnd(ctx, made, rec.Throttle))
	for i, b := range rec.Backups {
		if err := h.checkS11Credentials(ctx, created[i], b.manifest, b.Credentials); err != nil {
			errs = append(errs, fmt.Errorf("look for credentials of %s: %w", b.Backup, err))
		}
		if err := h.checkS11Logs(ctx, s11OverlappingClusterName, true, lastTarget(b.Phases),
			requestedOnServer(created[i], requested[i]), started, b.Credentials, before.Pods); err != nil {
			errs = append(errs, fmt.Errorf("search the logs for %s: %w", b.Backup, err))
		}
	}
	if err := h.standaloneAtEnd(ctx, made.cluster, rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: %w", err))
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	errs = append(errs, h.checkSourceS12(ctx, rec.Source, s11OverlappingSourceDataCheckFile))
	// A writer that was still uploading at the first listing shows up now,
	// with the limit removed.
	for i, b := range rec.Backups {
		if b.Attempts == nil || !b.Attempts.Listed {
			continue
		}
		at, err := h.listS11Attempts(ctx, created[i], &b.s11Record)
		b.AttemptsAtEnd = at
		if err != nil {
			errs = append(errs, fmt.Errorf("list the prefix of %s again at the end: %w", b.Backup, err))
			continue
		}
		for _, event := range secondWriter(at, "at the end") {
			result.NeverEvents = append(result.NeverEvents, b.Backup+": "+event)
		}
	}
	errs = append(errs, h.sameManager(ctx, "S11 "+s11OverlappingVariant, &before, &rec.ManagerAfter))
	return errors.Join(errs...)
}

// limitAfterBackups reads the rate limit's counters once both backups ended,
// as evidence of what passed it since the requests. A read that fails is
// recorded and judges nothing.
func (h *haRun) limitAfterBackups(ctx context.Context, made *s11InterruptedMade, f *s11Throttle,
	result *evidence.Scenario) {
	if made.throttle == nil || f.AtRequest == nil {
		return
	}
	counters, err := made.throttle.Counters(ctx)
	if err != nil {
		f.ExecErrors = append(f.ExecErrors, time.Now().UTC().Format(time.RFC3339Nano)+
			" read the rate limit's counters after the backups: "+err.Error())
		return
	}
	f.Last = &counters
	result.Measurements["uploadBytesThroughLimit"] = f.sentSince(counters)
}

// waitOverlapping reads both backups in turn until both are Completed or
// Failed, for at most BackupLimit, recording each change of each in its
// phases, searching every status read for a credential, and recording what
// each round showed of the two together (observe). Passing the limit is a
// failure, as in waitBackup.
func (h *haRun) waitOverlapping(ctx context.Context, created [2]*databasev1alpha1.CubridBackup,
	rec *s11OverlappingRecord) ([2]*databasev1alpha1.CubridBackup, error) {
	wctx, cancel := context.WithTimeout(ctx, h.cfg.BackupLimit)
	defer cancel()
	read := [2]*databasev1alpha1.CubridBackup{{}, {}}
	var ended [2]bool
	// readErr is the last reading's error, nil once a round succeeded.
	var readErr error
	err := pollUntil(wctx, h.cfg.Poll, func() (bool, error) {
		for i, b := range rec.Backups {
			if ended[i] {
				continue
			}
			backup := &databasev1alpha1.CubridBackup{}
			if err := h.Client.Get(wctx, client.ObjectKeyFromObject(created[i]), backup); err != nil {
				if wctx.Err() != nil {
					return false, nil
				}
				if retryable(err) {
					readErr = err
					return false, nil
				}
				return false, err
			}
			if backup.UID != created[i].UID {
				return false, backupVerdict{fmt.Errorf("the CubridBackup %s was replaced", b.Backup)}
			}
			if err := h.recordBackup(backup, &b.Phases, b.Credentials); err != nil {
				return false, err
			}
			read[i] = backup
			s := backup.Status.Phase
			ended[i] = s == databasev1alpha1.BackupPhaseCompleted || s == databasev1alpha1.BackupPhaseFailed
		}
		readErr = nil
		rec.observe(read, time.Now().UTC())
		// Recorded first, so that a replaced target is judged.
		for i, b := range rec.Backups {
			if t := read[i].Status.TargetInstance; t != "" && t != rec.Throttle.Target {
				return false, faultStep{fmt.Errorf("%s's target is %s, not %s, the Pod the limit acts in", b.Backup,
					t, rec.Throttle.Target)}
			}
		}
		return ended[0] && ended[1], nil
	})
	if err != nil && ctx.Err() == nil && wctx.Err() != nil && readErr != nil {
		return read, fmt.Errorf("the CubridBackups could not be read before backup_limit %s passed: %w",
			h.cfg.BackupLimit, readErr)
	}
	if err != nil && ctx.Err() == nil && wctx.Err() != nil {
		return read, backupVerdict{fmt.Errorf("the backups were not both Completed or Failed within backup_limit "+
			"%s; their last phases were %q and %q", h.cfg.BackupLimit, read[0].Status.Phase, read[1].Status.Phase)}
	}
	return read, err
}

// observe records what one round of readings showed: each backup's
// operationRefs and targets, a backup held after its target refused the
// start as a conflict, and both Running or Uploading with an operation.
func (rec *s11OverlappingRecord) observe(read [2]*databasev1alpha1.CubridBackup, at time.Time) {
	o := rec.Overlap
	for i, b := range rec.Backups {
		s, other := read[i].Status, read[1-i].Status
		if s.OperationRef != "" && !slices.Contains(b.OperationRefsSeen, s.OperationRef) {
			b.OperationRefsSeen = append(b.OperationRefsSeen, s.OperationRef)
		}
		if s.OperationRef != "" && !slices.Contains(o.Started, b.Backup) {
			o.Started = append(o.Started, b.Backup)
		}
		if target := s.TargetInstance + " (" + s.TargetRole + ")"; s.TargetInstance != "" &&
			!slices.Contains(b.TargetsSeen, target) {
			b.TargetsSeen = append(b.TargetsSeen, target)
		}
		c := meta.FindStatusCondition(s.Conditions, backupReady)
		if c == nil || s.OperationRef != "" || !slices.Contains([]string{s11OperationInProgressReason,
			s11StagingInUseReason, s11RequestConflictReason, s11ConflictReason}, c.Reason) {
			continue
		}
		hold := s11Hold{At: at, Reason: c.Reason, Message: c.Message, TargetInstance: s.TargetInstance,
			OtherPhase: string(other.Phase), OtherOperationRef: other.OperationRef, OtherTarget: other.TargetInstance}
		if n := len(b.Holds); n > 0 {
			last := b.Holds[n-1]
			if last.Reason == hold.Reason && last.TargetInstance == hold.TargetInstance &&
				last.OtherPhase == hold.OtherPhase {
				continue
			}
		}
		b.Holds = append(b.Holds, hold)
	}
	a, b := read[0].Status, read[1].Status
	if inProgress(a.Phase) && a.OperationRef != "" && inProgress(b.Phase) && b.OperationRef != "" {
		if o.BothInProgressFrom == nil {
			o.BothInProgressFrom = &at
		}
		o.BothInProgressTo = &at
	}
}

// judgeOverlapping judges the two backups as the wait left them: the
// operations and targets the readings showed (judgeOverlapOperations), each
// backup (judgeOverlapBackup) and the two together (judgeOverlap).
//
// The code's prediction. Each backup has its own idempotency key,
// cubridbackup:<namespace>:<name>:<uid> (idempotencyKey in
// internal/controller/cubridbackup_controller.go), and its own staging
// destination, backup-staging/<uid> (buildBackupRequest). The cluster has one
// member, so both select it, in the role unknown (selectBackupTarget in
// internal/controller/backuptarget.go). The first upload takes minutes
// through the limit, so the second start reaches the member while the
// first's operation has not ended: the Instance Manager finds no operation
// under the second's key and refuses it with 409 OperationInProgress
// (FindOrCreate in internal/instancemanager/opstore.go, backup in
// server.go); its staging destination is its own, so StagingDestinationInUse
// is not expected. The controller maps that to errStartConflict (statusError
// in internal/controller/imclient.go) and holdRecordedTarget, which looks up
// only the backup's own key: it finds nothing, so the backup waits with
// InstanceManagerOperationInProgress, keeps its target, and sends the start
// again at the next reconcile until the member accepts it. It never ends
// before backupTargetWait (30 minutes) has passed, with
// RecordedTargetNotStarted. So both end Completed, one after the other, each
// with its own operation and artifact. A refusal with
// InstanceManagerRequestConflict, a conflict of the other backup's operation
// taken for one under the backup's own key, must never happen; Failed with a
// conflict reason, RecordedTargetNotStarted or a staging failure is a
// never-event, and Failed for any other reason fails the variant.
func (h *haRun) judgeOverlapping(ctx context.Context, read [2]*databasev1alpha1.CubridBackup, waitErr error,
	requested [2]time.Time, rec *s11OverlappingRecord, result *evidence.Scenario) error {
	judgeOverlapOperations(rec, result)
	if waitErr != nil {
		return waitErr
	}
	var errs []error
	for i, b := range rec.Backups {
		errs = append(errs, h.judgeOverlapBackup(ctx, read[i], requested[i], b, rec.Backups[1-i], result))
	}
	return errors.Join(append(errs, h.judgeOverlap(ctx, read, rec, result))...)
}

// judgeOverlapOperations adds a never-event for an operation the two
// backups share, a second operation or target of one backup, and a refusal
// of one backup's start as a request of its own key with another body.
func judgeOverlapOperations(rec *s11OverlappingRecord, result *evidence.Scenario) {
	a, b := rec.Backups[0], rec.Backups[1]
	distinct := true
	for _, op := range a.OperationRefsSeen {
		if slices.Contains(b.OperationRefsSeen, op) {
			distinct = false
			result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("%s and %s share operation %s", a.Backup,
				b.Backup, op))
		}
	}
	result.Measurements["operationRefsDistinct"] = distinct
	for _, x := range rec.Backups {
		if len(x.OperationRefsSeen) > 1 {
			result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("%s's operationRef was %s: a second "+
				"operation", x.Backup, strings.Join(x.OperationRefsSeen, ", then ")))
		}
		if len(x.TargetsSeen) > 1 {
			result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("%s's target was %s: its recorded target "+
				"was replaced", x.Backup, strings.Join(x.TargetsSeen, ", then ")))
		}
		for _, hold := range x.Holds {
			if hold.Reason == s11RequestConflictReason {
				result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("%s's start was refused at %s as an "+
					"operation of its own key with another request (%s: %s) while the other backup was %s: a "+
					"conflict with the other backup's operation taken for its own", x.Backup,
					hold.At.Format(time.RFC3339Nano), hold.Reason, hold.Message, hold.OtherPhase))
				break
			}
		}
	}
}

// judgeOverlapBackup judges one backup as the wait left it: it must be
// Completed with an artifact that passes S11 complete's verification, a
// manifest that names its own operation and one attempt prefix with one
// manifest under its prefix. other is the other backup.
func (h *haRun) judgeOverlapBackup(ctx context.Context, backup *databasev1alpha1.CubridBackup, requested time.Time,
	b, other *s11OverlapBackup, result *evidence.Scenario) error {
	s := backup.Status
	if s.StartedAt != nil {
		at := s.StartedAt.UTC()
		b.StatusStartedAt = &at
	}
	if s.Phase == databasev1alpha1.BackupPhaseFailed {
		b.Failure = readyReason(backup)
		c := meta.FindStatusCondition(s.Conditions, backupReady)
		if c != nil && (slices.Contains([]string{s11OperationInProgressReason, s11StagingInUseReason,
			s11RequestConflictReason, s11ConflictReason, s11TargetNotStartedReason}, c.Reason) ||
			strings.Contains(c.Message, "staging")) {
			result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("%s ended Failed for a conflict or its "+
				"staging: %s", b.Backup, b.Failure))
		}
		return backupVerdict{fmt.Errorf("%s ended Failed: %s", b.Backup, b.Failure)}
	}
	within := b.Phases[len(b.Phases)-1].At.Sub(requested)
	b.CompletedWithin = within.Round(time.Millisecond).String()
	result.Measurements[b.letter+"."+measBackupCompletedWithin] = b.CompletedWithin
	if done := s.CompletedAt; done != nil {
		b.StatusCompletedWithin = done.Sub(backup.CreationTimestamp.Time).String()
	}
	b.Artifact = s.Artifact
	if b.Artifact == nil {
		result.NeverEvents = append(result.NeverEvents, b.Backup+" is Completed without an artifact in its status")
		return nil
	}
	manifest, verifyErr := h.verifyS11Artifact(ctx, backup, &b.s11Record)
	b.manifest = manifest
	var errs []error
	if manifest != nil {
		if err := h.write(b.manifestFile, manifest); err != nil {
			errs = append(errs, fmt.Errorf("write the manifest of %s: %w", b.Backup, err))
		}
	}
	v := b.Verification
	if v.ObjectsChecked {
		result.Measurements[b.letter+"."+measObjectsVerified] = v.ObjectsVerified
		result.Measurements[b.letter+"."+measArtifactBytes] = v.BytesDownloaded
	}
	if v.Listed {
		result.Measurements[b.letter+"."+measObjectsNotInManifest] = len(v.NotInManifest)
		if len(v.NotInManifest) > 0 {
			// No upload is interrupted here, so no earlier attempt left them.
			result.NeverEvents = append(result.NeverEvents, b.Backup+": objects in its own attempt prefix that its "+
				"manifest does not name, from another writer: "+strings.Join(v.NotInManifest, ", "))
		}
	}
	if len(v.Unreadable) > 0 {
		result.NeverEvents = append(result.NeverEvents, b.Backup+" is Completed, and objects of its artifact could "+
			"not be read: "+strings.Join(v.Unreadable, "; "))
	}
	if len(v.Problems) > 0 {
		errs = append(errs, backupVerdict{fmt.Errorf("the artifact of %s does not match: %s", b.Backup,
			strings.Join(v.Problems, "; "))})
	}
	if verifyErr != nil {
		errs = append(errs, fmt.Errorf("verify the artifact of %s: %w", b.Backup, verifyErr))
	}
	errs = append(errs, manifestOwner(manifest, s.OperationRef, b, other, result))
	if err := h.checkOverlapAttempts(ctx, backup, b, result); err != nil {
		errs = append(errs, fmt.Errorf("list the prefix of %s: %w", b.Backup, err))
	}
	return errors.Join(errs...)
}

// manifestOwner records the manifest's operationID and createdAt and
// requires the operationID to be operationRef, the backup's own: one of the
// other backup's operations must never happen.
func manifestOwner(manifest []byte, operationRef string, b, other *s11OverlapBackup,
	result *evidence.Scenario) error {
	if manifest == nil {
		return nil
	}
	parsed, err := instancemanager.ParseManifest(manifest)
	if err != nil {
		// verifyS11Artifact recorded it as a problem.
		return nil
	}
	b.ManifestOperationID = parsed.OperationID
	if created, err := time.Parse(time.RFC3339, parsed.CreatedAt); err == nil {
		created = created.UTC()
		b.ManifestCreatedAt = &created
	}
	switch {
	case parsed.OperationID != "" && slices.Contains(other.OperationRefsSeen, parsed.OperationID):
		result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("the manifest of %s names operation %s, "+
			"which is %s's", b.Backup, parsed.OperationID, other.Backup))
	case parsed.OperationID != operationRef:
		return backupVerdict{fmt.Errorf("the manifest of %s names operation %q, not its status.operationRef %q",
			b.Backup, parsed.OperationID, operationRef)}
	}
	return nil
}

// checkOverlapAttempts lists the backup's prefix with the run's own client
// (listS11Attempts): exactly one attempt prefix of the backup may hold
// objects, the artifact's, with its one manifest.json; a second one, or an
// object outside an attempt prefix of the backup, the other's among them,
// must never happen.
func (h *haRun) checkOverlapAttempts(ctx context.Context, backup *databasev1alpha1.CubridBackup,
	b *s11OverlapBackup, result *evidence.Scenario) error {
	a, err := h.listS11Attempts(ctx, backup, &b.s11Record)
	b.Attempts = a
	if err != nil {
		return err
	}
	result.Measurements[b.letter+".attemptPrefixes"] = len(a.Prefixes)
	for _, event := range secondWriter(a, "") {
		result.NeverEvents = append(result.NeverEvents, b.Backup+": "+event)
	}
	manifestKey := b.Verification.ManifestKey
	if len(a.Prefixes) == 1 && manifestKey != "" &&
		(a.Prefixes[0].Prefix != path.Dir(manifestKey) || a.Manifests != 1) {
		return backupVerdict{fmt.Errorf("the one attempt prefix of %s that holds objects, %s, is not the "+
			"artifact's, %s, with its one manifest.json (%d found)", b.Backup, a.Prefixes[0].Prefix,
			path.Dir(manifestKey), a.Manifests)}
	}
	return nil
}

// judgeOverlap judges the two backups together. StagingDestinationInUse is
// not predicted, since each backup has its own staging destination, and
// fails the variant. The Instance Manager runs one operation of the database
// at a time (judgeOneTarget). The second must have been held with
// InstanceManagerOperationInProgress: otherwise the first ended before the
// second was accepted, the overlap was not exercised, and the variant is
// blocked. Both in progress in one round of readings is recorded, not judged.
func (h *haRun) judgeOverlap(ctx context.Context, read [2]*databasev1alpha1.CubridBackup,
	rec *s11OverlappingRecord, result *evidence.Scenario) error {
	o := rec.Overlap
	var errs []error
	for _, b := range rec.Backups {
		for _, hold := range b.Holds {
			switch hold.Reason {
			case s11OperationInProgressReason:
				o.HeldWithConflict = true
			case s11StagingInUseReason:
				errs = append(errs, backupVerdict{fmt.Errorf("%s was held at %s because a running operation of "+
					"another request owns its staging destination (%s), which its own staging destination rules out",
					b.Backup, hold.At.Format(time.RFC3339Nano), hold.Message)})
			}
		}
	}
	result.Measurements["heldWithConflict"] = o.HeldWithConflict
	result.Measurements["bothInProgressInReadings"] = o.BothInProgressFrom != nil
	errs = append(errs, h.orderJudged(ctx, read, rec, result))
	if !o.HeldWithConflict {
		o.NotContended = "no reading showed a backup held with " + s11OperationInProgressReason + ": the first " +
			"ended before the second was accepted, so the overlap was not exercised"
		errs = append(errs, errors.New(o.NotContended))
	}
	return errors.Join(errs...)
}

// heldWithConflict reports whether a reading showed b held with
// InstanceManagerOperationInProgress.
func heldWithConflict(b *s11OverlapBackup) bool {
	return slices.ContainsFunc(b.Holds, func(h s11Hold) bool { return h.Reason == s11OperationInProgressReason })
}

// overlapOrder orders the two backups on the member: the one held after a
// conflict ran second; when neither or both were, the one with the earlier
// status.startedAt ran first. Otherwise the order cannot be told.
func overlapOrder(rec *s11OverlappingRecord) (first, second *s11OverlapBackup, err error) {
	a, b := rec.Backups[0], rec.Backups[1]
	switch heldA, heldB := heldWithConflict(a), heldWithConflict(b); {
	case heldA && !heldB:
		return b, a, nil
	case heldB && !heldA:
		return a, b, nil
	case a.StatusStartedAt == nil || b.StatusStartedAt == nil || a.StatusStartedAt.Equal(*b.StatusStartedAt):
		return nil, nil, errors.New("which of the two backups ran first could not be told: neither " +
			"or both were held, and their status.startedAt is missing or the same")
	case a.StatusStartedAt.Before(*b.StatusStartedAt):
		return a, b, nil
	}
	return b, a, nil
}

// orderJudged judges the order of the two operations on the member
// (judgeOneTarget) and requires that it was judged: a pass without the
// member's records of both would show nothing about their order.
func (h *haRun) orderJudged(ctx context.Context, read [2]*databasev1alpha1.CubridBackup,
	rec *s11OverlappingRecord, result *evidence.Scenario) error {
	if read[0].Status.OperationRef == "" || read[1].Status.OperationRef == "" {
		return errors.New("the order on the member was not judged: a backup has no operationRef")
	}
	if err := h.judgeOneTarget(ctx, rec.Throttle.Target, rec, result); err != nil {
		return err
	}
	if o := rec.Overlap; o.First == "" || o.FirstEndedAt == nil || o.SecondCreatedAt == nil {
		return errors.New("the order on the member was not judged: the records of both operations were not read")
	}
	return nil
}

// judgeOneTarget requires, on target, the member's record of the
// second backup's operation created at or after the record of the first's
// was written as ended. The Instance Manager refuses a new operation of the
// database while a record of another one has not ended (FindOrCreate in
// internal/instancemanager/opstore.go), and writes a backup's end only after
// its upload ended and its staging was removed (runBackup in server.go); both
// times are the member's clock, under the store's lock. A record that cannot
// be read, or a first one that has not ended, leaves it unknown and blocks
// the variant. The second's status.startedAt and the first's manifest
// createdAt are recorded as evidence only.
func (h *haRun) judgeOneTarget(ctx context.Context, target string, rec *s11OverlappingRecord,
	result *evidence.Scenario) error {
	o := rec.Overlap
	first, second, err := overlapOrder(rec)
	if err != nil {
		return err
	}
	o.First, o.Second = first.Backup, second.Backup
	o.SecondStartedAt, o.FirstManifestCreatedAt = second.StatusStartedAt, first.ManifestCreatedAt
	ended, err := h.readOperationRecord(ctx, target, first)
	if err != nil {
		return err
	}
	created, err := h.readOperationRecord(ctx, target, second)
	if err != nil {
		return err
	}
	if !ended.State.IsTerminal() {
		return fmt.Errorf("the record of %s's operation %s on %s is %s, not ended, so when it ended is not known",
			first.Backup, ended.ID, target, ended.State)
	}
	endedAt, createdAt := ended.UpdatedAt.UTC(), created.CreatedAt.UTC()
	o.FirstEndedAt, o.SecondCreatedAt = &endedAt, &createdAt
	if createdAt.Before(endedAt) {
		result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("two backups ran on %s at once: the record of "+
			"%s's operation %s was created at %s, before the record of %s's operation %s was written as ended at %s, "+
			"by the member's clock", target, second.Backup, created.ID, createdAt.Format(time.RFC3339Nano),
			first.Backup, ended.ID, endedAt.Format(time.RFC3339Nano)))
	}
	return nil
}

// readOperationRecord reads, in the target member, the Instance Manager's
// record of b's operation and records what it read in b.OperationRecord. A
// record that cannot be read, or is not of that backup operation, is
// returned as an error.
func (h *haRun) readOperationRecord(ctx context.Context, target string, b *s11OverlapBackup) (
	instancemanager.Operation, error) {
	var op instancemanager.Operation
	id := b.OperationRefsSeen[0]
	r := &s11OperationRecord{Pod: target, OperationID: id}
	b.OperationRecord = r
	fail := func(msg string) (instancemanager.Operation, error) {
		r.ReadError = msg
		return op, fmt.Errorf("the record of %s's operation %s on %s could not be read, so whether the two backups "+
			"ran at once is not known: %s", b.Backup, id, target, msg)
	}
	if !plainName.MatchString(id) {
		return fail("the operation ID is not a plain name")
	}
	out, err := h.member(ctx, target, "cat", "--", imOperationsDir+"/"+id+".json")
	if err != nil {
		return fail(err.Error())
	}
	if json.Unmarshal([]byte(out), &op) != nil {
		return fail("the answer is not an operation record")
	}
	if op.ID != id || op.Kind != instancemanager.OpBackup || op.CreatedAt.IsZero() || op.UpdatedAt.IsZero() {
		return fail(fmt.Sprintf("the record read is of %s operation %q, or has no times", op.Kind, op.ID))
	}
	createdAt, updatedAt := op.CreatedAt.UTC(), op.UpdatedAt.UTC()
	r.State, r.CreatedAt, r.UpdatedAt = string(op.State), &createdAt, &updatedAt
	return op, nil
}
