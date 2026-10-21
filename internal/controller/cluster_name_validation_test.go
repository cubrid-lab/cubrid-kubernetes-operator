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
	"fmt"
	"math"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// A CUBRID cluster name must keep every name and label value the operator and
// the built-in controllers derive from it within the Kubernetes 63-character
// limits. The longest is the StatefulSet's controller-revision-hash Pod label,
// <cluster>-<hash of up to 10 characters>, so the API refuses cluster names
// longer than 52 characters or names that violate RFC 1035 DNS label
// constraints (#459).
var _ = Describe("Cluster name validation (#459)", func() {
	ctx := context.Background()

	dryRun := func(c *databasev1alpha1.CubridCluster) error {
		return k8sClient.Create(ctx, c, client.DryRunAll)
	}

	It("accepts a 52-character name and refuses a 53-character name in a standalone CubridCluster", func() {
		longest := strings.Repeat("a", 52)
		tooLong := strings.Repeat("a", 53)

		Expect(dryRun(standaloneCluster(longest))).To(Succeed())
		Expect(dryRun(standaloneCluster(tooLong))).
			To(MatchError(ContainSubstring("at most 52 characters")))
	})

	It("accepts a 52-character name and refuses a 53-character or 61-character name in an HA CubridCluster", func() {
		longest := strings.Repeat("a", 52)
		tooLong53 := strings.Repeat("a", 53)
		tooLong61 := strings.Repeat("a", 61)

		Expect(dryRun(haCluster(longest))).To(Succeed())
		Expect(dryRun(haCluster(tooLong53))).
			To(MatchError(ContainSubstring("at most 52 characters")))
		Expect(dryRun(haCluster(tooLong61))).
			To(MatchError(ContainSubstring("at most 52 characters")))
	})

	It("refuses cluster names with invalid DNS-1035 syntax", func() {
		// Dots are valid in generic CR metadata.name (DNS subdomain) but invalid in Service names.
		Expect(dryRun(standaloneCluster("prod.example"))).
			To(MatchError(ContainSubstring("DNS-1035")))
		// Must start with an alphabetic character.
		Expect(dryRun(standaloneCluster("1cluster"))).
			To(MatchError(ContainSubstring("DNS-1035")))
		// Must not end with a hyphen.
		Expect(dryRun(standaloneCluster("cluster-"))).
			To(MatchError(ContainSubstring("DNS-1035")))
		// Must not contain uppercase characters.
		Expect(dryRun(standaloneCluster("Cluster"))).
			To(MatchError(ContainSubstring("DNS-1035")))
		// Must not contain underscores.
		Expect(dryRun(standaloneCluster("cluster_name"))).
			To(MatchError(ContainSubstring("DNS-1035")))
	})

	It("accepts valid short and standard kebab-case cluster names", func() {
		Expect(dryRun(standaloneCluster("c"))).To(Succeed())
		Expect(dryRun(standaloneCluster("my-cubrid-01"))).To(Succeed())
		Expect(dryRun(haCluster("my-cubrid-ha"))).To(Succeed())
	})

	It("verifies that all child resource names for the 52-character boundary conform to Kubernetes limits", func() {
		name := strings.Repeat("a", databasev1alpha1.MaxClusterNameLength)
		Expect(name).To(HaveLen(52))
		ha := haCluster(name)

		// Governing headless Service name (<cluster>-instances).
		govSvc := instancesServiceName(name)
		Expect(govSvc).To(HaveLen(62))
		Expect(validation.IsDNS1035Label(govSvc)).To(BeEmpty())

		// Per-member alias Service names (<cluster>-<ordinal>), which are also
		// the Pod names and the statefulset.kubernetes.io/pod-name label values.
		members, err := haMemberNames(ha)
		Expect(err).NotTo(HaveOccurred())
		Expect(members).To(HaveLen(3))
		for _, m := range members {
			Expect(m).To(HaveLen(54))
			Expect(validation.IsDNS1035Label(m)).To(BeEmpty())
			Expect(validation.IsDNS1123Label(m)).To(BeEmpty())
			Expect(validation.IsValidLabelValue(m)).To(BeEmpty())
		}

		// IM token secret (<cluster>-im-token).
		imSecret := imTokenSecretName(name)
		Expect(imSecret).To(HaveLen(61))
		Expect(validation.IsDNS1123Subdomain(imSecret)).To(BeEmpty())

		// HA ConfigMap (<cluster>-ha-config).
		haCM := haConfigMapName(name)
		Expect(haCM).To(HaveLen(62))
		Expect(validation.IsDNS1123Subdomain(haCM)).To(BeEmpty())

		// NetworkPolicies.
		Expect(databaseNetworkPolicyName(name)).To(HaveLen(61))
		Expect(brokerNetworkPolicyName(name)).To(HaveLen(59))

		// Broker resources.
		Expect(brokerConfigMapName(name)).To(HaveLen(66))
		Expect(validation.IsDNS1123Subdomain(brokerConfigMapName(name))).To(BeEmpty())
		Expect(rwServiceName(name)).To(HaveLen(55))
		Expect(validation.IsDNS1035Label(rwServiceName(name))).To(BeEmpty())
		Expect(roServiceName(name)).To(HaveLen(55))
		Expect(validation.IsDNS1035Label(roServiceName(name))).To(BeEmpty())
		for _, mode := range []string{"rw", "ro"} {
			dep := brokerDeploymentName(name, mode)
			Expect(dep).To(HaveLen(62))
			Expect(validation.IsDNS1123Label(dep)).To(BeEmpty())
			// The Deployment controller names each ReplicaSet
			// <deployment>-<pod-template-hash> and sets the hash alone as the
			// pod-template-hash label value of the ReplicaSet's Pods.
			rs := dep + "-" + longestControllerHash
			Expect(rs).To(HaveLen(73))
			Expect(validation.IsDNS1123Subdomain(rs)).To(BeEmpty())
			Expect(validation.IsValidLabelValue(longestControllerHash)).To(BeEmpty())
		}

		// StatefulSet name and PVC volumeClaimTemplate naming: data-<cluster>-0.
		pvcName := "data-" + name + "-0"
		Expect(pvcName).To(HaveLen(59))
		Expect(validation.IsDNS1123Subdomain(pvcName)).To(BeEmpty())
	})

	// The StatefulSet is named after the cluster, and the StatefulSet
	// controller labels each Pod controller-revision-hash=<statefulset>-<hash>.
	// envtest runs no StatefulSet controller, so the label is computed here.
	It("keeps the StatefulSet controller-revision-hash Pod label within 63 characters exactly up to the limit", func() {
		revisionLabel := func(cluster string) string { return cluster + "-" + longestControllerHash }

		longest := revisionLabel(strings.Repeat("a", databasev1alpha1.MaxClusterNameLength))
		Expect(longest).To(HaveLen(63))
		Expect(validation.IsValidLabelValue(longest)).To(BeEmpty())

		// One character more is no longer a valid label value: the limit is tight.
		over := revisionLabel(strings.Repeat("a", databasev1alpha1.MaxClusterNameLength+1))
		Expect(validation.IsValidLabelValue(over)).NotTo(BeEmpty())
	})
})

// longestControllerHash is the longest hash the StatefulSet and Deployment
// controllers put in generated names and labels: both encode the decimal form
// of a 32-bit FNV hash with rand.SafeEncodeString, which maps each character
// to one character, so the largest uint32 gives the longest result.
var longestControllerHash = rand.SafeEncodeString(fmt.Sprint(uint32(math.MaxUint32)))
