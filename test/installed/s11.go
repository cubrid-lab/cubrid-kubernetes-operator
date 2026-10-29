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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	s11Lane         = "lab-installed-ha-s11"
	s11ID           = "S11"
	s11Variant      = "complete"
	s11RecordFile   = "S11/complete/record.json"
	s11ManifestFile = "S11/complete/manifest.json"
	s11BackupName   = "s11-complete"
	// s11Marker names the marker row written right before the backup; its
	// value is the run ID.
	s11Marker = "s11"
	// backupReady is the condition that holds a backup's reason.
	backupReady = "Ready"
	// backupLimitName is the contract's limit on a backup (backup_limit).
	backupLimitName = "backup_limit"
	// The measurements of an artifact that S11 complete and the variants
	// that verify one share.
	measBackupCompletedWithin = "backupCompletedWithin"
	measObjectsVerified       = "objectsVerified"
	measArtifactBytes         = "artifactBytes"
	measObjectsNotInManifest  = "objectsNotInManifest"
)

// s3Bucket is the bucket rule of a CubridBackup's destination; two adjacent
// dots are refused separately.
var s3Bucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// The flags of S11 variants, as the refusals of a combination name them.
const (
	flagRestore                = "-restore"
	flagMissingDestination     = "-backup-missing-destination"
	flagDeniedCredentials      = "-backup-denied-credentials"
	flagSignatureMismatch      = "-backup-signature-mismatch"
	flagInterruptedUpload      = "-backup-interrupted-upload"
	flagOperatorRestart        = "-backup-operator-restart"
	flagInstanceManagerRestart = "-backup-instance-manager-restart"
	flagOverlapping            = "-backup-overlapping"
	flagOperationStoreLoss     = "-backup-operation-store-loss"
	flagGracefulStop           = "-backup-graceful-stop"
)

// validateS11 checks the inputs of S11 complete, when it is selected.
func (c HAConfig) validateS11() error {
	for _, v := range []struct {
		selected      bool
		flag, variant string
	}{{c.BackupMissingDestination, flagMissingDestination, "missing-destination"},
		{c.BackupDeniedCredentials, flagDeniedCredentials, "denied-credentials"},
		{c.BackupSignatureMismatch, flagSignatureMismatch, "signature-mismatch"},
		{c.BackupInterruptedUpload, flagInterruptedUpload, "interrupted-upload"},
		{c.BackupOperatorRestart, flagOperatorRestart, "operator-restart"},
		{c.BackupInstanceManagerRestart, flagInstanceManagerRestart, "instance-manager-restart"},
		{c.BackupOverlapping, flagOverlapping, "overlapping"},
		{c.BackupOperationStoreLoss, flagOperationStoreLoss, s11StoreLossVariant},
		{c.BackupGracefulStop, flagGracefulStop, s11GracefulStopVariant}} {
		if v.selected && c.BackupBucket == "" {
			return fmt.Errorf("%s needs -backup-bucket: S11 %s follows S11 complete", v.flag, v.variant)
		}
	}
	if c.BackupBucket == "" {
		return nil
	}
	if c.Fault != "" {
		return errors.New("-backup-bucket and -fault each select a scenario after LAB01; a run takes one of them")
	}
	if !s3Bucket.MatchString(c.BackupBucket) || strings.Contains(c.BackupBucket, "..") || c.BackupLimit <= 0 {
		return errors.New("S11 needs an S3 bucket name for the backups and a positive backup limit")
	}
	if c.BackupMissingDestination && c.Restore {
		return errors.New("-backup-missing-destination and -restore select different lanes; a run takes one of them")
	}
	if c.BackupDeniedCredentials && c.Restore {
		return errors.New("-backup-denied-credentials and -restore select different lanes; a run takes one of them")
	}
	if c.BackupDeniedCredentials && c.BackupMissingDestination {
		return errors.New("-backup-denied-credentials and -backup-missing-destination select different lanes; a " +
			"run takes one of them")
	}
	if c.BackupDeniedCredentials && c.BackupDeniedReadyLimit <= 0 {
		return errors.New("-backup-denied-credentials needs a positive -backup-denied-ready-limit for its cluster")
	}
	if err := c.validateS11Signature(); err != nil {
		return err
	}
	if err := c.validateS11Interrupted(); err != nil {
		return err
	}
	if err := c.validateS11OperatorRestart(); err != nil {
		return err
	}
	if err := c.validateS11InstanceManagerRestart(); err != nil {
		return err
	}
	if err := c.validateS11Overlapping(); err != nil {
		return err
	}
	if err := c.validateS11OperationStoreLoss(); err != nil {
		return err
	}
	if err := c.validateS11GracefulStop(); err != nil {
		return err
	}
	_, _, _, err := splitService(c.ObjectStoreService)
	return err
}

// splitService reads <namespace>/<service>:<port>.
func splitService(s string) (namespace, service, port string, err error) {
	namespace, rest, slash := strings.Cut(s, "/")
	service, port, colon := strings.Cut(rest, ":")
	n, convErr := strconv.Atoi(port)
	if !slash || !colon || len(validation.IsDNS1123Label(namespace)) != 0 ||
		len(validation.IsDNS1035Label(service)) != 0 || convErr != nil || n < 1 || n > 65535 {
		return "", "", "", fmt.Errorf("the object store Service must be <namespace>/<service>:<port>, not %q", s)
	}
	return namespace, service, port, nil
}

// errObjectNotFound is the error of an object the store answers it does not
// hold. Only that error shows a missing object; any other leaves the object
// unchecked.
var errObjectNotFound = errors.New("the object store holds no such object")

// maxManifestSize bounds the manifest the run reads. A larger one blocks
// S11: the bound is the run's, not the contract's.
var maxManifestSize int64 = 16 << 20

// objectStore reads an S3-compatible store and writes to it. S11 verifies
// the artifact with a client of its own, not through the Operator or the
// Instance Manager; S12 tampered writes and removes its copy of it with the
// same client, S11 missing-destination asks it whether a bucket exists, and
// S11 denied-credentials and signature-mismatch list a prefix with the seed keys
// and with keys the store must refuse.
type objectStore interface {
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	List(ctx context.Context, bucket, prefix string) ([]string, error)
	BucketExists(ctx context.Context, bucket string) (bool, error)
	// Put stores size bytes of r under key; Remove removes key.
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64) error
	Remove(ctx context.Context, bucket, key string) error
}

type minioStore struct{ client *minio.Client }

func newMinioStore(endpoint, accessKey, secretKey string, secure bool) (objectStore, error) {
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure})
	if err != nil {
		return nil, err
	}
	return minioStore{client: c}, nil
}

// Get opens an object; a missing object fails at the first read, with
// errObjectNotFound.
func (s minioStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, notFound(err)
	}
	return minioObject{obj}, nil
}

type minioObject struct{ *minio.Object }

func (o minioObject) Read(p []byte) (int, error) {
	n, err := o.Object.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = notFound(err)
	}
	return n, err
}

// notFound marks an answer that the store holds no such key, or bucket, as
// errObjectNotFound.
func notFound(err error) error {
	if r := minio.ToErrorResponse(err); r.Code == "NoSuchKey" || r.Code == "NoSuchBucket" ||
		r.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %v", errObjectNotFound, err)
	}
	return err
}

func (s minioStore) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

// ListSized lists the objects under prefix with their sizes.
func (s minioStore) ListSized(ctx context.Context, bucket, prefix string) ([]storedObject, error) {
	var objects []storedObject
	for obj := range s.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		objects = append(objects, storedObject{Key: obj.Key, Size: obj.Size})
	}
	return objects, nil
}

// ListIncomplete lists the multipart uploads under prefix that were started
// and neither completed nor aborted, as key and upload ID.
func (s minioStore) ListIncomplete(ctx context.Context, bucket, prefix string) ([]string, error) {
	var uploads []string
	for upload := range s.client.ListIncompleteUploads(ctx, bucket, prefix, true) {
		if upload.Err != nil {
			return nil, upload.Err
		}
		uploads = append(uploads, upload.Key+" (upload "+upload.UploadID+")")
	}
	return uploads, nil
}

func (s minioStore) BucketExists(ctx context.Context, bucket string) (bool, error) {
	return s.client.BucketExists(ctx, bucket)
}

func (s minioStore) Put(ctx context.Context, bucket, key string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{})
	return err
}

func (s minioStore) Remove(ctx context.Context, bucket, key string) error {
	return s.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

// s11KnownPoint is where the client history stood when the backup was
// requested; S12 compares a restore of the artifact with it. No client runs
// between LAB01's workload and the backup, so the position does not move
// while the backup runs.
type s11KnownPoint struct {
	// Marker and MarkerValue are the marker row written through the
	// read-write Service right before the backup.
	Marker          string    `json:"marker"`
	MarkerValue     string    `json:"markerValue"`
	MarkerWrittenAt time.Time `json:"markerWrittenAt,omitzero"`
	// Acknowledged counts the acknowledged operations of the history,
	// rolled-back ones included; LastAcknowledged is the last of them.
	Acknowledged     int    `json:"acknowledgedBefore"`
	LastAcknowledged string `json:"lastAcknowledgedOpId,omitempty"`
}

// s11Phase is one observed state of the backup; a new one is recorded when
// any of its fields but the time changes.
type s11Phase struct {
	At             time.Time          `json:"at"`
	Phase          string             `json:"phase"`
	OperationRef   string             `json:"operationRef,omitempty"`
	TargetInstance string             `json:"targetInstance,omitempty"`
	TargetRole     string             `json:"targetRole,omitempty"`
	FallbackUsed   bool               `json:"fallbackUsed"`
	Conditions     []metav1.Condition `json:"conditions"`
}

// s11Verification is the run's own check of the artifact.
type s11Verification struct {
	ManifestKey string `json:"manifestKey"`
	// ManifestSHA256 is the checksum of manifest.json as stored;
	// ManifestDigest is BackupManifest.Digest of it as parsed.
	ManifestSHA256  string `json:"manifestSHA256,omitempty"`
	ManifestDigest  string `json:"manifestDigest,omitempty"`
	Objects         int    `json:"objects"`
	ObjectsVerified int    `json:"objectsVerified"`
	// ManifestSizeBytes is the sum of the sizes the manifest lists, which
	// status.artifact.sizeBytes reports (UploadBackup); BytesDownloaded is
	// what the run read.
	ManifestSizeBytes int64 `json:"manifestSizeBytes"`
	BytesDownloaded   int64 `json:"bytesDownloaded"`
	// NotInManifest lists objects under the attempt prefix that the manifest
	// does not name, besides manifest.json.
	NotInManifest []string `json:"objectsNotInManifest"`
	// Listed is set once the attempt prefix was listed, ObjectsChecked once
	// the manifest was parsed and its objects were being downloaded.
	Listed         bool `json:"listed"`
	ObjectsChecked bool `json:"objectsChecked"`
	// Unreadable lists the manifest and the objects it names that could not
	// be read: what must never happen.
	Unreadable []string `json:"unreadable"`
	Problems   []string `json:"problems"`
}

// s11Credentials is where the run looked for the object store's keys.
type s11Credentials struct {
	StatusChecked bool `json:"statusChecked"`
	// StatusesChecked counts the statuses read while the run waited.
	StatusesChecked int  `json:"statusesChecked"`
	ManifestChecked bool `json:"manifestChecked"`
	EventsChecked   int  `json:"eventsChecked"`
	BackupEvents    int  `json:"backupEvents"`
	// Logs are the container logs searched (checkS11Logs), once the search
	// started.
	Logs    []s11LogCheck `json:"logs,omitempty"`
	FoundIn []string      `json:"foundIn"`
}

// s11Record is the evidence of S11 complete.
type s11Record struct {
	KnownPoint         *s11KnownPoint `json:"knownPoint,omitempty"`
	Backup             string         `json:"backup"`
	BackupUID          string         `json:"backupUID,omitempty"`
	Bucket             string         `json:"bucket"`
	Prefix             string         `json:"prefix"`
	ObjectStoreService string         `json:"objectStoreService"`
	RequestedAt        *time.Time     `json:"requestedAt,omitempty"`
	Phases             []s11Phase     `json:"phases"`
	// CompletedWithin runs from the return of the request to the reading
	// that saw Completed, an upper bound; StatusCompletedWithin from the
	// backup's creationTimestamp to its status.completedAt, as the API server
	// and the Operator recorded them.
	CompletedWithin       string                                 `json:"backupCompletedWithin,omitempty"`
	StatusCompletedWithin string                                 `json:"statusCompletedWithin,omitempty"`
	Artifact              *databasev1alpha1.CubridBackupArtifact `json:"artifact,omitempty"`
	EngineVersion         string                                 `json:"engineVersion"`
	Verification          *s11Verification                       `json:"verification,omitempty"`
	Credentials           *s11Credentials                        `json:"credentials,omitempty"`
	Notes                 []string                               `json:"notes,omitempty"`
	// Residue is what the run leaves in the object store.
	Residue string `json:"residue"`
	// source is the cluster the backup is of; nil is the HA cluster.
	source *s11Source
}

// s11Source is the cluster a backup's artifact must name: its UID, its
// members, and the role its target is selected in. An empty role is the HA
// cluster's PreferStandby: slave, or master when the selection fell back.
type s11Source struct {
	clusterUID string
	members    []string
	role       string
}

// sourceOf is the cluster rec's backup is of.
func (h *haRun) sourceOf(rec *s11Record) s11Source {
	if rec.source != nil {
		return *rec.source
	}
	return s11Source{clusterUID: h.rec.ClusterUID, members: h.members}
}

// runS11 runs S11 complete on the cluster LAB01 left in its checked state: a
// marker row is written, a full backup of the run's database is requested
// and must end Completed within BackupLimit, and the run downloads the
// artifact with its own client and checks it against the manifest and the
// status. No credential of the object store may be in the backup's status,
// the manifest, the Events of the run namespace, the installed manager's log
// or the Instance Manager's log of the backup's target (checkS11Logs). A run
// that ends before S11 is judged is blocked, not failed.
func (h *haRun) runS11(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s11ID+"/"+s11Variant, &result)()
	started := time.Now().UTC()
	result = evidence.Scenario{ID: s11ID, Variant: s11Variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s11Record{Backup: s11BackupName, Bucket: h.cfg.BackupBucket, Prefix: h.rec.Seed.Prefix + "/backups",
		ObjectStoreService: h.cfg.ObjectStoreService, Phases: []s11Phase{}, EngineVersion: h.engineVersion,
		Residue: "the artifact under the prefix is left in the bucket for S12; this run does not remove it"}
	// writeErr is the manifest's copy that could not be written.
	var writeErr error
	defer func() {
		h.s11Rec = &rec
		// A pass without its evidence is a failure; a blocked or failed S11
		// keeps its result, with the evidence that could not be written.
		if err := h.writeJSON(s11RecordFile, rec); err != nil {
			writeErr = errors.Join(writeErr, fmt.Errorf("write S11 evidence: %w", err))
		}
		if writeErr != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; "+strings.ReplaceAll(writeErr.Error(), "\n", "; "),
				"; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S11/")
		})
	}()
	end := func(r evidence.Result, format string, args ...any) evidence.Scenario {
		result.Result, result.Reason = r, fmt.Sprintf(format, args...)
		// A run that ended before S11 was judged is blocked, unless it had
		// already seen something that must never happen.
		if ctx.Err() != nil && r == evidence.Fail && len(result.NeverEvents) == 0 {
			result.Result, result.Reason = evidence.Blocked, "the run ended before S11 was judged: "+result.Reason
		}
		return result
	}
	result.Limits = map[string]string{backupLimitName: h.cfg.BackupLimit.String()}
	result.Measurements = map[string]any{measBackupCompletedWithin: evidence.Unknown,
		measObjectsVerified: evidence.Unknown, measArtifactBytes: evidence.Unknown,
		measObjectsNotInManifest: evidence.Unknown}
	rec.Credentials = &s11Credentials{FoundIn: []string{}}
	if _, err := h.holdsCredential(nil); err != nil {
		return end(evidence.Blocked, "%v; S11 could not tell whether a credential is in a status, the manifest or "+
			"an Event", err)
	}
	known, err := h.knownPoint(ctx)
	rec.KnownPoint = known
	if err != nil {
		return end(evidence.Blocked, "record the known point: %v", err)
	}
	backup, err := h.createBackup(ctx, s11BackupName, haClusterName, h.cfg.BackupBucket, rec.Prefix)
	if err != nil {
		return end(evidence.Blocked, "%v", err)
	}
	requested := time.Now().UTC()
	rec.BackupUID, rec.RequestedAt = string(backup.UID), &requested
	backup, err = h.waitBackup(ctx, backup, &rec.Phases, rec.Credentials)
	if found := rec.Credentials.FoundIn; len(found) > 0 &&
		(err != nil || backup.Status.Phase != databasev1alpha1.BackupPhaseCompleted) {
		// A credential in a status must never happen, whatever came after it.
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
		result.Result, result.Reason = evidence.Fail, result.NeverEvents[0]+"; the backup did not complete: "+
			cmp.Or(errorText(err), readyReason(backup))
		return result
	}
	// What the backup was seen to do is a verdict, also when the run ends
	// right after: it is not routed through end.
	switch {
	case errors.As(err, new(backupVerdict)):
		result.Result, result.Reason = evidence.Fail, err.Error()
		return result
	case err != nil:
		// The backup could not be read: S11 could not be judged.
		return end(evidence.Blocked, "read the CubridBackup: %v", err)
	case backup.Status.Phase == databasev1alpha1.BackupPhaseFailed:
		result.Result, result.Reason = evidence.Fail, "the backup ended Failed: "+readyReason(backup)
		return result
	}
	within := rec.Phases[len(rec.Phases)-1].At.Sub(requested)
	rec.CompletedWithin = within.Round(time.Millisecond).String()
	result.Measurements[measBackupCompletedWithin] = rec.CompletedWithin
	if done := backup.Status.CompletedAt; done != nil {
		rec.StatusCompletedWithin = done.Sub(backup.CreationTimestamp.Time).String()
	}
	rec.Artifact = backup.Status.Artifact
	if rec.Artifact == nil {
		result.NeverEvents = append(result.NeverEvents, "the backup is Completed without an artifact in its status")
		return end(evidence.Fail, "%s", strings.Join(result.NeverEvents, "; "))
	}
	manifest, verifyErr := h.verifyS11Artifact(ctx, backup, &rec)
	if manifest != nil {
		if err := h.write(s11ManifestFile, manifest); err != nil {
			writeErr = fmt.Errorf("write the manifest: %w", err)
		}
	}
	v := rec.Verification
	if v.ObjectsChecked {
		result.Measurements[measObjectsVerified] = v.ObjectsVerified
		result.Measurements[measArtifactBytes] = v.BytesDownloaded
	}
	if v.Listed {
		result.Measurements[measObjectsNotInManifest] = len(v.NotInManifest)
	}
	if len(v.Unreadable) > 0 {
		result.NeverEvents = append(result.NeverEvents, "the backup is Completed, and objects of its artifact could "+
			"not be read: "+strings.Join(v.Unreadable, "; "))
	}
	credErr := h.checkS11Credentials(ctx, backup, manifest, rec.Credentials)
	logErr := h.checkS11Logs(ctx, haClusterName, false, lastTarget(rec.Phases),
		requestedOnServer(backup, requested), started, rec.Credentials, h.rec.OperatorAfter.Pods)
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	// What was not checked keeps a confirmed failure a failure, named after
	// it; alone, it blocks S11.
	var unchecked string
	if verifyErr != nil {
		unchecked = "verify the artifact: " + verifyErr.Error()
	}
	if credErr != nil {
		unchecked = strings.TrimPrefix(unchecked+"; look for credentials: "+credErr.Error(), "; ")
	}
	if logErr != nil {
		unchecked = strings.TrimPrefix(unchecked+"; search the logs: "+strings.ReplaceAll(logErr.Error(), "\n", "; "),
			"; ")
	}
	failure := strings.Join(result.NeverEvents, "; ")
	if len(v.Problems) > 0 {
		failure = strings.TrimPrefix(failure+"; the artifact does not match: "+strings.Join(v.Problems, "; "), "; ")
	}
	switch {
	case failure != "":
		result.Result, result.Reason = evidence.Fail, strings.TrimSuffix(failure+"; "+unchecked, "; ")
		return result
	case unchecked != "":
		return end(evidence.Blocked, "%s", unchecked)
	}
	result.Result = evidence.Pass
	result.Reason = "the backup ended Completed within backup_limit, and every object the manifest names was " +
		"downloaded by the run and matches it, the status and the cluster; no credential was found in the " +
		"recorded statuses, the manifest, the Events, the installed Operator's manager log or the Instance " +
		"Manager's log in the backup's target Pod, each log read from " + logSinceMargin.String() + " before S11 " +
		"complete started; the Instance Managers of the other members were not searched"
	rec.Notes = append(rec.Notes, logsNote(h.logLimit()))
	return result
}

// credentialEvent is the never-event of the places a credential was found.
func credentialEvent(found []string) string {
	return "a credential value of the object store is in " + strings.Join(found, ", ")
}

// errorText is err's text, or empty for no error.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// knownPoint records the client history's position and writes the marker
// row through the read-write Service.
func (h *haRun) knownPoint(ctx context.Context) (*s11KnownPoint, error) {
	point := &s11KnownPoint{Marker: s11Marker, MarkerValue: h.rec.RunID}
	events, err := workload.ReadEvents(strings.NewReader(h.history.String()))
	if err != nil {
		return point, err
	}
	for _, e := range events {
		if e.Event == workload.Acknowledged {
			point.Acknowledged++
			point.LastAcknowledged = e.OpID
		}
	}
	out, err := h.clientRun(ctx, fmt.Sprintf("workload marker %s %s 2>&1", s11Marker, h.rec.RunID))
	if err != nil {
		return point, fmt.Errorf("write the marker row: %w\n%s", err, out)
	}
	if !strings.Contains(out, "marker "+s11Marker+" written") {
		return point, fmt.Errorf("the marker row was not acknowledged: %s", strings.TrimSpace(out))
	}
	point.MarkerWrittenAt = time.Now().UTC()
	return point, nil
}

// createBackup requests the CubridBackup name, a full backup of the run's
// database on cluster to bucket under prefix. The target is left to its
// default, PreferStandby. The Operator reaches the store with the cluster's
// spec.objectStorage: its endpoint and its Secret, through the members'
// environment.
func (h *haRun) createBackup(ctx context.Context, name, cluster, bucket, prefix string) (
	*databasev1alpha1.CubridBackup, error) {
	if err := h.guard(ctx); err != nil {
		return nil, err
	}
	backup := &databasev1alpha1.CubridBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.cfg.Namespace},
		Spec: databasev1alpha1.CubridBackupSpec{ClusterRef: databasev1alpha1.LocalObjectRef{Name: cluster},
			Database: databaseName, Destination: databasev1alpha1.CubridBackupDestination{
				Type:          databasev1alpha1.DestinationObjectStorage,
				ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: bucket, Prefix: prefix}}}}
	if err := h.Client.Create(ctx, backup); err != nil {
		return nil, fmt.Errorf("create the CubridBackup: %w", err)
	}
	if backup.UID == "" {
		return nil, errors.New("created CubridBackup has no observed UID")
	}
	return backup, nil
}

// waitBackup reads the backup until it is Completed or Failed, recording
// each change in phases and searching every status read for a credential in
// creds, for at most BackupLimit from the request's return. Passing the limit
// is a failure: it is the contract's limit.
// backupVerdict is an error that judges the backup, as against one that
// kept the run from reading it.
type backupVerdict struct{ error }

func (h *haRun) waitBackup(ctx context.Context, created *databasev1alpha1.CubridBackup, phases *[]s11Phase,
	creds *s11Credentials) (*databasev1alpha1.CubridBackup, error) {
	return h.waitBackupWith(ctx, created, phases, creds, nil)
}

// waitBackupWith is waitBackup that passes every status read, once it was
// recorded and searched, to each, unless each is nil; an error of each ends
// the wait with it.
func (h *haRun) waitBackupWith(ctx context.Context, created *databasev1alpha1.CubridBackup, phases *[]s11Phase,
	creds *s11Credentials, each func(context.Context, *databasev1alpha1.CubridBackup) error) (
	*databasev1alpha1.CubridBackup, error) {
	wctx, cancel := context.WithTimeout(ctx, h.cfg.BackupLimit)
	defer cancel()
	backup := &databasev1alpha1.CubridBackup{}
	// readErr is the last reading's error, nil once a reading succeeded.
	var readErr error
	err := pollUntil(wctx, h.cfg.Poll, func() (bool, error) {
		if err := h.Client.Get(wctx, client.ObjectKeyFromObject(created), backup); err != nil {
			if wctx.Err() != nil {
				// The limit, or the run, ended this reading.
				return false, nil
			}
			if retryable(err) {
				readErr = err
				return false, nil
			}
			return false, err
		}
		readErr = nil
		if backup.UID != created.UID {
			return false, backupVerdict{errors.New("the CubridBackup was replaced")}
		}
		if err := h.recordBackup(backup, phases, creds); err != nil {
			return false, err
		}
		s := backup.Status
		if each != nil {
			if err := each(wctx, backup); err != nil {
				return false, err
			}
		}
		return s.Phase == databasev1alpha1.BackupPhaseCompleted || s.Phase == databasev1alpha1.BackupPhaseFailed, nil
	})
	if err != nil && ctx.Err() == nil && wctx.Err() != nil && readErr != nil {
		// The phase at the limit is not known: the run could not judge.
		return backup, fmt.Errorf("the CubridBackup could not be read before backup_limit %s passed: %w",
			h.cfg.BackupLimit, readErr)
	}
	if err != nil && ctx.Err() == nil && wctx.Err() != nil {
		return backup, backupVerdict{fmt.Errorf("%w %s; its last phase was %q", errBackupLimitPassed,
			h.cfg.BackupLimit, backup.Status.Phase)}
	}
	return backup, err
}

// errBackupLimitPassed starts the verdict of a wait whose backup was neither
// Completed nor Failed within backup_limit.
var errBackupLimitPassed = errors.New("the backup was neither Completed nor Failed within backup_limit")

// recordBackup records a reading of backup in phases when it differs from
// the last one recorded, and searches its whole status, not only what a
// phase records, for a credential in creds.
func (h *haRun) recordBackup(backup *databasev1alpha1.CubridBackup, phases *[]s11Phase,
	creds *s11Credentials) error {
	s := backup.Status
	seen := s11Phase{At: time.Now().UTC(), Phase: string(s.Phase), OperationRef: s.OperationRef,
		TargetInstance: s.TargetInstance, TargetRole: s.TargetRole, FallbackUsed: s.FallbackUsed,
		Conditions: s.Conditions}
	if seen.Conditions == nil {
		seen.Conditions = []metav1.Condition{}
	}
	if n := len(*phases); n == 0 || !samePhase((*phases)[n-1], seen) {
		*phases = append(*phases, seen)
	}
	creds.StatusesChecked++
	return h.lookForCredentials(&creds.FoundIn, "the status read in phase "+string(s.Phase), s)
}

func samePhase(a, b s11Phase) bool {
	a.At, b.At = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// readyReason is the reason and message of the backup's Ready condition.
func readyReason(backup *databasev1alpha1.CubridBackup) string {
	for _, c := range backup.Status.Conditions {
		if c.Type == backupReady {
			return c.Reason + ": " + c.Message
		}
	}
	return "no Ready condition"
}

// verifyS11Artifact downloads the artifact through a port-forward to the
// object store with the run's own client, records what it found in
// rec.Verification and returns manifest.json as stored. Only an answer that
// the store holds no such object shows an unreadable artifact; any other
// error, and any once ctx has ended, kept the run from checking and is
// returned.
func (h *haRun) verifyS11Artifact(ctx context.Context, backup *databasev1alpha1.CubridBackup, rec *s11Record) (
	[]byte, error) {
	v := &s11Verification{NotInManifest: []string{}, Unreadable: []string{}, Problems: []string{}}
	rec.Verification = v
	artifact := backup.Status.Artifact
	key, err := artifactKey(artifact.URI, h.cfg.BackupBucket,
		path.Join(rec.Prefix, h.sourceOf(rec).clusterUID, databaseName, string(backup.UID)))
	if err != nil {
		v.Problems = append(v.Problems, err.Error())
		return nil, nil
	}
	v.ManifestKey = key
	store, stop, err := h.openS11Store(ctx)
	if err != nil {
		return nil, err
	}
	defer stop()
	manifest, err := download(ctx, store, h.cfg.BackupBucket, key)
	switch {
	case errors.Is(err, errManifestTooLarge):
		// A limit of the run, not of the contract: the run could not check.
		return nil, fmt.Errorf("read %s: %w", key, err)
	case err != nil:
		return nil, unreadable(ctx, v, key, err)
	}
	sum := sha256.Sum256(manifest)
	v.ManifestSHA256 = hex.EncodeToString(sum[:])
	parsed, err := instancemanager.ParseManifest(manifest)
	if err != nil {
		v.Problems = append(v.Problems, err.Error())
		return manifest, nil
	}
	v.ManifestDigest, _ = parsed.Digest()
	v.Objects, v.ManifestSizeBytes = len(parsed.Objects), parsed.TotalSize()
	v.Problems = append(v.Problems, h.checkManifestIdentity(parsed, backup, h.sourceOf(rec))...)
	v.Problems = append(v.Problems, checkManifestIntegrity(parsed, artifact, v)...)
	return manifest, h.verifyS11Objects(ctx, store, parsed, path.Dir(key), v)
}

// unreadable records an object the store answered it does not hold, and
// returns any other error: the run could not check the object.
func unreadable(ctx context.Context, v *s11Verification, key string, err error) error {
	if ctx.Err() != nil || !errors.Is(err, errObjectNotFound) {
		return fmt.Errorf("read %s: %w", key, err)
	}
	v.Unreadable = append(v.Unreadable, fmt.Sprintf("%s could not be read: %v", key, err))
	return nil
}

// verifyS11Objects streams every object the manifest names and lists the
// attempt prefix. It stops at the first error that is not a missing object.
func (h *haRun) verifyS11Objects(ctx context.Context, store objectStore, m instancemanager.BackupManifest,
	attempt string, v *s11Verification) error {
	v.ObjectsChecked = true
	listed := map[string]bool{}
	for _, obj := range m.Objects {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if listed[obj.Key] {
			v.Problems = append(v.Problems, fmt.Sprintf("the manifest lists object key %q twice", obj.Key))
			continue
		}
		listed[obj.Key] = true
		if !validObjectKey(obj.Key) {
			v.Problems = append(v.Problems, fmt.Sprintf("manifest object key %q is not a clean path below backup/",
				obj.Key))
			continue
		}
		key := attempt + "/" + obj.Key
		got, size, err := downloadSum(ctx, store, h.cfg.BackupBucket, key)
		v.BytesDownloaded += size
		switch {
		case err != nil:
			if err := unreadable(ctx, v, key+", named in the manifest,", err); err != nil {
				return err
			}
		case size != obj.SizeBytes || got != obj.SHA256:
			v.Problems = append(v.Problems, fmt.Sprintf("%s has %d bytes and sha256 %s, the manifest %d and %s",
				key, size, got, obj.SizeBytes, obj.SHA256))
		default:
			v.ObjectsVerified++
		}
	}
	keys, err := store.List(ctx, h.cfg.BackupBucket, attempt+"/")
	if err != nil {
		return fmt.Errorf("list %s: %w", attempt, err)
	}
	v.Listed = true
	// An object the manifest does not name is recorded, not failed: a
	// restore reads only the objects the manifest names (ADR-0007), and an
	// earlier, interrupted upload into the same attempt is not ruled out
	// here. The never-happen is the opposite, a named object that is absent.
	for _, k := range keys {
		if rel := strings.TrimPrefix(k, attempt+"/"); rel != instancemanager.ManifestObjectName && !listed[rel] {
			v.NotInManifest = append(v.NotInManifest, k)
		}
	}
	return nil
}

// openS11Store opens the port-forward and the run's client with the seed
// Secret's keys; stop ends the port-forward.
func (h *haRun) openS11Store(ctx context.Context) (objectStore, func(), error) {
	return h.openStoreWith(ctx, h.accessKey, h.secretKey)
}

// openStoreWith opens the port-forward and the run's client with accessKey
// and secretKey; stop ends the port-forward.
func (h *haRun) openStoreWith(ctx context.Context, accessKey, secretKey string) (objectStore, func(), error) {
	if h.forward == nil {
		return nil, nil, errors.New("no port-forward to the object store is available")
	}
	namespace, service, port, err := splitService(h.cfg.ObjectStoreService)
	if err != nil {
		return nil, nil, err
	}
	if err := h.sameObjectStore(ctx, namespace, service, port); err != nil {
		return nil, nil, err
	}
	endpoint, stop, err := h.forward(ctx, namespace, service, port)
	if err != nil {
		return nil, nil, fmt.Errorf("port-forward to the object store: %w", err)
	}
	open := h.openStore
	if open == nil {
		open = newMinioStore
	}
	store, err := open(endpoint, accessKey, secretKey, !h.cfg.SeedInsecure)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("open the object store client: %w", err)
	}
	return store, stop, nil
}

// sameObjectStore refuses an object store Service that is not the one the
// cluster's spec.objectStorage endpoint names: the run would check another
// store than the backup wrote to. That is a setup error, never a failure of
// the backup.
func (h *haRun) sameObjectStore(ctx context.Context, namespace, service, port string) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return err
	}
	storage := cluster.Spec.ObjectStorage
	if storage == nil {
		return errors.New("the cluster has no spec.objectStorage")
	}
	host, endpointPort, err := net.SplitHostPort(storage.Endpoint)
	if err != nil {
		// No port: the scheme's.
		host, endpointPort = storage.Endpoint, "443"
		if storage.Insecure {
			endpointPort = "80"
		}
	}
	names := []string{service + "." + namespace, service + "." + namespace + ".svc",
		service + "." + namespace + ".svc.cluster.local"}
	if !slices.Contains(names, strings.TrimSuffix(host, ".")) || endpointPort != port {
		return fmt.Errorf("-object-store-service %s/%s:%s is not the Service of the cluster's spec.objectStorage "+
			"endpoint %s; the run would check another store than the backup wrote to", namespace, service, port,
			storage.Endpoint)
	}
	return nil
}

// checkManifestIdentity compares what the manifest says of the backup with
// its status, src, the cluster it is of, and the engine. The run checks what
// instancemanager's Verify would, itself, so that the Operator's own
// verification is not what is tested.
func (h *haRun) checkManifestIdentity(m instancemanager.BackupManifest,
	backup *databasev1alpha1.CubridBackup, src s11Source) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	s, artifact := backup.Status, backup.Status.Artifact
	if m.ManifestVersion != instancemanager.ManifestVersion {
		add("manifestVersion is %d, not %d", m.ManifestVersion, instancemanager.ManifestVersion)
	}
	if m.Database != databaseName || artifact.Database != databaseName {
		add("the manifest names database %q and the status %q, not %s", m.Database, artifact.Database, databaseName)
	}
	if m.ClusterUID != src.clusterUID || string(s.ClusterUID) != src.clusterUID {
		add("the manifest's clusterUID is %q and the status's %q, not the cluster's %s", m.ClusterUID,
			s.ClusterUID, src.clusterUID)
	}
	if m.Level != 0 || artifact.Level != databasev1alpha1.CubridBackupFull {
		add("the manifest's level is %d and the status's %q, not a full backup", m.Level, artifact.Level)
	}
	if m.SourceInstance != s.TargetInstance {
		add("the manifest's sourceInstance %q is not status.targetInstance %q", m.SourceInstance, s.TargetInstance)
	}
	if !slices.Contains(src.members, m.SourceInstance) {
		add("the manifest's sourceInstance %q is not a member of the cluster", m.SourceInstance)
	}
	// PreferStandby runs on a slave, or on the master when it fell back; a
	// standalone member is selected in the role src names.
	role := src.role
	switch {
	case role != "":
	case s.FallbackUsed:
		role = string(databasev1alpha1.RoleMaster)
	default:
		role = string(databasev1alpha1.RoleSlave)
	}
	if m.SourceRole != s.TargetRole || m.SourceRole != role {
		add("the manifest's sourceRole %q, status.targetRole %q and fallbackUsed %t do not agree", m.SourceRole,
			s.TargetRole, s.FallbackUsed)
	}
	// The manifest's cubridVersion is the engine version the backup's member
	// observed (#621, ADR-0011): every member runs the same image, so it is
	// the version cubrid_rel reported on the master, of the cluster's
	// spec.version series.
	if !strings.HasPrefix(m.CubridVersion, haCubridVersion+".") {
		add("the manifest's cubridVersion %q is not of the cluster's spec.version %s", m.CubridVersion, haCubridVersion)
	}
	if m.CubridVersion != h.engineVersion {
		add("the manifest's cubridVersion %q is not the engine version %s", m.CubridVersion, h.engineVersion)
	}
	// The controller copies the operation's version into the status (#621),
	// so an empty one is a mismatch too.
	if artifact.CubridVersion != m.CubridVersion {
		add("status.artifact.cubridVersion is %q, the manifest's %q", artifact.CubridVersion, m.CubridVersion)
	}
	return problems
}

// checkManifestIntegrity compares the manifest's digest and sizes with the
// status.
func checkManifestIntegrity(m instancemanager.BackupManifest, artifact *databasev1alpha1.CubridBackupArtifact,
	v *s11Verification) []string {
	var problems []string
	// UploadBackup stores the canonical encoding, so both must be the digest.
	if v.ManifestDigest != artifact.ManifestDigest {
		problems = append(problems, fmt.Sprintf("status.artifact.manifestDigest is %s, the manifest's digest %s",
			artifact.ManifestDigest, v.ManifestDigest))
	}
	if v.ManifestSHA256 != artifact.ManifestDigest {
		problems = append(problems, fmt.Sprintf("status.artifact.manifestDigest is %s, the stored file's sha256 %s",
			artifact.ManifestDigest, v.ManifestSHA256))
	}
	if m.TotalSize() != artifact.SizeBytes {
		problems = append(problems, fmt.Sprintf("status.artifact.sizeBytes is %d, the manifest's objects hold %d",
			artifact.SizeBytes, m.TotalSize()))
	}
	if len(m.Objects) == 0 {
		problems = append(problems, "the manifest lists no objects")
	}
	return problems
}

// attemptSegment is the last directory of an artifact's key
// (instancemanager.BackupAttemptPrefix).
var attemptSegment = regexp.MustCompile(fmt.Sprintf(`^attempt-[0-9a-f]{%d}$`, instancemanager.BackupAttemptIDLength))

// artifactKey returns the manifest's key of an s3://<bucket>/<key> URI,
// which must be <dir>/attempt-<id>/manifest.json in bucket. dir is the
// backup's own prefix, <prefix>/<clusterUID>/<database>/<backupUID>, as the
// controller writes it (backupArtifactPrefix).
func artifactKey(uri, bucket, dir string) (string, error) {
	rest, s3 := strings.CutPrefix(uri, "s3://")
	inBucket, key, _ := strings.Cut(rest, "/")
	if !s3 || inBucket != bucket {
		return "", fmt.Errorf("the artifact URI %q is not in bucket %s", uri, bucket)
	}
	attempt := path.Dir(key)
	if path.Clean(key) != key || path.Dir(attempt) != dir || path.Base(key) != instancemanager.ManifestObjectName {
		return "", fmt.Errorf("the artifact URI %q is not a manifest.json of this backup under the run's prefix, %s",
			uri, dir)
	}
	if !attemptSegment.MatchString(path.Base(attempt)) {
		return "", fmt.Errorf("the artifact URI %q does not end in attempt-<%d hex digits>/manifest.json", uri,
			instancemanager.BackupAttemptIDLength)
	}
	return key, nil
}

// validObjectKey reports a clean relative key below backup/.
func validObjectKey(key string) bool {
	rest, below := strings.CutPrefix(key, "backup/")
	return below && rest != "" && path.Clean(key) == key && !strings.ContainsAny(key, "\\\x00") &&
		!slices.Contains(strings.Split(key, "/"), "..")
}

// errManifestTooLarge is a manifest over maxManifestSize.
var errManifestTooLarge = errors.New("the manifest is too large")

// download reads a whole manifest, at most maxManifestSize bytes.
func download(ctx context.Context, store objectStore, bucket, key string) ([]byte, error) {
	r, err := store.Get(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, maxManifestSize+1))
	if err == nil && int64(len(data)) > maxManifestSize {
		return nil, fmt.Errorf("%w: larger than %d bytes", errManifestTooLarge, maxManifestSize)
	}
	return data, err
}

// downloadSum streams an object and returns its sha256 and size.
func downloadSum(ctx context.Context, store objectStore, bucket, key string) (string, int64, error) {
	r, err := store.Get(ctx, bucket, key)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = r.Close() }()
	hash := sha256.New()
	n, err := io.Copy(hash, r)
	return hex.EncodeToString(hash.Sum(nil)), n, err
}

// lookForCredentials adds where to found when v, as JSON, holds a key of the
// object store in any encoding the evidence redactor removes
// (evidence.SecretForms).
func (h *haRun) lookForCredentials(found *[]string, where string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	holds, err := h.holdsCredential(data)
	if err != nil {
		return err
	}
	if holds && !slices.Contains(*found, where) {
		*found = append(*found, where)
	}
	return nil
}

// holdsCredential reports whether data holds a key of the object store: a
// key of the seed Secret or, once S11 denied-credentials or signature-mismatch made
// them, a key of that variant. The access key of S11 signature-mismatch is the
// seed Secret's, so its forms are searched for twice; a place is recorded
// once all the same (lookForCredentials). A key too short for
// evidence.SecretForms cannot be searched for: the run does not claim it
// found none.
func (h *haRun) holdsCredential(data []byte) (bool, error) {
	access, secret := evidence.SecretForms(h.accessKey), evidence.SecretForms(h.secretKey)
	if len(access) == 0 || len(secret) == 0 {
		return false, errors.New("a key of the seed Secret is too short to be searched for")
	}
	forms := append(access, secret...)
	for _, key := range h.deniedKeys {
		denied := evidence.SecretForms(key)
		if len(denied) == 0 {
			return false, errors.New("a denied key is too short to be searched for")
		}
		forms = append(forms, denied...)
	}
	return slices.ContainsFunc(forms, func(form string) bool { return strings.Contains(string(data), form) }), nil
}

// checkS11Credentials looks for the object store's keys in the backup's
// status as read now, in the manifest and in every Event of the run
// namespace, adding to what waitBackup found in each status it read. The
// logs are searched by checkS11Logs.
func (h *haRun) checkS11Credentials(ctx context.Context, created *databasev1alpha1.CubridBackup, manifest []byte,
	found *s11Credentials) error {
	if manifest != nil {
		found.ManifestChecked = true
		// As stored, not as JSON text of it.
		holds, err := h.holdsCredential(manifest)
		if err != nil {
			return err
		}
		if holds {
			found.FoundIn = append(found.FoundIn, "manifest.json")
		}
	}
	backup := &databasev1alpha1.CubridBackup{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), backup); err != nil {
		return err
	}
	if err := h.lookForCredentials(&found.FoundIn, "the CubridBackup's status", backup.Status); err != nil {
		return err
	}
	found.StatusChecked = true
	events := &corev1.EventList{}
	if err := h.Client.List(ctx, events, client.InNamespace(h.cfg.Namespace)); err != nil {
		return fmt.Errorf("list the Events: %w", err)
	}
	for _, event := range events.Items {
		found.EventsChecked++
		if event.InvolvedObject.UID == created.UID {
			found.BackupEvents++
		}
		if err := h.lookForCredentials(&found.FoundIn, "Event "+event.Name, event); err != nil {
			return err
		}
	}
	return nil
}
