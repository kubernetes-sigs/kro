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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("GraphRevision Lifecycle", func() {

	It("should create a GraphRevision when an RGD is created", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gr-create-%s", rand.String(5))
		kind := fmt.Sprintf("GrCreate%s", rand.String(5))
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
		var grs []internalv1alpha1.GraphRevision
		Eventually(func(g Gomega) {
			grs = listGraphRevisions(ctx, rgdName)
			g.Expect(grs).To(HaveLen(1))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		gr := grs[0]

		// Verify spec fields
		Expect(gr.Spec.Snapshot.Name).To(Equal(rgdName))
		Expect(gr.Spec.Revision).To(Equal(int64(1)))
		Expect(gr.Spec.Snapshot.SpecHash).ToNot(BeEmpty())
		Expect(gr.Spec.Snapshot.Spec.Schema).ToNot(BeNil())
		Expect(gr.Spec.Snapshot.Spec.Schema.Kind).To(Equal(kind))

		// Verify naming convention
		Expect(gr.Name).To(HavePrefix(rgdName + "-"))
		Expect(gr.Name).To(ContainSubstring("-r1-"))

		// Verify labels (UID label is not set on GraphRevisions — lineage is name-based)
		Expect(gr.Labels[metadata.ResourceGraphDefinitionNameLabel]).To(Equal(rgdName))
		Expect(gr.Labels).ToNot(HaveKey(metadata.ResourceGraphDefinitionIDLabel))

		// Verify OwnerReference
		Expect(gr.OwnerReferences).To(HaveLen(1))
		Expect(gr.OwnerReferences[0].Name).To(Equal(rgdName))
		Expect(gr.OwnerReferences[0].UID).To(Equal(activeRGD.UID))

		// Verify GR status conditions
		graphVerified := findGRCondition(gr.Status.Conditions, krov1alpha1.GraphRevisionConditionTypeGraphVerified)
		Expect(graphVerified).ToNot(BeNil())
		Expect(graphVerified.Status).To(Equal(metav1.ConditionTrue))

		ready := findGRCondition(gr.Status.Conditions, krov1alpha1.ConditionType(apis.ConditionReady))
		Expect(ready).ToNot(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))

		// Verify topological order is populated
		Expect(gr.Status.TopologicalOrder).ToNot(BeEmpty())
		Expect(gr.Status.TopologicalOrder).To(ContainElement("deployment"))

		// Verify RGD status tracks the issued revision high-water mark.
		Expect(activeRGD.Status.LastIssuedRevision).To(Equal(int64(1)))
	})

	It("should create GraphRevision with correct topological order for multi-resource graphs", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gr-topo-%s", rand.String(5))
		kind := fmt.Sprintf("GrTopo%s", rand.String(5))

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

		var grs []internalv1alpha1.GraphRevision
		Eventually(func(g Gomega) {
			grs = listGraphRevisions(ctx, rgdName)
			g.Expect(grs).To(HaveLen(1))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		gr := grs[0]
		// configmap should come before deployment in topological order
		Expect(gr.Status.TopologicalOrder).To(HaveLen(2))
		cmIdx := -1
		deployIdx := -1
		for i, id := range gr.Status.TopologicalOrder {
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
		Expect(gr.Status.Resources).ToNot(BeEmpty())
		var deployRes *krov1alpha1.ResourceInformation
		for i := range gr.Status.Resources {
			if gr.Status.Resources[i].ID == "deployment" {
				deployRes = &gr.Status.Resources[i]
			}
		}
		Expect(deployRes).ToNot(BeNil())
		Expect(deployRes.Dependencies).To(HaveLen(1))
		Expect(deployRes.Dependencies[0].ID).To(Equal("configmap"))
	})

	It("should complete GraphRevision deletion after delete-start finalizer handling", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gr-delete-%s", rand.String(5))
		kind := fmt.Sprintf("GrDelete%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActive(ctx, rgdName)

		var gr internalv1alpha1.GraphRevision
		Eventually(func(g Gomega) {
			grs := listGraphRevisions(ctx, rgdName)
			g.Expect(grs).To(HaveLen(1))
			gr = grs[0]
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			current := &internalv1alpha1.GraphRevision{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: gr.Name}, current)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(metadata.HasGraphRevisionFinalizer(current)).To(BeTrue())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, &gr)).To(Succeed())

		Eventually(func(g Gomega) {
			current := &internalv1alpha1.GraphRevision{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: gr.Name}, current)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
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
			firstHash = gvs[0].Spec.Snapshot.SpecHash
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

			revisionMap := map[int64]internalv1alpha1.GraphRevision{}
			for _, gv := range gvs {
				revisionMap[gv.Spec.Revision] = gv
			}

			// Revision 1 should still exist
			rev1, ok := revisionMap[1]
			g.Expect(ok).To(BeTrue(), "revision 1 should still exist")
			g.Expect(rev1.Spec.Snapshot.SpecHash).To(Equal(firstHash))

			// Revision 2 should exist with a different hash
			rev2, ok := revisionMap[2]
			g.Expect(ok).To(BeTrue(), "revision 2 should exist")
			g.Expect(rev2.Spec.Snapshot.SpecHash).ToNot(Equal(firstHash))
			g.Expect(rev2.Spec.Revision).To(Equal(int64(2)))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// RGD status should reflect the issued revision high-water mark.
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
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

var _ = Describe("GraphRevision Invalid Updates", func() {

	It("should keep serving the last good revision when a later RGD update is invalid", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		rgdName := fmt.Sprintf("gv-invalid-%s", rand.String(5))
		kind := fmt.Sprintf("GvInvalid%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActive(ctx, rgdName)

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
					"data": "hello",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		configMapName := fmt.Sprintf("cm-%s", instanceName)
		configMap := &corev1.ConfigMap{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, configMap)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMap.Data).To(HaveKeyWithValue("key", "hello"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		invalidRGD := invalidConfigmapRGD(rgdName, kind)
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			fresh.Spec = *invalidRGD.Spec.DeepCopy()
			err = env.Client.Update(ctx, fresh)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(int64(1)))

			accepted := findRGDCondition(
				fresh.Status.Conditions,
				krov1alpha1.ConditionType(resourcegraphdefinition.ResourceGraphAccepted),
			)
			g.Expect(accepted).ToNot(BeNil())
			g.Expect(accepted.Status).To(Equal(metav1.ConditionFalse))

			ready := findRGDCondition(fresh.Status.Conditions, krov1alpha1.ConditionType(apis.ConditionReady))
			g.Expect(ready).ToNot(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(listGraphRevisions(ctx, rgdName)).To(HaveLen(1))
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			current := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
					"kind":       kind,
				},
			}
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, current)
			g.Expect(err).ToNot(HaveOccurred())
			current.Object["spec"] = map[string]interface{}{"data": "world"}
			err = env.Client.Update(ctx, current)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, configMap)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMap.Data).To(HaveKeyWithValue("key", "world"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
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

			fresh.Spec.Schema.Spec.Raw = []byte(
				`{"replicas":"integer | default=1","image":"string | default=nginx:1.26-updated"}`,
			)
			err = env.Client.Update(ctx, fresh)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Wait for revision 2 to be issued.
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(fresh.Status.LastIssuedRevision).To(BeNumerically(">=", int64(2)))
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
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())
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

		var gv internalv1alpha1.GraphRevision
		Eventually(func(g Gomega) {
			gvs := listGraphRevisions(ctx, rgdName)
			g.Expect(gvs).To(HaveLen(1))
			gv = gvs[0]
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Try to mutate the GraphRevision spec
		gv.Spec.Snapshot.SpecHash = "tampered-hash"
		err := env.Client.Update(ctx, &gv)
		Expect(err).To(HaveOccurred(), "GraphRevision spec should be immutable")
		Expect(err.Error()).To(ContainSubstring("immutable"),
			"error message should mention immutability")
	})

	It("should recover after cascade delete and recreate with the same name and same spec", func(ctx SpecContext) {
		const numRevisions = 5
		rgdName := fmt.Sprintf("gr-recreate-same-%s", rand.String(5))
		kind := fmt.Sprintf("GrRecrSame%s", rand.String(5))

		createRGDWithRevisions(ctx, rgdName, kind, numRevisions)

		// Collect hashes before deletion
		grs := listGraphRevisions(ctx, rgdName)
		Expect(grs).To(HaveLen(numRevisions))
		lastHash := ""
		for _, gr := range grs {
			if gr.Spec.Revision == numRevisions {
				lastHash = gr.Spec.Snapshot.SpecHash
			}
		}
		Expect(lastHash).ToNot(BeEmpty())

		// Delete RGD (cascade) and wait for everything to be gone
		rgd := &krov1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: rgdName},
		}
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// envtest has no GC controller — manually delete GRs to emulate cascade.
		emulateGCForRGD(ctx, rgdName)

		// Recreate with the same spec as the last revision
		rgd2 := generator.NewResourceGraphDefinition(rgdName,
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
					"labels": map[string]interface{}{
						"revision": fmt.Sprintf("rev-%d", numRevisions),
					},
				},
				"data": map[string]interface{}{
					"key": "${schema.spec.data}",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd2)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd2)).To(Succeed())
		})

		// All old revisions are gone (cascade). New RGD starts fresh from
		// revision 1 since there's no watermark to recover from.
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(int64(1)))

			grs := listGraphRevisions(ctx, rgdName)
			g.Expect(grs).To(HaveLen(1))
			g.Expect(grs[0].Spec.Revision).To(Equal(int64(1)))
			g.Expect(grs[0].Spec.Snapshot.Name).To(Equal(rgdName))
			g.Expect(grs[0].Spec.Snapshot.SpecHash).To(Equal(lastHash),
				"same spec should produce the same hash")
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should recover after cascade delete and recreate with the same name but different spec", func(ctx SpecContext) {
		const numRevisions = 5
		rgdName := fmt.Sprintf("gr-recreate-diff-%s", rand.String(5))
		kind := fmt.Sprintf("GrRecrDiff%s", rand.String(5))

		createRGDWithRevisions(ctx, rgdName, kind, numRevisions)

		// Collect all hashes
		existingHashes := map[string]bool{}
		for _, gr := range listGraphRevisions(ctx, rgdName) {
			existingHashes[gr.Spec.Snapshot.SpecHash] = true
		}

		// Delete (cascade) and wait for full cleanup
		rgd := &krov1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: rgdName},
		}
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// envtest has no GC controller — manually delete GRs to emulate cascade.
		emulateGCForRGD(ctx, rgdName)

		// Recreate with a completely different spec
		rgd2 := generator.NewResourceGraphDefinition(rgdName,
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
					"labels": map[string]interface{}{
						"revision": "completely-new",
					},
				},
				"data": map[string]interface{}{
					"key": "${schema.spec.data}",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd2)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd2)).To(Succeed())
		})

		// All old revisions are gone. New RGD starts from revision 1 with a
		// different hash.
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(int64(1)))

			grs := listGraphRevisions(ctx, rgdName)
			g.Expect(grs).To(HaveLen(1))
			g.Expect(grs[0].Spec.Revision).To(Equal(int64(1)))
			g.Expect(existingHashes).ToNot(HaveKey(grs[0].Spec.Snapshot.SpecHash),
				"new spec should produce a different hash from all old revisions")
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should adopt orphaned revisions after delete with orphan policy and recreate with same spec", func(ctx SpecContext) {
		const numRevisions = 5
		rgdName := fmt.Sprintf("gr-orphan-%s", rand.String(5))
		kind := fmt.Sprintf("GrOrphan%s", rand.String(5))

		createRGDWithRevisions(ctx, rgdName, kind, numRevisions)

		// Snapshot the revisions before deletion
		grs := listGraphRevisions(ctx, rgdName)
		Expect(grs).To(HaveLen(numRevisions))

		// Build a map of revision number → hash for exact assertions later
		revisionHashes := map[int64]string{}
		for _, gr := range grs {
			revisionHashes[gr.Spec.Revision] = gr.Spec.Snapshot.SpecHash
		}
		// Verify exact revision numbers 1..5
		for i := int64(1); i <= numRevisions; i++ {
			Expect(revisionHashes).To(HaveKey(i), "revision %d should exist", i)
		}
		latestHash := revisionHashes[numRevisions]

		// Emulate orphan deletion — strip ownerRefs then delete RGD.
		// envtest has no GC so real orphan propagation doesn't work.
		emulateOrphanDeleteForRGD(ctx, rgdName)

		// All 5 GRs should survive without an owner
		Expect(listGraphRevisions(ctx, rgdName)).To(HaveLen(numRevisions))

		// Recreate with the same spec as the latest revision (template has
		// labels.revision: rev-5 from the last updateRGDTemplate call).
		rgd2 := generator.NewResourceGraphDefinition(rgdName,
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
					"labels": map[string]interface{}{
						"revision": fmt.Sprintf("rev-%d", numRevisions),
					},
				},
				"data": map[string]interface{}{
					"key": "${schema.spec.data}",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd2)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			for _, gr := range listGraphRevisions(ctx, rgdName) {
				_ = env.Client.Delete(ctx, &gr)
			}
			_ = env.Client.Delete(ctx, rgd2)
		})

		// RGD becomes active. Hash matches latest orphaned revision, so no new
		// revision should be issued. All 5 orphaned revisions should remain.
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

			grs := listGraphRevisions(ctx, rgdName)
			g.Expect(grs).To(HaveLen(numRevisions),
				"no new revision should be issued when hash matches")

			// Verify exact revision numbers 1..5 still present
			revNums := map[int64]bool{}
			for _, gr := range grs {
				revNums[gr.Spec.Revision] = true
			}
			for i := int64(1); i <= numRevisions; i++ {
				g.Expect(revNums).To(HaveKey(i), "revision %d should exist", i)
			}

			// Revision 5's hash should match the recreated RGD's spec
			g.Expect(revNums).To(HaveKey(int64(numRevisions)))
			for _, gr := range grs {
				if gr.Spec.Revision == numRevisions {
					g.Expect(gr.Spec.Snapshot.SpecHash).To(Equal(latestHash))
				}
			}
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should issue a new revision when recreated with orphaned revisions and a different spec", func(ctx SpecContext) {
		const numRevisions = 5
		rgdName := fmt.Sprintf("gr-orphan-diff-%s", rand.String(5))
		kind := fmt.Sprintf("GrOrphDif%s", rand.String(5))

		createRGDWithRevisions(ctx, rgdName, kind, numRevisions)

		// Snapshot state before deletion
		grs := listGraphRevisions(ctx, rgdName)
		Expect(grs).To(HaveLen(numRevisions))

		// Collect all existing hashes and verify exact revision numbers
		existingHashes := map[string]bool{}
		for _, gr := range grs {
			existingHashes[gr.Spec.Snapshot.SpecHash] = true
		}
		for i := int64(1); i <= numRevisions; i++ {
			found := false
			for _, gr := range grs {
				if gr.Spec.Revision == i {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "revision %d should exist before deletion", i)
		}

		// Emulate orphan deletion — strip ownerRefs then delete RGD.
		emulateOrphanDeleteForRGD(ctx, rgdName)

		// All 5 GRs should survive without an owner
		Expect(listGraphRevisions(ctx, rgdName)).To(HaveLen(numRevisions))

		// Recreate with a completely different spec
		rgd2 := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				kind, "v1alpha1",
				map[string]interface{}{
					"data": "string | default=completely-different-spec",
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
		Expect(env.Client.Create(ctx, rgd2)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			for _, gr := range listGraphRevisions(ctx, rgdName) {
				_ = env.Client.Delete(ctx, &gr)
			}
			_ = env.Client.Delete(ctx, rgd2)
		})

		// RGD becomes active. Hash doesn't match any orphan, so a new revision
		// must be issued. Watermark recovers from the orphaned revisions.
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

			grs := listGraphRevisions(ctx, rgdName)
			g.Expect(grs).To(HaveLen(numRevisions+1),
				"should have 5 orphaned + 1 new revision")

			// Verify exact revision numbers 1..6
			revByNum := map[int64]internalv1alpha1.GraphRevision{}
			for _, gr := range grs {
				revByNum[gr.Spec.Revision] = gr
			}
			for i := int64(1); i <= int64(numRevisions+1); i++ {
				g.Expect(revByNum).To(HaveKey(i), "revision %d should exist", i)
			}

			// Revision 6 (the new one) should have a hash different from all orphans
			newRevision := revByNum[int64(numRevisions+1)]
			g.Expect(existingHashes).ToNot(HaveKey(newRevision.Spec.Snapshot.SpecHash),
				"revision 6 should have a hash different from all orphaned revisions")

			// LastIssuedRevision should reflect the watermark
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(int64(numRevisions + 1)))
		}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should serve instances using compiled graph from current revision", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gr-serve-%s", rand.String(5))
		kind := fmt.Sprintf("GrServe%s", rand.String(5))
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		rgd := configmapRGD(rgdName, kind)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		// Wait for active
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

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
					"data": "v1-value",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		// Wait for ConfigMap with v1 data
		cmName := fmt.Sprintf("cm-%s", instanceName)
		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: cmName, Namespace: namespace}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data).To(HaveKeyWithValue("key", "v1-value"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify revision 1 exists
		Eventually(func(g Gomega) {
			grs := listGraphRevisions(ctx, rgdName)
			g.Expect(grs).To(HaveLen(1))
			g.Expect(grs[0].Spec.Revision).To(Equal(int64(1)))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Update instance data — instance controller should use the compiled
		// graph from the current revision to reconcile the change.
		Eventually(func(g Gomega) {
			current := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
					"kind":       kind,
				},
			}
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, current)
			g.Expect(err).ToNot(HaveOccurred())
			current.Object["spec"] = map[string]interface{}{"data": "v2-value"}
			g.Expect(env.Client.Update(ctx, current)).To(Succeed())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: cmName, Namespace: namespace}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data).To(HaveKeyWithValue("key", "v2-value"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})

func listGraphRevisions(ctx SpecContext, rgdName string) []internalv1alpha1.GraphRevision {
	list := &internalv1alpha1.GraphRevisionList{}
	sel := labels.SelectorFromSet(map[string]string{
		metadata.ResourceGraphDefinitionNameLabel: rgdName,
	})
	ExpectWithOffset(1, env.Client.List(ctx, list, &client.ListOptions{LabelSelector: sel})).To(Succeed())
	return list.Items
}

func findGRCondition(conditions []krov1alpha1.Condition, t krov1alpha1.ConditionType) *krov1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == t {
			return &conditions[i]
		}
	}
	return nil
}

func findRGDCondition(conditions []krov1alpha1.Condition, t krov1alpha1.ConditionType) *krov1alpha1.Condition {
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

// createRGDWithRevisions creates a configmap RGD, waits for it to become active,
// then mutates the spec n-1 additional times to produce exactly n revisions.
// Returns the RGD name and kind for further operations.
func createRGDWithRevisions(ctx SpecContext, rgdName, kind string, n int) {
	rgd := configmapRGD(rgdName, kind)
	ExpectWithOffset(1, env.Client.Create(ctx, rgd)).To(Succeed())

	// Wait for active + revision 1
	EventuallyWithOffset(1, func(g Gomega) {
		fresh := &krov1alpha1.ResourceGraphDefinition{}
		err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		g.Expect(listGraphRevisions(ctx, rgdName)).To(HaveLen(1))
	}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

	// Mutate resource template n-1 times to produce revisions 2..n
	for i := 2; i <= n; i++ {
		updateRGDTemplate(ctx, rgdName, fmt.Sprintf("rev-%d", i))
		expectedRevision := int64(i)
		EventuallyWithOffset(1, func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.LastIssuedRevision).To(BeNumerically(">=", expectedRevision))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	}

	// Wait for all n revisions to exist
	EventuallyWithOffset(1, func(g Gomega) {
		g.Expect(listGraphRevisions(ctx, rgdName)).To(HaveLen(n))
	}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

// updateRGDTemplate updates the configmap RGD's resource template to trigger a
// new revision. Each call adds a unique label to the ConfigMap template so the
// spec hash changes without touching the schema.
func updateRGDTemplate(ctx SpecContext, rgdName, label string) {
	EventuallyWithOffset(1, func(g Gomega) {
		fresh := &krov1alpha1.ResourceGraphDefinition{}
		err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
		g.Expect(err).ToNot(HaveOccurred())
		fresh.Spec.Resources[0].Template.Raw = []byte(fmt.Sprintf(
			`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-${schema.metadata.name}","labels":{"revision":"%s"}},"data":{"key":"${schema.spec.data}"}}`,
			label,
		))
		err = env.Client.Update(ctx, fresh)
		g.Expect(err).ToNot(HaveOccurred())
	}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

// emulateGCForRGD manually deletes GraphRevisions that have a deletionTimestamp,
// emulating what the Kubernetes garbage collector would do on cascade delete.
// envtest does not run a GC controller, so ownerReference-based cascade
// deletion doesn't work. Only revisions already marked for deletion are removed.
func emulateGCForRGD(ctx SpecContext, rgdName string) {
	list := &internalv1alpha1.GraphRevisionList{}
	sel := labels.SelectorFromSet(map[string]string{
		metadata.ResourceGraphDefinitionNameLabel: rgdName,
	})
	ExpectWithOffset(1, env.Client.List(ctx, list, &client.ListOptions{LabelSelector: sel})).To(Succeed())

	for i := range list.Items {
		gr := &list.Items[i]
		if !gr.DeletionTimestamp.IsZero() {
			// Remove finalizer so the API server can complete deletion
			gr.Finalizers = nil
			ExpectWithOffset(1, env.Client.Update(ctx, gr)).To(Succeed())
		} else {
			// Not marked for deletion — delete it explicitly (emulate GC marking + deletion)
			ExpectWithOffset(1, env.Client.Delete(ctx, gr)).To(Succeed())
		}
	}

	EventuallyWithOffset(1, func(g Gomega) {
		remaining := &internalv1alpha1.GraphRevisionList{}
		g.Expect(env.Client.List(ctx, remaining, &client.ListOptions{LabelSelector: sel})).To(Succeed())
		g.Expect(remaining.Items).To(BeEmpty())
	}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

// emulateOrphanDeleteForRGD emulates DeletePropagationOrphan in envtest by
// stripping ownerReferences from all GraphRevisions first, then deleting the
// RGD. Without ownerReferences, envtest won't cascade-delete the GRs.
// This matches the end state of a real orphan deletion: RGD gone, GRs alive
// with no owner.
func emulateOrphanDeleteForRGD(ctx SpecContext, rgdName string) {
	// Strip ownerReferences from all GRs
	for _, gr := range listGraphRevisions(ctx, rgdName) {
		gr := gr
		gr.OwnerReferences = nil
		ExpectWithOffset(1, env.Client.Update(ctx, &gr)).To(Succeed())
	}

	// Now delete the RGD — GRs have no ownerRef so they won't be cascade-deleted
	rgd := &krov1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: rgdName},
	}
	ExpectWithOffset(1, env.Client.Delete(ctx, rgd)).To(Succeed())

	// Wait for RGD to be gone
	EventuallyWithOffset(1, func(g Gomega) {
		err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, &krov1alpha1.ResourceGraphDefinition{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

func invalidConfigmapRGD(name, kind string) *krov1alpha1.ResourceGraphDefinition {
	return generator.NewResourceGraphDefinition(name,
		generator.WithSchema(
			kind, "v1alpha1",
			map[string]interface{}{
				"data": "string | default=hello",
			},
			nil,
		),
		generator.WithResource("vpc", map[string]interface{}{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1",
			"kind":       "VPC",
			"metadata": map[string]interface{}{
				"name": "${subnet.status.subnetID}",
			},
			"spec": map[string]interface{}{
				"cidrBlocks": []interface{}{"192.168.0.0/16"},
			},
		}, nil, nil),
		generator.WithResource("subnet", map[string]interface{}{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1",
			"kind":       "Subnet",
			"metadata": map[string]interface{}{
				"name": "test-subnet",
			},
			"spec": map[string]interface{}{
				"vpcID":     "${vpc.status.vpcID}",
				"cidrBlock": "192.168.1.0/24",
			},
		}, nil, nil),
	)
}
