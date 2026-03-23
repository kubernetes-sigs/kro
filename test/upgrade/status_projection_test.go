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
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = ginkgo.Describe("Post-Upgrade Status Projection", func() {
	ginkgo.BeforeEach(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("Status projection checks only run in post-upgrade mode")
		}
	})

	ginkgo.It("should project availableReplicas from deployment to instance status", func() {
		// The simple-deployment RGD projects: availableReplicas: ${deployment.status.availableReplicas}
		obj, err := dynamicClient.Resource(kroGVR("upgradesimpleapps")).
			Namespace("upgrade-test").
			Get(ctx, "test-simple", metav1.GetOptions{})
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
			"availableReplicas", availableReplicas)
	})
})
