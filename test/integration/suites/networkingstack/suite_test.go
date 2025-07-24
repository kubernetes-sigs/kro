// Copyright 2025 The Kube Resource Orchestrator Authors
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

package networkingstack_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	ctrlinstance "github.com/kro-run/kro/pkg/controller/instance"
	"github.com/kro-run/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kro-run/kro/test/integration/environment"
)

var env *environment.Environment

func TestNetworkingStack(t *testing.T) {
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		var err error
		env, err = environment.New(
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

	RunSpecs(t, "NetworkingStack Suite")
}

var _ = Describe("NetworkingStack", func() {
	It("should handle complete lifecycle of ResourceGraphDefinition and Instance", func() {
		ctx := context.Background()
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())

		// Create ResourceGraphDefinition
		rgd, genInstance := networkingStack("test-networking-stack")
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("NetworkingStack"))
			g.Expect(createdRGD.Spec.Schema.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(5)) // vpc, 3 subnets, security group

			// Verify the ResourceGraphDefinition status
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(5))
			g.Expect(createdRGD.Status.TopologicalOrder).To(Equal([]string{
				"vpc",
				"securityGroup",
				"subnetAZA",
				"subnetAZB",
				"subnetAZC",
			}))
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
		}, 10*time.Second, time.Second).Should(Succeed())

		// Create instance
		instance := genInstance(namespace, "test-instance")
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Check if the instance is created
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).Should(Succeed())

		// Verify VPC creation
		vpcGVK := schema.GroupVersionKind{
			Group:   "ec2.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "VPC",
		}
		vpc := &unstructured.Unstructured{}
		vpc.SetGroupVersionKind(vpcGVK)

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("vpc-%s", instance.GetName()),
				Namespace: namespace,
			}, vpc)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).Should(Succeed())

		// Mock VPC status
		vpc.Object["status"] = map[string]interface{}{
			"vpcID": "vpc-12345",
		}
		Expect(env.Client.Status().Update(ctx, vpc)).To(Succeed())

		// Verify Security Group creation
		sgGVK := schema.GroupVersionKind{
			Group:   "ec2.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "SecurityGroup",
		}
		sg := &unstructured.Unstructured{}
		sg.SetGroupVersionKind(sgGVK)

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("security-group-%s", instance.GetName()),
				Namespace: namespace,
			}, sg)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify security group spec
			vpcID, found, _ := unstructured.NestedString(sg.Object, "spec", "vpcID")
			g.Expect(found).To(BeTrue())
			g.Expect(vpcID).To(Equal("vpc-12345"))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Mock security group status
		sg.Object["status"] = map[string]interface{}{
			"id": "sg-12345",
		}
		Expect(env.Client.Status().Update(ctx, sg)).To(Succeed())

		subnetGVK := schema.GroupVersionKind{
			Group:   "ec2.services.k8s.aws",
			Version: "v1alpha1",
			Kind:    "Subnet",
		}

		// Verify each subnet
		subnets := []struct {
			name string
			id   string
		}{
			{fmt.Sprintf("subnet-a-%s", instance.GetName()), "subnet-a12345"},
			{fmt.Sprintf("subnet-b-%s", instance.GetName()), "subnet-b12345"},
			{fmt.Sprintf("subnet-c-%s", instance.GetName()), "subnet-c12345"},
		}

		for _, s := range subnets {
			subnet := &unstructured.Unstructured{}
			subnet.SetGroupVersionKind(subnetGVK)
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      s.name,
					Namespace: namespace,
				}, subnet)
				g.Expect(err).ToNot(HaveOccurred())

				// Verify subnet spec
				vpcID, found, _ := unstructured.NestedString(subnet.Object, "spec", "vpcID")
				g.Expect(found).To(BeTrue())
				g.Expect(vpcID).To(Equal("vpc-12345"))
			}, 20*time.Second, time.Second).Should(Succeed())

			// Mock subnet status
			subnet.Object["status"] = map[string]interface{}{
				"subnetID": s.id,
			}
			Expect(env.Client.Status().Update(ctx, subnet)).To(Succeed())
		}

		// Verify instance status is updated with networking info
		Eventually(func(g Gomega) {
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
			g.Expect(networkingInfo["subnetAZC"]).To(Equal("subnet-c12345"))
			g.Expect(networkingInfo["securityGroup"]).To(Equal("sg-12345"))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Delete instance
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// Verify resources are deleted
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("vpc-%s", instance.GetName()),
				Namespace: namespace,
			}, vpc)
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("cluster-security-group-%s", instance.GetName()),
				Namespace: namespace,
			}, sg)
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		// Verify instance is deleted
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		// Delete ResourceGraphDefinition
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is deleted
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		// Cleanup namespace
		Expect(env.Client.Delete(ctx, ns)).To(Succeed())
	})
})
