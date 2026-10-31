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
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	s12GracefulStopLane                = "lab-installed-ha-s12-graceful-stop"
	s12GracefulStopVariant             = "graceful-stop"
	s12GracefulStopRecordFile          = "S12/graceful-stop/record.json"
	s12GracefulStopSamplesFile         = "S12/graceful-stop/sampler.jsonl"
	s12GracefulStopLogFile             = "S12/graceful-stop/member-log.jsonl"
	s12GracefulStopDataCheckFile       = "S12/graceful-stop/data-check.json"
	s12GracefulStopSourceDataCheckFile = "S12/graceful-stop/source-data-check.json"
	// s12GracefulStopClusterName is the cluster restored while its restore
	// target's Pod is deleted, in the run namespace, and
	// s12GracefulStopSecretName its own copy of the seed Secret.
	s12GracefulStopClusterName = "ha-gracefulstop"
	s12GracefulStopSecretName  = "object-storage-gracefulstop"
	// s12GracefulStopMarker names the marker row written through that
	// cluster; its value is the run ID.
	s12GracefulStopMarker   = "s12gs"
	flagRestoreGracefulStop = "-restore-graceful-stop"
	// restoredbUtility is the utility the variant's sampler watches, and
	// restoredbCommand starts the command the Instance Manager logs for it
	// (Restore in internal/instancemanager/restore.go).
	restoredbUtility = "restoredb"
	restoredbCommand = "cubrid restoredb "
	// restoreStopReasonPrefix starts the failure reason of a restore whose
	// restoredb was ended by a stop of its member: stoppingReason
	// (internal/instancemanager/operations.go) puts errStopping's text before
	// "restore failed: " (runRestore in server.go) and "restoredb failed: "
	// (Restore in restore.go).
	restoreStopReasonPrefix = memberStopping + "restore failed: restoredb failed: "
	// imDataRoot is the mount of the member's data volume, the sampler's
	// root: the restore's database directory and its staging directory are
	// two levels below it (restoreTargetRoot and restoreStagingRoot in
	// internal/controller/bootstrap.go).
	imDataRoot = "/var/lib/cubrid"
	// The measurements of the variant.
	measDeleteAfterRestoredbSeen = "deleteAfterRestoredbSeen"
	measRestoredbDurationMs      = "restoredbDurationMs"
	measCleanupSeenAfterDelete   = "cleanupSeenAfterDelete"
	measHoldersAfterCleanup      = "holdersAfterCleanup"
	measRetriesAfterStop         = "retriesAfterStop"
	measOperationIDChanged       = "operationIDChanged"
	// signalKilled is the error of a utility the cancellation killed
	// (os/exec's ExitError for SIGKILL).
	signalKilled = "signal: killed"
)

// restoreSamplerScript is samplerScript for the data volume: its readings
// list the entries two levels below it, among them databases/<db> and
// restore-staging/<cluster UID>, and every open file below it.
func restoreSamplerScript() string {
	return samplerScript(imDataRoot)
}

// s12StopSampler is what the run took from the sampler in the restore
// target's database container and found in it.
type s12StopSampler struct {
	samplerStream
	// FirstRestoredbAt and LastRestoredbAt are the first and the last
	// reading, by the run's clock, that showed a restoredb process.
	FirstRestoredbAt *time.Time `json:"firstRestoredbAt,omitempty"`
	LastRestoredbAt  *time.Time `json:"lastRestoredbAt,omitempty"`
	// DatabaseDir and StagingDir are the entries, below imDataRoot, of the
	// database directory the restore registers and of its staging
	// directory. DatabaseDirSeenAt is the first reading that showed the
	// database directory, CleanupSeenAt the first after it that did not: the
	// restore's cleanup had begun. StagingGoneAt is the first reading without
	// the staging directory after one that showed it.
	DatabaseDir       string     `json:"databaseDir"`
	StagingDir        string     `json:"stagingDir"`
	DatabaseDirSeenAt *time.Time `json:"databaseDirSeenAt,omitempty"`
	CleanupSeenAt     *time.Time `json:"cleanupSeenAt,omitempty"`
	StagingGoneAt     *time.Time `json:"stagingGoneAt,omitempty"`
	// LastHolder is the last file of the restore's directories a CUBRID
	// process held before CleanupSeenAt, and LastHolderAt its reading.
	LastHolder   string     `json:"lastHolder,omitempty"`
	LastHolderAt *time.Time `json:"lastHolderAt,omitempty"`
	// HoldersAfterCleanup are the files of the restore's directories a
	// CUBRID process held at or after CleanupSeenAt, and UtilityAfterCleanup
	// the restoredb processes of the readings after it: neither may ever be
	// there. OtherHoldersAfterCleanup are holders that were not CUBRID
	// processes, such as the Instance Manager while it removes the
	// directories: recorded, not judged.
	HoldersAfterCleanup      []string `json:"holdersAfterCleanup"`
	UtilityAfterCleanup      []string `json:"restoredbAfterCleanup"`
	OtherHoldersAfterCleanup []string `json:"otherHoldersAfterCleanup"`
	// PresentAfterOperationFailed are the readings, by the container's
	// clock, that the sampler started after the Instance Manager logged the
	// operation Failed and that still showed the database or staging
	// directory: the cleanup came after the record, which must never be.
	PresentAfterOperationFailed []string `json:"presentAfterOperationFailed"`
	// ReadingsAfterOperationFailed counts the readings the sampler started
	// after the operation was logged Failed, and LastReadingAt is when it
	// started its last reading, both by the container's clock.
	ReadingsAfterOperationFailed int        `json:"readingsAfterOperationFailed"`
	LastReadingAt                *time.Time `json:"lastReadingAt,omitempty"`
}

func (s *s12StopSampler) stream() *samplerStream { return &s.samplerStream }

func (s *s12StopSampler) sawUtility(at time.Time) {
	if s.FirstRestoredbAt == nil {
		s.FirstRestoredbAt = &at
	}
	s.LastRestoredbAt = &at
}

// s12StopLog is what the run found in the followed log of the restore
// target's database container: the lines it orders, as their line numbers,
// and the times the Instance Manager logged them, by the container's clock.
type s12StopLog struct {
	BytesRead int64      `json:"bytesRead"`
	Truncated bool       `json:"truncated"`
	Lines     int        `json:"lines"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	EndError  string     `json:"endError,omitempty"`
	// RestoredbLine is the line of the end of restoredb into the cluster's
	// staging, RestoredbResult its message (Command failed or Ran command),
	// RestoredbDurationMs its duration and RestoredbAt its time.
	RestoredbLine       int        `json:"restoredbLine,omitempty"`
	RestoredbResult     string     `json:"restoredbResult,omitempty"`
	RestoredbDurationMs *int64     `json:"restoredbDurationMs,omitempty"`
	RestoredbAt         *time.Time `json:"restoredbAt,omitempty"`
	// RestoredbErr and RestoredbExitCode are the error and the exit code the
	// line gave; a utility killed by a signal has exit code -1.
	RestoredbErr      string `json:"restoredbErr,omitempty"`
	RestoredbExitCode *int   `json:"restoredbExitCode,omitempty"`
	// OperationID, OperationFailedLine, OperationFailedReason and
	// OperationFailedAt are of the first Operation failed of a restore
	// operation.
	OperationID           string     `json:"operationID,omitempty"`
	OperationFailedLine   int        `json:"operationFailedLine,omitempty"`
	OperationFailedReason string     `json:"operationFailedReason,omitempty"`
	OperationFailedAt     *time.Time `json:"operationFailedAt,omitempty"`
	// HeartbeatStopLine and ServerStopLine are of the shutdown's commands,
	// with the message each ended with and its time.
	HeartbeatStopLine   int        `json:"heartbeatStopLine,omitempty"`
	HeartbeatStopResult string     `json:"heartbeatStopResult,omitempty"`
	HeartbeatStopAt     *time.Time `json:"heartbeatStopAt,omitempty"`
	ServerStopLine      int        `json:"serverStopLine,omitempty"`
	ServerStopResult    string     `json:"serverStopResult,omitempty"`
	ServerStopAt        *time.Time `json:"serverStopAt,omitempty"`
	// StopLine, StopLoggedAt and StopLoggedAfterDelete are as in S11
	// graceful-stop's log (s11StopLog).
	StopLine              int        `json:"stopLine,omitempty"`
	StopLoggedAt          *time.Time `json:"stopLoggedAt,omitempty"`
	StopLoggedAfterDelete string     `json:"stopLoggedAfterDelete,omitempty"`
}

func (l *s12StopLog) setEnd(at time.Time, read int64, truncated bool, endErr string) {
	l.EndedAt, l.BytesRead, l.Truncated, l.EndError = &at, read, truncated, endErr
}

// s12GracefulStopRecord is the evidence of S12 graceful-stop besides the
// sampler's readings, the target's log and the data checks.
type s12GracefulStopRecord struct {
	s12Record
	SecretUID string `json:"secretUID,omitempty"`
	// TargetMember is the restore's target, the member whose Pod is deleted.
	TargetMember string          `json:"targetMember"`
	Trigger      *s11StopTrigger `json:"trigger"`
	Sampler      *s12StopSampler `json:"sampler"`
	Log          *s12StopLog     `json:"log"`
	// AttemptsAtDelete and OperationAtDelete are of the last reading of the
	// cluster recorded before the deletion was accepted, deleteReading its
	// index in Bootstrap (-1 when there is none). StoppedOperation is the
	// restore operation the stop ended, as the target's log names it.
	AttemptsAtDelete  *int32 `json:"attemptsAtDelete,omitempty"`
	OperationAtDelete string `json:"operationAtDelete,omitempty"`
	StoppedOperation  string `json:"stoppedOperation,omitempty"`
	deleteReading     int
	// Reasons are the BootstrapReady reasons of the readings after the
	// deletion, each change once; Attempts the attempts and operations every
	// reading showed; Retries the attempts the status showed ending, and
	// RetriesAfterStop those after the deletion; OperationIDAfter and
	// AttemptsAfter are of the last reading that named an operation;
	// RetryEvents are the messages of the Operator's RestoreFailedRetrying
	// Events of the cluster, EventsError why they could not be read.
	Reasons          []string     `json:"reasonsAfterDelete"`
	Attempts         []s12Attempt `json:"attempts"`
	Retries          []s12Retry   `json:"retries"`
	RetriesAfterStop []s12Retry   `json:"retriesAfterStop"`
	OperationIDAfter string       `json:"operationIDAfter,omitempty"`
	AttemptsAfter    *int32       `json:"attemptsAfter,omitempty"`
	RetryEvents      []string     `json:"retryEvents"`
	EventsError      string       `json:"eventsError,omitempty"`
	// PlacementMissed says why the stop did not land while restoredb ran.
	PlacementMissed string           `json:"placementMissed,omitempty"`
	ManagerBefore   *managerIdentity `json:"managerBefore,omitempty"`
	ManagerAfter    *managerIdentity `json:"managerAfter,omitempty"`
	// Searched is where, besides the statuses, the run looked for the object
	// store's keys: the Events, the sampler's output and the logs.
	Searched *s11Credentials `json:"credentialsSearched"`
	Cleanup  string          `json:"cleanup"`
	JudgedAt *time.Time      `json:"judgedAt,omitempty"`
}

// s12GracefulStopMade is what the variant created, for its cleanup.
type s12GracefulStopMade struct {
	cluster *databasev1alpha1.CubridCluster
	secret  *corev1.Secret
}

// validateS12GracefulStop checks the inputs of S12 graceful-stop, when it is
// selected.
func (c HAConfig) validateS12GracefulStop() error {
	if !c.RestoreGracefulStop {
		return nil
	}
	if !c.Restore {
		return errors.New(flagRestoreGracefulStop + " needs -restore: S12 graceful-stop follows S12 exact")
	}
	for _, other := range []struct {
		selected bool
		flag     string
	}{{c.RestoreTampered, "-restore-tampered"}, {c.RestoreWrongTarget, "-restore-wrong-target"},
		{c.RestoreOperatorRestart, "-restore-operator-restart"},
		{c.RestoreInstanceManagerRestart, "-restore-instance-manager-restart"}} {
		if other.selected {
			return fmt.Errorf("%s and %s select different lanes; a run takes one of them", flagRestoreGracefulStop,
				other.flag)
		}
	}
	return nil
}

// planS12GracefulStop returns the lane and the requirements of a run that
// selects S12 graceful-stop after S12 exact, and names it in the summary's
// notes; otherwise lane and requirements as they are.
func planS12GracefulStop(cfg HAConfig, lane string, requirements []evidence.Requirement,
	summary *evidence.Summary) (string, []evidence.Requirement) {
	if !cfg.RestoreGracefulStop {
		return lane, requirements
	}
	summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete, S12 exact and " +
		"S12 graceful-stop: a full backup to object storage, restored into a new cluster in the run namespace, and " +
		"into another whose restore target's Pod is deleted with its grace period while restoredb runs, which " +
		"must end that attempt for the stop with no CUBRID process holding a file of the restore once its cleanup " +
		"began, and complete on the next; no other S00-S15 qualification"
	return s12GracefulStopLane, append(requirements,
		evidence.Requirement{ID: s12ID, Variant: s12GracefulStopVariant})
}

// newS12GracefulStopRecord is the record S12 graceful-stop starts with.
func (h *haRun) newS12GracefulStopRecord() *s12GracefulStopRecord {
	return &s12GracefulStopRecord{s12Record: s12Record{Cluster: s12GracefulStopClusterName,
		SeedPrefix: h.rec.Seed.Prefix + "/gracefulstop-seed", SecretRef: s12GracefulStopSecretName,
		Bootstrap: []s12Phase{}, Members: []imageIdentity{}, SecretsReferenced: []string{},
		Credentials: &s12Credentials{FoundIn: []string{}},
		Notes: []string{"the cluster restored while its restore target's Pod is deleted uses a copy of the seed " +
			"Secret of its own, " + s12GracefulStopSecretName + "; the source uses " + seedSecretName,
			"the run restores S11's artifact as it is, without filler data: in POC-27 a restoredb of a 5 MB " +
				"backup of a database with a 512 MB data volume still ran 2 s after its start; a restoredb that " +
				"ended before the stop reached the Instance Manager leaves the variant blocked",
			"the sampler runs in the restore target's database container through one exec session and reads it " +
				"every " + samplerInterval.String() + ": a writer that holds a file for less than that may not be " +
				"seen, and the session ends when the container stops, so nothing after the container's end is " +
				"sampled",
			"the sampler prints process IDs, command names and argv, the entries two levels below " + imDataRoot +
				" and the paths of open files below it, never a file's contents",
			"the restore's cleanup logs nothing (unregister and the staging's removal in Restore, internal/" +
				"instancemanager/restore.go); the sampler sees it begun at its first reading without the " +
				"database directory, which goes at the end of the directory's removal, so a hold in the few " +
				"milliseconds of that removal is not judged",
			"holders of the restore's files are judged when they are CUBRID processes (cub*); other holders, " +
				"such as the Instance Manager's own removal, are recorded",
			"the target's log is followed through pods/log from before the deletion until the deleted container " +
				"ended, whole; the deleted Pod's log is gone once it is replaced, so this is the only read of it",
			"the deletion uses the Pod's own terminationGracePeriodSeconds, so its preStop hook runs " +
				"instance-manager shutdown before the container gets SIGTERM",
			"the Operator's Events of the cluster other than RestoreFailedRetrying were searched for keys, not " +
				"judged",
			logsNote(h.logLimit())},
		Residue: "the cluster's seed backups under its seed prefix are not removed by this run; its PVCs go with " +
			"the namespace"},
		TargetMember: s12GracefulStopClusterName + "-0", Trigger: &s11StopTrigger{},
		Sampler: &s12StopSampler{samplerStream: samplerStream{Interval: samplerInterval.String()},
			DatabaseDir:         strings.TrimPrefix(imDatabasesDir, imDataRoot+"/") + "/" + databaseName,
			HoldersAfterCleanup: []string{}, UtilityAfterCleanup: []string{}, OtherHoldersAfterCleanup: []string{},
			PresentAfterOperationFailed: []string{}},
		Log: &s12StopLog{}, deleteReading: -1, Reasons: []string{}, Attempts: []s12Attempt{},
		Retries: []s12Retry{}, RetriesAfterStop: []s12Retry{}, RetryEvents: []string{},
		Searched: &s11Credentials{FoundIn: []string{}}, Cleanup: nothingCreated}
}

// runS12GracefulStop runs S12 graceful-stop after S12 exact passed. A new
// cluster is restored from S11's artifact. Once its restore target's
// database container runs, the run starts two streams from it: a sampler
// that prints the cub* processes, the entries two levels below the data
// volume and the files open below it every 100 ms (restoreSamplerScript),
// and the container's log, followed. As soon as the sampler shows a process
// whose argv runs restoredb, the run deletes the target's Pod with its grace
// period and its UID as a precondition.
//
// The Pod's preStop hook asks the Instance Manager to stop
// (preStopShutdown in internal/controller/cubridcluster_controller.go). Stop
// cancels the restore's context and waits for the operation before it runs
// heartbeat stop and server stop (Stop and Shutdown in
// internal/instancemanager/operations.go). The cancellation kills restoredb's
// process group, and Run returns only once the group is gone
// (internal/instancemanager/cli.go). Restore then removes the database
// directory it registered and its line of databases.txt (unregister), and
// the staging directory it created; runRestore records the operation Failed
// with restoreStopReasonPrefix (Restore in restore.go, runRestore and
// stoppingReason in server.go and operations.go). The record is on the data
// volume. The Operator reads it from the old or the replacement's Instance
// Manager, ends the attempt with RestoreFailedRetrying and starts the next
// under a new key, at most maxBootstrapAttempts in all; the next attempt
// finds no marked leftover to reclaim (reclaimIncomplete in ownership.go)
// and restores again (restoreAttemptFailed in internal/controller/
// bootstrap.go).
//
// So the run requires: in the target's log, restoredb's end, then the
// restore operation's Operation failed with restoreStopReasonPrefix, then
// the shutdown's heartbeat stop and server stop; no sampler reading at or
// after the first one without the database directory showing a CUBRID
// process holding a file of the database or staging directory, nor a later
// one showing restoredb; no reading started after the Operation failed line
// showing either directory; then exactly one retry naming the stop, another
// operation that completes, and what S12 exact requires of the cluster,
// within restore_limit of its creation; the source and the installed
// manager unchanged; and no key in a status, an Event, the sampler's output
// or the logs. A restoredb that was not seen while the restore ran, or that
// ended before the stop reached the Instance Manager, leaves the variant
// blocked, as does a log or sampler that ended before it showed what is
// judged.
func (h *haRun) runS12GracefulStop(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s12ID+"/"+s12GracefulStopVariant, &result)()
	result = evidence.Scenario{ID: s12ID, Variant: s12GracefulStopVariant, Result: evidence.Fail,
		NeverEvents: []string{}}
	rec := h.newS12GracefulStopRecord()
	watch := &stopWatch{utility: restoredbUtility, seen: make(chan struct{})}
	var made s12GracefulStopMade
	var memberLog []byte
	defer func() {
		cleaned := h.deleteS12GracefulStop(ctx, made, rec)
		result.CleanupSucceeded = &cleaned
		h.writeStopEvidence(stopEvidence{name: "S12 graceful-stop", dir: "S12/" + s12GracefulStopVariant + "/",
			samples: s12GracefulStopSamplesFile, log: s12GracefulStopLogFile, record: s12GracefulStopRecordFile},
			watch, memberLog, rec, &result)
	}()
	result.Limits = map[string]string{restoreLimitName: h.cfg.RestoreLimit.String()}
	result.Measurements = map[string]any{restoredWithinMeasurement: evidence.Unknown,
		dataRulesHeldMeasurement: evidence.Unknown, postBackupRowsMeasurement: evidence.Unknown,
		measSamples: evidence.Unknown, measDeleteAfterRestoredbSeen: evidence.Unknown,
		measRestoredbDurationMs: evidence.Unknown, measCleanupSeenAfterDelete: evidence.Unknown,
		measHoldersAfterCleanup: evidence.Unknown, measStopLoggedAfterDelete: evidence.Unknown,
		measRetriesAfterStop: evidence.Unknown, measOperationIDChanged: evidence.Unknown}
	err := h.s12GracefulStopSteps(ctx, rec, &made, watch, &memberLog, &result)
	h.judgeS12GracefulStopRun(ctx, err, rec, &result)
	judged := time.Now().UTC()
	rec.JudgedAt = &judged
	return result
}

// judgeS12GracefulStopRun sets the result of S12 graceful-stop from the
// error of its steps and the never-events they recorded. A missed placement
// blocks the variant unless something that must never happen was seen, the
// restored data differed, or the restore was judged (restoreVerdict): a
// restore that failed or broke a rule stays a failure whatever the stop did.
func (h *haRun) judgeS12GracefulStopRun(ctx context.Context, err error, rec *s12GracefulStopRecord,
	result *evidence.Scenario) {
	found := slices.Concat(rec.Credentials.FoundIn, rec.Searched.FoundIn)
	if len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	never := strings.Join(result.NeverEvents, "; ")
	if missed := rec.PlacementMissed; missed != "" && never == "" && !errors.As(err, new(dataViolation)) &&
		!errors.As(err, new(restoreVerdict)) {
		reason := "placement missed: " + missed
		if err != nil {
			reason += "; then: " + err.Error()
		}
		result.Result, result.Reason = evidence.Blocked, reason
		return
	}
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
	case errors.As(err, new(restoreVerdict)) || errors.As(err, new(dataViolation)) || never != "":
		result.Result, result.Reason = evidence.Fail, reason
	case err != nil && ctx.Err() != nil:
		result.Result, result.Reason = evidence.Blocked, "the run ended before S12 graceful-stop was judged: "+
			reason
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, reason
	default:
		result.Result, result.Reason = evidence.Pass, s12GracefulStopPassReason(rec)
	}
}

// s12GracefulStopPassReason is the reason of a pass, from what the run
// recorded.
func s12GracefulStopPassReason(rec *s12GracefulStopRecord) string {
	t, s, l := rec.Trigger, rec.Sampler, rec.Log
	last := "no CUBRID process held a file of the restore in any reading, about every " + samplerInterval.String()
	if s.LastHolder != "" {
		last = fmt.Sprintf("the last file of the restore a CUBRID process held was %s, at %s", s.LastHolder,
			s.LastHolderAt.Format(time.RFC3339Nano))
	}
	attempts := int32(0)
	if rec.AttemptsAfter != nil {
		attempts = *rec.AttemptsAfter
	}
	ended := "without an error"
	if s.EndError != "" {
		ended = fmt.Sprintf("with %q after its last reading at %s, once the shutdown was logged", s.EndError,
			s.LastReadingAt.Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("the sampler showed %q in %s's database container at %s, and that Pod was deleted with its "+
		"grace period, the deletion accepted at %s; the member's log held restoredb's %s, then %s for the restore "+
		"operation %s with %q, then the shutdown's heartbeat stop, in that order, restoredb killed (%q); the sampler "+
		"saw the database directory %s gone at %s and the staging directory gone at %s; none of the %d reading(s) "+
		"it started after the operation was logged Failed showed either directory; no CUBRID process held a file "+
		"of either from the database directory's removal on, nor did restoredb run after it, in any reading, about "+
		"every "+samplerInterval.String()+" (%s); its output ended %s; the Operator ended that attempt with %s "+
		"naming the stop and completed the restore under operation %s, attempt %d, with one retry; the cluster "+
		"reported Ready within restore_limit only after its bootstrap completed; its members and Services held "+
		"exactly the rows acknowledged before the known point and S11's marker, no row written after the backup, "+
		"and took a normal write; the source and the installed manager were unchanged; no credential was in its "+
		"statuses, the Events, the sampler's output, the deleted member's followed log, the replacement's log or "+
		"the installed Operator's manager log", t.SeenProcess, rec.TargetMember,
		t.SeenAt.Format(time.RFC3339Nano), t.DeleteAcceptedAt.Format(time.RFC3339Nano), l.RestoredbResult,
		logOperationFailed, rec.StoppedOperation, l.OperationFailedReason, l.RestoredbErr, s.DatabaseDir,
		s.CleanupSeenAt.Format(time.RFC3339Nano), s.StagingGoneAt.Format(time.RFC3339Nano),
		s.ReadingsAfterOperationFailed, last, ended, s12RetryReason, rec.OperationIDAfter, attempts)
}

// s12GracefulStopSteps runs the steps of S12 graceful-stop in order. Before
// the deletion, the first error ends them. After it, the stop, the cluster
// and the checks at the end are all judged; their errors are joined.
func (h *haRun) s12GracefulStopSteps(ctx context.Context, rec *s12GracefulStopRecord, made *s12GracefulStopMade,
	watch *stopWatch, memberLog *[]byte, result *evidence.Scenario) error {
	started := time.Now().UTC()
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S12 graceful-stop could not tell whether a credential is in a status or an Event", err)
	}
	if h.execStream == nil || h.followLogs == nil {
		return errors.New("S12 graceful-stop needs a streaming exec and a followed log, and this runner has " +
			"neither or only one; nothing was created")
	}
	s11 := h.s11Rec
	if s11 == nil || s11.KnownPoint == nil || s11.Artifact == nil {
		return errors.New("S11 recorded no known point or artifact to restore")
	}
	expected := h.s12Expected
	if expected == "" {
		return errors.New("S12 exact recorded no history at S11's known point")
	}
	rec.KnownPoint, rec.ArtifactURI, rec.ManifestDigest = s11.KnownPoint, s11.Artifact.URI, s11.Artifact.ManifestDigest
	before := h.rec.OperatorAfter
	rec.ManagerBefore = &before
	cluster, err := h.createRestored(ctx, s12GracefulStopClusterName, s12GracefulStopSecretName, rec.SeedPrefix,
		rec.ArtifactURI)
	if err := h.recordGracefulStopMade(ctx, cluster, made, rec, err); err != nil {
		return err
	}
	created := time.Now()
	createdAt := created.UTC()
	rec.ClusterUID, rec.CreatedAt = string(cluster.UID), &createdAt
	rec.Sampler.StagingDir = strings.TrimPrefix(imRestoreStagingRoot, imDataRoot+"/") + "/" + rec.ClusterUID
	if err := h.waitStopTarget(ctx, cluster, created, rec, result); err != nil {
		return err
	}
	sctx, stopStreams := context.WithCancel(ctx)
	defer stopStreams()
	watch.mu.Lock()
	watch.armed = true
	watch.mu.Unlock()
	samplerDone, err := h.startSampler(sctx, rec.TargetMember, restoreSamplerScript(), watch, rec.Sampler)
	if err != nil {
		return err
	}
	logDone, err := h.startMemberLog(sctx, rec.TargetMember, rec.Log, memberLog)
	if err != nil {
		stopStreams()
		<-samplerDone
		return err
	}
	placeErr := h.stopDuringRestoredb(ctx, cluster, created, rec, watch, result)
	deleted := rec.Trigger.DeleteAcceptedAt != nil
	var roles *startingState
	var restoredErr error
	if deleted {
		var readyAt time.Time
		roles, readyAt, restoredErr = h.waitRestored(ctx, cluster, created, &rec.s12Record, result)
		if restoredErr == nil {
			rec.RestoredWithin = readyAt.Sub(created).Round(time.Millisecond).String()
			result.Measurements[restoredWithinMeasurement] = rec.RestoredWithin
		}
	} else {
		// Without a deletion the container runs on, and so do the streams.
		stopStreams()
	}
	awaitStreams(samplerDone, logDone, stopStreams)
	result.Measurements[measSamples] = rec.Sampler.Samples
	h.readStopRetryEvents(ctx, cluster, rec)
	errs := []error{placeErr, restoredErr}
	if deleted {
		errs = append(errs, judgeRestoreStop(watch, *memberLog, rec, result))
	}
	if deleted && placeErr == nil && restoredErr == nil {
		errs = append(errs, h.gracefulStopRestoredChecks(ctx, cluster, expected, roles, rec, result))
	}
	errs = append(errs, h.gracefulStopRestoreCredentials(ctx, rec, watch, *memberLog, started))
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	errs = append(errs, h.checkSourceS12(ctx, rec.Source, s12GracefulStopSourceDataCheckFile))
	errs = append(errs, h.sameManager(ctx, "S12 graceful-stop", rec.ManagerBefore, &rec.ManagerAfter))
	return errors.Join(errs...)
}

// recordGracefulStopMade records the cluster createRestored made, with err
// its error, and reads the Secret it made for it, so that both are deleted
// by their UIDs at the end.
func (h *haRun) recordGracefulStopMade(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	made *s12GracefulStopMade, rec *s12GracefulStopRecord, err error) error {
	secret := &corev1.Secret{}
	if getErr := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: s12GracefulStopSecretName},
		secret); getErr == nil {
		made.secret, rec.SecretUID = secret, string(secret.UID)
	} else if err == nil && !apierrors.IsNotFound(getErr) {
		err = fmt.Errorf("read the Secret %s the run made: %w", s12GracefulStopSecretName, getErr)
	}
	if err != nil {
		return err
	}
	made.cluster = cluster
	return nil
}

// waitStopTarget waits, recording every reading of the cluster as S12 exact
// does, until the restore's target Pod, which the cluster's StatefulSet must
// control, runs its database container, and records the Pod's UID and the
// container's ID. A bootstrap that names another target is an error; one
// read past its restore, or retried, before then has run restoredb before
// the sampler could see it: the placement was missed.
func (h *haRun) waitStopTarget(ctx context.Context, created *databasev1alpha1.CubridCluster, createdAt time.Time,
	rec *s12GracefulStopRecord, result *evidence.Scenario) error {
	var firstPast string
	return h.waitPlaced(ctx, created, createdAt, &rec.s12Record, &firstPast, result,
		"restore target's database container did not run",
		func(cluster *databasev1alpha1.CubridCluster, phase string) (bool, error) {
			boot := cluster.Status.Bootstrap
			if boot.TargetMember != "" && boot.TargetMember != rec.TargetMember {
				return false, fmt.Errorf("the restore's target is %s, not %s", boot.TargetMember, rec.TargetMember)
			}
			if missed := pastRestoredb(phase, boot.Attempts); missed != "" {
				rec.PlacementMissed = missed + " before the sampler started"
				return false, errors.New("placement missed: " + rec.PlacementMissed)
			}
			pod := &corev1.Pod{}
			if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.TargetMember},
				pod); err != nil {
				return false, nil
			}
			owner := metav1.GetControllerOf(pod)
			if owner == nil || owner.Kind != kindStatefulSet || owner.Name != cluster.Name {
				return false, fmt.Errorf("the Pod %s is not controlled by the StatefulSet of %s", pod.Name,
					cluster.Name)
			}
			status := databaseContainerStatus(pod)
			if pod.UID == "" || status == nil || status.State.Running == nil || status.ContainerID == "" {
				return false, nil
			}
			rec.Trigger.PodUID, rec.Trigger.ContainerID = string(pod.UID), status.ContainerID
			return true, nil
		})
}

// pastRestoredb says, for a reading of the bootstrap in phase at attempts,
// why the first attempt's restoredb had ended: the bootstrap seeds the other
// members or completed, or an attempt had already failed. It is empty
// otherwise.
func pastRestoredb(phase string, attempts int32) string {
	switch {
	case phase == string(databasev1alpha1.BootstrapSeedingReplicas) ||
		phase == string(databasev1alpha1.BootstrapComplete):
		return "the bootstrap was read " + phase
	case attempts > 0:
		return fmt.Sprintf("the bootstrap was read %s after %d failed attempt(s)", phase, attempts)
	}
	return ""
}

// stopDuringRestoredb deletes the restore target's Pod once the sampler
// shows restoredb (triggerStop), while it reads and records the cluster,
// until the deletion was accepted or refused. A reading past the first
// attempt's restoredb (pastRestoredb), or a sampler that ended, before the
// sampler showed restoredb disarms the trigger: the placement was missed. It
// then records the reading the deletion followed.
func (h *haRun) stopDuringRestoredb(ctx context.Context, created *databasev1alpha1.CubridCluster,
	createdAt time.Time, rec *s12GracefulStopRecord, watch *stopWatch, result *evidence.Scenario) error {
	t := rec.Trigger
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	triggered := make(chan struct{})
	go func() {
		defer close(triggered)
		select {
		case <-watch.seen:
			h.triggerStop(tctx, watch, rec.TargetMember, t)
		case <-tctx.Done():
		}
	}()
	var firstPast string
	err := h.waitPlaced(ctx, created, createdAt, &rec.s12Record, &firstPast, result,
		"restore was not seen running restoredb on "+rec.TargetMember,
		func(cluster *databasev1alpha1.CubridCluster, phase string) (bool, error) {
			watch.mu.Lock()
			defer watch.mu.Unlock()
			switch {
			case t.DeleteAcceptedAt != nil:
				return true, nil
			case t.DeleteError != "":
				return false, fmt.Errorf("the sampler showed restoredb, but the restore target's Pod could not be "+
					"deleted: %s", t.DeleteError)
			case watch.fired:
				return false, nil
			}
			missed := pastRestoredb(phase, cluster.Status.Bootstrap.Attempts)
			if missed == "" && rec.Sampler.EndedAt != nil {
				missed = "the sampler's output ended"
			}
			if missed == "" {
				return false, nil
			}
			watch.armed = false
			t.Missed = fmt.Sprintf("%s before the sampler showed restoredb in %d reading(s)", missed,
				len(watch.samples))
			return false, errors.New("placement missed: " + t.Missed)
		})
	watch.mu.Lock()
	watch.armed = false
	fired := watch.fired
	watch.mu.Unlock()
	if !fired {
		cancel()
	}
	<-triggered
	if t.Missed != "" {
		rec.PlacementMissed = t.Missed
	}
	if t.SeenAt != nil && t.DeleteAcceptedAt != nil {
		result.Measurements[measDeleteAfterRestoredbSeen] = t.DeleteAcceptedAt.Sub(*t.SeenAt).
			Round(time.Millisecond).String()
	}
	if t.DeleteAcceptedAt != nil {
		for i, p := range rec.Bootstrap {
			if p.At.After(*t.DeleteAcceptedAt) {
				break
			}
			attempts := p.Attempts
			rec.deleteReading, rec.AttemptsAtDelete, rec.OperationAtDelete = i, &attempts, p.OperationID
		}
	}
	return err
}

// readStopRetryEvents records the messages of the Operator's
// RestoreFailedRetrying Events of the cluster (readRetryEvents).
func (h *haRun) readStopRetryEvents(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	rec *s12GracefulStopRecord) {
	r := &s12IMRestart{At: IMRestartAtRestoring, RetryEvents: []string{}}
	h.readRetryEvents(ctx, cluster, r)
	rec.RetryEvents, rec.EventsError = r.RetryEvents, r.EventsError
}

// judgeRestoreStop judges what the target's log, the sampler and the
// cluster's readings showed of the stop. A restoredb that succeeded, or a
// stopping reason without restoredb's failure, shows a stop that reached the
// Instance Manager after restoredb had ended: the placement was missed, and
// nothing else of the stop is judged.
func judgeRestoreStop(watch *stopWatch, memberLog []byte, rec *s12GracefulStopRecord,
	result *evidence.Scenario) error {
	l := rec.Log
	scanRestoreStopLog(memberLog, rec)
	if l.RestoredbDurationMs != nil {
		result.Measurements[measRestoredbDurationMs] = *l.RestoredbDurationMs
	}
	if t := rec.Trigger.DeleteAcceptedAt; l.StopLoggedAt != nil && t != nil {
		l.StopLoggedAfterDelete = l.StopLoggedAt.Sub(*t).Round(time.Millisecond).String()
		result.Measurements[measStopLoggedAfterDelete] = l.StopLoggedAfterDelete
	}
	rec.StoppedOperation = l.OperationID
	if l.RestoredbResult == logRanCommand {
		rec.PlacementMissed = fmt.Sprintf("the member's log shows restoredb succeed (line %d) before the stop "+
			"reached the Instance Manager", l.RestoredbLine)
		return errors.New("placement missed: " + rec.PlacementMissed)
	}
	reasonErr := judgeRestoreStopReason(rec)
	if rec.PlacementMissed != "" {
		return reasonErr
	}
	errs := []error{reasonErr, restoreStopLogGaps(rec), restoredbKilled(l)}
	for _, o := range restoreStopLogOrder(l) {
		event := "the member's log holds " + o
		result.NeverEvents = append(result.NeverEvents, event)
		errs = append(errs, restoreVerdict{errors.New(event)})
	}
	if l.OperationID != "" && rec.OperationAtDelete != "" && l.OperationID != rec.OperationAtDelete {
		errs = append(errs, fmt.Errorf("the member's log names %s as the restore operation the stop ended, and "+
			"the cluster's status named %s before the deletion", l.OperationID, rec.OperationAtDelete))
	}
	errs = append(errs, judgeRestoreStopSamples(watch, rec, result), judgeStopRetries(rec, result))
	return errors.Join(errs...)
}

// scanRestoreStopLog records in rec.Log the first line of each kind the
// variant orders: the end of restoredb from the cluster's staging, the
// first Operation failed of a restore operation, and the shutdown's
// heartbeat stop and server stop, and the first line the stop caused
// (isStopLine).
func scanRestoreStopLog(data []byte, rec *s12GracefulStopRecord) {
	l := rec.Log
	backupDir := " -B " + imRestoreStagingRoot + "/" + rec.ClusterUID + "/backup "
	at := func(v string) *time.Time {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil
		}
		t = t.UTC()
		return &t
	}
	for i, raw := range bytes.Split(data, []byte("\n")) {
		var line stopLogLine
		if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &line) != nil {
			continue
		}
		n := i + 1
		l.Lines++
		ours := strings.HasPrefix(line.Command, restoredbCommand) && strings.Contains(line.Command, backupDir)
		own := l.OperationID != "" && line.OperationID == l.OperationID
		if l.StopLine == 0 && isStopLine(line, ours, own) {
			l.StopLine, l.StopLoggedAt = n, at(line.Time)
		}
		switch {
		case l.RestoredbLine == 0 && ours && (line.Msg == logCommandFailed || line.Msg == logRanCommand):
			l.RestoredbLine, l.RestoredbResult, l.RestoredbDurationMs = n, line.Msg, line.DurationMs
			l.RestoredbAt, l.RestoredbErr, l.RestoredbExitCode = at(line.Time), line.Err, line.ExitCode
		case l.OperationFailedLine == 0 && line.Msg == logOperationFailed &&
			line.Kind == string(instancemanager.OpRestore):
			l.OperationID, l.OperationFailedLine, l.OperationFailedReason = line.OperationID, n, line.Reason
			l.OperationFailedAt = at(line.Time)
		case line.Event != logEventCommand:
		case l.HeartbeatStopLine == 0 && line.Command == heartbeatStop:
			l.HeartbeatStopLine, l.HeartbeatStopResult, l.HeartbeatStopAt = n, line.Msg, at(line.Time)
		case l.ServerStopLine == 0 && strings.HasPrefix(line.Command, serverStop):
			l.ServerStopLine, l.ServerStopResult, l.ServerStopAt = n, line.Msg, at(line.Time)
		}
	}
}

// judgeRestoreStopReason judges the reason of the restore operation's
// Operation failed. restoreStopReasonPrefix is the stop's. A reason of the
// stop alone, or of the stop after restoredb (the start of the restored
// database refused or failed), when the log shows no failed restoredb, is
// a stop that reached the Instance Manager after restoredb had ended: the
// placement was missed. Any other reason fails the variant: the stop landed
// while restoredb ran, and the attempt failed for something else.
func judgeRestoreStopReason(rec *s12GracefulStopRecord) error {
	l := rec.Log
	reason := l.OperationFailedReason
	switch {
	case l.OperationFailedLine == 0, strings.HasPrefix(reason, restoreStopReasonPrefix):
		return nil
	case (reason == errStoppingText || strings.HasPrefix(reason, memberStopping)) &&
		l.RestoredbResult != logCommandFailed:
		rec.PlacementMissed = fmt.Sprintf("the restore operation %s failed with %q, with no failed restoredb in "+
			"the member's log: the stop reached the Instance Manager after restoredb had ended", l.OperationID, reason)
		return errors.New("placement missed: " + rec.PlacementMissed)
	}
	return restoreVerdict{fmt.Errorf("the restore operation %s ended Failed after the restore target's Pod was "+
		"deleted while restoredb ran, but its reason does not start with %q: %q", l.OperationID,
		restoreStopReasonPrefix, reason)}
}

// restoredbKilled requires a restoredb that the log shows fail to have been
// killed: its error says "signal: killed" or its exit code is -1, as when
// the stop's cancellation ended its process group (Run in internal/
// instancemanager/cli.go). The stopping reason alone does not show it:
// stoppingReason prefixes any failure while the member stops. Another
// failure leaves the stop's effect inconclusive and blocks the variant.
func restoredbKilled(l *s12StopLog) error {
	if l.RestoredbResult != logCommandFailed || strings.Contains(l.RestoredbErr, signalKilled) ||
		(l.RestoredbExitCode != nil && *l.RestoredbExitCode == -1) {
		return nil
	}
	code := evidence.Unknown
	if l.RestoredbExitCode != nil {
		code = fmt.Sprint(*l.RestoredbExitCode)
	}
	return fmt.Errorf("restoredb failed (line %d) with exit code %s and %q, not killed: whether the stop ended "+
		"it is inconclusive", l.RestoredbLine, code, l.RestoredbErr)
}

// restoreStopLogGaps names the lines the variant needs that the target's log
// does not hold; any of them missing means the log is incomplete and the
// variant is blocked. server stop is not required: it is checked for order
// when present, but its absence does not block the variant, the same rule the
// backup graceful-stop check applies (stopLogGaps, S11 graceful-stop).
func restoreStopLogGaps(rec *s12GracefulStopRecord) error {
	l := rec.Log
	var missing []string
	if l.RestoredbLine == 0 {
		missing = append(missing, "restoredb's end for "+imRestoreStagingRoot+"/"+rec.ClusterUID)
	}
	if l.OperationFailedLine == 0 {
		missing = append(missing, logOperationFailed+" of a restore operation")
	}
	if l.HeartbeatStopLine == 0 {
		missing = append(missing, "the shutdown's "+heartbeatStop)
	}
	if len(missing) == 0 {
		return nil
	}
	cut := ""
	if l.Truncated {
		cut = ", which reached the run's limit,"
	}
	return fmt.Errorf("the member's followed log%s is incomplete: it holds no %s", cut,
		strings.Join(missing, ", no "))
}

// restoreStopLogOrder names each pair of the lines found that is out of
// order: restoredb's end, then Operation failed, then heartbeat stop and
// server stop.
func restoreStopLogOrder(l *s12StopLog) []string {
	if l.OperationFailedLine == 0 {
		return nil
	}
	var order []string
	if l.RestoredbLine != 0 && l.OperationFailedLine < l.RestoredbLine {
		order = append(order, fmt.Sprintf("%s (line %d) before restoredb's end (line %d)", logOperationFailed,
			l.OperationFailedLine, l.RestoredbLine))
	}
	for _, stop := range []struct {
		command string
		line    int
	}{{heartbeatStop, l.HeartbeatStopLine}, {serverStop, l.ServerStopLine}} {
		if stop.line != 0 && stop.line < l.OperationFailedLine {
			order = append(order, fmt.Sprintf("%s (line %d) before the restore operation's %s (line %d)",
				stop.command, stop.line, logOperationFailed, l.OperationFailedLine))
		}
	}
	return order
}

// judgeRestoreStopSamples finds, in the sampler's readings, the first that
// showed the database directory and the first after it that did not, where
// the restore's cleanup had begun, and requires that no reading from that
// one on shows a CUBRID process holding a file of the database or staging
// directory, and that no reading after it shows restoredb. A holder's line
// comes after the entries in a reading, so a holder in the first reading
// without the directory was seen once the directory was gone; a process's
// line comes before them, so only the readings after it are searched for
// restoredb. The sampler's and the log's times are the container's clock:
// no reading the sampler started after the restore operation was logged
// Failed may show either directory. A database directory never seen gone
// blocks the variant.
func judgeRestoreStopSamples(watch *stopWatch, rec *s12GracefulStopRecord, result *evidence.Scenario) error {
	watch.mu.Lock()
	samples := slices.Clone(watch.samples)
	watch.mu.Unlock()
	s := rec.Sampler
	paths := []string{imDatabasesDir + "/" + databaseName, imRestoreStagingRoot + "/" + rec.ClusterUID}
	seen, gone := findCleanup(samples, s)
	end := len(samples)
	if gone >= 0 {
		end = gone
	}
	visible := false
	for _, sample := range samples[:end] {
		visible = visible || len(sample.Holders) > 0 || len(sample.FdListed) > 0
		if held, _ := restoreHolders(sample, paths); len(held) > 0 {
			at := sample.Received
			s.LastHolder, s.LastHolderAt = held[len(held)-1], &at
		}
	}
	if !visible {
		// No holder and no listing of a cub* process's open files: an empty
		// answer of ls would pass the check below without showing anything.
		return errors.New("the sampler could not see open files: no reading before the restore's cleanup showed " +
			"an open file below " + imDataRoot + " or the open files of a cub* process")
	}
	if gone < 0 {
		why := "its output ended"
		if s.Truncated {
			why = "its output reached the run's limit"
		}
		if seen < 0 {
			return fmt.Errorf("the sampler never showed the database directory %s before %s, so whether a "+
				"process held a file of the restore once its cleanup began is not known", s.DatabaseDir, why)
		}
		return fmt.Errorf("the sampler did not show the database directory %s gone before %s, so whether a "+
			"process held a file of the restore once its cleanup began is not known", s.DatabaseDir, why)
	}
	if t := rec.Trigger; t.DeleteAcceptedAt != nil {
		result.Measurements[measCleanupSeenAfterDelete] = s.CleanupSeenAt.Sub(*t.DeleteAcceptedAt).
			Round(time.Millisecond).String()
	}
	afterCleanup(samples[gone:], paths, s)
	last := samples[len(samples)-1].At
	s.LastReadingAt = &last
	if failed := rec.Log.OperationFailedAt; failed != nil {
		for _, sample := range samples {
			if !sample.At.After(*failed) {
				continue
			}
			s.ReadingsAfterOperationFailed++
			if slices.Contains(sample.Entries, s.DatabaseDir) || slices.Contains(sample.Entries, s.StagingDir) {
				s.PresentAfterOperationFailed = append(s.PresentAfterOperationFailed,
					sample.At.Format(time.RFC3339Nano))
			}
		}
	}
	unjudged := samplerUnjudged(rec)
	result.Measurements[measHoldersAfterCleanup] = len(s.HoldersAfterCleanup)
	cleanup := s.CleanupSeenAt.Format(time.RFC3339Nano)
	var events []string
	if len(s.HoldersAfterCleanup) > 0 {
		events = append(events, fmt.Sprintf("a CUBRID process held a file of the restore at or after %s, when its "+
			"cleanup had begun: %s", cleanup, strings.Join(s.HoldersAfterCleanup, "; ")))
	}
	if len(s.UtilityAfterCleanup) > 0 {
		events = append(events, fmt.Sprintf("restoredb ran after %s, when the restore's cleanup had begun: %s",
			cleanup, strings.Join(s.UtilityAfterCleanup, "; ")))
	}
	if len(s.PresentAfterOperationFailed) > 0 {
		events = append(events, fmt.Sprintf("the sampler showed the restore's database or staging directory in "+
			"readings started after the restore operation was logged %s at %s: %s", logOperationFailed,
			rec.Log.OperationFailedAt.Format(time.RFC3339Nano), strings.Join(s.PresentAfterOperationFailed, ", ")))
	}
	result.NeverEvents = append(result.NeverEvents, events...)
	errs := make([]error, 0, len(events)+1)
	for _, event := range events {
		errs = append(errs, restoreVerdict{errors.New(event)})
	}
	return errors.Join(append(errs, unjudged)...)
}

// samplerUnjudged says what keeps the sampler from showing the order of the
// cleanup and the record, which then blocks the variant:
//   - an Operation failed line whose time could not be read;
//   - no reading started after that time: no reading could show a directory
//     left after the record, so its absence shows nothing;
//   - a staging directory never seen gone;
//   - output that reached the run's limit, whose later readings were not
//     read;
//   - output that ended with an error before the container's stop. The exec
//     session ends with the container, and kubectl may report that end as an
//     error, so an error is accepted only when the last reading started at
//     or after the time of the shutdown's last command logged (server stop,
//     else heartbeat stop): every reading the variant needs was taken, and
//     the container was being stopped. An error before that may have cut
//     readings off, and blocks.
func samplerUnjudged(rec *s12GracefulStopRecord) error {
	s, l := rec.Sampler, rec.Log
	var why []string
	switch {
	case l.OperationFailedLine != 0 && l.OperationFailedAt == nil:
		why = append(why, fmt.Sprintf("the time of the restore operation's %s (line %d) could not be read",
			logOperationFailed, l.OperationFailedLine))
	case l.OperationFailedAt != nil && s.ReadingsAfterOperationFailed == 0:
		why = append(why, fmt.Sprintf("no sampler reading started after the restore operation was logged %s at %s",
			logOperationFailed, l.OperationFailedAt.Format(time.RFC3339Nano)))
	}
	if s.StagingGoneAt == nil {
		why = append(why, "the sampler never showed the staging directory "+s.StagingDir+" gone")
	}
	if s.Truncated {
		why = append(why, "the sampler's output reached the run's limit")
	}
	if s.EndError != "" {
		stopped := cmp.Or(l.ServerStopAt, l.HeartbeatStopAt)
		if stopped == nil || s.LastReadingAt == nil || s.LastReadingAt.Before(*stopped) {
			why = append(why, fmt.Sprintf("the sampler's output ended with %q before a reading at or after the "+
				"shutdown's last logged command", s.EndError))
		}
	}
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("whether the restore's directories were gone before the operation was recorded Failed is "+
		"not known: %s", strings.Join(why, "; "))
}

// findCleanup records in s, from the readings in order, the first that
// showed the database directory, the first after it that did not, and the
// first without the staging directory after one that showed it, and
// returns the indexes of the first two, -1 when there is none.
func findCleanup(samples []stopSample, s *s12StopSampler) (seen, gone int) {
	seen, gone = -1, -1
	stagingSeen := false
	for i, sample := range samples {
		at := sample.Received
		present := slices.Contains(sample.Entries, s.DatabaseDir)
		if seen < 0 && present {
			seen, s.DatabaseDirSeenAt = i, &at
		}
		if seen >= 0 && !present && gone < 0 {
			gone, s.CleanupSeenAt = i, &at
		}
		switch staged := slices.Contains(sample.Entries, s.StagingDir); {
		case staged:
			stagingSeen = true
		case stagingSeen && s.StagingGoneAt == nil:
			s.StagingGoneAt = &at
		}
	}
	return seen, gone
}

// afterCleanup records in s, from the readings from the first one without
// the database directory on, the holders of a file at or below paths, by
// whether they are CUBRID processes, and the restoredb processes of the
// readings after that first one.
func afterCleanup(samples []stopSample, paths []string, s *s12StopSampler) {
	for i, sample := range samples {
		at := sample.Received.Format(time.RFC3339Nano)
		held, other := restoreHolders(sample, paths)
		for _, h := range held {
			s.HoldersAfterCleanup = append(s.HoldersAfterCleanup, at+" "+h)
		}
		for _, h := range other {
			s.OtherHoldersAfterCleanup = append(s.OtherHoldersAfterCleanup, at+" "+h)
		}
		if p := sample.utilityProcess(restoredbUtility); i > 0 && p != "" {
			s.UtilityAfterCleanup = append(s.UtilityAfterCleanup, at+" "+p)
		}
	}
}

// restoreHolders returns the open files of a reading at or below one of
// paths, as "<pid> <path>": those of the reading's cub* processes, and
// those of any other process. A file removed while held ends with
// " (deleted)".
func restoreHolders(sample stopSample, paths []string) (cubrid, other []string) {
	pids := make([]string, 0, len(sample.Processes))
	for _, p := range sample.Processes {
		pid, _, _ := strings.Cut(p, " ")
		pids = append(pids, pid)
	}
	for _, holder := range sample.Holders {
		pid, path, _ := strings.Cut(holder, " ")
		path = strings.TrimSuffix(path, " (deleted)")
		if !slices.ContainsFunc(paths, func(p string) bool { return path == p || strings.HasPrefix(path, p+"/") }) {
			continue
		}
		if slices.Contains(pids, pid) {
			cubrid = append(cubrid, holder)
		} else {
			other = append(other, holder)
		}
	}
	return cubrid, other
}

// judgeStopRetries judges the cluster's readings after the deletion, as S12
// instance-manager-restart judges them after its kill
// (scanIMRestartBootstrap): what must never happen, attempts that rose
// without RestoreFailedRetrying, and, once the cluster completed, exactly
// one retry from the attempt at the deletion whose message holds
// restoreStopReasonPrefix, and another operation than the one the stop
// ended at the end. A second retry is a restore that failed again and fails
// the variant; no retry, or a first retry for another reason, leaves the
// stop's effect unobserved and blocks it.
func judgeStopRetries(rec *s12GracefulStopRecord, result *evidence.Scenario) error {
	stopped := cmp.Or(rec.StoppedOperation, rec.OperationAtDelete)
	r := &s12IMRestart{At: IMRestartAtRestoring, cause: restoreStopReasonPrefix, restartReading: rec.deleteReading,
		RetryEvents: rec.RetryEvents}
	r.KilledAt, r.AttemptsBefore, r.OperationIDBefore = rec.Trigger.DeleteAcceptedAt, rec.AttemptsAtDelete, stopped
	events, unexplained, afterStop := scanIMRestartBootstrap(&s12IMRestartRecord{s12Record: rec.s12Record,
		IMRestart: r})
	rec.Reasons, rec.Attempts, rec.Retries, rec.RetriesAfterStop = r.Reasons, r.Attempts, r.Retries,
		append([]s12Retry{}, afterStop...)
	rec.OperationIDAfter, rec.AttemptsAfter = r.OperationIDAfter, r.AttemptsAfter
	result.Measurements[measRetriesAfterStop] = len(afterStop)
	if stopped != "" && rec.OperationIDAfter != "" {
		result.Measurements[measOperationIDChanged] = stopped != rec.OperationIDAfter
	}
	result.NeverEvents = append(result.NeverEvents, events...)
	errs := make([]error, 0, len(events)+2)
	for _, event := range events {
		errs = append(errs, restoreVerdict{errors.New(event)})
	}
	if len(unexplained) > 0 {
		errs = append(errs, fmt.Errorf("the restored cluster's bootstrap attempts rose %s, and no reading showed the "+
			"reason %s: the cause is unknown", strings.Join(unexplained, ", "), s12RetryReason))
	}
	if rec.deleteReading < 0 {
		errs = append(errs, errors.New("no reading of the cluster was recorded before the deletion, so the "+
			"attempt it ended is not known"))
	}
	// The retries are judged on a restore that completed; one that did not
	// is judged by the wait for it.
	if rec.RestoredWithin == "" || rec.deleteReading < 0 {
		return errors.Join(errs...)
	}
	switch {
	case len(afterStop) == 0 && rec.OperationIDAfter == stopped:
		errs = append(errs, fmt.Errorf("the stop did not end the restore: operation %s completed with no failed "+
			"attempt after the deletion", stopped))
	case len(afterStop) == 0:
		errs = append(errs, fmt.Errorf("the restore completed under operation %s, and no reading or Event of the "+
			"Operator showed the attempt the stop ended end with %s", rec.OperationIDAfter, s12RetryReason))
	case len(afterStop) > 1:
		var failed []string
		for _, retry := range afterStop[1:] {
			failed = append(failed, retry.Message)
		}
		errs = append(errs, restoreVerdict{fmt.Errorf("the restore failed again after the attempt the stop "+
			"ended: %s", strings.Join(failed, "; "))})
	case !strings.Contains(afterStop[0].Message, restoreStopReasonPrefix):
		errs = append(errs, fmt.Errorf("the attempt the stop ended ended with %q, not with the stopping reason "+
			"%q: whether the stop ended it was not observed", afterStop[0].Message, restoreStopReasonPrefix))
	case rec.OperationIDAfter == stopped:
		errs = append(errs, restoreVerdict{fmt.Errorf("the operation %s the stop ended is the one that completed "+
			"after the next attempt started", stopped)})
	}
	return errors.Join(errs...)
}

// gracefulStopRestoredChecks makes, once the cluster is Ready, the checks of
// S12 exact (s12Steps) and reads the target's replacement, in that order;
// the first error ends them.
func (h *haRun) gracefulStopRestoredChecks(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	expected string, roles *startingState, rec *s12GracefulStopRecord, result *evidence.Scenario) error {
	if err := h.stopTargetReplaced(ctx, cluster, rec); err != nil {
		return err
	}
	if err := h.checkRestoredSecrets(ctx, &rec.s12Record, result); err != nil {
		return err
	}
	if err := h.checkHANodes(ctx, roles, &rec.s12Record, result); err != nil {
		return err
	}
	if err := h.checkRestoredData(ctx, expected, roles, &rec.s12Record, result,
		s12GracefulStopDataCheckFile); err != nil {
		return err
	}
	if err := h.normalWriteS12(ctx, roles, &rec.s12Record, s12GracefulStopMarker); err != nil {
		return err
	}
	final := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), final); err != nil {
		return fmt.Errorf("read the restored cluster at the end: %w", err)
	}
	recordBootstrap(final, &rec.Bootstrap)
	rec.Credentials.StatusesChecked++
	return h.lookForCredentials(&rec.Credentials.FoundIn, "the restored cluster's status at the end", final.Status)
}

// stopTargetReplaced reads the restore's target at the end and records it
// as the deleted Pod's replacement: a Pod of the cluster's StatefulSet with
// another UID than the deleted one.
func (h *haRun) stopTargetReplaced(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	rec *s12GracefulStopRecord) error {
	t := rec.Trigger
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.TargetMember},
		pod); err != nil {
		return fmt.Errorf("read the restore's target %s at the end: %w", rec.TargetMember, err)
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != kindStatefulSet || owner.Name != cluster.Name {
		return fmt.Errorf("the Pod %s at the end is not controlled by the StatefulSet of %s", pod.Name,
			cluster.Name)
	}
	if string(pod.UID) == t.PodUID {
		return fmt.Errorf("the restore's target %s is still the Pod %s whose deletion was accepted", pod.Name,
			t.PodUID)
	}
	at := time.Now().UTC()
	t.ReplacementUID, t.ReplacementAt = string(pod.UID), &at
	return nil
}

// gracefulStopRestoreCredentials looks for the object store's keys in every
// Event of the run namespace, the sampler's output, the followed log of the
// deleted target, the replacement's log, whole, and each installed manager
// Pod's log from logSinceMargin before started; the statuses were searched
// as they were read. A stream cut at the run's limit, and a log that could
// not be read, leave the search incomplete.
func (h *haRun) gracefulStopRestoreCredentials(ctx context.Context, rec *s12GracefulStopRecord, watch *stopWatch,
	memberLog []byte, started time.Time) error {
	creds := rec.Searched
	var errs []error
	events := &corev1.EventList{}
	if err := h.Client.List(ctx, events, client.InNamespace(h.cfg.Namespace)); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: list the Events: %w", err))
	} else {
		for _, event := range events.Items {
			creds.EventsChecked++
			if err := h.lookForCredentials(&creds.FoundIn, "Event "+event.Name, event); err != nil {
				errs = append(errs, err)
				break
			}
		}
	}
	watch.mu.Lock()
	sampled := slices.Clone(watch.output.Bytes())
	watch.mu.Unlock()
	errs = append(errs, h.searchStreams(creds,
		searchedStream{"the sampler's output in " + rec.TargetMember, sampled, rec.Sampler.Truncated},
		searchedStream{"the followed log of the deleted target " + rec.TargetMember, memberLog,
			rec.Log.Truncated})...)
	if creds.Logs == nil {
		creds.Logs = []s11LogCheck{}
	}
	since := started.Add(-logSinceMargin).Truncate(time.Second)
	managers := h.rec.OperatorAfter.Pods
	if len(managers) == 0 {
		errs = append(errs, errors.New("no installed manager Pod was recorded, so the Operator's log was not "+
			"searched"))
	}
	for _, m := range managers {
		errs = append(errs, h.searchLog(ctx, creds, s11LogCheck{Log: operatorLog, Namespace: h.cfg.OperatorNamespace,
			Pod: m.Pod, UID: m.UID, Container: managerContainer, ReadFrom: logFromSinceTime, SinceTime: since}, nil))
	}
	if uid := rec.Trigger.ReplacementUID; uid != "" {
		errs = append(errs, h.searchLog(ctx, creds, s11LogCheck{Log: instanceManagerLog, Namespace: h.cfg.Namespace,
			Pod: rec.TargetMember, UID: uid, Container: databaseContainer, ReadFrom: logWhole},
			func(pod *corev1.Pod) error {
				owner := metav1.GetControllerOf(pod)
				if owner == nil || owner.Kind != kindStatefulSet || owner.Name != rec.Cluster {
					return fmt.Errorf("the Pod is not controlled by the StatefulSet of %s", rec.Cluster)
				}
				return nil
			}))
	}
	return errors.Join(errs...)
}

// deleteS12GracefulStop deletes the cluster and the Secret the run created,
// in that order and each by its UID (deleteByUID), and records each
// outcome. It does not judge the variant.
func (h *haRun) deleteS12GracefulStop(ctx context.Context, made s12GracefulStopMade,
	rec *s12GracefulStopRecord) bool {
	var objects []ownedObject
	if made.cluster != nil && made.cluster.UID != "" {
		objects = append(objects, ownedObject{ownedClusterKind, made.cluster})
	}
	if made.secret != nil && made.secret.UID != "" {
		objects = append(objects, ownedObject{"Secret", made.secret})
	}
	if len(objects) == 0 {
		return true
	}
	outcome, cleaned := h.deleteByUID(ctx, objects)
	rec.Cleanup = outcome
	return cleaned
}
