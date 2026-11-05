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
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

const (
	s12IMRestartLane    = "lab-installed-ha-s12-instance-manager-restart"
	s12IMRestartVariant = "instance-manager-restart"
	// s12IMRestartClusterName is the cluster restored while the Instance
	// Manager of its target member restarts, in the run namespace, and
	// s12IMRestartSecretName its own copy of the seed Secret.
	s12IMRestartClusterName = "ha-imrestart"
	s12IMRestartSecretName  = "object-storage-imrestart"
	// s12IMRestartMarker names the marker row written through that cluster;
	// its value is the run ID.
	s12IMRestartMarker = "s12imr"
	// s12IMRestartedReason is the failure reason the Instance Manager records,
	// when it starts, for every operation of its store that had not ended
	// (reasonRestarted in internal/instancemanager/opstore.go). The Operator
	// carries it into the message of RestoreFailedRetrying.
	s12IMRestartedReason = "manager restarted while operation was in progress"
	// s12OperationMissingReason is the BootstrapReady reason of a recovery
	// whose target answered that it holds no record of the recorded
	// operation (restoreOperationMissing in internal/controller/bootstrap.go).
	s12OperationMissingReason = "RecordedOperationMissing"
	// s12MaxAttempts is maxBootstrapAttempts (internal/controller/
	// habootstrap.go): restoreAttemptFailed raises attempts only while
	// attempts+1 is below it, so attempts stays below it.
	s12MaxAttempts = 3
	// sigkillExitCode is the status the entrypoint shell exits with when its
	// manager was killed by SIGKILL, 128 + 9; oomKilledReason is the kubelet's
	// reason for a container the kernel killed for memory, with the same code.
	sigkillExitCode = 137
	oomKilledReason = "OOMKilled"
	// imBinary is the Instance Manager the entrypoint starts as its child,
	// with no argument (IM_BIN in build/instance-manager/entrypoint.sh).
	imBinary = "/usr/local/bin/instance-manager"
	// imKillRefused starts the answer of imKillScript when it did not find
	// exactly one Instance Manager process, and killed nothing.
	imKillRefused = "instance-manager processes:"
	// s12SeedRetryReason is the BootstrapReady reason of a failed seed restore
	// that the Operator starts again under the next attempt of the database's
	// bootstrap steps, s12SeedBackupRetryReason that of a failed seed backup
	// (bootstrapStepFailed with SeedRestoreFailed and SeedBackupFailed in
	// internal/controller/habootstrap.go).
	s12SeedRetryReason       = "SeedRestoreFailedRetrying"
	s12SeedBackupRetryReason = "SeedBackupFailedRetrying"
	// imOperationsDir is where a member's Instance Manager keeps its
	// operation records, one <id>.json each (IM_OPERATIONS_DIR,
	// operationsDir in internal/controller/cubridcluster_controller.go), and
	// imRestoreStagingRoot the root below which a recovery restore stages the
	// backup, in a directory named after the cluster's UID
	// (restoreStagingRoot and reconcileRecovery in internal/controller/
	// bootstrap.go).
	imOperationsDir      = "/var/lib/cubrid/operations"
	imRestoreStagingRoot = "/var/lib/cubrid/restore-staging"
	// imStagingLine starts the last line of killedOperationScript's answer.
	imStagingLine = "staging: "
)

// Placements of the kill in S12 instance-manager-restart
// (-restore-instance-manager-restart-at).
const (
	// IMRestartAtRestoring kills the restore's target member's Instance
	// Manager after a reading of the bootstrap Restoring with an operation.
	IMRestartAtRestoring = "restoring"
	// IMRestartAtPreparing kills it after a reading of the bootstrap
	// Preparing with an operation: the Operator has read the operation no
	// further than Pending, the state its start answers, or Downloading.
	IMRestartAtPreparing = "preparing"
	// IMRestartAtSeeding kills the Instance Manager of the member being
	// seeded after a reading of the bootstrap SeedingReplicas with a seed
	// restore recorded for that member.
	IMRestartAtSeeding = "seeding"
	// IMRestartAtStarting kills the restore's target member's Instance
	// Manager at the first reading of the target's own record of the
	// bootstrap's operation in the state Starting: restoredb has succeeded and
	// the restored database is being started.
	IMRestartAtStarting = "starting"
	// IMRestartAtSeedBackup kills the Instance Manager of the seed backup's
	// source, the member the restore restored, while its seed backup for the
	// first peer runs: after a reading of SeedingReplicas with a seed backup
	// and no seed restore recorded, and a reading of the source's own record
	// of that backup that has not ended.
	IMRestartAtSeedBackup = "seed-backup"
)

// Measurements of the retries after a fault during a recovery bootstrap,
// which S12 instance-manager-restart and graceful-stop take.
const (
	measRetriesAfterKill            = "retriesAfterKill"
	measSeedRetriesAfterKill        = "seedRetriesAfterKill"
	measSeedAttemptsAdded           = "seedAttemptsAdded"
	measSeedRestoreOperationChanged = "seedRestoreOperationChanged"
)

// imRestartSummaryNotes are the summary's notes of a placement besides the
// lane's: at seed-backup, the run namespace's Pod Security.
var imRestartSummaryNotes = map[string][]string{
	IMRestartAtSeedBackup: {"The run namespace enforces Pod Security privileged, with warn and audit at " +
		"restricted, so that the rate limit's ephemeral container is admitted"},
}

// imRestartPlacementNote says, for the summary, what is killed when.
var imRestartPlacementNote = map[string]string{
	IMRestartAtRestoring: "the Instance Manager of the restore's target member is killed during its restore",
	IMRestartAtPreparing: "the Instance Manager of the restore's target member is killed while its bootstrap is " +
		"Preparing with an operation",
	IMRestartAtSeeding: "the Instance Manager of a member being seeded from the restored member is killed during " +
		"its seed restore",
	IMRestartAtStarting: "the Instance Manager of the restore's target member is killed while the restored " +
		"database is being started",
	IMRestartAtSeedBackup: "the Instance Manager of the restored member is killed while it takes the seed " +
		"backup for a peer",
}

// imRestartAt is the selected placement of the kill.
func (c HAConfig) imRestartAt() string {
	return cmp.Or(c.RestoreInstanceManagerRestartAt, IMRestartAtRestoring)
}

// imRestartVariant is the variant of the placement at: the placement
// restoring keeps the name of the variant it was first.
func imRestartVariant(at string) string {
	if at == IMRestartAtRestoring {
		return s12IMRestartVariant
	}
	return s12IMRestartVariant + "-" + at
}

// imRestartLane is the lane of the placement at.
func imRestartLane(at string) string {
	if at == IMRestartAtRestoring {
		return s12IMRestartLane
	}
	return s12IMRestartLane + "-" + at
}

// imKillScript kills the Instance Manager of a member's database container
// with SIGKILL, as a crash would end it. The image's entrypoint shell stays
// PID 1 and runs the manager as its child, /usr/local/bin/instance-manager
// with no argument; its other uses of that binary (wait-volume-free,
// shutdown) carry one (build/instance-manager/entrypoint.sh). The script
// kills only the one process whose parent is PID 1 and whose command line is
// exactly imBinary, and kills nothing when it finds none or more than one.
// It runs as the container's user, the manager's own. Once the manager is
// gone the shell exits with its status, and the container ends.
const imKillScript = imFindScript + `kill -KILL "$1" && echo "killed instance-manager $1"`

// imFindScript is the part of imKillScript that finds the Instance Manager:
// it leaves its process ID in $1, or ends the script with imKillRefused.
const imFindScript = `found=""; ` +
	`for d in /proc/[0-9]*; do ` +
	`s=$(cat "$d/stat" 2>/dev/null) || continue; read -r -a st <<< "${s##*) }"; [ "${st[1]}" = 1 ] || continue; ` +
	`mapfile -d '' -t argv < "$d/cmdline" 2>/dev/null || continue; ` +
	`[ "${#argv[@]}" = 1 ] && [ "${argv[0]}" = ` + imBinary + ` ] || continue; ` +
	`found="$found ${d#/proc/}"; done; set -- $found; ` +
	`if [ "$#" != 1 ]; then echo "` + imKillRefused + `${found:- none}"; exit 3; fi; `

// s12Attempt is one value of status.bootstrap.attempts the readings showed,
// with the operations they named under it, in order.
type s12Attempt struct {
	Attempt    int32    `json:"attempt"`
	Operations []string `json:"operations"`
}

// memberKill is the kill of a member's Instance Manager (instanceManagerKill)
// and the restart of its database container that follows.
type memberKill struct {
	// Pod is the member killed; PodUID is its UID, and ContainerBefore,
	// RestartsBefore and StartedBefore its database container, all of the
	// reading right before the kill.
	Pod             string     `json:"pod,omitempty"`
	PodUID          string     `json:"podUID,omitempty"`
	ContainerBefore string     `json:"containerIDBefore,omitempty"`
	RestartsBefore  *int32     `json:"restartCountBefore,omitempty"`
	StartedBefore   *time.Time `json:"containerStartedBefore,omitempty"`
	// KilledAt is when the kill was issued, KillAnswer what the command
	// answered: the container may end before it does.
	KilledAt   *time.Time `json:"killedAt,omitempty"`
	KillAnswer string     `json:"killAnswer,omitempty"`
	// EndedAt is the reading, by the run's clock, that saw the container
	// ended or restarted; ExitCode and FinishedAt are the kubelet's for the
	// ended container.
	EndedAt    *time.Time `json:"containerEndedAt,omitempty"`
	ExitCode   *int32     `json:"exitCode,omitempty"`
	ExitReason string     `json:"exitReason,omitempty"`
	FinishedAt *time.Time `json:"containerFinishedAt,omitempty"`
	// EndedOtherwise says how the container ended when that was not the
	// kill's end, exit code 137 without OOMKilled.
	EndedOtherwise string `json:"endedOtherwise,omitempty"`
	// readsAfterKill counts the reads of the Pod that succeeded while the run
	// waited for the container, lastReadErr is the last that failed.
	readsAfterKill int
	lastReadErr    error
	// ContainerAfter, RestartsAfter and StartedAfter are of the container
	// that ran again, BackAt the reading that saw it running, and
	// RestartsAtEnd the restart count at the end.
	ContainerAfter string     `json:"containerIDAfter,omitempty"`
	RestartsAfter  *int32     `json:"restartCountAfter,omitempty"`
	StartedAfter   *time.Time `json:"containerStartedAfter,omitempty"`
	BackAt         *time.Time `json:"backAt,omitempty"`
	BackWithin     string     `json:"backWithin,omitempty"`
	RestartsAtEnd  *int32     `json:"restartCountAtEnd,omitempty"`
	// PodReplaced is set when the killed member's Pod got another UID: not
	// the fault the variants judge.
	PodReplaced bool `json:"podReplaced,omitempty"`
}

// recordBefore records pod, read right before the kill, and its database
// container, which must be running.
func (k *memberKill) recordBefore(pod *corev1.Pod) error {
	status := databaseContainerStatus(pod)
	if status == nil || status.State.Running == nil || status.ContainerID == "" {
		return fmt.Errorf("the database container of %s is not running", pod.Name)
	}
	restarts, started := status.RestartCount, status.State.Running.StartedAt.UTC()
	k.Pod, k.PodUID, k.ContainerBefore, k.RestartsBefore, k.StartedBefore = pod.Name, string(pod.UID),
		status.ContainerID, &restarts, &started
	return nil
}

// s12IMRestart is the Instance Manager's restart on the restore's target
// member, or on the member being seeded, and the operations around it.
type s12IMRestart struct {
	// At is the placement of the kill; variant is the scenario variant it
	// selects.
	At      string `json:"at"`
	variant string
	// cause is the text the message of the retry the fault caused must
	// hold: s12IMRestartedReason when empty. S12 graceful-stop sets it to
	// the stopping reason (restoreStopReasonPrefix).
	cause string
	// PhaseFirstPastPreparing is the bootstrap phase of the first reading
	// that was neither Preparing nor without a phase: Restoring when the
	// kill could be placed during the restore.
	PhaseFirstPastPreparing string `json:"phaseFirstPastPreparing,omitempty"`
	// memberKill is the kill: Pod is the member killed, the restore's target
	// or, at seeding, the member being seeded.
	memberKill
	// PhaseAtRestart, OperationIDBefore, AttemptsBefore and MessageAtRestart
	// are of the reading of the cluster right before the kill, which says
	// whether restoredb ran or the restored database was being started;
	// PhaseAfterRestart is of the first reading after it.
	PhaseAtRestart    string `json:"phaseAtRestart,omitempty"`
	OperationIDBefore string `json:"operationIDBefore,omitempty"`
	AttemptsBefore    *int32 `json:"attemptsBefore,omitempty"`
	MessageAtRestart  string `json:"messageAtRestart,omitempty"`
	PhaseAfterRestart string `json:"phaseAfterRestart,omitempty"`
	// restartReading is the index in the record's bootstrap of the reading
	// right before the kill, -1 until it is taken.
	restartReading int
	// Reasons are the BootstrapReady reasons of the readings after the kill,
	// each change once, in order. Attempts are the attempts and operations
	// every reading showed; OperationIDAfter and AttemptsAfter are of the
	// last reading that named an operation; Retries are the attempts the
	// status showed ending.
	Reasons          []string     `json:"reasonsAfterKill"`
	Attempts         []s12Attempt `json:"attempts"`
	OperationIDAfter string       `json:"operationIDAfter,omitempty"`
	AttemptsAfter    *int32       `json:"attemptsAfter,omitempty"`
	Retries          []s12Retry   `json:"retries"`
	// RetryEvents are the messages of the Operator's RestoreFailedRetrying
	// Events of the cluster, read once it was Ready; EventsError is why they
	// could not be read.
	RetryEvents []string `json:"retryEvents"`
	EventsError string   `json:"eventsError,omitempty"`
	// retriesAfterKill counts the retries after the kill.
	retriesAfterKill int
	// At seeding: SeedRestoreOperationIDBefore and SeedAttemptsBefore are the
	// killed member's seed restore and the database's attempts of the reading
	// right before the kill; SeedRestoreOperationIDAfter and SeedAttemptsAfter
	// are of the last reading after the kill that recorded a seed restore of
	// that member under a later attempt; SeedRetries are the seed steps the
	// status showed ending, and SeedRetryEvents the messages of the
	// Operator's SeedRestoreFailedRetrying Events of the cluster.
	SeedRestoreOperationIDBefore string     `json:"seedRestoreOperationIDBefore,omitempty"`
	SeedAttemptsBefore           *int32     `json:"seedAttemptsBefore,omitempty"`
	SeedRestoreOperationIDAfter  string     `json:"seedRestoreOperationIDAfter,omitempty"`
	SeedAttemptsAfter            *int32     `json:"seedAttemptsAfter,omitempty"`
	SeedRetries                  []s12Retry `json:"seedRetries,omitempty"`
	SeedRetryEvents              []string   `json:"seedRetryEvents,omitempty"`
	// At seed-backup: SeedPeer is the member the seed backup was for, and
	// SeedBackupOperationIDBefore the seed backup of the reading right before
	// the kill; SeedBackupOperationIDAfter is of the last reading after the
	// kill that recorded a seed backup for that member under a later attempt.
	// KilledSeedBackup is the source's record of the killed seed backup, read
	// once the cluster was Ready.
	SeedPeer                    string              `json:"seedPeer,omitempty"`
	SeedBackupOperationIDBefore string              `json:"seedBackupOperationIDBefore,omitempty"`
	SeedBackupOperationIDAfter  string              `json:"seedBackupOperationIDAfter,omitempty"`
	KilledSeedBackup            *s12KilledOperation `json:"killedSeedBackup,omitempty"`
	// At seed-backup the source's TCP traffic to the object store passes a
	// rate limit (Throttle) from before seeding until after the kill, so that
	// the seed backup's upload lasts long enough to be killed during it.
	// PodSecurity are the run namespace's Pod Security labels as read before
	// anything was created; UploadBaseline are the limit's counters at the
	// first reading of the seed backup's record that had not ended, AtKill at
	// the reading that placed the kill, and SentBeforeKill the bytes between
	// them.
	PodSecurity    map[string]string      `json:"podSecurity,omitempty"`
	Throttle       *s11Throttle           `json:"throttle,omitempty"`
	UploadBaseline *netfault.BandCounters `json:"uploadBaseline,omitempty"`
	AtKill         *netfault.BandCounters `json:"atKill,omitempty"`
	SentBeforeKill uint64                 `json:"sentBeforeKill,omitempty"`
	// limit holds, at seed-backup, the rate limit and whether it was removed.
	limit *s11InterruptedMade
	// seedRetriesAfterKill counts the seed retries after the kill.
	seedRetriesAfterKill int
	// KilledOperation is, at preparing, the target's record of the operation
	// at the kill, read once the cluster was Ready.
	KilledOperation *s12KilledOperation `json:"killedOperation,omitempty"`
	// At starting: OperationReadings are the readings of the target's record
	// of the bootstrap's operation before the kill, each change of state once,
	// and PlacingReading is the one that placed the kill. CompletingOperation
	// is the target's record of the operation that completed the restore,
	// read once the cluster was Ready.
	OperationReadings   []s12OperationReading `json:"operationReadings,omitempty"`
	PlacingReading      *s12OperationReading  `json:"placingReading,omitempty"`
	CompletingOperation *s12KilledOperation   `json:"completingOperation,omitempty"`
}

// s12OperationReading is one reading of an Instance Manager's record of an
// operation: when it was read, by the run's clock, and the state it held.
type s12OperationReading struct {
	At        time.Time `json:"at"`
	Operation string    `json:"operation"`
	State     string    `json:"state"`
}

// s12KilledOperation is what the target's Instance Manager recorded of the
// operation the kill interrupted, and whether the recovery's staging
// directory was left on the target.
type s12KilledOperation struct {
	State         string `json:"state,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	// Restored is the manifest of the restored artifact the record holds:
	// set only once restoredb had succeeded (Operation.Restored in
	// internal/instancemanager/operation.go).
	Restored       string `json:"restored,omitempty"`
	StagingPresent *bool  `json:"stagingPresent,omitempty"`
	ReadError      string `json:"readError,omitempty"`
}

// file is the path of the evidence file name of the variant.
func (r *s12IMRestart) file(name string) string { return "S12/" + r.variant + "/" + name }

// s12IMRestartRecord is the evidence of S12 instance-manager-restart besides
// the data checks and the timeline.
type s12IMRestartRecord struct {
	s12Record
	IMRestart *s12IMRestart `json:"instanceManagerRestart,omitempty"`
}

// validateS12InstanceManagerRestart checks the inputs of S12
// instance-manager-restart, when it is selected.
func (c HAConfig) validateS12InstanceManagerRestart() error {
	switch c.RestoreInstanceManagerRestartAt {
	case "", IMRestartAtRestoring, IMRestartAtPreparing, IMRestartAtSeeding, IMRestartAtStarting,
		IMRestartAtSeedBackup:
	default:
		return fmt.Errorf("-restore-instance-manager-restart-at must be %s, %s, %s, %s or %s", IMRestartAtRestoring,
			IMRestartAtPreparing, IMRestartAtSeeding, IMRestartAtStarting, IMRestartAtSeedBackup)
	}
	if !c.RestoreInstanceManagerRestart {
		if c.imRestartAt() != IMRestartAtRestoring {
			return errors.New("-restore-instance-manager-restart-at needs -restore-instance-manager-restart")
		}
		return nil
	}
	if !c.Restore {
		return errors.New("-restore-instance-manager-restart needs -restore: S12 instance-manager-restart follows " +
			"S12 exact")
	}
	if c.RestoreTampered || c.RestoreWrongTarget || c.RestoreOperatorRestart {
		return errors.New("-restore-instance-manager-restart, -restore-tampered, -restore-wrong-target and " +
			"-restore-operator-restart select different lanes; a run takes one of them")
	}
	return nil
}

// runS12InstanceManagerRestart runs S12 instance-manager-restart after S12
// exact passed: a new cluster is restored from S11's artifact, and once a
// reading shows its bootstrap Restoring with an operation, the Instance
// Manager on the restore's target member is killed, and with it the
// member's database container, which the kubelet starts again on the same
// Pod and volume. The Instance Manager fails, at its start, every operation
// of its store on that volume that had not ended, with
// s12IMRestartedReason; the Operator, polling the recorded operation, then
// ends the attempt with RestoreFailedRetrying and starts the next one,
// before which the Instance Manager removes what its own failed restore
// left or takes over the data it had recorded as restored (ADR-0008). So
// exactly one retry, naming that reason, must follow, under a new operation,
// and the cluster must then pass as S12 exact's does, within RestoreLimit of
// its creation. A kill that cannot be placed during the restore, a run that
// ends, or a check that cannot be made, before S12 instance-manager-restart
// is judged blocks it; what was seen to break stays a failure.
func (h *haRun) runS12InstanceManagerRestart(ctx context.Context) (result evidence.Scenario) {
	at := h.cfg.imRestartAt()
	variant := imRestartVariant(at)
	defer h.inScenario(s12ID+"/"+variant, &result)()
	result = evidence.Scenario{ID: s12ID, Variant: variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s12IMRestartRecord{s12Record: s12Record{Cluster: s12IMRestartClusterName,
		SeedPrefix: h.rec.Seed.Prefix + "/imrestart-seed", SecretRef: s12IMRestartSecretName, Bootstrap: []s12Phase{},
		Members: []imageIdentity{}, SecretsReferenced: []string{}, Credentials: &s12Credentials{FoundIn: []string{}},
		Notes: append([]string{"the cluster restored while " + map[bool]string{true: "its target's",
			false: "an"}[at == IMRestartAtRestoring] + " Instance Manager restarts uses a copy of the seed Secret of " +
			"its own, " + s12IMRestartSecretName + "; the source uses " + seedSecretName}, imRestartNotes[at]...),
		Residue: "the cluster's seed backups under its seed prefix are not removed by this run; the cluster goes " +
			"with the namespace"},
		IMRestart: &s12IMRestart{At: at, variant: variant, restartReading: -1, Reasons: []string{},
			Attempts: []s12Attempt{}, RetryEvents: []string{}, Retries: []s12Retry{}}}
	defer func() {
		// A pass without its evidence is a failure; a blocked or failed S12
		// instance-manager-restart keeps its result, with the evidence that
		// could not be written.
		if err := h.writeJSON(rec.IMRestart.file("record.json"), rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S12 "+variant+" evidence: "+
				err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S12/"+variant+"/")
		})
	}()
	result.Limits = map[string]string{restoreLimitName: h.cfg.RestoreLimit.String()}
	result.Measurements = map[string]any{restoredWithinMeasurement: evidence.Unknown, "memberBackWithin": evidence.Unknown,
		"containerRestarts": evidence.Unknown, "operationIDChanged": evidence.Unknown,
		"attemptsAdded": evidence.Unknown, measRetriesAfterKill: evidence.Unknown,
		dataRulesHeldMeasurement: evidence.Unknown, postBackupRowsMeasurement: evidence.Unknown}
	switch at {
	case IMRestartAtPreparing:
		result.Measurements["restoredAtKill"], result.Measurements["stagingLeftAtEnd"] = evidence.Unknown,
			evidence.Unknown
	case IMRestartAtStarting:
		result.Measurements["restoredAtKill"], result.Measurements["stagingLeftAtEnd"] = evidence.Unknown,
			evidence.Unknown
		result.Measurements["tookOverRestoredData"] = evidence.Unknown
	case IMRestartAtSeeding:
		result.Measurements[measSeedRetriesAfterKill], result.Measurements[measSeedAttemptsAdded] = evidence.Unknown,
			evidence.Unknown
		result.Measurements[measSeedRestoreOperationChanged] = evidence.Unknown
	case IMRestartAtSeedBackup:
		result.Measurements[measSeedRetriesAfterKill], result.Measurements[measSeedAttemptsAdded] = evidence.Unknown,
			evidence.Unknown
		result.Measurements["seedBackupOperationChanged"] = evidence.Unknown
		result.Measurements["killedBackupFailedByRestart"] = evidence.Unknown
		result.Measurements["uploadBytesBeforeKill"] = evidence.Unknown
	}
	err := h.imRestartSteps(ctx, &result, &rec)
	// A step that ended before the kill leaves the rate limit to remove here.
	err = errors.Join(err, h.removeSeedLimit(ctx, rec.IMRestart))
	// What the bootstrap showed is judged whichever step ended the variant.
	if judged := judgeIMRestartBootstrap(&rec, &result); err == nil || errors.As(judged, new(restoreVerdict)) {
		err = errors.Join(judged, err)
	}
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	if blocked := notInterrupted(&rec); blocked != "" && len(result.NeverEvents) == 0 &&
		!errors.As(err, new(dataViolation)) {
		// What followed the kill is not this variant's to judge; data that
		// differs from the backup stays a failure.
		if err != nil {
			blocked += "; then: " + err.Error()
		}
		err = errors.New(blocked)
	}
	never := strings.Join(result.NeverEvents, "; ")
	switch {
	case errors.As(err, new(restoreVerdict)) || errors.As(err, new(dataViolation)):
		result.Result, result.Reason = evidence.Fail, err.Error()
		switch {
		case slices.Contains(result.NeverEvents, err.Error()):
			result.Reason = never
		case never != "":
			result.Reason = never + "; " + err.Error()
		}
	case err != nil && never != "":
		// What was not checked keeps a confirmed failure a failure.
		result.Result, result.Reason = evidence.Fail, never+"; "+err.Error()
	case err != nil && ctx.Err() != nil:
		result.Result, result.Reason = evidence.Blocked, "the run ended before S12 "+variant+" was judged: "+
			err.Error()
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, err.Error()
	case never != "":
		result.Result, result.Reason = evidence.Fail, never
	default:
		result.Result, result.Reason = evidence.Pass, imRestartPassReasonAt(rec.IMRestart)
	}
	return result
}

// imOperatorNotChecked is the note of what of the Operator a placement
// other than seeding does not read.
const imOperatorNotChecked = "the Operator's logs, and its Events of the cluster other than " +
	"RestoreFailedRetrying, were not checked"

// imRestartNotes are the record's notes of each placement, besides the
// Secret's.
var imRestartNotes = map[string][]string{
	IMRestartAtRestoring: {"the kill is placed after a reading that saw the bootstrap Restoring; the Instance " +
		"Manager's operation may have moved on before the kill took effect",
		"whether the next attempt removed the failed attempt's data or took it over was not read: the Instance " +
			"Manager's logs and operation records were not checked",
		imOperatorNotChecked},
	IMRestartAtPreparing: {"the kill is placed after a reading that saw the bootstrap Preparing with an operation: " +
		"the Operator had read the operation no further than Pending or Downloading; the " +
		"Instance Manager records its operation Restoring only right before restoredb, after the backup is " +
		"verified and downloaded (runRestore in internal/instancemanager/server.go), but it may have done so " +
		"after the Operator's last read, so whether the download or restoredb ran at the kill was not observed, " +
		"only that restoredb had not succeeded",
		"the record of the killed operation on the target's volume, and whether the recovery's staging directory " +
			"was left there, were read once the cluster was Ready; the next attempt's own record, the Instance " +
			"Manager's logs and the rest of the volume were not read",
		imOperatorNotChecked},
	IMRestartAtSeeding: {"the kill is placed after a reading that saw the bootstrap SeedingReplicas with a seed " +
		"restore recorded for the member killed; the Instance Manager's operation may have moved on before the " +
		"kill took effect",
		"whether the next seed attempt took a new seed backup or reused the one the failed attempt restored, and " +
			"whether the member's Instance Manager removed the failed attempt's data or took it over, were not " +
			"read: the Instance Manager's logs and operation records were not checked",
		"the Operator's logs, and its Events of the cluster other than RestoreFailedRetrying and " +
			"SeedRestoreFailedRetrying, were not checked"},
	IMRestartAtSeedBackup: {"the restore target's TCP traffic to the object store is limited to " +
		strconv.Itoa(interruptedRate) + " bit/s from a reading of the bootstrap Preparing or Restoring until " +
		"after the kill, by a network fault in an ephemeral container with NET_ADMIN and NET_RAW " +
		"(test/faults/netfault), as in S11 instance-manager-restart, so that its seed backup's upload lasts long " +
		"enough to be killed during it; the run namespace enforces Pod Security privileged for it, with warn and " +
		"audit at restricted; the limit acts on what the target sends only, so the restore's download passes it " +
		"only as acknowledgements and requests, and the ephemeral container stays in the target's Pod",
		"the kill is placed at the first reading, taken no more often than every " + imStartingPoll.String() +
			" once the bootstrap was SeedingReplicas, of the source's own record of the recorded seed backup " +
			"Uploading, with no seed restore recorded for its peer and " + strconv.Itoa(interruptedUploadSeen) +
			" bytes through the limit since the first reading of that record; the backup may have ended before " +
			"the kill took effect, which the record of the killed backup Failed by the restart rules out",
		"the kill ends the source's database container and with it the restored database's server, the only " +
			"copy of the restored data until a peer is seeded; the entrypoint starts heartbeat again when the " +
			"container runs again (build/instance-manager/entrypoint.sh); that the restored data was not " +
			"restored again is read from the bootstrap (no restore retry and no reading of restoredb running " +
			"after the kill) and from the data checks, not from the volume",
		"the seed backup left in object storage by the killed attempt, the Instance Manager's logs and the " +
			"Operator's logs, and its Events of the cluster other than RestoreFailedRetrying and " +
			"SeedBackupFailedRetrying, were not checked"},
	IMRestartAtStarting: {"the kill is placed at the first reading, taken no more often than every " +
		imStartingPoll.String() + " while the bootstrap was Restoring, of the target's own record of the " +
		"bootstrap's operation in the state Starting: restoredb had succeeded and the restored database was " +
		"being started; the start may have ended before the kill took effect, which a record of the killed " +
		"operation Failed by the restart, not Completed, rules out; a record read Failed for any reason " +
		"before Starting blocks the variant, the restore's own failure being S12 exact's to judge",
		"the records of the killed operation and of the operation that completed the restore were read on the " +
			"target's volume once the cluster was Ready; the same restored artifact in both, and no reading of the " +
			"completing operation showing restoredb running, are taken as the data taken over rather than " +
			"restored again; a second restore of the same artifact would record the same artifact, and the " +
			"readings may miss a short restoredb run, so a second restore is not ruled out; the Instance " +
			"Manager's logs and the rest of the volume were not read",
		imOperatorNotChecked},
}

// imRestartPassReasonAt is the reason of a pass at the placement of r.
func imRestartPassReasonAt(r *s12IMRestart) string {
	switch r.At {
	case IMRestartAtPreparing:
		return imRestartPreparingPassReason(r)
	case IMRestartAtSeeding:
		return imRestartSeedingPassReason(r)
	case IMRestartAtStarting:
		return imRestartStartingPassReason(r)
	case IMRestartAtSeedBackup:
		return imRestartSeedBackupPassReason(r)
	}
	return imRestartPassReason(r)
}

// notInterrupted says why the kill did not interrupt what it was placed in,
// when the first reading after it was already past that and no retry
// followed; it is empty otherwise. At restoring, that is a first reading
// SeedingReplicas or Complete; at preparing also Restoring, which the
// Operator records only from the Instance Manager's answer
// (setRestoreProgress in internal/controller/bootstrap.go), and a restarted
// one answers Failed for the operation; at seeding, the first changed
// reading showing the member seeded or the bootstrap Complete.
func notInterrupted(rec *s12IMRestartRecord) string {
	r := rec.IMRestart
	if r.KilledAt == nil {
		return ""
	}
	if seedPlacement(r.At) {
		if r.seedRetriesAfterKill > 0 || r.restartReading < 0 || r.restartReading+1 >= len(rec.Bootstrap) {
			return ""
		}
		p := rec.Bootstrap[r.restartReading+1]
		if p.Phase != string(databasev1alpha1.BootstrapComplete) && !slices.Contains(p.SeededMembers, seededPeer(r)) {
			return ""
		}
		return fmt.Sprintf("the kill did not interrupt %s: the first changed reading after it was %s with %s "+
			"seeded, and no seed retry followed", seedStepName(r.At), cmp.Or(p.Phase, noBootstrapPhase),
			cmp.Or(strings.Join(p.SeededMembers, ", "), "no member"))
	}
	past := r.PhaseAfterRestart == string(databasev1alpha1.BootstrapSeedingReplicas) ||
		r.PhaseAfterRestart == string(databasev1alpha1.BootstrapComplete)
	if r.At == IMRestartAtPreparing && r.PhaseAfterRestart == string(databasev1alpha1.BootstrapRestoring) {
		past = true
	}
	if !past || r.retriesAfterKill > 0 {
		return ""
	}
	what := "a restore"
	if r.At == IMRestartAtPreparing {
		what = "the restore while it was Preparing"
	}
	return fmt.Sprintf("the kill did not interrupt %s: the first reading after it was %s, and no retry "+
		"followed", what, r.PhaseAfterRestart)
}

// seedPlacement says whether the placement at kills during seeding, after
// the restore completed.
func seedPlacement(at string) bool { return at == IMRestartAtSeeding || at == IMRestartAtSeedBackup }

// seededPeer is the member whose seed step the kill interrupts: the member
// killed at seeding, the peer of the seed backup at seed-backup.
func seededPeer(r *s12IMRestart) string {
	if r.At == IMRestartAtSeedBackup {
		return r.SeedPeer
	}
	return r.Pod
}

// seedStepName names the seed step the placement at interrupts.
func seedStepName(at string) string {
	if at == IMRestartAtSeedBackup {
		return "a seed backup"
	}
	return "a seed restore"
}

// seedRetryReasonAt is the BootstrapReady reason of the seed retry the kill
// at the placement at causes (bootstrapStepFailed in internal/controller/
// habootstrap.go): SeedBackupFailedRetrying for a seed backup that failed,
// SeedRestoreFailedRetrying for a seed restore.
func seedRetryReasonAt(at string) string {
	if at == IMRestartAtSeedBackup {
		return s12SeedBackupRetryReason
	}
	return s12SeedRetryReason
}

// imRestartSteps runs the steps of S12 instance-manager-restart in order and
// returns the first error.
func (h *haRun) imRestartSteps(ctx context.Context, result *evidence.Scenario, rec *s12IMRestartRecord) error {
	r := rec.IMRestart
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S12 %s could not tell whether a credential is in the cluster's status", err, r.variant)
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
	if r.At == IMRestartAtSeedBackup {
		// Refused before anything is created: the limit's ephemeral container
		// needs a privileged namespace, and the limit an address to act on.
		throttle := h.newS11Throttle("")
		r.Throttle = &throttle
		if err := h.privilegedNamespace(ctx, "S12 "+r.variant, &r.PodSecurity); err != nil {
			return err
		}
		if err := h.interruptedStore(ctx, r.Throttle); err != nil {
			return err
		}
	}
	cluster, err := h.createRestored(ctx, s12IMRestartClusterName, s12IMRestartSecretName, rec.SeedPrefix,
		rec.ArtifactURI)
	if err != nil {
		return err
	}
	created := time.Now()
	createdAt := created.UTC()
	rec.ClusterUID, rec.CreatedAt = string(cluster.UID), &createdAt
	if err := h.waitIMPlacement(ctx, cluster, created, rec, result); err != nil {
		return err
	}
	err = h.killDuringRestore(ctx, cluster, created, rec, result)
	if r.At == IMRestartAtSeedBackup {
		// The limit is removed right after the source's container runs
		// again; the retry may begin under it briefly.
		err = errors.Join(err, h.removeSeedLimit(ctx, r))
	}
	if err != nil {
		return err
	}
	roles, readyAt, err := h.waitRestored(ctx, cluster, created, &rec.s12Record, result)
	if err != nil {
		return err
	}
	rec.RestoredWithin = readyAt.Sub(created).Round(time.Millisecond).String()
	result.Measurements[restoredWithinMeasurement] = rec.RestoredWithin
	h.readRetryEvents(ctx, cluster, r)
	if err := h.targetRestartsAtEnd(ctx, &r.memberKill, result); err != nil {
		return err
	}
	if err := h.checkRestoredSecrets(ctx, &rec.s12Record, result); err != nil {
		return err
	}
	if err := h.checkHANodes(ctx, roles, &rec.s12Record, result); err != nil {
		return err
	}
	if err := h.checkRestoredData(ctx, expected, roles, &rec.s12Record, result, r.file("data-check.json")); err != nil {
		return err
	}
	if err := h.normalWriteS12(ctx, roles, &rec.s12Record, s12IMRestartMarker); err != nil {
		return err
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	if err := h.checkSourceS12(ctx, rec.Source, r.file("source-data-check.json")); err != nil {
		return err
	}
	final := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), final); err != nil {
		return fmt.Errorf("read the restored cluster at the end: %w", err)
	}
	recordBootstrap(final, &rec.Bootstrap)
	rec.Credentials.StatusesChecked++
	if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the restored cluster's status at the end",
		final.Status); err != nil {
		return err
	}
	if r.At == IMRestartAtSeedBackup {
		return h.checkKilledSeedBackup(ctx, r, result)
	}
	if r.At != IMRestartAtPreparing && r.At != IMRestartAtStarting {
		return nil
	}
	// Last, so that what the data showed is judged whatever the records say.
	return h.checkKilledOperation(ctx, cluster, rec, result)
}

// waitIMPlacement waits for the reading the kill follows, as
// waitRestoring does: at restoring that is waitRestoring; at preparing a
// reading of Preparing with an operation, and a first phase past Preparing
// blocks the variant; at seeding a reading of SeedingReplicas with a seed
// restore recorded for a member, and Complete before it blocks the variant;
// at starting waitRestoring and then waitStarting.
func (h *haRun) waitIMPlacement(ctx context.Context, cluster *databasev1alpha1.CubridCluster, created time.Time,
	rec *s12IMRestartRecord, result *evidence.Scenario) error {
	r := rec.IMRestart
	switch r.At {
	case IMRestartAtPreparing:
		return h.waitPlaced(ctx, cluster, created, &rec.s12Record, &r.PhaseFirstPastPreparing, result,
			restoreTarget(cluster.Name), "restore did not start",
			func(c *databasev1alpha1.CubridCluster, phase string) (bool, error) {
				if phase != string(databasev1alpha1.BootstrapPreparing) {
					return false, fmt.Errorf("the kill could not be placed while the restore was Preparing: the "+
						"bootstrap was first seen %s, not Preparing with an operation", phase)
				}
				return c.Status.Bootstrap.OperationID != "", nil
			})
	case IMRestartAtSeeding:
		return h.waitPlaced(ctx, cluster, created, &rec.s12Record, &r.PhaseFirstPastPreparing, result,
			seedingMembers(cluster.Name), "seeding did not start a seed restore",
			func(c *databasev1alpha1.CubridCluster, phase string) (bool, error) {
				switch phase {
				case string(databasev1alpha1.BootstrapPreparing), string(databasev1alpha1.BootstrapRestoring):
					return false, nil
				case string(databasev1alpha1.BootstrapSeedingReplicas):
					seeding := firstDatabaseSeeding(c)
					return seeding != nil && seeding.Member != "" && seeding.RestoreOperationID != "", nil
				}
				return false, fmt.Errorf("the kill could not be placed during seeding: the bootstrap was seen %s "+
					"before a reading of SeedingReplicas with a seed restore", phase)
			})
	}
	if r.At == IMRestartAtSeedBackup {
		if err := h.limitSeedSource(ctx, cluster, created, rec, result); err != nil {
			return err
		}
		if err := h.waitPlaced(ctx, cluster, created, &rec.s12Record, &r.PhaseFirstPastPreparing, result,
			restoreTarget(cluster.Name), "restore did not complete",
			func(_ *databasev1alpha1.CubridCluster, phase string) (bool, error) {
				switch phase {
				case string(databasev1alpha1.BootstrapPreparing), string(databasev1alpha1.BootstrapRestoring):
					return false, nil
				case string(databasev1alpha1.BootstrapSeedingReplicas):
					return true, nil
				}
				return false, fmt.Errorf(imNotAtSeedBackup+"the bootstrap was seen %s before SeedingReplicas", phase)
			}); err != nil {
			return err
		}
		return h.waitSeedBackup(ctx, cluster, created, rec, result)
	}
	if err := h.waitRestoring(ctx, cluster, created, &rec.s12Record, &r.PhaseFirstPastPreparing, result); err != nil ||
		r.At != IMRestartAtStarting {
		return err
	}
	return h.waitStarting(ctx, cluster, created, rec, result)
}

// imNoSeedLimit starts the reason of a rate limit that could not be placed
// at seed-backup.
const imNoSeedLimit = "the rate limit could not be placed before the seed backup: "

// limitSeedSource places, at seed-backup, the rate limit on the restore
// target's TCP traffic to the object store, the seed backup's source, as S11
// instance-manager-restart does on its member (netfault.Throttle at
// interruptedRate): once a reading shows the bootstrap Preparing or Restoring
// with its target, it reads the target's Pod, installs the limit and confirms
// it. The Operator starts the first seed backup in the reconcile that records
// SeedingReplicas (reconcileRecoverySeeding, seedMember in
// internal/controller/), so the limit must be in place before that phase is
// read; a bootstrap first seen in another phase blocks the variant. The limit
// acts on what the target sends to the store only: the restore's download
// passes it only as the acknowledgements and requests the target sends, and
// the seed restore on the peer not at all.
func (h *haRun) limitSeedSource(ctx context.Context, cluster *databasev1alpha1.CubridCluster, created time.Time,
	rec *s12IMRestartRecord, result *evidence.Scenario) error {
	r, f := rec.IMRestart, rec.IMRestart.Throttle
	if err := h.waitPlaced(ctx, cluster, created, &rec.s12Record, &r.PhaseFirstPastPreparing, result,
		restoreTarget(cluster.Name), "restore did not start",
		func(c *databasev1alpha1.CubridCluster, phase string) (bool, error) {
			switch phase {
			case string(databasev1alpha1.BootstrapPreparing), string(databasev1alpha1.BootstrapRestoring):
			default:
				return false, fmt.Errorf(imNoSeedLimit+"the bootstrap was seen %s before the target's Pod could "+
					"take the limit", phase)
			}
			target := c.Status.Bootstrap.TargetMember
			if target == "" {
				return false, nil
			}
			if !slices.Contains(clusterMembers(cluster.Name), target) {
				return false, fmt.Errorf("the restore's target member %q is not a member of %s", target, cluster.Name)
			}
			return h.seedSourceReady(ctx, cluster, target, f)
		}); err != nil {
		return err
	}
	env := netfault.Env{Client: h.Client, Kubectl: h.Command, Poll: h.cfg.Poll,
		Reserve: min(netfault.DefaultReserve, h.cfg.CleanupTimeout/4)}
	r.limit = &s11InterruptedMade{throttle: &netfault.Throttle{Env: env,
		Target: netfault.Endpoint{Name: f.Target, Namespace: h.cfg.Namespace, Pod: f.Target, UID: f.TargetUID,
			IP: f.TargetIP},
		Peer: netfault.Endpoint{Name: f.Store, IP: f.StoreIP, Port: f.StorePort}, Rate: interruptedRate}}
	if err := r.limit.throttle.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare the rate limit: %w", err)
	}
	if err := confirmThrottle(ctx, r.limit.throttle, f); err != nil {
		return err
	}
	return h.limitBeforeSeeding(ctx, cluster, rec, result)
}

// seedSourceReady reports whether the restore target's Pod can take the
// limit: it exists, is not being deleted, the cluster's StatefulSet controls
// it and it has an IP. The IP comes with the Pod's sandbox, and with it the
// network namespace the limit acts in, before the database container runs:
// the limit goes in before the Instance Manager can start the restore, and
// so long before its seed backup. The Operator names the target in its first
// reconcile of a recovery, usually before the Pod is scheduled, so a Pod
// that is missing or has no IP yet is waited for; a Pod of another owner is
// an error. Once ready, its UID and IP are recorded in f.
func (h *haRun) seedSourceReady(ctx context.Context, cluster *databasev1alpha1.CubridCluster, target string,
	f *s11Throttle) (bool, error) {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: target}, pod); err != nil {
		if apierrors.IsNotFound(err) || retryable(err) {
			f.Waiting = "the target's Pod " + target + " could not be read yet: " + err.Error()
			return false, nil
		}
		return false, fmt.Errorf("read the target's Pod %s: %w", target, err)
	}
	if owner := metav1.GetControllerOf(pod); owner == nil || owner.Kind != kindStatefulSet ||
		owner.Name != cluster.Name {
		return false, fmt.Errorf("the Pod %s is not controlled by the StatefulSet of %s", target, cluster.Name)
	}
	if pod.Status.PodIP == "" || pod.DeletionTimestamp != nil {
		f.Waiting = "the target's Pod " + target + " has no IP yet or is being deleted"
		return false, nil
	}
	f.Target, f.TargetUID, f.TargetIP, f.Waiting = target, string(pod.UID), pod.Status.PodIP, ""
	return true, nil
}

// limitBeforeSeeding reads the restored cluster once the limit is
// confirmed. What decides is that the seed backup's upload passes the limit,
// which the kill requires anyway (seedUploadUnderWay counts 512 KiB through
// it while the backup is Uploading); the limit acts from its installation.
// So a seed backup already recorded is accepted while the source's record of
// it has not ended. A seed restore already recorded, a seed backup whose
// record ended or cannot be read, or a bootstrap past SeedingReplicas shows
// a seed backup whose upload may have run before the limit, which blocks the
// variant.
func (h *haRun) limitBeforeSeeding(ctx context.Context, created *databasev1alpha1.CubridCluster,
	rec *s12IMRestartRecord, result *evidence.Scenario) error {
	r := rec.IMRestart
	cluster, err := h.readRestarted(ctx, created)
	if err != nil {
		return fmt.Errorf("read the restored cluster after the rate limit was confirmed: %w", err)
	}
	phase, err := h.restoredStatus(cluster, &rec.s12Record, result)
	if err != nil {
		return err
	}
	switch phase {
	case string(databasev1alpha1.BootstrapPreparing), string(databasev1alpha1.BootstrapRestoring),
		string(databasev1alpha1.BootstrapSeedingReplicas):
	default:
		return fmt.Errorf(imNoSeedLimit+"the bootstrap was %s when the limit was confirmed", cmp.Or(phase,
			noBootstrapPhase))
	}
	seeding := firstDatabaseSeeding(cluster)
	switch {
	case seeding == nil || seeding.BackupOperationID == "" && seeding.RestoreOperationID == "":
		return nil
	case seeding.RestoreOperationID != "":
		return fmt.Errorf(imNoSeedLimit+"seed restore %s was already recorded when the limit was confirmed",
			seeding.RestoreOperationID)
	}
	id := seeding.BackupOperationID
	op, err := h.readIMOperation(ctx, r.Throttle.Target, id)
	if err != nil {
		return fmt.Errorf(imNoSeedLimit+"seed backup %s was recorded when the limit was confirmed, and its record "+
			"could not be read: %w", id, err)
	}
	r.OperationReadings = append(r.OperationReadings, s12OperationReading{At: time.Now().UTC(), Operation: id,
		State: string(op.State)})
	if op.State.IsTerminal() {
		return fmt.Errorf(imNoSeedLimit+"seed backup %s was read %s when the limit was confirmed", id, op.State)
	}
	return nil
}

// removeSeedLimit removes, at seed-backup, the rate limit once
// (removeInterruptedFaults) and reads the store's address again. A limit not
// seen removed blocks the variant: the source's traffic may still be
// limited. Called again after a removal was tried, it does nothing.
func (h *haRun) removeSeedLimit(ctx context.Context, r *s12IMRestart) error {
	if r.limit == nil || r.limit.tried {
		return nil
	}
	return h.throttleAtEnd(ctx, r.limit, r.Throttle)
}

// imNotAtSeedBackup starts the reason of a kill that could not be placed at
// seed-backup.
const imNotAtSeedBackup = "the kill could not be placed while the seed backup ran: "

// seedUploadUnderWay reads, at seed-backup, the rate limit's counters at
// reading, a reading of the seed backup's record that has not ended, and
// reports whether it places the kill: the record Uploading, and
// interruptedUploadSeen bytes through the limit since the first such reading
// (UploadBaseline), so that the kill lands in an upload under way, not in its
// first request. Each other reading is recorded once per state. A read of the
// counters that fails is tolerated as S11's are (tolerate).
func (h *haRun) seedUploadUnderWay(ctx context.Context, r *s12IMRestart, reading s12OperationReading,
	result *evidence.Scenario) (bool, error) {
	f := r.Throttle
	counters, err := r.limit.throttle.Counters(ctx)
	if err != nil {
		return false, r.limit.tolerate(f, "read the rate limit's counters", err)
	}
	r.limit.errorsInARow = 0
	f.Last = &counters
	if r.UploadBaseline == nil {
		r.UploadBaseline = &counters
	}
	sent := counters.Bytes - min(counters.Bytes, r.UploadBaseline.Bytes)
	if reading.State != string(instancemanager.OpUploading) || sent < interruptedUploadSeen {
		if n := len(r.OperationReadings); n == 0 || r.OperationReadings[n-1].State != reading.State {
			r.OperationReadings = append(r.OperationReadings, reading)
		}
		f.Waiting = fmt.Sprintf("%d bytes through the rate limit since the first reading of the seed backup, its "+
			"record %s", sent, reading.State)
		return false, nil
	}
	r.PlacingReading, r.AtKill, r.SentBeforeKill, f.Waiting = &reading, &counters, sent, ""
	result.Measurements["uploadBytesBeforeKill"] = sent
	return true, nil
}

// waitSeedBackup reads, at seed-backup, the restored cluster every
// imStartingPoll while its bootstrap is SeedingReplicas and, once the first
// database's seeding records a seed backup and no seed restore, the
// source's own record of that backup, until the record is read in a state
// that has not ended, which places the kill, or RestoreLimit has passed
// since createdAt. The Operator records the seed backup's operation once it
// started it on the source, polls it until it ends, and records the seed
// restore on the peer only after the backup completed (seedMember,
// seedBackup in internal/controller/habootstrap.go). A bootstrap seen in
// another phase, a seed restore recorded, or a record read Completed or
// Failed, before any reading of the backup running, means the kill could
// not be placed there, which blocks the variant; a seed backup that failed
// is S12 exact's to judge. A record that cannot be read is read again.
func (h *haRun) waitSeedBackup(ctx context.Context, created *databasev1alpha1.CubridCluster, createdAt time.Time,
	rec *s12IMRestartRecord, result *evidence.Scenario) error {
	r := rec.IMRestart
	wctx, cancel := context.WithDeadline(ctx, createdAt.Add(h.cfg.RestoreLimit))
	defer cancel()
	// readErr is the last error of a read of the cluster or the record, nil
	// once one succeeded.
	var readErr error
	err := pollUntil(wctx, imStartingPoll, func() (bool, error) {
		cluster, err := h.readRestarted(wctx, created)
		switch {
		case (err != nil || cluster == nil) && wctx.Err() != nil:
			return false, nil
		case err != nil && retryable(err):
			readErr = err
			return false, nil
		case err != nil:
			return false, err
		}
		readErr = nil
		phase, err := h.restoredStatus(cluster, &rec.s12Record, result)
		switch {
		case err != nil:
			return false, err
		case phase == string(databasev1alpha1.BootstrapFailed):
			return false, restoreVerdict{fmt.Errorf("the restored cluster's bootstrap ended Failed: %s",
				bootstrapReason(cluster))}
		case phase != string(databasev1alpha1.BootstrapSeedingReplicas):
			return false, fmt.Errorf(imNotAtSeedBackup+"the bootstrap was seen %s before any reading of a seed "+
				"backup running", cmp.Or(phase, noBootstrapPhase))
		}
		seeding := firstDatabaseSeeding(cluster)
		switch {
		case seeding == nil || seeding.Member == "":
			return false, nil
		case seeding.RestoreOperationID != "":
			return false, fmt.Errorf(imNotAtSeedBackup+"a seed restore %s of %s was recorded before any reading of "+
				"its seed backup running", seeding.RestoreOperationID, seeding.Member)
		case seeding.BackupOperationID == "":
			return false, nil
		case !slices.Contains(clusterMembers(created.Name), seeding.Source):
			return false, fmt.Errorf("the seed backup's source %q is not a member of %s", seeding.Source, created.Name)
		case seeding.Source != cluster.Status.Bootstrap.TargetMember:
			return false, fmt.Errorf(imNotAtSeedBackup+"the seed backup's source is %s, not the restored member %s",
				seeding.Source, cluster.Status.Bootstrap.TargetMember)
		}
		id := seeding.BackupOperationID
		op, err := h.readIMOperation(wctx, seeding.Source, id)
		switch {
		case err != nil && wctx.Err() != nil:
			return false, nil
		case err != nil:
			readErr = fmt.Errorf("read the record of the seed backup %s on %s: %w", id, seeding.Source, err)
			return false, nil
		}
		readErr = nil
		reading := s12OperationReading{At: time.Now().UTC(), Operation: id, State: string(op.State)}
		if op.State.IsTerminal() {
			r.OperationReadings = append(r.OperationReadings, reading)
			return false, fmt.Errorf(imNotAtSeedBackup+"the source's record of the seed backup %s was read %s before "+
				"the kill was placed", id, op.State)
		}
		return h.seedUploadUnderWay(wctx, r, reading, result)
	})
	switch {
	case err == nil:
		return nil
	case limitEnded(ctx, wctx, err) && readErr != nil:
		return h.limitUnread(seedingMembers(created.Name), fmt.Errorf(imNotAtSeedBackup+"no reading before "+
			"restore_limit %s passed showed it: %w", h.cfg.RestoreLimit, readErr))
	case limitEnded(ctx, wctx, err):
		return h.limitPassed(seedingMembers(created.Name), fmt.Errorf("the restored cluster's seeding recorded no "+
			"seed backup within restore_limit %s", h.cfg.RestoreLimit))
	}
	return h.runEnded(ctx, seedingMembers(created.Name), err)
}

// imStartingPoll is how often, at starting, the target's record of the
// operation is read: the restored database is started within seconds of
// restoredb's end, a window the run's poll interval would mostly miss.
const imStartingPoll = 250 * time.Millisecond

// waitStarting reads, at starting, the restored cluster every
// imStartingPoll and, while its bootstrap is Restoring with the operation the
// kill follows, the restore target's own record of that operation, until the
// record is read Starting, which places the kill, or RestoreLimit has passed
// since created. Each tick runs one read of the cluster and one kubectl exec,
// so imStartingPoll is a lower bound of the interval. The Instance Manager
// records Starting only after restoredb succeeded and the restored artifact is
// on record, right before it starts the restored database (runRestore,
// startRestoredDatabase in internal/instancemanager/server.go); the Operator
// keeps the bootstrap Restoring with that operation meanwhile
// (setRestoreProgress in internal/controller/bootstrap.go). A bootstrap seen
// in another phase or with another operation, or a record read Completed or
// Failed, before any reading of Starting means the kill could not be placed
// there, which blocks the variant; a restore that failed is S12 exact's to
// judge. A record that cannot be read is read again.
func (h *haRun) waitStarting(ctx context.Context, created *databasev1alpha1.CubridCluster, createdAt time.Time,
	rec *s12IMRestartRecord, result *evidence.Scenario) error {
	r := rec.IMRestart
	last := rec.Bootstrap[len(rec.Bootstrap)-1]
	id, member := last.OperationID, last.TargetMember
	if !slices.Contains(clusterMembers(created.Name), member) {
		return fmt.Errorf("the restore's target member %q is not a member of %s", member, created.Name)
	}
	const notPlaced = "the kill could not be placed while the restored database was being started: "
	wctx, cancel := context.WithDeadline(ctx, createdAt.Add(h.cfg.RestoreLimit))
	defer cancel()
	// readErr is the last error of a read of the cluster or the record, nil
	// once one succeeded.
	var readErr error
	err := pollUntil(wctx, imStartingPoll, func() (bool, error) {
		cluster, err := h.readRestarted(wctx, created)
		switch {
		case (err != nil || cluster == nil) && wctx.Err() != nil:
			return false, nil
		case err != nil && retryable(err):
			readErr = err
			return false, nil
		case err != nil:
			return false, err
		}
		phase, err := h.restoredStatus(cluster, &rec.s12Record, result)
		current := ""
		if boot := cluster.Status.Bootstrap; boot != nil {
			current = boot.OperationID
		}
		switch {
		case err != nil:
			return false, err
		case phase == string(databasev1alpha1.BootstrapFailed):
			return false, restoreVerdict{fmt.Errorf("the restored cluster's bootstrap ended Failed: %s",
				bootstrapReason(cluster))}
		case phase != string(databasev1alpha1.BootstrapRestoring) || current != id:
			return false, fmt.Errorf(notPlaced+"the bootstrap was seen %s with operation %q before any reading of "+
				"the record of operation %s Starting", cmp.Or(phase, noBootstrapPhase), current, id)
		}
		op, err := h.readIMOperation(wctx, member, id)
		switch {
		case err != nil && wctx.Err() != nil:
			return false, nil
		case err != nil:
			readErr = fmt.Errorf("read the record of operation %s on %s: %w", id, member, err)
			return false, nil
		}
		readErr = nil
		reading := s12OperationReading{At: time.Now().UTC(), Operation: id, State: string(op.State)}
		if op.State == instancemanager.OpStarting {
			r.PlacingReading = &reading
			return true, nil
		}
		if n := len(r.OperationReadings); n == 0 || r.OperationReadings[n-1].State != reading.State {
			r.OperationReadings = append(r.OperationReadings, reading)
		}
		if op.State.IsTerminal() {
			return false, fmt.Errorf(notPlaced+"the target's record of operation %s was read %s before any "+
				"reading of it Starting", id, op.State)
		}
		return false, nil
	})
	switch {
	case err == nil:
		return nil
	case limitEnded(ctx, wctx, err) && readErr != nil:
		return h.limitUnread([]string{member}, fmt.Errorf(notPlaced+"no reading before restore_limit %s passed "+
			"showed it: %w", h.cfg.RestoreLimit, readErr))
	case limitEnded(ctx, wctx, err):
		return h.limitPassed([]string{member}, fmt.Errorf("the restored database was not started within "+
			"restore_limit %s: the target's record of operation %s was last read %s", h.cfg.RestoreLimit, id,
			lastOperationState(r.OperationReadings)))
	}
	return h.runEnded(ctx, []string{member}, err)
}

// lastOperationState is the state of the last of readings, or unknown.
func lastOperationState(readings []s12OperationReading) string {
	if n := len(readings); n > 0 {
		return readings[n-1].State
	}
	return evidence.Unknown
}

// operationRecordScript prints the Instance Manager's record of operation
// id, a plain name checked by the caller, so that it names nothing outside
// the records' directory.
func operationRecordScript(id string) string {
	return "cat -- '" + imOperationsDir + "/" + id + ".json'"
}

// readIMOperation reads the Instance Manager's record of operation id on
// the member pod's volume. A record of another operation is an error.
func (h *haRun) readIMOperation(ctx context.Context, pod, id string) (instancemanager.Operation, error) {
	var op instancemanager.Operation
	if !plainName.MatchString(id) {
		return op, fmt.Errorf("the operation ID %q is not a plain name", id)
	}
	out, err := h.member(ctx, pod, "bash", "-c", operationRecordScript(id))
	if err != nil {
		return op, err
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &op); err != nil {
		return op, fmt.Errorf("the answer is not a record: %w", err)
	}
	if op.ID != id {
		return op, fmt.Errorf("the record read is of operation %q", op.ID)
	}
	return op, nil
}

// firstDatabaseSeeding is the seeding in the status of the spec's first
// database, or nil.
func firstDatabaseSeeding(cluster *databasev1alpha1.CubridCluster) *databasev1alpha1.DatabaseSeeding {
	status := firstDatabaseStatus(cluster)
	if status == nil {
		return nil
	}
	return status.Seeding
}

// firstDatabaseStatus is the status of the spec's first database, or nil.
func firstDatabaseStatus(cluster *databasev1alpha1.CubridCluster) *databasev1alpha1.DatabaseStatus {
	if len(cluster.Spec.Databases) == 0 {
		return nil
	}
	for i := range cluster.Status.Databases {
		if cluster.Status.Databases[i].Name == cluster.Spec.Databases[0].Name {
			return &cluster.Status.Databases[i]
		}
	}
	return nil
}

// imRestartPassReason is the reason of a pass, from what the run recorded.
func imRestartPassReason(r *s12IMRestart) string {
	attempts := int32(0)
	if r.AttemptsAfter != nil {
		attempts = *r.AttemptsAfter
	}
	return fmt.Sprintf("the Instance Manager on %s was killed while the cluster restored from S11's artifact was %s "+
		"with operation %s (%q), its container ran again on the same Pod %s after the kill, and the Operator "+
		"ended that attempt with %s naming the Instance Manager's restart and completed the restore under "+
		"operation %s, attempt %d, with one retry; the cluster reported Ready within restore_limit only after "+
		"its bootstrap completed, with BootstrapReady never True before; its members and Services held exactly "+
		"the rows acknowledged before the known point and S11's marker, no row written after the backup, and "+
		"took a normal write; the source was unchanged; no credential was in its status and no Secret of the "+
		"source in its Pods; the Instance Manager's and the Operator's logs were not checked", r.Pod,
		r.PhaseAtRestart, r.OperationIDBefore, r.MessageAtRestart, r.BackWithin, s12RetryReason,
		r.OperationIDAfter, attempts)
}

// imRestartPreparingPassReason is the reason of a pass at preparing.
func imRestartPreparingPassReason(r *s12IMRestart) string {
	attempts := int32(0)
	if r.AttemptsAfter != nil {
		attempts = *r.AttemptsAfter
	}
	staging := evidence.Unknown
	if k := r.KilledOperation; k != nil && k.StagingPresent != nil {
		staging = map[bool]string{true: "left", false: "not left"}[*k.StagingPresent]
	}
	return fmt.Sprintf("the Instance Manager on %s was killed while the cluster restored from S11's artifact was %s "+
		"with operation %s, which no reading showed the Operator read past Downloading; its container ran again on "+
		"the same Pod "+
		"%s after the kill; the Instance Manager's record of that operation shows it Failed by the restart with no "+
		"restored data, so restoredb had not succeeded (whether the download or restoredb ran at the kill was not "+
		"observed); the Operator ended that attempt with %s naming the Instance Manager's restart and completed "+
		"the restore under operation %s, attempt %d, with one retry, and the staging directory was %s on %s; the "+
		"cluster reported Ready within restore_limit only after its bootstrap completed, with BootstrapReady never "+
		"True before; its members and Services held exactly the rows acknowledged before the known point and S11's "+
		"marker, no row written after the backup, and took a normal write; the source was unchanged; no credential "+
		"was in its status and no Secret of the source in its Pods; the Instance Manager's and the Operator's logs "+
		"were not checked", r.Pod, r.PhaseAtRestart, r.OperationIDBefore, r.BackWithin, s12RetryReason,
		r.OperationIDAfter, attempts, staging, r.Pod)
}

// imRestartSeedingPassReason is the reason of a pass at seeding.
func imRestartSeedingPassReason(r *s12IMRestart) string {
	attempt, seedAfter := evidence.Unknown, evidence.Unknown
	switch {
	case r.SeedAttemptsAfter != nil:
		attempt = fmt.Sprint(*r.SeedAttemptsAfter)
	case r.SeedAttemptsBefore != nil:
		// No reading showed the member's seed restore under the next attempt.
		attempt = fmt.Sprintf("%d (inferred)", *r.SeedAttemptsBefore+1)
	}
	if r.SeedRestoreOperationIDAfter != "" {
		seedAfter = r.SeedRestoreOperationIDAfter
	}
	return fmt.Sprintf("the Instance Manager on %s was killed while it was seeded from the cluster restored from "+
		"S11's artifact, with seed restore operation %s (%q), its container ran again on the same Pod %s after the "+
		"kill, and the Operator ended that seed attempt with %s naming the Instance Manager's restart and seeded "+
		"%s again under seeding attempt %s (seed restore operation %s), with one seed retry, while the restore kept "+
		"operation %s with no retry after the kill; the cluster reported Ready within restore_limit only after its "+
		"bootstrap completed, with BootstrapReady never True before; its members and Services held exactly the rows "+
		"acknowledged before the known point and S11's marker, no row written after the backup, and took a normal "+
		"write; the source was unchanged; no credential was in its status and no Secret of the source in its Pods; "+
		"the Instance Manager's and the Operator's logs were not checked", r.Pod, r.SeedRestoreOperationIDBefore,
		r.MessageAtRestart, r.BackWithin, s12SeedRetryReason, r.Pod, attempt, seedAfter, r.OperationIDBefore)
}

// imRestartStartingPassReason is the reason of a pass at starting.
func imRestartStartingPassReason(r *s12IMRestart) string {
	attempts := int32(0)
	if r.AttemptsAfter != nil {
		attempts = *r.AttemptsAfter
	}
	placed, restored := evidence.Unknown, evidence.Unknown
	if p := r.PlacingReading; p != nil {
		placed = p.At.Format(time.RFC3339Nano)
	}
	if k := r.KilledOperation; k != nil && k.Restored != "" {
		restored = k.Restored
	}
	return fmt.Sprintf("the Instance Manager on %s was killed while the cluster restored from S11's artifact was %s "+
		"with operation %s (%q), right after the record of operation %s on %s was read Starting at %s: restoredb "+
		"had succeeded and the restored database was being started; its container ran again on the same Pod %s "+
		"after the kill; the Instance Manager's record of that operation shows it Failed by the restart and "+
		"holding the restored artifact %s; the Operator ended that attempt with %s naming the Instance Manager's "+
		"restart and completed the restore under operation %s, attempt %d, with one retry, whose record holds "+
		"the same restored artifact and which no reading showed running restoredb, so it took over the data "+
		"rather than restoring it again (a second restore of the same artifact between two readings would look "+
		"the same); the cluster reported Ready within restore_limit only after its bootstrap completed, with "+
		"BootstrapReady never True before; its members and Services held exactly the rows acknowledged before the "+
		"known point and S11's marker, no row written after the backup, and took a normal write; the source was "+
		"unchanged; no credential was in its status and no Secret of the source in its Pods; the Instance "+
		"Manager's and the Operator's logs were not checked", r.Pod, r.PhaseAtRestart, r.OperationIDBefore,
		r.MessageAtRestart, r.OperationIDBefore, r.Pod, placed, r.BackWithin, restored, s12RetryReason,
		r.OperationIDAfter, attempts)
}

// imRestartSeedBackupPassReason is the reason of a pass at seed-backup.
func imRestartSeedBackupPassReason(r *s12IMRestart) string {
	attempt, backupAfter, placed, state := evidence.Unknown, evidence.Unknown, evidence.Unknown, evidence.Unknown
	if r.SeedAttemptsAfter != nil {
		attempt = fmt.Sprint(*r.SeedAttemptsAfter)
	}
	if r.SeedBackupOperationIDAfter != "" {
		backupAfter = r.SeedBackupOperationIDAfter
	}
	if p := r.PlacingReading; p != nil {
		placed, state = p.At.Format(time.RFC3339Nano), p.State
	}
	return fmt.Sprintf("the Instance Manager on %s was killed while it took the seed backup for %s of the cluster "+
		"restored from S11's artifact, with its traffic to the object store limited, right after its record of "+
		"seed backup operation %s was read %s at %s with %d bytes through the limit since the first reading of "+
		"that record, and the limit was seen removed after the kill; its "+
		"container, and with it the restored database's server, ran again on the same Pod %s after the kill; the "+
		"Instance Manager's record of that backup shows it Failed by the restart; the Operator ended that seed "+
		"attempt with %s naming the Instance Manager's restart and seeded %s under seeding attempt %s with seed "+
		"backup operation %s, with one seed retry, while the restore kept operation %s with no retry after the "+
		"kill and no reading of restoredb running; the cluster reported Ready within restore_limit only after its "+
		"bootstrap completed, with BootstrapReady never True before; its members and Services held exactly the rows "+
		"acknowledged before the known point and S11's marker, no row written after the backup, and took a normal "+
		"write; the source was unchanged; no credential was in its status and no Secret of the source in its Pods; "+
		"the Instance Manager's and the Operator's logs were not checked", r.Pod, r.SeedPeer,
		r.SeedBackupOperationIDBefore, state, placed, r.SentBeforeKill, r.BackWithin, s12SeedBackupRetryReason,
		r.SeedPeer, attempt,
		backupAfter, r.OperationIDBefore)
}

// checkKilledSeedBackup reads, at seed-backup and once the cluster is Ready,
// the source's record of the seed backup at the kill. The record is on the
// source's volume and kept across the restart; the restarted Instance
// Manager fails every operation that had not ended with the restart's reason
// (reconcileOnStart in internal/instancemanager/opstore.go). A record that
// cannot be read, or did not end Failed by the restart, leaves what ended the
// backup unknown, which blocks the variant.
func (h *haRun) checkKilledSeedBackup(ctx context.Context, r *s12IMRestart, result *evidence.Scenario) error {
	k := &s12KilledOperation{}
	r.KilledSeedBackup = k
	id := r.SeedBackupOperationIDBefore
	op, err := h.readIMOperation(ctx, r.Pod, id)
	if err != nil {
		k.ReadError = err.Error()
		return fmt.Errorf("the record of the seed backup %s on %s could not be read, so what ended it is not "+
			"known: %w", id, r.Pod, err)
	}
	k.State, k.FailureReason = string(op.State), op.FailureReason
	failed := op.State == instancemanager.OpFailed && op.FailureReason == s12IMRestartedReason
	result.Measurements["killedBackupFailedByRestart"] = failed
	if !failed {
		return fmt.Errorf("the Instance Manager's record of the seed backup %s on %s is %s (%q), not Failed by its "+
			"restart: what ended the backup is not known", id, r.Pod, op.State, op.FailureReason)
	}
	return nil
}

// killedOperationScript prints the Instance Manager's record of operation
// id and then, on a line of its own, whether the staging directory of the
// recovery of the cluster uid is there. Both are plain names, checked by the
// caller, so that they name nothing outside the two directories.
func killedOperationScript(id, uid string) string {
	return "cat -- '" + imOperationsDir + "/" + id + ".json' && echo && if [ -e '" + imRestoreStagingRoot + "/" +
		uid + "' ]; then echo '" + imStagingLine + "present'; else echo '" + imStagingLine + "absent'; fi"
}

// plainName is what killedOperationScript takes as a name.
var plainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// checkKilledOperation reads, at preparing or starting and once the cluster is
// Ready, the target's record of the operation at the kill, and whether the
// recovery's staging directory (restoreStagingRoot/<cluster UID>,
// reconcileRecovery in internal/controller/bootstrap.go) is left there. The
// record is on the member's volume and kept across the restart (FindOrCreate,
// writeLocked in internal/instancemanager/opstore.go); its restored artifact
// is set only once restoredb had succeeded, and stays when the operation fails
// afterwards (Operation.Restored in internal/instancemanager/operation.go;
// reconcileOnStart changes only the state and the reason). So a record that
// holds a restored artifact shows the kill landed after restoredb, not while
// the restore was Preparing, and a record that cannot be read, is another
// operation's, or did not end Failed by the restart leaves that unknown: each
// blocks the variant. At starting the record must hold the restored artifact:
// one without it shows the kill landed before restoredb succeeded, which
// blocks the variant, and one with it is compared with the record of the
// operation that completed the restore (checkTakenOver). A successful restore
// removes the staging directory, and drops the error of that removal (Restore
// in internal/instancemanager/restore.go), so a directory left is recorded
// and not judged.
func (h *haRun) checkKilledOperation(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	rec *s12IMRestartRecord, result *evidence.Scenario) error {
	r := rec.IMRestart
	k := &s12KilledOperation{}
	r.KilledOperation = k
	id, uid := r.OperationIDBefore, string(cluster.UID)
	if !plainName.MatchString(id) || !plainName.MatchString(uid) {
		k.ReadError = "the operation ID or the cluster's UID is not a plain name"
		return fmt.Errorf("the record of operation %q on %s was not read: %s", id, r.Pod, k.ReadError)
	}
	out, err := h.member(ctx, r.Pod, "bash", "-c", killedOperationScript(id, uid))
	if err != nil {
		k.ReadError = err.Error()
		return fmt.Errorf("the record of operation %s on %s could not be read, so whether restoredb had succeeded "+
			"before the kill is not known: %w", id, r.Pod, err)
	}
	record, staging, found := strings.Cut(strings.TrimSpace(out), "\n"+imStagingLine)
	var op instancemanager.Operation
	if !found || json.Unmarshal([]byte(record), &op) != nil {
		k.ReadError = "the answer is not a record followed by the staging line"
		return fmt.Errorf("the record of operation %s on %s could not be read, so whether restoredb had succeeded "+
			"before the kill is not known: %s", id, r.Pod, k.ReadError)
	}
	if op.ID != id {
		k.ReadError = fmt.Sprintf("the record read is of operation %q", op.ID)
		return fmt.Errorf("the record read for operation %s on %s is of operation %q", id, r.Pod, op.ID)
	}
	k.State, k.FailureReason = string(op.State), op.FailureReason
	if op.Restored != nil {
		k.Restored = op.Restored.ManifestURI
	}
	present := strings.TrimSpace(staging) == "present"
	k.StagingPresent = &present
	result.Measurements["restoredAtKill"], result.Measurements["stagingLeftAtEnd"] = op.Restored != nil, present
	switch {
	case op.State != instancemanager.OpFailed || op.FailureReason != s12IMRestartedReason:
		return fmt.Errorf("the Instance Manager's record of operation %s on %s is %s (%q), not Failed by its "+
			"restart: what ended the operation is not known", id, r.Pod, op.State, op.FailureReason)
	case r.At == IMRestartAtStarting && op.Restored == nil:
		return fmt.Errorf("the kill landed before restoredb succeeded, not while the restored database was being "+
			"started: the Instance Manager's record of operation %s on %s holds no restored artifact", id, r.Pod)
	case r.At == IMRestartAtStarting:
		return h.checkTakenOver(ctx, rec, op.Restored, result)
	case op.Restored != nil:
		return fmt.Errorf("the kill did not land while the restore was Preparing: the Instance Manager's record of "+
			"operation %s on %s holds the restored artifact %s, which it records only once restoredb succeeded", id,
			r.Pod, op.Restored.ManifestURI)
	}
	return nil
}

// imRestoredbRunning starts the Operator's BootstrapReady message while the
// Instance Manager answers its operation Restoring, restoredb about to run or
// running (setRestoreProgress in internal/controller/bootstrap.go).
const imRestoredbRunning = "restoredb is running on "

// checkTakenOver reads, at starting and once the cluster is Ready, the
// target's record of the operation that completed the restore, and compares
// its restored artifact with killed, the one the killed operation recorded.
// The next attempt's operation takes over data a failed attempt of the same
// request recorded as restored: it records the same artifact and starts the
// data, without running restoredb, so without ever being Restoring
// (adoptRestored in internal/instancemanager/ownership.go, runRestore in
// internal/instancemanager/server.go). So a completed record holding another
// artifact, or none, and a reading of that operation showing restoredb
// running, each show the data restored again instead of taken over, which
// fails the variant. A record that cannot be read, is another operation's or
// is not Completed blocks it. The same artifact does not rule out a second
// restore of it between two readings.
func (h *haRun) checkTakenOver(ctx context.Context, rec *s12IMRestartRecord, killed *instancemanager.OperationArtifact,
	result *evidence.Scenario) error {
	r := rec.IMRestart
	c := &s12KilledOperation{}
	r.CompletingOperation = c
	// The operation the last reading named, as judgeIMRestartBootstrap takes
	// it later.
	id := ""
	for _, p := range rec.Bootstrap {
		id = cmp.Or(p.OperationID, id)
	}
	op, err := h.readIMOperation(ctx, r.Pod, id)
	if err != nil {
		c.ReadError = err.Error()
		return fmt.Errorf("the record of operation %s, which completed the restore, on %s could not be read, so "+
			"whether it took over the restored data is not known: %w", id, r.Pod, err)
	}
	c.State, c.FailureReason = string(op.State), op.FailureReason
	if op.Restored != nil {
		c.Restored = op.Restored.ManifestURI
	}
	if op.State != instancemanager.OpCompleted {
		return fmt.Errorf("the Instance Manager's record of operation %s on %s, which completed the restore, is %s "+
			"(%q), not Completed: whether it took over the restored data is not known", id, r.Pod, op.State,
			op.FailureReason)
	}
	again := ""
	for _, p := range rec.Bootstrap {
		if boot := meta.FindStatusCondition(p.Conditions, bootstrapReadyCondition); p.OperationID == id &&
			boot != nil && strings.HasPrefix(boot.Message, imRestoredbRunning) {
			again = fmt.Sprintf("a reading at %s showed operation %s with %q", p.At.Format(time.RFC3339), id,
				boot.Message)
			break
		}
	}
	same := op.Restored != nil && *op.Restored == *killed
	result.Measurements["tookOverRestoredData"] = same && again == ""
	switch {
	case !same:
		return restoreVerdict{fmt.Errorf("the data was restored again instead of taken over: the record of "+
			"operation %s on %s holds the restored artifact %q, not %s, which the killed operation %s recorded", id,
			r.Pod, c.Restored, killed.ManifestURI, r.OperationIDBefore)}
	case again != "":
		return restoreVerdict{fmt.Errorf("the data was restored again instead of taken over: %s", again)}
	}
	return nil
}

// killDuringRestore kills the Instance Manager of the member the placement
// names, once a reading right before shows the bootstrap at that placement
// and the member's Pod and container are recorded (beforeKill), and waits for
// the container to run again, until RestoreLimit has passed since
// createdAt. While it waits, the restored cluster is read and recorded.
func (h *haRun) killDuringRestore(ctx context.Context, cluster *databasev1alpha1.CubridCluster, createdAt time.Time,
	rec *s12IMRestartRecord, result *evidence.Scenario) error {
	r := rec.IMRestart
	// seen is a verdict a reading of the cluster gave after the kill.
	var seen error
	outcome := faults.Run(ctx, faults.Scenario{
		Fault: instanceManagerKill{h: h, k: &r.memberKill, name: imKillName(r.At)},
		Limits: faults.Limits{Confirm: min(2*time.Minute, h.cfg.RestoreLimit),
			Outcome: max(time.Until(createdAt.Add(h.cfg.RestoreLimit)), h.cfg.Poll), Cleanup: time.Minute,
			Poll: h.cfg.Poll},
		Before: func(ctx context.Context) error { return h.beforeKill(ctx, cluster, rec, result) },
		Check: func(ctx context.Context) (bool, error) {
			if err := h.readDuringRestart(ctx, cluster, &rec.s12Record, &r.PhaseAfterRestart, result); err != nil {
				seen = err
				return false, err
			}
			done, err := h.memberBack(ctx, &r.memberKill)
			if ctx.Err() != nil {
				// The step's limit, or the run's: not a broken expectation.
				return false, nil
			}
			return done, err
		},
	})
	if !outcome.FaultIssuedAt.IsZero() {
		at := outcome.FaultIssuedAt
		r.KilledAt = &at
	}
	if err := h.writeTimeline(r.file("timeline.jsonl"), outcome.Timeline); err != nil {
		return err
	}
	switch {
	case r.EndedOtherwise != "":
		return fmt.Errorf("the container ended for another reason than the kill: %s", r.EndedOtherwise)
	case seen != nil:
		return seen
	case r.PodReplaced:
		return fmt.Errorf("the Pod %s of the member killed was replaced, not only its container restarted: %s", r.Pod,
			outcome.Reason)
	case outcome.Result == evidence.Fail && r.readsAfterKill == 0 && r.lastReadErr != nil:
		return fmt.Errorf("the Pod %s could not be read after the kill was confirmed, so whether its container ran "+
			"again is not known: %w", r.Pod, r.lastReadErr)
	case outcome.Result == evidence.Fail:
		return restoreVerdict{fmt.Errorf("the container of %s did not run again within restore_limit %s after its "+
			"Instance Manager was killed: %s", r.Pod, h.cfg.RestoreLimit, outcome.Reason)}
	case outcome.Result != evidence.Pass:
		return errors.New(outcome.Reason)
	}
	r.BackWithin = outcome.OutcomeAt.Sub(outcome.FaultIssuedAt).Round(time.Millisecond).String()
	result.Measurements["memberBackWithin"] = r.BackWithin
	return nil
}

// beforeKill requires a reading of the restored cluster at the placement of
// the kill (killedMember), which it records as the state at the kill, and the
// member it names: a Pod of this cluster's StatefulSet, which the cluster
// owns, with its database container running. The Pod's UID and the
// container's ID and restart count are recorded; the kill is issued only
// against them.
func (h *haRun) beforeKill(ctx context.Context, created *databasev1alpha1.CubridCluster, rec *s12IMRestartRecord,
	result *evidence.Scenario) error {
	r := rec.IMRestart
	cluster, err := h.readRestarted(ctx, created)
	if err != nil {
		return err
	}
	phase, err := h.restoredStatus(cluster, &rec.s12Record, result)
	if err != nil {
		return err
	}
	boot := cluster.Status.Bootstrap
	member, err := killedMember(cluster, r.At, phase)
	if err != nil {
		return err
	}
	sts := &appsv1.StatefulSet{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), sts); err != nil {
		return fmt.Errorf("read the StatefulSet of %s: %w", cluster.Name, err)
	}
	if !controllerOwner(sts, "CubridCluster", cluster.UID) {
		return fmt.Errorf("the StatefulSet %s does not belong to the restored cluster", sts.Name)
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: member}, pod); err != nil {
		return fmt.Errorf("read the member %s: %w", member, err)
	}
	if !controllerOwner(pod, "StatefulSet", sts.UID) {
		return fmt.Errorf("pod %s does not belong to the restored cluster's StatefulSet", pod.Name)
	}
	if err := r.recordBefore(pod); err != nil {
		return err
	}
	if p := r.PlacingReading; r.At == IMRestartAtStarting && (p == nil || p.Operation != boot.OperationID) {
		return fmt.Errorf("the kill could not be placed while the restored database was being started: right "+
			"before it the bootstrap named operation %q, not the one whose record was read Starting", boot.OperationID)
	}
	attempts := boot.Attempts
	r.PhaseAtRestart, r.OperationIDBefore, r.AttemptsBefore = phase, boot.OperationID, &attempts
	if c := meta.FindStatusCondition(cluster.Status.Conditions, bootstrapReadyCondition); c != nil {
		r.MessageAtRestart = c.Message
	}
	if r.At == IMRestartAtSeeding {
		dbStatus := firstDatabaseStatus(cluster)
		seedAttempts := dbStatus.BootstrapAttempts
		r.SeedRestoreOperationIDBefore, r.SeedAttemptsBefore = dbStatus.Seeding.RestoreOperationID, &seedAttempts
	}
	if r.At == IMRestartAtSeedBackup {
		dbStatus := firstDatabaseStatus(cluster)
		if p := r.PlacingReading; p == nil || p.Operation != dbStatus.Seeding.BackupOperationID {
			return fmt.Errorf(imNotAtSeedBackup+"right before it the seeding named the seed backup %q, not the one "+
				"whose record was read running", dbStatus.Seeding.BackupOperationID)
		}
		seedAttempts := dbStatus.BootstrapAttempts
		r.SeedPeer, r.SeedBackupOperationIDBefore, r.SeedAttemptsBefore = dbStatus.Seeding.Member,
			dbStatus.Seeding.BackupOperationID, &seedAttempts
	}
	r.restartReading = len(rec.Bootstrap) - 1
	return nil
}

// killedMember is the member whose Instance Manager the placement at kills,
// from a reading of cluster in phase right before the kill: the restore's
// target, in Restoring or Preparing with an operation, or the member being
// seeded, in SeedingReplicas with a seed restore recorded for it. That member
// is never the restore's target, which is not seeded over (reconcileSeeding
// in internal/controller/habootstrap.go); or, at seed-backup, the source of
// the seed backup, the restore's target, in SeedingReplicas with a seed
// backup and no seed restore recorded. Any other reading, or a member that is not one of the cluster's,
// is an error that blocks the variant.
func killedMember(cluster *databasev1alpha1.CubridCluster, at, phase string) (string, error) {
	boot := cluster.Status.Bootstrap
	member := boot.TargetMember
	switch at {
	case IMRestartAtPreparing:
		if phase != string(databasev1alpha1.BootstrapPreparing) || boot.OperationID == "" {
			return "", fmt.Errorf("the kill could not be placed while the restore was Preparing: right before it "+
				"the bootstrap phase was %q", cmp.Or(phase, noBootstrapPhase))
		}
	case IMRestartAtSeeding:
		seeding := firstDatabaseSeeding(cluster)
		if phase != string(databasev1alpha1.BootstrapSeedingReplicas) || seeding == nil || seeding.Member == "" ||
			seeding.RestoreOperationID == "" {
			return "", fmt.Errorf("the kill could not be placed during a seed restore: right before it the "+
				"bootstrap phase was %q with no seed restore recorded", cmp.Or(phase, noBootstrapPhase))
		}
		if seeding.Member == boot.TargetMember {
			return "", fmt.Errorf("the member being seeded, %s, is the restore's target", seeding.Member)
		}
		member = seeding.Member
	case IMRestartAtSeedBackup:
		seeding := firstDatabaseSeeding(cluster)
		if phase != string(databasev1alpha1.BootstrapSeedingReplicas) || seeding == nil || seeding.Member == "" ||
			seeding.BackupOperationID == "" || seeding.RestoreOperationID != "" {
			return "", fmt.Errorf(imNotAtSeedBackup+"right before it the bootstrap phase was %q without a seed "+
				"backup recorded alone", cmp.Or(phase, noBootstrapPhase))
		}
		if seeding.Source == seeding.Member {
			return "", fmt.Errorf("the seed backup's source, %s, is the member being seeded", seeding.Source)
		}
		// The placement is the restored member's backup for the first peer.
		if seeding.Source != boot.TargetMember {
			return "", fmt.Errorf(imNotAtSeedBackup+"the seed backup's source is %s, not the restored member %s",
				seeding.Source, boot.TargetMember)
		}
		member = seeding.Source
	default:
		if phase != string(databasev1alpha1.BootstrapRestoring) || boot.OperationID == "" {
			return "", fmt.Errorf("the restart could not be placed during the restore: right before it the "+
				"bootstrap phase was %q", cmp.Or(phase, noBootstrapPhase))
		}
	}
	if !slices.Contains(clusterMembers(cluster.Name), member) {
		if at == IMRestartAtSeeding {
			return "", fmt.Errorf("the member being seeded %q is not a member of %s", member, cluster.Name)
		}
		return "", fmt.Errorf("the restore's target member %q is not a member of %s", member, cluster.Name)
	}
	return member, nil
}

// databaseContainerStatus returns the status of a member Pod's database
// container, or nil.
func databaseContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == databaseContainer {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// instanceManagerKill kills the Instance Manager of the member k names
// (imKillScript). A kubectl exec carries no precondition, so the Pod's UID
// and its container's ID and restart count are read again right before, and
// the kill is not issued unless they are those recorded before it
// (memberKill.recordBefore); the container can still be replaced between
// that read and the exec. The kill is permanent: the kubelet starts the
// container again on its own. name says what is killed, for the timeline;
// script is the command that kills it, imKillScript when empty.
type instanceManagerKill struct {
	h      *haRun
	k      *memberKill
	name   string
	script string
}

// imKillName is the name of the kill at the S12 placement at.
func imKillName(at string) string {
	switch at {
	case IMRestartAtSeeding:
		return "kill the Instance Manager of the member being seeded with SIGKILL"
	case IMRestartAtSeedBackup:
		return "kill the Instance Manager of the seed backup's source with SIGKILL"
	}
	return "kill the Instance Manager of the restore's target member with SIGKILL"
}

func (f instanceManagerKill) Name() string { return f.name }

func (f instanceManagerKill) Inject(ctx context.Context) error {
	pod := &corev1.Pod{}
	if err := f.h.Client.Get(ctx, client.ObjectKey{Namespace: f.h.cfg.Namespace, Name: f.k.Pod}, pod); err != nil {
		return fmt.Errorf("read %s again before the kill: %w", f.k.Pod, err)
	}
	status := databaseContainerStatus(pod)
	if string(pod.UID) != f.k.PodUID || status == nil || status.ContainerID != f.k.ContainerBefore ||
		status.RestartCount != *f.k.RestartsBefore {
		return fmt.Errorf("the Pod %s or its database container is not the one read right before the kill", f.k.Pod)
	}
	out, err := f.h.member(ctx, f.k.Pod, "bash", "-c", cmp.Or(f.script, imKillScript))
	answer := strings.TrimSpace(out)
	if err != nil {
		// The container may end before the command answers; Confirm decides.
		answer = strings.TrimSpace(answer + " " + err.Error())
	}
	f.k.KillAnswer = answer
	if strings.Contains(answer, imKillRefused) {
		return fmt.Errorf("the Instance Manager of %s could not be told apart, nothing was killed: %s", f.k.Pod, answer)
	}
	return nil
}

// Confirm reports the kill once the Pod's database container is seen ended
// or restarted: a status that names another container or a higher restart
// count. A Pod with another UID was replaced, which is not this fault.
func (f instanceManagerKill) Confirm(ctx context.Context) (bool, error) {
	pod := &corev1.Pod{}
	if err := f.h.Client.Get(ctx, client.ObjectKey{Namespace: f.h.cfg.Namespace, Name: f.k.Pod}, pod); err != nil {
		return false, err
	}
	if string(pod.UID) != f.k.PodUID {
		f.k.PodReplaced = true
		return false, fmt.Errorf("the Pod %s was replaced", f.k.Pod)
	}
	status := databaseContainerStatus(pod)
	if status == nil {
		return false, nil
	}
	ended := status.State.Terminated != nil && status.ContainerID == f.k.ContainerBefore
	if !ended && status.ContainerID == f.k.ContainerBefore && status.RestartCount == *f.k.RestartsBefore {
		return false, nil
	}
	at := time.Now().UTC()
	f.k.EndedAt = &at
	for _, t := range []*corev1.ContainerStateTerminated{status.State.Terminated,
		status.LastTerminationState.Terminated} {
		if t != nil && (t.ContainerID == "" || t.ContainerID == f.k.ContainerBefore) {
			code, finished := t.ExitCode, t.FinishedAt.UTC()
			f.k.ExitCode, f.k.ExitReason, f.k.FinishedAt = &code, t.Reason, &finished
			break
		}
	}
	// The shell exits with the killed manager's status, 128 + SIGKILL. Any
	// other end, or one the kubelet reports as OOMKilled, is not the kill's.
	switch {
	case f.k.ExitCode == nil:
		f.k.EndedOtherwise = "its exit code was not reported"
	case *f.k.ExitCode != sigkillExitCode || f.k.ExitReason == oomKilledReason:
		f.k.EndedOtherwise = fmt.Sprintf("exit code %d, reason %q", *f.k.ExitCode, f.k.ExitReason)
	}
	return true, nil
}

func (f instanceManagerKill) Remove(context.Context) error { return nil }

// memberBack reports whether the killed member's database container runs
// again: another container than the killed one, running, with a higher
// restart count, on the same Pod. A Pod with another UID is an error. A read
// that fails is remembered, and the successful ones are counted.
func (h *haRun) memberBack(ctx context.Context, r *memberKill) (bool, error) {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: r.Pod}, pod); err != nil {
		// Not judged here; a wait in which no read succeeded blocks.
		r.lastReadErr = err
		return false, nil
	}
	r.readsAfterKill++
	if string(pod.UID) != r.PodUID {
		r.PodReplaced = true
		return false, fmt.Errorf("the Pod %s was replaced", r.Pod)
	}
	status := databaseContainerStatus(pod)
	if status == nil || status.State.Running == nil || status.ContainerID == r.ContainerBefore ||
		status.RestartCount <= *r.RestartsBefore {
		return false, nil
	}
	restarts, started, back := status.RestartCount, status.State.Running.StartedAt.UTC(), time.Now().UTC()
	r.ContainerAfter, r.RestartsAfter, r.StartedAfter, r.BackAt = status.ContainerID, &restarts, &started, &back
	return true, nil
}

// readRetryEvents records the messages of the Operator's RestoreFailedRetrying
// Events of the cluster (restoreAttemptFailed in internal/controller/
// bootstrap.go records one with the condition), and at seeding those of its
// SeedRestoreFailedRetrying Events (bootstrapStepFailed in internal/
// controller/habootstrap.go). An Event list that cannot
// be read is recorded: the Events only back a retry the readings missed.
func (h *haRun) readRetryEvents(ctx context.Context, cluster *databasev1alpha1.CubridCluster, r *s12IMRestart) {
	events := &corev1.EventList{}
	if err := h.Client.List(ctx, events, client.InNamespace(h.cfg.Namespace)); err != nil {
		r.EventsError = err.Error()
		return
	}
	for _, event := range events.Items {
		if event.InvolvedObject.UID != cluster.UID {
			continue
		}
		switch {
		case event.Reason == s12RetryReason:
			r.RetryEvents = append(r.RetryEvents, event.Message)
		case seedPlacement(r.At) && event.Reason == seedRetryReasonAt(r.At):
			r.SeedRetryEvents = append(r.SeedRetryEvents, event.Message)
		}
	}
}

// targetRestartsAtEnd reads the member killed once the cluster is Ready and
// records its restart count. A replaced Pod blocks the variant; more than
// one restart after one kill fails it: the container did not stay up.
func (h *haRun) targetRestartsAtEnd(ctx context.Context, r *memberKill, result *evidence.Scenario) error {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: r.Pod}, pod); err != nil {
		return fmt.Errorf("read the member killed, %s, at the end: %w", r.Pod, err)
	}
	if string(pod.UID) != r.PodUID {
		r.PodReplaced = true
		return fmt.Errorf("the Pod %s of the member killed was replaced after its container restarted", r.Pod)
	}
	status := databaseContainerStatus(pod)
	if status == nil {
		return fmt.Errorf("the member killed, %s, has no database container status at the end", r.Pod)
	}
	restarts := status.RestartCount
	r.RestartsAtEnd = &restarts
	added := restarts - *r.RestartsBefore
	result.Measurements["containerRestarts"] = added
	if added != 1 {
		event := fmt.Sprintf("the database container of %s restarted %d times after one kill of its Instance "+
			"Manager, not once", r.Pod, added)
		result.NeverEvents = append(result.NeverEvents, event)
		return restoreVerdict{errors.New(event)}
	}
	return nil
}

// judgeIMRestartBootstrap judges the bootstrap the readings recorded, in
// order. The Operator polls the recorded operation by its ID; the Instance
// Manager keeps its operation records on the member's data volume
// (IM_OPERATIONS_DIR under the data mount, internal/controller/
// cubridcluster_controller.go) and, at its start, fails each one that had
// not ended with s12IMRestartedReason (reconcileOnStart in
// internal/instancemanager/opstore.go). The Operator then ends the attempt
// with RestoreFailedRetrying, raises attempts and clears the operation, and
// starts the next attempt under a new idempotency key, a new operation
// (restoreAttemptFailed, recoveryIdempotencyKey in internal/controller/
// bootstrap.go). So these must never happen: BootstrapReady True before
// Complete; attempts that fall or reach s12MaxAttempts; an operation that
// becomes another under the same attempt (a restore started twice); the
// target answering that it holds no record of the operation
// (RecordedOperationMissing): the record was on the volume before its ID was
// answered (FindOrCreate, writeLocked), so a container restart that kept the
// volume must not lose it, and the Operator would wait 30 minutes and fail
// the recovery (restoreOperationMissing). Once the cluster completed, after
// the kill there must be exactly one retry, from the attempt at the kill to
// the next, whose message names the Instance Manager's restart, and the
// operation that completed must be another than the one at the kill. A
// second retry is a restore that failed again and fails the variant. No
// retry with the operation of the kill at the end means the kill did not
// interrupt a running restore, and a first retry for another reason leaves
// the restart's effect unobserved: both block it, as attempts that rose
// without the reason RestoreFailedRetrying do.
func judgeIMRestartBootstrap(rec *s12IMRestartRecord, result *evidence.Scenario) error {
	r := rec.IMRestart
	events, unexplained, afterKill := scanIMRestartBootstrap(rec)
	if r.KilledAt != nil {
		result.Measurements[measRetriesAfterKill] = len(afterKill)
	}
	if r.OperationIDBefore != "" && r.OperationIDAfter != "" {
		result.Measurements["operationIDChanged"] = r.OperationIDBefore != r.OperationIDAfter
		result.Measurements["attemptsAdded"] = *r.AttemptsAfter - *r.AttemptsBefore
	}
	result.NeverEvents = append(result.NeverEvents, events...)
	var errs []error
	if len(unexplained) > 0 {
		errs = append(errs, fmt.Errorf("the restored cluster's bootstrap attempts rose %s, and no reading showed the "+
			"reason %s: the cause is unknown", strings.Join(unexplained, ", "), s12RetryReason))
	}
	switch {
	case seedPlacement(r.At):
		errs = append(errs, judgeSeedingKill(rec, result, afterKill))
	// The expectation of the kill is judged on a restore that completed.
	case r.KilledAt != nil && r.OperationIDBefore != "" && rec.RestoredWithin != "":
		errs = append(errs, judgeKillRetries(r, afterKill))
	}
	if r.At == IMRestartAtPreparing {
		errs = append(errs, movedOnBeforeKill(rec))
	}
	return errors.Join(errs...)
}

// movedOnBeforeKill says, at preparing, whether a reading after the kill and
// under the attempt and operation at the kill showed the bootstrap past
// Preparing. The Operator sets the phase from the state the Instance Manager
// answers for the operation (setRestoreProgress in internal/controller/
// bootstrap.go); a restarted Instance Manager answers Failed for it
// (reconcileOnStart in internal/instancemanager/opstore.go), which ends the
// attempt without changing the phase (restoreAttemptFailed). Such a reading
// therefore shows an answer read before the kill took effect: the kill did
// not land while the restore was Preparing, which blocks the variant.
func movedOnBeforeKill(rec *s12IMRestartRecord) error {
	r := rec.IMRestart
	if r.KilledAt == nil || r.restartReading < 0 || r.AttemptsBefore == nil {
		return nil
	}
	for _, p := range rec.Bootstrap[r.restartReading+1:] {
		if p.Attempts != *r.AttemptsBefore || p.OperationID != r.OperationIDBefore || p.Phase == "" ||
			p.Phase == string(databasev1alpha1.BootstrapPreparing) {
			continue
		}
		return fmt.Errorf("the kill did not land while the restore was Preparing: a reading after it showed the "+
			"bootstrap %s with operation %s under attempt %d, which the Operator sets only from the Instance "+
			"Manager's answer read before the kill took effect", p.Phase, p.OperationID, p.Attempts)
	}
	return nil
}

// judgeSeedingKill judges, at seeding, the readings from the seed's side
// (scanSeedRetries) and the retries of the restore after the kill. Once the
// restore is recorded as complete on its target, the Operator only seeds the
// other members and never polls or starts the restore again
// (reconcileRecovery in internal/controller/bootstrap.go), so a restore retry
// after the kill must never happen. On a cluster that completed, the kill's
// seed retries are judged (judgeSeedKillRetries).
func judgeSeedingKill(rec *s12IMRestartRecord, result *evidence.Scenario, afterKill []s12Retry) error {
	r := rec.IMRestart
	events, unexplained, seedAfterKill := scanSeedRetries(rec)
	if r.KilledAt != nil {
		result.Measurements[measSeedRetriesAfterKill] = len(seedAfterKill)
	}
	if r.SeedAttemptsBefore != nil && r.SeedAttemptsAfter != nil {
		result.Measurements[measSeedAttemptsAdded] = *r.SeedAttemptsAfter - *r.SeedAttemptsBefore
	}
	if r.SeedRestoreOperationIDBefore != "" && r.SeedRestoreOperationIDAfter != "" {
		result.Measurements[measSeedRestoreOperationChanged] = r.SeedRestoreOperationIDBefore != r.SeedRestoreOperationIDAfter
	}
	if r.SeedBackupOperationIDBefore != "" && r.SeedBackupOperationIDAfter != "" {
		result.Measurements["seedBackupOperationChanged"] = r.SeedBackupOperationIDBefore != r.SeedBackupOperationIDAfter
	}
	if r.KilledAt != nil && len(afterKill) > 0 {
		events = append(events, fmt.Sprintf("the restore was retried after the kill, after it had completed and "+
			"seeding had begun: %s", afterKill[0].Message))
	}
	if again := restoredbAfterKill(rec); r.At == IMRestartAtSeedBackup && again != "" {
		events = append(events, "the restore ran again after the kill, after it had completed and seeding had "+
			"begun: "+again)
	}
	// What must never happen fails the variant through the never-events.
	result.NeverEvents = append(result.NeverEvents, events...)
	var errs []error
	if len(unexplained) > 0 {
		errs = append(errs, fmt.Errorf("the restored cluster's seeding attempts rose %s, and no reading showed the "+
			"reason %s or %s: the cause is unknown", strings.Join(unexplained, ", "), s12SeedRetryReason,
			s12SeedBackupRetryReason))
	}
	if r.KilledAt != nil && (r.SeedRestoreOperationIDBefore != "" || r.SeedBackupOperationIDBefore != "") &&
		rec.RestoredWithin != "" {
		errs = append(errs, judgeSeedKillRetries(r, seedAfterKill))
	}
	return errors.Join(errs...)
}

// restoredbAfterKill describes the first reading after the kill whose
// BootstrapReady message says restoredb is running (imRestoredbRunning), or
// is empty. Once the restore completed on its target the Operator never
// starts it again, so after a kill during seeding no reading may show it.
func restoredbAfterKill(rec *s12IMRestartRecord) string {
	r := rec.IMRestart
	if r.KilledAt == nil || r.restartReading < 0 {
		return ""
	}
	for _, p := range rec.Bootstrap[r.restartReading+1:] {
		if boot := meta.FindStatusCondition(p.Conditions, bootstrapReadyCondition); boot != nil &&
			strings.HasPrefix(boot.Message, imRestoredbRunning) {
			return fmt.Sprintf("a reading at %s showed %q", p.At.Format(time.RFC3339), boot.Message)
		}
	}
	return ""
}

// scanSeedRetries records, from the readings in order, the seed retries and
// the killed member's seed restore after the kill. The Operator raises the
// database's attempts (status.databases[].bootstrapAttempts) when a seed
// backup or a seed restore failed, with the reason SeedBackupFailedRetrying
// or SeedRestoreFailedRetrying, clears the seeding, and starts that step
// again under the next attempt's key; it stops raising them below
// maxBootstrapAttempts (bootstrapStepFailed, seedMember in internal/
// controller/habootstrap.go). A seed restore, once recorded for a member, is
// only polled (seedMember). So these must never happen: the database's
// attempts falling or reaching s12MaxAttempts, and the seed restore of a
// member becoming another under the same attempts. It returns those, the
// rises without either reason, and the seed retries after the kill.
func scanSeedRetries(rec *s12IMRestartRecord) (events, unexplained []string, afterKill []s12Retry) {
	r := rec.IMRestart
	r.SeedRetries = []s12Retry{}
	var rises []attemptRise
	attempts := int32(0)
	ops, opAttempts := map[string]string{}, map[string]int32{}
	beyond := false
	after := func(i int) bool { return r.KilledAt != nil && r.restartReading >= 0 && i > r.restartReading }
	for i, p := range rec.Bootstrap {
		boot := meta.FindStatusCondition(p.Conditions, bootstrapReadyCondition)
		switch {
		case i > 0 && p.DatabaseAttempts < attempts:
			events = append(events, fmt.Sprintf("the restored cluster's seeding attempts went from %d back to %d",
				attempts, p.DatabaseAttempts))
		case p.DatabaseAttempts > attempts && boot != nil &&
			(boot.Reason == s12SeedRetryReason || boot.Reason == s12SeedBackupRetryReason):
			retry := s12Retry{At: p.At, From: attempts, To: p.DatabaseAttempts, Reason: boot.Reason,
				Message: boot.Message}
			r.SeedRetries = append(r.SeedRetries, retry)
			if after(i) {
				afterKill = append(afterKill, retry)
			}
		case p.DatabaseAttempts > attempts:
			rises = append(rises, attemptRise{index: i, at: p.At, from: attempts, to: p.DatabaseAttempts})
		}
		attempts = p.DatabaseAttempts
		if p.DatabaseAttempts >= s12MaxAttempts && !beyond {
			beyond = true
			events = append(events, fmt.Sprintf("the restored cluster's seeding attempts reached %d, beyond the "+
				"Operator's %d attempts", p.DatabaseAttempts, s12MaxAttempts))
		}
		if after(i) {
			seedBackupAfterKill(r, p)
		}
		if p.SeedMember == "" || p.SeedRestoreOperationID == "" {
			continue
		}
		if last, ok := ops[p.SeedMember]; ok && last != p.SeedRestoreOperationID &&
			opAttempts[p.SeedMember] == p.DatabaseAttempts {
			events = append(events, fmt.Sprintf("the seed restore of %s was started again without a failed attempt: "+
				"the operation %s became %s under seeding attempt %d", p.SeedMember, last, p.SeedRestoreOperationID,
				p.DatabaseAttempts))
		}
		ops[p.SeedMember], opAttempts[p.SeedMember] = p.SeedRestoreOperationID, p.DatabaseAttempts
		if r.At == IMRestartAtSeeding && p.SeedMember == r.Pod && after(i) && r.SeedAttemptsBefore != nil &&
			p.DatabaseAttempts > *r.SeedAttemptsBefore {
			seedAttempts := p.DatabaseAttempts
			r.SeedRestoreOperationIDAfter, r.SeedAttemptsAfter = p.SeedRestoreOperationID, &seedAttempts
		}
	}
	if len(afterKill) == 0 && len(rises) == 1 && after(rises[0].index) {
		if retry := seedRetryFromEvent(r, rises[0]); retry != nil {
			afterKill, rises = append(afterKill, *retry), nil
		}
	}
	for _, x := range rises {
		unexplained = append(unexplained, fmt.Sprintf("from %d to %d", x.from, x.to))
	}
	r.seedRetriesAfterKill = len(afterKill)
	return events, unexplained, afterKill
}

// seedBackupAfterKill records, at seed-backup, the peer's seed backup and the
// database's attempts of p, a reading after the kill, when p shows a seed
// backup recorded for the peer under a later attempt than the one at the
// kill.
func seedBackupAfterKill(r *s12IMRestart, p s12Phase) {
	if r.At != IMRestartAtSeedBackup || p.SeedMember != r.SeedPeer || p.SeedBackupOperationID == "" ||
		r.SeedAttemptsBefore == nil || p.DatabaseAttempts <= *r.SeedAttemptsBefore {
		return
	}
	seedAttempts := p.DatabaseAttempts
	r.SeedBackupOperationIDAfter, r.SeedAttemptsAfter = p.SeedBackupOperationID, &seedAttempts
}

// seedRetryFromEvent returns the seed retry x is, and records it, when an
// Event of the Operator's names the restart on the killed member and the
// attempt x starts, as retryFromEvent does for the restore: x must be by one
// from the database's attempts at the kill.
func seedRetryFromEvent(r *s12IMRestart, x attemptRise) *s12Retry {
	if r.SeedAttemptsBefore == nil || x.from != *r.SeedAttemptsBefore || x.to != x.from+1 {
		return nil
	}
	starting := fmt.Sprintf("starting attempt %d of ", x.to+1)
	for _, message := range r.SeedRetryEvents {
		if strings.HasPrefix(message, "on "+r.Pod+": ") && strings.Contains(message, cmp.Or(r.cause, s12IMRestartedReason)) &&
			strings.Contains(message, starting) {
			retry := s12Retry{At: x.at, From: x.from, To: x.to, Reason: seedRetryReasonAt(r.At), Message: message,
				FromEvent: true}
			r.SeedRetries = append(r.SeedRetries, retry)
			slices.SortStableFunc(r.SeedRetries, func(a, b s12Retry) int { return a.At.Compare(b.At) })
			return &retry
		}
	}
	return nil
}

// judgeSeedKillRetries judges the seed retries after the kill of a cluster
// that completed: exactly one, the failed seed step on the killed member
// naming the Instance Manager's restart (a seed restore at seeding, a seed
// backup on the source at seed-backup), and, when a reading showed that
// step's operation under the next attempt, another operation than the one at
// the kill. The Instance Manager fails the operation at its start
// (reconcileOnStart), the Operator reads it Failed and retries the seed step
// (seedMember, seedBackup), and the next attempt's key creates a new
// operation (bootstrapKey, FindOrCreate).
func judgeSeedKillRetries(r *s12IMRestart, afterKill []s12Retry) error {
	step, before, after := "seed restore", r.SeedRestoreOperationIDBefore, r.SeedRestoreOperationIDAfter
	if r.At == IMRestartAtSeedBackup {
		step, before, after = "seed backup", r.SeedBackupOperationIDBefore, r.SeedBackupOperationIDAfter
	}
	switch {
	case len(afterKill) == 0:
		return fmt.Errorf("the kill did not interrupt the %s: %s was seeded with no failed seed attempt, so "+
			"operation %s had ended before the Instance Manager of %s was killed", step, seededPeer(r), before, r.Pod)
	case len(afterKill) > 1:
		var failed []string
		for _, retry := range afterKill[1:] {
			failed = append(failed, retry.Reason+": "+retry.Message)
		}
		return restoreVerdict{fmt.Errorf("the seeding failed again after the seed attempt the kill ended: %s",
			strings.Join(failed, "; "))}
	case afterKill[0].Reason != seedRetryReasonAt(r.At) || !strings.HasPrefix(afterKill[0].Message, "on "+r.Pod+": ") ||
		!strings.Contains(afterKill[0].Message, cmp.Or(r.cause, s12IMRestartedReason)):
		cause := "the Instance Manager's restart"
		if r.cause != "" {
			cause = "the member's stop"
		}
		return fmt.Errorf("the seed attempt the kill interrupted ended with %s %q, not with a %s on %s failed by "+
			"%s (%q): whether the fault ended it was not observed", afterKill[0].Reason, afterKill[0].Message, step,
			r.Pod, cause, cmp.Or(r.cause, s12IMRestartedReason))
	// The peer's seed backup stays recorded through its seed restore, so a
	// reading normally shows it; the seed restore is cleared once the peer is
	// seeded, which a reading may miss (imRestartSeedingPassReason).
	case r.At == IMRestartAtSeedBackup && after == "":
		return fmt.Errorf("no reading after the kill showed a seed backup for %s under the next seed attempt, so "+
			"whether the retry took a new one instead of seed backup %s is not known", seededPeer(r), before)
	case after == before:
		return restoreVerdict{fmt.Errorf("the %s operation %s the kill ended is the one recorded for %s after the "+
			"next seed attempt started", step, before, seededPeer(r))}
	}
	return nil
}

// scanIMRestartBootstrap records, from the readings in order, the reasons
// after the kill, the attempts with their operations, the retries and the
// operation at the end. It returns what must never happen, the rises of
// attempts without the reason RestoreFailedRetrying, and the retries after
// the kill.
func scanIMRestartBootstrap(rec *s12IMRestartRecord) (events, unexplained []string, afterKill []s12Retry) {
	r := rec.IMRestart
	var rises []attemptRise
	lastOp, opAttempts, attempts := "", int32(0), int32(0)
	seen := map[string]bool{}
	// once adds the event of kind the first time it is seen.
	once := func(kind, event string) {
		if !seen[kind] {
			seen[kind] = true
			events = append(events, event)
		}
	}
	r.Reasons, r.Attempts, r.Retries = []string{}, []s12Attempt{}, []s12Retry{}
	afterKillReading := func(i int) bool { return r.KilledAt != nil && r.restartReading >= 0 && i > r.restartReading }
	for i, p := range rec.Bootstrap {
		boot := meta.FindStatusCondition(p.Conditions, bootstrapReadyCondition)
		readingNeverEvents(p, boot, once)
		if boot != nil && afterKillReading(i) {
			if n := len(r.Reasons); n == 0 || r.Reasons[n-1] != boot.Reason {
				r.Reasons = append(r.Reasons, boot.Reason)
			}
		}
		switch {
		case i > 0 && p.Attempts < attempts:
			events = append(events, fmt.Sprintf("the restored cluster's bootstrap attempts went from %d back to %d",
				attempts, p.Attempts))
		case p.Attempts > attempts && boot != nil && boot.Reason == s12RetryReason:
			retry := s12Retry{At: p.At, From: attempts, To: p.Attempts, Reason: boot.Reason, Message: boot.Message}
			r.Retries = append(r.Retries, retry)
			if afterKillReading(i) {
				afterKill = append(afterKill, retry)
			}
		case p.Attempts > attempts:
			rises = append(rises, attemptRise{index: i, at: p.At, from: attempts, to: p.Attempts})
		}
		attempts = p.Attempts
		if p.OperationID == "" {
			continue
		}
		if lastOp != "" && p.OperationID != lastOp && p.Attempts == opAttempts {
			events = append(events, fmt.Sprintf("the restore was started again without a failed attempt: the "+
				"operation %s became %s under attempt %d", lastOp, p.OperationID, p.Attempts))
		}
		r.Attempts = withOperation(r.Attempts, p.Attempts, p.OperationID)
		lastOp, opAttempts = p.OperationID, p.Attempts
	}
	if lastOp != "" {
		after := opAttempts
		r.OperationIDAfter, r.AttemptsAfter = lastOp, &after
	}
	if len(afterKill) == 0 && len(rises) == 1 && afterKillReading(rises[0].index) {
		if retry := retryFromEvent(r, rises[0]); retry != nil {
			afterKill, rises = append(afterKill, *retry), nil
		}
	}
	for _, x := range rises {
		unexplained = append(unexplained, fmt.Sprintf("from %d to %d", x.from, x.to))
	}
	r.retriesAfterKill = len(afterKill)
	return events, unexplained, afterKill
}

// readingNeverEvents adds, through once, what one reading must never show:
// BootstrapReady True before Complete, the reason RecordedOperationMissing,
// and attempts at or beyond s12MaxAttempts.
func readingNeverEvents(p s12Phase, boot *metav1.Condition, once func(kind, event string)) {
	if early := bootstrapReadyEarly(p, boot); early != "" {
		once("early", early)
	}
	if boot != nil && boot.Reason == s12OperationMissingReason {
		once("missing", fmt.Sprintf("the target answered that it holds no record of the restore "+
			"operation %q (%s): the Instance Manager's operation store on the member's volume lost a record it "+
			"had answered for", p.OperationID, boot.Message))
	}
	if p.Attempts >= s12MaxAttempts {
		once("beyond", fmt.Sprintf("the restored cluster's bootstrap attempts reached %d, beyond the "+
			"Operator's %d attempts", p.Attempts, s12MaxAttempts))
	}
}

// withOperation adds operation under attempt to attempts, in order.
func withOperation(attempts []s12Attempt, attempt int32, operation string) []s12Attempt {
	if n := len(attempts); n == 0 || attempts[n-1].Attempt != attempt {
		attempts = append(attempts, s12Attempt{Attempt: attempt, Operations: []string{}})
	}
	if a := &attempts[len(attempts)-1]; !slices.Contains(a.Operations, operation) {
		a.Operations = append(a.Operations, operation)
	}
	return attempts
}

// attemptRise is a rise of attempts that no reading gave the reason of.
type attemptRise struct {
	index    int
	at       time.Time
	from, to int32
}

// retryFromEvent returns the retry x is, and records it, when the Operator's
// Event of it names the restart (r.cause) and the attempt x starts. The reason
// RestoreFailedRetrying lasts until the next reconcile, which may come at
// once, so a reading may miss it. x must be the only rise after the kill, by
// one from the attempt at the kill, with another operation at the end. The
// attempt number keeps an Event of an earlier retry of the same cluster from
// standing for it: the Operator numbers attempts from 1 in its message
// ("starting attempt 2 of 3" for attempts 1), and they only rise.
func retryFromEvent(r *s12IMRestart, x attemptRise) *s12Retry {
	if r.AttemptsBefore == nil || x.from != *r.AttemptsBefore || x.to != x.from+1 || r.OperationIDAfter == "" ||
		r.OperationIDAfter == r.OperationIDBefore {
		return nil
	}
	starting := fmt.Sprintf("starting attempt %d of ", x.to+1)
	for _, message := range r.RetryEvents {
		if strings.Contains(message, cmp.Or(r.cause, s12IMRestartedReason)) && strings.Contains(message, starting) {
			retry := s12Retry{At: x.at, From: x.from, To: x.to, Reason: s12RetryReason, Message: message,
				FromEvent: true}
			r.Retries = append(r.Retries, retry)
			slices.SortStableFunc(r.Retries, func(a, b s12Retry) int { return a.At.Compare(b.At) })
			return &retry
		}
	}
	return nil
}

// judgeKillRetries judges the retries after the kill of a restore that
// completed: exactly one, from the attempt at the kill, naming the Instance
// Manager's restart, and another operation at the end.
func judgeKillRetries(r *s12IMRestart, afterKill []s12Retry) error {
	switch {
	case len(afterKill) == 0 && r.OperationIDAfter == r.OperationIDBefore:
		return fmt.Errorf("the kill did not interrupt the restore: operation %s completed under "+
			"attempt %d with no failed attempt, so it had ended before the Instance Manager was killed",
			r.OperationIDBefore, *r.AttemptsBefore)
	case len(afterKill) == 0:
		// Another operation without a retry is judged above.
	case len(afterKill) > 1:
		var failed []string
		for _, retry := range afterKill[1:] {
			failed = append(failed, retry.Message)
		}
		return restoreVerdict{fmt.Errorf("the restore failed again after the attempt the kill ended: %s",
			strings.Join(failed, "; "))}
	// The first retry after the kill starts from the attempts at the kill:
	// the reading at the kill is the last one recorded before it, and attempts
	// that change in between either fall, a never-event, or rise without the
	// reason, which blocks (scanIMRestartBootstrap).
	case !strings.Contains(afterKill[0].Message, s12IMRestartedReason):
		return fmt.Errorf("the attempt the kill interrupted ended with %q, not with the Instance Manager's "+
			"restart (%q): whether the restart ended it was not observed", afterKill[0].Message,
			s12IMRestartedReason)
	case r.OperationIDAfter == r.OperationIDBefore:
		return restoreVerdict{fmt.Errorf("the operation %s the kill ended is the one that completed "+
			"after the next attempt started", r.OperationIDBefore)}
	}
	return nil
}
