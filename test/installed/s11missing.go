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
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	s11MissingLane                = "lab-installed-ha-s11-missing-destination"
	s11MissingVariant             = "missing-destination"
	s11MissingRecordFile          = "S11/missing-destination/record.json"
	s11MissingSourceDataCheckFile = "S11/missing-destination/source-data-check.json"
	s11MissingBackupName          = "s11-missing-destination"
	// s11MissingBucketPrefix starts the name of the bucket that must not
	// exist; the run ID follows it.
	s11MissingBucketPrefix = "cubrid-lab-missing-"
	// s11MissingReason is the Ready reason of a backup whose operation the
	// Instance Manager ended Failed (failedBackup in
	// internal/controller/cubridbackup_controller.go).
	s11MissingReason = "BackupFailed"
	// uploadFailed starts the failure reason of an operation whose upload
	// failed (runBackup in internal/instancemanager/server.go).
	uploadFailed = "upload failed: "
	// stagingNotChecked ends the residue: the run does not read the member.
	stagingNotChecked = "; the target member's staging directory after the failed upload is not checked"
	// The kinds of the object store's answer the failure quotes.
	storeErrorNoSuchBucket = "no-such-bucket"
	storeErrorAccessDenied = "access-denied"
	// storeErrorReset is a connection reset or refused, or a broken pipe:
	// what a cut connection to the store reports.
	storeErrorReset = "reset-or-refused"
	storeErrorOther = "other"
)

// s11MissingBucket is what the run's own client found of the bucket that
// must not exist, before the request and at the end. ObjectsAtEnd lists what
// it held at the end, when it existed then; ManifestAtEnd is set when a
// manifest.json was among them.
type s11MissingBucket struct {
	Name          string     `json:"name"`
	AbsentBefore  bool       `json:"absentBefore"`
	CheckedBefore *time.Time `json:"checkedBefore,omitempty"`
	AbsentAtEnd   bool       `json:"absentAtEnd"`
	CheckedAtEnd  *time.Time `json:"checkedAtEnd,omitempty"`
	ObjectsAtEnd  []string   `json:"objectsAtEnd"`
	ManifestAtEnd bool       `json:"manifestAtEnd"`
}

// s11MissingRecord is the evidence of S11 missing-destination besides the
// source's data check.
type s11MissingRecord struct {
	Backup             string            `json:"backup"`
	BackupUID          string            `json:"backupUID,omitempty"`
	Prefix             string            `json:"prefix"`
	ObjectStoreService string            `json:"objectStoreService"`
	Bucket             *s11MissingBucket `json:"bucket,omitempty"`
	RequestedAt        *time.Time        `json:"requestedAt,omitempty"`
	Phases             []s11Phase        `json:"phases"`
	// FailedWithin runs from the return of the request to the reading that
	// saw Failed, an upper bound; StatusFailedWithin from the backup's
	// creationTimestamp to its status.completedAt, as the API server and the
	// Operator recorded them.
	FailedWithin       string `json:"failedWithin,omitempty"`
	StatusFailedWithin string `json:"statusFailedWithin,omitempty"`
	// Failure is the Ready reason and message of the backup as it ended.
	// StoreError is the object store's answer as the failure quotes it,
	// after "put <bucket>/<key>: ", and StoreErrorKind what it names:
	// storeErrorNoSuchBucket, storeErrorAccessDenied or storeErrorOther. They
	// are observations; the verdict does not depend on them.
	Failure        string                                 `json:"failure,omitempty"`
	StoreError     string                                 `json:"storeError,omitempty"`
	StoreErrorKind string                                 `json:"storeErrorKind,omitempty"`
	Artifact       *databasev1alpha1.CubridBackupArtifact `json:"artifact,omitempty"`
	// ManagerBefore is the installed manager as LAB01 read it at its end,
	// ManagerAfter as this variant read it at its end.
	ManagerBefore *managerIdentity `json:"managerBefore,omitempty"`
	ManagerAfter  *managerIdentity `json:"managerAfter,omitempty"`
	Source        *s12Source       `json:"source,omitempty"`
	Credentials   *s11Credentials  `json:"credentials,omitempty"`
	// JudgedAt is when the run reached its verdict.
	JudgedAt *time.Time `json:"judgedAt,omitempty"`
	Notes    []string   `json:"notes,omitempty"`
	// Cleanup says whether the CubridBackup the run created was deleted;
	// Residue is what the run leaves in the object store.
	Cleanup string `json:"cleanup"`
	Residue string `json:"residue"`
}

// planS11MissingDestination returns the lane and the requirements of a run
// that selects S11 missing-destination after S11 complete, and names it in
// the summary's first note; otherwise lane and requirements as they are.
func planS11MissingDestination(cfg HAConfig, lane string, requirements []evidence.Requirement,
	summary *evidence.Summary) (string, []evidence.Requirement) {
	if !cfg.BackupMissingDestination {
		return lane, requirements
	}
	summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete and S11 " +
		"missing-destination: a full backup to object storage, downloaded and checked by the run, and a backup " +
		"to a bucket that does not exist, which must end Failed; no other S00-S15 qualification"
	return s11MissingLane, append(requirements, evidence.Requirement{ID: s11ID, Variant: s11MissingVariant})
}

// runS11MissingDestination runs S11 missing-destination after S11 complete
// passed: the run confirms with its own client that a bucket named after the
// run does not exist, requests a full backup of the run's database to it, and
// the backup must end Failed within BackupLimit for its upload, never
// Completed and without status.artifact. The bucket must still be absent at
// the end, the source unchanged, and no credential in a status read or an
// Event. The run never creates, writes or removes a bucket; it deletes the
// CubridBackup it created. A run that ends, or a check that cannot be made,
// before S11 missing-destination is judged blocks it; what was seen to break
// stays a failure.
func (h *haRun) runS11MissingDestination(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s11ID+"/"+s11MissingVariant, &result)()
	result = evidence.Scenario{ID: s11ID, Variant: s11MissingVariant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s11MissingRecord{Backup: s11MissingBackupName, Prefix: h.rec.Seed.Prefix + "/missing-destination",
		ObjectStoreService: h.cfg.ObjectStoreService, Phases: []s11Phase{},
		Bucket:      &s11MissingBucket{Name: s11MissingBucketPrefix + h.rec.RunID, ObjectsAtEnd: []string{}},
		Credentials: &s11Credentials{FoundIn: []string{}},
		Notes: []string{"the run only asks the object store whether the bucket exists and lists it; it never " +
			"creates, writes to or removes a bucket",
			"the run asks with the seed Secret's keys, and the members write with the cluster's " +
				"spec.objectStorage Secret, a copy of them; the verdict does not depend on the object store's " +
				"error code (storeError), which is recorded until a lab run shows what the store answers",
			"the Operator's and the Instance Manager's logs were not checked"},
		Cleanup: "nothing to delete: the run created no CubridBackup", Residue: "nothing: the run wrote nothing " +
			"to the object store" + stagingNotChecked}
	var created *databasev1alpha1.CubridBackup
	defer func() {
		cleaned := h.deleteMissingBackup(ctx, created, &rec)
		result.CleanupSucceeded = &cleaned
		// A pass without its evidence is a failure; a blocked or failed S11
		// missing-destination keeps its result, with the evidence that could
		// not be written.
		if err := h.writeJSON(s11MissingRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S11 missing-destination evidence: "+
				err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S11/"+s11MissingVariant+"/")
		})
	}()
	result.Limits = map[string]string{backupLimitName: h.cfg.BackupLimit.String()}
	result.Measurements = map[string]any{"failedWithin": evidence.Unknown}
	err := h.missingSteps(ctx, &result, &rec, &created)
	// A credential found in any status read or Event is a failure, whichever
	// step ended S11 missing-destination.
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
		result.Result, result.Reason = evidence.Blocked, "the run ended before S11 missing-destination was judged: "+
			reason
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, reason
	default:
		result.Result = evidence.Pass
		result.Reason = "the backup to " + rec.Bucket.Name + ", which the run found absent before the request, " +
			"ended Failed within backup_limit for its upload, never Completed and without status.artifact; the " +
			"bucket was still absent at the end; the source was unchanged; no credential was in the statuses read " +
			"or the Events; the Operator's and the Instance Manager's logs were not checked"
	}
	judged := time.Now().UTC()
	rec.JudgedAt = &judged
	return result
}

// missingSteps runs the steps of S11 missing-destination in order. Before
// the request, the first error ends them. After it, the backup is judged and
// the checks at the end are all made; their errors are joined.
func (h *haRun) missingSteps(ctx context.Context, result *evidence.Scenario, rec *s11MissingRecord,
	created **databasev1alpha1.CubridBackup) error {
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S11 missing-destination could not tell whether a credential is in a status or an "+
			"Event", err)
	}
	before := h.rec.OperatorAfter
	rec.ManagerBefore = &before
	if err := h.missingBefore(ctx, rec.Bucket); err != nil {
		return err
	}
	backup, err := h.createBackup(ctx, s11MissingBackupName, haClusterName, rec.Bucket.Name, rec.Prefix)
	if err != nil {
		return err
	}
	requested := time.Now().UTC()
	*created = backup
	rec.BackupUID, rec.RequestedAt = string(backup.UID), &requested
	backup, err = h.waitBackup(ctx, backup, &rec.Phases, rec.Credentials)
	errs := []error{h.judgeMissing(backup, err, requested, rec, result)}
	errs = append(errs, h.missingAtEnd(ctx, rec.Bucket))
	if b := rec.Bucket; b.CheckedAtEnd != nil && !b.AbsentAtEnd {
		rec.Residue = "bucket " + b.Name + " exists at the end; the run did not create it and does not remove it" +
			stagingNotChecked
	}
	if err := h.checkS11Credentials(ctx, *created, nil, rec.Credentials); err != nil {
		errs = append(errs, fmt.Errorf("look for credentials: %w", err))
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	errs = append(errs, h.checkSourceS12(ctx, rec.Source, s11MissingSourceDataCheckFile))
	errs = append(errs, h.sameManager(ctx, "S11 missing-destination", rec.ManagerBefore, &rec.ManagerAfter))
	return errors.Join(errs...)
}

// missingBefore asks the object store, with the run's own client, whether
// the bucket exists. One that exists, or a store that cannot be asked, blocks
// S11 missing-destination before anything is requested.
func (h *haRun) missingBefore(ctx context.Context, b *s11MissingBucket) error {
	store, stop, err := h.openS11Store(ctx)
	if err != nil {
		return err
	}
	defer stop()
	exists, err := store.BucketExists(ctx, b.Name)
	if err != nil {
		return fmt.Errorf("ask the object store whether bucket %s exists: %w", b.Name, err)
	}
	at := time.Now().UTC()
	b.CheckedBefore = &at
	if exists {
		return fmt.Errorf("bucket %s exists; S11 missing-destination needs a bucket that does not exist, and the "+
			"run does not remove one", b.Name)
	}
	b.AbsentBefore = true
	return nil
}

// judgeMissing judges the backup as waitBackup left it. The Instance Manager
// uploads every data object before the manifest (UploadBackup in
// internal/instancemanager/upload.go), so a bucket that does not exist fails
// the first Put, which MinioObjectStore.Put reports as "put <bucket>/<key>:"
// and runBackup (internal/instancemanager/server.go) ends the operation
// Failed with "upload failed: " and that error; no manifest is written. The
// Operator maps the Failed operation onto the backup in failedBackup: phase
// Failed, Ready reason BackupFailed with the operation's reason as message.
// That is what is required. Completed, or a status.artifact, must never
// happen; Failed for another reason fails S11 missing-destination, and so
// does a backup neither Completed nor Failed within BackupLimit.
func (h *haRun) judgeMissing(backup *databasev1alpha1.CubridBackup, waitErr error, requested time.Time,
	rec *s11MissingRecord, result *evidence.Scenario) error {
	s := backup.Status
	rec.Artifact = s.Artifact
	if s.Phase == databasev1alpha1.BackupPhaseCompleted || s.Phase == databasev1alpha1.BackupPhaseFailed {
		rec.Failure = readyReason(backup)
	}
	var events []string
	if s.Phase == databasev1alpha1.BackupPhaseCompleted {
		events = append(events, "the backup to "+rec.Bucket.Name+", a bucket that did not exist, ended Completed")
	}
	if s.Artifact != nil {
		events = append(events, "status.artifact names "+s.Artifact.URI+" for a backup to "+rec.Bucket.Name+
			", a bucket that did not exist")
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
		// The backup could not be read: S11 missing-destination could not
		// be judged.
		return fmt.Errorf("read the CubridBackup: %w", waitErr)
	}
	c := meta.FindStatusCondition(s.Conditions, backupReady)
	if c == nil || c.Reason != s11MissingReason || !strings.HasPrefix(c.Message, uploadFailed) {
		// The backup ended before its upload, so the missing bucket was not
		// exercised: S11 missing-destination could not be judged.
		return fmt.Errorf("the backup to the missing bucket ended Failed before its upload, not with %s %q: %s",
			s11MissingReason, uploadFailed, readyReason(backup))
	}
	want := uploadFailed + "put " + rec.Bucket.Name + "/"
	if !strings.HasPrefix(c.Message, want) {
		return backupVerdict{fmt.Errorf("the upload of the backup to the missing bucket failed for another reason "+
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
	return nil
}

// accessRefusals are the S3 error codes and messages, lower case, of a
// refused access: a denied request, an unknown access key and a signature
// made with another secret key.
var accessRefusals = []string{"accessdenied", "access denied", "invalidaccesskeyid",
	"access key id you provided does not exist", "signaturedoesnotmatch", "signature we calculated does not match"}

// storeErrorKind names what the object store's answer says: a bucket that
// does not exist, a refused access, a reset or refused connection, or
// something else. It is an observation: the answer's words are the store's.
// A refused access is named first: the message of an unknown access key also
// says "does not exist".
func storeErrorKind(answer string) string {
	lower := strings.ToLower(answer)
	switch {
	case slices.ContainsFunc(accessRefusals, func(s string) bool { return strings.Contains(lower, s) }):
		return storeErrorAccessDenied
	case strings.Contains(lower, "nosuchbucket") || strings.Contains(lower, "does not exist"):
		return storeErrorNoSuchBucket
	case strings.Contains(lower, "connection reset") || strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "broken pipe"):
		return storeErrorReset
	}
	return storeErrorOther
}

// missingAtEnd asks the object store again whether the bucket exists, and
// lists it if it does. A bucket that exists at the end fails S11
// missing-destination: what the backup was requested to write went
// somewhere. A store that cannot be asked blocks it.
func (h *haRun) missingAtEnd(ctx context.Context, b *s11MissingBucket) error {
	store, stop, err := h.openS11Store(ctx)
	if err != nil {
		return err
	}
	defer stop()
	exists, err := store.BucketExists(ctx, b.Name)
	if err != nil {
		return fmt.Errorf("ask the object store at the end whether bucket %s exists: %w", b.Name, err)
	}
	at := time.Now().UTC()
	b.CheckedAtEnd = &at
	if !exists {
		b.AbsentAtEnd = true
		return nil
	}
	keys, err := store.List(ctx, b.Name, "")
	if err != nil {
		return errors.Join(backupVerdict{fmt.Errorf("bucket %s exists at the end", b.Name)},
			fmt.Errorf("list bucket %s: %w", b.Name, err))
	}
	b.ObjectsAtEnd = keys
	b.ManifestAtEnd = slices.ContainsFunc(keys, func(k string) bool {
		return path.Base(k) == instancemanager.ManifestObjectName
	})
	held := fmt.Sprintf("bucket %s exists at the end, holding %d objects", b.Name, len(keys))
	if b.ManifestAtEnd {
		held += ", a manifest.json among them"
	}
	return backupVerdict{errors.New(held)}
}

// sameManager reads the installed manager at the end of the variant named
// into after. A manager other than before, the one LAB01 checked, blocks the
// variant: the backup was not taken by the Operator the run started with.
func (h *haRun) sameManager(ctx context.Context, variant string, before *managerIdentity,
	after **managerIdentity) error {
	read, err := h.observeManager(ctx, h.cfg.Config)
	*after = &read
	if err != nil {
		return err
	}
	if read.DeploymentUID != before.DeploymentUID || !slices.Equal(read.Pods, before.Pods) {
		return errors.New("the installed manager identity changed during " + variant)
	}
	return nil
}

// deleteMissingBackup deletes the CubridBackup the run created, by its UID,
// with a budget of its own so that a run that ended still deletes it, and
// records the outcome. It does not judge S11 missing-destination.
func (h *haRun) deleteMissingBackup(ctx context.Context, created *databasev1alpha1.CubridBackup,
	rec *s11MissingRecord) bool {
	if created == nil {
		return true
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.CleanupTimeout)
	defer cancel()
	uid := created.UID
	name := fmt.Sprintf("the CubridBackup %s (UID %s)", created.Name, uid)
	if err := h.Client.Delete(dctx, created, client.Preconditions{UID: &uid}); err != nil &&
		!apierrors.IsNotFound(err) {
		rec.Cleanup = name + " could not be deleted and is left until the namespace is removed: " + err.Error()
		return false
	}
	rec.Cleanup = name + " was deleted"
	return true
}
