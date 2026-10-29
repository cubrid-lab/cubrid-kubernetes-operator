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

// Package installed runs a namespace-scoped SQL connection smoke against an
// existing Operator. It never installs or removes shared infrastructure.
package installed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
)

const (
	laneName          = "lab-installed-smoke"
	scenarioID        = "LAB00"
	clusterName       = "smoke"
	databaseName      = "appdb"
	databaseContainer = "cubrid"
	recordFile        = "LAB00/record.json"
	// installedManagedBy labels the namespace an installed command creates,
	// and the lab's object store admits that label (installed-ha.md).
	installedManagedBy = "cubrid-installed-smoke"
)

// NamespaceMode selects who owns the namespace lifecycle.
// Empty retains the fresh namespace default.
type NamespaceMode string

const (
	FreshNamespace        NamespaceMode = "fresh"
	PreallocatedNamespace NamespaceMode = "preallocated"
)

// Config selects the installed target and bounded evidence run.
type Config struct {
	NamespaceMode                                      NamespaceMode
	NamespaceUID, OwnerConfigMap, OwnerUID, OwnerNonce string
	Namespace, OperatorNamespace, OperatorDeployment   string
	OperatorImage, InstanceManagerImage, Commit        string
	StorageClass, EvidenceDir, Level, ClusterUID       string
	Timeout, CleanupTimeout, Poll                      time.Duration
}

// Runner uses an API client and command runner bound to the same target.
type Runner struct {
	Client   client.Client
	Command  func(context.Context, ...string) (string, error)
	Redactor *evidence.Redactor
	// forward opens a port-forward to port of a Service and returns the local
	// address and a func that ends it (Target.forward). S11 reaches the
	// object store through it.
	forward func(ctx context.Context, namespace, service, port string) (string, func(), error)
	// openStore returns the run's own client of the object store at endpoint;
	// nil selects minio-go.
	openStore func(endpoint, accessKey, secretKey string, secure bool) (objectStore, error)
	// podLogs opens a container's log through the API server; S11 searches
	// the installed manager's and the Instance Manager's logs with it.
	podLogs podLogReader
	// followLogs opens a container's log through the API server and follows
	// it until the container ends or ctx does, without the bound on one read
	// that podLogs has; S11 graceful-stop records the log of a member Pod it
	// deletes with it.
	followLogs podLogReader
	// execStream runs kubectl with args and returns its standard output as
	// it is written; Close ends the process. S11 graceful-stop samples a
	// member's processes through it.
	execStream func(ctx context.Context, args ...string) (io.ReadCloser, error)
	// maxLogBytes bounds what S11 reads of one container instance's log;
	// zero selects defaultMaxLogBytes.
	maxLogBytes int64
}

type sqlStep struct {
	Statement string `json:"statement"`
	Output    string `json:"output"`
	Error     string `json:"error,omitempty"`
}

type record struct {
	RunID                 string          `json:"runId"`
	Namespace             string          `json:"namespace"`
	NamespaceUID          string          `json:"namespaceUID,omitempty"`
	ClusterUID            string          `json:"clusterUID,omitempty"`
	KubernetesClusterUID  string          `json:"kubernetesClusterUID"`
	SelectedCommit        string          `json:"selectedCommit"`
	ExpectedOperatorImage string          `json:"expectedOperatorImage"`
	ExpectedInstanceImage string          `json:"expectedInstanceManagerImage"`
	StorageClass          string          `json:"storageClass"`
	StorageProvisioner    string          `json:"storageProvisioner,omitempty"`
	Operator              managerIdentity `json:"operator"`
	OperatorAfter         managerIdentity `json:"operatorAfter"`
	Database              imageIdentity   `json:"database"`
	SQL                   []sqlStep       `json:"sql"`
	Cleanup               string          `json:"cleanup"`
	NamespaceMode         NamespaceMode   `json:"namespaceMode"`
	OwnershipEpoch        *ownershipEpoch `json:"ownershipEpoch,omitempty"`
	CleanupScope          string          `json:"cleanupScope"`
	RetainedPVCs          []pvcIdentity   `json:"retainedPVCs"`
	FixtureReusable       *bool           `json:"fixtureReusable,omitempty"`
	ResetRequired         *bool           `json:"resetRequired,omitempty"`
	// ManagerChangedAtCleanup is set when preallocated cleanup saw a manager
	// incarnation other than the recorded one. The run then fails.
	ManagerChangedAtCleanup bool `json:"managerChangedAtCleanup,omitempty"`
}

// recordCleanup records the cleanup outcome and returns what fails the run
// because of it. Preallocated cleanup tolerates another manager incarnation;
// the run does not.
func recordCleanup(rec *record, cleanupErr error) error {
	rec.Cleanup = "complete"
	var err error
	if cleanupErr != nil {
		rec.Cleanup = cleanupErr.Error()
		err = fmt.Errorf("scoped cleanup: %w", cleanupErr)
	}
	if rec.ManagerChangedAtCleanup {
		err = errors.Join(err, errors.New("candidate manager incarnation changed before cleanup completed"))
	}
	return err
}

func (c Config) validate() error {
	if c.ClusterUID == "" {
		return errors.New("the expected kube-system namespace UID is required to identify the target cluster")
	}
	if c.NamespaceMode != "" && c.NamespaceMode != FreshNamespace && c.NamespaceMode != PreallocatedNamespace {
		return errors.New("namespace mode must be fresh or preallocated")
	}
	if len(validation.IsDNS1123Label(c.Namespace)) != 0 {
		return errors.New("a valid explicit namespace is required")
	}
	if c.NamespaceMode == PreallocatedNamespace {
		if c.NamespaceUID == "" || c.OwnerUID == "" || c.OwnerNonce == "" ||
			len(validation.IsDNS1123Subdomain(c.OwnerConfigMap)) != 0 {
			return errors.New("preallocated mode requires namespace UID, owner ConfigMap name/UID and nonce")
		}
	} else {
		if !strings.HasPrefix(c.Namespace, "cubrid-lab-") || c.Namespace == c.OperatorNamespace {
			return errors.New("a fresh cubrid-lab- namespace distinct from the Operator namespace is required")
		}
		if c.NamespaceUID != "" || c.OwnerConfigMap != "" || c.OwnerUID != "" || c.OwnerNonce != "" {
			return errors.New("external ownership inputs require explicit preallocated mode")
		}
	}
	if len(validation.IsDNS1123Label(c.OperatorNamespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(c.OperatorDeployment)) != 0 ||
		len(validation.IsDNS1123Subdomain(c.StorageClass)) != 0 {
		return errors.New("explicit Operator namespace/deployment and StorageClass names are required")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(c.Commit) {
		return errors.New("the selected commit must be a full lowercase Git SHA")
	}
	for _, image := range []string{c.OperatorImage, c.InstanceManagerImage} {
		if !validPinnedImage(image, c.Commit) {
			return errors.New("expected images must be pinned by digest or tagged main-<selected commit>")
		}
	}
	if c.Timeout <= 0 || c.CleanupTimeout <= 0 || c.Poll <= 0 {
		return errors.New("run, cleanup and poll durations must be positive")
	}
	if c.Level != "kind" && c.Level != "vm-lab" && c.Level != "unit" {
		return errors.New("an explicit evidence environment level is required")
	}
	return nil
}

// validPinnedImage validates the repository as well as the pin so malformed
// references, including embedded credentials, cannot enter the evidence record.
func validPinnedImage(image, commit string) bool {
	var repository string
	if before, digest, pinned := strings.Cut(image, "@sha256:"); pinned {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(digest) {
			return false
		}
		repository = before
	} else {
		var pinned bool
		repository, pinned = strings.CutSuffix(image, ":main-"+commit)
		if !pinned {
			return false
		}
	}
	// A registry may include a numeric port. Repository path components follow
	// Docker's lowercase naming convention and never contain colons or @ signs.
	component := `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	repositoryPattern := `^(?:[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]+)?/)?` +
		component + `(?:/` + component + `)*$`
	return regexp.MustCompile(repositoryPattern).MatchString(repository)
}

// Run uses the installed manager as read-only infrastructure. The only
// cluster-scoped write is creation/deletion of this run's own namespace.
func (r *Runner) Run(ctx context.Context, cfg Config) (runErr error) {
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
	redactor := r.Redactor
	if redactor == nil {
		redactor = evidence.NewRedactor()
	}
	rec := record{RunID: string(uuid.NewUUID()), Namespace: cfg.Namespace, SelectedCommit: cfg.Commit,
		ExpectedOperatorImage: cfg.OperatorImage, ExpectedInstanceImage: cfg.InstanceManagerImage,
		StorageClass: cfg.StorageClass, SQL: []sqlStep{}, Cleanup: "not owned",
		NamespaceMode: cfg.NamespaceMode, CleanupScope: "namespace-uid", RetainedPVCs: []pvcIdentity{}}
	summary := evidence.Summary{RunID: rec.RunID, StartedAt: time.Now().UTC(),
		Environment: evidence.Environment{Level: cfg.Level, OperatorCommit: cfg.Commit,
			KubernetesVersion: evidence.Unknown, OperatorImageDigest: evidence.Unknown,
			CubridImageDigest: evidence.Unknown, EngineVersion: evidence.Unknown, ClientDriver: "csql"},
		Notes: []string{"Connection smoke against an installed Operator; no installation, VM fault or S00-S15 qualification",
			"Commit is selected by the campaign; actual runtime image identities are recorded separately",
			"Namespace cleanup does not certify deletion of retained backend volumes"}}
	lane := laneName
	variant := ""
	if cfg.NamespaceMode == PreallocatedNamespace {
		lane, variant = preallocatedLane, "preallocated"
		rec.CleanupScope = ownedWorkloadsScope
		rec.FixtureReusable, rec.ResetRequired = new(false), new(true)
		rec.OwnershipEpoch = &ownershipEpoch{ConfigMap: cfg.OwnerConfigMap, UID: cfg.OwnerUID, Nonce: cfg.OwnerNonce}
		summary.Notes = append(summary.Notes, "Component SQL/owned-workload cleanup only; retained PVCs and ownership epoch "+
			"require separate fixture reset evidence")
	}
	requirements := []evidence.Requirement{{ID: scenarioID, Variant: variant}}
	summary.Plan(requirements, "connection smoke has not started")
	scenario := evidence.Scenario{ID: scenarioID, Variant: variant, Result: evidence.Blocked, NeverEvents: []string{}}
	var owned *corev1.Namespace
	var borrowed *namespaceGuard
	var created *databasev1alpha1.CubridCluster
	writeRecord := func() error {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return err
		}
		return redactor.WriteFile(cfg.EvidenceDir, recordFile, append(data, '\n'))
	}
	checkpoint := func() error {
		if err := writeRecord(); err != nil {
			return err
		}
		return summary.Checkpoint(cfg.EvidenceDir, redactor, lane, time.Now())
	}
	defer func() {
		if owned != nil || created != nil {
			cleanupErr := r.cleanupRun(cfg, owned, borrowed)
			cleaned := cleanupErr == nil
			scenario.CleanupSucceeded = &cleaned
			runErr = errors.Join(runErr, recordCleanup(&rec, cleanupErr))
		}
		if err := writeRecord(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("write connection evidence: %w", err))
		} else {
			scenario.Evidence = []string{recordFile}
		}
		if runErr == nil {
			scenario.Result = evidence.Pass
			scenario.Reason = "committed SQL row read through the installed Operator's new cluster"
		} else {
			if owned != nil || created != nil {
				scenario.Result = evidence.Fail
			}
			scenario.Reason = runErr.Error()
		}
		summary.Record(scenario)
		summary.FinishedAt = time.Now().UTC()
		verdictLane := evidence.Lane{Name: lane, Required: requirements}
		runErr = errors.Join(runErr, summary.Conclude(cfg.EvidenceDir, redactor, verdictLane))
		if runErr != nil {
			runErr = errors.New(string(redactor.Redact([]byte(runErr.Error()))))
		}
	}()
	if err := checkpoint(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err := r.observeEnvironment(ctx, cfg, &rec, &summary.Environment); err != nil {
		return err
	}
	if err := checkpoint(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runNamespace, borrowedGuard, err := r.prepareNamespace(ctx, cfg, &rec, false)
	if err != nil {
		return err
	}
	owned, borrowed = runNamespace, borrowedGuard
	guard := func(ctx context.Context) error { return r.requireOwned(ctx, owned, borrowed) }
	if err := checkpoint(); err != nil {
		return err
	}
	if err := guard(ctx); err != nil {
		return err
	}
	cluster := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: cfg.Namespace},
		Spec: databasev1alpha1.CubridClusterSpec{
			Version: "11.4", Image: &databasev1alpha1.CubridImage{Repository: cfg.InstanceManagerImage},
			Databases: []databasev1alpha1.CubridDatabase{{Name: databaseName}},
			Topology:  databasev1alpha1.CubridTopology{PromotableMembers: 1},
			Storage: databasev1alpha1.CubridStorage{Data: databasev1alpha1.CubridStorageSpec{
				Size: resource.MustParse("2Gi"), StorageClassName: &cfg.StorageClass}}}}
	if err := r.Client.Create(ctx, cluster); err != nil {
		return fmt.Errorf("create smoke cluster: %w", err)
	}
	if cluster.UID == "" {
		return errors.New("created smoke cluster has no observed UID")
	}
	created = cluster
	rec.ClusterUID = string(cluster.UID)
	if borrowed != nil {
		borrowed.cluster = cluster
	}
	database, err := r.waitDatabase(ctx, cfg, cluster, guard)
	if err != nil {
		return err
	}
	rec.Database = database
	if borrowed != nil {
		borrowed.requireStorage = true
	}
	summary.Environment.CubridImageDigest = rec.Database.ImageID
	return r.checkSQL(ctx, cfg, guard, &rec, &summary.Environment, checkpoint)
}

func (r *Runner) observeEnvironment(ctx context.Context, cfg Config, rec *record, env *evidence.Environment) error {
	system := &corev1.Namespace{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: "kube-system"}, system); err != nil {
		return fmt.Errorf("identify target cluster: %w", err)
	}
	if string(system.UID) != cfg.ClusterUID {
		return errors.New("target cluster UID does not match the selected lab; no resources were created")
	}
	rec.KubernetesClusterUID = string(system.UID)
	version, err := r.Command(ctx, "version", "-o", "json")
	if err != nil {
		return fmt.Errorf("observe Kubernetes version: %w", err)
	}
	var observed struct{ ServerVersion struct{ GitVersion string } }
	if err := json.Unmarshal([]byte(version), &observed); err != nil || observed.ServerVersion.GitVersion == "" {
		return errors.New("the Kubernetes server version was not observed")
	}
	env.KubernetesVersion = observed.ServerVersion.GitVersion
	rec.Operator, err = r.observeManager(ctx, cfg)
	if err != nil {
		return err
	}
	env.OperatorImageDigest = rec.Operator.Pods[0].ImageID
	sc := &storagev1.StorageClass{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: cfg.StorageClass}, sc); err != nil {
		return fmt.Errorf("observe selected StorageClass: %w", err)
	}
	rec.StorageProvisioner = sc.Provisioner
	return nil
}

func (r *Runner) checkSQL(
	ctx context.Context, cfg Config, guard func(context.Context) error, rec *record,
	env *evidence.Environment, checkpoint func() error,
) error {
	execInDatabase := func(args ...string) (string, error) {
		if err := guard(ctx); err != nil {
			return "", err
		}
		current := &corev1.Pod{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: rec.Database.Pod}, current); err != nil {
			return "", err
		}
		identity, err := podIdentity(current, databaseContainer, cfg.InstanceManagerImage)
		if err != nil || identity.UID != rec.Database.UID || identity.ImageID != rec.Database.ImageID ||
			identity.ContainerID != rec.Database.ContainerID {
			return "", errors.New("database identity changed before command execution")
		}
		command := make([]string, 0, 7+len(args))
		command = append(command, "-n", cfg.Namespace, "exec", rec.Database.Pod, "-c", databaseContainer, "--")
		return r.Command(ctx, append(command, args...)...)
	}
	uid, err := execInDatabase("id", "-u")
	if err != nil || strings.TrimSpace(uid) != "1000" {
		return errors.New("database container did not report the expected non-root UID")
	}
	engine, err := execInDatabase("bash", "-c", "PATH=\"$CUBRID/bin:$PATH\" cubrid_rel")
	if err != nil {
		return fmt.Errorf("observe engine version: %w", err)
	}
	env.EngineVersion = instancemanager.ParseEngineVersion(engine)
	if !strings.HasPrefix(env.EngineVersion, "11.4.") {
		return errors.New("the expected CUBRID 11.4 engine version was not observed")
	}
	runSQL := func(statement string) (string, error) {
		out, err := execInDatabase("bash", "-c",
			"PATH=\"$CUBRID/bin:$PATH\" csql -u dba \"$0\" -c \"$1\"", databaseName+"@localhost", statement)
		step := sqlStep{Statement: statement, Output: out}
		if err != nil {
			step.Error = err.Error()
		}
		rec.SQL = append(rec.SQL, step)
		return out, err
	}
	marker := "installed-" + rec.RunID
	if err := sqlcheck.WriteRow(runSQL, marker); err != nil {
		return err
	}
	if err := checkpoint(); err != nil {
		return err
	}
	if err := sqlcheck.ReadRow(runSQL, marker); err != nil {
		return err
	}
	currentPod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: cfg.Namespace, Name: rec.Database.Pod}
	if err := r.Client.Get(ctx, key, currentPod); err != nil {
		return err
	}
	currentIdentity, err := podIdentity(currentPod, databaseContainer, cfg.InstanceManagerImage)
	if err != nil || currentIdentity.UID != rec.Database.UID || currentIdentity.ImageID != rec.Database.ImageID ||
		currentIdentity.ContainerID != rec.Database.ContainerID {
		return errors.New("database runtime identity changed during the SQL connection check")
	}
	rec.OperatorAfter, err = r.observeManager(ctx, cfg)
	if err != nil {
		return err
	}
	if rec.OperatorAfter.DeploymentUID != rec.Operator.DeploymentUID ||
		!slices.Equal(rec.OperatorAfter.Pods, rec.Operator.Pods) {
		return errors.New("the installed manager identity changed during the connection smoke")
	}
	return nil
}

func (r *Runner) requireNamespace(ctx context.Context, owned *corev1.Namespace) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current := &corev1.Namespace{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(owned), current); err != nil {
		return err
	}
	if owned.UID == "" || current.UID != owned.UID || !current.DeletionTimestamp.IsZero() {
		return errors.New("run namespace was replaced or is terminating; no further work is allowed")
	}
	return nil
}

func (r *Runner) cleanup(ctx context.Context, ns *corev1.Namespace, poll time.Duration) error {
	current := &corev1.Namespace{}
	key := client.ObjectKeyFromObject(ns)
	if err := r.Client.Get(ctx, key, current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if current.UID != ns.UID {
		return errors.New("run namespace was replaced; the replacement was left in place")
	}
	uid := ns.UID
	options := &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
	if err := r.Client.Delete(ctx, ns, options); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return pollUntil(ctx, poll, func() (bool, error) {
		err := r.Client.Get(ctx, key, current)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if current.UID != uid {
			return false, errors.New("run namespace was replaced during cleanup; the replacement was left in place")
		}
		return false, nil
	})
}

func pollUntil(ctx context.Context, interval time.Duration, check func() (bool, error)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := check()
		if done || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// prepareNamespace creates the fresh run namespace, or checks the
// preallocated one. A fresh namespace enforces Pod Security restricted;
// privileged makes it enforce privileged, for the ephemeral container of the
// network faults (test/faults/netfault), and keeps warn and audit at
// restricted, so that a Pod that would break restricted still shows up.
func (r *Runner) prepareNamespace(ctx context.Context, cfg Config, rec *record, privileged bool) (*corev1.Namespace,
	*namespaceGuard, error) {
	if cfg.NamespaceMode == PreallocatedNamespace {
		borrowed := &namespaceGuard{runner: r, cfg: cfg, rec: rec, seen: map[string]string{}}
		if err := borrowed.check(ctx); err != nil {
			return nil, nil, err
		}
		rec.NamespaceUID = cfg.NamespaceUID
		return nil, borrowed, nil
	} else {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace, Labels: map[string]string{
			"app.kubernetes.io/managed-by": installedManagedBy, "app.kubernetes.io/instance": rec.RunID,
			podSecurityEnforce: podSecurityRestricted}}}
		if privileged {
			ns.Labels[podSecurityEnforce] = podSecurityPrivileged
			ns.Labels[podSecurityWarn], ns.Labels[podSecurityAudit] = podSecurityRestricted, podSecurityRestricted
		}
		if err := r.Client.Create(ctx, ns); err != nil {
			return nil, nil, fmt.Errorf("create fresh run namespace (no cleanup authority granted): %w", err)
		}
		if ns.UID == "" {
			return nil, nil, errors.New("created namespace has no observed UID; cleanup authority was not granted")
		}
		rec.NamespaceUID = string(ns.UID)
		return ns, nil, nil
	}

}

// Pod Security labels and levels of a namespace.
const (
	podSecurityEnforce    = "pod-security.kubernetes.io/enforce"
	podSecurityWarn       = "pod-security.kubernetes.io/warn"
	podSecurityAudit      = "pod-security.kubernetes.io/audit"
	podSecurityRestricted = "restricted"
	podSecurityPrivileged = "privileged"
)

// podSecurityLabels returns the Pod Security labels among labels.
func podSecurityLabels(labels map[string]string) map[string]string {
	found := map[string]string{}
	for _, key := range []string{podSecurityEnforce, podSecurityWarn, podSecurityAudit} {
		if value, ok := labels[key]; ok {
			found[key] = value
		}
	}
	return found
}

func (r *Runner) requireOwned(ctx context.Context, owned *corev1.Namespace, borrowed *namespaceGuard) error {
	if borrowed != nil {
		return borrowed.check(ctx)
	}
	return r.requireNamespace(ctx, owned)
}

func (r *Runner) cleanupRun(cfg Config, owned *corev1.Namespace, borrowed *namespaceGuard) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.CleanupTimeout)
	defer cancel()
	if borrowed != nil {
		return borrowed.cleanup(ctx, cfg.Poll)
	}
	return r.cleanup(ctx, owned, cfg.Poll)
}
