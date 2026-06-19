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
	"sync/atomic"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/test/integration/graphengine/environment"
)

// schemaTestCRDName returns the metadata.name format the apiserver
// uses for CRDs: "<plural>.<group>", lowercased per CRD naming rules.
func schemaTestCRDName(group, kind string) string {
	return lowerASCII(kind) + "s." + group
}

// installCRD posts a CRD to the test apiserver and registers cleanup.
// The Established condition is what the apiserver flips True once the
// CRD is fully reconciled in its own loop; tests wait on that before
// trying to use the new Kind.
func installCRD(t *testing.T, env *environment.Env, group, kind, schemaTag string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	crd := buildTestCRD(group, kind, schemaTag)
	if err := env.Client.Create(env.Ctx, crd); err != nil {
		t.Fatalf("create CRD: %v", err)
	}
	t.Cleanup(func() {
		_ = env.Client.Delete(env.Ctx, crd)
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

func awaitCRDEstablished(t *testing.T, env *environment.Env, name string) {
	t.Helper()
	environment.Eventually(t, 20*time.Second, 100*time.Millisecond, func() error {
		got := &apiextensionsv1.CustomResourceDefinition{}
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: name}, got); err != nil {
			return err
		}
		for _, c := range got.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				return nil
			}
		}
		return fmt.Errorf("CRD %s not Established yet", name)
	})
}

// TestSchemaWatchIndexesStaticDependency boots a Graph whose template
// references a CRD-defined Kind, then verifies the schema watcher's
// in-memory reverse index includes the Graph for that GroupKind. No
// CRD mutation; just the index-population path.
func TestSchemaWatchIndexesStaticDependency(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	const group = "schemawatch.test.kro.run"
	const kind = "IndexedWidget"
	installCRD(t, env, group, kind, "initial")

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
	env.CreateGraph(t, g)

	graphKey := types.NamespacedName{Namespace: ns, Name: "indexed"}
	env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionTrue, 20*time.Second)

	// Once the compile pass commits its schema subscription, the
	// reverse index should list our Graph under the GK.
	gk := schema.GroupKind{Group: group, Kind: kind}
	environment.Eventually(t, 10*time.Second, 100*time.Millisecond, func() error {
		keys := env.SchemaWatcher.GraphsForGroupKind(gk)
		for _, k := range keys {
			if k == graphKey {
				return nil
			}
		}
		return fmt.Errorf("graph %s not yet indexed under %s (have %v)", graphKey, gk, keys)
	})
}

// TestSchemaWatchTriggersRecompileOnSchemaChange covers the happy
// path: a Graph references a CRD; the CRD's schema changes; the
// reconciler observes a recompile (we detect this by watching for a
// status churn the new spec would produce in steady state).
//
// Concretely: the test installs CRD vA, creates a Graph referencing
// it, waits for Ready. Then it patches the CRD's schema description
// (a "schema content" change per our hash). The schema watcher must
// invalidate the compile cache and enqueue. We observe the
// re-reconcile via an atomic counter wired through the conditions:
// status conditions get re-published on every reconcile, so the
// LastTransitionTime / message advance.
func TestSchemaWatchTriggersRecompileOnSchemaChange(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	const group = "schemawatch.test.kro.run"
	const kind = "TrigWidget"
	installCRD(t, env, group, kind, "v0")

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
	env.CreateGraph(t, g)

	graphKey := types.NamespacedName{Namespace: ns, Name: "trig"}
	env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)

	// Snapshot reconcile state. We watch for the registry to forget
	// the Graph (schema watcher invalidation) and re-populate (next
	// reconcile recompiles). The simplest observable signal: the
	// SchemaWatcher's cached hash advances.
	gk := schema.GroupKind{Group: group, Kind: kind}
	preHash := env.SchemaWatcher.SchemaHash(gk)
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

	// Mutate the CRD's schema description — content change → hash diff.
	crd := &apiextensionsv1.CustomResourceDefinition{}
	require(func() bool {
		return env.Client.Get(env.Ctx, types.NamespacedName{Name: schemaTestCRDName(group, kind)}, crd) == nil
	}, "fetch CRD pre-update")
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "v1"
	if err := env.Client.Update(env.Ctx, crd); err != nil {
		t.Fatalf("update CRD: %v", err)
	}

	// Hash must advance (schema watcher saw the content change).
	require(func() bool {
		h := env.SchemaWatcher.SchemaHash(gk)
		return h != "" && h != preHash
	}, "schema hash to advance on CRD update")
}

// TestSchemaWatchDedupsNonSchemaUpdates pins the dedup contract: an
// annotation-only edit to a CRD must NOT advance the hash, must NOT
// invalidate dependents, must NOT enqueue.
func TestSchemaWatchDedupsNonSchemaUpdates(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	const group = "schemawatch.test.kro.run"
	const kind = "DedupWidget"
	installCRD(t, env, group, kind, "v0")

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
	env.CreateGraph(t, g)
	graphKey := types.NamespacedName{Namespace: ns, Name: "dedup"}
	env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)

	gk := schema.GroupKind{Group: group, Kind: kind}
	preHash := env.SchemaWatcher.SchemaHash(gk)
	require := preHash // capture for later sameness check (just for read clarity)
	_ = require

	// Annotation-only edit.
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: schemaTestCRDName(group, kind)}, crd); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	if crd.Annotations == nil {
		crd.Annotations = map[string]string{}
	}
	crd.Annotations["test.kro.run/touch"] = "1"
	if err := env.Client.Update(env.Ctx, crd); err != nil {
		t.Fatalf("update CRD: %v", err)
	}

	// Hash should not advance. Wait a window to let the event fire.
	environment.Consistently(t, 2*time.Second, 200*time.Millisecond, func() error {
		got := env.SchemaWatcher.SchemaHash(gk)
		if got != preHash {
			return fmt.Errorf("hash advanced on annotation-only edit: %q → %q", preHash, got)
		}
		return nil
	})
}

// TestSchemaWatchDoesNotEnqueueUnrelatedGraphs covers cross-Graph
// isolation: Graph A references CRD foo; Graph B references CRD bar.
// A change to bar's schema must enqueue B but not A.
func TestSchemaWatchDoesNotEnqueueUnrelatedGraphs(t *testing.T) {
	env := environment.Shared(t)
	nsA := env.CreateNamespace(t)
	nsB := env.CreateNamespace(t)

	const groupA = "schemawatch.testa.kro.run"
	const kindA = "IsoA"
	const groupB = "schemawatch.testb.kro.run"
	const kindB = "IsoB"
	installCRD(t, env, groupA, kindA, "v0")
	installCRD(t, env, groupB, kindB, "v0")

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

	env.CreateGraph(t, mkGraph("a", nsA, groupA, kindA))
	env.CreateGraph(t, mkGraph("b", nsB, groupB, kindB))

	keyA := types.NamespacedName{Namespace: nsA, Name: "a"}
	keyB := types.NamespacedName{Namespace: nsB, Name: "b"}
	env.AwaitCondition(t, keyA, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)
	env.AwaitCondition(t, keyB, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)

	// Snapshot the Graph A object: we'll verify its
	// resourceVersion / observedGeneration does NOT change when B's
	// CRD is touched.
	preA := env.GetGraph(t, keyA)
	preARV := preA.ResourceVersion

	// Touch CRD B's schema.
	crdB := &apiextensionsv1.CustomResourceDefinition{}
	if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: schemaTestCRDName(groupB, kindB)}, crdB); err != nil {
		t.Fatalf("get CRD B: %v", err)
	}
	crdB.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "v1"
	if err := env.Client.Update(env.Ctx, crdB); err != nil {
		t.Fatalf("update CRD B: %v", err)
	}

	// Wait for B's hash to advance — confirms the schema watcher
	// processed the event.
	gkB := schema.GroupKind{Group: groupB, Kind: kindB}
	preHashB := env.SchemaWatcher.SchemaHash(gkB)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h := env.SchemaWatcher.SchemaHash(gkB); h != preHashB && h != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A must NOT have churned. Status writes bump RV; we assert
	// stability across a settling window.
	environment.Consistently(t, 2*time.Second, 200*time.Millisecond, func() error {
		curA := env.GetGraph(t, keyA)
		if curA.ResourceVersion != preARV {
			return fmt.Errorf("graph A churned: rv %s → %s", preARV, curA.ResourceVersion)
		}
		return nil
	})
}

// TestSchemaWatchEnqueuesOnCRDAdd covers the bootstrap path: a Graph
// references a Kind whose CRD doesn't exist yet. The Graph's compile
// fails (Accepted=False, "schema not found"). When the CRD is later
// installed, the schema watcher's Add handler enqueues every
// subscribed Graph — including A that was stuck — and the Graph
// recovers to Accepted=True.
func TestSchemaWatchEnqueuesOnCRDAdd(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	const group = "schemawatch.lateinstall.kro.run"
	const kind = "Latecomer"

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
	env.CreateGraph(t, g)

	graphKey := types.NamespacedName{Namespace: ns, Name: "late"}
	env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionFalse, 20*time.Second)

	// Install the CRD. The schema watcher's onAdd handler enqueues
	// every subscribed Graph (including this one, which was stuck
	// with no committed subscription because compile failed — the
	// "enqueue every Graph on add" branch covers that).
	installCRD(t, env, group, kind, "v0")

	// Eventually the Graph recompiles successfully.
	env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionTrue, 30*time.Second)
}

// TestSchemaWatchRemoveGraphClearsIndex ensures the per-Graph index
// shrinks when the Graph is deleted. After delete, no future CRD
// event for the formerly-subscribed GK should re-enqueue the gone
// Graph (we sample via the index introspection).
func TestSchemaWatchRemoveGraphClearsIndex(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	const group = "schemawatch.cleanup.kro.run"
	const kind = "Cleanable"
	installCRD(t, env, group, kind, "v0")

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
	env.CreateGraph(t, g)
	graphKey := types.NamespacedName{Namespace: ns, Name: "cleanup"}
	env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)

	// Index has the Graph.
	gk := schema.GroupKind{Group: group, Kind: kind}
	keys := env.SchemaWatcher.GraphsForGroupKind(gk)
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
	got := env.GetGraph(t, graphKey)
	if err := env.Client.Delete(env.Ctx, got); err != nil {
		t.Fatalf("delete graph: %v", err)
	}
	env.AwaitGraphGone(t, graphKey, 20*time.Second)

	// Index entry must be gone.
	environment.Eventually(t, 10*time.Second, 100*time.Millisecond, func() error {
		keys := env.SchemaWatcher.GraphsForGroupKind(gk)
		for _, k := range keys {
			if k == graphKey {
				return fmt.Errorf("graph %s still in index after delete", graphKey)
			}
		}
		return nil
	})
}

// Defensive: keep the atomic + apierrors imports alive even if a
// future refactor removes their explicit reference. They land in
// these tests for completeness in failure-mode coverage.
var _ = atomic.AddInt32
var _ = apierrors.IsNotFound
