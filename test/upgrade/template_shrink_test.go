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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const (
	shrinkTimeout  = 2 * time.Minute
	shrinkInterval = 2 * time.Second

	shrinkRGDName        = "upgrade-template-shrink"
	shrinkResourceID     = "configmap"
	shrinkConfigMapName  = "test-template-shrink-configmap"
	shrinkInstanceNS     = "upgrade-test"
	shrinkRemovableKey   = "removable"
	shrinkRetainedKey    = "keep"
	shrinkRetainedValue  = "retained"
	shrinkRemovableValue = "remove-me"
)

// Removing a field from a template has to take effect on resources that were
// created by the version we upgraded from.
//
// kro applies managed resources with server-side apply, which only clears
// fields the same field manager set previously. Everything about that is
// version-sensitive: if a release changes the field manager it applies under,
// objects created by an earlier release keep their fields owned by a manager
// that never applies again, and dropping a field from a template silently stops
// removing it from exactly those objects — the pre-existing ones. New objects
// behave correctly, which makes the divergence easy to miss.
//
// Adding a field is already covered by the RGD mutation checks. This is the
// other direction, which is the one that depends on field ownership carrying
// across the upgrade.
var _ = ginkgo.Describe("Post-Upgrade Template Shrink", ginkgo.Ordered, func() {
	ginkgo.BeforeAll(func() {
		if !isPostUpgrade() {
			ginkgo.Skip("Template shrink checks only run in post-upgrade mode")
		}
	})

	ginkgo.It("should start from a resource carrying both template fields", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			cm := &corev1.ConfigMap{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      shrinkConfigMapName,
				Namespace: shrinkInstanceNS,
			}, cm)).To(gomega.Succeed())
			g.Expect(cm.Data).To(gomega.HaveKeyWithValue(shrinkRetainedKey, shrinkRetainedValue))
			g.Expect(cm.Data).To(gomega.HaveKeyWithValue(shrinkRemovableKey, shrinkRemovableValue),
				"the pre-upgrade fixture should have created both data keys")
		}, shrinkTimeout, shrinkInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should remove the field from the RGD template", func() {
		rgd := &krov1alpha1.ResourceGraphDefinition{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: shrinkRGDName}, rgd)).To(gomega.Succeed())

		var patched bool
		for i, res := range rgd.Spec.Resources {
			if res == nil || res.ID != shrinkResourceID {
				continue
			}

			var template map[string]any
			gomega.Expect(json.Unmarshal(res.Template.Raw, &template)).To(gomega.Succeed())

			data, ok := template["data"].(map[string]any)
			gomega.Expect(ok).To(gomega.BeTrue(), "template should carry a data block")
			delete(data, shrinkRemovableKey)

			raw, err := json.Marshal(template)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			rgd.Spec.Resources[i].Template.Raw = raw
			patched = true
			break
		}
		gomega.Expect(patched).To(gomega.BeTrue(),
			"should have found resource %q to patch", shrinkResourceID)

		gomega.Expect(k8sClient.Update(ctx, rgd)).To(gomega.Succeed())
		ginkgo.GinkgoLogr.Info("removed template field", "rgd", shrinkRGDName, "key", shrinkRemovableKey)
	})

	ginkgo.It("should see the RGD return to Active after the change", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			rgd := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: shrinkRGDName}, rgd)).To(gomega.Succeed())
			g.Expect(rgd.Status.State).To(gomega.Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			for _, cond := range rgd.Status.Conditions {
				if string(cond.Type) == legacyConditionResourceGraphAccepted {
					continue
				}
				g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue),
					"Condition %s should be True", cond.Type)
			}
		}, shrinkTimeout, shrinkInterval).Should(gomega.Succeed())
	})

	ginkgo.It("should drop the removed field from the pre-upgrade resource", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			cm := &corev1.ConfigMap{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      shrinkConfigMapName,
				Namespace: shrinkInstanceNS,
			}, cm)).To(gomega.Succeed())

			g.Expect(cm.Data).NotTo(gomega.HaveKey(shrinkRemovableKey),
				"field removed from the template is still present on a resource created "+
					"before the upgrade; managed fields: %s", managedFieldSummary(cm))

			// Guard against the opposite failure: over-deleting the fields that
			// are still declared.
			g.Expect(cm.Data).To(gomega.HaveKeyWithValue(shrinkRetainedKey, shrinkRetainedValue),
				"a field still declared in the template was removed")
		}, shrinkTimeout, shrinkInterval).Should(gomega.Succeed())
	})
})

// managedFieldSummary renders which field manager owns which parts of an
// object, so a failure shows why a field was not removed.
func managedFieldSummary(obj metav1.Object) string {
	out := ""
	for _, entry := range obj.GetManagedFields() {
		fields := ""
		if entry.FieldsV1 != nil {
			fields = string(entry.FieldsV1.Raw)
		}
		out += "[" + entry.Manager + "/" + string(entry.Operation) + " " + fields + "] "
	}
	if out == "" {
		return "<none>"
	}
	return out
}
