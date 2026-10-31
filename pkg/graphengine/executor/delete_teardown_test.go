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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// Use real SSA ownership metadata and API-server delete precondition enforcement.
func TestSimple_Delete_WriteAhead(t *testing.T) {
	cl := patchEnvClient(t)
	ctx := context.Background()
	for _, mode := range []string{"owned", "foreign", "recorded-stale", "recreated-after-get", "peer-after-get", "updated-after-get"} {
		t.Run(mode, func(t *testing.T) {
			name := "writeahead-" + mode
			g := generator.NewGraph("g", generator.WithNamespace("default"),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": name},
					"data":     map[string]any{"value": "owned"},
				}))
			g.SetUID("writeahead-graph")
			ex := NewSimple(cl)
			ex.ConflictDetection = true
			result, err := ex.Apply(ctx, compileAndBuild(t, g), watchrouter.NoopWatcher{})
			require.NoError(t, err)
			require.Len(t, result.Applied, 1)
			entry := result.Applied[0]
			live := getConfigMap(t, cl, "default", name)
			require.NotEmpty(t, live.GetUID())
			require.True(t, hasFieldManager(live, templateFieldManager(g.GetUID())))
			require.Equal(t, string(live.GetUID()), entry.UID)
			t.Cleanup(func() { _ = cl.Delete(ctx, live) })
			owner := g.GetUID()
			if mode == "recorded-stale" {
				// Even a replacement carrying our marker must not supersede a
				// recorded UID: the originally tracked object is already gone.
				require.NoError(t, cl.Delete(ctx, live))
				_, err = ex.Apply(ctx, compileAndBuild(t, g), watchrouter.NoopWatcher{})
				require.NoError(t, err)
				live = getConfigMap(t, cl, "default", name)
				require.NotEqual(t, entry.UID, string(live.GetUID()))
			} else {
				entry.UID = "" // replay persisted pre-apply intent
			}
			if mode == "foreign" {
				owner = "peer-graph"
			}
			raced := mode == "recreated-after-get" || mode == "peer-after-get" || mode == "updated-after-get"
			deleteCalls := 0
			if raced {
				ex.Client = interceptor.NewClient(cl, interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						deleteCalls++
						preconditions := (&client.DeleteOptions{}).ApplyOptions(opts).Preconditions
						require.NotNil(t, preconditions)
						require.NotNil(t, preconditions.UID)
						require.NotNil(t, preconditions.ResourceVersion)
						require.Equal(t, live.GetUID(), *preconditions.UID)
						require.Equal(t, live.GetResourceVersion(), *preconditions.ResourceVersion)
						switch mode {
						case "recreated-after-get":
							require.NoError(t, c.Delete(ctx, obj))
							replacement := &unstructured.Unstructured{}
							replacement.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
							replacement.SetName(obj.GetName())
							replacement.SetNamespace(obj.GetNamespace())
							require.NoError(t, c.Create(ctx, replacement))
						case "peer-after-get":
							// A peer's disjoint SSA can finish after both Graphs checked ownership.
							peer := &unstructured.Unstructured{Object: map[string]any{
								"apiVersion": "v1", "kind": "ConfigMap",
								"metadata": map[string]any{"namespace": "default", "name": name},
								"data":     map[string]any{"peer": "owned"},
							}}
							require.NoError(t, ex.ssaApply(ctx, peer, templateFieldManager("peer-graph"), false))
						case "updated-after-get":
							updated := live.DeepCopy()
							require.NoError(t, unstructured.SetNestedField(updated.Object, "true", "metadata", "annotations", "example.com/updated"))
							require.NoError(t, c.Update(ctx, updated))
						}
						return c.Delete(ctx, obj, opts...)
					},
				})
			}

			err = ex.Delete(ctx, owner, []expv1alpha1.ManagedResource{entry})
			if raced {
				require.Equal(t, 1, deleteCalls)
				require.True(t, apierrors.IsConflict(err), "a changed recovery snapshot must be retried: %v", err)
			} else {
				require.NoError(t, err)
			}
			if mode == "owned" {
				assertCMGone(t, cl, name, "default")
				return
			}
			current := getConfigMap(t, cl, "default", name)
			if mode == "recreated-after-get" {
				assert.NotEqual(t, live.GetUID(), current.GetUID(), "replacement must survive the failed precondition")
			} else {
				assert.Equal(t, live.GetUID(), current.GetUID(), "the live object must survive")
			}
			if mode == "peer-after-get" {
				require.True(t, hasFieldManager(current, templateFieldManager(g.GetUID())))
				require.True(t, hasFieldManager(current, templateFieldManager("peer-graph")))
			}
			if raced {
				require.NotEqual(t, live.GetResourceVersion(), current.GetResourceVersion())
				ex.Client = cl
				require.NoError(t, ex.Delete(ctx, owner, []expv1alpha1.ManagedResource{entry}))
				if mode == "updated-after-get" {
					assertCMGone(t, cl, name, "default")
				} else {
					retried := getConfigMap(t, cl, "default", name)
					assert.Equal(t, current.GetUID(), retried.GetUID(), "retry must preserve the replacement or peer-owned object")
				}
			}
		})
	}
}

// Exercise overlapping and non-template markers produced by real SSA, rather than
// manually assigning managedFields as in the unit boundary table.
func TestSimple_Delete_OwnershipMarkers(t *testing.T) {
	cl := patchEnvClient(t)
	ctx := context.Background()
	const ownerUID = "writeahead-marker-graph"
	self := templateFieldManager(ownerUID)
	peer := templateFieldManager("peer-marker-graph")
	for _, tc := range []struct {
		name     string
		managers []string
		labels   bool
	}{
		{name: "self-and-peer", managers: []string{self, peer}},
		{name: "bystander"},
		{name: "patch-only", managers: []string{patchFieldManager(ownerUID, "n")}},
		{name: "shared-only", managers: []string{FieldManager}},
		{name: "labels-only", labels: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "writeahead-marker-" + tc.name
			desired := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"namespace": "default", "name": name},
				"data":     map[string]any{"value": "kept"},
			}}
			if tc.labels {
				desired.SetLabels(map[string]string{metadata.InstanceIDLabel: ownerUID, metadata.NodeIDLabel: "n"})
			}
			ex := NewSimple(cl)
			if len(tc.managers) == 0 {
				require.NoError(t, cl.Create(ctx, desired, client.FieldOwner("kubectl")))
			} else {
				for i, manager := range tc.managers {
					apply := desired.DeepCopy()
					// Disjoint fields retain both managers on overlapping objects.
					apply.Object["data"] = map[string]any{fmt.Sprintf("field%d", i): "kept"}
					require.NoError(t, ex.ssaApply(ctx, apply, manager, false))
				}
			}
			live := getConfigMap(t, cl, "default", name)
			t.Cleanup(func() { _ = cl.Delete(ctx, live) })
			for _, manager := range tc.managers {
				require.True(t, hasFieldManager(live, manager), "API must retain marker %q", manager)
			}
			require.NoError(t, ex.Delete(ctx, ownerUID, []expv1alpha1.ManagedResource{{
				NodeID: "n", APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: name,
			}}))
			current := getConfigMap(t, cl, "default", name)
			assert.Equal(t, live.GetUID(), current.GetUID(), "unowned object must survive without recreation")
		})
	}
}
