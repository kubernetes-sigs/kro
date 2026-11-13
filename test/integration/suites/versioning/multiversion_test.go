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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// hubSpec is the instance spec of the hub version used by these specs.
var hubSpec = map[string]any{"value": "string | default=\"hello\""}

func multiVersionRGD(name, kind string) *krov1alpha1.ResourceGraphDefinition {
	return generator.NewResourceGraphDefinition(name,
		generator.WithSchema(
			kind, "v1alpha1",
			hubSpec,
			map[string]any{"configMapName": "${cm.metadata.name}"},
		),
		generator.WithResource("cm", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-cm",
				"namespace": "${schema.metadata.namespace}",
			},
			"data": map[string]any{"value": "${schema.spec.value}"},
		}, nil, nil),
	)
}

// updateRGD applies mutate to the live RGD, retrying on conflict.
func updateRGD(ctx SpecContext, name string, mutate func(*krov1alpha1.ResourceGraphDefinition)) {
	GinkgoHelper()
	Eventually(func(g Gomega, ctx SpecContext) {
		rgd := &krov1alpha1.ResourceGraphDefinition{}
		g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: name}, rgd)).To(Succeed())
		mutate(rgd)
		g.Expect(env.Client.Update(ctx, rgd)).To(Succeed())
	}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
}

// expectKindUnready waits for the RGD's KindReady condition to go false with
// the given reason, and returns the condition message.
func expectKindUnready(ctx SpecContext, name, reason string) string {
	GinkgoHelper()
	var message string
	Eventually(func(g Gomega, ctx SpecContext) {
		rgd := &krov1alpha1.ResourceGraphDefinition{}
		g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: name}, rgd)).To(Succeed())
		var found bool
		for _, condition := range rgd.Status.Conditions {
			if condition.Type != krov1alpha1.RGDConditionTypeKindReady {
				continue
			}
			found = true
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(condition.Reason).NotTo(BeNil())
			g.Expect(*condition.Reason).To(Equal(reason))
			if condition.Message != nil {
				message = *condition.Message
			}
		}
		g.Expect(found).To(BeTrue(), "KindReady condition not present")
	}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
	return message
}

// instanceCRD reads the generated CRD for a kind. The isolating client maps
// the per-process group back to kro.run on the way out.
func instanceCRD(g Gomega, ctx SpecContext, kind string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	name := strings.ToLower(kind) + "s.kro.run"
	g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: name}, crd)).To(Succeed())
	return crd
}

func storageVersionOf(crd *apiextensionsv1.CustomResourceDefinition) string {
	for _, version := range crd.Spec.Versions {
		if version.Storage {
			return version.Name
		}
	}
	return ""
}

func servedVersionsOf(crd *apiextensionsv1.CustomResourceDefinition) []string {
	var served []string
	for _, version := range crd.Spec.Versions {
		if version.Served {
			served = append(served, version.Name)
		}
	}
	return served
}

var _ = Describe("MultiVersion", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	It("moves the hub forward, keeps serving the previous version and migrates storage", func(ctx SpecContext) {
		kind := "HubMoveApp"
		rgd := multiVersionRGD("test-hub-move-"+rand.String(5), kind)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, rgd))).To(Succeed())
		})
		waitForRGDActive(ctx, rgd)

		instance := newInstance(kind, namespace, "legacy")
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "legacy-cm"}, cm)).To(Succeed())
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Move the hub to v1beta1, keeping v1alpha1 as a served spoke.
		updateRGD(ctx, rgd.Name, func(live *krov1alpha1.ResourceGraphDefinition) {
			live.Spec.Schema.APIVersion = "v1beta1"
			live.Spec.Schema.PreviousVersions = []krov1alpha1.PreviousSchemaVersion{{
				APIVersion: "v1alpha1",
				Spec:       toRawExtension(hubSpec),
			}}
		})
		waitForRGDActive(ctx, rgd)

		Eventually(func(g Gomega, ctx SpecContext) {
			crd := instanceCRD(g, ctx, kind)
			g.Expect(storageVersionOf(crd)).To(Equal("v1beta1"))
			g.Expect(servedVersionsOf(crd)).To(ConsistOf("v1beta1", "v1alpha1"))
			g.Expect(crd.Spec.Conversion).NotTo(BeNil())
			g.Expect(crd.Spec.Conversion.Strategy).To(Equal(apiextensionsv1.NoneConverter))
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// The object created as v1alpha1 is readable as v1beta1 and keeps
		// reconciling under the new hub.
		Eventually(func(g Gomega, ctx SpecContext) {
			beta := &unstructured.Unstructured{}
			beta.SetGroupVersionKind(instance.GroupVersionKind())
			beta.SetAPIVersion("kro.run/v1beta1")
			beta.SetKind(kind)
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "legacy"}, beta)).To(Succeed())
			value, found, err := unstructured.NestedString(beta.Object, "spec", "value")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(value).To(Equal("hello"))
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Storage migration trims status.storedVersions to the hub alone.
		Eventually(func(g Gomega, ctx SpecContext) {
			crd := instanceCRD(g, ctx, kind)
			g.Expect(crd.Status.StoredVersions).To(Equal([]string{"v1beta1"}))
		}, 90*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Once nothing is stored under v1alpha1, it can be dropped.
		updateRGD(ctx, rgd.Name, func(live *krov1alpha1.ResourceGraphDefinition) {
			live.Spec.Schema.PreviousVersions = nil
		})
		Eventually(func(g Gomega, ctx SpecContext) {
			crd := instanceCRD(g, ctx, kind)
			g.Expect(servedVersionsOf(crd)).To(ConsistOf("v1beta1"))
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
	})

	It("rejects dropping the stored version in the same edit that moves the hub", func(ctx SpecContext) {
		kind := "DropHubApp"
		rgd := multiVersionRGD("test-drop-hub-"+rand.String(5), kind)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, rgd))).To(Succeed())
		})
		waitForRGDActive(ctx, rgd)

		updateRGD(ctx, rgd.Name, func(live *krov1alpha1.ResourceGraphDefinition) {
			live.Spec.Schema.APIVersion = "v1beta1"
		})

		message := expectKindUnready(ctx, rgd.Name, "VersionRemovalBlocked")
		Expect(message).To(ContainSubstring("v1alpha1"))

		Consistently(func(g Gomega, ctx SpecContext) {
			crd := instanceCRD(g, ctx, kind)
			g.Expect(storageVersionOf(crd)).To(Equal("v1alpha1"))
		}, 5*time.Second, 1*time.Second).WithContext(ctx).Should(Succeed())

		// Declaring the old hub as a previous version recovers the RGD.
		updateRGD(ctx, rgd.Name, func(live *krov1alpha1.ResourceGraphDefinition) {
			live.Spec.Schema.PreviousVersions = []krov1alpha1.PreviousSchemaVersion{{
				APIVersion: "v1alpha1",
				Spec:       toRawExtension(hubSpec),
			}}
		})
		waitForRGDActive(ctx, rgd)
		Eventually(func(g Gomega, ctx SpecContext) {
			crd := instanceCRD(g, ctx, kind)
			g.Expect(storageVersionOf(crd)).To(Equal("v1beta1"))
		}, 60*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
	})

	It("rejects a previous version that cannot serve what the hub stores", func(ctx SpecContext) {
		kind := "SpokeDriftApp"
		rgd := generator.NewResourceGraphDefinition("test-spoke-"+rand.String(5),
			generator.WithSchema(
				kind, "v1beta1",
				map[string]any{
					"value": "string | default=\"hello\"",
					"extra": "string | default=\"present\"",
				},
				map[string]any{"configMapName": "${cm.metadata.name}"},
			),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "${schema.metadata.name}-cm",
					"namespace": "${schema.metadata.namespace}",
				},
				"data": map[string]any{"value": "${schema.spec.value}"},
			}, nil, nil),
		)
		rgd.Spec.Schema.PreviousVersions = []krov1alpha1.PreviousSchemaVersion{{
			APIVersion: "v1alpha1",
			Spec: toRawExtension(map[string]any{
				"value": "string | default=\"hello\"",
				"extra": "string | default=\"present\"",
			}),
		}}
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, rgd))).To(Succeed())
		})
		waitForRGDActive(ctx, rgd)

		// Drop a field from the spoke that the hub still serves.
		updateRGD(ctx, rgd.Name, func(live *krov1alpha1.ResourceGraphDefinition) {
			live.Spec.Schema.PreviousVersions = []krov1alpha1.PreviousSchemaVersion{{
				APIVersion: "v1alpha1",
				Spec:       toRawExtension(map[string]any{"value": "string | default=\"hello\""}),
			}}
		})

		message := expectKindUnready(ctx, rgd.Name, "SpokeSchemaMismatch")
		Expect(message).To(ContainSubstring("v1alpha1"))
	})

	It("rejects moving the hub backwards", func(ctx SpecContext) {
		kind := "BackwardsApp"
		rgd := generator.NewResourceGraphDefinition("test-backwards-"+rand.String(5),
			generator.WithSchema(
				kind, "v1beta1",
				hubSpec,
				map[string]any{"configMapName": "${cm.metadata.name}"},
			),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "${schema.metadata.name}-cm",
					"namespace": "${schema.metadata.namespace}",
				},
				"data": map[string]any{"value": "${schema.spec.value}"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, rgd))).To(Succeed())
		})
		waitForRGDActive(ctx, rgd)

		updateRGD(ctx, rgd.Name, func(live *krov1alpha1.ResourceGraphDefinition) {
			live.Spec.Schema.APIVersion = "v1alpha1"
			live.Spec.Schema.PreviousVersions = nil
		})

		message := expectKindUnready(ctx, rgd.Name, "Failed")
		Expect(message).To(ContainSubstring("may only move forward"))
	})
})
