// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.
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

	symphonyv1alpha1 "github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	ctrlinstance "github.com/aws-controllers-k8s/symphony/internal/controller/instance"
	"github.com/aws-controllers-k8s/symphony/test/integration/environment"
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
	It("should handle complete lifecycle of ResourceGroup and Instance", func() {
		ctx := context.Background()
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())

		// Create ResourceGroup
		rg, genInstance := networkingStack("test-networking-stack", namespace)
		Expect(env.Client.Create(ctx, rg)).To(Succeed())

		// Verify ResourceGroup is created and becomes ready
		createdRG := &symphonyv1alpha1.ResourceGroup{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rg.Name,
				Namespace: namespace,
			}, createdRG)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGroup fields
			g.Expect(createdRG.Spec.Kind).To(Equal("NetworkingStack"))
			g.Expect(createdRG.Spec.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRG.Spec.Resources).To(HaveLen(5)) // vpc, 3 subnets, security group

			// Verify the ResourceGroup status
			g.Expect(createdRG.Status.TopoligicalOrder).To(HaveLen(5))
			g.Expect(createdRG.Status.TopoligicalOrder).To(Equal([]string{
				"vpc",
				"securityGroup",
				"subnetAZA",
				"subnetAZB",
				"subnetAZC",
			}))
			g.Expect(createdRG.Status.Conditions).To(HaveLen(3))
			g.Expect(createdRG.Status.Conditions[0].Type).To(Equal(symphonyv1alpha1.ResourceGroupConditionTypeReconcilerReady))
			g.Expect(createdRG.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRG.Status.Conditions[1].Type).To(Equal(symphonyv1alpha1.ResourceGroupConditionTypeGraphVerified))
			g.Expect(createdRG.Status.Conditions[1].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRG.Status.Conditions[2].Type).To(Equal(symphonyv1alpha1.ResourceGroupConditionTypeCustomResourceDefinitionSynced))
			g.Expect(createdRG.Status.Conditions[2].Status).To(Equal(metav1.ConditionTrue))

			g.Expect(createdRG.Status.State).To(Equal(symphonyv1alpha1.ResourceGroupStateActive))
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

		// Delete ResourceGroup
		Expect(env.Client.Delete(ctx, rg)).To(Succeed())

		// Verify ResourceGroup is deleted
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rg.Name,
				Namespace: namespace,
			}, &symphonyv1alpha1.ResourceGroup{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		// Cleanup namespace
		Expect(env.Client.Delete(ctx, ns)).To(Succeed())
	})
})
