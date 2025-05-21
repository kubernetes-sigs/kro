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
	"context"
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

	"github.com/kro-run/kro/pkg/metadata"
	"github.com/kro-run/kro/pkg/testutil/generator"
)

var _ = Describe("CRD", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	Context("CRD Creation", func() {
		It("should create CRD when ResourceGraphDefinition is created", func() {
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
			Eventually(func(g Gomega) {
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
			}, 10*time.Second, time.Second).Should(Succeed())
		})

		It("should update CRD when ResourceGraphDefinition is updated", func() {
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
			Eventually(func() error {
				return env.Client.Get(ctx, types.NamespacedName{
					Name: "testupdates.kro.run",
				}, crd)
			}, 10*time.Second, time.Second).Should(Succeed())

			// Update ResourceGraphDefinition with new fields
			Eventually(func(g Gomega) {
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
			}, 10*time.Second, time.Second).Should(Succeed())

			// Verify CRD is updated
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: "testupdates.kro.run",
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties
				g.Expect(props["spec"].Properties).To(HaveLen(3))
				g.Expect(props["spec"].Properties["field3"].Type).To(Equal("boolean"))
			}, 10*time.Second, time.Second).Should(Succeed())
		})

		It("should delete CRD when ResourceGraphDefinition is deleted", func() {
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
			Eventually(func() error {
				return env.Client.Get(ctx, types.NamespacedName{Name: crdName},
					&apiextensionsv1.CustomResourceDefinition{})
			}, 10*time.Second, time.Second).Should(Succeed())

			// Delete ResourceGraphDefinition
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

			// Verify CRD is deleted
			Eventually(func() bool {
				err := env.Client.Get(ctx, types.NamespacedName{Name: crdName},
					&apiextensionsv1.CustomResourceDefinition{})
				return errors.IsNotFound(err)
			}, 10*time.Second, time.Second).Should(BeTrue())
		})
	})

	Context("CRD Watch Reconciliation", func() {
		It("should reconcile the ResourceGraphDefinition back when CRD is manually modified", func() {
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

			Eventually(func(g Gomega) {
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
			}, 10*time.Second, time.Second).Should(Succeed())

			// Manually modify the CRD to simulate external modification
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: crdName,
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				// modify the schema (removing field2)
				delete(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties, "field2")

				err = env.Client.Update(ctx, crd)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).Should(Succeed())

			// verify that the ResourceGraphDefinition controller reconciles the CRD
			// back to its original state
			Eventually(func(g Gomega) {
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
			}, 20*time.Second, 2*time.Second).Should(Succeed())
		})
	})
})
