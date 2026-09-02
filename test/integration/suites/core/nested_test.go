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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// These specs drive the controller to create *nested* RGDs (and their
// generated CRDs). The controller creates the nested RGD under the group named
// in its schema, bypassing the core suite's per-process group-isolating client
// — so both specs would otherwise race on the same nested identity
// (rg-nested-<type> / the generated CRD) on the shared control plane.
//
// Instead of booting a separate apiserver per spec, each spec stamps a unique
// token into the nested RGD's name and API group — parameterizing the group
// the same way the suite virtualizes kro.run per process. Distinct groups and
// names mean the specs run concurrently on the shared control plane without
// colliding.
var _ = Describe("Nested ResourceGraphDefinition", func() {
	var ns *corev1.Namespace

	BeforeEach(func(ctx SpecContext) {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("test-%s", rand.String(5)),
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, ns)).To(Succeed())
	})

	It("should handle nested ResourceGraphDefinition lifecycle", func(ctx SpecContext) {
		token := rand.String(5)
		nestedName := fmt.Sprintf("rg-nested-string-%s", token)

		// Create parent ResourceGraphDefinition
		rg, genInstance := nestedResourceGraphDefinition("testnestedrg", token)
		Expect(env.Client.Create(ctx, rg)).To(Succeed())

		// Wait for parent ResourceGraphDefinition to be ready
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rg.Name,
			}, rg)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rg.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Create instance
		instance := genInstance(ns.Name, "test-string", "string", "10")
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Expect instance status to eventually be Active
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: ns.Name,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			instanceState, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(instanceState).To(Equal("ACTIVE"))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Wait for nested ResourceGraphDefinition to be created and ready
		var nestedRG krov1alpha1.ResourceGraphDefinition
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: nestedName,
			}, &nestedRG)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(nestedRG.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Delete instance
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// Verify nested ResourceGraphDefinition is deleted
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: nestedName,
			}, &nestedRG)
			g.Expect(err).To(MatchError(errors.IsNotFound, "nested RGD should be deleted"))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Delete parent ResourceGraphDefinition
		Expect(env.Client.Delete(ctx, rg)).To(Succeed())
	})

	It("should dynamically create RGDs with different schema field types", func(ctx SpecContext) {
		token := rand.String(5)

		// Create parent ResourceGraphDefinition
		By("Creating parent ResourceGraphDefinition")
		rg, genInstance := nestedResourceGraphDefinition("testmultirg", token)
		Expect(env.Client.Create(ctx, rg)).To(Succeed())

		// Wait for parent ResourceGraphDefinition to be ready
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rg.Name,
			}, rg)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rg.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Create instances with different types
		testCases := []struct {
			name       string
			typeVal    string
			defaultVal string
		}{
			{"testinteger", "integer", "10"},
			{"teststring", "string", "default"},
		}

		// Create all instances
		for _, t := range testCases {
			By(fmt.Sprintf("Creating instance %s", t.name))
			instance := genInstance(ns.Name, t.name, t.typeVal, t.defaultVal)
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
		}

		// Wait for all nested ResourceGraphDefinitions and verify status
		for _, t := range testCases {
			By(fmt.Sprintf("Verifying instance %s", t.name))
			nestedName := fmt.Sprintf("rg-nested-%s-%s", t.typeVal, token)
			// Wait for nested ResourceGraphDefinition
			var nestedRG krov1alpha1.ResourceGraphDefinition
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: nestedName,
				}, &nestedRG)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(nestedRG.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

			// Verify parent instance status
			Eventually(func(g Gomega, ctx SpecContext) {
				instance := genInstance(ns.Name, t.name, t.typeVal, t.defaultVal)
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      t.name,
					Namespace: ns.Name,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())

				instanceStatus, found, _ := unstructured.NestedMap(instance.Object, "status")
				g.Expect(found).To(BeTrue())
				g.Expect(instanceStatus).ToNot(BeNil())
				g.Expect(instanceStatus).To(HaveKeyWithValue("state", "ACTIVE"),
					fmt.Sprintf("instance status should have state field, status was %v", instanceStatus))
			}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
		}

		// Delete all instances
		for _, t := range testCases {
			By(fmt.Sprintf("Deleting instance %s", t.name))
			instance := genInstance(ns.Name, t.name, t.typeVal, t.defaultVal)
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		}

		// Verify all nested ResourceGraphDefinitions are deleted
		for _, t := range testCases {
			By(fmt.Sprintf("Verifying instance deletion %s", t.name))
			nestedName := fmt.Sprintf("rg-nested-%s-%s", t.typeVal, token)
			Eventually(func(g Gomega, ctx SpecContext) {
				var nestedRG krov1alpha1.ResourceGraphDefinition
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: nestedName,
				}, &nestedRG)
				g.Expect(err).To(MatchError(errors.IsNotFound, "nested RGD should be deleted"))
			}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
		}

		// Delete parent ResourceGraphDefinition
		By("Deleting parent ResourceGraphDefinition")
		Expect(env.Client.Delete(ctx, rg)).To(Succeed())

		// Verify parent RGD is actually deleted
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rg.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "parent RGD should be deleted"))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
	})
})

// nestedResourceGraphDefinition creates a ResourceGraphDefinition inception.
// The nested RGD's object name and generated-CRD API group are both suffixed
// with a per-spec token so concurrent specs never collide on the same nested
// identity on the shared control plane. The nested kind is left unmangled; the
// group is what disambiguates the generated CRD across specs/processes.
func nestedResourceGraphDefinition(rgName, token string) (
	*krov1alpha1.ResourceGraphDefinition,
	func(namespace, name string, typeVal string, defaultVal string) *unstructured.Unstructured,
) {
	nestedGroup := fmt.Sprintf("nested%s.%s", token, krov1alpha1.KRODomainName)

	rg := generator.NewResourceGraphDefinition(rgName,
		generator.WithSchema(
			"NestedRGD"+rgName, "v1alpha1",
			map[string]any{
				"type":    "string",
				"default": "string",
			},
			map[string]any{},
		),
		generator.WithResource("nested", map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "ResourceGraphDefinition",
			"metadata": map[string]any{
				"name": fmt.Sprintf("rg-nested-${schema.spec.type}-%s", token),
			},
			"spec": map[string]any{
				"schema": map[string]any{
					"apiVersion": "v1alpha1",
					"group":      nestedGroup,
					"kind":       "NestedRGD${schema.spec.type}",
					"spec": map[string]any{
						"name": "string",
						"somefield": map[string]any{
							"nested": "${schema.spec.type} | default=${schema.spec.default}",
						},
					},
				},
			},
		}, nil, nil),
	)

	instanceGen := func(namespace, name string, typeVal string, defaultVal string) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "NestedRGD" + rgName,
				"metadata": map[string]any{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]any{
					"type":    typeVal,
					"default": defaultVal,
				},
			},
		}
	}

	return rg, instanceGen
}
