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

package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// memoryStore is an object store that keeps what is put into it.
type memoryStore struct{ objects map[string][]byte }

func (m *memoryStore) Put(_ context.Context, bucket, key string, r io.Reader, _ int64, _ string) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.objects[bucket+"/"+key] = data
	return int64(len(data)), nil
}

func (m *memoryStore) StatSize(_ context.Context, bucket, key string) (int64, error) {
	data, ok := m.objects[bucket+"/"+key]
	if !ok {
		return 0, fmt.Errorf("no object %s/%s", bucket, key)
	}
	return int64(len(data)), nil
}

func (m *memoryStore) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	data, ok := m.objects[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("no object %s/%s", bucket, key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// uploadedManifestURI uploads a staged backup to bucket/prefix the way the
// Instance Manager does and returns the manifest URI it reports.
func uploadedManifestURI(t *testing.T, bucket, prefix string) string {
	t.Helper()
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, "demodb_bk0v000"), []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := instancemanager.UploadBackup(context.Background(), &memoryStore{objects: map[string][]byte{}},
		instancemanager.UploadSpec{StagingDir: staging, Bucket: bucket, Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return res.ManifestURI
}

// destinationCase is a bucket and prefix a backup is written under: whether
// the API server admits them on a CubridBackup destination and on a
// CubridCluster spec.objectStorage, and whether the operator runs a backup
// to them that an earlier release stored.
type destinationCase struct {
	name     string
	bucket   string
	prefix   string
	admitted bool
	runs     bool
}

const (
	destinationBucket = "cubrid-backups"
	destinationPrefix = "nightly"
)

var destinationCases = []destinationCase{
	{"no prefix", destinationBucket, "", true, true},
	{"one segment", destinationBucket, destinationPrefix, true, true},
	{"two segments", destinationBucket, "team-a/nightly", true, true},
	{"dotted bucket and segments", "my.backups", "v1.2/.x/a..b", true, true},
	{"longest prefix", destinationBucket, strings.Repeat("p", 512), true, true},

	// Refused at admission. The operator still runs a backup an earlier
	// release stored when the key it writes is the one its URI names:
	// path.Join removes a trailing or doubled slash and resolves ".".
	{"trailing slash", destinationBucket, "backups/", false, true},
	{"empty segment", destinationBucket, "a//b", false, true},
	{"current segment", destinationBucket, ".", false, true},
	{"current segment first", destinationBucket, "./a", false, true},
	{"parent segment resolved", destinationBucket, "a/../b", false, true},
	{"prefix over the length limit", destinationBucket, strings.Repeat("p", 513), false, true},
	// Refused at admission, and the operator runs no backup: the URI of the
	// artifact would be refused as a recovery source.
	{"leading slash", destinationBucket, "/backups", false, false},
	{"parent segment", destinationBucket, "..", false, false},
	{"leading parent segment", destinationBucket, "../backups", false, false},
	{"percent", destinationBucket, "a%2Fb", false, false},
	{"space", destinationBucket, "a b", false, false},
	{"query", destinationBucket, "a?b", false, false},
	{"fragment", destinationBucket, "a#b", false, false},
	{"upper-case bucket", "Backups", destinationPrefix, false, false},
	{"2-character bucket", "ab", destinationPrefix, false, false},
	{"underscore in bucket", "my_backups", destinationPrefix, false, false},
	{"adjacent dots in bucket", "my..backups", destinationPrefix, false, false},
	{"bucket starting with a hyphen", "-backups", destinationPrefix, false, false},
	{"bucket ending with a hyphen", "backups-", destinationPrefix, false, false},
}

// sourceCluster and destinationBackup are the cluster and the backup whose
// artifact location the destination tests compute, with realistic UIDs.
func sourceCluster() *databasev1alpha1.CubridCluster {
	cluster := standaloneCluster("source")
	cluster.UID = "0b8e3c7e-6a43-4f6e-9a39-0f5c1d0e2a11"
	return cluster
}

func destinationBackup(name, cluster, bucket, prefix string) *databasev1alpha1.CubridBackup {
	return &databasev1alpha1.CubridBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: "5f2d7a90-1c4b-4b3e-8f61-2a9d3e4b5c66"},
		Spec: databasev1alpha1.CubridBackupSpec{
			ClusterRef: databasev1alpha1.LocalObjectRef{Name: cluster},
			Database:   testDatabase,
			Destination: databasev1alpha1.CubridBackupDestination{
				Type:          databasev1alpha1.DestinationObjectStorage,
				ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: bucket, Prefix: prefix},
			},
		},
	}
}

// Every backup the operator runs reports a manifest URI a recovery accepts,
// naming the manifest that was written; every other one it refuses before it
// asks a member. The seed backups of an HA cluster have that form too.
func TestOperatorProducedManifestURIsAreAccepted(t *testing.T) {
	cluster := sourceCluster()
	sel := TargetSelection{Instance: "source-0", Role: databasev1alpha1.RoleMaster, Selected: true}

	for _, tc := range destinationCases {
		t.Run(tc.name, func(t *testing.T) {
			backup := destinationBackup("source-backup", cluster.Name, tc.bucket, tc.prefix)
			refusal := backupDestinationRefusal(backup, cluster)
			if runs := refusal == ""; runs != tc.runs {
				t.Fatalf("the operator runs the backup: %v, want %v (%s)", runs, tc.runs, refusal)
			}
			req := (&CubridBackupReconciler{}).buildBackupRequest(backup, cluster, sel)
			uri := uploadedManifestURI(t, req.Upload.Bucket, req.Upload.Prefix)
			bucket, prefix, err := parseManifestURI(uri)
			if (err == nil) != tc.runs {
				t.Fatalf("the backup runs: %v, but a recovery from its artifact URI %q is refused: %v", tc.runs, uri, err)
			}
			if err == nil && (bucket != req.Upload.Bucket || prefix != req.Upload.Prefix) {
				t.Fatalf("artifact URI %q parses to %q,%q, uploaded to %q,%q",
					uri, bucket, prefix, req.Upload.Bucket, req.Upload.Prefix)
			}
		})
	}

	ha := haCluster("seeded")
	ha.UID = cluster.UID
	ha.Spec.ObjectStorage.Prefix = "operator"
	for _, artifact := range []string{"", "seeded-1-from-seeded-0", "seeded-2-from-seeded-0-r1"} {
		bucket, prefix := seedLocation(ha, testDatabase, artifact)
		uri := uploadedManifestURI(t, bucket, prefix)
		gotBucket, gotPrefix, err := parseManifestURI(uri)
		if err != nil {
			t.Errorf("seed backup URI %q is refused: %v", uri, err)
			continue
		}
		if gotBucket != bucket || gotPrefix != prefix {
			t.Errorf("seed backup URI %q parses to %q,%q, uploaded to %q,%q", uri, gotBucket, gotPrefix, bucket, prefix)
		}
	}
}

// A restore an earlier release started for a URI outside the accepted form
// is followed when it reads the object the URI names, and only then.
func TestStartedManifestLocation(t *testing.T) {
	tests := []struct {
		uri, bucket, prefix string
		followed            bool
	}{
		{"s3://bucket/prod/uid/manifest.json", uriBucket, "prod/uid", true},
		{"s3://bucket/manifest.json", uriBucket, ".", true},
		{"s3://Bucket/p/manifest.json", "Bucket", "p", true},
		{"s3://b/p/manifest.json", "b", "p", true},
		{"s3://bucket/a%2Fb/manifest.json", uriBucket, "a%2Fb", true},
		{"s3://bucket//p/manifest.json", "", "", false},
		{"s3://bucket/a/b/other.json", "", "", false},
		{"s3://bucket/a/../manifest.json", "", "", false},
		{"s3://bucket/../manifest.json", "", "", false},
		{"s3://bucket/./p/manifest.json", "", "", false},
		{"s3://bucket/a//manifest.json", "", "", false},
		{"s3://bucket/p/manifest.json/", "", "", false},
		{"s3://bucket/p/manifest.json?versionId=1", "", "", false},
		{"https://bucket/p/manifest.json", "", "", false},
		{"s3://bucket", "", "", false},
		{"s3:///p/manifest.json", "", "", false},
	}
	for _, tc := range tests {
		bucket, prefix, err := startedManifestLocation(tc.uri)
		if (err == nil) != tc.followed {
			t.Errorf("startedManifestLocation(%q) = %q,%q,%v, want followed %v", tc.uri, bucket, prefix, err, tc.followed)
			continue
		}
		if tc.followed && (bucket != tc.bucket || prefix != tc.prefix) {
			t.Errorf("startedManifestLocation(%q) = %q,%q, want %q,%q", tc.uri, bucket, prefix, tc.bucket, tc.prefix)
		}
	}
}

// schemaPath is where a field's schema is in a CRD version.
func schemaPath(fields ...string) []string {
	path := make([]string, 0, 2+2*len(fields))
	path = append(path, "schema", "openAPIV3Schema")
	for _, f := range fields {
		path = append(path, "properties", f)
	}
	return path
}

var (
	manifestURISchemaPath        = schemaPath("spec", "bootstrap", "recovery", "manifestUri")
	clusterBucketSchemaPath      = schemaPath("spec", "objectStorage", "bucket")
	clusterPrefixSchemaPath      = schemaPath("spec", "objectStorage", "prefix")
	destinationBucketSchemaPath  = schemaPath("spec", "destination", "objectStorage", "bucket")
	destinationPrefixSchemaPath  = schemaPath("spec", "destination", "objectStorage", "prefix")
	bucketRule                   = "self == '' || (self.matches('" + databasev1alpha1.S3BucketNamePattern + "') && !self.matches('" + databasev1alpha1.S3BucketAdjacentDotsPattern + "'))"
	prefixRule                   = "self == '' || self.matches('" + databasev1alpha1.S3KeyPrefixPattern + "')"
	clustersCRDFile, backupsFile = "database.cubrid.io_cubridclusters.yaml", "database.cubrid.io_cubridbackups.yaml"
)

// The XValidation rules in the generated CRDs are the patterns the operator
// compiles: the two cannot drift apart unnoticed.
func TestStorageRulesInCRDsMatchTheOperator(t *testing.T) {
	tests := []struct {
		file      string
		path      []string
		maxLength int
		rules     []string
	}{
		{clustersCRDFile, manifestURISchemaPath, databasev1alpha1.ManifestURIMaxLength, []string{
			"self.matches('" + databasev1alpha1.ManifestURIPattern + "')",
			"!self.matches('" + databasev1alpha1.ManifestURIBucketAdjacentDotsPattern + "')",
		}},
		{clustersCRDFile, clusterBucketSchemaPath, 63, []string{bucketRule}},
		{clustersCRDFile, clusterPrefixSchemaPath, databasev1alpha1.S3KeyPrefixMaxLength, []string{prefixRule}},
		{backupsFile, destinationBucketSchemaPath, 63, []string{bucketRule}},
		{backupsFile, destinationPrefixSchemaPath, databasev1alpha1.S3KeyPrefixMaxLength, []string{prefixRule}},
	}
	for _, tc := range tests {
		data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		var crd map[string]any
		if err := yaml.Unmarshal(data, &crd); err != nil {
			t.Fatal(err)
		}
		versions, _, _ := unstructured.NestedSlice(crd, "spec", "versions")
		if len(versions) != 1 {
			t.Fatalf("%s has %d versions, want 1", tc.file, len(versions))
		}
		field, found, err := unstructured.NestedMap(versions[0].(map[string]any), tc.path...)
		if err != nil || !found {
			t.Fatalf("%s: schema %v not found: %v", tc.file, tc.path, err)
		}
		if got := field["maxLength"]; got != float64(tc.maxLength) {
			t.Errorf("%s %v: maxLength = %v, want %d", tc.file, tc.path, got, tc.maxLength)
		}
		validations, _, _ := unstructured.NestedSlice(field, "x-kubernetes-validations")
		rules := make([]string, 0, len(validations))
		for _, v := range validations {
			rules = append(rules, v.(map[string]any)["rule"].(string))
		}
		if fmt.Sprint(rules) != fmt.Sprint(tc.rules) {
			t.Errorf("%s %v: rules = %q\nwant %q", tc.file, tc.path, rules, tc.rules)
		}
	}
}

// installedSchemas changes field schemas of an installed CRD, as an earlier
// release had them, and puts them back.
type installedSchemas struct {
	ctx  context.Context
	name string
}

func (s installedSchemas) get() *unstructured.Unstructured {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	Expect(k8sClient.Get(s.ctx, types.NamespacedName{Name: s.name}, crd)).To(Succeed())
	return crd
}

// field returns the installed schema at path.
func (s installedSchemas) field(path []string) map[string]any {
	versions, _, _ := unstructured.NestedSlice(s.get().Object, "spec", "versions")
	field, _, _ := unstructured.NestedMap(versions[0].(map[string]any), path...)
	return field
}

// set replaces the schemas at the paths.
func (s installedSchemas) set(fields map[string]map[string]any, paths map[string][]string) {
	Eventually(func() error {
		crd := s.get()
		versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
		version := versions[0].(map[string]any)
		for name, field := range fields {
			if err := unstructured.SetNestedMap(version, field, paths[name]...); err != nil {
				return err
			}
		}
		versions[0] = version
		if err := unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"); err != nil {
			return err
		}
		return k8sClient.Update(s.ctx, crd)
	}).Should(Succeed())
}

// relax replaces the schemas at the paths by plain strings and returns what
// puts them back.
func (s installedSchemas) relax(paths map[string][]string) (restore func()) {
	current, earlier := map[string]map[string]any{}, map[string]map[string]any{}
	for name, path := range paths {
		current[name] = s.field(path)
		earlier[name] = map[string]any{"type": "string", "description": current[name]["description"]}
	}
	s.set(earlier, paths)
	return func() { s.set(current, paths) }
}

// The API server admits a manifestUri exactly when the operator would act on
// it, and an object stored before the rule existed stays updatable while the
// operator refuses its URI with a reason of its own (#320).
var _ = Describe("The manifestUri of a recovery bootstrap (#320)", Serial, func() {
	ctx := context.Background()

	recoveryCluster := func(name, uri string) *databasev1alpha1.CubridCluster {
		c := standaloneCluster(name)
		c.Spec.ObjectStorage = testObjectStorage()
		c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: uri},
		}
		return c
	}
	reconcileOnce := func(r *CubridClusterReconciler, key types.NamespacedName) *databasev1alpha1.CubridCluster {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		return got
	}

	It("is admitted by the API server exactly when the operator restores what it names", func() {
		for i, tc := range manifestURICases {
			_, _, parseErr := parseManifestURI(tc.uri)
			c := recoveryCluster(fmt.Sprintf("manifest-uri-%d", i), tc.uri)
			createErr := k8sClient.Create(ctx, c)
			if createErr != nil {
				Expect(apierrors.IsInvalid(createErr)).To(BeTrue(), "%s: %v", tc.name, createErr)
				Expect(createErr.Error()).To(ContainSubstring("manifestUri"), tc.name)
			}
			Expect(createErr == nil).To(Equal(tc.accepted), "%s: admission of %q: %v", tc.name, tc.uri, createErr)
			Expect(parseErr == nil).To(Equal(createErr == nil),
				"%s: admission (%v) and the operator (%v) disagree on %q", tc.name, createErr, parseErr, tc.uri)
			if createErr != nil {
				continue
			}

			restore := &fakeRestoreClient{}
			r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
			got := reconcileOnce(r, client.ObjectKeyFromObject(c))
			Expect(got.Status.Bootstrap.Phase).NotTo(Equal(databasev1alpha1.BootstrapFailed), tc.name)
			Expect(restore.requests).To(HaveLen(1), tc.name)
			Expect(restore.requests[0].Bucket).To(Equal(tc.wantBucket), tc.name)
			Expect(restore.requests[0].Prefix).To(Equal(tc.wantPrefix), tc.name)
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		}
	})

	It("keeps a cluster stored with a URI accepted before updatable, and follows its restore only to the object it names", func() {
		crd := installedSchemas{ctx: ctx, name: "cubridclusters.database.cubrid.io"}
		const (
			otherFile   = "s3://bucket/a/b/other.json"
			traversal   = "s3://bucket/a/../manifest.json"
			emptyPrefix = "s3://bucket/manifest.json"
			upperBucket = "s3://Bucket/p/manifest.json"
		)
		By("storing clusters while the CRD has the rules of an earlier release")
		restoreRules := crd.relax(map[string][]string{"manifestUri": manifestURISchemaPath})
		DeferCleanup(restoreRules)
		store := func(name, uri string) *databasev1alpha1.CubridCluster {
			c := recoveryCluster(name, uri)
			Eventually(func() error { return k8sClient.Create(ctx, c) }).Should(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
			return c
		}
		stored := store("manifest-uri-earlier", otherFile)
		notStarted := store("manifest-uri-not-started", emptyPrefix)
		inFlightOther := store("manifest-uri-started-other", traversal)
		inFlightSame := store("manifest-uri-started-same", emptyPrefix)
		retried := store("manifest-uri-retried", upperBucket)

		By("installing the current rules")
		restoreRules()
		Eventually(func() bool {
			probe := recoveryCluster("manifest-uri-probe", otherFile)
			err := k8sClient.Create(ctx, probe)
			if err == nil {
				_ = k8sClient.Delete(ctx, probe)
			}
			return apierrors.IsInvalid(err)
		}).Should(BeTrue(), "the current rules refuse the URI on create")

		By("the stored cluster stays updatable: its unchanged URI is not checked again")
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stored), got)).To(Succeed())
		got.Labels = map[string]string{"updated": "yes"}
		got.Spec.ObjectStorage.Region = "eu-west-1"
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		got.Spec.Bootstrap.Recovery.ManifestURI = testManifestURI
		Expect(k8sClient.Update(ctx, got)).To(MatchError(ContainSubstring("bootstrap")),
			"the URI stays immutable")

		expectFailed := func(c *databasev1alpha1.CubridCluster, uri string) {
			cond := meta.FindStatusCondition(c.Status.Conditions, conditionBootstrapReady)
			Expect(c.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("InvalidManifestURI"))
			Expect(cond.Message).To(ContainSubstring(uri))
			Expect(meta.IsStatusConditionTrue(c.Status.Conditions, conditionReady)).To(BeFalse())
		}
		setStatus := func(c *databasev1alpha1.CubridCluster, uri, operation string, attempts int32) {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
			c.Status.Bootstrap = &databasev1alpha1.BootstrapStatus{
				Mode: "Recovery", Phase: databasev1alpha1.BootstrapRestoring, ManifestURI: uri,
				OperationID: operation, Attempts: attempts, TargetMember: c.Name + "-0",
			}
			Expect(k8sClient.Status().Update(ctx, c)).To(Succeed(), "status stays writable")
		}

		By("a recovery that has not started is refused as a terminal failure, and starts no restore")
		for _, c := range []*databasev1alpha1.CubridCluster{stored, notStarted} {
			restore := &fakeRestoreClient{}
			r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
			expectFailed(reconcileOnce(r, client.ObjectKeyFromObject(c)), c.Spec.Bootstrap.Recovery.ManifestURI)
			reconcileOnce(r, client.ObjectKeyFromObject(c))
			Expect(restore.started).To(BeFalse(), "a failed recovery is not started later")
		}

		By("a restore started for a URI that names another object than the one it reads is not followed")
		setStatus(inFlightOther, traversal, opRestore, 0)
		restore := &fakeRestoreClient{}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		expectFailed(reconcileOnce(r, client.ObjectKeyFromObject(inFlightOther)), traversal)
		Expect(restore.polls).To(BeZero())
		Expect(restore.started).To(BeFalse())

		By("a restore started for a URI that names the object it reads is followed")
		setStatus(inFlightSame, emptyPrefix, opRestore, 0)
		restore = &fakeRestoreClient{}
		r = &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		followed := reconcileOnce(r, client.ObjectKeyFromObject(inFlightSame))
		Expect(followed.Status.Bootstrap.Phase).NotTo(Equal(databasev1alpha1.BootstrapFailed))
		Expect(restore.polls).To(Equal(1))

		By("and its next attempt reads the same object")
		setStatus(retried, upperBucket, "", 1)
		restore = &fakeRestoreClient{}
		r = &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		again := reconcileOnce(r, client.ObjectKeyFromObject(retried))
		Expect(again.Status.Bootstrap.Phase).NotTo(Equal(databasev1alpha1.BootstrapFailed))
		Expect(restore.requests).To(HaveLen(1))
		Expect(restore.requests[0].Bucket).To(Equal("Bucket"))
		Expect(restore.requests[0].Prefix).To(Equal("p"))
	})
})

// The bucket and prefix the operator writes a backup under are admitted only
// when the URI of the artifact is one a recovery accepts, and a backup an
// earlier release stored with another one runs nothing (#437).
var _ = Describe("The object-storage location of a backup (#437)", Serial, func() {
	ctx := context.Background()
	backupCRD := installedSchemas{ctx: ctx, name: "cubridbackups.database.cubrid.io"}

	It("is admitted on a CubridBackup and a CubridCluster only when a recovery accepts the artifact URI", func() {
		for i, tc := range destinationCases {
			backup := destinationBackup(fmt.Sprintf("destination-%d", i), "no-cluster", tc.bucket, tc.prefix)
			backup.UID = ""
			err := k8sClient.Create(ctx, backup)
			Expect(err == nil).To(Equal(tc.admitted), "%s: CubridBackup admission: %v", tc.name, err)
			if err == nil {
				Expect(k8sClient.Delete(ctx, backup)).To(Succeed())
			} else {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: %v", tc.name, err)
			}

			cluster := haCluster(fmt.Sprintf("storage-location-%d", i))
			cluster.Spec.ObjectStorage.Bucket = tc.bucket
			cluster.Spec.ObjectStorage.Prefix = tc.prefix
			err = k8sClient.Create(ctx, cluster)
			Expect(err == nil).To(Equal(tc.admitted), "%s: CubridCluster admission: %v", tc.name, err)
			if err == nil {
				Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
			} else {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: %v", tc.name, err)
			}

			if tc.admitted {
				Expect(backupDestinationRefusal(destinationBackup("b", "c", tc.bucket, tc.prefix), sourceCluster())).
					To(BeEmpty(), "%s: an admitted destination is refused by the operator", tc.name)
			}
		}

		By("a standalone cluster still needs no bucket")
		standalone := standaloneCluster("storage-location-no-bucket")
		standalone.Spec.ObjectStorage = testObjectStorage()
		Expect(k8sClient.Create(ctx, standalone)).To(Succeed())
		Expect(k8sClient.Delete(ctx, standalone)).To(Succeed())
	})

	It("ends a stored backup whose artifact URI a recovery refuses before any member is asked", func() {
		cluster := standaloneCluster("destination-source")
		cluster.Spec.ObjectStorage = testObjectStorage()
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

		By("storing backups while the CRD has the rules of an earlier release")
		paths := map[string][]string{"bucket": destinationBucketSchemaPath, "prefix": destinationPrefixSchemaPath}
		restoreRules := backupCRD.relax(paths)
		DeferCleanup(restoreRules)
		var stored []*databasev1alpha1.CubridBackup
		for i, tc := range destinationCases {
			if tc.runs {
				continue
			}
			backup := destinationBackup(fmt.Sprintf("stored-destination-%d", i), cluster.Name, tc.bucket, tc.prefix)
			backup.UID = ""
			Eventually(func() error { return k8sClient.Create(ctx, backup) }).Should(Succeed(), tc.name)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
			stored = append(stored, backup)
		}
		restoreRules()
		Eventually(func() bool {
			probe := destinationBackup("destination-probe", cluster.Name, destinationBucket, "/backups")
			probe.UID = ""
			err := k8sClient.Create(ctx, probe)
			if err == nil {
				_ = k8sClient.Delete(ctx, probe)
			}
			return apierrors.IsInvalid(err)
		}).Should(BeTrue(), "the current rules refuse the prefix on create")

		for _, backup := range stored {
			key := client.ObjectKeyFromObject(backup)
			By("the backup to " + backup.Spec.Destination.ObjectStorage.Bucket + "/" + backup.Spec.Destination.ObjectStorage.Prefix)
			got := &databasev1alpha1.CubridBackup{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			got.Labels = map[string]string{"updated": "yes"}
			Expect(k8sClient.Update(ctx, got)).To(Succeed(), "a stored backup stays updatable")

			fake := &fakeBackupClient{}
			r := &CubridBackupReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Prober: &memberProber{master: cluster.Name + "-0"}, Backup: fake,
			}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(fake.started).To(BeFalse(), "a backup whose artifact cannot be restored was dispatched")
			Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
			Expect(got.Status.TargetInstance).To(BeEmpty())
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionBackupReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("InvalidBackupDestination"))
		}
	})
})
