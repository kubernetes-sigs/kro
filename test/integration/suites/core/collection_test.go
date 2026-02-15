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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("ForEach Collections", func() {
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

	It("should create multiple ConfigMaps from a forEach collection", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-collection",
			generator.WithSchema(
				"MultiConfigMap", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key": "${value}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

			var readyCondition krov1alpha1.Condition
			for _, cond := range createdRGD.Status.Conditions {
				if cond.Type == resourcegraphdefinition.Ready {
					readyCondition = cond
				}
			}
			g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-multi-cm"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "MultiConfigMap",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{"alpha", "beta", "gamma"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"alpha", "beta", "gamma"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should handle cartesian product with multiple forEach iterators", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-cartesian",
			generator.WithSchema(
				"CartesianConfigMaps", "v1alpha1",
				map[string]interface{}{
					"name":    "string",
					"regions": "[]string",
					"tiers":   "[]string",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${region}-${tier}",
				},
				"data": map[string]interface{}{
					"region": "${region}",
					"tier":   "${tier}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"region": "${schema.spec.regions}"},
					{"tier": "${schema.spec.tiers}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-cartesian"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "CartesianConfigMaps",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":    name,
					"regions": []interface{}{"us", "eu"},
					"tiers":   []interface{}{"web", "api"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		expectedCombinations := []struct {
			region string
			tier   string
		}{
			{"us", "web"},
			{"us", "api"},
			{"eu", "web"},
			{"eu", "api"},
		}

		for _, combo := range expectedCombinations {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s-%s", name, combo.region, combo.tier),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["region"]).To(Equal(combo.region))
				g.Expect(cm.Data["tier"]).To(Equal(combo.tier))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should create collection with includeWhen condition", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-conditional-collection",
			generator.WithSchema(
				"ConditionalCollection", "v1alpha1",
				map[string]interface{}{
					"name":    "string",
					"values":  "[]string",
					"enabled": "boolean",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key": "${value}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				nil,
				[]string{"${schema.spec.enabled}"}, // includeWhen
			),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Test 1: Create instance with enabled=false - no ConfigMaps should be created
		name := "test-disabled"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "ConditionalCollection",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":    name,
					"values":  []interface{}{"alpha", "beta"},
					"enabled": false,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"alpha", "beta"} {
			cm := &corev1.ConfigMap{}
			Consistently(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test: includeWhen toggling from false to true
	// Verifies that when includeWhen changes from false to true, the collection
	// resources are created, and vice versa.
	It("should toggle collection resources when includeWhen changes", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-toggle-collection",
			generator.WithSchema(
				"ToggleCollection", "v1alpha1",
				map[string]interface{}{
					"name":    "string",
					"values":  "[]string",
					"enabled": "boolean",
				},
				nil,
			),
			generator.WithResourceCollection("configs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key": "${value}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				nil,
				[]string{"${schema.spec.enabled}"}, // includeWhen
			),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Phase 1: Create instance with enabled=false - no ConfigMaps should be created
		name := "test-toggle"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "ToggleCollection",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":    name,
					"values":  []interface{}{"alpha", "beta"},
					"enabled": false,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"alpha", "beta"} {
			cm := &corev1.ConfigMap{}
			Consistently(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// Phase 2: Toggle enabled to true - ConfigMaps should be created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			err = unstructured.SetNestedField(instance.Object, true, "spec", "enabled")
			g.Expect(err).ToNot(HaveOccurred())

			err = env.Client.Update(ctx, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"alpha", "beta"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// Phase 3: Toggle enabled back to false - ConfigMaps should be deleted
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			err = unstructured.SetNestedField(instance.Object, false, "spec", "enabled")
			g.Expect(err).ToNot(HaveOccurred())

			err = env.Client.Update(ctx, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"alpha", "beta"} {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).To(MatchError(errors.IsNotFound, "ConfigMap should be deleted"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should create collection with dependency on regular resource", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-collection-dependency",
			generator.WithSchema(
				"CollectionWithDependency", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			// First resource: a regular ConfigMap
			generator.WithResource("baseConfig", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-base",
				},
				"data": map[string]interface{}{
					"version": "v1.0.0",
				},
			}, nil, nil),
			// Second resource: collection that depends on the base config
			generator.WithResourceCollection("configs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key":     "${value}",
					"version": "${baseConfig.data.version}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-dep"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "CollectionWithDependency",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{"one", "two"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		baseCM := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-base", name),
				Namespace: namespace,
			}, baseCM)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(baseCM.Data["version"]).To(Equal("v1.0.0"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"one", "two"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
				g.Expect(cm.Data["version"]).To(Equal("v1.0.0"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should handle collection chaining with dynamic forEach expression", func(ctx SpecContext) {
		// This tests collection chaining where the forEach expression references another resource
		rgd := generator.NewResourceGraphDefinition("test-collection-chaining",
			generator.WithSchema(
				"CollectionChaining", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			// First resource: a regular ConfigMap that must exist before collection expands
			generator.WithResource("baseConfig", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-base",
				},
				"data": map[string]interface{}{
					"enabled": "true",
					"prefix":  "chained",
				},
			}, nil, nil),
			// Second resource: collection with forEach that references the first resource
			// The forEach expression checks if baseConfig.data.enabled exists
			generator.WithResourceCollection("chainedConfigs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${val}",
				},
				"data": map[string]interface{}{
					"key":    "${val}",
					"prefix": "${baseConfig.data.prefix}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					// forEach expression references baseConfig (another resource)
					// This creates a dynamic dependency on baseConfig
					{"val": "${has(baseConfig.data.enabled) ? schema.spec.values : []}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-chaining"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "CollectionChaining",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{"one", "two"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		baseCM := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-base", name),
				Namespace: namespace,
			}, baseCM)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(baseCM.Data["enabled"]).To(Equal("true"))
			g.Expect(baseCM.Data["prefix"]).To(Equal("chained"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"one", "two"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
				g.Expect(cm.Data["prefix"]).To(Equal("chained"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should handle collection-to-collection chaining", func(ctx SpecContext) {
		// This tests one collection iterating over another collection's output
		// The second collection's forEach expression is: ${firstConfigs}
		// which returns the list of expanded ConfigMaps from the first collection
		rgd := generator.NewResourceGraphDefinition("test-collection-to-collection",
			generator.WithSchema(
				"CollectionToCollection", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			// First collection: creates multiple ConfigMaps
			generator.WithResourceCollection("firstConfigs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-first-${val}",
				},
				"data": map[string]interface{}{
					"key":    "${val}",
					"source": "first-collection",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"val": "${schema.spec.values}"},
				},
				nil, nil),
			// Second collection: iterates over the first collection
			// ${firstConfigs} is typed as list(ConfigMap)
			generator.WithResourceCollection("secondConfigs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-second-${config.data.key}",
				},
				"data": map[string]interface{}{
					"originalKey":    "${config.data.key}",
					"originalSource": "${config.data.source}",
					"source":         "second-collection",
				},
			},
				[]krov1alpha1.ForEachDimension{
					// forEach iterates over another collection - ${firstConfigs} returns list(ConfigMap)
					{"config": "${firstConfigs}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-c2c"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "CollectionToCollection",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{"alpha", "beta"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"alpha", "beta"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-first-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
				g.Expect(cm.Data["source"]).To(Equal("first-collection"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// Second collection ConfigMaps are created by iterating over first collection
		for _, value := range []string{"alpha", "beta"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-second-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["originalKey"]).To(Equal(value))
				g.Expect(cm.Data["originalSource"]).To(Equal("first-collection"))
				g.Expect(cm.Data["source"]).To(Equal("second-collection"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should handle empty collection list gracefully", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-empty-collection",
			generator.WithSchema(
				"EmptyCollection", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key": "${value}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-empty"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "EmptyCollection",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{}, // Empty list
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Empty list means no resources to create - instance should still become ACTIVE
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Deep chaining: BaseConfig -> Collection1 -> Collection2 -> SummaryConfig -> FinalPods
	// Tests scale up/down by editing instance spec and verifying dependent collections update.
	// Instance status reflects collection sizes at each scale point.
	It("should handle deep chaining with scale up and scale down", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-deep-chaining",
			generator.WithSchema(
				"DeepChain", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"items":  "[]string",
					"prefix": "string",
				},
				map[string]interface{}{
					"level1Count": "${string(size(level1Configs))}",
					"level2Count": "${string(size(level2Configs))}",
					"podCount":    "${string(size(finalPods))}",
				},
			),
			generator.WithResource("baseConfig", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-base",
				},
				"data": map[string]interface{}{
					"prefix":    "${schema.spec.prefix}",
					"itemCount": "${string(size(schema.spec.items))}",
				},
			}, nil, nil),
			generator.WithResourceCollection("level1Configs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-l1-${entry}",
				},
				"data": map[string]interface{}{
					"entry":  "${entry}",
					"prefix": "${baseConfig.data.prefix}",
					"level":  "1",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"entry": "${schema.spec.items}"},
				},
				nil, nil),
			// Collection-to-collection: level2 iterates over level1
			generator.WithResourceCollection("level2Configs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-l2-${l1.data.entry}",
				},
				"data": map[string]interface{}{
					"sourceEntry":  "${l1.data.entry}",
					"sourcePrefix": "${l1.data.prefix}",
					"level":        "2",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"l1": "${level1Configs}"},
				},
				nil, nil),
			// Aggregates data from level2 collection using size()
			generator.WithResource("summaryConfig", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-summary",
				},
				"data": map[string]interface{}{
					"level1Count": "${string(size(level1Configs))}",
					"level2Count": "${string(size(level2Configs))}",
				},
			}, nil, nil),
			// Final collection depends on summaryConfig
			generator.WithResourceCollection("finalPods", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-pod-${l2.data.sourceEntry}",
				},
				"spec": map[string]interface{}{
					"restartPolicy": "Never",
					"containers": []interface{}{
						map[string]interface{}{
							"name":    "worker",
							"image":   "busybox:latest",
							"command": []interface{}{"sh", "-c", "echo ${summaryConfig.data.level2Count} items && sleep 3600"},
						},
					},
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"l2": "${level2Configs}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 15*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-deep"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "DeepChain",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"items":  []interface{}{"a", "b"},
					"prefix": "test",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
			// Validate instance status reflects collection sizes (2 items: a, b)
			level1Count, _, _ := unstructured.NestedString(instance.Object, "status", "level1Count")
			level2Count, _, _ := unstructured.NestedString(instance.Object, "status", "level2Count")
			podCount, _, _ := unstructured.NestedString(instance.Object, "status", "podCount")
			g.Expect(level1Count).To(Equal("2"), "status.level1Count should be 2")
			g.Expect(level2Count).To(Equal("2"), "status.level2Count should be 2")
			g.Expect(podCount).To(Equal("2"), "status.podCount should be 2")
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		baseConfig := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-base", name),
				Namespace: namespace,
			}, baseConfig)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(baseConfig.Data["prefix"]).To(Equal("test"))
			g.Expect(baseConfig.Data["itemCount"]).To(Equal("2"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, item := range []string{"a", "b"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-l1-%s", name, item),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["entry"]).To(Equal(item))
				g.Expect(cm.Data["prefix"]).To(Equal("test"))
				g.Expect(cm.Data["level"]).To(Equal("1"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		for _, item := range []string{"a", "b"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-l2-%s", name, item),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["sourceEntry"]).To(Equal(item))
				g.Expect(cm.Data["sourcePrefix"]).To(Equal("test"))
				g.Expect(cm.Data["level"]).To(Equal("2"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		summaryConfig := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-summary", name),
				Namespace: namespace,
			}, summaryConfig)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(summaryConfig.Data["level1Count"]).To(Equal("2"))
			g.Expect(summaryConfig.Data["level2Count"]).To(Equal("2"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, item := range []string{"a", "b"} {
			pod := &corev1.Pod{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-pod-%s", name, item),
					Namespace: namespace,
				}, pod)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(pod.Spec.Containers[0].Name).To(Equal("worker"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// SCALE UP: add item "c"
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// Update items to [a, b, c]
			err = unstructured.SetNestedSlice(instance.Object, []interface{}{"a", "b", "c"}, "spec", "items")
			g.Expect(err).ToNot(HaveOccurred())

			err = env.Client.Update(ctx, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-l1-%s", name, "c"),
				Namespace: namespace,
			}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data["entry"]).To(Equal("c"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-l2-%s", name, "c"),
				Namespace: namespace,
			}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data["sourceEntry"]).To(Equal("c"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-summary", name),
				Namespace: namespace,
			}, summaryConfig)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(summaryConfig.Data["level1Count"]).To(Equal("3"))
			g.Expect(summaryConfig.Data["level2Count"]).To(Equal("3"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		for _, item := range []string{"a", "b", "c"} {
			Eventually(func(g Gomega, ctx SpecContext) {
				pod := &corev1.Pod{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-pod-%s", name, item),
					Namespace: namespace,
				}, pod)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(pod.Spec.Containers[0].Name).To(Equal("worker"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// Validate instance status reflects scaled up collection sizes (3 items: a, b, c)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			level1Count, _, _ := unstructured.NestedString(instance.Object, "status", "level1Count")
			level2Count, _, _ := unstructured.NestedString(instance.Object, "status", "level2Count")
			podCount, _, _ := unstructured.NestedString(instance.Object, "status", "podCount")
			g.Expect(level1Count).To(Equal("3"), "status.level1Count should be 3 after scale up")
			g.Expect(level2Count).To(Equal("3"), "status.level2Count should be 3 after scale up")
			g.Expect(podCount).To(Equal("3"), "status.podCount should be 3 after scale up")
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// SCALE DOWN: remove item "b"
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// Update items to [a, c] (remove b)
			err = unstructured.SetNestedSlice(instance.Object, []interface{}{"a", "c"}, "spec", "items")
			g.Expect(err).ToNot(HaveOccurred())

			err = env.Client.Update(ctx, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-l1-%s", name, "b"),
				Namespace: namespace,
			}, cm)
			g.Expect(err).To(MatchError(errors.IsNotFound, "l1-b should be deleted"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-l2-%s", name, "b"),
				Namespace: namespace,
			}, cm)
			g.Expect(err).To(MatchError(errors.IsNotFound, "l2-b should be deleted"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			pod := &corev1.Pod{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-pod-%s", name, "b"),
				Namespace: namespace,
			}, pod)
			g.Expect(err).To(MatchError(errors.IsNotFound, "pod-b should be deleted"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-summary", name),
				Namespace: namespace,
			}, summaryConfig)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(summaryConfig.Data["level1Count"]).To(Equal("2"))
			g.Expect(summaryConfig.Data["level2Count"]).To(Equal("2"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, item := range []string{"a", "c"} {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-l1-%s", name, item),
				Namespace: namespace,
			}, cm)
			Expect(err).ToNot(HaveOccurred())
			Expect(cm.Data["entry"]).To(Equal(item))
		}

		for _, item := range []string{"a", "c"} {
			pod := &corev1.Pod{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-pod-%s", name, item),
				Namespace: namespace,
			}, pod)
			Expect(err).ToNot(HaveOccurred())
			Expect(pod.Spec.Containers[0].Name).To(Equal("worker"))
		}

		// Validate instance status reflects scaled down collection sizes (2 items: a, c)
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			level1Count, _, _ := unstructured.NestedString(instance.Object, "status", "level1Count")
			level2Count, _, _ := unstructured.NestedString(instance.Object, "status", "level2Count")
			podCount, _, _ := unstructured.NestedString(instance.Object, "status", "podCount")
			g.Expect(level1Count).To(Equal("2"), "status.level1Count should be 2 after scale down")
			g.Expect(level2Count).To(Equal("2"), "status.level2Count should be 2 after scale down")
			g.Expect(podCount).To(Equal("2"), "status.podCount should be 2 after scale down")
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Collection readyWhen blocks dependents until ALL items satisfy the condition.
	// Pattern: Worker Pods (collection) -> Coordinator ConfigMap
	It("should block dependent resources until collection readyWhen is satisfied", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-collection-dag-blocking",
			generator.WithSchema(
				"WorkerCluster", "v1alpha1",
				map[string]interface{}{
					"name":    "string",
					"workers": "[]string",
				},
				nil,
			),
			// readyWhen: each pod must be Running (AND across all items)
			generator.WithResourceCollection("workerPods", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-worker-${worker}",
				},
				"spec": map[string]interface{}{
					"restartPolicy": "Never",
					"containers": []interface{}{
						map[string]interface{}{
							"name":    "worker",
							"image":   "busybox:latest",
							"command": []interface{}{"sh", "-c", "sleep 3600"},
						},
					},
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"worker": "${schema.spec.workers}"},
				},
				[]string{"${each.status.phase == 'Running'}"},
				nil),
			// Depends on workerPods - blocked until all workers are Running
			generator.WithResource("coordinator", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-coordinator",
				},
				"data": map[string]interface{}{
					"workerCount":  "${string(size(workerPods))}",
					"firstWorker":  "${workerPods[0].metadata.name}",
					"allWorkerIPs": "${workerPods.map(w, has(w.status.podIP) ? w.status.podIP : 'pending').join(',')}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-dag"
		workers := []string{"alpha", "beta", "gamma"}
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "WorkerCluster",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":    name,
					"workers": []interface{}{"alpha", "beta", "gamma"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		for _, worker := range workers {
			Eventually(func(g Gomega, ctx SpecContext) {
				pod := &corev1.Pod{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-worker-%s", name, worker),
					Namespace: namespace,
				}, pod)
				g.Expect(err).ToNot(HaveOccurred())
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// Instance stays IN_PROGRESS while workers not ready
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("IN_PROGRESS"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Coordinator blocked until workers are Running
		coordinatorName := fmt.Sprintf("%s-coordinator", name)
		Consistently(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      coordinatorName,
				Namespace: namespace,
			}, cm)
			g.Expect(errors.IsNotFound(err)).To(BeTrue(), "coordinator should NOT be created while workers are not Running")
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Patch only 2 of 3 pods to Running - not enough
		for _, worker := range workers[:2] {
			pod := &corev1.Pod{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-worker-%s", name, worker),
				Namespace: namespace,
			}, pod)
			Expect(err).ToNot(HaveOccurred())
			pod.Status.Phase = corev1.PodRunning
			pod.Status.PodIP = fmt.Sprintf("10.0.0.%d", len(worker))
			Expect(env.Client.Status().Update(ctx, pod)).To(Succeed())
		}

		Consistently(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      coordinatorName,
				Namespace: namespace,
			}, cm)
			g.Expect(errors.IsNotFound(err)).To(BeTrue(), "coordinator should NOT be created until ALL workers are Running")
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Patch last worker to Running
		pod := &corev1.Pod{}
		err := env.Client.Get(ctx, types.NamespacedName{
			Name:      fmt.Sprintf("%s-worker-%s", name, workers[2]),
			Namespace: namespace,
		}, pod)
		Expect(err).ToNot(HaveOccurred())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = "10.0.0.99"
		Expect(env.Client.Status().Update(ctx, pod)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      coordinatorName,
				Namespace: namespace,
			}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data["workerCount"]).To(Equal("3"))
			g.Expect(cm.Data["firstWorker"]).To(ContainSubstring("worker-alpha"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// readyWhen with `each` keyword for per-item checks
	It("should evaluate readyWhen per-item expressions with each keyword", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-readywhen-collection",
			generator.WithSchema(
				"ReadyWhenCollection", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			generator.WithResourceCollection("configs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key":   "${value}",
					"ready": "true",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				[]string{"${each.data.ready == 'true'}"},
				nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-ready"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "ReadyWhenCollection",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{"one", "two", "three"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"one", "two", "three"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
				g.Expect(cm.Data["ready"]).To(Equal("true"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Collections without readyWhen are ready once all items are created
	It("should create collection resources without readyWhen expressions", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-no-readywhen-collection",
			generator.WithSchema(
				"NoReadyWhenCollection", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			generator.WithResourceCollection("configs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key": "${value}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-no-ready"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "NoReadyWhenCollection",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{"a", "b", "c"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"a", "b", "c"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Instance deletion triggers cleanup of ALL collection resources
	It("should delete all collection resources when instance is deleted", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-deletion-collection",
			generator.WithSchema(
				"DeletionTest", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			generator.WithResourceCollection("configs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key": "${value}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-deletion"
		values := []string{"one", "two", "three"}
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "DeletionTest",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{"one", "two", "three"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range values {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Collection resources must be cleaned up
		for _, value := range values {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).To(MatchError(errors.IsNotFound, fmt.Sprintf("ConfigMap %s-%s should be deleted", name, value)))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Drift detection: manually modified collection resources are restored
	It("should detect and restore drift in collection resources", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-drift-collection",
			generator.WithSchema(
				"DriftTest", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"items":  "[]string",
					"prefix": "string",
				},
				nil,
			),
			generator.WithResourceCollection("configs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${entry}",
				},
				"data": map[string]interface{}{
					"entry":  "${entry}",
					"prefix": "${schema.spec.prefix}",
					"static": "unchanged",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"entry": "${schema.spec.items}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		namespace := fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())

		name := "test-drift"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "DriftTest",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"items":  []interface{}{"alpha", "beta"},
					"prefix": "original",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		for _, item := range []string{"alpha", "beta"} {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, item),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["entry"]).To(Equal(item))
				g.Expect(cm.Data["prefix"]).To(Equal("original"))
				g.Expect(cm.Data["static"]).To(Equal("unchanged"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// Simulate drift
		alphaCM := &corev1.ConfigMap{}
		err := env.Client.Get(ctx, types.NamespacedName{
			Name:      fmt.Sprintf("%s-alpha", name),
			Namespace: namespace,
		}, alphaCM)
		Expect(err).ToNot(HaveOccurred())

		alphaCM.Data["prefix"] = "DRIFTED"
		alphaCM.Data["extra"] = "unexpected"
		Expect(env.Client.Update(ctx, alphaCM)).To(Succeed())

		// Server-side apply restores managed fields (extra field stays - unmanaged)
		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-alpha", name),
				Namespace: namespace,
			}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data["prefix"]).To(Equal("original"))
			g.Expect(cm.Data["entry"]).To(Equal("alpha"))
			g.Expect(cm.Data["static"]).To(Equal("unchanged"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Collection readyWhen without dependents: instance stays IN_PROGRESS until satisfied
	It("should keep instance IN_PROGRESS until collection readyWhen is satisfied (no dependents)", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-collection-readywhen-no-deps",
			generator.WithSchema(
				"StandaloneCollection", "v1alpha1",
				map[string]interface{}{
					"name":      "string",
					"values":    "[]string",
					"makeReady": "boolean | default=false", // Controls whether collection items are "ready"
				},
				nil,
			),
			generator.WithResourceCollection("configs", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key":   "${value}",
					"ready": "${string(schema.spec.makeReady)}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				[]string{"${each.data.ready == 'true'}"},
				nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-standalone"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "StandaloneCollection",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":      name,
					"values":    []interface{}{"alpha", "beta", "gamma"},
					"makeReady": false,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		for _, value := range []string{"alpha", "beta", "gamma"} {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
				g.Expect(cm.Data["ready"]).To(Equal("false"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("IN_PROGRESS"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			status, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(status).To(Equal("IN_PROGRESS"), "instance should stay IN_PROGRESS while collection items are not ready")
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		err := env.Client.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		}, instance)
		Expect(err).ToNot(HaveOccurred())
		Expect(unstructured.SetNestedField(instance.Object, true, "spec", "makeReady")).To(Succeed())
		Expect(env.Client.Update(ctx, instance)).To(Succeed())

		for _, value := range []string{"alpha", "beta", "gamma"} {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["ready"]).To(Equal("true"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should delete collection resources across multiple namespaces", func(ctx SpecContext) {
		altNamespace := fmt.Sprintf("alt-%s", namespace)
		altNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: altNamespace,
			},
		}
		Expect(env.Client.Create(ctx, altNs)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, altNs)
		})

		// Even indices -> ns1, odd indices -> ns2
		rgd := generator.NewResourceGraphDefinition("test-cross-ns-collection",
			generator.WithSchema(
				"CrossNamespaceCollection", "v1alpha1",
				map[string]interface{}{
					"name": "string",
					"ns1":  "string",
					"ns2":  "string",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "${schema.spec.name}-item-${string(i)}",
					"namespace": "${i % 2 == 0 ? schema.spec.ns1 : schema.spec.ns2}",
				},
				"data": map[string]interface{}{
					"index": "${string(i)}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"i": "${lists.range(6)}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := "test-cross-ns"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "CrossNamespaceCollection",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
					"ns1":  namespace,
					"ns2":  altNamespace,
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		for _, idx := range []int{0, 2, 4} {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-item-%d", name, idx),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["index"]).To(Equal(fmt.Sprintf("%d", idx)))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		for _, idx := range []int{1, 3, 5} {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-item-%d", name, idx),
					Namespace: altNamespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["index"]).To(Equal(fmt.Sprintf("%d", idx)))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cross-namespace deletion
		for _, idx := range []int{0, 2, 4} {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-item-%d", name, idx),
					Namespace: namespace,
				}, cm)
				g.Expect(err).To(MatchError(errors.IsNotFound, "ConfigMap in primary namespace should be deleted"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		for _, idx := range []int{1, 3, 5} {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-item-%d", name, idx),
					Namespace: altNamespace,
				}, cm)
				g.Expect(err).To(MatchError(errors.IsNotFound, "ConfigMap in alternate namespace should be deleted"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test for https://github.com/kubernetes-sigs/kro/issues/17#issuecomment-3843929563
	// Empty collections should allow dependents to resolve with empty list in CEL context.
	It("should allow dependents to reference empty collections in CEL expressions", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-empty-collection-dep",
			generator.WithSchema(
				"EmptyCollectionDep", "v1alpha1",
				map[string]interface{}{
					"names": "[]string",
				},
				nil,
			),
			// Collection of ConfigMaps - will be empty when names=[]
			generator.WithResourceCollection("entries", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.metadata.name}-entry-${name}",
				},
				"data": map[string]interface{}{
					"name": "${name}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"name": "${schema.spec.names}"},
				},
				nil, nil),
			generator.WithResource("summary", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.metadata.name}-summary",
				},
				"data": map[string]interface{}{
					// This expression references the 'entries' collection
					// When entries is empty, CEL should evaluate size(entries) to 0
					"itemCount": "${string(size(entries))}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Wait for RGD to become active
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance with EMPTY names array - this triggers the bug
		name := "test-empty-dep"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "EmptyCollectionDep",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"names": []interface{}{},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// The instance should become ACTIVE even with empty collection
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify the summary ConfigMap was created with itemCount=0
		summaryCM := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-summary", name),
				Namespace: namespace,
			}, summaryCM)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(summaryCM.Data).To(HaveKey("itemCount"))
			g.Expect(summaryCM.Data["itemCount"]).To(Equal("0"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
