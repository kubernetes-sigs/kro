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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Schema-aware CEL value conversion", func() {
	It("should convert Secret data bytes so string() works at runtime", func(ctx SpecContext) {
		ns := fmt.Sprintf("test-%s", rand.String(5))

		By("creating namespace")
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}
		Expect(env.Client.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, namespace)
		})

		By("creating ResourceGraphDefinition with string(secret.data.key) in status")

		// The RGD creates a Secret via stringData (Kubernetes base64-encodes into data).
		// The status expression uses string(secret.data.clientId) which requires
		// schema-aware conversion: the OpenAPI schema declares data values as
		// format:"byte", so UnstructuredToVal converts the base64 string to cel bytes,
		// making string(bytes) work correctly.
		rgd := generator.NewResourceGraphDefinition(
			"test-schema-cel",
			generator.WithSchema(
				"TestSchemaCel",
				"v1alpha1",
				map[string]any{
					"clientId": "string",
				},
				map[string]any{
					"decodedClientId": `${string(secret.data.clientId)}`,
				},
			),
			generator.WithResource("secret", map[string]any{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]any{
					"name": "${schema.metadata.name}",
				},
				"stringData": map[string]any{
					"clientId": "${schema.spec.clientId}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("waiting for RGD to become Active")
		Eventually(func(g Gomega, ctx SpecContext) {
			obj := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, obj)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(obj.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating instance with a known clientId value")
		instance := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestSchemaCel",
				"metadata": map[string]any{
					"name":      "test-schema-cel",
					"namespace": ns,
				},
				"spec": map[string]any{
					"clientId": "my-secret-client",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		By("waiting for Secret to be created")
		Eventually(func(g Gomega, ctx SpecContext) {
			secret := &corev1.Secret{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-schema-cel",
				Namespace: ns,
			}, secret)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(secret.Data).To(HaveKey("clientId"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("verifying instance status has the decoded secret value")
		Eventually(func(g Gomega, ctx SpecContext) {
			obj := &unstructured.Unstructured{}
			obj.SetAPIVersion("kro.run/v1alpha1")
			obj.SetKind("TestSchemaCel")

			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-schema-cel",
				Namespace: ns,
			}, obj)
			g.Expect(err).ToNot(HaveOccurred())

			status, ok := obj.Object["status"].(map[string]any)
			g.Expect(ok).To(BeTrue(), "status should be a map")
			g.Expect(status).To(HaveKeyWithValue("state", "ACTIVE"))
			g.Expect(status).To(HaveKeyWithValue("decodedClientId", "my-secret-client"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
