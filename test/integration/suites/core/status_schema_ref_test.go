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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Status schema references", func() {
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

	It("should populate status from schema spec field only", func(ctx SpecContext) {
		// Status expression references schema.spec directly — no resource needed in the expression.
		rgd := generator.NewResourceGraphDefinition("test-status-schema-only",
			generator.WithSchema(
				"StatusSchemaOnly", "v1alpha1",
				map[string]interface{}{
					"host": "string",
				},
				map[string]interface{}{
					"echoHost": "${schema.spec.host}",
				},
			),
			generator.WithResource("cm", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.host}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		instanceName := "test-schema-only"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "StatusSchemaOnly",
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"host": "example.com",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			echoHost, found, err := unstructured.NestedString(instance.Object, "status", "echoHost")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.echoHost should exist")
			g.Expect(echoHost).To(Equal("example.com"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should populate status mixing resource field and schema spec field", func(ctx SpecContext) {
		// Status expression combines a resource field with a schema spec field.
		rgd := generator.NewResourceGraphDefinition("test-status-schema-mixed",
			generator.WithSchema(
				"StatusSchemaMixed", "v1alpha1",
				map[string]interface{}{
					"path": "string",
				},
				map[string]interface{}{
					"url": "${configmap.metadata.name + '/' + schema.spec.path}",
				},
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "my-configmap",
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

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		instanceName := "test-schema-mixed"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "StatusSchemaMixed",
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"path": "api/v1",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			url, found, err := unstructured.NestedString(instance.Object, "status", "url")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.url should exist")
			g.Expect(url).To(Equal("my-configmap/api/v1"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should reflect updated schema spec value in status", func(ctx SpecContext) {
		// Verifies that when spec changes, the schema-referencing status field updates too.
		rgd := generator.NewResourceGraphDefinition("test-status-schema-update",
			generator.WithSchema(
				"StatusSchemaUpdate", "v1alpha1",
				map[string]interface{}{
					"label": "string",
				},
				map[string]interface{}{
					"echoLabel": "${schema.spec.label}",
				},
			),
			generator.WithResource("cm", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "static-cm",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		instanceName := "test-schema-update"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "StatusSchemaUpdate",
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"label": "first",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			echoLabel, found, err := unstructured.NestedString(instance.Object, "status", "echoLabel")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(echoLabel).To(Equal("first"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Update spec.label and verify status tracks it.
		Expect(unstructured.SetNestedField(instance.Object, "second", "spec", "label")).To(Succeed())
		Expect(env.Client.Update(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			echoLabel, found, err := unstructured.NestedString(instance.Object, "status", "echoLabel")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(echoLabel).To(Equal("second"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
