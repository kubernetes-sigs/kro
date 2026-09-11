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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = DescribeTable("IncludeWhen readiness",
	func(ctx SpecContext, kind, predicate string, disableWhileUnready bool) {
		namespace := "test-inclusion-readiness-" + rand.String(5)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, ns))).To(Succeed())
		})

		rgd := generator.NewResourceGraphDefinition(namespace,
			generator.WithSchema(kind, "v1alpha1", map[string]any{
				"name": "string", "enabled": "boolean", "extra": "boolean",
			}, nil), // No author status: inclusion must not depend on placeholder projection.
			generator.WithResource("db", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${schema.spec.name}-db"},
				// The test populates phase externally; kro owns only owner.
				"data": map[string]any{"owner": "${schema.spec.name}"},
			}, []string{`${db.data.phase == "Running"}`}, nil),
			generator.WithResource("app", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${schema.spec.name}-app"},
				"data":     map[string]any{"dbOwner": "${db.data.owner}"},
			}, nil, []string{predicate}),
			generator.WithResource("extra", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${schema.spec.name}-extra"},
			}, nil, []string{"${schema.spec.extra}"}),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, rgd))).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		const name = "inclusion"
		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": krov1alpha1.KRODomainName + "/v1alpha1",
			"kind":       kind,
			"metadata":   map[string]any{"name": name, "namespace": namespace},
			"spec":       map[string]any{"name": name, "enabled": true, "extra": true},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, instance))).To(Succeed())
			waitForResourceDeleted(ctx, namespace, name, instance)
		})

		instanceKey := client.ObjectKeyFromObject(instance)
		dbKey := types.NamespacedName{Namespace: namespace, Name: name + "-db"}
		appKey := types.NamespacedName{Namespace: namespace, Name: name + "-app"}
		extraKey := types.NamespacedName{Namespace: namespace, Name: name + "-extra"}
		patchPhase := func(phase string) {
			db := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: dbKey.Name, Namespace: namespace}}
			patch := client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"data":{"phase":%q}}`, phase)))
			Expect(env.Client.Patch(ctx, db, patch)).To(Succeed())
		}
		waitForState := func(state, ready string) {
			Eventually(func(g Gomega) {
				g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
				got, _, err := unstructured.NestedString(instance.Object, "status", "state")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).To(Equal(state))
				condition := findInstanceConditionByType(instance, "ResourcesReady")
				g.Expect(condition).To(HaveKeyWithValue("status", ready))
				g.Expect(condition).To(HaveKeyWithValue("observedGeneration", instance.GetGeneration()))
			}, 30*time.Second, 250*time.Millisecond).Should(Succeed())
		}

		By("establishing the child with db ready")
		Eventually(func() error {
			return env.Client.Get(ctx, dbKey, &corev1.ConfigMap{})
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())
		patchPhase("Running")
		waitForState("ACTIVE", "True")
		original := &corev1.ConfigMap{}
		Expect(env.Client.Get(ctx, appKey, original)).To(Succeed())
		Expect(original.UID).NotTo(BeEmpty())
		Expect(original.Data).To(Equal(map[string]string{"dbOwner": name}))
		extra := &corev1.ConfigMap{}
		Expect(env.Client.Get(ctx, extraKey, extra)).To(Succeed())

		By("observing the dependency's interim false value before checking retention")
		patchPhase("Starting")
		waitForState("IN_PROGRESS", "False")
		if disableWhileUnready {
			By("disabling app and an unrelated resource while the template dependency is unready")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"enabled":false,"extra":false}}`))
			Expect(env.Client.Patch(ctx, instance, patch)).To(Succeed())
			waitForState("IN_PROGRESS", "False")
		}
		Consistently(func(g Gomega) {
			app := &corev1.ConfigMap{}
			g.Expect(env.Client.Get(ctx, appKey, app)).To(Succeed(),
				"app must survive an applied-but-not-ready dependency")
			g.Expect(app.UID).To(Equal(original.UID))
			g.Expect(app.Data).To(Equal(original.Data))
			if disableWhileUnready {
				retired := &corev1.ConfigMap{}
				g.Expect(env.Client.Get(ctx, extraKey, retired)).To(Succeed(),
					"the skipped extra must remain while app is withheld")
				g.Expect(retired.UID).To(Equal(extra.UID))
			}
		}, 10*time.Second, 250*time.Millisecond).Should(Succeed())

		By("evaluating inclusion once the dependency is ready again")
		patchPhase("Running")
		waitForState("ACTIVE", "True")
		if disableWhileUnready {
			waitForResourceDeleted(ctx, namespace, appKey.Name, &corev1.ConfigMap{})
			waitForResourceDeleted(ctx, namespace, extraKey.Name, &corev1.ConfigMap{})
		} else {
			app := &corev1.ConfigMap{}
			Expect(env.Client.Get(ctx, appKey, app)).To(Succeed())
			Expect(app.UID).To(Equal(original.UID))
			Expect(app.Data).To(Equal(original.Data))
		}
	},
	Entry("retains an existing child through a false interim resource value",
		"InclusionReadyInputs", `${schema.spec.name != "" && db.data.phase == "Running"}`, false, SpecTimeout(120*time.Second)),
	Entry("retains schema-disabled children and a skipped extra until template inputs are ready",
		"InclusionSchemaDisabled", `${schema.spec.enabled}`, true, SpecTimeout(120*time.Second)),
)
