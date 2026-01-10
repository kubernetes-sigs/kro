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

	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Labels and Annotations", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
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

	It("should have correct conditions when ResourceGraphDefinition is created", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-apply",
			generator.WithSchema(
				"TestApply", "v1alpha1",
				map[string]interface{}{
					"field1": "string",
				},
				nil,
			),
			generator.WithResource("res1", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.field1}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		apiVersion, kind := schema.GroupVersionKind{
			Group:   rgd.Spec.Schema.Group,
			Version: rgd.Spec.Schema.APIVersion,
			Kind:    rgd.Spec.Schema.Kind,
		}.ToAPIVersionAndKind()

		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": apiVersion,
				"kind":       kind,
				"metadata": map[string]interface{}{
					"name":      "instance",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"field1": "foobar",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			g.Expect(instance.GetAnnotations()).To(SatisfyAll(
				Not(BeNil()),
				HaveKeyWithValue(applyset.ApplySetGKsAnnotation, "ConfigMap"),
				HaveKeyWithValue(applyset.ApplySetToolingAnnotation, "kro/devel"),
				// Per KEP-3659, additional-namespaces excludes parent namespace (implicit)
				HaveKeyWithValue(applyset.ApplySetAdditionalNamespacesAnnotation, ""),
			), "instance should have correct applyset annotations")

			g.Expect(instance.GetLabels()).To(SatisfyAll(
				Not(BeNil()),
				HaveKeyWithValue(applyset.ApplySetParentIDLabel, applyset.ID(instance)),
			), "instance should be assigned as apply set parent")

			g.Expect(instance.GetLabels()).To(SatisfyAll(
				HaveKeyWithValue(metadata.KROVersionLabel, "devel"),
				HaveKeyWithValue(metadata.OwnedLabel, "true"),
				HaveKeyWithValue(metadata.ResourceGraphDefinitionIDLabel, string(rgd.GetUID())),
				HaveKeyWithValue(metadata.ResourceGraphDefinitionNameLabel, rgd.GetName()),
			), "default kro labels should also be present")

		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		cfgMap := &corev1.ConfigMap{}
		err := env.Client.Get(ctx, types.NamespacedName{
			Name:      "foobar",
			Namespace: namespace,
		}, cfgMap)
		Expect(err).ToNot(HaveOccurred())
		Expect(cfgMap.GetLabels()).To(SatisfyAll(
			HaveKeyWithValue(applyset.ApplysetPartOfLabel, applyset.ID(instance)),
			HaveKeyWithValue(metadata.InstanceNamespaceLabel, instance.GetNamespace()),
			HaveKeyWithValue(metadata.InstanceLabel, instance.GetName()),
			HaveKeyWithValue(metadata.InstanceIDLabel, string(instance.GetUID())),
			HaveKeyWithValue(metadata.KROVersionLabel, "devel"),
			HaveKeyWithValue(metadata.OwnedLabel, "true"),
			HaveKeyWithValue(metadata.ResourceGraphDefinitionIDLabel, string(rgd.GetUID())),
			HaveKeyWithValue(metadata.ResourceGraphDefinitionNameLabel, rgd.GetName()),
		), "config map should be created as part of apply set managed by instance created through rgd")
	})

})
