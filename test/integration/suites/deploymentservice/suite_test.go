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

package deploymentservice_test

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/release-utils/version"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

var env *environment.Environment

func TestDeploymentservice(t *testing.T) {
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

	RunSpecs(t, "DeploymentService Suite")
}

var _ = Describe("DeploymentService", func() {
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
		rgd, genInstance := deploymentService("test-deployment-service")
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("DeploymentService"))
			g.Expect(createdRGD.Spec.Schema.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(2))

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
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(2))
			g.Expect(createdRGD.Status.TopologicalOrder).To(Equal([]string{"deployment", "service"}))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance
		instance := genInstance(namespace, "test-instance", 8080)
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Check if the instance is created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify Deployment creation and specs
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment specs
			g.Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort).To(Equal(int32(8080)))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

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
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, service)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify service specs
			g.Expect(service.Spec.Ports).To(HaveLen(1))
			g.Expect(service.Spec.Ports[0].Port).To(Equal(int32(8080)))
			g.Expect(service.Spec.Ports[0].TargetPort.IntVal).To(Equal(int32(8080)))
			g.Expect(service.ObjectMeta.Labels).To(HaveKeyWithValue(metadata.OwnedLabel, "true"))
			g.Expect(service.ObjectMeta.Labels).
				To(HaveKeyWithValue(metadata.KROVersionLabel, version.GetVersionInfo().GitVersion))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify instance status is updated
		Eventually(func(g Gomega, ctx SpecContext) {
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

			// now lets check for the optional field that should be available
			status, found, _ := unstructured.NestedString(instance.Object, "status", "available")
			g.Expect(found).To(BeTrue())
			g.Expect(status).ToNot(BeEmpty())
			g.Expect(status).To(Equal(string(metav1.ConditionTrue)))

			// and also check for the unavailable field that should never be available
			condition, found, _ := unstructured.NestedFieldNoCopy(instance.Object, "status", "unavailable")
			g.Expect(found).To(BeFalse())
			g.Expect(condition).To(BeNil())

			// Validate Ready condition
			statusConditions, found, _ := unstructured.NestedSlice(instance.Object, "status", "conditions")
			g.Expect(found).To(BeTrue())
			g.Expect(statusConditions).ToNot(BeEmpty())

			// Find the Ready condition
			var readyCondition map[string]interface{}
			for _, condInterface := range statusConditions {
				if cond, ok := condInterface.(map[string]interface{}); ok {
					condType, _ := cond["type"].(string)
					if condType == ctrlinstance.Ready {
						readyCondition = cond
						break
					}
				}
			}

			// Validate Ready condition
			g.Expect(readyCondition).ToNot(BeNil(), "Ready condition should be present")
			g.Expect(readyCondition["status"]).To(Equal("True"), "Ready condition should be True")
			g.Expect(readyCondition["observedGeneration"]).To(Equal(instance.GetGeneration()),
				"Ready observedGeneration should match instance generation")
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Delete instance
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// Verify Deployment and Service are deleted
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, &appsv1.Deployment{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "deployment should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, &corev1.Service{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "service should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify instance is deleted
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Delete ResourceGraphDefinition
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is deleted
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

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
