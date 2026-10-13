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

// The manifest URIs the operator itself produces, the CubridBackup artifact
// URI and the seed backups of an HA cluster, are accepted as a recovery
// source and name the manifest that was written.
func TestOperatorProducedManifestURIsAreAccepted(t *testing.T) {
	cluster := standaloneCluster("source")
	cluster.UID = "0b8e3c7e-6a43-4f6e-9a39-0f5c1d0e2a11"
	backup := &databasev1alpha1.CubridBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", UID: "5f2d7a90-1c4b-4b3e-8f61-2a9d3e4b5c66"},
		Spec:       databasev1alpha1.CubridBackupSpec{Database: testDatabase},
	}
	sel := TargetSelection{Instance: "source-0", Role: databasev1alpha1.RoleMaster, Selected: true}

	for _, storePrefix := range []string{"", "nightly", "team-a/nightly", "nightly/"} {
		backup.Spec.Destination = databasev1alpha1.CubridBackupDestination{
			Type:          databasev1alpha1.DestinationObjectStorage,
			ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: "operator-backups", Prefix: storePrefix},
		}
		req := (&CubridBackupReconciler{}).buildBackupRequest(backup, cluster, sel)
		uri := uploadedManifestURI(t, req.Upload.Bucket, req.Upload.Prefix)
		bucket, prefix, err := parseManifestURI(uri)
		if err != nil {
			t.Errorf("CubridBackup artifact URI %q (destination prefix %q) is refused: %v", uri, storePrefix, err)
			continue
		}
		if bucket != req.Upload.Bucket || prefix != req.Upload.Prefix {
			t.Errorf("CubridBackup artifact URI %q parses to %q,%q, uploaded to %q,%q",
				uri, bucket, prefix, req.Upload.Bucket, req.Upload.Prefix)
		}
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

// The XValidation rules on manifestUri in the generated CRD are the patterns
// the operator compiles: the two cannot drift apart unnoticed.
func TestManifestURIRulesInCRDMatchTheOperator(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "database.cubrid.io_cubridclusters.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	versions, _, _ := unstructured.NestedSlice(crd, "spec", "versions")
	if len(versions) != 1 {
		t.Fatalf("CRD has %d versions, want 1", len(versions))
	}
	field, found, err := unstructured.NestedMap(versions[0].(map[string]any), manifestURISchemaPath...)
	if err != nil || !found {
		t.Fatalf("manifestUri schema not found: %v", err)
	}
	if got := field["maxLength"]; got != float64(databasev1alpha1.ManifestURIMaxLength) {
		t.Errorf("maxLength = %v, want %d", got, databasev1alpha1.ManifestURIMaxLength)
	}
	validations, _, _ := unstructured.NestedSlice(field, "x-kubernetes-validations")
	rules := make([]string, 0, len(validations))
	for _, v := range validations {
		rules = append(rules, v.(map[string]any)["rule"].(string))
	}
	want := []string{
		"self.matches('" + databasev1alpha1.ManifestURIPattern + "')",
		"!self.matches('" + databasev1alpha1.ManifestURIBucketAdjacentDotsPattern + "')",
	}
	if fmt.Sprint(rules) != fmt.Sprint(want) {
		t.Errorf("manifestUri rules = %q\nwant %q", rules, want)
	}
}

// manifestURISchemaPath is where the manifestUri schema is in a CRD version.
var manifestURISchemaPath = strings.Split(
	"schema/openAPIV3Schema/properties/spec/properties/bootstrap/properties/recovery/properties/manifestUri", "/")

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

	It("keeps a cluster stored with a URI accepted before updatable, and refuses that URI as Failed", func() {
		crdKey := types.NamespacedName{Name: "cubridclusters.database.cubrid.io"}
		crdGVK := schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
		getCRD := func() *unstructured.Unstructured {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(crdGVK)
			Expect(k8sClient.Get(ctx, crdKey, crd)).To(Succeed())
			return crd
		}
		// setRules replaces the manifestUri rules of the installed CRD:
		// relaxed, as an earlier release had them, or back to these.
		setRules := func(field map[string]any) {
			Eventually(func() error {
				crd := getCRD()
				versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
				version := versions[0].(map[string]any)
				if err := unstructured.SetNestedMap(version, field, manifestURISchemaPath...); err != nil {
					return err
				}
				versions[0] = version
				if err := unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"); err != nil {
					return err
				}
				return k8sClient.Update(ctx, crd)
			}).Should(Succeed())
		}
		versions, _, _ := unstructured.NestedSlice(getCRD().Object, "spec", "versions")
		current, _, _ := unstructured.NestedMap(versions[0].(map[string]any), manifestURISchemaPath...)
		earlier := map[string]any{"type": "string", "minLength": int64(1), "description": current["description"]}

		const (
			otherFile   = "s3://bucket/a/b/other.json"
			traversal   = "s3://bucket/a/../manifest.json"
			startedName = "manifest-uri-started"
		)
		By("storing clusters while the CRD has the rules of an earlier release")
		setRules(earlier)
		DeferCleanup(func() { setRules(current) })
		stored := recoveryCluster("manifest-uri-earlier", otherFile)
		Eventually(func() error { return k8sClient.Create(ctx, stored) }).Should(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, stored) })
		started := recoveryCluster(startedName, traversal)
		Expect(k8sClient.Create(ctx, started)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, started) })

		By("installing the current rules")
		setRules(current)
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

		By("the operator refuses the URI as a terminal failure and starts no restore")
		restore := &fakeRestoreClient{}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		failed := reconcileOnce(r, client.ObjectKeyFromObject(stored))
		Expect(failed.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
		cond := meta.FindStatusCondition(failed.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("InvalidManifestURI"))
		Expect(cond.Message).To(ContainSubstring(otherFile))
		Expect(restore.started).To(BeFalse())
		reconcileOnce(r, client.ObjectKeyFromObject(stored))
		Expect(restore.started).To(BeFalse(), "a failed recovery is not started later")

		By("a restore started by an earlier release under such a URI is not followed further")
		inFlight := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(started), inFlight)).To(Succeed())
		inFlight.Status.Bootstrap = &databasev1alpha1.BootstrapStatus{
			Mode: "Recovery", Phase: databasev1alpha1.BootstrapRestoring, ManifestURI: traversal,
			OperationID: opRestore, TargetMember: startedName + "-0",
		}
		Expect(k8sClient.Status().Update(ctx, inFlight)).To(Succeed(), "status stays writable")
		restoreInFlight := &fakeRestoreClient{}
		r = &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restoreInFlight}
		stopped := reconcileOnce(r, client.ObjectKeyFromObject(started))
		Expect(stopped.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
		Expect(meta.FindStatusCondition(stopped.Status.Conditions, conditionBootstrapReady).Reason).
			To(Equal("InvalidManifestURI"))
		Expect(restoreInFlight.polls).To(BeZero())
		Expect(restoreInFlight.started).To(BeFalse())
		Expect(meta.IsStatusConditionTrue(stopped.Status.Conditions, conditionReady)).To(BeFalse())
	})
})
