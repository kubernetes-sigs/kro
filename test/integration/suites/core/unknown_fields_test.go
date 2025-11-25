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

package core_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Unknown Fields", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))

		By("creating namespace")
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		By("cleaning up namespace")
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	Context("CRD with preserve unknown fields", func() {
		It("should allow instances with arbitrary unknown nested fields and resolve references", func(ctx SpecContext) {
			By("applying CRD AllowUnknown with x-kubernetes-preserve-unknown-fields")
			crd := &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "allowunknowns.kro.run",
				},
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Group: "kro.run",
					Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
						{
							Name:    "v1alpha1",
							Served:  true,
							Storage: true,
							Subresources: &apiextensionsv1.CustomResourceSubresources{
								Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
							},
							Schema: &apiextensionsv1.CustomResourceValidation{
								OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"spec": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"static": {
													Type: "object",
													Properties: map[string]apiextensionsv1.JSONSchemaProps{
														"key":  {Type: "string"},
														"key2": {Type: "string"},
														"key3": {Type: "string"},
														"key4": {Type: "string"},
														"key5": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
														"key6": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
													},
												},
												"config": {
													Type:                   "object",
													XPreserveUnknownFields: ptr.To(true),
												},
												"anyMap": {
													Type: "object",
													AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
														Schema: &apiextensionsv1.JSONSchemaProps{
															XPreserveUnknownFields: ptr.To(true),
														},
													},
												},
											},
										},
										"status": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"additional": {
													Type: "object",
													AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
														Schema: &apiextensionsv1.JSONSchemaProps{
															XPreserveUnknownFields: ptr.To(true),
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
					Scope: apiextensionsv1.NamespaceScoped,
					Names: apiextensionsv1.CustomResourceDefinitionNames{
						Plural:     "allowunknowns",
						Singular:   "allowunknown",
						Kind:       "AllowUnknown",
						ShortNames: []string{"alu"},
					},
				},
			}

			Expect(env.Client.Create(ctx, crd)).To(Succeed())

			By("waiting for CRD to be ready")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx,
					types.NamespacedName{Name: "allowunknowns.kro.run"},
					&apiextensionsv1.CustomResourceDefinition{},
				)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating an external object reference")
			existingAllowUnknown := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "AllowUnknown",
					"metadata": map[string]any{
						"name":      "existing-allow-unknown",
						"namespace": namespace,
					},
					"spec": map[string]any{
						"config": map[string]any{
							"foo": "bar",
						},
					},
				},
			}
			Eventually(func(g Gomega, ctx SpecContext) {
				g.Expect(env.Client.Create(ctx, existingAllowUnknown)).To(Succeed())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
			Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      existingAllowUnknown.GetName(),
				Namespace: existingAllowUnknown.GetNamespace(),
			}, existingAllowUnknown)).To(Succeed())
			existingAllowUnknown.Object["status"] = map[string]any{
				"additional": map[string]any{
					"key": "value",
				},
			}
			Expect(env.Client.Status().Update(ctx, existingAllowUnknown)).To(Succeed())

			By("creating ResourceGraphDefinition that uses unknown fields")
			rgd := generator.NewResourceGraphDefinition("check-unknown-fields",
				generator.WithSchema(
					"AllowUnknownInstance", "v1alpha1",
					map[string]any{
						"name":   "string",
						"nested": "object",
					},
					nil,
				),
				generator.WithResource("example",
					map[string]any{
						"apiVersion": "kro.run/v1alpha1",
						"kind":       "AllowUnknown",
						"metadata": map[string]any{
							"name": "${schema.spec.name}",
						},
						"spec": map[string]any{
							"config": map[string]any{
								"foo": "bar",
							},
							"static": map[string]any{
								"key":  "${external.status.additional.key}",
								"key2": "${external.status.additional.?key}",
								"key3": "${external.spec.config.foo}",
								"key4": "${external.spec.config.?foo}",
								"key5": "${external.spec.config}",
								"key6": "${external.status.additional}",
							},
							"anyMap": map[string]any{
								"key": "${external.status.additional.?key}",
							},
						},
					},
					nil,
					nil,
				),
				generator.WithResource("configmap",
					map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name": "check-unknown-fields",
							"annotations": map[string]string{
								"optionalAnnotationFromMap":         "${example.spec.anyMap.?key}",
								"optionalAnnotationFromNestedField": "${schema.spec.nested.?field}",
							},
						},
						"data": map[string]any{
							"foo":                 "${example.spec.config.foo}",
							"name":                "${schema.spec.name}",
							"nested":              "${schema.spec.nested.field}",
							"optionalNested":      "${schema.spec.nested.?field}",
							"valueFromMap":        "${example.spec.anyMap.key}",
							"nonExisting":         "${example.spec.anyMap.?abc}",
							"optionalStatusField": "${external.status.additional.?key}",
							"statusField":         "${external.status.additional.key}",
						},
					},
					nil,
					nil,
				),
				generator.WithExternalRef("external",
					&krov1alpha1.ExternalRef{
						APIVersion: "kro.run/v1alpha1",
						Kind:       "AllowUnknown",
						Metadata: krov1alpha1.ExternalRefMetadata{
							Name:      "existing-allow-unknown",
							Namespace: namespace,
						},
					},
					nil,
					nil,
				),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			By("waiting for RGD to become Active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating instance including unknown nested fields")

			instance := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "AllowUnknownInstance",
					"metadata": map[string]any{
						"name":      "check-unknown-fields",
						"namespace": namespace,
					},
					"spec": map[string]any{
						"name": "name",
						"nested": map[string]any{
							"field": "value",
						},
					},
				},
			}

			Expect(env.Client.Create(ctx, instance)).To(Succeed())

			By("waiting for Instance to become Ready")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx,
					types.NamespacedName{Name: "check-unknown-fields", Namespace: namespace},
					instance,
				)
				g.Expect(err).ToNot(HaveOccurred())
				state, found, err := unstructured.NestedString(instance.Object, "status", "state")
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(state).To(Equal("ACTIVE"))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying ConfigMap resolved unknown-field expressions")

			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx,
					types.NamespacedName{Name: "check-unknown-fields", Namespace: namespace},
					cm,
				)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(cm.Data["foo"]).To(Equal("bar"))
				g.Expect(cm.Data["name"]).To(Equal("name"))
				g.Expect(cm.Data["nested"]).To(Equal("value"))
				g.Expect(cm.Data["valueFromMap"]).To(Equal("value"))
				g.Expect(cm.Data["nonExisting"]).To(Equal(""))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})
	})

	Context("Second RGD status referencing first RGD status", func() {
		It("should not panic when the second RGD embeds the first RGD", func(ctx SpecContext) {
			basicRGD := generator.NewResourceGraphDefinition(
				"basic-app-"+rand.String(4),
				generator.WithSchema(
					"BasicApp", "v1alpha1",
					map[string]any{
						"name":     "string",
						"image":    "string | default=\"nginx\"",
						"replicas": "integer | default=3",
					},
					map[string]any{
						"deploymentConditions": "${deployment.status.conditions}",
						"availableReplicas":    "${deployment.status.availableReplicas}",
					},
				),
				generator.WithResource(
					"deployment",
					map[string]any{
						"apiVersion": "apps/v1",
						"kind":       "Deployment",
						"metadata": map[string]any{
							"name": "${schema.spec.name}",
						},
						"spec": map[string]any{
							"replicas": "${schema.spec.replicas}",
							"selector": map[string]any{
								"matchLabels": map[string]any{
									"app": "${schema.spec.name}",
								},
							},
							"template": map[string]any{
								"metadata": map[string]any{
									"labels": map[string]any{
										"app": "${schema.spec.name}",
									},
								},
								"spec": map[string]any{
									"containers": []any{
										map[string]any{
											"name":  "${schema.spec.name}",
											"image": "${schema.spec.image}",
											"ports": []any{
												map[string]any{
													"containerPort": 80,
												},
											},
										},
									},
								},
							},
						},
					},
					nil,
					nil,
				),
			)

			Expect(env.Client.Create(ctx, basicRGD)).To(Succeed())

			Eventually(func(g Gomega, ctx SpecContext) {
				g.Expect(env.Client.Get(ctx,
					types.NamespacedName{Name: basicRGD.Name},
					basicRGD,
				)).To(Succeed())
				g.Expect(basicRGD.Status.State).
					To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			advancedRGD := generator.NewResourceGraphDefinition(
				"advanced-app-"+rand.String(4),
				generator.WithSchema(
					"AdvancedApp", "v1alpha1",
					map[string]any{
						"name":     "string",
						"replicas": "integer | default=3",
						"image":    "string | default=\"nginx\"",
					},
					map[string]any{
						// This line forces the second RGD to load the BasicApp status schema
						"deploymentInfo": "${rgddep.status}",
					},
				),
				generator.WithResource(
					"rgddep",
					map[string]any{
						"apiVersion": "kro.run/v1alpha1",
						"kind":       "BasicApp",
						"metadata": map[string]any{
							"name": "my-advanced-app-${schema.spec.name}",
						},
						"spec": map[string]any{
							"name":     "${schema.spec.name}",
							"replicas": "${schema.spec.replicas}",
							"image":    "${schema.spec.image}",
						},
					},
					nil,
					nil,
				),
			)

			Expect(env.Client.Create(ctx, advancedRGD)).To(Succeed())

			Eventually(func(g Gomega, ctx SpecContext) {
				g.Expect(env.Client.Get(ctx,
					types.NamespacedName{Name: advancedRGD.Name},
					advancedRGD,
				)).To(Succeed())

				// IF THE PANIC STILL EXISTS: this never reaches Active
				g.Expect(advancedRGD.Status.State).
					To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})
	})
})
