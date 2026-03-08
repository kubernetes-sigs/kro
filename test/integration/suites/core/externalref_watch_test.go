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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("ExternalRef Watch", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})
	})

	It("external ref watch triggers re-reconciliation on change", func(ctx SpecContext) {
		By("creating RGD with external ref ConfigMap and managed ConfigMap that copies its data")
		rgd := generator.NewResourceGraphDefinition("test-extref-watch",
			generator.WithSchema(
				"TestExtRefWatch", "v1alpha1",
				map[string]interface{}{},
				nil,
			),
			generator.WithExternalRef("extcm", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Name: "ext-config",
				},
			}, nil, nil),
			generator.WithResource("managed", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "managed-config",
				},
				"data": map[string]interface{}{
					"replicas": "${extcm.data.replicas}",
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

		By("creating the external ConfigMap with replicas=1")
		extCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-config",
				Namespace: namespace,
			},
			Data: map[string]string{
				"replicas": "1",
			},
		}
		Expect(env.Client.Create(ctx, extCM)).To(Succeed())

		By("creating the instance")
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExtRefWatch",
				"metadata": map[string]interface{}{
					"name":      "test-instance",
					"namespace": namespace,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("waiting for instance to become ACTIVE and managed ConfigMap to have replicas=1")
		managedCM := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

			err = env.Client.Get(ctx, types.NamespacedName{
				Name:      "managed-config",
				Namespace: namespace,
			}, managedCM)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(managedCM.Data).To(HaveKeyWithValue("replicas", "1"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("updating external ConfigMap to replicas=3")
		Expect(env.Client.Get(ctx, types.NamespacedName{
			Name:      "ext-config",
			Namespace: namespace,
		}, extCM)).To(Succeed())
		extCM.Data["replicas"] = "3"
		Expect(env.Client.Update(ctx, extCM)).To(Succeed())

		By("asserting managed ConfigMap updates to replicas=3 within 3s (watch-driven)")
		// DefaultRequeueDuration is 5s. If the managed ConfigMap updates within 3s,
		// it proves the watch (not the requeue timer) triggered the reconciliation.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "managed-config",
				Namespace: namespace,
			}, managedCM)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(managedCM.Data).To(HaveKeyWithValue("replicas", "3"))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed(),
			"managed ConfigMap should update reactively via watch, not the 5s requeue timer",
		)
	})

	It("external collection with matchExpressions CEL resolves dynamic values", func(ctx SpecContext) {
		By("creating RGD with external collection ref using matchExpressions with CEL")
		rgd := generator.NewResourceGraphDefinition("test-extcoll-matchexpr",
			generator.WithSchema(
				"TestExtCollMatchExpr", "v1alpha1",
				map[string]interface{}{
					"teamName": "string",
				},
				map[string]interface{}{
					"configCount": "${string(size(extconfigs))}",
				},
			),
			generator.WithExternalRef("extconfigs", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "team",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"${schema.spec.teamName}"},
							},
						},
					},
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

		By("creating ConfigMaps with label team=bravo")
		cm1 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bravo-config-1",
				Namespace: namespace,
				Labels:    map[string]string{"team": "bravo"},
			},
			Data: map[string]string{"key": "value1"},
		}
		cm2 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bravo-config-2",
				Namespace: namespace,
				Labels:    map[string]string{"team": "bravo"},
			},
			Data: map[string]string{"key": "value2"},
		}
		Expect(env.Client.Create(ctx, cm1)).To(Succeed())
		Expect(env.Client.Create(ctx, cm2)).To(Succeed())

		By("creating a ConfigMap with different label to verify it's excluded")
		cmOther := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "charlie-config-1",
				Namespace: namespace,
				Labels:    map[string]string{"team": "charlie"},
			},
			Data: map[string]string{"key": "other"},
		}
		Expect(env.Client.Create(ctx, cmOther)).To(Succeed())

		By("creating the instance with teamName=bravo")
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExtCollMatchExpr",
				"metadata": map[string]interface{}{
					"name":      "test-matchexpr",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"teamName": "bravo",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("waiting for instance to become ACTIVE with configCount=2 (only team=bravo matched)")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

			configCount, found, err := unstructured.NestedString(instance.Object, "status", "configCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(configCount).To(Equal("2"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("external collection watch reacts to new matching resources", func(ctx SpecContext) {
		By("creating RGD with external collection ref using label selector")
		rgd := generator.NewResourceGraphDefinition("test-extcoll-watch",
			generator.WithSchema(
				"TestExtCollWatch", "v1alpha1",
				map[string]interface{}{},
				map[string]interface{}{
					"configCount": "${string(size(extconfigs))}",
				},
			),
			generator.WithExternalRef("extconfigs", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"team": "alpha",
						},
					},
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

		By("creating first ConfigMap with label team=alpha")
		cm1 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "alpha-config-1",
				Namespace: namespace,
				Labels: map[string]string{
					"team": "alpha",
				},
			},
			Data: map[string]string{"key": "value1"},
		}
		Expect(env.Client.Create(ctx, cm1)).To(Succeed())

		By("creating the instance")
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExtCollWatch",
				"metadata": map[string]interface{}{
					"name":      "test-instance",
					"namespace": namespace,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("waiting for instance to become ACTIVE with configCount=1")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

			configCount, found, err := unstructured.NestedString(instance.Object, "status", "configCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(configCount).To(Equal("1"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating a second ConfigMap with label team=alpha")
		cm2 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "alpha-config-2",
				Namespace: namespace,
				Labels: map[string]string{
					"team": "alpha",
				},
			},
			Data: map[string]string{"key": "value2"},
		}
		Expect(env.Client.Create(ctx, cm2)).To(Succeed())

		By("asserting configCount updates to 2 within 3s (watch-driven)")
		// DefaultRequeueDuration is 5s. If the status updates within 3s,
		// it proves the watch (not the requeue timer) triggered the reconciliation.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			configCount, found, err := unstructured.NestedString(instance.Object, "status", "configCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(configCount).To(Equal("2"))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed(),
			"instance status.configCount should update reactively via watch, not the 5s requeue timer",
		)
	})

	It("external collection sortBy orders resources by a data field", func(ctx SpecContext) {
		By("creating RGD with external collection ref and sortBy CEL expression")
		rgd := generator.NewResourceGraphDefinition("test-extcoll-sortby",
			generator.WithSchema(
				"TestExtCollSortBy", "v1alpha1",
				map[string]interface{}{},
				map[string]interface{}{
					"sortedNames": "${extconfigs.sortBy(c, c.data.priority).map(c, c.metadata.name).join(\",\")}",
					"configCount": "${string(size(extconfigs))}",
				},
			),
			generator.WithExternalRef("extconfigs", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "sorttest",
						},
					},
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

		By("creating 3 ConfigMaps out of alphabetical/priority order with label app=sorttest")
		cmCharlie := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cm-charlie",
				Namespace: namespace,
				Labels:    map[string]string{"app": "sorttest"},
			},
			Data: map[string]string{"priority": "1"},
		}
		cmAlpha := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cm-alpha",
				Namespace: namespace,
				Labels:    map[string]string{"app": "sorttest"},
			},
			Data: map[string]string{"priority": "2"},
		}
		cmBravo := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cm-bravo",
				Namespace: namespace,
				Labels:    map[string]string{"app": "sorttest"},
			},
			Data: map[string]string{"priority": "3"},
		}
		Expect(env.Client.Create(ctx, cmCharlie)).To(Succeed())
		Expect(env.Client.Create(ctx, cmAlpha)).To(Succeed())
		Expect(env.Client.Create(ctx, cmBravo)).To(Succeed())

		By("creating the instance")
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExtCollSortBy",
				"metadata": map[string]interface{}{
					"name":      "test-sortby",
					"namespace": namespace,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("waiting for instance to become ACTIVE with sorted names and correct count")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

			configCount, found, err := unstructured.NestedString(instance.Object, "status", "configCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(configCount).To(Equal("3"))

			sortedNames, found, err := unstructured.NestedString(instance.Object, "status", "sortedNames")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(sortedNames).To(Equal("cm-charlie,cm-alpha,cm-bravo"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("external ref to a chained RGD keeps the producer instance reconciling after consumer deletion",
		func(ctx SpecContext) {
			By("creating the producer RGD that owns the watched custom resource kind")
			producerRGD := generator.NewResourceGraphDefinition("test-chained-producer",
				generator.WithSchema(
					"WatchedDatabase", "v1alpha1",
					map[string]interface{}{
						"value": "string",
					},
					nil,
				),
				generator.WithResource("managed", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "watched-database-config",
					},
					"data": map[string]interface{}{
						"value": "${schema.spec.value}",
					},
				}, nil, nil),
			)
			Expect(env.Client.Create(ctx, producerRGD)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, producerRGD)).To(Succeed())
			})

			By("waiting for the producer RGD to become active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: producerRGD.Name}, producerRGD)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(producerRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating a producer instance and waiting for its managed ConfigMap")
			producerInstance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "WatchedDatabase",
					"metadata": map[string]interface{}{
						"name":      "watched-db",
						"namespace": namespace,
					},
					"spec": map[string]interface{}{
						"value": "one",
					},
				},
			}
			Expect(env.Client.Create(ctx, producerInstance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				err := env.Client.Delete(ctx, producerInstance)
				if err != nil && !apierrors.IsNotFound(err) {
					Expect(err).ToNot(HaveOccurred())
				}
			})

			producerCM := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      producerInstance.GetName(),
					Namespace: namespace,
				}, producerInstance)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(producerInstance.Object).To(HaveKey("status"))
				g.Expect(producerInstance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

				err = env.Client.Get(ctx, types.NamespacedName{
					Name:      "watched-database-config",
					Namespace: namespace,
				}, producerCM)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(producerCM.Data).To(HaveKeyWithValue("value", "one"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating a consumer RGD that externalRefs the producer custom resource kind")
			consumerRGD := generator.NewResourceGraphDefinition("test-chained-consumer",
				generator.WithSchema(
					"DatabaseObserver", "v1alpha1",
					map[string]interface{}{},
					nil,
				),
				generator.WithExternalRef("database", &krov1alpha1.ExternalRef{
					APIVersion: "kro.run/v1alpha1",
					Kind:       "WatchedDatabase",
					Metadata: krov1alpha1.ExternalRefMetadata{
						Name: "watched-db",
					},
				}, nil, nil),
				generator.WithResource("managed", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "database-observer-config",
					},
					"data": map[string]interface{}{
						"value": "${database.spec.value}",
					},
				}, nil, nil),
			)
			Expect(env.Client.Create(ctx, consumerRGD)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, consumerRGD)).To(Succeed())
			})

			By("waiting for the consumer RGD to become active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: consumerRGD.Name}, consumerRGD)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(consumerRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating the consumer instance so it registers an externalRef watch on the producer kind")
			consumerInstance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "DatabaseObserver",
					"metadata": map[string]interface{}{
						"name":      "db-observer",
						"namespace": namespace,
					},
				},
			}
			Expect(env.Client.Create(ctx, consumerInstance)).To(Succeed())

			consumerCM := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      consumerInstance.GetName(),
					Namespace: namespace,
				}, consumerInstance)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(consumerInstance.Object).To(HaveKey("status"))
				g.Expect(consumerInstance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

				err = env.Client.Get(ctx, types.NamespacedName{
					Name:      "database-observer-config",
					Namespace: namespace,
				}, consumerCM)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(consumerCM.Data).To(HaveKeyWithValue("value", "one"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("deleting the consumer instance so its externalRef watch is cleaned up")
			Expect(env.Client.Delete(ctx, consumerInstance)).To(Succeed())
			Eventually(func() bool {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      consumerInstance.GetName(),
					Namespace: namespace,
				}, consumerInstance)
				return apierrors.IsNotFound(err)
			}, 20*time.Second, time.Second).WithContext(ctx).Should(BeTrue())

			By("updating the producer instance after consumer deletion")
			Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      producerInstance.GetName(),
				Namespace: namespace,
			}, producerInstance)).To(Succeed())
			Expect(unstructured.SetNestedField(producerInstance.Object, "two", "spec", "value")).To(Succeed())
			Expect(env.Client.Update(ctx, producerInstance)).To(Succeed())

			By("asserting the producer instance still reconciles via its parent watch")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "watched-database-config",
					Namespace: namespace,
				}, producerCM)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(producerCM.Data).To(HaveKeyWithValue("value", "two"))
			}, 5*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed(),
				"producer managed resource should still update after the consumer externalRef instance is deleted",
			)
		})
})
