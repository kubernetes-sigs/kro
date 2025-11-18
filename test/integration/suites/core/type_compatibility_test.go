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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Structural Type Compatibility", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	Context("Comprehensive Type Compatibility", func() {
		It("should validate all type compatibility scenarios", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-comprehensive-types",
				generator.WithSchema(
					"AppDeployment", "v1alpha1",
					map[string]interface{}{
						"appName":    "string",
						"containers": "[]containerConfig",
						"ports":      "[]portConfig",
						"labels":     "map[string]string",
					},
					nil,
					generator.WithTypes(map[string]interface{}{
						"containerConfig": map[string]interface{}{
							"name":  "string",
							"image": "string",
						},
						"portConfig": map[string]interface{}{
							"name":          "string",
							"containerPort": "integer",
							"protocol":      "string",
						},
					}),
				),
				generator.WithResource("basePod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":      "${schema.spec.appName}-base",
						"namespace": namespace,
						"labels":    "${schema.spec.labels}",
					},
					"spec": map[string]interface{}{
						"containers": "${schema.spec.containers}",
					},
				}, nil, nil),
				generator.WithResource("replicaPod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":        "${schema.spec.appName}-replica",
						"namespace":   namespace,
						"labels":      "${basePod.metadata.labels}",
						"annotations": "${basePod.metadata.annotations}",
					},
					"spec": map[string]interface{}{
						"containers": "${basePod.spec.containers}",
					},
				}, nil, nil),
				generator.WithResource("podWithPorts", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":      "${schema.spec.appName}-ports",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "${schema.spec.containers[0].name}",
								"image": "${schema.spec.containers[0].image}",
								"ports": []interface{}{
									map[string]interface{}{
										"name":          "${schema.spec.ports[0].name}",
										"containerPort": "${schema.spec.ports[0].containerPort}",
										"protocol":      "${schema.spec.ports[0].protocol}",
									},
								},
							},
						},
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should reject incompatible types comprehensively", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-incompatible-types",
				generator.WithSchema(
					"BadAppDeployment", "v1alpha1",
					map[string]interface{}{
						"podName":    "string",
						"containers": "[]badContainerConfig",
					},
					nil,
					generator.WithTypes(map[string]interface{}{
						"badContainerConfig": map[string]interface{}{
							"name":       "integer",
							"image":      "string",
							"extraField": "string",
						},
					}),
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":      "${schema.spec.podName}",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"containers": "${schema.spec.containers}",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))

				var condition *krov1alpha1.Condition
				for _, cond := range rgd.Status.Conditions {
					if cond.Type == resourcegraphdefinition.Ready {
						condition = &cond
						break
					}
				}
				g.Expect(condition).ToNot(BeNil())
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(*condition.Message).To(Or(
					ContainSubstring("kind mismatch"),
					ContainSubstring("exists in output but not in expected type"),
				))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
	})
})
