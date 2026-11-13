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

package versioning_test

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// configMapRGD builds an RGD whose only resource is a ConfigMap carrying the
// given extra data keys, so an edit to those keys issues a new graph revision
// with an observable effect.
func configMapRGD(name, kind string, extra map[string]any) *krov1alpha1.ResourceGraphDefinition {
	data := map[string]any{"value": "${schema.spec.value}"}
	for k, v := range extra {
		data[k] = v
	}
	return generator.NewResourceGraphDefinition(name,
		generator.WithSchema(
			kind, "v1alpha1",
			map[string]any{"value": "string | default=\"hello\""},
			map[string]any{"configMapName": "${cm.metadata.name}"},
		),
		generator.WithResource("cm", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-cm",
				"namespace": "${schema.metadata.namespace}",
			},
			"data": data,
		}, nil, nil),
	)
}

func newInstance(kind, namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kro.run/v1alpha1",
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{"value": "hello"},
	}}
}

// instanceGraphRevision reads status.graphRevision off a live instance.
func instanceGraphRevision(g Gomega, ctx SpecContext, inst *unstructured.Unstructured) int64 {
	fetched := inst.DeepCopy()
	g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(inst), fetched)).To(Succeed())
	revision, found, err := unstructured.NestedInt64(fetched.Object, "status", "graphRevision")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue(), "instance %s has no status.graphRevision", inst.GetName())
	return revision
}

// waitForRGDActive waits until the RGD is serving the spec generation that is
// currently on the wire, so a spec change is never observed as "already
// Active" from the previous generation's status.
func waitForRGDActive(ctx SpecContext, rgd *krov1alpha1.ResourceGraphDefinition) {
	GinkgoHelper()
	Eventually(func(g Gomega, ctx SpecContext) {
		g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
		g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive),
			"conditions: %s", conditionsSummary(rgd))
		for _, condition := range rgd.Status.Conditions {
			if condition.Type == krov1alpha1.InstanceConditionTypeReady {
				g.Expect(condition.ObservedGeneration).To(Equal(rgd.Generation),
					"status is still reporting generation %d", condition.ObservedGeneration)
			}
		}
	}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
}

// conditionsSummary renders an RGD's conditions for assertion messages.
func conditionsSummary(rgd *krov1alpha1.ResourceGraphDefinition) string {
	parts := make([]string, 0, len(rgd.Status.Conditions))
	for _, condition := range rgd.Status.Conditions {
		reason, message := "", ""
		if condition.Reason != nil {
			reason = *condition.Reason
		}
		if condition.Message != nil {
			message = *condition.Message
		}
		parts = append(parts, fmt.Sprintf("%s=%s(%s: %s)", condition.Type, condition.Status, reason, message))
	}
	return strings.Join(parts, ", ")
}

var _ = Describe("Rollout", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	It("keeps Manual instances on their revision until the pin moves them", func(ctx SpecContext) {
		kind := "ManualApp"
		rgd := configMapRGD("test-manual-"+rand.String(5), kind, nil)
		rgd.Spec.Rollout = &krov1alpha1.Rollout{Strategy: krov1alpha1.RolloutStrategyManual}
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, rgd))).To(Succeed())
		})
		waitForRGDActive(ctx, rgd)

		first := newInstance(kind, namespace, "first")
		Expect(env.Client.Create(ctx, first)).To(Succeed())

		cm := &corev1.ConfigMap{}
		cmKey := types.NamespacedName{Namespace: namespace, Name: "first-cm"}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, cmKey, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKeyWithValue("value", "hello"))
			g.Expect(instanceGraphRevision(g, ctx, first)).To(Equal(int64(1)))
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Issue revision 2 by changing the rendered ConfigMap.
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			rgd.Spec.Resources[0].Template = toRawExtension(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "${schema.metadata.name}-cm",
					"namespace": "${schema.metadata.namespace}",
				},
				"data": map[string]any{
					"value": "${schema.spec.value}",
					"extra": "from-revision-2",
				},
			})
			g.Expect(env.Client.Update(ctx, rgd)).To(Succeed())
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
		waitForRGDActive(ctx, rgd)

		// A fresh instance adopts the latest revision...
		second := newInstance(kind, namespace, "second")
		Expect(env.Client.Create(ctx, second)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			secondCM := &corev1.ConfigMap{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "second-cm"}, secondCM)).To(Succeed())
			g.Expect(secondCM.Data).To(HaveKeyWithValue("extra", "from-revision-2"))
			g.Expect(instanceGraphRevision(g, ctx, second)).To(Equal(int64(2)))
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// ...while the existing one stays on revision 1 with the old data.
		Consistently(func(g Gomega, ctx SpecContext) {
			g.Expect(instanceGraphRevision(g, ctx, first)).To(Equal(int64(1)))
			g.Expect(env.Client.Get(ctx, cmKey, cm)).To(Succeed())
			g.Expect(cm.Data).NotTo(HaveKey("extra"))
		}, 10*time.Second, 1*time.Second).WithContext(ctx).Should(Succeed())

		// Pinning it to revision 2 moves it.
		Eventually(func(g Gomega, ctx SpecContext) {
			live := first.DeepCopy()
			g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(first), live)).To(Succeed())
			annotations := live.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[krov1alpha1.InstanceGraphRevisionAnnotation] = "2"
			live.SetAnnotations(annotations)
			g.Expect(env.Client.Update(ctx, live)).To(Succeed())
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(instanceGraphRevision(g, ctx, first)).To(Equal(int64(2)))
			g.Expect(env.Client.Get(ctx, cmKey, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKeyWithValue("extra", "from-revision-2"))
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
	})

	It("retains a graph revision an instance is pinned to", func(ctx SpecContext) {
		kind := "PinnedApp"
		rgd := configMapRGD("test-retention-"+rand.String(5), kind, nil)
		rgd.Spec.Rollout = &krov1alpha1.Rollout{Strategy: krov1alpha1.RolloutStrategyManual}
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, rgd))).To(Succeed())
		})
		waitForRGDActive(ctx, rgd)

		pinned := newInstance(kind, namespace, "pinned")
		pinned.SetAnnotations(map[string]string{krov1alpha1.InstanceGraphRevisionAnnotation: "1"})
		Expect(env.Client.Create(ctx, pinned)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(instanceGraphRevision(g, ctx, pinned)).To(Equal(int64(1)))
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Four more revisions, well past MaxGraphRevisions.
		for i := 2; i <= 5; i++ {
			value := fmt.Sprintf("edit-%d", i)
			Eventually(func(g Gomega, ctx SpecContext) {
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
				rgd.Spec.Resources[0].Template = toRawExtension(map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]any{
						"name":      "${schema.metadata.name}-cm",
						"namespace": "${schema.metadata.namespace}",
					},
					"data": map[string]any{
						"value": "${schema.spec.value}",
						"extra": value,
					},
				})
				g.Expect(env.Client.Update(ctx, rgd)).To(Succeed())
			}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
			waitForRGDActive(ctx, rgd)
		}

		revisionOne := graphRevisionName(rgd.Name, 1)
		Consistently(func(g Gomega, ctx SpecContext) {
			gr := &internalv1alpha1.GraphRevision{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: revisionOne}, gr)).To(Succeed())
			g.Expect(gr.DeletionTimestamp).To(BeNil())
		}, 15*time.Second, 1*time.Second).WithContext(ctx).Should(Succeed())

		// Unpinning releases the revision; the next RGD edit runs GC again.
		Eventually(func(g Gomega, ctx SpecContext) {
			live := pinned.DeepCopy()
			g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pinned), live)).To(Succeed())
			live.SetAnnotations(map[string]string{
				krov1alpha1.InstanceGraphRevisionAnnotation: krov1alpha1.GraphRevisionLatest,
			})
			g.Expect(env.Client.Update(ctx, live)).To(Succeed())
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(instanceGraphRevision(g, ctx, pinned)).To(BeNumerically(">", int64(1)))
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			rgd.Spec.Resources[0].Template = toRawExtension(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "${schema.metadata.name}-cm",
					"namespace": "${schema.metadata.namespace}",
				},
				"data": map[string]any{
					"value": "${schema.spec.value}",
					"extra": "final",
				},
			})
			g.Expect(env.Client.Update(ctx, rgd)).To(Succeed())
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
		waitForRGDActive(ctx, rgd)

		Eventually(func(g Gomega, ctx SpecContext) {
			gr := &internalv1alpha1.GraphRevision{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: revisionOne}, gr)
			g.Expect(apierrors.IsNotFound(err) || gr.DeletionTimestamp != nil).To(BeTrue(),
				"graph revision 1 should be collected once nothing references it")
		}, 60*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())
	})
})

// graphRevisionName mirrors the controller's deterministic naming for the
// common (non-truncated) case.
func graphRevisionName(rgdName string, revision int64) string {
	return fmt.Sprintf("%s-r%05d", rgdName, revision)
}
