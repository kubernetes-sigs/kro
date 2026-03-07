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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("GraphRevision Lifecycle", func() {

	It("should create a GraphRevision when an RGD is created", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gv-create-%s", rand.String(5))
		kind := fmt.Sprintf("GvCreate%s", rand.String(5))
		rgd := simpleDeploymentRGD(rgdName, kind)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		// Wait for RGD to become Active
		activeRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, activeRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(activeRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify a GraphRevision was created
		var gvs []krov1alpha1.GraphRevision
		Eventually(func(g Gomega) {
			gvs = listGraphRevisions(ctx, rgdName)
			g.Expect(gvs).To(HaveLen(1))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		gv := gvs[0]

		// Verify spec fields
		Expect(gv.Spec.ResourceGraphDefinitionName).To(Equal(rgdName))
		Expect(gv.Spec.Revision).To(Equal(int64(1)))
		Expect(gv.Spec.SpecHash).ToNot(BeEmpty())
		Expect(gv.Spec.DefinitionSpec.Schema).ToNot(BeNil())
		Expect(gv.Spec.DefinitionSpec.Schema.Kind).To(Equal(kind))

		// Verify naming convention
		Expect(gv.Name).To(Equal(fmt.Sprintf("%s-r1", rgdName)))

		// Verify labels
		Expect(gv.Labels[metadata.ResourceGraphDefinitionNameLabel]).To(Equal(rgdName))
		Expect(gv.Labels[metadata.ResourceGraphDefinitionIDLabel]).To(Equal(string(activeRGD.UID)))

		// Verify OwnerReference
		Expect(gv.OwnerReferences).To(HaveLen(1))
		Expect(gv.OwnerReferences[0].Name).To(Equal(rgdName))
		Expect(gv.OwnerReferences[0].UID).To(Equal(activeRGD.UID))

		// Verify GV status conditions
		graphVerified := findGVCondition(gv.Status.Conditions, krov1alpha1.GraphRevisionConditionTypeGraphVerified)
		Expect(graphVerified).ToNot(BeNil())
		Expect(graphVerified.Status).To(Equal(metav1.ConditionTrue))

		ready := findGVCondition(gv.Status.Conditions, krov1alpha1.GraphRevisionConditionTypeReady)
		Expect(ready).ToNot(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))

		// Verify topological order is populated
		Expect(gv.Status.TopologicalOrder).ToNot(BeEmpty())
		Expect(gv.Status.TopologicalOrder).To(ContainElement("deployment"))

		// Verify RGD status references the GV
		Expect(activeRGD.Status.LatestObservedGV).ToNot(BeNil())
		Expect(activeRGD.Status.LatestObservedGV.Name).To(Equal(gv.Name))
		Expect(activeRGD.Status.LatestObservedGV.Revision).To(Equal(int64(1)))
		Expect(activeRGD.Status.LatestActiveGV).ToNot(BeNil())
		Expect(activeRGD.Status.LatestActiveGV.Revision).To(Equal(int64(1)))
		Expect(activeRGD.Status.LastIssuedRevision).To(Equal(int64(1)))
	})

	It("should create GraphRevision with correct topological order for multi-resource graphs", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gv-topo-%s", rand.String(5))
		kind := fmt.Sprintf("GvTopo%s", rand.String(5))

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				kind, "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "cm-${schema.spec.name}",
				},
				"data": map[string]interface{}{
					"key": "value",
				},
			}, nil, nil),
			generator.WithResource("deployment", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "deploy-${schema.spec.name}",
				},
				"spec": map[string]interface{}{
					"replicas": 1,
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "test",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "test",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "app",
									"image": "nginx",
									"env": []interface{}{
										map[string]interface{}{
											"name":  "CM_NAME",
											"value": "${configmap.metadata.name}",
										},
									},
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

		waitForRGDActive(ctx, rgdName)

		var gvs []krov1alpha1.GraphRevision
		Eventually(func(g Gomega) {
			gvs = listGraphRevisions(ctx, rgdName)
			g.Expect(gvs).To(HaveLen(1))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		gv := gvs[0]
		// configmap should come before deployment in topological order
		Expect(gv.Status.TopologicalOrder).To(HaveLen(2))
		cmIdx := -1
		deployIdx := -1
		for i, id := range gv.Status.TopologicalOrder {
			if id == "configmap" {
				cmIdx = i
			}
			if id == "deployment" {
				deployIdx = i
			}
		}
		Expect(cmIdx).To(BeNumerically("<", deployIdx),
			"configmap should appear before deployment in topological order")

		// Verify dependency info in status
		Expect(gv.Status.Resources).ToNot(BeEmpty())
		var deployRes *krov1alpha1.ResourceInformation
		for i := range gv.Status.Resources {
			if gv.Status.Resources[i].ID == "deployment" {
				deployRes = &gv.Status.Resources[i]
			}
		}
		Expect(deployRes).ToNot(BeNil())
		Expect(deployRes.Dependencies).To(HaveLen(1))
		Expect(deployRes.Dependencies[0].ID).To(Equal("configmap"))
	})
})

var _ = Describe("GraphRevision Hash Deduplication", func() {

	It("should not create a new revision when the RGD spec has not changed", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gv-dedup-%s", rand.String(5))
		kind := fmt.Sprintf("GvDedup%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActive(ctx, rgdName)

		// Wait for exactly one revision
		Eventually(func(g Gomega) {
			gvs := listGraphRevisions(ctx, rgdName)
			g.Expect(gvs).To(HaveLen(1))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())

			if fresh.Annotations == nil {
				fresh.Annotations = map[string]string{}
			}
			fresh.Annotations["test-trigger"] = "re-reconcile"
			err = env.Client.Update(ctx, fresh)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Wait a few seconds then verify no new revision was created
		Consistently(func(g Gomega) {
			gvs := listGraphRevisions(ctx, rgdName)
			g.Expect(gvs).To(HaveLen(1))
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})

var _ = Describe("GraphRevision Bumping", func() {

	It("should create a new revision when the RGD spec changes", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gv-bump-%s", rand.String(5))
		kind := fmt.Sprintf("GvBump%s", rand.String(5))

		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				kind, "v1alpha1",
				map[string]interface{}{
					"replicas": "integer | default=1",
					"image":    "string | default=nginx:latest",
				},
				nil,
			),
			generator.WithResource("deployment", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "deploy-${schema.metadata.name}",
				},
				"spec": map[string]interface{}{
					"replicas": "${schema.spec.replicas}",
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "test",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "test",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "app",
									"image": "${schema.spec.image}",
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

		waitForRGDActive(ctx, rgdName)

		// Wait for revision 1
		var firstHash string
		Eventually(func(g Gomega) {
			gvs := listGraphRevisions(ctx, rgdName)
			g.Expect(gvs).To(HaveLen(1))
			g.Expect(gvs[0].Spec.Revision).To(Equal(int64(1)))
			firstHash = gvs[0].Spec.SpecHash
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Update the RGD spec: change the schema default to produce a different hash
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())

			fresh.Spec.Schema.Spec.Raw = []byte(`{"replicas":"integer | default=3","image":"string | default=nginx:1.25"}`)
			err = env.Client.Update(ctx, fresh)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify revision 2 was created and RGD status is updated
		Eventually(func(g Gomega) {
			gvs := listGraphRevisions(ctx, rgdName)
			g.Expect(len(gvs)).To(BeNumerically(">=", 2))

			revisionMap := map[int64]krov1alpha1.GraphRevision{}
			for _, gv := range gvs {
				revisionMap[gv.Spec.Revision] = gv
			}

			// Revision 1 should still exist
			rev1, ok := revisionMap[1]
			g.Expect(ok).To(BeTrue(), "revision 1 should still exist")
			g.Expect(rev1.Spec.SpecHash).To(Equal(firstHash))

			// Revision 2 should exist with a different hash
			rev2, ok := revisionMap[2]
			g.Expect(ok).To(BeTrue(), "revision 2 should exist")
			g.Expect(rev2.Spec.SpecHash).ToNot(Equal(firstHash))
			g.Expect(rev2.Spec.Revision).To(Equal(int64(2)))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// RGD status should reference revision 2
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.LatestObservedGV).ToNot(BeNil())
			g.Expect(fresh.Status.LatestObservedGV.Revision).To(Equal(int64(2)))
			g.Expect(fresh.Status.LastIssuedRevision).To(BeNumerically(">=", int64(2)))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should produce monotonically increasing revision numbers across multiple updates", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gv-mono-%s", rand.String(5))
		kind := fmt.Sprintf("GvMono%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActive(ctx, rgdName)

		// Wait for initial revision
		Eventually(func(g Gomega) {
			gvs := listGraphRevisions(ctx, rgdName)
			g.Expect(gvs).To(HaveLen(1))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Perform 3 spec changes in sequence
		for i := 2; i <= 4; i++ {
			expectedRevision := int64(i)
			Eventually(func(g Gomega) {
				fresh := &krov1alpha1.ResourceGraphDefinition{}
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
				g.Expect(err).ToNot(HaveOccurred())

				// Change the default value in schema to produce a different hash
				fresh.Spec.Schema.Spec.Raw = []byte(fmt.Sprintf(`{"data":"string | default=value-%d"}`, i))
				err = env.Client.Update(ctx, fresh)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			// Wait for the new revision
			Eventually(func(g Gomega) {
				gvs := listGraphRevisions(ctx, rgdName)
				maxRev := int64(0)
				for _, gv := range gvs {
					if gv.Spec.Revision > maxRev {
						maxRev = gv.Spec.Revision
					}
				}
				g.Expect(maxRev).To(Equal(expectedRevision))
			}, 20*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())
		}

		// Verify all revisions are monotonically increasing
		gvs := listGraphRevisions(ctx, rgdName)
		Expect(len(gvs)).To(BeNumerically(">=", 4))

		revisionNumbers := make([]int64, len(gvs))
		for i, gv := range gvs {
			revisionNumbers[i] = gv.Spec.Revision
		}

		for i := 1; i < len(revisionNumbers); i++ {
			for j := 0; j < i; j++ {
				// No duplicates
				Expect(revisionNumbers[i]).ToNot(Equal(revisionNumbers[j]),
					"revision numbers should be unique")
			}
		}
	})
})

var _ = Describe("GraphRevision Instance Resolution", func() {

	It("should resolve the compiled graph from the registry and reconcile instances", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		rgdName := fmt.Sprintf("gv-resolve-%s", rand.String(5))
		kind := fmt.Sprintf("GvResolve%s", rand.String(5))
		rgd := simpleDeploymentRGD(rgdName, kind)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActive(ctx, rgdName)

		// Create an instance
		instanceName := fmt.Sprintf("inst-%s", rand.String(5))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       kind,
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"image":    "nginx:1.25",
					"replicas": 2,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		// Verify the deployment was created — proving the instance controller
		// successfully resolved the compiled graph from the registry
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("deploy-%s", instanceName),
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.25"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should serve updated graph to instances after RGD spec change", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		rgdName := fmt.Sprintf("gv-update-%s", rand.String(5))
		kind := fmt.Sprintf("GvUpdate%s", rand.String(5))

		// Start with a deployment-only RGD
		rgd := simpleDeploymentRGD(rgdName, kind)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActive(ctx, rgdName)

		// Create instance
		instanceName := fmt.Sprintf("inst-%s", rand.String(5))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       kind,
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"image":    "nginx:1.25",
					"replicas": 1,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		// Verify initial deployment is created
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("deploy-%s", instanceName),
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Update the RGD spec: change the default image
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())

			fresh.Spec.Schema.Spec.Raw = []byte(`{"replicas":"integer | default=1","image":"string | default=nginx:1.26-updated"}`)
			err = env.Client.Update(ctx, fresh)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Wait for revision 2 to become Active
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(fresh.Status.LatestObservedGV).ToNot(BeNil())
			g.Expect(fresh.Status.LatestObservedGV.Revision).To(Equal(int64(2)))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Update the instance to use the new default image
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			instance.Object["spec"] = map[string]interface{}{
				"image":    "nginx:1.26-updated",
				"replicas": int64(1),
			}
			err = env.Client.Update(ctx, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify deployment is updated with new image
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("deploy-%s", instanceName),
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.26-updated"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})

var _ = Describe("GraphRevision Multiple RGDs", func() {

	It("should maintain independent revision sequences for different RGDs", func(ctx SpecContext) {
		rgdNameA := fmt.Sprintf("gv-multi-a-%s", rand.String(5))
		rgdNameB := fmt.Sprintf("gv-multi-b-%s", rand.String(5))
		kindA := fmt.Sprintf("GvMultiA%s", rand.String(5))
		kindB := fmt.Sprintf("GvMultiB%s", rand.String(5))

		rgdA := configmapRGD(rgdNameA, kindA)
		rgdB := configmapRGD(rgdNameB, kindB)

		Expect(env.Client.Create(ctx, rgdA)).To(Succeed())
		Expect(env.Client.Create(ctx, rgdB)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgdA)).To(Succeed())
			Expect(env.Client.Delete(ctx, rgdB)).To(Succeed())
		})

		waitForRGDActive(ctx, rgdNameA)
		waitForRGDActive(ctx, rgdNameB)

		// Both should have exactly one revision
		Eventually(func(g Gomega) {
			gvsA := listGraphRevisions(ctx, rgdNameA)
			gvsB := listGraphRevisions(ctx, rgdNameB)
			g.Expect(gvsA).To(HaveLen(1))
			g.Expect(gvsB).To(HaveLen(1))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Update RGD A only
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdNameA}, fresh)
			g.Expect(err).ToNot(HaveOccurred())

			fresh.Spec.Schema.Spec.Raw = []byte(`{"data":"string | default=changed"}`)
			err = env.Client.Update(ctx, fresh)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Wait for RGD A to get revision 2
		Eventually(func(g Gomega) {
			gvsA := listGraphRevisions(ctx, rgdNameA)
			maxRev := int64(0)
			for _, gv := range gvsA {
				if gv.Spec.Revision > maxRev {
					maxRev = gv.Spec.Revision
				}
			}
			g.Expect(maxRev).To(Equal(int64(2)))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// RGD B should still have only one revision — independent
		gvsB := listGraphRevisions(ctx, rgdNameB)
		Expect(gvsB).To(HaveLen(1))
		Expect(gvsB[0].Spec.Revision).To(Equal(int64(1)))
	})
})

var _ = Describe("GraphRevision Spec Immutability", func() {

	It("should reject updates to an existing GraphRevision spec", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gv-immut-%s", rand.String(5))
		kind := fmt.Sprintf("GvImmut%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActive(ctx, rgdName)

		var gv krov1alpha1.GraphRevision
		Eventually(func(g Gomega) {
			gvs := listGraphRevisions(ctx, rgdName)
			g.Expect(gvs).To(HaveLen(1))
			gv = gvs[0]
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Try to mutate the GraphRevision spec
		gv.Spec.SpecHash = "tampered-hash"
		err := env.Client.Update(ctx, &gv)
		Expect(err).To(HaveOccurred(), "GraphRevision spec should be immutable")
		Expect(err.Error()).To(ContainSubstring("immutable"),
			"error message should mention immutability")
	})
})

func listGraphRevisions(ctx SpecContext, rgdName string) []krov1alpha1.GraphRevision {
	list := &krov1alpha1.GraphRevisionList{}
	sel := labels.SelectorFromSet(map[string]string{
		metadata.ResourceGraphDefinitionNameLabel: rgdName,
	})
	ExpectWithOffset(1, env.Client.List(ctx, list, &client.ListOptions{LabelSelector: sel})).To(Succeed())
	return list.Items
}

func findGVCondition(conditions []krov1alpha1.Condition, t krov1alpha1.ConditionType) *krov1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == t {
			return &conditions[i]
		}
	}
	return nil
}

func simpleDeploymentRGD(name, kind string) *krov1alpha1.ResourceGraphDefinition {
	return generator.NewResourceGraphDefinition(name,
		generator.WithSchema(
			kind, "v1alpha1",
			map[string]interface{}{
				"replicas": "integer | default=1",
				"image":    "string | default=nginx:latest",
			},
			nil,
		),
		generator.WithResource("deployment", map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name": "deploy-${schema.metadata.name}",
			},
			"spec": map[string]interface{}{
				"replicas": "${schema.spec.replicas}",
				"selector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"app": "test",
					},
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": map[string]interface{}{
							"app": "test",
						},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "${schema.spec.image}",
							},
						},
					},
				},
			},
		}, nil, nil),
	)
}

func configmapRGD(name, kind string) *krov1alpha1.ResourceGraphDefinition {
	return generator.NewResourceGraphDefinition(name,
		generator.WithSchema(
			kind, "v1alpha1",
			map[string]interface{}{
				"data": "string | default=hello",
			},
			nil,
		),
		generator.WithResource("configmap", map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name": "cm-${schema.metadata.name}",
			},
			"data": map[string]interface{}{
				"key": "${schema.spec.data}",
			},
		}, nil, nil),
	)
}
