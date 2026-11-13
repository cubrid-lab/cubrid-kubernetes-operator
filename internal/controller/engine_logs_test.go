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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// cubridLogDir is $CUBRID/log in the Instance Manager image, where CUBRID
// writes its own log files (docs/poc/RESULTS.md, POC-35).
const cubridLogDir = "/home/cubrid/CUBRID/log"

// dataClaim is the StatefulSet's data claim template.
const dataClaim = "data"

// logsTestImage is the image of the StatefulSets these tests build.
const logsTestImage = "registry.example/cubrid-instance-manager:logs"

// mountAt returns the mount of the container at path, if there is one.
func mountAt(c corev1.Container, path string) (corev1.VolumeMount, bool) {
	for _, m := range c.VolumeMounts {
		if m.MountPath == path {
			return m, true
		}
	}
	return corev1.VolumeMount{}, false
}

// A Broker keeps no access log, the only Broker log whose rotated copies were
// never removed in POC-35, and rotates each CAS SQL and slow-query log at a
// fixed size, so that its logs fit the emptyDir they are written to (#252).
func TestBrokerConfBoundsTheBrokerLogs(t *testing.T) {
	for _, mode := range []string{brokerModeRW, brokerModeRO} {
		conf := generateBrokerConf(mode, DefaultBrokerMaxApplServers)
		for _, want := range []string{"ACCESS_LOG              =OFF\n", "SQL_LOG_MAX_SIZE        =4096\n"} {
			if !strings.Contains(conf, want) {
				t.Errorf("%s Broker configuration lacks %q:\n%s", mode, want, conf)
			}
		}
	}
}

// A Broker Pod has no persistent volume: its logs are in an emptyDir, which
// a container restart keeps and a Pod deletion does not. The emptyDir has a
// size limit that holds every CAS's bounded logs, so a Broker with more CAS
// processes gets a larger one (#252).
func TestBrokerPodKeepsItsLogsInABoundedEmptyDir(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxServer int
		want      string
	}{
		{name: "default number of CAS processes", want: "256Mi"},
		{name: "twenty CAS processes", maxServer: 20, want: "532Mi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &CubridClusterReconciler{BrokerMaxApplServers: tc.maxServer}
			spec := r.brokerPodSpec(brokerTestCluster(), brokerModeRW, "registry.example/cubrid:test")
			mount, ok := mountAt(spec.Containers[0], cubridLogDir)
			if !ok {
				t.Fatalf("the Broker container mounts nothing at %s: %+v", cubridLogDir, spec.Containers[0].VolumeMounts)
			}
			var volume *corev1.Volume
			for i := range spec.Volumes {
				if spec.Volumes[i].Name == mount.Name {
					volume = &spec.Volumes[i]
				}
			}
			if volume == nil || volume.EmptyDir == nil {
				t.Fatalf("the volume %q at %s is not an emptyDir: %+v", mount.Name, cubridLogDir, volume)
			}
			if volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.Cmp(resource.MustParse(tc.want)) != 0 {
				t.Errorf("emptyDir sizeLimit = %v, want %s", volume.EmptyDir.SizeLimit, tc.want)
			}
		})
	}
}

var _ = Describe("CUBRID's log files on a volume of each member (#252, #856)", func() {
	ctx := context.Background()
	var r *CubridClusterReconciler

	BeforeEach(func() {
		r = &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(500),
			DefaultImage: logsTestImage,
		}
	})
	reconcileOnce := func(c *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		Expect(err).NotTo(HaveOccurred())
	}
	create := func(c *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	statefulSet := func(c *databasev1alpha1.CubridCluster) appsv1.StatefulSet {
		GinkgoHelper()
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), &sts)).To(Succeed())
		return sts
	}
	templateNames := func(sts appsv1.StatefulSet) []string {
		names := make([]string, 0, len(sts.Spec.VolumeClaimTemplates))
		for _, t := range sts.Spec.VolumeClaimTemplates {
			names = append(names, t.Name)
		}
		return names
	}

	It("gives a new StatefulSet only the data claim template and keeps $CUBRID/log in a subdirectory of the data volume", func() {
		c := standaloneCluster("logs-new")
		create(c)
		reconcileOnce(c)
		sts := statefulSet(c)

		// A second claim per member can be bound to a node on its own while
		// the data claim is not, which left a member of a new HA cluster
		// unschedulable for good, so a member has a single claim (#856).
		Expect(templateNames(sts)).To(Equal([]string{dataClaim}),
			"a new member must have one claim, so its claims can never be bound to different nodes")

		container := sts.Spec.Template.Spec.Containers[0]
		mount, ok := mountAt(container, cubridLogDir)
		Expect(ok).To(BeTrue(), "the database container mounts nothing at %s, so the logs would be lost with the Pod", cubridLogDir)
		Expect(mount.Name).To(Equal(dataClaim), "the logs are on the data volume")
		Expect(mount.SubPath).To(Equal("cubrid-log"), "the logs are in a subdirectory of their own on the data volume")
		data, _ := mountAt(container, dataMountPath)
		Expect(data.Name).To(Equal(dataClaim))
		Expect(data.SubPath).To(BeEmpty())

		By("leaving the StatefulSet as it is on the next reconcile")
		generation := sts.Generation
		reconcileOnce(c)
		Expect(statefulSet(c).Generation).To(Equal(generation))
	})

	It("keeps the log claim template and its mount on a StatefulSet created with one", func() {
		c := standaloneCluster("logs-claim")
		class := "log-test-class"
		c.Spec.Storage.Data.StorageClassName = &class
		create(c)
		// A StatefulSet an earlier build created with a log claim template
		// (#849). Its templates cannot change, and changing its Pod template
		// would restart every member, so it keeps mounting the log claim (#812).
		logClaim := corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: engineLogVolume},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &class,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		old := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace, Labels: labelsFor(c)},
			Spec: appsv1.StatefulSetSpec{
				Selector:             &metav1.LabelSelector{MatchLabels: labelsFor(c)},
				ServiceName:          instancesServiceName(c.Name),
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{r.dataPVCTemplate(c), logClaim},
				PodManagementPolicy:  appsv1.ParallelPodManagement,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(c)},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: appName, Image: logsTestImage,
						VolumeMounts: []corev1.VolumeMount{
							{Name: dataClaim, MountPath: dataMountPath},
							{Name: engineLogVolume, MountPath: cubridLogDir},
						},
					}}},
				},
			},
		}
		Expect(controllerutil.SetControllerReference(c, old, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, old)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, old))).To(Succeed()) })

		reconcileOnce(c)
		sts := statefulSet(c)
		Expect(templateNames(sts)).To(Equal([]string{dataClaim, engineLogVolume}))
		container := sts.Spec.Template.Spec.Containers[0]
		mount, ok := mountAt(container, cubridLogDir)
		Expect(ok).To(BeTrue(), "a member with a log claim must keep writing its logs there")
		Expect(mount.Name).To(Equal(engineLogVolume))
		Expect(mount.SubPath).To(BeEmpty())
		for _, m := range container.VolumeMounts {
			Expect(m.SubPath).To(BeEmpty(), "a member with a log claim must not also mount a subdirectory of its data volume: %+v", m)
		}

		By("not updating the StatefulSet again on the next reconcile")
		generation := sts.Generation
		reconcileOnce(c)
		Expect(statefulSet(c).Generation).To(Equal(generation))
	})

	It("leaves a StatefulSet created without the log claim template without one, and mounts nothing at $CUBRID/log", func() {
		c := standaloneCluster("logs-old")
		create(c)
		// A StatefulSet a build before #849 created, with no log claim
		// template and nothing mounted at $CUBRID/log: its members keep their
		// logs in the container, and adding a mount would restart them.
		old := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace, Labels: labelsFor(c)},
			Spec: appsv1.StatefulSetSpec{
				Selector:             &metav1.LabelSelector{MatchLabels: labelsFor(c)},
				ServiceName:          instancesServiceName(c.Name),
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{r.dataPVCTemplate(c)},
				PodManagementPolicy:  appsv1.ParallelPodManagement,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(c)},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: appName, Image: logsTestImage,
					}}},
				},
			},
		}
		Expect(controllerutil.SetControllerReference(c, old, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, old)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, old))).To(Succeed()) })

		reconcileOnce(c)
		sts := statefulSet(c)
		Expect(templateNames(sts)).To(Equal([]string{dataClaim}))
		_, mounted := mountAt(sts.Spec.Template.Spec.Containers[0], cubridLogDir)
		Expect(mounted).To(BeFalse(), "a StatefulSet created before log volumes must not get one mounted, which would restart its members")
		for _, v := range sts.Spec.Template.Spec.Volumes {
			Expect(v.Name).NotTo(Equal(engineLogVolume))
		}

		By("not updating the StatefulSet again on the next reconcile")
		generation := sts.Generation
		reconcileOnce(c)
		Expect(statefulSet(c).Generation).To(Equal(generation))
	})
})
