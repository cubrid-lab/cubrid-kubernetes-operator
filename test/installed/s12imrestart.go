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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

const (
	s12IMRestartLane                = "lab-installed-ha-s12-instance-manager-restart"
	s12IMRestartVariant             = "instance-manager-restart"
	s12IMRestartRecordFile          = "S12/instance-manager-restart/record.json"
	s12IMRestartTimelineFile        = "S12/instance-manager-restart/timeline.jsonl"
	s12IMRestartDataCheckFile       = "S12/instance-manager-restart/data-check.json"
	s12IMRestartSourceDataCheckFile = "S12/instance-manager-restart/source-data-check.json"
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
)

// imRestartPlacementNote says, for the summary, what is killed when.
var imRestartPlacementNote = map[string]string{
	IMRestartAtRestoring: "the Instance Manager of the restore's target member is killed during its restore",
	IMRestartAtPreparing: "the Instance Manager of the restore's target member is killed while its bootstrap is " +
		"Preparing with an operation",
	IMRestartAtSeeding: "the Instance Manager of a member being seeded from the restored member is killed during " +
		"its seed restore",
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
const imKillScript = `found=""; ` +
	`for d in /proc/[0-9]*; do ` +
	`s=$(cat "$d/stat" 2>/dev/null) || continue; read -r -a st <<< "${s##*) }"; [ "${st[1]}" = 1 ] || continue; ` +
	`mapfile -d '' -t argv < "$d/cmdline" 2>/dev/null || continue; ` +
	`[ "${#argv[@]}" = 1 ] && [ "${argv[0]}" = ` + imBinary + ` ] || continue; ` +
	`found="$found ${d#/proc/}"; done; set -- $found; ` +
	`if [ "$#" != 1 ]; then echo "` + imKillRefused + `${found:- none}"; exit 3; fi; ` +
	`kill -KILL "$1" && echo "killed instance-manager $1"`

// s12Attempt is one value of status.bootstrap.attempts the readings showed,
// with the operations they named under it, in order.
type s12Attempt struct {
	Attempt    int32    `json:"attempt"`
	Operations []string `json:"operations"`
}

// s12IMRestart is the Instance Manager's restart on the restore's target
// member, or on the member being seeded, and the operations around it.
type s12IMRestart struct {
	// At is the placement of the kill; variant is the scenario variant it
	// selects.
	At      string `json:"at"`
	variant string
	// PhaseFirstPastPreparing is the bootstrap phase of the first reading
	// that was neither Preparing nor without a phase: Restoring when the
	// kill could be placed during the restore.
	PhaseFirstPastPreparing string `json:"phaseFirstPastPreparing,omitempty"`
	// Pod is the member killed, the restore's target or, at seeding, the
	// member being seeded; PodUID is its UID, and ContainerBefore,
	// RestartsBefore and StartedBefore its database container, all of the
	// reading right before the kill.
	Pod             string     `json:"pod,omitempty"`
	PodUID          string     `json:"podUID,omitempty"`
	ContainerBefore string     `json:"containerIDBefore,omitempty"`
	RestartsBefore  *int32     `json:"restartCountBefore,omitempty"`
	StartedBefore   *time.Time `json:"containerStartedBefore,omitempty"`
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
	// RestartsAtEnd the restart count once the cluster was Ready.
	ContainerAfter string     `json:"containerIDAfter,omitempty"`
	RestartsAfter  *int32     `json:"restartCountAfter,omitempty"`
	StartedAfter   *time.Time `json:"containerStartedAfter,omitempty"`
	BackAt         *time.Time `json:"backAt,omitempty"`
	BackWithin     string     `json:"backWithin,omitempty"`
	RestartsAtEnd  *int32     `json:"restartCountAtEnd,omitempty"`
	// PodReplaced is set when the target member's Pod got another UID: not
	// the fault this variant judges.
	PodReplaced bool `json:"podReplaced,omitempty"`
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
	// seedRetriesAfterKill counts the seed retries after the kill.
	seedRetriesAfterKill int
	// KilledOperation is, at preparing, the target's record of the operation
	// at the kill, read once the cluster was Ready.
	KilledOperation *s12KilledOperation `json:"killedOperation,omitempty"`
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
	case "", IMRestartAtRestoring, IMRestartAtPreparing, IMRestartAtSeeding:
	default:
		return fmt.Errorf("-restore-instance-manager-restart-at must be %s, %s or %s", IMRestartAtRestoring,
			IMRestartAtPreparing, IMRestartAtSeeding)
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
		"attemptsAdded": evidence.Unknown, "retriesAfterKill": evidence.Unknown,
		dataRulesHeldMeasurement: evidence.Unknown, postBackupRowsMeasurement: evidence.Unknown}
	switch at {
	case IMRestartAtPreparing:
		result.Measurements["restoredAtKill"], result.Measurements["stagingLeftAtEnd"] = evidence.Unknown,
			evidence.Unknown
	case IMRestartAtSeeding:
		result.Measurements["seedRetriesAfterKill"], result.Measurements["seedAttemptsAdded"] = evidence.Unknown,
			evidence.Unknown
		result.Measurements["seedRestoreOperationChanged"] = evidence.Unknown
	}
	err := h.imRestartSteps(ctx, &result, &rec)
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

// imRestartNotes are the record's notes of each placement, besides the
// Secret's.
var imRestartNotes = map[string][]string{
	IMRestartAtRestoring: {"the kill is placed after a reading that saw the bootstrap Restoring; the Instance " +
		"Manager's operation may have moved on before the kill took effect",
		"whether the next attempt removed the failed attempt's data or took it over was not read: the Instance " +
			"Manager's logs and operation records were not checked",
		"the Operator's logs, and its Events of the cluster other than RestoreFailedRetrying, were not checked"},
	IMRestartAtPreparing: {"the kill is placed after a reading that saw the bootstrap Preparing with an operation: " +
		"the Operator had read the operation no further than Pending or Downloading; the " +
		"Instance Manager records its operation Restoring before it downloads and verifies the backup " +
		"(runRestore in internal/instancemanager/server.go), so whether the download or restoredb ran at the kill " +
		"was not observed, only that restoredb had not succeeded",
		"the record of the killed operation on the target's volume, and whether the recovery's staging directory " +
			"was left there, were read once the cluster was Ready; the next attempt's own record, the Instance " +
			"Manager's logs and the rest of the volume were not read",
		"the Operator's logs, and its Events of the cluster other than RestoreFailedRetrying, were not checked"},
	IMRestartAtSeeding: {"the kill is placed after a reading that saw the bootstrap SeedingReplicas with a seed " +
		"restore recorded for the member killed; the Instance Manager's operation may have moved on before the " +
		"kill took effect",
		"whether the next seed attempt took a new seed backup or reused the one the failed attempt restored, and " +
			"whether the member's Instance Manager removed the failed attempt's data or took it over, were not " +
			"read: the Instance Manager's logs and operation records were not checked",
		"the Operator's logs, and its Events of the cluster other than RestoreFailedRetrying and " +
			"SeedRestoreFailedRetrying, were not checked"},
}

// imRestartPassReasonAt is the reason of a pass at the placement of r.
func imRestartPassReasonAt(r *s12IMRestart) string {
	switch r.At {
	case IMRestartAtPreparing:
		return imRestartPreparingPassReason(r)
	case IMRestartAtSeeding:
		return imRestartSeedingPassReason(r)
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
	if r.At == IMRestartAtSeeding {
		if r.seedRetriesAfterKill > 0 || r.restartReading < 0 || r.restartReading+1 >= len(rec.Bootstrap) {
			return ""
		}
		p := rec.Bootstrap[r.restartReading+1]
		if p.Phase != string(databasev1alpha1.BootstrapComplete) && !slices.Contains(p.SeededMembers, r.Pod) {
			return ""
		}
		return fmt.Sprintf("the kill did not interrupt a seed restore: the first changed reading after it was %s "+
			"with %s seeded, and no seed retry followed", cmp.Or(p.Phase, noBootstrapPhase),
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
	if err := h.killDuringRestore(ctx, cluster, created, rec, result); err != nil {
		return err
	}
	roles, readyAt, err := h.waitRestored(ctx, cluster, created, &rec.s12Record, result)
	if err != nil {
		return err
	}
	rec.RestoredWithin = readyAt.Sub(created).Round(time.Millisecond).String()
	result.Measurements[restoredWithinMeasurement] = rec.RestoredWithin
	h.readRetryEvents(ctx, cluster, r)
	if err := h.targetRestartsAtEnd(ctx, r, result); err != nil {
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
	if r.At != IMRestartAtPreparing {
		return nil
	}
	// Last, so that what the data showed is judged whatever the record says.
	return h.checkKilledOperation(ctx, cluster, r, result)
}

// waitIMPlacement waits for the reading the kill follows, as
// waitRestoring does: at restoring that is waitRestoring; at preparing a
// reading of Preparing with an operation, and a first phase past Preparing
// blocks the variant; at seeding a reading of SeedingReplicas with a seed
// restore recorded for a member, and Complete before it blocks the variant.
func (h *haRun) waitIMPlacement(ctx context.Context, cluster *databasev1alpha1.CubridCluster, created time.Time,
	rec *s12IMRestartRecord, result *evidence.Scenario) error {
	r := rec.IMRestart
	switch r.At {
	case IMRestartAtPreparing:
		return h.waitPlaced(ctx, cluster, created, &rec.s12Record, &r.PhaseFirstPastPreparing, result,
			"restore did not start", func(c *databasev1alpha1.CubridCluster, phase string) (bool, error) {
				if phase != string(databasev1alpha1.BootstrapPreparing) {
					return false, fmt.Errorf("the kill could not be placed while the restore was Preparing: the "+
						"bootstrap was first seen %s, not Preparing with an operation", phase)
				}
				return c.Status.Bootstrap.OperationID != "", nil
			})
	case IMRestartAtSeeding:
		return h.waitPlaced(ctx, cluster, created, &rec.s12Record, &r.PhaseFirstPastPreparing, result,
			"seeding did not start a seed restore", func(c *databasev1alpha1.CubridCluster, phase string) (bool, error) {
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
	return h.waitRestoring(ctx, cluster, created, &rec.s12Record, &r.PhaseFirstPastPreparing, result)
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
		"with operation %s, read by the Operator no further than Downloading; its container ran again on the same Pod "+
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
	attempts, seedAfter := int32(0), evidence.Unknown
	if r.SeedAttemptsAfter != nil {
		attempts = *r.SeedAttemptsAfter
	}
	if r.SeedRestoreOperationIDAfter != "" {
		seedAfter = r.SeedRestoreOperationIDAfter
	} else if r.SeedAttemptsBefore != nil {
		attempts = *r.SeedAttemptsBefore + 1
	}
	return fmt.Sprintf("the Instance Manager on %s was killed while it was seeded from the cluster restored from "+
		"S11's artifact, with seed restore operation %s (%q), its container ran again on the same Pod %s after the "+
		"kill, and the Operator ended that seed attempt with %s naming the Instance Manager's restart and seeded "+
		"%s again under seeding attempt %d (seed restore operation %s), with one seed retry, while the restore kept "+
		"operation %s with no retry after the kill; the cluster reported Ready within restore_limit only after its "+
		"bootstrap completed, with BootstrapReady never True before; its members and Services held exactly the rows "+
		"acknowledged before the known point and S11's marker, no row written after the backup, and took a normal "+
		"write; the source was unchanged; no credential was in its status and no Secret of the source in its Pods; "+
		"the Instance Manager's and the Operator's logs were not checked", r.Pod, r.SeedRestoreOperationIDBefore,
		r.MessageAtRestart, r.BackWithin, s12SeedRetryReason, r.Pod, attempts, seedAfter, r.OperationIDBefore)
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

// checkKilledOperation reads, at preparing and once the cluster is Ready, the
// target's record of the operation at the kill, and whether the recovery's
// staging directory (restoreStagingRoot/<cluster UID>, reconcileRecovery in
// internal/controller/bootstrap.go) is left there. The record is on the
// member's volume and kept across the restart (FindOrCreate, writeLocked in
// internal/instancemanager/opstore.go); its restored artifact is set only
// once restoredb had succeeded, and stays when the operation fails afterwards
// (Operation.Restored in internal/instancemanager/operation.go;
// reconcileOnStart changes only the state and the reason). So a record that
// holds a restored artifact shows the kill landed after restoredb, not while
// the restore was Preparing, and a record that cannot be read, is another
// operation's, or did not end Failed by the restart leaves that unknown: each
// blocks the variant. A successful restore removes the staging directory,
// and drops the error of that removal (Restore in internal/instancemanager/
// restore.go), so a directory left is recorded and not judged.
func (h *haRun) checkKilledOperation(ctx context.Context, cluster *databasev1alpha1.CubridCluster, r *s12IMRestart,
	result *evidence.Scenario) error {
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
	k.State, k.FailureReason = string(op.State), op.FailureReason
	if op.Restored != nil {
		k.Restored = op.Restored.ManifestURI
	}
	present := strings.TrimSpace(staging) == "present"
	k.StagingPresent = &present
	result.Measurements["restoredAtKill"], result.Measurements["stagingLeftAtEnd"] = op.Restored != nil, present
	switch {
	case op.ID != id:
		return fmt.Errorf("the record read for operation %s on %s is of operation %q", id, r.Pod, op.ID)
	case op.State != instancemanager.OpFailed || op.FailureReason != s12IMRestartedReason:
		return fmt.Errorf("the Instance Manager's record of operation %s on %s is %s (%q), not Failed by its "+
			"restart: what ended the operation is not known", id, r.Pod, op.State, op.FailureReason)
	case op.Restored != nil:
		return fmt.Errorf("the kill did not land while the restore was Preparing: the Instance Manager's record of "+
			"operation %s on %s holds the restored artifact %s, which it records only once restoredb succeeded", id,
			r.Pod, op.Restored.ManifestURI)
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
		Fault: instanceManagerKill{h: h, r: r},
		Limits: faults.Limits{Confirm: min(2*time.Minute, h.cfg.RestoreLimit),
			Outcome: max(time.Until(createdAt.Add(h.cfg.RestoreLimit)), h.cfg.Poll), Cleanup: time.Minute,
			Poll: h.cfg.Poll},
		Before: func(ctx context.Context) error { return h.beforeKill(ctx, cluster, rec, result) },
		Check: func(ctx context.Context) (bool, error) {
			if err := h.readDuringRestart(ctx, cluster, &rec.s12Record, &r.PhaseAfterRestart, result); err != nil {
				seen = err
				return false, err
			}
			done, err := h.memberBack(ctx, r)
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
		return fmt.Errorf("the target member's Pod %s was replaced, not only its container restarted: %s", r.Pod,
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
	status := databaseContainerStatus(pod)
	if status == nil || status.State.Running == nil || status.ContainerID == "" {
		return fmt.Errorf("the database container of %s is not running", pod.Name)
	}
	restarts, started := status.RestartCount, status.State.Running.StartedAt.UTC()
	r.Pod, r.PodUID, r.ContainerBefore, r.RestartsBefore, r.StartedBefore = pod.Name, string(pod.UID),
		status.ContainerID, &restarts, &started
	attempts := boot.Attempts
	r.PhaseAtRestart, r.OperationIDBefore, r.AttemptsBefore = phase, boot.OperationID, &attempts
	if c := meta.FindStatusCondition(cluster.Status.Conditions, bootstrapReadyCondition); c != nil {
		r.MessageAtRestart = c.Message
	}
	if r.At == IMRestartAtSeeding {
		status := firstDatabaseStatus(cluster)
		seedAttempts := status.BootstrapAttempts
		r.SeedRestoreOperationIDBefore, r.SeedAttemptsBefore = status.Seeding.RestoreOperationID, &seedAttempts
	}
	r.restartReading = len(rec.Bootstrap) - 1
	return nil
}

// killedMember is the member whose Instance Manager the placement at kills,
// from a reading of cluster in phase right before the kill: the restore's
// target, in Restoring or Preparing with an operation, or the member being
// seeded, in SeedingReplicas with a seed restore recorded for it. That member
// is never the restore's target, which is not seeded over (reconcileSeeding
// in internal/controller/habootstrap.go). Any other reading, or a member that
// is not one of the cluster's, is an error that blocks the variant.
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

// instanceManagerKill kills the Instance Manager of the restore's target
// member (imKillScript). A kubectl exec carries no precondition, so the Pod's
// UID and its container's ID and restart count are read again right before,
// and the kill is not issued unless they are those beforeKill recorded; the
// container can still be replaced between that read and the exec. The kill
// is permanent: the kubelet starts the container again on its own.
type instanceManagerKill struct {
	h *haRun
	r *s12IMRestart
}

func (f instanceManagerKill) Name() string {
	if f.r.At == IMRestartAtSeeding {
		return "kill the Instance Manager of the member being seeded with SIGKILL"
	}
	return "kill the Instance Manager of the restore's target member with SIGKILL"
}

func (f instanceManagerKill) Inject(ctx context.Context) error {
	pod := &corev1.Pod{}
	if err := f.h.Client.Get(ctx, client.ObjectKey{Namespace: f.h.cfg.Namespace, Name: f.r.Pod}, pod); err != nil {
		return fmt.Errorf("read %s again before the kill: %w", f.r.Pod, err)
	}
	status := databaseContainerStatus(pod)
	if string(pod.UID) != f.r.PodUID || status == nil || status.ContainerID != f.r.ContainerBefore ||
		status.RestartCount != *f.r.RestartsBefore {
		return fmt.Errorf("the Pod %s or its database container is not the one read right before the kill", f.r.Pod)
	}
	out, err := f.h.member(ctx, f.r.Pod, "bash", "-c", imKillScript)
	answer := strings.TrimSpace(out)
	if err != nil {
		// The container may end before the command answers; Confirm decides.
		answer = strings.TrimSpace(answer + " " + err.Error())
	}
	f.r.KillAnswer = answer
	if strings.Contains(answer, imKillRefused) {
		return fmt.Errorf("the Instance Manager of %s could not be told apart, nothing was killed: %s", f.r.Pod, answer)
	}
	return nil
}

// Confirm reports the kill once the Pod's database container is seen ended
// or restarted: a status that names another container or a higher restart
// count. A Pod with another UID was replaced, which is not this fault.
func (f instanceManagerKill) Confirm(ctx context.Context) (bool, error) {
	pod := &corev1.Pod{}
	if err := f.h.Client.Get(ctx, client.ObjectKey{Namespace: f.h.cfg.Namespace, Name: f.r.Pod}, pod); err != nil {
		return false, err
	}
	if string(pod.UID) != f.r.PodUID {
		f.r.PodReplaced = true
		return false, fmt.Errorf("the Pod %s was replaced", f.r.Pod)
	}
	status := databaseContainerStatus(pod)
	if status == nil {
		return false, nil
	}
	ended := status.State.Terminated != nil && status.ContainerID == f.r.ContainerBefore
	if !ended && status.ContainerID == f.r.ContainerBefore && status.RestartCount == *f.r.RestartsBefore {
		return false, nil
	}
	at := time.Now().UTC()
	f.r.EndedAt = &at
	for _, t := range []*corev1.ContainerStateTerminated{status.State.Terminated,
		status.LastTerminationState.Terminated} {
		if t != nil && (t.ContainerID == "" || t.ContainerID == f.r.ContainerBefore) {
			code, finished := t.ExitCode, t.FinishedAt.UTC()
			f.r.ExitCode, f.r.ExitReason, f.r.FinishedAt = &code, t.Reason, &finished
			break
		}
	}
	// The shell exits with the killed manager's status, 128 + SIGKILL. Any
	// other end, or one the kubelet reports as OOMKilled, is not the kill's.
	switch {
	case f.r.ExitCode == nil:
		f.r.EndedOtherwise = "its exit code was not reported"
	case *f.r.ExitCode != sigkillExitCode || f.r.ExitReason == oomKilledReason:
		f.r.EndedOtherwise = fmt.Sprintf("exit code %d, reason %q", *f.r.ExitCode, f.r.ExitReason)
	}
	return true, nil
}

func (f instanceManagerKill) Remove(context.Context) error { return nil }

// memberBack reports whether the target member's database container runs
// again: another container than the killed one, running, with a higher
// restart count, on the same Pod. A Pod with another UID is an error. A read
// that fails is remembered, and the successful ones are counted.
func (h *haRun) memberBack(ctx context.Context, r *s12IMRestart) (bool, error) {
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
		case event.Reason == s12SeedRetryReason && r.At == IMRestartAtSeeding:
			r.SeedRetryEvents = append(r.SeedRetryEvents, event.Message)
		}
	}
}

// targetRestartsAtEnd reads the target member once the cluster is Ready and
// records its restart count. A replaced Pod blocks the variant; more than
// one restart after one kill fails it: the container did not stay up.
func (h *haRun) targetRestartsAtEnd(ctx context.Context, r *s12IMRestart, result *evidence.Scenario) error {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: r.Pod}, pod); err != nil {
		return fmt.Errorf("read the target member %s at the end: %w", r.Pod, err)
	}
	if string(pod.UID) != r.PodUID {
		r.PodReplaced = true
		return fmt.Errorf("the target member's Pod %s was replaced after its container restarted", r.Pod)
	}
	status := databaseContainerStatus(pod)
	if status == nil {
		return fmt.Errorf("the target member %s has no database container status at the end", r.Pod)
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
		result.Measurements["retriesAfterKill"] = len(afterKill)
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
	case r.At == IMRestartAtSeeding:
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
		result.Measurements["seedRetriesAfterKill"] = len(seedAfterKill)
	}
	if r.SeedAttemptsBefore != nil && r.SeedAttemptsAfter != nil {
		result.Measurements["seedAttemptsAdded"] = *r.SeedAttemptsAfter - *r.SeedAttemptsBefore
	}
	if r.SeedRestoreOperationIDBefore != "" && r.SeedRestoreOperationIDAfter != "" {
		result.Measurements["seedRestoreOperationChanged"] = r.SeedRestoreOperationIDBefore != r.SeedRestoreOperationIDAfter
	}
	if r.KilledAt != nil && len(afterKill) > 0 {
		events = append(events, fmt.Sprintf("the restore was retried after the kill, after it had completed and "+
			"seeding had begun: %s", afterKill[0].Message))
	}
	// What must never happen fails the variant through the never-events.
	result.NeverEvents = append(result.NeverEvents, events...)
	var errs []error
	if len(unexplained) > 0 {
		errs = append(errs, fmt.Errorf("the restored cluster's seeding attempts rose %s, and no reading showed the "+
			"reason %s or %s: the cause is unknown", strings.Join(unexplained, ", "), s12SeedRetryReason,
			s12SeedBackupRetryReason))
	}
	if r.KilledAt != nil && r.SeedRestoreOperationIDBefore != "" && rec.RestoredWithin != "" {
		errs = append(errs, judgeSeedKillRetries(r, seedAfterKill))
	}
	return errors.Join(errs...)
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
		if p.SeedMember == r.Pod && after(i) && r.SeedAttemptsBefore != nil &&
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
		if strings.HasPrefix(message, "on "+r.Pod+": ") && strings.Contains(message, s12IMRestartedReason) &&
			strings.Contains(message, starting) {
			retry := s12Retry{At: x.at, From: x.from, To: x.to, Reason: s12SeedRetryReason, Message: message,
				FromEvent: true}
			r.SeedRetries = append(r.SeedRetries, retry)
			slices.SortStableFunc(r.SeedRetries, func(a, b s12Retry) int { return a.At.Compare(b.At) })
			return &retry
		}
	}
	return nil
}

// judgeSeedKillRetries judges the seed retries after the kill of a cluster
// that completed: exactly one, a failed seed restore on the killed member
// naming the Instance Manager's restart, and, when a reading showed the
// member's seed restore under the next attempt, another operation than the
// one at the kill. The Instance Manager fails the operation at its start
// (reconcileOnStart), the Operator reads it Failed and retries the seed step
// (seedMember), and the next attempt's key creates a new operation
// (bootstrapKey, FindOrCreate).
func judgeSeedKillRetries(r *s12IMRestart, afterKill []s12Retry) error {
	switch {
	case len(afterKill) == 0:
		return fmt.Errorf("the kill did not interrupt the seed restore: %s was seeded with no failed seed attempt, "+
			"so operation %s had ended before its Instance Manager was killed", r.Pod, r.SeedRestoreOperationIDBefore)
	case len(afterKill) > 1:
		var failed []string
		for _, retry := range afterKill[1:] {
			failed = append(failed, retry.Reason+": "+retry.Message)
		}
		return restoreVerdict{fmt.Errorf("the seeding failed again after the seed attempt the kill ended: %s",
			strings.Join(failed, "; "))}
	case afterKill[0].Reason != s12SeedRetryReason || !strings.HasPrefix(afterKill[0].Message, "on "+r.Pod+": ") ||
		!strings.Contains(afterKill[0].Message, s12IMRestartedReason):
		return fmt.Errorf("the seed attempt the kill interrupted ended with %s %q, not with a seed restore on %s "+
			"failed by the Instance Manager's restart (%q): whether the restart ended it was not observed",
			afterKill[0].Reason, afterKill[0].Message, r.Pod, s12IMRestartedReason)
	case r.SeedRestoreOperationIDAfter == r.SeedRestoreOperationIDBefore:
		return restoreVerdict{fmt.Errorf("the seed restore operation %s the kill ended is the one recorded for %s "+
			"after the next seed attempt started", r.SeedRestoreOperationIDBefore, r.Pod)}
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
	if boot != nil && boot.Status == metav1.ConditionTrue && p.Phase != string(databasev1alpha1.BootstrapComplete) {
		once("early", fmt.Sprintf("the restored cluster reported BootstrapReady True (%s) while its "+
			"bootstrap phase was %q", boot.Reason, cmp.Or(p.Phase, noBootstrapPhase)))
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
// Event of it names the restart and the attempt x starts. The reason
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
		if strings.Contains(message, s12IMRestartedReason) && strings.Contains(message, starting) {
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
