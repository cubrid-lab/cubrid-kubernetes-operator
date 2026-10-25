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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

// s3Bucket is the bucket rule of a CubridBackup's destination; two adjacent
// dots are refused separately.
var s3Bucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// validateS11 checks the inputs of S11 complete, when it is selected.
func (c HAConfig) validateS11() error {
	if c.BackupBucket == "" {
		return nil
	}
	if c.Fault != "" {
		return errors.New("-backup-bucket and -fault each select a scenario after LAB01; a run takes one of them")
	}
	if !s3Bucket.MatchString(c.BackupBucket) || strings.Contains(c.BackupBucket, "..") || c.BackupLimit <= 0 {
		return errors.New("S11 needs an S3 bucket name for the backups and a positive backup limit")
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

// objectStore reads an S3-compatible store. S11 verifies the artifact with
// a client of its own, not through the Operator or the Instance Manager.
type objectStore interface {
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	List(ctx context.Context, bucket, prefix string) ([]string, error)
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

// Get opens an object; a missing object fails at the first read.
func (s minioStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return s.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
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
	// Unreadable lists the manifest and the objects it names that could not
	// be read: what must never happen.
	Unreadable []string `json:"unreadable"`
	Problems   []string `json:"problems"`
}

// s11Credentials is where the run looked for the object store's keys.
type s11Credentials struct {
	StatusChecked   bool     `json:"statusChecked"`
	ManifestChecked bool     `json:"manifestChecked"`
	EventsChecked   int      `json:"eventsChecked"`
	BackupEvents    int      `json:"backupEvents"`
	FoundIn         []string `json:"foundIn"`
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
}

// runS11 runs S11 complete on the cluster LAB01 left in its checked state: a
// marker row is written, a full backup of the run's database is requested
// and must end Completed within BackupLimit, and the run downloads the
// artifact with its own client and checks it against the manifest and the
// status. No credential of the object store may be in the backup's status,
// the manifest or the Events of the run namespace. A run that ends before
// S11 is judged is blocked, not failed.
func (h *haRun) runS11(ctx context.Context) (result evidence.Scenario) {
	result = evidence.Scenario{ID: s11ID, Variant: s11Variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s11Record{Backup: s11BackupName, Bucket: h.cfg.BackupBucket, Prefix: h.rec.Seed.Prefix + "/backups",
		ObjectStoreService: h.cfg.ObjectStoreService, Phases: []s11Phase{}, EngineVersion: h.engineVersion,
		Residue: "the artifact under the prefix is left in the bucket for S12; this run does not remove it"}
	defer func() {
		if err := h.writeJSON(s11RecordFile, rec); err != nil {
			result.Result, result.Reason = evidence.Fail, "write S11 evidence: "+err.Error()
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
	result.Limits = map[string]string{"backup_limit": h.cfg.BackupLimit.String()}
	result.Measurements = map[string]any{"backupCompletedWithin": evidence.Unknown,
		"objectsVerified": evidence.Unknown, "artifactBytes": evidence.Unknown}
	known, err := h.knownPoint(ctx)
	rec.KnownPoint = known
	if err != nil {
		return end(evidence.Blocked, "record the known point: %v", err)
	}
	backup, err := h.createBackup(ctx, rec.Prefix)
	if err != nil {
		return end(evidence.Blocked, "%v", err)
	}
	requested := time.Now().UTC()
	rec.BackupUID, rec.RequestedAt = string(backup.UID), &requested
	backup, err = h.waitBackup(ctx, backup, &rec)
	if err != nil {
		return end(evidence.Fail, "%v", err)
	}
	if backup.Status.Phase == databasev1alpha1.BackupPhaseFailed {
		return end(evidence.Fail, "the backup ended Failed: %s", readyReason(backup))
	}
	within := rec.Phases[len(rec.Phases)-1].At.Sub(requested)
	rec.CompletedWithin = within.Round(time.Millisecond).String()
	result.Measurements["backupCompletedWithin"] = rec.CompletedWithin
	if done := backup.Status.CompletedAt; done != nil {
		rec.StatusCompletedWithin = done.Sub(backup.CreationTimestamp.Time).String()
	}
	rec.Artifact = backup.Status.Artifact
	if rec.Artifact == nil {
		result.NeverEvents = append(result.NeverEvents, "the backup is Completed without an artifact in its status")
		return end(evidence.Fail, "%s", strings.Join(result.NeverEvents, "; "))
	}
	manifest, err := h.verifyS11Artifact(ctx, backup, &rec)
	if err != nil {
		return end(evidence.Blocked, "verify the artifact: %v", err)
	}
	if manifest != nil {
		if err := h.write(s11ManifestFile, manifest); err != nil {
			return end(evidence.Fail, "write the manifest: %v", err)
		}
	}
	v := rec.Verification
	result.Measurements["objectsVerified"], result.Measurements["artifactBytes"] = v.ObjectsVerified, v.BytesDownloaded
	result.Measurements["objectsNotInManifest"] = len(v.NotInManifest)
	if len(v.Unreadable) > 0 {
		result.NeverEvents = append(result.NeverEvents, "the backup is Completed, and objects of its artifact could "+
			"not be read: "+strings.Join(v.Unreadable, "; "))
	}
	rec.Credentials, err = h.checkS11Credentials(ctx, backup, manifest)
	if err != nil {
		return end(evidence.Blocked, "look for credentials: %v", err)
	}
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, "a credential value of the object store is in "+
			strings.Join(found, ", "))
	}
	if len(result.NeverEvents) > 0 {
		return end(evidence.Fail, "%s", strings.Join(result.NeverEvents, "; "))
	}
	if len(v.Problems) > 0 {
		return end(evidence.Fail, "the artifact does not match: %s", strings.Join(v.Problems, "; "))
	}
	result.Result = evidence.Pass
	result.Reason = "the backup ended Completed within backup_limit, and every object the manifest names was " +
		"downloaded by the run and matches it, the status and the cluster; no credential was found"
	return result
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

// createBackup requests a full backup of the run's database to BackupBucket
// under prefix. The target is left to its default, PreferStandby. The
// Operator reaches the store with the cluster's spec.objectStorage: its
// endpoint and the copied Secret, through the members' environment.
func (h *haRun) createBackup(ctx context.Context, prefix string) (*databasev1alpha1.CubridBackup, error) {
	if err := h.guard(ctx); err != nil {
		return nil, err
	}
	backup := &databasev1alpha1.CubridBackup{
		ObjectMeta: metav1.ObjectMeta{Name: s11BackupName, Namespace: h.cfg.Namespace},
		Spec: databasev1alpha1.CubridBackupSpec{ClusterRef: databasev1alpha1.LocalObjectRef{Name: haClusterName},
			Database: databaseName, Destination: databasev1alpha1.CubridBackupDestination{
				Type: databasev1alpha1.DestinationObjectStorage,
				ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: h.cfg.BackupBucket,
					Prefix: prefix}}}}
	if err := h.Client.Create(ctx, backup); err != nil {
		return nil, fmt.Errorf("create the CubridBackup: %w", err)
	}
	if backup.UID == "" {
		return nil, errors.New("created CubridBackup has no observed UID")
	}
	return backup, nil
}

// waitBackup reads the backup until it is Completed or Failed, recording
// each change, for at most BackupLimit from the request's return. Passing
// the limit is a failure: it is the contract's limit.
func (h *haRun) waitBackup(ctx context.Context, created *databasev1alpha1.CubridBackup, rec *s11Record) (
	*databasev1alpha1.CubridBackup, error) {
	wctx, cancel := context.WithTimeout(ctx, h.cfg.BackupLimit)
	defer cancel()
	backup := &databasev1alpha1.CubridBackup{}
	err := pollUntil(wctx, h.cfg.Poll, func() (bool, error) {
		if err := h.Client.Get(wctx, client.ObjectKeyFromObject(created), backup); err != nil {
			if retryable(err) {
				return false, nil
			}
			return false, err
		}
		if backup.UID != created.UID {
			return false, errors.New("the CubridBackup was replaced")
		}
		s := backup.Status
		seen := s11Phase{At: time.Now().UTC(), Phase: string(s.Phase), TargetInstance: s.TargetInstance,
			TargetRole: s.TargetRole, FallbackUsed: s.FallbackUsed, Conditions: s.Conditions}
		if seen.Conditions == nil {
			seen.Conditions = []metav1.Condition{}
		}
		if n := len(rec.Phases); n == 0 || !samePhase(rec.Phases[n-1], seen) {
			rec.Phases = append(rec.Phases, seen)
		}
		return s.Phase == databasev1alpha1.BackupPhaseCompleted || s.Phase == databasev1alpha1.BackupPhaseFailed, nil
	})
	if err != nil && ctx.Err() == nil && wctx.Err() != nil {
		return backup, fmt.Errorf("the backup was neither Completed nor Failed within backup_limit %s; its last "+
			"phase was %q", h.cfg.BackupLimit, backup.Status.Phase)
	}
	return backup, err
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
// rec.Verification and returns manifest.json as stored. An error is what
// kept the run from checking: the port-forward or the client.
func (h *haRun) verifyS11Artifact(ctx context.Context, backup *databasev1alpha1.CubridBackup, rec *s11Record) (
	[]byte, error) {
	v := &s11Verification{NotInManifest: []string{}, Unreadable: []string{}, Problems: []string{}}
	rec.Verification = v
	artifact := backup.Status.Artifact
	key, err := artifactKey(artifact.URI, h.cfg.BackupBucket, rec.Prefix)
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
	manifest, _, err := download(ctx, store, h.cfg.BackupBucket, key)
	if err != nil {
		v.Unreadable = append(v.Unreadable, fmt.Sprintf("%s could not be read: %v", key, err))
		return nil, nil
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
	v.Problems = append(v.Problems, h.checkManifest(parsed, backup, v)...)
	if artifact.CubridVersion == "" {
		rec.Notes = append(rec.Notes, "status.artifact.cubridVersion is not reported; the manifest's cubridVersion "+
			"was compared with the cluster's spec.version and the engine version instead")
	}
	attempt := path.Dir(key)
	listed := map[string]bool{}
	for _, obj := range parsed.Objects {
		listed[obj.Key] = true
		if !validObjectKey(obj.Key) {
			v.Problems = append(v.Problems, fmt.Sprintf("manifest object key %q is not a clean path below backup/",
				obj.Key))
			continue
		}
		data := attempt + "/" + obj.Key
		got, size, err := downloadSum(ctx, store, h.cfg.BackupBucket, data)
		v.BytesDownloaded += size
		switch {
		case err != nil:
			v.Unreadable = append(v.Unreadable, fmt.Sprintf("%s, named in the manifest, could not be read: %v", data, err))
		case size != obj.SizeBytes || got != obj.SHA256:
			v.Problems = append(v.Problems, fmt.Sprintf("%s has %d bytes and sha256 %s, the manifest %d and %s",
				data, size, got, obj.SizeBytes, obj.SHA256))
		default:
			v.ObjectsVerified++
		}
	}
	keys, err := store.List(ctx, h.cfg.BackupBucket, attempt+"/")
	if err != nil {
		v.Problems = append(v.Problems, fmt.Sprintf("list %s: %v", attempt, err))
	}
	// An object the manifest does not name is recorded, not failed: a
	// restore reads only the objects the manifest names (ADR-0007), and an
	// earlier, interrupted upload into the same attempt is not ruled out
	// here. The never-happen is the opposite, a named object that is absent.
	for _, k := range keys {
		if rel := strings.TrimPrefix(k, attempt+"/"); rel != instancemanager.ManifestObjectName && !listed[rel] {
			v.NotInManifest = append(v.NotInManifest, k)
		}
	}
	return manifest, nil
}

// openS11Store opens the port-forward and the run's client; stop ends the
// port-forward.
func (h *haRun) openS11Store(ctx context.Context) (objectStore, func(), error) {
	if h.forward == nil {
		return nil, nil, errors.New("no port-forward to the object store is available")
	}
	namespace, service, port, err := splitService(h.cfg.ObjectStoreService)
	if err != nil {
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
	store, err := open(endpoint, h.accessKey, h.secretKey, !h.cfg.SeedInsecure)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("open the object store client: %w", err)
	}
	return store, stop, nil
}

// checkManifest compares the manifest with the backup's status, the
// cluster and the engine. It checks what instancemanager's Verify would,
// itself, so that the Operator's own verification is not what is tested.
func (h *haRun) checkManifest(m instancemanager.BackupManifest, backup *databasev1alpha1.CubridBackup,
	v *s11Verification) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	s, artifact := backup.Status, backup.Status.Artifact
	if m.ManifestVersion != instancemanager.ManifestVersion {
		add("manifestVersion is %d, not %d", m.ManifestVersion, instancemanager.ManifestVersion)
	}
	if m.Database != databaseName || artifact.Database != databaseName {
		add("the manifest names database %q and the status %q, not %s", m.Database, artifact.Database, databaseName)
	}
	if m.ClusterUID != h.rec.ClusterUID || string(s.ClusterUID) != h.rec.ClusterUID {
		add("the manifest's clusterUID is %q and the status's %q, not the cluster's %s", m.ClusterUID,
			s.ClusterUID, h.rec.ClusterUID)
	}
	if m.Level != 0 || artifact.Level != databasev1alpha1.CubridBackupFull {
		add("the manifest's level is %d and the status's %q, not a full backup", m.Level, artifact.Level)
	}
	if m.SourceInstance != s.TargetInstance || !slices.Contains(h.members, m.SourceInstance) {
		add("the manifest's sourceInstance %q is not status.targetInstance %q", m.SourceInstance, s.TargetInstance)
	}
	// PreferStandby runs on a slave, or on the master when it fell back.
	role := string(databasev1alpha1.RoleSlave)
	if s.FallbackUsed {
		role = string(databasev1alpha1.RoleMaster)
	}
	if m.SourceRole != s.TargetRole || m.SourceRole != role {
		add("the manifest's sourceRole %q, status.targetRole %q and fallbackUsed %t do not agree", m.SourceRole,
			s.TargetRole, s.FallbackUsed)
	}
	// The manifest's cubridVersion is the cluster's spec.version; the engine
	// reports the full version within it.
	if m.CubridVersion != haCubridVersion || !strings.HasPrefix(h.engineVersion, m.CubridVersion+".") {
		add("the manifest's cubridVersion %q is not the cluster's %s, or engine %s is not within it",
			m.CubridVersion, haCubridVersion, h.engineVersion)
	}
	if artifact.CubridVersion != "" && artifact.CubridVersion != m.CubridVersion {
		add("status.artifact.cubridVersion is %q, the manifest's %q", artifact.CubridVersion, m.CubridVersion)
	}
	// UploadBackup stores the canonical encoding, so both must be the digest.
	if v.ManifestDigest != artifact.ManifestDigest || v.ManifestSHA256 != artifact.ManifestDigest {
		add("status.artifact.manifestDigest is %s; the manifest's digest is %s and the stored file's sha256 %s",
			artifact.ManifestDigest, v.ManifestDigest, v.ManifestSHA256)
	}
	if m.TotalSize() != artifact.SizeBytes {
		add("status.artifact.sizeBytes is %d, the manifest's objects hold %d", artifact.SizeBytes, m.TotalSize())
	}
	if len(m.Objects) == 0 {
		add("the manifest lists no objects")
	}
	return problems
}

// artifactKey returns the manifest's key of an s3://<bucket>/<key> URI,
// which must be manifest.json in bucket under prefix.
func artifactKey(uri, bucket, prefix string) (string, error) {
	rest, s3 := strings.CutPrefix(uri, "s3://")
	inBucket, key, _ := strings.Cut(rest, "/")
	if !s3 || inBucket != bucket {
		return "", fmt.Errorf("the artifact URI %q is not in bucket %s", uri, bucket)
	}
	if path.Clean(key) != key || !strings.HasPrefix(key, prefix+"/") ||
		path.Base(key) != instancemanager.ManifestObjectName {
		return "", fmt.Errorf("the artifact URI %q is not a manifest.json under the run's prefix %s", uri, prefix)
	}
	return key, nil
}

// validObjectKey reports a clean relative key below backup/.
func validObjectKey(key string) bool {
	rest, below := strings.CutPrefix(key, "backup/")
	return below && rest != "" && path.Clean(key) == key && !strings.ContainsAny(key, "\\\x00") &&
		!slices.Contains(strings.Split(key, "/"), "..")
}

// download reads a whole object.
func download(ctx context.Context, store objectStore, bucket, key string) ([]byte, int64, error) {
	r, err := store.Get(ctx, bucket, key)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	return data, int64(len(data)), err
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

// checkS11Credentials looks for the object store's keys, as they are and
// base64-encoded, in the backup's status as read now, in the manifest and
// in every Event of the run namespace. The Operator's logs are not read.
func (h *haRun) checkS11Credentials(ctx context.Context, created *databasev1alpha1.CubridBackup, manifest []byte) (
	*s11Credentials, error) {
	found := &s11Credentials{FoundIn: []string{}}
	holds := func(data []byte) bool {
		for _, value := range []string{h.accessKey, h.secretKey} {
			if strings.Contains(string(data), value) ||
				strings.Contains(string(data), base64.StdEncoding.EncodeToString([]byte(value))) {
				return true
			}
		}
		return false
	}
	backup := &databasev1alpha1.CubridBackup{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), backup); err != nil {
		return found, err
	}
	status, err := json.Marshal(backup.Status)
	if err != nil {
		return found, err
	}
	found.StatusChecked = true
	if holds(status) {
		found.FoundIn = append(found.FoundIn, "the CubridBackup's status")
	}
	if manifest != nil {
		found.ManifestChecked = true
		if holds(manifest) {
			found.FoundIn = append(found.FoundIn, "manifest.json")
		}
	}
	events := &corev1.EventList{}
	if err := h.Client.List(ctx, events, client.InNamespace(h.cfg.Namespace)); err != nil {
		return found, fmt.Errorf("list the Events: %w", err)
	}
	for _, event := range events.Items {
		found.EventsChecked++
		if event.InvolvedObject.UID == created.UID {
			found.BackupEvents++
		}
		data, err := json.Marshal(event)
		if err != nil {
			return found, err
		}
		if holds(data) {
			found.FoundIn = append(found.FoundIn, "Event "+event.Name)
		}
	}
	return found, nil
}
