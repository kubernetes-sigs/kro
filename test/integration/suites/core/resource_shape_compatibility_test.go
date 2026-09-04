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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// Resource shapes a definition is allowed to reference or produce.
//
//   - API versions are opaque strings. Kubernetes only requires a CRD version
//     to be a valid DNS label, and widely used operators pick names outside the
//     vN[alpha|beta]N convention — Azure Service Operator versions every type
//     as vNapiYYYYMMDD, and Google's Config Connector ships versions like
//     v1p1beta1. kro must template and reference those types like any other.
//   - A definition may create a CustomResourceDefinition. Shipping an operator
//     or a set of CRDs as a kro API is an established pattern, and a CRD's
//     openAPIV3Schema is a deeply nested, self-referential document, which
//     makes it the most demanding template payload kro handles.
//   - When a resource's namespace cannot be resolved, the error has to name the
//     field the author must fix. A cluster-scoped instance gives its namespaced
//     children no namespace to inherit, so an expression that yields an empty
//     string is an authoring mistake, and the diagnostic is the only way to
//     find it.
var _ = Describe("ResourceShapeCompatibility", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	DescribeTable("templates a resource whose apiVersion is outside the vN convention",
		func(ctx SpecContext, version, schemaKind string) {
			group := fmt.Sprintf("shape%s.example.com", rand.String(5))
			installTestCRD(ctx, group, version, "Widget", "widgets", apiextensionsv1.NamespaceScoped)

			rgd := generator.NewResourceGraphDefinition("test-shape-"+rand.String(5),
				generator.WithSchema(
					schemaKind, "v1alpha1",
					map[string]any{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("widget", map[string]any{
					"apiVersion": fmt.Sprintf("%s/%s", group, version),
					"kind":       "Widget",
					"metadata": map[string]any{
						"name": "${schema.spec.name}-widget",
					},
					"spec": map[string]any{
						"size": "large",
					},
				}, nil, nil),
			)
			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})
			waitForRGDActive(ctx, rgd.Name)

			name := "shape-version"
			instance := newInstance(schemaKind, name, namespace, map[string]any{
				"name": name,
			})
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")

			widget := &unstructured.Unstructured{}
			widget.SetGroupVersionKind(schemaGVK(group, version, "Widget"))
			Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-widget",
				Namespace: namespace,
			}, widget)).To(Succeed())
		},
		// Azure Service Operator's convention.
		Entry("date-stamped version", "v1api20200601", "TestShapeDateVersion"),
		// Google Config Connector's convention.
		Entry("patch-qualified version", "v1p1beta1", "TestShapePatchVersion"),
	)

	It("resolves an external reference whose apiVersion is outside the vN convention", func(ctx SpecContext) {
		group := fmt.Sprintf("shaperef%s.example.com", rand.String(5))
		const version = "v1api20200601"
		installTestCRD(ctx, group, version, "Widget", "widgets", apiextensionsv1.NamespaceScoped)

		existing := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": fmt.Sprintf("%s/%s", group, version),
			"kind":       "Widget",
			"metadata": map[string]any{
				"name":      "existing-widget",
				"namespace": namespace,
			},
			"spec": map[string]any{
				"size": "small",
			},
		}}
		Expect(env.Client.Create(ctx, existing)).To(Succeed())

		rgd := generator.NewResourceGraphDefinition("test-shape-ref-"+rand.String(5),
			generator.WithSchema(
				"TestShapeRefVersion", "v1alpha1",
				map[string]any{
					"name": "string",
				},
				nil,
			),
			generator.WithExternalRef("widget", &krov1alpha1.ExternalRef{
				APIVersion: fmt.Sprintf("%s/%s", group, version),
				Kind:       "Widget",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Name:      "existing-widget",
					Namespace: namespace,
				},
			}, nil, nil),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-cm",
				},
				"data": map[string]any{
					"size": "${widget.spec.size}",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "shape-ref"
		instance := newInstance("TestShapeRefVersion", name, namespace, map[string]any{
			"name": name,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")

		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-cm",
				Namespace: namespace,
			}, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKeyWithValue("size", "small"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("creates a CustomResourceDefinition declared by a definition", func(ctx SpecContext) {
		group := fmt.Sprintf("crdtpl%s.example.com", rand.String(5))
		crdName := "gadgets." + group

		rgd := generator.NewResourceGraphDefinition("test-crd-template-"+rand.String(5),
			generator.WithSchema(
				"TestCRDTemplate", "v1alpha1",
				map[string]any{
					"name": "string",
				},
				nil,
			),
			// A representative CRD payload: a versioned, structural schema with
			// descriptions and nested properties, as generated by controller-gen.
			generator.WithResource("gadgetCRD", map[string]any{
				"apiVersion": "apiextensions.k8s.io/v1",
				"kind":       "CustomResourceDefinition",
				"metadata": map[string]any{
					"name": crdName,
				},
				"spec": map[string]any{
					"group": group,
					"scope": "Namespaced",
					"names": map[string]any{
						"kind":     "Gadget",
						"listKind": "GadgetList",
						"plural":   "gadgets",
						"singular": "gadget",
					},
					"versions": []any{
						map[string]any{
							"name":    "v1alpha1",
							"served":  true,
							"storage": true,
							"schema": map[string]any{
								"openAPIV3Schema": map[string]any{
									"description": "Gadget is a test resource.",
									"type":        "object",
									"properties": map[string]any{
										"apiVersion": map[string]any{
											"description": "APIVersion defines the versioned schema of this representation.",
											"type":        "string",
										},
										"kind": map[string]any{
											"description": "Kind is a string value representing the REST resource.",
											"type":        "string",
										},
										"metadata": map[string]any{
											"type": "object",
										},
										"spec": map[string]any{
											"description": "GadgetSpec defines the desired state.",
											"type":        "object",
											"properties": map[string]any{
												"size": map[string]any{
													"description": "Size of the gadget.",
													"type":        "string",
												},
											},
										},
										"status": map[string]any{
											"description": "GadgetStatus defines the observed state.",
											"type":        "object",
											"properties": map[string]any{
												"ready": map[string]any{
													"type": "boolean",
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "crd-template"
		instance := newInstance("TestCRDTemplate", name, namespace, map[string]any{
			"name": name,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: crdName},
			})
		})

		waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")

		Eventually(func(g Gomega, ctx SpecContext) {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd)).To(Succeed())
			g.Expect(crd.Spec.Names.Kind).To(Equal("Gadget"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("names the unresolved field when a cluster-scoped instance's child namespace is empty", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-unresolved-namespace",
			generator.WithSchema(
				"TestUnresolvedNamespace", "v1alpha1",
				map[string]any{
					"name":            "string",
					"targetNamespace": "string",
				},
				nil,
				generator.WithScope(krov1alpha1.ResourceScopeCluster),
			),
			// A cluster-scoped instance gives namespaced children nothing to
			// inherit, so this expression yielding "" is an authoring mistake.
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "${schema.spec.name}-cm",
					"namespace": "${schema.spec.targetNamespace}",
				},
				"data": map[string]any{
					"managed": "yes",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "unresolved-ns-" + rand.String(4)
		instance := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestUnresolvedNamespace",
				"metadata": map[string]any{
					"name": name,
				},
				"spec": map[string]any{
					"name":            name,
					"targetNamespace": "",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, instance)
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: name}, instance)).To(Succeed())
			conds := instanceConditions(instance)
			g.Expect(conds).To(ContainSubstring("metadata.namespace"),
				"the failure should name the field the author has to fix; conditions: %s", conds)
		}, 60*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())
	})
})

// installTestCRD creates a minimal CRD for use as a template or reference
// target and waits for it to be Established, removing it when the spec ends.
func installTestCRD(
	ctx SpecContext,
	group, version, kind, plural string,
	scope apiextensionsv1.ResourceScope,
) {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Scope: scope,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     kind,
				ListKind: kind + "List",
				Plural:   plural,
				Singular: strings.ToLower(kind),
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    version,
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"size": {Type: "string"},
								},
							},
							"status": {
								Type:                   "object",
								XPreserveUnknownFields: ptrTrue(),
							},
						},
					},
				},
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
			}},
		},
	}
	Expect(env.Client.Create(ctx, crd)).To(Succeed())
	DeferCleanup(func(ctx SpecContext) {
		_ = env.Client.Delete(ctx, &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: crd.Name},
		})
	})

	Eventually(func(g Gomega, ctx SpecContext) {
		current := &apiextensionsv1.CustomResourceDefinition{}
		g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: crd.Name}, current)).To(Succeed())
		established := false
		for _, cond := range current.Status.Conditions {
			if cond.Type == apiextensionsv1.Established &&
				cond.Status == apiextensionsv1.ConditionTrue {
				established = true
			}
		}
		g.Expect(established).To(BeTrue(), "CRD %s is not Established", crd.Name)
	}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

func ptrTrue() *bool {
	v := true
	return &v
}

// schemaGVK builds a GroupVersionKind for the test CRDs above.
func schemaGVK(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}
