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

	"github.com/awslabs/symphony/internal/testutil/generator"
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
		It("should create CRD when ResourceGroup is created", func() {
			// Create a simple ResourceGroup
			rg := generator.NewResourceGroup("test-crd",
				generator.WithNamespace(namespace),
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
						"name": "${spec.field1}",
					},
					"data": map[string]interface{}{
						"key":  "value",
						"key2": "${spec.field2}",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rg)).To(Succeed())

			// Verify CRD is created
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: "testresources.x.symphony.k8s.aws",
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				// Verify CRD spec
				g.Expect(crd.Spec.Group).To(Equal("x.symphony.k8s.aws"))
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

		It("should update CRD when ResourceGroup is updated", func() {
			// Create initial ResourceGroup
			rg := generator.NewResourceGroup("test-crd-update",
				generator.WithNamespace(namespace),
				generator.WithSchema(
					"TestUpdate", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
						"field2": "integer | default=42",
					},
					nil,
				),
			)
			Expect(env.Client.Create(ctx, rg)).To(Succeed())

			// Wait for initial CRD
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Eventually(func() error {
				return env.Client.Get(ctx, types.NamespacedName{
					Name: "testupdates.x.symphony.k8s.aws",
				}, crd)
			}, 10*time.Second, time.Second).Should(Succeed())

			// Update ResourceGroup with new fields
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      rg.Name,
					Namespace: namespace,
				}, rg)
				g.Expect(err).ToNot(HaveOccurred())

				rg.Spec.Schema.Spec = toRawExtension(map[string]interface{}{
					"field1": "string",
					"field2": "integer | default=42",
					"field3": "boolean",
				})

				err = env.Client.Update(ctx, rg)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).Should(Succeed())

			// Verify CRD is updated
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: "testupdates.x.symphony.k8s.aws",
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties
				g.Expect(props["spec"].Properties).To(HaveLen(3))
				g.Expect(props["spec"].Properties["field3"].Type).To(Equal("boolean"))
			}, 10*time.Second, time.Second).Should(Succeed())
		})

		It("should delete CRD when ResourceGroup is deleted", func() {
			// Create ResourceGroup
			rg := generator.NewResourceGroup("test-crd-delete",
				generator.WithNamespace(namespace),
				generator.WithSchema(
					"TestDelete", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
					},
					nil,
				),
			)
			Expect(env.Client.Create(ctx, rg)).To(Succeed())

			// Wait for CRD creation
			crdName := "testdeletes.x.symphony.k8s.aws"
			Eventually(func() error {
				return env.Client.Get(ctx, types.NamespacedName{Name: crdName},
					&apiextensionsv1.CustomResourceDefinition{})
			}, 10*time.Second, time.Second).Should(Succeed())

			// Delete ResourceGroup
			Expect(env.Client.Delete(ctx, rg)).To(Succeed())

			// Verify CRD is deleted
			Eventually(func() bool {
				err := env.Client.Get(ctx, types.NamespacedName{Name: crdName},
					&apiextensionsv1.CustomResourceDefinition{})
				return errors.IsNotFound(err)
			}, 10*time.Second, time.Second).Should(BeTrue())
		})
	})
})
