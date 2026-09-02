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

	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// Status fields declared as arrays must be projected as arrays.
//
// A status block is a nested document, and an author can put expressions
// anywhere in it — including inside a list, which is how you expose a set of
// endpoints, addresses or names. The projected instance status has to keep
// that shape: an expression at endpoints[0] belongs in an array under
// "endpoints", not under a key literally named "endpoints[0]".
//
// This one is worth pinning because the failure mode is silent. A field
// written to the wrong path is dropped by the generated CRD's structural
// schema, so the instance still reports healthy and the status field simply
// never appears — no error, no condition, nothing in the logs.
var _ = Describe("StatusArrayProjection", func() {
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

	It("projects a top-level array of expressions as an array", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-status-array",
			generator.WithSchema(
				"TestStatusArray", "v1alpha1",
				map[string]any{
					"name": "string",
				},
				map[string]any{
					"endpoints": []any{
						"${primary.metadata.name}",
						"${secondary.metadata.name}",
					},
				},
			),
			generator.WithResource("primary", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-primary",
				},
			}, nil, nil),
			generator.WithResource("secondary", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-secondary",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "status-array"
		instance := newInstance("TestStatusArray", name, namespace, map[string]any{
			"name": name,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)).To(Succeed())

			endpoints, found, err := unstructured.NestedStringSlice(instance.Object, "status", "endpoints")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(),
				"status.endpoints must be projected as an array; conditions: %s",
				instanceConditions(instance))
			g.Expect(endpoints).To(ConsistOf(name+"-primary", name+"-secondary"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("projects an array nested under an object as an array", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-status-nested-array",
			generator.WithSchema(
				"TestStatusNestedArray", "v1alpha1",
				map[string]any{
					"name": "string",
				},
				map[string]any{
					"network": map[string]any{
						"names": []any{
							"${primary.metadata.name}",
						},
					},
				},
			),
			generator.WithResource("primary", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-primary",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "status-nested-array"
		instance := newInstance("TestStatusNestedArray", name, namespace, map[string]any{
			"name": name,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)).To(Succeed())

			names, found, err := unstructured.NestedStringSlice(instance.Object, "status", "network", "names")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(),
				"status.network.names must be projected as an array; conditions: %s",
				instanceConditions(instance))
			g.Expect(names).To(ConsistOf(name + "-primary"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
