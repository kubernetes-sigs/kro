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

var _ = Describe("Metadata Fields Access", func() {
	It("should access all metadata fields including namespace in CEL expressions", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		// Create RGD that accesses ALL metadata fields
		rgd := generator.NewResourceGraphDefinition("metadata-field-tester",
			generator.WithSchema(
				"MetadataTest", "v1alpha1",
				map[string]interface{}{
					"dummy": "string",
				},
				map[string]interface{}{},
			),
			generator.WithResource("testConfigMap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "${schema.metadata.name}-${schema.metadata.namespace}-metadata-test",
					"namespace": "${schema.metadata.namespace}",
				},
				"data": map[string]interface{}{
					"instance-name":            "${schema.metadata.name}",
					"instance-namespace":       "${schema.metadata.namespace}",
					"instance-uid":             "${schema.metadata.uid}",
					"instance-generation":      "${string(schema.metadata.generation)}",
					"instance-resourceVersion": "${schema.metadata.resourceVersion}",
					"instance-generateName":    "${schema.metadata.?generateName.orValue(\"not-set\")}",
					"instance-has-labels":      "${has(schema.metadata.labels) ? \"true\" : \"false\"}",
					"instance-has-annotations": "${has(schema.metadata.annotations) ? \"true\" : \"false\"}",
				},
			}, nil, nil),
		)

		// Create RGD
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		// Wait for RGD to be active (which means CRD is registered and ready)
		Eventually(func(g Gomega, ctx SpecContext) {
			created := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, created)).To(Succeed())
			g.Expect(created.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "MetadataTest",
				"metadata": map[string]interface{}{
					"name":      "test-instance",
					"namespace": namespace,
					"labels": map[string]interface{}{
						"test-label": "test-value",
					},
					"annotations": map[string]interface{}{
						"test-annotation": "test-value",
					},
				},
				"spec": map[string]interface{}{
					"dummy": "value",
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		// Wait for ConfigMap to be created with all metadata fields
		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			expectedConfigMapName := fmt.Sprintf("test-instance-%s-metadata-test", namespace)
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      expectedConfigMapName,
				Namespace: namespace,
			}, cm)).To(Succeed())

			// Verify all metadata fields are populated
			g.Expect(cm.Data).To(HaveKey("instance-name"))
			g.Expect(cm.Data["instance-name"]).To(Equal("test-instance"))

			g.Expect(cm.Data).To(HaveKey("instance-namespace"))
			g.Expect(cm.Data["instance-namespace"]).To(Equal(namespace))

			g.Expect(cm.Data).To(HaveKey("instance-uid"))
			g.Expect(cm.Data["instance-uid"]).ToNot(BeEmpty())

			g.Expect(cm.Data).To(HaveKey("instance-generation"))
			g.Expect(cm.Data["instance-generation"]).ToNot(BeEmpty())

			g.Expect(cm.Data).To(HaveKey("instance-resourceVersion"))
			g.Expect(cm.Data["instance-resourceVersion"]).ToNot(BeEmpty())

			g.Expect(cm.Data).To(HaveKey("instance-has-labels"))
			g.Expect(cm.Data["instance-has-labels"]).To(Equal("true"))

			g.Expect(cm.Data).To(HaveKey("instance-has-annotations"))
			g.Expect(cm.Data["instance-has-annotations"]).To(Equal("true"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
