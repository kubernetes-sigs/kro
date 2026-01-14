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
	"sort"
	"strings"
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
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("ApplySet", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
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

	Describe("Multi-GVK Apply", func() {
		It("should apply resources with correct KEP applyset labels and annotations", func(ctx SpecContext) {
			By("creating RGD with multiple resource types")
			rgd := generator.NewResourceGraphDefinition("test-applyset-multi-gvk",
				generator.WithSchema(
					"ApplySetMultiGVK", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("configMap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-cm",
					},
					"data": map[string]interface{}{
						"key": "value",
					},
				}, nil, nil),
				generator.WithResource("secret", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-secret",
					},
					"stringData": map[string]interface{}{
						"password": "secret123",
					},
				}, nil, nil),
				generator.WithResource("serviceAccount", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ServiceAccount",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-sa",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			By("waiting for RGD to become active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating instance")
			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "ApplySetMultiGVK",
					"metadata": map[string]interface{}{
						"name":      "test-multi",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name": "multi",
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance)
			})

			By("waiting for instance to become ACTIVE and verifying parent labels/annotations")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				status, _, _ := unstructured.NestedString(instance.Object, "status", "state")
				g.Expect(status).To(Equal("ACTIVE"))

				// Compute expected ApplySet ID
				expectedApplySetID := applyset.ID(instance)

				// Verify KEP ApplySet parent label (exactly)
				g.Expect(instance.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplySetParentIDLabel,
					expectedApplySetID,
				), "instance should have exact applyset parent ID label")

				// Verify KEP ApplySet parent annotations (exactly)
				annotations := instance.GetAnnotations()

				// Tooling annotation must be "kro/<version>"
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetToolingAnnotation,
					applyset.ToolingID(),
				), "instance should have exact tooling annotation")

				// GKs annotation must contain exactly ConfigMap, Secret, ServiceAccount (sorted)
				gksAnnotation := annotations[applyset.ApplySetGKsAnnotation]
				gks := strings.Split(gksAnnotation, ",")
				sort.Strings(gks)
				g.Expect(gks).To(Equal([]string{"ConfigMap", "Secret", "ServiceAccount"}),
					"instance should have exactly these GKs in sorted order")

				// Per KEP-3659, additional-namespaces only lists namespaces OTHER than parent's namespace.
				// Parent namespace is implicitly included, so annotation should be empty when all
				// resources are in the parent namespace.
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetAdditionalNamespacesAnnotation,
					"",
				), "instance should have empty additional-namespaces annotation (parent ns is implicit)")
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying child resources have correct labels with exact values")
			applySetID := applyset.ID(instance)

			// Check ConfigMap with exact node ID
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "multi-cm",
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				verifyChildResourceLabelsWithNodeID(g, cm.GetLabels(), applySetID, instance, rgd, "configMap")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Check Secret with exact node ID
			secret := &corev1.Secret{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "multi-secret",
					Namespace: namespace,
				}, secret)
				g.Expect(err).ToNot(HaveOccurred())
				verifyChildResourceLabelsWithNodeID(g, secret.GetLabels(), applySetID, instance, rgd, "secret")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Check ServiceAccount with exact node ID
			sa := &corev1.ServiceAccount{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "multi-sa",
					Namespace: namespace,
				}, sa)
				g.Expect(err).ToNot(HaveOccurred())
				verifyChildResourceLabelsWithNodeID(g, sa.GetLabels(), applySetID, instance, rgd, "serviceAccount")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})
	})

	Describe("Pruning", func() {
		It("should prune resources when includeWhen becomes false", func(ctx SpecContext) {
			By("creating RGD with conditional resource")
			rgd := generator.NewResourceGraphDefinition("test-applyset-prune",
				generator.WithSchema(
					"ApplySetPrune", "v1alpha1",
					map[string]interface{}{
						"name":          "string",
						"includeSecret": "boolean",
					},
					nil,
				),
				generator.WithResource("configMap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-cm",
					},
					"data": map[string]interface{}{
						"key": "always-present",
					},
				}, nil, nil),
				generator.WithResource("secret", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-secret",
					},
					"stringData": map[string]interface{}{
						"password": "conditional",
					},
				}, nil, []string{"${schema.spec.includeSecret}"}),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			By("waiting for RGD to become active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating instance with includeSecret=true")
			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "ApplySetPrune",
					"metadata": map[string]interface{}{
						"name":      "test-prune",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name":          "prune",
						"includeSecret": true,
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance)
			})

			By("waiting for both resources to be created")
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "prune-cm",
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())

				secret := &corev1.Secret{}
				err = env.Client.Get(ctx, types.NamespacedName{
					Name:      "prune-secret",
					Namespace: namespace,
				}, secret)
				g.Expect(err).ToNot(HaveOccurred())
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying exact parent annotations with both types")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				annotations := instance.GetAnnotations()

				// Tooling annotation must be exact
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetToolingAnnotation,
					applyset.ToolingID(),
				), "instance should have exact tooling annotation")

				// GKs annotation must contain exactly ConfigMap and Secret (sorted)
				gksAnnotation := annotations[applyset.ApplySetGKsAnnotation]
				gks := strings.Split(gksAnnotation, ",")
				sort.Strings(gks)
				g.Expect(gks).To(Equal([]string{"ConfigMap", "Secret"}),
					"instance should have exactly ConfigMap and Secret GKs")

				// Per KEP-3659, additional-namespaces excludes parent namespace (implicit)
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetAdditionalNamespacesAnnotation,
					"",
				), "instance should have empty additional-namespaces annotation")

				// ApplySet parent ID label must be exact
				expectedApplySetID := applyset.ID(instance)
				g.Expect(instance.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplySetParentIDLabel,
					expectedApplySetID,
				), "instance should have exact applyset parent ID label")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("updating instance to set includeSecret=false")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(unstructured.SetNestedField(instance.Object, false, "spec", "includeSecret")).To(Succeed())
				err = env.Client.Update(ctx, instance)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("waiting for secret to be pruned")
			Eventually(func(g Gomega, ctx SpecContext) {
				secret := &corev1.Secret{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "prune-secret",
					Namespace: namespace,
				}, secret)
				g.Expect(errors.IsNotFound(err)).To(BeTrue(), "secret should be pruned")
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying ConfigMap still exists")
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "prune-cm",
				Namespace: namespace,
			}, cm)
			Expect(err).ToNot(HaveOccurred())

			By("verifying exact parent annotations after pruning (Secret removed)")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				annotations := instance.GetAnnotations()

				// GKs annotation should now contain only ConfigMap
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetGKsAnnotation,
					"ConfigMap",
				), "instance should have only ConfigMap in GKs after pruning")

				// Tooling annotation should remain unchanged
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetToolingAnnotation,
					applyset.ToolingID(),
				), "tooling annotation should remain exact")

				// Namespace annotation should remain unchanged (empty per KEP-3659)
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetAdditionalNamespacesAnnotation,
					"",
				), "namespace annotation should remain empty")

				// ApplySet parent ID label should remain unchanged
				expectedApplySetID := applyset.ID(instance)
				g.Expect(instance.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplySetParentIDLabel,
					expectedApplySetID,
				), "applyset parent ID should remain exact")
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})
	})

	Describe("Annotation Tracking", func() {
		It("should grow annotations when resources are added and shrink when removed", func(ctx SpecContext) {
			By("creating RGD with two conditional resources")
			rgd := generator.NewResourceGraphDefinition("test-annotation-tracking",
				generator.WithSchema(
					"AnnotationTracking", "v1alpha1",
					map[string]interface{}{
						"name":          "string",
						"includeSecret": "boolean",
						"includeSA":     "boolean",
					},
					nil,
				),
				// ConfigMap is always present
				generator.WithResource("configMap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-cm",
					},
					"data": map[string]interface{}{
						"key": "always-present",
					},
				}, nil, nil),
				// Secret is conditional
				generator.WithResource("secret", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-secret",
					},
					"stringData": map[string]interface{}{
						"password": "conditional",
					},
				}, nil, []string{"${schema.spec.includeSecret}"}),
				// ServiceAccount is conditional
				generator.WithResource("serviceAccount", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ServiceAccount",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-sa",
					},
				}, nil, []string{"${schema.spec.includeSA}"}),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			By("waiting for RGD to become active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("PHASE 1: creating instance with only ConfigMap (no Secret, no SA)")
			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "AnnotationTracking",
					"metadata": map[string]interface{}{
						"name":      "test-tracking",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name":          "track",
						"includeSecret": false,
						"includeSA":     false,
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance)
			})

			By("verifying GKs annotation contains only ConfigMap")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				status, _, _ := unstructured.NestedString(instance.Object, "status", "state")
				g.Expect(status).To(Equal("ACTIVE"))

				annotations := instance.GetAnnotations()
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetGKsAnnotation,
					"ConfigMap",
				), "Phase 1: should have only ConfigMap in GKs")
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("PHASE 2: adding Secret (annotations should GROW)")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(unstructured.SetNestedField(instance.Object, true, "spec", "includeSecret")).To(Succeed())
				err = env.Client.Update(ctx, instance)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("waiting for Secret to be created")
			Eventually(func(g Gomega, ctx SpecContext) {
				secret := &corev1.Secret{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "track-secret",
					Namespace: namespace,
				}, secret)
				g.Expect(err).ToNot(HaveOccurred())
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying GKs annotation grew to include Secret")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				annotations := instance.GetAnnotations()
				gksAnnotation := annotations[applyset.ApplySetGKsAnnotation]
				gks := strings.Split(gksAnnotation, ",")
				sort.Strings(gks)
				g.Expect(gks).To(Equal([]string{"ConfigMap", "Secret"}),
					"Phase 2: GKs should have grown to include Secret")
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("PHASE 3: adding ServiceAccount (annotations should GROW more)")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(unstructured.SetNestedField(instance.Object, true, "spec", "includeSA")).To(Succeed())
				err = env.Client.Update(ctx, instance)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("waiting for ServiceAccount to be created")
			Eventually(func(g Gomega, ctx SpecContext) {
				sa := &corev1.ServiceAccount{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "track-sa",
					Namespace: namespace,
				}, sa)
				g.Expect(err).ToNot(HaveOccurred())
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying GKs annotation grew to include all three types")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				annotations := instance.GetAnnotations()
				gksAnnotation := annotations[applyset.ApplySetGKsAnnotation]
				gks := strings.Split(gksAnnotation, ",")
				sort.Strings(gks)
				g.Expect(gks).To(Equal([]string{"ConfigMap", "Secret", "ServiceAccount"}),
					"Phase 3: GKs should have all three types")
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("PHASE 4: removing Secret (annotations should SHRINK)")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(unstructured.SetNestedField(instance.Object, false, "spec", "includeSecret")).To(Succeed())
				err = env.Client.Update(ctx, instance)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("waiting for Secret to be pruned")
			Eventually(func(g Gomega, ctx SpecContext) {
				secret := &corev1.Secret{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "track-secret",
					Namespace: namespace,
				}, secret)
				g.Expect(errors.IsNotFound(err)).To(BeTrue(), "Secret should be pruned")
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying GKs annotation shrunk after successful prune")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				annotations := instance.GetAnnotations()
				gksAnnotation := annotations[applyset.ApplySetGKsAnnotation]
				gks := strings.Split(gksAnnotation, ",")
				sort.Strings(gks)
				g.Expect(gks).To(Equal([]string{"ConfigMap", "ServiceAccount"}),
					"Phase 4: GKs should have shrunk (Secret removed)")
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("PHASE 5: removing ServiceAccount (annotations should SHRINK to just ConfigMap)")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(unstructured.SetNestedField(instance.Object, false, "spec", "includeSA")).To(Succeed())
				err = env.Client.Update(ctx, instance)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("waiting for ServiceAccount to be pruned")
			Eventually(func(g Gomega, ctx SpecContext) {
				sa := &corev1.ServiceAccount{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "track-sa",
					Namespace: namespace,
				}, sa)
				g.Expect(errors.IsNotFound(err)).To(BeTrue(), "ServiceAccount should be pruned")
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying GKs annotation shrunk back to just ConfigMap")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				annotations := instance.GetAnnotations()
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetGKsAnnotation,
					"ConfigMap",
				), "Phase 5: GKs should be back to just ConfigMap")
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying ConfigMap still exists throughout all phases")
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "track-cm",
				Namespace: namespace,
			}, cm)
			Expect(err).ToNot(HaveOccurred(), "ConfigMap should exist through all phases")
		})
	})

	Describe("Collection Labels", func() {
		It("should apply correct collection-specific labels to forEach resources", func(ctx SpecContext) {
			By("creating RGD with forEach collection")
			rgd := generator.NewResourceGraphDefinition("test-applyset-collection",
				generator.WithSchema(
					"ApplySetCollection", "v1alpha1",
					map[string]interface{}{
						"name":   "string",
						"values": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("configmaps", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${element}",
					},
					"data": map[string]interface{}{
						"value": "${element}",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"element": "${schema.spec.values}"},
					},
					nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			By("waiting for RGD to become active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating instance with 3 values")
			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "ApplySetCollection",
					"metadata": map[string]interface{}{
						"name":      "test-collection",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name":   "coll",
						"values": []interface{}{"alpha", "beta", "gamma"},
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance)
			})

			By("waiting for instance to become ACTIVE and verifying parent annotations")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				status, _, _ := unstructured.NestedString(instance.Object, "status", "state")
				g.Expect(status).To(Equal("ACTIVE"))

				// Verify exact parent annotations
				annotations := instance.GetAnnotations()

				// Tooling annotation must be exact
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetToolingAnnotation,
					applyset.ToolingID(),
				), "instance should have exact tooling annotation")

				// GKs annotation should only contain ConfigMap for this collection
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetGKsAnnotation,
					"ConfigMap",
				), "instance should have exact GKs annotation")

				// Per KEP-3659, additional-namespaces excludes parent namespace (implicit)
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetAdditionalNamespacesAnnotation,
					"",
				), "instance should have empty additional-namespaces annotation")

				// ApplySet parent ID label must be exact
				expectedApplySetID := applyset.ID(instance)
				g.Expect(instance.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplySetParentIDLabel,
					expectedApplySetID,
				), "instance should have exact applyset parent ID label")
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			applySetID := applyset.ID(instance)

			By("verifying collection items have correct labels with exact values")
			values := []string{"alpha", "beta", "gamma"}
			for i, value := range values {
				cm := &corev1.ConfigMap{}
				Eventually(func(g Gomega, ctx SpecContext) {
					err := env.Client.Get(ctx, types.NamespacedName{
						Name:      fmt.Sprintf("coll-%s", value),
						Namespace: namespace,
					}, cm)
					g.Expect(err).ToNot(HaveOccurred())

					labels := cm.GetLabels()

					// Verify all standard labels with exact values via helper
					verifyChildResourceLabelsWithNodeID(g, labels, applySetID, instance, rgd, "configmaps")

					// Verify collection-specific labels with exact values
					g.Expect(labels).To(HaveKeyWithValue(
						metadata.CollectionIndexLabel,
						fmt.Sprintf("%d", i),
					), "collection item should have exact collection-index label")

					g.Expect(labels).To(HaveKeyWithValue(
						metadata.CollectionSizeLabel,
						"3",
					), "collection item should have exact collection-size label")

				}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
			}
		})

		It("should prune collection items when collection shrinks", func(ctx SpecContext) {
			By("creating RGD with forEach collection")
			rgd := generator.NewResourceGraphDefinition("test-applyset-coll-prune",
				generator.WithSchema(
					"ApplySetCollPrune", "v1alpha1",
					map[string]interface{}{
						"name":   "string",
						"values": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("secrets", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${element}",
					},
					"stringData": map[string]interface{}{
						"value": "${element}",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"element": "${schema.spec.values}"},
					},
					nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			By("waiting for RGD to become active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating instance with 3 values")
			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "ApplySetCollPrune",
					"metadata": map[string]interface{}{
						"name":      "test-coll-prune",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name":   "shrink",
						"values": []interface{}{"one", "two", "three"},
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance)
			})

			By("waiting for all 3 secrets to be created")
			for _, value := range []string{"one", "two", "three"} {
				Eventually(func(g Gomega, ctx SpecContext) {
					secret := &corev1.Secret{}
					err := env.Client.Get(ctx, types.NamespacedName{
						Name:      fmt.Sprintf("shrink-%s", value),
						Namespace: namespace,
					}, secret)
					g.Expect(err).ToNot(HaveOccurred())
				}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
			}

			By("updating instance to only have 1 value")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(unstructured.SetNestedStringSlice(instance.Object, []string{"one"}, "spec", "values")).To(Succeed())
				err = env.Client.Update(ctx, instance)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("waiting for 'two' and 'three' secrets to be pruned")
			for _, value := range []string{"two", "three"} {
				Eventually(func(g Gomega, ctx SpecContext) {
					secret := &corev1.Secret{}
					err := env.Client.Get(ctx, types.NamespacedName{
						Name:      fmt.Sprintf("shrink-%s", value),
						Namespace: namespace,
					}, secret)
					g.Expect(errors.IsNotFound(err)).To(BeTrue(), "secret %s should be pruned", value)
				}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
			}

			By("verifying 'one' secret still exists with updated collection labels (exact)")
			applySetID := applyset.ID(instance)
			Eventually(func(g Gomega, ctx SpecContext) {
				secret := &corev1.Secret{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "shrink-one",
					Namespace: namespace,
				}, secret)
				g.Expect(err).ToNot(HaveOccurred())

				labels := secret.GetLabels()

				// Verify all standard labels with exact values via helper
				verifyChildResourceLabelsWithNodeID(g, labels, applySetID, instance, rgd, "secrets")

				// Verify collection-specific labels updated to reflect new collection size
				g.Expect(labels).To(HaveKeyWithValue(metadata.CollectionIndexLabel, "0"),
					"remaining item should have exact collection-index 0")
				g.Expect(labels).To(HaveKeyWithValue(metadata.CollectionSizeLabel, "1"),
					"remaining item should have exact collection-size 1 after pruning")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})
	})

	Describe("ApplySet Isolation", func() {
		It("should not prune resources from different RGDs", func(ctx SpecContext) {
			By("creating first RGD")
			rgd1 := generator.NewResourceGraphDefinition("test-isolation-rgd1",
				generator.WithSchema(
					"IsolationRGD1", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("configMap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-rgd1-cm",
					},
					"data": map[string]interface{}{
						"source": "rgd1",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd1)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd1)).To(Succeed())
			})

			By("creating second RGD with same schema structure but different kind")
			rgd2 := generator.NewResourceGraphDefinition("test-isolation-rgd2",
				generator.WithSchema(
					"IsolationRGD2", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("configMap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-rgd2-cm",
					},
					"data": map[string]interface{}{
						"source": "rgd2",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd2)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd2)).To(Succeed())
			})

			By("waiting for both RGDs to become active")
			for _, rgd := range []*krov1alpha1.ResourceGraphDefinition{rgd1, rgd2} {
				Eventually(func(g Gomega, ctx SpecContext) {
					err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
			}

			By("creating instance from RGD1")
			instance1 := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "IsolationRGD1",
					"metadata": map[string]interface{}{
						"name":      "test-iso1",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name": "shared",
					},
				},
			}
			Expect(env.Client.Create(ctx, instance1)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance1)
			})

			By("creating instance from RGD2 with same instance name as RGD1")
			// Using the same instance name ("test-iso1") to verify that different Kinds
			// produce different ApplySet IDs even with identical names (GKNN includes Kind)
			instance2 := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "IsolationRGD2",
					"metadata": map[string]interface{}{
						"name":      "test-iso1",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name": "shared",
					},
				},
			}
			Expect(env.Client.Create(ctx, instance2)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance2)
			})

			By("waiting for both instances to become ACTIVE")
			for _, inst := range []*unstructured.Unstructured{instance1, instance2} {
				Eventually(func(g Gomega, ctx SpecContext) {
					err := env.Client.Get(ctx, types.NamespacedName{
						Name:      inst.GetName(),
						Namespace: namespace,
					}, inst)
					g.Expect(err).ToNot(HaveOccurred())
					status, _, _ := unstructured.NestedString(inst.Object, "status", "state")
					g.Expect(status).To(Equal("ACTIVE"))
				}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
			}

			By("verifying both ConfigMaps exist with different ApplySet IDs")
			applySetID1 := applyset.ID(instance1)
			applySetID2 := applyset.ID(instance2)

			// Verify they have different ApplySet IDs
			Expect(applySetID1).ToNot(Equal(applySetID2),
				"different instances should have different ApplySet IDs")

			// Check RGD1's ConfigMap
			cm1 := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "shared-rgd1-cm",
					Namespace: namespace,
				}, cm1)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm1.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplysetPartOfLabel,
					applySetID1,
				), "RGD1's ConfigMap should belong to instance1's ApplySet")
				g.Expect(cm1.GetLabels()).To(HaveKeyWithValue(
					metadata.ResourceGraphDefinitionNameLabel,
					rgd1.GetName(),
				), "RGD1's ConfigMap should reference rgd1")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Check RGD2's ConfigMap
			cm2 := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "shared-rgd2-cm",
					Namespace: namespace,
				}, cm2)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm2.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplysetPartOfLabel,
					applySetID2,
				), "RGD2's ConfigMap should belong to instance2's ApplySet")
				g.Expect(cm2.GetLabels()).To(HaveKeyWithValue(
					metadata.ResourceGraphDefinitionNameLabel,
					rgd2.GetName(),
				), "RGD2's ConfigMap should reference rgd2")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("triggering reconcile by updating instance1")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance1.GetName(),
					Namespace: namespace,
				}, instance1)
				g.Expect(err).ToNot(HaveOccurred())

				annotations := instance1.GetAnnotations()
				if annotations == nil {
					annotations = make(map[string]string)
				}
				annotations["test-trigger"] = "reconcile"
				instance1.SetAnnotations(annotations)
				err = env.Client.Update(ctx, instance1)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying RGD2's ConfigMap was NOT pruned by RGD1's reconcile")
			Consistently(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "shared-rgd2-cm",
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred(), "RGD2's ConfigMap should not be pruned by RGD1")
				g.Expect(cm.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplysetPartOfLabel,
					applySetID2,
				), "ConfigMap should still belong to instance2's ApplySet")
			}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})

		It("should not prune resources from different instances of the same RGD", func(ctx SpecContext) {
			By("creating RGD that creates ConfigMap named after instance")
			rgd := generator.NewResourceGraphDefinition("test-isolation-instances",
				generator.WithSchema(
					"IsolationInstances", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("configMap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-cm",
					},
					"data": map[string]interface{}{
						"instance": "${schema.spec.name}",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			By("waiting for RGD to become active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating first instance")
			instance1 := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "IsolationInstances",
					"metadata": map[string]interface{}{
						"name":      "instance-a",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name": "instance-a",
					},
				},
			}
			Expect(env.Client.Create(ctx, instance1)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance1)
			})

			By("creating second instance")
			instance2 := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "IsolationInstances",
					"metadata": map[string]interface{}{
						"name":      "instance-b",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name": "instance-b",
					},
				},
			}
			Expect(env.Client.Create(ctx, instance2)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance2)
			})

			By("waiting for both instances to become ACTIVE")
			for _, inst := range []*unstructured.Unstructured{instance1, instance2} {
				Eventually(func(g Gomega, ctx SpecContext) {
					err := env.Client.Get(ctx, types.NamespacedName{
						Name:      inst.GetName(),
						Namespace: namespace,
					}, inst)
					g.Expect(err).ToNot(HaveOccurred())
					status, _, _ := unstructured.NestedString(inst.Object, "status", "state")
					g.Expect(status).To(Equal("ACTIVE"))
				}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
			}

			By("verifying both ConfigMaps exist with different ApplySet IDs")
			applySetID1 := applyset.ID(instance1)
			applySetID2 := applyset.ID(instance2)

			// Verify they have different ApplySet IDs (different instance names)
			Expect(applySetID1).ToNot(Equal(applySetID2),
				"different instances of same RGD should have different ApplySet IDs")

			// Check instance1's ConfigMap
			cm1 := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "instance-a-cm",
					Namespace: namespace,
				}, cm1)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm1.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplysetPartOfLabel,
					applySetID1,
				), "instance-a's ConfigMap should belong to instance1's ApplySet")
				g.Expect(cm1.GetLabels()).To(HaveKeyWithValue(
					metadata.InstanceLabel,
					"instance-a",
				), "ConfigMap should reference instance-a")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Check instance2's ConfigMap
			cm2 := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "instance-b-cm",
					Namespace: namespace,
				}, cm2)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm2.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplysetPartOfLabel,
					applySetID2,
				), "instance-b's ConfigMap should belong to instance2's ApplySet")
				g.Expect(cm2.GetLabels()).To(HaveKeyWithValue(
					metadata.InstanceLabel,
					"instance-b",
				), "ConfigMap should reference instance-b")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("triggering reconcile by updating instance1")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance1.GetName(),
					Namespace: namespace,
				}, instance1)
				g.Expect(err).ToNot(HaveOccurred())

				annotations := instance1.GetAnnotations()
				if annotations == nil {
					annotations = make(map[string]string)
				}
				annotations["test-trigger"] = "reconcile"
				instance1.SetAnnotations(annotations)
				err = env.Client.Update(ctx, instance1)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying instance2's ConfigMap was NOT pruned by instance1's reconcile")
			Consistently(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "instance-b-cm",
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred(), "instance-b's ConfigMap should not be pruned by instance-a")
				g.Expect(cm.GetLabels()).To(HaveKeyWithValue(
					applyset.ApplysetPartOfLabel,
					applySetID2,
				), "ConfigMap should still belong to instance2's ApplySet")
			}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying both instances still have correct parent annotations")
			for _, inst := range []*unstructured.Unstructured{instance1, instance2} {
				Eventually(func(g Gomega, ctx SpecContext) {
					err := env.Client.Get(ctx, types.NamespacedName{
						Name:      inst.GetName(),
						Namespace: namespace,
					}, inst)
					g.Expect(err).ToNot(HaveOccurred())

					annotations := inst.GetAnnotations()
					expectedApplySetID := applyset.ID(inst)

					// Verify exact parent annotations
					g.Expect(annotations).To(HaveKeyWithValue(
						applyset.ApplySetToolingAnnotation,
						applyset.ToolingID(),
					), "instance should have exact tooling annotation")
					g.Expect(annotations).To(HaveKeyWithValue(
						applyset.ApplySetGKsAnnotation,
						"ConfigMap",
					), "instance should have exact GKs annotation")
					g.Expect(inst.GetLabels()).To(HaveKeyWithValue(
						applyset.ApplySetParentIDLabel,
						expectedApplySetID,
					), "instance should have exact applyset parent ID")
				}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
			}
		})
	})

	Describe("Label Preservation", func() {
		It("should preserve user-defined labels and annotations on instance after reconciliation", func(ctx SpecContext) {
			By("creating RGD")
			rgd := generator.NewResourceGraphDefinition("test-label-preserve",
				generator.WithSchema(
					"LabelPreserve", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("configMap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-cm",
					},
					"data": map[string]interface{}{
						"key": "value",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			By("waiting for RGD to become active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating instance with custom labels and annotations")
			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "LabelPreserve",
					"metadata": map[string]interface{}{
						"name":      "test-preserve",
						"namespace": namespace,
						"labels": map[string]interface{}{
							"custom-label":           "custom-value",
							"app.kubernetes.io/team": "platform",
						},
						"annotations": map[string]interface{}{
							"custom-annotation": "custom-annotation-value",
							"description":       "This is a test instance",
						},
					},
					"spec": map[string]interface{}{
						"name": "preserve",
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance)
			})

			By("waiting for instance to become ACTIVE")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())
				status, _, _ := unstructured.NestedString(instance.Object, "status", "state")
				g.Expect(status).To(Equal("ACTIVE"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying custom labels are preserved alongside ApplySet labels")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				labels := instance.GetLabels()
				annotations := instance.GetAnnotations()

				// Custom labels must be preserved
				g.Expect(labels).To(HaveKeyWithValue("custom-label", "custom-value"),
					"custom-label should be preserved")
				g.Expect(labels).To(HaveKeyWithValue("app.kubernetes.io/team", "platform"),
					"app.kubernetes.io/team label should be preserved")

				// ApplySet labels must also be present
				expectedApplySetID := applyset.ID(instance)
				g.Expect(labels).To(HaveKeyWithValue(
					applyset.ApplySetParentIDLabel,
					expectedApplySetID,
				), "ApplySet parent ID label should be present")

				// Custom annotations must be preserved
				g.Expect(annotations).To(HaveKeyWithValue("custom-annotation", "custom-annotation-value"),
					"custom-annotation should be preserved")
				g.Expect(annotations).To(HaveKeyWithValue("description", "This is a test instance"),
					"description annotation should be preserved")

				// ApplySet annotations must also be present
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetToolingAnnotation,
					applyset.ToolingID(),
				), "ApplySet tooling annotation should be present")
				g.Expect(annotations).To(HaveKeyWithValue(
					applyset.ApplySetGKsAnnotation,
					"ConfigMap",
				), "ApplySet GKs annotation should be present")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("triggering another reconcile and verifying labels are still preserved")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				// Add a trigger annotation to force reconcile
				annotations := instance.GetAnnotations()
				annotations["reconcile-trigger"] = "1"
				instance.SetAnnotations(annotations)
				err = env.Client.Update(ctx, instance)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying labels and annotations are still preserved after second reconcile")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				labels := instance.GetLabels()
				annotations := instance.GetAnnotations()

				// Custom labels still preserved
				g.Expect(labels).To(HaveKeyWithValue("custom-label", "custom-value"),
					"custom-label should still be preserved after second reconcile")
				g.Expect(labels).To(HaveKeyWithValue("app.kubernetes.io/team", "platform"),
					"app.kubernetes.io/team should still be preserved after second reconcile")

				// Custom annotations still preserved
				g.Expect(annotations).To(HaveKeyWithValue("custom-annotation", "custom-annotation-value"),
					"custom-annotation should still be preserved after second reconcile")
				g.Expect(annotations).To(HaveKeyWithValue("description", "This is a test instance"),
					"description should still be preserved after second reconcile")

				// ApplySet labels/annotations still present
				g.Expect(labels).To(HaveKey(applyset.ApplySetParentIDLabel),
					"ApplySet parent ID should still be present")
				g.Expect(annotations).To(HaveKey(applyset.ApplySetToolingAnnotation),
					"ApplySet tooling should still be present")
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})
	})
})

// verifyChildResourceLabels checks that a child resource has all expected labels with exact values
func verifyChildResourceLabels(
	g Gomega,
	labels map[string]string,
	applySetID string,
	instance *unstructured.Unstructured,
	rgd *krov1alpha1.ResourceGraphDefinition,
) {
	// KEP ApplySet membership label (exact)
	g.Expect(labels).To(HaveKeyWithValue(
		applyset.ApplysetPartOfLabel,
		applySetID,
	), "child should have exact applyset part-of label")

	// KRO ownership labels (exact)
	g.Expect(labels).To(HaveKeyWithValue(metadata.OwnedLabel, "true"),
		"child should have exact owned label")
	g.Expect(labels).To(HaveKeyWithValue(metadata.KROVersionLabel, "devel"),
		"child should have exact kro-version label")
	g.Expect(labels).To(HaveKeyWithValue(metadata.ManagedByLabelKey, metadata.ManagedByKROValue),
		"child should have exact managed-by label")

	// Instance tracking labels (exact)
	g.Expect(labels).To(HaveKeyWithValue(metadata.InstanceIDLabel, string(instance.GetUID())),
		"child should have exact instance-id label")
	g.Expect(labels).To(HaveKeyWithValue(metadata.InstanceLabel, instance.GetName()),
		"child should have exact instance-name label")
	g.Expect(labels).To(HaveKeyWithValue(metadata.InstanceNamespaceLabel, instance.GetNamespace()),
		"child should have exact instance-namespace label")

	// RGD tracking labels (exact)
	g.Expect(labels).To(HaveKeyWithValue(metadata.ResourceGraphDefinitionIDLabel, string(rgd.GetUID())),
		"child should have exact rgd-id label")
	g.Expect(labels).To(HaveKeyWithValue(metadata.ResourceGraphDefinitionNameLabel, rgd.GetName()),
		"child should have exact rgd-name label")

	// Node ID label existence (value varies by resource)
	g.Expect(labels).To(HaveKey(metadata.NodeIDLabel),
		"child should have node-id label")
}

// verifyChildResourceLabelsWithNodeID checks labels including exact node ID
func verifyChildResourceLabelsWithNodeID(
	g Gomega,
	labels map[string]string,
	applySetID string,
	instance *unstructured.Unstructured,
	rgd *krov1alpha1.ResourceGraphDefinition,
	nodeID string,
) {
	verifyChildResourceLabels(g, labels, applySetID, instance, rgd)

	// Verify exact node ID
	g.Expect(labels).To(HaveKeyWithValue(metadata.NodeIDLabel, nodeID),
		"child should have exact node-id label")
}
