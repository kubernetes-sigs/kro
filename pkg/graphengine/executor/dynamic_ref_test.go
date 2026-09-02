// Copyright 2025 The Kube Resource Orchestrator Authors
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

package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// TestSimple_Apply_DynamicRef exercises a ref: node whose apiVersion/kind is a
// CEL expression (the graph.md "Dynamic GVKs" ref example). The GVR has no
// compile-time value, so the read path resolves it from the rendered object's
// concrete GVK through the live REST mapper at apply time — identical to the
// dynamic-template apply path, but read-only. A single ref Gets one object; a
// selector ref Lists a collection; and a ref whose target GVK the cluster can't
// map yet is a SOFT not-ready (Unresolved + ErrNotReady), never a hard abort.
func TestSimple_Apply_DynamicRef(t *testing.T) {
	t.Parallel()

	_, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))

	seedCM := func(name, tier string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name": name, "namespace": "default",
				"labels": map[string]any{"tier": tier},
			},
			"data": map[string]any{"k": name},
		}}
	}

	t.Run("single dynamic ref resolves its GVK and reads the live object", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			// crd.group feeds the ref's apiVersion — the ref is dynamic.
			generator.WithDef("crd", map[string]any{"group": "v1"}),
			generator.WithRef("target", &expv1alpha1.ExternalRef{
				APIVersion: "${crd.group}",
				Kind:       "ConfigMap",
				Metadata:   expv1alpha1.ExternalRefMetadata{Name: "live-cm", Namespace: "default"},
			}),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(rm).
			WithObjects(seedCM("live-cm", "db")).Build()

		w := &recordingWatcher{}
		res, err := NewSimple(cl).Apply(context.Background(), rt, w)
		require.NoError(t, err)

		assert.Empty(t, res.Applied, "a ref is read-only and never a managed resource")
		assert.NotContains(t, res.Unresolved, "target", "a resolvable ref is not unresolved")
		// The apply-time-resolved GVR drives the drift watch.
		require.Len(t, w.reqs, 1)
		assert.Equal(t, "configmaps", w.reqs[0].GVR.Resource)
		assert.Equal(t, "live-cm", w.reqs[0].Name)
	})

	t.Run("dynamic selector ref lists matching objects", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("crd", map[string]any{"kind": "ConfigMap"}),
			generator.WithRef("coll", &expv1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "${crd.kind}",
				Metadata: expv1alpha1.ExternalRefMetadata{
					Namespace: "default",
					Selector:  runtime.RawExtension{Raw: []byte(`{"matchLabels":{"tier":"db"}}`)},
				},
			}),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(rm).
			WithObjects(seedCM("db-a", "db"), seedCM("db-b", "db"), seedCM("web", "web")).Build()

		w := &recordingWatcher{}
		res, err := NewSimple(cl).Apply(context.Background(), rt, w)
		require.NoError(t, err)

		assert.Empty(t, res.Applied, "collection members are read-only, never managed")
		assert.Empty(t, res.Unresolved, "a listed collection is resolved")
		// One selector watch on the apply-time-resolved GVR/namespace.
		require.Len(t, w.reqs, 1)
		assert.Equal(t, "configmaps", w.reqs[0].GVR.Resource)
		assert.Equal(t, "default", w.reqs[0].Namespace)
		require.NotNil(t, w.reqs[0].Selector)
		assert.True(t, w.reqs[0].Selector.Matches(labels.Set{"tier": "db"}))
		assert.False(t, w.reqs[0].Selector.Matches(labels.Set{"tier": "web"}))
	})

	t.Run("uninstalled target GVK is a soft not-ready, not a hard error", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("crd", map[string]any{"group": "example.com/v1"}),
			generator.WithRef("target", &expv1alpha1.ExternalRef{
				APIVersion: "${crd.group}",
				Kind:       "Widget", // no REST mapping in the fake discovery
				Metadata:   expv1alpha1.ExternalRefMetadata{Name: "w", Namespace: "default"},
			}),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(rm).Build()

		res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrNotReady),
			"an uninstalled dynamic-ref target GVK must requeue, not fail hard, got %v", err)
		assert.ErrorIs(t, err, errSchemaNotReady)
		assert.Contains(t, res.Unresolved, "target",
			"an unresolved dynamic ref must be reported so prune is withheld")
		assert.Empty(t, res.Applied)
	})

	t.Run("uninstalled selector target GVK is a soft not-ready", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("crd", map[string]any{"kind": "Widget"}),
			generator.WithRef("coll", &expv1alpha1.ExternalRef{
				APIVersion: "example.com/v1",
				Kind:       "${crd.kind}",
				Metadata: expv1alpha1.ExternalRefMetadata{
					Namespace: "default",
					Selector:  runtime.RawExtension{Raw: []byte(`{}`)},
				},
			}),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(rm).Build()

		res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrNotReady))
		assert.ErrorIs(t, err, errSchemaNotReady)
		assert.Contains(t, res.Unresolved, "coll")
		assert.Empty(t, res.Applied)
	})
}
