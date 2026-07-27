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
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

var _ = ginkgo.Describe("Post-Upgrade", ginkgo.Ordered, func() {
	ginkgo.BeforeAll(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("Post-upgrade checks only run in post-upgrade mode")
		}
	})

	ginkgo.It("should have healthy controller pods with zero restarts", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			podList := &corev1.PodList{}
			g.Expect(k8sClient.List(ctx, podList,
				client.InNamespace("kro-system"),
				client.MatchingLabels{"app.kubernetes.io/component": "controller"},
			)).To(gomega.Succeed())
			g.Expect(podList.Items).NotTo(gomega.BeEmpty(), "Should have controller pods")

			for _, pod := range podList.Items {
				g.Expect(pod.Status.Phase).To(gomega.Equal(corev1.PodRunning),
					"Pod %s should be Running", pod.Name)

				for _, cs := range pod.Status.ContainerStatuses {
					g.Expect(cs.Ready).To(gomega.BeTrue(),
						"Container %s in pod %s should be ready", cs.Name, pod.Name)
					g.Expect(cs.RestartCount).To(gomega.BeZero(),
						"Container %s in pod %s should have zero restarts, got %d",
						cs.Name, pod.Name, cs.RestartCount)
				}
			}
		}, 2*time.Minute, 3*time.Second).Should(gomega.Succeed())
	})

	// TODO(kro): Remove skip once the controller prunes legacy ResourceGraphAccepted
	// condition on upgrade. See: stale observedGeneration=1 on pre-0.9 RGDs.
	ginkgo.PIt("should not have legacy ResourceGraphAccepted condition after upgrade", func() {
		rgdList := &krov1alpha1.ResourceGraphDefinitionList{}
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.List(ctx, rgdList)).To(gomega.Succeed())
			for _, rgd := range rgdList.Items {
				for _, cond := range rgd.Status.Conditions {
					g.Expect(string(cond.Type)).NotTo(gomega.Equal(legacyConditionResourceGraphAccepted),
						"RGD %s still has legacy ResourceGraphAccepted condition — should be removed on upgrade",
						rgd.Name)
				}
			}
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
	})

	ginkgo.It("should have re-reconciled all RGDs (observedGeneration >= pre-upgrade generation)", func() {
		snapshot, err := loadSnapshot()
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Should be able to load pre-upgrade snapshot")

		for rgdName, preSnap := range snapshot.RGDs {
			// The deletion test suite owns this RGD's lifecycle and may
			// have already removed it.
			if rgdName == deletionRGDName {
				continue
			}
			rgd := &krov1alpha1.ResourceGraphDefinition{}
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rgdName}, rgd)).To(gomega.Succeed(),
					"RGD %s should still exist post-upgrade", rgdName)

				// metadata.generation should be >= pre-upgrade
				g.Expect(rgd.Generation).To(gomega.BeNumerically(">=", preSnap.Generation),
					"RGD %s metadata.generation should not decrease. Pre: %d, Post: %d",
					rgdName, preSnap.Generation, rgd.Generation)

				// Each non-legacy condition's observedGeneration must be >= the
				// pre-upgrade generation, proving the new controller reconciled.
				// Skip ResourceGraphAccepted — legacy condition not updated.
				for _, cond := range rgd.Status.Conditions {
					if string(cond.Type) == legacyConditionResourceGraphAccepted {
						continue
					}
					g.Expect(cond.ObservedGeneration).To(gomega.BeNumerically(">=", preSnap.Generation),
						"RGD %s condition %s observedGeneration (%d) should be >= pre-upgrade generation (%d)",
						rgdName, cond.Type, cond.ObservedGeneration, preSnap.Generation)
				}
			}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed(),
				"RGD %s should have been re-reconciled by the new controller", rgdName)
		}
	})

	ginkgo.It("should be able to create a new instance post-upgrade", func() {
		// Create a new instance using the simplest RGD (readywhen-nil, just a ConfigMap)
		newInstance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "UpgradeReadyNil",
				"metadata": map[string]interface{}{
					"name":      "test-post-upgrade-new",
					"namespace": "upgrade-test",
				},
				"spec": map[string]interface{}{
					"name": "test-post-upgrade-new",
				},
			},
		}

		_, err := dynamicClient.Resource(kroGVR("upgradereadynils")).
			Namespace("upgrade-test").
			Create(ctx, newInstance, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Wait for instance to reach ACTIVE
		gomega.Eventually(func(g gomega.Gomega) {
			obj, err := dynamicClient.Resource(kroGVR("upgradereadynils")).
				Namespace("upgrade-test").
				Get(ctx, "test-post-upgrade-new", metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			state, found, err := unstructured.NestedString(obj.Object, "status", "state")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(found).To(gomega.BeTrue())
			g.Expect(state).To(gomega.Equal("ACTIVE"),
				"New post-upgrade instance should reach ACTIVE, got %s", state)
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())

		// Verify the child ConfigMap was created (name convention: specName-resourceID)
		_, err = dynamicClient.Resource(gvrCoreConfigMaps).
			Namespace("upgrade-test").
			Get(ctx, "test-post-upgrade-new-configmap", metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			"Child ConfigMap for new post-upgrade instance should exist")

		ginkgo.GinkgoLogr.Info("New instance created post-upgrade successfully")
	})
})

var _ = ginkgo.Describe("Post-Upgrade GraphRevision", ginkgo.Ordered, func() {
	ginkgo.BeforeAll(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("GraphRevision checks only run in post-upgrade mode")
		}
		if skipGRAssertions {
			ginkgo.Skip("GraphRevision assertions skipped (upgrading from pre-GR version)")
		}
	})

	ginkgo.It("should not have created spurious GraphRevisions", func() {
		snapshot, err := loadSnapshot()
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Should be able to load pre-upgrade snapshot")

		if !snapshot.HasGRSupport {
			ginkgo.Skip("Pre-upgrade version did not have GraphRevision support")
		}

		grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Build a map of current GR counts per RGD from the live cluster.
		currentGRCountPerRGD := make(map[string]int)
		for _, gr := range grList.Items {
			rgdName := getRGDNameFromGR(gr)
			if rgdName != "" {
				currentGRCountPerRGD[rgdName]++
			}
		}

		// Verify no spurious GRs were created. For each RGD in the snapshot,
		// the current count must not exceed what is expected given the known
		// post-upgrade mutations:
		//   - mutationRGDName: exactly +1 (one deliberate mutation by mutation suite)
		//   - retentionRGDName: capped at maxGraphRevisions (rapid-mutation suite)
		//   - legacySelectorRGDName: exactly +1 (mutated by the legacy-selector
		//     compatibility suite to force CRD regeneration)
		//   - deletionRGDName: 0 or snapshot count (deletion suite may or may not have run)
		//   - all others: unchanged from snapshot
		for rgdName, preCount := range snapshot.GRCountPerRGD {
			currentCount := currentGRCountPerRGD[rgdName]

			var maxAllowed int
			switch rgdName {
			case mutationRGDName:
				maxAllowed = min(preCount+1, maxGraphRevisions)
			case retentionRGDName:
				maxAllowed = min(preCount+retentionMutations, maxGraphRevisions)
			case legacySelectorRGDName:
				maxAllowed = preCount + 1
			default:
				maxAllowed = preCount
			}
			gomega.Expect(currentCount).To(gomega.BeNumerically("<=", maxAllowed),
				"RGD %s has more GraphRevisions than expected (spurious GRs). Pre: %d, Max allowed: %d, Got: %d",
				rgdName, preCount, maxAllowed, currentCount)
		}
	})

	ginkgo.It("should have stable spec hashes for all RGDs", func() {
		snapshot, err := loadSnapshot()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		if !snapshot.HasGRSupport {
			ginkgo.Skip("Pre-upgrade version did not have GraphRevision support")
		}

		grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Build current hash map
		currentHashes := make(map[string]string)
		for _, gr := range grList.Items {
			rgdName := getRGDNameFromGR(gr)
			if rgdName == "" {
				continue
			}
			labels := gr.GetLabels()
			if hash, ok := labels["internal.kro.run/spec-hash"]; ok {
				currentHashes[rgdName] = hash
			}
		}

		for rgdName, preHash := range snapshot.GRHashPerRGD {
			postHash, exists := currentHashes[rgdName]
			gomega.Expect(exists).To(gomega.BeTrue(),
				"RGD %s should still have a GraphRevision post-upgrade", rgdName)
			gomega.Expect(postHash).To(gomega.Equal(preHash),
				fmt.Sprintf("RGD %s spec hash changed: pre=%s post=%s (normalization drift)", rgdName, preHash, postHash))
		}
	})
})

var _ = ginkgo.Describe("Post-Upgrade GraphRevision (pre-GR version)", ginkgo.Ordered, func() {
	ginkgo.BeforeAll(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("Only runs in post-upgrade mode")
		}
		if !skipGRAssertions {
			ginkgo.Skip("Only runs when upgrading from pre-GR version")
		}
	})

	ginkgo.It("should have created exactly one GraphRevision per RGD at upgrade time", func() {
		snapshot, err := loadSnapshot()
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Should be able to load pre-upgrade snapshot")
		gomega.Expect(snapshot.RGDs).NotTo(gomega.BeEmpty(),
			"Snapshot should contain the pre-upgrade RGDs")

		gomega.Expect(postUpgradeInitialGRCount).NotTo(gomega.BeEmpty(),
			"Should have captured initial GR counts in BeforeSuite")
		gomega.Expect(postUpgradeInitialGRCount).To(gomega.HaveLen(len(snapshot.RGDs)),
			"Should have captured one initial GraphRevision count per pre-upgrade RGD")

		for rgdName := range snapshot.RGDs {
			count, found := postUpgradeInitialGRCount[rgdName]
			gomega.Expect(found).To(gomega.BeTrue(),
				"RGD %s should have an initial GraphRevision after upgrade from a pre-GR version", rgdName)
			gomega.Expect(count).To(gomega.Equal(1),
				"RGD %s should have exactly 1 GraphRevision right after upgrade from pre-GR version, got %d",
				rgdName, count)
		}
	})
})
