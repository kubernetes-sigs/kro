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

package core_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// unstickTerminatingCRD completes the deletion of the named CRD when it is
// stuck Terminating with only the apiserver's customresourcecleanup finalizer.
//
// Under heavy parallel CRD churn against the shared control plane, the
// apiserver's finalizer controller starves and can leave a CRD Terminating for
// tens of seconds, which flakes specs that block on the CRD being gone or
// recreated. kro never finalizes the CRD object itself (its finalizer lives on
// the ResourceGraphDefinition), and the CRDs in the specs that call this create
// no custom resources, so dropping that lone finalizer is exactly the step the
// apiserver would eventually perform — it just does it now.
//
// It is deliberately scoped to a single CRD owned by the calling spec (no
// cross-process effect, unlike a background reaper) and returns once the CRD is
// gone or has been recreated (no deletionTimestamp).
func unstickTerminatingCRD(ctx SpecContext, name string) {
	GinkgoHelper()
	Eventually(func(g Gomega, ctx SpecContext) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := env.Client.Get(ctx, types.NamespacedName{Name: name}, crd)
		if errors.IsNotFound(err) {
			return // deletion completed
		}
		g.Expect(err).ToNot(HaveOccurred())
		if crd.DeletionTimestamp == nil {
			return // live, or already recreated by the controller
		}
		if len(crd.Finalizers) == 1 &&
			crd.Finalizers[0] == apiextensionsv1.CustomResourceCleanupFinalizer {
			crd.Finalizers = nil
			// Best effort: the apiserver's own finalizer controller may win the
			// race (conflict / not-found); the next poll reconciles either way.
			_ = env.Client.Update(ctx, crd)
		}
		g.Expect(crd.DeletionTimestamp).To(BeNil(), "waiting for stuck Terminating CRD to clear")
	}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
}
