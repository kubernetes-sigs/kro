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

// Values inside an instance are data, not templates.
//
// An instance is written by the consumer of a kro-generated API, who knows the
// schema but not the ResourceGraphDefinition behind it. Whatever they put in a
// spec field or an annotation has to be carried through verbatim:
//
//   - Users legitimately store text that contains "${...}" — shell snippets,
//     container commands, Grafana dashboards, config for a downstream
//     templating engine. That text must survive a round trip unchanged.
//   - Instance values must not be resolvable against the graph's resources.
//     Resource ids are an internal detail of the definition, and resources can
//     hold cluster state the instance's author has no access to, so a value
//     that happens to name one must stay a plain string.
//
// Both properties are easy to lose the moment instance data is fed through the
// same path as author-written templates, and neither fails loudly when it
// breaks: the value is either rejected with an error about an identifier the
// user never wrote, or silently replaced by something else entirely.
var _ = Describe("InstanceExpressionIsolation", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	It("carries a literal ${...} in an instance spec field through to a managed resource", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-instance-literal-expr",
			generator.WithSchema(
				"TestInstanceLiteralExpr", "v1alpha1",
				map[string]any{
					"name": "string",
					"note": "string",
				},
				nil,
			),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-cm",
				},
				"data": map[string]any{
					"note": "${schema.spec.note}",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		// A shell snippet is the most common way this shows up in practice.
		const literal = "echo ${HOME} && echo ${NOT_A_RESOURCE.field}"

		name := "literal-expr"
		instance := newInstance("TestInstanceLiteralExpr", name, namespace, map[string]any{
			"name": name,
			"note": literal,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")

		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-cm",
				Namespace: namespace,
			}, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKeyWithValue("note", literal))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("reconciles an instance whose annotations contain a literal ${...}", func(ctx SpecContext) {
		// Annotations are not covered by the generated CRD's schema, so they
		// are the least constrained place a "${...}" can appear. An instance
		// carrying one must still reconcile normally.
		rgd := generator.NewResourceGraphDefinition("test-instance-annotation-expr",
			generator.WithSchema(
				"TestInstanceAnnotationExpr", "v1alpha1",
				map[string]any{
					"name": "string",
				},
				nil,
			),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-cm",
				},
				"data": map[string]any{
					"name": "${schema.spec.name}",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "annotation-expr"
		instance := newInstance("TestInstanceAnnotationExpr", name, namespace, map[string]any{
			"name": name,
		})
		Expect(unstructured.SetNestedStringMap(instance.Object, map[string]string{
			"example.com/command": "run ${SOME_VAR}",
		}, "metadata", "annotations")).To(Succeed())
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")

		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-cm",
				Namespace: namespace,
			}, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKeyWithValue("name", name))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("does not resolve an instance value that names one of the definition's resources", func(ctx SpecContext) {
		// "${secretCarrier.data.token}" is a valid expression when an author
		// writes it in a template. Supplied as instance data it must stay a
		// string: the instance's author is not the definition's author and
		// should not be able to read a resource's contents by naming it.
		rgd := generator.NewResourceGraphDefinition("test-instance-scope-isolation",
			generator.WithSchema(
				"TestInstanceScopeIsolation", "v1alpha1",
				map[string]any{
					"name": "string",
					"note": "string",
				},
				nil,
			),
			generator.WithResource("secretCarrier", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-carrier",
				},
				"data": map[string]any{
					"token": "carrier-token-value",
				},
			}, nil, nil),
			generator.WithResource("echo", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-echo",
				},
				"data": map[string]any{
					"note": "${schema.spec.note}",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		const literal = "${secretCarrier.data.token}"

		name := "scope-isolation"
		instance := newInstance("TestInstanceScopeIsolation", name, namespace, map[string]any{
			"name": name,
			"note": literal,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")

		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-echo",
				Namespace: namespace,
			}, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKeyWithValue("note", literal))
			g.Expect(cm.Data["note"]).NotTo(Equal("carrier-token-value"),
				"the referenced resource's value must not be substituted into instance data")
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})

// newInstance builds an unstructured instance of a kro-generated kind.
func newInstance(kind, name, namespace string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
			"kind":       kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": spec,
		},
	}
}

// waitForInstanceState blocks until the instance reports the wanted
// .status.state, surfacing its conditions on failure.
func waitForInstanceState(
	ctx SpecContext,
	instance *unstructured.Unstructured,
	name, namespace, want string,
) {
	Eventually(func(g Gomega, ctx SpecContext) {
		g.Expect(env.Client.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		}, instance)).To(Succeed())
		state, _, err := unstructured.NestedString(instance.Object, "status", "state")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(state).To(Equal(want), "instance conditions: %s", instanceConditions(instance))
	}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

// instanceConditions renders an instance's conditions as type=status(reason):
// message, for use in failure output.
func instanceConditions(instance *unstructured.Unstructured) string {
	conds, _, err := unstructured.NestedSlice(instance.Object, "status", "conditions")
	if err != nil || len(conds) == 0 {
		return "<none>"
	}
	out := ""
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(cond, "type")
		status, _, _ := unstructured.NestedString(cond, "status")
		reason, _, _ := unstructured.NestedString(cond, "reason")
		msg, _, _ := unstructured.NestedString(cond, "message")
		out += fmt.Sprintf("[%s=%s(%s): %s] ", typ, status, reason, msg)
	}
	return out
}
