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
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// dbaTestPassword is the value of the DBA password Secrets of these tests. No
// status, Event or log line may hold it.
const dbaTestPassword = "Dba-Secret-Value-805"

// dbaTestKey is the key of the password in those Secrets.
const dbaTestKey = "password"

// dbaPasswordRef names key of Secret name as a cluster's DBA password.
func dbaPasswordRef(name, key string) *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
}

// The API admits a DBA password Secret on a standalone or an HA cluster
// (#803, #834), only with a Secret name and key, never as optional, and never
// changed after the cluster was created.
var _ = Describe("The DBA password Secret reference in the API (#803)", func() {
	ctx := context.Background()

	create := func(c *databasev1alpha1.CubridCluster) error {
		err := k8sClient.Create(ctx, c)
		if err == nil {
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		}
		return err
	}
	change := func(key types.NamespacedName, edit func(*databasev1alpha1.CubridCluster)) error {
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		edit(c)
		return k8sClient.Update(ctx, c)
	}

	It("is admitted on a standalone cluster, new or restored", func() {
		c := standaloneCluster("dba-api-standalone")
		c.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba", dbaTestKey)
		Expect(create(c)).To(Succeed())

		restored := standaloneCluster("dba-api-restored")
		restored.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba", dbaTestKey)
		restored.Spec.ObjectStorage = testObjectStorage()
		restored.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
		}
		Expect(create(restored)).To(Succeed())
	})

	It("is admitted on an HA cluster, new or restored", func() {
		c := haCluster("dba-api-ha")
		c.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba", dbaTestKey)
		Expect(create(c)).To(Succeed())

		restored := haCluster("dba-api-ha-restored")
		restored.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba", dbaTestKey)
		restored.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
		}
		Expect(create(restored)).To(Succeed())

		By("and cannot be pointed elsewhere afterwards, as on a standalone cluster")
		Expect(change(client.ObjectKeyFromObject(c), func(c *databasev1alpha1.CubridCluster) {
			c.Spec.DBAPasswordSecretRef.Name = "dba-other"
		})).To(MatchError(ContainSubstring("dbaPasswordSecretRef cannot be added, removed or changed")))
	})

	It("is refused without a Secret name or key, and as optional", func() {
		for name, ref := range map[string]*corev1.SecretKeySelector{
			"dba-api-no-name": dbaPasswordRef("", dbaTestKey),
			"dba-api-no-key":  dbaPasswordRef("dba", ""),
		} {
			c := standaloneCluster(name)
			c.Spec.DBAPasswordSecretRef = ref
			Expect(create(c)).To(MatchError(ContainSubstring("dbaPasswordSecretRef")), name)
		}
		optional := true
		c := standaloneCluster("dba-api-optional")
		c.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba", dbaTestKey)
		c.Spec.DBAPasswordSecretRef.Optional = &optional
		Expect(create(c)).To(MatchError(ContainSubstring("optional")),
			"an optional password would create a database without one when the Secret is missing")
	})

	It("cannot be added, removed or pointed elsewhere after the cluster was created", func() {
		without := standaloneCluster("dba-api-add")
		Expect(create(without)).To(Succeed())
		Expect(change(client.ObjectKeyFromObject(without), func(c *databasev1alpha1.CubridCluster) {
			c.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba", dbaTestKey)
		})).To(MatchError(ContainSubstring("dbaPasswordSecretRef cannot be added, removed or changed")))

		with := standaloneCluster("dba-api-change")
		with.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba", dbaTestKey)
		Expect(create(with)).To(Succeed())
		key := client.ObjectKeyFromObject(with)
		for what, edit := range map[string]func(*databasev1alpha1.CubridCluster){
			"removed":        func(c *databasev1alpha1.CubridCluster) { c.Spec.DBAPasswordSecretRef = nil },
			"another Secret": func(c *databasev1alpha1.CubridCluster) { c.Spec.DBAPasswordSecretRef.Name = "dba-other" },
			"another key":    func(c *databasev1alpha1.CubridCluster) { c.Spec.DBAPasswordSecretRef.Key = "other-key" },
		} {
			Expect(change(key, edit)).To(MatchError(ContainSubstring("dbaPasswordSecretRef cannot be added, removed or changed")), what)
		}

		By("another change to the same cluster is still accepted")
		Expect(change(key, func(c *databasev1alpha1.CubridCluster) {
			c.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
		})).To(Succeed())
	})
})

// The operator delivers the DBA password the cluster names to the DB
// container as a file, and creates the database only once the Secret holds a
// usable value, so a database is never created without the password it asked
// for (#805). It never puts the value into anything it writes.
var _ = Describe("The DBA password Secret of a standalone or HA cluster (#805, #834)", func() {
	ctx := context.Background()

	// logged collects the log lines of the reconciles that run with its
	// context.
	type logged struct {
		mu    sync.Mutex
		lines []string
	}
	withLog := func() (context.Context, *logged) {
		l := &logged{}
		return logf.IntoContext(ctx, funcr.New(func(prefix, args string) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.lines = append(l.lines, prefix+" "+args)
		}, funcr.Options{Verbosity: 2})), l
	}
	newReconciler := func() *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100),
		}
	}
	createCluster := func(name, secret, key string) types.NamespacedName {
		c := standaloneCluster(name)
		c.Spec.DBAPasswordSecretRef = dbaPasswordRef(secret, key)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		return client.ObjectKeyFromObject(c)
	}
	createSecret := func(name string, data map[string][]byte) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}, Data: data}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, s))).To(Succeed()) })
		return s
	}
	getCluster := func(key types.NamespacedName) *databasev1alpha1.CubridCluster {
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		return c
	}
	statefulSetExists := func(key types.NamespacedName) bool {
		err := k8sClient.Get(ctx, key, &appsv1.StatefulSet{})
		if apierrors.IsNotFound(err) {
			return false
		}
		Expect(err).NotTo(HaveOccurred())
		return true
	}
	// expectNoValue checks that the value is in no status, Event or log line.
	expectNoValue := func(key types.NamespacedName, events []string, l *logged) {
		status, err := json.Marshal(getCluster(key).Status)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(status)).NotTo(ContainSubstring(dbaTestPassword), "the status holds the password")
		for _, e := range events {
			Expect(e).NotTo(ContainSubstring(dbaTestPassword), "an Event holds the password")
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, line := range l.lines {
			Expect(line).NotTo(ContainSubstring(dbaTestPassword), "a log line holds the password")
		}
	}

	// A Secret that cannot be used keeps the database from being created, and
	// says which Secret and key are missing, never what they hold.
	DescribeTable("does not create the database while the Secret cannot be used",
		func(name string, data map[string][]byte, reason, says string) {
			if data != nil {
				createSecret(name+"-secret", data)
			}
			key := createCluster(name, name+"-secret", dbaTestKey)
			r := newReconciler()
			logCtx, l := withLog()

			_, err := r.Reconcile(logCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred(), "the Secret watch brings the cluster back; nothing is retried in between")
			Expect(statefulSetExists(key)).To(BeFalse(), "a database would be created without the password it asks for")

			got := getCluster(key)
			resolved := meta.FindStatusCondition(got.Status.Conditions, conditionDBAPasswordSecretResolved)
			Expect(resolved).NotTo(BeNil())
			Expect(resolved.Status).To(Equal(metav1.ConditionFalse))
			Expect(resolved.Reason).To(Equal(reason))
			Expect(resolved.Message).To(ContainSubstring(name + "-secret"))
			Expect(resolved.Message).To(ContainSubstring(says))
			ready := meta.FindStatusCondition(got.Status.Conditions, conditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(reason))

			events := drain(r.Recorder.(*record.FakeRecorder))
			Expect(events).To(ContainElement(And(HavePrefix(corev1.EventTypeWarning+" "+reason), ContainSubstring(name+"-secret"))))
			expectNoValue(key, events, l)

			By("reporting the same problem once")
			_, err = r.Reconcile(logCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(drain(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
		},
		Entry("when the Secret does not exist", "dba-missing", nil, reasonDBAPasswordSecretNotFound, "does not exist"),
		Entry("when the Secret has no such key", "dba-no-key",
			map[string][]byte{"other": []byte(dbaTestPassword)}, reasonDBAPasswordKeyNotFound, `"`+dbaTestKey+`"`),
		Entry("when the key is empty", "dba-empty",
			map[string][]byte{dbaTestKey: {}}, reasonDBAPasswordKeyEmpty, "empty"),
		Entry("when the key holds only a line feed", "dba-newline",
			map[string][]byte{dbaTestKey: []byte("\n")}, reasonDBAPasswordKeyEmpty, "empty"),
		Entry("when the value holds a line feed the Instance Manager refuses", "dba-two-lines",
			map[string][]byte{dbaTestKey: []byte(dbaTestPassword + "\n" + dbaTestPassword)},
			reasonDBAPasswordValueInvalid, "line feed"),
		Entry("when a control byte would interrupt the terminal password prompt", "dba-control-interrupt",
			map[string][]byte{dbaTestKey: []byte(dbaTestPassword + "\x03")},
			reasonDBAPasswordValueInvalid, "control byte"),
		Entry("when DEL would edit the terminal password", "dba-control-erase",
			map[string][]byte{dbaTestKey: []byte(dbaTestPassword + "\x7f")},
			reasonDBAPasswordValueInvalid, "control byte"),
		Entry("when the value is longer than the Instance Manager accepts", "dba-too-long",
			map[string][]byte{dbaTestKey: []byte(strings.Repeat(dbaTestPassword, 20))},
			reasonDBAPasswordValueInvalid, "256 bytes"),
	)

	It("reports a Secret it cannot read, retries, and creates no database", func() {
		key := createCluster("dba-forbidden", "dba-forbidden-secret", dbaTestKey)
		r := newReconciler()
		r.APIReader = forbiddenSecrets{Reader: k8sClient}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "a read that may succeed later is retried: %v", err)
		Expect(statefulSetExists(key)).To(BeFalse())
		resolved := meta.FindStatusCondition(getCluster(key).Status.Conditions, conditionDBAPasswordSecretResolved)
		Expect(resolved).NotTo(BeNil())
		Expect(resolved.Reason).To(Equal(reasonDBAPasswordSecretReadFailed))
		Expect(resolved.Message).To(ContainSubstring("dba-forbidden-secret"))
	})

	It("mounts only the named key, read-only, into the DB container alone and names the file in its environment", func() {
		createSecret("dba-present-secret", map[string][]byte{
			dbaTestKey: []byte(dbaTestPassword + "\n"), "unrelated": []byte("not for the DB Pod"),
		})
		key := createCluster("dba-present", "dba-present-secret", dbaTestKey)
		r := newReconciler()
		logCtx, l := withLog()
		_, err := r.Reconcile(logCtx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		spec := sts.Spec.Template.Spec
		var volume *corev1.Volume
		for i := range spec.Volumes {
			if spec.Volumes[i].Name == dbaPasswordVolume {
				volume = &spec.Volumes[i]
			}
		}
		Expect(volume).NotTo(BeNil(), "the Pod has no DBA password volume")
		Expect(volume.Secret).NotTo(BeNil())
		Expect(volume.Secret.SecretName).To(Equal("dba-present-secret"))
		Expect(volume.Secret.Items).To(Equal([]corev1.KeyToPath{{Key: dbaTestKey, Path: dbaPasswordFileName}}),
			"only the named key is projected")
		Expect(volume.Secret.DefaultMode).NotTo(BeNil())
		Expect(*volume.Secret.DefaultMode).To(Equal(dbaPasswordFileMode))
		Expect(volume.Secret.Optional).To(SatisfyAny(BeNil(), HaveValue(BeFalse())),
			"a missing Secret must keep the container from starting")

		db := spec.Containers[0]
		Expect(db.Name).To(Equal(appName))
		Expect(db.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name: dbaPasswordVolume, MountPath: dbaPasswordMountPath, ReadOnly: true,
		}), "no subPath: the kubelet does not update a subPath mount")
		file, ok := envValue(db, instancemanager.DBAPasswordFileEnv)
		Expect(ok).To(BeTrue())
		Expect(file.Value).To(Equal(dbaPasswordMountPath + "/" + dbaPasswordFileName))
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers[1:]...) {
			for _, m := range c.VolumeMounts {
				Expect(m.Name).NotTo(Equal(dbaPasswordVolume), "container %s mounts the DBA password", c.Name)
			}
		}

		By("saying that the Secret resolves, and nothing about the password being applied")
		resolved := meta.FindStatusCondition(getCluster(key).Status.Conditions, conditionDBAPasswordSecretResolved)
		Expect(resolved).NotTo(BeNil())
		Expect(resolved.Status).To(Equal(metav1.ConditionTrue))
		Expect(resolved.Reason).To(Equal(reasonDBAPasswordSecretResolved))

		raw, err := json.Marshal(sts)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(ContainSubstring(dbaTestPassword), "the StatefulSet holds the password")
		expectNoValue(key, drain(r.Recorder.(*record.FakeRecorder)), l)
	})

	It("adds nothing to the Pod template of a cluster without the reference", func() {
		r := newReconciler()
		without := standaloneCluster("dba-none")
		with := standaloneCluster("dba-none")
		with.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba", dbaTestKey)
		plain := r.podSpec(without, "image")
		Expect(plain.Volumes).NotTo(ContainElement(HaveField("Name", dbaPasswordVolume)))
		_, set := envValue(plain.Containers[0], instancemanager.DBAPasswordFileEnv)
		Expect(set).To(BeFalse())

		By("the Pod template with the reference differs only by the volume, its mount and the variable")
		withRef := r.podSpec(with, "image")
		withRef.Volumes = removeVolume(withRef.Volumes, dbaPasswordVolume)
		withRef.Containers[0].VolumeMounts = removeMount(withRef.Containers[0].VolumeMounts, dbaPasswordVolume)
		withRef.Containers[0].Env = removeEnv(withRef.Containers[0].Env, instancemanager.DBAPasswordFileEnv)
		want, err := json.Marshal(plain)
		Expect(err).NotTo(HaveOccurred())
		got, err := json.Marshal(withRef)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(string(want)))

		By("and sets no DBA password condition on such a cluster")
		Expect(k8sClient.Create(ctx, without)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, without))).To(Succeed()) })
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(without)})
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.FindStatusCondition(getCluster(client.ObjectKeyFromObject(without)).Status.Conditions,
			conditionDBAPasswordSecretResolved)).To(BeNil())
	})

	It("gives a restored cluster the same file", func() {
		restored := standaloneCluster("dba-restored")
		restored.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba", dbaTestKey)
		restored.Spec.ObjectStorage = testObjectStorage()
		restored.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
		}
		spec := newReconciler().podSpec(restored, "image")
		Expect(spec.Volumes).To(ContainElement(HaveField("Name", dbaPasswordVolume)))
		file, ok := envValue(spec.Containers[0], instancemanager.DBAPasswordFileEnv)
		Expect(ok).To(BeTrue())
		Expect(file.Value).To(Equal(dbaPasswordMountPath + "/" + dbaPasswordFileName))
	})

	// An HA cluster's members are created by the same StatefulSet, and its HA
	// bootstrap asks a running member to create the database. So while the
	// Secret is missing, no member runs and the bootstrap cannot start; once
	// it resolves, every member gets the file (#834).
	It("starts no member and no HA bootstrap of an HA cluster while the Secret is missing", func() {
		c := haCluster("dba-ha-missing")
		c.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba-ha-missing-secret", dbaTestKey)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		key := client.ObjectKeyFromObject(c)
		boot := &fakeHABootstrapClient{state: instancemanager.OpCompleted}
		r := newReconciler()
		r.Prober = &memberProber{}
		r.HABootstrap = boot
		logCtx, l := withLog()

		for range 2 {
			_, err := r.Reconcile(logCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(statefulSetExists(key)).To(BeFalse(), "an HA member would create the database without the password")
		Expect(boot.members).To(BeEmpty(), "the HA bootstrap was asked to create the database")
		got := getCluster(key)
		Expect(got.Status.Databases).To(BeEmpty(), "the HA bootstrap recorded progress")
		resolved := meta.FindStatusCondition(got.Status.Conditions, conditionDBAPasswordSecretResolved)
		Expect(resolved).NotTo(BeNil())
		Expect(resolved.Reason).To(Equal(reasonDBAPasswordSecretNotFound))
		ready := meta.FindStatusCondition(got.Status.Conditions, conditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(reasonDBAPasswordSecretNotFound))

		By("creating the Secret, after which every member is created with the file")
		createSecret("dba-ha-missing-secret", map[string][]byte{dbaTestKey: []byte(dbaTestPassword)})
		_, err := r.Reconcile(logCtx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(statefulSetExists(key)).To(BeTrue())
		expectNoValue(key, drain(r.Recorder.(*record.FakeRecorder)), l)
	})

	It("mounts only the named key, read-only, into the DB container of every HA member and names the file in its environment", func() {
		createSecret("dba-ha-secret", map[string][]byte{
			dbaTestKey: []byte(dbaTestPassword + "\n"), "unrelated": []byte("not for the DB Pod"),
		})
		c := haCluster("dba-ha")
		c.Spec.DBAPasswordSecretRef = dbaPasswordRef("dba-ha-secret", dbaTestKey)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		key := client.ObjectKeyFromObject(c)
		r := newReconciler()
		logCtx, l := withLog()
		_, err := r.Reconcile(logCtx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		Expect(sts.Spec.Replicas).To(HaveValue(Equal(c.Spec.Topology.PromotableMembers)),
			"every member is a Pod of this one template")
		spec := sts.Spec.Template.Spec
		Expect(spec.Volumes).To(ContainElement(dbaPasswordVolumeFor(c.Spec.DBAPasswordSecretRef)),
			"only the named key is projected, as a Secret volume that is not optional")
		db := spec.Containers[0]
		Expect(db.Name).To(Equal(appName))
		Expect(db.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name: dbaPasswordVolume, MountPath: dbaPasswordMountPath, ReadOnly: true,
		}))
		file, ok := envValue(db, instancemanager.DBAPasswordFileEnv)
		Expect(ok).To(BeTrue())
		Expect(file.Value).To(Equal(dbaPasswordFile))
		for _, other := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers[1:]...) {
			for _, m := range other.VolumeMounts {
				Expect(m.Name).NotTo(Equal(dbaPasswordVolume), "container %s mounts the DBA password", other.Name)
			}
		}

		By("keeping the HA configuration the members need beside it")
		_, ok = envValue(db, "CUBRID_HA_CONF")
		Expect(ok).To(BeTrue())

		raw, err := json.Marshal(sts)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(ContainSubstring(dbaTestPassword), "the StatefulSet holds the password")
		expectNoValue(key, drain(r.Recorder.(*record.FakeRecorder)), l)
	})

	It("leaves an existing StatefulSet as it is when the Secret goes away, and reports it", func() {
		secret := createSecret("dba-gone-secret", map[string][]byte{dbaTestKey: []byte(dbaTestPassword)})
		key := createCluster("dba-gone", "dba-gone-secret", dbaTestKey)
		r := newReconciler()
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		before := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, before)).To(Succeed())
		drain(r.Recorder.(*record.FakeRecorder))

		Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		logCtx, l := withLog()
		_, err = r.Reconcile(logCtx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		after := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
		Expect(after.UID).To(Equal(before.UID), "the StatefulSet was recreated")
		Expect(after.Spec.Template).To(Equal(before.Spec.Template))
		resolved := meta.FindStatusCondition(getCluster(key).Status.Conditions, conditionDBAPasswordSecretResolved)
		Expect(resolved).NotTo(BeNil())
		Expect(resolved.Status).To(Equal(metav1.ConditionFalse))
		Expect(resolved.Reason).To(Equal(reasonDBAPasswordSecretNotFound))
		events := drain(r.Recorder.(*record.FakeRecorder))
		Expect(events).To(ContainElement(HavePrefix(corev1.EventTypeWarning + " " + reasonDBAPasswordSecretNotFound)))
		expectNoValue(key, events, l)
	})

	// The Secret is usually created after or together with the cluster. The
	// operator does not watch the user's Secrets (#282), so a waiting cluster
	// is checked again after DBAPasswordRecheck, and goes on once it appears.
	It("creates the database once the missing Secret is created, without being asked again", func() {
		const namespace = "dba-password-watch"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		skipNameValidation := true
		cacheOptions := cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Cache:                  cacheOptions,
			Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		r := &CubridClusterReconciler{
			Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(),
			Recorder: record.NewFakeRecorder(100), DBAPasswordRecheck: 500 * time.Millisecond,
		}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		mgrCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() {
			stop()
			<-done
		})

		c := standaloneCluster("late")
		c.Namespace = namespace
		c.Spec.DBAPasswordSecretRef = dbaPasswordRef("late-dba", dbaTestKey)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		key := client.ObjectKeyFromObject(c)
		Eventually(func() string {
			resolved := meta.FindStatusCondition(getCluster(key).Status.Conditions, conditionDBAPasswordSecretResolved)
			if resolved == nil {
				return ""
			}
			return resolved.Reason
		}).WithTimeout(10 * time.Second).Should(Equal(reasonDBAPasswordSecretNotFound))
		Consistently(func() bool { return statefulSetExists(key) }).WithTimeout(2 * time.Second).Should(BeFalse())

		By("creating the Secret, which the operator does not label or own")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "late-dba", Namespace: namespace},
			Data:       map[string][]byte{dbaTestKey: []byte(dbaTestPassword)},
		})).To(Succeed())
		Eventually(func() bool { return statefulSetExists(key) }).WithTimeout(10 * time.Second).Should(BeTrue())
	})
})

// forbiddenSecrets refuses every read of a Secret, as the API server does for
// an identity without the permission.
type forbiddenSecrets struct{ client.Reader }

func (f forbiddenSecrets) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("no permission to read Secrets"))
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

func removeVolume(in []corev1.Volume, name string) []corev1.Volume {
	var out []corev1.Volume
	for _, v := range in {
		if v.Name != name {
			out = append(out, v)
		}
	}
	return out
}

func removeMount(in []corev1.VolumeMount, name string) []corev1.VolumeMount {
	var out []corev1.VolumeMount
	for _, m := range in {
		if m.Name != name {
			out = append(out, m)
		}
	}
	return out
}

func removeEnv(in []corev1.EnvVar, name string) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, e := range in {
		if e.Name != name {
			out = append(out, e)
		}
	}
	return out
}
