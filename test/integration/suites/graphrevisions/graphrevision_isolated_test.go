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

package graphrevisions_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	graphhash "github.com/kubernetes-sigs/kro/pkg/graph/hash"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

const isolatedGraphRevisionRetentionLimit = 5

var _ = Describe("GraphRevision Integration", Serial, func() {
	It("should retain only the newest revisions and keep issuing from the watermark", func(ctx SpecContext) {
		testEnv := newIsolatedGraphRevisionEnv(ctx, isolatedGraphRevisionRetentionLimit)
		rgdName := fmt.Sprintf("gv-retain-%s", rand.String(5))
		kind := fmt.Sprintf("GvRetain%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(testEnv.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActiveInEnv(ctx, testEnv, rgdName)

		for revision := int64(2); revision <= int64(isolatedGraphRevisionRetentionLimit+1); revision++ {
			updateRGDDataDefaultInEnv(ctx, testEnv, rgdName, fmt.Sprintf("value-%d", revision))

			Eventually(func(g Gomega) {
				fresh := &krov1alpha1.ResourceGraphDefinition{}
				err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				g.Expect(fresh.Status.LastIssuedRevision).To(Equal(revision))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		Eventually(func(g Gomega) {
			gvs := nonTerminatingGraphRevisionsInEnv(ctx, testEnv, rgdName)
			g.Expect(gvs).To(HaveLen(isolatedGraphRevisionRetentionLimit))
			g.Expect(graphRevisionNumbers(gvs)).To(ConsistOf(
				expectedRetainedRevisionNumbers(
					int64(isolatedGraphRevisionRetentionLimit + 1),
				),
			))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		updateRGDDataDefaultInEnv(
			ctx,
			testEnv,
			rgdName,
			fmt.Sprintf("value-%d", isolatedGraphRevisionRetentionLimit+2),
		)

		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(int64(isolatedGraphRevisionRetentionLimit + 2)))

			gvs := listGraphRevisionsInEnv(ctx, testEnv, rgdName)
			gvs = nonTerminatingGraphRevisions(gvs)
			g.Expect(gvs).To(HaveLen(isolatedGraphRevisionRetentionLimit))
			g.Expect(graphRevisionNumbers(gvs)).To(ConsistOf(
				expectedRetainedRevisionNumbers(
					int64(isolatedGraphRevisionRetentionLimit + 2),
				),
			))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should delete the oldest graph revision on each update past the retention limit", func(ctx SpecContext) {
		testEnv := newIsolatedGraphRevisionEnv(ctx, isolatedGraphRevisionRetentionLimit)
		rgdName := fmt.Sprintf("gv-gc-window-%s", rand.String(5))
		kind := fmt.Sprintf("GvGCWindow%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(testEnv.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActiveInEnv(ctx, testEnv, rgdName)

		const updatesPastRetention = 5
		latestRevision := int64(isolatedGraphRevisionRetentionLimit + updatesPastRetention)
		for revision := int64(2); revision <= latestRevision; revision++ {
			updateRGDDataDefaultInEnv(ctx, testEnv, rgdName, fmt.Sprintf("gc-window-%d", revision))

			Eventually(func(g Gomega) {
				fresh := &krov1alpha1.ResourceGraphDefinition{}
				err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				g.Expect(fresh.Status.LastIssuedRevision).To(Equal(revision))

				gvs := listGraphRevisionsInEnv(ctx, testEnv, rgdName)
				expectedCount := int(revision)
				if expectedCount > isolatedGraphRevisionRetentionLimit {
					expectedCount = isolatedGraphRevisionRetentionLimit
				}
				g.Expect(gvs).To(HaveLen(expectedCount))

				if revision <= int64(isolatedGraphRevisionRetentionLimit) {
					expectedNumbers := make([]int64, 0, revision)
					for retained := int64(1); retained <= revision; retained++ {
						expectedNumbers = append(expectedNumbers, retained)
					}
					g.Expect(graphRevisionNumbers(gvs)).To(ConsistOf(expectedNumbers))
					return
				}

				g.Expect(graphRevisionNumbers(gvs)).To(ConsistOf(
					expectedRetainedRevisionNumbers(revision),
				))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}
	})

	It("should keep migrating a live instance while old graph revisions are garbage-collected", func(ctx SpecContext) {
		testEnv := newIsolatedGraphRevisionEnv(ctx, isolatedGraphRevisionRetentionLimit)
		namespace := fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(testEnv.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, ns)).To(Succeed())
		})

		rgdName := fmt.Sprintf("gv-live-gc-%s", rand.String(5))
		kind := fmt.Sprintf("GvLiveGC%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)
		Expect(testEnv.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActiveInEnv(ctx, testEnv, rgdName)

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
					"data": "sticky-value",
				},
			},
		}
		Expect(testEnv.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, instance)).To(Succeed())
		})

		configMapName := fmt.Sprintf("cm-%s", instanceName)
		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			err := testEnv.Client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data).To(HaveKeyWithValue("key", "sticky-value"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		const updatesPastRetention = 5
		latestRevision := int64(isolatedGraphRevisionRetentionLimit + updatesPastRetention)
		for revision := int64(2); revision <= latestRevision; revision++ {
			updateRGDTemplateLabelInEnv(ctx, testEnv, rgdName, fmt.Sprintf("rev-%d", revision))

			Eventually(func(g Gomega) {
				fresh := &krov1alpha1.ResourceGraphDefinition{}
				err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				g.Expect(fresh.Status.LastIssuedRevision).To(Equal(revision))

				cm := &corev1.ConfigMap{}
				err = testEnv.Client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data).To(HaveKeyWithValue("key", "sticky-value"))
				g.Expect(cm.Labels).To(HaveKeyWithValue("revision", fmt.Sprintf("rev-%d", revision)))

				gvs := listGraphRevisionsInEnv(ctx, testEnv, rgdName)
				expectedCount := int(revision)
				if expectedCount > isolatedGraphRevisionRetentionLimit {
					expectedCount = isolatedGraphRevisionRetentionLimit
				}
				g.Expect(gvs).To(HaveLen(expectedCount))

				if revision <= int64(isolatedGraphRevisionRetentionLimit) {
					expectedNumbers := make([]int64, 0, revision)
					for retained := int64(1); retained <= revision; retained++ {
						expectedNumbers = append(expectedNumbers, retained)
					}
					g.Expect(graphRevisionNumbers(gvs)).To(ConsistOf(expectedNumbers))
					return
				}

				g.Expect(graphRevisionNumbers(gvs)).To(ConsistOf(
					expectedRetainedRevisionNumbers(revision),
				))
			}, 60*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}
	})

	It("should not issue duplicate graph revisions for an unchanged RGD across controller restart", func(ctx SpecContext) {
		testEnv := newIsolatedGraphRevisionEnv(ctx, 20)
		rgdName := fmt.Sprintf("gv-stable-%s", rand.String(5))
		kind := fmt.Sprintf("GvStable%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(testEnv.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, rgd)).To(Succeed())
		})

		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(int64(1)))

			gvs := listGraphRevisionsInEnv(ctx, testEnv, rgdName)
			g.Expect(gvs).To(HaveLen(1))
			g.Expect(gvs[0].Spec.Revision).To(Equal(int64(1)))

			selected := &internalv1alpha1.GraphRevisionList{}
			err = testEnv.CtrlManager.GetAPIReader().List(ctx, selected, client.MatchingFields{
				"spec.snapshot.name": rgdName,
			})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(selected.Items).To(HaveLen(1))
			g.Expect(selected.Items[0].Spec.Revision).To(Equal(int64(1)))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(testEnv.RestartControllers()).To(Succeed())

		Consistently(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(int64(1)))

			gvs := listGraphRevisionsInEnv(ctx, testEnv, rgdName)
			g.Expect(gvs).To(HaveLen(1))
			g.Expect(maxGraphRevisionNumber(gvs)).To(Equal(int64(1)))

			selected := &internalv1alpha1.GraphRevisionList{}
			err = testEnv.CtrlManager.GetAPIReader().List(ctx, selected, client.MatchingFields{
				"spec.snapshot.name": rgdName,
			})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(selected.Items).To(HaveLen(1))
			g.Expect(selected.Items[0].Spec.Revision).To(Equal(int64(1)))
		}, 15*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It(
		"should warm the registry on controller restart and continue issuing from the recovered watermark",
		func(ctx SpecContext) {
			testEnv := newIsolatedGraphRevisionEnv(ctx, 20)
			namespace := fmt.Sprintf("test-%s", rand.String(5))
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
			Expect(testEnv.Client.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(testEnv.Client.Delete(ctx, ns)).To(Succeed())
			})

			rgdName := fmt.Sprintf("gv-restart-%s", rand.String(5))
			kind := fmt.Sprintf("GvRestart%s", rand.String(5))
			rgd := configmapRGD(rgdName, kind)

			Expect(testEnv.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(testEnv.Client.Delete(ctx, rgd)).To(Succeed())
			})

			waitForRGDActiveInEnv(ctx, testEnv, rgdName)

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
						"data": "before-restart",
					},
				},
			}
			Expect(testEnv.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(testEnv.Client.Delete(ctx, instance)).To(Succeed())
			})

			configMapName := fmt.Sprintf("cm-%s", instanceName)
			configMap := &corev1.ConfigMap{}
			Eventually(func(g Gomega) {
				err := testEnv.Client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, configMap)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(configMap.Data).To(HaveKeyWithValue("key", "before-restart"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			updateRGDDataDefaultInEnv(ctx, testEnv, rgdName, "restart-value-2")
			Eventually(func(g Gomega) {
				fresh := &krov1alpha1.ResourceGraphDefinition{}
				err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				g.Expect(fresh.Status.LastIssuedRevision).To(Equal(int64(2)))
				g.Expect(maxGraphRevisionNumber(listGraphRevisionsInEnv(ctx, testEnv, rgdName))).To(Equal(int64(2)))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(testEnv.RestartControllers()).To(Succeed())

			Consistently(func(g Gomega) {
				g.Expect(maxGraphRevisionNumber(listGraphRevisionsInEnv(ctx, testEnv, rgdName))).To(Equal(int64(2)))
			}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			expectExactRGDConditionsInEnv(ctx, testEnv, rgdName, 100*time.Millisecond, exactRGDExpectationInEnv{
				state:      krov1alpha1.ResourceGraphDefinitionStateActive,
				lastIssued: new(int64(2)),
				conditions: map[krov1alpha1.ConditionType]conditionExpectationInEnv{
					krov1alpha1.ConditionType(apis.ConditionReady): {
						status:  metav1.ConditionTrue,
						reason:  apis.ConditionReady,
						message: "",
					},
					krov1alpha1.ConditionType(resourcegraphdefinition.GraphAccepted): {
						status:  metav1.ConditionTrue,
						reason:  "Valid",
						message: "resource graph and schema are valid",
					},
					krov1alpha1.ConditionType(resourcegraphdefinition.GraphRevisionsResolved): {
						status:  metav1.ConditionTrue,
						reason:  "Resolved",
						message: "revision 2 compiled and active",
					},
				},
			})

			Eventually(func(g Gomega) {
				current := &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
						"kind":       kind,
					},
				}
				err := testEnv.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, current)
				g.Expect(err).ToNot(HaveOccurred())
				current.Object["spec"] = map[string]interface{}{"data": "after-restart"}
				err = testEnv.Client.Update(ctx, current)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Eventually(func(g Gomega) {
				err := testEnv.Client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, configMap)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(configMap.Data).To(HaveKeyWithValue("key", "after-restart"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			updateRGDDataDefaultInEnv(ctx, testEnv, rgdName, "restart-value-3")
			Eventually(func(g Gomega) {
				fresh := &krov1alpha1.ResourceGraphDefinition{}
				err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(fresh.Status.LastIssuedRevision).To(Equal(int64(3)))
				g.Expect(maxGraphRevisionNumber(listGraphRevisionsInEnv(ctx, testEnv, rgdName))).To(Equal(int64(3)))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		},
	)

	It("should issue monotonic revisions under rapid spec churn", func(ctx SpecContext) {
		testEnv := newIsolatedGraphRevisionEnv(ctx, 50)
		rgdName := fmt.Sprintf("gv-churn-%s", rand.String(5))
		kind := fmt.Sprintf("GvChurn%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(testEnv.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActiveInEnv(ctx, testEnv, rgdName)

		const updates = 8
		for i := 1; i <= updates; i++ {
			updateRGDDataDefaultInEnv(ctx, testEnv, rgdName, fmt.Sprintf("churn-%d", i))
		}

		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			gvs := nonTerminatingGraphRevisionsInEnv(ctx, testEnv, rgdName)
			latest := maxGraphRevisionNumber(gvs)
			g.Expect(latest).To(BeNumerically(">=", int64(2)))
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(latest))
			g.Expect(gvs).To(HaveLen(int(latest)))

			seen := map[int64]struct{}{}
			var latestHash string
			for _, gv := range gvs {
				seen[gv.Spec.Revision] = struct{}{}
				if gv.Spec.Revision == latest {
					latestHash = mustComputeSpecHash(gv.Spec.Snapshot.Spec)
				}
			}
			for revision := int64(1); revision <= latest; revision++ {
				_, ok := seen[revision]
				g.Expect(ok).To(BeTrue(), fmt.Sprintf("missing revision %d", revision))
			}

			currentHash, hashErr := graphhash.Spec(fresh.Spec)
			g.Expect(hashErr).ToNot(HaveOccurred())
			g.Expect(latestHash).To(Equal(currentHash))
		}, 40*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should not issue phantom revisions across repeated restart and update cycles", func(ctx SpecContext) {
		testEnv := newIsolatedGraphRevisionEnv(ctx, 20)
		namespace := fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(testEnv.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, ns)).To(Succeed())
		})

		rgdName := fmt.Sprintf("gv-restart-churn-%s", rand.String(5))
		kind := fmt.Sprintf("GvRestartChurn%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		Expect(testEnv.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, rgd)).To(Succeed())
		})

		waitForRGDActiveInEnv(ctx, testEnv, rgdName)

		const cycles = 3
		for cycle := 1; cycle <= cycles; cycle++ {
			expectedLatest := int64(cycle + 1)
			updateRGDDataDefaultInEnv(ctx, testEnv, rgdName, fmt.Sprintf("restart-churn-%d", cycle))

			Eventually(func(g Gomega) {
				fresh := &krov1alpha1.ResourceGraphDefinition{}
				err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				g.Expect(fresh.Status.LastIssuedRevision).To(Equal(expectedLatest))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(testEnv.RestartControllers()).To(Succeed())

			Consistently(func(g Gomega) {
				g.Expect(maxGraphRevisionNumber(listGraphRevisionsInEnv(ctx, testEnv, rgdName))).To(Equal(expectedLatest))
			}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

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
					"data": "post-restart",
				},
			},
		}
		Expect(testEnv.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(testEnv.Client.Delete(ctx, instance)).To(Succeed())
		})

		configMapName := fmt.Sprintf("cm-%s", instanceName)
		configMap := &corev1.ConfigMap{}
		Eventually(func(g Gomega) {
			err := testEnv.Client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, configMap)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMap.Data).To(HaveKeyWithValue("key", "post-restart"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		finalExpectedLatest := int64(cycles + 1)
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(finalExpectedLatest))
			g.Expect(maxGraphRevisionNumber(listGraphRevisionsInEnv(ctx, testEnv, rgdName))).To(Equal(finalExpectedLatest))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})

// Helpers — self-contained for this suite.

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

func mustComputeSpecHash(spec krov1alpha1.ResourceGraphDefinitionSpec) string {
	h, err := graphhash.Spec(spec)
	if err != nil {
		panic(fmt.Sprintf("failed to compute spec hash: %v", err))
	}
	return h
}

func newIsolatedGraphRevisionEnv(ctx SpecContext, maxGraphRevisions int) *environment.Environment {
	testEnv, err := environment.New(ctx, environment.ControllerConfig{
		AllowCRDDeletion: true,
		ReconcileConfig: ctrlinstance.ReconcileConfig{
			DefaultRequeueDuration: 5 * time.Second,
		},
		MaxGraphRevisions: maxGraphRevisions,
		LogWriter:         GinkgoWriter,
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		Expect(stopEnvironmentWithRetry(testEnv)).To(Succeed())
	})
	return testEnv
}

func stopEnvironmentWithRetry(testEnv *environment.Environment) error {
	sleepTime := 1 * time.Millisecond
	var err error
	for i := 0; i < 12; i++ {
		if err = testEnv.Stop(); err == nil {
			return nil
		}
		sleepTime *= 2
		time.Sleep(sleepTime)
	}
	return err
}

func waitForRGDActiveInEnv(ctx SpecContext, testEnv *environment.Environment, name string) {
	Eventually(func(g Gomega) {
		rgd := &krov1alpha1.ResourceGraphDefinition{}
		err := testEnv.Client.Get(ctx, types.NamespacedName{Name: name}, rgd)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
	}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

func listGraphRevisionsInEnv(
	ctx SpecContext,
	testEnv *environment.Environment,
	rgdName string,
) []internalv1alpha1.GraphRevision {
	list := &internalv1alpha1.GraphRevisionList{}
	sel := labels.SelectorFromSet(map[string]string{
		metadata.ResourceGraphDefinitionNameLabel: rgdName,
	})
	ExpectWithOffset(1, testEnv.Client.List(ctx, list, &client.ListOptions{LabelSelector: sel})).To(Succeed())
	return list.Items
}

func nonTerminatingGraphRevisionsInEnv(
	ctx SpecContext,
	testEnv *environment.Environment,
	rgdName string,
) []internalv1alpha1.GraphRevision {
	return nonTerminatingGraphRevisions(listGraphRevisionsInEnv(ctx, testEnv, rgdName))
}

func nonTerminatingGraphRevisions(gvs []internalv1alpha1.GraphRevision) []internalv1alpha1.GraphRevision {
	kept := make([]internalv1alpha1.GraphRevision, 0, len(gvs))
	for _, gv := range gvs {
		if gv.GetDeletionTimestamp().IsZero() {
			kept = append(kept, gv)
		}
	}
	return kept
}

func updateRGDDataDefaultInEnv(ctx SpecContext, testEnv *environment.Environment, rgdName, value string) {
	Eventually(func(g Gomega) {
		fresh := &krov1alpha1.ResourceGraphDefinition{}
		err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
		g.Expect(err).ToNot(HaveOccurred())
		fresh.Spec.Schema.Spec.Raw = []byte(fmt.Sprintf(`{"data":"string | default=%s"}`, value))
		err = testEnv.Client.Update(ctx, fresh)
		g.Expect(err).ToNot(HaveOccurred())
	}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

func updateRGDTemplateLabelInEnv(ctx SpecContext, testEnv *environment.Environment, rgdName, label string) {
	Eventually(func(g Gomega) {
		fresh := &krov1alpha1.ResourceGraphDefinition{}
		err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
		g.Expect(err).ToNot(HaveOccurred())
		template := `{"apiVersion":"v1","kind":"ConfigMap",` +
			`"metadata":{"name":"cm-${schema.metadata.name}",` +
			`"labels":{"revision":"%s"}},"data":{"key":"${schema.spec.data}"}}`
		fresh.Spec.Resources[0].Template.Raw = []byte(fmt.Sprintf(template, label))
		err = testEnv.Client.Update(ctx, fresh)
		g.Expect(err).ToNot(HaveOccurred())
	}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}

func graphRevisionNumbers(gvs []internalv1alpha1.GraphRevision) []int64 {
	numbers := make([]int64, 0, len(gvs))
	for _, gv := range gvs {
		numbers = append(numbers, gv.Spec.Revision)
	}
	return numbers
}

func maxGraphRevisionNumber(gvs []internalv1alpha1.GraphRevision) int64 {
	var maxRevision int64
	for _, gv := range gvs {
		if gv.Spec.Revision > maxRevision {
			maxRevision = gv.Spec.Revision
		}
	}
	return maxRevision
}

func expectedRetainedRevisionNumbers(latest int64) []int64 {
	start := latest - int64(isolatedGraphRevisionRetentionLimit) + 1
	numbers := make([]int64, 0, isolatedGraphRevisionRetentionLimit)
	for revision := start; revision <= latest; revision++ {
		numbers = append(numbers, revision)
	}
	return numbers
}

type exactRGDExpectationInEnv struct {
	state      krov1alpha1.ResourceGraphDefinitionState
	lastIssued *int64
	conditions map[krov1alpha1.ConditionType]conditionExpectationInEnv
}

type conditionExpectationInEnv struct {
	status                   metav1.ConditionStatus
	reason                   string
	message                  string
	observedGenerationOffset int64
}

func expectExactRGDConditionsInEnv(
	ctx SpecContext,
	testEnv *environment.Environment,
	rgdName string,
	interval time.Duration,
	want exactRGDExpectationInEnv,
) {
	Eventually(func(g Gomega) {
		fresh := &krov1alpha1.ResourceGraphDefinition{}
		err := testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
		g.Expect(err).ToNot(HaveOccurred())

		if want.state != "" {
			g.Expect(fresh.Status.State).To(Equal(want.state))
		}
		if want.lastIssued != nil {
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(*want.lastIssued))
		}
		for conditionType, expectation := range want.conditions {
			assertConditionContractInEnv(g, fresh, conditionType, expectation)
		}
	}, 20*time.Second, interval).WithContext(ctx).Should(Succeed())
}

func assertConditionContractInEnv(
	g Gomega,
	obj apis.Object,
	conditionType krov1alpha1.ConditionType,
	want conditionExpectationInEnv,
) {
	g.ExpectWithOffset(1, obj.GetGeneration()).To(BeNumerically(">", 0))

	cond := findConditionByTypeInEnv(obj.GetConditions(), conditionType)
	g.ExpectWithOffset(1, cond).ToNot(BeNil(), "expected condition %s", conditionType)
	g.ExpectWithOffset(1, cond.Status).To(Equal(want.status), "unexpected status for condition %s", conditionType)
	g.ExpectWithOffset(1, cond.ObservedGeneration).To(
		Equal(obj.GetGeneration()+want.observedGenerationOffset),
		"unexpected observedGeneration for condition %s",
		conditionType,
	)
	g.ExpectWithOffset(1, cond.LastTransitionTime).ToNot(
		BeNil(),
		"LastTransitionTime must be set for condition %s",
		conditionType,
	)
	g.ExpectWithOffset(1, cond.LastTransitionTime.Time).ToNot(
		BeZero(),
		"LastTransitionTime must not be zero for condition %s",
		conditionType,
	)
	g.ExpectWithOffset(1, cond.Reason).ToNot(BeNil(), "Reason must be set for condition %s", conditionType)
	g.ExpectWithOffset(1, cond.Message).ToNot(BeNil(), "Message must be set for condition %s", conditionType)
	g.ExpectWithOffset(1, *cond.Reason).To(Equal(want.reason), "unexpected reason for condition %s", conditionType)
	g.ExpectWithOffset(1, *cond.Message).To(Equal(want.message), "unexpected message for condition %s", conditionType)
}

func findConditionByTypeInEnv(conditions []krov1alpha1.Condition, t krov1alpha1.ConditionType) *krov1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == t {
			return &conditions[i]
		}
	}
	return nil
}
