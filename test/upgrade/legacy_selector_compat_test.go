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

package upgrade_test

// TestLegacySelectorCompatibility verifies that an RGD whose externalRef selector
// was authored as a literal Kubernetes LabelSelector object (the pre-schemaless
// form) still works after the field was relaxed to runtime.RawExtension.
//
// The test runs in post-upgrade mode only.  It exercises:
//
//  1. Admission: the API server can still store and serve the old object — no
//     schema validation error when the RGD is applied or when the new controller
//     reads it.
//
//  2. Build: the graph builder accepts the old selector shape and the RGD reaches
//     Active state.
//
//  3. Reconciliation: the instance controller correctly resolves the external
//     collection via the legacy selector and the managed summary ConfigMap is
//     created.
//
//  4. Post-mutation: a harmless spec change forces the controller to rebuild the
//     graph, regenerate the CRD, and reconcile again — proving the new code path
//     handles a stored legacy-selector RGD without errors.

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const (
	legacySelectorRGDName      = "upgrade-legacy-selector"
	legacySelectorInstanceNS   = "upgrade-test"
	legacySelectorInstanceName = "test-legacy-selector"
	legacySelectorTimeout      = 2 * time.Minute
	legacySelectorInterval     = 2 * time.Second
)

var _ = ginkgo.Describe("Post-Upgrade Legacy Selector Compatibility", ginkgo.Ordered, func() {
	var rgdGenBefore int64

	ginkgo.BeforeAll(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("Legacy selector compatibility checks only run in post-upgrade mode")
		}
	})

	ginkgo.It("should have the legacy-selector RGD in Active state after upgrade", func() {
		// The RGD was applied pre-upgrade with a selector that was a literal
		// LabelSelector object.  After the controller restarts with the new
		// build, it must reconcile the stored object and reach Active.
		rgd := &krov1alpha1.ResourceGraphDefinition{}
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: legacySelectorRGDName}, rgd)).To(gomega.Succeed())
			g.Expect(rgd.Status.State).To(gomega.Equal(krov1alpha1.ResourceGraphDefinitionStateActive),
				"RGD %s should be Active after upgrade", legacySelectorRGDName)
		}, legacySelectorTimeout, legacySelectorInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should have the instance in ACTIVE state", func() {
		gvrLegacy := kroGVR("upgradelegacyselectors")
		gomega.Eventually(func(g gomega.Gomega) {
			obj, err := dynamicClient.Resource(gvrLegacy).
				Namespace(legacySelectorInstanceNS).
				Get(ctx, legacySelectorInstanceName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			state, found, err := unstructured.NestedString(obj.Object, "status", "state")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(found).To(gomega.BeTrue(), "instance should have status.state")
			g.Expect(state).To(gomega.Equal("ACTIVE"),
				"instance %s should be ACTIVE, got %s", legacySelectorInstanceName, state)
		}, legacySelectorTimeout, legacySelectorInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should project collectionSize status via the legacy selector", func() {
		// collectionSize is projected from extcollection into instance status,
		// so this verifies the selector is parsed and the matching external
		// ConfigMaps are listed correctly.
		gomega.Eventually(func(g gomega.Gomega) {
			gvrLegacy := kroGVR("upgradelegacyselectors")
			obj, err := dynamicClient.Resource(gvrLegacy).
				Namespace(legacySelectorInstanceNS).
				Get(ctx, legacySelectorInstanceName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			// collectionSize is set to string(size(extcollection)) in schema.status.
			// The shared external-data ConfigMap carries the label, so expect "1".
			collectionSize, found, err := unstructured.NestedString(obj.Object, "status", "collectionSize")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(found).To(gomega.BeTrue(), "instance should have status.collectionSize")
			g.Expect(collectionSize).To(gomega.Equal("1"),
				"collectionSize should be 1 (the labeled external-data ConfigMap)")
		}, legacySelectorTimeout, legacySelectorInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should record RGD generation before mutation", func() {
		rgd := &krov1alpha1.ResourceGraphDefinition{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: legacySelectorRGDName}, rgd)).To(gomega.Succeed())
		rgdGenBefore = rgd.Generation
		ginkgo.GinkgoLogr.Info("RGD generation before mutation", "generation", rgdGenBefore)
	})

	ginkgo.It("should remain Active after a harmless spec mutation (forces CRD regeneration)", func() {
		// Touch the externalRef resource by adding a no-op includeWhen.  This
		// bumps the spec hash and causes the controller to rebuild the graph and
		// update the generated CRD — exercising the full reconcile path for a
		// stored legacy-selector RGD.
		gomega.Eventually(func(g gomega.Gomega) {
			rgd := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: legacySelectorRGDName}, rgd)).To(gomega.Succeed())

			var patched bool
			for i, res := range rgd.Spec.Resources {
				if res == nil || res.ID != "extcollection" {
					continue
				}
				// Toggle between two equivalent standalone CEL expressions so the
				// update always changes spec and bumps generation, even on reruns.
				nextIncludeWhen := "${true}"
				if len(rgd.Spec.Resources[i].IncludeWhen) == 1 && rgd.Spec.Resources[i].IncludeWhen[0] == "${true}" {
					nextIncludeWhen = "${1 == 1}"
				}
				rgd.Spec.Resources[i].IncludeWhen = []string{nextIncludeWhen}
				patched = true
				break
			}
			g.Expect(patched).To(gomega.BeTrue(), "should have found the extcollection resource to patch")
			g.Expect(k8sClient.Update(ctx, rgd)).To(gomega.Succeed())
		}, legacySelectorTimeout, legacySelectorInterval).Should(gomega.Succeed())

		// After the mutation, the RGD generation should bump and the controller
		// must bring the RGD back to Active with all conditions True.
		gomega.Eventually(func(g gomega.Gomega) {
			rgd := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: legacySelectorRGDName}, rgd)).To(gomega.Succeed())

			g.Expect(rgd.Generation).To(gomega.BeNumerically(">", rgdGenBefore),
				"RGD generation should have bumped from %d", rgdGenBefore)
			g.Expect(rgd.Status.State).To(gomega.Equal(krov1alpha1.ResourceGraphDefinitionStateActive),
				"RGD should be Active after mutation")

			for _, cond := range rgd.Status.Conditions {
				if string(cond.Type) == legacyConditionResourceGraphAccepted {
					continue
				}
				g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue),
					"condition %s should be True after mutation", cond.Type)
				g.Expect(cond.ObservedGeneration).To(gomega.BeNumerically(">=", rgd.Generation),
					"condition %s observedGeneration should be >= current generation", cond.Type)
			}
		}, legacySelectorTimeout, legacySelectorInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should still resolve the external collection and keep status projection", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			gvrLegacy := kroGVR("upgradelegacyselectors")
			obj, err := dynamicClient.Resource(gvrLegacy).
				Namespace(legacySelectorInstanceNS).
				Get(ctx, legacySelectorInstanceName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			collectionSize, found, err := unstructured.NestedString(obj.Object, "status", "collectionSize")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(found).To(gomega.BeTrue())
			g.Expect(collectionSize).To(gomega.Equal("1"),
				"collectionSize should still be 1 after the mutation")
		}, legacySelectorTimeout, legacySelectorInterval).Should(gomega.Succeed())
	})
})
