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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("ExternalRef", func() {
	It("should handle ResourceGraphDefinition with ExternalRef", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		By("creating ResourceGraphDefinition with ExternalRef")

		// Create ResourceGraphDefinition with ExternalRef
		rgd := generator.NewResourceGraphDefinition("test-externalref",
			generator.WithSchema(
				"TestExternalRef", "v1alpha1",
				map[string]interface{}{},
				map[string]interface{}{},
			),
			generator.WithExternalRef("deployment1", &krov1alpha1.ExternalRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Name: "test-deployment",
					// namespace should be defaulted to the instance namespace
					// Namespace: namespace,
				},
			}, nil, nil),
			generator.WithResource("deployment", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.metadata.name}",
				},
				"spec": map[string]interface{}{
					"replicas": "${deployment1.spec.replicas}",
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "deployment",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "deployment",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "web",
									"image": "nginx",
								},
							},
						},
					},
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("ensuring ResourceGraphDefinition is created and becomes active")

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("TestExternalRef"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(2))
			g.Expect(createdRGD.Spec.Resources[0].ExternalRef).ToNot(BeNil())
			g.Expect(createdRGD.Spec.Resources[0].ExternalRef.Kind).To(Equal("Deployment"))
			g.Expect(createdRGD.Spec.Resources[0].ExternalRef.Metadata.Name).To(Equal("test-deployment"))

			// Verify the ResourceGraphDefinition status
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(2))
			g.Expect(createdRGD.Status.TopologicalOrder).To(ContainElements("deployment1", "deployment"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating instance")
		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExternalRef",
				"metadata": map[string]interface{}{
					"name":      "foo-instance",
					"namespace": namespace,
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// this is the expected dpeloyment
		deployment1 := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deployment",
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To[int32](2),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "test-deployment",
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "test-deployment",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container", Image: "nginx"},
						},
					},
				},
			},
		}

		By("ensuring instance is created but doesnt become ready yet because reference is missing")
		Eventually(func(g Gomega, ctx SpecContext) {
			instance := instance.DeepCopy()
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: instance.GetNamespace(),
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment has the ConfigMap reference in envFrom
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "IN_PROGRESS"))

			deployment1 := deployment1.DeepCopy()
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      deployment1.Name,
				Namespace: deployment1.Namespace,
			}, deployment1)).To(MatchError(errors.IsNotFound, "deployment should not be created yet"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("ensuring ResourceGraphDefinition becomes ready")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition status
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(2))
			g.Expect(createdRGD.Status.TopologicalOrder).To(ContainElements("deployment1", "deployment"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating external ref dependency")
		Expect(env.Client.Create(ctx, deployment1)).To(Succeed())

		By("ensuring instance becomes ready")
		Eventually(func(g Gomega, ctx SpecContext) {
			instance := instance.DeepCopy()
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "foo-instance",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify Deployment is created with correct environment variables
		By("ensuring dependent deployment is created with correct environment variables")
		Eventually(func(g Gomega, ctx SpecContext) {
			deployment := &appsv1.Deployment{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "foo-instance",
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment has the ConfigMap reference in envFrom
			g.Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Expect(env.Client.Delete(ctx, deployment1)).To(Succeed())
	})

	It("should handle ExternalRef to CRD with CEL expressions in metadata", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))
		crdName := "testexternalcrds.kro.run"

		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		By("creating test CRD that will be referenced")

		testCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: crdName,
			},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "kro.run",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Kind:   "TestExternalCrds",
					Plural: "testexternalcrds",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{
						Name:    "v1",
						Served:  true,
						Storage: true,
						Schema: &apiextensionsv1.CustomResourceValidation{
							OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
								Type: "object",
							},
						},
					},
				},
			},
		}
		Expect(env.Client.Create(ctx, testCRD)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, testCRD)).To(Succeed())
		})

		By("creating ResourceGraphDefinition with ExternalRef to CRD using CEL in metadata")

		rgd := generator.NewResourceGraphDefinition("test-crd-externalref",
			generator.WithSchema(
				"TestCRDExternalRef", "v1alpha1",
				map[string]interface{}{
					"crdName": "string",
				},
				nil,
			),
			generator.WithExternalRef("crd", &krov1alpha1.ExternalRef{
				APIVersion: "apiextensions.k8s.io/v1",
				Kind:       "CustomResourceDefinition",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Name: "${schema.spec.crdName}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("ensuring ResourceGraphDefinition is created and becomes active")

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(createdRGD.Status.TopologicalOrder).To(ContainElements("crd"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating instance")

		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestCRDExternalRef",
				"metadata": map[string]interface{}{
					"name":      "test-instance",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"crdName": crdName,
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("ensuring instance becomes ready")

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: instance.GetNamespace(),
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
	})
})
