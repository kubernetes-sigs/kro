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
	"sigs.k8s.io/controller-runtime/pkg/client"

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

		// this is the expected deployment
		deployment1 := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deployment",
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(2)),
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

		By("ensuring instance is in progress while waiting for external reference")
		Eventually(func(g Gomega, ctx SpecContext) {
			instance := instance.DeepCopy()
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: instance.GetNamespace(),
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// Instance should be IN_PROGRESS while waiting for external ref
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

		By("patching the external deployment and ensuring the managed deployment reacts via watch")
		Expect(env.Client.Get(ctx, types.NamespacedName{
			Name:      deployment1.Name,
			Namespace: deployment1.Namespace,
		}, deployment1)).To(Succeed())
		originalExternalDeployment := deployment1.DeepCopy()
		deployment1.Spec.Replicas = ptr.To(int32(4))
		Expect(env.Client.Patch(ctx, deployment1, client.MergeFrom(originalExternalDeployment))).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			deployment := &appsv1.Deployment{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "foo-instance",
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(4)))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

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
				Name:        crdName,
				Annotations: map[string]string{"phase": "pending"},
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
				map[string]interface{}{
					"observedPhase": "${crd.metadata.annotations[\"phase\"]}",
				},
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

			observedPhase, found, err := unstructured.NestedString(instance.Object, "status", "observedPhase")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(observedPhase).To(Equal("pending"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("patching the referenced CRD annotation and ensuring instance status reacts via watch")
		Expect(env.Client.Get(ctx, types.NamespacedName{Name: crdName}, testCRD)).To(Succeed())
		originalCRD := testCRD.DeepCopy()
		testCRD.Annotations["phase"] = "ready"
		Expect(env.Client.Patch(ctx, testCRD, client.MergeFrom(originalCRD))).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: instance.GetNamespace(),
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			observedPhase, found, err := unstructured.NestedString(instance.Object, "status", "observedPhase")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(observedPhase).To(Equal("ready"))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
	})

	It("should list all resources when external collection has empty selector", func(ctx SpecContext) {
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

		By("creating ConfigMaps in the namespace")
		cm1 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "config-alpha",
				Namespace: namespace,
				Labels:    map[string]string{"empty-selector-test": "alpha"},
			},
			Data: map[string]string{"key": "value1"},
		}
		cm2 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "config-beta",
				Namespace: namespace,
				Labels:    map[string]string{"empty-selector-test": "beta"},
			},
			Data: map[string]string{"key": "value2"},
		}
		Expect(env.Client.Create(ctx, cm1)).To(Succeed())
		Expect(env.Client.Create(ctx, cm2)).To(Succeed())

		By("creating RGD with external collection ref using empty selector (match all)")
		rgd := generator.NewResourceGraphDefinition("test-extcoll-empty-sel",
			generator.WithSchema(
				"TestExtCollEmptySel", "v1alpha1",
				map[string]interface{}{},
				map[string]interface{}{
					"configCount": "${string(size(allconfigs))}",
					"teamValues": "${allconfigs.filter(" +
						"c, c.metadata.name == 'config-alpha' || c.metadata.name == 'config-beta'" +
						").sortBy(c, c.metadata.name).map(c, c.data.key).join(\",\")}",
				},
			),
			generator.WithExternalRef("allconfigs", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Selector: krov1alpha1.MustJSON(metav1.LabelSelector{}),
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("waiting for RGD to become active")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating the instance")
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExtCollEmptySel",
				"metadata": map[string]interface{}{
					"name":      "test-empty-selector",
					"namespace": namespace,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("waiting for instance to become ACTIVE with configCount >= 2 (all ConfigMaps matched)")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

			configCount, found, err := unstructured.NestedString(instance.Object, "status", "configCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			// At least our 2 ConfigMaps should be matched (there may be
			// additional ones like kube-root-ca.crt in the namespace).
			count := 0
			_, err = fmt.Sscanf(configCount, "%d", &count)
			g.Expect(err).To(Succeed())
			g.Expect(count).To(BeNumerically(">=", 2))

			teamValues, found, err := unstructured.NestedString(instance.Object, "status", "teamValues")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(teamValues).To(Equal("value1,value2"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("patching a matched ConfigMap and ensuring the collection status reacts via watch")
		Expect(env.Client.Get(ctx, types.NamespacedName{
			Name:      cm2.Name,
			Namespace: cm2.Namespace,
		}, cm2)).To(Succeed())
		originalCM := cm2.DeepCopy()
		cm2.Data["key"] = "value2-updated"
		Expect(env.Client.Patch(ctx, cm2, client.MergeFrom(originalCM))).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			teamValues, found, err := unstructured.NestedString(instance.Object, "status", "teamValues")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(teamValues).To(Equal("value1,value2-updated"))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
	})

	It("should resolve an external collection selector from instance spec", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))
		uniqueLabel := fmt.Sprintf("instance-selector-%s", rand.String(5))

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		By("creating ConfigMaps that will be matched by an instance-supplied selector")
		cm1 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "selector-instance-alpha",
				Namespace: namespace,
				Labels:    map[string]string{"issue-1228": uniqueLabel},
			},
			Data: map[string]string{"key": "value1"},
		}
		cm2 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "selector-instance-beta",
				Namespace: namespace,
				Labels:    map[string]string{"issue-1228": uniqueLabel},
			},
			Data: map[string]string{"key": "value2"},
		}
		Expect(env.Client.Create(ctx, cm1)).To(Succeed())
		Expect(env.Client.Create(ctx, cm2)).To(Succeed())

		By("creating an RGD whose external collection selector comes from instance spec")
		rgd := generator.NewResourceGraphDefinition("test-extcoll-instance-selector",
			generator.WithSchema(
				"TestExtCollInstanceSelector", "v1alpha1",
				map[string]any{
					"selector": "object",
				},
				map[string]any{
					"configCount": "${string(size(allconfigs))}",
				},
			),
			generator.WithExternalRef("allconfigs", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Selector: krov1alpha1.MustJSON("${schema.spec.selector}"),
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("waiting for the RGD to become active")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating an instance with a full selector object in spec")
		instance := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExtCollInstanceSelector",
				"metadata": map[string]any{
					"name":      "test-instance-selector",
					"namespace": namespace,
				},
				"spec": map[string]any{
					"selector": map[string]any{
						"matchLabels": map[string]any{"issue-1228": uniqueLabel},
					},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("ensuring the instance resolves the external collection")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

			configCount, found, err := unstructured.NestedString(instance.Object, "status", "configCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(configCount).To(Equal("2"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should list external collection resources across all namespaces when namespace is omitted", func(ctx SpecContext) {
		// This test verifies that an external collection without an explicit
		// namespace lists resources across ALL namespaces, not just the
		// instance's namespace.
		uniqueLabel := fmt.Sprintf("cross-ns-%s", rand.String(5))

		nsA := fmt.Sprintf("test-ns-a-%s", rand.String(5))
		nsB := fmt.Sprintf("test-ns-b-%s", rand.String(5))
		instanceNS := fmt.Sprintf("test-inst-%s", rand.String(5))

		for _, name := range []string{nsA, nsB, instanceNS} {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: name},
			}
			Expect(env.Client.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(env.Client.Delete(ctx, ns)).To(Succeed())
			})
		}

		By("creating ConfigMaps with the same label in three different namespaces")
		for i, ns := range []string{nsA, nsB, instanceNS} {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("cm-%d", i),
					Namespace: ns,
					Labels:    map[string]string{"suite": uniqueLabel},
				},
				Data: map[string]string{"from": ns},
			}
			Expect(env.Client.Create(ctx, cm)).To(Succeed())
		}

		By("creating RGD with external collection using label selector and NO namespace")
		rgdName := fmt.Sprintf("test-crossns-%s", rand.String(5))
		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				"TestCrossNsExtColl", "v1alpha1",
				map[string]interface{}{},
				map[string]interface{}{
					"configCount": "${string(size(allconfigs))}",
				},
			),
			generator.WithExternalRef("allconfigs", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					// No Namespace — should list across all namespaces.
					Selector: krov1alpha1.MustJSON(metav1.LabelSelector{
						MatchLabels: map[string]string{"suite": uniqueLabel},
					}),
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("waiting for RGD to become active")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating the instance in its own namespace")
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestCrossNsExtColl",
				"metadata": map[string]interface{}{
					"name":      "cross-ns-test",
					"namespace": instanceNS,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("asserting instance sees ConfigMaps from ALL three namespaces (configCount=3)")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: instanceNS,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

			configCount, found, err := unstructured.NestedString(instance.Object, "status", "configCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(configCount).To(Equal("3"),
				"external collection without namespace should list across all namespaces, not just the instance namespace")
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
	})
})
