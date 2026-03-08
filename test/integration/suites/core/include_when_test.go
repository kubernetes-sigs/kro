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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Conditions", func() {
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

	It("should not create deployment, service, and configmap "+
		"due to condition deploymentEnabled == false", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-conditions",
			generator.WithSchema(
				"TestConditions", "v1alpha1",
				map[string]interface{}{
					"name": "string",

					"deploymentA": map[string]interface{}{
						"name":    "string",
						"enabled": "boolean",
					},
					"deploymentBenabled":     "boolean",
					"serviceAccountAenabled": "boolean",
					"serviceAccountBenabled": "boolean",
					"serviceBenabled":        "boolean",
				},
				nil,
			),
			// Deployment - no dependencies
			generator.WithResource("deploymentA", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.deploymentA.name}",
				},
				"spec": map[string]interface{}{
					"replicas": 1,
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
			}, nil, []string{"${schema.spec.deploymentA.enabled}"}),
			// Depends on serviceAccountA
			generator.WithResource("deploymentB", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-b",
				},
				"spec": map[string]interface{}{
					"replicas": 1,
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
							"serviceAccountName": "${serviceAccountA.metadata.name + schema.spec.name}",
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
			}, nil, []string{"${schema.spec.deploymentBenabled}"}),
			// serviceAccountA - no dependencies
			generator.WithResource("serviceAccountA", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-a",
				},
			}, nil, []string{"${schema.spec.serviceAccountAenabled}"}),
			// ServiceAccount - depends on service
			generator.WithResource("serviceAccountB", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata": map[string]interface{}{
					"name": "${serviceA.metadata.name}",
				},
			}, nil, []string{"${schema.spec.serviceAccountBenabled}"}),
			// ServiceA - depends on DeploymentA
			generator.WithResource("serviceA", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name": "${deploymentA.metadata.name}",
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
			// ServiceB - depends on deploymentA and deploymentB
			generator.WithResource("serviceB", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name": "${deploymentB.metadata.name + deploymentA.metadata.name}",
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
			}, nil, []string{"${schema.spec.serviceBenabled}"}),
		)

		// Create ResourceGraphDefinition
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("TestConditions"))
			g.Expect(createdRGD.Spec.Schema.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(6))

			g.Expect(createdRGD.Status.TopologicalOrder).To(Equal([]string{
				"deploymentA",
				"serviceAccountA",
				"deploymentB",
				"serviceA",
				"serviceAccountB",
				"serviceB",
			}))

			// Verify the ResourceGraphDefinition status
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(6))
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

		name := "test-conditions"
		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestConditions",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
					"deploymentA": map[string]interface{}{
						"enabled": false,
					},
					"deploymentBenabled":     true,
					"serviceAccountAenabled": true,
					"serviceBenabled":        true,
					"serviceAccountBenabled": true,
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
			val, b, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(b).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify DeploymentA is not created
		Eventually(func(g Gomega, ctx SpecContext) bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-a",
				Namespace: namespace,
			}, &appsv1.Deployment{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).WithContext(ctx).Should(BeTrue())

		// Verify serviceAccountA is created
		serviceAccountA := &corev1.ServiceAccount{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-a", name),
				Namespace: namespace,
			}, serviceAccountA)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify DeploymentB is created
		deploymentB := &appsv1.Deployment{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-b",
				Namespace: namespace,
			}, deploymentB)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment specs
			g.Expect(deploymentB.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(deploymentB.Spec.Template.Spec.ServiceAccountName).To(Equal(name + "-a" + name))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify ServiceA is not created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-a",
				Namespace: namespace,
			}, &corev1.Service{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "serviceA should not be created"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify ServiceB is not created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-b",
				Namespace: namespace,
			}, &corev1.Service{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "serviceB should not be created"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify ServiceAccountB is not created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, &corev1.ServiceAccount{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "serviceAccountB should not be created"))
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

	It("should skip downstream dependents when an upstream includeWhen is false", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-conditions-contagious",
			generator.WithSchema(
				"ContagiousConditions", "v1alpha1",
				map[string]interface{}{
					"name":         "string",
					"enableParent": "boolean",
				},
				nil,
			),
			generator.WithResource("parent", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-parent",
				},
				"data": map[string]interface{}{
					"key": "parent",
				},
			}, nil, []string{"${schema.spec.enableParent}"}),
			generator.WithResource("child", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${parent.metadata.name}-child",
				},
				"data": map[string]interface{}{
					"fromParent": "${parent.data.key}",
				},
			}, nil, nil),
			generator.WithResource("always", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-always",
				},
				"data": map[string]interface{}{
					"key": "always",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-conditions-contagious"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "ContagiousConditions",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":         name,
					"enableParent": false,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, instance)
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		always := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-always",
				Namespace: namespace,
			}, always)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(always.Data).To(HaveKeyWithValue("key", "always"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-parent",
				Namespace: namespace,
			}, &corev1.ConfigMap{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue(), "parent should be skipped by includeWhen")
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-parent-child",
				Namespace: namespace,
			}, &corev1.ConfigMap{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue(), "child should be skipped contagiously because parent is skipped")
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should propagate exclusion when includeWhen depends on another resource", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-conditions-resource-backed-contagious",
			generator.WithSchema(
				"ResourceBackedContagiousConditions", "v1alpha1",
				map[string]interface{}{
					"name":         "string",
					"enableMiddle": "boolean",
				},
				nil,
			),
			generator.WithResource("source", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-source",
				},
				"data": map[string]interface{}{
					"enabled": "${schema.spec.enableMiddle ? 'true' : 'false'}",
				},
			}, nil, nil),
			generator.WithResource("middle", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-middle",
				},
				"data": map[string]interface{}{
					"value": "middle",
				},
			}, nil, []string{"${source.data.enabled == 'true'}"}),
			generator.WithResource("child", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${middle.metadata.name}-child",
				},
				"data": map[string]interface{}{
					"fromMiddle": "${middle.data.value}",
				},
			}, nil, nil),
			generator.WithResource("always", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-always",
				},
				"data": map[string]interface{}{
					"key": "always",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-resource-backed-contagious"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "ResourceBackedContagiousConditions",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":         name,
					"enableMiddle": false,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, instance)
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		source := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-source",
				Namespace: namespace,
			}, source)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(source.Data).To(HaveKeyWithValue("enabled", "false"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		always := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-always",
				Namespace: namespace,
			}, always)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-middle",
				Namespace: namespace,
			}, &corev1.ConfigMap{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue(), "middle should be excluded by resource-backed includeWhen")
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-middle-child",
				Namespace: namespace,
			}, &corev1.ConfigMap{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue(), "child should be excluded contagiously because middle is skipped")
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

})
