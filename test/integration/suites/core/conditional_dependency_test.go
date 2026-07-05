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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Conditional Dependencies", func() {
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

	It("should only create cache service when useCache=true", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("conditional-dep",
			generator.WithSchema(
				"ConditionalApp", "v1alpha1",
				map[string]interface{}{
					"name":     "string",
					"useCache": "boolean",
				},
				nil,
			),
			// Cache service
			generator.WithResource("cache", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-cache",
				},
				"spec": map[string]interface{}{
					"ports": []interface{}{
						map[string]interface{}{
							"port":       6379,
							"targetPort": 6379,
						},
					},
					"selector": map[string]interface{}{
						"app": "cache",
					},
				},
			}, nil, []string{"${schema.spec.useCache}"}),
			// Database service
			generator.WithResource("database", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-db",
				},
				"spec": map[string]interface{}{
					"ports": []interface{}{
						map[string]interface{}{
							"port":       5432,
							"targetPort": 5432,
						},
					},
					"selector": map[string]interface{}{
						"app": "database",
					},
				},
			}, nil, []string{"${!schema.spec.useCache}"}),
			// App deployment with conditional endpoint reference
			generator.WithResource("app", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-app",
				},
				"spec": map[string]interface{}{
					"replicas": 1,
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "app",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "app",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "app",
									"image": "nginx",
									"env": []interface{}{
										map[string]interface{}{
											"name":  "BACKEND_ENDPOINT",
											"value": "${schema.spec.useCache ? cache.spec.clusterIP : database.spec.clusterIP}",
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

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 1*time.Second).Should(Succeed())

		// Create instance with useCache=true
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "ConditionalApp",
				"metadata": map[string]interface{}{
					"name":      "test-app",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":     "myapp",
					"useCache": true,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify cache service is created
		cacheService := &corev1.Service{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "myapp-cache",
				Namespace: namespace,
			}, cacheService)
			g.Expect(err).NotTo(HaveOccurred())
		}, 30*time.Second, 1*time.Second).Should(Succeed())

		// Verify database service is NOT created
		dbService := &corev1.Service{}
		Consistently(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "myapp-db",
				Namespace: namespace,
			}, dbService)
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}, 5*time.Second, 1*time.Second).Should(Succeed())

		// Verify instance becomes ACTIVE
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-app",
				Namespace: namespace,
			}, instance)
			g.Expect(err).NotTo(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})

	It("should only create database service when useCache=false", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("conditional-dep-2",
			generator.WithSchema(
				"ConditionalApp2", "v1alpha1",
				map[string]interface{}{
					"name":     "string",
					"useCache": "boolean",
				},
				nil,
			),
			generator.WithResource("cache", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-cache",
				},
				"spec": map[string]interface{}{
					"ports": []interface{}{
						map[string]interface{}{
							"port":       6379,
							"targetPort": 6379,
						},
					},
					"selector": map[string]interface{}{
						"app": "cache",
					},
				},
			}, nil, []string{"${schema.spec.useCache}"}),
			generator.WithResource("database", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-db",
				},
				"spec": map[string]interface{}{
					"ports": []interface{}{
						map[string]interface{}{
							"port":       5432,
							"targetPort": 5432,
						},
					},
					"selector": map[string]interface{}{
						"app": "database",
					},
				},
			}, nil, []string{"${!schema.spec.useCache}"}),
			generator.WithResource("app", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-app",
				},
				"spec": map[string]interface{}{
					"replicas": 1,
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "app",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "app",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "app",
									"image": "nginx",
									"env": []interface{}{
										map[string]interface{}{
											"name":  "BACKEND_ENDPOINT",
											"value": "${schema.spec.useCache ? cache.spec.clusterIP : database.spec.clusterIP}",
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

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 1*time.Second).Should(Succeed())

		// Create instance with useCache=false
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "ConditionalApp2",
				"metadata": map[string]interface{}{
					"name":      "test-app",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":     "myapp",
					"useCache": false,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify database service is created
		dbService := &corev1.Service{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "myapp-db",
				Namespace: namespace,
			}, dbService)
			g.Expect(err).NotTo(HaveOccurred())
		}, 30*time.Second, 1*time.Second).Should(Succeed())

		// Verify cache service is NOT created
		cacheService := &corev1.Service{}
		Consistently(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "myapp-cache",
				Namespace: namespace,
			}, cacheService)
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}, 5*time.Second, 1*time.Second).Should(Succeed())

		// Verify instance becomes ACTIVE
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-app",
				Namespace: namespace,
			}, instance)
			g.Expect(err).NotTo(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})

})
