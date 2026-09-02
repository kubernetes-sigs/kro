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
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/crd"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

var _ = Describe("Graph RGD Reconcile", func() {
	It("applies resources translated from an RGD composition through executor.Simple.Apply", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		rgdName := fmt.Sprintf("webapp-%s", rand.String(5))
		kindName := fmt.Sprintf("WebApp%s", rand.String(5))

		// ── 1. Build and ensure the WebApp CRD ──────────────────────────────────
		rgd := newWebAppRGD(rgdName, kindName, ns)
		ensureTestInstanceCRD(t, env, rgd)

		// Wait for the CRD to be established before creating instances.
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

		// ── 2. Create a WebApp instance in the cluster ───────────────────────────
		instance := &unstructured.Unstructured{}
		instance.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "kro.run",
			Version: "v1alpha1",
			Kind:    kindName,
		})
		instance.SetName("demo-app")
		instance.SetNamespace(ns)
		if err := unstructured.SetNestedField(instance.Object, "demo-app", "spec", "name"); err != nil {
			t.Fatalf("set spec.name: %v", err)
		}

		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Create(ctx, instance); err != nil {
			t.Fatalf("create WebApp instance: %v", err)
		}
		t.Cleanup(func() {
			_ = env.Client.Delete(context.Background(), instance)
		})

		// Re-fetch to get the server-populated fields (UID, resourceVersion, etc.)
		if err := env.Client.Get(ctx, types.NamespacedName{Name: "demo-app", Namespace: ns}, instance); err != nil {
			t.Fatalf("get WebApp instance: %v", err)
		}

		// ── 3. Build the Runtime via BuildRuntimeForInstance ────────────────────
		comp, err := compiler.NewCompiler(env.ClientSet.RESTConfig(), env.ClientSet.HTTPClient())
		if err != nil {
			t.Fatalf("build compiler: %v", err)
		}

		rt, g, err := rgdadapter.BuildRuntimeForInstance(rgd, instance, comp)
		if err != nil {
			t.Fatalf("BuildRuntimeForInstance: %v", err)
		}

		// ── 4. Apply through executor.Simple ─────────────────────────────────────
		exec := executor.NewSimple(env.Client)
		result, err := exec.Apply(ctx, rt, watchrouter.NoopWatcher{})
		if err != nil {
			t.Fatalf("executor.Apply: %v", err)
		}

		// ── 5. Assert ApplyResult tracks both resources ──────────────────────────
		if len(result.Applied) != 2 {
			t.Fatalf("ApplyResult.Applied = %d resources, want 2; unresolved=%v",
				len(result.Applied), result.Unresolved)
		}
		if len(result.Unresolved) != 0 {
			t.Fatalf("ApplyResult.Unresolved = %v, want empty", result.Unresolved)
		}

		// ── 6. Assert against the live cluster ───────────────────────────────────
		cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

		// cm1: data.app should equal "demo-app" (resolved from instance spec via schema def node)
		env.AwaitObject(t, cmGVK, types.NamespacedName{Namespace: ns, Name: "cm1"}, func(u *unstructured.Unstructured) error {
			app, _, _ := unstructured.NestedString(u.Object, "data", "app")
			if app != "demo-app" {
				return &notYetError{msg: "cm1.data.app=" + app + ", want demo-app"}
			}
			return nil
		}, 15*time.Second)

		// cm2: data.ref should equal "cm1" (cross-node CEL reference)
		env.AwaitObject(t, cmGVK, types.NamespacedName{Namespace: ns, Name: "cm2"}, func(u *unstructured.Unstructured) error {
			ref, _, _ := unstructured.NestedString(u.Object, "data", "ref")
			if ref != "cm1" {
				return &notYetError{msg: "cm2.data.ref=" + ref + ", want cm1"}
			}
			return nil
		}, 15*time.Second)

		// Verify the ManagedResource entries have the correct node IDs.
		nodeIDs := map[string]bool{}
		for _, mr := range result.Applied {
			nodeIDs[mr.NodeID] = true
		}
		if !nodeIDs["cm1"] {
			t.Fatalf("ApplyResult.Applied missing cm1 node; got nodeIDs %v", nodeIDs)
		}
		if !nodeIDs["cm2"] {
			t.Fatalf("ApplyResult.Applied missing cm2 node; got nodeIDs %v", nodeIDs)
		}

		// Verify the ManagedResource UID is populated (SSA returned server fields).
		for _, mr := range result.Applied {
			if mr.UID == "" {
				t.Fatalf("ApplyResult.Applied[%s] has empty UID (SSA must return it)", mr.NodeID)
			}
			if mr.Name == "" {
				t.Fatalf("ApplyResult.Applied[%s] has empty Name", mr.NodeID)
			}
		}

		t.Logf("applied 2 ConfigMaps to the cluster via Graph engine executor")
		t.Logf("  cm1.data.app = demo-app (resolved from instance spec via schema def node)")
		t.Logf("  cm2.data.ref = cm1 (cross-node CEL reference)")
		t.Logf("  Graph=%s/%s, runtime nodes=%d, managed resources=%d",
			g.Namespace, g.Name, len(rt.Nodes()), len(result.Applied))
	})
})

// ── helpers ──────────────────────────────────────────────────────────────────

// ensureTestInstanceCRD synthesizes and installs the CRD for an RGD instance.
func ensureTestInstanceCRD(t environment.TestingT, env *environment.Environment, rgd *krov1alpha1.ResourceGraphDefinition) {
	t.Helper()
	specSchema, err := graph.BuildInstanceSpecSchema(rgd.Spec.Schema)
	if err != nil {
		t.Fatalf("build instance spec schema: %v", err)
	}
	scope := apiextensionsv1.NamespaceScoped
	if rgd.Spec.Schema != nil && rgd.Spec.Schema.Scope == krov1alpha1.ResourceScopeCluster {
		scope = apiextensionsv1.ClusterScoped
	}
	crdObj := crd.SynthesizeCRD(
		rgd.Spec.Schema.Group,
		rgd.Spec.Schema.APIVersion,
		rgd.Spec.Schema.Kind,
		*specSchema,
		apiextensionsv1.JSONSchemaProps{Type: "object"},
		false,
		scope,
		rgd.Spec.Schema,
	)
	ctx := env.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := env.Client.Create(ctx, crdObj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create test instance crd: %v", err)
	}
	awaitCRDEstablished(t, env, crdObj.Name)
	t.Cleanup(func() {
		_ = env.Client.Delete(context.Background(), crdObj)
	})
}

// newWebAppRGD builds a representative RGD used by the apply/deletion tests.
//   - Kind=WebApp, group=kro.run, version=v1alpha1
//   - spec.name string
//   - cm1: ConfigMap with data.app = ${schema.spec.name}
//   - cm2: ConfigMap with data.ref = ${cm1.metadata.name}
func newWebAppRGD(name, kind, namespace string) *krov1alpha1.ResourceGraphDefinition {
	specRaw, _ := json.Marshal(map[string]any{"name": "string"})
	cm1Raw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "cm1", "namespace": namespace},
		"data":       map[string]any{"app": "${schema.spec.name}"},
	})
	cm2Raw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "cm2", "namespace": namespace},
		"data":       map[string]any{"ref": "${cm1.metadata.name}"},
	})

	return &krov1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name + "-uid"),
		},
		Spec: krov1alpha1.ResourceGraphDefinitionSpec{
			Schema: &krov1alpha1.Schema{
				Kind:       kind,
				APIVersion: "v1alpha1",
				Group:      "kro.run",
				Spec: runtime.RawExtension{
					Raw: specRaw,
				},
			},
			Resources: []*krov1alpha1.Resource{
				{
					ID: "cm1",
					Template: runtime.RawExtension{
						Raw: cm1Raw,
					},
				},
				{
					ID: "cm2",
					Template: runtime.RawExtension{
						Raw: cm2Raw,
					},
				},
			},
		},
	}
}

func awaitCRDEstablished(t environment.TestingT, env *environment.Environment, name string) {
	t.Helper()
	environment.Eventually(t, 20*time.Second, 100*time.Millisecond, func() error {
		got := &apiextensionsv1.CustomResourceDefinition{}
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
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

// notYetError is a plain error type used in AwaitObject match closures so the
// failure message is readable.
type notYetError struct{ msg string }

func (e *notYetError) Error() string { return e.msg }
