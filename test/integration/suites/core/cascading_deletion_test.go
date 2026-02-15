// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package core_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

const testGroup = "test.kro.bug"

var _ = Describe("Cascading Deletion of Nested RGDs", func() {
	var ns *corev1.Namespace

	BeforeEach(func(ctx SpecContext) {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("test-%s", rand.String(5)),
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, ns)).To(Succeed())
	})

	It("should cascade delete all nested RGD instances and their resources", func(ctx SpecContext) {
		// Create 3-level nested RGD hierarchy
		level3RGD := createLevel3RGD()
		level2RGD := createLevel2RGD()
		level1RGD := createLevel1RGD()

		// Create and wait for all RGDs to be active
		for _, rgd := range []*krov1alpha1.ResourceGraphDefinition{level3RGD, level2RGD, level1RGD} {
			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			waitForRGDActive(ctx, rgd.Name)
		}

		// Create Level1Resource instance
		By("Creating Level1Resource instance")
		level1Instance := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": testGroup + "/v1alpha1",
				"kind":       "Level1Resource",
				"metadata": map[string]any{
					"name":      "test-app",
					"namespace": ns.Name,
				},
				"spec": map[string]any{
					"applicationName": "my-test-app",
					"environment":     "production",
				},
			},
		}
		Expect(env.Client.Create(ctx, level1Instance)).To(Succeed())

		// Verify all instances reach ACTIVE state
		waitForInstanceActive(ctx, ns.Name, "test-app", level1Instance)
		level2Instance := waitForNestedInstanceActive(ctx, ns.Name, "test-app-level2", "Level2Resource")
		level3Instance := waitForNestedInstanceActive(ctx, ns.Name, "test-app-level2-level3", "Level3Resource")

		// Verify all child resources exist
		verifyResourceExists(ctx, ns.Name, "test-app-config", &corev1.ConfigMap{})
		verifyResourceExists(ctx, ns.Name, "test-app-level2-config", &corev1.ConfigMap{})
		verifyResourceExists(ctx, ns.Name, "test-app-level2-level3-config", &corev1.ConfigMap{})
		verifyResourceExists(ctx, ns.Name, "test-app-level2-level3-secret", &corev1.Secret{})
		verifyResourceExists(ctx, ns.Name, "test-app-level2-svc", &corev1.Service{})
		verifyResourceExists(ctx, ns.Name, "test-app-deployment", &appsv1.Deployment{})

		// Delete Level1Resource instance
		By("Deleting Level1Resource instance")
		Expect(env.Client.Delete(ctx, level1Instance)).To(Succeed())

		// Verify cascading deletion of all resources (critical test for bug #985)
		waitForResourceDeleted(ctx, ns.Name, "test-app", level1Instance)
		waitForResourceDeleted(ctx, ns.Name, "test-app-level2", level2Instance)        // Was orphaned in bug
		waitForResourceDeleted(ctx, ns.Name, "test-app-level2-level3", level3Instance) // Was orphaned in bug
		waitForResourceDeleted(ctx, ns.Name, "test-app-config", &corev1.ConfigMap{})
		waitForResourceDeleted(ctx, ns.Name, "test-app-level2-config", &corev1.ConfigMap{})
		waitForResourceDeleted(ctx, ns.Name, "test-app-level2-level3-config", &corev1.ConfigMap{})
		waitForResourceDeleted(ctx, ns.Name, "test-app-level2-level3-secret", &corev1.Secret{})
		waitForResourceDeleted(ctx, ns.Name, "test-app-level2-svc", &corev1.Service{})
		waitForResourceDeleted(ctx, ns.Name, "test-app-deployment", &appsv1.Deployment{})

		// Cleanup RGDs
		Expect(env.Client.Delete(ctx, level1RGD)).To(Succeed())
		Expect(env.Client.Delete(ctx, level2RGD)).To(Succeed())
		Expect(env.Client.Delete(ctx, level3RGD)).To(Succeed())
	})
})

// Helper functions to reduce test verbosity

func createLevel3RGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition("level3-resource-rgd",
		generator.WithSchema("Level3Resource", "v1alpha1",
			map[string]any{"name": "string", "message": "string"},
			map[string]any{},
		),
		generator.WithResource("configMap", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-config",
				"namespace": "${schema.metadata.namespace}",
			},
			"data": map[string]any{
				"name":    "${schema.spec.name}",
				"message": "${schema.spec.message}",
				"level":   "3",
			},
		}, nil, nil),
		generator.WithResource("secret", map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-secret",
				"namespace": "${schema.metadata.namespace}",
			},
			"type": "Opaque",
			"stringData": map[string]any{
				"level":    "3",
				"resource": "${schema.spec.name}",
			},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = testGroup
	return rgd
}

func createLevel2RGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition("level2-resource-rgd",
		generator.WithSchema("Level2Resource", "v1alpha1",
			map[string]any{"name": "string", "description": "string"},
			map[string]any{},
		),
		generator.WithResource("configMap", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-config",
				"namespace": "${schema.metadata.namespace}",
			},
			"data": map[string]any{
				"name":        "${schema.spec.name}",
				"description": "${schema.spec.description}",
				"level":       "2",
			},
		}, nil, nil),
		generator.WithResource("level3Resource", map[string]any{
			"apiVersion": testGroup + "/v1alpha1",
			"kind":       "Level3Resource",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-level3",
				"namespace": "${schema.metadata.namespace}",
			},
			"spec": map[string]any{
				"name":    "${schema.spec.name}-level3",
				"message": "Created by Level2Resource: ${schema.spec.name}",
			},
		}, nil, nil),
		generator.WithResource("service", map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-svc",
				"namespace": "${schema.metadata.namespace}",
			},
			"spec": map[string]any{
				"type": "ClusterIP",
				"ports": []any{
					map[string]any{"port": 80, "targetPort": 8080},
				},
				"selector": map[string]any{"app": "${schema.spec.name}"},
			},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = testGroup
	return rgd
}

func createLevel1RGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition("level1-resource-rgd",
		generator.WithSchema("Level1Resource", "v1alpha1",
			map[string]any{"applicationName": "string", "environment": "string"},
			map[string]any{},
		),
		generator.WithResource("configMap", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-config",
				"namespace": "${schema.metadata.namespace}",
			},
			"data": map[string]any{
				"applicationName": "${schema.spec.applicationName}",
				"environment":     "${schema.spec.environment}",
				"level":           "1",
			},
		}, nil, nil),
		generator.WithResource("level2Resource", map[string]any{
			"apiVersion": testGroup + "/v1alpha1",
			"kind":       "Level2Resource",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-level2",
				"namespace": "${schema.metadata.namespace}",
			},
			"spec": map[string]any{
				"name":        "${schema.spec.applicationName}-level2",
				"description": "Created by Level1Resource in ${schema.spec.environment}",
			},
		}, nil, nil),
		generator.WithResource("deployment", map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-deployment",
				"namespace": "${schema.metadata.namespace}",
			},
			"spec": map[string]any{
				"replicas": 1,
				"selector": map[string]any{
					"matchLabels": map[string]any{"app": "${schema.spec.applicationName}"},
				},
				"template": map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{"app": "${schema.spec.applicationName}"},
					},
					"spec": map[string]any{
						"containers": []any{
							map[string]any{
								"name":  "app",
								"image": "nginx:latest",
								"ports": []any{
									map[string]any{"containerPort": 8080},
								},
							},
						},
					},
				},
			},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = testGroup
	return rgd
}

func waitForRGDActive(ctx SpecContext, name string) {
	Eventually(func(g Gomega, ctx SpecContext) {
		rgd := &krov1alpha1.ResourceGraphDefinition{}
		err := env.Client.Get(ctx, types.NamespacedName{Name: name}, rgd)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
	}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

func waitForInstanceActive(ctx SpecContext, namespace, name string, instance *unstructured.Unstructured) {
	Eventually(func(g Gomega, ctx SpecContext) {
		err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
		g.Expect(err).ToNot(HaveOccurred())
		state, found, err := unstructured.NestedString(instance.Object, "status", "state")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(state).To(Equal("ACTIVE"))
	}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

func waitForNestedInstanceActive(ctx SpecContext, namespace, name, kind string) *unstructured.Unstructured {
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": testGroup + "/v1alpha1",
			"kind":       kind,
		},
	}
	waitForInstanceActive(ctx, namespace, name, instance)
	return instance
}

func verifyResourceExists(ctx SpecContext, namespace, name string, obj client.Object) {
	By(fmt.Sprintf("Verifying %s exists", name))
	nsName := types.NamespacedName{Name: name, Namespace: namespace}
	Expect(env.Client.Get(ctx, nsName, obj)).To(Succeed())
}

func waitForResourceDeleted(ctx SpecContext, namespace, name string, obj client.Object) {
	Eventually(func(g Gomega, ctx SpecContext) {
		nsName := types.NamespacedName{Name: name, Namespace: namespace}
		err := env.Client.Get(ctx, nsName, obj)
		g.Expect(err).To(MatchError(errors.IsNotFound, fmt.Sprintf("%s should be deleted", name)))
	}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}
