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

package core_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Data Pending", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should surface data pending condition when resources depend on unavailable status fields", func(ctx SpecContext) {
		// This test creates a resource graph with chained dependencies using ACK resources:
		// - VPC: no dependencies, has status.vpcID that downstream resources need
		// - InternetGateway: depends on vpc.status.vpcID
		// - Subnet: depends on vpc.status.vpcID
		// - NATGateway: depends on subnet.status.subnetID (chain dependency)
		//
		// When VPC is first created, its status is empty. The controller should:
		// 1. Create VPC immediately
		// 2. Show "data pending" / "unresolved" in conditions while waiting for status
		// 3. Create InternetGateway and Subnet once VPC status is populated
		// 4. Create NATGateway once Subnet status is populated

		rgd := generator.NewResourceGraphDefinition("test-data-pending",
			generator.WithSchema(
				"TestDataPending", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				map[string]interface{}{
					"vpcID":    "${vpc.status.vpcID}",
					"subnetID": "${subnet.status.subnetID}",
				},
			),
			// VPC - no dependencies
			generator.WithResource("vpc", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "VPC",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-vpc",
				},
				"spec": map[string]interface{}{
					"cidrBlocks": []interface{}{
						"10.0.0.0/16",
					},
				},
			}, nil, nil),
			// InternetGateway - depends on vpc.status.vpcID
			generator.WithResource("igw", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "InternetGateway",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-igw",
				},
				"spec": map[string]interface{}{
					"vpc": "${vpc.status.vpcID}",
				},
			}, nil, nil),
			// Subnet - depends on vpc.status.vpcID
			generator.WithResource("subnet", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "Subnet",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-subnet",
				},
				"spec": map[string]interface{}{
					"vpcID":     "${vpc.status.vpcID}",
					"cidrBlock": "10.0.1.0/24",
				},
			}, nil, nil),
			// NATGateway - depends on subnet.status.subnetID (chain dependency)
			generator.WithResource("natgw", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "NATGateway",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-natgw",
				},
				"spec": map[string]interface{}{
					"subnetID": "${subnet.status.subnetID}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		// Wait for RGD to be ready
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())

			var readyCondition *krov1alpha1.Condition
			for _, cond := range rgd.Status.Conditions {
				if cond.Type == resourcegraphdefinition.Ready {
					readyCondition = &cond
					break
				}
			}
			g.Expect(readyCondition).ToNot(BeNil())
			g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

			// Verify topological order - VPC should be first
			g.Expect(rgd.Status.TopologicalOrder).To(HaveLen(4))
			g.Expect(rgd.Status.TopologicalOrder[0]).To(Equal("vpc"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance
		instanceName := "test-data-pending"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestDataPending",
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": instanceName,
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, instance)
		})

		// Step 1: VPC should be created immediately (no dependencies)
		vpc := &unstructured.Unstructured{}
		vpc.SetAPIVersion("ec2.services.k8s.aws/v1alpha1")
		vpc.SetKind("VPC")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-vpc",
				Namespace: namespace,
			}, vpc)
			g.Expect(err).ToNot(HaveOccurred())
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Step 2: Instance should be IN_PROGRESS with ResourcesReady=False and message indicating unresolved resources
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// Check state is IN_PROGRESS
			state, found, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("IN_PROGRESS"))

			// Check ResourcesReady condition is False with message about unresolved resources
			statusConditions, found, _ := unstructured.NestedSlice(instance.Object, "status", "conditions")
			g.Expect(found).To(BeTrue())

			var resourcesReadyCondition map[string]interface{}
			for _, condInterface := range statusConditions {
				if cond, ok := condInterface.(map[string]interface{}); ok {
					if cond["type"] == ctrlinstance.ResourcesReady {
						resourcesReadyCondition = cond
						break
					}
				}
			}
			g.Expect(resourcesReadyCondition).ToNot(BeNil(), "ResourcesReady condition should exist")
			g.Expect(resourcesReadyCondition["status"]).To(
				Equal("False"), "ResourcesReady should be False while data is pending")

			// CRITICAL: Assert that the message indicates data is pending/unresolved
			msg, _ := resourcesReadyCondition["message"].(string)
			g.Expect(msg).To(
				ContainSubstring("unresolved"), "Message should indicate unresolved dependencies")
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Step 3: InternetGateway and Subnet should NOT exist yet
		igw := &unstructured.Unstructured{}
		igw.SetAPIVersion("ec2.services.k8s.aws/v1alpha1")
		igw.SetKind("InternetGateway")
		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-igw",
				Namespace: namespace,
			}, igw)
			g.Expect(err).To(MatchError(errors.IsNotFound, "igw should not exist while vpc.status.vpcID is missing"))
		}, 5*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Step 4: Simulate VPC controller populating status.vpcID
		Expect(unstructured.SetNestedField(vpc.Object, "vpc-12345", "status", "vpcID")).To(Succeed())
		Expect(env.Client.Status().Update(ctx, vpc)).To(Succeed())

		// Step 5: InternetGateway and Subnet should now be created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-igw",
				Namespace: namespace,
			}, igw)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify it has the correct vpcID
			vpcID, found, _ := unstructured.NestedString(igw.Object, "spec", "vpc")
			g.Expect(found).To(BeTrue())
			g.Expect(vpcID).To(Equal("vpc-12345"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		subnet := &unstructured.Unstructured{}
		subnet.SetAPIVersion("ec2.services.k8s.aws/v1alpha1")
		subnet.SetKind("Subnet")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-subnet",
				Namespace: namespace,
			}, subnet)
			g.Expect(err).ToNot(HaveOccurred())

			vpcID, found, _ := unstructured.NestedString(subnet.Object, "spec", "vpcID")
			g.Expect(found).To(BeTrue())
			g.Expect(vpcID).To(Equal("vpc-12345"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Step 6: NATGateway should still NOT exist (waiting for subnet.status.subnetID)
		natgw := &unstructured.Unstructured{}
		natgw.SetAPIVersion("ec2.services.k8s.aws/v1alpha1")
		natgw.SetKind("NATGateway")
		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-natgw",
				Namespace: namespace,
			}, natgw)
			g.Expect(err).To(MatchError(errors.IsNotFound, "natgw should not exist while subnet.status.subnetID is missing"))
		}, 5*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Step 7: Simulate Subnet controller populating status.subnetID
		subnetNN := types.NamespacedName{Name: instanceName + "-subnet", Namespace: namespace}
		Expect(env.Client.Get(ctx, subnetNN, subnet)).To(Succeed())
		Expect(unstructured.SetNestedField(subnet.Object, "subnet-67890", "status", "subnetID")).To(Succeed())
		Expect(env.Client.Status().Update(ctx, subnet)).To(Succeed())

		// Step 8: NATGateway should now be created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-natgw",
				Namespace: namespace,
			}, natgw)
			g.Expect(err).ToNot(HaveOccurred())

			subnetID, found, _ := unstructured.NestedString(natgw.Object, "spec", "subnetID")
			g.Expect(found).To(BeTrue())
			g.Expect(subnetID).To(Equal("subnet-67890"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Step 9: Instance should become ACTIVE with all conditions True
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			state, found, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))

			// Verify status fields are populated
			vpcID, found, _ := unstructured.NestedString(instance.Object, "status", "vpcID")
			g.Expect(found).To(BeTrue())
			g.Expect(vpcID).To(Equal("vpc-12345"))

			subnetID, found, _ := unstructured.NestedString(instance.Object, "status", "subnetID")
			g.Expect(found).To(BeTrue())
			g.Expect(subnetID).To(Equal("subnet-67890"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should create independent resources in parallel while waiting for dependent data", func(ctx SpecContext) {
		// This test verifies that independent resources are created even when
		// other resources are blocked waiting for data. The graph looks like:
		//
		// vpcA (no deps) --> subnetA (depends on vpcA.status.vpcID)
		// vpcB (no deps) --> subnetB (depends on vpcB.status.vpcID)
		//
		// Both VPCs should be created immediately.
		// SubnetA should wait for vpcA.status, SubnetB should wait for vpcB.status.
		// When vpcA.status is populated, subnetA should be created (even if vpcB.status is missing)

		rgd := generator.NewResourceGraphDefinition("test-parallel-pending",
			generator.WithSchema(
				"TestParallelPending", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			// vpcA - independent, no dependencies
			generator.WithResource("vpcA", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "VPC",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-vpc-a",
				},
				"spec": map[string]interface{}{
					"cidrBlocks": []interface{}{"10.0.0.0/16"},
				},
			}, nil, nil),
			// vpcB - independent, no dependencies
			generator.WithResource("vpcB", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "VPC",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-vpc-b",
				},
				"spec": map[string]interface{}{
					"cidrBlocks": []interface{}{"10.1.0.0/16"},
				},
			}, nil, nil),
			// subnetA - depends on vpcA.status.vpcID
			generator.WithResource("subnetA", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "Subnet",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-subnet-a",
				},
				"spec": map[string]interface{}{
					"vpcID":     "${vpcA.status.vpcID}",
					"cidrBlock": "10.0.1.0/24",
				},
			}, nil, nil),
			// subnetB - depends on vpcB.status.vpcID
			generator.WithResource("subnetB", map[string]interface{}{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "Subnet",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-subnet-b",
				},
				"spec": map[string]interface{}{
					"vpcID":     "${vpcB.status.vpcID}",
					"cidrBlock": "10.1.1.0/24",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		// Wait for RGD to be ready
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance
		instanceName := "test-parallel"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestParallelPending",
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": instanceName,
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, instance)
		})

		// Both VPCs should be created immediately
		vpcA := &unstructured.Unstructured{}
		vpcA.SetAPIVersion("ec2.services.k8s.aws/v1alpha1")
		vpcA.SetKind("VPC")
		vpcB := &unstructured.Unstructured{}
		vpcB.SetAPIVersion("ec2.services.k8s.aws/v1alpha1")
		vpcB.SetKind("VPC")

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-vpc-a",
				Namespace: namespace,
			}, vpcA)
			g.Expect(err).ToNot(HaveOccurred())

			err = env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-vpc-b",
				Namespace: namespace,
			}, vpcB)
			g.Expect(err).ToNot(HaveOccurred())
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Subnets should NOT exist yet
		subnetA := &unstructured.Unstructured{}
		subnetA.SetAPIVersion("ec2.services.k8s.aws/v1alpha1")
		subnetA.SetKind("Subnet")
		subnetB := &unstructured.Unstructured{}
		subnetB.SetAPIVersion("ec2.services.k8s.aws/v1alpha1")
		subnetB.SetKind("Subnet")

		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName + "-subnet-a", Namespace: namespace}, subnetA)
			g.Expect(err).To(MatchError(errors.IsNotFound, "subnetA should not exist"))

			err = env.Client.Get(ctx, types.NamespacedName{Name: instanceName + "-subnet-b", Namespace: namespace}, subnetB)
			g.Expect(err).To(MatchError(errors.IsNotFound, "subnetB should not exist"))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Populate vpcA.status.vpcID (but NOT vpcB.status.vpcID)
		Expect(unstructured.SetNestedField(vpcA.Object, "vpc-aaaa", "status", "vpcID")).To(Succeed())
		Expect(env.Client.Status().Update(ctx, vpcA)).To(Succeed())

		// subnetA should now be created (vpcA's status is available)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-subnet-a",
				Namespace: namespace,
			}, subnetA)
			g.Expect(err).ToNot(HaveOccurred())

			vpcID, found, _ := unstructured.NestedString(subnetA.Object, "spec", "vpcID")
			g.Expect(found).To(BeTrue())
			g.Expect(vpcID).To(Equal("vpc-aaaa"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// subnetB should STILL not exist (vpcB's status is still missing)
		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName + "-subnet-b", Namespace: namespace}, subnetB)
			g.Expect(err).To(MatchError(errors.IsNotFound, "subnetB should still not exist"))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Now populate vpcB.status.vpcID
		vpcBNN := types.NamespacedName{Name: instanceName + "-vpc-b", Namespace: namespace}
		Expect(env.Client.Get(ctx, vpcBNN, vpcB)).To(Succeed())
		Expect(unstructured.SetNestedField(vpcB.Object, "vpc-bbbb", "status", "vpcID")).To(Succeed())
		Expect(env.Client.Status().Update(ctx, vpcB)).To(Succeed())

		// subnetB should now be created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-subnet-b",
				Namespace: namespace,
			}, subnetB)
			g.Expect(err).ToNot(HaveOccurred())

			vpcID, found, _ := unstructured.NestedString(subnetB.Object, "spec", "vpcID")
			g.Expect(found).To(BeTrue())
			g.Expect(vpcID).To(Equal("vpc-bbbb"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Instance should become ACTIVE
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			state, found, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
