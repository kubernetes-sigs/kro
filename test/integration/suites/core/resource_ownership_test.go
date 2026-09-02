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

	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// How kro takes ownership of the resources it manages.
//
// Two properties, both about not losing track of what we own:
//
//   - The server-side-apply field manager is a compatibility surface. Which
//     manager owns a field decides whether dropping that field from a template
//     actually removes it from the live object: server-side apply only deletes
//     fields the *same* manager previously owned. If the name changes between
//     releases, every object created by an older version keeps its fields
//     owned by a manager that will never apply again, and template removals
//     silently stop taking effect on exactly those objects.
//
//   - Two instances that render the same object must not silently overwrite
//     each other. Fixed resource names are easy to write by accident (a name
//     that doesn't derive from the instance), and force-applying in a loop
//     leaves both instances reporting healthy while the object flips between
//     them on every reconcile.
var _ = Describe("ResourceOwnership", func() {
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

	It("applies managed resources under a stable field manager", func(ctx SpecContext) {
		// The expected name is spelled out rather than read from
		// applyset.FieldManager on purpose: referencing the constant would make
		// this assertion follow a rename instead of catching it. If the manager
		// name has to change, update it here and treat pre-existing objects as
		// a migration concern — see the note above.
		const expectedFieldManager = "kro.run/applyset"
		Expect(applyset.FieldManager).To(Equal(expectedFieldManager),
			"the field manager kro applies with has changed; objects created by "+
				"earlier versions keep their fields owned by %q, which no longer "+
				"applies, so template field removals will not take effect on them",
			expectedFieldManager)

		rgd := generator.NewResourceGraphDefinition("test-field-manager",
			generator.WithSchema(
				"TestFieldManager", "v1alpha1",
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
					"managed": "yes",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "field-manager"
		instance := newInstance("TestFieldManager", name, namespace, map[string]any{
			"name": name,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, instance)
		})
		waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")

		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-cm",
				Namespace: namespace,
			}, cm)).To(Succeed())

			managers := make([]string, 0, len(cm.GetManagedFields()))
			ownsData := false
			for _, entry := range cm.GetManagedFields() {
				managers = append(managers, fmt.Sprintf("%s/%s", entry.Manager, entry.Operation))
				if entry.Manager == expectedFieldManager &&
					entry.Operation == metav1.ManagedFieldsOperationApply {
					ownsData = true
				}
			}
			g.Expect(ownsData).To(BeTrue(),
				"no Apply entry for field manager %q on the managed resource; managedFields: %v",
				expectedFieldManager, managers)
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("does not let two instances silently overwrite the same resource", func(ctx SpecContext) {
		// The resource name is fixed, so both instances render the same object.
		rgd := generator.NewResourceGraphDefinition("test-shared-target",
			generator.WithSchema(
				"TestSharedTarget", "v1alpha1",
				map[string]any{
					"owner": "string",
				},
				nil,
			),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "shared-target",
				},
				"data": map[string]any{
					"owner": "${schema.spec.owner}",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		first := newInstance("TestSharedTarget", "owner-a", namespace, map[string]any{
			"owner": "a",
		})
		Expect(env.Client.Create(ctx, first)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, first)
		})
		waitForInstanceState(ctx, first, "owner-a", namespace, "ACTIVE")

		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      "shared-target",
				Namespace: namespace,
			}, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKeyWithValue("owner", "a"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		firstApplySetID := cm.Labels[applyset.ApplysetPartOfLabel]
		Expect(firstApplySetID).ToNot(BeEmpty(),
			"managed resource should carry an ApplySet membership label")

		// A second instance now claims the same object.
		second := newInstance("TestSharedTarget", "owner-b", namespace, map[string]any{
			"owner": "b",
		})
		Expect(env.Client.Create(ctx, second)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, second)
		})

		// The established owner keeps the object, and its ApplySet membership
		// label is not rewritten. Sampling over a window matters here: a single
		// read can land between two competing writes and look stable.
		Consistently(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      "shared-target",
				Namespace: namespace,
			}, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKeyWithValue("owner", "a"),
				"the resource changed hands between instances")
			g.Expect(cm.Labels).To(HaveKeyWithValue(applyset.ApplysetPartOfLabel, firstApplySetID),
				"ApplySet membership was reassigned to a competing instance")
		}, 20*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())

		// And the contention is reported rather than hidden: the instance that
		// could not take the resource must not claim to be reconciled.
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      "owner-b",
				Namespace: namespace,
			}, second)).To(Succeed())
			state, _, err := unstructured.NestedString(second.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(state).ToNot(Equal("ACTIVE"),
				"an instance that could not take ownership of its resource reports ACTIVE; "+
					"conditions: %s", instanceConditions(second))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
