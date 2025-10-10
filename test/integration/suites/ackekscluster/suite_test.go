// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ackekscluster_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
)

var env *environment.Environment

func TestEKSCluster(t *testing.T) {
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		var err error
		env, err = environment.New(t.Context(),
			environment.ControllerConfig{
				AllowCRDDeletion: true,
				ReconcileConfig: ctrlinstance.ReconcileConfig{
					DefaultRequeueDuration: 15 * time.Second,
				},
			},
		)
		Expect(err).NotTo(HaveOccurred())
	})
	AfterSuite(func() {
		Expect(env.Stop()).NotTo(HaveOccurred())
	})

	RunSpecs(t, "EKSCluster Suite")
}

var _ = Describe("EKSCluster", func() {
	DescribeTableSubtree("apply mode",
		testEKS,
		Entry(string(krov1alpha1.ApplyModeApplySetSSA), krov1alpha1.ResourceGraphDefinitionReconcileSpec{
			ApplyMode: krov1alpha1.ApplyModeApplySetSSA,
		}, Label(string(krov1alpha1.ApplyModeApplySetSSA))),
		Entry(string(krov1alpha1.ApplyModeDeltaCSA), krov1alpha1.ResourceGraphDefinitionReconcileSpec{
			ApplyMode: krov1alpha1.ApplyModeDeltaCSA,
		}, Label(string(krov1alpha1.ApplyModeDeltaCSA))),
	)
})

func testEKS(reconcileSpec krov1alpha1.ResourceGraphDefinitionReconcileSpec) {
	It("should handle complete lifecycle of ResourceGraphDefinition and Instance", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())

		// Create ResourceGraphDefinition
		rgd, genInstance := eksCluster(namespace, "test-eks-cluster", reconcileSpec)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rgd.Name,
				Namespace: namespace,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("EKSCluster"))
			g.Expect(createdRGD.Spec.Schema.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(12)) // All resources from the generator

			g.Expect(createdRGD.Status.TopologicalOrder).To(Equal([]string{
				"clusterRole",
				"clusterVPC",
				"clusterInternetGateway",
				"clusterRouteTable",
				"clusterSubnetA",
				"clusterSubnetB",
				"cluster",
				"clusterAdminRole",
				"clusterElasticIPAddress",
				"clusterNATGateway",
				"clusterNodeRole",
				"clusterNodeGroup",
			}))

			// Verify the ResourceGraphDefinition status
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(12))
			// Verify ready condition.
			g.Expect(createdRGD.Status.Conditions).ShouldNot(BeEmpty())
			var readyCondition krov1alpha1.Condition
			for _, cond := range createdRGD.Status.Conditions {
				if cond.Type == resourcegraphdefinition.Ready {
					readyCondition = cond
				}
			}
			g.Expect(readyCondition).ToNot(BeNil())
			g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(readyCondition.ObservedGeneration).To(Equal(createdRGD.Generation))

			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance
		instance := genInstance(namespace, "test-instance", "1.27")
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Check if the instance is created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		roleGVK := schema.GroupVersionKind{
			Group:   "iam.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "Role",
		}
		clusterRole := &unstructured.Unstructured{}
		clusterRole.SetGroupVersionKind(roleGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-role",
				Namespace: namespace,
			}, clusterRole)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		clusterRole.Object["status"] = map[string]interface{}{
			"ackResourceMetadata": map[string]interface{}{
				"ownerAccountID": "123456789012",
				"region":         "us-west-2",
				"arn":            "arn:aws:iam::123456789012:role/kro-cluster-role",
			},
		}
		Expect(env.Client.Status().Update(ctx, clusterRole)).To(Succeed())

		// 2. Verify VPC
		vpcGVK := schema.GroupVersionKind{
			Group:   "ec2.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "VPC",
		}
		vpc := &unstructured.Unstructured{}
		vpc.SetGroupVersionKind(vpcGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-vpc",
				Namespace: namespace,
			}, vpc)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		vpc.Object["status"] = map[string]interface{}{
			"vpcID": "vpc-12345",
		}
		Expect(env.Client.Status().Update(ctx, vpc)).To(Succeed())

		// 3. Verify Internet Gateway
		igwGVK := schema.GroupVersionKind{
			Group:   "ec2.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "InternetGateway",
		}
		igw := &unstructured.Unstructured{}
		igw.SetGroupVersionKind(igwGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-igw",
				Namespace: namespace,
			}, igw)
			g.Expect(err).ToNot(HaveOccurred())

			vpcID, found, _ := unstructured.NestedString(igw.Object, "spec", "vpc")
			g.Expect(found).To(BeTrue())
			g.Expect(vpcID).To(Equal("vpc-12345"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		igw.Object["status"] = map[string]interface{}{
			"internetGatewayID": "igw-12345",
		}
		Expect(env.Client.Status().Update(ctx, igw)).To(Succeed())

		// 4. Verify Route Table
		rtGVK := schema.GroupVersionKind{
			Group:   "ec2.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "RouteTable",
		}
		rt := &unstructured.Unstructured{}
		rt.SetGroupVersionKind(rtGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-public-route-table",
				Namespace: namespace,
			}, rt)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		rt.Object["status"] = map[string]interface{}{
			"routeTableID": "rtb-12345",
		}
		Expect(env.Client.Status().Update(ctx, rt)).To(Succeed())

		// 5-6. Verify Subnets A and B
		subnetGVK := schema.GroupVersionKind{
			Group:   "ec2.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "Subnet",
		}

		// SubnetA
		subnetA := &unstructured.Unstructured{}
		subnetA.SetGroupVersionKind(subnetGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-public-subnet1",
				Namespace: namespace,
			}, subnetA)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		subnetA.Object["status"] = map[string]interface{}{
			"subnetID": "subnet-a12345",
		}
		Expect(env.Client.Status().Update(ctx, subnetA)).To(Succeed())

		// SubnetB
		subnetB := &unstructured.Unstructured{}
		subnetB.SetGroupVersionKind(subnetGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-public-subnet2",
				Namespace: namespace,
			}, subnetB)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		subnetB.Object["status"] = map[string]interface{}{
			"subnetID": "subnet-b12345",
		}
		Expect(env.Client.Status().Update(ctx, subnetB)).To(Succeed())

		// 7. Verify EKS Cluster
		clusterGVK := schema.GroupVersionKind{
			Group:   "eks.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "Cluster",
		}
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(clusterGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, cluster)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		cluster.Object["status"] = map[string]interface{}{
			"ackResourceMetadata": map[string]interface{}{
				"ownerAccountID": "123456789012",
				"region":         "us-west-2",
				"arn":            "arn:aws:eks:us-west-2:123456789012:cluster/test-instance",
			},
		}
		Expect(env.Client.Status().Update(ctx, cluster)).To(Succeed())

		// 8. Verify Admin Role
		adminRole := &unstructured.Unstructured{}
		adminRole.SetGroupVersionKind(roleGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-pia-role",
				Namespace: namespace,
			}, adminRole)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		adminRole.Object["status"] = map[string]interface{}{
			"ackResourceMetadata": map[string]interface{}{
				"ownerAccountID": "123456789012",
				"region":         "us-west-2",
				"arn":            "arn:aws:iam::123456789012:role/kro-cluster-pia-role",
			},
		}
		Expect(env.Client.Status().Update(ctx, adminRole)).To(Succeed())

		// 9. Verify Elastic IP
		eipGVK := schema.GroupVersionKind{
			Group:   "ec2.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "ElasticIPAddress",
		}
		eip := &unstructured.Unstructured{}
		eip.SetGroupVersionKind(eipGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-eip",
				Namespace: namespace,
			}, eip)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		eip.Object["status"] = map[string]interface{}{
			"allocationID": "eipalloc-12345",
		}
		Expect(env.Client.Status().Update(ctx, eip)).To(Succeed())

		// 10. Verify NAT Gateway
		natGVK := schema.GroupVersionKind{
			Group:   "ec2.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "NATGateway",
		}
		nat := &unstructured.Unstructured{}
		nat.SetGroupVersionKind(natGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-natgateway1",
				Namespace: namespace,
			}, nat)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		nat.Object["status"] = map[string]interface{}{
			"natGatewayID": "nat-12345",
		}
		Expect(env.Client.Status().Update(ctx, nat)).To(Succeed())

		// 11. Verify Node Role
		nodeRole := &unstructured.Unstructured{}
		nodeRole.SetGroupVersionKind(roleGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-node-role",
				Namespace: namespace,
			}, nodeRole)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		nodeRole.Object["status"] = map[string]interface{}{
			"ackResourceMetadata": map[string]interface{}{
				"ownerAccountID": "123456789012",
				"region":         "us-west-2",
				"arn":            "arn:aws:iam::123456789012:role/kro-cluster-node-role",
			},
		}
		Expect(env.Client.Status().Update(ctx, nodeRole)).To(Succeed())

		// 12. Verify Node Group
		nodeGroupGVK := schema.GroupVersionKind{
			Group:   "eks.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "Nodegroup",
		}
		nodeGroup := &unstructured.Unstructured{}
		nodeGroup.SetGroupVersionKind(nodeGroupGVK)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "kro-cluster-nodegroup",
				Namespace: namespace,
			}, nodeGroup)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify final instance status
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			networkingInfo, found, _ := unstructured.NestedMap(instance.Object, "status", "networkingInfo")
			g.Expect(found).To(BeTrue())
			g.Expect(networkingInfo["vpcID"]).To(Equal("vpc-12345"))
			g.Expect(networkingInfo["subnetAZA"]).To(Equal("subnet-a12345"))
			g.Expect(networkingInfo["subnetAZB"]).To(Equal("subnet-b12345"))

			clusterARN, found, _ := unstructured.NestedString(instance.Object, "status", "clusterARN")
			g.Expect(found).To(BeTrue())
			g.Expect(clusterARN).To(Equal("arn:aws:eks:us-west-2:123456789012:cluster/test-instance"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Before deletion, check version update
		// Store resource versions
		latestResources := make(map[string]*unstructured.Unstructured)
		for _, obj := range []*unstructured.Unstructured{
			vpc, igw, rt, subnetA, subnetB, cluster, adminRole, eip, nat, nodeRole, nodeGroup, clusterRole,
		} {
			latestResources[fmt.Sprintf("%s/%s", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())] = obj
		}

		// Update cluster version
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			spec := instance.Object["spec"].(map[string]interface{})
			spec["version"] = "1.28"
			err = env.Client.Update(ctx, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Wait and verify only cluster was updated
		time.Sleep(5 * time.Second)
		Eventually(func(g Gomega, ctx SpecContext) {

			for key, latestResource := range latestResources {
				kind := strings.Split(key, "/")[0]
				name := strings.Split(key, "/")[1]

				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(latestResource.GetObjectKind().GroupVersionKind())
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      name,
					Namespace: namespace,
				}, obj)
				g.Expect(err).ToNot(HaveOccurred())

				if kind == "Cluster" {
					g.Expect(obj.GetResourceVersion()).ToNot(Equal(latestResource.GetResourceVersion()),
						"Cluster should be updated for version change")
				} else {
					g.Expect(obj.GetResourceVersion()).To(Equal(latestResource.GetResourceVersion()),
						"Resource %s should not be updated during version change", key)
				}
			}
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Delete instance
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// Verify instance and all its resources are deleted
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Delete ResourceGraphDefinition
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is deleted
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rgd.Name,
				Namespace: namespace,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup namespace
		Expect(env.Client.Delete(ctx, ns)).To(Succeed())
	})

}
