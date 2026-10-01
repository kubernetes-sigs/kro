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

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	applysetspec "github.com/kubernetes-sigs/kro/pkg/applyset"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// deletionPolicy: Detach is the promise that kro will not delete a resource it
// would otherwise remove. The failure mode is destructive and unrecoverable:
// the object the author asked to keep is gone. These specs drive the two paths
// that can remove a resource (instance teardown and prune) end to end, because
// both rebuild their candidate set from the cluster rather than from the graph.
var _ = Describe("DeletionPolicy", func() {
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

	It("releases a Detach resource and deletes the rest when the instance is deleted", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-deletion-policy-teardown",
			generator.WithSchema(
				"TestDeletionPolicyTeardown", "v1alpha1",
				map[string]any{"name": "string"},
				nil,
			),
			generator.WithResource("retained", configMapTemplate("${schema.spec.name}-retained"), nil, nil),
			generator.WithResource("removed", configMapTemplate("${schema.spec.name}-removed"), nil, nil),
			generator.WithResourceDeletionPolicy("retained", krov1alpha1.DeletionPolicyDetach),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "teardown"
		instance := newInstance("TestDeletionPolicyTeardown", name, namespace, map[string]any{"name": name})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")

		// The policy has to be on the object, since that is where teardown
		// reads it from.
		retained := waitForConfigMap(ctx, namespace, name+"-retained")
		Expect(retained.Annotations).To(HaveKeyWithValue(
			metadata.DeletionPolicyAnnotation, string(krov1alpha1.DeletionPolicyDetach)))
		waitForConfigMap(ctx, namespace, name+"-removed")

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// The instance must finish deleting: a retained resource left in the
		// candidate set would hold its finalizer forever.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace},
				&unstructured.Unstructured{Object: map[string]any{
					"apiVersion": krov1alpha1.KRODomainName + "/v1alpha1",
					"kind":       "TestDeletionPolicyTeardown",
				}})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "instance still present (err=%v)", err)
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: name + "-removed", Namespace: namespace,
			}, &corev1.ConfigMap{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Delete-policy resource survived (err=%v)", err)
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		released := &corev1.ConfigMap{}
		Expect(env.Client.Get(ctx, types.NamespacedName{
			Name: name + "-retained", Namespace: namespace,
		}, released)).To(Succeed(), "Detach-policy resource must survive its instance")
		expectReleased(released.ObjectMeta)
	})

	It("releases a Detach resource that leaves the desired set", func(ctx SpecContext) {
		// Prune, not teardown: the resource's includeWhen turns false, so the
		// graph stops describing it while the instance lives on.
		rgd := generator.NewResourceGraphDefinition("test-deletion-policy-prune",
			generator.WithSchema(
				"TestDeletionPolicyPrune", "v1alpha1",
				map[string]any{
					"name":    "string",
					"enabled": "boolean | default=true",
				},
				nil,
			),
			generator.WithResource("retained", configMapTemplate("${schema.spec.name}-retained"),
				nil, []string{"${schema.spec.enabled}"}),
			generator.WithResourceDeletionPolicy("retained", krov1alpha1.DeletionPolicyDetach),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "prune"
		instance := newInstance("TestDeletionPolicyPrune", name, namespace, map[string]any{
			"name":    name,
			"enabled": true,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")
		waitForConfigMap(ctx, namespace, name+"-retained")

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name: name, Namespace: namespace,
			}, instance)).To(Succeed())
			g.Expect(unstructured.SetNestedField(instance.Object, false, "spec", "enabled")).To(Succeed())
			g.Expect(env.Client.Update(ctx, instance)).To(Succeed())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// The resource stays, released rather than pruned, and stops coming back
		// as a prune candidate.
		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name: name + "-retained", Namespace: namespace,
			}, cm)).To(Succeed())
			g.Expect(cm.Labels).ToNot(HaveKey(applysetspec.ApplysetPartOfLabel),
				"prune must release the resource, instance conditions: %s", instanceConditions(instance))
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		released := &corev1.ConfigMap{}
		Consistently(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name: name + "-retained", Namespace: namespace,
			}, released)).To(Succeed())
		}, 10*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())
		expectReleased(released.ObjectMeta)
	})
})

// The API server rejects the field where it has no meaning, so an author who
// puts it on an externalRef finds out on apply rather than discovering months
// later that the policy was silently ignored.
var _ = Describe("DeletionPolicyValidation", func() {
	It("rejects deletionPolicy on an externalRef", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-deletion-policy-externalref",
			generator.WithSchema(
				"TestDeletionPolicyExternalRef", "v1alpha1",
				map[string]any{"name": "string"},
				nil,
			),
			generator.WithExternalRef("existing", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata:   krov1alpha1.ExternalRefMetadata{Name: "some-cm"},
			}, nil, nil),
			generator.WithResourceDeletionPolicy("existing", krov1alpha1.DeletionPolicyDetach),
		)

		err := env.Client.Create(ctx, rgd)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("deletionPolicy is only supported on template resources"))
	})
})

func configMapTemplate(name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":   name,
			"labels": map[string]any{"app": "deletion-policy"},
		},
		"data": map[string]any{"key": "value"},
	}
}

func waitForConfigMap(ctx SpecContext, namespace, name string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{}
	Eventually(func(g Gomega, ctx SpecContext) {
		g.Expect(env.Client.Get(ctx, types.NamespacedName{
			Name: name, Namespace: namespace,
		}, cm)).To(Succeed())
	}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	return cm
}

// expectReleased asserts that nothing of kro's is left on the object, which is
// what takes it out of the ApplySet and makes it adoptable again, while the
// author's own metadata is untouched.
func expectReleased(meta metav1.ObjectMeta) {
	Expect(meta.Labels).To(HaveKeyWithValue("app", "deletion-policy"))
	for key := range meta.Labels {
		Expect(isKROMetadataKey(key, meta.Labels[key])).To(BeFalse(), "label %q was not released", key)
	}
	for key := range meta.Annotations {
		Expect(isKROMetadataKey(key, meta.Annotations[key])).To(BeFalse(), "annotation %q was not released", key)
	}
}

func isKROMetadataKey(key, value string) bool {
	switch {
	case strings.HasPrefix(key, metadata.KROPrefix), strings.HasPrefix(key, metadata.InternalKROPrefix):
		return true
	case key == applysetspec.ApplysetPartOfLabel:
		return true
	case key == metadata.ManagedByLabelKey:
		return value == metadata.ManagedByKROValue
	default:
		return false
	}
}
