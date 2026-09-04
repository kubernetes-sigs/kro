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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// Pruning a resource that left the definition must not depend on unrelated
// resources being ready.
//
// Removing a resource from a ResourceGraphDefinition is how an author retires
// something, and it has to take effect on existing instances. Readiness of the
// *other* resources in the graph is a separate concern: a graph that contains
// one permanently-unready resource is normal in a degraded environment, and it
// must not pin retired resources in the cluster indefinitely.
//
// The failure mode is a silent leak. Nothing reports that a prune was skipped,
// so the orphaned object simply stays, and the instance keeps reporting
// whatever its readiness state was.
var _ = Describe("ResourcePruning", func() {
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

	It("prunes a removed resource while another resource is not ready", func(ctx SpecContext) {
		// "gate" never satisfies its readyWhen, so the instance stays
		// IN_PROGRESS for the whole spec. "retired" is removed from the
		// definition partway through and must still be cleaned up.
		newRGD := func(withRetired bool) *krov1alpha1.ResourceGraphDefinition {
			opts := []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"TestPruneWhileUnready", "v1alpha1",
					map[string]any{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("gate", map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]any{
						"name": "${schema.spec.name}-gate",
					},
					"data": map[string]any{
						"ready": "false",
					},
				}, []string{`${gate.data.ready == "true"}`}, nil),
			}
			if withRetired {
				opts = append(opts, generator.WithResource("retired", map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]any{
						"name": "${schema.spec.name}-retired",
					},
					"data": map[string]any{
						"keep": "for-now",
					},
				}, nil, nil))
			}
			return generator.NewResourceGraphDefinition("test-prune-while-unready", opts...)
		}

		rgd := newRGD(true)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "prune-unready"
		instance := newInstance("TestPruneWhileUnready", name, namespace, map[string]any{
			"name": name,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Both resources exist, and the instance is held IN_PROGRESS by "gate".
		for _, suffix := range []string{"-gate", "-retired"} {
			Eventually(func(g Gomega, ctx SpecContext) {
				g.Expect(env.Client.Get(ctx, types.NamespacedName{
					Name:      name + suffix,
					Namespace: namespace,
				}, &corev1.ConfigMap{})).To(Succeed())
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}
		waitForInstanceState(ctx, instance, name, namespace, "IN_PROGRESS")

		// Retire the resource.
		Eventually(func(g Gomega, ctx SpecContext) {
			current := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, current)).To(Succeed())
			current.Spec.Resources = newRGD(false).Spec.Resources
			g.Expect(env.Client.Update(ctx, current)).To(Succeed())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		waitForRGDActive(ctx, rgd.Name)

		// The retired resource is cleaned up even though "gate" is still not
		// ready, and the still-declared resource is left alone.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-retired",
				Namespace: namespace,
			}, &corev1.ConfigMap{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"resource removed from the definition was not pruned (err=%v); instance conditions: %s",
				err, instanceConditions(instance))
		}, 60*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Get(ctx, types.NamespacedName{
			Name:      name + "-gate",
			Namespace: namespace,
		}, &corev1.ConfigMap{})).To(Succeed(), "still-declared resource must not be pruned")
	})
})
