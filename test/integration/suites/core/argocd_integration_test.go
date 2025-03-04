// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package core_test

import (
	"context"
	"fmt"
	"github.com/kro-run/kro/pkg/metadata"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/testutil/generator"
)

var _ = Describe("ArgoCD", func() {
	var (
		ctx       context.Context
		namespace string
	)
	BeforeEach(func() {
		ctx = context.Background()
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should have correct label and ownership propagation", func() {
		rgd := generator.NewResourceGraphDefinition("argo-label-test",
			generator.WithSchema(
				"TestStatus", "v1alpha1",
				map[string]interface{}{
					"field1": "string",
				},
				nil,
			),
			generator.WithResource("res1", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "${schema.spec.field1}",
					"namespace": namespace,
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition status
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, rgd)
			g.Expect(err).ToNot(HaveOccurred())

			// Check conditions
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

		}, 10*time.Second, time.Second).Should(Succeed())

		simpleInstance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KroDomainName, "v1alpha1"),
				"kind":       "TestStatus",
				"metadata": map[string]interface{}{
					"name":      "sample-instance",
					"namespace": namespace,
					"labels": map[string]interface{}{
						metadata.K8sAppInstanceLabel: "ArgoTestApp",
					},
				},
				"spec": map[string]interface{}{
					"field1": "sample-instance-map",
				},
			},
		}

		Expect(env.Client.Create(ctx, simpleInstance)).To(Succeed())

		// Verify ResourceGraphDefinition status
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Namespace: namespace,
				Name: simpleInstance.GetName(),
			}, simpleInstance)
			g.Expect(err).ToNot(HaveOccurred())

		}, 10*time.Second, time.Second).Should(Succeed())

		// verify ConfigMap including OwnerReference
		Eventually(func(g Gomega) {

			configMap := corev1.ConfigMap{}

			err := env.Client.Get(ctx, types.NamespacedName{Namespace: namespace,
				Name: "sample-instance-map",
			}, &configMap)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMap.Labels).To(HaveKeyWithValue(metadata.K8sAppInstanceLabel, "ArgoTestApp"))
			g.Expect(configMap.OwnerReferences).To(HaveLen(1))

			ownerRef := configMap.OwnerReferences[0]
			g.Expect(ownerRef.Name).To(Equal("sample-instance"))
			g.Expect(ownerRef.Kind).To(Equal("TestStatus"))

		}, 10*time.Second, time.Second).Should(Succeed())

	})
})
