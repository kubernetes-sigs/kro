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

var _ = Describe("Variable Nodes", func() {
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

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	Context("RGD Reconciliation", func() {
		It("should reconcile RGD with variable nodes and generate correct CRD", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-data-node-rgd",
				generator.WithSchema(
					"TestDataNodeRGD", "v1alpha1",
					map[string]interface{}{
						"name":        "string",
						"environment": "string",
					},
					nil,
				),
				generator.WithVariable("naming", map[string]interface{}{
					"prefix": "${schema.spec.name + '-' + schema.spec.environment}",
				}, nil, nil, nil),
				generator.WithResource("configmap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${naming.prefix}-config",
					},
					"data": map[string]interface{}{
						"env": "${schema.spec.environment}",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			// Wait for RGD to become active
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				// Variable nodes should appear in topological order
				g.Expect(rgd.Status.TopologicalOrder).To(ContainElement("naming"))
				g.Expect(rgd.Status.TopologicalOrder).To(ContainElement("configmap"))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})

		It("should reject RGD with stuttered variable key", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-data-stutter",
				generator.WithSchema(
					"TestDataStutter", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithVariable("naming", map[string]interface{}{
					"naming": "stutter",
				}, nil, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			expectRGDInactiveWithError(ctx, rgd, "top-level key")
		})
	})

	Context("Instance Reconciliation", func() {
		It("should evaluate variable node expressions and pass values to downstream resources", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-data-instance",
				generator.WithSchema(
					"TestDataInstance", "v1alpha1",
					map[string]interface{}{
						"appName":     "string",
						"environment": "string",
					},
					nil,
				),
				generator.WithVariable("fullName", "${schema.spec.appName + '-' + schema.spec.environment}", nil, nil, nil),
				generator.WithResource("configmap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${fullName}",
					},
					"data": map[string]interface{}{
						"name": "${fullName}",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			// Wait for RGD to become active
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Create instance
			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
					"kind":       "TestDataInstance",
					"metadata": map[string]interface{}{
						"name":      "test-data",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"appName":     "myapp",
						"environment": "prod",
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance)
			})

			// Verify ConfigMap is created with the correct computed name
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "myapp-prod",
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["name"]).To(Equal("myapp-prod"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Verify instance becomes ACTIVE
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "test-data",
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())
				state, found, _ := unstructured.NestedString(instance.Object, "status", "state")
				g.Expect(found).To(BeTrue())
				g.Expect(state).To(Equal("ACTIVE"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Cleanup: delete instance and verify ConfigMap is cleaned up
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "myapp-prod",
					Namespace: namespace,
				}, cm)
				g.Expect(err).To(MatchError(errors.IsNotFound, "configmap should be deleted"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})

		It("should support variable-to-variable references with correct evaluation order", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-data-chain",
				generator.WithSchema(
					"TestDataChain", "v1alpha1",
					map[string]interface{}{
						"name": "string",
						"env":  "string",
					},
					nil,
				),
				// First variable node: compute prefix
				generator.WithVariable("naming", map[string]interface{}{
					"prefix": "${schema.spec.name + '-' + schema.spec.env}",
				}, nil, nil, nil),
				// Second variable node: references first variable node
				generator.WithVariable("labels", map[string]interface{}{
					"app": "${naming.prefix + '-app'}",
				}, nil, nil, nil),
				// Template resource: uses second variable node
				generator.WithResource("configmap", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${naming.prefix}-cm",
					},
					"data": map[string]interface{}{
						"appLabel": "${labels.app}",
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

			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
					"kind":       "TestDataChain",
					"metadata": map[string]interface{}{
						"name":      "test-chain",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"name": "svc",
						"env":  "staging",
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance)
			})

			// Verify ConfigMap has the chained variable node values
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "svc-staging-cm",
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["appLabel"]).To(Equal("svc-staging-app"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})

		It("should expand forEach variable nodes and pass expanded values to downstream resources", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-data-foreach-var",
				generator.WithSchema(
					"TestDataForEachVar", "v1alpha1",
					map[string]interface{}{
						"appName": "string",
						"regions": "[]string",
					},
					nil,
				),
				// Variable node with forEach: computes a label per region
				generator.WithVariable("labels",
					map[string]interface{}{
						"fullName": "${schema.spec.appName + '-' + region}",
					},
					nil, nil,
					[]krov1alpha1.ForEachDimension{
						{"region": "${schema.spec.regions}"},
					},
				),
				// One ConfigMap per forEach iteration, consuming the variable
				generator.WithResourceCollection("configmaps",
					map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]interface{}{
							"name": "${label.fullName + '-cm'}",
						},
						"data": map[string]interface{}{
							"label": "${label.fullName}",
						},
					},
					[]krov1alpha1.ForEachDimension{
						{"label": "${labels}"},
					},
					nil, nil,
				),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			// Wait for RGD to become active
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Create instance with two regions
			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
					"kind":       "TestDataForEachVar",
					"metadata": map[string]interface{}{
						"name":      "test-foreach-var",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"appName": "myapp",
						"regions": []interface{}{"east", "west"},
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				_ = env.Client.Delete(ctx, instance)
			})

			// Verify two ConfigMaps are created with correct computed names
			cm1 := &corev1.ConfigMap{}
			cm2 := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "myapp-east-cm",
					Namespace: namespace,
				}, cm1)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm1.Data["label"]).To(Equal("myapp-east"))

				err = env.Client.Get(ctx, types.NamespacedName{
					Name:      "myapp-west-cm",
					Namespace: namespace,
				}, cm2)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm2.Data["label"]).To(Equal("myapp-west"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Verify instance becomes ACTIVE
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "test-foreach-var",
					Namespace: namespace,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())
				state, found, _ := unstructured.NestedString(instance.Object, "status", "state")
				g.Expect(found).To(BeTrue())
				g.Expect(state).To(Equal("ACTIVE"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})
	})

	Context("Circular Dependencies", func() {
		It("should reject circular dependencies involving variable nodes", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-data-cycle",
				generator.WithSchema(
					"TestDataCycle", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithVariable("nodeA", map[string]interface{}{
					"val": "${nodeB.val}",
				}, nil, nil, nil),
				generator.WithVariable("nodeB", map[string]interface{}{
					"val": "${nodeA.val}",
				}, nil, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			})

			expectRGDInactiveWithError(ctx, rgd, "cycle")
		})
	})
})
