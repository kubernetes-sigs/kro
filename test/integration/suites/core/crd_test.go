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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("CRD", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
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

	Context("CRD Creation", func() {
		It("should create CRD when ResourceGraphDefinition is created", func(ctx SpecContext) {
			// Create a simple ResourceGraphDefinition
			rgd := generator.NewResourceGraphDefinition("test-crd",
				generator.WithSchema(
					"TestResource", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
						"field2": "integer | default=42",
					},
					nil,
				),
				generator.WithResource("res1", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.field1}",
					},
					"data": map[string]interface{}{
						"key":  "value",
						"key2": "${schema.spec.field2}",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			// Verify CRD is created
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: "testresources.kro.run",
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				// Verify CRD spec
				g.Expect(crd.Spec.Group).To(Equal("kro.run"))
				g.Expect(crd.Spec.Names.Kind).To(Equal("TestResource"))
				g.Expect(crd.Spec.Names.Plural).To(Equal("testresources"))

				// Verify schema
				props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties

				// Check spec schema
				g.Expect(props["spec"].Properties["field1"].Type).To(Equal("string"))
				g.Expect(props["spec"].Properties["field2"].Type).To(Equal("integer"))
				g.Expect(props["spec"].Properties["field2"].Default.Raw).To(Equal([]byte("42")))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should update CRD when ResourceGraphDefinition is updated", func(ctx SpecContext) {
			// Create initial ResourceGraphDefinition
			rgd := generator.NewResourceGraphDefinition("test-crd-update",
				generator.WithSchema(
					"TestUpdate", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
						"field2": "integer | default=42",
					},
					nil,
				),
			)
			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			// Wait for initial CRD
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Eventually(func(g Gomega, ctx SpecContext) {
				g.Expect(env.Client.Get(ctx, types.NamespacedName{
					Name: "testupdates.kro.run",
				}, crd)).To(Succeed())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Update ResourceGraphDefinition with new fields
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())

				rgd.Spec.Schema.Spec = toRawExtension(map[string]interface{}{
					"field1": "string",
					"field2": "integer | default=42",
					"field3": "boolean",
				})

				err = env.Client.Update(ctx, rgd)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Verify CRD is updated
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: "testupdates.kro.run",
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties
				g.Expect(props["spec"].Properties).To(HaveLen(3))
				g.Expect(props["spec"].Properties["field3"].Type).To(Equal("boolean"))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should delete CRD when ResourceGraphDefinition is deleted", func(ctx SpecContext) {
			// Create ResourceGraphDefinition
			rgd := generator.NewResourceGraphDefinition("test-crd-delete",
				generator.WithSchema(
					"TestDelete", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
					},
					nil,
				),
			)
			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			// Wait for CRD creation
			crdName := "testdeletes.kro.run"
			Eventually(func(g Gomega, ctx SpecContext) {
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: crdName},
					&apiextensionsv1.CustomResourceDefinition{})).To(Succeed())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Delete ResourceGraphDefinition
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

			// Verify CRD is deleted
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: crdName},
					&apiextensionsv1.CustomResourceDefinition{})
				g.Expect(err).To(MatchError(errors.IsNotFound, "crd should be deleted"))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})
	})

	Context("CRD Ownership Conflicts", func() {
		It("should prevent multiple ResourceGraphDefinitions from managing the same CRD", func(ctx SpecContext) {
			// Create first RGD
			rgd1 := generator.NewResourceGraphDefinition("test-crd-owner-1",
				generator.WithSchema(
					"ConflictTest", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
					},
					nil,
				),
			)
			Expect(env.Client.Create(ctx, rgd1)).To(Succeed())

			// Wait for CRD to be created by RGD1
			crdName := "conflicttests.kro.run"
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(metadata.IsKROOwned(crd.ObjectMeta)).To(BeTrue())
				g.Expect(crd.Labels[metadata.ResourceGraphDefinitionNameLabel]).To(Equal("test-crd-owner-1"))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Create second RGD trying to manage the same CRD
			rgd2 := generator.NewResourceGraphDefinition("test-crd-owner-2",
				generator.WithSchema(
					"ConflictTest", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
						"field2": "integer",
					},
					nil,
				),
			)
			Expect(env.Client.Create(ctx, rgd2)).To(Succeed())

			// Verify RGD2 fails to reconcile due to ownership conflict
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: "test-crd-owner-2"}, rgd2)
				g.Expect(err).ToNot(HaveOccurred())
				// RGD2 should remain in Inactive state due to CRD ownership conflict
				g.Expect(rgd2.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Cleanup
			Expect(env.Client.Delete(ctx, rgd1)).To(Succeed())
			Expect(env.Client.Delete(ctx, rgd2)).To(Succeed())
		})
	})

	Context("CRD Watch Reconciliation", func() {
		It("should reconcile the ResourceGraphDefinition back when CRD is manually modified", func(ctx SpecContext) {
			rgdName := "test-crd-watch"
			rgd := generator.NewResourceGraphDefinition(rgdName,
				generator.WithSchema(
					"TestWatch", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
						"field2": "integer | default=42",
					},
					nil,
				),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			// wait for CRD to be created and verify its initial state
			crdName := "testwatches.kro.run"
			crd := &apiextensionsv1.CustomResourceDefinition{}
			var originalCRDVersion string

			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: crdName,
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(metadata.IsKROOwned(crd.ObjectMeta)).To(BeTrue())
				g.Expect(crd.Labels[metadata.ResourceGraphDefinitionNameLabel]).To(Equal(rgdName))

				// store the original schema for later comparison
				originalSchema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
				g.Expect(originalSchema["field1"].Type).To(Equal("string"))
				g.Expect(originalSchema["field2"].Type).To(Equal("integer"))
				g.Expect(originalSchema["field2"].Default.Raw).To(Equal([]byte("42")))

				// store the original resource version
				originalCRDVersion = crd.ResourceVersion
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Manually modify the CRD to simulate external modification
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: crdName,
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				// modify the schema (removing field2)
				delete(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties, "field2")

				err = env.Client.Update(ctx, crd)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// verify that the ResourceGraphDefinition controller reconciles the CRD
			// back to its original state
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: crdName,
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				// expect resource version to be different (indicating an update)
				g.Expect(crd.ResourceVersion).NotTo(Equal(originalCRDVersion))

				schemaProps := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
				g.Expect(schemaProps["field1"].Type).To(Equal("string"))
				g.Expect(schemaProps["field2"].Type).To(Equal("integer")) // Should be restored
				g.Expect(schemaProps["field2"].Default.Raw).To(Equal([]byte("42")))
			}, 20*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
	})
})
