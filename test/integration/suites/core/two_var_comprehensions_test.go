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

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("TwoVarComprehensions", func() {
	It("should evaluate transformMap, transformMapEntry, and transformList in resource templates", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		// Create an RGD that uses two-variable comprehension macros in CEL
		// expressions to compute ConfigMap data values. This exercises the
		// full pipeline: graph build → CEL compilation → runtime eval.
		rgd := generator.NewResourceGraphDefinition("test-two-var-comp",
			generator.WithSchema(
				"TwoVarComp", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
				},
				"data": map[string]interface{}{
					// transformMap on a map: add 10 to each value, then read key 'a'
					"transformMapValue": "${string({'a': 1, 'b': 2}.transformMap(k, v, v + 10)['a'])}",
					// transformMapEntry on a list: build {value: index} map, read 'x'
					"transformMapEntryValue": "${string(['x', 'y'].transformMapEntry(i, v, {v: string(i)})['x'])}",
					// transformList on a map: extract keys, check size
					"transformListSize": "${string({'p': 1, 'q': 2, 'r': 3}.transformList(k, v, k).size())}",
					// transformMap on a list with filter: keep only index > 0
					"transformMapFiltered": "${string([10, 20, 30].transformMap(i, v, i > 0, v * 2).size())}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		// Wait for the RGD to become Active (graph builds, CEL compiles)
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create an instance of the RGD
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TwoVarComp",
				"metadata": map[string]interface{}{
					"name":      "test-two-var",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": "two-var-result",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify the ConfigMap was created with correctly computed values
		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "two-var-result",
				Namespace: namespace,
			}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			// transformMap: {'a':1,'b':2}.transformMap(k,v,v+10)['a'] == 11
			g.Expect(cm.Data["transformMapValue"]).To(Equal("11"))
			// transformMapEntry: ['x','y'].transformMapEntry(i,v,{v:string(i)})['x'] == "0"
			g.Expect(cm.Data["transformMapEntryValue"]).To(Equal("0"))
			// transformList: {'p':1,'q':2,'r':3}.transformList(k,v,k).size() == 3
			g.Expect(cm.Data["transformListSize"]).To(Equal("3"))
			// transformMap filtered: [10,20,30].transformMap(i,v,i>0,v*2).size() == 2
			g.Expect(cm.Data["transformMapFiltered"]).To(Equal("2"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
	})
})
