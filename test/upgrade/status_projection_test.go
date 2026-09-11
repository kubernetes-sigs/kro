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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

var _ = ginkgo.Describe("Post-Upgrade Status Projection", func() {
	ginkgo.BeforeEach(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("Status projection checks only run in post-upgrade mode")
		}
	})

	ginkgo.It("should remove and restore projected status when a child field disappears", func() {
		// The simple-deployment RGD projects: availableReplicas: ${deployment.status.availableReplicas}
		instances := dynamicClient.Resource(kroGVR("upgradesimpleapps")).Namespace("upgrade-test")
		obj, err := instances.Get(ctx, "test-simple", metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Instance should be ACTIVE
		state, found, _ := unstructured.NestedString(obj.Object, "status", "state")
		gomega.Expect(found).To(gomega.BeTrue())
		gomega.Expect(state).To(gomega.Equal("ACTIVE"))

		// availableReplicas should be projected from the deployment
		availableReplicas, found, err := unstructured.NestedFieldNoCopy(obj.Object, "status", "availableReplicas")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(found).To(gomega.BeTrue(),
			"Instance status should have availableReplicas projected from deployment")
		gomega.Expect(availableReplicas).NotTo(gomega.BeNil())

		ginkgo.GinkgoLogr.Info("Status projection verified",
			"availableReplicas", availableReplicas, "uid", obj.GetUID(), "managedFields", obj.GetManagedFields())

		replicas, found, err := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(found).To(gomega.BeTrue())
		gomega.Expect(replicas).To(gomega.BeNumerically(">", 0))
		gomega.Expect(availableReplicas).To(gomega.BeNumerically("==", replicas))
		setReplicas := func(count int64) {
			_, err := instances.Patch(ctx, "test-simple", types.MergePatchType,
				[]byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, count)), metav1.PatchOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		// This fixture is shared: cleanup both tests restoration and leaves it
		// healthy for subsequent upgrade checks, including when removal fails.
		ginkgo.DeferCleanup(func() {
			setReplicas(replicas)
			gomega.Eventually(func(g gomega.Gomega) {
				got, err := instances.Get(ctx, "test-simple", metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(got.GetUID()).To(gomega.Equal(obj.GetUID()))
				value, found, err := unstructured.NestedInt64(got.Object, "status", "availableReplicas")
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(found).To(gomega.BeTrue())
				g.Expect(value).To(gomega.Equal(replicas))
				state, _, _ := unstructured.NestedString(got.Object, "status", "state")
				g.Expect(state).To(gomega.Equal("ACTIVE"))
				conditions, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
				g.Expect(conditions).NotTo(gomega.BeEmpty())
				for _, condition := range conditions {
					c := condition.(map[string]any)
					g.Expect(c["status"]).To(gomega.Equal("True"))
					g.Expect(c["observedGeneration"]).To(gomega.Equal(got.GetGeneration()))
				}
			}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
		})

		setReplicas(0)
		gomega.Eventually(func(g gomega.Gomega) {
			deployment, err := dynamicClient.Resource(gvrAppsDeployments).Namespace("upgrade-test").
				Get(ctx, "test-simple-deployment", metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			desired, _, _ := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
			g.Expect(desired).To(gomega.BeZero())
			observed, _, _ := unstructured.NestedInt64(deployment.Object, "status", "observedGeneration")
			g.Expect(observed).To(gomega.BeNumerically(">=", deployment.GetGeneration()))
			_, found, err := unstructured.NestedFieldNoCopy(deployment.Object, "status", "availableReplicas")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(found).To(gomega.BeFalse(), "the child field must actually disappear")
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			got, err := instances.Get(ctx, "test-simple", metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(got.GetUID()).To(gomega.Equal(obj.GetUID()))
			status, _, _ := unstructured.NestedMap(got.Object, "status")
			g.Expect(status).NotTo(gomega.HaveKey("availableReplicas"), "an unresolved projection must not stay stale")
			g.Expect(status["conditions"]).NotTo(gomega.BeEmpty())
			g.Expect(status["state"]).NotTo(gomega.BeEmpty())
			// readyWhen also reads the missing field, so ACTIVE is not required here.
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
	})
})
