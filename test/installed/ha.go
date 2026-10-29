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
	// haCubridVersion is the cluster's spec.version.
	haCubridVersion = "11.4"
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
	// Fault selects a scenario after LAB01: empty for none, or
	// FaultOperatorRestart for S06 restart, which deletes the installed
	// manager's Pod.
	Fault string
	// StablePeriod is how long the clients must stay acknowledged after the
	// fault (stable_period); ResyncLimit bounds the replacement manager's
	// first fresh status (operator_resync_limit), and in S11 and S12
	// operator-restart its return.
	StablePeriod, ResyncLimit time.Duration
	// LeaderLease is the manager's leader election Lease in its namespace,
	// for S06 restart and S11 and S12 operator-restart.
	LeaderLease string
	// BackupBucket selects S11 complete after LAB01: a CubridBackup of the
	// run's database to this bucket. It cannot be combined with Fault.
	BackupBucket string
	// BackupLimit bounds the backup from its request until Completed or
	// Failed (backup_limit), and in S11 missing-destination,
	// denied-credentials, signature-mismatch, interrupted-upload and
	// instance-manager-restart until Failed.
	// In S11 operator-restart it bounds the second backup until Completed or
	// Failed, the manager's restart included.
	BackupLimit time.Duration
	// BackupMissingDestination selects S11 missing-destination after S11
	// complete: a CubridBackup of the run's database to a bucket that does
	// not exist. It needs BackupBucket and cannot be combined with Restore.
	BackupMissingDestination bool
	// BackupDeniedCredentials selects S11 denied-credentials after S11
	// complete: a CubridBackup to BackupBucket of a standalone cluster whose
	// spec.objectStorage Secret holds keys the object store refuses. It
	// needs BackupBucket and cannot be combined with Restore or
	// BackupMissingDestination.
	BackupDeniedCredentials bool
	// BackupDeniedReadyLimit bounds that standalone cluster from its
	// creation until it is Ready. It is the run's bound, not the contract's.
	BackupDeniedReadyLimit time.Duration
	// BackupSignatureMismatch selects S11 signature-mismatch after S11 complete: a
	// CubridBackup to BackupBucket of a standalone cluster whose
	// spec.objectStorage Secret holds the seed Secret's access key with a
	// wrong secret key. It needs BackupBucket and cannot be combined with
	// Restore, BackupMissingDestination, BackupDeniedCredentials or
	// BackupInterruptedUpload.
	BackupSignatureMismatch bool
	// BackupSignatureMismatchReadyLimit bounds that standalone cluster from its
	// creation until it is Ready. It is the run's bound, not the contract's.
	BackupSignatureMismatchReadyLimit time.Duration
	// BackupInterruptedUpload selects S11 interrupted-upload after S11
	// complete: a CubridBackup to BackupBucket of a standalone cluster whose
	// upload to the object store is limited in rate and then reset by
	// network faults in its member's Pod. It needs BackupBucket, makes the
	// run namespace's Pod Security enforce level privileged, and cannot be
	// combined with Restore, BackupMissingDestination,
	// BackupDeniedCredentials or BackupSignatureMismatch.
	BackupInterruptedUpload bool
	// BackupInterruptedReadyLimit bounds that standalone cluster from its
	// creation until it is Ready. It is the run's bound, not the contract's.
	BackupInterruptedReadyLimit time.Duration
	// BackupOperatorRestart selects S11 operator-restart after S11 complete:
	// a second CubridBackup of the HA cluster to BackupBucket, during which
	// the installed manager's Pod is deleted without a grace period. It needs
	// BackupBucket, ResyncLimit and LeaderLease, and cannot be combined with
	// Restore, BackupMissingDestination, BackupDeniedCredentials,
	// BackupSignatureMismatch, BackupInterruptedUpload or
	// BackupInstanceManagerRestart.
	BackupOperatorRestart bool
	// BackupInstanceManagerRestart selects S11 instance-manager-restart
	// after S11 complete: a CubridBackup to BackupBucket of a standalone
	// cluster whose upload is limited in rate by a network fault in its
	// member's Pod, and whose member's Instance Manager is killed during the
	// upload. It needs BackupBucket, makes the run namespace's Pod Security
	// enforce level privileged, and cannot be combined with Restore or
	// another S11 variant.
	BackupInstanceManagerRestart bool
	// BackupInstanceManagerRestartReadyLimit bounds that standalone cluster
	// from its creation until it is Ready, and from the kill until it is
	// Ready again. It is the run's bound, not the contract's.
	BackupInstanceManagerRestartReadyLimit time.Duration
	// BackupOverlapping selects S11 overlapping after S11 complete: two
	// CubridBackups of a standalone cluster to BackupBucket, requested back
	// to back, each under a prefix of its own, while the member's upload is
	// limited in rate by a network fault in its Pod. It needs BackupBucket,
	// makes the run namespace's Pod Security enforce level privileged, and
	// cannot be combined with Restore or another S11 variant.
	BackupOverlapping bool
	// BackupOverlappingReadyLimit bounds that standalone cluster from its
	// creation until it is Ready. It is the run's bound, not the contract's.
	BackupOverlappingReadyLimit time.Duration
	// ObjectStoreService is the object store's Service as
	// <namespace>/<service>:<port>. The run downloads the artifact through a
	// port-forward to it, with its own client.
	ObjectStoreService string
	// Restore selects S12 exact after S11 complete: a new cluster restored
	// from S11's artifact. It needs BackupBucket.
	Restore bool
	// RestoreLimit bounds the restored cluster from its creation until it is
	// Ready (restore_limit), and the clusters restored from the tampered copy
	// and over volumes that hold a database until their restore is refused.
	RestoreLimit time.Duration
	// RestoreTampered selects S12 tampered after S12 exact: a copy of S11's
	// artifact with one byte changed, restored into another new cluster. It
	// needs Restore.
	RestoreTampered bool
	// RestoreWrongTarget selects S12 wrong-target after S12 exact: S11's
	// artifact restored into a new cluster over the retained volumes of a
	// cluster of the same name, which hold a database. It needs Restore and
	// cannot be combined with RestoreTampered.
	RestoreWrongTarget bool
	// RestoreOperatorRestart selects S12 operator-restart after S12 exact:
	// S11's artifact restored into another new cluster while the manager's
	// Pod is deleted during the restore. It needs Restore, ResyncLimit and
	// LeaderLease, and cannot be combined with RestoreTampered or
	// RestoreWrongTarget.
	RestoreOperatorRestart bool
	// RestoreInstanceManagerRestart selects S12 instance-manager-restart
	// after S12 exact: S11's artifact restored into another new cluster while
	// the Instance Manager of the restore's target member is killed during
	// the restore. It needs Restore, and cannot be combined with
	// RestoreTampered, RestoreWrongTarget or RestoreOperatorRestart.
	RestoreInstanceManagerRestart bool
	// RestoreInstanceManagerRestartAt places that kill in the recovery:
	// IMRestartAtRestoring, the default when empty, IMRestartAtPreparing or
	// IMRestartAtSeeding. Each placement is a variant and a lane of its own.
	// Another value than the default needs RestoreInstanceManagerRestart.
	RestoreInstanceManagerRestartAt string
}

// privileged reports whether the run namespace must enforce Pod Security
// privileged: S11 interrupted-upload, instance-manager-restart and
// overlapping add an ephemeral container with NET_ADMIN and NET_RAW to a
// member's Pod.
func (c HAConfig) privileged() bool {
	return c.BackupInterruptedUpload || c.BackupInstanceManagerRestart || c.BackupOverlapping
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
	if c.Fault != "" && c.Fault != FaultOperatorRestart {
		return fmt.Errorf("fault must be empty or %s", FaultOperatorRestart)
	}
	if c.Fault != "" && (c.StablePeriod <= 0 || c.ResyncLimit <= 0 ||
		len(validation.IsDNS1123Subdomain(c.LeaderLease)) != 0) {
		return errors.New("a fault needs a positive stable period and resync limit, and the leader Lease")
	}
	if err := c.validateS11(); err != nil {
		return err
	}
	if err := c.validateS12(); err != nil {
		return err
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
	RunID                 string     `json:"runId"`
	Namespace             string     `json:"namespace"`
	NamespaceUID          string     `json:"namespaceUID,omitempty"`
	ClusterUID            string     `json:"clusterUID,omitempty"`
	KubernetesClusterUID  string     `json:"kubernetesClusterUID"`
	SelectedCommit        string     `json:"selectedCommit"`
	ExpectedOperatorImage string     `json:"expectedOperatorImage"`
	ExpectedInstanceImage string     `json:"expectedInstanceManagerImage"`
	ExpectedWorkloadImage string     `json:"expectedWorkloadImage"`
	StorageClass          string     `json:"storageClass"`
	StorageProvisioner    string     `json:"storageProvisioner,omitempty"`
	Seed                  seedRecord `json:"seed"`
	// PodSecurity are the Pod Security labels the run created its namespace
	// with.
	PodSecurity   map[string]string `json:"podSecurity,omitempty"`
	Operator      managerIdentity   `json:"operator"`
	OperatorAfter managerIdentity   `json:"operatorAfter"`
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
	// LostSessions are the csql reads whose session was lost, which the run
	// made again (#238).
	LostSessions []lostSession `json:"lostSessions,omitempty"`
}

// lostSession is one csql read on a member whose output held both errors of
// a session the server dropped (POC-36): when, in which scenario, on which
// member, the first line of the query, the error lines and the size of csql's
// output, and how the read made again answered.
type lostSession struct {
	At          time.Time `json:"at"`
	Scenario    string    `json:"scenario"`
	Pod         string    `json:"pod"`
	Query       string    `json:"query"`
	Errors      []string  `json:"errors"`
	OutputBytes int       `json:"outputBytes"`
	Again       string    `json:"again"`
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
	// labPassed is set once LAB01's checks have passed.
	labPassed bool
	// s06 is the result of S06 restart, once it ran.
	s06 *evidence.Scenario
	// s11 is the result of S11 complete, once it ran, and s11Rec its record;
	// s11Missing is that of S11 missing-destination, s11Denied that of S11
	// denied-credentials, s11Signature that of S11 signature-mismatch and
	// s11Interrupted that of S11 interrupted-upload, s11Restart that of S11
	// operator-restart, s11IMRestart that of S11 instance-manager-restart and
	// s11Overlapping that of S11 overlapping.
	s11, s11Missing, s11Denied, s11Signature, s11Interrupted, s11Restart, s11IMRestart,
	s11Overlapping *evidence.Scenario
	s11Rec *s11Record
	// s12 is the result of S12 exact, once it ran, s12Tampered that of S12
	// tampered, s12WrongTarget that of S12 wrong-target and s12Restart that of
	// S12 operator-restart, s12IMRestart that of S12 instance-manager-restart
	// at the selected placement.
	// s12Expected is the history at S11's known point that S12 exact took.
	s12, s12Tampered, s12WrongTarget, s12Restart, s12IMRestart *evidence.Scenario
	s12Expected                                                string
	// scenario names the scenario whose reads run now, for lostSessions, and
	// reads counts the csql reads each scenario made.
	scenario string
	reads    map[string]int
	// engineVersion is the engine version cubrid_rel reported on the master.
	engineVersion string
	// accessKey and secretKey are the seed Secret's values. They go to the
	// redactor and to the run's own object store client, nowhere else.
	accessKey, secretKey string
	// deniedKeys are the access key and the secret key S11
	// denied-credentials or S11 signature-mismatch made, once it did; the access
	// key of S11 signature-mismatch is accessKey. They go to the redactor, to the
	// run's own object store client and to the Secret of that variant's
	// cluster, nowhere else; holdsCredential looks for them too.
	deniedKeys []string
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
	h := &haRun{Runner: r, cfg: cfg, redactor: r.Redactor, scenario: haScenarioID}
	if h.redactor == nil {
		h.redactor = evidence.NewRedactor()
	}
	h.members = clusterMembers(haClusterName)
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
	lane, requirements := haLane, []evidence.Requirement{{ID: haScenarioID}}
	switch {
	case cfg.Fault == FaultOperatorRestart:
		lane = s06Lane
		requirements = append(requirements, evidence.Requirement{ID: s06ID, Variant: s06Variant})
		summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S06 restart: the " +
			"manager Pod is deleted without a grace period; no other S00-S15 qualification"
	case cfg.Restore:
		lane = s12Lane
		requirements = append(requirements, evidence.Requirement{ID: s11ID, Variant: s11Variant},
			evidence.Requirement{ID: s12ID, Variant: s12Variant})
		summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete and S12 " +
			"exact: a full backup to object storage, restored into a new cluster in the run namespace; no other " +
			"S00-S15 qualification"
		summary.Notes = append(summary.Notes, "The S11 artifact is left in the backup bucket; the restored "+
			"cluster's seed backups are left under the run's prefix")
		if cfg.RestoreTampered {
			lane = s12TamperedLane
			requirements = append(requirements, evidence.Requirement{ID: s12ID, Variant: s12TamperedVariant})
			summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete, S12 " +
				"exact and S12 tampered: a full backup to object storage, restored into a new cluster in the run " +
				"namespace, and a copy of it with one byte changed refused by another; no other S00-S15 " +
				"qualification"
		}
		if cfg.RestoreWrongTarget {
			lane = s12WrongTargetLane
			requirements = append(requirements, evidence.Requirement{ID: s12ID, Variant: s12WrongTargetVariant})
			summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete, S12 " +
				"exact and S12 wrong-target: a full backup to object storage, restored into a new cluster in the " +
				"run namespace, and refused by a cluster created over the retained volumes of another that hold a " +
				"database; no other S00-S15 qualification"
		}
		if cfg.RestoreOperatorRestart {
			lane = s12RestartLane
			requirements = append(requirements, evidence.Requirement{ID: s12ID, Variant: s12RestartVariant})
			summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete, S12 " +
				"exact and S12 operator-restart: a full backup to object storage, restored into a new cluster in " +
				"the run namespace, and into another while the manager Pod is deleted without a grace period " +
				"during its restore; no other S00-S15 qualification"
		}
		if cfg.RestoreInstanceManagerRestart {
			at := cfg.imRestartAt()
			lane = imRestartLane(at)
			requirements = append(requirements, evidence.Requirement{ID: s12ID, Variant: imRestartVariant(at)})
			summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete, S12 " +
				"exact and S12 " + imRestartVariant(at) + ": a full backup to object storage, restored into a new " +
				"cluster in the run namespace, and into another while " + imRestartPlacementNote[at] +
				"; no other S00-S15 qualification"
		}
	case cfg.BackupBucket != "":
		lane = s11Lane
		requirements = append(requirements, evidence.Requirement{ID: s11ID, Variant: s11Variant})
		summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete: a full " +
			"backup to object storage, downloaded and checked by the run; no other S00-S15 qualification"
		summary.Notes = append(summary.Notes, "The S11 artifact is left in the backup bucket for S12")
		lane, requirements = planS11MissingDestination(cfg, lane, requirements, &summary)
		lane, requirements = planS11DeniedCredentials(cfg, lane, requirements, &summary)
		lane, requirements = planS11SignatureMismatch(cfg, lane, requirements, &summary)
		lane, requirements = planS11InterruptedUpload(cfg, lane, requirements, &summary)
		lane, requirements = planS11OperatorRestart(cfg, lane, requirements, &summary)
		lane, requirements = planS11InstanceManagerRestart(cfg, lane, requirements, &summary)
		lane, requirements = planS11Overlapping(cfg, lane, requirements, &summary)
	}
	summary.Plan(requirements, "the HA run has not started")
	scenario := evidence.Scenario{ID: haScenarioID, Result: evidence.Blocked, NeverEvents: []string{}}
	var owned *corev1.Namespace
	defer func() {
		var cleanupErr error
		if owned != nil {
			cleanupErr = r.cleanupRun(cfg.Config, owned, nil)
			cleaned := cleanupErr == nil
			scenario.CleanupSucceeded = &cleaned
			h.rec.Cleanup = "complete"
			if cleanupErr != nil {
				h.rec.Cleanup = cleanupErr.Error()
				runErr = errors.Join(runErr, fmt.Errorf("scoped cleanup: %w", cleanupErr))
			}
		}
		recordErr := h.writeJSON(haRecordFile, h.rec)
		if recordErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("write HA evidence: %w", recordErr))
		}
		// The files of a scenario after LAB01 are its own.
		scenario.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, haScenarioID+"/")
		})
		if counts := h.rec.Operations; counts != nil {
			scenario.Operations = &evidence.Operations{Attempted: counts[workload.Attempted],
				Acknowledged: counts[workload.Acknowledged], Failed: counts[workload.Failed],
				Unknown: counts[workload.Unknown]}
		}
		scenario.Measurements = h.measurements()
		scenario.Measurements["lostSessions"] = h.lostIn(haScenarioID)
		if h.labPassed && cleanupErr == nil && recordErr == nil {
			scenario.Result = evidence.Pass
			scenario.Reason = "LAB01's starting state, a subset of S01's, held for the stable period, and the data " +
				"rules held on every member and both Services"
		} else {
			if owned != nil {
				scenario.Result = evidence.Fail
			}
			scenario.Reason = runErr.Error()
		}
		if n := len(h.rec.LostSessions); n > 0 {
			summary.Notes = append(summary.Notes, fmt.Sprintf("%d csql read(s) on a member lost their session "+
				"and were made once more (#238); %s lists them, and each scenario counts its own as lostSessions",
				n, haRecordFile))
		}
		summary.Record(scenario)
		h.recordAfterLAB01(&summary)
		summary.FinishedAt = time.Now().UTC()
		runErr = errors.Join(runErr, summary.Conclude(cfg.EvidenceDir, h.redactor,
			evidence.Lane{Name: lane, Required: requirements}))
		if runErr != nil {
			runErr = errors.New(string(h.redactor.Redact([]byte(runErr.Error()))))
		}
	}()
	checkpoint := func() error {
		if err := h.writeJSON(haRecordFile, h.rec); err != nil {
			return err
		}
		return summary.Checkpoint(cfg.EvidenceDir, h.redactor, lane, time.Now())
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
	ns, _, err := r.prepareNamespace(ctx, cfg.Config, &observed, cfg.privileged())
	if err != nil {
		return err
	}
	owned, h.rec.NamespaceUID = ns, observed.NamespaceUID
	h.rec.PodSecurity = podSecurityLabels(ns.Labels)
	h.guard = func(ctx context.Context) error { return r.requireNamespace(ctx, owned) }
	if err := checkpoint(); err != nil {
		return err
	}
	if err := h.run(ctx, &summary.Environment, checkpoint); err != nil {
		return err
	}
	h.labPassed = true
	return h.afterLAB01(ctx)
}

// recordAfterLAB01 records the scenarios selected to follow LAB01, and
// settles as blocked those that did not run.
func (h *haRun) recordAfterLAB01(summary *evidence.Summary) {
	if h.s06 != nil {
		summary.Record(*h.s06)
	} else if h.cfg.Fault == FaultOperatorRestart {
		summary.Settle(evidence.Requirement{ID: s06ID, Variant: s06Variant}, evidence.Blocked,
			"S06 starts from LAB01's checked state, and LAB01 did not pass")
	}
	if h.s11 != nil {
		summary.Record(*h.s11)
	} else if h.cfg.BackupBucket != "" {
		summary.Settle(evidence.Requirement{ID: s11ID, Variant: s11Variant}, evidence.Blocked,
			"S11 starts from LAB01's checked state, and LAB01 did not pass")
	}
	h.recordS11Variants(summary)
	switch {
	case h.s12 != nil:
		summary.Record(*h.s12)
	case h.cfg.Restore && h.s11 == nil:
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: s12Variant}, evidence.Blocked,
			"S12 restores S11's artifact, and LAB01 did not pass")
	case h.cfg.Restore:
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: s12Variant}, evidence.Blocked,
			"S12 restores S11's artifact, and S11 did not pass")
	}
	switch {
	case h.s12Tampered != nil:
		summary.Record(*h.s12Tampered)
	case h.cfg.RestoreTampered && h.s12 == nil:
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: s12TamperedVariant}, evidence.Blocked,
			"S12 tampered follows S12 exact, and S12 exact did not run")
	case h.cfg.RestoreTampered:
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: s12TamperedVariant}, evidence.Blocked,
			"S12 tampered follows S12 exact, and S12 exact did not pass")
	}
	switch {
	case h.s12WrongTarget != nil:
		summary.Record(*h.s12WrongTarget)
	case h.cfg.RestoreWrongTarget && h.s12 == nil:
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: s12WrongTargetVariant}, evidence.Blocked,
			"S12 wrong-target follows S12 exact, and S12 exact did not run")
	case h.cfg.RestoreWrongTarget:
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: s12WrongTargetVariant}, evidence.Blocked,
			"S12 wrong-target follows S12 exact, and S12 exact did not pass")
	}
	switch {
	case h.s12Restart != nil:
		summary.Record(*h.s12Restart)
	case h.cfg.RestoreOperatorRestart && h.s12 == nil:
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: s12RestartVariant}, evidence.Blocked,
			"S12 operator-restart follows S12 exact, and S12 exact did not run")
	case h.cfg.RestoreOperatorRestart:
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: s12RestartVariant}, evidence.Blocked,
			"S12 operator-restart follows S12 exact, and S12 exact did not pass")
	}
	switch {
	case h.s12IMRestart != nil:
		summary.Record(*h.s12IMRestart)
	case h.cfg.RestoreInstanceManagerRestart && h.s12 == nil:
		variant := imRestartVariant(h.cfg.imRestartAt())
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: variant}, evidence.Blocked,
			"S12 "+variant+" follows S12 exact, and S12 exact did not run")
	case h.cfg.RestoreInstanceManagerRestart:
		variant := imRestartVariant(h.cfg.imRestartAt())
		summary.Settle(evidence.Requirement{ID: s12ID, Variant: variant}, evidence.Blocked,
			"S12 "+variant+" follows S12 exact, and S12 exact did not pass")
	}
}

// recordS11Variants records the S11 variants selected to follow S11
// complete, and settles as blocked those that did not run.
func (h *haRun) recordS11Variants(summary *evidence.Summary) {
	for _, v := range []struct {
		selected bool
		result   *evidence.Scenario
		variant  string
	}{{h.cfg.BackupMissingDestination, h.s11Missing, s11MissingVariant},
		{h.cfg.BackupDeniedCredentials, h.s11Denied, s11DeniedVariant},
		{h.cfg.BackupSignatureMismatch, h.s11Signature, s11SignatureVariant},
		{h.cfg.BackupInterruptedUpload, h.s11Interrupted, s11InterruptedVariant},
		{h.cfg.BackupOperatorRestart, h.s11Restart, s11RestartVariant},
		{h.cfg.BackupInstanceManagerRestart, h.s11IMRestart, s11IMRestartVariant},
		{h.cfg.BackupOverlapping, h.s11Overlapping, s11OverlappingVariant}} {
		switch {
		case v.result != nil:
			summary.Record(*v.result)
		case v.selected && h.s11 == nil:
			summary.Settle(evidence.Requirement{ID: s11ID, Variant: v.variant}, evidence.Blocked,
				"S11 "+v.variant+" follows S11 complete, and S11 complete did not run")
		case v.selected:
			summary.Settle(evidence.Requirement{ID: s11ID, Variant: v.variant}, evidence.Blocked,
				"S11 "+v.variant+" follows S11 complete, and S11 complete did not pass")
		}
	}
}

// afterLAB01 runs the scenario selected to follow LAB01, if any.
func (h *haRun) afterLAB01(ctx context.Context) error {
	switch {
	case h.cfg.Fault == FaultOperatorRestart:
		s06 := h.runS06(ctx)
		h.s06 = &s06
		if s06.Result != evidence.Pass {
			return fmt.Errorf("S06/%s: %s", s06Variant, s06.Reason)
		}
	case h.cfg.BackupBucket != "":
		s11 := h.runS11(ctx)
		h.s11 = &s11
		if s11.Result != evidence.Pass {
			return fmt.Errorf("S11/%s: %s", s11Variant, s11.Reason)
		}
		if h.cfg.BackupMissingDestination {
			missing := h.runS11MissingDestination(ctx)
			h.s11Missing = &missing
			if missing.Result != evidence.Pass {
				return fmt.Errorf("S11/%s: %s", s11MissingVariant, missing.Reason)
			}
		}
		if h.cfg.BackupDeniedCredentials {
			denied := h.runS11DeniedCredentials(ctx)
			h.s11Denied = &denied
			if denied.Result != evidence.Pass {
				return fmt.Errorf("S11/%s: %s", s11DeniedVariant, denied.Reason)
			}
		}
		if h.cfg.BackupSignatureMismatch {
			wrong := h.runS11SignatureMismatch(ctx)
			h.s11Signature = &wrong
			if wrong.Result != evidence.Pass {
				return fmt.Errorf("S11/%s: %s", s11SignatureVariant, wrong.Reason)
			}
		}
		if h.cfg.BackupInterruptedUpload {
			interrupted := h.runS11InterruptedUpload(ctx)
			h.s11Interrupted = &interrupted
			if interrupted.Result != evidence.Pass {
				return fmt.Errorf("S11/%s: %s", s11InterruptedVariant, interrupted.Reason)
			}
		}
		if h.cfg.BackupOperatorRestart {
			restart := h.runS11OperatorRestart(ctx)
			h.s11Restart = &restart
			if restart.Result != evidence.Pass {
				return fmt.Errorf("S11/%s: %s", s11RestartVariant, restart.Reason)
			}
		}
		if h.cfg.BackupInstanceManagerRestart {
			restart := h.runS11InstanceManagerRestart(ctx)
			h.s11IMRestart = &restart
			if restart.Result != evidence.Pass {
				return fmt.Errorf("S11/%s: %s", s11IMRestartVariant, restart.Reason)
			}
		}
		if h.cfg.BackupOverlapping {
			overlapping := h.runS11Overlapping(ctx)
			h.s11Overlapping = &overlapping
			if overlapping.Result != evidence.Pass {
				return fmt.Errorf("S11/%s: %s", s11OverlappingVariant, overlapping.Reason)
			}
		}
		if !h.cfg.Restore {
			return nil
		}
		s12 := h.runS12(ctx)
		h.s12 = &s12
		if s12.Result != evidence.Pass {
			return fmt.Errorf("S12/%s: %s", s12Variant, s12.Reason)
		}
		if h.cfg.RestoreOperatorRestart {
			restart := h.runS12OperatorRestart(ctx)
			h.s12Restart = &restart
			if restart.Result != evidence.Pass {
				return fmt.Errorf("S12/%s: %s", s12RestartVariant, restart.Reason)
			}
		}
		if h.cfg.RestoreInstanceManagerRestart {
			restart := h.runS12InstanceManagerRestart(ctx)
			h.s12IMRestart = &restart
			if restart.Result != evidence.Pass {
				return fmt.Errorf("S12/%s: %s", restart.Variant, restart.Reason)
			}
		}
		if h.cfg.RestoreWrongTarget {
			wrong := h.runS12WrongTarget(ctx)
			h.s12WrongTarget = &wrong
			if wrong.Result != evidence.Pass {
				return fmt.Errorf("S12/%s: %s", s12WrongTargetVariant, wrong.Reason)
			}
		}
		if !h.cfg.RestoreTampered {
			return nil
		}
		tampered := h.runS12Tampered(ctx)
		h.s12Tampered = &tampered
		if tampered.Result != evidence.Pass {
			return fmt.Errorf("S12/%s: %s", s12TamperedVariant, tampered.Reason)
		}
	}
	return nil
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
	h.engineVersion = env.EngineVersion
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
	if err := h.checkData(ctx, answered, haDataCheckFile); err != nil {
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
	h.accessKey, h.secretKey = string(data[seedAccessKey]), string(data[seedSecretKey])
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
		Spec:       h.clusterSpec(h.rec.Seed.Prefix, seedSecretName)}
	if err := h.Client.Create(ctx, cluster); err != nil {
		return nil, fmt.Errorf("create the HA cluster: %w", err)
	}
	if cluster.UID == "" {
		return nil, errors.New("created HA cluster has no observed UID")
	}
	h.rec.ClusterUID = string(cluster.UID)
	return cluster, nil
}

// clusterSpec is the spec of a three-member HA cluster of the run, seeded
// through the seed store under prefix with secret, a copy of the seed Secret.
func (h *haRun) clusterSpec(prefix, secret string) databasev1alpha1.CubridClusterSpec {
	return databasev1alpha1.CubridClusterSpec{
		Version: haCubridVersion, Image: &databasev1alpha1.CubridImage{Repository: h.cfg.InstanceManagerImage},
		Databases:        []databasev1alpha1.CubridDatabase{{Name: databaseName}},
		Topology:         databasev1alpha1.CubridTopology{PromotableMembers: haMemberCount},
		HighAvailability: databasev1alpha1.CubridHighAvailability{Enabled: true},
		ObjectStorage: &databasev1alpha1.CubridObjectStorage{Endpoint: h.cfg.SeedEndpoint,
			Insecure: h.cfg.SeedInsecure, Bucket: h.cfg.SeedBucket, Prefix: prefix,
			CredentialsSecretRef: corev1.LocalObjectReference{Name: secret}},
		Storage: databasev1alpha1.CubridStorage{Data: databasev1alpha1.CubridStorageSpec{
			Size: resource.MustParse("2Gi"), StorageClassName: &h.cfg.StorageClass}}}
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

// unread joins what could not be read to its retryable API error in what
// does not hold: a reading that was not made, not a state that was seen.
const unread = " could not be read: "

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
			if c := meta.FindStatusCondition(cluster.Status.Conditions, bootstrapReadyCondition); c != nil {
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

// clusterMembers names the member Pods of a three-member cluster.
func clusterMembers(cluster string) []string {
	members := make([]string, 0, haMemberCount)
	for i := range haMemberCount {
		members = append(members, fmt.Sprintf("%s-%d", cluster, i))
	}
	return members
}

// roles asks CUBRID on every member of the HA cluster for its role.
func (h *haRun) roles(ctx context.Context) (*startingState, string, error) {
	return h.rolesOf(ctx, h.members)
}

// commandRan is how kubectl exec reports a command that ran in the container
// and exited non-zero; an error without it is kubectl's own, such as an API
// server or kubelet that could not be reached.
var commandRan = regexp.MustCompile(`(?m)^command terminated with exit code \d+\s*$`)

// rolesOf asks CUBRID on every member named for its role. One active master
// and standby slaves are a state; anything else is what does not hold, and an
// exec that did not reach a member is named with unread.
func (h *haRun) rolesOf(ctx context.Context, members []string) (*startingState, string, error) {
	state := &startingState{Slaves: []string{}}
	var masters []string
	for _, pod := range members {
		out, err := h.member(ctx, pod, "bash", "-c", heartbeatStatus)
		if err != nil {
			// The run's deadline is not a member's failure.
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			// Without kubectl's exit code of the command, the exec did not
			// reach the member: nothing was asked.
			if !commandRan.MatchString(out) {
				return nil, "cubrid heartbeat status on " + pod + unread + err.Error(), nil
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

// memberIdentities returns each member's Pod identity once every member of
// the cluster is Ready, owned through the cluster's StatefulSet. What does
// not hold names an object that could not be read with unread.
func (h *haRun) memberIdentities(ctx context.Context, cluster *databasev1alpha1.CubridCluster) (
	[]imageIdentity, string, error) {
	sts := &appsv1.StatefulSet{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "the StatefulSet is not found", nil
		}
		if retryable(err) {
			return nil, "the StatefulSet" + unread + err.Error(), nil
		}
		return nil, "", err
	}
	if !controllerOwner(sts, "CubridCluster", cluster.UID) {
		return nil, "", errors.New("the database StatefulSet does not belong to the HA cluster")
	}
	identities := make([]imageIdentity, 0, haMemberCount)
	for _, name := range clusterMembers(cluster.Name) {
		pod := &corev1.Pod{}
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, name + " is not found", nil
			}
			if retryable(err) {
				return nil, name + unread + err.Error(), nil
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

// brokersAvailable reports a Broker Deployment that is not fully available,
// or, with unread, one that could not be read.
func (h *haRun) brokersAvailable(ctx context.Context, cluster *databasev1alpha1.CubridCluster) (string, error) {
	for _, mode := range []string{"rw", "ro"} {
		deployment := &appsv1.Deployment{}
		name := cluster.Name + "-broker-" + mode
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, deployment); err != nil {
			if apierrors.IsNotFound(err) {
				return name + " is not found", nil
			}
			if retryable(err) {
				return name + unread + err.Error(), nil
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
	jdbc := func(mode string, port int) string { return h.jdbcURL(haClusterName, mode, port) }
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

// jdbcURL is the JDBC URL of a cluster's Service of the access mode.
func (h *haRun) jdbcURL(cluster, mode string, port int) string {
	return fmt.Sprintf("jdbc:cubrid:%s-%s.%s.svc:%d:%s:::?connectTimeout=10&queryTimeout=30",
		cluster, mode, h.cfg.Namespace, port, databaseName)
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

// dataTarget is a cluster whose rows a data check reads: its members, read
// directly, its Services, read through the client, and the members CUBRID
// reports as slaves.
type dataTarget struct {
	members []string
	dumps   map[string]string
	slaves  []string
}

// sourceTarget is the HA cluster LAB01 formed.
func (h *haRun) sourceTarget() dataTarget {
	return dataTarget{members: h.members, dumps: dumps, slaves: h.rec.StartingState.Slaves}
}

// lagging is a violation a slave that has not applied the last rows yet
// can cause: a row a slave, or the read-only Service in front of the slaves,
// lacks. Every other violation fails at once, a row the master or the
// read-write Service lacks among them.
func (t dataTarget) lagging(member string, v workload.Violation) bool {
	if v.Rule == workload.RuleMembersDiverge {
		return strings.HasPrefix(v.Detail, "present on ")
	}
	return v.Rule == workload.RuleMissing && (member == "ro" || slices.Contains(t.slaves, member))
}

// stopper returns the first violation that lag cannot explain, if any.
func (t dataTarget) stopper(r workload.Report) (string, workload.Violation, bool) {
	for _, name := range slices.Sorted(maps.Keys(r.Members)) {
		for _, v := range r.Members[name].Violations {
			if !t.lagging(name, v) {
				return name, v, true
			}
		}
	}
	for _, v := range r.Diverged {
		if !t.lagging("", v) {
			return "", v, true
		}
	}
	return "", workload.Violation{}, false
}

// dataViolation is a data check that read the rows and found the rules
// broken, as against one that could not read them.
type dataViolation struct{ error }

// checkData applies the data rules to the HA cluster against the run's whole
// history and records how long they took to hold.
func (h *haRun) checkData(ctx context.Context, answered time.Time, file string) error {
	_, held, err := h.checkDataOf(ctx, h.history.String(), h.sourceTarget(), answered, file)
	if err == nil {
		seconds := held.Seconds()
		h.rec.DataRulesHeldAfterSeconds = &seconds
	}
	return err
}

// checkDataOf applies the data rules of history to every member of target,
// read directly, and to its Services. While the only violations are rows a
// member lacks, they are read again until the rules hold or DataCheckWithin
// has passed since answered. The last report read is kept in file in any
// case and returned. A report that breaks the rules is a dataViolation, unless
// the run ended while only lag could explain it; held is the time from
// answered until the rules held.
func (h *haRun) checkDataOf(ctx context.Context, historyText string, target dataTarget, answered time.Time,
	file string) (*workload.Report, time.Duration, error) {
	history, err := workload.ReadHistory(strings.NewReader(historyText))
	if err != nil {
		return nil, 0, err
	}
	var report *workload.Report
	keep := func() error {
		if report == nil {
			return nil
		}
		return h.writeJSON(file, struct {
			Operations map[string]int `json:"operations"`
			workload.Report
		}{history.Counts, *report})
	}
	for {
		readCtx, cancel := context.WithTimeout(ctx, max(h.cfg.DataCheckWithin-time.Since(answered), 0)+readSlack)
		rows, readErr := h.readRowsOf(readCtx, target)
		cancel()
		if readErr == nil {
			checked := workload.Check(history, rows)
			report = &checked
			if checked.OK {
				return report, time.Since(answered), keep()
			}
		}
		passed := time.Since(answered) >= h.cfg.DataCheckWithin
		final := passed || ctx.Err() != nil
		var stopMember string
		var stopViolation workload.Violation
		stopped := false
		if readErr == nil {
			stopMember, stopViolation, stopped = target.stopper(*report)
			final = final || stopped
		}
		if final {
			if err := keep(); err != nil {
				return report, 0, err
			}
			if report == nil || (readErr != nil && ctx.Err() != nil) {
				return report, 0, errors.Join(fmt.Errorf("read the ledger rows: %w", readErr), ctx.Err())
			}
			member, v, total := report.FirstViolation(append(slices.Clone(target.members), "rw", "ro"), "")
			if stopped {
				member, v = stopMember, stopViolation
			}
			broken := errors.Join(fmt.Errorf("the data rules did not hold: %s on %s for %s: %s (%d violations in all)",
				v.Rule, cmp.Or(member, "the members"), v.OpID, v.Detail, total), readErr)
			// Only lag explains what was read when the run ended: not confirmed.
			if !stopped && (!passed || ctx.Err() != nil) {
				return report, 0, broken
			}
			return report, 0, dataViolation{broken}
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

// readRowsOf reads the ledger rows of every member of target, not through a
// Broker, and through its Services.
func (h *haRun) readRowsOf(ctx context.Context, target dataTarget) (map[string][]workload.Row, error) {
	rows := map[string][]workload.Row{}
	for _, pod := range target.members {
		out, err := sqlcheck.Run(h.csqlRead(ctx, pod), workload.LedgerQuery)
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
	for _, name := range slices.Sorted(maps.Keys(target.dumps)) {
		out, err := h.clientRun(ctx, target.dumps[name]+" 2>/tmp/dump.err")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if rows[name], err = workload.ReadRows(strings.NewReader(out)); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return rows, nil
}

// csqlRead runs a query with csql on a member, not through a Broker. It is for
// reads only: a query whose output holds both errors of a session the server
// dropped (#238, POC-36) is recorded and run once more, which a write must
// never be. The second answer is the read's, whatever it is.
func (h *haRun) csqlRead(ctx context.Context, pod string) sqlcheck.Runner {
	return func(sql string) (string, error) {
		run := func() (string, error) {
			return h.member(ctx, pod, "bash", "-c", `PATH="${CUBRID}/bin:${PATH}" csql -u dba "$0" -c "$1"`,
				databaseName+"@localhost", sql)
		}
		if h.reads == nil {
			h.reads = map[string]int{}
		}
		h.reads[h.scenario]++
		out, err := run()
		if err != nil || !sessionLost(out) {
			return out, err
		}
		lost := lostSession{At: time.Now().UTC(), Scenario: h.scenario, Pod: pod, Query: firstLine(sql),
			Errors: errorLines(out), OutputBytes: len(out)}
		again, err := run()
		switch {
		case err != nil:
			lost.Again = "the read failed: " + err.Error()
		case sessionLost(again):
			lost.Again = "the session was lost again"
		default:
			lost.Again = "answered"
		}
		h.rec.LostSessions = append(h.rec.LostSessions, lost)
		return again, err
	}
}

// lostSessionErrors are the two errors csql prints when the server dropped
// its session, before or after the result.
var lostSessionErrors = []*regexp.Regexp{
	regexp.MustCompile(`ERROR CODE = -199\b`),
	regexp.MustCompile(`ERROR CODE = -111\b`),
}

// sessionLost reports whether csql's output holds both errors of a dropped
// session.
func sessionLost(out string) bool {
	for _, re := range lostSessionErrors {
		if !re.MatchString(out) {
			return false
		}
	}
	return true
}

// errorLines are the lines of csql's output that name an error code, each with
// the message line after it.
func errorLines(out string) []string {
	lines := strings.Split(out, "\n")
	var found []string
	for i, line := range lines {
		if strings.Contains(line, "ERROR CODE = ") {
			if i+1 < len(lines) {
				line += " | " + strings.TrimSpace(lines[i+1])
			}
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

// firstLine is the first line of s.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// inScenario names the scenario whose reads follow, for the lost sessions it
// records, and returns a function that sets the scenario's lostSessions
// measurement and names LAB01 again.
func (h *haRun) inScenario(name string, result *evidence.Scenario) func() {
	h.scenario = name
	return func() {
		if result.Measurements == nil {
			result.Measurements = map[string]any{}
		}
		result.Measurements["lostSessions"] = h.lostIn(name)
		h.scenario = haScenarioID
	}
}

// lostIn counts the reads of a scenario that lost their session, unknown
// when the scenario made no csql read.
func (h *haRun) lostIn(name string) any {
	if h.reads[name] == 0 {
		return evidence.Unknown
	}
	n := 0
	for _, lost := range h.rec.LostSessions {
		if lost.Scenario == name {
			n++
		}
	}
	return n
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
