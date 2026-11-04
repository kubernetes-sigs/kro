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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
)

func managedFieldsEntry(t *testing.T, manager string, raw string) metav1.ManagedFieldsEntry {
	t.Helper()
	return metav1.ManagedFieldsEntry{
		Manager:  manager,
		FieldsV1: &metav1.FieldsV1{Raw: []byte(raw)},
	}
}

func TestForeignFieldOwner(t *testing.T) {
	segments, err := compiler.ParseFieldPath("spec.containers[name=nginx].env[name=FOO].value")
	require.NoError(t, err)

	t.Run("nil current is never foreign owned", func(t *testing.T) {
		assert.Equal(t, "", foreignFieldOwner(nil, segments))
	})

	t.Run("only kro's own managers is not foreign owned", func(t *testing.T) {
		current := &unstructured.Unstructured{Object: map[string]any{}}
		current.SetManagedFields([]metav1.ManagedFieldsEntry{
			managedFieldsEntry(t, FieldManager, `{"f:spec":{"f:containers":{"k:{\"name\":\"nginx\"}":{"f:env":{"k:{\"name\":\"FOO\"}":{"f:value":{}}}}}}}`),
		})
		assert.Equal(t, "", foreignFieldOwner(current, segments))
	})

	t.Run("a foreign manager claiming the exact selected field is detected", func(t *testing.T) {
		current := &unstructured.Unstructured{Object: map[string]any{}}
		current.SetManagedFields([]metav1.ManagedFieldsEntry{
			managedFieldsEntry(t, FieldManager, `{"f:spec":{"f:replicas":{}}}`),
			managedFieldsEntry(t, "manager-b", `{"f:spec":{"f:containers":{"k:{\"name\":\"nginx\"}":{"f:env":{"k:{\"name\":\"FOO\"}":{"f:value":{}}}}}}}`),
		})
		assert.Equal(t, "manager-b", foreignFieldOwner(current, segments))
	})

	t.Run("a foreign manager claiming a sibling field is not a match", func(t *testing.T) {
		current := &unstructured.Unstructured{Object: map[string]any{}}
		current.SetManagedFields([]metav1.ManagedFieldsEntry{
			managedFieldsEntry(t, "manager-b", `{"f:spec":{"f:containers":{"k:{\"name\":\"nginx\"}":{"f:env":{"k:{\"name\":\"BAR\"}":{"f:value":{}}}}}}}`),
		})
		assert.Equal(t, "", foreignFieldOwner(current, segments))
	})

	t.Run("two different foreign managers both claiming the field is still detected, no arbitration needed", func(t *testing.T) {
		current := &unstructured.Unstructured{Object: map[string]any{}}
		claim := `{"f:spec":{"f:containers":{"k:{\"name\":\"nginx\"}":{"f:env":{"k:{\"name\":\"FOO\"}":{"f:value":{}}}}}}}`
		current.SetManagedFields([]metav1.ManagedFieldsEntry{
			managedFieldsEntry(t, FieldManager, `{"f:spec":{"f:replicas":{}}}`),
			managedFieldsEntry(t, "manager-a", claim),
			managedFieldsEntry(t, "manager-b", claim),
		})
		owner := foreignFieldOwner(current, segments)
		assert.Contains(t, []string{"manager-a", "manager-b"}, owner, "either foreign claimant is an acceptable answer, kro only needs to know it is no longer the sole owner")
	})

	t.Run("a numeric merge key value is matched by type, not just by string form", func(t *testing.T) {
		numericSegments, err := compiler.ParseFieldPath("spec.ports[containerPort=8080].protocol")
		require.NoError(t, err)
		current := &unstructured.Unstructured{Object: map[string]any{}}
		current.SetManagedFields([]metav1.ManagedFieldsEntry{
			managedFieldsEntry(t, "manager-b", `{"f:spec":{"f:ports":{"k:{\"containerPort\":8080}":{"f:protocol":{}}}}}`),
		})
		assert.Equal(t, "manager-b", foreignFieldOwner(current, numericSegments))
	})

	t.Run("kro's own patch node manager is not foreign owned", func(t *testing.T) {
		current := &unstructured.Unstructured{Object: map[string]any{}}
		current.SetManagedFields([]metav1.ManagedFieldsEntry{
			managedFieldsEntry(t, PatchFieldManager("some-uid", "patchnode"),
				`{"f:spec":{"f:containers":{"k:{\"name\":\"nginx\"}":{"f:env":{"k:{\"name\":\"FOO\"}":{"f:value":{}}}}}}}`),
		})
		assert.Equal(t, "", foreignFieldOwner(current, segments), "a Patch node contributing to the same path is kro's own write, not an external controller")
	})

	t.Run("a boolean merge key value is matched by type, not just by string form", func(t *testing.T) {
		boolSegments, err := compiler.ParseFieldPath("spec.items[primary=true].value")
		require.NoError(t, err)
		current := &unstructured.Unstructured{Object: map[string]any{}}
		current.SetManagedFields([]metav1.ManagedFieldsEntry{
			managedFieldsEntry(t, "manager-b", `{"f:spec":{"f:items":{"k:{\"primary\":true}":{"f:value":{}}}}}`),
		})
		assert.Equal(t, "manager-b", foreignFieldOwner(current, boolSegments))
	})
}

func TestRemoveFieldPath(t *testing.T) {
	t.Run("plain map key", func(t *testing.T) {
		obj := map[string]any{"spec": map[string]any{"replicas": 3.0}}
		segments, err := compiler.ParseFieldPath("spec.replicas")
		require.NoError(t, err)
		removeFieldPath(obj, segments)
		spec := obj["spec"].(map[string]any)
		_, ok := spec["replicas"]
		assert.False(t, ok)
	})

	t.Run("field inside a selected list item", func(t *testing.T) {
		obj := map[string]any{
			"spec": map[string]any{
				"containers": []any{
					map[string]any{"name": "sidecar", "env": []any{
						map[string]any{"name": "FOO", "value": "keep-me-out"},
					}},
					map[string]any{"name": "nginx", "env": []any{
						map[string]any{"name": "FOO", "value": "1"},
						map[string]any{"name": "BAR", "value": "2"},
					}},
				},
			},
		}
		segments, err := compiler.ParseFieldPath("spec.containers[name=nginx].env[name=FOO].value")
		require.NoError(t, err)
		removeFieldPath(obj, segments)

		containers := obj["spec"].(map[string]any)["containers"].([]any)
		sidecarEnv := containers[0].(map[string]any)["env"].([]any)[0].(map[string]any)
		assert.Equal(t, "keep-me-out", sidecarEnv["value"], "a same-named field on a different list item must be untouched")

		nginxEnv := containers[1].(map[string]any)["env"].([]any)
		foo := nginxEnv[0].(map[string]any)
		_, ok := foo["value"]
		assert.False(t, ok, "the matched item's targeted field must be removed")
		assert.Equal(t, "FOO", foo["name"], "the matched item's identity field must survive")
		bar := nginxEnv[1].(map[string]any)
		assert.Equal(t, "2", bar["value"], "a sibling item in the same list must be untouched")
	})

	t.Run("missing segment is a no-op", func(t *testing.T) {
		obj := map[string]any{"spec": map[string]any{}}
		segments, err := compiler.ParseFieldPath("spec.containers[name=nginx].image")
		require.NoError(t, err)
		assert.NotPanics(t, func() { removeFieldPath(obj, segments) })
	})

	t.Run("no matching list item is a no-op", func(t *testing.T) {
		obj := map[string]any{
			"spec": map[string]any{"containers": []any{map[string]any{"name": "other"}}},
		}
		segments, err := compiler.ParseFieldPath("spec.containers[name=nginx].image")
		require.NoError(t, err)
		removeFieldPath(obj, segments)
		containers := obj["spec"].(map[string]any)["containers"].([]any)
		assert.Equal(t, "other", containers[0].(map[string]any)["name"])
	})
}
