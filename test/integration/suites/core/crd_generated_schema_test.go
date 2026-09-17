// Copyright 2026 The Kubernetes Authors.
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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

// These specs exercise the graph-native way to define a Kind: a Graph (or an
// RGD) templates a CustomResourceDefinition whose openAPIV3Schema is produced
// by CEL with simpleschema.toOpenAPI() from a SimpleSchema block (spec,
// types, status) — held in a def node for the Graph, carried by the instance
// for the RGD. Beyond the CRD being created, the real API server must accept
// the generated schema (structural validation on admission) and enforce it on
// instances of the new Kind: defaults are applied, required fields and
// validation markers are enforced.
var _ = Describe("CRD with CEL-generated schema", func() {
	// simpleSchemaFields is the SimpleSchema block the CRD schema is built from.
	simpleSchemaFields := map[string]any{
		"name":     "string | required=true",
		"replicas": "integer | default=1 minimum=0",
		"tags":     "[]string",
	}

	// expectGeneratedSchema asserts the CRD the controller applied carries the
	// schema simpleschema.toOpenAPI derived from simpleSchemaFields.
	expectGeneratedSchema := func(g Gomega, crd *apiextensionsv1.CustomResourceDefinition) {
		g.Expect(crd.Spec.Versions).To(HaveLen(1))
		g.Expect(crd.Spec.Versions[0].Schema).ToNot(BeNil())
		root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
		g.Expect(root).ToNot(BeNil())
		g.Expect(root.Type).To(Equal("object"))
		g.Expect(root.Properties).To(HaveKey("spec"))
		spec := root.Properties["spec"]
		g.Expect(spec.Type).To(Equal("object"))
		g.Expect(spec.Required).To(Equal([]string{"name"}))
		g.Expect(spec.Properties).To(HaveKey("name"))
		g.Expect(spec.Properties["name"].Type).To(Equal("string"))
		g.Expect(spec.Properties).To(HaveKey("replicas"))
		g.Expect(spec.Properties["replicas"].Type).To(Equal("integer"))
		g.Expect(spec.Properties["replicas"].Default).ToNot(BeNil())
		g.Expect(string(spec.Properties["replicas"].Default.Raw)).To(Equal("1"))
		g.Expect(spec.Properties["replicas"].Minimum).To(HaveValue(Equal(float64(0))))
		g.Expect(spec.Properties).To(HaveKey("tags"))
		g.Expect(spec.Properties["tags"].Type).To(Equal("array"))
	}

	// expectSchemaEnforced creates instances of the generated Kind and checks
	// the API server applies the generated schema: defaults, required fields
	// and the minimum marker.
	expectSchemaEnforced := func(ctx SpecContext, gvk schema.GroupVersionKind, namespace string) {
		newInstance := func(name string, spec map[string]any) *unstructured.Unstructured {
			u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
			u.SetGroupVersionKind(gvk)
			u.SetName(name)
			u.SetNamespace(namespace)
			return u
		}

		By("creating an instance of the generated Kind and observing server-side defaulting")
		valid := newInstance("valid", map[string]any{"name": "demo"})
		Expect(env.Client.Create(ctx, valid)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			// Runs before the CRD-owning Graph/instance is torn down (LIFO), so
			// the CRD has no custom resources left for the apiserver's
			// customresourcecleanup finalizer to reap.
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, valid))).To(Succeed())
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, client.ObjectKeyFromObject(valid), valid.DeepCopy())
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}, 20*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
		})
		replicas, found, err := unstructured.NestedInt64(valid.Object, "spec", "replicas")
		Expect(err).ToNot(HaveOccurred())
		Expect(found).To(BeTrue(), "spec.replicas must be defaulted by the API server from the generated schema")
		Expect(replicas).To(Equal(int64(1)))

		By("rejecting an instance that violates the generated schema")
		err = env.Client.Create(ctx, newInstance("missing-required", map[string]any{"replicas": int64(2)}))
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.name"))

		err = env.Client.Create(ctx, newInstance("below-minimum", map[string]any{"name": "demo", "replicas": int64(-1)}))
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.replicas"))
	}

	It("Graph: templates a CRD whose openAPIV3Schema comes from simpleschema.toOpenAPI", func(ctx SpecContext) {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		suffix := rand.String(5)
		group := fmt.Sprintf("gen-%s.kro.run", suffix)
		kind := "Widget" + strings.ToUpper(suffix[:1]) + suffix[1:]
		plural := strings.ToLower(kind) + "s"
		crdName := plural + "." + group

		// A Graph deletion prunes the CRD it owns; the fallback keeps the shared
		// envtest API server clean if that ever fails.
		DeferCleanup(func(ctx SpecContext) {
			crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: crdName}}
			_ = client.IgnoreNotFound(env.Client.Delete(ctx, crd))
		})

		g := &krov1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "kind-from-schema", Namespace: ns},
			Spec: krov1alpha1.GraphSpec{
				Nodes: []krov1alpha1.Node{
					{
						// The Kind's definition in SimpleSchema, the way an RGD author
						// would write spec.schema: spec and status field maps plus the
						// custom types they share. The remaining keys describe the CRD
						// and are ignored by simpleschema.toOpenAPI(kindSpec).
						ID: "kindSpec",
						Def: environment.RawExt(t, map[string]any{
							"group":   group,
							"kind":    kind,
							"plural":  plural,
							"version": "v1alpha1",
							"spec":    simpleSchemaFields,
							"types": map[string]any{
								"Condition": map[string]any{
									"type":    "string | required=true",
									"status":  "string | required=true",
									"reason":  "string",
									"message": "string",
								},
							},
							"status": map[string]any{
								"items":      "integer",
								"conditions": "[]Condition",
							},
						}),
					},
					{
						ID: "crd",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "apiextensions.k8s.io/v1",
							"kind":       "CustomResourceDefinition",
							"metadata":   map[string]any{"name": "${kindSpec.plural + '.' + kindSpec.group}"},
							"spec": map[string]any{
								"group": "${kindSpec.group}",
								"names": map[string]any{
									"kind":   "${kindSpec.kind}",
									"plural": "${kindSpec.plural}",
								},
								"scope": "Namespaced",
								"versions": []any{map[string]any{
									"name":    "${kindSpec.version}",
									"served":  true,
									"storage": true,
									"subresources": map[string]any{
										"status": map[string]any{},
									},
									"schema": map[string]any{
										// Root, spec and status are all derived from the block.
										"openAPIV3Schema": "${simpleschema.toOpenAPI(kindSpec)}",
									},
								}},
							},
						}),
						ReadyWhen: []string{
							`${crd.?status.?conditions.orValue([]).exists(c, c.type == 'Established' && c.status == 'True')}`,
						},
					},
				},
			},
		}
		env.CreateGraph(t, g)

		By("waiting for the Graph to be accepted and ready")
		key := types.NamespacedName{Namespace: ns, Name: g.Name}
		env.AwaitCondition(t, key, krov1alpha1.GraphConditionTypeAccepted, metav1.ConditionTrue, 30*time.Second)
		env.AwaitCondition(t, key, krov1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 60*time.Second)

		By("checking the applied CRD carries the generated schema")
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd)).To(Succeed())
			g.Expect(crd.Spec.Group).To(Equal(group))
			g.Expect(crd.Spec.Names.Kind).To(Equal(kind))
			g.Expect(crd.Spec.Names.Plural).To(Equal(plural))
			g.Expect(crd.Spec.Versions[0].Name).To(Equal("v1alpha1"))
			expectGeneratedSchema(g, crd)

			// The status block is converted with the same custom types as spec.
			root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			g.Expect(root.Properties).To(HaveKey("status"))
			status := root.Properties["status"]
			g.Expect(status.Type).To(Equal("object"))
			g.Expect(status.Properties).To(HaveKey("items"))
			g.Expect(status.Properties["items"].Type).To(Equal("integer"))
			g.Expect(status.Properties).To(HaveKey("conditions"))
			g.Expect(status.Properties["conditions"].Type).To(Equal("array"))
			g.Expect(status.Properties["conditions"].Items).ToNot(BeNil())
			g.Expect(status.Properties["conditions"].Items.Schema.Required).To(Equal([]string{"status", "type"}))
		}, 20*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		expectSchemaEnforced(ctx, schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: kind}, ns)
	})

	It("RGD: an instance's SimpleSchema fields become the openAPIV3Schema of a templated CRD", func(ctx SpecContext) {
		namespace := env.CreateNamespace(GinkgoT())

		suffix := rand.String(5)
		factoryKind := "KindFactory" + strings.ToUpper(suffix[:1]) + suffix[1:]
		group := fmt.Sprintf("gen-%s.kro.run", suffix)
		kind := "Gadget" + strings.ToUpper(suffix[:1]) + suffix[1:]
		plural := strings.ToLower(kind) + "s"
		crdName := plural + "." + group

		DeferCleanup(func(ctx SpecContext) {
			crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: crdName}}
			_ = client.IgnoreNotFound(env.Client.Delete(ctx, crd))
		})

		By("creating an RGD whose CRD resource has CEL outside metadata")
		// The instance carries the Kind's SimpleSchema block as an opaque
		// object; the CRD template converts it at reconcile time.
		rgd := generator.NewResourceGraphDefinition("kind-factory-"+suffix,
			generator.WithSchema(
				factoryKind, "v1alpha1",
				map[string]any{
					"group":      "string | required=true",
					"kind":       "string | required=true",
					"plural":     "string | required=true",
					"version":    "string | default=v1alpha1",
					"definition": "object",
				},
				nil,
			),
			generator.WithResource("crd", map[string]any{
				"apiVersion": "apiextensions.k8s.io/v1",
				"kind":       "CustomResourceDefinition",
				"metadata":   map[string]any{"name": "${schema.spec.plural + '.' + schema.spec.group}"},
				"spec": map[string]any{
					"group": "${schema.spec.group}",
					"names": map[string]any{
						"kind":   "${schema.spec.kind}",
						"plural": "${schema.spec.plural}",
					},
					"scope": "Namespaced",
					"versions": []any{map[string]any{
						"name":    "${schema.spec.version}",
						"served":  true,
						"storage": true,
						"schema": map[string]any{
							"openAPIV3Schema": "${simpleschema.toOpenAPI(schema.spec.definition)}",
						},
					}},
				},
			}, []string{
				`${crd.?status.?conditions.orValue([]).exists(c, c.type == 'Established' && c.status == 'True')}`,
			}, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, rgd))).To(Succeed())
		})

		By("waiting for the RGD to become active")
		Eventually(func(g Gomega, ctx SpecContext) {
			got := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, got)).To(Succeed())
			g.Expect(got.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		By("creating a factory instance that describes the new Kind in SimpleSchema")
		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       factoryKind,
			"metadata":   map[string]any{"name": "gadget-kind", "namespace": namespace},
			"spec": map[string]any{
				"group":      group,
				"kind":       kind,
				"plural":     plural,
				"version":    "v1alpha1",
				"definition": map[string]any{"spec": simpleSchemaFields},
			},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, instance))).To(Succeed())
			// kro finalizes the instance only once the CRD it manages is gone.
			// Under parallel CRD churn the apiserver's customresourcecleanup
			// finalizer can lag for tens of seconds (see unstickTerminatingCRD);
			// the generated Kind has no instances left at this point, so clear it
			// while waiting, then let kro drop the instance finalizer before the
			// RGD is removed.
			Eventually(func(g Gomega, ctx SpecContext) {
				crd := &apiextensionsv1.CustomResourceDefinition{}
				if err := env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd); err == nil &&
					crd.DeletionTimestamp != nil && len(crd.Finalizers) == 1 &&
					crd.Finalizers[0] == apiextensionsv1.CustomResourceCleanupFinalizer {
					crd.Finalizers = nil
					_ = env.Client.Update(ctx, crd) // best effort; the apiserver may win the race
				}
				err := env.Client.Get(ctx, types.NamespacedName{Name: instance.GetName(), Namespace: namespace}, instance.DeepCopy())
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "factory instance still present")
			}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
		})

		By("checking the CRD the instance produced carries the generated schema")
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd)).To(Succeed())
			g.Expect(crd.Spec.Group).To(Equal(group))
			g.Expect(crd.Spec.Names.Kind).To(Equal(kind))
			g.Expect(crd.Spec.Versions[0].Name).To(Equal("v1alpha1"))
			expectGeneratedSchema(g, crd)
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		By("waiting for the instance to report the established CRD as ready")
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: instance.GetName(), Namespace: namespace}, instance)).To(Succeed())
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		expectSchemaEnforced(ctx, schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: kind}, namespace)
	})
})
