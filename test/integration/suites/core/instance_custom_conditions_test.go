// Copyright 2026 The Kube Resource Orchestrator Authors
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
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// findInstanceConditionByType walks .status.conditions[] on an unstructured instance
// and returns the first condition whose type matches, or nil if absent.
func findInstanceConditionByType(inst *unstructured.Unstructured, condType string) map[string]interface{} {
	statusConditions, found, _ := unstructured.NestedSlice(inst.Object, "status", "conditions")
	if !found {
		return nil
	}
	for _, c := range statusConditions {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == condType {
			return m
		}
	}
	return nil
}

// conditionTypes returns the set of condition.type values on an instance.
// Useful for asserting which conditions are (and aren't) present.
func conditionTypes(inst *unstructured.Unstructured) []string {
	statusConditions, found, _ := unstructured.NestedSlice(inst.Object, "status", "conditions")
	if !found {
		return nil
	}
	var out []string
	for _, c := range statusConditions {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, ok := m["type"].(string); ok {
			out = append(out, t)
		}
	}
	return out
}

var _ = Describe("Instance Custom Conditions", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-cc-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	// ---------------------------------------------------------------
	// Happy path: a static, well-formed conditions: block produces the
	// expected wire conditions on every reconcile.
	// ---------------------------------------------------------------
	It("writes author-defined conditions to status.conditions", func(ctx SpecContext) {
		rgdName := "test-cc-happy"
		instanceKind := "TestCcHappy"

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				instanceKind, "v1alpha1",
				map[string]interface{}{"name": "string"},
				nil,
			),
			generator.WithStatusConditions(
				`${runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: 'OK', message: 'always healthy in this test'})}`,
				`${runtime.newCondition({type: 'AppReady', status: 'False', reason: 'Init', message: ''})}`,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "${schema.spec.name}"},
				"data":       map[string]interface{}{"foo": "${schema.spec.name}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
			"kind":       instanceKind,
			"metadata":   map[string]interface{}{"name": "demo", "namespace": namespace},
			"spec":       map[string]interface{}{"name": "demo"},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Author conditions appear on the wire with correct fields,
		// and kro built-ins are absent.
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name: "demo", Namespace: namespace,
			}, instance)).To(Succeed())

			primary := findInstanceConditionByType(instance, "PrimaryReady")
			g.Expect(primary).ToNot(BeNil(), "PrimaryReady should be on the wire")
			g.Expect(primary["status"]).To(Equal("True"))
			g.Expect(primary["reason"]).To(Equal("OK"))
			g.Expect(primary["message"]).To(Equal("always healthy in this test"))
			g.Expect(primary["lastTransitionTime"]).ToNot(BeNil())
			g.Expect(primary["observedGeneration"]).To(Equal(instance.GetGeneration()))

			appReady := findInstanceConditionByType(instance, "AppReady")
			g.Expect(appReady).ToNot(BeNil())
			g.Expect(appReady["status"]).To(Equal("False"))
			g.Expect(appReady["reason"]).To(Equal("Init"))

			types := conditionTypes(instance)
			g.Expect(types).ToNot(ContainElement(ctrlinstance.InstanceManaged))
			g.Expect(types).ToNot(ContainElement(ctrlinstance.GraphResolved))
			g.Expect(types).ToNot(ContainElement(ctrlinstance.ResourcesReady))
			g.Expect(types).ToNot(ContainElement(ctrlinstance.Ready),
				"kro lifecycle Ready must not appear when the author did not declare a Ready condition")
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// lastTransitionTime preserved when status doesn't change between
	// reconciles. We force a reconcile by patching the spec, then
	// verify lastTransitionTime didn't advance.
	// ---------------------------------------------------------------
	It("preserves lastTransitionTime when condition status is unchanged", func(ctx SpecContext) {
		rgdName := "test-cc-ltt"
		instanceKind := "TestCcLtt"

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				instanceKind, "v1alpha1",
				map[string]interface{}{"name": "string", "label": "string | default=initial"},
				nil,
			),
			generator.WithStatusConditions(
				`${runtime.newCondition({type: 'AlwaysTrue', status: 'True', reason: 'X', message: ''})}`,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":   "${schema.spec.name}",
					"labels": map[string]interface{}{"app": "${schema.spec.label}"},
				},
				"data": map[string]interface{}{"foo": "${schema.spec.name}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
			"kind":       instanceKind,
			"metadata":   map[string]interface{}{"name": "demo", "namespace": namespace},
			"spec":       map[string]interface{}{"name": "demo", "label": "initial"},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Capture initial lastTransitionTime.
		var firstLTT interface{}
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())
			c := findInstanceConditionByType(instance, "AlwaysTrue")
			g.Expect(c).ToNot(BeNil())
			firstLTT = c["lastTransitionTime"]
			g.Expect(firstLTT).ToNot(BeNil())
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		// Bump generation by editing label
		Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())
		Expect(unstructured.SetNestedField(instance.Object, "updated", "spec", "label")).To(Succeed())
		Expect(env.Client.Update(ctx, instance)).To(Succeed())

		// Wait for ObservedGeneration to advance, then assert LTT did NOT.
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())
			c := findInstanceConditionByType(instance, "AlwaysTrue")
			g.Expect(c).ToNot(BeNil())
			g.Expect(c["observedGeneration"]).To(Equal(instance.GetGeneration()),
				"observedGeneration should track the new generation")
			g.Expect(c["lastTransitionTime"]).To(Equal(firstLTT),
				"lastTransitionTime should be preserved when status hasn't changed")
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// An RGD without a `conditions:` block keeps producing the
	// existing four kro built-in conditions.
	// ---------------------------------------------------------------
	It("emits kro built-in conditions when no conditions: block is present", func(ctx SpecContext) {
		rgdName := "test-cc-backcompat"
		instanceKind := "TestCcBackCompat"

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				instanceKind, "v1alpha1",
				map[string]interface{}{"name": "string"},
				nil,
			),
			// No WithStatusConditions call.
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "${schema.spec.name}"},
				"data":       map[string]interface{}{"foo": "${schema.spec.name}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
			"kind":       instanceKind,
			"metadata":   map[string]interface{}{"name": "demo", "namespace": namespace},
			"spec":       map[string]interface{}{"name": "demo"},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())
			ts := conditionTypes(instance)
			g.Expect(ts).To(ContainElement(ctrlinstance.Ready))
			g.Expect(ts).To(ContainElement(ctrlinstance.InstanceManaged))
			g.Expect(ts).To(ContainElement(ctrlinstance.GraphResolved))
			g.Expect(ts).To(ContainElement(ctrlinstance.ResourcesReady))

			ready := findInstanceConditionByType(instance, ctrlinstance.Ready)
			g.Expect(ready["status"]).To(Equal("True"))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// Build-time validation: invalid status literal — RGD goes Inactive.
	// ---------------------------------------------------------------
	It("rejects an RGD whose condition uses an invalid literal status", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-cc-bad-status",
			generator.WithSchema(
				"TestCcBadStatus", "v1alpha1",
				map[string]interface{}{"name": "string"},
				nil,
			),
			generator.WithStatusConditions(
				// 'YES' is not in {True, False, Unknown}.
				`${runtime.newCondition({type: 'X', status: 'YES', reason: 'R', message: 'M'})}`,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "${schema.spec.name}"},
				"data":       map[string]interface{}{"foo": "${schema.spec.name}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))

			var ready *krov1alpha1.Condition
			for _, c := range rgd.Status.Conditions {
				if c.Type == ctrlinstance.Ready {
					ready = &c
					break
				}
			}
			g.Expect(ready).ToNot(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// Build-time validation: unknown literal key — RGD goes Inactive.
	// ---------------------------------------------------------------
	It("rejects an RGD whose condition uses an unknown literal key", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-cc-bad-key",
			generator.WithSchema(
				"TestCcBadKey", "v1alpha1",
				map[string]interface{}{"name": "string"},
				nil,
			),
			generator.WithStatusConditions(
				// 'extra' is not an allowed key.
				`${runtime.newCondition({type: 'X', status: 'True', reason: 'R', message: 'M', "extra": 'foo'})}`,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "${schema.spec.name}"},
				"data":       map[string]interface{}{"foo": "${schema.spec.name}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))
		}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// Build-time validation: self-reference between custom conditions.
	// PrimaryReady is defined; a second condition tries to read it via
	// runtime.condition(schema, 'PrimaryReady') — illegal.
	// ---------------------------------------------------------------
	It("rejects an RGD whose conditions reference each other", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-cc-self-ref",
			generator.WithSchema(
				"TestCcSelfRef", "v1alpha1",
				map[string]interface{}{"name": "string"},
				nil,
			),
			generator.WithStatusConditions(
				`${runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: '', message: ''})}`,
				`${runtime.newCondition({type: 'Ready', status: runtime.condition(schema, 'PrimaryReady').status, reason: '', message: ''})}`,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "${schema.spec.name}"},
				"data":       map[string]interface{}{"foo": "${schema.spec.name}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))
		}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// Author Ready overrides kro's lifecycle Ready. The author writes
	// 'False' verbatim; even though all built-ins are True, the wire
	// shows the author's value.
	// ---------------------------------------------------------------
	It("honors an author-defined Ready that overrides kro's lifecycle Ready", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-cc-author-ready",
			generator.WithSchema(
				"TestCcAuthorReady", "v1alpha1",
				map[string]interface{}{"name": "string"},
				nil,
			),
			generator.WithStatusConditions(
				// Hardcoded False. kro's lifecycle Ready would be True
				// for this trivial RGD, but the author's value wins.
				`${runtime.newCondition({type: 'Ready', status: 'False', reason: 'AuthorSaysNo', message: 'this is a test'})}`,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "${schema.spec.name}"},
				"data":       map[string]interface{}{"foo": "${schema.spec.name}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
			"kind":       "TestCcAuthorReady",
			"metadata":   map[string]interface{}{"name": "demo", "namespace": namespace},
			"spec":       map[string]interface{}{"name": "demo"},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())
			ready := findInstanceConditionByType(instance, ctrlinstance.Ready)
			g.Expect(ready).ToNot(BeNil())
			g.Expect(ready["status"]).To(Equal("False"),
				"author Ready=False must win, even when kro lifecycle would otherwise say True")
			g.Expect(ready["reason"]).To(Equal("AuthorSaysNo"))
			g.Expect(ready["message"]).To(Equal("this is a test"))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// Conditions can read schema.spec fields. When a spec field changes,
	// the condition's evaluated status follows.
	// ---------------------------------------------------------------
	It("evaluates conditions against schema.spec fields and updates them when spec changes", func(ctx SpecContext) {
		rgdName := "test-cc-schema-read"
		instanceKind := "TestCcSchemaRead"

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				instanceKind, "v1alpha1",
				map[string]interface{}{
					"name":    "string",
					"healthy": "boolean | default=true",
				},
				nil,
			),
			generator.WithStatusConditions(
				`${runtime.newCondition({type: 'AppReady', status: schema.spec.healthy ? 'True' : 'False', reason: 'CheckedSpec', message: ''})}`,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "${schema.spec.name}"},
				"data":       map[string]interface{}{"foo": "${schema.spec.name}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
			"kind":       instanceKind,
			"metadata":   map[string]interface{}{"name": "demo", "namespace": namespace},
			"spec":       map[string]interface{}{"name": "demo", "healthy": true},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Initially True.
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())
			c := findInstanceConditionByType(instance, "AppReady")
			g.Expect(c).ToNot(BeNil())
			g.Expect(c["status"]).To(Equal("True"))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		// Flip spec.healthy to false; condition should follow.
		Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())
		Expect(unstructured.SetNestedField(instance.Object, false, "spec", "healthy")).To(Succeed())
		Expect(env.Client.Update(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())
			c := findInstanceConditionByType(instance, "AppReady")
			g.Expect(c).ToNot(BeNil())
			g.Expect(c["status"]).To(Equal("False"),
				"condition status should follow spec.healthy")
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// runtime.condition(schema, 'ResourcesReady') must return
	// kro's INTERNAL value, even when the author has overridden
	// 'ResourcesReady' on the wire with their own value. This
	// proves the four reserved condition types are insulated:
	// what the author writes to .status.conditions[] does NOT
	// shadow the value kro hands to runtime.condition.
	// ---------------------------------------------------------------
	It("runtime.condition reads kro's internal value, not the author's wire override", func(ctx SpecContext) {
		rgdName := "test-cc-internal-not-wire"
		instanceKind := "TestCcInternalNotWire"

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				instanceKind, "v1alpha1",
				map[string]interface{}{"name": "string"},
				nil,
			),
			generator.WithStatusConditions(
				// Author overrides ResourcesReady: claims 'True' with
				// their own reason. This puts {ResourcesReady, True} on the
				// wire, regardless of what kro thinks.
				`${runtime.newCondition({
					type: 'ResourcesReady',
					status: 'True',
					reason: 'AuthorOverride',
					message: 'author claims ready',
				})}`,
				// DerivedReady reads kro's internal ResourcesReady. If
				// the lookup is correctly insulated, it sees False (kro's
				// value, because the configmap's readyWhen never passes).
				// If the implementation regressed to reading the wire, it
				// would see True (the author's override) and this test
				// would fail.
				`${runtime.newCondition({
					type: 'DerivedReady',
					status: runtime.condition(schema, 'ResourcesReady').status,
					reason: 'MirrorsKroInternal',
					message: '',
				})}`,
			),
			// readyWhen references a data field that's never populated, so
			// the configmap stays in WaitingForReadiness and kro's internal
			// ResourcesReady stays False for the duration of the test.
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "${schema.spec.name}"},
				"data":       map[string]interface{}{"foo": "${schema.spec.name}"},
			}, []string{`configmap.data["ready"] == "true"`}, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
			"kind":       instanceKind,
			"metadata":   map[string]interface{}{"name": "demo", "namespace": namespace},
			"spec":       map[string]interface{}{"name": "demo"},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Author's wire override and kro's internal value disagree:
		//   wire:     ResourcesReady=True  (author override)
		//   internal: ResourcesReady=False (configmap.readyWhen unsatisfied)
		// DerivedReady must reflect kro's internal False, proving the lookup
		// reads kroBuiltins rather than the wire.
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())

			rr := findInstanceConditionByType(instance, ctrlinstance.ResourcesReady)
			g.Expect(rr).ToNot(BeNil(), "author's ResourcesReady override should appear on the wire")
			g.Expect(rr["status"]).To(Equal("True"),
				"author's declaration should appear unchanged on the wire")
			g.Expect(rr["reason"]).To(Equal("AuthorOverride"))

			derived := findInstanceConditionByType(instance, "DerivedReady")
			g.Expect(derived).ToNot(BeNil())
			g.Expect(derived["status"]).To(Equal("False"),
				"runtime.condition(schema, 'ResourcesReady') must reflect kro's internal value (False), "+
					"not the author's overriding 'True' on the wire")
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// Duplicate condition types are dropped and the reconcile
	// surfaces state=Error on the wire. Surviving conditions (those
	// without a type collision) are persisted normally — analogous to
	// SSA replacing the conditions slice and the duplicated type
	// simply not appearing.
	// ---------------------------------------------------------------
	It("drops duplicate condition types, sets state=Error, keeps survivors", func(ctx SpecContext) {
		rgdName := "test-cc-dup-type"
		instanceKind := "TestCcDupType"

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				instanceKind, "v1alpha1",
				map[string]interface{}{"name": "string"},
				nil,
			),
			generator.WithStatusConditions(
				`${runtime.newCondition({type: 'Survivor', status: 'True', reason: '', message: ''})}`,
				`${runtime.newCondition({type: 'Same', status: 'True', reason: '', message: ''})}`,
				`${runtime.newCondition({type: 'Same', status: 'False', reason: '', message: ''})}`,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "${schema.spec.name}"},
				"data":       map[string]interface{}{"foo": "${schema.spec.name}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
			"kind":       instanceKind,
			"metadata":   map[string]interface{}{"name": "demo", "namespace": namespace},
			"spec":       map[string]interface{}{"name": "demo"},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "demo", Namespace: namespace}, instance)).To(Succeed())

			state, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(state).To(Equal(string(krov1alpha1.InstanceStateError)),
				"duplicate condition type must surface as state=Error")

			g.Expect(findInstanceConditionByType(instance, "Survivor")).ToNot(BeNil(),
				"Survivor must remain on the wire")
			g.Expect(findInstanceConditionByType(instance, "Same")).To(BeNil(),
				"both copies of the duplicated 'Same' type must be dropped")
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
	})
})
