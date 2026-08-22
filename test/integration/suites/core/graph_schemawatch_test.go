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
	"context"
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

func getSchemaWatchEnv(t environment.TestingT) *environment.Environment {
	if env != nil && env.SchemaWatcher != nil {
		return env
	}
	testEnv, err := environment.New(context.Background(), environment.ControllerConfig{
		AllowCRDDeletion: true,
		LogWriter:        GinkgoWriter,
	})
	if err != nil {
		t.Fatalf("failed to create isolated env for schema watch: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })
	return testEnv
}

// schemaTestCRDName returns the metadata.name format the apiserver
// uses for CRDs: "<plural>.<group>", lowercased per CRD naming rules.
func schemaTestCRDName(group, kind string) string {
	return lowerASCII(kind) + "s." + group
}

// installCRD posts a CRD to the test apiserver and registers cleanup.
// The Established condition is what the apiserver flips True once the
// CRD is fully reconciled in its own loop; tests wait on that before
// trying to use the new Kind.
func installCRD(t environment.TestingT, env *environment.Environment, group, kind, schemaTag string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	crd := buildTestCRD(group, kind, schemaTag)
	ctx := env.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := env.Client.Create(ctx, crd); err != nil {
		t.Fatalf("create CRD: %v", err)
	}
	t.Cleanup(func() {
		_ = env.Client.Delete(context.Background(), crd)
	})
	awaitCRDEstablished(t, env, crd.Name)
	return crd
}

// buildTestCRD constructs an apiextensionsv1.CRD object suitable for
// pushing through the envtest apiserver. The "schemaTag" string is
// stashed in the schema's description so we can make distinct edits
// that change the schema-content hash.
func buildTestCRD(group, kind, schemaTag string) *apiextensionsv1.CustomResourceDefinition {
	plural := lowerASCII(kind) + "s"
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: plural + "." + group,
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     kind,
				ListKind: kind + "List",
				Plural:   plural,
				Singular: lowerASCII(kind),
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1",
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:        "object",
						Description: schemaTag,
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"value": {Type: "string"},
								},
							},
						},
					},
				},
			}},
		},
	}
}

func lowerASCII(s string) string {
	out := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out[i] = c
	}
	return string(out)
}

var _ = Describe("Graph Schema Watch", func() {
	It("indexes static dependencies in the schema watcher reverse index", func() {
		t := GinkgoT()
		testEnv := getSchemaWatchEnv(t)
		ns := testEnv.CreateNamespace(t)

		group := fmt.Sprintf("schemawatch-%s.kro.run", rand.String(5))
		kind := fmt.Sprintf("IndexedWidget%s", rand.String(5))
		installCRD(t, testEnv, group, kind, "initial")

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "indexed", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "widget",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": group + "/v1",
						"kind":       kind,
						"metadata":   map[string]any{"name": "w"},
						"spec":       map[string]any{"value": "x"},
					}),
				}},
			},
		}
		testEnv.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "indexed"}
		testEnv.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionTrue, 20*time.Second)

		// Once the compile pass commits its schema subscription, the
		// reverse index should list our Graph under the GK.
		gk := schema.GroupKind{Group: group, Kind: kind}
		environment.Eventually(t, 10*time.Second, 100*time.Millisecond, func() error {
			keys := testEnv.SchemaWatcher.GraphsForGroupKind(gk)
			for _, k := range keys {
				if k == graphKey {
					return nil
				}
			}
			return fmt.Errorf("graph %s not yet indexed under %s (have %v)", graphKey, gk, keys)
		})
	})

	It("triggers recompile on schema content change", func() {
		t := GinkgoT()
		testEnv := getSchemaWatchEnv(t)
		ns := testEnv.CreateNamespace(t)

		group := fmt.Sprintf("schemawatch-%s.kro.run", rand.String(5))
		kind := fmt.Sprintf("TrigWidget%s", rand.String(5))
		installCRD(t, testEnv, group, kind, "v0")

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "trig", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "w",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": group + "/v1",
						"kind":       kind,
						"metadata":   map[string]any{"name": "w"},
						"spec":       map[string]any{"value": "stable"},
					}),
				}},
			},
		}
		testEnv.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "trig"}
		testEnv.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)

		// Snapshot reconcile state.
		gk := schema.GroupKind{Group: group, Kind: kind}
		preHash := testEnv.SchemaWatcher.SchemaHash(gk)
		require := func(cond func() bool, msg string) {
			t.Helper()
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				if cond() {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatalf("timeout waiting for %s", msg)
		}

		ctx := testEnv.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		// Mutate the CRD's schema description — content change → hash diff.
		crd := &apiextensionsv1.CustomResourceDefinition{}
		require(func() bool {
			return testEnv.Client.Get(ctx, types.NamespacedName{Name: schemaTestCRDName(group, kind)}, crd) == nil
		}, "fetch CRD pre-update")
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "v1"
		if err := testEnv.Client.Update(ctx, crd); err != nil {
			t.Fatalf("update CRD: %v", err)
		}

		// Hash must advance (schema watcher saw the content change).
		require(func() bool {
			h := testEnv.SchemaWatcher.SchemaHash(gk)
			return h != "" && h != preHash
		}, "schema hash to advance on CRD update")
	})

	It("deduplicates non-schema updates to CRD", func() {
		t := GinkgoT()
		testEnv := getSchemaWatchEnv(t)
		ns := testEnv.CreateNamespace(t)

		group := fmt.Sprintf("schemawatch-%s.kro.run", rand.String(5))
		kind := fmt.Sprintf("DedupWidget%s", rand.String(5))
		installCRD(t, testEnv, group, kind, "v0")

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "dedup", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "w",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": group + "/v1",
						"kind":       kind,
						"metadata":   map[string]any{"name": "w"},
						"spec":       map[string]any{"value": "x"},
					}),
				}},
			},
		}
		testEnv.CreateGraph(t, g)
		graphKey := types.NamespacedName{Namespace: ns, Name: "dedup"}
		testEnv.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)

		gk := schema.GroupKind{Group: group, Kind: kind}
		preHash := testEnv.SchemaWatcher.SchemaHash(gk)

		ctx := testEnv.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		// Annotation-only edit.
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: schemaTestCRDName(group, kind)}, crd); err != nil {
			t.Fatalf("get CRD: %v", err)
		}
		if crd.Annotations == nil {
			crd.Annotations = map[string]string{}
		}
		crd.Annotations["test.kro.run/touch"] = "1"
		if err := testEnv.Client.Update(ctx, crd); err != nil {
			t.Fatalf("update CRD: %v", err)
		}

		// Hash should not advance.
		environment.Consistently(t, 2*time.Second, 200*time.Millisecond, func() error {
			got := testEnv.SchemaWatcher.SchemaHash(gk)
			if got != preHash {
				return fmt.Errorf("hash advanced on annotation-only edit: %q → %q", preHash, got)
			}
			return nil
		})
	})

	It("does not enqueue unrelated graphs when an independent CRD changes", func() {
		t := GinkgoT()
		testEnv := getSchemaWatchEnv(t)
		nsA := testEnv.CreateNamespace(t)
		nsB := testEnv.CreateNamespace(t)

		groupA := fmt.Sprintf("schemawatch-%s.kro.run", rand.String(5))
		kindA := fmt.Sprintf("IsoA%s", rand.String(5))
		groupB := fmt.Sprintf("schemawatch-%s.kro.run", rand.String(5))
		kindB := fmt.Sprintf("IsoB%s", rand.String(5))
		installCRD(t, testEnv, groupA, kindA, "v0")
		installCRD(t, testEnv, groupB, kindB, "v0")

		mkGraph := func(name, namespace, group, kind string) *expv1alpha1.Graph {
			return &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: expv1alpha1.GraphSpec{
					Nodes: []expv1alpha1.Node{{
						ID: "w",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": group + "/v1",
							"kind":       kind,
							"metadata":   map[string]any{"name": "w"},
							"spec":       map[string]any{"value": "x"},
						}),
					}},
				},
			}
		}

		testEnv.CreateGraph(t, mkGraph("a", nsA, groupA, kindA))
		testEnv.CreateGraph(t, mkGraph("b", nsB, groupB, kindB))

		keyA := types.NamespacedName{Namespace: nsA, Name: "a"}
		keyB := types.NamespacedName{Namespace: nsB, Name: "b"}
		testEnv.AwaitCondition(t, keyA, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)
		testEnv.AwaitCondition(t, keyB, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)

		// Allow any trailing in-flight initial reconcile to settle before snapshotting.
		time.Sleep(500 * time.Millisecond)

		// Snapshot Graph A object.
		preA := testEnv.GetGraph(t, keyA)
		preARV := preA.ResourceVersion

		ctx := testEnv.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		// Touch CRD B's schema.
		crdB := &apiextensionsv1.CustomResourceDefinition{}
		if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: schemaTestCRDName(groupB, kindB)}, crdB); err != nil {
			t.Fatalf("get CRD B: %v", err)
		}
		crdB.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "v1"
		if err := testEnv.Client.Update(ctx, crdB); err != nil {
			t.Fatalf("update CRD B: %v", err)
		}

		// Wait for B's hash to advance.
		gkB := schema.GroupKind{Group: groupB, Kind: kindB}
		preHashB := testEnv.SchemaWatcher.SchemaHash(gkB)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if h := testEnv.SchemaWatcher.SchemaHash(gkB); h != preHashB && h != "" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		// A must NOT have churned.
		environment.Consistently(t, 2*time.Second, 200*time.Millisecond, func() error {
			curA := testEnv.GetGraph(t, keyA)
			if curA.ResourceVersion != preARV {
				return fmt.Errorf("graph A churned: rv %s → %s", preARV, curA.ResourceVersion)
			}
			return nil
		})
	})

	It("enqueues on late CRD addition to unblock compilation", func() {
		t := GinkgoT()
		testEnv := getSchemaWatchEnv(t)
		ns := testEnv.CreateNamespace(t)

		group := fmt.Sprintf("schemawatch-%s.kro.run", rand.String(5))
		kind := fmt.Sprintf("Latecomer%s", rand.String(5))

		// Graph references a Kind that doesn't exist yet. The first
		// reconcile fails to compile. Accepted should flip to False.
		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "late", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "w",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": group + "/v1",
						"kind":       kind,
						"metadata":   map[string]any{"name": "w"},
						"spec":       map[string]any{"value": "x"},
					}),
				}},
			},
		}
		testEnv.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "late"}
		testEnv.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionFalse, 20*time.Second)

		// Install the CRD.
		installCRD(t, testEnv, group, kind, "v0")

		// Eventually the Graph recompiles successfully.
		testEnv.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionTrue, 30*time.Second)
	})

	It("clears schema index when Graph is deleted", func() {
		t := GinkgoT()
		testEnv := getSchemaWatchEnv(t)
		ns := testEnv.CreateNamespace(t)

		group := fmt.Sprintf("schemawatch-%s.kro.run", rand.String(5))
		kind := fmt.Sprintf("Cleanable%s", rand.String(5))
		installCRD(t, testEnv, group, kind, "v0")

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "cleanup", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "w",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": group + "/v1",
						"kind":       kind,
						"metadata":   map[string]any{"name": "w"},
						"spec":       map[string]any{"value": "x"},
					}),
				}},
			},
		}
		testEnv.CreateGraph(t, g)
		graphKey := types.NamespacedName{Namespace: ns, Name: "cleanup"}
		testEnv.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)

		// Index has the Graph.
		gk := schema.GroupKind{Group: group, Kind: kind}
		keys := testEnv.SchemaWatcher.GraphsForGroupKind(gk)
		found := false
		for _, k := range keys {
			if k == graphKey {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected graph %s in index for %s, got %v", graphKey, gk, keys)
		}

		// Delete the Graph.
		got := testEnv.GetGraph(t, graphKey)
		ctx := testEnv.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := testEnv.Client.Delete(ctx, got); err != nil {
			t.Fatalf("delete graph: %v", err)
		}
		testEnv.AwaitGraphGone(t, graphKey, 20*time.Second)

		// Index entry must be gone.
		environment.Eventually(t, 10*time.Second, 100*time.Millisecond, func() error {
			keys := testEnv.SchemaWatcher.GraphsForGroupKind(gk)
			for _, k := range keys {
				if k == graphKey {
					return fmt.Errorf("graph %s still in index after delete", graphKey)
				}
			}
			return nil
		})
	})
})

var _ = atomic.AddInt32
var _ = apierrors.IsNotFound
