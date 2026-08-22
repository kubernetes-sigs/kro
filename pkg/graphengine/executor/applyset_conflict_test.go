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

package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

func TestApplySetConflict(t *testing.T) {
	t.Parallel()

	makeObj := func(applySetID string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm", "namespace": "default"},
		}}
		if applySetID != "" {
			o.SetLabels(map[string]string{applyset.ApplysetPartOfLabel: applySetID})
		}
		return o
	}

	t.Run("nil current never conflicts", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, applySetConflict(nil, makeObj("applyset-a")))
	})

	t.Run("current without applyset label never conflicts", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, applySetConflict(makeObj(""), makeObj("applyset-a")))
		assert.NoError(t, applySetConflict(makeObj(""), makeObj("")))
	})

	t.Run("matching applyset IDs do not conflict", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, applySetConflict(makeObj("applyset-a"), makeObj("applyset-a")))
	})

	t.Run("live object owned by applyset-A conflicts when desired has empty applyset label", func(t *testing.T) {
		t.Parallel()
		err := applySetConflict(makeObj("applyset-a"), makeObj(""))
		require.Error(t, err)

		var conflictErr *applyset.ApplySetConflictError
		require.True(t, errors.As(err, &conflictErr))
		assert.Equal(t, "applyset-a", conflictErr.CurrentApplySetID)
		assert.Equal(t, "", conflictErr.DesiredApplySetID)
		assert.Equal(t, "cm", conflictErr.ResourceName)
		assert.Equal(t, "default", conflictErr.ResourceNamespace)
	})

	t.Run("live object owned by applyset-A conflicts when desired has applyset-B", func(t *testing.T) {
		t.Parallel()
		err := applySetConflict(makeObj("applyset-a"), makeObj("applyset-b"))
		require.Error(t, err)

		var conflictErr *applyset.ApplySetConflictError
		require.True(t, errors.As(err, &conflictErr))
		assert.Equal(t, "applyset-a", conflictErr.CurrentApplySetID)
		assert.Equal(t, "applyset-b", conflictErr.DesiredApplySetID)
	})
}

func TestSimple_ApplyTemplate_ApplySetConflict_Scalar(t *testing.T) {
	t.Parallel()

	t.Run("refuses to overwrite live object owned by another applyset when desired has empty label", func(t *testing.T) {
		t.Parallel()
		live := liveCM("protected-cm")
		live.SetLabels(map[string]string{applyset.ApplysetPartOfLabel: "applyset-tenant-a-v1"})
		live.Object["data"] = map[string]any{"owner": "tenant-a"}

		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(live).Build()

		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "protected-cm"},
				"data":     map[string]any{"owner": "attacker"},
			}),
		)

		res, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, g), watchrouter.NoopWatcher{})

		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrNotReady), "ApplySet conflict must be a hard error")

		var conflictErr *applyset.ApplySetConflictError
		require.True(t, errors.As(err, &conflictErr))
		assert.Equal(t, "applyset-tenant-a-v1", conflictErr.CurrentApplySetID)
		assert.Equal(t, "", conflictErr.DesiredApplySetID)
		assert.Empty(t, res.Applied)

		// Verify cluster object was NOT overwritten
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(configMapGVK)
		require.NoError(t, cl.Get(context.Background(),
			types.NamespacedName{Namespace: "default", Name: "protected-cm"}, got))
		data, _, _ := unstructured.NestedStringMap(got.Object, "data")
		assert.Equal(t, "tenant-a", data["owner"])
	})

	t.Run("refuses to overwrite live object owned by another applyset when desired has different label", func(t *testing.T) {
		t.Parallel()
		live := liveCM("protected-cm")
		live.SetLabels(map[string]string{applyset.ApplysetPartOfLabel: "applyset-tenant-a-v1"})
		live.Object["data"] = map[string]any{"owner": "tenant-a"}

		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(live).Build()

		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{
					"name": "protected-cm",
					"labels": map[string]any{
						applyset.ApplysetPartOfLabel: "applyset-tenant-b-v1",
					},
				},
				"data": map[string]any{"owner": "tenant-b"},
			}),
		)

		res, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, g), watchrouter.NoopWatcher{})

		require.Error(t, err)
		var conflictErr *applyset.ApplySetConflictError
		require.True(t, errors.As(err, &conflictErr))
		assert.Equal(t, "applyset-tenant-a-v1", conflictErr.CurrentApplySetID)
		assert.Equal(t, "applyset-tenant-b-v1", conflictErr.DesiredApplySetID)
		assert.Empty(t, res.Applied)
	})
}

func TestSimple_ApplyTemplate_ApplySetConflict_Collection(t *testing.T) {
	t.Parallel()

	live0 := liveCM("cm-alpha")
	live0.SetLabels(map[string]string{applyset.ApplysetPartOfLabel: "applyset-other-v1"})
	live0.Object["data"] = map[string]any{"owner": "other"}

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(live0).Build()

	res, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotReady), "collection ApplySet conflict must be a hard error")

	var conflictErr *applyset.ApplySetConflictError
	require.True(t, errors.As(err, &conflictErr))
	assert.Equal(t, "applyset-other-v1", conflictErr.CurrentApplySetID)
	assert.Equal(t, "", conflictErr.DesiredApplySetID)

	// cm-alpha was rejected due to conflict; cm-beta may have landed
	appliedNames := make([]string, 0, len(res.Applied))
	for _, a := range res.Applied {
		appliedNames = append(appliedNames, a.Name)
	}
	assert.NotContains(t, appliedNames, "cm-alpha", "conflicted item must not be recorded as applied")
}
