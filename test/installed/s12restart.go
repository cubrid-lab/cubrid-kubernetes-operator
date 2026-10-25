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

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

const (
	s12RestartLane                = "lab-installed-ha-s12-operator-restart"
	s12RestartVariant             = "operator-restart"
	s12RestartRecordFile          = "S12/operator-restart/record.json"
	s12RestartTimelineFile        = "S12/operator-restart/timeline.jsonl"
	s12RestartDataCheckFile       = "S12/operator-restart/data-check.json"
	s12RestartSourceDataCheckFile = "S12/operator-restart/source-data-check.json"
	// s12RestartClusterName is the cluster restored while the manager
	// restarts, in the run namespace, and s12RestartSecretName its own copy
	// of the seed Secret.
	s12RestartClusterName = "ha-restart"
	s12RestartSecretName  = "object-storage-restart"
	// s12RestartMarker names the marker row written through that cluster; its
	// value is the run ID.
	s12RestartMarker = "s12or"
	// s12RetryReason is the BootstrapReady reason of a failed restore attempt
	// that the Operator starts again under the next attempt's key
	// (restoreAttemptFailed in internal/controller/bootstrap.go).
	s12RetryReason = "RestoreFailedRetrying"
	// managerContainer is the manager's container in its Pod.
	managerContainer = "manager"
	// dataRulesHeldMeasurement is the measurement of how long the data rules
	// took to hold.
	dataRulesHeldMeasurement = "dataRulesHeldAfterSeconds"
)

// s12Retry is a restore attempt the status showed ending, with the reason
// the Operator gave.
type s12Retry struct {
	At      time.Time `json:"at"`
	From    int32     `json:"fromAttempts"`
	To      int32     `json:"toAttempts"`
	Reason  string    `json:"reason"`
	Message string    `json:"message,omitempty"`
}

// s12Restart is the manager's restart during the restore and the restore
// operation around it.
type s12Restart struct {
	ManagerBefore imageIdentity  `json:"managerBefore"`
	ManagerAfter  *imageIdentity `json:"managerAfter,omitempty"`
	// PhaseFirstPastPreparing is the bootstrap phase of the first reading
	// that was neither Preparing nor without a phase: Restoring when the
	// restart could be placed.
	PhaseFirstPastPreparing string `json:"phaseFirstPastPreparing,omitempty"`
	// PhaseAtRestart, OperationIDBefore and AttemptsBefore are of the reading
	// right before the manager Pod was deleted, PhaseAfterRestart of the
	// first reading after it.
	PhaseAtRestart    string `json:"phaseAtRestart,omitempty"`
	OperationIDBefore string `json:"operationIDBefore,omitempty"`
	AttemptsBefore    *int32 `json:"attemptsBefore,omitempty"`
	PhaseAfterRestart string `json:"phaseAfterRestart,omitempty"`
	// PhaseAtTakeover and OperationIDAtTakeover are the bootstrap of the last
	// reading at or before the replacement acquired the Lease.
	PhaseAtTakeover       string `json:"phaseAtTakeover,omitempty"`
	OperationIDAtTakeover string `json:"operationIDAtTakeover,omitempty"`
	// RestartedAt is when the deletion was issued, GoneAt when the old Pod
	// was confirmed gone, BackAt the reading that saw the replacement running
	// and holding the Lease, all by the run's clock. ReplacementStartedAt and
	// LeaseAcquiredAt are the times the cluster wrote.
	RestartedAt          *time.Time `json:"restartedAt,omitempty"`
	GoneAt               *time.Time `json:"goneAt,omitempty"`
	ReplacementStartedAt *time.Time `json:"replacementStartedAt,omitempty"`
	LeaseHolder          string     `json:"leaseHolder,omitempty"`
	LeaseAcquiredAt      *time.Time `json:"leaseAcquiredAt,omitempty"`
	BackAt               *time.Time `json:"backAt,omitempty"`
	BackWithin           string     `json:"backWithin,omitempty"`
	// ImageChanged is set when the replacement runs another image: the
	// environment changed, which blocks the variant.
	ImageChanged bool `json:"imageChanged,omitempty"`
	// OperationIDAfter and AttemptsAfter are of the last reading that named
	// an operation; Retries are the attempts the status showed ending.
	OperationIDAfter string     `json:"operationIDAfter,omitempty"`
	AttemptsAfter    *int32     `json:"attemptsAfter,omitempty"`
	Retries          []s12Retry `json:"retries"`
}

// s12RestartRecord is the evidence of S12 operator-restart besides the data
// checks and the timeline.
type s12RestartRecord struct {
	s12Record
	Restart *s12Restart `json:"restart,omitempty"`
}

// validateS12OperatorRestart checks the inputs of S12 operator-restart, when
// it is selected.
func (c HAConfig) validateS12OperatorRestart() error {
	if !c.RestoreOperatorRestart {
		return nil
	}
	if !c.Restore {
		return errors.New("-restore-operator-restart needs -restore: S12 operator-restart follows S12 exact")
	}
	if c.RestoreTampered || c.RestoreWrongTarget {
		return errors.New("-restore-operator-restart, -restore-tampered and -restore-wrong-target select " +
			"different lanes; a run takes one of them")
	}
	if c.ResyncLimit <= 0 || len(validation.IsDNS1123Subdomain(c.LeaderLease)) != 0 {
		return errors.New("S12 operator-restart needs a positive resync limit and the leader Lease")
	}
	return nil
}

// runS12OperatorRestart runs S12 operator-restart after S12 exact passed: a
// new cluster is restored from S11's artifact, and once a reading shows its
// bootstrap Restoring, the manager's Pod is deleted without a grace period,
// as S06 restart deletes it. The replacement must run the same image and hold
// the leader Lease within ResyncLimit. The cluster must then pass as S12
// exact's does, within RestoreLimit of its creation, and the restore must not
// be started again: the status must keep its operation, unless it shows a
// failed attempt with its reason. BootstrapReady True before Complete must
// never happen. A restart that cannot be placed during the restore, a run
// that ends, or a check that cannot be made, before S12 operator-restart is
// judged blocks it; what was seen to break stays a failure.
func (h *haRun) runS12OperatorRestart(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s12ID+"/"+s12RestartVariant, &result)()
	result = evidence.Scenario{ID: s12ID, Variant: s12RestartVariant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s12RestartRecord{s12Record: s12Record{Cluster: s12RestartClusterName,
		SeedPrefix: h.rec.Seed.Prefix + "/restart-seed", SecretRef: s12RestartSecretName, Bootstrap: []s12Phase{},
		Members: []imageIdentity{}, SecretsReferenced: []string{}, Credentials: &s12Credentials{FoundIn: []string{}},
		Notes: []string{"the cluster restored while the manager restarts uses a copy of the seed Secret of its own, " +
			s12RestartSecretName + "; the source uses " + seedSecretName,
			"the restart is placed after a reading that saw the bootstrap Restoring; the Instance Manager's " +
				"operation may have moved on before the deletion took effect",
			"the Operator's logs and Events of the cluster were not checked"},
		Residue: "the cluster's seed backups under its seed prefix are not removed by this run; the cluster goes " +
			"with the namespace"},
		Restart: &s12Restart{Retries: []s12Retry{}}}
	defer func() {
		// A pass without its evidence is a failure; a blocked or failed S12
		// operator-restart keeps its result, with the evidence that could not
		// be written.
		if err := h.writeJSON(s12RestartRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S12 operator-restart evidence: "+err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S12/"+s12RestartVariant+"/")
		})
	}()
	result.Limits = map[string]string{restoreLimitName: h.cfg.RestoreLimit.String(),
		"operator_resync_limit": h.cfg.ResyncLimit.String()}
	result.Measurements = map[string]any{"restoredWithin": evidence.Unknown, "operatorBackWithin": evidence.Unknown,
		"operationIDUnchanged": evidence.Unknown, "attemptsAdded": evidence.Unknown, "phaseAtTakeover": evidence.Unknown,
		dataRulesHeldMeasurement: evidence.Unknown, "postBackupRowsPresent": evidence.Unknown}
	err := h.restartSteps(ctx, &result, &rec)
	// What the bootstrap showed is judged whichever step ended the variant.
	if judged := judgeRestartBootstrap(&rec, &result); err == nil {
		err = judged
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
		result.Result, result.Reason = evidence.Blocked, "the run ended before S12 operator-restart was judged: "+
			err.Error()
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, err.Error()
	case never != "":
		result.Result, result.Reason = evidence.Fail, never
	default:
		result.Result, result.Reason = evidence.Pass, restartPassReason(rec.Restart)
	}
	return result
}

// restartSteps runs the steps of S12 operator-restart in order and returns
// the first error.
func (h *haRun) restartSteps(ctx context.Context, result *evidence.Scenario, rec *s12RestartRecord) error {
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S12 operator-restart could not tell whether a credential is in the cluster's status", err)
	}
	s11 := h.s11Rec
	if s11 == nil || s11.KnownPoint == nil || s11.Artifact == nil {
		return errors.New("S11 recorded no known point or artifact to restore")
	}
	expected := h.s12Expected
	if expected == "" {
		return errors.New("S12 exact recorded no history at S11's known point")
	}
	if len(h.rec.OperatorAfter.Pods) != 1 {
		return errors.New("S12 operator-restart needs exactly one manager Pod")
	}
	rec.Restart.ManagerBefore = h.rec.OperatorAfter.Pods[0]
	rec.KnownPoint, rec.ArtifactURI, rec.ManifestDigest = s11.KnownPoint, s11.Artifact.URI, s11.Artifact.ManifestDigest
	cluster, err := h.createRestored(ctx, s12RestartClusterName, s12RestartSecretName, rec.SeedPrefix, rec.ArtifactURI)
	if err != nil {
		return err
	}
	created := time.Now()
	createdAt := created.UTC()
	rec.ClusterUID, rec.CreatedAt = string(cluster.UID), &createdAt
	if err := h.waitRestoring(ctx, cluster, created, rec, result); err != nil {
		return err
	}
	back, err := h.restartDuringRestore(ctx, cluster, rec, result)
	// A manager back too late stays a failure whatever a later step finds.
	var late error
	if back > h.cfg.ResyncLimit {
		late = restoreVerdict{fmt.Errorf("the replacement manager was back %s after its Pod was deleted, over "+
			"operator_resync_limit %s", back.Round(time.Millisecond), h.cfg.ResyncLimit)}
	}
	if err != nil {
		return errors.Join(late, err)
	}
	return errors.Join(late, h.afterRestart(ctx, cluster, created, expected, rec, result))
}

// afterRestart runs the steps of S12 operator-restart after the manager is
// back, as S12 exact's after its restored cluster was created, and returns
// the first error.
func (h *haRun) afterRestart(ctx context.Context, cluster *databasev1alpha1.CubridCluster, created time.Time,
	expected string, rec *s12RestartRecord, result *evidence.Scenario) error {
	roles, readyAt, err := h.waitRestored(ctx, cluster, created, &rec.s12Record, result)
	if err != nil {
		return err
	}
	rec.RestoredWithin = readyAt.Sub(created).Round(time.Millisecond).String()
	result.Measurements["restoredWithin"] = rec.RestoredWithin
	if err := h.checkRestoredSecrets(ctx, &rec.s12Record, result); err != nil {
		return err
	}
	if err := h.checkHANodes(ctx, roles, &rec.s12Record, result); err != nil {
		return err
	}
	if err := h.checkRestoredData(ctx, expected, roles, &rec.s12Record, result, s12RestartDataCheckFile); err != nil {
		return err
	}
	if err := h.normalWriteS12(ctx, roles, &rec.s12Record, s12RestartMarker); err != nil {
		return err
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	if err := h.checkSourceS12(ctx, rec.Source, s12RestartSourceDataCheckFile); err != nil {
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
	return nil
}

// restartPassReason is the reason of a pass, from what the run recorded.
func restartPassReason(r *s12Restart) string {
	attempts := int32(0)
	if r.AttemptsAfter != nil {
		attempts = *r.AttemptsAfter
	}
	return fmt.Sprintf("the manager Pod was deleted while the cluster restored from S11's artifact was %s with "+
		"operation %s, and the replacement ran the same image and held the leader Lease %s later, within "+
		"operator_resync_limit; the bootstrap was %s at the takeover; the cluster reported Ready within "+
		"restore_limit only after its bootstrap completed, with BootstrapReady never True before, and kept "+
		"operation %s under attempt %d with %d retries; its members and Services held exactly the rows "+
		"acknowledged before the known point and S11's marker, no row written after the backup, and took a "+
		"normal write; the source was unchanged; no credential was in its status and no Secret of the source in "+
		"its Pods; the Operator's logs were not checked", r.PhaseAtRestart, r.OperationIDBefore, r.BackWithin,
		r.PhaseAtTakeover, r.OperationIDAfter, attempts, len(r.Retries))
}

// waitRestoring reads the restored cluster until a reading shows its
// bootstrap Restoring with an operation, until RestoreLimit has passed since
// createdAt. A reading that shows it past Restoring first means the restart
// cannot be placed during the restore, which blocks the variant. Each reading
// is recorded and checked as S12 exact's.
func (h *haRun) waitRestoring(ctx context.Context, created *databasev1alpha1.CubridCluster, createdAt time.Time,
	rec *s12RestartRecord, result *evidence.Scenario) error {
	wctx, cancel := context.WithDeadline(ctx, createdAt.Add(h.cfg.RestoreLimit))
	defer cancel()
	// readErr is the last reading's error, nil once a reading succeeded.
	var readErr error
	err := pollUntil(wctx, h.cfg.Poll, func() (bool, error) {
		cluster, err := h.readRestarted(wctx, created)
		if err != nil || cluster == nil {
			if wctx.Err() != nil {
				return false, nil
			}
			if retryable(err) {
				readErr = err
				return false, nil
			}
			return false, err
		}
		readErr = nil
		phase, err := h.restoredStatus(cluster, &rec.s12Record, result)
		if err != nil {
			return false, err
		}
		switch phase {
		case "", string(databasev1alpha1.BootstrapPreparing):
			return false, nil
		case string(databasev1alpha1.BootstrapFailed):
			return false, restoreVerdict{fmt.Errorf("the restored cluster's bootstrap ended Failed: %s",
				bootstrapReason(cluster))}
		}
		if rec.Restart.PhaseFirstPastPreparing == "" {
			rec.Restart.PhaseFirstPastPreparing = phase
		}
		switch {
		case phase == string(databasev1alpha1.BootstrapRestoring) && cluster.Status.Bootstrap.OperationID != "":
			return true, nil
		case phase == string(databasev1alpha1.BootstrapRestoring):
			// A failed attempt clears the operation; the next one starts.
			return false, nil
		}
		return false, fmt.Errorf("the restart could not be placed during the restore: the bootstrap was first "+
			"seen %s, not Restoring", phase)
	})
	switch {
	case err == nil:
		return nil
	case ctx.Err() == nil && wctx.Err() != nil && readErr != nil:
		return fmt.Errorf("the restored cluster could not be read before restore_limit %s passed: %w",
			h.cfg.RestoreLimit, readErr)
	case ctx.Err() == nil && wctx.Err() != nil:
		phase := noBootstrapPhase
		if n := len(rec.Bootstrap); n > 0 {
			phase = rec.Bootstrap[n-1].Phase
		}
		return restoreVerdict{fmt.Errorf("the restored cluster's restore did not start within restore_limit %s; its "+
			"last bootstrap phase was %q", h.cfg.RestoreLimit, phase)}
	}
	return err
}

// readRestarted reads the restored cluster. A cluster with another UID was
// replaced, which fails the variant.
func (h *haRun) readRestarted(ctx context.Context, created *databasev1alpha1.CubridCluster) (
	*databasev1alpha1.CubridCluster, error) {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), cluster); err != nil {
		return nil, err
	}
	if cluster.UID != created.UID {
		return nil, restoreVerdict{errors.New("the restored cluster was replaced")}
	}
	return cluster, nil
}

// restartDuringRestore deletes the manager's Pod as S06 restart does, once
// the manager and a bootstrap Restoring with an operation are seen again
// right before, and waits for the replacement. While it waits, the restored
// cluster is read and recorded. It returns the time from the deletion to the
// reading that saw the replacement back.
func (h *haRun) restartDuringRestore(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	rec *s12RestartRecord, result *evidence.Scenario) (time.Duration, error) {
	r := rec.Restart
	// seen is a verdict a reading of the cluster gave while the manager was
	// away.
	var seen error
	var goneAt time.Time
	outcome := faults.Run(ctx, faults.Scenario{
		Fault:  managerPodKill{h: h, pod: r.ManagerBefore},
		Limits: h.s06FlowLimits(),
		Before: func(ctx context.Context) error { return h.beforeRestart(ctx, cluster, rec, result) },
		Check: func(ctx context.Context) (bool, error) {
			if goneAt.IsZero() {
				goneAt = time.Now().UTC()
				r.GoneAt = &goneAt
			}
			if err := h.readDuringRestart(ctx, cluster, rec, result); err != nil {
				seen = err
				return false, err
			}
			done, err := h.managerBack(ctx, r)
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
	if err := h.writeTimeline(s12RestartTimelineFile, outcome.Timeline); err != nil {
		return 0, err
	}
	switch {
	case seen != nil:
		return 0, seen
	case r.ImageChanged:
		return 0, fmt.Errorf("the environment changed: %s", outcome.Reason)
	case outcome.Result == evidence.Fail:
		return 0, restoreVerdict{fmt.Errorf("the manager was not back after its Pod was deleted: %s", outcome.Reason)}
	case outcome.Result != evidence.Pass:
		return 0, errors.New(outcome.Reason)
	}
	back := outcome.OutcomeAt.Sub(outcome.FaultIssuedAt)
	r.BackWithin = back.Round(time.Millisecond).String()
	result.Measurements["operatorBackWithin"] = r.BackWithin
	if err := takeover(rec, result); err != nil {
		return back, err
	}
	return back, nil
}

// takeover records the bootstrap of the last reading at or before the
// replacement acquired the Lease, by the run's clock against the Lease's
// time. The deleted process may still write until its Lease expires: a
// bootstrap past Restoring, or with another operation than at the restart,
// then shows the old manager advanced the restore, and what the replacement
// resumed was not observed, which blocks the variant.
func takeover(rec *s12RestartRecord, result *evidence.Scenario) error {
	r := rec.Restart
	if r.LeaseAcquiredAt == nil {
		return errors.New("the time the replacement acquired the Lease is not known")
	}
	acquired := *r.LeaseAcquiredAt
	var at *s12Phase
	for i := range rec.Bootstrap {
		if rec.Bootstrap[i].At.After(acquired) {
			break
		}
		at = &rec.Bootstrap[i]
	}
	if at == nil {
		return fmt.Errorf("no reading of the restored cluster was taken at or before the replacement acquired the "+
			"Lease at %s", acquired.Format(time.RFC3339Nano))
	}
	r.PhaseAtTakeover, r.OperationIDAtTakeover = cmp.Or(at.Phase, noBootstrapPhase), at.OperationID
	result.Measurements["phaseAtTakeover"] = r.PhaseAtTakeover
	if at.Phase != string(databasev1alpha1.BootstrapRestoring) || at.OperationID != r.OperationIDBefore {
		return fmt.Errorf("the old manager advanced the restore before the replacement led: at the takeover the "+
			"bootstrap was %s with operation %q, at the restart %s with %q", r.PhaseAtTakeover, at.OperationID,
			r.PhaseAtRestart, r.OperationIDBefore)
	}
	return nil
}

// beforeRestart requires the manager Pod LAB01 checked and a reading of the
// restored cluster in bootstrap phase Restoring with an operation, which it
// records as the state at the restart.
func (h *haRun) beforeRestart(ctx context.Context, created *databasev1alpha1.CubridCluster, rec *s12RestartRecord,
	result *evidence.Scenario) error {
	r := rec.Restart
	manager, err := h.observeManager(ctx, h.cfg.Config)
	if err != nil {
		return err
	}
	if len(manager.Pods) != 1 || manager.Pods[0] != r.ManagerBefore {
		return errors.New("the manager Pod is not the one LAB01 checked")
	}
	cluster, err := h.readRestarted(ctx, created)
	if err != nil {
		return err
	}
	phase, err := h.restoredStatus(cluster, &rec.s12Record, result)
	if err != nil {
		return err
	}
	if phase != string(databasev1alpha1.BootstrapRestoring) || cluster.Status.Bootstrap.OperationID == "" {
		return fmt.Errorf("the restart could not be placed during the restore: right before it the bootstrap "+
			"phase was %q", cmp.Or(phase, noBootstrapPhase))
	}
	attempts := cluster.Status.Bootstrap.Attempts
	r.PhaseAtRestart, r.OperationIDBefore, r.AttemptsBefore = phase, cluster.Status.Bootstrap.OperationID, &attempts
	return nil
}

// readDuringRestart reads and records the restored cluster while the
// manager is away. A cluster that cannot be read is not judged here: the
// manager's return is the outcome. A replaced cluster, a bootstrap Failed and
// what S12 exact's readings must never see are verdicts.
func (h *haRun) readDuringRestart(ctx context.Context, created *databasev1alpha1.CubridCluster,
	rec *s12RestartRecord, result *evidence.Scenario) error {
	cluster, err := h.readRestarted(ctx, created)
	if err != nil {
		if errors.As(err, new(restoreVerdict)) {
			return err
		}
		return nil
	}
	phase, err := h.restoredStatus(cluster, &rec.s12Record, result)
	if err != nil {
		return err
	}
	if rec.Restart.PhaseAfterRestart == "" {
		rec.Restart.PhaseAfterRestart = cmp.Or(phase, noBootstrapPhase)
	}
	if phase == string(databasev1alpha1.BootstrapFailed) {
		return restoreVerdict{fmt.Errorf("the restored cluster's bootstrap ended Failed: %s", bootstrapReason(cluster))}
	}
	return nil
}

// managerBack reports whether the manager's replacement is back: one Pod of
// the Deployment that is not the deleted one, with the same runtime image,
// its manager container running, and holding the leader Lease. Another image
// is an error that blocks the variant.
func (h *haRun) managerBack(ctx context.Context, r *s12Restart) (bool, error) {
	manager, err := h.observeManager(ctx, h.cfg.Config)
	if err != nil || len(manager.Pods) != 1 || manager.Pods[0].UID == r.ManagerBefore.UID {
		return false, nil
	}
	if manager.Pods[0].ImageID != r.ManagerBefore.ImageID {
		r.ImageChanged = true
		return false, fmt.Errorf("the replacement manager runs %s, not %s", manager.Pods[0].ImageID,
			r.ManagerBefore.ImageID)
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: manager.Pods[0].Pod},
		pod); err != nil {
		return false, nil
	}
	var started time.Time
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == managerContainer && status.State.Running != nil {
			started = status.State.Running.StartedAt.UTC()
		}
	}
	if started.IsZero() {
		return false, nil
	}
	lease := &coordinationv1.Lease{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: h.cfg.LeaderLease},
		lease); err != nil {
		return false, nil
	}
	if lease.Spec.HolderIdentity == nil || !strings.HasPrefix(*lease.Spec.HolderIdentity, pod.Name+"_") ||
		lease.Spec.AcquireTime == nil {
		return false, nil
	}
	acquired, backAt := lease.Spec.AcquireTime.UTC(), time.Now().UTC()
	r.ManagerAfter, r.ReplacementStartedAt = &manager.Pods[0], &started
	r.LeaseHolder, r.LeaseAcquiredAt, r.BackAt = *lease.Spec.HolderIdentity, &acquired, &backAt
	return true, nil
}

// judgeRestartBootstrap judges the bootstrap the readings recorded, in order.
// The Operator records a restore's operation with the first phase it reports
// for it, polls it by that recorded ID after a restart, and starts a
// second one only after an attempt failed, with the reason
// RestoreFailedRetrying and the attempt counter raised
// (internal/controller/bootstrap.go). So an operation that becomes another
// under the same attempt is a restore started twice, an attempt counter that
// falls is a lost attempt, and BootstrapReady True before Complete a bootstrap
// reported done before its recovery work; each must never happen. An attempt
// counter that rose without a reading of that reason leaves the cause
// unknown, and a retry that left another operation at the end leaves the
// restart's effect on the operation unobserved; both block the variant. The
// operation before and after the
// restart, the attempts and the retries are recorded.
func judgeRestartBootstrap(rec *s12RestartRecord, result *evidence.Scenario) error {
	r := rec.Restart
	var events, unexplained []string
	lastOp, opAttempts, attempts := "", int32(0), int32(0)
	early := false
	for i, p := range rec.Bootstrap {
		boot := meta.FindStatusCondition(p.Conditions, bootstrapReadyCondition)
		if boot != nil && boot.Status == metav1.ConditionTrue && p.Phase != string(databasev1alpha1.BootstrapComplete) &&
			!early {
			early = true
			events = append(events, fmt.Sprintf("the restored cluster reported BootstrapReady True (%s) while its "+
				"bootstrap phase was %q", boot.Reason, cmp.Or(p.Phase, noBootstrapPhase)))
		}
		switch {
		case i > 0 && p.Attempts < attempts:
			events = append(events, fmt.Sprintf("the restored cluster's bootstrap attempts went from %d back to %d",
				attempts, p.Attempts))
		case p.Attempts > attempts && boot != nil && boot.Reason == s12RetryReason:
			r.Retries = append(r.Retries, s12Retry{At: p.At, From: attempts, To: p.Attempts, Reason: boot.Reason,
				Message: boot.Message})
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
		lastOp, opAttempts = p.OperationID, p.Attempts
	}
	if lastOp != "" {
		after := opAttempts
		r.OperationIDAfter, r.AttemptsAfter = lastOp, &after
	}
	if r.OperationIDBefore != "" && r.OperationIDAfter != "" {
		result.Measurements["operationIDUnchanged"] = r.OperationIDBefore == r.OperationIDAfter
		result.Measurements["attemptsAdded"] = *r.AttemptsAfter - *r.AttemptsBefore
	}
	result.NeverEvents = append(result.NeverEvents, events...)
	var errs []error
	if len(unexplained) > 0 {
		errs = append(errs, fmt.Errorf("the restored cluster's bootstrap attempts rose %s, and no reading showed the "+
			"reason %s: the cause is unknown", strings.Join(unexplained, ", "), s12RetryReason))
	}
	// A retry with its reason is no restore started twice, but the operation
	// that ran through the restart is not the one that completed.
	if len(r.Retries) > 0 && r.OperationIDBefore != "" && r.OperationIDAfter != r.OperationIDBefore {
		errs = append(errs, fmt.Errorf("a failed attempt replaced the operation %s with %s; whether the restart "+
			"preserved it was not observed", r.OperationIDBefore, r.OperationIDAfter))
	}
	return errors.Join(errs...)
}
