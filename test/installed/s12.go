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
)

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
}

// s12DataCheck summarizes the data check of the restored cluster.
type s12DataCheck struct {
	// HistoryEvents and Acknowledged are the events of the history truncated
	// at the known point, and the acknowledged operations among them.
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

// validateS12 checks the inputs of S12 exact, when it is selected.
func (c HAConfig) validateS12() error {
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

// historyThrough returns the lines of history up to and including the
// acknowledgement of opID: what the client was told before the known point.
// An attempt of opID, or any other event of it, does not end it.
func historyThrough(history, opID string) (string, error) {
	if opID == "" {
		return "", errors.New("the known point names no acknowledged operation")
	}
	var b strings.Builder
	line := 0
	for text := range strings.SplitAfterSeq(history, "\n") {
		line++
		b.WriteString(text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		var e workload.Event
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			return "", fmt.Errorf("history line %d: %w", line, err)
		}
		if e.OpID == opID && e.Event == workload.Acknowledged {
			return b.String(), nil
		}
	}
	return "", fmt.Errorf("the history holds no acknowledgement of %s", opID)
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
	result = evidence.Scenario{ID: s12ID, Variant: s12Variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s12Record{Cluster: s12ClusterName, SeedPrefix: h.rec.Seed.Prefix + "/restore-seed",
		SecretRef: seedSecretName, Bootstrap: []s12Phase{}, Members: []imageIdentity{}, SecretsReferenced: []string{},
		Credentials: &s12Credentials{FoundIn: []string{}},
		Notes: []string{"the restored cluster uses the run's copy of the seed Secret, object-storage, which the " +
			"run created in its namespace and neither cluster owns", "the Operator's logs were not checked"},
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
	result.Limits = map[string]string{"restore_limit": h.cfg.RestoreLimit.String()}
	result.Measurements = map[string]any{"restoredWithin": evidence.Unknown,
		"dataRulesHeldAfterSeconds": evidence.Unknown, "postBackupRowsPresent": evidence.Unknown}
	err := h.s12Steps(ctx, &result, &rec)
	never := strings.Join(result.NeverEvents, "; ")
	switch {
	case errors.As(err, new(restoreVerdict)) || errors.As(err, new(dataViolation)):
		result.Result, result.Reason = evidence.Fail, err.Error()
		if never != "" && !slices.Contains(result.NeverEvents, err.Error()) {
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
	truncated, err := h.postBackupWrites(ctx, rec)
	if err != nil {
		return err
	}
	cluster, err := h.createRestored(ctx, rec)
	if err != nil {
		return err
	}
	created := time.Now()
	roles, readyAt, err := h.waitRestored(ctx, cluster, rec, result)
	if err != nil {
		return err
	}
	rec.RestoredWithin = readyAt.Sub(created).Round(time.Millisecond).String()
	result.Measurements["restoredWithin"] = rec.RestoredWithin
	if err := h.checkRestoredSecrets(ctx, rec, result); err != nil {
		return err
	}
	if err := h.checkRestoredData(ctx, truncated, roles, rec, result); err != nil {
		return err
	}
	if err := h.normalWriteS12(ctx, roles, rec); err != nil {
		return err
	}
	if err := h.checkSourceS12(ctx, rec); err != nil {
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
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	return nil
}

// postBackupWrites truncates the history at S11's known point, then writes
// through the source, so that rows written after the backup exist, and
// returns the truncated history. The writes must all be acknowledged: they
// are part of S12's starting state.
func (h *haRun) postBackupWrites(ctx context.Context, rec *s12Record) (string, error) {
	known := rec.KnownPoint
	truncated, err := historyThrough(h.history.String(), known.LastAcknowledged)
	if err != nil {
		return "", fmt.Errorf("truncate the history at S11's known point: %w", err)
	}
	before, err := workload.ReadHistory(strings.NewReader(truncated))
	if err != nil {
		return "", err
	}
	if n := before.Counts[workload.Acknowledged]; n != known.Acknowledged {
		return "", fmt.Errorf("the history through %s holds %d acknowledged operations, S11's known point %d",
			known.LastAcknowledged, n, known.Acknowledged)
	}
	out, runErr := h.clientRun(ctx, fmt.Sprintf("CLIENT_ID=%s OPS=%d ROLLBACK_EVERY=5 workload run 2>/tmp/%[1]s.err",
		s12PostBackupClient, s12PostBackupOperations))
	h.history.WriteString(out)
	if err := h.write(haHistoryFile, []byte(h.history.String())); err != nil {
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
	all, err := workload.ReadHistory(strings.NewReader(h.history.String()))
	if err != nil {
		return "", err
	}
	h.rec.Operations = all.Counts
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
	return truncated, nil
}

// createRestored creates the restored cluster in the run namespace: the
// source's spec, restored from S11's artifact, seeding its members under a
// prefix of its own with the run's copy of the seed Secret. None of the
// names the Operator derives from it is one of the source's (ADR-0004):
// they all start with the cluster's name and a hyphen.
func (h *haRun) createRestored(ctx context.Context, rec *s12Record) (*databasev1alpha1.CubridCluster, error) {
	if err := h.guard(ctx); err != nil {
		return nil, err
	}
	cluster := &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: s12ClusterName, Namespace: h.cfg.Namespace},
		Spec:       h.clusterSpec(rec.SeedPrefix)}
	cluster.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
		Recovery: &databasev1alpha1.RecoverySource{ManifestURI: rec.ArtifactURI}}
	if err := h.Client.Create(ctx, cluster); err != nil {
		return nil, fmt.Errorf("create the restored cluster: %w", err)
	}
	if cluster.UID == "" {
		return nil, errors.New("created restored cluster has no observed UID")
	}
	created := time.Now().UTC()
	rec.ClusterUID, rec.CreatedAt = string(cluster.UID), &created
	return cluster, nil
}

// waitRestored reads the restored cluster until it is Ready with its
// bootstrap Complete, its members Ready and owned, its Brokers available,
// one master and two slaves by CUBRID and status.currentPrimary naming the
// master, for at most RestoreLimit. Each change of the bootstrap is
// recorded, and every status read is searched for a credential. A Ready
// cluster whose bootstrap is not Complete must never happen.
func (h *haRun) waitRestored(ctx context.Context, created *databasev1alpha1.CubridCluster, rec *s12Record,
	result *evidence.Scenario) (*startingState, time.Time, error) {
	wctx, cancel := context.WithTimeout(ctx, h.cfg.RestoreLimit)
	defer cancel()
	// readErr is the last reading's error, nil once a reading succeeded.
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
		readErr = nil
		if cluster.UID != created.UID {
			return false, restoreVerdict{errors.New("the restored cluster was replaced")}
		}
		state, unmet, err := h.restoredReading(wctx, cluster, rec, result)
		if err != nil && wctx.Err() != nil && !errors.As(err, new(restoreVerdict)) {
			return false, nil
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
		return nil, time.Time{}, fmt.Errorf("the restored cluster could not be read before restore_limit %s "+
			"passed: %w", h.cfg.RestoreLimit, readErr)
	case ctx.Err() == nil && wctx.Err() != nil:
		phase := "none"
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
// its roles once everything holds, or what does not hold yet.
func (h *haRun) restoredReading(ctx context.Context, cluster *databasev1alpha1.CubridCluster, rec *s12Record,
	result *evidence.Scenario) (*startingState, string, error) {
	phase := ""
	if boot := cluster.Status.Bootstrap; boot != nil {
		phase = string(boot.Phase)
	}
	recordBootstrap(cluster, rec)
	rec.Credentials.StatusesChecked++
	if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the restored cluster's status read in bootstrap "+
		"phase "+cmp.Or(phase, "none"), cluster.Status); err != nil {
		return nil, "", err
	}
	if ready := meta.FindStatusCondition(cluster.Status.Conditions, startingConditions[0]); ready != nil &&
		ready.Status == metav1.ConditionTrue && phase != string(databasev1alpha1.BootstrapComplete) {
		event := fmt.Sprintf("the restored cluster reported Ready while its bootstrap phase was %q", phase)
		result.NeverEvents = append(result.NeverEvents, event)
		return nil, "", restoreVerdict{errors.New(event)}
	}
	switch phase {
	case string(databasev1alpha1.BootstrapFailed):
		return nil, "", restoreVerdict{fmt.Errorf("the restored cluster's bootstrap ended Failed: %s",
			bootstrapReason(cluster))}
	case string(databasev1alpha1.BootstrapComplete):
	default:
		return nil, fmt.Sprintf("the bootstrap phase is %q", phase), nil
	}
	if unmet, err := clusterReady(cluster); err != nil {
		return nil, "", restoreVerdict{err}
	} else if unmet != "" {
		return nil, unmet, nil
	}
	members, unmet, err := h.memberIdentities(ctx, cluster)
	if unmet != "" || err != nil {
		return nil, unmet, err
	}
	if unmet, err := h.brokersAvailable(ctx, cluster); unmet != "" || err != nil {
		return nil, unmet, err
	}
	state, unmet, err := h.rolesOf(ctx, clusterMembers(cluster.Name))
	if unmet != "" || err != nil {
		return nil, unmet, err
	}
	state.CurrentPrimary = cluster.Status.CurrentPrimary
	if state.CurrentPrimary != state.Master {
		return nil, fmt.Sprintf("status.currentPrimary is %q and CUBRID reports %s as master",
			state.CurrentPrimary, state.Master), nil
	}
	rec.Members = members
	return state, "", nil
}

// recordBootstrap records the restored cluster's bootstrap when it changed:
// its phase, attempts, target, operation, manifest, or the type, status or
// reason of a condition.
func recordBootstrap(cluster *databasev1alpha1.CubridCluster, rec *s12Record) {
	seen := s12Phase{At: time.Now().UTC(), Conditions: slices.Clone(cluster.Status.Conditions)}
	if seen.Conditions == nil {
		seen.Conditions = []metav1.Condition{}
	}
	if boot := cluster.Status.Bootstrap; boot != nil {
		seen.Phase, seen.Attempts, seen.TargetMember = string(boot.Phase), boot.Attempts, boot.TargetMember
		seen.OperationID, seen.ManifestURI = boot.OperationID, boot.ManifestURI
	}
	key := func(p s12Phase) string {
		var k strings.Builder
		fmt.Fprintf(&k, "%s|%d|%s|%s|%s", p.Phase, p.Attempts, p.TargetMember, p.OperationID, p.ManifestURI)
		for _, c := range p.Conditions {
			fmt.Fprintf(&k, "|%s=%s:%s", c.Type, c.Status, c.Reason)
		}
		return k.String()
	}
	if n := len(rec.Bootstrap); n == 0 || key(rec.Bootstrap[n-1]) != key(seen) {
		rec.Bootstrap = append(rec.Bootstrap, seen)
	}
}

// bootstrapReason is the reason and message of the BootstrapReady condition.
func bootstrapReason(cluster *databasev1alpha1.CubridCluster) string {
	if c := meta.FindStatusCondition(cluster.Status.Conditions, "BootstrapReady"); c != nil {
		return c.Reason + ": " + c.Message
	}
	return "no BootstrapReady condition"
}

// checkRestoredSecrets records the Secrets the restored cluster's Pod
// templates name. One that a Pod template of the source names, other than the
// run's copy of the seed Secret, must never happen: the new cluster would
// depend on a Secret of the source. A Pod template names Secrets of its own
// namespace only.
func (h *haRun) checkRestoredSecrets(ctx context.Context, rec *s12Record, result *evidence.Scenario) error {
	restored, err := h.secretsOf(ctx, s12ClusterName)
	if err != nil {
		return err
	}
	rec.SecretsReferenced = restored
	source, err := h.secretsOf(ctx, haClusterName)
	if err != nil {
		return err
	}
	shared := slices.DeleteFunc(slices.Clone(restored), func(name string) bool {
		return name == seedSecretName || !slices.Contains(source, name)
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

// restoredDumps read the ledger through each Service of the restored
// cluster.
func (h *haRun) restoredDumps() map[string]string {
	return map[string]string{
		"rw": fmt.Sprintf(`JDBC_URL="%s" workload dump`, h.jdbcURL(s12ClusterName, "rw", 33000)),
		"ro": fmt.Sprintf(`JDBC_URL="%s" workload dump`, h.jdbcURL(s12ClusterName, "ro", 33001))}
}

// checkRestoredData applies the data rules of the history truncated at the
// known point to the restored members and Services, then looks for S11's
// marker on every restored member. A row written after the backup is a row
// no operation of the truncated history has: the rules report it as
// unexpected, and the run names it.
func (h *haRun) checkRestoredData(ctx context.Context, truncated string, roles *startingState, rec *s12Record,
	result *evidence.Scenario) error {
	history, err := workload.ReadHistory(strings.NewReader(truncated))
	if err != nil {
		return err
	}
	events, err := workload.ReadEvents(strings.NewReader(truncated))
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
	members := clusterMembers(s12ClusterName)
	target := dataTarget{members: members, dumps: h.restoredDumps(), slaves: roles.Slaves}
	report, held, err := h.checkDataOf(ctx, truncated, target, time.Now(), s12DataCheckFile)
	if report != nil {
		check.PostBackupRowsPresent = postBackupRows(*report, full)
		result.Measurements["postBackupRowsPresent"] = len(check.PostBackupRowsPresent)
	}
	if err != nil {
		if present := check.PostBackupRowsPresent; len(present) > 0 && errors.As(err, new(dataViolation)) {
			return dataViolation{fmt.Errorf("rows written after the backup are on the restored cluster: %s; %w",
				strings.Join(present, ", "), err)}
		}
		return err
	}
	check.Held = true
	result.Measurements["dataRulesHeldAfterSeconds"] = held.Seconds()
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
			out, err := sqlcheck.Run(h.csql(ctx, pod), markerQuery)
			if err != nil {
				return nil, found, fmt.Errorf("read the marker table on %s: %w", pod, err)
			}
			rows, err := sqlcheck.Values(out)
			if err != nil {
				return nil, found, fmt.Errorf("read the marker table on %s: %w", pod, err)
			}
			for _, row := range rows {
				if n, v, ok := strings.Cut(row, "="); ok && n == name {
					found[pod] = v
				}
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

// normalWriteS12 writes a marker through the restored cluster's read-write
// Service and reads it back on every restored member.
func (h *haRun) normalWriteS12(ctx context.Context, roles *startingState, rec *s12Record) error {
	write := &s12NormalWrite{Marker: s12Marker, ReadBackOn: []string{}, MissingOn: []string{}}
	rec.NormalWrite = write
	service := s12ClusterName + "-rw"
	out, err := h.clientRun(ctx, fmt.Sprintf(`JDBC_URL="%s" workload marker %s %s 2>&1`,
		h.jdbcURL(s12ClusterName, "rw", 33000), s12Marker, h.rec.RunID))
	switch {
	case err != nil && ctx.Err() != nil:
		return fmt.Errorf("write marker %s through %s: %w", s12Marker, service, err)
	case err != nil:
		return restoreVerdict{fmt.Errorf("the marker %s written through %s failed: %w: %s", s12Marker, service, err,
			strings.TrimSpace(out))}
	case !strings.Contains(out, "marker "+s12Marker+" written"):
		return restoreVerdict{fmt.Errorf("the marker %s written through %s was not acknowledged: %s", s12Marker,
			service, strings.TrimSpace(out))}
	}
	write.Acknowledged = true
	members := clusterMembers(s12ClusterName)
	missing, _, err := h.waitMarker(ctx, members, roles.Master, s12Marker, h.rec.RunID)
	if err != nil {
		return err
	}
	write.MissingOn = append(write.MissingOn, missing...)
	write.ReadBackOn = slices.DeleteFunc(members, func(m string) bool { return slices.Contains(missing, m) })
	if len(missing) > 0 {
		return restoreVerdict{fmt.Errorf("the marker %s written through %s is not on restored member(s) %s",
			s12Marker, service, strings.Join(missing, ", "))}
	}
	return nil
}

// checkSourceS12 requires the source as LAB01 left it: the same member Pods
// and containers, the same master by CUBRID and status.currentPrimary, and
// the data rules over the whole history, writes after the backup included.
func (h *haRun) checkSourceS12(ctx context.Context, rec *s12Record) error {
	source := &s12Source{MembersAfter: []imageIdentity{}}
	rec.Source = source
	members, restarted, err := h.membersUnchanged(ctx, h.rec.MembersAfter)
	if members != nil {
		source.MembersAfter = members
	}
	if err != nil {
		return fmt.Errorf("read the source's members: %w", err)
	}
	source.Restarted = &restarted
	if restarted > 0 {
		return restoreVerdict{fmt.Errorf("the source changed: %d of its members were replaced or restarted", restarted)}
	}
	roles, unmet, err := h.roles(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("read the source's roles: %w", err)
	case strings.HasPrefix(unmet, "cubrid heartbeat status failed"):
		return fmt.Errorf("read the source's roles: %s", unmet)
	case unmet != "":
		return restoreVerdict{fmt.Errorf("the source changed: %s", unmet)}
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return fmt.Errorf("read the source cluster: %w", err)
	}
	source.Master, source.CurrentPrimary = roles.Master, cluster.Status.CurrentPrimary
	if want := h.rec.StartingState.Master; roles.Master != want || source.CurrentPrimary != want {
		return restoreVerdict{fmt.Errorf("the source changed: CUBRID reports %s as master and status.currentPrimary "+
			"is %q, %s at LAB01's end", roles.Master, source.CurrentPrimary, want)}
	}
	if _, _, err := h.checkDataOf(ctx, h.history.String(), h.sourceTarget(), time.Now(),
		s12SourceDataCheckFile); err != nil {
		if errors.As(err, new(dataViolation)) {
			return dataViolation{fmt.Errorf("the source changed: %w", err)}
		}
		return fmt.Errorf("check the source's data: %w", err)
	}
	source.DataRulesHeld = true
	return nil
}
