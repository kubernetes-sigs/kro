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
package deploymentservice_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	symphonyv1alpha1 "github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	ctrlinstance "github.com/aws-controllers-k8s/symphony/internal/controller/instance"
	"github.com/aws-controllers-k8s/symphony/test/integration/environment"
)

var env *environment.Environment

func TestDeploymentservice(t *testing.T) {
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

	RunSpecs(t, "DeploymentService Suite")
}

var _ = Describe("DeploymentService", func() {
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
		rg, genInstance := deploymentService(namespace, "test-deployment-service")
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
			g.Expect(createdRG.Spec.Kind).To(Equal("DeploymentService"))
			g.Expect(createdRG.Spec.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRG.Spec.Resources).To(HaveLen(2))

			// Verify the ResourceGroup status
			g.Expect(createdRG.Status.Conditions).To(HaveLen(3))
			g.Expect(createdRG.Status.Conditions[0].Type).To(Equal(symphonyv1alpha1.ResourceGroupConditionTypeReconcilerReady))
			g.Expect(createdRG.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRG.Status.Conditions[1].Type).To(Equal(symphonyv1alpha1.ResourceGroupConditionTypeGraphVerified))
			g.Expect(createdRG.Status.Conditions[1].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRG.Status.Conditions[2].Type).To(
				Equal(symphonyv1alpha1.ResourceGroupConditionTypeCustomResourceDefinitionSynced),
			)
			g.Expect(createdRG.Status.Conditions[2].Status).To(Equal(metav1.ConditionTrue))

			g.Expect(createdRG.Status.State).To(Equal(symphonyv1alpha1.ResourceGroupStateActive))
			g.Expect(createdRG.Status.TopoligicalOrder).To(HaveLen(2))
			g.Expect(createdRG.Status.TopoligicalOrder).To(Equal([]string{"deployment", "service"}))
		}, 10*time.Second, time.Second).Should(Succeed())

		// Create instance
		instance := genInstance(namespace, "test-instance", 8080)
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Check if the instance is created
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).Should(Succeed())

		// Verify Deployment creation and specs
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment specs
			g.Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort).To(Equal(int32(8080)))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Patch the deployment to have available replicas in status
		deployment.Status.Replicas = 1
		deployment.Status.ReadyReplicas = 1
		deployment.Status.AvailableReplicas = 1
		deployment.Status.Conditions = []appsv1.DeploymentCondition{
			{
				Type:    appsv1.DeploymentAvailable,
				Status:  corev1.ConditionTrue,
				Reason:  "MinimumReplicasAvailable",
				Message: "Deployment has minimum availability.",
			},
		}
		Expect(env.Client.Status().Update(ctx, deployment)).To(Succeed())

		// Verify Service creation and specs
		service := &corev1.Service{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, service)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify service specs
			g.Expect(service.Spec.Ports).To(HaveLen(1))
			g.Expect(service.Spec.Ports[0].Port).To(Equal(int32(8080)))
			g.Expect(service.Spec.Ports[0].TargetPort.IntVal).To(Equal(int32(8080)))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Verify instance status is updated
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			conditions, found, _ := unstructured.NestedSlice(instance.Object, "status", "deploymentConditions")
			g.Expect(found).To(BeTrue())
			g.Expect(conditions).ToNot(BeEmpty())

			availableReplicas, found, _ := unstructured.NestedInt64(instance.Object, "status", "availableReplicas")
			g.Expect(found).To(BeTrue())
			g.Expect(availableReplicas).To(Equal(int64(1)))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Delete instance
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// Verify Deployment and Service are deleted
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, &appsv1.Deployment{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, &corev1.Service{})
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
		/* Eventually(func() bool {
			ns := &corev1.Namespace{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: namespace}, ns)
			b, _ := json.MarshalIndent(ns, "", "  ")
			fmt.Println(string(b))
			return errors.IsNotFound(err)
		}, 30*time.Second, time.Second).Should(BeTrue()) */
	})
})
