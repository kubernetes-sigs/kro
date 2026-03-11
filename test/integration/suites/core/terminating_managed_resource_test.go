// Copyright 2026 The Kubernetes Authors.
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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Terminating Managed Resources", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should not create downstream resources while a managed resource is terminating", func(ctx SpecContext) {
		const (
			rgdName           = "test-terminating-managed-resource"
			instanceName      = "test-terminating"
			blockingFinalizer = "tests.kro.run/block-delete"
		)

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				"TerminatingManagedResource", "v1alpha1",
				map[string]interface{}{
					"name":             "string",
					"createDeployment": "boolean",
				},
				nil,
			),
			generator.WithResource("config", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-config",
				},
				"data": map[string]interface{}{
					"value": "active",
				},
			}, nil, nil),
			// The Deployment depends on config data, but it is only desired after createDeployment flips to true.
			generator.WithResource("deployment", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
				},
				"spec": map[string]interface{}{
					"replicas": 1,
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "${schema.spec.name}",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "${schema.spec.name}",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "nginx",
									"image": "nginx",
									"env": []interface{}{
										map[string]interface{}{
											"name":  "CONFIG_VALUE",
											"value": "${config.data.value}",
										},
									},
								},
							},
						},
					},
				},
			}, nil, []string{"${schema.spec.createDeployment}"}),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, rgd)
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TerminatingManagedResource",
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":             instanceName,
					"createDeployment": false,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, instance)
		})

		configMapName := instanceName + "-config"
		configMapKey := types.NamespacedName{Name: configMapName, Namespace: namespace}
		deploymentKey := types.NamespacedName{Name: instanceName, Namespace: namespace}

		// Start with only the upstream managed resource desired.
		configMap := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, configMapKey, configMap)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMap.Data["value"]).To(Equal("active"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, deploymentKey, &appsv1.Deployment{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "deployment should not be created before it becomes desired"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Hold the ConfigMap in a terminating state so reconcile sees a live object with deletionTimestamp set.
		DeferCleanup(func(ctx SpecContext) {
			current := &corev1.ConfigMap{}
			if err := env.Client.Get(ctx, configMapKey, current); err != nil {
				return
			}
			current.Finalizers = nil
			_ = env.Client.Update(ctx, current)
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, configMapKey, configMap)
			g.Expect(err).ToNot(HaveOccurred())
			configMap.Finalizers = append(configMap.Finalizers, blockingFinalizer)
			g.Expect(env.Client.Update(ctx, configMap)).To(Succeed())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, configMap)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			current := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, configMapKey, current)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(current.GetDeletionTimestamp()).ToNot(BeNil())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Make the downstream Deployment newly desired while the upstream ConfigMap is still terminating.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			spec, found, err := unstructured.NestedMap(instance.Object, "spec")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			spec["createDeployment"] = true
			g.Expect(unstructured.SetNestedMap(instance.Object, spec, "spec")).To(Succeed())
			g.Expect(env.Client.Update(ctx, instance)).To(Succeed())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("IN_PROGRESS"))

			statusConditions, found, err := unstructured.NestedSlice(instance.Object, "status", "conditions")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())

			var resourcesReadyCondition map[string]interface{}
			for _, condInterface := range statusConditions {
				if cond, ok := condInterface.(map[string]interface{}); ok && cond["type"] == ctrlinstance.ResourcesReady {
					resourcesReadyCondition = cond
					break
				}
			}

			g.Expect(resourcesReadyCondition).ToNot(BeNil())
			g.Expect(resourcesReadyCondition["status"]).To(Equal("False"))
			g.Expect(resourcesReadyCondition["reason"]).To(Equal("ResourceDeleting"))
			msg, _ := resourcesReadyCondition["message"].(string)
			g.Expect(msg).To(ContainSubstring(fmt.Sprintf(`resource "%s/%s"`, namespace, configMapName)))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, deploymentKey, &appsv1.Deployment{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "deployment should not be created while configmap is terminating"))
		}, 8*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Once deletion completes, the controller should recreate the ConfigMap and then continue with the Deployment.
		Expect(env.Client.Get(ctx, configMapKey, configMap)).To(Succeed())
		configMap.Finalizers = nil
		Expect(env.Client.Update(ctx, configMap)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, configMapKey, &corev1.ConfigMap{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "terminating configmap should be deleted after finalizer removal"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			current := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, configMapKey, current)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(current.GetDeletionTimestamp()).To(BeNil())
			g.Expect(current.Data["value"]).To(Equal("active"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			current := &appsv1.Deployment{}
			err := env.Client.Get(ctx, deploymentKey, current)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(current.Spec.Template.Spec.Containers).ToNot(BeEmpty())
			g.Expect(current.Spec.Template.Spec.Containers[0].Env).ToNot(BeEmpty())
			g.Expect(current.Spec.Template.Spec.Containers[0].Env[0].Value).To(Equal("active"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
