// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package core_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	symphonyv1alpha1 "github.com/awslabs/symphony/api/v1alpha1"
	"github.com/awslabs/symphony/internal/testutil/generator"
)

var _ = Describe("Topology", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should correctly order AWS resources in dependency graph", func() {
		rg := generator.NewResourceGroup("test-topology",
			generator.WithNamespace(namespace),
			generator.WithKind("TestTopology", "v1alpha1"),
			generator.WithDefinition(
				map[string]interface{}{
					"name":    "string",
					"version": "string",
				},
				map[string]interface{}{
					"clusterStatus": "${cluster.status.status}",
				},
			),
			// IAM Role - no dependencies
			generator.WithResource("clusterRole", map[string]interface{}{
				"apiVersion": "iam.services.k8s.aws/v1alpha1",
				"kind":       "Role",
				"metadata": map[string]interface{}{
					"name": "test-cluster-role",
				},
				"spec": map[string]interface{}{
					"name": "test-cluster-role",
					"policies": []interface{}{
						"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy",
					},
				},
			}, nil, nil),
			// VPC - no dependencies
			generator.WithResource("vpc", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "VPC",
				"metadata": map[string]interface{}{
					"name": "test-vpc",
				},
				"spec": map[string]interface{}{
					"cidrBlocks": []interface{}{
						"192.168.0.0/16",
					},
					"enableDNSHostnames": true,
					"enableDNSSupport":   true,
				},
			}, nil, nil),
			// Subnet A - depends on VPC
			generator.WithResource("subnetA", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "Subnet",
				"metadata": map[string]interface{}{
					"name": "test-subnet-a",
				},
				"spec": map[string]interface{}{
					"availabilityZone": "us-west-2a",
					"cidrBlock":        "192.168.0.0/18",
					"vpcID":            "${vpc.status.vpcID}",
				},
			}, nil, nil),
			// Subnet B - depends on VPC
			generator.WithResource("subnetB", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "Subnet",
				"metadata": map[string]interface{}{
					"name": "test-subnet-b",
				},
				"spec": map[string]interface{}{
					"availabilityZone": "us-west-2b",
					"cidrBlock":        "192.168.64.0/18",
					"vpcID":            "${vpc.status.vpcID}",
				},
			}, nil, nil),
			// Cluster - depends on VPC, Subnets, and IAM Role
			generator.WithResource("cluster", map[string]interface{}{
				"apiVersion": "eks.services.k8s.aws/v1alpha1",
				"kind":       "Cluster",
				"metadata": map[string]interface{}{
					"name": "${spec.name}",
				},
				"spec": map[string]interface{}{
					"name":    "${spec.name}",
					"roleARN": "${clusterRole.status.ackResourceMetadata.arn}",
					"version": "${spec.version}",
					"resourcesVPCConfig": map[string]interface{}{
						"subnetIDs": []interface{}{
							"${subnetA.status.subnetID}",
							"${subnetB.status.subnetID}",
						},
						"endpointPrivateAccess": false,
						"endpointPublicAccess":  true,
					},
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rg)).To(Succeed())

		// Verify ResourceGroup topology
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rg.Name,
				Namespace: namespace,
			}, rg)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify graph is verified
			var graphCondition *symphonyv1alpha1.Condition
			for _, cond := range rg.Status.Conditions {
				if cond.Type == symphonyv1alpha1.ResourceGroupConditionTypeGraphVerified {
					graphCondition = &cond
					break
				}
			}
			g.Expect(graphCondition).ToNot(BeNil())
			g.Expect(graphCondition.Status).To(Equal(metav1.ConditionTrue))

			// Verify topological order
			g.Expect(rg.Status.TopologicalOrder).To(HaveLen(5))
			g.Expect(rg.Status.TopologicalOrder).To(Equal([]string{
				"clusterRole",
				"vpc",
				"subnetA",
				"subnetB",
				"cluster",
			}))
		}, 10*time.Second, time.Second).Should(Succeed())
	})

	It("should detect cyclic dependencies in AWS resource definitions", func() {
		rg := generator.NewResourceGroup("test-topology-cyclic",
			generator.WithNamespace(namespace),
			generator.WithKind("TestTopologyCyclic", "v1alpha1"),
			generator.WithDefinition(
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithResource("vpc", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "VPC",
				"metadata": map[string]interface{}{
					"name": "${subnet.status.subnetID}", // Creating cyclic dependency
				},
				"spec": map[string]interface{}{
					"cidrBlocks": []interface{}{
						"192.168.0.0/16",
					},
				},
			}, nil, nil),
			generator.WithResource("subnet", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "Subnet",
				"metadata": map[string]interface{}{
					"name": "test-subnet",
				},
				"spec": map[string]interface{}{
					"vpcID":     "${vpc.status.vpcID}", // Creating cyclic dependency
					"cidrBlock": "192.168.1.0/24",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rg)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rg.Name,
				Namespace: namespace,
			}, rg)
			g.Expect(err).ToNot(HaveOccurred())

			var graphCondition *symphonyv1alpha1.Condition
			for _, cond := range rg.Status.Conditions {
				if cond.Type == symphonyv1alpha1.ResourceGroupConditionTypeGraphVerified {
					graphCondition = &cond
					break
				}
			}
			g.Expect(graphCondition).ToNot(BeNil())
			g.Expect(graphCondition.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(*graphCondition.Reason).To(ContainSubstring("This would create a cycle"))
			g.Expect(rg.Status.State).To(Equal(symphonyv1alpha1.ResourceGroupStateInactive))
		}, 10*time.Second, time.Second).Should(Succeed())
	})
})
