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
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	s12Lane                = "lab-installed-ha-s12"
	s12ID                  = "S12"
	s12Variant             = "exact"
	s12RecordFile          = "S12/exact/record.json"
	s12DataCheckFile       = "S12/exact/data-check.json"
	s12SourceDataCheckFile = "S12/exact/source-data-check.json"
	// s12ExpectedHistoryFile is the history at the known point, which the
	// restored cluster is checked against; s12PostBackupFile is what the
	// writes after the backup recorded.
	s12ExpectedHistoryFile = "S12/exact/expected-history.jsonl"
	s12PostBackupFile      = "S12/exact/post-backup-history.jsonl"
	// s12ClusterName is the cluster restored from S11's artifact, in the run
	// namespace.
	s12ClusterName = "ha-restore"
	// s12Marker names the marker row written through the restored cluster;
	// its value is the run ID.
	s12Marker = "s12"
	// s12PostBackupClient writes s12PostBackupOperations operations through
	// the source after the backup.
	s12PostBackupClient     = "s12p"
	s12PostBackupOperations = 20
	// s12SecretName is the restored cluster's own copy of the seed Secret.
	s12SecretName = "object-storage-restore"
	// restoreLimitName is the limit S12's variants are bound by.
	restoreLimitName = "restore_limit"
	// noBootstrapPhase and noBootstrapCondition name a bootstrap phase and a
	// BootstrapReady condition that were not seen.
	noBootstrapPhase     = "none"
	noBootstrapCondition = "no BootstrapReady condition"
	// bootstrapReadyCondition is the condition that holds a cluster's
	// bootstrap reason.
	bootstrapReadyCondition = "BootstrapReady"
	// heartbeatStatus prints CUBRID's HA state on a member.
	heartbeatStatus = `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`
)

// haNodeLine is a node line of cubrid heartbeat status (POC-13 shape E).
var haNodeLine = regexp.MustCompile(`(?m)^\s*Node (\S+) \(priority`)

// s12PostBackup is where the client history stood after the writes through
// the source that followed the backup.
type s12PostBackup struct {
	Client string `json:"client"`
	// Acknowledged counts the acknowledged operations of these writes and
	// LastAcknowledged is the last of them.
	Acknowledged     int            `json:"acknowledged"`
	LastAcknowledged string         `json:"lastAcknowledgedOpId,omitempty"`
	Operations       map[string]int `json:"operations"`
	// HistoryEvents counts the events of the whole history after them.
	HistoryEvents int `json:"historyEventsAfter"`
}

// s12Phase is one observed state of the restored cluster's bootstrap; a new
// one is recorded when any of its fields but the time changes.
type s12Phase struct {
	At           time.Time          `json:"at"`
	Phase        string             `json:"phase"`
	Attempts     int32              `json:"attempts"`
	TargetMember string             `json:"targetMember,omitempty"`
	OperationID  string             `json:"operationID,omitempty"`
	ManifestURI  string             `json:"manifestUri,omitempty"`
	Conditions   []metav1.Condition `json:"conditions"`
	// DatabaseAttempts, SeedMember, SeedRestoreOperationID and SeededMembers
	// are of the status of the spec's first database: the attempts of its
	// bootstrap steps (status.databases[].bootstrapAttempts, which seeding
	// raises, not status.bootstrap.attempts), the member being seeded, the
	// seed restore recorded for it, and the members seeded so far.
	DatabaseAttempts       int32    `json:"databaseAttempts,omitempty"`
	SeedMember             string   `json:"seedMember,omitempty"`
	SeedRestoreOperationID string   `json:"seedRestoreOperationID,omitempty"`
	SeededMembers          []string `json:"seededMembers,omitempty"`
}

// s12DataCheck summarizes the data check of the restored cluster.
type s12DataCheck struct {
	// HistoryEvents and Acknowledged are the events of the history as it
	// stood at the known point, and the acknowledged operations among them.
	HistoryEvents int  `json:"historyEvents"`
	Acknowledged  int  `json:"acknowledged"`
	Held          bool `json:"held"`
	// PostBackupRowsPresent names the operations written after the backup
	// whose rows the restored cluster holds; set once a report was read.
	PostBackupRowsPresent []string `json:"postBackupRowsPresent"`
	// Markers is S11's marker, as found on each restored member.
	Markers map[string]string `json:"markers,omitempty"`
}

// s12NormalWrite is the marker written through the restored cluster.
type s12NormalWrite struct {
	Marker       string   `json:"marker"`
	Acknowledged bool     `json:"acknowledged"`
	ReadBackOn   []string `json:"readBackOn"`
	MissingOn    []string `json:"missingOn"`
	// MissingState is, for each member in MissingOn, its replication as the
	// cluster's status and the member's applyinfo reported it, and its state.
	MissingState map[string]string `json:"missingState,omitempty"`
}

// s12Source is the source cluster as S12 read it at its end.
type s12Source struct {
	MembersAfter   []imageIdentity `json:"membersAfter"`
	Restarted      *int            `json:"restarted,omitempty"`
	Master         string          `json:"master,omitempty"`
	CurrentPrimary string          `json:"currentPrimary,omitempty"`
	DataRulesHeld  bool            `json:"dataRulesHeld"`
}

// s12Credentials is where the run looked for the object store's keys.
type s12Credentials struct {
	StatusesChecked int      `json:"statusesChecked"`
	FoundIn         []string `json:"foundIn"`
}

// s12Record is the evidence of S12 exact besides the data checks.
type s12Record struct {
	KnownPoint     *s11KnownPoint  `json:"knownPoint,omitempty"`
	PostBackup     *s12PostBackup  `json:"postBackupWrites,omitempty"`
	ArtifactURI    string          `json:"artifactUri,omitempty"`
	ManifestDigest string          `json:"manifestDigest,omitempty"`
	Cluster        string          `json:"cluster"`
	ClusterUID     string          `json:"clusterUID,omitempty"`
	SeedPrefix     string          `json:"seedPrefix"`
	SecretRef      string          `json:"credentialsSecretRef"`
	CreatedAt      *time.Time      `json:"createdAt,omitempty"`
	Bootstrap      []s12Phase      `json:"bootstrap"`
	RestoredWithin string          `json:"restoredWithin,omitempty"`
	Members        []imageIdentity `json:"members"`
	Roles          *startingState  `json:"roles,omitempty"`
	// HANodes is the node list cubrid heartbeat status reports on one member
	// of each cluster, by cluster.
	HANodes map[string][]string `json:"haNodes,omitempty"`
	// SecretsReferenced are the Secrets the restored cluster's Pod templates
	// name.
	SecretsReferenced []string        `json:"secretsReferenced"`
	DataCheck         *s12DataCheck   `json:"dataCheck,omitempty"`
	NormalWrite       *s12NormalWrite `json:"normalWrite,omitempty"`
	Source            *s12Source      `json:"source,omitempty"`
	Credentials       *s12Credentials `json:"credentials,omitempty"`
	Notes             []string        `json:"notes,omitempty"`
	Residue           string          `json:"residue"`
}

// validateS12 checks the inputs of S12 exact, tampered, wrong-target,
// operator-restart, instance-manager-restart and graceful-stop, when they are
// selected.
func (c HAConfig) validateS12() error {
	if c.RestoreTampered && !c.Restore {
		return errors.New("-restore-tampered needs -restore: S12 tampered follows S12 exact")
	}
	if c.RestoreWrongTarget && !c.Restore {
		return errors.New("-restore-wrong-target needs -restore: S12 wrong-target follows S12 exact")
	}
	if c.RestoreWrongTarget && c.RestoreTampered {
		return errors.New("-restore-wrong-target and -restore-tampered select different lanes; a run takes one of them")
	}
	if err := c.validateS12OperatorRestart(); err != nil {
		return err
	}
	if err := c.validateS12InstanceManagerRestart(); err != nil {
		return err
	}
	if err := c.validateS12GracefulStop(); err != nil {
		return err
	}
	if !c.Restore {
		return nil
	}
	if c.Fault != "" {
		return errors.New("-restore follows S11 and -fault selects S06; a run takes one of them")
	}
	if c.BackupBucket == "" {
		return errors.New("-restore needs -backup-bucket: S12 restores the artifact of S11")
	}
	if c.RestoreLimit <= 0 {
		return errors.New("S12 needs a positive restore limit")
	}
	return nil
}

// restoreVerdict is an error that judges the restore, as against one that
// kept the run from checking it.
type restoreVerdict struct{ error }

// runS12 runs S12 exact after S11 complete passed: the client writes through
// the source after the backup, a new cluster is restored from S11's artifact
// and must be Ready within RestoreLimit only after its bootstrap completed,
// and its members and Services must hold exactly the history truncated at
// S11's known point, S11's marker, and then a marker written through it. The
// source must be unchanged. A run that ends, or a check that cannot be made,
// before S12 is judged blocks it; what was seen to break stays a failure.
func (h *haRun) runS12(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s12ID+"/"+s12Variant, &result)()
	result = evidence.Scenario{ID: s12ID, Variant: s12Variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s12Record{Cluster: s12ClusterName, SeedPrefix: h.rec.Seed.Prefix + "/restore-seed",
		SecretRef: s12SecretName, Bootstrap: []s12Phase{}, Members: []imageIdentity{}, SecretsReferenced: []string{},
		Credentials: &s12Credentials{FoundIn: []string{}},
		Notes: []string{"the restored cluster uses a copy of the seed Secret of its own, " + s12SecretName +
			"; the source uses " + seedSecretName, "the Operator's logs were not checked",
			"use of the source's Secrets is checked in the restored cluster's Pod templates only: the Operator " +
				"itself using one, for example to download the artifact, would not be seen"},
		Residue: "the restored cluster's seed backups under its seed prefix are not removed by this run; the " +
			"cluster goes with the namespace"}
	defer func() {
		// A pass without its evidence is a failure; a blocked or failed S12
		// keeps its result, with the evidence that could not be written.
		if err := h.writeJSON(s12RecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S12 evidence: "+err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S12/")
		})
	}()
	result.Limits = map[string]string{restoreLimitName: h.cfg.RestoreLimit.String()}
	result.Measurements = map[string]any{restoredWithinMeasurement: evidence.Unknown,
		dataRulesHeldMeasurement: evidence.Unknown, postBackupRowsMeasurement: evidence.Unknown}
	err := h.s12Steps(ctx, &result, &rec)
	// A credential found in any status read is a failure, whichever step
	// ended S12.
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	if post := rec.PostBackup; post != nil {
		result.Operations = &evidence.Operations{Attempted: post.Operations[workload.Attempted],
			Acknowledged: post.Operations[workload.Acknowledged], Failed: post.Operations[workload.Failed],
			Unknown: post.Operations[workload.Unknown]}
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
		result.Result, result.Reason = evidence.Blocked, "the run ended before S12 was judged: "+err.Error()
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, err.Error()
	case never != "":
		result.Result, result.Reason = evidence.Fail, never
	default:
		result.Result = evidence.Pass
		result.Reason = "the cluster restored from S11's artifact reported Ready within restore_limit only after its " +
			"bootstrap completed; its members and Services held exactly the rows acknowledged before the known " +
			"point and S11's marker, no row written after the backup, and took a normal write; the source was " +
			"unchanged; no credential was in its status and no Secret of the source in its Pods; the Operator's " +
			"logs were not checked"
	}
	return result
}

// s12Steps runs the steps of S12 exact in order and returns the first error.
func (h *haRun) s12Steps(ctx context.Context, result *evidence.Scenario, rec *s12Record) error {
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S12 could not tell whether a credential is in the restored cluster's status", err)
	}
	s11 := h.s11Rec
	if s11 == nil || s11.KnownPoint == nil || s11.Artifact == nil {
		return errors.New("S11 recorded no known point or artifact to restore")
	}
	rec.KnownPoint, rec.ArtifactURI, rec.ManifestDigest = s11.KnownPoint, s11.Artifact.URI, s11.Artifact.ManifestDigest
	expected, err := h.postBackupWrites(ctx, rec)
	if err != nil {
		return err
	}
	h.s12Expected = expected
	cluster, err := h.createRestored(ctx, s12ClusterName, s12SecretName, rec.SeedPrefix, rec.ArtifactURI)
	if err != nil {
		return err
	}
	created := time.Now()
	createdAt := created.UTC()
	rec.ClusterUID, rec.CreatedAt = string(cluster.UID), &createdAt
	roles, readyAt, err := h.waitRestored(ctx, cluster, created, rec, result)
	if err != nil {
		return err
	}
	rec.RestoredWithin = readyAt.Sub(created).Round(time.Millisecond).String()
	result.Measurements[restoredWithinMeasurement] = rec.RestoredWithin
	if err := h.checkRestoredSecrets(ctx, rec, result); err != nil {
		return err
	}
	if err := h.checkHANodes(ctx, roles, rec, result); err != nil {
		return err
	}
	if err := h.checkRestoredData(ctx, expected, roles, rec, result, s12DataCheckFile); err != nil {
		return err
	}
	if err := h.normalWriteS12(ctx, roles, rec, s12Marker); err != nil {
		return err
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	if err := h.checkSourceS12(ctx, rec.Source, s12SourceDataCheckFile); err != nil {
		return err
	}
	final := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), final); err != nil {
		return fmt.Errorf("read the restored cluster at the end: %w", err)
	}
	rec.Credentials.StatusesChecked++
	if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the restored cluster's status at the end",
		final.Status); err != nil {
		return err
	}
	return nil
}

// postBackupWrites takes the history as it stands, which is where it stood
// at S11's known point: no client runs between LAB01's workload and these
// writes, and S11's marker goes to the marker table. Its acknowledged
// operations must be the known point's. It then writes through the source,
// so that rows written after the backup exist, and returns that history:
// what the restored cluster must hold, an operation with an unknown outcome
// allowed either way. The writes must all be acknowledged: they are part of
// S12's starting state.
func (h *haRun) postBackupWrites(ctx context.Context, rec *s12Record) (string, error) {
	known := rec.KnownPoint
	expected := h.history.String()
	if err := h.write(s12ExpectedHistoryFile, []byte(expected)); err != nil {
		return "", err
	}
	before, err := workload.ReadHistory(strings.NewReader(expected))
	if err != nil {
		return "", err
	}
	snapshot, err := workload.ReadEvents(strings.NewReader(expected))
	if err != nil {
		return "", err
	}
	last := ""
	for _, e := range snapshot {
		if e.Event == workload.Acknowledged {
			last = e.OpID
		}
	}
	if n := before.Counts[workload.Acknowledged]; n != known.Acknowledged || last != known.LastAcknowledged {
		return "", fmt.Errorf("the history holds %d acknowledged operations, the last %q, and S11's known point %d, "+
			"the last %q: the history changed after the known point", n, last, known.Acknowledged,
			known.LastAcknowledged)
	}
	out, runErr := h.clientRun(ctx, fmt.Sprintf("CLIENT_ID=%s OPS=%d ROLLBACK_EVERY=5 workload run 2>/tmp/%[1]s.err",
		s12PostBackupClient, s12PostBackupOperations))
	if runErr != nil {
		// kubectl ends the output of a failed command with its own text, and a
		// client that was stopped may leave a line unfinished: only the events
		// the client recorded go to the history; runErr keeps the whole
		// output. An operation without its answer stays attempted, with an
		// unknown outcome.
		var events strings.Builder
		for line := range strings.Lines(out) {
			var e workload.Event
			if json.Unmarshal([]byte(line), &e) == nil && e.OpID != "" {
				events.WriteString(strings.TrimRight(line, "\n") + "\n")
			}
		}
		out = events.String()
	}
	h.history.WriteString(out)
	if err := h.write(haHistoryFile, []byte(h.history.String())); err != nil {
		return "", err
	}
	if err := h.write(s12PostBackupFile, []byte(out)); err != nil {
		return "", err
	}
	if runErr != nil {
		diagnostics, _ := h.clientRun(ctx, "cat /tmp/"+s12PostBackupClient+".err")
		return "", fmt.Errorf("write through the source after the backup: %w\n%s", runErr, diagnostics)
	}
	written, err := workload.ReadHistory(strings.NewReader(out))
	if err != nil {
		return "", err
	}
	events, err := workload.ReadEvents(strings.NewReader(h.history.String()))
	if err != nil {
		return "", err
	}
	// LAB01's operation counts stay its own client's; these are S12's.
	post := &s12PostBackup{Client: s12PostBackupClient, Operations: written.Counts,
		Acknowledged: written.Counts[workload.Acknowledged], HistoryEvents: len(events)}
	for _, e := range events {
		if e.Client == s12PostBackupClient && e.Event == workload.Acknowledged {
			post.LastAcknowledged = e.OpID
		}
	}
	rec.PostBackup = post
	if written.Counts[workload.Attempted] != s12PostBackupOperations || post.Acknowledged != s12PostBackupOperations {
		return "", fmt.Errorf("%d of %d writes through the source after the backup were acknowledged (attempted %d, "+
			"failed %d, unknown %d)", post.Acknowledged, s12PostBackupOperations, written.Counts[workload.Attempted],
			written.Counts[workload.Failed], written.Counts[workload.Unknown])
	}
	return expected, nil
}

// createRestored creates a restored cluster in the run namespace: the
// source's spec, restored from the artifact manifestURI names, seeding its
// members under seedPrefix with secretName, a copy of the seed Secret of its
// own, which no Pod of the source names. None of the names the Operator
// derives from it is one of the source's (ADR-0004): they all start with the
// cluster's name and a hyphen.
func (h *haRun) createRestored(ctx context.Context, name, secretName, seedPrefix, manifestURI string) (
	*databasev1alpha1.CubridCluster, error) {
	if err := h.guard(ctx); err != nil {
		return nil, err
	}
	// The values are the seed Secret's, which the redactor already removes.
	h.redactor.Add(h.accessKey)
	h.redactor.Add(h.secretKey)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: h.cfg.Namespace},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{seedAccessKey: []byte(h.accessKey),
			seedSecretKey: []byte(h.secretKey)}}
	if err := h.Client.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("copy the seed Secret for %s: %w", name, err)
	}
	cluster := &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.cfg.Namespace},
		Spec:       h.clusterSpec(seedPrefix, secretName)}
	cluster.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
		Recovery: &databasev1alpha1.RecoverySource{ManifestURI: manifestURI}}
	if err := h.Client.Create(ctx, cluster); err != nil {
		return nil, fmt.Errorf("create the cluster %s: %w", name, err)
	}
	if cluster.UID == "" {
		return nil, fmt.Errorf("created cluster %s has no observed UID", name)
	}
	return cluster, nil
}

// waitRestored reads the restored cluster until it is Ready with its
// bootstrap Complete, its members Ready and owned, its Brokers available,
// one master and two slaves by CUBRID and status.currentPrimary naming the
// master, until RestoreLimit has passed since createdAt. Each change of the
// bootstrap is recorded, and every status read is searched for a credential.
// A Ready cluster whose bootstrap is not Complete must never happen.
func (h *haRun) waitRestored(ctx context.Context, created *databasev1alpha1.CubridCluster, createdAt time.Time,
	rec *s12Record, result *evidence.Scenario) (*startingState, time.Time, error) {
	wctx, cancel := context.WithDeadline(ctx, createdAt.Add(h.cfg.RestoreLimit))
	defer cancel()
	// readErr is the last reading's error, nil once a reading was made in
	// full; a reading the limit cut short keeps the one before.
	var readErr error
	var waiting string
	var roles *startingState
	var readyAt time.Time
	err := pollUntil(wctx, h.cfg.Poll, func() (bool, error) {
		cluster := &databasev1alpha1.CubridCluster{}
		if err := h.Client.Get(wctx, client.ObjectKeyFromObject(created), cluster); err != nil {
			if wctx.Err() != nil {
				return false, nil
			}
			if retryable(err) {
				readErr = err
				return false, nil
			}
			return false, err
		}
		if cluster.UID != created.UID {
			return false, restoreVerdict{errors.New("the restored cluster was replaced")}
		}
		state, unmet, notRead, err := h.restoredReading(wctx, cluster, rec, result)
		if err != nil && wctx.Err() != nil && !errors.As(err, new(restoreVerdict)) {
			return false, nil
		}
		readErr = nil
		if notRead {
			readErr = errors.New(unmet)
		}
		if err != nil || unmet != "" {
			waiting = unmet
			return false, err
		}
		roles, readyAt = state, time.Now()
		return true, nil
	})
	switch {
	case err == nil:
		rec.Roles = roles
		return roles, readyAt, nil
	case ctx.Err() == nil && wctx.Err() != nil && readErr != nil:
		// The state at the limit is not known: the run could not judge.
		return nil, time.Time{}, fmt.Errorf("the restored cluster or its objects could not be read before "+
			"restore_limit %s passed: %w", h.cfg.RestoreLimit, readErr)
	case ctx.Err() == nil && wctx.Err() != nil:
		phase := noBootstrapPhase
		if n := len(rec.Bootstrap); n > 0 {
			phase = rec.Bootstrap[n-1].Phase
		}
		return nil, time.Time{}, restoreVerdict{fmt.Errorf("the restored cluster was not Ready within restore_limit "+
			"%s; its last bootstrap phase was %q; waiting for: %s", h.cfg.RestoreLimit, phase,
			cmp.Or(waiting, "the first reading"))}
	}
	return nil, time.Time{}, err
}

// restoredReading records one reading of the restored cluster and returns
// its roles once everything holds, or what does not hold yet and whether
// that is a member Pod, the StatefulSet or a Broker Deployment that could not
// be read, not a state that was seen.
func (h *haRun) restoredReading(ctx context.Context, cluster *databasev1alpha1.CubridCluster, rec *s12Record,
	result *evidence.Scenario) (*startingState, string, bool, error) {
	phase, err := h.restoredStatus(cluster, rec, result)
	if err != nil {
		return nil, "", false, err
	}
	switch phase {
	case string(databasev1alpha1.BootstrapFailed):
		return nil, "", false, restoreVerdict{fmt.Errorf("the restored cluster's bootstrap ended Failed: %s",
			bootstrapReason(cluster))}
	case string(databasev1alpha1.BootstrapComplete):
	default:
		return nil, fmt.Sprintf("the bootstrap phase is %q", phase), false, nil
	}
	if unmet, err := clusterReady(cluster); err != nil {
		return nil, "", false, restoreVerdict{err}
	} else if unmet != "" {
		return nil, unmet, false, nil
	}
	// Only these and rolesOf name what could not be read with unread; a
	// condition's message, which clusterReady passes on, is not searched for
	// it.
	members, unmet, err := h.memberIdentities(ctx, cluster)
	if unmet != "" || err != nil {
		return nil, unmet, strings.Contains(unmet, unread), err
	}
	if unmet, err := h.brokersAvailable(ctx, cluster); unmet != "" || err != nil {
		return nil, unmet, strings.Contains(unmet, unread), err
	}
	state, unmet, err := h.rolesOf(ctx, clusterMembers(cluster.Name))
	if unmet != "" || err != nil {
		return nil, unmet, strings.Contains(unmet, unread), err
	}
	state.CurrentPrimary = cluster.Status.CurrentPrimary
	if state.CurrentPrimary != state.Master {
		return nil, fmt.Sprintf("status.currentPrimary is %q and CUBRID reports %s as master",
			state.CurrentPrimary, state.Master), false, nil
	}
	rec.Members = members
	return state, "", false, nil
}

// restoredStatus records the status of one reading of the restored cluster,
// searches it for a credential and returns its bootstrap phase. Ready True
// while the bootstrap is not Complete must never happen.
func (h *haRun) restoredStatus(cluster *databasev1alpha1.CubridCluster, rec *s12Record, result *evidence.Scenario) (
	string, error) {
	phase := ""
	if boot := cluster.Status.Bootstrap; boot != nil {
		phase = string(boot.Phase)
	}
	recordBootstrap(cluster, &rec.Bootstrap)
	rec.Credentials.StatusesChecked++
	if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the restored cluster's status read in bootstrap "+
		"phase "+cmp.Or(phase, noBootstrapPhase), cluster.Status); err != nil {
		return phase, err
	}
	if ready := meta.FindStatusCondition(cluster.Status.Conditions, startingConditions[0]); ready != nil &&
		ready.Status == metav1.ConditionTrue && phase != string(databasev1alpha1.BootstrapComplete) {
		event := fmt.Sprintf("the restored cluster reported Ready while its bootstrap phase was %q", phase)
		result.NeverEvents = append(result.NeverEvents, event)
		return phase, restoreVerdict{errors.New(event)}
	}
	return phase, nil
}

// recordBootstrap adds a restored cluster's bootstrap to phases when it
// changed: its phase, attempts, target, operation, manifest, the first
// database's attempts and seeding, or the type, status or reason of a
// condition.
func recordBootstrap(cluster *databasev1alpha1.CubridCluster, phases *[]s12Phase) {
	seen := s12Phase{At: time.Now().UTC(), Conditions: slices.Clone(cluster.Status.Conditions)}
	if seen.Conditions == nil {
		seen.Conditions = []metav1.Condition{}
	}
	if boot := cluster.Status.Bootstrap; boot != nil {
		seen.Phase, seen.Attempts, seen.TargetMember = string(boot.Phase), boot.Attempts, boot.TargetMember
		seen.OperationID, seen.ManifestURI = boot.OperationID, boot.ManifestURI
	}
	if len(cluster.Spec.Databases) > 0 {
		for _, db := range cluster.Status.Databases {
			if db.Name != cluster.Spec.Databases[0].Name {
				continue
			}
			seen.DatabaseAttempts, seen.SeededMembers = db.BootstrapAttempts, slices.Clone(db.SeededMembers)
			if db.Seeding != nil {
				seen.SeedMember, seen.SeedRestoreOperationID = db.Seeding.Member, db.Seeding.RestoreOperationID
			}
		}
	}
	key := func(p s12Phase) string {
		var k strings.Builder
		fmt.Fprintf(&k, "%s|%d|%s|%s|%s|%d|%s|%s|%s", p.Phase, p.Attempts, p.TargetMember, p.OperationID,
			p.ManifestURI, p.DatabaseAttempts, p.SeedMember, p.SeedRestoreOperationID,
			strings.Join(p.SeededMembers, ","))
		for _, c := range p.Conditions {
			fmt.Fprintf(&k, "|%s=%s:%s", c.Type, c.Status, c.Reason)
		}
		return k.String()
	}
	if n := len(*phases); n == 0 || key((*phases)[n-1]) != key(seen) {
		*phases = append(*phases, seen)
	}
}

// bootstrapReason is the reason and message of the BootstrapReady condition.
func bootstrapReason(cluster *databasev1alpha1.CubridCluster) string {
	if c := meta.FindStatusCondition(cluster.Status.Conditions, bootstrapReadyCondition); c != nil {
		return c.Reason + ": " + c.Message
	}
	return noBootstrapCondition
}

// checkRestoredSecrets records the Secrets the restored cluster's Pod
// templates name. One that a Pod template of the source also names must
// never happen: the new cluster would depend on a Secret of the source. A Pod
// template names Secrets of its own namespace only.
func (h *haRun) checkRestoredSecrets(ctx context.Context, rec *s12Record, result *evidence.Scenario) error {
	restored, err := h.secretsOf(ctx, rec.Cluster)
	if err != nil {
		return err
	}
	rec.SecretsReferenced = restored
	source, err := h.secretsOf(ctx, haClusterName)
	if err != nil {
		return err
	}
	shared := slices.DeleteFunc(slices.Clone(restored), func(name string) bool {
		return !slices.Contains(source, name)
	})
	if len(shared) > 0 {
		result.NeverEvents = append(result.NeverEvents, "the restored cluster's Pods name Secrets of the source "+
			"cluster: "+strings.Join(shared, ", "))
	}
	return nil
}

// secretsOf returns the Secrets the Pod templates of a cluster's StatefulSet
// and Broker Deployments name.
func (h *haRun) secretsOf(ctx context.Context, cluster string) ([]string, error) {
	sts := &appsv1.StatefulSet{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: cluster}, sts); err != nil {
		return nil, fmt.Errorf("read the StatefulSet of %s: %w", cluster, err)
	}
	names := secretNames(&sts.Spec.Template.Spec)
	for _, mode := range []string{"rw", "ro"} {
		deployment := &appsv1.Deployment{}
		key := client.ObjectKey{Namespace: h.cfg.Namespace, Name: cluster + "-broker-" + mode}
		if err := h.Client.Get(ctx, key, deployment); err != nil {
			return nil, fmt.Errorf("read Deployment %s: %w", key.Name, err)
		}
		names = append(names, secretNames(&deployment.Spec.Template.Spec)...)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// secretNames returns the Secrets a Pod spec names.
func secretNames(spec *corev1.PodSpec) []string {
	names := []string{}
	for _, ref := range spec.ImagePullSecrets {
		names = append(names, ref.Name)
	}
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		for _, env := range c.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
				names = append(names, env.ValueFrom.SecretKeyRef.Name)
			}
		}
		for _, from := range c.EnvFrom {
			if from.SecretRef != nil {
				names = append(names, from.SecretRef.Name)
			}
		}
	}
	for _, v := range spec.Volumes {
		if v.Secret != nil {
			names = append(names, v.Secret.SecretName)
		}
		if v.Projected != nil {
			for _, source := range v.Projected.Sources {
				if source.Secret != nil {
					names = append(names, source.Secret.Name)
				}
			}
		}
	}
	return names
}

// restoredDumps read the ledger through each Service of a restored cluster.
func (h *haRun) restoredDumps(cluster string) map[string]string {
	return map[string]string{
		"rw": fmt.Sprintf(`JDBC_URL="%s" workload dump`, h.jdbcURL(cluster, "rw", 33000)),
		"ro": fmt.Sprintf(`JDBC_URL="%s" workload dump`, h.jdbcURL(cluster, "ro", 33001))}
}

// checkRestoredData applies the data rules of the history as it stood at the
// known point to the restored members and Services, then looks for S11's
// marker on every restored member. A row written after the backup is a row
// no operation of that history has: the rules report it as unexpected, and
// the run names it. The last report is kept in file.
func (h *haRun) checkRestoredData(ctx context.Context, expected string, roles *startingState, rec *s12Record,
	result *evidence.Scenario, file string) error {
	history, err := workload.ReadHistory(strings.NewReader(expected))
	if err != nil {
		return err
	}
	events, err := workload.ReadEvents(strings.NewReader(expected))
	if err != nil {
		return err
	}
	full, err := workload.ReadHistory(strings.NewReader(h.history.String()))
	if err != nil {
		return err
	}
	check := &s12DataCheck{HistoryEvents: len(events), Acknowledged: history.Counts[workload.Acknowledged],
		PostBackupRowsPresent: []string{}}
	rec.DataCheck = check
	members := clusterMembers(rec.Cluster)
	target := dataTarget{members: members, dumps: h.restoredDumps(rec.Cluster), slaves: roles.Slaves}
	report, held, err := h.checkDataOf(ctx, expected, target, time.Now(), file)
	if report != nil {
		check.PostBackupRowsPresent = postBackupRows(*report, full)
		result.Measurements[postBackupRowsMeasurement] = len(check.PostBackupRowsPresent)
	}
	if err != nil {
		if present := check.PostBackupRowsPresent; len(present) > 0 && errors.As(err, new(dataViolation)) {
			return dataViolation{fmt.Errorf("rows written after the backup are on the restored cluster: %s; %w",
				strings.Join(present, ", "), err)}
		}
		return err
	}
	check.Held = true
	result.Measurements[dataRulesHeldMeasurement] = held.Seconds()
	known := rec.KnownPoint
	missing, found, err := h.waitMarker(ctx, members, roles.Master, known.Marker, known.MarkerValue)
	check.Markers = found
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return restoreVerdict{fmt.Errorf("S11's marker %s=%s is not on restored member(s) %s", known.Marker,
			known.MarkerValue, strings.Join(missing, ", "))}
	}
	return nil
}

// postBackupRows returns the operations of the client that wrote after the
// backup whose rows a report found, by ID.
func postBackupRows(report workload.Report, full workload.History) []string {
	present := []string{}
	add := func(v workload.Violation) {
		if op, ok := full.Operations[v.OpID]; ok && op.Client == s12PostBackupClient &&
			!slices.Contains(present, v.OpID) {
			present = append(present, v.OpID)
		}
	}
	for _, check := range report.Members {
		for _, v := range check.Violations {
			if v.Rule != workload.RuleMissing {
				add(v)
			}
		}
	}
	slices.Sort(present)
	return present
}

// markerQuery returns each row of the marker table as name=value.
const markerQuery = `SELECT name || '=' || [value] AS v FROM marker ORDER BY name;`

// waitMarker reads the marker table of every member until each holds
// name=value, the master lacks it, or DataCheckWithin has passed: a slave
// may not have applied the row yet. It returns the members that lack it and
// what each member holds under name.
func (h *haRun) waitMarker(ctx context.Context, members []string, master, name, value string) (
	[]string, map[string]string, error) {
	start := time.Now()
	for {
		found := map[string]string{}
		var missing []string
		for _, pod := range members {
			table, err := h.markerTable(ctx, pod)
			if err != nil {
				return nil, found, err
			}
			if v, ok := table[name]; ok {
				found[pod] = v
			}
			if v, ok := found[pod]; !ok || v != value {
				missing = append(missing, pod)
			}
		}
		if len(missing) == 0 || slices.Contains(missing, master) || time.Since(start) >= h.cfg.DataCheckWithin {
			return missing, found, nil
		}
		select {
		case <-ctx.Done():
			return missing, found, fmt.Errorf("the run ended while marker %s was missing on %s: %w", name,
				strings.Join(missing, ", "), ctx.Err())
		case <-time.After(h.cfg.Poll):
		}
	}
}

// markerTable reads the marker table of a member, by name.
func (h *haRun) markerTable(ctx context.Context, pod string) (map[string]string, error) {
	out, err := sqlcheck.Run(h.csqlRead(ctx, pod), markerQuery)
	if err != nil {
		return nil, fmt.Errorf("read the marker table on %s: %w\n%s", pod, err, h.memberState(ctx, pod))
	}
	rows, err := sqlcheck.Values(out)
	if err != nil {
		return nil, fmt.Errorf("read the marker table on %s: %w", pod, err)
	}
	table := map[string]string{}
	for _, row := range rows {
		if n, v, ok := strings.Cut(row, "="); ok {
			table[n] = v
		}
	}
	return table, nil
}

// memberStateScript prints what CUBRID reports on a member: its heartbeat
// status, the HA mode of its server and the end of its newest server logs.
// Each command is bounded, since a server that stopped answering a read may
// not answer these either.
const memberStateScript = `PATH="${CUBRID}/bin:${PATH}"; ` +
	`timeout 15 cubrid heartbeat status 2>&1 | head -n 40; ` +
	`timeout 15 cubrid changemode ` + databaseName + `@localhost 2>&1 | head -n 5; ` +
	`echo "== server logs"; set -- $(ls -t "${CUBRID}"/log/server/*.err 2>/dev/null | head -n 2); ` +
	`[ $# -eq 0 ] && echo "(none)"; for f; do echo "== $f"; tail -n 20 "$f"; done`

// memberState describes a member right after a read on it failed, so that a
// failure the run cannot judge still shows what the member was doing: the
// restarts of its containers and what CUBRID reports there. What cannot be
// read is said so.
func (h *haRun) memberState(ctx context.Context, pod string) string {
	var b strings.Builder
	current := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: pod}, current); err != nil {
		fmt.Fprintf(&b, "restarts of %s: unknown: %v\n", pod, err)
	} else {
		restarts := make([]string, 0, len(current.Status.ContainerStatuses))
		for _, c := range current.Status.ContainerStatuses {
			restarts = append(restarts, fmt.Sprintf("%s=%d", c.Name, c.RestartCount))
		}
		fmt.Fprintf(&b, "restarts of %s: %s\n", pod, strings.Join(restarts, " "))
	}
	// A failed command's error holds its output.
	if out, err := h.member(ctx, pod, "bash", "-c", memberStateScript); err != nil {
		fmt.Fprintf(&b, "state of %s could not be read in full: %v\n", pod, err)
	} else {
		b.WriteString(out)
	}
	return b.String()
}

// workloadFailed is how kubectl exec reports the workload's own failure: it
// exits 1 on an SQL error, a failed JDBC connection among them. Any other
// exit code, such as 2 for a usage error, 126 or 127 for a command the shell
// could not run, or 137 for a killed process, and any error without one, is
// not an answer of the database.
var workloadFailed = regexp.MustCompile(`(?m)^command terminated with exit code 1\s*$`)

// normalWriteS12 writes marker through the restored cluster's read-write
// Service and reads it back on every restored member. Only the workload's
// own failure fails S12: any other error kept the run from writing.
func (h *haRun) normalWriteS12(ctx context.Context, roles *startingState, rec *s12Record, marker string) error {
	write := &s12NormalWrite{Marker: marker, ReadBackOn: []string{}, MissingOn: []string{}}
	rec.NormalWrite = write
	service := rec.Cluster + "-rw"
	out, err := h.clientRun(ctx, fmt.Sprintf(`JDBC_URL="%s" workload marker %s %s 2>&1`,
		h.jdbcURL(rec.Cluster, "rw", 33000), marker, h.rec.RunID))
	switch {
	case err != nil && (ctx.Err() != nil || !workloadFailed.MatchString(out)):
		return fmt.Errorf("write marker %s through %s: %w", marker, service, err)
	case err != nil:
		return restoreVerdict{fmt.Errorf("the marker %s written through %s failed: %w: %s", marker, service, err,
			strings.TrimSpace(out))}
	case !strings.Contains(out, "marker "+marker+" written"):
		return restoreVerdict{fmt.Errorf("the marker %s written through %s was not acknowledged: %s", marker,
			service, strings.TrimSpace(out))}
	}
	write.Acknowledged = true
	members := clusterMembers(rec.Cluster)
	missing, _, err := h.waitMarker(ctx, members, roles.Master, marker, h.rec.RunID)
	if err != nil {
		return err
	}
	write.MissingOn = append(write.MissingOn, missing...)
	write.ReadBackOn = slices.DeleteFunc(members, func(m string) bool { return slices.Contains(missing, m) })
	if len(missing) > 0 {
		write.MissingState = map[string]string{}
		var replication []string
		for _, pod := range missing {
			status, state := h.replicationState(ctx, rec.Cluster, pod, roles.Master)
			write.MissingState[pod] = state
			replication = append(replication, status)
		}
		return restoreVerdict{fmt.Errorf("the marker %s written through %s is not on restored member(s) %s; %s",
			marker, service, strings.Join(missing, ", "), strings.Join(replication, "; "))}
	}
	return nil
}

// applyinfoScript prints what a member's applier reports for the master's
// copied log, bounded as memberStateScript is. $1 is the master; $0 names
// the script.
const applyinfoScript = `PATH="${CUBRID}/bin:${PATH}"; ` +
	`timeout 15 cubrid applyinfo -L "/var/lib/cubrid/databases/` + databaseName + `_$1" -a ` + databaseName +
	` 2>&1 | head -n 40`

// appliersScript prints a member's applier lock, the database's file under
// $CUBRID/var/APPLYLOGDB that one log applier holds at a time (POC-16, POC-17,
// POC-21), the applylogdb processes that run with whether each holds that file
// open, the locks /proc/locks lists on it, and the end of their newest error
// logs: whether the applier of the master's copied log or another one
// holds the lock (#416). Each part says when there is nothing.
const appliersScript = `L="${CUBRID}/var/APPLYLOGDB/` + databaseName + `"; echo "== applier lock $L"; i=; ` +
	`if [ -f "$L" ]; then ls -l --time-style=full-iso "$L"; i=$(stat -L -c %i "$L" 2>/dev/null); fi; ` +
	`if [ -z "$i" ]; then echo "(none)"; else echo "locks on inode $i:"; ` +
	`grep -E ":$i " /proc/locks || echo "(none)"; fi; ` +
	`echo "== applylogdb processes, and whether each holds the lock open"; n=0; ` +
	`for p in /proc/[0-9]*; do c=$( { tr '\0' ' ' < "$p/cmdline"; } 2>/dev/null); case "$c" in "cub_admin applylogdb"*) ` +
	`h=no; for fd in "$p"/fd/*; do [ -n "$i" ] && [ "$(stat -L -c %i "$fd" 2>/dev/null)" = "$i" ] && h=yes; done; ` +
	`echo "${p#/proc/} holds=$h $c"; n=$((n+1));; esac; done; [ "$n" -eq 0 ] && echo "(none)"; ` +
	`echo "== applylogdb logs"; set -- $(ls -t "${CUBRID}"/log/*applylogdb*.err 2>/dev/null | head -n 4); ` +
	`[ $# -eq 0 ] && echo "(none)"; for f; do echo "== $f"; tail -n 15 "$f"; done`

// replicationState describes a member that lacks a row written through its
// cluster (#416): its replication as the cluster's status reports it, the
// ReplicationHealthy condition, the member's own applyinfo for the master's
// log, its applier lock and appliers, and memberState. It returns a one-line
// summary of the status and the whole description. What cannot be read is
// said so.
func (h *haRun) replicationState(ctx context.Context, cluster, pod, master string) (string, string) {
	summary := "replication in status of " + pod + ": unknown"
	current := &databasev1alpha1.CubridCluster{}
	var b strings.Builder
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: cluster}, current); err != nil {
		fmt.Fprintf(&b, "%s (the cluster could not be read: %v)\n", summary, err)
	} else {
		summary = "replication in status of " + pod + ": none reported"
		for _, instance := range current.Status.Instances {
			if r := instance.Replication; instance.Name == pod && r != nil {
				stalled := "not stalled"
				if r.StalledSince != nil {
					stalled = "stalled since " + r.StalledSince.UTC().Format(time.RFC3339)
				}
				summary = fmt.Sprintf("replication in status of %s: source %s, appliedChanges %d, delayedPages %d, "+
					"failCount %d, %s", pod, r.Source, r.AppliedChanges, r.DelayedPages, r.FailCount, stalled)
			}
		}
		healthy := "no ReplicationHealthy condition"
		if c := meta.FindStatusCondition(current.Status.Conditions, "ReplicationHealthy"); c != nil {
			healthy = fmt.Sprintf("ReplicationHealthy=%s %s: %s", c.Status, c.Reason, c.Message)
		}
		fmt.Fprintf(&b, "%s\n%s\n", summary, healthy)
	}
	if pod == master {
		fmt.Fprintf(&b, "applyinfo of %s: not read, the master applies no log\n", pod)
	} else {
		fmt.Fprintf(&b, "applyinfo of %s for %s's log:\n", pod, master)
		if out, err := h.member(ctx, pod, "bash", "-c", applyinfoScript, "applyinfo", master); err != nil {
			fmt.Fprintf(&b, "could not be read: %v\n", err)
		} else {
			b.WriteString(out)
		}
		if out, err := h.member(ctx, pod, "bash", "-c", appliersScript); err != nil {
			fmt.Fprintf(&b, "the appliers of %s could not be read: %v\n", pod, err)
		} else {
			b.WriteString(out)
		}
	}
	b.WriteString(h.memberState(ctx, pod))
	return summary, b.String()
}

// checkSourceS12 requires the source as LAB01 left it: the same member Pods
// and containers, the same master by CUBRID and status.currentPrimary, no
// marker written through a restored cluster, and the data rules over the
// whole history, writes after the backup included. It records what it read
// in source and the source's data check in dataFile.
func (h *haRun) checkSourceS12(ctx context.Context, source *s12Source, dataFile string) error {
	members, changed, err := h.membersUnchanged(ctx, h.rec.MembersAfter)
	if members != nil {
		source.MembersAfter = members
	}
	if err != nil {
		return fmt.Errorf("read the source's members: %w", err)
	}
	restarted := len(changed)
	source.Restarted = &restarted
	if restarted > 0 {
		return restoreVerdict{fmt.Errorf("the source changed: %d of its members were replaced or restarted: %s",
			restarted, strings.Join(changed, "; "))}
	}
	if err := h.sourceRoles(ctx, source); err != nil {
		return err
	}
	for _, pod := range h.members {
		table, err := h.markerTable(ctx, pod)
		if err != nil {
			return fmt.Errorf("read the source's marker table: %w", err)
		}
		for _, written := range []struct{ marker, cluster string }{{s12Marker, s12ClusterName},
			{s12RestartMarker, s12RestartClusterName}, {s12IMRestartMarker, s12IMRestartClusterName},
			{s12GracefulStopMarker, s12GracefulStopClusterName}} {
			if _, ok := table[written.marker]; ok {
				return restoreVerdict{fmt.Errorf("the source changed: the marker %s written through %s-rw is on %s",
					written.marker, written.cluster, pod)}
			}
		}
	}
	if _, _, err := h.checkDataOf(ctx, h.history.String(), h.sourceTarget(), time.Now(),
		dataFile); err != nil {
		if errors.As(err, new(dataViolation)) {
			return dataViolation{fmt.Errorf("the source changed: %w", err)}
		}
		return fmt.Errorf("check the source's data: %w", err)
	}
	source.DataRulesHeld = true
	return nil
}

// sourceRoles reads the source's roles and status until CUBRID reports the
// master of LAB01's end, with status.currentPrimary naming it, or
// DataCheckWithin has passed: one reading taken while a member answers
// otherwise is not a change of the source. What still differs then fails S12,
// unless the members could not be asked.
func (h *haRun) sourceRoles(ctx context.Context, source *s12Source) error {
	want := h.rec.StartingState.Master
	start := time.Now()
	for {
		roles, unmet, err := h.roles(ctx)
		if err != nil {
			return fmt.Errorf("read the source's roles: %w", err)
		}
		cluster := &databasev1alpha1.CubridCluster{}
		key := client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}
		if err := h.Client.Get(ctx, key, cluster); err != nil {
			return fmt.Errorf("read the source cluster: %w", err)
		}
		problem := unmet
		unasked := strings.HasPrefix(unmet, "cubrid heartbeat status failed") || strings.Contains(unmet, unread)
		if unmet == "" {
			source.Master, source.CurrentPrimary = roles.Master, cluster.Status.CurrentPrimary
			if roles.Master != want || source.CurrentPrimary != want {
				problem = fmt.Sprintf("CUBRID reports %s as master and status.currentPrimary is %q, %s at LAB01's end",
					roles.Master, source.CurrentPrimary, want)
			}
		}
		if problem == "" {
			return nil
		}
		if time.Since(start) >= h.cfg.DataCheckWithin || ctx.Err() != nil {
			if unasked || ctx.Err() != nil {
				return errors.Join(fmt.Errorf("read the source's roles: %s", problem), ctx.Err())
			}
			return restoreVerdict{fmt.Errorf("the source changed: %s", problem)}
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Poll):
		}
	}
}

// checkHANodes reads the node list cubrid heartbeat status reports on the
// master of each cluster. A node of the other cluster, or a list that is not
// the cluster's own members, must never happen: the clusters would share HA
// membership.
func (h *haRun) checkHANodes(ctx context.Context, restored *startingState, rec *s12Record,
	result *evidence.Scenario) error {
	rec.HANodes = map[string][]string{}
	for _, c := range []struct{ cluster, pod string }{{rec.Cluster, restored.Master},
		{haClusterName, h.rec.StartingState.Master}} {
		out, err := h.member(ctx, c.pod, "bash", "-c", heartbeatStatus)
		if err != nil {
			return fmt.Errorf("read the HA node list on %s: %w", c.pod, err)
		}
		nodes := []string{}
		for _, m := range haNodeLine.FindAllStringSubmatch(out, -1) {
			nodes = append(nodes, m[1])
		}
		slices.Sort(nodes)
		rec.HANodes[c.cluster] = nodes
		if len(nodes) == 0 {
			return fmt.Errorf("cubrid heartbeat status on %s lists no node", c.pod)
		}
		if want := clusterMembers(c.cluster); !slices.Equal(nodes, want) {
			result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("cross-cluster HA membership: CUBRID on %s "+
				"lists the nodes %s, not %s", c.pod, strings.Join(nodes, ", "), strings.Join(want, ", ")))
		}
	}
	return nil
}
