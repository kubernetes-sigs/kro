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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const (
	// Dedicated fixture for deletion tests — isolated from other test suites.
	deletionRGDName    = "upgrade-deletion-target"
	deletionInstanceNS = "upgrade-test"
	deletionInstance   = "test-deletion-target"
	deletionChild      = "test-deletion-target-configmap"
	deletionTimeout    = 2 * time.Minute
	deletionInterval   = 2 * time.Second
)

var _ = ginkgo.Describe("Post-Upgrade Deletion", ginkgo.Ordered, func() {
	ginkgo.BeforeAll(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("Deletion checks only run in post-upgrade mode")
		}
	})

	ginkgo.It("should delete an instance and see child resources garbage collected", func() {
		ginkgo.By("Verifying instance and child exist before deletion")
		_, err := dynamicClient.Resource(kroGVR("upgradedeletiontargets")).
			Namespace(deletionInstanceNS).
			Get(ctx, deletionInstance, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		_, err = dynamicClient.Resource(gvrCoreConfigMaps).
			Namespace(deletionInstanceNS).
			Get(ctx, deletionChild, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Deleting the instance")
		err = dynamicClient.Resource(kroGVR("upgradedeletiontargets")).
			Namespace(deletionInstanceNS).
			Delete(ctx, deletionInstance, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Waiting for instance to be gone")
		gomega.Eventually(func(g gomega.Gomega) {
			_, err := dynamicClient.Resource(kroGVR("upgradedeletiontargets")).
				Namespace(deletionInstanceNS).
				Get(ctx, deletionInstance, metav1.GetOptions{})
			g.Expect(errors.IsNotFound(err)).To(gomega.BeTrue(),
				"Instance should be deleted, got: %v", err)
		}, deletionTimeout, deletionInterval).Should(gomega.Succeed())

		ginkgo.By("Waiting for child ConfigMap to be garbage collected")
		gomega.Eventually(func(g gomega.Gomega) {
			_, err := dynamicClient.Resource(gvrCoreConfigMaps).
				Namespace(deletionInstanceNS).
				Get(ctx, deletionChild, metav1.GetOptions{})
			g.Expect(errors.IsNotFound(err)).To(gomega.BeTrue(),
				"Child ConfigMap should be garbage collected, got: %v", err)
		}, deletionTimeout, deletionInterval).Should(gomega.Succeed())

		ginkgo.GinkgoLogr.Info("Instance deleted, child resources garbage collected")
	})

	ginkgo.It("should recreate the instance and see it become ACTIVE with child resources", func() {
		ginkgo.By("Recreating the instance")
		newInstance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "UpgradeDeletionTarget",
				"metadata": map[string]interface{}{
					"name":      deletionInstance,
					"namespace": deletionInstanceNS,
				},
				"spec": map[string]interface{}{
					"name": deletionInstance,
				},
			},
		}

		_, err := dynamicClient.Resource(kroGVR("upgradedeletiontargets")).
			Namespace(deletionInstanceNS).
			Create(ctx, newInstance, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Waiting for instance to be ACTIVE")
		gomega.Eventually(func(g gomega.Gomega) {
			obj, err := dynamicClient.Resource(kroGVR("upgradedeletiontargets")).
				Namespace(deletionInstanceNS).
				Get(ctx, deletionInstance, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			state, found, _ := unstructured.NestedString(obj.Object, "status", "state")
			g.Expect(found).To(gomega.BeTrue())
			g.Expect(state).To(gomega.Equal("ACTIVE"),
				"Recreated instance should be ACTIVE, got %s", state)
		}, deletionTimeout, deletionInterval).Should(gomega.Succeed())

		ginkgo.By("Verifying child ConfigMap exists again")
		_, err = dynamicClient.Resource(gvrCoreConfigMaps).
			Namespace(deletionInstanceNS).
			Get(ctx, deletionChild, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			"Child ConfigMap should exist after instance recreation")

		ginkgo.GinkgoLogr.Info("Instance recreated successfully")
	})

	ginkgo.It("should block RGD deletion from removing the generated CRD (allowCRDDeletion=false)", func() {
		ginkgo.By("Verifying the CRD exists")
		_, err := dynamicClient.Resource(gvrCRDs).
			Get(ctx, "upgradedeletiontargets.kro.run", metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Deleting the RGD")
		err = k8sClient.Delete(ctx, &krov1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: deletionRGDName},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Waiting for RGD to be gone")
		gomega.Eventually(func(g gomega.Gomega) {
			rgd := &krov1alpha1.ResourceGraphDefinition{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: deletionRGDName}, rgd)
			g.Expect(errors.IsNotFound(err)).To(gomega.BeTrue(),
				"RGD should be deleted, got: %v", err)
		}, deletionTimeout, deletionInterval).Should(gomega.Succeed())

		ginkgo.By("Verifying the generated CRD still exists (allowCRDDeletion=false)")
		_, err = dynamicClient.Resource(gvrCRDs).
			Get(ctx, "upgradedeletiontargets.kro.run", metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			"Generated CRD should still exist when allowCRDDeletion=false")

		ginkgo.By("Verifying GraphRevisions for the deleted RGD are cleaned up")
		if !skipGRAssertions {
			gomega.Eventually(func(g gomega.Gomega) {
				grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())

				for _, gr := range grList.Items {
					rgdName := getRGDNameFromGR(gr)
					g.Expect(rgdName).NotTo(gomega.Equal(deletionRGDName),
						"GraphRevision %s should be deleted with its RGD", gr.GetName())
				}
			}, deletionTimeout, deletionInterval).Should(gomega.Succeed())

			ginkgo.GinkgoLogr.Info("GraphRevisions cleaned up for deleted RGD")
		}

		ginkgo.GinkgoLogr.Info("RGD deleted but CRD preserved (allowCRDDeletion=false)")
	})
})
