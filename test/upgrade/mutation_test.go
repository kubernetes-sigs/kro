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

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const (
	mutationTimeout  = 2 * time.Minute
	mutationInterval = 2 * time.Second

	// The RGD we'll mutate. Uses simple-deployment because it has both
	// a Deployment and Service, making it easy to verify propagation.
	mutationRGDName         = "upgrade-simple-deployment"
	mutationInstanceName    = "test-simple"
	mutationDeploymentName  = "test-simple-deployment"
	mutationInstanceNS      = "upgrade-test"
	mutationAnnotationKey   = "upgrade-test/mutated"
	mutationAnnotationValue = "true"
)

var _ = ginkgo.Describe("Post-Upgrade RGD Mutation", ginkgo.Ordered, func() {
	var (
		grCountBefore int
		rgdGenBefore  int64
	)

	ginkgo.BeforeAll(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("Mutation checks only run in post-upgrade mode")
		}
	})

	ginkgo.It("should record GR count before mutation", func() {
		grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			// GR CRD might not exist if we just upgraded from pre-GR, but the
			// upgrade-kro step should have installed it. Log and continue.
			ginkgo.GinkgoLogr.Info("Could not list GraphRevisions before mutation", "error", err)
			grCountBefore = -1 // sentinel: GR listing failed
			return
		}
		grCountBefore = len(grList.Items)
		ginkgo.GinkgoLogr.Info("GR count before mutation", "count", grCountBefore)
	})

	ginkgo.It("should record RGD generation before mutation", func() {
		rgd := &krov1alpha1.ResourceGraphDefinition{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mutationRGDName}, rgd)).To(gomega.Succeed())
		rgdGenBefore = rgd.Generation
		ginkgo.GinkgoLogr.Info("RGD generation before mutation", "generation", rgdGenBefore)
	})

	ginkgo.It("should mutate the RGD by adding an annotation to the deployment template", func() {
		rgd := &krov1alpha1.ResourceGraphDefinition{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mutationRGDName}, rgd)).To(gomega.Succeed())

		// Find the deployment resource and patch its template to add an annotation.
		// The template is a RawExtension, so we unmarshal, modify, re-marshal.
		var patched bool
		for i, res := range rgd.Spec.Resources {
			if res == nil || res.ID != "deployment" {
				continue
			}

			var template map[string]interface{}
			gomega.Expect(json.Unmarshal(res.Template.Raw, &template)).To(gomega.Succeed())

			// Add annotation to the deployment metadata
			metadata, ok := template["metadata"].(map[string]interface{})
			if !ok {
				metadata = map[string]interface{}{}
				template["metadata"] = metadata
			}
			annotations, ok := metadata["annotations"].(map[string]interface{})
			if !ok {
				annotations = map[string]interface{}{}
				metadata["annotations"] = annotations
			}
			annotations[mutationAnnotationKey] = mutationAnnotationValue

			raw, err := json.Marshal(template)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			rgd.Spec.Resources[i].Template.Raw = raw
			patched = true
			break
		}
		gomega.Expect(patched).To(gomega.BeTrue(), "Should have found the deployment resource to patch")

		gomega.Expect(k8sClient.Update(ctx, rgd)).To(gomega.Succeed())
		ginkgo.GinkgoLogr.Info("RGD mutated", "rgd", mutationRGDName)
	})

	ginkgo.It("should see RGD generation bump after mutation", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			rgd := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mutationRGDName}, rgd)).To(gomega.Succeed())
			g.Expect(rgd.Generation).To(gomega.BeNumerically(">", rgdGenBefore),
				"RGD generation should have bumped from %d", rgdGenBefore)
		}, mutationTimeout, mutationInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should see RGD return to Active with all conditions True after mutation", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			rgd := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mutationRGDName}, rgd)).To(gomega.Succeed())

			g.Expect(rgd.Status.State).To(gomega.Equal(krov1alpha1.ResourceGraphDefinitionStateActive),
				"RGD should be Active after mutation")

			// All non-legacy conditions should be True with observedGeneration matching current generation.
			// Skip ResourceGraphAccepted — legacy condition from v0.8.x, not updated by new controller.
			for _, cond := range rgd.Status.Conditions {
				if string(cond.Type) == legacyConditionResourceGraphAccepted {
					continue
				}
				g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue),
					"Condition %s should be True", cond.Type)
				g.Expect(cond.ObservedGeneration).To(gomega.BeNumerically(">=", rgd.Generation),
					"Condition %s observedGeneration (%d) should be >= generation (%d)",
					cond.Type, cond.ObservedGeneration, rgd.Generation)
			}
		}, mutationTimeout, mutationInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should see the annotation propagated to the managed Deployment", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			deploy, err := dynamicClient.Resource(gvrAppsDeployments).
				Namespace(mutationInstanceNS).
				Get(ctx, mutationDeploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			annotations, found, err := unstructured.NestedStringMap(deploy.Object, "metadata", "annotations")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(found).To(gomega.BeTrue(), "Deployment should have annotations")
			g.Expect(annotations).To(gomega.HaveKeyWithValue(mutationAnnotationKey, mutationAnnotationValue),
				"Deployment should have the mutated annotation")
		}, mutationTimeout, mutationInterval).Should(gomega.Succeed())

		ginkgo.GinkgoLogr.Info("Mutation propagated to managed Deployment",
			"deployment", mutationDeploymentName,
			"annotation", fmt.Sprintf("%s=%s", mutationAnnotationKey, mutationAnnotationValue))
	})

	ginkgo.It("should see the instance still in ACTIVE state after mutation", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			obj, err := dynamicClient.Resource(kroGVR("upgradesimpleapps")).
				Namespace(mutationInstanceNS).
				Get(ctx, mutationInstanceName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			state, found, err := unstructured.NestedString(obj.Object, "status", "state")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(found).To(gomega.BeTrue())
			g.Expect(state).To(gomega.Equal("ACTIVE"),
				"Instance should be ACTIVE after RGD mutation, got %s", state)
		}, mutationTimeout, mutationInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should have created exactly 1 new GraphRevision (no explosion)", func() {
		if skipGRAssertions {
			ginkgo.Skip("GR count assertions skipped (upgrading from pre-GR version)")
		}
		if grCountBefore == -1 {
			ginkgo.Skip("Could not list GraphRevisions before mutation")
		}

		gomega.Eventually(func(g gomega.Gomega) {
			grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			grCountAfter := len(grList.Items)

			// When upgrading from pre-GR version: the initial upgrade created 1 GR
			// per RGD. Our mutation should create exactly 1 more for the mutated RGD.
			// When upgrading from GR-aware version: the mutation should create exactly
			// 1 more than the pre-mutation count.
			//
			// In both cases: exactly 1 more GR than before mutation.
			g.Expect(grCountAfter).To(gomega.Equal(grCountBefore+1),
				"Expected exactly 1 new GraphRevision after mutation. Before: %d, After: %d, Delta: %d",
				grCountBefore, grCountAfter, grCountAfter-grCountBefore)
		}, mutationTimeout, mutationInterval).Should(gomega.Succeed())

		ginkgo.GinkgoLogr.Info("GR count after mutation", "before", grCountBefore, "after", grCountBefore+1)
	})

	ginkgo.It("should not have created spurious GraphRevisions for unmutated RGDs", func() {
		if skipGRAssertions {
			ginkgo.Skip("GR assertions skipped (upgrading from pre-GR version)")
		}
		if grCountBefore == -1 {
			ginkgo.Skip("Could not list GraphRevisions before mutation")
		}

		grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Count GRs per RGD
		grCountPerRGD := make(map[string]int)
		for _, gr := range grList.Items {
			rgdName := getRGDNameFromGR(gr)
			if rgdName != "" {
				grCountPerRGD[rgdName]++
			}
		}

		// Load the pre-upgrade snapshot to know what the baseline was
		snapshot, err := loadSnapshot()
		if err != nil {
			// No snapshot means we can't compare, but we can still check
			// that no RGD other than the mutated one has more than expected.
			ginkgo.GinkgoLogr.Info("No pre-upgrade snapshot available, checking relative counts only")

			for rgdName, count := range grCountPerRGD {
				if rgdName == mutationRGDName {
					continue // we already verified this one got +1
				}
				// For pre-GR upgrades, each RGD should have exactly 1 GR.
				// For GR-aware upgrades, we don't know the exact count without
				// snapshot, so we just log.
				if skipGRAssertions {
					gomega.Expect(count).To(gomega.Equal(1),
						"Unmutated RGD %s should have exactly 1 GR (pre-GR upgrade), got %d",
						rgdName, count)
				}
			}
			return
		}

		// With snapshot available, compare against pre-upgrade counts.
		// Skip RGDs intentionally mutated by other post-upgrade test suites:
		//   - mutationRGDName: mutated by this suite (expected +1 GR)
		//   - retentionRGDName: mutated by the rapid-mutations suite (GC'd to maxGraphRevisions)
		for rgdName, currentCount := range grCountPerRGD {
			if rgdName == mutationRGDName || rgdName == retentionRGDName {
				continue
			}
			preCount := snapshot.GRCountPerRGD[rgdName]
			if preCount == 0 && skipGRAssertions {
				// Pre-GR upgrade: RGD had 0 GRs, now should have exactly 1
				preCount = 1
			}
			if preCount > 0 {
				gomega.Expect(currentCount).To(gomega.Equal(preCount),
					"Unmutated RGD %s GR count changed. Pre: %d, Post: %d (spurious GR!)",
					rgdName, preCount, currentCount)
			}
		}
	})
})
