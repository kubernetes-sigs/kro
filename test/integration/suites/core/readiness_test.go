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
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Readiness", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It(`should wait for deployment to have deployment.spec.replicas 
		== deployment.status.availableReplicas before creating service`, func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-readiness",
			generator.WithSchema(
				"TestReadiness", "v1alpha1",
				map[string]interface{}{
					"name":     "string",
					"replicas": "integer",
					"deployment": map[string]interface{}{
						"includeAnnotations": "boolean | default=false",
						"annotations":        "map[string]string",
					},
					"service": map[string]interface{}{
						"includeAnnotations": "boolean | default=true",
						"annotations":        "map[string]string",
					},
				},
				nil,
			),
			// Deployment - no dependencies
			generator.WithResource("deployment", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name":        "${schema.spec.name}",
					"annotations": `${schema.spec.deployment.includeAnnotations ? schema.spec.deployment.annotations : {}}`,
				},
				"spec": map[string]interface{}{
					"replicas": "${schema.spec.replicas}",
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "deployment",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "deployment",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "${schema.spec.name}-deployment",
									"image": "nginx",
									"ports": []interface{}{
										map[string]interface{}{
											"containerPort": 8080,
										},
									},
								},
							},
						},
					},
				},
			}, []string{"${deployment.spec.replicas == deployment.status.availableReplicas}"}, nil),
			// ServiceB - depends on deploymentA and deploymentB
			generator.WithResource("service", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name":        "${deployment.metadata.name}",
					"annotations": `${schema.spec.service.includeAnnotations ? schema.spec.service.annotations : {}}`,
				},
				"spec": map[string]interface{}{
					"selector": map[string]interface{}{
						"app": "deployment",
					},
					"ports": []interface{}{
						map[string]interface{}{
							"port":       8080,
							"targetPort": 8080,
						},
					},
				},
			}, nil, nil),
		)

		// Create ResourceGraphDefinition
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}

		//nolint:dupl
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("TestReadiness"))
			g.Expect(createdRGD.Spec.Schema.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(2))

			g.Expect(createdRGD.Status.TopologicalOrder).To(Equal([]string{
				"deployment",
				"service",
			}))

			// Verify the ResourceGraphDefinition status
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(2))
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

		name := "test-readiness"
		replicas := 5
		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestReadiness",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":     name,
					"replicas": replicas,
					"deployment": map[string]interface{}{
						"includeAnnotations": false,
						"annotations":        map[string]interface{}{},
					},
					"service": map[string]interface{}{
						"includeAnnotations": true,
						"annotations": map[string]interface{}{
							"app": "service",
						},
					},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Check if instance is created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify DeploymentB is created
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment specs
			g.Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(replicas)))
			g.Expect(deployment.Annotations).To(HaveLen(0))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify Service is not created yet
		err := env.Client.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		}, &corev1.Service{})
		Expect(err).To(MatchError(errors.IsNotFound, "service should not be created yet"))

		// Expect instance state to be IN_PROGRESS
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("IN_PROGRESS"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Patch the deployment to have available replicas in status
		deployment.Status.Replicas = int32(replicas)
		deployment.Status.ReadyReplicas = int32(replicas)
		deployment.Status.AvailableReplicas = int32(replicas)
		deployment.Status.Conditions = []appsv1.DeploymentCondition{
			{
				Type:    appsv1.DeploymentAvailable,
				Status:  corev1.ConditionTrue,
				Reason:  "MinimumReplicasAvailable",
				Message: "Deployment has minimum availability.",
			},
		}
		Expect(env.Client.Status().Update(ctx, deployment)).To(Succeed())

		service := &corev1.Service{}
		// Verify Service is created now
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, service)
			g.Expect(err).ToNot(HaveOccurred())

			// validate service spec
			g.Expect(service.Annotations).To(HaveLen(1))
			g.Expect(service.Annotations["app"]).To(Equal("service"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Delete instance
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// Verify instance is deleted
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
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
	})

	It(`should should wait for the last node in the DAG to be ready`, func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-readiness-tail",
			generator.WithSchema(
				"TestReadinessTail", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			// Job resource
			generator.WithResource("job", map[string]interface{}{
				"apiVersion": "batch/v1",
				"kind":       "Job",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
				},
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "sleep",
									"image": "busybox",
									"args":  []interface{}{"sleep", "3"},
								},
							},
							"restartPolicy": "Never",
						},
					},
				},
			}, []string{"${job.status.completionTime != null}"}, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		instanceName := "test-readiness-tail"

		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestReadinessTail",
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

		// Check if instance is created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Expect instance state to be IN_PROGRESS
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify Job is created now
		job := &batchv1.Job{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, job)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify instance state is IN_PROGRESS consistently for 30seconds
		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("IN_PROGRESS"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Patch job completionTime to simulate job completion
		now := metav1.Now()
		job.Status.CompletionTime = &now
		Expect(env.Client.Status().Update(ctx, job)).To(Succeed())

		// Verify instance is now marked as ACTIVE
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("ACTIVE"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
	})
})
