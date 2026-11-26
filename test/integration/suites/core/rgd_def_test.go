// Copyright 2026 The Kubernetes Authors.
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("ResourceGraphDefinition def", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	waitActive := func(ctx SpecContext, rgd *krov1alpha1.ResourceGraphDefinition) {
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	}

	newInstance := func(kind, name string, spec map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
			"kind":       kind,
			"metadata":   map[string]any{"name": name, "namespace": namespace},
			"spec":       spec,
		}}
	}

	It("evaluates a def chain and feeds a template and a status field", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("rgd-def-basic",
			generator.WithSchema("RgdDefBasic", "v1alpha1",
				map[string]any{"app": "string", "env": "string"},
				map[string]any{"fullName": "${derived.full}"},
			),
			generator.WithDef("naming", map[string]any{
				"prefix": "${schema.spec.app + '-' + schema.spec.env}",
			}, nil, nil, nil),
			generator.WithDef("derived", map[string]any{
				"full": "${naming.prefix + '-cfg'}",
			}, nil, nil, nil),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "${derived.full}"},
				"data":       map[string]any{"prefix": "${naming.prefix}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { _ = env.Client.Delete(ctx, rgd) })
		waitActive(ctx, rgd)

		instance := newInstance("RgdDefBasic", "basic", map[string]any{"app": "myapp", "env": "prod"})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { _ = env.Client.Delete(ctx, instance) })

		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "myapp-prod-cfg", Namespace: namespace}, cm)).To(Succeed())
			g.Expect(cm.Data["prefix"]).To(Equal("myapp-prod"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "basic", Namespace: namespace}, instance)).To(Succeed())
			state, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(state).To(Equal("ACTIVE"))
			full, _, _ := unstructured.NestedString(instance.Object, "status", "fullName")
			g.Expect(full).To(Equal("myapp-prod-cfg"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("expands a forEach def into a list consumed by a collection", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("rgd-def-foreach",
			generator.WithSchema("RgdDefForEach", "v1alpha1",
				map[string]any{"app": "string", "regions": "[]string"}, nil),
			generator.WithDef("labels",
				map[string]any{"full": "${schema.spec.app + '-' + region}"},
				[]krov1alpha1.ForEachDimension{{"region": "${schema.spec.regions}"}},
				nil, nil),
			generator.WithResourceCollection("cms", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "${label.full + '-cm'}"},
				"data":       map[string]any{"label": "${label.full}"},
			}, []krov1alpha1.ForEachDimension{{"label": "${labels}"}}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { _ = env.Client.Delete(ctx, rgd) })
		waitActive(ctx, rgd)

		instance := newInstance("RgdDefForEach", "foreach", map[string]any{
			"app": "myapp", "regions": []any{"east", "west"},
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { _ = env.Client.Delete(ctx, instance) })

		for _, region := range []string{"east", "west"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "myapp-" + region + "-cm", Namespace: namespace}, cm)).To(Succeed())
				g.Expect(cm.Data["label"]).To(Equal("myapp-" + region))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}
	})

	It("rejects an RGD whose def field type does not match the consuming field", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("rgd-def-mismatch",
			generator.WithSchema("RgdDefMismatch", "v1alpha1", map[string]any{"replicas": "integer"}, nil),
			generator.WithDef("computed", map[string]any{"count": "${schema.spec.replicas}"}, nil, nil, nil),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "cm"},
				"data":       map[string]any{"count": "${computed.count}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { _ = env.Client.Delete(ctx, rgd) })
		expectRGDInactiveWithError(ctx, rgd, "type mismatch")
	})

	It("rejects a resource that sets both template and def", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("rgd-def-both",
			generator.WithSchema("RgdDefBoth", "v1alpha1", map[string]any{"name": "string"}, nil),
			generator.WithDef("both", map[string]any{"a": "b"}, nil, nil, nil),
		)
		rgd.Spec.Resources[0].Template = runtime.RawExtension{
			Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x"}}`),
		}
		err := env.Client.Create(ctx, rgd)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of template, externalRef, or def"))
	})
})
