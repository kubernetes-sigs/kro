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

var _ = Describe("ClusterScopedInstance", func() {
	// This test validates that a cluster-scoped instance CRD (scope: Cluster)
	// can own namespaced child resources, collections, and external refs.
	// Children must specify their namespace explicitly via CEL.

	var (
		namespace string
		rgdName   string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		rgdName = fmt.Sprintf("test-cluster-scoped-%s", rand.String(5))
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	It("should reconcile a cluster-scoped instance with child resources and external refs", func(ctx SpecContext) {
		By("creating pre-existing resources for external refs")

		// External ref target: a ConfigMap looked up by name/namespace
		externalCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-config",
				Namespace: namespace,
			},
			Data: map[string]string{"setting": "enabled"},
		}
		Expect(env.Client.Create(ctx, externalCM)).To(Succeed())

		// External collection targets: ConfigMaps matched by label selector
		for i, team := range []string{"alpha", "beta"} {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("team-%s", team),
					Namespace: namespace,
					Labels:    map[string]string{"role": "team-config"},
				},
				Data: map[string]string{"team": team, "priority": fmt.Sprintf("%d", i+1)},
			}
			Expect(env.Client.Create(ctx, cm)).To(Succeed())
		}

		By("creating the cluster-scoped RGD")

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				"ClusterPolicy", "v1alpha1",
				map[string]interface{}{
					"targetNamespace": "string",
					"envValues":       "[]string",
				},
				map[string]interface{}{
					// Status from external ref
					"externalSetting": "${extconfig.data.setting}",
					// Status from external collection
					"teamCount": "${string(size(teamconfigs))}",
				},
				generator.WithScope(krov1alpha1.ResourceScopeCluster),
			),
			// 1. External ref by name/namespace — looked up in the target namespace
			generator.WithExternalRef("extconfig", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Name:      "ext-config",
					Namespace: "${schema.spec.targetNamespace}",
				},
			}, nil, nil),
			// 2. External collection by selector/namespace
			generator.WithExternalRef("teamconfigs", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Namespace: "${schema.spec.targetNamespace}",
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"role": "team-config"},
					},
				},
			}, nil, nil),
			// 3. Normal resource — namespace set explicitly via CEL
			generator.WithResource("policyCm", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "${schema.metadata.name}-policy",
					"namespace": "${schema.spec.targetNamespace}",
				},
				"data": map[string]interface{}{
					"setting": "${extconfig.data.setting}",
				},
			}, nil, nil),
			// 4. Collection — one ConfigMap per envValue, namespace explicit
			generator.WithResourceCollection("envCms", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "${schema.metadata.name}-env-${val}",
					"namespace": "${schema.spec.targetNamespace}",
				},
				"data": map[string]interface{}{
					"env": "${val}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"val": "${schema.spec.envValues}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, &krov1alpha1.ResourceGraphDefinition{})
				g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})

		By("waiting for RGD to become active")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating cluster-scoped instance (no namespace)")
		instanceName := fmt.Sprintf("test-policy-%s", rand.String(5))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "ClusterPolicy",
				"metadata": map[string]interface{}{
					"name": instanceName,
				},
				"spec": map[string]interface{}{
					"targetNamespace": namespace,
					"envValues":       []interface{}{"dev", "staging"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("waiting for instance to become ACTIVE")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("verifying instance has no namespace (cluster-scoped)")
		Expect(instance.GetNamespace()).To(BeEmpty())

		By("verifying status fields from external refs")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			setting, found, err := unstructured.NestedString(instance.Object, "status", "externalSetting")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(setting).To(Equal("enabled"))

			teamCount, found, err := unstructured.NestedString(instance.Object, "status", "teamCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(teamCount).To(Equal("2"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("verifying normal resource was created in the target namespace")
		policyCM := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-policy",
				Namespace: namespace,
			}, policyCM)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(policyCM.Data["setting"]).To(Equal("enabled"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("verifying collection resources were created in the target namespace")
		for _, val := range []string{"dev", "staging"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-env-%s", instanceName, val),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["env"]).To(Equal(val))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		By("deleting the cluster-scoped instance")
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("verifying child resources were cleaned up")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-policy",
				Namespace: namespace,
			}, &corev1.ConfigMap{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "policy configmap should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, val := range []string{"dev", "staging"} {
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-env-%s", instanceName, val),
					Namespace: namespace,
				}, &corev1.ConfigMap{})
				g.Expect(err).To(MatchError(errors.IsNotFound, "collection configmap should be deleted"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}
	})
})
