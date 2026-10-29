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
	"crypto/rand"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	s11DeniedLane                = "lab-installed-ha-s11-denied-credentials"
	s11DeniedVariant             = "denied-credentials"
	s11DeniedRecordFile          = "S11/denied-credentials/record.json"
	s11DeniedSourceDataCheckFile = "S11/denied-credentials/source-data-check.json"
	s11DeniedBackupName          = "s11-denied-credentials"
	// s11DeniedClusterName is the standalone cluster whose spec.objectStorage
	// names s11DeniedSecretName, the Secret with the denied keys.
	s11DeniedClusterName = "s11-denied"
	s11DeniedSecretName  = "object-storage-denied"
	// clusterReadyCondition is the condition a standalone cluster reports
	// True once its member is ready (setReady in
	// internal/controller/cubridcluster_controller.go).
	clusterReadyCondition = "Ready"
	// deniedPVCResidue names what the run leaves of the standalone cluster:
	// its data PVC is retained when the cluster is deleted
	// (pvcRetentionPolicy).
	deniedPVCResidue = "; the data PVC of " + s11DeniedClusterName + " is retained when the cluster is deleted " +
		"and goes with the namespace"
)

// s11DeniedFault is what the run's own client was answered when it asked the
// object store with the denied keys, before anything was created. Refused is
// set only for an answer that refuses access (storeErrorAccessDenied).
type s11DeniedFault struct {
	CheckedAt  *time.Time `json:"checkedAt,omitempty"`
	Refused    bool       `json:"refused"`
	Answer     string     `json:"answer,omitempty"`
	AnswerKind string     `json:"answerKind,omitempty"`
}

// s11DeniedCluster is the standalone cluster the backup is taken of, and the
// Secret its spec.objectStorage names. ReadyWithin runs from the return of
// its creation to the reading that saw it Ready; Waiting is the last of its
// status that did not hold while the run waited.
type s11DeniedCluster struct {
	Name        string     `json:"name"`
	UID         string     `json:"uid,omitempty"`
	Secret      string     `json:"secret"`
	SecretUID   string     `json:"secretUID,omitempty"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	ReadyWithin string     `json:"readyWithin,omitempty"`
	Waiting     string     `json:"waiting,omitempty"`
}

// s11DeniedRecord is the evidence of S11 denied-credentials besides the
// source's data check.
type s11DeniedRecord struct {
	Backup             string            `json:"backup"`
	BackupUID          string            `json:"backupUID,omitempty"`
	Bucket             string            `json:"bucket"`
	Prefix             string            `json:"prefix"`
	ObjectStoreService string            `json:"objectStoreService"`
	Fault              *s11DeniedFault   `json:"fault,omitempty"`
	Cluster            *s11DeniedCluster `json:"cluster,omitempty"`
	// ReadyLimit is BackupDeniedReadyLimit, the run's bound on the cluster.
	ReadyLimit  string     `json:"readyLimit"`
	RequestedAt *time.Time `json:"requestedAt,omitempty"`
	Phases      []s11Phase `json:"phases"`
	// FailedWithin runs from the return of the request to the reading that
	// saw Failed, an upper bound; StatusFailedWithin from the backup's
	// creationTimestamp to its status.completedAt, as the API server and the
	// Operator recorded them.
	FailedWithin       string `json:"failedWithin,omitempty"`
	StatusFailedWithin string `json:"statusFailedWithin,omitempty"`
	// Failure is the Ready reason and message of the backup as it ended.
	// StoreError is the object store's answer as the failure quotes it,
	// after "put <bucket>/<key>: ", and StoreErrorKind what it names. A pass
	// needs StoreErrorKind storeErrorAccessDenied.
	Failure        string                                 `json:"failure,omitempty"`
	StoreError     string                                 `json:"storeError,omitempty"`
	StoreErrorKind string                                 `json:"storeErrorKind,omitempty"`
	Artifact       *databasev1alpha1.CubridBackupArtifact `json:"artifact,omitempty"`
	// ObjectsAtEnd is what the run's client, with the seed Secret's keys,
	// listed under the prefix at the end; ManifestAtEnd is set when a
	// manifest.json was among them.
	CheckedAtEnd  *time.Time `json:"checkedAtEnd,omitempty"`
	ObjectsAtEnd  []string   `json:"objectsAtEnd"`
	ManifestAtEnd bool       `json:"manifestAtEnd"`
	// ManagerBefore is the installed manager as LAB01 read it at its end,
	// ManagerAfter as this variant read it at its end.
	ManagerBefore *managerIdentity `json:"managerBefore,omitempty"`
	ManagerAfter  *managerIdentity `json:"managerAfter,omitempty"`
	Source        *s12Source       `json:"source,omitempty"`
	Credentials   *s11Credentials  `json:"credentials,omitempty"`
	// JudgedAt is when the run reached its verdict.
	JudgedAt *time.Time `json:"judgedAt,omitempty"`
	Notes    []string   `json:"notes,omitempty"`
	// Cleanup says whether the CubridBackup, the cluster and the Secret the
	// run created were deleted; Residue is what the run leaves.
	Cleanup string `json:"cleanup"`
	Residue string `json:"residue"`
}

// s11DeniedMade is what S11 denied-credentials created, for its cleanup.
type s11DeniedMade struct {
	secret  *corev1.Secret
	cluster *databasev1alpha1.CubridCluster
	backup  *databasev1alpha1.CubridBackup
}

// planS11DeniedCredentials returns the lane and the requirements of a run
// that selects S11 denied-credentials after S11 complete, and names it in
// the summary's first note; otherwise lane and requirements as they are.
func planS11DeniedCredentials(cfg HAConfig, lane string, requirements []evidence.Requirement,
	summary *evidence.Summary) (string, []evidence.Requirement) {
	if !cfg.BackupDeniedCredentials {
		return lane, requirements
	}
	summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete and S11 " +
		"denied-credentials: a full backup to object storage, downloaded and checked by the run, and a backup of " +
		"a standalone cluster whose object store keys the store refuses, which must end Failed; no other " +
		"S00-S15 qualification"
	return s11DeniedLane, append(requirements, evidence.Requirement{ID: s11ID, Variant: s11DeniedVariant})
}

// runS11DeniedCredentials runs S11 denied-credentials after S11 complete
// passed. The Instance Manager reads the object store's keys only from its
// environment, which the cluster's spec.objectStorage Secret sets, so the
// variant has a cluster of its own: the run generates an access key and a
// secret key, confirms with its own client that the object store refuses
// them, creates a Secret with them and a standalone cluster that names it,
// waits until the cluster is Ready, and requests a full backup of its
// database to BackupBucket. The backup must end Failed within BackupLimit for
// its upload, never Completed and without status.artifact; nothing may be
// under its prefix at the end, the source unchanged, and neither the seed
// Secret's keys nor the denied keys in a status read or an Event. The run
// never writes to or removes from the object store; it deletes the backup,
// the cluster and the Secret it created. A run that ends, or a check that
// cannot be made, before S11 denied-credentials is judged blocks it; what
// was seen to break stays a failure.
func (h *haRun) runS11DeniedCredentials(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s11ID+"/"+s11DeniedVariant, &result)()
	result = evidence.Scenario{ID: s11ID, Variant: s11DeniedVariant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s11DeniedRecord{Backup: s11DeniedBackupName, Bucket: h.cfg.BackupBucket,
		Prefix: h.rec.Seed.Prefix + "/denied-credentials", ObjectStoreService: h.cfg.ObjectStoreService,
		Cluster:    &s11DeniedCluster{Name: s11DeniedClusterName, Secret: s11DeniedSecretName},
		ReadyLimit: h.cfg.BackupDeniedReadyLimit.String(), Phases: []s11Phase{}, ObjectsAtEnd: []string{},
		Credentials: &s11Credentials{FoundIn: []string{}},
		Notes: []string{"the denied keys are random, generated by the run and never recorded; with them the run " +
			"only lists the backup bucket under the prefix, and with the seed Secret's keys it lists the same at " +
			"the end; it never writes to or removes from the object store",
			"the run did not read the standalone member's environment: that its Instance Manager holds the denied " +
				"keys follows from the cluster's spec.objectStorage Secret, which the Instance Manager reads at " +
				"start only",
			"a pass needs the object store's answer quoted in the failure (storeError) to refuse the keys " +
				"(storeErrorKind access-denied); another answer blocks the variant",
			"the fault check reaches the store through -object-store-service, while the member uploads to the " +
				"cluster's spec.objectStorage endpoint; the run assumes, as S11 complete does, that they are the " +
				"same store, and refuses a Service that the endpoint does not name",
			"the Operator's and the Instance Manager's logs were not checked"},
		Cleanup: "nothing to delete: the run created no Secret, CubridCluster or CubridBackup",
		Residue: "nothing: the run wrote nothing to the object store" + stagingNotChecked}
	var made s11DeniedMade
	defer func() {
		cleaned := h.deleteDenied(ctx, made, &rec)
		result.CleanupSucceeded = &cleaned
		// A pass without its evidence is a failure; a blocked or failed S11
		// denied-credentials keeps its result, with the evidence that could
		// not be written.
		if err := h.writeJSON(s11DeniedRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S11 denied-credentials evidence: "+
				err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S11/"+s11DeniedVariant+"/")
		})
	}()
	result.Limits = map[string]string{backupLimitName: h.cfg.BackupLimit.String()}
	result.Measurements = map[string]any{"failedWithin": evidence.Unknown}
	err := h.deniedSteps(ctx, &result, &rec, &made)
	// A credential found in any status read or Event is a failure, whichever
	// step ended S11 denied-credentials.
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	never := strings.Join(result.NeverEvents, "; ")
	// The reason names each never-event once, then what else went wrong.
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
		// What was not checked keeps a confirmed failure a failure.
		result.Result, result.Reason = evidence.Fail, reason
	case err != nil && ctx.Err() != nil:
		result.Result, result.Reason = evidence.Blocked, "the run ended before S11 denied-credentials was judged: "+
			reason
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, reason
	default:
		result.Result = evidence.Pass
		result.Reason = "the object store refused the denied keys before the request; the backup of " +
			s11DeniedClusterName + ", whose spec.objectStorage Secret holds them, ended Failed within backup_limit " +
			"for its upload to " + rec.Bucket + " with an answer that refuses the keys, never Completed and " +
			"without status.artifact; nothing was under " +
			"its prefix at the end; the source was unchanged; neither the seed keys nor the denied keys were in " +
			"the statuses read or the Events; the Operator's and the Instance Manager's logs were not checked"
	}
	judged := time.Now().UTC()
	rec.JudgedAt = &judged
	return result
}

// deniedSteps runs the steps of S11 denied-credentials in order. Before the
// request, the first error ends them. After it, the backup is judged and the
// checks at the end are all made; their errors are joined.
func (h *haRun) deniedSteps(ctx context.Context, result *evidence.Scenario, rec *s11DeniedRecord,
	made *s11DeniedMade) error {
	h.denyKeys()
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S11 denied-credentials could not tell whether a credential is in a status or an "+
			"Event", err)
	}
	before := h.rec.OperatorAfter
	rec.ManagerBefore = &before
	if err := h.deniedFault(ctx, rec); err != nil {
		return err
	}
	if err := h.createDenied(ctx, rec.Cluster, made); err != nil {
		return err
	}
	if err := h.waitDeniedReady(ctx, made.cluster, rec); err != nil {
		return err
	}
	backup, err := h.createBackup(ctx, s11DeniedBackupName, s11DeniedClusterName, rec.Bucket, rec.Prefix)
	if err != nil {
		return err
	}
	requested := time.Now().UTC()
	made.backup = backup
	rec.BackupUID, rec.RequestedAt = string(backup.UID), &requested
	backup, err = h.waitBackup(ctx, backup, &rec.Phases, rec.Credentials)
	errs := []error{h.judgeDenied(backup, err, requested, rec, result)}
	errs = append(errs, h.deniedAtEnd(ctx, rec))
	if len(rec.ObjectsAtEnd) > 0 {
		rec.Residue = fmt.Sprintf("%d objects under %s/%s at the end; the run did not write them and does not "+
			"remove them", len(rec.ObjectsAtEnd), rec.Bucket, rec.Prefix) + stagingNotChecked
	}
	if err := h.checkS11Credentials(ctx, made.backup, nil, rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: %w", err))
	}
	if err := h.deniedClusterAtEnd(ctx, made.cluster, rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: %w", err))
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	errs = append(errs, h.checkSourceS12(ctx, rec.Source, s11DeniedSourceDataCheckFile))
	errs = append(errs, h.sameManager(ctx, "S11 denied-credentials", rec.ManagerBefore, &rec.ManagerAfter))
	return errors.Join(errs...)
}

// denyKeys generates the denied access key and secret key, random and long
// enough for evidence.SecretForms to search for them in every encoding the
// redactor removes, and registers both with the redactor.
func (h *haRun) denyKeys() {
	access, secret := rand.Text(), rand.Text()+rand.Text()
	h.redactor.Add(access, secret)
	h.deniedKeys = []string{access, secret}
}

// deniedFault asks the object store, with the run's own client and the
// denied keys, to list the backup bucket under the prefix. Only an answer
// that refuses access confirms the fault. A store that accepts the keys, one
// that cannot be asked, or another answer blocks S11 denied-credentials
// before anything is created: the fault is not in place, and the run does
// not change the store's access rules.
func (h *haRun) deniedFault(ctx context.Context, rec *s11DeniedRecord) error {
	store, stop, err := h.openStoreWith(ctx, h.deniedKeys[0], h.deniedKeys[1])
	if err != nil {
		return err
	}
	defer stop()
	_, err = store.List(ctx, rec.Bucket, rec.Prefix+"/")
	at := time.Now().UTC()
	fault := &s11DeniedFault{CheckedAt: &at}
	rec.Fault = fault
	if err == nil {
		return fmt.Errorf("the object store accepted the denied keys and listed bucket %s; S11 denied-credentials "+
			"needs a store that refuses them, and the run does not change its access rules", rec.Bucket)
	}
	// The code of an S3 answer is not in its text.
	fault.Answer = err.Error()
	fault.AnswerKind = storeErrorKind(strings.TrimSpace(minio.ToErrorResponse(err).Code + " " + err.Error()))
	if fault.AnswerKind != storeErrorAccessDenied {
		return fmt.Errorf("the object store did not refuse access to the denied keys, so the fault is not "+
			"confirmed: list bucket %s: %w", rec.Bucket, err)
	}
	fault.Refused = true
	return nil
}

// createDenied creates the Secret with the denied keys and the standalone
// cluster that names it in the run namespace: one member, the HA cluster's
// version, image, database and storage, and the HA cluster's object store
// endpoint without a bucket, which only an HA cluster needs.
func (h *haRun) createDenied(ctx context.Context, rec *s11DeniedCluster, made *s11DeniedMade) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s11DeniedSecretName, Namespace: h.cfg.Namespace},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{seedAccessKey: []byte(h.deniedKeys[0]),
			seedSecretKey: []byte(h.deniedKeys[1])}}
	if err := h.Client.Create(ctx, secret); err != nil {
		return fmt.Errorf("create the Secret %s with the denied keys: %w", s11DeniedSecretName, err)
	}
	made.secret, rec.SecretUID = secret, string(secret.UID)
	if secret.UID == "" {
		return fmt.Errorf("created Secret %s has no observed UID", s11DeniedSecretName)
	}
	spec := h.clusterSpec("", s11DeniedSecretName)
	spec.Topology.PromotableMembers = 1
	spec.HighAvailability.Enabled = false
	spec.ObjectStorage.Bucket = ""
	cluster := &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: s11DeniedClusterName, Namespace: h.cfg.Namespace}, Spec: spec}
	if err := h.Client.Create(ctx, cluster); err != nil {
		return fmt.Errorf("create the cluster %s: %w", s11DeniedClusterName, err)
	}
	created := time.Now().UTC()
	made.cluster, rec.UID, rec.CreatedAt = cluster, string(cluster.UID), &created
	if cluster.UID == "" {
		return fmt.Errorf("created cluster %s has no observed UID", s11DeniedClusterName)
	}
	return nil
}

// waitDeniedReady reads the standalone cluster until its status has observed
// its generation and reports Ready True, for at most BackupDeniedReadyLimit
// from its creation. Every status read is searched for a credential. A
// cluster that is not Ready by then, or whose bootstrap failed, blocks S11
// denied-credentials: the backup could not be requested.
func (h *haRun) waitDeniedReady(ctx context.Context, created *databasev1alpha1.CubridCluster,
	rec *s11DeniedRecord) error {
	wctx, cancel := context.WithDeadline(ctx, rec.Cluster.CreatedAt.Add(h.cfg.BackupDeniedReadyLimit))
	defer cancel()
	err := pollUntil(wctx, h.cfg.Poll, func() (bool, error) {
		if err := h.guard(wctx); err != nil {
			return false, err
		}
		cluster := &databasev1alpha1.CubridCluster{}
		if err := h.Client.Get(wctx, client.ObjectKeyFromObject(created), cluster); err != nil {
			if retryable(err) || wctx.Err() != nil {
				rec.Cluster.Waiting = "the cluster" + unread + err.Error()
				return false, nil
			}
			return false, err
		}
		if cluster.UID != created.UID {
			return false, fmt.Errorf("the cluster %s was replaced", created.Name)
		}
		rec.Credentials.StatusesChecked++
		if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the status read of "+created.Name,
			cluster.Status); err != nil {
			return false, err
		}
		unmet, err := standaloneReady(cluster)
		rec.Cluster.Waiting = unmet
		return unmet == "" && err == nil, err
	})
	if err != nil {
		if ctx.Err() == nil && wctx.Err() != nil {
			return fmt.Errorf("the cluster %s was not Ready within %s: %s", created.Name,
				h.cfg.BackupDeniedReadyLimit, cmp.Or(rec.Cluster.Waiting, "no reading was taken"))
		}
		return fmt.Errorf("the cluster %s was not Ready: %s: %w", created.Name,
			cmp.Or(rec.Cluster.Waiting, "no reading was taken"), err)
	}
	rec.Cluster.ReadyWithin = time.Since(*rec.Cluster.CreatedAt).Round(time.Millisecond).String()
	return nil
}

// deniedClusterAtEnd reads the standalone cluster once more at the end and
// searches its status for a credential, so that a key written into it after
// it was Ready is found too.
func (h *haRun) deniedClusterAtEnd(ctx context.Context, created *databasev1alpha1.CubridCluster,
	creds *s11Credentials) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), cluster); err != nil {
		return fmt.Errorf("read the cluster %s at the end: %w", created.Name, err)
	}
	creds.StatusesChecked++
	return h.lookForCredentials(&creds.FoundIn, "the status of "+created.Name+" at the end", cluster.Status)
}

// standaloneReady reports what of a standalone cluster's status does not
// hold yet, and a bootstrap that failed as an error: it does not start again
// by itself.
func standaloneReady(cluster *databasev1alpha1.CubridCluster) (string, error) {
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
	c := meta.FindStatusCondition(cluster.Status.Conditions, clusterReadyCondition)
	if c == nil {
		return clusterReadyCondition + " is not reported", nil
	}
	if c.Status != metav1.ConditionTrue {
		return fmt.Sprintf("%s is %s (%s: %s)", clusterReadyCondition, c.Status, c.Reason, c.Message), nil
	}
	return "", nil
}

// judgeDenied judges the backup as waitBackup left it. backupdb on the
// member does not use the object store; the Instance Manager's first Put
// (UploadBackup in internal/instancemanager/upload.go) does, with the keys
// of its environment, and a store that refuses them fails it, which
// MinioObjectStore.Put reports as "put <bucket>/<key>:" and runBackup
// (internal/instancemanager/server.go) ends the operation Failed with
// "upload failed: " and that error; no manifest is written. The Operator
// maps it onto the backup in failedBackup: phase Failed, Ready reason
// BackupFailed with the operation's reason as message. That is what is
// required. Completed, or a status.artifact, must never happen; a failure
// before the upload did not exercise the denied keys and blocks S11
// denied-credentials, and so does an upload failure whose quoted answer is
// not a refusal of the keys (storeErrorAccessDenied); an upload that failed
// but not for its destination fails it, and so does a backup neither
// Completed nor Failed within BackupLimit.
func (h *haRun) judgeDenied(backup *databasev1alpha1.CubridBackup, waitErr error, requested time.Time,
	rec *s11DeniedRecord, result *evidence.Scenario) error {
	s := backup.Status
	rec.Artifact = s.Artifact
	if s.Phase == databasev1alpha1.BackupPhaseCompleted || s.Phase == databasev1alpha1.BackupPhaseFailed {
		rec.Failure = readyReason(backup)
	}
	var events []string
	if s.Phase == databasev1alpha1.BackupPhaseCompleted {
		events = append(events, "the backup of "+s11DeniedClusterName+", whose object store keys the store "+
			"refused, ended Completed")
	}
	if s.Artifact != nil {
		events = append(events, "status.artifact names "+s.Artifact.URI+" for a backup of "+s11DeniedClusterName+
			", whose object store keys the store refused")
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
		if errors.As(waitErr, new(backupVerdict)) {
			return waitErr
		}
		// The backup could not be read: S11 denied-credentials could not be
		// judged.
		return fmt.Errorf("read the CubridBackup: %w", waitErr)
	}
	c := meta.FindStatusCondition(s.Conditions, backupReady)
	if c == nil || c.Reason != s11MissingReason || !strings.HasPrefix(c.Message, uploadFailed) {
		// The backup ended before its upload, so the denied keys were not
		// exercised: S11 denied-credentials could not be judged.
		return fmt.Errorf("the backup with the denied keys ended Failed before its upload, not with %s %q: %s",
			s11MissingReason, uploadFailed, readyReason(backup))
	}
	want := uploadFailed + "put " + rec.Bucket + "/"
	if !strings.HasPrefix(c.Message, want) {
		return backupVerdict{fmt.Errorf("the upload of the backup with the denied keys failed for another reason "+
			"than its destination, not with %q: %s", want, readyReason(backup))}
	}
	// The store's answer follows "upload failed: put <bucket>/<key>: ".
	if _, answer, ok := strings.Cut(strings.TrimPrefix(c.Message, want), ": "); ok {
		rec.StoreError, rec.StoreErrorKind = answer, storeErrorKind(answer)
	}
	rec.FailedWithin = rec.Phases[len(rec.Phases)-1].At.Sub(requested).Round(time.Millisecond).String()
	result.Measurements["failedWithin"] = rec.FailedWithin
	if done := s.CompletedAt; done != nil {
		rec.StatusFailedWithin = done.Sub(backup.CreationTimestamp.Time).String()
	}
	// Only an answer that refuses the keys shows that the denied keys failed
	// the upload; a timeout, a reset or a missing bucket does not.
	if rec.StoreErrorKind != storeErrorAccessDenied {
		answer := rec.StoreError
		if answer == "" {
			answer = "no answer quoted in " + strconv.Quote(c.Message)
		}
		return fmt.Errorf("the upload to %s failed, but the store's answer does not show a refusal of the keys: "+
			"%s; the denied keys' effect is not confirmed", rec.Bucket, answer)
	}
	return nil
}

// deniedAtEnd lists the backup bucket under the prefix with the seed
// Secret's keys. Any object there fails S11 denied-credentials: what the
// backup was requested to write went to the store. A store that cannot be
// asked blocks it.
func (h *haRun) deniedAtEnd(ctx context.Context, rec *s11DeniedRecord) error {
	store, stop, err := h.openS11Store(ctx)
	if err != nil {
		return err
	}
	defer stop()
	keys, err := store.List(ctx, rec.Bucket, rec.Prefix+"/")
	if err != nil {
		return fmt.Errorf("list bucket %s under %s at the end: %w", rec.Bucket, rec.Prefix, err)
	}
	at := time.Now().UTC()
	rec.CheckedAtEnd = &at
	if len(keys) == 0 {
		return nil
	}
	rec.ObjectsAtEnd = keys
	rec.ManifestAtEnd = slices.ContainsFunc(keys, func(k string) bool {
		return path.Base(k) == instancemanager.ManifestObjectName
	})
	held := fmt.Sprintf("bucket %s holds %d objects under %s at the end", rec.Bucket, len(keys), rec.Prefix)
	if rec.ManifestAtEnd {
		held += ", a manifest.json among them"
	}
	return backupVerdict{errors.New(held)}
}

// deleteDenied deletes the CubridBackup, the cluster and the Secret the run
// created, in that order and each by its UID, with a budget of its own so
// that a run that ended still deletes them, and records each outcome. The
// cluster's data PVC is retained and goes with the namespace. It does not
// judge S11 denied-credentials.
func (h *haRun) deleteDenied(ctx context.Context, made s11DeniedMade, rec *s11DeniedRecord) bool {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.CleanupTimeout)
	defer cancel()
	type owned struct {
		kind string
		obj  client.Object
	}
	var objects []owned
	if made.backup != nil {
		objects = append(objects, owned{"CubridBackup", made.backup})
	}
	if made.cluster != nil && made.cluster.UID != "" {
		objects = append(objects, owned{"cluster", made.cluster})
		rec.Residue = strings.TrimSuffix(rec.Residue, stagingNotChecked) + deniedPVCResidue + stagingNotChecked
	}
	if made.secret != nil && made.secret.UID != "" {
		objects = append(objects, owned{"Secret", made.secret})
	}
	if len(objects) == 0 {
		return true
	}
	cleaned := true
	outcomes := make([]string, 0, len(objects))
	for _, o := range objects {
		uid := o.obj.GetUID()
		name := fmt.Sprintf("the %s %s (UID %s)", o.kind, o.obj.GetName(), uid)
		err := h.Client.Delete(dctx, o.obj, client.Preconditions{UID: &uid},
			client.PropagationPolicy(metav1.DeletePropagationBackground))
		switch {
		case apierrors.IsNotFound(err):
			outcomes = append(outcomes, name+" was already gone")
		case err != nil:
			cleaned = false
			outcomes = append(outcomes, name+" could not be deleted and is left until the namespace is removed: "+
				err.Error())
		default:
			outcomes = append(outcomes, "the deletion of "+strings.TrimPrefix(name, "the ")+" was accepted")
		}
	}
	rec.Cleanup = strings.Join(outcomes, "; ")
	return cleaned
}
