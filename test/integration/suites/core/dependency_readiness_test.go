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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Dependency Readiness", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should wait for all dependencies to be ready before creating dependent resource", func(ctx SpecContext) {
		// This test creates a resource graph with:
		// - ConfigMap A (no dependencies, has readyWhen condition)
		// - ConfigMap B (no dependencies, has readyWhen condition)
		// - Deployment (depends on both ConfigMap A and B)
		// The deployment should only be created after both configmaps satisfy their readyWhen conditions

		rgd := generator.NewResourceGraphDefinition("test-dependency-readiness",
			generator.WithSchema(
				"TestDependencyReadiness", "v1alpha1",
				map[string]any{
					"name": "string",
					"configA": map[string]any{
						"data":  "string",
						"ready": "boolean | default=false",
					},
					"configB": map[string]any{
						"data":  "string",
						"ready": "boolean | default=false",
					},
					"replicas": "integer | default=1",
				},
				nil,
			),
			// ConfigMap A - no dependencies, has readyWhen
			generator.WithResource("configmapA", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-config-a",
				},
				"data": map[string]any{
					"value": "${schema.spec.configA.data}",
					"ready": "${string(schema.spec.configA.ready)}",
				},
			}, []string{"${configmapA.data.?ready.orValue(\"false\") == \"true\"}"}, nil),
			// ConfigMap B - no dependencies, has readyWhen
			generator.WithResource("configmapB", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-config-b",
				},
				"data": map[string]any{
					"value": "${schema.spec.configB.data}",
					"ready": "${string(schema.spec.configB.ready)}",
				},
			}, []string{"${configmapB.data.?ready.orValue(\"false\") == \"true\"}"}, nil),
			// Deployment - depends on both configmaps
			generator.WithResource("deployment", map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]any{
					"name": "${schema.spec.name}",
				},
				"spec": map[string]any{
					"replicas": "${schema.spec.replicas}",
					"selector": map[string]any{
						"matchLabels": map[string]any{
							"app": "test",
						},
					},
					"template": map[string]any{
						"metadata": map[string]any{
							"labels": map[string]any{
								"app": "test",
							},
						},
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name":  "nginx",
									"image": "nginx",
									"env": []any{
										map[string]any{
											"name":  "CONFIG_A",
											"value": "${configmapA.data.?value.orValue(\"\")}",
										},
										map[string]any{
											"name":  "CONFIG_B",
											"value": "${configmapB.data.?value.orValue(\"\")}",
										},
									},
								},
							},
						},
					},
				},
			}, nil, nil),
		)

		// Create ResourceGraphDefinition
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("TestDependencyReadiness"))
			g.Expect(createdRGD.Spec.Schema.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(3))

			// Verify topological order (configmaps should come before deployment)
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(3))
			g.Expect(createdRGD.Status.TopologicalOrder[2]).To(Equal("deployment"))

			// Verify ready condition
			g.Expect(createdRGD.Status.Conditions).ShouldNot(BeEmpty())
			var readyCondition krov1alpha1.Condition
			for _, cond := range createdRGD.Status.Conditions {
				if cond.Type == resourcegraphdefinition.Ready {
					readyCondition = cond
				}
			}
			g.Expect(readyCondition).ToNot(BeNil())
			g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(readyCondition.ObservedGeneration).To(Equal(createdRGD.Generation))

			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		instanceName := "test-dep-readiness"
		// Create instance with both configmaps NOT ready initially
		instance := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestDependencyReadiness",
				"metadata": map[string]any{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]any{
					"name": instanceName,
					"configA": map[string]any{
						"data":  "valueA",
						"ready": false,
					},
					"configB": map[string]any{
						"data":  "valueB",
						"ready": false,
					},
					"replicas": 1,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Check if instance is created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Verify ConfigMaps are created
		configMapA := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-config-a",
				Namespace: namespace,
			}, configMapA)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMapA.Data["value"]).To(Equal("valueA"))
			g.Expect(configMapA.Data["ready"]).To(Equal("false"))
		}, 20*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		configMapB := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-config-b",
				Namespace: namespace,
			}, configMapB)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMapB.Data["value"]).To(Equal("valueB"))
			g.Expect(configMapB.Data["ready"]).To(Equal("false"))
		}, 20*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Verify Deployment is NOT created yet (dependencies not ready)
		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, &appsv1.Deployment{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "deployment should not be created yet"))
		}, 7*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Verify instance state is IN_PROGRESS
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("IN_PROGRESS"))
		}, 20*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Update instance spec to set ConfigMap A to ready
		Eventually(func(g Gomega, ctx SpecContext) {
			// Get fresh copy to avoid conflicts
			freshInstance := &unstructured.Unstructured{}
			freshInstance.SetGroupVersionKind(instance.GroupVersionKind())
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, freshInstance)
			g.Expect(err).ToNot(HaveOccurred())

			err = unstructured.SetNestedField(freshInstance.Object, true, "spec", "configA", "ready")
			g.Expect(err).ToNot(HaveOccurred())

			err = env.Client.Update(ctx, freshInstance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Verify deployment is still NOT created (ConfigMap B not ready yet)
		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, &appsv1.Deployment{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "deployment should still not be created"))
		}, 7*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Update instance spec to set ConfigMap B to ready
		Eventually(func(g Gomega, ctx SpecContext) {
			// Get fresh copy to avoid conflicts
			freshInstance := &unstructured.Unstructured{}
			freshInstance.SetGroupVersionKind(instance.GroupVersionKind())
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, freshInstance)
			g.Expect(err).ToNot(HaveOccurred())

			err = unstructured.SetNestedField(freshInstance.Object, true, "spec", "configB", "ready")
			g.Expect(err).ToNot(HaveOccurred())

			err = env.Client.Update(ctx, freshInstance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Now verify Deployment IS created (all dependencies are ready)
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment specs
			g.Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))

			// Verify environment variables from configmaps
			envVars := deployment.Spec.Template.Spec.Containers[0].Env
			g.Expect(envVars).To(HaveLen(2))

			var foundConfigA, foundConfigB bool
			for _, env := range envVars {
				if env.Name == "CONFIG_A" && env.Value == "valueA" {
					foundConfigA = true
				}
				if env.Name == "CONFIG_B" && env.Value == "valueB" {
					foundConfigB = true
				}
			}
			g.Expect(foundConfigA).To(BeTrue(), "CONFIG_A should be set from configMapA")
			g.Expect(foundConfigB).To(BeTrue(), "CONFIG_B should be set from configMapB")
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Verify instance state becomes ACTIVE once all resources are synced
		waitForInstanceActive(ctx, namespace, instanceName, instance)

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
	})

	It("should block dependent resources until readyWhen conditions are satisfied", func(ctx SpecContext) {
		instanceName := "test-jobs-instance"
		rgd := generator.NewResourceGraphDefinition("test-jobs",
			generator.WithSchema(
				"TestJobs", "v1alpha1",
				map[string]any{
					"name": "string",
				},
				nil,
			),
			generator.WithResource("job1", map[string]any{
				"apiVersion": "batch/v1",
				"kind":       "Job",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-job1",
				},
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name":    "sleeper",
									"image":   "busybox",
									"command": []any{"sh", "-c", "echo 'Job 1 starting' && sleep 5 && echo 'Job 1 complete'"},
								},
							},
							"restartPolicy": "Never",
						},
					},
				},
			}, []string{"${job1.status.?completionTime.orValue(null) != null}"}, nil),
			generator.WithResource("job2", map[string]any{
				"apiVersion": "batch/v1",
				"kind":       "Job",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-job2",
					"annotations": map[string]any{
						"depends-on": "${job1.metadata.name}",
					},
				},
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name":    "sleeper",
									"image":   "busybox",
									"command": []any{"sh", "-c", "echo 'Job 2 starting' && sleep 1 && echo 'Job 2 complete'"},
								},
							},
							"restartPolicy": "Never",
						},
					},
				},
			}, []string{"${job2.status.?completionTime.orValue(null) != null}"}, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			var readyCondition krov1alpha1.Condition
			for _, cond := range createdRGD.Status.Conditions {
				if cond.Type == resourcegraphdefinition.Ready {
					readyCondition = cond
				}
			}
			g.Expect(readyCondition).ToNot(BeNil())
			g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		instance := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestJobs",
				"metadata": map[string]any{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]any{
					"name": "test-jobs",
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		job1 := &batchv1.Job{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-jobs-job1",
				Namespace: namespace,
			}, job1)
			g.Expect(err).ToNot(HaveOccurred())
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-jobs-job2",
				Namespace: namespace,
			}, &batchv1.Job{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "job2 should not be created while job1 is running"))
		}, 7*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		now := metav1.Now()
		job1.Status.Conditions = append(job1.Status.Conditions,
			batchv1.JobCondition{
				Type:               batchv1.JobSuccessCriteriaMet,
				Status:             corev1.ConditionTrue,
				LastProbeTime:      now,
				LastTransitionTime: now,
				Reason:             "JobSuccessCriteriaMet",
				Message:            "Job has successfully completed all of its specified success criteria",
			},
			batchv1.JobCondition{
				Type:               batchv1.JobComplete,
				Status:             corev1.ConditionTrue,
				LastProbeTime:      now,
				LastTransitionTime: now,
				Reason:             batchv1.JobReasonCompletionsReached,
				Message:            "Job has reached the specified number of completions",
			})
		job1.Status.StartTime = &now
		job1.Status.CompletionTime = &now
		job1.Status.Succeeded = 1
		Expect(env.Client.Status().Update(ctx, job1)).To(Succeed())

		job2 := &batchv1.Job{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-jobs-job2",
				Namespace: namespace,
			}, job2)
			g.Expect(err).ToNot(HaveOccurred())
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		now = metav1.Now()
		job2.Status.Conditions = append(job1.Status.Conditions, batchv1.JobCondition{
			Type:               batchv1.JobComplete,
			Status:             corev1.ConditionTrue,
			LastProbeTime:      now,
			LastTransitionTime: now,
			Reason:             batchv1.JobReasonCompletionsReached,
			Message:            "Job has reached the specified number of completions",
		})
		job2.Status.StartTime = &now
		job2.Status.CompletionTime = &now
		job2.Status.Succeeded = 1
		Expect(env.Client.Status().Update(ctx, job2)).To(Succeed())

		waitForInstanceActive(ctx, namespace, instanceName, instance)
	}, SpecTimeout(120*time.Second))
})
