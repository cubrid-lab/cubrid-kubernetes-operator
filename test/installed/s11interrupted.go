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
	"maps"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

const (
	s11InterruptedLane                = "lab-installed-ha-s11-interrupted-upload"
	s11InterruptedVariant             = "interrupted-upload"
	s11InterruptedRecordFile          = "S11/interrupted-upload/record.json"
	s11InterruptedSourceDataCheckFile = "S11/interrupted-upload/source-data-check.json"
	s11InterruptedBackupName          = "s11-interrupted-upload"
	// s11InterruptedClusterName is the standalone cluster whose upload is
	// cut; s11InterruptedMember is its one member's Pod.
	s11InterruptedClusterName = "s11-interrupted"
	s11InterruptedMember      = s11InterruptedClusterName + "-0"
	// interruptedRate is the rate, in bits per second, the member's traffic
	// to the object store is limited to. S11 complete's backup on the VM lab
	// was one object of about 5 MB, which takes close to three minutes at
	// this rate: far longer than the few readings and commands between
	// seeing the upload under way and the cut, and far shorter than the
	// default backup_limit of 15 minutes.
	interruptedRate = 256000
	// interruptedUploadSeen is how many bytes must have passed the limit
	// since the request, with the backup neither Completed nor Failed,
	// before the upload is cut: an upload under way, not a request of a few
	// hundred bytes.
	interruptedUploadSeen = 512 << 10
	// interruptedPVCResidue names what the run leaves of the standalone
	// cluster: its data PVC is retained when the cluster is deleted.
	interruptedPVCResidue = "; the data PVC of " + s11InterruptedClusterName + " is retained when the cluster is " +
		"deleted and goes with the namespace"
	// interruptedContainerResidue names the ephemeral container the faults
	// add to the member's Pod; it cannot be removed and goes with the Pod.
	interruptedContainerResidue = "; the ephemeral container " + netfault.Container + " stays in " +
		s11InterruptedMember + " until the Pod is deleted"
)

// failedWithinKey is the measurement of the time to Failed.
const failedWithinKey = "failedWithin"

// How the store's address was found (s11InterruptedFaults.AddressMode).
const (
	addressClusterIP = "cluster-ip"
	addressHeadless  = "headless-endpoint"
)

// interruptedExecTolerance is how many errors in a row of a fault's command,
// or of a reading of the store's address, the wait for the backup tolerates;
// the next ends it. A Pod that changed is never tolerated.
const interruptedExecTolerance = 3

// interruptedCutConfirmLimit bounds how long after its installation the cut
// may take to show a rejected packet while the backup has not ended.
var interruptedCutConfirmLimit = 2 * time.Minute

// storedObject is an object under a prefix, with its size.
type storedObject struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// sizedLister lists objects with their sizes, and the multipart uploads
// under a prefix that were started and neither completed nor aborted. The
// run's minio-go client does; a store that does not leaves both unlisted.
type sizedLister interface {
	ListSized(ctx context.Context, bucket, prefix string) ([]storedObject, error)
	ListIncomplete(ctx context.Context, bucket, prefix string) ([]string, error)
}

// s11InterruptedFaults is what the run did to the member's traffic and what
// it saw of it. Every instant is UTC.
type s11InterruptedFaults struct {
	// Target is the member's Pod the faults act in, with its UID and IP.
	Target    string `json:"target"`
	TargetUID string `json:"targetUID,omitempty"`
	TargetIP  string `json:"targetIP,omitempty"`
	// Store is -object-store-service, and StoreIP and StorePort the
	// destination the member's own network namespace sends the upload to.
	// AddressMode says how it was found: addressClusterIP, the Service's
	// ClusterIP and port, which the node translates only after the packets
	// leave the Pod; or addressHeadless, the one ready endpoint of a headless
	// Service and its port, which the member dials directly. StorePod names
	// that endpoint's Pod when its EndpointSlice does.
	Store       string `json:"store"`
	AddressMode string `json:"addressMode,omitempty"`
	StoreIP     string `json:"storeIP,omitempty"`
	StorePort   int    `json:"storePort,omitempty"`
	StorePod    string `json:"storePod,omitempty"`
	// StoreCheckedBeforeCut and StoreCheckedAtEnd are when the address was
	// read again before the cut and at the end, and found unchanged.
	StoreCheckedBeforeCut *time.Time `json:"storeCheckedBeforeCut,omitempty"`
	StoreCheckedAtEnd     *time.Time `json:"storeCheckedAtEnd,omitempty"`
	// RateBitsPerSecond is the limit's rate; UploadSeenBytes how many bytes
	// must have passed it before the cut.
	RateBitsPerSecond int64  `json:"rateBitsPerSecond"`
	UploadSeenBytes   uint64 `json:"uploadSeenBytes"`
	// LimitedAt is when the limit was installed, LimitConfirmedAt when
	// connects to the store were seen to pass through it.
	LimitedAt        *time.Time `json:"limitedAt,omitempty"`
	LimitConfirmedAt *time.Time `json:"limitConfirmedAt,omitempty"`
	// AtRequest, FirstGrowth and AtCut are the limit's counters when the
	// backup was requested, at the first reading after it that saw bytes
	// pass, and at the reading that decided the cut; Last is the last
	// reading. SentBeforeCut is what passed from the request to the cut's
	// reading, and ObservedRate the rate from FirstGrowth to AtCut.
	AtRequest      *netfault.BandCounters `json:"atRequest,omitempty"`
	FirstGrowth    *netfault.BandCounters `json:"firstGrowth,omitempty"`
	AtCut          *netfault.BandCounters `json:"atCut,omitempty"`
	Last           *netfault.BandCounters `json:"last,omitempty"`
	SentBeforeCut  uint64                 `json:"sentBeforeCut"`
	ObservedRate   string                 `json:"observedRate,omitempty"`
	PhaseAtCut     string                 `json:"phaseAtCut,omitempty"`
	CutIssuedAt    *time.Time             `json:"cutIssuedAt,omitempty"`
	CutAt          *time.Time             `json:"cutAt,omitempty"`
	CutConfirmedAt *time.Time             `json:"cutConfirmedAt,omitempty"`
	// Rejected is the reset rule's counter when it confirmed the cut, on a
	// reading at which the backup had not ended. RejectedAtEnd is the counter
	// read at the reading that first saw the backup ended, when the cut was
	// not confirmed before it: evidence, not a confirmation.
	Rejected      *netfault.RejectCounters `json:"rejected,omitempty"`
	RejectedAtEnd *netfault.RejectCounters `json:"rejectedAtEnd,omitempty"`
	// ExecErrors are the errors of a fault's command, or of a reading of the
	// store's address, that the wait tolerated, each with its UTC time.
	ExecErrors []string `json:"execErrors"`
	// Waiting is what the cut waited for at the last reading before it.
	Waiting string `json:"waiting,omitempty"`
	// RemovedAt is when both faults were seen removed; Removal what became
	// of each.
	RemovedAt *time.Time `json:"removedAt,omitempty"`
	Removal   string     `json:"removal"`
}

// s11ArtifactReading is a status.artifact read on a backup that was not
// Completed.
type s11ArtifactReading struct {
	At    time.Time `json:"at"`
	Phase string    `json:"phase"`
	URI   string    `json:"uri"`
}

// s11InterruptedRecord is the evidence of S11 interrupted-upload besides the
// source's data check.
type s11InterruptedRecord struct {
	Backup             string `json:"backup"`
	BackupUID          string `json:"backupUID,omitempty"`
	Bucket             string `json:"bucket"`
	Prefix             string `json:"prefix"`
	ObjectStoreService string `json:"objectStoreService"`
	// PodSecurity are the run namespace's Pod Security labels as the run
	// read them before anything was created.
	PodSecurity map[string]string     `json:"podSecurity"`
	Cluster     *s11StandaloneCluster `json:"cluster,omitempty"`
	// ReadyLimit is BackupInterruptedReadyLimit, the run's bound on the
	// cluster.
	ReadyLimit  string                `json:"readyLimit"`
	Faults      *s11InterruptedFaults `json:"faults"`
	RequestedAt *time.Time            `json:"requestedAt,omitempty"`
	Phases      []s11Phase            `json:"phases"`
	// LastPhase is the phase of the last reading the wait took, and
	// EndedAfterCut is set when the wait saw the backup ended at a reading
	// after the one that confirmed the cut.
	LastPhase     string `json:"lastPhase,omitempty"`
	EndedAfterCut bool   `json:"endedAfterCut"`
	// FailedWithin runs from the return of the request to the reading that
	// saw Failed, and FailedAfterCut from the cut's installation to it, upper
	// bounds; StatusFailedWithin from the backup's creationTimestamp to its
	// status.completedAt.
	FailedWithin       string `json:"failedWithin,omitempty"`
	FailedAfterCut     string `json:"failedAfterCut,omitempty"`
	StatusFailedWithin string `json:"statusFailedWithin,omitempty"`
	// Failure is the Ready reason and message of the backup as it ended;
	// StoreError the error the upload failed with, after "put
	// <bucket>/<key>: " when it names an object, and StoreErrorKind what it
	// names.
	Failure        string `json:"failure,omitempty"`
	StoreError     string `json:"storeError,omitempty"`
	StoreErrorKind string `json:"storeErrorKind,omitempty"`
	// CompletedBeforeCut is set when the backup ended Completed before the
	// cut rejected a packet: a complete backup, not a partial one.
	CompletedBeforeCut bool `json:"completedBeforeCut"`
	// PhaseAtEnd and ArtifactAtEnd are the backup as read once more after
	// the faults were removed and the prefix was listed.
	PhaseAtEnd       string                                 `json:"phaseAtEnd,omitempty"`
	ArtifactAtEnd    *databasev1alpha1.CubridBackupArtifact `json:"artifactAtEnd,omitempty"`
	Artifact         *databasev1alpha1.CubridBackupArtifact `json:"artifact,omitempty"`
	ArtifactsNotDone []s11ArtifactReading                   `json:"artifactsNotCompleted"`
	// ObjectsAtEnd is what the run's client listed under the prefix at the
	// end, with sizes; ManifestAtEnd is set when a manifest.json was among
	// them, and PartialArtifact when objects were and no manifest.json.
	// IncompleteUploads are the multipart uploads it listed there, when
	// IncompleteListed.
	CheckedAtEnd      *time.Time     `json:"checkedAtEnd,omitempty"`
	ObjectsAtEnd      []storedObject `json:"objectsAtEnd"`
	ManifestAtEnd     bool           `json:"manifestAtEnd"`
	PartialArtifact   bool           `json:"partialArtifact"`
	IncompleteListed  bool           `json:"incompleteUploadsListed"`
	IncompleteUploads []string       `json:"incompleteUploads"`
	// ManagerBefore is the installed manager as LAB01 read it at its end,
	// ManagerAfter as this variant read it at its end.
	ManagerBefore *managerIdentity `json:"managerBefore,omitempty"`
	ManagerAfter  *managerIdentity `json:"managerAfter,omitempty"`
	Source        *s12Source       `json:"source,omitempty"`
	Credentials   *s11Credentials  `json:"credentials,omitempty"`
	JudgedAt      *time.Time       `json:"judgedAt,omitempty"`
	Notes         []string         `json:"notes,omitempty"`
	// Cleanup says whether the faults were removed and the CubridBackup and
	// the cluster the run created were deleted; Residue is what the run
	// leaves.
	Cleanup string `json:"cleanup"`
	Residue string `json:"residue"`
}

// s11InterruptedMade is what S11 interrupted-upload created and installed,
// for its cleanup.
type s11InterruptedMade struct {
	cluster  *databasev1alpha1.CubridCluster
	backup   *databasev1alpha1.CubridBackup
	throttle *netfault.Throttle
	reset    *netfault.Reset
	// removed is set once both faults were seen removed, or none was
	// installed; tried once their removal ran.
	removed, tried bool
	// errorsInARow counts the tolerated errors since the last success of a
	// fault's command or a reading of the store's address.
	errorsInARow int
}

// faultStep is an error of a fault's step during the wait for the backup: it
// kept the run from cutting the upload, not from reading the backup.
type faultStep struct{ error }

// validateS11Interrupted checks the inputs of S11 interrupted-upload, when
// it is selected; BackupBucket is checked by validateS11.
func (c HAConfig) validateS11Interrupted() error {
	if !c.BackupInterruptedUpload {
		return nil
	}
	for _, other := range []struct {
		selected bool
		flag     string
	}{{c.Restore, "-restore"}, {c.BackupMissingDestination, "-backup-missing-destination"},
		{c.BackupDeniedCredentials, "-backup-denied-credentials"},
		{c.BackupSignatureMismatch, "-backup-signature-mismatch"}} {
		if other.selected {
			return fmt.Errorf("-backup-interrupted-upload and %s select different lanes; a run takes one of them",
				other.flag)
		}
	}
	if c.BackupInterruptedReadyLimit <= 0 {
		return errors.New("-backup-interrupted-upload needs a positive -backup-interrupted-ready-limit for its " +
			"cluster")
	}
	return nil
}

// planS11InterruptedUpload returns the lane and the requirements of a run
// that selects S11 interrupted-upload after S11 complete, and names it and
// the run namespace's Pod Security in the summary's notes; otherwise lane
// and requirements as they are.
func planS11InterruptedUpload(cfg HAConfig, lane string, requirements []evidence.Requirement,
	summary *evidence.Summary) (string, []evidence.Requirement) {
	if !cfg.BackupInterruptedUpload {
		return lane, requirements
	}
	summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete and S11 " +
		"interrupted-upload: a full backup to object storage, downloaded and checked by the run, and a backup of " +
		"a standalone cluster whose upload is slowed and then reset by network faults in its member's Pod, which " +
		"must end Failed; no other S00-S15 qualification"
	summary.Notes = append(summary.Notes, "The run namespace enforces Pod Security privileged, with warn and "+
		"audit at restricted, so that the network faults' ephemeral container is admitted")
	return s11InterruptedLane, append(requirements, evidence.Requirement{ID: s11ID, Variant: s11InterruptedVariant})
}

// runS11InterruptedUpload runs S11 interrupted-upload after S11 complete
// passed. The run creates a standalone cluster with the run's own object
// store Secret, waits until it is Ready, limits the rate of its member's TCP
// traffic to the object store's Service and requests a full backup of its
// database to BackupBucket. Once the limit has let interruptedUploadSeen
// bytes through since the request and the backup has not ended, it resets
// that traffic in the member's Pod and holds both faults until the backup
// ends. The backup must end Failed for its upload within BackupLimit, never
// Completed and without status.artifact; no manifest.json may be under its
// prefix at the end, the source must be unchanged, and neither seed key in
// a status read, an Event or the logs checkS11Logs searches. The run removes both faults, records what is
// left under the prefix as a partial artifact, never writes to or removes
// from the object store, and deletes the backup and the cluster it created.
// A backup that ends before the cut took effect, a fault that cannot be
// installed or confirmed, a run that ends, or a check that cannot be made
// blocks it; what was seen to break stays a failure.
func (h *haRun) runS11InterruptedUpload(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s11ID+"/"+s11InterruptedVariant, &result)()
	result = evidence.Scenario{ID: s11ID, Variant: s11InterruptedVariant, Result: evidence.Fail,
		NeverEvents: []string{}}
	rec := s11InterruptedRecord{Backup: s11InterruptedBackupName, Bucket: h.cfg.BackupBucket,
		Prefix: h.rec.Seed.Prefix + "/interrupted-upload", ObjectStoreService: h.cfg.ObjectStoreService,
		PodSecurity: map[string]string{},
		Cluster:     &s11StandaloneCluster{Name: s11InterruptedClusterName, Secret: seedSecretName},
		ReadyLimit:  h.cfg.BackupInterruptedReadyLimit.String(), Phases: []s11Phase{},
		ArtifactsNotDone: []s11ArtifactReading{}, ObjectsAtEnd: []storedObject{}, IncompleteUploads: []string{},
		Faults: &s11InterruptedFaults{Target: s11InterruptedMember, Store: h.cfg.ObjectStoreService,
			RateBitsPerSecond: interruptedRate, UploadSeenBytes: interruptedUploadSeen, ExecErrors: []string{},
			Removal: "nothing to remove: no fault was installed"},
		Credentials: &s11Credentials{FoundIn: []string{}},
		Notes: []string{"the faults act in the network namespace of the standalone member's Pod, from an " +
			"ephemeral container with NET_ADMIN and NET_RAW (test/faults/netfault); the run namespace enforces " +
			"Pod Security privileged for it, with warn and audit at restricted",
			"the member reaches the object store at the cluster's spec.objectStorage endpoint, which names " +
				"-object-store-service; the limit and the cut match the address the member's own network namespace " +
				"sends to: a Service's ClusterIP and port, which the node translates only afterwards, or, for a " +
				"headless Service, its one ready endpoint, which the member dials directly; the run assumes, as S11 " +
				"complete does, that the endpoint resolves to that Service",
			"the cut is a TCP reset made by the member's own kernel: the object store never saw the connection " +
				"close, and how the store and the Instance Manager behave when the traffic is silently dropped is " +
				"not covered",
			"both faults are held until the backup ended, so a request the Instance Manager's client sends again " +
				"is refused as well",
			"the run never writes to or removes from the object store; with the seed Secret's keys it lists the " +
				"backup bucket under the prefix at the end",
			logsNote(h.logLimit())},
		Cleanup: "nothing to delete: the run created no CubridCluster or CubridBackup",
		Residue: "nothing: the run wrote nothing to the object store" + stagingNotChecked}
	var made s11InterruptedMade
	defer func() {
		removed := h.removeInterruptedFaults(ctx, &made, rec.Faults)
		deleted := h.deleteInterrupted(ctx, made, &rec)
		cleaned := removed && deleted
		result.CleanupSucceeded = &cleaned
		if made.throttle != nil {
			maps.Copy(result.Measurements, prefixed("rateLimit.", made.throttle.Measurements()))
			maps.Copy(result.Measurements, prefixed("cut.", made.reset.Measurements()))
		}
		// A pass without its evidence is a failure; a blocked or failed S11
		// interrupted-upload keeps its result, with the evidence that could
		// not be written.
		if err := h.writeJSON(s11InterruptedRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S11 interrupted-upload evidence: "+
				err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S11/"+s11InterruptedVariant+"/")
		})
	}()
	result.Limits = map[string]string{backupLimitName: h.cfg.BackupLimit.String()}
	result.Measurements = map[string]any{failedWithinKey: evidence.Unknown, "failedAfterCut": evidence.Unknown,
		"uploadBytesBeforeCut": evidence.Unknown}
	err := h.interruptedSteps(ctx, &result, &rec, &made)
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	never := strings.Join(result.NeverEvents, "; ")
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
	case errors.As(err, new(backupVerdict)) || errors.As(err, new(restoreVerdict)) ||
		errors.As(err, new(dataViolation)):
		result.Result, result.Reason = evidence.Fail, reason
	case never != "":
		result.Result, result.Reason = evidence.Fail, reason
	case err != nil && ctx.Err() != nil:
		result.Result, result.Reason = evidence.Blocked, "the run ended before S11 interrupted-upload was judged: "+
			reason
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, reason
	default:
		result.Result = evidence.Pass
		failure := "with an error that names a reset or refused connection (storeErrorKind " + storeErrorReset + ")"
		if rec.StoreErrorKind != storeErrorReset {
			failure = "with another error, recorded as storeErrorKind " + rec.StoreErrorKind
		}
		result.Reason = "the upload of " + s11InterruptedClusterName + "'s backup to " + rec.Bucket + " was seen " +
			"under way through the rate limit and then reset in its member's Pod, confirmed by the reset rule's " +
			"counter before the backup ended; the backup ended Failed within backup_limit for its upload, " +
			failure + ", never Completed and without status.artifact, also when read again at the end; no " +
			"manifest.json was under its prefix at the end; both faults were seen removed; the store's address " +
			"was unchanged before the cut and at the end; the source was unchanged; no seed key was in the " +
			"statuses read, the Events, the installed Operator's manager log, read from " +
			logSinceMargin.String() + " before the variant started, or the whole Instance Manager's log in the " +
			"member's Pod"
	}
	judged := time.Now().UTC()
	rec.JudgedAt = &judged
	return result
}

// prefixed returns m with prefix before each key.
func prefixed(prefix string, m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[prefix+k] = v
	}
	return out
}

// interruptedSteps runs the steps of S11 interrupted-upload in order. Before
// the request, the first error ends them. After it, the backup is judged,
// the faults are removed, and the checks at the end are all made; their
// errors are joined.
func (h *haRun) interruptedSteps(ctx context.Context, result *evidence.Scenario, rec *s11InterruptedRecord,
	made *s11InterruptedMade) error {
	started := time.Now().UTC()
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S11 interrupted-upload could not tell whether a credential is in a status or an "+
			"Event", err)
	}
	before := h.rec.OperatorAfter
	rec.ManagerBefore = &before
	if err := h.interruptedNamespace(ctx, rec); err != nil {
		return err
	}
	if err := h.interruptedStore(ctx, rec.Faults); err != nil {
		return err
	}
	if err := h.createInterrupted(ctx, rec.Cluster, made); err != nil {
		return err
	}
	if err := h.waitStandaloneReady(ctx, made.cluster, rec.Cluster, rec.Credentials,
		h.cfg.BackupInterruptedReadyLimit); err != nil {
		return err
	}
	if err := h.interruptedTarget(ctx, made.cluster, rec.Faults); err != nil {
		return err
	}
	env := netfault.Env{Client: h.Client, Kubectl: h.Command, Poll: h.cfg.Poll,
		Reserve: min(netfault.DefaultReserve, h.cfg.CleanupTimeout/4)}
	target := netfault.Endpoint{Name: rec.Faults.Target, Namespace: h.cfg.Namespace, Pod: rec.Faults.Target,
		UID: rec.Faults.TargetUID, IP: rec.Faults.TargetIP}
	store := netfault.Endpoint{Name: rec.Faults.Store, IP: rec.Faults.StoreIP, Port: rec.Faults.StorePort}
	made.throttle = &netfault.Throttle{Env: env, Target: target, Peer: store, Rate: interruptedRate}
	made.reset = &netfault.Reset{Env: env, Target: target, Peer: store}
	err := h.interruptedUpload(ctx, result, rec, made)
	if rec.RequestedAt == nil {
		return err
	}
	errs := []error{err}
	// The faults go before the prefix is listed: what is there at the end
	// is what the store holds with nothing in the way. A fault not seen
	// removed blocks the variant: the member's traffic may still be cut.
	if !h.removeInterruptedFaults(ctx, made, rec.Faults) {
		errs = append(errs, fmt.Errorf("the faults were not seen removed: %s", rec.Faults.Removal))
	}
	if err := h.recheckStore(ctx, rec.Faults); err != nil {
		errs = append(errs, fmt.Errorf("at the end: %w", err))
	} else {
		at := time.Now().UTC()
		rec.Faults.StoreCheckedAtEnd = &at
	}
	errs = append(errs, h.interruptedAtEnd(ctx, rec, result))
	errs = append(errs, h.interruptedBackupAtEnd(ctx, made.backup, rec, result))
	if err := h.checkS11Credentials(ctx, made.backup, nil, rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: %w", err))
	}
	if err := h.standaloneAtEnd(ctx, made.cluster, rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: %w", err))
	}
	if err := h.checkS11Logs(ctx, s11InterruptedClusterName, true, lastTarget(rec.Phases), *rec.RequestedAt, started,
		rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("search the logs: %w", err))
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	errs = append(errs, h.checkSourceS12(ctx, rec.Source, s11InterruptedSourceDataCheckFile))
	errs = append(errs, h.sameManager(ctx, "S11 interrupted-upload", rec.ManagerBefore, &rec.ManagerAfter))
	return errors.Join(errs...)
}

// interruptedNamespace reads the run namespace's Pod Security labels and
// refuses the variant, before anything is created, unless it enforces
// privileged: the faults' ephemeral container would not be admitted.
func (h *haRun) interruptedNamespace(ctx context.Context, rec *s11InterruptedRecord) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	ns := &corev1.Namespace{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: h.cfg.Namespace}, ns); err != nil {
		return fmt.Errorf("read the run namespace: %w", err)
	}
	rec.PodSecurity = podSecurityLabels(ns.Labels)
	if level := ns.Labels[podSecurityEnforce]; level != podSecurityPrivileged {
		return fmt.Errorf("the run namespace %s enforces Pod Security %q, not %s: the network faults' ephemeral "+
			"container would not be admitted, and S11 interrupted-upload creates nothing", h.cfg.Namespace, level,
			podSecurityPrivileged)
	}
	return nil
}

// storeAddress is where the member's own network namespace sends the
// upload: see s11InterruptedFaults.AddressMode.
type storeAddress struct {
	mode, ip, pod string
	port          int
}

func (a storeAddress) String() string {
	return fmt.Sprintf("%s:%d (%s)", a.ip, a.port, a.mode)
}

// interruptedStore resolves -object-store-service to the address the
// member's own network namespace sends the upload to (resolveStore), and
// records it. It refuses a Service that the cluster's endpoint does not
// name.
func (h *haRun) interruptedStore(ctx context.Context, f *s11InterruptedFaults) error {
	namespace, name, port, err := splitService(h.cfg.ObjectStoreService)
	if err != nil {
		return err
	}
	if err := h.sameObjectStore(ctx, namespace, name, port); err != nil {
		return err
	}
	addr, err := h.resolveStore(ctx)
	if err != nil {
		return err
	}
	f.AddressMode, f.StoreIP, f.StorePort, f.StorePod = addr.mode, addr.ip, addr.port, addr.pod
	return nil
}

// resolveStore reads -object-store-service. A Service with an IPv4
// ClusterIP gives that ClusterIP and the Service's TCP port: the node
// translates it only after the packets leave the Pod. A headless Service,
// whose name resolves to its endpoints' Pod IPs, gives its one ready IPv4
// endpoint address and the endpoint port of the Service's TCP port, read
// from its EndpointSlices: the member dials that address directly. A
// Service without that TCP port, another ClusterIP, and a headless Service
// with no or several ready addresses are refused.
func (h *haRun) resolveStore(ctx context.Context) (storeAddress, error) {
	namespace, name, port, err := splitService(h.cfg.ObjectStoreService)
	if err != nil {
		return storeAddress{}, err
	}
	number, _ := strconv.Atoi(port)
	svc := &corev1.Service{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, svc); err != nil {
		return storeAddress{}, fmt.Errorf("read the object store's Service %s/%s: %w", namespace, name, err)
	}
	i := slices.IndexFunc(svc.Spec.Ports, func(p corev1.ServicePort) bool {
		return int(p.Port) == number && (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP)
	})
	if i < 0 {
		return storeAddress{}, fmt.Errorf("the object store's Service %s/%s has no TCP port %d", namespace, name,
			number)
	}
	switch ip := svc.Spec.ClusterIP; {
	case ip == corev1.ClusterIPNone:
		return h.headlessEndpoint(ctx, namespace, name, svc.Spec.Ports[i])
	case net.ParseIP(ip).To4() != nil:
		return storeAddress{mode: addressClusterIP, ip: ip, port: number}, nil
	}
	return storeAddress{}, fmt.Errorf("the object store's Service %s/%s has ClusterIP %q, neither an IPv4 address "+
		"nor None", namespace, name, svc.Spec.ClusterIP)
}

// headlessEndpoint returns the one ready IPv4 address of the headless
// Service's EndpointSlices that has the endpoint port of servicePort over
// TCP. An endpoint whose ready condition is not set counts as ready, as the
// EndpointSlice API says.
func (h *haRun) headlessEndpoint(ctx context.Context, namespace, name string, servicePort corev1.ServicePort) (
	storeAddress, error) {
	list := &discoveryv1.EndpointSliceList{}
	if err := h.Client.List(ctx, list, client.InNamespace(namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: name}); err != nil {
		return storeAddress{}, fmt.Errorf("list the EndpointSlices of the object store's Service %s/%s: %w",
			namespace, name, err)
	}
	var found []storeAddress
	for _, slice := range list.Items {
		if slice.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		port := 0
		for _, p := range slice.Ports {
			if p.Port != nil && ptrValue(p.Name) == servicePort.Name &&
				(p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP) {
				port = int(*p.Port)
			}
		}
		if port == 0 {
			continue
		}
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			pod := ""
			if ep.TargetRef != nil && ep.TargetRef.Kind == "Pod" {
				pod = ep.TargetRef.Name
			}
			for _, ip := range ep.Addresses {
				addr := storeAddress{mode: addressHeadless, ip: ip, port: port, pod: pod}
				if !slices.ContainsFunc(found, func(a storeAddress) bool { return a.ip == ip && a.port == port }) {
					found = append(found, addr)
				}
			}
		}
	}
	if len(found) != 1 {
		return storeAddress{}, fmt.Errorf("the headless object store Service %s/%s has %d ready IPv4 endpoint "+
			"addresses with the TCP port of its port %d, not one: %v; the faults need one address to act on",
			namespace, name, len(found), servicePort.Port, found)
	}
	// Nothing translates the address of a headless Service: the member
	// dials the Service's port at the Pod's IP, so a target port that
	// differs would leave the faults on a port nothing is sent to.
	if found[0].port != int(servicePort.Port) {
		return storeAddress{}, fmt.Errorf("the headless object store Service %s/%s maps its port %d to endpoint "+
			"port %d; the member dials %d at the Pod's IP, so the faults need the two to be the same",
			namespace, name, servicePort.Port, found[0].port, servicePort.Port)
	}
	return found[0], nil
}

func ptrValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// recheckStore resolves the store's address again and fails when it is not
// the one the faults act on, such as a store Pod that moved.
func (h *haRun) recheckStore(ctx context.Context, f *s11InterruptedFaults) error {
	addr, err := h.resolveStore(ctx)
	if err != nil {
		return fmt.Errorf("read the object store's address again: %w", err)
	}
	if addr.mode != f.AddressMode || addr.ip != f.StoreIP || addr.port != f.StorePort {
		return storeMoved{fmt.Errorf("the object store's address is now %s, not %s:%d (%s), the address the "+
			"faults act on", addr, f.StoreIP, f.StorePort, f.AddressMode)}
	}
	return nil
}

// storeMoved is an error of recheckStore that saw another address.
type storeMoved struct{ error }

// createInterrupted creates the standalone cluster in the run namespace
// (standaloneSpec) with the run's copy of the seed Secret.
func (h *haRun) createInterrupted(ctx context.Context, rec *s11StandaloneCluster, made *s11InterruptedMade) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	cluster := &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: s11InterruptedClusterName, Namespace: h.cfg.Namespace},
		Spec:       h.standaloneSpec(seedSecretName)}
	if err := h.Client.Create(ctx, cluster); err != nil {
		return fmt.Errorf("create the cluster %s: %w", s11InterruptedClusterName, err)
	}
	created := time.Now().UTC()
	made.cluster, rec.UID, rec.CreatedAt = cluster, string(cluster.UID), &created
	if cluster.UID == "" {
		return fmt.Errorf("created cluster %s has no observed UID", s11InterruptedClusterName)
	}
	return nil
}

// interruptedTarget reads the member's Pod and records its UID and IP. It
// refuses a Pod that the cluster's StatefulSet does not own, or that has no
// IP yet.
func (h *haRun) interruptedTarget(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	f *s11InterruptedFaults) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: s11InterruptedMember},
		pod); err != nil {
		return fmt.Errorf("read the member Pod %s: %w", s11InterruptedMember, err)
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "StatefulSet" || owner.Name != cluster.Name {
		return fmt.Errorf("the Pod %s is not controlled by the StatefulSet of %s", s11InterruptedMember,
			cluster.Name)
	}
	if pod.Status.PodIP == "" {
		return fmt.Errorf("the Pod %s has no IP", s11InterruptedMember)
	}
	f.TargetUID, f.TargetIP = string(pod.UID), pod.Status.PodIP
	return nil
}

// interruptedUpload installs and confirms the rate limit, prepares the cut,
// requests the backup and waits for it, cutting the upload once it is seen
// under way, and judges the backup.
func (h *haRun) interruptedUpload(ctx context.Context, result *evidence.Scenario, rec *s11InterruptedRecord,
	made *s11InterruptedMade) error {
	f := rec.Faults
	if err := made.throttle.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare the rate limit: %w", err)
	}
	if err := made.reset.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare the cut: %w", err)
	}
	if err := made.throttle.Inject(ctx); err != nil {
		return fmt.Errorf("install the rate limit: %w", err)
	}
	limited := time.Now().UTC()
	f.LimitedAt = &limited
	switch ok, err := made.throttle.Confirm(ctx); {
	case err != nil:
		return fmt.Errorf("the rate limit was not confirmed: %w", err)
	case !ok:
		return errors.New("the rate limit was not confirmed")
	}
	confirmed := time.Now().UTC()
	f.LimitConfirmedAt = &confirmed
	atRequest, err := made.throttle.Counters(ctx)
	if err != nil {
		return fmt.Errorf("read the rate limit's counters: %w", err)
	}
	f.AtRequest = &atRequest
	backup, err := h.createBackup(ctx, s11InterruptedBackupName, s11InterruptedClusterName, rec.Bucket, rec.Prefix)
	if err != nil {
		return err
	}
	requested := time.Now().UTC()
	made.backup = backup
	rec.BackupUID, rec.RequestedAt = string(backup.UID), &requested
	backup, err = h.waitBackupWith(ctx, backup, &rec.Phases, rec.Credentials,
		func(wctx context.Context, read *databasev1alpha1.CubridBackup) error {
			return h.interruptedReading(wctx, read, rec, made)
		})
	return h.judgeInterrupted(backup, err, requested, rec, result)
}

// interruptedReading acts on one status read of the backup while the run
// waits for it. An artifact on a backup that is not Completed is recorded.
// Before the cut, a backup that is Running or Uploading has the limit's
// counters read, and once interruptedUploadSeen bytes passed since the
// request the cut is installed. After it, the reset rule's counter is read
// until it confirms the cut; a cut not confirmed within
// interruptedCutConfirmLimit, while the backup has not ended, ends the wait.
func (h *haRun) interruptedReading(ctx context.Context, backup *databasev1alpha1.CubridBackup,
	rec *s11InterruptedRecord, made *s11InterruptedMade) error {
	s, f := backup.Status, rec.Faults
	rec.LastPhase = string(s.Phase)
	ended := s.Phase == databasev1alpha1.BackupPhaseCompleted || s.Phase == databasev1alpha1.BackupPhaseFailed
	if s.Artifact != nil && s.Phase != databasev1alpha1.BackupPhaseCompleted {
		rec.ArtifactsNotDone = append(rec.ArtifactsNotDone, s11ArtifactReading{At: time.Now().UTC(),
			Phase: string(s.Phase), URI: s.Artifact.URI})
	}
	if s.TargetInstance != "" && s.TargetInstance != f.Target {
		return faultStep{fmt.Errorf("the backup's target is %s, not %s, the Pod the faults act in",
			s.TargetInstance, f.Target)}
	}
	if f.CutAt == nil {
		if ended || (s.Phase != databasev1alpha1.BackupPhaseRunning && s.Phase != databasev1alpha1.BackupPhaseUploading) {
			return nil
		}
		counters, err := made.throttle.Counters(ctx)
		if err != nil {
			return made.tolerate(f, "read the rate limit's counters", err)
		}
		f.Last = &counters
		sent := counters.Bytes - min(counters.Bytes, f.AtRequest.Bytes)
		if f.FirstGrowth == nil && sent > 0 {
			f.FirstGrowth = &counters
		}
		if sent < interruptedUploadSeen {
			made.errorsInARow = 0
			f.Waiting = fmt.Sprintf("%d bytes through the rate limit since the request, in phase %s", sent, s.Phase)
			return nil
		}
		// The store's address is read again: a store Pod that moved would
		// leave the cut on an address nothing is sent to.
		if err := h.recheckStore(ctx, f); err != nil {
			if errors.As(err, new(storeMoved)) {
				return faultStep{fmt.Errorf("before the cut: %w", err)}
			}
			return made.tolerate(f, "read the object store's address before the cut", err)
		}
		// The count is reset only here: a counters read that succeeds
		// must not hide an address read that keeps failing.
		made.errorsInARow = 0
		checked := time.Now().UTC()
		f.StoreCheckedBeforeCut = &checked
		f.AtCut, f.SentBeforeCut, f.PhaseAtCut, f.Waiting = &counters, sent, string(s.Phase), ""
		if first := f.FirstGrowth; first != nil && counters.At.After(first.At) {
			rate := float64(counters.Bytes-first.Bytes) / counters.At.Sub(first.At).Seconds()
			f.ObservedRate = strconv.FormatFloat(rate, 'f', 0, 64) + " bytes/s"
		}
		issued := time.Now().UTC()
		f.CutIssuedAt = &issued
		if err := made.reset.Inject(ctx); err != nil {
			return faultStep{fmt.Errorf("cut the upload: %w", err)}
		}
		installed := time.Now().UTC()
		f.CutAt = &installed
	}
	if f.CutConfirmedAt != nil {
		return nil
	}
	ok, err := made.reset.Confirm(ctx)
	if err != nil {
		return made.tolerate(f, "confirm the cut", err)
	}
	made.errorsInARow = 0
	if ended {
		// A packet rejected by now may have been sent after the upload
		// ended: the reading that first sees the backup ended confirms
		// nothing, and its counter is kept as evidence only.
		if rejected, read := made.reset.Rejected(); read && ok {
			f.RejectedAtEnd = &rejected
		}
		return nil
	}
	if ok {
		at := time.Now().UTC()
		f.CutConfirmedAt = &at
		if rejected, read := made.reset.Rejected(); read {
			f.Rejected = &rejected
		}
		return nil
	}
	if !ended && time.Since(*f.CutAt) > interruptedCutConfirmLimit {
		return faultStep{fmt.Errorf("the cut was not confirmed within %s of its installation: its rule rejected "+
			"no packet to the object store", interruptedCutConfirmLimit)}
	}
	return nil
}

// tolerate records err of a fault's step and lets the wait go on while no
// more than interruptedExecTolerance errors came in a row; the next, and
// any error that says the target Pod changed, ends the wait.
func (made *s11InterruptedMade) tolerate(f *s11InterruptedFaults, step string, err error) error {
	if errors.As(err, new(*netfault.PodChangedError)) {
		return faultStep{fmt.Errorf("%s: %w", step, err)}
	}
	made.errorsInARow++
	f.ExecErrors = append(f.ExecErrors, time.Now().UTC().Format(time.RFC3339Nano)+" "+step+": "+err.Error())
	if made.errorsInARow > interruptedExecTolerance {
		return faultStep{fmt.Errorf("%s: %d errors in a row, the last: %w", step, made.errorsInARow, err)}
	}
	return nil
}

// judgeInterrupted judges the backup as the wait left it. The Instance
// Manager uploads each object before the manifest (UploadBackup in
// internal/instancemanager/upload.go); a reset connection fails the object's
// Put, which MinioObjectStore.Put reports as "put <bucket>/<key>:", and
// runBackup (internal/instancemanager/server.go) ends the operation Failed
// with "upload failed: " and that error, without a manifest. The Operator
// maps it onto the backup in failedBackup: phase Failed, Ready reason
// BackupFailed. That is what is required once the cut was confirmed. The
// cut is confirmed only at a reading at which the backup had not ended, so
// a backup seen ended with the cut confirmed ended after it. Completed after
// the cut was confirmed, or a status.artifact on a backup that is not
// Completed, must never happen. A backup that ended before the cut was
// confirmed did not show what an interrupted upload does, and blocks S11
// interrupted-upload; so does an error of a fault's step. A failure after
// the cut that is not for the upload, or a backup neither Completed nor
// Failed within BackupLimit, fails it.
func (h *haRun) judgeInterrupted(backup *databasev1alpha1.CubridBackup, waitErr error, requested time.Time,
	rec *s11InterruptedRecord, result *evidence.Scenario) error {
	s, f := backup.Status, rec.Faults
	rec.Artifact = s.Artifact
	if s.Phase == databasev1alpha1.BackupPhaseCompleted || s.Phase == databasev1alpha1.BackupPhaseFailed {
		rec.Failure = readyReason(backup)
		rec.EndedAfterCut = waitErr == nil && f.CutConfirmedAt != nil
	}
	var events []string
	if s.Phase == databasev1alpha1.BackupPhaseCompleted && f.CutConfirmedAt != nil {
		events = append(events, "the backup of "+s11InterruptedClusterName+" ended Completed after its upload was "+
			"cut")
		if s.Artifact != nil {
			events = append(events, "status.artifact names "+s.Artifact.URI+" for a backup of "+
				s11InterruptedClusterName+" whose upload was cut")
		}
	}
	for _, a := range rec.ArtifactsNotDone {
		event := "status.artifact names " + a.URI + " for a backup of " + s11InterruptedClusterName + " in phase " +
			a.Phase
		if !slices.Contains(events, event) {
			events = append(events, event)
		}
	}
	if len(events) > 0 {
		result.NeverEvents = append(result.NeverEvents, events...)
		errs := make([]error, 0, len(events))
		for _, event := range events {
			errs = append(errs, backupVerdict{errors.New(event)})
		}
		return errors.Join(errs...)
	}
	if waitErr != nil {
		if errors.As(waitErr, new(backupVerdict)) || errors.As(waitErr, new(faultStep)) {
			return waitErr
		}
		return fmt.Errorf("read the CubridBackup: %w", waitErr)
	}
	switch {
	case s.Phase == databasev1alpha1.BackupPhaseCompleted:
		rec.CompletedBeforeCut = true
		state := "before the cut was installed"
		if f.CutAt != nil {
			state = "before the cut was confirmed at an earlier reading"
		}
		return fmt.Errorf("the backup ended Completed %s, with %d bytes through the rate limit at the last "+
			"reading: the upload was not interrupted, S11 interrupted-upload is inconclusive, and the artifact is "+
			"a complete backup, not a partial one", state, f.lastSent())
	case f.CutAt == nil:
		return fmt.Errorf("the backup ended Failed before its upload was seen under way (%s): %s",
			cmp.Or(f.Waiting, "no reading of the rate limit was taken"), readyReason(backup))
	case f.CutConfirmedAt == nil:
		seen := "its rule rejected no packet before"
		if f.RejectedAtEnd != nil {
			seen = "its rule showed rejected packets only at the reading that saw the backup end, not before"
		}
		return fmt.Errorf("the backup ended Failed after the cut was installed, but %s: the cut was not "+
			"confirmed: %s", seen, readyReason(backup))
	}
	c := meta.FindStatusCondition(s.Conditions, backupReady)
	if c == nil || c.Reason != s11MissingReason || !strings.HasPrefix(c.Message, uploadFailed) {
		return backupVerdict{fmt.Errorf("the backup ended Failed after its upload was cut, but not for its upload "+
			"(%s %q): %s", s11MissingReason, uploadFailed, readyReason(backup))}
	}
	rec.StoreError = strings.TrimPrefix(c.Message, uploadFailed)
	if rest, ok := strings.CutPrefix(rec.StoreError, "put "+rec.Bucket+"/"); ok {
		if _, answer, found := strings.Cut(rest, ": "); found {
			rec.StoreError = answer
		}
	}
	rec.StoreErrorKind = storeErrorKind(rec.StoreError)
	failed := rec.Phases[len(rec.Phases)-1].At
	rec.FailedWithin = failed.Sub(requested).Round(time.Millisecond).String()
	rec.FailedAfterCut = failed.Sub(*f.CutAt).Round(time.Millisecond).String()
	result.Measurements[failedWithinKey] = rec.FailedWithin
	result.Measurements["failedAfterCut"] = rec.FailedAfterCut
	result.Measurements["uploadBytesBeforeCut"] = f.SentBeforeCut
	if done := s.CompletedAt; done != nil {
		rec.StatusFailedWithin = done.Sub(backup.CreationTimestamp.Time).String()
	}
	return nil
}

// lastSent is how many bytes passed the limit from the request to its last
// reading.
func (f *s11InterruptedFaults) lastSent() uint64 {
	if f.AtRequest == nil || f.Last == nil {
		return 0
	}
	return f.Last.Bytes - min(f.Last.Bytes, f.AtRequest.Bytes)
}

// removeInterruptedFaults removes the cut, then the rate limit, once, with a
// budget of its own so that a run that ended still removes them, and
// records what became of each. It reports whether both were seen removed;
// a fault that was never installed needs no removal.
func (h *haRun) removeInterruptedFaults(ctx context.Context, made *s11InterruptedMade,
	f *s11InterruptedFaults) bool {
	if made.tried || made.throttle == nil {
		return made.removed || made.throttle == nil
	}
	made.tried = true
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.CleanupTimeout)
	defer cancel()
	var outcomes []string
	made.removed = true
	for _, fault := range []struct {
		name   string
		remove func(context.Context) error
	}{{"the cut", made.reset.Remove}, {"the rate limit", made.throttle.Remove}} {
		if err := fault.remove(rctx); err != nil {
			made.removed = false
			outcomes = append(outcomes, fault.name+" was not seen removed: "+err.Error())
			continue
		}
		outcomes = append(outcomes, fault.name+" is not installed or was seen removed")
	}
	f.Removal = strings.Join(outcomes, "; ")
	if made.removed {
		at := time.Now().UTC()
		f.RemovedAt = &at
	}
	return made.removed
}

// interruptedAtEnd lists the backup bucket under the prefix with the seed
// Secret's keys, with sizes and incomplete multipart uploads when the
// client can list them. A manifest.json there must never happen, unless the
// backup ended Completed before the cut; other objects are a partial
// artifact, recorded and not failed. A store that cannot be asked blocks
// S11 interrupted-upload.
func (h *haRun) interruptedAtEnd(ctx context.Context, rec *s11InterruptedRecord, result *evidence.Scenario) error {
	store, stop, err := h.openS11Store(ctx)
	if err != nil {
		return err
	}
	defer stop()
	prefix := rec.Prefix + "/"
	var objects []storedObject
	sized, canSize := store.(sizedLister)
	if canSize {
		objects, err = sized.ListSized(ctx, rec.Bucket, prefix)
	} else {
		var keys []string
		keys, err = store.List(ctx, rec.Bucket, prefix)
		for _, k := range keys {
			objects = append(objects, storedObject{Key: k, Size: -1})
		}
	}
	if err != nil {
		return fmt.Errorf("list bucket %s under %s at the end: %w", rec.Bucket, rec.Prefix, err)
	}
	at := time.Now().UTC()
	rec.CheckedAtEnd = &at
	var errs []error
	if canSize {
		uploads, err := sized.ListIncomplete(ctx, rec.Bucket, prefix)
		if err != nil {
			errs = append(errs, fmt.Errorf("list the incomplete multipart uploads under %s at the end: %w",
				rec.Prefix, err))
		} else {
			rec.IncompleteListed, rec.IncompleteUploads = true, append(rec.IncompleteUploads, uploads...)
		}
	}
	rec.ObjectsAtEnd = append(rec.ObjectsAtEnd, objects...)
	rec.ManifestAtEnd = slices.ContainsFunc(objects, func(o storedObject) bool {
		return path.Base(o.Key) == instancemanager.ManifestObjectName
	})
	rec.PartialArtifact = len(objects) > 0 && !rec.ManifestAtEnd
	if len(objects) > 0 || len(rec.IncompleteUploads) > 0 {
		what := "a partial artifact"
		if rec.ManifestAtEnd {
			what = "an artifact with a manifest.json"
		}
		rec.Residue = fmt.Sprintf("%s under %s/%s at the end: %d objects and %d incomplete multipart uploads; "+
			"the run did not write them and does not remove them", what, rec.Bucket, rec.Prefix, len(objects),
			len(rec.IncompleteUploads)) + stagingNotChecked
	}
	switch {
	case !rec.ManifestAtEnd || rec.CompletedBeforeCut:
	case rec.EndedAfterCut:
		event := fmt.Sprintf("bucket %s holds a manifest.json under %s, the prefix of a backup that ended after "+
			"its upload was cut", rec.Bucket, rec.Prefix)
		result.NeverEvents = append(result.NeverEvents, event)
		errs = append(errs, backupVerdict{errors.New(event)})
	default:
		// The wait did not see the backup end after a confirmed cut: the
		// upload may have gone on once the faults were removed.
		errs = append(errs, fmt.Errorf("bucket %s holds a manifest.json under %s, but the wait did not see the "+
			"backup end after a confirmed cut (its last phase %q): recorded, and S11 interrupted-upload cannot be "+
			"judged", rec.Bucket, rec.Prefix, cmp.Or(rec.LastPhase, "not read")))
	}
	return errors.Join(errs...)
}

// interruptedBackupAtEnd reads the backup once more, after the faults were
// removed and the prefix was listed, so that a backup driven on after the
// wait is not missed. Completed or a status.artifact then must never
// happen when the wait saw it end after a confirmed cut; otherwise it is
// recorded and blocks S11 interrupted-upload, unless the backup completed
// before the cut, which the judgement already blocks.
func (h *haRun) interruptedBackupAtEnd(ctx context.Context, created *databasev1alpha1.CubridBackup,
	rec *s11InterruptedRecord, result *evidence.Scenario) error {
	backup := &databasev1alpha1.CubridBackup{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), backup); err != nil {
		return fmt.Errorf("read the CubridBackup at the end: %w", err)
	}
	if backup.UID != created.UID {
		return fmt.Errorf("the CubridBackup %s was replaced", created.Name)
	}
	s := backup.Status
	rec.PhaseAtEnd, rec.ArtifactAtEnd = string(s.Phase), s.Artifact
	completed := s.Phase == databasev1alpha1.BackupPhaseCompleted
	if rec.CompletedBeforeCut || (!completed && s.Artifact == nil) {
		return nil
	}
	if !rec.EndedAfterCut {
		return fmt.Errorf("the CubridBackup read at the end is %s with status.artifact %t, and the wait did not see "+
			"it end after a confirmed cut: recorded, and S11 interrupted-upload cannot be judged", s.Phase,
			s.Artifact != nil)
	}
	if rec.LastPhase == string(databasev1alpha1.BackupPhaseCompleted) {
		// The judgement named it already.
		return nil
	}
	var events []string
	if completed {
		events = append(events, "the backup of "+s11InterruptedClusterName+", seen "+rec.LastPhase+" after its "+
			"upload was cut, is Completed when read again at the end")
	}
	if s.Artifact != nil {
		events = append(events, "status.artifact names "+s.Artifact.URI+" for the backup of "+
			s11InterruptedClusterName+" read again at the end, after its upload was cut")
	}
	result.NeverEvents = append(result.NeverEvents, events...)
	errs := make([]error, 0, len(events))
	for _, event := range events {
		errs = append(errs, backupVerdict{errors.New(event)})
	}
	return errors.Join(errs...)
}

// deleteInterrupted deletes the CubridBackup and the cluster the run
// created, in that order and each by its UID (deleteByUID), and records each
// outcome after the faults' removal. The cluster's data PVC is retained and
// goes with the namespace. It does not judge S11 interrupted-upload.
func (h *haRun) deleteInterrupted(ctx context.Context, made s11InterruptedMade, rec *s11InterruptedRecord) bool {
	var objects []ownedObject
	if made.backup != nil {
		objects = append(objects, ownedObject{"CubridBackup", made.backup})
	}
	if made.cluster != nil && made.cluster.UID != "" {
		objects = append(objects, ownedObject{"cluster", made.cluster})
		residue := interruptedPVCResidue
		if made.throttle != nil {
			residue += interruptedContainerResidue
		}
		rec.Residue = strings.TrimSuffix(rec.Residue, stagingNotChecked) + residue + stagingNotChecked
	}
	removal := "faults: " + rec.Faults.Removal
	if len(objects) == 0 {
		if made.throttle != nil {
			rec.Cleanup = removal
		}
		return true
	}
	outcome, cleaned := h.deleteByUID(ctx, objects)
	rec.Cleanup = outcome
	if made.throttle != nil {
		rec.Cleanup = removal + "; " + outcome
	}
	return cleaned
}
