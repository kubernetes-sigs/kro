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
	retentionRGDName    = "upgrade-readywhen-empty"
	retentionInstanceNS = "upgrade-test"
	retentionInstance   = "test-readyempty"
	retentionChild      = "test-readyempty-configmap"
	maxGraphRevisions   = 5
	retentionMutations  = 10
	retentionTimeout    = 2 * time.Minute
	retentionInterval   = 2 * time.Second
)

var _ = ginkgo.Describe("Post-Upgrade Rapid Mutations", ginkgo.Ordered, func() {
	ginkgo.BeforeAll(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("Only runs in post-upgrade mode")
		}
	})

	for i := 1; i <= retentionMutations; i++ {
		ginkgo.It(fmt.Sprintf("mutation %d/%d: update RGD, verify instance and child updated",
			i, retentionMutations), func() {
			ginkgo.By("Mutating the RGD")
			rgd := &krov1alpha1.ResourceGraphDefinition{}
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: retentionRGDName}, rgd)).To(gomega.Succeed())

			for j, res := range rgd.Spec.Resources {
				if res == nil || res.ID != "configmap" {
					continue
				}
				var template map[string]interface{}
				gomega.Expect(json.Unmarshal(res.Template.Raw, &template)).To(gomega.Succeed())

				metadata, _ := template["metadata"].(map[string]interface{})
				if metadata == nil {
					metadata = map[string]interface{}{}
					template["metadata"] = metadata
				}
				annotations, _ := metadata["annotations"].(map[string]interface{})
				if annotations == nil {
					annotations = map[string]interface{}{}
					metadata["annotations"] = annotations
				}
				annotations["upgrade-test/rapid-iteration"] = fmt.Sprintf("%d", i)

				raw, err := json.Marshal(template)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				rgd.Spec.Resources[j].Template.Raw = raw
				break
			}
			gomega.Expect(k8sClient.Update(ctx, rgd)).To(gomega.Succeed())

			ginkgo.By("Waiting for RGD to be Active")
			gomega.Eventually(func(g gomega.Gomega) {
				updated := &krov1alpha1.ResourceGraphDefinition{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: retentionRGDName}, updated)).To(gomega.Succeed())
				g.Expect(updated.Status.State).To(gomega.Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, retentionTimeout, retentionInterval).Should(gomega.Succeed())

			ginkgo.By("Waiting for child ConfigMap to have the new annotation")
			gomega.Eventually(func(g gomega.Gomega) {
				cm, err := dynamicClient.Resource(gvrCoreConfigMaps).
					Namespace(retentionInstanceNS).
					Get(ctx, retentionChild, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())

				annotations, _, _ := unstructured.NestedStringMap(cm.Object, "metadata", "annotations")
				g.Expect(annotations).To(gomega.HaveKeyWithValue(
					"upgrade-test/rapid-iteration", fmt.Sprintf("%d", i)),
					"Iteration %d: child ConfigMap should have the updated annotation", i)
			}, retentionTimeout, retentionInterval).Should(gomega.Succeed())

			ginkgo.By("Verifying instance is ACTIVE")
			obj, err := dynamicClient.Resource(kroGVR("upgradereadyempties")).
				Namespace(retentionInstanceNS).
				Get(ctx, retentionInstance, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			state, found, _ := unstructured.NestedString(obj.Object, "status", "state")
			gomega.Expect(found).To(gomega.BeTrue())
			gomega.Expect(state).To(gomega.Equal("ACTIVE"),
				"Iteration %d: instance should be ACTIVE, got %s", i, state)
		})
	}

	ginkgo.It("should have retained only the latest GraphRevisions", func() {
		if skipGRAssertions {
			ginkgo.Skip("GR assertions skipped (pre-GR version)")
		}

		gomega.Eventually(func(g gomega.Gomega) {
			grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			var grCount int
			var highestRevision, lowestRevision int64
			lowestRevision = 999999
			for _, gr := range grList.Items {
				if getRGDNameFromGR(gr) != retentionRGDName {
					continue
				}
				grCount++
				revision, found, _ := unstructured.NestedInt64(gr.Object, "spec", "revision")
				if found {
					if revision > highestRevision {
						highestRevision = revision
					}
					if revision < lowestRevision {
						lowestRevision = revision
					}
				}
			}

			g.Expect(grCount).To(gomega.BeNumerically("<=", maxGraphRevisions),
				"Should have at most %d GRs, got %d", maxGraphRevisions, grCount)

			// The retained revisions should be the most recent ones
			// e.g. after 10 mutations with retention=5, revisions 6-10 should remain
			expectedLowest := highestRevision - int64(maxGraphRevisions) + 1
			if expectedLowest < 1 {
				expectedLowest = 1
			}
			g.Expect(lowestRevision).To(gomega.BeNumerically(">=", expectedLowest),
				"Lowest retained revision should be >= %d (highest=%d, max=%d), got %d",
				expectedLowest, highestRevision, maxGraphRevisions, lowestRevision)

			ginkgo.GinkgoLogr.Info("GR retention verified",
				"grCount", grCount,
				"revisionRange", fmt.Sprintf("%d-%d", lowestRevision, highestRevision),
				"maxAllowed", maxGraphRevisions)
		}, retentionTimeout, retentionInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should have RGD lastIssuedRevision matching highest GR", func() {
		if skipGRAssertions {
			ginkgo.Skip("GR assertions skipped (pre-GR version)")
		}

		grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		var highestRevision int64
		for _, gr := range grList.Items {
			if getRGDNameFromGR(gr) != retentionRGDName {
				continue
			}
			revision, found, _ := unstructured.NestedInt64(gr.Object, "spec", "revision")
			if found && revision > highestRevision {
				highestRevision = revision
			}
		}

		rgd := &krov1alpha1.ResourceGraphDefinition{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: retentionRGDName}, rgd)).To(gomega.Succeed())

		gomega.Expect(rgd.Status.LastIssuedRevision).To(gomega.Equal(highestRevision),
			"lastIssuedRevision (%d) should match highest GR revision (%d)",
			rgd.Status.LastIssuedRevision, highestRevision)
	})
})
