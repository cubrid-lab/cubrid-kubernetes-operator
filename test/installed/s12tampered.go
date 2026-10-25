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
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	s12TamperedLane                = "lab-installed-ha-s12-tampered"
	s12TamperedVariant             = "tampered"
	s12TamperedRecordFile          = "S12/tampered/record.json"
	s12TamperedSourceDataCheckFile = "S12/tampered/source-data-check.json"
	// s12TamperedClusterName is the cluster restored from the tampered copy,
	// in the run namespace, and s12TamperedSecretName its own copy of the
	// seed Secret.
	s12TamperedClusterName = "ha-tampered"
	s12TamperedSecretName  = "object-storage-tampered"
	// s12RefusedReason is the BootstrapReady reason of a recovery that gave
	// up after its last attempt (restoreAttemptFailed).
	s12RefusedReason = "RestoreFailed"
)

// s12Copy is the copy of S11's artifact with one byte changed.
type s12Copy struct {
	// SourceURI and SourceManifestDigest are S11's artifact as S11 recorded
	// it; URI is the copy's manifest under Prefix in the backup bucket.
	SourceURI            string `json:"sourceUri"`
	SourceManifestDigest string `json:"sourceManifestDigest"`
	URI                  string `json:"uri"`
	Prefix               string `json:"prefix"`
	Objects              int    `json:"objects"`
	ObjectsCopied        int    `json:"objectsCopied"`
	// TamperedKey is the manifest's key of the object whose byte at Offset
	// was changed from ByteBefore to ByteAfter. SHA256Before is the manifest's
	// checksum of it, which the run's read of S11's object matched, and
	// SHA256After that of what the run wrote.
	TamperedKey  string `json:"tamperedKey,omitempty"`
	Offset       int64  `json:"offset"`
	ByteBefore   string `json:"byteBefore,omitempty"`
	ByteAfter    string `json:"byteAfter,omitempty"`
	SHA256Before string `json:"sha256Before,omitempty"`
	SHA256After  string `json:"sha256After,omitempty"`
	// Checked is set once the run read the copy back: ObjectsMatching of its
	// objects match the manifest and ObjectsNotMatching do not.
	// ManifestUnchanged is a copied manifest equal to S11's, SourceUnchanged
	// S11's manifest read again with the digest S11 recorded.
	Checked            bool     `json:"checked"`
	ObjectsMatching    int      `json:"objectsMatchingManifest"`
	ObjectsNotMatching []string `json:"objectsNotMatchingManifest"`
	ManifestUnchanged  bool     `json:"manifestUnchanged"`
	SourceUnchanged    bool     `json:"sourceUnchanged"`
	// SourceUnchangedAtEnd is S11's manifest read once more after the
	// refusal, with the digest S11 recorded.
	SourceUnchangedAtEnd bool `json:"sourceUnchangedAtEnd"`
	// Removed is set once every object of the copy was removed.
	Removed bool `json:"removed"`
}

// s12TamperedRecord is the evidence of S12 tampered besides the data check.
type s12TamperedRecord struct {
	Copy       *s12Copy   `json:"copy,omitempty"`
	Cluster    string     `json:"cluster"`
	ClusterUID string     `json:"clusterUID,omitempty"`
	SeedPrefix string     `json:"seedPrefix"`
	SecretRef  string     `json:"credentialsSecretRef"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	Bootstrap  []s12Phase `json:"bootstrap"`
	// RefusedWithin runs to the reading that saw the refusal, whose
	// BootstrapReady reason and message Refusal holds.
	RefusedWithin string `json:"refusedWithin,omitempty"`
	Refusal       string `json:"refusal,omitempty"`
	// SecretsReferenced are the Secrets the cluster's StatefulSet Pod
	// template names.
	SecretsReferenced []string        `json:"secretsReferenced"`
	Source            *s12Source      `json:"source,omitempty"`
	Credentials       *s12Credentials `json:"credentials,omitempty"`
	Notes             []string        `json:"notes,omitempty"`
	Residue           string          `json:"residue"`
}

// runS12Tampered runs S12 tampered after S12 exact passed: the run copies
// S11's artifact under a prefix of its own with one byte of one object
// changed and checks the copy itself, a new cluster is created from the copy,
// and its restore must be refused within RestoreLimit, with the bootstrap
// Failed for that object's checksum mismatch, never Ready and never
// Complete. The source must be unchanged. The copy is removed at the end. A
// run that ends, a copy that cannot be prepared, or a check that cannot be
// made, before S12 tampered is judged blocks it; what was seen to break stays
// a failure.
func (h *haRun) runS12Tampered(ctx context.Context) (result evidence.Scenario) {
	result = evidence.Scenario{ID: s12ID, Variant: s12TamperedVariant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s12TamperedRecord{Cluster: s12TamperedClusterName, SeedPrefix: h.rec.Seed.Prefix + "/tampered-seed",
		SecretRef: s12TamperedSecretName, Bootstrap: []s12Phase{}, SecretsReferenced: []string{},
		Credentials: &s12Credentials{FoundIn: []string{}},
		Notes: []string{"the cluster restored from the tampered copy uses a copy of the seed Secret of its own, " +
			s12TamperedSecretName + "; the source uses " + seedSecretName,
			"the Operator's logs and Events were not checked",
			"after the refusal, the cluster was read once more, at the end"},
		Residue: "nothing of the copy was written"}
	// written are the keys of the copy the run stored, or tried to.
	var written []string
	defer func() {
		h.removeCopy(ctx, written, &rec)
		// A pass without its evidence is a failure; a blocked or failed S12
		// tampered keeps its result, with the evidence that could not be
		// written.
		if err := h.writeJSON(s12TamperedRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S12 tampered evidence: "+err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S12/"+s12TamperedVariant+"/")
		})
	}()
	result.Limits = map[string]string{"restore_limit": h.cfg.RestoreLimit.String()}
	result.Measurements = map[string]any{"refusedWithin": evidence.Unknown, "attempts": evidence.Unknown}
	err := h.tamperedSteps(ctx, &result, &rec, &written)
	// A credential found in any status read is a failure, whichever step
	// ended S12 tampered.
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
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
		result.Result, result.Reason = evidence.Blocked, "the run ended before S12 tampered was judged: "+err.Error()
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, err.Error()
	case never != "":
		result.Result, result.Reason = evidence.Fail, never
	default:
		result.Result = evidence.Pass
		result.Reason = "the cluster created from a copy of S11's artifact with one byte of " + rec.Copy.TamperedKey +
			" changed ended its bootstrap Failed within restore_limit, for that object's checksum mismatch, and " +
			"was never Ready or Complete; the source was unchanged and S11's manifest kept its recorded digest; no " +
			"credential was in its " +
			"status and no Secret of the source in its Pods; the Operator's logs were not checked"
	}
	return result
}

// tamperedSteps runs the steps of S12 tampered in order and returns the first
// error. written collects the keys of the copy, for its removal.
func (h *haRun) tamperedSteps(ctx context.Context, result *evidence.Scenario, rec *s12TamperedRecord,
	written *[]string) error {
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S12 tampered could not tell whether a credential is in the cluster's status", err)
	}
	s11 := h.s11Rec
	if s11 == nil || s11.Artifact == nil || s11.Verification == nil || s11.Verification.ManifestKey == "" {
		return errors.New("S11 recorded no artifact to copy")
	}
	key, err := h.prepareCopy(ctx, s11, rec, written)
	if err != nil {
		return err
	}
	cluster, err := h.createRestored(ctx, s12TamperedClusterName, s12TamperedSecretName, rec.SeedPrefix, rec.Copy.URI)
	if err != nil {
		return err
	}
	created := time.Now()
	createdAt := created.UTC()
	rec.ClusterUID, rec.CreatedAt = string(cluster.UID), &createdAt
	refusedAt, err := h.waitRefused(ctx, cluster, key, rec, result)
	if err != nil {
		return err
	}
	rec.RefusedWithin = refusedAt.Sub(created).Round(time.Millisecond).String()
	result.Measurements["refusedWithin"] = rec.RefusedWithin
	if err := h.checkTamperedSecrets(ctx, rec, result); err != nil {
		return err
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	if err := h.checkSourceS12(ctx, rec.Source, s12TamperedSourceDataCheckFile); err != nil {
		return err
	}
	if err := h.checkS11ManifestAtEnd(ctx, s11, rec.Copy); err != nil {
		return err
	}
	final := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), final); err != nil {
		return fmt.Errorf("read the cluster restored from the tampered copy at the end: %w", err)
	}
	refused, err := h.tamperedReading(final, key, "status at the end", rec, result)
	if err != nil {
		return err
	}
	if !refused {
		return restoreVerdict{fmt.Errorf("at the end, the cluster restored from the tampered copy no longer held "+
			"the refusal: %s", bootstrapReason(final))}
	}
	return nil
}

// prepareCopy copies S11's manifest and every object it lists under the
// copy's prefix in the backup bucket, inverting the middle byte of the first
// listed object that has one, and checks the copy and S11's artifact. Each
// object S11 holds must still match the manifest as the run reads it. The
// manifest is written last and unchanged. It returns the key of the changed
// object, which the refusal must name. Any error blocks S12 tampered.
func (h *haRun) prepareCopy(ctx context.Context, s11 *s11Record, rec *s12TamperedRecord, written *[]string) (
	string, error) {
	c := &s12Copy{SourceURI: s11.Artifact.URI, SourceManifestDigest: s11.Artifact.ManifestDigest,
		Prefix: h.rec.Seed.Prefix + "/tampered", ObjectsNotMatching: []string{}}
	c.URI = "s3://" + h.cfg.BackupBucket + "/" + c.Prefix + "/" + instancemanager.ManifestObjectName
	rec.Copy = c
	bucket, source := h.cfg.BackupBucket, s11.Verification.ManifestKey
	store, stop, err := h.openS11Store(ctx)
	if err != nil {
		return "", err
	}
	defer stop()
	manifest, err := download(ctx, store, bucket, source)
	if err != nil {
		return "", fmt.Errorf("read S11's manifest %s: %w", source, err)
	}
	if sum := sha256Of(manifest); sum != c.SourceManifestDigest {
		return "", fmt.Errorf("S11's manifest %s has sha256 %s, not the digest S11 recorded, %s", source, sum,
			c.SourceManifestDigest)
	}
	parsed, err := instancemanager.ParseManifest(manifest)
	if err != nil {
		return "", fmt.Errorf("parse S11's manifest: %w", err)
	}
	c.Objects = len(parsed.Objects)
	target := -1
	for i, obj := range parsed.Objects {
		if !validObjectKey(obj.Key) {
			return "", fmt.Errorf("S11's manifest object key %q is not a clean path below backup/", obj.Key)
		}
		if target < 0 && obj.SizeBytes > 0 {
			target = i
		}
	}
	if target < 0 {
		return "", errors.New("S11's manifest lists no object with a byte to change")
	}
	changed := parsed.Objects[target]
	c.TamperedKey, c.Offset, c.SHA256Before = changed.Key, changed.SizeBytes/2, changed.SHA256
	for _, obj := range parsed.Objects {
		to := c.Prefix + "/" + obj.Key
		*written = append(*written, to)
		if err := copyObject(ctx, store, bucket, path.Dir(source)+"/"+obj.Key, to, obj, obj.Key == c.TamperedKey,
			c); err != nil {
			return "", err
		}
	}
	to := c.Prefix + "/" + instancemanager.ManifestObjectName
	*written = append(*written, to)
	if err := store.Put(ctx, bucket, to, bytes.NewReader(manifest), int64(len(manifest))); err != nil {
		return "", fmt.Errorf("write %s: %w", to, err)
	}
	return c.TamperedKey, checkCopy(ctx, store, bucket, source, parsed, manifest, c)
}

// copyObject streams one object of S11's artifact to the copy, inverting the
// byte at c.Offset when tamper is set. What it read of S11's object must be
// what the manifest lists.
func copyObject(ctx context.Context, store objectStore, bucket, from, to string, obj instancemanager.ManifestObject,
	tamper bool, c *s12Copy) error {
	r, err := store.Get(ctx, bucket, from)
	if err != nil {
		return fmt.Errorf("read S11's object %s: %w", from, err)
	}
	defer func() { _ = r.Close() }()
	before, after := sha256.New(), sha256.New()
	var read byteCount
	src := io.TeeReader(r, io.MultiWriter(before, &read))
	flip := &byteFlip{r: src, at: -1}
	if tamper {
		flip.at = c.Offset
	}
	if err := store.Put(ctx, bucket, to, io.TeeReader(flip, after), obj.SizeBytes); err != nil {
		return fmt.Errorf("write %s: %w", to, err)
	}
	if sum := hex.EncodeToString(before.Sum(nil)); int64(read) != obj.SizeBytes || sum != obj.SHA256 {
		return fmt.Errorf("S11's object %s read %d bytes with sha256 %s, the manifest %d and %s", from, read, sum,
			obj.SizeBytes, obj.SHA256)
	}
	if tamper {
		if !flip.done {
			return fmt.Errorf("byte %d of %s was not changed", c.Offset, from)
		}
		c.ByteBefore, c.ByteAfter = fmt.Sprintf("0x%02x", flip.before), fmt.Sprintf("0x%02x", flip.after)
		c.SHA256After = hex.EncodeToString(after.Sum(nil))
	}
	c.ObjectsCopied++
	return nil
}

// checkCopy reads the copy back: its manifest must be S11's, exactly the
// changed object must not match it, with the checksum the run wrote, and
// every other object must. S11's manifest, read again, must still have the
// digest S11 recorded; S11's objects were checked as they were copied.
func checkCopy(ctx context.Context, store objectStore, bucket, source string, m instancemanager.BackupManifest,
	manifest []byte, c *s12Copy) error {
	copied, err := download(ctx, store, bucket, c.Prefix+"/"+instancemanager.ManifestObjectName)
	if err != nil {
		return fmt.Errorf("read the copy's manifest: %w", err)
	}
	c.ManifestUnchanged = bytes.Equal(copied, manifest)
	var problems []string
	for _, obj := range m.Objects {
		key := c.Prefix + "/" + obj.Key
		sum, size, err := downloadSum(ctx, store, bucket, key)
		if err != nil {
			return fmt.Errorf("read %s: %w", key, err)
		}
		if size == obj.SizeBytes && sum == obj.SHA256 {
			c.ObjectsMatching++
		} else {
			c.ObjectsNotMatching = append(c.ObjectsNotMatching, obj.Key)
		}
		if obj.Key == c.TamperedKey && (size != obj.SizeBytes || sum != c.SHA256After) {
			problems = append(problems, fmt.Sprintf("%s has %d bytes and sha256 %s, not the %d bytes and %s written",
				key, size, sum, obj.SizeBytes, c.SHA256After))
		}
	}
	c.Checked = true
	again, err := download(ctx, store, bucket, source)
	if err != nil {
		return fmt.Errorf("read S11's manifest %s again: %w", source, err)
	}
	c.SourceUnchanged = sha256Of(again) == c.SourceManifestDigest
	if !c.ManifestUnchanged {
		problems = append(problems, "the copy's manifest is not S11's")
	}
	if !slices.Equal(c.ObjectsNotMatching, []string{c.TamperedKey}) {
		problems = append(problems, fmt.Sprintf("in the copy, the objects %s do not match the manifest, not %s alone",
			strings.Join(c.ObjectsNotMatching, ", "), c.TamperedKey))
	}
	if !c.SourceUnchanged {
		problems = append(problems, "S11's manifest no longer has the digest S11 recorded")
	}
	if len(problems) > 0 {
		return fmt.Errorf("the tampered copy is not as intended: %s", strings.Join(problems, "; "))
	}
	return nil
}

// waitRefused reads the cluster restored from the tampered copy until its
// restore is refused for the changed object, for at most RestoreLimit. Each
// change of the bootstrap is recorded, and every status read is searched for
// a credential.
func (h *haRun) waitRefused(ctx context.Context, created *databasev1alpha1.CubridCluster, key string,
	rec *s12TamperedRecord, result *evidence.Scenario) (time.Time, error) {
	wctx, cancel := context.WithTimeout(ctx, h.cfg.RestoreLimit)
	defer cancel()
	// readErr is the last reading's error, nil once a reading succeeded.
	var readErr error
	var refusedAt time.Time
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
			return false, restoreVerdict{errors.New("the cluster restored from the tampered copy was replaced")}
		}
		refused, err := h.tamperedReading(cluster, key, "status read", rec, result)
		if err != nil || !refused {
			return false, err
		}
		refusedAt = time.Now()
		return true, nil
	})
	switch {
	case err == nil:
		return refusedAt, nil
	case ctx.Err() == nil && wctx.Err() != nil && readErr != nil:
		// The state at the limit is not known: the run could not judge.
		return time.Time{}, fmt.Errorf("the cluster restored from the tampered copy could not be read before "+
			"restore_limit %s passed: %w", h.cfg.RestoreLimit, readErr)
	case ctx.Err() == nil && wctx.Err() != nil:
		phase, reason := "none", "no BootstrapReady condition"
		if n := len(rec.Bootstrap); n > 0 {
			last := rec.Bootstrap[n-1]
			phase = last.Phase
			if c := meta.FindStatusCondition(last.Conditions, bootstrapReadyCondition); c != nil {
				reason = c.Reason + ": " + c.Message
			}
		}
		return time.Time{}, restoreVerdict{fmt.Errorf("the restore of the tampered copy was not refused within "+
			"restore_limit %s; its last bootstrap phase was %q, BootstrapReady %s", h.cfg.RestoreLimit, phase, reason)}
	}
	return time.Time{}, err
}

// tamperedReading records one reading of the cluster restored from the
// tampered copy and reports whether it shows the refusal: bootstrap phase
// Failed with BootstrapReady reason RestoreFailed and a message naming the
// checksum mismatch of the changed object. Ready True and phase Complete
// must never happen; Failed for another reason fails S12 tampered.
func (h *haRun) tamperedReading(cluster *databasev1alpha1.CubridCluster, key, where string, rec *s12TamperedRecord,
	result *evidence.Scenario) (bool, error) {
	phase, attempts := "", int32(0)
	if boot := cluster.Status.Bootstrap; boot != nil {
		phase, attempts = string(boot.Phase), boot.Attempts
	}
	recordBootstrap(cluster, &rec.Bootstrap)
	rec.Credentials.StatusesChecked++
	if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the tampered cluster's "+where+" in bootstrap "+
		"phase "+cmp.Or(phase, "none"), cluster.Status); err != nil {
		return false, err
	}
	var events []string
	if ready := meta.FindStatusCondition(cluster.Status.Conditions, startingConditions[0]); ready != nil &&
		ready.Status == metav1.ConditionTrue {
		events = append(events, fmt.Sprintf("the cluster restored from the tampered copy reported Ready in bootstrap "+
			"phase %q", phase))
	}
	if phase == string(databasev1alpha1.BootstrapComplete) {
		events = append(events, "the restore of the tampered copy completed: the bootstrap phase is Complete")
	}
	if len(events) > 0 {
		result.NeverEvents = append(result.NeverEvents, events...)
		return false, restoreVerdict{errors.New(strings.Join(events, "; "))}
	}
	if phase != string(databasev1alpha1.BootstrapFailed) {
		return false, nil
	}
	want := "object " + key + " checksum mismatch"
	if c := meta.FindStatusCondition(cluster.Status.Conditions, bootstrapReadyCondition); c == nil ||
		c.Reason != s12RefusedReason || !strings.Contains(c.Message, want) {
		return false, restoreVerdict{fmt.Errorf("the restore of the tampered copy ended Failed for another reason "+
			"than %s %q: %s", s12RefusedReason, want, bootstrapReason(cluster))}
	}
	rec.Refusal = bootstrapReason(cluster)
	result.Measurements["attempts"] = attempts
	return true, nil
}

// checkTamperedSecrets records the Secrets the Pod template of the cluster's
// StatefulSet names. One that a Pod template of the source also names must
// never happen: the new cluster would depend on a Secret of the source.
func (h *haRun) checkTamperedSecrets(ctx context.Context, rec *s12TamperedRecord, result *evidence.Scenario) error {
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: h.cfg.Namespace, Name: s12TamperedClusterName}
	if err := h.Client.Get(ctx, key, sts); err != nil {
		return fmt.Errorf("read the StatefulSet of %s: %w", s12TamperedClusterName, err)
	}
	names := secretNames(&sts.Spec.Template.Spec)
	slices.Sort(names)
	rec.SecretsReferenced = slices.Compact(names)
	source, err := h.secretsOf(ctx, haClusterName)
	if err != nil {
		return err
	}
	shared := slices.DeleteFunc(slices.Clone(rec.SecretsReferenced), func(name string) bool {
		return !slices.Contains(source, name)
	})
	if len(shared) > 0 {
		result.NeverEvents = append(result.NeverEvents, "the Pods of the cluster restored from the tampered copy "+
			"name Secrets of the source cluster: "+strings.Join(shared, ", "))
	}
	return nil
}

// checkS11ManifestAtEnd reads S11's manifest once more after the refusal: a
// digest other than the one S11 recorded means S11's artifact changed while
// the cluster restored from the copy ran, which fails S12 tampered. A read
// that fails blocks it.
func (h *haRun) checkS11ManifestAtEnd(ctx context.Context, s11 *s11Record, c *s12Copy) error {
	store, stop, err := h.openS11Store(ctx)
	if err != nil {
		return err
	}
	defer stop()
	manifest, err := download(ctx, store, h.cfg.BackupBucket, s11.Verification.ManifestKey)
	if err != nil {
		return fmt.Errorf("read S11's manifest %s at the end: %w", s11.Verification.ManifestKey, err)
	}
	c.SourceUnchangedAtEnd = sha256Of(manifest) == c.SourceManifestDigest
	if !c.SourceUnchangedAtEnd {
		return restoreVerdict{fmt.Errorf("S11's manifest %s no longer has the digest S11 recorded, %s, after the "+
			"cluster restored from the copy ran", s11.Verification.ManifestKey, c.SourceManifestDigest)}
	}
	return nil
}

// removeCopy removes the keys of the copy the run wrote, with a budget of
// its own so that a run that ended still removes them, and records whether
// the prefix is empty after it. What it leaves is the residue; it does not
// judge S12 tampered.
func (h *haRun) removeCopy(ctx context.Context, keys []string, rec *s12TamperedRecord) {
	const cluster = "; the cluster goes with the namespace, and its seed backups under its seed prefix, if any, " +
		"are not removed by this run"
	if len(keys) == 0 {
		rec.Residue = "nothing of the copy was written" + cluster
		return
	}
	c := rec.Copy
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.CleanupTimeout)
	defer cancel()
	err := func() error {
		store, stop, err := h.openS11Store(rctx)
		if err != nil {
			return err
		}
		defer stop()
		// The manifest, written last, goes first: no reader sees a manifest
		// that names objects already removed.
		var errs []error
		for _, key := range slices.Backward(keys) {
			if err := store.Remove(rctx, h.cfg.BackupBucket, key); err != nil {
				errs = append(errs, fmt.Errorf("remove %s: %w", key, err))
			}
		}
		left, err := store.List(rctx, h.cfg.BackupBucket, c.Prefix+"/")
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("list %s/: %w", c.Prefix, err))
		case len(left) > 0:
			errs = append(errs, fmt.Errorf("left under %s/: %s", c.Prefix, strings.Join(left, ", ")))
		}
		return errors.Join(errs...)
	}()
	if err != nil {
		rec.Residue = fmt.Sprintf("the copy under %s/ in %s may be left: %s", c.Prefix, h.cfg.BackupBucket,
			strings.ReplaceAll(err.Error(), "\n", "; ")) + cluster
		return
	}
	c.Removed = true
	rec.Residue = "the copy under " + c.Prefix + "/ was removed" + cluster
}

// sha256Of is the hex SHA-256 of data.
func sha256Of(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// byteCount counts what is written to it.
type byteCount int64

func (n *byteCount) Write(p []byte) (int, error) {
	*n += byteCount(len(p))
	return len(p), nil
}

// byteFlip inverts the byte at offset at of what it reads, once, and keeps
// it before and after.
type byteFlip struct {
	r             io.Reader
	at, read      int64
	before, after byte
	done          bool
}

func (f *byteFlip) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if i := f.at - f.read; !f.done && f.at >= 0 && i >= 0 && i < int64(n) {
		f.before = p[i]
		p[i] = ^p[i]
		f.after, f.done = p[i], true
	}
	f.read += int64(n)
	return n, err
}
