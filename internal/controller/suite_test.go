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
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = databasev1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	testEnv.ControlPlane.GetAPIServer().Configure().Set("service-cluster-ip-range", serviceIPRange)

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	By("checking that the API server allocates from the configured Service range")
	// An address outside the default 10.0.0.0/24 is accepted only when the
	// range above took effect.
	probe := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "service-range-probe", Namespace: metav1.NamespaceDefault},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.0.255.10",
			Ports:     []corev1.ServicePort{{Port: 80}},
		},
	}
	Expect(k8sClient.Create(ctx, probe)).To(Succeed())
	Expect(k8sClient.Delete(ctx, probe)).To(Succeed())
})

var _ = AfterSuite(func() {
	By("checking that the suite's Services left room in the Service IP range")
	// envtest runs no garbage collector, so every Service the specs create
	// keeps its ClusterIP until the suite ends. A range that fills up fails
	// whichever spec comes next, in random order (#530).
	Expect(clusterIPServices()).To(BeNumerically("<", serviceIPCapacity/2),
		"ClusterIP Services use more than half of the %d addresses of %s", serviceIPCapacity, serviceIPRange)

	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// serviceIPRange is the API server's Service range, and serviceIPCapacity the
// number of ClusterIPs it can allocate (the network and broadcast addresses
// and the kubernetes Service excluded).
const (
	serviceIPRange    = "10.0.0.0/16"
	serviceIPCapacity = 1<<16 - 3
)

// clusterIPServices counts the Services that hold a ClusterIP.
func clusterIPServices() int {
	var services corev1.ServiceList
	Expect(k8sClient.List(context.Background(), &services)).To(Succeed())
	n := 0
	for _, s := range services.Items {
		if s.Spec.ClusterIP != "" && s.Spec.ClusterIP != corev1.ClusterIPNone {
			n++
		}
	}
	return n
}

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
