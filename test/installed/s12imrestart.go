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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
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
	// imKillRefused starts the answer of imKillScript when it did not find
	// exactly one Instance Manager process, and killed nothing.
	imKillRefused = "instance-manager processes:"
)

// imKillScript kills the Instance Manager of a member's database container
// with SIGKILL, as a crash would end it. The image's entrypoint shell stays
// PID 1 and runs the manager as its child, /usr/local/bin/instance-manager
// with no argument; its other uses of that binary (wait-volume-free,
// shutdown) carry one (build/instance-manager/entrypoint.sh). The script
// kills only the one process whose parent is PID 1 and whose command line is
// that binary alone, and kills nothing when it finds none or more than one.
// It runs as the container's user, the manager's own. Once the manager is
// gone the shell exits with its status, and the container ends.
const imKillScript = `found=""; ` +
	`for d in /proc/[0-9]*; do ` +
	`s=$(cat "$d/stat" 2>/dev/null) || continue; read -r -a st <<< "${s##*) }"; [ "${st[1]}" = 1 ] || continue; ` +
	`mapfile -d '' -t argv < "$d/cmdline" 2>/dev/null || continue; ` +
	`[ "${#argv[@]}" = 1 ] && [ "${argv[0]##*/}" = instance-manager ] || continue; ` +
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
// member and the restore operations around it.
type s12IMRestart struct {
	// PhaseFirstPastPreparing is the bootstrap phase of the first reading
	// that was neither Preparing nor without a phase: Restoring when the
	// kill could be placed.
	PhaseFirstPastPreparing string `json:"phaseFirstPastPreparing,omitempty"`
	// Pod is the target member, PodUID its UID, and ContainerBefore,
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
	FinishedAt *time.Time `json:"containerFinishedAt,omitempty"`
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
}

// s12IMRestartRecord is the evidence of S12 instance-manager-restart besides
// the data checks and the timeline.
type s12IMRestartRecord struct {
	s12Record
	IMRestart *s12IMRestart `json:"instanceManagerRestart,omitempty"`
}

// validateS12InstanceManagerRestart checks the inputs of S12
// instance-manager-restart, when it is selected.
func (c HAConfig) validateS12InstanceManagerRestart() error {
	if !c.RestoreInstanceManagerRestart {
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
	defer h.inScenario(s12ID+"/"+s12IMRestartVariant, &result)()
	result = evidence.Scenario{ID: s12ID, Variant: s12IMRestartVariant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s12IMRestartRecord{s12Record: s12Record{Cluster: s12IMRestartClusterName,
		SeedPrefix: h.rec.Seed.Prefix + "/imrestart-seed", SecretRef: s12IMRestartSecretName, Bootstrap: []s12Phase{},
		Members: []imageIdentity{}, SecretsReferenced: []string{}, Credentials: &s12Credentials{FoundIn: []string{}},
		Notes: []string{"the cluster restored while its target's Instance Manager restarts uses a copy of the seed " +
			"Secret of its own, " + s12IMRestartSecretName + "; the source uses " + seedSecretName,
			"the kill is placed after a reading that saw the bootstrap Restoring; the Instance Manager's operation " +
				"may have moved on before the kill took effect",
			"whether the next attempt removed the failed attempt's data or took it over was not read: the Instance " +
				"Manager's logs and operation records were not checked",
			"the Operator's logs and Events of the cluster were not checked"},
		Residue: "the cluster's seed backups under its seed prefix are not removed by this run; the cluster goes " +
			"with the namespace"},
		IMRestart: &s12IMRestart{restartReading: -1, Reasons: []string{}, Attempts: []s12Attempt{},
			Retries: []s12Retry{}}}
	defer func() {
		// A pass without its evidence is a failure; a blocked or failed S12
		// instance-manager-restart keeps its result, with the evidence that
		// could not be written.
		if err := h.writeJSON(s12IMRestartRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S12 instance-manager-restart evidence: "+
				err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S12/"+s12IMRestartVariant+"/")
		})
	}()
	result.Limits = map[string]string{restoreLimitName: h.cfg.RestoreLimit.String()}
	result.Measurements = map[string]any{restoredWithinMeasurement: evidence.Unknown, "memberBackWithin": evidence.Unknown,
		"containerRestarts": evidence.Unknown, "operationIDChanged": evidence.Unknown,
		"attemptsAdded": evidence.Unknown, "retriesAfterKill": evidence.Unknown,
		dataRulesHeldMeasurement: evidence.Unknown, postBackupRowsMeasurement: evidence.Unknown}
	err := h.imRestartSteps(ctx, &result, &rec)
	// What the bootstrap showed is judged whichever step ended the variant.
	if judged := judgeIMRestartBootstrap(&rec, &result); err == nil || errors.As(judged, new(restoreVerdict)) {
		err = errors.Join(judged, err)
	}
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
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
		result.Result, result.Reason = evidence.Blocked, "the run ended before S12 instance-manager-restart was "+
			"judged: "+err.Error()
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, err.Error()
	case never != "":
		result.Result, result.Reason = evidence.Fail, never
	default:
		result.Result, result.Reason = evidence.Pass, imRestartPassReason(rec.IMRestart)
	}
	return result
}

// imRestartSteps runs the steps of S12 instance-manager-restart in order and
// returns the first error.
func (h *haRun) imRestartSteps(ctx context.Context, result *evidence.Scenario, rec *s12IMRestartRecord) error {
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S12 instance-manager-restart could not tell whether a credential is in the "+
			"cluster's status", err)
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
	r := rec.IMRestart
	if err := h.waitRestoring(ctx, cluster, created, &rec.s12Record, &r.PhaseFirstPastPreparing,
		result); err != nil {
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
	if err := h.targetRestartsAtEnd(ctx, r, result); err != nil {
		return err
	}
	if err := h.checkRestoredSecrets(ctx, &rec.s12Record, result); err != nil {
		return err
	}
	if err := h.checkHANodes(ctx, roles, &rec.s12Record, result); err != nil {
		return err
	}
	if err := h.checkRestoredData(ctx, expected, roles, &rec.s12Record, result, s12IMRestartDataCheckFile); err != nil {
		return err
	}
	if err := h.normalWriteS12(ctx, roles, &rec.s12Record, s12IMRestartMarker); err != nil {
		return err
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	if err := h.checkSourceS12(ctx, rec.Source, s12IMRestartSourceDataCheckFile); err != nil {
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

// killDuringRestore kills the Instance Manager of the restore's target
// member, once a reading right before shows the bootstrap Restoring with an
// operation and the member's Pod and container are recorded, and waits for
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
	if err := h.writeTimeline(s12IMRestartTimelineFile, outcome.Timeline); err != nil {
		return err
	}
	switch {
	case seen != nil:
		return seen
	case r.PodReplaced:
		return fmt.Errorf("the target member's Pod %s was replaced, not only its container restarted: %s", r.Pod,
			outcome.Reason)
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

// beforeKill requires a reading of the restored cluster in bootstrap phase
// Restoring with an operation, which it records as the state at the kill,
// and the target member it names: a Pod of this cluster's StatefulSet, which
// the cluster owns, with its database container running. The Pod's UID and
// the container's ID and restart count are recorded; the kill is issued only
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
	if phase != string(databasev1alpha1.BootstrapRestoring) || boot.OperationID == "" {
		return fmt.Errorf("the restart could not be placed during the restore: right before it the bootstrap "+
			"phase was %q", cmp.Or(phase, noBootstrapPhase))
	}
	if !slices.Contains(clusterMembers(cluster.Name), boot.TargetMember) {
		return fmt.Errorf("the restore's target member %q is not a member of %s", boot.TargetMember, cluster.Name)
	}
	sts := &appsv1.StatefulSet{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), sts); err != nil {
		return fmt.Errorf("read the StatefulSet of %s: %w", cluster.Name, err)
	}
	if !controllerOwner(sts, "CubridCluster", cluster.UID) {
		return fmt.Errorf("the StatefulSet %s does not belong to the restored cluster", sts.Name)
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: boot.TargetMember},
		pod); err != nil {
		return fmt.Errorf("read the target member %s: %w", boot.TargetMember, err)
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
	r.restartReading = len(rec.Bootstrap) - 1
	return nil
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
			f.r.ExitCode, f.r.FinishedAt = &code, &finished
			break
		}
	}
	return true, nil
}

func (f instanceManagerKill) Remove(context.Context) error { return nil }

// memberBack reports whether the target member's database container runs
// again: another container than the killed one, running, with a higher
// restart count, on the same Pod. A Pod with another UID is an error.
func (h *haRun) memberBack(ctx context.Context, r *s12IMRestart) (bool, error) {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: r.Pod}, pod); err != nil {
		return false, nil
	}
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
		return restoreVerdict{fmt.Errorf("the database container of %s restarted %d times after one kill of its "+
			"Instance Manager, not once", r.Pod, added)}
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
	// The expectation of the kill is judged on a restore that completed.
	if r.KilledAt != nil && r.OperationIDBefore != "" && rec.RestoredWithin != "" {
		errs = append(errs, judgeKillRetries(r, afterKill))
	}
	return errors.Join(errs...)
}

// scanIMRestartBootstrap records, from the readings in order, the reasons
// after the kill, the attempts with their operations, the retries and the
// operation at the end. It returns what must never happen, the rises of
// attempts without the reason RestoreFailedRetrying, and the retries after
// the kill.
func scanIMRestartBootstrap(rec *s12IMRestartRecord) (events, unexplained []string, afterKill []s12Retry) {
	r := rec.IMRestart
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
		if boot != nil && boot.Status == metav1.ConditionTrue && p.Phase != string(databasev1alpha1.BootstrapComplete) {
			once("early", fmt.Sprintf("the restored cluster reported BootstrapReady True (%s) while its "+
				"bootstrap phase was %q", boot.Reason, cmp.Or(p.Phase, noBootstrapPhase)))
		}
		if boot != nil && afterKillReading(i) {
			if n := len(r.Reasons); n == 0 || r.Reasons[n-1] != boot.Reason {
				r.Reasons = append(r.Reasons, boot.Reason)
			}
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
			unexplained = append(unexplained, fmt.Sprintf("from %d to %d", attempts, p.Attempts))
		}
		attempts = p.Attempts
		if p.OperationID == "" {
			continue
		}
		if lastOp != "" && p.OperationID != lastOp && p.Attempts == opAttempts {
			events = append(events, fmt.Sprintf("the restore was started again without a failed attempt: the "+
				"operation %s became %s under attempt %d", lastOp, p.OperationID, p.Attempts))
		}
		if n := len(r.Attempts); n == 0 || r.Attempts[n-1].Attempt != p.Attempts {
			r.Attempts = append(r.Attempts, s12Attempt{Attempt: p.Attempts, Operations: []string{}})
		}
		if a := &r.Attempts[len(r.Attempts)-1]; !slices.Contains(a.Operations, p.OperationID) {
			a.Operations = append(a.Operations, p.OperationID)
		}
		lastOp, opAttempts = p.OperationID, p.Attempts
	}
	if lastOp != "" {
		after := opAttempts
		r.OperationIDAfter, r.AttemptsAfter = lastOp, &after
	}
	return events, unexplained, afterKill
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
	case afterKill[0].From != *r.AttemptsBefore || !strings.Contains(afterKill[0].Message, s12IMRestartedReason):
		return fmt.Errorf("the attempt the kill interrupted ended from attempt %d with %q, not with the "+
			"Instance Manager's restart (%q) from attempt %d: whether the restart ended it was not observed",
			afterKill[0].From, afterKill[0].Message, s12IMRestartedReason, *r.AttemptsBefore)
	case r.OperationIDAfter == r.OperationIDBefore:
		return restoreVerdict{fmt.Errorf("the operation %s the kill ended is the one that completed "+
			"after the next attempt started", r.OperationIDBefore)}
	}
	return nil
}
