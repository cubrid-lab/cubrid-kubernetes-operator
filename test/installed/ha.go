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
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	haLane          = "lab-installed-ha"
	haScenarioID    = "LAB01"
	haClusterName   = "ha"
	haMemberCount   = 3
	haRecordFile    = "LAB01/record.json"
	haHistoryFile   = "LAB01/history.jsonl"
	haDataCheckFile = "LAB01/data-check.json"
	clientPodName   = "workload-client"
	clientContainer = "client"
	// clientUser is the unprivileged user of the workload client image.
	clientUser = 65532
	// seedSecretName is the copy of the seed Secret in the run namespace.
	seedSecretName = "object-storage"
	// seedAccessKey and seedSecretKey are the keys of the seed Secret the
	// DB Pods read (spec.objectStorage.credentialsSecretRef).
	seedAccessKey = "accessKey"
	seedSecretKey = "secretKey"
	// minSecretLength is the shortest value evidence.Redactor removes.
	minSecretLength = 4
	// maxOperations bounds the workload of one run.
	maxOperations = 100000
	roleMaster    = "master"
	roleSlave     = "slave"
)

// databasePhaseFailed is the status.databases[].phase of a bootstrap that
// failed and is not tried again (DatabaseStatus.Phase).
const databasePhaseFailed = "Failed"

// startingConditions are the conditions S01's starting state needs True.
var startingConditions = []string{"Ready", "HAReady", "PrimaryResolved", "RoutingReady"}

// HAConfig selects the installed target, the object store the members are
// seeded through and the workload of an HA run (LAB01).
type HAConfig struct {
	Config
	// WorkloadImage is the SQL workload client, pinned by digest.
	WorkloadImage string
	// SeedEndpoint (host[:port]) and SeedBucket are the object store the
	// members are seeded through; SeedInsecure selects plain HTTP.
	SeedEndpoint, SeedBucket string
	SeedInsecure             bool
	// SeedSecretNamespace and SeedSecret name a Secret with accessKey and
	// secretKey. It is copied into the run namespace; its values are never
	// recorded.
	SeedSecretNamespace, SeedSecret string
	// Operations is how many operations the workload client runs.
	Operations int
	// DataCheckWithin bounds how long, after the workload's last answer, the
	// data rules may take to hold on every member and both Services.
	DataCheckWithin time.Duration
	// StableFor is how long the starting state must keep holding, with the
	// same master, slaves and Pods, before the workload starts.
	StableFor time.Duration
}

func (c HAConfig) validate() error {
	if c.NamespaceMode != FreshNamespace {
		return errors.New("an HA run needs a fresh namespace of its own")
	}
	if err := c.Config.validate(); err != nil {
		return err
	}
	if !strings.Contains(c.WorkloadImage, "@sha256:") || !validPinnedImage(c.WorkloadImage, c.Commit) {
		return errors.New("the workload client image must be pinned by digest")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?$`).MatchString(c.SeedEndpoint) ||
		len(validation.IsDNS1123Subdomain(c.SeedBucket)) != 0 {
		return errors.New("a seed object store endpoint (host[:port]) and bucket are required")
	}
	if len(validation.IsDNS1123Label(c.SeedSecretNamespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(c.SeedSecret)) != 0 || c.SeedSecretNamespace == c.Namespace {
		return errors.New("the seed Secret must be named, outside the run namespace")
	}
	if c.Operations < 1 || c.Operations > maxOperations || c.DataCheckWithin <= 0 || c.StableFor < 0 {
		return fmt.Errorf("operations must be 1 to %d, the data check window positive and the stable period "+
			"not negative", maxOperations)
	}
	return nil
}

type seedRecord struct {
	Endpoint   string `json:"endpoint"`
	Bucket     string `json:"bucket"`
	Prefix     string `json:"prefix"`
	SecretFrom string `json:"secretFrom"`
	// Residue is what the run leaves in the object store.
	Residue string `json:"residue"`
}

// startingState is S01's starting state as the run observed it.
type startingState struct {
	Master         string   `json:"master"`
	Slaves         []string `json:"slaves"`
	CurrentPrimary string   `json:"currentPrimary"`
	// FormationSeconds is set for the starting state only; the roles read at
	// the end leave it out.
	FormationSeconds float64 `json:"formationSeconds,omitempty"`
}

type haRecord struct {
	RunID                 string          `json:"runId"`
	Namespace             string          `json:"namespace"`
	NamespaceUID          string          `json:"namespaceUID,omitempty"`
	ClusterUID            string          `json:"clusterUID,omitempty"`
	KubernetesClusterUID  string          `json:"kubernetesClusterUID"`
	SelectedCommit        string          `json:"selectedCommit"`
	ExpectedOperatorImage string          `json:"expectedOperatorImage"`
	ExpectedInstanceImage string          `json:"expectedInstanceManagerImage"`
	ExpectedWorkloadImage string          `json:"expectedWorkloadImage"`
	StorageClass          string          `json:"storageClass"`
	StorageProvisioner    string          `json:"storageProvisioner,omitempty"`
	Seed                  seedRecord      `json:"seed"`
	Operator              managerIdentity `json:"operator"`
	OperatorAfter         managerIdentity `json:"operatorAfter"`
	// Waiting is the last part of the starting state that did not hold
	// while the run waited for it.
	Waiting       string          `json:"waiting,omitempty"`
	StartingState *startingState  `json:"startingState,omitempty"`
	Members       []imageIdentity `json:"members"`
	MembersAfter  []imageIdentity `json:"membersAfter"`
	// RolesAfter is the master and slaves CUBRID reported at the end.
	RolesAfter *startingState `json:"rolesAfter,omitempty"`
	Client     *imageIdentity `json:"client,omitempty"`
	Operations map[string]int `json:"operations,omitempty"`
	// DataRulesHeldAfterSeconds is the time from the workload's last answer
	// until the data rules held everywhere.
	DataRulesHeldAfterSeconds *float64 `json:"dataRulesHeldAfterSeconds,omitempty"`
	Cleanup                   string   `json:"cleanup"`
	CleanupScope              string   `json:"cleanupScope"`
}

// haRun is the state of one HA run.
type haRun struct {
	*Runner
	cfg      HAConfig
	redactor *evidence.Redactor
	rec      haRecord
	guard    func(context.Context) error
	members  []string
	history  strings.Builder
	files    []string
}

// RunHA creates a three-member HA cluster in a fresh namespace through the
// installed Operator, waits for S01's starting state, runs the workload
// client through the read-write Service, checks the data rules on every
// member and both Services, and removes the namespace. Like Run, its only
// cluster-scoped write is its own namespace; the seed Secret is read, never
// recorded, and copied into that namespace.
func (r *Runner) RunHA(ctx context.Context, cfg HAConfig) (runErr error) {
	if cfg.NamespaceMode == "" {
		cfg.NamespaceMode = FreshNamespace
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	if r.Client == nil || r.Command == nil {
		return errors.New("a target client and command runner are required")
	}
	if err := evidence.RequireEmptyDir(cfg.EvidenceDir); err != nil {
		return err
	}
	h := &haRun{Runner: r, cfg: cfg, redactor: r.Redactor}
	if h.redactor == nil {
		h.redactor = evidence.NewRedactor()
	}
	for i := range haMemberCount {
		h.members = append(h.members, fmt.Sprintf("%s-%d", haClusterName, i))
	}
	runID := string(uuid.NewUUID())
	h.rec = haRecord{RunID: runID, Namespace: cfg.Namespace, SelectedCommit: cfg.Commit,
		ExpectedOperatorImage: cfg.OperatorImage, ExpectedInstanceImage: cfg.InstanceManagerImage,
		ExpectedWorkloadImage: cfg.WorkloadImage, StorageClass: cfg.StorageClass, Cleanup: "not owned",
		CleanupScope: "namespace-uid", Members: []imageIdentity{}, MembersAfter: []imageIdentity{},
		Seed: seedRecord{Endpoint: cfg.SeedEndpoint, Bucket: cfg.SeedBucket, Prefix: "lab-runs/" + runID,
			Residue: "the seed backups under the prefix are not removed by this run"}}
	summary := evidence.Summary{RunID: runID, StartedAt: time.Now().UTC(),
		Environment: evidence.Environment{Level: cfg.Level, OperatorCommit: cfg.Commit,
			KubernetesVersion: evidence.Unknown, OperatorImageDigest: evidence.Unknown,
			CubridImageDigest: evidence.Unknown, EngineVersion: evidence.Unknown,
			ClientDriver: "CUBRID JDBC (workload client)"},
		Notes: []string{"Three-member HA cluster against an installed Operator; no fault and no S00-S15 qualification",
			"Commit is selected by the campaign; actual runtime image identities are recorded separately",
			"Namespace cleanup does not certify deletion of retained backend volumes or of the seed backups"}}
	requirements := []evidence.Requirement{{ID: haScenarioID}}
	summary.Plan(requirements, "the HA run has not started")
	scenario := evidence.Scenario{ID: haScenarioID, Result: evidence.Blocked, NeverEvents: []string{}}
	var owned *corev1.Namespace
	defer func() {
		if owned != nil {
			cleanupErr := r.cleanupRun(cfg.Config, owned, nil)
			cleaned := cleanupErr == nil
			scenario.CleanupSucceeded = &cleaned
			h.rec.Cleanup = "complete"
			if cleanupErr != nil {
				h.rec.Cleanup = cleanupErr.Error()
				runErr = errors.Join(runErr, fmt.Errorf("scoped cleanup: %w", cleanupErr))
			}
		}
		if err := h.writeJSON(haRecordFile, h.rec); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("write HA evidence: %w", err))
		}
		scenario.Evidence = h.files
		if counts := h.rec.Operations; counts != nil {
			scenario.Operations = &evidence.Operations{Attempted: counts[workload.Attempted],
				Acknowledged: counts[workload.Acknowledged], Failed: counts[workload.Failed],
				Unknown: counts[workload.Unknown]}
		}
		scenario.Measurements = h.measurements()
		if runErr == nil {
			scenario.Result = evidence.Pass
			scenario.Reason = "LAB01's starting state, a subset of S01's, held for the stable period, and the data " +
				"rules held on every member and both Services"
		} else {
			if owned != nil {
				scenario.Result = evidence.Fail
			}
			scenario.Reason = runErr.Error()
		}
		summary.Record(scenario)
		summary.FinishedAt = time.Now().UTC()
		runErr = errors.Join(runErr, summary.Conclude(cfg.EvidenceDir, h.redactor,
			evidence.Lane{Name: haLane, Required: requirements}))
		if runErr != nil {
			runErr = errors.New(string(h.redactor.Redact([]byte(runErr.Error()))))
		}
	}()
	checkpoint := func() error {
		if err := h.writeJSON(haRecordFile, h.rec); err != nil {
			return err
		}
		return summary.Checkpoint(cfg.EvidenceDir, h.redactor, haLane, time.Now())
	}
	if err := checkpoint(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	observed := record{RunID: runID}
	if err := r.observeEnvironment(ctx, cfg.Config, &observed, &summary.Environment); err != nil {
		return err
	}
	h.rec.KubernetesClusterUID, h.rec.Operator = observed.KubernetesClusterUID, observed.Operator
	h.rec.StorageProvisioner = observed.StorageProvisioner
	ns, _, err := r.prepareNamespace(ctx, cfg.Config, &observed)
	if err != nil {
		return err
	}
	owned, h.rec.NamespaceUID = ns, observed.NamespaceUID
	h.guard = func(ctx context.Context) error { return r.requireNamespace(ctx, owned) }
	if err := checkpoint(); err != nil {
		return err
	}
	return h.run(ctx, &summary.Environment, checkpoint)
}

func (h *haRun) run(ctx context.Context, env *evidence.Environment, checkpoint func() error) error {
	if err := h.copySeedSecret(ctx); err != nil {
		return err
	}
	cluster, err := h.createCluster(ctx)
	if err != nil {
		return err
	}
	if err := h.waitStartingState(ctx, cluster, checkpoint); err != nil {
		return err
	}
	env.CubridImageDigest = h.rec.Members[0].ImageID
	engine, err := h.member(ctx, h.rec.StartingState.Master, "bash", "-c", `PATH="$CUBRID/bin:$PATH" cubrid_rel`)
	if err != nil {
		return fmt.Errorf("observe engine version: %w", err)
	}
	env.EngineVersion = instancemanager.ParseEngineVersion(engine)
	if !strings.HasPrefix(env.EngineVersion, "11.4.") {
		return errors.New("the expected CUBRID 11.4 engine version was not observed")
	}
	if err := h.startClient(ctx); err != nil {
		return err
	}
	if err := checkpoint(); err != nil {
		return err
	}
	answered, err := h.runWorkload(ctx)
	if err != nil {
		return err
	}
	if err := h.checkData(ctx, answered); err != nil {
		return err
	}
	return h.checkIdentities(ctx)
}

// writeJSON writes v, redacted, as an evidence file and lists it.
func (h *haRun) writeJSON(rel string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return h.write(rel, append(data, '\n'))
}

func (h *haRun) write(rel string, data []byte) error {
	if err := h.redactor.WriteFile(h.cfg.EvidenceDir, rel, data); err != nil {
		return err
	}
	if !slices.Contains(h.files, rel) {
		h.files = append(h.files, rel)
	}
	return nil
}

func (h *haRun) measurements() map[string]any {
	m := map[string]any{"formationSeconds": evidence.Unknown, "dataRulesHeldAfterSeconds": evidence.Unknown}
	if h.rec.StartingState != nil {
		m["formationSeconds"] = h.rec.StartingState.FormationSeconds
	}
	if h.rec.DataRulesHeldAfterSeconds != nil {
		m["dataRulesHeldAfterSeconds"] = *h.rec.DataRulesHeldAfterSeconds
	}
	return m
}

// copySeedSecret copies the keys the DB Pods need from the seed Secret into
// the run namespace. Their values go to the redactor and nowhere else.
func (h *haRun) copySeedSecret(ctx context.Context) error {
	source := &corev1.Secret{}
	key := client.ObjectKey{Namespace: h.cfg.SeedSecretNamespace, Name: h.cfg.SeedSecret}
	if err := h.Client.Get(ctx, key, source); err != nil {
		return fmt.Errorf("read the seed Secret: %w", err)
	}
	data := map[string][]byte{}
	for _, name := range []string{seedAccessKey, seedSecretKey} {
		value := source.Data[name]
		// A shorter value would not be redacted (evidence.Redactor).
		if len(value) < minSecretLength {
			return fmt.Errorf("the seed Secret has no %s of at least %d bytes", name, minSecretLength)
		}
		h.redactor.Add(string(value))
		data[name] = value
	}
	if err := h.guard(ctx); err != nil {
		return err
	}
	copied := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: seedSecretName, Namespace: h.cfg.Namespace},
		Type: corev1.SecretTypeOpaque, Data: data}
	if err := h.Client.Create(ctx, copied); err != nil {
		return fmt.Errorf("copy the seed Secret: %w", err)
	}
	h.rec.Seed.SecretFrom = key.String()
	return nil
}

func (h *haRun) createCluster(ctx context.Context) (*databasev1alpha1.CubridCluster, error) {
	if err := h.guard(ctx); err != nil {
		return nil, err
	}
	cluster := &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: haClusterName, Namespace: h.cfg.Namespace},
		Spec: databasev1alpha1.CubridClusterSpec{
			Version: "11.4", Image: &databasev1alpha1.CubridImage{Repository: h.cfg.InstanceManagerImage},
			Databases:        []databasev1alpha1.CubridDatabase{{Name: databaseName}},
			Topology:         databasev1alpha1.CubridTopology{PromotableMembers: haMemberCount},
			HighAvailability: databasev1alpha1.CubridHighAvailability{Enabled: true},
			ObjectStorage: &databasev1alpha1.CubridObjectStorage{Endpoint: h.cfg.SeedEndpoint,
				Insecure: h.cfg.SeedInsecure, Bucket: h.cfg.SeedBucket, Prefix: h.rec.Seed.Prefix,
				CredentialsSecretRef: corev1.LocalObjectReference{Name: seedSecretName}},
			Storage: databasev1alpha1.CubridStorage{Data: databasev1alpha1.CubridStorageSpec{
				Size: resource.MustParse("2Gi"), StorageClassName: &h.cfg.StorageClass}}}}
	if err := h.Client.Create(ctx, cluster); err != nil {
		return nil, fmt.Errorf("create the HA cluster: %w", err)
	}
	if cluster.UID == "" {
		return nil, errors.New("created HA cluster has no observed UID")
	}
	h.rec.ClusterUID = string(cluster.UID)
	return cluster, nil
}

// member runs a command in a member's database container.
func (h *haRun) member(ctx context.Context, pod string, args ...string) (string, error) {
	if err := h.guard(ctx); err != nil {
		return "", err
	}
	command := make([]string, 0, 7+len(args))
	command = append(command, "-n", h.cfg.Namespace, "exec", pod, "-c", databaseContainer, "--")
	return h.Command(ctx, append(command, args...)...)
}

// clientRun runs a shell script in the workload client.
func (h *haRun) clientRun(ctx context.Context, script string) (string, error) {
	if err := h.guard(ctx); err != nil {
		return "", err
	}
	return h.Command(ctx, "-n", h.cfg.Namespace, "exec", clientPodName, "-c", clientContainer, "--", "sh", "-c", script)
}

// waitStartingState waits for S01's starting state and then for it to hold
// again, with the same master, slaves and Pods, StableFor later. What does
// not hold yet is recorded as waiting. A cluster, StatefulSet, Pod or Broker
// that is not the run's own, or a bootstrap that failed, ends the wait.
func (h *haRun) waitStartingState(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	checkpoint func() error) error {
	created := time.Now()
	var first *startingState
	var firstMembers []imageIdentity
	var firstAt time.Time
	err := pollUntil(ctx, h.cfg.Poll, func() (bool, error) {
		state, members, unmet, err := h.startingState(ctx, cluster)
		if err != nil {
			return false, err
		}
		if unmet == "" && first != nil && (state.Master != first.Master || !slices.Equal(members, firstMembers)) {
			unmet = "the master or a member Pod changed while the starting state settled"
		}
		if unmet != "" {
			first = nil
			if unmet != h.rec.Waiting {
				h.rec.Waiting = unmet
				if err := checkpoint(); err != nil {
					return false, err
				}
			}
			return false, nil
		}
		if first == nil {
			first, firstMembers, firstAt = state, members, time.Now()
			state.FormationSeconds = time.Since(created).Seconds()
		}
		if time.Since(firstAt) < h.cfg.StableFor {
			return false, nil
		}
		h.rec.StartingState, h.rec.Members, h.rec.Waiting = first, firstMembers, ""
		return true, nil
	})
	if err != nil {
		if h.rec.Waiting != "" {
			return fmt.Errorf("the starting state of S01 was not reached: %s: %w", h.rec.Waiting, err)
		}
		return fmt.Errorf("the starting state of S01 was not reached: %w", err)
	}
	return checkpoint()
}

// retryable is an API error that a later reading may not have.
func retryable(err error) bool {
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err)
}

// startingState returns S01's starting state, or what of it does not hold.
func (h *haRun) startingState(ctx context.Context, created *databasev1alpha1.CubridCluster) (
	*startingState, []imageIdentity, string, error) {
	if err := h.guard(ctx); err != nil {
		return nil, nil, "", err
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), cluster); err != nil {
		if retryable(err) {
			return nil, nil, "the cluster could not be read: " + err.Error(), nil
		}
		return nil, nil, "", err
	}
	if cluster.UID != created.UID {
		return nil, nil, "", errors.New("the HA cluster was replaced")
	}
	if unmet, err := clusterReady(cluster); unmet != "" || err != nil {
		return nil, nil, unmet, err
	}
	members, unmet, err := h.memberIdentities(ctx, cluster)
	if unmet != "" || err != nil {
		return nil, nil, unmet, err
	}
	if unmet, err := h.brokersAvailable(ctx, cluster); unmet != "" || err != nil {
		return nil, nil, unmet, err
	}
	state, unmet, err := h.roles(ctx)
	if unmet != "" || err != nil {
		return nil, nil, unmet, err
	}
	state.CurrentPrimary = cluster.Status.CurrentPrimary
	if state.CurrentPrimary != state.Master {
		return nil, nil, fmt.Sprintf("status.currentPrimary is %q and CUBRID reports %s as master",
			state.CurrentPrimary, state.Master), nil
	}
	return state, members, "", nil
}

// clusterReady reports what of the cluster's status does not hold yet, and
// a bootstrap that failed as an error: it does not start again by itself.
func clusterReady(cluster *databasev1alpha1.CubridCluster) (string, error) {
	for _, db := range cluster.Status.Databases {
		if db.Phase == databasePhaseFailed {
			reason := "no reason recorded"
			if c := meta.FindStatusCondition(cluster.Status.Conditions, "BootstrapReady"); c != nil {
				reason = c.Reason + ": " + c.Message
			}
			return "", fmt.Errorf("the bootstrap of database %s failed: %s", db.Name, reason)
		}
	}
	if cluster.Status.ObservedGeneration < cluster.Generation {
		return "status has not observed the current generation", nil
	}
	for _, condition := range startingConditions {
		c := meta.FindStatusCondition(cluster.Status.Conditions, condition)
		if c == nil {
			return condition + " is not reported", nil
		}
		if c.Status != metav1.ConditionTrue {
			return fmt.Sprintf("%s is %s (%s: %s)", condition, c.Status, c.Reason, c.Message), nil
		}
	}
	return "", nil
}

// roles asks CUBRID on every member for its role. One active master and
// standby slaves are a state; anything else is what does not hold.
func (h *haRun) roles(ctx context.Context) (*startingState, string, error) {
	state := &startingState{Slaves: []string{}}
	var masters []string
	for _, pod := range h.members {
		out, err := h.member(ctx, pod, "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status`)
		if err != nil {
			// The run's deadline is not a member's failure.
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			return nil, fmt.Sprintf("cubrid heartbeat status failed on %s", pod), nil
		}
		switch heartbeatRole(pod, out) {
		case roleMaster:
			masters = append(masters, pod)
		case roleSlave:
			state.Slaves = append(state.Slaves, pod)
		default:
			return nil, pod + " is neither an active master nor a standby slave", nil
		}
	}
	if len(masters) != 1 || len(state.Slaves) != haMemberCount-1 {
		return nil, fmt.Sprintf("CUBRID reports masters %v and slaves %v", masters, state.Slaves), nil
	}
	state.Master = masters[0]
	return state, "", nil
}

// heartbeatRole reads a member's own role from cubrid heartbeat status.
func heartbeatRole(pod, out string) string {
	switch {
	case strings.Contains(out, "current "+pod+", state master") && strings.Contains(out, "registered_and_active"):
		return roleMaster
	case strings.Contains(out, "current "+pod+", state slave") && strings.Contains(out, "registered_and_standby"):
		return roleSlave
	}
	return ""
}

// memberIdentities returns each member's Pod identity once every member is
// Ready, owned through the cluster's StatefulSet.
func (h *haRun) memberIdentities(ctx context.Context, cluster *databasev1alpha1.CubridCluster) (
	[]imageIdentity, string, error) {
	sts := &appsv1.StatefulSet{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), sts); err != nil {
		if apierrors.IsNotFound(err) || retryable(err) {
			return nil, "the StatefulSet could not be read: " + err.Error(), nil
		}
		return nil, "", err
	}
	if !controllerOwner(sts, "CubridCluster", cluster.UID) {
		return nil, "", errors.New("the database StatefulSet does not belong to the HA cluster")
	}
	identities := make([]imageIdentity, 0, haMemberCount)
	for _, name := range h.members {
		pod := &corev1.Pod{}
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, pod); err != nil {
			if apierrors.IsNotFound(err) || retryable(err) {
				return nil, name + " could not be read: " + err.Error(), nil
			}
			return nil, "", err
		}
		if !controllerOwner(pod, "StatefulSet", sts.UID) {
			return nil, "", fmt.Errorf("pod %s does not belong to the HA StatefulSet", name)
		}
		identity, err := podIdentity(pod, databaseContainer, h.cfg.InstanceManagerImage)
		if err != nil {
			return nil, fmt.Sprintf("%s: %v", name, err), nil
		}
		identities = append(identities, identity)
	}
	return identities, "", nil
}

// brokersAvailable reports a Broker Deployment that is not fully available.
func (h *haRun) brokersAvailable(ctx context.Context, cluster *databasev1alpha1.CubridCluster) (string, error) {
	for _, mode := range []string{"rw", "ro"} {
		deployment := &appsv1.Deployment{}
		name := haClusterName + "-broker-" + mode
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, deployment); err != nil {
			if apierrors.IsNotFound(err) || retryable(err) {
				return name + " could not be read: " + err.Error(), nil
			}
			return "", err
		}
		if !controllerOwner(deployment, "CubridCluster", cluster.UID) {
			return "", fmt.Errorf("deployment %s does not belong to the HA cluster", name)
		}
		s := deployment.Status
		if s.Replicas == 0 || s.UpdatedReplicas != s.Replicas || s.AvailableReplicas != s.Replicas {
			return fmt.Sprintf("%s is not fully available (replicas %d, updated %d, available %d)",
				name, s.Replicas, s.UpdatedReplicas, s.AvailableReplicas), nil
		}
	}
	return "", nil
}

// clientPod runs the workload client without privileges or an API token. It
// sleeps; the run executes the client's commands in it.
func (h *haRun) clientPod() *corev1.Pod {
	jdbc := func(mode string, port int) string {
		return fmt.Sprintf("jdbc:cubrid:%s-%s.%s.svc:%d:%s:::?connectTimeout=10&queryTimeout=30",
			haClusterName, mode, h.cfg.Namespace, port, databaseName)
	}
	no, yes, user, grace := false, true, int64(clientUser), int64(1)
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: clientPodName, Namespace: h.cfg.Namespace,
		Labels: map[string]string{"app.kubernetes.io/name": clientPodName,
			"app.kubernetes.io/managed-by": installedManagedBy, "app.kubernetes.io/instance": h.rec.RunID}},
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: &no,
			TerminationGracePeriodSeconds: &grace,
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: clientContainer, Image: h.cfg.WorkloadImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "infinity"},
				Env: []corev1.EnvVar{{Name: "JDBC_URL", Value: jdbc("rw", 33000)},
					{Name: "RO_JDBC_URL", Value: jdbc("ro", 33001)}},
				SecurityContext: &corev1.SecurityContext{RunAsNonRoot: &yes, RunAsUser: &user,
					AllowPrivilegeEscalation: &no,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}}}}
}

// clientStuck are the reasons a client container waits that do not pass
// by themselves.
var clientStuck = []string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError",
	"CreateContainerError"}

func (h *haRun) startClient(ctx context.Context) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	pod := h.clientPod()
	if err := h.Client.Create(ctx, pod); err != nil {
		return fmt.Errorf("create the workload client: %w", err)
	}
	if pod.UID == "" {
		return errors.New("created workload client has no observed UID")
	}
	uid := pod.UID
	_, digest, _ := strings.Cut(h.cfg.WorkloadImage, "@")
	err := pollUntil(ctx, h.cfg.Poll, func() (bool, error) {
		if err := h.Client.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			if retryable(err) {
				return false, nil
			}
			return false, err
		}
		if pod.UID != uid {
			return false, errors.New("the workload client was replaced")
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("the workload client ended: %s", pod.Status.Phase)
		}
		for _, status := range pod.Status.ContainerStatuses {
			if w := status.State.Waiting; w != nil && slices.Contains(clientStuck, w.Reason) {
				return false, fmt.Errorf("the workload client cannot start: %s: %s", w.Reason, w.Message)
			}
		}
		identity, err := podIdentity(pod, clientContainer, h.cfg.WorkloadImage)
		if err != nil {
			return false, nil
		}
		if !strings.HasSuffix(identity.ImageID, digest) {
			return false, fmt.Errorf("the workload client runs image %s, not the pinned %s", identity.ImageID, digest)
		}
		h.rec.Client = &identity
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("wait for the workload client: %w", err)
	}
	return nil
}

// runWorkload creates the tables, runs the operations through the
// read-write Service and one write through the read-only Service, and
// returns when the last answer came. Every operation through the read-write
// Service must be acknowledged, and the write through the read-only Service
// refused by the read-only Broker.
func (h *haRun) runWorkload(ctx context.Context) (time.Time, error) {
	if out, err := h.clientRun(ctx, "workload init 2>&1"); err != nil {
		return time.Time{}, fmt.Errorf("create the workload tables: %w\n%s", err, out)
	}
	run := func(settings string) (workload.History, error) {
		out, err := h.clientRun(ctx, settings+" workload run 2>/tmp/workload.err")
		h.history.WriteString(out)
		if werr := h.write(haHistoryFile, []byte(h.history.String())); werr != nil {
			return workload.History{}, werr
		}
		if err != nil {
			diagnostics, _ := h.clientRun(ctx, "cat /tmp/workload.err")
			return workload.History{}, fmt.Errorf("workload run: %w\n%s", err, diagnostics)
		}
		history, err := workload.ReadHistory(strings.NewReader(h.history.String()))
		if err != nil {
			return workload.History{}, err
		}
		h.rec.Operations = history.Counts
		return workload.ReadHistory(strings.NewReader(out))
	}
	written, err := run(fmt.Sprintf("CLIENT_ID=c1 OPS=%d ROLLBACK_EVERY=5", h.cfg.Operations))
	if err != nil {
		return time.Time{}, err
	}
	if n := written.Counts[workload.Acknowledged]; written.Counts[workload.Attempted] != h.cfg.Operations ||
		n != h.cfg.Operations {
		return time.Time{}, fmt.Errorf("%d of %d operations through the read-write Service were acknowledged "+
			"(attempted %d, failed %d, unknown %d)", n, h.cfg.Operations, written.Counts[workload.Attempted],
			written.Counts[workload.Failed], written.Counts[workload.Unknown])
	}
	refused, err := run(`JDBC_URL="$RO_JDBC_URL" ENDPOINT=ro CLIENT_ID=ro OPS=1 ROLLBACK_EVERY=0`)
	answered := time.Now()
	if err != nil {
		return time.Time{}, err
	}
	if err := workload.CheckReadOnlyRefusal(refused); err != nil {
		return time.Time{}, err
	}
	return answered, nil
}

// lagging is a violation a slave that has not applied the last rows yet
// can cause: a row a slave, or the read-only Service in front of the slaves,
// lacks. Every other violation fails at once, a row the master or the
// read-write Service lacks among them.
func (h *haRun) lagging(member string, v workload.Violation) bool {
	if v.Rule == workload.RuleMembersDiverge {
		return strings.HasPrefix(v.Detail, "present on ")
	}
	return v.Rule == workload.RuleMissing && (member == "ro" || slices.Contains(h.rec.StartingState.Slaves, member))
}

// stopper returns the first violation that lag cannot explain, if any.
func (h *haRun) stopper(r workload.Report) (string, workload.Violation, bool) {
	for _, name := range slices.Sorted(maps.Keys(r.Members)) {
		for _, v := range r.Members[name].Violations {
			if !h.lagging(name, v) {
				return name, v, true
			}
		}
	}
	for _, v := range r.Diverged {
		if !h.lagging("", v) {
			return "", v, true
		}
	}
	return "", workload.Violation{}, false
}

// checkData applies the data rules to every member, read directly, and to
// both Services. While the only violations are rows a member lacks, they
// are read again until the rules hold or DataCheckWithin has passed since
// the workload's last answer. The last report read is kept in any case.
func (h *haRun) checkData(ctx context.Context, answered time.Time) error {
	history, err := workload.ReadHistory(strings.NewReader(h.history.String()))
	if err != nil {
		return err
	}
	var report *workload.Report
	keep := func() error {
		if report == nil {
			return nil
		}
		return h.writeJSON(haDataCheckFile, struct {
			Operations map[string]int `json:"operations"`
			workload.Report
		}{history.Counts, *report})
	}
	for {
		readCtx, cancel := context.WithTimeout(ctx, max(h.cfg.DataCheckWithin-time.Since(answered), 0)+readSlack)
		rows, readErr := h.readRows(readCtx)
		cancel()
		if readErr == nil {
			checked := workload.Check(history, rows)
			report = &checked
			if checked.OK {
				held := time.Since(answered).Seconds()
				h.rec.DataRulesHeldAfterSeconds = &held
				return keep()
			}
		}
		final := time.Since(answered) >= h.cfg.DataCheckWithin || ctx.Err() != nil
		var stopMember string
		var stopViolation workload.Violation
		stopped := false
		if readErr == nil {
			stopMember, stopViolation, stopped = h.stopper(*report)
			final = final || stopped
		}
		if final {
			if err := keep(); err != nil {
				return err
			}
			if report == nil || (readErr != nil && ctx.Err() != nil) {
				return errors.Join(fmt.Errorf("read the ledger rows: %w", readErr), ctx.Err())
			}
			member, v, total := report.FirstViolation(append(slices.Clone(h.members), "rw", "ro"), "")
			if stopped {
				member, v = stopMember, stopViolation
			}
			return errors.Join(fmt.Errorf("the data rules did not hold: %s on %s for %s: %s (%d violations in all)",
				v.Rule, cmp.Or(member, "the members"), v.OpID, v.Detail, total), readErr)
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Poll):
		}
	}
}

// readSlack is how long one reading of the rows may take beyond the data
// check window.
const readSlack = 30 * time.Second

// readRows reads the ledger rows of every member, not through a Broker, and
// through both Services.
func (h *haRun) readRows(ctx context.Context) (map[string][]workload.Row, error) {
	rows := map[string][]workload.Row{}
	for _, pod := range h.members {
		csql := func(sql string) (string, error) {
			return h.member(ctx, pod, "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" csql -u dba "$0" -c "$1"`,
				databaseName+"@localhost", sql)
		}
		out, err := sqlcheck.Run(csql, workload.LedgerQuery)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pod, err)
		}
		values, err := sqlcheck.Values(out)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pod, err)
		}
		if rows[pod], err = workload.ReadRows(strings.NewReader(strings.Join(values, "\n"))); err != nil {
			return nil, fmt.Errorf("%s: %w", pod, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(dumps)) {
		out, err := h.clientRun(ctx, dumps[name]+" 2>/tmp/dump.err")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if rows[name], err = workload.ReadRows(strings.NewReader(out)); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return rows, nil
}

// dumps read the ledger through each Service.
var dumps = map[string]string{"rw": "workload dump", "ro": `JDBC_URL="$RO_JDBC_URL" workload dump`}

// checkIdentities requires the same cluster, the same member Pods and
// containers, the same master and slaves as CUBRID and the status reported
// them at the starting state, and the same manager: no member was replaced
// or restarted, and no role moved.
func (h *haRun) checkIdentities(ctx context.Context) error {
	cluster := &databasev1alpha1.CubridCluster{}
	key := client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}
	if err := h.Client.Get(ctx, key, cluster); err != nil {
		return err
	}
	if string(cluster.UID) != h.rec.ClusterUID {
		return errors.New("the HA cluster was replaced during the run")
	}
	members, unmet, err := h.memberIdentities(ctx, cluster)
	if err != nil {
		return err
	}
	h.rec.MembersAfter = members
	if unmet != "" {
		return fmt.Errorf("a member is not as at the starting state: %s", unmet)
	}
	for i, before := range h.rec.Members {
		if members[i] != before {
			return fmt.Errorf("member %s was replaced or restarted during the run", before.Pod)
		}
	}
	roles, unmet, err := h.roles(ctx)
	if err != nil {
		return err
	}
	if unmet != "" {
		return fmt.Errorf("the roles are not as at the starting state: %s", unmet)
	}
	roles.CurrentPrimary = cluster.Status.CurrentPrimary
	h.rec.RolesAfter = roles
	if start := h.rec.StartingState; roles.Master != start.Master || roles.CurrentPrimary != start.Master {
		return fmt.Errorf("the master moved during the run: CUBRID reports %s and status.currentPrimary %q, "+
			"%s at the starting state", roles.Master, roles.CurrentPrimary, start.Master)
	}
	h.rec.OperatorAfter, err = h.observeManager(ctx, h.cfg.Config)
	if err != nil {
		return err
	}
	if h.rec.OperatorAfter.DeploymentUID != h.rec.Operator.DeploymentUID ||
		!slices.Equal(h.rec.OperatorAfter.Pods, h.rec.Operator.Pods) {
		return errors.New("the installed manager identity changed during the HA run")
	}
	return nil
}
