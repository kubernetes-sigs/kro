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
	"time"

	. "github.com/onsi/ginkgo/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

var _ = Describe("Graph RGD Deletion", func() {
	It("tears resources down in reverse-applied (dependents-first) order", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		rgdName := fmt.Sprintf("webapp-%s-del", rand.String(5))
		kindName := fmt.Sprintf("WebAppDel%s", rand.String(5))

		// ── 1. Build and serve the CRD for the instance kind ────────────────────
		rgd := newWebAppRGD(rgdName, kindName, ns)
		ensureTestInstanceCRD(t, env, rgd)

		webAppGVR := schema.GroupVersionResource{
			Group:    "kro.run",
			Version:  "v1alpha1",
			Resource: lowerASCII(kindName) + "s",
		}
		environment.Eventually(t, 30*time.Second, 500*time.Millisecond, func() error {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "kro.run",
				Version: "v1alpha1",
				Kind:    kindName + "List",
			})
			ctx := env.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return env.Client.List(ctx, list)
		})
		_ = webAppGVR

		// ── 2. Create the instance ───────────────────────────────────────────────
		instance := &unstructured.Unstructured{}
		instance.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "kro.run",
			Version: "v1alpha1",
			Kind:    kindName,
		})
		instance.SetName("del-app")
		instance.SetNamespace(ns)
		if err := unstructured.SetNestedField(instance.Object, "del-app", "spec", "name"); err != nil {
			t.Fatalf("set spec.name: %v", err)
		}
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Create(ctx, instance); err != nil {
			t.Fatalf("create WebAppDel instance: %v", err)
		}
		t.Cleanup(func() { _ = env.Client.Delete(context.Background(), instance) })

		if err := env.Client.Get(ctx, types.NamespacedName{Name: "del-app", Namespace: ns}, instance); err != nil {
			t.Fatalf("get WebAppDel instance: %v", err)
		}

		// ── 3. Build the compiler and Runtime ───────────────────────────────────
		comp, err := compiler.NewCompiler(env.ClientSet.RESTConfig(), env.ClientSet.HTTPClient())
		if err != nil {
			t.Fatalf("build compiler: %v", err)
		}

		rt, g, err := rgdadapter.BuildRuntimeForInstance(rgd, instance, comp)
		if err != nil {
			t.Fatalf("BuildRuntimeForInstance: %v", err)
		}
		_ = g

		// ── 4. Apply ─────────────────────────────────────────────────────────────
		exec := executor.NewSimple(env.Client)
		result, err := exec.Apply(ctx, rt, watchrouter.NoopWatcher{})
		if err != nil {
			t.Fatalf("executor.Apply: %v", err)
		}
		if len(result.Applied) != 2 {
			t.Fatalf("Apply: want 2 resources, got %d (unresolved=%v)", len(result.Applied), result.Unresolved)
		}

		// ── 5. Assert Apply order: dependencies-first (cm1 before cm2) ───────────
		applied := result.Applied
		cm1Idx, cm2Idx := -1, -1
		for i, mr := range applied {
			switch mr.NodeID {
			case "cm1":
				cm1Idx = i
			case "cm2":
				cm2Idx = i
			}
		}
		if cm1Idx < 0 || cm2Idx < 0 {
			t.Fatalf("Applied list missing cm1 or cm2: %+v", applied)
		}
		if cm1Idx >= cm2Idx {
			t.Errorf("Apply order violation: cm1 (idx %d) must precede cm2 (idx %d) — dependencies-first", cm1Idx, cm2Idx)
		}

		// ── 6. Assert Delete order: dependents-first == reverse of Applied ────────
		deleteOrder := make([]string, len(applied))
		for i := range applied {
			deleteOrder[i] = applied[len(applied)-1-i].NodeID
		}
		if deleteOrder[0] != "cm2" || deleteOrder[1] != "cm1" {
			t.Errorf("Delete order (reverse-slice): want [cm2 cm1], got %v", deleteOrder)
		}

		// ── 7. Assert DAG.ReverseTopologicalLayers matches reverse-slice order ───
		g2, err := rgdadapter.ResourceGraphDefinitionToGraph(rgd)
		if err != nil {
			t.Fatalf("ResourceGraphDefinitionToGraph: %v", err)
		}
		schemaNode, err := rgdadapter.InstanceSchemaNode(instance)
		if err != nil {
			t.Fatalf("InstanceSchemaNode: %v", err)
		}
		g2.Spec.Nodes = append([]krov1alpha1.Node{schemaNode}, g2.Spec.Nodes...)

		prog2, err := comp.Compile(g2)
		if err != nil {
			t.Fatalf("Compile for DAG check: %v", err)
		}

		layers, err := prog2.DAG.ReverseTopologicalLayers()
		if err != nil {
			t.Fatalf("ReverseTopologicalLayers: %v", err)
		}

		var filteredLayers [][]string
		for _, layer := range layers {
			var filtered []string
			for _, id := range layer {
				if id != "schema" {
					filtered = append(filtered, id)
				}
			}
			if len(filtered) > 0 {
				filteredLayers = append(filteredLayers, filtered)
			}
		}

		if len(filteredLayers) < 2 {
			t.Fatalf("ReverseTopologicalLayers: want ≥2 layers (after filtering schema), got %d: %v", len(filteredLayers), filteredLayers)
		}
		cm2InLayer0 := false
		for _, id := range filteredLayers[0] {
			if id == "cm2" {
				cm2InLayer0 = true
			}
		}
		if !cm2InLayer0 {
			t.Errorf("ReverseTopologicalLayers parity: cm2 not in layer 0 (dependents-first); layers=%v", filteredLayers)
		}
		cm1Layer := -1
		for li, layer := range filteredLayers {
			for _, id := range layer {
				if id == "cm1" {
					cm1Layer = li
				}
			}
		}
		if cm1Layer <= 0 {
			t.Errorf("ReverseTopologicalLayers parity: cm1 not in a later layer than cm2; cm1Layer=%d, layers=%v", cm1Layer, filteredLayers)
		}

		// ── 8. Execute Delete and verify both resources are gone from the cluster ─
		if err := exec.Delete(ctx, applied); err != nil {
			t.Fatalf("executor.Delete: %v", err)
		}

		cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
		for _, name := range []string{"cm1", "cm2"} {
			environment.Eventually(t, 15*time.Second, 500*time.Millisecond, func() error {
				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(cmGVK)
				err := env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj)
				if apierrors.IsNotFound(err) {
					return nil
				}
				if err != nil {
					return err
				}
				return fmt.Errorf("ConfigMap %s still exists", name)
			})
		}
	})
})
