// Copyright 2025 The Kube Resource Orchestrator Authors
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
	"context"
	"fmt"
	"time"

	"github.com/kro-run/kro/pkg/metadata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/testutil/generator"
)

var _ = Describe("Instance Resource Watch", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	DescribeTable("watch behavior on ConfigMap",
		func(ctx SpecContext, labelSet bool) {
			rgd := generator.NewResourceGraphDefinition(fmt.Sprintf("test-instance-resource-reconcile-%v", labelSet),
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
						"name": "static-configmap",
					},
					"data": map[string]interface{}{
						"key": "${schema.spec.field1}",
					},
				}, nil, nil),
			)

			if labelSet {
				rgd.ObjectMeta.Labels = map[string]string{
					metadata.InstanceWatchResourcesLabel: "true",
				}
			}

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			Eventually(func(g Gomega, ctx context.Context) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.Conditions).To(Not(BeNil()))
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				for _, cond := range rgd.Status.Conditions {
					g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				}
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			instance := &unstructured.Unstructured{}
			instance.SetAPIVersion(krov1alpha1.GroupVersion.String())
			instance.SetKind("TestStatus")
			instance.SetName("test-instance")
			instance.SetNamespace(namespace)
			instance.Object["spec"] = map[string]interface{}{
				"field1": "foo",
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())

			Eventually(func(g Gomega, ctx context.Context) {
				g.Expect(env.Client.Get(ctx, types.NamespacedName{
					Name:      instance.GetName(),
					Namespace: instance.GetNamespace(),
				}, instance)).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			cfgMap := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx context.Context) {
				g.Expect(env.Client.Get(ctx, types.NamespacedName{
					Name:      "static-configmap",
					Namespace: namespace,
				}, cfgMap)).ToNot(HaveOccurred())
				g.Expect(cfgMap.Data).To(HaveKeyWithValue("key", "foo"))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			time.Sleep(5 * time.Second)
			cfgMap.Data["key"] = "updated"
			Expect(env.Client.Update(ctx, cfgMap)).To(Succeed())

			var expectedKeyValue string
			if labelSet {
				expectedKeyValue = "foo"
			} else {
				expectedKeyValue = "updated"
			}

			Eventually(func(g Gomega, ctx context.Context) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      "static-configmap",
					Namespace: namespace,
				}, cfgMap)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cfgMap.Data).To(HaveKeyWithValue("key", expectedKeyValue))
			}, 3*time.Second, time.Second).WithContext(ctx).Should(Succeed(),
				"ConfigMap should be updated to reflect the instance reconcile due to watch on resources",
			)
		},
		Entry("should not update when watch label is not set", false),
		Entry("should update when watch label is set", true),
	)
})
